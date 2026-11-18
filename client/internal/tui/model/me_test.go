package model_test

import (
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// at takes every read the engine makes at now, answering each with
// answer, and says whether whoami was among them.
func at(e *model.Engine, now time.Time, answer func(model.Job) error) (whoami bool) {
	e.Touch(now)
	for range 100 {
		j, ok, _ := e.Take(now)
		if !ok {
			return whoami
		}
		whoami = whoami || j.Kind == model.KWhoAmI
		err := answer(j)
		if j.Kind == model.KApps && err == nil {
			e.SetApps(names(3), now)
		}
		e.Done(j, now, err)
	}
	return whoami
}

// forbidApp answers a 403 for app-0's status and every other read.
func forbidApp(j model.Job) error {
	if j.Kind == model.KStatus && j.App == "app-0" {
		return fail(model.FailForbidden)
	}
	return nil
}

// fresh starts an engine whose first whoami, list and statuses are read
// at t0.
func fresh(t *testing.T) *model.Engine {
	t.Helper()
	e := model.NewEngine(t0)
	if !at(e, t0, func(model.Job) error { return nil }) {
		t.Fatal("whoami not read at the start")
	}
	return e
}

// TestMeAlwaysIsFiveMinutes: with who am I closed, whoami is read every
// five minutes, written here as the number.
func TestMeAlwaysIsFiveMinutes(t *testing.T) {
	if model.MeAlways != 5*time.Minute || model.MeEvery != time.Minute {
		t.Fatalf("MeAlways %s, MeEvery %s", model.MeAlways, model.MeEvery)
	}
	e := model.NewEngine(t0)
	var me []time.Time
	for _, c := range sim(e, names(3), t0, 30*time.Minute+time.Second, 10*time.Millisecond, ok) {
		if c.j.Kind == model.KWhoAmI {
			me = append(me, c.at)
		}
	}
	if len(me) != 7 {
		t.Fatalf("whoami read %d times in 30 minutes: %v", len(me), me)
	}
	for i := 1; i < len(me); i++ {
		if g := me[i].Sub(me[i-1]); g < 5*time.Minute || g > 5*time.Minute+time.Second {
			t.Errorf("whoami read %s after the last", g)
		}
	}
}

// TestForbiddenWhoAmILimits: a 403 asks whoami only when its last
// answer is a minute old and no 403 asked it in the last minute; each
// condition alone holds it back.
func TestForbiddenWhoAmILimits(t *testing.T) {
	again := func(e *model.Engine, now time.Time) bool {
		e.Again(now, model.Job{Kind: model.KStatus, App: "app-0"})
		return at(e, now, forbidApp)
	}

	// Both old: asked.
	e := fresh(t)
	if !again(e, t0.Add(2*time.Minute)) {
		t.Error("a 403 with whoami's answer two minutes old did not ask whoami")
	}

	// Whoami's answer under a minute old, no 403 before: not asked.
	e = fresh(t)
	if again(e, t0.Add(model.MeEvery-time.Second)) {
		t.Error("a 403 asked whoami with its answer under a minute old")
	}

	// Whoami's answer old, but a 403 asked it under a minute ago (and
	// that whoami was refused, so its answer is still old): not asked.
	e = fresh(t)
	now := t0.Add(2 * time.Minute)
	e.Again(now, model.Job{Kind: model.KStatus, App: "app-0"})
	asked := at(e, now, func(j model.Job) error {
		if j.Kind == model.KWhoAmI {
			return fail(model.FailThis)
		}
		return forbidApp(j)
	})
	if !asked {
		t.Fatal("the first 403 did not ask whoami")
	}
	if again(e, now.Add(model.MeEvery-time.Second)) {
		t.Error("a second 403 within a minute asked whoami again")
	}
	if !again(e, now.Add(model.MeEvery)) {
		t.Error("a 403 a minute after the last did not ask whoami, its answer old")
	}
}

// TestOpenMeFresh: opening who am I reads whoami at once only when its
// answer is a minute old; a fresher one waits for its minute.
func TestOpenMeFresh(t *testing.T) {
	e := fresh(t)
	now := t0.Add(20 * time.Second)
	e.MeOpen(true, now)
	if at(e, now, func(model.Job) error { return nil }) {
		t.Error("opening who am I read whoami with its answer 20 s old")
	}
	if !at(e, t0.Add(model.MeEvery), func(model.Job) error { return nil }) {
		t.Error("whoami not read a minute after its answer, who am I open")
	}
	e = fresh(t)
	now = t0.Add(model.MeEvery)
	e.MeOpen(true, now)
	if !at(e, now, func(model.Job) error { return nil }) {
		t.Error("opening who am I did not read whoami with its answer a minute old")
	}
}

// TestMeOpenGuard: MeOpen is told at every screen shown; told again
// what it already knows, it moves nothing, so a whoami a 403 asked for
// is not put back by the next key.
func TestMeOpenGuard(t *testing.T) {
	e := fresh(t)
	now := t0.Add(2 * time.Minute)
	if at(e, now, func(model.Job) error { return nil }) {
		t.Fatal("whoami read before its five minutes")
	}
	e.Again(now, model.Job{Kind: model.KStatus, App: "app-0"})
	e.Touch(now)
	j, ok, _ := e.Take(now)
	if !ok || j.Kind != model.KStatus || j.App != "app-0" {
		t.Fatalf("took %v", j)
	}
	e.Done(j, now, fail(model.FailForbidden))
	e.MeOpen(false, now) // a screen shown, who am I still closed
	if !at(e, now, forbidApp) {
		t.Error("whoami asked by a 403 was put back by MeOpen(false)")
	}

	e = fresh(t)
	now = t0.Add(2 * time.Minute)
	e.MeOpen(true, now)
	if !at(e, now, forbidApp) {
		t.Fatal("opening who am I did not read whoami")
	}
	e.MeOpen(true, now.Add(10*time.Second))
	if at(e, now.Add(10*time.Second), forbidApp) {
		t.Error("MeOpen(true) again read whoami again")
	}
}
