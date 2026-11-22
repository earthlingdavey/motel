package termtext

import (
	"encoding/json"
	"reflect"
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
		// Tabbed and TabbedLines are held the same: UTF-8, no rune From
		// escapes (a tab among them), and From where s holds no tab.
		tabbed := []string{Tabbed(s).String()}
		for _, l := range TabbedLines(s) {
			tabbed = append(tabbed, l.String())
		}
		for _, got := range tabbed {
			if !utf8.ValidString(got) || strings.ContainsFunc(got, unsafeRune) {
				t.Fatalf("Tabbed(%q) or a line of it = %q keeps an unsafe rune, or is not UTF-8", s, got)
			}
		}
		if !strings.Contains(s, "\t") && tabbed[0] != out {
			t.Fatalf("Tabbed(%q) = %q, From %q", s, tabbed[0], out)
		}
	})
}

// JSON keeps the value and writes nothing a terminal acts on: each
// such rune, which JSON allows raw only in a string, becomes its \u
// escape, a byte that is not UTF-8 becomes \ufffd, and a carriage
// return between values is dropped. A body with none is not touched.
func TestJSON(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"nothing to escape", "{\"a\":\"caf\u00e9 \u65e5\u672c \\u001b[2J\",\n\t\"b\":[1,2]}\n", "{\"a\":\"caf\u00e9 \u65e5\u672c \\u001b[2J\",\n\t\"b\":[1,2]}\n"},
		{"a C1 control", "{\"a\":\"x\u009b31mred\"}", `{"a":"x\u009b31mred"}`},
		{"an OSC in C1", "{\"a\":\"\u009d52;c;cHduZWQ=\u009c\"}", `{"a":"\u009d52;c;cHduZWQ=\u009c"}`},
		{"DEL", "{\"a\":\"x\x7fy\"}", `{"a":"x\u007fy"}`},
		{"a bidi override", "{\"a\":\"abc\u202edef\"}", `{"a":"abc\u202edef"}`},
		{"an isolate and a mark", "{\"a\":\"\u2066x\u2069\u200f\u061c\"}", `{"a":"\u2066x\u2069\u200f\u061c"}`},
		{"line and paragraph separators", "{\"a\":\"x\u2028y\u2029\"}", `{"a":"x\u2028y\u2029"}`},
		{"bytes that are not UTF-8", "{\"a\":\"x\xff\xfey\xc2\"}", `{"a":"x\ufffd\ufffdy\ufffd"}`},
		{"a carriage return between values", "{\"a\":1,\r\n\"b\":2}\r", "{\"a\":1,\n\"b\":2}"},
		{"in a key", "{\"a\u202e\":1}", `{"a\u202e":1}`},
	} {
		got := string(JSON([]byte(tc.in)))
		if got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
		for _, r := range got {
			if r == utf8.RuneError || (r != '\n' && r != '\t' && unsafeRune(r)) {
				t.Errorf("%s: the output holds %U: %q", tc.name, r, got)
			}
		}
		// The same value: what a JSON reader makes of each.
		var before, after any
		errIn, errOut := json.Unmarshal([]byte(tc.in), &before), json.Unmarshal([]byte(got), &after)
		if errIn != nil || errOut != nil || !reflect.DeepEqual(before, after) {
			t.Errorf("%s: not the same value: %v (%v), then %v (%v)", tc.name, before, errIn, after, errOut)
		}
	}
	clean := []byte("{\"a\":\"b\"}\n")
	if got := JSON(clean); &got[0] != &clean[0] {
		t.Error("a body with nothing to escape was copied")
	}
	// Not JSON at all (the callers never pass one): still nothing a
	// terminal acts on.
	if got := string(JSON([]byte("\x1b]0;title\x07 \x00"))); got != `\u001b]0;title\u0007 \u0000` {
		t.Errorf("raw controls: %q", got)
	}
}

// TestTabbed: a tab is spaces to the next stop of 8 in the text's own
// columns, each escape counted as its characters; every other control
// is escaped as From escapes it; no tab is written. TabbedLines splits
// as Lines does.
func TestTabbed(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"\t/app/main.go:10 +0x1d", "        /app/main.go:10 +0x1d"},
		{"GET\t/cart\t200", "GET     /cart   200"},
		{"12345678\tx", "12345678        x"},
		{"a\t\tb", "a               b"},
		{"\x1b[31m\tred\r", "\\x1b[31m        red\\x0d"},
		{"日\tx", "日       x"},
		{"\xff\tx", "\ufffd       x"},
		{"no tab \x1b", "no tab \\x1b"},
	} {
		got := Tabbed(c.in).String()
		if got != c.want || strings.ContainsRune(got, '\t') {
			t.Errorf("%q: %q, want %q", c.in, got, c.want)
		}
	}
	got := TabbedLines("a\tb\r\nc\n")
	if len(got) != 2 || got[0].String() != "a       b" || got[1].String() != "c" {
		t.Errorf("TabbedLines: %q", got)
	}
	if From("a\tb").String() != "a\\x09b" {
		t.Errorf("From changed: %q", From("a\tb"))
	}
}
