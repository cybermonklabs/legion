package record

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The members that a root's departure left suspended under the release before the one that
// stores no tree, which the first boot on this release brings back (item 46 of
// dispatch://INFRA-4730; R20 and R21 of dispatch://INFRA-3320). The release before suspended a
// root's whole tree when the root left Legion's work: the leave (workflow's beginLinger at main
// 4e851eb1) queued a suspend of every claim of every issue of the tree, which its outbox ran as a
// suspension by the daemon (supervise.SuspendedByDaemon). A return of the root brought its members
// back only at the promotion that opened its tree again, and the root carried a mark until then
// (issues.members_owed). This release keeps no tree: a root's leave stops only its own claims, and
// its return resumes only its own phase (R20). So nothing would bring such a member back.
//
// ReadMembersBack is the one reading of them. The conversion's step that brings them back calls it
// (the daemon's bringMembersBack), and so does `legion members-back`, which an operator runs on the
// store of the release a project's daemon runs before the upgrade. That release may be as old as
// legion-v22.16.2. So it reads only columns that legion-v22.16.2's schema holds: issues.key, tree,
// project, phase, held_from, status, generation, linger_until and released_for_wait_at;
// slots.issue; claims.project, issue, role, state and suspended_by; pull_requests.issue and state;
// tree_lifecycles.project, tree, authority and cleanup_confirmed_at; handovers.issue; and
// dispatch_claims.key and claimed_by. The one column it reads that is newer,
// issues.members_owed, it reads only where the store holds it (membersOwedRead). The steps of the
// conversion before it change none of these.

// MemberBack is one member that the first boot brings back: its key, the phase its record stands
// in, where it starts again, its tree's root as the release before stored it (issues.tree), and how
// that tree left Legion's work.
type MemberBack struct {
	Key   string
	Phase phase.Phase
	Root  string
	Left  TreeLeft
}

// TreeLeft is how the tree of a member that the first boot brings back left Legion's work, read
// from its root's records (membersBackWorld.treeLeft): TreeLeftMerge, TreeLeftOperator,
// TreeLeftFailure or TreeLeftReturned, or else the root's own status out of Legion's work
// (backlog, icebox, triage or done), which a person, the controller or a close wrote.
type TreeLeft string

const (
	// TreeLeftMerge is a root closed at the merge of its pull request.
	TreeLeftMerge TreeLeft = "merge"
	// TreeLeftOperator is a root whose tree the operator holds, or a claim of which the operator
	// suspended.
	TreeLeftOperator TreeLeft = "operator"
	// TreeLeftFailure is a root a claim of which failed.
	TreeLeftFailure TreeLeft = "failure"
	// TreeLeftReturned is a root back in Legion's work whose members the release before had still
	// to bring back (issues.members_owed).
	TreeLeftReturned TreeLeft = "returned"
)

// ReadMembersBack is, in tx, each member of project (a Dispatch project key) that the first boot
// brings back, in key order (membersBack).
func ReadMembersBack(ctx context.Context, tx pgx.Tx, project string) ([]MemberBack, error) {
	world, err := readMembersBackWorld(ctx, tx, project)
	if err != nil {
		return nil, fmt.Errorf("read the members the first boot of %s brings back: %w", project, err)
	}
	return membersBack(world), nil
}

