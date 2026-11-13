package liveswap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/smallhoursorg/motel/liveswap/deploytrust"
	"github.com/smallhoursorg/motel/liveswap/wire"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(Handler{})
}

// maxPayloadBytes bounds the webhook JSON body.
const maxPayloadBytes = 64 * 1024

// loggedAppName bounds a request-supplied app name for the log: no
// configured name is longer than appNameRe allows, so anything past
// that is noise.
func loggedAppName(name string) string {
	if len(name) <= appNameMaxLen {
		return name
	}
	// At a rune boundary, as every bound of the preamble's cuts: a cut
	// inside one would leave invalid UTF-8 for that bound to quote, and
	// the journal would carry the cut's artefact rather than the name.
	return deploytrust.CutRunes(name, appNameMaxLen) + "..."
}

// Handler implements the liveswap webhook endpoint. Mount it in its own
// (HTTPS) site block; the final path segment names the app:
//
//	POST /<app>  deploys: {"url": "...", "version": "...", "auth_header": "..."}
//	GET  /<app>  returns status JSON (phase, current version, last deploy)
//
// Responses are synchronous — the POST returns when the deploy has
// fully succeeded (200) or failed (5xx with the old version, if there
// was one, still serving), so `curl --fail-with-body` makes CI red exactly when it
// should be, with the reason in the body. A POST that asks for the
// stream (Accept: application/x-ndjson) is the exception: once its
// first phase line is out the status is 200 whatever the outcome, and
// the last line's http_status carries the 5xx (deployStream).
type Handler struct {
	app     *App
	logger  *zap.Logger
	limiter *deploytrust.Limiter
	// stage is a test seam for stageUpload; nil is stageUpload. A full
	// disk cannot be made in a test that runs as root.
	stage func(body io.Reader, tmpDir string, maxBytes int64) (string, string, error)
}

// CaddyModule returns the Caddy module information.
func (Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.liveswap_webhook",
		New: func() caddy.Module { return new(Handler) },
	}
}

// Provision binds the handler to the liveswap app.
func (h *Handler) Provision(ctx caddy.Context) error {
	h.logger = ctx.Logger()
	appModule, err := ctx.App("liveswap")
	if err != nil {
		return err
	}
	h.app = appModule.(*App)
	if len(h.app.Apps) == 0 {
		return fmt.Errorf("liveswap_webhook is configured but no apps are defined in the liveswap global options")
	}
	h.limiter = deploytrust.Shared()
	return nil
}

// ServeHTTP is terminal: it never calls the next handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, _ caddyhttp.Handler) error {
	name := path.Base(path.Clean(r.URL.Path))
	ma := h.app.managedApp(name)

	// Authenticate before revealing whether the app exists. For an
	// unknown app the token is verified against the global trust
	// sources, so the status never leaks valid app names to
	// unauthenticated callers (with no global trust configured, an
	// unknown app is simply unauthenticable → 401). No refusal waits on
	// an issuer; the time to the 401 can still differ by the local work
	// a name's sources do, the residual DESIGN-threat-model.md accepts
	// ("Webhook endpoint").
	verifiers := h.app.globalVerifiers
	if ma != nil {
		verifiers = ma.currentVerifiers()
	}
	who, refusal := h.limiter.Authenticate(r, verifiers, h.logger, "app", loggedAppName(name))
	if refusal != nil {
		return refusal.Write(func(code int, body any) error { return RespondJSON(w, code, body, nil) })
	}
	if ma == nil {
		return RespondJSON(w, http.StatusNotFound, wire.Error{Error: fmt.Sprintf("unknown app %q", name)}, nil)
	}

	switch r.Method {
	case http.MethodGet:
		// Parsed strictly: URL.Query drops a pair it cannot decode, and
		// a malformed `?deploy=%ZZ` would fall through to the status.
		q, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			return RespondJSON(w, http.StatusUnprocessableEntity, wire.Error{Error: "malformed query: " + err.Error()}, ma.redactorFor(statusSnapshot{}))
		}
		if q.Has("deploy") {
			return h.deployRecord(w, ma, q.Get("deploy"))
		}
		window := wire.MetricsRanges[0]
		if q.Has("range") {
			// Refused whether or not the box keeps metrics: a typo is told
			// the same on every box.
			if window = q.Get("range"); !slices.Contains(wire.MetricsRanges, window) {
				return RespondJSON(w, http.StatusUnprocessableEntity, wire.Error{
					Error: "range must be one of " + strings.Join(wire.MetricsRanges, ", ")}, ma.redactorFor(statusSnapshot{}))
			}
		}
		s := ma.status()
		if src := h.app.metricsSource; src != nil {
			// Best-effort and bounded by the source itself: a store that
			// does not answer is an error inside the object, never a
			// failed or stalled status.
			s.Metrics = src.AppMetrics(r.Context(), ma.name, window)
		}
		return RespondJSON(w, http.StatusOK, s, ma.redactorFor(s))
	case http.MethodPost:
		return h.deploy(w, r, ma, who.By)
	default:
		w.Header().Set("Allow", "GET, POST")
		return RespondJSON(w, http.StatusMethodNotAllowed, wire.Error{Error: "method not allowed"}, ma.redactorFor(statusSnapshot{}))
	}
}

