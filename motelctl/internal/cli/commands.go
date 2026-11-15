package cli

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/smallhoursorg/motel/liveswap/wire"
	"github.com/smallhoursorg/motel/motelctl/internal/client"
	"github.com/smallhoursorg/motel/motelctl/internal/render"
	"github.com/smallhoursorg/motel/motelctl/internal/sshkey"
	"github.com/smallhoursorg/motel/motelctl/internal/termtext"
)

// ClientVersion is `motelctl version --json`: the client's release,
// and the protocols and token forms it speaks.
type ClientVersion struct {
	// Version is motelctl's release.
	Version string `json:"version"`
	// Protocols are the Motel-Protocol numbers it speaks.
	Protocols []int `json:"protocols"`
	// TokenForms are the person-token envelopes it can make.
	TokenForms []string `json:"token_forms"`
}

func (r *runner) version() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print motelctl's release and the protocol it speaks (experimental)",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(*cobra.Command, []string) error {
			v := ClientVersion{Version: r.env.Version, Protocols: []int{client.Protocol}, TokenForms: []string{strings.TrimSuffix(sshkey.Envelope, ".")}}
			if r.json {
				b, err := json.Marshal(v)
				if err != nil {
					return err
				}
				return r.emit(b)
			}
			_, err := fmt.Fprintf(r.env.Stdout, "motelctl %s\nprotocol %d (person token %s)\n", v.Version, client.Protocol, v.TokenForms[0])
			return err
		},
	}
}

// KeyInfo is `motelctl key --json`: the key motelctl would sign with,
// why that one, and the person line that lists it on a box.
type KeyInfo struct {
	// Key is the key as the person named it, or "the agent's key".
	Key string `json:"key"`
	// Source is how it was chosen: --key, MOTEL_KEY, git's
	// user.signingkey, the default file, the agent's only key.
	Source      string `json:"source"`
	Type        string `json:"type"`
	Fingerprint string `json:"fingerprint"`
	// PersonLine is the line the box's operator adds to the box block
	// of its Caddyfile; its name is <name> until --name gives one.
	PersonLine string `json:"person_line"`
}

func (r *runner) key() *cobra.Command {
	var keyFlag, name string
	c := &cobra.Command{
		Use:   "key",
		Short: "Say which ssh key motelctl signs with, and the person line that lists it (experimental)",
		Long: "key says which ssh key motelctl would sign a token with, why that one, and its\n" +
			"fingerprint; and prints the person line for the box's operator to add.\n" +
			"It sends nothing. A box takes ed25519 keys for now, hardware ones included:\n" +
			"not ECDSA yet, and never RSA. For either, key says why and prints no line.\n\n" + experimental,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if name != "" && !validName(name) {
				return &client.Failure{Exit: client.ExitUsage, Message: fmt.Sprintf("--name %q is not a name a box takes: letters, digits and . _ @ + -, at most 256 bytes", termtext.From(name))}
			}
			k, err := sshkey.Resolve(cmd.Context(), sshkey.Lookup{Flag: keyFlag, Getenv: r.env.Getenv, Home: r.env.Home, Dir: r.env.Dir, Run: r.env.Run})
			if err != nil {
				return keyFailure(err)
			}
			who := name
			if who == "" {
				who = "<name>"
			}
			info := KeyInfo{Key: k.Shown, Source: k.Source, Type: k.Public.Type, Fingerprint: k.Public.Fingerprint(), PersonLine: "person " + who + " " + k.Public.Line()}
			if r.json {
				b, err := json.Marshal(info)
				if err != nil {
					return err
				}
				return r.emit(b)
			}
			hint := ""
			if name == "" {
				hint = " (--name puts yours in place of <name>)"
			}
			_, err = fmt.Fprintf(r.env.Stdout, "key  %s (%s)  %s\nSend this line to the box's operator, for the box block of its Caddyfile%s:\n    %s\n",
				info.Key, info.Source, info.Fingerprint, hint, info.PersonLine)
			return err
		},
	}
	c.Flags().StringVar(&keyFlag, "key", "", "the ssh key to sign with: its .pub, or the private key with a .pub beside it")
	c.Flags().StringVar(&name, "name", "", "your name on the box, e.g. you@example.com")
	return c
}

