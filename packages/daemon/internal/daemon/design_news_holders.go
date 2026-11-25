package daemon

// design_news_holders.go is the one rule for who holds a member's news of the approved design: the
// claim that holds its phase now (newsClaims.holderOf), how that holder is told (holderTold), and
// whether a news-only start reaches it (newsClaimTakes), so the rule reads in one file.

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// newsHolder is the claim that holds one member's phase now, as the reconcile, the start, the send
// and the root architect's summary all name it (newsClaims.holderOf): the claim of the agent that
// works the step of the role the member's phase names, or, for a member in accepting, the
// architect that owns it. issue is the issue whose role that claim holds, the member itself or the
// issue the owning architect's token names, role is that role, and token is the claim's.
type newsHolder struct {
	issue record.Issue
	role  claim.Role
	token claim.Token
}

// phaseHolder is the holder of member's phase where a phase worker holds it: in the project whose
// token is project, the claim of the agent that works the step of the role that works the member
// there (record.Issue.RoleAt: at step the role of the running step, a role the deployment
// defines). That claim is record.WorkerClaim's over the member's phases rows, read in tx: the
// claim the role's row names while that row runs a step of the plan, which can be the claim of a
// key's agent that started on an earlier step of another role, beside the role's own claim. A
// phase no role holds, accepting, which the owning architect holds (newsClaims.holderOf), and a
// token that cannot be built name none, and read no rows.
func phaseHolder(ctx context.Context, tx pgx.Tx, phases record.PhaseReader, project string, member record.Issue) (newsHolder, bool, error) {
	role := member.RoleAt(member.Phase)
	if role == "" || role == claim.RoleArchitect {
		return newsHolder{}, false, nil
	}
	rows, err := phases.Phases(ctx, tx, member.Key)
	if err != nil {
		return newsHolder{}, false, fmt.Errorf("read the phases rows of %s: %w", member.Key, err)
	}
	token, err := record.WorkerClaim(project, member.Key, role, rows)
	if err != nil {
		return newsHolder{}, false, nil
	}
	return newsHolder{issue: member, role: role, token: token}, true, nil
}

// newsClaims names the holder of each member of one tree (holderOf). project is the tree's project
// token, read from its root's record as notice routing reads it (readNoticeRoute), "" where that
// project names none. members is the tree's issues by key, none of them or every one: a caller
// hands over the facts it read of the tree in its own transaction, whole (record.NewsFacts:
// newNewsClaims, seed), and member reads the tree whole on a miss. So the owner walk, which climbs
// through a member's parents, reads every issue of the tree (owningIssue). Every reader takes a
// member's issue from the record, through member or, at a send, through records.Issue
// (readNewsAtSend), which skips a key the record holds no issue of. So none mistakes such a key
// for an issue in no phase. runs is the outbox's live view of whether an architect's claim holds
// its role, which the outbox's start weighs the owner walk by (withLiveView), nil for a reader
// that weighs the claims the store records. owner is that walk, built the first time an owner is
// asked (owningIssue), and nil until then.
type newsClaims struct {
	project string
	root    record.Issue
	members map[string]record.Issue
	runs    func(claim.Token) bool
	owner   func(record.Issue) string
}

// newNewsClaims names the holders of root's tree. facts is what the caller read of the tree in its
// transaction (record.TreeNewsFacts), every issue of the tree among it, or nil for a caller that
// read none: member then reads the tree's issues the first time it is asked, or a later reader of
// the facts hands them over (seed). It takes the facts, never a list of issues, so c holds every
// issue of the tree or none.
func newNewsClaims(root record.Issue, facts *record.NewsFacts) *newsClaims {
	claims := &newsClaims{root: root}
	claims.project, _ = claim.ProjectToken(root.Project)
	if facts == nil {
		claims.members = map[string]record.Issue{}
		return claims
	}
	claims.members = make(map[string]record.Issue, len(facts.Members))
	claims.seed(facts)
	return claims
}

// seed holds each issue of the tree, as facts its caller read in its transaction name it, under
// its key where c holds no issue of that key yet, so member reads the tree's issues again only for
// a key of no issue of the tree. An issue c already holds was read in the same transaction, and is
// kept.
func (c *newsClaims) seed(facts *record.NewsFacts) {
	for _, member := range facts.Members {
		if _, held := c.members[member.Key]; !held {
			c.members[member.Key] = member
		}
	}
}

