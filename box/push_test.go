package box

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"

	"github.com/smallhoursorg/motel/liveswap/deploytrust"
	"github.com/smallhoursorg/motel/liveswap/deploytrust/trusttest"
)

// pushRig is a box (the applier's harness: a signed repository, the
// real ssh-keygen, a tree under a temporary root) with box_webhook in
// front of it, on the same tree.
type pushRig struct {
	*testBox
	h    *Handler
	priv ed25519.PrivateKey
	vs   []deploytrust.Verifier
	val  *fakeValidator
	clk  *sleepClock
	slot chan struct{} // the process's push slot, which every handler of the rig shares
}

// fakeValidator answers step 6 as told and records what it was given.
type fakeValidator struct {
	mu    sync.Mutex
	msg   string
	err   error
	calls int
	got   []byte
	host  string   // the host reachability was asked for
	door  []string // the loaded server's listen addresses it was given
	// boxSecrets are the box slot's variables it was told the push carries.
	boxSecrets []string
	// block, when set, is waited on inside validate: the admission lock
	// is held meanwhile.
	block   chan struct{}
	entered chan struct{}
}

func (f *fakeValidator) validate(ctx context.Context, _ string, caddyfile []byte, host string, door, boxSecrets []string) (string, error) {
	f.mu.Lock()
	f.calls++
	f.got, f.host, f.door, f.boxSecrets = caddyfile, host, door, boxSecrets
	block, entered := f.block, f.entered
	msg, err := f.msg, f.err
	f.mu.Unlock()
	if entered != nil {
		close(entered)
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return "", ctx.Err() // as the real children report a caller gone
		}
	}
	return msg, err
}

// sleepClock is the box's fake clock, with a callback at each of step
// 8's sleeps: a test runs the applier there, or writes a result.
type sleepClock struct {
	*fakeClock
	mu      sync.Mutex
	onSleep func()
	sleeps  int
}

func (c *sleepClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.sleeps++
	f := c.onSleep
	c.mu.Unlock()
	if f != nil {
		f()
	}
	c.advance(d)
	return nil
}

func newPushRig(t testing.TB) *pushRig {
	t.Helper()
	b := newTestBox(t)
	priv, pub := trusttest.GenerateKey()
	vs, err := deploytrust.New([]deploytrust.TrustConfig{{Kind: "local", PublicKey: trusttest.KeyFile(t, pub), Audience: "box1"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	p := &pushRig{testBox: b, priv: priv, vs: vs, val: &fakeValidator{}, clk: &sleepClock{fakeClock: b.clock}, slot: make(chan struct{}, 1)}
	p.h = p.handler()
	return p
}

// handler is a box_webhook on the rig's tree; two of them stand for the
// handler before and after a reload.
func (p *pushRig) handler() *Handler {
	return &Handler{
		app: &App{verifiers: p.vs}, logger: zap.New(p.core), limiter: deploytrust.NewLimiter(trusttest.NewClock()),
		installed: filepath.Join(p.root, installedFile), dir: filepath.Join(p.root, exchangeDir), secrets: filepath.Join(p.root, secretsDir), agePub: filepath.Join(p.root, agePubFile),
		verifier: p.verifier, validator: p.val, clock: p.clk, slot: p.slot,
	}
}

func (p *pushRig) token(t testing.TB, sha string) string {
	var claims map[string]string
	if sha != "" {
		claims = map[string]string{"sha": sha}
	}
	return trusttest.Mint(t, p.priv, "box1", claims)
}

// change is a commit on the baseline changing the file: v2, signed by
// alice, and its bundle.
func (p *pushRig) change() (string, []byte) {
	v2 := boxFile(2, p.alice)
	c2 := p.repo.commit(v2, &p.alice, p.base)
	return c2, tgz(p.t, p.repo.bundleFiles(c2, p.base))
}

type push struct {
	id, token, ctype string
	body             io.Reader
	length           int64 // set when not -1 (unknown) or the body's own
	header           http.Header
	ctx              context.Context
}

// post serves one push, failing the test rather than hanging.
func (p *pushRig) post(t *testing.T, h *Handler, q push) *httptest.ResponseRecorder {
	t.Helper()
	hr := p.request(q)
	w := httptest.NewRecorder()
	done := make(chan error, 1)
	go func() {
		done <- h.ServeHTTP(w, hr, nextFunc(func(http.ResponseWriter, *http.Request) error { return errors.New("passed on") }))
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeHTTP: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the handler blocked")
	}
	return w
}

func (p *pushRig) request(q push) *http.Request {
	hr := httptest.NewRequest(http.MethodPost, "/", q.body)
	if q.ctx != nil {
		hr = hr.WithContext(q.ctx)
	}
	if q.length != 0 {
		hr.ContentLength = q.length
	}
	if q.ctype == "" {
		q.ctype = "application/gzip"
	}
	hr.Header.Set("Content-Type", q.ctype)
	if q.id != "" {
		hr.Header.Set(headerRequestID, q.id)
	}
	if q.token != "" {
		hr.Header.Set("Authorization", "Bearer "+q.token)
	}
	for k, vs := range q.header {
		hr.Header[k] = vs
	}
	return hr
}

// get serves one GET on `/` with the target's query.
func (p *pushRig) get(t *testing.T, target, token string) *httptest.ResponseRecorder {
	t.Helper()
	hr := httptest.NewRequest(http.MethodGet, target, nil)
	if token != "" {
		hr.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	if err := p.h.ServeHTTP(w, hr, nextFunc(func(http.ResponseWriter, *http.Request) error { return errors.New("passed on") })); err != nil {
		t.Fatal(err)
	}
	return w
}

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("not JSON: %v: %s", err, w.Body)
	}
	return m
}

func wantResult(t *testing.T, w *httptest.ResponseRecorder, code int, id, phase string) map[string]any {
	t.Helper()
	m := decode(t, w)
	if w.Code != code || m["id"] != id || m["phase"] != phase {
		t.Fatalf("got %d %s, want %d id %s phase %s", w.Code, w.Body, code, id, phase)
	}
	return m
}

// untouched asserts admission wrote nothing: in/ is empty, and stage/
// holds at most the admission lock.
func (p *pushRig) untouched(t *testing.T) {
	t.Helper()
	if got := p.names(p.x("in")); len(got) != 0 {
		t.Errorf("in/ = %v", got)
	}
	for _, n := range p.names(p.x("stage")) {
		if n != "lock" {
			t.Errorf("stage/ gained %s", n)
		}
	}
}

func (p *pushRig) writeResult(t *testing.T, r result) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(p.x("out"), r.ID+".json"), r.marshal(), 0o640); err != nil {
		t.Fatal(err)
	}
}

