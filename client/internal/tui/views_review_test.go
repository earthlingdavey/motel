package tui

import (
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// TestDeepJSONOnScreen: y on an app whose status holds one more field,
// 58 KB nested 9000 deep (the review's), takes under 200 MB and a
// second (about 6 MB, 56 under -race, which counts the frames drawn
// meanwhile; it was over 2 GB), and the overlay says, in the screen's cells, that its lines
// are indented as at IndentMax and how many it does not draw.
func TestDeepJSONOnScreen(t *testing.T) {
	deep := strings.Repeat("[", 9000) + strings.TrimSuffix(strings.Repeat("1,", 20000), ",") + strings.Repeat("]", 9000)
	body := `{"app": "wiki", "phase": "idle", "running": false, "available_versions": [], "deploys": [], "x": ` + deep + `}`
	h := start(t, 80, 24, func(f *demo.Fake) { f.Body("status/wiki.json", body) })
	h.open(app("wiki"))
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	at := time.Now()
	h.press("y")
	took := time.Since(at)
	runtime.ReadMemStats(&m1)
	if mb := (m1.TotalAlloc - m0.TotalAlloc) >> 20; mb >= 200 || took >= time.Second {
		t.Errorf("y on a %d-byte body nested 9000 deep took %v and %d MB", len(body), took, mb)
	}
	all := joinRows(overlayCells(h), " ")
	if !strings.Contains(all, "nested deeper than "+strconv.Itoa(model.IndentMax)+" levels and indented as at") {
		t.Errorf("the overlay does not say the deep lines are indented less:\n%s", all)
	}
	h.press(tcell.KeyEnd)
	all = joinRows(overlayCells(h), " ")
	// Its lines: { and six fields, the last opening the first [, 8999
	// more [, 20000 numbers, 9000 ], }.
	if more := 7 + 8999 + 20000 + 9000 + 1 - model.OverlayLines; !strings.Contains(all, "… "+strconv.Itoa(more)+" more lines not drawn here") {
		t.Errorf("the overlay's end does not count the %d lines not drawn:\n%s", more, all)
	}
}
