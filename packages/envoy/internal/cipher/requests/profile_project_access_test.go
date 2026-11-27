// packages/envoy/internal/cipher/requests/profile_project_access_test.go
// A profile never widens what its source allows: a profile of a private project's secret, served
// under a name of its own, is held to that project wherever the source is — the request, the
// release, the airlock, the hidden ids and the judge — since each reads the project off the
// served Secret.
package requests

import (
	"crypto/ecdsa"
	"errors"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
)

// privateProfiles is the profiles file of these rows: PRIV_READ, a profile of PRIV_KEY; TEAM_READ,
// one of TEAM_TOKEN, both behind the example kind; and SLACK_POST, one of SLACK_TOKEN, behind the
// slack kind. Each is behind the proxy, as a stored value's profile must be, and each source is
// served only through its profile.
var privateProfiles = policytest.KindsFile(
	`{"name": "PRIV_READ", "source": "PRIV_KEY", "proxy": "example"}`,
	`{"name": "TEAM_READ", "source": "TEAM_TOKEN", "proxy": "example"}`,
	`{"name": "SLACK_POST", "source": "SLACK_TOKEN", "proxy": "slack"}`,
)

// newPrivateProfileFixture is newProfiledProjectFixture serving privateProfiles.
func newPrivateProfileFixture(t *testing.T, team projectaccesstest.Row) (m *Machine, enrollmentID string, key *ecdsa.PrivateKey) {
	t.Helper()
	m, enrollmentID, key, _ = newProfiledProjectFixture(t, privateProfiles, team)
	return m, enrollmentID, key
}

// TestAProfileOfAPrivateProjectsSecretGoesOnlyToASessionThatNamesIt: PRIV_READ, a profile of
// PRIV's secret, is denied project-not-named to a session naming no project, as PRIV_KEY is
// (TestARequestForAPrivateSecretFromASessionNamingNoProjectIsDenied); granted and released to the
// same session once it names PRIV; and its grant ends for good at the release after the session
// no longer names PRIV.
func TestAProfileOfAPrivateProjectsSecretGoesOnlyToASessionThatNamesIt(t *testing.T) {
	m, enr, key := newPrivateProfileFixture(t, publicTeam)
	request, err := m.Create(t.Context(), enr, signRequest(t, m, key, "read it", "PRIV_READ"), "")
	deniedFor(t, "a request for PRIV_READ, a profile of PRIV_KEY, from a session naming no project", request, err, "PRIV_READ", policy.ReasonProjectNotNamed)

	nameProjects(t, m, enr, privProject)
	_, grantID := grantOf(t, m, enr, key, "read it", "PRIV_READ")
	if release, err := m.Values(t.Context(), grantID, enr, ReleaseCall{}); err != nil || release.Airlock == nil {
		t.Fatalf("Values(PRIV_READ) for a session naming PRIV = %+v, %v; want it released behind the airlock", release, err)
	}
	nameProjects(t, m, enr)
	if _, err := m.Values(t.Context(), grantID, enr, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("Values(PRIV_READ) once the session names no project = %v; want ErrGrantNotLive", err)
	}
	if by, _ := endedForGood(t, m, grantID); by != GrantEndedActor {
		t.Fatalf("grant %s of PRIV_READ revoked by %q, want %q", grantID, by, GrantEndedActor)
	}
}

// TestTheAirlockRefusesAConnectionOfAProfileOfALostProject: a proxy connection of a grant of
// TEAM_READ, a profile of TEAM's secret, opens while TEAM is public; once TEAM is private and the
// session does not name it, the next connection is refused ErrGrantNotLive and the grant ended, as
// for TEAM_TOKEN itself (TestTheAirlockRefusesAConnectionOfALostProject).
func TestTheAirlockRefusesAConnectionOfAProfileOfALostProject(t *testing.T) {
	m, enr, key := newPrivateProfileFixture(t, publicTeam)
	_, grantID := grantOf(t, m, enr, key, "read it", "TEAM_READ")
	if _, err := m.Values(t.Context(), grantID, enr, ReleaseCall{}); err != nil {
		t.Fatalf("Values: %v", err)
	}
	placeholder := placeholderRows(t, m, grantID)["TEAM_READ/"+SlotValue]
	if placeholder == "" {
		t.Fatalf("grant %s minted no placeholder for TEAM_READ", grantID)
	}
	if _, err := m.AirlockConnection(t.Context(), placeholder); err != nil {
		t.Fatalf("AirlockConnection while TEAM is public: %v", err)
	}
	answerFrom(t, m, dispatchRows(privateTeam(fixtureOperator)))
	if conn, err := m.AirlockConnection(t.Context(), placeholder); !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("the airlock kept grant %s of TEAM_READ open (%+v, %v) after TEAM turned private and the session names no project; want ErrGrantNotLive", grantID, conn, err)
	}
	if by, _ := endedForGood(t, m, grantID); by != GrantEndedActor {
		t.Fatalf("grant %s of TEAM_READ revoked by %q, want %q", grantID, by, GrantEndedActor)
	}
}

