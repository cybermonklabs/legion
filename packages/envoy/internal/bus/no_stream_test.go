package bus

// A write no stream takes is sent again while the stream is there and the write's bound lasts
// (sentAgainWhileNoStream): a stream whose leader was just named answers "no response from
// stream" until that leader serves. The loop's own rows script the answers, with no server. The
// rest make "no responders" on one server, with no timing deciding it: the bucket's or the
// notification stream's stream deleted, or the notification stream moved off the subject a
// publish is sent on, which is a stream that is there and does not take the write, as one whose
// leader does not serve yet. A row that ends a context ends it from inside a send, at the send it
// names.

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// noStreamRowWait is the noStreamWait the rows below give a bucket handle: a hang guard for a row
// whose writes the server never takes, not a wait any passing row spends.
const noStreamRowWait = 30 * time.Second

// untilTheRowsEnd is a context that ends 30 s before the test binary's deadline: the bound of a
// row's wait for something that comes.
func untilTheRowsEnd(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testwait.Bounded(t, 24*time.Hour, 30*time.Second))
	t.Cleanup(cancel)
	return ctx
}

// scriptedWrite answers each send of a write as script says, in order, and nil once it is spent:
// each step is the error that send answers. gone answers whether the stream was found gone, and
// counts how often it was asked. No server is behind it.
type scriptedWrite struct {
	script []error
	sends  int
	gone   bool
	reads  int
}

func (s *scriptedWrite) send() (int, error) {
	s.sends++
	if s.sends <= len(s.script) {
		return s.sends, s.script[s.sends-1]
	}
	return s.sends, nil
}

func (s *scriptedWrite) streamGone(context.Context) bool {
	s.reads++
	return s.gone
}

// The loop sends a write again after each "no response from stream" while the stream is there,
// and returns the first other answer: here two such answers, then the write is taken.
func TestAWriteNoStreamTookIsSentAgainUntilAStreamTakesIt(t *testing.T) {
	write := &scriptedWrite{script: []error{nats.ErrNoStreamResponse, nats.ErrNoStreamResponse}}
	value, err := sentAgainWhileNoStream(untilTheRowsEnd(t), write.send, write.streamGone)
	if err != nil || value != 3 || write.sends != 3 || write.reads != 2 {
		t.Fatalf("a write answered no stream twice = %d, %v after %d sends and %d reads; want the third send taken, the stream read after each no-stream answer", value, err, write.sends, write.reads)
	}
}

// The loop returns at once, without sending again or reading the stream, an answer that is not
// "no response from stream": here a timeout, which may have reached a stream.
func TestAWriteAnsweredOtherwiseIsSentOnce(t *testing.T) {
	write := &scriptedWrite{script: []error{nats.ErrTimeout}}
	if _, err := sentAgainWhileNoStream(untilTheRowsEnd(t), write.send, write.streamGone); !errors.Is(err, nats.ErrTimeout) || write.sends != 1 || write.reads != 0 {
		t.Fatalf("a write that timed out = %v after %d sends and %d reads, want the timeout after one send and no read", err, write.sends, write.reads)
	}
}

// The loop returns "no response from stream" at once when the stream is gone: sending again could
// not reach it.
func TestAWriteToAStreamFoundGoneIsSentOnce(t *testing.T) {
	write := &scriptedWrite{script: []error{nats.ErrNoStreamResponse, nats.ErrNoStreamResponse}, gone: true}
	if _, err := sentAgainWhileNoStream(untilTheRowsEnd(t), write.send, write.streamGone); !errors.Is(err, nats.ErrNoStreamResponse) || write.sends != 1 {
		t.Fatalf("a write to a stream found gone = %v after %d sends, want nats.ErrNoStreamResponse after one send", err, write.sends)
	}
}

// When its context ends, the loop returns the last send's "no response from stream", not the
// context's end: that answer says nothing was stored.
func TestAWriteNoStreamTakesReturnsThatAnswerWhenItsContextEnds(t *testing.T) {
	write := &scriptedWrite{script: []error{nats.ErrNoStreamResponse}}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sentAgainWhileNoStream(ended, write.send, write.streamGone); !errors.Is(err, nats.ErrNoStreamResponse) || errors.Is(err, context.Canceled) || write.sends != 1 {
		t.Fatalf("a write no stream took under an ended context = %v after %d sends, want nats.ErrNoStreamResponse after one send", err, write.sends)
	}
}

