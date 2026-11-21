package busprojects

import (
	"context"
	"time"
)

// SetLookup makes a's proofs resolve each listener workload's instances with lookup, in place of
// the system resolver, so a test can give one name several replicas' addresses.
func SetLookup(a *HubAdmission, lookup func(ctx context.Context, host string) ([]string, error)) {
	a.lookup = lookup
}

// SetPeriod makes Run prove every period, in place of proofPeriod.
func SetPeriod(a *HubAdmission, period time.Duration) {
	a.period = period
}

// KeptProofs is the number of the proof whose result a holds: 0 before any, and n once its nth
// proof has ended and been kept.
func KeptProofs(a *HubAdmission) uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.kept
}