// TestAGrantOfAPrivateProjectsProfileIsHiddenFromASessionOutsideIt: the fixture's session, naming
// no project, gets TEAM_READ, a profile of TEAM's secret, while TEAM is public; then TEAM turns
// private, its list naming carol. The grant touches TEAM through its name's project alone, since
// its session names none, so a session of mallory, on no list, asking for its values is answered
// as for a grant id no grant has, ErrGrantNotLive, never ErrNotYours, which would say the grant
// exists; a session of carol that names TEAM is told ErrNotYours.
func TestAGrantOfAPrivateProjectsProfileIsHiddenFromASessionOutsideIt(t *testing.T) {
	m, enr, key := newPrivateProfileFixture(t, publicTeam)
	_, grantID := grantOf(t, m, enr, key, "read it", "TEAM_READ")
	answerFrom(t, m, dispatchRows(privateTeam(insider)))
	outside, _ := newEnrollment(t, m.Store, "session", "box-outside-"+t.Name(), new(outsider), nil)
	if _, err := m.Values(t.Context(), grantID, outside, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) || errors.Is(err, ErrNotYours) {
		t.Fatalf("Values(the TEAM_READ grant) from a session of %s = %v; want ErrGrantNotLive alone", outsider, err)
	}
	inside, _ := newEnrollment(t, m.Store, "session", "box-inside-"+t.Name(), new(insider), nil)
	nameProjects(t, m, inside, teamProject)
	if _, err := m.Values(t.Context(), grantID, inside, ReleaseCall{}); !errors.Is(err, ErrNotYours) {
		t.Fatalf("Values(the TEAM_READ grant) from a session of %s, on TEAM's list and naming it = %v; want ErrNotYours", insider, err)
	}
}

// TestTheJudgeLeavesAPrivateProjectsProfileToAPerson: the model judge decides no private
// project's request, and a request for PRIV_READ is one, though PRIV_READ names no project of its
// own: JudgeProject answers true for it, as for PRIV_KEY, and false for an untagged secret.
func TestTheJudgeLeavesAPrivateProjectsProfileToAPerson(t *testing.T) {
	m, enr, _ := newPrivateProfileFixture(t, publicTeam)
	if private, err := m.JudgeProject(t.Context(), enr, "PRIV_READ", ""); err != nil || !private {
		t.Fatalf("JudgeProject(PRIV_READ) = %v, %v; want true: its source is PRIV's, a private project", private, err)
	}
	if private, err := m.JudgeProject(t.Context(), enr, "SHARED_KEY", ""); err != nil || private {
		t.Fatalf("JudgeProject(SHARED_KEY) = %v, %v; want false", private, err)
	}
}

// TestAProfileOfTheSlackTokenCarriesItsSourcesChannelLimit: SLACK_POST, a profile of the Slack
// token, released behind the airlock to a session naming private PRIV, carries Limited and the
// channels PRIV's owner named, as SLACK_TOKEN does (TestAPrivateProjectsSlackGrantCarriesItsOwnersChannels),
// so the proxy holds each post to them.
func TestAProfileOfTheSlackTokenCarriesItsSourcesChannelLimit(t *testing.T) {
	m, enr, key := newPrivateProfileFixture(t, publicTeam)
	answerFrom(t, m, slackRows(projectaccesstest.Row{SlackChannels: []string{"C0ALLOWED1"}}, publicTeam))
	nameProjects(t, m, enr, privProject)
	_, grantID := grantOf(t, m, enr, key, "post the release notes", "SLACK_POST")
	values, _, err := m.AirlockValues(t.Context(), grantID)
	if err != nil {
		t.Fatalf("AirlockValues(SLACK_POST): %v", err)
	}
	if got := values["SLACK_POST"]; !got.Limited || !slices.Equal(got.Channels, []string{"C0ALLOWED1"}) {
		t.Fatalf("SLACK_POST is released with Limited=%v Channels=%#v; want Limited and [C0ALLOWED1], its source's limit", got.Limited, got.Channels)
	}
}
