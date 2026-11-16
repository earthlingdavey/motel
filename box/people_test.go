package box

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	_ "github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/smallhoursorg/motel/liveswap/access"
	"github.com/smallhoursorg/motel/liveswap/access/accesstest"
	"github.com/smallhoursorg/motel/liveswap/deploytrust/trusttest"
)

// The box of the access tests: two apps, a and b, each with its own
// deploy source and a canary value in its env_file; the box channel's
// source; and people. alice views a, bob views b, carol views the box,
// dave is declared with no grant, and ops is the signer (a person by its
// ed25519 key, with no grant). The same file is the installed one the
// registry is read from, with its site written as the walk reads a
// box's (a bare hostname), and the config the test provisions, with the
// site over plain http so that no certificate is involved.
const peopleIdentity = "deploy.example.test"

type peopleBox struct {
	t      testing.TB
	dir    string
	people map[string]accesstest.Key
	deploy map[string]ed25519.PrivateKey // "a", "b", "box"
	canary map[string]string
	extra  string // more box lines
	global string // more global options
	grants []string
	file   string // the installed Caddyfile
	ctx    caddy.Context
	server *caddyhttp.Server
}

func newPeopleBox(t testing.TB) *peopleBox {
	b := &peopleBox{
		t: t, dir: t.TempDir(),
		people: map[string]accesstest.Key{
			"alice@example.test": accesstest.Ed25519(),
			"bob@example.test":   accesstest.SKEd25519(),
			"carol@example.test": accesstest.Ed25519(),
			"dave@example.test":  accesstest.Ed25519(),
			"ops@example.test":   accesstest.Ed25519(),
		},
		deploy: map[string]ed25519.PrivateKey{},
		canary: map[string]string{"a": "canary-of-app-a-7f3e9d", "b": "canary-of-app-b-41c8aa"},
		grants: []string{
			"grant alice@example.test viewer app a",
			"grant bob@example.test viewer app b",
			"grant carol@example.test viewer box",
		},
	}
	for _, src := range []string{"a", "b", "box"} {
		priv, _ := trusttest.GenerateKey()
		b.deploy[src] = priv
	}
	return b
}

