package liveswap

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"
	"golang.org/x/crypto/ssh"

	"github.com/smallhoursorg/motel/liveswap/access"
)

// motelctl with a person's token against the real liveswap_webhook,
// with people declared: every key is made by ssh-keygen and every
// token signed by it, as on a laptop, so the box's checker takes what
// OpenSSH makes.

// ctlIdentity is the box's identity in these tests: the address the
// test server listens on. A token's aud and the request's Host must
// both equal it, and the client sends the Host it dials.
const ctlIdentity = "127.0.0.1"

// sshKey is a key ssh-keygen made in the test: its private file, which
// MOTEL_KEY names, and its public half as a person line has it.
type sshKey struct {
	path, typ, b64, fp string
}

// keygen makes a key with the system ssh-keygen, with no passphrase.
func keygen(t *testing.T, dir, name, typ string) sshKey {
	t.Helper()
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("SKIPPED — NOT TESTED HERE: no ssh-keygen on this machine, and motelctl signs with it; CI's runners have it")
	}
	path := filepath.Join(dir, name)
	args := []string{"-q", "-t", typ, "-N", "", "-C", name, "-f", path}
	if typ == "ecdsa" {
		args = append(args, "-b", "256")
	}
	if out, err := exec.Command("ssh-keygen", args...).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}
	line, err := os.ReadFile(path + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey(line)
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Fields(string(line))
	return sshKey{path: path, typ: f[0], b64: f[1], fp: ssh.FingerprintSHA256(pub)}
}

// lastBody serves h and keeps the body of the answer it last wrote.
type lastBody struct {
	mu   sync.Mutex
	body string
}

func (l *lastBody) serve(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		l.mu.Lock()
		l.body = rec.Body.String()
		l.mu.Unlock()
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	})
}

func (l *lastBody) get() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.body
}

// hostileProbe is what the app answered its health probe with, which
// the deploy record carries: escapes a terminal acts on, a bidi
// override and a C1 control.
const hostileProbe = "osc52 \x1b]52;c;cHduZWQ=\x07 csi \x1b[2J bidi abc\u202edef c1 \u009b31m"

// unsafeTerminal is any C0 control but the line feed, DEL, a C1
// control, a bidi control, U+2028 or U+2029.
var unsafeTerminal = regexp.MustCompile(`[\x00-\x09\x0b-\x1f\x7f\x{80}-\x{9f}\x{061c}\x{200e}\x{200f}\x{202a}-\x{202e}\x{2066}-\x{2069}\x{2028}\x{2029}]`)

// printedAs says whether stdout is the handler's body as --json prints
// it: byte for byte when the body holds nothing a terminal acts on,
// and otherwise the same JSON value with none of it left raw (the
// record of v2 carries what the app answered its probe with).
func printedAs(stdout, body string) bool {
	if unsafeTerminal.FindString(body) == "" {
		return stdout == body
	}
	var got, want any
	return json.Unmarshal([]byte(stdout), &got) == nil && json.Unmarshal([]byte(body), &want) == nil &&
		reflect.DeepEqual(got, want) && unsafeTerminal.FindString(stdout) == ""
}

