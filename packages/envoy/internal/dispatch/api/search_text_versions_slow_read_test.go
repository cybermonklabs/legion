package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/textversions/textversionstest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The lines of the fill the tests read, by the start of each one's message: a batch's
// (filler.record), the pause's two (textversions' pausedMessage and continuesMessage), the hold's
// take and its loss (tookHoldMessage and lostHoldMessage), and a pass's failure
// (runner.fillUnder), which carries the pass's error, such as a batch's statement past its
// statement_timeout.
const (
	keywordBatchLine      = "dispatch: the keyword fill wrote a batch"
	keywordPausedLine     = "dispatch: the keyword fill paused:"
	keywordContinuesLine  = "dispatch: the keyword fill continues:"
	keywordTookHoldLine   = "dispatch: the keyword fill took its hold"
	keywordLostHoldLine   = "dispatch: the keyword fill's hold is lost"
	keywordPassFailedLine = "dispatch: the keyword fill's pass failed"
)

// keywordArmSlowRead runs the fill beside server on its backlog with the first batch of that fill,
// one that asked its pause while the reads were fast, held at a gate of the test's own before its
// transaction begins (keywordBatchGate), makes an answers read through server's handler slow, and
// opens the gate. It answers once that batch has written and the fill has paused before its next
// batch, with the fill, the transaction holding message_deliveries, on which the read is held, the
// channel the held read answers on, and the index of the batch's line among the fill's lines: the
// first line the fill logged after the read turned slow, the paused line the next.
//
// The read turns slow 800 ms after it began, on the wall clock, and the batch waits through that
// turn at the gate alone. It has begun no transaction there, so none of its bounds runs on it
// (textversions' batchBoundsQuery sets its lock_timeout, statement_timeout and
// idle_in_transaction_session_timeout inside the transaction), and nothing lets it go on but the
// test's opening of the gate, or the end of the fill's context, which ends the batch. So however
// long the read takes to turn slow, or the test's own thread to see it, the batch in flight once
// the read is slow is the one that asked its pause while the reads were fast.
//
// A round (keywordArmRound) in which the fill loses its hold or a pass of it fails, as a loaded
// machine can bring about on correct code (a renewal of the hold that does not reach the database
// within its margin, a batch's statement past its statement_timeout), is taken again, as main's
// row took it again: the read is let go, the round's fill stopped, the keyword table and its
// cursor emptied, and once the reads are fast again the next round's fill runs, its first batch
// held at a gate of its own (keywordEndArmRound, keywordGates). No count bounds the rounds: each
// wait of a round is the shared wait and names what the rounds before it ended in
// (keywordArmRounds), so a fill that ends every round so gives up naming the last line it ended
// one in, whichever wait the give-up comes in. Any other wrong outcome fails the row by its own
// name: before the gate opens, a batch's or a pause's line, or a last line other than the hold's
// take; once it opens, a line other than that batch's (a pause that stops the batch in flight);
// after that batch's, a line other than its pause (another batch, or another line first).
func keywordArmSlowRead(t *testing.T, server keywordServer, answers string) (*textversionstest.Fill, pgx.Tx, chan *httptest.ResponseRecorder, int) {
	t.Helper()
	// The rounds' locks and the transactions they sit in run on the shared wait's context, so a
	// lock that is never granted ends its statement at the give-up (keywordLock).
	bound := testwait.Context(t)
	gates := &keywordGates{}
	gate := gates.arm()
	leases := reserveKeywordFillLeases(t, storetest.Traced(t, server.database.Pool, gates, nil))
	rounds := &keywordArmRounds{}
	for {
		rounds.number++
		fill, holder, held, wrote, ended := keywordArmRound(t, bound, server, answers, leases, gate, rounds)
		if ended == "" {
			return fill, holder, held, wrote
		}
		rounds.again++
		rounds.last = ended
		t.Logf("round %d of arming the slow read ended on %s; it is taken again", rounds.number, ended)
		keywordEndArmRound(t, bound, server, fill, holder, held, rounds)
		gate = gates.arm()
	}
}

