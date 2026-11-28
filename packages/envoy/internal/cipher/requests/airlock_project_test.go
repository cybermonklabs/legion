// packages/envoy/internal/cipher/requests/airlock_project_test.go
// c-private-secrets' second run: the airlock is a release path of its own, and a private
// project's secret behind it goes only to a session that still names the project.
package requests

import (
	"errors"
	"testing"
	"time"
)

// TestTheAirlockRefusesAConnectionOfALostProject: a proxy connection with the placeholder of a
// grant of TEAM's secret opens while TEAM is public; once TEAM is private and the session's
// enrollment does not name it, the next connection is refused ErrGrantNotLive and the grant is
// ended for good.
func TestTheAirlockRefusesAConnectionOfALostProject(t *testing.T) {
	t.Parallel()
	m, enr, key, _ := newProjectFixture(t, bound(rule("TEAM_TOKEN", "example")), publicTeam)
	_, grantID := grantOf(t, m, enr, key, "read it", "TEAM_TOKEN")
	if _, err := m.Values(t.Context(), grantID, enr, ReleaseCall{}); err != nil {
		t.Fatalf("Values: %v", err)
	}
	placeholder := placeholderRows(t, m, grantID)["TEAM_TOKEN/"+SlotValue]
	if placeholder == "" {
		t.Fatalf("grant %s minted no placeholder for TEAM_TOKEN", grantID)
	}
	if _, err := m.AirlockConnection(t.Context(), placeholder); err != nil {
		t.Fatalf("AirlockConnection while TEAM is public: %v", err)
	}
	answerFrom(t, m, dispatchRows(privateTeam(fixtureOperator)))
	if conn, err := m.AirlockConnection(t.Context(), placeholder); !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("the airlock kept grant %s of TEAM_TOKEN open (%+v, %v) after TEAM turned private and the session names no project; want ErrGrantNotLive", grantID, conn, err)
	}
	if by, _ := endedForGood(t, m, grantID); by != GrantEndedActor {
		t.Fatalf("the airlock refused grant %s's connection but left the grant live (revoked by %q); want it ended by %q", grantID, by, GrantEndedActor)
	}
}

// TestTheAirlockStopsServingAPrivateSecretAfterAccessIsLost: AirlockValues of a grant of private
// TEAM's secret, for a session naming TEAM, answers each value's Expires no later than the read
// time plus airlockProjectRecheck, so a pod holding it runs the release checks again within that
// bound; once the session's person leaves TEAM's list, the next AirlockValues ends the grant.
func TestTheAirlockStopsServingAPrivateSecretAfterAccessIsLost(t *testing.T) {
	t.Parallel()
	m, enr, key, _ := newProjectFixture(t, bound(rule("TEAM_TOKEN", "example")), privateTeam(fixtureOperator))
	nameProjects(t, m, enr, teamProject)
	_, grantID := grantOf(t, m, enr, key, "read it", "TEAM_TOKEN")
	if _, err := m.Values(t.Context(), grantID, enr, ReleaseCall{}); err != nil {
		t.Fatalf("Values: %v", err)
	}
	read := time.Now()
	values, _, err := m.AirlockValues(t.Context(), grantID)
	if err != nil {
		t.Fatalf("AirlockValues of grant %s: %v", grantID, err)
	}
	held, ok := values["TEAM_TOKEN"]
	if !ok || held.Slots[SlotValue] != "team-v1" {
		t.Fatalf("AirlockValues of grant %s = %+v; want TEAM_TOKEN's value", grantID, values)
	}
	if held.Expires.IsZero() || held.Expires.After(time.Now().Add(airlockProjectRecheck)) || held.Expires.Before(read.Add(airlockProjectRecheck-time.Minute)) {
		t.Fatalf("grant %s's TEAM_TOKEN expires %v; want about %v, the read plus %v, so a pod holding it checks again", grantID, held.Expires, read.Add(airlockProjectRecheck), airlockProjectRecheck)
	}
	answerFrom(t, m, dispatchRows(privateTeam()))
	if values, _, err := m.AirlockValues(t.Context(), grantID); !errors.Is(err, ErrGrantNotLive) || values != nil {
		t.Fatalf("AirlockValues of grant %s after its person left TEAM's list = %v, %v; want ErrGrantNotLive and no value", grantID, values, err)
	}
	if by, _ := endedForGood(t, m, grantID); by != GrantEndedActor {
		t.Fatalf("grant %s revoked by %q, want %q", grantID, by, GrantEndedActor)
	}
}

// TestTheAirlockOfAnUntaggedGrantKeepsItsValues: the same steps for an untagged secret keep
// serving, leave a stored value's Expires zero, and ask Dispatch nothing of a project; what they
// ask is the session's person's member kind (restrictedOperator).
func TestTheAirlockOfAnUntaggedGrantKeepsItsValues(t *testing.T) {
	t.Parallel()
	m, enr, key, _ := newProjectFixture(t, bound(rule("AUTO_TOKEN", "example")), publicTeam)
	dispatch := answerFrom(t, m, dispatchRows(publicTeam))
	_, grantID := grantOf(t, m, enr, key, "read it", "AUTO_TOKEN")
	if _, err := m.Values(t.Context(), grantID, enr, ReleaseCall{}); err != nil {
		t.Fatalf("Values: %v", err)
	}
	placeholder := placeholderRows(t, m, grantID)["AUTO_TOKEN/"+SlotValue]
	for range 2 {
		if _, err := m.AirlockConnection(t.Context(), placeholder); err != nil {
			t.Fatalf("AirlockConnection of untagged grant %s: %v", grantID, err)
		}
		values, _, err := m.AirlockValues(t.Context(), grantID)
		if err != nil || values["AUTO_TOKEN"].Slots[SlotValue] != "auto-v1" || !values["AUTO_TOKEN"].Expires.IsZero() {
			t.Fatalf("AirlockValues of untagged grant %s = %+v, %v; want its value with no expiry", grantID, values, err)
		}
		answerFrom(t, m, dispatchRows(privateTeam()))
	}
	if calls := dispatch.ProjectCalls(); calls != 0 {
		t.Fatalf("the airlock of untagged grant %s asked Dispatch's project route %d times; want none", grantID, calls)
	}
}
