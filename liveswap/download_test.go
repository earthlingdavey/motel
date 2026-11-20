package liveswap

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// entryFor derives the literal allowlist entry covering rawURL —
// host plus the exact port. There is no port wildcard, so tests
// declare the httptest server's real port the same way an operator
// declares a known port.
func entryFor(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "127.0.0.1"
	}
	entry := u.Hostname()
	if p := u.Port(); p != "" {
		entry += ":" + p
	}
	return entry
}

func testDownloadOpts(t *testing.T, rawURL string) downloadOpts {
	t.Helper()
	return downloadOpts{
		url:           rawURL,
		destDir:       t.TempDir(),
		maxBytes:      1024,
		allowInsecure: true, // httptest servers are plain http
		allowlist:     mustAllowlist(t, entryFor(rawURL)),
		client:        &http.Client{},
	}
}

func mustAllowlist(t *testing.T, entries ...string) []artifactAllowEntry {
	t.Helper()
	parsed, err := parseAllowlist(entries)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestDownloadArtifactHappyPath(t *testing.T) {
	var gotAuth, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		_, _ = w.Write([]byte("artifact-bytes"))
	}))
	defer srv.Close()

	opts := testDownloadOpts(t, srv.URL+"/demo.tar.gz")
	opts.authHeader = "Bearer tok123"
	path, err := downloadArtifact(context.Background(), opts)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "artifact-bytes" {
		t.Fatalf("content = %q", data)
	}
	if gotAuth != "Bearer tok123" {
		t.Fatalf("auth header not forwarded: %q", gotAuth)
	}
	if gotAccept != "application/octet-stream" {
		t.Fatalf("accept header = %q", gotAccept)
	}
}

func TestDownloadArtifactRejectsPlainHTTPByDefault(t *testing.T) {
	opts := testDownloadOpts(t, "http://example.com/a.tgz")
	opts.allowInsecure = false
	_, err := downloadArtifact(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "allow_insecure_http") {
		t.Fatalf("want https-only error, got %v", err)
	}
}

func TestDownloadArtifactRejectsWeirdScheme(t *testing.T) {
	_, err := downloadArtifact(context.Background(), testDownloadOpts(t, "ftp://example.com/a.tgz"))
	if err == nil || !strings.Contains(err.Error(), "unsupported scheme") {
		t.Fatalf("want scheme error, got %v", err)
	}
}

// Every payload-caused refusal is a validationError, so the webhook
// answers 422 with the reason — never a generic 500 the CI author
// cannot act on.
func TestDownloadPayloadRefusalsAreValidationErrors(t *testing.T) {
	for name, rawURL := range map[string]string{
		"unparseable url": "https://exa mple.com/x",
		"weird scheme":    "ftp://example.com/a.tgz",
		"plain http":      "http://example.com/a.tgz",
		"not allowlisted": "https://evil.example/a.tgz",
		"undeclared port": "https://127.0.0.1:1234/a.tgz",
	} {
		opts := testDownloadOpts(t, rawURL)
		opts.allowInsecure = false
		opts.allowlist = mustAllowlist(t, "127.0.0.1")
		_, err := downloadArtifact(context.Background(), opts)
		var vErr validationError
		if err == nil || !errors.As(err, &vErr) {
			t.Errorf("%s: want validationError, got %T: %v", name, err, err)
		}
	}
}

