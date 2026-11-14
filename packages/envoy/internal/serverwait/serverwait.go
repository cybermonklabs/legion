// Package serverwait reads the wait a server names before the next request, in Retry-After or in
// GitHub's X-RateLimit-Reset, by the one rule every reader of such a wait in this module keeps: a
// wait past Longest reads as Longest, never as zero or a negative wait; a negative wait reads as no
// wait; and any other wait reads as itself. A count of seconds that large wraps
// time.Duration(seconds)*time.Second, and an epoch that large wraps time.Unix into the distant
// past, so a reader that converted the value as it came would read a server's longest wait as no
// wait at all and ask again at once.
//
// Its readers are Dispatch's GitHub client (internal/dispatch/githubapp, rateLimitWait), the
// broker's mint (internal/broker/secrets, serverWait), the model loop's one retry (agentloop,
// retryable) and the broker helper's refresh (internal/broker/helper, retryAfter). Each keeps its
// own fallback for an answer that names no wait. It imports the standard library alone, so no
// binary that links it links a module more.
package serverwait

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"
)

// Longest is the longest wait a reader answers: the most whole seconds a time.Duration holds,
// about 292 years.
const Longest = math.MaxInt64 / time.Second * time.Second

// longestSeconds is Longest in seconds, the most a count of seconds is held to before it is
// multiplied out.
const longestSeconds = int64(Longest / time.Second)

// RetryAfter reads a Retry-After value against now, in either form RFC 9110 gives it: a count of
// seconds, or an HTTP date, whose wait is the time from now until it. named is false for a value in
// neither form, the empty value among them. A count past what an int64 holds still names a wait:
// ParseInt answers it as the largest or the smallest int64 with ErrRange, which the rule reads as
// Longest or as no wait.
func RetryAfter(value string, now time.Time) (wait time.Duration, named bool) {
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil || errors.Is(err, strconv.ErrRange) {
		return time.Duration(min(max(seconds, 0), longestSeconds)) * time.Second, true
	}
	if at, err := http.ParseTime(value); err == nil {
		return bounded(at.Sub(now)), true
	}
	return 0, false
}

// Read is the wait header names, read against now: Retry-After's (RetryAfter), else, when
// X-RateLimit-Remaining is 0, the time until X-RateLimit-Reset's epoch seconds. named is false when
// it names neither, and the caller's own fallback decides the wait.
func Read(header http.Header, now time.Time) (wait time.Duration, named bool) {
	if wait, named := RetryAfter(header.Get("Retry-After"), now); named {
		return wait, true
	}
	if header.Get("X-RateLimit-Remaining") != "0" {
		return 0, false
	}
	reset, err := strconv.ParseInt(header.Get("X-RateLimit-Reset"), 10, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, false
	}
	// time.Unix wraps an epoch this close to math.MaxInt64 into the distant past, so a reset
	// further out than Longest is held first to one second past it, which the rule then reads as
	// Longest whatever fraction of a second now carries.
	return bounded(time.Unix(min(reset, now.Unix()+longestSeconds+1), 0).Sub(now)), true
}

// bounded is the rule on a wait already read as a Duration: a negative wait is none, and one past
// Longest is Longest. time.Time.Sub answers the largest or the smallest Duration for a gap past
// what one holds, so a date or a reset never wraps before it reaches here.
func bounded(wait time.Duration) time.Duration {
	return min(max(wait, 0), Longest)
}
