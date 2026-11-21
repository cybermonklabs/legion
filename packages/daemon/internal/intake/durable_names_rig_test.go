package intake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/consumernames"

	"github.com/sjawhar/legion/daemon/internal/testnats"
)

// The rig of the daemon's fresh-name rows (durable_names_test.go and, under the odatrace tag,
// durable_names_forced_order_test.go): the daemon's database as the operator's cutover leaves it,
// records as a killed daemon leaves them, the stream as the listener makes it, the daemon's JSON
// log, and a spy on every consumer request the server is sent.

// nameNotSettled is the message of the daemon's one ERROR line for a failed name step.
const nameNotSettled = "intake: a durable consumer's physical name could not be settled"

// identifiedStream creates ENVOY_NOTIFICATIONS on js as the listener does, carrying its stream
// identity (consumernames.StreamIdentityMetadataKey), which a last floor must name to count.
func identifiedStream(t testing.TB, js jetstream.JetStream) (jetstream.Stream, string) {
	t.Helper()
	id := consumernames.FreshName("stream")
	return testnats.CreateStream(t, js, jetstream.StreamConfig{Name: notificationStream, Subjects: []string{"notifications.>"},
		Metadata: map[string]string{consumernames.StreamIdentityMetadataKey: id}}), id
}

// freshJetStream is the package's server and its notification stream, with its identity.
func freshJetStream(t *testing.T) (jetstream.JetStream, jetstream.Stream, string) {
	t.Helper()
	js := testnats.JetStream(t)
	stream, id := identifiedStream(t, js)
	return js, stream, id
}

// selectionOf is logical's selection record, failing t when there is none.
func selectionOf(t testing.TB, pool *pgxpool.Pool, logical string) consumernames.Record {
	t.Helper()
	rec, _, err := consumernames.NewPGStore(pool).Read(context.Background(), logical)
	if err != nil {
		t.Fatalf("read the selection record of %s: %v", logical, err)
	}
	return rec
}

// putRecord writes logical's record over whatever it holds, as a daemon killed at some step
// leaves it.
func putRecord(t testing.TB, pool *pgxpool.Pool, logical string, rec consumernames.Record) {
	t.Helper()
	store := consumernames.NewPGStore(pool)
	_, revision, err := store.Read(context.Background(), logical)
	if err != nil && !errors.Is(err, consumernames.ErrNoRecord) {
		t.Fatalf("read the selection record of %s: %v", logical, err)
	}
	if _, err := store.Write(context.Background(), logical, rec, revision); err != nil {
		t.Fatalf("write the selection record of %s: %v", logical, err)
	}
}

// selectionRows counts intake_consumers' rows, the sentinel among them.
func selectionRows(t testing.TB, pool *pgxpool.Pool) int {
	t.Helper()
	var rows int
	if err := pool.QueryRow(context.Background(), `select count(*) from intake_consumers`).Scan(&rows); err != nil {
		t.Fatalf("count the selection rows: %v", err)
	}
	return rows
}

// heldInfo is name's info on stream, or nil when the stream holds no such consumer.
func heldInfo(t testing.TB, stream jetstream.Stream, name string) *jetstream.ConsumerInfo {
	t.Helper()
	consumer, err := stream.Consumer(context.Background(), name)
	if errors.Is(err, jetstream.ErrConsumerNotFound) {
		return nil
	}
	if err != nil {
		t.Fatalf("read consumer %s: %v", name, err)
	}
	return consumer.CachedInfo()
}

// consumerNames are the names of every consumer stream holds, sorted.
func consumerNames(t testing.TB, stream jetstream.Stream) []string {
	t.Helper()
	var names []string
	lister := stream.ConsumerNames(context.Background())
	for name := range lister.Name() {
		names = append(names, name)
	}
	if err := lister.Err(); err != nil {
		t.Fatalf("list the consumers: %v", err)
	}
	slices.Sort(names)
	return names
}

// fixedNames are the three names the daemon of project creates on fixed names.
func fixedNames(project string) []string {
	return []string{dispatchConsumerName(project), githubConsumerName(project), wakeConsumerName(project)}
}

