package render

import (
	"io"

	"github.com/smallhoursorg/motel/client/internal/termtext"
)

// Rules is the rules the box alerts on as the screen lays them: each
// line the client's words, and the box's after the client's labels or
// quoted (the TUI model's RuleBlock), whole.
type Rules struct {
	Lines []termtext.Text
}

// WriteRules writes the rules, a line each. No line begins with the
// box's text.
func WriteRules(w io.Writer, r Rules) error {
	p := &printer{w: w}
	for _, l := range r.Lines {
		p.line("%s", l)
	}
	return p.err
}
