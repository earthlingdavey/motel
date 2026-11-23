package box

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/smallhoursorg/motel/liveswap/deploytrust"
	"github.com/smallhoursorg/motel/liveswap/deploytrust/trusttest"
)

// sha is a commit id.
const sha = "3f9a1c2b4d5e6f708192a3b4c5d6e7f8091a2b3c"

const jwt = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJyZXBvIn0.c2lnbmF0dXJlX2hlcmVfMTIz" // gitleaks:allow

type rig struct {
	h         *Handler
	clock     *trusttest.Clock
	priv      ed25519.PrivateKey
	logs      *observer.ObservedLogs
	dir       string // the exchange tree
	installed string
	nexted    int // requests passed to the next handler
}

func newRig(t testing.TB) *rig {
	t.Helper()
	priv, pub := trusttest.GenerateKey()
	vs, err := deploytrust.New([]deploytrust.TrustConfig{{Kind: "local", PublicKey: trusttest.KeyFile(t, pub), Audience: "box1"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	core, logs := observer.New(zap.InfoLevel)
	r := &rig{clock: trusttest.NewClock(), priv: priv, logs: logs, dir: filepath.Join(tmp, "motel-box"), installed: filepath.Join(tmp, "Caddyfile")}
	r.h = &Handler{
		app:       &App{verifiers: vs},
		logger:    zap.New(core),
		limiter:   deploytrust.NewLimiter(r.clock),
		installed: r.installed,
		dir:       r.dir,
		secrets:   filepath.Join(tmp, "secrets"),
		agePub:    filepath.Join(tmp, "age.pub"),
		slot:      make(chan struct{}, 1),
	}
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	r.write(t, r.installed, file(good))
	return r
}

func (r *rig) token(t testing.TB) string { return trusttest.Mint(t, r.priv, "box1", nil) }

func (r *rig) write(t testing.TB, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (r *rig) writeJSON(t testing.TB, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	r.write(t, path, b)
}

func (r *rig) applied(t *testing.T, commit string) {
	r.writeJSON(t, filepath.Join(r.dir, "applied.json"), applied{SHA: commit, Path: "box1/Caddyfile", SHA256: strings.Repeat("0", 64), Signer: "alice", When: r.clock.Now()})
}

type req struct {
	method, target, token string
	header                http.Header
	body                  io.Reader
}

// do serves one request, failing the test rather than hanging if the
// handler blocks (a FIFO opened without O_NONBLOCK would).
func (r *rig) do(t *testing.T, q req) *httptest.ResponseRecorder {
	t.Helper()
	if q.method == "" {
		q.method = http.MethodGet
	}
	hr := httptest.NewRequest(q.method, q.target, q.body)
	for k, vs := range q.header {
		hr.Header[k] = vs
	}
	if q.token != "" {
		hr.Header.Set("Authorization", "Bearer "+q.token)
	}
	w := httptest.NewRecorder()
	done := make(chan error, 1)
	go func() {
		done <- r.h.ServeHTTP(w, hr, nextFunc(func(http.ResponseWriter, *http.Request) error {
			r.nexted++
			return nil
		}))
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeHTTP: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the handler blocked")
	}
	return w
}

type nextFunc func(http.ResponseWriter, *http.Request) error

func (f nextFunc) ServeHTTP(w http.ResponseWriter, r *http.Request) error { return f(w, r) }

func body(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type %q, body %s", ct, w.Body)
	}
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("not JSON: %v: %s", err, w.Body)
	}
	return m
}

func wantError(t *testing.T, w *httptest.ResponseRecorder, code int, msg string) {
	t.Helper()
	if w.Code != code {
		t.Fatalf("status %d, want %d: %s", w.Code, code, w.Body)
	}
	if got, _ := body(t, w)["error"].(string); !strings.Contains(got, msg) {
		t.Fatalf("error %q, want it to contain %q", got, msg)
	}
}

const unauthorized = "invalid or missing deploy token (Authorization: Bearer <jwt>)"

func TestHandlerPassesOtherPaths(t *testing.T) {
	r := newRig(t)
	for _, target := range []string{"/x", "/x?result=00", "/deploy/app", "/%2F", "/index.html", "/./"} {
		for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut} {
			before := r.nexted
			w := r.do(t, req{method: m, target: target})
			if r.nexted != before+1 || w.Body.Len() != 0 || len(w.Header()) != 0 {
				t.Errorf("%s %s was answered (%d %s), not passed on", m, target, w.Code, w.Body)
			}
		}
	}
	if r.h.limiter.Size() != 0 {
		t.Error("a passed-on request was charged")
	}
}

func TestHandlerMethodBeforeAuth(t *testing.T) {
	r := newRig(t)
	for _, m := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodHead, http.MethodOptions, "BREW"} {
		w := r.do(t, req{method: m, target: "/"})
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, POST" {
			t.Errorf("%s: %d, Allow %q", m, w.Code, w.Header().Get("Allow"))
		}
		if m != http.MethodHead {
			wantError(t, w, http.StatusMethodNotAllowed, msgMethod)
		}
	}
	if r.h.limiter.Size() != 0 || r.nexted != 0 {
		t.Error("a 405 was charged or passed on")
	}
}

