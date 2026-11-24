//go:build twobuild

// The read-only personal token half of the two-build suite: a token this tree mints read-only,
// kept in agent_read_tokens (migration 20261120023712_agent_read_tokens), presented to the
// previous build on the database both builds share (twobuild_harness_test.go holds the harness
// both builds start with).

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A read-only personal token writes through neither build. This tree mints one holding read and
// person, the widest set a read-only token may hold, and the previous build is asked GET
// /api/v1/whoami, GET /api/v1/issues/{key} and POST /api/v1/issues/{key}/comments with it, on the
// database both builds share. A build before read-only tokens looks a bearer up in agent_tokens
// alone, finds no row and answers whoami 401, and then every request must answer 401. A build
// that knows read-only tokens, which is what CI's same-build job pairs, answers whoami 200 naming
// read, and then the write must answer 403 READ_ONLY_TOKEN. Neither branch writes a comment, and
// the row logs which branch it took. This tree's own whoami of the token is the control: it
// answers 200 naming read, so a 401 is the previous build's answer and not a token nothing knows.
func TestAReadOnlyPersonalTokenWritesThroughNeitherBuild(t *testing.T) {
	builds := newTwoBuilds(t)
	previous := builds.startPrevious(t, nil)
	current := builds.startCurrent(t, nil)
	// The migration versions the database both builds share holds, logged before the first
	// request: once main moves past this tree's base, the previous build can carry a migration
	// this tree lacks, and a failure of the row is then read first against that, not as a
	// failure of the branch it took. The harness logs none of its own.
	var versions []string
	if err := builds.database.Pool.QueryRow(context.Background(),
		"select coalesce(array_agg(version::text order by version), '{}') from schema_migrations").Scan(&versions); err != nil {
		t.Fatalf("read the migration versions the shared database holds: %v", err)
	}
	t.Logf("the database both builds share holds %d migration versions (schema_migrations): %v", len(versions), versions)
	project := uniqueProjectKey(t, "READ")
	current.createProject(t, project, "Read-only tokens")
	issue, _ := current.createIssueIn(t, project, "An issue a read-only token reads", "A spec to read.")

	var minted struct {
		Token string `json:"token"`
	}
	answer := current.call(t, http.MethodPost, "/api/v1/me/agent-tokens",
		jsonBody(t, map[string]any{"name": "twobuild-reader", "scopes": []string{model.ScopeRead, model.ScopePerson}}), http.StatusCreated)
	if err := json.Unmarshal(answer, &minted); err != nil || minted.Token == "" {
		t.Fatalf("decode the minted token: %v: %s", err, answer)
	}
	bearer := http.Header{"Authorization": {"Bearer " + minted.Token}}
	// whoamiScopes is the one member of a whoami answer the row reads. Each build's answer is
	// decoded into a value of its own, so one build's scopes never stand in for the other's.
	type whoamiScopes struct {
		Scopes []string `json:"scopes"`
	}
	var ownWhoami whoamiScopes
	own := current.send(t, http.MethodGet, "/api/v1/whoami", "", bearer)
	if own.status != http.StatusOK || json.Unmarshal(own.body, &ownWhoami) != nil || !slices.Contains(ownWhoami.Scopes, model.ScopeRead) {
		t.Fatalf("the %s's whoami of its own read-only token: %s, want 200 naming read", builds.currentName(), own)
	}

	const comment = "Written under a read-only token through the previous build."
	asked := previous.send(t, http.MethodGet, "/api/v1/whoami", "", bearer)
	read := previous.send(t, http.MethodGet, "/api/v1/issues/"+issue, "", bearer)
	write := previous.send(t, http.MethodPost, "/api/v1/issues/"+issue+"/comments", jsonBody(t, map[string]any{
		"body": comment, "actor": map[string]string{"kind": "session", "id": "twobuild-reader"},
	}), bearer)
	switch asked.status {
	case http.StatusUnauthorized:
		t.Logf("the %s answers the read-only token's whoami 401: it predates read-only tokens, so every request must answer 401", builds.previousName())
		for _, request := range []struct {
			name   string
			answer httpAnswer
		}{
			{"GET /api/v1/issues/" + issue, read},
			{"POST /api/v1/issues/" + issue + "/comments", write},
		} {
			if request.answer.status != http.StatusUnauthorized {
				t.Errorf("the %s answered %s %s, want 401 as it answered whoami", builds.previousName(), request.name, request.answer)
			}
		}
	case http.StatusOK:
		var previousWhoami whoamiScopes
		if json.Unmarshal(asked.body, &previousWhoami) != nil || !slices.Contains(previousWhoami.Scopes, model.ScopeRead) {
			t.Fatalf("the %s answered whoami %s, want it to name read: a build that resolves the token must read it as read-only", builds.previousName(), asked)
		}
		t.Logf("the %s answers the read-only token's whoami 200 naming read: it knows read-only tokens, so the write must answer 403 %s",
			builds.previousName(), contracts.DispatchReadOnlyToken)
		if write.status != http.StatusForbidden || write.code() != contracts.DispatchReadOnlyToken {
			t.Errorf("the %s answered the comment %s, want 403 %s", builds.previousName(), write, contracts.DispatchReadOnlyToken)
		}
	default:
		t.Fatalf("the %s answered the read-only token's whoami %s, want 401 (a build before read-only tokens) or 200 (one that knows them)", builds.previousName(), asked)
	}

	var comments int
	if err := builds.database.Pool.QueryRow(context.Background(),
		`select count(*) from comments where body = $1`, comment).Scan(&comments); err != nil {
		t.Fatalf("count the comment: %v", err)
	}
	if comments != 0 {
		t.Fatalf("the %s wrote %d comments under a read-only token, want none", builds.previousName(), comments)
	}
}