// TestPushBeforeTheBody: what is refused before the bundle is used —
// the request id, the media type, the size — writes nothing, and the
// first two never read the body.
func TestPushBeforeTheBody(t *testing.T) {
	p := newPushRig(t)
	c2, bundle := p.change()
	id := randomID(t)
	for _, tc := range []struct {
		name string
		q    push
		code int
		msg  string
	}{
		{"no id", push{body: failingReader{t}}, http.StatusBadRequest, msgRequestID},
		{"upper-case id", push{id: strings.ToUpper(id), body: failingReader{t}}, http.StatusBadRequest, msgRequestID},
		{"short id", push{id: id[:31], body: failingReader{t}}, http.StatusBadRequest, msgRequestID},
		{"long id", push{id: id + "0", body: failingReader{t}}, http.StatusBadRequest, msgRequestID},
		{"two ids", push{header: http.Header{headerRequestID: {id, id}}, body: failingReader{t}}, http.StatusBadRequest, msgRequestID},
		{"not gzip", push{id: id, ctype: "application/json", body: failingReader{t}}, http.StatusUnsupportedMediaType, msgMediaType},
		{"no media type", push{id: id, ctype: "", header: http.Header{"Content-Type": {""}}, body: failingReader{t}}, http.StatusUnsupportedMediaType, msgMediaType},
		{"declared too large", push{id: id, length: maxBody + 1, body: failingReader{t}}, http.StatusRequestEntityTooLarge, msgTooLarge},
		{"streamed too large", push{id: id, length: -1, body: io.LimitReader(zeros{}, maxBody+1)}, http.StatusRequestEntityTooLarge, msgTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.q.token = p.token(t, c2)
			w := p.post(t, p.h, tc.q)
			wantError(t, w, tc.code, tc.msg)
			p.untouched(t)
		})
	}
	// The same push, whole, is admitted: none of the above was the
	// bundle's fault.
	w := p.post(t, p.h, push{id: id, token: p.token(t, c2), body: bytes.NewReader(bundle)})
	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("the push itself: %d %s", w.Code, w.Body)
	}
}

type zeros struct{}

func (zeros) Read(b []byte) (int, error) {
	clear(b)
	return len(b), nil
}

// TestPushBundleAndToken: steps 3 and 4 refuse by name, 422 with a
// `refused` result the handler writes no file for, before the lock.
func TestPushBundleAndToken(t *testing.T) {
	p := newPushRig(t)
	c2, bundle := p.change()
	for _, tc := range []struct {
		name, sha string
		body      []byte
		msg       string
	}{
		{"not gzip", c2, []byte("plain"), "bundle: not a gzip stream"},
		{"no sha claim", "", bundle, msgNoSHAClaim},
		{"another sha", p.base, bundle, msgTokenNames(p.base, c2)},
		{"a sha that is not one", "x\ny", bundle, msgTokenNames("x\ny", c2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := randomID(t)
			w := p.post(t, p.h, push{id: id, token: p.token(t, tc.sha), body: bytes.NewReader(tc.body)})
			m := wantResult(t, w, http.StatusUnprocessableEntity, id, phaseRefused)
			if m["error"] != tc.msg {
				t.Fatalf("error %q, want %q", m["error"], tc.msg)
			}
			p.untouched(t)
			if exists(p.x("stage/lock")) {
				t.Error("the admission lock was taken before step 4 passed")
			}
		})
	}
	if strings.Contains(msgTokenNames("x\ny", c2), "\n") {
		t.Error("a claim's newline reached the message")
	}
}

// TestPushFastPath: HEAD is the baseline and the file is the installed
// one — 200 `no_change`, nothing written, no lock taken, even while
// another push is pending; after a console edit the same push is real
// work.
func TestPushFastPath(t *testing.T) {
	p := newPushRig(t)
	bundle := tgz(t, p.repo.bundleFiles(p.base, p.base))
	// Pending work in in/ does not stop the fast path.
	p.push(p.repo.bundleFiles(p.base, p.base))
	before := p.names(p.x("stage"))
	id := randomID(t)
	w := p.post(t, p.h, push{id: id, token: p.token(t, p.base), body: bytes.NewReader(bundle)})
	m := wantResult(t, w, http.StatusOK, id, phaseNoChange)
	if m["commit"] != p.base {
		t.Fatalf("commit %v: %s", m["commit"], w.Body)
	}
	if got := p.names(p.x("stage")); strings.Join(got, ",") != strings.Join(before, ",") || exists(p.x("out/"+id+".json")) {
		t.Fatalf("the fast path wrote: stage/ %v", got)
	}
	if p.val.calls != 0 {
		t.Fatal("the fast path validated")
	}
	if w := p.get(t, "/?result="+id, p.token(t, "")); w.Code != http.StatusNotFound {
		t.Fatalf("a fast path's id polls %d, want 404: %s", w.Code, w.Body)
	}

	// After a console edit the digests differ: the push goes to root
	// (once the pending one is settled).
	must(t, p.run(hooks{}))
	p.writeInstalled(append(p.installed(), []byte("# edited\n")...))
	id = randomID(t)
	w = p.post(t, p.h, push{id: id, token: p.token(t, p.base), body: bytes.NewReader(bundle)})
	if w.Code != http.StatusGatewayTimeout || !exists(p.x("in/"+id+".tar")) {
		t.Fatalf("a replay over a console edit: %d %s, in/ %v", w.Code, w.Body, p.names(p.x("in")))
	}
}

