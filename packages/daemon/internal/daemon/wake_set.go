package daemon

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"sync"
)

// wakeSet is the one shape every wake queue of the daemon's own has: a set of keys that whoever
// learns of the work records - a route answering an agent, a callback inside the supervisor's
// decision, the commit of a fact's transaction - and a goroutine of the daemon's takes in a
// batch. Recording is a map write and a non-blocking send, so none of those waits on the work; a
// key recorded twice before the taker reaches it is one piece of work rather than two, which is
// what makes a second handoff of the same task one read and a second fact one ladder.
//
// Three readers have one each: the architects whose waiting notices are released
// (workflowRuntime.readied), the reads of a task's pull request a handoff asks for (handedOff),
// and the task pull requests GitHub's merge state is read for (mergeStates). A take answers its keys in the
// order compare gives them.
type wakeSet[K comparable] struct {
	mu      sync.Mutex
	keys    map[K]bool
	wake    chan struct{}
	compare func(a, b K) int
}

// newWakeSet is an empty set of ordered keys with nothing recorded and no wake pending.
func newWakeSet[K cmp.Ordered]() *wakeSet[K] {
	return newWakeSetBy(cmp.Compare[K])
}

// newWakeSetBy is newWakeSet for keys that compare orders, such as a key of several fields.
func newWakeSetBy[K comparable](compare func(a, b K) int) *wakeSet[K] {
	return &wakeSet[K]{keys: map[K]bool{}, wake: make(chan struct{}, 1), compare: compare}
}

// add records key and wakes the taker, without blocking: the wake channel holds one wake, and a
// wake already pending is the one that will deliver this key too. A nil set has no taker behind
// it - the API-only daemon, and a test that drives the work itself - and records nothing.
func (s *wakeSet[K]) add(key K) {
	var zero K
	if s == nil || key == zero {
		return
	}
	s.mu.Lock()
	s.keys[key] = true
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// take waits for the next wake and answers every key recorded since the last take, in order, and
// whether the taker should go on: false says ctx ended. The batch can be empty, since one wake
// can cover keys an earlier take has already carried.
func (s *wakeSet[K]) take(ctx context.Context) ([]K, bool) {
	select {
	case <-ctx.Done():
		return nil, false
	case <-s.wake:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := slices.SortedFunc(maps.Keys(s.keys), s.compare)
	clear(s.keys)
	return keys, true
}

// waiting is how many keys are recorded and not yet taken. Only a test asks, to see that nothing
// stands behind the work it watched finish.
func (s *wakeSet[K]) waiting() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.keys)
}
