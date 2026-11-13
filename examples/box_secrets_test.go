package examples

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/smallhoursorg/motel/liveswap"
)

// The box template's secrets commands — bin/secrets behind make secret,
// secrets, secret-show, rekey and secrets-import, and bin/signer's
// reader line — in a repository made from examples/box, with the age
// CLI built here and every key made at test time.

var (
	ageOnce sync.Once
	ageDir  string
	ageErr  error
)

// ageTools builds age and age-keygen once, at the version of
// filippo.io/age this repository pins (go.mod): the version the box
// decrypts with is the version the template's commands are tested with,
// and a bump moves both. It is installed outside this module's graph
// (GOWORK=off), with age's own go.sum: the CLI needs modules nothing
// here imports. Debian 13 ships age 1.2.1, which reads and writes the
// same files (the e2e lane runs the commands with it).
func ageTools(t *testing.T) string {
	t.Helper()
	ageOnce.Do(func() {
		dir, err := os.MkdirTemp("", "age-bin")
		if err != nil {
			ageErr = err
			return
		}
		ageDir = dir // TestMain removes it, built or not
		v, err := exec.Command("go", "list", "-m", "-f", "{{.Version}}", "filippo.io/age").Output()
		if err != nil {
			ageErr = fmt.Errorf("the pinned filippo.io/age: %w", err)
			return
		}
		version := strings.TrimSpace(string(v))
		cmd := exec.Command("go", "install",
			"filippo.io/age/cmd/age@"+version, "filippo.io/age/cmd/age-keygen@"+version)
		cmd.Env = append(os.Environ(), "GOBIN="+dir, "GOWORK=off", "GOFLAGS=")
		if out, err := cmd.CombinedOutput(); err != nil {
			ageErr = fmt.Errorf("go install age %s: %w\n%s", version, err, out)
		}
	})
	if ageErr != nil {
		t.Fatal(ageErr)
	}
	return ageDir
}

// secretsRepo is a box repository with the age CLI on its PATH.
func secretsRepo(t *testing.T) *boxRepo {
	t.Helper()
	dir := ageTools(t)
	r := newBoxRepo(t)
	r.env = append(r.env, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return r
}

// secrets runs bin/secrets <cmd> with stdin and env, under the shell
// given (the script's own #!/bin/sh when shell is "").
func (r *boxRepo) secrets(shell, stdin, cmd string, env ...string) (string, int) {
	r.t.Helper()
	script := filepath.Join(r.dir, "bin", "secrets")
	c := exec.Command(script, cmd)
	if shell != "" {
		c = exec.Command(shell, script, cmd)
	}
	c.Dir, c.Env = r.dir, append(append([]string{}, r.env...), env...)
	c.Stdin = strings.NewReader(stdin)
	out, err := c.CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return string(out), exit.ExitCode()
	}
	if err != nil {
		r.t.Fatalf("bin/secrets %s: %v\n%s", cmd, err, out)
	}
	return string(out), 0
}

// boxKey makes an age identity, as motel box key would on a box, and
// returns its file and its recipient.
func (r *boxRepo) boxKey(name string) (string, string) {
	r.t.Helper()
	id := filepath.Join(r.home, name+".age-identity")
	keygen := filepath.Join(ageTools(r.t), "age-keygen")
	if out, err := exec.Command(keygen, "-o", id).CombinedOutput(); err != nil {
		r.t.Fatalf("age-keygen: %v\n%s", err, out)
	}
	out, err := exec.Command(keygen, "-y", id).Output()
	if err != nil {
		r.t.Fatalf("age-keygen -y: %v", err)
	}
	return id, strings.TrimSpace(string(out))
}

// decrypt reads an encrypted value with an identity file, or reports
// that it could not.
func (r *boxRepo) decrypt(identity, rel string) (string, error) {
	r.t.Helper()
	c := exec.Command(filepath.Join(ageTools(r.t), "age"), "-d", "-i", identity, filepath.Join(r.dir, rel))
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, stderr.String())
	}
	return string(out), nil
}

func (r *boxRepo) mustDecrypt(identity, rel, want string) {
	r.t.Helper()
	got, err := r.decrypt(identity, rel)
	if err != nil {
		r.t.Fatalf("%s with %s: %v", rel, filepath.Base(identity), err)
	}
	if got != want {
		r.t.Fatalf("%s with %s = %q, want %q", rel, filepath.Base(identity), got, want)
	}
}

func (r *boxRepo) rsaKey(name string, bits int) string {
	r.t.Helper()
	priv := filepath.Join(r.home, ".ssh", name)
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "rsa", "-b", fmt.Sprint(bits), "-N", "", "-C", name, "-f", priv).CombinedOutput(); err != nil {
		r.t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	return priv + ".pub"
}

func (r *boxRepo) exists(rel string) bool {
	_, err := os.Stat(filepath.Join(r.dir, rel))
	return err == nil
}

// shortKey is how bin/secrets names a box's key in a message.
func shortKey(k string) string { return k[:8] + "…" + k[len(k)-4:] }

const (
	recipientsFile = "deploy.example.com/secrets/recipients"
	dbFile         = "deploy.example.com/secrets/example/DATABASE_URL.age"
)

// signers: alice (ed25519) and carol (RSA 2048) are readers; bob (a
// hardware sk- key) and dave (RSA 1024) cannot be, and are told so.
func readersRepo(t *testing.T) (r *boxRepo, alice, carol string) {
	t.Helper()
	r = secretsRepo(t)
	carolPub := r.rsaKey("carol", 2048)
	davePub := r.rsaKey("dave", 1024)
	for _, s := range [][]string{{carolPub, "carol"}, {davePub, "dave"}} {
		if out, code := r.run("signer", "KEY="+s[0], "PRINCIPAL="+s[1]); code != 0 {
			t.Fatalf("make signer %s: exit %d\n%s", s[1], code, out)
		}
	}
	// No hardware here to make an sk- key with: its line by hand. A key
	// of a type age cannot take is never read, only named.
	f := r.read(boxFile)
	r.write(boxFile, strings.Replace(f, "\t\t# signers\n",
		"\t\t# signers\n\t\tsigner bob@example.com sk-ssh-ed25519@openssh.com AAAAGnNrLXNzaC1lZDI1NTE5QG9wZW5zc2guY29tAAAAIA==\n", 1))
	alicePub := r.key("id_ed25519")
	if out, code := r.run("signer"); code != 0 { // last, so the checkout signs, and reads, as alice
		t.Fatalf("make signer: exit %d\n%s", code, out)
	}
	return r, strings.TrimSuffix(alicePub, ".pub"), strings.TrimSuffix(carolPub, ".pub")
}

