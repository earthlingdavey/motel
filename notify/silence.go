package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"

	"github.com/smallhoursorg/motel/liveswap"
)

// Silences: `motel monitor silence add` and `expire` post to
// POST /notify/silences and POST /notify/silences/expire on the admin
// socket, and the route, not the command, writes to Alertmanager's
// silence API and the journal line (DESIGN-notify.md, "Silences").

// SilenceRequest is `motel monitor silence add`'s body.
type SilenceRequest struct {
	// Matchers are amtool's grammar, one per argument: label=value,
	// label!=value, label=~regex, label!~regex.
	Matchers []string `json:"matchers"`
	Comment  string   `json:"comment"`
	// Duration is Go's (2h, 90m), from now.
	Duration string `json:"duration"`
	// By is the invoking user's name, as the command finds it; the
	// silence's createdBy is `motel monitor: <by>`.
	By string `json:"by"`
}

// ExpireRequest is `motel monitor silence expire`'s body.
type ExpireRequest struct {
	ID string `json:"id"`
	By string `json:"by"`
}

// SilenceResult is both commands' answer: the silence's id and, for an
// add, when it ends. Its JSON is what `--json` prints.
type SilenceResult struct {
	ID     string `json:"id"`
	EndsAt string `json:"ends_at,omitempty"`
}

const (
	defaultSilence = 2 * time.Hour
	minSilence     = time.Minute
	maxSilence     = 24 * time.Hour
	maxMatchers    = 16
	maxMatcherLen  = 200
)

var (
	labelNameRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*`)
	byRe        = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.@-]{0,63}$`)
	silenceIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// amMatcher is one matcher in Alertmanager's v2 API.
type amMatcher struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	IsRegex bool   `json:"isRegex"`
	IsEqual bool   `json:"isEqual"`
	re      *regexp.Regexp
}

// matches is Alertmanager's own test of one label value: an absent
// label is the empty string, and a regex matches the whole value.
func (m amMatcher) matches(labels map[string]string) bool {
	v := labels[m.Name]
	var hit bool
	if m.IsRegex {
		hit = m.re.MatchString(v)
	} else {
		hit = v == m.Value
	}
	return hit == m.IsEqual
}

// parseMatcher reads one matcher strictly: a label name, one of the
// four operators, and the rest as the value. No quotes: the shell's
// quoting is the only quoting, so `alertname="x"` is refused rather
// than read as a value with quotes in it.
func parseMatcher(s string) (amMatcher, error) {
	if len(s) > maxMatcherLen {
		return amMatcher{}, fmt.Errorf("matcher %.40q…: at most %d bytes", s, maxMatcherLen)
	}
	name := labelNameRe.FindString(s)
	if name == "" {
		return amMatcher{}, fmt.Errorf("matcher %q: begins with a label name ([a-zA-Z_][a-zA-Z0-9_]*)", s)
	}
	rest := s[len(name):]
	m := amMatcher{Name: name}
	switch {
	case strings.HasPrefix(rest, "=~"):
		m.IsRegex, m.IsEqual, m.Value = true, true, rest[2:]
	case strings.HasPrefix(rest, "!~"):
		m.IsRegex, m.Value = true, rest[2:]
	case strings.HasPrefix(rest, "!="):
		m.Value = rest[2:]
	case strings.HasPrefix(rest, "="):
		m.IsEqual, m.Value = true, rest[1:]
	default:
		return amMatcher{}, fmt.Errorf("matcher %q: label=value, label!=value, label=~regex or label!~regex", s)
	}
	if strings.HasPrefix(m.Value, `"`) || strings.HasPrefix(m.Value, "'") {
		return amMatcher{}, fmt.Errorf("matcher %q: no quotes around the value; the shell's quoting is enough", s)
	}
	if strings.TrimSpace(m.Value) != m.Value || strings.ContainsFunc(m.Value, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return amMatcher{}, fmt.Errorf("matcher %q: the value has a space at an end or a control character", s)
	}
	if m.IsRegex {
		re, err := regexp.Compile("^(?:" + m.Value + ")$")
		if err != nil {
			return amMatcher{}, fmt.Errorf("matcher %q: %w", s, err)
		}
		m.re = re
	}
	return m, nil
}

// watchdogLabels are the heartbeat's: the engine posts them (tick), and
// a silence that matches them all is refused.
func watchdogLabels() map[string]string {
	return map[string]string{"alertname": alertWatchdog, "severity": "none"}
}

