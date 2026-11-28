package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/session"
	envoystore "github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// busyPoolSize is the shared pool's size in the busy case. A full pool is what the case needs, of
// any size; filling one of production's store.sharedPoolSize would hold that many server
// connections while the other packages' tests share the server, past a default Postgres's
// hundred.
const busyPoolSize = 8

// busySharedPool holds every connection in the shared pool until the test ends.
// The outbox pool derived from it must make progress despite there being no
// capacity in the pool the API uses.
func busySharedPool(t *testing.T, database *store.Store) *store.Store {
	t.Helper()
	config := database.Pool.Config().Copy()
	config.MaxConns = busyPoolSize
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open the busy shared pool: %v", err)
	}
	busy := &store.Store{Pool: store.NewPool(pool)}
	t.Cleanup(func() {
		testwait.Returns(t, "the busy pool's connections to come back and its pool to close", busy.Pool.Close)
	})
	for range busyPoolSize {
		conn, err := busy.Pool.Acquire(testwait.Context(t))
		if err != nil {
			t.Fatalf("occupy the shared pool: %v", err)
		}
		t.Cleanup(conn.Release)
	}
	return busy
}

// seedThroughputBacklog appends count events across issues issues, each routed to role:reviewer,
// alternating a plain human message (its issue topic and the role route) and a human reply to a
// session's message (those and the session's own topic, after a lookup of the parent's author).
// It returns the events in id order.
func seedThroughputBacklog(t *testing.T, database *store.Store, broker *events.Broker, issues, count int) []model.Event {
	t.Helper()
	keys, parents := seedLoadMessageIssues(t, database, issues)
	written := make([]model.Event, 0, count)
	for index := range count {
		written = append(written, loadMessage(index, keys[index%issues], parents[index%issues], index%2 == 1))
	}
	return appendEvents(t, database, broker, written...)
}

// seedCommentReplyBacklog appends count events across issues issues. Every second event is a
// person's reply in a comment thread, on one of issues/2 issues that a session (claimant-N) claimed
// and that have no route. Each such thread is on a passage of the issue's document, which session
// writer-N wrote, and holds the person's root comment and writer-N's reply, so the reply reaches
// its issue topic, the claimant's topic and writer-N's topic. The other events are as
// seedThroughputBacklog's, on the other issues/2 issues, each routed to role:reviewer: a plain
// human message and a human reply to a session's message in turn. It returns the events in id
// order.
func seedCommentReplyBacklog(t *testing.T, database *store.Store, broker *events.Broker, issues, count int) []model.Event {
	t.Helper()
	ctx := context.Background()
	half := issues / 2
	messageKeys, parents := seedLoadMessageIssues(t, database, half)
	person := model.Actor{Kind: "user", ID: "alice"}
	personJSON, err := json.Marshal(person)
	if err != nil {
		t.Fatalf("encode the person: %v", err)
	}
	commentKeys := make([]string, half)
	roots := make([]string, half)
	for index := range half {
		number := half + index + 1
		commentKeys[index] = fmt.Sprintf("TT-%d", number)
		claimant, err := json.Marshal(model.Actor{Kind: "session", ID: fmt.Sprintf("claimant-%d", index)})
		if err != nil {
			t.Fatalf("encode the claimant: %v", err)
		}
		if _, err := database.Pool.Exec(ctx, `
			insert into issues (key, project_key, number, title, created_by, rank, claimed_by, claimed_at)
			values ($1, 'TT', $2, 'Load', '{"kind":"user","id":"alice"}', 'U', $3, now())
		`, commentKeys[index], number, claimant); err != nil {
			t.Fatalf("create claimed issue %s: %v", commentKeys[index], err)
		}
		writer := model.Actor{Kind: "session", ID: fmt.Sprintf("writer-%d", index)}
		document := seedDocument(t, database, new(commentKeys[index]), fmt.Sprintf("spec-%d", index), writer, []model.Actor{writer})
		anchor, err := json.Marshal(anchorOn(document))
		if err != nil {
			t.Fatalf("encode the anchor: %v", err)
		}
		if err := database.Pool.QueryRow(ctx, `
			insert into comments (issue_key, author, body, anchor) values ($1, $2, 'On this passage.', $3)
			returning id::text
		`, commentKeys[index], personJSON, anchor).Scan(&roots[index]); err != nil {
			t.Fatalf("create the thread root on %s: %v", commentKeys[index], err)
		}
		seedComment(t, database, commentKeys[index], writer, "Done.", &roots[index])
	}
	written := make([]model.Event, 0, count)
	replies := make([]loadComment, count/2)
	for i := range replies {
		thread := i % half
		replies[i] = loadComment{key: commentKeys[thread], root: new(roots[thread])}
	}
	commentIDs := seedLoadComments(t, database, personJSON, replies)
	for index := range count {
		if index%2 == 1 {
			message := index / 2
			written = append(written, loadMessage(index, messageKeys[message%half], parents[message%half], message%2 == 1))
			continue
		}
		reply := index / 2
		written = append(written, commentEvent(replies[reply], commentIDs[reply], person))
	}
	return appendEvents(t, database, broker, written...)
}

