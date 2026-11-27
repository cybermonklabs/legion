package graphcensus

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
)

// The rows a batch of this child's passes locks, as the holds below name their steps; lockstep
// names the owner's lock and the store's own steps. Every writer of an item takes them in this
// order, and so does each batch (backfill_inbox.go): the owner's lock, the lock of the project an
// ask's waiting person is read under (inboxwrite.HoldDefault), the item rows in ascending id, the
// item's block state row (refs' item index) and the rows of its blocks, links or citations, all
// before the item's store writes, and after those a thread's user_threads rows in ascending login.
const (
	stepProject    = "the lock of the project an ask's waiting person is read under"
	stepItem       = "the item rows"
	stepBlockState = "the item's block state row"
	stepItemPart   = "the rows of the item's blocks, links or citations"
	stepThread     = "the thread's user_threads rows"
)

// writersRowOrder is those steps in the order the writers take them.
var writersRowOrder = []string{lockstep.StepOwner, stepProject, stepItem, stepBlockState, stepItemPart, stepThread}

// requireTheWritersRowOrder holds the order a pass's batch queued behind holds in to the writers'
// row order, which lockstep.RequireTheStoreLockOrder does not know: the steps of writersRowOrder in
// that order, the rows of one of them in ascending (Kind, ID), and every row of the item - its owner,
// its project, its own row, its block state and its parts - before any step of the store. Where
// the store falls against a thread's rows it leaves to lockstep's invariants, as the one writer of
// a thread queues its writer's participates_in before it takes the thread's rows
// (inboxwrite.Record's step 1), and requireQueuedBefore holds a reply's deliveries after them.
func requireTheWritersRowOrder(t *testing.T, batch string, queued []*lockstep.Hold) {
	t.Helper()
	order := lockstep.HoldNames(queued)
	rank := func(hold *lockstep.Hold) int { return slices.Index(writersRowOrder, hold.Step) }
	itemRows := slices.Index(writersRowOrder, stepItemPart)
	var previous *lockstep.Hold
	for index, hold := range queued {
		at := rank(hold)
		if at < 0 {
			for _, later := range queued[index+1:] {
				if position := rank(later); position >= 0 && position <= itemRows {
					t.Errorf("%s took %s (%s) after %s (%s): the item's rows are taken before the store is written (queued in order: %s)",
						batch, later.Name, later.Step, hold.Name, hold.Step, order)
				}
			}
			continue
		}
		if previous != nil {
			before := rank(previous)
			switch {
			case at < before:
				t.Errorf("%s took %s (%s) after %s (%s): the writers take %s in that order (queued in order: %s)",
					batch, hold.Name, hold.Step, previous.Name, previous.Step, strings.Join(writersRowOrder, ", "), order)
			case at == before && (hold.Kind < previous.Kind || hold.Kind == previous.Kind && hold.ID < previous.ID):
				t.Errorf("%s took %s before %s: the rows of %s are taken in ascending order (queued in order: %s)",
					batch, previous.Name, hold.Name, hold.Step, order)
			}
		}
		previous = hold
	}
}

// requireQueuedBefore fails unless the batch queued behind earlier before it queued behind later.
func requireQueuedBefore(t *testing.T, batch string, queued []*lockstep.Hold, earlier, later *lockstep.Hold, rule string) {
	t.Helper()
	first, second := slices.Index(queued, earlier), slices.Index(queued, later)
	if first < 0 || second < 0 || first > second {
		t.Errorf("%s took %s before %s: %s (queued in order: %s)", batch, later.Name, earlier.Name, rule, lockstep.HoldNames(queued))
	}
}

// inboxPassOf is the pass of this child's backfill that reports kind.
func inboxPassOf(t *testing.T, kind string) inboxPass {
	t.Helper()
	for _, pass := range inboxPasses() {
		if pass.kind == kind {
			return pass
		}
	}
	t.Fatalf("no pass of the inbox reports %s", kind)
	return inboxPass{}
}

