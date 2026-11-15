package client

// The exit codes. Scripts branch on them, so each keeps its meaning
// once a release ships them; every command is experimental until then.
const (
	// ExitOK: done, and the answer was read, whatever it says (a
	// status read exits 0 whatever the app's phase).
	ExitOK = 0
	// ExitFailed: the box answered, and the answer is a failure (a 409
	// from box status, a 5xx in motel's own words).
	ExitFailed = 1
	// ExitUsage: nothing was sent (bad arguments, no box or key named,
	// an RSA key).
	ExitUsage = 2
	// ExitUnknown: could not tell (no answer, a timeout, a body that is
	// not motel's, a 429 that stood after one wait).
	ExitUnknown = 3
	// ExitRefused: a 401 that stood after one retry.
	ExitRefused = 4
	// ExitForbidden: a 403.
	ExitForbidden = 5
	// ExitNotFound: a 404 (an unknown deploy version).
	ExitNotFound = 6
	// ExitTooOld is held for a box whose answer carries no
	// Motel-Protocol: one that predates people's tokens. Nothing
	// returns it until boxes send the header.
	ExitTooOld = 7
	// ExitCertificate: the box's certificate did not verify. motelctl
	// never continues past one.
	ExitCertificate = 8
	// ExitInterrupted: SIGINT.
	ExitInterrupted = 130
)

// Failure is a command that did not get its answer: the exit code,
// what motelctl says, and, when the box's own body is the answer, that
// body for --json.
type Failure struct {
	Exit int
	// Message is safe to print: motelctl's own words, with whatever a
	// box or a person sent in it made safe (termtext) when it was put
	// in.
	Message string
	// Body is the box's answer as received, printed by --json in place
	// of an {"error"} of motelctl's.
	Body []byte
}

func (f *Failure) Error() string { return f.Message }