// Each refusal quotes the URL, the deployer's text, in its 422; its
// said, what the operator's channels are told, holds none of it.
func TestPinArtifactURLSaidQuotesNoURL(t *testing.T) {
	const words = "URGENT-call-evil.example"
	for name, rawURL := range map[string]string{
		"weird scheme":    "ftp://example.com/" + words,
		"plain http":      "http://127.0.0.1/" + words,
		"not allowlisted": "https://evil.example/" + words,
		"undeclared port": "https://127.0.0.1:1234/" + words,
		"query":           "https://127.0.0.1/a.tgz?" + words + "=1",
	} {
		_, err := pinArtifactURL(rawURL, false, mustAllowlist(t, "127.0.0.1"))
		var vErr validationError
		if !errors.As(err, &vErr) || !strings.Contains(vErr.msg, words) {
			t.Errorf("%s: want a 422 quoting the url, got %v", name, err)
			continue
		}
		if vErr.said == "" || strings.Contains(vErr.said, words) {
			t.Errorf("%s: said = %q, want the refusal without the url", name, vErr.said)
		}
	}
	// An unparseable URL is refused with url.Parse's reason alone, in
	// the 422 too: its error quotes the whole URL, a presigned query's
	// secret with it.
	_, err := pinArtifactURL("https://exa mple.com/a.tgz?X-Amz-Signature="+words, false, mustAllowlist(t, "127.0.0.1"))
	var vErr validationError
	if !errors.As(err, &vErr) || vErr.msg != `invalid artifact url: invalid character " " in host name` || vErr.said != "invalid artifact url" {
		t.Fatalf("unparseable url: msg %q, said %q (%v)", vErr.msg, vErr.said, err)
	}
}

// A pinned sha256 binds the download to the bytes the deployer hashed:
// the same bytes come through, other bytes are a validationError (a
// 422 upstream) naming both digests, and nothing is left on disk.
func TestDownloadArtifactPinnedDigest(t *testing.T) {
	body := []byte("artifact-bytes")
	sum := sha256.Sum256(body)
	pin := hex.EncodeToString(sum[:])
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	opts := testDownloadOpts(t, srv.URL+"/demo.tar.gz")
	opts.sha256 = pin
	path, err := downloadArtifact(context.Background(), opts)
	if err != nil {
		t.Fatalf("pinned download of the pinned bytes: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != string(body) {
		t.Fatalf("content = %q", data)
	}

	opts = testDownloadOpts(t, srv.URL+"/demo.tar.gz")
	opts.sha256 = strings.Repeat("0", 64)
	_, err = downloadArtifact(context.Background(), opts)
	var vErr validationError
	if !errors.As(err, &vErr) {
		t.Fatalf("mismatch: want validationError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "request pinned "+opts.sha256) || !strings.Contains(err.Error(), "downloaded "+pin) {
		t.Fatalf("mismatch names neither digest: %v", err)
	}
	if entries, _ := os.ReadDir(opts.destDir); len(entries) != 0 {
		t.Fatalf("mismatched download left behind: %v", entries)
	}
}

// The pin crosses from the request into the download through the real
// fetcher: a mismatch is refused (what the refusal says and leaves is
// TestDownloadArtifactPinnedDigest's), and the same request with the
// right pin extracts the release.
func TestReleaseFetcherHoldsTheDownloadToThePin(t *testing.T) {
	archive := buildTarGz(t, []tarEntry{{name: "server", body: "#!/bin/sh\n", mode: 0o755}})
	bytes, err := os.ReadFile(archive)
	must(t, err)
	sum := sha256.Sum256(bytes)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes)
	}))
	defer srv.Close()
	spec := testSpec(t)
	spec.allowlist = mustAllowlist(t, entryFor(srv.URL))
	spec.allowInsecure = true
	rf := &releaseFetcher{client: &http.Client{}}
	req := deployRequest{url: srv.URL + "/a.tgz", version: "v1", sha256: strings.Repeat("0", 64)}

	_, _, err = rf.fetch(context.Background(), spec, req, func(string) {})
	var vErr validationError
	if !errors.As(err, &vErr) {
		t.Fatalf("mismatched pin: want the validationError, got %T: %v", err, err)
	}

	req.sha256 = hex.EncodeToString(sum[:])
	dir, _, err := rf.fetch(context.Background(), spec, req, func(string) {})
	if err != nil {
		t.Fatalf("pinned pull of the pinned bytes: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "server")); err != nil {
		t.Fatalf("release not extracted: %v", err)
	}
}

