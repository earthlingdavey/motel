package notify

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"text/template"
	"time"

	"go.yaml.in/yaml/v3"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden")

const testStateDir = "/var/lib/motel-notify"

func renderFull(t *testing.T) ([]byte, map[string]string, map[string][]string) {
	t.Helper()
	a := mustParse(t, fullBlock)
	r, err := a.resolve(testTrust(), testSecrets())
	if err != nil {
		t.Fatal(err)
	}
	return render(r, testStateDir)
}

func TestRenderGolden(t *testing.T) {
	cfg, _, _ := renderFull(t)
	golden := filepath.Join("testdata", "alertmanager.golden.yml")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, cfg, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run with -update)", err)
	}
	if !bytes.Equal(cfg, want) {
		t.Fatalf("render differs from %s (run with -update and read the diff):\n%s", golden, cfg)
	}
	// Deterministic: the same block renders the same bytes, so an
	// unchanged reload writes nothing and reloads nothing.
	again, _, _ := renderFull(t)
	if !bytes.Equal(cfg, again) {
		t.Fatal("two renders of one block differ")
	}
}

func TestRenderKeepsURLsOutOfTheConfig(t *testing.T) {
	cfg, files, _ := renderFull(t)
	for key, v := range testSecrets() {
		if bytes.Contains(cfg, []byte(v)) {
			t.Errorf("the config holds the value of %s", key)
		}
		if files[urlFileName(key)] != v {
			t.Errorf("url file for %s = %q", key, files[urlFileName(key)])
		}
	}
	if len(files) != len(testSecrets()) {
		t.Errorf("files = %v", files)
	}
}

// receiverChannels is each receiver's channels, by the secret its URL
// file holds, sorted.
func receiverChannels(t *testing.T, cfg []byte) map[string][]string {
	t.Helper()
	var c amConfig
	if err := yaml.Unmarshal(cfg, &c); err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	strip := func(p string) string { return strings.TrimPrefix(filepath.Base(p), "url-") }
	for _, r := range c.Receivers {
		var keys []string
		for _, w := range r.WebhookConfigs {
			keys = append(keys, strip(w.URLFile))
		}
		for _, s := range r.SlackConfigs {
			keys = append(keys, strip(s.APIURLFile))
		}
		for _, d := range r.DiscordConfigs {
			keys = append(keys, strip(d.WebhookURLFile))
		}
		slices.Sort(keys)
		out[r.Name] = keys
	}
	return out
}

// reach is where Alertmanager's router sends an alert in a rendered
// tree: each route's children in order, the first match ending the walk
// unless it has continue, the route itself when no child matches. Only
// the matchers motel writes: k="v" and k=~".+". amtool says the same
// (TestRenderRoutesWithAmtool) where it is installed.
func reach(rt amRoute, labels map[string]string, inherited string) []string {
	for _, m := range rt.Matchers {
		if k, v, ok := strings.Cut(m, `=~`); ok {
			if v != `".+"` {
				panic("reach: matcher " + m)
			}
			if labels[k] == "" {
				return nil
			}
			continue
		}
		k, v, _ := strings.Cut(m, "=")
		if labels[k] != strings.Trim(v, `"`) {
			return nil
		}
	}
	if rt.Receiver != "" {
		inherited = rt.Receiver
	}
	var out []string
	for _, c := range rt.Routes {
		got := reach(c, labels, inherited)
		out = append(out, got...)
		if got != nil && !c.Continue {
			break
		}
	}
	if len(out) == 0 {
		out = []string{inherited}
	}
	return out
}

func labelSet(kv ...string) map[string]string {
	m := map[string]string{}
	for _, s := range kv {
		k, v, _ := strings.Cut(s, "=")
		m[k] = v
	}
	return m
}

