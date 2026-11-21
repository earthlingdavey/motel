//go:build integration

package unit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/smallhoursorg/motel/backups/envfile"
)

// These run as root against a real system manager (make
// test-integration): every claim in the package doc is a measurement
// here, and none of them can be made of a fake.

// testUser runs what no test cares about the owner of.
const testUser = "motel-backup"

func runner(t *testing.T) *Runner {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root and a system manager")
	}
	ensureUser(t, "motel")
	ensureUser(t, testUser)
	r, err := NewSystemRunner(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func ensureUser(t *testing.T, name string) {
	t.Helper()
	if exec.Command("id", name).Run() == nil {
		return
	}
	if out, err := exec.Command("useradd", "--system", "--no-create-home", "--home-dir", "/nonexistent", "--shell", "/usr/sbin/nologin", name).CombinedOutput(); err != nil {
		t.Fatalf("useradd %s: %v: %s", name, err, out)
	}
}

func name(t *testing.T) string {
	n := strings.ToLower(strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()))
	return "motel_backup_test_" + n + ".service"
}

// outFile is somewhere the manager can write and no unit can see.
func outFile(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/root", "unit-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "stdout")
}

func activeState(name string) string {
	out, _ := exec.Command("systemctl", "show", "-p", "ActiveState", "--value", name).Output()
	return strings.TrimSpace(string(out))
}

func TestIntegrationExitStatusIsTheOutcomeNotAnError(t *testing.T) {
	r := runner(t)
	out, err := r.Run(context.Background(), Spec{Name: name(t), Argv: []string{"/bin/sh", "-c", "exit 12"}, User: testUser})
	if err != nil {
		t.Fatalf("a command that exits 12 is an outcome, got error %v", err)
	}
	if out.ExitStatus != 12 || out.Result != "exit-code" || out.OK() {
		t.Fatalf("outcome = %+v, want exit status 12, result exit-code", out)
	}
	if st := activeState(name(t)); st == "failed" {
		t.Fatalf("the unit is left in the failed state")
	}
	ok, err := r.Run(context.Background(), Spec{Name: name(t), Argv: []string{"/bin/true"}, User: testUser})
	if err != nil || !ok.OK() {
		t.Fatalf("a clean exit under the same name: %+v, %v", ok, err)
	}
}

func TestIntegrationArgvIsNeverExpanded(t *testing.T) {
	r := runner(t)
	stdout := outFile(t)
	out, err := r.Run(context.Background(), Spec{
		Name: name(t), Argv: []string{"/bin/echo", "${SECRET}", "$SECRET", "$$", "%h %n %%"}, User: testUser,
		Environment: []string{"SECRET=the-repository-password"}, StdoutFile: stdout,
	})
	if err != nil || !out.OK() {
		t.Fatalf("%+v, %v", out, err)
	}
	got, _ := os.ReadFile(stdout)
	if string(got) != "${SECRET} $SECRET $$ %h %n %%\n" {
		t.Fatalf("the command received %q", got)
	}
}

func TestIntegrationBindSourceIsAPathAndNothingElse(t *testing.T) {
	r := runner(t)
	dir := filepath.Join(t.TempDir(), "live swap:50%$HOME", "blog", "shared")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// TempDir is under /tmp, which PrivateTmp hides from the unit but
	// not from the manager resolving the bind source.
	must(t, os.WriteFile(filepath.Join(dir, "row"), []byte("row\n"), 0o644))
	for p := dir; p != "/tmp" && p != "/"; p = filepath.Dir(p) {
		must(t, os.Chmod(p, 0o755))
	}
	stdout := outFile(t)
	out, err := r.Run(context.Background(), Spec{
		Name: name(t), Argv: []string{"/bin/cat", "/backup/blog/files/row"}, User: testUser,
		Binds: []Bind{{Source: dir, Dest: "/backup/blog/files"}}, StdoutFile: stdout,
	})
	if err != nil || !out.OK() {
		t.Fatalf("%+v, %v", out, err)
	}
	if got, _ := os.ReadFile(stdout); string(got) != "row\n" {
		t.Fatalf("read %q through the bind", got)
	}
}