// TestPushFastPathNeedsTheShape: HEAD is the baseline and the file is
// the installed one, but the bundle is in a shape root refuses — at a
// path the box did not record, with a parent past the end of the chain,
// without a tree the file proof needs. Each is refused as root would
// refuse it, never answered no_change (a green run whose next real
// change would be the first red).
func TestPushFastPathNeedsTheShape(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edit  func(p *pushRig, files map[string][]byte)
		error string // what the refusal begins with or holds
	}{
		{"another path", func(p *pushRig, _ map[string][]byte) {
			a := p.applied()
			a.Path = "box2/Caddyfile"
			p.writeApplied(a)
		}, "this box's file is box2/Caddyfile; the bundle is " + testPath + " — "},
		{"a parent bundled", func(p *pushRig, files map[string][]byte) {
			files["parents/0001"] = p.repo.objects[p.base]
		}, "parents/0001 is past the end of the chain"},
		{"a tree missing", func(p *pushRig, files map[string][]byte) {
			delete(files, "trees/"+p.repo.trees[p.base][1])
		}, "is not in the bundle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPushRig(t)
			files := p.repo.bundleFiles(p.base, p.base)
			tc.edit(p, files)
			id := randomID(t)
			w := p.post(t, p.h, push{id: id, token: p.token(t, p.base), body: bytes.NewReader(tgz(t, files))})
			m := wantResult(t, w, http.StatusUnprocessableEntity, id, phaseRefused)
			if msg := m["error"].(string); !strings.HasPrefix(msg, tc.error) && !strings.Contains(msg, tc.error) {
				t.Fatalf("error %q, want %q in it", msg, tc.error)
			}
			p.untouched(t)
			if p.val.calls != 0 {
				t.Fatal("validated a bundle pre-verification refuses")
			}
		})
	}
}

// TestPushKnownIDBeforeTheFastPath: a retry after a lost response is
// told its push exists, even when the box now runs that push's commit —
// the fast path would otherwise answer one id two ways.
func TestPushKnownIDBeforeTheFastPath(t *testing.T) {
	p := newPushRig(t)
	bundle := tgz(t, p.repo.bundleFiles(p.base, p.base))
	id := randomID(t)
	p.writeResult(t, result{ID: id, Phase: phaseApplied, Commit: p.base})
	wantError(t, p.post(t, p.h, push{id: id, token: p.token(t, p.base), body: bytes.NewReader(bundle)}), http.StatusConflict, msgDuplicate(id))
}

// TestPushBoxFaults: a name in the exchange tree that cannot be looked
// up is the box's trouble, a 500 — never "duplicate", never "admitted".
func TestPushBoxFaults(t *testing.T) {
	p := newPushRig(t)
	c2, _ := p.change()
	id := randomID(t)
	// out/ replaced by a file: every lookup under it is ENOTDIR.
	must(t, os.RemoveAll(p.x("out")))
	p.write(t, p.x("out"), nil)
	w := p.post(t, p.h, push{id: id, token: p.token(t, c2), body: failingReader{t}})
	if w.Code != http.StatusInternalServerError || !strings.HasPrefix(decode(t, w)["error"].(string), "could not check the push: ") {
		t.Fatalf("push: %d %s", w.Code, w.Body)
	}
	p.untouched(t)
	// stage/ replaced by a file: the poll cannot tell a marker.
	must(t, os.RemoveAll(p.x("out")))
	must(t, os.Mkdir(p.x("out"), 0o755))
	must(t, os.RemoveAll(p.x("stage")))
	p.write(t, p.x("stage"), nil)
	w = p.get(t, "/?result="+id, p.token(t, ""))
	if w.Code != http.StatusInternalServerError || !strings.HasPrefix(decode(t, w)["error"].(string), "could not read the push "+id+": ") {
		t.Fatalf("poll: %d %s", w.Code, w.Body)
	}
}

