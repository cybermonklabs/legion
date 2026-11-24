package daemon

import (
	"context"
	"sync"

	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// provisionConcurrency is how many workspace checkouts the outbox's provisions run at once
// (provisionLocks.slot, workspace.Request.CheckoutSlot). Under tmux each provision checks out the
// issue's whole working copy on the daemon's host — one project's repository is 80,975 files and
// 1.5 GB, about 20 s to check out on a loaded build host — and a daemon that takes over a project's
// store provisions a workspace for every claim it relaunches, hundreds at once, which one at a time
// takes hours. The checkouts write only their own directories and run side by side
// (workspace.Request.StoreLock); sixteen keep a host's disk busy without a few hundred writers
// contending for it. Only the checkout takes a place: the rest of a provision is ordered by its
// repository's store lock, and a provision that waited on that lock while it held a place would
// keep every other repository's checkouts waiting behind it. The clone and fetch under each store
// lock are one at a time per repository and unbounded across repositories, on purpose: a project
// works in a handful of repositories, each fetch brings two branches, and a bound shared across
// repositories would bring back the cross-repository wait the place's position removed.
const provisionConcurrency = 16

// provisionLocks orders the outbox's workspace provisions and removals. At most
// provisionConcurrency checkouts run at once (slot). Provisions and removals of one issue take
// turns (issues, by workspace directory): two launches that both found a workspace missing or not
// checked out provision it once, and a removal never runs under its issue's provision. Each
// repository's shared clone has one store lock (stores, by clone directory), which
// workspace.Provision holds for every command on the clone but the checkout, and workspace.Remove
// for every step but the delete of the workspace's files; it takes the clone's lock across
// processes too (workspace.CloneLock), which a pane's shim holds while it adds a task's working
// copy to the same clone. A provision takes its issue's turn, then its store lock, and lets the
// lock go before it waits for a checkout place; a removal takes its issue's turn, then its store
// lock. Nothing that holds a place waits on a store lock or a turn, and nothing that holds a store
// lock waits on a turn or a place, so no two wait on each other. A shim holds the clone's lock for
// the commands that add one working copy and waits on nothing of the daemon's while it does. The
// zero value is ready to use.
type provisionLocks struct {
	mu     sync.Mutex
	slots  chan struct{}
	issues keyedLocks
	stores cloneLocks
}

// slot waits for one of the provisionConcurrency checkout places, or for ctx to end, and returns
// the release of the place it took: a workspace.Request's CheckoutSlot.
func (p *provisionLocks) slot(ctx context.Context) (func(), error) {
	p.mu.Lock()
	if p.slots == nil {
		p.slots = make(chan struct{}, provisionConcurrency)
	}
	slots := p.slots
	p.mu.Unlock()
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// keyedLocks is one mutex per key, kept only while someone holds or awaits it. The zero value is
// ready to use.
type keyedLocks struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

// keyedLock is one key's mutex and the number of its current users, and the key's lock across
// processes when it has one (cloneLocks), taken after the mutex and let go before it.
type keyedLock struct {
	sync.Mutex
	users  int
	across tryLocker
}

// tryLocker is a lock that can also be taken only when it is free: a key's lock across processes.
type tryLocker interface {
	sync.Locker
	TryLock() bool
}

func (l *keyedLock) Lock() {
	l.Mutex.Lock()
	if l.across != nil {
		l.across.Lock()
	}
}

// TryLock takes the key's mutex and its lock across processes when both are free, and reports
// whether it did; it waits for neither, and holds neither when it answers false.
func (l *keyedLock) TryLock() bool {
	if !l.Mutex.TryLock() {
		return false
	}
	if l.across != nil && !l.across.TryLock() {
		l.Mutex.Unlock()
		return false
	}
	return true
}

func (l *keyedLock) Unlock() {
	if l.across != nil {
		l.across.Unlock()
	}
	l.Mutex.Unlock()
}

// take is key's mutex, not locked, and the release that ends this use of it: once a key's last
// user releases it, the key's entry goes.
func (k *keyedLocks) take(key string) (*keyedLock, func()) {
	return k.takeAcross(key, nil)
}

// takeAcross is take for a key that has a lock across processes: across makes it when the key's
// entry is made, nil for none.
func (k *keyedLocks) takeAcross(key string, across func(key string) tryLocker) (*keyedLock, func()) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.locks == nil {
		k.locks = map[string]*keyedLock{}
	}
	lock := k.locks[key]
	if lock == nil {
		lock = &keyedLock{}
		if across != nil {
			lock.across = across(key)
		}
		k.locks[key] = lock
	}
	lock.users++
	return lock, func() {
		k.mu.Lock()
		defer k.mu.Unlock()
		if lock.users--; lock.users == 0 {
			delete(k.locks, key)
		}
	}
}

// cloneLocks is the store lock of each shared clone, by its directory: a keyedLock whose lock
// across processes is the clone's (workspace.CloneLock). The zero value is ready to use.
type cloneLocks struct{ keyedLocks }

// take is clone's store lock, not locked, and the release that ends this use of it.
func (c *cloneLocks) take(clone string) (*keyedLock, func()) {
	return c.takeAcross(clone, func(clone string) tryLocker { return workspace.CloneLock(clone) })
}