// TestMotelctlPeople runs every command a person has against the real
// webhook, for a viewer of one app, a viewer of the box, a person with
// no grant, a key no line lists, and a person outside their grant's
// from list: the exit code; --json equal to the body the handler wrote
// (printedAs); human output with nothing a terminal acts on; the same
// 403 for an app that exists and one that does not; and the 401 that
// names every cause the client cannot tell apart.
func TestMotelctlPeople(t *testing.T) {
	bin := motelctlBin(t)
	keys := t.TempDir()
	alice := keygen(t, keys, "alice", "ed25519")
	carol := keygen(t, keys, "carol", "ed25519")
	dave := keygen(t, keys, "dave", "ed25519")
	erin := keygen(t, keys, "erin", "ed25519")

	h, rig := newTestHandler(t)
	rig.spec.deployLogLines = 40
	tok := appToken(t)
	if w := do(t, h, http.MethodPost, "/demo", tok, `{"url":"https://x/a.tgz","version":"v1"}`); w.Code != http.StatusOK {
		t.Fatalf("deploy v1: %d %s", w.Code, w.Body)
	}
	// v2 fails its health gate, and its record carries what the app
	// answered the probe with.
	rig.prober.err = fmt.Errorf("not healthy within deadline 5m: %w", &probeError{status: 500, body: hostileProbe})
	if w := do(t, h, http.MethodPost, "/demo", tok, `{"url":"https://x/b.tgz","version":"v2"}`); w.Code != http.StatusInternalServerError {
		t.Fatalf("deploy v2: %d %s", w.Code, w.Body)
	}
	blog := newManagedApp("blog")
	blog.spec, blog.verifiers, blog.logger = rig.spec, rig.ma.verifiers, zap.NewNop()
	h.app.managed["blog"] = blog

	var l access.Lines
	for name, k := range map[string]sshKey{"alice@example.test": alice, "carol@example.test": carol, "dave@example.test": dave, "erin@example.test": erin} {
		key, err := access.ParsePerson([]string{name, k.typ, k.b64})
		if err != nil {
			t.Fatal(err)
		}
		l.Keys = append(l.Keys, key)
	}
	for _, line := range []string{
		"alice@example.test viewer app demo",
		"carol@example.test viewer box",
		"erin@example.test viewer app demo from 198.51.100.0/24",
	} {
		g, err := access.ParseGrant(strings.Fields(line))
		if err != nil {
			t.Fatal(err)
		}
		l.Grants = append(l.Grants, g)
	}
	if _, err := access.CheckLines(l, nil); err != nil {
		t.Fatal(err)
	}
	h.people = access.NewRegistry(ctlIdentity, l)

	var last lastBody
	box := serveCtl(t, last.serve(webhook(h)))
	app := box.url + "/demo"
	envOf := func(k sshKey) []string {
		return []string{"MOTEL_KEY=" + k.path, "MOTEL_URL=" + app, "SSL_CERT_FILE=" + box.cert}
	}

	commands := map[string][]string{
		"whoami":      {"whoami"},
		"apps":        {"apps"},
		"status":      {"status", app},
		"deploys":     {"deploys", app},
		"one deploy":  {"deploys", app, "v2"},
		"another app": {"status", box.url + "/blog"},
		"no such app": {"status", box.url + "/ghost"},
	}
	type want struct {
		code   int
		stdout string // the human output, when it is to be exact
	}
	forbidden := want{code: 5}
	for _, c := range []struct {
		who  string
		key  sshKey
		want map[string]want
	}{
		{"a viewer of demo", alice, map[string]want{
			"whoami": {0, ""}, "apps": {0, "demo\n"}, "status": {0, ""}, "deploys": {0, ""}, "one deploy": {0, ""},
			"another app": forbidden, "no such app": forbidden,
		}},
		{"a viewer of the box", carol, map[string]want{
			"whoami": {0, ""}, "apps": {0, "blog\ndemo\n"}, "status": {0, ""}, "deploys": {0, ""}, "one deploy": {0, ""},
			"another app": {0, ""}, "no such app": {6, ""},
		}},
		{"a person with no grant", dave, map[string]want{
			"whoami": {0, ""}, "apps": {0, ""}, "status": forbidden, "deploys": forbidden, "one deploy": forbidden,
			"another app": forbidden, "no such app": forbidden,
		}},
	} {
		for name, args := range commands {
			t.Run(c.who+", "+name, func(t *testing.T) {
				w := c.want[name]
				r := ctl(t, bin, envOf(c.key), append(args, "--json")...)
				body := last.get()
				if name == "deploys" && w.code == 0 {
					var st struct{ Deploys json.RawMessage }
					if err := json.Unmarshal([]byte(body), &st); err != nil {
						t.Fatal(err)
					}
					body = string(st.Deploys) + "\n"
				}
				if r.code != w.code || !printedAs(r.stdout, body) {
					t.Fatalf("--json: exit %d, stdout\n%s\nwant %d and the handler's\n%s\nstderr %s", r.code, r.stdout, w.code, body, r.stderr)
				}
				r = ctl(t, bin, envOf(c.key), args...)
				if r.code != w.code || (w.stdout != "" && r.stdout != w.stdout) {
					t.Fatalf("exit %d, stdout\n%s\nwant %d and\n%s\nstderr %s", r.code, r.stdout, w.code, w.stdout, r.stderr)
				}
				if bad := unsafeTerminal.FindString(r.stdout + r.stderr); bad != "" {
					t.Errorf("the output holds %q:\n%s%s", bad, r.stdout, r.stderr)
				}
				if w.code == 0 && name != "apps" && r.stderr != "" {
					t.Errorf("stderr %q", r.stderr)
				}
				if w.code == 5 && !strings.Contains(r.stderr, "does not allow your key this: it needs the viewer role on ") {
					t.Errorf("the 403: %q", r.stderr)
				}
			})
		}
	}

	// The record's hostile probe body reaches the terminal made visible.
	r := ctl(t, bin, envOf(alice), "deploys", app, "v2")
	if !strings.Contains(r.stdout, `osc52 \x1b]52;c;cHduZWQ=\x07 csi \x1b[2J bidi abc\u202edef c1 \u009b31m`) {
		t.Errorf("the probe body, made visible, is not in the record:\n%s", r.stdout)
	}

	// Who each is, from here.
	for _, c := range []struct {
		key  sshKey
		want []string
	}{
		{alice, []string{"name     alice@example.test\n", "box      127.0.0.1, motel", "protocol 1\n", "key      " + alice.fp + " (" + alice.path + ".pub, MOTEL_KEY)\n", "roles    viewer of app demo\n", "token    expires "}},
		{carol, []string{"name     carol@example.test\n", "roles    viewer of the box\n"}},
		{dave, []string{"name     dave@example.test\n", "roles    none from this address\n"}},
	} {
		r := ctl(t, bin, envOf(c.key), "whoami")
		for _, w := range c.want {
			if !strings.Contains(r.stdout, w) {
				t.Errorf("whoami lacks %q:\n%s", w, r.stdout)
			}
		}
	}

	// The 403 for an app that exists and one that does not read the same.
	exists, ghost := ctl(t, bin, envOf(alice), "status", box.url+"/blog"), ctl(t, bin, envOf(alice), "status", box.url+"/ghost")
	if exists.stderr != strings.ReplaceAll(ghost.stderr, "ghost", "blog") || strings.Contains(exists.stderr, "blog") {
		t.Errorf("the 403s differ, or name the app:\n%s%s", exists.stderr, ghost.stderr)
	}
	exists, ghost = ctl(t, bin, envOf(alice), "status", box.url+"/blog", "--json"), ctl(t, bin, envOf(alice), "status", box.url+"/ghost", "--json")
	if exists.stdout != ghost.stdout {
		t.Errorf("the 403s' bodies differ: %q, %q", exists.stdout, ghost.stdout)
	}

	// No person with no grant views any app: said in words on stderr.
	if r := ctl(t, bin, envOf(dave), "apps"); r.stdout != "" || r.stderr != "motelctl: 127.0.0.1 lists no app your key may view from this address.\n" {
		t.Errorf("dave's apps: %+v", r)
	}
}