// caddyfile is the box's file, its webhook site at site.
func (b *peopleBox) caddyfile(site string) string {
	t := b.t
	pub := func(src string) string {
		return trusttest.KeyFile(t, b.deploy[src].Public().(ed25519.PublicKey))
	}
	var lines []string
	for name, k := range b.people {
		if name == "ops@example.test" {
			lines = append(lines, fmt.Sprintf("signer %s %s %s", name, k.Type(), k.B64()))
			continue
		}
		lines = append(lines, k.Line(name))
	}
	lines = append(lines, b.grants...)
	if b.extra != "" {
		lines = append(lines, b.extra)
	}
	app := func(name string) string {
		env := filepath.Join(b.dir, name+".env")
		if err := os.WriteFile(env, []byte("SECRET_TOKEN="+b.canary[name]+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf(`		app %s {
			command ./%s
			env_file %s
			deploy_trust local {
				public_key %s
				audience %s
			}
		}
`, name, name, env, pub(name), name)
	}
	return fmt.Sprintf(`{
	admin off
	log {
		output discard
	}
%s
	box {
		deploy_trust local {
			public_key %s
			audience box
		}
		%s
	}
	liveswap {
		root %s
		artifact_allowlist artifacts.example.test/
%s%s	}
}

%s {
	liveswap_webhook
	box_webhook
}
`, b.global, pub("box"), strings.Join(lines, "\n\t\t"), filepath.Join(b.dir, "liveswap"), app("a"), app("b"), site)
}

// install writes the installed file and points the registry at it.
func (b *peopleBox) install() {
	b.file = filepath.Join(b.dir, "Caddyfile")
	if err := os.WriteFile(b.file, []byte(b.caddyfile(peopleIdentity)), 0o644); err != nil {
		b.t.Fatal(err)
	}
	setRegistryFile(b.t, b.file)
}

// load adapts and provisions the config, as a start or a reload does up
// to the point where the apps start, and keeps its server. No app is
// started: a request served now is one served in the moments after a
// start, before liveswap's Start installs the apps (issue #473).
func (b *peopleBox) load() {
	t := b.t
	t.Helper()
	cfgJSON, _, err := caddyconfig.GetAdapter("caddyfile").Adapt([]byte(b.caddyfile("http://"+peopleIdentity)), map[string]any{"filename": "Caddyfile"})
	if err != nil {
		t.Fatal(err)
	}
	var cfg caddy.Config
	if err := json.Unmarshal(cfgJSON, &cfg); err != nil {
		t.Fatal(err)
	}
	ctx, err := caddy.ProvisionContext(&cfg)
	if err != nil {
		t.Fatalf("provisioning the box's config: %v", err)
	}
	b.ctx = ctx
	httpApp, err := ctx.App("http")
	if err != nil {
		t.Fatal(err)
	}
	for _, srv := range httpApp.(*caddyhttp.App).Servers {
		b.server = srv
	}
	if b.server == nil {
		t.Fatal("no server")
	}
}

// registry is the box app's, as this load built it.
func (b *peopleBox) registry() *access.Registry {
	b.t.Helper()
	app, err := b.ctx.App("box")
	if err != nil {
		b.t.Fatal(err)
	}
	return app.(access.Source).Access()
}

// get serves one GET through the whole server, as Caddy would.
func (b *peopleBox) get(target, auth string) *httptest.ResponseRecorder {
	return b.getFrom(target, auth, "", "")
}

// getFrom is get from the peer remote, with an X-Forwarded-For.
func (b *peopleBox) getFrom(target, auth, remote, forwarded string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "http://"+peopleIdentity+target, nil)
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	if remote != "" {
		r.RemoteAddr = remote
	}
	if forwarded != "" {
		r.Header.Set("X-Forwarded-For", forwarded)
	}
	w := httptest.NewRecorder()
	b.server.ServeHTTP(w, r)
	return w
}

// setRegistryFile points the box app's registry at path until the test
// ends.
func setRegistryFile(t testing.TB, path string) {
	saved := registryPath.Load()
	registryPath.Store(&path)
	t.Cleanup(func() { registryPath.Store(saved) })
}

// TestLiveswapReachesTheRegistry is the first of the two checks the
// design never tried: the box app builds the registry at provision, and
// liveswap_webhook, provisioned in the same config, reaches it through
// ctx.AppIfConfigured("box") and access.Source with no import of box
// and no cycle at provision, whichever app Caddy provisions first.
func TestLiveswapReachesTheRegistry(t *testing.T) {
	for range 4 { // Caddy provisions the apps in map order
		b := newPeopleBox(t)
		b.install()
		b.load()
		reg := b.registry()
		if reg == nil || reg.Identity() != peopleIdentity || reg.Keys() != len(b.people) {
			t.Fatalf("registry: %v keys, identity %q", reg.Keys(), reg.Identity())
		}
		for name, k := range b.people {
			if got, ok := reg.Name(k.Fingerprint()); !ok || got != name {
				t.Errorf("%s's key names %q", name, got)
			}
		}
	}
}

// TestRegistryAtLoad: the registry is read from the installed file at
// each load, and every line it writes names that file as the one this
// machine runs (a validate of a candidate reads it too). No file, an
// unreadable one, or one the walk refuses admits no one, with a line
// saying why: a warning when the config declares people, else
// information, and only debug for a config that declares no one on a
// machine with no installed file; a block with signers alone
// admits no one and says nothing; and a reload reads the file again, so
// a person whose lines a push removed is not in the next registry.
func TestRegistryAtLoad(t *testing.T) {
	b := newPeopleBox(t)
	good := []byte(b.caddyfile(peopleIdentity))
	declaring := &App{People: []PersonConfig{{Name: "x"}}}
	for _, c := range []struct {
		name  string
		file  []byte // nil: no file
		app   *App
		level zapcore.Level
		want  string
	}{
		{"no installed file", nil, declaring, zapcore.WarnLevel, "there is no such file"},
		{"no installed file, no people in the config", nil, &App{}, zapcore.DebugLevel, "there is no such file"},
		{"a file the walk refuses", []byte("import x\n"), declaring, zapcore.WarnLevel, "not one the box can speak for"},
		{"a file the walk refuses, no people in the config", []byte("import x\n"), &App{}, zapcore.InfoLevel, "not one the box can speak for"},
	} {
		path := filepath.Join(t.TempDir(), "Caddyfile")
		if c.file != nil {
			if err := os.WriteFile(path, c.file, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		core, logs := observer.New(zapcore.DebugLevel)
		if reg := c.app.loadRegistry(zap.New(core), path); reg != nil {
			t.Errorf("%s: a registry of %d keys", c.name, reg.Keys())
		}
		got := logs.FilterMessageSnippet("no person is admitted from the Caddyfile this machine runs (" + path + ")").All()
		if len(got) != 1 || got[0].Level != c.level || !strings.Contains(got[0].Message, c.want) || got[0].ContextMap()["file"] != path {
			t.Errorf("%s: journal %v, want one %s line saying %q", c.name, logs.All(), c.level, c.want)
		}
	}

	path := filepath.Join(t.TempDir(), "Caddyfile")
	write := func(b []byte) {
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	core, logs := observer.New(zapcore.DebugLevel)
	write(good)
	reg := declaring.loadRegistry(zap.New(core), path)
	if reg.Keys() != len(b.people) || logs.FilterMessage("box: people admitted from the Caddyfile this machine runs ("+path+")").Len() != 1 ||
		logs.FilterMessageSnippet("the Caddyfile this machine runs ("+path+"): people are declared and no monitor channel is").Len() != 1 {
		t.Fatalf("the good file: %d keys; journal %v", reg.Keys(), logs.All())
	}

	// A push removes alice's lines; the reload's registry has no alice.
	alice := b.people["alice@example.test"]
	delete(b.people, "alice@example.test")
	b.grants = b.grants[1:]
	write([]byte(b.caddyfile(peopleIdentity)))
	reg = declaring.loadRegistry(zap.New(core), path)
	if _, ok := reg.Lookup(alice.Fingerprint()); ok || reg.Keys() != len(b.people) {
		t.Errorf("after alice's lines are removed: alice listed %v, %d keys", ok, reg.Keys())
	}

	// A block of signers alone declares no one.
	b.people = map[string]accesstest.Key{"ops@example.test": b.people["ops@example.test"]}
	b.grants = nil
	write([]byte(b.caddyfile(peopleIdentity)))
	logs.TakeAll()
	if reg := (&App{}).loadRegistry(zap.New(core), path); reg != nil {
		t.Errorf("a block of signers alone: a registry of %d keys", reg.Keys())
	}
	if logs.Len() != 0 {
		t.Errorf("a block of signers alone: journal %v", logs.All())
	}
}

// TestPersonRefusalsAreTheFlat401: on the box webhook too, every way a
// person's token is refused is answered exactly as a request with no
// token is, on each route, a box with no people included; the 405 of
// another method comes before any of it, as for a deploy token.
func TestPersonRefusalsAreTheFlat401(t *testing.T) {
	alice, stranger := accesstest.Ed25519(), accesstest.Ed25519()
	k, err := access.ParsePerson([]string{"alice", alice.Type(), alice.B64()})
	if err != nil {
		t.Fatal(err)
	}
	g, err := access.ParseGrant([]string{"alice", "viewer", "box"})
	if err != nil {
		t.Fatal(err)
	}
	reg := access.NewRegistry(peopleIdentity, access.Lines{Keys: []access.Key{k}, Grants: []access.Grant{g}})
	now := time.Now()
	for _, c := range []struct {
		name       string
		auth       string
		noRegistry bool
	}{
		{"no registry", alice.Token(peopleIdentity, now, time.Minute), true},
		{"a key no line lists", stranger.Token(peopleIdentity, now, time.Minute), false},
		{"for another box", alice.Token("other.example.test", now, time.Minute), false},
		{"expired", alice.Token(peopleIdentity, now.Add(-time.Hour), time.Minute), false},
		{"garbage", "Motel m1.x.y", false},
	} {
		for _, target := range []string{"/", "/?result=" + strings.Repeat("0", 32)} {
			for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut} {
				r := newRig(t)
				if !c.noRegistry {
					r.h.app.registry = reg
				}
				do := func(auth string) *httptest.ResponseRecorder {
					hr := httptest.NewRequest(method, "http://"+peopleIdentity+target, nil)
					if auth != "" {
						hr.Header.Set("Authorization", auth)
					}
					w := httptest.NewRecorder()
					if err := r.h.ServeHTTP(w, hr, nextFunc(func(http.ResponseWriter, *http.Request) error { return nil })); err != nil {
						t.Fatal(err)
					}
					return w
				}
				want, got := do(""), do(c.auth)
				if got.Code != want.Code || got.Body.String() != want.Body.String() || fmt.Sprint(got.Header()) != fmt.Sprint(want.Header()) {
					t.Errorf("%s, %s %s: %d %v %q, want %d %v %q", c.name, method, target, got.Code, got.Header(), got.Body, want.Code, want.Header(), want.Body)
				}
			}
		}
	}
}

// TestPersonOnTheBoxWebhook: a viewer of the box reads the box's
// status, through the row that answers it; a person whose grants are
// on apps alone is told the viewer role on the box; a push's result,
// which only the box's own source reads, names the deployer.
func TestPersonOnTheBoxWebhook(t *testing.T) {
	carol, alice := accesstest.Ed25519(), accesstest.SKEd25519()
	var l access.Lines
	for name, k := range map[string]accesstest.Key{"carol": carol, "alice": alice} {
		key, err := access.ParsePerson([]string{name, k.Type(), k.B64()})
		if err != nil {
			t.Fatal(err)
		}
		l.Keys = append(l.Keys, key)
	}
	for _, args := range [][]string{{"carol", "viewer", "box"}, {"alice", "viewer", "app", "a"}} {
		g, err := access.ParseGrant(args)
		if err != nil {
			t.Fatal(err)
		}
		l.Grants = append(l.Grants, g)
	}
	reg := access.NewRegistry(peopleIdentity, l)
	served := recordRows(t)
	now := time.Now()
	for _, c := range []struct {
		who    accesstest.Key
		name   string
		target string
		code   int
		body   string
	}{
		{carol, "carol", "/", http.StatusOK, ""},
		{alice, "alice", "/", http.StatusForbidden, `{"error":"this needs the viewer role on the box","needs":"viewer","scope":"box"}` + "\n"},
		{carol, "carol", "/?result=" + strings.Repeat("0", 32), http.StatusForbidden, `{"error":"this needs a deploy token of the box","needs":"deployer","scope":"box"}` + "\n"},
	} {
		r := newRig(t)
		r.h.app.registry = reg
		*served = nil
		hr := httptest.NewRequest(http.MethodGet, "http://"+peopleIdentity+c.target, nil)
		hr.Header.Set("Authorization", c.who.Token(peopleIdentity, now, time.Minute))
		w := httptest.NewRecorder()
		if err := r.h.ServeHTTP(w, hr, nextFunc(func(http.ResponseWriter, *http.Request) error { return nil })); err != nil {
			t.Fatal(err)
		}
		if w.Code != c.code || (c.body != "" && w.Body.String() != c.body) {
			t.Errorf("%s GET %s: %d %q, want %d %q", c.name, c.target, w.Code, w.Body, c.code, c.body)
		}
		if (c.code == http.StatusOK) != (*served != nil) {
			t.Errorf("%s GET %s: answered by row %v", c.name, c.target, *served)
		}
	}
}

// TestPersonNotRefusedAfterAStart is the second check the design never
// tried: issue #473's window. A config is provisioned and no app has
// started, which is where a start leaves the server when Caddy starts
// the http app before liveswap. A deploy token of app a is not refused
// then: it verifies against the sources the config built, and waits for
// liveswap's Start to install a (here it never does, so a request that
// ends first is told to come back); a stranger is the flat 401 at once.
// A person's token is answered at once, as after the start, because
// the registry is built at provision and the person's way in reads
// nothing liveswap's Start installs.
func TestPersonNotRefusedAfterAStart(t *testing.T) {
	b := newPeopleBox(t)
	b.install()
	b.load()
	now := time.Now()
	alice, carol := b.people["alice@example.test"], b.people["carol@example.test"]
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	r := httptest.NewRequestWithContext(ctx, http.MethodGet, "http://"+peopleIdentity+"/a", nil)
	r.Header.Set("Authorization", "Bearer "+trusttest.Mint(t, b.deploy["a"], "a", nil))
	w := httptest.NewRecorder()
	b.server.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("a deploy token of a before liveswap starts: %d %v %s, want the 503 with Retry-After", w.Code, w.Header(), w.Body)
	}
	began := time.Now()
	if w := b.get("/a", "Bearer not-a-jwt"); w.Code != http.StatusUnauthorized || time.Since(began) > time.Second {
		t.Errorf("a stranger before liveswap starts: %d %s in %v", w.Code, w.Body, time.Since(began))
	}
	for _, c := range []struct {
		who          accesstest.Key
		name, target string
		code         int
	}{
		{alice, "alice", "/_motel/whoami", http.StatusOK},
		{alice, "alice", "/a", http.StatusOK},
		{alice, "alice", "/_motel/apps", http.StatusOK},
		{carol, "carol", "/b", http.StatusOK},
		{carol, "carol", "/", 0}, // the box's status, read from the box's own paths: whatever they hold
	} {
		w := b.get(c.target, c.who.Token(peopleIdentity, now, time.Minute))
		if w.Code != c.code && (c.code != 0 || w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden) {
			t.Errorf("%s GET %s at once: %d %s, want %d", c.name, c.target, w.Code, w.Body, c.code)
		}
		if w.Header().Get("Motel-Protocol") != "1" {
			t.Errorf("%s GET %s: no Motel-Protocol: %v", c.name, c.target, w.Header())
		}
	}
}

// TestGrantOnAnAppNotRunRefusedAtLoad: a grant on an app the config's
// liveswap block does not run fails the load, and so `motel validate`,
// as the walk refuses it: the box app asks the liveswap app which apps
// it runs. A config with no liveswap block runs none.
func TestGrantOnAnAppNotRunRefusedAtLoad(t *testing.T) {
	b := newPeopleBox(t)
	b.install()
	b.grants = append(b.grants, "grant alice@example.test viewer app shop")
	cfgJSON, _, err := caddyconfig.GetAdapter("caddyfile").Adapt([]byte(b.caddyfile("http://"+peopleIdentity)), map[string]any{"filename": "Caddyfile"})
	if err != nil {
		t.Fatal(err)
	}
	var cfg caddy.Config
	if err := json.Unmarshal(cfgJSON, &cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := caddy.ProvisionContext(&cfg); err == nil || !strings.Contains(err.Error(), "grant alice@example.test viewer app shop: the liveswap block runs no app shop") {
		t.Errorf("a grant on an app not run: %v", err)
	}
	delete(cfg.AppsRaw, "liveswap")
	delete(cfg.AppsRaw, "http")
	if _, err := caddy.ProvisionContext(&cfg); err == nil || !strings.Contains(err.Error(), "viewer app a: the liveswap block runs no app a") {
		t.Errorf("no liveswap block: %v", err)
	}
}

// TestTwoAppsTwoPeopleOnTheBoxWebhook is the two-apps-two-people test's
// half on box_webhook (liveswap's TestTwoAppsTwoPeople has the apps'
// routes): every row of its route table, for every kind of caller, has
// the status the action table gives, and a person's 200 names no app.
// A row whose journal name the test does not know fails it.
func TestTwoAppsTwoPeopleOnTheBoxWebhook(t *testing.T) {
	b := newPeopleBox(t)
	var l access.Lines
	for name, k := range b.people {
		var key access.Key
		var err error
		if name == "ops@example.test" {
			key, err = access.ParsePerson([]string{name, k.Type(), k.B64()})
			key = access.SignerKey(key.Name, key.Type, key.B64, key.Wire)
		} else {
			key, err = access.ParsePerson([]string{name, k.Type(), k.B64()})
		}
		if err != nil {
			t.Fatal(err)
		}
		l.Keys = append(l.Keys, key)
	}
	for _, line := range b.grants {
		g, err := access.ParseGrant(strings.Fields(line)[1:])
		if err != nil {
			t.Fatal(err)
		}
		l.Grants = append(l.Grants, g)
	}
	reg := access.NewRegistry(peopleIdentity, l)
	stranger := accesstest.Ed25519()
	now := time.Now()
	callers := []string{"alice@example.test", "bob@example.test", "carol@example.test", "dave@example.test", "ops@example.test",
		"a key no line lists", "deployer of a", "deployer of b", "the box channel", "no token"}
	for _, rt := range routes {
		for _, caller := range callers {
			r := newRig(t)
			r.h.app.registry = reg
			r.applied(t, strings.Repeat("c", 40))
			q := req{method: rt.method, target: "http://" + peopleIdentity + "/"}
			switch rt.journal {
			case "status":
			case "result":
				q.target += "?result=" + strings.Repeat("0", 32)
			case "push":
				q.body = strings.NewReader("not a bundle")
			default:
				t.Fatalf("the row %s / (%s) has no request in this test: write its expectations", rt.method, rt.journal)
			}
			switch caller {
			case "a key no line lists":
				q.header = http.Header{"Authorization": {stranger.Token(peopleIdentity, now, time.Minute)}}
			case "deployer of a", "deployer of b":
				q.header = http.Header{"Authorization": {"Bearer " + trusttest.Mint(t, b.deploy[strings.TrimPrefix(caller, "deployer of ")], "box", nil)}}
			case "the box channel":
				q.token = r.token(t)
			case "no token":
			default:
				q.header = http.Header{"Authorization": {b.people[caller].Token(peopleIdentity, now, time.Minute)}}
			}
			w := r.do(t, q)
			_, person := b.people[caller]
			want := http.StatusUnauthorized
			switch {
			case caller == "the box channel":
				want = 0 // the row's own answer, whatever it is
			case person && rt.method != http.MethodGet:
			case person && caller == "carol@example.test" && rt.action == access.BoxStatus:
				want = http.StatusOK
			case person:
				want = http.StatusForbidden
			}
			switch {
			case want == 0 && (w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden):
				t.Errorf("%s, %s / (%s): %d %s", caller, rt.method, rt.journal, w.Code, w.Body)
			case want != 0 && w.Code != want:
				t.Errorf("%s, %s / (%s): %d %s, want %d", caller, rt.method, rt.journal, w.Code, w.Body, want)
			}
			if w.Code == http.StatusOK && person {
				for _, s := range []string{`"a"`, `"b"`, b.canary["a"], b.canary["b"]} {
					if strings.Contains(w.Body.String(), s) {
						t.Errorf("%s, %s / (%s): the 200 holds %q: %s", caller, rt.method, rt.journal, s, w.Body)
					}
				}
			}
		}
	}
}

// TestRemovedPersonRefusedAfterReload: a push that removes a person's
// lines is applied by a reload, which reads the installed file again;
// from then on that person's token is the flat 401, and everyone else's
// is answered as before.
func TestRemovedPersonRefusedAfterReload(t *testing.T) {
	b := newPeopleBox(t)
	b.install()
	b.load()
	alice, carol := b.people["alice@example.test"], b.people["carol@example.test"]
	now := time.Now()
	if w := b.get("/_motel/whoami", alice.Token(peopleIdentity, now, time.Minute)); w.Code != http.StatusOK {
		t.Fatalf("alice before: %d %s", w.Code, w.Body)
	}
	delete(b.people, "alice@example.test")
	b.grants = b.grants[1:]
	b.install()
	b.load()
	stranger := b.get("/_motel/whoami", "")
	if w := b.get("/_motel/whoami", alice.Token(peopleIdentity, now, time.Minute)); w.Code != http.StatusUnauthorized || w.Body.String() != stranger.Body.String() {
		t.Errorf("alice after her lines are removed: %d %s", w.Code, w.Body)
	}
	if w := b.get("/b", carol.Token(peopleIdentity, now, time.Minute)); w.Code != http.StatusOK {
		t.Errorf("carol after the reload: %d %s", w.Code, w.Body)
	}
}

// TestFromBehindATrustedProxy drives a request with a forged
// X-Forwarded-For through a provisioned server whose trusted_proxies
// names the peer. The client writes an address on alice's list; the
// proxy appends the client's real one, on bob's. Without
// trusted_proxies_strict, Caddy's client_ip is the client's choice, so
// no from list holds a forwarded request at all: both are refused (and
// motel validate refuses such a config). With it, client_ip is the
// right-most address outside the ranges, the real one: bob reads, alice
// does not. A request from a peer that is no proxy is judged by the
// peer in either mode, whatever header it writes.
func TestFromBehindATrustedProxy(t *testing.T) {
	for _, strict := range []bool{false, true} {
		b := newPeopleBox(t)
		b.global = "\tservers {\n\t\ttrusted_proxies static 192.0.2.0/24\n"
		if strict {
			b.global += "\t\ttrusted_proxies_strict\n"
		}
		b.global += "\t}"
		b.grants = []string{
			"grant alice@example.test viewer app a from 198.51.100.0/24",
			"grant bob@example.test viewer app b from 203.0.113.0/24",
		}
		b.install()
		b.load()
		now := time.Now()
		alice := b.people["alice@example.test"].Token(peopleIdentity, now, time.Minute)
		bob := b.people["bob@example.test"].Token(peopleIdentity, now, time.Minute)
		const proxy, forged = "192.0.2.1:4321", "198.51.100.7, 203.0.113.9"
		bobCode := http.StatusUnauthorized
		if strict {
			bobCode = http.StatusOK
		}
		for _, c := range []struct {
			name, target, auth, remote, forwarded string
			code                                  int
		}{
			{"alice, forged through the proxy", "/a", alice, proxy, forged, http.StatusUnauthorized},
			{"bob, the real address through the proxy", "/b", bob, proxy, forged, bobCode},
			{"alice, from her network, no proxy", "/a", alice, "198.51.100.7:1", "", http.StatusOK},
			{"alice's header from elsewhere, no proxy", "/a", alice, "203.0.113.9:1", "198.51.100.7", http.StatusUnauthorized},
		} {
			if w := b.getFrom(c.target, c.auth, c.remote, c.forwarded); w.Code != c.code {
				t.Errorf("strict %v, %s: %d %s, want %d", strict, c.name, w.Code, w.Body, c.code)
			}
		}
	}
}
