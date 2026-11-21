package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smallhoursorg/motel/backups/envfile"
	"github.com/smallhoursorg/motel/backups/unit"
)

// setup --existing: the operator's word that a repository is there, a
// rebuilt box's. No password is made or shown on any path; the look
// has setup's longer clock, and the repository's own password is asked
// for; where the storage says there is none, or has not answered at
// that clock, setup ends with the working file as it was.

// With --existing no init runs: nothing answers under setupClock
// beside it, so it is short here, for the tests that wait it out.
func (b *box) setupExisting(t *testing.T, m *term, repo string) (*SetupReport, error) {
	t.Helper()
	clockMet()
	return Setup(t.Context(), b.cfg, b, SetupOptions{Repository: repo, Existing: true, Terminal: m})
}

// A retry line of restic's, as the look's clock would cut it off
// [measured: restic 0.18.0's shape, a 503 from the storage], and the
// stop's own words after it, which say nothing of the storage: the
// signal, the opening it ended, and the two lines after every failed
// opening [measured: the e2e's wrong key].
const lookRetrying = "Load(<config/>, 0, 0) returned error, retrying after 552.330144ms: client.GetObject: 503 Slow Down\nsignal terminated received, cleaning up\nFatal: unable to open config file: Stat: context canceled\nIs there a repository at the following location?\ns3:http://e2e-s3:9000/box\n"

// The same stop with no retry line before it: nothing is quoted — the
// URL least of all, in restic's own spelling of it, which need not be
// the operator's, and wherever the signal's line lands among the
// three restic writes together.
const lookStoppedSilent = "Fatal: unable to open config file: Stat: context canceled\nIs there a repository at the following location?\nsignal terminated received, cleaning up\ns3:http://e2e-s3:9000/box/\n"

// onProbe runs f before the nth probe unit of the setup.
func (b *box) onProbe(n int, f func()) {
	probes := 0
	b.before = func(s unit.Spec) {
		if strings.Contains(s.Name, "_probe_") {
			if probes++; probes == n {
				f()
			}
		}
	}
}

// The look found the repository: as without the flag, under the
// longer clock.
func TestExistingOpensTheRepositoryTheLookFound(t *testing.T) {
	b, m := setupBox(t)
	b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
	m.answers = []string{"AKIDX", "the-secret", "its-own-password"}
	rep, err := b.setupExisting(t, m, "s3:http://e2e-s3:9000/box")
	if err != nil {
		t.Fatalf("%v\nsaid:\n%s", err, m.saidAll())
	}
	if got, want := b.roles(), "plan probe open"; got != want {
		t.Fatalf("units: %s, want %s", got, want)
	}
	if got, want := m.askedAll(), "Storage key id (AWS_ACCESS_KEY_ID): \nsecret:Storage secret key (AWS_SECRET_ACCESS_KEY): \nsecret:Repository password: "; got != want {
		t.Fatalf("asked:\n%s", got)
	}
	if !strings.Contains(m.saidAll(), "looking for a repository at s3:http://e2e-s3:9000/box (up to 200ms)") || !strings.Contains(m.saidAll(), "the repository exists; its password is needed") || strings.Contains(m.saidAll(), "shown above") {
		t.Fatalf("said:\n%s", m.saidAll())
	}
	noPasswordMade(t, m)
	if v := envOf(t, b.cfg.EnvFile); v["RESTIC_PASSWORD"] != "its-own-password" {
		t.Fatalf("the file holds password %q", v["RESTIC_PASSWORD"])
	}
	if rep.New || rep.RepositoryID != repoID || !strings.Contains(m.saidAll(), "repository ready: s3:http://e2e-s3:9000/box (existing, id bbd0e899)") {
		t.Fatalf("report %+v\nsaid:\n%s", rep, m.saidAll())
	}
	// Without the flag the look has its short clock, as before.
	b, m = setupBox(t)
	b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
	m.answers = []string{"AKIDX", "the-secret", "its-own-password"}
	if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err != nil || !strings.Contains(m.saidAll(), "(up to 100ms)") {
		t.Fatalf("%v\nsaid:\n%s", err, m.saidAll())
	}
}

