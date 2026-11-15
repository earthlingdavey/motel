// Package client asks a box over HTTPS: the box and the credential a
// command uses, one GET with the retry rules every command shares, and
// what each kind of answer means as an exit code and a sentence. It
// decodes into liveswap/wire's types, the ones the box marshals, so
// there is one shape and nothing is copied; that package imports only
// the standard library, so motelctl links no Caddy.
package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/smallhoursorg/motel/liveswap/wire"
	"github.com/smallhoursorg/motel/motelctl/internal/sshkey"
	"github.com/smallhoursorg/motel/motelctl/internal/termtext"
)

// Credential is what a request carries in Authorization.
type Credential interface {
	// Authorization is the header's value, made fresh for each
	// request a retry sends.
	Authorization(ctx context.Context) (string, error)
}

// The headers motel stamps on every answer to a token it verified
// (liveswap.StampVersion), and never on a 401: the release, when the
// build links one, and the protocol number.
const (
	versionHeader  = "Motel-Version"
	protocolHeader = "Motel-Protocol"
)

// Protocol is the person-token protocol this client speaks: the
// Motel-Protocol a box answers with, 1 being the token form m1.
const Protocol = 1

// NewDeployToken takes MOTEL_TOKEN's value. A deploy token is one
// header-safe word (a JWT is base64url and dots); a value with a
// space, a line break or a control byte cannot be sent, and nothing is.
// The value itself is never quoted back: it is a credential.
func NewDeployToken(v string) (DeployToken, error) {
	for i := 0; i < len(v); i++ {
		if v[i] <= ' ' || v[i] >= 0x7f {
			return "", &Failure{Exit: ExitUsage, Message: "MOTEL_TOKEN holds something that is not a token (a space, a line break, a control or non-ASCII byte): set it to the deploy token alone"}
		}
	}
	return DeployToken(v), nil
}

// DeployToken is MOTEL_TOKEN: a deploy token, or an issuer's ID
// token, sent as Bearer. With it the reads work against any motel
// release, since a deployer of an app may read its status.
type DeployToken string

// Authorization is the token as Bearer.
func (t DeployToken) Authorization(context.Context) (string, error) {
	return "Bearer " + string(t), nil
}

// PersonToken is a token the person's own ssh key signs.
type PersonToken struct {
	Key    sshkey.Key
	Signer sshkey.Signer
	Box    Box
	Now    func() time.Time

	// The token last signed, and its times: reused while it has
	// minLeft of life, so a retry asks a hardware key for no second
	// touch.
	header   string
	iat, exp time.Time
}

// minLeft is the life a token must have left to be sent again.
const minLeft = 60 * time.Second

// Authorization is the token last signed while it has a minute of
// life left, and a new one otherwise.
func (p *PersonToken) Authorization(ctx context.Context) (string, error) {
	now := p.Now()
	if p.header != "" && p.exp.Sub(now) >= minLeft {
		return p.header, nil
	}
	h, err := sshkey.Token(ctx, p.Signer, p.Key, p.Box.Host, now)
	if err != nil {
		return "", err
	}
	p.header = h
	p.iat, p.exp = sshkey.Times(now)
	return h, nil
}

// TokenFor refuses to send MOTEL_TOKEN to a box other than MOTEL_URL's
// when both are set: the token was made for the box MOTEL_URL names,
// and another box, or whoever answers for it, would be handed it.
// Without MOTEL_URL nothing says which box the token is for.
func TokenFor(box Box, getenv func(string) string) error {
	v := getenv("MOTEL_URL")
	if v == "" {
		return nil
	}
	urlBox, _, err := parseHTTPS(v, "MOTEL_URL")
	if err != nil {
		return err
	}
	if urlBox != box {
		return &Failure{Exit: ExitUsage, Message: fmt.Sprintf(
			"the token in MOTEL_TOKEN belongs with MOTEL_URL's box, %s, and this command asks %s: name %s, or unset MOTEL_URL if the token is for %s, or unset MOTEL_TOKEN to sign with your own key",
			urlBox.Addr(), box.Addr(), urlBox.Addr(), box.Addr())}
	}
	return nil
}

