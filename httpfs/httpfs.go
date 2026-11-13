// Package httpfs is a read-only [fs.FS] over HTTP, for games that load their
// assets from a web server instead of embedding them: a browser build
// (js/wasm, where net/http uses fetch) or an editor webview reading the
// workspace. It uses the standard library only.
//
// A file name is resolved against the base URL: with base
// "https://example.com/assets/", "hero/hero.png" is
// "https://example.com/assets/hero/hero.png". Directories cannot be listed;
// the asset system never lists them.
package httpfs

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// FS reads files with HTTP GET and stats them with HTTP HEAD. It implements
// [fs.FS], [fs.ReadFileFS] and [fs.StatFS]. Calls block until the response
// is in, so on js/wasm keep them on the asset workers (the asset system's
// default), not on the game goroutine.
type FS struct {
	base   *url.URL
	client *http.Client
}

// New returns an FS that reads files under baseURL. A nil client means
// http.DefaultClient. It returns an error if baseURL does not parse; a
// missing trailing slash is added, so the base always names a directory.
func New(baseURL string, client *http.Client) (*FS, error) {
	if !strings.HasSuffix(baseURL, "/") {
		baseURL += "/"
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("httpfs: base URL: %w", err)
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &FS{base: u, client: client}, nil
}

// Open reads the whole file with GET and returns it as an in-memory file.
func (f *FS) Open(name string) (fs.File, error) {
	data, info, err := f.get("open", name)
	if err != nil {
		return nil, err
	}
	return &file{Reader: bytes.NewReader(data), info: info}, nil
}

// ReadFile reads the whole file with GET.
func (f *FS) ReadFile(name string) ([]byte, error) {
	data, _, err := f.get("readfile", name)
	return data, err
}

// Stat requests the file with HEAD. Size comes from Content-Length (-1 if
// the server sends none), ModTime from Last-Modified (zero if absent).
func (f *FS) Stat(name string) (fs.FileInfo, error) {
	resp, err := f.do(http.MethodHead, "stat", name)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	return newInfo(name, resp.ContentLength, resp.Header), nil
}

// get performs a GET for name and reads the body.
func (f *FS) get(op, name string) ([]byte, fs.FileInfo, error) {
	resp, err := f.do(http.MethodGet, op, name)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, &fs.PathError{Op: op, Path: name, Err: err}
	}
	return data, newInfo(name, int64(len(data)), resp.Header), nil
}

// do sends one request for name and turns a 404 into fs.ErrNotExist and
// any other non-2xx status into an error naming it. On success the caller
// closes the body.
func (f *FS) do(method, op, name string) (*http.Response, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}
	// Escape each segment: asset names may contain spaces ("04 Stranded -
	// Pack 4.png") or '#', which would otherwise end the path.
	u := f.base.JoinPath(name)
	req, err := http.NewRequest(method, u.String(), nil)
	if err != nil {
		return nil, &fs.PathError{Op: op, Path: name, Err: err}
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, &fs.PathError{Op: op, Path: name, Err: err}
	}
	if resp.StatusCode/100 == 2 {
		return resp, nil
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, &fs.PathError{Op: op, Path: name, Err: fs.ErrNotExist}
	}
	return nil, &fs.PathError{Op: op, Path: name, Err: fmt.Errorf("HTTP %s", resp.Status)}
}

// file is an fs.File over bytes already read.
type file struct {
	*bytes.Reader
	info fs.FileInfo
}

func (f *file) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *file) Close() error               { return nil }

// info is the fs.FileInfo of a file read over HTTP.
type info struct {
	name    string
	size    int64
	modTime time.Time
}

func newInfo(name string, size int64, h http.Header) *info {
	t, _ := http.ParseTime(h.Get("Last-Modified")) // zero if absent or invalid
	return &info{name: path.Base(name), size: size, modTime: t}
}

func (i *info) Name() string       { return i.name }
func (i *info) Size() int64        { return i.size }
func (i *info) Mode() fs.FileMode  { return 0o444 }
func (i *info) ModTime() time.Time { return i.modTime }
func (i *info) IsDir() bool        { return false }
func (i *info) Sys() any           { return nil }
