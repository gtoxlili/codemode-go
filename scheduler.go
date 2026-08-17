package codemode

import (
	"context"
	"fmt"
	"sync"
)

// scheduler runs the sub-calls of one program.
//
//   - Start order is submission order, i.e. the order the program evaluated
//     tools.name(...). Nothing is reordered.
//   - Ordinary calls go into a pool of MaxParallel. That pool is where the
//     parallelism of a Promise.all comes from.
//   - A call that conflicts with an outstanding one — shares a ConflictKey with
//     it and at least one of the two mutates — waits for the pool to drain, runs
//     alone, and only then lets later calls start.
type subResult struct {
	out string
	err error
}

type subCall struct {
	binding   Binding
	args      string
	exclusive bool
	done      chan subResult
	prior     *priorSubmit
}

// priorSubmit is the conflict surface of a call that is outstanding. It is
// removed the moment the call finishes: conflicts only exist between calls that
// overlap in time. Keeping finished calls in the set would mean that writing a
// digest and then reading a batch of files back — including the one just
// written — serializes the whole read fan-out behind a write that is long done.
type priorSubmit struct {
	keys     []string
	mutating bool
}

type scheduler struct {
	ctx         context.Context
	limits      Limits
	deps        deps
	mu          sync.Mutex
	queued      []*subCall
	prior       []*priorSubmit
	running     int
	barrier     bool // an exclusive call is running: everything else waits
	submitted   int
	failedLimit bool
}

func newScheduler(ctx context.Context, limits Limits, d deps) *scheduler {
	return &scheduler{ctx: ctx, limits: limits, deps: d}
}

func (s *scheduler) Submit(b Binding, args string) (<-chan subResult, error) {
	ch := make(chan subResult, 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failedLimit {
		return nil, errTooManySubCalls
	}
	if s.submitted >= s.limits.MaxSubCalls {
		s.failedLimit = true
		return nil, errTooManySubCalls
	}
	s.submitted++

	keys := conflictKeysOf(s.ctx, s.deps, b, args)
	entry := &priorSubmit{keys: keys, mutating: b.Mutating}
	c := &subCall{
		binding:   b,
		args:      args,
		done:      ch,
		exclusive: exclusiveAgainst(keys, b.Mutating, s.prior),
		prior:     entry,
	}
	s.prior = append(s.prior, entry)
	s.queued = append(s.queued, c)
	s.dispatchLocked()
	return ch, nil
}

// dispatchLocked starts whatever the head of the queue allows. An exclusive
// call at the head blocks the queue until it is done — head-of-line waiting is
// the price of strict submission order, and it buys predictable ordering.
func (s *scheduler) dispatchLocked() {
	for len(s.queued) > 0 && !s.barrier {
		head := s.queued[0]
		if head.exclusive && s.running > 0 {
			return
		}
		if !head.exclusive && s.running >= s.limits.MaxParallel {
			return
		}
		s.queued = s.queued[1:]
		s.running++
		if head.exclusive {
			s.barrier = true
		}
		go s.run(head)
	}
}

func (s *scheduler) run(c *subCall) {
	out, err := safeInvoke(s.ctx, c.binding, c.args)
	s.mu.Lock()
	s.running--
	if c.exclusive {
		s.barrier = false
	}
	for i, p := range s.prior {
		if p == c.prior {
			s.prior = append(s.prior[:i], s.prior[i+1:]...)
			break
		}
	}
	s.dispatchLocked()
	s.mu.Unlock()

	c.done <- subResult{out: out, err: err} // buffered, single reader
}

// inflight reports outstanding sub-calls. The watchdog uses it to tell "the
// program is computing" from "the program is waiting": a single-threaded VM
// suspended on await is not burning compute.
func (s *scheduler) inflight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// safeInvoke turns a panicking tool into a failed call. Bindings come from the
// host and may be anything; one bad tool must not take the run down.
func safeInvoke(ctx context.Context, b Binding, args string) (out string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("tool panicked: %v", r)
		}
	}()
	return b.Invoke(ctx, args)
}

// conflictKeysOf collects a call's keys, dropping empties and duplicates. A
// panic here is a bug in the host's tool, not in the program, so it is reported
// to the host's logger and the call is scheduled as conflict-free rather than
// failing the run.
func conflictKeysOf(ctx context.Context, d deps, b Binding, args string) (keys []string) {
	if b.ConflictKeys == nil {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			d.logger(ctx, "codemode: ConflictKeys panicked, scheduling the sub-call as conflict-free",
				"tool", b.Name, "panic", r)
			keys = nil
		}
	}()
	raw := b.ConflictKeys(args)
	for _, k := range raw {
		if k == "" {
			continue
		}
		dup := false
		for _, seen := range keys {
			if seen == k {
				dup = true
				break
			}
		}
		if !dup {
			keys = append(keys, k)
		}
	}
	return keys
}

func exclusiveAgainst(keys []string, mutating bool, prior []*priorSubmit) bool {
	if len(keys) == 0 {
		return false
	}
	for _, p := range prior {
		if !mutating && !p.mutating {
			continue
		}
		for _, k := range keys {
			for _, pk := range p.keys {
				if k == pk {
					return true
				}
			}
		}
	}
	return false
}
