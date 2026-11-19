package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/client/internal/client"
	"github.com/smallhoursorg/motel/client/internal/local"
	"github.com/smallhoursorg/motel/client/internal/sshkey"
	"github.com/smallhoursorg/motel/client/internal/tui"
	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	tuihostile "github.com/smallhoursorg/motel/client/internal/tui/hostile"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// recorder is model.Client that says which method a read called.
type recorder struct{ called string }

func (r *recorder) WhoAmI(context.Context) (model.Got[wire.WhoAmI], error) {
	r.called = "WhoAmI"
	return model.Got[wire.WhoAmI]{}, nil
}

func (r *recorder) Apps(context.Context) (model.Got[[]string], error) {
	r.called = "Apps"
	return model.Got[[]string]{}, nil
}

func (r *recorder) Status(context.Context, string) (model.Got[wire.Status], error) {
	r.called = "Status"
	return model.Got[wire.Status]{}, nil
}

func (r *recorder) Deploy(context.Context, string, string) (model.Got[wire.DeployResult], error) {
	r.called = "Deploy"
	return model.Got[wire.DeployResult]{}, nil
}

func (r *recorder) BoxStatus(context.Context) (model.Got[wire.BoxStatus], error) {
	r.called = "BoxStatus"
	return model.Got[wire.BoxStatus]{}, nil
}

// wantRead is the read each command makes, written out: the command
// an action names, and the model.Client method its read must call,
// which live maps to the client's read of the same name.
var wantRead = map[string]string{
	"whoami":                  "WhoAmI",
	"apps":                    "Apps",
	"status <app>":            "Status",
	"deploys <app> <version>": "Deploy",
	"box status":              "BoxStatus",
}

// TestActionsAreCommands: every read the TUI makes is an action naming
// a command that exists and takes the arguments it names; each
// action's read calls that command's method, as written out above; and
// every command above has its action.
func TestActionsAreCommands(t *testing.T) {
	root := (&runner{}).tree()
	seen := map[string]bool{}
	for _, a := range model.Actions {
		words := strings.Fields(a.Command)
		var path, args []string
		for _, w := range words {
			if strings.HasPrefix(w, "<") {
				args = append(args, w)
			} else {
				path = append(path, w)
			}
		}
		cmd, rest, err := root.Find(path)
		if err != nil || len(rest) != 0 || cmd == root {
			t.Errorf("%q: no command %q", a.Command, strings.Join(path, " "))
			continue
		}
		if err := cmd.Args(cmd, args); err != nil {
			t.Errorf("%q: %s does not take %v: %v", a.Command, cmd.CommandPath(), args, err)
		}
		want, ok := wantRead[a.Command]
		if !ok {
			t.Errorf("an action names %q, which is not a read written out here", a.Command)
			continue
		}
		seen[a.Command] = true
		rec := &recorder{}
		if err := a.Read(context.Background(), rec, model.Job{Kind: a.Kind, App: "blog", Version: "v1"}); err != nil {
			t.Fatal(err)
		}
		if rec.called != want {
			t.Errorf("%q reads with %s, want %s", a.Command, rec.called, want)
		}
		if b, ok := model.ActionFor(a.Kind); !ok || b.Command != a.Command {
			t.Errorf("kind %d finds %q, want %q", a.Kind, b.Command, a.Command)
		}
	}
	for c := range wantRead {
		if !seen[c] {
			t.Errorf("no action reads as %q does", c)
		}
	}
}

// TestForTUI: the TUI's client, and only it, signs tokens of the
// longest life a box takes and hands a 429 back; no other file of the
// module sets either.
func TestForTUI(t *testing.T) {
	kc := &client.Client{Cred: &client.PersonToken{Signer: sshkey.Keygen{}}}
	forTUI(kc, time.Now)
	if k, ok := kc.Cred.(*client.PersonToken).Signer.(*signer).inner.(sshkey.Keygen); !ok || !k.NoTerminal {
		t.Errorf("the TUI's ssh-keygen may reach the terminal: %#v", kc.Cred.(*client.PersonToken).Signer)
	}
	c := &client.Client{Cred: &client.PersonToken{Signer: &fixedSigner{}}}
	forTUI(c, time.Now)
	pt := c.Cred.(*client.PersonToken)
	if pt.Life != sshkey.MaxLife || !c.NoWait {
		t.Errorf("life %s, NoWait %v", pt.Life, c.NoWait)
	}
	if _, ok := pt.Signer.(*signer); !ok {
		t.Errorf("the signer does not tell the screen")
	}
	files, _ := filepath.Glob("../../*.go")
	more, _ := filepath.Glob("../*/*.go")
	deeper, _ := filepath.Glob("../*/*/*.go")
	for _, name := range append(append(files, more...), deeper...) {
		if strings.HasSuffix(name, "_test.go") || strings.HasSuffix(name, filepath.Join("cli", "tui.go")) {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			// Each field set, with its value: Life set to anything, and
			// NoWait or NoTerminal set to true, are the TUI's client's.
			type set struct {
				name string
				val  ast.Expr
			}
			var sets []set
			switch n := n.(type) {
			case *ast.AssignStmt:
				for i, l := range n.Lhs {
					if s, ok := l.(*ast.SelectorExpr); ok && i < len(n.Rhs) {
						sets = append(sets, set{s.Sel.Name, n.Rhs[i]})
					}
				}
			case *ast.KeyValueExpr:
				if k, ok := n.Key.(*ast.Ident); ok {
					sets = append(sets, set{k.Name, n.Value})
				}
			}
			for _, st := range sets {
				isTrue := false
				if id, ok := st.val.(*ast.Ident); ok && id.Name == "true" {
					isTrue = true
				}
				if st.name == "Life" || (st.name == "NoWait" || st.name == "NoTerminal") && isTrue {
					t.Errorf("%s sets %s: only cli/tui.go builds the TUI's client", name, st.name)
				}
			}
			return true
		})
	}
}

