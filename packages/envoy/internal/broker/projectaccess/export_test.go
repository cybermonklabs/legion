package projectaccess

import "time"

// SetClock makes c read now instead of the wall clock, for the test that the cache keeps an
// answer CacheTTL and no longer.
func SetClock(c *Client, now func() time.Time) { c.now = now }
