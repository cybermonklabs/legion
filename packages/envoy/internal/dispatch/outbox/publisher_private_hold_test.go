package outbox

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The private-delivery hold (Deps.PrivateHubsReady, Deps.holdsPrivate): while the hubs' proof
// answers false, the batch's read chooses no owner of a private project (readBatchSQL,
// privateOwnerSQL), the fallback pass claims no copy of such an owner's event
// (claimFallbackCopies), and the retry timer looks again every privateHoldWait while one waits
// (Outbox.nextRetry); once it answers true, the next scan publishes them; a nil func holds
// nothing.
//
// Each row seeds PRIV, a private project, and PUB, a public one, each with issues routed nowhere,
// so an issue's event has its topic family alone for a destination; PRIV's events are written
// first, so a hold that stalled the owners behind it would hold PUB's back too.

// seedHoldProjects writes PRIV private and PUB public, PRIV with privateIssues issues PRIV-1 on
// and PUB with its one issue, PUB-1.
func seedHoldProjects(t *testing.T, database *store.Store, privateIssues int) {
	t.Helper()
	ctx := context.Background()
	for _, project := range []struct {
		key, visibility string
		issues          int
	}{{"PRIV", "private", privateIssues}, {"PUB", "public", 1}} {
		if _, err := database.Pool.Exec(ctx, `
			insert into projects (key, name, default_assignee, owner, visibility) values ($1, $1, 'alice', 'alice', $2)
		`, project.key, project.visibility); err != nil {
			t.Fatalf("create project %s: %v", project.key, err)
		}
		if _, err := database.Pool.Exec(ctx, `
			insert into issues (key, project_key, number, title, route, created_by, rank)
			select $1 || '-' || n, $1, n, 'Issue ' || n || ' of ' || $1, null, '{"kind":"user","id":"alice"}', 'U'
			from generate_series(1, $2::int) n
		`, project.key, project.issues); err != nil {
			t.Fatalf("create the issues of %s: %v", project.key, err)
		}
	}
}

// appendHoldMessage appends a message.created event on issue, as a message to the issue writes it.
func appendHoldMessage(t *testing.T, database *store.Store, broker *events.Broker, issue string) model.Event {
	t.Helper()
	return appendEvent(t, database, broker, model.Event{
		IssueKey: new(issue), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.Message{ID: uuid.NewString(), IssueKey: new(issue), Body: "A note on " + issue},
	})
}

// topicsNaming is every topic publisher was sent that holds fragment.
func topicsNaming(publisher *recordingPublisher, fragment string) []string {
	var named []string
	for _, item := range publisher.all() {
		if strings.Contains(item.Topic, fragment) {
			named = append(named, item.Topic)
		}
	}
	return named
}

// untouched reports whether event id's row is as its write left it: no attempt counted, no
// error, no wait and no destination reached.
func untouched(t *testing.T, database *store.Store, id int64) (bool, string) {
	t.Helper()
	var attempts, reached int
	var failure *string
	var waits bool
	if err := database.Pool.QueryRow(context.Background(), `
		select attempt_count, publish_error, next_attempt_at is not null, cardinality(published_destinations)
		from events where id = $1`, id,
	).Scan(&attempts, &failure, &waits, &reached); err != nil {
		t.Fatalf("read event %d: %v", id, err)
	}
	state := fmt.Sprintf("%d attempts, error %v, a wait %t, %d destinations reached", attempts, nullable(failure), waits, reached)
	return attempts == 0 && failure == nil && !waits && reached == 0, state
}