func TestHandlerAuthenticates(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	for _, q := range []req{
		{target: "/"},
		{target: "/?result=" + id},
		{target: "/?result=nonsense"},
		{target: "/?other=1"},
		{method: http.MethodPost, target: "/", body: strings.NewReader("x")},
	} {
		t.Run(q.method+" "+q.target, func(t *testing.T) {
			r := newRig(t)
			r.applied(t, sha)
			var first *httptest.ResponseRecorder
			for _, token := range []string{"", "not.a.jwt", trusttest.MintExpired(t, r.priv, "box1", nil), trusttest.Mint(t, r.priv, "another-box", nil)} {
				q.token = token
				w := r.do(t, q)
				wantError(t, w, http.StatusUnauthorized, unauthorized)
				if first == nil {
					first = w
				}
			}
			if r.h.limiter.Size() != 1 {
				t.Fatalf("failures not charged: %d addresses", r.h.limiter.Size())
			}
			for range deploytrust.FailBudget {
				r.do(t, q)
			}
			// Past the budget the answer is the first refusal's, byte for
			// byte: a caller cannot see the budget.
			w := r.do(t, q)
			if w.Code != first.Code || !reflect.DeepEqual(w.Header(), first.Header()) || w.Body.String() != first.Body.String() {
				t.Fatalf("past the budget: %d %v %s; want the first refusal's %d %v %s", w.Code, w.Header(), w.Body, first.Code, first.Header(), first.Body)
			}
		})
	}
}

func TestHandlerJournalNamesTheRoute(t *testing.T) {
	r := newRig(t)
	r.do(t, req{method: http.MethodPost, target: "/"})
	got := r.logs.FilterMessage("webhook auth failed").All()
	if len(got) != 1 || got[0].ContextMap()["box_request"] != "push" || got[0].ContextMap()["app"] != nil {
		t.Fatalf("want one line scoped box_request=push: %v", r.logs.All())
	}
}

func TestHandlerStatus(t *testing.T) {
	r := newRig(t)
	wantError(t, r.do(t, req{target: "/", token: r.token(t)}), http.StatusConflict, msgNoBaseline)

	// applied.json records the file installed: nothing is said of an
	// edit.
	sum := sha256.Sum256(file(good))
	r.writeJSON(t, filepath.Join(r.dir, "applied.json"), applied{SHA: sha, Path: "box1/Caddyfile", SHA256: hex.EncodeToString(sum[:]), Signer: "alice", When: r.clock.Now()})
	w := r.do(t, req{target: "/", token: r.token(t)})
	want := map[string]any{"commit": sha, "box_webhook": "deploy.example.com", "sha256": hex.EncodeToString(sum[:])}
	if got := body(t, w); w.Code != http.StatusOK || len(got) != len(want) || got["commit"] != want["commit"] || got["box_webhook"] != want["box_webhook"] || got["sha256"] != want["sha256"] {
		t.Fatalf("%d %v, want %v", w.Code, got, want)
	}

	// applied.json's cap holds a 4 KiB path that JSON's escapes grow
	// sixfold (each of these bytes is written \u0026).
	r.writeJSON(t, filepath.Join(r.dir, "applied.json"), applied{SHA: sha, Path: strings.Repeat("&", 4096), SHA256: strings.Repeat("0", 64), Signer: strings.Repeat("&", 256)})
	if w := r.do(t, req{target: "/", token: r.token(t)}); w.Code != http.StatusOK {
		t.Fatalf("an escaped 4 KiB path: %d %s", w.Code, w.Body)
	}

	// A console edit since the last apply shows in the digest, and is
	// said: the digest is not the one applied.json records for the
	// commit, as a push would report it (#577).
	r.writeJSON(t, filepath.Join(r.dir, "applied.json"), applied{SHA: sha, Path: "box1/Caddyfile", SHA256: hex.EncodeToString(sum[:]), Signer: "alice", When: r.clock.Now()})
	edited := append(file(good), "# edited\n"...)
	r.write(t, r.installed, edited)
	w = r.do(t, req{target: "/", token: r.token(t)})
	if got := body(t, w); got["sha256"] == want["sha256"] || got["caddyfile_edited_out_of_band"] != true || got["commit"] != sha {
		t.Fatalf("an edited file: %v", got)
	}
	// Edited back to the bytes applied: nothing is said again.
	r.write(t, r.installed, file(good))
	if got := body(t, r.do(t, req{target: "/", token: r.token(t)})); len(got) != len(want) {
		t.Fatalf("the applied file again: %v", got)
	}
}