// TestFailureForTheTUI: what each client failure means to the engine.
func TestFailureForTheTUI(t *testing.T) {
	for _, c := range []struct {
		f    *client.Failure
		want model.Fail
	}{
		{&client.Failure{Exit: client.ExitRefused, Status: 401}, model.FailSignedOut},
		{&client.Failure{Exit: client.ExitCertificate}, model.FailCertificate},
		{&client.Failure{Exit: client.ExitUsage}, model.FailNotSigned},
		{&client.Failure{Exit: client.ExitUnknown, Status: 429, RetryAfter: 7 * time.Second}, model.FailSlowed},
		{&client.Failure{Exit: client.ExitForbidden, Status: 403}, model.FailForbidden},
		{&client.Failure{Exit: client.ExitNotFound, Status: 404}, model.FailNotFound},
		{&client.Failure{Exit: client.ExitUnknown}, model.FailNoAnswer},
		{&client.Failure{Exit: client.ExitUnknown, Status: 500}, model.FailNoAnswer},
		{&client.Failure{Exit: client.ExitUnknown, Status: 599}, model.FailNoAnswer},
		{&client.Failure{Exit: client.ExitUnknown, Status: 499}, model.FailUnusable},
		{&client.Failure{Exit: client.ExitUnknown, Status: 600}, model.FailUnusable},
		{&client.Failure{Exit: client.ExitUnknown, Status: 200}, model.FailUnusable},
		{&client.Failure{Exit: client.ExitFailed, Status: 409}, model.FailThis},
	} {
		var m *model.Failure
		if !errors.As(failure(c.f), &m) || m.Kind != c.want || m.RetryAfter != c.f.RetryAfter {
			t.Errorf("%+v: %+v, want kind %d", c.f, m, c.want)
		}
	}
	if err := failure(context.Canceled); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled read is %v", err)
	}
}

// TestHiddenVersionFailsItsReadAlone: a deploy whose version the box's
// filter hid is listed in the client's own words. Opening it sends
// nothing (the client here is nil) and fails that read alone, where
// the usage failure of a version that is no version would stop every
// read as a token not signed.
func TestHiddenVersionFailsItsReadAlone(t *testing.T) {
	_, err := live{}.Deploy(context.Background(), "blog", client.Filtered)
	var m *model.Failure
	if !errors.As(err, &m) || m.Kind != model.FailNotSent || !strings.Contains(m.Message, "filter hid this deploy's version") {
		t.Errorf("a record asked by a hidden version: %+v (%v), want this read alone to fail", m, err)
	}
}

// TestTUIRefusesAPassphraseKey: a key file that needs a passphrase the
// agent does not hold is refused before anything is sent, exit 2.
func TestTUIRefusesAPassphraseKey(t *testing.T) {
	fb := newFakeBox(t)
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "me.pub"), []byte(meLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	enc := pem.EncodeToMemory(&pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: []byte("openssh-key-v1\x00\x00\x00\x00\x0aaes256-ctr")})
	if err := os.WriteFile(filepath.Join(home, "me"), enc, 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"MOTEL_KEY": "~/me", "MOTEL_URL": fb.srv.URL + "/blog"}
	var out, errOut bytes.Buffer
	sent := 0
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		sent++
		return fb.srv.Client().Transport.RoundTrip(r)
	})
	code := Main(context.Background(), nil, Env{Terminal: true,
		Getenv: func(k string) string { return env[k] }, Stdout: &out, Stderr: &errOut, Dir: t.TempDir(), Home: home,
		Now: time.Now, Sleep: func(context.Context, time.Duration) error { return nil },
		Transport: rt, Run: noPrograms{}, Signer: &fixedSigner{}, Version: "test",
	})
	if code != client.ExitUsage || !strings.Contains(errOut.String(), "ssh-add ~/me") {
		t.Errorf("exit %d, stderr %q", code, errOut.String())
	}
	if sent != 0 {
		t.Errorf("%d requests sent", sent)
	}
}

