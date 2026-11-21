// hidden_grant_test.go holds RevokeByApprover's person rule: a grant of a private project, by a
// project tag of a name it releases, by a project a credential its request has had names, or by
// an issue key its reason names, is ended by someone it does not name only where Dispatch says
// that person may see the project, the people GrantsForApprover lists it to. Anyone else is
// answered pgx.ErrNoRows, the answer a grant id no grant has, and nothing is revoked; while
// Dispatch cannot answer, nothing is revoked either. The grant's approver and its enrollment's
// operator end it whatever Dispatch answers.
package requests

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/record"
)

// requireGrantLive fails t unless grant grantID has not been revoked.
func requireGrantLive(t *testing.T, m *Machine, grantID string) {
	t.Helper()
	var revoked *time.Time
	if err := m.Store.Pool.QueryRow(t.Context(), `select revoked_at from grants where id=$1`, grantID).Scan(&revoked); err != nil {
		t.Fatalf("read grant %s: %v", grantID, err)
	}
	if revoked != nil {
		t.Fatalf("grant %s was revoked at %s; want it live", grantID, revoked)
	}
}

// requireRevokedBy fails t unless grant grantID was revoked by login's revoke.
func requireRevokedBy(t *testing.T, m *Machine, grantID, login string) {
	t.Helper()
	var revokedBy *string
	if err := m.Store.Pool.QueryRow(t.Context(), `select revoked_by from grants where id=$1`, grantID).Scan(&revokedBy); err != nil {
		t.Fatalf("read grant %s: %v", grantID, err)
	}
	if revokedBy == nil || *revokedBy != record.HumanActor(login) {
		t.Fatalf("grant %s revoked_by = %v; want %s", grantID, revokedBy, record.HumanActor(login))
	}
}

// requireRevokedAsUnknown fails t unless login's revoke of grantID answers what the same revoke
// of a grant id no grant has answers, pgx.ErrNoRows and nothing beside it, and leaves the grant
// live. A refusal that wrapped pgx.ErrNoRows with words of its own would still pass errors.Is,
// so the two answers are compared whole.
func requireRevokedAsUnknown(t *testing.T, m *Machine, grantID, login, what string) {
	t.Helper()
	err := m.RevokeByApprover(t.Context(), grantID, login, record.HumanActor(login))
	unknown := m.RevokeByApprover(t.Context(), uuid.NewString(), login, record.HumanActor(login))
	if !errors.Is(err, pgx.ErrNoRows) || errors.Is(err, ErrNotApprover) || fmt.Sprint(err) != fmt.Sprint(unknown) {
		t.Fatalf("RevokeByApprover(%s, %s) = %v; want %v alone, the answer of a grant id no grant has", what, login, err, unknown)
	}
	requireGrantLive(t, m, grantID)
}

// TestAnOutsiderCannotRevokeAJudgedGrantOfAProjectTurnedPrivate: the model judge approves
// SHARED_KEY on a service's pod in anyone's stead, so the grant's approver is
// record.AnyoneApprover, which mayRevoke lets every signed-in person end. Its reason names
// INFRA-1353, and INFRA then turns private with carol alone on its list. mallory, outside INFRA
// and named nowhere on the grant, is answered pgx.ErrNoRows, as for a grant id no grant has, the
// grant stays live, and her list does not hold it; while Dispatch cannot answer, her revoke is
// ErrProjectAccessUnavailable and the grant stays live; carol, on INFRA's list, ends it.
func TestAnOutsiderCannotRevokeAJudgedGrantOfAProjectTurnedPrivate(t *testing.T) {
	m, _, _ := newJudgeFixture(t)
	pod, podKey := newServiceEnrollment(t, m.Store, fixtureService, fixtureSubject, "pod-"+t.Name())
	grantID := judgedGrant(t, m, pod, podKey, 600, "SHARED_KEY")
	infra := map[string]projectaccesstest.Row{"INFRA": {Visibility: "private", Owner: otherPerson, Listed: []string{insider}}}
	answerFrom(t, m, infra)

	t.Run("an outsider is answered as for a grant id no grant has", func(t *testing.T) {
		requireRevokedAsUnknown(t, m, grantID, outsider, "the judged grant of INFRA")
		listed, err := m.GrantsForApprover(t.Context(), outsider)
		if err != nil || slices.ContainsFunc(listed, func(g ApproverGrant) bool { return g.GrantID == grantID }) {
			t.Fatalf("GrantsForApprover(%s) = %+v, %v; want it answered, without the grant", outsider, listed, err)
		}
	})

	t.Run("while Dispatch cannot answer, an outsider ends nothing", func(t *testing.T) {
		down := answerFrom(t, m, infra)
		down.SetDown(true)
		if err := m.RevokeByApprover(t.Context(), grantID, outsider, record.HumanActor(outsider)); !errors.Is(err, ErrProjectAccessUnavailable) {
			t.Fatalf("RevokeByApprover(the judged grant, %s) while Dispatch is down = %v; want ErrProjectAccessUnavailable", outsider, err)
		}
		requireGrantLive(t, m, grantID)
		if down.Calls() == 0 {
			t.Fatal("the down Dispatch was never asked: the row above is blind")
		}
	})

	answerFrom(t, m, infra)
	t.Run("a person on INFRA's list ends it", func(t *testing.T) {
		if err := m.RevokeByApprover(t.Context(), grantID, insider, record.HumanActor(insider)); err != nil {
			t.Fatalf("RevokeByApprover(the judged grant, %s, on INFRA's list) = %v; want it ended", insider, err)
		}
		var revoked *time.Time
		if err := m.Store.Pool.QueryRow(t.Context(), `select revoked_at from grants where id=$1`, grantID).Scan(&revoked); err != nil || revoked == nil {
			t.Fatalf("the judged grant after %s's revoke: revoked_at %v, %v; want it revoked", insider, revoked, err)
		}
	})
}