func TestParseRequestID(t *testing.T) {
	id := randomID(t)
	if got, ok := parseRequestID(id); !ok || got != id {
		t.Fatalf("%q -> %q %v", id, got, ok)
	}
	for _, bad := range []string{"", id[:31], id + "0", strings.ToUpper(id), id[:31] + "g", id[:31] + "\x00"} {
		if _, ok := parseRequestID(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestPushNoBaseline(t *testing.T) {
	p := newPushRig(t)
	c2, bundle := p.change()
	must(t, os.Remove(p.x("applied.json")))
	wantError(t, p.post(t, p.h, push{id: randomID(t), token: p.token(t, c2), body: bytes.NewReader(bundle)}), http.StatusConflict, msgNoBaseline)
	p.untouched(t)
}

// TestPushAdmission is the Admission table, row by row.
func TestPushAdmission(t *testing.T) {
	for _, tc := range []struct {
		name string
		// setup prepares the tree for a push of id; it returns the
		// message a 409 must carry, or "" for an admitted push.
		setup func(t *testing.T, p *pushRig, id string) string
	}{
		{"nothing pending", func(*testing.T, *pushRig, string) string { return "" }},
		{"lock held", func(t *testing.T, p *pushRig, _ string) string {
			holdLock(t, p.x("stage/lock"))
			return msgAdmitting
		}},
		{"a bundle in in/", func(t *testing.T, p *pushRig, _ string) string {
			other := p.pushAt(p.repo.bundleFiles(p.base, p.base), p.clock.Now())
			then := p.clock.Now().Add(-90 * time.Second)
			must(t, os.Chtimes(p.x("in/"+other+".tar"), then, then))
			return msgPending(other, 90*time.Second)
		}},
		{"anything in in/", func(t *testing.T, p *pushRig, _ string) string {
			must(t, os.Mkdir(p.x("in/x\ny"), 0o755))
			return msgPending(`"x\ny"`, 0)
		}},
		{"a fresh marker", func(t *testing.T, p *pushRig, _ string) string {
			other := randomID(t)
			p.markerAt(other, p.clock.Now().Add(-5*time.Minute))
			return msgPending(other, 5*time.Minute)
		}},
		{"a fresh marker, verified", func(t *testing.T, p *pushRig, _ string) string {
			other := randomID(t)
			p.markerAt(other, p.clock.Now().Add(-5*time.Minute))
			p.writeResult(t, result{ID: other, Phase: phaseVerified})
			return msgPending(other, 5*time.Minute)
		}},
		{"a fresh marker, ended", func(t *testing.T, p *pushRig, _ string) string {
			other := randomID(t)
			p.markerAt(other, p.clock.Now().Add(-5*time.Minute))
			p.writeResult(t, result{ID: other, Phase: phaseApplied})
			return ""
		}},
		{"a marker past the bound", func(t *testing.T, p *pushRig, _ string) string {
			p.markerAt(randomID(t), p.clock.Now().Add(-pendingAge))
			return ""
		}},
		{"a marker slightly ahead", func(t *testing.T, p *pushRig, _ string) string {
			other := randomID(t)
			p.markerAt(other, p.clock.Now().Add(5*time.Minute))
			return msgPending(other, 0)
		}},
		{"a marker far ahead", func(t *testing.T, p *pushRig, _ string) string {
			p.markerAt(randomID(t), p.clock.Now().Add(pendingAge+time.Second))
			return ""
		}},
		{"a marker that does not read", func(t *testing.T, p *pushRig, _ string) string {
			p.write(t, filepath.Join(p.x("stage"), randomID(t)+".auth"), []byte("{"))
			return ""
		}},
		{"its own marker", func(t *testing.T, p *pushRig, id string) string {
			p.markerAt(id, p.clock.Now().Add(-time.Hour))
			return msgDuplicate(id)
		}},
		{"its own result", func(t *testing.T, p *pushRig, id string) string {
			p.writeResult(t, result{ID: id, Phase: phaseFailed, Error: "x"})
			return msgDuplicate(id)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPushRig(t)
			c2, bundle := p.change()
			id := randomID(t)
			want := tc.setup(t, p, id)
			stageBefore := p.names(p.x("stage"))
			inBefore := p.names(p.x("in"))
			w := p.post(t, p.h, push{id: id, token: p.token(t, c2), body: bytes.NewReader(bundle)})
			if want != "" {
				wantError(t, w, http.StatusConflict, want)
				if p.val.calls != 0 {
					t.Error("validated a push that was not admitted")
				}
				if got := p.names(p.x("in")); strings.Join(got, ",") != strings.Join(inBefore, ",") {
					t.Errorf("in/ %v, was %v", got, inBefore)
				}
				return
			}
			if w.Code != http.StatusGatewayTimeout || decode(t, w)["id"] != id {
				t.Fatalf("admitted push: %d %s", w.Code, w.Body)
			}
			if !exists(p.x("in/"+id+".tar")) || !exists(p.x("stage/"+id+".auth")) {
				t.Fatalf("not dropped: in/ %v stage/ %v (was %v)", p.names(p.x("in")), p.names(p.x("stage")), stageBefore)
			}
		})
	}
}

// holdLock takes stage/lock as another admission would, until the test
// ends.
func holdLock(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
}

func (p *pushRig) write(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestPushPreVerification: step 5 refuses what root would, by the same
// words, before validate sees the bytes; nothing is dropped.
func TestPushPreVerification(t *testing.T) {
	p := newPushRig(t)
	v2 := boxFile(2, p.alice)
	unsigned := p.repo.commit(v2, nil, p.base)
	byMallory := p.repo.commit(v2, &p.mallory, p.base)
	signed := p.repo.commit(v2, &p.alice, p.base)
	tampered := p.repo.bundleFiles(signed, p.base)
	tampered["Caddyfile"] = boxFile(3, p.alice)
	// Refusals that name commits other than HEAD: each id must reach the
	// workflow unmasked by the response filter.
	mid := p.repo.commit(v2, nil, p.base)
	above := p.repo.commit(boxFile(3, p.alice), &p.alice, mid)
	elsewhere := p.repo.commit(v2, &p.alice, p.repo.commit(boxFile(7, p.alice), &p.alice))
	for _, tc := range []struct {
		name, head string
		files      map[string][]byte
		msg        string
	}{
		{"unsigned", unsigned, p.repo.bundleFiles(unsigned, p.base), unsigned + " is not signed; the box applies only commits signed by a key in its signer list"},
		{"unlisted", byMallory, p.repo.bundleFiles(byMallory, p.base), byMallory + " is signed by a key that is not a signer in the Caddyfile this box runs"},
		{"tampered", signed, tampered, "the file sent is not " + testPath + " in " + signed},
		{"unsigned on the chain", above, p.repo.bundleFiles(above, p.base), mid + ", between the commit this box runs and " + above +
			", is not signed; every commit on main must be — rebase it out and force-push; the box still runs " + p.base},
		{"not a descendant", elsewhere, p.repo.bundleFiles(elsewhere, p.base), elsewhere + " does not descend from the commit this box runs (" + p.base + ") along main's first-parent line. " +
			"If the box already runs a later commit, nothing is wrong. " +
			"If a branch merged main into itself before it was fast-forwarded, rebase it onto main and force-push. " +
			"Only if main was rewritten below " + p.base + " does motel box baseline " + elsewhere + ", as root on the box, reset trust to " + elsewhere + ", whose ancestors the box will then never examine"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := randomID(t)
			w := p.post(t, p.h, push{id: id, token: p.token(t, tc.head), body: bytes.NewReader(tgz(t, tc.files))})
			m := wantResult(t, w, http.StatusUnprocessableEntity, id, phaseRefused)
			if m["error"] != tc.msg || m["commit"] != tc.head {
				t.Fatalf("got %s, want error %q", w.Body, tc.msg)
			}
			p.untouched(t)
			if p.val.calls != 0 {
				t.Fatal("validate saw bytes the proof refused")
			}
			if len(p.logged(zap.WarnLevel, "box push refused")) == 0 {
				t.Fatal("no refusal line")
			}
		})
	}
}

// TestPushValidate: step 6's refusal is a 422 with its words; its
// failure is the box's, a 500; neither drops anything.
func TestPushValidate(t *testing.T) {
	p := newPushRig(t)
	c2, bundle := p.change()
	p.val.msg = "motel validate: unrecognized directive: nope"
	id := randomID(t)
	m := wantResult(t, p.post(t, p.h, push{id: id, token: p.token(t, c2), body: bytes.NewReader(bundle)}), http.StatusUnprocessableEntity, id, phaseRefused)
	if m["error"] != p.val.msg || !bytes.Equal(p.val.got, boxFile(2, p.alice)) {
		t.Fatalf("got %v, validated %q", m, p.val.got)
	}
	if shape, err := Walk(p.val.got); err != nil || p.val.host != shape.Host || shape.Host == "" {
		t.Fatalf("reachability asked for %q; the file's host is %q (%v)", p.val.host, shape.Host, err)
	}
	if p.val.door != nil {
		t.Fatalf("a push no Caddy server received has no door; got %q", p.val.door)
	}
	p.untouched(t)

	// An unreachable / is one of step 6's refusals: the same 422, the
	// same untouched tree, and the next push admitted.
	p.val.msg = msgUnreachable("static_response answers first")
	id = randomID(t)
	m = wantResult(t, p.post(t, p.h, push{id: id, token: p.token(t, c2), body: bytes.NewReader(bundle)}), http.StatusUnprocessableEntity, id, phaseRefused)
	if m["error"] != p.val.msg {
		t.Fatalf("got %v", m)
	}
	p.untouched(t)

	p.val.msg, p.val.err = "", errors.New("fork/exec /usr/bin/motel: no such file or directory")
	w := p.post(t, p.h, push{id: randomID(t), token: p.token(t, c2), body: bytes.NewReader(bundle)})
	wantError(t, w, http.StatusInternalServerError, msgCheckFailed(p.val.err))
	p.untouched(t)
	if len(p.logged(zap.ErrorLevel, "box push failed")) != 1 {
		t.Fatal("no failure line")
	}
}

// TestPushDoor: step 6 is given the door — the listen addresses of the
// Caddy server that received the push, from the request's context, as
// Caddy sets it on every request — and a moved door is one of its
// refusals: a 422 with the catalogue text, nothing dropped.
func TestPushDoor(t *testing.T) {
	p := newPushRig(t)
	c2, bundle := p.change()
	srv := &caddyhttp.Server{Listen: []string{":8443", "tcp/127.0.0.1:8443"}}
	ctx := context.WithValue(context.Background(), caddyhttp.ServerCtxKey, srv)
	p.val.msg = msgListenMoved
	id := randomID(t)
	m := wantResult(t, p.post(t, p.h, push{id: id, token: p.token(t, c2), body: bytes.NewReader(bundle), ctx: ctx}), http.StatusUnprocessableEntity, id, phaseRefused)
	if m["error"] != p.val.msg || !slices.Equal(p.val.door, srv.Listen) {
		t.Fatalf("got %v, the door %q; want the server's %q", m, p.val.door, srv.Listen)
	}
	p.untouched(t)

	// A server with no addresses is an empty door, not none: compared,
	// never skipped. (None receives a push; the sentinel is kept apart.)
	none := context.WithValue(context.Background(), caddyhttp.ServerCtxKey, &caddyhttp.Server{})
	p.val.msg = msgListenMoved
	id = randomID(t)
	wantResult(t, p.post(t, p.h, push{id: id, token: p.token(t, c2), body: bytes.NewReader(bundle), ctx: none}), http.StatusUnprocessableEntity, id, phaseRefused)
	if p.val.door == nil || len(p.val.door) != 0 {
		t.Fatalf("the door for a server with no addresses is %#v; want an empty list", p.val.door)
	}
	p.untouched(t)

	// Kept: admitted as any push, the bundle in in/ until root runs.
	p.val.msg = ""
	id = randomID(t)
	w := p.post(t, p.h, push{id: id, token: p.token(t, c2), body: bytes.NewReader(bundle), ctx: ctx})
	if w.Code != http.StatusGatewayTimeout || !exists(p.x("in/"+id+".tar")) || !slices.Equal(p.val.door, srv.Listen) {
		t.Fatalf("got %d, the door %q", w.Code, p.val.door)
	}
}

// TestPushWait is step 8: the first result root writes decides the
// status; none in 30 s is 504 with the id; the handler never waits past
// the first.
func TestPushWait(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, p *pushRig, id string) // at the first sleep
		code  int
		phase string
	}{
		{"verified", func(t *testing.T, p *pushRig, id string) { p.writeResult(t, result{ID: id, Phase: phaseVerified}) }, http.StatusAccepted, phaseVerified},
		{"refused", func(t *testing.T, p *pushRig, id string) {
			p.writeResult(t, result{ID: id, Phase: phaseRefused, Error: "no"})
		}, http.StatusUnprocessableEntity, phaseRefused},
		{"rolled back", func(t *testing.T, p *pushRig, id string) {
			p.writeResult(t, result{ID: id, Phase: phaseRolledBack, Error: msgReloadFailed})
		}, http.StatusUnprocessableEntity, phaseRolledBack},
		{"the applier", func(t *testing.T, p *pushRig, _ string) { must(t, p.run(hooks{})) }, http.StatusOK, phaseApplied},
		{"nothing", func(*testing.T, *pushRig, string) {}, http.StatusGatewayTimeout, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPushRig(t)
			c2, bundle := p.change()
			id := randomID(t)
			first := true
			p.clk.onSleep = func() {
				if first {
					first = false
					tc.write(t, p, id)
				}
			}
			w := p.post(t, p.h, push{id: id, token: p.token(t, c2), body: bytes.NewReader(bundle)})
			if tc.phase == "" {
				if w.Code != tc.code || decode(t, w)["id"] != id || p.clk.sleeps != int(resultWait/resultInterval) {
					t.Fatalf("got %d %s after %d sleeps", w.Code, w.Body, p.clk.sleeps)
				}
				return
			}
			wantResult(t, w, tc.code, id, tc.phase)
			if p.clk.sleeps != 1 {
				t.Fatalf("waited %d times past the first result", p.clk.sleeps)
			}
			if tc.phase == phaseApplied && !bytes.Equal(p.installed(), boxFile(2, p.alice)) {
				t.Fatal("applied, but the file is not the pushed one")
			}
		})
	}
}

