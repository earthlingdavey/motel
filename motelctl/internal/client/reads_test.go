package client

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// TestNoWaitReturnsThe429: the TUI's client hands a 429 back at once,
// with the wait the box asked for, so its engine waits it out for every
// read; a command's client waits it out itself (TestGet's own cases).
func TestNoWaitReturnsThe429(t *testing.T) {
	b := newBox(t, DeployToken("tok"), func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "12")
		motelJSON(w, http.StatusTooManyRequests, `{"error":"slow down"}`)
	})
	b.client.NoWait = true
	_, err := b.client.Get(context.Background(), "/blog", nil)
	f := failure(t, err)
	if f.Status != http.StatusTooManyRequests || f.RetryAfter != 12*time.Second || f.Exit != ExitUnknown {
		t.Errorf("failure %+v", f)
	}
	if len(b.slept) != 0 || b.hits.Load() != 1 {
		t.Errorf("slept %v over %d requests, want none over one", b.slept, b.hits.Load())
	}
}

// TestReadsCheckWhatWasAsked: the read functions take an answer only
// about what was asked, as the commands always have.
func TestReadsCheckWhatWasAsked(t *testing.T) {
	b := newBox(t, DeployToken("tok"), func(_ int, w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("deploy") != "" {
			motelJSON(w, 200, `{"version":"v2","status":"succeeded"}`)
			return
		}
		motelJSON(w, 200, `{"app":"shop","phase":"idle"}`)
	})
	ctx := context.Background()
	if _, _, err := b.client.Status(ctx, App{Name: "blog", Path: "/blog"}, ""); failure(t, err).Exit != ExitUnknown {
		t.Errorf("another app's status was taken")
	}
	if _, _, err := b.client.Deploy(ctx, App{Name: "blog", Path: "/blog"}, "v1"); failure(t, err).Exit != ExitUnknown {
		t.Errorf("another version's record was taken")
	}
	if _, _, err := b.client.Status(ctx, App{Name: "blog", Path: "/blog"}, "7d"); failure(t, err).Exit != ExitUsage {
		t.Errorf("a range outside the box's was sent")
	}
}