// An over-cap body is cut short, so its digest says nothing: the
// streaming cap is the error, not a mismatch the deployer would chase.
// Chunked and flushed, so no Content-Length refuses it before a byte
// is hashed (TestDownloadArtifactContentLengthCap covers that path).
func TestDownloadArtifactCapBeatsThePin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 64; i++ {
			_, _ = w.Write(make([]byte, 64))
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()
	opts := testDownloadOpts(t, srv.URL)
	opts.maxBytes = 512
	opts.sha256 = strings.Repeat("0", 64)
	_, err := downloadArtifact(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "exceeded max size") {
		t.Fatalf("want the streaming cap error, got %v", err)
	}
	if strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("the pin was judged on a cut-short body: %v", err)
	}
}

// Go's client drops Authorization when a redirect leaves the first
// host's domain (a subdomain of it keeps the header:
// shouldCopyHeaderOnRedirect) — the GitHub -> S3 pattern depends on the
// credential NOT following the redirect. That stdlib behavior is
// load-bearing for credential scope, so pin it: 127.0.0.1 and localhost
// are the same server here but neither is the other or a subdomain of
// it, which is exactly what a cross-host hop is.
func TestDownloadDropsAuthHeaderOnCrossHostRedirect(t *testing.T) {
	authByPath := map[string]string{}
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authByPath[r.URL.Path] = r.Header.Get("Authorization")
		if r.URL.Path == "/hop1.tgz" {
			u, _ := url.Parse(srvURL)
			http.Redirect(w, r, "http://localhost:"+u.Port()+"/hop2.tgz", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()
	srvURL = srv.URL

	opts := testDownloadOpts(t, srv.URL+"/hop1.tgz")
	opts.authHeader = "Bearer tok123"
	opts.client = newDownloadClient(true)
	if _, err := downloadArtifact(context.Background(), opts); err != nil {
		t.Fatalf("download: %v", err)
	}
	if authByPath["/hop1.tgz"] != "Bearer tok123" {
		t.Fatalf("first hop should carry the credential, got %q", authByPath["/hop1.tgz"])
	}
	if got, ok := authByPath["/hop2.tgz"]; !ok {
		t.Fatal("redirect target never reached")
	} else if got != "" {
		t.Fatalf("credential followed a cross-host redirect: %q", got)
	}
}

func TestDownloadArtifactHostAllowlist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()

	opts := testDownloadOpts(t, srv.URL+"/a.tgz")
	opts.allowlist = mustAllowlist(t, "github.com/smallhoursorg/")
	_, err := downloadArtifact(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "artifact_allowlist") {
		t.Fatalf("want allowlist error, got %v", err)
	}

	opts.allowlist = mustAllowlist(t, "github.com/smallhoursorg/", entryFor(srv.URL))
	if _, err := downloadArtifact(context.Background(), opts); err != nil {
		t.Fatalf("allowlisted host rejected: %v", err)
	}
}

func TestDownloadArtifactContentLengthCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "99999")
		_, _ = w.Write(make([]byte, 99999))
	}))
	defer srv.Close()
	_, err := downloadArtifact(context.Background(), testDownloadOpts(t, srv.URL))
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("want content-length cap error, got %v", err)
	}
}

func TestDownloadArtifactStreamingCap(t *testing.T) {
	// No Content-Length (chunked) but body exceeds the cap anyway.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 64; i++ {
			_, _ = w.Write(make([]byte, 64))
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()
	opts := testDownloadOpts(t, srv.URL)
	opts.maxBytes = 512
	_, err := downloadArtifact(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "exceeded max size") {
		t.Fatalf("want streaming cap error, got %v", err)
	}
	// The partial download must have been cleaned up.
	entries, _ := os.ReadDir(opts.destDir)
	if len(entries) != 0 {
		t.Fatalf("partial download left behind: %v", entries)
	}
}

func TestDownloadArtifactHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()
	_, err := downloadArtifact(context.Background(), testDownloadOpts(t, srv.URL+"/missing.tgz"))
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("want HTTP 404 error, got %v", err)
	}
}

// An un-allowed query parameter must be refused BEFORE any request
// leaves the box, and the refusal must be a validationError — that is
// what makes the webhook answer 422 with the explanation in the body,
// so the CI author sees exactly which parameter tripped the gate.
func TestDownloadRejectsUnallowedQueryParam(t *testing.T) {
	contacted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		contacted = true
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()

	opts := testDownloadOpts(t, srv.URL+"/a.tgz?p=2&token=SECRETVALUE")
	_, err := downloadArtifact(context.Background(), opts)
	if err == nil {
		t.Fatal("un-allowed query admitted")
	}
	var vErr validationError
	if !errors.As(err, &vErr) {
		t.Fatalf("refusal must be a validationError (422 to the caller), got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), `"p"`) || !strings.Contains(err.Error(), "query parameter") {
		t.Fatalf("refusal should name the parameter: %v", err)
	}
	if strings.Contains(err.Error(), "SECRETVALUE") {
		t.Fatalf("refusal leaked a query value: %v", err)
	}
	if contacted {
		t.Fatal("artifact server was contacted despite the refusal")
	}

	// Declaring the names in the entry admits the same URL.
	opts.allowlist = mustAllowlist(t, entryFor(srv.URL)+"?p&token")
	if _, err := downloadArtifact(context.Background(), opts); err != nil {
		t.Fatalf("declared params should admit: %v", err)
	}
	if !contacted {
		t.Fatal("download did not happen after declaring the params")
	}
}

func TestRedactURLDropsQuery(t *testing.T) {
	u, _ := url.Parse("https://gitlab.com/api/artifacts/7?private_token=SECRET")
	got := redactURL(u)
	if strings.Contains(got, "SECRET") || !strings.Contains(got, "gitlab.com/api/artifacts/7") {
		t.Fatalf("redactURL leaked or mangled: %q", got)
	}
}

// A permitted-looking https URL must not be able to downgrade the
// fetch to cleartext via a redirect: Go's client follows https->http
// redirects by default, so the scheme policy is enforced per hop in
// CheckRedirect.
func TestDownloadRefusesHTTPSToHTTPDowngrade(t *testing.T) {
	// The cleartext destination that must never be reached.
	reached := false
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		_, _ = w.Write([]byte("plaintext"))
	}))
	defer httpSrv.Close()
	// The https entry point that tries the downgrade, with a presigned
	// query on the refused hop: the refusal quotes the raw Location
	// header, and must not carry it.
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, httpSrv.URL+"/asset?X-Amz-Signature=SECRETTHREE", http.StatusFound)
	}))
	defer tlsSrv.Close()

	client := newDownloadClient(false)
	// Trust the httptest TLS cert on our transport (and only ours —
	// the CheckRedirect under test must be the real one).
	pool := x509.NewCertPool()
	pool.AddCert(tlsSrv.Certificate())
	client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: pool}

	_, err := downloadArtifact(context.Background(), downloadOpts{
		url:       tlsSrv.URL + "/artifact.tar.gz",
		destDir:   t.TempDir(),
		maxBytes:  1 << 20,
		allowlist: mustAllowlist(t, entryFor(tlsSrv.URL)),
		client:    client,
	})
	if err == nil || !strings.Contains(err.Error(), "downgrades") {
		t.Fatalf("want downgrade refusal, got %v", err)
	}
	if reached {
		t.Fatal("the cleartext destination was contacted despite the refusal")
	}
	if strings.Contains(err.Error(), "SECRETTHREE") {
		t.Fatalf("refusal leaked the redirect target's query: %v", err)
	}
}

// unreachableHost is a host the test client refuses to dial: a
// transport failure on a chosen hop, deterministic and without a
// name lookup (the transport hands DialContext the address as-is).
const unreachableHost = "unreachable.invalid"

