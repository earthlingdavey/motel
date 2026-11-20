package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/backups/record"
	"github.com/smallhoursorg/motel/backups/status"
)

var update = flag.Bool("update", false, "rewrite testdata/status-*.json from what status prints now")

// statusNow is when the golden reports were decided, with a fraction of
// a second and in a zone not UTC: the JSON gives the second, in UTC.
var statusNow = time.Date(2026, 9, 20, 17, 0, 0, 123456789, time.FixedZone("", 2*60*60))

func statusAgo(d time.Duration) time.Time { return statusNow.Add(-d) }

func statusAt(d time.Duration) *time.Time { t := statusAgo(d); return &t }

// goldenInput is a record with one app of every kind a status screen
// shows: sound, stale and failing with its drill failed, pending, and
// with no result on record; a warning of the run's and of the
// credential file; units running, one of them unrecognised.
func goldenInput() status.Input {
	const idA = "aaaaaaaa11111111aaaaaaaa11111111aaaaaaaa11111111aaaaaaaa11111111"
	const idB = "bbbbbbbb22222222bbbbbbbb22222222bbbbbbbb22222222bbbbbbbb22222222"
	fresh := &record.Snapshot{ID: idA, Time: statusAgo(10 * time.Minute), Seen: statusAt(9 * time.Minute)}
	old := &record.Snapshot{ID: idB, Time: statusAgo(30 * time.Hour), Seen: statusAt(9 * time.Minute)}
	proven := &record.Drill{Snapshot: *old, Time: statusAgo(48 * time.Hour)}
	return status.Input{
		Record: &record.Status{
			Started: statusAgo(11 * time.Minute), Finished: statusAgo(9 * time.Minute), Listed: statusAt(9 * time.Minute),
			Warning:   "shop: a database was busy",
			LastDrill: &record.Drill{Time: statusAgo(48 * time.Hour)},
			LastCheck: &record.Check{Time: statusAgo(48 * time.Hour), Group: "38/52", Class: record.CheckClean},
			Apps: map[string]*record.App{
				"blog": {Class: record.OK, Looked: "/var/lib/liveswap/blog/shared", Snapshot: fresh, LastOK: fresh, LastSnapshot: fresh, RestoreProven: proven},
				"shop": {
					Class: record.Failed, Detail: "the storage could not be reached (exit 1)", Looked: "/var/lib/liveswap/shop/shared",
					LastOK: old, LastSnapshot: old, RestoreProven: proven,
					RestoreDrill: &record.Drill{Snapshot: *old, Time: statusAgo(time.Hour), Detail: "no room to fetch it"},
				},
				"wiki": {Class: record.Pending, Looked: "/var/lib/liveswap/wiki/shared"},
				"gone": nil,
			},
		},
		Now:      statusNow,
		SetUp:    statusAgo(30 * 24 * time.Hour),
		Warnings: []string{"/etc/motel-backup/repository.env: AWS_DEFAULT_REGION is set twice: the last wins"},
		Running: []status.Running{
			{Role: "upload", App: "blog", State: "activating", Since: statusAgo(2 * time.Minute)},
			{Role: "run", State: "deactivating"},
		},
		Unrecognised: []string{"motel_backup_zzz.service"},
	}
}

// golden compares what printStatus wrote, as JSON, with the file
// testdata/name, which is the contract: equal, not a superset.
func golden(t *testing.T, name string, r status.Result) []byte {
	t.Helper()
	var out bytes.Buffer
	must(t, printStatus(&out, r, true))
	path := filepath.Join("testdata", name)
	if *update {
		must(t, os.MkdirAll("testdata", 0o755))
		must(t, os.WriteFile(path, out.Bytes(), 0o644))
	}
	want, err := os.ReadFile(path)
	must(t, err)
	if !bytes.Equal(out.Bytes(), want) {
		t.Errorf("status --json is not %s (go test -run %s -update rewrites it):\n%s", path, t.Name(), out.String())
	}
	return out.Bytes()
}

// The JSON's shape is pinned whole, and its lines are the text's, line
// for line: one Result is printed both ways.
func TestStatusJSONIsTheContract(t *testing.T) {
	r := status.Report(goldenInput())
	raw := golden(t, "status-report.json", r)

	var text bytes.Buffer
	must(t, printStatus(&text, r, false))
	var back struct {
		Lines []struct{ Text string }
	}
	must(t, json.Unmarshal(raw, &back))
	var said []string
	for _, l := range back.Lines {
		said = append(said, l.Text+"\n")
	}
	if strings.Join(said, "") != text.String() {
		t.Errorf("the JSON's lines are not the text:\n%s\nvs\n%s", strings.Join(said, ""), text.String())
	}
	if r.Healthy {
		t.Error("a report with a stale app is healthy")
	}
}

// Could not tell is JSON too, with why, and the warnings found before.
func TestStatusJSONCouldNotTell(t *testing.T) {
	why := errors.New("the record of the last run could not be read: unexpected end of JSON input")
	golden(t, "status-could-not-tell.json", status.CouldNotTell(statusNow, goldenInput().Warnings, why.Error()))
}

