// Package asset is a handle-based asset system: typed stores, background
// loading, scopes with reference counts and dependency tracking.
//
// It uses the standard library only and knows no graphics or audio API, so it
// is testable with fstest.MapFS and fake loaders, without a GPU. Asset types
// that need a GPU or a decoder (textures, sprites, fonts, sounds) live in
// your own packages and plug in through [Loader].
//
// # Model
//
// Components hold a [Handle], an 8-byte generational index into a [Store] of
// one asset type. [Load] returns a handle immediately and queues the work; the
// asset becomes ready later. [Store.Get] never returns nil: handles that are
// zero, not ready, failed or stale resolve to the type's placeholder.
//
// A [Loader] has two phases. Decode (file bytes to a plain value) runs on
// worker goroutines and must not touch stores. Finish (create GPU objects,
// resolve dependencies, publish sub-assets) runs on the game goroutine inside
// [Server.Update], optionally under a time budget.
//
// Lifetime is by [Scope]: a scope holds the assets it loaded, and assets
// nobody holds are unloaded by the next [Server.Update]. Handles inside
// components are non-owning.
//
// # Goroutines
//
// Everything except Decode runs on one goroutine, the game goroutine: Load,
// Update, Get, scopes, Close. Decode may run on several workers at once, so a
// Loader's Decode must be safe for concurrent use.
//
// # Panics and errors
//
// Programmer errors panic with an "asset: " prefix (unregistered type, bad
// key, Load after Close, strict-mode stale handle). Bad files and failing
// loaders are not programmer errors: they put the asset in state [Failed],
// are reported through [Options.OnError] and never panic.
package asset
