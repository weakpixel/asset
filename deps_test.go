package asset

import (
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
)

// bundle depends on other assets listed one per line as "text:path" or
// "bundle:path". Finish resolves them all, so a successful Finish proves every
// dependency was Ready.
type bundle struct {
	Texts   []Handle[text]
	Bundles []Handle[bundle]
}

type bundleDecoded struct {
	texts   []DepRef[text]
	bundles []DepRef[bundle]
}

type bundleLoader struct {
	finishes atomic.Int32
	order    []string // keys in Finish order (game goroutine only)
	check    func(ctx *FinishContext, b bundle)
}

func (l *bundleLoader) Decode(ctx *DecodeContext, data []byte) (bundleDecoded, error) {
	var d bundleDecoded
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		kind, path, _ := strings.Cut(line, ":")
		switch kind {
		case "text":
			d.texts = append(d.texts, Dep[text](ctx, path))
		case "bundle":
			d.bundles = append(d.bundles, Dep[bundle](ctx, path))
		}
	}
	return d, nil
}

func (l *bundleLoader) Finish(ctx *FinishContext, d bundleDecoded) (bundle, error) {
	l.finishes.Add(1)
	l.order = append(l.order, ctx.Key.String())
	var b bundle
	for _, r := range d.texts {
		b.Texts = append(b.Texts, Resolve(ctx, r))
	}
	for _, r := range d.bundles {
		b.Bundles = append(b.Bundles, Resolve(ctx, r))
	}
	if l.check != nil {
		l.check(ctx, b)
	}
	return b, nil
}

func (l *bundleLoader) Unload(*bundle) {}

func newBundleHarness(t *testing.T, files fstest.MapFS, opts Options) (*harness, *Store[bundle], *bundleLoader) {
	t.Helper()
	h := newHarness(t, files, opts)
	bl := &bundleLoader{}
	bs := Register(h.srv, Loader[bundle, bundleDecoded](bl), bundle{})
	return h, bs, bl
}

func TestDepChain(t *testing.T) {
	for _, workers := range []int{0, 4} {
		h, bundles, bl := newBundleHarness(t, fstest.MapFS{
			"top.bundle":  file("text:t.txt\nbundle:mid.bundle"),
			"mid.bundle":  file("bundle:leaf.bundle"),
			"leaf.bundle": file("text:l.txt"),
			"t.txt":       file("T"),
			"l.txt":       file("L"),
		}, Options{Workers: workers})
		bl.check = func(ctx *FinishContext, b bundle) {
			for _, th := range b.Texts {
				if !h.texts.Ready(th) {
					t.Errorf("%s finished before its text dependency was ready", ctx.Key)
				}
			}
			for _, bh := range b.Bundles {
				if !StoreOf[bundle](h.srv).Ready(bh) {
					t.Errorf("%s finished before its bundle dependency was ready", ctx.Key)
				}
			}
		}
		sc := h.srv.NewScope("s")
		top := Load[bundle](sc, "top.bundle")

		if ready, total := sc.Progress(); ready != 0 || total != 1 {
			t.Fatalf("workers=%d: Progress before Update = %d/%d", workers, ready, total)
		}
		h.settle(sc)

		if !bundles.Ready(top) || len(h.errs) != 0 {
			t.Fatalf("workers=%d: top not ready, errs=%v", workers, h.errs)
		}
		// Progress counts dependencies: top, t.txt, mid, leaf, l.txt.
		if ready, total := sc.Progress(); ready != 5 || total != 5 {
			t.Fatalf("workers=%d: Progress = %d/%d, want 5/5", workers, ready, total)
		}
		// Parents finish after their dependencies.
		pos := map[string]int{}
		for i, k := range bl.order {
			pos[k] = i
		}
		if !(pos["leaf.bundle"] < pos["mid.bundle"] && pos["mid.bundle"] < pos["top.bundle"]) {
			t.Fatalf("workers=%d: finish order = %v", workers, bl.order)
		}
		v := bundles.Get(top)
		if h.texts.Get(v.Texts[0]).S != "T" {
			t.Fatal("resolved text handle does not point at t.txt")
		}
	}
}

func TestDepRelativePath(t *testing.T) {
	h, bundles, _ := newBundleHarness(t, fstest.MapFS{
		"chars/hero.bundle": file("text:hero.txt\ntext:../shared/x.txt"),
		"chars/hero.txt":    file("H"),
		"shared/x.txt":      file("X"),
	}, Options{})
	sc := h.srv.NewScope("s")
	b := Load[bundle](sc, "chars/hero.bundle")
	h.srv.Update()

	if !bundles.Ready(b) {
		t.Fatalf("not ready: %v", bundles.Err(b))
	}
	got := bundles.Get(b).Texts
	if h.texts.Key(got[0]).Path != "chars/hero.txt" || h.texts.Key(got[1]).Path != "shared/x.txt" {
		t.Fatalf("dependency keys: %v %v", h.texts.Key(got[0]), h.texts.Key(got[1]))
	}
}

func TestDepShared(t *testing.T) {
	h, bundles, bl := newBundleHarness(t, fstest.MapFS{
		"a.bundle": file("text:c.txt"),
		"b.bundle": file("text:c.txt"),
		"c.txt":    file("C"),
	}, Options{})
	sa, sb := h.srv.NewScope("a"), h.srv.NewScope("b")
	a := Load[bundle](sa, "a.bundle")
	b := Load[bundle](sb, "b.bundle")
	h.srv.Update()
	c := bundles.Get(a).Texts[0]
	if c != bundles.Get(b).Texts[0] || h.loader.decodes.Load() != 1 {
		t.Fatalf("shared dependency loaded twice (decodes=%d)", h.loader.decodes.Load())
	}

	sa.Release()
	h.srv.Update()
	if bundles.Ready(a) || !bundles.Ready(b) || !h.texts.Ready(c) {
		t.Fatal("releasing one parent must free it but keep the shared dependency")
	}
	sb.Release()
	ev := h.srv.Update()
	if h.texts.Ready(c) || bundles.Ready(b) {
		t.Fatal("dependency survived its last parent")
	}
	// b.bundle and c.txt both unloaded in one Update (cascade).
	if ev.Len() != 2 || h.loader.unloads.Load() != 1 || bl.finishes.Load() != 2 {
		t.Fatalf("events=%d unloads=%d", ev.Len(), h.loader.unloads.Load())
	}
}

