package graphcensus

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// The roll passes whose owed rows no index held before this release's partial indexes read them
// through those indexes, which hold the owed rows alone in the order each pass walks them: the
// user_threads rows whose marked_seq is null, the citations of an item whose tuples_stored is
// false, the rows of the five other read-state tables whose tuples_stored_at is null, the
// comment_mentions rows and the comments whose tuples_stored is false, and the documents whose
// identities_stated is false; and the published-delivery pass and the pass over an older build's
// events read their queues (events_published_owed, events_inbox_owed) through their keys. Without them,
// each tick that owes the catch-up read each of those tables whole (CatchUpOwedRows). The plan is
// taken with sequential scans off, since a fixture's few rows are read fastest whole however the
// statement is written; the index a plan then reads by is the one its predicate and its order let
// it read by.
func TestTheRollPassesReadTheirOwedRowsThroughThePartialIndexes(t *testing.T) {
	database := storetest.Open(t)
	readsBy := map[string]string{
		"user_threads":                "user_threads_unmarked",
		"comment_refs":                "refs_item_tuples_unstored",
		"message_refs":                "refs_item_tuples_unstored",
		"ask_refs":                    "refs_item_tuples_unstored",
		"user_agent_read":             "user_agent_read_unmarked",
		"user_agent_state":            "user_agent_state_unmarked",
		"user_agent_reply_read":       "user_agent_reply_read_unmarked",
		"user_agent_thread_dismissed": "user_agent_thread_dismissed_unmarked",
		"user_ask_snooze":             "user_ask_snooze_unmarked",
		"comment_mentions":            "comment_mentions_tuples_unstored",
		"comment_pointers":            "comments_tuples_unstored",
		"comments_past_bound":         "comments_tuples_unstored",
		"published_destinations":      "events_published_owed_pkey",
		"inbox_events":                "events_inbox_owed_pkey",
		"block_identities":            "document_block_versions_identities_unstated",
	}
	read := map[string]bool{}
	for _, pass := range rollPasses() {
		index, held := readsBy[pass.kind]
		if !held {
			continue
		}
		read[pass.kind] = true
		args := make([]any, 0, len(pass.start)+1)
		for _, key := range pass.start {
			args = append(args, key)
		}
		plan := planWithoutSeqScans(t, database, pass.batch, append(args, backfillBatch)...)
		scans := explaintest.IndexScans(t, plan)
		if !slices.ContainsFunc(scans, func(scan explaintest.IndexScan) bool { return scan.Index == index }) {
			t.Errorf("the %s pass reads its rows through %v, want %s: %s", pass.kind, scans, index, plan)
		}
	}
	for kind := range readsBy {
		if !read[kind] {
			t.Errorf("no roll pass of kind %s, which this test holds to an index", kind)
		}
	}
}

// A comment a build before this one writes while a run of the backfill is under way, after its
// comment pass read the table, holds no node until a later run or the block-index tick gives it
// one, so the run's thread pass cannot state what it owed its thread. The thread pass leaves the
// rows of that thread unmarked (threadItemsStored) rather than mark them at a version that already
// counts the comment's write, and the next run gives the comment its node, states its delivery to
// the person who wrote in the thread before it, and marks the rows above it, so R6's equation
// counts that delivery once. Its control is the thread pass without threadItemsStored: the rows are
// marked in the first run, and the delivery the second run states stands above alice's mark.
func TestTheThreadPassLeavesTheRowsOfAThreadWhoseCommentHoldsNoNodeYet(t *testing.T) {
	writes := newAddressingWrites(t)
	writes.createIssue(t, "CENS-1", 1, "U", alice)
	ctx := context.Background()
	thread := writeAsAnOlderBuild(t, writes, "comment.created", alice, `
		insert into comments (issue_key, author, body) values ('CENS-1', $1, 'Is the copy final?')
		returning id::text, created_at`, actorJSON(t, alice))
	// The run's comment pass reads the table and gives alice's comment its node.
	if _, err := backfillComments(ctx, writes.database); err != nil {
		t.Fatalf("run the comment pass: %v", err)
	}
	// bob's reply lands after that read: it holds no node, and the old trigger raised alice's row.
	writeAsAnOlderBuild(t, writes, "comment.created", bob, `
		insert into comments (issue_key, author, body, reply_to) values ('CENS-1', $1, 'It is.', $2)
		returning id::text, created_at`, actorJSON(t, bob), thread.id)
	if _, err := backfillInbox(ctx, writes.database); err != nil {
		t.Fatalf("run the inbox's passes: %v", err)
	}
	for _, login := range []string{"alice", "bob"} {
		if row := readThreadRow(t, writes.database, login, thread.id); row.markedSeq != nil {
			t.Errorf("%s's row of the thread is marked at %d while bob's reply holds no node, want it left owed", login, *row.markedSeq)
		}
	}
	backfillInboxOnce(t, writes.database)
	requireMarkedAtItsVersion(t, writes.database, "alice", thread.id, 1)
	requireMarkedAtItsVersion(t, writes.database, "bob", thread.id, 0)
}

