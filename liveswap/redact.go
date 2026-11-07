package liveswap

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The webhook's responses land in CI logs — readable by everyone with
// read access to the repository, retained, public for a public repo —
// so nothing leaves the box through them without passing this filter.
// motel is the right place for it: it built the app's environment
// from env_file, so it knows the secrets by value, and it knows which
// high-entropy strings of its own (versions, paths) are not secrets.
//
// A failed deploy's response carries the app's own bytes on purpose
// (deployDetail in app.go: the journal tail, a probe's body, the exit
// status), so this is what stands between an app that prints its
// database URL and a public Actions log.
//
// Four layers, in order:
//
//  1. Known values: every env_file value of secretMinLen or more, in
//     the forms it could appear in — as written, JSON-escaped, base64
//     (standard and URL alphabets, padded and not), hex, URL-escaped —
//     and, for a value that is a URL with a password, the password on
//     its own in the same forms. Replaced with [redacted:KEY]. Inline
//     `env` values are not secrets by policy (the Caddyfile is in a
//     repo) and are not redacted; SOCKET, HOME and the inherited PATH
//     are paths a diagnostic needs.
//  2. A safe list — the versions the status names, the app's dirs —
//     exempt from the heuristics below, so a 40-character git SHA used
//     as a version survives. Each entry is also split the way layer 4
//     tokenises, so a dotted version's generated segment is safe too.
//     Never from layer 1: a safe string equal to a known value is not
//     safe (rule 1 below).
//  3. Shape rules: credentials with a recognisable form (JWTs, bearer
//     tokens, URL userinfo and query strings, provider key prefixes,
//     PEM private keys, password=… assignments).
//  4. Entropy: a token of entropyMinLen or more characters from the
//     base64 or hex alphabet, mixed enough to look generated and with
//     Shannon entropy above the class's threshold — a bar that follows
//     the token's length, since a short one cannot reach a fixed bar
//     (looksGenerated) — replaced with [masked, N chars]. Never a
//     fingerprint: a hash of a secret is a small leak of it. An
//     assignment is judged by its parts (maskGenerated).
//
// Layer 1 also runs early, as a failed deploy's detail is built
// (failureDetail, app.go), before the journal tail and the probe body
// are cut to their caps: a cut inside a known value would leave a
// fragment this layer cannot match and no heuristic can see.
//
// Best effort past layer 1, and documented as such in
// DESIGN-threat-model.md: an encoding not in the list, a value under
// secretMinLen, a secret split across lines, and a short generated
// token the length-aware bar still misses (about half at 20 to 22
// characters), all pass.
//
// Four rules hold across the layers and every writer of a body, and
// the code is judged against them, not the other way round:
//
//  1. No safe string ever equals a known value. A safe string exempts
//     nothing from layer 1: one equal to an env_file value is dropped
//     in NewRedactor, whatever named it — a request, the config, a
//     name read off disk — so provenance is never tracked, and every
//     caller passes a plain list (redactorFor, recordRedactor).
//  2. The final known-value pass is the last write to a body. What
//     redactJSON adds after it cannot carry the app's bytes: the
//     redacted_env field (a fixed name, key names filtered, and the
//     strings the filtered body already held). The stream's spliced
//     event, phase and http_status fields (handler.go) are constants
//     and a phase word. Nothing else appends to a body after that
//     pass.
//  3. The outcome vocabulary is structural, never textual. A pair
//     "status":"<word>", "phase":"<word>" or "name":"<word>", the
//     word one of the deploy vocabulary (app.go), is the one place
//     layer 1 leaves a known form standing: a form that is a bare
//     word of the vocabulary (a value, or a URL value's password,
//     spelled as one) is replaced everywhere but in such a pair
//     (secretForm.outside) — and a word carries nothing of the
//     app's. Every other form is replaced wherever it is, and a body
//     with fewer pairs at the end than it had (a value that is part
//     of a word was replaced inside it) is withheld (filterBody).
//     The same word in error or detail is text like any other, and
//     filtered.
//  4. A version read off disk passes validVersion before it is used
//     as one — launched, listed, offered, recorded: state.json's
//     current_version (ensureRunning), a release directory's name
//     (listReleases), a record's (recordHead). Release GC enumerates
//     directory entries by name; that is not a use of a version.
const (
	secretMinLen  = 8
	entropyMinLen = 20
	// Bits per character. Random base64 is ~6, random hex ~4; English
	// identifiers sit around 3.5–4 and never clear the base64 bar.
	entropyBase64 = 4.5
	entropyHex    = 3.0
)

