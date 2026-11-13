package asset

// TypeID identifies one asset type (one [Store]) inside a [Server]. The zero
// TypeID is invalid. IDs are handed out by [Register] in registration order.
type TypeID uint16

// ID is the untyped form of a [Handle]: the store it belongs to plus index and
// generation. It is comparable, so it works as a map key in events and debug
// tools. Get one with [Store.ID].
type ID struct {
	Type  TypeID
	Index uint32
	Gen   uint32
}

// State is where an asset is in its load pipeline.
type State uint8

const (
	// Missing is a zero or stale handle, or a slot that was never loaded.
	Missing State = iota
	// Queued waits for a worker (or for the next Update when Workers is 0).
	Queued
	// Decoding means Decode is running, or its result is not applied yet.
	Decoding
	// Waiting is decoded and waits for its dependencies or the commit budget.
	// A reserved sub-asset also waits here until its parent is finished.
	Waiting
	// Ready means the value is committed; Get returns it.
	Ready
	// Failed means Decode or Finish failed, or a dependency or the parent
	// failed. Get returns the placeholder; [Store.Err] says why.
	Failed
)

// StateCount is the number of States, for arrays indexed by State.
const StateCount = int(Failed) + 1

var stateNames = [StateCount]string{"missing", "queued", "decoding", "waiting", "ready", "failed"}

// String returns the lower-case state name.
func (s State) String() string {
	if int(s) < len(stateNames) {
		return stateNames[s]
	}
	return "invalid"
}

// Handle refers to one asset of type T in one [Server]. It is 8 bytes,
// comparable and pointer-free, so it can live in components, draw items and
// map keys.
//
// The zero Handle means "none" and resolves to the store's placeholder. A
// handle never changes for the life of the asset. The generation changes only
// when the slot is freed, which makes older handles stale.
//
// Handles are only meaningful inside the Server that issued them.
type Handle[T any] struct {
	index uint32 // slot in Store[T]; 0 is the placeholder slot
	gen   uint32 // bumped when the slot is freed; detects stale handles
}

// IsZero reports whether h is the "none" handle.
func (h Handle[T]) IsZero() bool { return h.index == 0 }

// Index returns the handle's slot index in its store. It is meant for sort
// keys (handles to the same asset share an index) and debugging; it is not an
// identity, because it ignores the generation. The zero handle returns 0.
func (h Handle[T]) Index() uint32 { return h.index }

// Ref is the untyped payload of a handle: slot index and generation, without
// the asset type. It is 8 bytes, comparable and pointer-free.
//
// Ref exists so packages can give an asset kind its own concrete handle type
// (a SpriteHandle defined as struct{ asset.Ref }) instead of exposing Handle[T]
// to game code. Such a package converts with [Handle.Ref] and [HandleOf] in
// one place, its store facade; game code never converts by hand.
//
// The zero Ref means "none", like the zero Handle.
type Ref struct {
	index uint32
	gen   uint32
}

// IsZero reports whether r is the "none" reference.
func (r Ref) IsZero() bool { return r.index == 0 }

// Index returns the slot index, with the same meaning as [Handle.Index].
func (r Ref) Index() uint32 { return r.index }

// Ref returns the untyped form of h. It is free (a field copy).
func (h Handle[T]) Ref() Ref { return Ref{index: h.index, gen: h.gen} }

// HandleOf returns r as a Handle[T]. It is the inverse of [Handle.Ref] and
// does not check that r came from a Handle[T]: a Ref of another type resolves
// to an unrelated slot or a stale handle. Use it only inside a store facade
// that owns the concrete handle type for T, never at game call sites.
func HandleOf[T any](r Ref) Handle[T] { return Handle[T]{index: r.index, gen: r.gen} }