// The look could not tell at setup's longer clock — a wrong secret the
// storage retries, a bucket not there, a host that does not resolve, a
// storage slow for two minutes — or exited 0 saying nothing: said,
// with restic's last words where the clock cut it off, and the key is
// asked for again, as for one refused at once; the third ends setup
// naming the way out. No password was made or asked for, and the look
// only reads, so nothing is said of a lock. No init runs.
func TestExistingEndsWhereTheLookCannotTell(t *testing.T) {
	const alike = "a bucket not made yet, a wrong key and a wrong host look alike here; with --existing no password is made; check the URL and the key against the earlier box's, and run setup again; nothing of setup's has been written"
	for _, tc := range []struct {
		name string
		set  func(b *box)
		err  string
	}{
		{"the look is given up on at its clock", func(b *box) { b.hang = "probe"; b.probeErr = lookRetrying },
			"no repository answered within 200ms (restic said: Load(<config/>, 0, 0) returned error, retrying after 552.330144ms: client.GetObject: 503 Slow Down): " + alike},
		{"the look is given up on at its clock, restic saying only the stop's words", func(b *box) { b.hang = "probe"; b.probeErr = lookStoppedSilent },
			"no repository answered within 200ms: " + alike},
		{"the look exits 0 with nothing", func(b *box) { b.outcome["probe"] = unit.Outcome{Result: "success"} },
			"no repository answered within 200ms: " + alike},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, m := setupBox(t)
			tc.set(b)
			m.answers = []string{"AKIDX", "wrong", "AKIDX", "wrong", "AKIDX", "wrong", "its-own-password"}
			_, err := b.setupExisting(t, m, "s3:http://e2e-s3:9000/box")
			if err == nil || err.Error() != tc.err {
				t.Fatalf("err %v\nwant %s\nsaid:\n%s", err, tc.err, m.saidAll())
			}
			if got, want := b.roles(), "plan probe probe probe"; got != want {
				t.Fatalf("units: %s, want %s", got, want)
			}
			key := "Storage key id (AWS_ACCESS_KEY_ID): \nsecret:Storage secret key (AWS_SECRET_ACCESS_KEY): "
			if got, want := m.askedAll(), key+"\n"+key+"\n"+key; got != want {
				t.Fatalf("asked:\n%s", got)
			}
			// Said twice before the third, with the key asked for again
			// each time, and with --existing's own words, not the new
			// repository's.
			firstTwo, _, _ := strings.Cut(tc.err, "; check the URL")
			if strings.Count(m.saidAll(), firstTwo) != 2 || strings.Count(m.saidAll(), "the key id and secret again: a wrong key is one of the three; the same key, if the storage was only slow to answer, has the look run again") != 2 || strings.Contains(m.saidAll(), "refused the key") || strings.Contains(m.saidAll(), "still applies") {
				t.Fatalf("said:\n%s", m.saidAll())
			}
			// The stop's own words are not the storage's, and are not
			// quoted; nor is the new-repository path spoken of.
			all := m.saidAll() + "\n" + err.Error()
			if strings.Contains(all, "signal terminated") || strings.Contains(all, "restic said: s3:") || strings.Contains(all, "a new repository will be made") || strings.Contains(all, "lock") {
				t.Fatalf("said:\n%s", all)
			}
			noPasswordMade(t, m)
			nothingOfSetups(t, b)
		})
	}
}

// The key asked for again after a clock answers: the look then finds
// the repository, and it is opened with its own password.
func TestExistingOpensTheRepositoryTheSecondKeyFinds(t *testing.T) {
	b, m := setupBox(t)
	b.hang = "probe"
	b.probeErr = lookRetrying
	b.onProbe(2, func() {
		b.hang, b.probeErr = "", ""
		b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
	})
	m.answers = []string{"AKIDX", "wrong", "AKIDX", "the-secret", "its-own-password"}
	rep, err := b.setupExisting(t, m, "s3:http://e2e-s3:9000/box")
	if err != nil {
		t.Fatalf("%v\nsaid:\n%s", err, m.saidAll())
	}
	if got, want := b.roles(), "plan probe probe open"; got != want {
		t.Fatalf("units: %s, want %s", got, want)
	}
	if !strings.Contains(m.saidAll(), "no repository answered within 200ms (restic said: Load(<config/>, 0, 0) returned error, retrying after 552.330144ms: client.GetObject: 503 Slow Down): a bucket not made yet, a wrong key and a wrong host look alike here; with --existing no password is made") || !strings.Contains(m.saidAll(), "the repository exists; its password is needed") {
		t.Fatalf("said:\n%s", m.saidAll())
	}
	noPasswordMade(t, m)
	if v := envOf(t, b.cfg.EnvFile); v["RESTIC_PASSWORD"] != "its-own-password" || v["AWS_SECRET_ACCESS_KEY"] != "the-secret" || rep.New || rep.RepositoryID != repoID {
		t.Fatalf("the file holds %v; report %+v", v, rep)
	}
}

