package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
)

// holding returns an outbox on deps whose instance holds the publisher hold's row for the next
// hour, as a process the hold names does, for a test that scans it without running it: a record's
// back-offs are written only while the hold is its own (recordBatch).
func holding(t *testing.T, deps Deps) *Outbox {
	t.Helper()
	o := New(deps)
	holdRow(t, deps.Store, o.instance)
	return o
}

// holdRow writes the publisher hold's row naming holder, lapsing an hour from now on the database's
// clock.
func holdRow(t *testing.T, database *store.Store, holder uuid.UUID) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into duty_holds (name, holder, expires_at) values ($1, $2, now() + interval '1 hour')
		on conflict (name) do update set holder = excluded.holder, expires_at = excluded.expires_at
	`, publisherHold, holder); err != nil {
		t.Fatalf("write the publisher hold's row: %v", err)
	}
}

// holdingProcess is the process the publisher hold's row names, whatever its lease says: the
// identity a test gives a fallback pass it runs outside an Outbox (publishFallbackCopies), since
// in production the pass runs in the process that took the hold and is given that process's
// instance (Outbox.serve), and its record is written only while the hold is still that
// process's (recordFallbackCopies). A test whose pass must find the hold gone passes an identity
// of its own instead.
func holdingProcess(t *testing.T, database *store.Store) uuid.UUID {
	t.Helper()
	var holder uuid.UUID
	if err := database.Pool.QueryRow(context.Background(),
		`select holder from duty_holds where name = $1`, publisherHold).Scan(&holder); err != nil {
		t.Fatalf("read the process the publisher hold names: %v", err)
	}
	return holder
}

// statementTracer records every statement its pool's connections run, with its arguments.
type statementTracer struct {
	mu         sync.Mutex
	statements []tracedStatement
}

type tracedStatement struct {
	sql  string
	args []any
}

func (s *statementTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statements = append(s.statements, tracedStatement{sql: data.SQL, args: slices.Clone(data.Args)})
	return ctx
}

func (s *statementTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// matching returns the statements recorded so far whose SQL holds every one of fragments.
func (s *statementTracer) matching(fragments ...string) []tracedStatement {
	s.mu.Lock()
	defer s.mu.Unlock()
	var found []tracedStatement
	for _, statement := range s.statements {
		if !slices.ContainsFunc(fragments, func(fragment string) bool { return !strings.Contains(statement.sql, fragment) }) {
			found = append(found, statement)
		}
	}
	return found
}

// matchingAny returns the statements recorded so far whose SQL holds any of fragments, each once.
func (s *statementTracer) matchingAny(fragments ...string) []tracedStatement {
	s.mu.Lock()
	defer s.mu.Unlock()
	var found []tracedStatement
	for _, statement := range s.statements {
		if slices.ContainsFunc(fragments, func(fragment string) bool { return strings.Contains(statement.sql, fragment) }) {
			found = append(found, statement)
		}
	}
	return found
}

// tracedStore opens a store on database's database whose every statement tracer sees: one that
// records them (statementTracer), or one that holds one of them up (heldRelease).
func tracedStore(t *testing.T, database *store.Store, tracer pgx.QueryTracer) *store.Store {
	t.Helper()
	config := database.Pool.Config().Copy()
	config.ConnConfig.Tracer = tracer
	opened, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open a traced pool: %v", err)
	}
	pool := store.NewPool(opened)
	t.Cleanup(pool.Close)
	return &store.Store{Pool: pool}
}

// sendClock is a bus client whose windows report each send's answer to onAnswer as it comes back,
// before the outbox sees it: a test that must act the moment a send is decided waits on that
// rather than on a clock of its own.
type sendClock struct {
	*bus.Client
	onAnswer func()
}

func (c *sendClock) WindowUntil(deadline func() time.Time) bus.Sender {
	return sendClockSender{Sender: c.Client.WindowUntil(deadline), clock: c}
}

type sendClockSender struct {
	bus.Sender
	clock *sendClock
}

func (s sendClockSender) PublishAsync(item contracts.Envelope) <-chan error {
	answer := s.Sender.PublishAsync(item)
	relayed := make(chan error, 1)
	go func() {
		err := <-answer
		s.clock.onAnswer()
		relayed <- err
	}()
	return relayed
}

// An issue's events each go to its topic family and to session S. S's destination fails for the
// first event, and the first event is backed off. Two later events written while it waits reach
// the topic family at once, but S only after the first one does, and S takes all three in order.
// S is reached two ways: as a session route on its own subject, which fails when the stream stops
// capturing agent subjects, so NATS answers its publish with no responders; and as the holder of
// the issue's role, whose first forward fails because the stream, capturing no agent subject,
// acknowledges nothing of it.
func TestABackedOffEventHoldsBackOnlyItsDestination(t *testing.T) {
	t.Parallel()
	t.Run("a session route the stream does not take", holdsBackASessionRouteTheStreamRefuses)
	t.Run("a role holder whose forward the stream does not acknowledge", holdsBackARoleHolderWhoseForwardIsNotAcknowledged)
}

func holdsBackASessionRouteTheStreamRefuses(t *testing.T) {
	url := testnats.URL(t)
	client, err := bus.ConnectOwningStream([]string{url})
	if err != nil {
		t.Fatalf("connect the outbox's client: %v", err)
	}
	t.Cleanup(client.Close)
	reader := testnats.Connect(t, url)
	js, err := reader.JetStream()
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	info, err := js.StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	capturing := info.Config
	notAgents := info.Config
	notAgents.Subjects = slices.DeleteFunc(slices.Clone(capturing.Subjects), func(subject string) bool {
		return subject == contracts.AgentTopicPrefix+">"
	})
	if _, err := js.UpdateStream(&notAgents); err != nil {
		t.Fatalf("stop the stream capturing agent subjects: %v", err)
	}

	database := storetest.Open(t)
	broker := events.NewBroker()
	route := "session:S"
	seedIssue(t, database, "T-1", &route)
	write := func(body string) model.Event {
		return appendEvent(t, database, broker, model.Event{
			IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
			Payload: model.Message{ID: "5a660655-04ad-4ce0-8a9b-93dd03c412b7", IssueKey: new("T-1"), Body: body},
		})
	}
	first := write("first")
	stop := runOutbox(t, database, client, broker)
	defer stop()
	waitFor(t, 10*time.Second, "the first event backed off for S's subject", func() bool {
		_, attempts := publishedDestinations(t, database, first.ID)
		return attempts > 0
	})
	// The later events are written while the stream still refuses S's subject, so what holds the
	// first event back is this row's own doing and not a timer it has to beat: every retry of it
	// fails for S again, for as long as the subject is refused, exactly as the withheld receipt
	// holds the first event in the half of this row below.
	second, third := write("second"), write("third")

	topic := "notifications.dispatch.issue.T-1.message.created"
	session := contracts.AgentSubject("S")
	// The two later events reach the topic family while the first event is still unreached at S:
	// the back-off holds back its own destination and no other. It is read here, while that hold
	// is on, and not from where two subjects' copies fell in one stream's sequence afterwards:
	// the first event's retry falls due on its own back-off, about a second after its failure, so
	// a run that took longer than that to restore the subject and write these two events saw S's
	// copy of the first event stored ahead of them and read the owner's order as broken, though
	// nothing had waited on anything. That is how this row reached a CI red (run 42779139068
	// attempt 2, dispatch://INFRA-1075).
	waitFor(t, eventWait, "the later events on the topic family", func() bool {
		bySubject, _ := streamOrder(t, reader)
		return slices.Contains(bySubject[topic], second.ID) && slices.Contains(bySubject[topic], third.ID)
	})
	for _, event := range []model.Event{first, second, third} {
		if publishedAt(t, database, event.ID) != nil {
			t.Fatalf("event %d was published while S's subject was still refused, want it waiting on S", event.ID)
		}
	}
	if bySubject, _ := streamOrder(t, reader); len(bySubject[session]) != 0 {
		t.Fatalf("S's subject holds %v while the stream refuses it, want nothing", bySubject[session])
	}

	// S's subject is taken again and the first event is made due, with a wake of its own: an
	// update of the row notifies nothing, and the outbox's retry timer is armed to the back-off
	// the refusals grew, which this row would otherwise wait out.
	if _, err := js.UpdateStream(&capturing); err != nil {
		t.Fatalf("let the stream capture agent subjects again: %v", err)
	}
	dueNow(t, database, first.ID)
	broker.Publish(model.Event{})

	waitFor(t, 30*time.Second, "all three events published", func() bool {
		return publishedAt(t, database, first.ID) != nil && publishedAt(t, database, second.ID) != nil &&
			publishedAt(t, database, third.ID) != nil
	})
	for _, event := range []model.Event{second, third} {
		if _, attempts := publishedDestinations(t, database, event.ID); attempts != 0 {
			t.Fatalf("event %d counted %d attempts, want none: it only waited on the first event", event.ID, attempts)
		}
	}
	bySubject, _ := streamOrder(t, reader)
	want := []int64{first.ID, second.ID, third.ID}
	if !slices.Equal(bySubject[topic], want) {
		t.Fatalf("the topic family holds %v, want %v", bySubject[topic], want)
	}
	if !slices.Equal(bySubject[session], want) {
		t.Fatalf("S's subject holds %v, want %v: each event only after the one before it", bySubject[session], want)
	}
}

func holdsBackARoleHolderWhoseForwardIsNotAcknowledged(t *testing.T) {
	rig := newRoleRig(t)
	const role, holder = "reviewer", "S"
	rig.claim(t, role, holder, session.SessionEntry{SelfSubscribed: true})
	rig.silent(t, contracts.RoleTopicPrefix+role)
	// The stream captures no agent subject until it is let capture them again, so nothing
	// acknowledges a forward to S and the first event's send runs out of time. S is handed every
	// forward over core NATS all the same, and answers none of them: nothing but the stream does.
	capturing := rig.narrowAgentCapture(t)
	var (
		mu       sync.Mutex
		arrivals []int64
	)
	sub, err := rig.conn.Subscribe(contracts.AgentSubject(holder), func(msg *natsgo.Msg) {
		id := envelopeEventID(t, msg.Data)
		mu.Lock()
		arrivals = append(arrivals, id)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("subscribe S: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := rig.conn.Flush(); err != nil {
		t.Fatalf("flush S's subscription: %v", err)
	}
	taken := func() []int64 {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(arrivals)
	}
	first := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})
	write := func(body string) model.Event {
		return appendEvent(t, rig.database, rig.broker, model.Event{
			IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
			Payload: model.Message{ID: uuid.NewString(), IssueKey: new("T-1"), Body: body},
		})
	}
	stop := runOutbox(t, rig.database, rig.client, rig.broker)
	defer stop()
	waitFor(t, 15*time.Second, "the first event backed off for S", func() bool {
		_, attempts := publishedDestinations(t, rig.database, first.ID)
		return attempts > 0
	})
	second, third := write("second"), write("third")

	topic := "notifications.dispatch.issue.T-1.message.created"
	waitFor(t, 10*time.Second, "the later events on the topic family", func() bool {
		bySubject, _ := streamOrder(t, rig.conn)
		return slices.Contains(bySubject[topic], second.ID) && slices.Contains(bySubject[topic], third.ID)
	})
	for _, got := range taken() {
		if got != first.ID {
			t.Fatalf("S took event %d while the first event's forward was unacknowledged: %v", got, taken())
		}
	}
	for _, event := range []model.Event{first, second, third} {
		if publishedAt(t, rig.database, event.ID) != nil {
			t.Fatalf("event %d was published before the stream stored its forward to S", event.ID)
		}
	}

	capturing()
	waitFor(t, 45*time.Second, "all three events published", func() bool {
		return publishedAt(t, rig.database, first.ID) != nil && publishedAt(t, rig.database, second.ID) != nil &&
			publishedAt(t, rig.database, third.ID) != nil
	})
	for _, event := range []model.Event{second, third} {
		if _, attempts := publishedDestinations(t, rig.database, event.ID); attempts != 0 {
			t.Fatalf("event %d counted %d attempts, want none: it only waited on the first event", event.ID, attempts)
		}
	}
	var order []int64
	for _, got := range taken() {
		if !slices.Contains(order, got) {
			order = append(order, got)
		}
	}
	if want := []int64{first.ID, second.ID, third.ID}; !slices.Equal(order, want) {
		t.Fatalf("S took events %v, want %v: each only after the one before it", order, want)
	}
}

// shiftedClock moves database's clock an hour ahead of the pod's, as a database host whose clock
// disagrees with the pod's has it, and returns a store whose connections read it: every now() they
// call answers an hour later than the pod's clock.
func shiftedClock(t *testing.T, database *store.Store) *store.Store {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		create schema testclock;
		create function testclock.now() returns timestamptz language sql stable
			as $$ select pg_catalog.now() + interval '1 hour' $$;
		do $$ begin
			execute format('alter database %I set search_path = testclock, pg_catalog, public', current_database());
		end $$;
	`); err != nil {
		t.Fatalf("shift the database's clock: %v", err)
	}
	return storetest.SecondProcess(t, database)
}

