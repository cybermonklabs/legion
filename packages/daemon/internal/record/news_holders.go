package record

// news_holders.go is the one rule for who holds a member's news of the approved design: the claim
// that holds its phase now (NewsClaims.HolderOf), what the record holds of that claim
// (ReadOwedHolders), and whether a news-only start reaches it (NewsClaimTakes), so the daemon's
// reconcile, its start and its send, and a replay of a recorded window, read the rule from one
// place.

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// ToldKey names one told row: an audience of one issue (DesignTold).
type ToldKey struct {
	Issue    string
	Audience DesignAudience
}

// OwedNewsItem is one item of a tree's owed news with the told row it is owed to.
type OwedNewsItem struct {
	Told ToldKey
	Item NewsItem
}

// OwedNewsItems is every item news owes, each with its told row: each member's to its holder row,
// and the architect's to the tree root's architect row.
func OwedNewsItems(news TreeNews, root string) []OwedNewsItem {
	var items []OwedNewsItem
	for _, member := range news.Members {
		for _, item := range member.Items {
			items = append(items, OwedNewsItem{Told: ToldKey{member.Issue, AudienceHolder}, Item: item})
		}
	}
	for _, item := range news.Architect {
		items = append(items, OwedNewsItem{Told: ToldKey{root, AudienceArchitect}, Item: item})
	}
	return items
}

// NewsHolder is the claim that holds one member's phase now, as the reconcile, the start, the send
// and the root architect's summary all name it (NewsClaims.HolderOf): the claim of the role the
// member's phase names, or, for a member in accepting, the architect that owns it. Issue is the
// issue whose role that claim holds, the member itself or the issue the owning architect's token
// names, and Role and Token are the claim's.
type NewsHolder struct {
	Issue Issue
	Role  claim.Role
	Token claim.Token
}

// PhaseHolder is the holder of member's phase where a phase worker holds it: the claim, in the
// project whose token is project, of the role the phase names. A phase no role holds, accepting,
// which the owning architect holds (NewsClaims.HolderOf), and a token that cannot be built name
// none.
func PhaseHolder(project string, member Issue) (NewsHolder, bool) {
	role := phase.RoleFor(member.Phase)
	if role == "" || role == claim.RoleArchitect {
		return NewsHolder{}, false
	}
	token, err := claim.NewToken(project, member.Key, role)
	if err != nil {
		return NewsHolder{}, false
	}
	return NewsHolder{Issue: member, Role: role, Token: token}, true
}

// NewsClaims names the holder of each member of one tree (HolderOf). project is the tree's project
// token, read from its root's record as the daemon's notice routing reads it, "" where that
// project names none. members is the tree's issues by key, none of them or every one: a caller
// hands over the facts it read of the tree in its own transaction, whole (NewsFacts:
// NewNewsClaims, Seed), and Member reads the tree whole on a miss. So the owner walk, which climbs
// through a member's parents, reads every issue of the tree (owningIssue). Every reader takes a
// member's issue from the record, through Member or, at the daemon's send, through Store.Issue,
// which skips a key the record holds no issue of. So none mistakes such a key for an issue in no
// phase. runs is the daemon's outbox's live view of whether an architect's claim holds its role,
// which the outbox's start weighs the owner walk by (WithLiveView), nil for a reader that weighs
// the claims the store records. owner is that walk, built the first time an owner is asked
// (owningIssue), and nil until then.
type NewsClaims struct {
	project string
	root    Issue
	members map[string]Issue
	runs    func(claim.Token) bool
	owner   func(Issue) string
}

// NewNewsClaims names the holders of root's tree. facts is what the caller read of the tree in its
// transaction (TreeNewsFacts), every issue of the tree among it, or nil for a caller that read
// none: Member then reads the tree's issues the first time it is asked, or a later reader of the
// facts hands them over (Seed). It takes the facts, never a list of issues, so c holds every issue
// of the tree or none.
func NewNewsClaims(root Issue, facts *NewsFacts) *NewsClaims {
	claims := &NewsClaims{root: root}
	claims.project, _ = claim.ProjectToken(root.Project)
	if facts == nil {
		claims.members = map[string]Issue{}
		return claims
	}
	claims.members = make(map[string]Issue, len(facts.Members))
	claims.Seed(facts)
	return claims
}

// Root is the tree root whose tree c names the holders of.
func (c *NewsClaims) Root() Issue { return c.root }

// Seed holds each issue of the tree, as facts its caller read in its transaction name it, under
// its key where c holds no issue of that key yet, so Member reads the tree's issues again only for
// a key of no issue of the tree. An issue c already holds was read in the same transaction, and is
// kept.
func (c *NewsClaims) Seed(facts *NewsFacts) {
	for _, member := range facts.Members {
		if _, held := c.members[member.Key]; !held {
			c.members[member.Key] = member
		}
	}
}

