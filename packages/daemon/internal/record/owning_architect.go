package record

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// OwningArchitect is the architect claim of the issue the owner walk from issue ends at
// (OwningIssue). It fails when that issue's key, or project, makes no claim token.
func OwningArchitect(project string, tree map[string]Issue, issue Issue, fromParent bool, runs func(claim.Token) bool) (claim.Token, error) {
	return claim.NewToken(project, OwningIssue(project, tree, issue, fromParent, runs), claim.RoleArchitect)
}

// NoticeStartsAtParent is whether the owner walk for a notice of kind, about role's claim, starts
// at its issue's parent (OwningIssue's fromParent) rather than at the issue itself. Two notices do.
// A child's close or status change: it tells of the child's own run starting, leaving, coming back
// or closing, which is not the child's own architect to judge (a leave suspends that architect and
// a close as done retires it, in workflow's leave), and the architect above it decides what the
// rest of the tree does. And a worker-died or a worker-stalled about an issue's own architect
// (role is claim.RoleArchitect): the notice is about that architect, so the architect above it
// must hear it, and the tree root's own architect, which has none above it, hears its own (the
// walk from the root ends at the root, OwningIssue). Either notice on a child with no parent goes
// to the tree root, never to the child's own architect: a failed claim is passed over by the walk
// anyway, but a stalled one still holds its role and would be told of its own stall, and the
// outbox can publish a child's close or status change while the child's own architect still holds
// its role, before its suspension or close lands.
func NoticeStartsAtParent(kind NoticeKind, role claim.Role) bool {
	aboutItsArchitect := role == claim.RoleArchitect && (kind == "worker-died" || kind == "worker-stalled")
	return aboutItsArchitect || kind == "child-closed" || kind == "child-status"
}

// OwningIssue is the key of the issue whose architect owns issue: the nearest issue at or above
// it, through its parents, whose architect claim holds its role (runs), else the tree root
// (issue.Tree), whose architect is the tree's own claim. The daemon starts a child's architect
// with the child's first phase, and starts a failed one again at the child's next phase start.
// While a child's architect does not hold its role, which HoldsRole decides (and the daemon's
// holdsRole over the machine's live view), the child's notices go on up to the architect above it.
// An issue whose key makes no claim token has no architect and owns nothing (TreeByKey), so a walk
// that starts at one ends at the tree root.
//
// fromParent starts the walk at the issue's parent instead, for a notice the architect above the
// issue must hear (NoticeStartsAtParent). Such a walk ends at the issue itself only when the issue
// is the tree root: one from an issue with no parent ends at the tree root, and so does one whose
// record's parents loop back to the issue.
//
// The walk stays inside the issue's tree: admission records a Dispatch re-parent as it comes and
// leaves the issue's tree as it was (admit's recordObservation), except a root that never ran,
// which joins its parent's live tree (workflow/adopt.go), so a parent that is not an issue of
// tree — not recorded, or recorded in another tree — ends the walk at the tree root.
//
// project is the claim project token (claim.ProjectToken), and tree is the tree's issues by key
// (TreeByKey). runs is the reader's own view of whether an architect's claim holds its role: the
// daemon's outbox reads the machine's live view, which also knows a stop in flight, and a reader
// in a transaction reads the claims the store records (ReadOwnerWalk). An issue the walk asks
// runs about ends the walk there when project makes no claim project token, since it has no claim
// to ask about; OwningArchitect then fails on it.
func OwningIssue(project string, tree map[string]Issue, issue Issue, fromParent bool, runs func(claim.Token) bool) string {
	if !claim.IsIssueKey(issue.Key) {
		return issue.Tree
	}
	current := issue
	seen := map[string]bool{}
	if fromParent {
		if issue.Parent == nil || claim.IsTreeRoot(issue.Key, issue.Tree) {
			return issue.Tree
		}
		parent, ok := tree[*issue.Parent]
		if !ok {
			return issue.Tree
		}
		seen[issue.Key] = true
		current = parent
	}
	for {
		if claim.IsTreeRoot(current.Key, current.Tree) {
			return issue.Tree
		}
		token, err := claim.NewToken(project, current.Key, claim.RoleArchitect)
		if err != nil || runs(token) {
			return current.Key
		}
		seen[current.Key] = true
		if current.Parent == nil || seen[*current.Parent] {
			return issue.Tree
		}
		parent, ok := tree[*current.Parent]
		if !ok {
			return issue.Tree
		}
		current = parent
	}
}

// TreeByKey is the tree the owner walk reads (OwningIssue, ArchitectRuns): members, the issues of
// one tree, by key, but for an issue whose key makes no claim token. Such an issue has no
// architect and owns nothing, so a walk that starts at it or meets it ends at the tree root.
func TreeByKey(members []Issue) map[string]Issue {
	tree := make(map[string]Issue, len(members))
	for _, member := range members {
		if claim.IsIssueKey(member.Key) {
			tree[member.Key] = member
		}
	}
	return tree
}

