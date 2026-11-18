package box

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The applier's units and its exchange tree, as the package ships them,
// held to DESIGN-box.md and read from it: the units, line for line, to
// the table in "The applier unit", and the tmpfiles.d file to the Paths
// table. Neither list is restated here, so the design cannot say one
// thing while the package ships another.

const (
	packagingDir = "../packaging"
	pathUnit     = "motel-box-apply.path"
	serviceUnit  = "motel-box-apply.service"
)

// forbidden is what neither unit may say, whatever the design's table
// says: the lines whose absence the applier's contract rests on (root;
// started again by the path unit, every time, and by its own name; the
// one non-zero exit failing the unit). Why each is left out is the
// design's, under the table in "The applier unit".
var forbidden = []string{"User", "Group", "DynamicUser", "Restart", "RemainAfterExit", "Unit", "SuccessExitStatus", "AmbientCapabilities"}

// The contract with the applier (box PR 3a): what runs, and the paths
// that start it — a bundle in in/, and what a killed run left, in work/
// and as the record — whatever the design's table comes to say.
const (
	applierCommand = "/usr/bin/moteld box apply"
	exchangeTree   = "/var/lib/motel-box"
)

func TestTheShippedUnitsSayWhatTheDesignLists(t *testing.T) {
	design := designUnits(t)
	for _, name := range []string{pathUnit, serviceUnit} {
		t.Run(name, func(t *testing.T) {
			want, got := design[name], values(readUnit(t, name))
			for _, s := range slices.Sorted(maps.Keys(got)) {
				if _, ok := want[s]; !ok {
					t.Errorf("[%s] is not in the design's table: %q", s, got[s])
				}
			}
			for _, s := range slices.Sorted(maps.Keys(want)) {
				for _, k := range slices.Sorted(maps.Keys(want[s])) {
					if !slices.Equal(got[s][k], want[s][k]) {
						t.Errorf("[%s] %s: the design says %q, the unit %q", s, k, want[s][k], got[s][k])
					}
				}
				for _, k := range slices.Sorted(maps.Keys(got[s])) {
					if _, ok := want[s][k]; !ok {
						t.Errorf("[%s] %s=%q is not in the design's table", s, k, got[s][k])
					}
				}
			}
			// The design's table only: the file is held to it above.
			for s, keys := range want {
				for _, k := range forbidden {
					if v, ok := keys[k]; ok {
						t.Errorf("[%s] %s=%q: never, whatever the table says", s, k, v)
					}
				}
			}
		})
	}
	// The contract with the applier, which the table must keep.
	svc, path := design[serviceUnit], design[pathUnit]
	if got := svc["Service"]["ExecStart"]; !slices.Equal(got, []string{applierCommand}) {
		t.Errorf("ExecStart %q: the applier is %s", got, applierCommand)
	}
	if got := svc["Service"]["Type"]; !slices.Equal(got, []string{"oneshot"}) {
		t.Errorf("Type %q: one run per start is a oneshot", got)
	}
	if _, ok := svc["Install"]; ok {
		t.Error("the service has an [Install] section: the path unit is its only start")
	}
	// Two routes around RestrictAddressFamilies= closed, as for app units
	// (#298): an io_uring ring's socket op, and on amd64 the x86-32 ABI's
	// socketcall().
	if got := svc["Service"]["SystemCallArchitectures"]; !slices.Equal(got, []string{"native"}) {
		t.Errorf("SystemCallArchitectures %q: on amd64 the x86-32 ABI's socketcall() opens any family", got)
	}
	filters := svc["Service"]["SystemCallFilter"]
	if len(filters) == 0 || strings.HasPrefix(filters[0], "~") || !slices.Contains(strings.Fields(filters[0]), "@system-service") {
		t.Errorf("SystemCallFilter %q: the first line must be the @system-service allow list, or the manager builds a deny list", filters)
	}
	denied := false
	for _, f := range filters[min(1, len(filters)):] {
		rest, ok := strings.CutPrefix(f, "~")
		if !ok {
			t.Errorf("SystemCallFilter=%s after the first line adds back what a deny line took out", f)
		}
		denied = denied || slices.Contains(strings.Fields(rest), "io_uring_setup")
	}
	if !denied {
		t.Errorf("SystemCallFilter %q still allows io_uring_setup: a ring opens sockets RestrictAddressFamilies= never sees", filters)
	}
	for key, want := range map[string][]string{
		"DirectoryNotEmpty": {exchangeTree + "/in", exchangeTree + "/work"},
		"PathExists":        {exchangeTree + "/txn.json"},
	} {
		for _, p := range want {
			if !slices.Contains(path["Path"][key], p) {
				t.Errorf("the path unit has no %s=%s: what starts the applier, or its recovery at boot, would not", key, p)
			}
		}
	}
	if !slices.Contains(path["Install"]["WantedBy"], "multi-user.target") {
		t.Errorf("the path unit is not WantedBy=multi-user.target: %q", path["Install"]["WantedBy"])
	}
}

