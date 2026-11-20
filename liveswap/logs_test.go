package liveswap

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// logsUID is the uid the scripted journal's entries are of: this
// process's, which the route holds them to as moteld's.
var logsUID = os.Getuid()

// scriptEntry is one entry of a scripted journal: what -o json prints of
// it, decoded by the test's own hand.
type scriptEntry struct {
	cursor         string
	at             time.Time
	message        any // a string, a []int of byte values, nil for null, or absentMessage
	unit, userUnit string
}

// absentMessage is an entry with no MESSAGE field.
type absentMessage struct{}

// json is the entry as journalctl -o json prints it, its fields in an
// order of the test's choosing (journalctl's is a hash's).
func (e scriptEntry) json() []byte {
	m := map[string]any{"__CURSOR": e.cursor, "__REALTIME_TIMESTAMP": strconv.FormatInt(e.at.UnixMicro(), 10), "_BOOT_ID": "fedcba9876543210fedcba9876543210"}
	if _, absent := e.message.(absentMessage); !absent {
		m["MESSAGE"] = e.message
	}
	if e.unit != "" {
		m["_SYSTEMD_UNIT"] = e.unit
	}
	if e.userUnit != "" {
		m["_SYSTEMD_USER_UNIT"] = e.userUnit
	}
	b, _ := json.Marshal(m)
	return b
}

// scriptedLogs is a journal of one app's selection, oldest first, read
// as journalctl reads it for logsArgs: a tail newest first, n+1; a
// follow from the cursor's entry, or from lands (the entry a cursor the
// journal does not hold makes journalctl seek to), n+2.
type scriptedLogs struct {
	mu      sync.Mutex
	journal []scriptEntry
	lands   int
	err     error
	calls   [][]string
	// read counts the entries each handed to the reader, call by call.
	read []int
	// hold, when set, is waited on before a read begins.
	hold chan struct{}
}

func (s *scriptedLogs) entries(ctx context.Context, args []string, each func([]byte) bool) error {
	if s.hold != nil {
		select {
		case <-s.hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.mu.Lock()
	s.calls = append(s.calls, args)
	journal := slices.Clone(s.journal)
	s.read = append(s.read, 0)
	call := len(s.read) - 1
	s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	var count int
	var from, cursor string
	reverse := false
	for i, a := range args {
		switch {
		case a == "-r":
			reverse = true
		case a == "-n":
			from = args[i+1]
		case strings.HasPrefix(a, "--cursor="):
			cursor = strings.TrimPrefix(a, "--cursor=")
		}
	}
	count, _ = strconv.Atoi(strings.TrimPrefix(from, "+"))
	var out []scriptEntry
	if reverse {
		for i := len(journal) - 1; i >= 0 && len(out) < count; i-- {
			out = append(out, journal[i])
		}
	} else {
		start := s.lands
		for i, e := range journal {
			if e.cursor == cursor {
				start = i
			}
		}
		for i := start; i < len(journal) && len(out) < count; i++ {
			out = append(out, journal[i])
		}
	}
	for _, e := range out {
		s.mu.Lock()
		s.read[call]++
		s.mu.Unlock()
		if !each(e.json()) {
			break
		}
	}
	return nil
}

// cursorOf is the made-up cursor of the scripted journal's i-th entry:
// of journalCursor's form, the sequence number i.
func cursorOf(i int) string {
	return fmt.Sprintf("s=0123456789abcdef0123456789abcdef;i=%x;b=fedcba9876543210fedcba9876543210;m=%x;t=%x;x=%x", i+1, i+1, 0x660ba6e64c877+i, 0x9edf869812ed4251+uint64(i))
}

// appJournal is n entries of the app demo, each its own line, from a
// unit of release v<i/10>.
func appJournal(n int, text func(i int) string) []scriptEntry {
	t0 := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	out := make([]scriptEntry, n)
	for i := range out {
		out[i] = scriptEntry{
			cursor: cursorOf(i), at: t0.Add(time.Duration(i) * time.Millisecond), message: text(i),
			unit: "user@" + strconv.Itoa(logsUID) + ".service", userUnit: "motel-demo.v" + strconv.Itoa(i/10) + ".0a1b2c3d0a1b2c3d.service",
		}
	}
	return out
}

func lineN(i int) string { return "line " + strconv.Itoa(i) }

func texts(lines []logEntry) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.text
	}
	return out
}