func TestBoxSecretSet(t *testing.T) {
	t.Parallel()
	r, alice, carol := readersRepo(t)
	app := []string{"APP=example", "VAR=DATABASE_URL"}

	// The box's first secret needs the box's own key.
	if out, code := r.secrets("", "v\n", "set", app...); code != 1 ||
		!strings.Contains(out, "this first one needs the box's own key: BOX_KEY=$(ssh root@<your box> motel box key)") {
		t.Fatalf("no BOX_KEY: exit %d\n%s", code, out)
	}
	for _, bad := range []string{"age1nope", "ssh-ed25519 AAAA", "age1pq1" + strings.Repeat("q", 55), "AGE-SECRET-KEY-1" + strings.Repeat("Q", 58)} {
		if out, code := r.secrets("", "v\n", "set", append(app, "BOX_KEY="+bad)...); code != 1 || !strings.Contains(out, "BOX_KEY is not a box's key") {
			t.Fatalf("BOX_KEY=%s: exit %d\n%s", bad, code, out)
		}
	}
	if r.exists(recipientsFile) {
		t.Fatal("a refused first secret made the recipients file")
	}

	boxID, box := r.boxKey("box")
	// A first value refused leaves no recipients file behind either.
	if out, code := r.secrets("", "", "set", append(app, "BOX_KEY="+box)...); code != 1 || !strings.Contains(out, "DATABASE_URL is empty") || r.exists(recipientsFile) {
		t.Fatalf("a refused first value: exit %d\n%s", code, out)
	}
	out, code := r.secrets("", "postgres://u:p@db/app\n", "set", append(app, "BOX_KEY="+box+"\r")...) // ssh -t adds a CR
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, want := range []string{
		"secret: made " + recipientsFile + ": this box (" + shortKey(box) + "), alice@example.com, carol\n",
		"secret: bob@example.com signs with a sk-ssh-ed25519@openssh.com key, which age cannot encrypt to; to read secrets they add an age key of their own",
		"secret: dave signs with an RSA key under 2048 bits, which age does not take;",
		"secret: " + dbFile + ", encrypted to 3 keys, added to the commit\n",
		"secret: example's block does not read its secrets yet; add this line to it:\nsecret:     env_file /etc/motel/secrets/example.env\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	for _, id := range []string{boxID, alice, carol} {
		r.mustDecrypt(id, dbFile, "postgres://u:p@db/app")
	}
	rec := r.read(recipientsFile)
	if first := firstKey(rec); first != box {
		t.Errorf("the recipients file's first key is %q, want the box's %q:\n%s", first, box, rec)
	}
	if !strings.Contains(rec, "# alice@example.com\nssh-ed25519 "+r.b64(alice+".pub")+"\n") || strings.Contains(rec, "dave") || strings.Contains(rec, "bob@") {
		t.Errorf("the recipients file:\n%s", rec)
	}
	if got := r.git("diff", "--cached", "--name-only"); !strings.Contains(got, dbFile) || !strings.Contains(got, recipientsFile) {
		t.Errorf("staged: %q", got)
	}

	// Again: the value replaced, the recipients file kept.
	if out, code := r.secrets("", "postgres://u:p@db/app2", "set", app...); code != 0 || !strings.Contains(out, dbFile+", encrypted to 3 keys: its new value added to the commit") {
		t.Fatalf("a second value: exit %d\n%s", code, out)
	}
	r.mustDecrypt(boxID, dbFile, "postgres://u:p@db/app2")
	if r.read(recipientsFile) != rec {
		t.Error("a second value changed the recipients file")
	}
	// A BOX_KEY that is not the recipients file's box is a rekey, not a set.
	_, other := r.boxKey("other")
	if out, code := r.secrets("", "x", "set", append(app, "BOX_KEY="+other)...); code != 1 || !strings.Contains(out, "a rebuilt box's secrets move to its new key with make rekey BOX_KEY="+other) {
		t.Fatalf("another BOX_KEY: exit %d\n%s", code, out)
	}

	// What the value may be: what an env file can carry, one line of
	// UTF-8. One trailing newline is dropped, nothing else is changed.
	for _, c := range []struct{ in, want string }{
		{"x\r\n", "x"},
		{"  \"quoted\" and 'single'  ", "  \"quoted\" and 'single'  "},
		{"#=a=b # c", "#=a=b # c"},
		{"caf" + string(rune(0xe9)) + " " + string(rune(0x1f600)) + string(rune(0x3000)), "caf" + string(rune(0xe9)) + " " + string(rune(0x1f600)) + string(rune(0x3000))},
		// The longest whose own line, EDGE="…" and its newline, fits
		// the box's 64 KiB env file.
		{strings.Repeat("v", 65536-len("EDGE")-4), strings.Repeat("v", 65536-len("EDGE")-4)},
	} {
		if out, code := r.secrets("", c.in, "set", "APP=example", "VAR=EDGE"); code != 0 {
			t.Fatalf("value %q: exit %d\n%s", c.in, code, out)
		}
		r.mustDecrypt(boxID, "deploy.example.com/secrets/example/EDGE.age", c.want)
	}
	before := r.read("deploy.example.com/secrets/example/EDGE.age")
	for _, c := range []struct{ in, want string }{
		{"", "EDGE is empty"},
		{"\n", "EDGE is empty"},
		{"a\nb", "EDGE has a line break in it"},
		{"a\n\n", "EDGE has a line break in it"},
		{"a\rb", "EDGE has a carriage return in it"},
		{"a\x00b", "EDGE has a NUL byte in it"},
		{"\xff", "EDGE is not UTF-8 text"},
		{"\xed\xa0\x80", "EDGE is not UTF-8 text"}, // a surrogate
		{"\xc0\xaf", "EDGE is not UTF-8 text"},     // overlong
		{"\xe2\x82", "EDGE is not UTF-8 text"},     // cut short
		{strings.Repeat("v", 65536-len("EDGE")-3), "EDGE is too long: its line in the env file would be over 65536 bytes"},
	} {
		if out, code := r.secrets("", c.in, "set", "APP=example", "VAR=EDGE"); code != 1 || !strings.Contains(out, c.want) {
			t.Errorf("value %q: exit %d, want %q\n%s", c.in, code, c.want, out)
		}
	}
	if r.read("deploy.example.com/secrets/example/EDGE.age") != before {
		t.Error("a refused value replaced the one there")
	}

	// Names.
	for _, c := range []struct {
		env  []string
		want string
	}{
		{[]string{"APP=example", "VAR=1X"}, "variable '1X': letters, digits and _ only, not starting with a digit"},
		{[]string{"APP=example", "VAR=A-B"}, "variable 'A-B': letters, digits and _ only"},
		{[]string{"APP=example", "VAR=" + strings.Repeat("V", 129)}, "at most 128 bytes"},
		{[]string{"APP=example"}, "name the variable: VAR=DATABASE_URL"},
		{[]string{"VAR=X"}, "name the app: APP=example"},
		{[]string{"APP=Example", "VAR=X"}, "app 'Example': lower-case letters, digits and - only"},
		{[]string{"APP=recipients", "VAR=X"}, "that name is the recipients file's"},
		{[]string{"APP=" + strings.Repeat("a", 64), "VAR=X"}, "at most 63 characters"},
		{[]string{"APP=example", "VAR=X", "BOX=nope.example.com"}, "no box nope.example.com here"},
		{[]string{"APP=_box", "VAR=DATABASE_URL"}, "variable 'DATABASE_URL': the box's own secrets (APP=_box) are notify's channel URLs, each named NOTIFY_ then capitals, digits and _"},
		{[]string{"APP=_box", "VAR=NOTIFY_ops"}, "variable 'NOTIFY_ops': the box's own secrets (APP=_box) are notify's"},
		{[]string{"APP=_boxes", "VAR=X"}, "app '_boxes': lower-case letters, digits and - only, as in its app block (or _box, for a notify channel's URL)"},
	} {
		if out, code := r.secrets("", "v", "set", c.env...); code != 1 || !strings.Contains(out, c.want) {
			t.Errorf("%v: exit %d, want %q\n%s", c.env, code, c.want, out)
		}
	}

	// The block that reads them: no hint once the line is there.
	f := r.read(boxFile)
	r.write(boxFile, strings.Replace(f, "# env_file /etc/motel/secrets/example.env", "env_file /etc/motel/secrets/example.env", 1))
	if out, code := r.secrets("", "v", "set", "APP=example", "VAR=TOKEN"); code != 0 || strings.Contains(out, "env_file") {
		t.Errorf("with the env_file line: exit %d\n%s", code, out)
	}
	r.write(boxFile, strings.Replace(f, "# env_file /etc/motel/secrets/example.env", "env_file /etc/motel/example.env", 1))
	if out, code := r.secrets("", "v", "set", "APP=example", "VAR=TOKEN"); code != 0 || !strings.Contains(out, "example's block reads env_file /etc/motel/example.env; to read its secrets it needs:\nsecret:     env_file /etc/motel/secrets/example.env") {
		t.Errorf("another env_file: exit %d\n%s", code, out)
	}
	if out, code := r.secrets("", "v", "set", "APP=blog", "VAR=TOKEN"); code != 0 || !strings.Contains(out, boxFile+" has no app blog yet: the box takes secrets only for an app its file declares") {
		t.Errorf("an app the file does not declare: exit %d\n%s", code, out)
	}
	r.write(boxFile, f)

	// The box's own slot: a notify channel's URL, which the monitor
	// block reads by name; no env_file hint, and the notify hint once
	// the name is read.
	notifyFile := "deploy.example.com/secrets/_box/NOTIFY_OPS_URL.age"
	if out, code := r.secrets("", "https://ntfy.sh/ops-topic", "set", "APP=_box", "VAR=NOTIFY_OPS_URL"); code != 0 ||
		!strings.Contains(out, "secret: "+notifyFile+", encrypted to 3 keys, added to the commit\n") ||
		!strings.Contains(out, "secret: no line of "+boxFile+"'s monitor block reads NOTIFY_OPS_URL yet: a channel reads it as  channel <name> json|ntfy|slack|discord {secret.NOTIFY_OPS_URL}") ||
		strings.Contains(out, "env_file") {
		t.Errorf("APP=_box: exit %d\n%s", code, out)
	}
	r.mustDecrypt(boxID, notifyFile, "https://ntfy.sh/ops-topic")
	r.write(boxFile, strings.Replace(f, "{\n", "{\n\tmonitor {\n\t\tchannel ops ntfy {secret.NOTIFY_OPS_URL}\n\t\toperator ops\n\t}\n", 1))
	if out, code := r.secrets("", "https://ntfy.sh/ops-2", "set", "APP=_box", "VAR=NOTIFY_OPS_URL"); code != 0 || strings.Contains(out, "monitor block reads") {
		t.Errorf("APP=_box, read by a channel line: exit %d\n%s", code, out)
	}
	r.write(boxFile, f)
	r.remove("deploy.example.com/secrets/_box")

	// On a Mac's disk two names that differ in case are one file: the
	// second would overwrite the first.
	r.write("deploy.example.com/secrets/example/Database_URL.age", "x")
	if out, code := r.secrets("", "v", "set", app...); code != 1 || !strings.Contains(out, "deploy.example.com/secrets/example/Database_URL.age differs from DATABASE_URL.age only in case: on a Mac's default disk they are one file") {
		t.Errorf("a case twin: exit %d\n%s", code, out)
	}
	if r.read("deploy.example.com/secrets/example/Database_URL.age") != "x" {
		t.Error("the case twin was overwritten")
	}
	r.remove("deploy.example.com/secrets/example/Database_URL.age")
	// The suffix too: DATABASE_URL.AGE is DATABASE_URL.age there.
	r.write("deploy.example.com/secrets/example/DATABASE_URL.AGE", "x")
	if out, code := r.secrets("", "v", "set", app...); code != 1 || !strings.Contains(out, "deploy.example.com/secrets/example/DATABASE_URL.AGE differs from DATABASE_URL.age only in case") {
		t.Errorf("a case twin by its suffix: exit %d\n%s", code, out)
	}
	r.remove("deploy.example.com/secrets/example/DATABASE_URL.AGE")

	// (An app's variables and the apps with secrets, against the box's
	// caps, are counted by make check in a pull request and by the box:
	// this checks only what it writes.)

	// The recipients file's first key is the box's, or nothing is
	// encrypted to it.
	rec = r.read(recipientsFile)
	r.write(recipientsFile, "# alice first\nssh-ed25519 "+r.b64(alice+".pub")+"\n"+rec)
	if out, code := r.secrets("", "v", "set", app...); code != 1 || !strings.Contains(out, "the first key in the recipients file is not a box's") {
		t.Errorf("a reader first: exit %d\n%s", code, out)
	}
	if out, code := r.secrets("", "", "rekey", "IDENTITY="+alice); code != 1 || !strings.Contains(out, "the first key in the recipients file is not a box's") {
		t.Errorf("rekey with a reader first: exit %d\n%s", code, out)
	}
	r.write(recipientsFile, rec)

	// More than one box: BOX names it.
	r.write("deploy2.example.com/Caddyfile", strings.Replace(f, "deploy.example.com {", "deploy2.example.com {", 1))
	if out, code := r.secrets("", "v", "set", app...); code != 1 || !strings.Contains(out, "more than one box; name one with BOX=: deploy.example.com deploy2.example.com") {
		t.Errorf("two boxes: exit %d\n%s", code, out)
	}
	if out, code := r.secrets("", "v", "set", append(app, "BOX=deploy.example.com/")...); code != 0 {
		t.Errorf("BOX=: exit %d\n%s", code, out)
	}
	// A box tracked but deleted here is on its way out, as make signer
	// and make check read it: the other is then the only one.
	r.commit("a second box")
	r.remove("deploy2.example.com")
	if out, code := r.secrets("", "v", "set", app...); code != 0 {
		t.Errorf("a deleted second box: exit %d\n%s", code, out)
	}
}