// TestDemoHidden: --demo is not in the help, and it runs with no box.
func TestDemoHidden(t *testing.T) {
	r := run(t, nil, nil, "--help")
	if r.code != 0 || strings.Contains(r.stdout, "demo") {
		t.Errorf("exit %d; help shows --demo:\n%s", r.code, r.stdout)
	}
}

// TestInterruptedAtTheFirstRead: an interrupt before the screen opens
// ends the command, with no screen opened over nothing.
func TestInterruptedAtTheFirstRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, errOut bytes.Buffer
	code := Main(ctx, []string{"--demo"}, Env{
		Getenv: func(string) string { return "" }, Stdout: &out, Stderr: &errOut, Dir: t.TempDir(), Home: t.TempDir(),
		Now: time.Now, Sleep: func(context.Context, time.Duration) error { return nil }, Run: noPrograms{}, Version: "test",
	})
	if code != client.ExitInterrupted || strings.Contains(errOut.String(), "screen") {
		t.Errorf("exit %d, stderr %q", code, errOut.String())
	}
}

// hangUp runs motel on the fake box and sends this process SIGHUP,
// as a closed terminal window does, at the TUI's read number at
// (from 1), which then waits to be cancelled; and says the exit code
// and what stderr holds.
func hangUp(t *testing.T, at int32) (int, string) {
	t.Helper()
	fb := newFakeBox(t)
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "me.pub"), []byte(meLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"MOTEL_KEY": "~/me.pub", "MOTEL_URL": fb.srv.URL + "/blog"}
	var n atomic.Int32
	var once sync.Once
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if n.Add(1) < at {
			return fb.srv.Client().Transport.RoundTrip(r)
		}
		once.Do(func() { _ = syscall.Kill(os.Getpid(), syscall.SIGHUP) })
		select {
		case <-r.Context().Done():
			return nil, r.Context().Err()
		case <-time.After(20 * time.Second): // a guard: the signal ended nothing
			t.Error("SIGHUP did not end the read")
			return nil, context.DeadlineExceeded
		}
	})
	var out, errOut bytes.Buffer
	code := Main(context.Background(), nil, Env{Terminal: true,
		Getenv: func(k string) string { return env[k] }, Stdout: &out, Stderr: &errOut, Dir: t.TempDir(), Home: home,
		Now: time.Now, Sleep: func(context.Context, time.Duration) error { return nil },
		Transport: rt, Run: noPrograms{}, Signer: &fixedSigner{}, Version: "test",
	})
	return code, errOut.String()
}

// TestHangUpAtTheFirstRead: SIGHUP during the first read ends motel
// as an interrupt does: exit 130, nothing said.
func TestHangUpAtTheFirstRead(t *testing.T) {
	if code, stderr := hangUp(t, 1); code != client.ExitInterrupted || stderr != "" {
		t.Errorf("exit %d, stderr %q; want %d and nothing said", code, stderr, client.ExitInterrupted)
	}
}

// endAt is local.Runner that, when the program named runs, ends the
// command as end does, once, and waits for the command's context to
// end, as a program a signal stopped; every other program is not
// installed.
type endAt struct {
	name string
	end  func()
	once *sync.Once
}

func (e endAt) Output(ctx context.Context, c local.Cmd) ([]byte, error) {
	if c.Name != e.name {
		return nil, exec.ErrNotFound
	}
	e.once.Do(e.end)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(20 * time.Second): // a guard: the signal ended nothing
		return nil, errors.New("the signal ended nothing")
	}
}

// TestSignalBeforeTheScreen: a closed terminal (SIGHUP) or an interrupt
// (Main's context ends, as SIGINT and SIGTERM end it) while motel
// sets up (the box found from the repository, the key from the agent,
// the passphrase check) ends it as an interrupt does: exit 130, nothing
// said, not the advice a failed step would give.
func TestSignalBeforeTheScreen(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "me.pub"), []byte(meLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	enc := pem.EncodeToMemory(&pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: []byte("openssh-key-v1\x00\x00\x00\x00\x0aaes256-ctr")})
	if err := os.WriteFile(filepath.Join(home, "me"), enc, 0o600); err != nil {
		t.Fatal(err)
	}
	url := newFakeBox(t).srv.URL + "/blog"
	steps := []struct {
		name, program string
		env           map[string]string
	}{
		{"the box", "git", map[string]string{"MOTEL_KEY": "~/me.pub"}},
		{"the key", "ssh-add", map[string]string{"MOTEL_URL": url}},
		{"the passphrase check", "ssh-add", map[string]string{"MOTEL_KEY": "~/me", "MOTEL_URL": url}},
	}
	for _, s := range steps {
		for _, sig := range []string{"SIGHUP", "interrupt"} {
			t.Run(s.name+"/"+sig, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				end := cancel
				if sig == "SIGHUP" {
					end = func() { _ = syscall.Kill(os.Getpid(), syscall.SIGHUP) }
				}
				var out, errOut bytes.Buffer
				code := Main(ctx, nil, Env{Terminal: true,
					Getenv: func(k string) string { return s.env[k] }, Stdout: &out, Stderr: &errOut, Dir: t.TempDir(), Home: home,
					Now: time.Now, Sleep: func(context.Context, time.Duration) error { return nil },
					Run: endAt{name: s.program, end: end, once: &sync.Once{}}, Signer: &fixedSigner{}, Version: "test",
				})
				if code != client.ExitInterrupted || errOut.String() != "" {
					t.Errorf("exit %d, stderr %q; want %d and nothing said", code, errOut.String(), client.ExitInterrupted)
				}
			})
		}
	}
}

