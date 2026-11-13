package notify

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

// fakeSource is a deploy_trust source as notify reads one.
type fakeSource struct {
	label  string
	claims map[string]string
}

func (f fakeSource) Label() string { return f.label }
func (f fakeSource) PinnedClaim(n string) (string, bool) {
	v, ok := f.claims[n]
	return v, ok
}

const ghIssuer = "oidc:https://token.actions.githubusercontent.com"

func testTrust() map[string][]TrustSource {
	return map[string][]TrustSource{
		"blog": {fakeSource{label: "local:/etc/motel/alice.pub"}},
		"shop": {
			fakeSource{label: ghIssuer, claims: map[string]string{"repository": "org/shop"}},
			fakeSource{label: "local:/etc/motel/bob.pub", claims: map[string]string{"sub": "bob"}},
		},
		"wiki": {fakeSource{label: "local:/etc/motel/alice.pub"}},
	}
}

func testSecrets() map[string]string {
	return map[string]string{
		"NOTIFY_OPS_URL":   "https://hooks.slack.com/services/T000/B000/XXXX", // gitleaks:allow
		"NOTIFY_PAGER_URL": "https://ntfy.sh/pager-topic",
		"NOTIFY_ALICE_URL": "https://discord.com/api/webhooks/1/abc", // gitleaks:allow
		"NOTIFY_BOB_URL":   "https://example.org/hook",
		"NOTIFY_HC_URL":    "https://hc-ping.com/1b4e28ba-2fa1-11d2-883f-0016d3cca427",
	}
}

func parse(t *testing.T, src string) (*App, error) {
	t.Helper()
	d := caddyfile.NewTestDispenser(src)
	a := new(App)
	return a, a.UnmarshalCaddyfile(d)
}

func mustParse(t *testing.T, src string) *App {
	t.Helper()
	a, err := parse(t, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return a
}

const fullBlock = `monitor {
	channel ops slack {secret.NOTIFY_OPS_URL}
	channel pager ntfy {secret.NOTIFY_PAGER_URL} severity critical
	channel alice discord {secret.NOTIFY_ALICE_URL}
	channel bob json {secret.NOTIFY_BOB_URL} severity warning
	operator ops pager
	bind local:/etc/motel/alice.pub alice
	bind "oidc:https://token.actions.githubusercontent.com repository=org/shop" bob
	heartbeat {secret.NOTIFY_HC_URL}
	deploy_succeeded on
}`

func TestCaddyfileFullBlock(t *testing.T) {
	a := mustParse(t, fullBlock)
	got, _ := json.Marshal(a)
	want := `{"channels":[{"name":"ops","kind":"slack","secret":"NOTIFY_OPS_URL","severity":"info"},` +
		`{"name":"pager","kind":"ntfy","secret":"NOTIFY_PAGER_URL","severity":"critical"},` +
		`{"name":"alice","kind":"discord","secret":"NOTIFY_ALICE_URL","severity":"info"},` +
		`{"name":"bob","kind":"json","secret":"NOTIFY_BOB_URL","severity":"warning"}],` +
		`"operator":["ops","pager"],` +
		`"binds":[{"identity":"local:/etc/motel/alice.pub","channel":"alice"},` +
		`{"identity":"oidc:https://token.actions.githubusercontent.com repository=org/shop","channel":"bob"}],` +
		`"heartbeat_secret":"NOTIFY_HC_URL","deploy_succeeded":true}`
	if string(got) != want {
		t.Fatalf("JSON:\n got %s\nwant %s", got, want)
	}
}

func TestCaddyfileRefusals(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"literal URL", `channel ops slack https://hooks.slack.com/x`, "never in the Caddyfile"},
		{"env placeholder", `channel ops slack {env.NOTIFY_OPS_URL}`, "never in the Caddyfile"},
		{"secret without prefix", `channel ops slack {secret.OPS_URL}`, "NOTIFY_"},
		{"secret lowercase", `channel ops slack {secret.NOTIFY_ops}`, "NOTIFY_"},
		{"unknown kind", `channel ops teams {secret.NOTIFY_OPS_URL}`, "is not one of json, ntfy, slack, discord"},
		{"bad name", `channel Ops slack {secret.NOTIFY_OPS_URL}`, "lowercase letters"},
		{"bad severity", `channel ops slack {secret.NOTIFY_OPS_URL} severity page`, "not critical, warning or info"},
		{"option after URL", `channel ops slack {secret.NOTIFY_OPS_URL} floor critical`, "only severity follows"},
		{"too few", `channel ops slack`, "channel takes"},
		{"empty operator", `operator`, "operator names one or more channels"},
		{"bind one arg", `bind ops`, "bind takes"},
		{"bind no prefix", `bind alice ops`, "an identity starts local:/"},
		{"bind oidc no claim", `bind oidc:https://token.actions.githubusercontent.com ops`, "at least one claim"},
		{"bind bad claim", `bind "local:/k.pub sub" ops`, "is not claim=value"},
		{"bind dup claim", `bind "local:/k.pub sub=a sub=b" ops`, "given twice"},
		{"heartbeat literal", `heartbeat https://hc-ping.com/x`, "never in the Caddyfile"},
		{"deploy_succeeded yes", `deploy_succeeded yes`, "on or off"},
		{"secrets_file empty", `secrets_file ""`, "secrets_file takes a path, not an empty string"},
		{"backup_record empty", `backup_record ""`, "backup_record takes a path, not an empty string"},
		{"unknown", `quiet_hours 22-07`, "unknown monitor subdirective"},
		{"block on a line", "operator ops {\n\tx\n}", "take no block"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parse(t, "monitor {\n\t"+c.body+"\n}")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to contain %q", err, c.want)
			}
		})
	}
}