// keywordArmRounds is what the rounds of keywordArmSlowRead have read: the number of the round
// under way, how many rounds were taken again, and the line the last of those ended in, as the
// fill wrote it, a lost hold's or a failed pass's with its error. Each wait of a round names it
// (what).
type keywordArmRounds struct {
	number, again int
	last          string
}

// what is fact, a fact a wait of the round under way waits for, named with what the rounds have
// read once one was taken again, so a give-up in any of the waits names the line that ended the
// last round taken again.
func (r *keywordArmRounds) what(fact string) string {
	if r.again == 0 {
		return fact
	}
	return fmt.Sprintf("%s (round %d of arming the slow read; %d rounds so far ended in a lost hold or a failed pass and were taken again, the last on %s)", fact, r.number, r.again, r.last)
}

// keywordArmRound is one round of keywordArmSlowRead: it runs a fill of its own on leases, whose
// first batch gate holds, makes an answers read through server's handler slow and opens the gate.
// It answers the line the round ended in, as the fill wrote it, when the fill lost its hold or a
// pass of it failed, so the round is taken again (keywordEndArmRound), and "" with the index of
// the batch's line once the batch held at the gate has written and the fill's next line is its
// pause. Any other outcome fails the test by its own name. The transaction that locks
// message_deliveries begins and takes its lock on bound, the shared wait's context.
func keywordArmRound(t *testing.T, bound context.Context, server keywordServer, answers string, leases *pgxpool.Pool, gate *keywordBatchGate, rounds *keywordArmRounds) (fill *textversionstest.Fill, holder pgx.Tx, held chan *httptest.ResponseRecorder, wrote int, ended string) {
	t.Helper()
	fill = runKeywordFill(t, server, leases)
	testwait.Until(t, rounds.what("the keyword fill's first batch to reach the gate before its transaction"), func() (string, bool) {
		reached := gate.reached()
		return fmt.Sprintf("at the gate: %t; the fill's log:\n%s", reached, fill.Log()), reached
	})
	holder = keywordBegin(t, bound, server.database, rounds.what("a transaction to lock message_deliveries in"))
	handed := false
	defer func() {
		if !handed {
			_ = holder.Rollback(context.Background())
		}
	}()
	keywordLock(t, bound, server.database, holder, "message_deliveries", "access exclusive",
		rounds.what("the access exclusive lock on message_deliveries, on which the answers read is to wait"))
	held = make(chan *httptest.ResponseRecorder, 1)
	go func() { held <- dispatchRequest(t, server.handler, http.MethodGet, answers, nil, "alice") }()
	testwait.Until(t, rounds.what("the answers read to wait on message_deliveries"), func() (int, bool) {
		queued, err := storetest.LockWaiters(testwait.Context(t), holder)
		if err != nil {
			t.Fatal(err)
		}
		return queued, queued >= 1
	})
	testwait.Until(t, rounds.what("the answers read held by the lock to turn slow"), func() (string, bool) {
		now := time.Now()
		slow := server.latency.Slow(now)
		return fmt.Sprintf("the latency's Slow at %s: %t; the held read answered: %t", now.Format(time.RFC3339Nano), slow, len(held) > 0), slow
	})
	// The fill logs its hold's take before its first batch. A try for the hold that failed before
	// the take is a standby's, after which the fill tries again 10 s later (textversions'
	// runner.standBy): its bound of 5 s covers the first dial of a fresh lease pool, which a loaded
	// machine can take longer than, and the batch held at the gate is still the first. A lost hold
	// or a failed pass, before the gate opens or after, ends the round.
	logged := fill.Lines(t, "")
	for index, line := range logged {
		if keywordEndsRound(line) {
			handed = true
			return fill, holder, held, 0, keywordRawLine(fill, index)
		}
	}
	for _, line := range logged {
		if strings.HasPrefix(line, keywordBatchLine) || strings.HasPrefix(line, keywordPausedLine) {
			t.Fatalf("as the answers read turned slow, with the keyword fill's first batch held at the gate before its transaction, the fill had logged %q: a batch or a pause before that batch:\n%s", logged, fill.Log())
		}
	}
	if len(logged) == 0 || !strings.HasPrefix(logged[len(logged)-1], keywordTookHoldLine) {
		t.Fatalf("as the answers read turned slow, with the keyword fill's first batch held at the gate before its transaction, the fill had logged %q, whose last line is not its hold's take:\n%s", logged, fill.Log())
	}
	wrote = len(logged)
	gate.open()
	// The batch's outcome is the fill's next line: its batch line once it commits, or the line of
	// a hold that was lost, of a pass that failed or of a pause. A batch rests as long as it ran,
	// its wait at the gate included, before the next asks the pause.
	switch first := keywordLineAt(t, fill, wrote, rounds); {
	case keywordEndsRound(first):
		handed = true
		return fill, holder, held, 0, keywordRawLine(fill, wrote)
	case !strings.HasPrefix(first, keywordBatchLine):
		t.Fatalf("once the gate let the batch held through the answers read's turn to slow go on, the fill logged %q, not that batch's line: the batch in flight did not write:\n%s", first, fill.Log())
	}
	switch next := keywordLineAt(t, fill, wrote+1, rounds); {
	case strings.HasPrefix(next, keywordBatchLine):
		t.Fatalf("after the answers read turned slow, the fill wrote the batch in flight and then another without pausing:\n%s", fill.Log())
	case keywordEndsRound(next):
		handed = true
		return fill, holder, held, 0, keywordRawLine(fill, wrote+1)
	case !strings.HasPrefix(next, keywordPausedLine):
		t.Fatalf("after the answers read turned slow, the fill wrote the batch in flight and then logged %q, not its pause:\n%s", next, fill.Log())
	}
	handed = true
	return fill, holder, held, wrote, ""
}

