package liveswap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
)

// A record is written for every deploy — a failure with its detail, a
// success — as the filter left it, and read back whole; the status
// lists each version's outcome newest first.
func TestDeployRecordsAreWrittenAndListed(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.envFile = filepath.Join(t.TempDir(), "app.env")
	must(t, os.WriteFile(rig.spec.envFile, []byte("SECRET=hunter2hunter2hunter2\n"), 0o600))
	rig.runner.runOnceErr = runOnceExit("exit status 3", "u", "failed", 0)
	rig.spec.preStart = []string{"./migrate"}
	rig.spec.deployLogLines = 40
	rig.ma.journal = &fakeJournal{lines: []string{"migrate: SECRET=hunter2hunter2hunter2 refused"}}
	if err := deployOnceV1(t, rig); err == nil {
		t.Fatal("v1 should have failed in pre_start")
	}
	// Dated so the order asserted below is the write time's, not the
	// name tie-break's (which "v2" > "v1" would also satisfy).
	base := time.Now().Add(-time.Hour)
	stampRecord(t, rig.spec.dirs, "v1", base)
	rec, err := readDeployRecord(rig.spec.dirs, "v1")
	if err != nil {
		t.Fatalf("no record for the failed v1: %v", err)
	}
	var got deployResult
	must(t, json.Unmarshal(rec, &got))
	if got.Version != "v1" || got.Status != "failed" || got.Phase != "preparing" || got.Detail == nil || got.Detail.Exit != "exit status 3" {
		t.Fatalf("record = %+v", got)
	}
	if s := string(rec); strings.Contains(s, "hunter2") || !strings.Contains(s, "[redacted:SECRET]") || !strings.Contains(s, `"redacted_env":["SECRET"]`) {
		t.Fatalf("the record must be the filtered result: %s", s)
	}

	rig.runner.runOnceErr = nil
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://example.test/v2.tgz", version: "v2", by: "test"}); err != nil {
		t.Fatal(err)
	}
	stampRecord(t, rig.spec.dirs, "v2", base.Add(time.Minute))
	st := rig.ma.status()
	if len(st.Deploys) != 2 || st.Deploys[0].Version != "v2" || st.Deploys[0].Status != "succeeded" || st.Deploys[1].Version != "v1" || st.Deploys[1].Status != "failed" || st.Deploys[1].Phase != "preparing" {
		t.Fatalf("status.deploys = %+v", st.Deploys)
	}
	if _, err := readDeployRecord(rig.spec.dirs, "v9"); !errors.Is(err, errNoDeployRecord) {
		t.Fatalf("a version never deployed: %v", err)
	}
}

// stampRecord dates version's deploy record. A record's write time is
// its place in the order pruning and the status use, and file times
// are too coarse to tell apart deploys a fake pipeline runs within a
// millisecond; the fake clock moves no file's time. A test that reads
// an order dates its records, a minute apart from a base an hour in
// the past, so that a record a deploy has just written — whose prune
// runs before the test can date it — is newer than every dated one.
func stampRecord(t *testing.T, d appDirs, version string, when time.Time) {
	t.Helper()
	if err := os.Chtimes(deployRecordPath(d.deploys, version), when, when); err != nil {
		t.Fatalf("dating %s's deploy record: %v", version, err)
	}
}

// Records are pruned with the releases: every version still on disk
// keeps its record, and the newest keep others survive.
func TestDeployRecordsArePrunedWithReleases(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.keep = 2
	// Six deploys back to back land within one filesystem timestamp
	// tick, and a tie between records is broken by name, where v1
	// beats every f*. So each record is dated once its deploy returns,
	// a minute after the last from a base an hour back, before the
	// next deploy's prune reads it (stampRecord).
	base, n := time.Now().Add(-time.Hour), 0
	deploy := func(v string, fail bool) {
		if fail {
			rig.runner.startErr = errors.New("boom")
		}
		err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://example.test/" + v, version: v, by: "test"})
		rig.runner.startErr = nil
		if (err != nil) != fail {
			t.Fatalf("%s: fail=%v, err=%v", v, fail, err)
		}
		n++
		stampRecord(t, rig.spec.dirs, v, base.Add(time.Duration(n)*time.Minute))
	}
	deploy("v1", false)
	deploy("f1", true)
	deploy("f2", true)
	deploy("f3", true) // a failed release is removed: f1's record goes, f3 and f2 fill the slots beside v1's
	deploy("v2", false)
	deploy("v3", false) // release GC keeps 2: v3, v2 (v1 pruned)
	var names []string
	for _, d := range rig.ma.status().Deploys {
		names = append(names, d.Version)
	}
	// On disk: v3, v2 → kept. Others by their record's time: f3, f2,
	// v1 → the newest 2 kept; v1's record went with its release.
	if got := strings.Join(names, ","); got != "v3,v2,f3,f2" {
		t.Fatalf("records after pruning = %s", got)
	}
}

func TestPruneDeployRecordsKeepsOnDiskAndNewest(t *testing.T) {
	d := newAppDirs(t.TempDir(), "demo")
	dir := d.deploys
	base := time.Now().Add(-time.Hour)
	for i, v := range []string{"a", "b", "c", "d"} {
		must(t, writeDeployRecord(d, v, []byte(`{"version":"`+v+`","status":"failed"}`)))
		stampRecord(t, d, v, base.Add(time.Duration(i)*time.Minute))
	}
	must(t, os.WriteFile(filepath.Join(dir, ".record-123.tmp"), []byte("{"), 0o600)) // a write that never got its rename
	// Strays hold no retention slot and are removed: a file that is
	// not JSON, a record whose version is not its name, a link.
	must(t, os.WriteFile(filepath.Join(dir, "junk.json"), []byte("{"), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "wrong.json"), []byte(`{"version":"other","status":"failed"}`), 0o600))
	must(t, os.Symlink("a.json", filepath.Join(dir, "link.json")))
	pruneDeployRecords(dir, 1, []string{"a"}, zap.NewNop())
	entries, _ := os.ReadDir(dir)
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if got := strings.Join(left, ","); got != "a.json,d.json" {
		t.Fatalf("left = %s", got)
	}
}

