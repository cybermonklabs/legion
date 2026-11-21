// round_token_lists_test.go holds the rows of what a credential's list decides
// (access.CredentialCovers, R3): a full member's list decides
// only which private projects its credential reads and writes, a restricted member's list narrows
// every project, and with no private project a full member's list decides nothing (R9).
package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The three projects of the rows, each owned by alice (brokerTestPerson), a full member: ALPHA, the
// project the tokens list; BETA, a public project no token lists; and GAMMA, private where a row
// makes it so. erin, a restricted member, is named on all three.
const (
	listedAlpha  = "ALPHA"
	publicBeta   = "BETA"
	privateGamma = "GAMMA"
)

// tokenListsRig is the server of the rows, its three projects, and the tokens it minted.
type tokenListsRig struct {
	*brokerTokenRig
	alpha, beta, gamma projectFixture
}

// newTokenListsRig seeds the three projects, names erin on each, and makes GAMMA private when
// private is set. The deployment signs in people from outside its domain (signInOutside), as one
// that holds a restricted member does: erin's tokens act only while that rule admits her
// (restrictedOwnerAdmitted), which the three lists do.
func newTokenListsRig(t *testing.T, private bool) *tokenListsRig {
	t.Helper()
	rig := &tokenListsRig{brokerTokenRig: newBrokerTokenRigOn(t, testServerOptions{signInOutside: true})}
	seedPeople(t, rig.database, accessErin)
	markRestricted(t, rig.database, accessErin)
	rig.alpha = seedProject(t, rig.brokerTokenRig, listedAlpha, "Alpha")
	rig.beta = seedProject(t, rig.brokerTokenRig, publicBeta, "Beta")
	rig.gamma = seedProject(t, rig.brokerTokenRig, privateGamma, "Gamma")
	for _, key := range []string{listedAlpha, publicBeta, privateGamma} {
		if named := rig.asPerson(t, http.MethodPut, "/api/v1/projects/"+key+"/access/people/"+accessErin, nil); named.Code != http.StatusOK {
			t.Fatalf("name erin on %s: status=%d body=%s", key, named.Code, named.Body.String())
		}
	}
	if private {
		makePrivate(t, rig.database, privateGamma)
	}
	return rig
}

// as calls the server as token, a personal token, naming the session it acts for in a write's
// body, as a personal token's write must.
func (rig *tokenListsRig) as(t *testing.T, token, method, path string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	if body != nil {
		withActor := map[string]any{"actor": sessionActor()}
		for field, value := range body {
			withActor[field] = value
		}
		body = withActor
	}
	return agentRequest(t, rig.handler, method, path, body, token)
}

// requireStatus fails, naming what, unless response is status.
func requireStatus(t *testing.T, what string, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Errorf("%s = %d %s, want %d", what, response.Code, strings.TrimSpace(response.Body.String()), status)
	}
}

// listedKeys reports, for each key, whether GET /api/v1/issues as token names it.
func (rig *tokenListsRig) listedKeys(t *testing.T, token string, keys ...string) map[string]bool {
	t.Helper()
	listing := rig.as(t, token, http.MethodGet, "/api/v1/issues", nil)
	if listing.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/issues = %d %s, want 200", listing.Code, listing.Body.String())
	}
	named := map[string]bool{}
	for _, key := range keys {
		named[key] = strings.Contains(listing.Body.String(), `"key":"`+key+`"`)
	}
	return named
}

// hideFromBrokerPerson makes project key, which the broker rig's person made and so owns, private
// and owned by carol instead, with no access list: a project the rig's tokens' person, alice, does
// not see.
func hideFromBrokerPerson(t *testing.T, rig *brokerTokenRig, key string) {
	t.Helper()
	for _, statement := range []string{
		`update projects set owner = 'carol@example.com' where key = $1`,
		`delete from project_access where project_key = $1`,
	} {
		if _, err := rig.database.Pool.Exec(t.Context(), statement, key); err != nil {
			t.Fatalf("give %s to carol: %v", key, err)
		}
	}
	makePrivate(t, rig.database, key)
}

