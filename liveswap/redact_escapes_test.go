// Layer 1 reads a known value in any backslash escaping: the outputs of
// the runtimes' own encoders (testdata/escapes/runtime-escapes.tsv), the
// edges of the pattern that reads them, and what it costs a body.
package liveswap

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"
)

// undoEscapes undoes one level of the backslash escapes the runtimes
// write, for a test to ask whether a value can still be read in a text.
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

// readable is whether v can be read in s at any of four levels of
// escaping.
func readable(s, v string) bool {
	for range 5 {
		if strings.Contains(s, v) {
			return true
		}
		s = undoEscapes(s)
	}
	return false
}

// TestRedactorRuntimeEscapes: each of the 494 measured outputs, in a
// text and in a failed deploy's tail, is replaced and its key reported;
// the body is sent, not withheld. Before layer 1 read escapes, 87 were
// sent as text and 196 in a body (Go's own json.Marshal among them: the
// body escapes the app's escaped line again).
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
	values := map[string]string{}
	n := 0
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
		v, written := values[p[0]], unquote(p[2])
		r := NewRedactor([]string{"SECRET=" + v}, nil)
		text := "log: " + written + " end"
		out, keys := r.Redact(text)
		if readable(out, v) || len(keys) != 1 {
			t.Errorf("%s, %s: as text %q -> %q %v", p[0], p[1], text, out, keys)
		}
		raw, _ := json.Marshal(map[string]any{"status": "failed", "log_tail": []string{text, "x"}})
		body := r.redactJSON(raw)
		var got struct {
			LogTail     []string `json:"log_tail"`
			RedactedEnv []string `json:"redacted_env"`
		}
		if err := json.Unmarshal([]byte(body), &got); err != nil || len(got.LogTail) != 2 || readable(got.LogTail[0], v) || len(got.RedactedEnv) != 1 {
			t.Errorf("%s, %s: in a body %s -> %s", p[0], p[1], raw, body)
		}
		n++
	}
	if n != 494 {
		t.Errorf("%d outputs read, want 494", n)
	}
}