// The storage took the key and says there is no repository (exit 10):
// setup ends, naming the URL and the way out, with nothing written and
// no password made or asked for.
func TestExistingIsRefusedWhereTheStorageSaysThereIsNone(t *testing.T) {
	b, m := setupBox(t)
	m.answers = []string{"AKIDX", "the-secret", "its-own-password"}
	_, err := b.setupExisting(t, m, "s3:http://e2e-s3:9000/box")
	want := "there is no repository at s3:http://e2e-s3:9000/box (exit 10): setup --existing opens a repository from an earlier box and makes none; check the URL — the bucket and the path — against the earlier box's, or run setup without --existing to make a new repository there"
	if err == nil || err.Error() != want {
		t.Fatalf("err %v\nwant %s\nsaid:\n%s", err, want, m.saidAll())
	}
	if got, want := b.roles(), "plan probe"; got != want {
		t.Fatalf("units: %s, want %s", got, want)
	}
	if got, want := m.askedAll(), "Storage key id (AWS_ACCESS_KEY_ID): \nsecret:Storage secret key (AWS_SECRET_ACCESS_KEY): "; got != want {
		t.Fatalf("asked:\n%s", got)
	}
	noPasswordMade(t, m)
	nothingOfSetups(t, b)
}

// The opening is as without the flag: its clock is its own, and exit
// 10 there after a look that found the repository — a storage
// contradicting itself — is said as any other failure, not as "run
// setup without --existing", which would have init made over what the
// look found.
func TestExistingLeavesTheOpeningAsItIs(t *testing.T) {
	b, m := setupBox(t)
	b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
	b.outcome["open"] = unit.Outcome{Result: "exit-code", ExitStatus: 10}
	m.answers = []string{"AKIDX", "the-secret", "its-own-password"}
	_, err := b.setupExisting(t, m, "s3:http://e2e-s3:9000/box")
	if err == nil || err.Error() != "there is no repository at the configured location (exit 10)" {
		t.Fatalf("err %v\nsaid:\n%s", err, m.saidAll())
	}
	noPasswordMade(t, m)
	if _, err := os.Lstat(b.cfg.EnvFile); err == nil {
		t.Fatal("the credential file was written")
	}
	b, m = setupBox(t)
	b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
	b.hang = "open"
	// The opening's stderr at its clock has the same shape as the look's:
	// a retry line, then the stop's own and the hint with the URL; the
	// retry line is what is quoted, as for the look.
	b.openErr = "Save(<lock/8f2a1c>) returned error, retrying after 1.2s: client.PutObject: 503 Slow Down\nsignal terminated received, cleaning up\nFatal: unable to open repository at s3:http://e2e-s3:9000/box: context canceled\nIs there a repository at the following location?\ns3:http://e2e-s3:9000/box/\n"
	m.answers = []string{"AKIDX", "the-secret", "its-own-password"}
	_, err = b.setupExisting(t, m, "s3:http://e2e-s3:9000/box")
	if err == nil || !strings.HasPrefix(err.Error(), "restic had not finished opening the repository with its lock within 200ms (restic said: Save(<lock/8f2a1c>) returned error, retrying after 1.2s: client.PutObject: 503 Slow Down); ") {
		t.Fatalf("err %v", err)
	}
}

