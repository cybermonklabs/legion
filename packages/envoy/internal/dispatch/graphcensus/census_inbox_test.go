package graphcensus

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// inboxCensus is a database the census of the one inbox is read over: the project CENS
// and its issue CENS-1, whose events each test appends through the broker as a write appends
// them, and whose nodes, tuples and projection rows each test states directly, as the writers
// would leave them, so each rule of the census is read over exactly the state it is about.
type inboxCensus struct {
	writes addressingWrites
	clock  time.Time
}

func newInboxCensus(t *testing.T) *inboxCensus {
	t.Helper()
	writes := newAddressingWrites(t)
	writes.createIssue(t, "CENS-1", 1, "U", alice)
	return &inboxCensus{writes: writes, clock: time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)}
}

// at is the fixture's clock moved on a minute: every write of a test is a minute after the last,
// so each moment the census compares is told apart from every other.
func (f *inboxCensus) at() time.Time {
	f.clock = f.clock.Add(time.Minute)
	return f.clock
}

// event appends an event of typ on CENS-1 by by, as the write it stands for appends it, and
// answers its id. An ask's event carries the ask flat (model.AskEventPayload), as the broker holds
// it to; every other carries payload.
func (f *inboxCensus) event(t *testing.T, typ string, by model.Actor, payload map[string]any) int64 {
	t.Helper()
	var body any = payload
	if strings.HasPrefix(typ, "ask.") {
		id, _ := payload["id"].(string)
		body = model.AskEventPayload{Ask: model.Ask{ID: id, IssueKey: new("CENS-1"), Author: by, CreatedAt: f.clock}}
	}
	return f.writes.write(t, func(context.Context, pgx.Tx) (model.Event, error) {
		return model.Event{IssueKey: new("CENS-1"), Type: typ, Actor: by, Payload: body}, nil
	}).ID
}

// state writes nodes and tuples through the store's own writers in one transaction: people's
// tuples through ToPerson, marks through FromPerson, and every other through Write.
func (f *inboxCensus) state(t *testing.T, nodes []graphstore.Node, tuples []graphstore.Tuple) {
	t.Helper()
	ctx := context.Background()
	tx, err := f.writes.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the store write: %v", err)
	}
	defer tx.Rollback(ctx)
	var plain, people, marks []graphstore.Tuple
	for _, tuple := range tuples {
		switch {
		case tuple.Kind == graphstore.TupleDeliveredTo && tuple.ToKind == graphstore.KindPerson,
			tuple.Kind == graphstore.TupleMentions && tuple.ToKind == graphstore.KindPerson,
			tuple.Kind == graphstore.TupleParticipatesIn && tuple.FromKind == graphstore.KindPerson:
			people = append(people, tuple)
		case tuple.FromKind == graphstore.KindPerson:
			marks = append(marks, tuple)
		default:
			plain = append(plain, tuple)
		}
	}
	if err := graphstore.Write(ctx, tx, nodes, plain); err != nil {
		t.Fatalf("write nodes %v and tuples %v: %v", nodes, plain, err)
	}
	if err := graphstore.ToPerson(ctx, tx, people); err != nil {
		t.Fatalf("write the people's tuples %v: %v", people, err)
	}
	if err := graphstore.FromPerson(ctx, tx, f.clock, marks); err != nil {
		t.Fatalf("write the marks %v: %v", marks, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the store write: %v", err)
	}
}

// exec runs one statement on the run's pool, whose connections carry this release's marker
// setting, as this release's writers' do.
func (f *inboxCensus) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.writes.database.Pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", strings.Join(strings.Fields(sql), " "), err)
	}
}

// newest is the log's newest position.
func (f *inboxCensus) newest(t *testing.T) int64 {
	t.Helper()
	var seq int64
	if err := f.writes.database.Pool.QueryRow(context.Background(), `select coalesce(max(seq), 0) from graph_tuples`).Scan(&seq); err != nil {
		t.Fatalf("read the log's newest position: %v", err)
	}
	return seq
}

// census takes the store census and logs what it printed.
func (f *inboxCensus) census(t *testing.T, when string) (*StoreReport, string) {
	t.Helper()
	report, err := StoreCensus(context.Background(), f.writes.database.Pool.Config().ConnString(), false, nil)
	if err != nil {
		t.Fatalf("take the store census %s: %v", when, err)
	}
	var printed strings.Builder
	report.Write(&printed)
	t.Logf("the census %s:\n%s", when, printed.String())
	return report, printed.String()
}

// node is one live node of kind, made by alice in the project CENS, or in no project where project
// is empty.
func (f *inboxCensus) node(kind, id, project string) graphstore.Node {
	return graphstore.Node{
		Kind: kind, ID: id, Supertag: kind, Visibility: graphstore.VisibilityProject, State: graphstore.StateLive,
		Project: project, MadeAt: f.clock, MadeBy: alice, SourceEvent: graphstore.NoEvent,
	}
}

// tuple is one asserted tuple at `at`, caused by event.
func tuple(fromKind, fromID, kind, toKind, toID string, at time.Time, event int64) graphstore.Tuple {
	return graphstore.Tuple{
		FromKind: fromKind, FromID: fromID, Kind: kind, ToKind: toKind, ToID: toID,
		At: at, Who: alice, SourceEvent: event, Asserted: true,
	}
}

func newUUID(t *testing.T, f *inboxCensus) string {
	t.Helper()
	var id string
	if err := f.writes.database.Pool.QueryRow(context.Background(), `select gen_random_uuid()::text`).Scan(&id); err != nil {
		t.Fatalf("draw an id: %v", err)
	}
	return id
}

