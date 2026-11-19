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
	"github.com/sjawhar/envoy/internal/testwait"
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

// probeGate is a bus client whose windows report the first probe of their connection, which a
// batch makes once a send of it went unanswered (scanBatch): a test thaws a frozen server there,
// so the probe is answered and the unanswered send is the batch's finding.
type probeGate struct {
	*bus.Client
	once    sync.Once
	probing chan struct{}
}

func (c *probeGate) WindowUntil(deadline func() time.Time) bus.Sender {
	return probeGateSender{Sender: c.Client.WindowUntil(deadline), gate: c}
}

type probeGateSender struct {
	bus.Sender
	gate *probeGate
}

func (s probeGateSender) Probe(timeout time.Duration) error {
	s.gate.once.Do(func() { close(s.gate.probing) })
	return s.Sender.Probe(timeout)
}

// An issue's events each go to its topic family and to session S. S's destination fails for the
// first event, and the first event is backed off. Two later events written while it waits reach
// the topic family at once, but S only after the first one does, and S takes all three in order.
// S is reached two ways: as a session route on its own subject, which fails when the stream stops
// capturing agent subjects, so NATS answers its publish with no responders; and as the holder of
// the issue's role, whose first forward fails because the stream, capturing no agent subject,
// acknowledges nothing of it.
func TestABackedOffEventHoldsBackOnlyItsDestination(t *testing.T) {
	t.Run("a session route the stream does not take", holdsBackASessionRouteTheStreamRefuses)
	t.Run("a role holder whose forward the stream does not acknowledge", holdsBackARoleHolderWhoseForwardIsNotAcknowledged)
}