// membersBack is each issue of world that the first boot brings back, in key order. It is such a
// member when all of these hold:
//
//   - it is in Legion's work (InLegionsWork), and its own status therefore is not one a person
//     stopped it with;
//   - its record stores another issue's key as its tree, and the daemon records that root;
//   - that root left Legion's work, or it is back in work and still owes its members
//     (issues.members_owed);
//   - it stands in a phase past admitted that a role works (phase.RoleFor of StandsAt): admitted
//     has run nothing in its run, and its first phase is the admission line's to start;
//   - the claim of that role is suspended by the daemon, the source that the release before's
//     suspend of a tree wrote, or by the unknown source, which migration 0038 wrote on every claim
//     already suspended before the source existed. A claim the operator suspended holds until the
//     operator resumes it, and a claim the idle release suspended comes back at the next wake of
//     its role;
//   - no hand-over of the release before is in flight for it: the conversion's next step decides
//     such an issue (the daemon's convertHandOvers);
//   - no person holds its Dispatch claim (PersonActor): Legion never resumes work another owner
//     took.
func membersBack(world membersBackWorld) []MemberBack {
	byKey := make(map[string]backIssue, len(world.issues))
	for _, issue := range world.issues {
		byKey[issue.key] = issue
	}
	var members []MemberBack
	for _, member := range world.issues {
		root, recorded := byKey[member.tree]
		if member.key == member.tree || !recorded || !member.inWork() || (root.inWork() && !root.membersOwed) {
			continue
		}
		if world.handedOver[member.key] || PersonActor(world.claimedBy[member.key]) {
			continue
		}
		at := member.standsAt()
		role := phase.RoleFor(at)
		if at == phase.Admitted || role == "" {
			continue
		}
		held, ok := world.claims[roleOf{issue: member.key, role: role}]
		if !ok || held.state != supervise.StateSuspended || (held.by != supervise.SuspendedByDaemon && held.by != supervise.SuspendedByUnknown) {
			continue
		}
		members = append(members, MemberBack{Key: member.key, Phase: member.phase, Root: root.key, Left: world.treeLeft(root)})
	}
	slices.SortFunc(members, func(a, b MemberBack) int { return cmp.Compare(a.Key, b.Key) })
	return members
}

// treeLeft is how root's tree left Legion's work, read in this order: a close at its pull
// request's merge, the operator's hold of its tree or the operator's suspension of a claim of it,
// a failed claim of it, its own status out of Legion's work, and else its return with its members
// still owed.
func (w membersBackWorld) treeLeft(root backIssue) TreeLeft {
	operator, failed := w.operator[root.key], false
	for _, role := range claim.Roles {
		held, ok := w.claims[roleOf{issue: root.key, role: role}]
		operator = operator || (ok && held.state == supervise.StateSuspended && held.by == supervise.SuspendedByOperator)
		failed = failed || (ok && held.state == supervise.StateFailed)
	}
	switch {
	case root.phase == phase.Done && root.status == "done" && w.merged[root.key]:
		return TreeLeftMerge
	case operator:
		return TreeLeftOperator
	case failed:
		return TreeLeftFailure
	case !root.inWork():
		return TreeLeft(root.status)
	}
	return TreeLeftReturned
}

// membersBackWorld is what membersBack reads of a project's store, as readMembersBackWorld reads
// it: every issue, each claim by its issue and role, the issues whose pull request merged, the
// trees the operator holds, the issues with a hand-over in flight, and who Dispatch shows holding
// each issue's claim.
type membersBackWorld struct {
	issues     []backIssue
	claims     map[roleOf]backClaim
	merged     map[string]bool
	operator   map[string]bool
	handedOver map[string]bool
	claimedBy  map[string]string
}

// backIssue is the part of an issue's record that membersBack reads.
type backIssue struct {
	key, tree, status string
	phase, heldFrom   phase.Phase
	membersOwed       bool
}

// inWork is InLegionsWork over the issue's phase and status.
func (i backIssue) inWork() bool { return InLegionsWork(Issue{Phase: i.phase, Status: i.status}) }

// standsAt is StandsAt over the issue's phase and the phase it is held from.
func (i backIssue) standsAt() phase.Phase {
	issue := Issue{Phase: i.phase}
	if i.heldFrom != "" {
		issue.Hold = &Hold{From: i.heldFrom}
	}
	return StandsAt(issue)
}

// roleOf names a claim by its issue and its role.
type roleOf struct {
	issue string
	role  claim.Role
}

// backClaim is the part of a claim that membersBack reads.
type backClaim struct {
	state supervise.ClaimState
	by    supervise.SuspendSource
}

