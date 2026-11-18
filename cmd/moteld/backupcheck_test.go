package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/liveswap"
)

// Caddyfiles adapted by Caddy's own adapter, as validate and reload
// adapt them: where each backup declaration comes from is Caddy's word,
// after its imports, snippets and heredocs.

// TestMain makes every test one on a box where motel-backup is not
// installed, unless it says otherwise (installed), and where the
// default liveswap root is not there: whether this machine has either
// is not the test's business.
func TestMain(m *testing.M) {
	backupBinary = "/nonexistent/motel-backup"
	liveswapDefaultRoot = "/nonexistent/liveswap"
	os.Exit(m.Run())
}

// installed stands a motel-backup in, reading caddyfile.
func installed(t *testing.T, caddyfile string) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "motel-backup")
	write(t, bin, "#!/bin/sh\n", 0o755)
	oldBin, oldFile := backupBinary, backupCaddyfile
	backupBinary, backupCaddyfile = bin, caddyfile
	t.Cleanup(func() { backupBinary, backupCaddyfile = oldBin, oldFile })
}

func write(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil { // not cut by the umask
		t.Fatal(err)
	}
}

const app = "app blog {\n\tcommand /bin/true\n\tbackup {\n\t\tfiles .\n\t}\n}\n"

func global(body string) string { return "{\n\tadmin off\n\tliveswap {\n" + body + "\t}\n}\n" }

// config is a directory standing for /etc/motel, open to others as
// it is on a box, and one outside it.
func dirs(t *testing.T) (config, outside string) {
	t.Helper()
	config, outside = t.TempDir(), t.TempDir()
	for _, d := range []string{config, outside} {
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return config, outside
}

// checked asserts that a Caddyfile was looked at and passed — not
// skipped: a check that never ran passes as well as one that did.
func checked(t *testing.T, cmd string, flags ...string) {
	t.Helper()
	liveswap.ClearBackupSources()
	if _, err := checkConfig(cmd, flags); err != nil {
		t.Fatalf("%s %q: %v", cmd, flags, err)
	}
	if len(liveswap.BackupSources()) == 0 {
		t.Fatalf("%s %q: nothing was checked", cmd, flags)
	}
}

func TestABackupDeclaredWhereARunSeesItPasses(t *testing.T) {
	config, _ := dirs(t)
	write(t, config+"/apps/blog.caddy", app, 0o644)
	write(t, config+"/Caddyfile", global("\t\troot /var/lib/liveswap\n\t\timport apps/*.caddy\n"), 0o644)
	checked(t, "validate", "--config", config+"/Caddyfile")
	// A link that stays under the directory is followed there.
	if err := os.Symlink("apps", config+"/sites"); err != nil {
		t.Fatal(err)
	}
	write(t, config+"/Caddyfile.linked", global("\t\timport sites/*.caddy\n"), 0o644)
	checked(t, "validate", "--config", config+"/Caddyfile.linked")
	checked(t, "validate", "-c"+config+"/Caddyfile")
	checked(t, "reload", "-fc", config+"/Caddyfile")
}

// Every way Caddy takes the flag, the Caddyfile given relatively, and
// its adjacent Caddyfile with none. In a process of its own, started in
// the directory: Caddy resolves a relative Caddyfile against the working
// directory it first saw, and caches it (caddy.FastAbs), so a test that
// changed directory part way would be asking about another one.
func TestARelativeCaddyfileIsCheckedWhereCaddyReadsIt(t *testing.T) {
	if os.Getenv("BACKUPCHECK_RELATIVE") == "" {
		config, _ := dirs(t)
		write(t, config+"/apps/blog.caddy", app, 0o644)
		write(t, config+"/Caddyfile", global("\t\timport apps/*.caddy\n"), 0o644)
		child := exec.Command(os.Args[0], "-test.run=^TestARelativeCaddyfileIsCheckedWhereCaddyReadsIt$", "-test.v")
		child.Dir, child.Env = config, append(os.Environ(), "BACKUPCHECK_RELATIVE=1")
		if out, err := child.CombinedOutput(); err != nil || !strings.Contains(string(out), "--- PASS") {
			t.Fatalf("%v\n%s", err, out)
		}
		return
	}
	checked(t, "reload", "--config=Caddyfile", "--force")
	checked(t, "validate", "-c", "Caddyfile")
	checked(t, "validate", "-cCaddyfile")
	checked(t, "run")
}

func TestABackupDeclaredOutsideTheCaddyfilesDirectoryIsRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		caddyfile, outsideFile, outsideBody string
		want                                []string
	}{
		"an imported file": {
			global("\t\timport OUTSIDE/shop.caddy\n"), "shop.caddy", app,
			[]string{"blog's backup is read from OUTSIDE/shop.caddy, outside CONFIG"},
		},
		"a snippet defined outside": {
			"import OUTSIDE/snip.caddy\n" + global("\t\tapp blog {\n\t\t\tcommand /bin/true\n\t\t\timport bk\n\t\t}\n"),
			"snip.caddy", "(bk) {\n\tbackup {\n\t\tfiles .\n\t}\n}\n",
			[]string{"blog's backup is read from OUTSIDE/snip.caddy"},
		},
		"the whole global block, root and all": {
			"import OUTSIDE/global.caddy\n", "global.caddy", global("\t\troot /var/lib/liveswap\n" + app),
			[]string{"the liveswap root is read from OUTSIDE/global.caddy", "blog's backup is read from OUTSIDE/global.caddy"},
		},
		"an import by a heredoc": {
			global("\t\timport <<P\n\t\tOUTSIDE/shop.caddy\n\t\tP\n"), "shop.caddy", app,
			[]string{"blog's backup is read from OUTSIDE/shop.caddy"},
		},
		"only its lines, from an import inside the block": {
			global("\t\tapp blog {\n\t\t\tcommand /bin/true\n\t\t\tbackup {\n\t\t\t\timport OUTSIDE/lines.caddy\n\t\t\t}\n\t\t}\n"),
			"lines.caddy", "files .\n",
			[]string{"blog's backup is read from OUTSIDE/lines.caddy"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			config, outside := dirs(t)
			write(t, filepath.Join(outside, tc.outsideFile), tc.outsideBody, 0o644)
			write(t, config+"/Caddyfile", strings.ReplaceAll(tc.caddyfile, "OUTSIDE", outside), 0o644)
			_, err := checkConfig("validate", []string{"--config", config + "/Caddyfile"})
			for _, w := range tc.want {
				w = strings.NewReplacer("OUTSIDE", outside, "CONFIG", config).Replace(w)
				if err == nil || !strings.Contains(err.Error(), w) {
					t.Fatalf("want %q in the error, got %v", w, err)
				}
			}
		})
	}
}

