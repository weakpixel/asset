package asset

import (
	"fmt"
	"reflect"
)

// Loader turns file bytes into a T in two phases. D is the intermediate value
// Decode hands to Finish.
//
// Decode runs on a worker goroutine, possibly several at once, so it must be
// safe for concurrent use: CPU work only (image/png, encoding/json, audio
// decoders, font parsing). It must not make GPU calls and must not touch
// stores or the Server.
//
// Finish runs on the game goroutine inside [Server.Update], after every
// dependency declared in Decode is Ready: create images, cache sub-images,
// compile shaders, resolve dependency handles, publish sub-assets. Keep it
// short; heavy CPU work belongs in Decode.
//
// With hot reload on ([Options.WatchInterval]), Finish can run again with the
// same D when a dependency changes, so it must not modify d. Its result
// replaces the old value only if it succeeds.
//
// Unload releases what Finish created. It runs on the game goroutine when the
// asset is unloaded, for every Ready value at [Server.Close], and for the old
// value after a hot reload replaced it.
type Loader[T, D any] interface {
	Decode(ctx *DecodeContext, data []byte) (D, error)
	Finish(ctx *FinishContext, d D) (T, error)
	Unload(v *T)
}

// BuiltinLoader is an optional method of a [Loader]: Builtin reports whether
// key names a value built into the loader (a starter voice by its name), not
// a file. For such a key the server reads nothing and calls Decode with nil
// data; hot reload never sees it change. Builtin runs on worker goroutines,
// so it must be safe for concurrent use, and must answer the same for a key
// every time.
type BuiltinLoader interface {
	Builtin(key Key) bool
}

// DecodeContext is handed to Decode. It is the only way a loader declares
// dependencies (atlas -> texture, tile map -> tileset, animation -> atlas). It is
// valid only during the Decode call.
type DecodeContext struct {
	// Key is the key being loaded.
	Key Key

	deps []depReq
}

// depReq is a dependency as declared in Decode. It is resolved later on the
// game goroutine, so a bad path in a data file becomes a load error instead of
// a panic on a worker.
type depReq struct {
	typ  reflect.Type
	path string
}

// DepRef names a dependency declared in Decode. Hand it to [Resolve] in
// Finish. The zero DepRef is invalid.
type DepRef[T any] struct{ n int } // 1-based index into the declared deps

// Dep declares that the asset being decoded needs a T at path. A relative
// path resolves against the directory of ctx.Key.Path ("hero.png" next to
// "chars/hero.json" is "chars/hero.png"); it may carry a "#sub" suffix.
//
// The server loads it with a hold owned by the parent and delays the parent's
// Finish until it is Ready. If it fails, the parent fails too, without Finish.
// A dependency cycle fails the asset that closes it (see [Server.Update]).
//
// T must be registered by the time the result is applied, or Update panics
// with "asset: type not registered".
func Dep[T any](ctx *DecodeContext, path string) DepRef[T] {
	ctx.deps = append(ctx.deps, depReq{typ: reflect.TypeFor[T](), path: path})
	return DepRef[T]{n: len(ctx.deps)}
}

// FinishContext is handed to Finish. It is valid only during the Finish call.
type FinishContext struct {
	// Key is the key being finished.
	Key Key

	srv  *Server
	id   ID
	deps []ID

	// reload is set when Finish replaces a Ready value (hot reload). AddChild
	// then stages children instead of publishing them, so a failing Finish
	// leaves the old children untouched.
	reload bool
	staged []stagedChild
}

// stagedChild is a sub-asset published by a reloading Finish, applied only
// if the Finish succeeds.
type stagedChild struct {
	id    ID
	fresh bool        // slot allocated by this Finish; freed if it fails
	apply func() bool // stores the value and makes it Ready; reports whether it was Ready
}

