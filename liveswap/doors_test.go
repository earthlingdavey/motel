package liveswap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/smallhoursorg/motel/liveswap/wire"
)

// strs is xs as plain strings: a test's view of typed text.
func strs[T ~string](xs []T) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = string(x)
	}
	return out
}

// doorBox is the handler's one app, demo, with the test people (alice
// views demo, carol views the box, erin reads its logs), an env_file, a
// 40-line tail, and v1 deployed over HTTP.
type doorBox struct {
	h      *Handler
	rig    *testRig
	people *testPeople
}

func newDoorBox(t *testing.T, env string) *doorBox {
	t.Helper()
	h, rig := newTestHandler(t)
	b := &doorBox{h: h, rig: rig, people: newTestPeople(t)}
	h.people = b.people.reg
	rig.spec.deployLogLines = 40
	rig.spec.envFile = filepath.Join(t.TempDir(), "demo.env")
	must(t, os.WriteFile(rig.spec.envFile, []byte(env), 0o600))
	if code, body := b.post(t, "v1", false); code != http.StatusOK {
		t.Fatalf("deploying v1: %d %s", code, body)
	}
	return b
}

// post is POST /demo with the app's deploy token, its phases streamed
// as lines when stream: the code and the bytes.
func (b *doorBox) post(t *testing.T, version string, stream bool) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/demo", strings.NewReader(`{"url":"https://x/`+version+`.tgz","version":"`+version+`"}`))
	req.Header.Set("Authorization", "Bearer "+appToken(t))
	if stream {
		req.Header.Set("Accept", ndjson)
	}
	return b.serve(t, req)
}

func (b *doorBox) serve(t *testing.T, req *http.Request) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	if err := b.h.ServeHTTP(w, req, nil); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	return w.Code, w.Body.String()
}

// sinks is every answer a reader can get of demo now, and every record
// on disk, by where it was read.
func (b *doorBox) sinks(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{
		"GET /demo":                  do(t, b.h, http.MethodGet, "/demo", appToken(t), "").Body.String(),
		"GET /demo, a viewer":        sendTo(t, b.h, http.MethodGet, "/demo", token(b.people.alice)).Body.String(),
		"GET /_motel/apps":           sendTo(t, b.h, http.MethodGet, "/_motel/apps", token(b.people.carol)).Body.String(),
		"GET /_motel/apps/demo/logs": sendTo(t, b.h, http.MethodGet, "/_motel/apps/demo/logs", token(b.people.erin)).Body.String(),
	}
	entries, err := os.ReadDir(b.rig.spec.dirs.deploys)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	for _, e := range entries {
		version := strings.TrimSuffix(e.Name(), ".json")
		raw, err := os.ReadFile(filepath.Join(b.rig.spec.dirs.deploys, e.Name()))
		must(t, err)
		out["record of "+version] = string(raw)
		out["GET /demo?deploy="+version] = do(t, b.h, http.MethodGet, "/demo?deploy="+version, appToken(t), "").Body.String()
	}
	return out
}

// readable is whether a reader of body can read value: one of the forms
// the filter knows, in its bytes or in a string the JSON decodes to.
func readable(body, value string) bool {
	texts := []string{body}
	for _, line := range strings.Split(body, "\n") {
		var v any
		if json.Unmarshal([]byte(line), &v) != nil {
			continue
		}
		texts = append(texts, jsonStrings(v, nil)...)
	}
	for _, text := range texts {
		for _, f := range secretForms(value) {
			if len(f) >= secretMinLen && strings.Contains(text, f) {
				return true
			}
		}
	}
	return false
}

// jsonStrings is every string, keys included, in the decoded JSON v.
func jsonStrings(v any, out []string) []string {
	switch x := v.(type) {
	case string:
		out = append(out, x)
	case []any:
		for _, e := range x {
			out = jsonStrings(e, out)
		}
	case map[string]any:
		for k, e := range x {
			out = jsonStrings(e, append(out, k))
		}
	}
	return out
}

// noneReadable fails the test for each sink a value is readable in.
func noneReadable(t *testing.T, sinks map[string]string, values ...string) {
	t.Helper()
	names := make([]string, 0, len(sinks))
	for n := range sinks {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		for _, v := range values {
			if readable(sinks[n], v) {
				t.Errorf("%s holds %q: %.600s", n, v, sinks[n])
			}
		}
	}
}