// TestHangUpHelper is not a test of its own: TestHangUpWithTheScreenUp
// runs the test binary with it under a pty, where the screen can open.
// Its third read is the first the screens make (whoami and the list
// are read before).
func TestHangUpHelper(t *testing.T) {
	if os.Getenv("MOTEL_TEST_HANGUP_HELPER") == "" {
		t.Skip("run by TestHangUpWithTheScreenUp")
	}
	code, stderr := hangUp(t, 3)
	_, _ = os.Stdout.WriteString("RESULT exit " + strconv.Itoa(code) + ", stderr " + strconv.Quote(stderr) + "\n")
}

// ptyScript is util-linux's script(1), which these tests run as
// script -qec <command> /dev/null to make a pty; the test is skipped,
// saying what is not held, where there is none. Another script (BSD's,
// on macOS, or busybox's) takes neither -e nor -c so: util-linux's
// alone says so to --version.
func ptyScript(t *testing.T, notHeld string) string {
	t.Helper()
	script, err := exec.LookPath("script")
	if err != nil {
		t.Skip("NOT RUN: no script(1) to make a pty with; " + notHeld)
	}
	if v, err := exec.Command(script, "--version").Output(); err != nil || !strings.Contains(string(v), "util-linux") {
		t.Skip("NOT RUN: " + script + " is not util-linux's script(1), whose -qec makes the pty these tests use; " + notHeld)
	}
	return script
}

// TestHangUpWithTheScreenUp: SIGHUP while the screen is up ends motel
// as an interrupt does: exit 130, nothing said. Under a real pty
// (script(1) makes one), as the screen needs a terminal.
func TestHangUpWithTheScreenUp(t *testing.T) {
	script := ptyScript(t, "SIGHUP with the screen up is not held here")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, script, "-qec", self+" -test.run=^TestHangUpHelper$ -test.count=1 -test.v", "/dev/null")
	cmd.Env = append(os.Environ(), "MOTEL_TEST_HANGUP_HELPER=1", "TERM=xterm")
	out, _ := cmd.CombinedOutput()
	tty := string(out)
	// The alternate screen, which the TUI's screen opens on.
	if !strings.Contains(tty, "\x1b[?1049h") {
		t.Fatalf("the screen did not open:\n%q", tty)
	}
	_, got, _ := strings.Cut(tty, "RESULT ")
	got, _, _ = strings.Cut(got, "\n")
	if want := `exit 130, stderr ""`; strings.TrimSpace(got) != want {
		t.Errorf("after SIGHUP with the screen up: %s; want %s", got, want)
	}
}

// TestPanicSaid: a panic is said on one line, its value made safe and
// cut, with a short stack, and the command exits non-zero.
func TestPanicSaid(t *testing.T) {
	err := runFailure(&tui.Panic{Value: "box said \x1b]52;c;Zm9v\x07" + strings.Repeat("x", 500), Stack: []byte("goroutine 1 [running]:\nmain.f()\n\t/src/f.go:1\nmain.g(0x1)\n\t/src/g.go:2\n")})
	var f *client.Failure
	if !errors.As(err, &f) || f.Exit == 0 || f.Exit == client.ExitUsage {
		t.Fatalf("exit %v", err)
	}
	if strings.ContainsAny(f.Message, "\x1b\x07\n") || !strings.Contains(f.Message, `\x1b]52`) || len(f.Message) > 600 || !strings.Contains(f.Message, "main.f < main.g") {
		t.Errorf("the panic is said as %q", f.Message)
	}
}

// TestTUISignals: a closed terminal window (SIGHUP) ends the TUI.
func TestTUISignals(t *testing.T) {
	if !slices.Contains(tuiSignals, os.Signal(syscall.SIGHUP)) {
		t.Errorf("SIGHUP is not among %v", tuiSignals)
	}
}

