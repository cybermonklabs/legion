package identity

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/oidc"
)

// A refresh and a Directory check are flights: each belongs to the person, outlives the request
// that started it, and runs on that request's values with none of its cancellation. Values of the
// request's that are bookkeeping of its own pool use must not reach the flight. The request's
// commit step (store.WithBeforeCommit, which marks a keyed request's Idempotency-Key committed)
// taken by the flight's transaction would mark the key for a write the request never made. The
// request's hold mark (store.WithTransactionTracking) shared with the flight would have the pool
// read each one's held connection as the other's, and refuse the second (store.ErrNestedAcquire).

// keyedRequest is a keyed request's context, as api's middleware and keyedWrites give a handler:
// tracked by the pool, carrying a commit step that counts the transactions that take it.
func keyedRequest() (context.Context, *atomic.Int32) {
	var stepped atomic.Int32
	ctx := store.WithBeforeCommit(store.WithTransactionTracking(context.Background()), func(context.Context, pgx.Tx) error {
		stepped.Add(1)
		return nil
	})
	return ctx, &stepped
}

// flightRow is one flight a keyed request's Standing starts: start begins it so that Standing
// stops waiting before it writes, and lets it write when release runs; landed reports whether its
// write landed.
type flightRow struct {
	name  string
	start func(t *testing.T, ctx context.Context) (release func(), landed func() bool)
}

// flightRows are the refresh of a cookie sign-in, which writes in transactions of the shared pool
// (ConfirmSubject), and the Directory check of IAP identity, which writes bare statements
// (RecordDirectoryActive).
var flightRows = []flightRow{
	{"a refresh", func(t *testing.T, ctx context.Context) (func(), func() bool) {
		r := newHoldRig(t, DomainRule{Domain: holdDomain})
		email := r.signIn(adaClaims())
		r.confirmedAgo(email, 2*time.Hour)
		gate := make(chan struct{})
		r.membership.SignIn = refresherFunc(func(ctx context.Context, token string) (oidc.Session, error) {
			<-gate
			return r.flow.Refresh(ctx, token)
		})
		before := r.row(email)
		if answer, err := r.membership.Standing(ctx, email); err != nil || answer.Held {
			t.Fatalf("Standing while the refresh waits = %+v, %v; want not held by the 24-hour rule", answer, err)
		}
		return func() { close(gate) }, func() bool {
			after := r.row(email)
			return after.ConfirmedAt != nil && after.ConfirmedAt.After(*before.ConfirmedAt)
		}
	}},
	{"a Directory check", func(t *testing.T, ctx context.Context) (func(), func() bool) {
		r := newDirectoryStandingRig(t)
		adaActive(r.google)
		r.record(directoryAda)
		r.active(directoryAda, "u-ada", 2*time.Hour)
		before := r.row(directoryAda)
		release := r.google.HoldSigner()
		if answer, err := r.membership.Standing(ctx, directoryAda); err != nil || answer.Held {
			t.Fatalf("Standing while the check is held = %+v, %v; want not held by the deadline", answer, err)
		}
		return release, func() bool {
			after := r.row(directoryAda)
			return after.confirmedAt != nil && after.confirmedAt.After(*before.confirmedAt)
		}
	}},
}

// TestAFlightTakesNoneOfItsRequestsPoolBookkeeping: a refresh and a Directory check that a keyed
// request's Standing starts take neither the request's commit step nor its hold mark. In the first
// row of each, the flight writes while the request holds no connection: its write lands and no
// transaction took the request's commit step. In the second, the request goes on into a
// connection of its own (store.HoldsConnection) after Standing stops waiting, and the flight's
// write still lands.
//
// Control: run a flight on context.WithoutCancel(ctx) alone. The refresh's confirmation takes the
// request's commit step, and each flight's write is refused under the request's hold mark, so it
// never lands.
func TestAFlightTakesNoneOfItsRequestsPoolBookkeeping(t *testing.T) {
	for _, row := range flightRows {
		t.Run(row.name+", the request holding no connection", func(t *testing.T) {
			ctx, stepped := keyedRequest()
			release, landed := row.start(t, ctx)
			release()
			waitFor(t, "the flight's write", landed)
			if n := stepped.Load(); n != 0 {
				t.Fatalf("the flight's transactions took the request's commit step %d times; want none: they would mark its key for a write it never made", n)
			}
		})
		t.Run(row.name+", the request holding a connection", func(t *testing.T) {
			ctx, _ := keyedRequest()
			release, landed := row.start(t, ctx)
			_, holding := store.HoldsConnection(ctx)
			defer holding()
			release()
			waitFor(t, "the flight's write while the request holds a connection", landed)
		})
	}
}
