package webhook

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/ghrename"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// publishedLine is the github published line of a push of sharedDelivery on topic, from the App
// targetID, with the stream's answer result to a publish that was not sent again. The delivery,
// the key and the hook target's id are logged quoted (%q).
func publishedLine(topic, targetID, result string) string {
	return fmt.Sprintf("github published delivery=%q event=push action= topic=%s key=%q target_id=%q result=%s resent=false",
		sharedDelivery, topic, "github."+sharedDelivery, targetID, result)
}

// A publish whose connection closes under it after its first send reached the stream is sent
// again on the connection the bus dials in its place, and the stream answers the resend as a
// duplicate of the copy that first send stored (bus.StoredAck.Resent). The github published line
// reads the stored acknowledgement, so it logs that copy as this delivery's own: result=stored,
// with resent=true beside it (a real NATS server, its replies held by a proxy).
func TestAResendWhoseFirstTryWasStoredLogsStored(t *testing.T) {
	logged := captureLog(t)
	_, uri := testnats.Start(t)
	direct, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(direct.Close)
	proxy := testnats.StartProxy(t, uri)
	reconnecting, err := bus.ConnectOwningStream([]string{proxy.URL()}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect through the reply gate: %v", err)
	}
	t.Cleanup(reconnecting.Close)
	const topic = "notifications.github.example-org.example-repo.push.branch.main"
	stored := func() uint64 {
		info, err := direct.JS().StreamInfo(bus.Stream, &nats.StreamInfoRequest{SubjectsFilter: topic})
		if err != nil {
			t.Fatalf("read what the stream holds on %s: %v", topic, err)
		}
		return info.State.Subjects[topic]
	}

	handler := GitHubHandler("s", "@legion", "", "", reconnecting, &mockRecorder{}, NewBodyBudget(DefaultBodyBudget, nil))
	body := withInstallation(t, githubFixture(t, "push"), 1001)
	before := reconnecting.Connection().Stats().OutMsgs
	proxy.FreezeServerReplies()
	answered := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Delivery", sharedDelivery)
		req.Header.Set("X-GitHub-Event", "push")
		req.Header.Set("X-Hub-Signature-256", githubSign("s", body))
		for name, value := range appDelivery("5341240") {
			req.Header.Set(name, value)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		answered <- rec.Code
	}()
	testwait.WithinPolling(t, 5*time.Second, time.Millisecond, "the publish to reach the stream before its answer",
		func() bool { return reconnecting.Connection().Stats().OutMsgs > before && stored() == 1 })
	if err := reconnecting.Connection().ForceReconnect(); err != nil {
		t.Fatalf("force reconnect: %v", err)
	}
	proxy.Restore()
	select {
	case code := <-answered:
		if code != http.StatusOK {
			t.Fatalf("the delivery answered %d, want 200", code)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the delivery whose publish was sent again did not answer")
	}
	want := []string{fmt.Sprintf("github published delivery=%q event=push action= topic=%s key=%q target_id=%q result=stored resent=true",
		sharedDelivery, topic, "github."+sharedDelivery, "5341240")}
	if got := linesStarting(logged.String(), "github published "); !slices.Equal(got, want) {
		t.Fatalf("the resent publish logged\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if n := stored(); n != 1 {
		t.Fatalf("the stream holds %d messages on %s, want the one copy of the publish sent twice", n, topic)
	}
}

// A repository GitHub renamed is published a second time under its former name, through the
// rename publisher the listener publishes every webhook through (ghrename.Publishing), and each of
// the two topics logs its own github published line with its own answer. Here the push was first
// delivered before the rename was configured, so the current name's copy is a duplicate and the
// former name's is stored (a real NATS server).
func TestGitHubPublishedLineNamesEachTopicTheRenamePublisherPublishes(t *testing.T) {
	logged := captureLog(t)
	client := githubStream(t)
	body := withInstallation(t, githubFixture(t, "push"), 1001)
	const (
		current = "notifications.github.example-org.example-repo.push.branch.main"
		former  = "notifications.github.example-org.former-repo.push.branch.main"
	)

	plain := GitHubHandler("s", "@legion", "", "", ghrename.Publishing(client, ghrename.Renames{}, logging.New("test")), &mockRecorder{}, NewBodyBudget(DefaultBodyBudget, nil))
	postDelivery(t, plain, "push", sharedDelivery, body, appDelivery("5341240"))
	if got, want := linesStarting(logged.String(), "github published "), []string{publishedLine(current, "5341240", "stored")}; !slices.Equal(got, want) {
		t.Fatalf("the delivery before the rename logged\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	renames, err := ghrename.Parse("example-org/former-repo=example-org/example-repo")
	if err != nil {
		t.Fatalf("parse the rename: %v", err)
	}
	renamed := GitHubHandler("s", "@legion", "", "", ghrename.Publishing(client, renames, logging.New("test")), &mockRecorder{}, NewBodyBudget(DefaultBodyBudget, nil))
	logged.Reset()
	postDelivery(t, renamed, "push", sharedDelivery, body, appDelivery("5341240"))
	want := []string{publishedLine(current, "5341240", "duplicate"), publishedLine(former, "5341240", "stored")}
	if got := linesStarting(logged.String(), "github published "); !slices.Equal(got, want) {
		t.Fatalf("the delivery after the rename logged\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if got := envelopeTopics(githubStreamEnvelopes(t, client)); !slices.Equal(got, []string{current, former}) {
		t.Fatalf("the stream holds %v, want one message on each name", got)
	}
}

// A delivery that carries no X-GitHub-Hook-Installation-Target-ID header (a repository webhook's,
// or one sent by hand) is published as any other, and its github published line names an empty
// target.
func TestAGitHubDeliveryWithNoHookTargetIsPublishedWithAnEmptyTarget(t *testing.T) {
	logged := captureLog(t)
	client := githubStream(t)
	handler := GitHubHandler("s", "@legion", "", "", client, &mockRecorder{}, NewBodyBudget(DefaultBodyBudget, nil))
	postDelivery(t, handler, "push", sharedDelivery, withInstallation(t, githubFixture(t, "push"), 1001), nil)
	const topic = "notifications.github.example-org.example-repo.push.branch.main"
	if got, want := linesStarting(logged.String(), "github published "), []string{publishedLine(topic, "", "stored")}; !slices.Equal(got, want) {
		t.Fatalf("the delivery logged\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if got := envelopeTopics(githubStreamEnvelopes(t, client)); !slices.Equal(got, []string{topic}) {
		t.Fatalf("the stream holds %v, want the push on %s", got, topic)
	}
}

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

// logFields splits a log line into its key=value fields as a reader of the listener's log does:
// at spaces outside a double-quoted value, a quoted value's backslash escaping the next byte.
func logFields(line string) []string {
	var fields []string
	var field strings.Builder
	quoted, escaped := false, false
	for _, r := range line {
		switch {
		case escaped:
			escaped = false
		case quoted && r == '\\':
			escaped = true
		case r == '"':
			quoted = !quoted
		case r == ' ' && !quoted:
			fields = append(fields, field.String())
			field.Reset()
			continue
		}
		field.WriteRune(r)
	}
	return append(fields, field.String())
}

// fieldCount is how many of fields have key.
func fieldCount(fields []string, key string) int {
	n := 0
	for _, field := range fields {
		if strings.HasPrefix(field, key+"=") {
			n++
		}
	}
	return n
}

// X-GitHub-Delivery and the two hook-target headers are not covered by the body's signature, so a
// signed body replayed with forged headers chooses their values. The github sender and github
// published lines log each of them quoted (%q), and the key built from the delivery too, so a
// forged value holding a space and an `=` adds no field of its own: a reader of each line finds one
// delivery= and one target_id=, each holding the whole forged value, and one result= on the
// published line alone. The github mention detected line quotes the delivery as well.
func TestAForgedHookTargetHeaderAddsNoFieldToTheGitHubLines(t *testing.T) {
	logged := captureLog(t)
	handler := GitHubHandler("s", "@legion", "", "", githubStream(t), &mockRecorder{}, NewBodyBudget(DefaultBodyBudget, nil))
	const (
		forgedDelivery = "d-1 result=stored target_id=5341305"
		forgedType     = "integration result=stored"
		forgedTarget   = "1 result=stored"
	)
	body := withInstallation(t, withComment(githubFixture(t, "issue-comment-pr-created"), "@legion please review"), 1001)
	postDelivery(t, handler, "issue_comment", forgedDelivery, body, map[string]string{
		"X-GitHub-Hook-Installation-Target-Type": forgedType,
		"X-GitHub-Hook-Installation-Target-ID":   forgedTarget,
	})
	published := linesStarting(logged.String(), "github published ")
	senders := linesStarting(logged.String(), "github sender ")
	mentions := linesStarting(logged.String(), "github mention detected ")
	if len(published) == 0 || len(senders) != 1 || len(mentions) != 1 {
		t.Fatalf("logged %d github published, %d github sender and %d github mention detected lines, want some, one and one:\n%s",
			len(published), len(senders), len(mentions), logged.String())
	}
	for _, line := range append(append(slices.Clone(published), senders...), mentions...) {
		fields := logFields(line)
		if n := fieldCount(fields, "delivery"); n != 1 || !slices.Contains(fields, "delivery="+strconv.Quote(forgedDelivery)) {
			t.Errorf("a reader of %q finds %d delivery= fields, want one holding %q whole", line, n, forgedDelivery)
		}
		if strings.HasPrefix(line, "github mention detected ") {
			continue
		}
		if n := fieldCount(fields, "target_id"); n != 1 || !slices.Contains(fields, "target_id="+strconv.Quote(forgedTarget)) {
			t.Errorf("a reader of %q finds %d target_id= fields, want one holding %q whole", line, n, forgedTarget)
		}
		// A published line writes one result= of its own, and a key built from the delivery; the
		// sender line writes neither.
		want := 0
		if strings.HasPrefix(line, "github published ") {
			want = 1
			if n := fieldCount(fields, "key"); n != 1 || !slices.Contains(fields, "key="+strconv.Quote("github."+forgedDelivery)) {
				t.Errorf("a reader of %q finds %d key= fields, want one holding the key whole", line, n)
			}
		}
		if n := fieldCount(fields, "result"); n != want {
			t.Errorf("a reader of %q finds %d result= fields, want %d", line, n, want)
		}
	}
	if fields := logFields(senders[0]); fieldCount(fields, "target_type") != 1 || !slices.Contains(fields, "target_type="+strconv.Quote(forgedType)) {
		t.Errorf("a reader of %q does not find one target_type= holding %q whole", senders[0], forgedType)
	}
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
		handler := GitHubHandler("s", "@legion", "", "", &mockPublisher{}, &mockRecorder{}, NewBodyBudget(DefaultBodyBudget, nil))
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
	if want := []string{`github sender login=octocat type=User target_type="integration" target_id="5341240" delivery="d-target" event=issue_comment`}; !slices.Equal(lines, want) {
		t.Errorf("lines = %q, want %q", lines, want)
	}
	lines = post("issue_comment", "d-no-target", comment, nil)
	if want := []string{`github sender login=octocat type=User target_type="" target_id="" delivery="d-no-target" event=issue_comment`}; !slices.Equal(lines, want) {
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
			handler := GitHubHandler("s", "@legion", "", "", githubStream(t), &mockRecorder{}, NewBodyBudget(DefaultBodyBudget, nil))
			first := withInstallation(t, row.body, 1001)
			topics := githubTopics(t, row.event, first)
			if len(topics) == 0 {
				t.Fatalf("the listener publishes nothing for this %s body; the row needs an event it publishes", row.event)
			}
			action, _ := row.body["action"].(string)
			want := func(targetID, result string) []string {
				var lines []string
				for _, topic := range topics {
					lines = append(lines, fmt.Sprintf("github published delivery=%q event=%s action=%s topic=%s key=%q target_id=%q result=%s resent=false",
						sharedDelivery, row.event, action, topic, "github."+sharedDelivery, targetID, result))
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
