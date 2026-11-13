package asset

import (
	"testing"
	"testing/fstest"
)

// TestChangedReloadsWithoutPolling: with WatchInterval 0, Changed alone
// reloads a Ready asset in place.
func TestChangedReloadsWithoutPolling(t *testing.T) {
	files := fstest.MapFS{"a.txt": file("one")}
	h := newHarness(t, files, Options{})
	sc := h.srv.NewScope("s")
	a, err := LoadSync[text](sc, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	files["a.txt"] = file("two")
	h.srv.Update()
	if got := h.texts.Get(a).S; got != "one" {
		t.Fatalf("reloaded without Changed: %q", got)
	}

	h.srv.Changed("a.txt", "unused.txt")
	ev := h.srv.Update()
	for range 100 {
		if h.texts.Get(a).S == "two" {
			break
		}
		ev = h.srv.Update()
	}
	if got := h.texts.Get(a).S; got != "two" || h.texts.Version(a) != 2 {
		t.Fatalf("after Changed: %q version %d", got, h.texts.Version(a))
	}
	_ = ev
	if len(h.srv.changed) != 0 {
		t.Fatalf("paths left recorded: %v", h.srv.changed)
	}
	if again := Load[text](sc, "a.txt"); again != a {
		t.Fatal("reload changed the handle")
	}
}

// TestChangedRetriesFailed: a Failed asset loads again after Changed.
func TestChangedRetriesFailed(t *testing.T) {
	files := fstest.MapFS{"a.txt": file("bad")}
	h := newHarness(t, files, Options{})
	sc := h.srv.NewScope("s")
	a := Load[text](sc, "a.txt")
	h.settle(sc)
	if _, state := h.texts.Lookup(a); state != Failed {
		t.Fatalf("state = %v, want Failed", state)
	}
	files["a.txt"] = file("good")
	h.srv.Changed("a.txt")
	for range 100 {
		h.srv.Update()
		if _, state := h.texts.Lookup(a); state == Ready {
			break
		}
	}
	if v, state := h.texts.Lookup(a); state != Ready || v.S != "good" {
		t.Fatalf("after Changed: %v %v", v, state)
	}
}

// TestChangedWhileBusyIsKept: a change reported while the asset is still
// reloading is applied after that reload, not lost.
func TestChangedWhileBusyIsKept(t *testing.T) {
	files := fstest.MapFS{"a.txt": file("one")}
	h := newHarness(t, files, Options{Workers: 0})
	sc := h.srv.NewScope("s")
	a, err := LoadSync[text](sc, "a.txt")
	if err != nil {
		t.Fatal(err)
	}

	files["a.txt"] = file("two")
	h.srv.Changed("a.txt")
	h.srv.reloadChanged() // queue the reload of "two" without running it
	files["a.txt"] = file("three")
	h.srv.Changed("a.txt") // arrives while "two" is in flight
	for range 100 {
		h.srv.Update()
		if h.texts.Get(a).S == "three" {
			break
		}
	}
	if got := h.texts.Get(a).S; got != "three" {
		t.Fatalf("value = %q, want three (the later change)", got)
	}
	// The second report is acted on once the first reload settled (here it
	// reads "three" again, which is harmless), and then forgotten.
	for range 10 {
		h.srv.Update()
	}
	if got := h.texts.Get(a).S; got != "three" {
		t.Fatalf("value = %q, want three", got)
	}
	if len(h.srv.changed) != 0 {
		t.Fatalf("paths left recorded: %v", h.srv.changed)
	}
}

// TestChangedReloadsParentOnly: the path of a sheet reloads the sheet once;
// its sub-assets are republished by the parent, not reloaded themselves.
func TestChangedReloadsParentOnly(t *testing.T) {
	files := fstest.MapFS{"s.sheet": file("idle,run")}
	c := newChildHarness(t, files, Options{})
	sc := c.srv.NewScope("s")
	run := Load[frame](sc, "s.sheet#run")
	c.settle(sc)
	if c.sl.finishes != 1 {
		t.Fatalf("finishes = %d", c.sl.finishes)
	}

	files["s.sheet"] = file("idle,run,jump")
	c.srv.Changed("s.sheet")
	for range 10 {
		c.srv.Update()
	}
	if c.sl.finishes != 2 {
		t.Fatalf("sheet finished %d times, want 2 (one reload)", c.sl.finishes)
	}
	if got := c.frames.Get(run).Name; got != "run" {
		t.Fatalf("child after reload = %q", got)
	}
}

// TestChangedReloadsDependentsWithoutWatch: with WatchInterval 0 nothing
// keeps Decode results, so a dependent of a changed asset is read from its
// file again and finished with the new dependency.
func TestChangedReloadsDependentsWithoutWatch(t *testing.T) {
	files := fstest.MapFS{"top.bundle": file("bundle:mid.bundle"), "mid.bundle": file("text:t.txt"), "t.txt": file("one")}
	h := newHarness(t, files, Options{})
	bundles := Register(h.srv, Loader[bundle, bundleDecoded](&bundleLoader{}), bundle{})
	sc := h.srv.NewScope("s")
	top, err := LoadSync[bundle](sc, "top.bundle")
	if err != nil {
		t.Fatal(err)
	}
	mid := bundles.Get(top).Bundles[0]

	files["t.txt"] = file("two")
	h.srv.Changed("t.txt")
	for range 100 {
		h.srv.Update()
		if bundles.Version(top) == 2 {
			break
		}
	}
	if bundles.Version(top) != 2 || bundles.Version(mid) != 2 {
		t.Fatalf("versions: top %d, mid %d, want 2", bundles.Version(top), bundles.Version(mid))
	}
	if bundles.Get(top).Bundles[0] != mid || h.texts.Get(bundles.Get(mid).Texts[0]).S != "two" {
		t.Fatal("dependents do not see the new dependency")
	}
}
