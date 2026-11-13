package asset

import (
	"strings"
	"testing"
	"testing/fstest"
)

// ten is a file of 10 bytes, the unit of the keep-alive budgets below.
var ten = strings.Repeat("x", 10)

// blob has a loader with a Size method; its size is twice the file length.
type blob struct{ n int }

type blobLoader struct{}

func (blobLoader) Decode(_ *DecodeContext, data []byte) (int, error) { return len(data), nil }
func (blobLoader) Finish(_ *FinishContext, n int) (blob, error)      { return blob{n}, nil }
func (blobLoader) Unload(*blob)                                      {}
func (blobLoader) Size(v *blob) int64                                { return int64(v.n) * 2 }

func TestBytes(t *testing.T) {
	h, _, _ := newBundleHarness(t, fstest.MapFS{"a.txt": file(ten), "b.blob": file(ten), "set.bundle": file("text:a.txt")}, Options{})
	Register(h.srv, Loader[blob, int](blobLoader{}), blob{})
	sc := h.srv.NewScope("s")
	Load[bundle](sc, "set.bundle")
	Load[blob](sc, "b.blob")
	h.settle(sc)

	st := h.srv.Stats()
	want := map[string]int64{"asset.text": 10, "asset.bundle": int64(len("text:a.txt")), "asset.blob": 20}
	for _, ts := range st.Types {
		if w, ok := want[ts.Name]; ok && ts.Bytes != w {
			t.Errorf("%s bytes = %d, want %d", ts.Name, ts.Bytes, w)
		}
		if ts.IdleBytes != 0 {
			t.Errorf("%s idle = %d with everything held", ts.Name, ts.IdleBytes)
		}
	}
	if st.Bytes != 10+int64(len("text:a.txt"))+20 {
		t.Fatalf("total bytes = %d", st.Bytes)
	}
}

func TestBytesChildrenCountZero(t *testing.T) {
	h := newChildHarness(t, fstest.MapFS{"s.sheet": file("a,b,c")}, Options{})
	sc := h.srv.NewScope("s")
	Load[frame](sc, "s.sheet#a")
	h.settle(sc)
	for _, ts := range h.srv.Stats().Types {
		if ts.Name == "asset.frame" && ts.Bytes != 0 {
			t.Fatalf("frame bytes = %d, want 0", ts.Bytes)
		}
	}
	if st := h.srv.Stats(); st.Bytes != 5 {
		t.Fatalf("total = %d, want the 5 byte sheet file", st.Bytes)
	}
}

func TestBytesReload(t *testing.T) {
	fsys := fstest.MapFS{"a.txt": file(ten)}
	h := newHarness(t, fsys, Options{})
	sc := h.srv.NewScope("s")
	Load[text](sc, "a.txt")
	h.settle(sc)
	fsys["a.txt"] = file(ten + ten)
	h.srv.Changed("a.txt")
	h.srv.Update()
	if got := h.srv.Stats().Bytes; got != 20 {
		t.Fatalf("bytes after reload = %d, want 20", got)
	}
}

func TestKeepAliveRevive(t *testing.T) {
	h := newHarness(t, fstest.MapFS{"a.txt": file(ten)}, Options{KeepAlive: 100})
	s1 := h.srv.NewScope("one")
	a := Load[text](s1, "a.txt")
	h.settle(s1)
	s1.Release()
	ev := h.srv.Update()
	if len(kinds(ev)) != 0 {
		t.Fatalf("parking emitted %v", kinds(ev))
	}
	if !h.texts.Ready(a) {
		t.Fatal("old handle stopped resolving while parked")
	}
	if st := h.srv.Stats(); st.Parked != 1 || st.IdleBytes != 10 {
		t.Fatalf("stats = parked %d idle %d", st.Parked, st.IdleBytes)
	}

	s2 := h.srv.NewScope("two")
	b := Load[text](s2, "a.txt")
	ev = h.srv.Update()
	if a != b {
		t.Fatalf("handle changed: %v -> %v", a, b)
	}
	if h.loader.decodes.Load() != 1 || h.loader.finishes.Load() != 1 || len(kinds(ev)) != 0 {
		t.Fatalf("revive reloaded: decodes %d finishes %d events %v", h.loader.decodes.Load(), h.loader.finishes.Load(), kinds(ev))
	}
	if st := h.srv.Stats(); st.Parked != 0 || st.IdleBytes != 0 {
		t.Fatalf("stats after revive = parked %d idle %d", st.Parked, st.IdleBytes)
	}
}