// For every way a Spec can say who runs the command: a property that
// quietly widens the view (DynamicUser= does) is found here.
func TestIntegrationViewIsWhatIsNamed(t *testing.T) {
	r := runner(t)
	// /run/systemd/incoming is the manager's own: the directory mounts
	// propagate into the namespace through, closed to the unit. Nothing
	// else of /run/systemd — the manager's sockets least of all — is
	// there unless a Spec names it.
	script := `for p in /etc/shadow /root /home /var/lib /etc/motel /run/dbus /run/systemd/private /run/systemd/journal; do [ -e "$p" ] && echo "present: $p"; done
ls /run/systemd | grep -v -x -e incoming -e resolve | sed 's/^/present: \/run\/systemd\//'; echo done`
	for who, spec := range map[string]Spec{
		"a user":                   {User: testUser},
		"a user with a capability": {User: testUser, Capabilities: []Capability{CapDACReadSearch}},
		"a user in its namespaces": {User: "motel", SameUIDNamespaces: true},
		"a user with the network":  {User: testUser, Network: true},
		"root with one capability": {AsRoot: true, Capabilities: []Capability{CapChown}},
	} {
		t.Run(who, func(t *testing.T) {
			spec.Name, spec.Argv, spec.StdoutFile = name(t), []string{"/bin/sh", "-c", script}, outFile(t)
			out, err := r.Run(context.Background(), spec)
			if err != nil || !out.OK() {
				t.Fatalf("%+v, %v", out, err)
			}
			got, _ := os.ReadFile(spec.StdoutFile)
			if string(got) != "done\n" {
				t.Fatalf("the view holds more than was named:\n%s", got)
			}
		})
	}
}

// A private network namespace here still holds lo and the kernel's
// fallback tunnel devices, so what is counted is routes: none, against
// the host's at least one.
func TestIntegrationNoNetworkUnlessAsked(t *testing.T) {
	r := runner(t)
	routes := func(network bool) string {
		stdout := outFile(t)
		n := strings.TrimSuffix(name(t), ".service") + map[bool]string{true: "_with", false: "_without"}[network] + ".service"
		out, err := r.Run(context.Background(), Spec{Name: n, Argv: []string{"/bin/sh", "-c", "tail -n +2 /proc/net/route | wc -l"}, User: testUser, Network: network, StdoutFile: stdout})
		if err != nil || !out.OK() {
			t.Fatalf("%+v, %v", out, err)
		}
		got, _ := os.ReadFile(stdout)
		return strings.TrimSpace(string(got))
	}
	if got := routes(false); got != "0" {
		t.Fatalf("a unit that did not ask for the network has %s route(s)", got)
	}
	if got := routes(true); got == "0" || got == "" {
		t.Fatalf("a unit that asked for the network has %q routes", got)
	}
}

// With the capability the upload unit has, and without: either way
// nothing of the masked file is read, and with it the file reads as
// empty rather than refusing — which is what restic meets.
func TestIntegrationMaskedPathsReadEmptyAndAMissingOneIsSkipped(t *testing.T) {
	r := runner(t)
	dir, _ := os.MkdirTemp("/root", "unit-mask-")
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	must(t, os.Chmod(dir, 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "app.db"), []byte("LIVE DATABASE BYTES"), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "a.png"), []byte("img"), 0o644))
	for who, caps := range map[string][]Capability{"with the read capability": {CapDACReadSearch}, "without it": nil} {
		t.Run(who, func(t *testing.T) {
			stdout := outFile(t)
			out, err := r.Run(context.Background(), Spec{
				Name: name(t), User: testUser, Capabilities: caps, StdoutFile: stdout,
				Argv:   []string{"/bin/sh", "-c", "cat /backup/x/files/app.db && echo read-it; echo \"|\"; cat /backup/x/files/a.png"},
				Binds:  []Bind{{Source: dir, Dest: "/backup/x/files"}},
				Masked: []string{"/backup/x/files/app.db", "/backup/x/files/app.db-wal"},
			})
			if err != nil || !out.OK() {
				t.Fatalf("%+v, %v", out, err)
			}
			want := "|\nimg"
			if caps != nil {
				want = "read-it\n|\nimg" // opened, and empty
			}
			if got, _ := os.ReadFile(stdout); string(got) != want {
				t.Fatalf("through the mask: %q, want %q", got, want)
			}
		})
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Fatalf("masking created something in the real directory: %v", entries)
	}
}

