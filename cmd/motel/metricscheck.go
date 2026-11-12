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

// observeHandler is liveswap's response observer as a route's handler
// names it in the JSON (liveswap/observe.go).
const observeHandler = "liveswap_observe"

// checkObserved refuses a rule that names an app whose responses no
// route observes: a request threshold or a post_deploy reads the series
// an `observe <app>` handler counts, and with none they are never
// pushed, so the rule never fires. notify's Provision cannot tell (the
// HTTP app's routes may provision after it), so the gate asks the whole
// config: every handler under the HTTP app, at any depth of routes and
// subroutes. An every-app rule is every observed app's. A config that is
// not JSON, or whose monitor app is not an object, is Caddy's to refuse.
func checkObserved(cfg []byte) error {
	var c struct {
		Apps struct {
			Monitor *struct {
				Metrics *struct {
					Rules []notify.Rule `json:"rules"`
				} `json:"metrics"`
			} `json:"monitor"`
			HTTP json.RawMessage `json:"http"`
		} `json:"apps"`
	}
	if json.Unmarshal(cfg, &c) != nil || c.Apps.Monitor == nil || c.Apps.Monitor.Metrics == nil {
		return nil
	}
	observed := map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if x["handler"] == observeHandler {
				if app, ok := x["app"].(string); ok {
					observed[app] = true
				}
			}
			for _, e := range x {
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	var http any
	if len(c.Apps.HTTP) > 0 && json.Unmarshal(c.Apps.HTTP, &http) != nil {
		return nil
	}
	walk(http)
	for _, r := range c.Apps.Monitor.Metrics.Rules {
		if r.App != "" && r.ReadsResponses() && !observed[r.App] {
			return fmt.Errorf("monitor: rule %s reads app %s's responses, and no route has `observe %s`: its request numbers are never pushed, so the rule would never fire; add `observe %s` first in the app's site or route", r.Name, r.App, r.App, r.App)
		}
	}
	return nil
}