func holdsBackASessionRouteTheStreamRefuses(t *testing.T) {
	_, url := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{url})
	if err != nil {
		t.Fatalf("connect the outbox's client: %v", err)
	}
	t.Cleanup(client.Close)
	reader := testnats.Connect(t, url)
	js, err := reader.JetStream(testnats.MaxWait(t))
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
	testwait.Eventually(t, "the first event backed off for S's subject", func() bool {
		_, attempts := publishedDestinations(t, database, first.ID)
		return attempts > 0
	})
	if _, err := js.UpdateStream(&capturing); err != nil {
		t.Fatalf("let the stream capture agent subjects again: %v", err)
	}
	second, third := write("second"), write("third")

	testwait.Eventually(t, "all three events published", func() bool {
		return publishedAt(t, database, first.ID) != nil && publishedAt(t, database, second.ID) != nil &&
			publishedAt(t, database, third.ID) != nil
	})
	for _, event := range []model.Event{second, third} {
		if _, attempts := publishedDestinations(t, database, event.ID); attempts != 0 {
			t.Fatalf("event %d counted %d attempts, want none: it only waited on the first event", event.ID, attempts)
		}
	}
	topic := "notifications.dispatch.issue.T-1.message.created"
	session := contracts.LegacyAgentSubject("S")
	order := map[string][]int64{}
	sequence := map[string]uint64{}
	stream, err := js.StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	for seq := stream.State.FirstSeq; seq <= stream.State.LastSeq; seq++ {
		message, err := js.GetMsg(bus.Stream, seq)
		if err != nil {
			t.Fatalf("read stream message %d: %v", seq, err)
		}
		id := envelopeEventID(t, message.Data)
		order[message.Subject] = append(order[message.Subject], id)
		sequence[fmt.Sprintf("%s %d", message.Subject, id)] = seq
	}
	want := []int64{first.ID, second.ID, third.ID}
	if !slices.Equal(order[topic], want) {
		t.Fatalf("the topic family holds %v, want %v", order[topic], want)
	}
	if !slices.Equal(order[session], want) {
		t.Fatalf("S's subject holds %v, want %v: each event only after the one before it", order[session], want)
	}
	firstAtS := sequence[fmt.Sprintf("%s %d", session, first.ID)]
	for _, event := range []model.Event{second, third} {
		if at := sequence[fmt.Sprintf("%s %d", topic, event.ID)]; at > firstAtS {
			t.Fatalf("event %d reached the topic family at stream sequence %d, after the first event reached S at %d: it waited on S",
				event.ID, at, firstAtS)
		}
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
	toS := subscribeInOrder(t, rig.conn, rig.client.Connection(), contracts.LegacyAgentSubject(holder), func(msg *natsgo.Msg, _ <-chan struct{}) {
		id := envelopeEventID(t, msg.Data)
		mu.Lock()
		arrivals = append(arrivals, id)
		mu.Unlock()
	})
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
	testwait.Eventually(t, "the first event backed off for S", func() bool {
		_, attempts := publishedDestinations(t, rig.database, first.ID)
		return attempts > 0
	})
	second, third := write("second"), write("third")

	topic := "notifications.dispatch.issue.T-1.message.created"
	testwait.Eventually(t, "the later events on the topic family", func() bool {
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
	testwait.Eventually(t, "all three events published", func() bool {
		return publishedAt(t, rig.database, first.ID) != nil && publishedAt(t, rig.database, second.ID) != nil &&
			publishedAt(t, rig.database, third.ID) != nil
	})
	for _, event := range []model.Event{second, third} {
		if _, attempts := publishedDestinations(t, rig.database, event.ID); attempts != 0 {
			t.Fatalf("event %d counted %d attempts, want none: it only waited on the first event", event.ID, attempts)
		}
	}
	// Each event is marked published once the stream acknowledged its forward to S, and S is
	// handed the same message on its own connection, which can arrive just after that
	// acknowledgement on a busy machine: S is read once it has taken every forward sent to it
	// (orderedSubscription.settle).
	toS.settle(t)
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

// Session S holds issue T-1's route and follows T-1's topic family, so its relay hands it its own
// subject and T-1's topic family together, in stream order, and it drops a second copy of an event
// by its id: S takes T-1's events in the order sessionOrderRig.views reads them. Event 1 is an
// agent's message, which goes to the topic family alone, and the stream refuses its topic publish
// (no responders), so it backs off; event 2 is a person's message, which goes to the topic family
// and to S's own subject. S takes them in issue order, event 1 first, because event 2 reaches S's
// subject only once event 1 has reached the topic family: when event 2 is written while event 1
// waits out its back-off, and when both are due in one batch, where event 1's topic send is in
// flight beside it. The control sends both events to S's own subject, which refuses event 1 first,
// and S takes them in order there with or without the hold, which shows the rig reads S's view in
// stream order.
func TestASessionTakesAnOwnersEventsInOrderThroughAFailedTopicPublish(t *testing.T) {
	t.Run("a later event written while the first backs off", func(t *testing.T) {
		takesTheOwnersEventsInOrderThroughATopicFailure(t, false)
	})
	t.Run("both events due in one batch", func(t *testing.T) {
		takesTheOwnersEventsInOrderThroughATopicFailure(t, true)
	})
	t.Run("control both events on the session subject", keepsTheOrderOnTheSessionsOwnSubject)
}

// sessionOrderRig is an outbox's stream with a reader of it, the stream's configuration as it
// started, which refuse narrows and captureAgain puts back, and issue T-1, whose route is S.
type sessionOrderRig struct {
	client    *bus.Client
	reader    *natsgo.Conn
	js        natsgo.JetStreamContext
	capturing natsgo.StreamConfig
	database  *store.Store
	broker    *events.Broker
}

func newSessionOrderRig(t *testing.T) sessionOrderRig {
	t.Helper()
	_, url := testnats.Start(t)
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
	database := storetest.Open(t)
	route := "session:S"
	seedIssue(t, database, "T-1", &route)
	return sessionOrderRig{client: client, reader: reader, js: js, capturing: info.Config, database: database, broker: events.NewBroker()}
}

// refuse has the stream capture no subject under prefix, so NATS answers a publish there with no
// responders.
func (r sessionOrderRig) refuse(t *testing.T, prefix string) {
	t.Helper()
	narrowed := r.capturing
	narrowed.Subjects = slices.DeleteFunc(slices.Clone(r.capturing.Subjects), func(subject string) bool {
		return strings.HasPrefix(subject, prefix)
	})
	if len(narrowed.Subjects) == len(r.capturing.Subjects) {
		t.Fatalf("no stream subject is under %q: %v", prefix, r.capturing.Subjects)
	}
	if _, err := r.js.UpdateStream(&narrowed); err != nil {
		t.Fatalf("stop the stream capturing %s: %v", prefix, err)
	}
}

func (r sessionOrderRig) captureAgain(t *testing.T) {
	t.Helper()
	capturing := r.capturing
	if _, err := r.js.UpdateStream(&capturing); err != nil {
		t.Fatalf("let the stream capture every subject again: %v", err)
	}
}

func (r sessionOrderRig) write(t *testing.T, actor model.Actor, body string) model.Event {
	t.Helper()
	return appendEvent(t, r.database, r.broker, model.Event{
		IssueKey: new("T-1"), Type: "message.created", Actor: actor,
		Payload: model.Message{ID: uuid.NewString(), IssueKey: new("T-1"), Body: body},
	})
}

// settled reports whether a batch has written what event id came to: a hold-back or a back-off
// sets its next attempt, and a publish its published_at.
func (r sessionOrderRig) settled(t *testing.T, id int64) bool {
	t.Helper()
	var settled bool
	if err := r.database.Pool.QueryRow(context.Background(),
		`select next_attempt_at is not null or published_at is not null from events where id = $1`, id).Scan(&settled); err != nil {
		t.Fatalf("read event %d: %v", id, err)
	}
	return settled
}

// views is, in stream order, the event ids on each of session's subjects - its own and T-1's topic
// family - and session's view of them: both together, a repeated event id dropped.
func (r sessionOrderRig) views(t *testing.T, session string) (bySubject map[string][]int64, view []int64) {
	t.Helper()
	bySubject = map[string][]int64{}
	seen := map[int64]bool{}
	for _, message := range streamMessages(t, r.reader) {
		if !strings.HasPrefix(message.Subject, "notifications.dispatch.issue.T-1.") && message.Subject != session {
			continue
		}
		id := envelopeEventID(t, message.Data)
		bySubject[message.Subject] = append(bySubject[message.Subject], id)
		if !seen[id] {
			seen[id] = true
			view = append(view, id)
		}
	}
	return bySubject, view
}

// takesTheOwnersEventsInOrderThroughATopicFailure is the case. The stream refuses the topic family
// until a batch has settled the second event, so that batch makes or holds the second event's send
// to S while the first event's topic send has not gone. oneBatch writes both events before the
// outbox starts, so its first batch reads both due and the first event's topic send is in flight
// beside the second event's. Otherwise the second is written once the first backed off, with the
// outbox stopped while the first is set not due for an hour, so no retry of it writes a sooner
// back-off and the batch that reads the second finds the first not due. Then, with the outbox
// stopped again, both are made due and the stream captures every subject, and an outbox started
// afresh publishes both in its first scan.
func takesTheOwnersEventsInOrderThroughATopicFailure(t *testing.T, oneBatch bool) {
	rig := newSessionOrderRig(t)
	rig.refuse(t, dispatchTopicPrefix)
	agent, person := model.Actor{Kind: "session", ID: "agent-other"}, model.Actor{Kind: "user", ID: "alice"}
	first := rig.write(t, agent, "first, by an agent")
	var second model.Event
	if oneBatch {
		second = rig.write(t, person, "second, by a person")
	}
	stop := runOutbox(t, rig.database, rig.client, rig.broker)
	defer func() { stop() }()
	if !oneBatch {
		testwait.Eventually(t, "the first event backed off for the topic family", func() bool {
			_, attempts := publishedDestinations(t, rig.database, first.ID)
			return attempts > 0
		})
		stop()
		if _, err := rig.database.Pool.Exec(context.Background(),
			`update events set next_attempt_at = now() + interval '1 hour' where id = $1`, first.ID); err != nil {
			t.Fatalf("hold the first event not due: %v", err)
		}
		second = rig.write(t, person, "second, by a person")
		stop = runOutbox(t, rig.database, rig.client, rig.broker)
	}
	testwait.Eventually(t, "a batch settled the second event while the topic family was refused", func() bool {
		return rig.settled(t, second.ID)
	})
	stop()
	if _, err := rig.database.Pool.Exec(context.Background(),
		`update events set next_attempt_at = null where id = any($1)`, []int64{first.ID, second.ID}); err != nil {
		t.Fatalf("make both events due: %v", err)
	}
	rig.captureAgain(t)
	stop = runOutbox(t, rig.database, rig.client, rig.broker)
	testwait.Eventually(t, "both events published", func() bool {
		return publishedAt(t, rig.database, first.ID) != nil && publishedAt(t, rig.database, second.ID) != nil
	})

	topic := "notifications.dispatch.issue.T-1.message.created"
	session := contracts.LegacyAgentSubject("S")
	firstTo, firstAttempts := publishedDestinations(t, rig.database, first.ID)
	secondTo, secondAttempts := publishedDestinations(t, rig.database, second.ID)
	bySubject, view := rig.views(t, session)
	want := []int64{first.ID, second.ID}
	t.Logf("event %d went to %v after %d attempts; event %d went to %v after %d attempts", first.ID, firstTo, firstAttempts, second.ID, secondTo, secondAttempts)
	t.Logf("the stream by subject: %v; S takes T-1's events as %v, want %v", bySubject, view, want)
	if !slices.Equal(firstTo, []string{topic}) {
		t.Fatalf("the first event went to %v, want the topic family alone", firstTo)
	}
	if !slices.Contains(secondTo, session) || !slices.Contains(secondTo, topic) {
		t.Fatalf("the second event went to %v, want the topic family and S's own subject %s", secondTo, session)
	}
	if !slices.Equal(bySubject[topic], want) {
		t.Errorf("the topic family holds %v, want %v", bySubject[topic], want)
	}
	if !slices.Equal(bySubject[session], []int64{second.ID}) {
		t.Errorf("S's subject holds %v, want only the second event %d", bySubject[session], second.ID)
	}
	if !slices.Equal(view, want) {
		t.Errorf("S takes T-1's events as %v, want %v: the second event reached S's subject before the first reached the topic family", view, want)
	}
}

// keepsTheOrderOnTheSessionsOwnSubject is the control: both events go to S's own subject, which
// refuses the first, and the second waits behind it on that key.
func keepsTheOrderOnTheSessionsOwnSubject(t *testing.T) {
	rig := newSessionOrderRig(t)
	rig.refuse(t, contracts.AgentTopicPrefix)
	person := model.Actor{Kind: "user", ID: "alice"}
	first := rig.write(t, person, "first, by a person")
	stop := runOutbox(t, rig.database, rig.client, rig.broker)
	defer stop()
	testwait.Eventually(t, "the first event backed off for S's subject", func() bool {
		_, attempts := publishedDestinations(t, rig.database, first.ID)
		return attempts > 0
	})
	rig.captureAgain(t)
	second := rig.write(t, person, "second, by a person")
	testwait.Eventually(t, "both events published", func() bool {
		return publishedAt(t, rig.database, first.ID) != nil && publishedAt(t, rig.database, second.ID) != nil
	})

	session := contracts.LegacyAgentSubject("S")
	bySubject, view := rig.views(t, session)
	want := []int64{first.ID, second.ID}
	t.Logf("the stream by subject: %v; S takes T-1's events as %v, want %v", bySubject, view, want)
	if !slices.Equal(bySubject[session], want) {
		t.Errorf("S's subject holds %v, want %v", bySubject[session], want)
	}
	if !slices.Equal(view, want) {
		t.Errorf("S takes T-1's events as %v, want %v", view, want)
	}
}

// In one batch, an owner's send to a session is held back behind the owner's earlier topic send
// that failed (chainSend.after), whichever route makes it: the issue's session route, its
// claimant, or a role route's forward to the role's holder. The earlier events are agents'
// messages, which go to the topic family alone, and the last a person's, which goes to the topic
// family and to the session, all due in one scan's batch. The first event's topic send fails, so
// the last event's send to the session is never made: the first event backs off, and every later
// one is blocked behind it, with no attempt of its own and a next attempt, rather than sent or
// released. Once the topic family takes sends again, the retry reaches the session only after every
// earlier event reached the topic family. The last event's own two copies may go either way round:
// the order asked for is that each session takes an event once, a second copy dropped by its
// dedupe key, and takes one issue's events in order, so the session takes that event once
// whichever copy comes first and drops the other. Every copy the outbox sends of one event carries
// the event's event_id (dispatch-<event id>), which a host's dedupe (createDeliveryDedupe in
// envoy-client) drops a repeat by whatever its key. Its dedupe key names its event
// (contracts.DedupeKeyNamesTheUpstreamEvent) but is not always that one: it is dispatch-<event id>
// here, on the topic copy and on the session's, and on a role forward that key behind the forward
// prefix, while a new comment's copy for a session whose mention of it failed carries that
// mention's key (destinations.addSession), as do the forwards resolveRoleHolders keys by a
// holder's own mention. The last row has two earlier events, so the session send waits on the
// second's topic send, which the first's failure held back and which was never made.
func TestASendToASessionIsHeldBackBehindAFailedTopicSendOfItsBatch(t *testing.T) {
	const topic = "notifications.dispatch.issue.T-1.message.created"
	agent, person := model.Actor{Kind: "session", ID: "agent-other"}, model.Actor{Kind: "user", ID: "alice"}
	for _, tc := range []struct {
		name  string
		route *string
		claim *model.Actor
		// copyTopic is the topic the publisher records the session's copy under: its agent
		// subject, or the role's topic for a forward to the role's holder.
		copyTopic string
		earlier   int
	}{
		{name: "the issue's session route", route: new("session:S"), copyTopic: contracts.LegacyAgentSubject("S"), earlier: 1},
		{name: "the issue's claimant", claim: &model.Actor{Kind: "session", ID: "S"}, copyTopic: contracts.LegacyAgentSubject("S"), earlier: 1},
		{name: "a role's holder by its forward", route: new("role:reviewer"), copyTopic: roleTopic, earlier: 1},
		{name: "behind a topic send that was held back itself", route: new("session:S"), copyTopic: contracts.LegacyAgentSubject("S"), earlier: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := store.WithTransactionTracking(context.Background())
			database := storetest.Open(t)
			broker := events.NewBroker()
			seedIssue(t, database, "T-1", tc.route)
			if tc.claim != nil {
				claimIssue(t, database, "T-1", *tc.claim)
			}
			write := func(actor model.Actor, body string) model.Event {
				return appendEvent(t, database, broker, model.Event{
					IssueKey: new("T-1"), Type: "message.created", Actor: actor,
					Payload: model.Message{ID: uuid.NewString(), IssueKey: new("T-1"), Body: body},
				})
			}
			var written []model.Event
			for index := range tc.earlier {
				written = append(written, write(agent, fmt.Sprintf("earlier %d, by an agent", index+1)))
			}
			written = append(written, write(person, "last, by a person"))
			publisher := &recordingPublisher{failTopic: topic}
			outbox := holding(t, withHeldRoles(Deps{Store: database, Publisher: publisher}))
			outbox.scan(ctx, unfenced{})

			if got := topicsOf(publisher.all()); len(got) != 0 {
				t.Fatalf("the batch sent %v while the first event's topic send failed, want nothing: the last event's copy for the session went before the first event reached the topic family", got)
			}
			ids := make([]int64, 0, len(written))
			for index, event := range written {
				ids = append(ids, event.ID)
				want := 0
				if index == 0 {
					want = 1
				}
				if attempts, due := nextAttempt(t, database, event.ID); attempts != want || due == nil {
					t.Fatalf("event %d has %d attempts and next attempt %v, want %d and a next attempt: the first event backs off and each later one waits behind it, none released",
						event.ID, attempts, due, want)
				}
			}

			publisher.setFailTopic("")
			if _, err := database.Pool.Exec(ctx, `update events set next_attempt_at = null where id = any($1)`, ids); err != nil {
				t.Fatalf("make the retry due: %v", err)
			}
			outbox.scan(ctx, unfenced{})
			// The last event's own topic send and its session copy may go either way round: the
			// copies of one event have no order among themselves (planChains), and the session drops
			// whichever comes second as a repeat, which the checks after this one hold.
			earlier := slices.Repeat([]string{topic}, len(written)-1)
			want := append(slices.Clone(earlier), topic, tc.copyTopic)
			alsoWant := append(slices.Clone(earlier), tc.copyTopic, topic)
			sent := publisher.all()
			if got := topicsOf(sent); !slices.Equal(got, want) && !slices.Equal(got, alsoWant) {
				t.Fatalf("the retry sent %v, want %v or %v: the session's copy only once every earlier event reached the topic family", got, want, alsoWant)
			}
			last := sent[len(sent)-2:]
			if last[0].EventID != last[1].EventID || last[0].EventID != fmt.Sprintf("dispatch-%d", written[len(written)-1].ID) {
				t.Fatalf("the last event's two copies carry event ids %q and %q, want both dispatch-%d: a host drops a second copy by it",
					last[0].EventID, last[1].EventID, written[len(written)-1].ID)
			}
			for _, copied := range last {
				if !contracts.DedupeKeyNamesTheUpstreamEvent(copied) {
					t.Fatalf("the copy to %s carries dedupe key %q from source %q, which names no event: a host would not drop a second copy by it",
						copied.Topic, copied.DedupeKey, copied.Source)
				}
			}
			if tc.copyTopic != roleTopic && last[0].DedupeKey != last[1].DedupeKey {
				t.Fatalf("the last event's two copies carry dedupe keys %q and %q, want one key", last[0].DedupeKey, last[1].DedupeKey)
			}
			for _, event := range written {
				if publishedAt(t, database, event.ID) == nil {
					t.Fatalf("event %d is not published after the retry", event.ID)
				}
			}
		})
	}
}

// In one batch, an owner's send to a session waits for the answer to the owner's earlier topic
// send, rather than going beside it (chainSend.after). Event 1 goes to the topic family alone and
// event 2 to the topic family and to S's own subject, both due in one batch: the batch sends event
// 1's topic send alone, and holds event 2's topic send behind it on the topic family's chain and
// event 2's send to S until NATS answers it. Once it does, both go.
func TestASendToASessionWaitsForTheAnswerToAnEarlierTopicSendOfItsBatch(t *testing.T) {
	database := storetest.Open(t)
	broker := events.NewBroker()
	route := "session:S"
	seedIssue(t, database, "T-1", &route)
	write := func(actor model.Actor, body string) model.Event {
		return appendEvent(t, database, broker, model.Event{
			IssueKey: new("T-1"), Type: "message.created", Actor: actor,
			Payload: model.Message{ID: uuid.NewString(), IssueKey: new("T-1"), Body: body},
		})
	}
	first := write(model.Actor{Kind: "session", ID: "agent-other"}, "first, by an agent")
	second := write(model.Actor{Kind: "user", ID: "alice"}, "second, by a person")
	publisher := &heldPublisher{}
	stop := run(t, database, publisher, broker)
	defer stop()

	topic := "notifications.dispatch.issue.T-1.message.created"
	session := contracts.LegacyAgentSubject("S")
	testwait.Eventually(t, "the first event's topic sent", func() bool { return len(publisher.sent()) > 0 })
	// Every send past the first waits for an answer: the publisher records how many answers it had
	// given when each send was made (answeredBefore), which the checks below read.
	publisher.accept(0, 1)
	testwait.Eventually(t, "the second event's sends, once the first's topic send was answered", func() bool { return len(publisher.sent()) == 3 })
	got, after := publisher.sent(), publisher.answeredBefore()
	if got[0] != topic || after[1] < 1 || after[2] < 1 {
		t.Fatalf("sent %v with %v answers given before each, want the first event's topic send first and every later send made once it was answered: the second event's copy for S went while that send had no answer", got, after)
	}
	if got := got[1:]; !slices.Contains(got, topic) || !slices.Contains(got, session) {
		t.Fatalf("sent %v once the first event's topic send was answered, want the second event's to the topic family and to %s", got, session)
	}
	publisher.accept(1, 3)
	testwait.Eventually(t, "both events marked published", func() bool {
		return publishedAt(t, database, first.ID) != nil && publishedAt(t, database, second.ID) != nil
	})
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
		_, url := testnats.Start(t)
		// A client that owns no stream, on a server that has none: no stream captures the subject.
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
		server, url := testnats.Start(t)
		client, err := bus.ConnectOwningStream([]string{url})
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(client.Close)
		database := storetest.Open(t)
		event := seed(t, database)
		gate := &probeGate{Client: client, probing: make(chan struct{})}
		thaw := server.Freeze(t)
		thawed := make(chan struct{})
		go func() {
			defer close(thawed)
			// The batch probes its connection once its send went unanswered past the window's wait
			// for an acknowledgement, and the connection answers that probe once the server thaws.
			<-gate.probing
			thaw()
		}()
		holding(t, Deps{Store: database, Publisher: gate}).scan(context.Background(), unfenced{})
		testwait.Receive(t, "the server thawed for the batch's probe", thawed)
		backedOff(t, database, event, bus.ErrNotAcknowledged.Error())
	})

	t.Run("a lost connection", func(t *testing.T) {
		_, url := testnats.Start(t)
		proxy := testnats.StartProxy(t, url)
		client, err := bus.ConnectOwningStream([]string{proxy.URL()})
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(client.Close)
		database := storetest.Open(t)
		event := seed(t, database)
		proxy.Cut()
		testwait.Eventually(t, "the client noticing the cut", func() bool { return !client.Connected() })
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
		testwait.Eventually(t, "the client reconnecting", client.Connected)
		outbox.scan(context.Background(), unfenced{})
		if publishedAt(t, database, event.ID) == nil {
			t.Fatal("the released event was not published once NATS was back")
		}
	})
}

