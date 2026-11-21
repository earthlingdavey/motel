// Fuzz target for the response filter: whatever an env_file value is,
// and whatever surrounds it, none of the forms the filter promises to
// catch (as written, base64, hex, URL-escaped, and each in a backslash
// escaping: JSON's, PHP's, .NET's, a quoting function's, twice over)
// survives in the output, and the key it belonged to is reported. The corpus
// test (TestRedactorCorpus) holds the other direction — what must
// survive; this one holds only the leak side.
package liveswap

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

func FuzzRedactor(f *testing.F) {
	for _, s := range [][3]string{
		{"hunter2hunter2", "dial ", " failed"},
		{`p@ss"w\ord-12345`, `{"error":"`, `"}`},
		{"k3J9xQ2mZ8pL0vT7wR4nY6bH1", "", ""},
		{"redacted", "a ", " b"},
		{"[redacted:X]", "", ""},
		{"with space in it", "x", "y"},
		{"ünïcödé-secret", "<", ">"},
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "aaaa", "aaaa"},
		{"12345678", "00", "00"},
		{"2023-11-14", "on ", ""},
		{`20Z","error":"`, "", ""},
		{`"2023-11-14`, "", ""},
		{"17000000", "", ""},
		{`00.25,"error`, "", ""},
	} {
		f.Add(s[0], s[1], s[2])
	}
	f.Fuzz(func(t *testing.T, value, prefix, suffix string) {
		// Below the floor, or not a value an env_file line can carry.
		// Invalid UTF-8 is out too: a stray lead byte at the value's end
		// and a stray continuation byte after it fuse into one character
		// once placed side by side, which no encoding of the value alone
		// can match — an artefact of the harness, not a form the filter
		// promises.
		if len(value) < secretMinLen || strings.ContainsAny(value, "\x00\n=") || !utf8.ValidString(value) {
			return
		}
		r := NewRedactor([]string{"SECRET=" + value}, nil)
		forms := []string{value, string(mustQuote(t, value)), base64.StdEncoding.EncodeToString([]byte(value)),
			base64.RawStdEncoding.EncodeToString([]byte(value)), base64.URLEncoding.EncodeToString([]byte(value)),
			base64.RawURLEncoding.EncodeToString([]byte(value)), hex.EncodeToString([]byte(value)),
			url.QueryEscape(value), url.PathEscape(value)}
		if escapables(value) <= escapeMax { // past it, as written and as encoding/json escapes it alone
			forms = append(forms, phpEscape(value), dotnetEscape(value), reprEscape(value),
				string(mustQuote(t, string(mustQuote(t, value)))), phpEscape(phpEscape(value)))
		}
		for _, form := range forms {
			// The one exemption is rule 3's: a value that is a word of
			// the outcome vocabulary stands where the text makes it an
			// outcome pair. No exemption for a value that is a substring
			// of the usual marker ("redacted", say): the marker is chosen
			// per key so that it never contains a form of the value.
			if (outcomeStatuses[value] || outcomePhases[value]) && outcomePairRe.MatchString(prefix+form+suffix) {
				continue
			}
			out, keys := r.Redact(prefix + form + suffix)
			if strings.Contains(out, form) {
				t.Fatalf("form %q of %q survived in %q", form, value, out)
			}
			if len(keys) == 0 || keys[0] != "SECRET" {
				t.Fatalf("form %q: keys = %v", form, keys)
			}
			// And through the JSON path, as a real body carries it — the
			// text in a string, escaped once more by the encoder: a valid
			// body in, a valid body out, the form absent in either
			// spelling, the key reported, whether the body was filtered or
			// withheld. A value past escapeMax has no pattern: an
			// encoding/json form escaped again is not read.
			if escapables(value) > escapeMax && string(mustQuote(t, form)) != form && form != value {
				continue
			}
			body, _ := json.Marshal(map[string]any{"error": prefix + form + suffix, "versions": []string{"x", "y"}, "status": "succeeded", "phase": "starting", "at": time.Unix(1_700_000_000, 0).UTC(), "count": 1700000000.25})
			js := r.redactJSON(body)
			if !json.Valid([]byte(js)) {
				t.Fatalf("form %q: redactJSON produced invalid JSON: %s", form, js)
			}
			// Strictly: the form is absent from the bytes written, in
			// any encoding, with two exceptions — the report field's own
			// fixed name, which is in every such response and so reveals
			// nothing, and (rule 3) a value that is a bare word of the
			// outcome vocabulary, where it stands as the body's own
			// outcome — and (rule 7) the body's time and number, whole, which a
			// form may be a piece of — and the key is reported unless
			// the body had to fall back to the one marker that can carry
			// nothing.
			// The held tokens' keys sort before the hoisted field, so
			// the hoist sets no held token beside other JSON here (rule
			// 7's hoist residual).
			rest := strings.Replace(js, `"redacted_env":`, "", 1)
			if outcomeStatuses[value] || outcomePhases[value] {
				rest = strings.NewReplacer(`"status":"succeeded"`, "", `"phase":"starting"`, "").Replace(rest)
			}
			for _, f := range []string{string(mustQuote(t, form)), form} {
				if outsideHeld(rest, f, [3]string{`"at":"`, "2023-11-14T22:13:20Z", `"`}, [3]string{`"count":`, "1700000000.25", `,`}) {
					t.Fatalf("form %q: survived in JSON body %s", form, js)
				}
			}
			if !strings.HasPrefix(js, `{"error":"response withheld`) && js != `{"error":"[#0]"}` && (!strings.Contains(js, `"status":"succeeded"`) || !strings.Contains(js, `"phase":"starting"`)) {
				t.Fatalf("form %q: the outcome was rewritten in %s", form, js)
			}
			if !strings.Contains(js, `"redacted_env":[`) && js != `{"error":"[#0]"}` {
				t.Fatalf("form %q: keys not reported in %s", form, js)
			}
			// And a body whose only text is the affixes, so that a
			// form that is a piece of the number (a seed of digits) is
			// held there whole with no marker of it in the body.
			lone, _ := json.Marshal(map[string]any{"error": prefix + suffix, "at": time.Unix(1_700_000_000, 0).UTC(), "count": 1700000000.25})
			lj := r.redactJSON(lone)
			if !json.Valid([]byte(lj)) {
				t.Fatalf("form %q: redactJSON produced invalid JSON: %s", form, lj)
			}
			for _, f := range []string{string(mustQuote(t, form)), form} {
				if outsideHeld(strings.Replace(lj, `"redacted_env":`, "", 1), f, [3]string{`"at":"`, "2023-11-14T22:13:20Z", `"`}, [3]string{`"count":`, "1700000000.25", `,`}) {
					t.Fatalf("form %q: survived in JSON body %s", form, lj)
				}
			}
		}
	})
}

