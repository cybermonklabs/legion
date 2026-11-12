package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// parseIssueParent decodes the tri-state `parent` field of an issue PATCH. Absent →
// (nil, false): leave it alone. JSON null → (nil, true): clear it. A string → its trimmed
// issue key, which must be non-empty. Anything else → 400 PARENT_INPUT.
func parseIssueParent(raw json.RawMessage) (parent *string, provided bool, err error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, true, nil
	}
	var key string
	if err := json.Unmarshal(raw, &key); err != nil {
		return nil, true, errorf(http.StatusBadRequest, "PARENT_INPUT", "parent must be an issue key or null")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, true, errorf(http.StatusBadRequest, "PARENT_INPUT", "parent must not be blank")
	}
	return &key, true, nil
}

// parentDepthCap is the most links an issue may sit below its root: a create or reparent that
// would put an issue deeper is refused (refuseParentTooDeep), so it bounds every recursive issue
// walk — the ancestor walk (refs.IssueAncestors, the one issueAncestors and the cycle refusal
// below read), the children subtree walk, the components and repository walks that climb a
// whole read's chains together, the architecture tree's containment walks, and the legion-label
// walk below (legionManagedStatusSQL) — like refs.Closure's depth cap: the queries terminate
// even if a raced reparent ever commits a cycle. It is refs.IssueDepthCap, where the ancestor
// walk lives so that docs and refs can read it too: one bound for every issue walk in the
// server.
// parentDepthCapSQL is the same number spliced into the walks built as SQL text.
const parentDepthCap = refs.IssueDepthCap

var parentDepthCapSQL = strconv.Itoa(parentDepthCap)

// legionLabel is the label that hands an issue's status to the Legion daemon. The daemon holds
// its own copy of the spelling (packages/daemon/internal/dispatch/types.go, LegionLabel): the two
// are separate constants in separate Go modules that have to be changed together, not one shared
// value. packages/envoy otherwise knows nothing about Legion, so this is the one Legion-specific
// rule Dispatch itself carries.
const legionLabel = "legion"

// legionManagedStatusSQL answers whether the Legion daemon owns the status of the issue keyed by
// $1: whether that issue, or any issue on its parent chain, carries the legion label. It lives
// here because it is one more parent-chain walk bounded by parentDepthCap, like the ones above.
// Labels keep the case they were typed in and are only de-duplicated case-insensitively, so
// `Legion` is a real stored value and the match is on lower(label). The walk starts at the issue
// itself, so one query answers both its own labels and its ancestors'. A chain deeper than the
// cap answers false. A claim never reaches that case - loadIssue refuses such a chain before this
// runs - but a write's advice does (computeWriteAdvice, write_advice.go), and there it means the
// issue is treated as one Legion does not run: the "still in triage" line prints. A chain that
// deep needs a reparent that refuseParentTooDeep would have refused.
var legionManagedStatusSQL = `
	with recursive chain as (
		select i.parent_key, i.labels, 0 as depth
		from issues i where i.key = $1
		union
		select p.parent_key, p.labels, c.depth + 1
		from issues p join chain c on p.key = c.parent_key
		where c.depth < ` + parentDepthCapSQL + `
	)
	select exists (
		select 1 from chain c, unnest(c.labels) as label where lower(label) = '` + legionLabel + `'
	)`

// legionManagedStatus runs legionManagedStatusSQL inside the caller's transaction. That keeps the
// issue's own row as the caller locked it, and nothing more: an ancestor's labels, and a reparent
// above the issue, are read as they stand and are not held still by any lock the caller takes. The
// answer is advisory in both callers, so a chain that moves under the walk costs a sentence of
// guidance and never a write. Both claim routes (issue_claim.go) and a session's write advice
// (write_advice.go) read it.
func legionManagedStatus(ctx context.Context, tx pgx.Tx, key string) (bool, error) {
	var managed bool
	if err := tx.QueryRow(ctx, legionManagedStatusSQL, key).Scan(&managed); err != nil {
		return false, fmt.Errorf("resolve whether Legion manages the status of %s: %w", key, err)
	}
	return managed, nil
}

func normalizeIssueBlockers(targets []string) ([]string, error) {
	if len(targets) > contracts.MaxIssueBlockers {
		return nil, countExceededError("BLOCKED_BY_INPUT", "blocked_by", len(targets), contracts.MaxIssueBlockers)
	}
	slices.Sort(targets)
	return slices.Compact(targets), nil
}

