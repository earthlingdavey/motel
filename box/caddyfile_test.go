package box

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/smallhoursorg/motel/liveswap/deploytrust"
	"github.com/smallhoursorg/motel/liveswap/deploytrust/trusttest"
)

// optionJSON parses one `box` option the way the adapter does and
// returns the app's JSON.
func optionJSON(t *testing.T, in string) (string, error) {
	t.Helper()
	v, err := parseGlobalOption(caddyfile.NewTestDispenser(in), nil)
	if err != nil {
		return "", err
	}
	app, ok := v.(httpcaddyfile.App)
	if !ok || app.Name != "box" {
		t.Fatalf("parseGlobalOption returned %#v", v)
	}
	return string(app.Value), nil
}

func TestBoxOptionParses(t *testing.T) {
	for name, c := range map[string]struct{ in, want string }{
		"one source, two signers": {
			"box {\n\tdeploy_trust github {\n\t\taudience motel\n\t\tclaim repository your-org/boxes\n\t\tclaim ref refs/heads/main\n\t}\n\tsigner alice@example.com ssh-ed25519 K1\n\tsigner bob ssh-ed25519 K2\n}",
			`{"deploy_trust":[{"kind":"github","audience":"motel","claims":{"ref":"refs/heads/main","repository":"your-org/boxes"}}],"signers":[{"principal":"alice@example.com","type":"ssh-ed25519","key":"K1"},{"principal":"bob","type":"ssh-ed25519","key":"K2"}]}`,
		},
		"two sources in order, subject folded": {
			"box {\n\tdeploy_trust local {\n\t\tpublic_key /etc/motel/deploy.pub\n\t\taudience box1\n\t}\n\tsigner a ssh-ed25519 K1\n\tdeploy_trust gitlab {\n\t\taudience motel\n\t\tsubject project_path:g/p:ref_type:branch:ref:main\n\t}\n}",
			`{"deploy_trust":[{"kind":"local","audience":"box1","public_key":"/etc/motel/deploy.pub"},{"kind":"gitlab","audience":"motel","claims":{"sub":"project_path:g/p:ref_type:branch:ref:main"}}],"signers":[{"principal":"a","type":"ssh-ed25519","key":"K1"}]}`,
		},
		// An empty block parses; Provision refuses it (no signer, no
		// source), so `moteld validate` does.
		"empty": {"box {\n}", `{}`},
		"bare":  {"box", `{}`},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := optionJSON(t, string(file(c.in)))
			if err != nil {
				t.Fatal(err)
			}
			if want := string(file(c.want)); got != want {
				t.Errorf("got  %s\nwant %s", got, want)
			}
		})
	}
}

func TestBoxOptionRefuses(t *testing.T) {
	for name, c := range map[string]struct{ in, want string }{
		"argument":                   {"box x {\n}", "wrong argument count"},
		"unknown subdirective":       {"box {\n\tsigners a ssh-ed25519 K1\n}", `unknown box subdirective "signers"`},
		"signer two args":            {"box {\n\tsigner a K1\n}", "signer takes <principal> <key-type> <base64>"},
		"signer four args":           {"box {\n\tsigner a ssh-ed25519 K1 x\n}", "signer takes <principal> <key-type> <base64>"},
		"signer with a block":        {"box {\n\tsigner a ssh-ed25519 K1 {\n\t\tx\n\t}\n}", "signer takes"},
		"signer with an empty block": {"box {\n\tsigner a ssh-ed25519 K1 {\n\t}\n\tsigner b ssh-ed25519 K2\n}", "signer takes"},
		"signer bad principal":       {"box {\n\tsigner a*b ssh-ed25519 K1\n}", `signer principal "a*b" may only contain`},
		"signer bad type":            {"box {\n\tsigner a ssh-dss K1\n}", `key type "ssh-dss" is not one of`},
		"signer bad key":             {"box {\n\tsigner a ssh-ed25519 AAAA\n}", "the key does not parse"},
		"signer not base64":          {"box {\n\tsigner a ssh-ed25519 !!!!\n}", "the key is not base64"},
		"deploy_trust no kind":       {"box {\n\tdeploy_trust\n}", "deploy_trust needs a preset"},
		"deploy_trust unknown":       {"box {\n\tdeploy_trust github {\n\t\tbranch main\n\t}\n}", `unknown deploy_trust subdirective "branch"`},
		"deploy_trust dup claim":     {"box {\n\tdeploy_trust github {\n\t\tclaim a b\n\t\tclaim a c\n\t}\n}", `duplicate claim "a"`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := optionJSON(t, string(file(c.in)))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want an error containing %q", err, c.want)
			}
		})
	}
	if _, err := parseGlobalOption(caddyfile.NewTestDispenser("box {\n}"), httpcaddyfile.App{Name: "box"}); err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Errorf("a second box option: %v", err)
	}
	// Through the adapter, as `moteld validate` meets it.
	twice := strings.Replace(good, "\tbox {\n", "\tbox {\n\t}\n\tbox {\n", 1)
	if _, _, err := adapt(t, twice); err == nil || !strings.Contains(err.Error(), "the box option is given more than once") {
		t.Errorf("two box blocks adapted: %v", err)
	}
}