// TestVersionGrammarIsTheClients: the : line takes a version (its
// check, model.ValidVersion, which prompt.go runs on :deploys' third
// word) exactly when `motel deploys <app> <version>` sends it to the
// box, on every edge the grammar has, but one difference, written out
// here as the one there is: a version over 64 characters is refused on
// the : line (the box's own grammar, liveswap's versionRe), and sent by
// the command, whose check (client.CheckVersion) leaves its length to
// the box's 422. A change to either side fails here.
func TestVersionGrammarIsTheClients(t *testing.T) {
	fb := newFakeBox(t)
	env := map[string]string{"MOTEL_TOKEN": "tok", "MOTEL_URL": fb.srv.URL + "/blog"}
	sends := func(v string) bool {
		r := run(t, fb, env, "deploys", "--", appURL(fb, "blog"), v)
		return r.code != client.ExitUsage
	}
	long, longer := strings.Repeat("a", 65), strings.Repeat("a", 300)
	differs := map[string]bool{long: true, longer: true} // refused on the line, sent by the command
	for _, v := range []string{"v1", "v41", "-v1", "_x", ".v1", "v1,", "a.b-c_d", "v1/x", "[x]", strings.Repeat("a", 64), long, longer, "v1\n", "ünï"} {
		line, command := model.ValidVersion(v), sends(v)
		if differs[v] {
			if line || !command {
				t.Errorf("%q: the : line takes it %v, the command sends it %v: the one known difference has changed", v, line, command)
			}
			continue
		}
		if line != command {
			t.Errorf("%q: the : line takes it %v, the command sends it %v", v, line, command)
		}
	}
}

// TestArgumentIsNoStop: a version the client refuses before asking is
// that read's own failure, not a stop of the session as a failed
// signature is; nothing is sent.
func TestArgumentIsNoStop(t *testing.T) {
	c := &client.Client{Box: client.Box{Host: "127.0.0.1"}, Cred: client.DeployToken("tok")}
	_, err := live{c}.Deploy(context.Background(), "blog", "v1,")
	if out, _ := model.Classify(err); out != model.Refused {
		t.Fatalf("a bad version is %v (%v)", out, err)
	}
	if out, stop := model.Classify(failure(&client.Failure{Exit: client.ExitUsage, Message: "ssh-keygen could not sign"})); out != model.Stopped || stop != model.NotSigned {
		t.Errorf("a signature that failed is %v", out)
	}
}

// TestDoubleDash: the commands take -- before their positional
// arguments, as the TUI prints them.
func TestDoubleDash(t *testing.T) {
	fb := newFakeBox(t)
	host := strings.TrimPrefix(fb.srv.URL, "https://")
	r := run(t, fb, map[string]string{"MOTEL_TOKEN": "tok", "MOTEL_URL": fb.srv.URL + "/blog"}, "deploys", "--json", "--", appURL(fb, "blog"), "v41")
	if r.code != 0 || !strings.Contains(r.stdout, `"version"`) {
		t.Errorf("deploys -- <app> <version>: exit %d, %q %q (%s)", r.code, r.stdout, r.stderr, host)
	}
}