// parse is the validation the command makes before it posts and the
// route makes again: the matchers, the duration, the comment and the
// caller's name.
func (r SilenceRequest) parse() ([]amMatcher, time.Duration, error) {
	if len(r.Matchers) == 0 {
		return nil, 0, errors.New("name at least one matcher, such as alertname=disk_low")
	}
	if len(r.Matchers) > maxMatchers {
		return nil, 0, fmt.Errorf("%d matchers; at most %d", len(r.Matchers), maxMatchers)
	}
	ms := make([]amMatcher, 0, len(r.Matchers))
	equal := false
	for _, s := range r.Matchers {
		m, err := parseMatcher(s)
		if err != nil {
			return nil, 0, err
		}
		equal = equal || m.IsEqual && !m.IsRegex && m.Value != ""
		ms = append(ms, m)
	}
	// One label=value at least: a set of regexes and negations alone can
	// match everything (alertname=~.*), which no check here can rule out.
	if !equal {
		return nil, 0, errors.New("at least one matcher must be label=value with a value, such as alertname=disk_low: regexes and negations alone could silence everything")
	}
	allWatchdog, heartbeat := true, watchdogLabels()
	for _, m := range ms {
		allWatchdog = allWatchdog && m.matches(heartbeat)
	}
	if allWatchdog {
		return nil, 0, errors.New("these matchers silence Watchdog, the heartbeat: its pings would stop and the check off the box would report the box down; pause that check instead")
	}
	d := defaultSilence
	if r.Duration != "" {
		var err error
		if d, err = time.ParseDuration(r.Duration); err != nil {
			return nil, 0, fmt.Errorf("duration %q: a Go duration such as 30m or 2h", r.Duration)
		}
	}
	if d < minSilence || d > maxSilence {
		return nil, 0, fmt.Errorf("duration %s: from 1m to 24h", d)
	}
	if err := checkComment(r.Comment); err != nil {
		return nil, 0, err
	}
	if !byRe.MatchString(r.By) {
		return nil, 0, errors.New("by is the invoking user's name: letters, digits and _.@-, at most 64")
	}
	return ms, d, nil
}

// Check is parse's verdict alone.
func (r SilenceRequest) Check() error {
	_, _, err := r.parse()
	return err
}

func checkComment(c string) error {
	if strings.TrimSpace(c) == "" || strings.ContainsAny(c, "\r\n") {
		return errors.New("comment is one line, not empty: say why, for whoever finds the silence")
	}
	if n := len([]rune(c)); n > maxSummary {
		return fmt.Errorf("comment is %d characters; at most %d", n, maxSummary)
	}
	return nil
}

// Check is the expire request's validation.
func (r ExpireRequest) Check() error {
	if !silenceIDRe.MatchString(r.ID) {
		return fmt.Errorf("%q is not a silence id (Alertmanager's are UUIDs, as silence add prints them)", r.ID)
	}
	if !byRe.MatchString(r.By) {
		return errors.New("by is the invoking user's name: letters, digits and _.@-, at most 64")
	}
	return nil
}

func createdBy(by string) string { return "motel monitor: " + by }

// amSilence is a silence in Alertmanager's v2 API
// (POST /api/v2/silences).
type amSilence struct {
	Matchers  []amMatcher `json:"matchers"`
	StartsAt  time.Time   `json:"startsAt"`
	EndsAt    time.Time   `json:"endsAt"`
	CreatedBy string      `json:"createdBy"`
	Comment   string      `json:"comment"`
}

func (c *amClient) postSilence(ctx context.Context, s amSilence) (string, error) {
	body, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	out, err := c.do(ctx, http.MethodPost, "/api/v2/silences", body)
	if err != nil {
		return "", err
	}
	var res struct {
		SilenceID string `json:"silenceID"`
	}
	if json.Unmarshal(out, &res) != nil || !silenceIDRe.MatchString(res.SilenceID) {
		return "", fmt.Errorf("alertmanager answered %q to /api/v2/silences", strings.TrimSpace(string(out)))
	}
	return res.SilenceID, nil
}

// silenceState is the state Alertmanager gives the silence id
// (active, pending or expired).
func (c *amClient) silenceState(ctx context.Context, id string) (string, error) {
	out, err := c.do(ctx, http.MethodGet, "/api/v2/silence/"+id, nil)
	if err != nil {
		return "", err
	}
	var res struct {
		Status struct {
			State string `json:"state"`
		} `json:"status"`
	}
	if json.Unmarshal(out, &res) != nil || res.Status.State == "" {
		return "", fmt.Errorf("alertmanager answered %q to /api/v2/silence/%s", strings.TrimSpace(string(out)), id)
	}
	return res.Status.State, nil
}

