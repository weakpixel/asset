# asset

`github.com/weakpixel/asset` is a handle-based asset system for Go games and other programs that load files in the background. Game state holds small typed handles instead of pointers; files load on worker goroutines, final values are built on the game goroutine, and scopes decide how long assets live.

- **Handles:** 8-byte generational indices, pointer-free and comparable. A stale handle is detected, never dangling.
- **Never nil:** `Store.Get` returns a placeholder while an asset is missing, loading or failed. It is a bounds check and a compare: no map, no lock, 0 allocations.
- **Two-phase loaders:** `Decode` (CPU work, on workers) and `Finish` (GPU objects and other game-goroutine work, under an optional time budget).
- **Scopes:** a level, a menu or the whole program owns the assets it loaded; releasing a scope unloads what nothing else holds. An optional keep-alive budget keeps recently released assets loaded.
- **Dependencies and sub-assets:** an atlas depends on its texture; a sprite is a sub-asset of its atlas (`"hero.json#run_0"`).
- **Hot reload:** by polling or by pushed paths, atomic per asset, under the same handles.
- **Any `fs.FS`:** `embed.FS`, `os.DirFS`, a zip, or `httpfs` for browser builds.

The module targets Go 1.27, uses the standard library only and has no dependencies. It builds for js/wasm.

| Package | Import path | What it is |
| --- | --- | --- |
| `asset` | `github.com/weakpixel/asset` | Server, stores, handles, scopes, loaders, events, hot reload, keep-alive |
| `httpfs` | `github.com/weakpixel/asset/httpfs` | A read-only `fs.FS` over HTTP and a reader for a change-event stream |

## Installation

```bash
go get github.com/weakpixel/asset
```

## Quick start

```go
package main

import (
	"fmt"
	"strings"
	"testing/fstest"

	"github.com/weakpixel/asset"
)

// Greeting is an asset type. A real one would hold a texture, a sound or
// parsed level data.
type Greeting struct{ Text string }

type greetingLoader struct{}

// Decode runs on a worker: CPU work only.
func (greetingLoader) Decode(_ *asset.DecodeContext, data []byte) (string, error) {
	return strings.ToUpper(string(data)), nil
}

// Finish runs inside Server.Update on the game goroutine.
func (greetingLoader) Finish(_ *asset.FinishContext, s string) (Greeting, error) {
	return Greeting{Text: s}, nil
}

func (greetingLoader) Unload(*Greeting) {}

func main() {
	srv := asset.NewServer(asset.Options{
		FS:      fstest.MapFS{"hello.txt": {Data: []byte("hello")}},
		Workers: 2,
	})
	defer srv.Close()
	greetings := asset.Register(srv, greetingLoader{}, Greeting{Text: "(loading)"})

	level := srv.NewScope("level-1")
	hello := asset.Load[Greeting](level, "hello.txt") // returns at once

	for !level.Done() { // a game calls Update once per tick instead
		srv.Update()
	}
	fmt.Println(greetings.Get(hello).Text) // HELLO

	level.Release()
	srv.Update()
	fmt.Println(greetings.Get(hello).Text) // (loading): the placeholder
}
```

## Documentation

- [Guide](doc/guide.md): handles, loading, scopes, keep-alive, states, dependencies, options, events, hot reload, HTTP, writing a loader, panics.
- [Design](doc/design.md): how stores, the loading pipeline, lifetime, hot reload and keep-alive are built and why.
