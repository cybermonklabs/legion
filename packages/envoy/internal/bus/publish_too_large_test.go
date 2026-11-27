package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// NATS takes a message only whole, and two limits decide what whole can be: nats.go refuses a
// message past the server's max payload before sending it, and the server closes the connection
// over a protocol line past its max control line (4 KiB by default), which a long enough subject
// makes. Both are ErrTooLarge, which names the size. A subject NATS does not accept, one holding
// whitespace or an empty token (which no stream's subjects match, so a JetStream publish waits out
// its deadline for an answer that never comes), is ErrInvalidSubject. Each is an ErrRefused, which
// a caller answers as a refusal rather than a failure to retry, and none may close the connection
// every subscription and watcher of the client runs on.
func TestPublishRefusesAnEnvelopeNATSCannotTakeWhole(t *testing.T) {
	t.Parallel()
	client, err := ConnectOwningStream([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)

	envelope := func(topic, payload string) contracts.Envelope {
		return contracts.Envelope{
			EventID:        "event",
			Source:         "github",
			SourceEventID:  "delivery",
			Topic:          topic,
			DedupeKey:      "github.delivery",
			IssuedAt:       contracts.NowMillis(),
			PayloadSummary: "push to a branch",
			Payload:        payload,
			TraceID:        "trace",
		}
	}
	push := "notifications.github.acme.widgets.push.branch."
	for _, tc := range []struct {
		name     string
		envelope contracts.Envelope
		want     error
		says     string
	}{
		{"a subject past the server's protocol line", envelope(push+strings.Repeat("r", 5000), "{}"), ErrTooLarge, "a subject of 5046 bytes"},
		{"a subject one byte past the bound", envelope(push+strings.Repeat("r", maxSubjectBytes-len(push)+1), "{}"), ErrTooLarge, "a subject of 4033 bytes, past 4032"},
		{"a payload past the server's max payload", envelope(push+"main", strings.Repeat("p", 1<<20)), ErrTooLarge, "max payload of 1048576 bytes"},
		{"a core subject past the server's protocol line", envelope(contracts.RoleTopicPrefix+strings.Repeat("r", 5000), "{}"), ErrTooLarge, "a subject of 5019 bytes"},
		{"a subject holding a space", envelope(push+"ci yml", "{}"), ErrInvalidSubject, `"notifications.github.acme.widgets.push.branch.ci yml"`},
		{"a subject holding an empty token", envelope(contracts.LegacyAgentSubject("sess..x"), "{}"), ErrInvalidSubject, `"notifications.agent.sess..x"`},
		{"a subject ending in an empty token", envelope(push+"main.", "{}"), ErrInvalidSubject, `"notifications.github.acme.widgets.push.branch.main."`},
		{"a core subject holding an empty token", envelope(contracts.RoleTopicPrefix+"a..b", "{}"), ErrInvalidSubject, `"notifications.role.a..b"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := client.Publish(tc.envelope)
			if !errors.Is(err, tc.want) || !errors.Is(err, ErrRefused) {
				t.Fatalf("publish error = %v, want %v, an ErrRefused", err, tc.want)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("publish error = %q, want it to say %q", err, tc.says)
			}
			if !client.Conn.IsConnected() {
				t.Fatalf("the refusal left the connection %v, want it connected", client.Conn.Status())
			}
			// The control: the same connection still publishes an envelope that fits.
			if err := client.Publish(envelope(push+"main", "{}")); err != nil {
				t.Fatalf("publish after the refusal: %v", err)
			}
		})
	}
	// The bound itself publishes: its line, reply inbox and sizes included, is within the server's.
	if err := client.Publish(envelope(push+strings.Repeat("r", maxSubjectBytes-len(push)), "{}")); err != nil {
		t.Fatalf("publish on a subject of %d bytes: %v", maxSubjectBytes, err)
	}
	if !client.Conn.IsConnected() {
		t.Fatalf("a subject at the bound left the connection %v, want it connected", client.Conn.Status())
	}
}

// sizeRig is a client owning the stream on a server of the package's shared ones, and that
// server's max payload, which the size tests below share.
type sizeRig struct {
	client     *Client
	url        string
	maxPayload int
}

func newSizeRig(t *testing.T) sizeRig {
	t.Helper()
	url := testnats.URL(t)
	client, err := ConnectOwningStream([]string{url})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	return sizeRig{client: client, url: url, maxPayload: int(client.Conn.MaxPayload())}
}

// sizeEnvelope is an envelope on topic carrying payload, of source dispatch, so its publish stores
// it under a message id (storedMsgID) whose Nats-Msg-Id header counts against the max payload.
// Every other field has a fixed width, so its encoding grows by one byte for each byte of a payload
// of 'p'.
func sizeEnvelope(topic, payload string) contracts.Envelope {
	return contracts.Envelope{
		EventID:        "event",
		Source:         "dispatch",
		SourceEventID:  "source-event",
		Topic:          topic,
		DedupeKey:      "dispatch-size",
		IssuedAt:       1_800_000_000_000,
		PayloadSummary: "a message measured against the max payload",
		Payload:        payload,
		TraceID:        "trace",
	}
}

// encodedSize is the size of item as a publish encodes it.
func encodedSize(t *testing.T, item contracts.Envelope) int {
	t.Helper()
	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("encode the envelope: %v", err)
	}
	return len(encoded)
}

// sizedPayload is the payload of 'p' that makes sizeEnvelope(topic, payload) encode to exactly
// size bytes.
func sizedPayload(t *testing.T, topic string, size int) string {
	t.Helper()
	fixed := encodedSize(t, sizeEnvelope(topic, "p")) - 1
	if size <= fixed {
		t.Fatalf("UNKNOWN: an envelope of %d bytes is smaller than its fixed fields, %d bytes", size, fixed)
	}
	return strings.Repeat("p", size-fixed)
}

// sizeText is the text of a refusal made on a message's own data and headers (pastMaxPayload): a
// message of size bytes, headers included, against maxPayload. The refusal of a relay frame
// carries frameText instead.
func sizeText(size, maxPayload int) string {
	return fmt.Sprintf("a message of %d bytes, its headers included, against the server's max payload of %d bytes", size, maxPayload)
}

// frameText is the text the refusal of a message of size bytes stored on subject carries, whose
// relay frame, with the relay's headers, is past maxPayload.
func frameText(size int, subject string, maxPayload int) string {
	return fmt.Sprintf("a message of %d bytes, which reaches a session in a relay frame of %d bytes with the relay's headers, against the server's max payload of %d bytes",
		size, size+RelayFrameHeaderBytes(subject), maxPayload)
}

// TooLargeToPublish measures a message as its publish on its own topic sends it - the envelope as
// encoded, the Nats-Msg-Id header it is stored under, and, for a topic the stream stores, the
// relay's own headers, which carry it to a session (RelayFrameHeaderBytes) - and refuses it past
// the server's max payload in the words of the publish's own refusal, sending nothing. The relay's
// headers are the larger reserve of the two, so they are what the bound on such a topic is. The
// bound is exact: a message that leaves them exactly their room is not refused, and the publish
// stores it, so the count is no stricter than it must be; one byte more is refused, and so is its
// publish, in the same words, which name the message's size and the size of its frame. The
// encoding is what counts, not the payload's length: a payload of '<', which the encoding writes
// as six bytes each (\u003c), is refused though the payload alone is a quarter of the bound. A
// client that knows no max payload is TestAClientThatHasNotConnectedRefusesNoMessageForItsSize's.
func TestTooLargeToPublishRefusesWhatThePublishWouldWithoutSendingIt(t *testing.T) {
	t.Parallel()
	rig := newSizeRig(t)
	const topic = "notifications.team.too-large"
	header := MessageBytes(&nats.Msg{Header: msgIDHeader(storedMsgID(sizeEnvelope(topic, "")))})
	if header == 0 {
		t.Fatal("UNKNOWN: the envelope earns no message id, so its publish carries no header to count")
	}
	reserved := RelayFrameHeaderBytes(topic)
	if reserved <= header {
		t.Fatalf("UNKNOWN: the relay's headers take %d bytes and the message id header %d: the rows below stand on the relay's being the larger reserve", reserved, header)
	}
	atBound := sizeEnvelope(topic, sizedPayload(t, topic, rig.maxPayload-reserved))
	pastBound := sizeEnvelope(topic, sizedPayload(t, topic, rig.maxPayload-reserved+1))
	escaped := sizeEnvelope(topic, strings.Repeat("<", rig.maxPayload/4))

	sentBefore := rig.client.Conn.Stats().OutMsgs
	if err := rig.client.TooLargeToPublish(atBound); err != nil {
		t.Fatalf("a message leaving the relay's %d header bytes exactly their room under the max payload of %d was refused: %v", reserved, rig.maxPayload, err)
	}
	for _, refused := range []struct {
		name string
		item contracts.Envelope
		says string
	}{
		{"a message one byte past the room the relay's headers need", pastBound, frameText(encodedSize(t, pastBound), topic, rig.maxPayload)},
		{"a payload a quarter of the bound that encodes past it", escaped, sizeText(encodedSize(t, escaped)+header, rig.maxPayload)},
	} {
		err := rig.client.TooLargeToPublish(refused.item)
		if !errors.Is(err, ErrTooLarge) || !errors.Is(err, ErrRefused) {
			t.Fatalf("%s: %v, want an ErrRefused naming ErrTooLarge", refused.name, err)
		}
		if !strings.Contains(err.Error(), refused.says) {
			t.Fatalf("%s: %q, want it to say %q", refused.name, err, refused.says)
		}
	}
	if sent := rig.client.Conn.Stats().OutMsgs - sentBefore; sent != 0 {
		t.Fatalf("the size checks sent %d messages, want none", sent)
	}

	if err := rig.client.Publish(atBound); err != nil {
		t.Fatalf("the publish of a message leaving the relay's headers exactly their room: %v, want it stored", err)
	}
	checked := rig.client.TooLargeToPublish(pastBound)
	if published := rig.client.Publish(pastBound); published == nil || published.Error() != checked.Error() {
		t.Fatalf("the publish of a message one byte past the bound answered %v, want the check's own refusal %q", published, checked)
	}

	// The message id header is the larger reserve once the dedupe key outgrows the relay's fixed
	// header bytes, which a Dispatch envelope's key may: then the header is the bound, exact to the
	// byte. No route answer can show this branch apart from the publish's own count of the same
	// header (checkSend), which refuses the same message before anything is sent, so it is held
	// here, at the check itself.
	t.Run("a message id that outgrows the relay's headers", func(t *testing.T) {
		long := func(payload string) contracts.Envelope {
			item := sizeEnvelope(topic, payload)
			item.DedupeKey = "dispatch-size-" + strings.Repeat("k", 300)
			return item
		}
		header := MessageBytes(&nats.Msg{Header: msgIDHeader(storedMsgID(long("")))})
		if header <= reserved {
			t.Fatalf("UNKNOWN: the message id header takes %d bytes and the relay's headers %d: the row wants the message id the larger reserve", header, reserved)
		}
		fixed := encodedSize(t, long("p")) - 1
		atBound, pastBound := long(strings.Repeat("p", rig.maxPayload-header-fixed)), long(strings.Repeat("p", rig.maxPayload-header-fixed+1))
		if err := rig.client.TooLargeToPublish(atBound); err != nil {
			t.Fatalf("a message of exactly the max payload with its %d-byte message id header was refused: %v", header, err)
		}
		err := rig.client.TooLargeToPublish(pastBound)
		if says := sizeText(encodedSize(t, pastBound)+header, rig.maxPayload); !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), says) {
			t.Fatalf("a message one byte past the max payload with its message id header: %v, want ErrTooLarge saying %q", err, says)
		}
	})
}

// Every send a caller's payload reaches refuses a message past the server's max payload, its
// headers included, before it waits on a connection or reads its context. nats.go refuses such a
// message before sending it, but only after a JetStream publish has read its context, and a core
// publish is refused on an ended context first (publishConfirmed), so a message the bus can never
// send was answered as a bus that did not answer whenever the caller's time had run out before
// the send. Each send is asked under what makes that matter: an ended context, a forward's window
// of no time. Two kinds of send are asked otherwise. The relay frame reads no time while its
// connection is up. The publish window's two sends read their own deadline before the size
// (WindowUntil), so they are asked under a live deadline, each on a window of its own, and each
// refusal must come before the window pinned the connection (window.lifeEnded is nil): nats.go's
// own refusal of the same message names the same size in the same words (refused), so only the
// pin tells the window's own check from nats.go's. Each send is asked a body past the bound by
// itself and, where the send writes a header, a body within the bound that the header takes one
// byte past it. The controls are each send with a small body under the same conditions: it is
// refused for that ended context or empty window and not for its size, or it is sent, so the
// check cannot pass by refusing every send as too large.
func TestEverySendRefusesAMessagePastTheMaxPayloadBeforeItWaitsOrReadsItsContext(t *testing.T) {
	t.Parallel()
	rig := newSizeRig(t)
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	// freshWindow is a publish window of its own under a live deadline, so a send on it pins the
	// connection only once it is past every refusal of its own.
	freshWindow := func() *window {
		return rig.client.WindowUntil(func() time.Time { return time.Now().Add(time.Minute) }).(*window)
	}
	spentWindow := rig.client.WindowUntil(func() time.Time { return time.Now().Add(-time.Second) })
	consumers, err := ConnectSessionConsumers([]string{rig.url}, 0, nil)
	if err != nil {
		t.Fatalf("connect the session consumers: %v", err)
	}
	t.Cleanup(consumers.Close)
	const stored, core = "notifications.team.spent-time", contracts.RoleTopicPrefix + "spent-time"
	holder := contracts.LegacyAgentSubject("ses_spent_time")
	storedID, forwardID := storedMsgID(sizeEnvelope(stored, "")), ForwardMsgID("dispatch-size", holder)
	frame := nats.Header{"Envoy-Test-Frame": []string{"1"}}
	refusedFor := func(want error) func(error) bool {
		return func(err error) bool { return errors.Is(err, want) && !errors.Is(err, ErrTooLarge) }
	}
	sent := func(err error) bool { return err == nil }
	// lastWindow is the window the last window send was made on, which pinned reads.
	var lastWindow *window
	pinned := func() bool { return lastWindow.lifeEnded() != nil }
	for _, path := range []struct {
		name, when string
		topic      string
		header     nats.Header
		send       func(data string) error
		control    func(error) bool
		// pinned, for a window's send, reports whether that send pinned the connection.
		pinned func() bool
	}{
		{"a stored publish", "its context ended", stored, msgIDHeader(storedID), func(payload string) error {
			_, err := rig.client.PublishAckWithin(ended, sizeEnvelope(stored, payload))
			return err
		}, refusedFor(context.Canceled), nil},
		{"a core publish", "its context ended", core, nil, func(payload string) error {
			return rig.client.PublishCoreContext(ended, core, sizeEnvelope(core, payload))
		}, refusedFor(context.Canceled), nil},
		{"a raw stored write", "its context ended", "", expectLastSubjectSequence(0), func(data string) error {
			_, err := rig.client.PublishCursorAt(ended, stored+"-cursor", []byte(data), 0)
			return err
		}, refusedFor(context.Canceled), nil},
		{"a role forward", "its window of no time", core, msgIDHeader(forwardID), func(payload string) error {
			forward, err := rig.client.StartRequestStoredTo(holder, sizeEnvelope(core, payload), forwardID, 0)
			if err == nil {
				_, err = forward.Wait()
			}
			return err
		}, refusedFor(ErrWindowEndedBeforePublish), nil},
		{"a publish window's send", "its deadline live", stored, msgIDHeader(storedID), func(payload string) error {
			lastWindow = freshWindow()
			return <-lastWindow.PublishStoredAsync(stored, sizeEnvelope(stored, payload), storedID)
		}, sent, pinned},
		{"a publish window's request", "its deadline live", core, msgIDHeader(forwardID), func(payload string) error {
			lastWindow = freshWindow()
			return <-lastWindow.Request(holder, sizeEnvelope(core, payload), RequestOptions{MsgID: forwardID})
		}, sent, pinned},
		{"a relay frame", "its connection up", "", frame, func(data string) error {
			return consumers.Publish(ended, &nats.Msg{Subject: DeliverSubject("ses_spent_time"), Header: frame, Data: []byte(data)})
		}, sent, nil},
	} {
		header := MessageBytes(&nats.Msg{Header: path.header})
		// body is what the send is handed for a message whose data takes size bytes: an envelope's
		// payload, or the raw data itself.
		body := func(size int) string {
			if path.topic == "" {
				return strings.Repeat("p", size)
			}
			return sizedPayload(t, path.topic, size)
		}
		rows := []struct {
			name string
			data int
		}{{"a body past the bound by itself", rig.maxPayload + 1}}
		if header > 0 {
			rows = append(rows, struct {
				name string
				data int
			}{"a body within the bound that its header takes past it", rig.maxPayload + 1 - header})
		}
		for _, row := range rows {
			err := path.send(body(row.data))
			if says := sizeText(row.data+header, rig.maxPayload); !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), says) {
				t.Errorf("%s of %s, %s: %v, want ErrTooLarge saying %q", path.name, row.name, path.when, err, says)
			}
			if path.pinned != nil && path.pinned() {
				t.Errorf("%s of %s, %s, pinned the connection before it was refused: the refusal was nats.go's, not the window's own check", path.name, row.name, path.when)
			}
		}
		small := "{}"
		if path.topic != "" {
			small = "p"
		}
		if err := path.send(small); !path.control(err) {
			t.Errorf("control: %s of a small body, %s: %v, want the send's own answer to that, not ErrTooLarge", path.name, path.when, err)
		}
		if path.pinned != nil && !path.pinned() {
			t.Errorf("control: %s of a small body, %s, was sent without pinning the connection, so the pin tells nothing", path.name, path.when)
		}
	}

	// A publish window reads its own deadline before the size (WindowUntil). The rows are the
	// window's two sends, each of a body past the bound by itself, on a window whose deadline has
	// passed.
	for _, past := range []struct {
		name string
		send func(payload string) error
	}{
		{"a publish window's send", func(payload string) error {
			return <-spentWindow.PublishStoredAsync(stored, sizeEnvelope(stored, payload), storedID)
		}},
		{"a publish window's request", func(payload string) error {
			return <-spentWindow.Request(holder, sizeEnvelope(core, payload), RequestOptions{MsgID: forwardID})
		}},
	} {
		err := past.send(sizedPayload(t, stored, rig.maxPayload+1))
		if !errors.Is(err, ErrWindowDeadline) || errors.Is(err, ErrTooLarge) {
			t.Errorf("%s of a body past the bound, its deadline passed: %v, want ErrWindowDeadline (WindowUntil)", past.name, err)
		}
	}
}

// A send whose own size check let it through meets nats.go's own refusal of the message, which
// refused names the same ErrTooLarge in the same words, its headers included. The client here is
// one built around a connection of its own, which keeps no max payload of that connection's
// server, as a client reconnected to a server that takes less has the old one for the moment
// before onReconnect reads the new. The row is a forward within the bound that its Nats-Msg-Id
// header takes one byte past it. The client that has read the max payload refuses the same
// forward in the same words before it sends it.
func TestARefusalNatsGoMakesIsTheSameErrTooLargeHeadersIncluded(t *testing.T) {
	t.Parallel()
	rig := newSizeRig(t)
	const topic = contracts.RoleTopicPrefix + "unread"
	holder := contracts.LegacyAgentSubject("ses_size_unread")
	msgID := ForwardMsgID("dispatch-size", holder)
	header := MessageBytes(&nats.Msg{Header: msgIDHeader(msgID)})
	item := sizeEnvelope(topic, sizedPayload(t, topic, rig.maxPayload+1-header))
	says := sizeText(rig.maxPayload+1, rig.maxPayload)

	unread := &Client{Conn: rig.client.Conn}
	if known := unread.maxPayload.Load(); known != 0 {
		t.Fatalf("UNKNOWN: a client built around a connection reads a max payload of %d, want none, so its own check lets the forward through", known)
	}
	if _, err := unread.StartRequestStoredTo(holder, item, msgID, 5*time.Second); !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), says) {
		t.Fatalf("nats.go's refusal of a forward one byte past the bound with its header = %v, want ErrTooLarge saying %q", err, says)
	}
	if _, err := rig.client.StartRequestStoredTo(holder, item, msgID, 5*time.Second); !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), says) {
		t.Fatalf("the size check's refusal of the same forward = %v, want ErrTooLarge saying %q", err, says)
	}
}

// A client whose first connection has not connected keeps no max payload, as nats.go itself keeps
// none until the server's INFO arrives, and sends an unconnected connection's message without
// measuring it (!nc.initc). So the size checks refuse nothing on it, whatever the message's size:
// a publish of a body twice any server's default bound is answered by the wait for that
// connection, here a deadline already past, and never as too large. A max payload of 0 refuses
// nothing at any size, so 2 MiB proves the row. The row is cybermonklabs/legion#5734's
// TestABoundOfZeroRefusesNothingForItsSize (the Envoy flakes peer's builder), which closed in
// favour of this pull request.
func TestAClientThatHasNotConnectedRefusesNoMessageForItsSize(t *testing.T) {
	t.Parallel()
	client, err := Connect([]string{"nats://127.0.0.1:1"}, WithEnvironment(testnats.SeedOnly("")), WithConnectInBackground())
	if err != nil {
		t.Fatalf("connect in the background: %v", err)
	}
	t.Cleanup(client.Close)
	if known := client.Connection().MaxPayload(); known != 0 {
		t.Fatalf("UNKNOWN: a connection that has not connected reports a max payload of %d, want none, so this row checks nothing", known)
	}
	const topic = "notifications.team.unconnected"
	payload := strings.Repeat("p", 2<<20)
	big := sizeEnvelope(topic, payload)
	if err := client.TooLargeToPublish(big); err != nil {
		t.Errorf("TooLargeToPublish on a client that knows no max payload = %v, want nil", err)
	}
	if err := client.checkSend(&nats.Msg{Subject: topic, Data: []byte(payload)}); err != nil {
		t.Errorf("checkSend on a client that knows no max payload = %v, want nil", err)
	}
	spent, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := client.PublishAckWithin(spent, big); !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrTooLarge) {
		t.Errorf("a stored publish of a 2 MiB message under a deadline already past = %v, want the wait's own context.DeadlineExceeded", err)
	}
}