// NATS that does not acknowledge an event's envelope on a connection still current backs it off:
// no responders (no stream captures the subject) and a stream that never answers (the relay
// frozen, the connection still current) each count an attempt and record the error, and the
// event's next attempt is set on the database's clock, so it is not sent again before then. A
// connection that is lost instead releases the event with no attempt, and it is sent once NATS is
// back.
func TestAnEventNATSNeverAcknowledgesIsBackedOffAndRecorded(t *testing.T) {
	t.Parallel()
	seed := func(t *testing.T, database *store.Store) model.Event {
		t.Helper()
		seedIssue(t, database, "T-1", nil)
		return appendEvent(t, database, events.NewBroker(), model.Event{
			IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
			Payload: model.Message{ID: "5a660655-04ad-4ce0-8a9b-93dd03c412b7", IssueKey: new("T-1"), Body: "acknowledge me"},
		})
	}
	backedOff := func(t *testing.T, database *store.Store, event model.Event, cause string) {
		t.Helper()
		var attempts int
		var failure *string
		if err := database.Pool.QueryRow(context.Background(),
			`select attempt_count, publish_error from events where id = $1`, event.ID).Scan(&attempts, &failure); err != nil {
			t.Fatalf("read the event: %v", err)
		}
		if attempts != 1 || failure == nil || !strings.Contains(*failure, cause) || publishedAt(t, database, event.ID) != nil {
			t.Fatalf("attempt_count %d, publish_error %v, published %v: want one attempt and an error naming %q",
				attempts, failure, publishedAt(t, database, event.ID) != nil, cause)
		}
	}

	t.Run("no responders", func(t *testing.T) {
		t.Parallel()
		url := testnats.URL(t)
		// A client that owns no stream, in an account that has none: no stream captures the subject.
		client, err := bus.Connect([]string{url})
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(client.Close)
		database := storetest.Open(t)
		event := seed(t, database)
		shifted := shiftedClock(t, database)
		counter := &sendCounter{}
		outbox := holding(t, Deps{Store: shifted, Publisher: countingClient{Client: client, counter: counter}})
		outbox.scan(context.Background(), unfenced{})

		backedOff(t, database, event, "no responders")
		var due float64
		var nextAttempt time.Time
		if err := shifted.Pool.QueryRow(context.Background(),
			`select extract(epoch from next_attempt_at - now()), next_attempt_at from events where id = $1`, event.ID).Scan(&due, &nextAttempt); err != nil {
			t.Fatalf("read the event's next attempt: %v", err)
		}
		if due <= 0 || due > 1 {
			t.Fatalf("the next attempt is %.3fs past the database's now(), want within the first back-off's second", due)
		}
		if until := time.Until(nextAttempt); until < 50*time.Minute {
			t.Fatalf("the next attempt is %v past the pod's clock, want it on the database's, an hour ahead", until)
		}
		sent, _ := counter.of(event.ID)
		outbox.scan(context.Background(), unfenced{})
		if again, _ := counter.of(event.ID); again != sent {
			t.Fatalf("the backed-off event was sent %d more times by the next scan, want none before its back-off", again-sent)
		}
	})

	t.Run("a stream that never answers", func(t *testing.T) {
		t.Parallel()
		server, url := testnats.Start(t)
		client, err := bus.ConnectOwningStream([]string{url})
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(client.Close)
		database := storetest.Open(t)
		event := seed(t, database)
		thaw := server.Freeze(t)
		// The send's own answer is the thaw's cue: the window gave the acknowledgement up, and the
		// flush probe that follows has not run yet, so the probe finds the connection answering.
		// Sleeping past the window's five seconds waited for this same moment by the clock
		// (dispatch://INFRA-1075).
		clock := &sendClock{Client: client, onAnswer: sync.OnceFunc(thaw)}
		holding(t, Deps{Store: database, Publisher: clock}).scan(context.Background(), unfenced{})
		backedOff(t, database, event, bus.ErrNotAcknowledged.Error())
	})

	t.Run("a lost connection", func(t *testing.T) {
		t.Parallel()
		url := testnats.URL(t)
		proxy := testnats.StartProxy(t, url)
		client, err := bus.ConnectOwningStream([]string{proxy.URL()})
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(client.Close)
		database := storetest.Open(t)
		event := seed(t, database)
		proxy.Cut()
		waitFor(t, eventWait, "the client noticing the cut", func() bool { return !client.Connected() })
		outbox := holding(t, Deps{Store: database, Publisher: client})
		outbox.scan(context.Background(), unfenced{})
		var attempts int
		var nextAttempt *time.Time
		var failure *string
		if err := database.Pool.QueryRow(context.Background(),
			`select attempt_count, next_attempt_at, publish_error from events where id = $1`, event.ID).Scan(&attempts, &nextAttempt, &failure); err != nil {
			t.Fatalf("read the event: %v", err)
		}
		if attempts != 0 || nextAttempt != nil || failure != nil || publishedAt(t, database, event.ID) != nil {
			t.Fatalf("attempt_count %d, next_attempt_at %v, publish_error %v after a lost connection, want it released untouched",
				attempts, nextAttempt, failure)
		}
		proxy.Restore()
		waitFor(t, 10*time.Second, "the client reconnecting", client.Connected)
		outbox.scan(context.Background(), unfenced{})
		if publishedAt(t, database, event.ID) == nil {
			t.Fatal("the released event was not published once NATS was back")
		}
	})
}

