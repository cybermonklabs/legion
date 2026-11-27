package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// servedPage is a stand-in for Dispatch that answers every read of an event log with page, each
// {auth} in it replaced by the Authorization the read carried.
func servedPage(t *testing.T, page string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, strings.ReplaceAll(page, "{auth}", r.Header.Get("Authorization")))
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// An event reads as one Event whether it comes from the event log (EventsAfter) or from the
// durable stream's bytes (DecodeEvent): its members, its created_at as a time, the zero time for
// none, a null or an empty one, and its payload as it came.
func TestAnEventReadsAlikeFromTheLogAndTheStream(t *testing.T) {
	notify := true
	const payload = `{"id":"ask-1","kind":"question","approval":{"version":4,"list":[1,"two",null]}}`
	for _, tc := range []struct {
		name, createdAt string
		want            time.Time
	}{
		{"a time", `,"created_at":"2026-11-23T03:04:05.678Z"`, time.Date(2026, 11, 23, 3, 4, 5, 678000000, time.UTC)},
		{"no time", ``, time.Time{}},
		{"a null time", `,"created_at":null`, time.Time{}},
		{"an empty time", `,"created_at":""`, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"id":41,"issue_key":"LEGION-9","seq":3,"type":"ask.opened","notify":true,` +
				`"actor":{"kind":"session","id":"legion-daemon:LEGION"}` + tc.createdAt + `,"payload":` + payload + `}`
			want := Event{ID: 41, IssueKey: "LEGION-9", Seq: 3, Actor: Actor{Kind: "session", ID: "legion-daemon:LEGION"}, Notify: &notify,
				Type: "ask.opened", CreatedAt: tc.want, Payload: json.RawMessage(payload)}
			served, err := New(servedPage(t, "["+raw+"]"), BearerTransport(dispatchToken, nil)).EventsAfter(context.Background(), "LEGION-9", 0)
			if err != nil || len(served) != 1 || !reflect.DeepEqual(served[0], want) {
				t.Fatalf("EventsAfter = %+v, %v; want the one event %+v", served, err, want)
			}
			streamed, err := DecodeEvent([]byte(raw))
			if err != nil || !reflect.DeepEqual(streamed, want) {
				t.Fatalf("DecodeEvent = %+v, %v; want %+v", streamed, err, want)
			}
			if !served[0].DaemonActor() {
				t.Fatalf("the event of %s's own session reads as another's", DaemonSession("LEGION-9"))
			}
		})
	}
}

// An event log Dispatch, or whatever answers in its place, serves with an event that does not
// read - a created_at that is not a time, here the Authorization the read carried, echoed; a
// created_at or a sequence of the wrong JSON kind; an event that is no object - is refused, and the
// durable stream's decoder refuses the same event. Each error names the member, or the event's
// place, where its decoder can, and holds no run of the bearer. A box's own type error in a list
// names no member and encoding/json adds none, so the log's refusal of a created_at that is a
// number names the member as Answer.Decode finds it, the list's item as "(item)", and the JSON
// kind. Captures dispatch.event-log/<case> and dispatch.event-stream/<case>.
func TestAnEventThatDoesNotReadIsRefusedWithoutItsText(t *testing.T) {
	const token = "dsp_event-RDX-5c1a9e7f3b"
	for _, tc := range []struct {
		name, event string
		// served and streamed are what EventsAfter's and DecodeEvent's errors name.
		served, streamed []string
	}{
		{name: "time", event: `{"id":1,"issue_key":"LEGION-9","seq":3,"type":"ask.opened","notify":true,"created_at":"{auth}","payload":{}}`,
			served: []string{"event 1 of the page, at sequence 3", "created_at"}, streamed: []string{"created_at"}},
		{name: "time-number", event: `{"id":1,"issue_key":"LEGION-9","seq":3,"type":"ask.opened","notify":true,"created_at":7,"payload":{"note":"{auth}"}}`,
			served: []string{`"(item).created_at"`, "JSON number"}, streamed: []string{"created_at", "JSON number"}},
		{name: "sequence", event: `{"id":1,"issue_key":"LEGION-9","seq":"{auth}","type":"ask.opened","notify":true,"payload":{}}`,
			served: []string{"seq", "JSON string"}, streamed: []string{"seq", "JSON string"}},
		{name: "no-object", event: `"{auth}"`,
			served: []string{"JSON string"}, streamed: []string{"JSON string"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(servedPage(t, "["+tc.event+"]"), BearerTransport(token, nil)).EventsAfter(context.Background(), "LEGION-9", 0)
			oauth2calltest.RequireNoPiece(t, "dispatch.event-log/"+tc.name, fmt.Sprint(err), token)
			for _, name := range tc.served {
				if err == nil || !strings.Contains(err.Error(), name) {
					t.Errorf("EventsAfter = %v, want an error naming %s", err, name)
				}
			}
			_, err = DecodeEvent([]byte(strings.ReplaceAll(tc.event, "{auth}", "Bearer "+token)))
			oauth2calltest.RequireNoPiece(t, "dispatch.event-stream/"+tc.name, fmt.Sprint(err), token)
			for _, name := range tc.streamed {
				if err == nil || !strings.Contains(err.Error(), name) {
					t.Errorf("DecodeEvent = %v, want an error naming %s", err, name)
				}
			}
		})
	}
}
