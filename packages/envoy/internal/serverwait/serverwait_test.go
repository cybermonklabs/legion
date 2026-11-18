package serverwait

import (
	"math"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// TestReadBoundsTheWaitAtAnHour: Read keeps the one rule on a clock the test sets. A wait past an
// hour reads as MaxWait, an hour: a Retry-After of two hours, which a bound at the longest
// Duration would read as itself, and the counts and dates past what a Duration holds, which
// converted as they came read as zero or a negative wait. A negative Retry-After and a reset
// already past read as no wait, and an ordinary value of each header, an hour included, reads as
// itself. A reset with requests still left, a value in neither of Retry-After's forms and no header
// at all name no wait, which leaves the caller's own fallback. The test counts the rows it drove
// and fails under the number the table must hold.
func TestReadBoundsTheWaitAtAnHour(t *testing.T) {
	const rowsToDrive = 16
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	reset := func(remaining, at string) http.Header {
		return http.Header{"X-Ratelimit-Remaining": {remaining}, "X-Ratelimit-Reset": {at}}
	}
	epoch := func(d time.Duration) string { return strconv.FormatInt(now.Add(d).Unix(), 10) }
	rows := []struct {
		name   string
		header http.Header
		want   time.Duration
		named  bool
	}{
		{"a Retry-After of two hours", http.Header{"Retry-After": {"7200"}}, time.Hour, true},
		{"a Retry-After of 9999999999 seconds", http.Header{"Retry-After": {"9999999999"}}, time.Hour, true},
		{"a Retry-After of more seconds than an int64 holds", http.Header{"Retry-After": {"99999999999999999999"}}, time.Hour, true},
		{"a Retry-After date in the year 9999", http.Header{"Retry-After": {"Fri, 31 Dec 9999 23:59:59 GMT"}}, time.Hour, true},
		{"an X-RateLimit-Reset two hours out with no requests left", reset("0", epoch(2*time.Hour)), time.Hour, true},
		{"an X-RateLimit-Reset of math.MaxInt64 with no requests left", reset("0", strconv.FormatInt(math.MaxInt64, 10)), time.Hour, true},
		{"a negative Retry-After", http.Header{"Retry-After": {"-30"}}, 0, true},
		{"a Retry-After of fewer seconds than an int64 holds", http.Header{"Retry-After": {"-99999999999999999999"}}, 0, true},
		{"an X-RateLimit-Reset an hour past with no requests left", reset("0", epoch(-time.Hour)), 0, true},
		{"a Retry-After of 30 seconds", http.Header{"Retry-After": {"30"}}, 30 * time.Second, true},
		{"a Retry-After of an hour", http.Header{"Retry-After": {"3600"}}, time.Hour, true},
		{"a Retry-After date 3 seconds out", http.Header{"Retry-After": {now.Add(3 * time.Second).Format(http.TimeFormat)}}, 3 * time.Second, true},
		{"an X-RateLimit-Reset 90 seconds out with no requests left", reset("0", epoch(90*time.Second)), 90 * time.Second, true},
		{"an X-RateLimit-Reset with requests left", reset("4", epoch(90*time.Second)), 0, false},
		{"a Retry-After in neither form", http.Header{"Retry-After": {"soon"}}, 0, false},
		{"no header", http.Header{}, 0, false},
	}
	driven := 0
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			driven++
			wait, named := Read(row.header, now)
			if wait != row.want || named != row.named {
				t.Errorf("Read = %s (named %t), want %s (named %t)", wait, named, row.want, row.named)
			}
		})
	}
	if driven < rowsToDrive {
		t.Fatalf("the table drove %d rows, want %d", driven, rowsToDrive)
	}
}
