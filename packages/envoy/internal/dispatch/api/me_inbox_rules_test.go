package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/outbox"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/logtest"
)

// The nodes c-hidden-near-end seeds, each id fixed so two stores seeded alike answer the same
// literals: the project's comment, which the person may read, and the three the person may not -
// a comment reserved for its author, a node of that author's that mentions the person, and a node
// of a kind this read has never seen.
const (
	hiddenProjectComment = "7e1d0000-0000-4000-8000-000000000001"
	hiddenReserved       = "7e1d0000-0000-4000-8000-000000000002"
	hiddenMentioner      = "7e1d0000-0000-4000-8000-000000000003"
	hiddenUnseen         = "7e1d0000-0000-4000-8000-000000000004"
	hiddenUnseenKind     = "t10_kind_this_read_never_saw"
)

// hiddenNearEndAt is the moment of the seed's step n, fixed so both seeds answer one literal.
func hiddenNearEndAt(n int) time.Time {
	return time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC).Add(time.Duration(n) * time.Minute)
}

// seedHiddenNearEnd writes the four nodes and alice's tuple from each, and alice's Reply rows of the
// project comment's and the reserved comment's threads on issue, unread. hidden false leaves out
// the three tuples from the nodes she may not see. The three are written past ToPerson, whose
// refusal (graphstore.ErrReservedForAuthor) keeps every writer of this build from appending them,
// because what this test proves is what the read does with a tuple some other write left there: a
// node whose visibility changed after the write, or a node this child did not write.
func seedHiddenNearEnd(t *testing.T, database *store.Store, issue string, hidden bool) {
	t.Helper()
	ctx := context.Background()
	seed := meInboxSeed{t: t, database: database}
	carol, bob := model.Actor{Kind: "user", ID: "carol"}, model.Actor{Kind: "user", ID: "bob"}
	seed.node(graphstore.KindComment, hiddenProjectComment, graphstore.VisibilityProject, carol, hiddenNearEndAt(0))
	seed.node(graphstore.KindComment, hiddenReserved, graphstore.VisibilityAuthor, bob, hiddenNearEndAt(1))
	seed.node(graphstore.KindComment, hiddenMentioner, graphstore.VisibilityAuthor, bob, hiddenNearEndAt(2))
	seed.node(hiddenUnseenKind, hiddenUnseen, graphstore.VisibilityAuthor, bob, hiddenNearEndAt(3))
	seed.toPerson(meInboxTuple(graphstore.KindComment, hiddenProjectComment, meInboxDelivered, graphstore.KindPerson, "alice", hiddenNearEndAt(10)))
	if hidden {
		for _, tuple := range []graphstore.Tuple{
			meInboxTuple(graphstore.KindComment, hiddenReserved, meInboxDelivered, graphstore.KindPerson, "alice", hiddenNearEndAt(11)),
			meInboxTuple(graphstore.KindComment, hiddenMentioner, meInboxNamed, graphstore.KindPerson, "alice", hiddenNearEndAt(12)),
			meInboxTuple(graphstore.KindPerson, "alice", meInboxParticipates, hiddenUnseenKind, hiddenUnseen, hiddenNearEndAt(13)),
		} {
			insertTupleAround(t, database, tuple)
		}
	}
	for index, thread := range []string{hiddenProjectComment, hiddenReserved} {
		if _, err := database.Pool.Exec(ctx, `
			insert into user_threads (login, thread_id, kind, project_key, issue_key, read_through, replied_at)
			select 'alice', $1::uuid, 'comment', i.project_key, i.key, $3, $4 from issues i where i.key = $2
		`, thread, issue, hiddenNearEndAt(0), hiddenNearEndAt(20+index)); err != nil {
			t.Fatalf("seed alice's unread row of %s: %v", thread, err)
		}
	}
}

