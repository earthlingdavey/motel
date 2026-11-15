package client

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/smallhoursorg/motel/liveswap/wire"
)

// A 403 to a person is told from needs and scope alone, made safe to
// print, with whoami named; the box's own sentence is not quoted, so
// an app named nowhere in the body is named nowhere in the message,
// and the 403 for an app that exists reads as the one for an app that
// does not. A deploy token's 403 does not send it to whoami.
func TestForbidden(t *testing.T) {
	person, _ := personCred(t)
	for _, c := range []struct {
		name   string
		cred   Credential
		body   string
		want   string
		absent string
	}{
		{"a viewer of one app asks another", person, `{"error":"this needs the viewer role on this app","needs":"viewer","scope":"app"}`,
			"127.0.0.1 does not allow your key this: it needs the viewer role on this app. `motelctl whoami` lists the roles you hold from here.", ""},
		{"the box's status", person, `{"error":"this needs the viewer role on the box","needs":"viewer","scope":"box"}`,
			"127.0.0.1 does not allow your key this: it needs the viewer role on the box. `motelctl whoami`", ""},
		{"a push's result", person, `{"error":"this needs a deploy token of the box","needs":"deployer","scope":"box"}`,
			"it needs a deploy token of the box.", ""},
		{"a sentence that names an app", person, `{"error":"this needs the viewer role on shop","needs":"viewer","scope":"app"}`,
			"it needs the viewer role on this app.", "shop"},
		{"words a later box may send", person, "{\"error\":\"x\",\"needs\":\"auditor\\u001b[2J\",\"scope\":\"team\\u202e\"}",
			"it needs the auditor\\x1b[2J role on a scope of kind team\\u202e.", "\x1b"},
		{"a deploy token where a key is needed", DeployToken("tok"), `{"error":"this needs a token signed by a declared key","needs":"key","scope":"box"}`,
			"127.0.0.1 does not allow the token in MOTEL_TOKEN this: it needs a token your own key signs. Unset MOTEL_TOKEN, or name your key with --key.", "whoami"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cl := &Client{Box: Box{Host: "127.0.0.1"}, Cred: c.cred}
			ans := &Answer{Code: http.StatusForbidden, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(c.body + "\n")}
			f := failure(t, cl.Read(ans, &wire.Status{}))
			if f.Exit != ExitForbidden || !strings.Contains(f.Message, c.want) || (c.absent != "" && strings.Contains(f.Message, c.absent)) {
				t.Errorf("exit %d\n%s\nwant 5 with\n%s", f.Exit, f.Message, c.want)
			}
			if !bytes.Equal(f.Body, ans.Body) {
				t.Errorf("--json would print %q, not the box's body", f.Body)
			}
		})
	}
}

// Motel-Protocol is read where an answer carries it: a later number
// than this client's is said once on stderr, and the command carries
// on; this client's own, none, or one that does not read say nothing.
func TestProtocolNote(t *testing.T) {
	for _, c := range []struct {
		name, header string
		noted        bool
	}{
		{"a later protocol", "2", true},
		{"this client's", "1", false},
		{"none: an answer to no verified token", "", false},
		{"not a number", "two", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newBox(t, DeployToken("tok"), func(_ int, w http.ResponseWriter, _ *http.Request) {
				if c.header != "" {
					w.Header().Set("Motel-Protocol", c.header)
				}
				motelJSON(w, 200, `{"app":"blog","phase":"running"}`)
			})
			var notes bytes.Buffer
			b.client.Notes = &notes
			for range 2 {
				ans, err := b.client.Get(context.Background(), "/blog", nil)
				if err != nil || b.client.Read(ans, &wire.Status{}) != nil {
					t.Fatalf("the command did not carry on: %v", err)
				}
			}
			want := ""
			if c.noted {
				want = "motelctl: 127.0.0.1 speaks protocol 2 and this motelctl 1: the box is newer than this client, which may not show all it answers. Carrying on; a newer motelctl reads it all.\n"
			}
			if notes.String() != want {
				t.Errorf("stderr %q, want %q", notes.String(), want)
			}
		})
	}
}

// whoami's and apps' bodies are motel's by their fields: a 200 without
// them is someone else's answer.
func TestReadPeoplesBodies(t *testing.T) {
	cl := &Client{Box: Box{Host: "127.0.0.1"}}
	for _, c := range []struct {
		name string
		v    any
		body string
		ok   bool
	}{
		{"whoami", &wire.WhoAmI{}, `{"name":"alice","key":"SHA256:x","roles":[],"box":"b","version":"","protocol":1,"token_expires_at":"2026-11-15T10:00:00Z"}`, true},
		{"whoami with no roles", &wire.WhoAmI{}, `{"name":"alice","key":"SHA256:x","box":"b"}`, false},
		{"whoami with no name", &wire.WhoAmI{}, `{"key":"SHA256:x","roles":[],"box":"b"}`, false},
		{"apps", &wire.Apps{}, `{"apps":["a","b"]}`, true},
		{"no apps", &wire.Apps{}, `{"apps":[]}`, true},
		{"apps as null", &wire.Apps{}, `{"apps":null}`, false},
		{"an empty object", &wire.Apps{}, `{}`, false},
	} {
		ans := &Answer{Code: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(c.body)}
		err := cl.Read(ans, c.v)
		if (err == nil) != c.ok {
			t.Errorf("%s: %v, want taken: %v", c.name, err, c.ok)
		}
		if err != nil && failure(t, err).Exit != ExitUnknown {
			t.Errorf("%s: exit %d, want 3", c.name, failure(t, err).Exit)
		}
	}
}