// deploy dispatches on the request shape:
//   - a gzip body  → push an uploaded artifact  (POST /<app>?version=v2)
//   - ?rollback=v1 → relaunch an on-disk release (POST /<app>?rollback=v1)
//   - otherwise    → pull from a URL             (JSON {url, version})
func (h *Handler) deploy(w http.ResponseWriter, r *http.Request, ma *managedApp, by string) error {
	switch {
	case isGzipUpload(r):
		return h.deployPush(w, r, ma, by)
	case r.URL.Query().Get("rollback") != "":
		return h.deployRollback(w, r, ma, by)
	default:
		return h.deployURL(w, r, ma, by)
	}
}

// deployURL is the default path: a JSON body naming an artifact URL to
// pull, size-capped like any control payload.
func (h *Handler) deployURL(w http.ResponseWriter, r *http.Request, ma *managedApp, by string) error {
	// Read one byte past the cap: exceeding it proves the payload is
	// oversized, which deserves an honest 413 — truncating at the cap
	// would surface as a misleading "invalid JSON" 400.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPayloadBytes+1))
	if err != nil {
		return RespondJSON(w, http.StatusBadRequest, wire.Error{Error: "reading body: " + err.Error()}, ma.redactorFor(statusSnapshot{}))
	}
	if len(body) > maxPayloadBytes {
		return RespondJSON(w, http.StatusRequestEntityTooLarge, wire.Error{
			Error: fmt.Sprintf("payload exceeds %d bytes", maxPayloadBytes)}, ma.redactorFor(statusSnapshot{}))
	}
	p, status, msg := parseDeployPayload(body)
	if status != 0 {
		return RespondJSON(w, status, wire.Error{Error: msg}, ma.redactorFor(statusSnapshot{}))
	}
	return h.runDeploy(w, r, ma, p.request(), by)
}

// parseDeployPayload is the body half of deployURL: the size-capped
// bytes in, either a payload the pipeline may act on or the 4xx that
// refuses it. Every check the JSON body gets before it becomes a
// deployRequest lives here (the URL allowlist is downstream, in the
// download), and it is kept free of the ResponseWriter so
// FuzzDeployRequest drives the production gate itself. status is 0 on
// success.
func parseDeployPayload(body []byte) (p deployPayload, status int, msg string) {
	if err := json.Unmarshal(body, &p); err != nil {
		return deployPayload{}, http.StatusBadRequest, "invalid JSON payload: " + err.Error()
	}
	if p.URL == "" {
		return deployPayload{}, http.StatusUnprocessableEntity, "url is required"
	}
	if !validVersion(p.Version) {
		return deployPayload{}, http.StatusUnprocessableEntity, fmt.Sprintf("version must match %s", versionRe)
	}
	// Go's transport would refuse a control character in the header
	// value anyway, but as an opaque 500 at fetch time; catching it
	// here names the field while it is still cheap to fix.
	if strings.ContainsFunc(p.AuthHeader, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return deployPayload{}, http.StatusUnprocessableEntity, "auth_header contains control characters"
	}
	// A pin is the digest as sha256sum prints it, in either case,
	// carried on in the lowercase form the journal and a mismatch name.
	if p.SHA256 != "" {
		if !sha256Re.MatchString(p.SHA256) {
			return deployPayload{}, http.StatusUnprocessableEntity, fmt.Sprintf("sha256 must match %s (the artifact's digest as sha256sum prints it)", sha256Re)
		}
		p.SHA256 = strings.ToLower(p.SHA256)
	}
	return p, 0, ""
}