// Each capability on a line of its own, with a reason in the design's
// table, and that reason, word for word, in the comment directly above
// it in the unit (the manager ORs the lines together): a capability
// added without a reason, two on one line sharing one, or a comment
// that says other than the design, fails here.
func TestEachCapabilityCarriesItsReason(t *testing.T) {
	reasons := map[string]string{}
	for _, r := range designRows(t) {
		if r.unit == serviceUnit && r.key == "CapabilityBoundingSet" {
			reasons[r.value] = r.why
		}
	}
	if len(reasons) == 0 {
		t.Fatal("the design's table grants no capability; the verifier's child could not change uid")
	}
	var caps []string
	for _, l := range readUnit(t, serviceUnit) {
		if l.Key != "CapabilityBoundingSet" {
			continue
		}
		f := strings.Fields(l.Value)
		if len(f) != 1 || !strings.HasPrefix(f[0], "CAP_") {
			t.Errorf("CapabilityBoundingSet=%q: one capability per line, never a reset or a ~", l.Value)
			continue
		}
		switch rest, ok := strings.CutPrefix(l.Comment, f[0]+":"); {
		case reasons[f[0]] == "":
			t.Errorf("%s has no reason in the design's table", f[0])
		case !ok:
			t.Errorf("the comment above %s does not open with %q: %q", f[0], f[0]+":", l.Comment)
		case plain(rest) != plain(reasons[f[0]]):
			t.Errorf("the comment above %s is not its row's reason:\n  unit:   %s\n  design: %s", f[0], plain(rest), plain(reasons[f[0]]))
		}
		caps = append(caps, f[0])
	}
	if want := slices.Sorted(maps.Keys(reasons)); !slices.Equal(slices.Sorted(slices.Values(caps)), want) {
		t.Errorf("capabilities %q, the design's %q", caps, want)
	}
}

// The units and the tree agree, across the design's two tables: what
// the path unit watches is a directory tmpfiles.d makes, or the record
// in its base; the service may write the tree and /etc/motel; and
// each watched directory counts every name in it — DirectoryNotEmpty=
// skips the names systemd takes for hidden or backup files, and the
// three globs beside it are what count those.
func TestTheUnitsWatchAndWriteTheTree(t *testing.T) {
	dirs := map[string]bool{}
	for _, e := range readTmpfiles(t) {
		dirs[e.path] = true
	}
	path := values(readUnit(t, pathUnit))["Path"]
	globs := path["PathExistsGlob"]
	for _, d := range path["DirectoryNotEmpty"] {
		if !dirs[d] {
			t.Errorf("DirectoryNotEmpty=%s: tmpfiles.d does not make it", d)
		}
		for _, g := range []string{"/*", "/.[!.]*", "/..?*"} {
			if !slices.Contains(globs, d+g) {
				t.Errorf("no PathExistsGlob=%s%s: an entry named so in %s would never start a run", d, g, d)
			}
		}
	}
	for _, g := range globs {
		if !slices.Contains(path["DirectoryNotEmpty"], filepath.Dir(g)) {
			t.Errorf("PathExistsGlob=%s watches a directory that is not one of the watched", g)
		}
	}
	for _, p := range path["PathExists"] {
		if !dirs[filepath.Dir(p)] {
			t.Errorf("PathExists=%s: not in a directory tmpfiles.d makes", p)
		}
	}
	rw := strings.Fields(strings.Join(values(readUnit(t, serviceUnit))["Service"]["ReadWritePaths"], " "))
	for _, need := range []string{"/etc/motel", exchangeTree} {
		if !slices.Contains(rw, need) {
			t.Errorf("ReadWritePaths %q lacks %s", rw, need)
		}
	}
	if !dirs[exchangeTree] {
		t.Error("tmpfiles.d does not make the base directory the service writes")
	}
}