// outsideHeld reports whether f occurs in s anywhere but wholly inside
// one of the held tokens (rule 7): each given as the text before it,
// itself and the text after, the pair as written, which no string's text
// can spell, its quotes being escaped there; a time's text without its
// quotes.
func outsideHeld(s, f string, held ...[3]string) bool {
	var spans [][2]int
	for _, h := range held {
		if i := strings.Index(s, h[0]+h[1]+h[2]); i >= 0 {
			spans = append(spans, [2]int{i + len(h[0]), i + len(h[0]) + len(h[1])})
		}
	}
	for from := 0; ; {
		i := strings.Index(s[from:], f)
		if i < 0 {
			return false
		}
		start, inside := from+i, false
		for _, sp := range spans {
			inside = inside || start >= sp[0] && start+len(f) <= sp[1]
		}
		if !inside {
			return true
		}
		from = start + 1
	}
}

func mustQuote(t *testing.T, s string) []byte {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return b[1 : len(b)-1]
}

// FuzzHeuristicsKeepJSON: a filter that knows no value never withholds a
// body for its structure. Only a known value can overlap a body's JSON,
// so a body withheld by such a filter is one a shape or entropy rule
// broke: a match that took half of an escape the body's encoding wrote
// in the app's text (a \" after a URL, the \t before a token). The
// seeds are each shape the rules match, beside each escape.
func FuzzHeuristicsKeepJSON(f *testing.F) {
	for _, s := range []string{
		`Get "https://api.example.com/v1?key=abc": dial tcp`,
		`rejected "DB_PASSWORD=hunter2hunter" at login`,
		`{"db_password":"hunter2hunter\\"}`,
		"https://x.example/a?key=abc\\",
		"id\tk3J9xQ2mZ8pL0vT7wR4nY6bH1",             // gitleaks:allow — made up for this test
		"one\nk3J9xQ2mZ8pL0vT7wR4nY6bH1 two",        // gitleaks:allow — made up for this test
		"a&k3J9xQ2mZ8pL0vT7wR4nY6bH1<b>",            // gitleaks:allow — made up for this test
		"\"Bearer AbCdEfGhIjKlMnOpQrStUvWxYz0123\"", // gitleaks:allow — made up for this test
		"\"postgres://app:hunter2@db/app\"",
		"-----BEGIN PRIVATE KEY-----\nMIIE\n-----END PRIVATE KEY-----\"",
		"\"Basic dToxMjM0NTY3OA==\"",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, line string) {
		if !utf8.ValidString(line) {
			return // encoding/json writes U+FFFD for it: not the app's bytes
		}
		body, err := json.Marshal(map[string]any{"status": "failed", "log_tail": []string{line, "x"}})
		if err != nil {
			t.Fatal(err)
		}
		// Withheld is structural: a filtered body keeps its log_tail, and
		// every withheld body is an error alone (the line may say
		// "response withheld" itself).
		js := (*Redactor)(nil).redactJSON(body)
		var got map[string]json.RawMessage
		if json.Unmarshal([]byte(js), &got) != nil || got["log_tail"] == nil {
			t.Fatalf("a filter that knows no value withheld %q: %s", line, js)
		}
	})
}