// keywordEndsRound is whether line, a message of the fill's, ends a round of keywordArmSlowRead
// for it to be taken again: the hold's loss, or a pass that failed.
func keywordEndsRound(line string) bool {
	return strings.HasPrefix(line, keywordLostHoldLine) || strings.HasPrefix(line, keywordPassFailedLine)
}

// keywordRawLine is line index of fill's log as the fill wrote it, with its attributes, the error
// of a pass that failed among them: the lines fill.Lines reads are these, in this order.
func keywordRawLine(fill *textversionstest.Fill, index int) string {
	at := 0
	for line := range strings.Lines(fill.Log()) {
		if at == index {
			return strings.TrimSuffix(line, "\n")
		}
		at++
	}
	return ""
}

// keywordEndArmRound ends a round of keywordArmSlowRead that is taken again, as main's row ended
// one: it releases message_deliveries and waits for the held read's answer, stops the round's
// fill and waits for it to return, empties the keyword table and its cursor, so the next round
// has the whole backlog to write, and waits for the answers reads to be fast again, so the next
// round's first batch asks its pause while they are. Each wait names what the rounds have read
// (keywordArmRounds.what), and the transaction that empties the tables runs on bound, the shared
// wait's context.
func keywordEndArmRound(t *testing.T, bound context.Context, server keywordServer, fill *textversionstest.Fill, holder pgx.Tx, held chan *httptest.ResponseRecorder, rounds *keywordArmRounds) {
	t.Helper()
	if err := holder.Rollback(t.Context()); err != nil {
		t.Fatalf("release message_deliveries: %v", err)
	}
	if response := testwait.Receive(t, rounds.what("the held answers read to answer once the lock is released"), held); response.Code != http.StatusOK {
		t.Fatalf("the held answers read answered %d: %s", response.Code, response.Body.String())
	}
	fill.Stop(t, rounds.what("the round's keyword fill to return after its stop"))
	// A batch locks the cursor's row and holds that lock to its end, so the exclusive lock waits
	// out any batch of the stopped fill still running on the server, whose Go side has already let
	// go, and the deletes after it see what that batch committed.
	emptying := keywordBegin(t, bound, server.database, rounds.what("a transaction to empty the keyword table and its cursor in"))
	defer func() { _ = emptying.Rollback(context.Background()) }()
	keywordLock(t, bound, server.database, emptying, "search_text_versions_fill", "exclusive",
		rounds.what("the exclusive lock on the keyword fill's cursor table, to empty it and the keyword table for the next round"))
	for _, statement := range []string{`delete from search_text_versions`, `delete from search_text_versions_fill`} {
		if _, err := emptying.Exec(bound, statement); err != nil {
			keywordBoundFailed(t, bound, rounds.what(fmt.Sprintf("the keyword table and its cursor to be emptied (%s)", statement)), err)
		}
	}
	if err := emptying.Commit(bound); err != nil {
		keywordBoundFailed(t, bound, rounds.what("the emptied keyword table and its cursor to commit"), err)
	}
	testwait.Until(t, rounds.what("the answers reads to be fast again"), func() (string, bool) {
		now := time.Now()
		slow := server.latency.Slow(now)
		return fmt.Sprintf("the latency's Slow at %s: %t", now.Format(time.RFC3339Nano), slow), !slow
	})
}

