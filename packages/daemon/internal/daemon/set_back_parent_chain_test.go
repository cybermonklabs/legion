package daemon

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// setBackChain is a parent rule Dispatch (parentRuleDispatch) over issues the test seeds, with the
// outbox that runs a set-back row of each, and the log that outbox writes.
type setBackChain struct {
	t       *testing.T
	pool    *store.Pool
	records *record.Postgres
	client  *parentRuleDispatch
	runner  *outbox
	logs    *syncBuffer
}

// newSetBackChain seeds one recorded issue for each of keys, each under the key before it (the
// first under none), in work in the record and at backlog in Dispatch, where a session moved the
// whole chain out of work (each shown at sequence 3). Dispatch's own read of each issue names its
// parent. onCopy also puts that parent in the Dispatch copy (dispatch_issues) and none in the
// record's own (issues.parent); otherwise the record's own names it and the copy holds no row, so
// the outbox holds no row behind its parent's and each waits on Dispatch's read (awaitParentInWork).
func newSetBackChain(t *testing.T, onCopy bool, keys ...string) *setBackChain {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	client := &parentRuleDispatch{issues: map[string]dispatch.Issue{}}
	for i, key := range keys {
		issue := record.Issue{Key: key, Status: "in_progress", DispatchStatus: "backlog", LastDispatchSeq: 3, SeenSeq: 3}
		shown := dispatch.Issue{Key: key, Status: "backlog", LastSeq: 3}
		if i > 0 {
			parent := keys[i-1]
			shown.Parent = &parent
			if onCopy {
				if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
					return records.PutDispatchIssue(ctx, tx, record.DispatchReading{Key: key, Project: "LEGION", Seq: 3,
						Status: record.Carry("backlog"), Parent: record.Carry(parent)})
				}); err != nil {
					t.Fatalf("seed the copy's row of %s: %v", key, err)
				}
			} else {
				issue.Parent = &parent
			}
		}
		seedRecord(t, pool, issue)
		client.issues[key] = shown
	}
	logs := &syncBuffer{}
	return &setBackChain{t: t, pool: pool, records: records, client: client, logs: logs,
		runner: &outbox{dispatchProject: "LEGION", pool: pool, records: records, dispatch: client, now: time.Now,
			log: slog.New(slog.NewJSONHandler(logs, nil))}}
}

// reassert queues the set-back of key from backlog to in_progress, as a session's write at
// sequence 3 owes (record.ReassertStatusWrite), due now.
func (c *setBackChain) reassert(key string) {
	c.t.Helper()
	c.reassertAt(key, time.Now())
}

// reassertAt is reassert due at the given moment, which a test that runs the outbox on a held
// clock takes from that clock.
func (c *setBackChain) reassertAt(key string, at time.Time) {
	c.t.Helper()
	enqueueOutbox(c.t, c.pool, c.records, mustOutboxRow(c.t, key, record.ReassertStatusWrite("in_progress", "backlog", 3), at))
}

// run makes every status row due and drains the outbox once, rounds times.
func (c *setBackChain) run(rounds int) {
	c.t.Helper()
	for range rounds {
		if _, err := c.pool.Exec(context.Background(), "update outbox set next_at = now() where kind = 'dispatch_status'"); err != nil {
			c.t.Fatalf("make the rows due: %v", err)
		}
		if err := c.runner.RunOnce(context.Background()); err != nil {
			c.t.Fatalf("run the rows: %v", err)
		}
	}
}

// wantSetBack holds the chain to R24's own words: no write of the daemon's was refused, and every
// one of keys is in work as the daemon set it, with no row left to write.
func (c *setBackChain) wantSetBack(keys ...string) {
	c.t.Helper()
	if len(c.client.refused) != 0 {
		c.t.Errorf("the writes Dispatch's parent rule refused = %v, want none", c.client.refused)
	}
	if lines := loggedLines(c.t, c.logs.String(), "outbox status write refused by Dispatch's parent status rule; the row ends and its caller is told"); len(lines) != 0 {
		c.t.Errorf("the parent-rule refusal lines = %v, want none", lines)
	}
	for _, key := range keys {
		if got := c.client.issues[key].Status; got != "in_progress" {
			c.t.Errorf("Dispatch shows %s %s after the set-backs, want in_progress", key, got)
		}
		if left := statusRowsOf(c.t, c.pool, key); left != 0 {
			c.t.Errorf("%s keeps %d status rows, want none", key, left)
		}
	}
}