// While a push that changes the Caddyfile is applied, the installed
// file is its new bytes and applied.json still names the commit before,
// until the applier advances it after the reload: GET / then says
// nothing of an edit, since the bytes are the applier's own (review of
// #577). The record (txn.json) stands for that window, and for a run
// stopped past its swap until recovery.
func TestHandlerStatusSaysNoEditDuringAnApply(t *testing.T) {
	p := newPushRig(t)
	get := func() map[string]any { return p.status(t) }
	c2, _ := p.change()
	var during map[string]any
	p.sd.onReload = func(int) {
		// The swap is done and the reload under way: the new file is
		// installed, the record stands, applied.json names the base.
		if !exists(p.x("txn.json")) || p.applied().SHA != p.base || !bytes.Equal(p.installed(), p.repo.files[c2]) {
			t.Errorf("the window is not open: record %v, applied %s", exists(p.x("txn.json")), p.applied().SHA)
		}
		during = get()
	}
	if r := p.pushOne(p.repo.bundleFiles(c2, p.base)); r.Phase != phaseApplied {
		t.Fatalf("the push: %+v", r)
	}
	if during["commit"] != p.base || during["sha256"] != digest(p.repo.files[c2]) || during["caddyfile_edited_out_of_band"] != nil {
		t.Errorf("GET / during the apply: %v", during)
	}
	if after := get(); after["commit"] != c2 || after["caddyfile_edited_out_of_band"] != nil {
		t.Errorf("GET / after the apply: %v", after)
	}
	// A console edit with no record standing is said.
	p.writeInstalled(append(p.installed(), "# edited\n"...))
	if edited := get(); edited["caddyfile_edited_out_of_band"] != true {
		t.Errorf("GET / after a console edit: %v", edited)
	}
}