// lockProjectIssueDependencies serializes blocker and parent changes before any row or rank
// lock. The two-key namespace is separate from Dispatch's one-key rank, document and event locks.
func lockProjectIssueDependencies(ctx context.Context, tx pgx.Tx, project string) error {
	if _, err := tx.Exec(ctx, `
		select pg_advisory_xact_lock(hashtext('dispatch-issue-dependencies'), hashtext($1))
	`, project); err != nil {
		return fmt.Errorf("lock project issue dependencies: %w", err)
	}
	return nil
}

// lockDeploymentIssueDependencies is the one dependency lock that is not a project's, and the
// first every dependency write takes: it is what two writers that share no project meet on. A
// write whose dependency component spans projects takes it exclusively, one whose component is
// inside a single project takes it shared, so the writes that stay inside one project conflict
// with nothing but a cross-project write and run beside each other as they always did.
//
// The key is its own name rather than lockProjectIssueDependencies' namespace with a literal
// second key, so a project whose key happens to hash to that literal cannot collide with it; the
// two-key shape with a literal is the one refuseRepoRenameChain already uses for a
// deployment-wide lock.
func lockDeploymentIssueDependencies(ctx context.Context, tx pgx.Tx, exclusive bool) error {
	statement := `select pg_advisory_xact_lock_shared(hashtext('dispatch-issue-dependencies-deployment'), 0)`
	if exclusive {
		statement = `select pg_advisory_xact_lock(hashtext('dispatch-issue-dependencies-deployment'), 0)`
	}
	if _, err := tx.Exec(ctx, statement); err != nil {
		return fmt.Errorf("lock deployment issue dependencies: %w", err)
	}
	return nil
}

// lockIssueDependencyWrite takes every lock a blocker or parent write needs, in the one order
// every such write takes them: the deployment-wide dependency lock first and always, then the
// project lock of the issue's own project and of each project a target of this write belongs to,
// in key order. project is the issue's own project, targets are its blocked_by targets, and ends
// are the write's other ends — the issue itself where it already has a row, and a proposed
// parent; an empty end is skipped. The number, rank and row locks follow, as they did before.
//
// Both reads it makes run with no lock held. Reading a target's project unlocked is sound
// because no route moves an issue between projects (project_scope.go's file doc), and a key
// created between that read and the lock is still refused by writeBlockedBy's own read inside
// the transaction. The second read is a probe, and answers one question: does the component this
// write joins touch a project other than the issue's own? That is what decides the deployment
// lock, and the decision holds even when the probe is stale:
//
//   - A write that can close a ring across projects has a component that spans projects, since
//     the whole of such a ring is reachable from the write's own ends, so it holds the exclusive
//     lock and two such writes never run at once.
//   - A write whose component is inside one project can close a ring inside that project alone,
//     and the project lock serializes those, as it does today.
//   - A probe that saw one project where the component spans by the time the locks are held can
//     only have become stale through a write that held the exclusive lock and committed before
//     this write's shared lock was granted. While this write holds the shared lock no spanning
//     write can run, and a write whose own component is inside one project cannot add an edge of
//     a ring that crosses projects, because such an edge would make its own component span.
//
// What it costs is one unlocked walk per dependency write, bounded by parentDepthCap as every
// issue walk is.
func lockIssueDependencyWrite(ctx context.Context, tx pgx.Tx, project string, targets []string, ends ...string) error {
	foreign, err := foreignTargetProjects(ctx, tx, project, targets)
	if err != nil {
		return err
	}
	spans := len(foreign) > 0
	if !spans {
		probe := make([]string, 0, len(ends)+len(targets))
		probe = append(probe, ends...)
		probe = append(probe, targets...)
		if spans, err = dependencyComponentLeavesProject(ctx, tx, project, probe); err != nil {
			return err
		}
	}
	if err := lockDeploymentIssueDependencies(ctx, tx, spans); err != nil {
		return err
	}
	projects := append(foreign, project)
	slices.Sort(projects)
	for _, locked := range slices.Compact(projects) {
		if err := lockProjectIssueDependencies(ctx, tx, locked); err != nil {
			return err
		}
	}
	return nil
}

// foreignTargetProjects is the project of every target belonging to one other than project, each
// named once. A target with no row is left out: writeBlockedBy's own read refuses it inside the
// transaction, which is where an unknown key is answered.
func foreignTargetProjects(ctx context.Context, q queryer, project string, targets []string) ([]string, error) {
	if len(targets) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `
		select distinct project_key from issues where key = any($1) and project_key <> $2
	`, targets, project)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// dependencyComponentLeavesProject answers whether the wait graph reachable from ends holds an