// backfillBeforePass writes what the pass reporting kind reads before it, as the backfill writes it
// before that pass runs: the comments' and the objects' nodes, and every pass of this child before
// it.
func backfillBeforePass(t *testing.T, database *store.Store, kind string) {
	t.Helper()
	ctx := context.Background()
	if _, err := backfillComments(ctx, database); err != nil {
		t.Fatalf("backfill the comments' nodes: %v", err)
	}
	if _, err := backfillObjectNodes(ctx, database); err != nil {
		t.Fatalf("backfill the objects' nodes: %v", err)
	}
	for _, pass := range inboxPasses() {
		if pass.kind == kind {
			return
		}
		if _, err := runInboxPass(ctx, database, pass, &storeGrowth{}); err != nil {
			t.Fatalf("run the %s pass before the %s pass: %v", pass.kind, kind, err)
		}
	}
	t.Fatalf("no pass of the inbox reports %s", kind)
}

// followThePass runs the pass reporting kind alone on a connection of its own while holds hold
// their rows, follows it through them (lockstep.Follow), holds the order it queued in to the
// writers' row order as well, and answers what the pass reports and that order. A later pass would
// take rows the holds stand for in steps of its own, so the pass is followed alone.
func followThePass(t *testing.T, database *store.Store, kind string, holds []*lockstep.Hold) (KindBackfill, []*lockstep.Hold) {
	t.Helper()
	pass := inboxPassOf(t, kind)
	pid, answered := startRun(t, database, func(ctx context.Context, database *store.Store) (BackfillReport, error) {
		done, err := runInboxPass(ctx, database, pass, &storeGrowth{})
		return BackfillReport{Kinds: []KindBackfill{done}}, err
	})
	done := make(chan error, 1)
	var report BackfillReport
	go func() {
		answer := <-answered
		report = answer.report
		done <- answer.err
	}()
	batch := "the " + kind + " pass"
	queued := lockstep.Follow(t, database, batch, pid, done, holds)
	requireTheWritersRowOrder(t, batch, queued)
	return report.Kinds[0], queued
}

// holdToPerson holds a tuple the batch appends through graphstore.ToPerson, by appending the same
// repeat key through ToPerson in a transaction left open, as holdWrite does through Write.
func holdToPerson(t *testing.T, database *store.Store, hold *lockstep.Hold, tuples []graphstore.Tuple) *lockstep.Hold {
	t.Helper()
	lockstep.Begin(t, database, hold)
	if err := graphstore.ToPerson(context.Background(), hold.Tx, tuples); err != nil {
		t.Fatalf("hold %s: %v", hold.Name, err)
	}
	return hold
}

// writePeople commits the nodes of logins, so a hold that names one of them as the far end of a
// delivery holds the delivery alone and never the person's node.
func writePeople(t *testing.T, database *store.Store, logins ...string) {
	t.Helper()
	ctx := context.Background()
	nodes := make([]graphstore.Node, 0, len(logins))
	for _, login := range logins {
		nodes = append(nodes, graphstore.PersonNode(login, backfillActor, time.Now(), graphstore.NoEvent))
	}
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		return graphstore.Write(ctx, tx, nodes, nil)
	}); err != nil {
		t.Fatalf("write the nodes of %v: %v", logins, err)
	}
}

// The holds every subtest below takes alike: the issue's owner lock, which every batch of these
// passes takes first; the project's, which only the ask pass takes; a user_threads row of a
// thread; and an item row.
func ownerHold(t *testing.T, database *store.Store, unlocked ...string) *lockstep.Hold {
	t.Helper()
	return lockstep.HoldOwner(t, database, &lockstep.Hold{
		Name: "issue CENS-1's owner lock", Step: lockstep.StepOwner, Taken: true,
		Statement: lockstep.OwnerLockStatement, Unlocked: unlocked,
	}, store.IssueOwner("CENS-1"))
}

func projectHold(t *testing.T, database *store.Store, taken bool, unlocked ...string) *lockstep.Hold {
	t.Helper()
	return lockstep.HoldOwner(t, database, &lockstep.Hold{
		Name: "project CENS's lock", Step: stepProject, Taken: taken,
		Statement: lockstep.OwnersLockSharedStatement, Unlocked: unlocked,
	}, store.ProjectOwner("CENS"))
}

