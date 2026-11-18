package liveswap

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/smallhoursorg/motel/liveswap/backupdecl"
)

func unmarshalApps(t *testing.T, appBody string) (*App, error) {
	t.Helper()
	var a App
	err := a.UnmarshalCaddyfile(caddyfile.NewTestDispenser("liveswap {\n app blog {\n command ./server\n" + appBody + "\n}\n}"))
	return &a, err
}

func TestCaddyfileBackupBlock(t *testing.T) {
	a, err := unmarshalApps(t, `backup {
		sqlite app.db
		files  uploads avatars
		sqlite "my data/jobs.db"
	}
	keep 3`)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := a.Apps["blog"].Backup
	want := &backupdecl.Config{SQLite: []string{"app.db", "my data/jobs.db"}, Files: []string{"uploads", "avatars"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("backup = %+v, want %+v", got, want)
	}
	// The block consumes exactly its own tokens: what follows it is
	// still the app's.
	if a.Apps["blog"].Keep != 3 {
		t.Fatalf("the directive after the backup block was lost: keep = %d", a.Apps["blog"].Keep)
	}
}

func TestCaddyfileNoBackupBlockDeclaresNothing(t *testing.T) {
	a, err := unmarshalApps(t, "")
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if a.Apps["blog"].Backup != nil {
		t.Fatalf("an app with no backup block has Backup = %+v, want nil", a.Apps["blog"].Backup)
	}
}

func TestCaddyfileBackupBlockRejects(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"a second block", "backup {\n sqlite a.db\n}\nbackup {\n files x\n}", "duplicate backup block"},
		{"an argument before the block", "backup nightly {\n sqlite a.db\n}", "wrong argument count"},
		{"an unknown line", "backup {\n postgres main\n}", `unknown backup subdirective "postgres"`},
		{"a kind with no path", "backup {\n sqlite\n}", "wrong argument count"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := unmarshalApps(t, tc.body)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.want)) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

// A block that names nothing parses — to a declaration, not to "no
// backup" — so that the one place that says what a valid declaration
// is, backupdecl through Validate, is the place that refuses it.
func TestCaddyfileEmptyBackupBlockIsADeclaration(t *testing.T) {
	for _, body := range []string{"backup", "backup {\n}"} {
		a, err := unmarshalApps(t, body)
		if err != nil {
			t.Fatalf("%q: unmarshal: %v", body, err)
		}
		b := a.Apps["blog"].Backup
		if b == nil {
			t.Fatalf("%q parsed to no declaration at all, which would load silently", body)
		}
		if err := b.Validate(); err == nil || !strings.Contains(err.Error(), "declares nothing") {
			t.Fatalf("%q: want Validate to refuse it as declaring nothing, got %v", body, err)
		}
	}
}

func TestValidateRefusesABadBackupDeclaration(t *testing.T) {
	valid := func(b *backupdecl.Config) *App {
		cfg := defaultedApp(t)
		cfg.Backup = b
		return &App{
			Root:              "/var/lib/liveswap",
			ArtifactAllowlist: []string{"github.com/smallhoursorg/"},
			DeployTrust:       githubTrust(),
			Apps:              map[string]*AppConfig{"blog": cfg},
		}
	}
	if err := valid(&backupdecl.Config{SQLite: []string{"app.db"}, Files: []string{"uploads"}}).Validate(); err != nil {
		t.Fatalf("valid declaration rejected: %v", err)
	}
	if err := valid(nil).Validate(); err != nil {
		t.Fatalf("an app that declares no backup was rejected: %v", err)
	}
	for _, tc := range []struct {
		name string
		b    *backupdecl.Config
		want string
	}{
		{"an empty block", &backupdecl.Config{}, "declares nothing"},
		{"a path outside shared", &backupdecl.Config{Files: []string{"../shop/shared"}}, "outside the app's shared dir"},
		{"a placeholder", &backupdecl.Config{SQLite: []string{"{shared_dir}/app.db"}}, "placeholder"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := valid(tc.b).Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "app blog: backup") {
				t.Fatalf("want an error naming `app blog: backup` and containing %q, got %v", tc.want, err)
			}
		})
	}
}

// A declaration is for a reader outside this process, which does not
// have this process's environment. A root only that environment can
// spell is therefore refused — but only where an app declares a
// backup: a config with no backup block is unaffected.
func TestBackupNeedsALiteralRoot(t *testing.T) {
	declared := map[string]*AppConfig{"blog": {Backup: &backupdecl.Config{Files: []string{"."}}}, "api": {}}
	undeclared := map[string]*AppConfig{"blog": {}, "api": nil}

	for _, root := range []string{"{env.LIVESWAP_ROOT}", "/srv/{env.NAME}/liveswap", "/srv/live}swap"} {
		err := backupNeedsLiteralRoot(root, declared)
		if err == nil || !strings.Contains(err.Error(), "app blog") || !strings.Contains(err.Error(), "placeholder") {
			t.Errorf("root %q with a declared backup: want an error naming app blog and the placeholder, got %v", root, err)
		}
		if err := backupNeedsLiteralRoot(root, undeclared); err != nil {
			t.Errorf("root %q with no backup declared must keep loading, got %v", root, err)
		}
	}
	for _, root := range []string{"", "/var/lib/liveswap", "/srv/live swap:50%$HOME"} {
		if err := backupNeedsLiteralRoot(root, declared); err != nil {
			t.Errorf("literal root %q rejected: %v", root, err)
		}
	}
}

