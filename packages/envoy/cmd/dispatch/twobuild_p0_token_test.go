//go:build twobuild

// The p0 half of the two-build suite: a personal token this tree mints holding person, approve
// and p0 (dispatch://INFRA-5637), whose set agent_tokens' check admits from migration
// 20261126005333_agent_token_scopes_p0 on, and a token the previous build mints, each presented to
// both builds on the database both builds share (twobuild_harness_test.go holds the harness both
// builds start with).

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// twobuildToken is a personal token one build minted: what a row calls it, its id and its
// plaintext.
type twobuildToken struct {
	name, id, token string
}

// twobuildListedToken is the one member of a token list's row the p0 row reads.
type twobuildListedToken struct {
	ID string `json:"id"`
}

// A personal token keeps its routes on both builds while a release rolls, whichever build wrote
// it (G4 of dispatch://INFRA-5637). This tree mints a token of alice's holding person, approve and
// p0, and the previous build mints one holding person and approve, the shape that build writes.
// The row makes a project and four issues, one for each process and token, each with its spec. On
// each process, with each token: GET /api/v1/inbox answers 200; a review of the spec of the issue
// of that process and token (POST /api/v1/artifacts/{id}/reviews) answers 201; GET /api/v1/whoami
// answers 200; and GET /api/v1/me/agent-tokens answers 200 and lists the token. Nothing in it
// depends on which build is the earlier one, so it passes in the same-build job too.
func TestAPersonalTokenHoldingP0KeepsItsRoutesOnBothBuilds(t *testing.T) {
	builds := newTwoBuilds(t)
	previous := builds.startPrevious(t, nil)
	current := builds.startCurrent(t, nil)
	// The migration versions the database both builds share holds, logged before the first
	// request, as TestAReadOnlyPersonalTokenWritesThroughNeitherBuild logs them: a failure of the
	// row is read first against a migration one build carries and the other lacks.
	var versions []string
	if err := builds.database.Pool.QueryRow(context.Background(),
		"select coalesce(array_agg(version::text order by version), '{}') from schema_migrations").Scan(&versions); err != nil {
		t.Fatalf("read the migration versions the shared database holds: %v", err)
	}
	t.Logf("the database both builds share holds %d migration versions (schema_migrations): %v", len(versions), versions)
	project := uniqueProjectKey(t, "P0")
	current.createProject(t, project, "Tokens holding p0")

	processes := []struct {
		name    string
		process *dispatchProcess
	}{
		{builds.currentName(), current},
		{builds.previousName(), previous},
	}
	var tokens []twobuildToken
	for _, minter := range []struct {
		process *dispatchProcess
		name    string
		scopes  []string
	}{
		{current, "a token the " + builds.currentName() + " mints holding person, approve and p0",
			[]string{model.ScopePerson, model.ScopeApprove, model.ScopeP0}},
		{previous, "a token the " + builds.previousName() + " mints holding person and approve",
			[]string{model.ScopePerson, model.ScopeApprove}},
	} {
		var minted struct {
			ID     string   `json:"id"`
			Token  string   `json:"token"`
			Scopes []string `json:"scopes"`
		}
		answer := minter.process.call(t, http.MethodPost, "/api/v1/me/agent-tokens",
			jsonBody(t, map[string]any{"name": minter.name, "scopes": minter.scopes}), http.StatusCreated)
		if err := json.Unmarshal(answer, &minted); err != nil || minted.ID == "" || minted.Token == "" {
			t.Fatalf("decode %s: %v: %s", minter.name, err, answer)
		}
		if !slices.Equal(minted.Scopes, minter.scopes) {
			t.Fatalf("%s holds %q, want %q", minter.name, minted.Scopes, minter.scopes)
		}
		tokens = append(tokens, twobuildToken{name: minter.name, id: minted.ID, token: minted.Token})
	}
	// The spec each process and token reviews, one of its own, so no review is another's repeat.
	documents := map[string]string{}
	for _, process := range processes {
		for _, token := range tokens {
			_, documents[process.name+"\x00"+token.id] = current.createIssueIn(t, project,
				fmt.Sprintf("Reviewed with %s on the %s", token.name, process.name), "A spec of its own to review.")
		}
	}

	for _, process := range processes {
		for _, token := range tokens {
			t.Run(fmt.Sprintf("%s, on the %s", token.name, process.name), func(t *testing.T) {
				bearer := http.Header{"Authorization": {"Bearer " + token.token}}
				for _, request := range []struct {
					method, path, body string
					status             int
				}{
					{http.MethodGet, "/api/v1/inbox", "", http.StatusOK},
					{http.MethodPost, "/api/v1/artifacts/" + documents[process.name+"\x00"+token.id] + "/reviews", `{"state":"approved"}`, http.StatusCreated},
					{http.MethodGet, "/api/v1/whoami", "", http.StatusOK},
				} {
					if answer := process.process.send(t, request.method, request.path, request.body, bearer); answer.status != request.status {
						t.Errorf("%s %s: %s, want %d", request.method, request.path, answer, request.status)
					}
				}
				listed := process.process.send(t, http.MethodGet, "/api/v1/me/agent-tokens", "", bearer)
				if listed.status != http.StatusOK {
					t.Fatalf("GET /api/v1/me/agent-tokens: %s, want 200", listed)
				}
				var rows []twobuildListedToken
				if err := json.Unmarshal(listed.body, &rows); err != nil {
					t.Fatalf("decode the token list: %v: %s", err, listed.body)
				}
				if !slices.Contains(rows, twobuildListedToken{ID: token.id}) {
					t.Errorf("GET /api/v1/me/agent-tokens answered %s, which does not list the token %s", listed.body, token.id)
				}
			})
		}
	}
}