// insertTupleAround appends tuple by a direct insert into the log, around every writer of the
// store and so around ToPerson's refusal.
func insertTupleAround(t *testing.T, database *store.Store, tuple graphstore.Tuple) {
	t.Helper()
	who, err := json.Marshal(tuple.Who)
	if err != nil {
		t.Fatalf("encode the writer of %s: %v", tuple.Kind, err)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
		values ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9)
	`, tuple.FromKind, tuple.FromID, tuple.Kind, tuple.ToKind, tuple.ToID, tuple.At, string(who), tuple.SourceEvent, tuple.Asserted); err != nil {
		t.Fatalf("insert %s %s/%s -> %s/%s: %v", tuple.Kind, tuple.FromKind, tuple.FromID, tuple.ToKind, tuple.ToID, err)
	}
}

// c-hidden-near-end: the predicate runs inside the one statement, so a node the caller may not
// see is absent from every row, kind, moment, near end and count, exactly as though its tuple had
// never been appended; and no write appends such a tuple to a person who is not its author.
func TestTheOneReadDropsANodeItsCallerMayNotSee(t *testing.T) {
	// Not parallel: it compares two servers' reads of GET /api/v1/me/inbox whole, the event horizon
	// each answers (sinceHorizon) among them, and a test beside it holding a transaction id on the
	// cluster (holdClusterTransactionID) can leave the two answering at different horizons.
	type seededStore struct {
		handler  http.Handler
		database *store.Store
	}
	seeded := func(hidden bool) seededStore {
		handler, database := newTestHandlerWithStore(t)
		issue := createTestIssue(t, handler, "HIDE", "The hidden near end")
		seedHiddenNearEnd(t, database, issue, hidden)
		return seededStore{handler: handler, database: database}
	}
	withHidden, without := seeded(true), seeded(false)
	hidden := []string{hiddenReserved, hiddenMentioner, hiddenUnseen}
	hiddenAt := []time.Time{hiddenNearEndAt(11), hiddenNearEndAt(12), hiddenNearEndAt(13)}
	requireNoneHidden := func(t *testing.T, what string, answer meInboxResponse) {
		t.Helper()
		for _, entry := range answer.Entries {
			if slices.Contains(hidden, entry.Node.ID) || entry.Node.Kind == hiddenUnseenKind {
				t.Errorf("%s answers the hidden node %s:%s", what, entry.Node.Kind, entry.Node.ID)
			}
			for _, relation := range entry.Relations {
				if slices.ContainsFunc(hiddenAt, relation.At.Equal) {
					t.Errorf("%s answers a relation %s at %s, a hidden tuple's moment", what, relation.Kind, relation.At)
				}
			}
			if slices.ContainsFunc(hiddenAt, entry.Moment.Equal) {
				t.Errorf("%s answers an entry at %s, a hidden tuple's moment", what, entry.Moment)
			}
		}
	}
	countsTheRows := func(t *testing.T, what string, answer meInboxResponse) {
		t.Helper()
		if answer.Total != len(answer.Entries) {
			t.Errorf("%s counts %d and answers %d rows: a count is taken after the predicate", what, answer.Total, len(answer.Entries))
		}
	}

	t.Run("the read answers the project comment and nothing of the three", func(t *testing.T) {
		answer := readMeInboxAs(t, withHidden.handler, "alice", nil)
		requireNoneHidden(t, "the walk", answer)
		if nodes := meInboxNodes(answer); !slices.Equal(nodes, []string{graphstore.KindComment + ":" + hiddenProjectComment}) {
			t.Fatalf("alice's read answers %v, want the project comment alone", nodes)
		}
		countsTheRows(t, "the walk", answer)
	})

	t.Run("the answer is the one a store that never held the three tuples gives", func(t *testing.T) {
		for _, query := range []url.Values{nil, {"state": {myRepliesStateUnread}}} {
			got := readMeInboxAs(t, withHidden.handler, "alice", query)
			want := readMeInboxAs(t, without.handler, "alice", query)
			// The statement's own clock is a field of the answer no seed decides, and so is the id
			// of each gate document (approvalGateLine.ArtifactID): each store draws its documents'
			// ids itself. The gate's steps and approvals still have to agree.
			got.AsOf, want.AsOf = time.Time{}, time.Time{}
			for _, answer := range []*meInboxResponse{&got, &want} {
				for e := range answer.Entries {
					for l := range answer.Entries[e].ApprovalGate {
						answer.Entries[e].ApprovalGate[l].ArtifactID = nil
					}
				}
			}
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(want)
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("under %v the read over the hidden tuples answers\n%s\nwhere the store without them answers\n%s", query, gotJSON, wantJSON)
			}
		}
	})

	t.Run("every narrowing and page leaves the three out, and counts what it answers", func(t *testing.T) {
		for _, kind := range []string{meInboxDelivered, meInboxAuthored, meInboxParticipates, meInboxNamed, hiddenUnseenKind} {
			answer := readMeInboxAs(t, withHidden.handler, "alice", url.Values{"kind": {kind}})
			requireNoneHidden(t, "kind="+kind, answer)
			countsTheRows(t, "kind="+kind, answer)
		}
		unread := readMeInboxAs(t, withHidden.handler, "alice", url.Values{"state": {myRepliesStateUnread}})
		requireNoneHidden(t, "state=unread", unread)
		countsTheRows(t, "state=unread", unread)
		if nodes := meInboxNodes(unread); !slices.Equal(nodes, []string{graphstore.KindComment + ":" + hiddenProjectComment}) {
			t.Errorf("state=unread answers %v, want the project comment's thread alone of alice's two unread rows", nodes)
		}
		// One entry stands, behind the three hidden tuples, which are newer than its own. So the
		// first page of one answers it, and a page from offset 1 is past it: an offset or a limit
		// taken before the predicate lands the first page on a hidden row, and the page from
		// offset 1 on another.
		first := readMeInboxAs(t, withHidden.handler, "alice", url.Values{"limit": {"1"}, "offset": {"0"}})
		requireNoneHidden(t, "the first page", first)
		if nodes := meInboxNodes(first); !slices.Equal(nodes, []string{graphstore.KindComment + ":" + hiddenProjectComment}) || first.Total != 1 {
			t.Errorf("the first page of one answers %v of %d, want the project comment, of 1", nodes, first.Total)
		}
		paged := readMeInboxAs(t, withHidden.handler, "alice", url.Values{"limit": {"1"}, "offset": {"1"}})
		requireNoneHidden(t, "the page from offset 1", paged)
		if len(paged.Entries) != 0 || paged.Total != 1 {
			t.Errorf("the page from offset 1 answers %v of %d, want no entry of the one", meInboxNodes(paged), paged.Total)
		}
	})

	t.Run("a node the predicate lets through again counts one higher", func(t *testing.T) {
		before := readMeInboxAs(t, withHidden.handler, "alice", url.Values{"kind": {meInboxNamed}})
		seed := meInboxSeed{t: t, database: withHidden.database}
		seed.node(graphstore.KindComment, hiddenMentioner, graphstore.VisibilityProject, model.Actor{Kind: "user", ID: "bob"}, hiddenNearEndAt(2))
		after := readMeInboxAs(t, withHidden.handler, "alice", url.Values{"kind": {meInboxNamed}})
		if before.Total != 0 || after.Total != before.Total+1 || len(after.Entries) != after.Total {
			t.Fatalf("the %s count reads %d then %d (%d rows) once the mentioning node is the project's, want 0 then 1",
				meInboxNamed, before.Total, after.Total, len(after.Entries))
		}
	})

	t.Run("the write refuses each kind to anyone but the node's author", func(t *testing.T) {
		reserved := func(person string) []graphstore.Tuple {
			return []graphstore.Tuple{
				meInboxTuple(graphstore.KindComment, hiddenReserved, meInboxDelivered, graphstore.KindPerson, person, hiddenNearEndAt(30)),
				meInboxTuple(graphstore.KindComment, hiddenReserved, meInboxNamed, graphstore.KindPerson, person, hiddenNearEndAt(30)),
				meInboxTuple(graphstore.KindPerson, person, meInboxParticipates, graphstore.KindComment, hiddenReserved, hiddenNearEndAt(30)),
			}
		}
		write := func(tuples ...graphstore.Tuple) error {
			return pgx.BeginFunc(context.Background(), withHidden.database.Pool, func(tx pgx.Tx) error {
				return graphstore.ToPerson(context.Background(), tx, tuples)
			})
		}
		for _, tuple := range reserved("carol") {
			err := write(tuple)
			if !errors.Is(err, graphstore.ErrReservedForAuthor) ||
				!strings.Contains(err.Error(), graphstore.KindComment+"/"+hiddenReserved) ||
				!strings.Contains(err.Error(), "visibility "+graphstore.VisibilityAuthor) {
				t.Errorf("ToPerson of %s to carol from bob's reserved comment = %v, want ErrReservedForAuthor naming the node and its visibility", tuple.Kind, err)
			}
		}
		if err := write(reserved("bob")...); err != nil {
			t.Fatalf("ToPerson of the three to the comment's own author: %v", err)
		}
	})
}

// meInboxDeliveredTo is every node a current delivered_to tuple of kind/id reaches, as kind:id.
func meInboxDeliveredTo(t *testing.T, database *store.Store, kind, id string) []string {
	t.Helper()
	tuples, err := graphstore.CurrentOut(context.Background(), database.Pool, kind, id, []string{meInboxDelivered})
	if err != nil {
		t.Fatalf("read what %s %s was delivered to: %v", kind, id, err)
	}
	reached := make([]string, 0, len(tuples))
	for _, tuple := range tuples {
		reached = append(reached, tuple.ToKind+":"+tuple.ToID)
	}
	slices.Sort(reached)
	return reached
}

// meInboxWalkDepth is outbox.maxCommentThreadDepth, the delivery walk's depth bound, read from the
// declaration itself: the constant is unexported and nothing exported carries it.
func meInboxWalkDepth(t *testing.T) int {
	t.Helper()
	source, err := os.ReadFile("../outbox/publisher.go")
	if err != nil {
		t.Fatalf("read outbox.maxCommentThreadDepth's declaration: %v", err)
	}
	declared := regexp.MustCompile(`(?m)^\s*maxCommentThreadDepth\s*=\s*(\d+)\s*$`).FindSubmatch(source)
	if declared == nil {
		t.Fatal("outbox/publisher.go declares no maxCommentThreadDepth = <number>")
	}
	depth, _ := strconv.Atoi(string(declared[1]))
	return depth
}

// meInboxSessionComment is one comment written on issue by the session id, a root.
func meInboxSessionComment(t *testing.T, handler http.Handler, issue, session, body string) string {
	t.Helper()
	return createThreadComment(t, handler, issue, map[string]any{
		"body": body, "actor": map[string]any{"kind": "session", "id": session},
	}, "").ID
}

// settleOutboxWithin runs the outbox until every event of issue is published, failing once ctx
// ends with what is still unpublished.
func settleOutboxWithin(t *testing.T, ctx context.Context, deps Deps, issue, what string) {
	t.Helper()
	running, stop := context.WithCancel(ctx)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		outbox.New(outbox.Deps{
			Store: deps.Store, Publisher: &recordingOutboxPublisher{}, Broker: events.NewBroker(),
			Listen: deps.Store.Pool.Listener, RoleClaims: claimedRole{}, Sessions: claimedRole{},
		}).Run(running)
	}()
	defer func() {
		stop()
		<-stopped
	}()
	for {
		var unpublished int
		if err := deps.Store.Pool.QueryRow(context.Background(),
			`select count(*) from events where issue_key = $1 and published_at is null`, issue).Scan(&unpublished); err != nil {
			t.Fatalf("count the unpublished events of %s: %v", issue, err)
		}
		if unpublished == 0 {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the outbox left %d events of %s unpublished at the deadline: %s", unpublished, issue, what)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// meInboxCase is one rule c-subtractions holds, run on its own.
type meInboxCase struct {
	name string
	run  func(t *testing.T)
}

// c-subtractions: every rule that holds mail back is kept, one case each, and each reads the
// delivery tuples, since every rule here decides a delivery rather than the read of R1. The three
// cases of a session's copy left to a mention of it - sent, failed and still being sent - read the
// copies the outbox delivered out of the comment's node beside the attempt's own delivery.
func TestOneInboxKeepsEverySubtractionRule(t *testing.T) {
	cases := []meInboxCase{
		{"the person's own ask, comment and message draw no delivery to them", meInboxOwnWrites},
		{"a session whose mention of a comment was sent gets no second copy", meInboxMentionSent},
		{"a session whose mention of a comment failed gets one copy", meInboxMentionFailed},
		{"a session whose mention of a comment is still being sent is held back, and the event is read again", meInboxMentionPending},
		{"a claimed or routed issue sends a person's comment to the claimant and the route, not the primary document's writers", meInboxClaimantAndRoute},
		{"a direct reply is unread past the read mark and the Clear alone, from the one person node", meInboxTwoMarks},
		{"a reply already marked by its id is out", meInboxReadByID},
		{"an open ask on an open issue assigned to the person is listed once, not as a thread row", meInboxAskOnce},
		{"a dismissed thread is out, a thread row and an agent row alike", meInboxDismissed},
		{"a snoozed ask reads as snoozed", meInboxSnoozed},
		{"the delivery walk ends a cycle and reaches its authors once", meInboxWalkCycle},
		{"the delivery walk answers a chain at its depth bound whole and cuts one a reply longer", meInboxWalkDepthCase},
	}
	for _, tc := range cases {
		t.Run(tc.name, tc.run)
	}
}

// meInboxOwnWrites holds the rule that leaves the writer out of their own write. A write that
// starts a thread has nobody in it yet, so its writer is among no people it could reach, and the
// rule is read only where the writer is in the thread already. So beside her own comment, ask and
// message, each a thread of its own, alice writes once more in a comment thread and in an issue
// message thread that bob started and she replied in, each of which she takes part in from that
// first reply: her second reply reaches every participant but her (inboxwrite.PlanRecord's
// own-login clause). Her ask waits on her, as bob's ask on the same issue shows, and draws no
// delivered_to tuple, since the person it waits on asked it (waitingOnTuples). Each write's
// delivery tuples hold no tuple to her, the thread it is written in is no row of her unread list,
// and the write stands in her read under its authorship tuple.
func meInboxOwnWrites(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createTestIssue(t, handler, "OWN", "Alice's own writes")
	comment := createThreadComment(t, handler, issue, map[string]any{"body": "Alice's own comment."}, "alice")
	ask := createReferenceAPIAsk(t, handler, issue, "Alice's own question?")
	message := createIssueMessage(t, handler, issue, map[string]any{"body": "Alice's own message."}, "alice")
	bobsThread := createThreadComment(t, handler, issue, map[string]any{"body": "Bob starts a thread."}, "bob")
	createThreadComment(t, handler, issue, map[string]any{"body": "Alice replies to him.", "reply_to": bobsThread.ID}, "alice")
	reply := createThreadComment(t, handler, issue, map[string]any{"body": "Alice writes in it again.", "reply_to": bobsThread.ID}, "alice")
	bobsMessage := createIssueMessage(t, handler, issue, map[string]any{"body": "Bob starts a conversation."}, "bob")
	createIssueMessage(t, handler, issue, map[string]any{"body": "Alice answers him.", "in_reply_to": bobsMessage.ID}, "alice")
	answer := createIssueMessage(t, handler, issue, map[string]any{"body": "Alice writes in it again.", "in_reply_to": bobsMessage.ID}, "alice")
	bobsAsk := createPersonAsk(t, handler, issue, "bob", "Bob's question?")

	// What makes each case read the rule: alice takes part in bob's two threads, and an ask of
	// this issue waits on her.
	taking, err := graphstore.CurrentOut(context.Background(), database.Pool, graphstore.KindPerson, "alice", []string{meInboxParticipates})
	if err != nil {
		t.Fatalf("read the threads alice takes part in: %v", err)
	}
	takesPart := make([]string, 0, len(taking))
	for _, tuple := range taking {
		takesPart = append(takesPart, tuple.ToKind+":"+tuple.ToID)
	}
	for _, thread := range []string{graphstore.KindComment + ":" + bobsThread.ID, graphstore.KindMessage + ":" + bobsMessage.ID} {
		if !slices.Contains(takesPart, thread) {
			t.Fatalf("alice takes part in %v, not in bob's thread %s, so her second reply there reads no rule", takesPart, thread)
		}
	}
	if reached := meInboxDeliveredTo(t, database, graphstore.KindAsk, bobsAsk.ID); !slices.Contains(reached, graphstore.KindPerson+":alice") {
		t.Fatalf("bob's ask on %s reached %v and not alice, so the asks of the issue do not wait on her", issue, reached)
	}

	unread := readMeInboxAs(t, handler, "alice", url.Values{"state": {myRepliesStateUnread}})
	walked := readMeInboxAs(t, handler, "alice", nil)
	for _, own := range []struct {
		what, kind, id string
		// threadKind and threadID are the root of the thread the write is in, and others each
		// person else in it whom the write reaches.
		threadKind, threadID string
		others               []string
	}{
		{"comment", graphstore.KindComment, comment.ID, graphstore.KindComment, comment.ID, nil},
		{"ask, which waits on her", graphstore.KindAsk, ask.ID, graphstore.KindAsk, ask.ID, nil},
		{"message", graphstore.KindMessage, message.ID, graphstore.KindMessage, message.ID, nil},
		{"reply in bob's comment thread", graphstore.KindComment, reply.ID, graphstore.KindComment, bobsThread.ID, []string{"bob"}},
		{"reply in bob's message thread", graphstore.KindMessage, answer.ID, graphstore.KindMessage, bobsMessage.ID, []string{"bob"}},
	} {
		reached := meInboxDeliveredTo(t, database, own.kind, own.id)
		if slices.Contains(reached, graphstore.KindPerson+":alice") {
			t.Errorf("alice's own %s %s drew a delivery to her: %v", own.what, own.id, reached)
		}
		for _, other := range own.others {
			if !slices.Contains(reached, graphstore.KindPerson+":"+other) {
				t.Errorf("alice's own %s %s reached %v, want %s, who takes part in it", own.what, own.id, reached, other)
			}
		}
		if _, held := meInboxEntryOfNode(unread, own.threadKind, own.threadID); held {
			t.Errorf("the thread of alice's own %s is in her unread list", own.what)
		}
		entry, held := meInboxEntryOfNode(walked, own.kind, own.id)
		if !held || !slices.Contains(meInboxKinds(entry), meInboxAuthored+"/"+meInboxInbound) {
			t.Errorf("alice's own %s stands in her read as %v, want it under its %s tuple", own.what, meInboxKinds(entry), meInboxAuthored)
		}
	}
}

// meInboxMentionedAlone posts alice's comment mentioning session alone - nothing else of the comment
// routes to it - on a fresh issue of a server whose listener is listener, and answers the server's
// store and deps, the issue's key and the comment's id.
func meInboxMentionedAlone(t *testing.T, listener string, timeout time.Duration, session string) (*store.Store, Deps, string, string) {
	t.Helper()
	handler, database, deps := newTestServer(t, testServerOptions{envoyURL: listener, envoyTimeout: timeout})
	issue := createInteractionIssue(t, handler, "MENT", "A mention of "+session+" alone", "before")
	created := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Are you there?", "mentions": []map[string]any{{"target": "session:" + session}},
	}))
	return database, deps, issue.Key, created.ID
}

// meInboxSessionDeliveries is the type of the event that caused each standing delivered_to tuple
// out of comment's node to session, in the order they were appended: a mention's delivery is
// recorded under the comment.delivery receipt of the attempt the listener took, and a copy the
// outbox published under the comment.created it is a copy of (outbox.deliveredGraph).
func meInboxSessionDeliveries(t *testing.T, database *store.Store, comment, session string) []string {
	t.Helper()
	causes, err := readStrings(context.Background(), database.Pool, `
		select e.type from graph_tuples t join events e on e.id = t.source_event
		where t.from_kind = 'comment' and t.from_id = $1 and t.kind = $2 and t.to_kind = 'session' and t.to_id = $3
		  and t.asserted
		order by t.seq
	`, comment, graphstore.TupleDeliveredTo, session)
	if err != nil {
		t.Fatalf("read the deliveries of comment %s to %s: %v", comment, session, err)
	}
	return causes
}

// meInboxAttemptReached is each session the first attempt of comment's mention of target
// delivered it to, as the delivered_to tuples out of the attempt's node hold them.
func meInboxAttemptReached(t *testing.T, database *store.Store, comment, target string) []string {
	t.Helper()
	return meInboxDeliveredTo(t, database, graphstore.KindDeliveryAttempt, graphstore.CommentAttempt(comment, target, 1).ID())
}

func meInboxMentionSent(t *testing.T) {
	listener, _ := newMentionListener(t)
	database, deps, issue, comment := meInboxMentionedAlone(t, listener.URL, time.Second, "s1")
	if settled := settledCommentDeliveries(t, database, comment); len(settled) != 1 || settled[0].State != model.DeliveryStateSent {
		t.Fatalf("the mention to a listener that answers = %#v, want its one attempt sent", settled)
	}
	runOutboxUntilSettled(t, deps, &recordingOutboxPublisher{}, issue, comment)
	if reached := meInboxAttemptReached(t, database, comment, "session:s1"); !slices.Equal(reached, []string{graphstore.KindSession + ":s1"}) {
		t.Fatalf("the sent mention's attempt reached %v, want s1", reached)
	}
	if causes := meInboxSessionDeliveries(t, database, comment, "s1"); !slices.Equal(causes, []string{"comment.delivery"}) {
		t.Errorf("the comment reached s1 under %v, want the mention's one delivery alone: s1 drew a second copy", causes)
	}
	if destinations := publishedDestinationsOf(t, database, "comment.created", comment); slices.Contains(destinations, contracts.LegacyAgentSubject("s1")) {
		t.Errorf("comment.created was published to %v: s1, whose mention was sent, drew a second copy", destinations)
	}
}

func meInboxMentionFailed(t *testing.T) {
	listener, state := newMentionListener(t)
	// Never released: the send to s2 waits until its caller gives up, and the attempt fails.
	_ = state.hold("s2")
	database, deps, issue, comment := meInboxMentionedAlone(t, listener.URL, time.Second, "s2")
	if settled := settledCommentDeliveries(t, database, comment); len(settled) != 1 || settled[0].State != model.DeliveryStateFailed {
		t.Fatalf("the mention whose send never answered = %#v, want its one attempt failed", settled)
	}
	runOutboxUntilSettled(t, deps, &recordingOutboxPublisher{}, issue, comment)
	if reached := meInboxAttemptReached(t, database, comment, "session:s2"); len(reached) != 0 {
		t.Fatalf("the failed mention's attempt reached %v, want nobody", reached)
	}
	if causes := meInboxSessionDeliveries(t, database, comment, "s2"); !slices.Equal(causes, []string{"comment.created"}) {
		t.Errorf("the comment reached s2 under %v, want the outbox's one copy: its mention failed", causes)
	}
}

func meInboxMentionPending(t *testing.T) {
	listener, state := newMentionListener(t)
	release := state.hold("s3")
	database, deps, issue, comment := meInboxMentionedAlone(t, listener.URL, 30*time.Second, "s3")
	if session := state.awaitSend(t); session != "s3" {
		t.Fatalf("the mention's send went to %s, want s3", session)
	}
	// While the send is held its attempt is pending, and the outbox that reads the comment's event
	// then holds it back: the event stays unpublished, to be read again, and nothing is delivered
	// to s3.
	runOutboxFor(t, deps, &recordingOutboxPublisher{}, time.Second)
	if row := commentCreatedRow(t, database, comment); row.publishedAt != nil {
		t.Errorf("comment.created was published while its mention of s3 was still being sent, want it held back")
	}
	if causes := meInboxSessionDeliveries(t, database, comment, "s3"); len(causes) != 0 {
		t.Errorf("the comment reached s3 under %v while the mention was still being sent, want nothing yet", causes)
	}
	release()
	if settled := settledCommentDeliveries(t, database, comment); len(settled) != 1 || settled[0].State != model.DeliveryStateSent {
		t.Fatalf("the mention released = %#v, want its one attempt sent", settled)
	}
	// Read again once the mention delivered the comment: the event is published, and s3 has the
	// mention's delivery alone.
	runOutboxUntilSettled(t, deps, &recordingOutboxPublisher{}, issue, comment)
	if row := commentCreatedRow(t, database, comment); row.publishedAt == nil {
		t.Errorf("comment.created is unpublished once its mention was sent")
	}
	if causes := meInboxSessionDeliveries(t, database, comment, "s3"); !slices.Equal(causes, []string{"comment.delivery"}) {
		t.Errorf("the comment reached s3 under %v once its mention was sent, want the mention's one delivery alone", causes)
	}
}

// runOutboxFor runs an outbox over deps' store for d and stops it, for a case that reads what one
// such run leaves unsettled.
func runOutboxFor(t *testing.T, deps Deps, publisher outbox.Publisher, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	outbox.New(outbox.Deps{
		Store: deps.Store, Publisher: publisher, Broker: events.NewBroker(),
		Listen:     deps.Store.Pool.Listener,
		RoleClaims: claimedRole{}, Sessions: claimedRole{},
	}).Run(ctx)
}

// meInboxClaimantAndRoute holds that a claimed or a routed issue sends a person's comment to the
// claimant and to the route, rather than to the writers of the issue's primary document: a claim
// stands those writers down on its own, and so does a route, so each is driven alone, on an issue of
// its own, beside an issue claimed and routed at once. Each issue's primary document credits the
// session s-doc, which hears alice's comment on an issue nobody holds.
func meInboxClaimantAndRoute(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	// reachedOn opens an issue of project, credits s-doc with its primary document, gives it
	// claimant and route where they are not empty, and answers what alice's comment on it reached
	// once the outbox has published it.
	reachedOn := func(t *testing.T, project, claimant, route string) []string {
		t.Helper()
		issue := createInteractionIssue(t, handler, project, "An issue of "+project, "before")
		if _, err := database.Pool.Exec(context.Background(), `
			update artifacts set created_by = '{"kind":"session","id":"s-doc"}'::jsonb where id = $1
		`, issue.PrimaryArtifactID); err != nil {
			t.Fatalf("credit s-doc with the primary document of %s: %v", issue.Key, err)
		}
		if claimant != "" {
			claimIssueFor(t, database, issue.Key, claimant)
		}
		if route != "" {
			if patched := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{"route": route}, "alice"); patched.Code != http.StatusOK {
				t.Fatalf("route %s to %s: status=%d body=%s", issue.Key, route, patched.Code, patched.Body.String())
			}
		}
		id := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Who hears this?"}, "alice").ID
		runOutboxUntilSettled(t, deps, &recordingOutboxPublisher{}, issue.Key, id)
		return meInboxDeliveredTo(t, database, graphstore.KindComment, id)
	}
	writer := graphstore.KindSession + ":s-doc"
	t.Run("nobody holds the issue", func(t *testing.T) {
		if reached := reachedOn(t, "FREE", "", ""); !slices.Contains(reached, writer) {
			t.Fatalf("a comment on an issue nobody holds reached %v, want the primary document's writer s-doc among them", reached)
		}
	})
	for _, held := range []struct {
		name, project, claimant, route string
		// hears is each session that holds the issue, which the comment reaches.
		hears []string
	}{
		{"claimed alone", "CLAIM", "s-claim", "", []string{graphstore.KindSession + ":s-claim"}},
		{"routed alone", "ROUTE", "", "session:s-route", []string{graphstore.KindSession + ":s-route"}},
		{"claimed and routed", "BOTH", "s-claim", "session:s-route", []string{graphstore.KindSession + ":s-claim", graphstore.KindSession + ":s-route"}},
	} {
		t.Run(held.name, func(t *testing.T) {
			reached := reachedOn(t, held.project, held.claimant, held.route)
			for _, want := range held.hears {
				if !slices.Contains(reached, want) {
					t.Errorf("the comment on the issue %s reached %v, want %s among them", held.name, reached, want)
				}
			}
			if slices.Contains(reached, writer) {
				t.Errorf("the comment on the issue %s reached %v: the primary document's writer s-doc stands down there", held.name, reached)
			}
		})
	}
}

// meInboxAgentEntry is the entry of login's read under state whose node is the root message.
func meInboxAgentEntry(t *testing.T, handler http.Handler, login, state, root string) (meInboxEntry, bool) {
	t.Helper()
	return meInboxEntryOfNode(readMeInboxAs(t, handler, login, url.Values{"state": {state}}), graphstore.KindMessage, root)
}

func meInboxTwoMarks(t *testing.T) {
	handler, database, root, reply, _ := directConversationFrom(t, "Alice")
	first := decodeBody[model.Message](t, reply("On it."))
	if reached := meInboxDeliveredTo(t, database, graphstore.KindMessage, first.ID); !slices.Contains(reached, graphstore.KindPerson+":alice") {
		t.Fatalf("s1's reply reached %v, want the person who started the conversation", reached)
	}
	if entry, held := meInboxAgentEntry(t, handler, "alice", myRepliesStateUnread, root.ID); !held || entry.Agent == nil || !entry.Agent.Unread {
		t.Fatalf("before any mark, alice's unread read holds %+v, want the conversation as an unread agent row", entry)
	}
	putAgentState(t, handler, "aLiCe", "s1", map[string]any{"read_through": first.CreatedAt})
	if _, held := meInboxAgentEntry(t, handler, "alice", myRepliesStateUnread, root.ID); held {
		t.Errorf("after the read mark alone, written under aLiCe, the conversation is still unread for alice")
	}
	second := decodeBody[model.Message](t, reply("And done."))
	if _, held := meInboxAgentEntry(t, handler, "Alice", myRepliesStateUnread, root.ID); !held {
		t.Fatalf("a reply past the read mark is not unread for Alice")
	}
	putAgentState(t, handler, "Alice", "s1", map[string]any{"cleared_before": second.CreatedAt})
	if _, held := meInboxAgentEntry(t, handler, "alice", myRepliesStateUnread, root.ID); held {
		t.Errorf("after the Clear alone, written under Alice, the conversation is still unread for alice")
	}
	if reached := meInboxDeliveredTo(t, database, graphstore.KindMessage, second.ID); !slices.Contains(reached, graphstore.KindPerson+":alice") {
		t.Errorf("the second reply reached %v: a mark takes a reply out of the list, never its delivery", reached)
	}
}

func meInboxReadByID(t *testing.T) {
	handler, database, root, reply, _ := directConversationFrom(t, "Alice")
	answer := decodeBody[model.Message](t, reply("Read me by my id."))
	if _, held := meInboxAgentEntry(t, handler, "alice", myRepliesStateUnread, root.ID); !held {
		t.Fatalf("the reply is not unread before it is read")
	}
	putAgentState(t, handler, "alice", "s1", map[string]any{"read_replies": []string{answer.ID}})
	if _, held := meInboxAgentEntry(t, handler, "alice", myRepliesStateUnread, root.ID); held {
		t.Errorf("the reply read by its id is still unread")
	}
	if reached := meInboxDeliveredTo(t, database, graphstore.KindMessage, answer.ID); !slices.Contains(reached, graphstore.KindPerson+":alice") {
		t.Errorf("the reply read by its id reached %v, want its delivery to alice standing", reached)
	}
}

func meInboxAskOnce(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createTestIssue(t, handler, "ONCE", "An ask that waits on alice")
	ask := createPersonAsk(t, handler, issue, "bob", "Bob's question for alice?")
	createThreadComment(t, handler, issue, map[string]any{"body": "Alice writes in its thread.", "ask_id": ask.ID}, "alice")
	createThreadComment(t, handler, issue, map[string]any{"body": "Bob answers her there.", "ask_id": ask.ID}, "bob")
	if reached := meInboxDeliveredTo(t, database, graphstore.KindAsk, ask.ID); !slices.Contains(reached, graphstore.KindPerson+":alice") {
		t.Fatalf("the ask reached %v, want alice, whom it waits on", reached)
	}
	inbox := readMeInboxAs(t, handler, "alice", url.Values{"state": {myRepliesStateInbox}})
	held := 0
	for _, entry := range inbox.Entries {
		if entry.Node.Kind == graphstore.KindAsk && entry.Node.ID == ask.ID {
			held++
			if entry.Ask == nil || entry.Thread != nil {
				t.Errorf("the ask waiting on alice is drawn as ask %v, thread %v: want its ask row alone", entry.Ask != nil, entry.Thread != nil)
			}
		}
	}
	if held != 1 {
		t.Errorf("the ask waiting on alice is %d entries of her inbox, want one", held)
	}
	if entry, ok := meInboxEntryOfNode(readMeInboxAs(t, handler, "alice", url.Values{"state": {myRepliesStateUnread}}), graphstore.KindAsk, ask.ID); ok {
		t.Errorf("the ask waiting on alice is a thread row of her unread list: %+v", entry)
	}
}

func meInboxDismissed(t *testing.T) {
	handler, database, root, reply, _ := directConversationFrom(t, "alice")
	decodeBody[model.Message](t, reply("An answer to dismiss."))
	issue := createTestIssue(t, handler, "GONE", "A thread to dismiss")
	thread := createThreadComment(t, handler, issue, map[string]any{"body": "Alice starts a thread."}, "alice")
	bobs := createThreadComment(t, handler, issue, map[string]any{"body": "Bob writes in it.", "reply_to": thread.ID}, "bob")
	if reached := meInboxDeliveredTo(t, database, graphstore.KindComment, bobs.ID); !slices.Contains(reached, graphstore.KindPerson+":alice") {
		t.Fatalf("bob's reply reached %v, want alice, who started the thread", reached)
	}
	inbox := readMeInboxAs(t, handler, "alice", url.Values{"state": {myRepliesStateInbox}})
	threadRow, heldThread := meInboxEntryOfNode(inbox, graphstore.KindComment, thread.ID)
	agentRow, heldAgent := meInboxEntryOfNode(inbox, graphstore.KindMessage, root.ID)
	if !heldThread || threadRow.Thread == nil || !heldAgent || agentRow.Agent == nil {
		t.Fatalf("alice's inbox is %v, want the comment thread's row and the conversation's agent row", meInboxNodes(inbox))
	}
	dismissed := postInboxDismiss(t, handler, "alice", map[string]any{
		"threads": []string{inboxDismissReplyPrefix + thread.ID, inboxDismissAgentPrefix + root.ID},
		"seen_replies": map[string]any{
			inboxDismissReplyPrefix + thread.ID: map[string]any{"version": *threadRow.Thread.Version},
			inboxDismissAgentPrefix + root.ID:   map[string]any{"newest_reply": agentRow.Agent.Reply.ID, "unread_replies": agentRow.Agent.UnreadReplyIDs},
		},
	})
	if dismissed.Code != http.StatusOK {
		t.Fatalf("dismiss: status=%d body=%s", dismissed.Code, dismissed.Body.String())
	}
	after := readMeInboxAs(t, handler, "alice", url.Values{"state": {myRepliesStateInbox}})
	out := readMeInboxAs(t, handler, "alice", url.Values{"state": {myRepliesStateDismissed}})
	for _, node := range []struct{ kind, id, row string }{
		{graphstore.KindComment, thread.ID, "thread"}, {graphstore.KindMessage, root.ID, "agent"},
	} {
		if _, held := meInboxEntryOfNode(after, node.kind, node.id); held {
			t.Errorf("the dismissed %s row is still in alice's inbox", node.row)
		}
		if _, held := meInboxEntryOfNode(out, node.kind, node.id); !held {
			t.Errorf("the dismissed %s row is not in alice's dismissed list", node.row)
		}
	}
}

// meInboxSnoozed holds that a snooze decides what the ask reads as and no delivery: the ask's
// delivery to alice, whom it waits on, stands as its create appended it, and her ask row carries
// the snooze.
func meInboxSnoozed(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createTestIssue(t, handler, "SNOOZE", "A snoozed ask")
	ask := createPersonAsk(t, handler, issue, "bob", "Snooze me?")
	delivered := meInboxDeliveredTo(t, database, graphstore.KindAsk, ask.ID)
	if !slices.Contains(delivered, graphstore.KindPerson+":alice") {
		t.Fatalf("the ask reached %v, want alice, whom it waits on", delivered)
	}
	until := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if snoozed := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/asks/"+ask.ID+"/snooze", map[string]any{"snoozed_until": until}, "alice"); snoozed.Code/100 != 2 {
		t.Fatalf("snooze: status=%d body=%s", snoozed.Code, snoozed.Body.String())
	}
	if after := meInboxDeliveredTo(t, database, graphstore.KindAsk, ask.ID); !slices.Equal(after, delivered) {
		t.Errorf("after alice snoozed the ask it reaches %v, want %v as before: a snooze takes no delivery away", after, delivered)
	}
	entry, held := meInboxEntryOfNode(readMeInboxAs(t, handler, "alice", url.Values{"state": {myRepliesStateInbox}}), graphstore.KindAsk, ask.ID)
	if !held || entry.Ask == nil || entry.Ask.SnoozedUntil == nil {
		t.Fatalf("the snoozed ask reads %+v, want its ask row carrying the snooze", entry)
	}
	if at, err := time.Parse(time.RFC3339Nano, *entry.Ask.SnoozedUntil); err != nil || at.Format(time.RFC3339) != until {
		t.Errorf("the ask reads snoozed until %s, want %s", *entry.Ask.SnoozedUntil, until)
	}
}

// meInboxWalkUp is the delivery walk's statement (outbox's commentRecipientsQuery) as the pool
// that runs it sees it, and in its one group the body of its walk up: the recursive CTE up, whose
// rows are the comments the walk visits from each comment it was given, each beside that comment
// (key).
var meInboxWalkUp = regexp.MustCompile(`(?s)^\s*with recursive\s+up as \((.*?)\n\s*\),\s*roots as \(`)

// meInboxWalkRuns records each run of the delivery walk on the pool it traces (Traced): the body
// of its walk up and the arguments the run bound, for a statement sent on its own or in a batch.
type meInboxWalkRuns struct {
	mu   sync.Mutex
	runs []meInboxWalkRun
}

type meInboxWalkRun struct {
	up   string
	args []any
}

func (w *meInboxWalkRuns) record(sql string, args []any) {
	found := meInboxWalkUp.FindStringSubmatch(sql)
	if found == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.runs = append(w.runs, meInboxWalkRun{up: found[1], args: slices.Clone(args)})
}

func (w *meInboxWalkRuns) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	w.record(data.SQL, data.Args)
	return ctx
}

func (w *meInboxWalkRuns) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (w *meInboxWalkRuns) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	for _, queued := range data.Batch.QueuedQueries {
		w.record(queued.SQL, queued.Arguments)
	}
	return ctx
}

func (w *meInboxWalkRuns) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

func (w *meInboxWalkRuns) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// taken is every run recorded so far.
func (w *meInboxWalkRuns) taken() []meInboxWalkRun {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.runs)
}

// meInboxWalkCycle holds the cycle guard: two comments whose reply_to point at each other, and
// alice's reply under the first. The walk returns, alice's reply reaches both of the cycle's
// authors, and no row is visited twice. That last is read off the walk itself, never off the
// deliveries, which the store's repeat key keeps single whatever the walk visited: each run of the
// walk the outbox made has its walk up run again, as the run wrote it and with the arguments it
// bound, over the same seed. The walk reads the thread a reply is written under, so its run for
// alice's reply walks up from the first comment, and each comment it visits is one row of that
// walk, from each comment the run was given.
func meInboxWalkCycle(t *testing.T) {
	walks := &meInboxWalkRuns{}
	handler, database, deps := newTestServer(t, testServerOptions{tracer: walks})
	issue := createTestIssue(t, handler, "CYCLE", "Two comments that reply to each other")
	first := meInboxSessionComment(t, handler, issue, "s-first", "The first of a cycle.")
	second := meInboxSessionComment(t, handler, issue, "s-second", "The second of a cycle.")
	reply := createThreadComment(t, handler, issue, map[string]any{"body": "Alice answers the first.", "reply_to": first}, "alice").ID
	// comments.reply_to carries no acyclicity constraint, so the two reply to each other once the
	// reply that names the first is written.
	if _, err := database.Pool.Exec(context.Background(), `
		update comments set reply_to = case id when $1::uuid then $2::uuid else $1::uuid end where id in ($1::uuid, $2::uuid)
	`, first, second); err != nil {
		t.Fatalf("tie the two comments into a cycle: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	settleOutboxWithin(t, ctx, deps, issue, "a cycle of two comments under alice's reply")
	reached := meInboxDeliveredTo(t, database, graphstore.KindComment, reply)
	for _, session := range []string{"s-first", "s-second"} {
		if !slices.Contains(reached, graphstore.KindSession+":"+session) {
			t.Errorf("alice's reply in the cycle reached %v, want %s, who wrote in it", reached, session)
		}
	}
	var visits int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from graph_tuples where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4
	`, graphstore.KindComment, reply, meInboxDelivered, graphstore.KindSession).Scan(&visits); err != nil {
		t.Fatalf("count alice's reply's deliveries to sessions: %v", err)
	}
	if visits != 2 {
		t.Errorf("alice's reply holds %d deliveries to sessions, want one to each of the cycle's two authors", visits)
	}
	runs := walks.taken()
	walkedCycle := false
	for _, run := range runs {
		rows, err := readStrings(context.Background(), database.Pool, `with recursive up as (`+run.up+`
			)
			select key::text || ' > ' || id::text from up order by 1`, run.args...)
		if err != nil {
			t.Fatalf("run the delivery walk's walk up again with the arguments %v: %v", run.args, err)
		}
		visited := map[string]int{}
		for _, row := range rows {
			visited[row]++
		}
		var repeated []string
		for _, row := range slices.Sorted(maps.Keys(visited)) {
			if times := visited[row]; times > 1 {
				repeated = append(repeated, fmt.Sprintf("%s %d times", row, times))
			}
		}
		if len(repeated) > 0 {
			t.Errorf("the delivery walk run with %v visits a row more than once (from > visited): %s; its walk up visits\n  %s",
				run.args, strings.Join(repeated, ", "), strings.Join(rows, "\n  "))
		}
		walkedCycle = walkedCycle || (slices.Contains(rows, first+" > "+first) && slices.Contains(rows, first+" > "+second))
	}
	if !walkedCycle {
		t.Fatalf("the outbox ran the delivery walk %d times, and none walked up from %s, the comment alice replied under, through both comments of the cycle", len(runs), first)
	}
}