// The records live beside proxy/, outside every instance's view.
func TestDeployRecordsAreOutsideTheSandboxView(t *testing.T) {
	spec := testSpec(t)
	sb := spec.sandboxSpecFor(spec.dirs.release("v1"), recordedNonce)
	if sb.inView(spec.dirs.deploys) || sb.inView(deployRecordPath(spec.dirs.deploys, "v1")) {
		t.Fatal("the deploys dir must not be inside the sandbox view")
	}
}

// A filter that replaced a timestamp-shaped value leaves the record in
// the list: the times are carried raw, and the order is the write time.
func TestDeploySummariesSurviveARedactedTimestamp(t *testing.T) {
	d := newAppDirs(t.TempDir(), "demo")
	must(t, writeDeployRecord(d, "v1", []byte(`{"version":"v1","status":"succeeded","started_at":"[redacted:STAMP]","finished_at":"[redacted:STAMP]"}`)))
	got := listDeploySummaries(d)
	if len(got) != 1 || got[0].Version != "v1" || string(got[0].FinishedAt) != `"[redacted:STAMP]"` {
		t.Fatalf("summaries = %+v", got)
	}
}

// A releases directory that cannot be listed is no reason to prune:
// every record stays, rather than an on-disk release losing its.
func TestDeployRecordsAreNotPrunedWhenReleasesAreUnreadable(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.keep = 1
	for _, v := range []string{"a", "b", "c"} {
		must(t, writeDeployRecord(rig.spec.dirs, v, []byte(`{"version":"`+v+`","status":"failed"}`)))
	}
	must(t, os.RemoveAll(rig.spec.dirs.releases))
	must(t, os.WriteFile(rig.spec.dirs.releases, []byte("not a dir"), 0o600)) // ReadDir fails
	rig.ma.recordDeploy(rig.ma.snapshot(), deployResult{Version: "d", Status: "failed"})
	if got := len(listDeploySummaries(rig.spec.dirs)); got != 4 {
		t.Fatalf("records after an unreadable releases dir = %d, want all 4 kept", got)
	}
}

// A refusal before any phase — re-posting the running version, or a
// version already on disk — writes no record: the record of the deploy
// that put the version there stays as it was.
func TestDeployRecordSurvivesARefusedRepost(t *testing.T) {
	rig := newTestRig(t)
	if err := deployOnceV1(t, rig); err != nil {
		t.Fatal(err)
	}
	before, err := readDeployRecord(rig.spec.dirs, "v1")
	must(t, err)
	err = deployOnceV1(t, rig)
	var ve validationError
	if !errors.As(err, &ve) {
		t.Fatalf("re-posting the running version should be refused: %v", err)
	}
	after, err := readDeployRecord(rig.spec.dirs, "v1")
	must(t, err)
	if string(after) != string(before) || !strings.Contains(string(after), `"status":"succeeded"`) {
		t.Fatalf("the refusal replaced v1's record:\n%s\n%s", before, after)
	}
	if d := rig.ma.status().Deploys; len(d) != 1 || d[0].Status != "succeeded" {
		t.Fatalf("deploys = %+v", d)
	}
}

// What fails before the request is accepted — here the app's
// directories cannot be made — is about the box, not the version, and
// writes no record; a refusal of the request's own content after
// acceptance (an artifact URL the allowlist refuses, a 422 raised
// inside the downloading phase) does not replace a version's record
// either.
func TestDeployRecordIsNotTouchedBeforeAcceptanceOrByARefusal(t *testing.T) {
	rig := newTestRig(t)
	must(t, os.MkdirAll(rig.spec.dirs.app, 0o750))
	must(t, os.RemoveAll(rig.spec.dirs.releases))
	must(t, os.WriteFile(rig.spec.dirs.releases, []byte("not a dir"), 0o600))
	if err := deployOnceV1(t, rig); err == nil {
		t.Fatal("want the box's failure")
	}
	if _, err := readDeployRecord(rig.spec.dirs, "v1"); !errors.Is(err, errNoDeployRecord) {
		t.Fatalf("a failure before acceptance must write no record: %v", err)
	}

	rig = newTestRig(t)
	if err := deployOnceV1(t, rig); err != nil {
		t.Fatal(err)
	}
	must(t, os.RemoveAll(rig.spec.dirs.release("v1"))) // pruned by release GC, say; the record survives
	before, err := readDeployRecord(rig.spec.dirs, "v1")
	must(t, err)
	rig.fetch.err = validationError{"artifact url https://elsewhere.test/a.tgz refused: host not in artifact_allowlist"}
	if err := deployOnceV1(t, rig); err == nil {
		t.Fatal("want the allowlist's refusal")
	}
	after, err := readDeployRecord(rig.spec.dirs, "v1")
	must(t, err)
	if string(after) != string(before) {
		t.Fatalf("a refusal of the request replaced v1's record:\n%s\n%s", before, after)
	}
}

