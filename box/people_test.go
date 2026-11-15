package box

import (
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
	t       testing.TB
	dir     string
	people  map[string]accesstest.Key
	deploy  map[string]ed25519.PrivateKey // "a", "b", "box"
	canary  map[string]string
	extra   string // more box lines
	grants  []string
	file    string // the installed Caddyfile
	ctx     caddy.Context
	server  *caddyhttp.Server
}

func newPeopleBox(t testing.TB) *peopleBox {
	b := &peopleBox{
		t: t, dir: t.TempDir(),
		people: map[string]accesstest.Key{
			"alice@example.test": accesstest.Ed25519(),
			"bob@example.test":   accesstest.ECDSA(),
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
`, pub("box"), strings.Join(lines, "\n\t\t"), filepath.Join(b.dir, "liveswap"), app("a"), app("b"), site)
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
// start, before liveswap's Start installs the apps' deploy sources
// (issue #473).
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

// do serves one request through the whole server, as Caddy would.
func (b *peopleBox) do(method, target, auth, remote string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://"+peopleIdentity+target, nil)
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	if remote != "" {
		r.RemoteAddr = remote
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
// each load. No file, an unreadable one, or one the walk refuses admits
// no one, with a journal line saying why (a warning when the config
// declares people, else information); a block with signers alone
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
		{"no installed file", nil, declaring, zapcore.WarnLevel, "the box runs no installed Caddyfile"},
		{"no installed file, no people in the config", nil, &App{}, zapcore.InfoLevel, "the box runs no installed Caddyfile"},
		{"a file the walk refuses", []byte("import x\n"), declaring, zapcore.WarnLevel, "not one the box can speak for"},
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
		got := logs.FilterMessageSnippet("no person is admitted").All()
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
	if reg.Keys() != len(b.people) || logs.FilterMessage("box: people admitted").Len() != 1 {
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