func meInboxWalkDepthCase(t *testing.T) {
	depth := meInboxWalkDepth(t)
	log := &logtest.Buffer{}
	logtest.SetDefault(t, slog.NewTextHandler(log, nil))
	handler, database, deps := newTestServer(t, testServerOptions{})
	createTestProject(t, handler, "DEEP")
	// chain writes a root and replies under it, each one under the one before and each by a
	// session of its own, and alice's reply under the root, which the walk reads the chain down
	// from. It answers the chain's comments, root first, and alice's reply.
	chain := func(replies int) ([]string, string) {
		t.Helper()
		created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
			"project": "DEEP", "title": fmt.Sprintf("A chain of %d replies", replies),
		}, "alice")
		if created.Code != http.StatusCreated {
			t.Fatalf("create the chain's issue: status=%d body=%s", created.Code, created.Body.String())
		}
		issue := decodeBody[model.Issue](t, created).Key
		comments := make([]string, 0, replies+1)
		for index := 0; index <= replies; index++ {
			comments = append(comments, meInboxSessionComment(t, handler, issue, fmt.Sprintf("s-%02d", index), fmt.Sprintf("Link %d.", index)))
		}
		reply := createThreadComment(t, handler, issue, map[string]any{"body": "Alice answers the root.", "reply_to": comments[0]}, "alice").ID
		// A chain one level a reply, as a build before every reply was stored against its thread's
		// head wrote one: each comment replies to the one before it.
		for index := 1; index < len(comments); index++ {
			if _, err := database.Pool.Exec(context.Background(), `update comments set reply_to = $2 where id = $1`, comments[index], comments[index-1]); err != nil {
				t.Fatalf("chain link %d: %v", index, err)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		settleOutboxWithin(t, ctx, deps, issue, fmt.Sprintf("a chain of %d replies", replies))
		return comments, reply
	}
	sessionsOf := func(from, to int) []string {
		sessions := []string{}
		for index := from; index <= to; index++ {
			sessions = append(sessions, fmt.Sprintf("%s:s-%02d", graphstore.KindSession, index))
		}
		return sessions
	}
	// The sessions alice's reply reached: the walk's recipients, and no person, whom the thread's
	// own rule reaches apart from the walk.
	sessionsReached := func(comment string) []string {
		t.Helper()
		sessions := []string{}
		for _, reached := range meInboxDeliveredTo(t, database, graphstore.KindComment, comment) {
			if strings.HasPrefix(reached, graphstore.KindSession+":") {
				sessions = append(sessions, reached)
			}
		}
		return sessions
	}
	const cutLine = "the comment thread walk stopped at its depth bound"

	atBound, whole := chain(depth)
	reached := sessionsReached(whole)
	if !slices.Equal(reached, sessionsOf(0, depth)) {
		t.Errorf("alice's reply under a chain of %d replies reached %v, want every one of its %d authors", depth, reached, depth+1)
	}
	if lines := log.Lines(cutLine); strings.Contains(lines, atBound[0]) {
		t.Errorf("the walk of a chain at its bound reported a cut: %s", lines)
	}

	longer, cutReply := chain(depth + 1)
	reached = sessionsReached(cutReply)
	if !slices.Equal(reached, sessionsOf(0, depth)) {
		t.Errorf("alice's reply under a chain of %d replies reached %v, want the authors down to the bound and not s-%02d past it",
			depth+1, reached, depth+1)
	}
	if lines := log.Lines(cutLine); !strings.Contains(lines, longer[0]) || !strings.Contains(lines, "depth="+strconv.Itoa(depth)) {
		t.Errorf("the walk of a chain of %d replies did not say it cut the chain under %s; the walk logged %q", depth+1, longer[0], lines)
	}
}