// validName is a person's name as a box reads one: the signer
// principal's rule (box/proof/signers.go).
func validName(s string) bool {
	if len(s) > 256 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && !strings.ContainsRune("._@+-", rune(c)) {
			return false
		}
	}
	return true
}

func (r *runner) status() *cobra.Command {
	var t target
	var rng string
	c := &cobra.Command{
		Use:   "status <app>",
		Short: "An app's status: phase, version, watchdog, metrics, last deploy (experimental)",
		Long: "status reads an app's status: GET /<app> on the box. <app> is a name, or the\n" +
			"app's URL (https://deploy.example.com/blog), which names the box too.\n" +
			"It exits 0 when it read the status, whatever the app's phase.\n\n" + experimental,
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := client.ParseApp(args[0])
			if err != nil {
				return err
			}
			// The range is refused before the box is looked for, as
			// every argument is.
			if cmd.Flags().Changed("range") && !slices.Contains(wire.MetricsRanges, rng) {
				return &client.Failure{Exit: client.ExitUsage, Message: fmt.Sprintf("--range %q is not one of %s", termtext.From(rng), strings.Join(wire.MetricsRanges, ", "))}
			}
			c, err := r.client(cmd.Context(), &t, app.Box, "this app's", "")
			if err != nil {
				return err
			}
			st, ans, err := c.Status(cmd.Context(), app, rng)
			if err != nil {
				return fail(err)
			}
			if r.json {
				return r.emit(ans.Body)
			}
			return render.WriteStatus(r.env.Stdout, r.style(), statusView(st, app.Name))
		},
	}
	t.flags(c)
	c.Flags().StringVar(&rng, "range", "", "the metrics' range: "+strings.Join(wire.MetricsRanges, " or ")+" (the box's default is "+wire.MetricsRanges[0]+")")
	return c
}

func (r *runner) deploys() *cobra.Command {
	var t target
	c := &cobra.Command{
		Use:   "deploys <app> [<version>]",
		Short: "An app's deploys, newest first; or one deploy's whole record (experimental)",
		Long: "deploys lists the latest outcome of each version the box keeps a record for,\n" +
			"newest first (from GET /<app>). With a version, it prints that deploy's whole\n" +
			"record (GET /<app>?deploy=<version>): its phases, its error, and the app's\n" +
			"own words when it failed. --json on the list prints the status's deploys\n" +
			"array, [] when there is none.\n\n" + experimental,
		Args: usageArgs(cobra.RangeArgs(1, 2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := client.ParseApp(args[0])
			if err != nil {
				return err
			}
			if len(args) == 2 {
				if err := client.CheckVersion(args[1]); err != nil {
					return err
				}
			}
			c, err := r.client(cmd.Context(), &t, app.Box, "this app's", "")
			if err != nil {
				return err
			}
			if len(args) == 2 {
				d, ans, err := c.Deploy(cmd.Context(), app, args[1])
				if err != nil {
					return fail(err)
				}
				if r.json {
					return r.emit(ans.Body)
				}
				return render.WriteDeploy(r.env.Stdout, r.style(), termtext.From(app.Name), deployView(d))
			}
			st, ans, err := c.Status(cmd.Context(), app, "")
			if err != nil {
				return fail(err)
			}
			if r.json {
				// The one projection: the status's deploys as the box
				// wrote them, so a newer box's added fields pass.
				var raw struct {
					Deploys json.RawMessage `json:"deploys"`
				}
				if err := json.Unmarshal(ans.Body, &raw); err != nil {
					return err
				}
				if len(raw.Deploys) == 0 || string(raw.Deploys) == "null" {
					raw.Deploys = json.RawMessage("[]")
				}
				return r.emit(raw.Deploys)
			}
			return render.WriteDeploys(r.env.Stdout, summaryViews(st.Deploys))
		},
	}
	t.flags(c)
	return c
}

func (r *runner) boxStatus() *cobra.Command {
	var t target
	c := &cobra.Command{
		Use:   "status",
		Short: "The commit the box runs, its host, its Caddyfile's digest, its key for secrets (experimental)",
		Long: "box status reads the box webhook's GET / on the box's host: the commit the box\n" +
			"runs, its box_webhook host, the sha256 of the Caddyfile it runs, and its key\n" +
			"for secrets. A box with no baseline yet answers 409, which exits 1.\n\n" + experimental,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := r.client(cmd.Context(), &t, nil, "the box block's", "")
			if err != nil {
				return err
			}
			b, ans, err := c.BoxStatus(cmd.Context())
			if err != nil {
				return fail(err)
			}
			if r.json {
				return r.emit(ans.Body)
			}
			return render.WriteBox(r.env.Stdout, boxView(b, c.Box.Host))
		},
	}
	t.flags(c)
	return c
}

