package dispatch

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/sjawhar/envoy/oauth2call"
)

// Event is one event of an issue's Dispatch event log, in the one shape GET
// /api/v1/issues/{key}/events serves it (HTTPClient.EventsAfter) and Dispatch's durable stream
// carries it (DecodeEvent). CreatedAt is Dispatch's time of the event, the zero time where the
// event carries none; a review's event shares it with the review's own row, which Dispatch writes
// in the same transaction. Notify is nil where the event carries no notify. Payload is the event's
// payload as it came: data, which the event's reader decodes by the event's type.
type Event struct {
	ID        int64
	IssueKey  string
	Seq       int64
	Actor     Actor
	Notify    *bool
	Type      string
	CreatedAt time.Time
	Payload   json.RawMessage
}

// DaemonActor says whether the daemon made the event: its actor is the session the daemon writes
// every Dispatch status and message of the event's issue under (DaemonSession). It is the one
// statement of that rule, which intake's decoder (DispatchIssue.DaemonActor) and the set-back's
// check (StatusChange.DaemonActor) both read.
func (e Event) DaemonActor() bool {
	return e.Actor.Kind == "session" && e.Actor.ID == DaemonSession(e.IssueKey)
}

// DecodeEvent decodes one event as Dispatch's durable stream carries it: the bytes of a stream
// message, which answer no request of the daemon's. An absent, null or empty created_at is the
// zero time. An event that does not decode is an error in fixed text naming the member and the
// JSON kind it held (oauth2call.UnmarshalData), and a created_at that is not an RFC 3339 time
// one naming the member, neither quoting a value of the event.
func DecodeEvent(data []byte) (Event, error) {
	var event streamEvent
	if err := oauth2call.UnmarshalData(data, &event); err != nil {
		return Event{}, err
	}
	at, ok := eventTime(event.CreatedAt)
	if !ok {
		return Event{}, errors.New("the event names a created_at that is not an RFC 3339 time (the value is not printed)")
	}
	return event.event(at, event.Payload), nil
}

// eventHeader is the members of an event that both its reads decode alike: all but its time and
// its payload.
type eventHeader struct {
	ID       int64  `json:"id"`
	IssueKey string `json:"issue_key"`
	Seq      int64  `json:"seq"`
	Actor    actor  `json:"actor"`
	Notify   *bool  `json:"notify"`
	Type     string `json:"type"`
}

// event is the Event of h, created at at, carrying payload.
func (h eventHeader) event(at time.Time, payload json.RawMessage) Event {
	return Event{ID: h.ID, IssueKey: h.IssueKey, Seq: h.Seq, Actor: Actor(h.Actor), Notify: h.Notify, Type: h.Type, CreatedAt: at, Payload: payload}
}

// streamEvent is an event as Dispatch's durable stream carries it, its created_at the text it
// came as.
type streamEvent struct {
	eventHeader
	CreatedAt string          `json:"created_at"`
	Payload   json.RawMessage `json:"payload"`
}

// eventTime is a stream event's created_at read as an RFC 3339 time, and whether it read: the zero
// time for an absent, null or empty one. time.Parse's error quotes the text, so none is answered.
func eventTime(text string) (time.Time, bool) {
	if text == "" {
		return time.Time{}, true
	}
	at, err := time.Parse(time.RFC3339Nano, text)
	return at, err == nil
}

// answeredEvent is an event as GET /api/v1/issues/{key}/events answers it, decoded through the
// answer (Answer.Decode): its created_at is Dispatch's words, held with the record of the request,
// and its payload is read apart from it, as data (HTTPClient.eventPage).
type answeredEvent struct {
	eventHeader
	CreatedAt oauth2call.RemoteText `json:"created_at"`
}

// createdAt is the event's created_at read as an RFC 3339 time, and whether it read: the zero time
// for an absent, null or empty one, as eventTime reads a stream event's.
func (e answeredEvent) createdAt() (time.Time, bool) {
	if e.CreatedAt.IsEmpty() {
		return time.Time{}, true
	}
	return e.CreatedAt.Time(time.RFC3339Nano)
}
