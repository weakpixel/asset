package asset

import (
	"fmt"
	"iter"
	"reflect"
)

// slot is one asset. gen and state sit next to the value because Get reads
// exactly those three; everything else lives behind c, which is allocated once
// per slot and has a stable address even when the slot slice grows.
type slot[T any] struct {
	val   T
	gen   uint32
	state State
	c     *cold
}

// cold is the per-asset bookkeeping that Get never touches.
type cold struct {
	refs     int32  // scope holds + parent-asset edges + externally held children
	key      Key    // kept after free, for stale-handle diagnostics
	err      error  // set when state is Failed
	version  uint32 // 1 after the first commit
	owner    ID     // parent asset of a sub-asset; Type 0 when none
	children []ID   // sub-assets owned by this asset
	deps     []ID   // assets this one needs, retained until it is freed
	decoded  any    // Decode result, until Finish consumes it

	// Hot reload bookkeeping; see watch.go and reload.go.
	dependents []ID       // reverse edges: assets that list this one in deps
	stamp      stamp      // file version seen by the last decode
	kept       any        // Decode result kept for re-finishing (watching, has deps)
	hasKept    bool       // kept is set (a nil D is a valid decode result)
	depFailed  bool       // Failed because a dependency failed: retried when it recovers
	staleDeps  []ID       // deps of a retried attempt, dropped once the new ones are held
	cand       *candidate // a reload in progress; the slot keeps its value meanwhile

	// Byte accounting and keep-alive; see keepalive.go.
	fileLen int64  // length of the file the current value was decoded from
	bytes   int64  // approximate size of the value; 0 for sub-assets
	parked  bool   // refs is 0 but the asset is kept in the LRU
	parkSeq uint64 // identifies the LRU entry of the latest park
	mark    uint32 // epoch of the last in-use mark pass
}

// Store holds every asset of one type. It is read and written on the game
// goroutine only; workers never touch it, so Get needs no lock.
//
// Create stores with [Register]; the zero Store is not usable.
type Store[T any] struct {
	srv   *Server
	tid   TypeID
	typ   reflect.Type
	slots []slot[T] // slots[0] is the placeholder, state Ready, gen 0
	free  []uint32
	byKey map[Key]uint32

	// Loader entry points with D erased. Set once by Register, then read-only,
	// so workers may call dec concurrently.
	dec func(*DecodeContext, []byte) (any, error)
	fin func(*FinishContext, any) (T, error)
	unl func(*T)
	siz func(*T) int64 // the loader's optional Size; nil without
	bin func(Key) bool // the loader's optional Builtin; nil without
}

func newStore[T any](srv *Server, tid TypeID, placeholder T) *Store[T] {
	return &Store[T]{
		srv:   srv,
		tid:   tid,
		typ:   reflect.TypeFor[T](),
		slots: []slot[T]{{val: placeholder, state: Ready, c: &cold{}}},
		byKey: make(map[Key]uint32),
	}
}

// Get returns the asset, or the placeholder if h is zero, stale, not ready or
// failed. It never returns nil and does not allocate.
//
// The pointer is valid until the next [Server.Update] (a commit may grow the
// slot slice) and must not be stored in components or closures; store the
// handle instead.
//
// A stale handle (its slot was freed or reused) counts in [Stats] and returns
// the placeholder, or panics with "asset: stale handle" when
// [Options.StrictHandles] is set.
func (s *Store[T]) Get(h Handle[T]) *T {
	// Fast path: one bounds check, one generation compare, one index. The
	// placeholder slot is Ready with gen 0, so the zero handle takes it too.
	if h.index < uint32(len(s.slots)) {
		sl := &s.slots[h.index]
		if sl.gen == h.gen && sl.state == Ready {
			return &sl.val
		}
	}
	return s.getSlow(h)
}

func (s *Store[T]) getSlow(h Handle[T]) *T {
	if sl, ok := s.find(h); ok && sl.state == Ready {
		return &sl.val
	}
	return &s.slots[0].val
}

// Lookup is Get plus the state, for code that must tell the placeholder from
// a real asset. For a zero handle the state is Missing, for a stale handle too
// (subject to the strict-mode panic described at [Store.Get]).
func (s *Store[T]) Lookup(h Handle[T]) (*T, State) {
	sl, ok := s.find(h)
	if !ok {
		return &s.slots[0].val, Missing
	}
	if sl.state == Ready {
		return &sl.val, Ready
	}
	return &s.slots[0].val, sl.state
}

