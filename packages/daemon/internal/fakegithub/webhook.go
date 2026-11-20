package fakegithub

import (
	"bytes"
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The webhook a repository of the server delivers to, as GitHub delivers one: each push a git
// receive-pack takes and each pull request AddPullRequest opens is a POST of the event's JSON to
// the URL Webhooks names, with the X-GitHub-Event, X-GitHub-Delivery and X-Hub-Signature-256
// headers GitHub sends, the last an HMAC-SHA256 of the body under the hook's secret, which is how
// the Envoy listener's /webhook/github verifies a delivery (internal/webhook's GitHubHandler). A
// delivery is made after the request that caused it has been answered, in the order the events
// happened, and Deliveries says what each one was answered.

// webhook is where the server delivers its events, and with what secret. Its goroutine delivers
// what queue holds, in order, until the queue is closed (close).
type webhook struct {
	url, secret string
	client      *http.Client
	// mu orders a send against close: a send takes it to queue a delivery, close to close the
	// queue, so no send meets a closed channel.
	mu     sync.Mutex
	closed bool
	queue  chan delivery
}

// delivery is one event to deliver.
type delivery struct {
	// event is the X-GitHub-Event, and label what Deliveries names the delivery by: the event and
	// the account whose act it is (`push from <login>`).
	event, label string
	body         []byte
}

// Webhooks has every repository of the server deliver its push and pull_request events to url,
// signed with secret, until the test ends or a later call names another hook. A later call
// replaces this one: every event from then goes to the new hook, and this one delivers what it
// had already queued and stops. At the test's end the server stops every hook, cancelling a
// delivery still under way, and waits for each one's goroutine to return.
func (s *Server) Webhooks(url, secret string) {
	hook := &webhook{url: url, secret: secret, client: &http.Client{Timeout: 30 * time.Second}, queue: make(chan delivery, 256)}
	s.mu.Lock()
	replaced := s.hook
	s.hook = hook
	s.mu.Unlock()
	if replaced != nil {
		replaced.close()
	}
	s.hooks.Go(func() {
		for d := range hook.queue {
			s.record(hook.deliver(s.hooksContext, d))
		}
	})
}

// stopWebhooks ends every hook at the test's end: deliveries still under way or queued are
// cancelled, and it returns once each hook's goroutine has.
func (s *Server) stopWebhooks() {
	s.stopHooks()
	s.mu.Lock()
	hook := s.hook
	s.hook = nil
	s.mu.Unlock()
	if hook != nil {
		hook.close()
	}
	s.hooks.Wait()
}

// enqueue queues d unless the hook is closed, and says whether it did.
func (h *webhook) enqueue(d delivery) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	h.queue <- d
	return true
}

// close ends the hook's queue, once: its goroutine delivers what the queue holds and returns.
func (h *webhook) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.closed {
		h.closed = true
		close(h.queue)
	}
}

// Deliveries is one line per webhook delivery made so far, in order: the event and the account it
// is from (`push from <login>`), its delivery id and the status the receiver answered, or the
// error that kept it from answering.
func (s *Server) Deliveries() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.delivered...)
}

func (s *Server) record(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delivered = append(s.delivered, line)
}

// send queues event, the act of login, with payload for the hook, if there is one: the hook of the
// moment it queues, so an event a replaced hook closed against goes to the hook that replaced it.
func (s *Server) send(event, login string, payload map[string]any) {
	label := event + " from " + login
	body, err := json.Marshal(payload)
	if err != nil {
		s.record(label + " not delivered: " + err.Error())
		return
	}
	for {
		s.mu.Lock()
		hook := s.hook
		s.mu.Unlock()
		if hook == nil || hook.enqueue(delivery{event: event, label: label, body: body}) {
			return
		}
	}
}