// The shells it runs under: the container's /bin/sh is dash; a Mac's is
// bash 3.2 in POSIX mode.
func TestBoxSecretSetUnderBash(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash here")
	}
	r := secretsRepo(t)
	r.key("id_ed25519")
	if out, code := r.run("signer"); code != 0 {
		t.Fatalf("make signer: exit %d\n%s", code, out)
	}
	boxID, box := r.boxKey("box")
	if out, code := r.secrets("bash", "v1\n", "set", "APP=example", "VAR=A", "BOX_KEY="+box); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	r.mustDecrypt(boxID, "deploy.example.com/secrets/example/A.age", "v1")
	if out, code := r.secrets("bash", "B=two\n", "import", "APP=example"); code != 0 {
		t.Fatalf("import: exit %d\n%s", code, out)
	}
	r.mustDecrypt(boxID, "deploy.example.com/secrets/example/B.age", "two")
}

func firstKey(recipients string) string {
	for _, l := range strings.Split(recipients, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			return strings.Fields(l)[0]
		}
	}
	return ""
}

func TestBoxSecretsListShowRekey(t *testing.T) {
	t.Parallel()
	r, alice, carol := readersRepo(t)
	boxID, box := r.boxKey("box")
	if out, code := r.secrets("", "one", "set", "APP=example", "VAR=DATABASE_URL", "BOX_KEY="+box); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if out, code := r.secrets("", "two", "set", "APP=example", "VAR=STRIPE_KEY"); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}

	// The names and the readers, with no key.
	out, code := r.secrets("", "", "list")
	if want := "deploy.example.com\n  readers: this box (" + shortKey(box) + "), alice@example.com, carol\n  example: DATABASE_URL STRIPE_KEY\n"; code != 0 || out != want {
		t.Fatalf("list: exit %d\n%s\nwant:\n%s", code, out, want)
	}
	// A signer whose key age takes, not in the file: named.
	erin := r.key("erin")
	f := r.read(boxFile)
	r.write(boxFile, strings.Replace(f, "\t\t# signers\n", "\t\t# signers\n\t\tsigner erin ssh-ed25519 "+r.b64(erin)+"\n", 1))
	if out, _ := r.secrets("", "", "list"); !strings.Contains(out, "  erin signs but is not a reader: their make signer adds them, then a reader runs make rekey\n") {
		t.Errorf("a signer who is not a reader:\n%s", out)
	}
	// The last signer a hardware key, which can never be a reader: still
	// a clean exit.
	carolLine := "signer carol ssh-rsa " + r.b64(carol+".pub") + "\n"
	if !strings.Contains(f, carolLine) {
		t.Fatalf("no carol line in:\n%s", f)
	}
	r.write(boxFile, strings.Replace(f, carolLine, carolLine+"\t\tsigner zed sk-ssh-ed25519@openssh.com AAAAGnNrLXNzaC1lZDI1NTE5QG9wZW5zc2guY29tAAAAIA==\n", 1))
	if out, code := r.secrets("", "", "list"); code != 0 {
		t.Errorf("list with an sk- signer last: exit %d\n%s", code, out)
	}
	r.write(boxFile, f)

	// Read back: with IDENTITY, else the key this checkout signs with.
	if out, code := r.secrets("", "", "show", "APP=example", "VAR=STRIPE_KEY"); code != 0 || out != "two" {
		t.Errorf("show as the signing key: exit %d %q", code, out)
	}
	if out, code := r.secrets("", "", "show", "APP=example", "VAR=STRIPE_KEY", "IDENTITY="+carol); code != 0 || out != "two" {
		t.Errorf("show with IDENTITY: exit %d %q", code, out)
	}
	if out, code := r.secrets("", "", "show", "APP=example", "VAR=NOPE"); code != 1 || !strings.Contains(out, "no deploy.example.com/secrets/example/NOPE.age: make secrets lists what there is") {
		t.Errorf("show of nothing: exit %d\n%s", code, out)
	}
	r.git("config", "--unset", "user.signingkey")
	if out, code := r.secrets("", "", "show", "APP=example", "VAR=STRIPE_KEY"); code != 1 || !strings.Contains(out, "no private key to read with: name yours with IDENTITY=") {
		t.Errorf("show with no key: exit %d\n%s", code, out)
	}

	// A rebuilt box: every value to its new key, as a reader.
	newID, newBox := r.boxKey("rebuilt")
	out, code = r.secrets("", "", "rekey", "BOX_KEY="+newBox, "IDENTITY="+alice)
	if code != 0 || !strings.Contains(out, "rekey: "+recipientsFile+" now names the box as "+shortKey(newBox)+"\n") ||
		!strings.Contains(out, "rekey: deploy.example.com: 2 secrets encrypted again to 3 keys, in the commit\n") {
		t.Fatalf("rekey: exit %d\n%s", code, out)
	}
	if first := firstKey(r.read(recipientsFile)); first != newBox {
		t.Errorf("first key after rekey: %q", first)
	}
	for _, f := range []string{dbFile, "deploy.example.com/secrets/example/STRIPE_KEY.age"} {
		r.mustDecrypt(newID, f, map[bool]string{true: "one", false: "two"}[f == dbFile])
		if _, err := r.decrypt(boxID, f); err == nil {
			t.Errorf("%s still opens with the old box's key", f)
		}
		r.mustDecrypt(carol, f, map[bool]string{true: "one", false: "two"}[f == dbFile])
	}

	// A reader added to the recipients file by hand: the rekey that
	// encrypts to them stages the file too, so the commit's ciphertext
	// and the readers it names agree.
	r.commit("rekeyed", "-c", "commit.gpgsign=false") // the signing key was unset above
	ivy := r.key("ivy")
	r.write(recipientsFile, r.read(recipientsFile)+"# ivy\nssh-ed25519 "+r.b64(ivy)+"\n")
	if out, code := r.secrets("", "", "rekey", "IDENTITY="+alice); code != 0 {
		t.Fatalf("rekey to a hand-added reader: exit %d\n%s", code, out)
	}
	if got := r.git("diff", "--name-only"); got != "" {
		t.Errorf("left unstaged after rekey: %q", got)
	}
	r.mustDecrypt(strings.TrimSuffix(ivy, ".pub"), dbFile, "one")
	// And make secret, the same.
	r.commit("ivy", "-c", "commit.gpgsign=false") // the signing key was unset above
	jo := r.key("jo")
	r.write(recipientsFile, r.read(recipientsFile)+"# jo\nssh-ed25519 "+r.b64(jo)+"\n")
	if out, code := r.secrets("", "three", "set", "APP=example", "VAR=STRIPE_KEY"); code != 0 {
		t.Fatalf("set to a hand-added reader: exit %d\n%s", code, out)
	}
	if got := r.git("diff", "--name-only"); got != "" {
		t.Errorf("left unstaged after set: %q", got)
	}

	// A value this reader cannot open: nothing changes.
	c := exec.Command(filepath.Join(ageTools(t), "age"), "-r", newBox, "-o", filepath.Join(r.dir, "deploy.example.com/secrets/example/BOX_ONLY.age"))
	c.Stdin = strings.NewReader("box only")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("age: %v\n%s", err, out)
	}
	snap := map[string]string{}
	for _, f := range []string{dbFile, "deploy.example.com/secrets/example/STRIPE_KEY.age", recipientsFile} {
		snap[f] = r.read(f)
	}
	_, third := r.boxKey("third")
	if out, code := r.secrets("", "", "rekey", "BOX_KEY="+third, "IDENTITY="+alice); code != 1 ||
		!strings.Contains(out, "could not read deploy.example.com/secrets/example/BOX_ONLY.age with "+alice+", so nothing changed") {
		t.Fatalf("rekey of a value it cannot read: exit %d\n%s", code, out)
	}
	for f, b := range snap {
		if r.read(f) != b {
			t.Errorf("%s changed in a refused rekey", f)
		}
	}
}