// keywordBegin begins a transaction on database for a round of keywordArmSlowRead on bound, the
// shared wait's context, and fails t naming what when it cannot (keywordBoundFailed).
func keywordBegin(t *testing.T, bound context.Context, database *store.Store, what string) pgx.Tx {
	t.Helper()
	tx, err := database.Pool.Begin(bound)
	if err != nil {
		keywordBoundFailed(t, bound, what, err)
	}
	return tx
}

// keywordLock takes the lock of mode on table in tx on bound, the shared wait's context
// (testwait.Context), for a round of keywordArmSlowRead. A lock that is never granted ends the
// statement at the give-up, and the failure names what, with the sessions that hold a lock on
// table then (keywordLockHolders).
func keywordLock(t *testing.T, bound context.Context, database *store.Store, tx pgx.Tx, table, mode, what string) {
	t.Helper()
	if _, err := tx.Exec(bound, fmt.Sprintf("lock table %s in %s mode", table, mode)); err != nil {
		keywordBoundFailed(t, bound, what, fmt.Errorf("%w; the sessions holding a lock on %s: %s", err, table, keywordLockHolders(t, database, table)))
	}
}

// keywordBoundFailed fails t on err, which ended a statement of a round of keywordArmSlowRead that
// ran on bound, the shared wait's context: once bound has ended, as the give-up of a wait for
// what, which is how a lock never granted ends; before, as what failing.
func keywordBoundFailed(t *testing.T, bound context.Context, what string, err error) {
	t.Helper()
	if bound.Err() != nil {
		t.Fatalf("gave up waiting for %s, at the shared wait's give-up (testwait.Context): %v", what, err)
	}
	t.Fatalf("%s failed: %v", what, err)
}

// keywordLockHolders is every session that holds a granted lock on table in database's database,
// each with its pid, the lock's mode, its state and the start of its last statement, for the
// failure of a lock to name; or why they could not be read.
func keywordLockHolders(t *testing.T, database *store.Store, table string) string {
	t.Helper()
	rows, err := database.Pool.Query(t.Context(), `
		select format('pid %s holds %s (%s): %s', l.pid, l.mode, a.state, left(a.query, 200))
		  from pg_locks l join pg_stat_activity a on a.pid = l.pid
		 where l.locktype = 'relation' and l.granted
		   and l.database = (select oid from pg_database where datname = current_database())
		   and l.relation = to_regclass($1)
		 order by l.pid`, table)
	if err != nil {
		return fmt.Sprintf("(not read: %v)", err)
	}
	holders, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Sprintf("(not read: %v)", err)
	}
	return fmt.Sprintf("%q", holders)
}

