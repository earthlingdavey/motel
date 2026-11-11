package main

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"

	"github.com/smallhoursorg/motel/notify"
)

// metricsStateDir is the directory vmalert reads rules.yml from,
// setup's (notify.DefaultMetricsStateDir): a config with no
// metrics_store block names none. A variable only so a test can stand
// another in.
var metricsStateDir = notify.DefaultMetricsStateDir

// retireMetricsRules is the gate's at `motel run` alone, with the config
// Caddy is about to load: one with no metrics_store block builds no
// MetricsStore, so nothing of notify would retire the rules a config
// with one wrote, and vmalert and the store, setup's units, would go on
// evaluating them after an edit and a restart (Copilot round 4 on
// #423). A config with the block is left alone: its Start writes its
// rules, and a shutdown keeps them. A reload that takes the block away
// is the pusher's (notify/metricspush.go, release); validate writes
// nothing. A config that is not JSON is Caddy's to refuse.
func retireMetricsRules(cfg []byte, stderr io.Writer) {
	var c struct {
		Apps map[string]json.RawMessage `json:"apps"`
	}
	if json.Unmarshal(cfg, &c) != nil {
		return
	}
	if _, ok := c.Apps["metrics_store"]; ok {
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
