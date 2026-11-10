package box

import (
	"bytes"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
	"filippo.io/age/armor"

	"github.com/smallhoursorg/motel/box/proof"
	"github.com/smallhoursorg/motel/liveswap"
)

// Identities are generated per test, never committed: gitleaks keeps an
// AGE-SECRET-KEY out of the tree.

func encrypt(t *testing.T, plain []byte, to ...age.Recipient) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := age.Encrypt(&out, to...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func newIdentity(t *testing.T) *age.X25519Identity {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestEnsureIdentity(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "identity")
	now := time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC)
	id, created, err := ensureIdentity(file, now)
	if err != nil || !created {
		t.Fatal(created, err)
	}
	fi, err := os.Lstat(file)
	if err != nil || fi.Mode().Perm() != 0o600 || !fi.Mode().IsRegular() {
		t.Fatalf("%v %v", fi.Mode(), err)
	}
	data, _ := os.ReadFile(file)
	if !strings.HasPrefix(string(data), "# created: 2026-10-12T20:00:00Z\n# public key: "+id.Recipient().String()+"\nAGE-SECRET-KEY-1") {
		t.Fatalf("not age-keygen's format: the key's comment lines are %q", strings.SplitN(string(data), "\n", 3)[:2])
	}
	// age-keygen's own reading of it: the same key.
	again, created, err := ensureIdentity(file, now.Add(time.Hour))
	if err != nil || created || again.String() != id.String() {
		t.Fatal("a second call replaced or misread the key", created, err)
	}
	// No temporary is left beside it.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("left %d entries", len(entries))
	}

	t.Run("a key that does not read is refused, never replaced", func(t *testing.T) {
		for name, body := range map[string]string{
			"not a key": "junk\n",
			"two keys":  newIdentity(t).String() + "\n" + newIdentity(t).String() + "\n",
			"empty":     "",
			"post-quantum": func() string {
				pq, err := age.GenerateHybridIdentity()
				if err != nil {
					t.Fatal(err)
				}
				return pq.String() + "\n"
			}(),
		} {
			file := filepath.Join(t.TempDir(), "identity")
			if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, _, err := ensureIdentity(file, now)
			if err == nil {
				t.Fatalf("%s: accepted", name)
			}
			if strings.Contains(err.Error(), "AGE-SECRET-KEY") {
				t.Fatalf("%s: the error quotes the key: %v", name, err)
			}
			if got, _ := os.ReadFile(file); string(got) != body {
				t.Fatalf("%s: replaced", name)
			}
		}
	})
	t.Run("a symlink is not followed", func(t *testing.T) {
		d := t.TempDir()
		real := filepath.Join(d, "real")
		if _, _, err := ensureIdentity(real, now); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(d, "identity")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		if _, _, err := ensureIdentity(link, now); err == nil {
			t.Fatal("read through a symlink")
		}
	})
	t.Run("a crashed run's temporaries are removed", func(t *testing.T) {
		// One stands for a crash before the link (a key nobody uses), one
		// for a crash after it (a second name for the box's key).
		d := t.TempDir()
		file := filepath.Join(d, "identity")
		if err := os.WriteFile(filepath.Join(d, ".identity-1"), []byte(newIdentity(t).String()+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		id, _, err := ensureIdentity(file, now)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Link(file, filepath.Join(d, ".identity-2")); err != nil {
			t.Fatal(err)
		}
		again, created, err := ensureIdentity(file, now)
		if err != nil || created || again.String() != id.String() {
			t.Fatal(created, err)
		}
		if left, _ := filepath.Glob(filepath.Join(d, ".identity-*")); len(left) != 0 {
			t.Fatalf("left %q", left)
		}
	})
	t.Run("the message for a key that does not read is the catalogue's", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "identity")
		if err := os.WriteFile(file, []byte("junk\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, err := ensureIdentity(file, now)
		if err == nil || err.Error() != file+" is not an age identity: move it aside, run motel box key again, then make rekey BOX_KEY=<the new key> in the repository" {
			t.Fatal(err)
		}
	})
	t.Run("no directory is no key, not a made one", func(t *testing.T) {
		_, _, err := ensureIdentity(filepath.Join(t.TempDir(), "absent", "identity"), now)
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
	})
	t.Run("racing calls agree on one key", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "identity")
		var wg sync.WaitGroup
		got := make([]string, 8)
		for i := range got {
			wg.Add(1)
			go func() {
				defer wg.Done()
				id, _, err := ensureIdentity(file, now)
				if err != nil {
					t.Error(err)
					return
				}
				got[i] = id.String()
			}()
		}
		wg.Wait()
		for _, k := range got {
			if k != got[0] {
				t.Fatal("racing calls made different keys")
			}
		}
	})
}

