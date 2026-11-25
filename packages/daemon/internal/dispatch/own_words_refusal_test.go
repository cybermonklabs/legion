package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call"
	"github.com/sjawhar/envoy/steprecord"
)

// ownWordsKey is the issue these rows write, and ownWordsProject the project of the gated-steps
// publish. Neither is a credential: a path's segments are not read for any.
const (
	ownWordsKey     = "issue-key-path"
	ownWordsProject = "project-key-path"
)

// refusingDispatch is a Dispatch that answers every request with status and the coded refusal
// code and said, as Dispatch's writeError writes one.
func refusingDispatch(t *testing.T, status int, code, said string) *HTTPClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSONStatus(t, w, status, map[string]string{"code": code, "error": said})
	}))
	t.Cleanup(server.Close)
	return New(server.URL, BearerTransport(dispatchToken, nil))
}

// offRouteRefusal is the refusal the daemon's client prints (dispatchError) for Dispatch's status
// and coded refusal code and said, answered to a request the daemon's client never makes: body,
// sent with method to path, a route of Dispatch's where the names of body the public-name table
// scopes to the daemon's writes are not public.
func offRouteRefusal(t *testing.T, method, path string, body any, status int, code, said string) string {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(method, "https://dispatch.example"+path, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+dispatchToken)
	refusal, err := json.Marshal(map[string]string{"code": code, "error": said})
	if err != nil {
		t.Fatal(err)
	}
	answer, err := oauth2call.ReadAnswer(&http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(refusal)), Request: request}, 1<<10)
	if err != nil {
		t.Fatal(err)
	}
	return dispatchError(status, oauth2call.StatusLine(status), answer).Error()
}

// requireWords requires got, a refusal as the daemon prints it, to hold each of prints and none
// of redacted, which then stands as Redacted.
func requireWords(t *testing.T, got string, prints, redacted []string) {
	t.Helper()
	for _, want := range prints {
		if !strings.Contains(got, want) {
			t.Errorf("the refusal prints %q; want %q in it as Dispatch wrote it", got, want)
		}
	}
	for _, hidden := range redacted {
		if strings.Contains(got, hidden) || !strings.Contains(got, "[redacted]") {
			t.Errorf("the refusal prints %q; want %q redacted", got, hidden)
		}
	}
}

// TestTheDaemonsSessionPrintsInDispatchsRefusalOfItsOwnWrites: Dispatch's refusal of a write the
// daemon makes as its own session prints the word session where it quotes it, on each write
// whose every kind is the writer's own (the claim's release, a message, a document upload, the
// gated-steps, design-gates, role-holder, step, rating and plan writes): the daemon's actor.kind is
// the constant session (daemonActor), which the public-name table declares public there. The same
// actor on the issue's PATCH, whose external links' kinds another writer sends back as Dispatch
// answered them, and on the claim route's POST or a comment, routes the daemon's writes do not
// take, is a credential, and the word stands redacted.
func TestTheDaemonsSessionPrintsInDispatchsRefusalOfItsOwnWrites(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name string
		call func(*HTTPClient) error
	}{
		{"the claim's release", func(client *HTTPClient) error { return client.ReleaseClaim(ctx, ownWordsKey) }},
		{"a message", func(client *HTTPClient) error { return client.PostMessage(ctx, ownWordsKey, "posted words") }},
		{"a document upload", func(client *HTTPClient) error {
			_, err := client.UploadArtifact(ctx, ownWordsKey, "notes-of-a-run", "a document's text")
			return err
		}},
		{"the gated steps", func(client *HTTPClient) error {
			return client.PutGatedSteps(ctx, ownWordsProject, []string{"criteria"})
		}},
		{"the design gates", func(client *HTTPClient) error {
			return client.PutDesignGates(ctx, ownWordsKey, []DesignGateDocument{{Step: "criteria", ArtifactID: "artifact-one"}})
		}},
		{"the role holders", func(client *HTTPClient) error { return client.PutRoleHolders(ctx, ownWordsKey, nil) }},
		{"a step", func(client *HTTPClient) error {
			return client.PutStep(ctx, ownWordsKey, "step-record-id", StepWrite{Task: "task-one", Kind: "step"})
		}},
		{"a rating", func(client *HTTPClient) error {
			return client.PutStepRating(ctx, ownWordsKey, "retro-record-id", StepWrite{Task: "task-one", Kind: "retro"}, PlanRating{Rating: 3, Reason: "tests-came-late"})
		}},
		{"a plan", func(client *HTTPClient) error {
			return client.PutPlan(ctx, ownWordsKey, "plan-record-id", PlanWrite{Generation: 1, Source: "default", Tasks: []PlanTask{{ID: "task-one", Steps: []string{"tester"}}}})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := c.call(refusingDispatch(t, http.StatusConflict, "ISSUE_CLAIMED", "issue is claimed by another session"))
			if err == nil {
				t.Fatal("the write was answered as taken; want Dispatch's refusal")
			}
			requireWords(t, err.Error(), []string{"another session"}, nil)
		})
	}
	actor := daemonActor(ownWordsKey)
	for _, c := range []struct {
		name, method, path string
		body               any
	}{
		{"the issue's PATCH", http.MethodPatch, "/api/v1/issues/" + ownWordsKey, daemonWrite{Status: "testing", Actor: actor}},
		{"the claim route's POST", http.MethodPost, "/api/v1/issues/" + ownWordsKey + "/claim", daemonWrite{Actor: actor}},
		{"a comment", http.MethodPost, "/api/v1/issues/" + ownWordsKey + "/comments", daemonWrite{Body: "posted words", Actor: actor}},
		{"below the role holders", http.MethodPut, "/api/v1/issues/" + ownWordsKey + "/role-holders/extra", roleHoldersWrite{Holders: []RoleHolder{}, Actor: actor}},
	} {
		t.Run("off "+c.name, func(t *testing.T) {
			got := offRouteRefusal(t, c.method, c.path, c.body, http.StatusConflict, "ISSUE_CLAIMED", "issue is claimed by another session")
			requireWords(t, got, nil, []string{"session"})
		})
	}
}

