package secrets_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/airlock/airlocktest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/logtest"
)

// tokenAnswerFault is how the answer to a token request breaks on its way back from a GitHub that
// has minted the token: GitHub holds a live token, and the minter gets no answer it can use.
type tokenAnswerFault string

const (
	// answerLost closes the connection before any of the answer arrives.
	answerLost tokenAnswerFault = "lost"
	// answerCut sends the 201 and the answer up to just past the token, and closes the connection.
	answerCut tokenAnswerFault = "cut off"
	// answerPastCap sends a 201 whose answer runs past the cap the minter reads a token answer to,
	// with the token first, where GitHub writes it.
	answerPastCap tokenAnswerFault = "past the cap"
	// answerPastCapTokenLast is that answer with the token only after the cap, which no read of
	// the cap can find.
	answerPastCapTokenLast tokenAnswerFault = "past the cap, its token after it"
	// answerUndecodable sends a 201 whose expires_at is no time, so the answer does not decode.
	answerUndecodable tokenAnswerFault = "undecodable"
	// answerNoExpiry sends a 201 with the token and no expires_at.
	answerNoExpiry tokenAnswerFault = "with no expiry"
)

// pastTheCap is more than the minter reads of a token answer (secrets' accessTokensResponseLimit,
// 1 MiB and 64 KiB for each of 100 repositories, 7.25 MiB in all).
const pastTheCap = 8 << 20

// breakingGitHub is the GitHub stand-in of gitHubCheckStand with each token request's answer
// broken as fault says: the stand-in mints the token, so it holds it as live as GitHub would, and
// what reaches the minter is the broken answer. Every other route is the stand-in's own. minted
// is every token the stand-in minted, in order.
type breakingGitHub struct {
	service *airlocktest.Service
	stand   *airlocktest.GitHubStand

	mu     sync.Mutex
	minted []string
}

func newBreakingGitHub(t *testing.T, fault tokenAnswerFault) *breakingGitHub {
	t.Helper()
	service, stand := gitHubCheckStand(t)
	g := &breakingGitHub{service: service, stand: stand}
	service.AnswerWith(func(w http.ResponseWriter, r *http.Request, seen airlocktest.Request) bool {
		if seen.Method != http.MethodPost || !strings.HasSuffix(seen.Path, "/access_tokens") {
			return stand.Answer(w, r, seen)
		}
		answered := httptest.NewRecorder()
		stand.Answer(answered, r, seen)
		var members map[string]json.RawMessage
		if answered.Code != http.StatusCreated || json.Unmarshal(answered.Body.Bytes(), &members) != nil {
			http.Error(w, fmt.Sprintf("the stand-in answered the token request %d, want a minted token", answered.Code), http.StatusInternalServerError)
			return true
		}
		var token string
		_ = json.Unmarshal(members["token"], &token)
		g.mu.Lock()
		g.minted = append(g.minted, token)
		g.mu.Unlock()
		g.breakAnswer(w, fault, members)
		return true
	})
	return g
}

// breakAnswer writes the stand-in's minted answer, members, broken as fault says.
func (g *breakingGitHub) breakAnswer(w http.ResponseWriter, fault tokenAnswerFault, members map[string]json.RawMessage) {
	whole := orderedAnswer(members, "token", "expires_at", "permissions", "repository_selection", "repositories")
	padding := []byte(`"padding":"` + strings.Repeat("x", pastTheCap) + `"`)
	switch fault {
	case answerLost:
		connection, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = connection.Close()
		}
	case answerCut:
		connection, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		// The token member whole and the start of the next one: the answer GitHub began, cut off.
		cut := len(`{"token":`) + len(members["token"]) + len(`,"expires_at"`)
		fmt.Fprintf(buffered, "HTTP/1.1 201 Created\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", len(whole))
		_, _ = buffered.Write(whole[:cut])
		_ = buffered.Flush()
		_ = connection.Close()
	case answerPastCap:
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(append(append(append(whole[:len(whole)-1:len(whole)-1], ','), padding...), '}'))
	case answerPastCapTokenLast:
		w.WriteHeader(http.StatusCreated)
		rest := orderedAnswer(members, "expires_at", "permissions", "repository_selection", "repositories", "token")
		_, _ = w.Write(append(append(append([]byte{'{'}, padding...), ','), rest[1:]...))
	case answerUndecodable:
		members["expires_at"] = json.RawMessage(`"soon"`)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(orderedAnswer(members, "token", "expires_at", "permissions", "repository_selection", "repositories"))
	case answerNoExpiry:
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(orderedAnswer(members, "token", "permissions", "repository_selection", "repositories"))
	}
}

// orderedAnswer is a JSON object of members' entries named in order, in that order, leaving out a
// name members does not hold: GitHub writes the token first, which a map's encoding does not.
func orderedAnswer(members map[string]json.RawMessage, order ...string) []byte {
	var answer bytes.Buffer
	answer.WriteByte('{')
	for _, name := range order {
		value, held := members[name]
		if !held {
			continue
		}
		if answer.Len() > 1 {
			answer.WriteByte(',')
		}
		fmt.Fprintf(&answer, "%q:%s", name, value)
	}
	answer.WriteByte('}')
	return answer.Bytes()
}