// requireKindAgrees fails unless kind read rows, holds no difference either way, digests its rows
// and the store alike, and counts earlier rows of the release before and this of this release.
func requireKindAgrees(t *testing.T, report *StoreReport, name string, earlier, this int64) KindReport {
	t.Helper()
	kind := censusKind(t, report, name)
	if kind.Rows == 0 || kind.RowDifferences != 0 || kind.NodeDifferences != 0 {
		t.Errorf("kind %s read %d rows, %d rows no tuple states and %d tuples no row projects (%v %v), want rows and no difference",
			name, kind.Rows, kind.RowDifferences, kind.NodeDifferences, kind.ReportedRows, kind.ReportedNodes)
	}
	if kind.RowsDigest != kind.StoreDigest {
		t.Errorf("kind %s digests its rows %s and the store %s, which agree entry for entry and should digest alike",
			name, kind.RowsDigest, kind.StoreDigest)
	}
	if !kind.CountsPopulations || kind.EarlierRelease != earlier || kind.ThisRelease != this {
		t.Errorf("kind %s reads %d row(s) of the release before and %d of this one (populations counted: %t), want %d and %d",
			name, kind.EarlierRelease, kind.ThisRelease, kind.CountsPopulations, earlier, this)
	}
	return kind
}

// narrowed is the count kind's narrowing name reads.
func narrowed(t *testing.T, kind KindReport, name string) int64 {
	t.Helper()
	for _, figure := range kind.Narrowings {
		if figure.Name == name {
			return figure.Count
		}
	}
	t.Fatalf("kind %s reports no narrowing %s (it reports %v)", kind.Kind, name, kind.Narrowings)
	return 0
}

// threadRow writes login's user_threads row of thread as a marked row: its version, its marks and
// the log position it was marked at.
func (f *inboxCensus) threadRow(t *testing.T, login, kind, thread string, readThrough time.Time, version, markedVersion, markedSeq int64,
	repliedAt *time.Time, lastReply *string) {
	t.Helper()
	f.exec(t, `
		insert into user_threads (login, thread_id, kind, project_key, issue_key, read_through, replied_at, last_reply_id,
		                          version, marked_version, marked_seq)
		values ($1, $2::uuid, $3, 'CENS', 'CENS-1', $4, $5, $6::uuid, $7, $8, $9)
	`, login, thread, kind, readThrough, repliedAt, lastReply, version, markedVersion, markedSeq)
}