// TestAFullMembersTokenListDecidesOnlyPrivateProjects (R3): alice's token that lists ALPHA, while
// GAMMA is private,
//
//   - reads and writes public BETA, which it does not list: BETA's issue reads 200, a comment on it
//     lands, and the issue listing names BETA's issue;
//   - is answered on private GAMMA, which it does not list, exactly as on an issue nobody holds: the
//     read and the comment answer as NOPE-404 does, the listing names no GAMMA issue, and an issue
//     filed in GAMMA is refused 403 PROJECT_SCOPE in the words a project nobody holds is;
//   - reads and writes GAMMA once alice's browser adds GAMMA to its list, and still reads BETA;
//   - reads a deployment-class read (GET /api/v1/priorities) as the open viewer it is, and is
//     refused a write of the whole deployment 403 PROJECT_SCOPE_DEPLOYMENT naming its list (plan
//     A v86, t-a-19: a list decides reads of private projects, and a write of the whole deployment
//     stays a person's or an unlisted credential's).
func TestAFullMembersTokenListDecidesOnlyPrivateProjects(t *testing.T) {
	rig := newTokenListsRig(t, true)
	token := mintListed(t, rig.handler, brokerTestPerson, "alpha", []string{listedAlpha})
	const nobody = "NOPE-404"
	comment := map[string]any{"body": "A remark."}

	t.Run("reads and writes public BETA", func(t *testing.T) {
		requireStatus(t, "GET BETA's issue", rig.as(t, token.Token, http.MethodGet, "/api/v1/issues/"+rig.beta.issue, nil), http.StatusOK)
		requireStatus(t, "a comment on BETA's issue", rig.as(t, token.Token, http.MethodPost, "/api/v1/issues/"+rig.beta.issue+"/comments", comment), http.StatusCreated)
		if listed := rig.listedKeys(t, token.Token, rig.beta.issue); !listed[rig.beta.issue] {
			t.Errorf("GET /api/v1/issues names no %s, want BETA's issue among them", rig.beta.issue)
		}
	})

	t.Run("answers private GAMMA as missing", func(t *testing.T) {
		requireAnsweredAsMissing(t, "GET GAMMA's issue",
			rig.as(t, token.Token, http.MethodGet, "/api/v1/issues/"+rig.gamma.issue, nil),
			rig.as(t, token.Token, http.MethodGet, "/api/v1/issues/"+nobody, nil), rig.gamma.issue, nobody)
		requireAnsweredAsMissing(t, "a comment on GAMMA's issue",
			rig.as(t, token.Token, http.MethodPost, "/api/v1/issues/"+rig.gamma.issue+"/comments", comment),
			rig.as(t, token.Token, http.MethodPost, "/api/v1/issues/"+nobody+"/comments", comment), rig.gamma.issue, nobody)
		if listed := rig.listedKeys(t, token.Token, rig.gamma.issue); listed[rig.gamma.issue] {
			t.Errorf("GET /api/v1/issues names %s, an issue of GAMMA, which the token does not list", rig.gamma.issue)
		}
		filed := func(project string) *httptest.ResponseRecorder {
			return rig.as(t, token.Token, http.MethodPost, "/api/v1/issues", map[string]any{"project": project, "title": "Elsewhere"})
		}
		requireB7Answer(t, "an issue filed in GAMMA", filed(privateGamma), b7Answer{http.StatusForbidden,
			`{"code":"PROJECT_SCOPE","error":"this request's credential may write project ALPHA; this write belongs to project GAMMA"}`})
		requireAnsweredAsMissing(t, "an issue filed in GAMMA", filed(privateGamma), filed("NOPE"), privateGamma, "NOPE")
	})

	t.Run("reads a deployment read and writes no deployment row", func(t *testing.T) {
		requireStatus(t, "GET /api/v1/priorities", rig.as(t, token.Token, http.MethodGet, "/api/v1/priorities", nil), http.StatusOK)
		requireB7Answer(t, "a write of the deployment", rig.as(t, token.Token, http.MethodPost, "/api/v1/priorities", map[string]any{"title": "A priority"}), b7Answer{http.StatusForbidden,
			`{"code":"PROJECT_SCOPE_DEPLOYMENT","error":"this request's credential may write project ALPHA and nothing else; POST /api/v1/priorities belongs to the deployment rather than to a project"}`})
	})

	t.Run("reads and writes GAMMA once GAMMA is on its list", func(t *testing.T) {
		edited := dispatchRequest(t, rig.handler, http.MethodPatch, "/api/v1/me/agent-tokens/"+token.ID,
			map[string]any{"scopes": model.Scopes, "projects": []string{listedAlpha, privateGamma}}, brokerTestPerson)
		requireStatus(t, "alice's browser adds GAMMA to the token's list", edited, http.StatusOK)
		requireStatus(t, "GET GAMMA's issue", rig.as(t, token.Token, http.MethodGet, "/api/v1/issues/"+rig.gamma.issue, nil), http.StatusOK)
		requireStatus(t, "a comment on GAMMA's issue", rig.as(t, token.Token, http.MethodPost, "/api/v1/issues/"+rig.gamma.issue+"/comments", comment), http.StatusCreated)
		requireStatus(t, "an issue filed in GAMMA", rig.as(t, token.Token, http.MethodPost, "/api/v1/issues", map[string]any{"project": privateGamma, "title": "Inside"}), http.StatusCreated)
		requireStatus(t, "GET BETA's issue", rig.as(t, token.Token, http.MethodGet, "/api/v1/issues/"+rig.beta.issue, nil), http.StatusOK)
		if listed := rig.listedKeys(t, token.Token, rig.beta.issue, rig.gamma.issue); !listed[rig.beta.issue] || !listed[rig.gamma.issue] {
			t.Errorf("GET /api/v1/issues names %v, want both BETA's and GAMMA's issue", listed)
		}
	})
}

