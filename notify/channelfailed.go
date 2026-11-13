package notify

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
)

// channel_failed without the metrics package (DESIGN-notify.md,
// "Failed delivery"): once a minute motel reads Alertmanager's own
// alertmanager_notifications_failed_total over the socket, and when it
// has risen tells the operator's other channels, one ChannelFailed alert
// per channel, each routed to that channel alone (notify_channel=).
// With --enable-feature=receiver-name-in-metrics (the drop-in sets it)
// the counter names the receiver, and the failing channel is narrowed
// to that receiver's channels of the failing integration; without it,
// to every channel of the integration.

// channelFailedLife is the alert's endsAt: re-posted while the counter
// keeps rising, gone a quarter-hour after it stops.
const channelFailedLife = 15 * time.Minute / clockDiv

// failWatch is the engine's memory of the counter, and the config it
// reads the counter against.
type failWatch struct {
	prev     map[string]float64 // integration/receiver/reason → value
	operator []string
	kinds    map[string]string   // channel → integration
	channels map[string][]string // receiver → channels
}

func sampleKey(s failedSample) string { return s.integration + "/" + s.receiver + "/" + s.reason }

// rises compares a read of the counter with the last and returns the
// series that rose. The first read is a baseline; a counter lower than
// the last is an Alertmanager restart, and counts from zero.
func (w *failWatch) rises(samples []failedSample) []failedSample {
	first := w.prev == nil
	next := make(map[string]float64, len(samples))
	var out []failedSample
	for _, s := range samples {
		k := sampleKey(s)
		next[k] = s.value
		if first {
			continue
		}
		prev, seen := w.prev[k]
		if (!seen || s.value < prev) && s.value > 0 || seen && s.value > prev {
			out = append(out, s)
		}
	}
	w.prev = next
	return out
}

// suspects is which channels a rise can be: those of the failing
// integration in the named receiver, or in every receiver when the
// counter names none.
func (w *failWatch) suspects(s failedSample) []string {
	var out []string
	add := func(ch string) {
		if w.kinds[ch] == s.integration && !slices.Contains(out, ch) {
			out = append(out, ch)
		}
	}
	if chans, ok := w.channels[s.receiver]; ok && s.receiver != "" {
		for _, ch := range chans {
			add(ch)
		}
	} else {
		for ch := range w.kinds {
			add(ch)
		}
	}
	sort.Strings(out)
	return out
}

// scrapeFailed is one read of the counter; it posts ChannelFailed for
// each rise to every operator channel that is not a suspect.
func (e *engine) scrapeFailed(ctx context.Context, read func(context.Context) ([]byte, error)) {
	body, err := read(ctx)
	var m amMetrics
	if err == nil {
		m, err = parseAMMetrics(body)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err != nil {
		// No reading this minute: the baseline stays the last good one,
		// so the next good read is compared with it, not with nothing.
		e.logger.Debug("monitor: reading Alertmanager's metrics", zap.Error(err))
		return
	}
	if e.active == nil {
		return
	}
	now := e.now()
	for _, s := range e.fail.rises(m.failed) {
		sus := e.fail.suspects(s)
		key := "notify/box/channel_failed/" + strings.Join(sus, ",")
		summary := fmt.Sprintf("Alertmanager could not deliver through %s (%s)", s.integration, s.reason)
		if s.receiver != "" {
			summary += " for receiver " + s.receiver
		}
		if len(sus) > 0 {
			summary += "; failing channel: " + strings.Join(sus, " or ")
		}
		fields := []zap.Field{zap.String("key", key), zap.String("integration", s.integration), zap.String("reason", s.reason),
			zap.String("receiver", s.receiver), zap.Strings("suspects", sus)}
		if !e.bucketFor(key).take(now) {
			e.logger.Warn("monitor: channel failed", append(fields, zap.String("suppressed", "more than four an hour for this key"))...)
			continue
		}
		var to []string
		for _, ch := range e.fail.operator {
			if !slices.Contains(sus, ch) {
				to = append(to, ch)
			}
		}
		e.logger.Warn("monitor: channel failed", append(fields, zap.Strings("told", to))...)
		alerts := make([]amAlert, 0, len(to))
		for _, ch := range to {
			alerts = append(alerts, amAlert{
				Labels: map[string]string{
					"alertname": alertChannelFailed, labelChannel: ch, "severity": SeverityWarning,
					"subject": "box", "integration": s.integration, "suspects": strings.Join(sus, ","),
				},
				Annotations: map[string]string{"summary": e.clean(summary)},
				StartsAt:    now, EndsAt: now.Add(channelFailedLife),
			})
		}
		if len(alerts) > 0 {
			e.enqueue(&pending{key: key, alerts: alerts, first: now, journal: true})
		}
	}
}
