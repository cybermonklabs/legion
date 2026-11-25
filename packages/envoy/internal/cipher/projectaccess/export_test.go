package projectaccess

import "time"

// SetClock makes c read now instead of the wall clock, for the test that the cache keeps an
// answer CacheTTL and no longer.
func SetClock(c *Client, now func() time.Time) { c.now = now }

// SetJoined makes c run joined each time a member-kind read joins one already in flight for the
// same person, for the test that such reads share one call to Dispatch.
func SetJoined(c *Client, joined func()) { c.joined = joined }

// FailureTTL is failureTTL, for the test that a failed member-kind read is answered again for
// that long and no longer.
const FailureTTL = failureTTL