// pastDeadline is a context whose deadline has passed while it has not ended, as a deadline's
// context is between its deadline and its timer, which fires late under load.
type pastDeadline struct{ context.Context }

func (pastDeadline) Deadline() (time.Time, bool) { return time.Now().Add(-time.Millisecond), true }

// The loop sends nothing again within noStreamMargin of its context's deadline, read from the
// deadline rather than from the context's end: here a context past its deadline that has not
// ended, after one "no response from stream".
func TestAWriteNoStreamTookIsNotSentAgainPastItsDeadline(t *testing.T) {
	write := &scriptedWrite{script: []error{nats.ErrNoStreamResponse}}
	ctx := pastDeadline{context.Background()}
	if ctx.Err() != nil {
		t.Fatalf("the past-deadline context has ended (%v), so this row holds nothing", ctx.Err())
	}
	if _, err := sentAgainWhileNoStream(ctx, write.send, write.streamGone); !errors.Is(err, nats.ErrNoStreamResponse) || write.sends != 1 {
		t.Fatalf("a write no stream took past its deadline = %v after %d sends, want nats.ErrNoStreamResponse after one send", err, write.sends)
	}
}

// nearDeadline is a context that has not ended and whose deadline, each time it is read after its
// first far reads, is 99 ms away: a context within noStreamMargin of its deadline however long
// the code reading it took. The first far reads answer a deadline an hour away.
type nearDeadline struct {
	context.Context
	far   int32
	reads atomic.Int32
}

func (c *nearDeadline) Deadline() (time.Time, bool) {
	if c.reads.Add(1) <= c.far {
		return time.Now().Add(time.Hour), true
	}
	return time.Now().Add(99 * time.Millisecond), true
}

// The loop sends nothing again within noStreamMargin of its context's deadline: here 99 ms away,
// before the pause, after one "no response from stream". A margin under 99 ms sends it again.
func TestAWriteNoStreamTookIsNotSentAgainWithinTheMarginOfItsDeadline(t *testing.T) {
	write := &scriptedWrite{script: []error{nats.ErrNoStreamResponse}}
	ctx := &nearDeadline{Context: context.Background()}
	if _, err := sentAgainWhileNoStream(ctx, write.send, write.streamGone); !errors.Is(err, nats.ErrNoStreamResponse) || write.sends != 1 {
		t.Fatalf("a write no stream took 99 ms before its deadline = %v after %d sends, want nats.ErrNoStreamResponse after one send", err, write.sends)
	}
}

// The loop reads its deadline again after the pause, and sends nothing again when the pause brought
// it within noStreamMargin: here the deadline is an hour away when read before the pause and 99 ms
// away when read after it.
func TestAWriteNoStreamTookIsNotSentAgainWhenThePauseBringsItsDeadlineNear(t *testing.T) {
	write := &scriptedWrite{script: []error{nats.ErrNoStreamResponse}}
	ctx := &nearDeadline{Context: context.Background(), far: 1}
	if _, err := sentAgainWhileNoStream(ctx, write.send, write.streamGone); !errors.Is(err, nats.ErrNoStreamResponse) || write.sends != 1 {
		t.Fatalf("a write no stream took whose pause brought its deadline within 99 ms = %v after %d sends, want nats.ErrNoStreamResponse after one send", err, write.sends)
	}
	if reads := ctx.reads.Load(); reads < 2 {
		t.Fatalf("the loop read its deadline %d times, want before and after the pause", reads)
	}
}

// The pause between two sends grows with each send and stops growing at 2 s: drawn at its top, it
// is 50 ms after the first send, then 250 ms, 500 ms, 1 s, 2 s and 2 s.
func TestThePauseBetweenSendsGrowsUpToTwoSeconds(t *testing.T) {
	top := func(n int64) int64 { return n - 1 }
	var got []time.Duration
	for sent := 1; sent <= 6; sent++ {
		got = append(got, noStreamPause(sent, top).Round(time.Millisecond))
	}
	want := []time.Duration{50 * time.Millisecond, 250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second, 2 * time.Second}
	if !slices.Equal(got, want) {
		t.Fatalf("the pauses at their top = %v, want %v", got, want)
	}
}

