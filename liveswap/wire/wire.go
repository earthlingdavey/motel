// Package wire is the deploy route's JSON on the wire: the bodies
// liveswap's webhook answers with (GET /<app>, GET /<app>?deploy=<v>,
// a deploy's response and its ndjson lines, every refusal) and the box
// webhook's GET /. liveswap marshals these types, so they are the
// shape and nothing is copied: a client decodes into them, and can be
// tested against the real handlers in-process.
//
// The types are the shape and nothing else: no state, no behaviour.
// Field order is part of the shape — the response filter keeps a
// nested object's keys in the order they are marshalled (liveswap's
// withField sorts only the top level), and a client reading "the last
// phase" of a failure body by position relies on it — so a field is
// added in the place it belongs, never moved. liveswap/testdata/wire
// pins every body byte for byte.
//
// Every body passes the app's response filter after it is marshalled
// (liveswap/redact.go): any string can come back as a marker, and a
// body the filter changed gains a top-level "redacted_env" naming the
// env keys whose values it replaced, which only DeployResult has a
// field for.
//
// The package imports only the standard library, so a program that
// reads these bodies does not link Caddy (TestImportsOnlyTheStandardLibrary).
package wire

import (
	"encoding/json"
	"time"
)

// Status is GET /<app>'s body, and a successful deploy's response.
type Status struct {
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
	// applied them: set at the unit's start, changed live by a reload,
	// or read back at reattach. Absent when none is set, and without
	// any cap the host does not enforce.
	Caps *Caps `json:"caps,omitempty"`
	// CapsUnenforced names the configured caps the host does not
	// enforce: the user manager does not delegate their controller to
	// the unit's cgroup, so the manager holds the value and the kernel
	// bounds nothing (not the case on Debian 13). Absent when every
	// cap is enforced.
	CapsUnenforced []string `json:"caps_unenforced,omitempty"`
	// Deploys is the outcome of the latest deploy of each version a
	// record is kept for (liveswap/deploys.go), newest first; the whole
	// record is GET /<app>?deploy=<version>. Serialized before
	// last_deploy: the examples' deploy.sh reads the failing phase as
	// the last "phase" in a failure body, which must stay last_deploy's.
	// That body carries the status under "status", whose keys' order
	// the filter keeps.
	Deploys    []DeploySummary `json:"deploys,omitempty"`
	LastDeploy *DeployResult   `json:"last_deploy,omitempty"`
	Watchdog   *Watchdog       `json:"watchdog,omitempty"`
	// AvailableVersions lists the on-disk releases, the running version
	// first, then most recently deployed first (liveswap/state.go,
	// "Release order") — the versions `?rollback=<version>` can
	// relaunch. Always serialized (a healthy app with no releases
	// reports []), so an empty set is distinguishable from a server
	// that doesn't report the field.
	AvailableVersions []string `json:"available_versions"`
	// OrderedByMtime names the entries of AvailableVersions placed by
	// their release dir's mtime, which the app can set: those with no
	// deploy record (deployed before records, a record write that
	// failed, or a first deploy still in flight) and those whose record
	// is there but cannot be read. Each comes after every recorded
	// release unless it is the running version, which the list puts
	// first; absent when there are none.
	OrderedByMtime []string `json:"ordered_by_mtime,omitempty"`
	// Metrics is the metrics store's summary of the app's numbers
	// (notify/DESIGN-metrics.md, "metrics in GET /<app>"): in GET /<app>
	// alone, and only when the monitor block's metrics line is loaded.
	Metrics *Metrics `json:"metrics,omitempty"`
}

// Caps is the unit's resource caps, in the directives' own names and
// units (bytes, a count, a percentage of one CPU).
type Caps struct {
	MemoryMax uint64 `json:"memory_max,omitempty"`
	TasksMax  int    `json:"tasks_max,omitempty"`
	CPUQuota  int    `json:"cpu_quota,omitempty"`
}

// DeploySummary is a record's outcome as the status lists it. The
// times are carried as the record has them, not parsed: a record is
// the filter's output, and a filter that replaced a value the shape
// of a timestamp must not make the record vanish from the list. The
// list is ordered by the record's write time instead. The pin is
// carried the same way, for the same reason: a digest equal to an
// env_file value is redacted in the record like any other.
type DeploySummary struct {
	Version    string          `json:"version"`
	Status     string          `json:"status"`
	Phase      string          `json:"phase,omitempty"` // where it failed
	By         string          `json:"deployed_by,omitempty"`
	SHA256     json.RawMessage `json:"sha256,omitempty"` // the pin; absent when there was none
	StartedAt  json.RawMessage `json:"started_at,omitempty"`
	FinishedAt json.RawMessage `json:"finished_at,omitempty"`
}

