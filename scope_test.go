package asset

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestLoadDedupesPerTypeAndKey(t *testing.T) {
	h := newHarness(t, fstest.MapFS{"a.txt": file("alpha")}, Options{})
	s1, s2 := h.srv.NewScope("one"), h.srv.NewScope("two")

	a1 := Load[text](s1, "a.txt")
	a1again := Load[text](s1, "./a.txt")
	a2 := Load[text](s2, "a.txt")
	if a1 != a1again || a1 != a2 {
		t.Fatalf("handles differ: %v %v %v", a1, a1again, a2)
	}
	h.srv.Update()
	if got := h.loader.decodes.Load(); got != 1 {
		t.Fatalf("decoded %d times, want 1", got)
	}
	if h.texts.Len() != 1 {
		t.Fatalf("Len = %d, want 1", h.texts.Len())
	}

	// Idempotent per scope: one release of s1 drops its single hold.
	s1.Release()
	h.srv.Update()
	if !h.texts.Ready(a1) {
		t.Fatal("asset unloaded while scope two still holds it")
	}
	s2.Release()
	h.srv.Update()
	if h.texts.Ready(a1) {
		t.Fatal("asset still loaded with no holds")
	}
}

func TestSharedAssetSurvivesOneScopeRelease(t *testing.T) {
	h := newHarness(t, fstest.MapFS{"shared.txt": file("s"), "only1.txt": file("1")}, Options{})
	l1, l2 := h.srv.NewScope("level-1"), h.srv.NewScope("level-2")
	shared := Load[text](l1, "shared.txt")
	only1 := Load[text](l1, "only1.txt")
	Load[text](l2, "shared.txt")
	h.srv.Update()

	l1.Release()
	ev := h.srv.Update()

	if !h.texts.Ready(shared) {
		t.Fatal("shared asset was unloaded")
	}
	if _, st := h.texts.Lookup(only1); st != Missing {
		t.Fatalf("only1 state = %v, want Missing (unloaded)", st)
	}
	if got := h.loader.unloads.Load(); got != 1 {
		t.Fatalf("Unload called %d times, want 1", got)
	}
	if !equalKinds(kinds(ev), []EventKind{EventUnloaded}) {
		t.Fatalf("events = %v", kinds(ev))
	}
}

func TestUnloadFreesSlotAndBumpsGeneration(t *testing.T) {
	h := newHarness(t, fstest.MapFS{"a.txt": file("a")}, Options{})
	sc := h.srv.NewScope("s")
	a := Load[text](sc, "a.txt")
	h.srv.Update()
	genBefore := h.texts.slots[a.index].gen

	sc.Release()
	h.srv.Update()

	if h.texts.slots[a.index].gen != genBefore+1 {
		t.Fatal("generation not bumped on free")
	}
	if h.texts.Len() != 0 {
		t.Fatalf("Len = %d after unload", h.texts.Len())
	}
	// The scope is reusable.
	b := Load[text](sc, "a.txt")
	h.srv.Update()
	if !h.texts.Ready(b) || b == a {
		t.Fatalf("reload after release: ready=%v same handle=%v", h.texts.Ready(b), b == a)
	}
}

func TestReloadBeforeUpdateRevivesAsset(t *testing.T) {
	h := newHarness(t, fstest.MapFS{"a.txt": file("a")}, Options{})
	sc := h.srv.NewScope("s")
	a := Load[text](sc, "a.txt")
	h.srv.Update()

	sc.Release()
	again := Load[text](sc, "a.txt") // before the Update that would unload it
	h.srv.Update()

	if again != a || !h.texts.Ready(a) {
		t.Fatal("asset released and reloaded within one tick was unloaded")
	}
	if h.loader.unloads.Load() != 0 {
		t.Fatal("Unload ran for a revived asset")
	}
}

