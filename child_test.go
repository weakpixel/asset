package asset

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"
)

// sheet is a parent asset: its file lists frame names, comma-separated, and
// Finish publishes each as a frame sub-asset.
type sheet struct{ Frames map[string]Handle[frame] }

type frame struct{ Name string }

type sheetLoader struct {
	finishes int
	// extra is published by Finish in addition to the file's names.
	extra func(ctx *FinishContext)
}

func (l *sheetLoader) Decode(_ *DecodeContext, data []byte) ([]string, error) {
	if string(data) == "bad" {
		return nil, errCorrupt
	}
	return strings.Split(string(data), ","), nil
}

func (l *sheetLoader) Finish(ctx *FinishContext, names []string) (sheet, error) {
	l.finishes++
	s := sheet{Frames: map[string]Handle[frame]{}}
	for _, n := range names {
		s.Frames[n] = AddChild(ctx, n, frame{Name: n})
	}
	if l.extra != nil {
		l.extra(ctx)
	}
	return s, nil
}

func (l *sheetLoader) Unload(*sheet) {}

type frameLoader struct{ unloads int }

// A frame loaded without "#sub" is an ordinary file: its contents are its name.
func (frameLoader) Decode(_ *DecodeContext, data []byte) (string, error) { return string(data), nil }
func (frameLoader) Finish(_ *FinishContext, s string) (frame, error)     { return frame{Name: s}, nil }
func (l *frameLoader) Unload(*frame)                                     { l.unloads++ }

var errCorrupt = errors.New("corrupt")

type childHarness struct {
	*harness
	sheets *Store[sheet]
	frames *Store[frame]
	sl     *sheetLoader
	fl     *frameLoader
}

func newChildHarness(t *testing.T, files fstest.MapFS, opts Options) *childHarness {
	t.Helper()
	h := newHarness(t, files, opts)
	c := &childHarness{harness: h, sl: &sheetLoader{}, fl: &frameLoader{}}
	c.sheets = Register(h.srv, Loader[sheet, []string](c.sl), sheet{})
	c.frames = Register(h.srv, Loader[frame, string](c.fl), frame{Name: "placeholder"})
	RegisterChild[sheet, frame](h.srv)
	return c
}

func TestChildLoadedBeforeParent(t *testing.T) {
	for _, workers := range []int{0, 3} {
		c := newChildHarness(t, fstest.MapFS{"s.sheet": file("idle,run")}, Options{Workers: workers})
		sc := c.srv.NewScope("s")
		run := Load[frame](sc, "s.sheet#run")

		// Reserved: waiting, no work started on the frame itself.
		if _, st := c.frames.Lookup(run); st != Waiting {
			t.Fatalf("workers=%d: reserved child state = %v, want Waiting", workers, st)
		}
		if c.frames.Get(run).Name != "placeholder" {
			t.Fatal("reserved child is not the placeholder")
		}
		c.settle(sc)

		if c.frames.Get(run).Name != "run" {
			t.Fatalf("workers=%d: child = %v (err %v)", workers, c.frames.Get(run), c.frames.Err(run))
		}
		// The parent's own table holds the very same handle: the slot was
		// reserved, then filled.
		parent := Load[sheet](sc, "s.sheet")
		if c.sheets.Get(parent).Frames["run"] != run {
			t.Fatalf("workers=%d: AddChild handle differs from the reserved handle", workers)
		}
		if c.sl.finishes != 1 {
			t.Fatalf("parent finished %d times", c.sl.finishes)
		}
	}
}

func TestChildLoadedAfterParent(t *testing.T) {
	c := newChildHarness(t, fstest.MapFS{"s.sheet": file("idle,run")}, Options{})
	sc := c.srv.NewScope("s")
	parent := Load[sheet](sc, "s.sheet")
	c.srv.Update()

	idle := Load[frame](sc, "s.sheet#idle")
	if st := c.frames.stateAt(idle.index); st != Ready {
		t.Fatalf("child of a ready parent has state %v right after Load", st)
	}
	if idle != c.sheets.Get(parent).Frames["idle"] {
		t.Fatal("Load returned a different handle than AddChild")
	}
	if c.frames.Key(idle) != (Key{Path: "s.sheet", Sub: "idle"}) {
		t.Fatalf("Key = %v", c.frames.Key(idle))
	}
	if c.srv.Update().Len() != 0 {
		t.Fatal("loading an existing child produced events")
	}
}

func TestChildMissing(t *testing.T) {
	c := newChildHarness(t, fstest.MapFS{"s.sheet": file("idle")}, Options{})
	sc := c.srv.NewScope("s")

	// Before the parent is ready.
	early := Load[frame](sc, "s.sheet#nope")
	c.srv.Update()
	want := `asset: s.sheet has no frame "nope"`
	if err := c.frames.Err(early); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("early err = %v, want %q", err, want)
	}
	// After the parent is ready.
	late := Load[frame](sc, "s.sheet#also-nope")
	if err := c.frames.Err(late); err == nil || !strings.Contains(err.Error(), `has no frame "also-nope"`) {
		t.Fatalf("late err = %v", err)
	}
	// The parent itself is fine and so is its real child.
	if !c.sheets.Ready(Load[sheet](sc, "s.sheet")) || !c.frames.Ready(Load[frame](sc, "s.sheet#idle")) {
		t.Fatal("parent or real child not ready")
	}
}