func TestOpenSecrets(t *testing.T) {
	box, other := newIdentity(t), newIdentity(t)
	sec := func(vars map[string][]byte) *proof.Secrets {
		var vs []proof.SecretVar
		for _, name := range slices.Sorted(maps.Keys(vars)) {
			vs = append(vs, proof.SecretVar{Name: name, Data: vars[name]})
		}
		return &proof.Secrets{Tree: "t", Apps: []proof.SecretApp{{Name: "example", Vars: vs}}}
	}
	t.Run("values, a trailing newline forgiven, rendered and read back", func(t *testing.T) {
		got, err := openSecrets(box, sec(map[string][]byte{
			"DATABASE_URL": encrypt(t, []byte("postgres://u:p@db/x\n"), box.Recipient(), other.Recipient()),
			"QUOTED":       encrypt(t, []byte(`"keep" the quotes `), box.Recipient()),
			"CRLF":         encrypt(t, []byte("windows\r\n"), box.Recipient()),
			"EMPTY":        encrypt(t, nil, box.Recipient()),
		}), "deploy.example.com/Caddyfile")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].name != "example" || !slices.Equal(got[0].vars, []string{"CRLF", "DATABASE_URL", "EMPTY", "QUOTED"}) {
			t.Fatalf("%+v", got)
		}
		vars, err := liveswap.ParseEnvLines("example.env", got[0].file)
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"CRLF=windows", "DATABASE_URL=postgres://u:p@db/x", "EMPTY=", `QUOTED="keep" the quotes `}; !slices.Equal(vars, want) {
			t.Fatalf("read back %q, want %q", vars, want)
		}
		// The same values under new ciphertext render the same bytes:
		// a rekey changes nothing on the box.
		again, err := openSecrets(box, sec(map[string][]byte{
			"DATABASE_URL": encrypt(t, []byte("postgres://u:p@db/x"), box.Recipient()),
			"QUOTED":       encrypt(t, []byte(`"keep" the quotes `), box.Recipient()),
			"CRLF":         encrypt(t, []byte("windows"), box.Recipient()),
			"EMPTY":        encrypt(t, []byte("\n"), box.Recipient()),
		}), "deploy.example.com/Caddyfile")
		if err != nil || !bytes.Equal(again[0].file, got[0].file) {
			t.Fatalf("rekeyed values rendered differently: %v", err)
		}
	})
	for name, c := range map[string]struct {
		data []byte
		want string
	}{
		"not to the box": {encrypt(t, []byte("v"), other.Recipient()), "deploy.example.com/secrets/example/K.age is not encrypted to this box's key (" + box.Recipient().String() + "): put that key first in deploy.example.com/secrets/recipients and run make rekey"},
		"a passphrase file": {func() []byte {
			r, err := age.NewScryptRecipient("pass")
			if err != nil {
				t.Fatal(err)
			}
			r.SetWorkFactor(10)
			return encrypt(t, []byte("v"), r)
		}(), "is not encrypted to this box's key"},
		"not an age file": {[]byte("DATABASE_URL=plaintext\n"), "deploy.example.com/secrets/example/K.age does not decrypt: it is not an age file"},
		"two lines":       {encrypt(t, []byte("a\nb"), box.Recipient()), "example/K: the value has a line break"},
		"two newlines":    {encrypt(t, []byte("a\n\n"), box.Recipient()), "example/K: the value has a line break"},
		"a lone CR":       {encrypt(t, []byte("a\rb"), box.Recipient()), "example/K: the value has a line break"},
		"a NUL":           {encrypt(t, []byte("a\x00b"), box.Recipient()), "example/K: the value has a NUL byte"},
		"not UTF-8":       {encrypt(t, []byte("\xff\xfe"), box.Recipient()), "example/K: the value is not UTF-8"},
		"tampered": {func() []byte {
			d := encrypt(t, []byte("value"), box.Recipient())
			d[len(d)-1] ^= 1
			return d
		}(), "deploy.example.com/secrets/example/K.age does not decrypt: its payload was changed or cut short after it was encrypted"},
		"a changed header": {func() []byte {
			d := encrypt(t, []byte("value"), box.Recipient())
			i := bytes.Index(d, []byte("\n--- ")) + len("\n--- ")
			d[i] ^= 1 // the header's MAC
			return d
		}(), "deploy.example.com/secrets/example/K.age does not decrypt: its header is not one age reads, or was changed after it was encrypted"},
		"armored": {func() []byte {
			var out bytes.Buffer
			aw := armor.NewWriter(&out)
			w, err := age.Encrypt(aw, box.Recipient())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write([]byte("v")); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			if err := aw.Close(); err != nil {
				t.Fatal(err)
			}
			return out.Bytes()
		}(), "deploy.example.com/secrets/example/K.age does not decrypt: it is ASCII-armored (age -a); the box reads age's binary form"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := openSecrets(box, sec(map[string][]byte{"K": c.data}), "deploy.example.com/Caddyfile")
			var r *refusal
			if !errors.As(err, &r) || !strings.Contains(r.msg, c.want) {
				t.Fatalf("got %v, want a refusal containing %q", err, c.want)
			}
			if strings.Contains(err.Error(), "plaintext") {
				t.Fatal("the refusal quotes the file's bytes")
			}
		})
	}
	t.Run("the env file cap", func(t *testing.T) {
		big := strings.Repeat("x", 40<<10)
		_, err := openSecrets(box, sec(map[string][]byte{
			"A": encrypt(t, []byte(big), box.Recipient()),
			"B": encrypt(t, []byte(big), box.Recipient()),
		}), "deploy.example.com/Caddyfile")
		var r *refusal
		if !errors.As(err, &r) || r.msg != "app example's env file would be 81930 bytes; the cap is 65536" {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a Caddyfile at the top of the repository", func(t *testing.T) {
		_, err := openSecrets(box, sec(map[string][]byte{"K": encrypt(t, []byte("v"), other.Recipient())}), "Caddyfile")
		var r *refusal
		if !errors.As(err, &r) || !strings.HasPrefix(r.msg, "secrets/example/K.age is not encrypted") {
			t.Fatalf("got %v", err)
		}
	})
}

