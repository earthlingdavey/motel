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
	"unicode"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"

	"github.com/smallhoursorg/motel/liveswap"
)

// Silences: `motel monitor silence add` and `expire` post to
// POST /notify/silences and POST /notify/silences/expire on the admin
// socket, and the route, not the command, writes to Alertmanager's
// silence API and the journal line (DESIGN-notify.md, "Silences").
// There are no free-form matchers: motel builds equality matchers from
// names it checks against closed sets.

// SilenceRequest is `motel monitor silence add`'s body.
type SilenceRequest struct {
	// Alert is one of the alert names motel raises on this box
	// (silenceableAlerts); App one of the apps it runs; Release a
	// release name in liveswap's grammar. Alert or App, or both.
	Alert   string `json:"alert,omitempty"`
	App     string `json:"app,omitempty"`
	Release string `json:"release,omitempty"`
	Comment string `json:"comment"`
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
)

var (
	byRe        = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.@-]{0,63}$`)
	silenceIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// amMatcher is one matcher in Alertmanager's v2 API. motel's are
// equalities only.
type amMatcher struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	IsRegex bool   `json:"isRegex"`
	IsEqual bool   `json:"isEqual"`
}

// parse is the shape the command checks before it posts and the route
// checks again: which flags, the release's grammar, the duration, the
// comment and the caller's name. The closed sets (alert, app) are the
// route's, which knows the loaded config. No refusal echoes a value: the
// admin API journals a route's refusal (Caddy's `request error` line).
func (r SilenceRequest) parse() (time.Duration, error) {
	if r.Alert == "" && r.App == "" {
		return 0, errors.New("name --alert, --app or both: a silence of a release alone, or of nothing, is too wide")
	}
	if r.Release != "" && !liveswap.ValidVersion(r.Release) {
		return 0, errors.New("--release is a release's name: letters, digits and ._-, at most 64, not starting with a dot")
	}
	d := defaultSilence
	if r.Duration != "" {
		var err error
		if d, err = time.ParseDuration(r.Duration); err != nil {
			return 0, errors.New("--duration is a Go duration such as 30m or 2h")
		}
	}
	if d < minSilence || d > maxSilence {
		return 0, errors.New("--duration is from 1m to 24h")
	}
	if err := checkComment(r.Comment); err != nil {
		return 0, err
	}
	if !byRe.MatchString(r.By) {
		return 0, errors.New("by is the invoking user's name: letters, digits and _.@-, at most 64")
	}
	return d, nil
}

// Check is parse's verdict alone.
func (r SilenceRequest) Check() error {
	_, err := r.parse()
	return err
}

// matchers are the silence's: an equality per name given, on the
// labels motel's own alerts carry (labelsFor, oneShotAlert, stateLabels).
func (r SilenceRequest) matchers() []amMatcher {
	var ms []amMatcher
	for _, m := range [][2]string{{"alertname", r.Alert}, {"app", r.App}, {"release", r.Release}} {
		if m[1] != "" {
			ms = append(ms, amMatcher{Name: m[0], Value: m[1], IsEqual: true})
		}
	}
	// An app's silence leaves its deploy mute alone: the mute reaches
	// no channel, and what it inhibits must stay inhibited.
	if r.Alert == "" {
		ms = append(ms, amMatcher{Name: "alertname", Value: alertDeployMute})
	}
	return ms
}

// releasesOf are the app's releases on disk, newest first: a var so
// that a test needs no liveswap.
var releasesOf = func(a *App, app string) []string {
	if a.ls == nil {
		return nil
	}
	return a.ls.Releases(app)
}

// checkSets holds the request's names to the loaded config's: the
// alerts motel raises here, the apps it runs, an app's releases on
// disk; and the pair to what can match: --app only with an alert about
// an app, --release only with --app and an alert that carries the
// release. A refusal lists motel's own names, which are safe to print,
// and never a value given that is not one of them.
func (r SilenceRequest) checkSets(a *App) error {
	if r.Alert != "" {
		names := silenceableAlerts(a)
		if !slices.Contains(names, r.Alert) {
			return fmt.Errorf("--alert names none of the alerts motel raises on this box; they are %s", strings.Join(names, ", "))
		}
	}
	if r.App != "" && !slices.Contains(a.cfg.apps, r.App) {
		apps := strings.Join(a.cfg.apps, ", ")
		if apps == "" {
			apps = "none"
		}
		return fmt.Errorf("--app names no app this box runs; they are %s", apps)
	}
	if r.Alert != "" && r.App != "" {
		app, pinned, _ := alertScope(a, r.Alert)
		if !app {
			return fmt.Errorf("%s is about the box, not an app, so a silence with --app would match nothing: leave --app out", r.Alert)
		}
		if pinned != "" && pinned != r.App {
			return fmt.Errorf("%s is a rule about %s alone, so a silence with another --app would match nothing", r.Alert, pinned)
		}
		if r.Alert == eventPostDeployRegressed {
			if _, ok := postDeployRuleFor(a.Metrics, r.App); !ok {
				return fmt.Errorf("no post_deploy rule judges %s's deploys, so %s never fires for it", r.App, eventPostDeployRegressed)
			}
		}
	}
	if r.Release != "" {
		if _, _, release := alertScope(a, r.Alert); r.App == "" || !release {
			return fmt.Errorf("--release narrows one app's alerts whose release is on disk, so it needs --app and --alert one of %s (a failed or refused deploy's release never stays on disk: silence its --alert and --app)", strings.Join(onDiskReleases, ", "))
		}
		rels := releasesOf(a, r.App)
		if !slices.Contains(rels, r.Release) {
			list := strings.Join(rels, ", ")
			if list == "" {
				list = "none on disk"
			}
			return fmt.Errorf("--release names no release of %s on this box; they are %s", r.App, list)
		}
	}
	return nil
}

func checkComment(c string) error {
	if strings.TrimSpace(c) == "" || strings.ContainsFunc(c, func(r rune) bool { return unicode.IsControl(r) || r == '\u2028' || r == '\u2029' }) {
		return errors.New("--comment is one line of text, not empty and with no control character: say why, for whoever finds the silence")
	}
	if n := len([]rune(c)); n > maxSummary {
		return fmt.Errorf("--comment is %d characters; at most %d", n, maxSummary)
	}
	return nil
}

// Check is the expire request's validation.
func (r ExpireRequest) Check() error {
	if !silenceIDRe.MatchString(r.ID) {
		return errors.New("that is not a silence id (Alertmanager's are UUIDs, as silence add prints them)")
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

// silenceTarget is the config notify's Start configured, the engine's
// logger and clock, and the Alertmanager a silence route writes to.
func silenceTarget() (*App, *amClient, *zap.Logger, time.Time, error) {
	e := theEngine
	e.mu.Lock()
	active, logger, now := e.active, e.logger, e.now()
	e.mu.Unlock()
	if active == nil || active.cfg == nil {
		return nil, nil, nil, now, caddy.APIError{HTTPStatus: http.StatusServiceUnavailable, Err: errors.New("this box's Caddyfile has no monitor block with channels")}
	}
	if !active.setUp() {
		return nil, nil, nil, now, caddy.APIError{HTTPStatus: http.StatusServiceUnavailable, Err: errNotSetUp}
	}
	return active, newAMClient(theSocket), logger, now, nil
}

func badGateway(err error) error {
	return caddy.APIError{HTTPStatus: http.StatusBadGateway, Err: err}
}

// serveSilence is POST /notify/silences: one silence, held to parse and
// to the loaded config's names, written to Alertmanager, then the
// `monitor: silence added` line.
func serveSilence(w http.ResponseWriter, r *http.Request) error {
	var req SilenceRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	d, err := req.parse()
	if err != nil {
		return unprocessable(err)
	}
	active, am, logger, now, err := silenceTarget()
	if err != nil {
		return err
	}
	if err := req.checkSets(active); err != nil {
		return unprocessable(err)
	}
	// The comment is the one free text: masked as a summary is, the
	// release safe as the engine takes it, rather than refused
	// (DESIGN-notify.md, "Silences").
	// The known values of the config checked above and the engine's
	// now: a reload between the two must neither unmask what the checked
	// config knew nor miss what the new one added.
	theEngine.mu.Lock()
	known := append(slices.Clone(active.known), theEngine.known...)
	theEngine.mu.Unlock()
	comment := cleanText(known, req.Comment, req.Release)
	now = now.UTC()
	s := amSilence{Matchers: req.matchers(), StartsAt: now, EndsAt: now.Add(d), CreatedBy: createdBy(req.By), Comment: comment}
	id, err := am.postSilence(r.Context(), s)
	if err != nil {
		return badGateway(fmt.Errorf("silencing in Alertmanager: %w", err))
	}
	res := SilenceResult{ID: id, EndsAt: s.EndsAt.Format(time.RFC3339Nano)}
	logger.Info("monitor: silence added", zap.String("id", id), zap.String("alert", req.Alert), zap.String("app", req.App),
		zap.String("release", req.Release), zap.String("ends_at", res.EndsAt), zap.String("created_by", s.CreatedBy), zap.String("comment", comment))
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
	_, am, logger, _, err := silenceTarget()
	if err != nil {
		return err
	}
	// Alertmanager 0.28.1 answers 200 to the expiry of a silence already
	// expired (measured in the notify e2e box): asked first, so that a
	// second expire says so rather than journalling an end that was not.
	notFound := func(err error) bool {
		var se *amStatusError
		return errors.As(err, &se) && se.code == http.StatusNotFound
	}
	state, err := am.silenceState(r.Context(), req.ID)
	switch {
	case notFound(err):
		return unprocessable(fmt.Errorf("no silence %s in Alertmanager", req.ID))
	case err != nil:
		return badGateway(fmt.Errorf("reading the silence from Alertmanager: %w", err))
	case state == "expired":
		return unprocessable(fmt.Errorf("silence %s has already expired", req.ID))
	}
	if err := am.expireSilence(r.Context(), req.ID); err != nil {
		if notFound(err) {
			return unprocessable(fmt.Errorf("no silence %s in Alertmanager", req.ID))
		}
		return badGateway(fmt.Errorf("expiring in Alertmanager: %w", err))
	}
	logger.Info("monitor: silence expired", zap.String("id", req.ID), zap.String("expired_by", createdBy(req.By)))
	return writeJSON(w, SilenceResult{ID: req.ID})
}
