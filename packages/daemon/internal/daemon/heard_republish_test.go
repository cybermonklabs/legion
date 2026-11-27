package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// eachHeldSubscription stands in for the runtime's subscription update (workflowRuntime's
// setRepositories) and holds every call it gets, not only the first: each call hands the test a
// release of its own on calls and waits there until the test closes it, then goes to intake.
type eachHeldSubscription struct {
	w     *workflowRuntime
	calls chan chan struct{}
}

func (h *eachHeldSubscription) install(w *workflowRuntime) {
	h.w = w
	w.setRepositories = h.set
}

func (h *eachHeldSubscription) set(ctx context.Context, repositories []ghrepo.Repository) error {
	release := make(chan struct{})
	h.calls <- release
	<-release
	return h.w.consumers.SetRepositories(ctx, repositories)
}

// hearsNothing stands in for the runtime's subscription update (workflowRuntime's
// setRepositories) in a runtime a test builds with no consumers: it takes every set and publishes
// it nowhere, so a launch that hears its repository (workLaunched) adds it to the heard set as the
// daemon's does, with no JetStream behind it.
func hearsNothing(context.Context, []ghrepo.Repository) error { return nil }

// The publication of the whole heard set again (republishHeard, the refresher's fourth job) holds
// no transaction of the pool across its JetStream call. The subscription update reaches JetStream,
// and a durable consumer that is missing has it read the pool the transaction would be on, so a
// publication that waited on it inside the transaction it reads the records in asked that pool for
// a second connection while holding one, and every connection of the pool sat idle in a
// transaction of that shape once the stream answered slowly (INFRA-3169). A stand-in holds each
// stream call; at each one no connection of the rig's own database is idle in a transaction, and a
// fact's transaction, which takes the fact serializer, still commits within a deadline of its own.
// Control: with the call moved into the records' read-only transaction, the first check finds that
// transaction idle in it.
func TestPublishingTheHeardSetAgainHoldsNoTransactionAcrossItsStreamCall(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	held := &eachHeldSubscription{calls: make(chan chan struct{})}
	w, _, _, _ := repositoryRigWith(t, &installations{}, held.install)
	published := make(chan error, 1)
	go func() { published <- w.republishHeard(ctx) }()

	calls := 0
	for {
		var release chan struct{}
		select {
		case release = <-held.calls:
		case err := <-published:
			if err != nil {
				t.Fatalf("the publication of the heard set: %v", err)
			}
			if calls == 0 {
				t.Fatal("the publication of the heard set made no stream call, so this proves nothing")
			}
			return
		case <-time.After(10 * time.Second):
			t.Fatalf("the publication of the heard set neither made a stream call nor finished within 10s, after %d calls", calls)
		}
		calls++
		if idle := rowCount(t, w, "select count(*) from pg_stat_activity where datname = current_database() and state like 'idle in transaction%'"); idle != 0 {
			close(release)
			t.Fatalf("stream call %d of the publication: %d connections of the pool sit idle in a transaction while the call is held, want none", calls, idle)
		}
		serialized := make(chan error, 1)
		go func() {
			serialized <- store.Transact(ctx, w.pool, func(ctx context.Context, tx pgx.Tx) error { return intake.SerializeFacts(ctx, tx) })
		}()
		select {
		case err := <-serialized:
			if err != nil {
				close(release)
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatalf("stream call %d of the publication: a fact's transaction could not take the serializer while the call was held, so the publication holds the serializer across it", calls)
		}
		close(release)
	}
}
