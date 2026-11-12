package notify

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// What cmd/motel's gate says of a config's monitor block before Caddy's
// own command runs: what a load would say in the journal alone, said
// also to whoever runs `motel validate` or `motel reload`.

// unitRoot prefixes the paths of setup's units (tests).
var unitRoot = "/"

// LoadWarnings are, for a config's monitor app (its JSON, nil for none),
// the warnings of the gate's command: that a part of the block is not
// set up on this box (validate and reload: a load serves and journals
// it), and that units setup wrote differ from this binary's (those and
// run, so that the restart onto an upgraded binary journals it).
func LoadWarnings(cmd string, monitor []byte) []string {
	var a App
	if len(monitor) == 0 || string(monitor) == "null" || json.Unmarshal(monitor, &a) != nil {
		return nil
	}
	notifies := !a.storeAlone()
	var out []string
	if cmd != "run" {
		if notifies && !a.setUp() {
			out = append(out, "monitor: Alertmanager is not set up on this box: motel journals every event and delivers none until root runs motel monitor setup")
		}
		if a.Metrics != nil {
			if _, err := os.Stat(a.Metrics.stateDir()); errors.Is(err, fs.ErrNotExist) {
				out = append(out, "monitor: the metrics store is not set up on this box: nothing is pushed or evaluated until root runs motel monitor setup --metrics")
			}
		}
	}
	if notifies {
		if stale := staleUnits(amUnits()); len(stale) > 0 {
			out = append(out, "monitor: the units motel monitor setup wrote differ from this motel's ("+strings.Join(stale, ", ")+"): run motel monitor setup as root to put this version's in place")
		}
	}
	if a.Metrics != nil {
		if stale := staleUnits(metricsUnits()); len(stale) > 0 {
			out = append(out, "monitor: the units motel monitor setup --metrics wrote differ from this motel's ("+strings.Join(stale, ", ")+"): run motel monitor setup --metrics as root to put this version's in place")
		}
	}
	return out
}

// amUnits are Alertmanager's setup's unit files, as metricsUnits are
// the store's.
func amUnits() []struct {
	path string
	data []byte
} {
	return []struct {
		path string
		data []byte
	}{{dropInPath, unitDropIn}, {socketPath, unitSocket}}
}

// staleUnits is each unit file setup wrote that differs from the one
// this binary would write. A file that is not there is not stale: setup
// has not run, which LoadWarnings says otherwise, or root removed it.
func staleUnits(units []struct {
	path string
	data []byte
}) []string {
	var out []string
	for _, u := range units {
		b, err := os.ReadFile(filepath.Join(unitRoot, u.path))
		if err == nil && !bytes.Equal(b, u.data) {
			out = append(out, u.path)
		}
	}
	return out
}