func demoQuery(n int, after string) logsQuery {
	return logsQuery{app: "demo", ident: "motel-demo", uid: logsUID, n: n, after: after}
}

// TestLogsArgs: journalctl's argv for a tail and a follow, exactly: the
// identifier, uid and count from their sources, the cursor one argument.
func TestLogsArgs(t *testing.T) {
	head := "--user --no-pager --quiet -o json --all --output-fields=MESSAGE,_SYSTEMD_UNIT,_SYSTEMD_USER_UNIT"
	match := "SYSLOG_IDENTIFIER=motel-blog _UID=996 _TRANSPORT=stdout"
	if got, want := logsArgs("motel-blog", 996, 200, ""), strings.Fields(head+" -r -n 201 "+match); !slices.Equal(got, want) {
		t.Errorf("tail:\n got %q\nwant %q", got, want)
	}
	if got, want := logsArgs("motel-blog", 996, 50, testCursor), strings.Fields(head+" --cursor="+testCursor+" -n +52 "+match); !slices.Equal(got, want) {
		t.Errorf("follow:\n got %q\nwant %q", got, want)
	}
}

// TestLogsEntryDecodes: a MESSAGE that is a string, or an array of byte
// values (not UTF-8, or a control character: as it is, for the filter to
// see as the app wrote it); the time
// and the cursor; and every entry that is not one as asked (no MESSAGE,
// null, no cursor, no time, a value that is no byte, a field twice) an
// error, never an empty line.
func TestLogsEntryDecodes(t *testing.T) {
	at := time.Date(2026, 11, 20, 9, 30, 0, 123456000, time.UTC)
	for _, c := range []struct {
		name    string
		message any
		want    string
	}{
		{"a string", "GET /cart 200", "GET /cart 200"},
		{"bytes that are not UTF-8", []int{'b', 'a', 'd', ' ', 0xff, 0xfe, '!'}, "bad \xff\xfe!"},
		{"a control character", []int{'c', 'r', '\r', 'x', 0x1b, '['}, "cr\rx\x1b["},
		{"a long line", strings.Repeat("h", 49152), strings.Repeat("h", 49152)},
	} {
		e, err := decodeEntry(scriptEntry{cursor: testCursor, at: at, message: c.message, unit: "user@996.service", userUnit: "motel-demo.v1.0a1b2c3d0a1b2c3d.service"}.json())
		if err != nil || e.text != c.want || !e.at.Equal(at) || e.cursor != testCursor || e.unit != "user@996.service" || e.userUnit != "motel-demo.v1.0a1b2c3d0a1b2c3d.service" {
			t.Errorf("%s: %+v %v", c.name, e, err)
		}
	}
	for name, raw := range map[string]string{
		"no MESSAGE":              `{"__CURSOR":"c","__REALTIME_TIMESTAMP":"1"}`,
		"a null MESSAGE":          `{"__CURSOR":"c","__REALTIME_TIMESTAMP":"1","MESSAGE":null}`,
		"no cursor":               `{"__REALTIME_TIMESTAMP":"1","MESSAGE":"x"}`,
		"no time":                 `{"__CURSOR":"c","MESSAGE":"x"}`,
		"a time that is none":     `{"__CURSOR":"c","__REALTIME_TIMESTAMP":"soon","MESSAGE":"x"}`,
		"a negative time":         `{"__CURSOR":"c","__REALTIME_TIMESTAMP":"-1","MESSAGE":"x"}`,
		"a value that is no byte": `{"__CURSOR":"c","__REALTIME_TIMESTAMP":"1","MESSAGE":[104,256]}`,
		"MESSAGE twice":           `{"__CURSOR":"c","__REALTIME_TIMESTAMP":"1","MESSAGE":["a","b"]}`,
		"a unit twice":            `{"__CURSOR":"c","__REALTIME_TIMESTAMP":"1","MESSAGE":"x","_SYSTEMD_USER_UNIT":["a","b"]}`,
		"a MESSAGE of a number":   `{"__CURSOR":"c","__REALTIME_TIMESTAMP":"1","MESSAGE":7}`,
		"not JSON":                `{"__CURSOR":"c"`,
	} {
		if e, err := decodeEntry([]byte(raw)); !errors.Is(err, errLogsEntry) {
			t.Errorf("%s: %+v %v", name, e, err)
		}
	}
}

