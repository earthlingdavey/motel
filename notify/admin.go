package notify

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"

	"github.com/smallhoursorg/motel/liveswap"
	"github.com/smallhoursorg/motel/liveswap/backupdecl"
)

func init() {
	caddy.RegisterModule(adminAPI{})
}

// adminAPI is notify's two routes on the admin API, which listens on
// /run/motel/admin.sock: root and the motel uid reach it, and no app
// does (the path is not in any app's view). They are how a producer
// outside motel emits an event, and how a teacher proves a channel.
type adminAPI struct{}

func (adminAPI) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "admin.api.notify",
		New: func() caddy.Module { return new(adminAPI) },
	}
}

func (adminAPI) Routes() []caddy.AdminRoute {
	return []caddy.AdminRoute{
		{Pattern: "/notify/events", Handler: caddy.AdminHandlerFunc(serveSend)},
		{Pattern: "/notify/test", Handler: caddy.AdminHandlerFunc(serveTest)},
	}
}

// SendRequest is `motel notify send`'s body.
type SendRequest struct {
	// Source is the producer's name; motel stamps it external:<source>.
	Source  string `json:"source"`
	Subject string `json:"subject"` // box, or app:<name>
	Name    string `json:"name"`
	// State is firing or resolved for a state alert, empty for a
	// one-shot.
	State    string            `json:"state,omitempty"`
	Severity string            `json:"severity"`
	Instance string            `json:"instance,omitempty"`
	Summary  string            `json:"summary"`
	Details  map[string]string `json:"details,omitempty"`
}

var (
	sourceRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	alertRe    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	instanceRe = regexp.MustCompile(`^[A-Za-z0-9._:/@-]{1,128}$`)
	detailRe   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
)

// reservedNames are what only motel posts: a producer that could post
// them could mute an app's watchdog alerts, fake the heartbeat, or
// speak as one of motel's own detectors.
var reservedNames = []string{
	"watchdog", alertChannelFailed, alertNotifyTest, alertDeployMute, alertDiskLow, "cert_failed",
	liveswap.EventWatchdogThrottled, liveswap.EventWatchdogRecovered, liveswap.EventRelaunchRefused,
	liveswap.EventRelaunchResolved, liveswap.EventDeployFailed, liveswap.EventDeployRefused,
	liveswap.EventDeploySucceeded, liveswap.EventRollback, liveswap.EventDeployStarted, liveswap.EventDeployEnded,
	"backup_stale", "backup_run_failed", "backup_unreadable", "drill_failed",
}

const maxDetails = 2 << 10

// Check is the validation `motel notify send` makes before it posts and
// the admin route makes again: the route is the gate, the command's
// check a courtesy that names the mistake first.
func (r SendRequest) Check() error {
	if !sourceRe.MatchString(r.Source) {
		return errors.New("source is the producer's name: lowercase letters, digits and dashes, at most 32; motel stamps it external:<name>")
	}
	if r.Subject != "box" {
		app, ok := strings.CutPrefix(r.Subject, "app:")
		if !ok || !backupdecl.ValidAppName(app) {
			return errors.New("subject is box or app:<name>")
		}
	}
	if !alertRe.MatchString(r.Name) {
		return errors.New("name is lowercase letters, digits and underscores, starting with a letter, at most 64")
	}
	if slices.ContainsFunc(reservedNames, func(n string) bool { return strings.EqualFold(n, r.Name) }) || strings.HasPrefix(r.Name, "watchdog_") {
		return fmt.Errorf("%s is one of motel's own events; name yours something else", r.Name)
	}
	switch r.State {
	case "", "firing", "resolved":
	default:
		return errors.New("state is firing or resolved for a state alert, or absent for a one-shot")
	}
	if severityRank(r.Severity) == 0 {
		return errors.New("severity is critical, warning or info")
	}
	if r.Instance != "" && !instanceRe.MatchString(r.Instance) {
		return errors.New("instance is at most 128 letters, digits and ._:/@-")
	}
	if strings.TrimSpace(r.Summary) == "" || strings.ContainsAny(r.Summary, "\r\n") {
		return errors.New("summary is one line, not empty")
	}
	if n := len([]rune(r.Summary)); n > maxSummary {
		return fmt.Errorf("summary is %d characters; at most %d", n, maxSummary)
	}
	size := 0
	for k, v := range r.Details {
		if !detailRe.MatchString(k) {
			return fmt.Errorf("detail %q: a name is lowercase letters, digits and underscores", k)
		}
		size += len(k) + len(v)
	}
	if size > maxDetails {
		return fmt.Errorf("details are %d bytes; at most %d", size, maxDetails)
	}
	return nil
}

func readJSON(r *http.Request, v any) error {
	if r.Method != http.MethodPost {
		return caddy.APIError{HTTPStatus: http.StatusMethodNotAllowed, Err: errors.New("POST only")}
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return caddy.APIError{HTTPStatus: http.StatusBadRequest, Err: fmt.Errorf("the body: %w", err)}
	}
	return nil
}

func unprocessable(err error) error {
	return caddy.APIError{HTTPStatus: http.StatusUnprocessableEntity, Err: err}
}

