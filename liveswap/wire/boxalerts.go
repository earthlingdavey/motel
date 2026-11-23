package wire

import "time"

// BoxAlerts is GET /_motel/alerts's body, for a viewer of the box:
// every alert Alertmanager holds, each with the app and the instance
// it names, and every silence that has not ended. The heartbeat, the
// test alert and the deploy mute are left out, as from an app's alerts
// (Alerts); an alert the mute holds is there, Inhibited.
type BoxAlerts struct {
	// Monitor is as Alerts.Monitor: MonitorOn, MonitorOff (Alerts and
	// Silences empty) or MonitorUnanswered (both empty, and saying
	// nothing).
	Monitor string     `json:"monitor" text:"moteld"`
	Alerts  []BoxAlert `json:"alerts"`
	// Silences are the active and pending ones, never an expired one:
	// the active newest first, then the pending soonest first.
	Silences []Silence `json:"silences"`
	// MoreAlerts and MoreSilences are that the box held more than it
	// sends (BoxAlertsMax, SilencesMax): the newest alerts are sent, and
	// the active silences and the pending ones soonest to start.
	MoreAlerts   bool `json:"more_alerts,omitempty"`
	MoreSilences bool `json:"more_silences,omitempty"`
}

// The most of each list one answer carries. motel bounds what it posts
// (a key holds at most four one-shots at once, a silence lasts a day at
// most); the caps guard against an Alertmanager that holds what motel
// did not post.
const (
	BoxAlertsMax = 500
	SilencesMax  = 200
)

// BoxAlert is one alert, as an app's alerts carry it, and what of it
// is the box's to say: the app its app label names ("" for a box
// event, which names an app, if any, in its instance) and its instance
// (a path, a certificate's name, an app, or a producer's own).
type BoxAlert struct {
	Alert
	App      string `json:"app,omitempty" text:"passes"`
	Instance string `json:"instance,omitempty" text:"passes"`
}

// Silence is one silence Alertmanager holds that has not ended, cut to
// what a screen reads.
type Silence struct {
	ID string `json:"id" text:"passes"`
	// State is SilenceActive or SilencePending (it starts later). A
	// client reads a word it does not know as not knowing.
	State string `json:"state" text:"passes"`
	// Matchers are as Alertmanager holds them: motel's own are equality
	// matchers on alertname, app and release, and an app's alone has
	// alertname!=deploy_in_progress beside it; root can post others.
	Matchers  []Matcher `json:"matchers"`
	StartsAt  time.Time `json:"starts_at"`
	EndsAt    time.Time `json:"ends_at"`
	CreatedBy string    `json:"created_by" text:"passes"`
	Comment   string    `json:"comment" text:"passes"`
}

// The words of Silence.State.
const (
	SilenceActive  = "active"
	SilencePending = "pending"
)

// Matcher is one matcher of a silence: a label's name, how it is
// compared (Op: MatchEqual, MatchNotEqual, MatchRegexp, MatchNotRegexp)
// and the value or the regular expression.
type Matcher struct {
	Name  string `json:"name" text:"passes"`
	Op    string `json:"op" text:"passes"`
	Value string `json:"value" text:"passes"`
}

// The words of Matcher.Op, as amtool writes them.
const (
	MatchEqual     = "="
	MatchNotEqual  = "!="
	MatchRegexp    = "=~"
	MatchNotRegexp = "!~"
)