// R6's equation on a marked user_threads row, and the rules this census decides for it (P13 and the
// two of this change):
//
//   - a delivered_to whose event is a reply's write counts: a comment's create, and an answer;
//   - a delivered_to from an ask's create (ask.opened) counts nothing, nor one under an edit's
//     event (comment.edited), the delivery an edit's new mention of a person states;
//   - a delivered_to with no event (graphstore.NoEvent, the backfill's and the seed's) from a
//     comment counts, and one from an ask counts where the ask states an answered_at at the
//     tuple's moment and not otherwise;
//   - replied_at and last_reply_id are the newest of the counted deliveries, and a row the backfill
//     marked carries the reply it held across its mark, held to the delivery the backfill stated
//     at or below marked_seq.
//
// Over those rows the census reads user_threads and user_threads_version agree, the backfill's row
// as the release before's and the three the writer began as this release's. Then each row is moved
// one way a writer that broke a rule would move it - a version that also counts the edit's
// delivery, a version that counts the ask's create, a replied_at no delivery holds - and each reads
// as a difference naming the person and the thread.
func TestTheVersionCountsTheDeliveriesARepliesCaused(t *testing.T) {
	f := newInboxCensus(t)
	root, reply, sentinelReply, edited := newUUID(t, f), newUUID(t, f), newUUID(t, f), newUUID(t, f)
	ask, sentinelAsk, carried, carriedReply := newUUID(t, f), newUUID(t, f), newUUID(t, f), newUUID(t, f)
	f.state(t, []graphstore.Node{
		f.node(graphstore.KindComment, root, "CENS"), f.node(graphstore.KindComment, reply, "CENS"),
		f.node(graphstore.KindComment, sentinelReply, "CENS"), f.node(graphstore.KindComment, edited, "CENS"),
		f.node(graphstore.KindAsk, ask, "CENS"), f.node(graphstore.KindAsk, sentinelAsk, "CENS"),
		f.node(graphstore.KindComment, carried, "CENS"), f.node(graphstore.KindComment, carriedReply, "CENS"),
	}, nil)
	for _, under := range []struct{ reply, head string }{{reply, root}, {sentinelReply, root}, {edited, root}, {carriedReply, carried}} {
		f.state(t, nil, []graphstore.Tuple{tuple(graphstore.KindComment, under.reply, graphstore.TupleRepliesTo,
			graphstore.KindComment, under.head, f.at(), graphstore.NoEvent)})
	}

	// The row the backfill marked: alice's row of `carried`, raised to version 3 by an older build,
	// whose last reply the backfill states under its own event before it marks the row.
	carriedAt := f.at()
	carriedEvent := f.event(t, "comment.created", bob, map[string]any{"id": carriedReply})
	readCarried := f.at()
	f.state(t, nil, []graphstore.Tuple{
		tuple(graphstore.KindPerson, "alice", graphstore.TupleParticipatesIn, graphstore.KindComment, carried, readCarried, graphstore.NoEvent),
		tuple(graphstore.KindComment, carriedReply, graphstore.TupleDeliveredTo, graphstore.KindPerson, "alice", carriedAt, carriedEvent),
		tuple(graphstore.KindPerson, "alice", graphstore.TupleReadThrough, graphstore.KindComment, carried, readCarried, graphstore.NoEvent),
	})
	f.threadRow(t, "alice", graphstore.KindComment, carried, readCarried, 3, 3, f.newest(t), &carriedAt, &carriedReply)

	// The rows the writer began: alice's of root, of ask and of sentinelAsk, each at version 0 and
	// marked at the log's newest position, with her read mark stated after the insert.
	type begun struct{ kind, thread string }
	rows := []begun{{graphstore.KindComment, root}, {graphstore.KindAsk, ask}, {graphstore.KindAsk, sentinelAsk}}
	marks := map[string]time.Time{}
	for _, row := range rows {
		read := f.at()
		marks[row.thread] = read
		f.state(t, nil, []graphstore.Tuple{
			tuple(graphstore.KindPerson, "alice", graphstore.TupleParticipatesIn, row.kind, row.thread, read, graphstore.NoEvent),
		})
		f.threadRow(t, "alice", row.kind, row.thread, read, 0, 0, f.newest(t), nil, nil)
		f.state(t, nil, []graphstore.Tuple{
			tuple(graphstore.KindPerson, "alice", graphstore.TupleReadThrough, row.kind, row.thread, read, graphstore.NoEvent),
		})
	}

	// root: bob's reply (counts), a reply stated at the sentinel (counts), and an edit's delivery,
	// newest of all (counts nothing).
	replyAt := f.at()
	replyEvent := f.event(t, "comment.created", bob, map[string]any{"id": reply})
	f.state(t, nil, []graphstore.Tuple{tuple(graphstore.KindComment, reply, graphstore.TupleDeliveredTo, graphstore.KindPerson, "alice", replyAt, replyEvent)})
	sentinelAt := f.at()
	f.state(t, nil, []graphstore.Tuple{tuple(graphstore.KindComment, sentinelReply, graphstore.TupleDeliveredTo, graphstore.KindPerson, "alice", sentinelAt, graphstore.NoEvent)})
	editEvent := f.event(t, "comment.edited", bob, map[string]any{"id": edited})
	f.state(t, nil, []graphstore.Tuple{tuple(graphstore.KindComment, edited, graphstore.TupleDeliveredTo, graphstore.KindPerson, "alice", f.at(), editEvent)})
	f.exec(t, `update user_threads set version = 2, replied_at = $2, last_reply_id = $3::uuid where login = 'alice' and thread_id = $1::uuid`,
		root, sentinelAt, sentinelReply)

	// ask: its create reaches alice (counts nothing), then carol's answer (counts).
	openEvent := f.event(t, "ask.opened", bob, map[string]any{"id": ask})
	f.state(t, nil, []graphstore.Tuple{tuple(graphstore.KindAsk, ask, graphstore.TupleDeliveredTo, graphstore.KindPerson, "alice", f.at(), openEvent)})
	answeredAt := f.at()
	answerEvent := f.event(t, "ask.answered", carol, map[string]any{"id": ask})
	f.state(t, nil, []graphstore.Tuple{
		tuple(graphstore.KindAsk, ask, graphstore.TupleAnsweredAt, graphstore.KindValue, answeredAt.Format(time.RFC3339Nano), answeredAt, answerEvent),
		tuple(graphstore.KindAsk, ask, graphstore.TupleDeliveredTo, graphstore.KindPerson, "alice", answeredAt, answerEvent),
	})
	f.exec(t, `update user_threads set version = 1, replied_at = $2, last_reply_id = $1::uuid where login = 'alice' and thread_id = $1::uuid`,
		ask, answeredAt)

	// sentinelAsk: an answer and a create stated at the sentinel, the answer's at the moment the ask
	// states its answered_at, the create's at none.
	sentinelAnswer := f.at()
	f.state(t, nil, []graphstore.Tuple{
		tuple(graphstore.KindAsk, sentinelAsk, graphstore.TupleAnsweredAt, graphstore.KindValue, sentinelAnswer.Format(time.RFC3339Nano), sentinelAnswer, graphstore.NoEvent),
		tuple(graphstore.KindAsk, sentinelAsk, graphstore.TupleDeliveredTo, graphstore.KindPerson, "alice", sentinelAnswer, graphstore.NoEvent),
	})
	f.state(t, nil, []graphstore.Tuple{tuple(graphstore.KindAsk, sentinelAsk, graphstore.TupleDeliveredTo, graphstore.KindPerson, "alice", f.at(), graphstore.NoEvent)})
	f.exec(t, `update user_threads set version = 1, replied_at = $2, last_reply_id = $1::uuid where login = 'alice' and thread_id = $1::uuid`,
		sentinelAsk, sentinelAnswer)

	report, _ := f.census(t, "over the four rows")
	requireKindAgrees(t, report, "user_threads", 1, 3)
	requireKindAgrees(t, report, "user_threads_version", 1, 3)

	for _, broken := range []struct{ what, kind, thread, update string }{
		{"a version that counts the edit's delivery", graphstore.KindComment, root,
			`update user_threads set version = 3 where login = 'alice' and thread_id = $1::uuid`},
		{"a version that counts the ask's create", graphstore.KindAsk, ask,
			`update user_threads set version = 2 where login = 'alice' and thread_id = $1::uuid`},
		{"a version that counts the sentinel create", graphstore.KindAsk, sentinelAsk,
			`update user_threads set version = 2 where login = 'alice' and thread_id = $1::uuid`},
		{"a replied_at no delivery holds", graphstore.KindComment, carried,
			`update user_threads set replied_at = replied_at + interval '1 second' where login = 'alice' and thread_id = $1::uuid`},
	} {
		var before struct {
			version   int64
			repliedAt time.Time
		}
		if err := f.writes.database.Pool.QueryRow(context.Background(),
			`select version, replied_at from user_threads where login = 'alice' and thread_id = $1::uuid`, broken.thread).
			Scan(&before.version, &before.repliedAt); err != nil {
			t.Fatalf("read alice's row of %s: %v", broken.thread, err)
		}
		f.exec(t, broken.update, broken.thread)
		report, printed := f.census(t, "with "+broken.what)
		kind := censusKind(t, report, "user_threads_version")
		row := "user_threads_version difference at user_threads alice " + broken.thread + " version -> "
		stated := "user_threads_version difference at person:alice version -> " + broken.kind + ":" + broken.thread + " "
		if kind.RowDifferences != 1 || kind.NodeDifferences != 1 || !strings.Contains(printed, row) || !strings.Contains(printed, stated) {
			t.Errorf("with %s the version kind reads %d row and %d tuple differences, want one of each naming alice and thread %s (%q and %q)",
				broken.what, kind.RowDifferences, kind.NodeDifferences, broken.thread, row, stated)
		}
		f.exec(t, `update user_threads set version = $2, replied_at = $3 where login = 'alice' and thread_id = $1::uuid`,
			broken.thread, before.version, before.repliedAt)
	}
}

