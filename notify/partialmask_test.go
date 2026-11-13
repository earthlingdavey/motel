package notify

import (
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// The topics and tokens below are made up for these tests.
const (
	pmTopic   = "ops-7f3a9c2e1b"                                            // gitleaks:allow — made up for this test
	pmNtfy    = "https://ntfy.sh/" + pmTopic                                // gitleaks:allow — made up for this test
	pmSlackTk = "Xq7Lm2Nv9Rt4Kw8Pz3Ab"                                      // gitleaks:allow — made up for this test
	pmSlack   = "https://hooks.slack.com/services/T0AB1/B0CD2/" + pmSlackTk // gitleaks:allow — made up for this test
	pmJSONSeg = "endpoint-for-alerts"
	pmJSON    = "https://alerts.example.com/hooks/" + pmJSONSeg + "?x=1"
	pmShort   = "https://ntfy.sh/ops7"
)

func pmChannels() ([]Channel, map[string]string) {
	return []Channel{
		{Name: "ops", Kind: "ntfy", Secret: "NOTIFY_OPS_URL"},
		{Name: "team", Kind: "slack", Secret: "NOTIFY_TEAM_URL"},
		{Name: "hook", Kind: "json", Secret: "NOTIFY_HOOK_URL"},
		{Name: "short", Kind: "ntfy", Secret: "NOTIFY_SHORT_URL"},
	}, map[string]string{
		"NOTIFY_OPS_URL":   pmNtfy,
		"NOTIFY_TEAM_URL":  pmSlack,
		"NOTIFY_HOOK_URL":  pmJSON,
		"NOTIFY_SHORT_URL": pmShort,
	}
}

// TestPartialMask: a channel URL's last path segment, the credential on
// its own (an ntfy topic, a Slack hook's token), is masked alone as
// [redacted:partial:KEY], in its encoded forms too; the whole URL keeps
// [redacted:KEY]. A json channel's path and a segment under 8
// characters are not taken.
func TestPartialMask(t *testing.T) {
	channels, secrets := pmChannels()
	te := newTestEngine(t)
	te.known = knownValues(secrets, channels)
	cases := []struct{ in, want string }{
		{"disk on " + pmTopic + " is full", "disk on [redacted:partial:NOTIFY_OPS_URL] is full"},
		{"posting to " + pmNtfy + " failed", "posting to [redacted:NOTIFY_OPS_URL] failed"},
		{"b64 " + base64.StdEncoding.EncodeToString([]byte(pmTopic)), "b64 [redacted:partial:NOTIFY_OPS_URL]"},
		{"hex " + hex.EncodeToString([]byte(pmTopic)), "hex [redacted:partial:NOTIFY_OPS_URL]"},
		{`json "` + pmTopic + `"`, `json "[redacted:partial:NOTIFY_OPS_URL]"`},
		{"token " + pmSlackTk, "token [redacted:partial:NOTIFY_TEAM_URL]"},
		{"hook " + pmSlack, "hook [redacted:NOTIFY_TEAM_URL]"},
		{"seg " + pmJSONSeg + " here", "seg " + pmJSONSeg + " here"},
		{"topic ops7 here", "topic ops7 here"},
	}
	for _, c := range cases {
		if got := te.clean(c.in); got != c.want {
			t.Errorf("clean(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// Through the engine to the post, as an external producer's summary.
	ev := deployFailed()
	ev.Summary = "check " + pmTopic
	te.report(ev)
	if got := te.drain(); len(got) != 1 || got[0].Annotations["summary"] != "check [redacted:partial:NOTIFY_OPS_URL]" {
		t.Fatalf("posts = %+v", got)
	}
}

// TestPartialKnownValuesNameNoSecret: the derived entries are labels of
// the marker alone. What reads the secrets (resolve, the missing-secret
// check, the _box slot) reads the file's map, never knownValues, so a
// partial: name is never a secret a user is told is missing; and every
// entry knownValues adds beyond the file's is one of them.
func TestPartialKnownValuesNameNoSecret(t *testing.T) {
	channels, secrets := pmChannels()
	known := knownValues(secrets, channels)
	var partial []string
	for _, kv := range known {
		k, _, _ := strings.Cut(kv, "=")
		if _, ok := secrets[k]; ok {
			continue
		}
		if !strings.HasPrefix(k, partialKeyPrefix) {
			t.Fatalf("an entry that is neither a secret nor partial: %q", k)
		}
		partial = append(partial, kv)
	}
	slices.Sort(partial)
	want := []string{
		"partial:NOTIFY_OPS_URL=" + pmTopic,
		"partial:NOTIFY_SHORT_URL=ops7", // the filter skips it: under 8
		"partial:NOTIFY_TEAM_URL=" + pmSlackTk,
	}
	if !slices.Equal(partial, want) {
		t.Fatalf("partial = %q, want %q", partial, want)
	}
	// A channel whose secret the file does not hold adds nothing.
	if got := knownValues(nil, channels); len(got) != 0 {
		t.Fatalf("no secrets: %q", got)
	}
}

// TestURLCredentials: the part of each kind's URL that is its
// credential on its own, past a fixed suffix, as written and decoded,
// and ntfy's auth parameter; nothing of a json URL.
func TestURLCredentials(t *testing.T) {
	cases := []struct {
		kind, in string
		want     []string
	}{
		{"ntfy", "https://ntfy.sh/topic-abc", []string{"topic-abc"}},
		{"ntfy", "https://ntfy.sh/topic-abc/", []string{"topic-abc"}},
		{"ntfy", "https://ntfy.sh/topic-abc#frag", []string{"topic-abc"}},
		{"ntfy", "https://ntfy.sh/topic-abc/publish", []string{"topic-abc"}},
		{"ntfy", "https://ntfy.sh/topic-abc/trigger", []string{"topic-abc"}},
		{"ntfy", "https://ntfy.sh/publish", []string{"publish"}}, // a topic named publish
		{"ntfy", "https://ntfy.sh/alerts%5Fops", []string{"alerts%5Fops", "alerts_ops"}},
		{"ntfy", "https://ntfy.sh/operator-alerts/%70ublish", []string{"operator-alerts"}}, // an escaped suffix is the suffix
		{"discord", "https://discord.com/api/webhooks/123/tok/%73lack", []string{"tok"}},
		{"ntfy", "https://ntfy.example.com/ops?auth=QmVhcmVyIHRrX2FiYw", []string{"ops", "QmVhcmVyIHRrX2FiYw"}}, // gitleaks:allow — made up for this test
		{"slack", "https://hooks.slack.com/services/T0/B0/tok", []string{"tok"}},
		{"slack", "https://hooks.slack.com/services/T0/B0/tok?auth=x", []string{"tok"}},
		{"discord", "https://discord.com/api/webhooks/123/tok", []string{"tok"}},
		{"discord", "https://discord.com/api/webhooks/123/tok/slack", []string{"tok"}},
		{"slack", "https://hooks.example.com/hook/slack", []string{"slack"}}, // Slack's own URL has no suffix
		{"json", "https://alerts.example.com/hooks/endpoint", nil},
		{"ntfy", "https://ntfy.sh/", nil},
		{"ntfy", "https://ntfy.sh", nil},
		{"ntfy", "not a url", nil},
		{"ntfy", "", nil},
	}
	for _, c := range cases {
		if got := urlCredentials(c.kind, c.in); !slices.Equal(got, c.want) {
			t.Errorf("urlCredentials(%s, %q) = %q, want %q", c.kind, c.in, got, c.want)
		}
	}
}

// TestPartialMaskPastASuffix: a topic before ntfy's /publish, and one
// written escaped in the file, are masked alone; a json line on the
// secret an ntfy line reads, listed first, takes nothing from it.
func TestPartialMaskPastASuffix(t *testing.T) {
	secrets := map[string]string{
		"NOTIFY_OPS_URL": "https://ntfy.sh/" + pmTopic + "/publish",
		"NOTIFY_ESC_URL": "https://ntfy.sh/alerts%5Fops%5F7f3a",
	}
	channels := []Channel{
		{Name: "raw", Kind: "json", Secret: "NOTIFY_OPS_URL"},
		{Name: "ops", Kind: "ntfy", Secret: "NOTIFY_OPS_URL"},
		{Name: "ops-crit", Kind: "ntfy", Secret: "NOTIFY_OPS_URL", Severity: SeverityCritical},
		{Name: "esc", Kind: "ntfy", Secret: "NOTIFY_ESC_URL"},
	}
	te := newTestEngine(t)
	te.known = knownValues(secrets, channels)
	for in, want := range map[string]string{
		"check " + pmTopic:    "check [redacted:partial:NOTIFY_OPS_URL]",
		"see alerts_ops_7f3a": "see [redacted:partial:NOTIFY_ESC_URL]",
	} {
		if got := te.clean(in); got != want {
			t.Errorf("clean(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestServeSendRefusesATopicAsName: a name, instance or source is the
// key and is not rewritten, so one holding a channel's topic is
// refused, as one holding its URL is.
func TestServeSendRefusesATopicAsName(t *testing.T) {
	withEngine(t)
	channels, secrets := pmChannels()
	theEngine.known = knownValues(secrets, channels)
	r := goodSend()
	r.Instance = pmTopic
	if _, err := callAdmin(t, serveSend, http.MethodPost, r); apiStatus(err) != http.StatusUnprocessableEntity || !strings.Contains(err.Error(), "filter masks") {
		t.Fatalf("a topic as the instance: %v", err)
	}
	r = goodSend()
	r.Source = pmTopic
	if _, err := callAdmin(t, serveSend, http.MethodPost, r); apiStatus(err) != http.StatusUnprocessableEntity || !strings.Contains(err.Error(), "filter masks") {
		t.Fatalf("a topic as the source: %v", err)
	}
}
