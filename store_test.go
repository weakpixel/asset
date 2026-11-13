package asset

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestStoreGetStates(t *testing.T) {
	h := newHarness(t, fstest.MapFS{"a.txt": file("alpha"), "bad.txt": file("bad")}, Options{})
	sc := h.srv.NewScope("t")

	a := Load[text](sc, "a.txt")
	bad := Load[text](sc, "bad.txt")

	// Queued: placeholder, state Queued, not an error.
	if v, st := h.texts.Lookup(a); v.S != "placeholder" || st != Queued {
		t.Fatalf("before Update: %v %v", v, st)
	}
	h.srv.Update()

	if v, st := h.texts.Lookup(a); v.S != "alpha" || st != Ready {
		t.Fatalf("ready: %v %v", v, st)
	}
	if got := h.texts.Get(a).S; got != "alpha" {
		t.Fatalf("Get = %q", got)
	}
	if !h.texts.Ready(a) || h.texts.Err(a) != nil {
		t.Fatal("Ready/Err wrong for ready asset")
	}
	if v, st := h.texts.Lookup(bad); v.S != "placeholder" || st != Failed {
		t.Fatalf("failed: %v %v", v, st)
	}
	if h.texts.Err(bad) == nil || h.texts.Ready(bad) {
		t.Fatal("Err/Ready wrong for failed asset")
	}
	if got := h.texts.Key(a); got != (Key{Path: "a.txt"}) {
		t.Fatalf("Key = %+v", got)
	}
	if h.texts.Version(a) != 1 || h.texts.Version(bad) != 0 {
		t.Fatalf("Version = %d, %d", h.texts.Version(a), h.texts.Version(bad))
	}
	if h.texts.Len() != 2 {
		t.Fatalf("Len = %d", h.texts.Len())
	}

	var zero Handle[text]
	if v, st := h.texts.Lookup(zero); v.S != "placeholder" || st != Missing {
		t.Fatalf("zero handle: %v %v", v, st)
	}
	if h.texts.Get(zero).S != "placeholder" {
		t.Fatal("Get(zero) is not the placeholder")
	}
}

func TestStoreAll(t *testing.T) {
	h := newHarness(t, fstest.MapFS{"a.txt": file("a"), "b.txt": file("b"), "bad.txt": file("bad")}, Options{})
	sc := h.srv.NewScope("t")
	Load[text](sc, "a.txt")
	Load[text](sc, "b.txt")
	Load[text](sc, "bad.txt")
	h.srv.Update()

	got := map[string]bool{}
	for hd, v := range h.texts.All() {
		if h.texts.Get(hd) != v {
			t.Fatal("All pointer differs from Get")
		}
		got[v.S] = true
	}
	if len(got) != 2 || !got["a"] || !got["b"] {
		t.Fatalf("All yielded %v, want only the ready a and b", got)
	}
}

func TestStoreGetZeroAllocs(t *testing.T) {
	h := newHarness(t, fstest.MapFS{"a.txt": file("alpha")}, Options{})
	sc := h.srv.NewScope("t")
	a := Load[text](sc, "a.txt")
	queued := Load[text](sc, "b.txt")
	var zero Handle[text]
	h.srv.Update()

	for name, hd := range map[string]Handle[text]{"ready": a, "failed or missing file": queued, "zero": zero} {
		if n := testing.AllocsPerRun(100, func() { _ = h.texts.Get(hd) }); n != 0 {
			t.Errorf("Get(%s) allocates %v times per call", name, n)
		}
	}
}

func TestStaleHandleRelease(t *testing.T) {
	h := newHarness(t, fstest.MapFS{"a.txt": file("alpha")}, Options{})
	sc := h.srv.NewScope("level")
	a := Load[text](sc, "a.txt")
	h.srv.Update()
	sc.Release()
	h.srv.Update()

	if v, st := h.texts.Lookup(a); v.S != "placeholder" || st != Missing {
		t.Fatalf("stale handle: %v %v", v, st)
	}
	if h.texts.Get(a).S != "placeholder" {
		t.Fatal("stale Get is not the placeholder")
	}
	if h.srv.Stats().StaleHits == 0 {
		t.Fatal("stale hits not counted")
	}
	if h.texts.Err(a) != nil || h.texts.Key(a) != (Key{}) || h.texts.Version(a) != 0 {
		t.Fatal("stale handle leaks data")
	}
}

func TestStaleHandleStrict(t *testing.T) {
	h := newHarness(t, fstest.MapFS{"a.txt": file("alpha")}, Options{StrictHandles: true})
	sc := h.srv.NewScope("level")
	a := Load[text](sc, "a.txt")
	h.srv.Update()
	sc.Release()
	h.srv.Update()

	mustPanic(t, "asset: stale handle: ", func() { h.texts.Get(a) })
	mustPanic(t, "a.txt", func() { h.texts.Lookup(a) })

	// Not stale in strict mode: zero handle, and not-ready-yet handles.
	var zero Handle[text]
	if h.texts.Get(zero).S != "placeholder" {
		t.Fatal("zero handle is not the placeholder")
	}
	sc2 := h.srv.NewScope("next")
	b := Load[text](sc2, "b.txt")
	if _, st := h.texts.Lookup(b); st != Queued {
		t.Fatalf("queued handle state = %v", st)
	}
}

func TestSlotReuseBumpsGeneration(t *testing.T) {
	h := newHarness(t, fstest.MapFS{"a.txt": file("alpha"), "b.txt": file("beta")}, Options{})
	sc := h.srv.NewScope("level")
	a := Load[text](sc, "a.txt")
	h.srv.Update()
	sc.Release()
	h.srv.Update()

	b := Load[text](h.srv.Global(), "b.txt")
	h.srv.Update()

	if a.index != b.index {
		t.Fatalf("expected slot reuse, got slots %d and %d", a.index, b.index)
	}
	if a.gen == b.gen {
		t.Fatal("reused slot kept its generation")
	}
	if h.texts.Get(a).S != "placeholder" {
		t.Fatal("old handle resolves to the new occupant of its slot")
	}
	if h.texts.Get(b).S != "beta" {
		t.Fatal("new handle does not resolve")
	}
	if strings.Contains(h.texts.Key(b).Path, "a.txt") {
		t.Fatal("key of reused slot is stale")
	}
}

func TestStoreID(t *testing.T) {
	h := newHarness(t, fstest.MapFS{"a.txt": file("alpha")}, Options{})
	a := Load[text](h.srv.Global(), "a.txt")
	id := h.texts.ID(a)
	if id.Type != TypeIDOf[text](h.srv) || id.Index != a.index || id.Gen != a.gen {
		t.Fatalf("ID = %+v", id)
	}
}
