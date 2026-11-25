package liveswap

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/smallhoursorg/motel/liveswap/deploytrust"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// An app that prints one of its env values must not reach a reader with
// it: a failed deploy's answer lands in a CI log, a status is read by
// people with a role. motel rendered the app's environment from its
// env_file, so it knows the values by name; and it knows which strings
// of an answer are the app's, because the app's bytes enter an answer or
// a stored record at a few places only, the doors:
//
//   - a failed deploy's journal tail, probe body and probe Location
//     (failureDetail, app.go);
//   - a line of the app's journal at the logs read (motel_logs.go).
//
// The text is filtered there, once, as text, before any cap cuts it,
// and lands in a field of type wire.AppText. Go refuses a string
// variable in such a field, so one gets there by a conversion or a
// decode; a conversion is written in this file alone, which
// TestAppTextOnlyFromTheDoor holds for the forms a walk of the source
// sees, and the rest is review's. An answer is then marshalled from its
// typed struct and sent: no filter runs over a finished body, so
// motel's own data (a key, a word of its vocabulary, a version, a time,
// a number, the app's name) is never replaced, and a body is never
// withheld. Every other string field of an answer is classed as
// moteld's own or as text that passes as written (the caller's, the
// operator's, another program's, an error's text as motel wrote it):
// the wire package's `text` tags, held by TestEveryStringFieldIsClassed.
// An error's text is not searched: the filter is for an app that prints
// a value by accident, and an error that quotes one (a file the app
// named after a value, a probe answer that is no HTTP, a redirect whose
// Location does not parse, a malformed env_file line) is accepted as
// sent (DESIGN-threat-model.md).
//
// The layers, over one text:
//
//  1. Known values: every env_file value of secretMinLen or more, in
//     the spellings secretForms lists (as written; JSON-escaped as Go
//     writes it; either with `/` as `\/`, `+` as `\u002B` or `=` as
//     `\u003d`, as other runtimes' encoders write them, and each of
//     those escaped once more; base64 in both alphabets padded and
//     not, hex, URL-escaped) and a URL value's password on its own, in
//     the same spellings. Replaced with [redacted:KEY]; the
//     keys go to the answer's redacted_env, carried from the door
//     beside the text. Inline `env` values are not secrets by policy
//     (the Caddyfile is in a repo); motel's SOCKET, the sandbox HOME
//     and the inherited PATH are paths a diagnostic needs, and none is
//     an env_file value (a HOME or PATH the env_file sets is one).
//  2. Shape rules (shapeRules, basicCredentialRe): ten credential forms
//     an ordinary log line does not hold, each replaced by its name (a
//     PEM private key, a JWT, a bearer, basic or token credential of 20
//     characters or more, a URL's userinfo, an AWS key id, the two
//     GitHub token forms, a Slack token, a Stripe key, a Basic
//     credential that decodes to user:password).
//
// Layer 1 runs again last, since a rule's marker text could spell a
// known value. A secret that is in no env_file is hidden only when it
// has one of the ten shapes: a URL's query, a `password=` assignment
// and a generated-looking token pass as written, as request ids,
// hashes and trace ids do (the owner's decision of 2026-11-22). What
// else passes is documented in DESIGN-threat-model.md: an encoding not
// in the list, a value under secretMinLen, a value split across two
// lines.
//
// The box webhook (box/validate.go, box/diff.go) and notify
// (notify/engine.go) build filters of their own with NewRedactor and
// call Redact on a text where it enters: doors of theirs.

const secretMinLen = 8

// Redactor is the filter: the layers above, over one set of known
// values. Exported, with NewRedactor and Redact, for the box webhook
// and notify; a nil *Redactor is the shape rules alone.
type Redactor struct {
	// secrets holds every form of every known value, longest first so
	// a value that contains another is replaced whole.
	secrets []*secretForm
}

// secretForm is one text to replace, the keys whose values it belongs
// to (two keys can hold the same value), and the marker that replaces
// it, chosen so that no form of any value is a substring of it.
type secretForm struct {
	text   string
	keys   []string
	marker string
}

