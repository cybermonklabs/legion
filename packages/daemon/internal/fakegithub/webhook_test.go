package fakegithub_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fakegithub"
)

// webhookEvent is one delivery the receiver took: its headers and its decoded body.
type webhookEvent struct {
	event, delivery, signature string
	body                       []byte
	payload                    map[string]any
}

// A push a real git makes and a pull request AddPullRequest opens each reach the webhook as
// GitHub delivers them: a POST naming the event and a delivery id, whose X-Hub-Signature-256 is
// the HMAC-SHA256 of the body under the hook's secret, carrying the ref the push moved from the
// commit it held to the one pushed, and the pull request's number, head and base.
func TestAPushAndAnOpenedPullRequestReachTheWebhookSignedWithItsSecret(t *testing.T) {
	const secret = "fake-github-webhook-not-a-secret"
	var mu sync.Mutex
	var events []webhookEvent
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		event := webhookEvent{event: r.Header.Get("X-GitHub-Event"), delivery: r.Header.Get("X-GitHub-Delivery"),
			signature: r.Header.Get("X-Hub-Signature-256"), body: body}
		_ = json.Unmarshal(body, &event.payload)
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(receiver.Close)
	s := fakegithub.Start(t, fakegithub.Options{})
	repo := s.NewRepository(t, "acme", "widgets", false)
	s.Grant("ghs_writer", true)
	s.Webhooks(receiver.URL, secret)
	s.Route(t)
	env := gitEnv(t, s)
	clone := filepath.Join(t.TempDir(), "widgets")
	if out, err := git(t.TempDir(), env, append(presenting("ghs_writer"), "clone", "--quiet", repo.URL(), clone)...); err != nil {
		t.Fatalf("git clone: %v: %s", err, out)
	}
	before := head(t, repo.Bare, "refs/heads/main")
	commitIn(t, clone, env, "pushed.txt")
	if out, err := git(clone, env, append(presenting("ghs_writer"), "push", "--quiet", "origin", "HEAD:refs/heads/legion/acme-1")...); err != nil {
		t.Fatalf("git push: %v: %s", err, out)
	}
	pushed := head(t, repo.Bare, "refs/heads/legion/acme-1")
	s.AddPullRequest(t, repo.Slug(), fakegithub.PullRequest{Number: 7, Title: "acme-1", Head: "legion/acme-1", Base: "main"})

	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		got := len(events)
		mu.Unlock()
		if got >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 {
		t.Fatalf("the webhook took %d deliveries, want the push and the pull request's opening; the server says %q", len(events), s.Deliveries())
	}
	for _, event := range events {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(event.body)
		if want := "sha256=" + hex.EncodeToString(mac.Sum(nil)); event.signature != want || event.delivery == "" {
			t.Errorf("the %s delivery %q is signed %q, want %q", event.event, event.delivery, event.signature, want)
		}
	}
	push, opened := events[0], events[1]
	if push.event != "push" || push.payload["ref"] != "refs/heads/legion/acme-1" || push.payload["after"] != pushed ||
		push.payload["before"] != "0000000000000000000000000000000000000000" {
		t.Errorf("the first delivery is %s %v, want the push of refs/heads/legion/acme-1 to %s (main was %s)", push.event, push.payload, pushed, before)
	}
	if repository, _ := push.payload["repository"].(map[string]any); repository["full_name"] != repo.Slug() {
		t.Errorf("the push names repository %v, want %s", push.payload["repository"], repo.Slug())
	}
	pr, _ := opened.payload["pull_request"].(map[string]any)
	headRef, _ := pr["head"].(map[string]any)
	if opened.event != "pull_request" || opened.payload["action"] != "opened" || pr["number"] != float64(7) ||
		headRef["ref"] != "legion/acme-1" || headRef["sha"] != pushed {
		t.Errorf("the second delivery is %s %v, want pull request 7 opened at %s", opened.event, opened.payload, pushed)
	}
}

// A second Webhooks call replaces the first: the pull request opened before it reaches the first
// hook alone, and the one opened after it the second hook alone.
func TestASecondWebhooksCallReplacesTheFirstHook(t *testing.T) {
	var mu sync.Mutex
	took := map[string][]float64{}
	receiver := func(name string) *httptest.Server {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var payload struct {
				PullRequest struct {
					Number float64 `json:"number"`
				} `json:"pull_request"`
			}
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &payload)
			mu.Lock()
			took[name] = append(took[name], payload.PullRequest.Number)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(server.Close)
		return server
	}
	first, second := receiver("first"), receiver("second")
	s := fakegithub.Start(t, fakegithub.Options{})
	repo := s.NewRepository(t, "acme", "widgets", false)
	s.Webhooks(first.URL, "first-not-a-secret")
	s.AddPullRequest(t, repo.Slug(), fakegithub.PullRequest{Number: 1, Title: "one", Head: "one", Base: "main"})
	s.Webhooks(second.URL, "second-not-a-secret")
	s.AddPullRequest(t, repo.Slug(), fakegithub.PullRequest{Number: 2, Title: "two", Head: "two", Base: "main"})

	deadline := time.Now().Add(10 * time.Second)
	for len(s.Deliveries()) < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(took["first"]) != 1 || took["first"][0] != 1 || len(took["second"]) != 1 || took["second"][0] != 2 {
		t.Fatalf("the first hook took pull requests %v and the second %v, want [1] and [2]; the server says %q",
			took["first"], took["second"], s.Deliveries())
	}
}
