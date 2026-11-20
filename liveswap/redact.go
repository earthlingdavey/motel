package liveswap

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"maps"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/smallhoursorg/motel/liveswap/wire"
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
//     its own in the same forms. Replaced with [redacted:KEY], but
//     inside a body's times and numbers, which no layer sees (rule
//     7). Inline
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
// Seven rules hold across the layers and every writer of a body, and
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
//     and a phase word. The body's times and numbers are put back
//     after it, held out of every layer (rule 7). Nothing
//     else appends to a body after that pass. A body about several
//     apps (GET /_motel/apps) is made of rows, and each row's summary
//     is a body here: filtered by its own app's filter, then given
//     the app's name, the route's own word, and put in the list,
//     which adds its punctuation alone; a row the
//     filter withheld is the name and the filter's sentence. The
//     box's alerts (GET /_motel/alerts) are rows the same way: an
//     app's alert is filtered by its app's filter, every other alert
//     and every silence by one that knows no value, and an alert row is
//     then given back its times, Alertmanager's and never the app's
//     bytes (motel_alerts.go, alertRow); a row withheld keeps only
//     motel's and Alertmanager's words from closed sets (a state, a
//     severity, inhibited), its times, an id Alertmanager made, the
//     filter's sentence, and its app's name when its own app's filter
//     withheld it. So no filter is ever given two apps' values,
//     and the outcome pairs of rule 3 are counted per row.
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
//  5. The app's own name stands where a JSON body names the app by
//     it. An env_file value that is the name as written (DB_NAME=<app>,
//     a common setup) is left in the pair "app":"<name>" (holdOwnName),
//     as rule 3 leaves an outcome word in its pair: the name is in the
//     request's path and in the config, and a status whose app is a
//     marker names no app. In a JSON body alone, where a quote inside a
//     string is escaped and the pair can only be the body's own field;
//     plain text (Redact) is filtered as if there were no rule. Only
//     that value: an encoding of a value, or a URL's password, that
//     happens to equal the name is replaced in the pair too. The value
//     is replaced everywhere else, as any is: in text, inside a path,
//     under another key. A value that holds the name inside it is a
//     longer form, replaced whole before the name is looked at
//     (longest first). The app list's rows name their app outside the
//     filter (motel_routes.go).
//  6. What a request itself sent is never run past the app's values
//     when the body holds nothing else of the app's. Layer 1 replaces
//     a value wherever a body holds it and names the key, so a body
//     that echoes the caller's text through the app's filter answers
//     a question the caller may not ask: "is this one of the app's
//     values?", for the value as written, inside a longer string, in
//     base64 or hex, or a URL value's password alone. A caller who may
//     only read could try values until a marker came back. So a body
//     made only of the handler's own words and this request's text is
//     written through a filter that knows no value (NewRedactor with
//     none; the shape and entropy layers still run, and a version
//     asked for is a safe string, as in the status). Today those are
//     the answers of a read that reads nothing of the app's, each of
//     which a person with the viewer role can ask for: the 404 of
//     GET /<app>?deploy=<version> for a version with no record, and
//     the 422 of a query that does not parse (handler.go). A body
//     that only might hold something of the app's does not qualify:
//     the 500 of that read carried the system's error, which named the
//     version asked for but also the app's directory, and an env_file
//     value may equal that. With the error in it the 500 was no such
//     body; it now says one fixed sentence, which is, and the journal
//     has the error.
//     The exemption is the body's, never the string's: a string is not
//     exempt because a request named it (rule 1), and a body that
//     holds one byte the app wrote, or text an earlier request stored
//     (a deployed version, in a status or a record), passes the app's
//     filter whole. A deploy's refusals that echo its version or URL
//     (a version that exists, a rollback with no release) still pass
//     the app's filter: only a deploy token reaches them, and it can
//     read the app's environment by deploying
//     (DESIGN-threat-model.md).
//  7. A time or a number is structural, never text. A JSON body's
//     time under one of heldTimeKeys, shaped as encoding/json writes a
//     time.Time (heldTimeRe), and every number of a JSON body (a token
//     outside every string), pass no layer. A time is the box's clock
//     (a deploy's and its phases', the watchdog's last restart, the
//     metrics store's step, a record's as written, rule 1 of
//     deploys.go). A number is a figure encoding/json wrote from a Go
//     number, never the app's bytes, which are always in a string: the
//     box's own (a pid, a phase's seconds, the watchdog's counts), the
//     operator's (the caps), the store's (the metrics summary, which the
//     app's behaviour drives and its bytes cannot write), the
//     deployer's (an artifact's size and entries) or the app's answer's
//     (a probe's status code, three digits). An env_file value that is
//     a piece of one (a date, API_VERSION=2026-11-19; a clock time;
//     digits of a time's fraction; the app's own memory limit in bytes,
//     equal to its memory_max) replaced there left a time no client
//     reads, or a body that is no JSON, which was withheld whole, the
//     key named. Held out (holdOut), the rest filtered, then put back
//     after the final pass (putBack); the value it equals is replaced
//     everywhere else, digits in a string included, and a body that
//     holds it only in a time or a number reports no key. The filter so
//     sees the body with placeholders where the tokens stood, and a
//     value made of a token's end and the JSON after it (or the JSON
//     before it and its start) could stand across the token's edge in
//     the bytes it returns: putBack looks for every known form across
//     each edge, and a body with one, or whose placeholders the filter
//     changed, or that holds a placeholder's shape already (a string
//     that is a NUL, a ; and digits), goes through whole as before,
//     times, numbers and all (TestHeldTimesFallBack,
//     TestHeldNumbersFallBack); a value across a number's edge so sends
//     the body's times through the layers too. After the filter, the
//     hoist's sort of the top level (withField) and the stream's spliced
//     fields can set a held token beside other JSON, and a value made of
//     a token's end and that JSON stands in the bytes written: it is the
//     body's own text (and for a body whose number held a value, which
//     was withheld before, takes a second value). A token whole names
//     no key, where a marker inside a time said the value was a piece
//     of it and a body withheld for a number said the key's value was
//     digits of one. A number that holds a value replaced elsewhere in
//     the body (the app printed it: replaced by this filter, or by an
//     earlier pass, a failed deploy's detail, whose marker the body
//     holds or a nested redacted_env names) would show it beside that
//     marker by difference, so such
//     a body goes through whole, and is withheld as before
//     (TestHeldNumbersNotByDifference,
//     TestFailedDeployNumberNotByDifference); a time keeps its hold, its
//     marker having said as much. Across two answers (a marker in one
//     record, the number in the status) the reader can still set them
//     side by side: an accepted row (DESIGN-liveswap.md). Rule 7 is the
//     hold of a filter that knows a value: one that knows none
//     replaces nothing a token could hold, and its tokens pass its
//     later layers as before.
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
	// inBody is outside for a JSON body when the text is the app's own
	// name as an env_file value holds it (rule 5): it matches the text
	// everywhere but its "app" pair, and an outcome word's pairs too.
	// Used in a body alone (redactJSON); text is filtered by outside.
	inBody *regexp.Regexp
	// literal: some key's value is this text as written, which rule 5
	// asks of the name; a form derived from a value (an encoding, a
	// URL's password) is not.
	literal bool
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
				sf = &secretForm{text: form, outside: outcomeOutside[form], literal: true}
				byText[form] = sf
				r.secrets = append(r.secrets, sf)
			}
			// Literal while every key it came from holds it as written:
			// shared with another value's encoding, it is that too.
			sf.literal = sf.literal && form == value
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
// stands as an outcome (rule 3), and in a JSON body (inBody) the
// app's own name everywhere but its "app" pair (rule 5). Markers
// contain no form, so a second pass over its own output changes
// nothing.
func (r *Redactor) replaceKnown(s string, seen map[string]bool, inBody bool) string {
	if r == nil {
		return s
	}
	for _, sec := range r.secrets {
		if !strings.Contains(s, sec.text) {
			continue
		}
		found := true
		pattern := sec.outside
		if inBody && sec.inBody != nil {
			pattern = sec.inBody
		}
		if pattern == nil {
			s = strings.ReplaceAll(s, sec.text, sec.marker)
		} else {
			found = false
			s = pattern.ReplaceAllStringFunc(s, func(m string) string {
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
	s = r.filter(s, seen, false)
	s = r.replaceKnown(s, seen, false)
	return s, reportedKeys(seen)
}

// filter is layers 1 to 4 in order, with the safe strings held out of
// the heuristic layers: a version shaped like a provider token, or a
// path with a generated-looking segment, comes back as it went in.
// inBody is whether s is a JSON body (replaceKnown).
func (r *Redactor) filter(s string, seen map[string]bool, inBody bool) string {
	s = r.replaceKnown(s, seen, inBody)
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

// holdOwnName is rule 5: when an env_file value is the app's name as
// written, a JSON body's pair "app":"<name>" is left standing, and the
// value is replaced everywhere else. In a JSON body only: there a
// quote inside a string is escaped, so the pair can only be the body's
// own field, never text an app wrote; plain text (Redact) has no such
// guarantee and is filtered as if there were no rule. Only the value
// itself: an encoding of a value, or a URL's password, equal to the
// name is replaced as before. Called by the app's filter (redactorFor)
// with its name.
func (r *Redactor) holdOwnName(name string) {
	if r == nil || name == "" {
		return
	}
	q := regexp.QuoteMeta(name)
	pairs := `"app":"` + q + `"`
	if outcomeOutside[name] != nil {
		pairs += `|"(?:status|phase|name)":"` + q + `"`
	}
	for _, sf := range r.secrets {
		if sf.text == name && sf.literal {
			sf.inBody = regexp.MustCompile(pairs + `|` + q)
		}
	}
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
// one thing the field is for. That fixed name, and a body's times and
// numbers (rule 7), are the texts the strict property (FuzzRedactor) does not
// cover.
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
		b, err := json.Marshal(wire.Error{Error: "response withheld: " + r.withhold})
		if err != nil {
			return `{"error":"[#0]"}`
		}
		body = r.filter(string(b), seen, true)
		if !json.Valid([]byte(body)) {
			body = `{"error":"response withheld: the app's env_file could not be read"}`
		}
		body = r.replaceKnown(body, seen, true)
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
			names[i] = r.replaceKnown(n, seen, false) // a key name, not a body
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
// outcomes. The body's times and numbers pass none of it (rule 7):
// they are held out and put back after the final pass, unless a value
// of the app's stands across one's edge in the bytes that would be
// sent, or the filter changed what held them; then the body goes
// through whole, times, numbers and all.
func (r *Redactor) filterBody(raw string, seen map[string]bool) string {
	if r != nil && len(r.secrets) > 0 {
		if body, held := holdOut(raw); len(held) > 0 {
			found := map[string]bool{}
			out := r.filterWhole(body, found)
			// Replaced in this body: what this pass found, and what an
			// earlier pass did (a failed deploy's detail, replaced as it
			// was built, failureDetail in app.go), which its marker in
			// the body shows, or a nested redacted_env names (the name
			// as written, or a marker when a value equals a key's name;
			// a marker the tail's cap cut).
			replaced := maps.Clone(found)
			for _, sec := range r.secrets {
				// As written, and as encoding/json escapes it (the angles
				// of "<KEY removed>").
				marked := strings.Contains(raw, sec.marker)
				if !marked && strings.ContainsAny(sec.marker, "<>&") {
					esc, _ := json.Marshal(sec.marker)
					marked = strings.Contains(raw, string(esc[1:len(esc)-1]))
				}
				if marked {
					for _, k := range sec.keys {
						replaced[k] = true
					}
				}
			}
			if strings.Contains(raw, `"redacted_env"`) {
				var tree any
				_ = json.Unmarshal([]byte(raw), &tree)
				for _, k := range nestedStrings(tree, "redacted_env", nil) {
					replaced[k] = true
				}
			}
			if out, ok := r.putBack(out, held, replaced); ok {
				for k := range found {
					seen[k] = true
				}
				return out
			}
		}
	}
	return r.filterWhole(raw, seen)
}

// filterWhole is filterBody over every byte of the body.
func (r *Redactor) filterWhole(raw string, seen map[string]bool) string {
	outcomes := len(outcomePairRe.FindAllStringIndex(raw, -1))
	body := r.filter(raw, seen, true)
	if !json.Valid([]byte(body)) {
		return r.replaceKnown(`{"error":"response withheld: a redacted value overlapped the response's own structure"}`, seen, true)
	}
	body = r.replaceKnown(body, seen, true)
	// Every outcome the body had is still a word (rule 3), or the body
	// is withheld: a value that is part of a word ("ucceeded") was
	// replaced inside it, and a body whose status is not a word of the
	// vocabulary is not served. No layer writes a quote it did not
	// find, and none moves one next to an outcome key, so nothing can
	// make a pair; the count can only fall.
	if len(outcomePairRe.FindAllStringIndex(body, -1)) < outcomes {
		return r.replaceKnown(`{"error":"response withheld: a redacted value overlapped the response's own outcome"}`, seen, true)
	}
	return body
}

// heldTimeKeys are the keys a body passing an app's filter holds a
// time under (wire's Status, Failure, DeployResult, PhaseEvent and the
// apps list's rows; TestHeldTimeKeysAreTheWireTimes), each as it stands
// quoted in the body. The alerts bodies keep their times out themselves
// (filterAlerts, alertRow).
var heldTimeKeys = map[string]bool{`"at"`: true, `"started_at"`: true, `"finished_at"`: true, `"last_restart_at"`: true}

// heldTimeRe is a time as encoding/json writes a time.Time, quoted:
// RFC 3339 with up to nine digits of the second.
var heldTimeRe = regexp.MustCompile(`^"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{1,9})?(?:Z|[+-][0-9]{2}:[0-9]{2})"$`)

// heldMark opens the placeholder a held time or number stands as while
// the body is filtered: a string that is a NUL, escaped, a ; and then
// the held token's number ("\u0000;3"), which placeholderRe matches.
// The ; parts the NUL's zeros from the number, so that no value of
// digits alone is a piece of a placeholder below ten million tokens,
// and ends the value a credential assignment's rule (layer 3) would
// take after a key such as "secret_…", which a placeholder then never
// fills. No layer makes such a string (a marker is key names in
// brackets or angles), and a body that holds one already, an app's line
// that is a NUL, a ; and digits, has nothing held. Placeholders are
// found as whole strings of the JSON, so an app's text that only holds
// the bytes (a binary probe body's quote before a NUL, a literal
// \u0000) is no placeholder.
const heldMark = `"\u0000;`

var placeholderRe = regexp.MustCompile(`^"\\u0000;[0-9]+"$`)

// eachToken calls str with the bounds of each string of the JSON text s,
// in order, and whether it is a key, and num with the bounds of each
// number; false when a string does not end. Outside a string, a digit
// or a minus sign can only begin a number (true, false and null hold
// none), which runs over the characters a number is written in.
func eachToken(s string, str func(i, j int, key bool), num func(i, j int)) bool {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"':
			j := i + 1
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(s) {
				return false
			}
			str(i, j+1, strings.HasPrefix(strings.TrimLeft(s[j+1:], " \t\n\r"), ":"))
			i = j
		case c == '-' || c >= '0' && c <= '9':
			j := i + 1
			for j < len(s) && strings.IndexByte("0123456789+-.eE", s[j]) >= 0 {
				j++
			}
			num(i, j)
			i = j - 1
		}
	}
	return true
}

// heldToken is a time or a number held out of the layers: its text as
// the body had it, the part of it no form of a known value may cross
// the edge of (a time's text inside its quotes, a number whole), and
// whether it is a number.
type heldToken struct {
	text   string
	lo, hi int
	number bool
}

// holdOut is raw with each number, and each time under one of
// heldTimeKeys as that key's value, swapped for its placeholder, and the
// tokens, in the order they stood. raw is a JSON text (a marshalled
// body, or a record as written); one that does not end its strings, or
// that holds a string shaped as a placeholder, has nothing held.
func holdOut(raw string) (string, []heldToken) {
	var b strings.Builder
	var held []heldToken
	var key string
	at, clash := 0, false
	hold := func(i, j, lo, hi int, number bool) {
		b.WriteString(raw[at:i])
		b.WriteString(heldMark + strconv.Itoa(len(held)) + `"`)
		held = append(held, heldToken{raw[i:j], lo, hi, number})
		at = j
	}
	ended := eachToken(raw, func(i, j int, isKey bool) {
		s := raw[i:j]
		switch {
		case placeholderRe.MatchString(s):
			clash = true
		case isKey:
			key = s
		case heldTimeKeys[key] && heldTimeRe.MatchString(s) && strings.HasSuffix(strings.TrimRight(raw[:i], " \t\n\r"), ":"):
			hold(i, j, 1, j-i-1, false)
		}
	}, func(i, j int) { hold(i, j, 0, j-i, true) })
	if !ended || clash || len(held) == 0 {
		return raw, nil
	}
	b.WriteString(raw[at:])
	return b.String(), held
}

// putBack is out, the filter's bytes of a body holdOut held tokens out
// of, with each placeholder replaced by its token; false when the
// placeholders are not each where the filter found it, in order (one
// gone, or the body withheld), or when a form of a known value stands
// in the bytes it makes across a held token's edge: one made of a
// token's last characters and the JSON after them, or the JSON before
// it and its first, which the filter never saw side by side, or one
// that holds a time's quote. A form wholly inside a time's text or a
// number is the token's own, and stays (rule 7), but in a number when
// the same key's value was replaced elsewhere in the body (replaced:
// by this filter, or by an earlier pass, its marker in the body or its
// key in a nested redacted_env):
// the number whole beside that marker would show the value by
// difference, where the body was withheld (a time's marker said as
// much). These
// are the bytes the filter returns: what redactJSON's hoist and the
// stream's splices do after is rule 2's.
func (r *Redactor) putBack(out string, held []heldToken, replaced map[string]bool) (string, bool) {
	var b strings.Builder
	type span struct {
		lo, hi int
		number bool
	}
	spans := make([]span, 0, len(held))
	n, at, misplaced := 0, 0, false
	ended := eachToken(out, func(i, j int, _ bool) {
		if !placeholderRe.MatchString(out[i:j]) {
			return
		}
		if misplaced || n == len(held) || out[i:j] != heldMark+strconv.Itoa(n)+`"` {
			misplaced = true
			return
		}
		b.WriteString(out[at:i])
		spans = append(spans, span{b.Len() + held[n].lo, b.Len() + held[n].hi, held[n].number})
		b.WriteString(held[n].text)
		at, n = j, n+1
	}, func(int, int) {})
	if !ended || misplaced || n != len(held) {
		return "", false
	}
	b.WriteString(out[at:])
	s := b.String()
	for _, sec := range r.secrets {
		elsewhere := false
		for _, k := range sec.keys {
			elsewhere = elsewhere || replaced[k]
		}
		// The spans are in order, as the occurrences are found: the
		// first span that ends past an occurrence's start is the only
		// one it can begin in or before, and the spans it can reach
		// follow it.
		first := 0
		for from := 0; ; {
			i := strings.Index(s[from:], sec.text)
			if i < 0 {
				break
			}
			start, end := from+i, from+i+len(sec.text)
			for first < len(spans) && spans[first].hi <= start {
				first++
			}
			for _, sp := range spans[first:] {
				if sp.lo >= end {
					break
				}
				if start < sp.lo || end > sp.hi || sp.number && elsewhere {
					return "", false
				}
			}
			from = start + 1
		}
	}
	return s, true
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