// TestPushWaitEnds: a caller that goes stops the wait; a result that
// does not read is the box's, a 500.
func TestPushWaitEnds(t *testing.T) {
	p := newPushRig(t)
	c2, bundle := p.change()
	ctx, cancel := context.WithCancel(context.Background())
	p.clk.onSleep = cancel
	w := p.post(t, p.h, push{id: randomID(t), token: p.token(t, c2), body: bytes.NewReader(bundle), ctx: ctx})
	if w.Code != http.StatusGatewayTimeout || p.clk.sleeps != 1 {
		t.Fatalf("got %d after %d sleeps", w.Code, p.clk.sleeps)
	}

	p = newPushRig(t)
	c2, bundle = p.change()
	id := randomID(t)
	p.clk.onSleep = func() { p.write(t, filepath.Join(p.x("out"), id+".json"), []byte("{")) }
	w = p.post(t, p.h, push{id: id, token: p.token(t, c2), body: bytes.NewReader(bundle)})
	if w.Code != http.StatusInternalServerError || !strings.HasPrefix(decode(t, w)["error"].(string), "could not read the result for "+id+": ") {
		t.Fatalf("got %d %s", w.Code, w.Body)
	}
}

// TestPushDrop: the bundle into in/ first, then the marker. A crash
// between them leaves a bundle root processes and no marker: the push's
// poll reads it as admitted while it is in in/, and finds root's result
// after; a marker that cannot be written does not undo a bundle root
// already holds.
func TestPushDrop(t *testing.T) {
	t.Run("crash after the bundle", func(t *testing.T) {
		p := newPushRig(t)
		c2, bundle := p.change()
		id := randomID(t)
		p.h.hooks.crash = func(point string) {
			if point == "bundle" {
				panic(crashed{point})
			}
		}
		p.postCrash(t, push{id: id, token: p.token(t, c2), body: bytes.NewReader(bundle)})
		if !exists(p.x("in/"+id+".tar")) || exists(p.x("stage/"+id+".auth")) {
			t.Fatalf("in/ %v stage/ %v", p.names(p.x("in")), p.names(p.x("stage")))
		}
		if w := p.get(t, "/?result="+id, p.token(t, "")); w.Code != http.StatusAccepted || decode(t, w)["phase"] != "admitted" {
			t.Fatalf("before root: %d %s", w.Code, w.Body)
		}
		must(t, p.run(hooks{}))
		wantResult(t, p.get(t, "/?result="+id, p.token(t, "")), http.StatusOK, id, phaseApplied)
	})
	t.Run("crash after the marker", func(t *testing.T) {
		p := newPushRig(t)
		c2, bundle := p.change()
		id := randomID(t)
		p.h.hooks.crash = func(point string) {
			if point == "marker" {
				panic(crashed{point})
			}
		}
		p.postCrash(t, push{id: id, token: p.token(t, c2), body: bytes.NewReader(bundle)})
		// The lock went with the crash: the next push is told what is
		// pending, not that one is being admitted.
		p.h.hooks.crash = nil
		c3 := p.repo.commit(boxFile(3, p.alice), &p.alice, c2)
		wantError(t, p.post(t, p.h, push{id: randomID(t), token: p.token(t, c3), body: bytes.NewReader(tgz(t, p.repo.bundleFiles(c3, p.base)))}),
			http.StatusConflict, msgPending(id, 0))
	})
	t.Run("the bundle cannot be written", func(t *testing.T) {
		p := newPushRig(t)
		c2, bundle := p.change()
		p.h.hooks.fail = func(point string) error {
			if point == "bundle" {
				return syscall.ENOSPC
			}
			return nil
		}
		w := p.post(t, p.h, push{id: randomID(t), token: p.token(t, c2), body: bytes.NewReader(bundle)})
		wantError(t, w, http.StatusInternalServerError, msgCheckFailed(syscall.ENOSPC))
		p.untouched(t)
	})
	t.Run("in/ cannot be synced", func(t *testing.T) {
		p := newPushRig(t)
		c2, bundle := p.change()
		id := randomID(t)
		p.h.hooks.fail = func(point string) error {
			if point == "in:sync" {
				return syscall.EIO
			}
			return nil
		}
		var polled *httptest.ResponseRecorder
		var retried *httptest.ResponseRecorder
		first := true
		p.clk.onSleep = func() {
			if !first {
				return
			}
			first = false
			// Before root: the bundle alone says the push was admitted,
			// and the id is known.
			polled = p.get(t, "/?result="+id, p.token(t, ""))
			retried = p.post(t, p.h, push{id: id, token: p.token(t, c2), body: failingReader{t}})
			must(t, p.run(hooks{}))
		}
		w := p.post(t, p.h, push{id: id, token: p.token(t, c2), body: bytes.NewReader(bundle)})
		wantResult(t, w, http.StatusOK, id, phaseApplied)
		if exists(p.x("stage/" + id + ".auth")) {
			t.Fatal("a marker was written beside a bundle whose in/ was not synced")
		}
		if m := decode(t, polled); polled.Code != http.StatusAccepted || m["phase"] != "admitted" {
			t.Fatalf("poll before root: %d %s", polled.Code, polled.Body)
		}
		wantError(t, retried, http.StatusConflict, msgDuplicate(id))
	})
	t.Run("the marker cannot be written", func(t *testing.T) {
		p := newPushRig(t)
		c2, bundle := p.change()
		id := randomID(t)
		p.h.hooks.fail = func(point string) error {
			if point == "marker" {
				return syscall.ENOSPC
			}
			return nil
		}
		p.clk.onSleep = func() { must(t, p.run(hooks{})) }
		w := p.post(t, p.h, push{id: id, token: p.token(t, c2), body: bytes.NewReader(bundle)})
		wantResult(t, w, http.StatusOK, id, phaseApplied)
		if len(p.logged(zap.ErrorLevel, "box webhook: the bundle is in in/, but its marker could not be written")) != 1 {
			t.Fatal("no line for the marker")
		}
	})
}