// The waits before the one retry of a 401. For a person's token one
// second covers the 55 to 61 ms after every start in which motel
// refuses a valid token (#473), and the token sent is still good, so
// it is sent again. For a deploy token it is deploy.sh's three: a box
// refuses the first token under an issuer's new signing key while it
// fetches the key.
const (
	retryPerson = time.Second
	retryDeploy = 3 * time.Second
	// maxRetryAfter is the longest a 429's Retry-After is waited out;
	// past it the command says how long the box asked to wait instead.
	maxRetryAfter = 60 * time.Second
	// maxBody bounds what is read of an answer. The largest body a
	// read gets is a deploy record with its 8 KiB log tail.
	maxBody = 4 << 20
	// timeout bounds one request.
	timeout = 30 * time.Second
)

// Client asks one box.
type Client struct {
	Box  Box
	Cred Credential
	// HTTP is the client requests go through; nil is one with the
	// system's roots and a 30 s timeout. It never follows a redirect.
	HTTP *http.Client
	// Now and Sleep are the clock; a test sets its own.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
	// UserAgent names motelctl and its release.
	UserAgent string
	// Scope says whose deploy_trust a refused deploy token is judged
	// by, for the 401's sentence: "this app's" or "the box block's".
	Scope string
	// Roots names SSL_CERT_FILE or SSL_CERT_DIR, with its value, when
	// the environment sets one: the roots it names replace the
	// system's, and are then the likelier cause of a certificate that
	// does not verify.
	Roots []string
	// Notes is where a word that does not stop the command goes (a box
	// newer than this client): stderr. nil says nothing.
	Notes io.Writer

	noted bool
}

// Answer is a box's answer.
type Answer struct {
	Code   int
	Header http.Header
	Body   []byte
}