// A batch of events of every kind whose destinations need a lookup - replies to comments that
// mention their thread's author, resolutions of those threads, ask answers, replies to messages,
// comments on documents and a person's settled edits of documents, on an issue a session holds the
// claim of, and comments on an issue nobody routes or claims, whose primary document a session
// wrote - reads their followers, authors, comment recipients and mention deliveries in at most
// seven statements, one per kind, and publishes every one of them. The kinds are the ask
// followers, the thread root authors a resolution reads, the message authors, the document
// authors, the mention deliveries a new comment whose mentions resolved to a session reads
// (loadMentionDeliveries), and #2366's two: the sessions of the threads the replies are in
// (loadCommentRecipients) and the sessions of the unclaimed issues' primary documents
// (loadPrimaryDocumentAuthors). The claimant costs no statement: the batch's own read carries the
// issue's claim (readBatch). A document's writers are one kind, whether a comment on it or an edit
// of it asks for them, so the edits add no statement.
func TestTheBatchResolvesFollowersAuthorsRecipientsAndMentionDeliveriesInAtMostSevenStatements(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	claimIssue(t, database, "T-1", model.Actor{Kind: "session", ID: "claimant"})
	seedSecondIssue(t, database, "T-2")
	seedSpec(t, database, "T-2", model.Actor{Kind: "session", ID: "primary-author"})
	const each = batchSize / 7
	var seeded []model.Event
	alice := model.Actor{Kind: "user", ID: "alice"}
	for index := range each {
		writer := model.Actor{Kind: "session", ID: fmt.Sprintf("writer-%d", index)}
		root := seedComment(t, database, "T-1", writer, "Draft", nil)
		reply := uuid.NewString()
		// Every other reply's mention of its thread's author reached the author; the rest failed,
		// so their author route still carries the reply.
		delivery := "failed"
		if index%2 == 0 {
			delivery = "sent"
		}
		seedMentionDelivery(t, database, reply, writer.ID, delivery)
		seeded = append(seeded, appendEvent(t, database, broker, model.Event{
			IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
			Payload: model.CommentEventPayload{
				Comment: model.Comment{
					ID: reply, IssueKey: new("T-1"), Body: "@" + writer.ID + " reviewed", ReplyTo: &root,
					Mentions: mentionOf(writer.ID),
				},
			},
		}))
		seeded = append(seeded, appendEvent(t, database, broker, model.Event{
			IssueKey: new("T-1"), Type: "comment.resolved", Actor: alice,
			Payload: map[string]any{"id": root},
		}))
		askID := seedAsk(t, database, "T-1", model.Actor{Kind: "session", ID: fmt.Sprintf("asker-%d", index)}, "Ship it?")
		seedFollower(t, database, askID, fmt.Sprintf("follower-%d", index))
		seeded = append(seeded, appendEvent(t, database, broker, model.Event{
			IssueKey: new("T-1"), Type: "ask.answered", Actor: alice,
			Payload: map[string]any{"id": askID, "question": "Ship it?"},
		}))
		message := seedMessage(t, database, "T-1", model.Actor{Kind: "session", ID: fmt.Sprintf("sender-%d", index)}, "Status?", nil)
		seeded = append(seeded, appendEvent(t, database, broker, model.Event{
			IssueKey: new("T-1"), Type: "message.created", Actor: alice,
			Payload: model.Message{ID: uuid.NewString(), IssueKey: new("T-1"), Body: "Fine", InReplyTo: &message},
		}))
		document := seedDocument(t, database, new("T-1"), fmt.Sprintf("notes-%d", index), model.Actor{Kind: "session", ID: fmt.Sprintf("author-%d", index)})
		seeded = append(seeded, appendEvent(t, database, broker, model.Event{
			IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
			Payload: map[string]any{"id": uuid.NewString(), "body": "A note", "anchor": anchorOn(document)},
		}))
		edited := seedDocument(t, database, new("T-1"), fmt.Sprintf("spec-%d", index), model.Actor{Kind: "session", ID: fmt.Sprintf("spec-author-%d", index)})
		seeded = append(seeded, appendEvent(t, database, broker, model.Event{
			IssueKey: new("T-1"), Type: "artifact.human_edited", Actor: alice,
			Payload: map[string]any{"artifact_id": edited, "name": "spec.md", "from_version": 1, "to_version": 2, "editors": []any{alice.ID}},
		}))
		seeded = append(seeded, appendEvent(t, database, broker, model.Event{
			IssueKey: new("T-2"), Type: "comment.created", Actor: alice,
			Payload: map[string]any{"id": uuid.NewString(), "body": "On the second issue"},
		}))
	}

	tracer := &statementTracer{}
	publisher := &recordingPublisher{}
	holding(t, Deps{Store: tracedStore(t, database, tracer), Publisher: publisher}).scan(context.Background(), unfenced{})

	for _, event := range seeded {
		if publishedAt(t, database, event.ID) == nil {
			t.Fatalf("event %d (%s) was not published", event.ID, event.Type)
		}
	}
	agents, claimant := 0, 0
	for _, item := range publisher.all() {
		if strings.HasPrefix(item.Topic, contracts.AgentTopicPrefix) {
			agents++
		}
		if item.Topic == contracts.AgentSubject("claimant") {
			claimant++
		}
	}
	// Each comment reply whose mention failed reaches its thread's author, each resolution its
	// thread root's author, each ask answer its two followers, each message reply its parent's
	// sender, each person's edit the edited document's author, and each comment on the unclaimed
	// issue its spec's author; and every reply, resolution, answer, message and comment on the
	// claimed issue - a person's - reaches the claimant, which an edit, no claimant event, does
	// not. Each comment on a document of the claimed issue reaches that document's author no
	// more: on an issue a session claims, the credits of a document are no second reason to
	// deliver what the claimant already hears on its own topic (addAuthorRoutes). The read of
	// those credits is made all the same, so the statement count below is unchanged.
	if want := each * 5; claimant != want {
		t.Fatalf("the batch reached the claimant %d times, want %d", claimant, want)
	}
	if want := each/2 + each*6 + each*5; agents != want {
		t.Fatalf("the batch reached %d sessions' own subjects, want %d", agents, want)
	}
	lookups := tracer.matchingAny("ask_followers", "from comments", "from messages", "artifact_versions", "from comment_deliveries", "is_primary")
	if len(lookups) > 7 {
		t.Fatalf("the batch of %d events ran %d follower, author, recipient and mention delivery lookups, want at most 7", len(seeded), len(lookups))
	}
	t.Logf("the batch of %d events ran %d follower, author, recipient and mention delivery lookups", len(seeded), len(lookups))
}

