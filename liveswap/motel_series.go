package liveswap

import (
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
	// The store's numbers, the box's clock and words of closed sets:
	// nothing of the app's text.
	return RespondJSON(w, http.StatusOK, body)
}
