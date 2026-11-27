package api

import (
	"maps"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// The fix beside the other steps' rows of this package's test tables (entryTestRows): the entry
// adds no route a plugin sends a body to (requests) and no answer (answers), and the state bodies
// the records are taken from carry its keys and values already (contract_shapes_test.go's
// stateCarryingTheRestOfTheShape and stateCarryingEveryDeclaredValue), so it adds no state body
// (states). It carries into narrowingState an issue whose task runs a fix beside its review,
// LEGION-231, so the entry's band finds the issue's `fix` to take there and writes its own line,
// as every frozen band does (TestANarrowedBodySaysSo).
func init() {
	registerEntryTestRows(entryTestRows{
		entry: FixBesideReviewContract,
		carry: withFixBesideReview,
	})
}

// withFixBesideReview is state with LEGION-231, an issue in review whose task runs a fix: a check
// a CI job runs failed at the task's head, and the fix runs the task's first step again in a
// working copy of its own. The issues are copied before the issue is added, so the state passed
// in is left as it was.
func withFixBesideReview(state State) State {
	issues := make(map[string]Issue, len(state.Issues)+1)
	maps.Copy(issues, state.Issues)
	issues["LEGION-231"] = Issue{Key: "LEGION-231", Generation: 1, Phase: phase.Reviewing, Status: "needs_review",
		Tree: "LEGION-231", Repo: "acme/widgets", DefaultBranch: "main", Workers: map[claim.Role]PhaseView{},
		Fix: &FixView{Task: "a", Step: 1, Check: "unit", Head: "c0de0000000000000000000000000000000000ff",
			Dir: "/var/lib/legion/workspaces/acme/widgets/LEGION-231-fix", Entry: 2, GivenEntry: 2}}
	state.Issues = issues
	return state
}