// A link where the records directory should be — whoever made it — is
// not followed: nothing is written through it, read through it, or
// listed from it.
func TestDeployRecordsRefuseAPlantedDirectoryLink(t *testing.T) {
	rig := newTestRig(t)
	must(t, os.MkdirAll(rig.spec.dirs.app, 0o750))
	must(t, os.Symlink("..", rig.spec.dirs.deploys)) // deploys -> the app dir: version "state" would be state.json
	rig.ma.recordDeploy(rig.ma.snapshot(), deployResult{Version: "state", Status: "failed"})
	if _, err := os.Lstat(filepath.Join(rig.spec.dirs.app, "state.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a record was written through the planted link: %v", err)
	}
	if _, err := readDeployRecord(rig.spec.dirs, "state"); err == nil || errors.Is(err, errNoDeployRecord) {
		t.Fatalf("a read through the planted link must be refused outright, got %v", err)
	}
	if got := listDeploySummaries(rig.spec.dirs); got != nil {
		t.Fatalf("listed through the planted link: %+v", got)
	}
}

// A record name that is a link is neither served nor summarised, and a
// record whose version is not the name it sits under is not listed.
func TestDeployRecordsDoNotFollowARecordLinkOrTrustAForgedVersion(t *testing.T) {
	d := newAppDirs(t.TempDir(), "demo")
	dir, err := recordsDir(d, true)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(dir, "other.txt"), []byte(`{"version":"v1","status":"succeeded"}`), 0o600))
	must(t, os.Symlink("other.txt", deployRecordPath(dir, "v1")))
	if _, err := readDeployRecord(d, "v1"); err == nil || errors.Is(err, errNoDeployRecord) {
		t.Fatalf("a linked record must be refused outright, got %v", err)
	}
	must(t, writeDeployRecord(d, "v2", []byte(`{"version":"AKIAFORGEDSECRETVALUE","status":"succeeded"}`)))
	// A version that is not a version, whose path component happens
	// to equal the name it sits under, is not one of ours either.
	must(t, os.WriteFile(filepath.Join(dir, "TOKEN.json"), []byte(`{"version":"../TOKEN","status":"succeeded"}`), 0o600))
	if got := listDeploySummaries(d); len(got) != 0 {
		t.Fatalf("a linked record or a forged version reached the list: %+v", got)
	}
}

// The temp file is created fresh under a random name (O_EXCL), so a
// write leaves no temp file behind and touches nothing planted; a
// planted temp-suffixed link is pruned, its target untouched.
func TestDeployRecordWriteLeavesNoTempFileAndPruneRemovesAPlantedOne(t *testing.T) {
	d := newAppDirs(t.TempDir(), "demo")
	dir, err := recordsDir(d, true)
	must(t, err)
	target := filepath.Join(d.app, "victim.txt") // outside the records dir: prune must not reach it through the link
	must(t, os.WriteFile(target, []byte("precious"), 0o600))
	must(t, os.Symlink("../victim.txt", filepath.Join(dir, ".record-planted.tmp")))
	must(t, writeDeployRecord(d, "v1", []byte(`{"version":"v1","status":"failed"}`)))
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") && e.Name() != ".record-planted.tmp" {
			t.Fatalf("a temp file was left behind: %s", e.Name())
		}
	}
	if b, _ := os.ReadFile(target); string(b) != "precious" {
		t.Fatalf("the planted link's target was rewritten: %q", b)
	}
	pruneDeployRecords(dir, 5, nil, zap.NewNop())
	if _, err := os.Lstat(filepath.Join(dir, ".record-planted.tmp")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the planted temp link was not pruned: %v", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "precious" {
		t.Fatalf("pruning the link touched its target: %q", b)
	}
}

// A record under a version's name that is not that version's — stale,
// planted, corrupted, or not an object at all — is not served as it.
func TestDeployRecordMustNameItsVersion(t *testing.T) {
	d := newAppDirs(t.TempDir(), "demo")
	must(t, writeDeployRecord(d, "v1", []byte(`{"version":"v2","status":"succeeded"}`)))
	must(t, writeDeployRecord(d, "v3", []byte(`["not","an","object"]`)))
	for _, v := range []string{"v1", "v3"} {
		if _, err := readDeployRecord(d, v); err == nil || errors.Is(err, errNoDeployRecord) {
			t.Fatalf("%s: a record that is not the version's must be refused outright, got %v", v, err)
		}
	}
	must(t, writeDeployRecord(d, "v4", []byte(`{"version":"v4","status":"failed"}`)))
	if _, err := readDeployRecord(d, "v4"); err != nil {
		t.Fatal(err)
	}
}

// The record is filtered with the deploy's own spec and the values
// known at the time, never withheld: a reload to an unreadable
// env_file while the deploy ran must not turn its outcome into a
// placeholder nothing can recover.
func TestDeployRecordIsNotWithheldByALaterUnreadableEnvFile(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.envFile = filepath.Join(t.TempDir(), "app.env")
	must(t, os.WriteFile(rig.spec.envFile, []byte("SECRET=hunter2hunter2hunter2\n"), 0o600))
	rig.ma.rememberSecrets(rig.spec.envFile, []string{"SECRET=hunter2hunter2hunter2"})
	c := rig.ma.snapshot() // the deploy's snapshot, env_file readable
	// The reload: the live spec now names an env_file that does not
	// exist, so the response filter would withhold every body.
	live := *rig.spec
	live.envFile = filepath.Join(t.TempDir(), "missing.env")
	rig.ma.spec = &live
	rig.ma.recordDeploy(c, deployResult{Version: "v1", Status: "failed", Error: "pre_start failed: SECRET=hunter2hunter2hunter2 refused"})
	rec, err := readDeployRecord(c.spec.dirs, "v1")
	if err != nil {
		t.Fatalf("the record was withheld or not written: %v", err)
	}
	if s := string(rec); !strings.Contains(s, `"status":"failed"`) || strings.Contains(s, "hunter2") || !strings.Contains(s, "[redacted:SECRET]") {
		t.Fatalf("record = %s", s)
	}
	if w := rig.ma.redactorFor(rig.ma.status()); w.withhold == "" {
		t.Fatal("the test's premise: the live filter withholds while the env_file is unreadable")
	}
}

