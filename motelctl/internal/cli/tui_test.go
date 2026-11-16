package cli

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/liveswap/wire"
	"github.com/smallhoursorg/motel/motelctl/internal/client"
	"github.com/smallhoursorg/motel/motelctl/internal/sshkey"
	"github.com/smallhoursorg/motel/motelctl/internal/tui/model"
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

// commandOf is each model.Client method's command: live calls the
// client's read of the same name, which that command calls.
var commandOf = map[string]string{"WhoAmI": "whoami", "Apps": "apps", "Status": "status", "Deploy": "deploys"}

// TestActionsAreCommands: the TUI's reads are made only through its
// table of actions, and each names a motelctl command that exists,
// takes the arguments it names, and is the command whose client read
// the action's read calls. The reads the TUI makes are all there.
func TestActionsAreCommands(t *testing.T) {
	root := (&runner{}).tree()
	kinds := map[model.Kind]bool{}
	for _, a := range model.Actions {
		kinds[a.Kind] = true
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
		rec := &recorder{}
		if err := a.Read(context.Background(), rec, model.Job{Kind: a.Kind, App: "blog", Version: "v1"}); err != nil {
			t.Fatal(err)
		}
		if commandOf[rec.called] != path[0] {
			t.Errorf("%q reads with %s, which is %q's read", a.Command, rec.called, commandOf[rec.called])
		}
	}
	for _, k := range []model.Kind{model.KWhoAmI, model.KApps, model.KStatus, model.KDeploy} {
		if !kinds[k] {
			t.Errorf("the TUI reads kind %d, and no action does it", k)
		}
	}
}

// TestForTUI: the TUI's client, and only it, signs tokens of the
// longest life a box takes and hands a 429 back; no other file of the
// module sets either.
func TestForTUI(t *testing.T) {
	c := &client.Client{Cred: &client.PersonToken{Signer: &fixedSigner{}}, Notes: os.Stderr}
	forTUI(c, time.Now)
	pt := c.Cred.(*client.PersonToken)
	if pt.Life != sshkey.MaxLife || !c.NoWait || c.Notes != nil {
		t.Errorf("life %s, NoWait %v, notes %v", pt.Life, c.NoWait, c.Notes)
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
			var names []string
			switch n := n.(type) {
			case *ast.AssignStmt:
				for _, l := range n.Lhs {
					if s, ok := l.(*ast.SelectorExpr); ok {
						names = append(names, s.Sel.Name)
					}
				}
			case *ast.KeyValueExpr:
				if k, ok := n.Key.(*ast.Ident); ok {
					names = append(names, k.Name)
				}
			}
			for _, nm := range names {
				if nm == "NoWait" || nm == "Life" {
					t.Errorf("%s sets %s: only cli/tui.go builds the TUI's client", name, nm)
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
		{&client.Failure{Exit: client.ExitUnknown, Status: 502}, model.FailNoAnswer},
		{&client.Failure{Exit: client.ExitUnknown, Status: 200}, model.FailThis},
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
	code := Main(context.Background(), []string{"tui"}, Env{
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
	r := run(t, nil, nil, "tui", "--help")
	if r.code != 0 || strings.Contains(r.stdout, "demo") {
		t.Errorf("exit %d; help shows --demo:\n%s", r.code, r.stdout)
	}
}
