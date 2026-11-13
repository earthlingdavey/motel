package box

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/smallhoursorg/motel/box/proof"
)

// `GET /`'s recipient is age.pub's one line: omitted when there is none,
// and when it does not read as one X25519 recipient — journaled, never a
// 500 — and held out of the response filter, which would mask a bech32
// key.
func TestHandlerStatusRecipient(t *testing.T) {
	r := newRig(t)
	r.applied(t, sha)
	key := newIdentity(t).Recipient().String()
	for name, c := range map[string]struct {
		pub  []byte // nil: no file
		want string
		log  bool
	}{
		"no key":             {},
		"the key":            {pub: []byte(key + "\n"), want: key},
		"no newline":         {pub: []byte(key), log: true},
		"two lines":          {pub: []byte(key + "\n" + key + "\n"), log: true},
		"not a recipient":    {pub: []byte("age1nope\n"), log: true},
		"an SSH key instead": {pub: []byte("ssh-ed25519 AAAA\n"), log: true},
	} {
		t.Run(name, func(t *testing.T) {
			_ = os.Remove(r.h.agePub)
			if c.pub != nil {
				r.write(t, r.h.agePub, c.pub)
			}
			before := len(r.logs.FilterMessage("box webhook could not read its state").All())
			w := r.do(t, req{target: "/", token: r.token(t)})
			got := body(t, w)
			if w.Code != http.StatusOK {
				t.Fatalf("%d %v", w.Code, got)
			}
			if rec, ok := got["recipient"]; c.want == "" && ok || c.want != "" && rec != c.want {
				t.Errorf("recipient %v, want %q", got["recipient"], c.want)
			}
			// The body byte for byte, as a client reads it: the keys in
			// their order, the recipient last and only when there is one.
			sum := sha256.Sum256(file(good))
			want := fmt.Sprintf(`{"commit":%q,"box_webhook":"deploy.example.com","sha256":%q`, sha, hex.EncodeToString(sum[:]))
			if c.want != "" {
				want += fmt.Sprintf(`,"recipient":%q`, c.want)
			}
			if want += "}\n"; w.Body.String() != want {
				t.Errorf("body\n got %s\nwant %s", w.Body, want)
			}
			if logged := len(r.logs.FilterMessage("box webhook could not read its state").All()) > before; logged != c.log {
				t.Errorf("journaled %v, want %v", logged, c.log)
			}
		})
	}
}

// readsSecrets is boxFile whose app example reads its secrets.
func readsSecrets(version int, signers ...testKey) []byte {
	return bytes.Replace(boxFile(version, signers...), []byte("\t\t\tcommand node server.js\n"),
		[]byte("\t\t\tcommand node server.js\n\t\t\tenv_file /etc/motel/secrets/example.env\n"), 1)
}

// A push whose secrets and Caddyfile disagree is refused at admission
// (step 6, last), 422, nothing dropped; one that agrees is admitted.
func TestPushSecretsFit(t *testing.T) {
	p := newPushRig(t)
	val := encrypt(t, []byte("v"), newIdentity(t).Recipient())
	c := p.repo.commitSecrets(boxFile(2, p.alice), secrets{"example": {"KEY": val}}, &p.alice, p.base)
	id := randomID(t)
	m := wantResult(t, p.post(t, p.h, push{id: id, token: p.token(t, c), body: bytes.NewReader(tgz(t, p.repo.bundleFiles(c, p.base)))}), http.StatusUnprocessableEntity, id, phaseRefused)
	if m["error"] != "app example has secrets and no env_file: add env_file /etc/motel/secrets/example.env to its block" {
		t.Fatalf("%v", m)
	}
	p.untouched(t)

	c = p.repo.commitSecrets(readsSecrets(2, p.alice), secrets{"example": {"KEY": val}}, &p.alice, p.base)
	id = randomID(t)
	if w := p.post(t, p.h, push{id: id, token: p.token(t, c), body: bytes.NewReader(tgz(t, p.repo.bundleFiles(c, p.base)))}); w.Code != http.StatusGatewayTimeout || !exists(p.x("in/"+id+".tar")) {
		t.Fatalf("a push whose secrets fit: %d %s", w.Code, w.Body)
	}
}