// An ancestor that is a link — `<root>/blog -> <root>/shop`, an
// operator's mistake — is not followed either: blog's records are
// not written, read or listed from shop's directory.
func TestDeployRecordsRefuseAPlantedAncestorLink(t *testing.T) {
	root := t.TempDir()
	shop := newAppDirs(root, "shop")
	must(t, os.MkdirAll(shop.app, 0o750))
	must(t, os.Symlink("shop", filepath.Join(root, "blog")))
	blog := newAppDirs(root, "blog")
	if _, err := recordsDir(blog, true); err == nil {
		t.Fatal("blog's deploys dir resolves into shop's and must be refused")
	}
	// Refused before anything was created through the link: shop has
	// no deploys dir that blog's request made for it.
	if _, err := os.Lstat(shop.deploys); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("blog's refused request created shop's deploys dir: %v", err)
	}
	must(t, writeDeployRecord(shop, "v1", []byte(`{"version":"v1","status":"succeeded"}`)))
	if err := writeDeployRecord(blog, "v9", []byte(`{"version":"v9","status":"failed"}`)); err == nil {
		t.Fatal("a write through the ancestor link must be refused")
	}
	if _, err := os.Lstat(deployRecordPath(shop.deploys, "v9")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("blog's record landed in shop's directory")
	}
	if _, err := readDeployRecord(blog, "v1"); err == nil || errors.Is(err, errNoDeployRecord) {
		t.Fatalf("a read through the ancestor link must be refused outright, got %v", err)
	}
	if got := listDeploySummaries(blog); got != nil {
		t.Fatalf("shop's records listed as blog's: %+v", got)
	}
	// An alias on the root itself is the one difference allowed.
	alias := filepath.Join(t.TempDir(), "alias")
	must(t, os.Symlink(root, alias))
	if _, err := recordsDir(newAppDirs(alias, "shop"), true); err != nil {
		t.Fatalf("a symlinked root is a legitimate layout: %v", err)
	}
	// And a root that is not on disk yet, under a linked ancestor,
	// resolves as far as it can and is made where it should be.
	linked := filepath.Join(t.TempDir(), "var")
	must(t, os.Symlink(root, linked))
	fresh := newAppDirs(filepath.Join(linked, "not-yet", "liveswap"), "news")
	if _, err := recordsDir(fresh, true); err != nil {
		t.Fatalf("a not-yet-existing root under a linked ancestor: %v", err)
	}
}

// A record planted under a name equal to a known env value must not
// let that value through the response filter: the status keeps the
// value redacted even though the same string is a "version".
func TestARecordedVersionEqualToAKnownValueIsStillRedacted(t *testing.T) {
	rig := newTestRig(t)
	secret := "q7Wm2xK9pL4vB8nR3tY6zH5c" // gitleaks:allow — a made-up value for this test
	rig.spec.envFile = filepath.Join(t.TempDir(), "app.env")
	must(t, os.WriteFile(rig.spec.envFile, []byte("TOKEN="+secret+"\n"), 0o600))
	must(t, writeDeployRecord(rig.spec.dirs, secret, []byte(`{"version":"`+secret+`","status":"succeeded"}`)))
	if err := deployOnceV1(t, rig); err != nil {
		t.Fatal(err)
	}
	s := rig.ma.status()
	if len(s.Deploys) == 0 {
		t.Fatal("the planted record is listed (its version matches its name); the filter is what must catch it")
	}
	raw, err := json.Marshal(s)
	must(t, err)
	body := rig.ma.redactorFor(s).redactJSON(raw)
	if strings.Contains(body, secret) || !strings.Contains(body, "[redacted:TOKEN]") {
		t.Fatalf("the known value leaked through a recorded version: %s", body)
	}
}

// A release directory named after a known env value — plantable while
// the app dir was writable — does not exempt that value from the
// record's filter.
func TestRecordFilterDoesNotTrustAReleaseNameEqualToAKnownValue(t *testing.T) {
	rig := newTestRig(t)
	secret := "q7Wm2xK9pL4vB8nR3tY6zH5c" // gitleaks:allow — a made-up value for this test
	rig.ma.rememberSecrets("/etc/app.env", []string{"TOKEN=" + secret})
	must(t, os.MkdirAll(rig.spec.dirs.release(secret), 0o755))
	rd, _ := rig.ma.recordRedactor(rig.ma.snapshot(), deployResult{Version: "v1"})
	rec := rd.redactJSON([]byte(`{"version":"v1","error":"got ` + secret + `"}`))
	if strings.Contains(rec, secret) || !strings.Contains(rec, "[redacted:TOKEN]") {
		t.Fatalf("a planted release name exempted the value: %s", rec)
	}
}

