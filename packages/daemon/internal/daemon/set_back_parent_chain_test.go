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
// whole chain out of work (each shown at sequence 3). onCopy puts each issue's parent in the
// Dispatch copy (dispatch_issues) and none in the record's own (issues.parent); otherwise the
// record's own names it and the copy holds no row.
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

// reassertAt is reassert due at the given moment, which a test that runs the lanes on a held
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
// child's, then the grandchild's. The parent a row waits for is the one the Dispatch copy names,
// and, where the copy holds no row of the issue, the record's own.
func TestTheSetBackOfAChainOfThreeNeverBreaksTheRule(t *testing.T) {
	t.Parallel()
	const root, child, grandchild = "LEGION-1", "LEGION-2", "LEGION-3"
	for _, tc := range []struct {
		name   string
		onCopy bool
	}{
		{name: "parents the record names", onCopy: false},
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
			chain.wantNoWaits()
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
// work runs at once, though its parent has a row queued, when that row writes a status out of work
// (the parent's own park) and when the child's row is its own parent's (a record that names an
// issue as its own parent, which no row may wait on for ever).
func TestASetBackWaitsOnlyForAParentsWriteIntoWork(t *testing.T) {
	t.Parallel()
	const root, child = "LEGION-1", "LEGION-2"
	t.Run("a parent whose queued write puts it out of work", func(t *testing.T) {
		t.Parallel()
		chain := newSetBackChain(t, false, root, child)
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
	t.Run("an issue recorded as its own parent", func(t *testing.T) {
		t.Parallel()
		chain := newSetBackChain(t, false, root)
		self := root
		chain.client.issues[root] = dispatch.Issue{Key: root, Status: "in_progress", Parent: &self, LastSeq: 3}
		if _, err := chain.pool.Exec(context.Background(), "update issues set parent = $1 where key = $1", root); err != nil {
			t.Fatalf("record %s as its own parent: %v", root, err)
		}
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
// it, and both end in work.
func TestASetBackOfAChildWaitsForItsParentsEventToArrive(t *testing.T) {
	t.Parallel()
	const root, child = "LEGION-1", "LEGION-2"
	chain := newSetBackChain(t, false, root, child)
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

// The lanes hold a child's row behind its parent's (a held row takes no lease, so its lane finds
// nothing due and exits) and nothing but the parent's finish wakes it again: the child's set-back
// runs once the parent's row has finished, and the grandchild's once the child's has, with the
// sweep never fired. Without that wake each would wait for the sweep, up to a lease later.
func TestASetBackRunsAsSoonAsItsParentsRowFinishesOnTheLanes(t *testing.T) {
	t.Parallel()
	const root, child, grandchild = "LEGION-1", "LEGION-2", "LEGION-3"
	chain := newSetBackChain(t, false, root, child, grandchild)
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