// A narrowing never leaves in the answer an entry it cannot judge: a project narrows every entry
// by its node's own project, and an issue keeps the page's rows of that issue alone, since no
// attribute every node carries says which issue it belongs to.
func TestTheOneReadsNarrowingsKeepOnlyWhatTheyCanJudge(t *testing.T) {
	// Not parallel: it reads GET /api/v1/me/inbox for what its writes through the routes stated,
	// and that read answers only what stands at or below the event horizon (sinceHorizon), which a
	// test beside it holding a transaction id on the cluster (holdClusterTransactionID) holds back.
	handler, database := newTestHandlerWithStore(t)
	here := createTestIssue(t, handler, "NARA", "The issue the read is narrowed to")
	there := createTestIssue(t, handler, "NARB", "Another issue")
	thread := createThreadComment(t, handler, here, map[string]any{"body": "Alice starts a thread here."}, "alice")
	reply := createThreadComment(t, handler, here, map[string]any{"body": "Bob answers it.", "reply_to": thread.ID}, "bob")
	elsewhere := createThreadComment(t, handler, there, map[string]any{"body": "Alice writes there, and nobody answers."}, "alice")
	if reached := meInboxDeliveredTo(t, database, graphstore.KindComment, reply.ID); !slices.Contains(reached, graphstore.KindPerson+":alice") {
		t.Fatalf("bob's reply reached %v, want alice", reached)
	}
	walked := readMeInboxAs(t, handler, "alice", nil)
	for _, id := range []string{thread.ID, reply.ID, elsewhere.ID} {
		if _, held := meInboxEntryOfNode(walked, graphstore.KindComment, id); !held {
			t.Fatalf("alice's whole read lacks comment %s: %v", id, meInboxNodes(walked))
		}
	}
	owned := readMeInboxAs(t, handler, "alice", url.Values{"issue": {here}})
	if nodes := meInboxNodes(owned); !slices.Equal(nodes, []string{graphstore.KindComment + ":" + thread.ID}) {
		t.Errorf("?issue=%s answers %v, want the thread row of that issue alone: no comment of another issue, and no entry the page draws no row for", here, nodes)
	}
	if entry, held := meInboxEntryOfNode(owned, graphstore.KindComment, thread.ID); held && entry.Thread == nil {
		t.Errorf("?issue=%s answers the thread without its row", here)
	}
	inThere := readMeInboxAs(t, handler, "alice", url.Values{"project": {"NARB"}})
	if nodes := meInboxNodes(inThere); !slices.Equal(nodes, []string{graphstore.KindComment + ":" + elsewhere.ID}) {
		t.Errorf("?project=NARB answers %v, want alice's comment of that project alone", nodes)
	}
}

