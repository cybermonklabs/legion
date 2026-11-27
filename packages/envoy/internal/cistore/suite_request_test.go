package cistore_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/webhook"
)

// A check_suite delivery whose action is requested or rerequested is GitHub asking the suite's App
// to run its checks: through the listener's real webhook route it leaves the head's record as it
// was, so the queued suite it carries never holds the head. A completed delivery of the same
// suite is recorded. The store opens on a fixture's connection (bustest.Fixture), so the open
// creates the CI bucket on the test's own server, as every other test of the package's does.
func TestASuiteRequestLeavesTheRecordAsItWas(t *testing.T) {
	_, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	defer client.Close()
	store, err := cistore.Open(t.Context(), bustest.Fixture(client.Conn), logging.New("test"), cistore.WithReplicas(1), cistore.WithTTL(time.Hour))
	if err != nil {
		t.Fatalf("open cistore: %v", err)
	}
	ready, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.WaitForCacheReady(ready); err != nil {
		t.Fatalf("cache ready: %v", err)
	}
	kv, err := client.JS().KeyValue(bus.CIStateBucket)
	if err != nil {
		t.Fatalf("bind the CI bucket: %v", err)
	}

	const (
		secret = "s"
		sha    = "abcdef1234567890abcdef1234567890abcdef12"
	)
	key := cistore.Key("example-org", "example-repo", "42", sha)
	handler := webhook.GitHubHandler(secret, "@legion", "", "", client, store, webhook.NewBodyBudget(webhook.DefaultBodyBudget, nil))
	post := func(event, delivery, body string) {
		t.Helper()
		req := httptest.NewRequest("POST", "/webhook/github", strings.NewReader(body))
		req.Header.Set("X-GitHub-Delivery", delivery)
		req.Header.Set("X-GitHub-Event", event)
		req.Header.Set("X-Hub-Signature-256", sign(secret, []byte(body)))
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("%s delivery %s answered %d, body=%s", event, delivery, rr.Code, rr.Body.String())
		}
	}
	suite := func(action, status, conclusion string) string {
		return fmt.Sprintf(`{
			"action": %q,
			"check_suite": {"id": 9200, "status": %q, "conclusion": %q, "updated_at": "2026-11-22T03:00:00Z",
				"latest_check_runs_count": 0, "head_sha": %q, "app": {"id": 5789269}, "pull_requests": [{"number": 42}]},
			"repository": {"name": "example-repo", "owner": {"login": "example-org"}}
		}`, action, status, conclusion, sha)
	}
	// stored is the record's revision and the suite 9200 it holds, nil for none.
	stored := func() (uint64, map[string]any) {
		t.Helper()
		entry, err := kv.Get(key)
		if err != nil {
			t.Fatalf("read the record: %v", err)
		}
		var record struct {
			Suites map[string]map[string]any `json:"suites"`
		}
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			t.Fatalf("decode the record: %v", err)
		}
		return entry.Revision(), record.Suites["9200"]
	}

	post("check_run", "d-run", fmt.Sprintf(`{
		"action": "completed",
		"check_run": {"id": 1, "name": "build", "status": "completed", "conclusion": "success", "completed_at": "2026-11-22T02:59:00Z",
			"head_sha": %q, "check_suite": {"id": 9100}, "app": {"id": 15368}, "pull_requests": [{"number": 42}]},
		"repository": {"name": "example-repo", "owner": {"login": "example-org"}}
	}`, sha))
	before, held := stored()
	if held != nil {
		t.Fatalf("the record holds suite 9200 before any delivery of it: %v", held)
	}
	for _, action := range []string{"requested", "rerequested"} {
		post("check_suite", "d-"+action, suite(action, "queued", ""))
		if revision, held := stored(); revision != before || held != nil {
			t.Fatalf("after a %s delivery the record is at revision %d holding suite 9200 as %v, want revision %d and no such suite", action, revision, held, before)
		}
	}
	post("check_suite", "d-completed", suite("completed", "completed", "success"))
	if revision, held := stored(); revision == before || held["status"] != "completed" || held["app_id"] != "5789269" {
		t.Fatalf("after a completed delivery the record is at revision %d holding suite 9200 as %v, want a new revision holding it completed", revision, held)
	}
}