// deployPush streams an uploaded gzip tarball to a staging file and
// deploys it — no artifact host needed. Version comes from the query
// string because the body is the artifact.
func (h *Handler) deployPush(w http.ResponseWriter, r *http.Request, ma *managedApp, by string) error {
	version := r.URL.Query().Get("version")
	if !validVersion(version) {
		return RespondJSON(w, http.StatusUnprocessableEntity, wire.Error{Error: fmt.Sprintf("version query param must match %s", versionRe)}, ma.redactorFor(statusSnapshot{}))
	}
	// Acquire the per-app deploy lock BEFORE staging the upload: a
	// concurrent push then gets an immediate 409 instead of streaming a
	// full tarball to disk only to lose the lock, and total staged bytes
	// stay bounded to a single upload.
	if !ma.deployMu.TryLock() {
		return h.mapDeployResult(w, ma, errDeployInProgress)
	}
	defer ma.endDeploy()

	// One snapshot for both staging and the pipeline, so a reload during
	// the upload can't stage under one spec and extract under another.
	c := ma.snapshot()
	spec := c.spec
	// The upload is the push's first write, so the pipeline's refusal
	// of an app dir that is not the directory it names comes first
	// here: a linked tmp/ would stage the upload wherever it points.
	if err := spec.dirs.writeRefusal(); err != nil {
		// Who asked, as the authorized line further on would say: the
		// refusal comes before it.
		c.logger.Error("deploy failed", zap.String("version", version), zap.String("via", by),
			zap.String("source", "push"), zap.String("remote", r.RemoteAddr), zap.Error(err))
		// Before the pipeline, so its own report does not run: the box's
		// failure, told as the pipeline's would be.
		ma.lifecycle(c, LifecycleEvent{Name: EventDeployFailed, Release: version, DeployedBy: by,
			Summary: "deploy of " + version + " failed before it began: the app's directories are not as motel made them"})
		return h.mapDeployResult(w, ma, err)
	}
	stage := h.stage
	if stage == nil {
		stage = stageUpload
	}
	archive, digest, err := stage(r.Body, spec.dirs.tmp, spec.maxArtifactSize)
	var stgErr *stagingError
	switch {
	case errors.Is(err, errArtifactTooLarge):
		return RespondJSON(w, http.StatusRequestEntityTooLarge, wire.Error{
			Error: fmt.Sprintf("uploaded artifact exceeds max_artifact_size (%d bytes)", spec.maxArtifactSize)}, ma.redactorFor(statusSnapshot{}))
	case errors.As(err, &stgErr):
		// A local filesystem failure (mkdir/create/write/close, e.g. a
		// full disk) is a server error, like a failed URL download, and
		// reported as one: the pipeline's own report never runs.
		ma.lifecycle(c, LifecycleEvent{Name: EventDeployFailed, Release: version, DeployedBy: by,
			Summary: "deploy of " + version + " failed before it began: the upload could not be staged on the box"})
		return RespondJSON(w, http.StatusInternalServerError, wire.Error{Error: "staging upload: " + err.Error()}, ma.redactorFor(statusSnapshot{}))
	case err != nil:
		// Otherwise the body read itself failed — a bad request.
		return RespondJSON(w, http.StatusBadRequest, wire.Error{Error: "reading upload: " + err.Error()}, ma.redactorFor(statusSnapshot{}))
	}
	// Backstop cleanup: fetch removes the archive once it extracts it,
	// but if the pipeline returns before fetch it would leak.
	defer func() { _ = os.Remove(archive) }()
	// The upload's digest is the push's pin: recorded with the version,
	// and what a later push of the same version is matched by
	// (relaunchRefusal). The bytes are the caller's own, so there is
	// nothing to check it against here.
	req := deployRequest{version: version, localArchive: archive, sha256: digest, by: by}
	h.logDeployAuthorized(r, ma, req)
	return h.finishDeploy(w, r, ma, func(onPhase func(string)) error {
		req.onPhase, c.onPhase = onPhase, onPhase
		return ma.deployLocked(r.Context(), req, c)
	})
}