// commitSecrets is repo.commit with a secrets directory beside the
// Caddyfile: a recipients file, and per app its <VAR>.age blobs. The
// trees and blobs ride in the commit's bundle, except recipients' blob,
// which the box never reads; drop names bundle files to leave out.
func (r *repo) commitSecrets(file []byte, apps map[string]map[string][]byte, key *testKey, parents ...string) string {
	extra := map[string][]byte{}
	var secrets []byte
	// git's order, by name: the test apps all sort before "recipients".
	for _, app := range slices.Sorted(maps.Keys(apps)) {
		var appTree []byte
		for _, v := range slices.Sorted(maps.Keys(apps[app])) {
			data := apps[app][v]
			id := proof.ObjectID("blob", data)
			appTree = append(appTree, treeObject("100644", v+".age", id)...)
			extra["blobs/"+id] = data
		}
		id := proof.ObjectID("tree", appTree)
		extra["trees/"+id] = appTree
		secrets = append(secrets, treeObject("40000", app, id)...)
	}
	secrets = append(secrets, treeObject("100644", "recipients", proof.ObjectID("blob", []byte("age1…\n")))...)
	secretsID := proof.ObjectID("tree", secrets)
	extra["trees/"+secretsID] = secrets
	sub := append(treeObject("100644", "Caddyfile", proof.ObjectID("blob", file)), treeObject("40000", "secrets", secretsID)...)
	id := r.commitSub(sub, file, key, parents...)
	r.extra[id] = extra
	return id
}