// TestRenderOneChannelPerReceiver: every receiver holds one channel, so
// that a failure counted by receiver names it alone; an audience is a
// leaf per channel under continue: the operator's channels plus the
// app's, each only at or above its floor.
func TestRenderOneChannelPerReceiver(t *testing.T) {
	cfg, _, recv := renderFull(t)
	got := receiverChannels(t, cfg)
	const ops, pager, alice, bob = "NOTIFY_OPS_URL", "NOTIFY_PAGER_URL", "NOTIFY_ALICE_URL", "NOTIFY_BOB_URL"
	secret := map[string]string{"ops": ops, "pager": pager, "alice": alice, "bob": bob}
	for name, keys := range got {
		switch name {
		case "blackhole":
			if len(keys) != 0 {
				t.Errorf("blackhole sends to %v", keys)
			}
		case "heartbeat":
			if !slices.Equal(keys, []string{"NOTIFY_HC_URL"}) || len(recv[name]) != 0 {
				t.Errorf("heartbeat = %v, channels %v", keys, recv[name])
			}
		default:
			if len(keys) != 1 || len(recv[name]) != 1 || secret[recv[name][0]] != keys[0] {
				t.Errorf("receiver %s sends to %v, render says channels %v", name, keys, recv[name])
			}
		}
	}
	if len(recv) != len(got)-1 { // the blackhole names no channel
		t.Errorf("render's receivers %v, the config's %v", recv, got)
	}
	// Each channel's receivers: pager takes criticals alone, bob
	// warnings and up, and neither alice nor bob, a deployer's, is an
	// operator's (no fallback).
	want := map[string][]string{
		"pager": {"ch-pager", "event-critical-pager", "fallback-pager", "state-critical-pager"},
		"bob":   {"ch-bob", "event-critical-bob", "event-warning-bob", "state-critical-bob", "state-warning-bob"},
		"alice": {"ch-alice", "event-critical-alice", "event-info-alice", "event-warning-alice",
			"state-critical-alice", "state-info-alice", "state-warning-alice"},
	}
	w := failWatch{channels: recv}
	for ch, names := range want {
		if g := w.receiversOf([]string{ch}); !slices.Equal(g, names) {
			t.Errorf("%s's receivers = %v, want %v", ch, g, names)
		}
	}

	var c amConfig
	if err := yaml.Unmarshal(cfg, &c); err != nil {
		t.Fatal(err)
	}
	routes := []struct {
		labels map[string]string
		want   []string
	}{
		{labelSet("alertname=Watchdog"), []string{"heartbeat"}},
		{labelSet("alertname=ChannelFailed", "notify_channel=pager", "severity=warning"), []string{"ch-pager"}},
		{labelSet("alertname=deploy_in_progress", "kind=mute", "app=blog"), []string{"blackhole"}},
		{labelSet("alertname=deploy_failed", "kind=event", "app=blog", "severity=warning"), []string{"event-warning-ops", "event-warning-alice"}},
		{labelSet("alertname=deploy_succeeded", "kind=event", "app=shop", "severity=info"), []string{"event-info-ops"}},
		{labelSet("alertname=x_failed", "kind=event", "app=shop", "severity=critical"), []string{"event-critical-ops", "event-critical-pager", "event-critical-bob"}},
		{labelSet("alertname=backup_stale", "kind=event", "subject=box", "severity=critical"), []string{"event-critical-ops", "event-critical-pager"}},
		{labelSet("alertname=relaunch_refused", "kind=state", "app=wiki", "severity=critical"), []string{"state-critical-ops", "state-critical-pager", "state-critical-alice"}},
		{labelSet("alertname=disk_low", "kind=state", "subject=box", "severity=warning"), []string{"state-warning-ops"}},
		{labelSet("alertname=watchdog_throttled", "kind=state", "app=nobody", "severity=critical"), []string{"state-critical-ops", "state-critical-pager"}},
		{labelSet("alertname=x"), []string{"fallback-ops", "fallback-pager"}},
	}
	for _, r := range routes {
		if g := reach(c.Route, r.labels, ""); !slices.Equal(g, r.want) {
			t.Errorf("%v → %v, want %v", r.labels, g, r.want)
		}
	}

	// send_resolved: state receivers announce the recovery, and the
	// fallback's; events and direct ones never do.
	for _, r := range c.Receivers {
		want := strings.HasPrefix(r.Name, "state-") || strings.HasPrefix(r.Name, "fallback-")
		for _, w := range r.WebhookConfigs {
			if w.SendResolved != want {
				t.Errorf("receiver %s send_resolved = %v", r.Name, w.SendResolved)
			}
		}
		for _, s := range r.SlackConfigs {
			if s.SendResolved != want {
				t.Errorf("receiver %s send_resolved = %v", r.Name, s.SendResolved)
			}
		}
		for _, d := range r.DiscordConfigs {
			if d.SendResolved != want {
				t.Errorf("receiver %s send_resolved = %v", r.Name, d.SendResolved)
			}
		}
	}
}

