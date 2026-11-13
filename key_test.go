package asset

import "testing"

func TestParseKey(t *testing.T) {
	tests := []struct {
		in   string
		want Key
	}{
		{"hero.png", Key{Path: "hero.png"}},
		{"chars/hero.json#run_0", Key{Path: "chars/hero.json", Sub: "run_0"}},
		{"./chars//hero.png", Key{Path: "chars/hero.png"}},
		{"a/b/../c.png", Key{Path: "a/c.png"}},
		{"fonts/ui.ttf#16", Key{Path: "fonts/ui.ttf", Sub: "16"}},
		{"a.json#x#y", Key{Path: "a.json", Sub: "x#y"}},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := ParseKey(tt.in); got != tt.want {
				t.Fatalf("ParseKey(%q) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseKeyBad(t *testing.T) {
	for _, in := range []string{
		"", "#sub", ".", "a\\b.png", "../x.png", "a/../../x.png", "/abs.png", "a.json#",
	} {
		t.Run(in, func(t *testing.T) {
			mustPanic(t, "asset: bad key", func() { ParseKey(in) })
		})
	}
}

func TestKeyString(t *testing.T) {
	for in, want := range map[Key]string{
		{Path: "a.png"}:            "a.png",
		{Path: "a.json", Sub: "x"}: "a.json#x",
	} {
		if got := in.String(); got != want {
			t.Errorf("%+v.String() = %q, want %q", in, got, want)
		}
	}
}

func TestHandleZero(t *testing.T) {
	var h Handle[text]
	if !h.IsZero() {
		t.Fatal("zero Handle is not IsZero")
	}
	if (Handle[text]{index: 1}).IsZero() {
		t.Fatal("Handle{index: 1} is IsZero")
	}
}

func TestHandleIndex(t *testing.T) {
	if got := (Handle[text]{}).Index(); got != 0 {
		t.Fatalf("zero Handle Index = %d, want 0", got)
	}
	a, b := Handle[text]{index: 5, gen: 1}, Handle[text]{index: 5, gen: 2}
	if a.Index() != 5 || a.Index() != b.Index() {
		t.Fatalf("Index ignores the generation: %d, %d", a.Index(), b.Index())
	}
}

func TestStateString(t *testing.T) {
	if Ready.String() != "ready" || State(99).String() != "invalid" {
		t.Fatalf("State.String: %q %q", Ready, State(99))
	}
}