// Ready reports whether h refers to a committed asset.
func (s *Store[T]) Ready(h Handle[T]) bool {
	_, st := s.Lookup(h)
	return st == Ready
}

// Err returns why the asset failed, or nil if it did not fail (including
// handles that are zero or stale).
func (s *Store[T]) Err(h Handle[T]) error {
	if sl, ok := s.find(h); ok && sl.state == Failed {
		return sl.c.err
	}
	return nil
}

// Key returns the key the asset was loaded from, or the zero Key for a zero or
// stale handle. For debugging and save files.
func (s *Store[T]) Key(h Handle[T]) Key {
	if sl, ok := s.find(h); ok {
		return sl.c.key
	}
	return Key{}
}

// Version is 0 until the asset is committed, 1 after the first commit, and
// goes up by one with every hot reload that replaced the value (see
// [Options.WatchInterval]). Caches of data derived from an asset compare it
// to notice changes.
func (s *Store[T]) Version(h Handle[T]) uint32 {
	if sl, ok := s.find(h); ok {
		return sl.c.version
	}
	return 0
}

// ID returns the untyped form of h, for events and debug tools.
func (s *Store[T]) ID(h Handle[T]) ID {
	return ID{Type: s.tid, Index: h.index, Gen: h.gen}
}

// Len is the number of loaded or loading assets, not counting the placeholder.
func (s *Store[T]) Len() int { return len(s.slots) - 1 - len(s.free) }

// All yields every Ready asset, for debug tools and editors. Pointers follow
// the validity rule of [Store.Get]. Don't load or release while iterating.
func (s *Store[T]) All() iter.Seq2[Handle[T], *T] {
	return func(yield func(Handle[T], *T) bool) {
		for i := 1; i < len(s.slots); i++ {
			sl := &s.slots[i]
			if sl.state != Ready {
				continue
			}
			if !yield(Handle[T]{index: uint32(i), gen: sl.gen}, &sl.val) {
				return
			}
		}
	}
}

// find resolves h to its live slot. The zero handle and any handle of another
// slot generation are not found; the latter is a stale handle and goes through
// [Store.stale].
func (s *Store[T]) find(h Handle[T]) (*slot[T], bool) {
	if h.index == 0 {
		return nil, false
	}
	if h.index < uint32(len(s.slots)) {
		sl := &s.slots[h.index]
		if sl.gen == h.gen && sl.state != Missing {
			return sl, true
		}
	}
	s.stale(h)
	return nil, false
}

// stale records a stale-handle hit and panics in strict mode.
func (s *Store[T]) stale(h Handle[T]) {
	s.srv.staleHits++
	if !s.srv.opts.StrictHandles {
		return
	}
	what := "unknown slot"
	if h.index < uint32(len(s.slots)) {
		sl := &s.slots[h.index]
		if sl.state == Missing {
			what = sl.c.key.String() // freed: the key it had
		} else {
			what = "slot reused by " + sl.c.key.String()
		}
	}
	panic(fmt.Sprintf("asset: stale handle: %s %s (index %d, gen %d)", s.typ, what, h.index, h.gen))
}

// anyStore is what Server needs from a Store[T] without knowing T.
type anyStore interface {
	typeID() TypeID
	name() string      // reflect type string, for panics: "sprites.Sprite"
	shortName() string // type name only, for load errors: "Sprite"

	lookupKey(Key) (uint32, bool)
	alloc(Key, State) uint32
	// liveCold returns the bookkeeping of slot i if its generation is gen and
	// it is in use, nil otherwise.
	liveCold(i, gen uint32) *cold
	coldAt(i uint32) *cold
	genAt(i uint32) uint32
	stateAt(i uint32) State
	setState(i uint32, s State)

	decode(ctx *DecodeContext, data []byte) (any, error)
	// builtin reports whether the loader serves key without a file.
	builtin(key Key) bool
	finish(ctx *FinishContext, i uint32, d any) error
	// refinish runs Finish again for a Ready slot and, on success, replaces
	// the value. The old value is unloaded after the reload settles.
	refinish(ctx *FinishContext, i uint32, d any) error
	slotCount() uint32
	// clearValue runs Unload on a Ready value and zeroes it. The state is left
	// to the caller.
	clearValue(i uint32)
	freeSlot(i uint32)
	closeAll()
	countStates(*[StateCount]int)
	// measure returns the size of Ready slot i: the loader's Size, or fileLen
	// when it has none.
	measure(i uint32, fileLen int64) int64
}