// A push's secrets/_box/ is named to validate, which root has not yet
// written into _box.env: the names in the commit's order, never a value.
func TestPushNamesBoxSecretsToValidate(t *testing.T) {
	p := newPushRig(t)
	enc := func(v string) []byte { return encrypt(t, []byte(v), newIdentity(t).Recipient()) }
	file := bytes.Replace(boxFile(2, p.alice), []byte("\tliveswap {"), []byte("\tmonitor {\n\t\tchannel ops ntfy {secret.NOTIFY_OPS_URL}\n\t\toperator ops\n\t}\n\n\tliveswap {"), 1)
	c := p.repo.commitSecrets(file, secrets{"_box": {"NOTIFY_OPS_URL": enc("https://ntfy.sh/ops"), "NOTIFY_HC_URL": enc("https://hc-ping.com/x")}}, &p.alice, p.base)
	id := randomID(t)
	if w := p.post(t, p.h, push{id: id, token: p.token(t, c), body: bytes.NewReader(tgz(t, p.repo.bundleFiles(c, p.base)))}); w.Code != http.StatusGatewayTimeout || !exists(p.x("in/"+id+".tar")) {
		t.Fatalf("a push of the box's own secrets: %d %s", w.Code, w.Body)
	}
	if !slices.Equal(p.val.boxSecrets, []string{"NOTIFY_HC_URL", "NOTIFY_OPS_URL"}) {
		t.Fatalf("validate was told %q", p.val.boxSecrets)
	}
}

// A monitor block naming a secret the push does not carry is refused
// before validate, which would refuse it too in words that name no
// remedy.
func TestPushRefusesAnUncarriedNotifySecretBeforeValidate(t *testing.T) {
	p := newPushRig(t)
	file := bytes.Replace(boxFile(2, p.alice), []byte("\tliveswap {"), []byte("\tmonitor {\n\t\tchannel ops ntfy {secret.NOTIFY_OPS_URL}\n\t\toperator ops\n\t}\n\n\tliveswap {"), 1)
	c := p.repo.commitSecrets(file, nil, &p.alice, p.base)
	id := randomID(t)
	m := wantResult(t, p.post(t, p.h, push{id: id, token: p.token(t, c), body: bytes.NewReader(tgz(t, p.repo.bundleFiles(c, p.base)))}), http.StatusUnprocessableEntity, id, phaseRefused)
	if !strings.Contains(fmt.Sprint(m["error"]), "the monitor block reads secret NOTIFY_OPS_URL") || p.val.calls != 0 {
		t.Fatalf("%v; validate ran %d times", m, p.val.calls)
	}
	p.untouched(t)
}

// The fast path answers a replay of the commit the box runs only while
// every env file it lists stands at its digest: after the loss of one,
// the push goes to root, which writes it again.
func TestPushFastPathSecrets(t *testing.T) {
	p := newPushRig(t)
	p.secretsDirAs(0o750 | os.ModeSetgid)
	id, _, err := ensureIdentity(p.path(identityFile), p.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	c := p.repo.commitSecrets(readsSecrets(2, p.alice), secrets{"example": {"KEY": encrypt(t, []byte("v"), id.Recipient())}}, &p.alice, p.base)
	if r := p.pushOne(p.repo.bundleFiles(c, p.base)); r.Phase != phaseApplied {
		t.Fatalf("setup: %+v", r)
	}
	replay := tgz(t, p.repo.bundleFiles(c, p.base)) // HEAD's parent is the base: no parents/
	rid := randomID(t)
	wantResult(t, p.post(t, p.h, push{id: rid, token: p.token(t, c), body: bytes.NewReader(replay)}), http.StatusOK, rid, phaseNoChange)

	if err := os.Remove(p.envPath("example")); err != nil {
		t.Fatal(err)
	}
	rid = randomID(t)
	if w := p.post(t, p.h, push{id: rid, token: p.token(t, c), body: bytes.NewReader(replay)}); w.Code != http.StatusGatewayTimeout || !exists(p.x("in/"+rid+".tar")) {
		t.Fatalf("a replay over a lost env file: %d %s", w.Code, w.Body)
	}
	must(t, p.run(hooks{}))
	if r := p.result(rid); r == nil || r.Phase != phaseApplied || p.env("example") == nil {
		t.Fatalf("root did not write the env file again: %+v", r)
	}
}

// A result's secrets' names reach the workflow as root wrote them: a
// variable's name can look as random as a key, and the filter would
// mask it.
func TestResultSecretNamesUnmasked(t *testing.T) {
	p := newPushRig(t)
	name := "example/K8XQ2PL7ZR4MW9TB3NV6YC1HD5JF0GSA"
	id := randomID(t)
	p.writeResult(t, result{ID: id, Phase: phaseApplied, Commit: p.base, SecretsAdded: []string{name}})
	w := p.get(t, "/?result="+id, p.token(t, ""))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), name) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}

	// Added after the filter, as the result's last fields: the answer
	// is the result's own bytes when the filter masks nothing.
	res := result{ID: randomID(t), Phase: phaseApplied, Commit: p.base, SecretsAdded: []string{name}, SecretsChanged: []string{"example/B"}, SecretsRemoved: []string{"api/C"}, SecretsOutOfBand: []string{"api"}}
	p.writeResult(t, res)
	w = p.get(t, "/?result="+res.ID, p.token(t, ""))
	if want, _ := json.Marshal(res); w.Body.String() != string(want)+"\n" {
		t.Fatalf("got %s\nwant %s", w.Body, want)
	}

	// The filter still reads the rest, beside the most names a push
	// can report.
	token := "ghp_" + strings.Repeat("aB3dE5fG7h", 4)[:36]
	res = result{ID: randomID(t), Phase: phaseApplied, Commit: p.base, Diff: "+ " + token + "\n", SecretsOutOfBand: []string{"api"}}
	for i := range maxSecretNames {
		res.SecretsAdded = append(res.SecretsAdded, "example/K"+itoa(i))
	}
	p.writeResult(t, res)
	w = p.get(t, "/?result="+res.ID, p.token(t, ""))
	var got result
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK || strings.Contains(w.Body.String(), token) || !slices.Equal(got.SecretsAdded, res.SecretsAdded) || !slices.Equal(got.SecretsOutOfBand, res.SecretsOutOfBand) {
		t.Fatalf("%d %v: %.300s", w.Code, err, w.Body)
	}
}

