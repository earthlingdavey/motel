package liveswap

import (
	"errors"
	"net/http"
	"net/netip"
	"time"

	"go.uber.org/zap"

	"github.com/smallhoursorg/motel/liveswap/access"
	"github.com/smallhoursorg/motel/liveswap/deploytrust"
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
// webhook's journal, and the clock (nil is time.Now).
type PersonGate struct {
	Registry *access.Registry
	Limiter  *deploytrust.Limiter
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
		}
		return Person{}, false, g.Limiter.Refuse(r, g.Logger, scopeKey, scopeValue, reason, fields...).Write(write)
	}
	name, _ := g.Registry.Name(v.Fingerprint)
	views, _ := g.Registry.Views(name, netip.Addr{})
	g.Limiter.Admit(r)
	StampVersion(w.Header())
	return Person{
		Name:      name,
		Token:     v,
		Principal: access.Principal{Kind: access.Person, Name: name, Views: views},
	}, true, nil
}
