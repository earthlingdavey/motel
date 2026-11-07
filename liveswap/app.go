package liveswap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/smallhoursorg/motel/liveswap/deploytrust"
	"go.uber.org/zap"
)

// appSpec is the fully-defaulted, internal form of one app's config.
// A fresh spec is installed on every config load; the managedApp holds
// it behind a lock and snapshots it at the start of each deploy, so a
// mid-deploy reload never sees a torn config.
type appSpec struct {
	name               string
	command            []string
	preStart           []string
	env                map[string]string
	envFile            string
	trust              []deploytrust.Source
	healthPath         string // "" = no HTTP check (health_path off)
	healthInterval     time.Duration
	healthTimeout      time.Duration
	soak               time.Duration
	deadline           time.Duration
	drain              time.Duration
	grace              time.Duration
	watchdogOn         bool
	wdFailures         int
	wdGrace            time.Duration
	wdRestarts         int
	wdWindow           time.Duration
	keep               int
	maxArtifactSize    int64
	maxArtifactEntries int
	deployLogLines     int // lines of the app's journal in a failed deploy's response; 0 = none
	caps               resourceCaps
	allowInsecure      bool
	allowlist          []artifactAllowEntry
	dirs               appDirs
}

// archiveLimits is what one artifact of this app may cost to extract.
func (s *appSpec) archiveLimits() archiveLimits {
	return archiveLimits{
		maxBytes:   s.maxArtifactSize * decompressionRatioCap,
		maxEntries: s.maxArtifactEntries,
	}
}

// capWarnFraction is how close to a cap an artifact may come before a
// deploy warns: a cap an app grows into with no notice is an outage on
// the deploy that crosses it, so the operator hears several deploys
// early and raises the directive on their own schedule.
const capWarnFraction = 0.75

// warnNearCaps logs what the artifact cost and, past capWarnFraction
// of either cap, a Warn naming the directive to raise.
func (s *appSpec) warnNearCaps(logger *zap.Logger, st archiveStats) {
	lim := s.archiveLimits()
	logger.Info("artifact extracted",
		zap.Int("entries", st.entries), zap.Int("max_entries", lim.maxEntries),
		zap.Int64("bytes", st.bytes), zap.Int64("max_bytes", lim.maxBytes))
	if float64(st.entries) >= capWarnFraction*float64(lim.maxEntries) {
		logger.Warn("artifact approaching max_artifact_entries; raise it before a deploy fails on it",
			zap.Int("entries", st.entries), zap.Int("max_entries", lim.maxEntries))
	}
	if float64(st.bytes) >= capWarnFraction*float64(lim.maxBytes) {
		logger.Warn("artifact approaching the decompressed byte cap (max_artifact_size x 10); raise max_artifact_size before a deploy fails on it",
			zap.Int64("bytes", st.bytes), zap.Int64("max_bytes", lim.maxBytes))
	}
}

// deployPayload is the JSON body of a URL deploy — the only request
// body the webhook ever decodes into a struct. It is deliberately a
// separate type from deployRequest: the request carries server-side
// fields (the staged upload path, the rollback flag, the authorizing
// source) that must never be reachable from a body, and keeping them
// out of the decoded type makes that structural rather than a matter
// of field visibility — for readers and for static analysis alike,
// which otherwise taints every field of a decoded struct.
// TestDeployPayloadIsTheOnlyWireType pins the split.
type deployPayload struct {
	URL        string `json:"url"`
	Version    string `json:"version"`
	AuthHeader string `json:"auth_header"`
	// SHA256 is the artifact's digest as sha256sum prints it, 64 hex
	// characters; optional. When set, the download must hash to it.
	SHA256 string `json:"sha256"`
}

// request is the one place a payload becomes a request: exactly the
// four wire fields cross over, everything else stays zero.
func (p deployPayload) request() deployRequest {
	return deployRequest{url: p.URL, version: p.Version, authHeader: p.AuthHeader, sha256: p.SHA256}
}

// deployRequest is the validated deploy request handed to the
// pipeline. It is never decoded from the wire (see deployPayload). The
// three sources are mutually exclusive: a URL to pull (the default), a
// pushed archive already staged on disk (localArchive), or a rollback
// to an existing on-disk release (rollback).
type deployRequest struct {
	url        string
	version    string
	authHeader string
	// sha256 is the artifact's digest, lowercase hex, or empty. For a
	// URL pull it is the pin the bytes must download to
	// (parseDeployPayload normalises it); for a push it is the digest
	// of the bytes the caller uploaded (stageUpload hashes them as they
	// land, so the request is never pinned to anything but itself). A
	// rollback fetches nothing and carries none. Either digest is
	// recorded as the version's pin, and is what a deploy of the
	// running version is matched by (relaunchRefusal).
	sha256 string
	// localArchive is the path of an already-staged pushed tarball (an
	// os.CreateTemp name under the app's own tmp dir, chosen by the
	// handler — never request input); empty for a URL pull.
	localArchive string
	// rollback relaunches an existing on-disk release/<Version> without
	// fetching or extracting anything.
	rollback bool
	// by is who this deploy is recorded under: the label of the trust
	// source that authorized it and the token's attribution claims
	// (attribute, deploytrust.go).
	by string
	// onPhase, when set, is told each phase the pipeline enters, on the
	// deploy's own goroutine before the phase's work begins — a
	// streaming response's line writer (handler.go). Server-side, never
	// from the wire.
	onPhase func(phase string)
}

// source names the deploy's artifact source, for the audit log.
func (r deployRequest) source() string {
	switch {
	case r.rollback:
		return "rollback"
	case r.localArchive != "":
		return "push"
	default:
		return "url"
	}
}

// deployResult records the outcome of the most recent deploy attempt
// for the status endpoint and webhook responses.
type deployResult struct {
	Version    string    `json:"version"`
	Status     string    `json:"status"` // "succeeded" | "failed"
	Error      string    `json:"error,omitempty"`
	Phase      string    `json:"phase,omitempty"`       // phase reached when it failed
	By         string    `json:"deployed_by,omitempty"` // the trust source that authorized it, and the token's attribution claims
	SHA256     string    `json:"sha256,omitempty"`      // the pin the deployer asserted, the digest of the bytes a push uploaded, or a rollback's carried from the record it replaces (recordedPin); never a hash of an unpinned download
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	// Note says what the deploy did that its outcome words do not: a
	// deploy or rollback naming the running version relaunched it from
	// the release already on disk (relaunchNote). Empty otherwise. It
	// is in last_deploy and the response only — a relaunch writes no
	// record, so the version's record keeps the deploy that put it
	// there, its pin and its times.
	Note string `json:"note,omitempty"`
	// What the artifact cost against max_artifact_entries and the
	// decompressed byte cap — the files, directories and links
	// extraction created, and its bytes — so an app growing toward
	// either is visible from the status endpoint before a deploy fails
	// on it. Zero for a rollback (nothing is extracted) and for a
	// deploy that failed before extraction.
	ArtifactEntries int   `json:"artifact_entries"`
	ArtifactBytes   int64 `json:"artifact_bytes"`
	// Phases is each phase the deploy passed through, in order, with
	// how long it took: where the time went, and where it stopped.
	Phases []phaseTiming `json:"phases,omitempty"`
	// Detail is why a failed deploy failed, from the app's side: how
	// its process ended, what its health endpoint answered, the last
	// lines it wrote. Nil for a success, and for a failure before a
	// launch (nothing of the app's ran).
	Detail *deployDetail `json:"detail,omitempty"`
	// RedactedEnv is the env keys whose values the detail's early
	// replacement found (failureDetail): by the time the response
	// filter runs they are markers already, so it reports them from
	// here — this field, wherever the result sits in a body, is hoisted
	// into the body's own `redacted_env` (withField, redact.go).
	RedactedEnv []string `json:"redacted_env,omitempty"`
}

type phaseTiming struct {
	Name      string    `json:"name"`
	StartedAt time.Time `json:"started_at"`
	Seconds   float64   `json:"seconds"`
}

// The outcome vocabulary: what a deploy's status and phase may say.
// A record's status is one of these and its phase one of these or
// absent (recordHead, deploys.go); the response filter holds the
// words out of every layer where they stand as an outcome (redact.go,
// rule 3), so they carry nothing of the app's and nothing rewrites
// them.
var (
	outcomeStatuses = map[string]bool{"succeeded": true, "failed": true}
	outcomePhases   = map[string]bool{"downloading": true, "extracting": true, "preparing": true, "starting": true, "soaking": true, "promoting": true, "draining": true, "stopping_old": true}
)

// deployDetail is the app's side of a failed deploy. Every string in
// it passes the response filter before it is written (redact.go), and
// the known values are replaced as it is built, before the tail and
// body caps cut, so a cap never leaves a fragment of one.
type deployDetail struct {
	// Exit is how the process ended, from the runner: "exit status 3",
	// "killed by signal 9 (killed)". The pre_start's when that failed,
	// otherwise the app's, when it died before becoming healthy.
	Exit string `json:"exit,omitempty"`
	// Probe is the last health probe that completed with a non-2xx
	// answer: the status, a redirect's target, the first bytes of the
	// body — where "Invalid HTTP_HOST header" or "no such table" is.
	// The body is the app's output and deploy_log_lines 0 leaves it
	// out, as it does the tail; the status and location stay.
	Probe *probeDetail `json:"probe,omitempty"`
	// LogTail is the last lines the app's units wrote to the journal
	// since the deploy began — pre_start and app alike — bounded by
	// deploy_log_lines and 8 KiB; LogTailTruncated says when a cap
	// dropped lines. LogTailError says why there is no tail when there
	// should have been one (journalctl missing, the journal unreadable).
	LogTail          []string `json:"log_tail,omitempty"`
	LogTailTruncated bool     `json:"log_tail_truncated,omitempty"`
	LogTailError     string   `json:"log_tail_error,omitempty"`
}

type probeDetail struct {
	Status   int    `json:"status"`
	Location string `json:"location,omitempty"`
	Body     string `json:"body,omitempty"`
}

// instance is one running version of an app.
type instance struct {
	version string
	nonce   string     // names the unit and the socket (newNonce)
	socket  string     // the unix socket the app listens on
	sock    *socketRef // how motel dials it: pinned by inode under proxy/, never by the app's name
	handle  handle
}

// Sentinel errors the webhook handler maps to status codes.
var (
	errDeployInProgress = errors.New("a deploy is already in progress for this app")
)

// validationError is a bad request (422), as opposed to a deploy that
// failed on its own merits (500).
type validationError struct{ msg string }

func (e validationError) Error() string { return e.msg }

