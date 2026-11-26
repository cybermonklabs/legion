package daemon

// design_news_holders.go is how the daemon reads the one rule for who holds a member's news of the
// approved design (record.NewsClaims.HolderOf, record.NewsClaimTakes): how that holder is told
// (holderTold), and why a news-only start reaches no claim as its machine holds it
// (newsOnlyRefusal).

import (
	"fmt"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// holderTold is how the holder of a member owed news is told, from its claim, a phase worker's or
// that of the child's architect that owns a member in accepting (readMemberClaims): by a task now
// when the reconcile queues a news-only start for the claim, which is when one reaches it
// (record.RecordedClaimTakesNews, the one rule the reconcile queues that start under and the start
// runs under) and it holds no task its worker has not taken yet (record.HoldsUntakenTask, the
// reconcile's other guard). A claim the start reaches that holds such a task gets no start until a
// turn takes it, so its news waits for that turn (workflow.NewsWaitsForTask). Otherwise by the next
// start of its phase: a claim behind its phase's current entry, owed that entry's start, no claim
// at all, or one that retired, failed, or an operator suspended, since the start that revives it
// carries the news.
func holderTold(run record.RoleRun, entry int64) workflow.NewsState {
	switch {
	case !record.RecordedClaimTakesNews(run.Claim, entry):
		return workflow.NewsAtNextStart
	case record.HoldsUntakenTask(run.Claim):
		return workflow.NewsWaitsForTask
	}
	return workflow.NewsSentNow
}

// newsOnlyRefusal is why a news-only start reaches no claim, as the claim's machine holds it when
// the start runs, and "" when it may: the guards the reconcile queued it under, read again
// (record.NewsClaimTakes), since the claim can have moved since. entry is the entry the role's
// phases row names, 0 where it names none. The start's other guards are the executor's own, read
// before this: an earlier generation, a phase the issue has left, an issue out of the workflow and
// a lingering tree each finish it, as they finish any start.
func newsOnlyRefusal(machine *supervise.Machine, found bool, entry int64) string {
	if !found {
		return "no claim holds the role"
	}
	held := machine.Claim()
	if record.NewsClaimTakes(held.GivenEntry, entry, held.State, held.RuleHolds()) {
		return ""
	}
	switch {
	case held.GivenEntry != entry:
		return fmt.Sprintf("the claim was given entry %d and its role's phases row names %d, so its phase start carries the news", held.GivenEntry, entry)
	case record.ClaimEnded(held.State):
		return fmt.Sprintf("the claim is %s, and the start that revives it carries the news", held.State)
	}
	return "an operator suspended the claim, and the start that resumes it carries the news"
}