// A state_dir line is refused by name, at its line: Alertmanager reads
// only the directory its drop-in names, so the line's own would be
// written and never read.
func TestCaddyfileRefusesStateDir(t *testing.T) {
	for _, line := range []string{"state_dir /srv/notify", `state_dir ""`, "state_dir " + DefaultStateDir} {
		_, err := parse(t, "monitor {\n\tchannel ops ntfy {secret.NOTIFY_OPS_URL}\n\toperator ops\n\t"+line+"\n}")
		if err == nil || !strings.Contains(err.Error(), "Testfile:4") ||
			!strings.Contains(err.Error(), "state_dir is not a monitor line: the state directory is fixed at /var/lib/motel-notify") {
			t.Errorf("%s: err = %v", line, err)
		}
	}
}

// So is an alertmanager line: motel-am.socket is where Alertmanager
// listens, and another path reaches nothing.
func TestCaddyfileRefusesAlertmanager(t *testing.T) {
	for _, line := range []string{"alertmanager /run/am.sock", `alertmanager ""`, "alertmanager " + DefaultSocket} {
		_, err := parse(t, "monitor {\n\tchannel ops ntfy {secret.NOTIFY_OPS_URL}\n\toperator ops\n\t"+line+"\n}")
		if err == nil || !strings.Contains(err.Error(), "Testfile:4") ||
			!strings.Contains(err.Error(), "alertmanager is not a monitor line: Alertmanager's socket is fixed at /run/motel-notify/am.sock") {
			t.Errorf("%s: err = %v", line, err)
		}
	}
}

func TestCaddyfileArgsOnOption(t *testing.T) {
	if _, err := parse(t, "notify extra {\n}"); err == nil {
		t.Fatal("an argument on the option itself was accepted")
	}
}

func TestResolveComposesAudiences(t *testing.T) {
	a := mustParse(t, fullBlock)
	r, err := a.resolve(testTrust(), testSecrets())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.operator, ","); got != "ops,pager" {
		t.Errorf("operator = %s", got)
	}
	// alice's key is trusted by blog and wiki; bob's repository by shop.
	want := map[string]string{"blog": "alice", "wiki": "alice", "shop": "bob"}
	for app, ch := range want {
		if got := strings.Join(r.appChannels[app], ","); got != ch {
			t.Errorf("app %s channels = %q, want %q", app, got, ch)
		}
	}
	if len(r.appChannels) != 3 {
		t.Errorf("appChannels = %v", r.appChannels)
	}
	if got := strings.Join(r.apps, ","); got != "blog,shop,wiki" {
		t.Errorf("apps = %s", got)
	}
	if r.urls["NOTIFY_PAGER_URL"] != "https://ntfy.sh/pager-topic" || r.heartbeat != "NOTIFY_HC_URL" {
		t.Errorf("urls = %v heartbeat = %q", r.urls, r.heartbeat)
	}
}

