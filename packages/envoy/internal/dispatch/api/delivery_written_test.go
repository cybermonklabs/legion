package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/logtest"
)

// TestDeliveryToAPersonIsWrittenByTheWriteThatDelivers is c-delivery-written (R2): a delivery to a
// person is the delivered_to tuple the write that delivers appends in its own transaction, and no
// read computes it again.
//
// The gap it closes comes first: the outbox records the topics a comment's event went to
// (events.published_destinations), which name agents and never a person, so a person's deliveries
// drawn from there would be none and a census over them would read zero over nothing. Then each of
// R2's four arms - the thread's other participants, the person an ask waits on, the person who
// started a direct conversation a session replied under, and a person a mention points at - is
// written through its real route, and the tuples it appended are held to recipients written out by
// hand, never computed: a role mention that forwards to a session, whose two mentions the case
// names, and a thread a person takes part in by a mention alone, whose next reply reaches her and
// nobody else, among them. The case that writes every arm but the direct conversation's then
// compares the stored record with what the delivery code's own attempts say, each side named where
// they differ, and shows the deliveries are the record whatever read draws them: with every
// user_threads row deleted, the one read answers the rows it answered before. The direct
// conversation's arm is a case of its own after it. Last, the record stands as written: an ask's
// delivery stands past its issue's close, a change of assignee appends the new person's delivery
// and the old one's retraction under its one event, a change whose append fails - at its first
// append, or after it with the first appended - leaves neither and the assignee as it was, and no
// tuple is ever updated.
func TestDeliveryToAPersonIsWrittenByTheWriteThatDelivers(t *testing.T) {
	t.Run("the outbox's destinations name no person", deliveryGapNamesNoPerson)
	t.Run("each arm appends the deliveries its write reaches, as written out by hand", deliveryArmsByHand)
	t.Run("a session's reply under a direct message reaches the person who started it", deliveryDirectReplyArm)
	t.Run("an ask's delivery stands as written, and a change of assignee moves it in one transaction or not at all", deliveryAssigneeRecord)
}

func deliveryGapNamesNoPerson(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "GAPS", "What the outbox records", "before")
	root := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Is the copy final?"}, "bob")
	reply := createThreadComment(t, handler, issue.Key, map[string]any{"body": "It is.", "reply_to": root.ID}, "alice")
	runOutboxUntilSettled(t, deps, &recordingOutboxPublisher{}, issue.Key, reply.ID)
	destinations := publishedDestinationsOf(t, database, "comment.created", reply.ID)
	if len(destinations) == 0 {
		t.Fatalf("the outbox recorded no destination of comment.created %s, so the case shows nothing", reply.ID)
	}
	for _, destination := range destinations {
		if strings.Contains(destination, "bob") || strings.HasPrefix(destination, graphstore.KindPerson+":") {
			t.Errorf("comment.created's destinations %v name a person: they are agent topics alone", destinations)
		}
	}
	// bob is reached all the same, by the tuple the comment's own write appended.
	requireRelationTuples(t, database, graphstore.TupleDeliveredTo, "comment:"+reply.ID,
		fmt.Sprintf("comment:%s > person:bob @%d", reply.ID, writeEventOf(t, database, "comment.created", reply.ID)))
}