// tokenRequests is how many token requests the stand-in was sent, each of which minted a token.
func (g *breakingGitHub) tokenRequests() int {
	made := 0
	for _, request := range g.service.Requests() {
		if request.Method == http.MethodPost && strings.HasSuffix(request.Path, "/access_tokens") {
			made++
		}
	}
	return made
}

// lastMinted is the last token the stand-in minted, and "" where it minted none.
func (g *breakingGitHub) lastMinted() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.minted) == 0 {
		return ""
	}
	return g.minted[len(g.minted)-1]
}

// TestATokenWhoseAnswerTheMinterCannotUseIsGivenBack: GitHub mints a token when it answers a token
// request 201, before the answer reaches the minter, so an answer that is lost, cut off, past the
// minter's cap, undecodable or without an expiry leaves a live token at GitHub that nobody was
// handed. A check (MintRequest.Check) mints it before anybody approved anything, so it gives that
// token back whenever the part of the answer that arrived names it, and it sends a token request
// whose answer was lost once, so a lost answer leaves at most one token it cannot revoke rather
// than one for each attempt. A release whose answer is unusable gives its token back the same
// way, and it still sends a lost answer's request again, since its caller waits for the token
// itself. Each answer is the upstream's trouble rather than a refusal, so the broker's narrowing
// check lets the request through to its approver, and no error or log line holds the token. An
// answer whose arrived part does not name the token is logged as a token nothing can revoke.
func TestATokenWhoseAnswerTheMinterCannotUseIsGivenBack(t *testing.T) {
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	value := githubAppValue(t, "123456", key)
	lean := &secrets.GitHubNarrowing{Owner: "ada", Repositories: []string{"lean"}, ReadOnly: true}

	for _, tc := range []struct {
		fault tokenAnswerFault
		// checkRequests and releaseRequests are the token requests each run sends: one for a check
		// whatever the answer, and a release's three attempts for an answer that was lost.
		checkRequests, releaseRequests int
		// live is whether the minted token still reads at GitHub after the run: only where no
		// part of the answer that arrived names it.
		live bool
		// unrevokable is whether the run logs that GitHub holds a token nothing can revoke: a 201
		// that arrived whose part does not name the token. A connection lost before any answer
		// carries no 201, so nothing says a token was minted.
		unrevokable bool
	}{
		{fault: answerLost, checkRequests: 1, releaseRequests: 3, live: true},
		{fault: answerCut, checkRequests: 1, releaseRequests: 3},
		{fault: answerPastCap, checkRequests: 1, releaseRequests: 1},
		{fault: answerPastCapTokenLast, checkRequests: 1, releaseRequests: 1, live: true, unrevokable: true},
		{fault: answerUndecodable, checkRequests: 1, releaseRequests: 1},
		{fault: answerNoExpiry, checkRequests: 1, releaseRequests: 1},
	} {
		for _, run := range []struct {
			name     string
			check    bool
			requests int
		}{
			{name: "check", check: true, requests: tc.checkRequests},
			{name: "release", requests: tc.releaseRequests},
		} {
			t.Run(fmt.Sprintf("an answer %s, to a %s", tc.fault, run.name), func(t *testing.T) {
				var logged bytes.Buffer
				logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
				github := newBreakingGitHub(t, tc.fault)
				retry, _, _ := testRetry(atTheBottom)
				app := secrets.GitHubApp{Base: github.service.URL(), HTTP: http.DefaultClient, Retry: retry}

				minted, err := app.Mint(t.Context(), secrets.MintRequest{Value: value, GitHubOwners: []string{"ada"}, GitHubNarrowing: lean, Check: run.check})
				if minted.Value != "" || secrets.MintErrorClass(err) != "upstream" {
					t.Fatalf("the %s answered %q, %v (class %s); want nothing handed over and the upstream's trouble, which lets the request through to its approver",
						run.name, minted.Value, err, secrets.MintErrorClass(err))
				}
				if made := github.tokenRequests(); made != run.requests {
					t.Fatalf("the %s sent %d token requests, want %d: each one GitHub answers mints a token", run.name, made, run.requests)
				}
				token := github.lastMinted()
				if token == "" {
					t.Fatal("the stand-in minted no token, so the row reads nothing")
				}
				if strings.Contains(err.Error(), token) || strings.Contains(logged.String(), token) {
					t.Fatalf("the error %q or the log %q holds the token", err, logged.String())
				}
				want := http.StatusUnauthorized
				if tc.live {
					want = http.StatusOK
				}
				if status := reachesWith(t, github.service, token); status != want {
					t.Fatalf("a read made with the token GitHub minted answered %d after the %s, want %d (200 is a token live at GitHub, 401 one given back)",
						status, run.name, want)
				}
				if said := strings.Contains(logged.String(), "so it cannot be revoked"); said != tc.unrevokable {
					t.Fatalf("log = %q; a line saying the token cannot be revoked: %t, want %t", logged.String(), said, tc.unrevokable)
				}
			})
		}
	}
}
