package proof

// Hand-made inputs: what git would never write, which the parsers must
// refuse by name, and the fixed ids that pin the hashing.

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"
)

const (
	emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	emptyBlob = "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391"
	zeroID    = "0000000000000000000000000000000000000000"
)

// commitObject is a commit as git lays one out, with the headers given
// after tree and parents, and the message.
func commitObject(tree string, parents []string, headers []string, msg string) []byte {
	var b strings.Builder
	b.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p + "\n")
	}
	b.WriteString("author Alice <alice@example.com> 1791000000 +0000\n")
	b.WriteString("committer Alice <alice@example.com> 1791000000 +0000\n")
	for _, h := range headers {
		b.WriteString(h + "\n")
	}
	b.WriteString("\n" + msg)
	return []byte(b.String())
}

// gpgsigHeader folds a signature into a gpgsig header.
func gpgsigHeader(sig string) string {
	lines := strings.Split(strings.TrimSuffix(sig, "\n"), "\n")
	return "gpgsig " + strings.Join(lines, "\n ")
}

const sshArmor = "-----BEGIN SSH SIGNATURE-----\nU1NIU0lHAAAAAQ==\n-----END SSH SIGNATURE-----\n"
const pgpArmor = "-----BEGIN PGP SIGNATURE-----\n\niQEzBAABCAAdFiEE\n=abcd\n-----END PGP SIGNATURE-----\n"

func TestObjectIDPins(t *testing.T) {
	if got := ObjectID("tree", nil); got != emptyTree {
		t.Fatalf("empty tree: %s", got)
	}
	if got := ObjectID("blob", nil); got != emptyBlob {
		t.Fatalf("empty blob: %s", got)
	}
	// `git hash-object` of "hello\n".
	if got := ObjectID("blob", []byte("hello\n")); got != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Fatalf("hello blob: %s", got)
	}
}