func TestWebhookDirective(t *testing.T) {
	for _, in := range []string{"box_webhook", "box_webhook {\n}"} {
		var h Handler
		if err := h.UnmarshalCaddyfile(caddyfile.NewTestDispenser(in)); err != nil {
			t.Errorf("%q: %v", in, err)
		}
	}
	for in, want := range map[string]string{
		"box_webhook /hook":           "takes no arguments and no matcher",
		"box_webhook @m":              "takes no arguments and no matcher",
		"box_webhook *":               "takes no arguments and no matcher",
		"box_webhook on":              "takes no arguments and no matcher",
		"box_webhook {\n\tpath /x\n}": "takes no block",
	} {
		var h Handler
		if err := h.UnmarshalCaddyfile(caddyfile.NewTestDispenser(in)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", in, err, want)
		}
	}
	// Through the adapter: a matcher is refused, not swallowed as one,
	// so a file cannot adapt with a webhook that leaves / unserved.
	for _, line := range []string{"box_webhook /hook", "box_webhook @m", "box_webhook *"} {
		in := strings.Replace(good, "\tbox_webhook\n", "\t@m path /\n\t"+line+"\n", 1)
		if _, _, err := adapt(t, in); err == nil || !strings.Contains(err.Error(), "takes no arguments and no matcher") {
			t.Errorf("%q adapted: %v", line, err)
		}
	}
}

// adapt runs the Caddyfile adapter on a template (file's K1/K2), as
// `moteld validate` does before it provisions.
func adapt(t testing.TB, in string) ([]byte, []caddyconfig.Warning, error) {
	t.Helper()
	return caddyconfig.GetAdapter("caddyfile").Adapt(file(in), map[string]any{"filename": "Caddyfile"})
}