// The tmpfiles.d file makes exactly the Paths table's rows whose
// "Creates" column is tmpfiles.d, each with the table's mode and owner,
// as a directory with no age.
func TestTmpfilesIsThePathsTable(t *testing.T) {
	want := designTmpfilesRows(t)
	got := map[string]string{}
	for _, e := range readTmpfiles(t) {
		if e.typ != "d" || e.age != "-" {
			t.Errorf("%s: type %q age %q, want a directory (d) with no age (-)", e.path, e.typ, e.age)
		}
		if _, dup := got[e.path]; dup {
			t.Errorf("%s: twice", e.path)
		}
		got[e.path] = e.mode + " " + e.user + ":" + e.group
	}
	if !reflect.DeepEqual(got, want) {
		for _, p := range slices.Sorted(maps.Keys(want)) {
			if got[p] != want[p] {
				t.Errorf("%s: tmpfiles.d says %q, the Paths table %q", p, got[p], want[p])
			}
		}
		for _, p := range slices.Sorted(maps.Keys(got)) {
			if _, ok := want[p]; !ok {
				t.Errorf("%s: tmpfiles.d makes it, and the Paths table does not say tmpfiles.d creates it", p)
			}
		}
	}
}

// The reader of the Paths table, on tables that would fool a careless
// one: a cell with a pipe in it is a row of another shape, and stops
// the test rather than being read short.
func TestThePathsTableReader(t *testing.T) {
	head := pathsMarker + "\n| Path | Mode | Owner | Creates | Writes | Reads | Removes |\n|---|---|---|---|---|---|---|\n"
	for _, tc := range []struct {
		name, doc string
		want      map[string]string
		err       string
	}{
		{"the base and a child, to the first line that is not a row", head +
			"| `/var/lib/motel-box/` | 2750 | root:motel | tmpfiles.d | — | — | — |\n" +
			"| `…/in/` | 0770 | root:motel | tmpfiles.d | handler | applier | applier |\n" +
			"| `…/txn.json` | 0600 | root:motel | applier | applier | applier | applier |\n\n" +
			"| `…/out/` | 2750 | root:motel | tmpfiles.d | applier | handler | — |\n",
			map[string]string{"/var/lib/motel-box": "2750 root:motel", "/var/lib/motel-box/in": "0770 root:motel"}, ""},
		{"no marker", "| Path | Mode | Owner | Creates | Writes | Reads | Removes |\n|---|---|---|---|---|---|---|\n", nil, "no line"},
		{"the marker twice", head + "\n" + head, nil, "twice"},
		{"the marker, but another table", pathsMarker + "\n| Situation | Action |\n|---|---|\n", nil, "no table"},
		{"no rows", head + "\n", nil, "no row"},
		{"a pipe in a cell", head + "| `…/in/` | 0770 | root:motel | tmpfiles.d | a | b | c | d |\n", nil, "8 cells"},
		{"a child before the base", head + "| `…/in/` | 0770 | root:motel | tmpfiles.d | — | — | — |\n", nil, "before the base"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTmpfilesRows(tc.doc)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("error %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("%v, want %v", got, tc.want)
			}
		})
	}
}

// The reader of the units' table, likewise.
func TestTheUnitTableReader(t *testing.T) {
	head := unitsMarker + "\n| Unit | Section | Line | Why |\n|---|---|---|---|\n"
	both := "| `.path` | `[Path]` | `PathExists=/x` | a |\n| `.service` | `[Service]` | `Type=oneshot` | |\n"
	for _, tc := range []struct {
		name, doc string
		want      []unitRow
		err       string
	}{
		{"both units, to the first line that is not a row", "## The applier unit\n\n```sh\n# a comment\n```\n\n" + head + both + "\n| `.path` | `[Path]` | `PathExists=/z` | c |\n",
			[]unitRow{{pathUnit, "Path", "PathExists", "/x", "a"}, {serviceUnit, "Service", "Type", "oneshot", ""}}, ""},
		{"no marker", "## The applier unit\n\n| Unit | Section | Line | Why |\n|---|---|---|---|\n" + both, nil, "no line"},
		{"one unit only", head + "| `.path` | `[Path]` | `PathExists=/x` | a |\n", nil, "both units"},
		{"a pipe in a cell", head + both + "| `.path` | `[Path]` | `PathExists=/x` | a | b |\n", nil, "5 cells"},
		{"an unknown unit", head + both + "| `.timer` | `[Timer]` | `OnCalendar=daily` | |\n", nil, "unknown unit"},
		{"a line that is not a key", head + both + "| `.path` | `[Path]` | `PathExists` | |\n", nil, "not a key"},
		{"a section not in brackets", head + both + "| `.path` | `Path` | `PathExists=/x` | |\n", nil, "section"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseUnitTable(tc.doc)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("error %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("%v, want %v", got, tc.want)
			}
		})
	}
}

