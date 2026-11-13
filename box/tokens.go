package box

import (
	"bytes"
	"regexp"
	"slices"
	"strings"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"

	"github.com/smallhoursorg/motel/box/proof"
)

// Shape is what the box needs to know about a Caddyfile, and all it
// reads from one: the one `box_webhook` site's host and the `box`
// block's signers. It is read from the raw tokens, never from the
// adapted configuration — which is what makes the reading safe to do
// as root on pushed input, and the same whoever does it: `init`, the
// handler's pre-check, the applier and `motel box webhook` cannot
// disagree about which address a file names or who may sign for it.
type Shape struct {
	// Host is the address of the one site carrying box_webhook, which
	// the rules below require to be exactly one bare hostname; lower
	// case, as the box's identity is compared.
	Host string
	// Signers are the box block's signer lines, in file order.
	Signers proof.Signers
	// Apps are the names of the global liveswap block's `app` lines, in
	// file order, as a result reports them; a name outside liveswap's
	// app-name grammar is left out, since liveswap refuses the file at
	// load for it anyway. Read from the first reading only: no proof
	// decides on them, only secretsFit's checks for mistakes.
	Apps []string
	// AppEnv is what each app's block says about its environment, by
	// app name, for every name in Apps: what secretsFit checks against
	// the secrets beside the file. Read like Apps, as raw tokens: an
	// env_file written with a placeholder is that text, not a path, and
	// a placeholder's default that re-shapes the file once expanded is
	// read as written — secretsFit catches mistakes and guards nothing,
	// so it does not hold the two readings to agree, as Walk does for
	// the host and the signers.
	AppEnv map[string]AppEnv
	// Notify is the global monitor block, nil for none: where its notify
	// lines read the box's own secrets from, for secretsFit. Read like
	// AppEnv.
	Notify *NotifyBlock
}

// NotifyBlock is what the box reads of the global monitor block: its
// notify lines (notify/DESIGN-notify.md).
type NotifyBlock struct {
	// Channels is whether a channel line is there: a monitor block that
	// is a metrics line alone runs no notify, which reads no secret.
	Channels bool
	// SecretsFile is the secrets_file line's argument, "" for none (the
	// default, the file the box writes from secrets/_box/).
	SecretsFile string
	// Secrets are the names the channel and heartbeat lines read, each
	// written {secret.<NAME>}, in file order: what a push must carry in
	// secrets/_box/. A URL written any other way is notify's to refuse.
	Secrets []string
}

// notifySecret is the name a channel or heartbeat line's
// {secret.<NAME>} argument reads, if it is one notify takes (boxVar):
// a name it refuses is validate's to name, with notify's own reason.
func notifySecret(arg string) (string, bool) {
	name, ok := strings.CutPrefix(arg, "{secret.")
	if !ok {
		return "", false
	}
	name, ok = strings.CutSuffix(name, "}")
	return name, ok && boxVar.MatchString(name)
}

// AppEnv is an app block's env_file and env lines.
type AppEnv struct {
	// EnvFile is the env_file line's argument, "" for none.
	EnvFile string
	// Env are the env lines' keys, in file order.
	Env []string
}

func (e AppEnv) hasEnv(key string) bool { return slices.Contains(e.Env, key) }

// appName is liveswap's app-name grammar (liveswap/names.go, appNameRe).
var appName = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)

// Refusal is why a Caddyfile cannot be a box's: one line beginning
// with a verb, which the caller prefixes with the file's name — "the
// new Caddyfile has no box block". The messages are the ones
// DESIGN-box.md "Refusals" lists.
type Refusal struct {
	Reason string
}

func (r *Refusal) Error() string { return "the Caddyfile " + r.Reason }

func refuse(reason string) error { return &Refusal{Reason: reason} }

// reasonNoSigner is the walk's refusal of a box block with no signer
// line, which check and `GET /` tell apart for the installed file:
// there it is a refusal of its own, "lists no signer" (runningRefused,
// check.go).
const reasonNoSigner = "has no signer"