// The relations of a person into a thread that no user_threads row projects by design are each a
// figure of their own and no difference: participation in a thread whose root names no project
// (a direct message), participation in an ask that reached the person as who it waits on, and
// the participation alone that a person's own move of an ask onto themselves states, under an
// event of that ask they made (inboxwrite.OwnMove); participation stated under the event of a
// mention of the person, which keeps them no row (inboxwrite.MentionParticipation); relations into
// a thread whose row a removal of its document deleted; and a session's participation, which no
// row of people holds. A participation that none of those explains - among them one in an ask with
// no delivery beside it under a move somebody else made - and a mark no row holds with no removal
// after it, is a tuple no row projects.
func TestTheThreadCensusCountsEachNarrowingAsAFigure(t *testing.T) {
	f := newInboxCensus(t)
	direct, addressed, removed, stray := newUUID(t, f), newUUID(t, f), newUUID(t, f), newUUID(t, f)
	selfMoved, movedByAnother, mentioning := newUUID(t, f), newUUID(t, f), newUUID(t, f)
	f.state(t, []graphstore.Node{
		f.node(graphstore.KindMessage, direct, ""), f.node(graphstore.KindAsk, addressed, "CENS"),
		f.node(graphstore.KindComment, removed, "CENS"), f.node(graphstore.KindComment, stray, "CENS"),
		f.node(graphstore.KindAsk, selfMoved, "CENS"), f.node(graphstore.KindAsk, movedByAnother, "CENS"),
		f.node(graphstore.KindComment, mentioning, "CENS"),
		graphstore.SessionNode("census-session", alice, f.clock, graphstore.NoEvent),
	}, []graphstore.Tuple{
		tuple(graphstore.KindComment, removed, graphstore.TupleOwnedBy, "artifact", "CENS/notes.md", f.at(), graphstore.NoEvent),
		tuple(graphstore.KindSession, "census-session", graphstore.TupleParticipatesIn, graphstore.KindComment, removed, f.at(), graphstore.NoEvent),
	})
	// bob asked both asks; frank then hands the first to himself, and alice hands the second to
	// grace, each move stating the participation alone.
	for _, ask := range []string{selfMoved, movedByAnother} {
		f.exec(t, `insert into asks (id, issue_key, author, question, state) values ($1::uuid, 'CENS-1', $2, 'Ship?', 'open')`,
			ask, actorJSON(t, bob))
	}
	frank := model.Actor{Kind: "user", ID: "frank"}
	handedToHimself := f.event(t, "ask.asked_of", frank, map[string]any{"id": selfMoved})
	ownMove := tuple(graphstore.KindPerson, "frank", graphstore.TupleParticipatesIn, graphstore.KindAsk, selfMoved, f.at(), handedToHimself)
	ownMove.Who = frank
	opened := f.event(t, "ask.opened", alice, map[string]any{"id": addressed})
	// alice's comment mentions henry, who writes nothing in its thread: the comment's write states
	// the mention and henry's participation under its one event.
	mentioned := f.event(t, "comment.created", alice, map[string]any{"id": mentioning})
	f.state(t, nil, []graphstore.Tuple{
		tuple(graphstore.KindPerson, "bob", graphstore.TupleParticipatesIn, graphstore.KindMessage, direct, f.at(), graphstore.NoEvent),
		tuple(graphstore.KindAsk, addressed, graphstore.TupleDeliveredTo, graphstore.KindPerson, "carol", f.at(), opened),
		tuple(graphstore.KindPerson, "carol", graphstore.TupleParticipatesIn, graphstore.KindAsk, addressed, f.clock, opened),
		ownMove,
		tuple(graphstore.KindPerson, "dave", graphstore.TupleParticipatesIn, graphstore.KindComment, removed, f.at(), graphstore.NoEvent),
		tuple(graphstore.KindPerson, "dave", graphstore.TupleReadThrough, graphstore.KindComment, removed, f.at(), graphstore.NoEvent),
		tuple(graphstore.KindComment, mentioning, graphstore.TupleMentions, graphstore.KindPerson, "henry", f.at(), mentioned),
		tuple(graphstore.KindPerson, "henry", graphstore.TupleParticipatesIn, graphstore.KindComment, mentioning, f.at(), mentioned),
	})
	f.event(t, "artifact.removed", alice, map[string]any{"artifact": map[string]any{"ref_key": "CENS/notes.md"}})

	report, _ := f.census(t, "over the narrowed relations")
	kind := censusKind(t, report, "user_threads")
	if kind.Rows != 0 || kind.Nodes != 0 || kind.RowDifferences != 0 || kind.NodeDifferences != 0 {
		t.Errorf("user_threads reads %d rows, %d tuples and %d and %d differences (%v %v), want nothing but the narrowings",
			kind.Rows, kind.Nodes, kind.RowDifferences, kind.NodeDifferences, kind.ReportedRows, kind.ReportedNodes)
	}
	for name, want := range map[string]int64{
		"no_project": 1, "addressed_alone": 2, "mentioned_alone": 1, "row_removed": 2, "session_authors": 1,
	} {
		if got := narrowed(t, kind, name); got != want {
			t.Errorf("user_threads reads %d of the narrowing %s, want %d", got, name, want)
		}
	}

	handedToGrace := f.event(t, "ask.asked_of", alice, map[string]any{"id": movedByAnother})
	f.state(t, nil, []graphstore.Tuple{
		tuple(graphstore.KindPerson, "erin", graphstore.TupleParticipatesIn, graphstore.KindComment, stray, f.at(), graphstore.NoEvent),
		tuple(graphstore.KindPerson, "erin", graphstore.TupleReadThrough, graphstore.KindComment, stray, f.at(), graphstore.NoEvent),
		tuple(graphstore.KindPerson, "grace", graphstore.TupleParticipatesIn, graphstore.KindAsk, movedByAnother, f.at(), handedToGrace),
	})
	report, printed := f.census(t, "with participations and a mark nothing explains")
	kind = censusKind(t, report, "user_threads")
	if kind.NodeDifferences != 3 || !strings.Contains(printed, "user_threads difference at person:erin participates_in -> comment:"+stray) ||
		!strings.Contains(printed, "user_threads difference at person:erin read_through -> comment:"+stray) ||
		!strings.Contains(printed, "user_threads difference at person:grace participates_in -> ask:"+movedByAnother) {
		t.Errorf("user_threads reads %d tuple differences, want erin's participation and mark, and grace's participation in the ask alice handed her with no delivery, each named as a tuple no row projects",
			kind.NodeDifferences)
	}
	if got := narrowed(t, kind, "addressed_alone"); got != 2 {
		t.Errorf("user_threads reads %d of the narrowing addressed_alone with grace's participation, want the 2 before it", got)
	}
}

