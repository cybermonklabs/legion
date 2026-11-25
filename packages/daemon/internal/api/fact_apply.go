package api

import (
	"context"
	"net/http"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// requesterKey is the context key of a request's requester (noteRequesters).
type requesterKey struct{}

// requester is the claim whose grant a request redeemed (redeem), "" while it has redeemed none.
// A controller's grant, an operator's bearer and a claim's own secret note none.
type requester struct {
	mu    sync.Mutex
	claim claim.Token
}

// noteRequesters gives every request a requester its grant check notes (redeem) and apply reads,
// in this one place because every route passes it, as trackTransactions marks every request for
// the pool.
func noteRequesters(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requesterKey{}, &requester{})))
	})
}

// noteRequester notes token as the claim whose grant the request of ctx redeemed.
func noteRequester(ctx context.Context, token claim.Token) {
	if held, ok := ctx.Value(requesterKey{}).(*requester); ok {
		held.mu.Lock()
		defer held.mu.Unlock()
		held.claim = token
	}
}

// requesterOf is the claim whose grant the request of ctx redeemed, "" for none.
func requesterOf(ctx context.Context) claim.Token {
	held, ok := ctx.Value(requesterKey{}).(*requester)
	if !ok {
		return ""
	}
	held.mu.Lock()
	defer held.mu.Unlock()
	return held.claim
}

// apply applies fact under source and eventID through the handlers (intake.ApplyFact). It is the
// package's one call of intake.ApplyFact, so every fact a route applies passes it.
//
// An agent's answer and its request reach the daemon apart, and the record of a step names the
// model of the answer the agent's machine has handled (workflow's endStep), so a fact of an
// agent's own request that ends the agent's own step must wait for the agent's stream first. A
// fact ends a step only of an issue it names, and the step is the agent's own only where the
// record names the agent the agent of that run. So before the fact's transaction apply waits for
// the requester's stream (Options.AwaitStream) where the record names the requester the agent of
// a run that is not ended on the issue the fact names (record.RunsAStepOn), or on any issue for a
// fact that names no one issue (intake.IssueOf). A request no claim's grant made has no requester
// and waits for nothing. A read of the record that fails waits, as a run named would: the wait
// costs at most its bound, and the fact applies either way.
func (s *server) apply(ctx context.Context, source, eventID string, fact intake.Fact) (intake.Result, error) {
	if token := requesterOf(ctx); token != "" && s.awaitStream != nil && s.pool != nil {
		var runs bool
		err := store.TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			if issue, named := intake.IssueOf(fact); named {
				runs, err = record.RunsAStepOn(ctx, tx, string(token), issue)
			} else {
				runs, err = record.RunsAStep(ctx, tx, string(token))
			}
			return err
		})
		if err != nil {
			s.log.Warn("api: could not read whether the requester runs a step the fact may end; waiting for its stream", "claim", token, "error", err)
		}
		if runs || err != nil {
			s.awaitStream(ctx, token)
		}
	}
	return intake.ApplyFact(ctx, s.pool, source, eventID, fact, s.handlers...)
}