// A deploy that failed before its launch remembered no env_file
// values, and its error can still carry one: the record's filter
// reads the deploy's own env_file, so the value is not written.
func TestDeployRecordFiltersACurrentValueBeforeAnyLaunch(t *testing.T) {
	rig := newTestRig(t)
	secret := "q7Wm2xK9pL4vB8nR3tY6zH5c" // gitleaks:allow — a made-up value for this test
	rig.spec.envFile = filepath.Join(t.TempDir(), "app.env")
	must(t, os.WriteFile(rig.spec.envFile, []byte("TOKEN="+secret+"\n"), 0o600))
	rig.fetch.err = errors.New("download https://x/a.tgz?sig=" + secret + ": HTTP 403")
	if err := deployOnceV1(t, rig); err == nil {
		t.Fatal("the download should have failed")
	}
	rec, err := readDeployRecord(rig.spec.dirs, "v1")
	must(t, err)
	// The value is gone and the key reported; which layer took it (the
	// query-string rule reaches a URL's query before the marker does)
	// is not the point.
	if s := string(rec); strings.Contains(s, secret) || !strings.Contains(s, `"redacted_env":["TOKEN"]`) {
		t.Fatalf("a current value reached the record of a pre-launch failure: %s", s)
	}
}

// A known value that is a fragment of the record's own JSON makes the
// filter withhold the whole body; the record is then an envelope that
// still names the version and status, readable and listed — and made
// only of values that cannot carry source data, so a second known
// value that the result held (here the deployer label) is not in it.
func TestDeployRecordSurvivesTheFiltersWholeBodyFallback(t *testing.T) {
	rig := newTestRig(t)
	label := "local:/shared/deploy-2f7e9c1a4b.pub"
	rig.ma.rememberSecrets("/etc/app.env", []string{`WEIRD="status":"failed","error"`, "LABEL=" + label})
	rig.runner.startErr = errors.New("boom")
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://example.test/v1.tgz", version: "v1", by: label}); err == nil {
		t.Fatal("v1 should have failed")
	}
	rec, err := readDeployRecord(rig.spec.dirs, "v1")
	if err != nil {
		t.Fatalf("the record must be readable as v1's: %v", err)
	}
	if s := string(rec); !strings.Contains(s, `"version":"v1"`) || !strings.Contains(s, `"status":"failed"`) || !strings.Contains(s, "record withheld") || strings.Contains(s, "boom") || strings.Contains(s, label) {
		t.Fatalf("envelope = %s", s)
	}
	if d := rig.ma.status().Deploys; len(d) != 1 || d[0].Version != "v1" || d[0].Status != "failed" {
		t.Fatalf("deploys = %+v", d)
	}
}

// A release directory named after a known value — off the filesystem,
// like a record's name — does not exempt that value from the response
// filter either.
func TestAnOnDiskVersionEqualToAKnownValueIsStillRedacted(t *testing.T) {
	rig := newTestRig(t)
	secret := "q7Wm2xK9pL4vB8nR3tY6zH5c" // gitleaks:allow — a made-up value for this test
	rig.ma.rememberSecrets("/etc/app.env", []string{"TOKEN=" + secret})
	must(t, os.MkdirAll(rig.spec.dirs.release(secret), 0o755))
	s := rig.ma.status()
	raw, err := json.Marshal(s)
	must(t, err)
	if body := rig.ma.redactorFor(s).redactJSON(raw); strings.Contains(body, secret) || !strings.Contains(body, "[redacted:TOKEN]") {
		t.Fatalf("a planted release name exempted the value: %s", body)
	}
}

// A FIFO where a record should be must not hold the open — and with
// it the status, and the deploy lock: it is not a regular file, and
// the open does not block on it.
func TestDeployRecordsDoNotBlockOnAPlantedFIFO(t *testing.T) {
	d := newAppDirs(t.TempDir(), "demo")
	dir, err := recordsDir(d, true)
	must(t, err)
	must(t, syscall.Mkfifo(filepath.Join(dir, "v1.json"), 0o600))
	must(t, writeDeployRecord(d, "v2", []byte(`{"version":"v2","status":"succeeded"}`)))
	done := make(chan struct{})
	go func() {
		defer close(done)
		if got := listDeploySummaries(d); len(got) != 1 || got[0].Version != "v2" {
			t.Errorf("list with a FIFO beside a record: %+v", got)
		}
		if _, err := readDeployRecord(d, "v1"); err == nil || errors.Is(err, errNoDeployRecord) {
			t.Errorf("a FIFO under a record's name must be refused outright, got %v", err)
		}
		pruneDeployRecords(dir, 5, nil, zap.NewNop())
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a planted FIFO blocked the record store")
	}
}

// A status poll makes nothing: an app never deployed has no deploys
// directory after GET /<app>, and no record is a 404, not a directory.
func TestStatusDoesNotCreateTheRecordsDir(t *testing.T) {
	rig := newTestRig(t)
	if d := rig.ma.status().Deploys; d != nil {
		t.Fatalf("deploys before any deploy: %+v", d)
	}
	if _, err := readDeployRecord(rig.spec.dirs, "v1"); !errors.Is(err, errNoDeployRecord) {
		t.Fatalf("no store yet: want errNoDeployRecord, got %v", err)
	}
	if _, err := os.Lstat(rig.spec.dirs.deploys); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a read created the records dir: %v", err)
	}
}

