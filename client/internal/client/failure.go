package client

import "time"

// The exit codes. Scripts branch on them, so each keeps its meaning
// once a release ships them; every command is experimental until then.
const (
	// ExitOK: done, and the answer was read, whatever it says (a
	// status read exits 0 whatever the app's phase).
	ExitOK = 0
	// ExitFailed: the box answered, and the answer is a failure (a 409
	// from box status). A 5xx is ExitUnknown: nothing tells moteld's own
	// from a gateway's.
	ExitFailed = 1
	// ExitUsage: nothing was sent (bad arguments, no box or key named,
	// an RSA or an ECDSA key).
	ExitUsage = 2
	// ExitUnknown: could not tell (no answer, a timeout, a body that is
	// not moteld's, any 5xx, a 429 that stood after one wait).
	ExitUnknown = 3
	// ExitRefused: a 401 that stood after one retry.
	ExitRefused = 4
	// ExitForbidden: a 403.
	ExitForbidden = 5
	// ExitNotFound: a 404 (an unknown deploy version).
	ExitNotFound = 6
	// ExitTooOld is held for a box too old for people's tokens. Nothing
	// returns it: no answer names motel's protocol in a header, so such
	// a box's 401 is the same as a key it does not list, and the 401's
	// message names both.
	ExitTooOld = 7
	// ExitCertificate: the box's certificate did not verify. The
	// client never continues past one.
	ExitCertificate = 8
	// ExitInterrupted: SIGINT.
	ExitInterrupted = 130
)

// Failure is a command that did not get its answer: the exit code,
// what the client says, and, when the box's own body is the answer, that
// body for --json.
type Failure struct {
	Exit int
	// Message is safe to print: the client's own words, with whatever a
	// box or a person sent in it made safe (termtext) when it was put
	// in.
	Message string
	// Body is the box's answer as received, printed by --json in place
	// of an {"error"} of the client's.
	Body []byte
	// Status is the HTTP status of the answer the failure is about, and
	// zero when none came; RetryAfter, on a 429, the wait the box asked
	// for. The TUI's engine reads them: a 401 stops it, a 403 stops one
	// app's reads, a 429 slows it.
	Status     int
	RetryAfter time.Duration
}

func (f *Failure) Error() string { return f.Message }