// TestAnOutsiderCannotRevokeAPersonsGrantOfAPrivateProject: a grant carol, on PRIV's list,
// approved is refused to mallory, outside PRIV, as a grant id no grant has, not with
// ErrNotApprover, whichever part of the grant makes it PRIV's: its secret's tag, the projects of
// the credential that asked, or those of the credential it hangs off after an adoption moved it.
func TestAnOutsiderCannotRevokeAPersonsGrantOfAPrivateProject(t *testing.T) {
	m, enr, key, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
	nameProjects(t, m, enr, privProject)
	_, tagged := approvedGrant(t, m, testAudience, enr, key, insider, "PRIV_HUMAN")
	requireRevokedAsUnknown(t, m, tagged, outsider, "PRIV_HUMAN's grant")

	for _, c := range []struct {
		name          string
		asked, adopts []string
	}{
		{"the credential that asked names PRIV", []string{privProject}, []string{}},
		{"the credential it hangs off now names PRIV", []string{}, []string{privProject}},
	} {
		t.Run(c.name, func(t *testing.T) {
			asking, askingKey := newEnrollment(t, m.Store, "session", "box-a-"+t.Name(), new(fixtureOperator), nil)
			nameProjects(t, m, asking, c.asked...)
			_, grantID := approvedGrant(t, m, testAudience, asking, askingKey, insider, "SHARED_KEY")
			adopter, _ := newEnrollment(t, m.Store, "session", "box-b-"+t.Name(), new(fixtureOperator), nil)
			nameProjects(t, m, adopter, c.adopts...)
			// An adoption moves the session's grant and its request to the enrollment of the box the
			// session runs on now (enroll.adopt); the record keeps the enrollment that asked.
			if _, err := m.Store.Pool.Exec(t.Context(), `with moved as (update grants set enrollment_id=$2 where id=$1 returning request_id)
				update requests set enrollment_id=$2 where id=(select request_id from moved)`, grantID, adopter); err != nil {
				t.Fatalf("move the grant to the adopting enrollment: %v", err)
			}
			requireRevokedAsUnknown(t, m, grantID, outsider, "SHARED_KEY's grant")
			if err := m.RevokeByApprover(t.Context(), grantID, insider, record.HumanActor(insider)); err != nil {
				t.Fatalf("RevokeByApprover(SHARED_KEY's grant, %s, on PRIV's list) = %v; want it ended", insider, err)
			}
		})
	}
}