func deliveryArmsByHand(t *testing.T) {
	listener, _ := newMentionListener(t)
	handler, database, _ := newTestServer(t, testServerOptions{envoyURL: listener.URL, envoyTimeout: time.Second})
	at := func(eventType, id string) int64 { return writeEventOf(t, database, eventType, id) }
	issue := createInteractionIssue(t, handler, "ARMS", "Every arm", "before")

	// A comment in a thread two other people wrote in reaches each of them, and not its author.
	root := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Is the copy final?"}, "bob")
	createThreadComment(t, handler, issue.Key, map[string]any{"body": "Nearly.", "reply_to": root.ID}, "carol")
	third := createThreadComment(t, handler, issue.Key, map[string]any{"body": "It is now.", "reply_to": root.ID}, "alice")
	thirdAt := at("comment.created", third.ID)
	requireRelationTuples(t, database, graphstore.TupleDeliveredTo, "comment:"+third.ID,
		fmt.Sprintf("comment:%s > person:bob @%d", third.ID, thirdAt),
		fmt.Sprintf("comment:%s > person:carol @%d", third.ID, thirdAt))

	// A comment that mentions a person reaches that person, who takes part in the thread from it
	// without writing in it; the thread's next reply, by its author, reaches her alone.
	mentioning := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Over to dispatch://person/" + mentionedPerson + " for the copy."}, "alice")
	mentioned := at("comment.created", mentioning.ID)
	requireRelationTuples(t, database, graphstore.TupleMentions, "comment:"+mentioning.ID,
		fmt.Sprintf("comment:%s > person:%s @%d", mentioning.ID, mentionedPerson, mentioned))
	requireRelationTuples(t, database, graphstore.TupleDeliveredTo, "comment:"+mentioning.ID,
		fmt.Sprintf("comment:%s > person:%s @%d", mentioning.ID, mentionedPerson, mentioned))
	later := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Thanks.", "reply_to": mentioning.ID}, "alice")
	replied := at("comment.created", later.ID)
	requireRelationTuples(t, database, graphstore.TupleParticipatesIn, "comment:"+mentioning.ID,
		fmt.Sprintf("person:alice > comment:%s @%d", mentioning.ID, mentioned),
		fmt.Sprintf("person:%s > comment:%s @%d", mentionedPerson, mentioning.ID, mentioned),
		fmt.Sprintf("person:alice > comment:%s @%d", mentioning.ID, replied))
	requireRelationTuples(t, database, graphstore.TupleDeliveredTo, "comment:"+later.ID,
		fmt.Sprintf("comment:%s > person:%s @%d", later.ID, mentionedPerson, replied))

	// A mention of a role whose holder is a session mentions both, and delivers to no person.
	forwarded := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Can you review the copy?", "mentions": []map[string]any{{"target": "role:reviewer"}},
	}))
	forwardedAt := at("comment.created", forwarded.ID)
	requireRelationTuples(t, database, graphstore.TupleMentions, "comment:"+forwarded.ID,
		fmt.Sprintf("comment:%s > role:reviewer @%d", forwarded.ID, forwardedAt),
		fmt.Sprintf("comment:%s > session:s1 @%d", forwarded.ID, forwardedAt))
	requireRelationTuples(t, database, graphstore.TupleDeliveredTo, "comment:"+forwarded.ID)

	// An ask on an issue assigned to a person reaches that person, at the ask's own write.
	assigned := createInteractionIssue(t, handler, "ARMT", "An assigned issue", "before")
	assignIssue(t, handler, assigned.Key, "bob")
	ask := createIssueAsk(t, handler, assigned.Key, "Is the copy final?")
	requireRelationTuples(t, database, graphstore.TupleDeliveredTo, "ask:"+ask,
		fmt.Sprintf("ask:%s > person:bob @%d", ask, at("ask.opened", ask)))

	// The delivery code's own answer for the role mention, held apart from the hand-written ones:
	// the session its attempt went to is the session its second mention names.
	stated, err := readStrings(context.Background(), database.Pool, `
		select to_id from graph_tuples
		where from_kind = 'comment' and from_id = $1 and kind = 'mentions' and to_kind = 'session' order by 1
	`, forwarded.ID)
	if err != nil {
		t.Fatalf("read the sessions the role mention names: %v", err)
	}
	attempted, err := readStrings(context.Background(), database.Pool, `
		select session_id from comment_deliveries where comment_id = $1::uuid and session_id is not null order by 1
	`, forwarded.ID)
	if err != nil {
		t.Fatalf("read the sessions the role mention's attempts went to: %v", err)
	}
	if !slices.Equal(stated, attempted) {
		t.Errorf("the role mention's tuples name sessions %v, and the delivery code's attempts went to %v", stated, attempted)
	}

	// No read-time arm computes a delivery: with every user_threads row gone, the one read answers
	// the rows it answered before, from the tuples.
	logins := []string{"bob", "carol", mentionedPerson}
	before := map[string][]string{}
	for _, login := range logins {
		before[login] = meInboxNodes(readMeInboxAs(t, handler, login, nil))
		if len(before[login]) == 0 {
			t.Fatalf("%s's read answers nothing before user_threads is emptied, so the comparison after it shows nothing", login)
		}
	}
	if _, err := database.Pool.Exec(context.Background(), `delete from user_threads`); err != nil {
		t.Fatalf("delete every user_threads row: %v", err)
	}
	for _, login := range logins {
		if after := meInboxNodes(readMeInboxAs(t, handler, login, nil)); !slices.Equal(before[login], after) {
			t.Errorf("with user_threads empty %s's read answers %v, want what it answered before: %v", login, after, before[login])
		}
	}
}

