package webhook

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// sharedDelivery is the X-GitHub-Delivery GUID of the event below. GitHub documents one GUID for
// each event, shared by every webhook that receives it, so two GitHub Apps installed on one
// repository deliver that event under one GUID, each naming its own App in the body's top-level
// installation member and in the X-GitHub-Hook-Installation-Target-ID header.
const sharedDelivery = "72d3162e-cc78-11e3-81ab-4c9367dc0958"

// appDelivery is the headers App id's webhook delivers with, beside GitHub's own three.
func appDelivery(id string) map[string]string {
	return map[string]string{"X-GitHub-Hook-Installation-Target-Type": "integration", "X-GitHub-Hook-Installation-Target-ID": id}
}

// A GitHub envelope is keyed by the delivery's GUID (contracts.GithubEnvelope), so the two Apps'
// copies of one event, which differ in their installation member and their hook-target header
// alone, leave one message on each topic the event is published on, under the key `github.<GUID>`,
// through the handler and a real NATS server; one body under two GUIDs is two events and leaves
// two. A kind the listener publishes nowhere leaves nothing for either copy. Every topic kind the
// listener publishes under has a row; a kind added with no row fails the test.
func TestOneGUIDWithTwoInstallationsIsOneStreamMessage(t *testing.T) {
	rows := []struct {
		name, event string
		body        map[string]any
		silent      bool // the listener publishes this event nowhere
	}{
		{name: "check_run of a pull request", event: "check_run", body: githubFixture(t, "check-run-first"), silent: true},
		{name: "check_run of an Actions job on the default branch", event: "check_run", body: githubFixture(t, "check-run-actions-job-without-pr")},
		{name: "check_suite of a pull request", event: "check_suite", body: githubFixture(t, "check-suite-first"), silent: true},
		{name: "issue_comment on an issue", event: "issue_comment", body: githubFixture(t, "issue-comment-created")},
		{name: "issue_comment that mentions the trigger", event: "issue_comment", body: withComment(githubFixture(t, "issue-comment-pr-created"), "@legion please review")},
		{name: "issues labeled", event: "issues", body: map[string]any{
			"action":     "labeled",
			"issue":      map[string]any{"number": 7, "title": "Widget", "body": "It breaks.", "updated_at": "2026-11-22T00:00:00Z", "user": map[string]any{"login": "octocat"}},
			"label":      map[string]any{"id": 11, "name": "bug"},
			"repository": githubRepository(),
			"sender":     map[string]any{"login": "octocat", "type": "User"},
		}},
		{name: "merge_group asking for checks", event: "merge_group", body: mergeGroupBody(), silent: !slices.Contains(contracts.GithubTopicKinds, "merge_group")},
		{name: "pull_request opened", event: "pull_request", body: githubFixture(t, "pull-request-opened")},
		{name: "pull_request_review submitted", event: "pull_request_review", body: githubFixture(t, "pull-request-review-submitted")},
		{name: "pull_request_review_comment created", event: "pull_request_review_comment", body: githubFixture(t, "pull-request-review-comment-created")},
		{name: "push to a branch", event: "push", body: githubFixture(t, "push")},
		{name: "status of a commit", event: "status", silent: true, body: map[string]any{
			"sha": "1111111111111111111111111111111111111111", "context": "ci/build", "state": "success",
			"target_url": "https://example.invalid/build/1", "repository": githubRepository(),
			"sender": map[string]any{"login": "octocat", "type": "User"},
		}},
		{name: "sub_issues added", event: "sub_issues", body: githubFixture(t, "sub-issues-added")},
		{name: "workflow_run completed", event: "workflow_run", body: githubFixture(t, "workflow-run-completed")},
	}
	var published []string
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			client := githubStream(t)
			handler := GitHubHandler("s", "@legion", "", "", client, &mockRecorder{}, NewBodyBudget(DefaultBodyBudget, nil))
			first := withInstallation(t, row.body, 1001)
			second := withInstallation(t, row.body, 2002)
			if bytes.Equal(first, second) {
				t.Fatal("the two Apps' bodies are the same bytes; the row needs them to differ in installation")
			}
			want := githubTopics(t, row.event, first)
			if row.silent != (len(want) == 0) {
				t.Fatalf("the listener publishes this %s body on %v; the row says silent=%v", row.event, want, row.silent)
			}
			published = append(published, want...)

			postDelivery(t, handler, row.event, sharedDelivery, first, appDelivery("5341240"))
			postDelivery(t, handler, row.event, sharedDelivery, second, appDelivery("5341305"))

			held := githubStreamEnvelopes(t, client)
			if got := envelopeTopics(held); !slices.Equal(got, want) {
				t.Fatalf("the stream holds %d messages on %v, want one on each of %v", len(got), got, want)
			}
			for _, item := range held {
				if item.SourceEventID != sharedDelivery || item.DedupeKey != "github."+sharedDelivery {
					t.Errorf("the copy on %s has source event id %q and dedupe key %q, want the delivery %s and github. before it",
						item.Topic, item.SourceEventID, item.DedupeKey, sharedDelivery)
				}
			}
		})
	}

	t.Run("one body under two GUIDs", func(t *testing.T) {
		client := githubStream(t)
		handler := GitHubHandler("s", "@legion", "", "", client, &mockRecorder{}, NewBodyBudget(DefaultBodyBudget, nil))
		body := withInstallation(t, githubFixture(t, "push"), 1001)
		postDelivery(t, handler, "push", sharedDelivery, body, appDelivery("5341240"))
		postDelivery(t, handler, "push", "9a0c5d8e-cc78-11e3-8c2f-1c5a2b7d3e41", body, appDelivery("5341240"))
		held := githubStreamEnvelopes(t, client)
		if len(held) != 2 || held[0].DedupeKey != "github."+sharedDelivery || held[1].DedupeKey != "github.9a0c5d8e-cc78-11e3-8c2f-1c5a2b7d3e41" {
			t.Fatalf("the stream holds %d messages on %v, want one of each GUID", len(held), envelopeTopics(held))
		}
	})

	// A check suite and a pull request's check runs are folded into the CI store and published
	// only as its settlement (contracts.GithubEnvelopes), so the ci kind reaches the stream from no
	// delivery.
	covered := map[string]bool{"ci": true}
	for _, topic := range published {
		for _, token := range strings.Split(topic, ".")[4:] {
			covered[token] = true
		}
	}
	for _, kind := range contracts.GithubTopicKinds {
		if !covered[kind] {
			t.Errorf("no row publishes under the topic kind %q", kind)
		}
	}
}