// postCrash serves a push whose hook panics, as a kill would end it.
func (p *pushRig) postCrash(t *testing.T, q push) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("no crash")
		} else if _, ok := r.(crashed); !ok {
			panic(r)
		}
	}()
	_ = p.h.ServeHTTP(httptest.NewRecorder(), p.request(q), nextFunc(func(http.ResponseWriter, *http.Request) error { return nil }))
}

// TestPushLockAcrossReload: the admission lock is a flock on a file, so
// a second handler instance — the one a reload provisions — is refused
// while the first admits, and admits once it is done.
func TestPushLockAcrossReload(t *testing.T) {
	p := newPushRig(t)
	c2, bundle := p.change()
	block := make(chan struct{})
	p.val.block, p.val.entered = block, make(chan struct{})
	first := make(chan *httptest.ResponseRecorder)
	go func() {
		first <- p.post(t, p.h, push{id: randomID(t), token: p.token(t, c2), body: bytes.NewReader(bundle)})
	}()
	<-p.val.entered
	// The handler a reload provisions shares the process's slot: refused
	// before its body is read.
	reloaded := p.handler()
	wantError(t, p.post(t, reloaded, push{id: randomID(t), token: p.token(t, c2), body: failingReader{t}}), http.StatusConflict, msgAdmitting)
	// One in another process has its own slot and meets the flock.
	other := p.handler()
	other.slot = make(chan struct{}, 1)
	wantError(t, p.post(t, other, push{id: randomID(t), token: p.token(t, c2), body: bytes.NewReader(bundle)}), http.StatusConflict, msgAdmitting)
	close(block)
	if w := <-first; w.Code != http.StatusGatewayTimeout {
		t.Fatalf("the first push: %d %s", w.Code, w.Body)
	}
}