// issue of any project but project. It walks waitStepSQL, the step assertNoWaitPath walks, from
// both halves of each end, and stops at parentDepthCap as that walk does: a graph deeper than
// the cap only decides which deployment lock the refusal assertNoWaitPath then writes is taken
// under.
func dependencyComponentLeavesProject(ctx context.Context, q queryer, project string, ends []string) (bool, error) {
	seen := map[waitNode]bool{}
	reached := map[string]bool{}
	var visited []string
	var frontier []waitNode
	for _, end := range ends {
		if end != "" {
			frontier = append(frontier, waitNode{key: end, done: true})
		}
	}
	for depth := 0; len(frontier) > 0 && depth <= parentDepthCap; depth++ {
		var starts, dones []string
		for _, node := range frontier {
			halves := []waitNode{node, {key: node.key}}
			if !node.done {
				halves = halves[:1]
			}
			for _, half := range halves {
				if seen[half] {
					continue
				}
				seen[half] = true
				if !reached[half.key] {
					reached[half.key] = true
					visited = append(visited, half.key)
				}
				if half.done {
					dones = append(dones, half.key)
				} else {
					starts = append(starts, half.key)
				}
			}
		}
		// Every half of this round was already seen, so the step below would be asked for the
		// waits of nothing and could only answer nothing: the walk has exhausted its component.
		if len(starts) == 0 && len(dones) == 0 {
			break
		}
		rows, err := q.Query(ctx, waitStepSQL, starts, dones)
		if err != nil {
			return false, err
		}
		frontier, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (waitNode, error) {
			var node waitNode
			return node, row.Scan(&node.key, &node.done)
		})
		if err != nil {
			return false, err
		}
	}
	if len(visited) == 0 {
		return false, nil
	}
	var leaves bool
	err := q.QueryRow(ctx, `
		select exists (select 1 from issues where key = any($1) and project_key <> $2)
	`, visited, project).Scan(&leaves)
	return leaves, err
}

// lockIssueRows locks key and the rows in others `for no key update` in one key order, the order
// LockOwners takes event owners, and answers key's parent as that lock read it. A PATCH locks
// every row it names through one call, so two writes naming the same rows queue one behind the
// other instead of each holding one row while waiting for the other. A key with no row is
// skipped; the caller's own read of that key refuses it.
func lockIssueRows(ctx context.Context, q queryer, key string, others ...string) (*string, error) {
	keys := append(make([]string, 0, len(others)+1), key)
	keys = append(keys, others...)
	slices.Sort(keys)
	var keyParent *string
	for index, row := range keys {
		if index > 0 && row == keys[index-1] {
			continue
		}
		var parent *string
		err := q.QueryRow(ctx, `select parent_key from issues where key = $1 for no key update`, row).Scan(&parent)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if row == key {
			keyParent = parent
		}
	}
	return keyParent, nil
}

// parentContended is the code of a PATCH whose issue's parent a concurrent reparent moved at
// every lock attempt. The Legion daemon retries a status write it refuses
// (packages/daemon/internal/dispatch/types.go:113, ParentContended) rather than dropping it, so
// the two spellings change together: no mechanism shares a value between the two Go modules.
const parentContended = "PARENT_CONTENDED"

// parentLockAttempts bounds how many times lockPatchRowsAndParent reads the issue's parent and
// locks it: the first, and one redo for a parent a concurrent reparent changed in between.
const parentLockAttempts = 2

// lockPatchRows locks the issue and the rows in named (a proposed parent, rank neighbours) in one
// key order, for a PATCH that appends no event to the issue's current parent.
func lockPatchRows(ctx context.Context, tx pgx.Tx, key string, named ...string) error {
	_, err := lockIssueRows(ctx, tx, key, named...)
	return err
}

// lockPatchRowsAndParent is lockPatchRows for a PATCH that appends an event to the issue's
// current parent (a status change, a reparent), which it locks in the same key order. The parent
// can only be read before the lock, and a concurrent reparent of the issue, which holds the
// issue's row, can change it in between, so the lock reads it again. A parent that moved releases
// every lock the attempt took (a savepoint) and the attempt starts over with the parent that
// stands, so no row is ever locked out of key order. A parent that moves at every attempt is
// 409 PARENT_CONTENDED, with nothing applied.
//
// It does not use resolveThenLock, whose resolve half runs with nothing held and whose write half
// owns its own transaction: both halves here run inside the PATCH's transaction, after its
// advisory locks, so the locks they take commit with the PATCH's writes.
func lockPatchRowsAndParent(ctx context.Context, tx pgx.Tx, key string, named ...string) error {
	rows := make([]string, 0, len(named)+1)
	for range parentLockAttempts {
		var parent *string
		if err := tx.QueryRow(ctx, `select parent_key from issues where key = $1`, key).Scan(&parent); err != nil {
			return err
		}
		rows = append(rows[:0], named...)
		if parent != nil {
			rows = append(rows, *parent)
		}
		attempt, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		locked, err := lockIssueRows(ctx, attempt, key, rows...)
		if err != nil {
			_ = attempt.Rollback(ctx)
			return err
		}
		if stringPointersEqual(parent, locked) {
			return attempt.Commit(ctx)
		}
		if err := attempt.Rollback(ctx); err != nil {
			return err
		}
	}
	return errorf(http.StatusConflict, parentContended,
		"%s's parent changed at each of %d attempts while this request waited to lock it, so nothing was applied: read the issue and retry", key, parentLockAttempts)
}