// The canary: an app prints two values of its env_file (one plain, one
// with a quote and a backslash, which JSON escapes) at every place the
// app's text reaches an answer or a record: its journal while a deploy
// fails, its health probe's body and Location, and its journal at the
// logs read. Neither is readable in any answer of any reader, streamed
// or not, nor in any record on disk; each place's keys are named.
func TestCanaryAtEveryDoor(t *testing.T) {
	const plain, quoted = "canary-printed-by-the-app-7Hq2", `canary"quoted\by-the-app-9Lm4`
	b := newDoorBox(t, "PLAIN="+plain+"\nQUOTED="+quoted+"\n")
	esc, _ := json.Marshal(quoted)
	tail := []string{"boot " + plain, `{"msg":` + string(esc) + `}`, "then " + quoted}
	// A failed health gate: the journal and the probe's answer.
	b.rig.ma.journal = &fakeJournal{lines: tail}
	b.rig.prober.err = fmt.Errorf("not healthy within deadline 5m0s: %w", &probeError{status: 503, location: "https://example.test/" + plain + "?q=" + quoted, body: `{"err":` + string(esc) + `} ` + plain})
	answers := map[string]string{}
	_, answers["a failed deploy's answer"] = b.post(t, "v2", false)
	_, answers["a failed deploy, streamed"] = b.post(t, "v3", true)
	// The logs read.
	b.rig.ma.logs = &scriptedLogs{journal: demoJournal(3, func(i int) string { return tail[i] })}
	sinks := b.sinks(t)
	for k, v := range answers {
		sinks[k] = v
	}
	noneReadable(t, sinks, plain, quoted)
	// Each place still says something, and names the keys it replaced.
	var st wire.Status
	must(t, json.Unmarshal([]byte(sinks["GET /demo"]), &st))
	if strings.Join(st.RedactedEnv, ",") != "PLAIN,QUOTED" {
		t.Errorf("GET /demo: %s", sinks["GET /demo"])
	}
	var logs wire.Logs
	must(t, json.Unmarshal([]byte(sinks["GET /_motel/apps/demo/logs"]), &logs))
	if len(logs.Lines) != 3 || strings.Join(logs.RedactedEnv, ",") != "PLAIN,QUOTED" {
		t.Errorf("the logs read: %s", sinks["GET /_motel/apps/demo/logs"])
	}
	failed := 0
	for k, body := range sinks {
		var rec wire.DeployResult
		if !strings.HasPrefix(k, "record of ") || json.Unmarshal([]byte(body), &rec) != nil || rec.Status != "failed" {
			continue
		}
		failed++
		if len(rec.RedactedEnv) == 0 {
			t.Errorf("%s: %s", k, body)
		}
	}
	if failed == 0 {
		t.Error("no failed deploy's record was read")
	}
}

// The logs door reads no more of a long line than the logs read always
// has: a value past the first 4 KiB and its margin is in no part of the
// filter's input, so it is named in no redacted_env; one inside what is
// read is replaced and named; what is sent is at most 4 KiB, ending in
// "…".
func TestLogsDoorReadsABoundedPieceOfALongLine(t *testing.T) {
	const near, far = "made-up-value-in-the-margin-01", "made-up-value-far-down-the-line-02"
	line := strings.Repeat("a", logsLineMax+100) + " " + near + " " + strings.Repeat("b", 20000) + " " + far + " " + strings.Repeat("c", 20000)
	if len(line) < 40000 {
		t.Fatal("the premise: a line longer than the filter reads")
	}
	seen := map[string]bool{}
	out := filterLogLine(NewRedactor([]string{"NEAR=" + near, "FAR=" + far}), line, seen)
	if len(out) > logsLineMax || !strings.HasSuffix(out, "…") || strings.Contains(out, near) || strings.Contains(out, far) {
		t.Errorf("sent %d bytes, ending %q", len(out), out[max(0, len(out)-20):])
	}
	if !seen["NEAR"] || seen["FAR"] {
		t.Errorf("the keys named %v: want NEAR, read in the margin, and not FAR, past what is read", seen)
	}
}

// wirePath is the wire package's import path.
const wirePath = "github.com/smallhoursorg/motel/liveswap/wire"