// Member is the issue of c's tree whose key is key. Where c holds no issue of key, it holds none
// of the tree's (NewsClaims) or key is of no issue of the tree, and the tree's issues are read
// again, whole (Store.TreeIssues), in the caller's transaction, so a member no caller handed over
// is still named by its real phase. A key the record holds no issue of in the tree is an error: the
// zero issue stands in no phase, names no holder, and would read as told at its next start.
func (c *NewsClaims) Member(ctx context.Context, tx pgx.Tx, records Store, key string) (Issue, error) {
	if issue, held := c.members[key]; held {
		return issue, nil
	}
	issues, err := records.TreeIssues(ctx, tx, c.root.Tree)
	if err != nil {
		return Issue{}, err
	}
	for _, issue := range issues {
		c.members[issue.Key] = issue
	}
	issue, held := c.members[key]
	if !held {
		return Issue{}, fmt.Errorf("the record holds no issue %s in the tree of %s", key, c.root.Key)
	}
	return issue, nil
}

// HolderOf is the one answer to which claim holds member's phase now, and whether one does: for a
// member in accepting, the architect that owns it (owningIssue: the owner walk from the member
// itself, as the reviewer's approval notice that moved it there was routed and as sign_off accepts
// it, which ends at its own architect while that claim holds its role, else at the nearest one
// above it whose claim does, else at the tree root's); for a member in any other phase, the claim
// of the role that phase names (PhaseHolder). The daemon's reconcile hands a member's news to it,
// a start carries the news of the members it holds, a send tells a member only while the claim it
// is held for is it, and the root architect's summary says how it is told.
//
// A member it names no holder of is held by no claim, in every one of those readers alike: a phase
// no role holds, and a token that cannot be built, the owning architect's among them. No
// news-only start is queued for such a member, no start attaches its news, a send drops it, and
// the summary says that the next start of its phase tells it, as it says of a member whose holder
// has no claim. The walk is read in the caller's transaction, once.
func (c *NewsClaims) HolderOf(ctx context.Context, tx pgx.Tx, records Store, member Issue) (NewsHolder, bool, error) {
	switch {
	case c.project == "":
		return NewsHolder{}, false, nil
	case member.Phase != phase.Accepting:
		holder, held := PhaseHolder(c.project, member)
		return holder, held, nil
	}
	key, err := c.owningIssue(ctx, tx, records, member)
	if err != nil {
		return NewsHolder{}, false, err
	}
	owner, err := c.Member(ctx, tx, records, key)
	if err != nil {
		return NewsHolder{}, false, err
	}
	token, err := claim.NewToken(c.project, owner.Key, claim.RoleArchitect)
	if err != nil {
		return NewsHolder{}, false, nil
	}
	return NewsHolder{Issue: owner, Role: claim.RoleArchitect, Token: token}, true, nil
}

// owningIssue is the key of the issue whose architect owns member now: the owner walk from the
// member itself (OwningIssue), built once for c's tree in the caller's transaction. The walk climbs
// through the member's parents, so it reads every issue of the tree: c holds every one or none
// (NewsClaims), and where it holds none, Member reads them, once, with the root's key. A reader in
// a transaction weighs the architect claims the store records (ArchitectRuns); the daemon's
// outbox's start weighs the machine's live view (runs, WithLiveView), as the outbox routes a
// notice.
func (c *NewsClaims) owningIssue(ctx context.Context, tx pgx.Tx, records Store, member Issue) (string, error) {
	if c.owner != nil {
		return c.owner(member), nil
	}
	if _, err := c.Member(ctx, tx, records, c.root.Key); err != nil {
		return "", err
	}
	tree := TreeByKey(slices.Collect(maps.Values(c.members)))
	runs := c.runs
	if runs == nil {
		var err error
		if runs, err = ArchitectRuns(ctx, records, tx, c.project, tree); err != nil {
			return "", err
		}
	}
	c.owner = func(issue Issue) string { return OwningIssue(c.project, tree, issue, false, runs) }
	return c.owner(member), nil
}

// WithLiveView makes c weigh the owner walk by runs, the daemon's outbox's live view of whether an
// architect's claim holds its role, which also passes over a claim a decision in flight is
// stopping. The outbox's start reads it so, as the outbox routes a notice, so a start never carries
// a member's news to an architect that a notice of the member would pass.
func (c *NewsClaims) WithLiveView(runs func(claim.Token) bool) *NewsClaims {
	c.runs = runs
	return c
}

// RootArchitect is the holder of the tree root's architect row: the root's architect claim, which
// works no phase but holds a phases row of the entries it is handed. A root whose project or key
// names no token has none.
func (c *NewsClaims) RootArchitect() (NewsHolder, bool) {
	if c.project == "" {
		return NewsHolder{}, false
	}
	token, err := claim.NewToken(c.project, c.root.Key, claim.RoleArchitect)
	if err != nil {
		return NewsHolder{}, false
	}
	return NewsHolder{Issue: c.root, Role: claim.RoleArchitect, Token: token}, true
}

