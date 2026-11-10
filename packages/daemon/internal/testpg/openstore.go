package testpg

import (
	"context"

	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// OpenStore is store.Open with this module's connect retry around it, and it is what a TEST opens
// a store with. The signature is store.Open's, so a test reports its failure as it did before.
//
// It exists because the retry the fixtures carry covers the connections the FIXTURES make — the
// clone pool and the admin pool (ClonePool, prepare) — and a test opens most of its connections
// through production code instead: 44 call sites of store.Open across this module's tests hand a
// fixture's DSN to store.Open, which makes a pool of its own (pgxpool.New, then Ping). A reset at
// one of those is a reset the fixture never saw, which is how
// TestADecisionHeldInTheOldArchitectsTaskAtAHandOverReachesTheChild failed at 0.12 s with no
// assertion reached while its run's own line read "661 connects, 0 needed a retry".
//
// What it retries is what testdb.OnConnect retries and nothing else: the socket going, three
// attempts 200 ms apart. A server that answered — a bad password, a database that does not exist,
// the connection cap's own refusal — fails at once, so a test still fails immediately on anything
// the server decided, and the campaign's absence of a cap refusal still means what it says.
//
// Production's own store.Open is untouched: a daemon's boot opens its store directly
// (internal/daemon's run), and whether that call wants a retry of its own is a question about the
// daemon rather than about this fixture (dispatch://INFRA-1524).
func OpenStore(ctx context.Context, dsn string) (*store.Store, error) {
	var opened *store.Store
	err := testdb.OnConnect(func() error {
		var err error
		opened, err = store.Open(ctx, dsn)
		return err
	})
	if err != nil {
		return nil, err
	}
	return opened, nil
}