func deliveryDirectReplyArm(t *testing.T) {
	live := true
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	t.Cleanup(listener.Close)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	created := postAgentMessage(t, handler, "s1", map[string]any{"body": "Can you look at the deploy?", "delivery": "steer"}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("send alice's direct message: status=%d body=%s", created.Code, created.Body.String())
	}
	root := decodeBody[model.Message](t, created)
	replied := replyTo(t, handler, root.ID, "Looking.")
	if replied.Code != http.StatusCreated {
		t.Fatalf("reply to alice's direct message: status=%d body=%s", replied.Code, replied.Body.String())
	}
	answer := decodeBody[model.Message](t, replied)
	requireRelationTuples(t, database, graphstore.TupleDeliveredTo, "message:"+answer.ID,
		fmt.Sprintf("message:%s > person:alice @%d", answer.ID, writeEventOf(t, database, "message.answered", answer.ID)))
}

// The change of assignee runs on an open issue: a closed issue's asks are withdrawn, and a change
// of its assignee moves none of them until its reopen (TestTheAssigneeArmFollowsWhoAnAskWaitsOn).
func deliveryAssigneeRecord(t *testing.T) {
	log := &logtest.Buffer{}
	logtest.SetDefault(t, slog.NewTextHandler(log, nil))
	handler, database := newTestHandlerWithStore(t)
	ctx := context.Background()
	seedPeople(t, database, "carol", "dana")

	// The close computes nothing again: the delivery the ask's create stored stands.
	closed := createInteractionIssue(t, handler, "RECC", "A closed issue", "A spec")
	assignIssue(t, handler, closed.Key, "bob")
	left := createIssueAsk(t, handler, closed.Key, "Is it shipped?")
	leftOpened := writeEventOf(t, database, "ask.opened", left)
	closeIssue(t, handler, closed.Key)
	requireRelationTuples(t, database, graphstore.TupleDeliveredTo, "ask:"+left,
		fmt.Sprintf("ask:%s > person:bob @%d", left, leftOpened))

	issue := createInteractionIssue(t, handler, "RECM", "A moved issue", "A spec")
	assignIssue(t, handler, issue.Key, "bob")
	ask := createIssueAsk(t, handler, issue.Key, "Is it ready?")
	opened := writeEventOf(t, database, "ask.opened", ask)
	thread := "ask:" + ask
	assignIssue(t, handler, issue.Key, "dana")
	moved := latestEventOf(t, database, "issue.updated")
	requireRelationTuples(t, database, graphstore.TupleDeliveredTo, thread,
		fmt.Sprintf("%s > person:bob @%d", thread, opened),
		fmt.Sprintf("%s > person:bob @%d retracted", thread, moved),
		fmt.Sprintf("%s > person:dana @%d", thread, moved))

	// The next change fails at its append: neither its delivery nor its retraction is written, the
	// assignee stays dana, and bob's first delivery stands as it was appended.
	refuseTuplesFrom(t, database, graphstore.KindAsk, ask, graphstore.TupleDeliveredTo)
	response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{"assignee": "carol"}, "alice")
	if response.Code < http.StatusInternalServerError {
		t.Fatalf("the change of assignee whose append failed answered %d %s, want a 5xx", response.Code, response.Body.String())
	}
	// assigneeOf is the login key's issue is assigned to, "" for nobody.
	assigneeOf := func(key string) string {
		t.Helper()
		var assignee string
		if err := database.Pool.QueryRow(ctx, `select coalesce(assignee, '') from issues where key = $1`, key).Scan(&assignee); err != nil {
			t.Fatalf("read %s's assignee: %v", key, err)
		}
		return assignee
	}
	if assignee := assigneeOf(issue.Key); assignee != "dana" {
		t.Errorf("after the failed change %s is assigned to %q, want dana still", issue.Key, assignee)
	}
	requireRelationTuples(t, database, graphstore.TupleDeliveredTo, thread,
		fmt.Sprintf("%s > person:bob @%d", thread, opened),
		fmt.Sprintf("%s > person:bob @%d retracted", thread, moved),
		fmt.Sprintf("%s > person:dana @%d", thread, moved))

	// A failure after the first of the move's appends leaves neither. On an ask of its own, the
	// test refuses the new assignee's delivery alone, and only once the old assignee's retraction
	// stands earlier in the same statement (refuseSecondAppend): the move's one statement appends
	// each retraction before each new tuple (inboxwrite.QueueMoveWaiting), so it fails at its
	// second append with its first appended. The answer is a 5xx, the refusal is the one that
	// ended it, the assignee stays dana, and every tuple of the ask stands as it did before.
	later := createInteractionIssue(t, handler, "RECS", "A move that fails at its second append", "A spec")
	assignIssue(t, handler, later.Key, "bob")
	laterAsk := createIssueAsk(t, handler, later.Key, "Is it final?")
	laterOpened := writeEventOf(t, database, "ask.opened", laterAsk)
	laterThread := "ask:" + laterAsk
	assignIssue(t, handler, later.Key, "dana")
	laterMoved := latestEventOf(t, database, "issue.updated")
	standing := []string{
		fmt.Sprintf("%s > person:bob @%d", laterThread, laterOpened),
		fmt.Sprintf("%s > person:bob @%d retracted", laterThread, laterMoved),
		fmt.Sprintf("%s > person:dana @%d", laterThread, laterMoved),
	}
	requireRelationTuples(t, database, graphstore.TupleDeliveredTo, laterThread, standing...)
	before := askTuplesOf(t, database, laterAsk)
	refuseSecondAppend(t, database, laterAsk)
	response = dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+later.Key, map[string]any{"assignee": "carol"}, "alice")
	if response.Code < http.StatusInternalServerError {
		t.Fatalf("the change of assignee whose second append was refused answered %d %s, want a 5xx: the refusal fires only "+
			"once the old assignee's retraction stands, so a change that succeeds appended the delivery before it, or no retraction",
			response.Code, response.Body.String())
	}
	if refused := log.Lines(secondAppendRefused); refused == "" {
		t.Errorf("the change of assignee failed, and not at the refusal of its second append; the server logged:\n%s", log.String())
	}
	if assignee := assigneeOf(later.Key); assignee != "dana" {
		t.Errorf("after the change whose second append failed %s is assigned to %q, want dana still", later.Key, assignee)
	}
	requireRelationTuples(t, database, graphstore.TupleDeliveredTo, laterThread, standing...)
	if after := askTuplesOf(t, database, laterAsk); !slices.Equal(after, before) {
		t.Errorf("after the change whose second append failed the ask's tuples are\n  %s\nwant them as they stood before it\n  %s",
			strings.Join(after, "\n  "), strings.Join(before, "\n  "))
	}
}

