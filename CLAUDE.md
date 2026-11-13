# asset

A handle-based asset system for Go: module `github.com/weakpixel/asset`, Go 1.27, MIT. Package `asset` at the repository root (server, stores, handles, scopes, loaders, events, hot reload, keep-alive) and package `httpfs` (a read-only `fs.FS` over HTTP and a change-event stream reader).

## The docs are the spec

[doc/guide.md](doc/guide.md) is the user guide (public API and behavior) and [doc/design.md](doc/design.md) explains how it is built and why. Implement against the guide. Every topic has one home; link to it instead of repeating it.

- If the code and a doc disagree, don't silently pick one. Fix the code, or if the doc is wrong or underspecified, update the doc in the same change and say so.
- Behavior the docs leave open: decide, implement, test, and add it to the doc.
- Panic messages listed in the guide are API. Tests assert them with `strings.Contains`.

## Layout

| Path | What |
| --- | --- |
| `server.go`, `store.go`, `handle.go`, `key.go` | `Server`, `Options`, `Register`, `Store[T]`, `Handle[T]`, `Ref`, keys |
| `load.go`, `loader.go`, `worker.go`, `update.go` | `Load`, `Hold`, `LoadSync`, the `Loader` interface and contexts, decode workers, `Server.Update` |
| `scope.go`, `keepalive.go` | Scopes and ref counts, parking and eviction |
| `reload.go`, `events.go` | Hot reload (polling and `Changed`), events |
| `httpfs/` | `httpfs.New`, `WatchChanges` |
| `module-deps_test.go` | Enforces stdlib only and no `require` |

## Commands

```bash
make all          # fmt, vet, test -race
make wasm-test    # go vet and go test under GOOS=js GOARCH=wasm (Node)
go test ./...     # quick
make bench name=<label>       # benchmarks -> build/bench/<label>.txt
make benchcmp new=<label>     # benchstat vs build/bench/baseline.txt
```

While iterating, run only the tests of the packages you changed. Run `make all` and `make wasm-test` once before opening a PR; CI runs both and fails on a gofmt diff.

## Hard rules

- **Standard library only.** `go.mod` has no `require`, and no package imports anything outside the standard library and this module. `module-deps_test.go` enforces it.
- **The repository is self-contained.** Code, comments, docs, skills, commits and PRs describe this module on its own terms. Examples use plain asset types (a text, a texture, an atlas, a level) and a neutral host loop (a plain `for` loop, a tick, a test), not a particular framework or program that uses the module.
- **One game goroutine.** Everything except `Loader.Decode` runs on the goroutine that calls `Update`. `Decode` runs on workers, never touches a store, and must be safe for concurrent use.
- **Panics, not errors**, for programmer errors (unregistered type, bad key, `Load` after `Close`). Every message starts with `asset: `. Bad files and failing loaders are errors: the asset goes to `Failed`, the error goes to `OnError`, nothing panics. Collect all problems of one file before reporting them (`errors.Join`).
- **Hot paths are 0 allocs/op:** `Store.Get`, steady-state `Server.Update`, the keep-alive pass. Enforce this with `testing.AllocsPerRun` tests, not by eye.
- **Handles never change** for the life of an asset, including across hot reload and keep-alive revival.
- **Build outputs go into `build/`** (`go build -o build/<name>`, benchmark runs, profiles). `build/` is ignored. Never commit binaries or `*.test` files. Before every commit run `git status --short` and `git diff --cached --stat`, and stage files by name, not with `git add -A` or `git add .`.
- **Versions stay 0.** No tag or release without the maintainer's word, and no backward compatibility shims before a release.
- **No attribution and no names:** no Co-Authored-By, Claude-Session or "Generated with" lines in commits, PRs, docs or code; never mention a person's name in any document, commit or PR; no hidden HTML markers or comments anywhere.

## Code quality

- **Readability first.** Clear names, small functions, straightforward control flow. An optimization that makes code harder to follow must earn its place with a benchmark.
- **Model the domain; no master struct.** Each concept has its own type and file (store, scope, loader contexts, keep-alive, reload). `Server` composes them and delegates.
- **Document the public API.** Every exported identifier has a doc comment that states what it does, its contract (goroutine, pointer validity, cost) and which panics it can raise.
- **Explain performance tricks inline** (generation compares, reused event buffers, epochs instead of clearing, preallocation), so a reader does not "simplify" them away.

## Conventions

- File names are kebab-case: `module-deps_test.go`, `keepalive_test.go`.
- Tests use the standard `testing` package only (no testify), table-driven with `t.Run`, with `fstest.MapFS` and fake loaders; `Workers: 0` keeps them deterministic.
- Use modern Go: range-over-func, `iter.Seq2`, `for i := range n`, `reflect.TypeFor[T]()`, `slices`/`maps`.

## Workflow

Non-trivial work goes through `/plan` (writes one plan file, no code) and then `/implement` (carries it out step by step and keeps the plan file up to date). Small, obvious requests don't need a plan.

| Skill | Use when |
| --- | --- |
| `plan` | User runs `/plan` |
| `implement` | User runs `/implement` |
| `doc-writer` | Any documentation change |