// TestRenderSeverityNoChannelTakesEndsThere: a severity no channel of
// the audience takes ends at the blackhole, and does not fall through
// to the info route's channels.
func TestRenderSeverityNoChannelTakesEndsThere(t *testing.T) {
	a := mustParse(t, `monitor {
	channel ops ntfy {secret.NOTIFY_OPS_URL} severity critical
	operator ops
	heartbeat {secret.NOTIFY_HC_URL}
}`)
	r, err := a.resolve(testTrust(), testSecrets())
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, _ := render(r, testStateDir)
	var c amConfig
	if err := yaml.Unmarshal(cfg, &c); err != nil {
		t.Fatal(err)
	}
	for _, sev := range []string{SeverityWarning, SeverityInfo} {
		if g := reach(c.Route, labelSet("alertname=x_failed", "kind=event", "severity="+sev), ""); !slices.Equal(g, []string{"blackhole"}) {
			t.Errorf("a %s → %v", sev, g)
		}
	}
	if g := reach(c.Route, labelSet("alertname=x_failed", "kind=event", "severity=critical"), ""); !slices.Equal(g, []string{"event-critical-ops"}) {
		t.Errorf("a critical → %v", g)
	}
}

// TestRenderMarksResolvedAlertsInText: Slack and Discord get motel's
// text, not the payload; a grouped notification names which alerts in
// it have resolved.
func TestRenderMarksResolvedAlertsInText(t *testing.T) {
	if !strings.Contains(msgText, `if eq .Status "resolved"`) || !strings.Contains(msgText, "[RESOLVED]") {
		t.Fatalf("msgText = %s", msgText)
	}
	cfg, _, _ := renderFull(t)
	if !bytes.Contains(cfg, []byte("[RESOLVED]")) {
		t.Fatal("the rendered config does not carry it")
	}
}

// TestRenderSlackColourAndNoDeadLink: Slack's colour follows the
// severity, not danger for everything firing; neither its title nor
// its fallback links to Alertmanager's external URL, where nothing
// listens.
func TestRenderSlackColourAndNoDeadLink(t *testing.T) {
	tmpl := template.Must(template.New("c").Parse(slackColor))
	for _, c := range []struct{ status, sev, want string }{
		{"firing", SeverityCritical, "danger"},
		{"firing", SeverityWarning, "warning"},
		{"firing", SeverityInfo, "#439FE0"},
		{"firing", "", "#439FE0"},
		{"resolved", SeverityCritical, "good"},
	} {
		var b strings.Builder
		data := struct {
			Status       string
			CommonLabels map[string]string
		}{c.status, map[string]string{"severity": c.sev}}
		if err := tmpl.Execute(&b, data); err != nil || b.String() != c.want {
			t.Errorf("%s %s: %q, %v; want %q", c.status, c.sev, b.String(), err, c.want)
		}
	}
	cfg, _, _ := renderFull(t)
	var c amConfig
	if err := yaml.Unmarshal(cfg, &c); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range c.Receivers {
		for _, s := range r.SlackConfigs {
			n++
			if s.TitleLink != "" || s.Fallback != msgTitle || s.Color != slackColor {
				t.Errorf("%s: %+v", r.Name, s)
			}
		}
	}
	if n == 0 || !bytes.Contains(cfg, []byte(`title_link: ""`)) {
		t.Fatal("no slack config with an empty title_link: Alertmanager's default would link to :9093")
	}
}

// TestRenderNtfyPriority: an ntfy channel's message carries the
// receiver's severity as ntfy's priority, so a critical pages; json
// channels, the same integration, carry no header.
func TestRenderNtfyPriority(t *testing.T) {
	cfg, _, _ := renderFull(t)
	var c amConfig
	if err := yaml.Unmarshal(cfg, &c); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		// pager is ntfy, critical alone; bob is json.
		"event-critical-pager": "5",
		"state-critical-pager": "5",
		"ch-pager":             "4", // ChannelFailed's warning, and a test at it
		"fallback-pager":       "",
		"event-critical-bob":   "",
		"state-critical-bob":   "",
		"state-warning-bob":    "",
	}
	got := map[string]string{}
	for _, r := range c.Receivers {
		for _, w := range r.WebhookConfigs {
			p := ""
			if w.HTTPConfig != nil {
				p = strings.Join(w.HTTPConfig.Headers["X-Priority"].Values, ",")
			}
			got[r.Name] = p
		}
	}
	for k, p := range want {
		if g, ok := got[k]; !ok || g != p {
			t.Errorf("%s: X-Priority %q (present %v), want %q", k, g, ok, p)
		}
	}
	for k, p := range got {
		if strings.HasSuffix(k, "-bob") && p != "" {
			t.Errorf("a json channel carries X-Priority %q (%s)", p, k)
		}
	}
	if ntfyPriority[SeverityWarning] != "4" || ntfyPriority[SeverityInfo] != "" {
		t.Errorf("priorities = %v", ntfyPriority)
	}
}