func TestIntegrationCapabilityReadsAnotherUsersFileAndWritesNothing(t *testing.T) {
	r := runner(t)
	ensureUser(t, "motel")
	ensureUser(t, "motel-backup")
	dir, _ := os.MkdirTemp("/root", "unit-cap-")
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if out, err := exec.Command("sh", "-c", "chmod 0755 "+dir+" && install -d -o motel -g motel -m 0700 "+dir+"/shared && install -o motel -g motel -m 0600 /dev/null "+dir+"/shared/secret && echo private > "+dir+"/shared/secret").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	spec := func(caps []Capability) Spec {
		return Spec{
			Name: name(t), User: "motel-backup", Capabilities: caps, StdoutFile: outFile(t),
			Argv:  []string{"/bin/sh", "-c", "cat /backup/x/files/secret; touch /backup/x/files/new 2>/dev/null && echo WROTE; true"},
			Binds: []Bind{{Source: dir + "/shared", Dest: "/backup/x/files"}},
		}
	}
	with := spec([]Capability{CapDACReadSearch})
	if out, err := r.Run(context.Background(), with); err != nil || !out.OK() {
		t.Fatalf("%+v, %v", out, err)
	}
	if got, _ := os.ReadFile(with.StdoutFile); string(got) != "private\n" {
		t.Fatalf("with the capability: %q", got)
	}
	without := spec(nil)
	if _, err := r.Run(context.Background(), without); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(without.StdoutFile); string(got) != "" {
		t.Fatalf("without the capability the file is still readable: %q", got)
	}
}

func TestIntegrationSameUIDNamespaces(t *testing.T) {
	r := runner(t)
	ensureUser(t, "motel")
	stdout := outFile(t)
	script := `read _ _ n < /proc/self/uid_map; echo "range=$n pid=$$"`
	out, err := r.Run(context.Background(), Spec{Name: name(t), Argv: []string{"/bin/sh", "-c", script}, User: "motel", SameUIDNamespaces: true, StdoutFile: stdout})
	if err != nil || !out.OK() {
		t.Fatalf("%+v, %v", out, err)
	}
	got, _ := os.ReadFile(stdout)
	if strings.Contains(string(got), "range=4294967295") || !strings.Contains(string(got), "pid=1\n") {
		t.Fatalf("not in its own user and PID namespaces: %q", got)
	}
}

// A real process that outlives the context, because only that shows
// what cancelling does: Run must not return until the unit is gone.
func TestIntegrationCancelStopsTheUnitAndConfirmsItGone(t *testing.T) {
	r := runner(t)
	ctx, cancel := context.WithCancel(context.Background())
	n := name(t)
	go func() {
		for activeState(n) != "activating" {
			time.Sleep(50 * time.Millisecond)
		}
		cancel()
	}()
	_, err := r.Run(ctx, Spec{Name: n, Argv: []string{"/bin/sleep", "600"}, User: testUser})
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrNotConfirmedGone) {
		t.Fatalf("want the context's error and a confirmed stop, got %v", err)
	}
	if st := activeState(n); st != "inactive" {
		t.Fatalf("after Run returned the unit is %q", st)
	}
	if out, _ := exec.Command("pgrep", "-f", "^/bin/sleep 600$").Output(); len(out) != 0 {
		t.Fatalf("the process is still running: %s", out)
	}
}

// As the command does it: one context, the signal's, for the connection
// and for the run. Cancelling it must not take away the connection the
// stop goes over.
func TestIntegrationARunnerOutlivesTheContextItWasMadeWith(t *testing.T) {
	runner(t) // the accounts, and the skip
	ctx, cancel := context.WithCancel(context.Background())
	r, err := NewSystemRunner(ctx)
	must(t, err)
	defer r.Close()
	n := name(t)
	go func() {
		for activeState(n) != "activating" {
			time.Sleep(50 * time.Millisecond)
		}
		cancel()
	}()
	_, err = r.Run(ctx, Spec{Name: n, Argv: []string{"/bin/sleep", "601"}, User: testUser})
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrNotConfirmedGone) {
		t.Fatalf("want the context's error and a confirmed stop, got %v", err)
	}
	if st := activeState(n); st != "inactive" {
		t.Fatalf("after Run returned the unit is %q", st)
	}
	// And the next unit — the one that removes plaintext — still starts.
	if o, err := r.Run(context.Background(), Spec{Name: name(t), Argv: []string{"/bin/true"}, User: testUser}); err != nil || !o.OK() {
		t.Fatalf("a unit after the cancel: %+v, %v", o, err)
	}
}