// deliver posts one event, cancelled with ctx, and says what came of it.
func (h *webhook) deliver(ctx context.Context, d delivery) string {
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	deliveryID := hex.EncodeToString(id)
	mac := hmac.New(sha256.New, []byte(h.secret))
	mac.Write(d.body)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(d.body))
	if err != nil {
		return fmt.Sprintf("%s %s: %v", d.label, deliveryID, err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "GitHub-Hookshot/fake")
	request.Header.Set("X-GitHub-Event", d.event)
	request.Header.Set("X-GitHub-Delivery", deliveryID)
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	response, err := h.client.Do(request)
	if err != nil {
		return fmt.Sprintf("%s %s: %v", d.label, deliveryID, err)
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(response.Body, 512))
	return fmt.Sprintf("%s %s -> %d %s", d.label, deliveryID, response.StatusCode, strings.TrimSpace(string(answer)))
}

// repositoryPayload is the `repository` object of an event of repo.
func repositoryPayload(repo *repository) map[string]any {
	return map[string]any{
		"id": repo.id, "name": repo.Name, "full_name": repo.Slug(), "private": !repo.Public, "default_branch": "main",
		"html_url": "https://" + Host + "/" + repo.Slug(), "clone_url": repo.URL(),
		"owner": map[string]any{"login": repo.Owner, "name": repo.Owner},
	}
}

// refs is every ref of the bare repository and the commit it names.
func (s *Server) refs(bare string) map[string]string {
	out, err := s.gitOutput(bare, "", "for-each-ref", "--format=%(refname) %(objectname)")
	if err != nil {
		return nil
	}
	refs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if name, commit, ok := strings.Cut(line, " "); ok {
			refs[name] = commit
		}
	}
	return refs
}

// pushed delivers a push event for each ref a receive-pack moved, from before to after, by the
// account login.
func (s *Server) pushed(repo *repository, login string, before, after map[string]string) {
	const zero = "0000000000000000000000000000000000000000"
	for ref, now := range after {
		if before[ref] != now {
			s.pushEvent(repo, login, ref, cmp.Or(before[ref], zero), now)
		}
	}
	for ref, was := range before {
		if _, kept := after[ref]; !kept {
			s.pushEvent(repo, login, ref, was, zero)
		}
	}
}

func (s *Server) pushEvent(repo *repository, login, ref, before, after string) {
	message, _ := s.gitOutput(repo.Bare, "", "log", "-1", "--format=%B", after)
	sender := map[string]any{"login": login, "type": "Bot"}
	s.send("push", login, map[string]any{
		"ref": ref, "before": before, "after": after, "created": strings.Trim(before, "0") == "", "deleted": strings.Trim(after, "0") == "",
		"repository": repositoryPayload(repo), "pusher": map[string]any{"name": login}, "sender": sender,
		"head_commit": map[string]any{"id": after, "message": strings.TrimSpace(message)},
		"commits":     []any{map[string]any{"id": after, "message": strings.TrimSpace(message)}},
		"compare":     "https://" + Host + "/" + repo.Slug() + "/compare/" + before + "..." + after,
	})
}

// opened delivers the pull_request event of pr's opening on repo.
func (s *Server) opened(repo *repository, pr PullRequest, head string) {
	s.send("pull_request", GrantedLogin, map[string]any{
		"action": "opened", "number": pr.Number, "repository": repositoryPayload(repo),
		"sender": map[string]any{"login": GrantedLogin, "type": "User"},
		"pull_request": map[string]any{
			"number": pr.Number, "title": pr.Title, "body": pr.Body, "state": pr.State, "merged": pr.Merged, "draft": false,
			"html_url": "https://" + Host + "/" + repo.Slug() + "/pull/" + fmt.Sprint(pr.Number),
			"head":     map[string]any{"ref": pr.Head, "sha": head, "repo": repositoryPayload(repo)},
			"base":     map[string]any{"ref": pr.Base, "repo": repositoryPayload(repo)},
			"user":     map[string]any{"login": GrantedLogin, "type": "User"},
		},
	})
}