// A known value equal to a word of the outcome vocabulary — a secret
// spelled "succeeded", a phase's name — does not rewrite a record's
// status or phase, on disk or when read back; the record's other text
// is still filtered.
func TestDeployRecordOutcomeSurvivesAVocabularySecret(t *testing.T) {
	rig := newTestRig(t)
	rig.ma.rememberSecrets("/etc/app.env", []string{"WORD=succeeded", "STEP=preparing"})
	if err := deployOnceV1(t, rig); err != nil {
		t.Fatal(err)
	}
	rec, err := readDeployRecord(rig.spec.dirs, "v1")
	must(t, err)
	if !strings.Contains(string(rec), `"status":"succeeded"`) {
		t.Fatalf("status rewritten on disk: %s", rec)
	}
	base := time.Now().Add(-time.Hour) // dated as in TestDeployRecordsAreWrittenAndListed
	stampRecord(t, rig.spec.dirs, "v1", base)
	rig.spec.preStart = []string{"./migrate"}
	rig.runner.runOnceErr = runOnceExit("exit status 1", "u", "failed", 0)
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://example.test/v2.tgz", version: "v2", by: "test"}); err == nil {
		t.Fatal("v2 should have failed")
	}
	stampRecord(t, rig.spec.dirs, "v2", base.Add(time.Minute))
	rec, err = readDeployRecord(rig.spec.dirs, "v2")
	must(t, err)
	if s := string(rec); !strings.Contains(s, `"status":"failed"`) || !strings.Contains(s, `"phase":"preparing"`) {
		t.Fatalf("outcome rewritten on disk: %s", s)
	}
	d := rig.ma.status().Deploys
	if len(d) != 2 || d[0].Status != "failed" || d[0].Phase != "preparing" || d[1].Status != "succeeded" {
		t.Fatalf("summaries = %+v", d)
	}
	// Read back through the live filter: the words stand outside it
	// there too, and the same word as text does not.
	served := rig.ma.redactorFor(rig.ma.status()).redactJSON(rec)
	if !strings.Contains(served, `"status":"failed"`) || !strings.Contains(served, `"phase":"preparing"`) || !strings.Contains(served, `"name":"preparing"`) {
		t.Fatalf("outcome rewritten when served: %s", served)
	}
	rec = []byte(`{"version":"v2","status":"failed","phase":"preparing","error":"preparing: migrate failed"}`)
	if served := rig.ma.redactorFor(rig.ma.status()).redactJSON(rec); !strings.Contains(served, `"error":"[redacted:STEP]: migrate failed"`) {
		t.Fatalf("a vocabulary word as text was not filtered: %s", served)
	}
}

// A version named exactly as an env_file value is redacted like the
// value (redact.go, rule 1), so its record could not name itself: it
// is written as the envelope, which says why, and is listed and served
// like any record — with the version redacted where the filter sees it.
func TestDeployRecordOfAVersionEqualToAValueIsAnEnvelope(t *testing.T) {
	rig := newTestRig(t)
	rig.ma.rememberSecrets("/etc/app.env", []string{"RELEASE=2026.09.14"})
	rig.runner.startErr = errors.New("boom")
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://example.test/r.tgz", version: "2026.09.14", by: "test"}); err == nil {
		t.Fatal("the deploy should have failed")
	}
	rec, err := readDeployRecord(rig.spec.dirs, "2026.09.14")
	must(t, err)
	if s := string(rec); !strings.Contains(s, `"version":"2026.09.14"`) || !strings.Contains(s, "record withheld: the version equals an env_file value") || strings.Contains(s, "boom") {
		t.Fatalf("record = %s", s)
	}
	if d := rig.ma.status().Deploys; len(d) != 1 || d[0].Version != "2026.09.14" || d[0].Status != "failed" {
		t.Fatalf("deploys = %+v", d)
	}
	if served := rig.ma.redactorFor(rig.ma.status()).redactJSON(rec); strings.Contains(served, "2026.09.14") || !strings.Contains(served, `"version":"[redacted:RELEASE]"`) {
		t.Fatalf("served = %s", served)
	}
}

// A version equal to a value too short to be a secret is redacted
// nowhere, and its record is a record, not the envelope.
func TestDeployRecordOfAVersionEqualToAShortValueIsWhole(t *testing.T) {
	rig := newTestRig(t)
	rig.ma.rememberSecrets("/etc/app.env", []string{"VERSION=v7"})
	rig.runner.startErr = errors.New("boom")
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://example.test/v7.tgz", version: "v7", by: "test"}); err == nil {
		t.Fatal("the deploy should have failed")
	}
	rec, err := readDeployRecord(rig.spec.dirs, "v7")
	must(t, err)
	if s := string(rec); !strings.Contains(s, `"version":"v7"`) || !strings.Contains(s, "boom") || strings.Contains(s, "record withheld") {
		t.Fatalf("record = %s", s)
	}
}

// A record's times are carried into the status body as the record has
// them, so they must be strings: a planted record whose time is an
// object is not a record (rule 1), or it could put structure of its
// own choosing into every status response.
func TestDeployRecordTimesMustBeStrings(t *testing.T) {
	d := newAppDirs(t.TempDir(), "demo")
	must(t, writeDeployRecord(d, "v1", []byte(`{"version":"v1","status":"succeeded","started_at":{"name":"succeeded"},"finished_at":"2026-09-14T00:00:00Z"}`)))
	must(t, writeDeployRecord(d, "v2", []byte(`{"version":"v2","status":"succeeded","finished_at":["x"]}`)))
	must(t, writeDeployRecord(d, "v3", []byte(`{"version":"v3","status":"succeeded","finished_at":"[redacted:STAMP]"}`)))
	must(t, writeDeployRecord(d, "v4", []byte(`{"version":"v4","status":"succeeded","started_at":null}`)))
	if got := listDeploySummaries(d); len(got) != 1 || got[0].Version != "v3" {
		t.Fatalf("summaries = %+v", got)
	}
}

// A record the filter leaves larger than a reader accepts is written
// as an envelope, readable, rather than as a file every reader refuses.
func TestDeployRecordTooLargeIsAnEnvelope(t *testing.T) {
	t.Parallel()
	rig := newTestRig(t)
	rig.runner.startErr = errors.New("boom: " + strings.Repeat("x", 2*deployRecordMaxBytes))
	if err := deployOnceV1(t, rig); err == nil {
		t.Fatal("v1 should have failed")
	}
	rec, err := readDeployRecord(rig.spec.dirs, "v1")
	if err != nil {
		t.Fatalf("an oversized outcome must still leave a readable record: %v", err)
	}
	if s := string(rec); !strings.Contains(s, `"version":"v1"`) || !strings.Contains(s, `"status":"failed"`) || !strings.Contains(s, "larger than a record can be") || len(s) > 1024 {
		t.Fatalf("envelope = %.200s… (%d bytes)", s, len(s))
	}
}

