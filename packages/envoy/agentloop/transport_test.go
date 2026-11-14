package agentloop

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/serverwait"
)

// TestRetryableBoundsTheWaitTheEndpointNames: the one retry of a model call reads the endpoint's
// Retry-After by the one rule every reader of a server's wait in this module keeps
// (internal/serverwait). A Retry-After of 9999999999 seconds and one dated in the year 9999 are
// past what a Duration holds and read as serverwait.Longest, which no run's deadline leaves time
// for, so the call is not made again: converted as it came, the count read as a negative wait,
// which a run's deadline always had time for, and the call was made again at once. A negative
// Retry-After reads as no wait, an ordinary one as itself, and an answer naming none waits
// defaultRetryWait. The test counts the rows it drove and fails under the number the table must
// hold.
func TestRetryableBoundsTheWaitTheEndpointNames(t *testing.T) {
	const rowsToDrive = 5
	rows := []struct {
		name       string
		status     int
		retryAfter string
		want       time.Duration
		// again is whether a run with a minute left waits the wait and makes the call again.
		again bool
	}{
		{"a 429 asking 9999999999 seconds", http.StatusTooManyRequests, "9999999999", serverwait.Longest, false},
		{"a 503 asking until the year 9999", http.StatusServiceUnavailable, "Fri, 31 Dec 9999 23:59:59 GMT", serverwait.Longest, false},
		{"a 429 asking a negative wait", http.StatusTooManyRequests, "-30", 0, true},
		{"a 429 asking 30 seconds", http.StatusTooManyRequests, "30", 30 * time.Second, true},
		{"a 503 naming no wait", http.StatusServiceUnavailable, "", defaultRetryWait, true},
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	driven := 0
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			driven++
			header := http.Header{}
			if row.retryAfter != "" {
				header.Set("Retry-After", row.retryAfter)
			}
			wait, retry := retryable(&http.Response{StatusCode: row.status, Header: header}, nil)
			if !retry || wait != row.want {
				t.Errorf("retryable = %s (retry %t), want %s and a retry", wait, retry, row.want)
			}
			if again := waitable(ctx, wait); again != row.again {
				t.Errorf("a run with a minute left makes the call again after %s: %t, want %t", wait, again, row.again)
			}
		})
	}
	if driven < rowsToDrive {
		t.Fatalf("the table drove %d rows, want %d", driven, rowsToDrive)
	}
}