// TestPrintedByTerminal: P's answer goes to a terminal indented and
// made safe, with a line on stderr that says so and how to get what
// --json prints; into a pipe or a file exactly what the command's
// --json prints for the same body (a C1 control, a bidi override and
// DEL in a string written as escapes), with nothing on stderr; the
// client's own words (a command line) the same either way.
func TestPrintedByTerminal(t *testing.T) {
	body := "{\"apps\":[{\"name\":\"blog\"}],\"note\":\"\u009b31m \u202eabc\x7f\"}"
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	f := &demo.Fake{Now: func() time.Time { return now }}
	f.Body("apps.json", body)
	s := &model.Session{Host: demo.Host, Key: demo.Key, Now: f.Now, Engine: model.NewEngine(now)}
	apply, err := s.Fetch(context.Background(), f, model.Job{Kind: model.KApps})
	if err != nil {
		t.Fatal(err)
	}
	apply()
	v := model.View{Screen: model.ScreenApps}
	raw, _ := s.Printable(v, 2)
	shown, _ := s.PrintShown(v, 2)
	p := tui.Printed{Raw: string(raw), Shown: shown}
	if model.IndentMax != 10 {
		t.Fatalf("stderr's sentence names 10 levels, and IndentMax is %d", model.IndentMax)
	}
	var flag bytes.Buffer
	if err := (&runner{env: Env{Stdout: &flag}}).emit([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(flag.String(), "\u009b\u202e\x7f") || strings.ContainsAny(shown, "\u009b\u202e\x7f") {
		t.Fatalf("a control printed raw: --json %q, the terminal's %q", flag.String(), shown)
	}
	for _, c := range []struct {
		terminal     bool
		p            tui.Printed
		stdout, warn string
	}{
		{true, p, model.JSONShown([]byte(body)), "motel: the box's answer was indented (at most 10 levels deep) and made safe for the terminal; redirect stdout (> file, or | cat) for what the command's --json prints\n"},
		{false, p, flag.String(), ""},
		{true, tui.Printed{Raw: "motel apps --box deploy.example.com\n"}, "motel apps --box deploy.example.com\n", ""},
		{false, tui.Printed{}, "", ""},
	} {
		var out, errOut bytes.Buffer
		r := &runner{env: Env{Terminal: c.terminal, Stdout: &out, Stderr: &errOut}}
		if err := r.printed(c.p); err != nil {
			t.Fatal(err)
		}
		if out.String() != c.stdout || errOut.String() != c.warn {
			t.Errorf("terminal %v: stdout %q, stderr %q; want %q, %q", c.terminal, out.String(), errOut.String(), c.stdout, c.warn)
		}
	}
}

// TestTUISessionBox: the TUI's client is built for the box --box or
// MOTEL_BOX names, port and all; its session names the host alone (the
// token's aud, the title) and prints commands that ask where it asks,
// with the key it signs with.
func TestTUISessionBox(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "me.pub"), []byte(meLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		flag string
		env  map[string]string
	}{
		"--box":     {"deploy.example.test:8443", map[string]string{"MOTEL_KEY": "~/me.pub"}},
		"MOTEL_BOX": {"", map[string]string{"MOTEL_KEY": "~/me.pub", "MOTEL_BOX": "deploy.example.test:08443"}},
	} {
		r := &runner{env: Env{Getenv: func(k string) string { return c.env[k] }, Stderr: &bytes.Buffer{}, Dir: t.TempDir(), Home: home,
			Now: time.Now, Run: noPrograms{}, Signer: &fixedSigner{}}}
		cl, err := r.client(context.Background(), &target{box: c.flag}, nil, "", "the screen")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		s := &model.Session{Engine: model.NewEngine(time.Now())}
		boxSession(s, cl, cl.Cred.(*client.PersonToken))
		if cl.Box.Addr() != "deploy.example.test:8443" || s.Host != "deploy.example.test" || s.Addr != "deploy.example.test:8443" {
			t.Errorf("%s: the client asks %s; the session's host %q, addr %q", name, cl.Box.Addr(), s.Host, s.Addr)
		}
		// The key MOTEL_KEY named is named in it too (sshkey.SourceEnv).
		if line, _ := s.Command(model.View{Screen: model.ScreenApps}); line != "motel apps --box deploy.example.test:8443 --key '~/me.pub'" {
			t.Errorf("%s: the command printed is %q", name, line)
		}
	}
}

// TestTUIClockSetBack: the TUI's client and session as the screen builds them,
// the real PersonToken and its signer, and a box whose clock is right:
// it takes a token from 60 s before its iat to 60 s after its exp, as
// liveswap/access does, and sends its Date. This machine's clock is
// set back after the first token is signed. Set back by more than a
// token bridges (2 m: it is sent until a minute before its exp on this
// clock, and taken until a minute after on the box's), that token is
// still sent when the box stops taking it, within its ten minutes:
// every read stops at the first read after, and the refusal screen
// names the clock first; until then who am I's token line says the
// step's length too much. Set back 2 m, or from 50 s fast to right, no
// read is refused, and the step asks the key for no signature.
func TestTUIClockSetBack(t *testing.T) {
	for _, c := range []struct {
		name       string
		fast, back time.Duration
		differs    string // "" when no read is refused
	}{
		{"back 1h", 0, time.Hour, "differs from the box's by 3600 s (behind it;"},
		{"back 3m", 0, 3 * time.Minute, "differs from the box's by 180 s (behind it;"},
		{"back 2m", 0, 2 * time.Minute, ""},
		{"fast 50s, set right", 50 * time.Second, 50 * time.Second, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			truth := whoamiNow // the box's clock
			off := c.fast
			wall := func() time.Time { return truth.Add(off) }
			fb := newFakeBox(t)
			fb.apps = wire.Apps{Apps: []wire.AppEntry{{Name: "blog"}}} // the one app this fake answers for
			box := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Date", truth.UTC().Format(http.TimeFormat))
				var times struct{ Iat, Exp int64 }
				tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), sshkey.Scheme+" "+sshkey.Envelope)
				claims, _, _ := strings.Cut(tok, ".")
				b, _ := base64.RawURLEncoding.DecodeString(claims)
				at := truth.Unix()
				if json.Unmarshal(b, &times) != nil || times.Iat-60 > at || at-60 > times.Exp {
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = w.Write([]byte(`{"error":"invalid or missing deploy token (Authorization: Bearer <jwt>)"}` + "\n"))
					return
				}
				fb.srv.Config.Handler.ServeHTTP(w, r)
			}))
			t.Cleanup(box.Close)
			home := t.TempDir()
			if err := os.WriteFile(filepath.Join(home, "me.pub"), []byte(meLine+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			env := map[string]string{"MOTEL_KEY": "~/me.pub", "MOTEL_URL": box.URL + "/blog"}
			signs := &fixedSigner{}
			r := &runner{env: Env{Getenv: func(k string) string { return env[k] }, Stderr: &bytes.Buffer{}, Dir: t.TempDir(), Home: home,
				Now: wall, Sleep: func(context.Context, time.Duration) error { return nil },
				Transport: box.Client().Transport, Run: noPrograms{}, Signer: signs, Version: "test"}}
			cl, err := r.client(context.Background(), &target{}, nil, "", "the screen")
			if err != nil {
				t.Fatal(err)
			}
			sig := forTUI(cl, wall)
			s := &model.Session{Now: wall, Engine: model.NewEngine(wall())}
			boxSession(s, cl, cl.Cred.(*client.PersonToken))
			s.TokenExpires = sig.Expires
			// read makes the next read the engine gives, as Drive does,
			// with a key pressed so that polling never pauses.
			read := func() {
				t.Helper()
				for range 10000 {
					s.Engine.Touch(wall())
					j, ok, wait := s.Engine.Take(wall())
					if !ok {
						truth = truth.Add(max(wait, time.Second))
						continue
					}
					apply, err := s.Fetch(context.Background(), live{cl}, j)
					apply()
					s.Engine.Done(j, wall(), err)
					truth = truth.Add(100 * time.Millisecond)
					return
				}
				t.Fatal("no read")
			}
			for start := truth; truth.Sub(start) < time.Minute; {
				read()
			}
			if s.Engine.Stopped() != model.Running || signs.calls != 1 {
				t.Fatalf("before the step: %v, %d signatures (%v)", s.Engine.Stopped(), signs.calls, s.StopErr)
			}
			exp := sig.Expires() // the first token's, on the box's clock too
			off -= c.back
			read()
			if signs.calls != 1 {
				t.Errorf("the first read after the step made signature %d", signs.calls)
			}
			if c.differs != "" {
				want := "(" + model.Ago(exp.Sub(wall())) + " left)"
				var lines []string
				for _, l := range s.MeView(wall()) {
					lines = append(lines, l.Text())
				}
				if got := strings.Join(lines, "\n"); exp.Sub(wall()) < c.back || !strings.Contains(got, want) {
					t.Errorf("who am I does not say %q, the step too much:\n%s", want, got)
				}
			}
			for end := truth.Add(25 * time.Minute); truth.Before(end) && s.Engine.Stopped() == model.Running; {
				read()
			}
			if c.differs == "" {
				if s.Engine.Stopped() != model.Running {
					t.Fatalf("stopped (%v): %v", s.Engine.Stopped(), s.StopErr)
				}
				return
			}
			if s.Engine.Stopped() != model.SignedOut {
				t.Fatalf("after 25 m, %v, want SignedOut", s.Engine.Stopped())
			}
			if signs.calls != 1 || truth.Before(exp.Add(sshkey.BoxSkew)) || truth.After(exp.Add(sshkey.BoxSkew+time.Minute)) {
				t.Errorf("stopped at %s with %d signatures; the first token was taken until %s", truth.Format(time.TimeOnly), signs.calls, exp.Add(sshkey.BoxSkew).Format(time.TimeOnly))
			}
			// What the refusal screen shows: its first line is the stop's
			// failure, the clock named before the other causes.
			shown := s.RefusedView(wall())[0].Text()
			if i, j := strings.Index(shown, "This machine's clock "+c.differs), strings.Index(shown, "no person or signer line"); i < 0 || j < i {
				t.Errorf("the refusal screen does not name the clock first:\n%s", shown)
			}
		})
	}
}