// writeTree makes a box directory in a working tree: its Caddyfile and
// the given files under secrets/ (a nil value is a directory).
func writeTree(t *testing.T, caddyfile []byte, files map[string][]byte) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "deploy.example.com")
	if err := os.MkdirAll(filepath.Join(dir, "secrets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Caddyfile"), caddyfile, 0o644); err != nil {
		t.Fatal(err)
	}
	for name, data := range files {
		p := filepath.Join(dir, "secrets", name)
		if data == nil {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "Caddyfile")
}

// describe reads the working tree's secrets as root reads the commit's:
// each value's length from its ciphertext, the recipients counted, the
// layout and the consistency checks refused, the caps past 75% warned.
func TestDescribeSecrets(t *testing.T) {
	box := newIdentity(t)
	reader := newIdentity(t)
	enc := func(v string) []byte { return encrypt(t, []byte(v), box.Recipient(), reader.Recipient()) }
	reads := secretsFile("\t\t\tenv_file /etc/motel/secrets/example.env\n", "")
	recipients := []byte("# the box\n" + box.Recipient().String() + "\n# alice\n" + reader.Recipient().String() + "\n")

	path := writeTree(t, reads, map[string][]byte{
		"recipients":               recipients,
		"example/DATABASE_URL.age": enc("postgres://db"),
		"example/EMPTY.age":        enc(""),
		"example/BIG.age":          enc(strings.Repeat("x", 40000)),
		"example/NL.age":           enc("value\n"),
	})
	var out bytes.Buffer
	if err := describe(path, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"recipients 2\n",
		"secret example BIG 40000\n",
		"secret example DATABASE_URL 13\n",
		"secret example EMPTY 0\n",
		"secret example NL 6\n", // the line break 15a strips is still counted: "to within"
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q in:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "warning") {
		t.Errorf("a warning under every cap:\n%s", out.String())
	}

	for name, c := range map[string]struct {
		file  []byte
		files map[string][]byte
		want  string
	}{
		"a directory for no app":           {reads, map[string][]byte{"other/KEY.age": enc("v")}, "secrets/other/ names no app in the new Caddyfile"},
		"secrets the block does not read":  {secretsFile("", ""), map[string][]byte{"example/KEY.age": enc("v")}, "app example has secrets and no env_file"},
		"a file that is not a secret":      {reads, map[string][]byte{"example/KEY": []byte("plain")}, "example/KEY is not a secret: only <VAR>.age files are"},
		"a variable name env cannot carry": {reads, map[string][]byte{"example/1KEY.age": enc("v")}, "1KEY is not a variable name an env file can carry"},
		"an app name out of grammar":       {reads, map[string][]byte{"Example/KEY.age": enc("v")}, "secrets/Example is not an app's directory"},
		"a stray file":                     {reads, map[string][]byte{"notes.txt": []byte("x")}, "secrets/notes.txt is not an app's directory: it is a file"},
		"an armored value":                 {reads, map[string][]byte{"example/KEY.age": []byte("-----BEGIN AGE ENCRYPTED FILE-----\nx\n")}, "is ASCII-armored"},
		"a plaintext value":                {reads, map[string][]byte{"example/KEY.age": []byte("hunter2\n")}, "does not decrypt: it is not an age file"},
		"recipients without the box first": {reads, map[string][]byte{"recipients": []byte("ssh-ed25519 AAAA\n"), "example/KEY.age": enc("v")}, "recipients:1: the first recipient is not the box's key"},
		"a recipient age cannot read":      {reads, map[string][]byte{"recipients": []byte(box.Recipient().String() + "\nssh-ed25519 AAAA\n"), "example/KEY.age": enc("v")}, "recipients:2: not a recipient age encrypts to"},
		"a mistyped reader's key":          {reads, map[string][]byte{"recipients": []byte(box.Recipient().String() + "\n" + mistyped(reader.Recipient().String()) + "\n"), "example/KEY.age": enc("v")}, "recipients:2: not a recipient age encrypts to"},
		"a trailing space":                 {reads, map[string][]byte{"recipients": []byte(box.Recipient().String() + " \n"), "example/KEY.age": enc("v")}, "recipients:1: a space or tab at the line's start or end"},
		"a recipients file with none":      {reads, map[string][]byte{"recipients": []byte("# the box\n"), "example/KEY.age": enc("v")}, "secrets/recipients has no recipient"},
		// age's own bounds on the file and its lines, and its refusal of
		// a private key among the recipients
		"an identity among them":        {reads, map[string][]byte{"recipients": []byte(box.Recipient().String() + "\n" + reader.String() + "\n"), "example/KEY.age": enc("v")}, "recipients:2: an age identity, a private key"},
		"a recipient's line over 8 KiB": {reads, map[string][]byte{"recipients": []byte(box.Recipient().String() + "\nssh-ed25519 " + strings.Repeat("A", 9000) + "\n"), "example/KEY.age": enc("v")}, "recipients:2: over 8192 bytes"},
		"a line over 64 KiB":            {reads, map[string][]byte{"recipients": []byte(box.Recipient().String() + "\n#" + strings.Repeat("x", 70000) + "\n"), "example/KEY.age": enc("v")}, "recipients:2: over 65536 bytes"},
		"a line not UTF-8":              {reads, map[string][]byte{"recipients": []byte(box.Recipient().String() + "\n\xff\n"), "example/KEY.age": enc("v")}, "recipients:2: not UTF-8"},
		"a file over 16 MiB":            {reads, map[string][]byte{"recipients": []byte(box.Recipient().String() + "\n" + strings.Repeat("#\n", 8<<20)), "example/KEY.age": enc("v")}, "secrets/recipients is over 16777216 bytes"},
	} {
		t.Run(name, func(t *testing.T) {
			path := writeTree(t, c.file, c.files)
			var out bytes.Buffer
			err := describe(path, &out)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%v, want it to contain %q", err, c.want)
			}
			if out.Len() != 0 {
				t.Errorf("a refusal printed %q", out.String())
			}
		})
	}

	// Past the env file's cap is said plainly, still as a warning.
	path = writeTree(t, reads, map[string][]byte{"example/BIG.age": enc(strings.Repeat("x", 70000))})
	out.Reset()
	if err := describe(path, &out); err != nil || !strings.Contains(out.String(), "warning app example's env file would be about 70007 bytes, over the cap of 65536: the box will refuse the push\n") {
		t.Errorf("over the cap: %v\n%s", err, out.String())
	}

	// A plugin's recipient is taken as it is: its checksum is the
	// plugin's to check.
	path = writeTree(t, reads, map[string][]byte{
		"recipients":      []byte(box.Recipient().String() + "\nage1yubikey1qwt50d05nh5vutpdzmlg5wn80xq5negm4uj9ghv0snvdd3yysf5yw3rhl3t\n"),
		"example/KEY.age": enc("v"),
	})
	if err := describe(path, &bytes.Buffer{}); err != nil {
		t.Errorf("a plugin's recipient: %v", err)
	}

	// As age reads a recipients file: a CRLF line is a line, and an SSH
	// key of a type age does not encrypt to is skipped with a warning.
	sk := "sk-ssh-ed25519@openssh.com AAAAGnNrLXNzaC1lZDI1NTE5QG9wZW5zc2guY29tAAAAIGYd6dqGEgHa0E1ijwY+XaYHV1Ts9ux0iEsNx8K1DtwPAAAABHNzaDo= alice"
	path = writeTree(t, reads, map[string][]byte{
		"recipients":      []byte(box.Recipient().String() + "\r\n" + sk + "\r\n"),
		"example/KEY.age": enc("v"),
	})
	out.Reset()
	if err := describe(path, &out); err != nil || !strings.Contains(out.String(), "recipients 1\n") || !strings.Contains(out.String(), "recipients:2: age skips this sk-ssh-ed25519@openssh.com key") {
		t.Errorf("CRLF and an sk- key: %v\n%s", err, out.String())
	}

	// A dot-name, which git usually ignores, is warned about and not read.
	path = writeTree(t, reads, map[string][]byte{".DS_Store": []byte("x"), "example/.KEY.age.swp": []byte("x"), "example/KEY.age": enc("v")})
	out.Reset()
	if err := describe(path, &out); err != nil || strings.Count(out.String(), "is not a secret, and is not read here; the box refuses the push if it is committed\n") != 2 {
		t.Errorf("dot-names: %v\n%s", err, out.String())
	}

	// A directory of dot-names alone is no commit's either, whatever its
	// name: its dot-names are warned about, and it is not refused.
	path = writeTree(t, secretsFile("", ""), map[string][]byte{"Example/.DS_Store": []byte("x")})
	out.Reset()
	if err := describe(path, &out); err != nil || !strings.Contains(out.String(), "secrets/Example/.DS_Store is not a secret, and is not read here") {
		t.Errorf("a dot-only directory: %v\n%s", err, out.String())
	}

	// A directory with no secret is not a commit's: git keeps none.
	path = writeTree(t, secretsFile("", ""), map[string][]byte{"example": nil})
	if err := describe(path, &bytes.Buffer{}); err != nil {
		t.Errorf("an empty app directory: %v", err)
	}
}