func threadRowHold(t *testing.T, database *store.Store, thread, login string, taken bool, statement string) *lockstep.Hold {
	t.Helper()
	return lockstep.HoldRow(t, database, &lockstep.Hold{
		Name: login + "'s user_threads row", Step: stepThread, Taken: taken, Kind: thread, ID: login,
		Statement: statement,
	}, `select 1 from user_threads where thread_id = $1::uuid and login = $2 for no key update`, thread, login)
}

func itemRowHold(t *testing.T, database *store.Store, table, id, name string, taken bool, statement string, unlocked ...string) *lockstep.Hold {
	t.Helper()
	return lockstep.HoldRow(t, database, &lockstep.Hold{
		Name: name, Step: stepItem, Taken: taken, Kind: table, ID: id, Statement: statement, Unlocked: unlocked,
	}, `select 1 from `+table+` where id = $1::uuid for no key update`, id)
}

// reachedRowsStatement is how inboxwrite.RecordOlder's lock of the rows a reply reached starts
// (lockReachedAndReadCleared).
const reachedRowsStatement = "select 1 from user_threads where thread_id = $1::uuid and login = any("

// deliveryStatement is how graphstore.ToPerson's write starts: its node arm first.
const deliveryStatement = "with graph_nodes_written as ("

// requireTheCommentPointersBatchLockOrder holds writeCommentPointers to the writers' order. An
// older build's comment and the reply that answers it, written before the pass: the batch takes
// the issue's owner lock, then both comments rows `for no key update` in ascending id, then appends
// the reply's pointers, then, recording what the reply owed its thread (inboxwrite.RecordOlder),
// takes alice's row of the thread - the person who wrote in it before bob - and only then appends
// the reply's delivery to her. It takes neither the project's lock nor bob's own row.
func requireTheCommentPointersBatchLockOrder(t *testing.T) {
	writes := newAddressingWrites(t)
	database := writes.database
	writes.createIssue(t, "CENS-1", 1, "U", alice)
	root := writeAsAnOlderBuild(t, writes, "comment.created", alice, `
		insert into comments (issue_key, author, body) values ('CENS-1', $1, 'Is the copy final?')
		returning id::text, created_at`, actorJSON(t, alice))
	reply := writeAsAnOlderBuild(t, writes, "comment.created", bob, `
		insert into comments (issue_key, author, body, reply_to) values ('CENS-1', $1, 'It is.', $2)
		returning id::text, created_at`, actorJSON(t, bob), root.id)
	backfillBeforePass(t, database, "comment_pointers")

	const read = "select c.id::text, c.author, c.created_at,"
	owner := ownerHold(t, database, "graph_nodes", "graph_tuples", "user_threads")
	project := projectHold(t, database, false)
	rootRow := itemRowHold(t, database, "comments", root.id, "the root comment's row", true, read, "graph_nodes", "graph_tuples", "user_threads")
	replyRow := itemRowHold(t, database, "comments", reply.id, "the reply's row", true, read, "graph_nodes", "graph_tuples", "user_threads")
	repliesTo := holdWrite(t, database, &lockstep.Hold{
		Name: "the reply's replies_to tuple", Step: lockstep.StepTuple, Taken: true,
		Statement: "insert into graph_tuples (",
	}, nil, []graphstore.Tuple{{
		FromKind: graphstore.KindComment, FromID: reply.id, Kind: graphstore.TupleRepliesTo,
		ToKind: graphstore.KindComment, ToID: root.id, At: reply.at, Who: bob, SourceEvent: reply.event, Asserted: true,
	}})
	aliceRow := threadRowHold(t, database, root.id, "alice", true, reachedRowsStatement)
	bobRow := threadRowHold(t, database, root.id, "bob", false, "")
	delivery := holdToPerson(t, database, &lockstep.Hold{
		Name: "the reply's delivery to alice", Step: lockstep.StepTuple, Taken: true, Statement: deliveryStatement,
	}, []graphstore.Tuple{{
		FromKind: graphstore.KindComment, FromID: reply.id, Kind: graphstore.TupleDeliveredTo,
		ToKind: graphstore.KindPerson, ToID: "alice", At: reply.at, Who: bob, SourceEvent: reply.event, Asserted: true,
	}})

	done, queued := followThePass(t, database, "comment_pointers",
		[]*lockstep.Hold{owner, project, rootRow, replyRow, repliesTo, aliceRow, bobRow, delivery})
	requireQueuedBefore(t, "the comment_pointers pass", queued, aliceRow, delivery,
		"the rows a reply reaches are taken before its deliveries are appended")
	if done.Rows != 2 || !done.Complete {
		t.Errorf("the comment pointers pass reports %#v, want both comments and nothing left", done)
	}
}