// managedApp owns everything about one app that must survive config
// reloads: the running instance, the active socket the proxy reads, and
// the single-deploy-at-a-time lock. It lives in the package-level
// UsagePool (see liveswap.go), never inside a config's module instance.
type managedApp struct {
	name string

	// specMu guards spec and every collaborator below it: they are all
	// (re)wired on config load while deploys may be reading them, so
	// readers take the snapshot accessors, never the fields.
	specMu    sync.RWMutex
	spec      *appSpec
	verifiers []deploytrust.Verifier // resolved deploy-auth trust sources; rewired every reload
	runner    runner
	prober    prober
	fetch     fetcher
	clock     clock
	store     stateStore
	journal   journalReader
	logger    *zap.Logger

	// deployMu serializes deploys per app. TryLock (not a queue): a
	// concurrent webhook gets an immediate 409 and CI can retry. The
	// one blocking Lock is Destruct's, on the app's removal, and it is
	// never released. Every other holder but the watchdog lets go
	// through unlockDeploy or endDeploy, which wake the watchdog.
	deployMu sync.Mutex
	// deploysEnded counts deploys that have let go of deployMu
	// (endDeploy): what tells a restart waiting out its backoff that a
	// deploy over its instance ended, from any other wake-up.
	deploysEnded atomic.Uint64

	// mu guards current, phase, phases and lastDeploy.
	mu         sync.Mutex
	current    *instance
	phase      string
	phases     []phaseTiming // the running deploy's phases, in order
	lastDeploy *deployResult

	// activeSocket is what GetUpstreams reads on every request; storing
	// it is the cutover. nil = nothing serving yet.
	activeSocket atomic.Pointer[socketRef]

	// Watchdog plumbing. The goroutine is pool-scoped like everything
	// else here: started once (first Start), never touched by
	// reloads, torn down in Destruct BEFORE the child is stopped so a
	// mid-restart watchdog can never orphan a fresh process.
	wdStarted bool // under specMu
	wdCtx     context.Context
	wdCancel  context.CancelFunc

	// configuredBy is the config that installed the current spec, and
	// prev what it replaced — so a candidate config Caddy rejects
	// after our Start (another app's Start failed) can be rolled back
	// by its Cleanup, leaving the still-serving config's definition in
	// place. Under specMu.
	configuredBy any
	prev         *appConfigState
	wdWG         sync.WaitGroup
	wdNotify     chan struct{} // buffered(1); poked whenever current changes
	wd           watchdogState

	// secrets is every env_file KEY=VALUE pair this process has
	// rendered for a launch, or read for the filter after a restart,
	// kept for the life of the process so a value rotated since stays
	// redacted from responses. What a still-running instance was
	// launched with before a restart is not known: only the file as it
	// is now. Under secretsMu; read by the handler's response path,
	// written by deploys, relaunches and configure.
	secretsMu sync.Mutex
	secrets   []string
	// secretsFrom is the env_file path whose contents are in secrets
	// (rendered by a launch, or read for the filter); a spec naming a
	// different path — a reload, or a rollback of one — is unread
	// until the next response's filter reads it. "" with
	// secretsLoaded set means the spec has no env_file.
	secretsFrom   string
	secretsLoaded bool

	// capsMu serializes syncCaps: its snapshot, compare and apply are
	// one step, so a publication's sync and a reload's cannot
	// interleave and leave the older spec's caps applied last. Taken
	// before specMu and mu, never while either is held.
	capsMu sync.Mutex
}

func newManagedApp(name string) *managedApp {
	return &managedApp{name: name, phase: "idle", wdNotify: make(chan struct{}, 1)}
}

// rememberSecrets records a launch's env_file pairs for the response
// filter (redact.go): path is the env_file the launch's spec named
// and kvs what it held. Values are added, never dropped: a secret
// that was ever handed to an app is one a response could still carry.
func (ma *managedApp) rememberSecrets(path string, kvs []string) {
	ma.secretsMu.Lock()
	defer ma.secretsMu.Unlock()
	ma.secretsFrom, ma.secretsLoaded = path, true
	ma.secrets = mergeSecrets(ma.secrets, kvs)
}

// redactorFor is the filter for a response about this app: the
// remembered secrets, with the versions the status names exempt from
// the heuristics (a git SHA as a version is high-entropy by design).
//
// After a motel restart the app is reattached without a launch, so
// nothing has rendered its env_file yet; and a reload can name a
// different env_file than the last launch rendered. Either way the
// current spec's file is unread, and the filter reads it here. A file
// that cannot be read now (a rewrite mid-flight, a mode not yet fixed)
// is not an error here — the next launch reports that — and the read
// is retried on the next response until it succeeds.
func (ma *managedApp) redactorFor(s statusSnapshot, names ...string) *Redactor {
	// The spec is read under secretsMu so the "which env_file is
	// loaded" check and the spec it is checked against are one
	// snapshot: a reload landing between the two could otherwise pair
	// the new spec with the old latch. secretsMu → specMu is the only
	// nesting: configure holds specMu and never takes secretsMu.
	ma.secretsMu.Lock()
	spec := ma.snapshot().spec
	var unread error
	if spec != nil && (!ma.secretsLoaded || ma.secretsFrom != spec.envFile) {
		if spec.envFile == "" {
			ma.secretsFrom, ma.secretsLoaded = "", true
		} else if kvs, err := parseEnvFile(spec.envFile); err == nil {
			ma.secrets = mergeSecrets(ma.secrets, kvs)
			ma.secretsFrom, ma.secretsLoaded = spec.envFile, true
		} else {
			unread = err
		}
	}
	kvs := append([]string(nil), ma.secrets...)
	ma.secretsMu.Unlock()
	// Safe strings are names: the app's, every version the status
	// names — the running one, the last deploy's, the releases on
	// disk, the recorded ones — the digests those deploys were pinned
	// to, the app's dirs, and any the caller's body names that the
	// status does not (the digests a refused pin reports,
	// deployOutcome). Whoever named one, it exempts nothing:
	// a safe string equal to a known value is dropped by NewRedactor
	// (redact.go, rule 1).
	safe := append([]string{ma.name, s.CurrentVersion}, s.AvailableVersions...)
	safe = append(safe, names...)
	if s.LastDeploy != nil {
		safe = append(safe, s.LastDeploy.Version, s.LastDeploy.SHA256)
	}
	for _, d := range s.Deploys {
		safe = append(safe, d.Version, d.pin())
	}
	if spec != nil {
		safe = append(safe, spec.dirs.root, spec.dirs.app, spec.dirs.releases, spec.dirs.shared, spec.dirs.run)
	}
	r := NewRedactor(kvs, safe)
	if unread != nil {
		// The values the running app holds are unknown to the filter,
		// and the heuristics alone are not the promise: no body until
		// the file reads. The reason names the file, not its contents.
		r.withhold = "the app's env_file could not be read, so its values are unknown to the filter (" + unread.Error() + ")"
	}
	return r
}

// mergeSecrets appends the pairs not already present.
func mergeSecrets(have, add []string) []string {
	known := make(map[string]bool, len(have))
	for _, kv := range have {
		known[kv] = true
	}
	for _, kv := range add {
		if !known[kv] {
			known[kv] = true
			have = append(have, kv)
		}
	}
	return have
}

// configure installs the latest spec and (re)wires collaborators. On
// the first provision the runner/prober/etc. are created; on reloads
// the spec, logger and state path are refreshed while the runner — and
// with it any running unit — is left untouched: a changed
// definition takes effect at the app's next launch (a deploy, a
// rollback, or a relaunch after a crash, a sustained health failure
// or a reboot), never by restarting a running app.
// appConfigState is what a config installs on a pooled app and what
// a rollback restores.
type appConfigState struct {
	spec      *appSpec
	verifiers []deploytrust.Verifier
	logger    *zap.Logger
	store     stateStore
}

// owner is the config installing this definition (see rollbackConfig).
// verifiers is the app's deploy_trust as the config resolved (and
// warmed) it in Provision; configure installs, never resolves, so the
// discovery cache a verifier carries is the one the handler sees.
// manager is the connection a runner built here talks to, passed in
// rather than reached for globally so a test can install its own; it
// is read only when this managedApp has no runner yet, because a
// pooled app keeps the runner — and so the connection — it was first
// started with across every later reload.
func (ma *managedApp) configure(owner any, spec *appSpec, verifiers []deploytrust.Verifier, logger *zap.Logger, clients *fetchClients, manager systemdConn) {
	ma.installSpec(owner, spec, verifiers, logger, clients, manager)
	// The caps are the one part of a definition that reaches the
	// instance already running, so they go with the install, after the
	// lock: a reload's changed caps apply now rather than at the next
	// launch. The first start has no instance yet; recovery syncs the
	// one it reattaches (ensureRunning).
	ma.syncCaps()
}

// installSpec is configure's locked half: the spec and its
// collaborators are swapped under specMu, and nothing that reads them
// back (syncCaps takes a snapshot) runs inside it.
func (ma *managedApp) installSpec(owner any, spec *appSpec, verifiers []deploytrust.Verifier, logger *zap.Logger, clients *fetchClients, manager systemdConn) {
	ma.specMu.Lock()
	defer ma.specMu.Unlock()
	changed := ma.spec != nil && !specEqual(ma.spec, spec)
	if ma.spec != nil {
		ma.prev = &appConfigState{spec: ma.spec, verifiers: ma.verifiers, logger: ma.logger, store: ma.store}
	}
	ma.configuredBy = owner
	ma.spec = spec
	// Deploy auth is not tied to the running process, so — unlike the
	// runner — it is rewired on every reload and takes effect at once.
	ma.verifiers = verifiers
	ma.logger = logger
	if ma.runner == nil {
		ma.runner = newSystemdRunner(manager, logger)
		ma.prober = &httpProber{clock: realClock{}}
		ma.fetch = &releaseFetcher{client: clients.download}
		ma.clock = realClock{}
		ma.journal = journalctlReader{}
	} else if sr, ok := ma.runner.(*systemdRunner); ok {
		sr.setLogger(logger)
	}
	ma.store = &fileStateStore{path: spec.dirs.state}
	if changed {
		logger.Info("app definition changed; it applies at the app's next launch (deploy, rollback, or relaunch after a crash, health failure or reboot), except its resource caps, which reach the running instance now")
	}
	// Wake the watchdog so a reload's spec (watchdog off, new
	// intervals) applies promptly even while the instance is healthy
	// or the loop is parked. The loop treats a poke with an unchanged
	// instance as a re-read, not a new instance — no grace re-arm.
	ma.pokeWatchdog()
}

// specEqual is a shallow inequality check good enough for the "config
// changed" notice; false negatives only cost a log line.
func specEqual(a, b *appSpec) bool {
	return fmt.Sprintf("%+v", a) == fmt.Sprintf("%+v", b)
}

// currentVerifiers snapshots the app's deploy-auth trust sources.
func (ma *managedApp) currentVerifiers() []deploytrust.Verifier {
	ma.specMu.RLock()
	defer ma.specMu.RUnlock()
	return ma.verifiers
}

// snapshot returns a consistent view of the spec and collaborators for
// one deploy/recovery run.
type collaborators struct {
	spec    *appSpec
	runner  runner
	prober  prober
	fetch   fetcher
	clock   clock
	store   stateStore
	journal journalReader
	logger  *zap.Logger
	// onPhase is the request's phase listener, carried with the
	// snapshot so setPhase needs no lock to find it.
	onPhase func(phase string)
}

func (ma *managedApp) snapshot() collaborators {
	ma.specMu.RLock()
	defer ma.specMu.RUnlock()
	return collaborators{
		spec:    ma.spec,
		runner:  ma.runner,
		prober:  ma.prober,
		fetch:   ma.fetch,
		clock:   ma.clock,
		store:   ma.store,
		journal: ma.journal,
		logger:  ma.logger,
	}
}

func (ma *managedApp) setPhase(c collaborators, phase string) {
	ma.mu.Lock()
	ma.phase = phase
	ma.phases = append(ma.phases, phaseTiming{Name: phase, StartedAt: c.clock.Now()})
	ma.mu.Unlock()
	c.logger.Info("deploy phase", zap.String("phase", phase))
	if c.onPhase != nil {
		c.onPhase(phase)
	}
}