// seedUnclaimedCommentBacklog inserts the project TT and issues issues TT-1 on, with no route and
// no claim, and appends count events across them: a person's comments, two on each issue in turn.
// The issue's primary document (artifacts.is_primary) was written by session writer-N, and holds a
// thread with the person's root comment and a reply from session replier-N. The first comment is
// on the issue's conversation and reaches its issue topic and writer-N's topic, through the query
// of the issues' primary documents. The second is a reply in the thread, which reaches its issue
// topic, writer-N's topic and replier-N's topic, through that query and the query of the threads'
// sessions. It returns the events in id order.
func seedUnclaimedCommentBacklog(t *testing.T, database *store.Store, broker *events.Broker, issues, count int) []model.Event {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `insert into projects (key, name, default_assignee) values ('TT', 'Test', 'alice')`); err != nil {
		t.Fatalf("create project: %v", err)
	}
	person := model.Actor{Kind: "user", ID: "alice"}
	personJSON, err := json.Marshal(person)
	if err != nil {
		t.Fatalf("encode the person: %v", err)
	}
	keys := make([]string, issues)
	roots := make([]string, issues)
	for index := range issues {
		number := index + 1
		keys[index] = fmt.Sprintf("TT-%d", number)
		if _, err := database.Pool.Exec(ctx, `
			insert into issues (key, project_key, number, title, created_by, rank)
			values ($1, 'TT', $2, 'Load', '{"kind":"user","id":"alice"}', 'U')
		`, keys[index], number); err != nil {
			t.Fatalf("create issue %s: %v", keys[index], err)
		}
		writer := model.Actor{Kind: "session", ID: fmt.Sprintf("writer-%d", index)}
		document := seedDocument(t, database, new(keys[index]), "spec", writer, []model.Actor{writer})
		if _, err := database.Pool.Exec(ctx, `update artifacts set is_primary = true where id = $1`, document); err != nil {
			t.Fatalf("make the document of %s primary: %v", keys[index], err)
		}
		anchor, err := json.Marshal(anchorOn(document))
		if err != nil {
			t.Fatalf("encode the anchor: %v", err)
		}
		if err := database.Pool.QueryRow(ctx, `
			insert into comments (issue_key, author, body, anchor) values ($1, $2, 'On this passage.', $3)
			returning id::text
		`, keys[index], personJSON, anchor).Scan(&roots[index]); err != nil {
			t.Fatalf("create the thread root on %s: %v", keys[index], err)
		}
		seedComment(t, database, keys[index], model.Actor{Kind: "session", ID: fmt.Sprintf("replier-%d", index)}, "Done.", &roots[index])
	}
	comments := make([]loadComment, count)
	for index := range comments {
		issue := (index / 2) % issues
		comments[index].key = keys[issue]
		if index%2 == 1 {
			comments[index].root = new(roots[issue])
		}
	}
	commentIDs := seedLoadComments(t, database, personJSON, comments)
	written := make([]model.Event, count)
	for index, comment := range comments {
		written[index] = commentEvent(comment, commentIDs[index], person)
	}
	return appendEvents(t, database, broker, written...)
}

type loadComment struct {
	key  string
	root *string
}

