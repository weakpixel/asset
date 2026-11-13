package asset

import (
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"
)

func mustPanic(t *testing.T, want string, f func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic containing %q", want)
		}
		if msg, _ := r.(string); !strings.Contains(msg, want) {
			t.Fatalf("panic %v does not contain %q", r, want)
		}
	}()
	f()
}

// text is the asset type of most tests: a file's contents as a string.
type text struct{ S string }

// textLoader is a fake loader. The file "bad" fails to decode and "badfinish"
// fails to finish. Counters are atomic because Decode runs on workers.
type textLoader struct {
	decodes, finishes, unloads atomic.Int32
	onFinish                   func(ctx *FinishContext, s string)
}

func (l *textLoader) Decode(ctx *DecodeContext, data []byte) (string, error) {
	l.decodes.Add(1)
	if string(data) == "bad" {
		return "", errors.New("corrupt")
	}
	return string(data), nil
}

func (l *textLoader) Finish(ctx *FinishContext, s string) (text, error) {
	l.finishes.Add(1)
	if s == "badfinish" {
		return text{}, errors.New("finish failed")
	}
	if l.onFinish != nil {
		l.onFinish(ctx, s)
	}
	return text{S: s}, nil
}

func (l *textLoader) Unload(v *text) { l.unloads.Add(1) }

// harness bundles a server with the text type registered, collecting errors
// passed to OnError.
type harness struct {
	t      *testing.T
	srv    *Server
	texts  *Store[text]
	loader *textLoader
	errs   []error
}

func newHarness(t *testing.T, files fstest.MapFS, opts Options) *harness {
	t.Helper()
	h := &harness{t: t, loader: &textLoader{}}
	opts.FS = files
	opts.OnError = func(err error) { h.errs = append(h.errs, err) }
	h.srv = NewServer(opts)
	h.texts = Register(h.srv, Loader[text, string](h.loader), text{S: "placeholder"})
	t.Cleanup(h.srv.Close)
	return h
}

func file(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }

// settle runs Update until the scope is done, failing the test if it never is.
// With Workers > 0 it sleeps briefly between ticks.
func (h *harness) settle(sc *Scope) {
	h.t.Helper()
	for range 2000 {
		h.srv.Update()
		if sc.Done() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	h.t.Fatalf("scope %s never became Done", sc.Name())
}

func kinds(ev *Events) []EventKind {
	var out []EventKind
	for e := range ev.All() {
		out = append(out, e.Kind)
	}
	return out
}

func equalKinds(a, b []EventKind) bool { return slices.Equal(a, b) }

// fakeClock is a clock tests advance by hand.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }
