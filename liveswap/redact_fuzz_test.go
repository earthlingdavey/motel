// Fuzz target for the filter at the doors: whatever an env_file value
// is, and whatever text of the app's surrounds it, none of the forms the
// filter promises to catch (as written, JSON-escaped, base64, hex,
// URL-escaped) survives in the text a door lets through, nor in the
// answer that carries it once marshalled, and the key it belonged to is
// named. The corpus test (TestRedactorCorpus) holds the other direction
// — what must survive; this one holds only the leak side.
package liveswap

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/smallhoursorg/motel/liveswap/wire"
)

func FuzzDoorText(f *testing.F) {
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
		{"succeeded", `"status":"`, `"`},
		{`20Z","error":"`, "", ""},
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
			// No exemption, for a word of motel's vocabulary or for a
			// value that is a substring of the usual marker ("redacted",
			// say): the marker is chosen per key so that it never
			// contains a form of the value.
			d := &door{r: r, seen: map[string]bool{}}
			text := d.text(prefix + form + suffix)
			if strings.Contains(string(text), form) {
				t.Fatalf("form %q of %q survived in %q", form, value, text)
			}
			if keys := d.keys(); len(keys) != 1 || keys[0] != "SECRET" {
				t.Fatalf("form %q: keys = %v", form, keys)
			}
			// Marshalled in the answer that carries it: the JSON encoder
			// escapes the text once, which makes no form of the value
			// from text that holds none.
			body, err := json.Marshal(wire.DeployDetail{LogTail: []wire.AppText{text}})
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range []string{form, string(mustQuote(t, form))} {
				if f != "" && strings.Contains(string(body), f) && !strings.Contains(string(text), f) {
					t.Fatalf("form %q: the encoder made it in %s", f, body)
				}
			}
		}
	})
}

func mustQuote(t *testing.T, s string) []byte {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return b[1 : len(b)-1]
}
