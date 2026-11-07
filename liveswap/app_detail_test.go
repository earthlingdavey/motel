package liveswap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// fakeJournal scripts what the app's units wrote.
type fakeJournal struct {
	lines []string
	err   error
	calls int
	of    tailOf
	since time.Time
	n     int
	// behind is how many reads find nothing yet: a journal that has
	// not caught up with the process.
	behind int
}

func (j *fakeJournal) tail(_ context.Context, of tailOf, since time.Time, n int) ([]string, error) {
	j.calls++
	j.of, j.since, j.n = of, since, n
	if j.calls <= j.behind {
		return nil, j.err
	}
	return j.lines, j.err
}

func deployOnceV1(t *testing.T, rig *testRig) error {
	t.Helper()
	return rig.ma.Deploy(context.Background(), deployRequest{url: "https://example.test/v1.tgz", version: "v1", by: "test"})
}

// A pre_start that exits non-zero: the response carries its exit
// status, the phases up to "preparing", and the last lines its unit
// wrote, and nothing about a health probe.
func TestFailureDetailPreStartExit(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.preStart = []string{"./migrate"}
	rig.spec.deployLogLines = 40
	rig.runner.runOnceErr = runOnceExit("exit status 3", "motel-demo.v1.x.prestart.service", "failed", 731)
	j := &fakeJournal{lines: []string{"migrate: schema v1 -> v2", "migrate: cannot open app.db"}}
	rig.ma.journal = j

	if err := deployOnceV1(t, rig); err == nil {
		t.Fatal("deploy should have failed in pre_start")
	}
	ld := rig.ma.status().LastDeploy
	if ld == nil || ld.Status != "failed" || ld.Phase != "preparing" {
		t.Fatalf("last deploy = %+v", ld)
	}
	if ld.Detail == nil || ld.Detail.Exit != "exit status 3" || ld.Detail.Probe != nil {
		t.Fatalf("detail = %+v", ld.Detail)
	}
	if strings.Join(ld.Detail.LogTail, "|") != "migrate: schema v1 -> v2|migrate: cannot open app.db" || ld.Detail.LogTailTruncated {
		t.Fatalf("log tail = %v (truncated %v)", ld.Detail.LogTail, ld.Detail.LogTailTruncated)
	}
	// Asked twice: once, and once more to see the count stop growing.
	if j.calls != 2 || j.n != 40 || len(j.of.units) != 2 || !strings.HasSuffix(j.of.units[0], ".prestart.service") || !strings.HasSuffix(j.of.units[1], ".service") {
		t.Fatalf("journal asked %d times for %d lines of %v", j.calls, j.n, j.of.units)
	}
	if !j.since.Equal(ld.StartedAt) {
		t.Fatalf("journal since %v, deploy started %v", j.since, ld.StartedAt)
	}
	// And by the pre_start's main process, the pid the manager recorded,
	// under the app's identifier: journald keeps no unit on a line it
	// reads after its writer has gone.
	if j.of.pid != 731 || j.of.ident != "motel-demo" {
		t.Fatalf("journal asked by pid %d under %q, want 731 under motel-demo", j.of.pid, j.of.ident)
	}
	var names []string
	for _, p := range ld.Phases {
		names = append(names, p.Name)
	}
	if got := strings.Join(names, ","); got != "downloading,extracting,preparing" {
		t.Fatalf("phases = %s", got)
	}
}

// An app that dies before it is healthy: the exit comes from the
// runner, since no error carried it.
func TestFailureDetailAppDiesOnStart(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.deployLogLines = 40
	rig.runner.startDies = "killed by signal 9 (killed)"
	rig.prober.err = &healthGateError{err: errProcessExited, probe: &probeError{status: 500, body: "booting"}}
	j := &fakeJournal{lines: []string{"fatal: SOCKET not set"}}
	rig.ma.journal = j

	if err := deployOnceV1(t, rig); err == nil {
		t.Fatal("deploy should have failed the health gate")
	}
	ld := rig.ma.status().LastDeploy
	// The exit is the app's own, and the probe it answered before it
	// died is kept alongside.
	if ld.Detail == nil || ld.Detail.Exit != "killed by signal 9 (killed)" || ld.Detail.Probe == nil || ld.Detail.Probe.Status != 500 {
		t.Fatalf("detail = %+v", ld.Detail)
	}
	if len(ld.Detail.LogTail) != 1 || ld.Detail.LogTail[0] != "fatal: SOCKET not set" {
		t.Fatalf("log tail = %v", ld.Detail.LogTail)
	}
	// The journal is asked by the pid the app's main process had.
	if j.of.pid != 4242 {
		t.Fatalf("journal asked by pid %d, want the app's 4242", j.of.pid)
	}
}