func TestKeepAliveOff(t *testing.T) {
	h := newHarness(t, fstest.MapFS{"a.txt": file(ten)}, Options{})
	sc := h.srv.NewScope("s")
	a := Load[text](sc, "a.txt")
	h.settle(sc)
	sc.Release()
	ev := h.srv.Update()
	if h.texts.Ready(a) || !equalKinds(kinds(ev), []EventKind{EventUnloaded}) {
		t.Fatalf("not unloaded: events %v", kinds(ev))
	}
}

func TestKeepAliveFailedNotParked(t *testing.T) {
	h := newHarness(t, fstest.MapFS{"bad.txt": file("bad"), "slow.txt": file(ten)}, Options{KeepAlive: 100})
	sc := h.srv.NewScope("s")
	bad := Load[text](sc, "bad.txt")
	h.settle(sc)
	Load[text](sc, "slow.txt") // released before it finished loading
	sc.Release()
	h.srv.Update()
	if _, st := h.texts.Lookup(bad); st != Missing {
		t.Fatalf("failed asset state = %v, want freed", st)
	}
	if h.texts.Len() != 0 || h.srv.Stats().Parked != 0 {
		t.Fatalf("len %d parked %d", h.texts.Len(), h.srv.Stats().Parked)
	}
}

// loadRelease loads key in its own scope, waits for it and releases the scope.
func loadRelease(h *harness, key string) Handle[text] {
	sc := h.srv.NewScope(key)
	a := Load[text](sc, key)
	h.settle(sc)
	sc.Release()
	h.srv.Update()
	return a
}

func TestKeepAliveEvictsOldest(t *testing.T) {
	h := newHarness(t, fstest.MapFS{"a.txt": file(ten), "b.txt": file(ten), "c.txt": file(ten)}, Options{KeepAlive: 20})
	a, b := loadRelease(h, "a.txt"), loadRelease(h, "b.txt")
	if !h.texts.Ready(a) || !h.texts.Ready(b) {
		t.Fatal("within budget but unloaded")
	}
	sc := h.srv.NewScope("c")
	Load[text](sc, "c.txt")
	h.settle(sc)
	sc.Release()
	ev := h.srv.Update()
	if h.texts.Ready(a) || !h.texts.Ready(b) {
		t.Fatalf("a ready %v, b ready %v; want only b", h.texts.Ready(a), h.texts.Ready(b))
	}
	if !equalKinds(kinds(ev), []EventKind{EventUnloaded}) {
		t.Fatalf("events = %v", kinds(ev))
	}
	if st := h.srv.Stats(); st.IdleBytes > 20 || st.Parked != 2 {
		t.Fatalf("idle %d parked %d", st.IdleBytes, st.Parked)
	}
}

func TestKeepAliveRevivedIsNewest(t *testing.T) {
	h := newHarness(t, fstest.MapFS{"a.txt": file(ten), "b.txt": file(ten), "c.txt": file(ten)}, Options{KeepAlive: 20})
	a, b := loadRelease(h, "a.txt"), loadRelease(h, "b.txt")
	loadRelease(h, "a.txt") // a is the most recently released now
	loadRelease(h, "c.txt")
	if !h.texts.Ready(a) || h.texts.Ready(b) {
		t.Fatalf("a ready %v, b ready %v; want only a", h.texts.Ready(a), h.texts.Ready(b))
	}
}

func TestKeepAliveCountsDeps(t *testing.T) {
	files := fstest.MapFS{
		"old.bundle": file("text:old.txt"), "old.txt": file(ten),
		"new.txt": file(ten), "newer.txt": file(ten),
	}
	h, bs, _ := newBundleHarness(t, files, Options{KeepAlive: 22})
	sc := h.srv.NewScope("old")
	oldB := Load[bundle](sc, "old.bundle")
	h.settle(sc)
	oldT := bs.slots[oldB.index].val.Texts[0]
	sc.Release()
	h.srv.Update()
	// The bundle file is 12 bytes and its text dependency 10: 22 idle bytes.
	if st := h.srv.Stats(); st.IdleBytes != 22 || st.Parked != 1 {
		t.Fatalf("idle %d parked %d, want 22 and 1", st.IdleBytes, st.Parked)
	}
	if !h.texts.Ready(oldT) {
		t.Fatal("dependency of a parked asset was unloaded")
	}

	loadRelease(h, "new.txt") // 32 idle: the old bundle goes, its texture is parked in front
	if bs.Ready(oldB) {
		t.Fatal("oldest bundle survived")
	}
	if !h.texts.Ready(oldT) {
		t.Fatal("dependency evicted together with the parent: it should be parked in front")
	}
	if st := h.srv.Stats(); st.IdleBytes != 20 || st.Parked != 2 {
		t.Fatalf("idle %d parked %d, want 20 and 2", st.IdleBytes, st.Parked)
	}
	loadRelease(h, "newer.txt") // 30 idle: the inherited age goes first
	if h.texts.Ready(oldT) {
		t.Fatal("old dependency outlived newer assets")
	}
}

