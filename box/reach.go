package box

// Step 6's reachability (DESIGN-box.md, "Reachability: what may stand
// before box_webhook"): of the pushed file's adaptation — Caddy's JSON
// as `moteld adapt` prints it — whether the server that carries
// box_webhook still listens where the push arrived (the door), whether
// the workflow's connection reaches that server's routes (its request
// limits among them), and whether a GET and a POST for / on the
// box_webhook host then meet box_webhook before any handler that could
// answer them, or cap, replace or cut short their body. Pure: the JSON and the
// door in, a verdict out, no module loaded but the five matcher types
// it decides with and Caddy's SNI matcher. Fail-closed every way: a
// handler, a listener wrapper or a TLS policy setting the tables have
// not weighed answers or refuses, a matcher they cannot decide may
// match, and one they do not know may fail.

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/caddy/v2/modules/caddytls"

	"github.com/smallhoursorg/motel/box/proof"
)

const msgAdaptationTooLarge = "the new Caddyfile's adaptation is larger than 16 MiB"

// msgUnreachable is the catalogue's prefix for every reachability
// refusal; what is the block's text.
func msgUnreachable(what string) string {
	return "the new Caddyfile does not reach box_webhook on /: " + what
}

// errAdaptation is the box's error for an adaptation the walk cannot
// read: a fixed text, since a decoder's words quote the input.
var errAdaptation = errors.New("the adaptation does not parse")

// The adaptation is decoded as one JSON value and converted once, in a
// single pass linear in its size, into the shape the walk reads: a
// route's group, matcher sets, handlers and terminal flag; a handler's
// module name, whether a headers handler has a request section, which
// of a request_body or a timeouts handler's settings the table refuses,
// and a subroute's routes. Another module's keys are never read,
// whatever they hold; a value of the wrong shape where a route list, a
// set, a handler or one of those settings must be is the adaptation's
// fault.
type adaptedRoute struct {
	group    string
	match    []map[string]any
	handle   []adaptedHandler
	terminal bool
}

// adaptedHandler's setting is "set" for a request_body that replaces
// the body, else "max_size" for one whose cap is positive and under
// the bundle's; "read_timeout" for a timeouts whose read idle deadline
// is positive and under its floor, else "read_min_rate" for one whose
// rate, under a positive deadline, is over its ceiling; "" otherwise.
type adaptedHandler struct {
	name    string
	request bool
	setting string
	sub     []adaptedRoute
}

// routeList converts a decoded "routes" value.
func routeList(v any) ([]adaptedRoute, error) {
	if v == nil {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, errAdaptation
	}
	routes := make([]adaptedRoute, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, errAdaptation
		}
		var r adaptedRoute
		r.group, _ = m["group"].(string)
		r.terminal, _ = m["terminal"].(bool)
		if sets := m["match"]; sets != nil {
			l, ok := sets.([]any)
			if !ok {
				return nil, errAdaptation
			}
			for _, s := range l {
				set, ok := s.(map[string]any)
				if !ok {
					return nil, errAdaptation
				}
				r.match = append(r.match, set)
			}
		}
		if hs := m["handle"]; hs != nil {
			l, ok := hs.([]any)
			if !ok {
				return nil, errAdaptation
			}
			for _, item := range l {
				hm, ok := item.(map[string]any)
				if !ok {
					return nil, errAdaptation
				}
				var h adaptedHandler
				h.name, _ = hm["handler"].(string)
				switch h.name {
				case "subroute":
					sub, err := routeList(hm["routes"])
					if err != nil {
						return nil, err
					}
					h.sub = sub
				case "headers":
					h.request = hm["request"] != nil
				case "request_body":
					setting, err := requestBodySetting(hm)
					if err != nil {
						return nil, err
					}
					h.setting = setting
				case "timeouts":
					setting, err := timeoutsSetting(hm)
					if err != nil {
						return nil, err
					}
					h.setting = setting
				}
				r.handle = append(r.handle, h)
			}
		}
		routes = append(routes, r)
	}
	return routes, nil
}

// bundleCap is the bundle's cap as the texts name it: the handler's own
// figure (exchange.go), so the two cannot drift.
var bundleCap = fmt.Sprintf("%d MiB", maxBody>>20)

// requestBodySetting reads a request_body handler's two settings as
// Caddy does (requestbody.RequestBody.ServeHTTP): a `set` that is not
// the empty string replaces every body, whatever `max_size` says, and
// a `max_size` is a cap only when positive. Each is read in Caddy's
// shape for it (an int64 for the cap, so a fraction or a number past
// int64 is the adaptation's fault, as validate refused it first); null
// is absent, as in the TLS policies.
func requestBodySetting(hm map[string]any) (string, error) {
	if v, ok := hm["set"]; ok && v != nil {
		s, ok := v.(string)
		if !ok {
			return "", errAdaptation
		}
		if s != "" {
			return "set", nil
		}
	}
	n, err := int64Of(hm, "max_size")
	if err != nil {
		return "", err
	}
	if n > 0 && n < maxBody {
		return "max_size", nil
	}
	return "", nil
}

