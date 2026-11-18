package proof

// The fixtures: a small repository's objects, made by git with SSH
// signing (gen_test.go), committed under testdata/ so every test runs
// from bytes git wrote, and remade live by TestLiveGit so the parser
// and git cannot drift apart unnoticed. The same table of checks runs
// over both sets.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// manifest is testdata/manifest.json: what the generator made.
type manifest struct {
	// Path is where the Caddyfile sits in the repository.
	Path string `json:"path"`
	// Cases name commits: base (the baseline), mid, head (two signed
	// commits above it), unsigned and mallory (above head: no
	// signature; a key not listed), bob (above head, a listed RSA
	// key), after_unsigned (signed, above unsigned), special (above
	// head: a symlink, a submodule and a directory beside the file),
	// orphan (a root commit on no branch of the baseline's).
	Cases map[string]fixtureCase `json:"cases"`
	// Signers are the listed keys, as signer-line arguments.
	Signers []string `json:"signers"`
	// Mallory is a key no signer line carries.
	Mallory string `json:"mallory"`
	// SHA256 is a commit from a SHA-256 repository, by its object name
	// under objects/.
	SHA256 string `json:"sha256"`
}

type fixtureCase struct {
	Commit string `json:"commit"`
	// File is the blob id of Path at that commit.
	File string `json:"file"`
}

// fixtures is a loaded set.
type fixtures struct {
	m       manifest
	objects map[string][]byte // every object by id (the SHA-256 one by its label)
	commits map[string]*Commit
	trees   map[string]*Tree
	signers Signers
	mallory Signer
}

func loadFixtures(t *testing.T, dir string) *fixtures {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("fixtures: %v (run `go test ./proof -run TestLiveGit -update` to make them)", err)
	}
	fx := &fixtures{objects: map[string][]byte{}, commits: map[string]*Commit{}, trees: map[string]*Tree{}}
	if err := json.Unmarshal(raw, &fx.m); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, "objects", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		fx.objects[e.Name()] = data
		if !IsID(e.Name()) {
			continue
		}
		switch {
		case ObjectID("commit", data) == e.Name():
			c, err := ParseCommit(data)
			if err != nil {
				t.Fatalf("commit %s: %v", e.Name(), err)
			}
			fx.commits[e.Name()] = c
		case ObjectID("tree", data) == e.Name():
			tr, err := ParseTree(data)
			if err != nil {
				t.Fatalf("tree %s: %v", e.Name(), err)
			}
			fx.trees[e.Name()] = tr
		case ObjectID("blob", data) == e.Name():
		default:
			t.Fatalf("object %s hashes to none of commit, tree or blob", e.Name())
		}
	}
	for _, line := range fx.m.Signers {
		fx.signers = append(fx.signers, parseSignerLine(t, line))
	}
	fx.mallory = parseSignerLine(t, fx.m.Mallory)
	return fx
}

