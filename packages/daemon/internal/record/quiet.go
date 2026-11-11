package record

import (
	"slices"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// TreeClaim is one role claim as the quiet measure reads it: the issue it is on, the state it
// stands in, and its two step times — when the daemon last saw its agent make a call (step_at)
// and when the turn that call belongs to began (turn_started_at, which is all a claim whose
// agent has sent no frame at all has, supervise.StepRecord). It is the whole of what QuietTrees
// asks of a claim, and both readers of the measure build it: the admission pass from the store's
// own narrow read (Store.ProjectClaims), the state's projection from the claims it already holds
// (TreeClaimsOf). Either step time the column holds as null reads as the zero time, which moves
// no tree.
type TreeClaim struct {
	Issue         string
	State         supervise.ClaimState
	StepAt        time.Time
	TurnStartedAt time.Time
}

// restingStates are the four claim states that are no work under way: an idle agent has finished
// its turn and waits to be prompted, a suspended one was stopped with its session kept, a failed
// one ran out of a budget, and a retired one is over. A claim in any one of the other seven
// states supervise.ClaimState names — queued, launch_uncertain, launching, shim_connected,
// registered, ready, working — is work under way, whether its process is still coming up or its
// agent is in a turn.
//
// This is not pods.IdleClaim.Live, and the two must not be merged. That cut asks whether a claim
// still holds its place in a pod: it names three states (suspended, failed, retired) and keeps an
// idle claim live on purpose, because the pod is that agent's and it waits there to be prompted
// again. A tree all of whose agents sit idle with nobody about to prompt them is exactly the tree
// this measure exists to find, so a reader that took the pod rule for this one would never find a
// stopped tree at all. Merging them would carry that exception into a cut it is wrong for.
var restingStates = []supervise.ClaimState{
	supervise.StateIdle, supervise.StateSuspended, supervise.StateFailed, supervise.StateRetired,
}

// AtWork is whether this one claim is work under way (restingStates).
func (c TreeClaim) AtWork() bool { return !slices.Contains(restingStates, c.State) }

// TreeClaimsOf is claims as the measure reads them, for a caller that already holds whole claims
// rather than the store's narrow read: the state's projection, which is handed every claim of the
// database beside the records it joins them to (projection.Project), and which must make no
// second query for what it has. The two readers of the measure build the same rows from the same
// four facts for that reason — one of them reading the claims differently is exactly how the
// admission pass and the state would come to answer a tree differently.
//
// Nothing is dropped here: a claim on no issue (the project's own controller holds one) and a
// claim of another project both carry an issue that is no member of any tree the caller measures,
// and QuietTrees drops them where it drops every other claim it cannot place.
func TreeClaimsOf(claims []supervise.Claim) []TreeClaim {
	read := make([]TreeClaim, 0, len(claims))
	for _, c := range claims {
		read = append(read, TreeClaim{
			Issue:         c.Issue,
			State:         c.State,
			StepAt:        c.Step.At,
			TurnStartedAt: c.Step.TurnStartedAt,
		})
	}
	return read
}

// TreeQuiet is one tree's measure: MovedAt, the newest movement of anything in it; Quiet, how
// long it has been still at the caller's own now; and AtWork, whether any claim of it is work
// under way. The two answers are one value because the rule that reads them is one rule — a tree
// that has been quiet for long enough is stopped only while nothing of it is at work — and a
// reader that took the two from two calls could take them from two moments.
type TreeQuiet struct {
	MovedAt time.Time
	Quiet   time.Duration
	AtWork  bool
}

// TreeMember says whether issue belongs to the tree its record names, for the quiet measure and
// for every rule written over it: its tree's root, whatever that root's own status says, or an
// issue under it the workflow still runs (InWorkflow). A child the record shows as done is no
// member.
//
// It is the one statement of that membership, and the three readers that must never differ take
// it: the measure itself (QuietTrees, where a claim of an issue that is no member moves
// nothing), the admission pass's gathering of a stalled tree's issues and the notice that names
// them (admit's tellStalledTrees and stalledTreeReason). A tree told to a controller as stopped
// must be made of exactly the issues whose stillness was measured, which is what one predicate
// gives.
func TreeMember(issue Issue) bool {
	return claim.IsTreeRoot(issue.Key, issue.Tree) || InWorkflow(issue)
}

// QuietTrees measures, for every tree the given records name, how long that tree has been still
// and whether it is at work, keyed by the tree's root key. It is the one statement of both
// questions, and every reader takes it: the admission pass, over the records and the claims it
// reads once for the whole pass, and the state's projection, over the records and claims it
// already holds. Neither may read per tree — both judge every tree of the project in one pass —
// so the measure answers them all from one walk of the records and one of the claims.
//
// The tree of a root is that root and the issues under it the workflow still runs (TreeMember),
// and nothing else. A child the record shows as done is no member: its pull request goes on
// drawing GitHub events — a comment, a deployment, a late check run — long after the work
// stopped, and each of those stamps that child's record (Store.MarkIssueMoved). Counted, they
// would keep a finished tree's quiet time at zero for as long as anyone kept writing on an old
// pull request. The root itself is a member whatever its own status says, because a tree is
// measured from its root even when that root alone is left.
//
// A tree's movement is the newest of two kinds of fact: the MovedAt of each member — when the
// daemon last saw that issue move (Issue.MovedAt) — and the step times of the claims of those
// members, which are what moves while an agent works without the record changing at all: a turn
// that has begun (TurnStartedAt) and each call the daemon has seen it make (StepAt). One issue
// moving moves the whole tree, since the roles of a tree work for the root and a tree with one
// role still stepping has not stopped. A claim of an issue that is no member — of a done child,
// of another tree, of no issue at all — moves nothing and is at work for nobody.
//
// Quiet is the caller's own now less that movement, and never negative: a fact can carry a time
// from another clock (Dispatch's, GitHub's), so a movement can stand ahead of the reader's now,
// and a tree that has just moved is quiet for no time rather than for a negative one.
//
// AtWork is true while any claim of the tree is in a state that is work under way (restingStates
// names the four that are not). It is the second question because the first cannot answer it: an
// agent in a long turn that makes no tool call steps nothing, and its tree's quiet time grows
// while the work is going on.
func QuietTrees(issues []Issue, claims []TreeClaim, now time.Time) map[string]TreeQuiet {
	trees := make(map[string]TreeQuiet, len(issues))
	// The tree each issue the measure counts stands in, which is also the membership test the
	// claims are read through: a claim of an issue absent here is a claim of no tree measured.
	member := make(map[string]string, len(issues))
	for _, issue := range issues {
		if !TreeMember(issue) {
			continue
		}
		member[issue.Key] = issue.Tree
		measure := trees[issue.Tree]
		if issue.MovedAt.After(measure.MovedAt) {
			measure.MovedAt = issue.MovedAt
		}
		trees[issue.Tree] = measure
	}
	for _, held := range claims {
		tree, stands := member[held.Issue]
		if !stands {
			continue
		}
		measure := trees[tree]
		if held.StepAt.After(measure.MovedAt) {
			measure.MovedAt = held.StepAt
		}
		if held.TurnStartedAt.After(measure.MovedAt) {
			measure.MovedAt = held.TurnStartedAt
		}
		measure.AtWork = measure.AtWork || held.AtWork()
		trees[tree] = measure
	}
	for tree, measure := range trees {
		measure.Quiet = max(0, now.Sub(measure.MovedAt))
		trees[tree] = measure
	}
	return trees
}
