package asset

import (
	"testing"
	"testing/fstest"
	"time"
)

func newBudgetHarness(t *testing.T, budget time.Duration, n int) (*harness, *Scope, *fakeClock) {
	t.Helper()
	files := fstest.MapFS{}
	for i := range n {
		files[string(rune('a'+i))+".txt"] = file("x")
	}
	h := newHarness(t, files, Options{CommitBudget: budget})
	clock := &fakeClock{t: time.Unix(0, 0)}
	h.srv.now = clock.now
	// Every Finish costs 1ms of fake time.
	h.loader.onFinish = func(*FinishContext, string) { clock.advance(time.Millisecond) }
	sc := h.srv.NewScope("s")
	for i := range n {
		Load[text](sc, string(rune('a'+i))+".txt")
	}
	return h, sc, clock
}

func TestCommitBudgetSpreadsFinishOverUpdates(t *testing.T) {
	h, sc, _ := newBudgetHarness(t, 3*time.Millisecond, 8)

	var per []int
	for !sc.Done() {
		before := int(h.loader.finishes.Load())
		h.srv.Update()
		per = append(per, int(h.loader.finishes.Load())-before)
		if len(per) > 20 {
			t.Fatal("never finished")
		}
	}
	// 1ms per Finish, 3ms budget: 3, 3, 2.
	want := []int{3, 3, 2}
	if len(per) != len(want) {
		t.Fatalf("finishes per Update = %v, want %v", per, want)
	}
	for i := range want {
		if per[i] != want[i] {
			t.Fatalf("finishes per Update = %v, want %v", per, want)
		}
	}
}

func TestCommitBudgetAlwaysMakesProgress(t *testing.T) {
	// Budget smaller than a single Finish: still one per Update.
	h, sc, _ := newBudgetHarness(t, time.Nanosecond, 4)
	for i := 1; i <= 4; i++ {
		h.srv.Update()
		if got := int(h.loader.finishes.Load()); got != i {
			t.Fatalf("after Update %d: %d finishes, want %d", i, got, i)
		}
	}
	if !sc.Done() {
		t.Fatal("not done")
	}
}

func TestCommitBudgetZeroIsUnlimited(t *testing.T) {
	h, sc, _ := newBudgetHarness(t, 0, 8)
	h.srv.Update()
	if !sc.Done() || h.loader.finishes.Load() != 8 {
		t.Fatalf("finishes = %d, done = %v", h.loader.finishes.Load(), sc.Done())
	}
}

func TestCommitBudgetKeepsFIFOOrder(t *testing.T) {
	h, _, _ := newBudgetHarness(t, time.Millisecond, 4)
	var order []string
	h.loader.onFinish = func(ctx *FinishContext, s string) { order = append(order, ctx.Key.Path) }
	for range 4 {
		h.srv.Update()
	}
	want := []string{"a.txt", "b.txt", "c.txt", "d.txt"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("finish order = %v, want %v", order, want)
		}
	}
}