// TestMotelctlPeopleRefused: a key no line lists, and a person whose
// one grant's from list does not hold the address they ask from, are
// each the flat 401, after one retry with the same token (exactly two
// requests), for every command; the message names every cause the
// client cannot tell apart, and what to send the operator.
func TestMotelctlPeopleRefused(t *testing.T) {
	bin := motelctlBin(t)
	keys := t.TempDir()
	erin := keygen(t, keys, "erin", "ed25519")
	stranger := keygen(t, keys, "stranger", "ed25519")
	h, _ := newTestHandler(t)
	key, err := access.ParsePerson([]string{"erin@example.test", erin.typ, erin.b64})
	if err != nil {
		t.Fatal(err)
	}
	g, err := access.ParseGrant(strings.Fields("erin@example.test viewer app demo from 198.51.100.0/24"))
	if err != nil {
		t.Fatal(err)
	}
	h.people = access.NewRegistry(ctlIdentity, access.Lines{Keys: []access.Key{key}, Grants: []access.Grant{g}})
	served := webhook(h)
	for who, k := range map[string]sshKey{"a key no line lists": stranger, "a person outside their from list": erin} {
		for _, args := range [][]string{{"whoami"}, {"apps"}, {"status"}, {"deploys"}, {"deploys", "v1"}} {
			t.Run(who+", "+strings.Join(args, " "), func(t *testing.T) {
				t.Parallel() // each waits out the one retry
				// A box of its own, counting what reaches it.
				var mu sync.Mutex
				var auths []string
				box := serveCtl(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					auths = append(auths, r.Header.Get("Authorization"))
					mu.Unlock()
					served.ServeHTTP(w, r)
				}))
				app := box.url + "/demo"
				if args[0] != "whoami" && args[0] != "apps" {
					args = append([]string{args[0], app}, args[1:]...)
				}
				r := ctl(t, bin, []string{"MOTEL_KEY=" + k.path, "MOTEL_URL=" + app, "SSL_CERT_FILE=" + box.cert}, args...)
				mu.Lock()
				defer mu.Unlock()
				if len(auths) != 2 || auths[0] != auths[1] || !strings.HasPrefix(auths[0], "Motel m1.") {
					t.Errorf("%d requests, the same token twice: %v", len(auths), len(auths) == 2 && auths[0] == auths[1])
				}
				for _, w := range []string{
					"motelctl: 127.0.0.1 did not accept your key " + k.fp,
					"no person or signer line on the box lists it",
					"your address is outside the from list on every grant you hold",
					"127.0.0.1 is not the box's main name",
					"the box's moteld is too old to take a key's token at all",
					"Send the operator the line `motelctl key` prints.",
				} {
					if !strings.Contains(r.stderr, w) {
						t.Errorf("stderr lacks %q:\n%s", w, r.stderr)
					}
				}
				if r.code != 4 || r.stdout != "" || strings.Contains(r.stderr, "clock") {
					t.Errorf("exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
				}
			})
		}
	}
}

