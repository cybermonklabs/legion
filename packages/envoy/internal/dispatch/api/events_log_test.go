package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

type eventLogRow struct {
	ID         int64   `json:"id"`
	Type       string  `json:"type"`
	IssueKey   *string `json:"issue_key"`
	ArtifactID *string `json:"artifact_id"`
	Project    string  `json:"project"`
}

func readEventLogPage(t *testing.T, handler http.Handler, query string) ([]eventLogRow, map[int64]json.RawMessage) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/events/log"+query, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/events/log%s: status=%d body=%s", query, response.Code, response.Body.String())
	}
	raw := decodeBody[[]json.RawMessage](t, response)
	rows := make([]eventLogRow, len(raw))
	byID := make(map[int64]json.RawMessage, len(raw))
	for index, event := range raw {
		if err := json.Unmarshal(event, &rows[index]); err != nil {
			t.Fatalf("decode event %s: %v", event, err)
		}
		byID[rows[index].ID] = event
	}
	return rows, byID
}

// readEventLogBehindHorizon reads the event log's first page until holds accepts it: the log
// answers only the events at or below the server's safe horizon (sinceHorizon), which a
// transaction held anywhere on the Postgres instance keeps back (awaitHorizonRead).
func readEventLogBehindHorizon(t *testing.T, handler http.Handler, database *store.Store, what string, holds func([]eventLogRow) bool) ([]eventLogRow, map[int64]json.RawMessage) {
	t.Helper()
	type page struct {
		rows []eventLogRow
		byID map[int64]json.RawMessage
	}
	read := awaitHorizonRead(t, database, what, func() page {
		rows, byID := readEventLogPage(t, handler, "")
		return page{rows: rows, byID: byID}
	}, func(read page) bool { return holds(read.rows) }, func(read page) string { return fmt.Sprintf("%+v", read.rows) })
	return read.rows, read.byID
}

// The event log is every event whatever owns it: a project's own event, an issue's and a project
// document's all appear, in id order, each exactly as its owner's event list serves it.
func TestEventLogPagesEveryEventAcrossOwnersInIDOrder(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "CORE", "Logged issue", "A spec")
	if response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Which path?",
		"actor":    sessionActor(),
	}); response.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", response.Code, response.Body.String())
	}
	document := createProjectDocument(t, handler, "CORE", "Design notes", "# Design notes\n")

	owners := func(rows []eventLogRow) (sawProject, sawIssue, sawDocument bool) {
		for _, row := range rows {
			switch {
			case row.Type == "project.created" && row.IssueKey == nil && row.ArtifactID == nil && row.Project == "CORE":
				sawProject = true
			case row.IssueKey != nil && *row.IssueKey == issue.Key:
				sawIssue = true
			case row.Type == "artifact.created" && row.IssueKey == nil && row.ArtifactID != nil && *row.ArtifactID == document.ID:
				sawDocument = true
			}
		}
		return sawProject, sawIssue, sawDocument
	}
	rows, logged := readEventLogBehindHorizon(t, handler, database, "the event log, with the project's, the issue's and the document's events", func(rows []eventLogRow) bool {
		sawProject, sawIssue, sawDocument := owners(rows)
		return sawProject && sawIssue && sawDocument
	})
	for index, row := range rows {
		if index > 0 && row.ID <= rows[index-1].ID {
			t.Fatalf("event %d has id %d after id %d; want ascending ids", index, row.ID, rows[index-1].ID)
		}
	}

	for _, owner := range []string{"/api/v1/issues/" + issue.Key + "/events", "/api/v1/artifacts/" + document.ID + "/events"} {
		response := dispatchRequest(t, handler, http.MethodGet, owner, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s: status=%d body=%s", owner, response.Code, response.Body.String())
		}
		listed := decodeBody[[]json.RawMessage](t, response)
		if len(listed) == 0 {
			t.Fatalf("GET %s listed no events", owner)
		}
		for _, event := range listed {
			var row eventLogRow
			if err := json.Unmarshal(event, &row); err != nil {
				t.Fatalf("decode %s event %s: %v", owner, event, err)
			}
			if got, ok := logged[row.ID]; !ok || string(got) != string(event) {
				t.Fatalf("event %d is %s on %s but %s in the event log", row.ID, event, owner, got)
			}
		}
	}
}