// The handler holds its query to meInboxQuery, the struct the document reads the route's keys
// from: the two lists are one list.
func TestTheOneReadsDeclaredKeysAreItsParamsStruct(t *testing.T) {
	t.Parallel()
	structure := reflect.TypeOf(meInboxQuery{})
	declared := make([]string, 0, structure.NumField())
	for index := range structure.NumField() {
		declared = append(declared, structure.Field(index).Tag.Get("query"))
	}
	if !slices.Equal(declared, meInboxQueryKeys) {
		t.Fatalf("meInboxQuery declares %v and the handler takes %v", declared, meInboxQueryKeys)
	}
}

// The read answers for the caller's own person and nobody else (R1): a key that would select
// another person or a session - login, person, assignee, session - is refused by its name with 400
// INVALID_QUERY, as is every other key the route does not declare, alone or beside a key it
// declares, with a value or with none, and the refusal carries no entry. A read under each key the
// route declares answers the caller as its person, and no entry of another person's.
func TestTheOneReadAnswersForItsCallerAlone(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	seed := meInboxSeed{t: t, database: database}
	writer := model.Actor{Kind: "user", ID: "carol"}
	at := func(minute int) time.Time {
		return time.Date(2026, 11, 17, 11, 0, 0, 0, time.UTC).Add(time.Duration(minute) * time.Minute)
	}
	mine, theirs := uuid.NewString(), uuid.NewString()
	seed.node(graphstore.KindComment, mine, graphstore.VisibilityProject, writer, at(0))
	seed.node(graphstore.KindComment, theirs, graphstore.VisibilityProject, writer, at(1))
	seed.toPerson(
		meInboxTuple(graphstore.KindComment, mine, meInboxDelivered, graphstore.KindPerson, "alice", at(2)),
		meInboxTuple(graphstore.KindComment, theirs, meInboxDelivered, graphstore.KindPerson, "bob", at(3)),
	)
	holds := func(answer meInboxResponse, id string) bool {
		_, held := meInboxEntryOfNode(answer, graphstore.KindComment, id)
		return held
	}
	// The control: each comment is an entry of its own person's read, so a read below that holds
	// no entry of bob's is a reading of alice's alone, and not of a seed no read reaches.
	if answer := readMeInboxAs(t, handler, "bob", nil); answer.Person != "bob" || !holds(answer, theirs) || holds(answer, mine) {
		t.Fatalf("bob's read = person %q, entries %v; want bob, with his comment and not alice's", answer.Person, meInboxNodes(answer))
	}
	if answer := readMeInboxAs(t, handler, "alice", nil); answer.Person != "alice" || !holds(answer, mine) || holds(answer, theirs) {
		t.Fatalf("alice's read = person %q, entries %v; want alice, with her comment and not bob's", answer.Person, meInboxNodes(answer))
	}

	t.Run("a key that selects a person or a session, or one the route does not declare, is refused by its name", func(t *testing.T) {
		refused := []string{
			// The identity selectors, each of which would name somebody other than the caller.
			"login", "person", "assignee", "session",
			// Keys the route does not declare: other words for an identity, the words of a
			// traversal, a word nothing reads, and a declared key spelled in another case.
			"user", "actor", "owner", "traverse", "depth", "frobnicate", "Limit",
		}
		for _, key := range refused {
			for _, query := range []url.Values{{key: {"bob"}}, {"limit": {"10"}, key: {""}}} {
				response := meInboxRequestAs(t, handler, context.Background(), "alice", query)
				var refusal struct{ Code, Error string }
				if response.Code != http.StatusBadRequest || json.Unmarshal(response.Body.Bytes(), &refusal) != nil ||
					refusal.Code != "INVALID_QUERY" || !strings.HasPrefix(refusal.Error, key+" is not a query parameter of ") {
					t.Errorf("alice's read with ?%s answers %d %s, want 400 INVALID_QUERY naming %s",
						query.Encode(), response.Code, response.Body.String(), key)
				}
				if body := response.Body.String(); strings.Contains(body, mine) || strings.Contains(body, theirs) {
					t.Errorf("the answer to alice's read with ?%s carries an entry, where a refusal carries none: %s", query.Encode(), body)
				}
			}
		}
	})

	t.Run("a read under each key the route declares answers the caller and no entry of another person's", func(t *testing.T) {
		values := map[string]string{
			"limit":  "50",
			"offset": "0",
			"kind":   meInboxDelivered,
			"state":  myRepliesStateInbox,
			// A cursor that names bob's comment, which selects nothing of his.
			"cursor":   meInboxCursor{moment: at(60), kind: graphstore.KindComment, id: theirs}.String(),
			"issue":    "ACME-1",
			"artifact": uuid.NewString(),
			"project":  "ACME",
		}
		for _, key := range meInboxQueryKeys {
			value, sent := values[key]
			if !sent {
				t.Errorf("the route declares %s and this test sends no value of it", key)
				continue
			}
			answer := readMeInboxAs(t, handler, "alice", url.Values{key: {value}})
			if answer.Person != "alice" || holds(answer, theirs) {
				t.Errorf("alice's read under ?%s=%s = person %q, entries %v; want alice, and no entry of bob's",
					key, value, answer.Person, meInboxNodes(answer))
			}
		}
	})
}

