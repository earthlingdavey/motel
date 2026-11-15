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
	"strconv"
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

// versionHeader is the release motel stamps on every answer to a
// verified token (liveswap.StampVersion); never on a 401.
const versionHeader = "Motel-Version"

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

// PersonToken is a token the person's own ssh key signs, made fresh
// for each request.
type PersonToken struct {
	Key    sshkey.Key
	Signer sshkey.Signer
	Box    Box
	Now    func() time.Time
}

// Authorization signs a new token for the box.
func (p *PersonToken) Authorization(ctx context.Context) (string, error) {
	return sshkey.Token(ctx, p.Signer, p.Key, p.Box.Host, p.Now())
}

// The waits before the one retry of a 401. For a person's token one
// second covers the 55 to 61 ms after every start in which motel
// refuses a valid token (#473). For a deploy token it is deploy.sh's
// three: a box refuses the first token under an issuer's new signing
// key while it fetches the key.
const (
	retryPerson = time.Second
	retryDeploy = 3 * time.Second
	// maxRetryAfter is the longest a 429's Retry-After is waited out;
	// past it the command says when to try again instead.
	maxRetryAfter = 60 * time.Second
	// clockLimit is how far apart the box's Date and this machine's
	// clock may be before a 401 names the clock as its cause: a box
	// takes a token within 60 s of its own clock.
	clockLimit = 50 * time.Second
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
// shares: one retry of a 401 after a wait, with a fresh token; one
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
			// out: the command says to try later.
			d, ok := retryAfter(ans.Header, c.Now())
			if waited || !ok {
				return nil, c.slowDown(d, ok)
			}
			waited = true
			if err := c.Sleep(ctx, d); err != nil {
				return nil, err
			}
			continue
		}
		return ans, nil
	}
}

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
		return &Failure{Exit: ExitCertificate, Message: fmt.Sprintf(
			"The certificate of %s did not verify (%s). motelctl never sends a token over an unverified connection. As root on the box: journalctl -u motel -g certificate",
			box, termtext.From(why))}
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
// of why: a box answers every refusal with the same sentence, so the
// causes are named from what this side knows, and the clock, which the
// answer's Date shows, first when it is off.
func (c *Client) refused(ans *Answer) error {
	box := c.Box.Host
	clock := ""
	if date, err := http.ParseTime(ans.Header.Get("Date")); err == nil {
		skew := c.Now().Sub(date)
		secs := int(math.Round(math.Abs(skew.Seconds())))
		if math.Abs(skew.Seconds()) > clockLimit.Seconds() {
			way := "behind"
			if skew > 0 {
				way = "ahead of"
			}
			return &Failure{Exit: ExitRefused, Message: fmt.Sprintf(
				"%s did not accept the token: this machine's clock is %d s %s the box's (its Date is %s), and a box takes a token only within 60 s of its own clock. Set the clock that is wrong, and try again.",
				box, secs, way, date.UTC().Format(time.RFC1123))}
		}
		clock = fmt.Sprintf(" Your clock is within %d s of the box's.", max(secs, 1))
	}
	switch cred := c.Cred.(type) {
	case *PersonToken:
		return &Failure{Exit: ExitRefused, Message: fmt.Sprintf(
			"%s did not accept your key %s (%s). Either no person line on the box lists it, your address is outside the address list on your grants, or %s is not the box's main name.%s Send the operator the line `motelctl key` prints.",
			box, cred.Key.Public.Fingerprint(), cred.Key.Shown, box, clock)}
	default:
		scope := c.Scope
		if scope == "" {
			scope = "this app's"
		}
		return &Failure{Exit: ExitRefused, Message: fmt.Sprintf(
			"%s refused the token in MOTEL_TOKEN (sent as Bearer). Most often %s deploy_trust does not accept the key it was minted with, or the audience, subject or claim it pins. Or the app is not one the box knows: an unknown app answers the same 401. Or the box could not consult the token's issuer.%s The box's journal (journalctl -u motel, 'webhook auth failed') names the check that refused it.",
			box, scope, clock)}
	}
}