// TestTheRolesAndStepKindsPrintInDispatchsRefusalOfRoleHolderAndStepWrites: Dispatch's refusal of
// the daemon's role-holder publication, step write or rating prints the role and the step kind it
// quotes: a role is one of the daemon's own role table (a claim's, or a plan step's, which plan
// validation holds to the built-in roles; a retro's implementer, a merge's merger) and a step's
// kind one of step, retro and merge, which the public-name table declares public on those writes.
// The session a holder or a step names is the one the claim's agent registered under, which the
// table keeps a credential everywhere but the broker's token exchange, so it stands redacted. Off
// those writes - below the role holders, on a step list with no record id, on the claim's
// release, which names no role - the role and the kind are credentials.
func TestTheRolesAndStepKindsPrintInDispatchsRefusalOfRoleHolderAndStepWrites(t *testing.T) {
	ctx := context.Background()
	session := "ses-step-held-name"
	retro := StepWrite{Task: "task-one", Kind: "retro", Role: "implementer", Session: &session}
	holders := []RoleHolder{{Role: "reviewer", Session: "ses-holder-one-two"}}
	for _, c := range []struct {
		name, said       string
		call             func(*HTTPClient) error
		prints, redacted []string
	}{
		{
			name: "the role holders", said: "role reviewer is named twice, once with session ses-holder-one-two",
			call:   func(client *HTTPClient) error { return client.PutRoleHolders(ctx, ownWordsKey, holders) },
			prints: []string{"role reviewer is named"}, redacted: []string{"ses-holder-one-two"},
		},
		{
			name: "a step", said: "kind retro of role implementer was ended by session ses-step-held-name before",
			call: func(client *HTTPClient) error {
				return client.PutStep(ctx, ownWordsKey, "retro-record-id", retro)
			},
			prints: []string{"kind retro of role implementer"}, redacted: []string{"ses-step-held-name"},
		},
		{
			name: "a rating", said: "kind retro of role implementer takes one rating",
			call: func(client *HTTPClient) error {
				return client.PutStepRating(ctx, ownWordsKey, "retro-record-id", StepWrite{Task: "task-one", Kind: "retro", Role: "implementer"}, PlanRating{Rating: 3, Reason: "tests-came-late"})
			},
			prints: []string{"kind retro of role implementer"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := c.call(refusingDispatch(t, http.StatusBadRequest, steprecord.CodeInput, c.said))
			if err == nil {
				t.Fatal("the write was answered as taken; want Dispatch's refusal")
			}
			requireWords(t, err.Error(), c.prints, c.redacted)
		})
	}
	actor := daemonActor(ownWordsKey)
	for _, c := range []struct {
		name, method, path, said string
		body                     any
		redacted                 []string
	}{
		{"below the role holders", http.MethodPut, "/api/v1/issues/" + ownWordsKey + "/role-holders/extra", "role reviewer is named twice",
			roleHoldersWrite{Holders: holders, Actor: actor}, []string{"reviewer"}},
		{"a step list with no record id", http.MethodPut, "/api/v1/issues/" + ownWordsKey + "/steps", "kind retro of role implementer",
			stepWrite{StepWrite: retro, Actor: actor}, []string{"retro", "implementer"}},
		{"the claim's release", http.MethodDelete, "/api/v1/issues/" + ownWordsKey + "/claim", "role reviewer holds no claim",
			roleHoldersWrite{Holders: holders, Actor: actor}, []string{"reviewer"}},
	} {
		t.Run("off "+c.name, func(t *testing.T) {
			requireWords(t, offRouteRefusal(t, c.method, c.path, c.body, http.StatusBadRequest, steprecord.CodeInput, c.said), nil, c.redacted)
		})
	}
}
