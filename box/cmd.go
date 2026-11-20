package box

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/caddyserver/caddy/v2"
	caddycmd "github.com/caddyserver/caddy/v2/cmd"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/smallhoursorg/motel/box/proof"
)

// `moteld box …` is one command with subcommands: webhook, describe,
// apply, baseline and key.
func init() {
	caddycmd.RegisterCommand(caddycmd.Command{
		Name:  "box",
		Usage: "<command>",
		Short: "The box config channel: config by signed push",
		Long: `Commands of the box config channel (box/DESIGN-box.md in the
motel repository): the box's Caddyfile is written in the operator's
repository, and a signed push reaches the box through box_webhook.`,
		CobraFunc: func(cmd *cobra.Command) {
			cmd.AddCommand(&cobra.Command{
				Use:   "webhook <Caddyfile>",
				Short: "Print the URL of the Caddyfile's box_webhook",
				Long: `Reads the Caddyfile the way the box does — its raw tokens, expanding
nothing and following nothing — and prints https://<host>/ for the one
site carrying box_webhook. It refuses the file's contents for every
reason the box would, so the laptop, CI and the box cannot disagree about which
address a file names. The file may be a pipe:

    moteld box webhook <(git show HEAD:deploy.example.com/Caddyfile)`,
				Args: cobra.ExactArgs(1),
				RunE: func(c *cobra.Command, args []string) error {
					url, err := webhookURL(args[0])
					if err != nil {
						return err
					}
					_, err = fmt.Fprintln(c.OutOrStdout(), url)
					return err
				},
			})
			cmd.AddCommand(&cobra.Command{
				Use:   "describe <Caddyfile>",
				Short: "Print a Caddyfile's box_webhook host, apps, signers, people and grants, as the box reads them",
				Long: `Reads the Caddyfile as "moteld box webhook" does and prints one line
per fact, for a script to read: the host, then the apps, then the
signers, each in file order:

    host deploy.example.com
    app example
    signer alice@example.com ssh-ed25519 AAAAC3Nz...

With a person or grant line in the box block, it adds a person line for
each key a person may hold (a signer line's too, marked signer; an
ssh-rsa signer key is not one), by the key's fingerprint, then the
grants, then a warning line for each doubt the lines were taken with:

    person bob@example.com ssh-ed25519 SHA256:e2/1Bk...
    grant bob@example.com viewer app example
    grant bob@example.com logs app example from 198.51.100.0/24
    warning person carol@example.com has no grant: their key is told only who they are here (/_motel/whoami)

Together they say who may reach this box over HTTPS with a token their
own key signs, and what each may read there, from the box's own name;
root's key, which is not in the file, is the one other way in.

With a secrets/ directory beside the file it reads that too, as the
working tree has it, and adds:

    recipients 2
    secret example DATABASE_URL 61
    warning app example has 200 secrets; the cap is 256

each secret's value length from its ciphertext alone (no key), and a
warning for each cap past 75%.

It refuses the file for every reason the box's reading of it would,
the secrets for every reason their layout would, and an app whose
env_file does not read its secrets; what only moteld validate refuses
(a module's own errors) it does not check. The box template's make check prints its line for each box from this, and
compares the signer lists of a repository's boxes. The file may be a
pipe, as for webhook (a pipe has no secrets beside it).`,
				Args: cobra.ExactArgs(1),
				RunE: func(c *cobra.Command, args []string) error {
					return describe(args[0], c.OutOrStdout())
				},
			})
			cmd.AddCommand(&cobra.Command{
				Use:   "apply",
				Short: "Apply the config pushes waiting on this box (root; motel-box-apply.service runs it)",
				Long: `Settles anything a crash left, takes every bundle box_webhook dropped
into /var/lib/motel-box/in, proves each against the Caddyfile this box
runs and installs it with systemctl reload moteld, rolling back on a
failed reload; then writes each push's result for the workflow's poll
and sweeps old results. One shot, as root: motel-box-apply.path starts
it. It exits non-zero only when the previous Caddyfile could not be put
back after a failed install (a full disk); the journal says so.`,
				Args: cobra.NoArgs,
				RunE: func(c *cobra.Command, _ []string) error {
					return runApply(c.Context(), os.Geteuid(), newApplier(caddy.Log().Named("box.apply")))
				},
			})
			cmd.AddCommand(&cobra.Command{
				Use:   "baseline <sha>",
				Short: "Reset the commit this box's pushes must descend from (root)",
				Long: `A trust reset, for the one case where the repository's history no
longer contains the commit this box runs: a rewrite below it, or a
chain longer than 500 commits. Every later push must descend from
<sha>, and the box never examines <sha>'s ancestors. The path and the
digest recorded at the last apply stay as they were, so an edit made
on the box since then is still reported as one. It is not the fix
for an unsigned commit above the commit the box runs: that is a rebase
and a force-push, with nothing to do on the box. As root, after
moteld init; it waits while the applier holds its lock.`,
				Args: cobra.ExactArgs(1),
				RunE: func(c *cobra.Command, args []string) error {
					return runBaseline(c.Context(), os.Geteuid(), newApplier(consoleLog("box.baseline")), args[0], c.OutOrStdout())
				},
			})
			cmd.AddCommand(&cobra.Command{
				Use:   "key",
				Short: "Print this box's key for secrets, making it if there is none (root)",
				Long: `Prints the box's key for secrets: the age recipient a repository's
secrets are encrypted to, the first line of <host>/secrets/recipients.
The box has one key, made by the first run (the package's postinstall
runs this on a running system, and init after its install), kept in
/etc/motel/age/identity, root's alone; it is never replaced. On
standard output the key alone, so that from the laptop

    make secret APP=example VAR=DATABASE_URL BOX_KEY=$(ssh root@box.example.com moteld box key)

passes it; on standard error what it did. An identity that does not
read is refused and left as it is: move it aside, run this again, then
make rekey BOX_KEY=<the new key> in the repository.`,
				Args: cobra.NoArgs,
				RunE: func(c *cobra.Command, _ []string) error {
					return runKey(os.Geteuid(), newApplier(consoleLog("box.key")), c.OutOrStdout(), c.ErrOrStderr())
				},
			})
		},
	})
}

