package notify

import (
	"context"
	"fmt"
	"maps"
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
// the counter names the receiver, which holds one channel (render): the
// failing channel is that one; without it, every channel of the
// integration.

// channelFailedLife is the alert's endsAt: re-posted while the counter
// keeps rising, gone a quarter-hour after it stops.
const channelFailedLife = 15 * time.Minute

// failWatch is the engine's memory of the counter, and the config it
// reads the counter against.
type failWatch struct {
	prev map[string]float64 // integration/receiver/reason → value
	// loaded is Alertmanager's last config load as the last read that
	// had one gave it (amMetrics.loaded; a read without one keeps it).
	// started is when this process began, which the first read's load
	// is compared with, and reloaded says that Alertmanager answered a
	// reload this process asked for before that read: a load after the
	// start, whatever the wall clock did since.
	loaded   float64
	started  time.Time
	reloaded bool
	operator []string
	kinds    map[string]string   // channel → integration
	channels map[string][]string // receiver → channels
}

// failureAudience is the operator's channels a ChannelFailed may go
// to: those whose severity floor takes a warning, which it is. A
// channel kept for criticals hears no failure of another, from motel
// or from vmalert.
func failureAudience(r *resolved) []string {
	var out []string
	for _, n := range r.operator {
		if r.channels[n].receives(SeverityWarning) {
			out = append(out, n)
		}
	}
	return out
}

func sampleKey(s failedSample) string { return s.integration + "/" + s.receiver + "/" + s.reason }

// loadSince reports whether Alertmanager's counters started again from
// 0 at a config load since the last read, so that all of a count is
// new. At the first read since this process started, a load after the
// start says so too, and a failure before that read is told: the reload
// moteld's start asks for (reloaded, which needs no clock), or a stamp
// after the start (setup's start of Alertmanager, its socket's at
// motel's first post). A load from before the start leaves the first
// read a baseline, since an earlier moteld may have told of what it
// counts.
func (w *failWatch) loadSince(m amMetrics) bool {
	if w.prev == nil {
		return m.loaded > 0 && (w.reloaded || !w.started.IsZero() && m.loaded > float64(w.started.UnixMilli())/1e3)
	}
	return newLoad(m.loaded, w.loaded)
}

// noteReload is applyRender's: Alertmanager answered a reload this
// process asked for. Before the first read, it makes that read count
// from 0 (loadSince); after, the stamp says it.
func (e *engine) noteReload() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.fail.prev == nil {
		e.fail.reloaded = true
	}
}

