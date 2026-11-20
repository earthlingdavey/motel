// Fuzz target for the response filter: whatever an env_file value is,
// and whatever surrounds it, none of the forms the filter promises to
// catch (as written, JSON-escaped, base64, hex, URL-escaped) survives
// in the output, and the key it belonged to is reported. The corpus
// test (TestRedactorCorpus) holds the other direction — what must
// survive; this one holds only the leak side.
package liveswap

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
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
		{`00.25,"started`, "", ""},
		{`"],"seconds":1700`, "", ""},
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
			// value in a string, escaped once by the encoder: a valid body
			// in, a valid body out, the form absent, the key reported,
			// whether the body was filtered or withheld. A form the
			// encoder would escape again (the JSON-escaped form itself)
			// is an encoding outside the list and is not asserted.
			if string(mustQuote(t, form)) != form && form != value {
				continue
			}
			body, _ := json.Marshal(map[string]any{"error": prefix + form + suffix, "versions": []string{"x", "y"}, "status": "succeeded", "phase": "starting", "started_at": time.Unix(1_700_000_000, 0).UTC(), "seconds": 1700000000.25})
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
			// The hoisted field whole: its brackets beside a held token
			// are the body's own JSON, as rule 7's hoist residual says.
			rest := redactedEnvField.ReplaceAllString(js, "\x00")
			if outcomeStatuses[value] || outcomePhases[value] {
				rest = strings.NewReplacer(`"status":"succeeded"`, "", `"phase":"starting"`, "").Replace(rest)
			}
			for _, f := range []string{string(mustQuote(t, form)), form} {
				if outsideHeld(rest, f, [3]string{`"started_at":"`, "2023-11-14T22:13:20Z", `"`}, [3]string{`"seconds":`, "1700000000.25", `,`}) {
					t.Fatalf("form %q: survived in JSON body %s", form, js)
				}
			}
			if !strings.HasPrefix(js, `{"error":"response withheld`) && js != `{"error":"[#0]"}` && (!strings.Contains(js, `"status":"succeeded"`) || !strings.Contains(js, `"phase":"starting"`)) {
				t.Fatalf("form %q: the outcome was rewritten in %s", form, js)
			}
			if !strings.Contains(js, `"redacted_env":[`) && js != `{"error":"[#0]"}` {
				t.Fatalf("form %q: keys not reported in %s", form, js)
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

// redactedEnvField is the hoisted report field, its key names with it.
var redactedEnvField = regexp.MustCompile(`"redacted_env":\[[^\]]*\]`)

func mustQuote(t *testing.T, s string) []byte {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return b[1 : len(b)-1]
}