// TestLogsEntryBound: readEntry reads one line of journalctl's output up
// to the bound, across the reader's buffer, and fails, named, past it.
func TestLogsEntryBound(t *testing.T) {
	long := strings.Repeat("x", 300)
	br := bufio.NewReaderSize(strings.NewReader(long+"\n"+"short\n"+"last"), 16)
	if got, err := readEntry(br, 300); err != nil || string(got) != long {
		t.Fatalf("an entry of the bound: %d bytes, %v", len(got), err)
	}
	if got, err := readEntry(br, 300); err != nil || string(got) != "short" {
		t.Fatalf("the next: %q, %v", got, err)
	}
	if got, err := readEntry(br, 300); string(got) != "last" || err == nil {
		t.Fatalf("the last, with no newline: %q, %v", got, err)
	}
	br = bufio.NewReaderSize(strings.NewReader(long+"x\n"), 16)
	if _, err := readEntry(br, 300); !errors.Is(err, errLogsEntryTooLong) {
		t.Fatalf("an entry over the bound: %v", err)
	}
}

// TestLogsTailKeepsTheNewest: a tail is the newest lines, oldest first,
// up to the count or logsTextMax of text, more when a line was left,
// the cursor the newest line's; and journalctl is asked for one entry
// more than is sent, and read no further than that.
func TestLogsTailKeepsTheNewest(t *testing.T) {
	src := &scriptedLogs{journal: appJournal(30, lineN)}
	read, err := readLogs(context.Background(), src, demoQuery(3, ""))
	if err != nil || !slices.Equal(texts(read.lines), []string{"line 27", "line 28", "line 29"}) || !read.more || read.gap || read.cursor != cursorOf(29) {
		t.Fatalf("%q more %v gap %v cursor %q, %v", texts(read.lines), read.more, read.gap, read.cursor, err)
	}
	if read.lines[0].userUnit != "motel-demo.v2.0a1b2c3d0a1b2c3d.service" {
		t.Errorf("the unit: %+v", read.lines[0])
	}
	if src.read[0] != 4 {
		t.Errorf("journalctl was read for %d entries, want 4", src.read[0])
	}
	// All of it: no more.
	read, _ = readLogs(context.Background(), &scriptedLogs{journal: appJournal(30, lineN)}, demoQuery(50, ""))
	if len(read.lines) != 30 || read.more || read.lines[0].text != "line 0" {
		t.Errorf("all of a short journal: %d lines, more %v", len(read.lines), read.more)
	}
	// Exactly the count: no more.
	read, _ = readLogs(context.Background(), &scriptedLogs{journal: appJournal(50, lineN)}, demoQuery(50, ""))
	if len(read.lines) != 50 || read.more {
		t.Errorf("a journal of the count: %d lines, more %v", len(read.lines), read.more)
	}
	// The text cap: lines of 4 KiB and more, each counted at most 4 KiB,
	// so sixteen; the newest sixteen.
	big := func(i int) string { return fmt.Sprintf("%05d ", i) + strings.Repeat("b", 9000) }
	src = &scriptedLogs{journal: appJournal(40, big)}
	read, _ = readLogs(context.Background(), src, demoQuery(200, ""))
	if len(read.lines) != logsTextMax/logsLineMax || !read.more || !strings.HasPrefix(read.lines[15].text, "00039 ") || !strings.HasPrefix(read.lines[0].text, "00024 ") {
		t.Errorf("the text cap: %d lines, more %v", len(read.lines), read.more)
	}
	if src.read[0] != 17 {
		t.Errorf("journalctl was read for %d entries at the text cap, want 17", src.read[0])
	}
	// Nothing at all.
	read, err = readLogs(context.Background(), &scriptedLogs{}, demoQuery(200, ""))
	if err != nil || len(read.lines) != 0 || read.more || read.cursor != "" {
		t.Errorf("an empty journal: %+v %v", read, err)
	}
}