// The Caddyfile's own mode is asked at a reload: a candidate being
// validated is not the file a run reads (TestACandidateIsNotHeldToWhereItStands).
func TestWhatOthersMayNotReadIsRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		cmd                               string
		caddyfileMode, appsMode, fileMode os.FileMode
		want                              string
	}{
		"the Caddyfile":     {"reload", 0o640, 0o755, 0o644, "the Caddyfile, CONFIG/Caddyfile, may not be read by others"},
		"the imported file": {"validate", 0o644, 0o755, 0o640, "blog's backup is read from CONFIG/apps/blog.caddy, which others may not read"},
		"a directory":       {"validate", 0o644, 0o750, 0o644, "blog's backup is read from under CONFIG/apps, which others may not enter"},
	} {
		t.Run(name, func(t *testing.T) {
			config, _ := dirs(t)
			write(t, config+"/apps/blog.caddy", app, tc.fileMode)
			write(t, config+"/Caddyfile", global("\t\timport apps/*.caddy\n"), tc.caddyfileMode)
			if err := os.Chmod(config+"/apps", tc.appsMode); err != nil {
				t.Fatal(err)
			}
			_, err := checkConfig(tc.cmd, []string{"--config", config + "/Caddyfile"})
			if w := strings.ReplaceAll(tc.want, "CONFIG", config); err == nil || !strings.Contains(err.Error(), w) {
				t.Fatalf("want %q in the error, got %v", w, err)
			}
		})
	}
}