// takePhases returns the running deploy's phases with their durations
// filled in (each ends where the next begins; the last at finished)
// and clears the record for the next deploy.
func (ma *managedApp) takePhases(finished time.Time) []phaseTiming {
	ma.mu.Lock()
	phases := ma.phases
	ma.phases = nil
	ma.mu.Unlock()
	for i := range phases {
		end := finished
		if i+1 < len(phases) {
			end = phases[i+1].StartedAt
		}
		phases[i].Seconds = end.Sub(phases[i].StartedAt).Seconds()
	}
	return phases
}

// preflightResult classifies a pre-flight's outcome: a refusal is the
// deployer's (422); anything else is motel's own failure to check.
func preflightResult(what string, err error) error {
	if err == nil {
		return nil
	}
	var pe *preflightError
	if errors.As(err, &pe) {
		return validationError{fmt.Sprintf("%s: %v", what, err)}
	}
	return fmt.Errorf("%s: %w", what, err)
}

// failureDetail is the app's side of a failed deploy, gathered after
// the fact: the exit the runner recorded (the pre_start's from its
// error; the app's as read before motel stopped it, and only when
// it died on its own), the last probe's answer, and the last lines the
// launch's units wrote. Nil when a launch left nothing to say.
func (ma *managedApp) failureDetail(c collaborators, spec *appSpec, err error, appExit string, appPID int, l launch, since time.Time) (*deployDetail, []string) {
	d := &deployDetail{}
	var ee *exitError
	var pe *probeError
	// The app's own bytes — the probe's body, the journal's lines — are
	// held to the known values here, before any cap cuts them: a cap
	// that fell inside a value would leave a fragment the response
	// filter's first layer cannot match, and no heuristic masks a
	// fragment under entropyMinLen (redact.go). The values are the
	// launch's own (l.secrets: what this instance's env_file rendered),
	// so no file is read and nothing is withheld on a file that went
	// unreadable since — the record keeps its detail (recordRedactor).
	// The filter runs whole over the finished body as always; this is
	// its first layer, early, and the keys it finds are returned for
	// the result's redacted_env, since the filter will not find the
	// values again.
	known := NewRedactor(l.secrets, nil)
	seen := map[string]bool{}
	withBody := spec.deployLogLines > 0
	// With the exit, the pid of the process that failed — the pre_start's
	// as the manager recorded it, or the app's: the journal finds that
	// process's lines by it when journald could not tell their unit
	// (journal.go).
	pid := appPID
	switch {
	case errors.As(err, &ee):
		d.Exit, pid = ee.exit, ee.pid
	case appExit != "":
		d.Exit = appExit
	}
	if errors.As(err, &pe) {
		d.Probe = &probeDetail{Status: pe.status, Location: pe.location}
		// The body is the app's output as much as the journal is:
		// deploy_log_lines 0 keeps both on the box.
		if withBody {
			d.Probe.Body = deploytrust.CutRunes(known.replaceKnown(pe.body, seen), probeBodyExcerpt)
		}
	}
	if withBody && c.journal != nil {
		of := tailOf{pid: pid, ident: syslogIdentifier(spec.name)}
		for _, oneshot := range []bool{true, false} {
			if u, err := unitName(l.startSpec(spec, spec.command), oneshot); err == nil {
				of.units = append(of.units, u)
			}
		}
		// The journal trails the process by a moment; ask again while
		// the count still grows, within a bound, rather than read once
		// and miss the last line — the one that says why.
		var lines []string
		ctx, cancel := context.WithTimeout(context.Background(), journalTailTimeout)
		defer cancel()
		for attempt := 0; attempt < 5 && ctx.Err() == nil; attempt++ {
			got, err := c.journal.tail(ctx, of, since, spec.deployLogLines)
			if err != nil {
				d.LogTailError = err.Error()
				break
			}
			grew := len(got) > len(lines)
			lines = got
			// Done past the cap, or once a read adds nothing to what an
			// earlier one found — not while there is nothing yet: the
			// journal may still be behind the process.
			if len(lines) > spec.deployLogLines || (attempt > 0 && !grew && len(lines) > 0) {
				break
			}
			c.clock.Sleep(200 * time.Millisecond)
		}
		if d.LogTailError == "" {
			for i, line := range lines {
				lines[i] = known.replaceKnown(line, seen)
			}
			d.LogTail, d.LogTailTruncated = capTail(lines, spec.deployLogLines, deployLogMaxBytes)
		}
	}
	if d.Exit == "" && d.Probe == nil && d.LogTail == nil && !d.LogTailTruncated && d.LogTailError == "" {
		return nil, nil
	}
	if len(seen) == 0 {
		return d, nil
	}
	return d, reportedKeys(seen)
}

// Deploy runs the full blue/green pipeline. Any failure before the
// promote step leaves the old version serving untouched — that is the
// rollback story. There is deliberately no post-promote auto-revert:
// rolling back is re-POSTing the previous version.
func (ma *managedApp) Deploy(ctx context.Context, req deployRequest) error {
	if !ma.deployMu.TryLock() {
		return errDeployInProgress
	}
	defer ma.endDeploy()
	c := ma.snapshot()
	c.onPhase = req.onPhase
	return ma.deployLocked(ctx, req, c)
}

// unlockDeploy lets go of deployMu for any holder but the watchdog, and
// wakes the watchdog: a restart that found the lock held waits for
// this, not for a timer (handleFailure). A poke where nothing changed
// reads as a re-read, so one more is harmless.
func (ma *managedApp) unlockDeploy() {
	ma.deployMu.Unlock()
	ma.pokeWatchdog()
}

// endDeploy is unlockDeploy at a deploy's end, whatever it came to,
// counted first: a restart waiting out its backoff over an instance a
// failed deploy left dead goes now rather than at the end of its step.
// A promoted deploy has already woken the watchdog with its instance.
func (ma *managedApp) endDeploy() {
	ma.deploysEnded.Add(1)
	ma.unlockDeploy()
}