// requireTheMessagesBatchLockOrder holds writeMessages to the writers' order. An older build's
// issue message and bob's reply to it: the batch takes the issue's owner lock, then both messages
// rows `for no key update` in ascending id, then writes their authors' nodes and appends their
// facts, then takes alice's row of the thread, the row bob's reply reached, and only then appends
// the delivery to her. It takes neither the project's lock nor bob's own row.
func requireTheMessagesBatchLockOrder(t *testing.T) {
	writes := newAddressingWrites(t)
	database := writes.database
	writes.createIssue(t, "CENS-1", 1, "U", alice)
	root := writeAsAnOlderBuild(t, writes, "message.created", alice, `
		insert into messages (issue_key, author, body) values ('CENS-1', $1, 'Ship it on Friday?')
		returning id::text, created_at`, actorJSON(t, alice))
	reply := writeAsAnOlderBuild(t, writes, "message.answered", bob, `
		insert into messages (issue_key, author, body, in_reply_to) values ('CENS-1', $1, 'Friday works.', $2)
		returning id::text, created_at`, actorJSON(t, bob), root.id)
	backfillBeforePass(t, database, "messages")
	writePeople(t, database, "alice")

	const read = "select m.id::text, coalesce(m.issue_key, ''), m.author,"
	owner := ownerHold(t, database, "graph_nodes", "graph_tuples", "user_threads")
	project := projectHold(t, database, false)
	rootRow := itemRowHold(t, database, "messages", root.id, "the root message's row", true, read, "graph_tuples", "user_threads")
	replyRow := itemRowHold(t, database, "messages", reply.id, "the reply's row", true, read, "graph_tuples", "user_threads")
	bobNode := holdWrite(t, database, &lockstep.Hold{
		Name: "person bob's node row", Step: lockstep.StepNode, Taken: true, Kind: graphstore.KindPerson, ID: "bob",
		Statement: "insert into graph_nodes (",
	}, []graphstore.Node{graphstore.PersonNode("bob", backfillActor, time.Now(), graphstore.NoEvent)}, nil)
	aliceRow := threadRowHold(t, database, root.id, "alice", true, reachedRowsStatement)
	bobRow := threadRowHold(t, database, root.id, "bob", false, "")
	delivery := holdToPerson(t, database, &lockstep.Hold{
		Name: "the reply's delivery to alice", Step: lockstep.StepTuple, Taken: true, Statement: deliveryStatement,
	}, []graphstore.Tuple{{
		FromKind: graphstore.KindMessage, FromID: reply.id, Kind: graphstore.TupleDeliveredTo,
		ToKind: graphstore.KindPerson, ToID: "alice", At: reply.at, Who: bob, SourceEvent: reply.event, Asserted: true,
	}})

	done, queued := followThePass(t, database, "messages",
		[]*lockstep.Hold{owner, project, rootRow, replyRow, bobNode, aliceRow, bobRow, delivery})
	requireQueuedBefore(t, "the messages pass", queued, aliceRow, delivery,
		"the rows a reply reaches are taken before its deliveries are appended")
	if done.Rows != 2 || !done.Complete {
		t.Errorf("the messages pass reports %#v, want both messages and nothing left", done)
	}
}

