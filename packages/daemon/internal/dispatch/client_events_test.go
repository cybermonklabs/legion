package dispatch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The reads of an issue's Dispatch event log: the outbox's recovery of the message bodies it
// posted (MessageBodiesSince) and the boot catch-up's read after a sequence (EventsAfter).

func TestMessageBodiesSincePagesNewestFirstUntilTheOutboxWindow(t *testing.T) {
	since := time.Date(2026, 9, 23, 1, 0, 0, 0, time.UTC)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/issues/LEGION-5/events" {
			t.Fatalf("request = %s %s, want GET /api/v1/issues/LEGION-5/events", r.Method, r.URL.Path)
		}
		assertBearer(t, r)
		requests++
		switch requests {
		case 1:
			if got := r.URL.Query(); got.Get("order") != "desc" || got.Get("limit") != "200" || got.Get("before") != "" {
				t.Fatalf("first query = %s, want newest-first page", r.URL.RawQuery)
			}
			events := make([]map[string]any, 200)
			for i := range events {
				events[i] = map[string]any{"seq": 202 - i, "type": "issue.updated", "created_at": "2026-09-23T01:01:00Z", "payload": map[string]string{"status": "testing"}}
			}
			events[0] = map[string]any{"seq": 202, "type": "message.created", "created_at": "2026-09-23T01:02:00Z", "payload": map[string]string{"body": "Existing daemon notice."}}
			writeJSON(t, w, events)
		case 2:
			if got := r.URL.Query(); got.Get("order") != "desc" || got.Get("limit") != "200" || got.Get("before") != "3" {
				t.Fatalf("second query = %s, want next newest-first page", r.URL.RawQuery)
			}
			writeJSON(t, w, []map[string]any{
				{"seq": 2, "type": "message.created", "created_at": "2026-09-23T00:59:59Z", "payload": map[string]string{"body": "Outside the outbox window."}},
			})
		default:
			t.Fatalf("unexpected request %d", requests)
		}
	}))
	defer server.Close()

	bodies, err := New(server.URL, BearerTransport(dispatchToken, nil)).MessageBodiesSince(context.Background(), "LEGION-5", since)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if requests != 2 || len(bodies) != 1 || bodies[0] != "Existing daemon notice." {
		t.Fatalf("requests=%d bodies=%#v, want one recent message", requests, bodies)
	}
}

// An event carries at most the first part of a long body and says so; the whole of it, the outbox
// marker at its end included, is read from the message itself. A body the event carries whole is
// read from the event alone, so the common message costs no second request.
func TestMessageBodiesSinceReadsACutBodyFromItsMessage(t *testing.T) {
	since := time.Date(2026, 9, 23, 1, 0, 0, 0, time.UTC)
	whole := strings.Repeat("x", 40000) + "\n\n<!-- legion-outbox:7 -->"
	reads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertBearer(t, r)
		switch r.URL.Path {
		case "/api/v1/issues/LEGION-5/events":
			writeJSON(t, w, []map[string]any{
				{"seq": 9, "type": "message.created", "created_at": "2026-09-23T01:02:00Z",
					"payload": map[string]any{"id": "f1c0ffee-0000-4000-8000-000000000001", "body": whole[:32000], "body_truncated": true}},
				{"seq": 8, "type": "message.created", "created_at": "2026-09-23T01:01:00Z",
					"payload": map[string]any{"id": "f1c0ffee-0000-4000-8000-000000000002", "body": "A short daemon notice."}},
			})
		case "/api/v1/issues/LEGION-5/messages/f1c0ffee-0000-4000-8000-000000000001":
			reads++
			writeJSON(t, w, map[string]any{"message": map[string]any{"id": "f1c0ffee-0000-4000-8000-000000000001", "body": whole}, "replies": []any{}})
		default:
			t.Fatalf("request = %s %s, want the issue's events or its cut message", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	bodies, err := New(server.URL, BearerTransport(dispatchToken, nil)).MessageBodiesSince(context.Background(), "LEGION-5", since)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if reads != 1 || len(bodies) != 2 || bodies[0] != whole || bodies[1] != "A short daemon notice." {
		t.Fatalf("message reads=%d bodies=%d, want the cut body read whole from its message and the short one from its event alone", reads, len(bodies))
	}
}

// The boot catch-up reads an issue's event log after a sequence, oldest first, page after page
// until a short page, each event in the shape Dispatch serves it; a page that does not ascend past
// the last sequence read is refused rather than read again.
func TestEventsAfterPagesTheIssuesEventLogInOrder(t *testing.T) {
	var afters []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/issues/LEGION-1/events" || r.URL.Query().Get("limit") != "200" {
			t.Fatalf("request = %s %s", r.Method, r.URL.String())
		}
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		afters = append(afters, r.URL.Query().Get("after"))
		count := 200
		if after >= 200 {
			count = 3
		}
		page := make([]map[string]any, 0, count)
		for seq := after + 1; seq <= after+int64(count); seq++ {
			page = append(page, map[string]any{"seq": seq, "type": "comment.created", "issue_key": "LEGION-1"})
		}
		writeJSON(t, w, page)
	}))
	defer server.Close()

	events, err := New(server.URL, BearerTransport(dispatchToken, nil)).EventsAfter(context.Background(), "LEGION-1", 7)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if !reflect.DeepEqual(afters, []string{"7", "207"}) || len(events) != 203 {
		t.Fatalf("pages after %v gave %d events, want pages after 7 and 207 giving 203", afters, len(events))
	}
	if last := events[len(events)-1]; last.Seq != 210 || last.Type != "comment.created" || last.IssueKey != "LEGION-1" {
		t.Fatalf("last event = %+v, want comment.created of LEGION-1 at seq 210", last)
	}

	stuck := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, []map[string]any{{"seq": 7, "type": "comment.created"}})
	}))
	defer stuck.Close()
	if _, err := New(stuck.URL, BearerTransport(dispatchToken, nil)).EventsAfter(context.Background(), "LEGION-1", 7); err == nil || !strings.Contains(err.Error(), "non-ascending") {
		t.Fatalf("a page at the sequence already read = %v, want refused", err)
	}
}