// Redactor is the filter: the layers above, over one app's known
// values and safe strings. Exported, with NewRedactor, Redact and
// RespondJSON, for the box webhook, which has bodies of its own to
// send; a nil *Redactor is layers 3 and 4 alone.
type Redactor struct {
	// secrets holds every form of every known value, longest first so
	// a value that contains another is replaced whole.
	secrets []*secretForm
	// safeExact is the safe strings as given, none equal to a known
	// value (rule 1): held out of the heuristics as spans. safeTokens
	// is those strings and every layer-4 token inside them: exempt
	// from the entropy heuristic.
	safeExact, safeTokens map[string]bool
	// withhold, when set, is why no body may be sent: the app's
	// env_file could not be read, so its values are unknown to the
	// filter. Fail closed rather than filter with the heuristics alone.
	withhold string
}

// secretForm is one text to replace, the keys whose values it belongs
// to (two keys can hold the same value), and the marker that replaces
// it — chosen so that no form of those keys' values is a substring of
// it, or the replacement would put the value back.
type secretForm struct {
	text string
	// outside, for a text that is a bare word of the outcome
	// vocabulary, matches the text everywhere but where it stands as
	// an outcome (rule 3): the word where it is a pair's, or on its
	// own. Nil for every other form.
	outside *regexp.Regexp
	keys    []string
	marker  string
}

