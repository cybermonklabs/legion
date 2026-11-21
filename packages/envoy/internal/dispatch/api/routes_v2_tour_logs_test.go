package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

func init() { addTourSteps("logs", logsTourSteps) }

// logsTourSteps are the route tour's steps of the five logs: each read whole and each read from a
// position, but a document's log (/artifacts/{id}/events), which the tour reads from a position
// alone.
func logsTourSteps() []tourStep {
	alice := tourPerson("alice")
	issueEvents := func(query url.Values) func(*testing.T, *routeTour) tourRequest {
		return func(t *testing.T, tour *routeTour) tourRequest {
			return tourRequest{Path: map[string]string{"key": tour.issue(t).Key}, Query: query, As: alice}
		}
	}
	return []tourStep{
		{
			Method: http.MethodGet, Pattern: "/events", Status: http.StatusOK,
			Setup: func(*testing.T, *routeTour) tourRequest { return tourRequest{As: alice} },
		},
		{
			Method: http.MethodGet, Pattern: "/events", Name: "a page above an id, newest first", Status: http.StatusOK,
			Setup: func(*testing.T, *routeTour) tourRequest {
				return tourRequest{Query: url.Values{"after": {"0"}, "sort": {"-id"}, "limit": {"5"}}, As: alice}
			},
		},
		{
			Method: http.MethodGet, Pattern: "/issues/{key}/events", Status: http.StatusOK,
			Setup:   issueEvents(nil),
			Missing: []tourMissing{{Param: "key", Value: "NOPE-404", Code: apicode.IssueNotFound}},
		},
		{
			Method: http.MethodGet, Pattern: "/issues/{key}/events", Name: "the newest page below a seq", Status: http.StatusOK,
			Setup:   issueEvents(url.Values{"before": {"1000"}, "limit": {"2"}, "offset": {"1"}}),
			Missing: []tourMissing{{Param: "key", Value: "NOPE-404", Code: apicode.IssueNotFound}},
		},
		{
			Method: http.MethodGet, Pattern: "/artifacts/{id}/events", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				return tourRequest{Path: map[string]string{"id": tour.document(t)}, Query: url.Values{"after": {"0"}}, As: alice}
			},
			Missing: []tourMissing{{Param: "id", Value: uuid.NewString(), Code: apicode.ArtifactNotFound}},
		},
		{
			Method: http.MethodGet, Pattern: "/references", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				return tourRequest{Query: url.Values{"to": {"dispatch://" + tour.issue(t).Key}}, As: alice}
			},
		},
		{
			Method: http.MethodGet, Pattern: "/references", Name: "the mentions above a source_seq", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				return tourRequest{Query: url.Values{"to": {"dispatch://" + tour.issue(t).Key}, "after": {"0"}}, As: alice}
			},
		},
		{
			Method: http.MethodGet, Pattern: "/me/threads", Status: http.StatusOK,
			Setup: func(*testing.T, *routeTour) tourRequest {
				return tourRequest{Query: url.Values{"kind": {"reply,agent"}, "state": {"inbox"}}, As: alice}
			},
		},
		{
			Method: http.MethodGet, Pattern: "/me/threads", Name: "the dismissed threads below a position", Status: http.StatusOK,
			Setup: func(*testing.T, *routeTour) tourRequest {
				before := time.Now().UTC().Format(time.RFC3339Nano) + "," + uuid.NewString()
				return tourRequest{Query: url.Values{"state": {"dismissed"}, "before": {before}}, As: alice}
			},
		},
	}
}

// document makes a project document in a project of its own, answering its id once the document
// owes no settlement: the settlement writes after the create answers, and a step's write that
// lands between the two fingerprints of unknown-keys would read as the refused request's.
func (tour *routeTour) document(t *testing.T) string {
	t.Helper()
	id := createProjectDocument(t, tour.handler, tour.project(t), "tour-notes.md", "# Notes\n\nA passage the tour reads.").ID
	awaitSettled(t, tour.database, id)
	return id
}

// issueOfEvents makes an issue whose log holds exactly events events, seq 1 to events: the API's
// own create writes the first, and the rest are written as the event broker writes an append, the
// issue's last_seq moved with them.
func (tour *routeTour) issueOfEvents(t *testing.T, events int) string {
	t.Helper()
	issue := tour.issue(t)
	key := issue.Key
	// The create queues its spec's settlement, which can append to the issue's log after the
	// create answers; the seed waits it out, so no write but the seed's moves last_seq.
	awaitSettled(t, tour.database, issue.Document)
	awaitBackgroundWrites(t, tour.served)
	seedExec(t, tour.database, "the issue's events", `
		with issue as (
			select key, last_seq from issues where key = $1 for update
		), appended as (
			insert into events (issue_key, seq, type, actor, payload, notify)
			select key, last_seq + n, 'tour.marker', '{"kind":"user","id":"alice"}'::jsonb, '{}'::jsonb, false
			from issue, generate_series(1, $2::int - last_seq) n
			returning seq
		)
		update issues set last_seq = $2::int where key = $1 and exists (select 1 from appended)
	`, key, events)
	var last int
	if err := tour.database.Pool.QueryRow(t.Context(), `select last_seq from issues where key = $1`, key).Scan(&last); err != nil {
		t.Fatalf("read the issue's last seq: %v", err)
	}
	if last != events {
		t.Fatalf("the issue's log holds %d events, want %d", last, events)
	}
	return key
}