// NewHTTP is the client motelctl asks with: the system's roots, TLS
// 1.2 at least, the environment's proxy, no redirect followed (a
// motel webhook never redirects, and the token must not follow one).
func NewHTTP(rt http.RoundTripper) *http.Client {
	if rt == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		rt = t
	}
	return &http.Client{
		Transport:     rt,
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Get asks the box for path and query, with the rules every command
// shares: one retry of a 401 after a wait, with the same token while
// it has a minute of life left (a fresh one otherwise); one
// wait for a 429's Retry-After. A 401 or 429 that stands, and an
// answer that never came, are a *Failure; every other answer is
// returned for the command to read.
func (c *Client) Get(ctx context.Context, path string, query url.Values) (*Answer, error) {
	retried, waited := false, false
	for {
		ans, err := c.once(ctx, path, query)
		if err != nil {
			return nil, err
		}
		// A 401 is no answer to a verified token, so whatever it
		// carries says nothing of the box's protocol.
		if ans.Code != http.StatusUnauthorized {
			c.noteProtocol(ans.Header)
		}
		switch {
		case ans.Code == http.StatusUnauthorized && !retried:
			retried = true
			if err := c.Sleep(ctx, c.retry401()); err != nil {
				return nil, err
			}
			continue
		case ans.Code == http.StatusUnauthorized:
			return nil, c.refused(ans)
		case ans.Code == http.StatusTooManyRequests:
			// A wait past the cap, or none that reads, is not waited
			// out: the command says what the box asked instead.
			secs, ok := retryAfter(ans.Header, c.Now())
			if waited || !ok || secs > int64(maxRetryAfter/time.Second) {
				return nil, c.slowDown(secs, ok, waited)
			}
			waited = true
			if err := c.Sleep(ctx, max(time.Duration(secs)*time.Second, time.Second)); err != nil {
				return nil, err
			}
			continue
		}
		return ans, nil
	}
}

// noteProtocol reads Motel-Protocol where an answer carries it (only
// an answer to a token the box verified does) and says once, on Notes,
// when the box speaks a later protocol than this client: the command
// carries on, since a later protocol adds to what a box answers.
func (c *Client) noteProtocol(h http.Header) {
	n, ok := protocolOf(h)
	if !ok || n <= Protocol || c.noted || c.Notes == nil {
		return
	}
	c.noted = true
	_, _ = fmt.Fprintf(c.Notes, "motelctl: %s speaks protocol %d and this motelctl %d: the box is newer than this client, which may not show all it answers. Carrying on; a newer motelctl reads it all.\n",
		c.Box.Host, n, Protocol)
}

// protocolOf is the Motel-Protocol an answer carries: exactly one
// header, 1 to 9 decimal digits, not zero. Anything else is no
// protocol number.
func protocolOf(h http.Header) (int, bool) {
	v := h.Values(protocolHeader)
	if len(v) != 1 || !protocolRe.MatchString(v[0]) {
		return 0, false
	}
	n, err := strconv.Atoi(v[0])
	return n, err == nil && n > 0
}

// stamped says whether an answer carries motel's stamp, well formed:
// a protocol number, or one Motel-Version that reads as a release.
func stamped(h http.Header) bool {
	if _, ok := protocolOf(h); ok {
		return true
	}
	v := h.Values(versionHeader)
	return len(v) == 1 && releaseRe.MatchString(v[0])
}

var (
	protocolRe = regexp.MustCompile(`^[0-9]{1,9}$`)
	// releaseRe is a release as the Makefile links it in: v0.6.1, and
	// a pre-release or build suffix.
	releaseRe = regexp.MustCompile(`^v?[0-9]{1,9}\.[0-9]{1,9}\.[0-9]{1,9}([-+][0-9A-Za-z.+-]{1,64})?$`)
	// nameRe is a name as a person or signer line holds one
	// (box/proof/signers.go), at most 256 bytes.
	nameRe = regexp.MustCompile(`^[A-Za-z0-9._@+-]{1,256}$`)
	// roleRe is a role as a box names one, in a 403's needs or a
	// whoami's roles: a word the client may say as a role.
	roleRe = regexp.MustCompile(`^[a-z][a-z_-]{0,31}$`)
)

func (c *Client) retry401() time.Duration {
	if _, ok := c.Cred.(DeployToken); ok {
		return retryDeploy
	}
	return retryPerson
}

// once sends one GET.
func (c *Client) once(ctx context.Context, path string, query url.Values) (*Answer, error) {
	u := url.URL{Scheme: "https", Host: c.Box.Addr(), Path: path, RawQuery: query.Encode()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, &Failure{Exit: ExitUsage, Message: err.Error()}
	}
	auth, err := c.Cred.Authorization(ctx)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	hc := c.HTTP
	if hc == nil {
		hc = NewHTTP(nil)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, c.unreachable(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, c.unreachable(ctx, err)
	}
	if len(body) > maxBody {
		return nil, &Failure{Exit: ExitUnknown, Message: fmt.Sprintf("%s answered with more than %d bytes, which is not motel's answer to a read. Is this the box's deploy host?", c.Box.Addr(), maxBody)}
	}
	return &Answer{Code: resp.StatusCode, Header: resp.Header, Body: body}, nil
}

// unreachable says why no answer came: a certificate that did not
// verify (never continued past), nothing answering, or a name not
// found.
func (c *Client) unreachable(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	box := c.Box.Addr()
	var (
		verify   *tls.CertificateVerificationError
		unknown  x509.UnknownAuthorityError
		hostname x509.HostnameError
		invalid  x509.CertificateInvalidError
		dns      *net.DNSError
		netErr   net.Error
	)
	switch {
	case errors.As(err, &verify), errors.As(err, &unknown), errors.As(err, &hostname), errors.As(err, &invalid):
		// The reason quotes the certificate's names, which whoever
		// answered chose.
		why := err.Error()
		if errors.As(err, &verify) {
			why = verify.Err.Error()
		}
		if len(c.Roots) > 0 {
			return &Failure{Exit: ExitCertificate, Message: fmt.Sprintf(
				"The certificate of %s did not verify; the check says: %s. motelctl never sends a token over an unverified connection. %s is set, and replaces the system's roots with the ones it names: that is the likelier cause. Else, as root on the box: journalctl -u motel -g certificate",
				box, termtext.Quote(why), termtext.From(strings.Join(c.Roots, " and ")))}
		}
		return &Failure{Exit: ExitCertificate, Message: fmt.Sprintf(
			"The certificate of %s did not verify; the check says: %s. motelctl never sends a token over an unverified connection. As root on the box: journalctl -u motel -g certificate",
			box, termtext.Quote(why))}
	case errors.As(err, &dns):
		return &Failure{Exit: ExitUnknown, Message: fmt.Sprintf(
			"%s was not found (%s): is the box's name right?", c.Box.Host, termtext.From(dns.Err))}
	}
	var why string
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		why = "connection refused"
	case errors.Is(err, syscall.ECONNRESET):
		why = "connection reset"
	case errors.As(err, &netErr) && netErr.Timeout():
		why = fmt.Sprintf("no answer in %d s", int(timeout/time.Second))
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		why = "the connection closed"
	default:
		why = termtext.From(err.Error()).String()
	}
	return &Failure{Exit: ExitUnknown, Message: fmt.Sprintf(
		"%s did not answer on %s (%s). If motel is down, as root on the box: systemctl status motel; journalctl -u motel -n 50. The box's off-box heartbeat also reports a stopped motel.",
		c.Box.Host, c.Box.PortOr443(), why)}
}

// refused is a 401 that stood after the retry. Its body says nothing
// of why: a box answers every refusal with the same sentence, and
// names nothing of motel in it, so the causes are named from what this
// side knows, all of them, since the client cannot tell them apart. A
// deploy token's do not involve this machine's clock. A person's token
// does: when the answer's Date is outside what the box takes for the
// token just sent (its iat and exp, with the box's allowance either
// way), the clock is named first, and the other causes stay, since a
// Date can be wrong or a gateway's. A box whose motel is too old to
// take a person's token at all is one of them: it answers the same 401.
func (c *Client) refused(ans *Answer) error {
	box := c.Box.Host
	cred, ok := c.Cred.(*PersonToken)
	if !ok {
		scope := c.Scope
		if scope == "" {
			scope = "this app's"
		}
		return &Failure{Exit: ExitRefused, Message: fmt.Sprintf(
			"%s refused the token in MOTEL_TOKEN (sent as Bearer). Most often %s deploy_trust does not accept the key it was minted with, or the audience, subject or claim it pins. Or the app is not one the box knows: an unknown app answers the same 401. Or the box could not consult the token's issuer. The box's journal (journalctl -u motel, 'webhook auth failed') names the check that refused it.",
			box, scope)}
	}
	clock := ""
	if date, err := http.ParseTime(ans.Header.Get("Date")); err == nil && !cred.exp.IsZero() &&
		(date.Before(cred.iat.Add(-sshkey.BoxSkew)) || date.After(cred.exp.Add(sshkey.BoxSkew))) {
		skew := c.Now().Sub(date)
		way := "behind it"
		if skew > 0 {
			way = "ahead of it"
		}
		clock = fmt.Sprintf(" This machine's clock differs from the box's by %d s (%s; the box's Date is %s), more than a token made here can bridge: if the box's Date is right, set this machine's clock.",
			int64(math.Round(math.Abs(skew.Seconds()))), way, date.UTC().Format(time.RFC1123))
	}
	lead := " The box says no more to a key it does not accept, so it is one of these:"
	if clock != "" {
		lead = clock + " Or, as the box says no more, one of these:"
	}
	return &Failure{Exit: ExitRefused, Message: fmt.Sprintf(
		"%s did not accept your key %s (%s).%s no person or signer line on the box lists it; your address is outside the from list on every grant you hold; %s is not the box's main name; or the box's motel is too old to take a key's token at all. Send the operator the line `motelctl key` prints.",
		box, cred.Key.Public.Fingerprint(), termtext.From(cred.Key.Shown), lead, box)}
}

// slowDown is a 429 that stood after the wait, or one that asked for a
// longer wait than motelctl makes, or one whose wait did not read.
func (c *Client) slowDown(secs int64, ok, waited bool) error {
	switch {
	case !ok:
		return &Failure{Exit: ExitUnknown, Message: fmt.Sprintf("%s asked you to slow down; try again later.", c.Box.Host)}
	case waited:
		return &Failure{Exit: ExitUnknown, Message: fmt.Sprintf("%s asked you to slow down; try again in %d s.", c.Box.Host, max(secs, 1))}
	}
	return &Failure{Exit: ExitUnknown, Message: fmt.Sprintf(
		"%s asked you to slow down: the box asks you to wait %d s, longer than motelctl waits; try again then.", c.Box.Host, secs)}
}

// retryAfter reads Retry-After, in seconds or as a date: the seconds
// the box asks for, never negative, kept as a number so that no value
// can overflow into a short wait (the caller compares it with the cap
// before it becomes a duration). ok is false when it does not read.
func retryAfter(h http.Header, now time.Time) (int64, bool) {
	v := h.Get("Retry-After")
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return n, n >= 0
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return 0, false
	}
	return max(t.Unix()-now.Unix(), 0), true
}