// An apply that ends while GET / reads, its applied.json read before
// the advance and its record gone by the lookup after: still no edit,
// the record having stood when the reads began.
func TestHandlerStatusSaysNoEditAsAnApplyEnds(t *testing.T) {
	p := newPushRig(t)
	c2, _ := p.change()
	v2 := p.repo.files[c2]
	p.writeInstalled(v2) // swapped, the reload under way
	if err := os.WriteFile(p.x("txn.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	p.h.hooks.crash = func(point string) {
		if point != "status:read" {
			return
		}
		// The applier advances and ends between the reads and the
		// lookup after them.
		p.writeApplied(applied{SHA: c2, Path: testPath, SHA256: digest(v2), Signer: p.alice.principal, When: p.clock.Now()})
		if err := os.Remove(p.x("txn.json")); err != nil {
			t.Fatal(err)
		}
	}
	if got := p.status(t); got["commit"] != p.base || got["caddyfile_edited_out_of_band"] != nil {
		t.Errorf("GET / as the apply ends: %v", got)
	}
}

// status is GET / on the rig's handler, answered 200.
func (p *pushRig) status(t *testing.T) map[string]any {
	t.Helper()
	hr := httptest.NewRequest(http.MethodGet, "/", nil)
	hr.Header.Set("Authorization", "Bearer "+p.token(t, ""))
	w := httptest.NewRecorder()
	if err := p.h.ServeHTTP(w, hr, nextFunc(func(http.ResponseWriter, *http.Request) error { return errors.New("passed on") })); err != nil || w.Code != http.StatusOK {
		t.Fatalf("GET /: %v %d %s", err, w.Code, w.Body)
	}
	return body(t, w)
}

// After `moteld box baseline`, applied.json keeps the record before's
// sha256, no claim about the commit it names, and records the installed
// file's digest apart (found_sha256): GET / holds the file to that
// (review of #577). (a) A push applied whose baseline could not be
// recorded, then the baseline its result asks for: the file is that
// commit's, and was said edited on every read. Then the same commit
// pushed again, which the fast path answers no_change with nothing
// written, and a console edit: said. (b) A trust reset over the file
// before: nothing said. (c) A baseline of an older release, with no
// found_sha256: nothing said, edit or not.
func TestHandlerStatusAfterABaseline(t *testing.T) {
	t.Run("the unrecorded push", func(t *testing.T) {
		p := newPushRig(t)
		c2, _ := p.change()
		p.writeInstalled(p.repo.files[c2]) // the push installed it; applied.json still names the base
		if err := runBaseline(context.Background(), 0, p.applier(hooks{}), c2, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		if got := p.status(t); got["commit"] != c2 || got["sha256"] != digest(p.repo.files[c2]) || got["caddyfile_edited_out_of_band"] != nil {
			t.Errorf("GET / after the baseline: %v", got)
		}
		id := randomID(t)
		w := p.post(t, p.h, push{id: id, token: p.token(t, c2), body: bytes.NewReader(tgz(t, p.repo.bundleFiles(c2, p.base)))})
		wantResult(t, w, http.StatusOK, id, phaseNoChange)
		if a := p.applied(); a.Signer != signerBaseline {
			t.Fatalf("the fast path wrote applied.json: %+v", a)
		}
		p.writeInstalled(append(p.installed(), "# 3am\n"...))
		if got := p.status(t); got["caddyfile_edited_out_of_band"] != true {
			t.Errorf("GET / after a console edit since the baseline: %v", got)
		}
	})
	t.Run("a trust reset", func(t *testing.T) {
		p := newPushRig(t)
		reset := p.repo.commit(boxFile(7, p.alice), &p.alice)
		if err := runBaseline(context.Background(), 0, p.applier(hooks{}), reset, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		if got := p.status(t); got["commit"] != reset || got["sha256"] != digest(p.v1) || got["caddyfile_edited_out_of_band"] != nil {
			t.Errorf("GET / after the reset: %v", got)
		}
	})
	t.Run("an older baseline", func(t *testing.T) {
		p := newPushRig(t)
		reset := p.repo.commit(boxFile(7, p.alice), &p.alice)
		p.writeApplied(applied{SHA: reset, Path: testPath, SHA256: digest(p.v1), Signer: signerBaseline, When: p.clock.Now()})
		p.writeInstalled(append(p.installed(), "# 3am\n"...))
		if got := p.status(t); got["commit"] != reset || got["caddyfile_edited_out_of_band"] != nil {
			t.Errorf("GET / with an older baseline's record: %v", got)
		}
	})
}

func TestHandlerStatusBoxErrors(t *testing.T) {
	big := append(file(good), bytes.Repeat([]byte("#"), 1<<20)...)
	for name, c := range map[string]struct {
		setup func(t *testing.T, r *rig)
		want  string
	}{
		"installed missing":     {func(t *testing.T, r *rig) { must(t, os.Remove(r.installed)) }, "could not read the Caddyfile this box runs: open "},
		"installed too big":     {func(t *testing.T, r *rig) { r.write(t, r.installed, big) }, "larger than 1048576 bytes"},
		"installed a FIFO":      {func(t *testing.T, r *rig) { must(t, os.Remove(r.installed)); mkfifo(t, r.installed) }, "not a regular file"},
		"installed a dir":       {func(t *testing.T, r *rig) { must(t, os.Remove(r.installed)); must(t, os.Mkdir(r.installed, 0o755)) }, "not a regular file"},
		"applied not JSON":      {func(t *testing.T, r *rig) { r.write(t, filepath.Join(r.dir, "applied.json"), []byte("{")) }, "could not read the baseline"},
		"applied sha not an id": {func(t *testing.T, r *rig) { r.applied(t, "HEAD") }, "sha is not a 40-hex commit id"},
		"applied too big": {func(t *testing.T, r *rig) {
			r.write(t, filepath.Join(r.dir, "applied.json"), bytes.Repeat([]byte(" "), maxApplied+1))
		}, "larger than"},
		"applied a symlink": {func(t *testing.T, r *rig) {
			r.applied(t, sha)
			must(t, os.Rename(filepath.Join(r.dir, "applied.json"), filepath.Join(r.dir, "real.json")))
			must(t, os.Symlink("real.json", filepath.Join(r.dir, "applied.json")))
		}, "could not read the baseline"},
		"applied a FIFO": {func(t *testing.T, r *rig) {
			must(t, os.Remove(filepath.Join(r.dir, "applied.json")))
			mkfifo(t, filepath.Join(r.dir, "applied.json"))
		}, "not a regular file"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.applied(t, sha)
			c.setup(t, r)
			w := r.do(t, req{target: "/", token: r.token(t)})
			wantError(t, w, http.StatusInternalServerError, c.want)
			if strings.Contains(w.Body.String(), jwt) {
				t.Errorf("a token-shaped address in the answer: %s", w.Body)
			}
			if len(w.Body.String()) > 600 {
				t.Errorf("an unbounded 500: %d bytes", w.Body.Len())
			}
			if r.logs.FilterLevelExact(zap.ErrorLevel).Len() != 1 {
				t.Errorf("want one error line: %v", r.logs.All())
			}
		})
	}
}

// A file the box runs that the walk refuses — a console edit that
// imports, say — is the operator's to mend, not the box failing: 409,
// as before init, with the refusal's reason, and journaled as a warning.
// The reason quotes the operator's own file as written, bounded: an
// address shaped like a token too, which an older moteld's heuristics
// masked in the answer (no filter runs over a box answer now).
func TestHandlerStatusRunsARefusedFile(t *testing.T) {
	for name, c := range map[string]struct {
		file string
		want string
	}{
		"an import": {strings.Replace(good, "\tbox_webhook\n", "\tbox_webhook\n\timport /root/backup.caddy\n", 1),
			"the Caddyfile this box runs imports /root/backup.caddy; inline the snippet"},
		"not a box's": {"{\n}\nx.com {\n}\n", "the Caddyfile this box runs has no box block"},
		"no signer": {strings.Replace(good, "\t\tsigner alice@example.com ssh-ed25519 K1\n\t\tsigner bob ssh-ed25519 K2\n", "", 1),
			"the Caddyfile this box runs lists no signer; moteld init is the way back"},
		"with input": {strings.Replace(good, "deploy.example.com {", strings.Repeat("a", 2000)+".com {", 1),
			"has a box_webhook site whose address is not one bare hostname"},
		"a token-shaped address": {strings.Replace(good, "deploy.example.com {", "https://"+jwt+" {", 1), "(https://" + jwt + ")"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.applied(t, sha)
			r.write(t, r.installed, file(c.file))
			w := r.do(t, req{target: "/", token: r.token(t)})
			wantError(t, w, http.StatusConflict, c.want)
			if len(w.Body.String()) > 600 {
				t.Errorf("an unbounded 409: %d bytes", w.Body.Len())
			}
			warned := r.logs.FilterMessage("box webhook runs a Caddyfile it cannot speak for").FilterLevelExact(zap.WarnLevel).All()
			if len(warned) != 1 || r.logs.FilterLevelExact(zap.ErrorLevel).Len() != 0 {
				t.Errorf("want one warning and no error line: %v", r.logs.All())
			}
		})
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func mkfifo(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Fatal(err)
	}
}

// failingReader fails the test if the handler reads the body.
type failingReader struct{ t *testing.T }

func (f failingReader) Read([]byte) (int, error) {
	f.t.Error("the body was read")
	return 0, io.EOF
}

// TestHandlerPushAuthenticatesFirst: an unauthenticated push is the
// flat 401, charged, its body unread.
func TestHandlerPushAuthenticatesFirst(t *testing.T) {
	r := newRig(t)
	wantError(t, r.do(t, req{method: http.MethodPost, target: "/", body: failingReader{t}}), http.StatusUnauthorized, unauthorized)
	if r.h.limiter.Size() != 1 {
		t.Fatal("a refused push was not charged")
	}
}