// Walk reads a Caddyfile the way the box reads one (DESIGN-box.md,
// "Reading the signed file"): caddyfile.Tokenize and a brace-depth
// walk, which expands nothing and follows nothing. It refuses the
// file if an `import` token stands at directive position anywhere; if
// a token at directive position, in a site address or anywhere inside
// the `box` block contains `{$`; if a site is written without braces
// (Caddy allows one, whose directives would then sit at depth zero);
// if there is no `box` block, no `signer`, no `deploy_trust` block
// with a line in it; if `box_webhook` stands anywhere but directly in
// a site block; or if the sites carrying it are not exactly one, with
// exactly one address that is a bare hostname.
//
// The walk runs twice: on the raw bytes, and on the bytes after
// Caddy's own placeholder expansion with an empty environment, because
// Caddy expands `{$NAME:default}` before it tokenizes and a default can
// carry a quote or a newline that re-shapes the file. Both readings
// must pass and agree on the host and the signers.
func Walk(input []byte) (*Shape, error) {
	if len(bytes.TrimSpace(input)) == 0 {
		return nil, refuse("is empty")
	}
	raw, err := walk(input)
	if err != nil {
		return nil, err
	}
	expanded, err := walk(expandEmptyEnv(input))
	if err != nil {
		return nil, err
	}
	if raw.Host != expanded.Host || !sameSigners(raw.Signers, expanded.Signers) {
		return nil, refuse("reads differently once its placeholders are expanded")
	}
	return raw, nil
}

func sameSigners(a, b proof.Signers) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Principal != b[i].Principal || a[i].Type != b[i].Type || a[i].B64 != b[i].B64 {
			return false
		}
	}
	return true
}

// blockKind is what a block is to the walk.
type blockKind int

const (
	kindOther    blockKind = iota // a nested block of a directive, or an option's
	kindGlobal                    // the key-less top-level block
	kindBox                       // the global block's `box` block
	kindTrust                     // the box block's `deploy_trust` block
	kindSite                      // a top-level block with addresses
	kindSnippet                   // a top-level `(name)` block
	kindLiveswap                  // the global block's `liveswap` block
	kindApp                       // an `app <name>` block in the liveswap block
	kindNotify                    // the global block's `monitor` block
)

// frame is one open brace.
type frame struct {
	kind  blockKind
	inBox bool  // every token inside the box block is held to the placeholder rule
	site  *site // the site this block is inside, if any
	// directive is the first token of the line that opened this block:
	// the name a refusal gives for where `box_webhook` was found.
	directive string
	// snippet is the `(name)` or `&(name)` block this frame is inside,
	// at any depth, if any.
	snippet *site
	// app is the app a kindApp block is for.
	app string
}

type site struct {
	addresses []string
	webhook   bool
}

