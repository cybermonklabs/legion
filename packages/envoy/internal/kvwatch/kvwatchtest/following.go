package kvwatchtest

import (
	"fmt"
	"testing"

	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testwait"
)

// AwaitFollowing waits for what through the shared wait (testwait.Until), until w's copy follows
// its bucket (kvwatch.Watcher.Following), a read of it having completed. A give-up prints the
// watcher's state as the last try read it: Following, NeedsRead and Err.
func AwaitFollowing(t testing.TB, what string, w *kvwatch.Watcher) {
	t.Helper()
	testwait.Until(t, what, func() (string, bool) {
		following := w.Following()
		return fmt.Sprintf("Following %t, NeedsRead %t, Err %v", following, w.NeedsRead(), w.Err()), following
	})
}
