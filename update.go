package asset

import "fmt"

// Update runs once per tick on the game goroutine, before any system that
// reads assets. In order it:
//
//  1. hands queued jobs to the workers (or decodes them inline when Workers
//     is 0) and applies finished decode results, loading declared
//     dependencies;
//  2. runs Finish for every decoded asset whose dependencies are Ready, FIFO,
//     until Options.CommitBudget is spent (at least one per call). An asset
//     whose dependency failed fails without Finish;
//  3. unloads assets whose reference count is 0, calling the loader's Unload,
//     bumping the slot generation and dropping their dependency holds. With
//     [Options.KeepAlive] they are parked instead, and only the least
//     recently released ones beyond the byte budget are unloaded;
//  4. returns the tick's events.
//
// With [Options.WatchInterval] set it first polls loaded files for changes
// and queues hot reloads; reloads go through the same steps 1 and 2 (see
// [Options.WatchInterval]). Paths reported with [Server.Changed] are reloaded
// the same way, with or without polling.
//
// The returned Events is reused: it is valid until the next Update. Pointers
// from Store.Get are valid until the next Update as well.
//
// It panics with "asset: type not registered" if a loader declared a
// dependency of an unregistered type. After Close it returns no events.
func (s *Server) Update() *Events {
	if !s.closed {
		s.poll()
		s.reloadChanged()
		s.tick()
	}
	// Swap buffers so a steady-state Update allocates nothing for events.
	s.events.list, s.evs = s.evs, s.events.list[:0]
	return &s.events
}

// tick is one pass of the pipeline. LoadSync calls it directly, so events it
// produces stay in s.evs for the next Update to return.
func (s *Server) tick() {
	s.dispatch()
	s.drain()
	s.dispatch() // dependencies queued by drain
	s.commit()
	s.runRetired()
	s.unload()
}

// applyResult moves a decoded asset to Waiting and loads its dependencies, or
// fails it. Results for assets unloaded meanwhile are dropped.
func (s *Server) applyResult(r result) {
	st, c, ok := s.live(r.id)
	if !ok {
		return
	}
	if r.reload {
		s.applyReload(st, c, r)
		return
	}
	if st.stateAt(r.id.Index) != Decoding {
		return
	}
	c.stamp = r.stamp
	c.fileLen = r.size
	// The deps of a retried attempt are dropped only now, after the new
	// attempt holds its own, so shared dependencies are not unloaded and
	// loaded again in between.
	defer s.dropStaleDeps(r.id, c)
	if r.err != nil {
		s.fail(r.id, loadError(st, c.key, r.err))
		return
	}
	c.decoded = r.data
	st.setState(r.id.Index, Waiting)
	s.changes++
	for _, dr := range r.deps {
		dep, err := s.addDep(r.id, dr)
		if err != nil {
			s.fail(r.id, loadError(st, c.key, err))
			return
		}
		c.deps = append(c.deps, dep)
	}
	s.waiting = append(s.waiting, r.id)
}

func loadError(st anyStore, key Key, err error) error {
	return fmt.Errorf("asset: load %s %s: %w", st.name(), key, err)
}

// commit runs Finish for decoded assets in FIFO order. It repeats passes while
// something progressed, because finishing a dependency can unblock a parent
// that was passed over earlier in the same call.
func (s *Server) commit() {
	budget := s.opts.CommitBudget
	if s.syncing > 0 {
		budget = 0
	}
	start := s.now()
	committed := 0
	for {
		progressed := false
		kept := s.waiting[:0] // in-place filter: write index never passes read index
		for i, id := range s.waiting {
			st, c, ok := s.live(id)
			if !ok {
				continue // unloaded meanwhile
			}
			// A Ready asset in the list waits with a reload candidate.
			reload := st.stateAt(id.Index) == Ready && c.cand != nil && c.cand.decoded
			if !reload && st.stateAt(id.Index) != Waiting {
				continue // failed meanwhile
			}
			deps := c.deps
			if reload {
				deps = c.cand.deps
			}
			switch dep, depState := s.depsState(deps); depState {
			case Failed:
				if reload {
					s.reloadFailed(c, dependencyError(s, dep))
					s.dropCandidate(id, c)
				} else {
					s.fail(id, loadError(st, c.key, dependencyError(s, dep)))
					c.depFailed = true
				}
				progressed = true
			case Ready:
				if budget > 0 && committed > 0 && s.now().Sub(start) >= budget {
					s.waiting = append(kept, s.waiting[i:]...)
					return
				}
				if reload {
					s.commitReload(id, c)
				} else {
					s.finish(id)
				}
				committed++
				progressed = true
			default:
				kept = append(kept, id)
			}
		}
		s.waiting = kept
		if !progressed {
			return
		}
	}
}