// timeoutsSetting reads a timeouts handler's two read settings as
// Caddy does (timeouts.Timeouts.ServeHTTP): `read_timeout` installs
// its reader on the body only when positive — with no rate an idle
// deadline reset on every read, with `read_min_rate` an allowance from
// the start of read_timeout plus a second per read_min_rate bytes
// received (caddyhttp.IdleDeadline.next) — so the rate counts only
// under a positive deadline. The floors are the server's for the
// limits they mirror (read_idle_timeout's and read_min_rate's), and a
// deadline under its floor is named ahead of a rate over its ceiling.
// Each is read in Caddy's shape for it: a time.Duration and an int64,
// both whole numbers (that field reads no duration string), so a
// string, a fraction or a number past int64 is the adaptation's fault,
// as validate refused it first; null is absent. The write settings and
// max_write_chunk bound the answer, which the poll recovers, and are
// not read.
func timeoutsSetting(hm map[string]any) (string, error) {
	idle, err := int64Of(hm, "read_timeout")
	if err != nil {
		return "", err
	}
	rate, err := int64Of(hm, "read_min_rate")
	if err != nil {
		return "", err
	}
	switch {
	case idle <= 0:
	case time.Duration(idle) < floorReadIdleTimeout:
		return "read_timeout", nil
	case rate > ceilingReadMinRate:
		return "read_min_rate", nil
	}
	return "", nil
}

// int64Of is a handler's whole-number setting as Caddy's int64 fields
// read it: absent or null is zero, and a value of another shape, a
// fraction or a number past int64 is the adaptation's fault.
func int64Of(hm map[string]any, key string) (int64, error) {
	v, ok := hm[key]
	if !ok || v == nil {
		return 0, nil
	}
	num, ok := v.(json.Number)
	if !ok {
		return 0, errAdaptation
	}
	n, err := num.Int64()
	if err != nil {
		return 0, errAdaptation
	}
	return n, nil
}

// reach asks the question of adapted for host, the box_webhook host
// the walk found, and door, the listen addresses of the loaded server
// the push arrived through — nil when no Caddy server received it (a
// test), and the door is not compared. nil: the door is kept and both
// requests meet box_webhook first. A *refusal: the
// catalogue text naming what stands in the way. Any other error is
// the box's own: an adaptation this code cannot read, or one with no
// box_webhook routed for the host, which the walk of the same bytes
// rules out.
func reach(adapted []byte, host string, door []string) error {
	// One value, numbers kept as written (json.Number): a float64 would
	// round an integer past 2^53 and break one past int64 when the
	// server's settings are encoded again into their typed fields. As
	// Unmarshal would, anything after the value is refused.
	var cfg any
	dec := json.NewDecoder(bytes.NewReader(adapted))
	dec.UseNumber()
	if dec.Decode(&cfg) != nil || !errors.Is(dec.Decode(new(any)), io.EOF) {
		return errAdaptation
	}
	servers, _ := dig(cfg, "apps", "http", "servers").(map[string]any)
	// The server the question is asked of is the one carrying
	// box_webhook whose listener the push arrived through: with a
	// door, the one whose listen addresses are the door's, since a site
	// bound to several addresses adapts into a server per set of
	// addresses it shares with other sites, each carrying its routes;
	// with none, the first carrying it. Another server's routes for the
	// same host answer on another port. No carrying server at the door
	// is a moved door — the push arrived through the loaded server's
	// addresses, which are thereby proven to work, and a file that
	// moves them is the console's to apply ("The door").
	var routes []adaptedRoute
	var server any
	carried := false
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		rs, err := routeList(dig(servers[name], "routes"))
		if err != nil {
			return err
		}
		if !carriesWebhook(rs) {
			continue
		}
		carried = true
		if door != nil {
			listen, err := listenOf(servers[name])
			if err != nil {
				return err
			}
			if !slices.Equal(canonical(door), canonical(listen)) {
				continue
			}
		}
		routes, server = rs, servers[name]
		break
	}
	switch {
	case !carried:
		return errors.New("the adaptation has no box_webhook")
	case server == nil:
		return &refusal{msg: msgListenMoved}
	}
	// The connection next: what refuses it stands before every route.
	s, err := settingsOf(server)
	if err != nil {
		return err
	}
	if what := s.connection(host); what != "" {
		return &refusal{msg: msgUnreachable(what)}
	}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		w := &reachWalk{req: reachRequest(method, host), groups: map[string]groupState{}}
		switch end, b := w.routes(routes, matchVerdict{status: matches}, true); end {
		case reachedWebhook:
		case blocked:
			return &refusal{msg: msgUnreachable(b.text(host))}
		default:
			return fmt.Errorf("the adaptation routes %s / on %s past box_webhook", method, proof.Bound(host))
		}
	}
	return nil
}

