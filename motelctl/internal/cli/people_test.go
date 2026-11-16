package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/smallhoursorg/motel/liveswap/wire"
	"github.com/smallhoursorg/motel/motelctl/internal/sshkey"
)

// whoamiNow is the clock of the whoami tests: the fake box's token
// expires 270 s after it.
var whoamiNow = time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)

// meLine is runKey's key, ~/me.pub: made up, and only its shape is read.
const meLine = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMuTYJV40qpZ7rH7T4/BJP6ERoj5JgqFQwHIEBTe5wrn" // gitleaks:allow — made up for this test

// meFingerprint is meLine's fingerprint, which an honest box names.
func meFingerprint() string {
	pk, err := sshkey.ParsePublic(meLine)
	if err != nil {
		panic(err)
	}
	return pk.Fingerprint()
}

// hostileWhoAmI is about runKey's key, from the box it asks (the fake
// box's address); a role a box may name later (auditor) is quoted as
// the box's word. whoami prints no box value outside its grammar: an answer with
// a hostile name, release, role or scope is not taken at all (client's
// TestWhoamiNullsAndRoles), and a key or box that differs is quoted
// (TestWhoamiAboutAnotherKeyOrBox).
func hostileWhoAmI() wire.WhoAmI {
	return wire.WhoAmI{
		Name: "alice@example.com", Key: meFingerprint(),
		Roles: []wire.RoleHeld{{Role: "viewer", Scope: "box"}, {Role: "viewer", Scope: "app:blog"}, {Role: "auditor", Scope: "app:shop-2"}},
		Box:   "127.0.0.1", Version: "v0.7.0", Protocol: 1, TokenExpiresAt: whoamiNow.Add(270 * time.Second),
	}
}

// runKey runs motelctl with a person's key, ~/me.pub, whose signer is
// a stand-in, at whoamiNow, on the fake box (whose port MOTEL_URL
// carries).
func runKey(t *testing.T, fb *fakeBox, env map[string]string, args ...string) result {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "me.pub"), []byte(meLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	all := map[string]string{"MOTEL_KEY": "~/me.pub", "MOTEL_URL": fb.srv.URL + "/blog"}
	for k, v := range env {
		all[k] = v
	}
	var out, errOut bytes.Buffer
	code := Main(context.Background(), args, Env{
		Getenv: func(k string) string { return all[k] }, Stdout: &out, Stderr: &errOut, Dir: t.TempDir(), Home: home,
		Now: func() time.Time { return whoamiNow }, Sleep: func(context.Context, time.Duration) error { return nil },
		Transport: fb.srv.Client().Transport, Run: noPrograms{}, Signer: &fixedSigner{}, Version: "test",
	})
	return result{code, out.String(), errOut.String()}
}

// whoami and apps: --json is the box's body as received; the human
// output holds nothing a box could use on a terminal, and is what the
// goldens say. (A list of apps with a name outside the app grammar is
// not taken at all: client's TestReadPeoplesFields.) The fake box's roles hold a role and a scope no box
// sends today, printed as they came.
func TestWhoamiAndApps(t *testing.T) {
	fb := newFakeBox(t)
	for _, c := range []struct {
		golden, target string
		args           []string
	}{
		{"whoami", "/_motel/whoami", []string{"whoami"}},
		{"apps", "/_motel/apps", []string{"apps"}},
	} {
		t.Run(c.golden, func(t *testing.T) {
			r := runKey(t, fb, nil, append(c.args, "--json")...)
			if r.code != 0 || r.stdout != string(fb.bodies[c.target]) {
				t.Fatalf("exit %d, stdout\n%q\nwant the box's\n%q\nstderr %s", r.code, r.stdout, fb.bodies[c.target], r.stderr)
			}
			if !strings.HasPrefix(fb.auth[len(fb.auth)-1], "Motel m1.") {
				t.Errorf("Authorization %q, want a person's token", fb.auth[len(fb.auth)-1])
			}
			r = runKey(t, fb, nil, c.args...)
			if r.code != 0 || r.stderr != "" {
				t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
			}
			if bad := unsafeOutput.FindString(r.stdout); bad != "" || !utf8.ValidString(r.stdout) {
				t.Errorf("the output holds %q, or is not UTF-8:\n%s", bad, r.stdout)
			}
			// The fingerprint is put by name, so the golden holds no
			// string a secret scan reads as a key.
			golden(t, "hostile-"+c.golden, strings.ReplaceAll(r.stdout, meFingerprint(), "<me.pub's fingerprint>"))
		})
	}
}

// whoami names "a build that names no release" for an empty version,
// "none" for no role, and a token already past its time as expired.
func TestWhoamiPlain(t *testing.T) {
	fb := newFakeBox(t)
	fb.whoami = wire.WhoAmI{Name: "dave", Key: meFingerprint(), Roles: []wire.RoleHeld{}, Box: "127.0.0.1", Protocol: 1, TokenExpiresAt: whoamiNow.Add(-5 * time.Second)}
	r := runKey(t, fb, nil, "whoami")
	want := "name     dave\n" +
		"box      127.0.0.1, motel, a build that names no release, protocol 1\n" +
		"key      " + meFingerprint() + " (~/me.pub, MOTEL_KEY)\n" +
		"roles    none from this address\n" +
		"token    expired 2026-11-15 09:59:55 UTC\n"
	if r.code != 0 || r.stdout != want {
		t.Fatalf("exit %d\n%s\nwant\n%s", r.code, r.stdout, want)
	}
}

