package tui

import (
	"strconv"
	"testing"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// sizes are the design size and the narrow one every screen must
// still work at.
var sizes = [][2]int{{100, 30}, {80, 24}}

func app(a string) model.View { return model.View{Screen: model.ScreenApp, App: a} }

func record(a, v string) model.View {
	return model.View{Screen: model.ScreenRecord, App: a, Version: v}
}

// TestScreens renders each screen from the fixtures, the hostile app's
// among them, at both sizes.
func TestScreens(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*demo.Fake)
		open  []model.View
	}{
		{"apps", nil, nil},
		{"app-blog", nil, []model.View{app("blog")}},
		{"record-blog-failed", nil, []model.View{app("blog"), record("blog", "e9a0b3f2c118")}},
		{"record-blog-succeeded", nil, []model.View{app("blog"), record("blog", "7c1e55d0a913")}},
		{"app-hostile", nil, []model.View{app("hostile")}},
		{"record-hostile", nil, []model.View{app("hostile"), record("hostile", "v7")}},
		{"one-app", func(f *demo.Fake) { f.Body("apps.json", `{"apps": [{"name": "blog"}]}`) }, nil},
	}
	for _, c := range cases {
		for _, sz := range sizes {
			name := c.name + "-" + strconv.Itoa(sz[0]) + "x" + strconv.Itoa(sz[1])
			t.Run(name, func(t *testing.T) {
				h := start(t, sz[0], sz[1], c.setup)
				for _, v := range c.open {
					h.open(v)
				}
				golden(t, name, h.screen())
			})
		}
	}
}