// isFresh reports whether physical is a fresh name of logical: logical, '~' and a NUID.
func isFresh(logical, physical string) bool {
	rest, ok := strings.CutPrefix(physical, logical+"~")
	return ok && len("~"+rest) == len(consumernames.FreshName(""))
}

// consumerCalls is every JetStream consumer request the server is sent on the spy's account while
// it watches, read off the requests themselves: a create's action is the request body's, so a
// create and an update of one name are told apart.
type consumerCalls struct {
	conn     *nats.Conn
	received chan *nats.Msg
	mu       sync.Mutex
	creates  []string
	updates  []string
	deletes  []string
}

// watchConsumerCalls starts the spy on conn, which must be granted `$JS.API.CONSUMER.>`.
func watchConsumerCalls(t testing.TB, conn *nats.Conn) *consumerCalls {
	t.Helper()
	calls := &consumerCalls{conn: conn, received: make(chan *nats.Msg, 1<<14)}
	sub, err := conn.ChanSubscribe("$JS.API.CONSUMER.>", calls.received)
	if err != nil {
		t.Fatalf("watch the consumer requests: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the spy's subscription: %v", err)
	}
	return calls
}

// read takes every request the server has sent the spy so far: a flush's answer comes after every
// message the server sent before it, and the client's read loop has put each of those on the
// channel by then.
func (c *consumerCalls) read(t testing.TB) {
	t.Helper()
	if err := c.conn.Flush(); err != nil {
		t.Fatalf("flush the spy: %v", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		select {
		case msg := <-c.received:
			tokens := strings.Split(msg.Subject, ".")
			switch {
			case len(tokens) >= 6 && tokens[3] == "CREATE":
				var body struct {
					Action string `json:"action"`
				}
				_ = json.Unmarshal(msg.Data, &body)
				if body.Action == "update" {
					c.updates = append(c.updates, tokens[5])
				} else {
					c.creates = append(c.creates, tokens[5])
				}
			case len(tokens) >= 7 && tokens[3] == "DURABLE" && tokens[4] == "CREATE":
				c.creates = append(c.creates, tokens[6])
			case len(tokens) >= 6 && tokens[3] == "DELETE":
				c.deletes = append(c.deletes, tokens[5])
			}
		default:
			return
		}
	}
}

// created are the names the server was sent a create of, in order.
func (c *consumerCalls) created(t testing.TB) []string {
	t.Helper()
	c.read(t)
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.creates)
}

// touched are the names the server was sent a create, an update or a delete of.
func (c *consumerCalls) touched(t testing.TB) []string {
	t.Helper()
	c.read(t)
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Concat(c.creates, c.updates, c.deletes)
}

// deleted are the names the server was sent a delete of.
func (c *consumerCalls) deleted(t testing.TB) []string {
	t.Helper()
	c.read(t)
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.deletes)
}

// noNameCreatedTwice fails t when any name was sent to a create more than once, or when a fixed
// name of project was created, updated or deleted: on fresh names the daemon never meets one.
func (c *consumerCalls) noNameCreatedTwice(t testing.TB, project string) {
	t.Helper()
	created := c.created(t)
	seen := map[string]bool{}
	for _, name := range created {
		if seen[name] {
			t.Fatalf("%s was sent to the server's create twice: %v", name, created)
		}
		seen[name] = true
	}
	for _, name := range c.touched(t) {
		if slices.Contains(fixedNames(project), name) {
			t.Fatalf("the fixed name %s was sent to the server: creates %v", name, created)
		}
	}
}

// errorLines are the daemon's ERROR lines for a failed name step in logs, each decoded.
func errorLines(t testing.TB, logs *lockedBuffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for line := range strings.SplitSeq(logs.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var fields map[string]any
		if err := json.NewDecoder(bytes.NewReader([]byte(line))).Decode(&fields); err != nil {
			t.Fatalf("a log line is no JSON: %q: %v", line, err)
		}
		if fields["msg"] == nameNotSettled {
			lines = append(lines, fields)
		}
	}
	return lines
}

