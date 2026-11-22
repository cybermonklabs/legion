package webhook

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// captureLog sends the standard logger to a buffer with no date or time until the test ends, and
// answers the buffer.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logged bytes.Buffer
	previous, flags := log.Writer(), log.Flags()
	log.SetOutput(&logged)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(previous); log.SetFlags(flags) })
	return &logged
}

// linesStarting are the lines of logged that start with prefix, in order.
func linesStarting(logged string, prefix string) []string {
	var lines []string
	for line := range strings.SplitSeq(logged, "\n") {
		if strings.HasPrefix(line, prefix) {
			lines = append(lines, line)
		}
	}
	return lines
}

// The github sender line names the resource whose webhook sent the delivery, from GitHub's
// X-GitHub-Hook-Installation-Target-Type and X-GitHub-Hook-Installation-Target-ID headers: for an
// App's webhook, integration and the App's id, so a read of the listener's log tells which App's
// webhook delivers. A delivery without the headers names neither value, and the line keeps both
// keys. Only a comment or a review writes the line, so a push and a merge group write none.
func TestGitHubSenderLineNamesTheWebhookApp(t *testing.T) {
	logged := captureLog(t)
	post := func(event, delivery string, body []byte, headers map[string]string) []string {
		t.Helper()
		logged.Reset()
		handler := GitHubHandler("s", "@legion", "", "", &mockPublisher{}, &mockRecorder{})
		req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Delivery", delivery)
		req.Header.Set("X-GitHub-Event", event)
		req.Header.Set("X-Hub-Signature-256", githubSign("s", body))
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s delivery answered %d, want 200; body = %s", event, rec.Code, rec.Body.String())
		}
		return linesStarting(logged.String(), "github sender ")
	}
	comment := []byte(`{
		"action": "created",
		"issue": {"number": 42, "title": "test issue"},
		"comment": {"body": "just a comment", "user": {"login": "octocat"}},
		"sender": {"login": "octocat", "type": "User"},
		"repository": {"name": "widgets", "owner": {"login": "acme"}, "full_name": "acme/widgets"}
	}`)

	lines := post("issue_comment", "d-target", comment, appDelivery("5341240"))
	if want := []string{"github sender login=octocat type=User target_type=integration target_id=5341240 delivery=d-target event=issue_comment"}; !slices.Equal(lines, want) {
		t.Errorf("lines = %q, want %q", lines, want)
	}
	lines = post("issue_comment", "d-no-target", comment, nil)
	if want := []string{"github sender login=octocat type=User target_type= target_id= delivery=d-no-target event=issue_comment"}; !slices.Equal(lines, want) {
		t.Errorf("lines without the headers = %q, want %q: both target keys with no value", lines, want)
	}
	for _, other := range []struct {
		event string
		body  map[string]any
	}{
		{"push", githubFixture(t, "push")},
		{"merge_group", mergeGroupBody()},
	} {
		if lines := post(other.event, "d-"+other.event, withInstallation(t, other.body, 1001), appDelivery("5341240")); len(lines) != 0 {
			t.Errorf("a %s delivery logged %q, want no github sender line", other.event, lines)
		}
	}
}

// Each publish of a GitHub delivery logs one github published line for each topic it publishes
// on, naming the delivery's GUID, its event and action, the topic, the key, the hook target's id
// and the stream's answer. The second App's copy of one event, under the same GUID, is answered
// as a duplicate on every topic, which is what tells the two Apps' copies of one event apart in
// the listener's log (a real NATS server).
func TestGitHubPublishedLineNamesKeyTargetAndResult(t *testing.T) {
	for _, row := range []struct {
		name, event string
		body        map[string]any
	}{
		{"push", "push", githubFixture(t, "push")},
		{"pull_request", "pull_request", githubFixture(t, "pull-request-opened")},
		{"issue_comment that mentions the trigger", "issue_comment", withComment(githubFixture(t, "issue-comment-pr-created"), "@legion please review")},
		{"check_run of an Actions job", "check_run", githubFixture(t, "check-run-actions-job-without-pr")},
	} {
		t.Run(row.name, func(t *testing.T) {
			logged := captureLog(t)
			handler := GitHubHandler("s", "@legion", "", "", githubStream(t), &mockRecorder{})
			first := withInstallation(t, row.body, 1001)
			topics := githubTopics(t, row.event, first)
			if len(topics) == 0 {
				t.Fatalf("the listener publishes nothing for this %s body; the row needs an event it publishes", row.event)
			}
			action, _ := row.body["action"].(string)
			want := func(targetID, result string) []string {
				var lines []string
				for _, topic := range topics {
					lines = append(lines, fmt.Sprintf("github published delivery=%s event=%s action=%s topic=%s key=github.%s target_id=%s result=%s",
						sharedDelivery, row.event, action, topic, sharedDelivery, targetID, result))
				}
				return lines
			}

			postDelivery(t, handler, row.event, sharedDelivery, first, appDelivery("5341240"))
			if got := linesStarting(logged.String(), "github published "); !slices.Equal(got, want("5341240", "stored")) {
				t.Fatalf("the first App's delivery logged\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want("5341240", "stored"), "\n"))
			}
			logged.Reset()
			postDelivery(t, handler, row.event, sharedDelivery, withInstallation(t, row.body, 2002), appDelivery("5341305"))
			if got := linesStarting(logged.String(), "github published "); !slices.Equal(got, want("5341305", "duplicate")) {
				t.Fatalf("the second App's copy logged\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want("5341305", "duplicate"), "\n"))
			}
		})
	}
}