// The listing is believed only when restic had nothing to say beside
// it, so what a command writes to stderr has to reach the file named —
// all of it, none of its stdout, and with the manager taking the
// property as it is typed here.
func TestIntegrationStderrReachesTheFileNamed(t *testing.T) {
	r := runner(t)
	out := outFile(t)
	errFile := filepath.Join(filepath.Dir(out), "stderr")
	o, err := r.Run(context.Background(), Spec{
		Name: name(t), Argv: []string{"/bin/sh", "-c", "echo to-stdout; echo to-stderr >&2"}, User: testUser,
		StdoutFile: out, StderrFile: errFile,
	})
	if err != nil || !o.OK() {
		t.Fatalf("%+v, %v", o, err)
	}
	gotOut, err := os.ReadFile(out)
	must(t, err)
	gotErr, err := os.ReadFile(errFile)
	must(t, err)
	if string(gotOut) != "to-stdout\n" || string(gotErr) != "to-stderr\n" {
		t.Fatalf("stdout %q, stderr %q", gotOut, gotErr)
	}
}

// What a unit run as the data user writes on stderr is stored under
// SyslogIdentifier, not its command's name: the roles run
// /usr/bin/motel-backup, which journald would otherwise store as
// motel-backup, the identifier of an app named backup, from the uid
// every app runs as. The command here is a shell copied to that name.
// Its line carries the unit as the system manager's (_SYSTEMD_UNIT) and
// no user unit, which liveswap's logs read takes as no app's line.
func TestIntegrationStderrIsStoredUnderTheUnitsIdentifier(t *testing.T) {
	r := runner(t)
	self := "/usr/local/bin/motel-backup"
	raw, err := os.ReadFile("/bin/sh")
	must(t, err)
	must(t, os.WriteFile(self, raw, 0o755))
	t.Cleanup(func() { _ = os.Remove(self) })
	mark := fmt.Sprintf("role-stderr-%d", time.Now().UnixNano())
	o, err := r.Run(context.Background(), Spec{
		Name: name(t), Argv: []string{self, "-c", "echo " + mark + " >&2"}, User: "motel", SameUIDNamespaces: true,
	})
	if err != nil || !o.OK() {
		t.Fatalf("%+v, %v", o, err)
	}
	idOut, err := exec.Command("id", "-u", "motel").Output()
	must(t, err)
	uid := strings.TrimSpace(string(idOut))
	type entry struct {
		Ident    string `json:"SYSLOG_IDENTIFIER"`
		UID      string `json:"_UID"`
		Unit     string `json:"_SYSTEMD_UNIT"`
		UserUnit string `json:"_SYSTEMD_USER_UNIT"`
		Message  string `json:"MESSAGE"`
	}
	var got []entry
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		out, _ := exec.Command("journalctl", "--no-pager", "--quiet", "-o", "json", "--grep", mark).Output()
		got = got[:0]
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			var e entry
			if json.Unmarshal([]byte(line), &e) == nil && e.Message == mark {
				got = append(got, e)
			}
		}
		if len(got) > 0 || time.Now().After(deadline) {
			break
		}
	}
	if len(got) != 1 {
		t.Fatalf("the line in the journal: %+v", got)
	}
	e := got[0]
	t.Logf("a data user's unit's stderr line: SYSLOG_IDENTIFIER=%q _UID=%s _SYSTEMD_UNIT=%q _SYSTEMD_USER_UNIT=%q", e.Ident, e.UID, e.Unit, e.UserUnit)
	if e.Ident != SyslogIdentifier || e.UID != uid || e.Unit != name(t) || e.UserUnit != "" {
		t.Errorf("stored as %+v, want under %s from uid %s, the unit %s and no user unit", e, SyslogIdentifier, uid, name(t))
	}
}

// A oneshot whose command is still running is "activating" — never
// "active", and not failed — and the manager says since when.
func TestIntegrationListActiveSaysWhatIsRunningAndSinceWhen(t *testing.T) {
	r := runner(t)
	n, before := name(t), time.Now().Add(-time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = r.Run(ctx, Spec{Name: n, Argv: []string{"/bin/sleep", "602"}, User: testUser})
	}()
	waitFor(t, n, "activating")
	got, err := ListActive(context.Background(), "motel_backup_test_*")
	must(t, err)
	found := false
	for _, a := range got {
		if a.Name == n {
			found = true
			if a.State != "activating" || a.Since.Before(before) || a.Since.After(time.Now()) {
				t.Errorf("%+v, started after %v", a, before)
			}
		}
	}
	if !found {
		t.Errorf("%s is running and was not listed: %+v", n, got)
	}
	cancel()
	<-done
	got, err = ListActive(context.Background(), "motel_backup_test_*")
	must(t, err)
	for _, a := range got {
		if a.Name == n {
			t.Errorf("stopped, and still listed: %+v", a)
		}
	}
}

