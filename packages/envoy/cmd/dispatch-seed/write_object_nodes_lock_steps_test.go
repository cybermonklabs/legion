package main

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
)

// The statements the steps of an object batch are read by, lower-cased with their whitespace
// collapsed, as lockstep reads the statement a writer waits at: graphstore.Write's insert of the
// node rows (queueNodes), and its append of the base attributes those nodes carry (queueAppend).
const (
	seedNodeWrite   = "insert into graph_nodes ("
	seedTupleAppend = "with written as ("
)

// TestTheSeedsObjectNodeWritesTakeTheStoreLockOrder holds the seed's writes of its issues', its
// asks' and its messages' nodes to the lock order written on api/inbox_read.go's
// markInboxReadThreads, against a real Postgres. writeIssues writes a batch's issue nodes through
// graphstore.Write and then copies the batch's issues rows; writeItemNodes writes the nodes of an
// ask batch (writeAsks) and of a message batch (writeMessages) the same way, before the batch's
// first asks or messages row. The issue batch takes no owner lock; an item batch takes the owner
// lock of each of its items first, in owner key order, since the window write after its rows takes
// each item's (refs.WriteItemWindows). Each batch writes its nodes in ascending (kind, id),
// whatever order it lists them in, graphstore.Write appends the base attributes those nodes carry
// right after them, at the sentinel, and only then does the batch write its rows, each of which
// names its node by its own key. The ask batch appends each ask's opening event after its row;
// that event causes none of the tuples, which carry the sentinel, and the seed numbers it itself.
//
// Each batch is followed through one hold of each step, every hold taken before the batch starts
// (lockstep.Follow), and the test reads where the batch's own backend waits behind each and which
// tables it holds locks on by then. The owner lock is held for the whole run: the project's for
// the issue batch, the lock an issue's create allocates its number under and a project's event is
// appended under, which that batch must never queue behind, and the issue's for the item batches,
// which every ask and message of the plan belongs to (store.LockOwners, the advisory lock every
// writer of the server takes first), which they must queue behind first, holding no lock of a
// node, a tuple or a row. The two nodes are new, so each is held as a row another transaction
// inserted and has not committed; they are listed with the higher one first. The tuples carry the
// sentinel, which no repeat key covers, so the tuple append is held as the table it writes, in
// share mode, which conflicts with an insert's lock and with no read; the node write is the
// batch's first write of graph_tuples. The first row the batch writes is held as a row of its key
// another transaction inserted and has not committed.
//
// Each run is held to the store's invariants (lockstep.RequireTheStoreLockOrder) and to the order
// the code takes its steps in: the owner lock where it takes one, the lower node, the upper node,
// the tuple append, then the row. What the batch wrote is then read back, so the run followed is
// the one that committed.
func TestTheSeedsObjectNodeWritesTakeTheStoreLockOrder(t *testing.T) {
	f := newSeedLockFixture(t)

	t.Run("the issue batch writes its issues' nodes before their rows", func(t *testing.T) {
		// writeIssues writes every issue of the plan, so the plan holds the batch's two issues
		// alone while it runs; the item batches below read the fixture's issue from it.
		issues := f.run.plan.issues
		t.Cleanup(func() { f.run.plan.issues = issues })
		issue := func(number int) issuePlan {
			return issuePlan{
				Key: fmt.Sprintf("%s-%d", seedLockProject, number), Project: 0, Number: number,
				Parent: -1, Rank: fmt.Sprintf("n%d", number), Status: model.IssueStatuses[0],
				CreatedAt: baseTime.Add(time.Duration(number) * time.Hour), Primary: -1,
			}
		}
		// The batch is the first write of graphstore's statements on the run's one connection, and
		// pgx prepares a statement that connection has not cached in a round trip of its own before
		// the batch runs (QueryExecModeCacheStatement): parsing the tuple append takes its row
		// exclusive lock on graph_tuples then, before the node insert runs, which no writer's
		// statement waits on but the tuple hold's share lock does. So an unheld batch of two other
		// issues writes first, and the batch followed below runs its statements as a deployment's
		// warm connection does.
		f.run.plan.issues = []issuePlan{issue(5), issue(4)}
		if err := f.run.writeIssues(context.Background()); err != nil {
			t.Fatalf("write two issues to cache the batch's statements: %v", err)
		}
		f.run.plan.issues = []issuePlan{issue(3), issue(2)}
		nodes := make([]graphstore.Node, len(f.run.plan.issues))
		for index, planned := range f.run.plan.issues {
			nodes[index] = graphstore.IssueNode(planned.Key, seedLockProject, seedAuthor, planned.CreatedAt)
		}
		first := f.run.plan.issues[0]
		f.followObjectBatch(t, objectBatch{
			run:       "the issue batch",
			table:     "issues",
			owner:     ownerLock{name: "the project's owner lock", owner: store.ProjectOwner(seedLockProject)},
			upper:     nodes[0],
			lower:     nodes[1],
			firstRow:  `copy "issues"`,
			rowLabel:  "the issues row of " + first.Key,
			holdRow:   `insert into issues (key, project_key, number, title, created_by, rank) values ($1, $2, $3, 'held', $4::jsonb, 'held')`,
			rowArgs:   []any{first.Key, seedLockProject, first.Number, seedActor},
			write:     f.run.writeIssues,
			keyColumn: "key",
		})
	})

	t.Run("the ask batch writes its asks' nodes before their rows", func(t *testing.T) {
		ask := func(number int) askPlan {
			return askPlan{
				ID:    fmt.Sprintf("5eed0000-0000-4000-a000-%012d", number),
				Issue: 0, Document: -1, BlockDoc: -1, State: "open",
				CreatedAt: baseTime.Add(time.Duration(number) * time.Hour),
			}
		}
		f.run.plan.asks = []askPlan{ask(2), ask(1)}
		nodes := make([]graphstore.Node, len(f.run.plan.asks))
		for index, planned := range f.run.plan.asks {
			nodes[index] = graphstore.AskNode(planned.ID, f.run.itemProject(planned.Issue, planned.Document), nil, seedAuthor, planned.CreatedAt)
		}
		first := f.run.plan.asks[0]
		f.followObjectBatch(t, objectBatch{
			run:       "the ask batch",
			table:     "asks",
			owner:     issueOwnerLock,
			upper:     nodes[0],
			lower:     nodes[1],
			firstRow:  "insert into asks (",
			rowLabel:  "the asks row of " + first.ID,
			holdRow:   `insert into asks (id, issue_key, author, question, options, multiple, urgency, state, created_at, kind) values ($1::uuid, $2, $3::jsonb, 'held', '[]'::jsonb, false, 'med', 'open', $4, 'question')`,
			rowArgs:   []any{first.ID, seedLockIssue, seedActor, first.CreatedAt},
			write:     f.run.writeAsks,
			keyColumn: "id",
		})
	})

	t.Run("the message batch writes its messages' nodes before their rows", func(t *testing.T) {
		message := func(number int) messagePlan {
			return messagePlan{
				ID:    fmt.Sprintf("5eed0000-0000-4000-b000-%012d", number),
				Issue: 0, InReplyTo: -1, RepliesTo: -1, Blocks: 2, MentionTo: -1,
				CreatedAt: baseTime.Add(time.Duration(number) * time.Hour),
			}
		}
		f.run.plan.messages = []messagePlan{message(2), message(1)}
		// The first batch of messages writes the one mention of a session (writeSessionMention).
		f.run.plan.sessions = []string{"seed-lock-session"}
		nodes := make([]graphstore.Node, len(f.run.plan.messages))
		for index, planned := range f.run.plan.messages {
			nodes[index] = graphstore.MessageNode(planned.ID, f.run.itemProject(planned.Issue, -1), nil, seedAuthor, planned.CreatedAt)
		}
		first := f.run.plan.messages[0]
		f.followObjectBatch(t, objectBatch{
			run:       "the message batch",
			table:     "messages",
			owner:     issueOwnerLock,
			upper:     nodes[0],
			lower:     nodes[1],
			firstRow:  "insert into messages (",
			rowLabel:  "the messages row of " + first.ID,
			holdRow:   `insert into messages (id, issue_key, author, body, created_at) values ($1::uuid, $2, $3::jsonb, 'held', $4)`,
			rowArgs:   []any{first.ID, seedLockIssue, seedActor, first.CreatedAt},
			write:     f.run.writeMessages,
			keyColumn: "id",
		})
	})
}