// seedLoadComments inserts the load's comments in one round trip, in event order.
func seedLoadComments(t *testing.T, database *store.Store, author []byte, comments []loadComment) []string {
	t.Helper()
	var batch pgx.Batch
	ids := make([]string, len(comments))
	for i, comment := range comments {
		batch.Queue(`
			insert into comments (issue_key, author, body, reply_to) values ($1, $2, 'Load', $3)
			returning id::text
		`, comment.key, author, comment.root).QueryRow(func(row pgx.Row) error { return row.Scan(&ids[i]) })
	}
	if err := database.Pool.SendBatch(t.Context(), &batch).Close(); err != nil {
		t.Fatalf("seed load comments: %v", err)
	}
	return ids
}

func commentEvent(comment loadComment, id string, author model.Actor) model.Event {
	return model.Event{
		IssueKey: new(comment.key), Type: "comment.created", Actor: author,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{ID: id, IssueKey: new(comment.key), Author: author, Body: "Load", ReplyTo: comment.root},
		},
	}
}

// seedLoadMessageIssues inserts the project TT and its role-routed issues, each
// with a parent message from a different session.
func seedLoadMessageIssues(t *testing.T, database *store.Store, issues int) ([]string, []string) {
	t.Helper()
	ctx := testwait.Context(t)
	if _, err := database.Pool.Exec(ctx, `insert into projects (key, name, default_assignee) values ('TT', 'Test', 'alice')`); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, route, created_by, rank)
		select 'TT-' || n, 'TT', n, 'Load', 'role:reviewer', '{"kind":"user","id":"alice"}', 'U'
		from generate_series(1, $1::int) n
	`, issues); err != nil {
		t.Fatalf("create routed issues: %v", err)
	}
	rows, err := database.Pool.Query(ctx, `
		with parents as (
			insert into messages (issue_key, author, body)
			select key, jsonb_build_object('kind', 'session', 'id', 'agent-' || (number - 1)), 'Status?'
			from issues
			returning issue_key, id
		)
		select p.issue_key, p.id::text from parents p join issues i on i.key = p.issue_key order by i.number
	`)
	if err != nil {
		t.Fatalf("insert parent messages: %v", err)
	}
	defer rows.Close()
	keys, parents := make([]string, 0, issues), make([]string, 0, issues)
	for rows.Next() {
		var key, parent string
		if err := rows.Scan(&key, &parent); err != nil {
			t.Fatalf("read parent message: %v", err)
		}
		keys, parents = append(keys, key), append(parents, parent)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read parent messages: %v", err)
	}
	if len(parents) != issues {
		t.Fatalf("seeded %d parent messages, want one per issue (%d)", len(parents), issues)
	}
	return keys, parents
}

// loadMessage is a human message on key's issue, numbered index, and a reply to
// the session's message parent when reply is set.
func loadMessage(index int, key, parent string, reply bool) model.Event {
	message := model.Message{ID: fmt.Sprintf("message-%d", index), IssueKey: new(key), Body: "Load"}
	if reply {
		message.InReplyTo = new(parent)
	}
	return model.Event{
		IssueKey: new(key), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.MessageEventPayload{Message: message},
	}
}

// unpublishedCount is how many of database's events are not yet marked published.
func unpublishedCount(t *testing.T, database *store.Store) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from events where published_at is null`).Scan(&count); err != nil {
		t.Fatalf("count unpublished events: %v", err)
	}
	return count
}

// streamMessages reads every message the notification stream holds, in stream order: the one walk
// of the stream the checks of what it holds share (streamOrder, assertKeptLaneInOrder).
func streamMessages(t *testing.T, conn *natsgo.Conn) []*natsgo.RawStreamMsg {
	t.Helper()
	js, err := conn.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	info, err := js.StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	if info.State.Msgs == 0 {
		return nil
	}
	// Read the snapshot in one ordered delivery instead of one request per
	// sequence. Check every sequence; a gap must still fail the read.
	sub, err := js.SubscribeSync(">", natsgo.BindStream(bus.Stream), natsgo.OrderedConsumer(), natsgo.StartSequence(info.State.FirstSeq))
	if err != nil {
		t.Fatalf("subscribe to the stream snapshot: %v", err)
	}
	defer func() {
		if err := sub.Unsubscribe(); err != nil {
			t.Errorf("unsubscribe from the stream snapshot: %v", err)
		}
	}()
	ctx := testwait.Context(t)
	messages := make([]*natsgo.RawStreamMsg, 0, info.State.Msgs)
	for sequence := info.State.FirstSeq; sequence <= info.State.LastSeq; sequence++ {
		msg, err := sub.NextMsgWithContext(ctx)
		if err != nil {
			t.Fatalf("read stream message %d: %v", sequence, err)
		}
		meta, err := msg.Metadata()
		if err != nil {
			t.Fatalf("read metadata for stream message %d: %v", sequence, err)
		}
		if meta.Sequence.Stream != sequence {
			t.Fatalf("stream sequence = %d, want %d", meta.Sequence.Stream, sequence)
		}
		messages = append(messages, &natsgo.RawStreamMsg{
			Subject: msg.Subject, Sequence: sequence, Header: msg.Header, Data: msg.Data, Time: meta.Timestamp,
		})
	}
	return messages
}