// apps with none: said on stderr in words, nothing on stdout, exit 0;
// --json is the box's [] as it came.
func TestAppsNone(t *testing.T) {
	fb := newFakeBox(t)
	fb.apps = wire.Apps{Apps: []wire.AppEntry{}}
	r := runKey(t, fb, nil, "apps")
	if r.code != 0 || r.stdout != "" || r.stderr != "motelctl: 127.0.0.1 lists no app your key may view from this address.\n" {
		t.Fatalf("%+v", r)
	}
	r = runKey(t, fb, nil, "apps", "--json")
	if r.code != 0 || r.stdout != `{"apps":[]}`+"\n" || r.stderr != "" {
		t.Fatalf("--json: %+v", r)
	}
}

// whoami and apps are asked with a key alone: with MOTEL_TOKEN set and
// no --key, nothing is sent and the command says how to proceed; with
// --key, the key's token is sent and MOTEL_TOKEN is not.
func TestWhoamiAndAppsNeedAKey(t *testing.T) {
	for _, cmd := range []string{"whoami", "apps"} {
		t.Run(cmd, func(t *testing.T) {
			fb := newFakeBox(t)
			env := map[string]string{"MOTEL_TOKEN": "a.deploy.token"}
			r := runKey(t, fb, env, cmd)
			want := "motelctl: " + cmd + " is asked with your own ssh key, and MOTEL_TOKEN is set, which motelctl would send instead: a deploy token is no one's key. Name your key with --key, or unset MOTEL_TOKEN.\n"
			if r.code != 2 || len(fb.auth) != 0 || r.stdout != "" || r.stderr != want {
				t.Fatalf("exit %d, %d requests, stdout %q, stderr %q", r.code, len(fb.auth), r.stdout, r.stderr)
			}
			r = runKey(t, fb, env, cmd, "--json")
			var e struct{ Error string }
			if r.code != 2 || json.Unmarshal([]byte(r.stdout), &e) != nil || !strings.Contains(e.Error, "unset MOTEL_TOKEN") {
				t.Fatalf("--json: %+v", r)
			}
			r = runKey(t, fb, env, cmd, "--key", "~/me.pub")
			if r.code != 0 || len(fb.auth) != 1 || !strings.HasPrefix(fb.auth[0], "Motel m1.") {
				t.Fatalf("with --key: exit %d, Authorization %v, stderr %s", r.code, fb.auth, r.stderr)
			}
		})
	}
}

// An answer about another key than the one this command signed with,
// or from another box than the one it asked, is not one to act on:
// only the warning is printed, the box's value quoted beside this
// side's, and nothing else of the answer; the command exits 3, and
// --json prints motelctl's own error, not the body.
func TestWhoamiAboutAnotherKeyOrBox(t *testing.T) {
	for _, c := range []struct {
		name  string
		edit  func(*wire.WhoAmI)
		warns string
	}{
		{"another key", func(me *wire.WhoAmI) { me.Key = "SHA256:another\x1b[2J" },
			`warning  the box names another key than the one this command signed with (<fp>): "SHA256:another\x1b[2J"`},
		{"another box", func(me *wire.WhoAmI) { me.Box = `other.example.com"` },
			`warning  the box answers as another box than the one this command asked (127.0.0.1): "other.example.com\""`},
	} {
		t.Run(c.name, func(t *testing.T) {
			fb := newFakeBox(t)
			c.edit(&fb.whoami)
			r := runKey(t, fb, nil, "whoami")
			if r.code != 3 || r.stdout != strings.ReplaceAll(c.warns, "<fp>", meFingerprint())+"\n" ||
				!strings.Contains(r.stderr, "is not one to act on") {
				t.Fatalf("exit %d, stdout\n%s\nstderr %s", r.code, r.stdout, r.stderr)
			}
			r = runKey(t, fb, nil, "whoami", "--json")
			if r.code != 3 || !strings.HasPrefix(r.stdout, `{"error":"127.0.0.1 answered about another key`) {
				t.Fatalf("--json: %+v", r)
			}
		})
	}
}

// The warning's quotation of the box's key is cut at the cap, with the
// cut marked outside the quotes.
func TestWhoamiWarningQuotationIsCut(t *testing.T) {
	fb := newFakeBox(t)
	fb.whoami.Key = "SHA256:" + strings.Repeat("a", 300)
	r := runKey(t, fb, nil, "whoami")
	want := "warning  the box names another key than the one this command signed with (" + meFingerprint() + "): \"SHA256:" + strings.Repeat("a", 193) + "\"…\n"
	if r.code != 3 || r.stdout != want {
		t.Fatalf("exit %d\n%q\nwant\n%q", r.code, r.stdout, want)
	}
}