func TestKeepAliveSharedDep(t *testing.T) {
	files := fstest.MapFS{"set.bundle": file("text:t.txt"), "t.txt": file(ten)}
	h, _, _ := newBundleHarness(t, files, Options{KeepAlive: 1000})
	live := h.srv.NewScope("live")
	tx := Load[text](live, "t.txt")
	sc := h.srv.NewScope("sc")
	Load[bundle](sc, "set.bundle")
	h.settle(sc)
	sc.Release()
	h.srv.Update()
	// Only the bundle is idle: the text is held by a live scope.
	if st := h.srv.Stats(); st.IdleBytes != int64(len("text:t.txt")) {
		t.Fatalf("idle = %d, want the bundle file only", st.IdleBytes)
	}
	live.Release()
	h.srv.Update()
	if !h.texts.Ready(tx) {
		t.Fatal("text held by the parked bundle was unloaded")
	}
	if st := h.srv.Stats(); st.IdleBytes != int64(len("text:t.txt"))+10 {
		t.Fatalf("idle = %d", st.IdleBytes)
	}
}

func TestKeepAliveTooLarge(t *testing.T) {
	h := newHarness(t, fstest.MapFS{"a.txt": file(ten)}, Options{KeepAlive: 5})
	a := loadRelease(h, "a.txt")
	if h.texts.Ready(a) || h.texts.Len() != 0 {
		t.Fatal("asset above the budget was kept")
	}
	if h.srv.Stats().Parked != 0 {
		t.Fatal("parked count not reset")
	}
}

func TestKeepAliveChildren(t *testing.T) {
	h := newChildHarness(t, fstest.MapFS{"s.sheet": file("a,b")}, Options{KeepAlive: 100})
	s1 := h.srv.NewScope("one")
	fa := Load[frame](s1, "s.sheet#a")
	h.settle(s1)
	s1.Release()
	h.srv.Update()
	if !h.frames.Ready(fa) {
		t.Fatal("sprite handle stopped resolving while its atlas is parked")
	}
	if h.srv.Stats().Parked != 1 {
		t.Fatalf("parked = %d, want only the sheet", h.srv.Stats().Parked)
	}

	s2 := h.srv.NewScope("two")
	fb := Load[frame](s2, "s.sheet#b")
	h.srv.Update()
	if h.sl.finishes != 1 || !h.frames.Ready(fb) {
		t.Fatalf("finishes %d, b ready %v", h.sl.finishes, h.frames.Ready(fb))
	}
	if h.srv.Stats().Parked != 0 {
		t.Fatal("a held sprite must keep the sheet from being parked")
	}
	s2.Release()
	h.srv.Update()
	if h.srv.Stats().Parked != 1 || !h.frames.Ready(fa) {
		t.Fatal("sheet not parked again")
	}
}

func TestKeepAliveReload(t *testing.T) {
	fsys := fstest.MapFS{"a.txt": file(ten)}
	h := newHarness(t, fsys, Options{KeepAlive: 100})
	a := loadRelease(h, "a.txt")
	fsys["a.txt"] = file(ten + ten)
	h.srv.Changed("a.txt")
	ev := h.srv.Update()
	if !equalKinds(kinds(ev), []EventKind{EventModified}) || h.texts.Get(a).S != ten+ten {
		t.Fatalf("parked asset not reloaded: events %v", kinds(ev))
	}
	if st := h.srv.Stats(); st.IdleBytes != 20 || st.Parked != 1 {
		t.Fatalf("idle %d parked %d", st.IdleBytes, st.Parked)
	}
}

func TestKeepAliveClose(t *testing.T) {
	h := newHarness(t, fstest.MapFS{"a.txt": file(ten), "b.txt": file(ten)}, Options{KeepAlive: 100})
	loadRelease(h, "a.txt")
	loadRelease(h, "b.txt")
	h.srv.Close()
	if got := h.loader.unloads.Load(); got != 2 {
		t.Fatalf("Unload ran %d times, want 2", got)
	}
	if h.srv.Stats().Parked != 0 {
		t.Fatal("parked count not cleared")
	}
}

func TestKeepAliveAllocs(t *testing.T) {
	h := newHarness(t, fstest.MapFS{"a.txt": file(ten)}, Options{KeepAlive: 100})
	sc := h.srv.NewScope("s")
	Load[text](sc, "a.txt")
	h.settle(sc)
	cycle := func() {
		sc.Release()
		h.srv.Update()
		Load[text](sc, "a.txt")
		h.srv.Update()
	}
	cycle() // warm the LRU and the mark stack
	if n := testing.AllocsPerRun(50, cycle); n != 0 {
		t.Fatalf("release, Update, Load = %v allocs, want 0", n)
	}
}