// One row no recipient read can take, among a batch's many ids of that read, fails only the event
// that asked it, and costs the batch a run of each half of the refused statement's ids, and of each
// refused half's halves down to that id alone (loadRecipients): at most 2*ceil(log2(n)) more
// statements for n ids, where a run of each id alone would cost n.
func TestOneBrokenRowOfARecipientReadCostsTheBatchAboutTwoStatementsPerHalving(t *testing.T) {
	t.Parallel()
	const n = 64
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	alice := model.Actor{Kind: "user", ID: "alice"}
	edits := make([]model.Event, n)
	for index := range n {
		issueKey := fmt.Sprintf("T-%d", index+1)
		if index > 0 {
			if _, err := database.Pool.Exec(context.Background(), `
				insert into issues (key, project_key, number, title, created_by, rank)
				values ($1, 'TT', $2, 'Another issue', '{"kind":"user","id":"alice"}', 'V')
			`, issueKey, index+1); err != nil {
				t.Fatalf("create issue %s: %v", issueKey, err)
			}
		}
		author := model.Actor{Kind: "session", ID: fmt.Sprintf("author-%d", index)}
		document := seedDocument(t, database, new(issueKey), "spec", author, []model.Actor{author})
		if index == n/3 {
			breakDocumentCredits(t, database, document)
		}
		edits[index] = appendEvent(t, database, broker, model.Event{
			IssueKey: new(issueKey), Type: "artifact.human_edited", Actor: alice,
			Payload: map[string]any{"artifact_id": document, "name": "spec.md", "from_version": 1, "to_version": 2, "editors": []any{alice.ID}},
		})
	}

	tracer := &statementTracer{}
	deps := Deps{Store: tracedStore(t, database, tracer), Publisher: &recordingPublisher{}}
	if _, err := scanBatch(context.Background(), deps, unfenced{}, holding(t, deps).instance); err != nil {
		t.Fatalf("scan batch: %v", err)
	}
	for index, edit := range edits {
		row := readEventRow(t, database, edit.ID)
		if index == n/3 {
			if row.publishedAt != nil || row.attempts != 1 || row.nextAttempt == nil {
				t.Fatalf("the edit of the broken document has published_at %v, attempt_count %d and next_attempt_at %v, want it backed off",
					row.publishedAt, row.attempts, row.nextAttempt)
			}
			continue
		}
		want := []string{contracts.AgentSubject(fmt.Sprintf("author-%d", index))}
		if row.publishedAt == nil || row.attempts != 0 || !slices.Equal(agentTopics(row.destinations), want) {
			t.Fatalf("edit %d has published_at %v, attempt_count %d and published_destinations %v, want it published to %v",
				index, row.publishedAt, row.attempts, row.destinations, want)
		}
	}
	reads := len(tracer.matching("artifact_versions"))
	if limit := 1 + 2*int(math.Ceil(math.Log2(n))); reads > limit {
		t.Fatalf("the batch ran %d document credit reads for %d documents, one broken, want at most %d", reads, n, limit)
	}
	t.Logf("the batch ran %d document credit reads for %d documents, one broken", reads, n)
}

