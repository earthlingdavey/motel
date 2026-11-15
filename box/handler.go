package box

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"filippo.io/age"
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"

	"github.com/smallhoursorg/motel/box/proof"
	"github.com/smallhoursorg/motel/liveswap"
	"github.com/smallhoursorg/motel/liveswap/deploytrust"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

func init() {
	caddy.RegisterModule(Handler{})
}

// Handler is `box_webhook` (DESIGN-box.md, "Handler contract"). It
// answers on exactly the path `/` of its site and passes every other
// path to the next handler:
//
//	GET  /              the baseline: {commit, box_webhook, sha256, recipient}
//	GET  /?result=<id>  a push's result (push.go)
//	POST /              a push (push.go, admission.go, validate.go)
//
// Another method on `/` is 405 before anything else. Every GET and
// POST on `/` is authenticated first, on the limiter liveswap's
// webhook uses, so an unauthenticated caller gets the same flat 401 a
// deploy host gives. Every body passes liveswap's response filter.
type Handler struct {
	app     *App
	logger  *zap.Logger
	limiter *deploytrust.Limiter

	// Where the handler reads: installedFile, exchangeDir and
	// secretsDir, set in Provision; a test's temporary files otherwise.
	installed string
	dir       string
	secrets   string
	agePub    string

	// A push's seams: the proof's verifier (ssh-keygen as the motel
	// uid, step 5), step 6's children, the clock step 8 waits on, and
	// the drop's test hooks.
	verifier  *proof.Verifier
	validator validator
	clock     Clock
	hooks     handlerHooks
	// slot is pushSlot, the one push in memory, shared across reloads;
	// upload, when not zero, stands for uploadTimeout in a test.
	slot   chan struct{}
	upload time.Duration
}

// CaddyModule returns the Caddy module information.
func (Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.box_webhook",
		New: func() caddy.Module { return new(Handler) },
	}
}

// Provision binds the handler to the box app. AppIfConfigured, not
// App: App would instantiate an empty `box` app, and the webhook would
// then fail every request rather than the config load.
func (h *Handler) Provision(ctx caddy.Context) error {
	h.logger = ctx.Logger()
	app, err := ctx.AppIfConfigured("box")
	if err != nil {
		return fmt.Errorf("box_webhook needs the box global option, which says who may push and who may sign: %w", err)
	}
	h.app = app.(*App)
	h.limiter = deploytrust.Shared()
	h.installed, h.dir, h.secrets, h.agePub = installedFile, exchangeDir, secretsDir, agePubFile
	h.verifier = &proof.Verifier{} // RunAs 0: as the motel uid, in the service's PrivateTmp
	h.validator = newChildValidator(h.x("stage"))
	h.clock = realClock{}
	h.slot = pushSlot
	return nil
}

// The handler's own messages (DESIGN-box.md, "Message catalogue").
const (
	msgMethod     = "method not allowed"
	msgNoBaseline = "this box has no baseline; motel init <sha>, as root inside the box's directory, sets one"
)

// ServeHTTP answers on `/` and passes every other path on.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if r.URL.Path != "/" {
		return next.ServeHTTP(w, r)
	}
	// A query is present when the target has a `?`, an empty one
	// included (ForceQuery): `GET /?` is a result poll with no id, not
	// the baseline.
	queried := r.URL.RawQuery != "" || r.URL.ForceQuery
	switch {
	case r.Method == http.MethodGet && !queried:
		return h.status(w, r)
	case r.Method == http.MethodGet:
		return h.result(w, r)
	case r.Method == http.MethodPost:
		return h.push(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		return respond(w, http.StatusMethodNotAllowed, errorBody(msgMethod))
	}
}

// authenticate is the preamble (deploytrust): ok=false means the
// refusal is written. route names the request in the preamble's
// journal lines, beside the address.
func (h *Handler) authenticate(w http.ResponseWriter, r *http.Request, route string) (deploytrust.Identity, bool, error) {
	ident, refusal := h.limiter.Authenticate(r, h.app.verifiers, h.logger, "box_request", route)
	if refusal != nil {
		return ident, false, refusal.Write(func(code int, body any) error { return respond(w, code, body) })
	}
	return ident, true, nil
}