// TestTheOutboxHoldsAPrivateProjectsEventWhileItsHubsAreUnproven: with PrivateHubsReady answering
// false, one batch publishes PUB's event and leaves PRIV's unpublished, sent to no topic and as
// its write left it, unread; once the func answers true, the next batch publishes it to PRIV-1's
// topic.
//
// Control: a read that ignores the hold (readRows binding false for $3) publishes PRIV's event in
// the first batch, and the row fails naming PRIV-1's topic.
func TestTheOutboxHoldsAPrivateProjectsEventWhileItsHubsAreUnproven(t *testing.T) {
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedHoldProjects(t, database, 1)
	private := appendHoldMessage(t, database, broker, "PRIV-1")
	public := appendHoldMessage(t, database, broker, "PUB-1")

	var proven atomic.Bool
	publisher := &recordingPublisher{}
	deps := Deps{Store: database, Publisher: publisher, Broker: broker, PrivateHubsReady: proven.Load}
	holder := holding(t, deps).instance
	if _, err := scanBatch(context.Background(), deps, unfenced{}, holder); err != nil {
		t.Fatalf("scan the batch while the hubs are unproven: %v", err)
	}
	if sent := topicsNaming(publisher, ".PRIV-1."); len(sent) != 0 || publishedAt(t, database, private.ID) != nil {
		t.Fatalf("while the hubs are unproven PRIV's event was sent to %v and published at %v, want it held: sent nowhere, unpublished",
			sent, publishedAt(t, database, private.ID))
	}
	if left, state := untouched(t, database, private.ID); !left {
		t.Fatalf("PRIV's held event has %s, want it as its write left it: the hold reads and writes none of it", state)
	}
	if sent := topicsNaming(publisher, ".PUB-1."); len(sent) != 1 || publishedAt(t, database, public.ID) == nil {
		t.Fatalf("while the hubs are unproven PUB's event was sent to %v and published at %v, want it published as before",
			sent, publishedAt(t, database, public.ID))
	}

	proven.Store(true)
	if _, err := scanBatch(context.Background(), deps, unfenced{}, holder); err != nil {
		t.Fatalf("scan the batch once the hubs are proven: %v", err)
	}
	want := "notifications.dispatch.issue.PRIV-1.message.created"
	if sent := topicsNaming(publisher, ".PRIV-1."); len(sent) != 1 || sent[0] != want || publishedAt(t, database, private.ID) == nil {
		t.Fatalf("once the hubs are proven PRIV's event was sent to %v and published at %v, want it published to %s",
			sent, publishedAt(t, database, private.ID), want)
	}
}

// TestAPublicOwnersEventPublishesInTheFirstBatchBesideAHundredHeldPrivateOwners: 100 owners of
// PRIV each have an event due, written before PUB-1's, and PrivateHubsReady answers false. The
// first batch publishes PUB-1's event: a held owner takes no place among the batchSize owners a
// batch chooses, so no number of them delays a public owner. No PRIV event is sent.
//
// Control: a hold made after the read (the per-row hold, which reads the owners and then holds
// them) fills the first batch with the 100 PRIV owners, and the row fails naming PUB-1's event.
func TestAPublicOwnersEventPublishesInTheFirstBatchBesideAHundredHeldPrivateOwners(t *testing.T) {
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedHoldProjects(t, database, batchSize)
	for n := 1; n <= batchSize; n++ {
		appendHoldMessage(t, database, broker, fmt.Sprintf("PRIV-%d", n))
	}
	public := appendHoldMessage(t, database, broker, "PUB-1")

	publisher := &recordingPublisher{}
	deps := Deps{Store: database, Publisher: publisher, Broker: broker, PrivateHubsReady: func() bool { return false }}
	if _, err := scanBatch(context.Background(), deps, unfenced{}, holding(t, deps).instance); err != nil {
		t.Fatalf("scan the first batch: %v", err)
	}
	if publishedAt(t, database, public.ID) == nil {
		t.Fatalf("the first batch left PUB-1's event unpublished beside %d held PRIV owners, sending %d envelopes, want it published",
			batchSize, len(publisher.all()))
	}
	if sent := topicsNaming(publisher, ".PRIV-"); len(sent) != 0 {
		t.Fatalf("while the hubs are unproven the batch sent %d PRIV topics, the first %s, want none", len(sent), sent[0])
	}
}

// TestAHeldPrivateEventPublishesOnceItsHubsAreProven runs the outbox (Run), its hold's re-poll
// shortened to 100 ms: while PrivateHubsReady answers false PUB's event publishes and PRIV's does
// not, and the outbox asks the func again and again; once it answers true, PRIV's event publishes
// with no other event written to wake the outbox: the retry timer the hold arms (Outbox.nextRetry)
// is the only wake, since the func signals nothing.
//
// Control: nextRetry without its hold arm (nextRetrySQL whatever the hold) arms no timer for an
// event the read leaves out, and the row fails waiting for the outbox to ask again.
func TestAHeldPrivateEventPublishesOnceItsHubsAreProven(t *testing.T) {
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedHoldProjects(t, database, 1)

	var proven atomic.Bool
	var asked atomic.Int64
	publisher := &recordingPublisher{}
	deps := Deps{
		Store: database, Publisher: publisher, Broker: broker, Listen: database.Pool.Listener,
		PrivateHubsReady: func() bool { asked.Add(1); return proven.Load() },
		privateHoldWait:  100 * time.Millisecond,
	}
	stop := runDeps(t, deps, 5*time.Second)
	defer stop()

	private := appendHoldMessage(t, database, broker, "PRIV-1")
	public := appendHoldMessage(t, database, broker, "PUB-1")
	waitFor(t, 10*time.Second, "PUB's event published while the hubs are unproven", func() bool {
		return publishedAt(t, database, public.ID) != nil
	})
	before := asked.Load()
	waitFor(t, 10*time.Second, "the outbox asking the hubs' proof again on its re-poll", func() bool {
		return asked.Load() >= before+10
	})
	if sent := topicsNaming(publisher, ".PRIV-1."); len(sent) != 0 || publishedAt(t, database, private.ID) != nil {
		t.Fatalf("while the hubs are unproven PRIV's event was sent to %v, want it held", sent)
	}

	proven.Store(true)
	waitFor(t, 10*time.Second, "PRIV's event published once the hubs are proven", func() bool {
		return publishedAt(t, database, private.ID) != nil
	})
	if sent := topicsNaming(publisher, ".PRIV-1."); len(sent) != 1 {
		t.Fatalf("once the hubs are proven PRIV's event was sent to %v, want its one topic", sent)
	}
}

