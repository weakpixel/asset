package asset

import "slices"

// Keep-alive. With Options.KeepAlive set, unload parks a released Ready
// top-level asset instead of freeing it: the slot, generation, value, key
// index, children and dependency holds all stay, only refs is 0 and
// cold.parked is set. Anything that retains it again (Load, Hold, a
// dependency edge) revives it, so the handle and Version are unchanged and no
// event fires.
//
// Parked assets sit in an LRU, oldest release first. After holds changed,
// trim measures the idle bytes and frees the oldest parked assets until the
// budget is met. "Idle" includes the dependencies only parked assets hold: a
// parked atlas keeps its texture alive, so the texture's bytes count too.

// lruEntry is one park. It is stale when the asset was revived, freed or
// parked again since: seq no longer matches cold.parkSeq.
type lruEntry struct {
	id  ID
	seq uint64
}

// canPark reports whether a zero-ref asset is kept instead of unloaded.
// Assets that are not Ready (a Failed one must be retried by the next Load)
// and sub-assets (they go with their parent) are never parked.
func (s *Server) canPark(st anyStore, id ID, c *cold) bool {
	return s.opts.KeepAlive > 0 && c.owner.Type == 0 && st.stateAt(id.Index) == Ready
}

// park keeps id loaded. front puts it ahead of every other parked asset: the
// dependencies of an evicted asset inherit its age, so they go next.
func (s *Server) park(id ID, c *cold, front bool) {
	c.parked = true
	s.parkedN++
	s.parkSeq++
	c.parkSeq = s.parkSeq
	e := lruEntry{id: id, seq: c.parkSeq}
	if front {
		s.lru = slices.Insert(s.lru, 0, e)
	} else {
		if len(s.lru) >= 2*s.parkedN+32 {
			s.compactLRU() // revived assets leave entries behind
		}
		s.lru = append(s.lru, e)
	}
	s.dirty = true
}

// entryAsset returns the parked asset of a live LRU entry.
func (s *Server) entryAsset(e lruEntry) (*cold, bool) {
	_, c, ok := s.live(e.id)
	return c, ok && c.parked && c.parkSeq == e.seq
}

func (s *Server) compactLRU() {
	s.lru = slices.DeleteFunc(s.lru, func(e lruEntry) bool {
		_, ok := s.entryAsset(e)
		return !ok
	})
}

// settleZero parks or frees every queued asset whose refs are still 0. Freeing
// drops dependency holds, which can queue more, so it runs until the queue is
// empty. front is passed on to park.
func (s *Server) settleZero(front bool) {
	for i := 0; i < len(s.zero); i++ {
		id := s.zero[i]
		st, c, ok := s.live(id)
		if !ok || c.refs != 0 || c.parked {
			continue
		}
		if s.canPark(st, id, c) {
			s.park(id, c, front)
		} else {
			s.free(id)
		}
	}
	s.zero = s.zero[:0]
}

// unload parks or frees the assets released since the last Update, then trims
// the parked ones to the budget.
func (s *Server) unload() {
	s.settleZero(false)
	if s.opts.KeepAlive > 0 {
		s.trim()
	}
}

// trim frees the oldest parked assets while the idle bytes exceed the budget.
// The mark pass runs only if holds or sizes changed since the last one, so a
// steady-state Update costs nothing.
func (s *Server) trim() {
	if s.parkedN == 0 || !s.dirty {
		return
	}
	s.markInUse()
	var idle int64
	for _, st := range s.list {
		_, i := s.bytesOf(st)
		idle += i
	}
	for idle > s.opts.KeepAlive {
		id, ok := s.oldest()
		if !ok {
			break
		}
		_, c, _ := s.live(id)
		idle -= c.bytes // the dependencies stay idle, now parked in front
		s.free(id)
		s.settleZero(true)
	}
	s.dirty = false
}

// oldest removes and returns the oldest parked asset.
func (s *Server) oldest() (ID, bool) {
	for len(s.lru) > 0 {
		e := s.lru[0]
		s.lru = slices.Delete(s.lru, 0, 1) // shifts instead of reslicing: keeps the capacity
		if _, ok := s.entryAsset(e); ok {
			return e.id, true
		}
	}
	return ID{}, false
}

// external is the number of holds on c that do not come from another asset:
// scope holds. refs also counts dependency edges and, while any sub-asset is
// held, one hold on the parent.
func (s *Server) external(c *cold) int {
	n := int(c.refs) - len(c.dependents)
	for _, ch := range c.children {
		if _, cc, ok := s.live(ch); ok && cc.refs > 0 {
			n--
			break
		}
	}
	return n
}

// markInUse sets cold.mark to the current epoch for every asset a live scope
// needs: assets with a scope hold and, transitively, their dependencies and
// owners. Everything else is idle. It reuses markStack, so a warm pass does
// not allocate.
func (s *Server) markInUse() {
	s.epoch++
	stack := s.markStack[:0]
	for _, st := range s.list {
		for i := uint32(1); i < st.slotCount(); i++ {
			if st.stateAt(i) == Missing {
				continue
			}
			if c := st.coldAt(i); s.external(c) > 0 {
				c.mark = s.epoch
				stack = append(stack, ID{Type: st.typeID(), Index: i, Gen: st.genAt(i)})
			}
		}
	}
	for len(stack) > 0 {
		c := s.coldOf(stack[len(stack)-1])
		stack = stack[:len(stack)-1]
		if c.owner.Type != 0 {
			stack = s.markID(stack, c.owner)
		}
		for _, d := range c.deps {
			stack = s.markID(stack, d)
		}
		for _, d := range c.staleDeps {
			stack = s.markID(stack, d)
		}
		if c.cand != nil {
			for _, d := range c.cand.deps {
				stack = s.markID(stack, d)
			}
		}
	}
	s.markStack = stack[:0]
}

func (s *Server) markID(stack []ID, id ID) []ID {
	if c := s.coldOf(id); c.mark != s.epoch {
		c.mark = s.epoch
		stack = append(stack, id)
	}
	return stack
}

// bytesOf sums the sizes of st's Ready values, and of those not marked by the
// last markInUse.
func (s *Server) bytesOf(st anyStore) (total, idle int64) {
	for i := uint32(1); i < st.slotCount(); i++ {
		if st.stateAt(i) != Ready {
			continue
		}
		c := st.coldAt(i)
		total += c.bytes
		if c.mark != s.epoch {
			idle += c.bytes
		}
	}
	return total, idle
}