// countedLegacy is the legacy handle of a bucket, counting the writes sent through it. Its Put
// answers as put says when put is set, and sends through the real handle otherwise.
type countedLegacy struct {
	nats.KeyValue
	put   func(n int32, key string, value []byte) (uint64, error)
	sends atomic.Int32
	// conn is the connection the bucket's handles are on.
	conn *nats.Conn
}

func (l *countedLegacy) Put(key string, value []byte) (uint64, error) {
	n := l.sends.Add(1)
	if l.put != nil {
		return l.put(n, key, value)
	}
	return l.KeyValue.Put(key, value)
}

func (l *countedLegacy) Update(key string, value []byte, last uint64) (uint64, error) {
	l.sends.Add(1)
	return l.KeyValue.Update(key, value, last)
}

// countedBucket is a bucket on the test's own server, its handle built over a counting legacy
// handle (countedLegacy) with noStreamRowWait, and the account's JetStream context.
func countedBucket(t *testing.T, bucket string) (KeyValue, *countedLegacy, nats.JetStreamContext, *nats.KeyValueConfig) {
	t.Helper()
	conn := testnats.Connect(t, testnats.URL(t))
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(nats.MaxWait(RequestTimeout))
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	config := &nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage}
	legacy, err := js.CreateKeyValue(config)
	if err != nil {
		t.Fatalf("create the bucket: %v", err)
	}
	streams, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("open the jetstream context: %v", err)
	}
	watch, err := streams.KeyValue(untilTheRowsEnd(t), bucket)
	if err != nil {
		t.Fatalf("open the jetstream handle: %v", err)
	}
	counted := &countedLegacy{KeyValue: legacy, conn: conn}
	kv := NewKeyValue(counted, watch, streams)
	kv.noStreamWait = noStreamRowWait
	return kv, counted, js, config
}

// A put the server answers "no response from stream" while the bucket's stream is there is sent
// again and stored: here the stream is deleted, and made again inside the first send that draws
// that answer, before the handle reads it.
func TestAPutNoStreamTookIsSentAgainOnceTheStreamIsThere(t *testing.T) {
	kv, legacy, js, config := countedBucket(t, "envoy_no_stream_put_test")
	if err := js.DeleteStream("KV_" + config.Bucket); err != nil {
		t.Fatalf("delete the bucket's stream: %v", err)
	}
	var once sync.Once
	var recreated error
	legacy.put = func(_ int32, key string, value []byte) (uint64, error) {
		revision, err := legacy.KeyValue.Put(key, value)
		if errors.Is(err, nats.ErrNoStreamResponse) {
			once.Do(func() { _, recreated = js.CreateKeyValue(config) })
		}
		return revision, err
	}
	if _, err := kv.Put("key", []byte("value")); err != nil {
		t.Fatalf("a put no stream took, the stream then made again: %v (sends %d, recreate %v)", err, legacy.sends.Load(), recreated)
	}
	if recreated != nil || legacy.sends.Load() < 2 {
		t.Fatalf("the stream was made again with %v after %d sends; want it made and the put sent again", recreated, legacy.sends.Load())
	}
	entry, err := kv.Get("key")
	if err != nil || string(entry.Value()) != "value" {
		t.Fatalf("read the key back = %v, %v; want value", entry, err)
	}
}

// A put to a bucket whose stream is gone fails with nats.ErrNoStreamResponse after one send, as it
// did before writes were sent again: the read after the answer finds no stream.
func TestAPutToABucketWhoseStreamIsGoneIsSentOnce(t *testing.T) {
	kv, legacy, js, config := countedBucket(t, "envoy_no_stream_gone_test")
	if err := js.DeleteStream("KV_" + config.Bucket); err != nil {
		t.Fatalf("delete the bucket's stream: %v", err)
	}
	if _, err := kv.Put("key", []byte("value")); !errors.Is(err, nats.ErrNoStreamResponse) || legacy.sends.Load() != 1 {
		t.Fatalf("a put to a bucket whose stream is gone = %v after %d sends, want nats.ErrNoStreamResponse after one send", err, legacy.sends.Load())
	}
}

