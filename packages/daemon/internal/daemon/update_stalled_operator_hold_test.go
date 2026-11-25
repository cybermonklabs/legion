package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// R2 of dispatch://INFRA-3686: the fields are those "the state that the reader acts on that its
// event has", and R3: "A list in a daemon text names each item its reader acts on, such as ... the
// claims an `update-stalled` names". Main's notice of an operator's hold named the claims the
// operator suspended ("An operator suspended tester (suspended). The daemon does not replace the
// pod until the operator resumes each of them.", podupdate.go's operatorHoldReason at #4547's
// 19674f67), which the controller's finding ask passes to the operator. Since #4547 the notice's
// list names every claim with a session that has not failed, a claim the daemon suspended among
// them (noticeClaims). An update-stalled notice of an operator's hold (holds: operator) over a
// tester the operator suspended and an architect an idle release suspended lists both as
// `suspended`, and nothing says which one the operator must resume.
func TestAnOperatorsHoldNamesTheClaimsTheOperatorSuspended(t *testing.T) {
	const round = "pod:round-1"
	opened := time.Date(2026, 11, 21, 6, 0, 0, 0, time.UTC)
	claimOf := func(role claim.Role, state supervise.ClaimState, by supervise.SuspendSource) supervise.Claim {
		return supervise.Claim{Token: claim.Token("legion-legion-legion-2-" + string(role)), Role: role, Issue: "LEGION-2",
			Session: "ses-" + string(role), State: state, SuspendedBy: by, UpdateRound: round, UpdateOpenedAt: &opened}
	}
	group := []supervise.Claim{
		claimOf(claim.RoleImplementer, supervise.StateIdle, ""),
		claimOf(claim.RoleTester, supervise.StateSuspended, supervise.SuspendedByOperator),
		claimOf(claim.RoleArchitect, supervise.StateSuspended, supervise.SuspendedByReleased),
	}
	stalled := updateStalledNotice(updateStall{issue: "LEGION-2", round: round, pod: "pod-1", holds: texts.HoldsOperator,
		waiting: waitingOf(round, noticeClaims(group))})
	notice, err := record.ControllerNoticeOf(stalled)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(notice)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Update struct {
			Waiting []map[string]any `json:"waiting"`
		} `json:"update"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	rows := map[string]string{}
	for _, row := range payload.Update.Waiting {
		role, _ := row["role"].(string)
		delete(row, "claim")
		delete(row, "role")
		facts, _ := json.Marshal(row)
		rows[role] = string(facts)
	}
	if len(rows) != 3 {
		t.Fatalf("the notice lists %v, want the three claims", rows)
	}
	if rows["tester"] == rows["architect"] {
		t.Errorf("the update-stalled notice of an operator's hold lists the tester the operator suspended and the architect the daemon released alike (%s), so nothing names the claim the operator must resume: %s",
			rows["tester"], raw)
	}
}