// member is the issue of c's tree whose key is key. Where c holds no issue of key, it holds none
// of the tree's (newsClaims) or key is of no issue of the tree, and the tree's issues are read
// again, whole (record.Store.TreeIssues), in the caller's transaction, so a member no caller
// handed over is still named by its real phase. A key the record holds no issue of in the tree is
// an error: the zero issue stands in no phase, names no holder, and would read as told at its next
// start.
func (c *newsClaims) member(ctx context.Context, tx pgx.Tx, records record.Store, key string) (record.Issue, error) {
	if issue, held := c.members[key]; held {
		return issue, nil
	}
	issues, err := records.TreeIssues(ctx, tx, c.root.Tree)
	if err != nil {
		return record.Issue{}, err
	}
	for _, issue := range issues {
		c.members[issue.Key] = issue
	}
	issue, held := c.members[key]
	if !held {
		return record.Issue{}, fmt.Errorf("the record holds no issue %s in the tree of %s", key, c.root.Key)
	}
	return issue, nil
}

// holderOf is the one answer to which claim holds member's phase now, and whether one does: for a
// member in accepting, the architect that owns it (owningIssue: the owner walk from the member
// itself, as the reviewer's approval notice that moved it there was routed and as sign_off accepts
// it, which ends at its own architect while that claim holds its role, else at the nearest one
// above it whose claim does, else at the tree root's); for a member in any other phase, the claim
// of the agent that works the step of the role that phase names (phaseHolder). The reconcile hands
// a member's news to it (settleNews), a start carries the news of the members it holds
// (owedToClaim), a send tells a member only while the claim it is held for is it
// (readNewsAtSend), and the root architect's summary says how it is told (readMemberClaims).
//
// A member it names no holder of is held by no claim, in every one of those readers alike: a phase
// no role holds, and a token that cannot be built, the owning architect's among them. No
// news-only start is queued for such a member, no start attaches its news, a send drops it, and
// the summary says that the next start of its phase tells it (workflow.NewsAtNextStart), as it
// says of a member whose holder has no claim. The walk is read in the caller's transaction, once.
func (c *newsClaims) holderOf(ctx context.Context, tx pgx.Tx, records record.Store, member record.Issue) (newsHolder, bool, error) {
	switch {
	case c.project == "":
		return newsHolder{}, false, nil
	case member.Phase != phase.Accepting:
		return phaseHolder(ctx, tx, records, c.project, member)
	}
	key, err := c.owningIssue(ctx, tx, records, member)
	if err != nil {
		return newsHolder{}, false, err
	}
	owner, err := c.member(ctx, tx, records, key)
	if err != nil {
		return newsHolder{}, false, err
	}
	token, err := claim.NewToken(c.project, owner.Key, claim.RoleArchitect)
	if err != nil {
		return newsHolder{}, false, nil
	}
	return newsHolder{issue: owner, role: claim.RoleArchitect, token: token}, true, nil
}

// owningIssue is the key of the issue whose architect owns member now: the owner walk from the
// member itself (record.OwningIssue), built once for c's tree in the caller's transaction. The walk
// climbs through the member's parents, so it reads every issue of the tree: c holds every one or
// none (newsClaims), and where it holds none, member reads them, once, with the root's key. A
// reader in a transaction weighs the architect claims the store records (record.ArchitectRuns);
// the outbox's start weighs the machine's live view (runs, withLiveView), as the outbox routes a
// notice (noticeArchitect).
func (c *newsClaims) owningIssue(ctx context.Context, tx pgx.Tx, records record.Store, member record.Issue) (string, error) {
	if c.owner != nil {
		return c.owner(member), nil
	}
	if _, err := c.member(ctx, tx, records, c.root.Key); err != nil {
		return "", err
	}
	tree := record.TreeByKey(slices.Collect(maps.Values(c.members)))
	runs := c.runs
	if runs == nil {
		var err error
		if runs, err = record.ArchitectRuns(ctx, records, tx, c.project, tree); err != nil {
			return "", err
		}
	}
	c.owner = func(issue record.Issue) string { return record.OwningIssue(c.project, tree, issue, false, runs) }
	return c.owner(member), nil
}

// withLiveView makes c weigh the owner walk by runs, the outbox's live view of whether an
// architect's claim holds its role (the outbox's holdsItsRole), which also passes over a claim a
// decision in flight is stopping. The outbox's start reads it so, as the outbox routes a notice, so
// a start never carries a member's news to an architect that a notice of the member would pass.
func (c *newsClaims) withLiveView(runs func(claim.Token) bool) *newsClaims {
	c.runs = runs
	return c
}

