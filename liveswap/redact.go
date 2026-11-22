package liveswap

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"go.uber.org/zap"

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
//   - a line of the app's journal at the logs read (motel_logs.go);
//   - an error's text, wherever it becomes a field (errText below): Go's
//     HTTP client quotes a probe answer that is no HTTP into its error,
//     and the operating system names paths the app made (a cleanup that
//     failed, a pre-flight that refused a link in a release);
//   - a lifecycle event's summary, as it is told (lifecycle.go).
//
// The text is filtered there, once, as text, before any cap cuts it,
// and lands in a field whose type says so: wire.AppText for the app's
// text, wire.ErrText for an error's. Go refuses a string variable in
// such a field, so one gets there by a conversion or a decode. A
// conversion is written in this file alone (and wire.OwnText's), which
// TestAppTextOnlyFromTheDoor and TestErrTextMakers hold for the forms a
// walk of the source sees: the type named under any import name, in
// parentheses, through a declared type or an alias, or instantiating a
// generic function. A decode (json.Unmarshal into such a field) makes
// one with nothing written, and only review sees it. An answer is then
// marshalled from its typed struct and sent: no filter runs over a
// finished body, so motel's own data (a key, a word of its vocabulary,
// a version, a time, a number, the app's name) is never replaced, and a
// body is never withheld. Every other string field of an answer is
// classed as moteld's own or as text that passes as written (the
// caller's, the operator's, another program's): the wire package's
// `text` tags, held by TestEveryStringFieldIsClassed.
//
// The layers, over one text:
//
//  1. Known values: every env_file value of secretMinLen or more, in
//     the forms it could appear in (as written, JSON-escaped, base64 in
//     both alphabets padded and not, hex, URL-escaped) and a URL
//     value's password on its own. Replaced with [redacted:KEY]; the
//     keys go to the answer's redacted_env, carried from the door
//     beside the text.
//  2. A safe list (the app's name, the versions and digests in play,
//     the app's dirs), exempt from the heuristics below. Never from
//     layer 1: a safe string equal to a known value is dropped.
//  3. Shape rules: credentials with a recognisable form.
//  4. Entropy: a generated-looking token of entropyMinLen or more.
//
// Layers 3 and 4 are best effort. Layer 1 runs again last, since a
// heuristic's marker text could spell a known value. What passes all
// four is documented in DESIGN-threat-model.md: an encoding not in the
// list, a value under secretMinLen, a value split across two lines.
//
// The box webhook (box/validate.go, box/diff.go) and notify
// (notify/engine.go) build filters of their own with NewRedactor and
// call Redact on a text where it enters: doors of theirs.

const (
	secretMinLen  = 8
	entropyMinLen = 20
	// Bits per character. Random base64 is ~6, random hex ~4; English
	// identifiers sit around 3.5–4 and never clear the base64 bar.
	entropyBase64 = 4.5
	entropyHex    = 3.0
)

// Redactor is the filter: the layers above, over one set of known
// values and safe strings. Exported, with NewRedactor and Redact, for
// the box webhook and notify; a nil *Redactor is layers 3 and 4 alone.
type Redactor struct {
	// secrets holds every form of every known value, longest first so
	// a value that contains another is replaced whole.
	secrets []*secretForm
	// safeExact is the safe strings as given, none equal to a known
	// value (rule 1): held out of the heuristics as spans. safeTokens
	// is those strings and every layer-4 token inside them: exempt
	// from the entropy heuristic.
	safeExact, safeTokens map[string]bool
}

// secretForm is one text to replace, the keys whose values it belongs
// to (two keys can hold the same value), and the marker that replaces
// it, chosen so that no form of any value is a substring of it.
type secretForm struct {
	text   string
	keys   []string
	marker string
}

// NewRedactor takes env_file KEY=VALUE pairs and the strings that must
// survive the heuristics. A safe string equal to a value is dropped:
// it exempts nothing. A nil *Redactor is usable: layers 3 and 4 only.
// Build one per set of safe strings and keep it where the values do
// not change (the box webhook's): every form of every value is
// computed here.
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

// Redact filters a text and reports which env keys' values it found.
// Layer 1 runs first and again last: the heuristic layers' own marker
// text ("[redacted:private-key]") can contain a known value, and only
// a pass over the finished text guarantees none remains.
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

// errText is the error net, and with wire.OwnText the only way a
// string that is not a constant becomes a wire.ErrText: s is an
// error's text, or a sentence of motel's with the operating system's or
// a library's words in it, on its way to a field of an answer, a
// deploy's record or the watchdog's last_failure. It passes the app's
// door as text, always: an error can quote what the app answered a
// probe with, and can name a path the app made, whichever step failed.
// The keys the door replaced come back for redacted_env.
//
// read is door's: whether the env_file may be read now (not for a
// deploy whose own launch rendered it). names are names the heuristics
// leave alone (the version, the digests the error names).
func (ma *managedApp) errText(s string, read bool, names ...string) (wire.ErrText, []string) {
	if s == "" {
		return "", nil
	}
	d := ma.door(read, names...)
	return d.errText(s), d.keys()
}

