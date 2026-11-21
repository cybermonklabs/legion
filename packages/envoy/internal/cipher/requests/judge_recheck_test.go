package requests

import (
	"errors"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/record"
)

// judgeRows is a Dispatch holding TEAM public and PRIV private.
func judgeRows(team projectaccesstest.Row) map[string]projectaccesstest.Row {
	return map[string]projectaccesstest.Row{
		teamProject: team,
		privProject: {Visibility: "private", Owner: otherPerson},
	}
}

// TestTheJudgesDecisionChecksTheProjectRuleAgain is round 3's first hardening item on
// cybermonklabs/legion#4135, with round 4's m1. The judge's run checks the project rule before it
// asks the model (JudgeProject) and then spends up to twenty seconds on the model call, in which
// the project an issue of the reason names can turn private, or the request can be adopted by an
// enrollment that names a private project. The decision asks Dispatch again before its
// transaction — afresh, past the client's cache, which the run's own check filled seconds before
// (projectaccess.Fresh) — so the judge's approval of a request that is a private project's by then
// is refused, neither granted nor denied, and the request waits for its person. The client here is
// the run's own, warmed by its check, so the change is one Dispatch made inside CacheTTL. A request
// still of no private project is granted, and so is any request on a broker with no project-access
// client, as at the merge base.
func TestTheJudgesDecisionChecksTheProjectRuleAgain(t *testing.T) {
	const reason = "TEAM-7: rotate the deploy key"
	for _, c := range []struct {
		name string
		turn func(t *testing.T, m *Machine, enrollmentID string, dispatch *projectaccesstest.Server)
	}{
		{"the reason's project turned private", func(t *testing.T, _ *Machine, _ string, dispatch *projectaccesstest.Server) {
			dispatch.SetRow(teamProject, privateTeam())
		}},
		{"the enrollment now names a private project", func(t *testing.T, m *Machine, enrollmentID string, _ *projectaccesstest.Server) {
			nameProjects(t, m, enrollmentID, privProject)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, enr, key := newJudgeFixture(t)
			dispatch := answerFrom(t, m, judgeRows(publicTeam))
			req := pendingJudgeable(t, m, enr, key, reason, 600, "DEEL_API_KEY")
			if private, err := m.JudgeProject(t.Context(), enr, "DEEL_API_KEY", reason); err != nil || private {
				t.Fatalf("JudgeProject before the change = %v, %v; want false: the run's own check passed", private, err)
			}
			c.turn(t, m, enr, dispatch)

			dec, err := m.ApplyJudgeDecision(t.Context(), *req.RecordID, judgeModel, map[string]any{"verdict": "approve"})
			if !errors.Is(err, record.ErrNotApprover) {
				t.Fatalf("the judge's approval once %s = %+v, %v; want record.ErrNotApprover", c.name, dec, err)
			}
			got := readDecided(t, m, *req.RecordID)
			if got.events != 0 || got.state != "pending" || got.decidedBy != "" || len(got.approvers) != 0 {
				t.Fatalf("after the refusal = %+v; want the request untouched and pending", got)
			}
			if judged := readJudged(t, m, *req.RecordID); len(judged) != 0 {
				t.Errorf("request.judged rows = %+v; want none: the transaction wrote nothing", judged)
			}
		})
	}

	for _, c := range []struct {
		name   string
		client bool
	}{
		{"nothing turned private", true},
		{"a broker with no project-access client", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, enr, key := newJudgeFixture(t)
			answerFrom(t, m, judgeRows(publicTeam))
			if !c.client {
				m.Projects = nil
			}
			req := pendingJudgeable(t, m, enr, key, reason, 600, "DEEL_API_KEY")
			dec, err := m.ApplyJudgeDecision(t.Context(), *req.RecordID, judgeModel, map[string]any{"verdict": "approve"})
			if err != nil || dec.State != "granted" || dec.GrantID == "" {
				t.Fatalf("the judge's approval, %s = %+v, %v; want it granted, as at the merge base", c.name, dec, err)
			}
		})
	}
}