// HoldsRole is whether a claim the store records in state, suspended by by, holds its role: this
// daemon supervises it (state is not ""), it has not failed or retired, and it is not suspended,
// unless the suspension is the idle release's (supervise.Claim's Released). A claim the outbox has
// not created yet has no state and holds none: a new child's architect is such a claim until the
// outbox runs the start its child's first phase start queued, and holds its role from that run on,
// queued or launching. A released claim keeps its session, its phase and its task, and the next
// wake for its role resumes it, so what it owns is still its own; any other suspension stops the
// claim until a rule or an operator starts it again. Were a released architect to lose its role
// here, a notice for it would walk up to the architect above it, which owns neither the issue nor
// the answer.
//
// It is the whole rule for a reader in a transaction (ArchitectRuns). The daemon's outbox reads
// the same rule over the machine's live view and also passes over a claim whose process a
// decision in flight is stopping (the daemon's holdsRole), which no record shows.
func HoldsRole(state supervise.ClaimState, by supervise.SuspendSource) bool {
	switch state {
	case "", supervise.StateFailed, supervise.StateRetired:
		return false
	case supervise.StateSuspended:
		return supervise.Claim{State: state, SuspendedBy: by}.Released()
	}
	return true
}

// ClaimEnded is whether a claim in state will not hold its role again: the daemon does not
// supervise it (""), or it failed or retired. A child's own architect that failed does start again,
// at its child's next phase start (workflow's reviveArchitect), but that launch's ready queues a
// catch-up of its own, so nothing held for the failed claim waits for it. A suspended claim has
// not ended: the operator's resume or deliver starts it again (`legion claims resume`), and so
// does the workflow's next start of its role for a claim the workflow suspended, which for a
// child's own architect is the child's next first phase start (workflow's StartFirstPhase), or,
// after its tree's linger, the re-admission's start of a child past admitted (admit's
// startMidPhaseMembers).
func ClaimEnded(state supervise.ClaimState) bool {
	return state == "" || state == supervise.StateFailed || state == supervise.StateRetired
}

// ArchitectRuns is the owner walk's runs for a reader in a transaction (ReadOwnerWalk): it reads
// in tx the architect claims of tree's issues in one statement (Store.RoleClaims), and answers
// whether a token's claim holds its role as the store records it (HoldsRole). A token the store
// holds no claim for holds no role, and neither does one of an issue outside tree, which the read
// did not cover. project is the claim project token (claim.ProjectToken); an issue whose key
// makes no claim token has no claim, so it is left out of the read.
func ArchitectRuns(ctx context.Context, store Store, tx pgx.Tx, project string, tree map[string]Issue) (func(claim.Token) bool, error) {
	tokens := make([]claim.Token, 0, len(tree))
	for key := range tree {
		if token, err := claim.NewToken(project, key, claim.RoleArchitect); err == nil {
			tokens = append(tokens, token)
		}
	}
	held, err := store.RoleClaims(ctx, tx, tokens)
	if err != nil {
		return nil, err
	}
	return func(token claim.Token) bool {
		c := held[token]
		return c != nil && HoldsRole(c.State, c.SuspendedBy)
	}, nil
}

// OwnerWalk is the owner walk over one tree as a transaction reads it (ReadOwnerWalk), for as
// many of the tree's issues as its reader asks about.
type OwnerWalk struct {
	project string
	tree    map[string]Issue
	runs    func(claim.Token) bool
}

// ReadOwnerWalk reads in tx, once, what the owner walk over treeKey's tree weighs: the tree's
// issues (Store.TreeIssues, as TreeByKey keeps them) and their architect claims as the store
// records them (ArchitectRuns). project is the claim project token (claim.ProjectToken).
func ReadOwnerWalk(ctx context.Context, store Store, tx pgx.Tx, project, treeKey string) (OwnerWalk, error) {
	members, err := store.TreeIssues(ctx, tx, treeKey)
	if err != nil {
		return OwnerWalk{}, err
	}
	tree := TreeByKey(members)
	runs, err := ArchitectRuns(ctx, store, tx, project, tree)
	if err != nil {
		return OwnerWalk{}, err
	}
	return OwnerWalk{project: project, tree: tree, runs: runs}, nil
}

// Owner is the key of the issue whose architect owns issue, an issue of the walk's tree
// (OwningIssue).
func (w OwnerWalk) Owner(issue Issue, fromParent bool) string {
	return OwningIssue(w.project, w.tree, issue, fromParent, w.runs)
}

// Architect is the architect claim that owns issue, an issue of the walk's tree (OwningArchitect).
func (w OwnerWalk) Architect(issue Issue, fromParent bool) (claim.Token, error) {
	return OwningArchitect(w.project, w.tree, issue, fromParent, w.runs)
}