// secretsFile is good's app block with lines added, and a second app.
func secretsFile(exampleLines, apiLines string) []byte {
	f := strings.Replace(good, "\t\t\tcommand node server.js\n", "\t\t\tcommand node server.js\n"+exampleLines, 1)
	f = strings.Replace(f, "\t\tapp example {", "\t\tapp api {\n\t\t\tcommand ./api\n"+apiLines+"\t\t}\n\t\tapp example {", 1)
	return file(f)
}

func TestWalkAppEnv(t *testing.T) {
	s, err := Walk(secretsFile("\t\t\tenv_file /etc/motel/secrets/example.env\n\t\t\tenv MODE production\n\t\t\tenv PORT {socket}\n\t\t\tdeploy_trust github {\n\t\t\t\tclaim env_file x\n\t\t\t}\n", ""))
	if err != nil {
		t.Fatal(err)
	}
	if got := s.AppEnv["example"]; got.EnvFile != "/etc/motel/secrets/example.env" || !slices.Equal(got.Env, []string{"MODE", "PORT"}) {
		t.Fatalf("%+v", got)
	}
	if got, ok := s.AppEnv["api"]; !ok || got.EnvFile != "" || len(got.Env) != 0 {
		t.Fatalf("%+v %v", got, ok)
	}
}

