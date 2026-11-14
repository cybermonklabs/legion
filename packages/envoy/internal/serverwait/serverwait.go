// Package serverwait reads the wait a server names before the next request, in Retry-After or in
// GitHub's X-RateLimit-Reset, by the one rule every reader of such a wait in this module keeps: a
// wait past MaxWait, one hour, reads as MaxWait, never as zero or a negative wait; a negative wait
// reads as no wait; and any other wait reads as itself. Converted as it came, a count of seconds
// past what a time.Duration holds wraps time.Duration(seconds)*time.Second, and an epoch that
// large wraps time.Unix into the distant past, so a reader would read a server's longest wait as
// no wait at all and ask again at once.
//
// Its readers are Dispatch's GitHub client (internal/dispatch/githubapp, rateLimitWait), the
// broker's mint (internal/broker/secrets, serverWait), the model loop's one retry (agentloop,
// retryable) and the broker helper's refresh (internal/broker/helper, retryAfter). Each keeps its
// own fallback for an answer that names no wait. It imports the standard library alone, so no
// binary that links it links a module more.
package serverwait

import (
	"errors"
	"net/http"
	"strconv"
	"time"
)

// MaxWait is the longest wait Read answers: one hour, GitHub's own longest. GitHub counts its
// primary rate limits per hour, so its X-RateLimit-Reset is at most an hour ahead, and its
// best-practices page asks a client for Retry-After's seconds, or a minute when there are none.
// A longer bound buys nothing and costs much: Dispatch records the wait in github_rate_limits
// under the App's scope, so every App-JWT call of every Dispatch process waits it out, and only
// deleting the row ends it sooner. A wait cut to the bound costs at most one more request, which
// a limit still in force answers with a fresh wait.
const MaxWait = time.Hour

// maxWaitSeconds is MaxWait in whole seconds, what a count of seconds is held to before it is
// multiplied out.
const maxWaitSeconds = int64(MaxWait / time.Second)

// Read is the wait header names, read against now: Retry-After's, in seconds or as an HTTP date
// (RFC 9110), else, when X-RateLimit-Remaining is 0, the time until X-RateLimit-Reset's epoch
// seconds. named is false when it names neither, and the caller's own fallback decides the wait.
// A count past what an int64 holds still names a wait: ParseInt answers it as the largest or the
// smallest int64 with ErrRange, which the rule reads as MaxWait or as no wait.
func Read(header http.Header, now time.Time) (wait time.Duration, named bool) {
	retryAfter := header.Get("Retry-After")
	if seconds, err := strconv.ParseInt(retryAfter, 10, 64); err == nil || errors.Is(err, strconv.ErrRange) {
		return time.Duration(min(max(seconds, 0), maxWaitSeconds)) * time.Second, true
	}
	if at, err := http.ParseTime(retryAfter); err == nil {
		return bounded(at.Sub(now)), true
	}
	if header.Get("X-RateLimit-Remaining") != "0" {
		return 0, false
	}
	reset, err := strconv.ParseInt(header.Get("X-RateLimit-Reset"), 10, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, false
	}
	// time.Unix wraps an epoch this close to math.MaxInt64 into the distant past, so a reset
	// further out than MaxWait is held first to one second past it, which the rule then reads as
	// MaxWait whatever fraction of a second now carries.
	return bounded(time.Unix(min(reset, now.Unix()+maxWaitSeconds+1), 0).Sub(now)), true
}

// bounded is the rule on a wait already read as a Duration: a negative wait is none, and one past
// MaxWait is MaxWait. time.Time.Sub answers the largest or the smallest Duration for a gap past
// what one holds, so a date or a reset never wraps before it reaches here.
func bounded(wait time.Duration) time.Duration {
	return min(max(wait, 0), MaxWait)
}