// A file under a version's name whose outcome is not vocabulary — no
// status, an invented phase — is not a record: not served, not listed.
func TestDeployRecordMustHaveAVocabularyOutcome(t *testing.T) {
	d := newAppDirs(t.TempDir(), "demo")
	must(t, writeDeployRecord(d, "v1", []byte(`{"version":"v1"}`)))
	must(t, writeDeployRecord(d, "v2", []byte(`{"version":"v2","status":"failed","phase":"idle"}`)))
	must(t, writeDeployRecord(d, "v3", []byte(`{"version":"v3","status":"failed"}`)))
	must(t, writeDeployRecord(d, "v4", []byte(`{"version":"v4","status":"succeeded","phase":"starting"}`))) // a success reached no failing phase
	for _, v := range []string{"v1", "v2", "v4"} {
		if _, err := readDeployRecord(d, v); err == nil || errors.Is(err, errNoDeployRecord) {
			t.Fatalf("%s: not vocabulary, must be refused outright, got %v", v, err)
		}
	}
	if got := listDeploySummaries(d); len(got) != 1 || got[0].Version != "v3" {
		t.Fatalf("listed = %+v", got)
	}
}

// A rollback that fails before its first phase records no phase: the
// app's "idle" is not a deploy's phase, and a record must be
// vocabulary to be read back.
func TestDeployRecordOfAPrePhaseFailureHasNoPhase(t *testing.T) {
	rig := newTestRig(t)
	if err := deployOnceV1(t, rig); err != nil {
		t.Fatal(err)
	}
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://example.test/v2.tgz", version: "v2", by: "test"}); err != nil {
		t.Fatal(err)
	}
	rig.spec.envFile = filepath.Join(t.TempDir(), "missing.env") // prepareLaunch fails before any phase
	err := rig.ma.Deploy(context.Background(), deployRequest{version: "v1", rollback: true, by: "test"})
	if err == nil {
		t.Fatal("the rollback should have failed reading its env_file")
	}
	rec, rerr := readDeployRecord(rig.spec.dirs, "v1")
	if rerr != nil {
		t.Fatalf("the failed rollback's record must be readable: %v", rerr)
	}
	if s := string(rec); !strings.Contains(s, `"status":"failed"`) || strings.Contains(s, `"phase":`) {
		t.Fatalf("record = %s", s)
	}
	if ld := rig.ma.status().LastDeploy; ld.Phase != "" {
		t.Fatalf("last_deploy.phase = %q for a failure before any phase", ld.Phase)
	}
}

// The size bound counts the newline the file ends with: a filtered
// record of exactly the bound is an envelope, not a file one byte too
// long for every reader.
func TestDeployRecordSizeBoundCountsTheNewline(t *testing.T) {
	t.Parallel()
	rig := newTestRig(t)
	// Pad the error so the filtered JSON lands on the bound exactly.
	probe := deployResult{Version: "v1", Status: "failed", By: "test", Error: ""}
	base, err := json.Marshal(probe)
	must(t, err)
	rig.runner.startErr = errors.New(strings.Repeat("x", deployRecordMaxBytes-len(base)-len("start failed: ")))
	if err := deployOnceV1(t, rig); err == nil {
		t.Fatal("v1 should have failed")
	}
	if _, err := readDeployRecord(rig.spec.dirs, "v1"); err != nil {
		t.Fatalf("a record at the bound must be readable (as itself or as the envelope): %v", err)
	}
}

// An env_file that cannot be parsed whole leaves the values before the
// bad line unknown to the filter — and the error that says so can
// carry one; the record is then the envelope, never the error.
func TestDeployRecordIsAnEnvelopeWhenTheEnvFileWillNotParseWhole(t *testing.T) {
	rig := newTestRig(t)
	secret := "q7Wm2xK9pL4vB8nR3tY6zH5c" // gitleaks:allow — a made-up value for this test
	rig.spec.envFile = filepath.Join(t.TempDir(), "app.env")
	must(t, os.WriteFile(rig.spec.envFile, []byte("TOKEN="+secret+"\n"+secret+"-=x\n"), 0o600))
	err := deployOnceV1(t, rig)
	if err == nil || !strings.Contains(err.Error(), secret) {
		t.Fatalf("the premise: the deploy fails on the bad line and the error names it: %v", err)
	}
	rec, rerr := readDeployRecord(rig.spec.dirs, "v1")
	must(t, rerr)
	if s := string(rec); strings.Contains(s, secret) || !strings.Contains(s, "record withheld") || !strings.Contains(s, `"status":"failed"`) {
		t.Fatalf("record = %s", s)
	}
}

// With the env_file unread past a bad line, the values are not all
// known, so an envelope cannot tell a pin from one of them: it names
// none, even one that sits before the bad line.
func TestDeployRecordEnvelopeDropsThePinWhenValuesAreUnknown(t *testing.T) {
	rig := newTestRig(t)
	sum := sha256.Sum256([]byte("a digest that is also a secret"))
	pin := hex.EncodeToString(sum[:])
	rig.spec.envFile = filepath.Join(t.TempDir(), "app.env")
	must(t, os.WriteFile(rig.spec.envFile, []byte("TOKEN="+pin+"\nnot a line\n"), 0o600))
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://example.test/v1.tgz", version: "v1", sha256: pin, by: "test"}); err == nil {
		t.Fatal("v1 should have failed on the bad line")
	}
	rec, err := readDeployRecord(rig.spec.dirs, "v1")
	must(t, err)
	if s := string(rec); strings.Contains(s, pin) || !strings.Contains(s, "record withheld") {
		t.Fatalf("record = %s", s)
	}
}

