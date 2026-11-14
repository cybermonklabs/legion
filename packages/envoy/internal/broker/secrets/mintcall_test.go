package secrets

import (
	"math"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// TestServerWaitBoundsTheWaitAnAnswerNames: serverWait keeps the one rule every reader of a
// server's wait in this module keeps (internal/serverwait), on a clock the test sets. A Retry-After
// of 9999999999 seconds, one past what an int64 holds and an X-RateLimit-Reset of math.MaxInt64
// with no requests left are past what a Duration holds, and a Retry-After of two hours is past
// GitHub's own longest wait; each reads as one hour. Converted as they came, the three counts read
// as zero or a negative wait, which mintRetry.again would take for no wait and make the call again
// at once. A negative Retry-After and a reset already past read as no wait, and an ordinary value
// of each header reads as itself. Each row is an answer that is no rate limit, so the wait it
// reads is the header's and never rateLimitedWait. The test counts the rows it drove and fails
// under the number the table must hold.
func TestServerWaitBoundsTheWaitAnAnswerNames(t *testing.T) {
	const rowsToDrive = 8
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	exhausted := func(reset string) http.Header {
		return http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {reset}}
	}
	rows := []struct {
		name   string
		header http.Header
		want   time.Duration
	}{
		{"a Retry-After of 9999999999 seconds", http.Header{"Retry-After": {"9999999999"}}, time.Hour},
		{"a Retry-After of more seconds than an int64 holds", http.Header{"Retry-After": {"99999999999999999999"}}, time.Hour},
		{"an X-RateLimit-Reset of math.MaxInt64 with no requests left", exhausted(strconv.FormatInt(math.MaxInt64, 10)), time.Hour},
		{"a Retry-After of two hours", http.Header{"Retry-After": {"7200"}}, time.Hour},
		{"a negative Retry-After", http.Header{"Retry-After": {"-30"}}, 0},
		{"an X-RateLimit-Reset an hour past with no requests left", exhausted(strconv.FormatInt(now.Add(-time.Hour).Unix(), 10)), 0},
		{"a Retry-After of 30 seconds", http.Header{"Retry-After": {"30"}}, 30 * time.Second},
		{"an X-RateLimit-Reset 90 seconds out with no requests left", exhausted(strconv.FormatInt(now.Add(90*time.Second).Unix(), 10)), 90 * time.Second},
	}
	driven := 0
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			driven++
			wait, named := serverWait(row.header, false, now)
			if !named || wait != row.want {
				t.Errorf("serverWait = %s (named %t), want %s named", wait, named, row.want)
			}
		})
	}
	if driven < rowsToDrive {
		t.Fatalf("the table drove %d rows, want %d", driven, rowsToDrive)
	}
}