// An event is written once, when its batch records it: reading the batch writes nothing, so however
// many destinations it has, a published event costs its row exactly one update.
func TestAPublishedEventIsWrittenOnce(t *testing.T) {
	t.Parallel()
	alice := model.Actor{Kind: "user", ID: "alice"}
	for _, tc := range []struct {
		destinations int
		seed         func(t *testing.T, database *store.Store, broker *events.Broker) model.Event
	}{
		{1, func(t *testing.T, database *store.Store, broker *events.Broker) model.Event {
			return appendEvent(t, database, broker, model.Event{
				IssueKey: new("T-1"), Type: "message.created", Actor: alice,
				Payload: model.Message{ID: uuid.NewString(), IssueKey: new("T-1"), Body: "one destination"},
			})
		}},
		{3, func(t *testing.T, database *store.Store, broker *events.Broker) model.Event {
			root := seedComment(t, database, "T-1", model.Actor{Kind: "session", ID: "writer"}, "Draft", nil)
			return appendEvent(t, database, broker, model.Event{
				IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
				Payload: model.CommentEventPayload{Comment: model.Comment{
					ID: uuid.NewString(), IssueKey: new("T-1"), Body: "Reviewed", ReplyTo: &root,
				}},
			})
		}},
		{7, func(t *testing.T, database *store.Store, broker *events.Broker) model.Event {
			askID := seedAsk(t, database, "T-1", model.Actor{Kind: "session", ID: "asker"}, "Ship it?")
			for index := range 4 {
				seedFollower(t, database, askID, fmt.Sprintf("follower-%d", index))
			}
			return appendEvent(t, database, broker, model.Event{
				IssueKey: new("T-1"), Type: "ask.answered", Actor: alice,
				Payload: map[string]any{"id": askID, "question": "Ship it?"},
			})
		}},
	} {
		t.Run(fmt.Sprintf("%d destinations", tc.destinations), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			database := storetest.Open(t)
			broker := events.NewBroker()
			route := "role:reviewer"
			seedIssue(t, database, "T-1", &route)
			if tc.destinations == 1 {
				if _, err := database.Pool.Exec(ctx, `update issues set route = null where key = 'T-1'`); err != nil {
					t.Fatalf("clear the route: %v", err)
				}
			}
			const published = 3
			var seeded []model.Event
			for range published {
				seeded = append(seeded, tc.seed(t, database, broker))
			}
			updates := func() int64 {
				var count int64
				if err := database.Pool.QueryRow(ctx, `select pg_stat_force_next_flush()`).Scan(new(any)); err != nil {
					t.Fatalf("flush this session's statistics: %v", err)
				}
				if err := database.Pool.QueryRow(ctx,
					`select n_tup_upd from pg_stat_user_tables where relid = 'events'::regclass`).Scan(&count); err != nil {
					t.Fatalf("read the events table's updates: %v", err)
				}
				return count
			}
			// A backend reports its statistics within a second of going idle: read the count once
			// it has held still that long.
			settled := func() int64 {
				last := updates()
				for {
					time.Sleep(1500 * time.Millisecond)
					now := updates()
					if now == last {
						return now
					}
					last = now
				}
			}
			before := settled()
			holding(t, withHeldRoles(Deps{Store: database, Publisher: &recordingPublisher{}})).scan(ctx, unfenced{})
			for _, event := range seeded {
				if publishedAt(t, database, event.ID) == nil {
					t.Fatalf("event %d was not published", event.ID)
				}
				if destinations, _ := publishedDestinations(t, database, event.ID); len(destinations) != tc.destinations {
					t.Fatalf("event %d reached %v, want %d destinations", event.ID, destinations, tc.destinations)
				}
			}
			waitFor(t, 15*time.Second, "the scan's updates reported", func() bool { return updates()-before >= published })
			if written := settled() - before; written != published {
				t.Fatalf("publishing %d events of %d destinations each updated events %d times, want %d: one write per event",
					published, tc.destinations, written, published)
			}
		})
	}
}

