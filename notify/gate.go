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
			if missing(a.Metrics.stateDir()) {
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

// ReadsSecrets reports whether a config's monitor app (its JSON) reads
// the box's secrets file at its load: every block that runs notify, all
// but a metrics line alone.
func ReadsSecrets(monitor []byte) bool {
	var a App
	if len(monitor) == 0 || string(monitor) == "null" || json.Unmarshal(monitor, &a) != nil {
		return false
	}
	return !a.storeAlone()
}

// missing reports a state directory setup has not made.
func missing(dir string) bool {
	_, err := os.Stat(dir)
	return errors.Is(err, fs.ErrNotExist)
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
// this binary would write, comments aside: a change to a comment alone
// changes nothing in how the unit runs, and is not worth a warning at
// every load. A file that is not there is not stale: setup has not run,
// which LoadWarnings says otherwise, or root removed it.
func staleUnits(units []struct {
	path string
	data []byte
}) []string {
	var out []string
	for _, u := range units {
		b, err := os.ReadFile(filepath.Join(unitRoot, u.path))
		if err == nil && !bytes.Equal(uncommented(b), uncommented(u.data)) {
			out = append(out, u.path)
		}
	}
	return out
}

// uncommented is a unit file as systemd reads it: each line trimmed,
// without its blank lines and its comment lines (# and ;), so that a
// reformat changes nothing.
func uncommented(b []byte) []byte {
	var out []byte
	for line := range bytes.Lines(b) {
		t := bytes.TrimSpace(line)
		if len(t) == 0 || t[0] == '#' || t[0] == ';' {
			continue
		}
		out = append(append(out, t...), '\n')
	}
	return out
}