func TestParseCommit(t *testing.T) {
	unsigned := commitObject(emptyTree, []string{zeroID}, nil, "msg\n")
	c, err := ParseCommit(unsigned)
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != ObjectID("commit", unsigned) || c.Tree != emptyTree || len(c.Parents) != 1 || c.Parents[0] != zeroID || c.Kind != Unsigned || c.Signature != nil {
		t.Fatalf("%+v", c)
	}
	if !bytes.Equal(c.Payload, unsigned) {
		t.Fatal("an unsigned commit's payload is the object")
	}

	sshSigned := commitObject(emptyTree, nil, []string{gpgsigHeader(sshArmor)}, "msg\n")
	c, err = ParseCommit(sshSigned)
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != SSHSig || string(c.Signature) != sshArmor {
		t.Fatalf("%v %q", c.Kind, c.Signature)
	}
	if !bytes.Equal(c.Payload, commitObject(emptyTree, nil, nil, "msg\n")) {
		t.Fatalf("payload %q", c.Payload)
	}
	if !bytes.Equal(reassemble(c), sshSigned) {
		t.Fatal("reassemble")
	}
	// A header after the signature is refused: git appends gpgsig last,
	// and the same payload under another header order would be another
	// commit id for one signature (armor_test.go).
	if _, err := ParseCommit(commitObject(emptyTree, nil, []string{gpgsigHeader(sshArmor), "encoding UTF-8"}, "msg\n")); err == nil || !strings.Contains(err.Error(), "not the last header") {
		t.Fatalf("a header after gpgsig: %v", err)
	}
	// One before it stays in the payload, in place.
	c, err = ParseCommit(commitObject(emptyTree, nil, []string{"encoding UTF-8", gpgsigHeader(sshArmor)}, "msg\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(c.Payload, commitObject(emptyTree, nil, []string{"encoding UTF-8"}, "msg\n")) {
		t.Fatalf("payload %q", c.Payload)
	}
	// The PGP armor's blank line is a ` ` continuation, not the
	// separator; the kind is OpenPGP.
	c, err = ParseCommit(commitObject(emptyTree, nil, []string{gpgsigHeader(pgpArmor)}, "msg\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != OpenPGPSig || string(c.Signature) != pgpArmor {
		t.Fatalf("%v %q", c.Kind, c.Signature)
	}
	c, err = ParseCommit(commitObject(emptyTree, nil, []string{gpgsigHeader("-----BEGIN SIGNED MESSAGE-----\nMIIB\n-----END SIGNED MESSAGE-----\n")}, "msg\n"))
	if err != nil || c.Kind != OtherSig {
		t.Fatalf("%v %v", c, err)
	}
	// A mergetag header with continuation lines, kept whole.
	merge := commitObject(emptyTree, []string{zeroID, zeroID}, []string{"mergetag object " + zeroID + "\n type commit\n tag v1", gpgsigHeader(sshArmor)}, "Merge tag 'v1'\n")
	c, err = ParseCommit(merge)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Parents) != 2 || c.Kind != SSHSig || !bytes.Equal(reassemble(c), merge) {
		t.Fatalf("%+v", c)
	}
	// An empty message: the separator is the object's end.
	if _, err := ParseCommit(commitObject(emptyTree, nil, nil, "")); err != nil {
		t.Fatal(err)
	}

	for name, in := range map[string][]byte{
		"empty":                    {},
		"no separator":             []byte("tree " + emptyTree + "\nauthor a\n"),
		"first header not tree":    []byte("parent " + zeroID + "\ntree " + emptyTree + "\n\nm"),
		"continuation first":       []byte(" tree " + emptyTree + "\n\nm"),
		"second tree":              commitObject(emptyTree, nil, []string{"tree " + emptyTree}, "m"),
		"tree id short":            []byte("tree abc\n\nm"),
		"tree id upper":            []byte("tree " + strings.ToUpper(emptyTree) + "\n\nm"),
		"parent id short":          commitObject(emptyTree, []string{"abc"}, nil, "m"),
		"two gpgsig":               commitObject(emptyTree, nil, []string{gpgsigHeader(sshArmor), gpgsigHeader(sshArmor)}, "m"),
		"header name with control": []byte("tree " + emptyTree + "\n\x01 x\n\nm"),
		"header without a value":   []byte("tree " + emptyTree + "\nauthor\n\nm"),
		"empty header name":        []byte("tree " + emptyTree + "\n\x00\n\nm"),
		"too large":                append(commitObject(emptyTree, nil, nil, ""), bytes.Repeat([]byte("x"), MaxCommit)...),
	} {
		_, err := ParseCommit(in)
		refusalContaining(t, err, "bundle: commit")
		if name == "too large" && !strings.Contains(err.Error(), "64 KiB") {
			t.Fatal(err)
		}
	}
	// SHA-256 repositories, three ways.
	id64 := strings.Repeat("ab", 32)
	for _, in := range [][]byte{
		[]byte("tree " + id64 + "\n\nm"),
		commitObject(emptyTree, []string{id64}, nil, "m"),
		commitObject(emptyTree, nil, []string{"gpgsig-sha256 -----BEGIN SSH SIGNATURE-----\n x\n -----END SSH SIGNATURE-----"}, "m"),
	} {
		_, err := ParseCommit(in)
		refusalContaining(t, err, ObjectID("commit", in)+" is in a SHA-256 repository, which the box does not read")
	}
}

// treeObject is a tree as git lays one out.
func treeObject(entries ...Entry) []byte {
	var b bytes.Buffer
	for _, e := range entries {
		id, err := hex.DecodeString(e.ID)
		if err != nil {
			panic(err)
		}
		b.WriteString(e.Mode + " " + e.Name + "\x00")
		b.Write(id)
	}
	return b.Bytes()
}

func TestParseTree(t *testing.T) {
	tr, err := ParseTree(nil)
	if err != nil || tr.ID != emptyTree {
		t.Fatalf("%v %v", tr, err)
	}
	if es, err := tr.Entries(); err != nil || len(es) != 0 {
		t.Fatalf("%v %v", es, err)
	}
	// In git's order: `Caddyfile` < `dir/` < `link` < `run` < `sub`.
	raw := treeObject(Entry{ModeFile, "Caddyfile", emptyBlob}, Entry{ModeDir, "dir", emptyTree}, Entry{ModeSymlink, "link", emptyBlob}, Entry{ModeExecutable, "run", emptyBlob}, Entry{ModeSubmodule, "sub", zeroID})
	tr, err = ParseTree(raw)
	if err != nil {
		t.Fatal(err)
	}
	es, err := tr.Entries()
	if err != nil || len(es) != 5 || es[0] != (Entry{ModeFile, "Caddyfile", emptyBlob}) || es[4] != (Entry{ModeSubmodule, "sub", zeroID}) || tr.ID != ObjectID("tree", raw) {
		t.Fatalf("%+v %v", es, err)
	}
	if e, ok, err := tr.entry("sub"); err != nil || !ok || e.Mode != ModeSubmodule {
		t.Fatal(e, ok, err)
	}
	if _, ok, err := tr.entry("nope"); err != nil || ok {
		t.Fatal(ok, err)
	}
	// git's directory rule: a directory `a` is read as `a/`, so it
	// sorts after `a-b` ('-' < '/') and `a.b`, before `a0`.
	for _, ok := range [][]Entry{
		{{ModeFile, "a-b", emptyBlob}, {ModeDir, "a", emptyTree}},
		{{ModeFile, "a.b", emptyBlob}, {ModeDir, "a", emptyTree}, {ModeFile, "a0", emptyBlob}},
		{{ModeFile, "a", emptyBlob}, {ModeDir, "ab", emptyTree}},
	} {
		if _, err := ParseTree(treeObject(ok...)); err != nil {
			t.Errorf("%v: %v", ok, err)
		}
	}
	if _, err := ParseTree(treeObject(Entry{ModeDir, "a", emptyTree}, Entry{ModeFile, "a-b", emptyBlob})); err == nil {
		t.Error("a/ before a-b accepted")
	}
	// A name twice — whatever the modes, as git's fsck has it, and
	// however many `x-…` names lie between the file `x` and the
	// directory `x` — and entries out of order are refused at the parse.
	_, err = ParseTree(treeObject(Entry{ModeFile, "x", emptyBlob}, Entry{ModeFile, "x", emptyTree}))
	refusalContaining(t, err, "an entry name appears twice")
	_, err = ParseTree(treeObject(Entry{ModeFile, "x", emptyBlob}, Entry{ModeDir, "x", emptyTree}))
	refusalContaining(t, err, "an entry name appears twice")
	_, err = ParseTree(treeObject(Entry{ModeFile, "a", emptyBlob}, Entry{ModeFile, "a-b", emptyBlob}, Entry{ModeDir, "a", emptyTree}))
	refusalContaining(t, err, "an entry name appears twice")
	_, err = ParseTree(treeObject(Entry{ModeFile, "a", emptyBlob}, Entry{ModeFile, "a-b", emptyBlob}, Entry{ModeFile, "a-c", emptyBlob}, Entry{ModeFile, "a.d", emptyBlob}, Entry{ModeDir, "a", emptyTree}))
	refusalContaining(t, err, "an entry name appears twice")
	// Not duplicates: the candidate retires once an entry sorts past
	// where its directory twin would stand.
	for _, ok := range [][]Entry{
		{{ModeFile, "a", emptyBlob}, {ModeFile, "a-b", emptyBlob}, {ModeDir, "ab", emptyTree}},
		{{ModeFile, "a", emptyBlob}, {ModeFile, "b", emptyBlob}, {ModeDir, "c", emptyTree}},
		{{ModeFile, "a", emptyBlob}, {ModeDir, "a-b", emptyTree}, {ModeFile, "a0", emptyBlob}},
	} {
		if _, err := ParseTree(treeObject(ok...)); err != nil {
			t.Errorf("%v: %v", ok, err)
		}
	}
	_, err = ParseTree(treeObject(Entry{ModeFile, "b", emptyBlob}, Entry{ModeFile, "a", emptyBlob}))
	refusalContaining(t, err, "entries are not in git's order")
	// Nothing is allocated per entry: a 500-entry tree costs the same
	// fixed handful (the struct, the id, the hasher's prefix) as five.
	var big []Entry
	for i := 0; i < 500; i++ {
		big = append(big, Entry{ModeFile, fmt.Sprintf("e%03d", i), emptyBlob})
	}
	bigRaw := treeObject(big...)
	five := testing.AllocsPerRun(20, func() { _, _ = ParseTree(raw) })
	hundreds := testing.AllocsPerRun(20, func() { _, _ = ParseTree(bigRaw) })
	if hundreds > five {
		t.Errorf("ParseTree allocates %v times for 500 entries, %v for five", hundreds, five)
	}
	for name, in := range map[string][]byte{
		"no space":        []byte("100644"),
		"no nul":          []byte("100644 x"),
		"short id":        []byte("100644 x\x00abc"),
		"zero-padded":     treeObject(Entry{"040000", "d", emptyTree}),
		"unknown mode":    treeObject(Entry{"100664", "x", emptyBlob}),
		"empty name":      treeObject(Entry{ModeFile, "", emptyBlob}),
		"dot":             treeObject(Entry{ModeFile, ".", emptyBlob}),
		"dotdot":          treeObject(Entry{ModeDir, "..", emptyTree}),
		"slash":           treeObject(Entry{ModeFile, "a/b", emptyBlob}),
		"twice":           treeObject(Entry{ModeFile, "x", emptyBlob}, Entry{ModeDir, "x", emptyTree}, Entry{ModeDir, "x", emptyTree}),
		"unsorted":        treeObject(Entry{ModeFile, "b", emptyBlob}, Entry{ModeFile, "a", emptyBlob}),
		"trailing":        append(treeObject(Entry{ModeFile, "x", emptyBlob}), ' '),
		"larger than cap": bytes.Repeat([]byte("x"), MaxTree+1),
	} {
		if _, err := ParseTree(in); err == nil {
			t.Errorf("%s: parsed", name)
		} else {
			refusalContaining(t, err, "bundle: ")
		}
	}
}

func TestSplitPath(t *testing.T) {
	for _, ok := range []string{"Caddyfile", "box1/Caddyfile", "a/b/c", "prod/box 1/Caddyfile", "x\\y", strings.Repeat("a/", MaxDepth-1) + "a", strings.Repeat("a", MaxPath)} {
		if _, err := SplitPath(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "/", "/box1/Caddyfile", "box1/", "box1//Caddyfile", "./Caddyfile", "box1/./Caddyfile", "../Caddyfile", "box1/../box2/Caddyfile", "a\x00b", "a\nb", "a\x7fb", "box1/Caddyfile\n", strings.Repeat("a/", MaxDepth) + "a", strings.Repeat("a", MaxPath+1)} {
		_, err := SplitPath(bad)
		refusalContaining(t, err, "bundle: path is not a relative path of safe components")
	}
}

func TestProveFileHandMade(t *testing.T) {
	file := []byte("content\n")
	blob := ObjectID("blob", file)
	inner := treeObject(Entry{ModeFile, "Caddyfile", blob})
	innerID := ObjectID("tree", inner)
	root := treeObject(Entry{ModeDir, "box1", innerID})
	rootID := ObjectID("tree", root)
	trees := map[string]*Tree{}
	for _, raw := range [][]byte{inner, root} {
		tr, err := ParseTree(raw)
		if err != nil {
			t.Fatal(err)
		}
		trees[tr.ID] = tr
	}
	c, err := ParseCommit(commitObject(rootID, nil, nil, "m\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ProveFile(c, trees, "box1/Caddyfile", file); err != nil {
		t.Fatal(err)
	}
	refusalContaining(t, ProveFile(c, trees, "box1/Caddyfile", []byte("content")), "the file sent is not box1/Caddyfile in "+c.ID)
	refusalContaining(t, ProveFile(c, trees, "Caddyfile", file), "the file sent is not")
	refusalContaining(t, ProveFile(c, trees, "box1", file), "box1 in "+c.ID+" is not a regular file")
	refusalContaining(t, ProveFile(c, trees, "box1/Caddyfile/x", file), "the file sent is not")
	refusalContaining(t, ProveFile(c, trees, "../x", file), "bundle: path is not a relative path of safe components")
	// A path with a rune that is not printable (a soft hyphen) is
	// quoted; a control byte never gets this far (SplitPath).
	softHyphen := string(rune(0xad))
	refusalContaining(t, ProveFile(c, trees, "box1/x"+softHyphen+"y", file), "the file sent is not \"box1/x\\"+"u00ady\" in")
	long := strings.Repeat("a", 400)
	err = ProveFile(c, trees, long, file)
	refusalContaining(t, err, strings.Repeat("a", maxBound)+"...")
	if strings.Contains(err.Error(), long) {
		t.Fatal("not bounded")
	}
	// Deep enough to be legal, with every tree present.
	deepTrees := map[string]*Tree{}
	cur := blob
	mode := ModeFile
	var comps []string
	for i := 0; i < MaxDepth; i++ {
		name := "d" + strconv.Itoa(i)
		raw := treeObject(Entry{mode, name, cur})
		tr, err := ParseTree(raw)
		if err != nil {
			t.Fatal(err)
		}
		deepTrees[tr.ID] = tr
		comps = append([]string{name}, comps...)
		cur, mode = tr.ID, ModeDir
	}
	deep, err := ParseCommit(commitObject(cur, nil, nil, "m\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ProveFile(deep, deepTrees, strings.Join(comps, "/"), file); err != nil {
		t.Fatal(err)
	}
}

func TestChainHandMade(t *testing.T) {
	// A straight line of unsigned commits, c0 ← c1 ← … ← cN.
	line := func(n int) []*Commit {
		var cs []*Commit
		parent := ""
		for i := 0; i <= n; i++ {
			var parents []string
			if parent != "" {
				parents = []string{parent}
			}
			c, err := ParseCommit(commitObject(emptyTree, parents, nil, "c"+strconv.Itoa(i)+"\n"))
			if err != nil {
				t.Fatal(err)
			}
			cs = append(cs, c)
			parent = c.ID
		}
		return cs
	}
	// below is the parents the workflow bundles for head cs[i] down to
	// (not including) cs[j]: cs[i-1] … cs[j+1].
	below := func(cs []*Commit, i, j int) []*Commit {
		var out []*Commit
		for k := i - 1; k > j; k-- {
			out = append(out, cs[k])
		}
		return out
	}
	cs := line(3)
	chain, err := Chain(cs[3], below(cs, 3, 0), cs[0].ID)
	if err != nil || len(chain) != 3 || chain[0] != cs[3] || chain[2] != cs[1] {
		t.Fatalf("%v %v", chain, err)
	}
	if chain, err := Chain(cs[3], nil, cs[3].ID); err != nil || len(chain) != 1 || chain[0] != cs[3] {
		t.Fatalf("%v %v", chain, err)
	}
	if chain, err := Chain(cs[3], nil, cs[2].ID); err != nil || len(chain) != 1 {
		t.Fatalf("%v %v", chain, err)
	}
	// Root reached: a baseline that is not in the history.
	_, err = Chain(cs[3], below(cs, 3, -1), zeroID)
	refusalContaining(t, err, cs[3].ID+" does not descend from the commit this box runs ("+zeroID+") along main's first-parent line", "moteld box baseline "+cs[3].ID)
	// Too few parents bundled: the walk ends before the baseline.
	_, err = Chain(cs[3], below(cs, 3, 1), cs[0].ID)
	refusalContaining(t, err, "does not descend")
	// Position and hash must agree: an entry that is not the previous
	// commit's first parent, and an entry past the baseline.
	_, err = Chain(cs[3], []*Commit{cs[1]}, cs[0].ID)
	refusalContaining(t, err, "bundle: parents/0001 is not the first parent of "+cs[3].ID)
	_, err = Chain(cs[3], []*Commit{cs[2], cs[0]}, cs[0].ID)
	refusalContaining(t, err, "bundle: parents/0002 is not the first parent of "+cs[2].ID)
	_, err = Chain(cs[3], []*Commit{cs[2], cs[1], cs[0]}, cs[1].ID)
	refusalContaining(t, err, "bundle: parents/0002 is past the end of the chain")
	_, err = Chain(cs[3], []*Commit{cs[2]}, cs[3].ID)
	refusalContaining(t, err, "bundle: parents/0001 is past the end of the chain")
	// A root reached with a parent still bundled is not "past the end"
	// (no baseline was met): it is a history off the box's.
	_, err = Chain(cs[1], []*Commit{cs[0], cs[0]}, zeroID)
	refusalContaining(t, err, "does not descend")
	// A merge: the second parent is not walked.
	m, err := ParseCommit(commitObject(emptyTree, []string{cs[1].ID, cs[3].ID}, nil, "merge\n"))
	if err != nil {
		t.Fatal(err)
	}
	if chain, err := Chain(m, []*Commit{cs[1]}, cs[0].ID); err != nil || len(chain) != 2 {
		t.Fatalf("%v %v", chain, err)
	}
	_, err = Chain(m, []*Commit{cs[3]}, cs[2].ID)
	refusalContaining(t, err, "bundle: parents/0001 is not the first parent of")
	_, err = Chain(m, []*Commit{cs[1], cs[0]}, cs[2].ID)
	refusalContaining(t, err, "does not descend", "merged main into itself")
	// The cap: MaxChain commits above the baseline pass, one more does
	// not. With head cs[N], the baseline cs[N-MaxChain] leaves exactly
	// MaxChain above it.
	cs = line(MaxChain + 2)
	head := cs[len(cs)-1]
	_, err = Chain(head, below(cs, len(cs)-1, 0), cs[0].ID)
	refusalContaining(t, err, "the chain from "+cs[0].ID+" to "+head.ID+" is longer than 500 commits; run moteld box baseline "+head.ID+" as root on the box")
	_, err = Chain(head, below(cs, len(cs)-1, 1), cs[1].ID)
	refusalContaining(t, err, "is longer than 500 commits")
	if chain, err := Chain(head, below(cs, len(cs)-1, 2), cs[2].ID); err != nil || len(chain) != MaxChain {
		t.Fatalf("%d %v", len(chain), err)
	}
	// The bundle's own bound: 499 parents fill the chain exactly; a
	// history that goes on past them is the cap, not a missing
	// baseline — and a root reached at exactly the cap is descent.
	_, err = Chain(head, below(cs, len(cs)-1, 2), cs[0].ID)
	refusalContaining(t, err, "is longer than 500 commits")
	short := line(MaxChain - 1) // cs[0] is a root; head cs[499] + 499 parents reach it
	_, err = Chain(short[len(short)-1], below(short, len(short)-1, -1), zeroID)
	refusalContaining(t, err, "does not descend")
}

// pubLine is a public key as a signer line's two arguments.
func pubLine(t *testing.T, key any) (string, string) {
	t.Helper()
	pub, err := ssh.NewPublicKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pub.Type(), base64.StdEncoding.EncodeToString(pub.Marshal())
}

func TestParseSigner(t *testing.T) {
	edPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	edType, edB64 := pubLine(t, edPub)
	ecType, ecB64 := pubLine(t, &ecKey.PublicKey)
	rsaType, rsaB64 := pubLine(t, &rsaKey.PublicKey)
	if edType != "ssh-ed25519" || ecType != "ecdsa-sha2-nistp384" || rsaType != "ssh-rsa" {
		t.Fatal(edType, ecType, rsaType)
	}
	// An sk- key: the wire form is the ed25519 one plus an application string.
	var sk bytes.Buffer
	for _, s := range [][]byte{[]byte("sk-ssh-ed25519@openssh.com"), edPub, []byte("ssh:")} {
		sk.Write([]byte{byte(len(s) >> 24), byte(len(s) >> 16), byte(len(s) >> 8), byte(len(s))})
		sk.Write(s)
	}
	skB64 := base64.StdEncoding.EncodeToString(sk.Bytes())
	// A 512-bit RSA key, which OpenSSH will not read from allowed_signers.
	small, err := ssh.NewPublicKey(&rsa.PublicKey{N: new(big.Int).SetBit(new(big.Int).SetInt64(1), 511, 1), E: 65537})
	if err != nil {
		t.Fatal(err)
	}
	smallRSA := base64.StdEncoding.EncodeToString(small.Marshal())
	if _, err := ParseSigner("a", "ssh-rsa", smallRSA); err == nil || !strings.Contains(err.Error(), "an RSA key of 512 bits") {
		t.Fatal(err)
	}

	// An ECDSA key is refused by name, with the reason and the remedy:
	// its signature has two forms, so one signed commit has two ids.
	if _, err := ParseSigner("bob", ecType, ecB64); err == nil || !strings.Contains(err.Error(), "an ecdsa-sha2-nistp384 key, which the box no longer takes: an ECDSA signature has two forms") || !strings.Contains(err.Error(), "ssh-keygen -t ed25519") {
		t.Fatalf("an ECDSA signer: %v", err)
	}
	// An Ed25519 key of small order is refused by name in either form:
	// anyone can sign for it (#536). The identity point, encoded as 1
	// and 31 zero bytes, is one of libsodium's blocklist.
	identity := "\x01" + strings.Repeat("\x00", 31)
	for typ, wire := range map[string][]byte{
		"ssh-ed25519":                ssh.Marshal(struct{ Type, Key string }{"ssh-ed25519", identity}),
		"sk-ssh-ed25519@openssh.com": ssh.Marshal(struct{ Type, Key, Application string }{"sk-ssh-ed25519@openssh.com", identity, "ssh:"}),
	} {
		_, err := ParseSigner("mallory", typ, base64.StdEncoding.EncodeToString(wire))
		if want := "signer mallory: the key is not a usable ed25519 key: its point has small order, so anyone can sign for it"; err == nil || err.Error() != want {
			t.Errorf("a signer of small order, %s: got %v, want %q", typ, err, want)
		}
	}
	for _, ok := range [][3]string{
		{"alice@example.com", edType, edB64},
		{"bob", rsaType, rsaB64},
		{"c.d_e+f-g", rsaType, rsaB64},
		{"alice-yubikey", "sk-ssh-ed25519@openssh.com", skB64},
	} {
		s, err := ParseSigner(ok[0], ok[1], ok[2])
		if err != nil {
			t.Errorf("%v: %v", ok, err)
			continue
		}
		if s.Principal != ok[0] || s.Type != ok[1] || s.B64 != ok[2] || len(s.Key) == 0 {
			t.Errorf("%+v", s)
		}
	}
	for name, bad := range map[string][3]string{
		"principal with a star":  {"*", edType, edB64},
		"principal with a comma": {"a,b", edType, edB64},
		"principal with a space": {"a b", edType, edB64},
		"principal with a quote": {`a"b`, edType, edB64},
		"principal with a bang":  {"!a", edType, edB64},
		"empty principal":        {"", edType, edB64},
		"unknown type":           {"a", "ssh-dss", edB64},
		"sk-ecdsa type":          {"a", "sk-ecdsa-sha2-nistp256@openssh.com", skB64},
		"type mismatch":          {"a", "ssh-rsa", edB64},
		"sk type mismatch":       {"a", "ssh-ed25519", skB64},
		"not base64":             {"a", edType, "AAAA!"},
		"base64 of junk":         {"a", edType, base64.StdEncoding.EncodeToString([]byte("junk"))},
		"principal too long":     {strings.Repeat("a", MaxPrincipal+1), edType, edB64},
		"newline inside the key": {"a", edType, edB64[:20] + "\n" + edB64[20:]},
		"crlf inside the key":    {"a", edType, edB64[:20] + "\r\n" + edB64[20:]},
		"trailing newline":       {"a", edType, edB64 + "\n"},
		"unpadded key":           {"a", "sk-ssh-ed25519@openssh.com", strings.TrimRight(skB64, "=")}, // 74 bytes: padded when canonical
		"rsa under the floor":    {"a", "ssh-rsa", smallRSA},
		"empty key":              {"a", edType, ""},
	} {
		if _, err := ParseSigner(bad[0], bad[1], bad[2]); err == nil {
			t.Errorf("%s: parsed", name)
		} else if !strings.HasPrefix(err.Error(), "signer ") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The file, and the same key twice.
	a, _ := ParseSigner("alice", edType, edB64)
	b, _ := ParseSigner("bob", rsaType, rsaB64)
	out, err := Signers{a, b}.AllowedSigners()
	if err != nil {
		t.Fatal(err)
	}
	want := "alice namespaces=\"git\" " + edType + " " + edB64 + "\nbob namespaces=\"git\" " + rsaType + " " + rsaB64 + "\n"
	if string(out) != want {
		t.Fatalf("%q", out)
	}
	a2, _ := ParseSigner("alice-laptop", edType, edB64)
	if _, err := (Signers{a, b, a2}).AllowedSigners(); err == nil || !strings.Contains(err.Error(), "signer alice and signer alice-laptop are the same key") {
		t.Fatal(err)
	}
	if !(Signers{a, b}).Has("bob") || (Signers{a, b}).Has("carol") {
		t.Fatal("Has")
	}
	if p, ok := (Signers{a, b}).PrincipalFor(b.Key); !ok || p != "bob" {
		t.Fatal(p, ok)
	}
	if _, ok := (Signers{a}).PrincipalFor(b.Key); ok {
		t.Fatal("PrincipalFor")
	}
	// A principal at the bound passes; the list check is linear, so a
	// list as long as a 1 MiB Caddyfile could hold is quick.
	if _, err := ParseSigner(strings.Repeat("a", MaxPrincipal), edType, edB64); err != nil {
		t.Fatal(err)
	}
	many := make(Signers, 0, 10_000)
	for i := range cap(many) {
		many = append(many, Signer{Principal: "p" + strconv.Itoa(i), Type: edType, Key: []byte(strconv.Itoa(i)), B64: edB64})
	}
	start := time.Now()
	if _, err := many.AllowedSigners(); err != nil {
		t.Fatal(err)
	}
	many = append(many, Signer{Principal: "again", Type: edType, Key: []byte("7")})
	if _, err := many.AllowedSigners(); err == nil || !strings.Contains(err.Error(), "signer p7 and signer again are the same key") {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("duplicate detection took %s", time.Since(start))
	}
}

// A refusal names the ids its call site marked, and nothing that only
// looks like one: a principal may be 40 lowercase hex (ParseSigner
// allows it) and stays filtered.
func TestRefusalNamesMarkedIDsOnly(t *testing.T) {
	ids := func(err error) []string {
		var r *Refusal
		if !errors.As(err, &r) {
			t.Fatalf("%v is not a Refusal", err)
		}
		return r.IDs
	}
	principal := strings.Repeat("ab", 20)
	got := ids(refuseCode(codeAltered, "%s is signed by %s, but the signature does not verify over the commit", oid(emptyBlob), principal))
	if !slices.Equal(got, []string{emptyBlob}) {
		t.Fatalf("IDs = %v, want only the commit", got)
	}
	got = ids(refuse("%s and %s, and %s again", oid(emptyTree), oid(emptyBlob), oid(emptyTree)))
	if !slices.Equal(got, []string{emptyTree, emptyBlob}) {
		t.Fatalf("IDs = %v, want each id once, in order", got)
	}
}

func TestBound(t *testing.T) {
	if got := Bound("box1/Caddyfile"); got != "box1/Caddyfile" {
		t.Fatal(got)
	}
	if got := Bound("a\nb"); got != `"a\nb"` {
		t.Fatal(got)
	}
	// (The expected text is assembled so that no tool rewrites its
	// escapes into the runes they name.)
	if got, want := Bound("é\x00"), "\"\\"+"u00e9\\"+"x00\""; got != want {
		t.Fatalf("%q != %q", got, want)
	}
	// A cut never leaves a partial rune: after the leading `a`, every
	// é straddles an even offset, so the cut at 300 backs off to 299.
	got := Bound("a" + strings.Repeat("é", 200))
	if !strings.HasSuffix(got, "...") || len(got) != 299+3 || !utf8.ValidString(got) {
		t.Fatalf("%d %q", len(got), got)
	}
	// Exactly at the bound, nothing is cut.
	if got := Bound(strings.Repeat("é", 150)); len(got) != 300 || strings.HasSuffix(got, "...") {
		t.Fatalf("%d %q", len(got), got)
	}
}

func TestBoundTail(t *testing.T) {
	if got := BoundTail("box1/Caddyfile"); got != "box1/Caddyfile" {
		t.Fatal(got)
	}
	// Quoted first, as Bound: the cut is of the quoted text.
	if got := BoundTail("a\nb"); got != `"a\nb"` {
		t.Fatal(got)
	}
	got := BoundTail(strings.Repeat("x", 1000) + "the reason")
	if got != "..."+strings.Repeat("x", 300-len("the reason"))+"the reason" {
		t.Fatalf("%d %q", len(got), got)
	}
	got = BoundTail(strings.Repeat("y", 1000) + "\n")
	if !strings.HasPrefix(got, "...") || !strings.HasSuffix(got, `y\n"`) || len(got) != 3+300 {
		t.Fatalf("%d %q", len(got), got)
	}
	// A cut never leaves a partial rune: before the trailing `a`, every
	// é straddles an odd offset from the end, so the cut keeps 299.
	got = BoundTail(strings.Repeat("é", 200) + "a")
	if !strings.HasPrefix(got, "...") || len(got) != 3+299 || !utf8.ValidString(got) {
		t.Fatalf("%d %q", len(got), got)
	}
	if got := BoundTail(strings.Repeat("é", 150)); len(got) != 300 || strings.HasPrefix(got, "...") {
		t.Fatalf("%d %q", len(got), got)
	}
}
