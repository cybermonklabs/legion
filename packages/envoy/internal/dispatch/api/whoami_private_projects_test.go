package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestWhoamiAnswersAListedCredentialsProjectsAndWhichArePrivate (plan t-a-16, KF4; c-a-private):
// GET /api/v1/whoami answers a credential that lists projects, a person's dsp_ token among them,
// in the agent shape with its owner, its projects, sorted, and private_projects, the keys of that
// list the caller must treat as private: a private project, and one the credential no longer sees
// whatever its visibility. A dsp_ token that lists nothing answers exactly the bytes it answered
// before the list existed (R9). The deployment signs in people from outside its domain, the rule a
// restricted member's token is held to (signInOutside), so bob's token acts while a project's
// list names him: PUB's does throughout, and his token lists PRIV alone.
//
// Controls, each run on its own: personalTokenActor without its read of the token's projects, and
// carol's row answers no projects; private_projects computed from visibility alone, and bob's row
// answers no private_projects.
func TestWhoamiAnswersAListedCredentialsProjectsAndWhichArePrivate(t *testing.T) {
	seed := seedPrivateProjectWith(t, testServerOptions{signInOutside: true})
	whoami := func(who, token string) (string, map[string]json.RawMessage) {
		t.Helper()
		response := agentRequest(t, seed.handler, http.MethodGet, "/api/v1/whoami", nil, token)
		if response.Code != http.StatusOK {
			t.Fatalf("%s: GET /api/v1/whoami = %d %s", who, response.Code, response.Body.String())
		}
		return strings.TrimSpace(response.Body.String()), decodeBody[map[string]json.RawMessage](t, response)
	}
	field := func(answer map[string]json.RawMessage, name string, into any) bool {
		t.Helper()
		raw, present := answer[name]
		if !present {
			return false
		}
		if err := json.Unmarshal(raw, into); err != nil {
			t.Fatalf("%s is %s, which does not decode: %v", name, raw, err)
		}
		return true
	}
	lists := func(who string, raw string, answer map[string]json.RawMessage, projects, private []string) {
		t.Helper()
		var got []string
		if !field(answer, "projects", &got) || !slices.Equal(got, projects) {
			t.Errorf("%s: projects = %q, want %q: the answer was %s", who, got, projects, raw)
		}
		got = nil
		if !field(answer, "private_projects", &got) || !slices.Equal(got, private) {
			t.Errorf("%s: private_projects = %q, want %q: the answer was %s", who, got, private, raw)
		}
	}

	// carol, on PRIV's list, mints a dsp_ token listing the two keys out of order.
	const carolsToken = "carol's token listing PUB and PRIV"
	carols := mintListed(t, seed.handler, seedCarol, "carol-both", []string{seedPublic, seedPrivate})
	raw, answer := whoami(carolsToken, carols.Token)
	var kind, owner string
	if !field(answer, "kind", &kind) || kind != "agent" || !field(answer, "owner", &owner) || owner != seedCarol {
		t.Errorf("%s: kind %q owner %q, want the agent shape with owner %s: %s", carolsToken, kind, owner, seedCarol, raw)
	}
	lists(carolsToken, raw, answer, []string{seedPrivate, seedPublic}, []string{seedPrivate})

	// bob, restricted and on PRIV's list, mints a token listing PRIV. PRIV then turns public and
	// bob is taken off its list, so bob no longer sees PRIV: his token's PRIV answers private.
	// carol names him on PUB's list first, so a project still names him and his token acts.
	const bobsToken = "bob's token listing PRIV"
	seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/projects/"+seedPublic+"/access/people/"+seedBob, nil, http.StatusOK)
	bobs := mintListed(t, seed.handler, seedBob, "bob-priv", []string{seedPrivate})
	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	seed.mustRequest(t, callerCarol, http.MethodDelete, "/api/v1/projects/"+seedPrivate+"/access/people/"+seedBob, nil, http.StatusOK)
	raw, answer = whoami(bobsToken, bobs.Token)
	lists(bobsToken, raw, answer, []string{seedPrivate}, []string{seedPrivate})

	// R9: alice's dsp_ token with no list answers the bytes the answer had before the list.
	alices := mintListed(t, seed.handler, seedAlice, "alice-none", nil)
	const before = `{"kind":"agent","owner":"alice","project":null,"scopes":["person","approve"],"service":null,"session":null}`
	if raw, _ := whoami("alice's token that lists nothing", alices.Token); raw != before {
		t.Errorf("alice's token that lists nothing: GET /api/v1/whoami = %s, want %s", raw, before)
	}
}