// A gate that fails before any launch — the sweep that precedes
// pre_start could not be confirmed — has no units to ask about: no
// detail, and the journal is never run.
func TestFailureDetailNoneBeforeLaunch(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.deployLogLines = 40
	rig.spec.preStart = []string{"./migrate"}
	rig.runner.sweepErr = errors.New("manager unreachable")
	j := &fakeJournal{lines: []string{"never"}}
	rig.ma.journal = j
	if err := deployOnceV1(t, rig); err == nil {
		t.Fatal("deploy should have failed before pre_start")
	}
	if d := rig.ma.status().LastDeploy.Detail; d != nil || j.calls != 0 {
		t.Fatalf("detail before any launch: %+v (journal calls %d)", d, j.calls)
	}
}

// A journal behind the process: the first reads find nothing, and the
// tail is asked for again rather than given up as empty.
func TestFailureDetailWaitsForAJournalBehind(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.preStart = []string{"./migrate"}
	rig.spec.deployLogLines = 40
	rig.runner.runOnceErr = runOnceExit("exit status 3", "u", "failed", 731)
	j := &fakeJournal{lines: []string{"migrate: cannot open app.db"}, behind: 2}
	rig.ma.journal = j
	if err := deployOnceV1(t, rig); err == nil {
		t.Fatal("deploy should have failed in pre_start")
	}
	if d := rig.ma.status().LastDeploy.Detail; d == nil || strings.Join(d.LogTail, "|") != "migrate: cannot open app.db" {
		t.Fatalf("a journal two reads behind: detail %+v after %d reads", d, j.calls)
	}
}

// More lines than deploy_log_lines: the tail is the last N and says
// there was more; a single oversized line is clipped, not dropped.
func TestFailureDetailCaps(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.deployLogLines = 2
	rig.spec.preStart = []string{"./migrate"}
	rig.runner.runOnceErr = runOnceExit("exit status 1", "u", "failed", 0)
	rig.ma.journal = &fakeJournal{lines: []string{"one", "two", "three"}} // journalctl asked for 3, returned 3
	if err := deployOnceV1(t, rig); err == nil {
		t.Fatal("deploy should have failed")
	}
	d := rig.ma.status().LastDeploy.Detail
	if strings.Join(d.LogTail, ",") != "two,three" || !d.LogTailTruncated {
		t.Fatalf("capped tail = %v (truncated %v)", d.LogTail, d.LogTailTruncated)
	}
}

// A health gate that never passes: the last probe's status, redirect
// target and body excerpt are in the detail; the process is alive, so
// there is no exit.
func TestFailureDetailProbe(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.deployLogLines = 40
	rig.prober.err = fmt.Errorf("not healthy within deadline 5m: %w", &probeError{status: 400, location: "", body: "Invalid HTTP_HOST header: 'localhost'."})
	j := &fakeJournal{}
	rig.ma.journal = j

	if err := deployOnceV1(t, rig); err == nil {
		t.Fatal("deploy should have failed the health gate")
	}
	ld := rig.ma.status().LastDeploy
	// Still running when the gate gave up on it: the journal is asked by
	// the pid it runs as, all the same.
	if j.of.pid != 4242 {
		t.Fatalf("journal asked by pid %d, want the instance's 4242", j.of.pid)
	}
	if ld.Detail == nil || ld.Detail.Probe == nil || ld.Detail.Probe.Status != 400 || !strings.Contains(ld.Detail.Probe.Body, "HTTP_HOST") {
		t.Fatalf("detail = %+v", ld.Detail)
	}
	if ld.Detail.Exit != "" {
		t.Fatalf("a stopped-by-motel instance must not report an exit as the cause: %q", ld.Detail.Exit)
	}
	if ld.Detail.LogTail != nil || ld.Detail.LogTailError != "" {
		t.Fatalf("an empty journal is no tail and no error: %+v", ld.Detail)
	}
}

