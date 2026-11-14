package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"

	"go.yaml.in/yaml/v3"
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
		stale := staleUnits(metricsUnits())
		// With scrape.yml, which only setup writes: a box set up before
		// it changed scrapes as it did until setup runs again.
		scrape := filepath.Join(DefaultMetricsStateDir, scrapeName)
		switch same, err := sameYAML(filepath.Join(unitRoot, scrape), scrapeConfig); {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			// The reason alone: a read's error names the path again.
			var pe *fs.PathError
			if errors.As(err, &pe) {
				err = pe.Err
			}
			out = append(out, "monitor: "+scrape+" could not be compared with this motel's ("+err.Error()+"): whether motel monitor setup --metrics is due is not known")
		case !same:
			stale = append(stale, scrape)
		}
		// The proxy shape's units, which setup retires: a box upgraded
		// from it is told what the run will do beyond rewriting files.
		var proxy []string
		for _, u := range oldAMProxyUnits {
			if p := filepath.Join(unitDir, u); isFile(filepath.Join(unitRoot, p)) {
				proxy = append(proxy, p)
			}
		}
		var what []string
		if len(stale) > 0 {
			what = append(what, "the files motel monitor setup --metrics wrote differ from this motel's ("+strings.Join(stale, ", ")+")")
		}
		if len(proxy) > 0 {
			what = append(what, "the proxy an older setup wrote for Alertmanager ("+strings.Join(proxy, ", ")+") is still in place")
		}
		if len(what) > 0 {
			msg := "monitor: " + strings.Join(what, ", and ") + ": run motel monitor setup --metrics as root"
			switch {
			case len(proxy) == 0:
				msg += " to put this version's in place"
			case len(stale) > 0:
				msg += " to put this version's in place; it retires the proxy"
			default:
				msg += ", which retires it"
			}
			if len(proxy) > 0 {
				// Setup's own test (amListenInNamespace): a box whose
				// retirement was cut short may listen there already.
				msg += " and, unless Alertmanager listens in the store's namespace already, restarts (or starts) Alertmanager once so that it does"
			}
			out = append(out, msg)
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
// on has nothing to change, and is no error. The producers' states kept
// for a restart are forgotten with the block, as at a reload's
// (external.go): a later start with it must not tell them again.
func SendNothing(dir, socket string) (changed bool, err error) {
	defer func() { err = errors.Join(err, removeExternal(filepath.Join(dir, externalName))) }()
	changed, _, err = writeState(dir, defaultConfig, map[string]string{})
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

// sameYAML reports whether the YAML file at path holds the same document
// as want, comments and layout aside: indentation is structure in YAML,
// so uncommented's trim would hide a change. A file that is not YAML is
// not the same; a read error is returned.
func sameYAML(path string, want []byte) (bool, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: setup's own path, under unitRoot
	if err != nil {
		return false, err
	}
	var got, exp any
	if yaml.Unmarshal(b, &got) != nil {
		return false, nil
	}
	if err := yaml.Unmarshal(want, &exp); err != nil {
		// Tests parse the embedded file; a load warns, never panics.
		return false, fmt.Errorf("this motel's own copy is not YAML: %w", err)
	}
	return reflect.DeepEqual(got, exp), nil
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
