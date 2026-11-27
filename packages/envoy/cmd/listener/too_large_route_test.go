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

// storedSize is what the route's check (bus.Client.TooLargeToPublish) holds a stored publish of
// item to against the server's max payload: its encoding and the larger of two reserves beside it,
// the Nats-Msg-Id header it is stored under, the dedupe key and the topic (bus.ForwardMsgID), which
// a minted key earns, and the room the relay's frame headers take (bus.RelayFrameHeaderBytes),
// since the stored message reaches a session in consumer mode as its bytes under those headers.
// For a key of the usual length the relay's headers are the larger.
func storedSize(t *testing.T, item contracts.Envelope) int {
	t.Helper()
	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("encode the envelope: %v", err)
	}
	msgID := (&natsgo.Msg{Header: natsgo.Header{natsgo.MsgIdHdr: []string{bus.ForwardMsgID(item.DedupeKey, item.Topic)}}}).Size()
	return len(encoded) + max(msgID, bus.RelayFrameHeaderBytes(item.Topic))
}

// bandText is a text of 'm's whose message, as size measures it, takes exactly want bytes. Past the
// summary's 160 runes a text is the envelope's payload whole, and the envelope's ids and issue time
// have a fixed width, so each further 'm' adds one byte; size is a row's own measure of the
// message it sends, under the idempotency key it sends.
func bandText(t *testing.T, size func(text string) int, want int) string {
	t.Helper()
	const base = 1000
	text := strings.Repeat("m", base+want-size(strings.Repeat("m", base)))
	if got := size(text); got != want {
		t.Fatalf("UNKNOWN: the solved text's message takes %d bytes, want %d", got, want)
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
// the route answered 404 after that read and a wake; and a publish from a sender whose count under
// the sender limit is empty, which the route answered 429. Every row must be 413 with no
// Retry-After, nothing may reach the row's subjects (its own publish, its target's relay subject,
// the reads of its target's session record) while the route runs, and no wake report may be
// raised: none in flight once the route answers, and none published. The controls are the first
// three with a message within the bound and a live deadline, each published, which the same watch
// sees, so a watch that could not see a send cannot pass the refusals; and a small publish from the
// limited sender after the refusal, which is the 429, so the limited row's count was empty when its
// 413 came. A message within the bound by itself and past it only with its Nats-Msg-Id header has
// no row here: the publish's own count of that header refuses it before anything is sent too, so
// no route answer can tell the route's count from the publish's, and that count is held at the
// check itself (internal/bus's TestTooLargeToPublishRefusesWhatThePublishWouldWithoutSendingIt).
//
// The band the route's check leaves is a message within the bound until the route adds its sender
// stamp (senderStamp). A publish of one is 413 whatever is left of its deadline, refused by the
// publish's own check before that reads its context. A send of one to a stopped session, under a
// live deadline, is 413 only after the route's own work for it: the watch must see the session read
// and the wake report, which show the message passed the route's check, and nothing on the
// target's subjects, which shows the publish sent nothing. Each band message is sized, under the
// idempotency key its request sends, to be within the bound the route's check holds it to
// (storedSize) without the stamp and one byte past it with, so a message outside the band fails the
// row: a smaller one is published, a larger one is refused before the route's reads.
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

	stamp := senderStamp(context.Background(), rig.deps.registry, rig.deps.sessions, live)
	if stamp == nil {
		t.Fatalf("UNKNOWN: %s has no sender stamp, so the stamp band is empty", live)
	}
	// stampBand is a text whose envelope, as build makes it under key, is within what the route
	// accepts on its topic without the stamp and one byte past it with (storedSize).
	stampBand := func(name string, build func(text string) contracts.Envelope) string {
		t.Helper()
		stamped := func(text string) contracts.Envelope {
			item := build(text)
			item.Sender = stamp
			return item
		}
		text := bandText(t, func(text string) int { return storedSize(t, stamped(text)) }, maxPayload+1)
		if bare := storedSize(t, build(text)); bare > maxPayload {
			t.Fatalf("UNKNOWN: %s takes %d bytes without the stamp, its reserve counted, want it within the max payload of %d", name, bare, maxPayload)
		}
		return text
	}
	publishKey := id.New()
	publishBand := stampBand("the publish's band message", func(text string) contracts.Envelope {
		return contracts.MessageEnvelope(listenerMessage(listenerwire.MessageBody{Source: "agent", SourceSession: live, Message: text}, contracts.ExpectsReplyUnset), topic, publishDedupePrefix+publishKey)
	})
	for _, deadline := range []struct {
		name string
		ctx  context.Context
	}{{"its deadline spent", spent}, {"its deadline live", context.Background()}} {
		recorder := request(deadline.ctx, "/v1/messages/publish", map[string]string{
			"topic": topic, "message": publishBand, "source_session": live, "idempotency_key": publishKey,
		})
		refused("a publish over the bound only with the sender stamp, "+deadline.name, recorder)
	}

	sendKey := id.New()
	sendBand := stampBand("the send's band message", func(text string) contracts.Envelope {
		return contracts.AgentSendEnvelope(stopped, sendKey, listenerMessage(listenerwire.MessageBody{Source: "agent", SourceSession: live, Message: text}, contracts.ExpectsReplyUnset))
	})
	const sendBandName = "a send to a stopped session over the bound only with the sender stamp, its deadline live"
	recorder := request(context.Background(), "/v1/messages/send", map[string]string{
		"target_session": stopped, "message": sendBand, "source_session": live, "idempotency_key": sendKey,
	})
	if recorder.Code != http.StatusRequestEntityTooLarge || !strings.Contains(recorder.Body.String(), "max payload of 1048576 bytes") {
		t.Errorf("%s: status = %d, want 413 naming the server's max payload; body = %.300s", sendBandName, recorder.Code, recorder.Body.String())
	}
	if retry := recorder.Header().Get("Retry-After"); retry != "" {
		t.Errorf("%s carries Retry-After %q", sendBandName, retry)
	}
	// The wake report this send raised is published once its slot is given back
	// (wakeReporter.report), so the watch below sees it.
	testwait.Eventually(t, "the band send's wake report to be published", func() bool { return len(rig.deps.wakes.slots) == 0 })
	sent := watch.through(t, conn)
	readSession := slices.ContainsFunc(sent, func(subject string) bool { return strings.Contains(subject, "KV_"+bus.SessionBucket) })
	if !readSession || !slices.Contains(sent, bus.LegionWakeTopic) {
		t.Errorf("%s sent %v while the route ran, want the session read and the wake report: without them the 413 is the route's own check, and the message is not in the band", sendBandName, sent)
	}
	for _, subject := range []string{contracts.LegacyAgentSubject(stopped), bus.DeliverSubject(stopped)} {
		if slices.Contains(sent, subject) {
			t.Errorf("%s sent %v while the route ran, including %s: the message was published", sendBandName, sent, subject)
		}
	}

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