// A candidate being validated is not where a run reads it: the box
// applier's step 6 validates a copy staged 0600 in a directory of its
// own, 0700. Its own mode and place are asked at the reload, of the
// file the push installs; what it imports is asked now.
func TestACandidateIsNotHeldToWhereItStands(t *testing.T) {
	stage := t.TempDir()
	if err := os.Chmod(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, stage+"/.1.Caddyfile", global(app), 0o600)
	checked(t, "validate", "--config", stage+"/.1.Caddyfile")
	_, err := checkConfig("reload", []string{"--config", stage + "/.1.Caddyfile"})
	for _, w := range []string{
		"the Caddyfile, " + stage + "/.1.Caddyfile, may not be read by others",
		"blog's backup is read from " + stage + "/.1.Caddyfile, which others may not read",
		"blog's backup is read from under " + stage + ", which others may not enter",
	} {
		if err == nil || !strings.Contains(err.Error(), w) {
			t.Fatalf("reload of the candidate: want %q in the error, got %v", w, err)
		}
	}
	_, outside := dirs(t)
	write(t, outside+"/shop.caddy", app, 0o644)
	write(t, stage+"/.2.Caddyfile", global("\t\timport "+outside+"/shop.caddy\n"), 0o600)
	_, err = checkConfig("validate", []string{"--config", stage + "/.2.Caddyfile"})
	if w := "blog's backup is read from " + outside + "/shop.caddy, outside " + stage; err == nil || !strings.Contains(err.Error(), w) {
		t.Fatalf("validate of a candidate importing from outside: want %q in the error, got %v", w, err)
	}
	// The file a run reads is no candidate: validated in place, it is
	// asked all of it.
	config, _ := dirs(t)
	write(t, config+"/Caddyfile", global(app), 0o600)
	installed(t, config+"/Caddyfile")
	_, err = checkConfig("validate", []string{"--config", config + "/Caddyfile"})
	if w := "the Caddyfile, " + config + "/Caddyfile, may not be read by others"; err == nil || !strings.Contains(err.Error(), w) {
		t.Fatalf("validate of the file backups read: want %q in the error, got %v", w, err)
	}
	// Its directory is asked too.
	write(t, config+"/Caddyfile", global(app), 0o644)
	if err := os.Chmod(config, 0o750); err != nil {
		t.Fatal(err)
	}
	_, err = checkConfig("validate", []string{"--config", config + "/Caddyfile"})
	if w := "blog's backup is read from under " + config + ", which others may not enter"; err == nil || !strings.Contains(err.Error(), w) {
		t.Fatalf("validate of the file backups read, in a 0750 directory: want %q in the error, got %v", w, err)
	}
}

// Nothing is asked of a Caddyfile that declares no backup, of one Caddy
// cannot adapt (its own command says why), or of a config that is not a
// Caddyfile.
func TestNothingIsAskedOfWhatHasNoBackupToLose(t *testing.T) {
	config, outside := dirs(t)
	write(t, outside+"/site.caddy", "http://:8080 {\n\trespond hi\n}\n", 0o600)
	write(t, config+"/Caddyfile", "import "+outside+"/site.caddy\n", 0o600)
	write(t, config+"/broken", global("\t\tnonsense\n")+app, 0o600)
	write(t, config+"/c.json", `{}`, 0o600)
	write(t, outside+"/shop.caddy", app, 0o644)
	write(t, config+"/sites.conf", global("\t\timport "+outside+"/shop.caddy\n"), 0o644)
	for _, flags := range [][]string{
		{"--config", config + "/Caddyfile"},
		{"--config", config + "/broken", "--adapter", "caddyfile"},
		{"--config", config + "/c.json"},
		{"--config", config + "/Caddyfile", "--adapter", "json"},
		// Caddy reads a name that neither begins "Caddyfile" nor ends
		// ".caddyfile" as JSON, with no --adapter; so is it left here.
		{"--config", config + "/sites.conf"},
	} {
		if _, err := checkConfig("validate", flags); err != nil {
			t.Errorf("%q: %v", flags, err)
		}
	}
}

