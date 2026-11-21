//go:build multiprocess

package main

// agent_loop_test.go holds the agent's loop as a deployment meets it: a real envoy-dispatch
// server on a real database, whose own envoy-dispatch agent calls a fake chat model over https
// (going/agent/agenttest), trusting its certificate through SSL_CERT_FILE, with the agent's test
// hooks on (going/agent/hooks.go) so a reply of the fake can hang a tool or a stream, or panic.
// Its rows run two processes, the server and its agent, and wait out real leases and holds, so
// they carry the multiprocess tag and run in CI's dispatch-multiprocess job; the harness they
// share with the other builds is agent_harness_test.go.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/going/agent"
	"github.com/sjawhar/envoy/going/agent/agenttest"
	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// TestAStaleOwnerWritesNothing: a run has a lease, and an owner whose lease ended writes nothing
// (check c17). The fake model holds one call for 3 times the lease while the agent's renewal goes
// on beside it, so the run is still the agent's; once another owner holds the run, the server
// refuses the renewal, and the loop's next call and step are refused, so nothing of the stale
// owner is written.
func TestAStaleOwnerWritesNothing(t *testing.T) {
	server := startModelServer(t, nil)
	server.waitForAgentRenewal(t)
	const words = "search while the model thinks"
	release := make(chan struct{})
	server.script.set(words, func(n int) agenttest.Reply {
		if n == 1 {
			reply := readCall("dispatch_search", map[string]any{"query": "thinking"})
			reply.Hold = release
			return reply
		}
		return answer("Never reached.")
	})
	run := server.begin(t, agentRun{login: "alice", words: words})[0]
	server.eventually(t, 30*time.Second, "the run's model call", func() bool { return len(server.fake.Asking(words)) == 1 })
	lease := func() (token string, ends time.Time) {
		t.Helper()
		if err := server.database.Pool.QueryRow(context.Background(), `
			select coalesce(lease_token::text, ''), coalesce(lease_ends_at, 'epoch') from going_runs where id = $1::uuid
		`, run).Scan(&token, &ends); err != nil {
			t.Fatal(err)
		}
		return token, ends
	}
	held, _ := lease()
	time.Sleep(3 * 30 * time.Second)
	token, ends := lease()
	if row := server.run(t, run); row.state != "working" || token != held || !ends.After(time.Now()) {
		t.Fatalf("after 3 lease terms of one model call the run is %s under lease %s ending %s, want working under its claim's lease %s, renewed beside the call",
			row.state, token, ends, held)
	}
	server.exec(t, `
		update going_runs set lease_token = gen_random_uuid(), lease_owner = 'an agent of another server', lease_ends_at = now() + interval '1 hour'
		where id = $1::uuid
	`, run)
	close(release)
	time.Sleep(15 * time.Second)
	if steps := server.steps(t, run); steps != 0 {
		t.Errorf("the stale owner saved %d steps of the run, want none", steps)
	}
	if calls := len(server.fake.Asking(words)); calls != 1 {
		t.Errorf("the stale owner made %d model calls, want none after the one it held", calls)
	}
	if row := server.run(t, run); row.state != "working" || row.owner != "an agent of another server" {
		t.Errorf("the run is %s held by %q, want working and its new owner's", row.state, row.owner)
	}
}

// TestStopCancelsAStreamInFlight: Stop reaches a run in the agent within 1 s (check c27). The fake
// model streams without end; Stop sets the run stopped, the agent's waiting renew is answered at
// once, and the fake sees its stream cancelled within 1 s; nothing more of the run is written.
func TestStopCancelsAStreamInFlight(t *testing.T) {
	server := startModelServer(t, nil)
	server.waitForAgentRenewal(t)
	const words = "say everything you know"
	server.script.set(words, func(int) agenttest.Reply { return agenttest.Reply{Endless: true} })
	run := server.begin(t, agentRun{login: "alice", words: words})[0]
	server.eventually(t, 30*time.Second, "the endless stream", func() bool { return len(server.fake.Asking(words)) == 1 })
	time.Sleep(2 * time.Second)
	stream := server.fake.Asking(words)[0]
	if _, left := stream.Left(); left {
		t.Fatal("the stream ended before the stop")
	}
	stopped := time.Now()
	server.stop(t, run)
	server.eventually(t, 15*time.Second, "the stream to be cancelled", func() bool { _, left := stream.Left(); return left })
	if left, _ := stream.Left(); left.Sub(stopped) > time.Second {
		t.Errorf("the stream was cancelled %s after the stop, want within 1 s", left.Sub(stopped))
	}
	time.Sleep(3 * time.Second)
	if row := server.run(t, run); row.state != "stopped" || server.steps(t, run) != 0 || len(server.fake.Asking(words)) != 1 {
		t.Errorf("after the stop the run is %s with %d steps and %d model calls, want stopped with nothing more written",
			row.state, server.steps(t, run), len(server.fake.Asking(words)))
	}
}