// TestAGrantsApproverAndOperatorRevokeItWhateverDispatchAnswers is must change 1 of round 3 on
// cybermonklabs/legion#4135. Four grants of PRIV_HUMAN, each on a session of the fixture's
// operator naming PRIV, approved by carol. Each grant row names carol as its approver and the
// operator as its enrollment's, so both already know it exists, and a revoke only takes access
// away: the person rule is not asked for either (hideUnseenGrant), as the session rule is not
// asked for the session that holds a row. While Dispatch cannot answer, carol ends the first and
// the operator the second, and Dispatch is asked nothing; once PRIV's list names neither of them,
// carol ends the third and the operator the fourth. Before, the first two were answered
// ErrProjectAccessUnavailable and the last two pgx.ErrNoRows, and none was revoked.
func TestAGrantsApproverAndOperatorRevokeItWhateverDispatchAnswers(t *testing.T) {
	m, _, _, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
	grants := map[string]string{}
	for _, box := range []string{"approver-down", "operator-down", "approver-left", "operator-left"} {
		session, sessionKey := newEnrollment(t, m.Store, "session", "box-"+box+"-"+t.Name(), new(fixtureOperator), nil)
		nameProjects(t, m, session, privProject)
		_, grants[box] = approvedGrant(t, m, testAudience, session, sessionKey, insider, "PRIV_HUMAN")
	}
	ends := func(t *testing.T, grantID, login string) {
		t.Helper()
		if err := m.RevokeByApprover(t.Context(), grantID, login, record.HumanActor(login)); err != nil {
			t.Fatalf("RevokeByApprover(%s, %s) = %v; want it ended", grantID, login, err)
		}
		requireRevokedBy(t, m, grantID, login)
	}

	down := answerFrom(t, m, dispatchRows(publicTeam))
	down.SetDown(true)
	t.Run("its approver while Dispatch is down", func(t *testing.T) { ends(t, grants["approver-down"], insider) })
	t.Run("its operator while Dispatch is down", func(t *testing.T) { ends(t, grants["operator-down"], fixtureOperator) })
	if calls := down.Calls(); calls != 0 {
		t.Errorf("the revokes by the grants' approver and operator asked the down Dispatch %d times; want none", calls)
	}

	answerFrom(t, m, map[string]projectaccesstest.Row{
		privProject: {Visibility: "private", Owner: otherPerson},
		teamProject: publicTeam,
	})
	t.Run("its approver once PRIV's list no longer names them", func(t *testing.T) { ends(t, grants["approver-left"], insider) })
	t.Run("its operator once PRIV's list no longer names them", func(t *testing.T) { ends(t, grants["operator-left"], fixtureOperator) })
}

// TestAnUntaggedGrantStaysWithItsOperatorOnceTheyLeaveItsProject is finding 1 of the TN deep
// review of cybermonklabs/legion#4135 at b358cd4cdc. The fixture's operator's session names PRIV
// and gets AUTO_TOKEN, their own agent-tier secret, which carries no project tag, so no release of
// it asks Dispatch anything again (checkRelease): once PRIV's list no longer names the operator,
// the grant goes on releasing. The operator, whom the grant names as its enrollment's, still finds
// it on their own list (GrantsForApprover) and ends it, and its next release is ErrGrantNotLive.
// mallory, whom the grant names nowhere and PRIV does not list, is answered as for a grant id no
// grant has and does not list it. Before, the operator's list left the grant out and their revoke
// was answered pgx.ErrNoRows, so nobody but its own session could end it.
func TestAnUntaggedGrantStaysWithItsOperatorOnceTheyLeaveItsProject(t *testing.T) {
	m, enr, key, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
	nameProjects(t, m, enr, privProject)
	_, grantID := grantOf(t, m, enr, key, "deploy the worker", "AUTO_TOKEN")
	answerFrom(t, m, map[string]projectaccesstest.Row{
		privProject: {Visibility: "private", Owner: otherPerson, Listed: []string{insider}},
		teamProject: publicTeam,
	})
	if released, err := m.Values(t.Context(), grantID, enr, ReleaseCall{}); err != nil || released.Values["AUTO_TOKEN"] == "" {
		t.Fatalf("Values(the AUTO_TOKEN grant) once PRIV's list no longer names %s = %+v, %v; want it released: the grant lives on", fixtureOperator, released.Values, err)
	}
	listed := func(login string) bool {
		t.Helper()
		grants, err := m.GrantsForApprover(t.Context(), login)
		if err != nil {
			t.Fatalf("GrantsForApprover(%s): %v", login, err)
		}
		return slices.ContainsFunc(grants, func(g ApproverGrant) bool { return g.GrantID == grantID })
	}

	t.Run("an outsider neither lists nor revokes it", func(t *testing.T) {
		if listed(outsider) {
			t.Errorf("GrantsForApprover(%s) holds the AUTO_TOKEN grant; want it left out", outsider)
		}
		requireRevokedAsUnknown(t, m, grantID, outsider, "the AUTO_TOKEN grant")
	})
	t.Run("its operator lists it", func(t *testing.T) {
		if !listed(fixtureOperator) {
			t.Errorf("GrantsForApprover(%s) lacks the AUTO_TOKEN grant of their own session; want it listed", fixtureOperator)
		}
	})
	t.Run("its operator revokes it, and its next release is refused", func(t *testing.T) {
		if err := m.RevokeByApprover(t.Context(), grantID, fixtureOperator, record.HumanActor(fixtureOperator)); err != nil {
			t.Fatalf("RevokeByApprover(the AUTO_TOKEN grant, %s, its operator) = %v; want it ended", fixtureOperator, err)
		}
		if released, err := m.Values(t.Context(), grantID, enr, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) {
			t.Fatalf("Values(the AUTO_TOKEN grant) after its operator's revoke = %+v, %v; want ErrGrantNotLive", released.Values, err)
		}
	})
}