// The five read-state tables, through user_ask_snooze: a row the backfill marked, whose mark stands at
// the sentinel, is the release before's, and a row a route marked under its event this release's;
// a row with no marker is the unmarked class; a mark whose row a build of the release before
// deleted - the log holds that build's deleting event, under which no tuple of this release stands
// - is a figure of its own; and a mark with no row and no such event, and a marked row whose mark
// the store does not hold, are differences in each direction.
func TestTheReadStateCensusTellsItsPopulationsAndAnOlderDelete(t *testing.T) {
	f := newInboxCensus(t)
	askA, askB, askC, askD, askE := newUUID(t, f), newUUID(t, f), newUUID(t, f), newUUID(t, f), newUUID(t, f)
	f.state(t, []graphstore.Node{
		f.node(graphstore.KindAsk, askA, "CENS"), f.node(graphstore.KindAsk, askB, "CENS"), f.node(graphstore.KindAsk, askC, "CENS"),
		f.node(graphstore.KindAsk, askD, "CENS"), f.node(graphstore.KindAsk, askE, "CENS"),
	}, nil)
	for _, ask := range []string{askA, askB, askC, askD, askE} {
		f.exec(t, `insert into asks (id, issue_key, author, question, state) values ($1::uuid, 'CENS-1', $2, 'Ship?', 'open')`,
			ask, actorJSON(t, bob))
	}
	until := f.at().Add(24 * time.Hour)
	// askA: the backfill's mark; askB: a route's, under its event; askC: a row no marker marks.
	f.state(t, nil, []graphstore.Tuple{tuple(graphstore.KindPerson, "alice", graphstore.TupleSnoozed, graphstore.KindAsk, askA, until, graphstore.NoEvent)})
	snoozed := f.event(t, "user_state.updated", alice, map[string]any{"login": "alice", "changed": []string{"snoozed"}})
	f.state(t, nil, []graphstore.Tuple{tuple(graphstore.KindPerson, "alice", graphstore.TupleSnoozed, graphstore.KindAsk, askB, until, snoozed)})
	f.exec(t, `
		insert into user_ask_snooze (login, ask_id, snoozed_until, tuples_stored_at)
		values ('alice', $1::uuid, $3, $3), ('alice', $2::uuid, $3, $3), ('alice', $4::uuid, $3, null)
	`, askA, askB, until, askC)
	// askD: bob snoozed it through this release, then a build of the release before unsnoozed it,
	// deleting the row and stating nothing.
	bobSnoozed := f.event(t, "user_state.updated", bob, map[string]any{"login": "bob", "changed": []string{"snoozed"}})
	f.state(t, nil, []graphstore.Tuple{tuple(graphstore.KindPerson, "bob", graphstore.TupleSnoozed, graphstore.KindAsk, askD, until, bobSnoozed)})
	f.event(t, "user_state.updated", bob, map[string]any{"login": "bob", "changed": []string{"snoozed"}})

	report, _ := f.census(t, "over the snoozes")
	kind := requireKindAgrees(t, report, "user_ask_snooze", 1, 1)
	if kind.Unmarked != 1 {
		t.Errorf("user_ask_snooze reads %d unmarked row(s), want askC's", kind.Unmarked)
	}
	if got := narrowed(t, kind, "left_by_an_older_delete"); got != 1 {
		t.Errorf("user_ask_snooze reads %d mark(s) an older build's delete left standing, want bob's of askD", got)
	}

	// carol's snooze of askE, whose row is gone with no event to say who deleted it, and alice's
	// row of askB with its mark taken out of the store: one difference each way.
	f.state(t, nil, []graphstore.Tuple{tuple(graphstore.KindPerson, "carol", graphstore.TupleSnoozed, graphstore.KindAsk, askE, until, graphstore.NoEvent)})
	f.exec(t, `delete from graph_tuples where from_kind = 'person' and from_id = 'alice' and kind = 'snoozed' and to_id = $1`, askB)
	report, printed := f.census(t, "with a stray mark and a lost one")
	kind = censusKind(t, report, "user_ask_snooze")
	if kind.RowDifferences != 1 || kind.NodeDifferences != 1 ||
		!strings.Contains(printed, "user_ask_snooze difference at user_ask_snooze alice "+askB) ||
		!strings.Contains(printed, "user_ask_snooze difference at person:carol snoozed -> ask:"+askE) {
		t.Errorf("user_ask_snooze reads %d and %d differences, want alice's row of askB with no tuple and carol's mark of askE with no row",
			kind.RowDifferences, kind.NodeDifferences)
	}
	if got := narrowed(t, kind, "left_by_an_older_delete"); got != 1 {
		t.Errorf("user_ask_snooze reads %d mark(s) an older build's delete left standing, want bob's alone", got)
	}
}