// describe's first refusal is the one the commit's proof names: a bad
// entry ahead of the cap is named before the cap; and an env file just
// over the cap by a line break the box strips is only "at or just over".
func TestDescribeSecretsFirstRefusal(t *testing.T) {
	id := newIdentity(t)
	files := map[string][]byte{"Bad/KEY.age": []byte("x")} // "B" sorts before every app
	for i := 0; i < proof.MaxSecretApps; i++ {
		files["app"+itoa(i)+"/KEY.age"] = []byte("x")
	}
	err := describe(writeTree(t, secretsFile("", ""), files), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "secrets/Bad is not an app's directory: Bad is not an app name") {
		t.Errorf("%v", err)
	}

	// Past the 256th secret a directory is still refused as one, as the
	// proof checks every entry's type before the cap.
	files = map[string][]byte{"example/zz/KEY.age": []byte("x")} // a directory, last in git's order
	for i := 0; i < proof.MaxSecretVars; i++ {
		files["example/K"+itoa(i)+".age"] = []byte("x")
	}
	err = describe(writeTree(t, secretsFile("\t\t\tenv_file /etc/motel/secrets/example.env\n", ""), files), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "secrets/example/zz is not a secret: it is a directory") {
		t.Errorf("%v", err)
	}

	// 65,529 bytes and a LF: describe counts KEY="…"\n as 3+65530+4 =
	// 65,537, but once 15a strips the LF it renders 65,536 bytes, at the
	// cap, which the box accepts.
	value := append(bytes.Repeat([]byte("x"), 65529), '\n')
	path := writeTree(t, secretsFile("\t\t\tenv_file /etc/motel/secrets/example.env\n", ""), map[string][]byte{"example/KEY.age": encrypt(t, value, id.Recipient())})
	var out bytes.Buffer
	if err := describe(path, &out); err != nil || !strings.Contains(out.String(), "at or just over the cap") || strings.Contains(out.String(), "will refuse") {
		t.Errorf("%v\n%s", err, out.String())
	}
}