func TestRenderPlumbingFirstAndInhibition(t *testing.T) {
	cfg, _, _ := renderFull(t)
	var c amConfig
	if err := yaml.Unmarshal(cfg, &c); err != nil {
		t.Fatal(err)
	}
	var first []string
	for _, r := range c.Route.Routes {
		first = append(first, strings.Join(r.Matchers, ","))
	}
	want := []string{`alertname="Watchdog"`, `notify_channel=~".+"`, `alertname="deploy_in_progress"`,
		`kind="state",alertname="disk_low"`, `kind="event"`, `kind="state"`, ``}
	if !slices.Equal(first, want) {
		t.Fatalf("top-level routes = %q\nwant %q", first, want)
	}
	// continue is a channel's leaf's alone: on any other route an alert
	// would reach two audiences.
	var walk func(rt amRoute, depth int)
	walk = func(rt amRoute, depth int) {
		if rt.Continue && (len(rt.Routes) > 0 || len(rt.Matchers) > 0 || rt.Receiver == "") {
			t.Errorf("continue on a route that is not a channel's leaf: %+v", rt)
		}
		for _, c := range rt.Routes {
			walk(c, depth+1)
		}
	}
	walk(c.Route, 0)
	if c.Route.Receiver != "blackhole" {
		t.Errorf("root receiver %q", c.Route.Receiver)
	}
	ev := c.Route.Routes[4]
	if !slices.Equal(ev.GroupBy, []string{"..."}) || ev.GroupWait != "0s" {
		t.Errorf("event route group_by %v group_wait %s", ev.GroupBy, ev.GroupWait)
	}
	if len(c.InhibitRules) != 1 || strings.Contains(strings.Join(c.InhibitRules[0].TargetMatchers, ""), "severity") {
		t.Errorf("inhibit rules = %+v", c.InhibitRules)
	}
}

// amtool is Debian's prometheus-alertmanager's; the tests that need it
// skip without it. `make test` runs in golang:trixie, which lacks it;
// e2e's image has it, and so does a dev container after
// `apt-get install prometheus-alertmanager`.
func amtool(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("amtool")
	if err != nil {
		t.Skip("amtool is not installed (Debian: prometheus-alertmanager)")
	}
	return p
}

func writeRendered(t *testing.T) string {
	t.Helper()
	a := mustParse(t, fullBlock)
	r, err := a.resolve(testTrust(), testSecrets())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg, files, _ := render(r, dir)
	if _, _, err := writeState(dir, cfg, files); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, ConfigName)
}

