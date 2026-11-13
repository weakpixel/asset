# Design

How the asset system is built, and why. The API is in the [guide](guide.md).

## Requirements

- Handles are **pointer-free**, so the structs that hold them (components, save data) stay cheap to scan for the garbage collector and compact in memory.
- Game state stays single-goroutine, and GPU objects are created on the game goroutine.
- `Get` per object per frame is **0 allocs** and cheap.
- An asset that is missing, still loading, failed or unloaded never crashes and never shows nothing by accident.
- Levels load in the background without hitching a frame.
- Authored data (levels, scenes) names assets by path; runtime data uses handles.

## Handles

| Option | Verdict | Why |
| --- | --- | --- |
| A pointer to the GPU object in game state | No | Pointers in every struct that holds one; can't express "still loading"; reload would need to patch every copy; not serializable |
| Path strings in game state | Not at run time | A map lookup per object per frame. Fine as authored data, resolved once at load |
| Dense index (`uint32`) | Almost | Cheap, but after an unload the slot is reused and old IDs silently point at another asset |
| **Generational handle** (index + generation, 8 bytes) | Yes | Same cost as an index plus one compare, stale handles are detected |

The zero handle means "none" and resolves to the placeholder in slot 0 of every store. A handle never changes for the life of its asset.

**`Ref` and concrete handles.** `Handle[T]` is generic, which is right inside the asset system (stores, loaders, `Dep`, `AddChild`). A program often wants one concrete handle type per kind instead (`type SpriteHandle struct{ asset.Ref }`, [guide](guide.md#concrete-handles)). The typical reason is package layering: the package that declares the structs holding handles cannot import the package that owns the asset type (because that one imports it), so with only `Handle[T]` each kind would need two handle types, one over a marker type and one over the real asset type, with conversions at every call site, and those conversions would compile between *any* two kinds. With `Ref`, there is one type per kind, and the only conversions (`Handle.Ref`, `HandleOf[T]`) live inside a store facade. The facade inlines to the generic calls: `Get` costs the same and stays 0 allocs.

## Stores

`Store[T]` is a slice of slots: value, state, generation, key, error, ref count. `Get(h)` is a bounds check, a generation compare and an index: no map, no lock, no allocation. Zero, not-ready, failed and stale handles return the placeholder (or panic with `asset: stale handle` under `Options.StrictHandles`, for debug builds). Pointers returned by `Get` stay valid until the next `Server.Update`.

## Loading: two phases

```
game goroutine                                         worker goroutines
Load[T](scope, key) -> slot (Queued), job ----------> read fs.FS, Loader.Decode (CPU only)
Server.Update (first thing in a tick): <------------- result
  drain results -> Waiting; declared Deps get loaded
  Finish (GPU objects, sub-assets) when deps are Ready, under CommitBudget
  unload slots nobody holds
  return the tick's Events
```

- `Load` only touches game-goroutine data: a map lookup on `(type, key)`, a free slot, a job on a channel. The handle is valid immediately, so the things that use it can exist before their art does.
- `Decode` never sees a store, a GPU object or game state, which keeps stores lock-free.
- `Finish` runs on the game goroutine, so GPU uploads and sub-image caching are ordinary ordered steps between ticks and never race with drawing. `CommitBudget` spreads a large level over several frames (at least one `Finish` runs per `Update`).
- `LoadSync` drives the same pipeline until the target settles: for boot screens, tests and tools, never in a running level.
- `Workers: 0` decodes inline in `Update`, which makes tests deterministic.

## Lifetime: scopes, not handles

Go has no destructors and handles are copied by value, so "the last struct using this asset was dropped" can't be detected cheaply, and per-handle acquire/release in gameplay code is easy to get wrong. So:

- **Owners are scopes and parent assets.** A slot's ref count is the number of scopes holding it plus the number of parents depending on it. Counting happens on load, release and dependency edges, never per handle copy.
- **Handles in game state are non-owning.** Releasing a level's scope while something still points at its sprite makes the handle stale: it resolves to the placeholder.
- Typical scopes: global (fonts, UI), one per level, one per transient screen. Loading level 2 before releasing level 1 keeps shared assets alive with no reload.

## Keys, dependencies, sub-assets

- The cache key is `(asset type, key)`; the same file can be loaded as two types. Keys are cleaned, slash-separated paths in the server's `fs.FS`; dependencies declared with relative paths resolve against the parent's directory.
- **Dependencies** (`asset.Dep` in `Decode`) are ref-counted edges: an atlas holds its texture, an animation set its atlas. A failed dependency fails the parent without running its `Finish`.
- **Sub-assets** (`"hero.json#run"`): loading a child key loads the parent; the child slot is reserved at once and filled by the parent's `AddChild`; a held child keeps its parent alive. A child type has exactly one parent type (`RegisterChild`).
- `embed.FS` in release builds, `os.DirFS` in development; an overlay or pack file system (mods, zip archives) is just another `fs.FS`.

## Hot reload

Development only, opt-in with `Options.WatchInterval` or pushed with `Server.Changed`. API and behavior: [guide](guide.md#hot-reload). The code is in `reload.go`.

- **Polling.** `Server.Update` stats the file of every Ready or Failed top-level asset at most once per interval. Sub-assets are not polled; their parent republishes them. The workers stat each file right before reading it, so the stamp (modtime, size) on the slot is the version that was decoded; stat before read means a save in between costs one extra reload, never a missed one. A file that cannot be stat'ed counts as unchanged (editors replace on save); `embed.FS` reports a constant stamp. The interval is wall-clock (`Server.now`, injectable in tests): `Update` has no tick delta.
- **Candidate.** A changed Ready asset gets a `candidate` and a decode job flagged `reload`. The slot stays Ready and serves the old value meanwhile; the job never touches the slot state. The candidate's dependencies are loaded and held for it, and it waits in the same FIFO as first loads, under the same `CommitBudget`. When they are Ready, `Finish` runs again (`FinishContext.reload`) and only its success swaps the value: same slot, same generation, `Version` + 1, `EventModified`. On success the candidate's dependencies become the asset's own and the old ones are released; on failure the candidate's are released. Either way the stamp of the decoded version is recorded, so a bad version is reported once (`asset: reload failed, keeping previous version: <key>: <err>`, through `OnError` only: no event, because the asset did not change state) and retried only when the file changes again.
- **Sub-assets.** A reloading `Finish` stages `AddChild` calls instead of publishing them. On success the staged values go into the children's existing slots (`EventModified`, or `EventLoaded` for a child that was missing or new); children the new version did not add switch to `Failed` (`asset: <path> no longer has <Type> "<sub>"`), so their handles show the placeholder instead of going stale; a removed child that comes back later is Ready again under the same handle. On failure the staged values are dropped and child slots allocated by that `Finish` are freed, so a half-run `Finish` changes nothing.
- **Dependents.** Every dependency edge has a reverse edge (`cold.dependents`, kept in step by `addDep` and `dropDeps`). After a swap, Ready dependents are finished again with their kept Decode result: an atlas re-cuts its sprites from the new texture into the same sprite slots, recursively. Only assets with dependencies keep their Decode result (`cold.kept`, and only while watching), so textures don't keep a CPU copy of their pixels. Loaders must therefore not modify `D` in `Finish`.
- **Old values** are unloaded after the commit step (`Server.retired`), once dependents were re-finished in the same `Update`, so a re-cut sprite never points at a deallocated texture. Dependent re-finishes are not budgeted for the same reason.
- **Pushed changes.** `Server.Changed(paths...)` records paths; the next `Update` (`reloadChanged`, right after `poll`) sends every matching top-level asset down the same `reload`/`retry` path polling uses, without a stat. A path whose asset has a load or a candidate in flight stays recorded for a later `Update` (so a save during a reload is not lost); the others are forgotten. It exists for editors that already watch files and for file systems where a synchronous `Stat` per asset per interval is too slow (HTTP in the browser, where a development server polls the files on its side and streams batches of keys, read by `httpfs.WatchChanges`).
- **Recovery.** A Failed top-level asset whose file changes is loaded again in place (`retry`): same handles, its failed children wait again, and its previous dependencies stay held until the new attempt has declared its own, so shared ones are not unloaded and reloaded in between. When it becomes Ready, dependents that failed only because of it (`cold.depFailed`) are retried too.
- **Why it's safe mid-game:** swaps happen inside `Update`, which runs before anything reads assets in a tick; code that keeps handles, not values, resolves them after the swap.

Known limits: an asset whose dependency is the same file (a whole-image sprite and its texture, a font and its font source) is modified twice for one save: once re-finished through the dependency, once by its own reload. If re-finishing a dependent fails (the new texture is too small for the atlas's frames), the dependent keeps its old value, whose sub-images point at the replaced texture, which was unloaded: it shows nothing until the file is fixed.

## Keep-alive

`Options.KeepAlive` is a byte budget for released assets (user view: [guide](guide.md#keep-alive)).

- **Parking, not a second store.** `unload` parks a Ready top-level asset with refs 0 (`cold.parked`) instead of freeing it. The slot, generation, value, key index, children and dependency holds stay, so `loadKey` finds it, the next `retain` revives it (same handle and `Version`, no events) and old handles keep resolving. Parking emits nothing; eviction frees through the normal `free`, so it emits `EventUnloaded`. Failed and unfinished assets are freed so a later `Load` retries, and sub-assets go with their parent.
- **Idle bytes include dependencies.** Without them the texture behind a parked atlas would be invisible to the budget. `markInUse` marks everything reachable from assets with a scope hold (`refs` minus dependency edges minus the one hold a held child puts on its parent) through `deps`, stale and candidate deps, and `owner`. Ready assets left unmarked are idle. The pass runs at the end of `Update` only when a hold or size changed (`Server.dirty`) and a parked asset exists, uses an epoch in `cold.mark` and a reused stack, and allocates nothing.
- **LRU by release order.** Parked assets sit in a slice, oldest first; entries carry a sequence number so a revived or re-parked asset leaves only stale entries behind, which are skipped and compacted. While idle bytes exceed the budget the oldest is freed. A dependency that this drops to refs 0 is parked at the *front*: it inherits the parent's age, so an old level goes before newer assets.
- **Sizes.** `Register` detects an optional `Size(*T) int64` on the loader; without it the file length from the decode result is used. The server stores the result in `cold.bytes` after `Finish` and after every reload commit. Sub-assets count 0.
- **Known limit.** Assets a loader holds through a scope of its own count as in use while the owning asset is parked, so they can exceed the budget until the owner is evicted and its `Unload` releases them.

## Not built yet

- **Watching beyond files.** Hot reload polls with `fs.Stat` or is pushed with `Server.Changed`; there is no built-in OS file-notification backend.
- **Multi-parent children.** `RegisterChild` gives a child type exactly one parent type, so only one asset type can publish children of a given type. Lifting it means keying child registration by parent type and key suffix.
- **Per-handle lifetime.** By design every asset belongs to scopes and parents; dropping a handle releases nothing.