// deployLocked runs the blue/green pipeline. The caller MUST already
// hold deployMu, and let go of it with endDeploy: URL/rollback go
// through Deploy, while the push handler
// acquires the lock BEFORE staging the upload, so a concurrent push gets
// an immediate 409 rather than streaming a whole tarball to disk only to
// lose the lock (which would also make aggregate staging unbounded).
//
// It runs against the single collaborators snapshot `c` the caller took,
// so a config reload mid-flight can never split one deploy across two app
// definitions (e.g. staging a push under the old size cap/root and
// extracting it under the new).
func (ma *managedApp) deployLocked(ctx context.Context, req deployRequest, c collaborators) (err error) {
	spec := c.spec
	started := c.clock.Now()
	logger := c.logger.With(zap.String("version", req.version))
	logger.Info("deploy started", zap.String("artifact_host", hostOf(req.url)))
	ma.mu.Lock()
	ma.phases = nil
	ma.mu.Unlock()

	pin := req.sha256 // a rollback's is read once it is accepted, below

	var stats archiveStats
	relaunch := false        // the running version, launched again from its release on disk: not the version's history
	accepted := false        // the request passed its checks: from here on, what happens is the version's history
	var detail *deployDetail // filled by the failure-detail defer below, which runs first
	var redacted []string    // the env keys the detail's early replacement found
	launched := false        // a pre_start or Start was attempted: there are units to ask about
	appExit := ""            // the app's exit, read before motel stops a failed instance
	appPID := 0              // and its main process's pid (runner.Exit)
	// marked: this deploy wrote the deploy marker (deploying.go).
	// Declared before the record's defer, so it runs after it: the
	// marker goes once the record is written, whatever the deploy came
	// to.
	marked := false
	defer func() {
		if !marked {
			return
		}
		if err := clearDeployMarker(spec.dirs); err != nil {
			logger.Warn("deploy marker not removed; the next start or deploy reads the record and clears it", zap.Error(err))
		}
	}()
	defer func() {
		finished := c.clock.Now()
		result := deployResult{
			Version:         req.version,
			Status:          "succeeded",
			By:              req.by,
			SHA256:          pin,
			StartedAt:       started,
			FinishedAt:      finished,
			ArtifactEntries: stats.entries,
			ArtifactBytes:   stats.bytes,
			Phases:          ma.takePhases(finished),
		}
		if relaunch && err == nil {
			result.Note = relaunchNote(spec, req.version)
		}
		if err != nil {
			result.Status = "failed"
			result.Error = err.Error()
			result.Detail, result.RedactedEnv = detail, redacted
			// The phase reached when it failed — none when it failed
			// before the first (a rollback whose env_file would not
			// read): "idle" is the app's state, not a deploy's phase.
			if len(result.Phases) > 0 {
				ma.mu.Lock()
				result.Phase = ma.phase
				ma.mu.Unlock()
			}
			logger.Error("deploy failed", zap.Error(err))
		} else {
			logger.Info("deploy succeeded")
		}
		ma.mu.Lock()
		ma.lastDeploy = &result
		ma.phase = "idle"
		ma.mu.Unlock()
		// What happened before the request was accepted — the version
		// already running or on disk, the app's directories not there
		// to make — is about the request or the box, not the version:
		// it must not replace the record of the deploy that put the
		// version there. Past acceptance, a refusal of the request's
		// own content (an artifact URL the allowlist refuses) is still
		// the request's; anything else that failed is the version's
		// history and is recorded. A relaunch of the running version
		// is not: the record stays the deploy that put the version
		// there, with its pin and its times, whether the relaunch
		// succeeded (the same release serves, from a new instance) or
		// failed (the old instance serves on, untouched); last_deploy
		// and the response say what the relaunch came to.
		var ve validationError
		if !relaunch && (err == nil || (accepted && !errors.As(err, &ve))) {
			ma.recordDeploy(c, result)
		}
	}()

	// Before anything is read or written through the app's dirs: one
	// that is not the directory it names would have this deploy write
	// into whatever it points at (writeRefusal).
	if err := spec.dirs.writeRefusal(); err != nil {
		return err
	}
	old := ma.currentInstance()
	if old != nil && old.version == req.version && c.runner.Alive(old.handle) {
		// The version that is running, asked for again: relaunched from
		// its release on disk — the normal pipeline minus fetch and
		// extract — when the request is for the same artifact
		// (relaunchRefusal), so a new env_file value or app definition
		// reaches it without a new version. Nothing is downloaded,
		// extracted or written under the release: a relaunch can only
		// run the bytes already there.
		recorded := recordedPin(spec.dirs, req.version)
		if err := relaunchRefusal(spec, req, recorded); err != nil {
			return err
		}
		relaunch, pin = true, recorded // the pin the running release was matched by: its record's, equal to the request's when it carried one
		logger.Info("version is already running; relaunching it from its release on disk (same artifact)")
	}
	if err := spec.dirs.ensure(); err != nil {
		return err
	}
	if err := ma.settleBeforeDeploy(c, old); err != nil {
		return err
	}
	// Versions are immutable: a deploy (URL or push) never overwrites an
	// existing on-disk release, so a version you can roll back to can't be
	// silently replaced with different content. Rollback is exempt — it
	// relaunches an existing release by design — and so is a relaunch,
	// which fetches nothing. Deploys are serialized per app (deployMu),
	// so this check-then-create has no race.
	if !req.rollback && !relaunch {
		switch _, statErr := os.Stat(spec.dirs.release(req.version)); {
		case statErr == nil:
			return validationError{fmt.Sprintf("version %s already exists — versions are immutable; deploy a new version, or roll back to relaunch this one", req.version)}
		case !os.IsNotExist(statErr):
			// A real I/O/permission error must not be read as "absent"
			// and fall through to fetch, whose RemoveAll would then
			// overwrite the existing release.
			return fmt.Errorf("checking release %s: %w", req.version, statErr)
		}
	}

	accepted = true
	// A rollback replaces the version's record, so it carries that
	// record's pin forward (a relaunch's was read above); anything else
	// names its own (or none).
	if req.rollback && !relaunch {
		pin = recordedPin(spec.dirs, req.version)
	}

	// extracted: this deploy put the release there, so a failure takes
	// it away again; a rollback and a relaunch launch a release that
	// was already on disk and must never delete it or re-run its
	// pre_start. The fetcher's rollback path is the one way an on-disk
	// release is taken as it is — a stat, nothing written — so a
	// relaunch is fetched as a rollback of the running version.
	extracted := !req.rollback && !relaunch
	if extracted {
		// Before anything is extracted: a motel that stops from here
		// on leaves the marker naming this release (deploying.go).
		if err := saveDeployMarker(spec.dirs, deployMarker{Version: req.version, StartedAt: started}); err != nil {
			return fmt.Errorf("writing the deploy marker: %w", err)
		}
		marked = true
	}
	fetchReq := req
	fetchReq.rollback = !extracted
	releaseDir, stats, err := c.fetch.fetch(ctx, spec, fetchReq, func(phase string) { ma.setPhase(c, phase) })
	if err != nil {
		return err
	}
	if extracted { // a rollback or relaunch extracted nothing; there is nothing to measure
		spec.warnNearCaps(logger, stats)
	}
	// If this freshly-extracted release never promotes (pre_start, start
	// or health-gate failure), remove it: a failed attempt must not
	// permanently reserve its immutable version or leave disk litter, so
	// the same version stays retriable. Rollback and relaunch are
	// excluded — each launches a pre-existing release it must never
	// delete (a relaunch's is the one serving).
	// newHandle is predeclared so the cleanup defer can confirm the failed
	// instance is really gone before deleting its release.
	var newHandle handle
	promoted := false
	stopUnconfirmed := false // Stop of the failed instance returned an error
	if extracted {
		defer func() {
			if err == nil || promoted {
				return
			}
			// Never delete a release out from under an instance that may
			// still be running (a Stop that errored, a start or pre_start
			// the runner could not reconcile, a handle still alive) —
			// that would pull files from beneath a live process. The
			// next deploy's sweep settles it against the runner.
			if unitUnconfirmed(err) || (newHandle != nil && (stopUnconfirmed || c.runner.Alive(newHandle))) {
				err = errors.Join(err, fmt.Errorf("release %s left on disk: the failed instance may still be running", req.version))
				return
			}
			// Surface a cleanup failure: otherwise the release lingers and
			// the next attempt at this version 422s (immutable) with no
			// explanation of why.
			if rmErr := os.RemoveAll(releaseDir); rmErr != nil {
				err = errors.Join(err, fmt.Errorf("cleanup of failed release %s: %w", req.version, rmErr))
			}
		}()
	}

	l, err := spec.prepareLaunch(logger, req.version)
	if err != nil {
		return err
	}
	ma.rememberSecrets(spec.envFile, l.secrets)
	// Declared after the cleanup defers above, so it runs before them:
	// the detail is read off the pipeline's own error, before cleanup
	// joins its own lines onto it.
	defer func() {
		if err != nil && launched {
			detail, redacted = ma.failureDetail(c, spec, err, appExit, appPID, l, started)
		}
	}()
	// A launch that never becomes the instance leaves nothing behind:
	// its run dir, and any pin the health gate made, go with it. Once
	// promoted, l.sock is the instance's own and lives on. And like
	// the release above, it is preserved while a unit may still be
	// live — an ambiguous start or pre_start, a stop the runner could
	// not confirm — since removing the dir from under a starting
	// process would leave it running and unreachable; the next
	// confirmed sweep stops that unit and prunes it.
	defer func() {
		if promoted || unitUnconfirmed(err) || (newHandle != nil && (stopUnconfirmed || c.runner.Alive(newHandle))) {
			return
		}
		l.sock.retire()
	}()

	// pre_start is deploy-time preparation; its outputs (migrations,
	// generated config, warmed caches) persist, so a rollback — which
	// relaunches an already-prepared on-disk release, like crash
	// recovery — must NOT re-run it, and nor must a relaunch of the
	// running version (the same release, already prepared and serving).
	// Re-running an old forward migration
	// would be wrong, and a flaky migration check must never block an
	// emergency rollback. The command's pre-flight above does run on a
	// rollback: it is a path resolution and a header read, nothing
	// that can flake, and what it refuses the launch would refuse too.
	// Pre-flight, before any unit exists and before a migration runs:
	// the command and the pre_start resolve to files inside the
	// sandbox view, built for this machine. A tarball from the wrong
	// runner is refused here as the deployer's error (422) with the
	// fix named, rather than surfacing as a failed exec a phase later.
	ma.setPhase(c, "preparing")
	if err := preflightResult("pre-flight", c.runner.Preflight(l.startSpec(spec, spec.command))); err != nil {
		return err
	}
	if len(spec.preStart) > 0 && extracted {
		pre := l.startSpec(spec, spec.preStart)
		// Bounded on the unit too: a motel that stops during it takes
		// preCtx with it, and the migration runs on (DESIGN-liveswap.md).
		pre.startTimeout = spec.deadline
		if err := preflightResult("pre-flight of pre_start", c.runner.Preflight(pre)); err != nil {
			return err
		}
		// A previous deploy's pre_start whose outcome could not be
		// observed may still be running; settle the ledger before
		// starting another migration beside it.
		if err := ma.sweepOrWhy(c, old, l.nonce); err != nil {
			return fmt.Errorf("%w; not running pre_start", err)
		}
		// Under deadline, not under the request: a client that hangs
		// up (a cancelled CI job) must not stop a migration midway,
		// since a half-applied one harms the app's data while the old
		// instance serves on. It runs to its end; the health gate
		// below then reads the request and fails the deploy, as it
		// does for any deploy whose client left.
		preCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), spec.deadline)
		// Under the same sandbox as the app it precedes: a migration
		// that writes where the app cannot read fails here, not at 3am.
		launched = true
		err := c.runner.RunOnce(preCtx, pre)
		cancel()
		if err != nil {
			return fmt.Errorf("pre_start failed: %w", err)
		}
	}

	ma.setPhase(c, "starting")
	// No Start without a confirmed sweep (keeping the version that is
	// serving): the new instance must not come up beside a unit the
	// manager still holds for this app.
	if err := ma.sweepOrWhy(c, old, l.nonce); err != nil {
		return fmt.Errorf("%w; not starting", err)
	}
	launched = true
	newHandle, err = c.runner.Start(l.startSpec(spec, spec.command))
	if err != nil {
		return fmt.Errorf("start failed: %w", err)
	}

	ma.setPhase(c, "soaking")
	alive := func() bool { return c.runner.Alive(newHandle) }
	if err := c.prober.waitHealthy(ctx, l.sock, alive, healthConfig{
		path:     spec.healthPath,
		interval: spec.healthInterval,
		timeout:  spec.healthTimeout,
		soak:     spec.soak,
		deadline: spec.deadline,
	}); err != nil {
		deployErr := fmt.Errorf("health gate: %w", err)
		// The exit is the app's own only when it died on its own; read
		// it now, before the Stop below would make every failure look
		// like a SIGTERM. Its pid is read either way: what the journal
		// finds its lines by.
		exit, pid := c.runner.Exit(newHandle)
		appPID = pid
		if errors.Is(err, errProcessExited) {
			appExit = exit
		}
		// If Stop can't confirm the instance is gone, surface that — and
		// the cleanup defer then leaves the release in place rather than
		// delete it beneath a possibly live process.
		if stopErr := c.runner.Stop(newHandle, spec.grace); stopErr != nil {
			stopUnconfirmed = true
			deployErr = errors.Join(deployErr, fmt.Errorf("failed instance could not be stopped: %w", stopErr))
		}
		return deployErr
	}

	// The point of no return. From here on the request context is
	// ignored: a CI client hanging up must not abort stop-old or GC.
	ma.setPhase(c, "promoting")
	newInst := l.instance(newHandle)
	if err := ma.publishInstance(c, newInst); err != nil { // ← the cutover
		logger.Error("state persistence failed; deploys still work but a Caddy restart will not know about this version", zap.Error(err))
	}
	promoted = true // past the cutover: never clean up the now-live release

	// A successful deploy is the fast path out of any watchdog wait:
	// it clears the failure count, restart budget and backoff, then
	// wakes the loop to adopt the new instance.
	ma.wd.reset()
	ma.pokeWatchdog()

	if old != nil && c.runner.Alive(old.handle) {
		ma.setPhase(c, "draining")
		c.clock.Sleep(spec.drain)
		ma.setPhase(c, "stopping_old")
		if err := c.runner.Stop(old.handle, spec.grace); err != nil {
			logger.Warn("stopping old version failed; the sweep below retries it", zap.String("version", old.version), zap.Error(err))
		}
	}
	if old != nil {
		old.sock.retire()
	}

	// Nothing is deleted while anything but the new instance may be
	// running out of a release dir: the sweep settles that against
	// the runner's own ledger (an unconfirmed stop above, a unit an
	// earlier motel left behind), and any doubt skips GC — the next
	// successful deploy catches up.
	if !ma.sweep(c, newInst, "") {
		logger.Warn("skipping release GC for this deploy")
		return nil
	}
	// The version old ran is the way back from this one: GC keeps it
	// next to the new one, by what served rather than by its record,
	// which a lost write could have left at an older deploy.
	previous := ""
	if old != nil {
		previous = old.version
	}
	gcReleases(spec.dirs, spec.keep, req.version, previous, logger)
	return nil
}

// relaunchRefusal decides whether a request naming the version that is
// running relaunches it: nil to proceed, or the refusal (a
// validationError, 422). A rollback is the same release by definition
// and needs no digest. A URL or push deploy relaunches only when it is
// for the same artifact: its digest (the URL deploy's pin; the push's
// upload, hashed by stageUpload) equals recorded, the one the
// version's record holds, so nothing is fetched or hashed here — a
// request with no digest, or a record without one (a push deployed
// before pushes were hashed, an unpinned pull), is refused rather than
// fetched and compared, and so is a different artifact: versions are
// immutable. Every same-version message says how a new env_file value
// reaches the app, since that is what the request was reaching for.
// A URL deploy whose digests match then gets the checks a fetch would
// give its url (pinArtifactURL), so a URL the allowlist refuses is
// refused here too rather than passing because nothing was downloaded
// from it; a request refused on its digest fetches nothing either way,
// and the digest's message is the one that names its remedy.
//
// The comparison is with the record, not the release's bytes (the
// record's known limit, recordedPin): whatever it decides, a relaunch
// runs the release on disk and introduces no bytes of its own.
func relaunchRefusal(spec *appSpec, req deployRequest, recorded string) error {
	if req.rollback {
		return nil
	}
	remedy := fmt.Sprintf("a rollback to %s relaunches it (that, or a new version, is also how a new secret reaches the app)", req.version)
	switch {
	case req.sha256 == "":
		return validationError{fmt.Sprintf("version %s is already running and the request carries no sha256 to match its artifact by; %s", req.version, remedy)}
	case recorded == "":
		return validationError{fmt.Sprintf("version %s is already running and its artifact's digest is not on record, so the request cannot be matched to it; %s", req.version, remedy)}
	case req.sha256 != recorded:
		return validationError{fmt.Sprintf("version %s is already running from a different artifact — versions are immutable; deploy a new version (a rollback to %s relaunches what is running, and either is how a new secret reaches the app)", req.version, req.version)}
	}
	if req.localArchive == "" {
		if _, err := pinArtifactURL(req.url, spec.allowInsecure, spec.allowlist); err != nil {
			return err
		}
	}
	return nil
}