// streamOrder is streamMessages read as the dispatch event id each message carries, grouped by
// subject, and the ids of all of them in stream order.
func streamOrder(t *testing.T, conn *natsgo.Conn) (map[string][]int64, []int64) {
	t.Helper()
	bySubject := map[string][]int64{}
	var all []int64
	for _, message := range streamMessages(t, conn) {
		id := envelopeEventID(t, message.Data)
		bySubject[message.Subject] = append(bySubject[message.Subject], id)
		all = append(all, id)
	}
	return bySubject, all
}

// envelopeEventID is the dispatch event id data's envelope carries (sourceEventID).
func envelopeEventID(t *testing.T, data []byte) int64 {
	t.Helper()
	var envelope contracts.Envelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("decode an envelope: %v", err)
	}
	return sourceEventID(t, envelope)
}

// sourceEventID is the dispatch event id envelope was made of, for a caller that has decoded it
// already.
func sourceEventID(t *testing.T, envelope contracts.Envelope) int64 {
	t.Helper()
	id, err := strconv.ParseInt(envelope.SourceEventID, 10, 64)
	if err != nil {
		t.Fatalf("envelope source event id %q: %v", envelope.SourceEventID, err)
	}
	return id
}

// ascending reports the first position at which ids decreases, or -1.
func firstDescent(ids []int64) int {
	for index := 1; index < len(ids); index++ {
		if ids[index] < ids[index-1] {
			return index
		}
	}
	return -1
}

// throughputHolder is the session that holds role:reviewer, every load issue's route, in the
// throughput rig.
const throughputHolder = "ses_reviewer"

// throughputRig is a real NATS server with its notification stream, the deployed bus client
// reaching it through a latency proxy, a direct connection that reads what arrived and on which
// role:reviewer's self-subscribed holder takes every forward and answers none, and a migrated
// database whose shared pool is kept busy.
type throughputRig struct {
	database *store.Store
	client   *bus.Client
	reader   *natsgo.Conn
	broker   *events.Broker
	clock    *clocktest.Fake
	// role is every forward the role's holder received, in arrival order, and marks the markers
	// it took (markers.await).
	roleMu sync.Mutex
	role   []int64
	marks  markers
}

// outstandingSends counts sends across the windows of one drain. In the backlog cases it
// withholds their answers from the outbox until the outstanding count reaches the floor.
type outstandingSends struct {
	Publisher
	mu          sync.Mutex
	outstanding int
	peak        int
	floor       int
	ready       chan struct{}
	stop        <-chan struct{}
}

// WindowUntil wraps the real window, so every send of it is counted.
func (c *outstandingSends) WindowUntil(deadline func() time.Time) bus.Sender {
	return outstandingWindow{Sender: c.Publisher.WindowUntil(deadline), sends: c}
}

// began counts one more send outstanding and keeps the peak.
func (c *outstandingSends) began() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.outstanding++
	if c.outstanding > c.peak {
		c.peak = c.outstanding
		if c.peak == c.floor {
			close(c.ready)
		}
	}
}

// answered counts one fewer send outstanding.
func (c *outstandingSends) answered() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.outstanding--
}

// peakOutstanding is the most sends that were outstanding at any one moment so far.
func (c *outstandingSends) peakOutstanding() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.peak
}

// outstandingWindow counts sends and relays their real answers after the gate opens.
// Probe goes straight through, as Connected and ConnectionChanged do on the publisher.
type outstandingWindow struct {
	bus.Sender
	sends *outstandingSends
}

