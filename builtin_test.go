package asset

import (
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// starterLoader serves "builtin:<name>" keys without a file and reads every
// other key from the file system.
type starterLoader struct{}

func (starterLoader) Builtin(k Key) bool { return strings.HasPrefix(k.Path, "builtin:") }

func (starterLoader) Decode(ctx *DecodeContext, data []byte) (string, error) {
	if data == nil {
		return "built-in " + strings.TrimPrefix(ctx.Key.Path, "builtin:"), nil
	}
	return string(data), nil
}

func (starterLoader) Finish(_ *FinishContext, s string) (starter, error) {
	return starter{S: s}, nil
}
func (starterLoader) Unload(*starter) {}

type starter struct{ S string }

func TestBuiltinKeysReadNoFile(t *testing.T) {
	for _, workers := range []int{0, 2} {
		srv := NewServer(Options{
			FS:            fstest.MapFS{"a.txt": file("from file")},
			Workers:       workers,
			WatchInterval: time.Millisecond,
		})
		st := Register[starter, string](srv, starterLoader{}, starter{S: "placeholder"})
		b, err := LoadSync[starter](srv.Global(), "builtin:chirpy")
		if err != nil {
			t.Fatalf("workers %d: builtin key: %v", workers, err)
		}
		if got := st.Get(b).S; got != "built-in chirpy" {
			t.Errorf("workers %d: builtin value %q", workers, got)
		}
		f, err := LoadSync[starter](srv.Global(), "a.txt")
		if err != nil || st.Get(f).S != "from file" {
			t.Errorf("workers %d: file key: %q, %v", workers, st.Get(f).S, err)
		}
		if _, err := LoadSync[starter](srv.Global(), "missing.txt"); err == nil {
			t.Errorf("workers %d: a missing file that is not builtin loaded", workers)
		}
		// Polling never reloads a builtin key: it has no file to stat.
		v := st.Version(b)
		for range 3 {
			time.Sleep(2 * time.Millisecond)
			srv.Update()
		}
		if st.Version(b) != v {
			t.Errorf("workers %d: builtin key reloaded", workers)
		}
		srv.Close()
	}
}
