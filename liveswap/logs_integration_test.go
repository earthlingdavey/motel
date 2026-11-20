//go:build integration

package liveswap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// These run the logs reader (logs.go) against a real journal: units
// started by motel's own runner (r.Start, the whole sandbox) on root's
// user manager in the dev-systemd container, so the uid the selection
// holds to is 0 here and motel's on a box. Each app's name is new in
// each run; every wait is a poll of at most ten seconds; no count that
// journald decides by its timing is asserted, but where the timing is
// made (journald paused).

// logsApp is scriptApp under the app's name, its version and a fresh
// nonce, as each launch of an app has.
func logsApp(t *testing.T, app, version, body string) startSpec {
	t.Helper()
	spec := scriptApp(t, body)
	spec.app, spec.version = app, version
	n, err := newNonce()
	if err != nil {
		t.Fatal(err)
	}
	spec.nonce = n
	spec.env = []string{"SOCKET=/var/lib/liveswap/" + app + "/run/" + n + ".sock", "PATH=" + os.Getenv("PATH")}
	return spec
}

// runToEnd starts spec and waits until its unit is gone.
func runToEnd(t *testing.T, r *systemdRunner, spec startSpec) {
	t.Helper()
	h, err := r.Start(spec)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case <-r.Wait(h):
	case <-time.After(15 * time.Second):
		_ = r.Stop(h, time.Second)
		t.Fatal("the unit did not end")
	}
}

// logsName is an app name no earlier run used.
func logsName(prefix string) string {
	return prefix + strconv.FormatInt(time.Now().UnixNano()%1e10, 36)
}

// readAppLogs is one read of app's logs, as the route makes it.
func readAppLogs(t *testing.T, app string, n int, after string) logsRead {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), journalTailTimeout)
	defer cancel()
	read, err := readLogs(ctx, journalctlLogs{}, logsQuery{app: app, ident: syslogIdentifier(app), uid: os.Getuid(), n: n, after: after})
	if err != nil {
		t.Fatalf("reading %s's logs: %v", app, err)
	}
	return read
}

// waitLogs tails app's logs until it has at least want lines.
func waitLogs(t *testing.T, app string, want int) logsRead {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		read := readAppLogs(t, app, 1000, "")
		if len(read.lines) >= want {
			return read
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %d lines, want %d: %q", app, len(read.lines), want, texts(read.lines))
		}
	}
}

// journalFields is every entry the journal holds of match, each with
// the fields a measurement names, as journalctl prints them.
func journalFields(t *testing.T, match ...string) []map[string]any {
	t.Helper()
	cmd := exec.Command(journalctlPath, append([]string{"--user", "--no-pager", "--quiet", "-o", "json", "--all",
		"--output-fields=MESSAGE,SYSLOG_IDENTIFIER,_UID,_TRANSPORT,_SYSTEMD_UNIT,_SYSTEMD_USER_UNIT,_LINE_BREAK,PRIORITY"}, match...)...)
	cmd.Env = []string{}
	out, err := cmd.Output()
	var ee *exec.ExitError
	if err != nil && (!errors.As(err, &ee) || ee.ExitCode() != 1 || len(out) != 0) {
		t.Fatalf("journalctl %v: %v", match, err)
	}
	var all []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("not an entry: %q", line)
		}
		all = append(all, m)
	}
	return all
}

func field(m map[string]any, k string) string { s, _ := m[k].(string); return s }

