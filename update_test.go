package asset

import (
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestUpdateEvents(t *testing.T) {
	h := newHarness(t, fstest.MapFS{
		"a.txt":    file("a"),
		"bad.txt":  file("bad"),
		"fin.txt":  file("badfinish"),
		"gone.txt": file("g"),
	}, Options{})
	sc := h.srv.NewScope("s")
	a := Load[text](sc, "a.txt")
	bad := Load[text](sc, "bad.txt")
	fin := Load[text](sc, "fin.txt")
	gone := Load[text](h.srv.NewScope("tmp"), "gone.txt")
	_ = gone

	ev := h.srv.Update()
	want := map[string]EventKind{
		"a.txt": EventLoaded, "bad.txt": EventFailed, "fin.txt": EventFailed, "gone.txt": EventLoaded,
	}
	got := map[string]EventKind{}
	for e := range ev.All() {
		got[e.Key.Path] = e.Kind
		if e.Kind == EventFailed && e.Err == nil {
			t.Errorf("failed event for %s has no error", e.Key)
		}
		if e.Kind == EventLoaded && e.ID.Type != TypeIDOf[text](h.srv) {
			t.Errorf("event ID type = %d", e.ID.Type)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("event for %s = %v, want %v", k, got[k], v)
		}
	}

	// Failed: placeholder, error, OnError called, no panic.
	for _, hd := range []Handle[text]{bad, fin} {
		if h.texts.Get(hd).S != "placeholder" || h.texts.Err(hd) == nil {
			t.Fatalf("failed handle %v: placeholder/err wrong", hd)
		}
	}
	if len(h.errs) != 2 {
		t.Fatalf("OnError called %d times, want 2: %v", len(h.errs), h.errs)
	}
	if !strings.Contains(h.texts.Err(bad).Error(), "corrupt") || !strings.Contains(h.texts.Err(fin).Error(), "finish failed") {
		t.Fatalf("errors: %v / %v", h.texts.Err(bad), h.texts.Err(fin))
	}
	if !h.texts.Ready(a) {
		t.Fatal("a.txt not ready")
	}

	// Events are per Update: a quiet tick has none, an unload appears once.
	if n := h.srv.Update().Len(); n != 0 {
		t.Fatalf("quiet Update returned %d events", n)
	}
}

func TestEventsOfAndUnloaded(t *testing.T) {
	h := newHarness(t, fstest.MapFS{"a.txt": file("a")}, Options{})
	Register(h.srv, otherLoader{}, other{})
	sc := h.srv.NewScope("s")
	Load[text](sc, "a.txt")
	h.srv.Update()
	sc.Release()
	ev := h.srv.Update()

	n := 0
	for e := range ev.Of(TypeIDOf[text](h.srv)) {
		n++
		if e.Kind != EventUnloaded || e.Key.Path != "a.txt" {
			t.Fatalf("event = %+v", e)
		}
	}
	for range ev.Of(TypeIDOf[other](h.srv)) {
		t.Fatal("Of leaked an event of another type")
	}
	if n != 1 {
		t.Fatalf("unload events = %d", n)
	}
}

func TestFailedAssetIsNotRetried(t *testing.T) {
	h := newHarness(t, fstest.MapFS{"bad.txt": file("bad")}, Options{})
	sc := h.srv.NewScope("s")
	a := Load[text](sc, "bad.txt")
	h.srv.Update()
	b := Load[text](sc, "bad.txt")
	h.srv.Update()
	if a != b || h.loader.decodes.Load() != 1 {
		t.Fatalf("failed asset reloaded: same=%v decodes=%d", a == b, h.loader.decodes.Load())
	}
}

func TestLoadSync(t *testing.T) {
	for _, workers := range []int{0, 3} {
		h := newHarness(t, fstest.MapFS{"a.txt": file("a"), "bad.txt": file("bad")}, Options{Workers: workers})
		sc := h.srv.NewScope("s")

		a, err := LoadSync[text](sc, "a.txt")
		if err != nil || h.texts.Get(a).S != "a" {
			t.Fatalf("workers=%d: LoadSync = %v, %v", workers, h.texts.Get(a), err)
		}
		bad, err := LoadSync[text](sc, "bad.txt")
		if err == nil || !strings.Contains(err.Error(), "corrupt") {
			t.Fatalf("workers=%d: LoadSync(bad) err = %v", workers, err)
		}
		if h.texts.Get(bad).S != "placeholder" {
			t.Fatalf("workers=%d: failed LoadSync did not give the placeholder", workers)
		}
		// Events from the sync drive are delivered by the next Update.
		if n := h.srv.Update().Len(); n != 2 {
			t.Fatalf("workers=%d: events after LoadSync = %d, want 2", workers, n)
		}
	}
}

func TestLoadSyncIgnoresCommitBudget(t *testing.T) {
	files := fstest.MapFS{}
	for _, n := range []string{"a", "b", "c", "d"} {
		files[n+".txt"] = file(n)
	}
	h := newHarness(t, files, Options{CommitBudget: time.Nanosecond})
	clock := &fakeClock{t: time.Unix(0, 0)}
	h.srv.now = clock.now
	h.loader.onFinish = func(*FinishContext, string) { clock.advance(time.Second) }
	sc := h.srv.NewScope("s")
	for _, n := range []string{"b.txt", "c.txt", "d.txt"} {
		Load[text](sc, n)
	}
	if _, err := LoadSync[text](sc, "a.txt"); err != nil {
		t.Fatal(err)
	}
	if !sc.Done() {
		t.Fatal("LoadSync stopped at the budget")
	}
}

func TestUnloadOfInFlightAsset(t *testing.T) {
	h := newHarness(t, fstest.MapFS{"a.txt": file("a")}, Options{})
	sc := h.srv.NewScope("s")
	a := Load[text](sc, "a.txt")
	sc.Release() // before any Update: the job is dropped
	h.srv.Update()
	if h.loader.decodes.Load() != 0 || h.texts.Ready(a) {
		t.Fatalf("released-before-load asset was decoded (%d)", h.loader.decodes.Load())
	}
}

func TestUpdateSteadyStateAllocs(t *testing.T) {
	h := newHarness(t, fstest.MapFS{"a.txt": file("a")}, Options{})
	Load[text](h.srv.Global(), "a.txt")
	h.srv.Update()
	if n := testing.AllocsPerRun(100, func() { h.srv.Update() }); n != 0 {
		t.Fatalf("idle Update allocates %v times", n)
	}
}