// validateIssueParent reads the ancestor chain while the project's dependency lock is held, and
// refuses a parent that would make key its own ancestor or put key's subtree past parentDepthCap.
func validateIssueParent(ctx context.Context, tx pgx.Tx, key, project, parent string) error {
	if parent == key {
		return errorf(http.StatusBadRequest, "PARENT_INPUT", "an issue cannot be its own parent")
	}
	var parentProject string
	err := tx.QueryRow(ctx, `select project_key from issues where key = $1`, parent).Scan(&parentProject)
	if errors.Is(err, pgx.ErrNoRows) {
		return errorf(http.StatusBadRequest, "PARENT_INPUT", "parent issue %s not found", parent)
	}
	if err != nil {
		return err
	}
	if parentProject != project {
		return errorf(http.StatusBadRequest, "PARENT_INPUT", "parent must be in the same project")
	}
	// parent and the issues above it, parent first: the server's one ancestor walk
	// (refs.IssueAncestors) with parent itself at its head, since a parent key already stands
	// above would make a cycle, as parent being key would.
	above, err := refs.IssueAncestors(ctx, tx, parent)
	if err != nil {
		return err
	}
	chain := append([]string{parent}, above...)
	for index, ancestor := range chain {
		if ancestor == key {
			path := key + " → " + strings.Join(chain[:index+1], " → ")
			return errorf(http.StatusConflict, "PARENT_INPUT", "would create a cycle: %s", path)
		}
	}
	return refuseParentTooDeep(ctx, tx, key, parent)
}

// refuseParentTooDeep refuses a parent link that would put an issue, or the deepest issue under
// it, more than parentDepthCap links below its root: 409 PARENT_INPUT. child is the issue being
// moved, or "" for one being created, which has nothing under it. The caller holds the project's
// dependency lock (lockProjectIssueDependencies), which every create with a parent and every
// reparent takes, so two writes cannot each pass alone and together pass the cap. Both walks stop
// one link past the cap, so a chain that loops or is already too deep ends them.
func refuseParentTooDeep(ctx context.Context, tx pgx.Tx, child, parent string) error {
	var above, below int
	if err := tx.QueryRow(ctx, `
		with recursive up as (
			select parent_key, 1 as depth from issues where key = $1
			union all
			select i.parent_key, u.depth + 1
			from issues i join up u on i.key = u.parent_key
			where u.depth <= $3
		), down as (
			select key, 0 as depth from issues where key = $2
			union all
			select i.key, d.depth + 1
			from issues i join down d on i.parent_key = d.key
			where d.depth <= $3
		)
		select coalesce((select max(depth) from up), 0), coalesce((select max(depth) from down), 0)
	`, parent, child, parentDepthCap).Scan(&above, &below); err != nil {
		return err
	}
	if above+below <= parentDepthCap {
		return nil
	}
	depth := strconv.Itoa(above+below) + " links"
	if above > parentDepthCap || below > parentDepthCap {
		// A walk stopped at its bound, so the chain is deeper than it read.
		depth = "more than " + parentDepthCapSQL + " links"
	}
	if child == "" {
		return errorf(http.StatusConflict, "PARENT_INPUT",
			"an issue under %s would sit %s below its root; an issue sits at most %d links below its root", parent, depth, parentDepthCap)
	}
	return errorf(http.StatusConflict, "PARENT_INPUT",
		"moving %s under %s would put the deepest issue under it %s below its root; an issue sits at most %d links below its root", child, parent, depth, parentDepthCap)
}

// waitNode is one half of an issue in the dependency graph. An issue starts after its blocked_by
// targets are done and after its parent starts; it is done after it starts and after its children
// are done. Splitting the two halves lets sibling dependencies stand while still refusing a child
// that waits on its parent, or a parent that waits on its descendant.
type waitNode struct {
	key  string
	done bool
}

