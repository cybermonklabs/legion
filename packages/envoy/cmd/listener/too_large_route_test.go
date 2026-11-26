package main

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/listenerwire"
)

// subjectWatch counts what reaches a set of subjects, over a connection of its own: a route's
// publish of a message, a probe of its target, a read of its target's session record and a wake
// report for it each land on a subject a row names, and nothing the listener's connection sends
// for its own sake (its caches' reads of their buckets) does. through marks the end of what a
// route sent: the server hands one connection's messages on in the order it took them, so once a
// control message the listener's connection published after the route answered has arrived,
// everything that connection published before it has arrived too.
type subjectWatch struct {
	control string
	mu      sync.Mutex
	seen    []string
}

// watchSubjects starts a subjectWatch of subjects over a connection of its own to url, ready: its
// subscriptions are flushed, so everything published on them from now on is the watch's to see.
func watchSubjects(t *testing.T, url string, subjects ...string) *subjectWatch {
	t.Helper()
	w := &subjectWatch{control: "test.too-large.control." + id.New()}
	watcher := testnats.Connect(t, url)
	t.Cleanup(watcher.Close)
	for _, subject := range append(subjects, w.control) {
		if _, err := watcher.Subscribe(subject, func(msg *natsgo.Msg) {
			w.mu.Lock()
			w.seen = append(w.seen, msg.Subject)
			w.mu.Unlock()
		}); err != nil {
			t.Fatalf("watch %s: %v", subject, err)
		}
	}
	if err := watcher.Flush(); err != nil {
		t.Fatalf("flush the subject watch: %v", err)
	}
	return w
}

// through publishes a control message on conn and waits for the watch to see it, and returns every
// subject the watch saw before it since the last call, which it forgets.
func (w *subjectWatch) through(t *testing.T, conn *natsgo.Conn) []string {
	t.Helper()
	if err := conn.Publish(w.control, nil); err != nil {
		t.Fatalf("publish the watch's control message: %v", err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the watch's control message: %v", err)
	}
	var before []string
	testwait.Eventually(t, "the watch to see its control message, without which it watches nothing", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		index := slices.Index(w.seen, w.control)
		if index < 0 {
			return false
		}
		before = slices.Clone(w.seen[:index])
		w.seen = slices.Clone(w.seen[index+1:])
		return true
	})
	return before
}

// headerBandText is a message for POST /v1/messages/publish on topic under idempotency key key
// whose envelope, as the route builds it, is within the server's max payload by itself, and past
// it by one byte with the Nats-Msg-Id header its publish carries: the band in which only a size
// check that counts the header refuses it before the publish.
func headerBandText(t *testing.T, topic, key string, maxPayload int) string {
	t.Helper()
	dedupeKey := publishDedupePrefix + key
	envelopeSize := func(text string) int {
		item := contracts.MessageEnvelope(listenerMessage(listenerwire.MessageBody{Source: "agent", Message: text}, contracts.ExpectsReplyUnset), topic, dedupeKey)
		encoded, err := json.Marshal(item)
		if err != nil {
			t.Fatalf("encode the envelope: %v", err)
		}
		return len(encoded)
	}
	header := (&natsgo.Msg{Header: natsgo.Header{natsgo.MsgIdHdr: []string{bus.ForwardMsgID(dedupeKey, topic)}}}).Size()
	// A text past the summary's 160 runes is the envelope's payload whole, so each further 'm' adds
	// one byte to the envelope, and its ids and issue time have a fixed width.
	const base = 1000
	text := strings.Repeat("m", base+(maxPayload+1-header)-envelopeSize(strings.Repeat("m", base)))
	if size := envelopeSize(text); size > maxPayload || size+header != maxPayload+1 {
		t.Fatalf("UNKNOWN: the envelope takes %d bytes and its header %d against the max payload of %d: the row wants the envelope within it and the message one byte past it", size, header, maxPayload)
	}
	return text
}

