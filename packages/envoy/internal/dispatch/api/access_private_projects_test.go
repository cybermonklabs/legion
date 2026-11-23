package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

const privateProjectsPath = "/api/v1/access/private-projects"

// TestTheBrokerListsThePrivateProjectsChannels (c-a-slack-channels): the secrets broker's own
// service-account token reads, for every private project and sorted by key, its key, its Slack
// channels ([] for none) and when that list last changed (null until a write changes the list),
// and nothing of a public project, whatever channels that one names. A dsp_ token and a person's
// browser are 403 ACCESS_ACCOUNT_REQUIRED, and a caller with no credential is 401 NO_IDENTITY.
// Control: admit any token, and the dsp_ token's 200 fails the test naming it.
func TestTheBrokerListsThePrivateProjectsChannels(t *testing.T) {
	t.Parallel()
	issuer := newServiceTokenIssuer(t)
	handler, database, _ := newTestServer(t, testServerOptions{oidc: issuer.verifier(t), accessAccounts: []string{listedServiceAccount}})
	for _, key := range []string{"PUB", "PRIV", "ALPHA"} {
		createAccessProject(t, handler, key, "alice")
	}
	patch := func(key string, channels ...string) {
		t.Helper()
		if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/"+key+"/access",
			map[string]any{"slack_channels": channels}, "alice"); response.Code != http.StatusOK {
			t.Fatalf("alice names %s's channels: %d %s", key, response.Code, response.Body.String())
		}
	}
	// PUB names a channel while public; PRIV names two once private; ALPHA is private and names none.
	patch("PUB", "C0PUBLIC001")
	makePrivate(t, database, "PRIV")
	makePrivate(t, database, "ALPHA")
	patch("PRIV", "GPRIV000002", "C0PRIV00001")
	var changedAt time.Time
	if err := database.Pool.QueryRow(context.Background(), `select slack_channels_changed_at from projects where key = 'PRIV'`).Scan(&changedAt); err != nil {
		t.Fatalf("read PRIV's channels time: %v", err)
	}
	stamp, err := json.Marshal(changedAt)
	if err != nil {
		t.Fatalf("encode PRIV's channels time: %v", err)
	}

	bearer := issuer.Mint(t, issuer.signing, issuer.Claims(listedServiceAccount, serviceTokenAudience))
	response := agentRequest(t, handler, http.MethodGet, privateProjectsPath, nil, bearer)
	want := `{"projects":[` +
		`{"project":"ALPHA","slack_channels":[],"slack_channels_changed_at":null},` +
		`{"project":"PRIV","slack_channels":["C0PRIV00001","GPRIV000002"],"slack_channels_changed_at":` + string(stamp) + `}` +
		"]}\n"
	if response.Code != http.StatusOK || response.Body.String() != want {
		t.Fatalf("the broker's GET %s = %d %s, want 200 %s", privateProjectsPath, response.Code, response.Body.String(), want)
	}

	dsp := personalTokenOwnedBy(t, database, "alice", model.ScopePerson)
	wantRefusal(t, agentRequest(t, handler, http.MethodGet, privateProjectsPath, nil, dsp),
		http.StatusForbidden, "ACCESS_ACCOUNT_REQUIRED", "alice's dsp_ token", "GET "+privateProjectsPath)
	wantRefusal(t, dispatchRequest(t, handler, http.MethodGet, privateProjectsPath, nil, "alice"),
		http.StatusForbidden, "ACCESS_ACCOUNT_REQUIRED", "alice at her browser", "GET "+privateProjectsPath)
	wantRefusal(t, dispatchRequest(t, handler, http.MethodGet, privateProjectsPath, nil, ""),
		http.StatusUnauthorized, "NO_IDENTITY", "a caller with no credential", "GET "+privateProjectsPath)
}

// TestThePrivateProjectsListIsAnEmptyArrayWhenNoneIsPrivate (c-a-slack-channels): with no private
// project, a public one naming a channel included, the broker's token reads 200 and the body
// {"projects":[]} byte for byte, then the newline every JSON answer ends with. projects is never
// null, missing or another type, because plan E's t-e-51 reads any of those as the membership
// answer being down. Control: start the list from a nil slice, and the body reads
// {"projects":null}, and the test fails naming projects.
func TestThePrivateProjectsListIsAnEmptyArrayWhenNoneIsPrivate(t *testing.T) {
	t.Parallel()
	issuer := newServiceTokenIssuer(t)
	handler, _, _ := newTestServer(t, testServerOptions{oidc: issuer.verifier(t), accessAccounts: []string{listedServiceAccount}})
	createAccessProject(t, handler, "PUB", "alice")
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/PUB/access",
		map[string]any{"slack_channels": []string{"C0PUBLIC001"}}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("alice names PUB's channel: %d %s", response.Code, response.Body.String())
	}
	bearer := issuer.Mint(t, issuer.signing, issuer.Claims(listedServiceAccount, serviceTokenAudience))
	response := agentRequest(t, handler, http.MethodGet, privateProjectsPath, nil, bearer)
	if want := "{\"projects\":[]}\n"; response.Code != http.StatusOK || response.Body.String() != want {
		t.Fatalf("the broker's GET %s with no private project = %d %q, want 200 %q: projects must be an empty array", privateProjectsPath, response.Code, response.Body.String(), want)
	}
}