func TestDownloadClientBoundsEveryStageBeforeTheBody(t *testing.T) {
	// A host that accepts the TCP connection and then stalls — at the
	// handshake or before its headers — must be cut off by the client
	// itself: the request context has no deadline, so an unbounded
	// stage would hold the per-app deploy lock for as long as the CI
	// client stays on the line.
	tr := newDownloadClient(false).Transport.(*http.Transport)
	if tr.DialContext == nil {
		t.Fatal("the connect stage must be bounded by a dialer with a timeout")
	}
	if tr.TLSHandshakeTimeout <= 0 {
		t.Fatal("the TLS handshake must be bounded")
	}
	if tr.ResponseHeaderTimeout <= 0 {
		t.Fatal("the wait for response headers must be bounded")
	}
	if !tr.ForceAttemptHTTP2 {
		t.Fatal("a custom dialer turns off net/http's automatic h2; the fetch must keep asking for it")
	}
	for name, d := range map[string]time.Duration{
		"dial": downloadDialTimeout, "tls": downloadTLSHandshakeTimeout, "headers": downloadResponseHeaderTimeout,
	} {
		if d <= 0 || d > time.Minute {
			t.Fatalf("%s bound %v is not a bound a stalled host should get", name, d)
		}
	}
}

// refusingDownloadClient is the real download client with one host
// unreachable at the dial, every other address dialed as usual.
func refusingDownloadClient(t *testing.T) *http.Client {
	t.Helper()
	client := newDownloadClient(true)
	dialer := &net.Dialer{}
	tr := client.Transport.(*http.Transport)
	// No proxy: with HTTP_PROXY in the environment the dial would be
	// to the proxy, not the host the test names.
	tr.Proxy = nil
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if host, _, err := net.SplitHostPort(addr); err == nil && host == unreachableHost {
			return nil, errors.New("dial refused by the test")
		}
		return dialer.DialContext(ctx, network, addr)
	}
	return client
}

// A failure on a later hop is reported by the client as that hop's
// URL — the redirect target, query included — and a redirect target
// is exactly where a presigned URL (S3, GitLab) lives. The wrapped
// error must name the hop without its query, and stay a *url.Error
// for callers that look for one.
func TestDownloadFailureOnRedirectTargetKeepsItsQueryOut(t *testing.T) {
	target := "http://" + unreachableHost + "/asset?X-Amz-Signature=SECRETTWO"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, http.StatusFound)
	}))
	defer srv.Close()

	opts := testDownloadOpts(t, srv.URL+"/a.tgz")
	opts.client = refusingDownloadClient(t)
	_, err := downloadArtifact(context.Background(), opts)
	if err == nil {
		t.Fatal("a redirect to an unreachable host must fail")
	}
	if strings.Contains(err.Error(), "SECRETTWO") || strings.Contains(err.Error(), "?") {
		t.Fatalf("hop failure leaked the redirect target's query: %v", err)
	}
	var ue *url.Error
	if !errors.As(err, &ue) {
		t.Fatalf("the client's *url.Error must survive redaction, got %T: %v", err, err)
	}
	if !strings.Contains(ue.URL, "/asset") {
		t.Fatalf("redaction must keep the failing hop's host and path, got %q", ue.URL)
	}
}

// The first hop's own URL may carry a vouched query too (a presigned
// URL the operator declared by name); a connect failure there quotes
// it the same way.
func TestDownloadFailureOnFirstHopKeepsItsQueryOut(t *testing.T) {
	opts := testDownloadOpts(t, "http://"+unreachableHost+"/a.tgz?token=SECRETONE")
	opts.allowlist = mustAllowlist(t, unreachableHost+"?token")
	opts.client = refusingDownloadClient(t)
	_, err := downloadArtifact(context.Background(), opts)
	if err == nil {
		t.Fatal("an unreachable host must fail")
	}
	if strings.Contains(err.Error(), "SECRETONE") || strings.Contains(err.Error(), "?") {
		t.Fatalf("first-hop failure leaked the query: %v", err)
	}
}

