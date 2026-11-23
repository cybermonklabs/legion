package main

// The test helpers the takeover copy's rows share: a role claim and a role's release through the
// listener's own routes, and what the stream holds on a subject, each message with its envelope.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// postRoleClaim is handler's answer to POST /v1/roles/set of role by sessionID, outright or soft,
// naming instance as its process when it is not empty. It touches no testing.T, so a row may make
// it on a goroutine of its own.
func postRoleClaim(handler http.Handler, sessionID, role, instance string, soft bool) *httptest.ResponseRecorder {
	fields := map[string]any{"session_id": sessionID, "role": role}
	if soft {
		fields["soft"] = true
	}
	if instance != "" {
		fields["client_instance_id"] = instance
	}
	body, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, postJSON("/v1/roles/set", string(body)))
	return recorder
}

// grantRoleClaim is postRoleClaim, and fails t unless the claim is granted.
func grantRoleClaim(t testing.TB, handler http.Handler, sessionID, role, instance string, soft bool) {
	t.Helper()
	if recorder := postRoleClaim(handler, sessionID, role, instance, soft); recorder.Code != http.StatusOK {
		t.Fatalf("%s claims %s (process %q, soft %v): status %d %s; want 200", sessionID, role, instance, soft, recorder.Code, recorder.Body.String())
	}
}

