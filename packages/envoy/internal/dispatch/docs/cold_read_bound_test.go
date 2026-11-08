package docs

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A request-pool transaction that waits for a cold read's fold sits idle in that transaction for
// as long as the fold takes: the check-result write does exactly that, under the two issue row
// locks it holds (api/results.go's refreshResolvedCheck -> coldRead), and a write carries no
// deadline of its own to end the wait. So the bound Postgres ends an idle request transaction by
// (store.RequestIdleInTransactionTimeout) has to outlast one whole fold and the rollback that
// follows it, or Postgres would break writes that are doing what they are meant to.
//
// The bound is computed from those two constants rather than written out, and this test holds the
// computation to what the fold actually waits: the fold's bound is store.ColdReadBound itself, so
// raising it raises the pool's bound with it, and a change that broke that tie - a coldReadTimeout
// of its own - fails here instead of silently undercutting the pool.
func TestTheRequestPoolsIdleBoundOutlastsAColdRead(t *testing.T) {
	if coldReadTimeout != store.ColdReadBound {
		t.Fatalf("coldReadTimeout = %v, want store.ColdReadBound (%v): the request pool's idle bound is computed from that constant",
			coldReadTimeout, store.ColdReadBound)
	}
	if want := coldReadTimeout + store.RollbackTimeout; store.RequestIdleInTransactionTimeout <= want {
		t.Fatalf("store.RequestIdleInTransactionTimeout = %v, want more than a cold read and the rollback after it (%v)",
			store.RequestIdleInTransactionTimeout, want)
	}
}
