package liveswap

import (
	"encoding/json"
	"net/http"

	"github.com/smallhoursorg/motel/liveswap/wire"
)

// appSeries is GET /_motel/apps/<app>/series[?range=]: the metrics
// store's panels of the handle's app over the window the query named
// (wire.SeriesRanges, the hour by default), each at the window's fixed
// step. The source is asked for that app alone, by the handle's name,
// and builds every query itself: nothing of the request's reaches the
// store but the window, one of a closed set. "store":"off" with no
// source (no metrics line). The body is about one app, so its words
// pass that app's filter; its numbers pass none (filterSeries).
func (h *Handler) appSeries(w http.ResponseWriter, r *http.Request, app appHandle, q appQuery) error {
	// The window as the set spells it: parseQuery took one of it.
	window := wire.SeriesRanges[0]
	for _, rng := range wire.SeriesRanges {
		if rng == q["range"] {
			window = rng
		}
	}
	body := wire.Series{Store: wire.StoreOff, Window: window, Panels: []wire.Panel{}}
	if src := h.app.seriesSource; src != nil {
		body = src.AppSeries(r.Context(), app.ma.name, window)
		if body.Panels == nil {
			body.Panels = []wire.Panel{}
		}
	}
	// Nothing in it names a version: the filter needs the app's values,
	// not its status's safe strings.
	f := app.ma.redactorFor(statusSnapshot{})
	if out, ok := filterSeries(body, f); ok {
		return respondFiltered(w, http.StatusOK, out)
	}
	// The filter withheld the body (its env_file unreadable) or changed
	// its structure: the body whole through the filter, numbers and all.
	return RespondJSON(w, http.StatusOK, body, f)
}

// filterSeries is the series body through the app's filter, its numbers
// passing none: the filter is given each panel's name and unit, and the
// store's word, the window (the box's own words, of closed sets), the
// step, the end and every point are put back after its last pass. They are the
// store's numbers and the box's clock, never the app's bytes, and an
// env_file value that is a run of digits (a port range, a unix time, an
// id), a date or a piece of one would otherwise be replaced inside a
// point or a time and leave a body no client reads. It reports false,
// for the caller to send the body whole through the filter, when the
// filter withheld the body or changed its structure (a value spanning
// two words). As for an app's alerts (filterAlerts), the filter runs
// over the words without the numbers between them, so a value made of
// JSON's own punctuation and a number's digits can stand in the bytes
// sent: an accepted row in DESIGN-access.md.
func filterSeries(body wire.Series, f *Redactor) (string, bool) {
	type words struct {
		Name string `json:"name"`
		Unit string `json:"unit"`
	}
	rows := make([]words, len(body.Panels))
	for i, p := range body.Panels {
		rows[i] = words{p.Name, p.Unit}
	}
	raw, _ := json.Marshal(struct {
		Panels []words `json:"panels"`
	}{rows}) // strings: nothing to fail
	var got map[string]json.RawMessage
	var panels []words
	if json.Unmarshal([]byte(f.redactJSON(raw)), &got) != nil || json.Unmarshal(got["panels"], &panels) != nil ||
		len(panels) != len(body.Panels) {
		return "", false
	}
	// The store's word and the window are the box's own, of closed sets,
	// put back as the numbers are: an env_file value "unanswered" would
	// otherwise make the answer no client takes.
	store, window := body.Store, body.Window
	// Each panel as its wire type writes it: what the filter left of its
	// words, the store's points.
	out := make([]wire.Panel, len(panels))
	for i := range panels {
		out[i] = wire.Panel{Name: panels[i].Name, Unit: panels[i].Unit, Points: body.Panels[i].Points}
		if out[i].Points == nil {
			out[i].Points = [][2]float64{}
		}
	}
	var b []byte
	var err error
	if len(got) == 1 {
		// The filter changed nothing at the top: the wire type's order.
		b, err = json.Marshal(wire.Series{Store: store, Window: window, StepSeconds: body.StepSeconds, End: body.End, Panels: out})
	} else {
		// A changed body's keys sorted, its redacted_env with them, as
		// redactJSON writes one.
		got["panels"], err = json.Marshal(out)
		if err == nil {
			got["store"], err = json.Marshal(store)
		}
		if err == nil {
			got["window"], err = json.Marshal(window)
		}
		if err == nil && body.StepSeconds != 0 {
			got["step_seconds"], err = json.Marshal(body.StepSeconds)
		}
		if err == nil && body.End != nil {
			got["end"], err = json.Marshal(body.End)
		}
		if err == nil {
			b, err = json.Marshal(got)
		}
	}
	if err != nil {
		return "", false
	}
	return string(b), true
}