// A message NATS cannot take whole is answered 413 naming the server's max payload before either
// message route does any bus work for it, whatever is left of its deadline. Each oversized request
// is sent with its context's deadline already past, the state a route reaches when the reading and
// encoding of a large message on loaded CPUs have spent its budget before the publish, and again
// with a live one. Under the spent deadline a size refusal made inside the publish, after nats.go
// read the publish's context, was the bus-unavailable 503 "envoy: the message bus did not answer:
// context deadline exceeded" with Retry-After: 1, though the bus was never asked. Under the live
// one the refusal came only after the route's own bus work for the message.
//
// The rows: a topic publish; a send to a live session in consumer delivery, which the route probes;
// a send to a stopped session only its known-session record names, whose entry the route reads
// from the bucket and for which it reports a wake; a send to a session neither store knows, which
// the route answered 404 after that read and a wake; a publish whose envelope is within the bound
// and whose Nats-Msg-Id header takes it past, which only a check that counts the header refuses
// before the publish; and a publish from a sender whose count under the sender limit is empty,
// which the route answered 429. Every row must be 413 with no Retry-After, nothing may reach the
// row's subjects (its own publish, its target's relay subject, the reads of its target's session
// record) while the route runs, and no wake report may be raised: none in flight once the route
// answers, and none published. The controls are the first three with a message within the bound
// and a live deadline, each published, which the same watch sees, so a watch that could not see
// a send cannot pass the refusals; and a small publish from the limited sender after the refusal,
// which is the 429, so the limited row's count was empty when its 413 came.
func TestAMessageNATSCannotTakeWholeIs413BeforeTheRouteDoesAnyBusWork(t *testing.T) {
	t.Parallel()
	rig := newQuietKnownSessionRig(t)
	wakesCtx, stopWakes := context.WithCancel(context.Background())
	t.Cleanup(stopWakes)
	rig.deps.wakes = newWakeReporter(wakesCtx, rig.harness.client)
	rig.deps.senderLimit = newSenderLimit(1, 1, func() time.Time { return senderLimitStart })
	const (
		live, stopped, unknown, limited = "ses_413_live", "ses_413_stopped", "ses_413_unknown", "ses_413_limited"
		topic                           = "notifications.team.too-large"
	)
	rig.register(t, live, nil)
	rig.register(t, stopped, nil)
	rig.stop(t, stopped)
	rig.awaitNoConsumer(t, stopped)
	// The stop's consumer delete runs off the route's goroutine and reads the stopped session's
	// record again once the consumer is gone (sessionDelivery.deleteConsumer), which a starved run
	// lets land during the first row. It is waited for here, so no row counts its read.
	deletesDone, cancelDeletes := context.WithTimeout(context.Background(), listenerWaitBound(t))
	rig.deps.consumers.awaitDeletes(deletesDone)
	cancelDeletes()
	conn := rig.harness.client.Conn
	maxPayload := int(conn.MaxPayload())
	sessionReads := []string{
		"$JS.API.DIRECT.GET.KV_" + bus.SessionBucket, "$JS.API.DIRECT.GET.KV_" + bus.SessionBucket + ".>",
		"$JS.API.STREAM.MSG.GET.KV_" + bus.SessionBucket,
	}
	watch := watchSubjects(t, rig.url, append([]string{
		topic, bus.LegionWakeTopic,
		contracts.LegacyAgentSubject(live), bus.DeliverSubject(live),
		contracts.LegacyAgentSubject(stopped), bus.DeliverSubject(stopped),
		contracts.LegacyAgentSubject(unknown), bus.DeliverSubject(unknown),
	}, sessionReads...)...)
	// Anything published on the watched subjects before the rows, a registration's own work among
	// it, is taken now and counted against no row.
	watch.through(t, conn)

	request := func(ctx context.Context, path string, fields map[string]string) *httptest.ResponseRecorder {
		body := map[string]string{"source": "agent"}
		maps.Copy(body, fields)
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		recorder := httptest.NewRecorder()
		rig.mux.ServeHTTP(recorder, httptest.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewReader(encoded)))
		return recorder
	}
	// refused asks that the route answered 413 naming the max payload, with no Retry-After, sent
	// nothing about the message, and raised no wake report.
	refused := func(name string, recorder *httptest.ResponseRecorder) {
		t.Helper()
		if recorder.Code != http.StatusRequestEntityTooLarge || !strings.Contains(recorder.Body.String(), "max payload of 1048576 bytes") {
			t.Errorf("%s: status = %d, want 413 naming the server's max payload; body = %.300s", name, recorder.Code, recorder.Body.String())
		}
		if retry := recorder.Header().Get("Retry-After"); retry != "" {
			t.Errorf("%s carries Retry-After %q: the same message is refused however often it is sent", name, retry)
		}
		// A wake report takes its slot before the route goes on and gives it back once the bus
		// has it (wakeReporter.report), so a report this route raised is in flight here or
		// published, and the watch below sees a published one.
		if inFlight := len(rig.deps.wakes.slots); inFlight != 0 {
			t.Errorf("%s left %d wake reports in flight, want none", name, inFlight)
		}
		if sent := watch.through(t, conn); len(sent) != 0 {
			t.Errorf("%s sent %v while the route ran, want nothing", name, sent)
		}
	}

	spent, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	oversized := strings.Repeat("m", 2<<20)
	band := headerBandText(t, topic, id.New(), maxPayload)
	for _, deadline := range []struct {
		name string
		ctx  context.Context
	}{{"its deadline spent", spent}, {"its deadline live", context.Background()}} {
		for _, row := range []struct {
			name string
			path string
			body map[string]string
		}{
			{"a topic publish", "/v1/messages/publish", map[string]string{"topic": topic, "message": oversized}},
			{"a send to a live session", "/v1/messages/send", map[string]string{"target_session": live, "message": oversized}},
			{"a send to a stopped session", "/v1/messages/send", map[string]string{"target_session": stopped, "message": oversized}},
			{"a send to a session neither store knows", "/v1/messages/send", map[string]string{"target_session": unknown, "message": oversized}},
			{"a publish within the bound but for its header", "/v1/messages/publish", map[string]string{"topic": topic, "message": band, "idempotency_key": id.New()}},
		} {
			refused(row.name+", "+deadline.name, request(deadline.ctx, row.path, row.body))
		}
	}

	// The limited sender spends its one message, so its count is empty for the oversized one.
	if first := request(context.Background(), "/v1/messages/publish", map[string]string{"topic": topic, "message": "the one message", "source_session": limited}); first.Code != http.StatusOK {
		t.Fatalf("the limited sender's first publish: status = %d, want 200; body = %.300s", first.Code, first.Body.String())
	}
	watch.through(t, conn)
	refused("a publish from a sender at its limit", request(context.Background(), "/v1/messages/publish", map[string]string{"topic": topic, "message": oversized, "source_session": limited}))
	if after := request(context.Background(), "/v1/messages/publish", map[string]string{"topic": topic, "message": "within the bound", "source_session": limited}); after.Code != http.StatusTooManyRequests {
		t.Errorf("control: the limited sender's small publish after the refusal: status = %d, want 429, so its count was empty; body = %.300s", after.Code, after.Body.String())
	}
	watch.through(t, conn)

	for _, control := range []struct {
		name string
		path string
		body map[string]string
	}{
		{"a topic publish", "/v1/messages/publish", map[string]string{"topic": topic}},
		{"a send to a live session", "/v1/messages/send", map[string]string{"target_session": live}},
		{"a send to a stopped session", "/v1/messages/send", map[string]string{"target_session": stopped}},
	} {
		control.body["message"] = "a message within the max payload"
		if recorder := request(context.Background(), control.path, control.body); recorder.Code != http.StatusOK {
			t.Errorf("control: %s of a message within the max payload: status = %d, want 200; body = %.300s", control.name, recorder.Code, recorder.Body.String())
		}
		if sent := watch.through(t, conn); len(sent) == 0 {
			t.Errorf("control: %s was published and the watch saw nothing reach its subjects, so it sees nothing", control.name)
		}
	}
}
