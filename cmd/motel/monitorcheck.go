package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/smallhoursorg/motel/notify"
)

// monitorJSON is a config's monitor app, nil for none or for a config
// that is not JSON, which is Caddy's to refuse.
func monitorJSON(cfg []byte) []byte {
	var c struct {
		Apps struct {
			Monitor json.RawMessage `json:"monitor"`
		} `json:"apps"`
	}
	if json.Unmarshal(cfg, &c) != nil || len(c.Apps.Monitor) == 0 || string(c.Apps.Monitor) == "null" {
		return nil
	}
	return c.Apps.Monitor
}

// warnMonitor says what notify.LoadWarnings finds of the config's
// monitor block (its JSON, nil for none): what setup has not made, at
// validate and reload, whose loads would journal it alone; units that
// differ from this binary's, at run too.
func warnMonitor(cmd string, monitor []byte, stderr io.Writer) {
	for _, w := range notify.LoadWarnings(cmd, monitor) {
		_, _ = fmt.Fprintf(stderr, "WARNING: %s\n", w)
	}
}

// reloadForces reports whether `motel reload` must load the config even
// when it is unchanged: a monitor block with channels reads the box's
// secrets file at every load, which the config does not hold, so a
// mended _box.env leaves the Caddyfile as it was, and Caddy's
// unchanged-config check would load nothing (it answers 200, saying
// `config is unchanged` in the server's journal alone), leaving the
// channels a boot dropped dropped. `systemctl reload motel`, which the
// box's push uses after it writes the secrets, passes --force for the
// same reason (packaging/motel.service); this is that, for the command
// an operator types. A --force the command already names, true or
// false, is left as it is.
func reloadForces(args []string, monitor []byte) bool {
	if !notify.ReadsSecrets(monitor) {
		return false
	}
	fs, ok := commandFlags("reload", args)
	return ok && !fs.Changed("force")
}
