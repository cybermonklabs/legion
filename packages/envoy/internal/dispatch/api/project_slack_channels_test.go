package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestOnlyTheOwnerNamesTheProjectsSlackChannels: the Slack channels a project's credentials may
// post to are its owner's to name. Anyone else is 403 PROJECT_OWNER_REQUIRED and writes nothing;
// an id of another shape is 400 INVALID_SLACK_CHANNEL naming it, and more than sixteen is 400
// CAP_EXCEEDED; the owner's write is stored sorted and deduplicated and appends one
// project.access_changed with change slack_channels.
func TestOnlyTheOwnerNamesTheProjectsSlackChannels(t *testing.T) {
	t.Parallel()
	handler, database := newAccessServer(t)
	before := allEvents(t, database)
	wantRefusal(t, dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/ACC/access",
		map[string]any{"slack_channels": []string{"C012345678"}}, accessCarol),
		http.StatusForbidden, "PROJECT_OWNER_REQUIRED", accessCarol, "PATCH ACC's Slack channels")
	if got := readAccess(t, handler, "alice").SlackChannels; len(got) != 0 {
		t.Fatalf("ACC's channels after a refused write = %v, want none", got)
	}
	if after := allEvents(t, database); after != before {
		t.Fatalf("events moved from %d to %d under a refused write", before, after)
	}

	bad := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/ACC/access",
		map[string]any{"slack_channels": []string{"C012345678", "#general"}}, "alice")
	if bad.Code != http.StatusBadRequest || !strings.Contains(bad.Body.String(), `"code":"INVALID_SLACK_CHANNEL"`) ||
		!strings.Contains(bad.Body.String(), "#general") {
		t.Fatalf("an id of another shape = %d %s, want 400 INVALID_SLACK_CHANNEL naming it", bad.Code, bad.Body.String())
	}
	seventeen := make([]string, 17)
	for index := range seventeen {
		seventeen[index] = "C0000000" + string(rune('A'+index))
	}
	wantRefusal(t, dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/ACC/access",
		map[string]any{"slack_channels": seventeen}, "alice"),
		http.StatusBadRequest, "CAP_EXCEEDED", "alice", "PATCH seventeen Slack channels")

	written := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/ACC/access",
		map[string]any{"slack_channels": []string{"GABCDEFGH1", "C012345678", "C012345678"}}, "alice")
	want := []string{"C012345678", "GABCDEFGH1"}
	if written.Code != http.StatusOK || !slices.Equal(decodeBody[model.ProjectAccess](t, written).SlackChannels, want) {
		t.Fatalf("the owner's write = %d %s, want %v", written.Code, written.Body.String(), want)
	}
	if got := readAccess(t, handler, accessCarol).SlackChannels; !slices.Equal(got, want) {
		t.Fatalf("ACC's channels read back = %v, want %v", got, want)
	}
	if got := countRows(t, database, `
		select count(*) from events
		where type = 'project.access_changed' and project_key = 'ACC' and payload->>'change' = 'slack_channels'
	`); got != 1 {
		t.Fatalf("project.access_changed events with change slack_channels = %d, want 1", got)
	}
}

// TestTheAccessAnswerCarriesTheProjectsSlackChannels: the membership answer the secrets broker
// reads carries the owner's list, [] before any write and the stored list after it, and names no
// repositories: its JSON has no `repositories` key (GitHub events carry no project).
func TestTheAccessAnswerCarriesTheProjectsSlackChannels(t *testing.T) {
	t.Parallel()
	issuer := newServiceTokenIssuer(t)
	handler, database, _ := newTestServer(t, testServerOptions{oidc: issuer.verifier(t), accessAccounts: []string{listedServiceAccount}})
	seedPeople(t, database, accessCarol)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "ACC", "name": "Access"}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create ACC: %d %s", response.Code, response.Body.String())
	}
	bearer := issuer.Mint(t, issuer.signing, issuer.Claims(listedServiceAccount, serviceTokenAudience))
	read := func() []string {
		t.Helper()
		response := agentRequest(t, handler, http.MethodGet, "/api/v1/access/projects/ACC?person=alice", nil, bearer)
		if response.Code != http.StatusOK {
			t.Fatalf("the membership answer: %d %s", response.Code, response.Body.String())
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(response.Body.Bytes(), &fields); err != nil {
			t.Fatalf("decode the membership answer's fields: %v", err)
		}
		if repositories, ok := fields["repositories"]; ok {
			t.Fatalf("the membership answer carries the field repositories (%s); it names no repositories", repositories)
		}
		return decodeBody[model.AccessAnswer](t, response).SlackChannels
	}
	if got := read(); len(got) != 0 || got == nil {
		t.Fatalf("ACC's channels before any write = %v, want the empty list", got)
	}
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/ACC/access",
		map[string]any{"slack_channels": []string{"C012345678", "GABCDEFGH1"}}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("name ACC's channels: %d %s", response.Code, response.Body.String())
	}
	if got := read(); !slices.Equal(got, []string{"C012345678", "GABCDEFGH1"}) {
		t.Fatalf("ACC's channels in the membership answer = %v, want the two written", got)
	}
}
