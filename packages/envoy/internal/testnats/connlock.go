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
// deadline less connLockSpare (testwait.Left), a guard against a lock nothing lets go. It never ends
// its goroutine: the hand-back rows call HoldWriteLock from a hook on the intake's goroutine, where
// t.Fatal would end the hook rather than the test. So when it cannot take the lock - no such field,
// a deadline already no more than connLockSpare away, the guard passing - it fails the test by name
// with Errorf and reports false. It reports false too, failing nothing, when the test ends first. A
// take that gets the lock after the wait has ended lets it go at once.
func takeConnLock(t *testing.T, conn *natsgo.Conn) (*sync.RWMutex, bool) {
	t.Helper()
	lock, found := connLock(t, conn)
	if !found {
		return nil, false
	}
	bound, err := testwait.Left(t, connLockSpare)
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
	guard := time.NewTimer(bound)
	defer guard.Stop()
	select {
	case <-handed:
		return lock, true
	case <-t.Context().Done():
		close(abandoned)
		return nil, false
	case <-guard.C:
		close(abandoned)
		t.Errorf("another nats.go call still held the connection lock %s before the test binary's deadline: nothing let it go", connLockSpare)
		return nil, false
	}
}