// A page that holds an event the client cannot read fails the read with an *EventLogError naming
// the event's place in the log read so far, counted from 1, here on the second page: an event
// whose seq does not decode, with sequence 0, an event at the sequence already read and one whose
// created_at is not a time, each with its sequence. A decode's refusal is Answer.Decode's fixed
// text, which names the member and the JSON kind it held and quotes no value; the others are the
// read's own text.
func TestEventsAfterNamesTheEventOfAPageItCannotRead(t *testing.T) {
	const page2 = "decode Dispatch GET /api/v1/issues/LEGION-1/events?after=207&limit=200 response: "
	for _, tc := range []struct {
		name string
		bad  map[string]any
		seq  int64
		// text is the error's whole text; where the decoder words it, names are what it holds
		// and value is what it never quotes.
		text  string
		names []string
		value string
	}{
		{name: "a seq that does not decode", bad: map[string]any{"seq": "210", "type": "comment.created"},
			names: []string{page2, "seq", "JSON string"}, value: "210"},
		{name: "a seq at the sequence already read", bad: map[string]any{"seq": 209, "type": "comment.created"}, seq: 209,
			text: "page issue events for LEGION-1: non-ascending sequence 209 after 209"},
		{name: "a created_at that is not a time", bad: map[string]any{"seq": 210, "type": "comment.created", "created_at": "yesterday"}, seq: 210,
			text: page2 + "event 3 of the page, at sequence 210, names a created_at that is not an RFC 3339 time (the value is not printed)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The first page holds sequences 8 to 207 and the second 208, 209 and the bad event,
			// the log's 203rd.
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
				if after == 7 {
					page := make([]map[string]any, 0, 200)
					for seq := after + 1; seq <= after+200; seq++ {
						page = append(page, map[string]any{"seq": seq, "type": "comment.created", "issue_key": "LEGION-1"})
					}
					writeJSON(t, w, page)
					return
				}
				writeJSON(t, w, []map[string]any{{"seq": 208, "type": "comment.created"}, {"seq": 209, "type": "comment.created"}, tc.bad})
			}))
			defer server.Close()

			events, err := New(server.URL, BearerTransport(dispatchToken, nil)).EventsAfter(context.Background(), "LEGION-1", 7)
			var unread *EventLogError
			if !errors.As(err, &unread) || unread.Event != 203 || unread.Seq != tc.seq || events != nil {
				t.Fatalf("EventsAfter = %d events, %#v; want an *EventLogError naming event 203, seq %d", len(events), err, tc.seq)
			}
			if tc.text != "" && err.Error() != tc.text {
				t.Fatalf("EventsAfter's error = %q, want %q", err, tc.text)
			}
			for _, name := range tc.names {
				if !strings.Contains(err.Error(), name) {
					t.Errorf("EventsAfter's error = %q, want one naming %q", err, name)
				}
			}
			if tc.value != "" && strings.Contains(err.Error(), tc.value) {
				t.Errorf("EventsAfter's error = %q quotes the value %q", err, tc.value)
			}
		})
	}
}