// Paging by the last id read with a small limit yields the same events as one page: no gap and
// no repeat, ending on a short page.
func TestEventLogPagesByAfterWithoutGapOrRepeat(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "CORE", "Paged issue", "A spec")
	for index := range 3 {
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
			"body": fmt.Sprintf("comment %d", index),
		}, "alice"); response.Code != http.StatusCreated {
			t.Fatalf("create comment: status=%d body=%s", response.Code, response.Body.String())
		}
	}
	createProjectDocument(t, handler, "CORE", "Paged notes", "# Paged notes\n")

	// Once one page holds them, the horizon stands above every event written so far, so the pages
	// below read the same log.
	whole, _ := readEventLogBehindHorizon(t, handler, database, "the event log, at least 5 events to page", func(rows []eventLogRow) bool {
		return len(rows) >= 5
	})
	var paged []eventLogRow
	after := int64(0)
	for {
		page, _ := readEventLogPage(t, handler, fmt.Sprintf("?after=%d&limit=2", after))
		paged = append(paged, page...)
		if len(page) < 2 {
			break
		}
		after = page[len(page)-1].ID
	}
	if len(paged) != len(whole) {
		t.Fatalf("paged %d events, one page holds %d", len(paged), len(whole))
	}
	for index := range whole {
		if paged[index].ID != whole[index].ID {
			t.Fatalf("paged event %d has id %d; one page has %d there", index, paged[index].ID, whole[index].ID)
		}
	}

	from := whole[1].ID
	rest, _ := readEventLogPage(t, handler, fmt.Sprintf("?after=%d", from))
	if len(rest) != len(whole)-2 || rest[0].ID != whole[2].ID {
		t.Fatalf("after=%d answered %+v; want the events after it, starting at id %d", from, rest, whole[2].ID)
	}
}

func TestEventLogRefusesAnInvalidQuery(t *testing.T) {
	t.Parallel()
	handler, _ := newTestHandlerWithStore(t)
	for _, query := range []string{"?after=-1", "?after=x", "?limit=0", "?limit=1001", "?limit=x"} {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/events/log"+query, nil, "alice")
		if response.Code != http.StatusBadRequest {
			t.Fatalf("GET /api/v1/events/log%s: status=%d body=%s; want 400", query, response.Code, response.Body.String())
		}
		if code := decodeBody[map[string]any](t, response)["code"]; code != "INVALID_QUERY" {
			t.Fatalf("GET /api/v1/events/log%s: code=%v; want INVALID_QUERY", query, code)
		}
	}
}

// The log admits exactly whom the live socket admits: any signed-in human or agent, and no
// anonymous caller.
func TestEventLogRefusesAnUnauthenticatedCallerAsTheLiveSocketDoes(t *testing.T) {
	t.Parallel()
	handler, _ := newTestHandlerWithStore(t)
	socket := dispatchRequest(t, handler, http.MethodGet, "/ws/live", nil, "")
	log := dispatchRequest(t, handler, http.MethodGet, "/api/v1/events/log", nil, "")
	if socket.Code != http.StatusUnauthorized || log.Code != socket.Code || log.Body.String() != socket.Body.String() {
		t.Fatalf("anonymous event log: %d %s; the live socket answers %d %s", log.Code, log.Body.String(), socket.Code, socket.Body.String())
	}
	if response := sessionRequest(t, handler, http.MethodGet, "/api/v1/events/log", nil); response.Code != http.StatusOK {
		t.Fatalf("bearer event log: status=%d body=%s", response.Code, response.Body.String())
	}
}