// phpEscape is a value as PHP's json_encode writes it by default: / as
// \/, every rune past ASCII as \uXXXX in lower case (a surrogate pair
// above the BMP), and JSON's own escapes.
func phpEscape(s string) string {
	return escapeEach(s, func(c rune) bool { return c >= 0x80 }, "\\u%04x", map[rune]string{'/': `\/`})
}

// dotnetEscape is a value as .NET's System.Text.Json writes it by
// default: < > & ' + ` " and every rune past ASCII as \uXXXX in upper
// case.
func dotnetEscape(s string) string {
	return escapeEach(s, func(c rune) bool { return c >= 0x7f || strings.ContainsRune("<>&'+`\"", c) }, "\\u%04X", nil)
}

// reprEscape is a value as Python's repr and Node's util.inspect write
// it in single quotes: ' and \ behind a backslash, a control character
// as \xXX.
func reprEscape(s string) string {
	var b strings.Builder
	for _, c := range s {
		switch {
		case c == '\'' || c == '\\':
			b.WriteString(`\` + string(c))
		case c < 0x20 || c == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, c)
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}

// escapeEach writes s with " and \ escaped, a control character as
// \u00XX, each rune esc says as format (two of them above the BMP), and
// the runes of also as given.
func escapeEach(s string, esc func(rune) bool, format string, also map[rune]string) string {
	var b strings.Builder
	for _, c := range s {
		switch {
		case also[c] != "":
			b.WriteString(also[c])
		case esc(c) && c >= 0x10000:
			hi, lo := utf16.EncodeRune(c)
			fmt.Fprintf(&b, format+format, hi, lo)
		case esc(c):
			fmt.Fprintf(&b, format, c)
		case c == '"' || c == '\\':
			b.WriteString(`\` + string(c))
		case c < 0x20:
			fmt.Fprintf(&b, `\u%04x`, c)
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}