// slowDown is a 429 that stood, or one that asked for a longer wait
// than motelctl makes.
func (c *Client) slowDown(d time.Duration, ok bool) error {
	when := "later"
	if ok {
		when = fmt.Sprintf("in %d s", int(math.Ceil(d.Seconds())))
	}
	return &Failure{Exit: ExitUnknown, Message: fmt.Sprintf("%s asked you to slow down; try again %s.", c.Box.Host, when)}
}

// retryAfter reads Retry-After, in seconds or as a date, and says
// whether it is a wait motelctl makes: at least one second, at most
// maxRetryAfter. The number is compared with the cap before it becomes
// a duration, so no value can overflow into a short wait; a negative
// one, one past the cap, or one that does not read is no wait.
func retryAfter(h http.Header, now time.Time) (time.Duration, bool) {
	v := h.Get("Retry-After")
	capSecs := int64(maxRetryAfter / time.Second)
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n < 0 || n > capSecs {
			return 0, false
		}
		return max(time.Duration(n)*time.Second, time.Second), true
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return 0, false
	}
	d := t.Sub(now).Round(time.Second)
	if d > maxRetryAfter {
		return 0, false
	}
	return max(d, time.Second), true
}

// Read decodes a 200's body into v and returns nil, or says why the
// answer is not the one asked for: the box's own refusal (403, 404, a
// 409 or a 5xx in motel's words, each with the box's sentence), or an
// answer that is not motel's.
func (c *Client) Read(ans *Answer, v any) error {
	if ans.Code == http.StatusOK {
		if err := decodeObject(ans, v); err != nil {
			return c.notMotel(ans, err.Error())
		}
		return nil
	}
	var e wire.Error
	if decodeObject(ans, &e) != nil || e.Error == "" {
		return c.notMotel(ans, "")
	}
	said := termtext.From(e.Error)
	switch ans.Code {
	case http.StatusForbidden:
		return &Failure{Exit: ExitForbidden, Message: fmt.Sprintf("%s does not allow this: %s", c.Box.Host, said), Body: ans.Body}
	case http.StatusNotFound:
		return &Failure{Exit: ExitNotFound, Message: fmt.Sprintf("%s: %s", c.Box.Host, said), Body: ans.Body}
	}
	// A gateway in front of a box can answer a 5xx in JSON too. Motel
	// stamps Motel-Version on every answer to a token it verified, so
	// a 5xx without it is not known to be motel's own.
	if ans.Code >= 500 && ans.Header.Get(versionHeader) == "" {
		return &Failure{Exit: ExitUnknown, Message: fmt.Sprintf(
			"Something at %s answered %d without %s, so not known to be motel's own answer: %s", c.Box.Addr(), ans.Code, versionHeader, said)}
	}
	return &Failure{Exit: ExitFailed, Message: fmt.Sprintf("%s answered %d: %s", c.Box.Host, ans.Code, said), Body: ans.Body}
}

func (c *Client) notMotel(ans *Answer, why string) error {
	if why != "" {
		why = ": " + termtext.From(why).String()
	}
	return &Failure{Exit: ExitUnknown, Message: fmt.Sprintf(
		"Something at %s answered (HTTP %d, not motel's answer%s). Is this the box's deploy host?", c.Box.Addr(), ans.Code, why)}
}

// decodeObject decodes a JSON body: motel answers in application/json
// and with one object.
func decodeObject(ans *Answer, v any) error {
	mt, _, err := mime.ParseMediaType(ans.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return errors.New("not JSON")
	}
	body := bytes.TrimSpace(ans.Body)
	if len(body) == 0 || body[0] != '{' {
		return errors.New("not a JSON object")
	}
	return json.Unmarshal(body, v)
}