// The unit reader refuses what it does not read the way the manager
// does, and ties a comment to the line directly under it only.
func TestTheUnitReader(t *testing.T) {
	for _, tc := range []struct{ name, unit, err string }{
		{"a continued line", "[Service]\nExecStart=/bin/a \\\n  b\n", "continued"},
		{"a key outside a section", "Type=oneshot\n[Service]\n", "outside a section"},
		{"a line with no =", "[Service]\noneshot\n", "not a key"},
		{"an empty key", "[Service]\n=x\n", "not a key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseUnit(strings.NewReader(tc.unit)); err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("error %v, want %q", err, tc.err)
			}
		})
	}
	lines, err := parseUnit(strings.NewReader("[Service]\n# a: kept\nA=1\n# b: dropped by the blank line\n\nB=2\n; c: kept too\nC=3\nD=4\n"))
	if err != nil {
		t.Fatal(err)
	}
	comments := map[string]string{}
	for _, l := range lines {
		comments[l.Key] = l.Comment
	}
	if want := map[string]string{"A": "a: kept", "B": "", "C": "c: kept too", "D": ""}; !reflect.DeepEqual(comments, want) {
		t.Fatalf("comments %q, want %q", comments, want)
	}
}

// unitLine is one Key=Value line of a unit file, with its section and
// the comment lines directly above it.
type unitLine struct {
	Section, Key, Value, Comment string
}

func readUnit(t *testing.T, name string) []unitLine {
	t.Helper()
	f, err := os.Open(filepath.Join(packagingDir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // read only
	lines, err := parseUnit(f)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return lines
}

// parseUnit reads a unit file the way the manager does for what these
// files say: [Section], Key=Value, comments (# or ;) and blank lines.
// No continuation lines: none is written, and a reader that skipped
// one would read its tail as a key of its own.
func parseUnit(r io.Reader) ([]unitLine, error) {
	var lines []unitLine
	var section string
	var comment []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "":
			comment = nil
		case strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
			comment = append(comment, strings.TrimSpace(line[1:]))
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			section, comment = line[1:len(line)-1], nil
		case strings.HasSuffix(line, "\\"):
			return nil, fmt.Errorf("a continued line, which this reader does not follow: %q", line)
		default:
			k, v, ok := strings.Cut(line, "=")
			if !ok || strings.TrimSpace(k) == "" {
				return nil, fmt.Errorf("not a key: %q", line)
			}
			if section == "" {
				return nil, fmt.Errorf("a key outside a section: %q", line)
			}
			lines = append(lines, unitLine{section, strings.TrimSpace(k), strings.TrimSpace(v), strings.Join(comment, "\n")})
			comment = nil
		}
	}
	return lines, sc.Err()
}

// plain is text as a comment carries it: no backquotes, and one space
// wherever the design's line breaks or the comment's wrapping put any.
func plain(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "`", "")), " ")
}

// values is section → key → each line's value, in file order.
func values(lines []unitLine) map[string]map[string][]string {
	out := map[string]map[string][]string{}
	for _, l := range lines {
		if out[l.Section] == nil {
			out[l.Section] = map[string][]string{}
		}
		out[l.Section][l.Key] = append(out[l.Section][l.Key], l.Value)
	}
	return out
}

// unitRow is one row of the design's table of unit lines.
type unitRow struct{ unit, section, key, value, why string }

func designRows(t *testing.T) []unitRow {
	t.Helper()
	rows, err := parseUnitTable(readDesign(t))
	if err != nil {
		t.Fatalf("DESIGN-box.md: %v", err)
	}
	return rows
}

// designUnits is the design's table as unit → section → key → values,
// in table order.
func designUnits(t *testing.T) map[string]map[string]map[string][]string {
	t.Helper()
	out := map[string]map[string]map[string][]string{}
	for _, r := range designRows(t) {
		if out[r.unit] == nil {
			out[r.unit] = map[string]map[string][]string{}
		}
		if out[r.unit][r.section] == nil {
			out[r.unit][r.section] = map[string][]string{}
		}
		out[r.unit][r.section][r.key] = append(out[r.unit][r.section][r.key], r.value)
	}
	return out
}

