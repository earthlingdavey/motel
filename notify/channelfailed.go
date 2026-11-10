package notify

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
)

// channel_failed without the metrics package (DESIGN-notify.md,
// "Failed delivery"): once a minute motel reads Alertmanager's own
// alertmanager_notifications_failed_total over the socket, and when it
// has risen tells the teacher's other channels, one ChannelFailed alert
// per channel, each routed to that channel alone (notify_channel=).
// With --enable-feature=receiver-name-in-metrics (the drop-in sets it)
// the counter names the receiver, and the failing channel is narrowed
// to that receiver's channels of the failing integration; without it,
// to every channel of the integration.

const failedMetric = "alertmanager_notifications_failed_total"

// channelFailedLife is the alert's endsAt: re-posted while the counter
// keeps rising, gone a quarter-hour after it stops.
const channelFailedLife = 15 * time.Minute

// failedSample is one series of the counter.
type failedSample struct {
	integration, receiver, reason string
	value                         float64
}

// parseFailed reads the counter's series out of a /metrics body.
func parseFailed(body []byte) []failedSample {
	var out []failedSample
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		rest, ok := strings.CutPrefix(line, failedMetric+"{")
		if !ok {
			continue
		}
		end := strings.LastIndex(rest, "} ")
		if end < 0 {
			continue
		}
		labels := parseLabels(rest[:end])
		v, err := strconv.ParseFloat(strings.Fields(rest[end+2:])[0], 64)
		if err != nil {
			continue
		}
		out = append(out, failedSample{integration: labels["integration"], receiver: labels["receiver_name"], reason: labels["reason"], value: v})
	}
	return out
}

// parseLabels reads `a="x",b="y\"z"` as the exposition format writes it.
func parseLabels(s string) map[string]string {
	out := map[string]string{}
	for s != "" {
		eq := strings.IndexByte(s, '=')
		if eq < 0 || eq+1 >= len(s) || s[eq+1] != '"' {
			return out
		}
		name := strings.TrimSpace(s[:eq])
		s = s[eq+2:]
		var b strings.Builder
		i := 0
		for ; i < len(s) && s[i] != '"'; i++ {
			if s[i] == '\\' && i+1 < len(s) {
				i++
				switch s[i] {
				case 'n':
					b.WriteByte('\n')
				default:
					b.WriteByte(s[i])
				}
				continue
			}
			b.WriteByte(s[i])
		}
		out[name] = b.String()
		if i+1 >= len(s) {
			return out
		}
		s = strings.TrimPrefix(s[i+1:], ",")
	}
	return out
}

// failWatch is the engine's memory of the counter, and the config it
// reads the counter against.
type failWatch struct {
	prev     map[string]float64 // integration/receiver/reason → value
	teacher  []string
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
// each rise to every teacher channel that is not a suspect.
func (e *engine) scrapeFailed(ctx context.Context, read func(context.Context) ([]byte, error)) {
	body, err := read(ctx)
	if err != nil {
		e.logger.Debug("notify: reading Alertmanager's metrics", zap.Error(err))
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.active == nil {
		return
	}
	now := e.now()
	for _, s := range e.fail.rises(parseFailed(body)) {
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
			e.logger.Warn("notify: channel failed", append(fields, zap.String("suppressed", "more than four an hour for this key"))...)
			continue
		}
		var to []string
		for _, ch := range e.fail.teacher {
			if !slices.Contains(sus, ch) {
				to = append(to, ch)
			}
		}
		e.logger.Warn("notify: channel failed", append(fields, zap.Strings("told", to))...)
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