func (s *Store[T]) typeID() TypeID    { return s.tid }
func (s *Store[T]) name() string      { return s.typ.String() }
func (s *Store[T]) shortName() string { return s.typ.Name() }

func (s *Store[T]) lookupKey(k Key) (uint32, bool) {
	i, ok := s.byKey[k]
	return i, ok
}

// alloc takes a slot from the free list (keeping its bumped generation) or
// appends one, and indexes it under k.
func (s *Store[T]) alloc(k Key, st State) uint32 {
	var i uint32
	if n := len(s.free); n > 0 {
		i = s.free[n-1]
		s.free = s.free[:n-1]
		*s.slots[i].c = cold{key: k}
	} else {
		i = uint32(len(s.slots))
		s.slots = append(s.slots, slot[T]{c: &cold{key: k}})
	}
	s.slots[i].state = st
	s.byKey[k] = i
	return i
}

func (s *Store[T]) liveCold(i, gen uint32) *cold {
	if i == 0 || i >= uint32(len(s.slots)) {
		return nil
	}
	sl := &s.slots[i]
	if sl.gen != gen || sl.state == Missing {
		return nil
	}
	return sl.c
}

func (s *Store[T]) coldAt(i uint32) *cold       { return s.slots[i].c }
func (s *Store[T]) genAt(i uint32) uint32       { return s.slots[i].gen }
func (s *Store[T]) stateAt(i uint32) State      { return s.slots[i].state }
func (s *Store[T]) setState(i uint32, st State) { s.slots[i].state = st }
func (s *Store[T]) countStates(n *[StateCount]int) {
	for i := 1; i < len(s.slots); i++ {
		if st := s.slots[i].state; st != Missing {
			n[st]++
		}
	}
}

func (s *Store[T]) measure(i uint32, fileLen int64) int64 {
	if s.siz == nil {
		return fileLen
	}
	return s.siz(&s.slots[i].val)
}

func (s *Store[T]) decode(ctx *DecodeContext, data []byte) (any, error) {
	return s.dec(ctx, data)
}

func (s *Store[T]) builtin(key Key) bool { return s.bin != nil && s.bin(key) }

// finish runs the loader's Finish and, on success, stores the value. It
// indexes s.slots after the call because Finish may add sub-assets, which can
// grow the slice.
func (s *Store[T]) finish(ctx *FinishContext, i uint32, d any) error {
	v, err := s.fin(ctx, d)
	if err != nil {
		return err
	}
	s.slots[i].val = v
	return nil
}

func (s *Store[T]) refinish(ctx *FinishContext, i uint32, d any) error {
	v, err := s.fin(ctx, d)
	if err != nil {
		return err
	}
	sl := &s.slots[i]
	old := sl.val
	sl.val = v
	s.srv.retire(func() { s.unl(&old) })
	return nil
}

func (s *Store[T]) slotCount() uint32 { return uint32(len(s.slots)) }

func (s *Store[T]) clearValue(i uint32) {
	sl := &s.slots[i]
	if sl.state == Ready {
		s.unl(&sl.val)
	}
	var zero T
	sl.val = zero
}

// freeSlot unloads the value, bumps the generation (which makes every
// outstanding handle stale) and returns the slot to the free list. The key is
// kept in cold for stale-handle messages until the slot is reused.
func (s *Store[T]) freeSlot(i uint32) {
	sl := &s.slots[i]
	s.clearValue(i)
	delete(s.byKey, sl.c.key)
	sl.gen++
	sl.state = Missing
	*sl.c = cold{key: sl.c.key}
	s.free = append(s.free, i)
}

// closeAll unloads every Ready value and empties the store. Generations are
// bumped, so every handle is stale afterwards.
func (s *Store[T]) closeAll() {
	for i := 1; i < len(s.slots); i++ {
		if s.slots[i].state != Missing {
			s.freeSlot(uint32(i))
		}
	}
}