// membersOwedRead is what readMembersBackWorld reads as a root's issues.members_owed. Where the
// store holds no such column, which a release before #5303 leaves, it is the statement with which
// that column's migration marks every root at the upgrade
// (migrations/20261123224717_members_owed.up.sql): a root of its own tree at todo and admitted on a
// run past its first, which no promotion has opened, holds no slot and has no wait's release and
// no linger. The upgrade runs that migration before the first boot's conversion reads the column,
// so the listing a release before #5303 is read for names the members that boot brings back.
const membersOwedRead = `(key = tree and generation > 1 and status = 'todo' and phase = 'admitted' and linger_until is null
	and released_for_wait_at is null and not exists (select 1 from slots where slots.issue = issues.key))`

// readMembersBackWorld reads membersBackWorld of project in tx, in seven queries whatever the
// number of issues, each naming only columns of legion-v22.16.2's schema but members_owed, which
// it names only where the store holds it.
func readMembersBackWorld(ctx context.Context, tx pgx.Tx, project string) (membersBackWorld, error) {
	token, err := claim.ProjectToken(project)
	if err != nil {
		return membersBackWorld{}, err
	}
	var kept bool
	if err := tx.QueryRow(ctx, `select exists (select 1 from information_schema.columns where table_schema = current_schema()
		and table_name = 'issues' and column_name = 'members_owed')`).Scan(&kept); err != nil {
		return membersBackWorld{}, err
	}
	owed := membersOwedRead
	if kept {
		owed = "members_owed"
	}
	world := membersBackWorld{claims: map[roleOf]backClaim{}}
	rows, err := tx.Query(ctx, `select key, tree, phase, coalesce(held_from, ''), status, `+owed+` from issues
		where project = $1`, project)
	if err != nil {
		return membersBackWorld{}, err
	}
	if world.issues, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (backIssue, error) {
		var issue backIssue
		var at, from string
		err := row.Scan(&issue.key, &issue.tree, &at, &from, &issue.status, &issue.membersOwed)
		issue.phase, issue.heldFrom = phase.Phase(at), phase.Phase(from)
		return issue, err
	}); err != nil {
		return membersBackWorld{}, err
	}
	rows, err = tx.Query(ctx, `select issue, role, state, suspended_by from claims where project = $1 and issue <> ''`, token)
	if err != nil {
		return membersBackWorld{}, err
	}
	if _, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct{}, error) {
		var issue, role, state, by string
		err := row.Scan(&issue, &role, &state, &by)
		world.claims[roleOf{issue: issue, role: claim.Role(role)}] = backClaim{state: supervise.ClaimState(state), by: supervise.SuspendSource(by)}
		return struct{}{}, err
	}); err != nil {
		return membersBackWorld{}, err
	}
	for _, set := range []struct {
		into  *map[string]bool
		query string
		arg   string
	}{
		{&world.merged, `select p.issue from pull_requests p join issues i on i.key = p.issue where i.project = $1 and p.state = 'merged'`, project},
		{&world.operator, `select tree from tree_lifecycles where project = $1 and authority = 'operator' and cleanup_confirmed_at is null`, token},
		{&world.handedOver, `select h.issue from handovers h join issues i on i.key = h.issue where i.project = $1`, project},
	} {
		rows, err := tx.Query(ctx, set.query, set.arg)
		if err != nil {
			return membersBackWorld{}, err
		}
		keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return membersBackWorld{}, err
		}
		*set.into = make(map[string]bool, len(keys))
		for _, key := range keys {
			(*set.into)[key] = true
		}
	}
	rows, err = tx.Query(ctx, `select c.key, c.claimed_by from dispatch_claims c join issues i on i.key = c.key where i.project = $1`, project)
	if err != nil {
		return membersBackWorld{}, err
	}
	world.claimedBy = map[string]string{}
	if _, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct{}, error) {
		var key, holder string
		err := row.Scan(&key, &holder)
		world.claimedBy[key] = holder
		return struct{}{}, err
	}); err != nil {
		return membersBackWorld{}, err
	}
	return world, nil
}