func (s outstandingWindow) PublishAsync(item contracts.Envelope) <-chan error {
	return s.count(s.Sender.PublishAsync(item))
}

func (s outstandingWindow) PublishStoredAsync(subject string, item contracts.Envelope, msgID string) <-chan error {
	return s.count(s.Sender.PublishStoredAsync(subject, item, msgID))
}

func (s outstandingWindow) Request(subject string, item contracts.Envelope, opts bus.RequestOptions) <-chan error {
	return s.count(s.Sender.Request(subject, item, opts))
}

// count marks a send outstanding until it can relay the answer. It waits for ready before
// taking the answer, and abandons the relay if stop wins either wait.
func (s outstandingWindow) count(answer <-chan error) <-chan error {
	s.sends.began()
	relayed := make(chan error, 1)
	go func() {
		select {
		case <-s.sends.ready:
		case <-s.sends.stop:
			return
		}
		select {
		case err := <-answer:
			s.sends.answered()
			relayed <- err
		case <-s.sends.stop:
		}
	}()
	return relayed
}

// proxyChunkClock signals the driver as soon as the proxy arms a chunk's delay.
type proxyChunkClock struct {
	*clocktest.Fake
	armed chan struct{}
}

func (c proxyChunkClock) Sleep(ctx context.Context, d time.Duration) error {
	timer := c.NewTimer(d)
	defer timer.Stop()
	select {
	case c.armed <- struct{}{}:
	default:
	}
	select {
	case <-timer.C():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// runProxyClock drains the armed chunk delays whenever the proxy signals, with the
// shared wait's poll as a fallback. The test goroutine joins the driver at cleanup.
func runProxyClock(t *testing.T, c proxyChunkClock) {
	t.Helper()
	stop, done := make(chan struct{}), make(chan struct{})
	wake := notify(c.armed)
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if !fireNext(c.Fake) {
				testwait.Within(c.armed, testwait.Poll)
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		wake()
		testwait.Receive(t, "the proxy clock driver returning", done)
	})
}

func newThroughputRig(t *testing.T, rtt time.Duration) *throughputRig {
	t.Helper()
	_, url := testnats.Start(t)
	chunks := proxyChunkClock{Fake: clocktest.New(time.Time{}), armed: make(chan struct{}, 1)}
	proxied := testnats.StartProxy(t, url, testnats.ProxyLatency(rtt), testnats.ProxyClock(chunks)).URL()
	runProxyClock(t, chunks)
	client, err := bus.ConnectOwningStream([]string{proxied})
	if err != nil {
		t.Fatalf("connect the outbox's client through the latency proxy: %v", err)
	}
	t.Cleanup(func() { bustest.Close(t, client) })
	reader := testnats.Connect(t, url)
	t.Cleanup(reader.Close)
	rig := &throughputRig{database: storetest.Open(t), client: client, reader: reader, broker: events.NewBroker(), clock: chunks.Fake}
	if _, err := reader.Subscribe(contracts.LegacyAgentSubject(throughputHolder), func(message *natsgo.Msg) {
		if rig.marks.took(message) {
			return
		}
		id := envelopeEventID(t, message.Data)
		rig.roleMu.Lock()
		rig.role = append(rig.role, id)
		rig.roleMu.Unlock()
	}); err != nil {
		t.Fatalf("subscribe the role's holder: %v", err)
	}
	flushSubscriber(t, reader)
	seedRoleHolders(t, reader, map[string]string{"reviewer": throughputHolder})
	return rig
}

// seedRoleHolders writes live self-subscribed holders directly to the buckets
// the outbox reads. It does not start the listener's registry watchers.
func seedRoleHolders(t *testing.T, conn *natsgo.Conn, holders map[string]string) {
	t.Helper()
	js, err := conn.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("open JetStream for the role fixture: %v", err)
	}
	ctx := testwait.Context(t)
	stamp := fixtureTime.UnixMilli()
	claims, err := bus.EnsureKeyValue(ctx, conn, js, &natsgo.KeyValueConfig{Bucket: bus.RoleClaimBucket, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("open the role claim bucket: %v", err)
	}
	sessions, err := bus.EnsureKeyValue(ctx, conn, js, &natsgo.KeyValueConfig{Bucket: bus.SessionBucket, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("open the session bucket: %v", err)
	}
	put := func(kv bus.KeyValue, key string, entry any) {
		t.Helper()
		value, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("encode %q: %v", key, err)
		}
		if _, err := kv.Put(key, value); err != nil {
			t.Fatalf("write %q: %v", key, err)
		}
	}
	for role, holder := range holders {
		put(claims, envoystore.RoleClaimKeyOf(role), envoystore.RoleClaim{
			HolderSessionID: holder, ClaimedAt: stamp, RenewedAt: stamp,
		})
		put(sessions, holder, session.SessionEntry{
			SessionID: holder, MachineID: "test-machine", UpdatedAt: stamp, SelfSubscribed: true,
		})
	}
}

func (r *throughputRig) roleArrivals() []int64 {
	r.roleMu.Lock()
	defer r.roleMu.Unlock()
	return append([]int64(nil), r.role...)
}

// The outbox sends while its earlier sends are unanswered, which is what lets it publish a
// backlog at well above the rate Dispatch creates events under load (2.3 to 4.5 a second in
// production) when every NATS round trip carries a remote hub's latency and the API keeps the
// shared database pool busy, and it keeps the order consumers rely on: each issue's events reach
// each of its destinations in id order - its subject on the stream, a session's own subject, the
// role's holder, forwarded to and acknowledged by the stream (the Legion daemon fences an issue's
// facts on their sequence and drops one older than the last it applied). Different issues' events
// interleave as their sends' answers allow. It does so for a backlog of messages, for one whose
// half are a person's replies in comment threads, which take the issue's claimant and a query of
// the threads' sessions, and for one of a person's comments on issues with no route and no claim,
// which take a query of the issues' primary documents' sessions and, for a reply, of the threads'
// sessions. The destinations each event type reached are logged for comparison; the count of
// destinations is checked last, so a publisher that reaches fewer still reports its order.
//
// The verdict is how many sends the outbox holds outstanding at once. The gate releases answers
// at minOutstanding, regardless of machine load. The rate and drain time are logged on the proxy
// clock for each case. They are not wall-time measurements or verdicts (see THROUGHPUT.md).
//
// The burst cases put a batch of messages on one issue, the shape whose events share every
// destination, so each destination takes them one round trip after another, and log the drain
// time: from the outbox's start until every event is marked published. One issue is one chain, so
// they hold only a few sends outstanding and reach no floor; the drain time is the reading.
func TestTheOutboxHoldsManySendsOutstandingUnderRemoteLatency(t *testing.T) {
	t.Parallel()
	const (
		backlogIssues = 20
		// Fill one claim and leave another round of events across the owners.
		backlogCount = batchSize + backlogIssues
		burstCount   = batchSize
		// minOutstanding is the fewest sends a correct drain holds outstanding at its peak, in
		// the non-burst cases. Measured three ways under 48 busy loops on a 2-CPU box on
		// 2026-11-09, with a backlog of 2 * batchSize. The product held 50 (messages), 55
		// (comment replies) and 60 (unclaimed comments), the same figure in each of three runs.
		// The product with concurrentBatches cut to 1 and the sends unchanged held 50, 55 and 60
		// too, so the reading is one batch's own overlap and not the scan's batches running side
		// by side. A drain patched to issue one chain and wait for its answer before the next
		// held 3 in every non-burst case. The floor sits between the two: 20 is 6.7 times the
		// serialized drain's 3. With the backlog at batchSize + backlogIssues, 120 non-burst
		// readings in two build Jobs on 4-CPU nodes on 2026-11-28 (the package run whole and the
		// row run by name) mostly read 50, 55 and 56 to 60 again; the lowest was 34, 1.7 times
		// the floor, and 12 of the 120 were under 45.
		minOutstanding = 20
	)
	for _, load := range []struct {
		name string
		seed func(t *testing.T, database *store.Store, broker *events.Broker, issues, count int) []model.Event
		// stream is how many messages the stream holds on the issue topics and the sessions' own
		// subjects once count events of the load are published, the role's forwards aside.
		stream func(count int) int
		// routed is how many of count events of the load take the role route, each forwarded to
		// the role's holder, whose agent subject the stream captures too.
		routed func(count int) int
		// burst is whether the load also runs as a single-issue burst.
		burst bool
	}{
		// Each event's issue topic, and the replies' author topic.
		{"messages", seedThroughputBacklog, func(count int) int { return count + count/2 }, func(count int) int { return count }, true},
		// Each comment reply's issue topic, its claimant's topic and its document writer's; each
		// message's issue topic, and the message replies' author topic.
		{"comment replies", seedCommentReplyBacklog, func(count int) int { return 3*count/2 + count/2 + count/4 }, func(count int) int { return count / 2 }, false},
		// Each comment's issue topic and its primary document writer's; each reply's thread
		// replier's.
		{"unclaimed comments", seedUnclaimedCommentBacklog, func(count int) int { return 2*count + count/2 }, func(int) int { return 0 }, false},
	} {
		// Each case has its own database and NATS server. Run them beside
		// one another so each still has real concurrent work around its drain.
		t.Run(load.name, func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct {
				rtt   time.Duration
				busy  bool
				burst bool
			}{
				{50 * time.Millisecond, false, false},
				{50 * time.Millisecond, true, false},
				{50 * time.Millisecond, false, true},
				{150 * time.Millisecond, false, false},
				{150 * time.Millisecond, true, false},
				{150 * time.Millisecond, false, true},
			} {
				if tc.burst && !load.burst {
					continue
				}
				rtt := tc.rtt
				shape := rtt.String() + " idle pool"
				if tc.busy {
					shape = rtt.String() + " busy pool"
				}
				issues, count := backlogIssues, backlogCount
				if tc.burst {
					shape = rtt.String() + " single-issue burst"
					issues, count = 1, burstCount
				}
				name := load.name + ", " + shape
				t.Run(shape, func(t *testing.T) {
					t.Parallel()
					rig := newThroughputRig(t, rtt)
					seeded := load.seed(t, rig.database, rig.broker, issues, count)
					outboxStore := rig.database
					if tc.busy {
						outboxStore = busySharedPool(t, rig.database)
					}

					started := rig.clock.Now()
					stopRelaying := make(chan struct{})
					sends := &outstandingSends{Publisher: rig.client, floor: minOutstanding, ready: make(chan struct{}), stop: stopRelaying}
					if tc.burst {
						sends.floor = 0
						close(sends.ready)
					}
					outbox := wiredOutboxJudgedBy(t, outboxStore, rig.client, sends, rig.broker, roles.Windows{Now: func() time.Time { return fixtureTime }})
					stop := runOn(t, outbox)
					defer stop()
					defer close(stopRelaying)
					if !tc.burst {
						testwait.Receive(t, fmt.Sprintf("%s: at least %d sends outstanding at the peak", name, minOutstanding), sends.ready)
					}
					waitFor(t, name+": every event published", func() bool { return unpublishedCount(t, rig.database) == 0 })
					elapsed := rig.clock.Now().Sub(started)
					rate := float64(count) / elapsed.Seconds()
					peak := sends.peakOutstanding()
					if tc.burst {
						t.Logf("%s: drained %d events on one issue in %s: %.1f events/s, %d sends outstanding at the peak", name, count, elapsed.Round(time.Millisecond), rate, peak)
					} else {
						t.Logf("%s: published %d events in %s: %.1f events/s, %d sends outstanding at the peak", name, count, elapsed.Round(time.Millisecond), rate, peak)
					}

					// One issue is one chain, so a burst holds a send or three at once whatever
					// the drain does, and reads nothing here.
					if !tc.burst && peak < minOutstanding {
						t.Fatalf("%s: %d sends outstanding at the peak, want %d or more: the outbox waited for each send's answer before it made the next, instead of sending while earlier sends were unanswered", name, peak, minOutstanding)
					}

					issueOf := map[int64]string{}
					for _, event := range seeded {
						issueOf[event.ID] = *event.IssueKey
					}
					bySubject, all := streamOrder(t, rig.reader)
					ownerSubjects := 0
					for subject, ids := range bySubject {
						if strings.HasPrefix(subject, "notifications.dispatch.issue.") {
							ownerSubjects++
						}
						// The holder's subject carries every issue's forwards, interleaved as their
						// acknowledgements allow: the order is each issue's.
						byIssue := map[string][]int64{}
						for _, id := range ids {
							byIssue[issueOf[id]] = append(byIssue[issueOf[id]], id)
						}
						for issue, issueIDs := range byIssue {
							if at := firstDescent(issueIDs); at >= 0 {
								t.Fatalf("subject %s holds %s's event %d after its event %d: %v", subject, issue, issueIDs[at], issueIDs[at-1], issueIDs)
							}
						}
					}
					if ownerSubjects != issues {
						t.Fatalf("the stream holds %d issue subjects, want %d", ownerSubjects, issues)
					}
					// Each event is marked published once the stream acknowledged its sends, and the
					// holder is handed its forward on a connection of its own, which can take the
					// last of them just after that acknowledgement on a busy machine: the holder is
					// read once it has taken every forward the outbox sent it (markers.await).
					rig.marks.await(t, rig.client, contracts.LegacyAgentSubject(throughputHolder))
					routed := load.routed(count)
					roles := rig.roleArrivals()
					arrivals := map[string][]int64{}
					for _, id := range roles {
						arrivals[issueOf[id]] = append(arrivals[issueOf[id]], id)
					}
					for issue, ids := range arrivals {
						if at := firstDescent(ids); at >= 0 {
							t.Fatalf("the role's holder received %s's event %d after its event %d", issue, ids[at], ids[at-1])
						}
					}
					logDestinations(t, name, seeded, all)
					if want := load.stream(count) + routed; len(all) != want {
						t.Fatalf("the stream holds %d messages, want %d", len(all), want)
					}
					if len(roles) != routed {
						t.Fatalf("the role's holder received %d forwards, want one per routed event: %d", len(roles), routed)
					}
				})
			}
		})
	}
}

