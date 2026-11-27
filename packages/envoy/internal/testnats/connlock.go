package testnats

import (
	"reflect"
	"sync"
	"testing"
	"time"
	"unsafe"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testwait"
)

// connLockSpare is what a take of a connection's lock (takeConnLock) leaves of the test binary's
// deadline for the failed test's cleanups, as testwait's own waits leave.
const connLockSpare = 30 * time.Second

// connLockGuard is what bounds a take of a connection's lock (takeConnLock). Its zero value is the
// bound every row takes: connLockSpare left of the test binary's deadline, and a timer for the rest.
// The row that proves a take on a hook's goroutine fails by name and returns
// (TestHoldWriteLockOnAHooksGoroutineChild) sets both, so that each of its two failures follows
// from what that row does and never from how long the binary took to reach it.
type connLockGuard struct {
	// spare is what the take leaves of the test binary's deadline (testwait.Left); 0 is
	// connLockSpare.
	spare time.Duration
	// after starts the guard for bound and returns the channel it fires on and its stop; nil is a
	// timer of bound.
	after func(bound time.Duration) (fired <-chan time.Time, stop func() bool)
}

// leaves is the spare g leaves of the test binary's deadline.
func (g connLockGuard) leaves() time.Duration {
	if g.spare == 0 {
		return connLockSpare
	}
	return g.spare
}

// start starts g's guard for bound.
func (g connLockGuard) start(bound time.Duration) (<-chan time.Time, func() bool) {
	if g.after != nil {
		return g.after(bound)
	}
	timer := time.NewTimer(bound)
	return timer.C, timer.Stop
}

// connLock is conn's nats.go connection lock, the sync.RWMutex Conn.mu (nats.go v1.55.0,
// nats.go:659). Every nats.go call that reads or changes the connection's state takes it: a
// publish holds it for writing through its write to the socket (Conn.publish, nats.go:4730-4825),
// so a write the socket does not take keeps every other call waiting, and a read of the status
// takes it for reading (Conn.Status, nats.go:6564). nats.go gives no way to hold it, so it is read
// from the unexported field, as the listener's tests read a registry's lock
// (cmd/listener/refilling_test.go, watcherGuard). A nats.go whose Conn has no such field fails the
// test by name with Errorf, so a caller on a goroutine other than the test's goes on, and reports
// false.
func connLock(t testing.TB, conn *natsgo.Conn) (*sync.RWMutex, bool) {
	t.Helper()
	if field := reflect.ValueOf(conn).Elem().FieldByName("mu"); field.IsValid() {
		if lock, ok := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Interface().(*sync.RWMutex); ok {
			return lock, true
		}
	}
	t.Error("nats.go's Conn has no field mu of type sync.RWMutex, the connection lock HoldWriteLock takes: find the lock this nats.go's Conn.publish holds through its write")
	return nil, false
}

// takeConnLock takes conn's connection lock (connLock) for writing and reports true once the caller
// holds it; the caller lets it go with Unlock. nats.go holds the lock only while one of its calls
// runs, so the take waits only for the calls that hold it now. Its one bound is the test binary's
// deadline less the guard's spare (testwait.Left, connLockGuard), a guard against a lock nothing lets
// go. It never ends its goroutine: the hand-back rows call HoldWriteLock from a hook on the intake's
// goroutine, where t.Fatal would end the hook rather than the test. So when it cannot take the lock -
// no such field, a deadline already no more than the spare away, the guard passing - it fails the
// test by name with Errorf and reports false. It reports false too, failing nothing, when the test
// ends first. A take that gets the lock after the wait has ended lets it go at once.
func takeConnLock(t *testing.T, conn *natsgo.Conn, g connLockGuard) (*sync.RWMutex, bool) {
	t.Helper()
	lock, found := connLock(t, conn)
	if !found {
		return nil, false
	}
	bound, err := testwait.Left(t, g.leaves())
	if err != nil {
		t.Error(err)
		return nil, false
	}
	handed, abandoned := make(chan struct{}), make(chan struct{})
	go func() {
		lock.Lock()
		select {
		case handed <- struct{}{}:
		case <-abandoned:
			lock.Unlock()
		}
	}()
	guard, stop := g.start(bound)
	defer stop()
	select {
	case <-handed:
		return lock, true
	case <-t.Context().Done():
		close(abandoned)
		return nil, false
	case <-guard:
		close(abandoned)
		t.Errorf("another nats.go call still held the connection lock %s before the test binary's deadline: nothing let it go", g.leaves())
		return nil, false
	}
}