// The known values are replaced before the caps cut: a probe body whose
// secret straddles the excerpt's end, and a journal line whose secret
// straddles the byte cap's cut, carry the marker, never a fragment the
// response filter could not match.
func TestFailureDetailCutsAfterTheKnownValues(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.deployLogLines = 40
	const secret = "k3J9xQ2mZ8pL0vT7wR4nY6bH1cD5fGhJ7kLmN9pQ" // gitleaks:allow
	envFile := filepath.Join(t.TempDir(), "app.env")
	must(t, os.WriteFile(envFile, []byte("API_TOKEN="+secret+"\n"), 0o600))
	rig.spec.envFile = envFile
	// The secret begins 20 bytes before the excerpt's end.
	body := strings.Repeat("x", probeBodyExcerpt-20) + secret + strings.Repeat("y", 200)
	rig.prober.err = fmt.Errorf("not healthy within deadline 5m: %w", &probeError{status: 500, body: body})
	// One line over the byte cap, its secret 16 bytes before the cut.
	line := strings.Repeat("a", 100) + secret + strings.Repeat("b", deployLogMaxBytes-20)
	rig.ma.journal = &fakeJournal{lines: []string{line}}

	if err := deployOnceV1(t, rig); err == nil {
		t.Fatal("deploy should have failed the health gate")
	}
	d := rig.ma.status().LastDeploy.Detail
	if d == nil || d.Probe == nil || len(d.Probe.Body) > probeBodyExcerpt || !strings.Contains(d.Probe.Body, "[redacted:API_TOKEN]") {
		t.Fatalf("probe body = %q, want the marker within %d bytes", d.Probe.Body, probeBodyExcerpt)
	}
	if len(d.LogTail) != 1 || !d.LogTailTruncated || !strings.HasPrefix(d.LogTail[0], "…") {
		t.Fatalf("tail = %v (truncated %v), want the one clipped line", d.LogTail, d.LogTailTruncated)
	}
	for i := 0; i+8 <= len(secret); i++ {
		if frag := secret[i : i+8]; strings.Contains(d.Probe.Body, frag) || strings.Contains(d.LogTail[0], frag) {
			t.Fatalf("a fragment of the secret, %q, survived the cut: body %q, tail %q", frag, d.Probe.Body, d.LogTail[0])
		}
	}
}

// The values held to are the launch's own, so an env_file that went
// unreadable after the deploy rendered it costs the detail nothing, and
// the keys found come back for the result's redacted_env.
func TestFailureDetailHoldsTheLaunchsOwnValues(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.deployLogLines = 40
	rig.spec.envFile = filepath.Join(t.TempDir(), "gone.env")
	rig.ma.journal = &fakeJournal{lines: []string{"token hunter2hunter2hunter2 refused"}}
	err := fmt.Errorf("not healthy within deadline 5m: %w", &probeError{status: 500, body: "bad token hunter2hunter2hunter2"})
	l := launch{secrets: []string{"API_TOKEN=hunter2hunter2hunter2"}} // gitleaks:allow
	d, keys := rig.ma.failureDetail(rig.ma.snapshot(), rig.spec, err, "exit status 3", 0, l, time.Now())
	if d == nil || d.Probe == nil || d.Probe.Body != "bad token [redacted:API_TOKEN]" || d.Exit != "exit status 3" || d.LogTailError != "" {
		t.Fatalf("detail = %+v, want the launch's value replaced and nothing withheld", d)
	}
	if strings.Join(keys, ",") != "API_TOKEN" {
		t.Fatalf("keys = %v, want the one found", keys)
	}
}

// The token a read bound cut in two goes, with an ellipsis in its
// place: the first of a kept suffix, the last of a kept prefix.
func TestDropUnfinished(t *testing.T) {
	for _, tc := range []struct {
		in      string
		atStart bool
		want    string
	}{
		{"cret=hunter2 then more\nline two", true, "… then more\nline two"},
		{"nowhitespaceatall", true, "…"},
		{"line one\nstarts a sec", false, "line one\nstarts a …"},
		{"nowhitespaceatall", false, "…"},
		{"ends with a space ", false, "ends with a space …"},
	} {
		if got := dropUnfinished(tc.in, tc.atStart); got != tc.want {
			t.Errorf("dropUnfinished(%q, %v) = %q, want %q", tc.in, tc.atStart, got, tc.want)
		}
	}
}