// TestARestrictedMembersTokenStaysHeldToItsList (R3, unchanged): erin, a restricted member
// every project names, holds a token that lists ALPHA, which reads ALPHA's issue and answers BETA's
// and GAMMA's as NOPE-404, since a restricted member's list narrows every project; and a token that
// lists nothing, which reads the public projects she sees, ALPHA and BETA, and answers GAMMA's as
// missing, since a private project needs a token that lists it. Each holds in a deployment where
// GAMMA is private, and the first in one where every project is public.
func TestARestrictedMembersTokenStaysHeldToItsList(t *testing.T) {
	for _, private := range []bool{false, true} {
		t.Run(map[bool]string{false: "while every project is public", true: "once GAMMA is private"}[private], func(t *testing.T) {
			rig := newTokenListsRig(t, private)
			listed := mintListed(t, rig.handler, accessErin, "erin-alpha", []string{listedAlpha}).Token
			unlisted := mintListed(t, rig.handler, accessErin, "erin-none", nil).Token
			read := func(token, key string) *httptest.ResponseRecorder {
				return rig.as(t, token, http.MethodGet, "/api/v1/issues/"+key, nil)
			}
			const nobody = "NOPE-404"
			requireStatus(t, "erin's ALPHA token reads ALPHA's issue", read(listed, rig.alpha.issue), http.StatusOK)
			requireAnsweredAsMissing(t, "erin's ALPHA token reads BETA's issue", read(listed, rig.beta.issue), read(listed, nobody), rig.beta.issue, nobody)
			requireAnsweredAsMissing(t, "erin's ALPHA token reads GAMMA's issue", read(listed, rig.gamma.issue), read(listed, nobody), rig.gamma.issue, nobody)
			if !private {
				return
			}
			requireStatus(t, "erin's token that lists nothing reads ALPHA's issue", read(unlisted, rig.alpha.issue), http.StatusOK)
			requireStatus(t, "erin's token that lists nothing reads BETA's issue", read(unlisted, rig.beta.issue), http.StatusOK)
			requireAnsweredAsMissing(t, "erin's token that lists nothing reads GAMMA's issue", read(unlisted, rig.gamma.issue), read(unlisted, nobody), rig.gamma.issue, nobody)
		})
	}
}

