package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
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
	return h.w.nats.intake().SetRepositories(ctx, repositories)
}

// No stream call of a mapping read is made inside a transaction that holds the fact serializer,
// whichever of the read's calls it is. A refresh that unmaps a repository makes two: the superset
// before the record snapshot and the narrowing after it (refreshMapping's first and third steps),
// and the narrowing is the one a later edit is likeliest to fold back into the snapshot's
// transaction, where it would sit across JetStream with the serializer held, which is INFRA-3169's
// R2. TestTheMappingRefreshHoldsNoTransactionAcrossItsStreamCalls holds the first call alone, so it
// passes whatever the later ones do; this holds every call, and at each one a fact's transaction,
// which takes the same serializer, must still commit within a deadline of its own.
func TestNoStreamCallOfAMappingReadHoldsTheFactSerializer(t *testing.T) {
	ctx := context.Background()
	held := &eachHeldSubscription{calls: make(chan chan struct{})}
	w, client, _, _ := mappingRigWith(t, &installations{}, held.install)
	client.set(dispatch.RepoProject{Repo: "acme/widgets", Project: "LEGION", Default: true})
	refreshed := make(chan error, 1)
	go func() { refreshed <- w.refreshMapping(ctx) }()

	calls := 0
	for {
		var release chan struct{}
		select {
		case release = <-held.calls:
		case err := <-refreshed:
			if err != nil {
				t.Fatalf("the mapping read: %v", err)
			}
			if calls == 0 {
				t.Fatal("the mapping read made no stream call, so this proves nothing")
			}
			return
		case <-time.After(10 * time.Second):
			t.Fatalf("the mapping read neither made a stream call nor finished within 10s, after %d calls", calls)
		}
		calls++
		serialized := make(chan error, 1)
		go func() {
			serialized <- pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error { return intake.SerializeFacts(ctx, tx) })
		}()
		select {
		case err := <-serialized:
			if err != nil {
				close(release)
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatalf("stream call %d of the mapping read: a fact's transaction could not take the serializer while the call was held, so the read holds the serializer across it", calls)
		}
		close(release)
	}
}