// secondAppendRefused is what refuseSecondAppend's trigger raises, and so what the server logs of
// the write it ends.
const secondAppendRefused = "the test refuses the second append"

// refuseSecondAppend makes the append of an asserted delivered_to from ask fail where a retraction
// of a delivered_to from that ask stands under the same event, for the rest of t: a trigger the
// test installs and its cleanup drops. A row trigger's own read sees the rows its statement
// appended before the row it fires for, so a move, which appends the old person's retraction and
// then the new person's delivery in one statement, fails at that second append with its first
// appended; a delivery appended before any retraction passes.
func refuseSecondAppend(t *testing.T, database *store.Store, ask string) {
	t.Helper()
	refuseTupleTrigger(t, database,
		`new.from_kind = `+quoteLiteral(graphstore.KindAsk)+` and new.from_id = `+quoteLiteral(ask)+`
		  and new.kind = `+quoteLiteral(graphstore.TupleDeliveredTo)+` and new.asserted`, `
			if exists (
				select 1 from graph_tuples t
				where t.from_kind = new.from_kind and t.from_id = new.from_id and t.kind = new.kind
				  and not t.asserted and t.source_event = new.source_event
			) then
				raise exception '`+secondAppendRefused+`, % to %/% under event %', new.kind, new.to_kind, new.to_id, new.source_event;
			end if;
			return new;`)
}

// askTuplesOf is every tuple with ask at either end, as `from kind to @event`, a retraction marked,
// sorted as Go orders bytes.
func askTuplesOf(t *testing.T, database *store.Store, ask string) []string {
	t.Helper()
	held, err := readStrings(context.Background(), database.Pool, `
		select from_kind || ':' || from_id || ' ' || kind || ' ' || to_kind || ':' || to_id || ' @' || source_event::text
		       || case when asserted then '' else ' retracted' end
		from graph_tuples
		where (from_kind = $1 and from_id = $2) or (to_kind = $1 and to_id = $2)
	`, graphstore.KindAsk, ask)
	if err != nil {
		t.Fatalf("read the tuples of ask %s: %v", ask, err)
	}
	slices.Sort(held)
	return held
}