// TestTheGrantIsCheckedAtEachCall: each call of a run is checked as the person's own request would
// be (check c30). In a deployment whose people sign in by cookie: a call after the person signs out
// is refused and the run stops with SESSION_ENDED, though the call before it ran; a sign-in past
// its 30 days stops the run with SIGN_IN_EXPIRED; an approval under a token without the approve
// scope is refused, and nothing of the plan runs; a membership the sign-in pool ended stops it with
// MEMBERSHIP_ENDED; a membership read the pool answers 503 waits, and the call runs once the pool
// answers; an approval under token B moves the grant to it, so revoking token A, which started
// the run, stops nothing; a run started by a sign-in or a personal token stores a grant that names
// the person and holds no cookie, token or hash of one; and a broker-issued token, the shared token
// and a service-account token are refused the start with RUN_NEEDS_PERSON_CREDENTIAL.
func TestTheGrantIsCheckedAtEachCall(t *testing.T) {
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), cookieClientID, cookieClientSecret)
	server := startModelServer(t, map[string]string{
		"DISPATCH_IDENTITY":             "cookie",
		"DISPATCH_SIGNIN_ISSUER":        issuer.URL(),
		"DISPATCH_SIGNIN_CLIENT_ID":     cookieClientID,
		"DISPATCH_SIGNIN_CLIENT_SECRET": cookieClientSecret,
		"DISPATCH_SIGNIN_GROUP":         cookieGroup,
		"DISPATCH_INSECURE_COOKIE":      "1",
	})
	// signIn signs the person in and answers the cookie and a grant of that sign-in, which
	// expires at expires.
	signIn := func(t *testing.T, expires time.Time) (*http.Cookie, string, agentGrant) {
		t.Helper()
		session := signInThrough(t, server.dispatchProcess, issuer)
		var login string
		grant := agentGrant{kind: "sign_in", expires: expires}
		if err := server.database.Pool.QueryRow(context.Background(), `select login, generation from user_sessions`).Scan(&login, &grant.generation); err != nil {
			t.Fatalf("read the sign-in's generation: %v", err)
		}
		return session, login, grant
	}
	whoami := readCall("dispatch_whoami", map[string]any{})
	lapse := func(t *testing.T) {
		t.Helper()
		server.exec(t, `update people set confirmed_at = now() - interval '2 hours' where email = $1`, cookiePerson)
	}
	month := time.Now().Add(30 * 24 * time.Hour)

	t.Run("a sign-out refuses the next call, and the run stops with that reason", func(t *testing.T) {
		const words = "who am I, twice"
		session, login, grant := signIn(t, month)
		signedOut := make(chan struct{})
		server.script.set(words, func(n int) agenttest.Reply {
			switch n {
			case 1:
				return whoami
			case 2:
				reply := whoami
				reply.Hold = signedOut
				return reply
			}
			return answer("Twice.")
		})
		run := server.begin(t, agentRun{login: login, words: words, grant: grant})[0]
		server.eventually(t, 30*time.Second, "the run's second model call", func() bool { return len(server.fake.Asking(words)) == 2 })
		request, err := http.NewRequest(http.MethodPost, server.url("/auth/logout"), nil)
		if err != nil {
			t.Fatal(err)
		}
		request.AddCookie(session)
		request.Header.Set("Sec-Fetch-Site", "same-origin")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode/100 != 2 {
			t.Fatalf("POST /auth/logout answered %d", response.StatusCode)
		}
		close(signedOut)
		row := server.ended(t, run, time.Minute)
		if row.state != "stopped" || !strings.HasPrefix(row.reason, "SESSION_ENDED") {
			t.Errorf("the run ended %s with %q, want stopped with SESSION_ENDED", row.state, row.reason)
		}
		if results := server.toolResults(t, run); len(results) != 1 || !strings.Contains(results[0], "status 200") {
			t.Errorf("the run's calls were told %q, want the one before the sign-out answered 200 and none after it", results)
		}
	})

	t.Run("a sign-in past its 30 days refuses the next call, and the run stops with that reason", func(t *testing.T) {
		const words = "who am I, after a month"
		_, login, grant := signIn(t, time.Now().Add(-time.Minute))
		server.script.set(words, func(n int) agenttest.Reply {
			if n == 1 {
				return whoami
			}
			return answer("Nobody.")
		})
		run := server.begin(t, agentRun{login: login, words: words, grant: grant})[0]
		if row := server.ended(t, run, time.Minute); row.state != "stopped" || !strings.HasPrefix(row.reason, "SIGN_IN_EXPIRED") {
			t.Errorf("the run ended %s with %q, want stopped with SIGN_IN_EXPIRED", row.state, row.reason)
		}
	})

	// asPerson sends body to path as the signed-in person, by their cookie, or by bearer where it
	// is set.
	asPerson := func(t *testing.T, session *http.Cookie, bearer, method, path, body string) httpAnswer {
		t.Helper()
		header := http.Header{"Sec-Fetch-Site": {"same-origin"}, "Cookie": {session.Name + "=" + session.Value}}
		if bearer != "" {
			header = http.Header{"Authorization": {"Bearer " + bearer}}
		}
		return server.send(t, method, path, body, header)
	}
	// token makes the person a personal agent token holding scopes, and answers it and its id.
	token := func(t *testing.T, session *http.Cookie, scopes ...string) (string, string) {
		t.Helper()
		encoded, err := json.Marshal(map[string]any{"name": "a token of " + strings.Join(scopes, " and "), "scopes": scopes})
		if err != nil {
			t.Fatal(err)
		}
		made := asPerson(t, session, "", http.MethodPost, "/api/v1/me/agent-tokens", string(encoded))
		var answer struct {
			ID    string `json:"id"`
			Token string `json:"token"`
		}
		if made.status != http.StatusCreated || json.Unmarshal(made.body, &answer) != nil || answer.Token == "" {
			t.Fatalf("POST /api/v1/me/agent-tokens answered %d %s", made.status, made.body)
		}
		return answer.Token, answer.ID
	}
	server.exec(t, `insert into projects (key, name, default_assignee) values ('GRANT', 'Grants', $1)`, cookiePerson)
	// planned scripts words to propose a comment on a new issue of the person's, and answers the
	// issue.
	planned := func(t *testing.T, session *http.Cookie, words string) string {
		t.Helper()
		made := asPerson(t, session, "", http.MethodPost, "/api/v1/issues", fmt.Sprintf(`{"project":"GRANT","title":%q,"force":true}`, words))
		var issue struct {
			Key string `json:"key"`
		}
		if made.status != http.StatusCreated || json.Unmarshal(made.body, &issue) != nil {
			t.Fatalf("POST /api/v1/issues answered %d %s", made.status, made.body)
		}
		server.script.set(words, func(n int) agenttest.Reply {
			if n == 1 {
				return toolCall("propose_plan", map[string]any{"summary": "A comment.", "changes": []map[string]any{
					{"tool": "dispatch_comment", "arguments": map[string]any{"issue": issue.Key, "body": "Planned on " + issue.Key + "."}},
				}})
			}
			return answer("Commented.")
		})
		return issue.Key
	}
	commented := func(t *testing.T, issue string) int {
		t.Helper()
		var count int
		if err := server.database.Pool.QueryRow(context.Background(), `select count(*) from comments where issue_key = $1`, issue).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	approve := func(run string) string { return "/api/v1/going/runs/" + run + "/plan/approve" }

	t.Run("a token without the approve scope is refused at the approval, and nothing of the plan runs", func(t *testing.T) {
		const words = "comment, under a token that cannot approve"
		session, login, _ := signIn(t, month)
		plaintext, id := token(t, session, "person")
		issue := planned(t, session, words)
		run := server.begin(t, agentRun{login: login, words: words, grant: agentGrant{kind: "token", token: id}})[0]
		if row := server.ended(t, run, time.Minute); row.state != "waiting" {
			t.Fatalf("the run ended %s with %q, want its plan waiting", row.state, row.reason)
		}
		if refused := asPerson(t, session, plaintext, http.MethodPost, approve(run), `{"plan":1}`); refused.status != http.StatusForbidden ||
			!strings.Contains(string(refused.body), "approve") {
			t.Fatalf("the approval under the token answered %d %s, want 403 naming the approve scope", refused.status, refused.body)
		}
		time.Sleep(2 * time.Second)
		if row := server.run(t, run); row.state != "waiting" || commented(t, issue) != 0 {
			t.Fatalf("after the refused approval the run is %s and %s holds %d comments, want it waiting with nothing run", row.state, issue, commented(t, issue))
		}
	})

	t.Run("a lost membership stops the run", func(t *testing.T) {
		const words = "who am I, once the pool forgets me"
		_, login, grant := signIn(t, month)
		lost := make(chan struct{})
		server.script.set(words, func(n int) agenttest.Reply {
			if n == 1 {
				reply := whoami
				reply.Hold = lost
				return reply
			}
			return answer("A member.")
		})
		run := server.begin(t, agentRun{login: login, words: words, grant: grant})[0]
		server.eventually(t, 30*time.Second, "the run's model call", func() bool { return len(server.fake.Asking(words)) == 1 })
		lapse(t)
		issuer.RevokeGrants()
		close(lost)
		if row := server.ended(t, run, time.Minute); row.state != "stopped" || !strings.HasPrefix(row.reason, "MEMBERSHIP_ENDED") {
			t.Errorf("the run ended %s with %q, want stopped with MEMBERSHIP_ENDED", row.state, row.reason)
		}
	})

	t.Run("a membership read answered 503 waits and then runs", func(t *testing.T) {
		const words = "who am I, while the pool is down"
		_, login, grant := signIn(t, month)
		lapse(t)
		issuer.FailToken(http.StatusServiceUnavailable, "")
		server.script.set(words, func(n int) agenttest.Reply {
			if n == 1 {
				return whoami
			}
			return answer("Still you.")
		})
		run := server.begin(t, agentRun{login: login, words: words, grant: grant})[0]
		server.eventually(t, 30*time.Second, "the run's model call", func() bool { return len(server.fake.Asking(words)) == 1 })
		time.Sleep(5 * time.Second)
		if row := server.run(t, run); row.state != "working" || len(server.fake.Asking(words)) != 1 {
			t.Fatalf("while the pool answers 503 the run is %s after %d model calls, want working, its call waiting", row.state, len(server.fake.Asking(words)))
		}
		issuer.FailToken(0, "")
		if row := server.ended(t, run, time.Minute); row.state != "done" {
			t.Fatalf("the run ended %s with %q, want done once the pool answers", row.state, row.reason)
		}
		if results := server.toolResults(t, run); len(results) != 1 || !strings.Contains(results[0], "status 200") || !strings.Contains(results[0], login) {
			t.Errorf("the call was told %q, want 200 naming %s", results, login)
		}
	})

	t.Run("the stored grant holds no cookie, assertion, token or hash", func(t *testing.T) {
		session, login, _ := signIn(t, month)
		plaintext, tokenID := token(t, session, "person")
		start := func(t *testing.T, bearer string) string {
			t.Helper()
			started := asPerson(t, session, bearer, http.MethodPost, "/api/v1/going/runs", `{"words":"a run whose grant is read"}`)
			var run struct {
				ID string `json:"id"`
			}
			if started.status != http.StatusCreated || json.Unmarshal(started.body, &run) != nil || run.ID == "" {
				t.Fatalf("POST /api/v1/going/runs answered %s", started)
			}
			return run.ID
		}
		byCookie, byToken := start(t, ""), start(t, plaintext)
		var generation int64
		if err := server.database.Pool.QueryRow(context.Background(), `select generation from user_sessions where login = $1`, login).Scan(&generation); err != nil {
			t.Fatalf("read the sign-in's generation: %v", err)
		}
		hash := sha256.Sum256([]byte(plaintext))
		parts := strings.Split(session.Value, ".")
		secrets := []string{session.Value, parts[len(parts)-1], plaintext, strings.TrimPrefix(plaintext, "dsp_"), hex.EncodeToString(hash[:])}
		type stored struct {
			kind, login, token, row string
			generation              *int64
			expires                 *time.Time
		}
		read := func(t *testing.T, run string) stored {
			t.Helper()
			var got stored
			var token *string
			if err := server.database.Pool.QueryRow(context.Background(), `
				select kind, login, generation, expires_at, token_id::text, row_to_json(g)::text from going_run_grants g where run_id = $1::uuid
			`, run).Scan(&got.kind, &got.login, &got.generation, &got.expires, &token, &got.row); err != nil {
				t.Fatalf("read the grant of %s: %v", run, err)
			}
			if token != nil {
				got.token = *token
			}
			for _, secret := range secrets {
				if strings.Contains(got.row, secret) {
					t.Errorf("the grant %s holds a credential the request presented: %q", got.row, secret)
				}
			}
			return got
		}
		if got := read(t, byCookie); got.kind != "sign_in" || got.login != login || got.generation == nil || *got.generation != generation ||
			got.expires == nil || got.expires.Before(time.Now().Add(29*24*time.Hour)) || got.expires.After(time.Now().Add(31*24*time.Hour)) || got.token != "" {
			t.Errorf("the sign-in's grant holds %s, want sign_in naming %s, the generation %d and the cookie's expiry 30 days ahead", got.row, login, generation)
		}
		if got := read(t, byToken); got.kind != "token" || got.login != login || got.token != tokenID || got.generation != nil || got.expires != nil {
			t.Errorf("the token's grant holds %s, want token naming %s and the token's row %s", got.row, login, tokenID)
		}
	})
	t.Run("an approval by token B moves the grant, and revoking token A stops nothing", func(t *testing.T) {
		const words = "comment, approved by another token"
		session, login, _ := signIn(t, month)
		_, first := token(t, session, "person", "approve")
		second, secondID := token(t, session, "person", "approve")
		issue := planned(t, session, words)
		run := server.begin(t, agentRun{login: login, words: words, grant: agentGrant{kind: "token", token: first}})[0]
		if row := server.ended(t, run, time.Minute); row.state != "waiting" {
			t.Fatalf("the run ended %s with %q, want its plan waiting", row.state, row.reason)
		}
		// The approved run waits at a shape no build reads until token A is revoked.
		server.exec(t, `update going_runs set shape = $2 where id = $1::uuid`, run, unreadShape)
		if approved := asPerson(t, session, second, http.MethodPost, approve(run), `{"plan":1}`); approved.status != http.StatusOK {
			t.Fatalf("the approval under token B answered %d %s", approved.status, approved.body)
		}
		var granted string
		if err := server.database.Pool.QueryRow(context.Background(), `select coalesce(token_id::text, '') from going_run_grants where run_id = $1::uuid`, run).Scan(&granted); err != nil || granted != secondID {
			t.Fatalf("the run's grant names token %q (%v), want token B's %s", granted, err, secondID)
		}
		if revoked := asPerson(t, session, "", http.MethodDelete, "/api/v1/me/agent-tokens/"+first, ""); revoked.status/100 != 2 {
			t.Fatalf("revoking token A answered %d %s", revoked.status, revoked.body)
		}
		server.exec(t, `update going_runs set shape = $2 where id = $1::uuid`, run, readShape)
		server.exec(t, `select pg_notify($1, 'ready')`, api.GoingRunsChannel)
		if row := server.ended(t, run, time.Minute); row.state != "done" || commented(t, issue) != 1 {
			t.Fatalf("the run ended %s with %q and %s holds %d comments, want done with the approved comment", row.state, row.reason, issue, commented(t, issue))
		}
	})
	t.Run("a broker token, the shared token and a service token are refused at the start with RUN_NEEDS_PERSON_CREDENTIAL", func(t *testing.T) {
		// A deployment that admits all three: a broker, whose proofs name the deployment's own
		// origin, and a service-account token issuer.
		const origin = "http://dispatch.test"
		broker := ciphertokentest.NewStubBroker(t)
		accountToken := filepath.Join(t.TempDir(), "account-token")
		if err := os.WriteFile(accountToken, []byte("an-account-token-of-dispatch"), 0o600); err != nil {
			t.Fatal(err)
		}
		services := oidctest.New(t)
		serviceKey := services.PublishKey(t, "service-token-key")
		other := startAgentServer(t, map[string]string{
			"DISPATCH_SERVER_URL":                       origin,
			"DISPATCH_AGENT_SECRETS_URL":                broker.Server.URL,
			"DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE": accountToken,
			"DISPATCH_OIDC_ISSUER":                      services.URL(),
			"DISPATCH_OIDC_AUDIENCE":                    "dispatch",
		})
		other.send(t, http.MethodGet, "/api/v1/me/going/settings", "", asUser("alice"))
		caller := ciphertokentest.NewCaller(t)
		const brokerToken = "a-person-scoped-broker-token"
		broker.Says(brokerToken, ciphertoken.Introspection{
			Active: true, Session: "ses_b1c2d3e4f5a6", Person: "alice", ProjectScope: ciphertoken.ProjectScopePerson,
			Scope: "dispatch.write", ExpiresAt: time.Now().Add(time.Hour).Unix(), ClientID: ciphertoken.ExchangeClientID, Thumbprint: caller.JKT(),
		})
		for _, refused := range []struct {
			name   string
			header func() http.Header
		}{
			{"a broker-issued token", func() http.Header {
				signed := caller.Signed(t, http.MethodPost, origin+"/api/v1/going/runs", brokerToken)
				return http.Header{"Authorization": {signed.Header.Get("Authorization")}, "DPoP": {signed.Header.Get("DPoP")}}
			}},
			{"the shared token", func() http.Header { return http.Header{"Authorization": {"Bearer " + dispatchTestAgentToken}} }},
			{"a service-account token", func() http.Header {
				minted := services.Mint(t, serviceKey, services.Claims("system:serviceaccount:legion:legion-worker", "dispatch"))
				return http.Header{"Authorization": {"Bearer " + minted}}
			}},
		} {
			t.Run(refused.name, func(t *testing.T) {
				answer := other.send(t, http.MethodPost, "/api/v1/going/runs", `{"words":"start a run"}`, refused.header())
				if answer.status != http.StatusForbidden || answer.code() != "RUN_NEEDS_PERSON_CREDENTIAL" {
					t.Errorf("the start under %s answered %s, want 403 RUN_NEEDS_PERSON_CREDENTIAL", refused.name, answer)
				}
			})
		}
		var runs int
		if err := other.database.Pool.QueryRow(context.Background(), `select count(*) from going_runs`).Scan(&runs); err != nil || runs != 0 {
			t.Errorf("the refused starts left %d runs (%v), want none", runs, err)
		}
	})
}