// R24 of dispatch://INFRA-3320 over a chain of three: a phase worker of the root moves the root
// and its child and grandchild to backlog with include_children, and Dispatch shows all three at
// backlog, each by an event of that session. The daemon sets each back to in_progress, a row on
// each issue's lane, and the events come deepest first, so the grandchild's row is the oldest.
// Each write into work under a parent out of work is one Dispatch refuses, so each row waits for
// its parent's queued write into work, whichever level that is: the root's first, then the
// child's, then the grandchild's. The parent a row waits for is Dispatch's, never the record's own
// (issues.parent). Where the copy names it, the row is not claimable until the parent's row has
// finished, so it never runs to wait. Where the copy holds no row of the issue, the row runs and
// waits on its backoff while Dispatch's read shows its parent out of work (awaitParentInWork), so
// what that case holds is that every set-back lands in the end and none is refused.
func TestTheSetBackOfAChainOfThreeNeverBreaksTheRule(t *testing.T) {
	t.Parallel()
	const root, child, grandchild = "LEGION-1", "LEGION-2", "LEGION-3"
	for _, tc := range []struct {
		name   string
		onCopy bool
	}{
		{name: "parents the copy holds no row of", onCopy: false},
		{name: "parents only the Dispatch copy names", onCopy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			chain := newSetBackChain(t, tc.onCopy, root, child, grandchild)
			for _, key := range []string{grandchild, child, root} {
				chain.reassert(key)
			}
			chain.run(4)
			chain.wantSetBack(root, child, grandchild)
			if tc.onCopy {
				chain.wantNoWaits()
			}
		})
	}
}

// wantNoWaits holds the outbox to never having run a row it should have held: a row held behind its
// parent's row is not claimable (record.outboxInOrder), so it never reaches its executor and never
// waits there on its parent (awaitParentInWork).
func (c *setBackChain) wantNoWaits() {
	c.t.Helper()
	if waits := loggedLines(c.t, c.logs.String(), "outbox row waits"); len(waits) != 0 {
		c.t.Errorf("rows that ran and waited = %v, want none: each row stays unclaimable until its parent's row has finished", waits)
	}
}

// A write that does not put its parent's row behind it waits for nothing: a child's write into
// work runs at once, though the parent the Dispatch copy names has a row queued, when that row
// writes a status out of work (the parent's own park) and when the child's row is its own parent's
// (a copy that names an issue as its own parent, which no row may wait on for ever).
func TestASetBackWaitsOnlyForAParentsWriteIntoWork(t *testing.T) {
	t.Parallel()
	const root, child = "LEGION-1", "LEGION-2"
	t.Run("a parent whose queued write puts it out of work", func(t *testing.T) {
		t.Parallel()
		chain := newSetBackChain(t, true, root, child)
		chain.client.issues[root] = dispatch.Issue{Key: root, Status: "in_progress", LastSeq: 3}
		chain.reassert(child)
		enqueueOutbox(t, chain.pool, chain.records, mustOutboxRow(t, root, record.ReassertStatusWrite("backlog", "in_progress", 3), time.Now()))
		chain.run(1)
		if got := chain.client.issues[child].Status; got != "in_progress" {
			t.Errorf("Dispatch shows %s %s after one drain, want in_progress: its parent's write out of work holds nothing back", child, got)
		}
		if got := chain.client.issues[root].Status; got != "backlog" {
			t.Errorf("Dispatch shows %s %s, want backlog as its own write set it", root, got)
		}
	})
	t.Run("an issue the copy shows as its own parent", func(t *testing.T) {
		t.Parallel()
		chain := newSetBackChain(t, true, root)
		self := root
		chain.client.issues[root] = dispatch.Issue{Key: root, Status: "in_progress", Parent: &self, LastSeq: 3}
		putCopyRow(t, chain.pool, root, root, "in_progress")
		enqueueOutbox(t, chain.pool, chain.records, mustOutboxRow(t, root, record.ReassertStatusWrite("testing", "in_progress", 3), time.Now()))
		chain.run(1)
		if got := chain.client.issues[root].Status; got != "testing" {
			t.Errorf("Dispatch shows %s %s after one drain, want testing: a row never waits for its own issue's row", root, got)
		}
	})
}

