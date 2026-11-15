// Package cli is motelctl's command tree: what each command reads,
// asks and prints, and the exit code it ends with. The commands are
// the client's whole surface; a later screen is a view over the same
// functions.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/smallhoursorg/motel/motelctl/internal/client"
	"github.com/smallhoursorg/motel/motelctl/internal/local"
	"github.com/smallhoursorg/motel/motelctl/internal/render"
	"github.com/smallhoursorg/motel/motelctl/internal/sshkey"
	"github.com/smallhoursorg/motel/motelctl/internal/termtext"
)

// Env is everything a command reads from outside its arguments. main
// fills it from the process; a test fills it with its own.
type Env struct {
	Getenv         func(string) string
	Stdout, Stderr io.Writer
	// Color is whether stdout may be dressed: a terminal, with
	// NO_COLOR unset.
	Color bool
	// Dir is the working directory, Home the home directory.
	Dir, Home string
	Now       func() time.Time
	Sleep     func(context.Context, time.Duration) error
	// Transport is what requests go through; nil is the system's.
	Transport http.RoundTripper
	Run       local.Runner
	// Signer signs a person's token; nil is ssh-keygen through Run.
	Signer sshkey.Signer
	// Version is motelctl's own release.
	Version string
}

// Protocol is the person-token protocol this client speaks: the value
// a box's Motel-Protocol will carry, 1 being the token form m1.
const Protocol = 1

const experimental = "Every command is experimental: its flags, output and exit codes can change\n" +
	"in any release until a release's notes say otherwise. The human output is\n" +
	"never a contract; --json is the box's own answer.\n\n" +
	"The box: an app's URL as <app>, else --box, MOTEL_BOX, the host of MOTEL_URL,\n" +
	"or the box repository the shell is in. The credential: a token your ssh key\n" +
	"signs when --key names the key; else MOTEL_TOKEN as Bearer when set (a deploy\n" +
	"token reads its app); else a token signed by the key MOTEL_KEY names, or\n" +
	"git's user.signingkey in a box repository, ssh's default files, or the\n" +
	"agent's only key, in that order. Only https: there is no way round it.\n\n" +
	"Exit codes: 0 read; 1 the box answered with a failure; 2 nothing sent (usage,\n" +
	"no box or key, an RSA key); 3 could not tell (no answer, not motel's answer,\n" +
	"a 429 that stood); 4 not accepted (a 401 after one retry); 5 not allowed (403);\n" +
	"6 not found (404); 8 the certificate did not verify; 130 interrupted."

// Main runs motelctl with args (without the program's name) and
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
	if ctx.Err() != nil {
		return client.ExitInterrupted
	}
	var f *client.Failure
	if !errors.As(err, &f) {
		// Anything else is cobra's own: a flag, an argument or a
		// command it could not read.
		f = &client.Failure{Exit: client.ExitUsage, Message: termtext.From(err.Error()).String()}
	}
	// A refusal of the arguments themselves comes before --json is
	// read, so it is looked for here too.
	if r.json || wantsJSON(args) {
		if f.Body != nil {
			_, _ = env.Stdout.Write(withNewline(f.Body))
		} else {
			b, _ := json.Marshal(struct {
				Error string `json:"error"`
			}{f.Message})
			_, _ = fmt.Fprintf(env.Stdout, "%s\n", b)
		}
	}
	_, _ = fmt.Fprintf(env.Stderr, "motelctl: %s\n", f.Message)
	return f.Exit
}

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
		Use:           "motelctl",
		Short:         "Read a motel box over HTTPS (experimental)",
		Long:          "motelctl reads a motel box over HTTPS: an app's status and deploys, and the box's own state.\n\n" + experimental,
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.AddGroup(&cobra.Group{ID: "experimental", Title: "Experimental commands:"})
	root.PersistentFlags().BoolVar(&r.json, "json", false, "print JSON on stdout: the box's answer as received")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &client.Failure{Exit: client.ExitUsage, Message: termtext.From(err.Error()).String()}
	})

	box := &cobra.Command{
		Use:     "box",
		Short:   "The box itself (experimental)",
		GroupID: "experimental",
		Args:    usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return &client.Failure{Exit: client.ExitUsage, Message: "box needs a command: motelctl box status"}
		},
	}
	box.AddCommand(r.boxStatus())
	for _, c := range []*cobra.Command{r.version(), r.key(), r.status(), r.deploys(), box} {
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
	c.Flags().StringVar(&t.box, "box", "", "the box's host name, e.g. deploy.example.com")
	c.Flags().StringVar(&t.key, "key", "", "the ssh key to sign with: its .pub, or the private key with a .pub beside it")
}

// client resolves the box and the credential, in that order, and
// returns the client that asks the box.
func (r *runner) client(ctx context.Context, t *target, fromApp *client.Box, scope string) (*client.Client, error) {
	box, err := client.ResolveBox(ctx, client.BoxLookup{FromApp: fromApp, Flag: t.box, Getenv: r.env.Getenv, Dir: r.env.Dir, Run: r.env.Run})
	if err != nil {
		return nil, err
	}
	// A --key typed on the command line beats MOTEL_TOKEN; MOTEL_TOKEN
	// beats MOTEL_KEY and every key motelctl would find by itself.
	var cred client.Credential
	if tok := r.env.Getenv("MOTEL_TOKEN"); tok != "" && t.key == "" {
		dt, err := client.NewDeployToken(tok)
		if err != nil {
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
		UserAgent: "motelctl/" + r.env.Version,
		Scope:     scope,
	}, nil
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

// emit prints a body as received, for --json.
func (r *runner) emit(body []byte) error {
	_, err := r.env.Stdout.Write(withNewline(body))
	return err
}

func withNewline(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\n' {
		return b
	}
	return append(b[:len(b):len(b)], '\n')
}

func (r *runner) style() render.Style { return render.Style{Color: r.env.Color} }