// NewRedactor takes env_file KEY=VALUE pairs and the strings that must
// survive the heuristics. A safe string equal to a value is dropped
// (rule 1): whoever named it, it exempts nothing. A nil *Redactor is
// usable: layers 3 and 4 only.
//
// The box webhook builds its own: the pairs are the caller's choice,
// as an app's are liveswap's — an app's filter is primed with
// env_file alone, and SOCKET, HOME and PATH stay readable because a
// diagnostic needs its paths — so a caller priming with a process
// environment (the box, whose `caddy validate` quotes expanded values
// in its errors) will want the same for the variables that are paths.
// The safe strings are the ids a body names, which the entropy layer
// would otherwise mask: a 40-hex commit sha, a 32-hex result id, as
// liveswap lists an app's versions. Build one per set of safe strings
// and keep it: every form of every value is computed here, and a
// running process's environment does not change.
func NewRedactor(envFile []string, safe []string) *Redactor {
	known := make(map[string]bool, len(envFile))
	for _, kv := range envFile {
		if _, value, ok := strings.Cut(kv, "="); ok {
			known[value] = true
		}
	}
	r := &Redactor{safeExact: make(map[string]bool, len(safe)), safeTokens: make(map[string]bool, len(safe))}
	for _, s := range safe {
		if s == "" || known[s] {
			continue
		}
		r.safeExact[s] = true
		r.safeTokens[s] = true
		for _, tok := range entropyTokenRe.FindAllString(s, -1) {
			r.safeTokens[tok] = true
		}
	}
	byText := map[string]*secretForm{}
	for _, kv := range envFile {
		key, value, ok := strings.Cut(kv, "=")
		if !ok || len(value) < secretMinLen {
			continue
		}
		forms := secretForms(value)
		// A connection URL's password is a secret on its own: a driver
		// prints it without the rest of the URL.
		for _, pw := range urlPasswords(value) {
			if len(pw) >= secretMinLen {
				forms = append(forms, secretForms(pw)...)
			}
		}
		for _, form := range forms {
			if len(form) < secretMinLen {
				continue
			}
			sf := byText[form]
			if sf == nil {
				sf = &secretForm{text: form, outside: outcomeOutside[form]}
				byText[form] = sf
				r.secrets = append(r.secrets, sf)
			}
			if !contains(sf.keys, key) {
				sf.keys = append(sf.keys, key)
			}
		}
	}
	all := make([]string, 0, len(r.secrets))
	for _, sf := range r.secrets {
		all = append(all, sf.text)
	}
	for _, sf := range r.secrets {
		sort.Strings(sf.keys)
		sf.marker = chooseMarker(sf.keys, all)
	}
	sort.SliceStable(r.secrets, func(i, j int) bool { return len(r.secrets[i].text) > len(r.secrets[j].text) })
	return r
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// chooseMarker is the replacement for the values of keys: the first
// candidate that contains no known form of any value — not only this
// key's, since another key's value ("redacted", say) can be a
// substring of a marker and would then be put back by a later
// replacement. The named candidates come first; when every one of
// them contains a form (a key named for its own value), the marker is
// a number in brackets, which at under 8 characters can contain no
// form at all.
func chooseMarker(keys []string, all []string) string {
	label := strings.Join(keys, ",")
	candidates := []string{
		"[redacted:" + label + "]",
		"[REDACTED:" + label + "]",
		"[withheld:" + label + "]",
		"<" + label + " removed>",
	}
	for n := 1; ; n++ {
		var c string
		if n <= len(candidates) {
			c = candidates[n-1]
		} else {
			c = "[#" + strconv.Itoa(n-len(candidates)) + "]"
		}
		clean := true
		for _, f := range all {
			if strings.Contains(c, f) {
				clean = false
				break
			}
		}
		if clean {
			return c
		}
	}
}

// urlPasswords is every password in a value of the form
// scheme://user:password@host…, as a driver reads it and as net/url
// does, where the two differ. libpq and others take a password with
// an unescaped `#`, a bare `%` or a space as it stands, where url.Parse
// reads a fragment, fails on the escape, or finds no user; so each
// authority is split by hand — after a `://`, up to the first `/`, `?`
// or control character, the user and password before its last `@` —
// and each `://` is an authority, since a driver's list of hosts
// (`http://a:9200,http://u:pw@b:9200`) holds several. net/url's reading
// counts beside it, for the escapes it decodes (`p%40ss` as `p@ss`),
// where the hand found one: an authority with no `@` is one with no
// user, or one cut inside a password that holds `/` or `?`, which both
// read short — and what stands before the cut is not the secret, so
// masking it wherever it appears would be a mask over nothing. Such a
// password is not read on its own (the residual); the URL whole still
// is. A space is a password's to hold, so a URL followed by prose with
// a `:` and an `@` in it reads a password that is none (the other
// residual, DESIGN-liveswap.md): a mask over a string that was no
// secret, named for the key.
func urlPasswords(v string) []string {
	var pws []string
	for rest := v; ; {
		i := strings.Index(rest, "://")
		if i < 0 {
			break
		}
		rest = rest[i+3:]
		authority := rest
		if j := strings.IndexFunc(authority, func(r rune) bool { return r == '/' || r == '?' || r < ' ' }); j >= 0 {
			authority = authority[:j]
		}
		if at := strings.LastIndexByte(authority, '@'); at >= 0 {
			if _, pw, ok := strings.Cut(authority[:at], ":"); ok {
				if !contains(pws, pw) {
					pws = append(pws, pw)
				}
				// And as a driver decodes it (`p%40ss` as `p@ss`), which
				// net/url below gives for the first authority alone.
				if decoded, err := url.PathUnescape(pw); err == nil && decoded != pw && !contains(pws, decoded) {
					pws = append(pws, decoded)
				}
			}
		}
	}
	if len(pws) == 0 {
		return nil
	}
	if u, err := url.Parse(v); err == nil && u.User != nil {
		if pw, ok := u.User.Password(); ok && !contains(pws, pw) {
			pws = append(pws, pw)
		}
	}
	return pws
}

// secretForms is every encoding of a value the filter recognises.
func secretForms(v string) []string {
	forms := []string{v}
	if j, err := json.Marshal(v); err == nil {
		if s := string(j[1 : len(j)-1]); s != v {
			forms = append(forms, s)
		}
	}
	b := []byte(v)
	forms = append(forms,
		base64.StdEncoding.EncodeToString(b),
		base64.RawStdEncoding.EncodeToString(b),
		base64.URLEncoding.EncodeToString(b),
		base64.RawURLEncoding.EncodeToString(b),
		hex.EncodeToString(b),
	)
	if q := url.QueryEscape(v); q != v {
		forms = append(forms, q)
	}
	if p := url.PathEscape(v); p != v {
		forms = append(forms, p)
	}
	return forms
}

// shapeRules are the recognisable credential forms. Each replacement
// keeps enough context to read the line and names the rule.
var shapeRules = []struct {
	re   *regexp.Regexp
	repl string
}{
	// PEM private keys, whole block.
	{regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`), "[redacted:private-key]"},
	// JWT: three base64url segments, the header starting {"…
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\b`), "[redacted:jwt]"},
	// Authorization schemes with a token-shaped credential. The
	// placeholder text in motel's own 401 body, "Bearer <jwt>", is
	// not token-shaped and passes.
	{regexp.MustCompile(`(?i)\b(bearer|basic|token)\s+[A-Za-z0-9._~+/=-]{20,}`), "$1 [redacted:token]"},
	// URL userinfo: scheme://user:pass@host.
	{regexp.MustCompile(`\b([a-z][a-z0-9+.-]*://)[^/\s"'@:]+:[^/\s"'@]+@`), "$1[redacted:userinfo]@"},
	// URL query strings: where presigned-URL credentials live. The
	// same rule the box applies to its own logs (redactURL).
	{regexp.MustCompile(`\b([a-z][a-z0-9+.-]*://[^\s"'?]+)\?[^\s"']+`), "$1?[redacted:query]"},
	// Provider prefixes.
	{regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`), "[redacted:aws-key]"},
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}\b`), "[redacted:github-token]"},
	{regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,}\b`), "[redacted:github-token]"},
	{regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}\b`), "[redacted:slack-token]"},
	{regexp.MustCompile(`\b[sr]k_(?:live|test)_[A-Za-z0-9]{16,}\b`), "[redacted:stripe-key]"},
	// KEY=value, key: value and JSON "key":"value" assignments whose
	// key contains a credential word, whatever surrounds it
	// (DB_PASSWORD, AWS_SECRET_ACCESS_KEY, STRIPE_API_KEY). The value
	// must be 8+ characters that are not a delimiter, not a marker
	// layer 1 left there, and not a path or placeholder (an argv flag
	// such as --api-key=/etc/motel/app.key names a file, not a
	// secret).
	{regexp.MustCompile(`(?i)\b([a-z0-9_-]*(?:password|passwd|secret|api[_-]?key|access[_-]?token|private[_-]?key)[a-z0-9_-]*)(["']?\s*[=:]\s*["']?)[^\s"',;\[/{][^\s"',;]{7,}`), "$1$2[redacted:credential]"},
}