// The journal's read keeps one byte more than its limit, so an output
// of exactly the limit is known whole and keeps its first token; only a
// longer one, cut, loses it.
func TestBoundedTail(t *testing.T) {
	for _, tc := range []struct {
		in    string
		limit int
		want  string
	}{
		{"abc def\n", 8, "abc def"},
		{"abc def\n", 7, "… def"},
		{"abc def", 7, "abc def"},
		{"ab\n", 8, "ab"},
		{"", 8, ""},
	} {
		if got := boundedTail([]byte(tc.in), tc.limit); got != tc.want {
			t.Errorf("boundedTail(%q, %d) = %q, want %q", tc.in, tc.limit, got, tc.want)
		}
	}
}

// deploy_log_lines 0 asks the journal for nothing; a journal that
// cannot be read says so instead of a tail; and a success carries no
// detail at all.
func TestFailureDetailSwitches(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.deployLogLines = 0
	rig.spec.preStart = []string{"./migrate"}
	rig.runner.runOnceErr = runOnceExit("exit status 1", "u", "failed", 0)
	j := &fakeJournal{lines: []string{"never read"}}
	rig.ma.journal = j
	if err := deployOnceV1(t, rig); err == nil {
		t.Fatal("deploy should have failed")
	}
	if d := rig.ma.status().LastDeploy.Detail; j.calls != 0 || d == nil || d.LogTail != nil || d.Exit != "exit status 1" {
		t.Fatalf("with deploy_log_lines 0: journal calls %d, detail %+v", j.calls, d)
	}

	rig = newTestRig(t)
	rig.spec.deployLogLines = 40
	rig.spec.preStart = []string{"./migrate"}
	rig.runner.runOnceErr = runOnceExit("exit status 1", "u", "failed", 0)
	rig.ma.journal = &fakeJournal{err: errors.New("journalctl: not found")}
	if err := deployOnceV1(t, rig); err == nil {
		t.Fatal("deploy should have failed")
	}
	if d := rig.ma.status().LastDeploy.Detail; d == nil || d.LogTailError != "journalctl: not found" || d.LogTail != nil {
		t.Fatalf("unreadable journal: %+v", d)
	}

	rig = newTestRig(t)
	rig.spec.deployLogLines = 40
	rig.ma.journal = &fakeJournal{lines: []string{"hello"}}
	if err := deployOnceV1(t, rig); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	ld := rig.ma.status().LastDeploy
	if ld.Detail != nil {
		t.Fatalf("a success carries no detail: %+v", ld.Detail)
	}
	if len(ld.Phases) == 0 || ld.Phases[len(ld.Phases)-1].Name != "promoting" && ld.Phases[len(ld.Phases)-1].Name != "stopping_old" {
		t.Fatalf("phases = %+v", ld.Phases)
	}
}

// The tail's two runs, argument by argument: the streams are learnt
// from the lines that name the launch — by a unit, or by a main
// process's pid under the app's identifier — and the tail is the units'
// lines and every line of those streams.
func TestJournalArgs(t *testing.T) {
	since := time.Unix(1790000000, 0)
	window := "--user --no-pager --quiet -n 41 --since @1789999999"
	units := "_SYSTEMD_USER_UNIT=a.prestart.service _SYSTEMD_USER_UNIT=a.service"
	of := tailOf{units: []string{"a.prestart.service", "a.service"}, pid: 731, ident: "motel-demo"}
	for _, c := range []struct {
		name      string
		got, want []string
	}{
		{"streams, by unit or by pid", streamArgs(of, since, 40), strings.Fields(window + " -o cat --output-fields=_STREAM_ID " + units + " + _PID=731 SYSLOG_IDENTIFIER=motel-demo")},
		{"streams, no pid recorded", streamArgs(tailOf{units: of.units, ident: of.ident}, since, 40), strings.Fields(window + " -o cat --output-fields=_STREAM_ID " + units)},
		{"streams, no identifier to go with the pid", streamArgs(tailOf{units: of.units, pid: 731}, since, 40), strings.Fields(window + " -o cat --output-fields=_STREAM_ID " + units)},
		{"tail, the units or their streams", tailArgs(of.units, []string{"0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"}, since, 40), strings.Fields(window + " -o cat " + units + " + _STREAM_ID=0123456789abcdef0123456789abcdef _STREAM_ID=fedcba9876543210fedcba9876543210")},
		{"tail, no stream learnt", tailArgs(of.units, nil, since, 40), strings.Fields(window + " -o cat " + units)},
	} {
		if strings.Join(c.got, " ") != strings.Join(c.want, " ") {
			t.Errorf("%s:\n got %q\nwant %q", c.name, c.got, c.want)
		}
	}
}