// logAnswer is a log's answer as a client reads it: its items' positions, and its total as JSON,
// null or a number.
type logAnswer struct {
	Items []struct {
		ID  int64 `json:"id"`
		Seq int64 `json:"seq"`
	} `json:"items"`
	Total  json.RawMessage `json:"total"`
	Limit  int             `json:"limit"`
	Offset int             `json:"offset"`
}

// TestV2LogPositions is c-log-position of dispatch://INFRA-4071 (R5), with c-unknown-keys' offset
// case: each log names the field its after and before read in its index entry; an after above the
// log's last written position, or one that is no position at all, is 400 POSITION_INVALID naming
// the parameter and the field; a before past the end answers the newest page; the global event log
// and the dismissed threads answer a null total, which the global log's index entry says, and every
// other log an exact one; and an offset of a long log answers the page it names.
func TestV2LogPositions(t *testing.T) {
	tour := newRouteTour(t)
	alice := tourPerson("alice")
	get := func(t *testing.T, pattern string, path map[string]string, query url.Values) *http.Response {
		t.Helper()
		route := tour.requireMounted(t, http.MethodGet, pattern)
		return tour.send(t, route, tourRequest{Path: path, Query: query, As: alice}, "").Result()
	}
	read := func(t *testing.T, pattern string, path map[string]string, query url.Values) logAnswer {
		t.Helper()
		response := get(t, pattern, path, query)
		defer response.Body.Close()
		var answer logAnswer
		if err := json.NewDecoder(response.Body).Decode(&answer); err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("GET %s %v: %d (%v), want 200", pattern, query, response.StatusCode, err)
		}
		return answer
	}
	refused := func(t *testing.T, pattern string, path map[string]string, query url.Values, names ...string) {
		t.Helper()
		response := get(t, pattern, path, query)
		defer response.Body.Close()
		var refusal refusedAnswer
		if err := json.NewDecoder(response.Body).Decode(&refusal); err != nil {
			t.Fatalf("decode the refusal: %v", err)
		}
		if response.StatusCode != http.StatusBadRequest || refusal.Code != "POSITION_INVALID" {
			t.Fatalf("GET %s %v: %d %s %q, want 400 POSITION_INVALID", pattern, query, response.StatusCode, refusal.Code, refusal.Error)
		}
		for _, name := range names {
			if !strings.Contains(refusal.Error, name) {
				t.Errorf("GET %s %v: the refusal %q does not name %s", pattern, query, refusal.Error, name)
			}
		}
	}
	seqs := func(answer logAnswer) []int64 {
		read := make([]int64, len(answer.Items))
		for index, item := range answer.Items {
			read[index] = item.Seq
		}
		return read
	}
	forty := map[string]string{"key": tour.issueOfEvents(t, 40)}

	t.Run("an issue's after above its last seq", func(t *testing.T) {
		refused(t, "/issues/{key}/events", forty, url.Values{"after": {"41000"}}, "after", "seq")
		if answer := read(t, "/issues/{key}/events", forty, url.Values{"after": {"40"}}); len(answer.Items) != 0 || string(answer.Total) != "0" {
			t.Fatalf("after the last seq itself: %d items, total %s, want none and 0", len(answer.Items), answer.Total)
		}
	})

	t.Run("a position that is no integer of 0 or more", func(t *testing.T) {
		refused(t, "/issues/{key}/events", forty, url.Values{"after": {"abc"}}, "after", "seq")
		refused(t, "/issues/{key}/events", forty, url.Values{"before": {"-1"}}, "before", "seq")
		refused(t, "/events", nil, url.Values{"before": {"x1"}}, "before", "id")
	})

	t.Run("the global log's after above the highest id written", func(t *testing.T) {
		awaitBackgroundWrites(t, tour.served)
		var highest int64
		if err := tour.database.Pool.QueryRow(t.Context(), `select coalesce(max(id), 0) from events`).Scan(&highest); err != nil {
			t.Fatalf("read the highest event id: %v", err)
		}
		above := strconv.FormatInt(highest+1, 10)
		refused(t, "/events", nil, url.Values{"after": {above}}, "after", "id")
		refused(t, "/references", nil, url.Values{"to": {"dispatch://" + forty["key"]}, "after": {above}}, "after", "source_seq")
		if answer := read(t, "/events", nil, url.Values{"after": {strconv.FormatInt(highest, 10)}}); string(answer.Total) != "null" {
			t.Fatalf("after the highest id itself: total %s, want null", answer.Total)
		}
	})

	t.Run("a before past the end answers the newest page", func(t *testing.T) {
		answer := read(t, "/issues/{key}/events", forty, url.Values{"before": {"41000"}, "limit": {"10"}})
		want := []int64{40, 39, 38, 37, 36, 35, 34, 33, 32, 31}
		if got := seqs(answer); !slices.Equal(got, want) || string(answer.Total) != "40" {
			t.Fatalf("before=41000: seqs %v, total %s, want %v and 40", got, answer.Total, want)
		}
		if whole := read(t, "/issues/{key}/events", forty, url.Values{"before": {"41000"}}); len(whole.Items) != 40 || whole.Items[0].Seq != 40 {
			t.Fatalf("before=41000 at the default limit: %d items from seq %v, want all 40 from the newest", len(whole.Items), seqs(whole))
		}
	})

	t.Run("the dismissed threads read forward from an after", func(t *testing.T) {
		mark := tour.requireMounted(t, http.MethodPatch, "/me/marks/{kind}/{id}")
		// bob answers the first thread, as a thread in the Inbox is answered, and nobody writes in
		// the second, which alice may dismiss too: its row has no reply and no replied_at, and the
		// read answers it with its root's time (myRepliesPageRest) rather than failing the page.
		for index := range 2 {
			issue := tour.issue(t)
			thread := tour.comment(t, issue)
			if index == 0 {
				if reply := dispatchRequest(t, tour.handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
					map[string]any{"body": "An answer in alice's thread.", "reply_to": thread}, "bob"); reply.Code != http.StatusCreated {
					t.Fatalf("bob answers thread %s: %d %s", thread, reply.Code, reply.Body.String())
				}
			}
			if dismissed := tour.send(t, mark, tourRequest{
				Path: map[string]string{"kind": "thread", "id": thread},
				Body: map[string]any{"dismissed": true, "version": tour.threadVersion(t, thread)}, As: alice,
			}, ""); dismissed.Code != http.StatusOK {
				t.Fatalf("dismiss thread %s: %d %s", thread, dismissed.Code, dismissed.Body.String())
			}
		}
		type dismissedThreads struct {
			Items []struct {
				ThreadID    string    `json:"thread_id"`
				DismissedAt time.Time `json:"dismissed_at"`
			} `json:"items"`
			Total *int `json:"total"`
		}
		threads := func(t *testing.T, query url.Values) dismissedThreads {
			t.Helper()
			query.Set("state", "dismissed")
			response := tour.send(t, tour.requireMounted(t, http.MethodGet, "/me/threads"), tourRequest{Query: query, As: alice}, "")
			if response.Code != http.StatusOK {
				t.Fatalf("GET /api/v2/me/threads %v: %d %s", query, response.Code, response.Body.String())
			}
			return decodeBody[dismissedThreads](t, response)
		}
		newestFirst := threads(t, url.Values{})
		if len(newestFirst.Items) < 2 || newestFirst.Total != nil {
			t.Fatalf("the dismissed threads answered %d rows, total %v, want the two dismissed and total null", len(newestFirst.Items), newestFirst.Total)
		}
		position := func(index int) string {
			row := newestFirst.Items[index]
			return row.DismissedAt.UTC().Format(time.RFC3339Nano) + "," + row.ThreadID
		}
		newest, older := newestFirst.Items[0].ThreadID, newestFirst.Items[1].ThreadID
		if above := threads(t, url.Values{"after": {position(1)}}); len(above.Items) != 1 || above.Items[0].ThreadID != newest || above.Total != nil {
			t.Fatalf("after the older dismissal: %+v, want the newest alone, total null", above)
		}
		if between := threads(t, url.Values{"after": {position(1)}, "before": {position(0)}}); len(between.Items) != 0 || between.Total != nil {
			t.Fatalf("between the two dismissals: %+v, want none, total null", between)
		}
		if below := threads(t, url.Values{"before": {position(0)}, "limit": {"1"}}); len(below.Items) != 1 || below.Items[0].ThreadID != older {
			t.Fatalf("before the newest dismissal: %+v, want the older first", below)
		}
		// A position's thread id read as uuid.Parse reads it, in any of its spellings: a urn is the
		// thread its canonical form names.
		urn := newestFirst.Items[0].DismissedAt.UTC().Format(time.RFC3339Nano) + ",urn:uuid:" + newest
		if below := threads(t, url.Values{"before": {urn}, "limit": {"1"}}); len(below.Items) != 1 || below.Items[0].ThreadID != older {
			t.Fatalf("before the newest dismissal, its thread id a urn: %+v, want the older first", below)
		}
		if atTheNewest := threads(t, url.Values{"after": {position(0)}}); len(atTheNewest.Items) != 0 || atTheNewest.Total != nil {
			t.Fatalf("after the newest dismissal itself: %+v, want none, total null", atTheNewest)
		}
		later := newestFirst.Items[0].DismissedAt.Add(time.Hour).UTC().Format(time.RFC3339Nano)
		refused(t, "/me/threads", nil, url.Values{"state": {"dismissed"}, "after": {later + "," + uuid.NewString()}}, "after", "dismissed_at,thread_id")
		refused(t, "/me/threads", nil, url.Values{"state": {"dismissed"}, "after": {later + ",urn:uuid:" + uuid.NewString()}}, "after", "dismissed_at,thread_id")
		refused(t, "/me/threads", nil, url.Values{"state": {"dismissed"}, "after": {"abc"}}, "after", "dismissed_at,thread_id")
	})

	t.Run("an offset of a long log answers the page it names", func(t *testing.T) {
		long := map[string]string{"key": tour.issueOfEvents(t, 250)}
		answer := read(t, "/issues/{key}/events", long, url.Values{"offset": {"200"}})
		got := seqs(answer)
		if len(got) != 50 || got[0] != 201 || got[49] != 250 || string(answer.Total) != "250" {
			t.Fatalf("offset=200 of 250 events: %d items from %v, total %s, want items 201 to 250 and total 250", len(got), got, answer.Total)
		}
	})

	t.Run("the global log's total and the dismissed threads' are null and every other log's a number", func(t *testing.T) {
		if global := read(t, "/events", nil, url.Values{"limit": {"3"}}); string(global.Total) != "null" || len(global.Items) != 3 {
			t.Fatalf("GET /api/v2/events: %d items, total %s, want 3 and null", len(global.Items), global.Total)
		}
		if dismissed := read(t, "/me/threads", nil, url.Values{"state": {"dismissed"}}); string(dismissed.Total) != "null" {
			t.Fatalf("GET /api/v2/me/threads?state=dismissed: total %s, want null", dismissed.Total)
		}
		for name, log := range map[string]struct {
			pattern string
			path    map[string]string
			query   url.Values
		}{
			"an issue's":               {"/issues/{key}/events", forty, nil},
			"a document's":             {"/artifacts/{id}/events", map[string]string{"id": tour.document(t)}, nil},
			"the references":           {"/references", nil, url.Values{"to": {"dispatch://" + forty["key"]}}},
			"the threads' unread":      {"/me/threads", nil, nil},
			"the threads' whole inbox": {"/me/threads", nil, url.Values{"state": {"inbox"}}},
		} {
			answer := read(t, log.pattern, log.path, log.query)
			if _, err := strconv.Atoi(string(answer.Total)); err != nil {
				t.Errorf("%s log answered total %s, want an integer", name, answer.Total)
			}
		}
	})

	t.Run("the index entries name each log's position", func(t *testing.T) {
		response := dispatchRequest(t, tour.handler, http.MethodGet, "/api/v2", nil, "")
		if response.Code != http.StatusOK {
			t.Fatalf("GET /api/v2: %d %s", response.Code, response.Body.String())
		}
		entries := map[string]routeIndexV2Entry{}
		for _, entry := range decodeBody[routeIndexV2](t, response).Items {
			entries[entry.Method+" "+entry.Path] = entry
		}
		for route, want := range map[string]routeIndexPosition{
			"GET /api/v2/issues/{key}/events":   {Field: "seq", Type: "integer"},
			"GET /api/v2/artifacts/{id}/events": {Field: "seq", Type: "integer"},
			"GET /api/v2/events":                {Field: "id", Type: "integer"},
			"GET /api/v2/references":            {Field: "source_seq", Type: "integer"},
			"GET /api/v2/me/threads":            {Field: "dismissed_at,thread_id", Type: "time,id"},
		} {
			entry, listed := entries[route]
			switch {
			case !listed:
				t.Errorf("the index lists no %s", route)
			case entry.Position == nil || *entry.Position != want || entry.Kind != string(kindLog):
				t.Errorf("%s: kind %q, position %+v, want a log read by %+v", route, entry.Kind, entry.Position, want)
			case !strings.Contains(entry.Description, want.Field):
				t.Errorf("%s: the description does not name %s: %s", route, want.Field, entry.Description)
			}
		}
		if global := entries["GET /api/v2/events"].Description; !strings.Contains(global, "total is null") {
			t.Errorf("GET /api/v2/events: the index entry does not say its total is null: %s", global)
		}
	})
}