// Read decodes a 200's body into v and returns nil, or says why the
// answer is not the one asked for: the box's own refusal (403, 404, a
// 409 or a 5xx in motel's words, each with the box's sentence quoted),
// or an answer that is not motel's. No message carries a decoder's
// error, which quotes what the body holds: a field that does not read
// is named by the client's own name for it, and nothing of its value.
func (c *Client) Read(ans *Answer, v any) error {
	if ans.Code == http.StatusOK {
		missing := identity(v).missing(ans.Body)
		err := decodeObject(ans, v)
		var syntax *json.SyntaxError
		switch {
		case errors.Is(err, errNotJSON), errors.As(err, &syntax):
			return c.notMotel(ans, "not JSON")
		case errors.Is(err, errNotObject):
			return c.notMotel(ans, "not a JSON object")
		case len(missing) > 0:
			return c.notMotel(ans, "it has no "+fieldList(missing))
		case err != nil:
			// motel's answer, by its fields, with a value that did not
			// decode: the box's response filter replaces a value equal
			// to a secret, a timestamp's included, with its marker.
			what := "a value in it"
			if f := badField(ans.Body, v); f != "" {
				what = "its " + f
			}
			return &Failure{Exit: ExitUnknown, Message: fmt.Sprintf(
				"%s answered, but %s could not be read: the box's response filter may have replaced it. The box's journal says which key it replaced.",
				c.Box.Addr(), what)}
		}
		if why := odd(v); why != "" {
			return c.notMotel(ans, why)
		}
		return nil
	}
	if ans.Code == http.StatusForbidden {
		return c.forbidden(ans)
	}
	var e wire.Error
	if decodeObject(ans, &e) != nil || e.Error == "" {
		return c.notMotel(ans, "")
	}
	said := termtext.Quote(e.Error)
	if ans.Code == http.StatusNotFound {
		return &Failure{Exit: ExitNotFound, Message: fmt.Sprintf("%s answered 404; the box says: %s", c.Box.Host, said), Body: ans.Body}
	}
	// A gateway in front of a box can answer a 5xx in JSON too. Motel
	// stamps Motel-Version on every answer to a token it verified, and,
	// since people's tokens, Motel-Protocol too (which a build that
	// links no release sends alone), so a 5xx without either is not
	// known to be motel's own.
	if ans.Code >= 500 && !stamped(ans.Header) {
		return &Failure{Exit: ExitUnknown, Message: fmt.Sprintf(
			"Something at %s answered %d without %s, so not known to be motel's own answer; it says: %s", c.Box.Addr(), ans.Code, versionHeader, said)}
	}
	return &Failure{Exit: ExitFailed, Message: fmt.Sprintf("%s answered %d; the box says: %s", c.Box.Host, ans.Code, said), Body: ans.Body}
}