// status is `GET /`: the commit the box runs, from applied.json, and
// the host and digest of the file it runs, read now — the digest a
// push's no_change fast path compares (step 4) — so a console edit since the
// last apply shows; and the box's key for secrets, when it has one.
func (h *Handler) status(w http.ResponseWriter, r *http.Request) error {
	if _, ok, err := h.authenticate(w, r, "status"); !ok {
		return err
	}
	base, file, what, err := h.readState()
	switch {
	case what == readingBaseline && errors.Is(err, fs.ErrNotExist):
		return respond(w, http.StatusConflict, errorBody(msgNoBaseline))
	case err != nil:
		return h.boxError(w, what, err)
	}
	shape, err := Walk(file)
	if err != nil {
		var ref *Refusal
		if !errors.As(err, &ref) {
			return h.boxError(w, readingInstalled, err)
		}
		// The box runs a file the channel cannot speak for (a console
		// edit that imports, say): the operator's to mend, as before
		// init, not the box failing.
		h.logger.Warn("box webhook runs a Caddyfile it cannot speak for", zap.String("error", proof.Bound(err.Error())))
		return respond(w, http.StatusConflict, errorBody(runningRefused(ref).Error()))
	}
	sum := digest(file) // what the fast path compares
	body := wire.BoxStatus{Commit: base.SHA, BoxWebhook: shape.Host, SHA256: sum, Recipient: h.recipient()}
	safe := []string{base.SHA, shape.Host, sum}
	if body.Recipient != "" { // a bech32 key is what the entropy layer catches
		safe = append(safe, body.Recipient)
	}
	return respond(w, http.StatusOK, body, safe...)
}

// readingKey is what the journal names when age.pub cannot be read.
const readingKey = "the box's key for secrets (age.pub)"

// recipient is age.pub's one line, held to an X25519 recipient, or ""
// when there is none: a box that has not run `motel box key`. One
// that cannot be read, or is not exactly one X25519 recipient, is
// journaled and omitted all the same, so a damaged key file never
// blocks `GET /` and with it a push of the Caddyfile; the workflow
// reads the omission as "this box has no key", whose remedy, `motel
// box key`, rewrites age.pub from the identity.
func (h *Handler) recipient() string {
	b, err := readFile(h.agePub, maxAgePub, false)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return ""
	case err != nil:
		h.logStateError(readingKey, err)
		return ""
	}
	line, ok := strings.CutSuffix(string(b), "\n")
	if r, err := age.ParseX25519Recipient(line); ok && err == nil && r.String() == line {
		return line
	}
	h.logStateError(readingKey, errors.New("not one X25519 recipient line"))
	return ""
}

// What readState names when a file fails it.
const (
	readingBaseline  = "the baseline"
	readingInstalled = "the Caddyfile this box runs"
)

// readState reads the baseline and the installed file, each once, as
// `GET /` and a push both need them. On an error, what names the file;
// a missing applied.json is fs.ErrNotExist, the box before `init`.
func (h *Handler) readState() (base *applied, installed []byte, what string, err error) {
	if base, err = readApplied(h.dir); err != nil {
		return nil, nil, readingBaseline, err
	}
	if installed, err = readFile(h.installed, proof.MaxCaddyfile, true); err != nil {
		return nil, nil, readingInstalled, err
	}
	return base, installed, "", nil
}

// envDigest is an installed env file's digest as the fast path compares
// it (step 4): read as root reads it — no link followed, held to a
// regular file, to maxEnvRead — by the motel group, whose files these
// are; and only for a file that is still as root wrote it, 0640 and
// owned as the exchange tree is, so that a file whose mode or owner
// drifted goes to root, which writes it again ("Values and the env
// file"), rather than staying readable or writable on every replay. Any
// error is the caller's "not unchanged".
func (h *Handler) envDigest(app string) (string, error) {
	path := filepath.Join(h.secrets, app+".env")
	base, err := os.Lstat(h.dir)
	if err != nil {
		return "", err
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	want, okBase := base.Sys().(*syscall.Stat_t)
	got, okFile := fi.Sys().(*syscall.Stat_t)
	if !okBase || !okFile || fi.Mode() != 0o640 || got.Uid != want.Uid || got.Gid != want.Gid {
		return "", errors.New("not as root writes it")
	}
	b, err := readFile(path, maxEnvRead, false)
	if err != nil {
		return "", err
	}
	return digest(b), nil
}

// boxError answers a failure that is the box's, never the caller's:
// 500 with the error, and the journal line, both bounded.
func (h *Handler) boxError(w http.ResponseWriter, what string, err error) error {
	h.logStateError(what, err)
	return respond(w, http.StatusInternalServerError, errorBody("could not read "+what+": "+proof.Bound(err.Error())))
}

// logStateError journals a state file the handler cannot read, bounded.
// Only an authenticated request reaches one: the preamble decides who
// can make the line, not how many — a token holder can repeat `GET /`
// against a box whose own files fail, and each request is a line, as
// each is an answer.
func (h *Handler) logStateError(what string, err error) {
	h.logger.Error("box webhook could not read its state", zap.String("reading", what), zap.String("error", proof.Bound(err.Error())))
}

func errorBody(msg string) wire.Error { return wire.Error{Error: msg} }

// respond writes body through liveswap's response filter: the shape
// and entropy layers, with safe — the protocol's own commit, digest and
// host, each one the caller checked against its grammar — held out of
// them, since a 40-hex commit and a 64-hex digest are exactly what the
// entropy layer exists to catch.
func respond(w http.ResponseWriter, code int, body any, safe ...string) error {
	return liveswap.RespondJSON(w, code, body, liveswap.NewRedactor(nil, safe))
}

var (
	_ caddy.Provisioner           = (*Handler)(nil)
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
)