// relaunchNote is the response's word for a relaunch that succeeded:
// what ran, and — when the app has one — that the env_file was read
// again, which is what the request was most likely for.
func relaunchNote(spec *appSpec, version string) string {
	note := fmt.Sprintf("relaunched %s (same artifact)", version)
	if spec.envFile != "" {
		note += "; it reads its env file now"
	}
	return note
}

// ensureRunning is crash/restart recovery, called from App.Start. If
// the instance already exists in memory (config reload — the pool kept
// it), this is a no-op. Otherwise it tries to reattach via the runner
// and falls back to relaunching the version recorded in state.json.
// The health gate is a deploy gate, not a boot gate: on a cold start
// there is nothing better to serve, so the port is published as soon
// as the process is up.
func (ma *managedApp) ensureRunning() error {
	if !ma.deployMu.TryLock() {
		// A deploy owns the lifecycle right now; it may still fail
		// before publishing anything, so recovery must look again.
		return &transientRecoveryError{errors.New("a deploy is in progress")}
	}
	defer ma.unlockDeploy()
	// Recovery pins and adopts, or relaunches into, this app's socket
	// dirs; the start-time sweep of unconfigured apps prunes such dirs.
	// Holding the app's ownerLock across recovery is what keeps the two
	// from interleaving on an app a reload has just added back.
	owner := ownerLock(ma.name)
	owner.Lock()
	defer owner.Unlock()

	c := ma.snapshot()
	spec := c.spec
	if inst := ma.currentInstance(); inst != nil && c.runner.Alive(inst.handle) {
		// Reload, or a retry after an earlier sweep could not confirm:
		// the instance is fine; the ledger must still be settled.
		if err := ma.sweepOrWhy(c, inst, ""); err != nil {
			return &transientRecoveryError{fmt.Errorf("instance %s running; %w", inst.version, err)}
		}
		ma.settleAfterRecovery(c, inst.version)
		return nil
	}
	st, ok, err := c.store.load()
	if err != nil {
		return &permanentRecoveryError{err} // corrupt state: never silently reset
	}
	if !ok || st.CurrentVersion == "" {
		// Nothing recorded — but a deploy whose state write failed may
		// have left a unit behind; the manager's ledger decides, not
		// the absence of a file.
		if err := ma.sweepOrWhy(c, nil, ""); err != nil {
			return &transientRecoveryError{err}
		}
		ma.settleAfterRecovery(c, "")
		ma.wd.setLastFailure("") // nothing to recover is nothing failing: an earlier reason is over
		return nil
	}
	// state.json is ours, but it is a file: the version it names
	// becomes a path component, an argument, an environment value,
	// so one that is not a version is used for nothing — not even to
	// look for its release. Corrupt state, like the rest: never
	// silently reset (redact.go, rule 4).
	if !validVersion(st.CurrentVersion) {
		return &permanentRecoveryError{fmt.Errorf("state names version %q, which is not a version", st.CurrentVersion)}
	}
	releaseDir := spec.dirs.release(st.CurrentVersion)
	if _, err := os.Stat(releaseDir); err != nil {
		return &permanentRecoveryError{fmt.Errorf("state names version %s but its release dir is missing: %w", st.CurrentVersion, err)}
	}

	// state.json is ours, but it is a file: a recorded unit name that
	// is not one of this app's units is never handed to the manager
	// (it could be a sibling's). Treat it as "nothing recorded".
	if u := st.Handle.Unit; u != "" && !unitBelongsTo(u, ma.name) {
		c.logger.Error("state.json names a unit that is not this app's; ignoring it", zap.String("unit", u))
		st.Handle.Unit = ""
	}
	// The recorded nonce names the socket the proxy will dial, so the
	// record is adopted only when it is the unit's own nonce and the
	// socket it names can be pinned — present, and a socket rather
	// than something planted under the name. Anything else — a unit
	// that is not ours, a nonce that disagrees with it, a socket that
	// is gone or forged — is "nothing recorded": the relaunch's sweep
	// stops whatever the manager still holds, rather than routing to
	// a socket that answers nobody, or to somebody else's.
	// The nonce becomes a path component under run/ and proxy/, so a
	// recorded value that is not a nonce is never used for anything —
	// not to dial, not to remove — before it is even compared.
	if st.Handle.Unit != "" && !nonceRe.MatchString(st.Nonce) {
		c.logger.Error("state.json record ignored: its nonce is not one",
			zap.String("unit", st.Handle.Unit), zap.String("nonce", st.Nonce))
		st.Handle.Unit = ""
	}
	if err := spec.dirs.ensure(); err != nil {
		return &transientRecoveryError{err}
	}
	var (
		socket string
		sock   *socketRef
	)
	if st.Handle.Unit != "" {
		socket, sock = spec.dirs.socket(st.Nonce), spec.dirs.socketRef(st.Nonce)
		reason := ""
		if un, ok := unitNonce(st.Handle.Unit); !ok || un != st.Nonce {
			reason = "its nonce is not the recorded unit's"
		} else if _, err := sock.dial(); err != nil {
			reason = "its socket cannot be dialled: " + err.Error()
		}
		if reason != "" {
			c.logger.Error("state.json record ignored: "+reason,
				zap.String("unit", st.Handle.Unit), zap.String("nonce", st.Nonce), zap.String("socket", socket))
			st.Handle.Unit = ""
		}
	}
	if st.Handle.Unit == "" {
		st.Handle = handleState{} // nothing to reattach to
	}
	// An unreadable unit state is not "not running": launching beside
	// a unit that may still be up would duplicate the app. Report it
	// as transient; recover() retries with backoff until the manager
	// answers, rather than guessing.
	h, attached, err := c.runner.Reattach(st.Handle)
	if err != nil {
		// The pin stays for the retry: the unit may well be serving,
		// and an app that has since replaced its own name (allowed)
		// could not be pinned again from it.
		return &transientRecoveryError{fmt.Errorf("cannot tell whether %s is still running; not relaunching: %w", st.CurrentVersion, err)}
	}
	if !attached && sock != nil {
		sock.retire() // pinned above but definitively not adopted; the relaunch gets its own
	}
	if attached {
		inst := &instance{version: st.CurrentVersion, nonce: st.Nonce, socket: socket, sock: sock, handle: h}
		ma.mu.Lock()
		ma.current = inst
		ma.mu.Unlock()
		ma.activeSocket.Store(inst.sock)
		ma.wd.setLastFailure("") // a recovery that gave up, or a refusal, is over
		c.logger.Info("reattached to running instance", zap.String("version", st.CurrentVersion))
		ma.pokeWatchdog()
		ma.syncCaps() // the config may have changed its caps while motel was down
		if err := ma.sweepOrWhy(c, inst, ""); err != nil {
			// Serving, but the ledger is unsettled: recover() retries
			// and the retry path above sweeps again.
			return &transientRecoveryError{fmt.Errorf("reattached %s; %w", st.CurrentVersion, err)}
		}
		ma.settleAfterRecovery(c, inst.version)
		return nil
	}

	inst, err := ma.launchVersion(c, st.CurrentVersion)
	if err != nil {
		// A preflight refusal is the release as shipped or the command
		// as configured — a binary that is not there, not executable,
		// outside the sandbox view, or built for another machine (the
		// launch runs the deploy's Preflight). None of that appears by
		// retrying; everything else here (sweep, manager, unit
		// reconcile) can.
		var pe *preflightError
		if errors.As(err, &pe) {
			return &permanentRecoveryError{fmt.Errorf("relaunching %s: %w", st.CurrentVersion, err)}
		}
		return &transientRecoveryError{fmt.Errorf("relaunching %s: %w", st.CurrentVersion, err)}
	}
	if err := ma.publishInstance(c, inst); err != nil {
		c.logger.Warn("persisting recovered state", zap.Error(err))
	}
	ma.wd.setLastFailure("") // a recovery that gave up, or a refusal, is over
	c.logger.Info("relaunched current version after restart",
		zap.String("version", st.CurrentVersion), zap.String("socket", inst.socket))
	ma.pokeWatchdog()
	ma.settleAfterRecovery(c, inst.version) // launchVersion's sweep confirmed: inst is the app's one unit
	return nil
}

// Recovery errors come in two kinds. transient: the manager could not
// be asked, a deploy held the lock, a unit could not be reconciled —
// retrying can fix it. permanent: the answer itself is bad (corrupt
// state, missing release dir, command not found) and no retry will
// change it. Anything not marked permanent is retried: an app down
// for an unclassified reason costs a log line a minute, an app left
// down for a transient one costs an outage.
type transientRecoveryError struct{ err error }

func (e *transientRecoveryError) Error() string { return e.err.Error() }
func (e *transientRecoveryError) Unwrap() error { return e.err }

type permanentRecoveryError struct{ err error }

func (e *permanentRecoveryError) Error() string { return e.err.Error() }
func (e *permanentRecoveryError) Unwrap() error { return e.err }

// transientRecovery reports whether err is worth retrying.
func transientRecovery(err error) bool {
	var p *permanentRecoveryError
	return err != nil && !errors.As(err, &p)
}

// errSweepUnconfirmed is the launch refusal when a pre-start sweep
// cannot confirm the app has no other unit running.
var errSweepUnconfirmed = errors.New("cannot confirm no other instance is running")

const (
	recoveryBackoffFloor = 2 * time.Second
	recoveryBackoffCap   = time.Minute
)

// recover runs ensureRunning until it succeeds or fails for a reason a
// retry cannot fix, backing off on transient manager trouble. A boot
// where the user manager is briefly unresponsive must not leave a
// healthy app unrouted until an operator reloads. ctx belongs to the
// config that started it and ends at that config's Cleanup.
func (ma *managedApp) recover(ctx context.Context, logger *zap.Logger) {
	delay := recoveryBackoffFloor
	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			return // destructed before this attempt
		}
		err := ma.ensureRunning()
		if err == nil {
			return
		}
		if !transientRecovery(err) {
			logger.Error("recovery failed", zap.Error(err))
			// Nothing runs, and nothing will retry: status says why
			// (watchdog.last_failure) until a launch replaces it.
			ma.wd.setLastFailure(err.Error())
			return
		}
		// Loud once, then a heartbeat: a manager that is down for an
		// hour should not fill the journal with sixty errors.
		level := zap.WarnLevel
		if attempt == 1 {
			level = zap.ErrorLevel
		}
		logger.Log(level, "recovery deferred; retrying", zap.Int("attempt", attempt), zap.Duration("in", delay), zap.Error(err))
		select {
		case <-ctx.Done():
			return
		case <-ma.snapshot().clock.After(delay):
		}
		if delay *= 2; delay > recoveryBackoffCap {
			delay = recoveryBackoffCap
		}
	}
}

// sweep stops every unit of this app other than keep — the runner's
// ledger, not ours, decides what is running. Recovery calls it so a
// unit motel lost track of (a state.json write that failed, an
// earlier stop that could not be confirmed) does not outlive the next
// start; deploys call it before GC. Returns false if something may
// still be running, in which case callers must not delete anything.
// A confirmed sweep is also the manager's word that no instance but
// keep is running, so every other instance's socket dirs are
// leftovers — except the one being launched right after, named by
// launching, whose run dir is already prepared.
func (ma *managedApp) sweep(c collaborators, keep *instance, launching string) bool {
	return ma.sweepOrWhy(c, keep, launching) == nil
}