// OwedHolder is the holder of one told row news owes (NewsHolder), with what the record holds of
// its claim: the entry its role's phases row names, 0 where the row names none, and its run
// (RoleRun: its queued starts and its claim). An architect's entry is read as a phase worker's
// is: a root's architect holds a phases row that states the work it is owed, and a child's own
// architect, which no start hands an entry, holds none.
type OwedHolder struct {
	NewsHolder
	Entry int64
	Run   RoleRun
}

// ReadOwedHolders is the one read of who is told each told row of keys, rows owed news: the holder
// of a member's holder row (NewsClaims.HolderOf), and of the tree root's architect row
// (NewsClaims.RootArchitect), each with its entry and run, by told row. A row whose holder cannot
// be named is absent, and a row of an issue the record does not hold in the tree is an error
// (NewsClaims.Member). The daemon's reconcile asks it of the rows it sends news to, and the root
// architect's summary of the holder row of each member owed news, to say how each is told.
func ReadOwedHolders(ctx context.Context, tx pgx.Tx, records Store, claims *NewsClaims, keys []ToldKey) (map[ToldKey]OwedHolder, error) {
	holders := make(map[ToldKey]OwedHolder, len(keys))
	for _, key := range keys {
		if key.Audience == AudienceArchitect {
			if holder, held := claims.RootArchitect(); held {
				holders[key] = OwedHolder{NewsHolder: holder}
			}
			continue
		}
		member, err := claims.Member(ctx, tx, records, key.Issue)
		if err != nil {
			return nil, err
		}
		holder, held, err := claims.HolderOf(ctx, tx, records, member)
		if err != nil {
			return nil, err
		}
		if held {
			holders[key] = OwedHolder{NewsHolder: holder}
		}
	}
	runKeys := make([]RoleRunKey, 0, len(holders))
	for key, holder := range holders {
		rows, err := records.Phases(ctx, tx, holder.Issue.Key)
		if err != nil {
			return nil, err
		}
		holder.Entry = PhaseRowOf(rows, holder.Role).Entry
		holders[key] = holder
		runKeys = append(runKeys, RoleRunKey{Token: holder.Token, Issue: holder.Issue.Key, Role: holder.Role, Generation: holder.Issue.Generation})
	}
	runs, err := records.RoleRuns(ctx, tx, runKeys)
	if err != nil {
		return nil, err
	}
	for key, holder := range holders {
		holder.Run = runs[holder.Token]
		holders[key] = holder
	}
	return holders, nil
}

// NewsStartQueued says whether run holds a news-only start still to run (SuperviseRequest.News):
// one is all a claim needs, since each reads what it carries when it runs.
func NewsStartQueued(run RoleRun) bool {
	return slices.ContainsFunc(run.Queued, func(queued QueuedSupervise) bool {
		return queued.Request.Op == "start" && queued.Request.News
	})
}

// HoldsUntakenTask says whether held, a claim as the record holds it, holds a task its worker has
// not taken yet: a pending delivery with no confirmation. A role the record holds no claim of (nil)
// holds none. A news-only start queued behind such a task would only wait, since the claim's
// machine takes no second task while one is pending (supervise.ErrDeliveryPending), and its start
// would retry on its backoff until a turn takes the first. So none is queued, and the news stays
// owed: the daemon's sweep queues the reconcile again, which queues the start once that task is
// taken, and the next start of the phase carries the news itself. The root architect's summary
// reads the same rule over the same claim, for a phase holder and for the child's architect that
// owns a member in accepting alike, and says that member's news waits until the task is taken.
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
func HoldsUntakenTask(held *RoleClaim) bool {
	return held != nil && supervise.HoldsTask(held.Pending) && held.Pending.ConfirmedAt.IsZero()
}

// NewsClaimTakes is the one rule for whether a news-only start may hand its news to a claim the
// caller holds, over what says it: it was given the entry its role's phases row names (entry, 0
// where the row names none), since a claim behind that entry is owed its phase start, which
// carries the news — a root's architect among them, whose start of an entry carries its summary
// as a phase worker's carries its news; it has not ended (ClaimEnded), since a failed or retired
// claim is revived by its phase's own repair, whose start carries the news; and no operator holds
// it suspended (operatorHolds). A claim an idle pod release suspended takes the news, and the
// start that carries it wakes the claim as any start wakes one. Both readers state it through
// this: the reconcile and the architect's summary over the claim the record holds
// (RecordedClaimTakesNews), and the daemon's start over the claim its machine holds.
func NewsClaimTakes(given, entry int64, state supervise.ClaimState, operatorHolds bool) bool {
	return given == entry && !ClaimEnded(state) && !operatorHolds
}

// RecordedClaimTakesNews is NewsClaimTakes over a claim as the record holds it (RoleClaim), nil
// for a role the record holds no claim of, which takes no news.
func RecordedClaimTakesNews(held *RoleClaim, entry int64) bool {
	if held == nil {
		return false
	}
	operator := held.State == supervise.StateSuspended && held.SuspendedBy == supervise.SuspendedByOperator
	return NewsClaimTakes(held.GivenEntry, entry, held.State, operator)
}
