package notify

import (
	"os"
	"strings"
	"sync/atomic"

	"github.com/caddyserver/caddy/v2"
)

// What a load knows of the command it runs under (DESIGN-notify.md,
// "Secrets at load"). Caddy's provisioning cannot tell `motel validate`
// from a config load: cmd/motel tells notify which command this process
// runs, before Caddy's own command does anything.

// PushedSecretsEnv names, to `motel validate`, the secrets a box push
// carries in its secrets/_box/: a comma-separated list of names, never
// a value (box.NotifyPushedSecretsEnv, which cmd/motel's test holds
// equal to this). The box handler validates a push before root writes
// those values into _box.env, and cannot read them (only root opens
// them), so validate takes each named one as held by the box: it is not
// refused as missing, and its value, the one installed now being the
// one the push replaces, is not checked.
const PushedSecretsEnv = "MOTEL_NOTIFY_PUSHED_SECRETS" //nolint:gosec // G101: a variable's name, not a credential

// command is the motel command this process runs, as SetCommand was
// told; "" when nothing told it (a test, another program linking the
// module), which is held to every refusal.
var command atomic.Value

// SetCommand tells notify the motel command this process runs: cmd/motel
// calls it with its first argument before Caddy's own command runs.
func SetCommand(cmd string) { command.Store(cmd) }

func runningCommand() string {
	s, _ := command.Load().(string)
	return s
}

// pushedSecrets are the names PushedSecretsEnv lists, read in `motel
// validate` alone: in any other command the variable is no word of the
// box's, and a load reads the secrets file as it is.
func pushedSecrets() map[string]bool {
	if runningCommand() != "validate" {
		return nil
	}
	out := map[string]bool{}
	for _, k := range strings.Split(os.Getenv(PushedSecretsEnv), ",") {
		if k != "" {
			out[k] = true
		}
	}
	return out
}

// dropsChannels reports whether this load drops a channel whose secret
// it cannot use (missing from the secrets file, or a value the URL
// check refuses) instead of refusing the config: the first load of
// `motel run`, with no config running. That load is a boot, or a
// restart, and refusing it would leave every site down for an alert
// channel; validate and a reload refuse, a refused reload keeping the
// running config, as cmd/motel's gate does for its own rules.
func dropsChannels() bool { return runningCommand() == "run" && !configRunning() }

// configRunning reports whether Caddy runs a config now: none before a
// process's first load has started, nor after a stop. A variable, so a
// test can stand in for a running one.
var configRunning = func() bool { return caddy.ActiveContext().Context != nil }