// requireTheMessagesPastBoundBatchLockOrder holds the past-bound pass to the writers' order. bob's
// two replies, which an older build wrote naming a block of alice's message whose node id the store
// cannot key, so the messages pass takes neither: the batch takes the issue's owner lock, then both
// replies' rows `for no key update` in ascending id, and marks them. It writes nothing of the
// store, and takes neither the project's lock, nor alice's message's row, nor a row of the thread.
func requireTheMessagesPastBoundBatchLockOrder(t *testing.T) {
	writes := newAddressingWrites(t)
	database := writes.database
	writes.createIssue(t, "CENS-1", 1, "U", alice)
	root := writeAsAnOlderBuild(t, writes, "message.created", alice, `
		insert into messages (issue_key, author, body) values ('CENS-1', $1, 'Ship it on Friday?')
		returning id::text, created_at`, actorJSON(t, alice))
	block := "message/" + root.id + "#" + strings.Repeat("b", graphstore.MaxAddressBytes)
	first := writeAsAnOlderBuild(t, writes, "message.answered", bob, `
		insert into messages (issue_key, author, body, in_reply_to, replies_to_block) values ('CENS-1', $1, 'Friday works.', $2, $3)
		returning id::text, created_at`, actorJSON(t, bob), root.id, block)
	second := writeAsAnOlderBuild(t, writes, "message.answered", bob, `
		insert into messages (issue_key, author, body, in_reply_to, replies_to_block) values ('CENS-1', $1, 'So does Monday.', $2, $3)
		returning id::text, created_at`, actorJSON(t, bob), root.id, block)
	backfillBeforePass(t, database, "messages_past_bound")

	// The pass reports only the rows it marked (pastBoundTable.markRecorded), so what it wrote of
	// the store is read off the store: its newest tuple and its nodes, before and after.
	storeFigures := func() (newest, nodes int64) {
		if err := database.Pool.QueryRow(context.Background(),
			`select (select coalesce(max(seq), 0) from graph_tuples), (select count(*) from graph_nodes)`).Scan(&newest, &nodes); err != nil {
			t.Fatalf("read the store's newest tuple and its nodes: %v", err)
		}
		return newest, nodes
	}
	newestBefore, nodesBefore := storeFigures()

	const mark = "with held as ( select m.id from messages m"
	unlocked := []string{"graph_nodes", "graph_tuples", "user_threads"}
	owner := ownerHold(t, database, unlocked...)
	project := projectHold(t, database, false)
	rootRow := itemRowHold(t, database, "messages", root.id, "alice's message's row", false, "")
	firstRow := itemRowHold(t, database, "messages", first.id, "bob's first reply's row", true, mark, unlocked...)
	secondRow := itemRowHold(t, database, "messages", second.id, "bob's second reply's row", true, mark, unlocked...)
	aliceRow := threadRowHold(t, database, root.id, "alice", false, "")

	done, _ := followThePass(t, database, "messages_past_bound",
		[]*lockstep.Hold{owner, project, rootRow, firstRow, secondRow, aliceRow})
	if done.Rows != 2 || !done.Complete {
		t.Errorf("the messages_past_bound pass reports %#v, want both replies marked and nothing left", done)
	}
	if newestAfter, nodesAfter := storeFigures(); newestAfter != newestBefore || nodesAfter != nodesBefore {
		t.Errorf("the messages_past_bound pass moved the store from tuple %d and %d nodes to tuple %d and %d nodes, want nothing of the store written",
			newestBefore, nodesBefore, newestAfter, nodesAfter)
	}
}