// A Location header the client cannot parse is quoted inside the
// cause, not in the URL, and the parse error inside that quotes it
// again. Neither copy may survive.
func TestDownloadUnparseableLocationKeepsItsQueryOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/bad%zz?token=SECRETFIVE")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	opts := testDownloadOpts(t, srv.URL+"/a.tgz")
	opts.client = newDownloadClient(true)
	_, err := downloadArtifact(context.Background(), opts)
	if err == nil {
		t.Fatal("an unparseable Location must fail")
	}
	if strings.Contains(err.Error(), "SECRETFIVE") || strings.Contains(err.Error(), "?") || strings.Contains(err.Error(), "%zz") {
		t.Fatalf("unparseable Location leaked the header: %v", err)
	}
	if !strings.Contains(err.Error(), "Location") {
		t.Fatalf("the cause should still say what failed: %v", err)
	}
}

// A refused redirect quotes the raw Location header, which may be
// relative; the redirect cap is the refusal that needs no second host.
func TestDownloadRedirectCapKeepsTheRelativeLocationQueryOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop?sig=SECRETFOUR", http.StatusFound)
	}))
	defer srv.Close()

	opts := testDownloadOpts(t, srv.URL+"/a.tgz")
	opts.client = newDownloadClient(true)
	_, err := downloadArtifact(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "10 redirects") {
		t.Fatalf("want the redirect cap, got %v", err)
	}
	if strings.Contains(err.Error(), "SECRETFOUR") || strings.Contains(err.Error(), "?") {
		t.Fatalf("redirect cap leaked the Location query: %v", err)
	}
	if !strings.Contains(err.Error(), "/loop") {
		t.Fatalf("the refused Location's path should still be named: %v", err)
	}
}

// Anything that is not the client's own error passes through untouched.
func TestRedactRequestErrorLeavesOtherErrorsAlone(t *testing.T) {
	err := errors.New("plain")
	if got := redactRequestError(err); got.Error() != "plain" || !errors.Is(got, err) {
		t.Fatalf("got %v", got)
	}
}

// With allow_insecure_http the same redirect is permitted — the policy
// belongs to the operator, per hop.
func TestDownloadAllowsDowngradeWhenInsecureAllowed(t *testing.T) {
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("payload"))
	}))
	defer httpSrv.Close()
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, httpSrv.URL, http.StatusFound)
	}))
	defer tlsSrv.Close()

	client := newDownloadClient(true)
	pool := x509.NewCertPool()
	pool.AddCert(tlsSrv.Certificate())
	client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: pool}

	path, err := downloadArtifact(context.Background(), downloadOpts{
		url:           tlsSrv.URL + "/artifact.tar.gz",
		destDir:       t.TempDir(),
		maxBytes:      1 << 20,
		allowInsecure: true,
		allowlist:     mustAllowlist(t, entryFor(tlsSrv.URL)),
		client:        client,
	})
	if err != nil {
		t.Fatalf("downgrade should be permitted with allow_insecure_http: %v", err)
	}
	if path == "" {
		t.Fatal("no artifact path returned")
	}
}

// Userinfo embedded in a webhook URL must never reach the wire: the
// outgoing URL is rebuilt field-by-field from the matched allowlist
// entry, and userinfo is deliberately not among the fields —
// credentials travel via auth_header only.
func TestDownloadDropsURLUserinfo(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	withUser := "http://leak:hunter2@" + u.Host + "/a.tgz"
	opts := testDownloadOpts(t, withUser)
	if _, err := downloadArtifact(context.Background(), opts); err != nil {
		t.Fatalf("download: %v", err)
	}
	if gotAuth != "" {
		t.Fatalf("userinfo leaked as Authorization: %q", gotAuth)
	}
}