// TestIntegrationLogsReadsTheAppsOwnLines: a real unit's lines, its own
// and a short-lived child's, through the real reader with an empty
// environment: their text, a time, the release from the unit when
// journald kept it, a cursor journalCursor takes (the grammar's own
// measurement on a real journal), and the unit fields every line
// carries, which the reader holds to the app's (belongsTo): the user
// unit one of the app's, the system unit the user manager's.
func TestIntegrationLogsReadsTheAppsOwnLines(t *testing.T) {
	r := integrationRunner(t)
	app := logsName("itlogs-r")
	before := time.Now().Add(-time.Second)
	runToEnd(t, r, logsApp(t, app, "v1", "echo first line\necho on stderr >&2\necho a child | sed 's/^/through /'\necho last line\nsleep 1\n"))
	read := waitLogs(t, app, 4)
	if got := strings.Join(texts(read.lines), "|"); got != "first line|on stderr|through a child|last line" {
		t.Fatalf("the lines: %q", got)
	}
	for _, l := range read.lines {
		if l.at.Before(before) || l.at.After(time.Now()) {
			t.Errorf("%q: at %v", l.text, l.at)
		}
		if !journalCursor.re.MatchString(l.cursor) {
			t.Errorf("%q: a cursor the key's grammar refuses: %q", l.text, l.cursor)
		}
		if rel, ok := unitRelease(l.userUnit, app); l.userUnit != "" && (!ok || rel != "v1") {
			t.Errorf("%q: unit %q, release %q", l.text, l.userUnit, rel)
		}
	}
	// The measurement the unit-field net stands on.
	for _, m := range journalFields(t, "SYSLOG_IDENTIFIER="+syslogIdentifier(app)) {
		t.Logf("an app's line: %q _UID=%s _TRANSPORT=%s _SYSTEMD_UNIT=%q _SYSTEMD_USER_UNIT=%q", field(m, "MESSAGE"), field(m, "_UID"), field(m, "_TRANSPORT"), field(m, "_SYSTEMD_UNIT"), field(m, "_SYSTEMD_USER_UNIT"))
		if u := field(m, "_SYSTEMD_UNIT"); u != "" && u != "user@"+strconv.Itoa(os.Getuid())+".service" {
			t.Errorf("%q: _SYSTEMD_UNIT %q, not the user manager's", field(m, "MESSAGE"), u)
		}
		if u := field(m, "_SYSTEMD_USER_UNIT"); u != "" && !unitBelongsTo(u, app) {
			t.Errorf("%q: _SYSTEMD_USER_UNIT %q, not the app's", field(m, "MESSAGE"), u)
		}
	}
	// The tail's cursor follows to nothing; an app with no line is no
	// line and no error, journalctl's exit included.
	if next := readAppLogs(t, app, 50, read.cursor); len(next.lines) != 0 || next.gap || next.more || next.cursor != "" {
		t.Errorf("after the last line: %+v", next)
	}
	if none := readAppLogs(t, logsName("itlogs-none"), 200, ""); len(none.lines) != 0 || none.more {
		t.Errorf("an app with no line: %+v", none)
	}
}

// TestIntegrationLogsNothingForgedFromTheSandbox: from inside a real
// app unit, journald's sockets are not there (/run/systemd absent,
// /dev/log dangling), and logger, logger --journald, systemd-cat and a
// redirect to the stdout socket each write nothing: no line of the
// forged text is in the journal under any identifier, and the victim's
// answer holds its own line alone.
func TestIntegrationLogsNothingForgedFromTheSandbox(t *testing.T) {
	r := integrationRunner(t)
	victim, other := logsName("itlogs-v"), logsName("itlogs-o")
	runToEnd(t, r, logsApp(t, victim, "v1", "echo the victim own line\nsleep 1\n"))
	forged := "FORGED-" + strconv.FormatInt(time.Now().UnixNano(), 16)
	body := `for p in /run/systemd /run/systemd/journal /run/systemd/journal/stdout /dev/log; do [ -e "$p" ] && echo "VIEW present $p"; done
logger -t motel-` + victim + ` ` + forged + `-logger 2>/dev/null; echo "LOGGER exit $?"
printf 'MESSAGE=%s-native\nSYSLOG_IDENTIFIER=motel-` + victim + `\n' ` + forged + ` | logger --journald 2>/dev/null; echo "LOGGERJ exit $?"
echo ` + forged + `-cat | systemd-cat -t motel-` + victim + ` 2>/dev/null; echo "CAT exit $?"
(echo ` + forged + `-redirect > /run/systemd/journal/stdout) 2>/dev/null; echo "REDIRECT exit $?"
echo other done
sleep 1
`
	runToEnd(t, r, logsApp(t, other, "v1", body))
	o := waitLogs(t, other, 5)
	for _, l := range o.lines {
		t.Logf("inside the sandbox: %s", l.text)
		if strings.HasPrefix(l.text, "VIEW present") {
			t.Errorf("the sandbox holds a journal path: %s", l.text)
		}
	}
	v := waitLogs(t, victim, 1)
	if got := texts(v.lines); len(got) != 1 || got[0] != "the victim own line" {
		t.Errorf("the victim's answer: %q", got)
	}
	if n := len(journalFields(t, "--grep", forged)); n != 0 {
		t.Errorf("%d lines of the forged text are in the journal", n)
	}
}