func TestRenderPassesAmtoolCheckConfig(t *testing.T) {
	tool := amtool(t)
	cfg := writeRendered(t)
	out, err := exec.Command(tool, "check-config", cfg).CombinedOutput()
	if err != nil {
		t.Fatalf("amtool check-config: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("SUCCESS")) {
		t.Fatalf("amtool check-config said:\n%s", out)
	}
}

// TestRenderRoutesWithAmtool asks Alertmanager's own router where each
// alert motel posts goes, which is the route tree's whole contract:
// plumbing to its own receivers, an app alert to one receiver for each
// channel of the operator's and the app's, a box alert to one for each
// of the operator's, nothing to the fallback.
func TestRenderRoutesWithAmtool(t *testing.T) {
	tool := amtool(t)
	cfg := writeRendered(t)
	cases := []struct {
		labels []string
		want   string
	}{
		{[]string{"alertname=Watchdog", "severity=none"}, "heartbeat"},
		{[]string{"alertname=ChannelFailed", "notify_channel=pager", "severity=warning"}, "ch-pager"},
		{[]string{"alertname=NotifyTest", "notify_channel=alice", "severity=info"}, "ch-alice"},
		{[]string{"alertname=deploy_in_progress", "kind=mute", "app=blog", "severity=none"}, "blackhole"},
		{[]string{"alertname=deploy_failed", "kind=event", "app=blog", "severity=warning"}, "event-warning-ops,event-warning-alice"},
		{[]string{"alertname=deploy_succeeded", "kind=event", "app=shop", "severity=info"}, "event-info-ops"},
		{[]string{"alertname=backup_stale", "kind=event", "subject=box", "severity=critical"}, "event-critical-ops,event-critical-pager"},
		{[]string{"alertname=watchdog_throttled", "kind=state", "app=blog", "severity=critical"}, "state-critical-ops,state-critical-pager,state-critical-alice"},
		{[]string{"alertname=relaunch_refused", "kind=state", "app=wiki", "severity=critical"}, "state-critical-ops,state-critical-pager,state-critical-alice"},
		{[]string{"alertname=disk_low", "kind=state", "subject=box", "severity=warning"}, "state-warning-ops"},
		{[]string{"alertname=cert_failed", "kind=state", "subject=box", "severity=warning"}, "state-warning-ops"},
		{[]string{"alertname=x_failed", "kind=event", "app=shop", "severity=critical"}, "event-critical-ops,event-critical-pager,event-critical-bob"},
		// bob, shop's, takes warnings and up: shop's info is ops' alone.
		{[]string{"alertname=x_done", "kind=state", "app=shop", "severity=info"}, "state-info-ops"},
		// An app with no bindings of its own (none here) or a name no
		// route knows: the operator's, on the kind's own receivers.
		{[]string{"alertname=watchdog_throttled", "kind=state", "app=nobody", "severity=critical"}, "state-critical-ops,state-critical-pager"},
		{[]string{"alertname=x"}, "fallback-ops,fallback-pager"},
	}
	for _, c := range cases {
		args := append([]string{"config", "routes", "test", "--config.file=" + cfg}, c.labels...)
		out, err := exec.Command(tool, args...).Output() // stdout: a warning about --alertmanager.url goes to stderr
		got := strings.TrimSpace(string(out))
		if err != nil || got != c.want {
			t.Errorf("%v → %q (%v), want %q", c.labels, got, err, c.want)
		}
	}
}

func TestWriteStateNotSetUp(t *testing.T) {
	_, _, err := writeState(filepath.Join(t.TempDir(), "missing"), []byte("x"), nil)
	if !errors.Is(err, errNotSetUp) {
		t.Fatalf("err = %v", err)
	}
}

func TestWriteStateModesStaleFilesAndNoChange(t *testing.T) {
	dir := t.TempDir()
	// A URL file from a channel since removed, and an operator's own
	// file, which is not motel's to remove.
	for _, n := range []string{"url-NOTIFY_GONE_URL", "README"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("old"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{"url-NOTIFY_OPS_URL": "https://ntfy.sh/ops"}
	changed, prev, err := writeState(dir, []byte("cfg-1\n"), files)
	if err != nil || !changed || prev != nil {
		t.Fatalf("first write: changed %v prev %q err %v", changed, prev, err)
	}
	for _, n := range []string{ConfigName, "url-NOTIFY_OPS_URL"} {
		st, err := os.Stat(filepath.Join(dir, n))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o640 {
			t.Errorf("%s mode %v", n, st.Mode().Perm())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "url-NOTIFY_GONE_URL")); !os.IsNotExist(err) {
		t.Errorf("a stale URL file was kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "README")); err != nil {
		t.Errorf("a file that is not motel's was removed: %v", err)
	}
	changed, prev, err = writeState(dir, []byte("cfg-1\n"), files)
	if err != nil || changed || string(prev) != "cfg-1\n" {
		t.Fatalf("unchanged write: changed %v prev %q err %v", changed, prev, err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("a temporary was left: %s", e.Name())
		}
	}
}

// fakeAM is an Alertmanager stand-in on a unix socket.
type fakeAM struct {
	mu       sync.Mutex
	reloads  int
	posts    [][]byte
	status   int // for /-/reload and /api/v2/alerts; 0 = 200
	metrics  string
	listener net.Listener
}

func startFakeAM(t *testing.T) (*fakeAM, string) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "am.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeAM{listener: l}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		code := f.status
		if code == 0 {
			code = http.StatusOK
		}
		switch r.URL.Path {
		case "/-/reload":
			f.reloads++
			w.WriteHeader(code)
			if code != http.StatusOK {
				_, _ = w.Write([]byte("failed to reload config: yaml: bad"))
			}
		case "/api/v2/alerts":
			var b bytes.Buffer
			_, _ = b.ReadFrom(r.Body)
			f.posts = append(f.posts, b.Bytes())
			w.WriteHeader(code)
		case "/metrics":
			_, _ = w.Write([]byte(f.metrics))
		default:
			http.NotFound(w, r)
		}
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return f, sock
}

func TestApplyReloadsAndRollsBack(t *testing.T) {
	f, sock := startFakeAM(t)
	am := newAMClient(sock)
	dir := t.TempDir()
	ctx := context.Background()
	reloaded, err := apply(ctx, dir, []byte("good\n"), nil, am)
	if err != nil || !reloaded || f.reloads != 1 {
		t.Fatalf("first apply: reloaded %v err %v reloads %d", reloaded, err, f.reloads)
	}
	// Nothing changed: no write, no reload.
	if reloaded, err := apply(ctx, dir, []byte("good\n"), nil, am); err != nil || reloaded || f.reloads != 1 {
		t.Fatalf("unchanged apply: reloaded %v err %v reloads %d", reloaded, err, f.reloads)
	}
	// Refused: the previous file is put back for Alertmanager's next start.
	f.mu.Lock()
	f.status = http.StatusBadRequest
	f.mu.Unlock()
	_, err = apply(ctx, dir, []byte("bad\n"), nil, am)
	var refused *amStatusError
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "failed to reload config") {
		t.Fatalf("refused apply: err = %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, ConfigName)); string(got) != "good\n" {
		t.Fatalf("after a refused reload the file is %q", got)
	}
}

func TestApplyWithoutAlertmanagerRunning(t *testing.T) {
	dir := t.TempDir()
	am := newAMClient(filepath.Join(t.TempDir(), "no.sock"))
	reloaded, err := apply(context.Background(), dir, []byte("cfg\n"), nil, am)
	if err != nil || reloaded {
		t.Fatalf("reloaded %v err %v", reloaded, err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, ConfigName)); string(got) != "cfg\n" {
		t.Fatalf("file = %q: written for Alertmanager's next start", got)
	}
}

// TestApplyRetriesAnUnansweredReload: a reload Alertmanager did not
// answer is an error, and the next load asks again though the file has
// not changed since.
func TestApplyRetriesAnUnansweredReload(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(t.TempDir(), "hung.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := apply(ctx, dir, []byte("cfg\n"), nil, newAMClient(sock)); !errors.Is(err, errUnanswered) {
		t.Fatalf("a hung Alertmanager: err = %v", err)
	}
	f, good := startFakeAM(t)
	reloaded, err := apply(context.Background(), dir, []byte("cfg\n"), nil, newAMClient(good))
	if err != nil || !reloaded || f.reloads != 1 {
		t.Fatalf("the next load: reloaded %v err %v reloads %d", reloaded, err, f.reloads)
	}
}

// TestRenderHeartbeatRepeatsEveryFlush: Alertmanager re-sends at a flush
// only when the last send was more than repeat_interval ago, stamped
// after the send returned; a repeat equal to the group interval skips
// every other flush.
func TestRenderHeartbeatRepeatsEveryFlush(t *testing.T) {
	cfg, _, _ := renderFull(t)
	var c amConfig
	if err := yaml.Unmarshal(cfg, &c); err != nil {
		t.Fatal(err)
	}
	hb := c.Route.Routes[0]
	if hb.Receiver != "heartbeat" {
		t.Fatalf("first route = %+v", hb)
	}
	interval, err1 := time.ParseDuration(hb.GroupInterval)
	repeat, err2 := time.ParseDuration(hb.RepeatInterval)
	if err1 != nil || err2 != nil {
		t.Fatalf("durations %q %q", hb.GroupInterval, hb.RepeatInterval)
	}
	if interval != 5*time.Minute || repeat >= interval {
		t.Fatalf("group_interval %v, repeat_interval %v: want a 5m interval and a repeat under it", interval, repeat)
	}
}