// TestEscapePatternEdges: what the measured outputs do not show.
func TestEscapePatternEdges(t *testing.T) {
	for _, tc := range []struct {
		name, value, in, want string
	}{
		{"mixed escaping", "ab/cd/ef/gh", `x ab\/cd/ef\/gh y`, "x [redacted:K] y"},
		{"upper-case hex", "p\u00e4ssw\u00f6rt12", `x p\u00E4ssw\u00F6rt12 y`, "x [redacted:K] y"},
		{"a surrogate pair, mixed case", "moon-\U0001F600-river7", `x moon-\uD83D\ude00-river7 y`, "x [redacted:K] y"},
		{"a rune as \\U", "moon-\U0001F600-river7", `x moon-\U0001f600-river7 y`, "x [redacted:K] y"},
		{"three levels", `p"w-12345`, `x p\\\\\\\"w-12345 y`, "x [redacted:K] y"},
		{"a backslash as pairs", `C:\Users\notes`, `x C:\\\\Users\\\\notes y`, "x [redacted:K] y"},
		{"no value is \\u with no backslash", "<script>a", "u003cscript>a", "u003cscript>a"},
		{"no letter of the value escaped", "ab/cdefgh", `ab\q/cdefgh`, `ab\q/cdefgh`},
		{"every character escaped, no anchor", "\u043f\u0430\u0440\u043e\u043b\u044c\u0434\u0430", `x \u043f\u0430\u0440\u043e\u043b\u044c\u0434\u0430 y`, "x [redacted:K] y"},
		{"a byte that is not UTF-8, as written", "pass\xffword1", "x pass\xffword1 y", "x [redacted:K] y"},
		{"a byte that is not UTF-8, as encoding/json writes it", "pass\xffword1", `x pass\ufffdword1 y`, "x [redacted:K] y"},
		{"a byte that is not UTF-8, as Go's %q writes it", "pass\xffword1", `x pass\xffword1 y`, "x [redacted:K] y"},
		{"a byte that is not UTF-8, as Python's surrogateescape writes it", "pass\xffword1", `x pass\udcffword1 y`, "x [redacted:K] y"},
		{"Ruby's inspect", "Pa#@w0rd-x9", `x "Pa\#@w0rd-x9" y`, `x "[redacted:K]" y`},
		{"a shell's printf %q", "Pa#$w0rd!x9 sp", `x Pa#\$w0rd\!x9\ sp y`, "x [redacted:K] y"},
		{"UTF-8 bytes as Python's repr of bytes writes them", "p\u00e4ssw\u00f6rd-\u00f1-12", `x b'p\xc3\xa4ssw\xc3\xb6rd-\xc3\xb1-12' y`, "x b'[redacted:K]' y"},
		{"a quote as \\U and eight digits", `p"w-12345`, `x p\U00000022w-12345 y`, "x [redacted:K] y"},
		{"a rune as Ruby's \\u{}", "key-\U000E0001-river7", `x key-\u{E0001}-river7 y`, "x [redacted:K] y"},
	} {
		r := NewRedactor([]string{"K=" + tc.value}, nil)
		if got, _ := r.Redact(tc.in); got != tc.want {
			t.Errorf("%s: %q -> %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
	// In a body: a run of backslashes before the value is taken whole,
	// and a value that ends in a backslash before an escaped quote
	// leaves the quote its own, so the body stays JSON.
	for _, tc := range []struct{ name, value, line string }{
		{"an odd run before the value", "ab/cdefgh", `x \ab/cdefgh y`},
		{"a value ending in a backslash, then a quote", `hunter2\`, `say "hunter2\" now`},
		{"a value beginning with a quote", `"hunter2hunter`, `x "hunter2hunter y`},
		{"the app's JSON of a value with a quote", `p"w\ord'x12`, `{"k":"p\"w\\ord'x12"}`},
	} {
		r := NewRedactor([]string{"K=" + tc.value}, nil)
		raw, _ := json.Marshal(map[string]any{"log_tail": []string{tc.line}})
		body := r.redactJSON(raw)
		var got struct {
			LogTail []string `json:"log_tail"`
		}
		if err := json.Unmarshal([]byte(body), &got); err != nil || len(got.LogTail) != 1 || readable(got.LogTail[0], tc.value) || !strings.Contains(got.LogTail[0], "[redacted:K]") {
			t.Errorf("%s: %s -> %s", tc.name, raw, body)
		}
	}
	// A value with more than escapeMax escapable characters has no
	// pattern: read as written and as encoding/json escapes it, as
	// before patterns.
	long := "abcdefgh" + strings.Repeat(`"`, escapeMax+1)
	r := NewRedactor([]string{"K=" + long}, nil)
	for _, in := range []string{long, string(mustQuote(t, long))} {
		if got, _ := r.Redact("x " + in + " y"); got != "x [redacted:K] y" {
			t.Errorf("a long value: %.40q", got)
		}
	}
	// A value's lone backslash is its own in text, never in a body,
	// where it begins an escape of the app's: a value ending in one is
	// not the line's text and its newline.
	r = NewRedactor([]string{`K=abcdefg\`}, nil)
	raw, _ := json.Marshal(map[string]any{"log_tail": []string{"abcdefg\nnext"}})
	if body := r.redactJSON(raw); body != string(raw) {
		t.Errorf("a lone backslash in a body: %s", body)
	}
	if got, _ := r.Redact(`x abcdefg\ y`); got != "x [redacted:K] y" {
		t.Errorf("a lone backslash in text: %q", got)
	}
	// A key whose name holds an escape's letters is no spelling of a
	// value: the marker is the named one.
	r = NewRedactor([]string{"u003cfoo12345=<foo12345"}, nil)
	if got, _ := r.Redact("x <foo12345 y"); got != "x [redacted:u003cfoo12345] y" {
		t.Errorf("marker: %q", got)
	}
}

// BenchmarkRedactFailedDeploy is the filter over a failed deploy's
// body of 64 KiB with an app's 20 values: the cost every body pays for
// layer 1's escapes against what the heuristics cost.
func BenchmarkRedactFailedDeploy(b *testing.B) {
	env := []string{
		"DATABASE_URL=postgres://app:s3cr3t-pa55word@db.internal:5432/app",                 // gitleaks:allow — made up for this benchmark
		"REDIS_URL=redis://:R3d1s/P4ss+w0rd@cache:6379/0",                                  // gitleaks:allow — made up for this benchmark
		"SECRET_KEY_BASE=8f2b6c1d9e0a7f3b5c4d2e1f0a9b8c7d8f2b6c1d9e0a7f3b5c4d2e1f0a9b8c7d", // gitleaks:allow — made up for this benchmark
		"STORAGE_KEY=Zm9vYmFy/cXV4+YmF6/d2lkZ2V0cw1Ab",                                     // gitleaks:allow — made up for this benchmark
		"SIGNING_SECRET=dGhpcyBpcyBhIHZlcnkgc2VjcmV0IGtleSE=",                              // gitleaks:allow — made up for this benchmark
		"SMTP_PASSWORD=riverstone42x",
		"API_TOKEN=ak-3f9a1c2b_4d5e6f70", // gitleaks:allow — made up for this benchmark
		"S3_BUCKET=demo-uploads-prod",
		"APP_URL=https://shop.example.com/",
		"SENTRY_DSN=https://abcdef0123456789@o123.ingest.sentry.io/456",
		"ADMIN_PASSWORD=p\u00e4ssw\u00f6rt-\u00d1-2024",
		`LEGACY_PW=p"w\ord'x12`,
		"MAIL_FROM=Shop <noreply@shop.example.com>",
		"OAUTH_CLIENT_SECRET=abcDEF123_ghiJKL456-mno",     // gitleaks:allow — made up for this benchmark
		"WEBHOOK_SECRET=MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw", // gitleaks:allow — made up for this benchmark
		"ENCRYPTION_KEY=q83vEjRWeJCrze/+7BV0YYKpRSU=",     // gitleaks:allow — made up for this benchmark
		"SESSION_SECRET=correct horse battery staple",
		"QUEUE_NAME=orders-priority",
		"REGION=eu-west-2-zone-b",
		"FEATURE_FLAGS=checkout,search,wishlist",
	}
	var lines []string
	for i := 0; len(strings.Join(lines, "\n")) < 60<<10; i++ {
		switch i % 6 {
		case 0:
			lines = append(lines, fmt.Sprintf("2026-11-21T10:%02d:%02d.123Z INFO request id=%08x method=GET path=/api/v1/orders/%d status=200 dur=12ms", i%60, i%60, i*7919, i))
		case 1:
			lines = append(lines, fmt.Sprintf(`{"level":"info","ts":1763719200.%03d,"msg":"cache miss","key":"product:%d","took_ms":%d}`, i%1000, i, i%50))
		case 2:
			lines = append(lines, fmt.Sprintf("Get \"https://api.partner.example/v2/items/%d\": context deadline exceeded", i))
		case 3:
			lines = append(lines, "    at Object.<anonymous> (/srv/app/node_modules/express/lib/router/index.js:284:15)")
		case 4:
			lines = append(lines, fmt.Sprintf("worker %d: processed batch %x in 0.%03ds", i%8, i*104729, i%1000))
		default:
			lines = append(lines, "SQL: SELECT id, name, price FROM products WHERE category_id = $1 ORDER BY name LIMIT 50")
		}
	}
	raw, err := json.Marshal(map[string]any{"status": "failed", "phase": "soaking", "version": "4c2d9e7b1a05", "log_tail": lines})
	if err != nil {
		b.Fatal(err)
	}
	r := NewRedactor(env, []string{"demo", "4c2d9e7b1a05"})
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	for range b.N {
		_ = r.redactJSON(raw)
	}
}

// TestLayer1ReadsEveryFormMainDid: every form layer 1 listed before its
// patterns (the value as written and as encoding/json escapes it, its
// base64, hex and URL escapes) is still replaced, in text, and in a body
// whose bytes hold it, for the measured values and for values whose
// own text holds JSON's escapes (a multi-line secret written with a
// literal \n, printed with real newlines).
func TestLayer1ReadsEveryFormMainDid(t *testing.T) {
	data, err := os.ReadFile("testdata/escapes/runtime-escapes.tsv")
	if err != nil {
		t.Fatal(err)
	}
	values := []string{`first-line-of-it\nsecond-line-of-it`, `tab\there-and-more`, `quote\"inside-it`, `C:\new\table\x`, `two\\backslashes`, `caf\u00e9-latte-12`}
	for _, line := range strings.Split(string(data), "\n") {
		if p := strings.Split(line, "\t"); len(p) == 3 && p[0] == "value" {
			var v string
			if err := json.Unmarshal([]byte(p[2]), &v); err != nil {
				t.Fatal(err)
			}
			values = append(values, v)
		}
	}
	for _, v := range values {
		j, _ := json.Marshal(v)
		forms := []string{v, string(j[1 : len(j)-1]), base64.StdEncoding.EncodeToString([]byte(v)),
			base64.RawStdEncoding.EncodeToString([]byte(v)), base64.URLEncoding.EncodeToString([]byte(v)),
			base64.RawURLEncoding.EncodeToString([]byte(v)), hex.EncodeToString([]byte(v)),
			url.QueryEscape(v), url.PathEscape(v)}
		r := NewRedactor([]string{"K=" + v}, nil)
		for _, f := range forms {
			if out, _ := r.Redact("x " + f + " y"); strings.Contains(out, f) {
				t.Errorf("%q: text keeps %q: %q", v, f, out)
			}
			raw := []byte(`{"log_tail":["x ` + f + ` y"]}`)
			if !json.Valid(raw) {
				continue // no body holds these bytes
			}
			if body := r.redactJSON(raw); strings.Contains(strings.Replace(body, `"redacted_env":`, "", 1), f) {
				t.Errorf("%q: a body keeps %q: %s", v, f, body)
			}
		}
	}
	// The reproduction: printed with real newlines, in a probe's body.
	r := NewRedactor([]string{`K=first-line-of-it\nsecond-line-of-it`}, nil)
	raw, _ := json.Marshal(map[string]any{"probe": map[string]any{"body": "config: first-line-of-it\nsecond-line-of-it end"}})
	if body := r.redactJSON(raw); !strings.Contains(body, "[redacted:K]") {
		t.Errorf("a multi-line secret printed with newlines: %s", body)
	}
	// And a level down, the app's JSON line holding it.
	line, _ := json.Marshal(map[string]string{"msg": "first-line-of-it\nsecond-line-of-it"})
	raw, _ = json.Marshal(map[string]any{"log_tail": []string{string(line)}})
	if body := r.redactJSON(raw); !strings.Contains(body, "[redacted:K]") {
		t.Errorf("a level down: %s", body)
	}
}

// TestLongestMatchBoundsEverySpelling: no spelling a form's pattern
// reads is longer than longestMatch, which the logs read's cuts leave
// room for: each character as the longest escape the pattern reads (\U
// and eight digits, \u{} with its zeros, a surrogate pair, each UTF-8
// byte as \xXX) behind seven backslashes. Eight is past the bound and
// is not read whole.
func TestLongestMatchBoundsEverySpelling(t *testing.T) {
	for _, tc := range []struct{ value, longest, deeper string }{
		{"////////", strings.Repeat(`\\\\\\\U0000002f`, 8), strings.Repeat(`\\\\\\\\U0000002f`, 8)},
		{"m\u00e4ss-\U0001F600-ab", `m` + `\\\\\\\xc3\\\\\\\xa4` + `ss` + `\\\\\\\-` +
			`\\\\\\\xf0\\\\\\\x9f\\\\\\\x98\\\\\\\x80` + `\\\\\\\-ab`, ""},
		{"q\u00e4-\u00f6-\u00fc12", `q\\\\\\\u{0000e4}\\\\\\\-\\\\\\\u{0000f6}\\\\\\\-\\\\\\\u{0000fc}12`, `q\\\\\\\u{00000e4}\\\\\\\-\\\\\\\u{00000f6}\\\\\\\-\\\\\\\u{00000fc}12`},
	} {
		r := NewRedactor([]string{"K=" + tc.value}, nil)
		if out, _ := r.Redact("x " + tc.longest + " y"); out != "x [redacted:K] y" {
			t.Errorf("%q: the longest spelling is not read: %q", tc.value, out)
		}
		if len(tc.longest) > r.longestMatch() {
			t.Errorf("%q: longestMatch %d, a spelling of %d", tc.value, r.longestMatch(), len(tc.longest))
		}
		for _, sec := range r.secrets {
			for _, inBody := range []bool{false, true} {
				if !sec.escaped {
					continue
				}
				for _, s := range []string{tc.longest, tc.deeper} {
					for _, m := range sec.spelling(inBody).FindAllStringIndex(s, -1) {
						if m[1]-m[0] > r.longestMatch() {
							t.Errorf("%q: a match of %d past longestMatch %d", tc.value, m[1]-m[0], r.longestMatch())
						}
					}
				}
			}
		}
		if tc.deeper != "" {
			if out, _ := r.Redact("x " + tc.deeper + " y"); out == "x [redacted:K] y" {
				t.Errorf("%q: a spelling past the bound read whole: %q", tc.value, out)
			}
		}
	}
}
