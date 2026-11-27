package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// getIssueReferences answers an issue's references read, one hop in each direction (refs.Closure):
// links, every node the issue's own content links to, and linked_from, every node that links to
// the issue or to its own content, each written [] when it has no row and never null. members are
// the documents of links, each read whole by its ref_key, at depth 1 and with its row's via, and
// truncated is false, since a read of one hop is never cut: both stay for the clients of the build
// before, which read them alone. The read runs under referencesClosureBound (boundedClosure);
// loadIssue before it and loadArtifactsByRefKey after it read by key on the pool, outside the
// bound. ?versions=latest leaves each member's artifact.versions holding only its latest version,
// under its own ETag; absent, empty or all is the full answer. A repeated versions is refused
// rather than read by its first value.
func (s *server) getIssueReferences(w http.ResponseWriter, r *http.Request) {
	who, ok := s.actingReader(w, r)
	if !ok {
		return
	}
	versionsValues := r.URL.Query()["versions"]
	if len(versionsValues) > 1 {
		s.writeHandlerError(w, errorf(http.StatusBadRequest, "INVALID_QUERY", "versions must be given once, as all or latest"))
		return
	}
	var latestOnly bool
	if len(versionsValues) == 1 {
		switch versionsValues[0] {
		case "", "all":
		case "latest":
			latestOnly = true
		default:
			s.writeHandlerError(w, errorf(http.StatusBadRequest, "INVALID_QUERY", "versions must be all or latest"))
			return
		}
	}
	issue, err := s.loadIssue(r.Context(), s.deps.Store.Pool, r.PathValue("key"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	closure, err := s.boundedClosure(r.Context(), who, issue.Key)
	if err != nil {
		if closureBoundHit(err) {
			s.writeHandlerError(w, errorf(http.StatusServiceUnavailable, "REFERENCES_TIMEOUT",
				"the issue's reference closure ran past its %s bound, so no reference was read; try again later", referencesClosureBound))
			return
		}
		s.writeHandlerError(w, err)
		return
	}
	refKeys := make([]string, len(closure.Members))
	for index, member := range closure.Members {
		refKeys[index] = member.RefKey
	}
	artifacts, err := s.loadArtifactsByRefKey(r.Context(), s.deps.Store.Pool, refKeys, latestOnly)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	members := make([]model.ReferenceMember, 0, len(closure.Members))
	for _, closureMember := range closure.Members {
		artifact, ok := artifacts[closureMember.RefKey]
		if !ok {
			s.writeHandlerError(w, fmt.Errorf("reference target %q disappeared while loading closure", closureMember.RefKey))
			return
		}
		members = append(members, model.ReferenceMember{
			Artifact: *artifact,
			Depth:    closureMember.Depth,
			Via:      closureMember.Via,
		})
	}
	// Each row's name reaches a caller that sent Dispatch-Trees: 1 with its tree, read where it
	// stands (newIssueReferencesRead). The answer is encoded once, exactly as WriteJSON writes it,
	// trailing newline and all, so the token hashes the bytes it validates and those are the bytes
	// written; the token names the reading and the parser that read the trees too
	// (referencesETag), so a validator given for one reading never answers the other.
	trees := wantsDisplayTrees(r)
	var answer bytes.Buffer
	if err := json.NewEncoder(&answer).Encode(newIssueReferencesRead(model.IssueReferences{
		Members:    members,
		Truncated:  false,
		Links:      closure.Links,
		LinkedFrom: closure.LinkedFrom,
	}, trees)); err != nil {
		s.writeHandlerError(w, fmt.Errorf("encode the issue's references: %w", err))
		return
	}
	if answerNotModified(w, r, s.referencesETag(answer.Bytes(), latestOnly, trees)) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(answer.Bytes())
}

// referencesClosureBound is the budget every statement of one GET of an issue's references shares
// (boundedClosure). A read that reaches it answers 503 REFERENCES_TIMEOUT, which the CLI prints as
// references unavailable, rather than holding a connection and a CPU for as long as the read
// takes. On 2026-11-20 from 00:27Z the eight-hop walk this read replaced held Dispatch's Cloud SQL
// instance at full CPU: every `dispatch read --issue` and the Artifacts tab ask for it, and Query
// Insights gave that one statement 31,299 s of execution in 20 minutes, 12 times every other
// statement together (cybermonklabs/legion#4554).
const referencesClosureBound = 2 * time.Second

// boundedClosure runs refs.Closure, as who reads it, in a read transaction of its own, through
// closureBudget, so that every statement of the read - the out and in statements and each loader
// that names their nodes - runs under what is left of referencesClosureBound since the read
// started. One statement_timeout of the whole bound for the transaction would not hold the bound:
// the read sends several statements, whose sum can pass the context's deadline with none past the
// bound, and at that deadline pgx closes the connection while the server's statement runs on (its
// default BuildContextWatcherHandler, as outbox/lease.go records). The context, a second past the
// bound, stays as the last guard, for a wait no statement_timeout covers, such as the begin's. The
// rollback runs on a context of its own, as the near-copy gate's does (duplicates.go): the read's
// context has expired exactly when the rollback matters, and pgx answers a rollback on an expired
// context by destroying the connection instead of returning it to the pool.
func (s *server) boundedClosure(ctx context.Context, who model.Actor, issueKey string) (refs.References, error) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, referencesClosureBound+time.Second)
	defer cancel()
	tx, err := s.begin(ctx)
	if err != nil {
		return refs.References{}, err
	}
	defer func() {
		rollback, cancelRollback := context.WithTimeout(context.WithoutCancel(ctx), duplicateGateRollback)
		defer cancelRollback()
		_ = tx.Rollback(rollback)
	}()
	return refs.Closure(ctx, closureBudget{tx: tx, started: started}, who, issueKey, s.deps.ServerURL)
}