// odd says why a body that decoded is still not one an honest box
// sends: a value outside the grammar the box holds it to. The client
// never prints such a value, so it never says it either.
func odd(v any) string {
	switch a := v.(type) {
	case *wire.Apps:
		for _, name := range a.Apps {
			if !appRe.MatchString(name) {
				return "a name in its list is no app name"
			}
		}
	case *wire.WhoAmI:
		// Every value whoami prints as the box's is held to the grammar
		// the box holds it to: the name to a person line's, the release
		// to the Makefile's; the key and the box are compared, not
		// printed.
		if !nameRe.MatchString(a.Name) {
			return "its name is not one a box takes"
		}
		if a.Version != "" && !releaseRe.MatchString(a.Version) {
			return "its version is not a release"
		}
		for _, r := range a.Roles {
			if !roleRe.MatchString(r.Role) || !scopeOK(r.Scope) {
				return "a role in it is not one a box names"
			}
		}
	}
	return ""
}

// scopeOK is a role's scope as a box writes one: "box", or "app:" and
// an app's name.
func scopeOK(s string) bool {
	if s == "box" {
		return true
	}
	name, ok := strings.CutPrefix(s, "app:")
	return ok && appRe.MatchString(name)
}

// badField is the first top-level field of v's type whose value in
// body does not decode into it, by the wire type's own JSON name; ""
// when none is found (the decoder matched a key by another case).
func badField(body []byte, v any) string {
	var m map[string]json.RawMessage
	t := reflect.TypeOf(v)
	if json.Unmarshal(body, &m) != nil || t.Kind() != reflect.Pointer || t.Elem().Kind() != reflect.Struct {
		return ""
	}
	t = t.Elem()
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		raw, ok := m[name]
		if name == "" || name == "-" || !ok {
			continue
		}
		if json.Unmarshal(raw, reflect.New(f.Type).Interface()) != nil {
			return name
		}
	}
	return ""
}

