package asset

import (
	"io/fs"
	"log"
	"reflect"
	"sync"
	"time"
)

// Options configures a [Server].
type Options struct {
	// FS is the file system assets are read from: embed.FS in release builds,
	// os.DirFS("assets") in development. Workers read from it concurrently.
	FS fs.FS

	// Workers is the number of decode goroutines. 0 decodes inline inside
	// Update: deterministic, for tests and tools.
	Workers int

	// CommitBudget caps the time Update spends in Finish calls. At least one
	// Finish runs per Update, so loading always progresses. 0 means no cap.
	// [LoadSync] ignores the cap. Unloads are not capped.
	CommitBudget time.Duration

	// StrictHandles makes a stale handle panic with "asset: stale handle: ..."
	// instead of resolving to the placeholder. Meant for debug builds, to find
	// entities that outlive the scope that loaded their assets. The zero handle
	// and handles of assets that are not ready yet are never stale.
	StrictHandles bool

	// OnError receives load failures, from Update on the game goroutine. The
	// default logs with the standard logger.
	OnError func(err error)

	// WatchInterval turns on hot reload, for development builds. Update stats
	// the file of every loaded asset at most once per interval (wall-clock
	// time). A different modtime or size reloads it: the file is decoded and
	// finished again into a candidate, which replaces the value in the same
	// slot (same handle, [Store.Version] + 1, [EventModified]) only if both
	// steps succeed. A failed reload keeps the old value and is reported
	// through OnError once per file version ("asset: reload failed, keeping
	// previous version: ..."). Assets that depend on a replaced asset are
	// finished again; its sub-assets keep their handles, and the ones the new
	// version no longer has switch to Failed. A Failed asset is loaded again
	// when its file changes. A file that cannot be stat'ed counts as
	// unchanged, and an embed.FS never changes.
	//
	// 0 turns it off: no file-system calls besides loading.
	WatchInterval time.Duration

	// KeepAlive is the byte budget of released assets that stay loaded. When
	// a scope is released, assets that lose their last hold are parked
	// instead of unloaded; loading the same key again revives the asset with
	// the same handle and without decoding. Once the idle bytes (parked
	// assets and the dependencies only they hold) exceed the budget, the
	// least recently released are unloaded. Sizes come from the loader's
	// optional Size method, else from the file length (see [Stats]). 0 turns
	// keep-alive off: released assets unload at the next Update.
	//
	// A parked asset's handles stay valid, so with keep-alive on a stale
	// handle is noticed later, or never. Use 0 when hunting them with
	// StrictHandles.
	KeepAlive int64
}

// Server owns the stores, the loaders, the scopes and the worker goroutines.
// All methods except the workers' Decode calls run on the game goroutine.
type Server struct {
	opts Options
	now  func() time.Time // injectable for CommitBudget tests

	types   map[reflect.Type]anyStore
	list    []anyStore // list[TypeID-1]
	childOf map[TypeID]TypeID

	global *Scope

	pending  []job // Queued, not yet handed to a worker or decoded
	waiting  []ID  // decoded, in FIFO order, waiting for deps or budget
	zero     []ID  // slots whose refs dropped to 0; parked or freed by the next Update
	inflight int   // jobs sent to workers whose result is not applied yet

	jobs    chan job
	results chan result
	done    chan struct{} // closed by Close; releases blocked workers
	wg      sync.WaitGroup

	evs     []Event // events since the last Update returned
	events  Events  // what the last Update returned
	changes uint64  // bumped on every state change; lets LoadSync see progress
	syncing int     // >0 while LoadSync drives the server: no commit budget

	retired  []func()  // Unloads of values replaced by hot reload, run after commit
	lastPoll time.Time // when poll last stat'ed files
	polled   bool
	// changed are the paths reported by Changed that the next Update reloads.
	changed map[string]struct{}

	staleHits uint64
	closed    bool

	// Keep-alive state; see keepalive.go.
	lru       []lruEntry // parked assets, oldest first; stale entries are skipped
	parkedN   int        // assets with parked set
	parkSeq   uint64
	dirty     bool // holds or sizes changed since the last idle-bytes pass
	epoch     uint32
	markStack []ID
}

// NewServer creates a server and starts opts.Workers decode goroutines. Call
// [Server.Close] to stop them.
func NewServer(opts Options) *Server {
	if opts.OnError == nil {
		opts.OnError = func(err error) { log.Print(err) }
	}
	opts.Workers = max(opts.Workers, 0)
	s := &Server{
		opts:    opts,
		now:     time.Now,
		types:   make(map[reflect.Type]anyStore),
		childOf: make(map[TypeID]TypeID),
		done:    make(chan struct{}),
	}
	s.global = &Scope{srv: s, name: "global", global: true, holds: make(map[ID]struct{})}
	if opts.Workers > 0 {
		s.jobs = make(chan job, jobQueueSize)
		s.results = make(chan result, jobQueueSize)
		s.wg.Add(opts.Workers)
		for range opts.Workers {
			go s.work()
		}
	}
	return s
}