// validate and reload stop; run serves, and says why.
// Apps share the box's network namespace, so an admin API on TCP is
// theirs to reach: a Caddyfile that runs apps is refused (validate,
// reload), or served with the error said (run), unless the API is off
// or on a unix socket. One that runs no app is not asked. The default
// is the config's alone: a CADDY_ADMIN in the shell that validates is
// not the unit's.
func TestAnAdminAPIOnTCPIsRefusedWhereAppsRun(t *testing.T) {
	t.Setenv("CADDY_ADMIN", "unix//nowhere/admin.sock")
	config, _ := dirs(t)
	caddyfile := func(adminLine string) string {
		return "{\n" + adminLine + "\tliveswap {\n\t\tapp blog {\n\t\t\tcommand /bin/true\n\t\t}\n\t}\n}\n"
	}
	for name, tc := range map[string]struct{ body, want string }{
		"the default":     {caddyfile(""), "localhost:2019 (Caddy's default"},
		"tcp":             {caddyfile("\tadmin 0.0.0.0:2019\n"), "0.0.0.0:2019"},
		"unix socket":     {caddyfile("\tadmin unix/" + config + "/admin.sock\n"), ""},
		"abstract socket": {caddyfile("\tadmin unix/@motel-admin\n"), "unix/@motel-admin (an abstract socket"},
		// A socket an app's sandbox holds: under the default root, where
		// the app's shared dir is bound writable; under the base view,
		// bound read-only into every sandbox; under a configured root,
		// the socket's mode suffix cut as Caddy's listener cuts it.
		"socket in an app's shared dir": {caddyfile("\tadmin unix//var/lib/liveswap/blog/shared/admin.sock\n"), "unix//var/lib/liveswap/blog/shared/admin.sock, which an app this config runs can reach: /var/lib/liveswap/blog/shared/admin.sock is under the liveswap root /var/lib/liveswap"},
		"socket in the base view":       {caddyfile("\tadmin unix//usr/local/run/admin.sock\n"), "unix//usr/local/run/admin.sock, which an app this config runs can reach: /usr/local/run/admin.sock is under /usr, which is bound read-only"},
		"socket under a configured root": {"{\n\tadmin unix/" + config + "/apps/blog/shared/admin.sock|0660\n\tliveswap {\n\t\troot " + config + "/apps\n\t\tapp blog {\n\t\t\tcommand /bin/true\n\t\t}\n\t}\n}\n",
			"unix/" + config + "/apps/blog/shared/admin.sock|0660, which an app this config runs can reach: " + config + "/apps/blog/shared/admin.sock is under the liveswap root " + config + "/apps"},
		// A relative path is Caddy's working directory's to place, which
		// the gate cannot see; one under the liveswap root would pass
		// the view check above by its spelling.
		"socket at a relative path": {caddyfile("\tadmin unix/admin.sock|0660\n"), "unix/admin.sock|0660, a unix socket at a relative path"},
		"off":                       {caddyfile("\tadmin off\n"), ""},
		"no app":                    {":8080 {\n\trespond ok\n}\n", ""},
		"said to be meant":          {"{\n\tadmin 0.0.0.0:2019\n\tliveswap {\n\t\tallow_tcp_admin\n\t\tapp blog {\n\t\t\tcommand /bin/true\n\t\t}\n\t}\n}\n", ""},
	} {
		write(t, config+"/Caddyfile", tc.body, 0o644)
		_, err := checkConfig("validate", []string{"--config", config + "/Caddyfile"})
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), "the admin API listens on "+tc.want)):
			t.Errorf("%s: %v, want the admin API refused", name, err)
		}
	}
	// An `--envfile` is loaded first, as Caddy loads it: a listener set
	// only there is the one judged, a key assigned twice takes its last
	// value (Caddy reads the file into a map), and a variable the
	// environment holds already is kept.
	write(t, config+"/listen.env", "# the rig's\nADMIN_LISTEN=unix//run/motel/admin.sock\nexport ADMIN_LISTEN=0.0.0.0:2019\nKEPT='from the file'\n", 0o644)
	t.Setenv("KEPT", "from the environment")
	write(t, config+"/Caddyfile", caddyfile("\tadmin {$ADMIN_LISTEN:unix//run/motel/admin.sock}\n"), 0o644)
	if _, err := checkConfig("validate", []string{"--config", config + "/Caddyfile", "--envfile", config + "/listen.env"}); err == nil || !strings.Contains(err.Error(), "the admin API listens on 0.0.0.0:2019") {
		t.Errorf("a listener from the envfile: %v, want the admin API refused", err)
	}
	if os.Getenv("KEPT") != "from the environment" {
		t.Errorf("the envfile overwrote the environment: KEPT=%q", os.Getenv("KEPT"))
	}
	// A config that is not a Caddyfile is asked the same: the rule is the
	// adapted config's, whatever wrote it.
	write(t, config+"/caddy.json", `{"apps":{"liveswap":{"apps":{"blog":{"command":["/bin/true"]}}}}}`, 0o644)
	if _, err := checkConfig("validate", []string{"--config", config + "/caddy.json"}); err == nil || !strings.Contains(err.Error(), "the admin API listens on localhost:2019 (Caddy's default") {
		t.Errorf("a JSON config: %v, want the admin API refused", err)
	}
	write(t, config+"/Caddyfile", caddyfile(""), 0o644)
	for cmd, stops := range map[string]bool{"validate": true, "reload": true, "run": false} {
		var stderr bytes.Buffer
		if got := gate([]string{cmd, "--config", config + "/Caddyfile"}, &stderr); got != stops || !strings.Contains(stderr.String(), "the admin API listens on localhost:2019") {
			t.Errorf("%s: stop = %v, want %v; said %q", cmd, got, stops, stderr.String())
		}
	}
}

