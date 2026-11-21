// Package client asks a box over HTTPS: the box and the credential a
// command uses, one GET with the retry rules every command shares, and
// what each kind of answer means as an exit code and a sentence. It
// decodes into liveswap/wire's types, the ones the box marshals, so
// there is one shape and nothing is copied; that package imports only
// the standard library, so the client links no Caddy.
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
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/smallhoursorg/motel/client/internal/sshkey"
	"github.com/smallhoursorg/motel/client/internal/termtext"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// Credential is what a request carries in Authorization.
type Credential interface {
	// Authorization is the header's value, made fresh for each
	// request a retry sends.
	Authorization(ctx context.Context) (string, error)
}

// Protocol is the person-token protocol this client speaks, as
// whoami's protocol names a box's, 1 being the token form m1.
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
	// Life is how long each token lasts: zero is a command's own
	// (sshkey's), and the TUI asks for sshkey.MaxLife.
	Life time.Duration

	// The token last signed, and its times: reused while it has
	// minLeft of life, so a retry asks a hardware key for no second
	// touch.
	header   string
	iat, exp time.Time
	// forget, set by Forget from any goroutine, drops the token last
	// signed before the next request.
	forget atomic.Bool
}

// Forget drops the token last signed, so the next request signs
// afresh: the TUI's r after a refusal, which a token refused for its
// times would otherwise meet again until it expired.
func (p *PersonToken) Forget() { p.forget.Store(true) }

// minLeft is the life a token must have left to be sent again.
const minLeft = 60 * time.Second

// Authorization is the token last signed while it has a minute of
// life left, and a new one otherwise.
func (p *PersonToken) Authorization(ctx context.Context) (string, error) {
	now := p.Now()
	if p.forget.Swap(false) {
		p.header = ""
	}
	if p.header != "" && p.exp.Sub(now) >= minLeft {
		return p.header, nil
	}
	h, err := sshkey.TokenLasting(ctx, p.Signer, p.Key, p.Box.Host, now, p.Life)
	if err != nil {
		return "", err
	}
	p.header = h
	p.iat, p.exp = sshkey.TimesLasting(now, p.Life)
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

// The waits before the one retry of a 401. For a person's token it is
// one second, and the token sent is still good, so it is sent again.
// It was written for the moments after a start in which motel refused
// a valid token (#473); that window was a deploy token's only (a
// person's registry is built before any request is served, and
// e2e/box/people.sh counts no person refused across restarts), and it
// is closed. No case is known that a person's retry covers now. For a
// deploy token it is deploy.sh's three: a box refuses the first token
// under an issuer's new signing key while it fetches the key.
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
	// UserAgent names motel and its release.
	UserAgent string
	// Scope says whose deploy_trust a refused deploy token is judged
	// by, for the 401's sentence: "this app's" or "the box block's".
	Scope string
	// Roots names SSL_CERT_FILE or SSL_CERT_DIR, with its value, when
	// the environment sets one: the roots it names replace the
	// system's, and are then the likelier cause of a certificate that
	// does not verify.
	Roots []string
	// NoWait returns a 429 at once, with the wait it asks for, rather
	// than waiting it out: the TUI's refresh engine waits it out for
	// every read it makes, so one read does not hold the rest.
	NoWait bool
}

// Expires is when the token last signed stops being taken, and zero
// before one is signed: the TUI shows it.
func (p *PersonToken) Expires() time.Time { return p.exp }

// Answer is a box's answer.
type Answer struct {
	Code   int
	Header http.Header
	Body   []byte
}

// NewHTTP is the HTTP client it asks with: the system's roots, TLS
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
			if waited || !ok || secs > int64(maxRetryAfter/time.Second) || c.NoWait {
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

var (
	// releaseRe is a release as the Makefile links it in, as whoami's
	// version names it: v0.6.1, and a pre-release or build suffix.
	releaseRe = regexp.MustCompile(`^v?[0-9]{1,9}\.[0-9]{1,9}\.[0-9]{1,9}([-+][0-9A-Za-z.+-]{1,64})?$`)
	// nameRe is a name as a person or signer line holds one
	// (box/proof/signers.go), at most 256 bytes.
	nameRe = regexp.MustCompile(`^[A-Za-z0-9._@+-]{1,256}$`)
	// roleRe is the shape of a role in a whoami's roles. A box grants
	// two today, viewer and logs; the shape is wider so that a later
	// motel's role does not make the whole answer unreadable, and a word
	// that is neither is printed quoted, as the box's (cli's whoamiView).
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
		return nil, &Failure{Exit: ExitUnknown, Status: resp.StatusCode, Message: fmt.Sprintf("%s answered with more than %d bytes, which is not moteld's answer to a read. Is this the box's deploy host?", c.Box.Addr(), maxBody)}
	}
	return &Answer{Code: resp.StatusCode, Header: resp.Header, Body: body}, nil
}

