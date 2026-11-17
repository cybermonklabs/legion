package record

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// OwningArchitect is the architect claim that owns a notice of kind, about role's claim, on issue:
// the claim of the issue the owner walk ends at (OwningIssue). It fails when that issue's key, or
// project, makes no claim token.
func OwningArchitect(project string, tree map[string]Issue, issue Issue, kind NoticeKind, role claim.Role, runs func(claim.Token) bool) (claim.Token, error) {
	return claim.NewToken(project, OwningIssue(project, tree, issue, kind, role, runs), claim.RoleArchitect)
}

// OwningIssue is the key of the issue whose architect owns a notice of kind, about role's claim,
// on issue: the nearest issue at or above it, through its parents, whose architect claim holds its
// role (runs), else the tree root (issue.Tree), whose architect is the tree's own claim. The daemon
// starts a child's architect with the child's first phase, and starts a failed one again at the
// child's next phase start. While a child's architect does not hold its role, which HoldsRole
// decides (and the daemon's holdsRole over the machine's live view), the child's notices go on up
// to the architect above it.
//
// Two notices start the walk at the issue's parent instead. A child's close or status change: it
// tells of the child's own run starting, leaving, coming back or closing, which is not the child's
// own architect to judge (a leave suspends that architect and a close as done retires it, in
// workflow's leave), and the architect above it decides what the rest of the tree does. And a
// worker-died or a worker-stalled about the architect of an issue that is not its tree's root
// (role is claim.RoleArchitect): the notice is about that architect, so the architect above it
// must hear it. Either notice on an issue with no parent goes to the tree root, never to the
// issue's own architect: a failed claim is passed over by the walk anyway, but a stalled one still
// holds its role and would be told of its own stall, and the outbox can publish a child's close or
// status change while the child's own architect still holds its role, before its suspension or
// close lands. A walk that starts at the parent never comes back to the issue itself: a record
// whose parents loop back to it ends the walk at the tree root.
//
// The walk stays inside the issue's tree: admission records a Dispatch re-parent as it comes and
// leaves the issue's tree as it was (admit's recordObservation), except a root that never ran,
// which joins its parent's live tree (workflow/adopt.go), so a parent that is not an issue of
// tree — not recorded, or recorded in another tree — ends the walk at the tree root.
//
// project is the claim project token (claim.ProjectToken), and tree is the tree's issues by key
// (TreeByKey). runs is the reader's own view of whether an architect's claim holds its role: the
// daemon's outbox reads the machine's live view, which also knows a stop in flight, and a reader
// in a transaction reads the claims the store records (ArchitectRuns). An issue the walk meets
// whose key, or project, makes no claim token ends the walk there, since it has no claim to ask
// runs about; OwningArchitect then fails on it.
func OwningIssue(project string, tree map[string]Issue, issue Issue, kind NoticeKind, role claim.Role, runs func(claim.Token) bool) string {
	current := issue
	seen := map[string]bool{}
	aboutItsArchitect := role == claim.RoleArchitect && (kind == "worker-died" || kind == "worker-stalled")
	if aboutItsArchitect || kind == "child-closed" || kind == "child-status" {
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
// architect and owns nothing, so a walk that meets it ends at the tree root.
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

// ArchitectRuns is OwningArchitect's runs for a reader in a transaction: it reads in tx the
// architect claims of tree's issues in one statement (Store.RoleClaims), and answers whether a
// token's claim holds its role as the store records it (HoldsRole). A token the store holds no
// claim for holds no role, and neither does one of an issue outside tree, which the read did not
// cover. project is the claim project token (claim.ProjectToken); an issue whose key makes no
// claim token has no claim, so it is left out of the read.
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
