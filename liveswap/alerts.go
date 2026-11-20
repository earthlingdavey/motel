package liveswap

import (
	"context"

	"github.com/smallhoursorg/motel/liveswap/wire"
)

// AlertsSource is what Alertmanager holds, for the people's reads of
// alerts (GET /_motel/apps/<app>/alerts, GET /_motel/alerts). notify, which liveswap cannot
// import (it imports liveswap), installs it on a config whose monitor
// block runs notify (SetAlertsSource), as it installs MetricsSource. A
// source holds the box's whole answer: which of it a read may show
// (one app's alerts, by their app label; every alert and silence, for a
// viewer of the box) is the route's to choose,
// through the handle Decide's answer made, never the source's.
type AlertsSource interface {
	// Alerts is what Alertmanager holds now, or ok false when it did not
	// answer within the source's own short budget. It is called with the
	// request's context; the caller only reads what it returns.
	Alerts(ctx context.Context) (held HeldAlerts, ok bool)
}

// HeldAlerts is one answer of Alertmanager's, as a source gives it.
type HeldAlerts struct {
	// Alerts is every alert it holds but the heartbeat, the test alert
	// and the deploy mute, which say nothing of any app or of the box's
	// health, in no order.
	Alerts []AppAlert
	// Silences is every silence it holds that has not expired, in no
	// order: the box-wide read's alone, never an app's.
	Silences []wire.Silence
}

// AppAlert is one alert a source holds, and the app its app label
// names: "" for none, a box event (which names an app, if any, in its
// instance, never in the label); and its instance label, which names
// paths and the box's other sites: the box-wide read's alone.
type AppAlert struct {
	App      string
	Instance string
	wire.Alert
}

// SetAlertsSource installs notify's source on this config. Called from
// the monitor app's Provision, after liveswap's own, so a config whose
// monitor block runs no notify, or that has none, answers
// "monitor":"off".
func (a *App) SetAlertsSource(s AlertsSource) { a.alertsSource = s }
