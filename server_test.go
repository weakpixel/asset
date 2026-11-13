package asset

import (
	"testing"
	"testing/fstest"
)

type other struct{}

type otherLoader struct{}

func (otherLoader) Decode(*DecodeContext, []byte) (struct{}, error) { return struct{}{}, nil }
func (otherLoader) Finish(*FinishContext, struct{}) (other, error)  { return other{}, nil }
func (otherLoader) Unload(*other)                                   {}

func TestRegisterPanics(t *testing.T) {
	h := newHarness(t, nil, Options{})

	t.Run("duplicate", func(t *testing.T) {
		mustPanic(t, "asset: type already registered: asset.text", func() {
			Register(h.srv, Loader[text, string](h.loader), text{})
		})
	})
	t.Run("StoreOf unregistered", func(t *testing.T) {
		mustPanic(t, "asset: type not registered: asset.other", func() { StoreOf[other](h.srv) })
	})
	t.Run("Load unregistered", func(t *testing.T) {
		mustPanic(t, "asset: type not registered: asset.other", func() { Load[other](h.srv.Global(), "x") })
	})
	t.Run("TypeIDOf unregistered", func(t *testing.T) {
		mustPanic(t, "asset: type not registered", func() { TypeIDOf[other](h.srv) })
	})
}

func TestStoreOfAndTypeIDs(t *testing.T) {
	h := newHarness(t, nil, Options{})
	if StoreOf[text](h.srv) != h.texts {
		t.Fatal("StoreOf returned a different store than Register")
	}
	Register(h.srv, otherLoader{}, other{})
	a, b := TypeIDOf[text](h.srv), TypeIDOf[other](h.srv)
	if a == 0 || b == 0 || a == b {
		t.Fatalf("TypeIDs = %d, %d", a, b)
	}
}

func TestPlaceholderIsReturnedForFailedFile(t *testing.T) {
	h := newHarness(t, fstest.MapFS{}, Options{})
	hd := Load[text](h.srv.Global(), "missing.txt")
	h.srv.Update()
	if h.texts.Get(hd).S != "placeholder" || h.texts.Err(hd) == nil {
		t.Fatal("missing file did not fail with placeholder")
	}
}

func TestNoFS(t *testing.T) {
	srv := NewServer(Options{OnError: func(error) {}})
	defer srv.Close()
	st := Register(srv, Loader[text, string](&textLoader{}), text{})
	hd := Load[text](srv.Global(), "a.txt")
	srv.Update()
	if st.Err(hd) == nil {
		t.Fatal("Load without an FS must fail the asset")
	}
}

func TestRegisterChildPanics(t *testing.T) {
	h := newHarness(t, nil, Options{})
	Register(h.srv, otherLoader{}, other{})
	RegisterChild[text, other](h.srv)

	mustPanic(t, "asset: child type already registered", func() { RegisterChild[text, other](h.srv) })
	mustPanic(t, "asset: RegisterChild", func() { RegisterChild[text, text](h.srv) })
	type unregistered struct{}
	mustPanic(t, "asset: type not registered", func() { RegisterChild[text, unregistered](h.srv) })
}