// deployRollback relaunches an already-extracted on-disk release.
func (h *Handler) deployRollback(w http.ResponseWriter, r *http.Request, ma *managedApp, by string) error {
	version := r.URL.Query().Get("rollback")
	if !validVersion(version) {
		return RespondJSON(w, http.StatusUnprocessableEntity, wire.Error{Error: fmt.Sprintf("rollback version must match %s", versionRe)}, ma.redactorFor(statusSnapshot{}))
	}
	return h.runDeploy(w, r, ma, deployRequest{version: version, rollback: true}, by)
}

// deployRecord answers GET /<app>?deploy=<version> with the recorded
// outcome of that version's latest deploy (deploys.go).
func (h *Handler) deployRecord(w http.ResponseWriter, ma *managedApp, version string) error {
	if !validVersion(version) {
		return RespondJSON(w, http.StatusUnprocessableEntity, wire.Error{Error: fmt.Sprintf("deploy query param must match %s", versionRe)}, ma.redactorFor(statusSnapshot{}))
	}
	c := ma.snapshot()
	if c.spec == nil {
		// An app the pool still holds but no loaded config describes
		// (status() makes the same allowance): nowhere to read from.
		return RespondJSON(w, http.StatusServiceUnavailable, wire.Error{Error: "app has no configuration loaded"}, ma.redactorFor(statusSnapshot{}))
	}
	rec, err := readDeployRecord(c.spec.dirs, version)
	// The requested version, and the record's own pin, are names the
	// filter should let through whether or not the status still lists
	// them by the time it is built (a prune, a reload between the read
	// and the response): safe strings like the versions the status
	// names, under the same rule (redactorFor).
	s := ma.status()
	head, _ := recordHead(rec, version+".json")
	head.Version = version
	s.Deploys = append(s.Deploys, head)
	switch {
	case errors.Is(err, errNoDeployRecord):
		return RespondJSON(w, http.StatusNotFound, wire.Error{Error: fmt.Sprintf("no deploy recorded for version %s", version)}, ma.redactorFor(s))
	case err != nil:
		return RespondJSON(w, http.StatusInternalServerError, wire.Error{Error: "reading deploy record: " + err.Error()}, ma.redactorFor(s))
	}
	// Through the live filter like every body; its outcome words
	// stand outside the filter there (redact.go, rule 3).
	return respondFiltered(w, http.StatusOK, ma.redactorFor(s).redactJSON(rec))
}

// runDeploy records who authorized it, runs the pipeline, and maps
// the outcome to a status code — or, when the client asked for it,
// streams the phases as they happen and ends with the same outcome.
func (h *Handler) runDeploy(w http.ResponseWriter, r *http.Request, ma *managedApp, req deployRequest, by string) error {
	req.by = by
	h.logDeployAuthorized(r, ma, req)
	if !wantsStream(r) {
		return h.mapDeployResult(w, ma, ma.Deploy(r.Context(), req))
	}
	st := newDeployStream(w, ma)
	req.onPhase = st.phase
	return st.finish(ma, ma.Deploy(r.Context(), req))
}

// finishDeploy is the push path's runDeploy: the pipeline already ran
// under the lock the handler took, and its outcome is written the way
// the request asked for.
func (h *Handler) finishDeploy(w http.ResponseWriter, r *http.Request, ma *managedApp, run func(onPhase func(string)) error) error {
	if !wantsStream(r) {
		return h.mapDeployResult(w, ma, run(nil))
	}
	st := newDeployStream(w, ma)
	return st.finish(ma, run(st.phase))
}

func (h *Handler) logDeployAuthorized(r *http.Request, ma *managedApp, req deployRequest) {
	fields := []zap.Field{
		zap.String("app", ma.name), zap.String("via", req.by),
		zap.String("source", req.source()), zap.String("remote", r.RemoteAddr),
	}
	if req.sha256 != "" {
		// What the pull is bound to, or what the push uploaded, for the
		// operator reading back what a deploy ran: a digest, never a
		// secret.
		fields = append(fields, zap.String("sha256", req.sha256))
	}
	h.logger.Info("deploy authorized", fields...)
}

// mapDeployResult turns a pipeline outcome into the webhook response.
func (h *Handler) mapDeployResult(w http.ResponseWriter, ma *managedApp, err error) error {
	code, body, rd := deployOutcome(ma, err)
	if body == nil {
		// The client hung up mid-deploy; nobody is reading this.
		return nil
	}
	return RespondJSON(w, code, body, rd)
}

