package githubapp

import (
	"net/http"
	"time"
)

// RateLimitAt is RateLimit with the wait read against now (rateLimitAt), and GraphQLRateLimitAt is
// GraphQLRateLimit with it (graphQLRateLimitAt). RateLimit and GraphQLRateLimit read the wait
// against time.Now, so a test outside the package that reads a wait against a reset GitHub names
// passes the moment it is read at, and requires the wait exactly.
func RateLimitAt(status int, header http.Header, body []byte, now time.Time) *RateLimitError {
	return rateLimitAt(status, header, body, now)
}

func GraphQLRateLimitAt(status int, header http.Header, body []byte, now time.Time) (*RateLimitError, bool) {
	return graphQLRateLimitAt(status, header, body, now)
}