// walk is one reading of the file.
func walk(input []byte) (*Shape, error) {
	tokens, err := caddyfile.Tokenize(input, "Caddyfile")
	if err != nil {
		// The lexer quotes input in its errors (a heredoc marker, a
		// body line), so the text is bounded like any other.
		return nil, refuse("does not tokenize: " + proof.Bound(err.Error()))
	}
	var (
		shape    Shape
		stack    []frame
		sites    []*site
		blocks   int // top-level blocks opened, in order
		boxes    int
		hasTrust bool
		pending  []string // addresses of a header continued by a trailing comma
	)
	lines := splitLines(tokens)
	for _, line := range lines {
		if len(stack) == 0 {
			// A block header: addresses, then `{` as the last token, or
			// no block at all. `import` here is Caddy's top-level import.
			if line[0].Text == "import" { // on a continued address line too: Caddy's addresses() checks every new line
				return nil, importRefusal(line)
			}
			if isClose(line[0]) {
				return nil, refuse("does not parse: a } with no block open")
			}
			addrs, opens, more, err := header(line, pending)
			if err != nil {
				return nil, err
			}
			if more {
				pending = addrs
				continue
			}
			pending = nil
			if !opens {
				// Caddy allows one brace-less site, whose directives
				// then sit at depth zero where this walk would read
				// them as addresses; every site here is braced.
				return nil, refuse("has a site without braces (" + proof.Bound(strings.Join(addrs, ", ")) + ")")
			}
			f := frame{kind: kindOther}
			blocks++
			switch {
			case len(addrs) == 0:
				// Caddy reads only the first top-level block as the
				// global options; a later key-less block it refuses.
				if blocks > 1 { // which also means there is at most one
					return nil, refuse("has a global options block that is not the first block")
				}
				f.kind = kindGlobal
			case len(addrs) == 1 && isSnippetOrNamedRoute(addrs[0]):
				f.kind, f.snippet = kindSnippet, &site{addresses: addrs}
			default:
				s := &site{addresses: addrs}
				sites = append(sites, s)
				f.kind, f.site = kindSite, s
			}
			stack = append(stack, f)
			continue
		}

		// Inside a block. The line's first token is at directive
		// position unless it is `}`; `{` may only end a line; `}` may
		// stand anywhere on one.
		top := stack[len(stack)-1]
		first := line[0]
		if !isClose(first) {
			if first.Text == "import" {
				return nil, importRefusal(line)
			}
			if strings.Contains(first.Text, "{$") {
				return nil, placeholderRefusal(first.Text)
			}
		}
		if top.inBox {
			for _, t := range line {
				if strings.Contains(t.Text, "{$") {
					return nil, placeholderRefusal(t.Text)
				}
			}
		}
		opens := false
		args := []string{}
		for i, t := range line {
			switch {
			case isOpen(t):
				if i != len(line)-1 || isClose(first) {
					return nil, refuse("does not parse: a { that does not end a directive's line")
				}
				opens = true
			case isClose(t):
				if len(stack) == 0 {
					return nil, refuse("does not parse: a } with no block open")
				}
				// Caddy closes a nested block on a directive's line, but
				// a top-level block only on a line of its own.
				if i != 0 && len(stack) == 1 {
					return nil, refuse("does not parse: a } that closes a top-level block on a directive's line")
				}
				stack = stack[:len(stack)-1]
				if len(stack) == 0 && i != len(line)-1 {
					return nil, refuse("does not parse: a token follows the } that closes a block")
				}
			default:
				if i > 0 {
					args = append(args, t.Text)
				}
			}
		}
		if isClose(first) {
			continue
		}
		if first.Text == "box_webhook" {
			switch {
			case top.snippet != nil:
				// A snippet's or a named route's body is directives to
				// Caddy, but it is not a site: a `box_webhook` there, at
				// any depth, would be the webhook only through an `import`
				// or an `invoke`, which the walk does not follow.
				return nil, refuse("has box_webhook inside a snippet or named route (" + proof.Bound(top.snippet.addresses[0]) + "); write it in the site")
			case top.kind == kindSite:
				// A matcher is an argument to Caddy: `box_webhook /x`
				// adapts, and leaves / unserved. The directive refuses
				// any argument too (caddyfile.go); the walk refuses it
				// here so the box never counts a webhook Caddy would
				// not serve on /.
				if len(args) > 0 {
					return nil, refuse("has box_webhook with arguments (" + proof.Bound(strings.Join(args, " ")) + "); it takes none, and a matcher would leave / unserved")
				}
				top.site.webhook = true
			default:
				// Only a direct child of the site block counts: under a
				// matched `handle /x`, a `route`, a `handle_response` that
				// runs only on an upstream response, or in a block where
				// the first token is a field, the webhook could not answer
				// on `/` — and a file that passed with one there would cut
				// the channel at its reload. Refused by name, never
				// ignored, so a live webhook can never go uncounted.
				where := top.directive
				if where == "" {
					where = "the global options"
				}
				return nil, refuse("has box_webhook inside " + proof.Bound(where) + "; it must stand directly in the site block, where no matcher or nested route can leave / unserved")
			}
		}
		child := frame{kind: kindOther, inBox: top.inBox, site: top.site, snippet: top.snippet, directive: first.Text}
		switch top.kind {
		case kindGlobal:
			if first.Text == "box" {
				boxes++
				if boxes > 1 {
					return nil, refuse("has more than one box block")
				}
				child.kind, child.inBox = kindBox, true
			}
			if first.Text == "liveswap" {
				child.kind = kindLiveswap
			}
			// The blocks the monitor block replaced, refused here by name
			// as motel validate refuses them (notify/caddyfile.go): the
			// walk would read no monitor block, and the slot's check
			// below would refuse the push in words that name neither.
			switch first.Text {
			case "notify":
				return nil, refuse("has a notify block, which is now `monitor { … }`, with the same lines but state_dir and alertmanager, whose paths are fixed; to move a box onto it, docs/upgrading.md, \"From the notify and metrics_store blocks\"")
			case "metrics_store":
				return nil, refuse("has a metrics_store block, which is now a `metrics [<socket path>]` line in the monitor block; to move a box onto it, docs/upgrading.md, \"From the notify and metrics_store blocks\"")
			}
			if first.Text == "monitor" && opens && shape.Notify == nil { // Caddy refuses a second
				shape.Notify = &NotifyBlock{}
				child.kind = kindNotify
			}
		case kindLiveswap:
			if first.Text == "app" && len(args) > 0 && appName.MatchString(args[0]) {
				shape.Apps = append(shape.Apps, args[0])
				if shape.AppEnv == nil {
					shape.AppEnv = map[string]AppEnv{}
				}
				if _, seen := shape.AppEnv[args[0]]; !seen { // liveswap refuses a second block of the same name
					shape.AppEnv[args[0]] = AppEnv{}
				}
				child.kind, child.app = kindApp, args[0]
			}
		case kindApp:
			e := shape.AppEnv[top.app]
			switch {
			case first.Text == "env_file" && len(args) > 0:
				e.EnvFile = args[0]
			case first.Text == "env" && len(args) > 0:
				e.Env = append(e.Env, args[0])
			}
			shape.AppEnv[top.app] = e
		case kindNotify:
			switch {
			case first.Text == "secrets_file" && len(args) > 0:
				shape.Notify.SecretsFile = args[0]
			case first.Text == "channel" && len(args) >= 3:
				shape.Notify.Channels = true
				if name, ok := notifySecret(args[2]); ok {
					shape.Notify.Secrets = append(shape.Notify.Secrets, name)
				}
			case first.Text == "heartbeat" && len(args) >= 1:
				if name, ok := notifySecret(args[0]); ok {
					shape.Notify.Secrets = append(shape.Notify.Secrets, name)
				}
			}
		case kindBox:
			switch first.Text {
			case "signer":
				if opens || len(args) != 3 {
					return nil, refuse("has a signer line that is not `signer <principal> <key-type> <base64>`")
				}
				s, err := proof.ParseSigner(args[0], args[1], args[2])
				if err != nil {
					return nil, refuse("has a bad " + err.Error())
				}
				shape.Signers = append(shape.Signers, s)
			case "deploy_trust":
				child.kind = kindTrust
			}
		case kindTrust:
			hasTrust = true // a line inside the deploy_trust block
		}
		if opens {
			stack = append(stack, child)
		}
	}
	if len(stack) != 0 {
		return nil, refuse("does not parse: a block is not closed")
	}
	if len(pending) != 0 {
		return nil, refuse("does not parse: a site address list ends with a comma")
	}
	if boxes == 0 {
		return nil, refuse("has no box block")
	}
	if len(shape.Signers) == 0 {
		return nil, refuse(reasonNoSigner)
	}
	if _, err := shape.Signers.AllowedSigners(); err != nil {
		return nil, refuse("has a bad " + err.Error())
	}
	if !hasTrust {
		return nil, refuse("has no deploy_trust")
	}
	var hooks []*site
	for _, s := range sites {
		if s.webhook {
			hooks = append(hooks, s)
		}
	}
	switch len(hooks) {
	case 0:
		return nil, refuse("has no site with box_webhook")
	case 1:
	default:
		return nil, refuse("has more than one site with box_webhook")
	}
	host, ok := BareHost(hooks[0].addresses)
	if !ok {
		return nil, refuse("has a box_webhook site whose address is not one bare hostname (" + proof.Bound(strings.Join(hooks[0].addresses, ", ")) + ")")
	}
	shape.Host = host
	return &shape, nil
}