// `moteld init` stands alone: the console's one step in a box's life
// on the normal path, beside run and reload rather than under box.
func init() {
	caddycmd.RegisterCommand(caddycmd.Command{
		Name:  "init",
		Usage: "<sha>",
		Short: "Install this directory's Caddyfile as the box's, and its first baseline (root)",
		Long: `Run as root inside the box's directory, copied over from the repository
(deploy.example.com/ for the box at deploy.example.com; init refuses a
directory with any other name), with the commit you are about to push. From the repository on your laptop, where the
$(...) is expanded (the box has no git), copying the commit's files
rather than the working tree's:

    git archive HEAD deploy.example.com | ssh root@box.example.com tar -xf -
    ssh root@box.example.com "cd deploy.example.com && moteld init $(git rev-parse HEAD)"

It reads ./Caddyfile the way the box reads every push, records its
path in the repository (<host>/Caddyfile, from its box_webhook host),
installs it as /etc/motel/Caddyfile and reloads moteld. The reload
is the check: a file motel will not load is rolled back, the old
configuration serving throughout, and init exits non-zero. moteld
must be running (on a new box: systemctl enable --now moteld); if it
is not, init refuses and says how to start it and run init again. From then on the box applies only signed commits that descend
from <sha>. Running it again re-seeds the box: a new host (its
directory renamed with it), or the way back from a Caddyfile that
lists no signer.`,
		CobraFunc: func(cmd *cobra.Command) {
			cmd.Args = cobra.ExactArgs(1)
			cmd.RunE = func(c *cobra.Command, args []string) error {
				return runInitCmd(c.Context(), os.Geteuid(), newApplier(consoleLog("box.init")), ".", args[0], c.OutOrStdout())
			}
		},
	})
}

// commitArg holds init's and baseline's argument to a commit id as the
// box records one: 40 lowercase hex.
func commitArg(sha string) error {
	if !proof.IsID(sha) {
		return fmt.Errorf("%s is not a commit id: 40 lowercase hex, as git rev-parse HEAD prints one", proof.Bound(sha))
	}
	return nil
}