// unreachable says why no answer was read: a certificate that did not
// verify (never continued past), a name not found, nothing to connect
// to, a timeout, a connection closed, or something that answered in no
// HTTP the client reads.
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
		// The x509 error's own words, without the request's around them.
		var why string
		switch {
		case errors.As(err, &verify):
			why = verify.Err.Error()
		case errors.As(err, &hostname):
			why = hostname.Error()
		case errors.As(err, &unknown):
			why = unknown.Error()
		default:
			why = invalid.Error()
		}
		if len(c.Roots) > 0 {
			return &Failure{Exit: ExitCertificate, Message: fmt.Sprintf(
				"The certificate of %s did not verify; the check says: %s. motel never sends a token over an unverified connection. %s is set, and replaces the system's roots with the ones it names: that is the likelier cause. Else, as root on the box: journalctl -u moteld -g certificate",
				box, termtext.Quote(why), termtext.From(strings.Join(c.Roots, " and ")))}
		}
		return &Failure{Exit: ExitCertificate, Message: fmt.Sprintf(
			"The certificate of %s did not verify; the check says: %s. motel never sends a token over an unverified connection. As root on the box: journalctl -u moteld -g certificate",
			box, termtext.Quote(why))}
	case errors.As(err, &dns):
		return &Failure{Exit: ExitUnknown, Message: fmt.Sprintf(
			"%s was not found (%s): is the box's name right?", c.Box.Host, termtext.From(dns.Err))}
	}
	// Past the dial, bytes may have come from the peer, and Go's error
	// text then quotes them (a status line, a header's value): only a
	// fixed reason of the client's own is said. The system's words are
	// kept for a dial alone, which no byte of the peer's reached.
	down := "If moteld is down, as root on the box: systemctl status moteld; journalctl -u moteld -n 50. The box's off-box heartbeat also reports a stopped moteld."
	var op *net.OpError
	switch {
	case errors.As(err, &op) && op.Op == "dial":
		why := "could not connect: " + termtext.From(op.Err.Error()).String()
		switch {
		case errors.Is(err, syscall.ECONNREFUSED):
			why = "connection refused"
		case op.Timeout():
			why = fmt.Sprintf("could not connect in %d s", int(timeout/time.Second))
		}
		return &Failure{Exit: ExitUnknown, Message: fmt.Sprintf("%s did not answer on %s (%s). %s", c.Box.Host, c.Box.PortOr443(), why, down)}
	case errors.As(err, &netErr) && netErr.Timeout():
		// Whichever timer ended it: the request's 30 s, or the
		// transport's own 10 s for a TLS handshake.
		return &Failure{Exit: ExitUnknown, Message: fmt.Sprintf("%s timed out on %s: no whole answer came, and motel waits at most %d s for one. %s", c.Box.Host, c.Box.PortOr443(), int(timeout/time.Second), down)}
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return &Failure{Exit: ExitUnknown, Message: fmt.Sprintf("%s closed the connection on %s before a whole answer came. %s", c.Box.Host, c.Box.PortOr443(), down)}
	}
	return &Failure{Exit: ExitUnknown, Message: fmt.Sprintf(
		"Something at %s answered, but not in HTTP as motel speaks it. Is this the box's deploy host?", c.Box.Addr())}
}