// A page that asks for the display trees (Dispatch-Trees: 1) is given them on every row it draws,
// read in the same statement where the table holds them and read for display where it does not:
// the ask's question and its thread's replies, and an agent row's conversation and its subject
// line, which no table holds and which is read where it stands (shapeBuilt). The second read is
// the first one's texts again, by then stored back.
func TestTheOneReadCarriesTheDisplayTreesThePageAsksFor(t *testing.T) {
	// Not parallel: it reads GET /api/v1/me/inbox for what its writes through the routes stated,
	// and that read answers only what stands at or below the event horizon (sinceHorizon), which a
	// test beside it holding a transaction id on the cluster (holdClusterTransactionID) holds back.
	handler, _, root, reply, _ := directConversationFrom(t, "alice")
	decodeBody[model.Message](t, reply("An answer with **weight**."))
	issue := createTestIssue(t, handler, "TREES", "Trees on every row")
	ask := createPersonAsk(t, handler, issue, "bob", "Is this **bold**?")
	createThreadComment(t, handler, issue, map[string]any{"body": "A reply in _the_ thread.", "ask_id": ask.ID}, "bob")
	for _, pass := range []string{"first", "second"} {
		answer := decodeMeInbox(t, treesRequest(t, handler, "/api/v1/me/inbox?state=inbox", true))
		askEntry, heldAsk := meInboxEntryOfNode(answer, graphstore.KindAsk, ask.ID)
		agentEntry, heldAgent := meInboxEntryOfNode(answer, graphstore.KindMessage, root.ID)
		if !heldAsk || askEntry.Ask == nil || !heldAgent || agentEntry.Agent == nil {
			t.Fatalf("the %s read holds %v, want the ask row and the agent row", pass, meInboxNodes(answer))
		}
		if len(askEntry.Ask.QuestionTree) == 0 || len(askEntry.Ask.Thread.Replies) != 1 || len(askEntry.Ask.Thread.Replies[0].BodyTree) == 0 {
			t.Errorf("the %s read's ask row carries question tree %q and replies %+v, want both trees", pass, askEntry.Ask.QuestionTree, askEntry.Ask.Thread.Replies)
		}
		conversation := agentEntry.Agent.Conversation
		if len(conversation.Message.BodyTree) == 0 || len(conversation.Replies) != 1 || len(conversation.Replies[0].BodyTree) == 0 {
			t.Errorf("the %s read's agent row carries %+v, want the root's and the reply's trees", pass, conversation)
		}
		if len(agentEntry.Agent.SubjectTree) == 0 {
			t.Errorf("the %s read's agent row carries subject %q with no tree, want its line read as GET /api/v1/me/replies reads it",
				pass, agentEntry.Agent.Subject)
		}
	}
}