// An Agent thread's dismissal is a user_agent_thread_dismissed row, and the route of the release
// before that deletes one - "Move to Inbox" on an Agent row - appends user_agent_state.updated
// (dismissInboxAgentThreads at 99e9da45abf1), the type every write of a person's Agent state
// appends and this release's dismissal and restore of one state their marks under. alice's
// dismissal is a row this release marked under that event; erin dismissed the same root through
// this release and a build of the release before put it back, deleting her row and stating
// nothing. Her mark is the figure left_by_an_older_delete, and no difference.
func TestTheAgentDismissalCensusTellsAnOlderRestore(t *testing.T) {
	f := newInboxCensus(t)
	erin := model.Actor{Kind: "user", ID: "erin"}
	var root string
	if err := f.writes.database.Pool.QueryRow(context.Background(), `
		insert into messages (author, body, target) values ($1, 'Can you look?', 'session:s1') returning id::text
	`, actorJSON(t, alice)).Scan(&root); err != nil {
		t.Fatalf("write the Agent thread's root: %v", err)
	}
	f.state(t, []graphstore.Node{f.node(graphstore.KindMessage, root, "")}, nil)
	dismissedAt := f.at()
	dismissed := f.event(t, "user_agent_state.updated", alice, map[string]any{"login": "alice", "session_id": "s1"})
	f.state(t, nil, []graphstore.Tuple{tuple(graphstore.KindPerson, "alice", graphstore.TupleDismissed, graphstore.KindMessage, root, dismissedAt, dismissed)})
	f.exec(t, `
		insert into user_agent_thread_dismissed (login, message_id, dismissed_at, tuples_stored_at) values ('alice', $1::uuid, $2, $2)
	`, root, dismissedAt)
	erinDismissed := f.event(t, "user_agent_state.updated", erin, map[string]any{"login": "erin", "session_id": "s1"})
	f.state(t, nil, []graphstore.Tuple{tuple(graphstore.KindPerson, "erin", graphstore.TupleDismissed, graphstore.KindMessage, root, f.at(), erinDismissed)})
	f.event(t, "user_agent_state.updated", erin, map[string]any{"login": "erin", "session_id": "s1"})

	report, _ := f.census(t, "over the Agent dismissals")
	kind := requireKindAgrees(t, report, "user_agent_thread_dismissed", 0, 1)
	if got := narrowed(t, kind, "left_by_an_older_delete"); got != 1 {
		t.Errorf("user_agent_thread_dismissed reads %d mark(s) an older build's restore left standing, want erin's", got)
	}
}

// A table whose rows record their creation splits its populations at the moment this release's
// first migration was applied: a messages row written before it is the release before's. The
// message kind holds each marked row to its node's relations - part_of its issue, authored_by its
// author - and a row no marker marks is the unmarked class.
func TestTheMessageCensusSplitsItsPopulationsAtThisReleasesSchema(t *testing.T) {
	f := newInboxCensus(t)
	var applied time.Time
	if err := f.writes.database.Pool.QueryRow(context.Background(),
		`select applied_at from schema_migrations where version = `+inboxReleaseVersion).Scan(&applied); err != nil {
		t.Fatalf("read when this release's first migration was applied: %v", err)
	}
	for _, at := range []time.Time{applied.Add(-24 * time.Hour), applied.Add(time.Hour)} {
		id := newUUID(t, f)
		row := graphstore.MessageRow{ID: id, IssueKey: "CENS-1"}
		node := f.node(graphstore.KindMessage, id, "CENS")
		nodes, tuples, projected := graphstore.MessageFacts([]graphstore.Node{node}, nil, row, alice, at, graphstore.NoEvent)
		authors, authored, named := graphstore.AuthorFacts(graphstore.KindMessage, id, alice, at, graphstore.NoEvent)
		if !projected || !named {
			t.Fatalf("message %s states nothing: projected %t, named %t", id, projected, named)
		}
		f.state(t, append(nodes, authors...), append(tuples, authored...))
		f.exec(t, `
			insert into messages (id, issue_key, author, body, created_at, node_kind, node_address)
			values ($1::uuid, 'CENS-1', $2, 'Ship it?', $3, 'message', $1)
		`, id, actorJSON(t, alice), at)
	}
	f.exec(t, `insert into messages (issue_key, author, body) values ('CENS-1', $1, 'An older build''s.')`, actorJSON(t, bob))

	report, _ := f.census(t, "over the messages")
	kind := requireKindAgrees(t, report, graphstore.KindMessage, 1, 1)
	if kind.Rows != 4 || kind.Unmarked != 1 {
		t.Errorf("the message kind reads %d triples and %d unmarked row(s), want each message's part_of and authored_by and the older build's row", kind.Rows, kind.Unmarked)
	}
	report.RequireBothReleases = true
	if empty := report.EmptyReleases(); !slices.Contains(empty, "user_threads (the release before)") || slices.Contains(empty, "message (the release before)") {
		t.Errorf("the census with both releases required names %v empty, want user_threads' release before and not the message kind's", empty)
	}
	if !report.Refused() {
		t.Error("the census with both releases required and kinds of one population passes, want a refusal")
	}
}

// The rows a build of the release before wrote, once the backfill has given them their tuples,
// read as the release before's population and agree with the store: an issue message and its
// reply, and a comment and a reply that answers it, each written before this release's schema
// reached the database, as that build writes them (rows naming no node and holding no
// tuples_stored, which its triggers project into user_threads), then the backfill. Before it, every
// row of theirs is the unmarked class; after it none is, the messages, the comment pointers, the
// user_threads rows and their versions agree, and each such row is counted among the release
// before's.
func TestTheCensusReadsAnOlderBuildsRowsOnceTheBackfillMarksThem(t *testing.T) {
	f := newInboxCensus(t)
	writes := f.writes
	// Each row is written a minute apart in 2020, before any applied_at a migrated database holds.
	before := func(minutes string) string {
		return `'2020-01-01T00:00:00Z'::timestamptz + make_interval(mins => ` + minutes + `::int)`
	}
	root := writeAsAnOlderBuild(t, writes, "message.created", alice, `
		insert into messages (issue_key, author, body, created_at) values ('CENS-1', $1, 'Ship it on Friday?', `+before("$2")+`)
		returning id::text, created_at`, actorJSON(t, alice), 1)
	writeAsAnOlderBuild(t, writes, "message.answered", bob, `
		insert into messages (issue_key, author, body, in_reply_to, created_at) values ('CENS-1', $1, 'Friday works.', $2::uuid, `+before("$3")+`)
		returning id::text, created_at`, actorJSON(t, bob), root.id, 2)
	thread := writeAsAnOlderBuild(t, writes, "comment.created", alice, `
		insert into comments (issue_key, author, body, created_at) values ('CENS-1', $1, 'Is the copy final?', `+before("$2")+`)
		returning id::text, created_at`, actorJSON(t, alice), 3)
	writeAsAnOlderBuild(t, writes, "comment.created", bob, `
		insert into comments (issue_key, author, body, reply_to, answers, created_at) values ('CENS-1', $1, 'It is.', $2::uuid, $2::uuid, `+before("$3")+`)
		returning id::text, created_at`, actorJSON(t, bob), thread.id, 4)

	report, _ := f.census(t, "before the backfill")
	for name, unmarked := range map[string]int64{graphstore.KindMessage: 2, "comment_pointers": 2, "user_threads": 4} {
		if kind := censusKind(t, report, name); kind.Unmarked != unmarked || kind.Rows != 0 {
			t.Errorf("before the backfill %s reads %d unmarked row(s) and %d checked, want %d and none", name, kind.Unmarked, kind.Rows, unmarked)
		}
	}

	backfillInboxOnce(t, writes.database)
	report, _ = f.census(t, "after the backfill")
	requireKindAgrees(t, report, graphstore.KindMessage, 2, 0)
	requireKindAgrees(t, report, "comment_pointers", 2, 0)
	requireKindAgrees(t, report, "user_threads", 4, 0)
	requireKindAgrees(t, report, "user_threads_version", 4, 0)
	for _, name := range []string{graphstore.KindMessage, "comment_pointers", "user_threads"} {
		if kind := censusKind(t, report, name); kind.Unmarked != 0 {
			t.Errorf("after the backfill %s reads %d unmarked row(s), want none", name, kind.Unmarked)
		}
	}
}

