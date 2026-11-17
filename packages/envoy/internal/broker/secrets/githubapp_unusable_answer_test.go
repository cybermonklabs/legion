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
// has minted the token: GitHub holds a live token, and the minter gets no answer it can use. The
// empty fault is the whole answer, as GitHub sends it.
type tokenAnswerFault string

const (
	// answerWhole is the stand-in's own answer, unbroken.
	answerWhole tokenAnswerFault = ""
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

// breakingGitHub is the airlock GitHub stand-in, installed on acme-org (1) and ada (2) and minting
// a token of its own for every token request (GitHubOptions.DistinctTokens), as GitHub does, with
// the answer to its nth token request broken as faults[n] says and every answer past the list
// whole. The stand-in mints each token before its answer breaks, so it holds every one as live as
// GitHub would, and its revoke route forgets the one it is given. Every other route is the
// stand-in's own. minted is every token it minted, in order.
type breakingGitHub struct {
	service *airlocktest.Service

	mu     sync.Mutex
	minted []string
}

func newBreakingGitHub(t *testing.T, faults ...tokenAnswerFault) *breakingGitHub {
	t.Helper()
	stand := airlocktest.NewGitHubStand(airlocktest.GitHubOptions{Installations: map[string]int64{"acme-org": 1, "ada": 2}, DistinctTokens: true})
	service, err := airlocktest.NewService(airlocktest.ServiceOptions{Plain: true})
	if err != nil {
		t.Fatalf("start the GitHub stand-in: %v", err)
	}
	t.Cleanup(service.Close)
	g := &breakingGitHub{service: service}
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
		fault := answerWhole
		if n := len(g.minted) - 1; n < len(faults) {
			fault = faults[n]
		}
		g.mu.Unlock()
		breakAnswer(w, fault, members)
		return true
	})
	return g
}

// breakAnswer writes the stand-in's minted answer, members, broken as fault says.
func breakAnswer(w http.ResponseWriter, fault tokenAnswerFault, members map[string]json.RawMessage) {
	whole := orderedAnswer(members, "token", "expires_at", "permissions", "repository_selection", "repositories")
	padding := []byte(`"padding":"` + strings.Repeat("x", pastTheCap) + `"`)
	switch fault {
	case answerWhole:
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(whole)
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

// tokens is every token the stand-in minted, one for each token request it served.
func (g *breakingGitHub) tokens() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.minted...)
}

// live is the tokens the stand-in still holds as live: a read made with each is answered as by a
// credential it minted (200 for a repository the token reaches, 404 for one it does not), where a
// token it forgot at its revoke is answered 401, as one it never minted is.
func (g *breakingGitHub) live(t *testing.T, tokens []string) []string {
	t.Helper()
	var live []string
	for _, token := range tokens {
		if reachesWith(t, g.service, token) != http.StatusUnauthorized {
			live = append(live, token)
		}
	}
	return live
}

