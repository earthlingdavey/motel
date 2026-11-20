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
			body, _ := json.Marshal(map[string]any{"error": prefix + form + suffix, "versions": []string{"x", "y"}, "status": "succeeded", "phase": "starting", "started_at": time.Unix(1_700_000_000, 0).UTC()})
			js := r.redactJSON(body)
			if !json.Valid([]byte(js)) {
				t.Fatalf("form %q: redactJSON produced invalid JSON: %s", form, js)
			}
			// Strictly: the form is absent from the bytes written, in
			// any encoding, with two exceptions — the report field's own
			// fixed name, which is in every such response and so reveals
			// nothing, and (rule 3) a value that is a bare word of the
			// outcome vocabulary, where it stands as the body's own
			// outcome — and (rule 7) the body's time, whole, which a
			// form may be a piece of — and the key is reported unless
			// the body had to fall back to the one marker that can carry
			// nothing.
			rest := strings.Replace(js, `"redacted_env":`, "", 1)
			if outcomeStatuses[value] || outcomePhases[value] {
				rest = strings.NewReplacer(`"status":"succeeded"`, "", `"phase":"starting"`, "").Replace(rest)
			}
			for _, f := range []string{string(mustQuote(t, form)), form} {
				if outsideTime(rest, f, `"started_at":"`, "2023-11-14T22:13:20Z") {
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

// outsideTime reports whether f occurs in s anywhere but wholly inside
// the text of the time tm where it stands as the value of key (rule 7):
// the pair as written, which no string's text can spell, its quotes
// being escaped there.
func outsideTime(s, f, key, tm string) bool {
	lo, hi := -1, -1
	if i := strings.Index(s, key+tm+`"`); i >= 0 {
		lo, hi = i+len(key), i+len(key)+len(tm)
	}
	for from := 0; ; {
		i := strings.Index(s[from:], f)
		if i < 0 {
			return false
		}
		start := from + i
		if start < lo || start+len(f) > hi {
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