// basicCredentialRe finds "Basic <base64>"; the decode check is in
// redact, since a regexp cannot tell a credential from a word. Four
// characters is the shortest base64 of a user:pass ("a:b").
var basicCredentialRe = regexp.MustCompile(`(?i)\bbasic[ \t]+[A-Za-z0-9+/]{4,}={0,2}`)

// entropyTokenRe finds candidates for layer 4. `/` is not in the
// alphabet on purpose: a URL path is a long base64-alphabet run with
// slashes, and splitting there keeps paths readable while a base64
// secret with a slash in it still leaves 20+-character pieces. `.` is
// out for the same reason (hostnames, unit names, dotted versions),
// which is why the safe list is split the same way.
var entropyTokenRe = regexp.MustCompile(`[A-Za-z0-9+=_-]{20,}`)

// replaceKnown is layer 1 on its own: every known form replaced by its
// marker, and the keys found added to seen. A form that is a bare
// word of the outcome vocabulary is replaced everywhere but where it
// stands as an outcome (rule 3). Markers contain no form, so a second
// pass over its own output changes nothing.
func (r *Redactor) replaceKnown(s string, seen map[string]bool) string {
	if r == nil {
		return s
	}
	for _, sec := range r.secrets {
		if !strings.Contains(s, sec.text) {
			continue
		}
		found := true
		if sec.outside == nil {
			s = strings.ReplaceAll(s, sec.text, sec.marker)
		} else {
			found = false
			s = sec.outside.ReplaceAllStringFunc(s, func(m string) string {
				if m[0] == '"' { // the pair, left as it is
					return m
				}
				found = true
				return sec.marker
			})
		}
		if !found {
			continue
		}
		for _, k := range sec.keys {
			seen[k] = true
		}
	}
	return s
}

// nestedStrings is every string in an array under name, anywhere in
// the decoded JSON v, appended to out: the strings, and only those,
// since the field is key names.
func nestedStrings(v any, name string, out []string) []string {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if k != name {
				out = nestedStrings(child, name, out)
				continue
			}
			if items, ok := child.([]any); ok {
				for _, it := range items {
					if s, ok := it.(string); ok {
						out = append(out, s)
					}
				}
			}
		}
	case []any:
		for _, child := range t {
			out = nestedStrings(child, name, out)
		}
	}
	return out
}