func TestHold(t *testing.T) {
	h := newHarness(t, fstest.MapFS{"a.txt": file("a")}, Options{})
	level, keep := h.srv.NewScope("level"), h.srv.NewScope("keep")
	a := Load[text](level, "a.txt")
	h.srv.Update()
	Hold(keep, a)
	Hold(keep, a) // idempotent
	var zero Handle[text]
	Hold(keep, zero) // ignored

	level.Release()
	h.srv.Update()
	if !h.texts.Ready(a) {
		t.Fatal("Hold did not keep the asset alive")
	}
	keep.Release()
	h.srv.Update()
	if h.texts.Ready(a) {
		t.Fatal("asset alive after every holder released")
	}
}

func TestHoldStale(t *testing.T) {
	for _, strict := range []bool{false, true} {
		h := newHarness(t, fstest.MapFS{"a.txt": file("a")}, Options{StrictHandles: strict})
		sc := h.srv.NewScope("s")
		a := Load[text](sc, "a.txt")
		h.srv.Update()
		sc.Release()
		h.srv.Update()

		if strict {
			mustPanic(t, "asset: stale handle", func() { Hold(sc, a) })
		} else {
			Hold(sc, a)
			if h.srv.Stats().StaleHits != 1 {
				t.Fatal("non-strict Hold of a stale handle was not counted")
			}
		}
	}
}

func TestGlobalScopeCannotBeReleased(t *testing.T) {
	h := newHarness(t, nil, Options{})
	mustPanic(t, "asset: cannot release the global scope", func() { h.srv.Global().Release() })
}

func TestProgressDoneErr(t *testing.T) {
	h := newHarness(t, fstest.MapFS{"a.txt": file("a"), "bad.txt": file("bad")}, Options{})
	sc := h.srv.NewScope("s")
	if !sc.Done() {
		t.Fatal("empty scope is not Done")
	}
	Load[text](sc, "a.txt")
	Load[text](sc, "bad.txt")

	if ready, total := sc.Progress(); ready != 0 || total != 2 {
		t.Fatalf("before Update: Progress = %d/%d", ready, total)
	}
	if sc.Done() {
		t.Fatal("Done before Update")
	}
	h.srv.Update()
	if ready, total := sc.Progress(); ready != 1 || total != 2 {
		t.Fatalf("after Update: Progress = %d/%d", ready, total)
	}
	if !sc.Done() {
		t.Fatal("not Done after both settled")
	}
	err := sc.Err()
	if err == nil || !strings.Contains(err.Error(), "bad.txt") || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("Err = %v", err)
	}
}

func TestCloseAndPanics(t *testing.T) {
	h := newHarness(t, fstest.MapFS{"a.txt": file("a")}, Options{})
	sc := h.srv.NewScope("s")
	a := Load[text](sc, "a.txt")
	h.srv.Update()

	h.srv.Close()
	h.srv.Close() // second Close is a no-op

	if h.loader.unloads.Load() != 1 {
		t.Fatalf("Close unloaded %d assets, want 1", h.loader.unloads.Load())
	}
	mustPanic(t, "asset: server closed", func() { Load[text](sc, "a.txt") })
	mustPanic(t, "asset: server closed", func() { Hold(sc, a) })
	if h.srv.Update().Len() != 0 {
		t.Fatal("Update after Close returned events")
	}
	sc.Release() // no-op, must not panic
	if h.texts.Get(a).S != "placeholder" {
		t.Fatal("handle still resolves after Close")
	}
}

func TestStats(t *testing.T) {
	h := newHarness(t, fstest.MapFS{"a.txt": file("a"), "bad.txt": file("bad")}, Options{})
	sc := h.srv.NewScope("s")
	Load[text](sc, "a.txt")
	Load[text](sc, "bad.txt")

	st := h.srv.Stats()
	if st.Queued != 2 || st.States[Queued] != 2 {
		t.Fatalf("before Update: %+v", st)
	}
	h.srv.Update()
	st = h.srv.Stats()
	if st.States[Ready] != 1 || st.States[Failed] != 1 || st.Queued != 0 {
		t.Fatalf("after Update: %+v", st)
	}
	if len(st.Types) != 1 || st.Types[0].States[Ready] != 1 || st.Types[0].Name != "asset.text" {
		t.Fatalf("per-type stats: %+v", st.Types)
	}
}
