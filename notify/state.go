package notify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// errNotSetUp is a state directory that is not there: `motel notify
// setup` has not run, so there is no Alertmanager to configure.
var errNotSetUp = errors.New("alertmanager is not set up on this box (run motel notify setup as root)")

// writeState writes what render made into dir: each URL file, then the
// config, each to a temporary beside it and renamed over the name, so
// Alertmanager never reads a config naming a file not yet written, or
// half a file. Files are 0640; dir is setgid prometheus (setup), so
// they are born group prometheus. A URL file no longer named is removed
// after the config that named it is gone. It reports whether anything
// changed, and the config's previous bytes (nil if there was none) for
// a rollback.
func writeState(dir string, cfg []byte, files map[string]string) (changed bool, previous []byte, err error) {
	st, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil, errNotSetUp
	}
	if err != nil {
		return false, nil, err
	}
	if !st.IsDir() {
		return false, nil, fmt.Errorf("%s is not a directory", dir)
	}
	for name, value := range files {
		c, err := writeIfChanged(dir, name, []byte(value))
		if err != nil {
			return false, nil, err
		}
		changed = changed || c
	}
	previous, err = os.ReadFile(filepath.Join(dir, ConfigName)) //nolint:gosec // motel's own state dir
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, nil, err
	}
	c, err := writeIfChanged(dir, ConfigName, cfg)
	if err != nil {
		return false, nil, err
	}
	changed = changed || c
	entries, err := os.ReadDir(dir)
	if err != nil {
		return changed, previous, err
	}
	for _, e := range entries {
		name := e.Name()
		if _, keep := files[name]; keep || !strings.HasPrefix(name, "url-") {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return changed, previous, err
		}
		changed = true
	}
	return changed, previous, nil
}

func writeIfChanged(dir, name string, data []byte) (bool, error) {
	path := filepath.Join(dir, name)
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) { //nolint:gosec // motel's own state dir
		return false, nil
	}
	f, err := os.CreateTemp(dir, "."+name+".tmp-")
	if err != nil {
		return false, err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err := f.Chmod(0o640); err != nil {
		_ = f.Close()
		return false, err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return false, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return false, err
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, path)
}

// apply writes the rendered state and has Alertmanager load it. When
// Alertmanager refuses the new config (it should not: the tests run
// amtool on every shape), the previous file is put back, so a restart
// of Alertmanager starts from a config it loads. An unreachable
// Alertmanager is not an error here: it reads the file when it next
// starts, which socket activation does on motel's first post.
func apply(ctx context.Context, dir string, cfg []byte, files map[string]string, am *amClient) (reloaded bool, err error) {
	changed, previous, err := writeState(dir, cfg, files)
	if err != nil || !changed {
		return false, err
	}
	err = am.reload(ctx)
	var refused *amStatusError
	if errors.As(err, &refused) {
		if previous != nil {
			if _, werr := writeIfChanged(dir, ConfigName, previous); werr != nil {
				return false, fmt.Errorf("%w; and putting the previous config back: %v", err, werr) //nolint:errorlint // the first error is the one to match
			}
		}
		return false, err
	}
	if err != nil {
		return false, nil //nolint:nilerr // not running yet: it loads the file when socket activation starts it
	}
	return true, nil
}
