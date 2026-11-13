package asset

import (
	"errors"
	"io/fs"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"
)

// watchHarness is a harness with hot reload on, a hand-driven clock and a file
// system tests edit between Updates.
type watchHarness struct {
	*harness
	files fstest.MapFS
	clock *fakeClock
	mod   time.Time
}

const watchEvery = time.Second

func newWatchHarness(t *testing.T, files fstest.MapFS, opts Options) *watchHarness {
	t.Helper()
	if opts.WatchInterval == 0 {
		opts.WatchInterval = watchEvery
	}
	h := newHarness(t, files, opts)
	clock := &fakeClock{t: time.Unix(1000, 0)}
	h.srv.now = clock.now
	return &watchHarness{harness: h, files: files, clock: clock, mod: time.Unix(1, 0)}
}

// write replaces a file with a newer modtime, as an editor saving it does.
func (w *watchHarness) write(name, data string) {
	w.mod = w.mod.Add(time.Second)
	w.files[name] = &fstest.MapFile{Data: []byte(data), ModTime: w.mod}
}

// tick advances the clock by one watch interval and runs Update.
func (w *watchHarness) tick() *Events {
	w.clock.advance(watchEvery)
	return w.srv.Update()
}

// errsContaining counts reported errors that contain want.
func (w *watchHarness) errsContaining(want string) int {
	n := 0
	for _, err := range w.errs {
		if strings.Contains(err.Error(), want) {
			n++
		}
	}
	return n
}

func eventsFor(ev *Events, k EventKind) []string {
	var out []string
	for e := range ev.All() {
		if e.Kind == k {
			out = append(out, e.Key.String())
		}
	}
	return out
}