// A key the storage refuses at the look is asked for again, with the
// flag as without it.
func TestExistingAsksForARefusedKeyAgain(t *testing.T) {
	b, m := setupBox(t)
	b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
	b.probeErr = refusedAtOnce
	b.onProbe(2, func() { b.outcome["probe"], b.probeErr = unit.Outcome{Result: "exit-code", ExitStatus: 12}, "" })
	m.answers = []string{"AKIDX", "wrong", "AKIDX", "the-secret", "its-own-password"}
	rep, err := b.setupExisting(t, m, b2Repo)
	if err != nil {
		t.Fatalf("%v\nsaid:\n%s", err, m.saidAll())
	}
	if got, want := b.roles(), "plan probe probe open"; got != want {
		t.Fatalf("units: %s, want %s", got, want)
	}
	if !strings.Contains(m.saidAll(), refusedSaid) || !strings.Contains(m.saidAll(), "the storage refused the key, or could not be reached: the key id and secret again") || strings.Contains(m.saidAll(), "still applies") {
		t.Fatalf("said:\n%s", m.saidAll())
	}
	noPasswordMade(t, m)
	if rep.New || rep.RepositoryID != repoID {
		t.Fatalf("report %+v", rep)
	}
	// Three refusals: the ending says, with the flag, what to check and
	// that nothing was written, as the clock's ending does.
	b, m = setupBox(t)
	b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
	b.probeErr = refusedAtOnce
	m.answers = []string{"AKIDX", "wrong", "AKIDX", "wrong", "AKIDX", "wrong"}
	_, err = b.setupExisting(t, m, b2Repo)
	if err == nil || err.Error() != refusedSaid+"; check the URL and the key against the earlier box's, and run setup again; nothing of setup's has been written" {
		t.Fatalf("err %v", err)
	}
	if got, want := b.roles(), "plan probe probe probe"; got != want {
		t.Fatalf("units: %s, want %s", got, want)
	}
	nothingOfSetups(t, b)
	// Without the flag, the third refusal ends as before.
	b, m = setupBox(t)
	b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
	b.probeErr = refusedAtOnce
	m.answers = []string{"AKIDX", "wrong", "AKIDX", "wrong", "AKIDX", "wrong"}
	if _, err = b.setup(t, m, b2Repo); err == nil || err.Error() != refusedSaid {
		t.Fatalf("err %v", err)
	}
}

// Without the flag, a look cut off at its clock quotes restic's last
// words before the stop's own, and the rest is as before: a password
// is made, to be stored beside any kept, and init answers.
func TestAClockedLookQuotesResticsLastWords(t *testing.T) {
	b, m := setupBox(t)
	b.hang = "probe"
	b.probeErr = lookRetrying
	rep, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err != nil {
		t.Fatalf("%v\nsaid:\n%s", err, m.saidAll())
	}
	want := "no repository answered within 100ms: a bucket not made yet, a wrong key and a wrong host look alike here; a new repository will be made with the password shown next (restic said: Load(<config/>, 0, 0) returned error, retrying after 552.330144ms: client.GetObject: 503 Slow Down)"
	if !strings.Contains(m.saidAll(), want) || strings.Contains(m.saidAll(), "signal terminated") || !strings.Contains(m.saidAll(), unsureStore) {
		t.Fatalf("said lacks %q:\n%s", want, m.saidAll())
	}
	if got, want := b.roles(), "plan probe init open"; got != want || !rep.New {
		t.Fatalf("units: %s, want %s; report %+v", got, want, rep)
	}
}

// noPasswordMade: nothing of the new-repository path was said — no
// password shown, no `stored` asked for, no word of one's fate.
func noPasswordMade(t *testing.T, m *term) {
	t.Helper()
	all := m.saidAll() + "\n" + m.askedAll()
	if len(passwordRe.FindAllString(all, -1)) != 0 || strings.Contains(all, "stored") || strings.Contains(all, "Repository password (new)") || strings.Contains(all, "password shown") {
		t.Fatalf("a password was made, or spoken of:\n%s", all)
	}
}

// nothingOfSetups: no working file, nothing of setup's beside it (the
// staged file, or what a Write on the way to either leaves: the two
// shapes envfile names), and no unit directory of setup's own left
// under the run directory (the lock file and the units directory are
// the run's).
func nothingOfSetups(t *testing.T, b *box) {
	t.Helper()
	if _, err := os.Lstat(b.cfg.EnvFile); err == nil {
		t.Fatal("the credential file was written")
	}
	beside, _ := os.ReadDir(filepath.Dir(b.cfg.EnvFile))
	for _, e := range beside {
		if envfile.IsLeftover(e.Name(), filepath.Base(b.cfg.EnvFile)) {
			t.Fatalf("left beside the working file: %s", e.Name())
		}
	}
	entries, _ := os.ReadDir(b.cfg.RunDir)
	for _, e := range entries {
		if e.IsDir() && e.Name() != "units" {
			t.Fatalf("left in the run directory: %v", entries)
		}
	}
}