func TestSecretsFit(t *testing.T) {
	secrets := func(apps ...string) *proof.Secrets {
		s := &proof.Secrets{Tree: "t"}
		for _, a := range apps {
			name, vars, _ := strings.Cut(a, ":")
			app := proof.SecretApp{Name: name}
			for _, v := range strings.Split(vars, ",") {
				app.Vars = append(app.Vars, proof.SecretVar{Name: v})
			}
			s.Apps = append(s.Apps, app)
		}
		return s
	}
	const managed = "\t\t\tenv_file /etc/motel/secrets/example.env\n"
	for name, c := range map[string]struct {
		example, api string
		secrets      *proof.Secrets
		want         string // "" fits
	}{
		"no secrets, no env_file":        {"", "", secrets(), ""},
		"no secrets, a hand-made file":   {"\t\t\tenv_file /etc/motel/example.env\n", "", secrets(), ""},
		"secrets read by their app":      {managed + "\t\t\tenv MODE x\n", "", secrets("example:DATABASE_URL,STRIPE_KEY"), ""},
		"a spelling that cleans to it":   {"\t\t\tenv_file /etc/motel//secrets/./example.env\n", "", secrets("example:K"), ""},
		"secrets for no app":             {managed, "", secrets("example:K", "billing:K"), "deploy.example.com/secrets/billing/ names no app in the new Caddyfile: add the app, or remove the directory"},
		"secrets, no env_file":           {"", "", secrets("example:K"), "app example has secrets and no env_file: add env_file /etc/motel/secrets/example.env to its block"},
		"secrets, the old file":          {"\t\t\tenv_file /etc/motel/example.env\n", "", secrets("example:K"), "app example has secrets, and its env_file is not /etc/motel/secrets/example.env: set it"},
		"secrets, a placeholder":         {"\t\t\tenv_file {env.SECRET_PATH}\n", "", secrets("example:K"), "its env_file is not /etc/motel/secrets/example.env: set it"},
		"an env line shadows a secret":   {managed + "\t\t\tenv STRIPE_KEY sk_test\n", "", secrets("example:DATABASE_URL,STRIPE_KEY"), "app example sets STRIPE_KEY with env and as a secret; env wins over env_file, so the secret would never reach it"},
		"a SOCKET secret":                {managed, "", secrets("example:SOCKET"), "secret SOCKET for app example: motel sets SOCKET last, so it would never reach the app"},
		"another app's file":             {managed, "\t\t\tenv_file /etc/motel/secrets/example.env\n", secrets("example:K"), "app api's env_file is app example's secrets file"},
		"the managed file, no secrets":   {managed, "", secrets(), "app example's env_file is /etc/motel/secrets/example.env, and deploy.example.com/secrets/example/ holds no secret: add one, or remove the env_file line"},
		"something else in secrets/":     {"", "\t\t\tenv_file /etc/motel/secrets/shared.conf\n", secrets(), "app api's env_file is in /etc/motel/secrets, where the box keeps each app's own file: use /etc/motel/secrets/api.env"},
		"a name outside the app grammar": {"", "\t\t\tenv_file /etc/motel/secrets/Bad.env\n", secrets(), "app api's env_file is in /etc/motel/secrets, where the box keeps each app's own file"},
		"the directory itself":           {"", "\t\t\tenv_file /etc/motel/secrets/\n", secrets(), "app api's env_file is in /etc/motel/secrets, where the box keeps each app's own file: use /etc/motel/secrets/api.env"},
	} {
		t.Run(name, func(t *testing.T) {
			shape, err := Walk(secretsFile(c.example, c.api))
			if err != nil {
				t.Fatal(err)
			}
			err = secretsFit(shape, c.secrets, "deploy.example.com/Caddyfile")
			if c.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var r *refusal
			if !errors.As(err, &r) || !strings.Contains(r.msg, c.want) {
				t.Fatalf("got %v, want a refusal containing %q", err, c.want)
			}
		})
	}
	t.Run("the box's own slot", func(t *testing.T) {
		withNotify := func(block string) []byte {
			return []byte(strings.Replace(string(secretsFile("", "")), "\tliveswap {", "\tnotify {\n"+block+"\t}\n\n\tliveswap {", 1))
		}
		const ops = "\t\tchannel ops ntfy {secret.NOTIFY_OPS_URL}\n\t\toperator ops\n"
		for name, c := range map[string]struct {
			file    []byte
			secrets *proof.Secrets
			want    string
		}{
			"read by the notify block":       {withNotify(ops), secrets("_box:NOTIFY_OPS_URL,NOTIFY_SPARE_URL"), ""},
			"the default, spelt out":         {withNotify(ops + "\t\tsecrets_file /etc/motel//secrets/_box.env\n"), secrets("_box:NOTIFY_OPS_URL"), ""},
			"beside an app's":                {withNotify(ops), secrets("_box:NOTIFY_OPS_URL", "example:K"), "app example has secrets and no env_file"},
			"no notify block":                {secretsFile("", ""), secrets("_box:NOTIFY_OPS_URL"), "deploy.example.com/secrets/_box/ holds the box's notify secrets, and the new Caddyfile has no notify block to read them: add one, or remove the directory"},
			"another secrets_file":           {withNotify(ops + "\t\tsecrets_file /etc/motel/s3cr3t.env\n"), secrets("_box:NOTIFY_OPS_URL"), "deploy.example.com/secrets/_box/ is written to /etc/motel/secrets/_box.env, and the notify block's secrets_file is another file: remove the secrets_file line"},
			"a name not notify's":            {withNotify(ops), secrets("_box:NOTIFY_OPS_URL,DATABASE_URL"), "deploy.example.com/secrets/_box/DATABASE_URL.age: the box's own slot holds notify's channel URLs alone"},
			"a lowercase notify name":        {withNotify(ops), secrets("_box:NOTIFY_ops"), "the box's own slot holds notify's channel URLs alone"},
			"an app's env_file is the box's": {secretsFile("\t\t\tenv_file /etc/motel/secrets/_box.env\n", ""), secrets(), "app example's env_file is the box's own secrets file, which notify reads and no app is given: use /etc/motel/secrets/example.env"},
		} {
			t.Run(name, func(t *testing.T) {
				shape, err := Walk(c.file)
				if err != nil {
					t.Fatal(err)
				}
				err = secretsFit(shape, c.secrets, "deploy.example.com/Caddyfile")
				if c.want == "" {
					if err != nil {
						t.Fatal(err)
					}
					return
				}
				var r *refusal
				if !errors.As(err, &r) || !strings.Contains(r.msg, c.want) {
					t.Fatalf("got %v, want a refusal containing %q", err, c.want)
				}
				if strings.Contains(r.msg, "s3cr3t") {
					t.Fatalf("a secrets_file value was quoted back: %v", err)
				}
			})
		}
	})
	t.Run("an env_file value is never quoted back", func(t *testing.T) {
		shape, err := Walk(secretsFile("\t\t\tenv_file /etc/motel/s3cr3t-path.env\n", ""))
		if err != nil {
			t.Fatal(err)
		}
		err = secretsFit(shape, secrets("example:K"), "deploy.example.com/Caddyfile")
		if err == nil || strings.Contains(err.Error(), "s3cr3t") {
			t.Fatalf("got %v", err)
		}
	})
}
