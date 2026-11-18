package brokertoken

// Waiting is how many readers hold a place on a metadata read in flight (GoogleAccount.waiting),
// for a test that holds that read open until every reader it started shares it.
func (g *GoogleAccount) Waiting() int64 { return g.waiting.Load() }
