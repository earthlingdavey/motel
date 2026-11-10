package notify

import (
	"bytes"
	"errors"
	"fmt"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// Alertmanager's delivery counters, which motel reads from its /metrics
// over the socket: channel_failed from the first, `motel notify test`
// from all three. With --enable-feature=receiver-name-in-metrics (the
// drop-in sets it) each carries receiver_name beside integration.
const (
	// failedMetric counts notifications given up as failed, by reason.
	failedMetric = "alertmanager_notifications_failed_total"
	// requestsMetric counts attempts to send, and requestsFailedMetric
	// the attempts that failed: an attempt that did not fail is a
	// delivery. alertmanager_notifications_total is raised before the
	// first attempt, so it says nothing of the outcome.
	requestsMetric       = "alertmanager_notification_requests_total"
	requestsFailedMetric = "alertmanager_notification_requests_failed_total"
)

// failedSample is one series of failedMetric.
type failedSample struct {
	integration, receiver, reason string
	value                         float64
}

// amMetrics is what one read of /metrics says of deliveries.
type amMetrics struct {
	failed []failedSample
	// requests and requestsFailed are by receiver_name, summed over
	// integrations; "" is the series without one.
	requests, requestsFailed map[string]float64
}

// errNoFailedCounter is a body without failedMetric. Alertmanager makes
// its series at zero for every receiver and integration the config uses
// when it loads the config, so while any channel is configured a body
// without it is not a reading: taken as one, every counter after it
// would look like a rise.
var errNoFailedCounter = errors.New("alertmanager's metrics have no " + failedMetric)

// parseAMMetrics reads a /metrics body with Prometheus's own text
// parser (prometheus/common/expfmt, linked already through Caddy's
// metrics). It is strict: a body cut short, a malformed line, or one
// without the failed counter is an error, and the caller takes no
// reading from it rather than half of one.
func parseAMMetrics(body []byte) (amMetrics, error) {
	p := expfmt.NewTextParser(model.UTF8Validation)
	fams, err := p.TextToMetricFamilies(bytes.NewReader(body))
	if err != nil {
		return amMetrics{}, fmt.Errorf("reading alertmanager's metrics: %w", err)
	}
	fam, ok := fams[failedMetric]
	if !ok {
		return amMetrics{}, errNoFailedCounter
	}
	m := amMetrics{requests: map[string]float64{}, requestsFailed: map[string]float64{}}
	for _, s := range fam.GetMetric() {
		l := labelMap(s)
		m.failed = append(m.failed, failedSample{integration: l["integration"], receiver: l["receiver_name"], reason: l["reason"], value: metricValue(s)})
	}
	for name, dst := range map[string]map[string]float64{requestsMetric: m.requests, requestsFailedMetric: m.requestsFailed} {
		for _, s := range fams[name].GetMetric() {
			dst[labelMap(s)["receiver_name"]] += metricValue(s)
		}
	}
	return m, nil
}

func labelMap(m *dto.Metric) map[string]string {
	out := make(map[string]string, len(m.GetLabel()))
	for _, l := range m.GetLabel() {
		out[l.GetName()] = l.GetValue()
	}
	return out
}

// metricValue is a counter's value, or an untyped series' (a body with
// no TYPE line for the family).
func metricValue(m *dto.Metric) float64 {
	if c := m.GetCounter(); c != nil {
		return c.GetValue()
	}
	return m.GetUntyped().GetValue()
}