func TestResolveRefusals(t *testing.T) {
	base := `channel ops slack {secret.NOTIFY_OPS_URL}
	channel alice discord {secret.NOTIFY_ALICE_URL}
`
	cases := []struct {
		name, body string
		secrets    map[string]string
		want       string
	}{
		{"no operator", base, nil, "no operator line"},
		{"operator unknown", base + "operator ops pager", nil, "operator names pager"},
		{"channel twice", base + "channel ops json {secret.NOTIFY_BOB_URL}\noperator ops", nil, "defined twice"},
		{"bind unknown channel", base + "operator ops\nbind local:/etc/motel/alice.pub nobody", nil, "names channel nobody"},
		{"bind untrusted key", base + "operator ops\nbind local:/etc/motel/carol.pub alice", nil, "no app's deploy_trust trusts"},
		// A claim no source of the label pins: named, not blamed on trust.
		{"bind claim not pinned", base + "operator ops\nbind \"local:/etc/motel/alice.pub sub=alice\" alice", nil, "no deploy_trust source of local:/etc/motel/alice.pub pins sub: a binding names what a source pins"},
		{"bind deployed_by pasted whole", base + "operator ops\nbind \"" + ghIssuer + " repository=org/shop ref=refs/heads/main actor=octo\" alice", nil, "pins actor, ref: a binding names what a source pins, not a token's every claim (deployed_by shows them all); remove actor and ref from the line"},
		{"bind wrong repository", base + "operator ops\nbind \"" + ghIssuer + " repository=org/blog\" alice", nil, "no app's deploy_trust trusts"},
		{"bind wrong repository and a token's claim", base + "operator ops\nbind \"" + ghIssuer + " repository=org/blog ref=refs/heads/main\" alice", nil, "no app's deploy_trust trusts"},
		// Without ref= it would name no claim, which an oidc line must:
		// the advice would be refused in turn.
		{"bind an oidc token's claim alone", base + "operator ops\nbind \"" + ghIssuer + " ref=refs/heads/main\" alice", nil, "no app's deploy_trust trusts"},
		{"missing secret", base + "operator ops", map[string]string{"NOTIFY_OPS_URL": "https://hooks.slack.com/x"}, "secret NOTIFY_ALICE_URL is not in the box's secrets"},
		{"empty secret", base + "operator ops", map[string]string{"NOTIFY_OPS_URL": "https://hooks.slack.com/x", "NOTIFY_ALICE_URL": ""}, "secret NOTIFY_ALICE_URL is empty in the box's secrets (/etc/motel/secrets/_box.env): give it the channel's URL"},
		{"heartbeat missing", base + "operator ops\nheartbeat {secret.NOTIFY_HC_URL}", map[string]string{"NOTIFY_OPS_URL": "https://a.example.org/", "NOTIFY_ALICE_URL": "https://b.example.org/"}, "heartbeat: secret NOTIFY_HC_URL"},
		{"http", base + "operator ops", map[string]string{"NOTIFY_OPS_URL": "http://hooks.example.org/x", "NOTIFY_ALICE_URL": "https://b.example.org/"}, "is not an https URL"},
		{"loopback", base + "operator ops", map[string]string{"NOTIFY_OPS_URL": "https://127.0.0.1:9000/x", "NOTIFY_ALICE_URL": "https://b.example.org/"}, "not to a public address"},
		{"private v4", base + "operator ops", map[string]string{"NOTIFY_OPS_URL": "https://10.1.2.3/x", "NOTIFY_ALICE_URL": "https://b.example.org/"}, "not to a public address"},
		{"localhost", base + "operator ops", map[string]string{"NOTIFY_OPS_URL": "https://localhost/x", "NOTIFY_ALICE_URL": "https://b.example.org/"}, "not to a public host name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := mustParse(t, "monitor {\n"+c.body+"\n}")
			sec := c.secrets
			if sec == nil {
				sec = testSecrets()
			}
			_, err := a.resolve(testTrust(), sec)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to contain %q", err, c.want)
			}
			for _, v := range sec {
				if v != "" && strings.Contains(err.Error(), v) {
					t.Fatalf("the error quotes a secret's value: %v", err)
				}
			}
		})
	}
}