// requireTheAsksOwedBatchLockOrder holds writeAsksOwed to the writers' order. An ask an older build
// created on an issue nobody is assigned, which bob answered as that build answers: the batch takes
// the issue's owner lock, then the lock of project CENS, whose default assignee the ask waits on
// (inboxwrite.HoldDefault, shared), then the asks row `for no key update`, and, recording bob's
// answer, takes alice's row of the ask's thread - the asker's, which the answer reached - before
// it appends the answer's delivery to her. It does not take bob's own row.
func requireTheAsksOwedBatchLockOrder(t *testing.T) {
	writes := newAddressingWrites(t)
	database := writes.database
	writes.createIssue(t, "CENS-1", 1, "U", alice)
	ask := writeAsAnOlderBuild(t, writes, "ask.opened", alice, `
		insert into asks (issue_key, author, question) values ('CENS-1', $1, 'Which colour?')
		returning id::text, created_at`, actorJSON(t, alice))
	var answerAt time.Time
	answer := writes.write(t, func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
		if err := tx.QueryRow(ctx, `
			update asks set state = 'answered',
			       answer = jsonb_build_object('user', 'bob', 'selected', '[]'::jsonb, 'text', 'Blue.', 'at', clock_timestamp())
			where id = $1::uuid
			returning (answer->>'at')::timestamptz
		`, ask.id).Scan(&answerAt); err != nil {
			return model.Event{}, err
		}
		text := "Blue."
		return model.Event{IssueKey: new("CENS-1"), Type: "ask.answered", Actor: bob, Payload: model.AskAnsweredEventPayload{
			AskEventPayload: model.AskEventPayload{Ask: model.Ask{ID: ask.id, IssueKey: new("CENS-1"), Author: alice,
				Answer: &model.AskAnswer{User: "bob", Selected: []string{}, Text: &text, At: answerAt}}},
		}}, nil
	})
	backfillBeforePass(t, database, "ask_tuples")
	writePeople(t, database, "alice")

	owner := ownerHold(t, database, "graph_nodes", "graph_tuples", "user_threads")
	project := projectHold(t, database, true, "graph_nodes", "graph_tuples", "user_threads")
	askRow := itemRowHold(t, database, "asks", ask.id, "the ask's row", true, "select k.id::text, k.author, k.created_at,",
		"graph_tuples", "user_threads")
	aliceRow := threadRowHold(t, database, ask.id, "alice", true, reachedRowsStatement)
	bobRow := threadRowHold(t, database, ask.id, "bob", false, "")
	delivery := holdToPerson(t, database, &lockstep.Hold{
		Name: "the answer's delivery to alice", Step: lockstep.StepTuple, Taken: true, Statement: deliveryStatement,
	}, []graphstore.Tuple{{
		FromKind: graphstore.KindAsk, FromID: ask.id, Kind: graphstore.TupleDeliveredTo,
		ToKind: graphstore.KindPerson, ToID: "alice", At: answerAt, Who: bob, SourceEvent: answer.ID, Asserted: true,
	}})

	done, queued := followThePass(t, database, "ask_tuples", []*lockstep.Hold{owner, project, askRow, aliceRow, bobRow, delivery})
	requireQueuedBefore(t, "the ask_tuples pass", queued, aliceRow, delivery,
		"the rows an answer reaches are taken before its deliveries are appended")
	if done.Rows != 1 || !done.Complete {
		t.Errorf("the ask pass reports %#v, want the ask and nothing left", done)
	}
}

// requireTheUserThreadsBatchLockOrder holds writeUserThreads to the writers' order. An older
// build's comment and bob's reply, whose own passes have run, so the batch catches nothing up: it
// takes the issue's owner lock, then the thread's rows `for no key update` in ascending login,
// alice's and then bob's, and states and marks them. It takes neither the project's lock nor a
// comments row, since nothing of the thread is owed.
func requireTheUserThreadsBatchLockOrder(t *testing.T) {
	writes := newAddressingWrites(t)
	database := writes.database
	writes.createIssue(t, "CENS-1", 1, "U", alice)
	root := writeAsAnOlderBuild(t, writes, "comment.created", alice, `
		insert into comments (issue_key, author, body) values ('CENS-1', $1, 'Is the copy final?')
		returning id::text, created_at`, actorJSON(t, alice))
	writeAsAnOlderBuild(t, writes, "comment.created", bob, `
		insert into comments (issue_key, author, body, reply_to) values ('CENS-1', $1, 'It is.', $2)
		returning id::text, created_at`, actorJSON(t, bob), root.id)
	backfillBeforePass(t, database, "user_threads")

	const read = "select u.login, u.thread_id::text, u.kind,"
	owner := ownerHold(t, database, "graph_nodes", "graph_tuples")
	project := projectHold(t, database, false)
	comment := itemRowHold(t, database, "comments", root.id, "the root comment's row", false, "")
	aliceRow := threadRowHold(t, database, root.id, "alice", true, read)
	bobRow := threadRowHold(t, database, root.id, "bob", true, read)

	done, _ := followThePass(t, database, "user_threads", []*lockstep.Hold{owner, project, comment, aliceRow, bobRow})
	if done.Rows != 2 || !done.Complete {
		t.Errorf("the user_threads pass reports %#v, want both rows of the thread and nothing left", done)
	}
}