// keyUnserved moves the stream of config's bucket off the subject of key, so a write of key draws
// "no response from stream" while the stream is there, and returns a func that moves it back.
func keyUnserved(t *testing.T, js nats.JetStreamContext, config *nats.KeyValueConfig) func() error {
	t.Helper()
	stream := "KV_" + config.Bucket
	info, err := js.StreamInfo(stream, nats.Context(untilTheRowsEnd(t)))
	if err != nil {
		t.Fatalf("read the bucket's stream: %v", err)
	}
	served := info.Config
	unserved := served
	unserved.Subjects = []string{"$KV." + config.Bucket + ".unserved.>"}
	if _, err := js.UpdateStream(&unserved, nats.Context(untilTheRowsEnd(t))); err != nil {
		t.Fatalf("move the bucket's stream off its keys: %v", err)
	}
	return func() error {
		_, err := js.UpdateStream(&served, nats.Context(untilTheRowsEnd(t)))
		return err
	}
}

// awaitSent waits, until the test binary's deadline less 30 s, until conn has sent n messages
// since before, and returns what it has sent.
func awaitSent(t *testing.T, conn *nats.Conn, before, n uint64, what string) uint64 {
	t.Helper()
	return testwait.Until(t, what, func() (uint64, bool) {
		sent := conn.Stats().OutMsgs - before
		return sent, sent >= n
	})
}

// A put through a handle given its caller's context (Within) goes through the jetstream handle and
// is sent again by the bus as one without it is: here the stream does not take the key until the
// first send has drawn "no response from stream" (nats.go's three requests and the bus's read of
// the stream, four messages), and the put, sent again, is stored.
func TestABoundPutNoStreamTookIsSentAgainAndStored(t *testing.T) {
	kv, legacy, js, config := countedBucket(t, "envoy_no_stream_bound_put_test")
	serve := keyUnserved(t, js, config)
	before := legacy.conn.Stats().OutMsgs
	put := make(chan error, 1)
	go func() {
		_, err := kv.Within(untilTheRowsEnd(t)).Put("key", []byte("value"))
		put <- err
	}()
	awaitSent(t, legacy.conn, before, 4, "the put's first send and the read of the stream after it")
	if err := serve(); err != nil {
		t.Fatalf("move the bucket's stream back onto its keys: %v", err)
	}
	if err := testwait.Receive(t, "the bound put's answer", put); err != nil {
		t.Fatalf("a bound put no stream took, the stream then moved back = %v", err)
	}
	if legacy.sends.Load() != 0 {
		t.Fatalf("the bound put went through the legacy handle %d times, want none", legacy.sends.Load())
	}
	entry, err := kv.Get("key")
	if err != nil || string(entry.Value()) != "value" {
		t.Fatalf("read the key back = %v, %v; want value", entry, err)
	}
}

// A put its caller gave up on (Bounded, Within) sends nothing after the caller has: here the stream
// does not take the key, the caller gives up during nats.go's own sends again, then the stream
// takes the key again and the caller's next write deletes it, and the key stays deleted once the
// given-up put has answered. The legacy handle's put went on sending after its caller gave up and
// could land after that delete.
func TestAPutWhoseCallerGaveUpLandsNothingAfterItsNextWrite(t *testing.T) {
	kv, legacy, js, config := countedBucket(t, "envoy_no_stream_caller_test")
	serve := keyUnserved(t, js, config)
	caller, giveUp := context.WithCancel(context.Background())
	defer giveUp()
	before := legacy.conn.Stats().OutMsgs
	gaveUp := make(chan error, 1)
	go func() {
		_, err := kv.Within(caller).Put("key", []byte("given up"))
		gaveUp <- err
	}()
	awaitSent(t, legacy.conn, before, 2, "the put's first request and nats.go's first send again")
	giveUp()
	if err := serve(); err != nil {
		t.Fatalf("move the bucket's stream back onto its keys: %v", err)
	}
	if err := kv.Delete("key"); err != nil {
		t.Fatalf("the caller's next write, a delete: %v", err)
	}
	if err := testwait.Receive(t, "the given-up put's answer", gaveUp); err == nil {
		t.Fatal("the given-up put answered that it was stored")
	}
	if entry, err := kv.Get("key"); !errors.Is(err, nats.ErrKeyNotFound) {
		t.Fatalf("read the key after the caller's delete = %v, %v; want it deleted, the given-up put landing nothing after it", entry, err)
	}
}

