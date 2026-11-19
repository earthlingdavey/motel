// Package cli is the client's command tree: what each command reads,
// asks and prints, and the exit code it ends with. motel alone, the
// root, opens the screens, a view over the same reads the commands
// make.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/smallhoursorg/motel/client/internal/client"
	"github.com/smallhoursorg/motel/client/internal/local"
	"github.com/smallhoursorg/motel/client/internal/render"
	"github.com/smallhoursorg/motel/client/internal/sshkey"
	"github.com/smallhoursorg/motel/client/internal/termtext"
)

// Env is everything a command reads from outside its arguments. main
// fills it from the process; a test fills it with its own.
type Env struct {
	Getenv         func(string) string
	Stdout, Stderr io.Writer
	// Terminal is whether stdout is a terminal; Color, whether it may
	// be dressed: a terminal, with NO_COLOR unset.
	Terminal, Color bool
	// Dir is the working directory, Home the home directory.
	Dir, Home string
	Now       func() time.Time
	Sleep     func(context.Context, time.Duration) error
	// Transport is what requests go through; nil is the system's.
	Transport http.RoundTripper
	Run       local.Runner
	// Signer signs a person's token; nil is ssh-keygen through Run.
	Signer sshkey.Signer
	// Version is the client's own release.
	Version string
	// Draw says why the screen could not be drawn here (CanDraw), and
	// is asked before anything is signed or sent; nil asks nothing, as
	// for a test's own screen.
	Draw func() error
}

const experimental = "Every command is experimental: its flags, output and exit codes can change\n" +
	"in any release until a release's notes say otherwise. The human output is\n" +
	"never a contract. --json prints the box's own JSON for a read of it (deploys\n" +
	"<app>: its deploys array alone; version and key: motel's own), and on\n" +
	"exit 1 or 6, or 5 when the 403 was JSON; on exit 2, 3, 4, 8 or another 5,\n" +
	"{\"error\":...} holding motel's own sentence, the one stderr prints.\n\n" +
	"The box: an app's URL as <app>, else --box, MOTEL_BOX, the host of MOTEL_URL,\n" +
	"or the box repository the shell is in. The credential: a token your ssh key\n" +
	"signs when --key names the key; else MOTEL_TOKEN as Bearer when set (a deploy\n" +
	"token reads its app); else a token signed by the key MOTEL_KEY names, or\n" +
	"git's user.signingkey in a box repository, ~/.ssh/id_ed25519, then\n" +
	"~/.ssh/id_ed25519_sk, or the agent's only key, in that order. Only https:\n" +
	"there is no way round it.\n\n" +
	"Exit codes: 0 read; 1 the box answered with a failure; 2 nothing sent (usage,\n" +
	"no box or key, an RSA or ECDSA key); 3 could not tell (no answer, not moteld's\n" +
	"answer, any 5xx, a 429 that stood); 4 not accepted (a 401 after one retry);\n" +
	"5 not allowed (403); 6 not found (404); 8 the certificate did not verify; 130\n" +
	"interrupted."

// Main runs the client with args (without the program's name) and
// returns the exit code.
func Main(ctx context.Context, args []string, env Env) int {
	r := &runner{env: env}
	root := r.tree()
	root.SetArgs(args)
	root.SetOut(env.Stdout)
	root.SetErr(env.Stderr)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return client.ExitOK
	}
	if ctx.Err() != nil || errors.Is(err, errInterrupted) {
		return client.ExitInterrupted
	}
	var f *client.Failure
	if !errors.As(err, &f) {
		// Anything else is cobra's own: a flag, an argument or a
		// command it could not read.
		f = &client.Failure{Exit: client.ExitUsage, Message: cobraLine(err.Error())}
	}
	// A refusal of the arguments themselves comes before --json is
	// read, so it is looked for here too.
	if r.json || wantsJSON(args) {
		if f.Body != nil {
			_, _ = env.Stdout.Write(withNewline(termtext.JSON(f.Body)))
		} else {
			b, _ := json.Marshal(struct {
				Error string `json:"error"`
			}{f.Message})
			_, _ = fmt.Fprintf(env.Stdout, "%s\n", b)
		}
	}
	_, _ = fmt.Fprintf(env.Stderr, "motel: %s\n", f.Message)
	return f.Exit
}