// isSnippetOrNamedRoute is Caddy's test for a top-level block that is
// not a site: a single key `(name)` (a snippet) or `&(name)` (a named
// route, invoked from a site with `invoke`).
func isSnippetOrNamedRoute(key string) bool {
	return strings.HasSuffix(key, ")") && (strings.HasPrefix(key, "(") || strings.HasPrefix(key, "&("))
}

// header reads a top-level block's addresses from one line: tokens up
// to `{`, which must be the last; a trailing comma says the next line
// continues the list, and Caddy then expects an address, not `{`.
// Every address is held to the placeholder rule.
func header(line []caddyfile.Token, pending []string) (addrs []string, opens, more bool, err error) {
	addrs = pending
	more = len(pending) > 0 // a comma carried over from the line above
	for i, t := range line {
		if isOpen(t) {
			if i != len(line)-1 {
				return nil, false, false, refuse("does not parse: a token follows { on its line")
			}
			if more {
				return nil, false, false, refuse("does not parse: a site address list ends with a comma")
			}
			return addrs, true, false, nil
		}
		if strings.Contains(t.Text, "{$") {
			return nil, false, false, placeholderRefusal(t.Text)
		}
		v := t.Text
		more = false
		if strings.HasSuffix(v, ",") {
			v = strings.TrimSuffix(v, ",")
			more = true
		}
		if strings.Contains(v, ",") {
			return nil, false, false, refuse("does not parse: a comma inside a site address")
		}
		if v != "" {
			addrs = append(addrs, v)
		}
	}
	return addrs, false, more, nil
}