// deployOutcome is the status code and body a pipeline outcome maps
// to, with the app's filter for it; a nil body is a client that hung
// up, which nothing is written for.
func deployOutcome(ma *managedApp, err error) (int, any, *Redactor) {
	status := ma.status()
	// A refused pin names two digests, and a digest is the shape of a
	// token the filter masks: told they are names — the deployer's own
	// pin, and what the host served — the 422 can say which is which.
	// Not a mismatch: no names (the zero value's are empty, and
	// NewRedactor skips those).
	var dm digestMismatch
	_ = errors.As(err, &dm)
	rd := ma.redactorFor(status, dm.names()...)
	var vErr validationError
	switch {
	case err == nil:
		return http.StatusOK, status, rd
	case errors.Is(err, errDeployInProgress):
		return http.StatusConflict, wire.Error{Error: err.Error()}, rd
	case errors.As(err, &vErr):
		return http.StatusUnprocessableEntity, wire.Error{Error: err.Error()}, rd
	case errors.Is(err, context.Canceled):
		return 0, nil, rd
	default:
		return http.StatusInternalServerError, wire.Failure{
			Error:  err.Error(),
			Status: status, // shows the old version still serving
		}, rd
	}
}

// ndjson is the media type a client sends in Accept to have the deploy
// streamed: one JSON object per line as the pipeline moves, the last
// being the body the single response carries, plus its status code.
const ndjson = "application/x-ndjson"

// wantsStream reports whether the request asked for the stream: the
// media type, exactly, among the items of every Accept header sent (a
// list, with or without parameters), not refused by a q of zero, and
// nothing that merely resembles it.
func wantsStream(r *http.Request) bool {
	for _, v := range r.Header.Values("Accept") {
		for _, item := range strings.Split(v, ",") {
			mt, params, err := mime.ParseMediaType(strings.TrimSpace(item))
			if err != nil || mt != ndjson {
				continue
			}
			if q, ok := params["q"]; ok {
				// A qvalue is 0 to 1; anything else — NaN, Inf, 2 —
				// is not one, and does not switch the protocol.
				if f, err := strconv.ParseFloat(q, 64); err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 || f > 1 {
					continue
				}
			}
			return true
		}
	}
	return false
}

// deployStream writes a deploy as JSON lines. The stream begins with
// the first phase: from then on the status line is 200 whatever the
// outcome — it went out before the outcome existed — and the last line
// carries "http_status", the code the single response would have had,
// which a client that streams reads instead. An outcome reached before
// any phase (a version refused, a deploy already running) has nothing
// streamed yet and is the single response with its real code. Every
// line passes the app's filter.
type deployStream struct {
	w     http.ResponseWriter
	ma    *managedApp
	begun bool
}

func newDeployStream(w http.ResponseWriter, ma *managedApp) *deployStream {
	return &deployStream{w: w, ma: ma}
}

// write sends one filtered JSON object as a line and flushes it. A
// write that fails is a client gone; the deploy carries on (its
// context says when to stop) and the last line's write reports it.
func (s *deployStream) write(filtered string) error {
	if !s.begun {
		s.begun = true
		s.w.Header().Set("Content-Type", ndjson)
		s.w.Header().Set("Cache-Control", "no-store")
		s.w.WriteHeader(http.StatusOK)
	}
	if _, err := s.w.Write(append([]byte(filtered), '\n')); err != nil {
		return err
	}
	_ = http.NewResponseController(s.w).Flush()
	return nil
}

// phase is the pipeline's listener: one line per phase entered. Like
// the last line, its fixed fields go on after the filter: a filter
// that withholds the whole body (an unreadable env_file) then leaves
// its diagnostic in the line and the event and phase beside it.
func (s *deployStream) phase(phase string) {
	raw, err := json.Marshal(map[string]any{"at": time.Now().UTC()})
	if err != nil {
		return
	}
	filtered := s.ma.redactorFor(statusSnapshot{}).redactJSON(raw)
	_ = s.write(fmt.Sprintf(`%s,"event":%q,"phase":%q}`, filtered[:len(filtered)-1], wire.EventPhase, phase))
}