// TestWithNoPrivateProjectAFullMembersListDecidesNoRead (R9): while every project is public,
// alice's token that lists ALPHA reads exactly as her token that lists nothing, which reads as
// every token did before lists: BETA's and GAMMA's issues and the deployment's priorities read
// alike, byte for byte, and a comment on BETA's issue lands for both. A write of the whole
// deployment stays an unlisted credential's in every state (plan A's t-a-19):
// the token listing nothing makes a priority, and the token listing ALPHA is refused 403
// PROJECT_SCOPE_DEPLOYMENT naming its list. main holds no personal token that lists a project,
// so that refusal changes no answer main gives.
func TestWithNoPrivateProjectAFullMembersListDecidesNoRead(t *testing.T) {
	rig := newTokenListsRig(t, false)
	listed := mintListed(t, rig.handler, brokerTestPerson, "alpha", []string{listedAlpha}).Token
	unlisted := mintListed(t, rig.handler, brokerTestPerson, "none", nil).Token
	for _, read := range []string{"/api/v1/issues/" + rig.beta.issue, "/api/v1/issues/" + rig.gamma.issue, "/api/v1/priorities"} {
		got, want := rig.as(t, listed, http.MethodGet, read, nil), rig.as(t, unlisted, http.MethodGet, read, nil)
		if got.Code != want.Code || got.Body.String() != want.Body.String() {
			t.Errorf("GET %s under the token listing ALPHA = %d %s, want the token listing nothing's %d %s",
				read, got.Code, strings.TrimSpace(got.Body.String()), want.Code, strings.TrimSpace(want.Body.String()))
		}
	}
	for _, token := range []struct{ name, token string }{{"the token listing ALPHA", listed}, {"the token listing nothing", unlisted}} {
		requireStatus(t, token.name+": a comment on BETA's issue",
			rig.as(t, token.token, http.MethodPost, "/api/v1/issues/"+rig.beta.issue+"/comments", map[string]any{"body": "A remark."}), http.StatusCreated)
	}
	requireStatus(t, "the token listing nothing: a priority",
		rig.as(t, unlisted, http.MethodPost, "/api/v1/priorities", map[string]any{"title": "A priority"}), http.StatusCreated)
	requireB7Answer(t, "the token listing ALPHA: a priority", rig.as(t, listed, http.MethodPost, "/api/v1/priorities", map[string]any{"title": "Another priority"}), b7Answer{http.StatusForbidden,
		`{"code":"PROJECT_SCOPE_DEPLOYMENT","error":"this request's credential may write project ALPHA and nothing else; POST /api/v1/priorities belongs to the deployment rather than to a project"}`})
}

// TestAPersonTokensTradedListHoldsNoDeploymentWriteWhileNoProjectIsPrivate (R9): a broker token
// acting as its person whose trade named BRK makes a priority, a write of the whole deployment,
// while no project is private, as main let it (its list limited no write there), and is refused
// one 403 PROJECT_SCOPE_DEPLOYMENT naming BRK once a project is private, as every credential that
// lists a project then is (t-a-19).
func TestAPersonTokensTradedListHoldsNoDeploymentWriteWhileNoProjectIsPrivate(t *testing.T) {
	for _, private := range []bool{false, true} {
		t.Run(map[bool]string{false: "while no project is private", true: "once a project is private"}[private], func(t *testing.T) {
			rig := newBrokerTokenRig(t)
			caller := newBrokerCaller(t)
			const token = "a-person-token-whose-trade-named-one-project"
			answer := liveNamedToken(caller.jkt)
			answer.ProjectScope, answer.Projects = ciphertoken.ProjectScopePerson, []string{brokerTestProject}
			rig.broker.Says(token, answer)
			seedProject(t, rig, brokerTestProject, "Broker")
			if private {
				seedProject(t, rig, "HIDE", "Hidden")
				makePrivate(t, rig.database, "HIDE")
			}
			wrote := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/priorities", token, map[string]any{"title": "A priority"}))
			if !private {
				requireStatus(t, "a priority", wrote, http.StatusCreated)
				return
			}
			requireB7Answer(t, "a priority", wrote, b7Answer{http.StatusForbidden,
				`{"code":"PROJECT_SCOPE_DEPLOYMENT","error":"this request's credential may write project BRK and nothing else; POST /api/v1/priorities belongs to the deployment rather than to a project"}`})
		})
	}
}

// TestANamedTokensQuestionOverEveryProjectAnswersAsBefore (R9): a broker token an operator's entry
// mapped to BRK, while no project is private, asking a question over every project (no project in
// the body) is refused 403 PROJECT_SCOPE in the words the release before private projects
// answered it (506395beea, going_routes.go's refuseWriteOutsideProjectScope with an empty project),
// and nothing is recorded.
func TestANamedTokensQuestionOverEveryProjectAnswersAsBefore(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-named-token-asking-of-every-project"
	rig.broker.Says(token, liveNamedToken(caller.jkt))
	seedProject(t, rig, brokerTestProject, "Broker")
	requireB7Answer(t, "a question over every project", rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/going/requests", token, map[string]any{
		"words": "what is going on everywhere?",
	})), b7Answer{http.StatusForbidden,
		`{"code":"PROJECT_SCOPE","error":"this request's credential may write project BRK; this write resolves to no project of that credential's"}`})
	if runs := countRows(t, rig.database, `select count(*) from going_requests`); runs != 0 {
		t.Errorf("%d runs were recorded, want none", runs)
	}
}