// union is both lists' names, each once, sorted.
func union(a, b []string) []string {
	set := make(map[string]bool, len(a)+len(b))
	for _, n := range a {
		set[n] = true
	}
	for _, n := range b {
		set[n] = true
	}
	return reportedKeys(set)
}

func reportedKeys(seen map[string]bool) []string {
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// redact filters s and reports which env keys' values it found. Layer
// 1 runs first and again last: the heuristic layers' own marker text
// ("[redacted:private-key]") can contain a known value, and only a
// pass over the finished text guarantees none remains. Rule 3 holds
// here as in a body: a value that is a word of the outcome vocabulary
// stands where the text spells it as an outcome pair. Every body goes
// through redactJSON; this is the text form, for tests and for what
// the box webhook writes somewhere other than a response body.
func (r *Redactor) Redact(s string) (string, []string) {
	seen := map[string]bool{}
	s = r.filter(s, seen)
	s = r.replaceKnown(s, seen)
	return s, reportedKeys(seen)
}

// filter is layers 1 to 4 in order, with the safe strings held out of
// the heuristic layers: a version shaped like a provider token, or a
// path with a generated-looking segment, comes back as it went in.
func (r *Redactor) filter(s string, seen map[string]bool) string {
	s = r.replaceKnown(s, seen)
	s, restore := r.protectSafe(s)
	s = r.heuristics(s)
	return restore(s)
}

// safeSpanMinLen is the shortest safe string protectSafe holds out of
// the heuristics. Below it nothing a rule matches fits inside the
// string, and a short one — a one-character app name or version —
// would otherwise be swapped out of the middle of a credential and
// split it into pieces too short for the entropy layer to see.
const safeSpanMinLen = 12

// protectSafe swaps every safe string of safeSpanMinLen or more for a
// placeholder no rule can match (control characters are in no rule's
// alphabet) and returns the function that swaps them back. Longest
// first, so a safe string containing another is protected whole.
func (r *Redactor) protectSafe(s string) (string, func(string) string) {
	if r == nil || len(r.safeExact) == 0 {
		return s, func(s string) string { return s }
	}
	safe := make([]string, 0, len(r.safeExact))
	for v := range r.safeExact {
		if len(v) >= safeSpanMinLen {
			safe = append(safe, v)
		}
	}
	sort.Slice(safe, func(i, j int) bool {
		return len(safe[i]) > len(safe[j]) || (len(safe[i]) == len(safe[j]) && safe[i] < safe[j])
	})
	var used []string
	for _, v := range safe {
		if strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, "\x00"+strconv.Itoa(len(used))+"\x00")
			used = append(used, v)
		}
	}
	return s, func(s string) string {
		for i := len(used) - 1; i >= 0; i-- {
			s = strings.ReplaceAll(s, "\x00"+strconv.Itoa(i)+"\x00", used[i])
		}
		return s
	}
}

// heuristics is layers 3 and 4.
func (r *Redactor) heuristics(s string) string {
	for _, rule := range shapeRules {
		s = rule.re.ReplaceAllString(s, rule.repl)
	}
	s = basicCredentialRe.ReplaceAllStringFunc(s, func(m string) string {
		// Basic's credential is base64 of user:password, and is one at
		// any length — the bearer rule's 20-character floor would let
		// `Basic dToxMjM0NTY3OA==` (u:12345678) through. Prose after the
		// word ("Basic authentication") does not decode to a user:pass.
		i := strings.LastIndexAny(m, " \t")
		if i < 0 {
			return m
		}
		dec, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(m[i+1:], "="))
		if err == nil && strings.Contains(string(dec), ":") {
			return m[:i+1] + "[redacted:basic-credential]"
		}
		return m
	})
	return entropyTokenRe.ReplaceAllStringFunc(s, r.maskGenerated)
}

