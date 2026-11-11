package liveswap

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// The on-disk layout every other file addresses an app's files
// through. Pure path arithmetic over one root and one app name: it
// holds no state, takes no lock, and knows nothing about the deploy
// pipeline, which is why it is not in app.go.

// appDirs is the on-disk layout for one app:
//
//	<root>/<app>/releases/<version>/   one dir per deployed version
//	<root>/<app>/shared/               persistent data, survives deploys
//	<root>/<app>/tmp/                  download staging
//	<root>/<app>/run/<nonce>/app.sock  one socket per instance, bound by the app; only run/<nonce>/ is in that instance's view
//	<root>/<app>/proxy/<nonce>.sock    the same inode, hard-linked by motel; what it dials (not in any view)
//	<root>/<app>/deploys/<version>.json  the latest deploy's outcome for that version, filtered (not in any view)
//	<root>/<app>/state.json            current version + process handle
//	<root>/<app>/deploying.json        a URL or push deploy in flight (deploying.go; not in any view)
//	<root>/<app>/alerts.json           what the watchdog reported to notify and has not ended (lifecycle.go; not in any view)
//	<root>/<app>/current -> releases/<version>   convenience symlink
type appDirs struct {
	root      string // the liveswap root every app lives under
	app       string
	releases  string
	shared    string
	tmp       string
	run       string
	proxy     string
	deploys   string
	state     string
	deploying string
	alerts    string
	current   string
}

func newAppDirs(root, name string) appDirs {
	app := filepath.Join(root, name)
	return appDirs{
		root:      root,
		app:       app,
		releases:  filepath.Join(app, "releases"),
		shared:    filepath.Join(app, "shared"),
		tmp:       filepath.Join(app, "tmp"),
		run:       filepath.Join(app, "run"),
		proxy:     filepath.Join(app, "proxy"),
		deploys:   filepath.Join(app, "deploys"),
		state:     filepath.Join(app, "state.json"),
		deploying: filepath.Join(app, "deploying.json"),
		alerts:    filepath.Join(app, "alerts.json"),
		current:   filepath.Join(app, "current"),
	}
}

// runDir is the one directory an instance may bind its socket in: its
// own, so that no other instance of the app — the one it replaces, or
// the one replacing it — can touch the name before motel pins it.
func (d appDirs) runDir(nonce string) string {
	return filepath.Join(d.run, nonce)
}

// socket is the path the instance identified by nonce listens on.
func (d appDirs) socket(nonce string) string {
	return filepath.Join(d.runDir(nonce), "app.sock")
}

// socketRef is how motel dials that instance: the socket, pinned
// under proxy/.
func (d appDirs) socketRef(nonce string) *socketRef {
	return newSocketRef(d.socket(nonce), filepath.Join(d.proxy, nonce+".sock"), d.runDir(nonce))
}

func (d appDirs) release(version string) string {
	// versionPathComponent: identity for valid tags, mechanical
	// traversal containment (and the analyzer-modeled sanitizer) for
	// anything else — validVersion remains the actual gate upstream.
	return filepath.Join(d.releases, versionPathComponent(version))
}

// linkAt returns the first of the app dir and dirs (its fixed subdirs)
// that is not the directory it names, and why: a link, dangling or
// not; anything else but a directory; or a path Lstat cannot answer
// for (EIO), since the reads after it might yet succeed through a link
// it missed. It returns "" when each is a directory or absent. An app
// name is one path element, so these are the only two levels below
// the root: with a link at neither, the path reaches the directory it
// names through whatever alias the root itself is — recordsDir's rule,
// read off Lstat rather than by resolving, so a dangling link cannot
// pass as the name it stands in for.
func (d appDirs) linkAt(dirs ...string) (string, error) {
	for _, p := range append([]string{d.app}, dirs...) {
		fi, err := os.Lstat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// absent: nothing to follow
		case err != nil:
			return p, fmt.Errorf("cannot be checked for a link: %w", err)
		case fi.Mode()&os.ModeSymlink != 0:
			return p, errors.New("is a link, which is not followed under the liveswap root")
		case !fi.IsDir():
			return p, errors.New("is not a directory")
		}
	}
	return "", nil
}

// notOwnDirFix is the remedy for what linkAt refuses, wherever that is
// reported.
const notOwnDirFix = "if a link or a file stands here, replace it with a real directory (to keep its contents on another disk, bind-mount that location at this path); if Lstat failed, the error says why, and nothing here needs replacing"

// fixedDirs is every dir ensure makes. deploys/ is not among them: the
// record store is optional, made on first use by recordsDir, and a
// failure there is a warning, not a deploy that cannot start.
func (d appDirs) fixedDirs() []string {
	return []string{d.releases, d.shared, d.tmp, d.run, d.proxy}
}

// writeRefusal refuses a deploy into an app dir that is not the
// directory it names — the app dir, or a dir ensure makes, that is a
// link (`<root>/blog -> shop`), a file, or unreadable (linkAt) — before
// the deploy reads or writes anything through it. Through such a link
// a deploy stages in shop's tmp/ and extracts into shop's releases/,
// where two apps can remove each other's releases, or leave one behind
// for the other to roll back to and run with its own data. The error
// names the path as this app spells it, never what a link resolves to.
func (d appDirs) writeRefusal() error {
	if p, why := d.linkAt(d.fixedDirs()...); p != "" {
		return fmt.Errorf("refusing to deploy: %s %w (fix: %s)", p, why, notOwnDirFix)
	}
	return nil
}

func (d appDirs) ensure() error {
	for _, dir := range d.fixedDirs() {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
	}
	return nil
}