// parseUnitTable reads the table in "The applier unit": the unit by
// its suffix, the section in brackets, the line, and why.
func parseUnitTable(doc string) ([]unitRow, error) {
	cells, err := tableRows(doc, unitsMarker, "| Unit | Section | Line | Why |")
	if err != nil {
		return nil, err
	}
	var rows []unitRow
	seen := map[string]bool{}
	for _, c := range cells {
		unit := "motel-box-apply" + strings.Trim(c[0], "`")
		if unit != pathUnit && unit != serviceUnit {
			return nil, fmt.Errorf("an unknown unit %s", c[0])
		}
		sec := strings.Trim(c[1], "`")
		if len(sec) < 2 || sec[0] != '[' || sec[len(sec)-1] != ']' {
			return nil, fmt.Errorf("a section not written [Section]: %s", c[1])
		}
		k, v, ok := strings.Cut(strings.Trim(c[2], "`"), "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("not a key: %s", c[2])
		}
		rows = append(rows, unitRow{unit, sec[1 : len(sec)-1], k, v, c[3]})
		seen[unit] = true
	}
	if !seen[pathUnit] || !seen[serviceUnit] {
		return nil, errors.New("the table must have rows for both units")
	}
	return rows, nil
}

// The two tables this file reads are marked in the document, each by
// a comment on the line before its header, so the reader needs to know
// nothing of markdown's headings or fences.
const (
	pathsMarker = "<!-- table: paths (box/units_test.go reads it) -->"
	unitsMarker = "<!-- table: applier units (box/units_test.go reads it) -->"
)

// tableRows is the cells of each row of the table on the lines after
// the marker: the header, its separator, then every line that is a
// row, up to the first that is not. The marker appears once; a row
// with another number of cells — a pipe inside a cell — is an error,
// never a short read.
func tableRows(doc, marker, header string) ([][]string, error) {
	lines := strings.Split(doc, "\n")
	at := slices.Index(lines, marker)
	if at < 0 {
		return nil, fmt.Errorf("no line %q", marker)
	}
	if slices.Index(lines[at+1:], marker) >= 0 {
		return nil, fmt.Errorf("%q twice", marker)
	}
	if at+2 >= len(lines) || lines[at+1] != header || !strings.HasPrefix(lines[at+2], "|---") {
		return nil, fmt.Errorf("no table %q on the lines after %q", header, marker)
	}
	n := strings.Count(header, "|") - 1
	var rows [][]string
	for _, line := range lines[at+3:] {
		if !strings.HasPrefix(line, "|") {
			break
		}
		cells := strings.Split(line, "|")
		if len(cells) != n+2 {
			return nil, fmt.Errorf("%d cells, not the table's %d, in %q", len(cells)-2, n, line)
		}
		row := make([]string, n)
		for i := range row {
			row[i] = strings.TrimSpace(cells[i+1])
		}
		rows = append(rows, row)
	}
	return rows, nil
}

type tmpfilesEntry struct{ typ, path, mode, user, group, age string }

func readTmpfiles(t *testing.T) []tmpfilesEntry {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(packagingDir, "motel-box.tmpfiles"))
	if err != nil {
		t.Fatal(err)
	}
	var out []tmpfilesEntry
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 6 {
			t.Fatalf("tmpfiles.d line %q: want type, path, mode, user, group and age", line)
		}
		out = append(out, tmpfilesEntry{f[0], f[1], f[2], f[3], f[4], f[5]})
	}
	if len(out) == 0 {
		t.Fatal("the tmpfiles.d file makes nothing")
	}
	return out
}

func readDesign(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("DESIGN-box.md")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// designTmpfilesRows is DESIGN-box.md's Paths table, the rows whose
// "Creates" column is tmpfiles.d, as path → "mode user:group".
func designTmpfilesRows(t *testing.T) map[string]string {
	t.Helper()
	rows, err := parseTmpfilesRows(readDesign(t))
	if err != nil {
		t.Fatalf("DESIGN-box.md: %v", err)
	}
	return rows
}

// parseTmpfilesRows reads the Paths table: "…/" in a path stands for
// the base directory, the first row under /var/lib.
func parseTmpfilesRows(doc string) (map[string]string, error) {
	cells, err := tableRows(doc, pathsMarker, "| Path | Mode | Owner | Creates | Writes | Reads | Removes |")
	if err != nil {
		return nil, err
	}
	rows := map[string]string{}
	base := ""
	for _, c := range cells {
		path := strings.TrimSuffix(strings.Trim(c[0], "`"), "/")
		if rest, ok := strings.CutPrefix(path, "…/"); ok {
			if base == "" {
				return nil, fmt.Errorf("%q comes before the base directory it is under", c[0])
			}
			path = base + "/" + rest
		} else if base == "" && strings.HasPrefix(path, "/var/lib/") {
			base = path
		}
		if c[3] == "tmpfiles.d" {
			rows[path] = c[1] + " " + c[2]
		}
	}
	if len(rows) == 0 {
		return nil, errors.New("no row the table says tmpfiles.d creates")
	}
	return rows, nil
}