func (c *amClient) expireSilence(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodDelete, "/api/v2/silence/"+id, nil)
	return err
}

// silenceTarget is the Alertmanager a silence route writes to, the
// engine's logger and clock, and its filter's known values: the config
// notify's Start configured.
func silenceTarget() (*amClient, *zap.Logger, time.Time, []string, error) {
	e := theEngine
	e.mu.Lock()
	active, logger, now, known := e.active, e.logger, e.now(), e.known
	e.mu.Unlock()
	if active == nil || active.cfg == nil {
		return nil, nil, now, nil, caddy.APIError{HTTPStatus: http.StatusServiceUnavailable, Err: errors.New("this box's Caddyfile has no monitor block with channels")}
	}
	if !active.setUp() {
		return nil, nil, now, nil, caddy.APIError{HTTPStatus: http.StatusServiceUnavailable, Err: errNotSetUp}
	}
	return newAMClient(active.socket()), logger, now, known, nil
}

// masked reports s changed by the filter, whose known values are the
// channel URLs: a comment wholly, so a secret-shaped token is refused
// too; a matcher with itself safe, so only a channel URL is, since a
// matcher names a label motel set, a release's git sha among them.
func masked(known []string, s string, asSafe bool) bool {
	var safe []string
	if asSafe {
		safe = []string{s}
	}
	out, _ := liveswap.NewRedactor(known, safe).Redact(s)
	return out != s
}

func badGateway(err error) error {
	return caddy.APIError{HTTPStatus: http.StatusBadGateway, Err: err}
}

// serveSilence is POST /notify/silences: one silence, held to parse,
// written to Alertmanager, then the `monitor: silence added` line.
func serveSilence(w http.ResponseWriter, r *http.Request) error {
	var req SilenceRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	ms, d, err := req.parse()
	if err != nil {
		return unprocessable(err)
	}
	am, logger, now, known, err := silenceTarget()
	if err != nil {
		return err
	}
	// The comment and the matchers are stored by Alertmanager and
	// written to the journal as they are: one that holds what the
	// filter would mask is refused, as send's instance is.
	if masked(known, req.Comment, false) {
		return unprocessable(errors.New("the comment holds a value the filter masks (a channel URL, or a secret-shaped token): name the thing, not its credential"))
	}
	if slices.ContainsFunc(req.Matchers, func(m string) bool { return masked(known, m, true) }) {
		return unprocessable(errors.New("a matcher holds a channel URL: name the channel, not its URL"))
	}
	now = now.UTC().Truncate(time.Second)
	s := amSilence{Matchers: ms, StartsAt: now, EndsAt: now.Add(d), CreatedBy: createdBy(req.By), Comment: req.Comment}
	id, err := am.postSilence(r.Context(), s)
	if err != nil {
		return badGateway(fmt.Errorf("silencing in Alertmanager: %w", err))
	}
	res := SilenceResult{ID: id, EndsAt: s.EndsAt.Format(time.RFC3339)}
	logger.Info("monitor: silence added", zap.String("id", id), zap.Strings("matchers", req.Matchers),
		zap.String("ends_at", res.EndsAt), zap.String("created_by", s.CreatedBy), zap.String("comment", req.Comment))
	return writeJSON(w, res)
}

// serveExpire is POST /notify/silences/expire: one silence ended now,
// then the `monitor: silence expired` line.
func serveExpire(w http.ResponseWriter, r *http.Request) error {
	var req ExpireRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if err := req.Check(); err != nil {
		return unprocessable(err)
	}
	am, logger, _, _, err := silenceTarget()
	if err != nil {
		return err
	}
	// Alertmanager 0.28.1 answers 200 to the expiry of a silence already
	// expired (measured in the notify e2e box): asked first, so that a
	// second expire says so rather than journalling an end that was not.
	state, err := am.silenceState(r.Context(), req.ID)
	if err == nil && state == "expired" {
		return unprocessable(fmt.Errorf("silence %s has already expired", req.ID))
	}
	if err == nil {
		err = am.expireSilence(r.Context(), req.ID)
	}
	if err != nil {
		var se *amStatusError
		if errors.As(err, &se) && se.code == http.StatusNotFound {
			return unprocessable(fmt.Errorf("no silence %s in Alertmanager", req.ID))
		}
		return badGateway(fmt.Errorf("expiring in Alertmanager: %w", err))
	}
	logger.Info("monitor: silence expired", zap.String("id", req.ID), zap.String("expired_by", createdBy(req.By)))
	return writeJSON(w, SilenceResult{ID: req.ID})
}