// netError is err through the error net (errText), for the error a
// deploy or a probe ends with, whose text the answer, last_deploy, the
// record and moteld's journal line all then carry. When the door could
// not read the env_file, the text is the filter's sentence, saying that
// the error is in moteld's journal, and the journal line names the
// error and the env_file's own read error beside it (logFields).
//
// beforeLaunch is a deploy's error from before its launch rendered the
// env_file (a download, an extraction, the file's own parse): then an
// env_file that is not there holds no values to know beyond those this
// process remembers from its launches, and the error passes the filter
// with those (main's record filter held the same), provided it
// remembers some. A process that remembers none (a restart, then the
// file gone) may be reattached to an instance launched from the file,
// whose values it does not know: the text is withheld, as it is
// anywhere else a file is gone (a probe of a running app).
func (ma *managedApp) netError(err error, read, beforeLaunch bool, names ...string) *doorError {
	d := ma.door(read, names...)
	if beforeLaunch && errors.Is(d.unreadErr, fs.ErrNotExist) && len(d.r.secrets) > 0 {
		d.unread, d.unreadErr = "", nil
	}
	de := &doorError{err: err, msg: d.errText(err.Error()), keys: d.keys(), unread: d.unreadErr}
	if de.unread != nil {
		de.msg = "[withheld: " + wire.ErrText(d.unread) + "; moteld's journal has this error]"
	}
	return de
}

// logFields is what moteld's journal line about err says of it: its
// text, which passed the error net, and when the net could not read
// the app's env_file, the error as it came and the file's read error.
// The journal is the operator's, which an older moteld gave every
// error as it came.
func logFields(err error) []zap.Field {
	fields := []zap.Field{zap.Error(err)}
	if de, ok := err.(*doorError); ok && de.unread != nil { //nolint:errorlint // the outermost error alone: the one netError made
		fields = append(fields, zap.NamedError("cause", de.err), zap.NamedError("env_file", de.unread))
	}
	return fields
}

// errText is the net for a place that holds the app's door already.
func (d *door) errText(s string) wire.ErrText { return wire.ErrText(d.quoted(s)) }

// door is an app's filter at a place the app's bytes enter: the app's
// known values, the names its text may hold, and the keys found so far,
// for the answer's redacted_env.
type door struct {
	r    *Redactor
	seen map[string]bool
	// unread, when set, is why the app's values are unknown to the
	// filter (its env_file could not be read after a restart or a
	// reload): the app's text is withheld, each text alone, and the
	// rest of the answer is sent. unreadErr is the read's error, for
	// moteld's journal (doorError).
	unread    string
	unreadErr error
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
// that begins inside what is sent is whole when it is looked for (A);
// a line is at most 48 KiB as journald stores it, so the filter reads
// at most 12 KiB of one. Where that cut fell, a value or token it split
// is a piece no layer can know; it begins in the margin, past
// logsLineMax, unless it is a token longer than the margin. So the
// filter also sees the text cut at logsLineMax (B), and what is kept is
// the start A and B have in common, less a run of layer 4's alphabet at
// its end. A piece the first cut split is past B's input, so not in it;
// one the second cut split is whole in A and replaced there, so A and B
// part at it; a token longer than the margin, raw in both, is the run
// that goes. Then what is left is made valid UTF-8 and cut to
// logsLineMax with "…" (a marker or plain text may be cut there,
// nothing a layer would have replaced). The keys A replaced are the
// door's. A door whose env_file could not be read sends its withheld
// sentence for the line, and an empty line empty.
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
	// journald ends a stdout line at a NUL, so none is in one; a NUL is
	// what the safe list's placeholders are made of (protectSafe).
	text = strings.ReplaceAll(text, "\x00", "\uFFFD")
	out := d.mixed(text)
	if cut {
		b, _ := d.r.Redact(cutText(text, logsLineMax))
		out = strings.TrimRight(commonStart(out, b), tokenAlphabet)
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
// (a bearer token, an assignment) then never joins two lines or masks
// the next line's first word, and the tail keeps its count of lines.
// A PEM block printed a line at a time is no block to the shape rule;
// its lines are long base64 runs the entropy layer takes.
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

// again is det, a failed deploy's detail an older moteld stored, with
// its app text and its error text through the door now (servedRecord,
// deploys.go). Its exit is moteld's own words and passes as it is.
func (d *door) again(det deployDetail) deployDetail {
	tail := make([]string, len(det.LogTail))
	for i, l := range det.LogTail {
		tail[i] = string(l)
	}
	det.LogTail = d.lines(tail)
	det.LogTailError = d.errText(string(det.LogTailError))
	if det.Probe != nil {
		p := *det.Probe
		p.Location, p.Body = d.text(string(p.Location)), d.text(string(p.Body))
		det.Probe = &p
	}
	return det
}

// mixed is s through the whole filter: layers 1 to 4, then layer 1
// again. s is the app's text, or a string of motel's that may quote the
// app's bytes or name a path the app made (an error); motel's own words
// in such a string meet the app's values too.
func (d *door) mixed(s string) string {
	if s == "" {
		return ""
	}
	if d.unread != "" {
		return "[withheld: " + d.unread + "]"
	}
	return d.r.replaceKnown(d.r.filter(s, d.seen), d.seen)
}

// goQuotedRe is a run Go's %q wrote: a quote, escapes, a quote.
var goQuotedRe = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

// quoted is mixed for a text in which Go may have quoted the app's
// bytes (the HTTP client's "malformed HTTP response %q"): each quoted
// run that holds an escape is also filtered as the bytes it quotes, the
// app's text as the app sent it, and written quoted again when the
// filter changed them. Without it a value the app sent JSON-escaped
// arrives escaped twice, a form layer 1 does not list.
func (d *door) quoted(s string) string {
	s = goQuotedRe.ReplaceAllStringFunc(s, func(q string) string {
		raw, err := strconv.Unquote(q)
		if err != nil || raw == q[1:len(q)-1] {
			return q // nothing escaped: the pass below sees the same bytes
		}
		if out := d.mixed(raw); out != raw {
			return strconv.Quote(out)
		}
		return q
	})
	return d.mixed(s)
}

// keys is the env keys whose values the door replaced, sorted; nil
// when none.
func (d *door) keys() []string {
	if len(d.seen) == 0 {
		return nil
	}
	return reportedKeys(d.seen)
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