// TestIntegrationLogsCrashLoopWhileJournaldIsBehind: launches of an app
// that print and exit while journald is paused lose the unit field on
// every line (the case the identifier selection was chosen for): the
// read finds every line, each with no release, none passed over. And a
// run of launches with journald running: every line found.
func TestIntegrationLogsCrashLoopWhileJournaldIsBehind(t *testing.T) {
	r := integrationRunner(t)
	launch := func(app string, i int) {
		runToEnd(t, r, logsApp(t, app, "v1", fmt.Sprintf("exec awk 'BEGIN { for (i = 0; i < 20; i++) printf \"launch %d line %%d\\n\", i; exit 1 }'\n", i)))
	}
	app := logsName("itlogs-p")
	resume := pauseJournald(t)
	began, n := time.Now(), 0
	for ; time.Since(began) < 2*time.Second; n++ {
		launch(app, n)
	}
	resume()
	read := waitLogs(t, app, n*20)
	noUnit := 0
	for _, l := range read.lines {
		if l.userUnit == "" && l.unit == "" {
			noUnit++
		}
	}
	t.Logf("journald paused: %d launches, %d lines written, %d read, %d of them with no unit, %d passed over", n, n*20, len(read.lines), noUnit, read.passedOver)
	if len(read.lines) != n*20 || read.passedOver != 0 {
		t.Errorf("%d of %d lines read, %d passed over", len(read.lines), n*20, read.passedOver)
	}
	app = logsName("itlogs-c")
	for i := 0; i < 20; i++ {
		launch(app, i)
	}
	read = waitLogs(t, app, 400)
	withUnit := 0
	for _, l := range read.lines {
		if l.userUnit != "" {
			withUnit++
		}
	}
	t.Logf("journald running: 20 launches, 400 lines written, %d read, %d of them with the unit, %d passed over", len(read.lines), withUnit, read.passedOver)
	if len(read.lines) != 400 || read.passedOver != 0 {
		t.Errorf("%d of 400 lines read, %d passed over", len(read.lines), read.passedOver)
	}
}

// TestIntegrationLogsTwoAppsNeverMix: apps x and x-2, one's name the
// other's start: a canary in one is never in the other's answer, and
// one's cursor sent for the other is not honoured (gap).
func TestIntegrationLogsTwoAppsNeverMix(t *testing.T) {
	r := integrationRunner(t)
	a := logsName("itlogs-t")
	b := a + "-2"
	canary := "CANARY-" + strconv.FormatInt(time.Now().UnixNano(), 16)
	runToEnd(t, r, logsApp(t, a, "v1", "echo "+canary+"\necho a second line of a\nsleep 1\n"))
	runToEnd(t, r, logsApp(t, b, "v1", "echo the first line of b\nsleep 1\n"))
	runToEnd(t, r, logsApp(t, b, "v2", "echo the second line of b\nsleep 1\n"))
	ra, rb := waitLogs(t, a, 2), waitLogs(t, b, 2)
	for _, l := range rb.lines {
		if strings.Contains(l.text, canary) {
			t.Fatalf("%s's canary in %s's answer", a, b)
		}
	}
	for _, l := range ra.lines {
		if strings.Contains(l.text, "of b") {
			t.Fatalf("%s's line in %s's answer", b, a)
		}
	}
	if got := strings.Join(texts(rb.lines), "|"); got != "the first line of b|the second line of b" {
		t.Errorf("%s: %q", b, got)
	}
	got := readAppLogs(t, b, 200, ra.lines[0].cursor)
	if !got.gap || len(got.lines) != 2 {
		t.Errorf("%s's cursor on %s: %+v", a, b, got)
	}
	if got := readAppLogs(t, b, 200, rb.lines[0].cursor); got.gap || strings.Join(texts(got.lines), "|") != "the second line of b" {
		t.Errorf("%s's own first cursor: %+v", b, got)
	}
}