// finish writes the outcome as the last line: the single response's
// body with "event":"done" and "http_status" added — or, when no
// phase was ever streamed, as the single response itself.
func (s *deployStream) finish(ma *managedApp, err error) error {
	code, body, rd := deployOutcome(ma, err)
	if body == nil {
		return nil
	}
	if !s.begun {
		return RespondJSON(s.w, code, body, rd)
	}
	// The single response's bytes, filtered as RespondJSON filters
	// them, with two fields appended after the filter: appended, not
	// re-marshalled, so the line has the single response's order, which
	// is whatever the filter left (a redaction re-marshals the top
	// level, sorting its keys; a nested object keeps its order — see
	// withField). A client reading "the last phase" by position relies
	// on it: the examples' deploy.sh reads a failure's, and every phase
	// in a failure body is inside "status", in statusSnapshot's order.
	// And after, so the outcome's markers survive a filter that
	// withholds the whole body (an unreadable env_file). redactJSON
	// always returns one JSON object, so the closing brace is where it
	// ends.
	raw, mErr := json.Marshal(body)
	if mErr != nil {
		return mErr
	}
	filtered := rd.redactJSON(raw)
	return s.write(fmt.Sprintf(`%s,"event":%q,"http_status":%d}`, filtered[:len(filtered)-1], wire.EventDone, code))
}

// isGzipUpload reports whether the request body is a pushed artifact.
func isGzipUpload(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	// Media types are case-insensitive (RFC 7231), so normalize before
	// matching — otherwise `Application/Gzip` would fall through to the
	// JSON URL path and fail as invalid JSON.
	switch strings.ToLower(strings.TrimSpace(ct)) {
	case "application/gzip", "application/x-gzip", "application/octet-stream":
		return true
	}
	return false
}

// errArtifactTooLarge signals a pushed upload that exceeded the cap.
var errArtifactTooLarge = errors.New("artifact too large")

// stagingError marks an upload failure that is a local filesystem/server
// fault (mkdir, create, disk write, close), as opposed to a bad request
// body — so the handler can answer 5xx vs 400.
type stagingError struct{ err error }

func (e *stagingError) Error() string { return e.err.Error() }
func (e *stagingError) Unwrap() error { return e.err }

// writeTracker records whether a write to the underlying file failed, so
// io.Copy's combined error can be attributed to the disk (server) rather
// than the request body (client).
type writeTracker struct {
	w      io.Writer
	failed bool
}

func (t *writeTracker) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	if err != nil {
		t.failed = true
	}
	return n, err
}

// stageUpload streams the request body to a temp file, capped at
// maxBytes, hashing it on the same pass. Returns the path and the
// sha256 of the staged bytes as lowercase hex; the caller owns cleanup.
// Local filesystem failures come back wrapped in *stagingError (→
// 5xx); a body-read failure comes back bare (→ 400); an over-cap body →
// errArtifactTooLarge.
func stageUpload(body io.Reader, tmpDir string, maxBytes int64) (string, string, error) {
	if err := os.MkdirAll(tmpDir, 0o750); err != nil {
		return "", "", &stagingError{err}
	}
	f, err := os.CreateTemp(tmpDir, pushTempPattern)
	if err != nil {
		return "", "", &stagingError{err}
	}
	path := f.Name()
	// One byte past the cap proves it is oversized.
	wt := &writeTracker{w: f}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(wt, h), io.LimitReader(body, maxBytes+1))
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(path)
		if wt.failed {
			return "", "", &stagingError{copyErr} // disk write failed
		}
		return "", "", copyErr // request body read failed
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return "", "", &stagingError{closeErr}
	}
	if n > maxBytes {
		_ = os.Remove(path)
		return "", "", errArtifactTooLarge
	}
	return path, hex.EncodeToString(h.Sum(nil)), nil
}

// RespondJSON writes every body the webhook sends, through the
// response filter (redact.go). r is the app's filter — every site
// with the app in scope passes ma.redactorFor, the 405 included —
// and nil only for the bodies written before an app is known (401,
// 404), where the shape and entropy layers still apply. When a
// known secret was found the object gains a `redacted_env` field
// naming the keys. The box webhook writes its bodies through it too,
// with a filter from NewRedactor.
func RespondJSON(w http.ResponseWriter, code int, v any, r *Redactor) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return respondFiltered(w, code, r.redactJSON(raw))
}

// respondFiltered writes a body that has already passed the filter.
func respondFiltered(w http.ResponseWriter, code int, filtered string) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, err := w.Write(append([]byte(filtered), '\n'))
	return err
}

// Interface guards.
var (
	_ caddy.Provisioner           = (*Handler)(nil)
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
)