func (r *runner) whoami() *cobra.Command {
	var t target
	c := &cobra.Command{
		Use:   "whoami",
		Short: "Who the box takes your key to be, and what it may view from here (experimental)",
		Long: "whoami asks the box who your ssh key is there: GET /_motel/whoami. It prints\n" +
			"the name the box lists the key under, the box asked and its release, the key\n" +
			"this command signed with and where motelctl found it, the roles it holds\n" +
			"from the address you ask from, and when the token expires. An answer that\n" +
			"names another key, or another box, is warned of and exits 3: it is not one\n" +
			"to act on. It is asked with your own key only: with MOTEL_TOKEN set, name\n" +
			"the key with --key.\n\n" + experimental,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := r.client(cmd.Context(), &t, nil, "", "whoami")
			if err != nil {
				return err
			}
			me, ans, err := c.WhoAmI(cmd.Context())
			if err != nil {
				return fail(err)
			}
			view := whoamiView(me, c.Box.Host, c.Cred.(*client.PersonToken).Key, r.env.Now())
			odd := view.KeyDiffers || view.BoxDiffers
			if r.json && !odd {
				return r.emit(ans.Body)
			}
			if !r.json {
				if err := render.WriteWhoAmI(r.env.Stdout, view); err != nil {
					return err
				}
			}
			if odd {
				return &client.Failure{Exit: client.ExitUnknown, Message: fmt.Sprintf(
					"%s answered about another key than the one this command signed with, or as another box than the one it asked: an answer that is not about this key here is not one to act on.", c.Box.Host)}
			}
			return nil
		},
	}
	t.flags(c)
	return c
}

func (r *runner) apps() *cobra.Command {
	var t target
	c := &cobra.Command{
		Use:   "apps",
		Short: "The apps your key may view on the box, one a line (experimental)",
		Long: "apps lists the apps your ssh key may view on the box from the address you ask\n" +
			"from: GET /_motel/apps. One name a line; none is said on stderr, with nothing\n" +
			"on stdout. It is asked with your own key only: with MOTEL_TOKEN set, name\n" +
			"the key with --key.\n\n" + experimental,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := r.client(cmd.Context(), &t, nil, "", "apps")
			if err != nil {
				return err
			}
			list, ans, err := c.Apps(cmd.Context())
			if err != nil {
				return fail(err)
			}
			if r.json {
				return r.emit(ans.Body)
			}
			if len(list.Apps) == 0 {
				_, err := fmt.Fprintf(r.env.Stderr, "motelctl: %s lists no app your key may view from this address.\n", c.Box.Host)
				return err
			}
			return render.WriteNames(r.env.Stdout, appsView(list))
		},
	}
	t.flags(c)
	return c
}