// githubFixture is the body of a GitHub delivery in packages/envoy/scripts/fixtures/github.
func githubFixture(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "scripts", "fixtures", "github", name+".json"))
	if err != nil {
		t.Fatalf("read the fixture %s: %v", name, err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode the fixture %s: %v", name, err)
	}
	return body
}

func githubRepository() map[string]any {
	return map[string]any{"name": "example-repo", "full_name": "example-org/example-repo", "owner": map[string]any{"login": "example-org"}}
}

// mergeGroupBody is the body GitHub sends when a merge queue's group asks for its checks.
func mergeGroupBody() map[string]any {
	return map[string]any{
		"action": "checks_requested",
		"merge_group": map[string]any{
			"head_sha": "2222222222222222222222222222222222222222",
			"head_ref": "refs/heads/gh-readonly-queue/main/pr-42-1111111111111111111111111111111111111111",
			"base_sha": "1111111111111111111111111111111111111111",
			"base_ref": "refs/heads/main",
		},
		"repository": githubRepository(),
		"sender":     map[string]any{"login": "github-merge-queue[bot]", "type": "Bot"},
	}
}

// withComment is body with its comment's text replaced by text.
func withComment(body map[string]any, text string) map[string]any {
	comment := maps.Clone(body["comment"].(map[string]any))
	comment["body"] = text
	body["comment"] = comment
	return body
}

// withInstallation is body as the App installed as installation id delivers it.
func withInstallation(t *testing.T, body map[string]any, id int) []byte {
	t.Helper()
	delivered := maps.Clone(body)
	delivered["installation"] = map[string]any{"id": id, "node_id": "MDIzOkludGVncmF0aW9uSW5zdGFsbGF0aW9u" + strconv.Itoa(id)}
	raw, err := json.Marshal(delivered)
	if err != nil {
		t.Fatalf("encode the delivery: %v", err)
	}
	return raw
}

// githubTopics is the topics the listener publishes body on, in its publish order.
func githubTopics(t *testing.T, event string, body []byte) []string {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode the delivery: %v", err)
	}
	return envelopeTopics(contracts.GithubEnvelopes(contracts.GithubEnvelopeInput{
		Event: event, Delivery: "topics", Body: decoded, EventID: "topics", TraceID: "topics",
	}, "@legion"))
}

func envelopeTopics(items []contracts.Envelope) []string {
	topics := make([]string, 0, len(items))
	for _, item := range items {
		topics = append(topics, item.Topic)
	}
	return topics
}

// githubStream is a bus client that owns a notification stream of this test's own.
func githubStream(t *testing.T) *bus.Client {
	t.Helper()
	client, err := bus.ConnectOwningStream([]string{testnats.URL(t)}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the bus: %v", err)
	}
	t.Cleanup(client.Close)
	return client
}

// postDelivery posts a signed GitHub delivery, with headers set beside GitHub's own three, and
// requires the 200 GitHub marks delivered: a copy the stream already holds is answered as one it
// stored.
func postDelivery(t *testing.T, handler http.Handler, event, guid string, body []byte, headers map[string]string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Delivery", guid)
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", githubSign("s", body))
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delivery %s of %s answered %d, want 200; body = %s", guid, event, rec.Code, rec.Body.String())
	}
}

// githubStreamEnvelopes is every GitHub envelope the notification stream holds, in stream order.
func githubStreamEnvelopes(t *testing.T, client *bus.Client) []contracts.Envelope {
	t.Helper()
	const subjects = "notifications.github.>"
	info, err := client.JS().StreamInfo(bus.Stream, &nats.StreamInfoRequest{SubjectsFilter: subjects})
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	var held uint64
	for _, count := range info.State.Subjects {
		held += count
	}
	items := make([]contracts.Envelope, 0, held)
	if held == 0 {
		return items
	}
	sub, err := client.JS().SubscribeSync(subjects, nats.BindStream(bus.Stream), nats.OrderedConsumer(), nats.DeliverAll())
	if err != nil {
		t.Fatalf("read %s: %v", subjects, err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	for range held {
		msg, err := sub.NextMsg(5 * time.Second)
		if err != nil {
			t.Fatalf("read message %d of %d: %v", len(items)+1, held, err)
		}
		var item contracts.Envelope
		if err := json.Unmarshal(msg.Data, &item); err != nil {
			t.Fatalf("decode the message on %s: %v", msg.Subject, err)
		}
		items = append(items, item)
	}
	return items
}