// Register adds the store and loader for T and returns the store. placeholder
// is what Get returns for missing, loading, failed and stale handles (a
// magenta checker sprite, a silent sound).
//
// It panics with "asset: type already registered: <T>" if T is registered.
// Register before the first Load of T.
func Register[T, D any](s *Server, l Loader[T, D], placeholder T) *Store[T] {
	rt := reflect.TypeFor[T]()
	if _, dup := s.types[rt]; dup {
		panic("asset: type already registered: " + rt.String())
	}
	st := newStore(s, TypeID(len(s.list)+1), placeholder)
	st.dec = func(ctx *DecodeContext, data []byte) (any, error) {
		d, err := l.Decode(ctx, data)
		return d, err
	}
	st.fin = func(ctx *FinishContext, d any) (T, error) {
		dv, _ := d.(D) // a nil interface D is a valid decode result
		return l.Finish(ctx, dv)
	}
	st.unl = l.Unload
	if sz, ok := any(l).(interface{ Size(*T) int64 }); ok {
		st.siz = sz.Size
	}
	if b, ok := any(l).(BuiltinLoader); ok {
		st.bin = b.Builtin
	}
	s.types[rt] = st
	s.list = append(s.list, st)
	return st
}

// RegisterChild says that "path#sub" keys for C are sub-assets of the P at
// "path" (a Sprite of an Atlas, a Clip of a ClipSet). Loading such a key loads
// the parent, reserves the child's slot right away, and the parent's Finish
// fills it with [AddChild]. Both types must be registered first.
//
// A C key without a '#sub' is an ordinary top-level asset of C.
//
// It panics with "asset: type not registered" for an unregistered type and
// with "asset: child type already registered" if C already has a parent.
func RegisterChild[P, C any](s *Server) {
	ps, cs := storeFor[P](s), storeFor[C](s)
	if ps.tid == cs.tid {
		panic("asset: RegisterChild: a type cannot be its own parent: " + ps.typ.String())
	}
	if _, dup := s.childOf[cs.tid]; dup {
		panic("asset: child type already registered: " + cs.typ.String())
	}
	s.childOf[cs.tid] = ps.tid
}

// StoreOf returns T's store. It panics with "asset: type not registered: <T>".
func StoreOf[T any](s *Server) *Store[T] { return storeFor[T](s) }

// TypeIDOf returns T's TypeID, for filtering [Events]. It panics like
// [StoreOf].
func TypeIDOf[T any](s *Server) TypeID { return storeFor[T](s).tid }

func storeFor[T any](s *Server) *Store[T] {
	rt := reflect.TypeFor[T]()
	st, ok := s.types[rt]
	if !ok {
		panic("asset: type not registered: " + rt.String())
	}
	return st.(*Store[T])
}

// Global returns the scope that is never released. Fonts, the UI atlas and
// other assets that live as long as the game belong here.
func (s *Server) Global() *Scope { return s.global }

// Stats is a debug snapshot of the server.
type Stats struct {
	// States counts assets per State across all types (placeholders excluded).
	States [StateCount]int
	// Types is the same per type, in registration order.
	Types []TypeStats
	// Queued is the number of jobs not yet handed to a worker or decoded;
	// InFlight the number handed to workers whose result is not applied yet.
	Queued, InFlight int
	// StaleHits counts Store lookups with a stale handle (also in strict mode,
	// right before the panic).
	StaleHits uint64
	// Bytes is the approximate size of all Ready values; IdleBytes the part
	// that no live scope needs: parked assets and the dependencies only they
	// hold. Parked counts the assets kept alive by [Options.KeepAlive].
	Bytes, IdleBytes int64
	Parked           int
}

// TypeStats is the per-type part of [Stats].
type TypeStats struct {
	Type   TypeID
	Name   string
	States [StateCount]int
	// Bytes and IdleBytes are the per-type parts of [Stats.Bytes] and
	// [Stats.IdleBytes].
	Bytes, IdleBytes int64
}

// Stats returns the current counts. It allocates; use it for overlays and
// tests, not per entity.
func (s *Server) Stats() Stats {
	st := Stats{
		Queued:    len(s.pending),
		InFlight:  s.inflight,
		StaleHits: s.staleHits,
		Parked:    s.parkedN,
		Types:     make([]TypeStats, 0, len(s.list)),
	}
	s.markInUse()
	for _, a := range s.list {
		ts := TypeStats{Type: a.typeID(), Name: a.name()}
		a.countStates(&ts.States)
		ts.Bytes, ts.IdleBytes = s.bytesOf(a)
		st.Bytes += ts.Bytes
		st.IdleBytes += ts.IdleBytes
		for i, n := range ts.States {
			st.States[i] += n
		}
		st.Types = append(st.Types, ts)
	}
	return st
}

// Close stops the workers, waits for them, and unloads every asset (calling
// each loader's Unload on Ready values). All handles are stale afterwards.
// Load after Close panics with "asset: server closed"; Update, Release and a
// second Close are harmless no-ops. Pending decode work is dropped.
func (s *Server) Close() {
	if s.closed {
		return
	}
	s.closed = true
	close(s.done)
	s.wg.Wait()
	s.runRetired()
	for _, st := range s.list {
		st.closeAll()
	}
	s.pending, s.waiting, s.zero = nil, nil, nil
	s.lru, s.parkedN = nil, 0
	s.inflight = 0
}

func (s *Server) mustBeOpen() {
	if s.closed {
		panic("asset: server closed")
	}
}

// live resolves an ID to its store and bookkeeping; ok is false if the slot
// was freed or reused since the ID was issued.
func (s *Server) live(id ID) (st anyStore, c *cold, ok bool) {
	if id.Type == 0 || int(id.Type) > len(s.list) {
		return nil, nil, false
	}
	st = s.list[id.Type-1]
	c = st.liveCold(id.Index, id.Gen)
	return st, c, c != nil
}

// coldOf is live for IDs that must be live (edges the server itself retains).
func (s *Server) coldOf(id ID) *cold {
	return s.list[id.Type-1].coldAt(id.Index)
}

func (s *Server) emit(k EventKind, id ID, key Key, err error) {
	s.evs = append(s.evs, Event{Kind: k, ID: id, Key: key, Err: err})
}