// A session's move of a parent and its child out of work together reaches the daemon child first
// (Dispatch writes the child first so the rule holds after each step), and the child's row can
// run before the parent's event has been applied and queued the parent's row, which nothing in the
// outbox yet holds it behind. The child's write is not made then: the record still has the parent
// in work while Dispatch shows it at backlog, so the row waits on its backoff, which no
// PARENT_STATUS_CONFLICT refusal ends. Once the parent's row is queued the child's is held behind
// it, since the copy the child's own event wrote names the parent, and both end in work.
func TestASetBackOfAChildWaitsForItsParentsEventToArrive(t *testing.T) {
	t.Parallel()
	const root, child = "LEGION-1", "LEGION-2"
	chain := newSetBackChain(t, true, root, child)
	chain.reassert(child)
	chain.run(1)
	if len(chain.client.refused) != 0 {
		t.Fatalf("the writes Dispatch's parent rule refused = %v, want none: the row waits for its parent's event", chain.client.refused)
	}
	if got := chain.client.issues[child].Status; got != "backlog" {
		t.Fatalf("Dispatch shows %s %s after the first drain, want backlog: nothing may write it into work under a parent at backlog", child, got)
	}
	if left := statusRowsOf(t, chain.pool, child); left != 1 {
		t.Fatalf("%s keeps %d status rows after the first drain, want the one waiting", child, left)
	}
	if waits := loggedLines(t, chain.logs.String(), "outbox row waits"); len(waits) != 1 || !strings.Contains(waits[0]["error"].(string), "waits for its parent "+root) {
		t.Fatalf("wait lines = %v, want the one naming %s as the parent the row waits for", waits, root)
	}
	chain.reassert(root)
	chain.run(3)
	chain.wantSetBack(root, child)
}

// The lanes hold a child's row behind the parent the Dispatch copy names (a held row takes no
// lease, so its lane finds nothing due and exits) and nothing but the parent's finish wakes it
// again: the child's set-back runs once the parent's row has finished, and the grandchild's once
// the child's has, with the sweep never fired. Without that wake each would wait for the sweep, up
// to a lease later.
func TestASetBackRunsAsSoonAsItsParentsRowFinishesOnTheLanes(t *testing.T) {
	t.Parallel()
	const root, child, grandchild = "LEGION-1", "LEGION-2", "LEGION-3"
	chain := newSetBackChain(t, true, root, child, grandchild)
	clock := microClock()
	stop := runLanes(t, chain.runner, clock)
	for _, key := range []string{grandchild, child, root} {
		chain.reassertAt(key, clock.Now())
	}
	// No commit hook wakes a lane for a row queued outside a fact, so each lane is woken as the
	// fact that queued its row would have woken it.
	for _, key := range []string{grandchild, child, root} {
		chain.runner.wake(key, record.OutboxKindDispatchStatus)
	}
	testwait.WithinPolling(t, 30*time.Second, 25*time.Millisecond, "every set-back ran, each after its parent's", func() bool {
		return statusRowsOf(t, chain.pool, root)+statusRowsOf(t, chain.pool, child)+statusRowsOf(t, chain.pool, grandchild) == 0
	})
	stop()
	chain.wantSetBack(root, child, grandchild)
	chain.wantNoWaits()
}

