package model

import (
	"time"

	"github.com/smallhoursorg/motel/client/internal/termtext"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// An app's lines (GET /_motel/apps/<app>/logs, `motel logs <app>`): how
// a follower of them reads on, and how a line is shown. `motel logs`
// uses these, and a screen of the lines is to use the same.

// LogsEvery is how often an app's lines are read while they are
// followed: 0.5 reads a second, so a follow beside an open TUI (1.5)
// brings a person to the box's 2. Each read that sends lines is an
// `access: person read logs` line in the box's journal
// (docs/people.md, "What the box records"). A read that stops with
// lines left after the last one sent is followed by the next at once
// (LogsTurn.Again).
const LogsEvery = 2 * time.Second

// LogsFollow is where a follower of an app's lines has read to: the
// cursor of the last line the box sent, which is the box's position in
// its journal, kept in memory only and never written anywhere. The zero
// LogsFollow has read nothing, and its first read is a tail.
type LogsFollow struct {
	after string
}

// After is the after of the next read: "" for a tail.
func (f *LogsFollow) After() string { return f.after }

// LogsTurn is what one answer means to a follower, beside its lines.
type LogsTurn struct {
	// Off: the app keeps its output on the box (kept "off"): no line of
	// it is ever sent, so there is nothing to follow.
	Off bool
	// Gap: the box did not honour the after sent (its line is no longer
	// in the journal, or was never this app's): the lines are a fresh
	// tail, and lines between may be missing.
	Gap bool
	// Older: the read was a tail and stopped with lines left, which are
	// older than the first one sent, and a follow's reads never reach
	// them (no read pages back; a tail of a larger count may reach further).
	Older bool
	// Again: the read was a follow and stopped with lines left, which are
	// after the last one sent: the next read is made at once.
	Again bool
}

// Take is l, the answer to a read made with After, and moves the
// follower on: to l's cursor when it has one; to a tail when the box
// honoured no after and gave no cursor (the app has no line the journal
// holds), or every read would be a gap; else it stays where it was (the
// read took no line). l's kept is the caller's to have held to a word
// it knows.
func (f *LogsFollow) Take(l wire.Logs) LogsTurn {
	if l.Kept == wire.KeptOff {
		return LogsTurn{Off: true}
	}
	sent := f.after != ""
	tail := !sent || l.Gap
	// Again only when the read moved on: a follow's more with no cursor,
	// or with the one sent (no moteld sends either), would ask for the
	// same lines at once, again and again.
	moved := l.Cursor != "" && l.Cursor != f.after
	t := LogsTurn{Gap: sent && l.Gap, Older: l.More && tail, Again: l.More && !tail && moved}
	switch {
	case l.Cursor != "":
		f.after = l.Cursor
	case l.Gap:
		f.after = ""
	}
	return t
}

// LogsNoRelease is the client's words for the release of a line whose
// unit the journal did not keep (a process that exited before journald
// read its line, any line while journald is behind).
const LogsNoRelease = "not kept by the journal"

// LogsLine is one line of an app's output as a person reads it: when
// the journal stored it, the release that wrote it (the box's version,
// held to its grammar, or LogsNoRelease), and the line made safe to
// show: a tab as spaces to the next stop (termtext.Tabbed), and every other
// control character and colour code written as an escape
// (termtext.From), never passed to the terminal.
type LogsLine struct {
	At      time.Time
	Release termtext.Text
	Text    termtext.Text
}

// LogsLines are the box's lines as LogsLines, in the box's order.
func LogsLines(lines []wire.LogLine) []LogsLine {
	out := make([]LogsLine, len(lines))
	for i, l := range lines {
		rel := l.Release
		if rel == "" {
			rel = LogsNoRelease
		}
		out[i] = LogsLine{At: l.At, Release: termtext.From(rel), Text: termtext.Tabbed(l.Text)}
	}
	return out
}
