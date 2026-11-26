package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

// TestAKeyedRepeatOfARouteThatAddressesNoObjectIsRefusedToACallerThatMayNotSeeEveryObject: a key
// belongs to a person whatever credential they write through (requestKeyLogin), so a repeat is held
// to what its caller sees now (heldAnswerHidden), and that is decided from the route's own address,
// as the first request's project check decides it (projectResolverFor). POST /api/v1/projects
// addresses no object, and its answer, a project, names its key and none of the names a stored
// answer was once read for. carol creates project SECR at her browser under a key; while no
// project is private, her token that lists no project repeats the request and is answered the
// first answer replayed, as every caller that sees every object is. SECR is then made private by
// SQL, and the same repeat through that token, which no longer sees SECR, is refused 409
// KEY_COMMITTED: not replayed, which would hand it SECR, and not run again, which would write a
// second time. Its answer holds no part of SECR.
//
// Control: heldAnswerHidden placing a stored answer by the names project, project_key and
// issue_key, as it did, under which the repeat replays the stored 201 with SECR in it and the test
// fails.
func TestAKeyedRepeatOfARouteThatAddressesNoObjectIsRefusedToACallerThatMayNotSeeEveryObject(t *testing.T) {
	t.Parallel()
	seed := seedPublicProjectsWith(t, testServerOptions{})
	payload, err := json.Marshal(map[string]string{"key": "SECR", "name": "The secret project"})
	if err != nil {
		t.Fatalf("encode the create: %v", err)
	}
	const key = "carol-creates-secr"
	keyed := func(caller privateCaller) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/projects", bytes.NewReader(payload))
		request.Header.Set("Content-Type", "application/json")
		if login := caller.login(); login != "" {
			request.Header.Set("X-Dispatch-User", login)
		} else {
			request.Header.Set("Authorization", "Bearer "+seed.bearer(t, caller))
		}
		request.Header.Set(contracts.IdempotencyKeyHeader, key)
		response := httptest.NewRecorder()
		seed.handler.ServeHTTP(response, request)
		return response
	}
	first := keyed(callerCarol)
	if first.Code != http.StatusCreated || !strings.Contains(first.Body.String(), `"SECR"`) {
		t.Fatalf("carol's keyed create of SECR = %d %s, want 201 with the project", first.Code, first.Body.String())
	}
	if again := keyed(callerCarolToken); again.Code != http.StatusCreated || again.Header().Get(contracts.IdempotencyReplayedHeader) == "" ||
		again.Body.String() != first.Body.String() {
		t.Fatalf("the repeat through carol's token while no project is private = %d %s (replayed %q), want the first answer replayed",
			again.Code, again.Body.String(), again.Header().Get(contracts.IdempotencyReplayedHeader))
	}

	if _, err := seed.database.Pool.Exec(t.Context(), `insert into session_projects_filled select now() where not exists (select 1 from session_projects_filled)`); err != nil {
		t.Fatalf("write the fill marker before making SECR private: %v", err)
	}
	if _, err := seed.database.Pool.Exec(t.Context(), `update projects set visibility = 'private' where key = 'SECR'`); err != nil {
		t.Fatalf("make SECR private: %v", err)
	}
	read := httptest.NewRequest(http.MethodGet, "/api/v1/issues", nil)
	read.Header.Set("Authorization", "Bearer "+seed.bearer(t, callerCarolToken))
	recorder := httptest.NewRecorder()
	reader, ok := seed.server.readerOf(recorder, read)
	if !ok || reader.Viewer == nil || seesEveryObject(reader) || slices.Contains(reader.Viewer.Sees, "SECR") {
		t.Fatalf("carol's token reads as %+v (%t, %d %s) once SECR is private, want a viewer that does not see SECR: the comparison proves nothing",
			reader.Viewer, ok, recorder.Code, recorder.Body.String())
	}
	repeat := keyed(callerCarolToken)
	code, _ := refusalOf(t, repeat)
	if repeat.Code != http.StatusConflict || code != contracts.IdempotencyKeyCommitted || strings.Contains(repeat.Body.String(), "SECR") ||
		repeat.Header().Get(contracts.IdempotencyReplayedHeader) != "" {
		t.Errorf("the repeat through carol's token once SECR is private = %d %s (replayed %q), want 409 %s holding no part of SECR",
			repeat.Code, repeat.Body.String(), repeat.Header().Get(contracts.IdempotencyReplayedHeader), contracts.IdempotencyKeyCommitted)
	}
	var projects int
	if err := seed.database.Pool.QueryRow(t.Context(), `select count(*) from projects where key = 'SECR'`).Scan(&projects); err != nil || projects != 1 {
		t.Errorf("projects named SECR = %d (%v), want the one the first request made: the repeat writes nothing", projects, err)
	}
}