// TestMotelctlPeopleECDSA: a person whose key ssh-keygen made as ECDSA
// cannot be declared yet (the box refuses the line by name: a person's
// key is an Ed25519 key for now), and motelctl refuses that key before
// it signs or sends anything, exit 2 with the remedy, for a read and
// for `motelctl key`, which prints no line the box would refuse.
func TestMotelctlPeopleECDSA(t *testing.T) {
	bin := motelctlBin(t)
	frank := keygen(t, t.TempDir(), "frank", "ecdsa")
	if _, err := access.ParsePerson([]string{"frank@example.test", frank.typ, frank.b64}); err == nil || err.Error() != "person frank@example.test: a person's key is an Ed25519 key for now, and this release does not take ecdsa-sha2-nistp256 yet (ssh-keygen -t ed25519, or -t ed25519-sk for a hardware key)" {
		t.Fatalf("the box's reading of frank's line: %v", err)
	}
	h, _ := newTestHandler(t)
	served := webhook(h)
	var mu sync.Mutex
	reached := 0
	box := serveCtl(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reached++
		mu.Unlock()
		served.ServeHTTP(w, r)
	}))
	app := box.url + "/demo"
	want := "motelctl: " + frank.path + ".pub (MOTEL_KEY, " + frank.fp + ") is an ecdsa-sha2-nistp256 key. A box takes Ed25519 keys for now, not ECDSA yet: make one with ssh-keygen -t ed25519 (or -t ed25519-sk for a hardware key) and name it with --key or MOTEL_KEY.\n"
	for _, args := range [][]string{{"whoami"}, {"apps"}, {"status", app}, {"key", "--name", "frank@example.test"}} {
		r := ctl(t, bin, []string{"MOTEL_KEY=" + frank.path, "MOTEL_URL=" + app, "SSL_CERT_FILE=" + box.cert}, args...)
		if r.code != 2 || r.stdout != "" || r.stderr != want {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", args, r.code, r.stdout, r.stderr)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if reached != 0 {
		t.Errorf("%d requests reached the box", reached)
	}
}