// countingQueryer is loadLookups' queryer counting every statement it is handed, whatever its SQL,
// and keeping each statement's SQL.
type countingQueryer struct {
	queryer
	statements int
	sql        map[string]struct{}
}

func (c *countingQueryer) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c.statements++
	if c.sql == nil {
		c.sql = map[string]struct{}{}
	}
	c.sql[sql] = struct{}{}
	return c.queryer.Query(ctx, sql, args...)
}

// A batch of events of every kind whose destinations need a lookup - replies to comments that
// mention their thread's author, resolutions of those threads, ask answers, replies to messages,
// comments on documents and a person's settled edits of documents, on an issue a session holds the
// claim of, and comments on an issue nobody routes or claims, whose primary document a session
// wrote - runs eight lookup statements, one per kind and two for the mention read, and publishes
// every one of them. The statements are the ask followers, the thread root authors a resolution
// reads, the message authors, the document authors, the mention deliveries a new comment whose
// mentions resolved to a session reads and the comment_fallback_copies rows that already own a
// destination of one (loadMentionDeliveries, its two statements), and #2366's two: the sessions of
// the threads the replies are in (loadCommentRecipients) and the sessions of the unclaimed issues'
// primary documents (loadPrimaryDocumentAuthors). The claimant costs no statement: the batch's own
// read carries the issue's claim (readBatch). A document's writers are one kind, whether a comment
// on it or an edit of it asks for them, so the edits add no statement. The statements are counted
// twice. Once where loadLookups hands each one to its queryer, for the batch the scan reads, so
// that count is of every statement one call of loadLookups runs, whatever its SQL: one more, or a
// kind the batch stops reading, fails it. And once among every statement a real scan of that
// batch runs (tracedStore), counting each whose SQL is one of the statements loadLookups ran,
// the comment_fallback_copies read among them: a scan that read the lookups more than once - per
// event, per owner, or once again - fails it. The second issue's events ask a kind the first's
// ask too (a reply to a message on it), so a read per owner runs that kind's statement twice.
func TestABatchThatAsksEveryLookupKindRunsEightLookupStatements(t *testing.T) {
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	claimIssue(t, database, "T-1", model.Actor{Kind: "session", ID: "claimant"})
	seedSecondIssue(t, database, "T-2")
	seedSpec(t, database, "T-2", model.Actor{Kind: "session", ID: "primary-author"})
	// Seven events of each index and the one reply on the second issue below: at most batchSize, so
	// one batch reads them all whatever batchSize is.
	const each = (batchSize - 1) / 7
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
	// A reply on the second issue to a message there: the message authors are a kind the first
	// issue's events ask too, so a scan that read the lookups once per owner would run it twice.
	otherMessage := seedMessage(t, database, "T-2", model.Actor{Kind: "session", ID: "other-sender"}, "Status?", nil)
	seeded = append(seeded, appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-2"), Type: "message.created", Actor: alice,
		Payload: model.Message{ID: uuid.NewString(), IssueKey: new("T-2"), Body: "Fine", InReplyTo: &otherMessage},
	}))

	// The batch the scan is about to read, read the same way, with its lookups run through a
	// queryer that counts them (resolveDestinations): the scan's own statements are many more than
	// its lookups, and no reading of their SQL tells a lookup from the rest.
	ctx := context.Background()
	reading := Deps{Store: database}
	reading.lanes = newLanes(func() {})
	pending, stopped, _, err := readBatch(ctx, reading)
	if err != nil || stopped {
		t.Fatalf("read the batch: stopped %v, %v", stopped, err)
	}
	if len(pending) != len(seeded) {
		t.Fatalf("the batch read %d events, want all %d", len(pending), len(seeded))
	}
	want := newWanted()
	for _, read := range pending {
		item, err := envelope(read.event, read.slug)
		if err != nil {
			t.Fatalf("build event %d's envelope: %v", read.event.ID, err)
		}
		_, _, _ = eventDestinations(ctx, reading, &batchEvent{pendingEvent: read, item: item}, want)
	}
	counted := &countingQueryer{queryer: database.Pool}
	if _, err := loadLookups(ctx, counted, want); err != nil {
		t.Fatalf("read the batch's lookups: %v", err)
	}
	if counted.statements != 8 {
		t.Fatalf("the batch of %d events ran %d lookup statements, want 8: one per kind and two for the mention read (loadLookups)", len(seeded), counted.statements)
	}
	t.Logf("the batch of %d events ran %d lookup statements", len(seeded), counted.statements)

	tracer := &statementTracer{}
	publisher := &recordingPublisher{}
	holding(t, Deps{Store: tracedStore(t, database, tracer), Publisher: publisher}).scan(ctx, unfenced{})
	scanned := 0
	for _, statement := range tracer.matching() {
		if _, ok := counted.sql[statement.sql]; ok {
			scanned++
		}
	}
	if _, ok := counted.sql[mentionFallbackHoldsQuery]; !ok {
		t.Fatalf("loadLookups ran no read of comment_fallback_copies (mentionFallbackHoldsQuery), so the scan's count below cannot see it")
	}
	if scanned != 8 {
		t.Fatalf("the scan of the batch ran %d lookup statements, want 8: it reads the batch's lookups once (resolveDestinations, loadLookups)", scanned)
	}
	t.Logf("the scan of the batch ran %d lookup statements", scanned)

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
		if item.Topic == contracts.LegacyAgentSubject("claimant") {
			claimant++
		}
	}
	// Each comment reply whose mention failed reaches its thread's author, each resolution its
	// thread root's author, each ask answer its two followers, each message reply its parent's
	// sender, each person's edit the edited document's author, and each comment on the unclaimed
	// issue its spec's author, and the one reply on it its parent's sender; and every reply,
	// resolution, answer, message and comment on the claimed issue - a person's - reaches the
	// claimant, which an edit, no claimant event, does not. Each comment on a document of the
	// claimed issue reaches that document's author no
	// more: on an issue a session claims, the credits of a document are no second reason to
	// deliver what the claimant already hears on its own topic (addAuthorRoutes). The read of
	// those credits is made all the same, so the statement count above is unchanged.
	if want := each * 5; claimant != want {
		t.Fatalf("the batch reached the claimant %d times, want %d", claimant, want)
	}
	if want := each/2 + each*6 + each*5 + 1; agents != want {
		t.Fatalf("the batch reached %d sessions' own subjects, want %d", agents, want)
	}
}