// ownerLock is the owner whose lock a batch is held to: how a failure names it, the owner, and
// whether the batch takes it, first, or must never queue behind it.
type ownerLock struct {
	name  string
	owner store.Owner
	taken bool
}

// issueOwnerLock is the fixture's issue, the owner of every ask and message the item batches write
// and of the opening event the ask batch appends for each ask, which those batches lock first.
var issueOwnerLock = ownerLock{name: "the issue's owner lock", owner: store.IssueOwner(seedLockIssue), taken: true}

// objectBatch is one batch of the seed whose nodes graphstore.Write writes before the batch writes
// the rows that name them: the batch as a failure names it and the table of its rows, the owner
// whose lock it must not take, its two nodes, listed in the plan with upper first, how the
// statement that writes its first row starts, that row as a failure names it and the statement
// that holds its key, and the write that runs the batch. keyColumn is the column of table holding
// the id of the row's node.
type objectBatch struct {
	run, table   string
	owner        ownerLock
	upper, lower graphstore.Node
	firstRow     string
	rowLabel     string
	holdRow      string
	rowArgs      []any
	write        func(context.Context) error
	keyColumn    string
}

// followObjectBatch holds each step of batch, runs it, and follows it through the holds
// (lockstep.Follow): the owner lock where the batch takes one, the node rows in ascending (kind,
// id), the tuple append, then the first row, which names the upper node. It then reads back that
// every row of the batch names a node of the store in the fixture's project with the project the
// node write stated of it.
func (f *seedLockFixture) followObjectBatch(t *testing.T, batch objectBatch) {
	t.Helper()
	ctx := context.Background()
	ownerHold := &lockstep.Hold{Name: batch.owner.name, Step: lockstep.StepOwner}
	if batch.owner.taken {
		ownerHold.Taken, ownerHold.Statement = true, lockstep.OwnerLockStatement
		ownerHold.Unlocked = []string{"graph_nodes", "graph_tuples", batch.table}
	}
	owner := lockstep.HoldOwner(t, f.database, ownerHold, batch.owner.owner)
	node := func(held graphstore.Node) *lockstep.Hold {
		hold := &lockstep.Hold{
			Name: "the node row of " + held.Kind + "/" + held.ID, Step: lockstep.StepNode, Taken: true,
			Kind: held.Kind, ID: held.ID, Statement: seedNodeWrite,
			Unlocked: []string{"graph_tuples", batch.table},
		}
		lockstep.Begin(t, f.database, hold)
		holdNewSeedLockNode(t, hold.Tx, held)
		return hold
	}
	lower, upper := node(batch.lower), node(batch.upper)
	tuples := &lockstep.Hold{
		Name: "the tuple append", Step: lockstep.StepTuple, Taken: true,
		Statement: seedTupleAppend, Unlocked: []string{batch.table},
	}
	lockstep.Begin(t, f.database, tuples)
	if _, err := tuples.Tx.Exec(ctx, `lock table graph_tuples in share mode`); err != nil {
		t.Fatalf("hold the writes of graph_tuples: %v", err)
	}
	row := &lockstep.Hold{
		Name: batch.rowLabel, Step: lockstep.StepProjection, Taken: true, Names: upper,
		Statement: batch.firstRow,
	}
	lockstep.Begin(t, f.database, row)
	if _, err := row.Tx.Exec(ctx, batch.holdRow, batch.rowArgs...); err != nil {
		t.Fatalf("hold %s: %v", batch.rowLabel, err)
	}
	holds := []*lockstep.Hold{owner, upper, lower, tuples, row}
	pid, done := f.startWrite(t, batch.write)
	queued := lockstep.Follow(t, f.database, batch.run, pid, done, holds)
	want := []*lockstep.Hold{lower, upper, tuples, row}
	if batch.owner.taken {
		want = append([]*lockstep.Hold{owner}, want...)
	}
	if !slices.Equal(queued, want) {
		t.Fatalf("%s queued behind %s, want %s: the batch takes its items' owner locks first where it takes any, graphstore.Write takes the node rows in ascending (kind, id) and appends their tuples, and the batch writes its rows after it",
			batch.run, lockstep.HoldNames(queued), lockstep.HoldNames(want))
	}
	ids := []string{batch.lower.ID, batch.upper.ID}
	var written int
	if err := f.database.Pool.QueryRow(ctx, fmt.Sprintf(`
		select count(*) from %[1]s r
		where r.%[2]s::text = any($1)
		  and exists (
			select 1 from graph_nodes n
			where n.kind = $2 and n.id = r.%[2]s::text and n.project_key = $3)
		  and exists (
			select 1 from graph_tuples t
			where t.from_kind = $2 and t.from_id = r.%[2]s::text
			  and t.kind = $4 and t.to_kind = $5 and t.to_id = $3)
	`, batch.table, batch.keyColumn), ids, batch.upper.Kind, seedLockProject,
		graphstore.AttributeProject, graphstore.KindValue).Scan(&written); err != nil {
		t.Fatalf("read back what %s wrote: %v", batch.run, err)
	}
	if written != len(ids) {
		t.Errorf("%s left %d of its %d %s rows naming a node of the store in %s with its project stated",
			batch.run, written, len(ids), batch.table, seedLockProject)
	}
}

// startWrite runs write on the run's one connection and answers that connection's backend and
// the channel the write's result arrives on.
func (f *seedLockFixture) startWrite(t *testing.T, write func(context.Context) error) (uint32, chan error) {
	t.Helper()
	ctx := context.Background()
	var pid int64
	if err := f.run.database.Pool.QueryRow(ctx, `select pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read the backend the seed runs on: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- write(ctx) }()
	return uint32(pid), done
}
