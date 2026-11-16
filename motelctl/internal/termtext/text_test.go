package termtext

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFrom(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"plain words, digits 0-9 and é", "plain words, digits 0-9 and é"},
		{"tab\there", `tab\x09here`},
		{"line\nbreak", `line\x0abreak`},
		{"\x1b]52;c;aGVsbG8=\x07", `\x1b]52;c;aGVsbG8=\x07`},
		{"\x1b[2J\x1b[H", `\x1b[2J\x1b[H`},
		{"del\x7f", `del\x7f`},
		{"c1 \u009b31m", `c1 \u009b31m`},
		{"next line \u0085", `next line \u0085`},
		{"bidi \u202eevil", "bidi \\u202eevil"},
		{"isolate \u2066x\u2069", "isolate \\u2066x\\u2069"},
		{"deprecated formats \u206a\u206b\u206c\u206d\u206e\u206f", "deprecated formats \\u206a\\u206b\\u206c\\u206d\\u206e\\u206f"},
		{"sep \u2028 \u2029", "sep \\u2028 \\u2029"},
		{"marks \u061c \u200e \u200f", "marks \\u061c \\u200e \\u200f"},
		{"not bidi: zero width \u200b, joiner \u200d", "not bidi: zero width \u200b, joiner \u200d"},
		{"bad \xff utf8", "bad \uFFFD utf8"},
		{"a cut rune \xe2\x82", "a cut rune \uFFFD\uFFFD"},
		{"a real \uFFFD stays", "a real \uFFFD stays"},
		{"[red]tags[-] and [\"region\"]", "[red]tags[-] and [\"region\"]"},
	}
	for _, c := range cases {
		if got := From(c.in).String(); got != c.want {
			t.Errorf("From(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLines(t *testing.T) {
	got := Lines("one\r\ntwo \x1b[31mred\nthree\n")
	want := []string{"one", `two \x1b[31mred`, "three"}
	if len(got) != len(want) {
		t.Fatalf("Lines = %q, want %q", got, want)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
	if Lines("") != nil {
		t.Error("Lines of the empty string is no lines")
	}
}

func FuzzFrom(f *testing.F) {
	for _, s := range []string{"", "ok", "\x1b]8;;http://x\x1b\\y\x1b]8;;\x1b\\", "\u202e", "\xff\xfe", "\u0085\u2028"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out := From(s).String()
		if !utf8.ValidString(out) {
			t.Fatalf("From(%q) is not UTF-8: %q", s, out)
		}
		for _, r := range out {
			if unsafeRune(r) {
				t.Fatalf("From(%q) = %q keeps %U", s, out, r)
			}
		}
		// Sanitising twice changes nothing: an escape is printable.
		if again := From(out).String(); again != out {
			t.Fatalf("From is not idempotent on %q: %q, then %q", s, out, again)
		}
		// What needs no escape is kept as it is.
		if !strings.ContainsFunc(s, func(r rune) bool { return r == utf8.RuneError || unsafeRune(r) }) && out != s {
			t.Fatalf("From(%q) = %q changed a clean string", s, out)
		}
	})
}