// TestLogsFollowKeepsTheOldest: a follow is the lines after the cursor,
// the oldest first, up to the count or logsTextMax of text, its cursor
// the last line sent; never the cursor's own line.
func TestLogsFollowKeepsTheOldest(t *testing.T) {
	src := &scriptedLogs{journal: appJournal(30, lineN)}
	read, err := readLogs(context.Background(), src, demoQuery(3, cursorOf(4)))
	if err != nil || !slices.Equal(texts(read.lines), []string{"line 5", "line 6", "line 7"}) || !read.more || read.gap || read.cursor != cursorOf(7) {
		t.Fatalf("%q more %v gap %v cursor %q, %v", texts(read.lines), read.more, read.gap, read.cursor, err)
	}
	if len(src.calls) != 1 || src.read[0] != 5 {
		t.Errorf("%d reads, the first of %d entries, want one of 5", len(src.calls), src.read[0])
	}
	// At the end: what is left, no more; and from the last, nothing and
	// no cursor.
	read, _ = readLogs(context.Background(), src, demoQuery(50, cursorOf(26)))
	if !slices.Equal(texts(read.lines), []string{"line 27", "line 28", "line 29"}) || read.more || read.cursor != cursorOf(29) {
		t.Errorf("near the end: %q more %v", texts(read.lines), read.more)
	}
	read, _ = readLogs(context.Background(), src, demoQuery(50, cursorOf(29)))
	if len(read.lines) != 0 || read.more || read.gap || read.cursor != "" {
		t.Errorf("at the end: %+v", read)
	}
	// The text cap keeps the oldest sixteen after the cursor.
	big := func(i int) string { return fmt.Sprintf("%05d ", i) + strings.Repeat("b", 9000) }
	read, _ = readLogs(context.Background(), &scriptedLogs{journal: appJournal(40, big)}, demoQuery(200, cursorOf(2)))
	if len(read.lines) != 16 || !read.more || !strings.HasPrefix(read.lines[0].text, "00003 ") || read.cursor != cursorOf(18) {
		t.Errorf("the text cap: %d lines, more %v, cursor %q", len(read.lines), read.more, read.cursor)
	}
}

// TestLogsFollowNeverSkipsOrRepeats: a follower that asks again with
// each answer's cursor, while the answers stop at the count and at the
// text cap by turns, is given every line once, in order.
func TestLogsFollowNeverSkipsOrRepeats(t *testing.T) {
	size := func(i int) string {
		if i%5 == 0 {
			return fmt.Sprintf("%05d ", i) + strings.Repeat("w", 30000)
		}
		return fmt.Sprintf("%05d", i)
	}
	src := &scriptedLogs{journal: appJournal(500, size)}
	// From the first line, as a follower that tailed it.
	cursor, next := cursorOf(0), 1
	for polls := 0; next < 500; polls++ {
		if polls > 500 {
			t.Fatalf("no end after %d polls, at %d", polls, next)
		}
		read, err := readLogs(context.Background(), src, demoQuery(50, cursor))
		if err != nil || read.gap {
			t.Fatalf("poll %d: %v gap %v", polls, err, read.gap)
		}
		for _, l := range read.lines {
			if !strings.HasPrefix(l.text, fmt.Sprintf("%05d", next)) {
				t.Fatalf("poll %d: %.10q where %d was next", polls, l.text, next)
			}
			next++
		}
		if read.cursor != "" {
			cursor = read.cursor
		}
		if !read.more && next < 500 {
			t.Fatalf("poll %d: no more at %d", polls, next)
		}
	}
}