// Resolve turns a DepRef from Decode into a Ready handle. The parent keeps its
// hold on the dependency until the parent is unloaded, so the handle stays
// valid as long as the asset being finished does.
//
// It panics with "asset: Resolve" if d is the zero DepRef or its type does not
// match the dependency.
func Resolve[T any](ctx *FinishContext, d DepRef[T]) Handle[T] {
	if d.n < 1 || d.n > len(ctx.deps) {
		panic("asset: Resolve: invalid DepRef")
	}
	st := storeFor[T](ctx.srv)
	id := ctx.deps[d.n-1]
	if id.Type != st.tid {
		panic(fmt.Sprintf("asset: Resolve: DepRef of %s does not match dependency of type %s", st.typ, ctx.srv.list[id.Type-1].name()))
	}
	return Handle[T]{index: id.Index, gen: id.Gen}
}

// AddChild publishes the sub-asset "path#sub" of the asset being finished and
// returns its handle. The child type C must be registered with
// [RegisterChild] for the parent's type.
//
// If a handle for that key was handed out earlier (Load of "path#sub" before
// the parent was ready), the same slot is filled, so existing handles stay
// valid. A child lives as long as its parent and is unloaded with it.
//
// During a hot reload the child's existing slot gets the new value (same
// handle, Version + 1) once the parent's Finish succeeds; children the new
// version does not add switch to Failed.
//
// It panics with "asset: AddChild" if C is not a registered child of the asset
// being finished, if sub is empty, or if the sub-asset was already added.
func AddChild[C any](ctx *FinishContext, sub string, v C) Handle[C] {
	s := ctx.srv
	cs := storeFor[C](s)
	if sub == "" {
		panic("asset: AddChild: empty sub name")
	}
	if parent, ok := s.childOf[cs.tid]; !ok || parent != ctx.id.Type {
		panic(fmt.Sprintf("asset: AddChild: %s is not a registered child of %s", cs.typ, s.list[ctx.id.Type-1].name()))
	}
	key := Key{Path: ctx.Key.Path, Sub: sub}
	if ctx.reload {
		return stageChild(ctx, cs, key, v)
	}
	i, reserved := cs.lookupKey(key)
	if reserved {
		if cs.stateAt(i) != Waiting {
			panic(fmt.Sprintf("asset: AddChild: %s %s was already added", cs.typ, key))
		}
	} else {
		i = cs.alloc(key, Waiting)
		cs.coldAt(i).owner = ctx.id
		pc := s.coldOf(ctx.id)
		pc.children = append(pc.children, ID{Type: cs.tid, Index: i, Gen: cs.genAt(i)})
	}
	cs.slots[i].val = v
	cs.setState(i, Ready)
	cs.coldAt(i).version = 1
	s.changes++
	s.emit(EventLoaded, ID{Type: cs.tid, Index: i, Gen: cs.genAt(i)}, key, nil)
	return Handle[C]{index: i, gen: cs.genAt(i)}
}

// stageChild is AddChild during a hot reload: the value is applied after the
// parent's Finish succeeded, into the child's existing slot if it has one.
func stageChild[C any](ctx *FinishContext, cs *Store[C], key Key, v C) Handle[C] {
	s := ctx.srv
	i, exists := cs.lookupKey(key)
	if exists {
		id := ID{Type: cs.tid, Index: i, Gen: cs.genAt(i)}
		for _, sc := range ctx.staged {
			if sc.id == id {
				panic(fmt.Sprintf("asset: AddChild: %s %s was already added", cs.typ, key))
			}
		}
	} else {
		i = cs.alloc(key, Waiting)
		cs.coldAt(i).owner = ctx.id
		pc := s.coldOf(ctx.id)
		pc.children = append(pc.children, ID{Type: cs.tid, Index: i, Gen: cs.genAt(i)})
	}
	id := ID{Type: cs.tid, Index: i, Gen: cs.genAt(i)}
	ctx.staged = append(ctx.staged, stagedChild{id: id, fresh: !exists, apply: func() bool {
		sl := &cs.slots[i] // indexed now: the slice may have grown since staging
		wasReady := sl.state == Ready
		if wasReady {
			old := sl.val
			s.retire(func() { cs.unl(&old) })
		}
		sl.val = v
		sl.state = Ready
		return wasReady
	}})
	return Handle[C]{index: i, gen: cs.genAt(i)}
}
