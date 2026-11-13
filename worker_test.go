package asset

import (
	"fmt"
	"runtime"
	"testing"
	"testing/fstest"
	"time"
)

func TestWorkersLoadMany(t *testing.T) {
	const n = 100
	files := fstest.MapFS{}
	for i := range n {
		files[fmt.Sprintf("f%03d.txt", i)] = file(fmt.Sprintf("v%d", i))
	}
	h := newHarness(t, files, Options{Workers: 4})
	sc := h.srv.NewScope("many")
	handles := make([]Handle[text], n)
	for i := range n {
		handles[i] = Load[text](sc, fmt.Sprintf("f%03d.txt", i))
	}
	h.settle(sc)

	for i, hd := range handles {
		if got, want := h.texts.Get(hd).S, fmt.Sprintf("v%d", i); got != want {
			t.Fatalf("asset %d = %q, want %q", i, got, want)
		}
	}
	if got := h.loader.decodes.Load(); got != n {
		t.Fatalf("decoded %d times, want %d", got, n)
	}
}

// More jobs than the channel holds: the overflow waits in pending and still
// loads.
func TestWorkersOverflowJobQueue(t *testing.T) {
	n := jobQueueSize*2 + 10
	files := fstest.MapFS{}
	for i := range n {
		files[fmt.Sprintf("f%d.txt", i)] = file("x")
	}
	h := newHarness(t, files, Options{Workers: 2})
	sc := h.srv.NewScope("many")
	for i := range n {
		Load[text](sc, fmt.Sprintf("f%d.txt", i))
	}
	h.settle(sc)
	if ready, total := sc.Progress(); ready != n || total != n {
		t.Fatalf("progress %d/%d", ready, total)
	}
}

func TestWorkersFinishRunsOnCallerGoroutine(t *testing.T) {
	// Finish runs inside Update; a Finish that touches the store directly is
	// only safe if that holds. The race detector enforces it.
	h := newHarness(t, fstest.MapFS{"a.txt": file("a"), "b.txt": file("b")}, Options{Workers: 4})
	h.loader.onFinish = func(ctx *FinishContext, s string) { _ = h.texts.Len() }
	sc := h.srv.NewScope("s")
	Load[text](sc, "a.txt")
	Load[text](sc, "b.txt")
	h.settle(sc)
}

func TestCloseStopsWorkers(t *testing.T) {
	before := runtime.NumGoroutine()
	srv := NewServer(Options{Workers: 8, FS: fstest.MapFS{"a.txt": file("a")}})
	Register(srv, Loader[text, string](&textLoader{}), text{})
	for range 20 {
		Load[text](srv.Global(), "a.txt")
	}
	if runtime.NumGoroutine() < before+8 {
		t.Fatalf("workers did not start: %d goroutines, was %d", runtime.NumGoroutine(), before)
	}
	srv.Update()
	srv.Close()

	// Close waits for the workers, so the count is back right away; allow the
	// runtime a moment to retire them.
	for range 100 {
		if runtime.NumGoroutine() <= before {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("goroutine leak: %d after Close, %d before", runtime.NumGoroutine(), before)
}

// A worker blocked sending a result must not outlive Close.
func TestCloseWithUnreadResults(t *testing.T) {
	before := runtime.NumGoroutine()
	files := fstest.MapFS{}
	for i := range 50 {
		files[fmt.Sprintf("f%d.txt", i)] = file("x")
	}
	srv := NewServer(Options{Workers: 4, FS: files})
	Register(srv, Loader[text, string](&textLoader{}), text{})
	for i := range 50 {
		Load[text](srv.Global(), fmt.Sprintf("f%d.txt", i))
	}
	srv.Update() // dispatches; results are never read
	srv.Close()
	for range 100 {
		if runtime.NumGoroutine() <= before {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("goroutine leak: %d after Close, %d before", runtime.NumGoroutine(), before)
}