func TestReloadSwapsInPlace(t *testing.T) {
	w := newWatchHarness(t, fstest.MapFS{}, Options{})
	w.write("a.txt", "one")
	sc := w.srv.NewScope("s")
	a, err := LoadSync[text](sc, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	w.srv.Update() // first poll: the file is as loaded
	if v := w.texts.Version(a); v != 1 {
		t.Fatalf("version after load = %d", v)
	}

	w.write("a.txt", "two")
	ev := w.tick()
	if got := w.texts.Get(a).S; got != "two" {
		t.Fatalf("value after reload = %q, want two", got)
	}
	if v := w.texts.Version(a); v != 2 {
		t.Fatalf("version after reload = %d, want 2", v)
	}
	if got := eventsFor(ev, EventModified); len(got) != 1 || got[0] != "a.txt" {
		t.Fatalf("modified events = %v", got)
	}
	if n := w.loader.unloads.Load(); n != 1 {
		t.Fatalf("old value unloaded %d times, want 1", n)
	}
	if again := Load[text](sc, "a.txt"); again != a {
		t.Fatal("reload changed the handle")
	}

	// Unchanged file: no more work.
	decodes := w.loader.decodes.Load()
	w.tick()
	if w.loader.decodes.Load() != decodes {
		t.Fatal("unchanged file was decoded again")
	}
}

func TestReloadPollsOncePerInterval(t *testing.T) {
	w := newWatchHarness(t, fstest.MapFS{}, Options{})
	w.write("a.txt", "one")
	sc := w.srv.NewScope("s")
	a, _ := LoadSync[text](sc, "a.txt")
	w.srv.Update() // polls now

	w.write("a.txt", "two")
	w.clock.advance(watchEvery / 2)
	w.srv.Update()
	if w.texts.Get(a).S != "one" {
		t.Fatal("reloaded before the interval elapsed")
	}
	w.clock.advance(watchEvery / 2)
	w.srv.Update()
	if w.texts.Get(a).S != "two" {
		t.Fatal("not reloaded after the interval elapsed")
	}
}

func TestReloadFailureKeepsOldValue(t *testing.T) {
	w := newWatchHarness(t, fstest.MapFS{}, Options{})
	w.write("a.txt", "one")
	sc := w.srv.NewScope("s")
	a, _ := LoadSync[text](sc, "a.txt")
	w.srv.Update()

	w.write("a.txt", "bad")
	ev := w.tick()
	w.tick()
	w.tick()
	if w.texts.Get(a).S != "one" || !w.texts.Ready(a) || w.texts.Version(a) != 1 {
		t.Fatalf("after a bad reload: %q ready=%v version=%d", w.texts.Get(a).S, w.texts.Ready(a), w.texts.Version(a))
	}
	if n := w.errsContaining("asset: reload failed, keeping previous version: a.txt: corrupt"); n != 1 {
		t.Fatalf("reload failure reported %d times, want once: %v", n, w.errs)
	}
	if ev.Len() != 0 {
		t.Fatalf("a failed reload emitted events: %v", kinds(ev))
	}
	if w.loader.unloads.Load() != 0 {
		t.Fatal("old value was unloaded")
	}

	// A finish error is a failed reload too, reported once for that version.
	w.write("a.txt", "badfinish")
	w.tick()
	w.tick()
	if n := w.errsContaining("keeping previous version: a.txt: finish failed"); n != 1 || w.texts.Get(a).S != "one" {
		t.Fatalf("finish failure: reported %d times, value %q", n, w.texts.Get(a).S)
	}

	w.write("a.txt", "fixed")
	w.tick()
	if w.texts.Get(a).S != "fixed" || w.texts.Version(a) != 2 {
		t.Fatalf("after the fix: %q version %d", w.texts.Get(a).S, w.texts.Version(a))
	}
}

func TestReloadMissingFileCountsAsUnchanged(t *testing.T) {
	w := newWatchHarness(t, fstest.MapFS{}, Options{})
	w.write("a.txt", "one")
	sc := w.srv.NewScope("s")
	a, _ := LoadSync[text](sc, "a.txt")
	w.srv.Update()

	delete(w.files, "a.txt")
	w.tick()
	if !w.texts.Ready(a) || w.texts.Get(a).S != "one" || len(w.errs) != 0 {
		t.Fatalf("missing file changed the asset: %q, errs %v", w.texts.Get(a).S, w.errs)
	}
	w.write("a.txt", "two")
	w.tick()
	if w.texts.Get(a).S != "two" {
		t.Fatal("not reloaded after the file came back")
	}
}

// countingFS counts Open calls. It has no Stat or ReadFile method, so every
// fs.Stat and fs.ReadFile goes through Open.
type countingFS struct {
	fs    fs.FS
	opens atomic.Int32
}

func (c *countingFS) Open(name string) (fs.File, error) {
	c.opens.Add(1)
	return c.fs.Open(name)
}

func TestWatchOffMakesNoFileCalls(t *testing.T) {
	cfs := &countingFS{fs: fstest.MapFS{"a.txt": file("a")}}
	srv := NewServer(Options{FS: cfs})
	t.Cleanup(srv.Close)
	Register(srv, Loader[text, string](&textLoader{}), text{})
	if _, err := LoadSync[text](srv.Global(), "a.txt"); err != nil {
		t.Fatal(err)
	}
	opens := cfs.opens.Load()
	for range 10 {
		srv.Update()
	}
	if cfs.opens.Load() != opens {
		t.Fatalf("Update opened files %d times with WatchInterval 0", cfs.opens.Load()-opens)
	}
}

func TestWatchStatsOncePerInterval(t *testing.T) {
	cfs := &countingFS{fs: fstest.MapFS{"a.txt": file("a"), "b.txt": file("b")}}
	srv := NewServer(Options{FS: cfs, WatchInterval: watchEvery})
	t.Cleanup(srv.Close)
	clock := &fakeClock{t: time.Unix(1000, 0)}
	srv.now = clock.now
	Register(srv, Loader[text, string](&textLoader{}), text{})
	LoadSync[text](srv.Global(), "a.txt")
	LoadSync[text](srv.Global(), "b.txt")
	srv.Update()

	opens := cfs.opens.Load()
	for range 10 {
		clock.advance(watchEvery / 10)
		srv.Update()
	}
	if n := cfs.opens.Load() - opens; n != 2 {
		t.Fatalf("stat calls in one interval = %d, want 2 (one per file)", n)
	}
}

func TestReloadRecoversFailedAsset(t *testing.T) {
	w := newWatchHarness(t, fstest.MapFS{}, Options{})
	w.write("a.txt", "bad")
	sc := w.srv.NewScope("s")
	a, err := LoadSync[text](sc, "a.txt")
	if err == nil {
		t.Fatal("bad file loaded")
	}
	w.srv.Update()
	w.tick() // unchanged: no retry
	if w.loader.decodes.Load() != 1 {
		t.Fatalf("failed asset decoded %d times without a change", w.loader.decodes.Load())
	}

	w.write("a.txt", "good")
	ev := w.tick()
	if !w.texts.Ready(a) || w.texts.Get(a).S != "good" || w.texts.Err(a) != nil {
		t.Fatalf("not recovered: %q, err %v", w.texts.Get(a).S, w.texts.Err(a))
	}
	if got := eventsFor(ev, EventLoaded); len(got) != 1 {
		t.Fatalf("loaded events = %v", got)
	}
	if v := w.texts.Version(a); v != 1 {
		t.Fatalf("version = %d, want 1 (first commit)", v)
	}
}

func TestReloadMissingFileRecovers(t *testing.T) {
	w := newWatchHarness(t, fstest.MapFS{}, Options{})
	sc := w.srv.NewScope("s")
	a, err := LoadSync[text](sc, "a.txt")
	if err == nil {
		t.Fatal("missing file loaded")
	}
	w.write("a.txt", "here")
	w.tick()
	if w.texts.Get(a).S != "here" {
		t.Fatalf("file that appeared was not loaded: %v", w.texts.Err(a))
	}
}

func TestReloadRefinishesDependents(t *testing.T) {
	w := newWatchHarness(t, fstest.MapFS{}, Options{})
	bl := &bundleLoader{}
	bundles := Register(w.srv, Loader[bundle, bundleDecoded](bl), bundle{})
	w.write("top.bundle", "bundle:mid.bundle")
	w.write("mid.bundle", "text:t.txt")
	w.write("t.txt", "one")
	sc := w.srv.NewScope("s")
	top, err := LoadSync[bundle](sc, "top.bundle")
	if err != nil {
		t.Fatal(err)
	}
	w.srv.Update()
	finishes := bl.finishes.Load()
	mid := bundles.Get(top).Bundles[0]

	w.write("t.txt", "two")
	ev := w.tick()
	if got := bl.finishes.Load() - finishes; got != 2 {
		t.Fatalf("dependents finished %d times, want 2 (mid, then top)", got)
	}
	if got := strings.Join(eventsFor(ev, EventModified), " "); got != "t.txt mid.bundle top.bundle" {
		t.Fatalf("modified = %q", got)
	}
	if bundles.Version(top) != 2 || bundles.Version(mid) != 2 {
		t.Fatalf("versions: top %d, mid %d", bundles.Version(top), bundles.Version(mid))
	}
	if bundles.Get(top).Bundles[0] != mid || w.texts.Get(bundles.Get(mid).Texts[0]).S != "two" {
		t.Fatal("dependents do not see the new dependency")
	}
}

func TestReloadChangesDependencies(t *testing.T) {
	w := newWatchHarness(t, fstest.MapFS{}, Options{})
	bundles := Register(w.srv, Loader[bundle, bundleDecoded](&bundleLoader{}), bundle{})
	w.write("b.bundle", "text:old.txt")
	w.write("old.txt", "old")
	w.write("new.txt", "new")
	sc := w.srv.NewScope("s")
	b, _ := LoadSync[bundle](sc, "b.bundle")
	oldH := bundles.Get(b).Texts[0]
	w.srv.Update()

	w.write("b.bundle", "text:new.txt")
	w.tick()
	w.srv.Update() // the old dependency, now unheld, unloads
	if got := w.texts.Get(bundles.Get(b).Texts[0]).S; got != "new" {
		t.Fatalf("dependency after reload = %q", got)
	}
	if _, st := w.texts.Lookup(oldH); st != Missing {
		t.Fatalf("dropped dependency state = %v, want unloaded", st)
	}

	// The new dependency is watched through the new edge.
	w.write("new.txt", "newer")
	w.tick()
	if v := bundles.Version(b); v != 3 {
		t.Fatalf("bundle version = %d, want 3", v)
	}
}

func TestReloadFailedDependencyKeepsOld(t *testing.T) {
	w := newWatchHarness(t, fstest.MapFS{}, Options{})
	bundles := Register(w.srv, Loader[bundle, bundleDecoded](&bundleLoader{}), bundle{})
	w.write("b.bundle", "text:a.txt")
	w.write("a.txt", "a")
	w.write("broken.txt", "bad")
	sc := w.srv.NewScope("s")
	b, _ := LoadSync[bundle](sc, "b.bundle")
	w.srv.Update()

	w.write("b.bundle", "text:broken.txt")
	w.tick()
	w.tick()
	if got := w.texts.Get(bundles.Get(b).Texts[0]).S; got != "a" || bundles.Version(b) != 1 {
		t.Fatalf("bundle changed after a failed reload: %q version %d", got, bundles.Version(b))
	}
	if n := w.errsContaining("keeping previous version: b.bundle: dependency"); n != 1 {
		t.Fatalf("dependency failure reported %d times: %v", n, w.errs)
	}
}

func TestReloadRetriesDependencyFailures(t *testing.T) {
	w := newWatchHarness(t, fstest.MapFS{}, Options{})
	bundles := Register(w.srv, Loader[bundle, bundleDecoded](&bundleLoader{}), bundle{})
	w.write("b.bundle", "text:a.txt")
	w.write("a.txt", "bad")
	sc := w.srv.NewScope("s")
	b, err := LoadSync[bundle](sc, "b.bundle")
	if err == nil {
		t.Fatal("bundle with a bad dependency loaded")
	}
	w.srv.Update()

	w.write("a.txt", "good")
	for range 3 {
		w.tick()
	}
	if !bundles.Ready(b) {
		t.Fatalf("bundle did not recover with its dependency: %v", bundles.Err(b))
	}
	if got := w.texts.Get(bundles.Get(b).Texts[0]).S; got != "good" {
		t.Fatalf("recovered bundle sees %q", got)
	}
}

// cut is a parent asset whose sub-assets are derived from a dependency, the
// way an atlas cuts sprites from a texture. Its file is "src:<text path>" and
// "pieces:a,b,c"; each piece is "<name>@<text>". A piece named "!" makes
// Finish fail after it published the others.
type cut struct{ Pieces []Handle[piece] }

type piece struct{ S string }

type cutDecoded struct {
	src    DepRef[text]
	pieces []string
}

type cutLoader struct{ texts *Store[text] }

func (cutLoader) Decode(ctx *DecodeContext, data []byte) (cutDecoded, error) {
	var d cutDecoded
	for line := range strings.SplitSeq(string(data), "\n") {
		k, v, _ := strings.Cut(line, ":")
		switch k {
		case "src":
			d.src = Dep[text](ctx, v)
		case "pieces":
			d.pieces = strings.Split(v, ",")
		}
	}
	return d, nil
}

func (l cutLoader) Finish(ctx *FinishContext, d cutDecoded) (cut, error) {
	src := l.texts.Get(Resolve(ctx, d.src)).S
	var c cut
	for _, n := range d.pieces {
		if n == "!" {
			return cut{}, errors.New("bad piece")
		}
		c.Pieces = append(c.Pieces, AddChild(ctx, n, piece{S: n + "@" + src}))
	}
	return c, nil
}

func (cutLoader) Unload(*cut) {}

type pieceLoader struct{ unloads int }

func (pieceLoader) Decode(*DecodeContext, []byte) (string, error) { return "", errors.New("no") }
func (pieceLoader) Finish(*FinishContext, string) (piece, error)  { return piece{}, nil }
func (l *pieceLoader) Unload(*piece)                              { l.unloads++ }

func newCutHarness(t *testing.T) (*watchHarness, *Store[cut], *Store[piece], *pieceLoader) {
	t.Helper()
	w := newWatchHarness(t, fstest.MapFS{}, Options{})
	pl := &pieceLoader{}
	cuts := Register(w.srv, Loader[cut, cutDecoded](cutLoader{texts: w.texts}), cut{})
	pieces := Register(w.srv, Loader[piece, string](pl), piece{S: "placeholder"})
	RegisterChild[cut, piece](w.srv)
	return w, cuts, pieces, pl
}

func TestReloadRecutsChildrenInPlace(t *testing.T) {
	w, cuts, pieces, pl := newCutHarness(t)
	w.write("c.cut", "src:img.txt\npieces:a,b")
	w.write("img.txt", "v1")
	sc := w.srv.NewScope("s")
	b := Load[piece](sc, "c.cut#b")
	c := Load[cut](sc, "c.cut")
	w.settle(sc)
	w.srv.Update()
	if pieces.Get(b).S != "b@v1" {
		t.Fatalf("piece = %q", pieces.Get(b).S)
	}

	// The dependency changes: the parent is re-finished, the pieces get the
	// new values in the same slots.
	w.write("img.txt", "v2")
	ev := w.tick()
	if pieces.Get(b).S != "b@v2" || pieces.Version(b) != 2 {
		t.Fatalf("piece after dependency change = %q version %d", pieces.Get(b).S, pieces.Version(b))
	}
	if cuts.Get(c).Pieces[1] != b {
		t.Fatal("re-finished parent published a different handle")
	}
	if got := strings.Join(eventsFor(ev, EventModified), " "); got != "img.txt c.cut c.cut#a c.cut#b" {
		t.Fatalf("modified = %q", got)
	}
	if pl.unloads != 2 {
		t.Fatalf("old pieces unloaded %d times, want 2", pl.unloads)
	}
}

func TestReloadRemovedChildFails(t *testing.T) {
	w, cuts, pieces, _ := newCutHarness(t)
	w.write("c.cut", "src:img.txt\npieces:a,b")
	w.write("img.txt", "v1")
	sc := w.srv.NewScope("s")
	a := Load[piece](sc, "c.cut#a")
	b := Load[piece](sc, "c.cut#b")
	c := Load[cut](sc, "c.cut")
	w.settle(sc)
	w.srv.Update()

	w.write("c.cut", "src:img.txt\npieces:a,c")
	ev := w.tick()
	if pieces.Get(a).S != "a@v1" || pieces.Version(a) != 2 {
		t.Fatalf("kept piece = %q version %d", pieces.Get(a).S, pieces.Version(a))
	}
	err := pieces.Err(b)
	if err == nil || !strings.Contains(err.Error(), `asset: c.cut no longer has piece "b"`) {
		t.Fatalf("removed piece error = %v", err)
	}
	if pieces.Get(b).S != "placeholder" {
		t.Fatal("removed piece does not resolve to the placeholder")
	}
	if got := eventsFor(ev, EventFailed); len(got) != 1 || got[0] != "c.cut#b" {
		t.Fatalf("failed events = %v", got)
	}
	added := cuts.Get(c).Pieces[1]
	if pieces.Get(added).S != "c@v1" || pieces.Key(added).Sub != "c" {
		t.Fatalf("new piece = %q", pieces.Get(added).S)
	}

	// Brought back: the same handle works again.
	w.write("c.cut", "src:img.txt\npieces:a,b")
	w.tick()
	if pieces.Get(b).S != "b@v1" || pieces.Err(b) != nil {
		t.Fatalf("restored piece = %q, err %v", pieces.Get(b).S, pieces.Err(b))
	}
}

func TestReloadFinishFailureLeavesChildren(t *testing.T) {
	w, cuts, pieces, _ := newCutHarness(t)
	w.write("c.cut", "src:img.txt\npieces:a,b")
	w.write("img.txt", "v1")
	sc := w.srv.NewScope("s")
	c := Load[cut](sc, "c.cut")
	w.settle(sc)
	w.srv.Update()
	n := pieces.Len()

	// "z" is added before "!" fails the Finish: it must not survive.
	w.write("c.cut", "src:img.txt\npieces:a,z,!")
	w.tick()
	if pieces.Len() != n {
		t.Fatalf("pieces = %d after a failed reload, want %d", pieces.Len(), n)
	}
	for _, h := range cuts.Get(c).Pieces {
		if !pieces.Ready(h) || pieces.Version(h) != 1 {
			t.Fatalf("piece %s changed: ready=%v version=%d", pieces.Key(h), pieces.Ready(h), pieces.Version(h))
		}
	}
	if w.errsContaining("keeping previous version: c.cut: bad piece") != 1 {
		t.Fatalf("errors: %v", w.errs)
	}
}

func TestReloadWithWorkers(t *testing.T) {
	w := newWatchHarness(t, fstest.MapFS{}, Options{Workers: 3})
	w.write("a.txt", "one")
	sc := w.srv.NewScope("s")
	a, _ := LoadSync[text](sc, "a.txt")
	w.srv.Update()

	w.write("a.txt", "two")
	w.tick()
	for range 2000 {
		if w.texts.Version(a) == 2 {
			break
		}
		// Ready while the candidate decodes: the old value is served.
		if !w.texts.Ready(a) {
			t.Fatal("asset not Ready during reload")
		}
		w.srv.Update()
		time.Sleep(time.Millisecond)
	}
	if w.texts.Get(a).S != "two" {
		t.Fatalf("value = %q after reload with workers", w.texts.Get(a).S)
	}
}

func TestReloadReleasedMeanwhile(t *testing.T) {
	w := newWatchHarness(t, fstest.MapFS{}, Options{Workers: 2})
	w.write("a.txt", "one")
	sc := w.srv.NewScope("s")
	a, _ := LoadSync[text](sc, "a.txt")
	w.srv.Update()

	w.write("a.txt", "two")
	w.tick() // candidate in flight (or done)
	sc.Release()
	for range 50 {
		w.srv.Update()
		time.Sleep(time.Millisecond)
	}
	if _, st := w.texts.Lookup(a); st != Missing {
		t.Fatalf("released asset state = %v", st)
	}
	if w.srv.Stats().InFlight != 0 {
		t.Fatal("result of a released reload was not drained")
	}
}