// TestPushSweepsOwnTemporaries: what a crash of an earlier admission
// left in stage/ — dot-names only — is removed under the lock; root's
// markers are not the handler's to touch.
func TestPushSweepsOwnTemporaries(t *testing.T) {
	p := newPushRig(t)
	c2, bundle := p.change()
	stale := randomID(t)
	p.write(t, filepath.Join(p.x("stage"), bundleTemp(stale)), []byte("half"))
	p.write(t, filepath.Join(p.x("stage"), stagedConfig(stale)), []byte("half"))
	old := randomID(t)
	p.markerAt(old, p.clock.Now().Add(-time.Hour))
	id := randomID(t)
	p.post(t, p.h, push{id: id, token: p.token(t, c2), body: bytes.NewReader(bundle)})
	got := strings.Join(p.names(p.x("stage")), ",")
	if strings.Contains(got, stale) || !strings.Contains(got, old+".auth") {
		t.Fatalf("stage/ = %s", got)
	}
}

// TestResultPoll is the Handler contract's `GET /?result=<id>` row.
func TestResultPoll(t *testing.T) {
	p := newPushRig(t)
	id := randomID(t)
	for _, target := range []string{"/?", "/?result=", "/?result=" + strings.ToUpper(id), "/?result=" + id + "&result=" + id, "/?result=" + id + "&x=1", "/?x=" + id, "/?%ZZ", "/?result=" + id[:31]} {
		wantError(t, p.get(t, target, p.token(t, "")), http.StatusBadRequest, msgResultID)
	}
	wantError(t, p.get(t, "/?result="+id, ""), http.StatusUnauthorized, unauthorized)
	if p.h.limiter.Size() != 1 {
		t.Fatal("an unauthenticated poll was not charged")
	}
	wantError(t, p.get(t, "/?result="+id, p.token(t, "")), http.StatusNotFound, msgNoResult(id))

	p.markerAt(id, p.clock.Now().Add(-48*time.Hour)) // admitted, whatever its age
	w := p.get(t, "/?result="+id, p.token(t, ""))
	if m := decode(t, w); w.Code != http.StatusAccepted || m["phase"] != "admitted" || len(m) != 1 {
		t.Fatalf("admitted: %d %s", w.Code, w.Body)
	}
	for phase, code := range map[string]int{
		phaseVerified: http.StatusAccepted, phaseNoChange: http.StatusOK, phaseApplied: http.StatusOK,
		phaseRefused: http.StatusUnprocessableEntity, phaseFailed: http.StatusUnprocessableEntity,
		phaseRolledBack: http.StatusUnprocessableEntity, phaseUnknown: http.StatusUnprocessableEntity,
	} {
		p.writeResult(t, result{ID: id, Phase: phase, Commit: p.base, BoxWebhook: "deploy.example.com"})
		m := wantResult(t, p.get(t, "/?result="+id, p.token(t, "")), code, id, phase)
		if m["commit"] != p.base {
			t.Fatalf("%s: the commit was masked: %v", phase, m)
		}
	}
	// A refusal naming a commit other than its own: the ids in `named`
	// are held out of the filter as `commit` is.
	head := randomID(t) + randomID(t)[:8]
	msg := head + " does not descend from the commit this box runs (" + p.base + ")"
	p.writeResult(t, result{ID: id, Phase: phaseRefused, Commit: head, Error: msg, Named: []string{head, p.base}})
	if m := wantResult(t, p.get(t, "/?result="+id, p.token(t, "")), http.StatusUnprocessableEntity, id, phaseRefused); m["error"] != msg {
		t.Fatalf("a named commit was masked: %v", m)
	}
	for name, raw := range map[string][]byte{
		"not JSON":        []byte("{"),
		"another id":      result{ID: randomID(t), Phase: phaseApplied}.marshal(),
		"not a phase":     result{ID: id, Phase: phaseInstalling}.marshal(),
		"named not an id": result{ID: id, Phase: phaseRefused, Named: []string{"not-an-id"}}.marshal(),
		"named too many":  result{ID: id, Phase: phaseRefused, Named: slices.Repeat([]string{p.base}, maxNamed+1)}.marshal(),
		"over the cap":    bytes.Repeat([]byte(" "), maxResult+1),
		"not a file":      nil,
	} {
		path := filepath.Join(p.x("out"), id+".json")
		must(t, os.RemoveAll(path))
		if raw == nil {
			must(t, os.Mkdir(path, 0o755))
		} else {
			p.write(t, path, raw)
		}
		w := p.get(t, "/?result="+id, p.token(t, ""))
		if w.Code != http.StatusInternalServerError || !strings.HasPrefix(decode(t, w)["error"].(string), "could not read the result for "+id+": ") {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body)
		}
	}
}

// TestPushStalledUpload: an upload that stops arriving does not hold
// the push slot past its deadline — through a real server, since a
// recorder has no connection to put a deadline on.
func TestPushStalledUpload(t *testing.T) {
	p := newPushRig(t)
	p.h.upload = 300 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = p.h.ServeHTTP(w, r, nextFunc(func(http.ResponseWriter, *http.Request) error { return nil }))
	}))
	defer srv.Close()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close() //nolint:errcheck // a test connection
	c2, _ := p.change()
	if _, err := fmt.Fprintf(conn, "POST / HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\n%s: %s\r\nContent-Type: application/gzip\r\nContent-Length: 1000\r\n\r\npartial",
		p.token(t, c2), headerRequestID, randomID(t)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no answer to a stalled upload: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d", resp.StatusCode)
	}
	release, ok := p.h.takeSlot()
	if !ok {
		t.Fatal("the slot outlived the stalled upload")
	}
	release()
}

