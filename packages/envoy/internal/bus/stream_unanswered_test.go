package bus

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/testnats"
)

// unansweredJetStream is a JetStream context that leaves a start's first stream reads and first
// stream creates unanswered, as a server whose meta leader is changing leaves them: the request
// never reaches the server and the caller is given the deadline its wait ran out with. Once the
// unanswered sends are spent every later request reaches the real context, and the counts say how
// many sends each request took. With refuseCreate set it answers every create with that error
// instead, which is how a row tells a refusal from a lost request without asking a particular
// server version to refuse anything.
type unansweredJetStream struct {
	nats.JetStreamContext
	mu               sync.Mutex
	unansweredReads  int
	unansweredCreate int
	refuseCreate     error
	reads            int
	creates          int
}

// send records one send of a request and reports whether it reaches the server: it does not while
// unanswered sends are left, and each such send spends one.
func (js *unansweredJetStream) send(unanswered, sent *int) bool {
	js.mu.Lock()
	defer js.mu.Unlock()
	*sent++
	if *unanswered > 0 {
		*unanswered--
		return false
	}
	return true
}

func (js *unansweredJetStream) StreamInfo(stream string, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	if !js.send(&js.unansweredReads, &js.reads) {
		return nil, context.DeadlineExceeded
	}
	return js.JetStreamContext.StreamInfo(stream, opts...)
}

func (js *unansweredJetStream) AddStream(cfg *nats.StreamConfig, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	if !js.send(&js.unansweredCreate, &js.creates) {
		return nil, context.DeadlineExceeded
	}
	if refusal := js.refusal(); refusal != nil {
		return nil, refusal
	}
	return js.JetStreamContext.AddStream(cfg, opts...)
}

// refusal is the answer the context gives every create in place of the server's, or nil when the
// server answers them.
func (js *unansweredJetStream) refusal() error {
	js.mu.Lock()
	defer js.mu.Unlock()
	return js.refuseCreate
}

// sent is how many stream reads and how many creates the start has sent through the context.
func (js *unansweredJetStream) sent() (reads, creates int) {
	js.mu.Lock()
	defer js.mu.Unlock()
	return js.reads, js.creates
}

// A start whose stream read draws no answer sends it again and goes on, rather than ending the
// program with the bare deadline of the JetStream context's own wait.
func TestAStreamReadTheServerLeavesUnansweredIsSentAgainAndTheStartGoesOn(t *testing.T) {
	t.Parallel()
	js := streamTestJetStream(t)
	losing := &unansweredJetStream{JetStreamContext: js, unansweredReads: 1}
	cfg := parseConnectOptions([]ConnectOption(nil)).streamConfig()

	if _, err := ensureStreamWithConfig(countedReads(t.Context()), slog.Default(), losing, &cfg, capsAnUnsetStream, atStart); err != nil {
		t.Fatalf("a start whose first stream read drew no answer = %v, want it to go on", err)
	}

	if subjects := deployedStream(t, js).Config.Subjects; !slices.Equal(subjects, cfg.Subjects) {
		t.Fatalf("stream subjects = %v, want %v", subjects, cfg.Subjects)
	}
	if reads, _ := losing.sent(); reads != 2 {
		t.Fatalf("stream reads sent = %d, want 2: the lost one and the one that answered", reads)
	}
}

// A start whose create draws no answer sends it again and goes on. Nothing reached the server the
// first time, so the second send is the create that makes the stream, with its own configuration
// and the id it writes.
func TestACreateTheServerLeavesUnansweredIsSentAgainAndTheStartGoesOn(t *testing.T) {
	t.Parallel()
	js := streamTestJetStream(t)
	losing := &unansweredJetStream{JetStreamContext: js, unansweredCreate: 1}
	cfg := parseConnectOptions([]ConnectOption(nil)).streamConfig()

	if _, err := ensureStreamWithConfig(t.Context(), slog.Default(), losing, &cfg, capsAnUnsetStream, atStart); err != nil {
		t.Fatalf("a start whose first create drew no answer = %v, want it to go on", err)
	}

	info := deployedStream(t, js)
	if !slices.Equal(info.Config.Subjects, cfg.Subjects) {
		t.Fatalf("stream subjects = %v, want %v", info.Config.Subjects, cfg.Subjects)
	}
	if id := info.Config.Metadata[StreamIDMetadataKey]; id == "" {
		t.Fatalf("stream metadata = %v, want the %s the create writes", info.Config.Metadata, StreamIDMetadataKey)
	}
	if _, creates := losing.sent(); creates != 2 {
		t.Fatalf("creates sent = %d, want 2: the lost one and the one that made the stream", creates)
	}
}