// DeployResult is one deploy attempt's outcome: the status's
// last_deploy, and GET /<app>?deploy=<v>'s body (the record, as the
// filter left it). A record the filter could not keep whole is an
// envelope of some of the same fields — version, status, phase,
// sha256, the times — in sorted order, whose error says why the rest
// is withheld.
type DeployResult struct {
	Version    string    `json:"version"`
	Status     string    `json:"status"` // "succeeded" | "failed"
	Error      string    `json:"error,omitempty"`
	Phase      string    `json:"phase,omitempty"`       // phase reached when it failed
	By         string    `json:"deployed_by,omitempty"` // the trust source that authorized it, and the token's attribution claims
	SHA256     string    `json:"sha256,omitempty"`      // the pin the deployer asserted, the digest of the bytes a push uploaded, or a rollback's carried from the record it replaces; never a hash of an unpinned download
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	// Note says what the deploy did that its outcome words do not: a
	// deploy or rollback naming the running version relaunched it from
	// the release already on disk. Empty otherwise. It is in
	// last_deploy and the response only — a relaunch writes no record,
	// so the version's record keeps the deploy that put it there, its
	// pin and its times.
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
	Phases []PhaseTiming `json:"phases,omitempty"`
	// Detail is why a failed deploy failed, from the app's side: how
	// its process ended, what its health endpoint answered, the last
	// lines it wrote. Nil for a success, and for a failure before a
	// launch (nothing of the app's ran).
	Detail *DeployDetail `json:"detail,omitempty"`
	// RedactedEnv is the env keys whose values the detail's early
	// replacement found: by the time the response filter runs they are
	// markers already, so it reports them from here — this field,
	// wherever the result sits in a body, is hoisted into the body's
	// own `redacted_env` (liveswap's withField).
	RedactedEnv []string `json:"redacted_env,omitempty"`
}

// PhaseTiming is one phase a deploy passed through.
type PhaseTiming struct {
	Name      string    `json:"name"`
	StartedAt time.Time `json:"started_at"`
	Seconds   float64   `json:"seconds"`
}

// DeployDetail is the app's side of a failed deploy. Every string in
// it has passed the response filter, and the known values were
// replaced as it was built, before the tail and body caps cut, so a
// cap never leaves a fragment of one.
type DeployDetail struct {
	// Exit is how the process ended, from the runner: "exit status 3",
	// "killed by signal 9 (killed)". The pre_start's when that failed,
	// otherwise the app's, when it died before becoming healthy.
	Exit string `json:"exit,omitempty"`
	// Probe is the last health probe that completed with a non-2xx
	// answer: the status, a redirect's target, the first bytes of the
	// body — where "Invalid HTTP_HOST header" or "no such table" is.
	// The body is the app's output and deploy_log_lines 0 leaves it
	// out, as it does the tail; the status and location stay.
	Probe *ProbeDetail `json:"probe,omitempty"`
	// LogTail is the last lines the app's units wrote to the journal
	// since the deploy began — pre_start and app alike — bounded by
	// deploy_log_lines and 8 KiB; LogTailTruncated says when a cap
	// dropped lines. LogTailError says why there is no tail when there
	// should have been one (journalctl missing, the journal unreadable).
	LogTail          []string `json:"log_tail,omitempty"`
	LogTailTruncated bool     `json:"log_tail_truncated,omitempty"`
	LogTailError     string   `json:"log_tail_error,omitempty"`
}

// ProbeDetail is the last non-2xx health probe answer.
type ProbeDetail struct {
	Status   int    `json:"status"`
	Location string `json:"location,omitempty"`
	Body     string `json:"body,omitempty"`
}

// Watchdog is the app's watchdog as the status reports it.
type Watchdog struct {
	State            string    `json:"state"`
	Failures         int       `json:"consecutive_failures,omitempty"`
	RestartsInWindow int       `json:"restarts_in_window,omitempty"`
	LastRestartAt    time.Time `json:"last_restart_at,omitzero"`
	LastRestartCause string    `json:"last_restart_cause,omitempty"`
	LastFailure      string    `json:"last_failure,omitempty"`
}

// Error is every refusal's body, and every failure's but a failed
// deploy's: the 401, 404, 405, 409, 422, 503 and the 500s before a
// deploy runs, and a body the filter withheld whole.
type Error struct {
	Error string `json:"error"`
}

// Failure is a deploy that failed once it ran (500): why, and the
// status as it is after it — the old version still serving.
type Failure struct {
	Error  string `json:"error"`
	Status Status `json:"status"`
}

// A streamed deploy (Accept: application/x-ndjson) is one JSON object
// per line: a PhaseEvent for each phase as the pipeline enters it,
// then the single response's body — Status, Failure or Error — with
// DoneEvent's two fields added at its end. An outcome reached before
// any phase is the single response itself, with its own code.
const (
	EventPhase = "phase"
	EventDone  = "done"
)

// PhaseEvent is a streamed deploy's line for one phase entered. A line
// whose body the filter withheld (an env_file it could not read, a
// known value overlapping the line's own structure) has Error, why, in
// place of At, and may have a top-level "redacted_env" beside it.
type PhaseEvent struct {
	Error string    `json:"error,omitempty"`
	At    time.Time `json:"at,omitzero"`
	Event string    `json:"event"` // EventPhase
	Phase string    `json:"phase"`
}

// DoneEvent is the fields the last line adds to the single response's
// body: the event, and the status code that response would have had.
// The handler appends them to the filtered body, so they are there
// when the filter withheld the rest — and then the body is an Error
// whatever HTTPStatus says, a 200 included: which body a line carries
// is told by its keys ("error" alone, or "error" and "status", or
// neither), never by its code.
type DoneEvent struct {
	Event      string `json:"event"` // EventDone
	HTTPStatus int    `json:"http_status"`
}

// BoxStatus is the box webhook's GET / body: the commit the box runs,
// the host its box_webhook answers on, the digest of the Caddyfile it
// runs, and the box's key for secrets.
type BoxStatus struct {
	Commit     string `json:"commit"`
	BoxWebhook string `json:"box_webhook"`
	SHA256     string `json:"sha256"`
	// Recipient is the box's key for secrets, which the workflow
	// compares with the first line of recipients ("The box's key").
	// Absent when the box has none.
	Recipient string `json:"recipient,omitempty"`
}