// sweepOrWhy is sweep for a caller that says why it stops:
// errSweepUnconfirmed, carrying the migration it waits for when that
// is the reason (a deploy refused beside one names it).
func (ma *managedApp) sweepOrWhy(c collaborators, keep *instance, launching string) error {
	var keepHandle handle
	keepNonces := []string{launching}
	if keep != nil {
		keepHandle = keep.handle
		keepNonces = append(keepNonces, keep.nonce)
	}
	if err := c.runner.Sweep(ma.name, keepHandle); err != nil {
		c.logger.Error("sweeping stray instances", zap.Error(err))
		var mr *migrationRunningError
		if errors.As(err, &mr) {
			return fmt.Errorf("%w: %w", errSweepUnconfirmed, err)
		}
		return errSweepUnconfirmed
	}
	pruneSockets(c.spec.dirs, keepNonces, c.logger)
	return nil
}

// launchVersion starts an already-on-disk version on a fresh socket
// and returns the new instance without publishing it. The version comes
// from the caller's record (state.json, or the live instance the
// watchdog is replacing); the command and the environment are rendered
// from the CURRENT spec, so an edited app definition does reach a
// relaunch — the same exposure a reload has always had here (a changed
// command relaunches on the next crash). The sandbox is the same one a
// deploy gets: the base view plus this app's own dirs, which no config
// change moves. Recording the whole launch disposition is the fuller
// answer — see #35.
func (ma *managedApp) launchVersion(c collaborators, version string) (*instance, error) {
	spec := c.spec
	if _, err := os.Stat(spec.dirs.release(version)); err != nil {
		return nil, fmt.Errorf("release dir for version %s is missing: %w", version, err)
	}
	l, err := spec.prepareLaunch(c.logger, version)
	if err != nil {
		return nil, err
	}
	ma.rememberSecrets(spec.envFile, l.secrets)
	// No Start without a confirmed sweep: whatever the manager still
	// runs for this app (a unit an earlier motel lost track of, a
	// stop that could not be confirmed) is settled first, or nothing
	// is launched beside it.
	if err := ma.sweepOrWhy(c, nil, l.nonce); err != nil {
		l.sock.retire()
		return nil, fmt.Errorf("%w; not launching", err)
	}
	// The deploy's pre-flight, on the spec about to start: a release
	// that cannot run on this box — built for another machine, its
	// command gone from the view — is refused here as a preflightError,
	// which every caller treats as permanent, rather than started to die
	// at exec and be restarted for ever. After the sweep, so a refusal
	// still leaves the ledger settled.
	ss := l.startSpec(spec, spec.command)
	if err := c.runner.Preflight(ss); err != nil {
		l.sock.retire()
		return nil, err
	}
	h, err := c.runner.Start(ss)
	if err != nil {
		if !unitUnconfirmed(err) { // an ambiguous start may be live: the next confirmed sweep prunes
			l.sock.retire()
		}
		return nil, err
	}
	return l.instance(h), nil
}

// launch is one instance's disposition — its nonce, socket, release
// dir and environment — prepared once and used for the pre_start unit
// and the app unit alike, by deploys and relaunches alike, so the two
// paths cannot drift.
type launch struct {
	version, nonce, socket, releaseDir string
	sock                               *socketRef
	env                                []string
	// secrets is the env_file subset of env: what the response filter
	// must never let out (redact.go).
	secrets []string
	// envFileMode is the mode of the env_file inode secrets came from,
	// read off the same descriptor; 0 when the spec names none.
	envFileMode os.FileMode
}

// prepareLaunch draws the instance's nonce and renders its environment
// from the current spec, creating the dirs the unit binds first: the
// runner resolves every bind source, and run/ holds nothing between
// instances. logger is the caller's snapshot (collaborators.logger):
// the one place both launch paths pass through is where the env_file
// mode warning lives, so neither path can lose it.
func (spec *appSpec) prepareLaunch(logger *zap.Logger, version string) (launch, error) {
	if err := spec.dirs.ensure(); err != nil {
		return launch{}, err
	}
	nonce, err := newNonce()
	if err != nil {
		return launch{}, err
	}
	// Exclusive: the dir is this launch's reservation of the nonce, and
	// an existing one is some other instance's, never adopted as ours.
	if err := os.Mkdir(spec.dirs.runDir(nonce), 0o750); err != nil {
		return launch{}, err
	}
	l := launch{version: version, nonce: nonce, socket: spec.dirs.socket(nonce), releaseDir: spec.dirs.release(version)}
	l.sock = spec.dirs.socketRef(nonce)
	if l.env, l.secrets, l.envFileMode, err = buildEnvFull(spec, version, l.socket, l.releaseDir); err != nil {
		l.sock.retire()
		return launch{}, err
	}
	if spec.envFile != "" {
		warnEnvFileModeBits(logger, spec.name, spec.envFile, l.envFileMode)
	}
	return l, nil
}

func (l launch) startSpec(spec *appSpec, command []string) startSpec {
	return startSpec{
		app:     spec.name,
		version: l.version,
		nonce:   l.nonce,
		command: expandArgs(command, spec, l.version, l.socket, l.releaseDir),
		dir:     l.releaseDir,
		env:     l.env,
		grace:   spec.grace,
		sandbox: spec.sandboxSpecFor(l.releaseDir, l.nonce),
		caps:    spec.caps,
	}
}

// syncCaps gives the running instance the caps the spec has now, when
// its unit carries others: a reload's changed memory_max, tasks_max or
// cpu_quota applies to the running app rather than waiting for its
// next launch, and a cap a reload removed is lifted. A unit reattached
// after a motel restart carries whatever it was started with, which
// this brings up to date too. A failure is a warning, not an error:
// the caps apply at the next launch regardless (startSpec).
//
// Serialized per app (capsMu): every install of a spec is followed by
// a sync, and a sync that waits behind another snapshots the spec
// after it, so whichever order a publication's and a reload's syncs
// start in, the last to apply carries the latest spec.
func (ma *managedApp) syncCaps() {
	ma.capsMu.Lock()
	defer ma.capsMu.Unlock()
	c := ma.snapshot()
	inst := ma.currentInstance()
	if c.spec == nil || c.runner == nil || inst == nil || !c.runner.Alive(inst.handle) {
		return
	}
	want := c.spec.caps
	if hs := inst.handle.state(); !hs.CapsUnknown && hs.Caps == want {
		return
	}
	fields := []zap.Field{zap.String("version", inst.version),
		zap.Uint64("memory_max", want.memoryMax), zap.Int("tasks_max", want.tasksMax), zap.Int("cpu_quota", want.cpuQuota)}
	if err := c.runner.SetCaps(inst.handle, want); err != nil {
		c.logger.Warn("resource caps not applied to the running instance; they apply at its next launch", append(fields, zap.Error(err))...)
		return
	}
	c.logger.Info("resource caps applied to the running instance", fields...)
}

func (l launch) instance(h handle) *instance {
	return &instance{version: l.version, nonce: l.nonce, socket: l.socket, sock: l.sock, handle: h}
}

// publishInstance installs inst as current, cuts traffic over to it
// and persists it for the next restart. It is the ONLY place current
// and activeSocket are installed — the watchdog's stale-instance guards
// depend on the swap-before-route ordering here, so promote, recovery
// and watchdog restarts must all go through it. The replaced
// instance's pinned socket is the caller's to retire, once that
// instance is stopped. A persist failure is returned, not logged: each
// caller has its own severity and message.
func (ma *managedApp) publishInstance(c collaborators, inst *instance) error {
	ma.mu.Lock()
	ma.current = inst
	ma.mu.Unlock()
	ma.activeSocket.Store(inst.sock)
	// Persisted before the caps are reconciled: the record is what a
	// restart recovers, and a sync that waits on the manager must not
	// hold it back from naming the instance that is already serving.
	err := ma.persistState(c, inst)
	// The instance was started from the spec its launch snapshotted;
	// a reload in between (a deploy's fetch and soak are long) changed
	// the caps on the instance it was replacing, not on this one.
	// Reconciled here, against the spec as it is now, so the serving
	// instance carries the accepted configuration's caps whichever
	// path published it.
	ma.syncCaps()
	return err
}

// unrouteIf clears activeSocket only while inst is still current, under
// the same lock that guards the current swap. Check-then-store without
// the lock races promote: a deploy could install and route a healthy
// instance between the check and the store, and the store would then
// unroute it permanently. The instance is dead, so its pinned socket
// is retired with it.
func (ma *managedApp) unrouteIf(inst *instance) {
	ma.mu.Lock()
	defer ma.mu.Unlock()
	if ma.current == inst {
		ma.activeSocket.Store(nil)
		inst.sock.retire()
	}
}

// dropIf is unrouteIf that also forgets inst as the current instance:
// a relaunch of it was refused for good (the watchdog), so nothing
// runs, and nothing is supervised, until a deploy or a reload's
// recovery launches the app again. state.json keeps the version, so a
// restart's recovery meets the same refusal.
func (ma *managedApp) dropIf(inst *instance) {
	ma.mu.Lock()
	defer ma.mu.Unlock()
	if ma.current == inst {
		ma.current = nil
		ma.activeSocket.Store(nil)
		inst.sock.retire()
	}
}

func (ma *managedApp) persistState(c collaborators, inst *instance) error {
	if err := c.store.save(appState{
		CurrentVersion: inst.version,
		Nonce:          inst.nonce,
		Handle:         inst.handle.state(),
		UpdatedAt:      c.clock.Now(),
	}); err != nil {
		return err
	}
	// The `current` symlink is for humans poking around the server;
	// state.json remains the source of truth.
	tmp := c.spec.dirs.current + ".tmp"
	_ = os.Remove(tmp)
	if err := os.Symlink(c.spec.dirs.release(inst.version), tmp); err == nil {
		_ = os.Rename(tmp, c.spec.dirs.current)
	}
	return nil
}

func (ma *managedApp) currentInstance() *instance {
	ma.mu.Lock()
	defer ma.mu.Unlock()
	return ma.current
}