// Each event a build before this one appends with a write that leaves a row of the inbox's caches
// owed makes the block-index tick owe its catch-up (EventsOweObjectPass), through a keyed read of
// the rows that event names, and stops making it owe once the catch-up has marked them: a person's
// read-state events, an answer, an item's create and edit, a comment's create beside the delivery
// routes it names, written with the comment's node as a build that writes the store's comment node
// writes it, a session's comment written so, whose pointers alone it owes, and an attempt's
// receipt and acceptance.
// Each case writes its rows and its event as that build does, with this build's marker writer
// setting off, and the object pass gives the item its node first, so the read answers true on the
// inbox's rows alone. Its control is rollOwedByEvents answering false: every case owes nothing.
func TestAnEventOwesTheCatchUpWhileItLeavesAnInboxRowOwed(t *testing.T) {
	writes := newAddressingWrites(t)
	writes.createIssue(t, "CENS-1", 1, "U", alice)
	ctx := context.Background()
	thread := writeAsAnOlderBuild(t, writes, "comment.created", alice, `
		insert into comments (issue_key, author, body) values ('CENS-1', $1, 'Is the copy final?')
		returning id::text, created_at`, actorJSON(t, alice))
	asked := writeAsAnOlderBuild(t, writes, "ask.opened", alice, `
		insert into asks (issue_key, author, question) values ('CENS-1', $1, 'Ready?')
		returning id::text, created_at`, actorJSON(t, alice))
	backfillInboxOnce(t, writes.database)

	var message string
	issue := new("CENS-1")
	project := new("CENS")
	for _, event := range []struct {
		name  string
		write func(ctx context.Context, tx pgx.Tx) (model.Event, error)
	}{
		{"a read mark's user_thread_state.updated", func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
			_, err := tx.Exec(ctx, `update user_threads set read_through = clock_timestamp() where login = 'alice' and thread_id = $1::uuid`, thread.id)
			return model.Event{ProjectKey: project, Type: "user_thread_state.updated", Actor: alice,
				Payload: model.UserThreadStateEventPayload{Login: "alice"}}, err
		}},
		{"a snooze's user_state.updated", func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
			_, err := tx.Exec(ctx, `insert into user_ask_snooze (login, ask_id, snoozed_until) values ('alice', $1::uuid, clock_timestamp() + interval '1 day')`, asked.id)
			return model.Event{ProjectKey: project, Type: "user_state.updated", Actor: alice,
				Payload: map[string]any{"login": "alice", "changed": []string{"snoozed"}}}, err
		}},
		{"an agent read's user_agent_state.updated", func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
			_, err := tx.Exec(ctx, `insert into user_agent_read (login, session_id, read_through) values ('alice', 's-keyed', clock_timestamp())`)
			return model.Event{ProjectKey: project, Type: "user_agent_state.updated", Actor: alice,
				Payload: model.UserAgentStateEventPayload{Login: "alice", SessionID: "s-keyed"}}, err
		}},
		{"an answer's ask.answered", func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
			_, err := tx.Exec(ctx, `update user_threads set read_through = clock_timestamp() where login = 'alice' and thread_id = $1::uuid`, asked.id)
			return model.Event{IssueKey: issue, Type: "ask.answered", Actor: bob,
				Payload: model.AskEventPayload{Ask: model.Ask{ID: asked.id, IssueKey: issue, Author: alice, CreatedAt: asked.at}}}, err
		}},
		{"an edit's comment.edited", func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
			_, err := tx.Exec(ctx, `
				insert into comment_blocks (comment_id, block_id, node_id, position, type, text_sha256, excerpt, created_at,
				                            made_at, changed_revision, changed_at)
				values ($1::uuid, 'b9', $2, 9, 'paragraph', '', 'An added paragraph.', now(), now(), 2, now())`,
				thread.id, "comment/"+thread.id+"#b9")
			return model.Event{IssueKey: issue, Type: "comment.edited", Actor: alice, Payload: map[string]any{"id": thread.id}}, err
		}},
		{"an ask edit's ask.edited", func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
			_, err := tx.Exec(ctx, `
				insert into ask_blocks (ask_id, block_id, node_id, position, type, text_sha256, excerpt, created_at,
				                        made_at, changed_revision, changed_at)
				values ($1::uuid, 'b9', $2, 9, 'paragraph', '', 'An added paragraph.', now(), now(), 2, now())`,
				asked.id, "ask/"+asked.id+"#b9")
			return model.Event{IssueKey: issue, Type: "ask.edited", Actor: alice,
				Payload: model.AskEventPayload{Ask: model.Ask{ID: asked.id, IssueKey: issue, Author: alice, CreatedAt: asked.at}}}, err
		}},
		{"a message's message.created", func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
			err := tx.QueryRow(ctx, `
				insert into messages (issue_key, author, body, target) values ('CENS-1', $1, 'For the agent.', 'session:s-keyed')
				returning id::text`, actorJSON(t, alice)).Scan(&message)
			return model.Event{IssueKey: issue, Type: "message.created", Actor: alice, Payload: map[string]any{"id": message}}, err
		}},
		{"an attempt's message.delivery", func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
			_, err := tx.Exec(ctx, `insert into message_deliveries (message_id, attempt, delivery, session_id, state) values ($1::uuid, 1, 'aside', '', 'pending')`, message)
			return model.Event{IssueKey: issue, Type: "message.delivery", Actor: alice,
				Payload: model.MessageDeliveryEventPayload{MessageID: message, Attempt: 1, Delivery: "aside", Target: "session:s-keyed", State: "failed"}}, err
		}},
		{"an acceptance's message.accepted", func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
			_, err := tx.Exec(ctx, `update message_deliveries set claimed_at = clock_timestamp() where message_id = $1::uuid and attempt = 1`, message)
			return model.Event{IssueKey: issue, Type: "message.accepted", Actor: model.Actor{Kind: "session", ID: "s-keyed"},
				Payload: model.MessageAcceptedEventPayload{MessageID: message, Attempt: 1, SessionID: "s-keyed", Target: "session:s-keyed"}}, err
		}},
		{"a mention's comment.delivery", func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
			_, err := tx.Exec(ctx, `insert into comment_deliveries (comment_id, target, attempt, delivery, state) values ($1::uuid, 'session:s-keyed', 1, 'aside', 'pending')`, thread.id)
			return model.Event{IssueKey: issue, Type: "comment.delivery", Actor: alice,
				Payload: model.CommentDeliveryEventPayload{CommentID: thread.id, Target: "session:s-keyed", Attempt: 1, Delivery: "aside", State: "failed"}}, err
		}},
		{"a comment mention's comment.created, the comment written with its node", func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
			var id string
			var at time.Time
			if err := tx.QueryRow(ctx, `select gen_random_uuid()::text, clock_timestamp()`).Scan(&id, &at); err != nil {
				return model.Event{}, err
			}
			// The node before the row that names it, as that build's comment write takes them.
			if err := graphstore.Write(ctx, tx, []graphstore.Node{{
				Kind: graphstore.KindComment, ID: id, Supertag: graphstore.KindComment, Visibility: graphstore.VisibilityProject,
				State: graphstore.StateLive, Project: "CENS", MadeAt: at, MadeBy: alice, SourceEvent: graphstore.NoEvent,
			}}, nil); err != nil {
				return model.Event{}, err
			}
			_, err := tx.Exec(ctx, `
				with written as (
					insert into comments (id, issue_key, author, body, node_kind, node_address)
					values ($1::uuid, 'CENS-1', $2, 'For the role.', $3, $1) returning id
				)
				insert into comment_mentions (comment_id, target, delivery, resolved_session_id)
				select id, 'role:keyed-role', 'steer', 's-keyed' from written`, id, actorJSON(t, alice), graphstore.KindComment)
			return model.Event{IssueKey: issue, Type: "comment.created", Actor: alice, Payload: map[string]any{"id": id}}, err
		}},
		{"a session's comment.created, the comment written with its node", func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
			session := model.Actor{Kind: "session", ID: "s-commenting"}
			var id string
			var at time.Time
			if err := tx.QueryRow(ctx, `select gen_random_uuid()::text, clock_timestamp()`).Scan(&id, &at); err != nil {
				return model.Event{}, err
			}
			if err := graphstore.Write(ctx, tx, []graphstore.Node{{
				Kind: graphstore.KindComment, ID: id, Supertag: graphstore.KindComment, Visibility: graphstore.VisibilityProject,
				State: graphstore.StateLive, Project: "CENS", MadeAt: at, MadeBy: session, SourceEvent: graphstore.NoEvent,
			}}, nil); err != nil {
				return model.Event{}, err
			}
			_, err := tx.Exec(ctx, `
				insert into comments (id, issue_key, author, body, node_kind, node_address)
				values ($1::uuid, 'CENS-1', $2, 'A session''s remark.', $3, $1)`, id, actorJSON(t, session), graphstore.KindComment)
			return model.Event{IssueKey: issue, Type: "comment.created", Actor: session, Payload: map[string]any{"id": id}}, err
		}},
		{"an ask's ask.opened", func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
			var id string
			var at time.Time
			err := tx.QueryRow(ctx, `insert into asks (issue_key, author, question) values ('CENS-1', $1, 'Later?') returning id::text, created_at`,
				actorJSON(t, alice)).Scan(&id, &at)
			return model.Event{IssueKey: issue, Type: "ask.opened", Actor: alice,
				Payload: model.AskEventPayload{Ask: model.Ask{ID: id, IssueKey: issue, Author: alice, CreatedAt: at}}}, err
		}},
	} {
		after := newestEventID(t, writes.database)
		writeAsTheReleaseBefore(t, writes, event.write)
		through := newestEventID(t, writes.database)
		if _, err := WriteOwedObjectNodes(ctx, writes.database, 4); err != nil {
			t.Fatalf("%s: run the object pass: %v", event.name, err)
		}
		if owed := eventsOwe(t, writes.database, after, through); !owed {
			t.Errorf("%s leaves an inbox row owed and owes the catch-up nothing, want it owed", event.name)
		}
		catchUpTheTick(t, writes.database, event.name)
		if owed := eventsOwe(t, writes.database, after, through); owed {
			t.Errorf("%s still owes the catch-up once it has marked the rows, want nothing owed", event.name)
		}
	}
}

// eventsOwe is EventsOweObjectPass over (after, through], failing the test on its error.
func eventsOwe(t *testing.T, database *store.Store, after, through int64) bool {
	t.Helper()
	owed, err := EventsOweObjectPass(context.Background(), database.Pool, after, through)
	if err != nil {
		t.Fatalf("read whether the events in (%d, %d] owe the pass: %v", after, through, err)
	}
	return owed
}
