package model

import (
	"slices"
	"strconv"
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

// The layout of an app's lines, which `motel logs` prints and the logs
// screen draws, each from these, so the two cannot differ: each line is
// its Clock and its text; a divider (LogsDivider) is written before the
// first line, after a note of the client's, and wherever the release or
// the day changes; and the client's notes among the lines are these
// sentences, each a divider of its own.

// Clock is the line's time as the layout shows it: UTC, to the second.
func (l LogsLine) Clock() string { return l.At.UTC().Format("15:04:05") }

// LogsDivider is the words of the divider written before l, the day
// (UTC) and the release, when one is: prev is the line written before
// it, nil when none was or a note of the client's was written since.
func LogsDivider(prev *LogsLine, l LogsLine) (termtext.Text, bool) {
	day := l.At.UTC().Format("2006-01-02")
	if prev != nil && prev.At.UTC().Format("2006-01-02") == day && prev.Release == l.Release {
		return termtext.Text{}, false
	}
	return termtext.From(day + " UTC, release " + l.Release.String()), true
}

// LogsDividers is where a layout written as reads bring lines has got
// to, for LogsDivider: the line written last.
type LogsDividers struct{ prev *LogsLine }

// Before is the divider written before l, when one is, and moves on to l.
func (d *LogsDividers) Before(l LogsLine) (termtext.Text, bool) {
	t, ok := LogsDivider(d.prev, l)
	d.prev = &l
	return t, ok
}

// Note is a note of the client's written: the next line has its divider
// again.
func (d *LogsDividers) Note() { d.prev = nil }

// LogsGapSays is the note of a follow's gap (LogsTurn.Gap): none, the
// answer brought no line of app; older, it stopped with older lines
// left (every entry it took was passed over), so not that the journal
// holds none.
func LogsGapSays(app string, none, older bool) string {
	newest := "these are its newest"
	switch {
	case none && older:
		newest = "this read sent no line"
	case none:
		newest = "it now holds no line of " + app
	}
	return "a gap: the box no longer holds the line this follow had read to, so lines since then may be missing; " + newest
}

// LogsOlderSays is the start of the note of a tail's more
// (LogsTurn.Older), n the count asked for: each reader says after it
// what it does about the older lines.
func LogsOlderSays(n string) string {
	return "older lines were not sent: a read sends at most " + n + " lines or 64 KiB of them"
}

// LogsMostLines is the largest count of wire.LogsLines: a tail of it
// reaches furthest back.
func LogsMostLines() string {
	return slices.MaxFunc(wire.LogsLines, func(a, b string) int {
		x, _ := strconv.Atoi(a)
		y, _ := strconv.Atoi(b)
		return x - y
	})
}

// LogsOffSays is the client's sentence for a kept "off": host keeps
// app's output on the box.
func LogsOffSays(host, app string) string {
	return host + " keeps " + app + "'s output on the box (deploy_log_lines 0): it sends no line of it"
}

// LogsNoneSays is the client's sentence for a first read that brings no
// line.
func LogsNoneSays(host, app string) string {
	return host + " holds no line of " + app + " in its journal"
}
