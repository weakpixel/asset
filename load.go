package asset

import (
	"errors"
	"fmt"
	"path"
	"slices"
)

// Load returns the handle for key and adds a hold for sc. It never blocks: a
// new key gets a slot in state Queued and a job; a known key returns the
// existing handle in whatever state it is in. It is idempotent per (T, key)
// and per scope, and it does not retry an asset that already Failed.
//
// Load is a map lookup on a string: call it at spawn time or in Init, keep the
// handle, and never call it per entity per tick. Don't call it from Draw.
//
// If T is registered as a child type and key has a "#sub", the parent is
// loaded too and the child's slot is reserved in state Waiting until the
// parent's Finish adds it (see [RegisterChild]).
//
// It panics with "asset: bad key" for a malformed key (see [ParseKey]), with
// "asset: type not registered" for an unregistered T, and with
// "asset: server closed" after Close.
func Load[T any](sc *Scope, key string) Handle[T] {
	s := sc.srv
	s.mustBeOpen()
	st := storeFor[T](s)
	id := s.loadKey(st, ParseKey(key))
	sc.hold(id)
	return Handle[T]{index: id.Index, gen: id.Gen}
}

// Hold adds sc as an owner of an existing handle: an entity spawned at
// runtime that should keep its art alive after the level that loaded it is
// released. The zero handle is ignored.
//
// Holding a stale handle is a programmer error: it panics with "asset: stale
// handle" in strict mode and is ignored (and counted) otherwise.
func Hold[T any](sc *Scope, h Handle[T]) {
	s := sc.srv
	s.mustBeOpen()
	if h.IsZero() {
		return
	}
	st := storeFor[T](s)
	if st.liveCold(h.index, h.gen) == nil {
		st.stale(h)
		return
	}
	sc.hold(st.ID(h))
}

// LoadSync is Load followed by driving the server until the asset (and its
// dependencies) is Ready or Failed, ignoring CommitBudget. It returns the
// handle and the asset's error, if it failed.
//
// It blocks the game loop. Use it for boot screens, tests and tools, never in
// a running level. Events produced meanwhile are delivered by the next
// Update.
func LoadSync[T any](sc *Scope, key string) (Handle[T], error) {
	s := sc.srv
	h := Load[T](sc, key)
	st := storeFor[T](s)
	s.syncing++
	defer func() { s.syncing-- }()

	settled := func() bool {
		state := st.stateAt(h.index)
		return state == Ready || state == Failed
	}
	for !settled() {
		before := s.changes
		s.tick()
		if settled() {
			break
		}
		if s.changes != before {
			continue
		}
		if s.inflight == 0 {
			return h, fmt.Errorf("asset: LoadSync stalled on %s %s: %s", st.typ, st.coldAt(h.index).key, st.stateAt(h.index))
		}
		s.waitForResult()
	}
	return h, st.Err(h)
}

// loadKey finds or creates the slot for (st, key) and returns its ID. It adds
// no hold; the caller does.
func (s *Server) loadKey(st anyStore, key Key) ID {
	if i, ok := st.lookupKey(key); ok {
		return ID{Type: st.typeID(), Index: i, Gen: st.genAt(i)}
	}
	if parent, isChild := s.childOf[st.typeID()]; isChild && key.Sub != "" {
		return s.reserveChild(st, s.list[parent-1], key)
	}
	i := st.alloc(key, Queued)
	id := ID{Type: st.typeID(), Index: i, Gen: st.genAt(i)}
	s.pending = append(s.pending, job{id: id, key: key, st: st})
	s.changes++
	return id
}

// reserveChild makes the slot for "path#sub" in state Waiting and loads the
// parent at "path". The parent's Finish fills the slot with AddChild. If the
// parent is already settled, the child is settled right away: a Ready parent
// without that child means it does not exist.
func (s *Server) reserveChild(st, parent anyStore, key Key) ID {
	pid := s.loadKey(parent, Key{Path: key.Path})
	i := st.alloc(key, Waiting)
	id := ID{Type: st.typeID(), Index: i, Gen: st.genAt(i)}
	st.coldAt(i).owner = pid
	pc := parent.coldAt(pid.Index)
	pc.children = append(pc.children, id)
	switch parent.stateAt(pid.Index) {
	case Ready:
		s.fail(id, noChildError(st, key))
	case Failed:
		s.fail(id, parentFailedError(st, key, pc))
	}
	s.changes++
	return id
}

func noChildError(st anyStore, key Key) error {
	return fmt.Errorf("asset: %s has no %s %q", key.Path, st.shortName(), key.Sub)
}

func parentFailedError(st anyStore, key Key, parent *cold) error {
	return fmt.Errorf("asset: %s %s: parent %s failed: %w", st.name(), key, parent.key, parent.err)
}

// addDep loads the dependency dr declared by parent, holds it for parent and
// records the reverse edge; the caller appends the returned ID to the
// parent's deps (or a reload candidate's). The dependency path resolves
// against the parent's directory. A bad path or a dependency cycle is returned
// as an error (it fails the parent), not a panic, because it comes from file
// contents.
func (s *Server) addDep(parent ID, dr depReq) (ID, error) {
	dst, ok := s.types[dr.typ]
	if !ok {
		panic("asset: type not registered: " + dr.typ.String())
	}
	pc := s.coldOf(parent)
	key, err := parseKey(path.Join(path.Dir(pc.key.Path), dr.path))
	if err != nil {
		return ID{}, err
	}
	dep := s.loadKey(dst, key)
	if chain := s.findPath(dep, parent); chain != nil {
		return ID{}, cycleError(s, append([]ID{parent}, chain...))
	}
	s.retain(dep)
	dc := s.coldOf(dep)
	dc.dependents = append(dc.dependents, parent)
	return dep, nil
}

// dropDeps undoes addDep for each of deps: it removes the reverse edge and
// releases the hold, which may queue the dependency for unloading.
func (s *Server) dropDeps(parent ID, deps []ID) {
	for _, d := range deps {
		dc := s.coldOf(d)
		if i := slices.Index(dc.dependents, parent); i >= 0 {
			dc.dependents = slices.Delete(dc.dependents, i, i+1)
		}
		s.release(d)
	}
}

// findPath returns the dependency chain from -> ... -> to, or nil if from
// does not reach to. A path from an asset to itself is [from].
func (s *Server) findPath(from, to ID) []ID {
	seen := make(map[ID]bool)
	var walk func(ID) []ID
	walk = func(id ID) []ID {
		if id == to {
			return []ID{id}
		}
		if seen[id] {
			return nil
		}
		seen[id] = true
		_, c, ok := s.live(id)
		if !ok {
			return nil
		}
		for _, d := range c.deps {
			if p := walk(d); p != nil {
				return append([]ID{id}, p...)
			}
		}
		return nil
	}
	return walk(from)
}

func cycleError(s *Server, chain []ID) error {
	msg := "asset: dependency cycle: "
	for i, id := range chain {
		if i > 0 {
			msg += " -> "
		}
		msg += s.coldOf(id).key.String()
	}
	return errors.New(msg)
}