// statusSnapshot backs the webhook's GET endpoint.
type statusSnapshot struct {
	App            string `json:"app"`
	Phase          string `json:"phase"`
	CurrentVersion string `json:"current_version,omitempty"`
	// Socket is the unix socket the instance listens on, under its
	// run/ dir. It is what the app bound, not what motel dials:
	// that is a hard link to the same inode under proxy/, outside
	// the app's reach. Reachable by the motel user (run/ is 0750).
	Socket string `json:"socket,omitempty"`
	PID    int    `json:"pid,omitempty"`
	// Command is the argv the manager reports for the running unit —
	// rendered, so an absolute path with placeholders substituted, and
	// not what the config says now (a reload does not restart an app).
	// Env is not reported: see DESIGN-threat-model.md.
	Command []string `json:"command,omitempty"`
	// Unit is the systemd unit running the instance — what to pass to
	// journalctl for the app's own output.
	Unit    string `json:"unit,omitempty"`
	Running bool   `json:"running"`
	// Caps is what the running unit is bounded to, as the manager last
	// applied them: set at the unit's start, changed live by a reload
	// (syncCaps), or read back at reattach. Absent when none is set,
	// and without any cap the host does not enforce.
	Caps *capsSnapshot `json:"caps,omitempty"`
	// CapsUnenforced names the configured caps the host does not
	// enforce: the user manager does not delegate their controller to
	// the unit's cgroup, so the manager holds the value and the kernel
	// bounds nothing (not the case on Debian 13). Absent when every
	// cap is enforced.
	CapsUnenforced []string `json:"caps_unenforced,omitempty"`
	// Deploys is the outcome of the latest deploy of each version a
	// record is kept for (deploys.go), newest first; the whole record
	// is GET /<app>?deploy=<version>. Serialized before last_deploy:
	// the examples' deploy.sh reads the failing phase as the last
	// "phase" in a failure body, which must stay last_deploy's. That
	// body carries the snapshot under "status", whose keys' order the
	// filter keeps (withField sorts only the top level).
	Deploys    []deploySummary   `json:"deploys,omitempty"`
	LastDeploy *deployResult     `json:"last_deploy,omitempty"`
	Watchdog   *watchdogSnapshot `json:"watchdog,omitempty"`
	// AvailableVersions lists the on-disk releases, the running version
	// first, then most recently deployed first (state.go, "Release
	// order") — the versions
	// `?rollback=<version>` can relaunch. Always serialized (a healthy
	// app with no releases reports []), so an empty set is
	// distinguishable from a server that doesn't report the field.
	AvailableVersions []string `json:"available_versions"`
	// OrderedByMtime names the entries of AvailableVersions placed by
	// their release dir's mtime, which the app can set: those with no
	// deploy record (deployed before records, a record write that
	// failed, or a first deploy still in flight) and those whose record
	// is there but cannot be read. Each comes after every recorded
	// release unless it is the running version, which the list puts
	// first; absent when there are none.
	OrderedByMtime []string `json:"ordered_by_mtime,omitempty"`
}

// capsSnapshot is resourceCaps on the wire, in the directives' own
// names and units (bytes, a count, a percentage of one CPU).
type capsSnapshot struct {
	MemoryMax uint64 `json:"memory_max,omitempty"`
	TasksMax  int    `json:"tasks_max,omitempty"`
	CPUQuota  int    `json:"cpu_quota,omitempty"`
}

// capsForStatus splits the unit's caps into the ones in effect and the
// ones the host does not enforce (named, not valued: a value the
// kernel ignores is not a cap). nil caps when nothing is in effect.
func capsForStatus(caps resourceCaps, unenforced []string) *capsSnapshot {
	off := map[string]bool{}
	for _, name := range unenforced {
		off[name] = true
	}
	s := &capsSnapshot{}
	if !off["memory_max"] {
		s.MemoryMax = caps.memoryMax
	}
	if !off["tasks_max"] {
		s.TasksMax = caps.tasksMax
	}
	if !off["cpu_quota"] {
		s.CPUQuota = caps.cpuQuota
	}
	if *s == (capsSnapshot{}) {
		s = nil
	}
	return s
}

func (ma *managedApp) status() statusSnapshot {
	c := ma.snapshot()
	var deploys []deploySummary
	available := []string{} // always an array in the JSON, never null
	var byMtime []string
	if c.spec != nil {
		// Read releases outside ma.mu — it is disk I/O, and status is polled.
		if rels, mtimes := listReleases(c.spec.dirs); rels != nil {
			available, byMtime = rels, mtimes
		}
		deploys = listDeploySummaries(c.spec.dirs)
	}
	ma.mu.Lock()
	defer ma.mu.Unlock()
	s := statusSnapshot{App: ma.name, Phase: ma.phase, LastDeploy: ma.lastDeploy, AvailableVersions: available, OrderedByMtime: byMtime, Deploys: deploys}
	if ma.current != nil {
		s.CurrentVersion = ma.current.version
		// Under the lock current_version comes from, so a cutover
		// since the disk read cannot leave the two disagreeing.
		s.AvailableVersions = runningFirst(s.AvailableVersions, s.CurrentVersion)
		s.OrderedByMtime = runningFirst(s.OrderedByMtime, s.CurrentVersion)
		s.Socket = ma.current.socket
		hs := ma.current.handle.state()
		s.PID = hs.PID
		s.Command = hs.Command
		s.Unit = hs.Unit
		s.Running = c.runner.Alive(ma.current.handle)
		if hs.Caps.any() && !hs.CapsUnknown {
			s.Caps, s.CapsUnenforced = capsForStatus(hs.Caps, hs.CapsUnenforced), hs.CapsUnenforced
		}
	}
	// After current: the watchdog records a restart before it installs
	// the new instance, so a status that shows the instance shows why
	// it was started.
	if c.spec != nil && c.clock != nil {
		s.Watchdog = ma.wd.statusSnapshot(c.clock.Now(), c.spec.wdWindow)
	}
	return s
}

// rollbackConfig restores the definition owner replaced, if owner's is
// still the one installed (a later config's stays). Used when a
// candidate is cleaned up while another config still holds the app.
func (ma *managedApp) rollbackConfig(owner any) bool {
	if !ma.restoreSpec(owner) {
		return false
	}
	// The candidate's caps reached the instance with its install
	// (configure): give it the serving definition's back.
	ma.syncCaps()
	return true
}

// restoreSpec is rollbackConfig's locked half.
func (ma *managedApp) restoreSpec(owner any) bool {
	ma.specMu.Lock()
	defer ma.specMu.Unlock()
	if ma.configuredBy != owner || ma.prev == nil {
		return false
	}
	p := ma.prev
	ma.spec, ma.verifiers, ma.logger, ma.store = p.spec, p.verifiers, p.logger, p.store
	ma.prev = nil
	ma.configuredBy = nil
	if sr, ok := ma.runner.(*systemdRunner); ok {
		sr.setLogger(p.logger)
	}
	// The watchdog re-snapshots the spec on a poke; without one, a
	// candidate that had turned it off could leave it parked on the
	// restored (watchdog on) definition.
	ma.pokeWatchdog()
	return true
}

// Known limitation, accepted: Caddy offers apps no "config accepted"
// hook, so between this app's Start and the whole config's activation
// the watchdog and recovery act on the candidate definition. Rollback
// restores the definition; it cannot undo an action taken in that
// window (a restart with the candidate's settings). Closing it would
// mean suspending supervision during every reload, which is worse
// than the milliseconds-wide window it removes.

// Destruct is called by the UsagePool when the last config referencing
// this app is unloaded — i.e. real shutdown or the app being removed
// from the Caddyfile, never a plain reload. The watchdog is stopped
// FIRST and waited for: only then is it impossible for a restart in
// flight to spawn a fresh process after the one below is stopped.
//
// On process exit the instance is deliberately left running: it is a
// systemd unit that does not depend on motel, state.json names it,
// and the next motel start reattaches to it — that is how apps
// survive motel restarts and upgrades. Only removing the app from
// the config stops it.
func (ma *managedApp) Destruct() error {
	ma.stopWatchdog()
	c := ma.snapshot()
	if c.runner == nil {
		return nil // pooled but never configured (a Start that failed before ours ran)
	}
	if caddyExiting() || liveStartedApps.Load() == 0 {
		// Process exit, `validate`, or a candidate config that never
		// became the serving one (another app's Start failed): the
		// units belong to whoever is or will be serving them. Leave
		// them; the next start's sweep settles any that are truly
		// orphaned.
		if inst := ma.currentInstance(); inst != nil {
			c.logger.Info("app keeps running for reattach", zap.String("version", inst.version), zap.String("unit", inst.handle.state().Unit))
		}
		return nil
	}
	// Removed by a reload that is now serving: no unit may outlive
	// the definition. A deploy of the app may still be in flight — a
	// reload stops the old config's servers without waiting for their
	// requests — and past the sweep below it would start a unit nothing
	// supervises, routes or stops. So the lock is taken, blocking: the
	// deploy ends first, within its own bounds (DESIGN-liveswap.md,
	// "Concurrency model"), and the reload's cleanup waits for it. It
	// is never released: a removed app takes no further deploy, and a
	// request the old config still serves gets a 409. The watchdog is
	// stopped above and the removing config's Cleanup has joined its
	// recovery, so nothing else can launch after this sweep either.
	// A wait is named: it holds the reload, and every config change
	// after it, for as long as the deploy runs.
	if !ma.deployMu.TryLock() {
		c.logger.Warn("app removed while a deploy of it is in flight; waiting for the deploy to end before stopping its units, and the reload completes then")
		ma.deployMu.Lock()
	}
	// Read under the lock: a deploy that just promoted left its own.
	inst := ma.currentInstance()
	// Stop what we track (if anything), then everything else the
	// manager holds for this app: a removed app must leave no unit
	// behind, tracked or not — an ambiguous start may have left one
	// that never became current.
	var stopErr error
	if inst != nil {
		c.logger.Info("app removed from config; stopping it", zap.String("version", inst.version))
		stopErr = c.runner.Stop(inst.handle, c.spec.grace)
		inst.sock.retire()
	}
	sweepErr := c.runner.StopAll(ma.name)
	if sweepErr == nil {
		pruneSockets(c.spec.dirs, nil, c.logger)
	}
	return errors.Join(stopErr, sweepErr)
}

// startWatchdog launches the supervision goroutine once per pooled
// app; reloads find wdStarted already set and leave it alone. The loop
// re-snapshots the spec every cycle, so config changes (including
// `watchdog off`) apply on its next iteration without a restart.
func (ma *managedApp) startWatchdog() {
	ma.specMu.Lock()
	defer ma.specMu.Unlock()
	if ma.wdStarted {
		return
	}
	ma.wdStarted = true
	ctx, cancel := context.WithCancel(context.Background())
	ma.wdCtx = ctx
	ma.wdCancel = cancel
	ma.wdWG.Add(1)
	go func() {
		defer ma.wdWG.Done()
		ma.watchdogLoop(ctx)
	}()
}

// stopWatchdog cancels the supervision goroutine and blocks until it
// has fully exited. Safe to call when the watchdog never started.
func (ma *managedApp) stopWatchdog() {
	ma.specMu.Lock()
	cancel := ma.wdCancel
	ma.specMu.Unlock()
	if cancel != nil {
		cancel()
	}
	ma.wdWG.Wait()
}

// pokeWatchdog wakes the supervision loop after current changed
// (promote, recovery). Non-blocking: the channel is buffered and a
// pending poke already means "re-examine the world".
func (ma *managedApp) pokeWatchdog() {
	select {
	case ma.wdNotify <- struct{}{}:
	default:
	}
}

// envAllowlist is the only part of Caddy's own environment that apps
// inherit. Everything else is withheld: the supervisor's env holds
// secrets (ACME DNS tokens, and whatever the operator sets), and
// handing those to every app defeats the isolation story — env-dumping
// supply-chain payloads read process.env before they read files.
// Operators pass anything extra explicitly via env_file or env.
var envAllowlist = []string{"PATH", "LANG", "TZ"}

func inheritedEnv() []string {
	var env []string
	for _, key := range envAllowlist {
		if v, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+v)
		}
	}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "LC_") {
			env = append(env, kv)
		}
	}
	return env
}

// buildEnv assembles the child environment. Precedence, lowest to
// highest: the allowlisted slice of Caddy's environment (PATH, LANG,
// TZ, LC_*), the sandbox HOME, env_file, inline env, then the injected
// SOCKET contract. HOME is never inherited: motel's own state
// dir does not exist inside the unit at all, so the app's HOME is its
// shared dir — the one writable, persistent place in the view. Not a
// nicety: every runtime that touches $HOME would ENOENT, which is the
// shape a deny-by-default view fails in. Applied before env_file and
// env, so an operator can still point it elsewhere.
func buildEnv(spec *appSpec, version, socket, releaseDir string) ([]string, error) {
	env, _, _, err := buildEnvFull(spec, version, socket, releaseDir)
	return env, err
}

