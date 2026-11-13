package asset

import (
	"fmt"
	"io/fs"
	"slices"
)

// Hot reload. Update polls the files of loaded assets (poll); a changed file
// is decoded again into a candidate while the slot keeps serving the old value
// (reload, applyReload), and the candidate replaces the value only if its
// dependencies are Ready and Finish succeeds (commitReload, refinish). A
// replaced asset re-finishes the assets that depend on it through the reverse
// edges in cold.dependents (cascade), and assets that failed only because of
// it are loaded again (retry).

// stamp is the version of a file as fs.Stat reports it. It is comparable; the
// zero stamp means "unknown" (not watching, or the file could not be stat'ed).
type stamp struct {
	mod  int64 // ModTime in Unix nanoseconds
	size int64
	ok   bool
}

func statStamp(fsys fs.FS, name string) stamp {
	fi, err := fs.Stat(fsys, name)
	if err != nil {
		return stamp{}
	}
	return stamp{mod: fi.ModTime().UnixNano(), size: fi.Size(), ok: true}
}

// candidate is a new version of a Ready asset on its way through the pipeline.
type candidate struct {
	data    any  // Decode result, once decoded
	deps    []ID // the new version's dependencies, held for it
	decoded bool // applied by applyReload; the asset is in Server.waiting
	fileLen int64
}

func (s *Server) watching() bool { return s.opts.WatchInterval > 0 }

// poll stats the file of every loaded top-level asset, at most once per
// WatchInterval of wall-clock time. A Ready asset whose file changed is
// reloaded; a Failed one is loaded again. Sub-assets are not polled: their
// parent republishes them. A file that cannot be stat'ed (missing while an
// editor replaces it) counts as unchanged.
func (s *Server) poll() {
	if !s.watching() || s.opts.FS == nil {
		return
	}
	now := s.now()
	if s.polled && now.Sub(s.lastPoll) < s.opts.WatchInterval {
		return
	}
	s.polled, s.lastPoll = true, now
	for _, st := range s.list {
		for i := uint32(1); i < st.slotCount(); i++ {
			state := st.stateAt(i)
			if state != Ready && state != Failed {
				continue
			}
			c := st.coldAt(i)
			if c.owner.Type != 0 || c.cand != nil || (c.refs == 0 && !c.parked) {
				continue
			}
			cur := statStamp(s.opts.FS, c.key.Path)
			if !cur.ok || cur == c.stamp {
				continue
			}
			id := ID{Type: st.typeID(), Index: i, Gen: st.genAt(i)}
			if state == Ready {
				s.reload(st, c, id)
			} else {
				s.retry(id)
			}
		}
	}
}

// Changed reports that the files at paths changed, for hosts that learn about
// changes themselves (an editor's file watcher) instead of polling. The next
// Update reloads every loaded top-level asset whose key has one of these
// paths, exactly as polling does when a file's stamp changes: a Ready asset
// is decoded again and swapped in place (EventModified; on failure the old
// value stays), a Failed one is loaded again. It works whether or not
// Options.WatchInterval is set, and never stats the file.
//
// A path whose asset is still loading or already reloading stays recorded
// until an Update can act on it, so a change is never lost. A path no loaded
// asset uses is dropped. Sub-assets are not reloaded directly: their parent
// republishes them. Call it on the game goroutine.
func (s *Server) Changed(paths ...string) {
	if s.changed == nil {
		s.changed = make(map[string]struct{}, len(paths))
	}
	for _, p := range paths {
		s.changed[p] = struct{}{}
	}
}

// reloadChanged acts on the paths recorded by Changed.
func (s *Server) reloadChanged() {
	if len(s.changed) == 0 {
		return
	}
	busy := make(map[string]bool, len(s.changed))
	for _, st := range s.list {
		for i := uint32(1); i < st.slotCount(); i++ {
			c := st.coldAt(i)
			if c == nil || c.owner.Type != 0 || (c.refs == 0 && !c.parked) {
				continue
			}
			if _, ok := s.changed[c.key.Path]; !ok {
				continue
			}
			state := st.stateAt(i)
			id := ID{Type: st.typeID(), Index: i, Gen: st.genAt(i)}
			switch {
			case c.cand != nil || (state != Ready && state != Failed):
				busy[c.key.Path] = true // act once this load or reload settles
			case state == Ready:
				s.reload(st, c, id)
			default:
				s.retry(id)
			}
		}
	}
	for p := range s.changed {
		if !busy[p] {
			delete(s.changed, p)
		}
	}
}

// reload queues a decode of a Ready asset's file into a candidate.
func (s *Server) reload(st anyStore, c *cold, id ID) {
	c.cand = &candidate{}
	s.pending = append(s.pending, job{id: id, key: c.key, st: st, reload: true})
	s.changes++
}

// retry loads a Failed asset again, in place: same slot, same handles. The
// sub-assets that failed with it wait for the new attempt. Its old
// dependencies stay held until the new attempt has declared its own (see
// dropStaleDeps).
func (s *Server) retry(id ID) {
	st, c, _ := s.live(id)
	c.err = nil
	c.depFailed = false
	c.staleDeps = append(c.staleDeps, c.deps...)
	c.deps = nil
	for _, ch := range c.children {
		if cst, cc, ok := s.live(ch); ok && cst.stateAt(ch.Index) == Failed {
			cc.err = nil
			cst.setState(ch.Index, Waiting)
		}
	}
	st.setState(id.Index, Queued)
	s.pending = append(s.pending, job{id: id, key: c.key, st: st})
	s.changes++
}

func (s *Server) dropStaleDeps(id ID, c *cold) {
	if len(c.staleDeps) > 0 {
		s.dropDeps(id, c.staleDeps)
		c.staleDeps = nil
	}
}