// TestAKilledAgentMakesEachChangeOnce (check c18): an approved plan of 7 changes, each a comment on
// an issue of its own, runs to its end though its agent process is killed at three points - with
// the plan's rows claimed and before their first call, while a call is held before its route, and
// while a call is held after its route committed and before the call heard the answer - and its
// server is killed while a call is held after its route committed. Each time the run goes on from
// its last saved step, and the store holds each of the 7 comments once. The first kill lands while
// the agent works a second run beside the plan's, so that death is charged to neither (the bound
// of three deaths a run causes alone is requireARunThatKillsTheAgentThreeTimesFails').
func TestAKilledAgentMakesEachChangeOnce(t *testing.T) {
	const project = "KILL"
	comments := func(n int) string { return fmt.Sprintf("/api/v1/issues/%s-%d/comments", project, n) }
	hooks := fmt.Sprintf("hold 30s POST %s; after 30s POST %s; after 50s POST %s", comments(3), comments(5), comments(7))
	server := startModelServer(t, map[string]string{"DISPATCH_TEST_AGENT_CALL_HOOK": hooks})
	first := server.agentStarted(t, 1, 30*time.Second)
	server.createProject(t, project, "Kills")
	changes := make([]map[string]any, 0, 7)
	issues := make([]string, 0, 7)
	for n := 1; n <= 7; n++ {
		key, _ := server.createIssueIn(t, project, fmt.Sprintf("Issue %d", n), "Context.\n")
		issues = append(issues, key)
		changes = append(changes, map[string]any{"tool": "dispatch_comment", "arguments": map[string]any{"issue": key, "body": "Change " + key + "."}})
	}
	const words, beside = "comment on the seven issues", "a run beside the plan's"
	server.script.set(words, func(n int) agenttest.Reply {
		if n == 1 {
			return toolCall("propose_plan", map[string]any{"summary": "A comment on each of the seven issues.", "changes": changes})
		}
		return answer("Commented on all seven.")
	})
	held := make(chan struct{})
	t.Cleanup(func() { close(held) })
	server.script.set(beside, func(n int) agenttest.Reply {
		if n == 1 {
			return agenttest.Reply{Text: "Held.", Hold: held}
		}
		return answer("Beside.")
	})
	ids := server.begin(t, agentRun{login: dispatchTestLogin, words: words}, agentRun{login: dispatchTestLogin, words: beside})
	run := ids[0]
	server.eventually(t, time.Minute, "the plan to wait for its person, beside the other run's model call", func() bool {
		return server.run(t, run).state == "waiting" && len(server.fake.Asking(beside)) == 1
	})

	// Killed with the plan's rows claimed and before their first call: the run, approved, is
	// leased to the agent while it is stopped, and the agent is killed.
	server.exec(t, `update going_runs set shape = $2 where id = $1::uuid`, run, unreadShape)
	server.call(t, http.MethodPost, "/api/v1/going/runs/"+run+"/plan/approve", `{"plan":1}`, http.StatusOK)
	server.stopHolding(t, first, run)
	signalProcess(t, first, syscall.SIGKILL)

	// Killed while a call is held before its route, and while one is held after its route committed.
	server.waitForLog(t, 2*time.Minute, `msg="dispatch: a test hook holds a call of the agent"`, comments(3))
	signalProcess(t, server.agentStarted(t, 2, 30*time.Second), syscall.SIGKILL)
	server.waitForLog(t, 3*time.Minute, `msg="dispatch: a test hook holds the answer of a call of the agent"`, comments(5))
	signalProcess(t, server.agentStarted(t, 3, 30*time.Second), syscall.SIGKILL)

	// The server killed while a call is held after its route committed, and started again on the
	// same database, without the hooks.
	server.waitForLog(t, 3*time.Minute, `msg="dispatch: a test hook holds the answer of a call of the agent"`, comments(7))
	if err := server.Cmd.Process.Kill(); err != nil {
		t.Fatalf("kill the server: %v", err)
	}
	<-server.Exited
	restarted := agentServer{
		dispatchProcess: startDispatchProcessWith(t, server.database.Pool.Config().ConnString(),
			dispatchOptions{Env: modelEnv(t, server.fake), OwnProcessGroup: true}),
		database: server.database,
	}
	restarted.waitHealthy(t)
	if row := restarted.ended(t, run, 3*time.Minute); row.state != "done" || row.kills >= 3 {
		t.Fatalf("the run ended %s with %q after %d deaths charged to it, want done", row.state, row.reason, row.kills)
	}
	for _, issue := range issues {
		var made int
		if err := server.database.Pool.QueryRow(context.Background(),
			`select count(*) from comments where issue_key = $1 and body like $2`, issue, "Change "+issue+"%").Scan(&made); err != nil {
			t.Fatal(err)
		}
		if made != 1 {
			t.Errorf("%s holds the plan's comment %d times, want once", issue, made)
		}
	}
	var steps int
	if err := server.database.Pool.QueryRow(context.Background(), `
		select count(distinct (body->'row'->>'row')::int) from going_run_steps where run_id = $1::uuid and body ? 'row'
	`, run).Scan(&steps); err != nil {
		t.Fatal(err)
	}
	if steps != 7 {
		t.Errorf("the run saved a step for %d of the plan's 7 rows, want each", steps)
	}
}

