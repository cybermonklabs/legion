//go:build unix

package memory_test

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
)

// TestACreateWaitingOnTheLockEndsWithItsDeadline: while another process holds the memory file's
// lock, a create whose deadline passes stops waiting, answers an error before the lock is released,
// and writes nothing, then or after the release. Control: with the lock free, the same create
// makes its secret.
func TestACreateWaitingOnTheLockEndsWithItsDeadline(t *testing.T) {
	t.Run("control: the lock free", func(t *testing.T) {
		w, _ := openCreateWriter(t)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := w.PrepareCreate("test/control", map[string]string{"tier": "agent"}).Store(ctx, "control-value"); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Describe(ctx, "test/control", secretstore.ReadOptions{}); err != nil {
			t.Fatalf("the created secret does not read: %v", err)
		}
	})
	t.Run("the lock held past the deadline", func(t *testing.T) {
		w, path := openCreateWriter(t)
		lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
			t.Fatal(err)
		}
		c := w.PrepareCreate("test/expired", nil)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- c.Store(ctx, "expired-value") }()
		var got error
		select {
		case got = <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("the create was still waiting on the lock 2s after its 30ms deadline")
		}
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
			t.Fatal(err)
		}
		if got == nil {
			t.Fatal("the create whose deadline passed while it waited answered no error")
		}
		if _, err := w.Describe(context.Background(), "test/expired", secretstore.ReadOptions{}); err == nil {
			t.Errorf("the create answered %v, and the store holds its secret", got)
		}
		if unsettled := c.Unsettled(); unsettled != "" {
			t.Errorf("Unsettled = %q after a create that wrote nothing, want nothing", unsettled)
		}
	})
}
