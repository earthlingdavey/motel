package liveswap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// After a restart the app is reattached without a launch: the first
// door built for it reads the env_file from the spec, so the app's text
// never carries a value the file holds, and a later launch adds to what
// is known rather than replacing it.
func TestDoorLoadsEnvFileWithoutALaunch(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "app.env")
	if err := os.WriteFile(envFile, []byte("DATABASE_URL=postgres://app:s3cr3t-pw-value@db/app\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := testSpec(t)
	spec.envFile = envFile
	ma := newManagedApp(spec.name)
	ma.spec = spec

	out, keys := ma.door().r.Redact("dial postgres://app:s3cr3t-pw-value@db/app: refused")
	if strings.Contains(out, "s3cr3t-pw-value") || strings.Join(keys, ",") != "DATABASE_URL" {
		t.Fatalf("env_file value not known before any launch: %q %v", out, keys)
	}

	// A launch with a rotated value: both are known afterwards.
	ma.rememberSecrets(envFile, []string{"DATABASE_URL=postgres://app:new-pw-value-2@db/app"})
	out, _ = ma.door().r.Redact("old s3cr3t-pw-value new new-pw-value-2")
	if strings.Contains(out, "s3cr3t-pw-value") || strings.Contains(out, "new-pw-value-2") {
		t.Fatalf("a rotated value must stay redacted: %q", out)
	}

	// A reload that names a different env_file: the next filter reads
	// it, and what the old one held stays known.
	other := filepath.Join(t.TempDir(), "other.env")
	if err := os.WriteFile(other, []byte("TOKEN=from-the-other-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded := testSpec(t)
	reloaded.envFile = other
	ma.spec = reloaded
	out, _ = ma.door().r.Redact("from-the-other-file and s3cr3t-pw-value")
	if strings.Contains(out, "from-the-other-file") || strings.Contains(out, "s3cr3t-pw-value") {
		t.Fatalf("after a reload naming another env_file: %q", out)
	}

	// A file that cannot be read yet is not an error here, and the
	// read is retried on the next response until it succeeds.
	spec2 := testSpec(t)
	spec2.envFile = filepath.Join(t.TempDir(), "late.env")
	ma2 := newManagedApp(spec2.name)
	ma2.spec = spec2
	// …but the app's text is withheld until it reads: the running app's
	// values are unknown, and the heuristics alone are not the promise.
	if out := ma2.door().text("a low-entropy secret"); strings.Contains(string(out), "low-entropy") || !strings.Contains(string(out), "env_file could not be read") {
		t.Fatalf("the app's text sent with the env_file unread: %s", out)
	}
	if err := os.WriteFile(spec2.envFile, []byte("TOKEN=late-but-known-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, _ := ma2.door().r.Redact("saw late-but-known-value"); strings.Contains(out, "late-but-known-value") {
		t.Fatalf("env_file written after the first response was never read: %q", out)
	}
	if out := ma2.door().text("fine now"); out != "fine now" {
		t.Fatalf("the app's text still withheld after the env_file read: %s", out)
	}

	// A value equal to one of the app's own paths, or its name, is a
	// secret like any other value, and the path is no longer a safe
	// string (layer 2): the door's safe list exempts nothing.
	spec3 := testSpec(t)
	spec3.envFile = filepath.Join(t.TempDir(), "paths.env")
	if err := os.WriteFile(spec3.envFile, []byte("APP_ROOT="+spec3.dirs.app+"\nNAME="+spec3.name+"-is-short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ma3 := newManagedApp(spec3.name)
	ma3.spec = spec3
	if out, keys := ma3.door().r.Redact("socket " + spec3.dirs.app + "/run/x/app.sock"); strings.Contains(out, spec3.dirs.app) || strings.Join(keys, ",") != "APP_ROOT" {
		t.Fatalf("a safe string exempted the env_file value equal to it: %q %v", out, keys)
	}
}
