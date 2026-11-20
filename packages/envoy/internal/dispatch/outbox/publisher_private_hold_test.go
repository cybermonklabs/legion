package outbox

import (
	"context"
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

// The private-delivery hold (Deps.PrivateHubsReady, heldPrivateOwners): while the hubs' proof
// answers false, no event of a private project's owner leaves, and a public project's events
// publish as before; once it answers true, the held event publishes; a nil func holds nothing.
//
// Each row seeds PRIV, a private project, and PUB, a public one, each with one issue routed
// nowhere, so an issue's event has its topic family alone for a destination; PRIV's event is
// written first, so a hold that also stalled the owners behind it would hold PUB's back too.

// seedHoldProjects writes PRIV private and PUB public, each with its one issue, PRIV-1 and PUB-1.
func seedHoldProjects(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := context.Background()
	for _, project := range []struct{ key, visibility string }{{"PRIV", "private"}, {"PUB", "public"}} {
		if _, err := database.Pool.Exec(ctx, `
			insert into projects (key, name, default_assignee, owner, visibility) values ($1, $1, 'alice', 'alice', $2)
		`, project.key, project.visibility); err != nil {
			t.Fatalf("create project %s: %v", project.key, err)
		}
		if _, err := database.Pool.Exec(ctx, `
			insert into issues (key, project_key, number, title, route, created_by, rank)
			values ($1 || '-1', $1, 1, 'The issue of ' || $1, null, '{"kind":"user","id":"alice"}', 'U')
		`, project.key); err != nil {
			t.Fatalf("create issue %s-1: %v", project.key, err)
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

// topicsNaming is every topic publisher was sent that names issue.
func topicsNaming(publisher *recordingPublisher, issue string) []string {
	var named []string
	for _, item := range publisher.all() {
		if strings.Contains(item.Topic, "."+issue+".") {
			named = append(named, item.Topic)
		}
	}
	return named
}

// heldRow is what the hold leaves of an event's row: no attempt counted, no error, and a
// next_attempt_at, which the batches' read of fresh events no longer reads.
func heldRow(t *testing.T, database *store.Store, id int64) (attempts int, failure *string, waits bool) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(),
		`select attempt_count, publish_error, next_attempt_at is not null from events where id = $1`, id,
	).Scan(&attempts, &failure, &waits); err != nil {
		t.Fatalf("read event %d: %v", id, err)
	}
	return attempts, failure, waits
}

// TestTheOutboxHoldsAPrivateProjectsEventWhileItsHubsAreUnproven: with PrivateHubsReady answering
// false, one batch publishes PUB's event and leaves PRIV's unpublished, sent to no topic, with no
// attempt counted, no error and a wait of its own; once the func answers true, the next batch
// that reads PRIV's event publishes it to PRIV-1's topic.
//
// Control: an outbox that ignores PrivateHubsReady (heldPrivateOwners answering nil at once)
// publishes PRIV's event in the first batch, and the row fails naming PRIV-1's topic.
func TestTheOutboxHoldsAPrivateProjectsEventWhileItsHubsAreUnproven(t *testing.T) {
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedHoldProjects(t, database)
	private := appendHoldMessage(t, database, broker, "PRIV-1")
	public := appendHoldMessage(t, database, broker, "PUB-1")

	var proven atomic.Bool
	publisher := &recordingPublisher{}
	deps := Deps{Store: database, Publisher: publisher, Broker: broker, PrivateHubsReady: proven.Load}
	holder := holding(t, deps).instance
	if _, err := scanBatch(context.Background(), deps, unfenced{}, holder); err != nil {
		t.Fatalf("scan the batch while the hubs are unproven: %v", err)
	}
	if sent := topicsNaming(publisher, "PRIV-1"); len(sent) != 0 || publishedAt(t, database, private.ID) != nil {
		t.Fatalf("while the hubs are unproven PRIV's event was sent to %v and published at %v, want it held: sent nowhere, unpublished",
			sent, publishedAt(t, database, private.ID))
	}
	if attempts, failure, waits := heldRow(t, database, private.ID); attempts != 0 || failure != nil || !waits {
		t.Fatalf("PRIV's held event has %d attempts, error %v and a wait %t, want no attempt, no error and a wait of its own",
			attempts, nullable(failure), waits)
	}
	if sent := topicsNaming(publisher, "PUB-1"); len(sent) != 1 || publishedAt(t, database, public.ID) == nil {
		t.Fatalf("while the hubs are unproven PUB's event was sent to %v and published at %v, want it published as before",
			sent, publishedAt(t, database, public.ID))
	}

	proven.Store(true)
	dueNow(t, database, private.ID)
	if _, err := scanBatch(context.Background(), deps, unfenced{}, holder); err != nil {
		t.Fatalf("scan the batch once the hubs are proven: %v", err)
	}
	want := "notifications.dispatch.issue.PRIV-1.message.created"
	if sent := topicsNaming(publisher, "PRIV-1"); len(sent) != 1 || sent[0] != want || publishedAt(t, database, private.ID) == nil {
		t.Fatalf("once the hubs are proven PRIV's event was sent to %v and published at %v, want it published to %s",
			sent, publishedAt(t, database, private.ID), want)
	}
}

// TestAHeldPrivateEventPublishesOnceItsHubsAreProven runs the outbox (Run), its hold's wait
// shortened to 100 ms: while PrivateHubsReady answers false PUB's event publishes and PRIV's does
// not, though the outbox asks the func again at each wait; once it answers true, PRIV's event
// publishes with no other event written to wake the outbox, on the wait's own timer
// (Outbox.nextRetry).
//
// Control: a hold that writes no wait (heldPrivateOwners' events left due, or an outcome with no
// delay) leaves nothing to wake the outbox, and the row fails waiting for PRIV's publish, or the
// func is asked once alone.
func TestAHeldPrivateEventPublishesOnceItsHubsAreProven(t *testing.T) {
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedHoldProjects(t, database)

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
	waitFor(t, 10*time.Second, "the outbox asking the hubs' proof again after the hold's wait", func() bool {
		return asked.Load() >= before+3
	})
	if sent := topicsNaming(publisher, "PRIV-1"); len(sent) != 0 || publishedAt(t, database, private.ID) != nil {
		t.Fatalf("while the hubs are unproven PRIV's event was sent to %v, want it held", sent)
	}

	proven.Store(true)
	waitFor(t, 10*time.Second, "PRIV's event published once the hubs are proven", func() bool {
		return publishedAt(t, database, private.ID) != nil
	})
	if sent := topicsNaming(publisher, "PRIV-1"); len(sent) != 1 {
		t.Fatalf("once the hubs are proven PRIV's event was sent to %v, want its one topic", sent)
	}
}

// TestANilPrivateHubsReadyHoldsNothing (R9): with no PrivateHubsReady, as cmd/dispatch runs until a
// hub can prove it, PRIV's event publishes in the first batch beside PUB's, and the batch reads
// nothing of any project's visibility: the outbox makes the statements it made before the hold.
//
// Control: a hold that reads the projects whatever the func (the nil check dropped from
// heldPrivateOwners) makes the statement this row looks for, and the row fails naming it.
func TestANilPrivateHubsReadyHoldsNothing(t *testing.T) {
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedHoldProjects(t, database)
	private := appendHoldMessage(t, database, broker, "PRIV-1")
	public := appendHoldMessage(t, database, broker, "PUB-1")

	tracer := &statementTracer{}
	traced := tracedStore(t, database, tracer)
	publisher := &recordingPublisher{}
	deps := Deps{Store: traced, Publisher: publisher, Broker: broker}
	if _, err := scanBatch(context.Background(), deps, unfenced{}, holding(t, deps).instance); err != nil {
		t.Fatalf("scan the batch: %v", err)
	}
	for _, event := range []model.Event{private, public} {
		if publishedAt(t, database, event.ID) == nil {
			t.Fatalf("with no PrivateHubsReady the event of %s is unpublished, want it published as before", *event.IssueKey)
		}
	}
	if read := tracer.matching("visibility"); len(read) != 0 {
		t.Fatalf("with no PrivateHubsReady the batch read a project's visibility: %q, want no statement the outbox did not make before", read[0].sql)
	}
}