func waitFor(t *testing.T, unit, state string) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); activeState(unit) != state; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s is %q, not %q, after 20s", unit, activeState(unit), state)
		}
	}
}

// The orchestrator dies without a chance to clean up; the manager ends
// what it started, and Run says the command gave no verdict.
func TestIntegrationBindsToEndsTheUnitWhenItsOrchestratorIsKilled(t *testing.T) {
	r := runner(t)
	orch := "motel_backup_test_orchestrator.service"
	t.Cleanup(func() { _ = exec.Command("systemctl", "stop", orch).Run() })
	go func() {
		_, _ = r.Run(context.Background(), Spec{Name: orch, Argv: []string{"/bin/sleep", "601"}, User: testUser})
	}()
	waitFor(t, orch, "activating")
	n := name(t)
	t.Cleanup(func() { _ = exec.Command("systemctl", "stop", n).Run() })
	type result struct {
		out Outcome
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := r.Run(context.Background(), Spec{Name: n, Argv: []string{"/bin/sleep", "602"}, User: testUser, BindsTo: orch})
		done <- result{out, err}
	}()
	waitFor(t, n, "activating")
	if out, err := exec.Command("systemctl", "kill", "--signal=SIGKILL", orch).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	select {
	case res := <-done:
		if res.err == nil || !strings.Contains(res.err.Error(), "ended from outside") || !errors.Is(res.err, ErrEndedFromOutside) {
			t.Fatalf("want an error saying the unit was ended from outside, got %+v, %v", res.out, res.err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the bound unit is still running 30s after its orchestrator was killed")
	}
	// Run has returned, so the process is gone — not about to be.
	if out, _ := exec.Command("pgrep", "-f", "^/bin/sleep 602$").Output(); len(out) != 0 {
		t.Fatalf("the process is still running: %s", out)
	}
}

// The credential file is written by setup and read by the manager, and
// the two have to agree on what a line means: envfile.Parse is what
// status lints a hand-edited file with. This is the
// measurement Parse is held to [M41]: one file with every shape a hand
// might write, a unit that prints its environment, and Parse of the
// same bytes.
func TestIntegrationSystemdReadsAnEnvFileAsParseDoes(t *testing.T) {
	r := runner(t)
	stdout := outFile(t)
	raw := strings.Join([]string{
		`PLAIN=value`,
		` SPACEKEY = spaced `,
		`DQ="double quoted"`,
		`SQ='single quoted'`,
		`DUP=first`,
		`DUP=second`,
		`# COMMENT=no`,
		`; SEMI=no`,
		`HASHIN=a#b`,
		`TRAIL=trail   `,
		`BS=a\b\\c`,
		`DQBS="a\b\\c\"d"`,
		`DOLLAR=$HOME x`,
		`CONT=one \`,
		`two`,
		`EMPTY=`,
		`NOEQ`,
		`MIDQ=ab"cd"ef`,
		`TAB=a	b`,
		`SEMIIN=a;b`,
		`PCT=100%s`,
		`UTF=héllo`,
		`MULTI="one`,
		`two"`,
		`QLEAD="\"quoted\""`,
		`QPAD="  both  "`,
		`QTAB="	tab"`,
		`QSQ="it's"`,
		`SQLEAD='"q'`,
		`AFTERQ="a" b`,
		`AFTERC="v" # prod`,
		`DQDOLLAR="a\$b"`,
		"DQBT=\"a\\`b\"",
		`SQESC='it\'s'`,
		`ESCSP=trail\ `,
		`export EXP=1`,
		`ODD=abc\\\`,
		`JOINED=yes`,
		`EVEN=abc\\`,
		`NOTJOINED=yes`,
		`SQCONT='a\`,
		`b'`,
		`DQCONT="a\`,
		`b"`,
		`AFTERQC="v"\`,
		`NEXT=1`,
		`AFTERQS="v" x\`,
		`NEXT2=2`,
		"NBSPV=bucket\u00a0",
		"\u00a0NBSPK=v",
		`REQ="a" "b"`,
		`SQIDIOM='it'"'"'s'`,
		"CRCONT=one \\\r",
		`CRB=2`,
		"CRMID=x\ry",
		"CRQ=\"a\rb\"",
		"  # an indented comment",
		`AFTERIC=1`,
		`LEADQ="abc`,
		`SWALLOWED=yes`,
		`EOFBS=abc\`,
	}, "\n")
	file := filepath.Join(filepath.Dir(stdout), "test.env")
	must(t, os.WriteFile(file, []byte(raw), 0o600))
	unit := envOfUnit(t, r, file, stdout)
	parsed, findings := envfile.Parse([]byte(raw))
	for k, v := range parsed {
		if u, ok := unit[k]; !ok || u != v {
			t.Errorf("%s: Parse reads %q, the manager gives the unit %q (present: %v)", k, v, u, ok)
		}
	}
	for _, k := range []string{"COMMENT", "SEMI", "NOEQ", "two", "SWALLOWED", "EXP", "NEXT", "NEXT2", "NBSPK", "\u00a0NBSPK", "y", "EOFBS"} {
		if _, ok := unit[k]; ok {
			t.Errorf("the manager gave the unit %s, which Parse skips", k)
		}
	}
	// Every key the file sets that the manager passes on, Parse reads.
	for k := range unit {
		if _, ok := parsed[k]; !ok && strings.Contains(raw, "\n"+k+"=") {
			t.Errorf("the manager gave the unit %s=%q, which Parse did not read", k, unit[k])
		}
	}
	if len(findings) != 7 {
		t.Errorf("findings: %q", findings)
	}
	// A backslash as the last byte of the file, outside quotes: dropped,
	// and the space before it kept.
	must(t, os.WriteFile(file, []byte("A=x\nB=abc \\"), 0o600))
	unit = envOfUnit(t, r, file, stdout)
	if bs, _ := envfile.Parse([]byte("A=x\nB=abc \\")); unit["B"] != "abc " || bs["B"] != unit["B"] {
		t.Errorf("a backslash at the end of the file: the manager gives %q, Parse reads %q", unit["B"], bs["B"])
	}
	// The bytes the manager refuses a whole file for, and the ones it
	// does not [M91]: Parse says the one where the manager does the
	// other, and a run refuses before any unit what Parse says it
	// refuses (envfile.RefusedWhole).
	for _, raw := range []string{
		"OK=1\nBAD=caf\xe9\nAFTER=2\n",
		"OK=1\nBAD=\"caf\xe9\"\nAFTER=2\n",
		"OK=1\ncaf\xe9=1\nAFTER=2\n",
		"OK=1\n# a\x00b\nAFTER=2\n",
		"OK=1\nBAD=a\x00b\nAFTER=2\n",
		"OK=1\n# caf\xe9\nAFTER=2\n",
		"OK=1\ncaf\xe9\nAFTER=2\n",
	} {
		must(t, os.WriteFile(file, []byte(raw), 0o600))
		out, err := r.Run(context.Background(), Spec{Name: name(t), Argv: []string{"/usr/bin/env", "-0"}, User: testUser, EnvironmentFile: file, StdoutFile: stdout})
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		_, findings := envfile.Parse([]byte(raw))
		if refused, said := out.Result == "resources", envfile.RefusedWhole(findings) != ""; refused != said {
			t.Errorf("%q: the manager's result %q; Parse says refused whole: %v (%q)", raw, out.Result, said, findings)
		}
	}
	// And the other way: what the writer writes of awkward values, the
	// manager reads back as they were.
	pairs := []envfile.Pair{{Key: "PLAIN", Value: "s3:https://h/b"}, {Key: "PW", Value: `p#a$s;s"w'o=rd\x`}, {Key: "QLEAD", Value: `"quoted"`},
		{Key: "SQLEAD", Value: `'q`}, {Key: "QPAD", Value: "  both  "}, {Key: "QTRAIL", Value: `trail\ `}, {Key: "UTF", Value: "héllo"}}
	written, err := envfile.Format(pairs)
	must(t, err)
	must(t, os.WriteFile(file, written, 0o600))
	unit = envOfUnit(t, r, file, stdout)
	for _, p := range pairs {
		if unit[p.Key] != p.Value {
			t.Errorf("%s: Format wrote %q as %q, the manager gives the unit %q", p.Key, p.Value, written, unit[p.Key])
		}
	}
}

// envOfUnit is the environment a unit is given from file.
func envOfUnit(t *testing.T, r *Runner, file, stdout string) map[string]string {
	t.Helper()
	out, err := r.Run(context.Background(), Spec{
		Name: name(t), Argv: []string{"/usr/bin/env", "-0"}, User: testUser,
		EnvironmentFile: file, StdoutFile: stdout,
	})
	if err != nil || !out.OK() {
		t.Fatalf("%+v, %v", out, err)
	}
	got, _ := os.ReadFile(stdout)
	unit := map[string]string{}
	for _, kv := range strings.Split(string(got), "\x00") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			unit[k] = v
		}
	}
	return unit
}

// Wait is what a lock holder does about a restic init an earlier setup
// left to finish: it returns once the unit has ended on its own, reaps
// it, and takes a unit that is not there as ended.
func TestIntegrationWaitReturnsOnceTheUnitEndsOnItsOwn(t *testing.T) {
	r := runner(t)
	done := make(chan Outcome, 1)
	go func() {
		out, _ := r.Run(context.Background(), Spec{Name: name(t), Argv: []string{"/bin/sleep", "2"}, User: testUser})
		done <- out
	}()
	for activeState(name(t)) != "activating" {
		time.Sleep(50 * time.Millisecond)
	}
	start := time.Now()
	if err := r.Wait(context.Background(), name(t)); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if took := time.Since(start); took < time.Second || took > 10*time.Second {
		t.Fatalf("Wait returned after %s of a 2 s unit", took)
	}
	if out := <-done; !out.OK() {
		t.Fatalf("the unit Wait watched: %+v", out)
	}
	if err := r.Wait(context.Background(), "motel_backup_test_never_started.service"); err != nil {
		t.Fatalf("Wait on a unit that is not there: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	go func() {
		_, _ = r.Run(context.Background(), Spec{Name: name(t), Argv: []string{"/bin/sleep", "3"}, User: testUser})
	}()
	for activeState(name(t)) != "activating" {
		time.Sleep(50 * time.Millisecond)
	}
	if err := r.Wait(ctx, name(t)); err == nil || !strings.Contains(err.Error(), "still activating") {
		t.Fatalf("Wait past its context: %v", err)
	}
	must(t, r.Stop(name(t)))
}

// Scheduled is what setup asks of the package's timer before saying
// what runs next: running of a unit whose command is running, not
// running of one that has ended, and neither running nor enabled — not
// an error — of a name the manager has never loaded, which is every
// box without the package's units. A transient unit has no unit file
// to be enabled by.
func TestIntegrationScheduledSaysWhetherAUnitIs(t *testing.T) {
	r := runner(t)
	done := make(chan Outcome, 1)
	go func() {
		out, _ := r.Run(context.Background(), Spec{Name: name(t), Argv: []string{"/bin/sleep", "2"}, User: testUser})
		done <- out
	}()
	for i := 0; activeState(name(t)) != "activating"; i++ {
		if i >= 200 {
			t.Fatalf("the unit never came up: %s", activeState(name(t)))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if active, enabled, err := r.Scheduled(context.Background(), name(t)); err != nil || !active || enabled {
		t.Fatalf("a running transient unit: active=%v enabled=%v, err=%v", active, enabled, err)
	}
	<-done
	if active, _, err := r.Scheduled(context.Background(), name(t)); err != nil || active {
		t.Fatalf("an ended unit: active=%v, err=%v", active, err)
	}
	if active, enabled, err := r.Scheduled(context.Background(), "motel_backup_never_loaded_0123456789ab.timer"); err != nil || active || enabled {
		t.Fatalf("a unit that was never loaded: active=%v enabled=%v, err=%v", active, enabled, err)
	}
}

// Sees is how a command knows that the manager can bind its mounts
// into a unit: the manager keeps a mount unit for every mount of its
// own namespace, and none for one made in another [M72]. A mount made
// here, in the manager's namespace, is seen at once; the same path
// with no mount on it is not, after the wait; and the unit's name is
// the one systemd-escape gives the path.
func TestIntegrationSeesAMountOfTheManagersNamespace(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root and mount(2)")
	}
	r := runner(t)
	dir, err := os.MkdirTemp("/run", "sees a-mount.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(dir) })
	escaped, err := exec.Command("systemd-escape", "--path", "--suffix=mount", dir).Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := MountUnit(dir); got != strings.TrimSpace(string(escaped)) {
		t.Fatalf("MountUnit(%q) = %q, systemd-escape says %q", dir, got, strings.TrimSpace(string(escaped)))
	}
	began := time.Now()
	if seen, err := r.Sees(context.Background(), dir); err != nil || seen {
		t.Fatalf("a directory with no mount on it: seen=%v, err=%v", seen, err)
	}
	if waited := time.Since(began); waited < seesWithin {
		t.Fatalf("a mount that is not there was given up on after %s, before the manager had %s to read it", waited, seesWithin)
	}
	if out, err := exec.Command("mount", "--bind", dir, dir).CombinedOutput(); err != nil {
		t.Fatalf("mount --bind: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("umount", "-l", dir).Run() })
	began = time.Now()
	if seen, err := r.Sees(context.Background(), dir); err != nil || !seen {
		t.Fatalf("a mount made in the manager's namespace: seen=%v, err=%v", seen, err)
	}
	if waited := time.Since(began); waited > 2*time.Second {
		t.Errorf("the manager took %s to see a mount of its own namespace", waited)
	}
	// And one made where the manager does not look: in a mount
	// namespace of its own, as a service with PrivateMounts= has.
	other, err := os.MkdirTemp("/run", "sees-elsewhere.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(other) })
	held := exec.Command("unshare", "--mount", "--propagation", "private", "sh", "-c", "mount --bind \"$0\" \"$0\" && echo mounted && exec sleep 30", other)
	out, err := held.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := held.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Process.Kill(); _ = held.Wait() })
	line := make([]byte, 8)
	if n, _ := out.Read(line); !strings.HasPrefix(string(line[:n]), "mounted") {
		t.Fatalf("the mount in a namespace of its own was not made: %q", line[:n])
	}
	if seen, err := r.Sees(context.Background(), other); err != nil || seen {
		t.Fatalf("a mount made in a mount namespace of its own: seen=%v, err=%v", seen, err)
	}
}

// probeEnv, set, makes this test binary a probe a unit runs (TestMain).
const probeEnv = "MOTEL_BACKUP_UNIT_PROBE"

func TestMain(m *testing.M) {
	if os.Getenv(probeEnv) == "open_by_handle_at" {
		capEff := ""
		status, _ := os.ReadFile("/proc/self/status")
		for _, line := range strings.Split(string(status), "\n") {
			if v, ok := strings.CutPrefix(line, "CapEff:"); ok {
				capEff = strings.TrimSpace(v)
			}
		}
		var errno unix.Errno
		errors.As(openByHandleAt(), &errno)
		fmt.Println(capEff, int(errno))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// openByHandleAt asks for a file by a handle that names none: who
// answers is what is asked.
func openByHandleAt() error {
	root, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer unix.Close(root) //nolint:errcheck // read-only
	fd, err := unix.OpenByHandleAt(root, unix.NewFileHandle(1, make([]byte, 8)), unix.O_RDONLY)
	if err == nil {
		_ = unix.Close(fd)
	}
	return err
}

// A unit that holds CAP_DAC_READ_SEARCH is denied open_by_handle_at,
// through which that capability reads any file of a filesystem by its
// handle, whatever the view [M34]. That the property is sent is
// TestOpenByHandleAtIsDeniedExactlyWhereTheCapabilityIsHeld; this is the
// manager's filter answering it. A handle that names nothing tells the
// two apart: to a caller that holds the capability the kernel says
// ESTALE, and the filter says EPERM.
func TestIntegrationTheFilterDeniesOpenByHandleAtWhereTheCapabilityIsHeld(t *testing.T) {
	r := runner(t)
	if err := openByHandleAt(); !errors.Is(err, unix.ESTALE) {
		t.Fatalf("fixture: the kernel answered this process %v, not ESTALE, so its answer cannot be told from the filter's", err)
	}
	self, err := os.Executable()
	must(t, err)
	raw, err := os.ReadFile(self)
	must(t, err)
	// Where the unit's view holds it.
	probe := "/usr/local/bin/motel-backup-unit-probe"
	must(t, os.WriteFile(probe, raw, 0o755))
	t.Cleanup(func() { _ = os.Remove(probe) })
	stdout := outFile(t)
	out, err := r.Run(context.Background(), Spec{
		Name: name(t), Argv: []string{probe}, User: testUser, Capabilities: []Capability{CapDACReadSearch},
		Environment: []string{probeEnv + "=open_by_handle_at"}, StdoutFile: stdout,
	})
	if err != nil || !out.OK() {
		t.Fatalf("%+v, %v", out, err)
	}
	said, _ := os.ReadFile(stdout)
	var capEff uint64
	var errno int
	if _, err := fmt.Sscanf(string(said), "%x %d", &capEff, &errno); err != nil {
		t.Fatalf("the probe said %q: %v", said, err)
	}
	if capEff&(1<<unix.CAP_DAC_READ_SEARCH) == 0 {
		t.Fatalf("the unit does not hold CAP_DAC_READ_SEARCH (CapEff %x): the test proves nothing", capEff)
	}
	if unix.Errno(errno) != unix.EPERM {
		t.Errorf("open_by_handle_at in a unit that holds CAP_DAC_READ_SEARCH: %v, want EPERM, the filter's", unix.Errno(errno))
	}
}
