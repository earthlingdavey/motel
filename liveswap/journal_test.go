package liveswap

import (
	"strings"
	"testing"
)

// Journal cursors of the form journalctl writes, every one made up.
const (
	testCursor   = "s=0123456789abcdef0123456789abcdef;i=826;b=fedcba9876543210fedcba9876543210;m=53520c3bfb;t=660ba6e64c877;x=9edf869812ed4251"
	widestCursor = "s=ffffffffffffffffffffffffffffffff;i=ffffffffffffffff;b=ffffffffffffffffffffffffffffffff;m=ffffffffffffffff;t=ffffffffffffffff;x=ffffffffffffffff"
	zeroCursor   = "s=00000000000000000000000000000000;i=0;b=00000000000000000000000000000000;m=0;t=0;x=0"
)

// cursorForms are values journalctl takes as a cursor, or nearly, that
// are not a cursor as it writes one: a piece of one, its fields
// reordered or in upper case, its numbers padded or prefixed, more than
// it holds. Each is the 422 on a row that takes a cursor.
var cursorForms = map[string]string{
	"t= alone":               "t=660ba6e64c877",
	"s= and i= alone":        "s=0123456789abcdef0123456789abcdef;i=826",
	"b= and m= alone":        "b=fedcba9876543210fedcba9876543210;m=53520c3bfb",
	"five fields":            strings.TrimSuffix(testCursor, ";x=9edf869812ed4251"),
	"reordered":              "t=660ba6e64c877;s=0123456789abcdef0123456789abcdef;i=826;b=fedcba9876543210fedcba9876543210;m=53520c3bfb;x=9edf869812ed4251",
	"upper-case hex":         strings.Replace(testCursor, "0123456789abcdef0123456789abcdef", "0123456789ABCDEF0123456789ABCDEF", 1),
	"upper-case letters":     strings.Replace(testCursor, "s=", "S=", 1),
	"0x":                     strings.Replace(testCursor, "i=826", "i=0x826", 1),
	"a leading zero":         strings.Replace(testCursor, "i=826", "i=0826", 1),
	"a space":                strings.Replace(testCursor, "i=826", "i= 826", 1),
	"a sign":                 strings.Replace(testCursor, "i=826", "i=-1", 1),
	"trailing garbage":       strings.Replace(testCursor, "i=826", "i=826zz", 1),
	"17 digits":              strings.Replace(testCursor, "i=826", "i=10000000000000000", 1),
	"33 digits of id":        strings.Replace(testCursor, "s=0", "s=00", 1),
	"UUID form":              strings.Replace(testCursor, "0123456789abcdef0123456789abcdef", "01234567-89ab-cdef-0123-456789abcdef", 1),
	"an unknown field":       testCursor + ";q=1",
	"a trailing ;":           testCursor + ";",
	"a leading ;":            ";" + testCursor,
	"an empty field":         strings.Replace(testCursor, ";i=", ";;i=", 1),
	"s==":                    strings.Replace(testCursor, "s=", "s==", 1),
	"spaces round a ;":       strings.Replace(testCursor, ";i=", " ; i=", 1),
	"a trailing newline":     testCursor + "\n",
	"4 KiB":                  testCursor + strings.Repeat(";x=1", 1024),
	"a leading -":            "-" + testCursor,
	"a cursor and an option": testCursor + " --since=2000-01-01",
}

// canonicalCursor is whether s is a cursor as journalctl writes one,
// said apart from journalCursor's pattern: six fields split on ";",
// named s i b m t x in that order, the ids 32 lower-case hex digits,
// the numbers 1 to 16 with no leading zero but a lone 0.
func canonicalCursor(s string) bool {
	fields := strings.Split(s, ";")
	if len(fields) != 6 {
		return false
	}
	hex := func(v string) bool {
		return strings.Trim(v, "0123456789abcdef") == ""
	}
	for i, name := range []string{"s", "i", "b", "m", "t", "x"} {
		v, ok := strings.CutPrefix(fields[i], name+"=")
		if !ok || !hex(v) {
			return false
		}
		switch name {
		case "s", "b":
			if len(v) != 32 {
				return false
			}
		default:
			if len(v) < 1 || len(v) > 16 || (len(v) > 1 && v[0] == '0') {
				return false
			}
		}
	}
	return true
}

// TestJournalCursorGrammar: journalCursor takes a cursor as journalctl
// writes one (a typical one, one of zeros, the widest at 145 bytes),
// refuses every other form journalctl reads as a cursor, and at one
// byte's change from a cursor takes exactly what canonicalCursor says
// is one still; every value it takes is a single argument that begins
// with no "-" and holds only the bytes a cursor is written in.
func TestJournalCursorGrammar(t *testing.T) {
	for _, c := range []string{testCursor, widestCursor, zeroCursor} {
		if !journalCursor.re.MatchString(c) || !canonicalCursor(c) {
			t.Errorf("refused %q", c)
		}
	}
	if len(widestCursor) != 145 {
		t.Errorf("the widest cursor is %d bytes, want 145", len(widestCursor))
	}
	for name, form := range cursorForms {
		if journalCursor.re.MatchString(form) || canonicalCursor(form) {
			t.Errorf("%s: %.60q taken", name, form)
		}
	}
	taken := 0
	for _, from := range []string{testCursor, widestCursor, zeroCursor} {
		for i := range len(from) {
			for b := range 256 {
				m := []byte(from)
				m[i] = byte(b)
				s := string(m)
				got := journalCursor.re.MatchString(s)
				if got != canonicalCursor(s) {
					t.Fatalf("%q: the grammar says %v, canonicalCursor %v", s, got, !got)
				}
				if !got {
					continue
				}
				taken++
				if strings.HasPrefix(s, "-") || strings.Trim(s, "0123456789abcdef;=imstx") != "" || len(s) > 145 {
					t.Fatalf("%q is taken and is not a plain argument", s)
				}
			}
		}
	}
	if taken == 0 {
		t.Fatal("no one-byte change of a cursor was taken: the test checks nothing")
	}
}
