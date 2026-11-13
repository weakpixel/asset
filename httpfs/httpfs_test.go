package httpfs

import (
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/weakpixel/asset"
)

var modTime = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

// newServer serves files under /assets/ and answers /broken/ with a 500.
func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	files := fstest.MapFS{
		"hero/hero.png":               {Data: []byte("png"), ModTime: modTime},
		"hero/04 Stranded - Pack.png": {Data: []byte("sheet"), ModTime: modTime},
	}
	mux := http.NewServeMux()
	mux.Handle("/assets/", http.StripPrefix("/assets/", http.FileServerFS(files)))
	mux.HandleFunc("/broken/", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newFS(t *testing.T, base string) *FS {
	t.Helper()
	f, err := New(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestReadFileAndStat(t *testing.T) {
	srv := newServer(t)
	fsys := newFS(t, srv.URL+"/assets") // no trailing slash: added

	for _, name := range []string{"hero/hero.png", "hero/04 Stranded - Pack.png"} {
		want := map[string]string{"hero/hero.png": "png", "hero/04 Stranded - Pack.png": "sheet"}[name]
		data, err := fs.ReadFile(fsys, name)
		if err != nil || string(data) != want {
			t.Errorf("ReadFile(%q) = %q, %v; want %q", name, data, err, want)
		}
		fi, err := fs.Stat(fsys, name)
		if err != nil {
			t.Fatalf("Stat(%q): %v", name, err)
		}
		if fi.Size() != int64(len(want)) || !fi.ModTime().Equal(modTime) || fi.IsDir() {
			t.Errorf("Stat(%q) = size %d, mod %v, dir %v", name, fi.Size(), fi.ModTime(), fi.IsDir())
		}
	}

	f, err := fsys.Open("hero/hero.png")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, _ := io.ReadAll(f)
	fi, _ := f.Stat()
	if string(data) != "png" || fi.Name() != "hero.png" || fi.Size() != 3 {
		t.Errorf("Open: data %q, name %q, size %d", data, fi.Name(), fi.Size())
	}
}

func TestErrors(t *testing.T) {
	srv := newServer(t)
	fsys := newFS(t, srv.URL+"/assets/")
	broken := newFS(t, srv.URL+"/broken/")

	tests := []struct {
		name    string
		call    func() error
		is      error  // errors.Is target, or nil
		message string // substring
	}{
		{"missing read", func() error { _, err := fs.ReadFile(fsys, "nope.png"); return err }, fs.ErrNotExist, "nope.png"},
		{"missing stat", func() error { _, err := fs.Stat(fsys, "nope.png"); return err }, fs.ErrNotExist, "nope.png"},
		{"missing open", func() error { _, err := fsys.Open("nope.png"); return err }, fs.ErrNotExist, "nope.png"},
		{"server error", func() error { _, err := fs.ReadFile(broken, "a.png"); return err }, nil, "HTTP 500"},
		{"invalid path", func() error { _, err := fs.ReadFile(fsys, "../secret"); return err }, fs.ErrInvalid, "../secret"},
		{"absolute path", func() error { _, err := fs.Stat(fsys, "/etc/passwd"); return err }, fs.ErrInvalid, "/etc/passwd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			if err == nil {
				t.Fatal("no error")
			}
			if tt.is != nil && !errors.Is(err, tt.is) {
				t.Errorf("err = %v, want errors.Is %v", err, tt.is)
			}
			if !strings.Contains(err.Error(), tt.message) {
				t.Errorf("err = %v, want it to contain %q", err, tt.message)
			}
		})
	}
}

// textLoader loads a file as a string.
type textLoader struct{}

func (textLoader) Decode(_ *asset.DecodeContext, data []byte) (string, error) {
	return string(data), nil
}
func (textLoader) Finish(_ *asset.FinishContext, s string) (string, error) { return s, nil }
func (textLoader) Unload(*string)                                          {}

func TestAssetServer(t *testing.T) {
	srv := newServer(t)
	assets := asset.NewServer(asset.Options{FS: newFS(t, srv.URL+"/assets/"), Workers: 0})
	store := asset.Register[string, string](assets, textLoader{}, "")
	h, err := asset.LoadSync[string](assets.Global(), "hero/04 Stranded - Pack.png")
	if err != nil {
		t.Fatal(err)
	}
	if got := *store.Get(h); got != "sheet" {
		t.Errorf("asset = %q, want %q", got, "sheet")
	}
}