// A create the server refuses is the server's answer, not a lost one, so the start returns it at
// once and sends it no second time. The refusal is the context's own and not a particular
// server's: which create a server refuses changes between versions (nats-server 2.10.29 refuses a
// three-copy create on one server and 2.15.1 lands it), and what this row holds is that any
// answer ends the sending.
func TestARefusedCreateIsSentOnce(t *testing.T) {
	t.Parallel()
	js := streamTestJetStream(t)
	refusal := &nats.APIError{Code: 400, ErrorCode: 10052, Description: "insufficient resources"}
	refused := &unansweredJetStream{JetStreamContext: js, refuseCreate: refusal}
	cfg := parseConnectOptions([]ConnectOption(nil)).streamConfig()

	_, err := ensureStreamWithConfig(t.Context(), slog.Default(), refused, &cfg, capsAnUnsetStream, atStart)
	var apiErr *nats.APIError
	if err == nil || !errors.As(err, &apiErr) || apiErr.ErrorCode != refusal.ErrorCode {
		t.Fatalf("a start whose create the server refused = %v, want that refusal back", err)
	}
	if _, creates := refused.sent(); creates != 1 {
		t.Fatalf("creates sent = %d, want 1: a refusal is the server's answer and is not sent again", creates)
	}
}

// A request the server never answers is given up on when the start's wait ends, and the start
// ends with an error naming the request and that end rather than sending for ever while its port
// answers /healthz "starting". The wait here is the caller's, a second; with none, it is the
// start's hang guard (startHangGuard).
//
// The three rows above lose one send each and so pass however long the wait is. This row is the
// one that holds the giving up itself: a start that kept sending past the end of its context never
// returns, and one that sent once does not send again.
func TestAStreamReadTheServerNeverAnswersIsGivenUpOn(t *testing.T) {
	js := streamTestJetStream(t)
	lost := &unansweredJetStream{JetStreamContext: js, unansweredReads: 1 << 30}
	cfg := parseConnectOptions([]ConnectOption(nil)).streamConfig()
	// A window, not a wait: the server never answers the read, and the start must give up when this
	// second ends, with context.DeadlineExceeded.
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	_, err := ensureStreamWithConfig(ctx, slog.Default(), lost, &cfg, capsAnUnsetStream, atStart)
	if !unanswered(err) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a start whose stream read is never answered = %v, want the deadline back: a start that goes on sending writes no fatal line of its own", err)
	}
	if want := Stream + " STREAM.INFO"; !strings.Contains(err.Error(), want) {
		t.Fatalf("a start whose stream read is never answered = %v, want it to name %q", err, want)
	}
	if reads, _ := lost.sent(); reads < 2 {
		t.Fatalf("stream reads sent = %d, want the read sent again while the start's wait lasted", reads)
	}
}

// A start that finds its bucket reads the bucket's stream for its copies, and that read is sent
// again when the server leaves it unanswered, as the lookup before it is: a member that is not
// the stream's leader leaves it unanswered while that leader is gone (sentAgain). Sent once, it
// ended the start with the bare deadline of one wait. The context here drops the first read of
// the stream the start makes itself (nats.go's lookup reads it through its own request, which
// this context does not see), on a bucket that already has the deployment's three copies.
func TestAStartThatFindsItsBucketSendsTheStreamReadAgain(t *testing.T) {
	t.Parallel()
	cluster := testnats.StartCluster(t, 3)
	conn, js := memberJetStream(t, cluster, 0)
	config := &nats.KeyValueConfig{Bucket: "envoy_found_read_test", Storage: nats.FileStorage, Replicas: 3}
	if _, err := EnsureKeyValue(t.Context(), conn, js, config); err != nil {
		t.Fatalf("create the three-copy bucket: %v", err)
	}
	lost := &unansweredJetStream{JetStreamContext: js, unansweredReads: 1}
	if _, err := EnsureKeyValue(t.Context(), conn, lost, config); err != nil {
		t.Fatalf("a start whose first read of its bucket's stream drew no answer = %v, want it to go on", err)
	}
	if reads, _ := lost.sent(); reads < 2 {
		t.Fatalf("stream reads sent = %d, want the lost one and the one sent again", reads)
	}
}

// The session registry's start sets the bucket's maximum age right after it opens the bucket, and
// fails if that call fails, so the call's read of the stream is sent again when the server leaves
// it unanswered, as every read a start makes is.
func TestTheMaxAgeReadTheServerLeavesUnansweredIsSentAgain(t *testing.T) {
	t.Parallel()
	conn := testnats.Connect(t, testnats.URL(t))
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(nats.MaxWait(RequestTimeout))
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	const bucket = "envoy_max_age_unanswered"
	if _, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage, TTL: time.Minute}); err != nil {
		t.Fatalf("create the bucket: %v", err)
	}
	t.Cleanup(func() { _ = js.DeleteKeyValue(bucket) })
	lost := &unansweredJetStream{JetStreamContext: js, unansweredReads: 1}
	age, err := SetKeyValueMaxAge(t.Context(), conn, lost, bucket, time.Hour)
	if err != nil || age != time.Hour {
		t.Fatalf("set the maximum age with the first read lost = %s, %v; want 1h0m0s and no error", age, err)
	}
	if reads, _ := lost.sent(); reads < 2 {
		t.Fatalf("stream reads sent = %d, want the lost one and the one sent again", reads)
	}
}
