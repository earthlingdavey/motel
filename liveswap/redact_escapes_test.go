package liveswap

import (
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"
)

// runtimeEscapesReadable is how many of the corpus's 494 outputs a door
// sends readable: none of the five realistic values' 190, and 47 of the
// rest, each on a value that is borderline or built to stress the filter
// (a quote or backslash under encoders other than Go's, non-ASCII as
// \uXXXX, a control character as \xXX, deeper nesting, an encoder with
// another habit). The owner accepted them on 2026-11-25
// (DESIGN-liveswap.md, accepted residuals). Pinned, so that a change to
// layer 1's spellings that moves it either way is seen.
const runtimeEscapesReadable = 47

// runtimeEscapesRealistic are the corpus's values a provider or a
// generator issues: a base64 key, a URL with a query, a word with
// digits, hex, and a value holding `=`.
var runtimeEscapesRealistic = map[string]bool{"base64": true, "url": true, "plain": true, "hex": true, "equals": true}

// TestRedactorRuntimeEscapes runs the corpus of pull request #651
// (closed; testdata/escapes/runtime-escapes.tsv, measured there in each
// runtime's official image): 13 made-up values as 38 encoders of seven
// runtimes wrote them, 494 outputs. Each output, in a log line, goes
// through door.text of an app whose env_file holds all 13. A value is
// readable in what the door sends when it is found there as written or
// after one to four levels of backslash escapes are undone.
func TestRedactorRuntimeEscapes(t *testing.T) {
	data, err := os.ReadFile("testdata/escapes/runtime-escapes.tsv")
	if err != nil {
		t.Fatal(err)
	}
	unquote := func(q string) string {
		var s string
		if err := json.Unmarshal([]byte(q), &s); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return s
	}
	type output struct{ value, encoder, written string }
	values := map[string]string{}
	var outputs []output
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		p := strings.Split(line, "\t")
		if len(p) != 3 {
			t.Fatalf("line %q", line)
		}
		if p[0] == "value" {
			values[p[1]] = unquote(p[2])
			continue
		}
		outputs = append(outputs, output{p[0], p[1], unquote(p[2])})
	}
	if len(outputs) != 494 || len(values) != 13 {
		t.Fatalf("%d outputs of %d values, want 494 of 13", len(outputs), len(values))
	}
	var env []string
	for name, v := range values {
		env = append(env, "K_"+strings.ToUpper(name)+"="+v)
	}
	sort.Strings(env) // one order on every run: map order is random
	r := NewRedactor(env)

	var left []string
	for _, o := range outputs {
		v, in := values[o.value], "log: "+o.written+" end"
		if !escapesReadable(in, v) {
			t.Fatalf("%s, %s: %q is not read as the value before any filter: the test is blind", o.value, o.encoder, o.written)
		}
		d := &door{r: r, seen: map[string]bool{}}
		out := string(d.text(in))
		if !escapesReadable(out, v) {
			continue
		}
		left = append(left, o.value+" as "+o.encoder)
		if runtimeEscapesRealistic[o.value] {
			t.Errorf("the realistic value %s as %s wrote it is sent readable: %q", o.value, o.encoder, out)
		}
	}
	if len(left) != runtimeEscapesReadable {
		sort.Strings(left)
		t.Errorf("%d of %d outputs are sent readable, want %d:\n%s", len(left), len(outputs), runtimeEscapesReadable, strings.Join(left, "\n"))
	}
}

// escapesReadable is whether v is in s as written or after one to four
// levels of backslash escapes are undone (#651's measure).
func escapesReadable(s, v string) bool {
	for range 5 {
		if strings.Contains(s, v) {
			return true
		}
		s = undoEscapes(s)
	}
	return false
}

// undoEscapes undoes one level of backslash escapes, as any of the
// corpus's encoders writes them: \uXXXX (a surrogate pair joined),
// \UXXXXXXXX, \xXX, \n \t \r \b \f, and a backslash before any other
// character.
func undoEscapes(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		hex := func(n int) (rune, bool) {
			if i+2+n > len(s) {
				return 0, false
			}
			v, err := strconv.ParseUint(s[i+2:i+2+n], 16, 32)
			return rune(v), err == nil
		}
		switch c := s[i+1]; c {
		case 'u':
			r, ok := hex(4)
			if !ok {
				b.WriteByte('\\')
				continue
			}
			i += 5
			if utf16.IsSurrogate(r) && i+6 < len(s) && s[i+1] == '\\' && s[i+2] == 'u' {
				if v, err := strconv.ParseUint(s[i+3:i+7], 16, 32); err == nil {
					r, i = utf16.DecodeRune(r, rune(v)), i+6
				}
			}
			b.WriteRune(r)
		case 'U':
			if r, ok := hex(8); ok {
				b.WriteRune(r)
				i += 9
			} else {
				b.WriteByte('\\')
			}
		case 'x':
			if r, ok := hex(2); ok {
				b.WriteRune(r)
				i += 3
			} else {
				b.WriteByte('\\')
			}
		default:
			if e, ok := map[byte]byte{'n': '\n', 't': '\t', 'r': '\r', 'b': '\b', 'f': '\f'}[c]; ok {
				c = e
			}
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}
