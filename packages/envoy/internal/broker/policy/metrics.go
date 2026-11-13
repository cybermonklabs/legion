package policy

import "github.com/sjawhar/envoy/internal/metrics"

// The read label of broker_policy_store_reads_total: what one call the Loader made to the store's
// metadata was.
var (
	// readList is one Store.List, the whole-namespace listing a Load takes.
	readList = [2]string{"read", "list"}
	// readDescribe is one Store.Describe, the single-name read a LoadOne takes.
	readDescribe = [2]string{"read", "describe"}
)

// NewStoreReads registers broker_policy_store_reads_total on r and answers it, for the Loader's
// StoreReads. It is the one observable for "this pod read the secret store": the broker logs no
// line for a read whose secrets it serves, and every read spends the store's quota, which on
// Secret Manager the whole project shares. It is built before the Loader's first load, which
// NewCurrent takes before any *Current exists, so the boot's own reads are counted too.
func NewStoreReads(r *metrics.Registry) *metrics.Counter {
	return r.NewCounter("broker_policy_store_reads_total",
		"Calls the policy made to the secret store's metadata, by read: list for a whole-namespace load, describe for one secret's reread.")
}