// isOpen and isClose are Caddy's own tests for a structural brace
// (caddyfile's isOpenCurlyBrace and isCloseCurlyBrace, v2.11.6): the
// text, and not quoted — `respond "{"` is a value, and so is a heredoc
// of one brace. `import`, by contrast, Caddy matches on the text alone,
// quoted or not, and so does the walk.
func isOpen(t caddyfile.Token) bool  { return t.Text == "{" && !t.Quoted() }
func isClose(t caddyfile.Token) bool { return t.Text == "}" && !t.Quoted() }

func importRefusal(line []caddyfile.Token) error {
	what := ""
	if len(line) > 1 {
		what = " " + proof.Bound(line[1].Text)
	}
	return refuse("imports" + what + "; inline the snippet")
}

func placeholderRefusal(token string) error {
	return refuse("has a placeholder where a directive name, a site address or a box line goes (" + proof.Bound(token) + ")")
}

// splitLines groups tokens by line the way Caddy's parser does
// (isNextOnNewLine): a token is on a new line when the previous token,
// counting the line breaks inside it, ended on an earlier line.
func splitLines(tokens []caddyfile.Token) [][]caddyfile.Token {
	var lines [][]caddyfile.Token
	for i, t := range tokens {
		if i == 0 || tokens[i-1].Line+tokens[i-1].NumLineBreaks() < t.Line {
			lines = append(lines, nil)
		}
		lines[len(lines)-1] = append(lines[len(lines)-1], t)
	}
	return lines
}

// BareHost is the one address, lowercased, if the list is exactly one
// bare hostname: labels of ASCII letters, digits and hyphens, joined by
// dots, no label beginning or ending with a hyphen, at most 253 bytes.
// A scheme, a port, a path, a wildcard, a placeholder or a second name
// is anything else.
func BareHost(addresses []string) (string, bool) {
	if len(addresses) != 1 {
		return "", false
	}
	h := addresses[0]
	if h == "" || len(h) > 253 {
		return "", false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
			default:
				return "", false
			}
		}
	}
	return strings.ToLower(h), true
}

// expandEmptyEnv is Caddy's replaceEnvVars (caddyconfig/caddyfile/
// parse.go, v2.11.6) with every lookup unset: `{$NAME}` becomes empty
// and `{$NAME:default}` its default, scanning to the first `}` after
// each `{$`, continuing after the replacement (one level deep), leaving
// `{$}` alone and an unclosed `{$` as it is. Kept line for line so the
// second reading sees the tokens Caddy would make of a default; the
// test pins it against caddyfile.Parse.
func expandEmptyEnv(input []byte) []byte {
	// One forward pass: Caddy splices each value into the buffer and
	// resumes right after it, which is the same as resuming after the
	// `}` in the input — and linear, where splicing is quadratic in
	// the number of placeholders (a 1 MiB file of `{$A}` is 262,144).
	spanOpen, spanClose := []byte("{$"), []byte("}")
	var out bytes.Buffer
	out.Grow(len(input))
	for {
		begin := bytes.Index(input, spanOpen)
		if begin < 0 {
			break
		}
		end := bytes.Index(input[begin+len(spanOpen):], spanClose)
		if end < 0 {
			break
		}
		end += begin + len(spanOpen)
		name := input[begin+len(spanOpen) : end]
		if len(name) == 0 { // `{$}` is left as it is
			out.Write(input[:end+len(spanClose)])
			input = input[end+len(spanClose):]
			continue
		}
		out.Write(input[:begin])
		if _, dflt, ok := bytes.Cut(name, []byte(":")); ok {
			out.Write(dflt) // unset, so the default; without one, nothing
		}
		input = input[end+len(spanClose):]
	}
	out.Write(input)
	return out.Bytes()
}