// keywordGates is the tracer (storetest.Traced) of the pool the rounds of keywordArmSlowRead run
// their fills on, one round at a time: it hands each statement to the gate of the round under way,
// which the round arms before its fill starts, once the fill of the round before has returned.
type keywordGates struct {
	current atomic.Pointer[keywordBatchGate]
}

// arm makes a new shut gate, which no batch has reached, the gate of the round under way, and
// answers it.
func (g *keywordGates) arm() *keywordBatchGate {
	gate := newKeywordBatchGate()
	g.current.Store(gate)
	return gate
}

func (g *keywordGates) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if gate := g.current.Load(); gate != nil {
		return gate.TraceQueryStart(ctx, conn, data)
	}
	return ctx
}

func (g *keywordGates) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// keywordBatchGate is the gate of one round of keywordArmSlowRead, to which the tracer of the pool
// the keyword fill alone runs on (keywordGates) hands the statements of that round's fill: it holds
// the first transaction begun from starting until the test opens the gate. The fill begins a
// transaction for each of its batches and for nothing else (textversions' filler.batch; its hold's
// statements and its read of where it stands run outside one), so the first is its first batch's,
// which asked its pause the moment before (filler.batchesTo). The gate holds the BEGIN before it
// is sent, so the server holds no transaction of the batch's yet. A context that ends lets the
// batch go too, so a fill stopped at the round's end or the test's does not wait there.
type keywordBatchGate struct {
	first sync.Once
	// at is closed once the first BEGIN is at the gate, opened once the test opens the gate.
	at, opened chan struct{}
}

// newKeywordBatchGate is a shut gate no batch has reached.
func newKeywordBatchGate() *keywordBatchGate {
	return &keywordBatchGate{at: make(chan struct{}), opened: make(chan struct{})}
}

func (g *keywordBatchGate) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !strings.HasPrefix(data.SQL, "begin") {
		return ctx
	}
	held := false
	g.first.Do(func() {
		held = true
		close(g.at)
	})
	if held {
		select {
		case <-g.opened:
		case <-ctx.Done():
		}
	}
	return ctx
}

func (g *keywordBatchGate) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// reached is whether the fill's first batch has reached the gate.
func (g *keywordBatchGate) reached() bool {
	select {
	case <-g.at:
		return true
	default:
		return false
	}
}

// open lets the batch held at the gate, and every later one, go on.
func (g *keywordBatchGate) open() {
	close(g.opened)
}

// keywordLineAt waits for fill to have logged its line index, and answers that line's message.
// Its one bound is the test binary's deadline (testwait.Until): a batch rests as long as it ran,
// which nothing in the fill bounds, before the next asks the pause. The wait names what the rounds
// of keywordArmSlowRead have read (keywordArmRounds.what).
func keywordLineAt(t *testing.T, fill *textversionstest.Fill, index int, rounds *keywordArmRounds) string {
	t.Helper()
	return testwait.Until(t, rounds.what(fmt.Sprintf("the keyword fill to log its line %d", index+1)), func() (string, bool) {
		lines := fill.Lines(t, "")
		if len(lines) > index {
			return lines[index], true
		}
		return fmt.Sprintf("%d lines:\n%s", len(lines), fill.Log()), false
	})
}

// keywordAwaitLineFrom waits for fill to log, at its line from or later, a line whose message
// starts with prefix, and answers that line's index among all its lines. Its one bound is the
// test binary's deadline (testwait.Until).
func keywordAwaitLineFrom(t *testing.T, fill *textversionstest.Fill, from int, prefix string) int {
	t.Helper()
	return testwait.Until(t, fmt.Sprintf("the keyword fill to log a %q line", prefix), func() (int, bool) {
		lines := fill.Lines(t, "")
		for index := from; index < len(lines); index++ {
			if strings.HasPrefix(lines[index], prefix) {
				return index, true
			}
		}
		return len(lines), false
	})
}