// assertParentLeavesNoDependencyCycle checks a parent link already written in tx. The link adds
// two waits: the parent's done waits on the child's done, and the child's start waits on the
// parent's start.
func assertParentLeavesNoDependencyCycle(ctx context.Context, tx pgx.Tx, child, parent string) error {
	if err := assertNoWaitPath(ctx, tx, []waitNode{{key: child, done: true}}, waitNode{key: parent, done: true}); err != nil {
		return err
	}
	return assertNoWaitPath(ctx, tx, []waitNode{{key: parent}}, waitNode{key: child})
}

// writeBlockedBy validates and replaces the normalized target list. A target may be an issue of
// another project: an issue that waits on work tracked elsewhere has to be able to record that
// wait, and the walk below is what refuses a ring, between projects as inside one.
//
// caller is the one project the request's credential may write, empty for a credential that
// names none (callerProject). Such a caller is refused a target outside its project here rather
// than in the pre-handler check, because that check reads the project of the object the path
// names and no key in the body (project_scope.go): this is the first point at which a target's
// project is known, and the write has still made no change.
func writeBlockedBy(ctx context.Context, tx pgx.Tx, issue, project, caller string, targets []string) error {
	if slices.Contains(targets, issue) {
		return errorf(http.StatusBadRequest, "BLOCKED_BY_INPUT", "%s cannot be blocked by itself", issue)
	}
	for _, target := range targets {
		var targetProject string
		err := tx.QueryRow(ctx, `select project_key from issues where key = $1`, target).Scan(&targetProject)
		if errors.Is(err, pgx.ErrNoRows) {
			return errorf(http.StatusBadRequest, "BLOCKED_BY_INPUT", "blocked_by issue %s not found", target)
		}
		if err != nil {
			return err
		}
		if targetProject != project && caller != "" {
			return errorf(http.StatusForbidden, codeProjectScope,
				"this request's credential may write project %s; blocked_by issue %s belongs to project %s",
				caller, target, targetProject)
		}
	}
	from := make([]waitNode, len(targets))
	for index, target := range targets {
		from[index] = waitNode{key: target, done: true}
	}
	if err := assertNoWaitPath(ctx, tx, from, waitNode{key: issue}); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `delete from issue_links where issue_key = $1 and kind = 'blocked_by'`, issue); err != nil {
		return err
	}
	if len(targets) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
		insert into issue_links (issue_key, kind, target_key)
		select $1, 'blocked_by', target_key from unnest($2::text[]) as target_key
	`, issue, targets)
	return err
}

// waitStepSQL is one step of the wait graph: what the start halves named in $1 wait on, and what
// the done halves named in $2 wait on. assertNoWaitPath walks it to refuse a ring and
// dependencyComponentLeavesProject walks it to see which projects a write's component reaches,
// so both read one definition of an edge.
const waitStepSQL = `
			select parent_key, false from issues where key = any($1) and parent_key is not null
			union
			select target_key, true from issue_links where issue_key = any($1) and kind = 'blocked_by'
			union
			select key, true from issues where parent_key = any($2)
		`

// assertNoWaitPath refuses a wait whose head reaches its tail, stopping at parentDepthCap.
// The caller serializes blocker and parent changes, so traversal needs only plain reads.
func assertNoWaitPath(ctx context.Context, tx pgx.Tx, from []waitNode, goal waitNode) error {
	seen := map[waitNode]bool{}
	frontier := from
	for depth := 0; len(frontier) > 0; depth++ {
		if depth > parentDepthCap {
			return errorf(http.StatusConflict, "DEPENDENCY_CYCLE", "the dependency graph is deeper than %d links", parentDepthCap)
		}
		var starts, dones []string
		for _, node := range frontier {
			halves := []waitNode{node, {key: node.key}}
			if !node.done {
				halves = halves[:1]
			}
			for _, half := range halves {
				if half == goal {
					return errorf(http.StatusConflict, "DEPENDENCY_CYCLE", "%s would wait on itself through blocked_by and parent links", goal.key)
				}
				if seen[half] {
					continue
				}
				seen[half] = true
				if half.done {
					dones = append(dones, half.key)
				} else {
					starts = append(starts, half.key)
				}
			}
		}
		rows, err := tx.Query(ctx, waitStepSQL, starts, dones)
		if err != nil {
			return err
		}
		frontier, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (waitNode, error) {
			var node waitNode
			return node, row.Scan(&node.key, &node.done)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func stringPointersEqual(left, right *string) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}