// TestNoScreenNothingSent: where the screen cannot be drawn (no
// terminal, a TERM it cannot draw with), the TUI ends before anything
// is signed or sent, exit 2, saying why; the --demo screen too.
func TestNoScreenNothingSent(t *testing.T) {
	fb := newFakeBox(t)
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "me.pub"), []byte(meLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"MOTEL_KEY": "~/me.pub", "MOTEL_URL": fb.srv.URL + "/blog"}
	for _, args := range [][]string{{"--key", "~/me.pub"}, {"--demo"}} {
		sent := 0
		rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			sent++
			return fb.srv.Client().Transport.RoundTrip(r)
		})
		s := &fixedSigner{}
		var out, errOut bytes.Buffer
		code := Main(context.Background(), args, Env{
			Getenv: func(k string) string { return env[k] }, Stdout: &out, Stderr: &errOut, Dir: t.TempDir(), Home: home,
			Now: time.Now, Sleep: func(context.Context, time.Duration) error { return nil },
			Transport: rt, Run: noPrograms{}, Signer: s, Version: "test",
			Draw: func() error { return errors.New("open /dev/tty: no such device or address") },
		})
		want := "motel: the screen could not start: open /dev/tty: no such device or address\n"
		if code != client.ExitUsage || errOut.String() != want || out.Len() != 0 || sent != 0 || s.calls != 0 {
			t.Errorf("%v: exit %d, stderr %q, stdout %q, %d sent, %d signed; want 2, %q, nothing sent or signed", args, code, errOut.String(), out.String(), sent, s.calls, want)
		}
	}
}