// planNode is one node of EXPLAIN (FORMAT JSON)'s plan.
type planNode struct {
	NodeType         string     `json:"Node Type"`
	Relation         string     `json:"Relation Name"`
	Index            string     `json:"Index Name"`
	RemovedByFilter  float64    `json:"Rows Removed by Filter"`
	RemovedByRecheck float64    `json:"Rows Removed by Index Recheck"`
	Plans            []planNode `json:"Plans"`
}

func (n planNode) walk(visit func(planNode)) {
	visit(n)
	for _, child := range n.Plans {
		child.walk(visit)
	}
}

// explainedPlan is one EXPLAIN of the claim: the plan's root node, and the JSON body the failure
// messages print.
type explainedPlan struct {
	root planNode
	body []byte
}

// claimCost is what the claim costs at one backlog size: the rows Postgres read from events while
// it ran, the blocks it fetched from the indexes of events, the ids it returned, and the plans it
// was explained with.
type claimCost struct {
	backlog     int
	rowsRead    int64
	seqRead     int64
	indexBlocks int64
	kept        []int64
	unpublished []int64
	plans       []explainedPlan
	fastest     float64
}

// claimReads runs the claim once on conn, inside a transaction it rolls back, and reports what
// Postgres read while it ran: the rows it read from events, all of them and the ones a sequential
// scan read, the blocks it fetched from the indexes of events, and the event ids the claim
// returned. Both counters count for the whole backend and a rollback keeps the count, so each
// figure is the difference across the claim rather than the counter itself, and every call must
// take the same pinned connection.
func claimReads(t *testing.T, conn *pgxpool.Conn, claim tracedStatement) (rowsRead, seqRead, indexBlocks int64, kept []int64) {
	t.Helper()
	ctx := context.Background()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	// The view holds no row for events until this transaction has read it, so sum over the rows
	// it does hold: that answers 0 and 0 rather than no row at all. The blocks are counted per
	// index, so they are summed over the indexes of events.
	counters := func() (seq, fetched, blocks int64) {
		if err := tx.QueryRow(ctx, `
			select coalesce(sum(seq_tup_read), 0), coalesce(sum(idx_tup_fetch), 0),
			       (select coalesce(sum(pg_stat_get_xact_blocks_fetched(indexrelid)), 0)
			          from pg_index where indrelid = 'events'::regclass)
			  from pg_stat_xact_user_tables where relname = 'events'
		`).Scan(&seq, &fetched, &blocks); err != nil {
			t.Fatalf("read what the claim read from events: %v", err)
		}
		return seq, fetched, blocks
	}
	seqBefore, fetchedBefore, blocksBefore := counters()
	rows, err := tx.Query(ctx, claim.sql, claim.args...)
	if err != nil {
		t.Fatalf("run the claim: %v\n%s", err, claim.sql)
	}
	// The first column is the event's id. Taking the row's values drains the row set, which the
	// counters need, and spells out none of the row's other nineteen columns.
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			t.Fatalf("read a claimed event: %v", err)
		}
		kept = append(kept, values[0].(int64))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the claimed events: %v", err)
	}
	seqAfter, fetchedAfter, blocksAfter := counters()
	return (seqAfter - seqBefore) + (fetchedAfter - fetchedBefore), seqAfter - seqBefore, blocksAfter - blocksBefore, kept
}

