package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/ciphertoken"
)

// TestARestrictedMembersBrokerTokenSeesOnlyItsListWhateverTheDeploymentHolds (R3): a broker
// token's list holds its reads whenever its person is a restricted member, as it does for every
// person once a project is private; only a full member's token reads what its person reads while
// every project is public (R9, TestABrokerTokenScopedToOneProjectAnswersAsBeforeWhileNoProjectIsPrivate).
// alice is a restricted member who sees two public projects, BRK and OTHER (she owns both). Her
// token an operator's entry mapped to BRK, and her token acting as herself whose trade named BRK,
// each read BRK's issue, read OTHER's issue exactly as an issue nobody holds, and are answered by
// whoami with the list BRK: in a deployment where every project is public, and the same once a
// third project is private. The deployment signs in people from outside its domain, as one that
// holds a restricted member does: with outside sign-in off a restricted member's broker token acts
// for nobody (TestARestrictedOwnersBrokerTokenActsOnlyWhileOutsideSignInAdmitsThem).
//
// Controls, in VisibleProjectsSQL's listed column: without its restricted-member arm (the rule at
// 4421eb110d) the all-public case reads OTHER's issue and whoami names no list; with a broker
// token's list holding no read at all, both cases do; with the list held for every person, the
// full member's R9 row (TestABrokerTokenScopedToOneProjectAnswersAsBeforeWhileNoProjectIsPrivate)
// fails and this one passes.
func TestARestrictedMembersBrokerTokenSeesOnlyItsListWhateverTheDeploymentHolds(t *testing.T) {
	t.Parallel()
	for _, private := range []bool{false, true} {
		t.Run(map[bool]string{false: "while every project is public", true: "once a project is private"}[private], func(t *testing.T) {
			rig := newBrokerTokenRigOn(t, testServerOptions{admitsRestricted: true})
			caller := newBrokerCaller(t)
			const named, person = "a-restricted-members-token-of-one-project", "a-restricted-members-person-token-of-one-project"
			rig.broker.Says(named, liveNamedToken(caller.jkt))
			answer := liveNamedToken(caller.jkt)
			answer.ProjectScope, answer.Projects = ciphertoken.ProjectScopePerson, []string{brokerTestProject}
			rig.broker.Says(person, answer)
			own := seedProject(t, rig, brokerTestProject, "Broker")
			other := seedProject(t, rig, "OTHER", "Other")
			if private {
				seedProject(t, rig, "HIDE", "Hidden")
				makePrivate(t, rig.database, "HIDE")
			}
			seedPeople(t, rig.database, brokerTestPerson)
			markRestricted(t, rig.database, brokerTestPerson)
			const nobody = "NOPE-404"
			as := func(token, method, path string) *httptest.ResponseRecorder {
				t.Helper()
				return rig.serve(caller.presenting(t, method, path, token, nil))
			}

			for _, token := range []string{named, person} {
				if read := as(token, http.MethodGet, "/api/v1/issues/"+own.issue); read.Code != http.StatusOK {
					t.Errorf("%s: GET its own project's issue = %d %s, want 200", token, read.Code, read.Body.String())
				}
				requireAnsweredAsMissing(t, token+": GET the issue of another project its person sees",
					as(token, http.MethodGet, "/api/v1/issues/"+other.issue), as(token, http.MethodGet, "/api/v1/issues/"+nobody), other.issue, nobody)
				whoami := as(token, http.MethodGet, "/api/v1/whoami")
				var answered struct {
					Projects []string `json:"projects"`
				}
				if err := json.Unmarshal(whoami.Body.Bytes(), &answered); whoami.Code != http.StatusOK || err != nil ||
					!slices.Equal(answered.Projects, []string{brokerTestProject}) {
					t.Errorf("%s: GET /api/v1/whoami = %d %s, want 200 naming projects [%s]", token, whoami.Code, whoami.Body.String(), brokerTestProject)
				}
			}
		})
	}
}