// A put through a handle given its caller's context sends nothing when the caller's deadline is
// within noStreamMargin: the jetstream handle sends under a context that ends noStreamMargin
// before the caller's deadline, so with that deadline 99 ms away the put is refused before it
// leaves, and answers a timeout. Without the margin it would go out and be stored.
func TestAPutWhoseCallerHasLessThanTheMarginLeftSendsNothing(t *testing.T) {
	kv, legacy, _, _ := countedBucket(t, "envoy_no_stream_margin_test")
	before := legacy.conn.Stats().OutMsgs
	_, err := kv.Within(&nearDeadline{Context: context.Background()}).Put("key", []byte("value"))
	sent := legacy.conn.Stats().OutMsgs - before
	if !errors.Is(err, nats.ErrTimeout) || sent != 0 || legacy.sends.Load() != 0 {
		t.Fatalf("a put whose caller's deadline is 99 ms away = %v after %d messages (%d through the legacy handle), want nats.ErrTimeout and none", err, sent, legacy.sends.Load())
	}
	if entry, err := kv.Get("key"); !errors.Is(err, nats.ErrKeyNotFound) {
		t.Fatalf("read the key = %v, %v; want it not stored", entry, err)
	}
}

// A write the stream refuses is not sent again: here an update at a revision the key has moved
// past, which the stream answers with its conflict at once.
func TestAConflictingUpdateIsSentOnce(t *testing.T) {
	kv, legacy, _, _ := countedBucket(t, "envoy_no_stream_conflict_test")
	first, err := kv.Put("key", []byte("one"))
	if err != nil {
		t.Fatalf("put the key: %v", err)
	}
	if _, err := kv.Put("key", []byte("two")); err != nil {
		t.Fatalf("put the key again: %v", err)
	}
	before := legacy.sends.Load()
	if _, err := kv.Update("key", []byte("three"), first); !IsCASConflict(err) {
		t.Fatalf("an update at a revision the key moved past = %v, want a conflict", err)
	}
	if sent := legacy.sends.Load() - before; sent != 1 {
		t.Fatalf("the conflicting update was sent %d times, want once", sent)
	}
}

// countedPublisher is a client's JetStream context that counts each publish the stored path sends
// through it and the messages the connection sent during it, and calls after, when set, with the
// publish's number and answer before it returns.
type countedPublisher struct {
	nats.JetStreamContext
	conn  *nats.Conn
	after func(n int, err error)
	mu    sync.Mutex
	sent  []uint64
}

func (p *countedPublisher) Publish(subject string, data []byte, opts ...nats.PubOpt) (*nats.PubAck, error) {
	before := p.conn.Stats().OutMsgs
	ack, err := p.JetStreamContext.Publish(subject, data, opts...)
	p.mu.Lock()
	p.sent = append(p.sent, p.conn.Stats().OutMsgs-before)
	n := len(p.sent)
	p.mu.Unlock()
	if p.after != nil {
		p.after(n, err)
	}
	return ack, err
}

func (p *countedPublisher) messages() []uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.sent)
}