// measureClaim seeds a database of its own with backlog backed-off events and backlog pending
// commands, ten events the publisher has sent and ten it has not, and reports what the statement
// choosing a batch costs there. Each size needs its own database: events.id is a sequence, so rows
// added to a seeded database after the fresh events carry higher ids, which the plan never walks.
func measureClaim(t *testing.T, backlog int) claimCost {
	t.Helper()
	ctx := context.Background()
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	// Postgres takes several statements in one round trip only over the simple protocol, so the
	// backlog's size goes into the SQL pgx writes rather than into a bound parameter.
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		select 'O-' || n, 'TT', 100 + n, 'Waiting', '{"kind":"user","id":"alice"}', 'W'
		from generate_series(1, 50) as n;
		insert into events (issue_key, seq, type, actor, payload, notify, attempt_count, next_attempt_at)
		select 'O-' || (n % 50 + 1), n, 'message.created', '{"kind":"user","id":"alice"}', '{"body":"backed off"}', true,
		       1, now() + interval '1 hour'
		from generate_series(1, $1) as n;
		insert into events (issue_key, seq, type, actor, payload, notify)
		select 'T-1', 100000 + n, 'subscription.remove_requested', '{"kind":"user","id":"alice"}',
		       '{"session_id":"gone","pending":true}', true
		from generate_series(1, $1) as n;
	`, pgx.QueryExecModeSimpleProtocol, backlog); err != nil {
		t.Fatalf("seed the backed-off events and the pending commands: %v", err)
	}
	fresh := func() []int64 {
		ids := make([]int64, 0, 10)
		for index := range 10 {
			ids = append(ids, appendEvent(t, database, broker, model.Event{
				IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
				Payload: model.Message{ID: uuid.NewString(), IssueKey: new("T-1"), Body: fmt.Sprintf("fresh %d", index)},
			}).ID)
		}
		return ids
	}
	fresh()
	if _, err := database.Pool.Exec(ctx, `analyze events`); err != nil {
		t.Fatalf("analyze events: %v", err)
	}

	tracer := &statementTracer{}
	holding(t, Deps{Store: tracedStore(t, database, tracer), Publisher: &recordingPublisher{}}).scan(ctx, unfenced{})
	claims := tracer.matching("published_at is null", "limit")
	if len(claims) == 0 {
		t.Fatal("the scan ran no statement choosing a batch")
	}
	claim := claims[0]
	// Ten more due events, so the statement measured and explained has a batch to choose.
	unpublished := fresh()

	// The fastest of several runs: the box is small and shared, and a run another test's work
	// delayed says nothing about the statement.
	const runs = 20
	fastest := math.Inf(1)
	plans := make([]explainedPlan, 0, runs)
	for range runs {
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		var plan []byte
		if err := tx.QueryRow(ctx, "explain (analyze, buffers, format json) "+claim.sql, claim.args...).Scan(&plan); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("explain the claim: %v\n%s", err, claim.sql)
		}
		_ = tx.Rollback(ctx)
		var explained []struct {
			Plan          planNode `json:"Plan"`
			ExecutionTime float64  `json:"Execution Time"`
		}
		if err := json.Unmarshal(plan, &explained); err != nil || len(explained) != 1 {
			t.Fatalf("decode the plan: %v\n%s", err, plan)
		}
		plans = append(plans, explainedPlan{root: explained[0].Plan, body: plan})
		fastest = min(fastest, explained[0].ExecutionTime)
	}
	// The measurement comes after the explains, not before them: a pooled connection this
	// function holds while they run adds about 0.2 ms to each of them, 0.65 ms against 0.43 ms
	// here, and the logged time would report that delay as the claim's own.
	conn, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire the connection the counters are read on: %v", err)
	}
	defer conn.Release()
	// What the claim reads is settled rather than read twice: claimReads runs again on the
	// pinned connection until two consecutive readings report the same rowsRead and the same
	// indexBlocks, at most five readings, and that pair is the measurement. A claim run marks
	// the dead index entries the scan's publish left and reads those pages again to do it,
	// which the 20 explains above run the claim first to absorb; under this lane's load the
	// marking of one page does not always land before the first reading, and two consecutive
	// readings then differ by one block. Job 130541308666 of run 43021338811 read 37 blocks on
	// the first run and 38 on the second and failed there, where the same head read one figure
	// twice in attempt 1 (job 130525083786) and in three earlier pairs. Five is the bound
	// because the marking is bounded work that each reading retires: no reading leaves more of
	// it than the reading before, so a pair agrees within a few readings, and five gives four
	// chances to agree before the loop ends rather than runs on. On a two-core pod all 20
	// measurements of 5 runs of this row agreed at the second reading, idle and under two busy
	// loops alike, so the four spare readings are for a pod slower than that one.
	const mostReadings = 5
	type reading struct {
		rowsRead    int64
		seqRead     int64
		indexBlocks int64
		kept        []int64
	}
	taken := make([]reading, 0, mostReadings)
	settledAt := -1
	for len(taken) < mostReadings && settledAt < 0 {
		rowsRead, seqRead, indexBlocks, kept := claimReads(t, conn, claim)
		taken = append(taken, reading{rowsRead: rowsRead, seqRead: seqRead, indexBlocks: indexBlocks, kept: kept})
		if last := len(taken) - 1; last > 0 && taken[last-1].rowsRead == rowsRead && taken[last-1].indexBlocks == indexBlocks {
			settledAt = last
		}
	}
	// Settling hides no regression, because a broken claim reads the same figures on every run:
	// the claim over a retry index rebuilt as (id, next_attempt_at) fetched 56 blocks at 5,000
	// and 113 at 20,000 on both runs of each size, so its first two readings agree and the
	// bounds below weigh those figures. Only the marking moves a reading, by the one block it
	// has left to do, and only on the first reading. Readings that never agree are a measurement
	// that does not hold still, which is no measurement at all, so the row names them all and
	// stops.
	if settledAt < 0 {
		shown := make([]string, 0, len(taken))
		for index, each := range taken {
			shown = append(shown, fmt.Sprintf("reading %d read %d rows and fetched %d blocks", index+1, each.rowsRead, each.indexBlocks))
		}
		t.Fatalf("no two consecutive readings of the claim agreed in %d readings on the same connection, want a pair reading the same rows and the same blocks from the indexes of events; in order: %s. Each figure is the difference across the claim, and a measurement that reads pg_stat_xact_user_tables' counter rather than its difference grows by about the first reading every time",
			mostReadings, strings.Join(shown, "; "))
	}
	measured := taken[settledAt]
	if measured.rowsRead == 0 {
		t.Errorf("the claim read 0 rows from events in both readings of the settled pair: track_counts must be on for pg_stat_xact_user_tables to count a row")
	}
	t.Logf("the claim settled at reading %d of at most %d: %d rows read from events and %d blocks fetched from its indexes",
		settledAt+1, mostReadings, measured.rowsRead, measured.indexBlocks)

	return claimCost{
		backlog:     backlog,
		rowsRead:    measured.rowsRead,
		seqRead:     measured.seqRead,
		indexBlocks: measured.indexBlocks,
		kept:        measured.kept,
		unpublished: unpublished,
		plans:       plans,
		fastest:     fastest,
	}
}

// someIDs renders ids as how many there are and at most the first twelve of them, so a message
// about a claim that returned thousands of rows stays short enough to read.
func someIDs(ids []int64) string {
	const most = 12
	shown, tail := ids, ""
	if len(shown) > most {
		shown, tail = shown[:most], " …"
	}
	text := make([]string, 0, len(shown))
	for _, id := range shown {
		text = append(text, strconv.FormatInt(id, 10))
	}
	return fmt.Sprintf("%d events: [%s%s]", len(ids), strings.Join(text, " "), tail)
}

// The statement that chooses a batch reads neither the pending subscription commands, which are
// never published, nor the backed-off events of owners that have nothing due: with 5,000 of each
// in the table it reads three rows of events for each event it returns and nothing else, no node
// of its plan that reads events removes a row by filter or recheck, none scans events in full,
// and with 20,000 of each it reads the same number of rows and fetches about as many blocks from
// the indexes of events. Three a row is what it reads today, and the row allows up to eight, so a
// plan that reads one more of each event still passes.
//
// The row counts two things, because each is blind to work the other sees.
//
// seq_tup_read + idx_tup_fetch of pg_stat_xact_user_tables counts the heap tuples the claim read,
// taken inside the claim's own transaction, as api/suggestion_outcomes_test.go's
// TestOldPendingSuggestionsOnOtherIssuesCostAPassNothing counts them. It misses a scan that walks
// a btree and fetches no heap row, which is what an index whose leading column is not the one the
// claim's predicate names gives: rebuild events_outbox_retry as (id, next_attempt_at) and the
// claim's retry node becomes an index scan of it with next_attempt_at <= now() as its index
// condition, which walks every entry of the index, returns none of them and touches no heap row.
// On 2026-11-09 that claim read the same 30 rows at both sizes, removed no row by filter or
// recheck and returned the right ten ids, so every other clause here passed it; of the figures
// the row logged before it counted index blocks, only the time moved, 0.43 ms to 1.08 ms at
// 20,000.
//
// The blocks the claim fetched from the indexes of events, summed over pg_index with
// pg_stat_get_xact_blocks_fetched in the same transaction and on the same connection, see that
// walk: on 2026-11-09 the healthy claim fetched 37 blocks at 5,000 and 37 at 20,000, and the
// claim over the rebuilt index 56 and 113. blocks_fetched counts a hit and a read alike, so cache
// pressure does not move it. The tuples an index returned do not separate the two cases, which
// the review of INFRA-2065 measured: the access method skips the entries without returning them.
//
// A count is the same on a loaded machine as on an idle one, which no wall clock is. This row
// asserted the claim ran in under 0.5 ms, and on 2026-11-08 the claim, unchanged and healthy,
// ran in 0.434 to 0.448 ms on an idle pod and in 0.749 to 0.799 ms on the same pod under 48 busy
// loops. No one wall-clock bound separates a healthy claim from a broken one on every machine
// that runs this row, so the row asserts the counts and only logs the time.
func TestTheClaimSkipsPendingCommandsAndBackedOffRows(t *testing.T) {
	t.Parallel()
	small := measureClaim(t, 5000)
	big := measureClaim(t, 20000)
	costs := []claimCost{small, big}
	for _, cost := range costs {
		t.Logf("with %d backed-off events and %d pending commands the claim read %d rows from events, %d of them sequentially, fetched %d blocks from the indexes of events, and returned %d of the %d events waiting; it ran in %.3f ms at its fastest of %d:\n%s",
			cost.backlog, cost.backlog, cost.rowsRead, cost.seqRead, cost.indexBlocks, len(cost.kept), len(cost.unpublished),
			cost.fastest, len(cost.plans), cost.plans[0].body)
	}
	for _, cost := range costs {
		if !slices.Equal(cost.kept, cost.unpublished) {
			t.Errorf("with a backlog of %d the claim returned %s, want the events no one has published, in id order: %s",
				cost.backlog, someIDs(cost.kept), someIDs(cost.unpublished))
		}
		if cost.seqRead != 0 {
			t.Errorf("with a backlog of %d the claim read %d rows of events sequentially, want none",
				cost.backlog, cost.seqRead)
		}
	}
	if big.rowsRead != small.rowsRead {
		t.Errorf("the claim read %d rows of events with a backlog of %d and %d with a backlog of %d, want the same figure at both sizes",
			big.rowsRead, big.backlog, small.rowsRead, small.backlog)
	}
	if bound := 8 * int64(len(small.kept)); small.rowsRead > bound {
		t.Errorf("the claim read %d rows of events to return %d events, want at most %d: it reads three of each event it returns, and the row allows eight",
			small.rowsRead, len(small.kept), bound)
	}
	// The blocks the claim fetches in the indexes of events must not grow with the backlog: work
	// that grows with the rows it skips is the regression this figure is here for. Measured on
	// 2026-11-09 on an idle pod: 37 blocks at both sizes in all 12 readings of 3 runs of the
	// healthy claim, against 56 and 113 for a claim whose retry index leads on id. The half is
	// for a btree that gains a level as the table grows, which costs one more block for each
	// descent the claim makes, around ten of them; half of 37 is 18, and the mutated claim is 29
	// blocks past its own bound.
	if bound := small.indexBlocks + small.indexBlocks/2; big.indexBlocks > bound {
		t.Errorf("the claim fetched %d blocks from the indexes of events with a backlog of %d and %d with a backlog of %d, want at most %d at the larger size: what the claim does inside an index must not grow with the rows it skips",
			big.indexBlocks, big.backlog, small.indexBlocks, small.backlog, bound)
	}
	for _, cost := range costs {
		for _, explained := range cost.plans {
			explained.root.walk(func(node planNode) {
				if node.Relation != "events" {
					return
				}
				if node.NodeType == "Seq Scan" || node.RemovedByFilter != 0 || node.RemovedByRecheck != 0 {
					t.Fatalf("the claim's %s of events (%s) removed %.0f rows by filter and %.0f by recheck, want none:\n%s",
						node.NodeType, node.Index, node.RemovedByFilter, node.RemovedByRecheck, explained.body)
				}
			})
		}
	}
}

// A process that lost the publisher hold mid-batch records what is true whoever holds it - the
// destinations its events reached, and the events that reached all of them published - and
// schedules nothing: no back-off, no attempt, no error, no hold-back.
func TestARecordFromAProcessThatLostTheHoldWritesNoBackOff(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, route, created_by, rank)
		values ('T-2', 'TT', 2, 'Routed', 'role:reviewer', '{"kind":"user","id":"alice"}', 'V')
	`); err != nil {
		t.Fatalf("create the routed issue: %v", err)
	}
	message := func(issue string) model.Event {
		return appendEvent(t, database, broker, model.Event{
			IssueKey: new(issue), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
			Payload: model.Message{ID: uuid.NewString(), IssueKey: new(issue), Body: "lost"},
		})
	}
	done, failing, behind := message("T-1"), message("T-2"), message("T-2")
	publisher := &recordingPublisher{failTopic: roleTopic}
	outbox := New(withHeldRoles(Deps{Store: database, Publisher: publisher}))
	// Another process holds the hold now.
	holdRow(t, database, uuid.New())

	end := outbox.scan(ctx, unfenced{})
	if !end.fenced {
		t.Fatalf("the scan ended %+v, want it stopped as one whose hold is another's", end)
	}
	if publishedAt(t, database, done.ID) == nil {
		t.Fatal("the event that reached every destination was not marked published")
	}
	for _, event := range []model.Event{failing, behind} {
		var attempts int
		var nextAttempt *time.Time
		var failure *string
		var destinations []string
		if err := database.Pool.QueryRow(ctx,
			`select attempt_count, next_attempt_at, publish_error, published_destinations from events where id = $1`, event.ID,
		).Scan(&attempts, &nextAttempt, &failure, &destinations); err != nil {
			t.Fatalf("read event %d: %v", event.ID, err)
		}
		if attempts != 0 || nextAttempt != nil || failure != nil {
			t.Fatalf("event %d: attempt_count %d, next_attempt_at %v, publish_error %v, want nothing scheduled by a process without the hold",
				event.ID, attempts, nextAttempt, failure)
		}
		if !slices.Equal(destinations, []string{"notifications.dispatch.issue.T-2.message.created"}) {
			t.Fatalf("event %d recorded %v, want the topic it reached", event.ID, destinations)
		}
	}
}