// serveSend is POST /notify/events: an outside producer's event, held
// to Check, then to the engine like motel's own (the filter, the
// bucket, the journal line, the queue).
func serveSend(w http.ResponseWriter, r *http.Request) error {
	var req SendRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if err := req.Check(); err != nil {
		return unprocessable(err)
	}
	e := theEngine
	e.mu.Lock()
	active := e.active
	e.mu.Unlock()
	if active == nil || active.cfg == nil {
		return caddy.APIError{HTTPStatus: http.StatusServiceUnavailable, Err: errors.New("this box's Caddyfile has no notify block")}
	}
	ev := Event{Name: req.Name, Severity: req.Severity, Instance: req.Instance, Source: "external:" + req.Source,
		Summary: req.Summary, Kind: kindEvent, Details: req.Details}
	if app, ok := strings.CutPrefix(req.Subject, "app:"); ok {
		if !slices.Contains(active.cfg.apps, app) {
			return unprocessable(fmt.Errorf("subject %s names no app this box runs", req.Subject))
		}
		ev.App = app
	}
	if req.State != "" {
		ev.Kind, ev.Firing = kindState, req.State == "firing"
	}
	e.report(ev)
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(map[string]string{"key": ev.key()})
}

// TestRequest is `motel notify test`'s body.
type TestRequest struct {
	Channel string `json:"channel"`
}

// TestResult is its answer: whether Alertmanager's counters show the
// notification sent, or failed, within the wait.
type TestResult struct {
	Channel   string `json:"channel"`
	Delivered bool   `json:"delivered"`
	Failed    string `json:"failed,omitempty"`
	Note      string `json:"note,omitempty"`
}

// testWait is how long the route watches the counters.
var testWait = 15 * time.Second

// serveTest is POST /notify/test: one NotifyTest alert to one channel,
// then Alertmanager's own counters read until they move. The only way a
// teacher proves a URL before an incident does (#418, open question 4).
func serveTest(w http.ResponseWriter, r *http.Request) error {
	var req TestRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	e := theEngine
	e.mu.Lock()
	active, post, read, now := e.active, e.post, e.metrics, e.now()
	e.mu.Unlock()
	if active == nil || active.cfg == nil {
		return caddy.APIError{HTTPStatus: http.StatusServiceUnavailable, Err: errors.New("this box's Caddyfile has no notify block")}
	}
	if _, ok := active.cfg.channels[req.Channel]; !ok {
		return unprocessable(fmt.Errorf("no channel named %q; the notify block's channels are %s", req.Channel, strings.Join(active.cfg.order, ", ")))
	}
	ctx, cancel := context.WithTimeout(r.Context(), testWait+amRequestTimeout)
	defer cancel()
	receiver := "ch-" + req.Channel
	var before counts
	if read != nil {
		if body, err := read(ctx); err == nil {
			before = countsFor(body, receiver)
		}
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	alert := amAlert{
		Labels: map[string]string{"alertname": alertNotifyTest, labelChannel: req.Channel, "severity": SeverityInfo,
			"subject": "box", "test": hex.EncodeToString(nonce)},
		Annotations: map[string]string{"summary": "a test from motel notify test: if you can read this, the channel " + req.Channel + " works"},
		StartsAt:    now, EndsAt: now.Add(oneShotLife),
	}
	e.logger.Info("notify: test", zap.String("channel", req.Channel))
	if err := post.postAlerts(ctx, []amAlert{alert}); err != nil {
		return caddy.APIError{HTTPStatus: http.StatusBadGateway, Err: fmt.Errorf("posting to Alertmanager: %w", err)}
	}
	res := TestResult{Channel: req.Channel}
	if read == nil {
		res.Note = "posted; Alertmanager's counters cannot be read here, so watch the channel"
		return writeJSON(w, res)
	}
	deadline := time.Now().Add(testWait)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
		body, err := read(ctx)
		if err != nil {
			continue
		}
		after := countsFor(body, receiver)
		if after.delivered(before) {
			res.Delivered = true
			return writeJSON(w, res)
		}
		if after.failed > before.failed {
			res.Failed = after.reason
			return writeJSON(w, res)
		}
	}
	res.Note = fmt.Sprintf("posted, but Alertmanager reported neither a delivery nor a failure within %s", testWait)
	return writeJSON(w, res)
}

func writeJSON(w http.ResponseWriter, v any) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(v)
}

// counts is one receiver's attempts, as Alertmanager counts them with
// receiver-name-in-metrics on: requests (each attempt to send), failed
// requests, and notifications given up as failed. A delivery is an
// attempt that did not fail; alertmanager_notifications_total is raised
// before the first attempt, so it says nothing of the outcome.
type counts struct {
	requests, requestsFailed, failed float64
	reason                           string
}

func (c counts) delivered(before counts) bool {
	return c.requests-c.requestsFailed > before.requests-before.requestsFailed
}

func countsFor(body []byte, receiver string) counts {
	var c counts
	for _, s := range parseFailed(body) {
		if s.receiver == receiver {
			c.failed += s.value
			if s.value > 0 {
				c.reason = s.integration + ": " + s.reason
			}
		}
	}
	for line := range strings.SplitSeq(string(body), "\n") {
		var dst *float64
		rest, ok := strings.CutPrefix(line, "alertmanager_notification_requests_total{")
		if ok {
			dst = &c.requests
		} else if rest, ok = strings.CutPrefix(line, "alertmanager_notification_requests_failed_total{"); ok {
			dst = &c.requestsFailed
		} else {
			continue
		}
		end := strings.LastIndex(rest, "} ")
		if end < 0 || parseLabels(rest[:end])["receiver_name"] != receiver {
			continue
		}
		var v float64
		if _, err := fmt.Sscan(rest[end+2:], &v); err == nil {
			*dst += v
		}
	}
	return c
}

var _ caddy.AdminRouter = adminAPI{}