// seedHoldFallbackCopies seeds a comment on PRIV-1 and one on PUB-1, each comment.created event
// already published, and one copy of each held as a comment_fallback_copies row on a session of
// its own, unclaimed (the state a row the batch just held back is in), as seedFallbackBacklog does.
// PRIV-1's session is one alice's broker token proved (session_owners): alice owns PRIV, so the
// session may be shown PRIV's events, which the fallback pass asks of it (decideFallbackCopy).
// It returns the two comments' ids.
func seedHoldFallbackCopies(t *testing.T, database *store.Store, broker *events.Broker) (private, public string) {
	t.Helper()
	ctx := context.Background()
	alice := model.Actor{Kind: "user", ID: "alice"}
	seed := func(issue, session string) string {
		body := "a comment on " + issue
		commentID := seedComment(t, database, issue, alice, body, nil)
		event := appendEvent(t, database, broker, model.Event{
			IssueKey: new(issue), Type: "comment.created", Actor: alice,
			Payload: model.CommentEventPayload{Comment: model.Comment{ID: commentID, IssueKey: new(issue), Body: body}},
		})
		if _, err := database.Pool.Exec(ctx, `update events set published_at = now() where id = $1`, event.ID); err != nil {
			t.Fatalf("publish the comment's event on %s: %v", issue, err)
		}
		if _, err := database.Pool.Exec(ctx, `
			insert into comment_fallback_copies (comment_id, destination, event_id, session_id) values ($1, $2, $3, $4)
		`, commentID, "notifications.agent."+session, event.ID, session); err != nil {
			t.Fatalf("hold the copy of the comment on %s: %v", issue, err)
		}
		return commentID
	}
	tieSession(t, database, "s-priv", "alice")
	return seed("PRIV-1", "s-priv"), seed("PUB-1", "s-pub")
}

// TestTheFallbackPassHoldsAPrivateCommentsCopyWhileItsHubsAreUnproven: with PrivateHubsReady
// answering false, a fallback pass publishes PUB's held copy and leaves PRIV's unclaimed and
// unsent, as the pass found it; once the func answers true, the next pass publishes PRIV's copy.
//
// Control: a claim that ignores the hold (claimFallbackCopies binding false for $3) publishes
// PRIV's copy in the first pass, and the row fails naming s-priv's subject.
func TestTheFallbackPassHoldsAPrivateCommentsCopyWhileItsHubsAreUnproven(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedHoldProjects(t, database, 1)
	private, public := seedHoldFallbackCopies(t, database, broker)

	var proven atomic.Bool
	publisher := &recordingPublisher{}
	deps := Deps{Store: database, Publisher: publisher, Broker: broker, PrivateHubsReady: proven.Load}
	holder := holding(t, deps).instance
	publishFallbackCopies(ctx, deps, unfenced{}, holder)
	if sent := topicsNaming(publisher, "s-priv"); len(sent) != 0 {
		t.Fatalf("while the hubs are unproven the pass sent PRIV's copy to %v, want it held", sent)
	}
	if row := readHeldCopy(t, database, private, "notifications.agent.s-priv"); row.settled || row.published || row.attempts != 0 || row.due != nil {
		t.Fatalf("PRIV's held copy is settled %v, published %v, with %d attempts and due %v, want it as the pass found it: unclaimed and untouched",
			row.settled, row.published, row.attempts, row.due)
	}
	if row := readHeldCopy(t, database, public, "notifications.agent.s-pub"); !row.settled || !row.published {
		t.Fatalf("while the hubs are unproven PUB's copy is settled %v and published %v, want it published as before", row.settled, row.published)
	}

	proven.Store(true)
	publishFallbackCopies(ctx, deps, unfenced{}, holder)
	if row := readHeldCopy(t, database, private, "notifications.agent.s-priv"); !row.settled || !row.published {
		t.Fatalf("once the hubs are proven PRIV's copy is settled %v and published %v, want it published", row.settled, row.published)
	}
	if sent := topicsNaming(publisher, "s-priv"); len(sent) != 1 {
		t.Fatalf("once the hubs are proven the pass sent PRIV's copy to %v, want s-priv's subject once", sent)
	}
}