// A drop of a parent's queued status writes (DropStatusWrites: a person's move of the parent out
// of the workflow, or its leave) lifts the hold the parent's write into work put on its child's,
// as that row's finish does. The child's lane found its row held and exited, and nothing else
// wakes it, so the drop's commit wakes it (projectRecords.DropStatusWrites): the child's set-back
// runs with the sweep never fired. Dispatch shows the parent in work already, so its rule takes
// the child's write. Without that wake the child's row would wait for the sweep, up to a lease
// later.
func TestADropOfAParentsStatusWritesWakesItsHeldChildsLane(t *testing.T) {
	t.Parallel()
	const root, child = "LEGION-1", "LEGION-2"
	chain := newSetBackChain(t, true, root, child)
	inWork := chain.client.issues[root]
	inWork.Status = "in_progress"
	chain.client.issues[root] = inWork
	clock := microClock()
	stop := runLanes(t, chain.runner, clock)
	// The root's write into work is not due on the held clock, so only the drop ends it.
	chain.reassertAt(root, clock.Now().Add(time.Hour))
	chain.reassertAt(child, clock.Now())
	chain.runner.wake(child, record.OutboxKindDispatchStatus)
	testwait.WithinPolling(t, 30*time.Second, 25*time.Millisecond, "the child's lane exited with its row held", func() bool {
		return liveLanes(chain.runner) == 0
	})
	if left := statusRowsOf(t, chain.pool, child); left != 1 {
		t.Fatalf("%s keeps %d status rows before the drop, want its set-back held behind %s's write into work", child, left, root)
	}
	records := projectRecords{Store: chain.records, project: "LEGION", wake: chain.runner.wake, log: quietLogger()}
	if err := store.Transact(context.Background(), chain.pool, func(ctx context.Context, tx pgx.Tx) error {
		return records.DropStatusWrites(ctx, tx, root)
	}); err != nil {
		t.Fatalf("drop the status writes of %s: %v", root, err)
	}
	testwait.WithinPolling(t, 30*time.Second, 25*time.Millisecond, "the child's set-back ran once the drop committed", func() bool {
		return statusRowsOf(t, chain.pool, child) == 0
	})
	stop()
	chain.wantSetBack(root, child)
	chain.wantNoWaits()
}

// A person took the child out from under its parent: Dispatch's read of the child names no parent
// and the copy's row of it shows none, while the record's own issues.parent still names the parent
// it had. The child's set-back into work waits for nothing of that former parent's, since the
// parent a row waits for is Dispatch's and never the record's: the write lands while the former
// parent's own write into work stands queued, not due on the held clock the outbox runs on, and no
// finish of that row would wake the child's lane (record.WaitingChildStatusIssues).
func TestASetBackOfADetachedChildWaitsForNoWriteOfItsFormerParent(t *testing.T) {
	t.Parallel()
	const root, child = "LEGION-1", "LEGION-2"
	chain := newSetBackChain(t, false, root, child)
	detached := chain.client.issues[child]
	detached.Parent = nil
	chain.client.issues[child] = detached
	putCopyRow(t, chain.pool, child, "", "backlog")
	clock := microClock()
	chain.runner.now, chain.runner.clock = clock.Now, clock
	chain.reassertAt(child, clock.Now())
	chain.reassertAt(root, clock.Now().Add(time.Hour))
	var waiting []string
	if err := store.TransactWith(context.Background(), chain.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		waiting, err = record.WaitingChildStatusIssues(ctx, tx, root)
		return err
	}); err != nil {
		t.Fatalf("list the status rows waiting on %s: %v", root, err)
	}
	if len(waiting) != 0 {
		t.Errorf("the status rows waiting on %s = %v, want none: Dispatch shows %s under no parent", root, waiting, child)
	}
	if err := chain.runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run the rows: %v", err)
	}
	chain.wantSetBack(child)
	if left := statusRowsOf(t, chain.pool, root); left != 1 {
		t.Errorf("%s keeps %d status rows, want its one write into work still queued: the child's write never waited for it", root, left)
	}
	if got := chain.client.issues[root].Status; got != "backlog" {
		t.Errorf("Dispatch shows %s %s, want backlog: its own write is not due yet", root, got)
	}
}