func TestOnlyValidateAndReloadAreStopped(t *testing.T) {
	config, outside := dirs(t)
	write(t, outside+"/shop.caddy", app, 0o644)
	write(t, config+"/Caddyfile", global("\t\timport "+outside+"/shop.caddy\n"), 0o644)
	for cmd, stops := range map[string]bool{"validate": true, "reload": true, "run": false, "adapt": false, "version": false} {
		var stderr bytes.Buffer
		if got := gate([]string{cmd, "--config", config + "/Caddyfile"}, &stderr); got != stops {
			t.Errorf("%s: stop = %v, want %v", cmd, got, stops)
		}
		said := stderr.String()
		switch {
		case cmd == "run" && !strings.Contains(said, "served all the same"):
			t.Errorf("run said %q", said)
		case stops && !strings.HasPrefix(said, "Error: a backup run would not see"):
			t.Errorf("%s said %q", cmd, said)
		case (cmd == "adapt" || cmd == "version") && said != "":
			t.Errorf("%s said %q", cmd, said)
		}
	}
}

// A config that is not a regular file — stdin, a pipe — is Caddy's to
// read, once: read here first, Caddy would be given nothing, and a pipe
// with no writer would never answer. So the gate cannot see whether it
// runs apps with the admin API on TCP, and refuses it by name rather
// than let it through unseen.
func TestAConfigThatIsNotAFileIsRefusedUnread(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "Caddyfile")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"a pipe": {"--config", fifo},
		"stdin":  {"--config", "-"},
	} {
		done := make(chan error, 1)
		go func() { _, err := checkConfig("validate", args); done <- err }()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "not a regular file, which this check cannot read before Caddy reads it once") {
				t.Errorf("%s: %v, want the config refused unread", name, err)
			}
			if name == "stdin" && !strings.Contains(err.Error(), "the config is stdin") {
				t.Errorf("stdin: %v, want it named", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the check opened a pipe nobody writes to", name)
		}
	}
}

// A link under the directory that leads out of it: Caddy reads through
// it, and joins imports as written, so what it reads looks as if it were
// under the directory; in a backup run's view the link leads nowhere.
func TestABackupReadThroughALinkOutIsRefused(t *testing.T) {
	config, outside := dirs(t)
	write(t, outside+"/blog.caddy", app, 0o644)
	if err := os.Symlink(outside, config+"/apps"); err != nil {
		t.Fatal(err)
	}
	write(t, config+"/Caddyfile", global("\t\timport apps/*.caddy\n"), 0o644)
	_, err := checkConfig("validate", []string{"--config", config + "/Caddyfile"})
	if w := "blog's backup is read from " + config + "/apps/blog.caddy, which leads to " + outside + "/blog.caddy, outside " + config; err == nil || !strings.Contains(err.Error(), w) {
		t.Fatalf("want %q in the error, got %v", w, err)
	}
}

// The root of the filesystem is a directory like any other.
func TestUnderTheRoot(t *testing.T) {
	for _, tc := range []struct {
		p, dir string
		want   bool
	}{
		{"/etc/Caddyfile", "/", true},
		{"/etc/motel/a", "/etc/motel", true},
		{"/etc/motel-old/a", "/etc/motel", false},
		{"/etc/motel", "/etc/motel", false},
	} {
		if got := under(tc.p, tc.dir); got != tc.want {
			t.Errorf("under(%q, %q) = %v", tc.p, tc.dir, got)
		}
	}
}

