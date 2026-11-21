package api

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestV2Retries is c-retry of dispatch://INFRA-4071: every /api/v2 write is keyed (R8), a repeat
// under one key answers the first answer marked a replay and writes nothing, and an equal PUT by
// the caller who set what it sets is answered 200 and writes nothing (decision 8,
// putChangesNothing), while a PUT that changes a field is a change. A case whose route /api/v2
// does not serve fails (requireMounted).
func TestV2Retries(t *testing.T) {
	tour := newRouteTour(t)
	alice, bob := tourPerson("alice"), tourPerson("bob")

	t.Run("an ask's answer", func(t *testing.T) {
		route := tour.requireMounted(t, http.MethodPut, "/asks/{id}/state")
		issue := tour.issue(t)
		ask := tour.ask(t, issue)
		answer := tourRequest{
			Path: map[string]string{"id": ask},
			Body: map[string]any{"state": "answered", "text": "Yes.", "expected_edited_at": nil}, As: alice,
		}
		key := uuid.NewString()
		first, again := tour.send(t, route, answer, key), tour.send(t, route, answer, key)
		if first.Code != http.StatusOK || again.Code != http.StatusOK || !replayed(again) || again.Body.String() != first.Body.String() {
			t.Fatalf("the answer twice under one key: %d then %d %s (replayed %v), want the first answer twice, the second a replay",
				first.Code, again.Code, again.Body.String(), replayed(again))
		}
		if answered := tour.eventCount(t, issue.Key, "ask.answered"); answered != 1 {
			t.Fatalf("the answer twice under one key wrote %d ask.answered events, want one", answered)
		}

		var at string
		if err := tour.database.Pool.QueryRow(t.Context(), `select answer->>'at' from asks where id = $1`, ask).Scan(&at); err != nil {
			t.Fatalf("read the answer's at: %v", err)
		}
		replacing := tour.send(t, route, tourRequest{
			Path: map[string]string{"id": ask},
			Body: map[string]any{"state": "answered", "text": "No, the other way.", "expected_edited_at": nil, "expected_answer_at": at},
			As:   alice,
		}, uuid.NewString())
		if replacing.Code != http.StatusOK {
			t.Fatalf("an answer that replaces the first: %d %s, want 200", replacing.Code, replacing.Body.String())
		}
		if answered := tour.eventCount(t, issue.Key, "ask.answered"); answered != 2 {
			t.Fatalf("the replacing answer left %d ask.answered events, want its own beside the first", answered)
		}

		second := tour.send(t, route, tourRequest{
			Path: map[string]string{"id": ask},
			Body: map[string]any{"state": "answered", "text": "Bob's view.", "expected_edited_at": nil}, As: bob,
		}, uuid.NewString())
		if code, _ := refusalOf(t, second); second.Code != http.StatusConflict || code != "ASK_ANSWERED" {
			t.Fatalf("a first answer from a second person: %d %s, want 409 ASK_ANSWERED", second.Code, second.Body.String())
		}
	})

	t.Run("a comment's resolve", func(t *testing.T) {
		route := tour.requireMounted(t, http.MethodPut, "/comments/{id}/state")
		issue := tour.issue(t)
		comment := tour.comment(t, issue)
		resolve := func(as tourCaller) {
			t.Helper()
			response := tour.send(t, route, tourRequest{
				Path: map[string]string{"id": comment}, Body: map[string]any{"state": "resolved"}, As: as,
			}, "")
			if response.Code != http.StatusOK {
				t.Fatalf("a resolve as %+v: %d %s, want 200", as, response.Code, response.Body.String())
			}
		}
		resolve(alice)
		events := tour.eventCount(t, issue.Key, "")
		resolve(alice)
		if after := tour.eventCount(t, issue.Key, ""); after != events {
			t.Fatalf("the resolve again by the caller who resolved it appended %d events, want none", after-events)
		}
		resolve(bob)
		if after := tour.eventCount(t, issue.Key, ""); after == events {
			t.Fatal("the resolve by another caller appended no event, want its own")
		}
	})

	t.Run("a grant's revoke", func(t *testing.T) {
		route := tour.requireMounted(t, http.MethodDelete, "/credential-grants/{id}")
		grant := uuid.NewString()
		key := uuid.NewString()
		revoke := tourRequest{Path: map[string]string{"id": grant}, As: alice}
		first, again := tour.send(t, route, revoke, key), tour.send(t, route, revoke, key)
		if first.Code/100 != 2 || !replayed(again) || again.Code != first.Code || again.Body.String() != first.Body.String() {
			t.Fatalf("the revoke twice under one key: %d %s then %d (replayed %v), want the first answer replayed",
				first.Code, first.Body.String(), again.Code, replayed(again))
		}
		if revokes := tour.secrets.callsTo(http.MethodPost, "/v1/grants/"+grant+"/revoke-by-approver"); revokes != 1 {
			t.Fatalf("the broker was asked to revoke %d times, want once", revokes)
		}
	})

	t.Run("an agent token's mint", func(t *testing.T) {
		route := tour.requireMounted(t, http.MethodPost, "/me/agent-tokens")
		count := func() int {
			var tokens int
			if err := tour.database.Pool.QueryRow(t.Context(), `select count(*) from agent_tokens where name = 'tour retry'`).Scan(&tokens); err != nil {
				t.Fatalf("count agent tokens: %v", err)
			}
			return tokens
		}
		mint := tourRequest{Body: map[string]any{"name": "tour retry", "scopes": []string{}}, As: alice}
		key := uuid.NewString()
		if first := tour.send(t, route, mint, key); first.Code != http.StatusCreated {
			t.Fatalf("the mint: %d %s, want 201", first.Code, first.Body.String())
		}
		again := tour.send(t, route, mint, key)
		if code, _ := refusalOf(t, again); again.Code != http.StatusConflict || code != "KEY_COMMITTED" {
			t.Fatalf("the mint again under its key: %d %s, want 409 KEY_COMMITTED", again.Code, again.Body.String())
		}
		if tokens := count(); tokens != 1 {
			t.Fatalf("the mint twice under one key stored %d tokens, want one", tokens)
		}
	})

	t.Run("a keyed upload above the JSON limit", func(t *testing.T) {
		route := tour.requireMounted(t, http.MethodPost, "/issues/{key}/artifacts")
		issue := tour.issue(t)
		content := bytes.Repeat([]byte("a file the tour uploads\n"), int(maxJSONRequestBytes)/24+1)
		upload := tour.send(t, route, tourRequest{
			Path: map[string]string{"key": issue.Key},
			Body: tourUpload{Fields: map[string]string{"name": "big.txt"}, Filename: "big.txt", ContentType: "text/plain", Content: content},
			As:   alice,
		}, uuid.NewString())
		if upload.Code != http.StatusCreated {
			t.Fatalf("a keyed upload of %d bytes: %d %s, want it stored", len(content), upload.Code, upload.Body.String())
		}
	})

	// The no-op PUT of a credential request: the broker's request already in that
	// state by that approver is answered 200 with the request and nothing is relayed, and the same
	// body from a caller who is not its approver is relayed, for the broker to judge.
	t.Run("a credential request's decision by its approver", func(t *testing.T) {
		route := tour.requireMounted(t, http.MethodPut, "/credential-requests/{id}/state")
		record := "retry-record-" + uuid.NewString()
		tour.secrets.answer(http.MethodPost, "/v1/machine-logins/lookup", http.StatusOK, map[string]any{
			"record_id": record, "kind": "launcher_credential", "state": "approved", "approver": "alice",
		})
		decide := tourRequest{Path: map[string]string{"id": record}, Body: map[string]any{"state": "approved", "code": "ABCD-1234"}, As: alice}
		approvals := func() int { return tour.secrets.callsTo(http.MethodPost, "/v1/credential-requests/"+record+"/approve") }
		if equal := tour.send(t, route, decide, ""); equal.Code != http.StatusOK || !strings.Contains(equal.Body.String(), record) {
			t.Fatalf("the approval of a request its approver approved: %d %s, want 200 with the request", equal.Code, equal.Body.String())
		}
		if relayed := approvals(); relayed != 0 {
			t.Fatalf("the approval its approver already made was relayed %d times, want never", relayed)
		}
		decide.As = bob
		if other := tour.send(t, route, decide, ""); other.Code != http.StatusOK {
			t.Fatalf("bob's approval of alice's request: %d %s, want the broker's answer relayed", other.Code, other.Body.String())
		}
		if relayed := approvals(); relayed != 1 {
			t.Fatalf("bob's approval was relayed %d times, want once: he is not the request's approver", relayed)
		}
	})

	// The equal PUTs whose objects record no setter, or whose setter is the
	// caller: each answered 200, the repeat writing no event.
	for _, put := range []struct {
		name, pattern string
		request       func(t *testing.T) (tourRequest, tourCaller)
	}{
		{"an issue's claim by its holder", "/issues/{key}/claim", func(t *testing.T) (tourRequest, tourCaller) {
			return tourRequest{Path: map[string]string{"key": tour.issue(t).Key}, Body: map[string]any{}, As: alice}, alice
		}},
		{"an ask's follower by any caller", "/asks/{id}/followers/{session_id}", func(t *testing.T) (tourRequest, tourCaller) {
			return tourRequest{Path: map[string]string{"id": tour.ask(t, tour.issue(t)), "session_id": tourSession}, As: alice}, bob
		}},
		{"a repository's mapping by any caller", "/repositories/{owner}/{repo}", func(t *testing.T) (tourRequest, tourCaller) {
			return tourRequest{
				Path: map[string]string{"owner": "tour", "repo": uuid.NewString()}, Body: map[string]any{"project": tour.project(t)}, As: alice,
			}, bob
		}},
		{"an architecture source by any caller", "/projects/{key}/architecture-sources/{owner}/{repo}", func(t *testing.T) (tourRequest, tourCaller) {
			owner, name, _ := strings.Cut(tour.architectureRepo(t), "/")
			return tourRequest{
				Path: map[string]string{"key": tour.project(t), "owner": owner, "repo": name}, Body: map[string]any{"branch": "main"}, As: alice,
			}, bob
		}},
		{"a suggestion's accept by the person who accepted it", "/comments/{id}/state", func(t *testing.T) (tourRequest, tourCaller) {
			return tourRequest{
				Path: map[string]string{"id": tour.suggestion(t, tour.issue(t), true)}, Body: map[string]any{"state": "accepted"}, As: alice,
			}, alice
		}},
		{"a delivery's accept by the session that accepted it", "/messages/{id}/deliveries/{attempt}/state", func(t *testing.T) (tourRequest, tourCaller) {
			recipient := tourBearer("agent-token")
			return tourRequest{
				Path: map[string]string{"id": tour.directMessage(t), "attempt": "1"},
				Body: map[string]any{"state": "accepted", "actor": map[string]any{"kind": "session", "id": tourRecipient}},
				As:   recipient,
			}, recipient
		}},
		{"a decision's keep by the person who kept it", "/issues/{key}/decisions/{block_id}/state", func(t *testing.T) (tourRequest, tourCaller) {
			issue := tour.issue(t)
			block, sum := tour.decision(t, issue)
			return tourRequest{
				Path: map[string]string{"key": issue.Key, "block_id": block}, Body: map[string]any{"state": "kept", "text_sha256": sum}, As: alice,
			}, alice
		}},
		{"a result's retraction by the caller who retracted it", "/results/{id}/state", func(t *testing.T) (tourRequest, tourCaller) {
			return tourRequest{
				Path: map[string]string{"id": tour.result(t, tour.issue(t))},
				Body: map[string]any{"state": "retracted", "reason": "Recorded at the wrong commit."}, As: alice,
			}, alice
		}},
	} {
		t.Run(put.name, func(t *testing.T) {
			route := tour.requireMounted(t, http.MethodPut, put.pattern)
			request, repeater := put.request(t)
			if first := tour.send(t, route, request, ""); first.Code != http.StatusOK {
				t.Fatalf("the PUT: %d %s, want 200", first.Code, first.Body.String())
			}
			events := tour.eventCount(t, "", "")
			request.As = repeater
			if again := tour.send(t, route, request, ""); again.Code != http.StatusOK {
				t.Fatalf("the equal PUT again: %d %s, want 200 with the object", again.Code, again.Body.String())
			}
			if after := tour.eventCount(t, "", ""); after != events {
				t.Fatalf("the equal PUT again appended %d events, want none", after-events)
			}
		})
	}
}