// closureBudget is boundedClosure's transaction tx as refs.Closure reads it (refs.Queryer). Before
// each statement it passes on, it gives that statement what is left of referencesClosureBound
// since started, the start of the read (setStatementBudget), so a statement that waits, on a lock
// or for the CPU, takes its wait out of the time of every statement after it. It answers the
// budget's refusal in place of a statement it does not send.
type closureBudget struct {
	tx      pgx.Tx
	started time.Time
}

func (b closureBudget) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if err := b.spend(ctx); err != nil {
		return nil, err
	}
	return b.tx.Query(ctx, sql, args...)
}

func (b closureBudget) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if err := b.spend(ctx); err != nil {
		return refusedRow{err: err}
	}
	return b.tx.QueryRow(ctx, sql, args...)
}

// spend gives the next statement of the read what is left of referencesClosureBound since
// started (setStatementBudget).
func (b closureBudget) spend(ctx context.Context) error {
	return setStatementBudget(ctx, b.tx, referencesClosureBound-time.Since(b.started))
}

// refusedRow is the row closureBudget answers for a statement it did not send: its Scan answers
// the refusal.
type refusedRow struct{ err error }

func (r refusedRow) Scan(...any) error { return r.err }

// errClosureBudgetSpent is setStatementBudget's answer when less than a millisecond of
// referencesClosureBound is left before a statement of boundedClosure's read: the read is at its
// bound, and the route answers it as it answers a statement Postgres cancelled there.
var errClosureBudgetSpent = errors.New("the reference closure's budget has less than 1ms left")

// setStatementBudget gives the next statement of boundedClosure's transaction tx what is left of
// referencesClosureBound, left, so that the statements of the read share the one bound rather
// than each having it whole. It sets statement_timeout for the rest of tx to the whole
// milliseconds of left, rounded down, 1ms at 1 ms left, with
// `select set_config('statement_timeout', $1, true)` after refs.ClosureTag, which starts every
// statement of the read, so that pg_stat_activity names this statement as the read's too. Under
// 1 ms left it sends no statement and answers errClosureBudgetSpent, which closureBoundHit reads
// as the bound, because the timeout rounded down would be 0, and a statement_timeout of 0 turns
// the timeout off.
func setStatementBudget(ctx context.Context, tx pgx.Tx, left time.Duration) error {
	milliseconds := left.Milliseconds()
	if milliseconds < 1 {
		return errClosureBudgetSpent
	}
	_, err := tx.Exec(ctx, refs.ClosureTag+"select set_config('statement_timeout', $1, true)", strconv.FormatInt(milliseconds, 10)+"ms")
	return err
}

// closureBoundHit says whether boundedClosure's read ended at its bound: Postgres cancelled a
// statement at the statement_timeout its budget set (SQLSTATE 57014), the budget had less than a
// millisecond left for the next statement (errClosureBudgetSpent), or the context expired first.
func closureBoundHit(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errClosureBudgetSpent) {
		return true
	}
	var pgError *pgconn.PgError
	return errors.As(err, &pgError) && pgError.Code == "57014" // query_canceled
}