// offSubjectClient is a client owning the notification stream on the test's own server, with the
// stream moved off the subjects of agent notifications, so a stored publish on one draws "no
// response from stream" while the stream is there. It returns the client, the counting publisher
// its stored path now sends through, and a func that moves the stream back onto those subjects.
func offSubjectClient(t *testing.T) (*Client, *countedPublisher, func() error) {
	t.Helper()
	client, err := ConnectOwningStream([]string{testnats.URL(t)}, WithKeepInterval(time.Hour))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	js := client.JS()
	info, err := js.StreamInfo(Stream, nats.Context(untilTheRowsEnd(t)))
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	original := info.Config
	off := original
	off.Subjects = slices.DeleteFunc(slices.Clone(original.Subjects), func(s string) bool { return s == "notifications.agent.>" })
	if len(off.Subjects) == len(original.Subjects) {
		t.Fatalf("the stream carries no notifications.agent.> to move off: %v", original.Subjects)
	}
	if _, err := js.UpdateStream(&off, nats.Context(untilTheRowsEnd(t))); err != nil {
		t.Fatalf("move the stream off notifications.agent.>: %v", err)
	}
	counted := &countedPublisher{JetStreamContext: js, conn: client.Conn}
	client.mu.Lock()
	client.js = counted
	client.mu.Unlock()
	restore := func() error {
		_, err := js.UpdateStream(&original, nats.Context(untilTheRowsEnd(t)))
		return err
	}
	return client, counted, restore
}

// offSubjectEnvelope is an agent notification, on a subject the stream moved off.
var offSubjectEnvelope = contracts.Envelope{
	EventID: "event-off-subject", Source: "dispatch",
	Topic: "notifications.agent.ses_off_subject", DedupeKey: "dispatch.event.off_subject",
}

// A stored publish sends one request at each send, so every pause between two sends is the bus
// loop's, which stops before a send once the publish's context has ended. nats.go's own loop sends
// again with a context that ended in its pause and answers the context's error, which the stored
// path reads as a send that may have stored the message. Here the stream is there and does not
// take the subject, and the second send ends the context: the publish fails with "no response from
// stream", without ErrUnacknowledged, after two sends of one message each.
func TestAStoredPublishNoStreamTakesSendsOneRequestEachSendAndStoresNothing(t *testing.T) {
	client, counted, _ := offSubjectClient(t)
	ctx, end := context.WithCancel(untilTheRowsEnd(t))
	defer end()
	counted.after = func(n int, _ error) {
		if n == 2 {
			end()
		}
	}
	_, err := client.PublishStoredAck(ctx, offSubjectEnvelope.Topic, offSubjectEnvelope, "")
	if !errors.Is(err, nats.ErrNoStreamResponse) || errors.Is(err, ErrUnacknowledged) {
		t.Fatalf("a publish no stream took, its context ended at the second send = %v, want nats.ErrNoStreamResponse without ErrUnacknowledged", err)
	}
	if sent := counted.messages(); !slices.Equal(sent, []uint64{1, 1}) {
		t.Fatalf("messages the connection sent during each publish = %v, want [1 1]: one request a send, two sends", sent)
	}
}

// A stored publish the server answers "no response from stream" while the stream is there is sent
// again and stored: here the stream is moved back onto the publish's subject inside the first send
// that draws that answer.
func TestAStoredPublishNoStreamTookIsSentAgainAndStored(t *testing.T) {
	client, counted, restore := offSubjectClient(t)
	var restored error
	counted.after = func(n int, err error) {
		if n == 1 && errors.Is(err, nats.ErrNoStreamResponse) {
			restored = restore()
		}
	}
	ack, err := client.PublishStoredAck(untilTheRowsEnd(t), offSubjectEnvelope.Topic, offSubjectEnvelope, "")
	sent := counted.messages()
	if err != nil || restored != nil {
		t.Fatalf("a publish no stream took, the stream then moved back = %v (move back %v, messages a send %v)", err, restored, sent)
	}
	if ack.Sequence == 0 || ack.Duplicate || len(sent) < 2 {
		t.Fatalf("the publish was acknowledged %+v after %d sends, want a new message stored by a send after the first", ack, len(sent))
	}
}

// A stored publish to a deleted notification stream fails with nats.ErrNoStreamResponse within
// its context and without ErrUnacknowledged, after one send of one message: the read after the
// answer finds no stream, so it is not sent again, and no send of it reached a stream.
func TestAStoredPublishToAStreamThatIsGoneIsSentOnceAndStoresNothing(t *testing.T) {
	client, counted, _ := offSubjectClient(t)
	if err := client.JS().DeleteStream(Stream); err != nil {
		t.Fatalf("delete the stream: %v", err)
	}
	_, err := client.PublishStoredAck(untilTheRowsEnd(t), offSubjectEnvelope.Topic, offSubjectEnvelope, "")
	if !errors.Is(err, nats.ErrNoStreamResponse) || errors.Is(err, ErrUnacknowledged) {
		t.Fatalf("a publish to a stream that is gone = %v, want nats.ErrNoStreamResponse without ErrUnacknowledged", err)
	}
	if sent := counted.messages(); !slices.Equal(sent, []uint64{1}) {
		t.Fatalf("messages the connection sent during each publish = %v, want [1]: one send, not sent again", sent)
	}
}

