package wire

import "time"

// Alerts is GET /_motel/apps/<app>/alerts's body: the alerts
// Alertmanager holds whose app label is that app, and whether the box
// could ask. It names no other app, no silence and none of the box's
// own alerts (a disk, a certificate, the backups), even one that names
// the app in its instance.
type Alerts struct {
	// Monitor is MonitorOn (Alertmanager answered: Alerts is what it
	// holds of the app, maybe none), MonitorOff (the config has no
	// monitor block that raises anything: nothing is raised, and Alerts
	// is empty) or MonitorUnanswered (Alertmanager did not answer within
	// the box's budget: Alerts is empty, and says nothing). A client
	// reads a word it does not know as not knowing whether anything
	// fires.
	Monitor string  `json:"monitor" text:"moteld"`
	Alerts  []Alert `json:"alerts"`
}

// The words of Alerts.Monitor.
const (
	MonitorOn         = "on"
	MonitorOff        = "off"
	MonitorUnanswered = "unanswered"
)

// Alert is one alert Alertmanager holds, cut to what a screen reads.
type Alert struct {
	// Name is its alertname: one of motel's events (notify/DESIGN-notify.md,
	// "Events"), an operator's rule, or an external producer's.
	Name string `json:"name" text:"passes"`
	// Severity is critical, warning or info.
	Severity string `json:"severity" text:"passes"`
	// Summary is motel's sentence about it, which the box built to
	// carry no secret and passed through the app's filter when it told
	// it; or an operator's rule's, or another program's, as written.
	Summary string `json:"summary" text:"passes"`
	// StartsAt is when it began firing, as Alertmanager holds it.
	StartsAt time.Time `json:"starts_at"`
	// State is Alertmanager's: active (told to its channels),
	// suppressed (silenced, or held by the deploy mute) or unprocessed
	// (just received, not yet judged). A client reads a word it does not
	// know as not knowing.
	State string `json:"state" text:"passes"`
	// SilencedUntil is when the last of the silences that suppress it
	// ends; absent when none does.
	SilencedUntil *time.Time `json:"silenced_until,omitempty"`
	// Inhibited is that the deploy mute holds it: a deploy of the app is
	// under way, and its watchdog's alerts wait for the deploy's end.
	Inhibited bool `json:"inhibited,omitempty"`
}
