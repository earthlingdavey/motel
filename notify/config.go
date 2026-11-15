package notify

import (
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Defaults for a packaged box. The secrets file is a Caddyfile line
// too, for tests and for the e2e image, which run as other users in
// other trees; the state directory and the socket are fixed, since
// the units setup writes name them (theStateDir, theSocket).
const (
	// DefaultSecretsFile is the box tier's env file: the values of
	// secrets/_box/NOTIFY_*.age, as root writes an app's
	// (box/DESIGN-box.md, "Values and the env file"), 0640 root:motel,
	// bound into no unit.
	DefaultSecretsFile = "/etc/motel/secrets/_box.env" //nolint:gosec // G101: a path, not a credential
	// DefaultStateDir holds what motel renders for Alertmanager: the
	// config and one file per channel URL. Made by `motel monitor setup`,
	// 2750 motel:prometheus, so what motel writes is born group
	// prometheus and Alertmanager reads it without motel holding that
	// group. Not under /var/lib/motel, which is 0750 motel:motel: the
	// prometheus uid could not traverse it.
	DefaultStateDir = "/var/lib/motel-notify"
	// DefaultSocket is Alertmanager's listener in the host's namespace, a
	// systemd socket unit (motel-am.socket), 0660 root:motel, in a
	// runtime directory of its own: /run/motel is motel.service's
	// RuntimeDirectory, removed whenever motel stops. Its one other, with
	// the store, is inside the metrics namespace (motel-am-metrics.socket).
	DefaultSocket = "/run/motel-notify/am.sock"
)

// theStateDir is where motel renders for Alertmanager: DefaultStateDir,
// the one directory setup makes and the drop-in's --config.file names.
// theSocket is where motel reaches it: DefaultSocket, motel-am.socket's.
// No Caddyfile line sets either (a state_dir or an alertmanager line
// is refused); variables only so that a test can stand its own in.
var (
	theStateDir = DefaultStateDir
	theSocket   = DefaultSocket
)

// Severities, highest first. A channel's floor is the lowest it
// receives.
const (
	SeverityCritical = "critical"
	SeverityWarning  = "warning"
	SeverityInfo     = "info"
)

func severityRank(s string) int {
	switch s {
	case SeverityCritical:
		return 3
	case SeverityWarning:
		return 2
	case SeverityInfo:
		return 1
	}
	return 0
}

// Channel kinds, and the Alertmanager integration each renders to:
// json and ntfy are its webhook (ntfy takes Alertmanager's payload as a
// message body), slack and discord its own.
var channelKinds = map[string]string{
	"json":    "webhook",
	"ntfy":    "webhook",
	"slack":   "slack",
	"discord": "discord",
}

var (
	channelNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	secretKeyRe   = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,127}$`)
)

var (
	errLiteralURL = errors.New("a channel URL is written {secret.NOTIFY_<NAME>}, its value in the box's secrets, never in the Caddyfile")
	errSecretName = errors.New("the secret's name is NOTIFY_ followed by capitals, digits and underscores")
)

// Channel is one `channel` line.
type Channel struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Secret is the key of the URL's value in the secrets file.
	Secret string `json:"secret"`
	// Severity is the lowest severity the channel receives.
	Severity string `json:"severity,omitempty"`
}

func (ch Channel) check() error {
	if !channelNameRe.MatchString(ch.Name) {
		return fmt.Errorf("channel name %q: lowercase letters, digits and dashes, at most 32", ch.Name)
	}
	if _, ok := channelKinds[ch.Kind]; !ok {
		return fmt.Errorf("channel %s: kind %q is not one of json, ntfy, slack, discord", ch.Name, ch.Kind)
	}
	if ch.Severity != "" && severityRank(ch.Severity) == 0 {
		return fmt.Errorf("channel %s: severity %q is not critical, warning or info", ch.Name, ch.Severity)
	}
	return nil
}

// Bind is one `bind <identity> <channel>` line.
type Bind struct {
	Identity string `json:"identity"`
	Channel  string `json:"channel"`
}

// identity is a parsed binding identity: a deploy_trust source's label
// and claims it pins. It is the deployed_by string's grammar
// (`oidc:<issuer> repository=org/app`), read as a description of a
// trust source rather than of one token: `ref=` and `actor=`, which a
// token carries and no source pins, would match nothing.
type identity struct {
	label  string
	claims map[string]string
}

func parseIdentity(s string) (identity, error) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return identity{}, errors.New("empty identity")
	}
	id := identity{label: fields[0], claims: map[string]string{}}
	switch {
	case strings.HasPrefix(id.label, "local:/"):
	case strings.HasPrefix(id.label, "oidc:https://"):
		if len(fields) == 1 {
			// Every source on an issuer would match: a binding names
			// one identity, as deploy_trust pins one.
			return identity{}, errors.New("an oidc identity names at least one claim the app's deploy_trust pins, such as repository=org/app")
		}
	default:
		return identity{}, errors.New("an identity starts local:/<public key path> or oidc:https://<issuer>, as deployed_by does")
	}
	for _, f := range fields[1:] {
		k, v, ok := strings.Cut(f, "=")
		if !ok || k == "" || v == "" {
			return identity{}, fmt.Errorf("%q is not claim=value", f)
		}
		if _, dup := id.claims[k]; dup {
			return identity{}, fmt.Errorf("claim %s is given twice", k)
		}
		id.claims[k] = v
	}
	return id, nil
}

// TrustSource is what notify reads of a deploy_trust source
// (deploytrust.Source).
type TrustSource interface {
	Label() string
	PinnedClaim(name string) (string, bool)
}

// matches reports whether the source is this identity: the same label,
// and every claim the identity names pinned by the source to the same
// value.
func (id identity) matches(s TrustSource) bool {
	if s.Label() != id.label {
		return false
	}
	for k, v := range id.claims {
		if got, ok := s.PinnedClaim(k); !ok || got != v {
			return false
		}
	}
	return true
}

// unpinnedClaims is, for an identity no source matches, the claims it
// names that keep a source of its label from matching: those that
// source does not pin at all, sorted. Empty unless some source of the
// label pins every other claim the identity names to the same value,
// so it is the reason alone, never a guess among sources.
func unpinnedClaims(id identity, apps []string, trust map[string][]TrustSource) []string {
	for _, app := range apps {
		for _, s := range trust[app] {
			if s.Label() != id.label {
				continue
			}
			var extra []string
			matches := true
			for k, v := range id.claims {
				got, ok := s.PinnedClaim(k)
				switch {
				case !ok:
					extra = append(extra, k)
				case got != v:
					matches = false
				}
			}
			// The line without them must still be an identity: an oidc
			// one names at least one claim (parseIdentity), so some claim
			// it names must be pinned, and match.
			kept := len(extra) < len(id.claims) || strings.HasPrefix(id.label, "local:")
			if matches && len(extra) > 0 && kept {
				sort.Strings(extra)
				return extra
			}
		}
	}
	return nil
}

// resolved is the configuration once checked against the apps and the
// secrets: what the renderer and the poster read.
type resolved struct {
	channels map[string]Channel
	order    []string // channel names, sorted
	operator []string // channel names, deduplicated, in the order given
	// appChannels is each app's bound channels, sorted; an app with
	// none is absent.
	appChannels map[string][]string
	apps        []string // every app liveswap configures, sorted
	urls        map[string]string
	heartbeat   string // the heartbeat's secret key, or ""
}

// resolve checks the whole block against the apps' trust and the
// secrets file's values, and is every refusal `motel validate` makes
// for it: an operator line naming no channel, a channel named twice, a
// binding to a channel that does not exist or to an identity no app
// trusts, a secret the box does not hold, a URL that is not https to a
// public address. Errors name keys and channels, never a value.
func (a *App) resolve(trust map[string][]TrustSource, secrets map[string]string) (*resolved, error) {
	r, _, err := a.resolveWith(trust, secrets, nil, false, nil)
	return r, err
}

// resolveWith is resolve with two departures from it, each one load's
// (DESIGN-notify.md, "Secrets at load"):
//
//   - held are the secrets a box push carries (pushedSecrets): each is
//     taken as held by the box without its value being read or checked,
//     and is given no URL. Only `motel validate` passes any, and only for
//     the box's own secrets file, which is where root writes a push's
//     secrets/_box/.
//   - drop, a boot's load (dropsChannels), drops a channel whose secret
//     is missing or whose value is refused, rather than refusing the
//     config: it leaves the operator line, the bindings and the route
//     tree, and each is in dropped, by its line and key, for the
//     journal. A missing heartbeat secret drops the heartbeat.
//   - unusable is why the secrets file could not be read or parsed, or
//     nil: every secret is then unusable, so a drop takes out every line
//     that reads one, each reason naming the file's error, and any other
//     load refuses the first.
func (a *App) resolveWith(trust map[string][]TrustSource, secrets map[string]string, held map[string]bool, drop bool, unusable error) (*resolved, []error, error) {
	r := &resolved{
		channels:    map[string]Channel{},
		appChannels: map[string][]string{},
		urls:        map[string]string{},
		heartbeat:   a.HeartbeatSecret,
	}
	for _, ch := range a.Channels {
		if err := ch.check(); err != nil {
			return nil, nil, err
		}
		if _, dup := r.channels[ch.Name]; dup {
			return nil, nil, fmt.Errorf("channel %s is defined twice", ch.Name)
		}
		r.channels[ch.Name] = ch
		r.order = append(r.order, ch.Name)
	}
	sort.Strings(r.order)
	if len(a.Operator) == 0 {
		return nil, nil, errors.New("no operator line: name the channels that receive every event with operator <channel>")
	}
	for _, name := range a.Operator {
		if _, ok := r.channels[name]; !ok {
			return nil, nil, fmt.Errorf("operator names %s, which no channel line defines", name)
		}
		if !slices.Contains(r.operator, name) {
			r.operator = append(r.operator, name)
		}
	}
	for app := range trust {
		r.apps = append(r.apps, app)
	}
	sort.Strings(r.apps)
	for _, b := range a.Binds {
		id, err := parseIdentity(b.Identity)
		if err != nil {
			return nil, nil, fmt.Errorf("bind %q: %w", b.Identity, err)
		}
		if _, ok := r.channels[b.Channel]; !ok {
			return nil, nil, fmt.Errorf("bind %q names channel %s, which no channel line defines", b.Identity, b.Channel)
		}
		trusted := false
		for _, app := range r.apps {
			for _, s := range trust[app] {
				if id.matches(s) {
					trusted = true
					if !slices.Contains(r.appChannels[app], b.Channel) {
						r.appChannels[app] = append(r.appChannels[app], b.Channel)
					}
					break
				}
			}
		}
		if !trusted {
			// A revoked key or a repository taken out of deploy_trust:
			// the binding would reach nobody's events, or, once the
			// identity is trusted again, reach them without the operator
			// having said so again. A source of the label that pins every
			// other claim is a deployed_by pasted whole: its ref=, actor=
			// or sub= is a token's, which no source pins.
			if extra := unpinnedClaims(id, r.apps, trust); len(extra) > 0 {
				return nil, nil, fmt.Errorf("bind %q: no deploy_trust source of %s pins %s: a binding names what a source pins, not a token's every claim (deployed_by shows them all); remove %s from the line", b.Identity, id.label, strings.Join(extra, ", "), strings.Join(extra, " and "))
			}
			return nil, nil, fmt.Errorf("bind %q: no app's deploy_trust trusts this identity; remove the line, or trust it", b.Identity)
		}
	}
	for app := range r.appChannels {
		sort.Strings(r.appChannels[app])
	}
	// Each secret key → every line that reads it, in the block's order:
	// a drop takes them all out (without), so the reason names them all.
	keys := map[string][]string{}
	for _, name := range r.order {
		key := r.channels[name].Secret
		keys[key] = append(keys[key], "channel "+name)
	}
	if r.heartbeat != "" {
		keys[r.heartbeat] = append(keys[r.heartbeat], "heartbeat")
	}
	bad := map[string]bool{}
	var dropped []error
	for _, key := range slices.Sorted(maps.Keys(keys)) {
		who := strings.Join(keys[key], ", ")
		if held[key] && filepath.Clean(a.secretsFile()) == DefaultSecretsFile {
			continue
		}
		v, ok := secrets[key]
		var why error
		switch {
		case unusable != nil:
			why = fmt.Errorf("%s: secret %s cannot be read: %w", who, key, unusable)
		case !ok:
			why = fmt.Errorf("%s: secret %s is not in the box's secrets (%s)", who, key, a.secretsFile())
		case v == "":
			why = fmt.Errorf("%s: secret %s is empty in the box's secrets (%s): give it the channel's URL", who, key, a.secretsFile())
		default:
			if err := checkURL(v, a.AllowInsecureChannels); err != nil {
				why = fmt.Errorf("%s: the value of %s %w", who, key, err)
			}
		}
		if why == nil {
			r.urls[key] = v
			continue
		}
		if !drop {
			return nil, nil, why
		}
		bad[key] = true
		dropped = append(dropped, why)
	}
	if len(bad) > 0 {
		r.without(bad)
	}
	return r, dropped, nil
}

// without takes out of r every channel whose secret is in bad, and the
// heartbeat if its secret is: from the channel lines, the operator's
// list and each app's bound channels, so that nothing renders a URL
// file that is not there.
func (r *resolved) without(bad map[string]bool) {
	gone := map[string]bool{}
	for _, name := range r.order {
		if bad[r.channels[name].Secret] {
			gone[name] = true
			delete(r.channels, name)
		}
	}
	keep := func(names []string) []string {
		return slices.DeleteFunc(names, func(n string) bool { return gone[n] })
	}
	r.order, r.operator = keep(r.order), keep(r.operator)
	for app, chans := range r.appChannels {
		if chans = keep(chans); len(chans) == 0 {
			delete(r.appChannels, app)
		} else {
			r.appChannels[app] = chans
		}
	}
	if bad[r.heartbeat] {
		r.heartbeat = ""
	}
}

// checkURL refuses what Alertmanager must not be pointed at: it sends
// from the box's network namespace, which every app shares, so a URL to
// loopback or a private address would let whoever wrote it reach a
// listener an app opened or a database's HTTP port (DESIGN-notify.md,
// "Threat model"). Literal addresses are checked; a name is refused
// only when it is local by its form. A public name that resolves to a
// private address, or rebinds to one, is the residual the design
// accepts. The URL itself never appears in an error.
func checkURL(raw string, insecure bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("is not an absolute URL")
	}
	if insecure {
		if u.Scheme != "https" && u.Scheme != "http" {
			return errors.New("is not an http or https URL")
		}
		return nil
	}
	if u.Scheme != "https" {
		return errors.New("is not an https URL")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if addr, err := netip.ParseAddr(host); err == nil {
		if !publicAddr(addr) {
			return errors.New("is not to a public address (loopback, private, link-local and the like are refused)")
		}
		return nil
	}
	if !strings.Contains(host, ".") || host == "localhost" ||
		strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") ||
		strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".home.arpa") {
		return errors.New("is not to a public host name")
	}
	return nil
}

var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64, which can reach the above
	netip.MustParsePrefix("2001:db8::/32"),
}

func publicAddr(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsGlobalUnicast() || a.IsPrivate() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

func (a *App) secretsFile() string {
	if a.SecretsFile == "" {
		return DefaultSecretsFile
	}
	return a.SecretsFile
}
