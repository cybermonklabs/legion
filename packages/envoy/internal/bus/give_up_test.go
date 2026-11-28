package bus

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testwait"
)

// UntilGiveUp is what is left of t's run until its own waits give up (testwait.GiveUpAt): the
// span a test hands a call that takes a timeout or a window rather than a context, so that no
// span of the test's own ends a wait before the shared wait would. Under -timeout 0, which sets
// no deadline, it is a day, a span no wait reaches. Past the give-up it is a nanosecond, and the
// call it bounds ends at once.
func UntilGiveUp(t testing.TB) time.Duration {
	end, bounded := testwait.GiveUpAt(t)
	if !bounded {
		return 24 * time.Hour
	}
	return max(time.Nanosecond, end.Sub(testwait.Now()))
}