// maskGenerated is layer 4 over one token. An assignment — an `=` with
// more after it, where base64 pads only at its end — is judged by its
// parts: `RandomizedDelaySec=40s` is a setting a reader needs, and
// `BUILD_ID=<secret>` keeps its key and masks its value (a key the
// shape rules know, `API_KEY=`, is theirs first); a value short of
// entropyMinLen is not carried over the bar by its key's letters.
func (r *Redactor) maskGenerated(tok string) string {
	if r != nil && r.safeTokens[tok] {
		return tok
	}
	if i := strings.IndexByte(tok, '='); i > 0 && i < len(tok)-1 && tok[i+1] != '=' {
		return r.maskGenerated(tok[:i]) + "=" + r.maskGenerated(tok[i+1:])
	}
	if len(tok) >= entropyMinLen && looksGenerated(tok) {
		return "[masked, " + strconv.Itoa(len(tok)) + " chars]"
	}
	return tok
}

// redactJSON filters a marshalled JSON body and, when a known value
// was found, adds a `redacted_env` field naming the keys. Whatever is
// finally sent — the filtered body or a fallback — gets a last
// layer-1 pass, so no known form is in the bytes written, and the
// result is valid JSON.
//
// The report field is added after that pass, its name verbatim and
// each key name filtered on its own: the name is fixed public text in
// every such response, and a value equal to it is not learned from
// the response, whereas a field renamed by the filter would break the
// one thing the field is for. That fixed name is the single text the
// strict property (FuzzRedactor) does not cover.
//
// Layer 1 is a plain substring replacement, so a value made of JSON's
// own punctuation could match across a string's delimiters and leave
// the body unparsable; that body is withheld rather than sent, with
// the keys still reported. A filter whose env_file could not be read
// withholds every body: the heuristics alone are not the promise. A
// body that is still not JSON at the end — a value equal to the
// response's own punctuation, twice over — becomes the one body that
// can contain nothing: a marker under 8 characters.
func (r *Redactor) redactJSON(raw []byte) string {
	seen := map[string]bool{}
	var body string
	if r != nil && r.withhold != "" {
		// Marshalled, not concatenated: the reason carries a path. And
		// filtered like any body: the path, or the parse error quoting
		// a line of the file, can carry a token-shaped string.
		b, err := json.Marshal(map[string]string{"error": "response withheld: " + r.withhold})
		if err != nil {
			return `{"error":"[#0]"}`
		}
		body = r.filter(string(b), seen)
		if !json.Valid([]byte(body)) {
			body = `{"error":"response withheld: the app's env_file could not be read"}`
		}
		body = r.replaceKnown(body, seen)
	} else {
		body = r.filterBody(string(raw), seen)
	}
	// The hoist runs when the filter found a value, and when the body
	// carries a redacted_env of its own below the top level — a
	// deployResult's, whose values the detail replaced before any
	// filter ran, so that the filter finds nothing here (app.go,
	// failureDetail).
	if len(seen) > 0 || strings.Contains(body, `"redacted_env"`) {
		names := reportedKeys(seen)
		for i, n := range names {
			names[i] = r.replaceKnown(n, seen)
		}
		body = withField(body, "redacted_env", names)
	}
	if !json.Valid([]byte(body)) {
		return `{"error":"[#0]"}`
	}
	return body
}

// filterBody is the layers over a body as marshalled, then the final
// pass; what it returns is the bytes written, or a withheld body when
// a known value overlapped the body's structure or one of its
// outcomes.
func (r *Redactor) filterBody(raw string, seen map[string]bool) string {
	outcomes := len(outcomePairRe.FindAllStringIndex(raw, -1))
	body := r.filter(raw, seen)
	if !json.Valid([]byte(body)) {
		return r.replaceKnown(`{"error":"response withheld: a redacted value overlapped the response's own structure"}`, seen)
	}
	body = r.replaceKnown(body, seen)
	// Every outcome the body had is still a word (rule 3), or the body
	// is withheld: a value that is part of a word ("ucceeded") was
	// replaced inside it, and a body whose status is not a word of the
	// vocabulary is not served. No layer writes a quote it did not
	// find, and none moves one next to an outcome key, so nothing can
	// make a pair; the count can only fall.
	if len(outcomePairRe.FindAllStringIndex(body, -1)) < outcomes {
		return r.replaceKnown(`{"error":"response withheld: a redacted value overlapped the response's own outcome"}`, seen)
	}
	return body
}

// outcomePairRe is a status, phase or phase name standing as one: a
// quoted key, a colon, a quoted word of the vocabulary.
var outcomePairRe = regexp.MustCompile(`"(?:status|phase|name)":"(?:` + outcomeAlternation() + `)"`)