// The check takes each command's flags from Caddy's own definition of
// it, not a copy: were that to stop being there, the check would skip
// itself, silently, for every Caddyfile.
func TestCaddysOwnFlagsAreWhereTheCheckTakesThem(t *testing.T) {
	for _, name := range []string{"validate", "reload", "run"} {
		fs, ok := commandFlags(name, nil)
		if !ok {
			t.Errorf("%s: no flags from Caddy's own command", name)
			continue
		}
		for _, flag := range []string{"config", "adapter"} {
			if fs.Lookup(flag) == nil {
				t.Errorf("%s: Caddy's command has no --%s", name, flag)
			}
		}
	}
}

// motel-backup reads one Caddyfile, /etc/motel/Caddyfile. Where it
// is installed, a reload that would run another, declaring a backup, is
// refused: the server would run what no backup run reads, and an app
// only in it would never be backed up while status said all was well.
// A start is not refused; validate is of a candidate, and is not asked.
// Where it is not installed, a reload is warned.
func TestAReloadOfAnotherCaddyfileIsRefusedWhereBackupsReadTheCanonicalOne(t *testing.T) {
	config, _ := dirs(t)
	write(t, config+"/apps/blog.caddy", app, 0o644)
	body := global("\t\timport apps/*.caddy\n")
	write(t, config+"/Caddyfile", body, 0o644)
	write(t, config+"/Caddyfile.new", body, 0o644)
	other := config + "/Caddyfile.new"

	installed(t, config+"/Caddyfile")
	for cmd, stops := range map[string]bool{"reload": true, "run": false, "validate": false} {
		var stderr bytes.Buffer
		if got := gate([]string{cmd, "--config", other}, &stderr); got != stops {
			t.Errorf("%s of another Caddyfile: stop = %v, want %v (%s)", cmd, got, stops, stderr.String())
		}
		said := stderr.String()
		if cmd == "validate" {
			if said != "" {
				t.Errorf("validate said %q", said)
			}
			continue
		}
		for _, w := range []string{"runs " + other + ", which declares backups", "motel-backup reads " + config + "/Caddyfile", "`motel-backup validate " + other + "`"} {
			if !strings.Contains(said, w) {
				t.Errorf("%s: want %q in %q", cmd, w, said)
			}
		}
	}
	// The one it reads, however it is named.
	checked(t, "reload", "--config", config+"/Caddyfile")
	if err := os.Symlink(config+"/Caddyfile", config+"/current"); err != nil {
		t.Fatal(err)
	}
	checked(t, "reload", "--config", config+"/current", "--adapter", "caddyfile")
	// The same file from another directory is not: Caddy resolves its
	// relative imports from there, beside other files than a backup run's.
	elsewhere := t.TempDir()
	write(t, elsewhere+"/apps/blog.caddy", app, 0o644)
	if err := os.Symlink(config+"/Caddyfile", elsewhere+"/Caddyfile"); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if !gate([]string{"reload", "--config", elsewhere + "/Caddyfile"}, &stderr) {
		t.Errorf("a link to the Caddyfile from another directory was taken for it: %s", stderr.String())
	}
	// Another that declares no backup is none of this.
	write(t, config+"/Caddyfile.plain", "http://:8080 {\n\trespond hi\n}\n", 0o644)
	if w, err := checkConfig("reload", []string{"--config", config + "/Caddyfile.plain"}); w != "" || err != nil {
		t.Errorf("no backup declared: %q, %v", w, err)
	}
}

func TestAReloadOfAnotherCaddyfileIsWarnedWhereBackupsAreNotInstalled(t *testing.T) {
	config, _ := dirs(t)
	write(t, config+"/apps/blog.caddy", app, 0o644)
	write(t, config+"/Caddyfile.new", global("\t\timport apps/*.caddy\n"), 0o644)
	for _, cmd := range []string{"reload", "run"} {
		var stderr bytes.Buffer
		if gate([]string{cmd, "--config", config + "/Caddyfile.new"}, &stderr) {
			t.Fatalf("%s stopped: %s", cmd, stderr.String())
		}
		// A start is never refused, installed or not: what would be is a reload.
		if said := stderr.String(); !strings.HasPrefix(said, "WARNING: ") || !strings.Contains(said, "/etc/motel/Caddyfile") ||
			strings.Contains(said, "motel-backup validate") || !strings.Contains(said, "a reload of it would be refused") {
			t.Fatalf("%s said %q", cmd, said)
		}
	}
}
