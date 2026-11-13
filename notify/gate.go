package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

// SendNothing is the gate's at `motel run` with a config that runs no
// notify (no monitor block, or a metrics line alone): such a config
// starts no notify App, so nothing in it would take back the render a
// config with channels left in the state directory, and Alertmanager,
// still running it across motel's restart, would end what it holds at
// resolve_timeout with a recovery that did not happen on every channel
// (the reload's case is engine.release's). It writes setup's config
// that sends nothing into dir, setup's state directory, removes the URL
// files, and has Alertmanager on socket reload only when that changed anything: a
// box whose block never had channels starts no Alertmanager for it. It
// reports whether it changed anything; a box Alertmanager is not set up
// on has nothing to change, and is no error.
func SendNothing(dir, socket string) (bool, error) {
	changed, _, err := writeState(dir, defaultConfig, map[string]string{})
	if errors.Is(err, errNotSetUp) || err == nil && !changed {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), amRequestTimeout)
	defer cancel()
	err = newAMClient(socket).reload(ctx)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
		return true, nil // not running: it reads the file when it starts
	}
	return true, err
}

// SendNothingMessage is what the gate says when SendNothing changed the
// state directory.
const SendNothingMessage = "monitor: this config has no monitor block with channels: Alertmanager was given the config that sends nothing, so what it held ends without a message"

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