// NewRedactor takes env_file KEY=VALUE pairs. A nil *Redactor is
// usable: the shape rules only.
//
// The box webhook builds its own: the pairs are the caller's choice,
// as an app's are liveswap's — an app's filter is primed with env_file
// alone, and motel's SOCKET, the sandbox HOME and the inherited PATH
// stay readable because a diagnostic needs its paths (a HOME or PATH
// the env_file sets is one of its values) — so a caller priming with a
// process environment (the box, whose `caddy validate` quotes expanded
// values in its errors) will want the same for the variables that are
// paths. Build one and keep it: every form of every value is computed
// here, and a running process's environment does not change.
func NewRedactor(envFile []string) *Redactor {
	r := &Redactor{}
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
				sf = &secretForm{text: form}
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

// secretForms is every spelling of a value the filter recognises: as
// written and as Go's encoding/json writes it in a string; those two
// with `/` as `\/` (PHP's json_encode, Ruby's script_safe), `+` as
// `\u002B` (.NET's default JSON writer) and `=` as `\u003d` (Gson's
// default); each of these escaped once more, as a JSON line logged
// inside another string is (a backslash and a quote), and that once
// more with `/` as `\/` (PHP's outer level); then base64 in both
// alphabets padded and not, hex, and URL-escaped. Measured on 38
// encoders of seven runtimes (TestRedactorRuntimeEscapes).
func secretForms(v string) []string {
	forms := []string{v}
	if j, err := json.Marshal(v); err == nil {
		if s := string(j[1 : len(j)-1]); s != v {
			forms = append(forms, s)
		}
	}
	// Each range reads forms as it stood before the loop; a form that
	// holds none of the character is the one it came from again, and
	// NewRedactor keeps one of each text.
	for _, s := range forms {
		forms = append(forms,
			strings.ReplaceAll(s, "/", `\/`),
			strings.ReplaceAll(s, "+", `\u002B`),
			strings.ReplaceAll(s, "=", `\u003d`),
		)
	}
	for _, s := range forms {
		s = escapeOnceMore.Replace(s)
		forms = append(forms, s, strings.ReplaceAll(s, "/", `\/`))
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

// escapeOnceMore is what Go's and most JSON encoders do to a string
// they write inside another: a backslash and a quote escaped (.NET's
// default writes the quote as \u0022, which is not listed).
var escapeOnceMore = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

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
	// Provider prefixes.
	{regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`), "[redacted:aws-key]"},
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}\b`), "[redacted:github-token]"},
	{regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,}\b`), "[redacted:github-token]"},
	{regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}\b`), "[redacted:slack-token]"},
	{regexp.MustCompile(`\b[sr]k_(?:live|test)_[A-Za-z0-9]{16,}\b`), "[redacted:stripe-key]"},
}

// basicCredentialRe finds "Basic <base64>"; the decode check is in
// shapes, since a regexp cannot tell a credential from a word. Four
// characters is the shortest base64 of a user:pass ("a:b").
var basicCredentialRe = regexp.MustCompile(`(?i)\bbasic[ \t]+[A-Za-z0-9+/]{4,}={0,2}`)

// replaceKnown is layer 1 on its own: every known form replaced by its
// marker, longest first, and the keys found added to seen.
func (r *Redactor) replaceKnown(s string, seen map[string]bool) string {
	if r == nil {
		return s
	}
	for _, sec := range r.secrets {
		if !strings.Contains(s, sec.text) {
			continue
		}
		s = strings.ReplaceAll(s, sec.text, sec.marker)
		for _, k := range sec.keys {
			seen[k] = true
		}
	}
	return s
}