// itemPart is one of the three passes over an item's body: its blocks, their links and its
// citations, each its own pass per item kind.
type itemPart struct {
	name string
	pass func(inboxItemKind) string
}

var itemParts = []itemPart{
	{"blocks", func(kind inboxItemKind) string { return kind.blocks }},
	{"links", func(kind inboxItemKind) string { return kind.links }},
	{"citations", func(kind inboxItemKind) string { return kind.kind + "_refs" }},
}

// requireTheItemBatchLockOrder answers the subtest that holds part's pass over kind's items to the
// writers' order (lockItems, then the pass's own read). An older build's item with one block, a
// link out of it and a citation, each naming no node: the batch takes the issue's owner lock, then
// the item's row, then its block state row, then the row of its block, link or citation
// `for no key update`, and only then writes the store - the block's node, or the link's or the
// citation's mentions tuple. It takes neither the project's lock nor the item's thread row.
func requireTheItemBatchLockOrder(kind inboxItemKind, part itemPart) func(t *testing.T) {
	return func(t *testing.T) {
		writes := newAddressingWrites(t)
		database := writes.database
		ctx := context.Background()
		writes.createIssue(t, "CENS-1", 1, "U", alice)
		writes.createIssue(t, "CENS-2", 2, "V", alice)
		var item olderWrite
		switch kind.kind {
		case graphstore.KindComment:
			item = writeAsAnOlderBuild(t, writes, "comment.created", alice, `
				insert into comments (issue_key, author, body) values ('CENS-1', $1, 'After dispatch://CENS-2.')
				returning id::text, created_at`, actorJSON(t, alice))
		case graphstore.KindMessage:
			item = writeAsAnOlderBuild(t, writes, "message.created", alice, `
				insert into messages (issue_key, author, body) values ('CENS-1', $1, 'After dispatch://CENS-2.')
				returning id::text, created_at`, actorJSON(t, alice))
		default:
			item = writeAsAnOlderBuild(t, writes, "ask.opened", alice, `
				insert into asks (issue_key, author, question) values ('CENS-1', $1, 'After dispatch://CENS-2?')
				returning id::text, created_at`, actorJSON(t, alice))
		}
		block := kind.kind + "/" + item.id + "#b1"
		columns, values := `, made_at, changed_revision, changed_at`, `, now(), 1, now()`
		if kind.kind == graphstore.KindMessage {
			columns, values = `, made_at, changed_at`, `, now(), now()`
		}
		for _, seed := range []struct {
			what, statement string
			args            []any
		}{
			{"block", `insert into ` + kind.blocks + ` (` + kind.column + `, block_id, node_id, position, type, text_sha256, excerpt, created_at` + columns + `)
				values ($1, 'b1', $2, 0, 'paragraph', '', 'After dispatch://CENS-2.', now()` + values + `)`, []any{item.id, block}},
			{"link", `insert into ` + kind.links + ` (` + kind.column + `, block_id, from_node, to_kind, to_id, kind, created_at, source_seq)
				values ($1, 'b1', $2, 'issue', 'CENS-2', 'mentions', now(), 100)`, []any{item.id, block}},
			{"citation", `insert into refs (from_kind, from_id, to_kind, to_id, kind, created_at, source_seq)
				values ($1, $2, 'issue', 'CENS-2', 'mentions', now(), 101)`, []any{kind.kind, item.id}},
			{"block state", `insert into ` + kind.state + ` (` + kind.column + `) values ($1) on conflict do nothing`, []any{item.id}},
		} {
			if _, err := database.Pool.Exec(ctx, seed.statement, seed.args...); err != nil {
				t.Fatalf("write the %s's %s as an older build writes it: %v", kind.kind, seed.what, err)
			}
		}
		passKind := part.pass(kind)
		backfillBeforePass(t, database, passKind)

		earlier := []string{"graph_nodes", "graph_tuples", "user_threads"}
		owner := ownerHold(t, database, earlier...)
		project := projectHold(t, database, false)
		itemRow := itemRowHold(t, database, kind.items, item.id, "the "+kind.kind+"'s row", true,
			"select 1 from "+kind.items+" where id = any($1::uuid[]) order by id for no key update", earlier...)
		state := lockstep.HoldRow(t, database, &lockstep.Hold{
			Name: "the " + kind.kind + "'s block state row", Step: stepBlockState, Taken: true,
			Statement: "select 1 from " + kind.state + " where " + kind.column + " = any(", Unlocked: earlier,
		}, `select 1 from `+kind.state+` where `+kind.column+` = $1::uuid for no key update`, item.id)
		thread := threadRowHold(t, database, item.id, "alice", false, "")
		var own, written *lockstep.Hold
		switch part.name {
		case "blocks":
			own = lockstep.HoldRow(t, database, &lockstep.Hold{
				Name: "the block's row", Step: stepItemPart, Taken: true, Unlocked: []string{"user_threads"},
				Statement: "select b." + kind.column + "::text, b.block_id, b.node_id,",
			}, `select 1 from `+kind.blocks+` where `+kind.column+` = $1::uuid and block_id = 'b1' for no key update`, item.id)
			written = holdWrite(t, database, &lockstep.Hold{
				Name: "the block's node row", Step: lockstep.StepNode, Taken: true, Kind: refs.BlockNodeKind, ID: block,
				Statement: "insert into graph_nodes (",
			}, []graphstore.Node{{
				Kind: refs.BlockNodeKind, ID: block, Supertag: refs.BlockSupertag,
				Visibility: graphstore.VisibilityProject, State: graphstore.StateLive,
				MadeAt: time.Now(), MadeBy: backfillActor, SourceEvent: graphstore.NoEvent,
			}}, nil)
		case "links":
			own = lockstep.HoldRow(t, database, &lockstep.Hold{
				Name: "the link's row", Step: stepItemPart, Taken: true, Unlocked: []string{"user_threads"},
				Statement: "select r." + kind.column + "::text, r.block_id, r.from_node,",
			}, `select 1 from `+kind.links+` where `+kind.column+` = $1::uuid and block_id = 'b1' for no key update`, item.id)
			written = holdWrite(t, database, &lockstep.Hold{
				Name: "the link's mentions tuple", Step: lockstep.StepTuple, Taken: true,
				Statement: "insert into graph_tuples (",
			}, nil, []graphstore.Tuple{{
				FromKind: refs.BlockNodeKind, FromID: block, Kind: graphstore.TupleMentions, ToKind: "issue", ToID: "CENS-2",
				At: time.Now(), Who: alice, SourceEvent: item.event, Asserted: true,
			}})
		default:
			own = lockstep.HoldRow(t, database, &lockstep.Hold{
				Name: "the citation's refs row", Step: stepItemPart, Taken: true, Unlocked: []string{"user_threads"},
				Statement: "select r.from_id, r.to_kind, r.to_id, r.kind,",
			}, `select 1 from refs where from_kind = $1 and from_id = $2 and to_kind = 'issue' and to_id = 'CENS-2' for no key update`,
				kind.kind, item.id)
			written = holdWrite(t, database, &lockstep.Hold{
				Name: "the citation's mentions tuple", Step: lockstep.StepTuple, Taken: true,
				Statement: "insert into graph_tuples (",
			}, nil, []graphstore.Tuple{{
				FromKind: kind.kind, FromID: item.id, Kind: graphstore.TupleMentions, ToKind: "issue", ToID: "CENS-2",
				At: time.Now(), Who: alice, SourceEvent: item.event, Asserted: true,
			}})
		}

		done, _ := followThePass(t, database, passKind, []*lockstep.Hold{owner, project, itemRow, state, own, written, thread})
		if done.Rows != 1 || !done.Complete {
			t.Errorf("the %s pass reports %#v, want the one row and nothing left", passKind, done)
		}
	}
}