func TestDepFailureFailsParentWithoutFinish(t *testing.T) {
	for name, files := range map[string]fstest.MapFS{
		"corrupt dependency": {"p.bundle": file("text:bad.txt"), "bad.txt": file("bad")},
		"missing dependency": {"p.bundle": file("text:nope.txt")},
	} {
		t.Run(name, func(t *testing.T) {
			h, bundles, bl := newBundleHarness(t, files, Options{})
			sc := h.srv.NewScope("s")
			p := Load[bundle](sc, "p.bundle")
			h.srv.Update()

			err := bundles.Err(p)
			if err == nil || !strings.Contains(err.Error(), "dependency") {
				t.Fatalf("parent err = %v", err)
			}
			if bl.finishes.Load() != 0 {
				t.Fatal("parent Finish ran although its dependency failed")
			}
			if !sc.Done() {
				t.Fatal("scope with failed assets must still be Done")
			}
			if sc.Err() == nil {
				t.Fatal("scope Err is nil")
			}
			if len(h.errs) != 2 {
				t.Fatalf("OnError called %d times, want 2 (dependency and parent): %v", len(h.errs), h.errs)
			}
		})
	}
}

func TestDepCycle(t *testing.T) {
	tests := map[string]struct {
		files fstest.MapFS
		root  string
		want  string
	}{
		"two": {
			files: fstest.MapFS{"a.bundle": file("bundle:b.bundle"), "b.bundle": file("bundle:a.bundle")},
			root:  "a.bundle",
			want:  "asset: dependency cycle: b.bundle -> a.bundle -> b.bundle",
		},
		"self": {
			files: fstest.MapFS{"a.bundle": file("bundle:a.bundle")},
			root:  "a.bundle",
			want:  "asset: dependency cycle: a.bundle -> a.bundle",
		},
		"three": {
			files: fstest.MapFS{
				"a.bundle": file("bundle:b.bundle"),
				"b.bundle": file("bundle:c.bundle"),
				"c.bundle": file("bundle:a.bundle"),
			},
			root: "a.bundle",
			want: "asset: dependency cycle: c.bundle -> a.bundle -> b.bundle -> c.bundle",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			h, bundles, bl := newBundleHarness(t, tt.files, Options{})
			sc := h.srv.NewScope("s")
			root := Load[bundle](sc, tt.root)
			h.srv.Update() // must not panic or hang

			if !sc.Done() || bundles.Ready(root) {
				t.Fatal("cycle must settle as Failed")
			}
			if err := sc.Err(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Err = %v, want it to contain %q", err, tt.want)
			}
			if bl.finishes.Load() != 0 {
				t.Fatal("Finish ran inside a cycle")
			}
			// And the cycle can be unloaded.
			sc.Release()
			h.srv.Update()
			if n := h.srv.Stats().States; n[Ready]+n[Failed]+n[Waiting] != 0 {
				t.Fatalf("assets left after release: %v", n)
			}
		})
	}
}

func TestDepBadPathIsLoadError(t *testing.T) {
	h, bundles, _ := newBundleHarness(t, fstest.MapFS{"p.bundle": file("text:../../escape.txt")}, Options{})
	sc := h.srv.NewScope("s")
	p := Load[bundle](sc, "p.bundle")
	h.srv.Update()
	if err := bundles.Err(p); err == nil || !strings.Contains(err.Error(), "asset: bad key") {
		t.Fatalf("Err = %v", err)
	}
}

type needsOther struct{}

// needsOtherLoader declares a dependency on `other`, which tests leave
// unregistered.
type needsOtherLoader struct{}

func (needsOtherLoader) Decode(ctx *DecodeContext, _ []byte) (DepRef[other], error) {
	return Dep[other](ctx, "o.x"), nil
}
func (needsOtherLoader) Finish(*FinishContext, DepRef[other]) (needsOther, error) {
	return needsOther{}, nil
}
func (needsOtherLoader) Unload(*needsOther) {}

func TestDepUnregisteredTypePanics(t *testing.T) {
	h := newHarness(t, fstest.MapFS{"n.x": file("x")}, Options{})
	Register(h.srv, Loader[needsOther, DepRef[other]](needsOtherLoader{}), needsOther{})
	Load[needsOther](h.srv.Global(), "n.x")
	mustPanic(t, "asset: type not registered", func() { h.srv.Update() })
}

func TestResolvePanics(t *testing.T) {
	h, _, bl := newBundleHarness(t, fstest.MapFS{"p.bundle": file("text:t.txt"), "t.txt": file("T")}, Options{})
	bl.check = func(ctx *FinishContext, _ bundle) {
		mustPanic(t, "asset: Resolve: invalid DepRef", func() { Resolve(ctx, DepRef[text]{}) })
		mustPanic(t, "asset: Resolve: DepRef of", func() { Resolve(ctx, DepRef[bundle]{n: 1}) })
	}
	sc := h.srv.NewScope("s")
	Load[bundle](sc, "p.bundle")
	h.srv.Update()
	if bl.finishes.Load() != 1 {
		t.Fatal("Finish did not run")
	}
}
