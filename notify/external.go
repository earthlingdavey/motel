package notify

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.uber.org/zap"
)

// An external producer's state (`motel monitor send --state firing`)
// outlives a restart of motel in externalName, in the state directory,
// as liveswap's watchdog keeps what it told in each app's alerts.json
// (DESIGN-notify.md, "Behaviour"): nothing else would report the key
// again, so Alertmanager would end the alert at resolve_timeout with a
// recovery that did not happen, and the producer's later resolve would
// find nothing. Written on each change, removed when none stands, read
// once per process at notify's first Start and told again as standing
// reports; forgotten when the block goes (a reload's release, or
// SendNothing at a start without it).
const externalName = "external.json"

// sourceExternal stamps a producer's source.
const sourceExternal = "external:"

// externalFile is externalName.
type externalFile struct {
	States []externalState `json:"states"`
}

// externalState is one key and what it last said: the event as the
// engine holds it (its summary and details already filtered), and
// whether it fires (false: its recovery's hold runs).
type externalState struct {
	Source   string            `json:"source"`
	App      string            `json:"app,omitempty"`
	Name     string            `json:"name"`
	Instance string            `json:"instance,omitempty"`
	Severity string            `json:"severity"`
	Summary  string            `json:"summary"`
	Details  map[string]string `json:"details,omitempty"`
	Firing   bool              `json:"firing"`
}

// loadExternal reads path: a file that is not there is nothing
// standing. It returns the bytes read, which are what the engine wrote
// last.
func loadExternal(path string) ([]externalState, []byte, error) {
	data, err := os.ReadFile(path) //nolint:gosec // motel's own state dir
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var f externalFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, nil, err
	}
	for _, s := range f.States {
		if !strings.HasPrefix(s.Source, sourceExternal) || s.Name == "" {
			return nil, nil, fmt.Errorf("an entry is not a producer's state (source %q, name %q)", s.Source, s.Name)
		}
	}
	return f.States, data, nil
}

// restoreExternal tells again, as standing reports, what the file says
// was told before this process, and points the writes of every later
// change at it. Called with mu held, at the first configure of a config
// with the block; the entries of an app the config does not run are
// resolved, as at a reload (prune).
func (e *engine) restoreExternal(apps []string) {
	path := filepath.Join(theStateDir, externalName)
	states, data, err := loadExternal(path)
	for _, s := range states {
		e.reportLocked(Event{Name: s.Name, Kind: kindState, Severity: s.Severity, App: s.App, Instance: s.Instance,
			Source: s.Source, Summary: s.Summary, Details: s.Details, Firing: s.Firing, Standing: true})
	}
	// Set after the reports, so that they write nothing until the table
	// holds all of them.
	e.externalPath, e.externalSaved = path, data
	if err != nil {
		// Removed, so the warning is said once: nothing stands now.
		e.logger.Warn("monitor: the producers' states kept before this start could not be read; taken as none, and removed",
			zap.String("file", path), zap.Error(err))
		e.writeExternal(nil)
	}
	e.prune(apps)
}

// persistExternal writes the producers' states when they changed since
// the last write. Called with mu held; nothing before the restore,
// whose file is the last process's.
func (e *engine) persistExternal() {
	if e.externalPath == "" {
		return
	}
	var out []externalState
	for _, st := range e.states {
		if ev := st.ev; strings.HasPrefix(ev.Source, sourceExternal) {
			out = append(out, externalState{Source: ev.Source, App: ev.App, Name: ev.Name, Instance: ev.Instance,
				Severity: ev.Severity, Summary: ev.Summary, Details: ev.Details, Firing: st.firing})
		}
	}
	var data []byte
	if len(out) > 0 {
		slices.SortFunc(out, func(a, b externalState) int {
			return cmp.Or(strings.Compare(a.Source, b.Source), strings.Compare(a.App, b.App),
				strings.Compare(a.Name, b.Name), strings.Compare(a.Instance, b.Instance))
		})
		var err error
		if data, err = json.Marshal(externalFile{States: out}); err != nil {
			return // strings and a map of strings: it does not fail
		}
	}
	if bytes.Equal(data, e.externalSaved) {
		return
	}
	if e.writeExternal(data) {
		e.externalSaved = data
	}
}

// writeExternal replaces the file with data, or removes it for nil. It
// reports false only when the state directory is not there (a box setup
// has not run on, which `send` already says), so that the next tick
// writes again once setup has made it. Any other failure is a warning,
// and the file stays as it was until the next change, as alerts.json
// does.
func (e *engine) writeExternal(data []byte) bool {
	if data == nil {
		if err := removeExternal(e.externalPath); err != nil {
			e.logger.Warn("monitor: the producers' states could not be kept for a restart: until the next change, a restart tells what the file still says",
				zap.String("file", e.externalPath), zap.Error(err))
		}
		return true
	}
	err := writeExternalFile(e.externalPath, data)
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	if err != nil {
		e.logger.Warn("monitor: the producers' states could not be kept for a restart: until the next change, a restart tells what the file still says",
			zap.String("file", e.externalPath), zap.Error(err))
	}
	return true
}

// writeExternalFile replaces path with data as liveswap's writeOwnFile
// does: a fresh 0600 temp file beside it, renamed over the name, and no
// flush, since the write runs under the engine's lock. A crash before
// the rename leaves the temp file; a power loss after it may leave an
// empty file, which the next start says and takes as nothing.
func writeExternalFile(path string, data []byte) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".external-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp) // best effort: the write's own error is the one to report
		}
	}()
	if _, err = f.Write(data); err != nil {
		_ = f.Close() // the write already failed; its error is the one to report
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// removeExternal forgets the producers' states kept at path: none
// stands, or the block that took them is gone.
func removeExternal(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
