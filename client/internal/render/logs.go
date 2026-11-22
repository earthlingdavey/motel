package render

import (
	"io"

	"github.com/smallhoursorg/motel/client/internal/termtext"
)

// LogLine is one line of an app's output as the layout writes it: the
// divider before it when one is written (the words between its dashes,
// the day and the release), its time (UTC, to the second) and its text
// made safe. Which lines have a divider, and its words, are the layout's
// (model.LogsDivider), shared with the TUI's logs screen.
type LogLine struct {
	Divider termtext.Text
	Clock   string
	Text    termtext.Text
}

// Logs writes an app's lines as reads bring them, each its time and its
// text, whole as the box sent it, after its divider when it has one. A
// notice of the client's (Note) is a divider of its own.
type Logs struct {
	W io.Writer
}

// Lines writes lines after those written before.
func (l *Logs) Lines(lines []LogLine) error {
	p := &printer{w: l.W}
	for _, ln := range lines {
		if !ln.Divider.Empty() {
			p.line("-- %s --", ln.Divider)
		}
		p.line("%s  %s", ln.Clock, ln.Text)
	}
	return p.err
}

// Note writes the client's own sentence s on a divider.
func (l *Logs) Note(s string) error {
	p := &printer{w: l.W}
	p.line("-- %s --", s)
	return p.err
}
