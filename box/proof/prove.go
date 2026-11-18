package proof

import (
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
)

// Caps on the path a bundle names (DESIGN-box.md, "Caps").
const (
	MaxPath  = 4096 // bytes
	MaxDepth = 32   // components
)

// SplitPath is the repository path's components, if it is a relative
// path of safe ones: at most MaxPath bytes and MaxDepth components,
// each non-empty, neither `.` nor `..`, with no control byte (NUL and
// newline included). The path is the workflow's claim of where the
// file sits in the commit; it is walked through the bundled trees and
// never touches a filesystem.
func SplitPath(path string) ([]string, error) {
	bad := refuse("bundle: path is not a relative path of safe components")
	if path == "" || len(path) > MaxPath || strings.ContainsFunc(path, isControl) {
		return nil, bad
	}
	parts := strings.Split(path, "/")
	if len(parts) > MaxDepth {
		return nil, bad
	}
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return nil, bad
		}
	}
	return parts, nil
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// ProveFile is step 13: the file is the committed file. The commit's
// root tree must be among the bundled trees and hash to the id the
// commit names; the path's components are walked through the trees;
// the last entry must be a blob (ModeFile or ModeExecutable) whose id
// is sha1("blob <n>\0" + file). A symlink, a submodule or a tree at the
// path is refused as not a regular file; a path that is not in the
// commit, or a file that is not the committed one, as "the file sent
// is not <path> in <sha>". Trees are keyed by id; Bundle guarantees a
// tree hashes to the id it is filed under.
func ProveFile(c *Commit, trees map[string]*Tree, path string, file []byte) error {
	parts, err := SplitPath(path)
	if err != nil {
		return err
	}
	shown := Bound(path)
	notHere := refuse("the file sent is not %s in %s", shown, oid(c.ID))
	t, err := walkTree(c, trees, parts[:len(parts)-1], shown, notHere)
	if err != nil {
		return err
	}
	e, ok, err := t.entry(parts[len(parts)-1])
	if err != nil {
		return err
	}
	if !ok {
		return notHere
	}
	switch e.Mode {
	case ModeFile, ModeExecutable:
	default:
		return refuse("%s in %s is not a regular file", shown, oid(c.ID))
	}
	if ObjectID("blob", file) != e.ID {
		return notHere
	}
	return nil
}