// An envelope keeps the pin, so a withheld record still says what the
// deploy was pinned to and a rollback can carry it — unless the pin
// equals a known value or holds one, which no envelope names
// (redact.go, rule 1).
func TestDeployRecordEnvelopeKeepsThePin(t *testing.T) {
	t.Parallel()
	sum := sha256.Sum256([]byte("what CI built"))
	pin := hex.EncodeToString(sum[:])
	for _, tc := range []struct {
		name, env string
		want      bool
	}{
		{"no known value", "", true},
		{"pin is a known value", "TOKEN=" + pin + "\n", false},
		{"pin holds a known value", "TOKEN=" + pin[10:40] + "\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newTestRig(t)
			rig.spec.envFile = filepath.Join(t.TempDir(), "app.env")
			must(t, os.WriteFile(rig.spec.envFile, []byte(tc.env), 0o600))
			rig.runner.startErr = errors.New("boom: " + strings.Repeat("x", 2*deployRecordMaxBytes))
			if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://example.test/v1.tgz", version: "v1", sha256: pin, by: "test"}); err == nil {
				t.Fatal("v1 should have failed")
			}
			rec, err := readDeployRecord(rig.spec.dirs, "v1")
			must(t, err)
			if s := string(rec); !strings.Contains(s, "larger than a record can be") || strings.Contains(s, pin) != tc.want {
				t.Fatalf("envelope names the pin = %v, want %v: %s", strings.Contains(s, pin), tc.want, s)
			}
			if got := recordedPin(rig.spec.dirs, "v1"); (got == pin) != tc.want {
				t.Fatalf("recordedPin = %q, want the pin %v", got, tc.want)
			}
		})
	}
}

// A pin equal to an env_file value is no name (redact.go, rule 1): the
// record and the status keep the value redacted, the record stays in
// the list with the placeholder where the pin was, and a rollback,
// which can read no digest there, records the version as unpinned.
func TestAPinEqualToAKnownValueIsStillRedacted(t *testing.T) {
	rig := newTestRig(t)
	sum := sha256.Sum256([]byte("a digest that is also a secret"))
	pin := hex.EncodeToString(sum[:])
	rig.spec.envFile = filepath.Join(t.TempDir(), "app.env")
	must(t, os.WriteFile(rig.spec.envFile, []byte("TOKEN="+pin+"\n"), 0o600))
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://example.test/v1.tgz", version: "v1", sha256: pin, by: "test"}); err != nil {
		t.Fatal(err)
	}
	rec, err := readDeployRecord(rig.spec.dirs, "v1")
	must(t, err)
	if s := string(rec); strings.Contains(s, pin) || !strings.Contains(s, `"sha256":"[redacted:TOKEN]"`) {
		t.Fatalf("the record must redact a pin equal to a known value: %s", s)
	}
	s := rig.ma.status()
	if len(s.Deploys) != 1 || string(s.Deploys[0].SHA256) != `"[redacted:TOKEN]"` {
		t.Fatalf("the record must stay listed with its redacted pin: %+v", s.Deploys)
	}
	raw, err := json.Marshal(s)
	must(t, err)
	if body := rig.ma.redactorFor(s).redactJSON(raw); strings.Contains(body, pin) {
		t.Fatalf("the known value leaked through a pin: %s", body)
	}
	if got := recordedPin(rig.spec.dirs, "v1"); got != "" {
		t.Fatalf("recordedPin = %q, want none: the record's pin is a placeholder", got)
	}
}

// A record is read off disk, so its sha256 field names nothing until
// it is a digest: a planted string of another shape is not exempt from
// the filter's heuristics, and is no pin for a rollback to carry; a
// pin that is not a string at all is not a record.
func TestARecordedPinMustBeADigest(t *testing.T) {
	rig := newTestRig(t)
	token := "Zq7Wm2xK9pL4vB8nR3tY6zH5cJ1dF0gS" // gitleaks:allow — a made-up value for this test
	must(t, writeDeployRecord(rig.spec.dirs, "v8", []byte(`{"version":"v8","status":"succeeded","sha256":"`+token+`"}`)))
	must(t, writeDeployRecord(rig.spec.dirs, "v9", []byte(`{"version":"v9","status":"succeeded","sha256":1}`)))
	s := rig.ma.status()
	if len(s.Deploys) != 1 || s.Deploys[0].Version != "v8" {
		t.Fatalf("only v8 is a record: %+v", s.Deploys)
	}
	raw, err := json.Marshal(s)
	must(t, err)
	if body := rig.ma.redactorFor(s).redactJSON(raw); strings.Contains(body, token) {
		t.Fatalf("a planted non-digest pin was exempted from the filter: %s", body)
	}
	if got := recordedPin(rig.spec.dirs, "v8"); got != "" {
		t.Fatalf("recordedPin = %q, want none", got)
	}
	// The writer records lowercase; an upper-case digest is no pin.
	upper := strings.Repeat("AB", 32)
	must(t, writeDeployRecord(rig.spec.dirs, "v7", []byte(`{"version":"v7","status":"succeeded","sha256":"`+upper+`"}`)))
	if got := recordedPin(rig.spec.dirs, "v7"); got != "" {
		t.Fatalf("recordedPin = %q, want none", got)
	}
}