func parseSignerLine(t *testing.T, line string) Signer {
	t.Helper()
	f := strings.Fields(line)
	if len(f) != 3 {
		t.Fatalf("signer line %q", line)
	}
	s, err := ParseSigner(f[0], f[1], f[2])
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (fx *fixtures) commit(t *testing.T, name string) *Commit {
	t.Helper()
	c, ok := fx.commits[fx.m.Cases[name].Commit]
	if !ok {
		t.Fatalf("no commit for case %q", name)
	}
	return c
}

// parentsFor is what the workflow bundles under parents/ for that
// case: first parents from its commit down to, not including, the
// baseline — or to the root, when the baseline is not an ancestor.
func (fx *fixtures) parentsFor(t *testing.T, name, baseline string) []*Commit {
	t.Helper()
	var out []*Commit
	cur := fx.commit(t, name)
	for len(cur.Parents) > 0 && cur.Parents[0] != baseline {
		p, ok := fx.commits[cur.Parents[0]]
		if !ok {
			break
		}
		out = append(out, p)
		cur = p
	}
	return out
}

func (fx *fixtures) file(t *testing.T, name string) []byte {
	t.Helper()
	b, ok := fx.objects[fx.m.Cases[name].File]
	if !ok {
		t.Fatalf("no file blob for case %q", name)
	}
	return b
}

// bundleFor builds the bundle the workflow would for that case: the
// file, HEAD, the first-parent chain down to (not including) the
// baseline, and every tree on the path.
func (fx *fixtures) bundleFor(t *testing.T, name, baseline string) []byte {
	t.Helper()
	head := fx.commit(t, name)
	files := map[string][]byte{
		"path":      []byte(fx.m.Path),
		"Caddyfile": fx.file(t, name),
		"commit":    head.Raw,
	}
	for i, p := range fx.parentsFor(t, name, baseline) {
		files[fmt.Sprintf("parents/%04d", i+1)] = p.Raw
	}
	id := head.Tree
	for _, comp := range strings.Split(fx.m.Path, "/") {
		tr, ok := fx.trees[id]
		if !ok {
			break
		}
		files["trees/"+id] = tr.Raw
		e, ok, err := tr.entry(comp)
		if err != nil || !ok {
			break
		}
		id = e.ID
	}
	return tgz(t, files)
}

// tgz is a gzip tarball of the files, in name order, as regular files.
func tgz(t testing.TB, files map[string][]byte) []byte {
	t.Helper()
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, n := range names {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(files[n])), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(files[n]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// needTools fails, rather than skips, when ssh-keygen or git is
// missing: both are in the dev container and on any laptop that
// clones, and a silent skip would leave the proof core's heart
// untested.
func needTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is not on PATH; the box depends on openssh-client and the fixtures on git", tool)
		}
	}
}

// refusalContaining fails unless err is a Refusal whose message holds
// every want.
func refusalContaining(t *testing.T, err error, want ...string) {
	t.Helper()
	var r *Refusal
	if !errors.As(err, &r) {
		t.Fatalf("want a refusal containing %q, got %v", want, err)
	}
	for _, w := range want {
		if !strings.Contains(r.Msg, w) {
			t.Fatalf("refusal %q does not contain %q", r.Msg, w)
		}
	}
}

// TestFixtures runs the table over the committed set.
func TestFixtures(t *testing.T) {
	runFixtureTable(t, loadFixtures(t, "testdata"))
}