// walkTree walks dirs — directory names, from the top of the commit —
// through the bundled trees and returns the tree at the end: the
// commit's root tree for none. A tree the walk needs and the bundle
// lacks is refused by id, naming shown as what it was needed for; a
// name the tree does not have, or has as anything but a directory, is
// absent.
func walkTree(c *Commit, trees map[string]*Tree, dirs []string, shown string, absent error) (*Tree, error) {
	t, err := bundledTree(c, trees, c.Tree, shown)
	if err != nil {
		return nil, err
	}
	for _, name := range dirs {
		e, ok, err := t.entry(name)
		if err != nil {
			return nil, err
		}
		if !ok || e.Mode != ModeDir {
			return nil, absent
		}
		if t, err = bundledTree(c, trees, e.ID, shown); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// bundledTree is the bundled tree of that id, or the refusal that names
// it as missing.
func bundledTree(c *Commit, trees map[string]*Tree, id, shown string) (*Tree, error) {
	t, ok := trees[id]
	if !ok || t.ID != id {
		return nil, refuse("bundle: tree %s, needed for %s in %s, is not in the bundle", oid(id), shown, oid(c.ID))
	}
	return t, nil
}

// Caps on a commit's secrets (DESIGN-box.md, "Caps" and "Proving the
// secrets").
const (
	MaxSecretApps = 32        // apps with a directory under secrets/
	MaxSecretVars = 256       // <VAR>.age files in one app's directory
	MaxVarName    = 128       // bytes in a VAR
	MaxSecretFile = 128 << 10 // one <VAR>.age file, encrypted
)

// Secrets is what a commit holds under secrets/ beside its Caddyfile,
// proven: every name in it, and every encrypted value as the bytes of
// the blob the commit's trees name.
type Secrets struct {
	// Tree is the secrets tree's id, or "" when the commit has none.
	Tree string
	// Apps are the app directories, in the tree's order (git's: by name).
	Apps []SecretApp
}

// SecretApp is one app's directory under secrets/.
type SecretApp struct {
	Name string
	Vars []SecretVar
}

// SecretVar is one <VAR>.age file: the variable's name, and its blob,
// still encrypted.
type SecretVar struct {
	Name string
	Blob string
	Data []byte
}

// Names are the grammars ProveSecrets holds directory and file names
// to. They are the box's to give — an app name is liveswap's, a
// variable name is what an env file can carry — and this package
// imports neither.
type Names struct {
	App func(string) bool
	Var func(string) bool
}

// ProveSecrets reads the secrets beside the Caddyfile at path from the
// commit's trees (DESIGN-box.md, "Proving the secrets"): the entry
// `secrets` in the Caddyfile's directory. No entry is no secrets; one
// that is not a directory is refused. In it, the one file is
// `recipients` — for the laptop: the box never reads it, and its blob
// need not be bundled — and every other entry is a directory named for
// an app; in an app's directory, every entry is a regular file
// `<VAR>.age`. Anything else — a symlink, a submodule, a directory one
// level too deep, a name outside the grammars — is refused by name.
// Every <VAR>.age blob must be bundled, so that what the commit holds
// is what the box sees: a file the tree no longer lists is a deletion
// the commit proves, and a blob the bundle lacks is refused, never read
// as one. ProveFile must have passed for the same path.
func ProveSecrets(c *Commit, trees map[string]*Tree, blobs map[string][]byte, path string, names Names) (*Secrets, error) {
	parts, err := SplitPath(path)
	if err != nil {
		return nil, err
	}
	dir := parts[:len(parts)-1]
	shownDir := Bound(strings.Join(append(slices.Clone(dir), "secrets"), "/"))
	dt, err := walkTree(c, trees, dir, Bound(path), refuse("the file sent is not %s in %s", Bound(path), oid(c.ID)))
	if err != nil {
		return nil, err
	}
	e, ok, err := dt.entry("secrets")
	if err != nil {
		return nil, err
	}
	if !ok {
		return &Secrets{}, nil
	}
	layout := NewSecretsLayout(shownDir, c.ID, names)
	if err := layout.Dir(e.Mode); err != nil {
		return nil, err
	}
	st, ok := trees[e.ID]
	if !ok || st.ID != e.ID {
		return nil, refuse("bundle: tree %s, needed for %s in %s, is not in the bundle", oid(e.ID), shownDir, oid(c.ID))
	}
	// One scan over the tree's bytes, as everywhere in the proof, through
	// the layout's one rule set (SecretsLayout).
	var bad error
	err = st.scan(func(mode, name, id []byte) bool {
		bad = layout.Entry(mode, name, id)
		return bad == nil
	})
	switch {
	case err != nil:
		return nil, err
	case bad != nil:
		return nil, bad
	}
	if err := layout.Done(); err != nil {
		return nil, err
	}
	out := &Secrets{Tree: st.ID}
	for _, a := range layout.Apps {
		app, err := proveSecretApp(c, trees, blobs, a.Name, a.ID, shownDir+"/"+a.Name, names)
		if err != nil {
			return nil, err
		}
		out.Apps = append(out.Apps, *app)
	}
	return out, nil
}

// proveSecretApp is one app's directory: its <VAR>.age files, each a
// bundled blob.
func proveSecretApp(c *Commit, trees map[string]*Tree, blobs map[string][]byte, name, id, shown string, names Names) (*SecretApp, error) {
	t, err := bundledTree(c, trees, id, shown)
	if err != nil {
		return nil, err
	}
	layout := NewAppLayout(name, shown, c.ID, names)
	var bad error
	err = t.scan(func(mode, file, id []byte) bool {
		bad = layout.Entry(mode, file, id)
		return bad == nil
	})
	switch {
	case err != nil:
		return nil, err
	case bad != nil:
		return nil, bad
	}
	if err := layout.Done(); err != nil {
		return nil, err
	}
	app := &SecretApp{Name: name}
	for _, v := range layout.Vars {
		data, ok := blobs[v.ID]
		if !ok || ObjectID("blob", data) != v.ID {
			return nil, refuse("bundle: blob %s, needed for %s in %s, is not in the bundle", oid(v.ID), v.Shown, oid(c.ID))
		}
		if len(data) > MaxSecretFile {
			return nil, refuse("%s", SecretFileTooLarge(v.Shown, int64(len(data))))
		}
		app.Vars = append(app.Vars, SecretVar{Name: v.Name, Blob: v.ID, Data: data})
	}
	return app, nil
}

// SecretFileTooLarge is the refusal of a secret's file over its cap.
func SecretFileTooLarge(shown string, size int64) string {
	return fmt.Sprintf("%s is %d bytes; the cap is %d", shown, size, MaxSecretFile)
}

// The layout of a secrets directory and of an app's (DESIGN-box.md,
// "Layout in the repository") is one rule set, fed one entry at a time
// in git's order — by a commit's trees (ProveSecrets) and by a working
// tree (`moteld box describe`) alike — so the two refuse the same
// entries, in the same words and in the same order. commit is where the
// entries were read, for the refusals' " in <sha>": a commit's id, or ""
// for a working tree. An entry's name becomes a string only once it is
// held to the layout, and only up to the cap, past which an entry is
// only counted (Caps: "tree entries materialised: none").

// LayoutRef is an entry the layout admitted: its name, its object's id
// from a commit ("" from a working tree), and how a refusal shows it.
type LayoutRef struct {
	Name, ID, Shown string
}

// SecretsLayout checks a secrets directory: `recipients` a regular
// file, every other entry a directory named for an app, at most
// MaxSecretApps of them.
type SecretsLayout struct {
	shown string
	in    inCommit
	names Names
	n     int
	// Apps are the app directories, in order.
	Apps []LayoutRef
}

// NewSecretsLayout begins the check of the secrets directory shown, read
// from commit ("" for a working tree).
func NewSecretsLayout(shown, commit string, names Names) *SecretsLayout {
	return &SecretsLayout{shown: shown, in: inCommit(commit), names: names}
}

// Dir holds the entry `secrets` itself to the layout: a directory.
func (l *SecretsLayout) Dir(mode string) error {
	if mode != ModeDir {
		return refuse("%s%s is not a directory", l.shown, l.in)
	}
	return nil
}

// Entry holds one entry to the layout: mode as git writes it, the raw
// object id or nil. The error is the layout's first refusal.
func (l *SecretsLayout) Entry(mode, name, id []byte) error {
	if string(name) == "recipients" {
		if m := string(mode); m != ModeFile && m != ModeExecutable {
			return refuse("%s/recipients%s is not an app's directory: recipients is the name of the recipients file, which is a regular file", l.shown, l.in)
		}
		return nil
	}
	if string(mode) != ModeDir {
		return refuse("%s/%s%s is not an app's directory: %s", l.shown, Bound(string(name)), l.in, modeWhat(string(mode)))
	}
	if l.n++; l.n > MaxSecretApps {
		return nil
	}
	a := string(name)
	if !l.names.App(a) {
		return refuse("%s/%s%s is not an app's directory: %s is not an app name", l.shown, Bound(a), l.in, Bound(a))
	}
	l.Apps = append(l.Apps, LayoutRef{Name: a, ID: hex.EncodeToString(id), Shown: l.shown + "/" + a})
	return nil
}

// Done is the check once every entry is in: the cap.
func (l *SecretsLayout) Done() error {
	if l.n > MaxSecretApps {
		return refuse("%d apps have secrets; the cap is %d", l.n, MaxSecretApps)
	}
	return nil
}

// AppLayout checks an app's directory: every entry a regular file
// `<VAR>.age`, VAR a name an env file can carry within MaxVarName, at
// most MaxSecretVars of them, and at least one.
type AppLayout struct {
	app, shown string
	in         inCommit
	names      Names
	n          int
	// Vars are the secrets, in order: Name is the variable's.
	Vars []LayoutRef
}

// NewAppLayout begins the check of app's directory, shown, read from
// commit ("" for a working tree).
func NewAppLayout(app, shown, commit string, names Names) *AppLayout {
	return &AppLayout{app: app, shown: shown, in: inCommit(commit), names: names}
}

// Entry holds one entry to the layout, as SecretsLayout.Entry does: the
// type of every entry, the name of those within the cap.
func (l *AppLayout) Entry(mode, file, id []byte) error {
	if m := string(mode); m != ModeFile && m != ModeExecutable {
		return refuse("%s/%s%s is not a secret: %s", l.shown, Bound(string(file)), l.in, modeWhat(m))
	}
	if l.n++; l.n > MaxSecretVars {
		return nil
	}
	f := string(file)
	where := l.shown + "/" + Bound(f)
	v, ok := strings.CutSuffix(f, ".age")
	switch {
	case !ok:
		return refuse("%s%s is not a secret: only <VAR>.age files are", where, l.in)
	case len(v) > MaxVarName:
		return refuse("%s%s is not a secret: its name is over %d bytes", where, l.in, MaxVarName)
	case !l.names.Var(v):
		return refuse("%s%s is not a secret: %s is not a variable name an env file can carry", where, l.in, Bound(v))
	}
	l.Vars = append(l.Vars, LayoutRef{Name: v, ID: hex.EncodeToString(id), Shown: where})
	return nil
}

// Done is the check once every entry is in: the cap, and a directory
// holding no secret.
func (l *AppLayout) Done() error {
	switch {
	case l.n > MaxSecretVars:
		return refuse("app %s has %d secrets; the cap is %d", l.app, l.n, MaxSecretVars)
	case l.n == 0:
		return refuse("%s%s is not an app's directory: it holds no secret", l.shown, l.in)
	}
	return nil
}

// modeWhat names what an entry is that the layout did not expect there:
// git's five modes, and anything else a working tree can hold.
func modeWhat(mode string) string {
	switch mode {
	case ModeFile, ModeExecutable:
		return "it is a file"
	case ModeSymlink:
		return "it is a symlink"
	case ModeSubmodule:
		return "it is a submodule"
	case ModeDir:
		return "it is a directory"
	}
	return "it is neither a file nor a directory"
}