// closingPublisher is a client's JetStream context whose first publish closes the client's
// connection and is then sent on it, so it answers that the connection closed and nothing reaches
// the server. The connection closes without its ClosedCB, so no recovery replaces it in the
// background: the one replacement is the one sendStored dials (ensureConnWithContext).
type closingPublisher struct {
	nats.JetStreamContext
	client *Client
	calls  atomic.Int32
}

func (p *closingPublisher) Publish(subject string, data []byte, opts ...nats.PubOpt) (*nats.PubAck, error) {
	if p.calls.Add(1) == 1 {
		p.client.mu.Lock()
		conn := p.client.Conn
		p.client.mu.Unlock()
		conn.SetClosedHandler(nil)
		conn.Close()
	}
	return p.JetStreamContext.Publish(subject, data, opts...)
}

// afterReconnectDeadline is a context that has not ended and whose deadline is an hour away while
// the client still has the connection it had when the row began, and 99 ms away once the client has
// replaced it: a publish's deadline that turns near only during its reconnect. It counts the near
// reads.
type afterReconnectDeadline struct {
	context.Context
	client *Client
	first  *nats.Conn
	near   atomic.Int32
}

func (c *afterReconnectDeadline) Deadline() (time.Time, bool) {
	c.client.mu.Lock()
	replaced := c.client.Conn != c.first
	c.client.mu.Unlock()
	if !replaced {
		return time.Now().Add(time.Hour), true
	}
	c.near.Add(1)
	return time.Now().Add(99 * time.Millisecond), true
}

// The stored path's send again after a reconnect starts under the loop's rule, read once the
// reconnect is done: not within noStreamMargin of the publish's deadline. Here the first send
// closes the client's connection and answers that it closed, sendStored dials a new one, and the
// deadline turns 99 ms away only once the new one is in place: the call fails with the first send's
// answer, nothing is sent again and nothing is stored. A rule read before the reconnect, while the
// deadline was still an hour away, sends again and stores the message.
func TestAStoredPublishIsNotSentAgainWhenItsReconnectBringsItsDeadlineNear(t *testing.T) {
	client, err := ConnectOwningStream([]string{testnats.URL(t)}, WithKeepInterval(time.Hour))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	before, err := client.JS().StreamInfo(Stream, nats.Context(untilTheRowsEnd(t)))
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	client.mu.Lock()
	first := client.Conn
	publisher := &closingPublisher{JetStreamContext: client.js, client: client}
	client.js = publisher
	client.mu.Unlock()
	ctx := &afterReconnectDeadline{Context: context.Background(), client: client, first: first}
	ack, err := client.sendStored(ctx, offSubjectEnvelope.Topic, []byte(`{}`), nats.Context(ctx))
	client.mu.Lock()
	replaced := client.Conn != first
	client.mu.Unlock()
	if !replaced {
		t.Fatal("sendStored dialed no new connection, so this row holds nothing")
	}
	if !errors.Is(err, nats.ErrConnectionClosed) || ack.Resent {
		t.Fatalf("a publish whose reconnect brought its deadline within 99 ms = %+v, %v, want nats.ErrConnectionClosed and no send again", ack, err)
	}
	if ctx.near.Load() == 0 {
		t.Fatal("the deadline was never read after the reconnect, so the rule was not read where this row holds it")
	}
	after, err := client.JS().StreamInfo(Stream, nats.Context(untilTheRowsEnd(t)))
	if err != nil {
		t.Fatalf("read the stream again: %v", err)
	}
	if after.State.Msgs != before.State.Msgs {
		t.Fatalf("the stream holds %d messages after the publish, %d before: the publish was sent again and stored", after.State.Msgs, before.State.Msgs)
	}
}