// logDestinations logs, for each event type of seeded, how many destinations its events reached
// on average: the stream messages that carry them, the role's forwards among them, since the
// stream captures the holder's agent subject. A reply in a comment thread counts apart from a
// comment that starts one.
func logDestinations(t *testing.T, name string, seeded []model.Event, stream []int64) {
	t.Helper()
	types := make(map[int64]string, len(seeded))
	counts := map[string]int{}
	for _, event := range seeded {
		eventType := event.Type
		if payload, ok := event.Payload.(model.CommentEventPayload); ok && payload.Comment.ReplyTo != nil {
			eventType += " reply"
		}
		types[event.ID] = eventType
		counts[eventType]++
	}
	destinations := map[string]int{}
	for _, id := range stream {
		destinations[types[id]]++
	}
	for _, eventType := range slices.Sorted(maps.Keys(counts)) {
		total := counts[eventType]
		t.Logf("%s: %d %s events reached %d destinations: %.2f per event", name, total, eventType, destinations[eventType], float64(destinations[eventType])/float64(total))
	}
}

// runOutbox runs the outbox as cmd/dispatch wires it, on database's outbox pool, publishing through
// client and resolving role routes from the role and session buckets on client's connection, until
// the returned func stops it.
func runOutbox(t *testing.T, database *store.Store, client *bus.Client, broker *events.Broker) func() {
	t.Helper()
	return runOn(t, wiredOutbox(t, database, client, client, broker))
}

// wiredOutbox builds an outbox without running it, so a test can set its clock or wrap its publisher.
func wiredOutbox(t *testing.T, database *store.Store, client *bus.Client, publisher Publisher, broker *events.Broker) *Outbox {
	t.Helper()
	return wiredOutboxJudgedBy(t, database, client, publisher, broker, roles.Windows{})
}

// wiredOutboxJudgedBy is wiredOutbox whose role holders are judged by windows. A roleRig dates its
// registrations on roleRigNow, so a row on one passes roles.Windows{Now: roleRigNow}.
func wiredOutboxJudgedBy(t *testing.T, database *store.Store, client *bus.Client, publisher Publisher, broker *events.Broker, windows roles.Windows) *Outbox {
	t.Helper()
	pool, err := database.Pool.Outbox()
	if err != nil {
		t.Fatalf("open the outbox's pool: %v", err)
	}
	holders := roles.NewBuckets(client, windows)
	return New(Deps{Store: &store.Store{Pool: pool}, Publisher: publisher, Broker: broker, Listen: database.Pool.Listener,
		RoleClaims: holders, Sessions: holders})
}