func reportedKeys(seen map[string]bool) []string {
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Redact filters a text and reports which env keys' values it found.
// Layer 1 runs first and again last: a shape rule's own marker text
// ("[redacted:private-key]") can contain a known value, and only a
// pass over the finished text guarantees none remains.
func (r *Redactor) Redact(s string) (string, []string) {
	seen := map[string]bool{}
	s = r.filter(s, seen)
	s = r.replaceKnown(s, seen)
	return s, reportedKeys(seen)
}

// filter is layers 1 and 2 in order.
func (r *Redactor) filter(s string, seen map[string]bool) string {
	return shapes(r.replaceKnown(s, seen))
}

// door is an app's filter at a place the app's bytes enter: the app's
// known values and the keys found so far, for the answer's
// redacted_env.
type door struct {
	r    *Redactor
	seen map[string]bool
	// unread, when set, is why the app's values are unknown to the
	// filter (its env_file could not be read after a restart or a
	// reload): the app's text is withheld, each text alone, and the
	// rest of the answer is sent.
	unread string
}

// text is s, which the app wrote, as an answer may carry it.
func (d *door) text(s string) wire.AppText { return wire.AppText(d.mixed(s)) }

// excerpt is text cut to max runes, filtered whole before the cut: a
// cut that fell inside a value would leave a fragment no layer matches.
func (d *door) excerpt(s string, max int) wire.AppText {
	return wire.AppText(deploytrust.CutRunes(d.mixed(s), max))
}

// logLine is one journal line's text through the door, at most
// logsLineMax bytes of it sent, with what is read of a long line and
// what is sent of it as the logs read has always had them (#635). The
// filter sees the text cut to logsLineMax and a margin, the longest
// form it knows or logsCutMargin if more, so every value or credential
// that begins inside what is sent is whole when it is looked for (A).
// Where that cut fell, a value or credential it split is a piece no
// layer can know; it begins in the margin, past logsLineMax, unless it
// is a credential longer than the margin. So the filter also sees the
// text cut at logsLineMax (B), and what is kept is the start A and B
// have in common. A piece the first cut split is past B's input, so
// not in it; one the second cut split is whole in A and replaced there,
// so A and B part at it; a credential longer than the margin is raw in
// both, and its start is sent (DESIGN-access.md, the accepted row on a
// credential over 8 KiB on one line). Then what is left is made valid
// UTF-8 and cut to logsLineMax with "…" (a marker or plain text may be
// cut there, nothing a layer would have replaced). The keys A replaced
// are the door's. A door whose env_file could not be read sends its
// withheld sentence for the line, and an empty line empty.
func (d *door) logLine(text string) wire.AppText {
	if d.unread != "" {
		return wire.AppText(d.mixed(text))
	}
	longest := logsCutMargin
	if len(d.r.secrets) > 0 {
		longest = max(longest, len(d.r.secrets[0].text)) // longest first
	}
	pre := logsLineMax + longest
	cut := len(text) > pre
	if cut {
		text = cutText(text, pre)
	}
	out := d.mixed(text)
	if cut {
		b, _ := d.r.Redact(cutText(text, logsLineMax))
		out = commonStart(out, b)
	}
	// The app's bytes that are not UTF-8, each run one U+FFFD, only now:
	// the filter saw them as written. Before the last cut, so the text
	// sent stays within logsLineMax.
	out = strings.ToValidUTF8(out, "\uFFFD")
	if cut || len(out) > logsLineMax {
		out = cutText(out, logsLineMax-len("…")) + "…"
	}
	return wire.AppText(out)
}

// lines is a journal tail through the door, each line on its own, as
// the logs read filters each: a rule that reaches across white space
// (a bearer token) then never joins two lines or masks the next line's
// first word, and the tail keeps its count of lines.
func (d *door) lines(lines []string) []wire.AppText {
	if len(lines) == 0 {
		return nil
	}
	out := make([]wire.AppText, len(lines))
	for i, l := range lines {
		out[i] = wire.AppText(d.mixed(l))
	}
	return out
}

// mixed is s through the whole filter: layers 1 and 2, then layer 1
// again.
func (d *door) mixed(s string) string {
	if s == "" {
		return ""
	}
	if d.unread != "" {
		return "[withheld: " + d.unread + "]"
	}
	return d.r.replaceKnown(d.r.filter(s, d.seen), d.seen)
}

// keys is the env keys whose values the door replaced, sorted; nil
// when none. A key's name that is a form of another key's value is that
// value's marker in the list, as in any text: a list of names must not
// spell a value.
func (d *door) keys() []string {
	if len(d.seen) == 0 {
		return nil
	}
	keys := reportedKeys(d.seen)
	for i, k := range keys {
		keys[i] = d.r.replaceKnown(k, map[string]bool{})
	}
	return keys
}

// shapes is layer 2.
func shapes(s string) string {
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
	return s
}
