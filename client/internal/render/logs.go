package render

import (
	"io"
	"time"

	"github.com/smallhoursorg/motel/client/internal/termtext"
)

// LogLine is one line of an app's output: when the journal stored it,
// the release that wrote it (the box's version, or the client's words
// when the journal kept none), and its text made safe.
type LogLine struct {
	At            time.Time
	Release, Text termtext.Text
}

// Logs writes an app's lines as reads bring them. Each line is its time
// (UTC, to the second) and its text, whole as the box sent it. The
// release is not on every line: a divider, before the first line and
// wherever the release or the day (UTC) changes, names the day and the
// release. A notice of the client's (Note) is a divider of its own, and
// the line after it has its divider again.
type Logs struct {
	W       io.Writer
	began   bool
	day     string
	release termtext.Text
}

// Lines writes lines after those written before.
func (l *Logs) Lines(lines []LogLine) error {
	p := &printer{w: l.W}
	for _, ln := range lines {
		day := ln.At.UTC().Format("2006-01-02")
		if !l.began || day != l.day || ln.Release != l.release {
			p.line("-- %s UTC, release %s --", day, ln.Release)
			l.began, l.day, l.release = true, day, ln.Release
		}
		p.line("%s  %s", ln.At.UTC().Format("15:04:05"), ln.Text)
	}
	return p.err
}

// Note writes the client's own sentence s on a divider.
func (l *Logs) Note(s string) error {
	l.began = false
	p := &printer{w: l.W}
	p.line("-- %s --", s)
	return p.err
}