// TestATokenWhoseAnswerTheMinterCannotUseIsGivenBack: GitHub mints a token when it answers a token
// request 201, before the answer reaches the minter, so an answer that is lost, cut off, past the
// minter's cap, undecodable or without an expiry leaves a live token at GitHub that nobody was
// handed. The stand-in mints a token of its own for every request, so every token is read on its
// own: each token whose answer names it is revoked before the request is sent again or the mint
// answers, so a release that rides out a cut-off answer hands over the second token and leaves
// the first one dead, and a release that fails at its second installation revokes the token of
// its first, which it would have handed over. A check (MintRequest.Check) mints before anybody
// approved anything, and it sends a token request whose answer was lost once, so a lost answer
// leaves it one token it cannot revoke rather than one for each attempt; a release still sends
// such a request again, and each lost answer leaves its token live, which nothing can name. Every
// unusable answer is the upstream's trouble rather than a refusal, so the broker's narrowing check
// lets the request through to its approver, and no error or log line holds a token. An answer
// whose arrived part does not name the token is logged as a token nothing can revoke.
func TestATokenWhoseAnswerTheMinterCannotUseIsGivenBack(t *testing.T) {
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	value := githubAppValue(t, "123456", key)
	lean := &secrets.GitHubNarrowing{Owner: "ada", Repositories: []string{"lean"}, ReadOnly: true}

	for _, tc := range []struct {
		name  string
		check bool
		// narrowing is the run's narrowing, and nil for an owner-wide release of both accounts.
		narrowing *secrets.GitHubNarrowing
		faults    []tokenAnswerFault
		// requests is the token requests the run sends, each of which mints a token.
		requests int
		// live is how many of those tokens are still live after the run: a lost answer's, which
		// nothing can name, an answer's whose arrived part does not hold its token, and the one
		// token a release hands over.
		live int
		// released is whether the run hands over a value, which only a release that rode out a
		// cut-off answer does here.
		released bool
		// unrevokable is whether the run logs that GitHub holds a token nothing can revoke: a 201
		// whose arrived part does not name the token. A connection lost before any answer carries
		// no 201, so nothing says a token was minted.
		unrevokable bool
	}{
		{name: "an answer lost, to a check", check: true, narrowing: lean, faults: []tokenAnswerFault{answerLost, answerLost, answerLost}, requests: 1, live: 1},
		{name: "an answer lost, to a release", narrowing: lean, faults: []tokenAnswerFault{answerLost, answerLost, answerLost}, requests: 3, live: 3},
		{name: "an answer cut off, to a check", check: true, narrowing: lean, faults: []tokenAnswerFault{answerCut, answerCut, answerCut}, requests: 1},
		{name: "an answer cut off, to a release", narrowing: lean, faults: []tokenAnswerFault{answerCut, answerCut, answerCut}, requests: 3},
		{name: "an answer cut off and then a whole one, to a check", check: true, narrowing: lean, faults: []tokenAnswerFault{answerCut}, requests: 1},
		{name: "an answer cut off and then a whole one, to a release", narrowing: lean, faults: []tokenAnswerFault{answerCut}, requests: 2, live: 1, released: true},
		{name: "an answer past the cap, to a check", check: true, narrowing: lean, faults: []tokenAnswerFault{answerPastCap}, requests: 1},
		{name: "an answer past the cap, to a release", narrowing: lean, faults: []tokenAnswerFault{answerPastCap}, requests: 1},
		{name: "an answer past the cap with its token after it, to a check", check: true, narrowing: lean, faults: []tokenAnswerFault{answerPastCapTokenLast}, requests: 1, live: 1, unrevokable: true},
		{name: "an answer past the cap with its token after it, to a release", narrowing: lean, faults: []tokenAnswerFault{answerPastCapTokenLast}, requests: 1, live: 1, unrevokable: true},
		{name: "an undecodable answer, to a check", check: true, narrowing: lean, faults: []tokenAnswerFault{answerUndecodable}, requests: 1},
		{name: "an undecodable answer, to a release", narrowing: lean, faults: []tokenAnswerFault{answerUndecodable}, requests: 1},
		{name: "an answer with no expiry, to a check", check: true, narrowing: lean, faults: []tokenAnswerFault{answerNoExpiry}, requests: 1},
		{name: "an answer with no expiry, to a release", narrowing: lean, faults: []tokenAnswerFault{answerNoExpiry}, requests: 1},
		{name: "a release of two accounts whose second answer is undecodable", faults: []tokenAnswerFault{answerWhole, answerUndecodable}, requests: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged bytes.Buffer
			logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
			github := newBreakingGitHub(t, tc.faults...)
			retry, _, _ := testRetry(atTheBottom)
			app := secrets.GitHubApp{Base: github.service.URL(), HTTP: http.DefaultClient, Retry: retry}

			owners := []string{"ada"}
			if tc.narrowing == nil {
				owners = []string{"acme-org", "ada"}
			}
			minted, err := app.Mint(t.Context(), secrets.MintRequest{Value: value, GitHubOwners: owners, GitHubNarrowing: tc.narrowing, Check: tc.check})
			if tc.released {
				if err != nil || minted.Value == "" {
					t.Fatalf("the release answered %v, handing over %d bytes; want a value", err, len(minted.Value))
				}
			} else if minted.Value != "" || secrets.MintErrorClass(err) != "upstream" {
				t.Fatalf("the run answered %q, %v (class %s); want nothing handed over and the upstream's trouble, which lets the request through to its approver",
					minted.Value, err, secrets.MintErrorClass(err))
			}
			tokens := github.tokens()
			if len(tokens) != tc.requests {
				t.Fatalf("the run sent %d token requests, want %d: GitHub mints a token for each one it answers", len(tokens), tc.requests)
			}
			for _, token := range tokens {
				if (err != nil && strings.Contains(err.Error(), token)) || strings.Contains(logged.String(), token) {
					t.Fatalf("the error %v or the log %q holds a token", err, logged.String())
				}
			}
			live := github.live(t, tokens)
			if len(live) != tc.live {
				t.Fatalf("%d of the %d tokens GitHub minted are still live after the run (%q), want %d", len(live), len(tokens), live, tc.live)
			}
			if tc.released {
				var handed mintedGitHubApp
				if err := json.Unmarshal([]byte(minted.Value), &handed); err != nil {
					t.Fatalf("the release's value: %v", err)
				}
				if got := handed.Installations["ada"].Token; len(live) != 1 || got != live[0] || got != tokens[len(tokens)-1] {
					t.Fatalf("the release handed over the token of request %d of %d; want the last one, the only one left live", indexOf(tokens, got)+1, len(tokens))
				}
			}
			if said := strings.Contains(logged.String(), "so it cannot be revoked"); said != tc.unrevokable {
				t.Fatalf("log = %q; a line saying the token cannot be revoked: %t, want %t", logged.String(), said, tc.unrevokable)
			}
		})
	}
}

// indexOf is where token stands in tokens, and -1 where it is none of them.
func indexOf(tokens []string, token string) int {
	for i, minted := range tokens {
		if minted == token {
			return i
		}
	}
	return -1
}