// rises compares a read of the counter with the last and returns the
// series that rose. load is loadSince's answer for the read: the
// baseline is then 0. Otherwise the first read is a baseline, and a
// counter lower than the last started again at a load the read gives
// no stamp of (a restart of an Alertmanager without
// receiver-name-in-metrics), and counts from zero.
func (w *failWatch) rises(samples []failedSample, load bool) []failedSample {
	first := w.prev == nil && !load
	next := make(map[string]float64, len(samples))
	var out []failedSample
	for _, s := range samples {
		k := sampleKey(s)
		next[k] = s.value
		if first {
			continue
		}
		prev, seen := w.prev[k]
		if load {
			prev, seen = 0, true
		}
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
	load := e.fail.loadSince(m)
	if m.loaded > 0 {
		e.fail.loaded = m.loaded
	}
	// A hold standing ends at a delivery through its suspects; one that
	// has stood heartbeatHoldAfter holds the heartbeat. Before this read's
	// rises, whose own baseline is this read.
	e.watchUntold(m, now, load)
	for _, s := range e.fail.rises(m.failed, load) {
		sus := e.fail.suspects(s)
		key := "notify/box/channel_failed/" + strings.Join(sus, ",")
		summary := fmt.Sprintf("Alertmanager could not deliver through %s (%s)", s.integration, s.reason)
		if s.receiver != "" {
			summary += " for receiver " + s.receiver
		}
		if len(sus) > 0 {
			summary += "; failing channel: " + strings.Join(sus, " or ")
		}
		summary = e.clean(summary)
		fields := []zap.Field{zap.String("key", key), zap.String("integration", s.integration), zap.String("reason", s.reason),
			zap.String("receiver", s.receiver), zap.Strings("suspects", sus)}
		var to []string
		for _, ch := range e.fail.operator {
			if !slices.Contains(sus, ch) {
				to = append(to, ch)
			}
		}
		// No channel to tell: the heartbeat is what can, by stopping.
		if len(to) == 0 {
			e.untoldFor(key, s.receiver, sus, s.integration, summary, m, now)
		} else if u := e.untold[key]; u != nil {
			// A record from before a load that gave the failure someone to
			// tell stands until its end, and is the latest rise's still.
			e.refreshUntold(key, u, s.integration, summary, now)
		}
		// The copy of a failure a record holds keeps the record's start,
		// as the record's own posts of it do (watchUntold).
		start := now
		if u := e.untold[key]; u != nil {
			start = u.since
		}
		if !e.bucketFor(key).take(now) {
			e.logger.Warn("monitor: channel failed", append(fields, zap.String("suppressed", "more than four an hour for this key"))...)
			// The bucket bounds what channels are told; the read-only copy
			// tells none, so it is posted at every rise, and the people's
			// reads keep one alert while the failure goes on.
			e.enqueue(&pending{key: key, first: now, alerts: []amAlert{readOnlyCopy(s.integration, sus, summary, start, now)}})
			continue
		}
		e.logger.Warn("monitor: channel failed", append(fields, zap.Strings("told", to))...)
		alerts := make([]amAlert, 0, len(to)+1)
		for _, ch := range to {
			alerts = append(alerts, failureCopy(ch, s.integration, sus, summary, now, now))
		}
		// A record standing across a load that gave its failure channels
		// to tell keeps what it told them, to resolve with its copy: the
		// read folds them under the copy, and would show them once the
		// copy is resolved.
		if u := e.untold[key]; u != nil {
			for _, a := range alerts {
				u.addressed[a.Labels[labelChannel]+"\x00"+a.Labels["integration"]] = a.Labels
			}
		}
		alerts = append(alerts, readOnlyCopy(s.integration, sus, summary, start, now))
		// A post that tells no channel failing is the hold's to hear, as
		// the hold's own re-posts are: no error line of its own.
		e.enqueue(&pending{key: key, alerts: alerts, first: now, journal: len(to) > 0})
	}
}

// readOnlyCopy is a failure's read-only copy (DESIGN-notify.md, "Failed
// delivery"): ChannelFailed with the addressed copies' labels but
// notify_channel readOnlyChannel, which no channel's name can be, so
// route 2 matches it, none of its leaves does, and it ends at that
// route's blackhole: held by Alertmanager and read by the people's
// box-wide read, told to no channel. It is posted with every rise,
// whoever else is told and whatever the key's bucket lets through, and
// while a hold stands at every read (watchUntold), from the hold's
// start, until the record ends (endUntold). The blackhole has no
// integration, so the copy moves none of the counters a rise or a
// hold's end is read from.
func readOnlyCopy(integration string, sus []string, summary string, start, now time.Time) amAlert {
	return failureCopy(readOnlyChannel, integration, sus, summary, start, now)
}

// failureCopy is one ChannelFailed of a failure: addressed to channel ch
// (route 2's leaf for it), or to readOnlyChannel, its read-only copy.
// One builder for both, which vmalert's rules copy the labels of and
// endTestedCopy resolves by.
func failureCopy(ch, integration string, sus []string, summary string, start, now time.Time) amAlert {
	return amAlert{
		Labels: map[string]string{
			"alertname": alertChannelFailed, labelChannel: ch, "severity": SeverityWarning,
			"subject": "box", "integration": integration, "suspects": strings.Join(sus, ","),
		},
		Annotations: map[string]string{"summary": summary},
		StartsAt:    start, EndsAt: now.Add(channelFailedLife),
	}
}

// The heartbeat's hold (DESIGN-notify.md, "Dead-man and uptime"): a
// failure no channel can be told of (failureAudience holds no channel
// but the suspects) would otherwise be heard nowhere while
// the heartbeat's check off the box stays green. So once such a failure
// has stood heartbeatHoldAfter with no delivery through a suspect's
// receivers since, motel stops posting Watchdog; the alert ends within
// heartbeatLife (its receiver sends no resolved), and the check off the
// box reports the box down through its own channels. The next delivery
// through a suspect's receivers (`moteld monitor test` makes one) ends
// the hold. It is in memory, as the counter's baseline is: a restart
// of moteld ends it.

// heartbeatHoldAfter is how long a failure told to no channel stands
// before the heartbeat is held: a failed notification is not logged,
// so Alertmanager sends it again at the group's next flushes, and a
// passing fault (one 5xx) is delivered meanwhile.
const heartbeatHoldAfter = 5 * time.Minute

// untold is a failure no channel was told of, and the hold it brings.
type untold struct {
	since    time.Time
	suspects []string
	// seen is each suspect receiver's deliveries (attempts that did not
	// fail) at the last read.
	seen map[string]float64
	held bool
	// integration and summary are the failure's: the read-only copy the
	// record keeps posting carries them.
	integration, summary string
	// addressed is the copies told to channels while the record stood
	// (after a load gave the failure channels to tell), by channel and
	// integration: resolved with the read-only copy at the record's end.
	addressed map[string]map[string]string
}

// receiversOf is every receiver that sends to one of chans.
func (w *failWatch) receiversOf(chans []string) []string {
	var out []string
	for name, cs := range w.channels {
		for _, c := range cs {
			if slices.Contains(chans, c) {
				out = append(out, name)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

func deliveries(m amMetrics, receiver string) float64 {
	return m.requests[receiver] - m.requestsFailed[receiver]
}

// untoldFor records a failure told to no channel; one recorded already
// for the same suspects keeps its start. A failure with no suspect is
// the heartbeat's own, which its check off the box hears already. One
// whose receiver is not this config's (the counter names none, or a
// name from before a reload) has suspects that are a guess, and no
// delivery count by channel to end a hold by. With mu held.
func (e *engine) untoldFor(key, receiver string, sus []string, integration, summary string, m amMetrics, now time.Time) {
	if len(sus) == 0 {
		return
	}
	if _, known := e.fail.channels[receiver]; !known {
		return
	}
	if u, ok := e.untold[key]; ok {
		e.refreshUntold(key, u, integration, summary, now)
		return
	}
	u := &untold{since: now, suspects: sus, seen: map[string]float64{}, integration: integration, summary: summary,
		addressed: map[string]map[string]string{}}
	for _, rc := range e.fail.receiversOf(sus) {
		u.seen[rc] = deliveries(m, rc)
	}
	e.untold[key] = u
}

// watchUntold is one read's part of the hold: a delivery through a
// suspect's receivers ends what it records, and what has stood
// heartbeatHoldAfter without one holds the heartbeat. What still stands
// has its read-only copy posted again, from its start, so that the
// people's reads hold one alert, with one start, for as long as the
// record stands; what ends has it resolved (endUntold). After a config
// load (load: loadSince's answer) Alertmanager counts from 0, so any
// delivery counted is since it: the documented mend, a reload, then
// `moteld monitor test`. A receiver with no count from an earlier read
// (a reload renamed it) is otherwise counted from this one, as is a
// count lower than the last. With mu held.
func (e *engine) watchUntold(m amMetrics, now time.Time, load bool) {
	for key, u := range e.untold {
		through := ""
		for _, rc := range e.fail.receiversOf(u.suspects) {
			d := deliveries(m, rc)
			prev, ok := u.seen[rc]
			if load {
				prev, ok = 0, true
			}
			if ok && d > prev && through == "" {
				through = rc
			}
			u.seen[rc] = d
		}
		if through != "" {
			if u.held {
				e.logger.Info("monitor: heartbeat resumed", zap.Strings("suspects", u.suspects), zap.String("delivered_through", through))
			}
			e.endUntold(key, u, now)
			continue
		}
		// With no heartbeat line there is no check off the box to hear a
		// hold: what stands is recorded, and held once a load adds one.
		if !u.held && now.Sub(u.since) >= heartbeatHoldAfter && e.heartbeatSet() {
			u.held = true
			e.logger.Warn("monitor: heartbeat held: a failed delivery no channel was told of; the heartbeat's check off the box will report this box down",
				zap.Strings("suspects", u.suspects), zap.Time("failed_at", u.since),
				zap.String("resumes", "at the next delivery through the channel: mend it, then moteld monitor test "+u.suspects[0]))
		}
		// No bucket: one post a read, as the heartbeat's, whatever the
		// rises; a post that fails is the queue's to try again.
		e.enqueue(&pending{key: key + "/read-only", first: now,
			alerts: []amAlert{readOnlyCopy(u.integration, u.suspects, u.summary, u.since, now)}})
	}
}

// heartbeatHeld reports whether a failure no channel was told of holds
// the heartbeat. With mu held.
func (e *engine) heartbeatHeld() bool {
	for _, u := range e.untold {
		if u.held {
			return true
		}
	}
	return false
}

// heartbeatSet reports whether the config the engine serves has a
// heartbeat line. With mu held.
func (e *engine) heartbeatSet() bool {
	return e.active != nil && e.active.cfg != nil && e.active.cfg.heartbeat != ""
}

// keepUntold drops, at a config load, what records only channels the
// new config no longer has: nothing can deliver through them, and the
// hold would stand until a restart. A config with no heartbeat line
// keeps what stands but holds nothing (watchUntold holds it again once
// a load adds the line). With mu held.
func (e *engine) keepUntold(channels map[string]Channel) {
	for _, u := range e.untold {
		if u.held && !e.heartbeatSet() {
			u.held = false
			e.logger.Info("monitor: heartbeat resumed", zap.Strings("suspects", u.suspects), zap.String("why", "the config has no heartbeat line"))
		}
	}
	for key, u := range e.untold {
		if slices.ContainsFunc(u.suspects, func(c string) bool { _, ok := channels[c]; return ok }) {
			continue
		}
		if u.held {
			e.logger.Info("monitor: heartbeat resumed", zap.Strings("suspects", u.suspects), zap.String("why", "the config no longer has the channel"))
		}
		e.endUntold(key, u, e.now())
	}
}

// endUntold ends what a record holds: the record, and its read-only
// copy, resolved (endsAt now), so that the people's reads stop showing
// a failure the channel has delivered past, or one of a channel the
// config no longer has. With mu held.
func (e *engine) endUntold(key string, u *untold, now time.Time) {
	delete(e.untold, key)
	e.endCopy(key, u, now)
	// The copies told to channels while it stood: their receivers send no
	// resolved, so ending them tells no one, and the read does not show
	// them once the read-only copy is gone.
	keys := slices.Sorted(maps.Keys(u.addressed))
	var ended []amAlert
	for _, k := range keys {
		ended = append(ended, amAlert{Labels: u.addressed[k], Annotations: map[string]string{"summary": u.summary}, StartsAt: u.since, EndsAt: now})
	}
	if len(ended) > 0 {
		e.enqueue(&pending{key: key + "/addressed", first: now, alerts: ended})
	}
}

// refreshUntold gives a standing record a later rise's integration and
// words, which that rise's own copy carries, so the record's re-posts
// say the same. A copy of another integration (a load changed the
// channel's kind) is another alert: ended, as the record moves on.
// With mu held.
func (e *engine) refreshUntold(key string, u *untold, integration, summary string, now time.Time) {
	if u.integration != integration {
		e.endCopy(key, u, now)
	}
	u.integration, u.summary = integration, summary
}

// endTestedCopy is a delivered `moteld monitor test <ch>`'s, posted
// after every one: the copies of a failure whose one suspect is ch are
// resolved. They are the read-only one and the one addressed to each
// other operator channel (which the read folds under it, and would show
// once it is gone), their labels built by failureCopy from the
// channel's name and its integration as the config being served has
// it, as the failure's own posts build them; and every such copy
// Alertmanager holds, by its own labels, so that one addressed to a
// channel a load has since taken out or set to criticals alone, or a
// read-only one of the channel's kind before a load, ends too. It needs
// no record in memory, so a channel mended and tested after a restart
// of moteld, which drops the records, has its row end too. A copy
// naming several suspects (a counter that names no receiver) is not one
// of these, and is left to its record or its 15 minutes. With the
// metrics package and another operator channel to tell, nothing is
// resolved: vmalert's rules post the addressed copies again at each
// evaluation while their window holds the failure, and with the
// read-only copy gone the read would list each of them, unfolded, and
// ring. The addressed copies' receivers send no resolved, so no channel
// hears it; Alertmanager holds a resolve of an alert it does not hold
// as resolved, which no read lists, and the resolve carries no summary
// of its own. It goes through the queue, behind any re-post of the copy
// queued before it.
func (e *engine) endTestedCopy(ctx context.Context, ch string) {
	e.mu.Lock()
	served := e.active
	integration, ok := e.fail.kinds[ch]
	others := slices.DeleteFunc(slices.Clone(e.fail.operator), func(c string) bool { return c == ch })
	skip := served == nil || !ok || len(others) > 0 && served.Metrics != nil
	held, logger := e.failureCopies, e.logger
	e.mu.Unlock()
	if skip {
		return // no longer a channel of the config served, or vmalert's to post again
	}
	labels := map[string]map[string]string{}
	add := func(l map[string]string) {
		ks := slices.Sorted(maps.Keys(l))
		var b strings.Builder
		for _, k := range ks {
			b.WriteString(k + "\x00" + l[k] + "\x00")
		}
		labels[b.String()] = l
	}
	for _, to := range append([]string{readOnlyChannel}, others...) {
		add(failureCopy(to, integration, []string{ch}, "", time.Time{}, time.Time{}).Labels)
	}
	if held != nil {
		got, err := held(ctx, ch)
		if err != nil {
			// The built ones still go: what the served config would post.
			logger.Debug("monitor: reading the ChannelFailed copies a test ends", zap.String("channel", ch), zap.Error(err))
		}
		for _, l := range got {
			add(l)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.active != served {
		// A load or notify's stop during the read: what was checked is
		// another config's, and a stopped engine's queue is not posted.
		return
	}
	now := e.now()
	ended := make([]amAlert, 0, len(labels))
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		ended = append(ended, amAlert{Labels: labels[k], StartsAt: now, EndsAt: now})
	}
	e.enqueue(&pending{key: "notify/box/channel_failed/" + ch + "/tested", first: now, alerts: ended})
}

// endCopy posts u's read-only copy resolved. With mu held.
func (e *engine) endCopy(key string, u *untold, now time.Time) {
	c := readOnlyCopy(u.integration, u.suspects, u.summary, u.since, now)
	c.EndsAt = now
	e.enqueue(&pending{key: key + "/read-only", first: now, alerts: []amAlert{c}})
}
