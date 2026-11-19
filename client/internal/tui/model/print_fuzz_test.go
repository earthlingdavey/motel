package model_test

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/smallhoursorg/motel/client/internal/client"
	"github.com/smallhoursorg/motel/client/internal/termtext"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// shellWords is line read into words as a POSIX shell reads a simple
// command (XCU 2.2 and 2.3): words parted by unquoted blanks (a space,
// a tab); a single-quoted run taken as it is, to the next '; a
// backslash outside quotes taking the next character as it is. Unquoted,
// only a letter, a digit or one of @ % + = : , . / _ - stands for
// itself: anything else a shell may read otherwise (an operator, a
// quote, an expansion, a glob, a line feed, a byte a locale may count a
// blank) is refused, and so is a word beginning with # (a comment), ~
// (the home directory) or = (zsh's =cmd), and a quote left open. It is
// the shell this side holds the printed line to; TestShellWordsIsSh
// holds it to a real sh on lines of every shape it takes.
func shellWords(line string) ([]string, error) {
	var words []string
	var w strings.Builder
	in := false // a word has begun, an empty quoted one too
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == ' ' || c == '\t':
			if in {
				words, in = append(words, w.String()), false
				w.Reset()
			}
		case c == '\'':
			end := strings.IndexByte(line[i+1:], '\'')
			if end < 0 {
				return nil, errors.New("a quote left open")
			}
			w.WriteString(line[i+1 : i+1+end])
			i, in = i+1+end, true
		case c == '\\':
			if i+1 == len(line) || line[i+1] == '\n' {
				return nil, errors.New("a backslash at the end of a line")
			}
			i++
			w.WriteByte(line[i])
			in = true
		case !in && (c == '#' || c == '~' || c == '='):
			return nil, errors.New("a word beginning with " + string(c))
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', strings.IndexByte("@%+=:,./_-", c) >= 0:
			w.WriteByte(c)
			in = true
		default:
			return nil, errors.New("an unquoted " + string(rune(c)))
		}
	}
	if in {
		words = append(words, w.String())
	}
	return words, nil
}

