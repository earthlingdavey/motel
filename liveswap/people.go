package liveswap

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"

	"github.com/smallhoursorg/motel/liveswap/access"
	"github.com/smallhoursorg/motel/liveswap/deploytrust"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// A person reaches either webhook with a token their own ssh key signs,
// in the Authorization scheme access.TokenScheme. Such a request is a
// person's only by its scheme, and only access.CheckToken judges it: it
// never meets a deploy token's sources, and a deploy token never meets
// CheckToken. Every refusal is the deploy token's flat 401, byte for
// byte, through the same failure budgets, with the reason (and the key
// and its person, once the key was found listed) in the journal alone.

// PersonGate is what a webhook admits a person through: the registry
// of this load (nil admits no one), the shared failure budgets, the
// rate per person (access.SharedRate; nil, in a test, takes every
// request), the webhook's journal, and the clock (nil is time.Now).
type PersonGate struct {
	Registry *access.Registry
	Limiter  *deploytrust.Limiter
	Rate     *access.Rate
	Logger   *zap.Logger
	Now      func() time.Time
}

// Person is a request whose person token verified.
type Person struct {
	Name      string
	Token     access.VerifiedToken
	Principal access.Principal
}

// AdmitPerson judges a request in the person token's scheme
// (access.HasPersonScheme). scopeKey and scopeValue are the webhook's
// one journal field, as for Limiter.Authenticate. With ok false the
// refusal is written through write, the webhook's filtered writer, and
// err is the write's. With ok true nothing is written, and the
// response's headers already name the release and the protocol
// (StampVersion): every answer from here is to a verified token.
func AdmitPerson(w http.ResponseWriter, r *http.Request, g PersonGate, scopeKey, scopeValue string, write func(code int, body any) error) (p Person, ok bool, err error) {
	now := time.Now
	if g.Now != nil {
		now = g.Now
	}
	v, err := access.CheckToken(access.TokenRequest{
		Authorization: r.Header.Values("Authorization"),
		Method:        r.Method,
		Host:          r.Host,
	}, g.Registry.Identity(), now(), g.Registry.Lookup)
	if err != nil {
		reason := err.Error()
		var fields []zap.Field
		var ref *access.TokenRefusal
		if errors.As(err, &ref) && ref.Fingerprint != "" {
			name, _ := g.Registry.Name(ref.Fingerprint)
			fields = append(fields, zap.String("key", ref.Fingerprint), zap.String("person", name))
			if ref.Reason == "host" {
				// A proxy in front that rewrites Host refuses every person:
				// the line says what arrived, cut to a hostname's length.
				fields = append(fields, zap.String("host", deploytrust.CutRunes(r.Host, 253)))
			}
		}
		return Person{}, false, g.Limiter.Refuse(r, g.Logger, scopeKey, scopeValue, reason, fields...).Write(write)
	}
	name, _ := g.Registry.Name(v.Fingerprint)
	// The address list: a grant whose list does not hold the request's
	// address does not exist for this request. A person left with none,
	// who holds some, is refused as a stranger is, so a key used from
	// elsewhere learns nothing; the journal names the key, the person,
	// the address the lists were held to (Caddy's client_ip) and the
	// peer it came through, inside the same budgets.
	addr, why := clientAddr(r)
	views, granted := g.Registry.Views(name, addr)
	if granted && views.Empty() {
		shown := why
		if addr.IsValid() {
			shown = addr.String()
		}
		return Person{}, false, g.Limiter.Refuse(r, g.Logger, scopeKey, scopeValue, "person token refused: outside-every-address-list",
			zap.String("key", v.Fingerprint), zap.String("person", name), zap.String("address", shown), zap.String("peer", r.RemoteAddr)).Write(write)
	}
	g.Limiter.Admit(r)
	StampVersion(w.Header())
	// The rate, after the address and before Decide: a person's own
	// budget of accepted requests, told when to come back.
	if g.Rate != nil {
		if ok, wait, log := g.Rate.Take(name); !ok {
			secs := max(1, int(math.Ceil(wait.Seconds())))
			if log {
				g.Logger.Warn("access: person throttled", zap.String("person", name), zap.String(scopeKey, scopeValue),
					zap.Int("per_second", access.RatePerSecond), zap.Int("burst", access.RateBurst))
			}
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			return Person{}, false, write(http.StatusTooManyRequests, wire.Error{Error: fmt.Sprintf("too many requests; retry after %d s", secs)})
		}
	}
	return Person{
		Name:      name,
		Token:     v,
		Principal: access.Principal{Kind: access.Person, Name: name, Views: views},
	}, true, nil
}

// clientAddr is the address a request came from as a grant's address
// list is compared with: Caddy's client_ip (the peer, or the address a
// proxy in the server's trusted_proxies forwarded for), whole, not
// grouped as the failure budgets group it; the peer when Caddy set
// none. The zero Addr, which no list holds, when neither parses, or
// when a trusted proxy forwarded the request and the server does not
// set trusted_proxies_strict: Caddy then takes the left-most address in
// X-Forwarded-For, which the client writes and a proxy appends to, so a
// client could name any address. why says which, for the journal.
func clientAddr(r *http.Request) (netip.Addr, string) {
	if trusted, _ := caddyhttp.GetVar(r.Context(), caddyhttp.TrustedProxyVarKey).(bool); trusted {
		srv, _ := r.Context().Value(caddyhttp.ServerCtxKey).(*caddyhttp.Server)
		if srv == nil || srv.TrustedProxiesStrict == 0 {
			return netip.Addr{}, "forwarded by a trusted proxy on a server without trusted_proxies_strict"
		}
	}
	addr := r.RemoteAddr
	if v, ok := caddyhttp.GetVar(r.Context(), caddyhttp.ClientIPVarKey).(string); ok && v != "" {
		addr = v
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		addr = host
	}
	a, err := netip.ParseAddr(addr)
	if err != nil {
		return netip.Addr{}, "unknown"
	}
	return a.Unmap(), ""
}