// TestLogsFollowHonoursOnlyTheCursorSent: a follow is honoured only when
// the first entry journalctl reads is the cursor sent, byte for byte:
// one of the grammar's form that the journal does not hold (made-up
// ids, a time chosen, a sequence number one back, another field
// changed, another app's, one vacuumed away) makes journalctl seek to
// some entry of the app's, and the answer is then a fresh tail with
// gap, never the lines after where it landed.
func TestLogsFollowHonoursOnlyTheCursorSent(t *testing.T) {
	real := cursorOf(10)
	for name, after := range map[string]string{
		"made-up ids, a chosen time":   "s=00000000000000000000000000000000;i=1;b=00000000000000000000000000000000;m=1;t=5f0000000000;x=1",
		"the sequence number one back": strings.Replace(real, "i=b;", "i=a;", 1),
		"x changed":                    strings.Replace(real, ";x=9edf869812ed425b", ";x=9edf869812ed425c", 1),
		"another boot":                 strings.Replace(real, "b=fedcba9876543210fedcba9876543210", "b=00000000000000000000000000000001", 1),
		"another app's":                "s=aaaa456789abcdef0123456789abcdef;i=3;b=fedcba9876543210fedcba9876543210;m=3;t=660ba6e64c879;x=1",
		"one vacuumed away":            cursorOf(1000),
	} {
		if after == real || !journalCursor.re.MatchString(after) {
			t.Fatalf("%s: %q is no forged cursor of the grammar", name, after)
		}
		src := &scriptedLogs{journal: appJournal(30, lineN), lands: 11}
		read, err := readLogs(context.Background(), src, demoQuery(3, after))
		if err != nil || !read.gap || !slices.Equal(texts(read.lines), []string{"line 27", "line 28", "line 29"}) || read.cursor != cursorOf(29) {
			t.Errorf("%s: %q gap %v cursor %q, %v", name, texts(read.lines), read.gap, read.cursor, err)
		}
		if len(src.calls) != 2 || src.read[0] != 1 || !slices.Contains(src.calls[1], "-r") {
			t.Errorf("%s: the follow read %v entries before the tail, in %d calls", name, src.read, len(src.calls))
		}
	}
	// The genuine one is honoured.
	read, err := readLogs(context.Background(), &scriptedLogs{journal: appJournal(30, lineN), lands: 11}, demoQuery(3, real))
	if err != nil || read.gap || read.lines[0].text != "line 11" {
		t.Errorf("the genuine cursor: %q gap %v, %v", texts(read.lines), read.gap, err)
	}
}

// TestLogsPassesOverAnotherUnitsEntry: an entry under the app's
// identifier whose unit fields, which journald sets, name a unit not the
// app's (another app's, the probe's, a system unit of the uid, a backup
// role's) is not sent; one with no unit field (a process journald read
// late) or with the app's is, its release the unit's version. The
// cursor moves past what was passed over, so a run of such entries
// cannot hold a follower in place.
func TestLogsPassesOverAnotherUnitsEntry(t *testing.T) {
	j := appJournal(8, lineN)
	j[1].userUnit = "motel-demo-2.v1.0a1b2c3d0a1b2c3d.service"
	j[2].userUnit = "motel-sandboxprobe_0a1b2c3d0a1b2c3d.service"
	j[3].unit, j[3].userUnit = "motel_backup_dump_demo.service", ""
	j[4].unit, j[4].userUnit = "", ""
	j[5].unit = "user@" + strconv.Itoa(logsUID+4242) + ".service" // another uid's manager
	j[6].userUnit = "motel-demo.v7.0a1b2c3d0a1b2c3d.prestart.service"
	read, err := readLogs(context.Background(), &scriptedLogs{journal: j}, demoQuery(50, ""))
	if err != nil || !slices.Equal(texts(read.lines), []string{"line 0", "line 4", "line 6", "line 7"}) || read.passedOver != 4 {
		t.Fatalf("%q passed over %d, %v", texts(read.lines), read.passedOver, err)
	}
	if read.passedUnit != "user@"+strconv.Itoa(logsUID+4242)+".service motel-demo.v0.0a1b2c3d0a1b2c3d.service" {
		t.Errorf("the first passed over, newest first: %q", read.passedUnit)
	}
	for i, l := range read.lines {
		rel, ok := unitRelease(l.userUnit, "demo")
		if want := []string{"v0", "", "v7", "v0"}[i]; rel != want || ok != (want != "") {
			t.Errorf("%q: release %q %v, want %q", l.text, rel, ok, want)
		}
	}
	// A follower behind a run of them moves on.
	j = appJournal(12, lineN)
	for i := 1; i <= 10; i++ {
		j[i].userUnit = "motel-blog.v1.0a1b2c3d0a1b2c3d.service"
	}
	read, err = readLogs(context.Background(), &scriptedLogs{journal: j}, demoQuery(3, cursorOf(0)))
	if err != nil || len(read.lines) != 0 || !read.more || read.cursor != cursorOf(4) {
		t.Fatalf("behind a run: %q more %v cursor %q, %v", texts(read.lines), read.more, read.cursor, err)
	}
	read, _ = readLogs(context.Background(), &scriptedLogs{journal: j}, demoQuery(3, cursorOf(8)))
	if !slices.Equal(texts(read.lines), []string{"line 11"}) || read.more || read.cursor != cursorOf(11) {
		t.Errorf("past the run: %q more %v cursor %q", texts(read.lines), read.more, read.cursor)
	}
}

