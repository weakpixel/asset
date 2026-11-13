package asset

import "errors"

// Scope owns asset holds: Global, a level, a UI screen, a cutscene. Loading
// through a scope adds a hold; releasing the scope drops all its holds, and
// assets without any remaining hold are unloaded by the next Update.
//
// Every asset has a reference count: the scopes that hold it, plus the parent
// assets that depend on it, plus one while any of its sub-assets is held. The
// count changes on load, release and dependency edges, never per entity.
type Scope struct {
	srv    *Server
	name   string
	global bool
	holds  map[ID]struct{}
	order  []ID // holds in load order, for deterministic Progress and Err
}

// NewScope creates a scope. The name is for debugging only.
func (s *Server) NewScope(name string) *Scope {
	return &Scope{srv: s, name: name, holds: make(map[ID]struct{})}
}

// Name returns the name given to NewScope.
func (sc *Scope) Name() string { return sc.name }

// Release drops every hold of sc. Assets with no remaining hold, from any
// scope or parent asset, are unloaded at the next Update, or parked when
// [Options.KeepAlive] is set. The scope can be reused afterwards. Handles of
// unloaded assets become stale.
//
// Release a level's scope only after the next level's scope is Done, so
// shared assets are not unloaded and reloaded.
//
// It panics with "asset: cannot release the global scope" for [Server.Global].
// After [Server.Close] it does nothing.
func (sc *Scope) Release() {
	if sc.global {
		panic("asset: cannot release the global scope")
	}
	if sc.srv.closed {
		return
	}
	for _, id := range sc.order {
		sc.srv.release(id)
	}
	clear(sc.holds)
	sc.order = sc.order[:0]
}

// hold adds one hold for sc on id, unless it already has one. A scope holds
// an asset at most once, which makes Load idempotent per scope.
func (sc *Scope) hold(id ID) {
	if _, ok := sc.holds[id]; ok {
		return
	}
	sc.holds[id] = struct{}{}
	sc.order = append(sc.order, id)
	sc.srv.retain(id)
}

// retain adds a reference. The first reference to a sub-asset also retains its
// parent, so a parent lives while any of its children is in use.
func (s *Server) retain(id ID) {
	c := s.coldOf(id)
	c.refs++
	s.dirty = true
	if c.refs == 1 {
		if c.parked {
			c.parked = false // revived: same slot, handle and value
			s.parkedN--
		}
		if c.owner.Type != 0 {
			s.retain(c.owner)
		}
	}
}

// release drops a reference. An asset that reaches 0 is queued for unloading,
// unless it is a sub-asset: those go away with their parent, which is
// released instead.
func (s *Server) release(id ID) {
	c := s.coldOf(id)
	c.refs--
	s.dirty = true
	if c.refs > 0 {
		return
	}
	if c.owner.Type != 0 {
		s.release(c.owner)
		return
	}
	s.zero = append(s.zero, id)
}

// closure lists everything sc needs: what it holds plus, transitively, the
// dependencies and parents of those. Depth-first, each asset once.
func (sc *Scope) closure() []ID {
	var out []ID
	seen := make(map[ID]struct{}, len(sc.order))
	var visit func(ID)
	visit = func(id ID) {
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		_, c, ok := sc.srv.live(id)
		if !ok {
			return
		}
		out = append(out, id)
		if c.owner.Type != 0 {
			visit(c.owner)
		}
		for _, d := range c.deps {
			visit(d)
		}
	}
	for _, id := range sc.order {
		visit(id)
	}
	return out
}

// Progress counts the assets sc needs, dependencies and parents included:
// how many are Ready and how many there are. Failed assets count in total
// only, so use Done to tell "finished with errors" from "still loading".
func (sc *Scope) Progress() (ready, total int) {
	for _, id := range sc.closure() {
		st, _, _ := sc.srv.live(id)
		if st.stateAt(id.Index) == Ready {
			ready++
		}
		total++
	}
	return ready, total
}

// Done reports whether every asset the scope needs is Ready or Failed. An
// empty scope is Done.
func (sc *Scope) Done() bool {
	for _, id := range sc.closure() {
		st, _, _ := sc.srv.live(id)
		if s := st.stateAt(id.Index); s != Ready && s != Failed {
			return false
		}
	}
	return true
}

// Err joins the errors of every Failed asset the scope needs, or returns nil.
// A parent that failed because of a dependency appears together with the
// dependency's own error.
func (sc *Scope) Err() error {
	var errs []error
	for _, id := range sc.closure() {
		st, c, _ := sc.srv.live(id)
		if st.stateAt(id.Index) == Failed {
			errs = append(errs, c.err)
		}
	}
	return errors.Join(errs...)
}