// One row no recipient read can take, among a batch's many ids of that read, fails only the event
// that asked it, and costs the batch a run of each half of the refused statement's ids, and of each
// refused half's halves down to that id alone (loadRecipients): at most 2*ceil(log2(n)) more
// statements for n ids, where a run of each id alone would cost n.
func TestOneBrokenRowOfARecipientReadCostsTheBatchAboutTwoStatementsPerHalving(t *testing.T) {
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
		want := []string{contracts.LegacyAgentSubject(fmt.Sprintf("author-%d", index))}
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
			// A backend reports its statistics when it goes idle and is forced to: each idle
			// connection of the pool flushes at the end of a statement of its own, before it answers
			// that statement, so the count read after holds every update those connections made.
			// Nothing else holds a connection of this pool while the row reads it.
			settled := func() int64 {
				for _, conn := range database.Pool.AcquireAllIdle(ctx) {
					_, err := conn.Exec(ctx, `select pg_stat_force_next_flush()`)
					conn.Release()
					if err != nil {
						t.Fatalf("flush a backend's statistics: %v", err)
					}
				}
				return updates()
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
	// The scan's publish left the old versions of the events it published dead, with their
	// entries in the indexes of events. The first claim to step over such an entry once no
	// snapshot can see its row marks it dead and reads its page again to do it, and whether a
	// snapshot can still see the row is judged from the oldest transaction on the whole server,
	// not on this database alone. On a Postgres other packages' tests share, a transaction open
	// in another database keeps the explains below from marking those entries, and the first
	// measured run pays for the marking instead: 38 blocks and then 37, or 39 and 37 at the larger
	// size. A vacuum removes the dead rows and their index entries outright, judging by this
	// database's own transactions, so the runs below measure the claim and none of that cleanup.
	if _, err := database.Pool.Exec(ctx, `vacuum (index_cleanup on) events`); err != nil {
		t.Fatalf("vacuum events: %v", err)
	}

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
	rowsRead, seqRead, indexBlocks, kept := claimReads(t, conn, claim)
	secondRowsRead, _, secondIndexBlocks, _ := claimReads(t, conn, claim)
	if rowsRead != secondRowsRead {
		t.Errorf("the claim read %d rows from events on the first run and %d on the second, want one figure twice: a measurement that reads pg_stat_xact_user_tables' counter rather than its difference across the claim gives the second run as twice the first",
			rowsRead, secondRowsRead)
	}
	// The blocks are as steady as the rows once the dead entries the scan's publish left are
	// vacuumed away (above): 37 on both runs of each size, in all 12 readings of 3 runs of the
	// healthy claim on 2026-11-09, and 56 and 113 on both runs of the mutated one.
	if indexBlocks != secondIndexBlocks {
		t.Errorf("the claim fetched %d blocks from the indexes of events on the first run and %d on the second, want one figure twice: the figure is the difference across the claim, and with no dead index entry left for the first run to mark, a second run on the same connection fetches the same blocks",
			indexBlocks, secondIndexBlocks)
	}
	if rowsRead == 0 && secondRowsRead == 0 {
		t.Errorf("the claim read 0 rows from events on both runs: track_counts must be on for pg_stat_xact_user_tables to count a row")
	}

	return claimCost{
		backlog:     backlog,
		rowsRead:    rowsRead,
		seqRead:     seqRead,
		indexBlocks: indexBlocks,
		kept:        kept,
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
