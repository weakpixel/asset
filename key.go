package asset

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
)

// Key names an asset: a clean, slash-separated path inside the Server's
// fs.FS, plus an optional sub-asset name after '#': "chars/hero.json#run_0".
// Together with the asset type it is the cache key, so loading the same key
// twice returns the same handle.
type Key struct {
	Path string
	Sub  string
}

// String returns "path" or "path#sub".
func (k Key) String() string {
	if k.Sub == "" {
		return k.Path
	}
	return k.Path + "#" + k.Sub
}

// ParseKey normalizes s into a Key: the path is cleaned with path.Clean and
// the text after the first '#' is the sub-asset name.
//
// It panics with "asset: bad key" when s contains a backslash, is empty, is
// absolute, escapes the root with "..", or has an empty sub name after '#'.
func ParseKey(s string) Key {
	k, err := parseKey(s)
	if err != nil {
		panic(err.Error())
	}
	return k
}

// parseKey is ParseKey returning an error, for keys that come from data files
// (dependency paths), where a bad key is a load error and not a panic.
func parseKey(s string) (Key, error) {
	if strings.ContainsRune(s, '\\') {
		return Key{}, badKey(s, errors.New("backslash, use forward slashes"))
	}
	p, sub, hasSub := strings.Cut(s, "#")
	if hasSub && sub == "" {
		return Key{}, badKey(s, errors.New("empty sub-asset name after '#'"))
	}
	if p == "" {
		return Key{}, badKey(s, errors.New("empty path"))
	}
	p = path.Clean(p)
	if p == "." || !fs.ValidPath(p) {
		return Key{}, badKey(s, errors.New("path must be relative and stay inside the root"))
	}
	return Key{Path: p, Sub: sub}, nil
}

func badKey(s string, why error) error {
	return fmt.Errorf("asset: bad key %q: %w", s, why)
}
