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
// NewUnservableSecrets is the other half, registered once that Current exists.
func NewStoreReads(r *metrics.Registry) *metrics.Counter {
	return r.NewCounter("broker_policy_store_reads_total",
		"Calls the policy made to the secret store's metadata, by read: list for a whole-namespace load, describe for one secret's reread.")
}

// NewUnservableSecrets registers broker_unservable_secrets on r, read from c's live policy
// (Current.Get) at each scrape: one series of value 1 for each secret it refuses to serve
// (Set.Refused), labelled secret (the store's whole name of the secret, the name its
// RefusedMessage line carries), reason and owner (Refusal.Owner). So the gauge holds the standing
// state the state-change lines only mark the edges of: a secret that becomes servable or leaves
// the store leaves the scrape at the next one, with no call to remove it, and a reread of a name
// the store does not hold adds none. It takes the Current NewCurrent answered, so it is
// registered once NewCurrent has, after NewStoreReads, whose counter the first load already
// counts through. The labels are store metadata the owners write, never a caller's text, and
// never a value.
func NewUnservableSecrets(r *metrics.Registry, c *Current) {
	r.NewGaugeSetFunc("broker_unservable_secrets",
		"Secrets under the namespace the policy refuses to serve, one series of value 1 each, by the secret's store id, the reason and its owner tag (empty when that tag is missing or malformed).",
		func() []metrics.GaugeSample {
			refused := c.Get().Refused
			samples := make([]metrics.GaugeSample, 0, len(refused))
			for id, refusal := range refused {
				samples = append(samples, metrics.GaugeSample{
					Labels: [][2]string{{"secret", id}, {"reason", refusal.Reason}, {"owner", refusal.Owner}},
					Value:  1,
				})
			}
			return samples
		})
}