// rootArchitect is the holder of the tree root's architect row: the root's architect claim, which
// works no phase but holds a phases row of the entries it is handed (dispatch://INFRA-2614). A root
// whose project or key names no token has none.
func (c *newsClaims) rootArchitect() (newsHolder, bool) {
	if c.project == "" {
		return newsHolder{}, false
	}
	token, err := claim.NewToken(c.project, c.root.Key, claim.RoleArchitect)
	if err != nil {
		return newsHolder{}, false
	}
	return newsHolder{issue: c.root, role: claim.RoleArchitect, token: token}, true
}

// owedHolder is the holder of one told row news owes (newsHolder), with what the record holds of
// its claim: the entry its role's phases row names, 0 where the row names none, and its run
// (record.RoleRun: its queued starts and its claim). An architect's entry is read as a phase
// worker's is: a root's architect holds a phases row that states the work it is owed
// (dispatch://INFRA-2614), and a child's own architect, which no start hands an entry, holds none.
type owedHolder struct {
	newsHolder
	entry int64
	run   record.RoleRun
}

// readOwedHolders is the one read of who is told each told row of keys, rows owed news: the holder
// of a member's holder row (newsClaims.holderOf), and of the tree root's architect row
// (newsClaims.rootArchitect), each with its entry and run, by told row. A row whose holder cannot
// be named is absent, and a row of an issue the record does not hold in the tree is an error
// (newsClaims.member). The reconcile asks it of the rows it sends news to (settleNews), and the
// root architect's summary of the holder row of each member owed news, to say how each is told
// (readMemberClaims).
func readOwedHolders(ctx context.Context, tx pgx.Tx, records record.Store, claims *newsClaims, keys []toldKey) (map[toldKey]owedHolder, error) {
	holders := make(map[toldKey]owedHolder, len(keys))
	for _, key := range keys {
		if key.audience == record.AudienceArchitect {
			if holder, held := claims.rootArchitect(); held {
				holders[key] = owedHolder{newsHolder: holder}
			}
			continue
		}
		member, err := claims.member(ctx, tx, records, key.issue)
		if err != nil {
			return nil, err
		}
		holder, held, err := claims.holderOf(ctx, tx, records, member)
		if err != nil {
			return nil, err
		}
		if held {
			holders[key] = owedHolder{newsHolder: holder}
		}
	}
	runKeys := make([]record.RoleRunKey, 0, len(holders))
	for key, holder := range holders {
		rows, err := records.Phases(ctx, tx, holder.issue.Key)
		if err != nil {
			return nil, err
		}
		holder.entry = record.PhaseRowOf(rows, holder.role).Entry
		holders[key] = holder
		runKeys = append(runKeys, record.RoleRunKey{Token: holder.token, Issue: holder.issue.Key, Role: holder.role, Generation: holder.issue.Generation})
	}
	runs, err := records.RoleRuns(ctx, tx, runKeys)
	if err != nil {
		return nil, err
	}
	for key, holder := range holders {
		holder.run = runs[holder.token]
		holders[key] = holder
	}
	return holders, nil
}

// holderTold is how the holder of a member owed news is told, from its claim, a phase worker's or
// that of the child's architect that owns a member in accepting (readMemberClaims): by a task now
// when the reconcile queues a news-only start for the claim, which is when one reaches it
// (recordedClaimTakesNews, the one rule the reconcile queues that start under and the start runs
// under) and it holds no task its worker has not taken yet (holdsUntakenTask, the reconcile's other
// guard). A claim the start reaches that holds such a task gets no start until a turn takes it, so
// its news waits for that turn (workflow.NewsWaitsForTask). Otherwise by the next start of its
// phase: a claim behind its phase's current entry, owed that entry's start, no claim at all, or one
// that retired, failed, or an operator suspended, since the start that revives it carries the news.
func holderTold(run record.RoleRun, entry int64) workflow.NewsState {
	switch {
	case !recordedClaimTakesNews(run.Claim, entry):
		return workflow.NewsAtNextStart
	case holdsUntakenTask(run.Claim):
		return workflow.NewsWaitsForTask
	}
	return workflow.NewsSentNow
}

// newsStartQueued says whether run holds a news-only start still to run (record.SuperviseRequest.News):
// one is all a claim needs, since each reads what it carries when it runs.
func newsStartQueued(run record.RoleRun) bool {
	return slices.ContainsFunc(run.Queued, func(queued record.QueuedSupervise) bool {
		return queued.Request.Op == "start" && queued.Request.News
	})
}