// The check has to see the root as written: Provision resolves
// {env.*} in place, after which a placeholder root is indistinguishable
// from a literal one.
func TestProvisionChecksTheRootBeforeResolvingIt(t *testing.T) {
	t.Setenv("LIVESWAP_TEST_ROOT", "/var/lib/liveswap")
	a := &App{
		Root: "{env.LIVESWAP_TEST_ROOT}",
		Apps: map[string]*AppConfig{"blog": {Backup: &backupdecl.Config{Files: []string{"."}}}},
	}
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()
	err := a.Provision(ctx)
	if err == nil || !strings.Contains(err.Error(), "placeholder") {
		t.Fatalf("want Provision to refuse the placeholder root, got %v", err)
	}
}

// The declaration reaches its reader as the JSON `moteld adapt`
// prints, so the shape is part of the contract.
func TestBackupJSONShape(t *testing.T) {
	a, err := unmarshalApps(t, "backup {\n sqlite app.db\n files uploads\n}")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	const want = `"backup":{"sqlite":["app.db"],"files":["uploads"]}`
	if !strings.Contains(string(raw), want) {
		t.Fatalf("adapted JSON = %s, want it to contain %s", raw, want)
	}
	plain, _ := unmarshalApps(t, "")
	if raw, _ := json.Marshal(plain); strings.Contains(string(raw), "backup") {
		t.Fatalf("an app that declares nothing still has a backup key: %s", raw)
	}
}

// The property the rules exist for: a declaration Validate accepts
// names nothing outside the app's shared dir, in a spelling that
// survives being joined and cleaned, and with nothing in it that a
// placeholder pass could rewrite.
func FuzzBackupDeclaration(f *testing.F) {
	for _, seed := range [][2]string{
		{"app.db", "uploads"}, {"data/app.db", "."}, {"../x", "y"}, {"/abs", "{env.X}"},
		{"a/../../b", "a//b"}, {"app.db", "app.db-wal"}, {"x\x00y", "z\n"}, {"..", "..."}, {"", "\xff"}, {"a" + string(rune(0x85)), "b" + string(rune(0x9f))},
	} {
		f.Add(seed[0], seed[1])
	}
	const shared = "/var/lib/liveswap/blog/shared"
	f.Fuzz(func(t *testing.T, sqlite, files string) {
		c := backupdecl.Config{SQLite: []string{sqlite}, Files: []string{files}}
		if c.Validate() != nil {
			return
		}
		for _, item := range []string{sqlite, files} {
			joined := filepath.Join(shared, item)
			if !pathWithin(joined, shared) {
				t.Fatalf("accepted %q, which joins to %q, outside %s", item, joined, shared)
			}
			if item != "." && joined != shared+"/"+item {
				t.Fatalf("accepted %q, which does not survive a join: %q", item, joined)
			}
			if strings.ContainsAny(item, "{}") {
				t.Fatalf("accepted %q, which a placeholder pass could rewrite", item)
			}
			if strings.ContainsFunc(item, unicode.IsControl) {
				t.Fatalf("accepted %q, which holds a control character", item)
			}
		}
		if sqlite == "." || joinedEqual(shared, sqlite, files) {
			t.Fatalf("accepted sqlite %q with files %q", sqlite, files)
		}
	})
}

func joinedEqual(base, a, b string) bool { return filepath.Join(base, a) == filepath.Join(base, b) }

// backupdecl repeats three of liveswap's values for readers that do
// not link liveswap. This is what notices either side changing.
func TestBackupdeclAgreesWithLiveswap(t *testing.T) {
	a := new(App)
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()
	if err := a.Provision(ctx); err != nil {
		t.Fatalf("provisioning a config with no apps: %v", err)
	}
	if a.Root != backupdecl.DefaultRoot {
		t.Errorf("liveswap's default root is %q, backupdecl.DefaultRoot is %q", a.Root, backupdecl.DefaultRoot)
	}
	for _, root := range []string{"/var/lib/liveswap", "/srv/live swap"} {
		if got, want := backupdecl.SharedDir(root, "blog"), newAppDirs(root, "blog").shared; got != want {
			t.Errorf("SharedDir(%q) = %q, liveswap's is %q", root, got, want)
		}
	}
	for _, name := range []string{"blog", "a", "blog-api", "0", "", "Blog", "blog_api", "blog.api", "blog/api", strings.Repeat("a", 63), strings.Repeat("a", 64), "-", "é"} {
		if got, want := backupdecl.ValidAppName(name), appNameRe.MatchString(name); got != want {
			t.Errorf("ValidAppName(%q) = %v, liveswap says %v", name, got, want)
		}
	}
}
