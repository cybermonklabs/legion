package projectaccess

// SetJoined makes c run joined each time a read joins one already in flight for the same person,
// for the test that such reads share one call to Dispatch.
func SetJoined(c *Client, joined func()) { c.joined = joined }

// FailureTTL is failureTTL, for the test that a failed member-kind read is answered again for
// that long and no longer.
const FailureTTL = failureTTL
