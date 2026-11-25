package api

// This file holds the handler of `GET /legion/v1/state` (stateRoute) and the snapshot both of its
// views read, the whole state and the observed view (readSnapshot).

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// projectState is the project's own value, as every route that reads it reads it: one
// repeatable-read read-only snapshot (readSnapshot), one read of the source, and the consumers'
// lag read after that snapshot commits. It writes its own refusal and answers false where it wrote
// one, so the two reading routes share the transaction, the read and the refusals and differ only
// in what each answers from the value.
//
// The claims are the snapshot's own, which is what a caller's contract is read from
// (writeAnswerFrom): a claim and the state it was projected into cannot come from two moments.
func (s *server) projectState(w http.ResponseWriter, r *http.Request) (State, []supervise.Claim, bool) {
	reading, cancel := context.WithTimeout(r.Context(), s.stateBound)
	defer cancel()
	var state State
	var claims []supervise.Claim
	read := func(ctx context.Context, tx pgx.Tx) (err error) {
		state, claims, err = s.state.State(ctx, tx)
		return err
	}
	if !s.readSnapshot(w, r, reading, "api: state source failed", read) {
		return State{}, nil, false
	}
	// The consumers' lag is read from NATS after the commit, so a slow or silent NATS server holds
	// no snapshot open; what it does not answer is each consumer's `unavailable`, never a refusal.
	if s.consumerLag != nil {
		state.Consumers = ConsumerLagViewsOf(s.consumerLag(reading))
	}
	// The fleet's lowest plugin contract is read from the claims of the very snapshot the state
	// was projected from, so the number the body reports and the shapes the body was served at
	// are read from one moment (State.LowestPluginContract).
	state.LowestPluginContract = lowestPluginContract(claims)
	return state, claims, true
}

// observedState is the part of the project's state that the observed view reads
// (StateSource.ObservedState), read as projectState reads the whole state: in one snapshot of its
// own (readSnapshot), under the same bound, statement timeout and rollback. The claims are the
// snapshot's own, which the caller's contract is read from, as they are for the whole state. It
// reads no consumers' lag, which is no part of what Dispatch observes.
func (s *server) observedState(w http.ResponseWriter, r *http.Request) (State, []supervise.Claim, bool) {
	reading, cancel := context.WithTimeout(r.Context(), s.stateBound)
	defer cancel()
	var observed State
	var claims []supervise.Claim
	read := func(ctx context.Context, tx pgx.Tx) (err error) {
		observed, claims, err = s.state.ObservedState(ctx, tx)
		return err
	}
	if !s.readSnapshot(w, r, reading, "api: observed state source failed", read) {
		return State{}, nil, false
	}
	return observed, claims, true
}

// readSnapshot runs read in one repeatable-read read-only transaction (store.TransactWith) and
// commits it, which is how both views of the state read the source (projectState,
// observedState). It writes its own refusal and answers false where it wrote one; failed is the
// line it logs when read fails. reading is the caller's context, under the read's bound; read runs
// on the transaction's own context.
//
// The read runs under a bound of the daemon's own (stateReadBound, Options.StateReadBound).
// Before INFRA-3169 the only thing that ended this read was the client giving up, so a read that
// never finished held its connection as long as the client held its socket, and 32 of those are
// the whole pool.
//
// The bound is laid twice over the same work, because the two ends it has to catch are different
// ones. The context deadline ends the handler's own waiting — on the Go work between statements
// and on anything that is not a statement — and statement_timeout ends a statement that runs
// long inside the transaction, a lock wait included, which no context deadline reaches until the
// driver's cancel lands. It is `set local`, so it holds for this transaction alone and never for
// the pool: a migration legitimately runs long DDL in one statement (internal/store's apply),
// and a statement bound on the pool would cut it. The pool carries the idle bound instead, which
// is the one that is true of every transaction (internal/store's idleBound).
//
// http.TimeoutHandler is not the way to do this and must not be added: it buffers the whole
// response into a buffer of its own before writing it, and the state's body is buffered once
// already — writeAnswerFrom hands the body to json.Marshal, which materialises all of it before
// a byte is written — so the handler would hold a second full copy of a body that measured
// 4,055,424 bytes on the production deployment on 2026-11-12. One such copy per concurrent read
// is a second resource failure inside the first one this bounds.
func (s *server) readSnapshot(w http.ResponseWriter, r *http.Request, reading context.Context, failed string, read func(ctx context.Context, tx pgx.Tx) error) bool {
	if s.stateTransactions == nil {
		s.log.Error("api: state transaction source is unset")
		s.refuse(w, r, http.StatusInternalServerError, "state unavailable")
		return false
	}
	// The step that failed names the log line, as each did before the read ran through
	// store.TransactWith: the begin, until the body runs; the statement bound; the read, which
	// failed names; and the commit, for a failure after a body that succeeded.
	failure := "api: begin state transaction failed"
	err := store.TransactWith(reading, s.stateTransactions, pgx.TxOptions{
		IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly,
	}, func(ctx context.Context, tx pgx.Tx) error {
		failure = "api: commit state transaction failed"
		if _, err := tx.Exec(ctx, "set local statement_timeout = "+strconv.FormatInt(statementBound(s.stateBound), 10)); err != nil {
			failure = "api: bound the state transaction's statements failed"
			return err
		}
		err := read(ctx, tx)
		if err != nil {
			failure = failed
		}
		return err
	})
	if err != nil {
		s.logRequestFailure(r, err, failure)
		s.refuse(w, r, http.StatusInternalServerError, "state unavailable")
		return false
	}
	return true
}