// dig follows keys through decoded objects; nil when the path is not
// there or runs through something else.
func dig(v any, keys ...string) any {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

// msgListenMoved is the catalogue text for a push that moves the box
// server's listen addresses. It names no address: an address may hold
// a value the adapter expanded from the service's environment, and
// the signer has the file's diff.
const msgListenMoved = "this push changes where the box's server listens (its listen addresses differ from the loaded config's): " +
	"a push cannot move the address it arrives on; make that change at the console"

// listenOf is a server's `listen` list as Caddy keeps it once loaded:
// each address with its placeholders replaced by the global replacer
// (`{env.*}`, `{system.*}`), as App.Provision replaces them before the
// loaded server holds them, so the two sides are spelt alike. The
// adapter writes every server a list, so a server without one, or
// with one of another shape, is the adaptation's fault, as in the
// routes; so is a placeholder that does not resolve, since Provision
// refuses it and validate ran Provision on the same bytes in the same
// environment.
func listenOf(server any) ([]string, error) {
	m, _ := server.(map[string]any)
	items, ok := m["listen"].([]any)
	if !ok {
		return nil, errAdaptation
	}
	repl := caddy.NewReplacer()
	listen := make([]string, 0, len(items))
	for _, item := range items {
		addr, ok := item.(string)
		if !ok {
			return nil, errAdaptation
		}
		addr, err := repl.ReplaceOrErr(addr, true, true)
		if err != nil {
			return nil, errAdaptation
		}
		listen = append(listen, addr)
	}
	return listen, nil
}

// canonical is a listen list as a sorted set of addresses as Caddy
// spells them — the network, host and port range as ParseNetworkAddress
// reads them and String prints them, the default network dropped — so
// that one list written with its network, in another order or with an
// address twice compares equal to itself. Spelling, not sockets: `:443`
// and `[::]:443` differ, as do `:443-444` and `:443, :444`; a port
// range stands as a range (the adapter writes none), never expanded.
// An address Caddy cannot parse stands as written.
func canonical(listen []string) []string {
	set := map[string]bool{}
	for _, addr := range listen {
		if na, err := caddy.ParseNetworkAddress(addr); err == nil {
			addr = na.String()
		}
		set[addr] = true
	}
	return slices.Sorted(maps.Keys(set))
}

// carriesWebhook reports whether a box_webhook handler stands anywhere
// in routes.
func carriesWebhook(routes []adaptedRoute) bool {
	for _, r := range routes {
		for _, h := range r.handle {
			if h.name == "box_webhook" || (h.name == "subroute" && carriesWebhook(h.sub)) {
				return true
			}
		}
	}
	return false
}

// serverSettings is what of a server's own keys the workflow's
// connection meets before any route runs: the protocols its listeners
// serve, its listener wrappers, its TLS connection policies, its
// strict SNI check and the request limits the floors are asked of.
// Only these keys are encoded again and decoded into Caddy's shapes
// for them (its own types, for client authentication and a duration),
// so a value of another shape is the adaptation's fault, as in the
// routes; no other key of the server is read.
type serverSettings struct {
	Protocols         []string          `json:"protocols"`
	ListenProtocols   [][]string        `json:"listen_protocols"`
	ListenerWrappers  []map[string]any  `json:"listener_wrappers"`
	StrictSNIHost     *bool             `json:"strict_sni_host"`
	RawPolicies       []json.RawMessage `json:"tls_connection_policies"`
	ReadTimeout       caddy.Duration    `json:"read_timeout"`
	ReadHeaderTimeout caddy.Duration    `json:"read_header_timeout"`
	ReadIdleTimeout   caddy.Duration    `json:"read_idle_timeout"`
	ReadMinRate       int64             `json:"read_min_rate"`
	WriteTimeout      caddy.Duration    `json:"write_timeout"`
	MaxHeaderBytes    int               `json:"max_header_bytes"`
	policies          []tlsPolicy
}

// serverKeys are the keys settingsOf reads: serverSettings' JSON tags,
// so a field added there is read without a second list to keep.
var serverKeys = func() []string {
	t := reflect.TypeFor[serverSettings]()
	keys := make([]string, 0, t.NumField())
	for i := range t.NumField() {
		if tag := t.Field(i).Tag.Get("json"); tag != "" {
			keys = append(keys, tag)
		}
	}
	return keys
}()

// The floors on the box server's request limits (DESIGN-box.md, Caps,
// and "The request limits" in "Reachability"): every site on the
// server shares them, so only a value the workflow's request cannot
// work with is refused. A limit counts only when positive: zero or
// absent is Caddy's default (App.Provision: 16 KiB of header, a minute
// to read the header, a minute's read idle, no whole-read deadline, no
// rate, no write deadline), and a negative one is no limit, to Go's
// net/http and to Caddy's idle reader alike (max_header_bytes: the
// default again). The rate counts only while a read idle deadline
// applies: Caddy's Server.ServeHTTP installs the reader that holds the
// rate only for a positive ReadIdleTimeout, which zero becomes. The
// idle floor and the rate's ceiling are asked as well of a `timeouts`
// handler ahead of box_webhook (timeoutsSetting), whose read_timeout
// and read_min_rate are the same two bounds per route.
const (
	floorMaxHeaderBytes    = 8000 // 8 KB: Caddy reads `max_header_size 8KB` with SI prefixes, and that spelling passes
	floorReadHeaderTimeout = 5 * time.Second
	floorReadTimeout       = 30 * time.Second
	floorReadIdleTimeout   = 10 * time.Second
	ceilingReadMinRate     = 64 << 10
	floorWriteTimeout      = 5 * time.Second
)

// tlsPolicy is one TLS connection policy: every key it sets, for the
// table's fail-closed default, and the values the table reads.
type tlsPolicy struct {
	keys                 map[string]json.RawMessage
	Match                map[string]any                 `json:"match"`
	ALPN                 []string                       `json:"alpn"`
	Drop                 bool                           `json:"drop"`
	ClientAuthentication *caddytls.ClientAuthentication `json:"client_authentication"`
}

func settingsOf(server any) (serverSettings, error) {
	m, _ := server.(map[string]any)
	keys := map[string]any{}
	for _, k := range serverKeys {
		if v, ok := m[k]; ok {
			keys[k] = v
		}
	}
	var s serverSettings
	b, err := json.Marshal(keys)
	if err != nil || json.Unmarshal(b, &s) != nil {
		return serverSettings{}, errAdaptation
	}
	for _, raw := range s.RawPolicies {
		var p tlsPolicy
		if json.Unmarshal(raw, &p.keys) != nil || json.Unmarshal(raw, &p) != nil {
			return serverSettings{}, errAdaptation
		}
		s.policies = append(s.policies, p)
	}
	return s, nil
}

// connection is the question asked before the routes, of the
// connection bin/apply's curl makes: HTTP/1.1 or HTTP/2 over TLS, both
// offered by ALPN, the host as its SNI (none for an IP address, to
// which curl sends none), no client certificate, no PROXY header. ""
// when it reaches the routes; otherwise the catalogue text of the first
// thing that refuses it, asked in the order of the tables: the
// listeners, the wrappers, the TLS policies, the strict SNI check,
// then the request limits.
func (s serverSettings) connection(host string) string {
	served := map[string]bool{}
	for _, ps := range s.listeners() {
		if !slices.Contains(ps, "h1") && !slices.Contains(ps, "h2") {
			return "a listener's protocols include neither h1 nor h2"
		}
		for _, p := range ps {
			served[p] = true
		}
	}
	tlsAt := slices.IndexFunc(s.ListenerWrappers, func(w map[string]any) bool { return w["wrapper"] == "tls" })
	for i, w := range s.ListenerWrappers {
		name, _ := w["wrapper"].(string)
		fallback, _ := w["fallback_policy"].(string)
		switch {
		case name == "tls":
		case name == "http_redirect" && i < tlsAt:
			// Ahead of the handshake it answers only a connection whose
			// first bytes are plaintext HTTP. Behind it — where every
			// wrapper stands when tls is not listed — it reads the
			// request itself and redirects an HTTP/1.1 one.
		case name == "proxy_protocol" && !strings.EqualFold(fallback, "require"):
			// A connection without a PROXY header is served as sent
			// unless the policy for its address requires one; the
			// address is not the file's to know.
		default:
			return "the " + named(name) + " listener wrapper may refuse the connection"
		}
	}
	sni := host
	if net.ParseIP(host) != nil {
		sni = ""
	}
	for _, p := range s.policies {
		// Caddy hands the handshake to the first policy whose matchers
		// all match; one decided per connection may or may not be it,
		// so it is weighed and the next ones too.
		v := p.match(sni)
		if v.status == noMatch {
			continue
		}
		if what := p.block(served); what != "" {
			if v.status == mayMatch {
				return "a TLS connection policy that may apply to " + proof.Bound(host) + " " + what +
					" (its " + named(v.matcher) + " matcher is decided per connection)"
			}
			return "a TLS connection policy for " + proof.Bound(host) + " " + what
		}
		if v.status == matches {
			break
		}
	}
	// Caddy turns the check on, when the file leaves it unset, for a
	// server where any policy authenticates clients; it then refuses a
	// request whose SNI is not its Host.
	strict := slices.ContainsFunc(s.policies, func(p tlsPolicy) bool {
		return p.ClientAuthentication != nil && p.ClientAuthentication.Active()
	})
	if s.StrictSNIHost != nil {
		strict = *s.StrictSNIHost
	}
	if strict && sni != host {
		return "the server's strict_sni_host refuses a request to an IP address, which carries no SNI"
	}
	return s.limits()
}

// limits is the Reachability table of request limits, in its order:
// "" when every limit set is one the workflow's request can work
// with, else the catalogue text of the first that is not. Each text is
// fixed words naming the setting and its floor, never the value.
func (s serverSettings) limits() string {
	under := func(d caddy.Duration, floor time.Duration) bool { return d > 0 && time.Duration(d) < floor }
	switch {
	case s.MaxHeaderBytes > 0 && s.MaxHeaderBytes < floorMaxHeaderBytes:
		return fmt.Sprintf("the server's max_header_bytes is under %d KB, the floor for a push's headers and token", floorMaxHeaderBytes/1000)
	case under(s.ReadHeaderTimeout, floorReadHeaderTimeout):
		return fmt.Sprintf("the server's read_header_timeout is under %s, the floor for a push's handshake and headers", floorReadHeaderTimeout)
	case under(s.ReadTimeout, floorReadTimeout):
		return fmt.Sprintf("the server's read_timeout is under %s, the floor for a push's upload", floorReadTimeout)
	case under(s.ReadIdleTimeout, floorReadIdleTimeout):
		return fmt.Sprintf("the server's read_idle_timeout is under %s, the floor for a push's upload", floorReadIdleTimeout)
	case s.ReadMinRate > ceilingReadMinRate && s.ReadIdleTimeout >= 0:
		return fmt.Sprintf("the server's read_min_rate is over %d KiB/s, the ceiling for a push's upload", ceilingReadMinRate>>10)
	case under(s.WriteTimeout, floorWriteTimeout):
		return fmt.Sprintf("the server's write_timeout is under %s, the floor for a push's handshake", floorWriteTimeout)
	}
	return ""
}

// named is a wrapper's, a matcher's or a setting's name for a
// refusal's text, bounded: never empty from Caddy, and "nameless" when
// other bytes give none, so the text still reads.
func named(s string) string {
	if s = proof.Bound(s); s == "" {
		return "nameless"
	}
	return s
}

// listeners is each listener's protocols as Caddy provisions them: the
// server's (h1, h2 and h3 when it lists none), unless listen_protocols
// gives the listener its own, in which "" stands for the server's.
func (s serverSettings) listeners() [][]string {
	server := s.Protocols
	if len(server) == 0 {
		server = []string{"h1", "h2", "h3"}
	}
	if s.ListenProtocols == nil {
		return [][]string{server}
	}
	out := make([][]string, 0, len(s.ListenProtocols))
	for _, ln := range s.ListenProtocols {
		switch {
		case len(ln) == 0:
			out = append(out, server)
		case slices.Contains(ln, ""):
			out = append(out, append(slices.Clone(ln), server...))
		default:
			out = append(out, ln)
		}
	}
	return out
}

// match is the policy's matchers asked of the workflow's ClientHello,
// whose SNI is sni: sni decided statically by Caddy's own type, any
// other matcher (remote_ip, local_ip, sni_regexp, one this code does
// not know) or a name holding a placeholder per connection, so "may".
// No matcher matches, as Caddy's catch-all policy does. Policies are
// walked in order: past 30 of them Caddy looks first at those naming
// the host exactly, which the adapter writes ahead of any other that
// matches it.
func (p tlsPolicy) match(sni string) matchVerdict {
	v := matchVerdict{status: matches}
	for _, k := range slices.Sorted(maps.Keys(p.Match)) {
		names, static := staticPatterns(p.Match[k])
		switch {
		case k == "sni" && static:
			if !caddytls.MatchServerName(names).Match(&tls.ClientHelloInfo{ServerName: sni}) {
				return matchVerdict{status: noMatch}
			}
		case v.status == matches:
			v = matchVerdict{status: mayMatch, matcher: k}
		}
	}
	return v
}

// block is the Reachability table of TLS policy settings: what the
// policy, when it applies to the box host, does to the workflow's
// handshake, served being the protocols the server's listeners serve,
// which bound what ALPN can agree on. "" when the handshake completes;
// a setting the table has not weighed may refuse it. A setting written
// as null is Caddy's zero value for it, as if absent.
func (p tlsPolicy) block(served map[string]bool) string {
	for _, k := range slices.Sorted(maps.Keys(p.keys)) {
		if string(p.keys[k]) == "null" {
			continue
		}
		switch k {
		case "match", "certificate_selection", "protocol_min", "protocol_max",
			"default_sni", "fallback_sni", "insecure_secrets_log":
		case "client_authentication":
			if requiresCertificate(p.ClientAuthentication) {
				return "requires a client certificate"
			}
		case "drop":
			if p.Drop {
				return "drops the connection"
			}
		case "alpn":
			// An empty list negotiates no protocol, and curl speaks
			// HTTP/1.1; otherwise Caddy takes out what no listener
			// serves.
			agrees := (len(p.ALPN) == 0 && served["h1"]) ||
				(slices.Contains(p.ALPN, "h2") && served["h2"]) ||
				(slices.Contains(p.ALPN, "http/1.1") && served["h1"])
			if !agrees {
				return "offers neither h2 nor http/1.1 by ALPN"
			}
		default:
			return "sets " + named(k) + ", which may refuse the handshake"
		}
	}
	return ""
}

// requiresCertificate is whether a client authentication refuses a
// client that sends no certificate: one Caddy counts as active, unless
// in mode request or verify_if_given. Its verifiers (trusted_leaf_certs
// becomes one) run only on a certificate presented, from v2.11.8
// (verifyConnection), so they refuse no client that sends none.
func requiresCertificate(ca *caddytls.ClientAuthentication) bool {
	if ca == nil || !ca.Active() {
		return false
	}
	return ca.Mode != "request" && ca.Mode != "verify_if_given"
}

// matchStatus is what a matcher, a set or a route says of the request:
// it does not match, it may (decided per request), it does.
type matchStatus int

const (
	noMatch matchStatus = iota
	mayMatch
	matches
)

// matchVerdict is a matcher's, a set's or a route's answer: its status,
// the matcher it hangs on when that is mayMatch, and whether a matcher
// may fail at request time — Caddy then answers its error and routes no
// further, whatever stands behind.
type matchVerdict struct {
	status  matchStatus
	matcher string
	fails   bool
}

// reachEnd is how a walk of a route list ended: the routes ran out, the
// request met box_webhook, or something stood in the way.
type reachEnd int

const (
	fellThrough reachEnd = iota
	reachedWebhook
	blocked
)

// reachBlock is what stood in the way: a handler by module name (and
// the setting of it, when one setting is the block: request_body's
// set or max_size, timeouts' read_timeout or read_min_rate), a
// terminal route that ran out of handlers (name
// ""; a site when at the server level), box_webhook itself behind a
// per-request matcher, or a matcher that may fail — under a match that
// is certain or, when may is set, hangs on matcher.
type reachBlock struct {
	name     string
	setting  string
	terminal bool
	site     bool
	changes  bool
	caps     bool
	cuts     bool
	may      bool
	behind   bool
	fails    bool
	matcher  string
}

// text is the block's catalogue text, every input-derived value
// bounded. A handler's and a matcher's name are module names, never
// empty from Caddy; from any other bytes they may be, and the text
// still has to read.
func (b *reachBlock) text(host string) string {
	matcher := proof.Bound(b.matcher)
	if matcher == "" {
		matcher = "nameless"
	}
	if b.fails {
		return "the " + matcher + " matcher ahead of it may fail (decided per request)"
	}
	what := proof.Bound(b.name)
	switch {
	case b.terminal && b.site:
		what = "an earlier site for " + proof.Bound(host)
	case b.terminal:
		what = "a terminal route"
	case what == "":
		what = "a nameless handler"
	case b.setting != "":
		what += "'s " + b.setting
	}
	switch {
	case b.behind:
		return what + " stands behind a " + matcher + " matcher decided per request"
	case b.may && b.changes:
		return what + " may change the request first (its " + matcher + " matcher is decided per request)"
	case b.may && b.caps:
		return what + " may cap the body under " + bundleCap + " first (its " + matcher + " matcher is decided per request)"
	case b.may && b.cuts:
		return what + " may cut " + cut(b.setting) + " first (its " + matcher + " matcher is decided per request)"
	case b.may:
		return what + " may answer first (its " + matcher + " matcher is decided per request)"
	case b.changes:
		return what + " changes the request first"
	case b.caps:
		return what + " caps the body under " + bundleCap + " first"
	case b.cuts:
		return what + " cuts " + cut(b.setting) + " first"
	default:
		return what + " answers first"
	}
}

// cut is what a timeouts setting past its floor does to a push, for
// the block's text: the upload the rate cuts, or the one the deadline
// cuts, each named by its floor, the server's own figure for that
// bound.
func cut(setting string) string {
	if setting == "read_min_rate" {
		return fmt.Sprintf("an upload under %d KiB/s", ceilingReadMinRate>>10)
	}
	return fmt.Sprintf("an upload that stalls under %s", floorReadIdleTimeout)
}

// groupState is what a route group has matched so far: Caddy skips a
// member once an earlier one matched, so a later member runs for sure
// only while no earlier one did, and at most "may" after one may have.
type groupState struct {
	status  matchStatus
	matcher string
}

type reachWalk struct {
	req    *http.Request
	groups map[string]groupState
}

// routes walks rs in Caddy's order, each route under the enclosing
// verdict outer, and ends at the first handler that decides. top is the
// server's own route list, whose routes are sites.
func (w *reachWalk) routes(rs []adaptedRoute, outer matchVerdict, top bool) (reachEnd, *reachBlock) {
	for _, r := range rs {
		// Caddy checks a route's group before its matchers (v2.11.8
		// on), so a member of a group already satisfied is skipped
		// with its matchers unrun: one that may fail fails only while
		// the group is not.
		if r.group != "" && w.groups[r.group].status == matches {
			continue
		}
		v := w.match(r.match)
		if v.fails {
			return blocked, &reachBlock{fails: true, matcher: v.matcher}
		}
		if v.status == noMatch {
			continue
		}
		// The enclosing condition first: a member runs only when every
		// route around it matched, and satisfies its group only as far
		// as they did (Caddy's group map is one per request, across
		// nesting).
		if outer.status < v.status {
			v = outer
		}
		if r.group != "" {
			// The group records the member's own match under the
			// enclosing condition, not the downgrade an earlier "may"
			// member imposes on it: a member that matches for sure
			// satisfies the group whether or not an earlier one may
			// have, and later members never run.
			g := w.groups[r.group]
			if v.status > g.status {
				w.groups[r.group] = groupState{v.status, v.matcher}
			}
			if g.status == mayMatch && v.status == matches {
				v = matchVerdict{status: mayMatch, matcher: g.matcher}
			}
		}
		may := v.status == mayMatch
		for _, h := range r.handle {
			switch classify(h) {
			case classReached:
				if may {
					// Reached for the requests the matcher admits; the
					// rest skip it, and whatever follows is not its
					// answer. No directive the walk accepts adapts to
					// this (box_webhook takes no matcher and its site
					// is a bare host); the contract is kept anyway.
					return blocked, &reachBlock{name: h.name, behind: true, matcher: v.matcher}
				}
				return reachedWebhook, nil
			case classSubroute:
				if end, b := w.routes(h.sub, v, false); end != fellThrough {
					return end, b
				}
			case classPasses:
			case classChanges:
				return blocked, &reachBlock{name: h.name, setting: h.setting, changes: true, may: may, matcher: v.matcher}
			case classCaps:
				return blocked, &reachBlock{name: h.name, setting: h.setting, caps: true, may: may, matcher: v.matcher}
			case classCuts:
				return blocked, &reachBlock{name: h.name, setting: h.setting, cuts: true, may: may, matcher: v.matcher}
			default:
				return blocked, &reachBlock{name: h.name, may: may, matcher: v.matcher}
			}
		}
		if r.terminal {
			// Caddy hands the rest of the chain an empty handler: the
			// request ends here with whatever was written, or nothing.
			return blocked, &reachBlock{terminal: true, site: top, may: may, matcher: v.matcher}
		}
	}
	return fellThrough, nil
}

type handlerClass int

const (
	classAnswers handlerClass = iota
	classPasses
	classChanges
	classCaps
	classCuts
	classSubroute
	classReached
)

// classify is the first Reachability table.
func classify(h adaptedHandler) handlerClass {
	switch h.name {
	case "box_webhook":
		return classReached
	case "subroute":
		return classSubroute
	case "vars", "map", "log_append", "tracing",
		"encode", "push", "hint_penaltybox":
		return classPasses
	case "headers":
		if h.request {
			return classChanges
		}
		return classPasses
	case "request_body":
		switch h.setting {
		case "set":
			return classChanges
		case "max_size":
			return classCaps
		}
		return classPasses
	case "timeouts":
		if h.setting != "" {
			return classCuts
		}
		return classPasses
	case "rewrite":
		return classChanges
	}
	return classAnswers
}

// match is a route's matcher sets, tried in order as Caddy tries them,
// until one matches: a set that matches for sure ends it (the sets
// after it never run); one that may fail ends it the other way; one
// that may match is remembered. No set is a match.
func (w *reachWalk) match(sets []map[string]any) matchVerdict {
	if len(sets) == 0 {
		return matchVerdict{status: matches}
	}
	best := matchVerdict{status: noMatch}
	for _, set := range sets {
		v := w.set(set)
		switch {
		case v.fails, v.status == matches:
			return v
		case v.status == mayMatch && best.status == noMatch:
			best = v
		}
	}
	return best
}

// set is one matcher set: every matcher must match. Caddy runs a set's
// matchers in an order fixed only at load, so one that may fail counts
// whatever the others say; otherwise one that does not match decides,
// and one that may, when none says no, leaves "may".
func (w *reachWalk) set(set map[string]any) matchVerdict {
	best := matchVerdict{status: matches}
	for _, k := range slices.Sorted(maps.Keys(set)) {
		v := w.one(k, set[k])
		switch {
		case v.fails:
			return v
		case v.status == noMatch:
			best = v
		case v.status == mayMatch && best.status == matches:
			best = v
		}
	}
	return best
}

// one is the second Reachability table: the five matchers decided
// statically with Caddy's own types; the matchers Caddy decides per
// request but never fails on, "may"; and the rest — `expression`,
// `file`, a matcher this walk does not know — may fail as well.
func (w *reachWalk) one(key string, v any) matchVerdict {
	may := matchVerdict{status: mayMatch, matcher: key}
	switch key {
	case "host":
		list, ok := staticPatterns(v)
		if !ok {
			return may
		}
		m := caddyhttp.MatchHost(list)
		if m.Provision(caddy.Context{}) != nil {
			return may
		}
		return decided(m.Match(w.req))
	case "path":
		list, ok := staticPatterns(v)
		if !ok {
			return may
		}
		m := caddyhttp.MatchPath(list)
		if m.Provision(caddy.Context{}) != nil {
			return may
		}
		return decided(m.Match(w.req))
	case "method":
		list, ok := staticPatterns(v)
		if !ok {
			return may
		}
		return decided(caddyhttp.MatchMethod(list).Match(w.req))
	case "protocol":
		// Only the scheme is the request's to know: http and https by
		// TLS, grpc by a content type the workflow never sends. An HTTP
		// version is the connection's, negotiated per request.
		switch proto, _ := v.(string); proto {
		case "http", "https", "grpc":
			return decided(caddyhttp.MatchProtocol(proto).Match(w.req))
		}
		return may
	case "not":
		list, ok := v.([]any)
		if !ok {
			return may
		}
		if len(list) == 0 {
			return matchVerdict{status: matches} // as Caddy: nothing negated
		}
		sets := make([]map[string]any, 0, len(list))
		for _, s := range list {
			set, ok := s.(map[string]any)
			if !ok {
				return may
			}
			sets = append(sets, set)
		}
		inner := w.match(sets)
		switch {
		case inner.fails:
			return matchVerdict{status: mayMatch, matcher: key, fails: true}
		case inner.status == matches:
			return matchVerdict{status: noMatch}
		case inner.status == noMatch:
			return matchVerdict{status: matches}
		}
		return may
	case "header", "header_regexp", "query", "path_regexp", "vars", "vars_regexp", "tls",
		// remote_ip and client_ip fail only while a TLS handshake is
		// incomplete: 0-RTT early data, which a push never sends.
		"remote_ip", "client_ip":
		return may
	}
	may.fails = true
	return may
}

// staticPatterns reads a list of patterns none of which holds a
// placeholder.
func staticPatterns(v any) ([]string, bool) {
	items, ok := v.([]any)
	if !ok {
		return nil, false
	}
	list := make([]string, 0, len(items))
	for _, item := range items {
		p, ok := item.(string)
		if !ok || strings.Contains(p, "{") {
			return nil, false
		}
		list = append(list, p)
	}
	return list, true
}

func decided(matched bool) matchVerdict {
	if matched {
		return matchVerdict{status: matches}
	}
	return matchVerdict{status: noMatch}
}

// reachRequest is the request the question is asked for: method on
// https://host/, no query, no header, over TLS, with the replacer
// Caddy's path matcher expects in the context.
func reachRequest(method, host string) *http.Request {
	r := &http.Request{
		Method: method, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		URL: &url.URL{Scheme: "https", Host: host, Path: "/"}, Host: host, RequestURI: "/",
		Header: http.Header{}, TLS: &tls.ConnectionState{},
	}
	return r.WithContext(context.WithValue(context.Background(), caddy.ReplacerCtxKey, caddy.NewReplacer()))
}