// handlerOrder is the handler names of the site whose host is host, in
// the order Caddy runs them: the server routes' subroute, flattened.
func handlerOrder(t *testing.T, cfg []byte, host string) []string {
	t.Helper()
	var c struct {
		Apps struct {
			HTTP struct {
				Servers map[string]struct {
					Routes []json.RawMessage `json:"routes"`
				} `json:"servers"`
			} `json:"http"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		t.Fatal(err)
	}
	var out []string
	var walk func(routes []json.RawMessage)
	walk = func(routes []json.RawMessage) {
		for _, raw := range routes {
			var r struct {
				Handle []struct {
					Handler string            `json:"handler"`
					Routes  []json.RawMessage `json:"routes"`
				} `json:"handle"`
			}
			if err := json.Unmarshal(raw, &r); err != nil {
				t.Fatal(err)
			}
			for _, h := range r.Handle {
				if h.Handler == "subroute" {
					walk(h.Routes)
					continue
				}
				out = append(out, h.Handler)
			}
		}
	}
	for _, srv := range c.Apps.HTTP.Servers {
		for _, raw := range srv.Routes {
			var r struct {
				Match []struct {
					Host []string `json:"host"`
				} `json:"match"`
			}
			if err := json.Unmarshal(raw, &r); err != nil {
				t.Fatal(err)
			}
			if len(r.Match) == 1 && len(r.Match[0].Host) == 1 && r.Match[0].Host[0] == host {
				walk([]json.RawMessage{raw})
			}
		}
	}
	return out
}

// TestRouteOrder adapts the design's example Caddyfile (DESIGN-box.md,
// "The shape") and a site that mixes the webhook with the directives a
// box site might carry: box_webhook runs first, before
// liveswap_webhook, which is terminal on every path it sees.
func TestRouteOrder(t *testing.T) {
	for name, site := range map[string]string{
		"the design's example": "deploy.example.com {\n\tliveswap_webhook\n\tbox_webhook\n}\n",
		"mixed": "deploy.example.com {\n\tliveswap_webhook\n\trespond /x ok\n\thandle /h {\n\t\trespond h\n\t}\n" +
			"\troute /r {\n\t\trespond r\n\t}\n\tredir /old /new\n\treverse_proxy /p localhost:9\n\tbox_webhook\n}\n",
	} {
		t.Run(name, func(t *testing.T) {
			global := good[:strings.Index(good, "\n}\n")+3] // its nested blocks close indented
			in := global + "\nexample.com {\n\treverse_proxy {\n\t\tdynamic liveswap example\n\t}\n}\n\n" + site
			cfg, _, err := adapt(t, in)
			if err != nil {
				t.Fatal(err)
			}
			order := handlerOrder(t, cfg, "deploy.example.com")
			if len(order) < 2 || order[0] != "box_webhook" {
				t.Fatalf("box_webhook is not the site's first handler: %v", order)
			}
			if i := strings.Index(strings.Join(order, " "), "liveswap_webhook"); i < 0 {
				t.Fatalf("no liveswap_webhook: %v", order)
			}
		})
	}
}

// localTrust is a deploy_trust block on a fresh local key.
func localTrust(t *testing.T) deploytrust.TrustConfig {
	t.Helper()
	_, pub := trusttest.GenerateKey()
	return deploytrust.TrustConfig{Kind: "local", PublicKey: trusttest.KeyFile(t, pub), Audience: "box1"}
}

func TestAppProvision(t *testing.T) {
	tc := localTrust(t)
	signer := SignerConfig{Principal: "alice", Type: "ssh-ed25519", Key: keys[0]}
	for name, c := range map[string]struct {
		app  App
		want string
	}{
		"no signer":  {App{DeployTrust: []deploytrust.TrustConfig{tc}}, "box: no signer"},
		"bad signer": {App{DeployTrust: []deploytrust.TrustConfig{tc}, Signers: []SignerConfig{{Principal: "a b", Type: "ssh-ed25519", Key: keys[0]}}}, `box: signer principal "a b" may only contain`},
		"bad key":    {App{DeployTrust: []deploytrust.TrustConfig{tc}, Signers: []SignerConfig{{Principal: "a", Type: "ssh-ed25519", Key: "AAAA"}}}, "box: signer a: the key does not parse"},
		"same key":   {App{DeployTrust: []deploytrust.TrustConfig{tc}, Signers: []SignerConfig{signer, {Principal: "bob", Type: "ssh-ed25519", Key: keys[0]}}}, "box: signer alice and signer bob are the same key"},
		"no source":  {App{Signers: []SignerConfig{signer}}, "box: deploy_trust: no source configured"},
		"bad source": {App{Signers: []SignerConfig{signer}, DeployTrust: []deploytrust.TrustConfig{{Kind: "local", PublicKey: "/nonexistent.pub"}}}, "box: deploy_trust[0]:"},
	} {
		t.Run(name, func(t *testing.T) {
			err := c.app.provision(zap.NewNop())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
		})
	}

	a := App{DeployTrust: []deploytrust.TrustConfig{tc}, Signers: []SignerConfig{signer}}
	if err := a.provision(zap.NewNop()); err != nil || len(a.verifiers) != 1 {
		t.Fatalf("a good app: %v, %d verifiers", err, len(a.verifiers))
	}

	// A block wider than it reads is warned about, named as the box's,
	// and the config is left as written: the warning resolves its own
	// copy of the placeholders.
	core, logs := observer.New(zap.InfoLevel)
	t.Setenv("BOX_REPO", "your-org/boxes")
	wide := App{Signers: []SignerConfig{signer}, DeployTrust: []deploytrust.TrustConfig{
		tc,
		{Kind: "github", Audience: "motel", Claims: map[string]string{"repository": "{env.BOX_REPO}"}},
	}}
	if err := wide.provision(zap.New(core)); err != nil {
		t.Fatal(err)
	}
	got := logs.FilterMessageSnippet("pins no branch").All()
	if len(got) != 1 || got[0].ContextMap()["option"] != "box" || got[0].ContextMap()["deploy_trust"] != int64(1) {
		t.Errorf("want one branch warning for the box's second block: %v", logs.All())
	}
	if wide.DeployTrust[1].Claims["repository"] != "{env.BOX_REPO}" {
		t.Errorf("the config was resolved in place: %v", wide.DeployTrust[1].Claims)
	}
}

// TestValidate is `moteld validate` on a box file: the adapter, then
// Provision of every app — so a box_webhook without the box option is
// a config error, not a webhook that refuses every request.
func TestValidate(t *testing.T) {
	tc := localTrust(t)
	site := "deploy.example.com {\n\tbox_webhook\n}\n"
	withBox := "{\n\tbox {\n\t\tdeploy_trust local {\n\t\t\tpublic_key " + tc.PublicKey + "\n\t\t\taudience box1\n\t\t}\n\t\tsigner alice ssh-ed25519 K1\n\t}\n}\n" + site
	cfg, _, err := adapt(t, withBox)
	if err != nil {
		t.Fatal(err)
	}
	if err := validate(t, cfg); err != nil {
		t.Fatalf("a good box file: %v", err)
	}
	cfg, _, err = adapt(t, site)
	if err != nil {
		t.Fatal(err)
	}
	if err := validate(t, cfg); err == nil || !strings.Contains(err.Error(), "box_webhook needs the box global option") {
		t.Fatalf("box_webhook alone: %v", err)
	}
}

func validate(t *testing.T, cfg []byte) error {
	t.Helper()
	var c caddy.Config
	if err := json.Unmarshal(cfg, &c); err != nil {
		t.Fatal(err)
	}
	// No ACME, no storage of the user's: validate provisions the tls
	// app too.
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	return caddy.Validate(&c)
}