// stateRoute answers the whole state, or with `?view=observed` the observed view, which is what
// Dispatch reads (ObservedState). Each is read in its own snapshot (projectState, observedState)
// and served through the one writer at the caller's contract: the observed view is the whole
// state's members read through observedOf after the whole state's own bands and strips, so at
// every contract its members are the whole state's (shaping.go's observedFor).
//
// A request that names any other `view` (stateViewOf) gets the whole state, as a daemon without
// the view serves it. For that request the route logs a warning that names the values it was given
// and the one view the route serves, because a later version may refuse such a request. The route
// authenticates nobody and the values are the caller's, so the warning is logged at most once in
// otherViewWarnEvery in all (otherViewWarnings.admit). The warning names how many such requests
// since the last one were not logged.
func (s *server) stateRoute(w http.ResponseWriter, r *http.Request) {
	observed, others := stateViewOf(r)
	if others != nil {
		if skipped, logged := s.otherViews.admit(s.now()); logged {
			s.log.Warn("api: the request names no view of the state, so it gets the whole state; a later version may refuse such a request",
				"views", others, "onlyView", observedView, "caller", r.RemoteAddr, "skipped", skipped)
		}
	}
	if observed {
		view, claims, ok := s.observedState(w, r)
		if !ok {
			return
		}
		s.writeAnswerFrom(w, r, AnswerObservedState, http.StatusOK, view, claims)
		return
	}
	state, claims, ok := s.projectState(w, r)
	if !ok {
		return
	}
	s.writeAnswerFrom(w, r, AnswerState, http.StatusOK, state, claims)
}

// observedView is the one value of the state route's `view` parameter: the observed view
// (ObservedState).
const observedView = "observed"

// stateViewOf answers which view of the state r asks the state route for. It reads r alone and
// logs nothing. A request with the one `view` value observedView asks for the observed view:
// observed is true and others is nil. A request with no `view` parameter asks for the whole state:
// observed is false and others is nil. A request with any other `view` (another value, an empty
// value, or two values) gets the whole state too: observed is false and others is every `view`
// value of the request, in its order, which the route names in its warning (stateRoute).
func stateViewOf(r *http.Request) (observed bool, others []string) {
	views, asked := r.URL.Query()["view"]
	if !asked {
		return false, nil
	}
	if len(views) == 1 && views[0] == observedView {
		return true, nil
	}
	return false, views
}

// otherViewWarnEvery is the least time between two warnings for a request to the state route that
// names a view other than observedView.
const otherViewWarnEvery = time.Minute

// otherViewWarnings spaces the warnings for requests to the state route that name a view other
// than observedView (stateRoute): one line in all, whoever asks, each otherViewWarnEvery.
// The zero value is ready for use.
type otherViewWarnings struct {
	mu sync.Mutex
	// last is when the latest warning was logged, and the zero time before the first.
	last time.Time
	// skipped counts the requests since that warning whose own warning was not logged.
	skipped int
}

// admit is called for each such request at now. It reports whether the request's warning is logged,
// and then how many requests since the last warning were not logged, which the line names. A
// request that is not logged is counted for the next line.
func (o *otherViewWarnings) admit(now time.Time) (skipped int, logged bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.last.IsZero() && now.Sub(o.last) < otherViewWarnEvery {
		o.skipped++
		return 0, false
	}
	skipped, o.skipped, o.last = o.skipped, 0, now
	return skipped, true
}

// statementBound is bound as PostgreSQL's statement_timeout takes it: whole milliseconds, and
// never the 0 a bound under a millisecond truncates to. In PostgreSQL 0 is no statement timeout
// at all, which is the opposite of what a caller that set a bound asked for, so a non-zero bound
// renders as the one millisecond that is still a bound. No caller sets a sub-millisecond bound
// today (stateReadBound is 60 s); this is what keeps one that did from switching the bound off.
func statementBound(bound time.Duration) int64 {
	milliseconds := bound.Milliseconds()
	if milliseconds == 0 && bound != 0 {
		return 1
	}
	return milliseconds
}