// applyReload takes a candidate's decode result: it loads the declared
// dependencies for the candidate and queues it for commit, or reports the
// failure. The stamp is recorded either way, so a bad version is reported
// once and retried only when the file changes again.
func (s *Server) applyReload(st anyStore, c *cold, r result) {
	cand := c.cand
	if cand == nil || cand.decoded {
		return
	}
	c.stamp = r.stamp
	if r.err != nil {
		s.reloadFailed(c, r.err)
		c.cand = nil
		return
	}
	for _, dr := range r.deps {
		dep, err := s.addDep(r.id, dr)
		if err != nil {
			s.reloadFailed(c, err)
			s.dropCandidate(r.id, c)
			return
		}
		cand.deps = append(cand.deps, dep)
	}
	cand.data, cand.decoded, cand.fileLen = r.data, true, r.size
	s.waiting = append(s.waiting, r.id)
	s.changes++
}

func (s *Server) dropCandidate(id ID, c *cold) {
	s.dropDeps(id, c.cand.deps)
	c.cand = nil
}

// commitReload finishes a candidate whose dependencies are Ready and, on
// success, makes its dependencies the asset's own.
func (s *Server) commitReload(id ID, c *cold) {
	cand := c.cand
	c.cand = nil
	if err := s.refinish(id, cand.data, cand.deps, cand.fileLen); err != nil {
		s.reloadFailed(c, err)
		s.dropDeps(id, cand.deps)
		return
	}
	c.fileLen = cand.fileLen
	old := c.deps
	c.deps = cand.deps
	s.dropDeps(id, old)
	s.keep(c, cand.data)
}

// refinish runs Finish for a Ready asset and swaps in the result: same slot,
// same handle, Version + 1, EventModified. Sub-assets the new version adds
// get their existing slots; the ones it no longer adds fail. Then the assets
// that depend on this one are re-finished too. On error nothing changes.
func (s *Server) refinish(id ID, d any, deps []ID, fileLen int64) error {
	st, c, _ := s.live(id)
	ctx := FinishContext{Key: c.key, srv: s, id: id, deps: deps, reload: true}
	if err := st.refinish(&ctx, id.Index, d); err != nil {
		for _, sc := range ctx.staged {
			if sc.fresh {
				c.children = slices.DeleteFunc(c.children, func(ch ID) bool { return ch == sc.id })
				s.list[sc.id.Type-1].freeSlot(sc.id.Index)
			}
		}
		return err
	}
	c.version++
	c.bytes = st.measure(id.Index, fileLen)
	s.dirty = true
	s.changes++
	s.emit(EventModified, id, c.key, nil)

	for _, sc := range ctx.staged {
		cc := s.coldOf(sc.id)
		kind := EventLoaded
		if sc.apply() {
			kind = EventModified
		}
		cc.err = nil
		cc.version++
		s.emit(kind, sc.id, cc.key, nil)
	}
	for _, ch := range c.children {
		cst, cc, ok := s.live(ch)
		if !ok || slices.ContainsFunc(ctx.staged, func(sc stagedChild) bool { return sc.id == ch }) {
			continue
		}
		if state := cst.stateAt(ch.Index); state == Ready || state == Waiting {
			s.fail(ch, removedChildError(cst, cc.key))
		}
	}
	s.cascade(id, true)
	return nil
}

func removedChildError(st anyStore, key Key) error {
	return fmt.Errorf("asset: %s no longer has %s %q", key.Path, st.shortName(), key.Sub)
}

// cascade brings the dependents of a replaced or recovered asset up to date:
// with refinish, Ready ones are re-finished with their kept Decode result (an
// atlas re-cuts its sprites from the new texture) or, when they kept none,
// reloaded from their file; ones that failed because a dependency failed
// are loaded again.
func (s *Server) cascade(id ID, refinish bool) {
	dependents := slices.Clone(s.coldOf(id).dependents) // re-finishing edits the edges
	for i, p := range dependents {
		if slices.Contains(dependents[:i], p) {
			continue // declared the same dependency twice
		}
		pst, pc, ok := s.live(p)
		if !ok {
			continue
		}
		switch pst.stateAt(p.Index) {
		case Ready:
			switch {
			case !refinish:
			case pc.hasKept:
				if err := s.refinish(p, pc.kept, pc.deps, pc.fileLen); err != nil {
					s.reloadFailed(pc, err)
				}
			case pc.cand == nil && pc.owner.Type == 0:
				// Nothing kept (WatchInterval is off and the change came
				// through Changed): read the dependent's file again. Its
				// Finish runs once its dependencies are Ready, so it sees
				// the replaced value.
				s.reload(pst, pc, p)
			}
		case Failed:
			if pc.depFailed {
				s.retry(p)
			}
		}
	}
}

// keep stores an asset's Decode result for re-finishing after a dependency
// changed. Only assets with dependencies can be re-finished that way, so
// assets without (textures and their pixels) keep nothing.
func (s *Server) keep(c *cold, d any) {
	if s.watching() && len(c.deps) > 0 {
		c.kept, c.hasKept = d, true
	} else {
		c.kept, c.hasKept = nil, false
	}
}

// reloadFailed reports a failed hot reload. The asset keeps its value.
func (s *Server) reloadFailed(c *cold, err error) {
	s.changes++
	s.opts.OnError(fmt.Errorf("asset: reload failed, keeping previous version: %s: %w", c.key, err))
}

// retire schedules the Unload of a value a reload replaced. It runs after
// the commit step, once dependents were re-finished, so they never hold a
// released object between two Updates.
func (s *Server) retire(f func()) { s.retired = append(s.retired, f) }

func (s *Server) runRetired() {
	for i, f := range s.retired {
		f()
		s.retired[i] = nil
	}
	s.retired = s.retired[:0]
}