// outcomeOutside is, per word of the vocabulary, the pattern that
// matches it everywhere but where it stands as an outcome
// (secretForm.outside): compiled once here, from the vocabulary's own
// constants, never from a value.
var outcomeOutside = func() map[string]*regexp.Regexp {
	m := make(map[string]*regexp.Regexp, len(outcomeStatuses)+len(outcomePhases))
	for _, w := range strings.Split(outcomeAlternation(), "|") {
		m[w] = regexp.MustCompile(`"(?:status|phase|name)":"` + w + `"|` + w)
	}
	return m
}()

// outcomeAlternation is the vocabulary as a pattern.
func outcomeAlternation() string {
	words := make([]string, 0, len(outcomeStatuses)+len(outcomePhases))
	for w := range outcomeStatuses {
		words = append(words, regexp.QuoteMeta(w))
	}
	for w := range outcomePhases {
		words = append(words, regexp.QuoteMeta(w))
	}
	sort.Strings(words)
	return strings.Join(words, "|")
}

// withField sets a string-array field on a JSON object body, merged
// with every array the body already holds under that name, at its top
// level or below it. The body is the filter's output: an array there —
// a stored record's own redacted_env, read back, or a deployResult's
// (RedactedEnv, the keys its detail's early replacement found) nested
// in a status — has been through every layer as body text, so its
// entries are kept, never replaced, and nothing that did not pass the
// filter is added; hoisting them keeps the body's own field the one
// place a reader looks. The final pass runs before this (rule 2). The
// object is re-marshalled, so its top-level keys come out sorted; its
// values are carried as raw JSON, so a nested object keeps its keys'
// order. Anything but an object is returned as it is.
func withField(body, name string, values []string) string {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &obj); err != nil || obj == nil {
		return body
	}
	var tree any
	_ = json.Unmarshal([]byte(body), &tree) // obj parsed, so this does too
	had := nestedStrings(tree, name, nil)
	all := union(values, had)
	if len(all) == 0 {
		return body // the name was a string's text, not a field: no empty field is added
	}
	v, err := json.Marshal(all)
	if err != nil {
		return body
	}
	obj[name] = v
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return string(out)
}

// looksGenerated is layer 4's test. Hex needs a digit and clears
// entropyHex; anything else needs a digit, an upper- and a lower-case
// letter (identifiers and words rarely have all three) and clears
// entropyBase64.
func looksGenerated(tok string) bool {
	var digit, upper, lower, other bool
	for _, c := range tok {
		switch {
		case c >= '0' && c <= '9':
			digit = true
		case c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
			if c >= 'a' {
				lower = true
			} else {
				upper = true
			}
		case c >= 'g' && c <= 'z':
			lower, other = true, true
		case c >= 'G' && c <= 'Z':
			upper, other = true, true
		default:
			other = true
		}
	}
	if !digit {
		return false
	}
	h := shannon(tok)
	if !other { // hex alphabet only
		return h >= entropyHex
	}
	// A string's entropy over its own characters is at most log2(len):
	// 4.32 bits at 20 characters, 4.46 at 22, so a fixed bar of 4.5 is
	// out of a short token's reach however random it is. The bar
	// follows the length, entropySlack under that ceiling, up to
	// entropyBase64. Measured 2026-11-07 on random url-safe tokens
	// against the mixed-case tokens in this repository's own text
	// (DESIGN-liveswap.md, "Accepted residuals"): this masks 55% of
	// random 20- and 22-character tokens and 41% of 24, where the
	// fixed bar masked none, and one non-secret of 148 (an e-mail's
	// local part); a bar 0.9·log2(len) would mask 80% of each length
	// and nine non-secrets, among them identifiers with a digit
	// (`loadEd25519PrivateKey`) and unit settings.
	bar := math.Min(entropyBase64, math.Log2(float64(len(tok)))-entropySlack)
	return upper && lower && h >= bar
}

// entropySlack is how far under log2(len) the base64 bar sits for a
// token too short to reach entropyBase64 (looksGenerated).
const entropySlack = 0.3

func shannon(s string) float64 {
	var counts [256]int
	for i := 0; i < len(s); i++ {
		counts[s[i]]++
	}
	n := float64(len(s))
	var h float64
	for _, c := range counts {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}