// TestPushTrickledUpload: a body that keeps arriving a byte at a time
// does not hold the push slot past the handler's bound — through a real
// server with Caddy's idle reader around the body, as Server.ServeHTTP
// installs it on v2.11.7 (its fields, its HandlerDone). That reader
// re-arms the connection's read deadline before every read, so the
// deadline the handler set bounds nothing past the first byte; the
// handler's own bound on the body does, within the bound and one idle
// window. Before it, this test saw no answer within its 10 s.
func TestPushTrickledUpload(t *testing.T) {
	p := newPushRig(t)
	p.h.upload = 300 * time.Millisecond
	// Twenty trickle intervals: a loaded runner's pause must not turn
	// the trickle into a stall, which the idle reader ends itself.
	const idle = time.Second
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := &caddyhttp.IdleTimeoutReader{
			ReadCloser: r.Body,
			Ctrl:       http.NewResponseController(w),
			Deadline:   caddyhttp.IdleDeadline{Start: time.Now(), Timeout: idle},
			Logger:     zap.NewNop(),
			// As the server sets it for an HTTP/1 request with a body.
			DrainDeadline: true,
		}
		defer body.HandlerDone()
		r.Body = body
		_ = p.h.ServeHTTP(w, r, nextFunc(func(http.ResponseWriter, *http.Request) error { return nil }))
	}))
	defer srv.Close()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close() //nolint:errcheck // a test connection
	c2, _ := p.change()
	start := time.Now()
	// 1000 bytes promised, a byte every 50 ms: whole after 50 s, long
	// after the client's own 10 s bound on the answer.
	if _, err := fmt.Fprintf(conn, "POST / HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\n%s: %s\r\nContent-Type: application/gzip\r\nContent-Length: 1000\r\n\r\npartial",
		p.token(t, c2), headerRequestID, randomID(t)); err != nil {
		t.Fatal(err)
	}
	trickled := make(chan struct{})
	go func() {
		defer close(trickled)
		for {
			time.Sleep(50 * time.Millisecond)
			if _, err := conn.Write([]byte("x")); err != nil {
				return // the server closed the connection: the answer is out
			}
		}
	}()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no answer to a trickled upload: %v", err)
	}
	elapsed := time.Since(start)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest || !strings.HasPrefix(string(b), `{"error":"could not read the bundle: the upload outlasted `) {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	release, ok := p.h.takeSlot()
	if !ok {
		t.Fatal("the slot outlived the trickled upload")
	}
	release()
	// The bound, one idle window — the read blocked past the bound
	// returns with the next byte, and the server's drain of the rest
	// ends at the deadline that read armed — and room for a loaded
	// runner.
	if limit := p.h.upload + idle + 2*time.Second; elapsed > limit {
		t.Fatalf("answered after %s, past %s", elapsed, limit)
	}
	_ = conn.Close()
	<-trickled
}

// TestPushStalledUploadUnderIdleReader: a stall under Caddy's idle
// reader ends with that reader's error at its idle deadline, after
// which it clears the connection's deadline (caddy#8103); before the
// answer's header goes out net/http drains what is left of the body,
// now with no deadline, so the answer never leaves while the client
// holds the connection open. The slot is given back all the same, by
// the idle deadline: the drain holds the connection, not the slot.
func TestPushStalledUploadUnderIdleReader(t *testing.T) {
	p := newPushRig(t)
	p.h.upload = 300 * time.Millisecond
	const idle = 300 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := &caddyhttp.IdleTimeoutReader{
			ReadCloser:    r.Body,
			Ctrl:          http.NewResponseController(w),
			Deadline:      caddyhttp.IdleDeadline{Start: time.Now(), Timeout: idle},
			Logger:        zap.NewNop(),
			DrainDeadline: true,
		}
		defer body.HandlerDone()
		r.Body = body
		_ = p.h.ServeHTTP(w, r, nextFunc(func(http.ResponseWriter, *http.Request) error { return nil }))
	}))
	defer srv.Close()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close() //nolint:errcheck // a test connection
	c2, _ := p.change()
	if _, err := fmt.Fprintf(conn, "POST / HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\n%s: %s\r\nContent-Type: application/gzip\r\nContent-Length: 1000\r\n\r\npartial",
		p.token(t, c2), headerRequestID, randomID(t)); err != nil {
		t.Fatal(err)
	}
	// The line is written after the slot is given back, by the idle
	// deadline, with room for a loaded runner; the answer is not waited
	// for, since none comes.
	deadline := time.Now().Add(idle + 2*time.Second)
	for len(p.logged(zap.WarnLevel, "box push upload failed")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no upload failed line by the idle deadline")
		}
		time.Sleep(20 * time.Millisecond)
	}
	release, ok := p.h.takeSlot()
	if !ok {
		t.Fatal("the slot is held with the drain")
	}
	release()
}

// TestPushDeadlineCleared: after a whole body the upload deadline no
// longer applies (net/http clears it as it starts its background read),
// so admission and the wait may outlast it — a deadline left standing
// there would end the request's context mid-validate.
func TestPushDeadlineCleared(t *testing.T) {
	p := newPushRig(t)
	p.h.upload = 300 * time.Millisecond
	block := make(chan struct{})
	p.val.block = block
	time.AfterFunc(900*time.Millisecond, func() { close(block) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = p.h.ServeHTTP(w, r, nextFunc(func(http.ResponseWriter, *http.Request) error { return nil }))
	}))
	defer srv.Close()
	c2, bundle := p.change()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/", bytes.NewReader(bundle))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+p.token(t, c2))
	req.Header.Set("Content-Type", "application/gzip")
	req.Header.Set(headerRequestID, randomID(t))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // a test response
	if resp.StatusCode != http.StatusGatewayTimeout {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
}

// TestPushSlotFreeDuringTheWait: the slot is held from the body to the
// drop and given back before step 8's wait, so the next push can begin
// while this one waits on root.
func TestPushSlotFreeDuringTheWait(t *testing.T) {
	p := newPushRig(t)
	c2, bundle := p.change()
	free := false
	p.clk.onSleep = func() {
		if release, ok := p.h.takeSlot(); ok {
			free = true
			release()
		}
	}
	if w := p.post(t, p.h, push{id: randomID(t), token: p.token(t, c2), body: bytes.NewReader(bundle)}); w.Code != http.StatusGatewayTimeout {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if !free {
		t.Fatal("the slot was held through the wait")
	}
	release, ok := p.h.takeSlot()
	if !ok {
		t.Fatal("the slot was not given back")
	}
	release()
}
