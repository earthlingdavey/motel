package wire

import "time"

// LogsLines are the counts GET /_motel/apps/<app>/logs?lines= takes,
// the first the default: a closed set, so that a token cannot ask the
// journal for any count it likes. Any other is refused (422).
// Experimental, as every route under /_motel/.
var LogsLines = []string{"200", "50", "1000"}

// The words of Logs.Kept.
const (
	// KeptOn: the app's output is read from the journal on the box.
	KeptOn = "on"
	// KeptOff: the app keeps its output on the box (deploy_log_lines 0),
	// so no line is ever sent, and Lines is empty.
	KeptOff = "off"
)

// Logs is GET /_motel/apps/<app>/logs's body: the app's own lines from
// the box's journal, oldest first (liveswap/README.md, "Reading an app's
// logs"). A read with no after is a tail, the newest lines; one with
// after (a Cursor an earlier answer gave) is the lines after that one.
// Each text has passed the app's response filter, which no filter can
// make a promise of: the lines carry what the app printed.
// Experimental: its fields may change until a release's notes say
// otherwise.
type Logs struct {
	// Kept is KeptOn, or KeptOff for an app that keeps its output on the
	// box. A client reads a word it does not know as not knowing.
	Kept  string    `json:"kept"`
	Lines []LogLine `json:"lines"`
	// More is true when the read stopped with a line left unsent: the
	// count asked for, or 64 KiB of the lines' text as the journal holds
	// it, was reached. After a
	// follow (after sent, Gap false) the lines left are after the last
	// one sent: a follower asks again at once, with Cursor. After a tail
	// (no after, or Gap true) they are older than the first one sent,
	// and no read reaches them.
	More bool `json:"more"`
	// Gap is true when the after a request sent was not honoured (it is
	// no entry of this app's that the journal still holds): Lines are
	// then a fresh tail, and lines between the two may be missing. With
	// no Cursor beside it (the app has no line the journal holds), the
	// client forgets its after and tails, or every poll is a gap.
	Gap bool `json:"gap"`
	// Cursor is the journal's cursor of the last entry this read took,
	// the after of the next read; absent when the read took none, and
	// then the client keeps the one it sent. It is the box's position
	// in its journal, to be held in memory only, never written to disk.
	Cursor string `json:"cursor,omitempty"`
	// RedactedEnv names the env_file keys whose values the filter
	// replaced; absent when none.
	RedactedEnv []string `json:"redacted_env,omitempty"`
}

// LogLine is one line the app wrote to its stdout or stderr, which the
// journal does not tell apart.
type LogLine struct {
	// At is when the journal stored the line.
	At time.Time `json:"at"`
	// Release is the version of the unit that wrote the line, when the
	// journal kept the unit; "" when it did not (a process that exited
	// before journald read its line, any line while journald is behind).
	Release string `json:"release"`
	// Text is the line, valid UTF-8, at most 4 KiB: a longer one is cut
	// and ends in "…". A run of bytes that are not UTF-8 is one U+FFFD; control
	// characters are as the app wrote them, for a client to escape.
	Text string `json:"text"`
}