// TestShellWordsIsSh: what shellWords reads of a line, a real sh reads
// too, for lines of each shape it takes (bare words, quoted runs, an
// empty word, a quote written as ShellQuote writes one (the quotation
// closed, a backslash and the quote, and opened again), a backslash, a
// tab) and printed commands with hostile key paths; and it refuses
// lines a shell may read otherwise. Skipped where no sh is installed.
func TestShellWordsIsSh(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("NOT RUN: no sh")
	}
	// motel is a function of the shell's own, which prints each argument
	// it got: no file is written, so a temporary directory that cannot
	// run one does not matter.
	const motel = "motel() { for a in \"$@\"; do printf '%s\\0' \"$a\"; done; }\n"
	lines := []string{
		`motel a b`, `motel 'a b' ''`, `motel 'it'\''s' x`, `motel a\ b`, "motel\ta\t'b\tc'",
		`motel '$(id)' '~' '#x' '=ls' 'a*?[' '"q"' '\' 'é'`, `motel a=b c:d %1 @x +y`, `motel a'b'c`,
	}
	for _, key := range []string{"~/.ssh/id_ed25519.pub", "~/.ssh/my key", "$(id)`id` \"q\" '", "back\\slash*?[", "'", "''", "\U0001f44d\U0001f3fd"} {
		s := &model.Session{Host: "deploy.example.com", Addr: "deploy.example.com:8443", Key: model.Key{Source: "--key", Shown: key}, Now: func() time.Time { return t0 }, Engine: model.NewEngine(t0)}
		line, _ := s.Command(model.View{Screen: model.ScreenRecord, App: "blog", Version: "-v1"})
		lines = append(lines, line)
	}
	for _, line := range lines {
		got, err := shellWords(line)
		if err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		out, err := exec.Command(sh, "-c", motel+line).Output()
		if err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		var want []string
		if len(out) > 0 {
			want = strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
		}
		if len(got) == 0 || got[0] != "motel" || !slices.Equal(got[1:], want) {
			t.Errorf("%q: shellWords reads %q, sh motel %q", line, got, want)
		}
	}
	for _, line := range []string{`motel $HOME`, `motel ~`, `motel a*`, `motel a;b`, `motel "a b"`, `motel 'a`, `motel #x`, "motel a\nb", `motel a\`, "motel \u00a0"} {
		if words, err := shellWords(line); err == nil {
			t.Errorf("%q is read as %q, though a shell may read it otherwise", line, words)
		}
	}
}

// FuzzPrintedCommand: for any app, version, box (host and port), key
// path and key source the client accepts (client.ParseApp's name,
// client.ResolveBox's --box, the key's path as the client shows it,
// made safe by termtext), every screen's printed command, read back by
// a POSIX shell (shellWords), is the arguments it was built from: the
// command's words, --box naming the box and the port when it is not
// 443, --key the key's path when the person named it, -- and the app
// and version. That line is what P prints and c shows first; c's
// second line is the same with --json among the flags. A version
// outside the grammar has no line. Seen to fail: with ShellQuote
// leaving a leading = bare (the key =ls: a word beginning with =),
// writing a ' inside quotes as it is (a key holding one: a quote left
// open), and leaving a space bare (the key /home/k/my key.pub: two
// words for one). testdata/fuzz/FuzzPrintedCommand holds the key path
// "--", which a run found the test itself got wrong: it put
// --json before the first "--" of the line, the key's.
func FuzzPrintedCommand(f *testing.F) {
	for _, c := range [][3]string{
		{"blog", "7c1e55d0a913", "~/.ssh/id_ed25519.pub"},
		{"blog", "-v1", "~/.ssh/my key"},
		{"blog", "-v1", "/home/k/my key.pub"},
		{"my-app-2", "--key", "/tmp/a;rm -rf x"},
		{"blog", "v1.x", "$(id)`id` \"q\" '"},
		{"blog", "v2", "back\\slash*?["},
		{"blog", "v1'x", "~root"},
		{"blog", "_", "=ls"},
		{"blog", "v3", "~/keys/\U0001f44d\U0001f3fd/\u034f\u0600.pub"},
	} {
		f.Add(c[0], c[1], "deploy.example.com", c[2], byte(0))
		f.Add(c[0], c[1], "deploy.example.com:8443", c[2], byte(1))
		f.Add(c[0], c[1], "box-1.example.com:443", c[2], byte(2))
	}
	// A key path whose line, each ' written as four characters, is longer
	// than a fixed window of 1<<20 cells (Copilot's on #591).
	f.Add("blog", "v1", "deploy.example.com", strings.Repeat("'", 262144), byte(0))
	f.Fuzz(func(t *testing.T, app, version, box, key string, source byte) {
		if a, err := client.ParseApp(app); err != nil || a.Name != app {
			return
		}
		if box == "" {
			return // ResolveBox would look further: MOTEL_BOX, MOTEL_URL, the repository
		}
		b, err := client.ResolveBox(context.Background(), client.BoxLookup{Flag: box})
		if err != nil {
			return
		}
		src := []string{"--key", "MOTEL_KEY", "the default file"}[int(source)%3]
		shown := termtext.From(key).String()
		s := &model.Session{Host: b.Host, Addr: b.Addr(), Key: model.Key{Source: src, Shown: shown}, Now: func() time.Time { return t0 }, Engine: model.NewEngine(t0)}
		for _, v := range []model.View{
			{Screen: model.ScreenApps}, {Screen: model.ScreenApp, App: app}, {Screen: model.ScreenRecord, App: app, Version: version},
			{Screen: model.ScreenMe}, {Screen: model.ScreenBox},
		} {
			line, ok := s.Command(v)
			if v.Screen == model.ScreenRecord && ok != model.ValidVersion(version) {
				t.Fatalf("version %q: a command %v, the grammar says %v", version, ok, model.ValidVersion(version))
			}
			if !ok {
				continue
			}
			args, _ := s.CommandArgs(v)
			if want := built(v, b, src, shown, false); !slices.Equal(args, want) {
				t.Fatalf("the command is built as %q, want %q", args, want)
			}
			got, err := shellWords(line)
			if err != nil || !slices.Equal(got, args) {
				t.Fatalf("%q is read back as %q (%v), built from %q", line, got, err, args)
			}
			if p, ok := s.Printable(v, 1); !ok || string(p) != line+"\n" {
				t.Fatalf("P prints %q, not %q", p, line+"\n")
			}
			// A window wide enough that no line c shows is cut, however
			// long the key's path: cutting is FuzzOverlay's. runes gives a
			// rune a cell, so the line's runes, and room for the indent,
			// --json and the apps row's "-- <app>", are enough.
			var shownLines []string
			for _, l := range s.Overlay(v, false, utf8.RuneCountInString(line)+64, runes, asIs) {
				if t := l.Text(); strings.HasPrefix(t, "  motel ") {
					shownLines = append(shownLines, strings.TrimPrefix(t, "  "))
				}
			}
			if len(shownLines) < 2 || shownLines[0] != line {
				t.Fatalf("c shows %q, not %q first", shownLines, line)
			}
			if got, err := shellWords(shownLines[1]); err != nil || !slices.Equal(got, built(v, b, src, shown, true)) {
				t.Fatalf("c's --json line %q is read back as %q (%v)", shownLines[1], got, err)
			}
		}
	})
}

// built is the command line for v as its arguments: motel, the
// command's words, --box and the box's address, --key and the key's
// path exactly when the person named it, --json when json, then -- and
// v's app and version when the command takes them.
func built(v model.View, b client.Box, src, shown string, json bool) []string {
	a, _ := model.ActionFor(map[model.Screen]model.Kind{model.ScreenApps: model.KApps, model.ScreenApp: model.KStatus, model.ScreenRecord: model.KDeploy, model.ScreenMe: model.KWhoAmI, model.ScreenBox: model.KBox}[v.Screen])
	want := []string{"motel"}
	var pos []string
	for _, w := range strings.Fields(a.Command) {
		switch w {
		case "<app>":
			pos = append(pos, v.App)
		case "<version>":
			pos = append(pos, v.Version)
		default:
			want = append(want, w)
		}
	}
	want = append(want, "--box", b.Addr())
	if src == "--key" || src == "MOTEL_KEY" {
		want = append(want, "--key", shown)
	}
	if json {
		want = append(want, "--json")
	}
	if len(pos) > 0 {
		want = append(append(want, "--"), pos...)
	}
	return want
}
