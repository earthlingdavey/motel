package liveswap

import (
	"context"

	"github.com/smallhoursorg/motel/liveswap/wire"
)

// SeriesSource is the metrics store's numbers for a person's read of
// one app's series (GET /_motel/apps/<app>/series). notify, which
// liveswap cannot import, installs it on a config whose monitor block
// has a metrics line (SetSeriesSource), as it installs MetricsSource.
// The route asks it only for the app of the handle Decide's answer
// made, by that app's name; the source builds every query itself, from
// the name and the window alone.
type SeriesSource interface {
	// AppSeries is app's panels over window, one of wire.SeriesRanges:
	// Store is wire.StoreOn with what the store holds, or
	// wire.StoreUnanswered when it did not answer within the source's
	// own short budget. It is called with the request's context, and
	// its answer may be shared with other callers: the caller only
	// reads it.
	AppSeries(ctx context.Context, app, window string) wire.Series
}

// SetSeriesSource installs the metrics store's source on this config.
// Called from the monitor app's Provision, after liveswap's own, so a
// config whose monitor block has no metrics line, or that has none,
// answers "store":"off".
func (a *App) SetSeriesSource(s SeriesSource) { a.seriesSource = s }