// requireTheWatchdogEndsAStuckAgent: a tool and a model stream that ignore their context hold
// their steps past the model call's time and 10 s: the watchdog gives up each run's lease and
// counts its goroutine stuck, and at 3 the agent exits and the server starts it again, while the
// server answers each request; the next agent works the runs to their end. Each run has 5 s of a
// Fast run's 90 s left, so its step's time is 5 s and the watchdog's 15 s.
func requireTheWatchdogEndsAStuckAgent(t *testing.T) {
	server := startModelServer(t, nil)
	first := server.agentStarted(t, 1, 30*time.Second)
	server.waitForAgentRenewal(t)
	stuck := map[string]agenttest.Reply{
		"hang in a tool":       toolCall("test_hang", map[string]any{}),
		"hang in another tool": toolCall("test_hang", map[string]any{}),
		"hang in the stream":   {Text: "[[test_hang]] and more"},
	}
	var runs []agentRun
	for words, reply := range stuck {
		server.script.set(words, func(n int) agenttest.Reply {
			if n == 1 {
				return reply
			}
			return answer("Done at last.")
		})
		runs = append(runs, agentRun{login: "alice", words: words, level: "fast", workingMS: 85_000})
	}
	ids := server.begin(t, runs...)
	next := server.agentStarted(t, 2, 90*time.Second)
	if next == first {
		t.Fatalf("the agent started again under the pid of the one that exited, %d", first)
	}
	if gaveUp := server.logLines(`msg="agent: the watchdog gave up a run`); len(gaveUp) != 3 {
		t.Errorf("the watchdog gave up %d runs before the agent exited, want 3:\n%s", len(gaveUp), strings.Join(gaveUp, "\n"))
	}
	server.requireServing(t)
	for _, id := range ids {
		if row := server.ended(t, id, 2*time.Minute); row.state != "done" {
			t.Errorf("run %s ended %s with %q, want done by the next agent", id, row.state, row.reason)
		}
	}
}