// mistyped is an age recipient with one data character changed, so its
// checksum fails.
func mistyped(r string) string {
	c := byte('q')
	if r[10] == 'q' {
		c = 'p'
	}
	return r[:10] + string(c) + r[11:]
}

// describe lists the working tree as a commit of it would: in git's
// order, where a directory sorts as if its name ended in `/` (so api-v2
// comes before api) and a submodule's checkout does not; a directory of
// dot-names alone, or of nothing, left out, its dot-names returned.
func TestGitTree(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"api", "api-v2", "sub", "sub-a", "empty", "dots"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"api/K.age", "api-v2/K.age", "sub/.git", "sub-a/K.age", "dots/.DS_Store", "recipients", ".swp"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("api", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	entries, dots, err := gitTree(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.name+":"+e.mode)
	}
	if want := "api-v2:40000 api:40000 link:120000 recipients:100644 sub:160000 sub-a:40000"; strings.Join(got, " ") != want {
		t.Errorf("%v, want %s", got, want)
	}
	if want := ".swp dots/.DS_Store"; strings.Join(dots, " ") != want {
		t.Errorf("dot-names %v, want %s", dots, want)
	}
}

// node is one entry of a box directory in a test: a file, an
// executable one, a symlink, or a submodule's checkout (a directory
// holding .git); a directory is implied by the paths below it.
type node struct {
	kind byte   // 'f', 'x', 'l' or 's'
	data []byte // a file's bytes, a link's target
}

