package api

import (
	"context"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestAReadRefreshesAnExpiredListenerAnswerBeforeItsContentRule: the session content rule
// (access.SessionContentClause), which every read of a session's conversation applies to each of
// its session parties, reads a listener list only from a stored listener answer that has not passed
// its expires_at, and judges a session of an expired one as a session with no list, which a session
// tied to nobody passes for no viewer but its own. So every request of a reader that does not see
// every object refreshes the stored answer before it reads (readerFor), and it takes the answer only
// while it has the margin of its life left that the request's own reads need
// (rosterCache.contentMargin, three seconds).
//
// refreshedListed is listed with PUB by the listener and tied to nobody; carol sends it a direct
// message. While PRIV is private alice, an open viewer who sees PUB, reads that message in the
// session's conversation (GET /api/v1/agents/{id}/messages) from a fresh stored answer, and again
// once that answer's expires_at is a second past: her request refreshes it first. With the answer
// still fresh but half the margin from its expires_at, her read asks the listener once, so the
// statements that follow her refresh never meet an answer that expired under them. With every
// project public alice sees every object, and her read of the same conversation from an expired
// answer asks the listener nothing (R9).
//
// Controls: the refresh taken out of readerFor, and alice's read from the expired answer holds no
// message; the margin at zero (contentMargin), and her read with half the margin left asks the
// listener nothing; the refresh made whatever the reader sees, and her read with every project
// public asks the listener once.
func TestAReadRefreshesAnExpiredListenerAnswerBeforeItsContentRule(t *testing.T) {
	t.Parallel()
	const refreshedListed = "ses-refreshed-listed"
	var fetches atomic.Int64
	listener, _ := newBroadcastListener(t,
		`[{"session_id":"`+refreshedListed+`","title":"Works on PUB","capabilities":["btw"],"last_seen":900,"projects":["PUB"]}]`,
		func(_ http.ResponseWriter, r *http.Request) bool {
			if r.Method == http.MethodGet && r.URL.Path == "/v1/sessions" {
				fetches.Add(1)
			}
			return false
		})
	seed := seedPrivateProjectWith(t, testServerOptions{envoyURL: listener.URL})
	ctx := context.Background()
	// An agents list stores the listener's answer, fresh, as any read of it does, so the session is
	// one Dispatch knows before carol messages it.
	seed.mustRequest(t, callerCarol, http.MethodGet, "/api/v1/agents", nil, http.StatusOK)
	sent := createdID(t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/agents/"+refreshedListed+"/messages",
		map[string]any{"body": "A direct message to the PUB session.", "delivery": "btw"}, http.StatusCreated))
	awaitBackgroundWrites(t, seed.server)
	var tied int
	if err := seed.database.Pool.QueryRow(ctx, `select count(*) from session_owners where session_id = $1`, refreshedListed).Scan(&tied); err != nil || tied != 0 {
		t.Fatalf("%s is tied to %d people (%v), want nobody: the row proves nothing", refreshedListed, tied, err)
	}
	// The answer fresh again, so the first read below reads its list at base as on the fix.
	if _, err := seed.database.Pool.Exec(ctx, `update route_roster set expires_at = now() + interval '1 hour'`); err != nil {
		t.Fatalf("make the stored listener answer fresh: %v", err)
	}
	read := func() []string {
		t.Helper()
		answer := seed.mustRequest(t, callerAlice, http.MethodGet, "/api/v1/agents/"+refreshedListed+"/messages", nil, http.StatusOK)
		ids := []string{}
		for _, item := range decodeBody[[]struct {
			Message model.Message `json:"message"`
		}](t, answer) {
			ids = append(ids, item.Message.ID)
		}
		return ids
	}
	expire := func() {
		t.Helper()
		if _, err := seed.database.Pool.Exec(ctx, `update route_roster set expires_at = now() - interval '1 second'`); err != nil {
			t.Fatalf("expire the stored listener answer: %v", err)
		}
	}
	if got := read(); !slices.Contains(got, sent) {
		t.Fatalf("PRIV private, the stored answer fresh: alice reads %v in %s's conversation, want carol's message %s: the row proves nothing", got, refreshedListed, sent)
	}
	expire()
	if got := read(); !slices.Contains(got, sent) {
		t.Errorf("PRIV private, the stored answer a second past its expires_at: alice reads %v in %s's conversation, want carol's message %s: her request must refresh the answer before the content rule reads it",
			got, refreshedListed, sent)
	}

	// Fresh, with half the margin left: a refresh that took it would leave the request's later
	// statements an answer about to expire. The read must ask the listener once.
	if _, err := seed.database.Pool.Exec(ctx, `update route_roster set expires_at = now() + make_interval(secs => $1)`,
		(routeRosterContentMargin / 2).Seconds()); err != nil {
		t.Fatalf("leave the stored listener answer half the margin of its life: %v", err)
	}
	before := fetches.Load()
	if got := read(); !slices.Contains(got, sent) {
		t.Errorf("PRIV private, the stored answer %s from its expires_at: alice reads %v in %s's conversation, want carol's message %s",
			routeRosterContentMargin/2, got, refreshedListed, sent)
	}
	if asked := fetches.Load() - before; asked != 1 {
		t.Errorf("PRIV private, the stored answer %s from its expires_at: alice's read asked the listener for its sessions %d times, want 1: a refresh must count an answer with less than %s of its life left as stale",
			routeRosterContentMargin/2, asked, routeRosterContentMargin)
	}

	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	expire()
	before = fetches.Load()
	if got := read(); !slices.Contains(got, sent) {
		t.Errorf("every project public: alice reads %v in %s's conversation, want carol's message %s", got, refreshedListed, sent)
	}
	if asked := fetches.Load() - before; asked != 0 {
		t.Errorf("every project public: alice's read of %s's conversation asked the listener for its sessions %d times, want none (R9)", refreshedListed, asked)
	}
}