// R9 holds the census to no difference on what this release's writers leave. An attempt the
// listener took is a delivery, and its delivered_to stays stated when the row leaves the taken
// states (graphstore's attemptMoved): a session can settle an attempt failed after it was sent or
// queued, since its reply that reports an error sets the row's state with no guard on the state it
// was in (api's replyComment, and the message reply's error arm). The census holds the node to
// that rule: it expects the delivered_to of a row its node was ever stated taken for
// (attemptTaken), and of no other.
//
// A comment's mention attempt goes from pending to sent, reaching ses-sent, and then to failed, and
// a targeted message's attempt from pending to queued for ses-queued and then to failed, each step
// stated through the writer as its route states it (graphstore.QueueAttempted). After each step the
// census of both attempt tables reads no difference, and once both have failed each node's
// delivered_to still stands, its one assertion. Then a second mention of the comment, to
// ses-never, is refused - opened and settled failed, never taken - and the census still reads no
// difference; and once its node asserts a delivered_to to ses-never, which no writer of an attempt
// states, the census of comment_deliveries reads a difference at that attempt, and that of
// message_deliveries none. Its control is the census expecting a delivered_to only while the row is
// taken (attemptFacts before INFRA-1152's review C15): each attempt that failed after it was taken
// reads a difference.
func TestTheAttemptCensusReadsNoDifferenceOnceATakenAttemptFails(t *testing.T) {
	f := newInboxCensus(t)
	ctx := context.Background()
	pool := f.writes.database.Pool
	var comment, message string
	if err := pool.QueryRow(ctx, `
		insert into comments (issue_key, author, body) values ('CENS-1', $1, 'For the agent.') returning id::text
	`, actorJSON(t, alice)).Scan(&comment); err != nil {
		t.Fatalf("write the comment that mentions ses-sent: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		insert into messages (issue_key, author, body, target) values ('CENS-1', $1, 'For the agent.', 'session:ses-queued') returning id::text
	`, actorJSON(t, alice)).Scan(&message); err != nil {
		t.Fatalf("write the message for ses-queued: %v", err)
	}
	attempts := []struct {
		what, session          string
		key                    graphstore.AttemptKey
		opened, taken, refused string
	}{{
		what: "the comment's mention attempt", session: "ses-sent", key: graphstore.CommentAttempt(comment, "session:ses-sent", 1),
		opened: `insert into comment_deliveries (comment_id, target, attempt, delivery, state, node_kind, node_address)
			values ($1::uuid, 'session:ses-sent', 1, 'aside', 'pending', $2, $3)`,
		taken: `update comment_deliveries set state = 'sent', session_id = 'ses-sent', envelope_id = 'envelope-sent'
			where comment_id = $1::uuid and target = 'session:ses-sent'`,
		refused: `update comment_deliveries set state = 'failed', error = 'the session could not act on it'
			where comment_id = $1::uuid and target = 'session:ses-sent'`,
	}, {
		what: "the message's attempt", session: "ses-queued", key: graphstore.MessageAttempt(message, 1),
		opened: `insert into message_deliveries (message_id, attempt, delivery, session_id, state, node_kind, node_address)
			values ($1::uuid, 1, 'aside', '', 'pending', $2, $3)`,
		taken:   `update message_deliveries set state = 'queued', session_id = 'ses-queued', envelope_id = 'envelope-queued' where message_id = $1::uuid`,
		refused: `update message_deliveries set state = 'failed', error = 'the session could not act on it' where message_id = $1::uuid`,
	}}
	itemOf := func(key graphstore.AttemptKey) string {
		if key.Message != "" {
			return key.Message
		}
		return key.Comment
	}
	// step writes one move of an attempt's row and states what the row then holds, in one
	// transaction, as the route that moves it does; the first move writes the attempt's node
	// before its row, which names it.
	step := func(what string, key graphstore.AttemptKey, opens bool, statement string) {
		t.Helper()
		if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			args := []any{itemOf(key)}
			if opens {
				if err := graphstore.Write(ctx, tx, []graphstore.Node{graphstore.AttemptNode(key, alice, f.at(), graphstore.NoEvent)}, nil); err != nil {
					return err
				}
				args = append(args, graphstore.KindDeliveryAttempt, key.ID())
			}
			if _, err := tx.Exec(ctx, statement, args...); err != nil {
				return err
			}
			return store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
				graphstore.QueueAttempted(b, graphstore.AttemptWriter, graphstore.NoEvent, key)
				return nil
			})
		}); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	// requireAttemptsAgree fails unless the census of each attempt table reads rows of its own and
	// no difference.
	requireAttemptsAgree := func(when string, rows map[string]int64) {
		t.Helper()
		report, _ := f.census(t, when)
		for name, want := range rows {
			if kind := censusKind(t, report, name); kind.Rows != want || kind.RowDifferences != 0 || kind.NodeDifferences != 0 {
				t.Errorf("%s the census of %s reads %d row(s), %d rows no tuple states and %d nodes no row projects (%v %v), want %d and no difference",
					when, name, kind.Rows, kind.RowDifferences, kind.NodeDifferences, kind.ReportedRows, kind.ReportedNodes, want)
			}
		}
	}
	one := map[string]int64{"comment_deliveries": 1, "message_deliveries": 1}

	for _, attempt := range attempts {
		step("open "+attempt.what, attempt.key, true, attempt.opened)
	}
	requireAttemptsAgree("with both attempts pending,", one)
	for _, attempt := range attempts {
		step("take "+attempt.what, attempt.key, false, attempt.taken)
		if asserted, _ := newestOf(t, f.writes.database, graphstore.KindDeliveryAttempt, attempt.key.ID(), graphstore.TupleDeliveredTo,
			graphstore.KindSession, attempt.session); !asserted {
			t.Fatalf("%s, taken by %s, states no delivered_to to that session: this test proves nothing", attempt.what, attempt.session)
		}
	}
	requireAttemptsAgree("with the comment's attempt sent and the message's queued,", one)
	for _, attempt := range attempts {
		step("fail "+attempt.what, attempt.key, false, attempt.refused)
		var stated, asserted int
		if err := pool.QueryRow(ctx, `
			select count(*), count(*) filter (where asserted)
			from graph_tuples
			where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4 and to_id = $5
		`, graphstore.KindDeliveryAttempt, attempt.key.ID(), graphstore.TupleDeliveredTo, graphstore.KindSession, attempt.session).Scan(
			&stated, &asserted); err != nil {
			t.Fatalf("read the delivered_to tuples of %s: %v", attempt.what, err)
		}
		if stated != 1 || asserted != 1 {
			t.Fatalf("once failed, %s holds %d delivered_to tuple(s) to %s, %d asserting, want its one assertion: this test proves nothing",
				attempt.what, stated, attempt.session, asserted)
		}
	}
	requireAttemptsAgree("once both attempts failed after they were taken,", one)

	refused := graphstore.CommentAttempt(comment, "session:ses-never", 1)
	step("open the comment's mention of ses-never", refused, true, `
		insert into comment_deliveries (comment_id, target, attempt, delivery, state, node_kind, node_address)
		values ($1::uuid, 'session:ses-never', 1, 'aside', 'pending', $2, $3)`)
	step("refuse the comment's mention of ses-never", refused, false, `
		update comment_deliveries set state = 'failed', session_id = 'ses-never', error = 'no live session ses-never'
		where comment_id = $1::uuid and target = 'session:ses-never'`)
	both := map[string]int64{"comment_deliveries": 2, "message_deliveries": 1}
	requireAttemptsAgree("with the mention of ses-never refused,", both)
	f.state(t, nil, []graphstore.Tuple{{
		FromKind: graphstore.KindDeliveryAttempt, FromID: refused.ID(), Kind: graphstore.TupleDeliveredTo,
		ToKind: graphstore.KindSession, ToID: "ses-never", At: f.at(), Who: graphstore.AttemptWriter,
		SourceEvent: graphstore.NoEvent, Asserted: true,
	}})
	report, _ := f.census(t, "once the refused mention's node asserts a delivered_to")
	// A digest kind reads a row and its node that disagree as one row difference at the row's label.
	kind := censusKind(t, report, "comment_deliveries")
	named := slices.ContainsFunc(kind.ReportedRows, func(difference Difference) bool {
		return strings.Contains(difference.At, "session:ses-never")
	})
	if kind.RowDifferences != 1 || !named {
		t.Errorf("once the never-taken mention of ses-never asserts a delivered_to the census of comment_deliveries reads %d row difference(s) (%v %v), want one, at that attempt",
			kind.RowDifferences, kind.ReportedRows, kind.ReportedNodes)
	}
	if kind := censusKind(t, report, "message_deliveries"); kind.RowDifferences+kind.NodeDifferences != 0 {
		t.Errorf("the census of message_deliveries reads %d difference(s) (%v %v), want none", kind.RowDifferences+kind.NodeDifferences,
			kind.ReportedRows, kind.ReportedNodes)
	}
}

// The census reads one read-only repeatable-read snapshot whose statement_timeout is what is left
// of its deadline, so a census held up behind a lock ends at its bound rather than holding its
// snapshot for as long as the lock stands.
func TestTheCensusSnapshotIsBoundedByItsDeadline(t *testing.T) {
	f := newInboxCensus(t)
	ctx := context.Background()
	url := f.writes.database.Pool.Config().ConnString()
	conn, err := store.Connect(ctx, url, applicationName)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	bounded, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	tx, err := openCensusSnapshot(bounded, conn)
	if err != nil {
		t.Fatalf("open the census snapshot: %v", err)
	}
	var isolation, readOnly, timeout string
	if err := tx.QueryRow(ctx, `select current_setting('transaction_isolation'), current_setting('transaction_read_only'),
		current_setting('statement_timeout')`).Scan(&isolation, &readOnly, &timeout); err != nil {
		t.Fatalf("read the snapshot's settings: %v", err)
	}
	_ = tx.Rollback(ctx)
	if isolation != "repeatable read" || readOnly != "on" || !strings.HasSuffix(timeout, "s") {
		t.Errorf("the census snapshot is %q, read only %q, statement_timeout %q; want repeatable read, read only and a bound of the deadline's",
			isolation, readOnly, timeout)
	}
	if _, err := openCensusSnapshot(ctx, conn); err == nil {
		t.Error("a snapshot opened with no deadline opened; want it refused")
	}

	holder, err := f.writes.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the lock holder: %v", err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `lock table graph_nodes in access exclusive mode`); err != nil {
		t.Fatalf("lock graph_nodes: %v", err)
	}
	short, stop := context.WithTimeout(ctx, 3*time.Second)
	defer stop()
	started := time.Now()
	_, err = StoreCensus(short, url, false, nil)
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		t.Fatal("the census read through an access exclusive lock it waited on, want it ended at its bound")
	case time.Since(started) > 20*time.Second:
		t.Errorf("the census held behind the lock ended after %s, want about its 3s bound", time.Since(started))
	case !(errors.As(err, &pgErr) && pgErr.Code == "57014") && !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "timeout"):
		t.Errorf("the census held behind the lock ended with %v, want the statement timeout or its deadline", err)
	}
	t.Logf("the census held behind the lock ended after %s: %v", time.Since(started).Round(time.Millisecond), err)
}