// A boot's load drops a channel whose secret it cannot use, from every
// place that names it, and says which, by line and key; anything else
// is refused as at any load.
func TestResolveDropsAtBoot(t *testing.T) {
	a := mustParse(t, fullBlock)
	sec := testSecrets()
	delete(sec, "NOTIFY_PAGER_URL")              // an operator channel, missing
	sec["NOTIFY_ALICE_URL"] = "http://10.0.0.1/" // alice's bound channel, refused
	delete(sec, "NOTIFY_HC_URL")                 // the heartbeat
	if _, _, err := a.resolveWith(testTrust(), sec, nil, false, nil); err == nil {
		t.Fatal("a load that does not drop accepted the missing secrets")
	}
	r, dropped, err := a.resolveWith(testTrust(), sec, nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.order, ","); got != "bob,ops" {
		t.Errorf("channels %s", got)
	}
	if got := strings.Join(r.operator, ","); got != "ops" {
		t.Errorf("operator %s", got)
	}
	if _, ok := r.appChannels["blog"]; ok || strings.Join(r.appChannels["shop"], ",") != "bob" {
		t.Errorf("appChannels %v", r.appChannels)
	}
	if r.heartbeat != "" || r.urls["NOTIFY_PAGER_URL"] != "" || r.urls["NOTIFY_OPS_URL"] == "" {
		t.Errorf("heartbeat %q urls %v", r.heartbeat, r.urls)
	}
	var reasons []string
	for _, e := range dropped {
		reasons = append(reasons, e.Error())
	}
	want := []string{
		"channel alice: the value of NOTIFY_ALICE_URL is not an https URL",
		"heartbeat: secret NOTIFY_HC_URL is not in the box's secrets (/etc/motel/secrets/_box.env)",
		"channel pager: secret NOTIFY_PAGER_URL is not in the box's secrets (/etc/motel/secrets/_box.env)",
	}
	if !slices.Equal(reasons, want) {
		t.Errorf("dropped:\n%s\nwant\n%s", strings.Join(reasons, "\n"), strings.Join(want, "\n"))
	}
	// What renders names no URL file it does not write.
	cfg, files, _ := render(r, "/state")
	for _, gone := range []string{"url-NOTIFY_PAGER_URL", "url-NOTIFY_ALICE_URL", "url-NOTIFY_HC_URL"} {
		if _, ok := files[gone]; ok || strings.Contains(string(cfg), gone) {
			t.Errorf("%s is rendered", gone)
		}
	}
	// A refusal that is not a secret's is refused all the same.
	b := mustParse(t, "monitor {\nchannel ops slack {secret.NOTIFY_OPS_URL}\noperator ops nosuch\n}")
	if _, _, err := b.resolveWith(testTrust(), map[string]string{}, nil, true, nil); err == nil || !strings.Contains(err.Error(), "operator names nosuch") {
		t.Fatalf("err = %v", err)
	}
}

