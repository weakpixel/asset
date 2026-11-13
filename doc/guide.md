# Guide

Package `asset` (`github.com/weakpixel/asset`) loads files into typed values in the background and hands out handles to them. Standard library only, no graphics or audio API, so it is testable with `fstest.MapFS` and no GPU. How it is built and why: [design.md](design.md).

## Concepts

| Concept | What it is |
| --- | --- |
| `Handle[T]` | 8-byte generational index into the store of `T`. Zero handle = "none". The loader-facing form; game code can use [concrete handles](#concrete-handles). |
| `Ref` | The untyped 8-byte payload of a handle. Concrete handle types embed it (`type SpriteHandle struct{ asset.Ref }`). |
| `Key` | `"path"` or `"path#sub"`: a slash-separated path in the server's `fs.FS`, plus an optional sub-asset name. Same key + type = same handle. |
| `Store[T]` | All assets of one type. `Get` never returns nil: zero, not ready, failed or stale handles give the placeholder. |
| `Server` | Owns stores, loaders, scopes and decode workers. |
| `Scope` | An owner of assets (global, a level, a screen). Assets nobody holds are unloaded by the next `Update`. |
| `Loader[T, D]` | `Decode` (worker goroutine, CPU only) then `Finish` (game goroutine: GPU objects, final values). |
| `BuiltinLoader` | Optional loader method `Builtin(key) bool`: keys it reports true for read no file (`Decode` gets nil data) and never hot reload. For built-in defaults that have a key but no file. Called on workers. |

The **game goroutine** is whichever goroutine calls `Update`, `Load`, `Get` and the scope methods. Everything except `Decode` runs there.

## Quick start

The [README](../README.md#quick-start) has a complete program. The shape of it:

```go
srv := asset.NewServer(asset.Options{
	FS:      os.DirFS("assets"), // or embed.FS in release builds
	Workers: 2,                  // 0 = decode inline in Update (tests)
	OnError: func(err error) { log.Print(err) },
})
defer srv.Close()
textures := asset.Register(srv, textureLoader{}, missingTexture) // placeholder

level := srv.NewScope("level1")
hero := asset.Load[Texture](level, "chars/hero.png") // returns at once

// every tick, before anything reads assets:
events := srv.Update()

tex := textures.Get(hero) // placeholder until Ready, never nil
```

## Concrete handles

`Handle[T]` is right inside loaders. Code that stores handles in many places (components, save data) is often better served by one concrete type per kind, with a small typed facade over the store:

```go
type SpriteHandle struct{ asset.Ref }

type SpriteStore struct{ s *asset.Store[Sprite] }

func SpriteStoreOf(srv *asset.Server) SpriteStore { return SpriteStore{asset.StoreOf[Sprite](srv)} }

func (st SpriteStore) Get(h SpriteHandle) *Sprite { return st.s.Get(asset.HandleOf[Sprite](h.Ref)) }

func LoadSprite(sc *asset.Scope, key string) SpriteHandle {
	return SpriteHandle{asset.Load[Sprite](sc, key).Ref()}
}
```

The only conversions (`Handle.Ref`, `asset.HandleOf[T]`) live in the facade; `HandleOf` does not check the type, so callers never convert by hand. The facade calls inline to the generic ones: `Get` costs the same and stays 0 allocs. Why this pattern exists: [design.md](design.md#handles).

## Loading

| Call | Behavior |
| --- | --- |
| `asset.Load[T](sc, key)` | Handle now, load in background. A map lookup: call once (at spawn, at init) and keep the handle. |
| `asset.Hold[T](sc, h)` | Make `sc` an owner of an existing handle. Stale handle panics. |
| `h.Ref()`, `asset.HandleOf[T](r)` | Convert between a typed handle and its untyped `Ref`. For store facades only: `HandleOf` does not check the type. |
| `asset.LoadSync[T](sc, key)` | Blocks until Ready (and its dependencies). For boot screens, tests, tools. Returns `(handle, error)`. |

## Scopes and lifetime

```go
level := srv.NewScope("level1")
asset.Load[Atlas](level, "level1/tiles.json")

next := srv.NewScope("level2")
asset.Load[Atlas](next, "level2/tiles.json")
// when next.Done():
level.Release() // assets nothing else holds unload on the next Update
```

| Method | |
| --- | --- |
| `srv.Global()` | Never released: fonts, UI art. |
| `sc.Release()` | Drop all holds. Release the old level only after the new scope is `Done`, so shared assets don't reload. |
| `sc.Progress()` | `(ready, total)` including dependencies, for a loading bar. |
| `sc.Done()` | Everything is `Ready` or `Failed`. |
| `sc.Err()` | Joined errors of failed assets. |

Handles stored in game state are **non-owning**. Dropping the last copy of a handle releases nothing; only scopes and parent assets own.

### Keep-alive

With `Options.KeepAlive` set, an asset that loses its last hold is **parked** instead of unloaded: it stays `Ready` and counts as *idle*. Going from level A to B and back to A then reloads nothing.

- **Revive:** `Load` or `Hold` of a parked key returns the same handle, `Version` is unchanged, no decode or `Finish` runs and no event is sent. Handles of parked assets keep resolving.
- **Evict:** after an `Update` the idle bytes never exceed the budget. The least recently released assets are unloaded first (`EventUnloaded`, handles go stale). An asset larger than the whole budget is unloaded in the same `Update`.
- **Dependencies count:** a parked atlas keeps its texture held, so the texture's bytes are idle too. When the atlas is evicted its dependencies inherit its age and go next. A dependency that a live scope also holds is not idle.
- **Never parked:** assets that are loading or `Failed` (a later `Load` tries again), and sub-assets, which go with their parent.
- **Sizes:** a loader may add `Size(v *T) int64` (a texture its GPU bytes, a sound its PCM). Others count the file length. `Stats` has `Bytes`, `IdleBytes` and `Parked`, and `TypeStats` the first two per type.
- **Limits:** assets that a loader holds through a scope of its own count as in use until the owning asset itself is evicted. With keep-alive on, code that outlives its scope sees no stale handle while the asset is parked: use `KeepAlive: 0` with `StrictHandles` to find such code. Hot reload also reloads parked assets.

How it works: [design.md](design.md#keep-alive).

## States and the store

`Missing`, `Queued`, `Decoding`, `Waiting`, `Ready`, `Failed`.

```go
v, state := store.Lookup(h) // tell the placeholder from the real thing
store.Ready(h)
store.Err(h)                // why it failed
store.Key(h)                // the key it was loaded from
for h, v := range store.All() { ... } // Ready assets, for debug tools
```

Pointers from `Get` stay valid until the next `Server.Update`.

## Dependencies and sub-assets

Keys with `#sub` name **sub-assets** (`"hero.atlas.json#idle_0"`). Loading one loads its parent; a held child keeps the parent alive.

```go
asset.RegisterChild[Atlas, Sprite](srv) // Sprite keys with #sub belong to an Atlas
```

A **dependency** is declared in `Decode` (`asset.Dep[T](ctx, "hero.png")`, relative to the asset's directory). The parent's `Finish` waits until it is `Ready`; if it fails, the parent fails.

## Options

| Option | |
| --- | --- |
| `FS` | file system to read from: `embed.FS`, `os.DirFS`, or [`httpfs`](#reading-assets-over-http) |
| `Workers` | decode goroutines; 0 = inline |
| `CommitBudget` | time cap for `Finish` calls per `Update` (at least one runs). `LoadSync` ignores it |
| `StrictHandles` | stale handle panics (`asset: stale handle`) instead of the placeholder. Debug builds |
| `OnError` | load failures; default logs |
| `WatchInterval` | [hot reload](#hot-reload): poll loaded files at most once per interval; 0 = off (no file-system calls besides loading) |
| `KeepAlive` | byte budget of released assets that stay loaded; 0 = unload at the next `Update`. See [Keep-alive](#keep-alive) |

## Events

`srv.Update()` returns `*asset.Events` (reused, valid until the next `Update`).

```go
for ev := range events.All() {
	switch ev.Kind {
	case asset.EventLoaded:   // Ready
	case asset.EventFailed:   // ev.Err
	case asset.EventModified: // hot reload replaced the value, same handle
	case asset.EventUnloaded: // handles are now stale
	}
}
for ev := range events.Of(asset.TypeIDOf[Sprite](srv)) { ... }
```

## Hot reload

Edit an asset file while the program runs: the asset changes on a following `Update`, under the same handle.

```go
srv := asset.NewServer(asset.Options{
	FS:            os.DirFS("assets"),
	WatchInterval: 500 * time.Millisecond, // development builds; 0 in release
})
```

- **Watching:** `Update` stats the file of every loaded asset at most once per interval (wall-clock). A different modtime or size reloads it. A file that is briefly missing (editors replace files on save) counts as unchanged. `embed.FS` never changes, so release builds never reload. Sub-assets (`"x.json#run"`) follow their parent's file.
- **Pushed changes:** a host that learns about changes itself (an editor's file watcher, a dev server) calls `srv.Changed(paths...)` instead of, or besides, polling. The next `Update` reloads every loaded top-level asset with one of those paths exactly as above (a Failed one loads again), with or without `WatchInterval`, and never stats the file. A path whose asset is still loading or reloading is kept until it can be acted on, so no change is lost; a path no loaded asset uses is dropped. Game goroutine only. For a browser build, [`httpfs.WatchChanges`](#change-events) turns a server's event stream into batches for `Changed`.
- **Atomic:** the new version is decoded and finished while the old one keeps serving `Get`. It replaces the value only if `Decode`, its dependencies and `Finish` all succeed: same handle, `Version` + 1, `EventModified`, the old value goes to the loader's `Unload`. Otherwise the old value stays and `asset: reload failed, keeping previous version: <key>: <err>` goes to `OnError`, once per file version.
- **Dependents follow:** assets that depend on a changed asset are finished again in the same `Update`: edit `hero.png` and the atlas re-cuts its sprites, into the same sprite handles. Each re-finished asset sends `EventModified`. Without `WatchInterval` nothing keeps Decode results, so after `Changed` a dependent is read from its own file again instead (same handle, `EventModified` once it is finished).
- **Sub-assets keep their handles.** Ones the new version no longer has switch to `Failed` (`asset: chars/hero.json no longer has Sprite "run_7"`) and resolve to the placeholder; if they come back, the same handle works again.
- **Recovery:** an asset that failed to load is loaded again when its file changes, and assets that failed only because of it are retried with it.
- **Loaders:** `Finish` may be called again with the same `D` (when a dependency changed), so it must not modify it.
- Code that derived data from an asset (a collision grid, cached images) rebuilds it on `EventModified` or when `store.Version(h)` changes.

Hot reload changes values, never handles. How it works: [design.md](design.md#hot-reload).

## Reading assets over HTTP

`httpfs` (`github.com/weakpixel/asset/httpfs`) is a read-only `fs.FS` over HTTP (standard library only), for a browser build that loads assets from its web server instead of embedding them, or for a host such as an editor webview that serves the workspace:

```go
fsys, err := httpfs.New("https://example.com/game/assets/", nil) // nil: http.DefaultClient
srv := asset.NewServer(asset.Options{FS: fsys, Workers: 2})
```

`ReadFile` and `Open` read the whole file with GET, `Stat` uses HEAD (`Content-Length`, `Last-Modified`). A 404 is `fs.ErrNotExist`, any other non-2xx status an error naming it, and a name `fs.ValidPath` rejects is `fs.ErrInvalid`. Names are escaped per segment, so spaces and `#` work. Directories cannot be listed. Each call blocks until the response is in, so keep decoding on workers (`Workers` ≥ 1). Don't set `WatchInterval` with it: polling would send a HEAD per loaded file from the game goroutine. Push changes with [`Server.Changed`](#hot-reload) instead, and have the server send `Cache-Control: no-cache` in development, so a re-read after a change is never served from the browser cache.

### Change events

`httpfs.WatchChanges(url)` returns a `<-chan []string` of changed keys read from a [server-sent event stream](https://html.spec.whatwg.org/multipage/server-sent-events.html) at `url`. The format a server sends:

```
data: {"paths": ["hero/hero.png", "levels/a.json"]}

```

Each event's `data` is a JSON object whose `paths` are keys relative to the asset root; one event is one batch, delivered in order. The reader runs on its own goroutine, reconnects with a capped backoff (250 ms to 5 s) when the connection drops, and logs and drops an event that does not parse or has no paths. The channel is never closed. Drain it on the game goroutine without blocking, once per tick, and pass each batch to `srv.Changed(batch...)`.

## Writing a loader

```go
type Level struct{ Name string }

type levelSpec struct {
	Name      string
	TilesPath string
	tiles     asset.DepRef[Atlas]
}

type levelLoader struct{}

// Decode runs on workers: CPU only, safe for concurrent use.
func (levelLoader) Decode(ctx *asset.DecodeContext, data []byte) (levelSpec, error) {
	var spec levelSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return spec, err
	}
	spec.tiles = asset.Dep[Atlas](ctx, spec.TilesPath)
	return spec, nil
}

// Finish runs on the game goroutine: create objects, resolve dependencies.
func (levelLoader) Finish(ctx *asset.FinishContext, s levelSpec) (Level, error) {
	atlas := asset.Resolve(ctx, s.tiles)
	_ = atlas
	return Level{Name: s.Name}, nil
}

func (levelLoader) Unload(v *Level) {} // release what Finish created

asset.Register[Level, levelSpec](srv, levelLoader{}, Level{Name: "missing"}) // last arg: placeholder
```

Finish can publish sub-assets with `asset.AddChild[C](ctx, "sub", value)` if `C` is registered as a child of the asset.

## Panics and errors

Programmer errors panic with `asset: ` (unregistered type, `bad key`, `Load` after `Close`, `cannot release the global scope`, `type already registered`). Bad files and failing loaders never panic: the asset goes to `Failed`.