// requireAPanicInACallbackIsRecovered: a tool that panics on the model loop's own goroutines is
// recovered: the model is told the tool failed, the run answers, and the agent's process id does
// not change.
func requireAPanicInACallbackIsRecovered(t *testing.T) {
	server := startModelServer(t, nil)
	pid := server.agentStarted(t, 1, 30*time.Second)
	const words = "call the tool that panics"
	server.script.set(words, func(n int) agenttest.Reply {
		if n == 1 {
			return toolCall("test_panic", map[string]any{})
		}
		return answer("The tool failed, and the run goes on.")
	})
	run := server.begin(t, agentRun{login: "alice", words: words})[0]
	if row := server.ended(t, run, time.Minute); row.state != "done" {
		t.Fatalf("the run ended %s with %q, want done", row.state, row.reason)
	}
	if results := server.fake.Asking(words)[1].ToolResults(); len(results) != 1 || !strings.Contains(results[0], "panicked") {
		t.Errorf("the model was told %q of the tool, want that it panicked", results)
	}
	if pids := server.agentPids(); len(pids) != 1 || pids[0] != pid {
		t.Errorf("the agents started are %v, want the one, %d, whose callback panicked", pids, pid)
	}
}

// requireARunThatPanicsPastItsRecoveryFails: a run whose tool panics past every recover kills the
// agent each time it works; the death it causes beside another run is charged to neither, each
// death it causes alone is charged to it, and at the third it fails with that reason, while the
// run beside it ends as its own model answers, charged nothing.
func requireARunThatPanicsPastItsRecoveryFails(t *testing.T) {
	server := startModelServer(t, nil)
	server.waitForAgentRenewal(t)
	const besideWords, crashWords = "wait beside the crash", "call the tool that kills the agent"
	never := make(chan struct{})
	server.script.set(besideWords, func(n int) agenttest.Reply {
		if n == 1 {
			return agenttest.Reply{Hold: never}
		}
		return answer("Done beside it.")
	})
	server.script.set(crashWords, func(int) agenttest.Reply { return toolCall("test_crash", map[string]any{}) })
	beside := server.begin(t, agentRun{login: "alice", words: besideWords})[0]
	server.eventually(t, 30*time.Second, "the run beside to be at its model", func() bool { return len(server.fake.Asking(besideWords)) == 1 })
	crash := server.begin(t, agentRun{login: "alice", words: crashWords})[0]
	if row := server.ended(t, crash, 3*time.Minute); row.state != "failed" || !strings.HasPrefix(row.reason, "AGENT_KILLED") || row.kills != 3 {
		t.Errorf("the run that kills the agent ended %s with %q and %d kills, want failed with AGENT_KILLED at 3", row.state, row.reason, row.kills)
	}
	if row := server.ended(t, beside, time.Minute); row.state != "done" || row.kills != 0 {
		t.Errorf("the run beside it ended %s with %q and %d kills, want done and charged none", row.state, row.reason, row.kills)
	}
	// The run fails at its fourth death's report, before the server starts the agent after it.
	server.agentStarted(t, 5, 30*time.Second)
	if pids := server.agentPids(); len(pids) != 5 {
		t.Errorf("%d agents started, want 5: the first and one after each of its 4 deaths", len(pids))
	}
	server.requireServing(t)
}

// requireAPanickingRouteAnswersAnError: a route that panics while the server runs a call of the
// agent's answers the call an error, which the model is told; the run goes on to its answer, and
// the server's process serves on.
func requireAPanickingRouteAnswersAnError(t *testing.T) {
	server := startModelServer(t, map[string]string{"DISPATCH_TEST_AGENT_CALL_HOOK": "panic GET /api/v1/search"})
	const words = "search where the route panics"
	server.script.set(words, func(n int) agenttest.Reply {
		if n == 1 {
			return readCall("dispatch_search", map[string]any{"query": "panics"})
		}
		return answer("The search failed.")
	})
	run := server.begin(t, agentRun{login: "alice", words: words})[0]
	if row := server.ended(t, run, time.Minute); row.state != "done" {
		t.Fatalf("the run ended %s with %q, want done", row.state, row.reason)
	}
	if results := server.toolResults(t, run); len(results) != 1 || !strings.Contains(results[0], agentwire.CodeCallPanicked) {
		t.Errorf("the call was told %q, want %s", results, agentwire.CodeCallPanicked)
	}
	if !server.logged(`msg="dispatch: a call of a command-bar run panicked"`) {
		t.Error("the server did not log the call's panic")
	}
	select {
	case <-server.Exited:
		t.Fatalf("the server exited:\n%s", server.Output.String())
	default:
	}
	server.requireServing(t)
}

// requireTheAgentsSlots: the agent works 8 runs at once; a ninth waits in the queue until one of
// them ends.
func requireTheAgentsSlots(t *testing.T) {
	server := startModelServer(t, nil)
	release := make(chan struct{})
	var runs []agentRun
	for index := range 9 {
		words := fmt.Sprintf("slot run %d", index)
		server.script.set(words, func(n int) agenttest.Reply {
			if n == 1 {
				return agenttest.Reply{Hold: release, Text: "Done."}
			}
			return answer("Done.")
		})
		runs = append(runs, agentRun{login: fmt.Sprintf("person%d", index/3), words: words})
	}
	ids := server.begin(t, runs...)
	server.eventually(t, 30*time.Second, "8 runs at their model", func() bool { return server.fake.Count() == 8 })
	time.Sleep(3 * time.Second)
	if working, queued, calls := server.runs(t, "working"), server.runs(t, "queued"), server.fake.Count(); working != 8 || queued != 1 || calls != 8 {
		t.Errorf("%d runs work, %d are queued and the model has %d calls, want 8 working, 1 queued, 8 calls", working, queued, calls)
	}
	close(release)
	for _, id := range ids {
		if row := server.ended(t, id, time.Minute); row.state != "done" {
			t.Errorf("run %s ended %s with %q, want done", id, row.state, row.reason)
		}
	}
}

