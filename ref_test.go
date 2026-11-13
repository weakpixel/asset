package asset

import (
	"testing"
	"testing/fstest"
)

func TestRefRoundTrip(t *testing.T) {
	h := newHarness(t, fstest.MapFS{"a.txt": file("a")}, Options{})
	sc := h.srv.Global()
	ha, err := LoadSync[text](sc, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	r := ha.Ref()
	if r.IsZero() || r.Index() != ha.Index() {
		t.Fatalf("Ref() = %+v, want index %d", r, ha.Index())
	}
	back := HandleOf[text](r)
	if back != ha {
		t.Fatalf("HandleOf(Ref()) = %+v, want %+v", back, ha)
	}
	if got := h.texts.Get(back).S; got != "a" {
		t.Fatalf("Get(HandleOf(Ref())) = %q, want %q", got, "a")
	}
	if !(Ref{}).IsZero() || !HandleOf[text](Ref{}).IsZero() {
		t.Fatal("zero Ref must map to the zero handle")
	}
}
