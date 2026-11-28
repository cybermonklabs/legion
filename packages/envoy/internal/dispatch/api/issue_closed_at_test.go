package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestAClosesEventCarriesTheRowsFinalClosedAt: a close through PATCH /api/v1/issues/{key} stamps
// the row's closed_at twice. Its update writes the start of its transaction, and its write phase
// stamps the row again once the asks' withdrawal and their document writes have run
// (clock_timestamp(), api/issue_patch.go). A read at a past time takes closed_at from the
// issue.closed payload alone (asof's issuesShadow), and a later reopen sets the row's closed_at to
// null. So the event must record the stamp that the row keeps: the row, the response, the stored
// payload and the published event give one closed_at, each written as the response writes it.
// That stamp is later than the start of the close's transaction, so a payload that held the first
// stamp fails here. Then the issue is reopened at once, and a read at a cut between the close and
// the reopen answers the stamp that the close left on the row.
func TestAClosesEventCarriesTheRowsFinalClosedAt(t *testing.T) {
	handler, database, deps, served := newRegisteredTestServer(t, testServerOptions{testHooks: true})
	settle := func() {
		t.Helper()
		asOfWrite(t, handler, http.MethodPost, "/api/v1/artifacts/_test/quiesce", nil, "alice")
		awaitBackgroundWrites(t, served)
		safeHorizon(t, served)
	}
	const project = "CLAT"
	asOfWrite(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": project, "name": "The stamp of a close"}, "alice")
	issue := asOfIssue(t, handler, map[string]any{"project": project, "title": "Close, then reopen"})
	asOfSessionWrite(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{"question": "Does the close withdraw this ask?"})
	settle()

	published, unsubscribe := deps.Events.Subscribe()
	defer unsubscribe()
	answered, _ := decodeBody[map[string]any](t, asOfWrite(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key,
		map[string]any{"status": "done"}, "alice"))["closed_at"].(string)
	answeredAt, err := time.Parse(time.RFC3339Nano, answered)
	if err != nil {
		t.Fatalf("the close's response gives closed_at %q, want a time: %v", answered, err)
	}

	var row *time.Time
	if err := database.Pool.QueryRow(context.Background(), `select closed_at from issues where key = $1`, issue.Key).Scan(&row); err != nil {
		t.Fatalf("read the closed row of %s: %v", issue.Key, err)
	}
	var stored string
	var began time.Time
	if err := database.Pool.QueryRow(context.Background(), `
		select payload->>'closed_at', created_at from events where issue_key = $1 and type = 'issue.closed'
	`, issue.Key).Scan(&stored, &began); err != nil {
		t.Fatalf("read the issue.closed of %s: %v", issue.Key, err)
	}
	if row == nil || !row.Equal(answeredAt) {
		t.Fatalf("the row of %s holds closed_at %v, want the response's %s", issue.Key, row, answered)
	}
	if !row.After(began) {
		t.Fatalf("the row of %s holds closed_at %s, which is not later than the start of the close's transaction %s: "+
			"the test cannot tell the close's two stamps apart", issue.Key, row, began)
	}
	if stored != answered {
		t.Errorf("the stored issue.closed of %s gives closed_at %q, want the response's %q, the row's final stamp", issue.Key, stored, answered)
	}
	for {
		event := testwait.Receive(t, "the close's issue.closed on the live stream", published)
		if event.Type == "" {
			t.Fatal("the live stream's subscription closed before the close's issue.closed was published")
		}
		if event.Type != "issue.closed" || event.IssueKey == nil || *event.IssueKey != issue.Key {
			continue
		}
		encoded, err := json.Marshal(event.Payload)
		if err != nil {
			t.Fatalf("encode the published payload: %v", err)
		}
		var payload struct {
			ClosedAt string `json:"closed_at"`
		}
		if err := json.Unmarshal(encoded, &payload); err != nil {
			t.Fatalf("decode the published payload: %v", err)
		}
		if payload.ClosedAt != answered {
			t.Errorf("the published issue.closed of %s gives closed_at %q, want the response's %q, the row's final stamp",
				issue.Key, payload.ClosedAt, answered)
		}
		break
	}
	settle()

	cut := databaseCut(t, database)
	time.Sleep(5 * time.Millisecond)
	asOfWrite(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{"status": "todo"}, "alice")
	settle()
	if present, _ := readAsAlice[model.Issue](t, handler, "/api/v1/issues/"+issue.Key); present.ClosedAt != nil {
		t.Fatalf("the reopened %s reads closed_at %s now, want none, so the read at the cut proves nothing", issue.Key, present.ClosedAt)
	}
	read, _ := readAsAlice[model.Issue](t, handler, withAsOf(t, "/api/v1/issues/"+issue.Key, cut))
	if read.ClosedAt == nil || !read.ClosedAt.Equal(*row) {
		t.Errorf("%s read at the cut %s, between its close and its reopen, gives closed_at %v, want %s, the stamp the close left on the row",
			issue.Key, cut, read.ClosedAt, row)
	}
}