// forbidden is a 403, which exits 5 whatever its body, and whose body
// --json prints as received whenever it is JSON. The box's words are
// never the client's advice: needs and scope are used only when they
// are words of their grammar (roleRe; "app" or "box"), and the
// sentence is built from them alone, so an app that exists and one
// that does not read the same and no app is named. A 403 that does not
// say what it needs in those words is said as a refusal, with the
// box's own sentence, if any, quoted as the box's.
func (c *Client) forbidden(ans *Answer) *Failure {
	var body []byte
	if json.Valid(ans.Body) {
		body = ans.Body
	}
	var f wire.Forbidden
	if decodeObject(ans, &f) != nil {
		f = wire.Forbidden{Error: f.Error} // a field of the wrong type: nothing but the sentence is read
	}
	_, person := c.Cred.(*PersonToken)
	whoami := ""
	if person {
		whoami = " `motelctl whoami` lists the roles you hold from here."
	}
	on := map[string]string{"app": "this app", "box": "the box"}[f.Scope]
	if !roleRe.MatchString(f.Needs) || on == "" {
		said := "."
		if f.Error != "" {
			said = "; the box says: " + termtext.Quote(f.Error).String() + "."
		}
		return &Failure{Exit: ExitForbidden, Body: body, Message: fmt.Sprintf(
			"%s refused this (HTTP 403) and did not say what it needs%s%s", c.Box.Host, said, whoami)}
	}
	var needs string
	switch f.Needs {
	case "viewer":
		needs = "the viewer role on " + on
	case "deployer":
		needs = "a deploy token of " + on
	case "key":
		needs = "a token your own key signs"
	default:
		needs = "the " + f.Needs + " role on " + on
	}
	if person {
		return &Failure{Exit: ExitForbidden, Body: body, Message: fmt.Sprintf(
			"%s does not allow your key this: it needs %s.%s", c.Box.Host, needs, whoami)}
	}
	hint := ""
	if f.Needs == "key" {
		hint = " Unset MOTEL_TOKEN, or name your key with --key."
	}
	return &Failure{Exit: ExitForbidden, Body: body, Message: fmt.Sprintf(
		"%s does not allow the token in MOTEL_TOKEN this: it needs %s.%s", c.Box.Host, needs, hint)}
}