// holdsUntakenTask says whether held, a claim as the record holds it, holds a task its worker has
// not taken yet: a pending delivery with no confirmation. A role the record holds no claim of (nil)
// holds none. A news-only start queued behind such a task would only wait, since the claim's
// machine takes no second task while one is pending (supervise.ErrDeliveryPending), and its start
// would retry on its backoff until a turn takes the first. So none is queued, and the news stays
// owed: the sweep queues the reconcile again (queueOwedDesignNews), which queues the start once
// that task is taken, and the next start of the phase carries the news itself. The root
// architect's summary reads the same rule over the same claim (holderTold), for a phase holder and
// for the child's architect that owns a member in accepting alike, and says that member's news
// waits until the task is taken (workflow.NewsWaitsForTask).
//
// The daemon's own delivery of a relaunch's record is no task (supervise.HoldsTask), as
// workflow's owed reads it for the same question: it carries only the sentence of what the
// relaunch ended, a newer task takes its place while no prompt of it waits on a result (the
// machine's queue), and that task carries the sentence in front of its own text. So a claim that
// holds it alone is queued its news-only start, and the summary says a task tells the member now.
// Should the start meet a prompt of it still waiting on its result, the machine refuses the start
// as it refuses one behind any untaken task, and the start retries on its backoff until that
// prompt's turn has taken the delivery.
//
// A claim whose turn is working a task it took (a confirmed delivery) is queued its news-only
// start, which does not take that task's place: the machine refuses a task of news alone behind
// any task that holds work (supervise.RequestDeliver.NewsOnly), since retiring the task of a
// running turn would lose it to that turn's provider error, which sends the task again only while
// it is pending. The start waits on its backoff until the turn has ended, and its task is sent
// then. The summary says a task tells such a member now, which is the task that start sends.
func holdsUntakenTask(held *record.RoleClaim) bool {
	return held != nil && supervise.HoldsTask(held.Pending) && held.Pending.ConfirmedAt.IsZero()
}

// newsClaimTakes is the one rule for whether a news-only start may hand its news to a claim the
// caller holds, over what says it: it was given the entry its role's phases row names (entry, 0
// where the row names none), since a claim behind that entry is owed its phase start, which
// carries the news — a root's architect among them, whose start of an entry carries its summary
// as a phase worker's carries its news (dispatch://INFRA-2614); it has not ended (claimEnded),
// since a failed or retired claim is revived by its phase's own repair, whose start carries the
// news; and no operator holds it suspended (operatorHolds). A claim an idle pod release suspended
// takes the news, and the start that carries it wakes the claim as any start wakes one. Both
// readers state it through this: the reconcile and the architect's summary over the claim the
// record holds (recordedClaimTakesNews), and the start over the claim its machine holds
// (newsOnlyRefusal).
func newsClaimTakes(given, entry int64, state supervise.ClaimState, operatorHolds bool) bool {
	return given == entry && !claimEnded(state) && !operatorHolds
}

// recordedClaimTakesNews is newsClaimTakes over a claim as the record holds it (record.RoleClaim),
// nil for a role the record holds no claim of, which takes no news.
func recordedClaimTakesNews(held *record.RoleClaim, entry int64) bool {
	if held == nil {
		return false
	}
	operator := held.State == supervise.StateSuspended && held.SuspendedBy == supervise.SuspendedByOperator
	return newsClaimTakes(held.GivenEntry, entry, held.State, operator)
}

// newsOnlyRefusal is why a news-only start reaches no claim, as the claim's machine holds it when
// the start runs, and "" when it may: the guards the reconcile queued it under, read again
// (newsClaimTakes), since the claim can have moved since. entry is the entry the role's phases row
// names, 0 where it names none. The start's other guards are the executor's own, read before this:
// an earlier generation, a phase the issue has left, an issue out of the workflow and a lingering
// tree each finish it, as they finish any start.
func newsOnlyRefusal(machine *supervise.Machine, found bool, entry int64) string {
	if !found {
		return "no claim holds the role"
	}
	held := machine.Claim()
	if newsClaimTakes(held.GivenEntry, entry, held.State, held.RuleHolds()) {
		return ""
	}
	switch {
	case held.GivenEntry != entry:
		return fmt.Sprintf("the claim was given entry %d and its role's phases row names %d, so its phase start carries the news", held.GivenEntry, entry)
	case claimEnded(held.State):
		return fmt.Sprintf("the claim is %s, and the start that revives it carries the news", held.State)
	}
	return "an operator suspended the claim, and the start that resumes it carries the news"
}