// refused is a 401 that stood after the retry. Its body says nothing
// of why: a box answers every refusal with the same sentence, and
// names nothing of motel in it, so the causes are named from what this
// side knows, all of them, since the client cannot tell them apart. A
// deploy token's do not involve this machine's clock. A person's token
// does: when the answer's Date is outside what the box takes for the
// token just sent (its iat and exp, with the box's allowance either
// way), the clock is named first, and the other causes stay, since a
// Date can be wrong or a gateway's. A box whose moteld is too old to
// take a person's token at all is one of them: it answers the same 401.
func (c *Client) refused(ans *Answer) error {
	box := c.Box.Host
	cred, ok := c.Cred.(*PersonToken)
	if !ok {
		scope := c.Scope
		if scope == "" {
			scope = "this app's"
		}
		return &Failure{Exit: ExitRefused, Status: http.StatusUnauthorized, Message: fmt.Sprintf(
			"%s refused the token in MOTEL_TOKEN (sent as Bearer). Most often %s deploy_trust does not accept the key it was minted with, or the audience, subject or claim it pins. Or the app is not one the box knows: an unknown app answers the same 401. Or the box could not consult the token's issuer. The box's journal (journalctl -u moteld, 'webhook auth failed') names the check that refused it.",
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
	return &Failure{Exit: ExitRefused, Status: http.StatusUnauthorized, Message: fmt.Sprintf(
		"%s did not accept your key %s (%s).%s no person or signer line on the box lists it; your address is outside the from list on every grant you hold; %s is not the box's main name; or the box's moteld is too old to take a key's token at all. Send the operator the line `motel key` prints.",
		box, cred.Key.Public.Fingerprint(), termtext.From(cred.Key.Shown), lead, box)}
}

// slowDown is a 429 that stood after the wait, or one that asked for a
// longer wait than the client makes, or one whose wait did not read.
func (c *Client) slowDown(secs int64, ok, waited bool) error {
	f := &Failure{Exit: ExitUnknown, Status: http.StatusTooManyRequests}
	if ok {
		// Capped as a number before it is a duration, so no value can
		// overflow into a short wait.
		f.RetryAfter = time.Duration(min(max(secs, 1), 3600)) * time.Second
	}
	switch {
	case !ok:
		f.Message = fmt.Sprintf("%s asked you to slow down; try again later.", c.Box.Host)
	case waited || c.NoWait && secs <= int64(maxRetryAfter/time.Second):
		f.Message = fmt.Sprintf("%s asked you to slow down; try again in %d s.", c.Box.Host, max(secs, 1))
	default:
		f.Message = fmt.Sprintf("%s asked you to slow down: the box asks you to wait %d s, longer than motel waits; try again then.", c.Box.Host, secs)
	}
	return f
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
// answer is not the one asked for: a refusal (403, 404, a 409 or
// another 4xx with an error sentence, each said as the box's with the
// sentence quoted), a 5xx, said as not known to be moteld's, a body the
// box's response filter withheld (a 200 whose one sentence says so,
// quoted), or an answer that is not moteld's; but a whoami answer about
// another key or box is taken, v holding its key and box alone
// (aboutAnother), for the caller to say so. No message carries a
// decoder's error, which quotes what the body holds: a field that does
// not read is named by the client's own name for it, and nothing of
// its value. A value held to a grammar that the box's filter replaced
// part of is Filtered in v (hold); the app and a record's version,
// which the caller compares with what was asked (Same), are as sent.
func (c *Client) Read(ans *Answer, v any) error {
	if ans.Code == http.StatusOK {
		if c.aboutAnother(ans, v) {
			return nil
		}
		missing := identity(v).missing(ans.Body)
		err := decodeObject(ans, v)
		var syntax *json.SyntaxError
		switch {
		case errors.Is(err, errNotJSON), errors.As(err, &syntax):
			return c.notMotel(ans, "not JSON")
		case errors.Is(err, errNotObject):
			return c.notMotel(ans, "not a JSON object")
		case len(missing) > 0:
			if said, ok := withheld(ans); ok {
				return &Failure{Exit: ExitFailed, Status: ans.Code, Body: ans.Body, Message: fmt.Sprintf(
					"%s withheld its answer; the box says: %s", c.Box.Host, said)}
			}
			return c.notMotel(ans, "it has no "+fieldList(missing))
		case err != nil:
			// moteld's answer, by its fields, with a value that did not
			// decode: the box's response filter replaces a value equal
			// to a secret, a timestamp's included, with its marker, and
			// names the key in the answer's redacted_env, which no log
			// line of the box's does: the sentence names them.
			what := "a value in it"
			if f := badField(ans.Body, v); f != "" {
				what = "its " + f
			}
			return &Failure{Exit: ExitUnknown, Status: ans.Code, Message: fmt.Sprintf(
				"%s answered, but %s could not be read: the box's response filter may have replaced it. %s",
				c.Box.Addr(), what, replacedKeys(ans.Body))}
		}
		if why := hold(v); why != "" {
			return c.notMotel(ans, why)
		}
		return nil
	}
	if ans.Code == http.StatusForbidden {
		return c.forbidden(ans)
	}
	// A motel webhook never redirects, and none is followed (NewHTTP),
	// so a 3xx is another thing's answer whatever its body holds: a JSON
	// error in one is not the box's sentence.
	if ans.Code >= 300 && ans.Code < 400 {
		return c.notMotel(ans, "")
	}
	var e wire.Error
	if decodeObject(ans, &e) != nil || e.Error == "" {
		return c.notMotel(ans, "")
	}
	said := termtext.Quote(e.Error)
	if ans.Code == http.StatusNotFound {
		return &Failure{Exit: ExitNotFound, Status: ans.Code, Message: fmt.Sprintf("%s answered 404; the box says: %s", c.Box.Host, said), Body: ans.Body}
	}
	// A gateway in front of a box can answer a 5xx in JSON too, and no
	// header tells moteld's own from it, so no 5xx is said as the box's.
	if ans.Code >= 500 {
		return &Failure{Exit: ExitUnknown, Status: ans.Code, Message: fmt.Sprintf(
			"Something at %s answered %d, not known to be moteld's own answer; it says: %s", c.Box.Addr(), ans.Code, said)}
	}
	return &Failure{Exit: ExitFailed, Status: ans.Code, Message: fmt.Sprintf("%s answered %d; the box says: %s", c.Box.Host, ans.Code, said), Body: ans.Body}
}

// The grammars the server holds the values to that the client prints
// unlabelled or as markers. A value outside them is no honest box's,
// but one its response filter replaced part of (hold).
var (
	// wireVersionRe is liveswap/names.go's versionRe: every version a
	// status or a record names was one a deploy took.
	wireVersionRe = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]{0,63}$`)
	// unitRe is liveswap/runner_systemd.go's unitNameRe: motel-, the
	// app, the version, the instance's nonce (liveswap/socket.go).
	unitRe = regexp.MustCompile(`^motel-[a-z0-9-]{1,63}\.[A-Za-z0-9_-][A-Za-z0-9._-]{0,63}\.[0-9a-f]{16}(\.prestart)?\.service$`)
	// commitRe is box/proof's IsID: a commit as the box records one,
	// 40 lowercase hex.
	commitRe = regexp.MustCompile(`^[0-9a-f]{40}$`)
	// digestRe is box/check.go's digest: a SHA-256 in lowercase hex.
	digestRe = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// outcomeStatuses and outcomePhases are liveswap/app.go's outcome
	// vocabulary: a deploy's status, and the phases it passes through
	// (the app's phase is one of those, or "idle", liveswap/app.go's
	// newManagedApp and its end of a deploy).
	outcomeStatuses = map[string]bool{"succeeded": true, "failed": true}
	// watchdogStates are liveswap/watchdog.go's wdState values: the
	// state is printed as a word, with the client's own words after it.
	watchdogStates = map[string]bool{"disabled": true, "idle": true, "grace": true, "watching": true, "backoff": true, "throttled": true}
	outcomePhases  = map[string]bool{"downloading": true, "extracting": true, "preparing": true, "starting": true, "soaking": true, "promoting": true, "draining": true, "stopping_old": true}
	// backupLevels are backups/status's levels of a line (Info, Warning,
	// Problem): what the client marks a line with, so held to the three.
	backupLevels = map[string]bool{"info": true, "warning": true, "problem": true}
	// backupResultRe is the shape of backups/record's classes (ok,
	// incomplete, pending, data missing, failed, not attempted, not
	// run): lowercase words, printed as the box's word in a column of
	// their own, so a class a later box adds is shown, not refused.
	backupResultRe = regexp.MustCompile(`^[a-z]{1,16}( [a-z]{1,16}){0,3}$`)
	// alertNameRe is an alertname as motel's alerts carry one (its own
	// events, notify/DESIGN-notify.md "Events"; an operator's rule,
	// notify's ruleNameRe; an external producer's, notify/admin.go's
	// alertRe; the store's chain events), widened to the names a
	// Prometheus rule takes, with a dot and a hyphen: printed as a word,
	// it holds no space, bracket or quote.
	alertNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_:.-]{0,63}$`)
	// alertWordRe is the shape of the words an alerts answer says
	// (monitor, an alert's severity and state): printed as a word, and
	// one the client does not know is said as such, not refused.
	alertWordRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	// seriesWordRe is the shape of a series panel's name and unit
	// (liveswap/wire/series.go's Panel names and units, 5xx_share
	// among them): printed as a word, and one the client does not know
	// is not drawn, not refused.
	seriesWordRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_]{0,31}$`)
	// seriesSpans are wire.SeriesRanges' spans, which bound a panel's
	// points by the body's step.
	seriesSpans = map[string]time.Duration{"1h": time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "14d": 14 * 24 * time.Hour}
)

// holdSeries holds a series answer to what the client draws: the
// store's word and the window to their grammars; with the store's
// answer, a step and an end; each panel's name and unit to a word's
// shape, each name once; and each panel's points, oldest first, no
// more than the window holds at the step, every time inside the
// window. A value is drawn by the client from its own number, so a
// point of any finite value is taken.
func holdSeries(s *wire.Series) string {
	if !alertWordRe.MatchString(s.Store) {
		return "its store is not a word"
	}
	span, ok := seriesSpans[s.Window]
	if !ok || !slices.Contains(wire.SeriesRanges, s.Window) {
		return "its window is not one a box keeps"
	}
	if s.Store != wire.StoreOn {
		if len(s.Panels) > 0 {
			return "it holds points from a store that did not answer"
		}
		return ""
	}
	// No window a box keeps has a thousand steps (337 at the most). The
	// seconds compared before they are a Duration, which a huge one
	// would wrap.
	if s.StepSeconds <= 0 || s.StepSeconds > int(span/time.Second) || int(span/time.Second)%s.StepSeconds != 0 || s.End == nil {
		return "its step or its end is not one a window has"
	}
	step := time.Duration(s.StepSeconds) * time.Second
	if span/step > 1000 {
		return "its step or its end is not one a window has"
	}
	start, end := s.End.Add(-span).Unix(), s.End.Unix()
	most := int(span/step) + 1
	seen := map[string]bool{}
	for i := range s.Panels {
		p := &s.Panels[i]
		if !held(&p.Name, seriesWordRe.MatchString) || !held(&p.Unit, seriesWordRe.MatchString) {
			return "a panel's name or unit in it is not a word"
		}
		if p.Name != Filtered && seen[p.Name] {
			return "it holds a panel twice"
		}
		seen[p.Name] = true
		if len(p.Points) > most {
			return "a panel in it holds more points than its window has steps"
		}
		for j, pt := range p.Points {
			t := pt[0]
			// Each at a multiple of the step back from the end, as moteld's.
			if math.IsNaN(t) || math.IsInf(pt[1], 0) || math.IsNaN(pt[1]) || t < float64(start) || t > float64(end) || t != math.Trunc(t) ||
				(end-int64(t))%int64(s.StepSeconds) != 0 || j > 0 && t <= p.Points[j-1][0] {
				return "a point in it is not one of its window, oldest first"
			}
		}
	}
	return ""
}

// The box's response filter writes a marker where it replaced text
// (liveswap/redact.go). For an env_file value of the app's, or a
// credential's shape, it is [redacted:<what>], <what> the key's name
// (several, with commas, for keys that hold one value) or the shape's;
// when that spelling would itself hold a known value (a key named for
// its own value), the next of [REDACTED:<what>], [withheld:<what>],
// <<what> removed> and [#N] that holds none (chooseMarker). For a
// token that looks generated it is [masked, N chars]. No grammar below
// takes a bracket or an angle, so a value outside its grammar that
// carries a marker is one the box's own filter changed, not another
// server's: an env_file value equal to the app's name (DB_NAME=<app>)
// is replaced inside the unit, which is named after the app.
//
// The filter's later layers run over the markers of its first, so
// what stands inside a marker can be rewritten there, to any depth: a
// key's name that looks generated is masked, [redacted:[masked, 24
// chars]]; one shaped like a provider's credential (AKIA…, ghp_…) is
// replaced, [redacted:[redacted:aws-key]]; and the last pass replaces
// a known value in what those wrote, [REDACTED:[[REDACTED:K]:aws-key]].
// Each is one marker, not a marker in a bracket: a marker runs from
// its opening bracket to the bracket that closes it, and one in angles
// from its < to its > (filterMarks).
var (
	// markOpens are how a marker with a label opens.
	markOpens = []string{"[redacted:", "[REDACTED:", "[withheld:"}
	// bareMarkRe is a marker with no label, whole.
	bareMarkRe = regexp.MustCompile(`^(?:\[#[0-9]{1,9}\]|\[masked, [0-9]{1,9} chars\])$`)
)

// filterMarks finds the filter's markers in s: where each starts and
// ends, in order, none inside another. One pass over s, and work in
// proportion to its length whatever it holds: a box's value is not
// trusted to be short or well formed.
func filterMarks(s string) [][]int {
	type opening struct {
		at    int
		angle bool
	}
	var (
		found [][]int
		open  []opening // the [ and < not yet closed
		left  [2]int    // how many of each kind are open: [ then <
	)
	kind := func(angle bool) int {
		if angle {
			return 1
		}
		return 0
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '[', '<':
			open = append(open, opening{i, c == '<'})
			left[kind(c == '<')]++
		case ']', '>':
			angle := c == '>'
			if left[kind(angle)] == 0 {
				continue
			}
			// Its pair is the nearest opening of its kind; one of the
			// other kind left open inside is no pair's, and goes.
			k := len(open) - 1
			for open[k].angle != angle {
				left[kind(open[k].angle)]--
				k--
			}
			left[kind(angle)]--
			start := open[k].at
			open = open[:k]
			m := s[start : i+1]
			if (angle && !angleMark(m)) || (!angle && !bracketMark(m)) {
				continue
			}
			// What stands inside a marker is part of it.
			for len(found) > 0 && found[len(found)-1][0] > start {
				found = found[:len(found)-1]
			}
			found = append(found, []int{start, i + 1})
		}
	}
	return found
}

// angleMark says whether m, from a < to the > that closes it, is the
// marker's spelling in angles: a label, then " removed".
func angleMark(m string) bool {
	return len(m) > len("< removed>") && strings.HasSuffix(m, " removed>")
}

// bracketMark says whether m, from a bracket to the one that closes
// it, is a marker: a known opening and a label, or a bare one.
func bracketMark(m string) bool {
	for _, o := range markOpens {
		if strings.HasPrefix(m, o) {
			return len(m) > len(o)+1
		}
	}
	return len(m) <= 32 && bareMarkRe.MatchString(m)
}

// Filtered is what the client shows in place of a value the box's
// response filter replaced part of: its own words, and nothing of the
// value, which is outside its grammar.
const Filtered = "[hidden by the box's filter]"

// Same says whether got, a value of the box's, is the one asked for:
// equal to it, or equal to it everywhere but where the box's filter
// left a marker ("blog-[redacted:APP_ENV]" for blog-production). The
// caller prints what was asked, never got.
func Same(got, asked string) bool {
	if got == asked {
		return true
	}
	marks := filterMarks(got)
	if len(marks) == 0 {
		return false
	}
	// asked is got with each marker standing for some text of its own:
	// the parts between the markers, in order, each where the one
	// before left off, the first at the start and the last at the end.
	var re strings.Builder
	re.WriteString("^")
	at := 0
	for _, m := range marks {
		re.WriteString(regexp.QuoteMeta(got[at:m[0]]))
		re.WriteString(".+")
		at = m[1]
	}
	re.WriteString(regexp.QuoteMeta(got[at:]))
	re.WriteString("$")
	ok, err := regexp.MatchString(re.String(), asked)
	return err == nil && ok
}

// held keeps *s when it is in its grammar, puts Filtered in its place
// when the box's filter changed it, and reports false for any other
// value: the one way a value outside its grammar is taken, and then
// not one byte of it is shown.
func held(s *string, in func(string) bool) bool {
	switch {
	case in(*s):
	case len(filterMarks(*s)) > 0:
		*s = Filtered
	default:
		return false
	}
	return true
}

// aboutAnother says whether ans is a whoami answer naming another key
// than the one this client signs with, or another box than the one it
// asks, whatever else it holds: the whoami command's rule
// (model.AboutAnother, the same comparison, which the command and the
// TUI make again on the value returned, and say). The key and the box
// are read on their own, each compared when it is there and a string,
// and are all v is given; nothing else of the answer is read or held to
// its grammar (hold): neither shows anything else of such an answer,
// and a value missing or outside its grammar, the other of the two
// among them, would otherwise hide that it is about another. One of the
// two not there, or not a string, is given as this side's own, so that
// only the one named is said to differ; an answer with neither is not
// about another.
func (c *Client) aboutAnother(ans *Answer, v any) bool {
	me, ok := v.(*wire.WhoAmI)
	pt, person := c.Cred.(*PersonToken)
	var named struct {
		Key json.RawMessage `json:"key"`
		Box json.RawMessage `json:"box"`
	}
	if !ok || !person || decodeObject(ans, &named) != nil {
		return false
	}
	key, keyNamed := jsonString(named.Key)
	box, boxNamed := jsonString(named.Box)
	fp := pt.Key.Public.Fingerprint()
	if !keyNamed {
		key = fp
	}
	if !boxNamed {
		box = c.Box.Host
	}
	if key == fp && box == c.Box.Host {
		return false
	}
	*me = wire.WhoAmI{Key: key, Box: box}
	return true
}

// jsonString is raw as a JSON string, and false when it is absent, null
// or not a string.
func jsonString(raw json.RawMessage) (string, bool) {
	var s *string
	if json.Unmarshal(raw, &s) != nil || s == nil {
		return "", false
	}
	return *s, true
}

// hold says why a body that decoded is still not one an honest box
// sends: a value outside the grammar the box holds it to, which the
// client never prints and so never says either. A value the box's own
// filter replaced part of is no such value: it becomes Filtered in v
// (but a record's version, which the caller compares and replaces).
func hold(v any) string {
	isVersion := wireVersionRe.MatchString
	switch a := v.(type) {
	case *wire.Status:
		if a.Phase != "idle" && !outcomePhases[a.Phase] {
			return "its phase is not one an app has"
		}
		if a.CurrentVersion != "" && !held(&a.CurrentVersion, isVersion) {
			return "a version in it is not a version"
		}
		for i := range a.AvailableVersions {
			if !held(&a.AvailableVersions[i], isVersion) {
				return "a version in it is not a version"
			}
		}
		if a.Unit != "" && !held(&a.Unit, unitRe.MatchString) {
			return "its unit is not one moteld names"
		}
		if wd := a.Watchdog; wd != nil && wd.State != "" && !held(&wd.State, func(s string) bool { return watchdogStates[s] }) {
			return "its watchdog's state is not one a watchdog has"
		}
		// The window is one of the ranges a box answers for
		// (wire.MetricsRanges), or absent when the store did not answer:
		// it is printed inside the client's own brackets.
		if m := a.Metrics; m != nil && m.Window != "" && !slices.Contains(wire.MetricsRanges, m.Window) {
			return "its metrics' window is not one a box keeps"
		}
		for i := range a.Deploys {
			d := &a.Deploys[i]
			if why := holdOutcome(&d.Version, d.Status, d.Phase, nil); why != "" {
				return why
			}
		}
		// The deploy in flight is printed beside the running version: its
		// version to the grammar of a record's, and its phases, which the
		// app's screen and status --follow print, to the outcome
		// vocabulary, as last_deploy's are (holdOutcome).
		if d := a.Deploying; d != nil {
			if !held(&d.Version, isVersion) {
				return "a version in it is not a version"
			}
			for _, p := range d.Phases {
				if !outcomePhases[p.Name] {
					return "a deploy's phase in it is not one a deploy has"
				}
			}
		}
		if d := a.LastDeploy; d != nil {
			return holdOutcome(&d.Version, d.Status, d.Phase, d.Phases)
		}
	case *wire.DeployResult:
		// The record's version is compared with the one asked for
		// (Same), which is what the caller prints: left as sent.
		version := a.Version
		return holdOutcome(&version, a.Status, a.Phase, a.Phases)
	case *wire.BoxStatus:
		if !commitRe.MatchString(a.Commit) {
			return "its commit is not a commit id"
		}
		if !digestRe.MatchString(a.SHA256) {
			return "its sha256 is not a digest"
		}
	case *wire.Backups:
		// What the client prints of the box's backups as a token: each
		// app's name, its result, and each line's level and app. The
		// lines' text and could_not_tell are free text, labelled and
		// capped; the units running are in the lines, and not read.
		for i := range a.Apps {
			if !held(&a.Apps[i].Name, appRe.MatchString) {
				return "a name in it is no app name"
			}
			if r := a.Apps[i].Result; r != "" && !backupResultRe.MatchString(r) {
				return "an app's result in it is not one a backup run says"
			}
		}
		for i := range a.Lines {
			l := &a.Lines[i]
			if !backupLevels[l.Level] {
				return "a line's level in it is not info, warning or problem"
			}
			if l.App != "" && !held(&l.App, appRe.MatchString) {
				return "a name in it is no app name"
			}
		}
	case *wire.Alerts:
		// What the client prints of an app's alerts as a token: the
		// monitor's word, and each alert's name, severity and state. The
		// summary is free text, labelled and capped.
		if !alertWordRe.MatchString(a.Monitor) {
			return "its monitor is not a word"
		}
		for i := range a.Alerts {
			al := &a.Alerts[i]
			if !held(&al.Name, alertNameRe.MatchString) {
				return "an alert's name in it is not one an alert has"
			}
			if al.Severity != "" && !held(&al.Severity, alertWordRe.MatchString) || !held(&al.State, alertWordRe.MatchString) {
				return "an alert's severity or state in it is not a word"
			}
		}
	case *wire.BoxAlerts:
		// What the client prints of the box's alerts as a word: the
		// monitor's, which a client reads as not knowing when it does not
		// know it, quoted. Everything else is the box's text, each on a
		// row after the client's label and capped, or a word the client
		// says as not known when it does not know it (an alert's state
		// and severity, a silence's state, a matcher's operator).
		if !alertWordRe.MatchString(a.Monitor) {
			return "its monitor is not a word"
		}
	case *wire.Rules:
		// What the client prints of the rules as a word: the store's,
		// which a client reads as not knowing when it does not know it,
		// quoted, and a reading's app, held to the app grammar (an app's
		// read is compared with the app asked for). Everything else is the
		// box's text, each on a row after the client's label and capped,
		// or a word the client says as not known when it does not know it
		// (a kind, a state, an op, a unit), or a number the client writes.
		if !alertWordRe.MatchString(a.Store) {
			return "its store is not a word"
		}
		for i := range a.Alerts {
			al := &a.Alerts[i]
			if al.App != "" && !held(&al.App, appRe.MatchString) {
				return "a name in it is no app name"
			}
			for j := range al.Apps {
				if !held(&al.Apps[j], appRe.MatchString) {
					return "a name in it is no app name"
				}
			}
			for j := range al.Readings {
				if r := &al.Readings[j]; r.App != "" && !held(&r.App, appRe.MatchString) {
					return "a name in it is no app name"
				}
			}
		}
	case *wire.Series:
		return holdSeries(a)
	case *wire.Apps:
		for _, e := range a.Apps {
			if !appRe.MatchString(e.Name) {
				return "a name in its list is no app name"
			}
		}
	case *wire.WhoAmI:
		// Every value whoami prints as the box's is held to the grammar
		// the box holds it to: the name to a person line's, the release
		// to the Makefile's; the key and the box are compared, not
		// printed. A role is held to its shape, which is wider than the
		// roles a box grants, and printed as a word only when it is one
		// of them, viewer or logs (roleRe).
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

// holdOutcome holds a deploy's version, status, phase and phases to
// the outcome vocabulary. The words are never the filter's to change:
// the box keeps an outcome word standing or withholds the body
// (liveswap/redact.go, rule 3).
func holdOutcome(version *string, status, phase string, phases []wire.PhaseTiming) string {
	switch {
	case !held(version, wireVersionRe.MatchString):
		return "a version in it is not a version"
	case !outcomeStatuses[status]:
		return "a deploy's status in it is not succeeded or failed"
	case phase != "" && !outcomePhases[phase]:
		return "a deploy's phase in it is not one a deploy has"
	}
	for _, p := range phases {
		if !outcomePhases[p.Name] {
			return "a deploy's phase in it is not one a deploy has"
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

// replacedKeys says which env_file keys the answer's redacted_env
// names: the keys whose values the box's response filter replaced
// anywhere in it (liveswap's redactJSON). A key is named when it holds
// to a key's grammar (liveswap's envKeyRe) within 128 characters, ten
// at most; the others are counted. A key the filter replaced in the
// list (one equal to another key's value) arrives as its marker, and is
// counted. A body without one names none: the value was not one the
// filter replaced (whoami's filter knows no value).
func replacedKeys(body []byte) string {
	// The key as written: a struct's field would take any case of it.
	var top map[string]json.RawMessage
	var all []string
	if json.Unmarshal(body, &top) != nil || json.Unmarshal(top["redacted_env"], &all) != nil {
		all = nil // not a list of strings: none named
	}
	all = slices.DeleteFunc(all, func(k string) bool { return k == "" }) // a null in it
	if len(all) == 0 {
		return "The answer names no key whose value the filter replaced."
	}
	keys := slices.DeleteFunc(slices.Clone(all), func(k string) bool { return len(k) > 128 || !envKeyRe.MatchString(k) })
	named := keys[:min(len(keys), 10)]
	switch {
	case len(named) == 0:
		return "The answer says the filter replaced values in it, and names their keys in no form shown here."
	case len(named) < len(all):
		return "The answer says the filter replaced, somewhere in it, the values of " + strings.Join(named, ", ") + " and " + strconv.Itoa(len(all)-len(named)) + " more."
	}
	return "The answer says the filter replaced, somewhere in it, the values of " + strings.Join(named, ", ") + "."
}

// envKeyRe is liveswap/app.go's: an env_file key as systemd takes one.
var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

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
// --json prints whenever it is JSON. The box's words are
// never the client's advice: needs and scope are used only when they
// are the words access.Decide writes (liveswap/access/access.go,
// Forbidden: "viewer", "logs", "deployer" or "key"; "app" or "box"), and the
// sentence is built from them alone, so an app that exists and one
// that does not read the same and no app is named. Any other needs,
// a word a later box may name among them, is never put in the
// client's own sentence, where a box could have it say "it needs the
// <words of the box's choosing> role": such a 403 is said as a refusal
// that did not say what it needs, with the box's own sentence, if any,
// quoted as the box's.
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
		whoami = " `motel whoami` lists the roles you hold from here."
	}
	on := map[string]string{"app": "this app", "box": "the box"}[f.Scope]
	needs := map[string]string{
		"viewer":   "the viewer role on " + on,
		"logs":     "the logs role on " + on,
		"deployer": "a deploy token of " + on,
		"key":      "a token your own key signs",
	}[f.Needs]
	if needs == "" || on == "" {
		said := "."
		if f.Error != "" {
			said = "; the box says: " + termtext.Quote(f.Error).String() + "."
		}
		return &Failure{Exit: ExitForbidden, Status: ans.Code, Body: body, Message: fmt.Sprintf(
			"%s refused this (HTTP 403) and did not say what it needs%s%s", c.Box.Host, said, whoami)}
	}
	if person {
		return &Failure{Exit: ExitForbidden, Status: ans.Code, Body: body, Message: fmt.Sprintf(
			"%s does not allow your key this: it needs %s.%s", c.Box.Host, needs, whoami)}
	}
	hint := ""
	if f.Needs == "key" {
		hint = " Unset MOTEL_TOKEN, or name your key with --key."
	}
	return &Failure{Exit: ExitForbidden, Status: ans.Code, Body: body, Message: fmt.Sprintf(
		"%s does not allow the token in MOTEL_TOKEN this: it needs %s.%s", c.Box.Host, needs, hint)}
}

// NotMotel is an answer that is not moteld's, for a command that reads
// what Read took and finds it about another thing than it asked (an
// app, a version, a host): why is the command's own words.
func (c *Client) NotMotel(ans *Answer, why string) error { return c.notMotel(ans, why) }

// withheld is the box's sentence, quoted, when a 200's body is the one
// its response filter writes in place of an answer it withheld: an
// error alone, in the filter's fixed opening words (liveswap/redact.go:
// an env_file it could not read, a replaced value that overlapped the
// body's structure or one of its outcomes), which say what the
// operator has to mend; or the bare marker [#0] it falls back to when
// what it would send is not JSON after its last pass, which says no
// more than that the answer was withheld. The opening is read as the
// filter's own last pass may have left it (opensWithheld), so a
// sentence that opens with a marker is taken too, whatever follows.
// Any other 200 without the answer's fields is still not moteld's.
func withheld(ans *Answer) (termtext.Text, bool) {
	var e wire.Error
	if decodeObject(ans, &e) != nil || !opensWithheld(e.Error) {
		return termtext.Text{}, false
	}
	return termtext.Quote(e.Error), true
}

// opensWithheld says whether s, the one sentence of such a body, opens
// with the filter's "response withheld" as the filter's own last pass
// may have left it (redact.go's filterBody): an env_file value that is
// a piece of the sentence stands there as a marker, and the piece can
// be any, the colon after the opening included ("withheld: a"). So
// what stands before the first marker must be the opening's start,
// and the marker stands for the rest of it; with no marker that far,
// the opening stands whole, and then the sentence ends, or goes on
// with a colon or a marker. [#0], a marker alone, is the filter's too.
func opensWithheld(s string) bool {
	const opening = "response withheld"
	before, marked := s, false
	if m := filterMarks(s); len(m) > 0 {
		before, marked = s[:m[0][0]], true
	}
	if rest, whole := strings.CutPrefix(before, opening); whole {
		return rest == "" || strings.HasPrefix(rest, ":")
	}
	return marked && strings.HasPrefix(opening, before)
}

func (c *Client) notMotel(ans *Answer, why string) error {
	if why != "" {
		why = ": " + termtext.From(why).String()
	}
	return &Failure{Exit: ExitUnknown, Status: ans.Code, Message: fmt.Sprintf(
		"Something at %s answered (HTTP %d, not moteld's answer%s). Is this the box's deploy host?", c.Box.Addr(), ans.Code, why)}
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
	case *wire.Backups:
		return shape{lists: []string{"apps", "lines"}, times: []string{"at"}}
	case *wire.Alerts:
		return shape{strs: []string{"monitor"}, lists: []string{"alerts"}}
	case *wire.BoxAlerts:
		return shape{strs: []string{"monitor"}, lists: []string{"alerts", "silences"}}
	case *wire.Rules:
		return shape{strs: []string{"store"}, lists: []string{"alerts"}}
	case *wire.Series:
		return shape{strs: []string{"store", "window"}, lists: []string{"panels"}}
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

// errNotJSON and errNotObject are a body that is not moteld's shape at
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