// The stream ids are read off journalctl's output, one an entry, each
// kept once, in the order met; what is not one as journald prints one
// is passed over.
func TestStreamIDs(t *testing.T) {
	a, b := "0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"
	out := strings.Join([]string{
		"89abcdef0123456789abcdef", // the first, its start cut by the byte bound
		a,
		"", // an entry with no stream
		b,
		a, // met again
		"0123456789ABCDEF0123456789ABCDEF",
		"x _PID=1",
		"",
	}, "\n")
	if got := strings.Join(streamIDs([]byte(out)), ","); got != a+","+b {
		t.Fatalf("stream ids = %q, want %q", got, a+","+b)
	}
}

func TestCapTail(t *testing.T) {
	lines := []string{"a", "bb", "ccc", "dddd"}
	if got, tr := capTail(lines, 2, 1000); strings.Join(got, ",") != "ccc,dddd" || !tr {
		t.Errorf("line cap: %v %v", got, tr)
	}
	if got, tr := capTail(lines, 10, 9); strings.Join(got, ",") != "ccc,dddd" || !tr { // 4+1 + 3+1 = 9 fits; 2+1 more does not
		t.Errorf("byte cap: %v %v", got, tr)
	}
	if got, tr := capTail(lines, 10, 1000); len(got) != 4 || tr {
		t.Errorf("no cap: %v %v", got, tr)
	}
	if got, tr := capTail(nil, 10, 1000); got != nil || tr {
		t.Errorf("empty: %v %v", got, tr)
	}
	if got, tr := capTail([]string{strings.Repeat("x", 100)}, 10, 50); len(got) != 1 || !strings.HasPrefix(got[0], "…") || len(got[0]) > 50 || !tr {
		t.Errorf("one line over the byte cap is clipped, not dropped: %v %v", got, tr)
	}
	if got, tr := capTail([]string{"short", strings.Repeat("y", 100)}, 10, 50); len(got) != 1 || !strings.HasPrefix(got[0], "…") || !tr {
		t.Errorf("an oversized last line after others: %v %v", got, tr)
	}
}

// deploy_log_lines 0 keeps the app's output on the box: the probe's
// status and redirect target are reported, its body is not.
func TestFailureDetailZeroKeepsProbeBodyOnTheBox(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.deployLogLines = 0
	rig.prober.err = fmt.Errorf("not healthy within deadline 5m: %w", &probeError{status: 500, body: "Traceback (most recent call last): secret things"})
	rig.ma.journal = &fakeJournal{lines: []string{"never read"}}
	if err := deployOnceV1(t, rig); err == nil {
		t.Fatal("deploy should have failed the health gate")
	}
	d := rig.ma.status().LastDeploy.Detail
	if d == nil || d.Probe == nil || d.Probe.Status != 500 || d.Probe.Body != "" {
		t.Fatalf("with deploy_log_lines 0 the probe body stays on the box: %+v", d)
	}
}

// tailWriter keeps the last max bytes whatever is written, in one
// write or many, and never more.
func TestTailWriter(t *testing.T) {
	w := &tailWriter{max: 8}
	for _, s := range []string{"abc", "def", "ghi"} {
		if n, err := w.Write([]byte(s)); n != 3 || err != nil {
			t.Fatalf("write %q: %d %v", s, n, err)
		}
	}
	if string(w.buf) != "bcdefghi" {
		t.Fatalf("after three writes: %q", w.buf)
	}
	if _, err := w.Write([]byte("0123456789ab")); err != nil || string(w.buf) != "456789ab" {
		t.Fatalf("one write over max: %q %v", w.buf, err)
	}
	if _, err := w.Write([]byte("xyz")); err != nil || string(w.buf) != "789abxyz" {
		t.Fatalf("a write after that: %q %v", w.buf, err)
	}
}

// The clipped oversized line is cut on a rune boundary.
func TestCapTailClipsOnRuneBoundary(t *testing.T) {
	line := strings.Repeat("é", 100) // 200 bytes, every one inside a 2-byte rune
	got, tr := capTail([]string{line}, 10, 50)
	if !tr || len(got) != 1 || !utf8.ValidString(got[0]) || len(got[0]) > 50 {
		t.Fatalf("clipped line must be valid UTF-8 within the cap: %q %v", got, tr)
	}
}
