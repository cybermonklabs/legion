package daemon

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/testwait"
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

// No stream call of a mapping read is made inside a transaction that holds the fact serializer,
// whichever of the read's calls it is. A refresh that unmaps a repository makes two: the superset
// before the record snapshot and the narrowing after it (refreshMapping's first and third steps),
// and the narrowing is the one a later edit is likeliest to fold back into the snapshot's
// transaction, where it would sit across JetStream with the serializer held, which is INFRA-3169's
// R2. TestTheMappingRefreshHoldsNoTransactionAcrossItsStreamCalls holds the first call alone, so it
// passes whatever the later ones do; this holds every call, and at each one a fact's transaction,
// which takes the same serializer, must still commit while the call is held.
func TestNoStreamCallOfAMappingReadHoldsTheFactSerializer(t *testing.T) {
	ctx := context.Background()
	held := &eachHeldSubscription{calls: make(chan chan struct{})}
	w, client, _, _ := mappingRigWith(t, &installations{}, held.install)
	client.set(dispatch.RepoProject{Repo: "acme/widgets", Project: "LEGION", Default: true})
	refreshed := make(chan error, 1)
	go func() { refreshed <- w.refreshMapping(ctx) }()

	// next is what the mapping read did next: a stream call it is held at, or its end.
	type next struct {
		release chan struct{}
		ended   bool
		err     error
	}
	calls := 0
	for {
		got := testwait.Until(t, fmt.Sprintf("the mapping read to make stream call %d or finish", calls+1), func() (next, bool) {
			select {
			case release := <-held.calls:
				return next{release: release}, true
			case err := <-refreshed:
				return next{ended: true, err: err}, true
			default:
				return next{}, false
			}
		})
		if got.ended {
			if got.err != nil {
				t.Fatalf("the mapping read: %v", got.err)
			}
			if calls == 0 {
				t.Fatal("the mapping read made no stream call, so this proves nothing")
			}
			return
		}
		calls++
		// The call goes on once the fact's transaction is through, or at the test's end when it
		// never is, so the read's own transaction lets go of the pool the cleanup closes.
		release := sync.OnceFunc(func() { close(got.release) })
		t.Cleanup(release)
		serialized := make(chan error, 1)
		go func() {
			serialized <- pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error { return intake.SerializeFacts(ctx, tx) })
		}()
		if err := testwait.Receive(t, fmt.Sprintf("a fact's transaction to take the serializer while stream call %d of the mapping read is held: a read that holds the serializer across the call never lets it", calls), serialized); err != nil {
			t.Fatal(err)
		}
		release()
	}
}