// runFixtureTable is every check that depends on git-made objects.
func runFixtureTable(t *testing.T, fx *fixtures) {
	t.Helper()
	needTools(t, "ssh-keygen")
	ctx := context.Background()
	v := &Verifier{TempDir: t.TempDir()}
	baseline := fx.m.Cases["base"].Commit
	path := fx.m.Path

	t.Run("kinds", func(t *testing.T) {
		for name, kind := range map[string]SigKind{"base": SSHSig, "mid": SSHSig, "head": SSHSig, "unsigned": Unsigned, "mallory": SSHSig, "bob": SSHSig, "after_unsigned": SSHSig, "special": SSHSig, "orphan": SSHSig} {
			if got := fx.commit(t, name).Kind; got != kind {
				t.Errorf("%s: kind %v, want %v", name, got, kind)
			}
		}
	})

	t.Run("payload reassembles the object", func(t *testing.T) {
		// Payload plus the gpgsig header folded back in is Raw: pins
		// that the header is removed whole and nothing else moves.
		for name := range fx.m.Cases {
			c := fx.commit(t, name)
			if got := reassemble(c); !bytes.Equal(got, c.Raw) {
				t.Errorf("%s: payload + signature != raw\n%q\n%q", name, got, c.Raw)
			}
		}
	})

	t.Run("prove file", func(t *testing.T) {
		for _, name := range []string{"base", "mid", "head", "unsigned", "bob", "special", "orphan"} {
			if err := ProveFile(fx.commit(t, name), fx.trees, path, fx.file(t, name)); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
		head := fx.commit(t, "head")
		file := fx.file(t, "head")
		off := append([]byte{}, file...)
		off[len(off)-1] ^= 1
		refusalContaining(t, ProveFile(head, fx.trees, path, off), "the file sent is not "+path+" in "+head.ID)
		refusalContaining(t, ProveFile(head, fx.trees, path, append(file, '\n')), "the file sent is not")
		refusalContaining(t, ProveFile(head, fx.trees, path, fx.file(t, "mid")), "the file sent is not")
		refusalContaining(t, ProveFile(head, fx.trees, "box1/missing", file), "the file sent is not box1/missing in")
		refusalContaining(t, ProveFile(head, fx.trees, "nope/Caddyfile", file), "the file sent is not")
		dir, _, _ := strings.Cut(path, "/")
		sp := fx.commit(t, "special")
		refusalContaining(t, ProveFile(sp, fx.trees, dir+"/link", file), dir+"/link in "+sp.ID+" is not a regular file")
		refusalContaining(t, ProveFile(sp, fx.trees, dir+"/sub", file), "is not a regular file")
		refusalContaining(t, ProveFile(sp, fx.trees, dir+"/dir", file), "is not a regular file")
		refusalContaining(t, ProveFile(sp, fx.trees, dir, file), "is not a regular file")
		refusalContaining(t, ProveFile(sp, fx.trees, path+"/x", file), "the file sent is not")
		// The root tree withheld.
		refusalContaining(t, ProveFile(head, map[string]*Tree{}, path, file), "bundle: tree "+head.Tree+", needed for "+path+" in "+head.ID+", is not in the bundle")
		// A tree filed under the wrong id.
		wrong := map[string]*Tree{}
		for id, tr := range fx.trees {
			wrong[id] = tr
		}
		wrong[head.Tree] = fx.trees[fx.commit(t, "base").Tree]
		refusalContaining(t, ProveFile(head, wrong, path, file), "is not in the bundle")
	})

	t.Run("chain", func(t *testing.T) {
		head, mid, base := fx.commit(t, "head"), fx.commit(t, "mid"), fx.commit(t, "base")
		chain, err := Chain(head, fx.parentsFor(t, "head", baseline), baseline)
		if err != nil {
			t.Fatal(err)
		}
		if len(chain) != 2 || chain[0] != head || chain[1] != mid {
			t.Fatalf("chain: %v", chain)
		}
		if chain, err := Chain(base, nil, baseline); err != nil || len(chain) != 1 || chain[0] != base {
			t.Fatalf("head == baseline: %v %v", chain, err)
		}
		// mid is the baseline: head's chain is head alone.
		if chain, err := Chain(head, nil, mid.ID); err != nil || len(chain) != 1 {
			t.Fatalf("one above: %v %v", chain, err)
		}
		// A replay: base pushed when the box runs head.
		_, err = Chain(base, nil, head.ID)
		refusalContaining(t, err, baseline+" does not descend from the commit this box runs ("+head.ID+") along main's first-parent line", "moteld box baseline "+baseline)
		// Another history: the orphan root.
		_, err = Chain(fx.commit(t, "orphan"), nil, baseline)
		refusalContaining(t, err, "does not descend")
		// The bundle withholding mid: the walk ends early.
		_, err = Chain(head, nil, baseline)
		refusalContaining(t, err, "does not descend")
		// An object in mid's slot that is not mid.
		_, err = Chain(head, []*Commit{base}, baseline)
		refusalContaining(t, err, "bundle: parents/0001 is not the first parent of "+head.ID)
		// An object past the baseline.
		_, err = Chain(head, []*Commit{mid, base}, baseline)
		refusalContaining(t, err, "bundle: parents/0002 is past the end of the chain")
	})

	t.Run("verify", func(t *testing.T) {
		for _, name := range []string{"base", "mid", "head", "after_unsigned", "special", "orphan"} {
			p, err := v.Verify(ctx, fx.commit(t, name), fx.signers)
			if err != nil || p != fx.signers[0].Principal {
				t.Errorf("%s: %q %v", name, p, err)
			}
		}
		if p, err := v.Verify(ctx, fx.commit(t, "bob"), fx.signers); err != nil || p != fx.signers[1].Principal {
			t.Errorf("bob: %q %v", p, err)
		}
		u := fx.commit(t, "unsigned")
		_, err := v.Verify(ctx, u, fx.signers)
		refusalContaining(t, err, u.ID+" is not signed; the box applies only commits signed by a key in its signer list")
		m := fx.commit(t, "mallory")
		_, err = v.Verify(ctx, m, fx.signers)
		refusalContaining(t, err, m.ID+" is signed by a key that is not a signer in the Caddyfile this box runs")
		// Listed, mallory verifies — the list is the authority.
		if p, err := v.Verify(ctx, m, append(Signers{fx.mallory}, fx.signers...)); err != nil || p != fx.mallory.Principal {
			t.Errorf("mallory listed: %q %v", p, err)
		}
		// Alice's key under another principal: the principal is the
		// list's, not the key's.
		renamed := Signers{{Principal: "ops", Type: fx.signers[0].Type, Key: fx.signers[0].Key, B64: fx.signers[0].B64}}
		if p, err := v.Verify(ctx, fx.commit(t, "head"), renamed); err != nil || p != "ops" {
			t.Errorf("renamed: %q %v", p, err)
		}
		// A listed key over an altered payload: the signature is
		// alice's and still does not verify.
		h := fx.commit(t, "head")
		altered := *h
		altered.Payload = append([]byte{}, h.Payload...)
		altered.Payload[len(altered.Payload)-1] ^= 1
		_, err = v.Verify(ctx, &altered, fx.signers)
		refusalContaining(t, err, h.ID+" is signed by "+fx.signers[0].Principal+", but the signature does not verify over the commit")
		// The same key listed twice is the list's fault, not a verdict.
		_, err = v.Verify(ctx, h, append(fx.signers, fx.signers[0]))
		var r *Refusal
		if err == nil || errors.As(err, &r) {
			t.Errorf("duplicate key: %v", err)
		}
	})

	t.Run("verify chain", func(t *testing.T) {
		chain, err := Chain(fx.commit(t, "head"), fx.parentsFor(t, "head", baseline), baseline)
		if err != nil {
			t.Fatal(err)
		}
		if p, err := VerifyChain(ctx, v, chain, fx.signers, fx.signers, baseline); err != nil || p != fx.signers[0].Principal {
			t.Fatalf("head chain: %q %v", p, err)
		}
		// An empty chain is a caller's bug, refused as the box's error.
		if _, err := VerifyChain(ctx, v, nil, fx.signers, fx.signers, baseline); err == nil || errors.As(err, new(*Refusal)) {
			t.Fatalf("empty chain: %v", err)
		}
		// after_unsigned → unsigned → head → mid → base: the unsigned
		// commit in the middle refuses the push, naming it, with HEAD
		// and the baseline the message is about.
		au := fx.commit(t, "after_unsigned")
		chain, err = Chain(au, fx.parentsFor(t, "after_unsigned", baseline), baseline)
		if err != nil || len(chain) != 4 {
			t.Fatalf("chain: %v %v", chain, err)
		}
		_, err = VerifyChain(ctx, v, chain, fx.signers, fx.signers, baseline)
		u := fx.commit(t, "unsigned")
		refusalContaining(t, err, u.ID+", between the commit this box runs and "+au.ID+", is not signed; every commit on main must be — rebase it out and force-push; the box still runs "+baseline)
		// HEAD itself unsigned: its own message, not the between one.
		chain, err = Chain(u, fx.parentsFor(t, "unsigned", baseline), baseline)
		if err != nil {
			t.Fatal(err)
		}
		_, err = VerifyChain(ctx, v, chain, fx.signers, fx.signers, baseline)
		refusalContaining(t, err, u.ID+" is not signed;")
		// A between-commit signed by a key the box did not list when it
		// last applied: named by the principal the incoming file gives
		// it, or treated as unsigned when the incoming file lists it no
		// more than the installed one. (VerifyChain reads no linkage,
		// so the chain is assembled by hand: head over bob's commit.)
		alice, bob := fx.signers[:1], fx.signers
		b := fx.commit(t, "bob")
		_, err = VerifyChain(ctx, v, []*Commit{fx.commit(t, "head"), b}, alice, bob, baseline)
		refusalContaining(t, err, b.ID+", between the commit this box runs and "+fx.commit(t, "head").ID+", is signed by a key this box did not list when it last applied ("+bob[1].Principal+"); the commit that adds the key must apply first — force main back to it, let it apply, then push the rest")
		_, err = VerifyChain(ctx, v, []*Commit{fx.commit(t, "head"), b}, alice, alice, baseline)
		refusalContaining(t, err, b.ID+", between the commit this box runs and ", ", is signed by a key this box did not list when it last applied (not in the new Caddyfile either);")
		// The same key at HEAD is HEAD's own refusal.
		_, err = VerifyChain(ctx, v, []*Commit{b}, alice, bob, baseline)
		refusalContaining(t, err, b.ID+" is signed by a key that is not a signer in the Caddyfile this box runs")
		// An altered intermediate signature keeps its own message: it
		// is neither unsigned nor unlisted.
		alteredMid := *fx.commit(t, "mid")
		alteredMid.Payload = append([]byte{}, alteredMid.Payload...)
		alteredMid.Payload[len(alteredMid.Payload)-1] ^= 1
		_, err = VerifyChain(ctx, v, []*Commit{fx.commit(t, "head"), &alteredMid}, fx.signers, fx.signers, baseline)
		refusalContaining(t, err, alteredMid.ID+" is signed by "+fx.signers[0].Principal+", but the signature does not verify")
		// ssh-keygen that cannot run is the box's error, not a verdict
		// (on a chain whose head is signed, so the verdict needs it).
		broken := &Verifier{SSHKeygen: "/nonexistent/ssh-keygen", TempDir: t.TempDir()}
		signed, err := Chain(fx.commit(t, "head"), fx.parentsFor(t, "head", baseline), baseline)
		if err != nil {
			t.Fatal(err)
		}
		_, err = VerifyChain(ctx, broken, signed, fx.signers, fx.signers, baseline)
		var r *Refusal
		if err == nil || errors.As(err, &r) {
			t.Fatalf("want an error that is not a refusal, got %v", err)
		}
	})

	t.Run("bundle round trip", func(t *testing.T) {
		b, err := ReadBundle(fx.bundleFor(t, "head", baseline))
		if err != nil {
			t.Fatal(err)
		}
		head := fx.commit(t, "head")
		if b.Path != path || b.Commit.ID != head.ID || !bytes.Equal(b.Caddyfile, fx.file(t, "head")) {
			t.Fatalf("bundle: %s", b)
		}
		if len(b.Parents) != 1 || b.Parents[0].ID != fx.m.Cases["mid"].Commit {
			t.Fatalf("parents: %v", b.Parents)
		}
		if len(b.Trees) != 2 || b.Trees[head.Tree] == nil {
			t.Fatalf("trees: %v", b.Trees)
		}
		// Then the whole proof, as the applier runs it.
		if err := ProveFile(b.Commit, b.Trees, b.Path, b.Caddyfile); err != nil {
			t.Fatal(err)
		}
		chain, err := Chain(b.Commit, b.Parents, baseline)
		if err != nil {
			t.Fatal(err)
		}
		if p, err := VerifyChain(ctx, v, chain, fx.signers, fx.signers, baseline); err != nil || p != fx.signers[0].Principal {
			t.Fatalf("%q %v", p, err)
		}
		// HEAD == baseline: a bundle with no parents.
		b, err = ReadBundle(fx.bundleFor(t, "base", baseline))
		if err != nil || len(b.Parents) != 0 {
			t.Fatalf("%v %v", b, err)
		}
		if chain, err := Chain(b.Commit, b.Parents, baseline); err != nil || len(chain) != 1 {
			t.Fatalf("%v %v", chain, err)
		}
	})

	t.Run("sha256 repository", func(t *testing.T) {
		raw, ok := fx.objects[fx.m.SHA256]
		if !ok {
			t.Fatal("no sha256 object")
		}
		_, err := ParseCommit(raw)
		refusalContaining(t, err, "is in a SHA-256 repository, which the box does not read")
	})
}

// reassemble folds the signature back into the payload as git's
// gpgsig header, after the committer line (where git writes it in these
// fixtures): the inverse of ParseCommit's split.
func reassemble(c *Commit) []byte {
	if c.Kind == Unsigned {
		return c.Payload
	}
	sep := bytes.Index(c.Payload, []byte("\n\n"))
	header, rest := c.Payload[:sep+1], c.Payload[sep+1:]
	var out bytes.Buffer
	out.Write(header)
	lines := bytes.SplitAfter(c.Signature, []byte("\n"))
	for i, l := range lines {
		if len(l) == 0 {
			continue
		}
		if i == 0 {
			out.WriteString("gpgsig ")
		} else {
			out.WriteByte(' ')
		}
		out.Write(l)
	}
	out.Write(rest)
	return out.Bytes()
}