// TestLogsLegacyIdentifiers: an app named backup or sandbox-probe, whose
// identifier motel's own units wrote under before they took one no app
// makes, is not sent an entry with no unit field, which could be one of
// those, nor one of motel's own units' (a backup unit's, the probe's);
// its own units' entries are sent, and those passed over are not
// counted as another unit's, which the journal would tell.
func TestLogsLegacyIdentifiers(t *testing.T) {
	for _, app := range []string{"backup", "sandbox-probe"} {
		j := appJournal(3, lineN)
		for i := range j {
			j[i].userUnit = "motel-" + app + ".v1.0a1b2c3d0a1b2c3d.service"
		}
		j = append(j, appJournal(2, lineN)...)
		j[1].unit, j[1].userUnit = "", ""
		j[3].unit, j[3].userUnit = "motel_backup_dump_shop.service", ""
		j[4].userUnit = "motel-sandboxprobe_0a1b2c3d0a1b2c3d.service"
		read, err := readLogs(context.Background(), &scriptedLogs{journal: j}, logsQuery{app: app, ident: syslogIdentifier(app), uid: logsUID, n: 50})
		if err != nil || !slices.Equal(texts(read.lines), []string{"line 0", "line 2"}) || read.passedOver != 0 {
			t.Errorf("%s: %q, passed over %d, %v", app, texts(read.lines), read.passedOver, err)
		}
	}
	// Any other app's are sent.
	j := appJournal(3, lineN)
	j[1].unit, j[1].userUnit = "", ""
	if read, _ := readLogs(context.Background(), &scriptedLogs{journal: j}, demoQuery(50, "")); len(read.lines) != 3 {
		t.Errorf("demo: %q", texts(read.lines))
	}
}

// cutSource hands the reader an entry cut short, as journalctl killed at
// the read's time inside one leaves it, after the time ran out.
type cutSource struct{ cancel context.CancelFunc }

func (c cutSource) entries(_ context.Context, _ []string, each func([]byte) bool) error {
	c.cancel()
	each([]byte(`{"__CURSOR":"s=0123`))
	return nil
}

// TestLogsReadFailsLoudly: an entry that is not one, a source that
// fails, and a cursor journalctl gives that the key's grammar would
// refuse, each fail the read, and nothing is sent.
func TestLogsReadFailsLoudly(t *testing.T) {
	j := appJournal(3, lineN)
	j[1].message = nil
	if read, err := readLogs(context.Background(), &scriptedLogs{journal: j}, demoQuery(50, "")); !errors.Is(err, errLogsEntry) || len(read.lines) != 0 {
		t.Errorf("a null MESSAGE: %+v %v", read, err)
	}
	if _, err := readLogs(context.Background(), &scriptedLogs{err: errors.New("journalctl: boom")}, demoQuery(50, "")); err == nil {
		t.Error("a failed journalctl read as no lines")
	}
	// journalctl killed inside an entry at the read's time: the time.
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := readLogs(ctx, cutSource{cancel}, demoQuery(50, "")); !errors.Is(err, context.Canceled) {
		t.Errorf("an entry cut by the time: %v", err)
	}
	j = appJournal(3, lineN)
	j[2].cursor = "s=0123;i=1"
	if read, err := readLogs(context.Background(), &scriptedLogs{journal: j}, demoQuery(50, "")); err == nil || len(read.lines) != 0 {
		t.Errorf("a cursor the grammar refuses: %+v %v", read, err)
	}
}

// TestUnitRelease: the version of one of the app's units, an instance's
// or a pre_start's, and nothing for another app's, the probe's or a
// name that is no unit of an app.
func TestUnitRelease(t *testing.T) {
	for name, want := range map[string]string{
		"motel-demo.v1.0a1b2c3d0a1b2c3d.service":                    "v1",
		"motel-demo.a41c09e2b7d1.0a1b2c3d0a1b2c3d.prestart.service": "a41c09e2b7d1",
		"motel-demo-2.v1.0a1b2c3d0a1b2c3d.service":                  "",
		"motel-sandboxprobe_0a1b2c3d0a1b2c3d.service":               "",
		"motel_backup_dump_demo.service":                            "",
		"user@996.service":                                          "",
		"":                                                          "",
	} {
		if got, ok := unitRelease(name, "demo"); got != want || ok != (want != "") {
			t.Errorf("%q: %q %v, want %q", name, got, ok, want)
		}
	}
}
