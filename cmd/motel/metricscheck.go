package main

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"

	"github.com/smallhoursorg/motel/notify"
)

// metricsStateDir is the directory vmalert reads rules.yml from,
// setup's (notify.DefaultMetricsStateDir): a config with no metrics
// line names none. A variable only so a test can stand another in.
var metricsStateDir = notify.DefaultMetricsStateDir

// retireMetricsRules is the gate's at `motel run` alone, with the config
// Caddy is about to load: one with no monitor block, or a monitor block
// with no metrics line, builds no MetricsStore, so nothing of notify
// would retire the rules a config with one wrote, and vmalert and the
// store, setup's units, would go on evaluating them after an edit and a
// restart (Copilot round 4 on #423). A config with the line is left
// alone: its Start writes its rules, and a shutdown keeps them. A reload
// that takes the line away is the pusher's (notify/metricspush.go,
// release); validate writes nothing. A config that is not JSON, or
// whose monitor app is not an object, is Caddy's to refuse.
func retireMetricsRules(cfg []byte, stderr io.Writer) {
	var c struct {
		Apps struct {
			Monitor *struct {
				Metrics json.RawMessage `json:"metrics"`
			} `json:"monitor"`
		} `json:"apps"`
	}
	if json.Unmarshal(cfg, &c) != nil {
		return
	}
	if m := c.Apps.Monitor; m != nil && len(m.Metrics) > 0 && string(m.Metrics) != "null" {
		return
	}
	path := filepath.Join(metricsStateDir, notify.RulesName)
	switch changed, err := notify.RetireRules(metricsStateDir); {
	case err != nil:
		_, _ = fmt.Fprintf(stderr, "ERROR: %s: %s: %v\n", notify.RulesNotRetiredMessage, path, err)
	case changed:
		_, _ = fmt.Fprintf(stderr, "%s: %s\n", notify.RulesRetiredMessage, path)
	}
}