// What status says of the credential file, as root, names no value of
// it — a password, a storage key, a URL with a key in it — however the
// file is written; and the JSON holds no more than the text.
func TestStatusSaysNoSecretOfTheCredentialFile(t *testing.T) {
	const (
		password = "correct-horse-battery-staple-9f3a" // gitleaks:allow — made up for this test
		keyID    = "0045e7c1b2a3d4f50000000007"        // gitleaks:allow — made up for this test
		secret   = "K004xQmZ7pR2sT9vW3yA6bC8dE1fG5h"   // gitleaks:allow — made up for this test
	)
	for name, file := range map[string]string{
		"well formed, region wrong": "RESTIC_REPOSITORY=s3:https://s3.us-west-004.backblazeb2.com/bucket/box\n" +
			"RESTIC_PASSWORD=" + password + "\nAWS_ACCESS_KEY_ID=" + keyID + "\nAWS_SECRET_ACCESS_KEY=" + secret + "\nAWS_DEFAULT_REGION=us-east-1\n",
		"keys in the URL": "RESTIC_REPOSITORY=s3:https://" + keyID + ":" + secret + "@s3.us-west-004.backblazeb2.com/bucket\n" +
			"RESTIC_PASSWORD=" + password + "\n",
		"hand-edited lines": " RESTIC_PASSWORD = " + password + "\nRESTIC_PASSWORD=" + password + "\n" +
			"AWS_SECRET_ACCESS_KEY " + secret + "\nAWS_ACCESS_KEY_ID=\"" + keyID + "\nexport AWS_SESSION_TOKEN=" + password + "\n",
		"set to nothing": "RESTIC_REPOSITORY=\nRESTIC_PASSWORD=\nAWS_SECRET_ACCESS_KEY=" + secret + "\n",
		"refused whole":  "RESTIC_PASSWORD=" + password + "\x00\nAWS_SECRET_ACCESS_KEY=" + secret + "\xff\n",
	} {
		in := goldenInput()
		in.Warnings = envWarnings("/etc/motel-backup/repository.env", []byte(file), nil)
		if len(in.Warnings) == 0 {
			t.Errorf("%s: nothing was said of the file", name)
		}
		for _, asJSON := range []bool{false, true} {
			var out bytes.Buffer
			must(t, printStatus(&out, status.Report(in), asJSON))
			must(t, printStatus(&out, status.CouldNotTell(statusNow, in.Warnings, "no record"), asJSON))
			for _, s := range []string{password, keyID, secret} {
				if strings.Contains(out.String(), s) {
					t.Errorf("%s (json %v): status says %q:\n%s", name, asJSON, s, out.String())
				}
			}
		}
	}
}

// Root's status says what a run refuses of the file's mode, in the
// run's words and first; of a file root's alone it says nothing of it.
func TestStatusAsRootSaysWhatTheRunRefusesOfTheFilesMode(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("the file has to be root's, as on a box")
	}
	path := filepath.Join(t.TempDir(), "repository.env")
	must(t, os.WriteFile(path, []byte("RESTIC_REPOSITORY=s3:https://s3.example.com/bucket\nRESTIC_PASSWORD=the-secret\n"), 0o600))
	for _, w := range credentialWarnings(path) {
		if strings.HasPrefix(w, "the credential file ") {
			t.Fatalf("of a file root's alone: %q", w)
		}
	}
	must(t, os.Chmod(path, 0o644))
	want := "the credential file " + path + " is open to others than root (mode 0644), and it holds the repository's password and the storage key: chmod 0600 " + path + ", as root"
	if got := credentialWarnings(path); len(got) == 0 || got[0] != want {
		t.Fatalf("status said %q\nwant first %q", got, want)
	}
}

// A FIFO in the file's place is said, as the run refuses it, and not
// waited on: a plain open would hold status until interrupted.
func TestStatusAsRootDoesNotWaitOnAFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repository.env")
	must(t, syscall.Mkfifo(path, 0o600))
	said := make(chan []string, 1)
	go func() { said <- credentialWarnings(path) }()
	select {
	case got := <-said:
		want := path + ": could not be read: it is not a regular file (p---------)"
		if len(got) != 1 || got[0] != want {
			t.Fatalf("status said %q\nwant %q", got, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("status waited on a FIFO in the credential file's place")
	}
}

func TestStatusTakesJSONAlone(t *testing.T) {
	for _, tc := range []struct {
		args       []string
		ok, asJSON bool
	}{
		{nil, true, false},
		{[]string{"--json"}, true, true},
		{[]string{"-json"}, true, true},
		{[]string{"--json", "--json"}, false, false},
		{[]string{"now"}, false, false},
		{[]string{"--json=true"}, false, false},
		{[]string{"--"}, false, false},
	} {
		if got := arguments("status", tc.args); got != tc.ok {
			t.Errorf("arguments(status, %q) = %v", tc.args, got)
		}
		if asJSON, ok := statusArgs(tc.args); ok != tc.ok || asJSON != tc.asJSON {
			t.Errorf("statusArgs(%q) = %v, %v", tc.args, asJSON, ok)
		}
	}
}

// A stdout that takes nothing does not change could not tell: no setup
// here is still 3, and the failed print is said with it.
func TestStatusLeavesWithItsStatusWhateverStdoutTakes(t *testing.T) {
	if _, err := os.Lstat(config().EnvFile); err == nil {
		t.Skip("backups are set up on this machine")
	}
	r, w, err := os.Pipe()
	must(t, err)
	must(t, r.Close())
	must(t, w.Close()) // every write to it fails
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()
	for _, asJSON := range []bool{false, true} {
		err := showStatus(context.Background(), asJSON)
		// The text of this look is no line at all: nothing to fail.
		if exitStatus(err) != 3 || asJSON != strings.Contains(fmt.Sprint(err), "could not be printed") {
			t.Errorf("json %v: exits %d (%v), want 3 and the print named", asJSON, exitStatus(err), err)
		}
	}
}
