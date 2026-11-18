// Package ctxlock is a mutual exclusion lock whose waiter can give up when its context ends.
//
// sync.Mutex's Lock waits for as long as the holder holds the lock, and no context reaches that
// wait. A caller that must answer inside a bound and finds the lock held by work running on a
// context with no deadline would wait that work out and miss its bound: in the agent-secrets
// helper, an enroll-session-key or unenroll-session-key op behind a refresh of the machine login
// (helper.Broker's credMu) or behind a trade of the sign-in of the person signed in on the box
// (brokerclient.SignInAuthority). Mutex lets such a caller wait only while its own context lives,
// and every other caller lock it as it locks a sync.Mutex.
//
// golang.org/x/sync/semaphore's Weighted also waits on a context, but refuses an ended context
// even when it is free, and its zero value is unusable. Mutex takes a free lock whatever the
// context, as sync.Mutex does, and its zero value is an unlocked mutex.
package ctxlock

import (
	"context"
	"sync"
)

// Mutex is a mutual exclusion lock. Its zero value is an unlocked mutex, and it must not be
// copied after its first use.
type Mutex struct {
	once sync.Once
	slot chan struct{} // holds one value while m is locked; made at its first use (ready)
}

// Lock locks m, waiting for as long as its holder holds it.
func (m *Mutex) Lock() { m.ready() <- struct{}{} }

// LockContext locks m at once when it is free, whether or not ctx has ended, as Lock would.
// Otherwise it waits for m's holder to unlock it, unless ctx ends first: then it answers
// ctx.Err(), holding nothing.
func (m *Mutex) LockContext(ctx context.Context) error {
	slot := m.ready()
	select {
	case slot <- struct{}{}:
		return nil
	default:
	}
	select {
	case slot <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Unlock unlocks m, which Lock or LockContext locked. Unlocking a mutex that is not locked
// panics, where sync.Mutex ends the program.
func (m *Mutex) Unlock() {
	select {
	case <-m.ready():
	default:
		panic("ctxlock: unlock of unlocked mutex")
	}
}

// ready is m's slot, made at its first use.
func (m *Mutex) ready() chan struct{} {
	m.once.Do(func() { m.slot = make(chan struct{}, 1) })
	return m.slot
}
