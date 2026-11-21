package api

import (
	"net/http"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
)

// TestALiveSocketClosesWhenItsTokensListChanges: a live socket resolves its caller again on every
// heartbeat (whileCallerResolves), and a personal token whose list of projects or whose scopes
// changed since the upgrade is a caller other than the one the socket was admitted as, so the
// socket closes with 1008 and the page opens a new one under what the token holds now. carol's
// token lists PUB and PRIV; narrowing it to PUB closes its socket, and a socket of a token whose
// list did not change stays open.
//
// Control: the heartbeat asks only whether the token still resolves. The narrowed token's socket
// stays open, still reading PRIV's events under the list it was admitted with, and the test fails.
func TestALiveSocketClosesWhenItsTokensListChanges(t *testing.T) {
	seed := seedPrivateProjectWith(t, testServerOptions{streamHeartbeat: 50 * time.Millisecond})
	mint := func(name string) (id, token string, scopes []string) {
		t.Helper()
		minted := decodeBody[struct {
			ID     string   `json:"id"`
			Token  string   `json:"token"`
			Scopes []string `json:"scopes"`
		}](t, dispatchRequest(t, seed.handler, http.MethodPost, "/api/v1/me/agent-tokens",
			mintBody(name, []string{seedPublic, seedPrivate}), seedCarol))
		if minted.Token == "" {
			t.Fatalf("carol mints %q: no token in the answer", name)
		}
		return minted.ID, minted.Token, minted.Scopes
	}
	narrowedID, narrowed, scopes := mint("narrowed")
	_, kept, _ := mint("kept")
	url := liveURLOf(t, seed.handler)
	narrowedSocket := seed.dialLive(t, url, withBearer(narrowed))
	keptSocket := seed.dialLive(t, url, withBearer(kept))
	subscribeEvents(t, narrowedSocket, "feed", nil)
	subscribeEvents(t, keptSocket, "feed", nil)

	// The scopes the token was minted with, unchanged: only its list moves.
	if response := dispatchRequest(t, seed.handler, http.MethodPatch, "/api/v1/me/agent-tokens/"+narrowedID,
		map[string]any{"scopes": scopes, "projects": []string{seedPublic}}, seedCarol); response.Code != http.StatusOK {
		t.Fatalf("carol narrows her token to PUB: %d %s", response.Code, response.Body.String())
	}
	closed := narrowedSocket.closeError(t, 3*time.Second)
	if closed == nil || closed.Code != gws.ClosePolicyViolation {
		t.Fatalf("the socket of the token narrowed to PUB closed with %v, want 1008: it still reads under the list it was admitted with", closed)
	}
	keptSocket.quiet(t, 300*time.Millisecond, "the socket of the token whose list did not change")
}