// madeIn is every place in the Go files under dirs (none of them tests),
// each named relative to base, where an AppText is made from something
// that is not a constant, by the file it is in, in the forms a walk of
// the source sees: a conversion written with the package's name or any
// name it is imported under, in parentheses or not, through a type or
// an alias declared outside wire, or as a generic function's explicit
// type argument. It is not complete and is not made so: a dot import,
// an alias declared in wire, an inferred type argument, a scan or a
// decode into such a field make one with nothing here to see, and are
// review's.
func madeIn(t *testing.T, base string, dirs ...string) []string {
	t.Helper()
	var conversions []string
	fset := gotoken.NewFileSet()
	base, err := filepath.Abs(base)
	must(t, err)
	for _, root := range dirs {
		root, err := filepath.Abs(root)
		must(t, err)
		err = filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(base, path)
			wireNames := map[string]bool{}
			for _, imp := range f.Imports {
				if p, _ := strconv.Unquote(imp.Path.Value); p == wirePath {
					if imp.Name != nil {
						wireNames[imp.Name.Name] = true
					} else {
						wireNames["wire"] = true
					}
				}
			}
			// isAppText is whether e names AppText.
			isAppText := func(e ast.Expr) bool {
				for {
					p, ok := e.(*ast.ParenExpr)
					if !ok {
						break
					}
					e = p.X
				}
				switch x := e.(type) {
				case *ast.SelectorExpr:
					pkg, ok := x.X.(*ast.Ident)
					return ok && wireNames[pkg.Name] && x.Sel.Name == "AppText"
				case *ast.Ident:
					return f.Name.Name == "wire" && x.Name == "AppText"
				case *ast.ArrayType: // []wire.AppText(…)
					s, ok := x.Elt.(*ast.SelectorExpr)
					return ok && wireNames[fmt.Sprint(s.X)] && s.Sel.Name == "AppText"
				}
				return false
			}
			ast.Inspect(f, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.TypeSpec: // type x = wire.AppText, type x wire.AppText
					if isAppText(x.Type) && f.Name.Name != "wire" {
						conversions = append(conversions, rel)
					}
				case *ast.IndexExpr: // conv[wire.AppText](s)
					if isAppText(x.Index) {
						conversions = append(conversions, rel)
					}
				case *ast.IndexListExpr:
					for _, ix := range x.Indices {
						if isAppText(ix) {
							conversions = append(conversions, rel)
						}
					}
				case *ast.CallExpr:
					if isAppText(x.Fun) {
						if len(x.Args) == 1 {
							if lit, ok := x.Args[0].(*ast.BasicLit); ok && lit.Kind == gotoken.STRING {
								return true // a constant: needs no filter
							}
						}
						conversions = append(conversions, rel)
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return conversions
}

// The directories that import wire to build answers: liveswap's own,
// the box webhook, notify, backups/status and cmd/moteld.
var guardDirs = []string{".", filepath.Join("..", "box"), filepath.Join("..", "notify"), filepath.Join("..", "backups", "status"), filepath.Join("..", "cmd", "moteld")}

// The app's text has a type only the filter makes: no conversion to
// wire.AppText is written anywhere on the box but in redact.go, so a
// field of that type cannot be given the app's bytes as they came. A
// door added elsewhere fails here.
func TestAppTextOnlyFromTheDoor(t *testing.T) {
	conv := madeIn(t, "..", guardDirs...)
	if !allAre(conv, filepath.Join("liveswap", "redact.go")) || len(conv) == 0 {
		t.Errorf("wire.AppText is converted to in %v; want liveswap/redact.go alone (the door)", conv)
	}
}

func allAre(got []string, allowed ...string) bool {
	for _, g := range got {
		ok := false
		for _, a := range allowed {
			ok = ok || g == a
		}
		if !ok {
			return false
		}
	}
	return true
}

// The guard sees what it is for, in the forms a walk can see: a
// conversion with the package's name, in parentheses, under a renamed
// import, through a declared type or alias and through a generic
// function.
func TestGuardsSeeANewMaker(t *testing.T) {
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "door.go"), []byte(`package liveswap
import (
	"github.com/smallhoursorg/motel/liveswap/wire"
	w "github.com/smallhoursorg/motel/liveswap/wire"
)
type mine = wire.AppText
type also w.AppText
func conv[T ~string](s string) T { return T(s) }
func f(s string) (wire.AppText, wire.AppText, w.AppText, wire.AppText) {
	return wire.AppText(s), (wire.AppText)(s), w.AppText(s), conv[wire.AppText](s)
}
func g() wire.AppText { return wire.AppText("a constant") }
`), 0o600))
	if got := madeIn(t, dir, dir); fmt.Sprint(got) != "[door.go door.go door.go door.go door.go door.go]" {
		t.Errorf("found %v", got)
	}
}