// requireOnePersonsRuns: two server processes and two agents share one database, and one person
// works at most 3 runs at once across them. Each agent works 2 runs at once, so of the person's
// first 3 runs each agent holds some; the person's next 2, queued once those 3 work, stay queued
// for longer than a claim waits, whichever agent asks, as the runs' lease owners show. And with 20
// of one person's runs queued, the start of a 21st is refused with its reason and writes nothing.
func requireOnePersonsRuns(t *testing.T) {
	t.Run("at most 3 of one person's runs work under two servers and their agents", func(t *testing.T) {
		database := storetest.Open(t)
		fake, script := newFakeModel(t)
		env := modelEnv(t, fake)
		env["DISPATCH_TEST_AGENT_SLOTS"] = "2"
		var servers []agentServer
		for range 2 {
			process := startDispatchProcessWith(t, database.Pool.Config().ConnString(),
				dispatchOptions{Env: env, OwnProcessGroup: true, SigningKey: multiprocessSigningKey})
			process.waitHealthy(t)
			server := agentServer{dispatchProcess: process, database: database}
			server.waitForAgentRenewal(t)
			servers = append(servers, server)
		}
		release := make(chan struct{})
		queue := func(from, to int) []string {
			t.Helper()
			var runs []agentRun
			for index := from; index < to; index++ {
				words := fmt.Sprintf("one of alice's runs, %d", index)
				script.set(words, func(int) agenttest.Reply { return agenttest.Reply{Hold: release, Text: "Done."} })
				runs = append(runs, agentRun{login: "alice", words: words})
			}
			return servers[0].begin(t, runs...)
		}
		// working is how many of alice's runs work under a live lease, by the agent instance that
		// holds each, and total their sum.
		working := func() map[string]int {
			t.Helper()
			rows, err := database.Pool.Query(context.Background(), `
				select lease_owner from going_runs where author_login = 'alice' and state = 'working' and lease_ends_at >= now()
			`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			owners := map[string]int{}
			for rows.Next() {
				var owner string
				if err := rows.Scan(&owner); err != nil {
					t.Fatal(err)
				}
				owners[owner]++
			}
			return owners
		}
		total := func(owners map[string]int) int {
			sum := 0
			for _, held := range owners {
				sum += held
			}
			return sum
		}
		ids := queue(0, 3)
		servers[0].eventually(t, time.Minute, "alice's first 3 runs to work under both agents", func() bool {
			owners := working()
			return total(owners) == 3 && len(owners) == 2
		})
		ids = append(ids, queue(3, 5)...)
		// A claim waits 25 s for a ready run and then asks again, so 35 s holds at least one claim
		// of each agent made after the 2 were queued, whether a notification woke it or not.
		for deadline := time.Now().Add(35 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			if owners := working(); total(owners) > 3 {
				t.Fatalf("%d of alice's runs work at once, held by the agents %v, want 3 at the most", total(owners), owners)
			}
		}
		if calls := fake.Count(); calls != 3 {
			t.Errorf("the model has %d of alice's calls, want the 3 runs' that work", calls)
		}
		close(release)
		for _, id := range ids {
			if row := servers[0].ended(t, id, time.Minute); row.state != "done" {
				t.Errorf("run %s ended %s with %q, want done", id, row.state, row.reason)
			}
		}
	})
	t.Run("the 21st queued run is refused with its reason", func(t *testing.T) {
		server := startAgentServer(t, nil)
		// Twenty queued at a shape no build reads, so no agent takes one while the 21st is asked for.
		for range 20 {
			server.recordRun(t, "alice", "queued", unreadShape)
		}
		answer := server.send(t, http.MethodPost, "/api/v1/going/runs", `{"words":"one run more"}`, asUser("alice"))
		if answer.status != http.StatusTooManyRequests || answer.code() != "GOING_RUN_QUEUE_FULL" || !strings.Contains(string(answer.body), "20 of your runs") {
			t.Errorf("the 21st start answered %s, want 429 GOING_RUN_QUEUE_FULL naming the 20 runs queued", answer)
		}
		if queued := server.runs(t, "queued"); queued != 20 {
			t.Errorf("%d runs are queued after the refused start, want the 20", queued)
		}
	})
}

// requireTheLevelsBudgets: a Fast run stops at its 6 model calls, at its 90 s of working time and
// at a call that writes its 4,000 output tokens, each with the bound's name, and offers Continue.
func requireTheLevelsBudgets(t *testing.T) {
	server := startModelServer(t, nil)
	server.waitForAgentRenewal(t)
	stopped := func(t *testing.T, run, reason string) {
		t.Helper()
		row := server.ended(t, run, 2*time.Minute)
		if row.state != "stopped" || !strings.HasPrefix(row.reason, reason) {
			t.Errorf("the run ended %s with %q, want stopped with %s", row.state, row.reason, reason)
		}
		if frame := server.endFrame(t, run); !slices.Equal(frame.Offers, []string{agentwire.OfferContinue}) {
			t.Errorf("the run's end offers %v, want Continue", frame.Offers)
		}
	}
	t.Run("a Fast run stops at its 6th model call", func(t *testing.T) {
		const words = "look for ever, quickly"
		server.script.set(words, func(int) agenttest.Reply { return toolCall("find_tools", map[string]any{"need": "an issue"}) })
		run := server.begin(t, agentRun{login: "alice", words: words, level: "fast"})[0]
		stopped(t, run, agent.ReasonCallBudget)
		if calls := len(server.fake.Asking(words)); calls != 6 {
			t.Errorf("the run made %d model calls, want 6", calls)
		}
	})
	t.Run("a Fast run stops at its 90 s of working time", func(t *testing.T) {
		const words = "think until the time is up"
		never := make(chan struct{})
		server.script.set(words, func(int) agenttest.Reply { return agenttest.Reply{Hold: never} })
		run := server.begin(t, agentRun{login: "alice", words: words, level: "fast", workingMS: 89_500})[0]
		stopped(t, run, agent.ReasonTimeBudget)
	})
	t.Run("a Fast run stops at a call that writes its 4,000 output tokens", func(t *testing.T) {
		const words = "write at length"
		server.script.set(words, func(int) agenttest.Reply {
			return agenttest.Reply{Text: "A reply cut at its tokens", Finish: "length", Output: 4000}
		})
		run := server.begin(t, agentRun{login: "alice", words: words, level: "fast"})[0]
		stopped(t, run, agent.ReasonOutputTokens)
		if tokens := server.fake.Asking(words)[0].Field("max_tokens"); tokens != float64(4000) {
			t.Errorf("the call asked for max_tokens %v, want 4000", tokens)
		}
	})
}

// requireOneModelCallsBounds: a model call's response is read up to 8 MiB and the reply it builds
// up to 512 KiB, past which the run fails and says why; and a 429 of the model is tried again
// after the time it names.
func requireOneModelCallsBounds(t *testing.T) {
	server := startModelServer(t, nil)
	server.waitForAgentRenewal(t)
	failed := func(t *testing.T, words string, reply agenttest.Reply, says string) {
		t.Helper()
		server.script.set(words, func(int) agenttest.Reply { return reply })
		run := server.begin(t, agentRun{login: "alice", words: words})[0]
		if row := server.ended(t, run, time.Minute); row.state != "failed" || !strings.HasPrefix(row.reason, "MODEL_REPLY_TOO_LARGE") || !strings.Contains(row.reason, says) {
			t.Errorf("the run ended %s with %q, want failed with MODEL_REPLY_TOO_LARGE naming %q", row.state, row.reason, says)
		}
	}
	t.Run("a response past 8 MiB fails the run", func(t *testing.T) {
		failed(t, "answer after a long stream", agenttest.Reply{Padding: 9 << 20, Text: "Short."}, "response passed its limit of 8388608 bytes")
	})
	t.Run("a reply past 512 KiB fails the run", func(t *testing.T) {
		failed(t, "answer at great length", agenttest.Reply{Text: strings.Repeat("plum ", 120_000)}, "reply passed its limit of 524288 bytes")
	})
	t.Run("a 429 is tried again after the time it names", func(t *testing.T) {
		const words = "answer when the quota allows"
		server.script.set(words, func(n int) agenttest.Reply {
			if n == 1 {
				return agenttest.Reply{Status: http.StatusTooManyRequests, RetryAfter: "2"}
			}
			return answer("Answered after the wait.")
		})
		run := server.begin(t, agentRun{login: "alice", words: words})[0]
		if row := server.ended(t, run, time.Minute); row.state != "done" {
			t.Fatalf("the run ended %s with %q, want done", row.state, row.reason)
		}
		requests := server.fake.Asking(words)
		if len(requests) != 2 || requests[1].At.Sub(requests[0].At) < 2*time.Second {
			t.Errorf("the model got %d calls, the second %s after the 429, want 2, at least 2 s apart", len(requests), requests[len(requests)-1].At.Sub(requests[0].At))
		}
	})
}

// requireOneReadToTheModel: the model is given at most 32 KiB of a read and a note that names the
// rest. The read is a 40 KiB document. The server's own bound of 1 MiB on a call's answer is
// TestTheAgentWireCalls' "an answer past 1 MiB is cut".
func requireOneReadToTheModel(t *testing.T) {
	server := startModelServer(t, nil)
	server.createProject(t, "READ", "Reads")
	_, document := server.createIssueIn(t, "READ", "A long spec", strings.Repeat("A line of the long spec.\n\n", 1640))
	const words = "read the long spec"
	server.script.set(words, func(n int) agenttest.Reply {
		if n == 1 {
			return readCall("dispatch_doc_read", map[string]any{"artifact": document})
		}
		return answer("Read part of it.")
	})
	run := server.begin(t, agentRun{login: dispatchTestLogin, words: words})[0]
	if row := server.ended(t, run, time.Minute); row.state != "done" {
		t.Fatalf("the run ended %s with %q, want done", row.state, row.reason)
	}
	results := server.fake.Asking(words)[1].ToolResults()
	if len(results) != 1 || len(results[0]) > 33<<10 || !strings.Contains(results[0], "you were given the first 32768") {
		t.Errorf("the model was given %d results, the first of %d bytes, want one of 32 KiB and a note naming the rest", len(results), len(results[0]))
	}
}

// requireOneStepsTime: a step whose tool ignores its context runs past its model call's time and
// 10 s: the watchdog gives up its lease and counts a stuck goroutine, which /metrics shows, and
// once the lease runs out the run is claimed again and works to its end.
func requireOneStepsTime(t *testing.T) {
	server := startModelServer(t, nil)
	server.waitForAgentRenewal(t)
	const words = "call the tool that never returns"
	server.script.set(words, func(n int) agenttest.Reply {
		if n == 1 {
			return toolCall("test_hang", map[string]any{})
		}
		return answer("Done the second time.")
	})
	run := server.begin(t, agentRun{login: "alice", words: words, level: "fast", workingMS: 85_000})[0]
	server.waitForLog(t, 40*time.Second, `msg="agent: the watchdog gave up a run`, "run="+run)
	server.eventually(t, 30*time.Second, "/metrics to show the stuck goroutine", func() bool {
		return server.scrape(t)[`envoy_dispatch_agent_stuck_goroutines`] == 1
	})
	if row := server.ended(t, run, 2*time.Minute); row.state != "done" || len(server.fake.Asking(words)) != 2 {
		t.Errorf("the run ended %s with %q after %d model calls, want done after 2", row.state, row.reason, len(server.fake.Asking(words)))
	}
}

// requireTheServersCallSlots: the server runs 16 calls for its agent at once, counted in the
// server; the 17th waits for a free one. 17 runs each read a route the test hook holds 20 s.
func requireTheServersCallSlots(t *testing.T) {
	server := startModelServer(t, map[string]string{
		"DISPATCH_TEST_AGENT_SLOTS":     "17",
		"DISPATCH_TEST_AGENT_CALL_HOOK": "hold 20s GET /api/v1/search",
	})
	var runs []agentRun
	for index := range 17 {
		words := fmt.Sprintf("held search %d", index)
		server.script.set(words, func(n int) agenttest.Reply {
			if n == 1 {
				return readCall("dispatch_search", map[string]any{"query": "held"})
			}
			return answer("Searched.")
		})
		runs = append(runs, agentRun{login: fmt.Sprintf("person%d", index/3), words: words})
	}
	for _, id := range server.begin(t, runs...) {
		if row := server.ended(t, id, 2*time.Minute); row.state != "done" {
			t.Errorf("run %s ended %s with %q, want done", id, row.state, row.reason)
		}
	}
	holds, releases := server.hookTimes(t, "holds"), server.hookTimes(t, "released")
	if len(holds) != 17 || len(releases) != 17 {
		t.Fatalf("the hook held %d calls and released %d, want 17 of each", len(holds), len(releases))
	}
	before := 0
	for _, held := range holds {
		if held.Before(releases[0]) {
			before++
		}
	}
	if before != 16 {
		t.Errorf("%d calls ran at once before the first ended, want 16", before)
	}
	if holds[16].Before(releases[0]) {
		t.Errorf("the 17th call entered its route at %s, before the first call ended at %s", holds[16], releases[0])
	}
}

// requireOneCallsTime: a call runs at most 60 s in the server, on a context of its own; a call
// past it fails with that reason, which the model is told, and the run goes on.
func requireOneCallsTime(t *testing.T) {
	server := startModelServer(t, map[string]string{"DISPATCH_TEST_AGENT_CALL_HOOK": "hold 70s GET /api/v1/search"})
	const words = "search for longer than a call may"
	server.script.set(words, func(n int) agenttest.Reply {
		if n == 1 {
			return readCall("dispatch_search", map[string]any{"query": "slow"})
		}
		return answer("The search ran out of time.")
	})
	run := server.begin(t, agentRun{login: "alice", words: words})[0]
	if row := server.ended(t, run, 3*time.Minute); row.state != "done" {
		t.Fatalf("the run ended %s with %q, want done", row.state, row.reason)
	}
	if results := server.toolResults(t, run); len(results) != 1 || !strings.Contains(results[0], agentwire.CodeCallFailed) || !strings.Contains(results[0], "ran past its 1m0s") {
		t.Errorf("the call was told %q, want %s naming its 60 s", results, agentwire.CodeCallFailed)
	}
	holds, releases := server.hookTimes(t, "holds"), server.hookTimes(t, "released")
	if len(holds) != 1 || len(releases) != 1 {
		t.Fatalf("the hook held %d calls and released %d, want 1 of each", len(holds), len(releases))
	}
	if took := releases[0].Sub(holds[0]); took < 59*time.Second || took > 65*time.Second {
		t.Errorf("the call ended %s after it entered its route, want at its 60 s", took)
	}
}

// requireASecondTryWaitsForTheFirst: a second try of a call's idempotency key waits for the first
// try, which runs on in the server after its agent died, and then runs. The first agent dies while
// the hook holds its call 20 s; the next one works the run again from its saved steps and makes the
// same call, whose route it enters only once the first try left it.
func requireASecondTryWaitsForTheFirst(t *testing.T) {
	server := startModelServer(t, map[string]string{"DISPATCH_TEST_AGENT_CALL_HOOK": "hold 20s GET /api/v1/search"})
	pid := server.agentStarted(t, 1, 30*time.Second)
	const words = "search, and search again after a crash"
	server.script.set(words, func(n int) agenttest.Reply {
		if n <= 2 {
			return readCall("dispatch_search", map[string]any{"query": "twice"})
		}
		return answer("Searched.")
	})
	run := server.begin(t, agentRun{login: "alice", words: words})[0]
	server.eventually(t, 30*time.Second, "the first try to be held", func() bool { return len(server.hookTimes(t, "holds")) == 1 })
	signalProcess(t, pid, syscall.SIGKILL)
	if row := server.ended(t, run, 2*time.Minute); row.state != "done" {
		t.Fatalf("the run ended %s with %q, want done", row.state, row.reason)
	}
	holds, releases := server.hookTimes(t, "holds"), server.hookTimes(t, "released")
	if len(holds) != 2 || len(releases) != 2 || len(server.fake.Asking(words)) != 3 {
		t.Fatalf("the hook held %d tries and released %d after %d model calls, want 2 tries of the call and 3 calls", len(holds), len(releases), len(server.fake.Asking(words)))
	}
	if holds[1].Before(releases[0]) {
		t.Errorf("the second try entered the route at %s, before the first left it at %s", holds[1], releases[0])
	}
}

// requireOneClaim: a claim waits 25 s at the most and answers that no run is ready, and the agent
// asks again: a run queued after that wait is worked.
func requireOneClaim(t *testing.T) {
	server := startModelServer(t, nil)
	server.waitForAgentRenewal(t)
	time.Sleep(27 * time.Second)
	const words = "a run after a quiet claim"
	server.script.set(words, func(int) agenttest.Reply { return answer("Claimed and done.") })
	run := server.begin(t, agentRun{login: "alice", words: words})[0]
	if row := server.ended(t, run, 30*time.Second); row.state != "done" {
		t.Errorf("the run ended %s with %q, want done by the agent's next claim", row.state, row.reason)
	}
}

// requireTheClaimGate: the agent takes no claim while its memory is above 70% of its GOMEMLIMIT,
// which the test sets at 1 MiB, under what any Go process holds: a ready run stays queued, and the
// agent says why.
func requireTheClaimGate(t *testing.T) {
	server := startModelServer(t, map[string]string{"DISPATCH_TEST_AGENT_GOMEMLIMIT": "1048576"})
	server.waitForAgentRenewal(t)
	const words = "a run the agent has no room for"
	server.script.set(words, func(int) agenttest.Reply { return answer("Done.") })
	run := server.begin(t, agentRun{login: "alice", words: words})[0]
	server.waitForLog(t, 30*time.Second, `msg="agent: its memory is above 70% of its limit, so it claims no run until it falls"`)
	time.Sleep(10 * time.Second)
	if row := server.run(t, run); row.state != "queued" || server.fake.Count() != 0 {
		t.Errorf("the run is %s after %d model calls, want queued with none", row.state, server.fake.Count())
	}
	server.requireServing(t)
}

// requireTheShortAnswersBounds: the short answer of R3, the model call that stays in the server
// with Jev's (R15), keeps its bounds in a real server: a person's new question ends the call of the
// one before; a stream past 4 MiB fails the answer, and one within it does not; an answer the model
// has not written at 30 s fails; and of 33 questions at once the 33rd waits for a free slot. Jev's
// call is made in the same run, inside the same bounds, which internal/dispatch/api's
// TestTheShortAnswerAndJevAreBoundedInTheServer holds with a decision model; this server names
// none.
func requireTheShortAnswersBounds(t *testing.T) {
	var mu sync.Mutex
	replies := map[string]agenttest.Reply{}
	// The answer asks the question as "question: <words>" on a line of its own.
	question := func(words string) string { return "question: " + words + "\n" }
	fake := agenttest.NewTLS(t, func(request *agenttest.Request) agenttest.Reply {
		mu.Lock()
		defer mu.Unlock()
		for words, reply := range replies {
			if strings.Contains(request.Text(), question(words)) {
				return reply
			}
		}
		return agenttest.Reply{Text: "No row of this test asks that."}
	})
	server := startAgentServer(t, modelEnv(t, fake))
	ask := func(t *testing.T, login, words string, reply agenttest.Reply) string {
		t.Helper()
		mu.Lock()
		replies[words] = reply
		mu.Unlock()
		body, _ := json.Marshal(map[string]any{"words": words})
		answer := server.send(t, http.MethodPost, "/api/v1/going/requests", string(body), asUser(login))
		var run struct {
			ID string `json:"id"`
		}
		if answer.status != http.StatusCreated || json.Unmarshal(answer.body, &run) != nil {
			t.Fatalf("%s asked %q: %d %s", login, words, answer.status, answer.body)
		}
		return run.ID
	}
	asked := func(words string) []*agenttest.Request { return fake.Matching(question(words)) }
	// ending waits up to within for run's terminal frame, and answers its name and data.
	ending := func(t *testing.T, run string, within time.Duration) (string, string) {
		t.Helper()
		var name, data string
		server.eventually(t, within, "the ending of run "+run, func() bool {
			err := server.database.Pool.QueryRow(context.Background(), `
				select name, data::text from going_events where request_id = $1::uuid and name in ('end', 'error') limit 1
			`, run).Scan(&name, &data)
			return err == nil
		})
		return name, data
	}

	t.Run("a person's new question ends the call of the one before", func(t *testing.T) {
		hold := make(chan struct{})
		defer close(hold)
		ask(t, "alice", "the words alice typed", agenttest.Reply{Hold: hold, Text: "An answer to words nobody reads now."})
		server.eventually(t, 30*time.Second, "the call of alice's first question", func() bool { return len(asked("the words alice typed")) == 1 })
		ask(t, "alice", "the words alice typed next", agenttest.Reply{Text: "An answer."})
		server.eventually(t, 10*time.Second, "the call of alice's first question to be left", func() bool {
			_, left := asked("the words alice typed")[0].Left()
			return left
		})
	})

	t.Run("a stream past 4 MiB fails, and one within it does not", func(t *testing.T) {
		const past, within = "stream past the limit", "stream within the limit"
		if name, data := ending(t, ask(t, "alice", past, agenttest.Reply{Padding: 4<<20 + 64<<10, Text: "An answer."}), 30*time.Second); name != "error" ||
			!strings.Contains(data, "ANSWER_FAILED") || len(asked(past)) != 1 {
			t.Errorf("a stream past 4 MiB ended %s %s after %d calls, want ANSWER_FAILED after the one", name, data, len(asked(past)))
		}
		if name, data := ending(t, ask(t, "alice", within, agenttest.Reply{Padding: 4<<20 - 512<<10, Text: "An answer."}), 30*time.Second); name != "end" {
			t.Errorf("a stream within 4 MiB ended %s %s, want answered", name, data)
		}
	})

	t.Run("an answer not written at 30 s fails", func(t *testing.T) {
		never := make(chan struct{})
		defer close(never)
		started := time.Now()
		name, data := ending(t, ask(t, "alice", "a question nobody answers", agenttest.Reply{Hold: never}), 45*time.Second)
		if took := time.Since(started); name != "error" || !strings.Contains(data, "ANSWER_DID_NOT_ARRIVE") || took < 29*time.Second || took > 36*time.Second {
			t.Errorf("an answer the model never wrote ended %s %s after %s, want ANSWER_DID_NOT_ARRIVE at its 30 s", name, data, took)
		}
	})

	t.Run("the 33rd question waits for a free slot", func(t *testing.T) {
		holds := make([]chan struct{}, 33)
		words := make([]string, 33)
		for asker := range holds {
			holds[asker], words[asker] = make(chan struct{}), fmt.Sprintf("the question of asker %02d", asker)
		}
		released := make([]bool, 33)
		defer func() {
			for asker, hold := range holds {
				if !released[asker] {
					close(hold)
				}
			}
		}()
		for asker := range holds {
			ask(t, fmt.Sprintf("asker-%02d", asker), words[asker], agenttest.Reply{Hold: holds[asker], Text: "An answer."})
		}
		waiting := func() []int {
			var none []int
			for asker := range words {
				if len(asked(words[asker])) == 0 {
					none = append(none, asker)
				}
			}
			return none
		}
		server.eventually(t, 30*time.Second, "32 of the 33 questions to call the model", func() bool { return len(waiting()) == 1 })
		time.Sleep(time.Second)
		last := waiting()
		if len(last) != 1 {
			t.Fatalf("questions %v have not called the model, want the one the 32 slots leave", last)
		}
		freed := (last[0] + 1) % len(holds)
		close(holds[freed])
		released[freed] = true
		server.eventually(t, 30*time.Second, "the 33rd question to call the model once one was answered", func() bool { return len(waiting()) == 0 })
	})
}