// cobraLine is cobra's own refusal as one line. To an unknown command
// cobra adds the nearest commands on lines of their own, after a blank
// one: they follow its first line as a sentence. The word typed is in
// that first line, written with %q, so a line break typed in it is
// \n there and cannot be taken for cobra's; whatever else the line
// holds is made visible with termtext.From.
func cobraLine(msg string) string {
	head, near, found := strings.Cut(msg, "\n\nDid you mean this?\n")
	line := termtext.From(head).String()
	if !found {
		return line
	}
	var names []string
	for _, n := range strings.Fields(near) {
		names = append(names, termtext.From(n).String())
	}
	if len(names) > 1 {
		names = append(names[:len(names)-2], names[len(names)-2]+" or "+names[len(names)-1])
	}
	return line + ". Did you mean this? " + strings.Join(names, ", ")
}

// errInterrupted is a command ended by a signal its context alone
// carries, not Main's (SIGHUP, which the TUI takes): it exits as an
// interrupt does, saying nothing.
var errInterrupted = errors.New("interrupted")

// wantsJSON says whether args ask for --json, read as cobra would
// read it, for an error cobra returned before reading it.
func wantsJSON(args []string) bool {
	for _, a := range args {
		switch a {
		case "--":
			return false
		case "--json", "--json=true", "--json=1", "--json=t", "--json=T", "--json=TRUE", "--json=True":
			return true
		}
	}
	return false
}

type runner struct {
	env  Env
	json bool
}

func (r *runner) tree() *cobra.Command {
	root := &cobra.Command{
		Use:           "motel",
		Short:         "Read a motel box over HTTPS (experimental)",
		Long:          "motel reads a box over HTTPS: who your key is there and the apps it may view,\nan app's status, deploys and alerts, and the box's own state, backups and alerts.\n\n" + screenLong + "\n\n" + experimental,
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.AddGroup(&cobra.Group{ID: "experimental", Title: "Experimental commands:"})
	root.PersistentFlags().BoolVar(&r.json, "json", false, "print JSON on stdout: for a read, the box's own answer")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &client.Failure{Exit: client.ExitUsage, Message: termtext.From(err.Error()).String()}
	})
	// The root runs the screens. Its Args stay unset, so cobra still
	// refuses a word that is not a command, naming the nearest; a word
	// cobra does not look at, after -- or empty, the root refuses itself
	// in cobra's words.
	r.screen(root)

	box := &cobra.Command{
		Use:     "box",
		Short:   "The box itself (experimental)",
		GroupID: "experimental",
		Args:    usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return &client.Failure{Exit: client.ExitUsage, Message: "box needs a command: motel box status"}
		},
	}
	box.AddCommand(r.boxStatus())
	backup := &cobra.Command{
		Use:     "backup",
		Short:   "The box's backups, as the box says (experimental)",
		GroupID: "experimental",
		Args:    usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return &client.Failure{Exit: client.ExitUsage, Message: "backup needs a command: motel backup status"}
		},
	}
	backup.AddCommand(r.backupStatus())
	for _, c := range []*cobra.Command{r.version(), r.key(), r.whoami(), r.apps(), r.status(), r.deploys(), box, backup, r.monitor()} {
		c.GroupID = "experimental"
		root.AddCommand(c)
	}
	return root
}

// usageArgs makes an argument count cobra refuses a usage refusal.
func usageArgs(check cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := check(cmd, args); err != nil {
			return &client.Failure{Exit: client.ExitUsage, Message: termtext.From(err.Error()).String() + "; " + cmd.CommandPath() + " --help says more"}
		}
		return nil
	}
}

// target is where a box-asking command's own flags land.
type target struct {
	box, key string
}