// A failed deploy's tail passes the door line by line: a rule that
// reaches across white space (a bearer token) never joins two lines or
// masks the next line's first word, and a line that
// holds a line break of its own stays one line, as the journal kept it.
func TestDoorTailKeepsItsLines(t *testing.T) {
	d := &door{r: NewRedactor(nil), seen: map[string]bool{}}
	in := []string{"refreshing token", "ConnectionPoolManagerStarted ok", "db password:", "connection established", "one\ntwo"}
	got := strs(d.lines(in))
	if strings.Join(got, "|") != strings.Join(in, "|") {
		t.Errorf("the tail through the door: %q", got)
	}
}

// redacted_env never spells a value: a key's name that is another key's
// value is that value's marker in the list.
func TestRedactedEnvSpellsNoValue(t *testing.T) {
	d := &door{r: NewRedactor([]string{"PRIMARY_KEY_NAME=STRIPE_LIVE_KEY", "STRIPE_LIVE_KEY=sk-value-0123456789"}), seen: map[string]bool{}}
	if got := d.text("boot sk-value-0123456789"); strings.Contains(string(got), "sk-value") {
		t.Fatalf("the text: %q", got)
	}
	keys := d.keys()
	if len(keys) != 1 || keys[0] == "STRIPE_LIVE_KEY" || !strings.HasPrefix(keys[0], "[") {
		t.Errorf("redacted_env: %q", keys)
	}
}

// An app that prints the git sha it was deployed as and its artifact's
// digest, in its journal and in its probe's answer, has both read as
// written in the deploy's detail and its record: no rule of the filter
// matches a commit or a digest.
func TestFailedDeploysOwnNamesStandInItsDetail(t *testing.T) {
	b := newDoorBox(t, "TOKEN=some-value-of-the-app-01\n")
	version := "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b"
	sum := sha256.Sum256([]byte("the artifact of " + version))
	pin := hex.EncodeToString(sum[:])
	said := "built " + version + " artifact sha256 " + pin
	b.rig.ma.journal = &fakeJournal{lines: []string{said}}
	b.rig.prober.err = fmt.Errorf("not healthy within deadline 5m0s: %w", &probeError{status: 503, body: said})
	if err := b.rig.ma.Deploy(context.Background(), deployRequest{url: "https://example.test/x.tgz", version: version, sha256: pin, by: "test"}); err == nil {
		t.Fatal("the deploy should have failed its health gate")
	}
	last := b.rig.ma.status().LastDeploy
	if last == nil || last.Detail == nil || last.Detail.Probe == nil || len(last.Detail.LogTail) != 1 ||
		string(last.Detail.LogTail[0]) != said || string(last.Detail.Probe.Body) != said {
		t.Fatalf("last_deploy: %+v", last)
	}
	if rec := b.sinks(t)["record of "+version]; !strings.Contains(rec, `"log_tail":["`+said+`"]`) {
		t.Errorf("the record: %s", rec)
	}
}

// A logs line that holds the git sha its release was deployed as reads
// as written, the line's release beside it: no rule of the filter
// matches a commit.
func TestLogsLinesReleaseStandsInItsText(t *testing.T) {
	version := "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b"
	j := demoJournal(1, func(int) string { return "booted " + version })
	j[0].userUnit = "motel-demo." + version + ".0a1b2c3d0a1b2c3d.service"
	h, people, _, _ := logsRig(t, j)
	w := sendTo(t, h, "GET", "/_motel/apps/demo/logs", token(people.erin))
	if got := decodeLogs(t, w); len(got.Lines) != 1 || got.Lines[0].Release != version || string(got.Lines[0].Text) != "booted "+version {
		t.Errorf("%d %s", w.Code, w.Body)
	}
}