// TestTheBrokersListIsSortedByKey: GET /api/v1/access/private-projects answers the private
// projects sorted by key, whatever order the table holds them in. ZED, MID and ALPHA are made, and
// then made private, in that order, so each project's live row is written after the one before it
// and a read in the table's own order answers ZED first. The break it catches: PrivateProjects
// losing `order by key`.
func TestTheBrokersListIsSortedByKey(t *testing.T) {
	t.Parallel()
	issuer := newServiceTokenIssuer(t)
	handler, database, _ := newTestServer(t, testServerOptions{oidc: issuer.verifier(t), accessAccounts: []string{listedServiceAccount}})
	for _, key := range []string{"ZED", "MID", "ALPHA"} {
		createAccessProject(t, handler, key, "alice")
	}
	for _, key := range []string{"ZED", "MID", "ALPHA"} {
		if _, err := database.Pool.Exec(context.Background(), `update projects set visibility = 'private' where key = $1`, key); err != nil {
			t.Fatalf("make %s private: %v", key, err)
		}
	}
	bearer := issuer.Mint(t, issuer.signing, issuer.Claims(listedServiceAccount, serviceTokenAudience))
	response := agentRequest(t, handler, http.MethodGet, privateProjectsPath, nil, bearer)
	var keys []string
	for _, project := range decodeBody[privateProjectsAnswer](t, response).Projects {
		keys = append(keys, project.Project)
	}
	if want := []string{"ALPHA", "MID", "ZED"}; response.Code != http.StatusOK || !slices.Equal(keys, want) {
		t.Fatalf("the broker's GET %s = %d, projects %v, want %v: sorted by key", privateProjectsPath, response.Code, keys, want)
	}
}

// TestTheBrokersListAdmitsOnlyTheListedAccount: the broker's list is read only by a service
// account DISPATCH_ACCESS_ACCOUNTS names. Another verified service account, which the deployment
// admits as a caller, and the shared agent token are 403 ACCESS_ACCOUNT_REQUIRED and read none of
// PRIV's channels. The break it catches: the route admitting any verified service account rather
// than the listed one. TestTheAccessAnswerAdmitsOnlyItsAccounts walks the other two K3 routes only,
// and TestTheBrokerListsThePrivateProjectsChannels sends no service account but the listed one.
func TestTheBrokersListAdmitsOnlyTheListedAccount(t *testing.T) {
	t.Parallel()
	issuer := newServiceTokenIssuer(t)
	handler, database, _ := newTestServer(t, testServerOptions{oidc: issuer.verifier(t), accessAccounts: []string{listedServiceAccount}})
	createAccessProject(t, handler, "PRIV", "alice")
	makePrivate(t, database, "PRIV", "C0LISTED001")
	listed := issuer.Mint(t, issuer.signing, issuer.Claims(listedServiceAccount, serviceTokenAudience))
	if response := agentRequest(t, handler, http.MethodGet, privateProjectsPath, nil, listed); response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), "C0LISTED001") {
		t.Fatalf("the listed account %s: GET %s = %d %s, want 200 naming PRIV's channel", listedServiceAccount, privateProjectsPath, response.Code, response.Body.String())
	}
	unlisted := issuer.Mint(t, issuer.signing, issuer.Claims(unlistedServiceAccount, serviceTokenAudience))
	for _, caller := range []struct{ who, token string }{{unlistedServiceAccount, unlisted}, {"the shared agent token", "agent-token"}} {
		response := agentRequest(t, handler, http.MethodGet, privateProjectsPath, nil, caller.token)
		wantRefusal(t, response, http.StatusForbidden, "ACCESS_ACCOUNT_REQUIRED", caller.who, "GET "+privateProjectsPath)
		if strings.Contains(response.Body.String(), "C0LISTED001") {
			t.Fatalf("%s: GET %s read PRIV's channel: %s", caller.who, privateProjectsPath, response.Body.String())
		}
	}
}
