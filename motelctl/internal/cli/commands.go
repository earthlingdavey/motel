package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
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
			v := ClientVersion{Version: r.env.Version, Protocols: []int{Protocol}, TokenForms: []string{strings.TrimSuffix(sshkey.Envelope, ".")}}
			if r.json {
				b, err := json.Marshal(v)
				if err != nil {
					return err
				}
				return r.emit(b)
			}
			_, err := fmt.Fprintf(r.env.Stdout, "motelctl %s\nprotocol %d (person token %s)\n", v.Version, Protocol, v.TokenForms[0])
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
			"It sends nothing. A box takes ed25519 and ecdsa keys, hardware ones included,\n" +
			"never RSA.\n\n" + experimental,
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
			q := url.Values{}
			if cmd.Flags().Changed("range") {
				if !slices.Contains(wire.MetricsRanges, rng) {
					return &client.Failure{Exit: client.ExitUsage, Message: fmt.Sprintf("--range %q is not one of %s", termtext.From(rng), strings.Join(wire.MetricsRanges, ", "))}
				}
				q.Set("range", rng)
			}
			c, err := r.client(cmd.Context(), &t, app.Box, "this app's")
			if err != nil {
				return err
			}
			ans, err := c.Get(cmd.Context(), app.Path, q)
			if err != nil {
				return fail(err)
			}
			var st wire.Status
			if err := c.Read(ans, &st); err != nil {
				return err
			}
			if r.json {
				return r.emit(ans.Body)
			}
			return render.WriteStatus(r.env.Stdout, r.style(), statusView(st))
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
			q := url.Values{}
			if len(args) == 2 {
				if err := client.CheckVersion(args[1]); err != nil {
					return err
				}
				q.Set("deploy", args[1])
			}
			c, err := r.client(cmd.Context(), &t, app.Box, "this app's")
			if err != nil {
				return err
			}
			ans, err := c.Get(cmd.Context(), app.Path, q)
			if err != nil {
				return fail(err)
			}
			if len(args) == 2 {
				var d wire.DeployResult
				if err := c.Read(ans, &d); err != nil {
					return err
				}
				if r.json {
					return r.emit(ans.Body)
				}
				return render.WriteDeploy(r.env.Stdout, r.style(), termtext.From(app.Name), deployView(d))
			}
			var st wire.Status
			if err := c.Read(ans, &st); err != nil {
				return err
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
			c, err := r.client(cmd.Context(), &t, nil, "the box block's")
			if err != nil {
				return err
			}
			ans, err := c.Get(cmd.Context(), "/", nil)
			if err != nil {
				return fail(err)
			}
			var b wire.BoxStatus
			if err := c.Read(ans, &b); err != nil {
				return err
			}
			if r.json {
				return r.emit(ans.Body)
			}
			return render.WriteBox(r.env.Stdout, boxView(b))
		},
	}
	t.flags(c)
	return c
}