func (c *Client) notMotel(ans *Answer, why string) error {
	if why != "" {
		why = ": " + termtext.From(why).String()
	}
	return &Failure{Exit: ExitUnknown, Message: fmt.Sprintf(
		"Something at %s answered (HTTP %d, not motel's answer%s). Is this the box's deploy host?", c.Box.Addr(), ans.Code, why)}
}

// shape is the fields that make a body the answer it is for: the ones
// the handlers always send. Any other field may be missing, and an
// unknown one is taken: a newer box adds fields.
type shape struct {
	// strs are strings that are never empty; lists arrays, empty or
	// not; counts integers of at least 1; times strings (decoding
	// reads them as times).
	strs, lists, counts, times []string
}

func identity(v any) shape {
	switch v.(type) {
	case *wire.Status:
		return shape{strs: []string{"app", "phase"}}
	case *wire.DeployResult:
		return shape{strs: []string{"version", "status"}}
	case *wire.BoxStatus:
		return shape{strs: []string{"box_webhook", "commit"}}
	case *wire.WhoAmI:
		return shape{strs: []string{"name", "key", "box"}, lists: []string{"roles"}, counts: []string{"protocol"}, times: []string{"token_expires_at"}}
	case *wire.Apps:
		return shape{lists: []string{"apps"}}
	}
	return shape{}
}

// missing is the fields of the shape body lacks, has as null, or has
// in another form, in the shape's order; all of them when body is no
// object. A required field that is null is missing, whatever its kind.
func (s shape) missing(body []byte) []string {
	var m map[string]json.RawMessage
	ok := json.Unmarshal(body, &m) == nil
	var out []string
	check := func(keys []string, good func(raw json.RawMessage) bool) {
		for _, k := range keys {
			raw, there := m[k]
			if !ok || !there || string(bytes.TrimSpace(raw)) == "null" || !good(raw) {
				out = append(out, k)
			}
		}
	}
	check(s.strs, func(raw json.RawMessage) bool {
		var v string
		return json.Unmarshal(raw, &v) == nil && v != ""
	})
	check(s.lists, func(raw json.RawMessage) bool {
		var a []json.RawMessage
		return json.Unmarshal(raw, &a) == nil
	})
	check(s.counts, func(raw json.RawMessage) bool {
		var n int64
		return json.Unmarshal(raw, &n) == nil && n >= 1
	})
	check(s.times, func(raw json.RawMessage) bool {
		var v string
		return json.Unmarshal(raw, &v) == nil && v != ""
	})
	return out
}

// fieldList names fields as a sentence does: "a", "a and b", "a, b
// and c".
func fieldList(f []string) string {
	if len(f) < 2 {
		return strings.Join(f, "")
	}
	return strings.Join(f[:len(f)-1], ", ") + " and " + f[len(f)-1]
}

// errNotJSON and errNotObject are a body that is not motel's shape at
// all.
var (
	errNotJSON   = errors.New("not JSON")
	errNotObject = errors.New("not a JSON object")
)

// decodeObject decodes a JSON body: motel answers in application/json
// and with one object.
func decodeObject(ans *Answer, v any) error {
	mt, _, err := mime.ParseMediaType(ans.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return errNotJSON
	}
	body := bytes.TrimSpace(ans.Body)
	if len(body) == 0 || body[0] != '{' {
		return errNotObject
	}
	return json.Unmarshal(body, v)
}
