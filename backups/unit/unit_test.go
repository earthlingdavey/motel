package unit

import (
	"context"
	godbus "github.com/godbus/dbus/v5"
	"strings"
	"testing"
	"time"

	sddbus "github.com/coreos/go-systemd/v22/dbus"
)

func validSpec() Spec {
	return Spec{Name: "motel_backup_test_x.service", Argv: []string{"/bin/true"}, User: "motel"}
}

func TestPropertiesRefusesASpecThatIsNotFullySaid(t *testing.T) {
	if _, err := validSpec().properties(); err != nil {
		t.Fatalf("valid spec refused: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Spec)
		want   string
	}{
		{"a name an app's unit could have", func(s *Spec) { s.Name = "motel-backup.1.abc.service" }, "does not match"},
		{"a name with a glob in it", func(s *Spec) { s.Name = "motel_backup_*.service" }, "does not match"},
		{"a relative command", func(s *Spec) { s.Argv = []string{"restic"} }, "absolute path"},
		{"no command", func(s *Spec) { s.Argv = nil }, "absolute path"},
		{"nobody to run as", func(s *Spec) { s.User = "" }, "exactly one of"},
		{"two answers to who runs it", func(s *Spec) { s.AsRoot = true }, "exactly one of"},
		{"a capability inside a user namespace", func(s *Spec) { s.SameUIDNamespaces = true; s.Capabilities = []Capability{CapDACReadSearch} }, "means nothing inside a user namespace"},
		{"a relative bind", func(s *Spec) { s.Binds = []Bind{{Source: "data"}} }, "absolute"},
		{"a relative mask", func(s *Spec) { s.Masked = []string{"app.db"} }, "not absolute"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := validSpec()
			tc.mutate(&s)
			if _, err := s.properties(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

// The properties that must never be absent or spelled the expanding
// way, whatever else a Spec says.
func TestPropertiesAlwaysSandboxAndNeverExpand(t *testing.T) {
	props, err := validSpec().properties()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, p := range props {
		got[p.Name] = p.Value.String()
	}
	if _, ok := got["ExecStart"]; ok {
		t.Error("the command is sent as ExecStart, which the manager expands from the unit's environment")
	}
	if !strings.Contains(got["ExecStartEx"], "no-env-expand") {
		t.Errorf("ExecStartEx = %s, want the no-env-expand flag", got["ExecStartEx"])
	}
	for name, want := range map[string]string{
		"TemporaryFileSystem":   `[("/", "ro",)]`,
		"PrivateNetwork":        "true",
		"NoNewPrivileges":       "true",
		"CapabilityBoundingSet": "@t 0",
		"AmbientCapabilities":   "@t 0",
		"Type":                  `"oneshot"`,
	} {
		if got[name] != want {
			t.Errorf("%s = %s, want %s", name, got[name], want)
		}
	}
	for _, never := range []string{"SuccessExitStatus", "ExecStopPost", "After"} {
		if _, ok := got[never]; ok {
			t.Errorf("%s is set", never)
		}
	}
}

// A unit that may read another account's files by capability is kept
// from reaching a file by handle, outside its view [CVE-2014-5277].
func TestOpenByHandleAtIsDeniedExactlyWhereTheCapabilityIsHeld(t *testing.T) {
	has := func(props []sddbus.Property, name string) (any, bool) {
		for _, p := range props {
			if p.Name == name {
				return p.Value.Value(), true
			}
		}
		return nil, false
	}
	with, err := Spec{Name: "motel_backup_x.service", Argv: []string{"/bin/true"}, User: "u", Capabilities: []Capability{CapDACReadSearch}}.properties()
	if err != nil {
		t.Fatal(err)
	}
	// Typed as the manager takes them: a deny-list of one name, and an
	// errno number — not the unit-file spelling, which the manager refuses.
	filter, ok := has(with, "SystemCallFilter")
	if f, isStruct := filter.(syscallFilter); !ok || !isStruct || f.AllowList || len(f.Names) != 1 || f.Names[0] != "open_by_handle_at" {
		t.Fatalf("a unit with CAP_DAC_READ_SEARCH: %#v", filter)
	}
	if errno, ok := has(with, "SystemCallErrorNumber"); !ok || errno != int32(1) {
		t.Errorf("the denied call kills instead of erroring, or the errno is not a number: %#v", errno)
	}
	// A unit without the capability carries no system-call filter: it
	// would otherwise be a filter to keep working for no reason.
	without, err := Spec{Name: "motel_backup_y.service", Argv: []string{"/bin/true"}, User: "u", Capabilities: []Capability{CapChown}}.properties()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := has(without, "SystemCallFilter"); ok {
		t.Error("a unit with only CAP_CHOWN carries a system-call filter")
	}
}

// A unit's stderr goes to the journal unless the Spec names a file for
// it — never to both names at once, and never to a pipe.
func TestStderrGoesToTheJournalOrToAFile(t *testing.T) {
	find := func(s Spec) map[string]string {
		props, err := s.properties()
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, p := range props {
			if _, twice := got[p.Name]; twice {
				t.Errorf("%s is sent twice", p.Name)
			}
			got[p.Name] = p.Value.String()
		}
		return got
	}
	if got := find(validSpec()); got["StandardError"] != `"journal"` || got["StandardErrorFileToTruncate"] != "" {
		t.Errorf("with no file: %s, %s", got["StandardError"], got["StandardErrorFileToTruncate"])
	}
	s := validSpec()
	s.StderrFile = "/run/x/err"
	if got := find(s); got["StandardErrorFileToTruncate"] != `"/run/x/err"` || got["StandardError"] != "" {
		t.Errorf("with a file: %s, %s", got["StandardError"], got["StandardErrorFileToTruncate"])
	}
}

// The manager's mount unit for a path, as systemd-escape writes it
// [the integration lane holds these to systemd-escape itself].
func TestAMountUnitIsNamedAsTheManagerNamesIt(t *testing.T) {
	for path, want := range map[string]string{
		"/run/motel-backup/0a1b2c3d4e5f/seen":  `run-motel\x2dbackup-0a1b2c3d4e5f-seen.mount`,
		"/run/motel-backup/0a1b2c3d4e5f/seen/": `run-motel\x2dbackup-0a1b2c3d4e5f-seen.mount`,
		"/":                                    "-.mount",
		"/var/lib/a b":                         `var-lib-a\x20b.mount`,
		"/.hidden/x.y":                         `\x2ehidden-x.y.mount`,
		"/srv//data/./disk1":                   "srv-data-disk1.mount",
		"/mnt/é":                               `mnt-\xc3\xa9.mount`,
		"/a_b:c":                               "a_b:c.mount",
	} {
		if got := MountUnit(path); got != want {
			t.Errorf("MountUnit(%q) = %q, want %q", path, got, want)
		}
	}
}

// Sees gives the manager its five seconds, and no more: a manager that
// takes the question and never answers is given up on at the bound
// and said, where the command would otherwise wait for it for good
// (Copilot on #155).
func TestSeesIsBoundedWhenTheManagerDoesNotAnswer(t *testing.T) {
	old := seesWithin
	seesWithin = 200 * time.Millisecond
	t.Cleanup(func() { seesWithin = old })
	r := fakeRunner(&fakeConn{silent: true})
	done := make(chan error, 1)
	go func() { _, err := r.Sees(context.Background(), "/run/motel-backup/0123456789ab/seen"); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "did not answer within") {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Sees waited on a manager that does not answer for three seconds, against a bound of 200ms")
	}
}

// And a manager that answers "no such unit" until the bound is one
// that does not see the mount: not seen, which the command refuses as a
// namespace of its own — never "did not answer". The bound is a second,
// not the 200ms above: the first question must be asked before it runs
// out, and a loaded CI runner has held a goroutine 200ms between arming
// the bound and asking (2026-11-12). The test waits out the whole bound.
func TestSeesSaysNotSeenWhenTheManagerAnswersUntilTheBound(t *testing.T) {
	old := seesWithin
	seesWithin = time.Second
	t.Cleanup(func() { seesWithin = old })
	r := fakeRunner(&fakeConn{readErr: godbus.Error{Name: "org.freedesktop.systemd1.NoSuchUnit"}})
	seen, err := r.Sees(context.Background(), "/run/motel-backup/0123456789ab/seen")
	if err != nil || seen {
		t.Fatalf("seen = %v, err = %v", seen, err)
	}
}