// TestCanDraw: the check finds the TERM the screen could not draw
// with, and the locale whose character set it could not write, whether
// or not this process has a terminal: a locale it can write passes the
// charset step, and then fails at /dev/tty or passes.
func TestCanDraw(t *testing.T) {
	setLocale(t, locale{lang: "C.UTF-8"})
	for term, want := range map[string]string{"dumb": "not cursor addressable", "": "term not set"} {
		t.Setenv("TERM", term)
		if err := CanDraw(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("TERM=%q: %v, want %q", term, err, want)
		}
	}
	t.Setenv("TERM", "xterm-256color") // tcell's own, built in
	for _, l := range locales {
		setLocale(t, l)
		if got := localeCharset(); got != l.charset {
			t.Errorf("%+v: the character set is %q, want %q", l, got, l.charset)
		}
		if err := CanDraw(); errors.Is(err, tcell.ErrNoCharset) != l.refused {
			t.Errorf("%+v: %v; want it refused for its character set: %v", l, err, l.refused)
		}
	}
}

// locale is LC_ALL, LC_CTYPE and LANG, the character set tcell reads
// from them, and whether tcell (which knows UTF-8 and ASCII alone, as
// the client registers no other) refuses it.
type locale struct {
	all, ctype, lang string
	charset          string
	refused          bool
}

var locales = []locale{
	{charset: "UTF-8"}, // none set
	{lang: "en_US.UTF-8", charset: "UTF-8"},
	{lang: "C.UTF-8", charset: "UTF-8"},
	{lang: "de_DE.utf8", charset: "utf8"},
	{lang: "C", charset: "US-ASCII"},
	{lang: "POSIX", charset: "US-ASCII"},
	{lang: "en_US", charset: "UTF-8"},
	{lang: "de_DE@euro", charset: "UTF-8"},
	{lang: "de_DE.UTF-8@euro", charset: "UTF-8"},
	{lang: "de_DE.ISO-8859-15@euro", charset: "ISO-8859-15", refused: true},
	{lang: "en_US.ISO-8859-1", charset: "ISO-8859-1", refused: true},
	{lang: "ja_JP.eucJP", charset: "eucJP", refused: true},
	{lang: "ru_RU.KOI8-R", charset: "KOI8-R", refused: true},
	{all: "en_US.UTF-8", lang: "en_US.ISO-8859-1", charset: "UTF-8"},
	{all: "en_US.ISO-8859-1", lang: "en_US.UTF-8", charset: "ISO-8859-1", refused: true},
	{ctype: "ru_RU.KOI8-R", lang: "en_US.UTF-8", charset: "KOI8-R", refused: true},
	{all: "C", ctype: "ru_RU.KOI8-R", charset: "US-ASCII"},
}

func setLocale(t *testing.T, l locale) {
	t.Helper()
	t.Setenv("LC_ALL", l.all)
	t.Setenv("LC_CTYPE", l.ctype)
	t.Setenv("LANG", l.lang)
}

// TestCharsetIsTcells: localeCharset reads the locale as tcell's own
// Init does, on its real terminfo screen over a terminal that records
// bytes: Init fails with ErrNoCharset for exactly the locales CanDraw
// refuses. A tcell that reads it otherwise fails here.
func TestCharsetIsTcells(t *testing.T) {
	for _, l := range locales {
		setLocale(t, l)
		scr, err := tuihostile.Screen(tuihostile.NewTTY())
		if err != nil {
			t.Fatal(err)
		}
		err = scr.Init()
		if err == nil {
			scr.Fini()
		}
		if errors.Is(err, tcell.ErrNoCharset) != (tcell.GetEncoding(localeCharset()) == nil) {
			t.Errorf("%+v: tcell's Init says %v, and CanDraw's check reads %q", l, err, localeCharset())
		}
		if err != nil && !errors.Is(err, tcell.ErrNoCharset) {
			t.Errorf("%+v: tcell's Init failed otherwise: %v", l, err)
		}
	}
}

// TestUnknownCharsetNothingSent: a locale whose character set the
// screen cannot write ends motel before anything is signed or sent,
// exit 2, in tcell's own words, with the check main makes; typed
// alone, under the help.
func TestUnknownCharsetNothingSent(t *testing.T) {
	setLocale(t, locale{lang: "en_US.ISO-8859-1"})
	t.Setenv("TERM", "xterm-256color")
	fb := newFakeBox(t)
	home := keyHome(t)
	help := run(t, nil, nil, "--help")
	env := map[string]string{"MOTEL_KEY": "~/me.pub", "MOTEL_URL": fb.srv.URL + "/blog"}
	want := "motel: the screen could not start: character set not supported\n"
	for _, args := range [][]string{nil, {"--key", "~/me.pub"}, {"--demo"}} {
		r := startMotel(t, fb, env, home, true, CanDraw, args...)
		wantOut := ""
		if args == nil {
			wantOut = help.stdout
		}
		if r.code != client.ExitUsage || r.stderr != want || r.stdout != wantOut || r.signed != 0 || r.sent != 0 {
			t.Errorf("%q: exit %d, stderr %q, %d signed, %d sent, stdout as wanted %v; want 2, %q, nothing signed or sent, and the help alone typed alone", args, r.code, r.stderr, r.signed, r.sent, r.stdout == wantOut, want)
		}
	}
}
