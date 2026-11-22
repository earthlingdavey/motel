package model

import (
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/termtext"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// TestLogsFollowTake: what each answer means to a follower, and where it
// reads next. A tail's more is older lines no follow reaches; a follow's
// more is lines after the last one sent, read at once; a gap is said
// only of an after that was sent, its lines a fresh tail; a cursor is
// taken; none is the one sent, kept, but after a gap, which tails next.
func TestLogsFollowTake(t *testing.T) {
	for _, c := range []struct {
		name      string
		after     string
		l         wire.Logs
		want      LogsTurn
		nextAfter string
	}{
		{"a tail", "", wire.Logs{Kept: wire.KeptOn, Cursor: "c1"}, LogsTurn{}, "c1"},
		{"a tail, more", "", wire.Logs{Kept: wire.KeptOn, More: true, Cursor: "c1"}, LogsTurn{Older: true}, "c1"},
		{"a tail of none", "", wire.Logs{Kept: wire.KeptOn}, LogsTurn{}, ""},
		{"a follow", "c1", wire.Logs{Kept: wire.KeptOn, Cursor: "c2"}, LogsTurn{}, "c2"},
		{"a follow, more", "c1", wire.Logs{Kept: wire.KeptOn, More: true, Cursor: "c2"}, LogsTurn{Again: true}, "c2"},
		{"a follow of none", "c1", wire.Logs{Kept: wire.KeptOn}, LogsTurn{}, "c1"},
		{"a follow's more with no cursor", "c1", wire.Logs{Kept: wire.KeptOn, More: true}, LogsTurn{}, "c1"},
		{"a follow's more with the cursor sent", "c1", wire.Logs{Kept: wire.KeptOn, More: true, Cursor: "c1"}, LogsTurn{}, "c1"},
		{"a gap", "c1", wire.Logs{Kept: wire.KeptOn, Gap: true, Cursor: "c3"}, LogsTurn{Gap: true}, "c3"},
		{"a gap, more", "c1", wire.Logs{Kept: wire.KeptOn, Gap: true, More: true, Cursor: "c3"}, LogsTurn{Gap: true, Older: true}, "c3"},
		{"a gap, no cursor", "c1", wire.Logs{Kept: wire.KeptOn, Gap: true}, LogsTurn{Gap: true}, ""},
		{"kept off", "c1", wire.Logs{Kept: wire.KeptOff}, LogsTurn{Off: true}, "c1"},
	} {
		f := LogsFollow{after: c.after}
		if got := f.Take(c.l); got != c.want || f.After() != c.nextAfter {
			t.Errorf("%s: %+v, next after %q; want %+v, %q", c.name, got, f.After(), c.want, c.nextAfter)
		}
	}
}

// TestLogsLinesShown: a line's text is made safe, every control and
// colour code an escape but a tab, which is spaces to the next stop of
// 8 in the text's own columns (an escape counted as its characters);
// a release the journal did not keep is said in the client's words.
func TestLogsLinesShown(t *testing.T) {
	at := time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)
	got := LogsLines([]wire.LogLine{{At: at, Release: "v42", Text: "\x1b[31mred\u202e"}, {At: at, Text: "x"}})
	if got[0].Text.String() != `\x1b[31mred\u202e` || got[0].Release.String() != "v42" || !got[0].At.Equal(at) {
		t.Errorf("line 0: %+v", got[0])
	}
	if got[1].Release.String() != LogsNoRelease {
		t.Errorf("line 1's release %q, want %q", got[1].Release, LogsNoRelease)
	}
	for _, c := range []struct{ in, want string }{
		// A Go stack frame.
		{"\t/app/main.go:10 +0x1d", "        /app/main.go:10 +0x1d"},
		// Tab-separated columns line up.
		{"GET\t/cart\t200", "GET     /cart   200"},
		{"POST\t/checkout\t500", "POST    /checkout       500"},
		// A tab at a stop goes to the next; two in a row.
		{"12345678\tx", "12345678        x"},
		{"a\t\tb", "a               b"},
		// An escape beside a tab is still an escape, its characters
		// counted: \x1b[31m is 8 columns.
		{"\x1b[31m\tred\x1b[0m", "\\x1b[31m        red\\x1b[0m"},
		// A wide character counts one column (rune count).
		{"日\tx", "日       x"},
		{"no tab", "no tab"},
	} {
		got := LogsLines([]wire.LogLine{{Text: c.in}})[0].Text.String()
		if got != c.want {
			t.Errorf("%q: %q, want %q", c.in, got, c.want)
		}
		if strings.ContainsRune(got, '\t') {
			t.Errorf("%q: a tab is written", c.in)
		}
	}
}

// TestLogsKeepBytes: lines long as shown are held to LogsKeepBytes, the
// oldest dropped first and counted.
func TestLogsKeepBytes(t *testing.T) {
	d := &logsData{}
	mib := termtext.From(strings.Repeat("x", 1<<20))
	for range 20 {
		d.items = append(d.items, logsItem{line: LogsLine{Text: mib}})
		d.lines++
		d.bytes += 1 << 20
	}
	d.trim()
	if d.lines != 16 || d.dropped != 4 || d.bytes != 16<<20 || d.first != 4 {
		t.Errorf("held %d lines, %d bytes; dropped %d, first %d", d.lines, d.bytes, d.dropped, d.first)
	}
}