// consoleLog is the logger of a console command, which writes to the
// terminal it runs in: warnings and errors (recovery's, a rollback's),
// not the per-push info line the applier's journal carries — the
// command prints its outcome itself.
func consoleLog(name string) *zap.Logger {
	return caddy.Log().Named(name).WithOptions(zap.IncreaseLevel(zapcore.WarnLevel))
}

// printf writes one of the console's lines.
func printf(w io.Writer, format string, args ...any) error {
	_, err := fmt.Fprintf(w, format, args...)
	return err
}

// shortSHA is a commit id as the console's lines print it.
func shortSHA(sha string) string { return sha[:min(len(sha), 7)] }

// runApply is `moteld box apply`: as root, one run.
func runApply(ctx context.Context, euid int, a *Applier) error {
	if euid != 0 {
		return errors.New("moteld box apply runs as root: it is motel-box-apply.service's command")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return a.Run(ctx)
}

// webhookURL is `moteld box webhook`: the walk on the file at path,
// read whole at most the Caddyfile cap. The path is the operator's own
// argument, so unlike the box's state files it may be a pipe — `<(git
// show HEAD:deploy.example.com/Caddyfile)`, /dev/stdin — and is read
// until its end.
func webhookURL(path string) (string, error) {
	shape, err := readShape(path)
	if err != nil {
		return "", err
	}
	return "https://" + shape.Host + "/", nil
}

// describe is `moteld box describe`: the walk's reading of the file at
// path, one line per fact, then the secrets beside it in the working
// tree (describe_secrets.go, whose header gives those lines). Every
// value is held to a grammar by the walk (a lowercase hostname,
// liveswap's app names, a principal, a key type, a canonical base64) or
// by the layout, so none can break a line. It refuses what root's
// reading of the file and the layout refuse, and the consistency checks
// of "Which checks run where".
func describe(path string, w io.Writer) error {
	shape, err := readShape(path)
	if err != nil {
		return err
	}
	ts, err := readTreeSecrets(path)
	if err != nil {
		return err
	}
	if err := secretsFit(shape, ts.secrets, path); err != nil {
		return err
	}
	if err := ts.measure(); err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "host %s\n", shape.Host)
	for _, app := range shape.Apps {
		fmt.Fprintf(&b, "app %s\n", app)
	}
	for _, s := range shape.Signers {
		fmt.Fprintf(&b, "signer %s %s %s\n", s.Principal, s.Type, s.B64)
	}
	describeAccess(&b, shape)
	for _, line := range ts.lines() {
		fmt.Fprintf(&b, "%s\n", line)
	}
	_, err = io.WriteString(w, b.String())
	return err
}

// describeAccess prints the people and the grants after the signers,
// for a block with a person or a grant line only, so a file without
// one describes as before: a `person` line per key a person's token
// may be signed with, by its fingerprint, ending `signer` when the key
// is a signer line's (an ssh-rsa signer key is not a person's and
// prints none); a `grant` line per grant, its addresses in their one
// spelling (a lone address as an address); then a
// `warning` line for each thing the lines were taken with a doubt for.
func describeAccess(b *strings.Builder, shape *Shape) {
	if !shape.Access.Declared() {
		return
	}
	for _, k := range shape.Access.Keys {
		if !k.Person() {
			continue
		}
		fmt.Fprintf(b, "person %s %s %s", k.Name, k.Type, k.Fingerprint)
		if k.Signer {
			b.WriteString(" signer")
		}
		b.WriteString("\n")
	}
	for _, g := range shape.Access.Grants {
		fmt.Fprintf(b, "grant %s\n", strings.Join(g.Args(), " "))
	}
	for _, w := range shape.AccessWarnings {
		fmt.Fprintf(b, "warning %s\n", w)
	}
}

// readShape walks the file at path, read whole at most the Caddyfile
// cap.
func readShape(path string) (*Shape, error) {
	f, err := os.Open(path) //nolint:gosec // the operator's own CLI argument
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only
	file, err := readCapped(f, path, proof.MaxCaddyfile)
	if err != nil {
		return nil, err
	}
	shape, err := Walk(file)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return shape, nil
}