// depsState reports Ready if every dependency is Ready (or there are none),
// Failed with the first failed dependency, or Waiting if some are pending.
func (s *Server) depsState(deps []ID) (failed ID, state State) {
	state = Ready
	for _, d := range deps {
		st, _, ok := s.live(d)
		if !ok {
			continue // cannot happen: parents retain their dependencies
		}
		switch st.stateAt(d.Index) {
		case Ready:
		case Failed:
			return d, Failed
		default:
			state = Waiting
		}
	}
	return ID{}, state
}

func dependencyError(s *Server, dep ID) error {
	dst, dc, _ := s.live(dep)
	return fmt.Errorf("dependency %s %s failed: %w", dst.name(), dc.key, dc.err)
}

// finish runs the loader's Finish for a decoded asset whose dependencies are
// Ready, and settles it and its reserved sub-assets.
func (s *Server) finish(id ID) {
	st, c, _ := s.live(id)
	data := c.decoded
	c.decoded = nil
	ctx := FinishContext{Key: c.key, srv: s, id: id, deps: c.deps}
	if err := st.finish(&ctx, id.Index, data); err != nil {
		s.fail(id, loadError(st, c.key, err))
		return
	}
	st.setState(id.Index, Ready)
	c.version = 1
	c.bytes = st.measure(id.Index, c.fileLen)
	s.dirty = true
	s.keep(c, data)
	s.changes++
	s.emit(EventLoaded, id, c.key, nil)
	// Sub-assets that were reserved by Load but not added by Finish don't exist.
	for _, ch := range c.children {
		if cst, _, ok := s.live(ch); ok && cst.stateAt(ch.Index) == Waiting {
			s.fail(ch, noChildError(cst, s.coldOf(ch).key))
		}
	}
	// A Failed asset that hot reload loaded again: dependents that failed
	// because of it try again. Ready dependents are left alone: one that
	// lists this asset is a reload candidate waiting for it.
	if len(c.dependents) > 0 {
		s.cascade(id, false)
	}
}

// fail settles an asset as Failed with err, reports it, and fails the
// sub-assets that were waiting for it.
func (s *Server) fail(id ID, err error) {
	st, c, ok := s.live(id)
	if !ok {
		return
	}
	st.clearValue(id.Index) // a child added by a Finish that then failed
	c.err = err
	c.decoded = nil
	c.kept, c.hasKept = nil, false
	c.depFailed = false // the caller sets it when a dependency is the cause
	st.setState(id.Index, Failed)
	s.changes++
	s.emit(EventFailed, id, c.key, err)
	s.opts.OnError(err)
	for _, ch := range c.children {
		cst, cc, ok := s.live(ch)
		if !ok {
			continue
		}
		if state := cst.stateAt(ch.Index); state == Waiting || state == Ready {
			s.fail(ch, parentFailedError(cst, cc.key, c))
		}
	}
}

// free unloads one asset together with its sub-assets and drops its
// dependency holds.
func (s *Server) free(id ID) {
	st, c, _ := s.live(id)
	prev := st.stateAt(id.Index)
	for _, ch := range c.children {
		if _, _, ok := s.live(ch); ok {
			s.free(ch)
		}
	}
	s.dropDeps(id, c.deps)
	s.dropDeps(id, c.staleDeps)
	if c.cand != nil {
		s.dropDeps(id, c.cand.deps)
	}
	if c.parked {
		s.parkedN--
	}
	key := c.key
	st.freeSlot(id.Index)
	s.changes++
	if prev == Ready || prev == Failed {
		s.emit(EventUnloaded, id, key, nil)
	}
}
