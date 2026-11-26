// credential_projects_test.go proves that a credential carries its list of projects: a personal
// token's agent_tokens.projects and a broker token's list reach the one rule of what a caller sees
// (internal/dispatch/access), and a mint or an edit lists only projects its caller sees.
package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The two projects every test here reads: PUB, public and alice's, and PRIV, carol's, which each
// test makes private. alice is a full member who sees PUB alone once PRIV is private; carol sees
// both.
const (
	publicProject  = "PUB"
	privateProject = "PRIV"
)

// listedToken is what the mint and the edit answer of a token, its list included.
type listedToken struct {
	ID       string   `json:"id"`
	Token    string   `json:"token"`
	Projects []string `json:"projects"`
}

// seedCredentialProjects makes PUB as alice and PRIV as carol, both public.
func seedCredentialProjects(t *testing.T, handler http.Handler, database *store.Store) {
	t.Helper()
	seedPeople(t, database, accessCarol)
	for _, project := range []struct{ key, creator string }{{publicProject, "alice"}, {privateProject, accessCarol}} {
		if created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
			"key": project.key, "name": project.key,
		}, project.creator); created.Code != http.StatusCreated {
			t.Fatalf("create %s as %s: status=%d body=%s", project.key, project.creator, created.Code, created.Body.String())
		}
	}
}

// makePrivate makes project private, naming channels when any are given. This release's route
// refuses the setting (409 PRIVATE_PROJECTS_OFF) until the deployment's bus can hold it, so the row
// is written as that write will write it, with the fill marker the switch requires first: a
// project the route made private implies session_projects_filled, since the switch refuses until
// it is written.
func makePrivate(t *testing.T, database *store.Store, project string, channels ...string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `update projects set visibility = 'private' where key = $1`, project); err != nil {
		t.Fatalf("make %s private: %v", project, err)
	}
	if len(channels) > 0 {
		if _, err := database.Pool.Exec(context.Background(), `update projects set slack_channels = $2 where key = $1`, project, channels); err != nil {
			t.Fatalf("name %s's Slack channels %v: %v", project, channels, err)
		}
	}
	if _, err := database.Pool.Exec(context.Background(), `insert into session_projects_filled select now() where not exists (select 1 from session_projects_filled)`); err != nil {
		t.Fatalf("write the fill marker beside %s: %v", project, err)
	}
}

// issueIn files an issue in project as login and answers its key.
func issueIn(t *testing.T, handler http.Handler, login, project string) string {
	t.Helper()
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": project, "title": "An issue of " + project,
	}, login)
	if created.Code != http.StatusCreated {
		t.Fatalf("file an issue in %s as %s: status=%d body=%s", project, login, created.Code, created.Body.String())
	}
	return decodeBody[struct {
		Key string `json:"key"`
	}](t, created).Key
}

// mintBody is the body of a mint of a token named name, naming projects unless it is nil.
func mintBody(name string, projects []string) map[string]any {
	body := map[string]any{"name": name}
	if projects != nil {
		body["projects"] = projects
	}
	return body
}

// mintListed mints login's token named name at the person's own browser, naming projects unless it
// is nil, and fails the test unless the mint answers 201.
func mintListed(t *testing.T, handler http.Handler, login, name string, projects []string) listedToken {
	t.Helper()
	minted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/me/agent-tokens", mintBody(name, projects), login)
	if minted.Code != http.StatusCreated {
		t.Fatalf("%s mints %q naming %v: status=%d body=%s", login, name, projects, minted.Code, minted.Body.String())
	}
	return decodeBody[listedToken](t, minted)
}

// bearerOf is a request carrying token as a bearer.
func bearerOf(token string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	return request
}