// releaseRoleThrough has sessionID's process instance hand role back through handler's POST
// /v1/interests/unsubscribe, naming the role's lane topic, which frees a claim naming that
// process, and fails t unless the route answers 200 and no claim of role stands after it, as
// registry reads it.
func releaseRoleThrough(t testing.TB, handler http.Handler, registry *store.Registry, sessionID, role, instance string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"session_id":         sessionID,
		"topics":             []string{contracts.RoleTopicPrefix + role},
		"client_instance_id": instance,
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, postJSON("/v1/interests/unsubscribe", string(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s releases %s: status %d %s; want 200", sessionID, role, recorder.Code, recorder.Body.String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	claim, err := registry.RoleClaim(ctx, role)
	if err != nil {
		t.Fatalf("read role %s's claim after %s released it: %v", role, sessionID, err)
	}
	if claim.HolderSessionID != "" {
		t.Fatalf("role %s's claim names %s after %s released it; want none", role, claim.HolderSessionID, sessionID)
	}
}

// storedEnvelope is one message the stream holds on a subject: its stream sequence, when the
// stream stored it, and its envelope.
type storedEnvelope struct {
	seq  uint64
	at   time.Time
	item contracts.Envelope
}

// storedEnvelopes is every message the stream holds on subject, in stream order (readStored), with
// its envelope. A read that fails, and a message no envelope decodes, fail t.
func storedEnvelopes(t testing.TB, client *bus.Client, subject string) []storedEnvelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	messages, err := readStored(ctx, client, subject)
	if err != nil {
		t.Fatalf("read what the stream holds on %s: %v", subject, err)
	}
	held := make([]storedEnvelope, 0, len(messages))
	for _, message := range messages {
		var item contracts.Envelope
		if err := json.Unmarshal(message.Data, &item); err != nil {
			t.Fatalf("decode the message at %d on %s: %v", message.Sequence, subject, err)
		}
		held = append(held, storedEnvelope{seq: message.Sequence, at: message.Time, item: item})
	}
	return held
}

// copiesOf is how many messages of held carry eventID.
func copiesOf(held []storedEnvelope, eventID string) int {
	copies := 0
	for _, message := range held {
		if message.item.EventID == eventID {
			copies++
		}
	}
	return copies
}

// markedFrame is a message's bytes as a row stores them on a subject, the shape it has, and its
// mark: a token of letters, digits and hyphens in its payload summary, or in its text where it is
// no JSON object, which every JSON writer leaves as it is, so a row finds the message by it in the
// raw bytes of what a session received (hearMarks) and of a role's kept lane (keptMarks), however
// little of the message a reader of JSON takes. For a role message it also holds the issued_at its
// sender wrote, which every copy of it must carry.
type markedFrame struct {
	shape    string
	mark     string
	data     []byte
	issuedAt int64
}

// String names the frame in a row's lines by its shape and its mark.
func (f markedFrame) String() string {
	return f.shape + " " + f.mark
}

// frameMark is a new mark (markedFrame).
func frameMark() string {
	return "mark-" + id.New()
}

// nestedDeepDepth is one more level of nesting than encoding/json reads; JSON.parse has no such
// limit.
const nestedDeepDepth = 10_001

// nestedDeep is data, a JSON object, with one more field, "nested": an array nested
// nestedDeepDepth deep around a string that holds an escaped quote and more closing brackets than
// opening ones, so a reader that counts brackets inside strings ends the field two levels early,
// and one that ends a string at an escaped quote finds no end to the string after it.
func nestedDeep(data []byte) []byte {
	value := strings.Repeat("[", nestedDeepDepth) + `"]]}\"{"` + strings.Repeat("]", nestedDeepDepth)
	deep := append([]byte{}, bytes.TrimSuffix(data, []byte("}"))...)
	return append(deep, `,"nested":`+value+"}"...)
}

// roleFramesOfEveryShape is a role message of role, as the role lane stores a forward to the
// role's holder, for each shape a reader of JSON can read wrongly. Four the listener's envelope
// decode refuses (decodeStoredEnvelope) although JSON holds them: a newer build's urgency, source
// and expects_reply words, and an issued_at written with a fraction of zero. Four that a read of
// the envelope into a struct refuses or reads as no role message: an event id written as a number,
// a UTF-8 byte order mark before the JSON, a "TOPIC" key after "topic", which a read that matches
// keys without case takes as the topic, and a value nested nestedDeepDepth deep, which
// encoding/json refuses outright. And two that every reader reads alike, a repeated "topic" key
// whose last value is the role's and a key written top\u0069c, which hold a reader to the plugins'
// rules: the last value of a repeated key wins, and a key is read with its escapes. The plugins'
// transport reads each of them as that role message. Each one's topic, read by its exact key past
// the byte order mark, is role's topic, and t fails for a shape that does not hold so.
func roleFramesOfEveryShape(t testing.TB, role string) []markedFrame {
	t.Helper()
	topic := contracts.RoleTopicPrefix + role
	shapes := []struct {
		shape string
		edit  func(item *contracts.Envelope, fields map[string]json.RawMessage)
		bytes func(data []byte) []byte
		// deep marks the shape encoding/json refuses: its topic is checked before nestedDeep.
		deep bool
		// alike marks a shape every reader of JSON reads as role's message; the plain read of
		// its topic by exact key below holds it to that.
		alike bool
	}{
		{shape: "a newer build's urgency", edit: func(item *contracts.Envelope, _ map[string]json.RawMessage) { item.Urgency = "critical" }},
		{shape: "a newer build's source", edit: func(item *contracts.Envelope, _ map[string]json.RawMessage) { item.Source = "linear" }},
		{shape: "a newer build's expects_reply word", edit: func(_ *contracts.Envelope, fields map[string]json.RawMessage) {
			fields["expects_reply"] = json.RawMessage(`"maybe"`)
		}},
		{shape: "an issued_at written with a fraction of zero", edit: func(item *contracts.Envelope, fields map[string]json.RawMessage) {
			fields["issued_at"] = json.RawMessage(strconv.FormatInt(item.IssuedAt, 10) + ".0")
		}},
		{shape: "an event id written as a number", edit: func(_ *contracts.Envelope, fields map[string]json.RawMessage) {
			fields["event_id"] = json.RawMessage(`4626`)
		}},
		{shape: "a byte order mark before the JSON", bytes: func(data []byte) []byte { return append([]byte("\xef\xbb\xbf"), data...) }},
		{shape: `a "TOPIC" key after "topic"`, bytes: func(data []byte) []byte {
			shadow := `,"TOPIC":"` + contracts.AgentTopicPrefix + `ses_topic_shadow"}`
			return append(append([]byte{}, bytes.TrimSuffix(data, []byte("}"))...), shadow...)
		}},
		{shape: "a value nested 10,001 deep", bytes: nestedDeep, deep: true},
		{shape: `a repeated "topic" key whose last value is the role's`, alike: true, bytes: func(data []byte) []byte {
			first := `{"topic":"` + contracts.AgentTopicPrefix + `ses_topic_first",`
			return append([]byte(first), bytes.TrimPrefix(data, []byte("{"))...)
		}},
		{shape: `a topic key written top\u0069c`, alike: true, bytes: func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"topic":`), []byte(`"top\u0069c":`), 1)
		}},
	}
	frames := make([]markedFrame, 0, len(shapes))
	for _, shape := range shapes {
		item := listenerTestEnvelope(topic, roleForwardDedupePrefix+"every-shape-"+id.New())
		mark := frameMark()
		item.PayloadSummary = shape.shape + " " + mark
		fields := map[string]json.RawMessage{}
		if shape.edit != nil {
			shape.edit(&item, fields)
		}
		plain := withFields(t, item, fields)
		data := plain
		if shape.bytes != nil {
			data = shape.bytes(plain)
		}
		checked := data
		if shape.deep {
			var all map[string]json.RawMessage
			if err := json.Unmarshal(data, &all); err == nil {
				t.Fatalf("encoding/json reads the %s frame; want it to refuse it", shape.shape)
			}
			checked = plain
		}
		if got, isString := exactField(t, checked, "topic"); !isString || got != topic {
			t.Fatalf("the %s frame %s has the topic %q by its exact key; want %s", shape.shape, checked, got, topic)
		}
		var strict contracts.Envelope
		if !shape.alike && decodeStoredEnvelope(data, &strict) == nil && strict.Topic == topic {
			t.Fatalf("the listener's envelope decode reads the %s frame %s as role %s's message; want it to refuse or misread it", shape.shape, data, role)
		}
		if !strings.Contains(string(data), mark) {
			t.Fatalf("the %s frame does not hold its mark %s as it is", shape.shape, mark)
		}
		frames = append(frames, markedFrame{shape: shape.shape, mark: mark, data: data, issuedAt: item.IssuedAt})
	}
	return frames
}

// framesNoReadCanTellTheRoleOf is a message for each way a stored one can be one whose role no
// read can tell (readRoleFrame): one that is not a JSON object, one whose topic is a number with
// no role mark, one whose topic is a JSON object with no role mark, and one whose role mark is a
// number under a topic that names no role. None of them is a role message to that read, so the
// relay sends each to the session whose subject holds it. t fails for a shape that does not hold
// so.
func framesNoReadCanTellTheRoleOf(t testing.TB) []markedFrame {
	t.Helper()
	marks := []string{frameMark(), frameMark(), frameMark(), frameMark()}
	notAnObject := "not a JSON object " + marks[0]
	topicItem := listenerTestEnvelope("", roleForwardDedupePrefix+"topic-a-number-"+id.New())
	topicItem.PayloadSummary = "a topic that is a number " + marks[1]
	numberTopic := withFields(t, topicItem, map[string]json.RawMessage{"topic": json.RawMessage(`5`)})
	objectItem := listenerTestEnvelope("", "topic-an-object-"+id.New())
	objectItem.PayloadSummary = "a topic that is a JSON object " + marks[2]
	objectTopic := withFields(t, objectItem, map[string]json.RawMessage{"topic": json.RawMessage(`{"name":"notifications.role.deliverer"}`)})
	markItem := listenerTestEnvelope(contracts.AgentTopicPrefix+"ses_role_mark_a_number", "role-mark-a-number-"+id.New())
	markItem.PayloadSummary = "a role mark that is a number " + marks[3]
	numberMark := withFields(t, markItem, map[string]json.RawMessage{"role": json.RawMessage(`7`)})
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(notAnObject), &fields) == nil {
		t.Fatalf("%q reads as a JSON object; want it not to", notAnObject)
	}
	if got, isString := exactField(t, numberTopic, "topic"); isString || got != "5" {
		t.Fatalf("the frame %s has the topic %q (a string: %t); want the number 5", numberTopic, got, isString)
	}
	if _, isString := exactField(t, objectTopic, "topic"); isString {
		t.Fatalf("the frame %s has a string topic; want a JSON object", objectTopic)
	}
	if got, isString := exactField(t, numberMark, "role"); isString || got != "7" {
		t.Fatalf("the frame %s has the role mark %q (a string: %t); want the number 7", numberMark, got, isString)
	}
	frames := []markedFrame{
		{shape: "a message that is not a JSON object", mark: marks[0], data: []byte(notAnObject)},
		{shape: "a topic that is a number, with no role mark", mark: marks[1], data: numberTopic},
		{shape: "a topic that is a JSON object, with no role mark", mark: marks[2], data: objectTopic},
		{shape: "a role mark that is a number, under a topic that names no role", mark: marks[3], data: numberMark},
	}
	for _, frame := range frames {
		if _, err := readRoleFrame(frame.data); !errors.Is(err, errRoleFrameUnreadable) {
			t.Fatalf("readRoleFrame answers %v for %s; want errRoleFrameUnreadable", err, frame.shape)
		}
	}
	return frames
}

// withFields is item's JSON with each of fields set to its raw value.
func withFields(t testing.TB, item contracts.Envelope, fields map[string]json.RawMessage) []byte {
	t.Helper()
	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	if len(fields) == 0 {
		return encoded
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &all); err != nil {
		t.Fatal(err)
	}
	for key, value := range fields {
		all[key] = value
	}
	data, err := json.Marshal(all)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// exactField is the value data's field key holds, read by its exact key past a leading UTF-8 byte
// order mark: the string it is, or its raw JSON when it is not a string. t fails unless data is a
// JSON object.
func exactField(t testing.TB, data []byte, key string) (string, bool) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &fields); err != nil {
		t.Fatalf("read %s as a JSON object: %v", data, err)
	}
	var value string
	if err := json.Unmarshal(fields[key], &value); err != nil {
		return string(fields[key]), false
	}
	return value, true
}

// eventIDOf is the event id of a message's bytes, read by its exact key, empty where it is not a
// JSON object or its event id is not a string.
func eventIDOf(data []byte) string {
	var fields map[string]json.RawMessage
	if json.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &fields) != nil {
		return ""
	}
	var eventID string
	_ = json.Unmarshal(fields["event_id"], &eventID)
	return eventID
}

// issuedAtOf is the issued_at of a message's bytes, read by its exact key as a JSON number, and -1
// where it is not a JSON object or its issued_at is no number.
func issuedAtOf(data []byte) int64 {
	var fields map[string]json.RawMessage
	if json.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &fields) != nil {
		return -1
	}
	var number json.Number
	if json.Unmarshal(fields["issued_at"], &number) != nil {
		return -1
	}
	value, err := strconv.ParseFloat(number.String(), 64)
	if err != nil {
		return -1
	}
	return int64(value)
}

// heardFrame is one frame a session stand-in received: its event id, the text it was read from -
// the payload summary a stand-in decoded, or the raw frame a process read as bytes (hearMarks) -
// which holds its mark (markedFrame), and the issued_at it carried (issuedAtOf).
type heardFrame struct {
	eventID  string
	text     string
	issuedAt int64
}

// heardFrames is every frame a session stand-in received, in the order it received them, drained
// from the stand-in as they arrive so a row reads what the session received at any point.
type heardFrames struct {
	mu     sync.Mutex
	frames []heardFrame
}

// add records frame as the last one the session received.
func (h *heardFrames) add(frame heardFrame) {
	h.mu.Lock()
	h.frames = append(h.frames, frame)
	h.mu.Unlock()
}

func hear(t *testing.T, standIn *sessionStandIn) *heardFrames {
	t.Helper()
	heard := &heardFrames{}
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		for {
			select {
			case frame := <-standIn.frames:
				heard.add(heardFrame{eventID: frame.item.EventID, text: frame.item.PayloadSummary, issuedAt: frame.item.IssuedAt})
			case <-done:
				return
			}
		}
	}()
	return heard
}

// countMark is how many frames holding mark the session received.
func (h *heardFrames) countMark(mark string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, frame := range h.frames {
		if strings.Contains(frame.text, mark) {
			n++
		}
	}
	return n
}

// allMarks is the start of the text of every frame the session received, enough to show its mark.
func (h *heardFrames) allMarks() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	shown := make([]string, len(h.frames))
	for i, frame := range h.frames {
		text := frame.text
		if len(text) > 160 {
			text = text[:160] + "..."
		}
		shown[i] = text
	}
	return shown
}

// requireIssuedAt fails the test unless every frame holding frame's mark the session received
// carries frame's issued_at: the sender's own time, which a copy must keep however the sender wrote
// it.
func (h *heardFrames) requireIssuedAt(t *testing.T, who string, frame markedFrame) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, heard := range h.frames {
		if strings.Contains(heard.text, frame.mark) && heard.issuedAt != frame.issuedAt {
			t.Errorf("%s received %q with issued_at %d, want %d, the issued_at its sender wrote", who, frame, heard.issuedAt, frame.issuedAt)
		}
	}
}

// awaitMarks waits up to within for the session to have received a frame of each of frames, by
// its mark, and reports whether it did.
func (h *heardFrames) awaitMarks(within time.Duration, frames ...markedFrame) bool {
	return testwait.Holds(within, 20*time.Millisecond, func() bool {
		for _, frame := range frames {
			if h.countMark(frame.mark) == 0 {
				return false
			}
		}
		return true
	})
}

// count is how many frames of eventID the session received.
func (h *heardFrames) count(eventID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, frame := range h.frames {
		if frame.eventID == eventID {
			n++
		}
	}
	return n
}

// all is every event the session received.
func (h *heardFrames) all() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	events := make([]string, len(h.frames))
	for i, frame := range h.frames {
		events[i] = frame.eventID
	}
	return events
}

// await waits up to within for the session to have received a frame of each of eventIDs, and
// reports whether it did.
func (h *heardFrames) await(within time.Duration, eventIDs ...string) bool {
	return testwait.Holds(within, 20*time.Millisecond, func() bool {
		for _, eventID := range eventIDs {
			if h.count(eventID) == 0 {
				return false
			}
		}
		return true
	})
}

// requireNoneHeard fails the test for each of eventIDs the session received.
func requireNoneHeard(t *testing.T, heard *heardFrames, who string, eventIDs ...string) {
	t.Helper()
	for _, eventID := range eventIDs {
		if n := heard.count(eventID); n != 0 {
			t.Errorf("%s received event %s %d times, want none: it received %v", who, eventID, n, heard.all())
		}
	}
}

// hearMarks is sessionID's process on a connection of its own: it takes each relay frame as bytes,
// records its event id and its raw text, which holds its mark (markedFrame) however little of it a
// read of JSON would take, and answers it received and done, as a session that wrote it does; it
// answers the connection probe (bus.ConnectionProbeHeader) done, as the plugins do. It answers
// nothing more, and its connection drains, once t ends.
func hearMarks(t testing.TB, url, sessionID string) *heardFrames {
	t.Helper()
	conn := testnats.Connect(t, url)
	heard := &heardFrames{}
	var silent atomic.Bool
	if _, err := conn.Subscribe(bus.DeliverSubject(sessionID), func(msg *natsgo.Msg) {
		if silent.Load() {
			return
		}
		replies := []string{relayReplyReceived, relayReplyDone}
		if msg.Header.Get(bus.ConnectionProbeHeader) == "1" {
			replies = []string{relayReplyDone}
		} else {
			heard.add(heardFrame{eventID: eventIDOf(msg.Data), text: string(msg.Data), issuedAt: issuedAtOf(msg.Data)})
		}
		// A reply that does not reach the relay leaves the frame to the relay's receipt wait,
		// which a row's own waits see; nothing here reports to t, which may have ended.
		for _, reply := range replies {
			_ = conn.Publish(msg.Reply, []byte(reply))
		}
	}); err != nil {
		t.Fatalf("subscribe %s's process to its relay subject: %v", sessionID, err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush %s's subscription: %v", sessionID, err)
	}
	t.Cleanup(func() {
		silent.Store(true)
		if err := conn.Drain(); err == nil {
			testwait.Holds(5*time.Second, testwait.Poll, conn.IsClosed)
		}
	})
	return heard
}

// storeFrames stores each of frames on sessionID's own subject, in order, as its bytes, and
// returns each one's stream sequence: a row stores this way a message no envelope of the
// listener's would marshal to (roleFramesOfEveryShape, framesNoReadCanTellTheRoleOf).
func (rig *relayRig) storeFrames(t *testing.T, sessionID string, frames []markedFrame) []uint64 {
	t.Helper()
	seqs := make([]uint64, len(frames))
	for i, frame := range frames {
		ack, err := rig.harness.client.JS().Publish(contracts.LegacyAgentSubject(sessionID), frame.data)
		if err != nil {
			t.Fatalf("store %s on %s's own subject: %v", frame.shape, sessionID, err)
		}
		seqs[i] = ack.Sequence
	}
	return seqs
}

// keptMarks is how many messages role's kept lane holds whose raw bytes hold mark (markedFrame),
// and how many it holds in all.
func keptMarks(t testing.TB, client *bus.Client, role, mark string) (int, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	messages, err := readStored(ctx, client, bus.KeptRoleSubject(role))
	if err != nil {
		t.Fatalf("read role %s's kept lane: %v", role, err)
	}
	marked := 0
	for _, message := range messages {
		if bytes.Contains(message.Data, []byte(mark)) {
			marked++
		}
	}
	return marked, len(messages)
}