// A NOTIFY_ name in an app's directory is warned of on stderr: what show
// prints on stdout is the value alone, so a capture of it is too.
func TestBoxSecretShowOfANotifyNameIsTheValueAlone(t *testing.T) {
	t.Parallel()
	r, _, _ := readersRepo(t)
	_, box := r.boxKey("box")
	if out, code := r.secrets("", "three", "set", "APP=example", "VAR=NOTIFY_LEGACY", "BOX_KEY="+box); code != 0 {
		t.Fatalf("set NOTIFY_LEGACY: exit %d\n%s", code, out)
	}
	show := exec.Command(filepath.Join(r.dir, "bin", "secrets"), "show")
	show.Dir, show.Env = r.dir, append(append([]string{}, r.env...), "APP=example", "VAR=NOTIFY_LEGACY")
	var stdout, stderr strings.Builder
	show.Stdout, show.Stderr = &stdout, &stderr
	if err := show.Run(); err != nil || stdout.String() != "three" ||
		!strings.Contains(stderr.String(), "motel withholds a NOTIFY_ variable from example's environment") {
		t.Errorf("show of a NOTIFY_ name: %v\nstdout %q\nstderr %q", err, stdout.String(), stderr.String())
	}
}

func TestBoxSignerAddsAReader(t *testing.T) {
	t.Parallel()
	r := secretsRepo(t)
	r.key("id_ed25519")
	if out, code := r.run("signer"); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	// No box with secrets: nothing about readers.
	erin := r.key("erin")
	if out, code := r.run("signer", "KEY="+erin, "PRINCIPAL=erin"); code != 0 || strings.Contains(out, "reader") || strings.Contains(out, "age-keygen") {
		t.Fatalf("no secrets yet: exit %d\n%s", code, out)
	}
	_, box := r.boxKey("box")
	if out, code := r.secrets("", "v", "set", "APP=example", "VAR=A", "BOX_KEY="+box); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	r.commit("a secret")

	frank := r.key("frank")
	out, code := r.run("signer", "KEY="+frank, "PRINCIPAL=frank")
	if code != 0 || !strings.Contains(out, "signer: added frank to "+recipientsFile+" as a reader; the secrets already there become readable to them after a reader runs make rekey\n") {
		t.Fatalf("a reader added: exit %d\n%s", code, out)
	}
	if !strings.HasSuffix(r.read(recipientsFile), "# frank\nssh-ed25519 "+r.b64(frank)+"\n") {
		t.Errorf("recipients:\n%s", r.read(recipientsFile))
	}
	if got := r.git("diff", "--cached", "--name-only"); !strings.Contains(got, recipientsFile) {
		t.Errorf("the recipients file is not staged: %q", got)
	}
	if out, _ := r.run("signer", "KEY="+frank, "PRINCIPAL=frank"); !strings.Contains(out, recipientsFile+" already lists this key as a reader") {
		t.Errorf("again:\n%s", out)
	}

	// A key a box no longer takes: an ECDSA signature has two forms, so
	// bin/signer refuses the key by name, before any line is written.
	priv := filepath.Join(r.home, ".ssh", "gina")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ecdsa", "-N", "", "-f", priv).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	before := r.read(recipientsFile)
	out, code = r.run("signer", "KEY="+priv+".pub", "PRINCIPAL=gina")
	if code == 0 || !strings.Contains(out, "is an ecdsa-sha2-nistp256 key, which a box no longer takes: an ECDSA signature has two forms") || !strings.Contains(out, "ssh-keygen -t ed25519") {
		t.Fatalf("an ecdsa key: exit %d\n%s", code, out)
	}
	if r.read(recipientsFile) != before {
		t.Error("an ecdsa key went into the recipients file")
	}
	// A key age cannot take, which a box does: a hardware sk- key, made
	// here by hand from an ed25519 one (the wire form is the ed25519
	// key's plus an application string; ssh-keygen -l reads it).
	edPriv := filepath.Join(r.home, ".ssh", "gina-sk")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", edPriv).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	edWire, err := base64.StdEncoding.DecodeString(strings.Fields(r.read(edPriv + ".pub"))[1])
	if err != nil {
		t.Fatal(err)
	}
	var sk bytes.Buffer
	for _, s := range [][]byte{[]byte("sk-ssh-ed25519@openssh.com"), edWire[len(edWire)-32:], []byte("ssh:")} {
		sk.Write([]byte{byte(len(s) >> 24), byte(len(s) >> 16), byte(len(s) >> 8), byte(len(s))})
		sk.Write(s)
	}
	skPub := filepath.Join(r.home, ".ssh", "gina-hw.pub")
	if err := os.WriteFile(skPub, []byte("sk-ssh-ed25519@openssh.com "+base64.StdEncoding.EncodeToString(sk.Bytes())+" gina\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code = r.run("signer", "KEY="+skPub, "PRINCIPAL=gina")
	if code != 0 || !strings.Contains(out, "signer: this key is a sk-ssh-ed25519@openssh.com key, which age cannot encrypt to, so it cannot read the boxes' secrets:") ||
		!strings.Contains(out, "make secret-show IDENTITY=~/.config/age/motel.txt") {
		t.Fatalf("an sk- key: exit %d\n%s", code, out)
	}
	if r.read(recipientsFile) != before {
		t.Error("an sk- key went into the recipients file")
	}

	// A file edited by hand, with no newline at its end: the key still
	// goes on a line of its own, and the edit is left for git add -p.
	r.write(recipientsFile, strings.TrimSuffix(before, "\n"))
	hank := r.key("hank")
	out, code = r.run("signer", "KEY="+hank, "PRINCIPAL=hank")
	if code != 0 || !strings.Contains(out, recipientsFile+" has other edits, or is not committed yet, so it is not staged") {
		t.Fatalf("a hand-edited file: exit %d\n%s", code, out)
	}
	if want := before + "# hank\nssh-ed25519 " + r.b64(hank) + "\n"; r.read(recipientsFile) != want {
		t.Errorf("recipients:\n%s\nwant:\n%s", r.read(recipientsFile), want)
	}
}

// An agent holds the key, with no file beside the .pub: still a
// reader in the file, and told that age cannot read through an agent.
func TestBoxSignerAgentHeldReader(t *testing.T) {
	t.Parallel()
	r := secretsRepo(t)
	r.key("id_ed25519")
	if out, code := r.run("signer"); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	_, box := r.boxKey("box")
	if out, code := r.secrets("", "v", "set", "APP=example", "VAR=A", "BOX_KEY="+box); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	kim := r.key("kim")
	if err := os.Remove(strings.TrimSuffix(kim, ".pub")); err != nil {
		t.Fatal(err)
	}
	out, code := r.run("signer", "KEY="+kim, "PRINCIPAL=kim")
	if code != 0 || !strings.Contains(out, "signer: an agent holds this key, with no file beside the .pub, and age decrypts only with a key file") ||
		!strings.Contains(out, "add its age1… line under '# kim'") {
		t.Fatalf("an agent-held key: exit %d\n%s", code, out)
	}
	if !strings.Contains(r.read(recipientsFile), "# kim\nssh-ed25519 "+r.b64(kim)+"\n") {
		t.Errorf("recipients:\n%s", r.read(recipientsFile))
	}
}

// A symbolic link anywhere in a box's secrets is refused before
// anything is written through it: the box takes none, and through one
// a write could reach outside the repository.
func TestBoxSecretsRefuseLinks(t *testing.T) {
	t.Parallel()
	r := secretsRepo(t)
	r.key("id_ed25519")
	if out, code := r.run("signer"); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	_, box := r.boxKey("box")
	if out, code := r.secrets("", "v", "set", "APP=example", "VAR=A", "BOX_KEY="+box); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	outside := t.TempDir()
	victim := filepath.Join(outside, "authorized_keys")
	if err := os.WriteFile(victim, []byte("theirs\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := func(rel, target string) {
		t.Helper()
		p := filepath.Join(r.dir, rel)
		if err := os.RemoveAll(p); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
	}
	rec := r.read(recipientsFile)
	appDir := "deploy.example.com/secrets/example"

	// The recipients file a link: make signer would append through it,
	// and make secrets would print parts of whatever it names.
	link(recipientsFile, victim)
	lee := r.key("lee")
	caddy := r.read(boxFile)
	if out, code := r.run("signer", "KEY="+lee, "PRINCIPAL=lee"); code == 0 || !strings.Contains(out, recipientsFile+" is a symbolic link") {
		t.Errorf("make signer over a linked recipients file: exit %d\n%s", code, out)
	}
	if r.read(boxFile) != caddy {
		t.Error("a refused make signer still changed the Caddyfile")
	}
	for _, cmd := range []string{"set", "rekey", "import", "list"} {
		if out, code := r.secrets("", "B=1\n", cmd, "APP=example", "VAR=B"); code != 1 || !strings.Contains(out, recipientsFile+" is a symbolic link") {
			t.Errorf("%s over a linked recipients file: exit %d\n%s", cmd, code, out)
		}
	}
	link(recipientsFile, "x") // a dangling one too
	if out, code := r.secrets("", "v", "set", "APP=example", "VAR=B"); code != 1 || !strings.Contains(out, "is a symbolic link") {
		t.Errorf("set over a dangling link: exit %d\n%s", code, out)
	}
	if err := os.Remove(filepath.Join(r.dir, recipientsFile)); err != nil {
		t.Fatal(err)
	}
	r.write(recipientsFile, rec)

	// The app's directory a link: the new .age would land outside.
	saved := r.read(appDir + "/A.age")
	link(appDir, outside)
	for _, cmd := range []string{"set", "import", "show"} {
		if out, code := r.secrets("", "B=1\n", cmd, "APP=example", "VAR=B"); code != 1 || !strings.Contains(out, appDir+" is a symbolic link") {
			t.Errorf("%s into a linked app directory: exit %d\n%s", cmd, code, out)
		}
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 1 {
		t.Errorf("something was written outside the repository: %v", entries)
	}
	if err := os.Remove(filepath.Join(r.dir, appDir)); err != nil {
		t.Fatal(err)
	}
	r.write(appDir+"/A.age", saved)

	// A value's file a link.
	link(appDir+"/B.age", victim)
	if out, code := r.secrets("", "v", "set", "APP=example", "VAR=B"); code != 1 || !strings.Contains(out, appDir+"/B.age is a symbolic link") {
		t.Errorf("set over a linked value: exit %d\n%s", code, out)
	}
	if out, code := r.secrets("", "", "rekey", "IDENTITY="+filepath.Join(r.home, ".ssh", "id_ed25519")); code != 1 || !strings.Contains(out, appDir+"/B.age is a symbolic link") {
		t.Errorf("rekey with a linked value: exit %d\n%s", code, out)
	}
	if b, _ := os.ReadFile(victim); string(b) != "theirs\n" {
		t.Errorf("the file outside was written: %q", b)
	}
	if err := os.Remove(filepath.Join(r.dir, appDir, "B.age")); err != nil {
		t.Fatal(err)
	}

	// A path it writes that is not what it must be: a move into a
	// directory where a file belongs would nest it there.
	r.remove(recipientsFile)
	r.write(recipientsFile+"/x", "")
	if out, code := r.secrets("", "v", "set", "APP=example", "VAR=C"); code != 1 || !strings.Contains(out, recipientsFile+" is not a regular file") || r.exists(appDir+"/C.age") {
		t.Errorf("a directory for the recipients file: exit %d\n%s", code, out)
	}
	r.remove(recipientsFile)
	r.write(recipientsFile, rec)
	r.write(appDir+"/C.age/x", "")
	if out, code := r.secrets("", "v", "set", "APP=example", "VAR=C"); code != 1 || !strings.Contains(out, appDir+"/C.age is not a regular file") {
		t.Errorf("a directory for the value: exit %d\n%s", code, out)
	}
	r.remove(appDir + "/C.age")
	r.write("deploy.example.com/secrets/blog", "")
	if out, code := r.secrets("", "v", "set", "APP=blog", "VAR=C"); code != 1 || !strings.Contains(out, "deploy.example.com/secrets/blog is not a directory") {
		t.Errorf("a file for the app's directory: exit %d\n%s", code, out)
	}
	r.remove("deploy.example.com/secrets/blog")
	if out, code := r.secrets("", "v", "set", "APP=example", "VAR=C"); code != 0 {
		t.Errorf("the paths put back: exit %d\n%s", code, out)
	}

	// The box's own Caddyfile a link: its signer lines would be copied
	// out of whatever it names.
	caddy = r.read(boxFile)
	outsideCaddy := filepath.Join(outside, "Caddyfile")
	if err := os.WriteFile(outsideCaddy, []byte(caddy), 0o644); err != nil {
		t.Fatal(err)
	}
	link(boxFile, outsideCaddy)
	for _, cmd := range []string{"set", "list"} {
		if out, code := r.secrets("", "v", cmd, "APP=example", "VAR=D"); code != 1 || !strings.Contains(out, boxFile+" is a symbolic link") {
			t.Errorf("%s with a linked Caddyfile: exit %d\n%s", cmd, code, out)
		}
	}
	if err := os.Remove(filepath.Join(r.dir, boxFile)); err != nil {
		t.Fatal(err)
	}
	r.write(boxFile, caddy)
}

// make signer with more than one box: a box whose recipients file it
// may not write stops it before any box is written, the good one too.
func TestBoxSignerChecksEveryBoxFirst(t *testing.T) {
	t.Parallel()
	r := secretsRepo(t)
	r.key("id_ed25519")
	if out, code := r.run("signer"); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	two := strings.Replace(r.read(boxFile), "deploy.example.com {", "deploy2.example.com {", 1)
	r.write("deploy2.example.com/Caddyfile", two)
	_, box := r.boxKey("box")
	for _, b := range []string{"deploy.example.com", "deploy2.example.com"} {
		if out, code := r.secrets("", "v", "set", "APP=example", "VAR=A", "BOX="+b, "BOX_KEY="+box); code != 0 {
			t.Fatalf("%s: exit %d\n%s", b, code, out)
		}
	}
	before := map[string]string{}
	for _, f := range []string{boxFile, recipientsFile, "deploy2.example.com/Caddyfile", "deploy2.example.com/secrets/recipients"} {
		before[f] = r.read(f)
	}
	// The second box's recipients file a link, to a copy of itself.
	copyOf := filepath.Join(t.TempDir(), "recipients")
	if err := os.WriteFile(copyOf, []byte(before["deploy2.example.com/secrets/recipients"]), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(r.dir, "deploy2.example.com/secrets/recipients")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(copyOf, filepath.Join(r.dir, "deploy2.example.com/secrets/recipients")); err != nil {
		t.Fatal(err)
	}
	mo := r.key("mo")
	if out, code := r.run("signer", "KEY="+mo, "PRINCIPAL=mo"); code == 0 || !strings.Contains(out, "deploy2.example.com/secrets/recipients is a symbolic link") {
		t.Fatalf("a bad second box: exit %d\n%s", code, out)
	}
	for f, b := range before {
		if r.read(f) != b {
			t.Errorf("%s changed in a refused make signer", f)
		}
	}
}

// snapshot is every regular file under dir, but .git, by its contents.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if d.Type().IsRegular() {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files[path] = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// A branch someone else pushed may hold links where a box's secrets go,
// and checking it out puts them here. No command reads through one: a
// linked .age would have make rekey decrypt a file from this laptop and
// encrypt it into the repository, and a linked secrets directory would
// have every command work on files outside it. git's index is asked
// too, for a link the checkout has not made yet.
func TestBoxSecretsNeverReadThroughALink(t *testing.T) {
	t.Parallel()
	r := secretsRepo(t)
	alicePub := r.key("id_ed25519")
	alice := strings.TrimSuffix(alicePub, ".pub")
	if out, code := r.run("signer"); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	_, box := r.boxKey("box")
	if out, code := r.secrets("", "v", "set", "APP=example", "VAR=A", "BOX_KEY="+box); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	r.commit("a secret")
	appDir := filepath.Join(r.dir, "deploy.example.com/secrets/example")

	// A file of this laptop's, encrypted to alice: what a linked .age
	// would hand make rekey to decrypt.
	const local = "a secret of this laptop's"
	outside := t.TempDir()
	localAge := filepath.Join(outside, "local.age")
	c := exec.Command(filepath.Join(ageTools(t), "age"), "-R", alicePub, "-o", localAge)
	c.Stdin = strings.NewReader(local)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("age: %v\n%s", err, out)
	}
	leak := filepath.Join(appDir, "LEAK.age")
	if err := os.Symlink(localAge, leak); err != nil {
		t.Fatal(err)
	}
	repoBefore, outsideBefore := snapshot(t, r.dir), snapshot(t, outside)
	refusals := []struct {
		cmd, stdin string
		env        []string
	}{
		{"set", "v", []string{"APP=example", "VAR=LEAK"}},
		{"show", "", []string{"APP=example", "VAR=LEAK", "IDENTITY=" + alice}},
		{"list", "", nil},
		{"rekey", "", []string{"IDENTITY=" + alice}},
		{"import", "LEAK=v\n", []string{"APP=example"}},
	}
	for _, c := range refusals {
		if out, code := r.secrets("", c.stdin, c.cmd, c.env...); code != 1 || !strings.Contains(out, "deploy.example.com/secrets/example/LEAK.age is a symbolic link") {
			t.Errorf("%s with a linked .age: exit %d\n%s", c.cmd, code, out)
		}
	}
	if fi, err := os.Lstat(leak); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("LEAK.age is no longer the link: %v %v", fi, err)
	}
	if got := snapshot(t, r.dir); !mapsEqual(got, repoBefore) {
		t.Error("a refused command changed the repository")
	}
	if got := snapshot(t, outside); !mapsEqual(got, outsideBefore) {
		t.Error("a refused command changed the file outside")
	}
	for path := range snapshot(t, r.dir) {
		if strings.HasSuffix(path, ".age") {
			if v, err := r.decrypt(alice, path); err == nil && v == local {
				t.Fatalf("%s holds the laptop's secret", path)
			}
		}
	}
	if err := os.Remove(leak); err != nil {
		t.Fatal(err)
	}

	// The secrets directory itself a link, to a copy outside that the
	// box would take.
	secrets := filepath.Join(r.dir, "deploy.example.com/secrets")
	moved := filepath.Join(outside, "secrets")
	if err := os.Rename(secrets, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, secrets); err != nil {
		t.Fatal(err)
	}
	outsideBefore = snapshot(t, outside)
	kim := r.key("kim")
	for _, c := range append(refusals[:0:0], []struct {
		cmd, stdin string
		env        []string
	}{
		{"set", "v", []string{"APP=example", "VAR=B"}},
		{"show", "", []string{"APP=example", "VAR=A", "IDENTITY=" + alice}},
		{"list", "", nil},
		{"rekey", "", []string{"IDENTITY=" + alice}},
		{"import", "B=v\n", []string{"APP=example"}},
	}...) {
		if out, code := r.secrets("", c.stdin, c.cmd, c.env...); code != 1 || !strings.Contains(out, "deploy.example.com/secrets is a symbolic link") {
			t.Errorf("%s with a linked secrets directory: exit %d\n%s", c.cmd, code, out)
		}
	}
	if out, code := r.run("signer", "KEY="+kim, "PRINCIPAL=kim"); code == 0 || !strings.Contains(out, "deploy.example.com/secrets is a symbolic link") {
		t.Errorf("make signer with a linked secrets directory: exit %d\n%s", code, out)
	}
	if got := snapshot(t, outside); !mapsEqual(got, outsideBefore) {
		t.Error("a refused command changed the files outside")
	}
	if err := os.Remove(secrets); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(moved, secrets); err != nil {
		t.Fatal(err)
	}
	r.git("checkout", "--", ".")

	// A link in git's index, a file in its place here: the next
	// checkout makes it a link again.
	if err := os.Symlink(localAge, filepath.Join(appDir, "LINKED.age")); err != nil {
		t.Fatal(err)
	}
	r.commit("a link", "-c", "commit.gpgsign=false")
	if err := os.Remove(filepath.Join(appDir, "LINKED.age")); err != nil {
		t.Fatal(err)
	}
	r.write("deploy.example.com/secrets/example/LINKED.age", "x")
	for _, c := range []struct {
		cmd string
		env []string
	}{
		{"rekey", []string{"IDENTITY=" + alice}},
		{"show", []string{"APP=example", "VAR=LINKED", "IDENTITY=" + alice}},
	} {
		if out, code := r.secrets("", "", c.cmd, c.env...); code != 1 || !strings.Contains(out, "deploy.example.com/secrets/example/LINKED.age is a symbolic link in git's index") {
			t.Errorf("%s with a link in the index: exit %d\n%s", c.cmd, code, out)
		}
	}
	// Under a box whose name git would quote by default (core.quotePath):
	// the box is listed, and its index entries read, as they are named.
	r.git("mv", "deploy.example.com", "café")
	if out, code := r.secrets("", "", "list"); code != 1 || !strings.Contains(out, "café/secrets/example/LINKED.age is a symbolic link in git's index") {
		t.Errorf("a link in the index under a non-ASCII box name: exit %d\n%s", code, out)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func TestBoxSecretsNeedAge(t *testing.T) {
	t.Parallel()
	r := newBoxRepo(t) // the age CLI not on PATH
	if _, err := exec.LookPath("age"); err == nil {
		t.Skip("age is installed here")
	}
	for _, cmd := range []string{"set", "show", "rekey", "import"} {
		if out, code := r.secrets("", "A=1\n", cmd, "APP=example", "VAR=A"); code != 1 || !strings.Contains(out, "age is not installed here: brew install age, or apt install age") {
			t.Errorf("%s with no age: exit %d\n%s", cmd, code, out)
		}
	}
}

func TestBoxSecretsImport(t *testing.T) {
	t.Parallel()
	r := secretsRepo(t)
	r.key("id_ed25519")
	if out, code := r.run("signer"); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	boxID, box := r.boxKey("box")
	nbsp, em, ideo, mmsp, nel := string(rune(0xa0)), string(rune(0x2003)), string(rune(0x3000)), string(rune(0x205f)), string(rune(0x85))
	corpus := strings.Join([]string{
		"# a comment",
		"",
		"   ",
		"export A=1",
		"B = two words  ",
		`C="quoted value"`,
		`D='single'`,
		`E="unbalanced`,
		"F=a=b=c",
		"G=  spaced" + nbsp,
		em + "H=em" + em,
		`I="  inner spaces kept  "`,
		"J=#not a comment",
		"K=" + ideo + "tail" + mmsp,
		"\tTAB_ED=x\t",
		"L=" + nel + "nel" + nel,
		`M=""quoted twice""`,
		"N=caf" + string(rune(0xe9)),
		"A=again",
		"CR=line\r",
		"",
	}, "\n")
	// The box's own reading of an env file (liveswap.ParseEnvLines): make
	// secrets-import must read a file as every launch does.
	want, err := liveswap.ParseEnvLines("corpus", []byte(corpus))
	if err != nil {
		t.Fatal(err)
	}
	out, code := r.secrets("", corpus, "import", "APP=example", "BOX_KEY="+box)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "secrets: note: A is given again on line 19; the last one is kept\n") {
		t.Errorf("no note of A twice:\n%s", out)
	}
	last := map[string]string{}
	for _, kv := range want {
		k, v, _ := strings.Cut(kv, "=")
		last[k] = v
	}
	for k, v := range last {
		r.mustDecrypt(boxID, "deploy.example.com/secrets/example/"+k+".age", v)
	}
	if !strings.Contains(out, "secrets: deploy.example.com/secrets/example: added A B C D E F G H I J K TAB_ED L M N CR\n") ||
		!strings.Contains(out, fmt.Sprintf("secrets: %d values, encrypted to 2 keys, in the commit\n", len(last))) {
		t.Errorf("summary:\n%s", out)
	}

	// Lines motel refuses, it refuses, and then imports nothing.
	for _, c := range []struct{ line, want string }{
		{"1BAD=x", "line 2: the name is not letters, digits and _, not starting with a digit"},
		{"NOEQUALS", "line 2: not KEY=VALUE"},
		{"=x", "line 2: not KEY=VALUE"},
		{"export\tX=1", "line 2: the name is not letters"},
		{"BAD-NAME=x", "line 2: the name is not letters"},
		{"EMPTY=", "line 2: EMPTY is empty"},
		{`EMPTY=""`, "line 2: EMPTY is empty"},
	} {
		input := "OK=1\n" + c.line + "\n"
		if _, err := liveswap.ParseEnvLines("input", []byte(input)); err == nil && !strings.Contains(c.want, "is empty") {
			t.Errorf("%q: motel's parser takes it, the test expects a refusal", c.line)
		}
		out, code := r.secrets("", input, "import", "APP=other")
		if code != 1 || !strings.Contains(out, "secrets: "+c.want) || !strings.Contains(out, "nothing imported") {
			t.Errorf("%q: exit %d, want %q\n%s", c.line, code, c.want, out)
		}
		if r.exists("deploy.example.com/secrets/other") {
			t.Fatalf("%q: a refused import wrote something", c.line)
		}
	}
	if out, code := r.secrets("", "# nothing\n", "import", "APP=other"); code != 1 || !strings.Contains(out, "the env file has no KEY=VALUE line") {
		t.Errorf("an empty file: exit %d\n%s", code, out)
	}
	// Two names one file on a Mac's disk, in the file or against one
	// already there.
	if out, code := r.secrets("", "foo=1\nFOO=2\n", "import", "APP=other"); code != 1 || !strings.Contains(out, "line 2: FOO and foo differ only in case") || r.exists("deploy.example.com/secrets/other") {
		t.Errorf("foo and FOO: exit %d\n%s", code, out)
	}
	if out, code := r.secrets("", "a=1\n", "import", "APP=example"); code != 1 || !strings.Contains(out, "deploy.example.com/secrets/example/A.age differs from a.age only in case") {
		t.Errorf("a against A: exit %d\n%s", code, out)
	}
	// A value at the cap, in time linear in its length.
	big := strings.Repeat("v", 65536-len("BIG")-4)
	if out, code := r.secrets("", "BIG="+big+"\n", "import", "APP=big"); code != 0 {
		t.Fatalf("a 64 KiB value: exit %d\n%s", code, out)
	}
	r.mustDecrypt(boxID, "deploy.example.com/secrets/big/BIG.age", big)
	// The wrong file entirely.
	if out, code := r.secrets("", strings.Repeat("x", 1048577), "import", "APP=other"); code != 1 || !strings.Contains(out, "the file is over 1048576 bytes") {
		t.Errorf("a file over the cap: exit %d\n%s", code, out)
	}
	// A value the box would refuse, inside a line motel's parser takes.
	if out, code := r.secrets("", "OK=1\nNUL=a\x00b\n", "import", "APP=other"); code != 1 || !strings.Contains(out, "NUL has a NUL byte in it") || r.exists("deploy.example.com/secrets/other") {
		t.Errorf("a NUL: exit %d\n%s", code, out)
	}
}

// TestBoxCheckSecrets is make check over a box with secrets: describe's
// lines read, its refusals and warnings shown (annotated in Actions),
// what make secret leaves to it checked, and a motel too old to read
// secrets named.
func TestBoxCheckSecrets(t *testing.T) {
	t.Parallel()
	bin := motelBinary(t)
	r := secretsRepo(t)
	r.key("id_ed25519")
	if out, code := r.run("signer"); code != 0 {
		t.Fatalf("signer: exit %d\n%s", code, out)
	}
	_, box := r.boxKey("box")
	check := func(env ...string) (string, int) {
		return r.run("check", append([]string{"MOTEL=" + bin}, env...)...)
	}
	r.write(boxFile, strings.Replace(r.read(boxFile), "# env_file /etc/motel/secrets/example.env", "env_file /etc/motel/secrets/example.env", 1))
	// A managed env_file and no secret: describe's consistency check.
	if out, code := check(); code != 1 || !strings.Contains(out, "holds no secret") {
		t.Fatalf("a managed env_file with no secret: exit %d\n%s", code, out)
	}
	if out, code := r.secrets("", "postgres://one", "set", "APP=example", "VAR=DATABASE_URL", "BOX_KEY="+box); code != 0 {
		t.Fatalf("make secret: exit %d\n%s", code, out)
	}
	if out, code := check(); code != 0 || out != "deploy.example.com/Caddyfile → https://deploy.example.com/ (1 app, 1 signer, 1 secret)\n" {
		t.Fatalf("one secret: exit %d\n%s", code, out)
	}
	// A monitor block reading the box's own secret, which is not on this
	// machine: validate is told it is the push's.
	withoutNotify := r.read(boxFile)
	r.write(boxFile, strings.Replace(withoutNotify, "{\n", "{\n\tmonitor {\n\t\tchannel ops ntfy {secret.NOTIFY_OPS_URL}\n\t\toperator ops\n\t}\n", 1))
	if out, code := r.secrets("", "https://ntfy.sh/ops-topic", "set", "APP=_box", "VAR=NOTIFY_OPS_URL"); code != 0 {
		t.Fatalf("make secret APP=_box: exit %d\n%s", code, out)
	}
	if out, code := check(); code != 0 || !strings.Contains(out, "(1 app, 1 signer, 2 secrets)") {
		t.Fatalf("a monitor block and its secret in _box/: exit %d\n%s", code, out)
	}
	r.remove("deploy.example.com/secrets/_box")
	r.write(boxFile, withoutNotify)

	// Two names one file on a Mac's disk: the box takes both, so a
	// warning, annotated in Actions.
	r.write("deploy.example.com/secrets/example/database_url.age", r.read(dbFile))
	out, code := check("GITHUB_ACTIONS=true")
	twin := "app example's secrets DATABASE_URL and database_url differ only in case"
	if code != 0 || !strings.Contains(out, "(1 app, 1 signer, 2 secrets)") ||
		!strings.Contains(out, "check: warning: deploy.example.com/Caddyfile: "+twin) ||
		!strings.Contains(out, "::warning file=deploy.example.com/Caddyfile::"+twin) {
		t.Fatalf("a case twin: exit %d\n%s", code, out)
	}
	r.remove("deploy.example.com/secrets/example/database_url.age")

	// Secrets for an app the file does not declare: describe refuses.
	r.write("deploy.example.com/secrets/ghost/X.age", r.read(dbFile))
	if out, code := check(); code != 1 || !strings.Contains(out, "secrets/ghost/ names no app in the new Caddyfile") {
		t.Fatalf("an undeclared app: exit %d\n%s", code, out)
	}
	r.remove("deploy.example.com/secrets/ghost")

	// Values, and no recipients file.
	recipients := r.read(recipientsFile)
	r.remove(recipientsFile)
	if out, code := check(); code != 1 || !strings.Contains(out, "deploy.example.com/secrets holds values but no recipients file") {
		t.Fatalf("no recipients file: exit %d\n%s", code, out)
	}
	r.write(recipientsFile, recipients)

	// A motel from before secrets: its describe reads none of them.
	old := filepath.Join(r.home, "old-motel")
	script := "#!/bin/sh\n" +
		"if [ \"$1 $2\" = \"box describe\" ]; then\n" +
		"\t\"" + bin + "\" box describe \"$3\" | grep -v -E '^(recipients|secret|warning) '\n" +
		"\texit 0\n" +
		"fi\n" +
		"exec \"" + bin + "\" \"$@\"\n"
	if err := os.WriteFile(old, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	tooOld := "deploy.example.com/Caddyfile: deploy.example.com/secrets holds values, and this motel does not read secrets: check with a release that does (deploy.example.com/MOTEL_VERSION would name this box's; without it, the workflow takes the latest stable release, or the one its MOTEL_VERSION variable names)\n"
	if out, code := r.run("check", "MOTEL="+old); code != 1 || !strings.Contains(out, tooOld) {
		t.Fatalf("an old motel: exit %d\n%s", code, out)
	}
	// The same release, picked by the box's own MOTEL_VERSION file:
	// the file is not the remedy, so the refusal says what is.
	hs := filepath.Join(r.home, "bins", "motel")
	if err := os.MkdirAll(filepath.Dir(hs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hs, []byte("#!/bin/sh\nexec \""+bin+"\" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hs+"-0.2.9", []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r.write("deploy.example.com/MOTEL_VERSION", "0.2.9\n")
	pinnedOld := "deploy.example.com/Caddyfile: deploy.example.com/secrets holds values, and " + hs + "-0.2.9, the release deploy.example.com/MOTEL_VERSION names, does not read secrets: upgrade the box to a release that does and name that one in the file, or remove the values\n"
	if out, code := r.run("check", "MOTEL="+hs); code != 1 || !strings.Contains(out, pinnedOld) {
		t.Fatalf("an old motel, pinned by the file: exit %d\n%s", code, out)
	}
}
