package asset

import (
	"fmt"
	"io/fs"
)

// jobQueueSize is the capacity of the job and result channels. Jobs beyond it
// wait in Server.pending, so Load never blocks.
const jobQueueSize = 256

// job is one Decode to run: read the file at key.Path, decode it as st's type.
type job struct {
	id     ID
	key    Key
	st     anyStore
	reload bool // a hot-reload candidate: the slot stays Ready meanwhile
}

// result is what a Decode produced. Only the game goroutine applies it.
type result struct {
	id     ID
	data   any
	deps   []depReq
	err    error
	reload bool
	stamp  stamp // file version that was read; zero unless watching
	size   int64 // length of the file that was read
}

// work is one decode goroutine. It touches no Store and no Server state: it
// reads the job, decodes, and sends the result back, which keeps every store
// lock-free and every GPU object on the game goroutine.
func (s *Server) work() {
	defer s.wg.Done()
	for {
		select {
		case j := <-s.jobs:
			select {
			case s.results <- s.runDecode(j):
			case <-s.done:
				return
			}
		case <-s.done:
			return
		}
	}
}

// runDecode reads and decodes one asset. It runs on a worker, or inline on the
// game goroutine when Workers is 0.
func (s *Server) runDecode(j job) result {
	r := result{id: j.id, reload: j.reload}
	if j.st.builtin(j.key) {
		ctx := DecodeContext{Key: j.key}
		r.data, r.err = j.st.decode(&ctx, nil)
		r.deps = ctx.deps
		return r
	}
	if s.opts.FS == nil {
		r.err = fmt.Errorf("no file system configured: %w", fs.ErrNotExist)
		return r
	}
	if s.watching() {
		// Stat before reading: if the file changes in between, the stamp is
		// older than the data and the next poll reloads once more, which is
		// harmless. The other order could miss a change.
		r.stamp = statStamp(s.opts.FS, j.key.Path)
	}
	data, err := fs.ReadFile(s.opts.FS, j.key.Path)
	if err != nil {
		r.err = err
		return r
	}
	r.size = int64(len(data))
	ctx := DecodeContext{Key: j.key}
	r.data, r.err = j.st.decode(&ctx, data)
	r.deps = ctx.deps
	return r
}

// dispatch hands queued jobs to the workers without blocking, or, with no
// workers, decodes them inline. Jobs whose asset was unloaded meanwhile are
// dropped.
func (s *Server) dispatch() {
	if s.opts.Workers == 0 {
		s.decodeInline()
		return
	}
	sent := 0
	for _, j := range s.pending {
		st, ok := s.runnable(j)
		if !ok {
			sent++
			continue
		}
		if !j.reload {
			st.setState(j.id.Index, Decoding)
		}
		select {
		case s.jobs <- j:
			s.inflight++
			s.changes++
			sent++
		default:
			if !j.reload {
				st.setState(j.id.Index, Queued)
			}
			s.pending = append(s.pending[:0], s.pending[sent:]...)
			return
		}
	}
	s.pending = s.pending[:0]
}

// decodeInline runs every queued job on the game goroutine. Applying a result
// can queue dependencies, which the loop picks up in the same call.
func (s *Server) decodeInline() {
	for i := 0; i < len(s.pending); i++ {
		j := s.pending[i]
		st, ok := s.runnable(j)
		if !ok {
			continue
		}
		if !j.reload {
			st.setState(j.id.Index, Decoding)
		}
		s.applyResult(s.runDecode(j))
	}
	s.pending = s.pending[:0]
}

// runnable reports whether a queued job is still wanted: its asset is live and
// held, and a reload job's candidate was not dropped meanwhile.
func (s *Server) runnable(j job) (anyStore, bool) {
	st, c, ok := s.live(j.id)
	if !ok || (c.refs == 0 && !c.parked) || (j.reload && c.cand == nil) {
		return nil, false
	}
	return st, true
}

// drain applies every result that is ready, without blocking.
func (s *Server) drain() {
	for s.inflight > 0 {
		select {
		case r := <-s.results:
			s.inflight--
			s.applyResult(r)
		default:
			return
		}
	}
}

// waitForResult blocks for one worker result and applies it. Only LoadSync
// calls it, and only when jobs are in flight.
func (s *Server) waitForResult() {
	select {
	case r := <-s.results:
		s.inflight--
		s.applyResult(r)
	case <-s.done:
	}
}