// buildEnvFull is buildEnv plus the env_file pairs on their own: the
// values the response filter treats as secrets. Inline `env` is not
// among them — it lives in the Caddyfile, in a repo, and is not secret
// by policy — and neither are HOME, SOCKET or the inherited PATH,
// which a diagnostic has to be able to name.
func buildEnvFull(spec *appSpec, version, socket, releaseDir string) (env, secrets []string, envFileMode os.FileMode, err error) {
	env = append(inheritedEnv(), "HOME="+spec.dirs.shared)
	if spec.envFile != "" {
		fileVars, perm, err := readEnvFile(spec.envFile)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("env_file: %w", err)
		}
		env = append(env, fileVars...)
		secrets, envFileMode = fileVars, perm
	}
	for k, v := range spec.env {
		env = append(env, k+"="+expandPlaceholders(v, spec, version, socket, releaseDir))
	}
	env = append(env, "SOCKET="+socket)
	return env, secrets, envFileMode, nil
}

// envKeyRe is what systemd accepts in Environment=; the exec runner
// let anything through to execve, so it is validated explicitly now —
// at config load for inline env, at deploy time for env_file.
var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validEnvKey(key string) bool { return envKeyRe.MatchString(key) }

// shellOwnedEnvKeys are the variables the sibling shell every unit
// runs behind (withSibling, systemd_dbus.go) sets for itself at its
// start whatever it inherited — measured on Debian 13's dash: IFS to
// space-tab-newline, OPTIND to 1, PPID to its own parent, and PWD to
// the working directory, which the script then unsets. Everything
// else passes byte for byte and nothing is added. A value given for
// one of these would not reach the app as built (invariant 8 of the
// runner), so every place a key enters refuses it by name instead.
var shellOwnedEnvKeys = map[string]bool{"IFS": true, "OPTIND": true, "PPID": true, "PWD": true}

func shellOwnedEnvKey(key string) bool { return shellOwnedEnvKeys[key] }

const shellOwnedEnvKeyMsg = "is set by the shell every app unit runs behind (/bin/sh resets IFS, OPTIND, PPID and PWD at its start), so an app cannot be given it"

// The env_file mode check. motel reads the file as the motel
// user, so its mode is not motel's concern; the documented
// `install -m 0640 -o root -g motel` is for everyone else on the
// box — an account that can read the file holds every secret in it,
// and one that can write it chooses the app's next environment. Apps
// are not among those accounts whatever the mode: /etc/motel is
// outside every view, validateEnvFileIsolation refuses an env_file
// inside the base view or a sibling's dirs, and one inside the app's
// own dirs is warnEnvFileInView's warning. A warning, never a refusal,
// in two places: at config load, by a stat of the path (loading the
// config does not open the file); and at each launch, from the
// descriptor the environment was just read through, so the mode
// reported is that of the inode whose values the unit got — a chmod
// between loads must not take an app down on a 3am relaunch, so a
// launch warns and goes on, and is not silent until a reload that may
// never come. Not checked: the owner and group (the service's group
// cannot be told from a stranger's when `validate` runs as another
// account, and the documented install line sets both), and the
// directories above the file, which is why the message says "any
// account that can reach it".

// warnEnvFileMode is the config-load half: warnEnvFileModeOf for every
// app that names an env_file.
func warnEnvFileMode(logger *zap.Logger, specs map[string]*appSpec) {
	for name, spec := range specs {
		warnEnvFileModeOf(logger, name, spec.envFile)
	}
}

// warnEnvFileModeOf stats one app's env_file at config load. An absent
// file is silent — the launch that needs it fails and says so. One
// this account cannot reach is noted, not warned about: `validate`
// runs unprivileged by design, and the launch checks the file as the
// user that reads it. Any other stat failure is reported as such — a
// line that reads "checked, and fine" over an error is worse than
// none.
func warnEnvFileModeOf(logger *zap.Logger, app, path string) {
	if logger == nil || path == "" {
		return
	}
	fi, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return
	case errors.Is(err, os.ErrPermission):
		logger.Info("env_file mode not checked: this account cannot reach the file; the launch, as the user that reads it, checks it",
			zap.String("app", app), zap.String("env_file", path), zap.Error(err))
		return
	case err != nil:
		logger.Warn("env_file mode could not be checked", zap.String("app", app), zap.String("env_file", path), zap.Error(err))
		return
	}
	warnEnvFileModeBits(logger, app, path, fi.Mode().Perm())
}

// warnEnvFileModeBits is the check itself, on a mode already in hand:
// world-readable, world-writable, each its own line.
func warnEnvFileModeBits(logger *zap.Logger, app, path string, perm os.FileMode) {
	if logger == nil || perm&0o006 == 0 {
		return
	}
	mode := zap.String("mode", fmt.Sprintf("%04o", perm))
	fix := zap.String("fix", "chmod 0640 and chown root:motel — the documented `install -m 0640 -o root -g motel`")
	if perm&0o004 != 0 {
		logger.Warn("env_file is world-readable: any account that can reach it holds every value in it",
			zap.String("app", app), zap.String("env_file", path), mode, fix)
	}
	if perm&0o002 != 0 {
		logger.Warn("env_file is world-writable: any account that can reach it chooses the app's next environment",
			zap.String("app", app), zap.String("env_file", path), mode, fix)
	}
}

// parseEnvFile reads simple KEY=VALUE lines: blank lines and #comments
// skipped, an optional `export ` prefix tolerated, and single or
// double quotes around the value stripped. Deliberately not a shell.
func parseEnvFile(path string) ([]string, error) {
	vars, _, err := readEnvFile(path)
	return vars, err
}

// readEnvFile is parseEnvFile plus the mode of the inode it read, taken
// from the open descriptor: a launch warns about the file it rendered,
// not about whatever the path names a moment later.
func readEnvFile(path string) (vars []string, perm os.FileMode, err error) {
	f, err := os.Open(path) //nolint:gosec // path is the operator's env_file config value, not request input
	if err != nil {
		return nil, 0, err
	}
	defer f.Close() //nolint:errcheck // read-only
	fi, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, 0, err
	}
	vars, err = parseEnvLines(path, data)
	return vars, fi.Mode().Perm(), err
}

// parseEnvLines is parseEnvFile on bytes already read; path is for the
// error messages.
func parseEnvLines(path string, data []byte) ([]string, error) {
	var vars []string
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, found := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !found || key == "" {
			return nil, fmt.Errorf("%s:%d: not KEY=VALUE", path, i+1)
		}
		if !validEnvKey(key) {
			return nil, fmt.Errorf("%s:%d: %q is not a valid environment variable name (systemd requires %s)", path, i+1, key, envKeyRe)
		}
		if shellOwnedEnvKey(key) {
			return nil, fmt.Errorf("%s:%d: %q %s", path, i+1, key, shellOwnedEnvKeyMsg)
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		vars = append(vars, key+"="+value)
	}
	return vars, nil
}

// ParseEnvLines is parseEnvLines, the reading every launch makes, for a
// caller that holds the bytes: the box, which reads the env file a push
// replaces to tell which variables it adds, changes or removes
// (box/DESIGN-box.md, "Record and result fields"). name is for the
// messages.
func ParseEnvLines(name string, data []byte) ([]string, error) {
	return parseEnvLines(name, data)
}

// ValidEnvKey reports whether key is a name an env file may carry: what
// systemd accepts in Environment= (envKeyRe), less the four names the
// sibling shell resets at its start (shellOwnedEnvKeys), which
// RenderEnvFile refuses — so the box's name checks refuse a secret
// called PWD where it is named, not at the render.
func ValidEnvKey(key string) bool { return validEnvKey(key) && !shellOwnedEnvKey(key) }

// RenderEnvFile is the inverse of the reading above, for the files the box
// writes from a repository's secrets (box/DESIGN-box.md, "Values and the
// env file"): one `KEY="value"` line per pair, sorted by key, so the
// bytes depend only on the pairs. parseEnvLines trims the line and the
// value, then strips one pair of matching quotes and does nothing else —
// no escapes — so every value inside double quotes reads back exactly:
// inner quotes, edge spaces, `#` and `=` included. What it cannot carry
// is refused rather than altered: a value with a line break (the reading
// is line by line, and a CR at a line's end is trimmed), a NUL, or bytes
// that are not UTF-8 (the unit's Environment= travels over D-Bus, whose
// strings are UTF-8); a key systemd would reject; a key given twice.
func RenderEnvFile(vars []string) ([]byte, error) {
	type pair struct{ key, value string }
	pairs := make([]pair, 0, len(vars))
	for _, kv := range vars {
		key, value, found := strings.Cut(kv, "=")
		if !found || !validEnvKey(key) {
			return nil, fmt.Errorf("%q is not a valid environment variable name (systemd requires %s)", key, envKeyRe)
		}
		if shellOwnedEnvKey(key) {
			return nil, fmt.Errorf("%q %s", key, shellOwnedEnvKeyMsg)
		}
		pairs = append(pairs, pair{key, value})
	}
	// By key, not by line: '=' sorts after the digits, so "A9=…" would
	// come before "A=…" in a sort of the lines.
	slices.SortFunc(pairs, func(a, b pair) int { return strings.Compare(a.key, b.key) })
	var b bytes.Buffer
	for i, p := range pairs {
		key, value := p.key, p.value
		if i > 0 && key == pairs[i-1].key {
			return nil, fmt.Errorf("%s is given twice", key)
		}
		switch {
		case strings.ContainsAny(value, "\n\r"):
			return nil, fmt.Errorf("the value of %s has a line break, which an env file cannot carry", key)
		case strings.ContainsRune(value, 0):
			return nil, fmt.Errorf("the value of %s has a NUL byte, which an environment cannot carry", key)
		case !utf8.ValidString(value):
			return nil, fmt.Errorf("the value of %s is not UTF-8", key)
		}
		b.WriteString(key)
		b.WriteString(`="`)
		b.WriteString(value)
		b.WriteString("\"\n")
	}
	return b.Bytes(), nil
}

// expandPlaceholders substitutes the deploy-time placeholders that are
// only knowable mid-deploy. These intentionally use the same brace
// style as Caddy placeholders but are resolved here, not by a
// caddy.Replacer — Provision's ReplaceKnown leaves them alone.
func expandPlaceholders(s string, spec *appSpec, version, socket, releaseDir string) string {
	return strings.NewReplacer(
		"{version}", version,
		"{socket}", socket,
		"{release_dir}", releaseDir,
		"{shared_dir}", spec.dirs.shared,
	).Replace(s)
}

func expandArgs(args []string, spec *appSpec, version, socket, releaseDir string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = expandPlaceholders(a, spec, version, socket, releaseDir)
	}
	return out
}

// hostOf returns the host[:port] of a raw URL for logging. It parses
// with net/url so any userinfo credentials (user:pass@host, e.g. a
// tokenised artifact URL from CI) are excluded — url.URL keeps those in
// .User, never in .Host — rather than slicing the raw string, which
// would log the secret. Empty string if the URL does not parse.
func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Host
}

// fetchClients bundles the shared HTTP clients handed to each
// managedApp at configure time.
type fetchClients struct {
	download *http.Client
	jwks     *http.Client // fetches OIDC issuers' public keys for deploy auth
}