// TestTheRetryTimerLooksAgainWhileTheHoldKeepsAPrivateEvent: nextRetry answers as before - the
// earliest back-off, PUB-1's event an hour out - with a nil PrivateHubsReady, with one that answers
// true, and with one that answers false while no private event waits (R9); while the hold keeps
// PRIV-1's unpublished event it answers privateHoldRecheck at the latest, and once that event is
// published, the hour again.
//
// Control: nextRetry without its hold arm answers the hour while PRIV-1's event is held, and the
// row fails naming it.
func TestTheRetryTimerLooksAgainWhileTheHoldKeepsAPrivateEvent(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedHoldProjects(t, database, 1)
	backedOff := appendHoldMessage(t, database, broker, "PUB-1")
	if _, err := database.Pool.Exec(ctx, `update events set next_attempt_at = now() + interval '1 hour', attempt_count = 1 where id = $1`, backedOff.ID); err != nil {
		t.Fatalf("back PUB-1's event off an hour: %v", err)
	}
	unproven := func() bool { return false }
	proven := func() bool { return true }
	retryOf := func(ready func() bool) (time.Duration, bool) {
		return New(Deps{Store: database, Publisher: &recordingPublisher{}, PrivateHubsReady: ready}).nextRetry(ctx, false)
	}
	anHour := func(name string, ready func() bool) {
		t.Helper()
		if wait, ok := retryOf(ready); !ok || wait < 50*time.Minute {
			t.Errorf("%s: the next retry is due in %v (%v), want PUB-1's back-off, about an hour away, as before", name, wait, ok)
		}
	}
	anHour("no private event waiting, a nil func", nil)
	anHour("no private event waiting, a func answering true", proven)
	anHour("no private event waiting, a func answering false", unproven)

	held := appendHoldMessage(t, database, broker, "PRIV-1")
	anHour("PRIV-1's event waiting, a nil func", nil)
	anHour("PRIV-1's event waiting, a func answering true", proven)
	if wait, ok := retryOf(unproven); !ok || wait > privateHoldRecheck {
		t.Errorf("PRIV-1's event held: the next retry is due in %v (%v), want %v at the latest, the hold's re-poll", wait, ok, privateHoldRecheck)
	}

	if _, err := database.Pool.Exec(ctx, `update events set published_at = now() where id = $1`, held.ID); err != nil {
		t.Fatalf("publish PRIV-1's event: %v", err)
	}
	anHour("PRIV-1's event published, a func answering false", unproven)
}

// TestANilPrivateHubsReadyHoldsNothing (R9): with no PrivateHubsReady, as cmd/dispatch runs until a
// hub can prove it, PRIV's event publishes in the first batch beside PUB's and the fallback pass
// publishes PRIV's copy, and every read binds the hold off.
//
// Control: a nil func read as an unproven hub (holdsPrivate answering true for nil) leaves PRIV's
// event unpublished, and the row fails naming it.
func TestANilPrivateHubsReadyHoldsNothing(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedHoldProjects(t, database, 1)
	privateCopy, _ := seedHoldFallbackCopies(t, database, broker)
	private := appendHoldMessage(t, database, broker, "PRIV-1")
	public := appendHoldMessage(t, database, broker, "PUB-1")

	tracer := &statementTracer{}
	traced := tracedStore(t, database, tracer)
	deps := Deps{Store: traced, Publisher: &recordingPublisher{}, Broker: broker}
	holder := holding(t, deps).instance
	if _, err := scanBatch(ctx, deps, unfenced{}, holder); err != nil {
		t.Fatalf("scan the batch: %v", err)
	}
	publishFallbackCopies(ctx, deps, unfenced{}, holder)
	for _, event := range []model.Event{private, public} {
		if publishedAt(t, database, event.ID) == nil {
			t.Fatalf("with no PrivateHubsReady the event of %s is unpublished, want it published as before", *event.IssueKey)
		}
	}
	if row := readHeldCopy(t, database, privateCopy, "notifications.agent.s-priv"); !row.settled || !row.published {
		t.Fatalf("with no PrivateHubsReady PRIV's copy is settled %v and published %v, want it published as before", row.settled, row.published)
	}
	held := tracer.matching("held.visibility = 'private'")
	if len(held) != 2 {
		t.Fatalf("with no PrivateHubsReady %d statements named the hold, want two: the batch's read and the pass's claim", len(held))
	}
	for _, statement := range held {
		if on := statement.args[len(statement.args)-1]; on != false {
			t.Fatalf("with no PrivateHubsReady a statement bound the hold %v, want false: %.80q", on, statement.sql)
		}
	}
}