func TestChildOfFailedParent(t *testing.T) {
	c := newChildHarness(t, fstest.MapFS{"bad.sheet": file("bad")}, Options{})
	sc := c.srv.NewScope("s")
	early := Load[frame](sc, "bad.sheet#x")
	c.srv.Update()
	if err := c.frames.Err(early); err == nil || !strings.Contains(err.Error(), "parent bad.sheet failed") {
		t.Fatalf("early err = %v", err)
	}
	late := Load[frame](sc, "bad.sheet#y")
	if err := c.frames.Err(late); err == nil || !strings.Contains(err.Error(), "parent bad.sheet failed") {
		t.Fatalf("late err = %v", err)
	}
}

func TestChildKeepsParentAliveAndGoesWithIt(t *testing.T) {
	c := newChildHarness(t, fstest.MapFS{"s.sheet": file("a,b")}, Options{})
	parentScope, childScope := c.srv.NewScope("parent"), c.srv.NewScope("child")
	parent := Load[sheet](parentScope, "s.sheet")
	c.srv.Update()
	a := c.sheets.Get(parent).Frames["a"]
	b := c.sheets.Get(parent).Frames["b"]
	Hold(childScope, a)

	// The parent scope lets go; a held child keeps the parent (and so its
	// sibling) alive.
	parentScope.Release()
	c.srv.Update()
	if !c.sheets.Ready(parent) || !c.frames.Ready(a) || !c.frames.Ready(b) {
		t.Fatal("parent unloaded while one of its children is held")
	}

	// Last hold goes: parent and all children are unloaded together.
	childScope.Release()
	ev := c.srv.Update()
	if c.sheets.Ready(parent) || c.frames.Ready(a) || c.frames.Ready(b) {
		t.Fatal("parent or children survived their last hold")
	}
	if ev.Len() != 3 {
		t.Fatalf("unload events = %d, want 3", ev.Len())
	}
	if c.fl.unloads != 2 {
		t.Fatalf("frame Unload called %d times, want 2", c.fl.unloads)
	}
	// And the handles are stale, not pointing at something else.
	if _, st := c.frames.Lookup(a); st != Missing {
		t.Fatalf("state of unloaded child = %v", st)
	}
}

func TestChildReleasedAloneUnloadsParent(t *testing.T) {
	c := newChildHarness(t, fstest.MapFS{"s.sheet": file("a")}, Options{})
	sc := c.srv.NewScope("s")
	a := Load[frame](sc, "s.sheet#a")
	c.srv.Update()
	sc.Release()
	c.srv.Update()
	if st := c.srv.Stats(); st.States[Ready]+st.States[Waiting]+st.States[Failed] != 0 {
		t.Fatalf("assets left: %v", st.States)
	}
	if c.frames.Ready(a) {
		t.Fatal("child alive")
	}
}

func TestChildProgressCountsParent(t *testing.T) {
	c := newChildHarness(t, fstest.MapFS{"s.sheet": file("a")}, Options{})
	sc := c.srv.NewScope("s")
	Load[frame](sc, "s.sheet#a")
	if ready, total := sc.Progress(); ready != 0 || total != 2 {
		t.Fatalf("Progress = %d/%d, want 0/2 (child and parent)", ready, total)
	}
	c.srv.Update()
	if ready, total := sc.Progress(); ready != 2 || total != 2 {
		t.Fatalf("Progress = %d/%d, want 2/2", ready, total)
	}
}

func TestChildWithoutSubIsTopLevel(t *testing.T) {
	c := newChildHarness(t, fstest.MapFS{"cursor.png": file("cursor")}, Options{})
	sc := c.srv.NewScope("s")
	f := Load[frame](sc, "cursor.png")
	c.srv.Update()
	if c.frames.Get(f).Name != "cursor" {
		t.Fatalf("top-level child type = %v (err %v)", c.frames.Get(f), c.frames.Err(f))
	}
}

func TestAddChildPanics(t *testing.T) {
	t.Run("not a registered child", func(t *testing.T) {
		c := newChildHarness(t, fstest.MapFS{"s.sheet": file("a")}, Options{})
		c.sl.extra = func(ctx *FinishContext) {
			mustPanic(t, "asset: AddChild: asset.text is not a registered child of asset.sheet", func() {
				AddChild(ctx, "x", text{})
			})
		}
		Load[sheet](c.srv.Global(), "s.sheet")
		c.srv.Update()
		if c.sl.finishes != 1 {
			t.Fatal("Finish did not run")
		}
	})
	t.Run("empty and duplicate", func(t *testing.T) {
		c := newChildHarness(t, fstest.MapFS{"s.sheet": file("a")}, Options{})
		c.sl.extra = func(ctx *FinishContext) {
			mustPanic(t, "asset: AddChild: empty sub name", func() { AddChild(ctx, "", frame{}) })
			mustPanic(t, "asset: AddChild: asset.frame s.sheet#a was already added", func() { AddChild(ctx, "a", frame{}) })
		}
		Load[sheet](c.srv.Global(), "s.sheet")
		c.srv.Update()
		if c.sl.finishes != 1 {
			t.Fatal("Finish did not run")
		}
	})
}

func TestChildAddedWithoutReservation(t *testing.T) {
	c := newChildHarness(t, fstest.MapFS{"s.sheet": file("a")}, Options{})
	var extra Handle[frame]
	c.sl.extra = func(ctx *FinishContext) { extra = AddChild(ctx, "bonus", frame{Name: "bonus"}) }
	sc := c.srv.NewScope("s")
	Load[sheet](sc, "s.sheet")
	c.srv.Update()
	if c.frames.Get(extra).Name != "bonus" {
		t.Fatal("unreserved child not readable")
	}
	// It is a child like any other: loadable by key, gone with the parent.
	if Load[frame](sc, "s.sheet#bonus") != extra {
		t.Fatal("Load of an added child returns another handle")
	}
	sc.Release()
	c.srv.Update()
	if c.frames.Ready(extra) {
		t.Fatal("child survived its parent")
	}
}