// A secret that several lines read is dropped from each, and the
// reason names every one of them, not only the last in the block.
func TestResolveDropNamesEveryLineOfASharedSecret(t *testing.T) {
	a := mustParse(t, "monitor {\nchannel ops slack {secret.NOTIFY_OPS_URL}\nchannel ops2 json {secret.NOTIFY_OPS_URL}\nchannel spare ntfy {secret.NOTIFY_PAGER_URL}\noperator ops ops2 spare\nheartbeat {secret.NOTIFY_OPS_URL}\n}")
	sec := testSecrets()
	delete(sec, "NOTIFY_OPS_URL")
	r, dropped, err := a.resolveWith(testTrust(), sec, nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 1 || dropped[0].Error() != "channel ops, channel ops2, heartbeat: secret NOTIFY_OPS_URL is not in the box's secrets (/etc/motel/secrets/_box.env)" {
		t.Fatalf("dropped %v", dropped)
	}
	if strings.Join(r.order, ",") != "spare" || r.heartbeat != "" {
		t.Errorf("order %v heartbeat %q", r.order, r.heartbeat)
	}
	// A refusal names them all too.
	if _, _, err := a.resolveWith(testTrust(), sec, nil, false, nil); err == nil || !strings.HasPrefix(err.Error(), "channel ops, channel ops2, heartbeat: ") {
		t.Fatalf("err = %v", err)
	}
}

// A secrets file that cannot be used drops, at a boot, every line that
// reads a secret, each reason naming the file's error; any other load
// refuses.
func TestResolveDropsEveryLineOfAnUnusableFile(t *testing.T) {
	a := mustParse(t, fullBlock)
	unusable := errors.New("/etc/motel/secrets/_box.env:2: not KEY=VALUE")
	r, dropped, err := a.resolveWith(testTrust(), map[string]string{}, nil, true, unusable)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.order) != 0 || len(r.operator) != 0 || len(r.appChannels) != 0 || r.heartbeat != "" || len(r.urls) != 0 {
		t.Errorf("left: order %v operator %v apps %v heartbeat %q urls %v", r.order, r.operator, r.appChannels, r.heartbeat, r.urls)
	}
	if len(dropped) != 5 {
		t.Fatalf("dropped %v", dropped)
	}
	for _, e := range dropped {
		if !errors.Is(e, unusable) || !strings.Contains(e.Error(), "cannot be read: "+unusable.Error()) {
			t.Errorf("reason %v", e)
		}
	}
	if _, _, err := a.resolveWith(testTrust(), map[string]string{}, nil, false, unusable); !errors.Is(err, unusable) {
		t.Fatalf("a load that does not drop: err = %v", err)
	}
}

func TestResolveNoLiveswap(t *testing.T) {
	a := mustParse(t, "monitor {\nchannel ops slack {secret.NOTIFY_OPS_URL}\noperator ops\n}")
	r, err := a.resolve(map[string][]TrustSource{}, testSecrets())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.apps) != 0 || len(r.appChannels) != 0 {
		t.Fatalf("apps %v channels %v", r.apps, r.appChannels)
	}
	a = mustParse(t, "monitor {\nchannel ops slack {secret.NOTIFY_OPS_URL}\noperator ops\nbind local:/etc/motel/alice.pub ops\n}")
	if _, err := a.resolve(map[string][]TrustSource{}, testSecrets()); err == nil {
		t.Fatal("a binding with no apps at all was accepted")
	}
}

func TestCheckURL(t *testing.T) {
	cases := []struct {
		url      string
		insecure bool
		ok       bool
	}{
		{"https://hooks.slack.com/services/x", false, true},
		{"https://ntfy.sh/topic?template=alertmanager", false, true},
		{"https://user:pass@ntfy.example.org/topic", false, true},
		{"https://1.1.1.1/x", false, true},
		{"https://[2606:4700:4700::1111]/x", false, true},
		{"http://ntfy.sh/topic", false, false},
		{"ftp://ntfy.sh/topic", false, false},
		{"https:///no-host", false, false},
		{"not a url", false, false},
		{"https://127.0.0.1/", false, false},
		{"https://[::1]/", false, false},
		{"https://[::ffff:127.0.0.1]/", false, false},
		{"https://169.254.169.254/latest/meta-data", false, false},
		{"https://192.168.1.1/", false, false},
		{"https://172.16.0.1/", false, false},
		{"https://100.64.0.1/", false, false},
		{"https://0.0.0.0/", false, false},
		{"https://[fd00::1]/", false, false},
		{"https://[fe80::1]/", false, false},
		{"https://[64:ff9b::7f00:1]/", false, false},
		{"https://224.0.0.1/", false, false},
		{"https://localhost:8080/", false, false},
		{"https://app.localhost/", false, false},
		{"https://router.local/", false, false},
		{"https://metadata.google.internal/", false, false},
		{"https://intranet/", false, false},
		{"https://ntfy.sh./x", false, true},
		{"http://e2e-sink:8080/operator", true, true},
		{"http://127.0.0.1:18090/x", true, true},
		{"ftp://e2e-sink/x", true, false},
	}
	for _, c := range cases {
		err := checkURL(c.url, c.insecure)
		if (err == nil) != c.ok {
			t.Errorf("checkURL(%q, insecure=%v) = %v, want ok=%v", c.url, c.insecure, err, c.ok)
		}
		if err != nil && strings.Contains(err.Error(), c.url) {
			t.Errorf("the error quotes the URL: %v", err)
		}
	}
}