func (t *target) flags(c *cobra.Command) {
	c.Flags().StringVar(&t.box, "box", "", "the box's host name, e.g. deploy.example.com, and :port when it is not 443")
	c.Flags().StringVar(&t.key, "key", "", "the ssh key to sign with: its .pub, or the private key with a .pub beside it")
}

// client resolves the box and the credential, in that order, and
// returns the client that asks the box. scope is whose deploy_trust a
// refused deploy token is judged by; keyOnly, for a command a person's
// key alone can ask, names that command and refuses MOTEL_TOKEN before
// anything is sent.
func (r *runner) client(ctx context.Context, t *target, fromApp *client.Box, scope, keyOnly string) (*client.Client, error) {
	box, err := client.ResolveBox(ctx, client.BoxLookup{FromApp: fromApp, Flag: t.box, Getenv: r.env.Getenv, Dir: r.env.Dir, Run: r.env.Run})
	if err != nil {
		return nil, err
	}
	// A --key typed on the command line beats MOTEL_TOKEN; MOTEL_TOKEN
	// beats MOTEL_KEY and every key the client would find by itself.
	var cred client.Credential
	tok := r.env.Getenv("MOTEL_TOKEN")
	if tok != "" && t.key == "" && keyOnly != "" {
		return nil, &client.Failure{Exit: client.ExitUsage, Message: keyOnly + " is asked with your own ssh key, and MOTEL_TOKEN is set, which motel would send instead: a deploy token is no one's key. Name your key with --key, or unset MOTEL_TOKEN."}
	}
	if tok != "" && t.key == "" {
		dt, err := client.NewDeployToken(tok)
		if err != nil {
			return nil, err
		}
		if err := client.TokenFor(box, r.env.Getenv); err != nil {
			return nil, err
		}
		cred = dt
	} else {
		key, err := sshkey.Resolve(ctx, sshkey.Lookup{Flag: t.key, Getenv: r.env.Getenv, Home: r.env.Home, Dir: r.env.Dir, Run: r.env.Run})
		if err != nil {
			return nil, keyFailure(err)
		}
		signer := r.env.Signer
		if signer == nil {
			signer = sshkey.Keygen{Run: r.env.Run, Stderr: r.env.Stderr}
		}
		cred = &client.PersonToken{Key: key, Signer: signer, Box: box, Now: r.env.Now}
	}
	return &client.Client{
		Box:       box,
		Cred:      cred,
		HTTP:      client.NewHTTP(r.env.Transport),
		Now:       r.env.Now,
		Sleep:     r.env.Sleep,
		UserAgent: "motel/" + r.env.Version,
		Scope:     scope,
		Roots:     roots(r.env.Getenv),
	}, nil
}

// roots names the variables that replace the system's roots for Go's
// TLS, with their values, when set.
func roots(getenv func(string) string) []string {
	var set []string
	for _, k := range []string{"SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if v := getenv(k); v != "" {
			set = append(set, k+"="+v)
		}
	}
	return set
}

// keyFailure is a key that cannot sign: nothing was sent.
func keyFailure(err error) error {
	var f *client.Failure
	if errors.As(err, &f) {
		return f
	}
	return &client.Failure{Exit: client.ExitUsage, Message: termtext.From(err.Error()).String()}
}

// fail turns what a request returned into the command's error: a
// *client.Failure as it is, a signing error as usage (nothing was
// sent), an interruption as itself.
func fail(err error) error {
	var f *client.Failure
	switch {
	case errors.As(err, &f):
		return f
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	}
	return keyFailure(err)
}

// emit prints a box's body for --json: the JSON value as received,
// with the characters a terminal acts on written as JSON's own escapes
// (termtext.JSON). stdout is as often a terminal as a pipe, and a pipe
// may end at one.
func (r *runner) emit(body []byte) error {
	_, err := r.env.Stdout.Write(withNewline(termtext.JSON(body)))
	return err
}

func withNewline(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\n' {
		return b
	}
	return append(b[:len(b):len(b)], '\n')
}

func (r *runner) style() render.Style { return render.Style{Color: r.env.Color} }