// TestIntegrationLogsFollowNeverSkipsOrRepeats: an app that writes a
// burst between two polls is read back by a follower in order, every
// line once, the answers bounded by the count and by 64 KiB of text.
// Smaller than the spike's 5,000 lines: every app of the lane shares
// journald's rate limit (#628).
func TestIntegrationLogsFollowNeverSkipsOrRepeats(t *testing.T) {
	for _, c := range []struct{ size, count int }{{100, 1500}, {10240, 200}} {
		t.Run(strconv.Itoa(c.size), func(t *testing.T) {
			r := integrationRunner(t)
			app := logsName("itlogs-b")
			h, err := r.Start(logsApp(t, app, "v1", fmt.Sprintf(`echo burst-start
sleep 2
awk 'BEGIN { pad = ""; for (i = 0; i < %d; i++) pad = pad "p"; for (i = 0; i < %d; i++) printf "seq=%%05d %%s\n", i, pad }'
echo burst-end
sleep 1
`, c.size-11, c.count)))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = r.Stop(h, time.Second) })
			start := waitLogs(t, app, 1)
			if start.lines[0].text != "burst-start" {
				t.Fatalf("the first read: %q", texts(start.lines))
			}
			select {
			case <-r.Wait(h):
			case <-time.After(30 * time.Second):
				t.Fatal("the burst did not end")
			}
			cursor, next, polls, ended := start.cursor, 0, 0, false
			for deadline := time.Now().Add(20 * time.Second); !ended; polls++ {
				if time.Now().After(deadline) {
					t.Fatalf("at %d of %d after %d polls", next, c.count, polls)
				}
				read := readAppLogs(t, app, 1000, cursor)
				if read.gap {
					t.Fatalf("poll %d: gap", polls)
				}
				for _, l := range read.lines {
					if l.text == "burst-end" {
						ended = true
						continue
					}
					var seq int
					if _, err := fmt.Sscanf(l.text, "seq=%d", &seq); err != nil || seq != next {
						t.Fatalf("poll %d: %.20q where %d was next", polls, l.text, next)
					}
					next++
				}
				if read.cursor != "" {
					cursor = read.cursor
				}
				if !read.more && !ended {
					time.Sleep(200 * time.Millisecond) // journald may still be storing the burst
				}
			}
			if next != c.count {
				t.Errorf("%d of %d lines", next, c.count)
			}
			t.Logf("%d lines of %d B read back in order in %d polls", next, c.size, polls)
		})
	}
}

// TestIntegrationLogsOddLines: what journald makes of odd output, read
// back: a line over its 48 KiB is several entries, each read whole
// (with --all; journalctl prints null for any of 4 KiB or more without
// it); a NUL splits a line; an empty line is no entry; bytes that are
// not UTF-8 are read as written (sent as U+FFFD, a run one); control
// characters stay; a <3> prefix is journald's, taken off.
func TestIntegrationLogsOddLines(t *testing.T) {
	r := integrationRunner(t)
	app := logsName("itlogs-x")
	runToEnd(t, r, logsApp(t, app, "v1", `printf 'plain\n'
printf '<3>an error with a level prefix\n'
printf 'bad \377\376 bytes\n'
printf '\033[31mred\033[0m\n'
printf '\n'
printf 'cr\rinside\n'
printf 'nul\000after\n'
awk 'BEGIN { s = ""; for (i = 0; i < 102400; i++) s = s "h"; print s }'
printf 'last\n'
sleep 1
`))
	read := waitLogs(t, app, 11)
	want := []string{"plain", "an error with a level prefix", "bad \xff\xfe bytes", "\x1b[31mred\x1b[0m", "cr\rinside", "nul", "after", "", "", "", "last"}
	got := texts(read.lines)
	if len(got) != len(want) {
		t.Fatalf("%d lines, want %d: %.60q", len(got), len(want), got)
	}
	long := 0
	for i, l := range got {
		if s := filterLogLine(nil, l, map[string]bool{}); !utf8.ValidString(s) {
			t.Errorf("line %d is not UTF-8 as sent: %q", i, s)
		}
		if want[i] == "" {
			if strings.Trim(l, "h") != "" {
				t.Errorf("line %d: %.20q, want a piece of the long line", i, l)
			}
			long += len(l)
			continue
		}
		if l != want[i] {
			t.Errorf("line %d: %q, want %q", i, l, want[i])
		}
	}
	if long != 102400 || len(got[7]) != 49152 {
		t.Errorf("the long line: %d bytes in all, the first piece %d", long, len(got[7]))
	}
}