// writeNodes makes nodes, paths from top, in a working tree.
func writeNodes(t *testing.T, top string, nodes map[string]node) {
	t.Helper()
	for p, n := range nodes {
		full := filepath.Join(top, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		var err error
		switch n.kind {
		case 'f':
			err = os.WriteFile(full, n.data, 0o644)
		case 'x':
			err = os.WriteFile(full, n.data, 0o755)
		case 'l':
			err = os.Symlink(string(n.data), full)
		case 's':
			if err = os.Mkdir(full, 0o755); err == nil {
				err = os.WriteFile(filepath.Join(full, ".git"), []byte("gitdir: ../.git/modules/x\n"), 0o644)
			}
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

// treeOfNodes is the tree git commits for nodes, with each tree under
// it and each file's blob added to trees and blobs: written here from
// git's rules, not from describe's, and held to them by
// proof.ParseTree, which refuses a tree out of git's order.
func treeOfNodes(nodes map[string]node, trees map[string][]byte, blobs map[string][]byte) []byte {
	type entry struct{ mode, name, id string }
	var entries []entry
	below := map[string]map[string]node{}
	for p, n := range nodes {
		if first, rest, nested := strings.Cut(p, "/"); nested {
			if below[first] == nil {
				below[first] = map[string]node{}
			}
			below[first][rest] = n
			continue
		}
		switch n.kind {
		case 'f', 'x':
			id := proof.ObjectID("blob", n.data)
			blobs[id] = n.data
			entries = append(entries, entry{map[byte]string{'f': "100644", 'x': "100755"}[n.kind], p, id})
		case 'l':
			entries = append(entries, entry{"120000", p, proof.ObjectID("blob", n.data)})
		case 's':
			entries = append(entries, entry{"160000", p, proof.ObjectID("commit", n.data)})
		}
	}
	for name, inner := range below {
		raw := treeOfNodes(inner, trees, blobs)
		id := proof.ObjectID("tree", raw)
		trees[id] = raw
		entries = append(entries, entry{"40000", name, id})
	}
	key := func(e entry) string {
		if e.mode == "40000" {
			return e.name + "/"
		}
		return e.name
	}
	slices.SortFunc(entries, func(a, b entry) int { return strings.Compare(key(a), key(b)) })
	var raw []byte
	for _, e := range entries {
		raw = append(raw, treeObject(e.mode, e.name, e.id)...)
	}
	return raw
}

// describe and root's proof hold the secrets to one layout: for a
// working tree and the commit of it, each case of the layout table
// ("Layout in the repository"), and the order in which a tree with
// more than one fault is refused, describe's first refusal is the
// proof's, less its " in <sha>"; and where the proof admits the tree,
// describe reads the same apps and variables. (Dot-names, which
// describe leaves out with a warning, are outside it: git commits them
// unless they are ignored.)
func TestDescribeRefusesAsTheProof(t *testing.T) {
	f := func(s string) node { return node{'f', []byte(s)} }
	link := node{'l', []byte("elsewhere")}
	sub := node{'s', []byte("a submodule")}
	value := f("age-encryption.org/v1\n")
	many := func(n int, path func(i int) string) map[string]node {
		nodes := map[string]node{}
		for i := range n {
			nodes[path(i)] = value
		}
		return nodes
	}
	with := func(nodes map[string]node, more map[string]node) map[string]node {
		maps.Copy(nodes, more)
		return nodes
	}
	cases := map[string]struct {
		nodes  map[string]node
		admits bool
	}{
		"apps, values and recipients":  {map[string]node{"secrets/recipients": f("age1…\n"), "secrets/example/DATABASE_URL.age": value, "secrets/example/STRIPE_KEY.age": value, "secrets/api/K.age": value}, true},
		"recipients alone":             {map[string]node{"secrets/recipients": f("age1…\n")}, true},
		"an executable value":          {map[string]node{"secrets/example/K.age": {'x', value.data}}, true},
		"secrets as a file":            {map[string]node{"secrets": f("x")}, false},
		"secrets as a symlink":         {map[string]node{"secrets": link}, false},
		"secrets as a submodule":       {map[string]node{"secrets": sub}, false},
		"a file in secrets/":           {map[string]node{"secrets/README": f("x")}, false},
		"a symlink in secrets/":        {map[string]node{"secrets/example": link}, false},
		"a submodule in secrets/":      {map[string]node{"secrets/example": sub}, false},
		"recipients as a directory":    {map[string]node{"secrets/recipients/K.age": value}, false},
		"recipients as a symlink":      {map[string]node{"secrets/recipients": link}, false},
		"an app name out of grammar":   {map[string]node{"secrets/Example/K.age": value}, false},
		"_backup, held for later":      {map[string]node{"secrets/_backup/K.age": value}, false},
		"a directory in an app's":      {map[string]node{"secrets/example/deeper/K.age": value}, false},
		"a symlinked value":            {map[string]node{"secrets/example/K.age": link}, false},
		"a submodule as a value":       {map[string]node{"secrets/example/K.age": sub}, false},
		"a value without .age":         {map[string]node{"secrets/example/DATABASE_URL": value}, false},
		"a variable name from a digit": {map[string]node{"secrets/example/1X.age": value}, false},
		"a variable name with a dash":  {map[string]node{"secrets/example/my-var.age": value}, false},
		"a variable name with a space": {map[string]node{"secrets/example/A B.age": value}, false},
		"a variable name over its cap": {map[string]node{"secrets/example/" + strings.Repeat("A", proof.MaxVarName+1) + ".age": value}, false},
		"a value over its cap":         {map[string]node{"secrets/example/K.age": f(strings.Repeat("x", proof.MaxSecretFile+1))}, false},
		// past a cap an entry is only counted, its name never read
		"apps past the cap":   {with(many(proof.MaxSecretApps+1, func(i int) string { return "secrets/app" + itoa(i) + "/K.age" }), map[string]node{"secrets/zBad/K.age": value}), false},
		"values past the cap": {with(many(proof.MaxSecretVars+1, func(i int) string { return "secrets/example/K" + itoa(i) + ".age" }), map[string]node{"secrets/example/zz": value}), false},
		// but its type is
		"a directory past the cap": {with(many(proof.MaxSecretVars+1, func(i int) string { return "secrets/example/K" + itoa(i) + ".age" }), map[string]node{"secrets/example/zz/K.age": value}), false},
		// the order of faults: git's order, every app's name before any
		// app's values, and one app's values and sizes before the next's
		"git's order of trees":               {map[string]node{"secrets/api/1BAD.age": value, "secrets/api-v2/2BAD.age": value}, false},
		"secrets/ before an app's directory": {map[string]node{"secrets/aa/1BAD.age": value, "secrets/zz-file": f("x")}, false},
		"an app's size before the next app":  {map[string]node{"secrets/aa/K.age": f(strings.Repeat("x", proof.MaxSecretFile+1)), "secrets/bb/1BAD.age": value}, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			caddyfile := []byte("# the file\n")
			// The commit, as git builds it.
			trees, blobs := map[string][]byte{}, map[string][]byte{}
			top := treeOfNodes(with(maps.Clone(c.nodes), map[string]node{"Caddyfile": {'f', caddyfile}}), trees, blobs)
			r := newRepo(t)
			head := r.commitSub(top, caddyfile, nil)
			commit, err := proof.ParseCommit(r.objects[head])
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range r.trees[head] {
				trees[id] = r.objects[id]
			}
			parsed := map[string]*proof.Tree{}
			for id, raw := range trees {
				tr, err := proof.ParseTree(raw)
				if err != nil {
					t.Fatal(err)
				}
				parsed[id] = tr
			}
			proved, perr := proof.ProveSecrets(commit, parsed, blobs, testPath, secretNames)

			// The working tree.
			work := t.TempDir()
			writeNodes(t, filepath.Join(work, "box1"), with(maps.Clone(c.nodes), map[string]node{"Caddyfile": {'f', caddyfile}}))
			t.Chdir(work)
			read, derr := readTreeSecrets(testPath)

			var want, got string
			if perr != nil {
				var r *proof.Refusal
				if !errors.As(perr, &r) || strings.HasPrefix(r.Msg, "bundle:") {
					t.Fatalf("the proof: %v", perr)
				}
				want = strings.ReplaceAll(r.Msg, " in "+head, "")
			}
			if derr != nil {
				got = derr.Error()
			}
			switch {
			case (perr == nil) != c.admits:
				t.Fatalf("the proof: %v", perr)
			case got != want:
				t.Fatalf("describe: %q\nthe proof: %q", got, want)
			case perr == nil && !sameNames(proved, read.secrets):
				t.Fatalf("describe read %+v, the proof %+v", read.secrets, proved)
			}
		})
	}
}

// sameNames reports whether a and b name the same apps and variables, in
// the same order.
func sameNames(a, b *proof.Secrets) bool {
	return slices.EqualFunc(a.Apps, b.Apps, func(x, y proof.SecretApp) bool {
		return x.Name == y.Name && slices.EqualFunc(x.Vars, y.Vars, func(v, w proof.SecretVar) bool { return v.Name == w.Name })
	})
}

// The caps past 75% are warnings in describe's output, before a push
// meets them.
func TestDescribeSecretsWarns(t *testing.T) {
	id := newIdentity(t)
	files := map[string][]byte{}
	one := encrypt(t, []byte("v"), id.Recipient())
	for i := 0; i < warnSecretVars; i++ {
		files["example/K"+strings.Repeat("A", i%5)+itoa(i)+".age"] = one
	}
	files["example/BIGGEST.age"] = encrypt(t, bytes.Repeat([]byte("x"), warnEnvFile), id.Recipient())
	path := writeTree(t, secretsFile("\t\t\tenv_file /etc/motel/secrets/example.env\n", ""), files)
	var out bytes.Buffer
	if err := describe(path, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"warning app example has 193 secrets; the cap is 256\n",
		"warning app example's env file would be about ",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q in:\n%s", want, out.String())
		}
	}
}

// A NOTIFY_ secret in an app's slot is withheld from the app at its
// launch: make check says so, a warning and not a refusal.
func TestDescribeWarnsOfAWithheldNotifySecret(t *testing.T) {
	id := newIdentity(t)
	path := writeTree(t, secretsFile("\t\t\tenv_file /etc/motel/secrets/example.env\n", ""), map[string][]byte{
		"example/NOTIFY_FROM.age": encrypt(t, []byte("ops@example.com"), id.Recipient()),
	})
	var out bytes.Buffer
	if err := describe(path, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "warning app example's secret NOTIFY_FROM is withheld from the app: a NOTIFY_ variable in an env file the box writes is notify's (a channel's URL goes in ") ||
		!strings.Contains(out.String(), "deploy.example.com/secrets/_box/); name it otherwise if the app reads it\n") {
		t.Errorf("describe:\n%s", out.String())
	}
}

func itoa(i int) string {
	const digits = "0123456789"
	if i < 10 {
		return digits[i : i+1]
	}
	return itoa(i/10) + digits[i%10:i%10+1]
}

// valueLength is the plaintext's length from an age file's sizes alone,
// for every chunk count.
func TestValueLength(t *testing.T) {
	id := newIdentity(t)
	for _, n := range []int{0, 1, 64<<10 - 1, 64 << 10, 64<<10 + 1, 2 * 64 << 10, 3*64<<10 + 7} {
		data := encrypt(t, bytes.Repeat([]byte("a"), n), id.Recipient())
		got, err := valueLength(data)
		if err != nil || got != int64(n) {
			t.Errorf("%d bytes: got %d, %v", n, got, err)
		}
	}
	var two age.Recipient = newIdentity(t).Recipient()
	if got, err := valueLength(encrypt(t, []byte("abc"), id.Recipient(), two)); err != nil || got != 3 {
		t.Errorf("two recipients: %d %v", got, err)
	}
	// A file cut inside its last frame — 1 to 16 bytes past a full one,
	// so no room for a tag and a byte — is refused, as root's decrypt
	// would be; one cut at a frame's end reads as the shorter value it
	// then is (which root's decrypt also refuses: the last chunk is not
	// marked final — describe reads sizes, not frames).
	whole := encrypt(t, bytes.Repeat([]byte("a"), 70000), id.Recipient())
	header, err := age.ExtractHeader(bytes.NewReader(whole))
	if err != nil {
		t.Fatal(err)
	}
	afterFull := len(header) + 16 + 64<<10 + 16
	for cut := 1; cut <= 16; cut++ {
		if _, err := valueLength(whole[:afterFull+cut]); err == nil {
			t.Errorf("cut %d bytes into the last frame: accepted", cut)
		}
	}
	if _, err := valueLength(whole[:len(header)+15]); err == nil {
		t.Error("a payload shorter than a nonce and a tag: accepted")
	}
}

// describe refuses a count past its cap before it reads a secret's file:
// the commit's proof refuses as early.
func TestDescribeSecretsCapsBeforeReading(t *testing.T) {
	files := map[string][]byte{}
	for i := 0; i <= proof.MaxSecretVars; i++ {
		files["example/K"+itoa(i)+".age"] = []byte("not read") // a read would refuse it as not an age file
	}
	path := writeTree(t, secretsFile("\t\t\tenv_file /etc/motel/secrets/example.env\n", ""), files)
	if err := describe(path, &bytes.Buffer{}); err == nil || err.Error() != "app example has 257 secrets; the cap is 256" {
		t.Errorf("%v", err)
	}
	files = map[string][]byte{}
	for i := 0; i <= proof.MaxSecretApps; i++ {
		files["app"+itoa(i)+"/KEY.age"] = []byte("not read")
	}
	if err := describe(writeTree(t, secretsFile("", ""), files), &bytes.Buffer{}); err == nil || err.Error() != "33 apps have secrets; the cap is 32" {
		t.Errorf("%v", err)
	}
}