// oneErrorLine fails t unless logs hold exactly one ERROR line for a failed name step, naming
// logical and op, with exactly the line's fields, and returns it.
func oneErrorLine(t testing.TB, logs *lockedBuffer, logical string, op consumernames.Operation) map[string]any {
	t.Helper()
	lines := errorLines(t, logs)
	if len(lines) != 1 {
		t.Fatalf("%d ERROR lines %q, want one; logs:\n%s", len(lines), nameNotSettled, logs)
	}
	line := lines[0]
	keys := make([]string, 0, len(line))
	for key := range line {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	if want := []string{"consumer", "error", "level", "msg", "op", "physical", "time"}; !slices.Equal(keys, want) {
		t.Fatalf("the ERROR line's fields = %v, want %v: %v", keys, want, line)
	}
	if line["level"] != "ERROR" || line["consumer"] != logical || line["op"] != string(op) || line["error"] == "" {
		t.Fatalf("the ERROR line = %v, want level ERROR, consumer %s, op %s and an error", line, logical, op)
	}
	return line
}

// fetchSeqs pulls name's messages until none comes for a second, acknowledging each, and
// returns their stream sequences in delivery order.
func fetchSeqs(t testing.TB, stream jetstream.Stream, name string) []uint64 {
	t.Helper()
	consumer, err := stream.Consumer(context.Background(), name)
	if err != nil {
		t.Fatalf("bind %s: %v", name, err)
	}
	var seqs []uint64
	for {
		batch, err := consumer.Fetch(10, jetstream.FetchMaxWait(time.Second))
		if err != nil {
			t.Fatalf("fetch from %s: %v", name, err)
		}
		n := 0
		for msg := range batch.Messages() {
			n++
			metadata, err := msg.Metadata()
			if err != nil {
				t.Fatalf("read a message's metadata: %v", err)
			}
			seqs = append(seqs, metadata.Sequence.Stream)
			if err := msg.DoubleAck(context.Background()); err != nil {
				t.Fatalf("ack stream sequence %d: %v", metadata.Sequence.Stream, err)
			}
		}
		if err := batch.Error(); err != nil {
			t.Fatalf("fetch from %s: %v", name, err)
		}
		if n == 0 {
			return seqs
		}
	}
}

// seqRange is from to through, inclusive.
func seqRange(from, through uint64) []uint64 {
	var seqs []uint64
	for seq := from; seq <= through; seq++ {
		seqs = append(seqs, seq)
	}
	return seqs
}

// publishWakes publishes n wake reports and returns their stream sequences: the wake consumer
// filters on their subject alone.
func publishWakes(t testing.TB, js jetstream.JetStream, n int) []uint64 {
	t.Helper()
	var seqs []uint64
	for i := range n {
		ack, err := js.Publish(context.Background(), legionWakeSubject, fmt.Appendf(nil, `{"kind":"message","role":"r","session":"s-%d","at":1}`, i))
		if err != nil {
			t.Fatalf("publish wake %d: %v", i, err)
		}
		seqs = append(seqs, ack.Sequence)
	}
	return seqs
}

// operatorServer is the consumers of a stream as the operator's credential reaches them: the info
// and the delete the daemon's own grant does not hold. It stands in for the server side of
// `envoy-infra consumer-names cleanup`, whose command reads the daemon's records the same way.
type operatorServer struct{ stream jetstream.Stream }

func (s operatorServer) Info(ctx context.Context, name string) (*jetstream.ConsumerInfo, error) {
	consumer, err := s.stream.Consumer(ctx, name)
	if errors.Is(err, jetstream.ErrConsumerNotFound) {
		return nil, fmt.Errorf("%w: %s", consumernames.ErrNotFound, name)
	}
	if err != nil {
		return nil, err
	}
	return consumer.CachedInfo(), nil
}

func (s operatorServer) Delete(ctx context.Context, name string) error {
	if err := s.stream.DeleteConsumer(ctx, name); err != nil && !errors.Is(err, jetstream.ErrConsumerNotFound) {
		return err
	}
	return nil
}
