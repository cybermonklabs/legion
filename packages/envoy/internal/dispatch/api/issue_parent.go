package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
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
		return nil, true, errorf(apicode.ParentInput, "parent must be an issue key or null")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, true, errorf(apicode.ParentInput, "parent must not be blank")
	}
	return &key, true, nil
}

// parentDepthCap is the most links an issue may sit below its root: a create or reparent that
// would put an issue deeper is refused (refuseParentTooDeep), so it bounds every recursive issue
// walk — the ancestor walk (refs.IssueAncestors, the one issueAncestors and the cycle refusal
// below read), the children subtree walk, the components and repository walks that climb a
// whole read's chains together, the architecture tree's containment walks, and the legion-label
// walk below (legionManagedStatusSQL) — so the queries terminate even if a raced reparent ever
// commits a cycle. It is refs.IssueDepthCap, where the ancestor walk lives so that docs and refs
// can read it too: one bound for every issue walk in the server.
// parentDepthCapSQL is the same number spliced into the walks built as SQL text.
const parentDepthCap = refs.IssueDepthCap

var parentDepthCapSQL = strconv.Itoa(parentDepthCap)

// legionLabel is the label that hands an issue's status to the Legion daemon. The daemon holds
// its own copy of the spelling (packages/daemon/internal/dispatch/types.go, LegionLabel): the two
// are separate constants in separate Go modules that have to be changed together, not one shared
// value. This label also selects the issues whose spec and Verification writers are checked
// (spec_writer.go). The daemon publishes a separate design-gate record (issue_design_gates.go)
// for the advice that names which gates a document write made stale.
// With DISPATCH_LEGION_STATUS_SCOPE on, a credential needs the person scope or the legion-status
// scope to write the status of an issue that this label makes a Legion issue, to take this label
// off an issue, or to move a Legion issue out from under every labelled issue, but for the status
// write of the architect of an issue above.
const legionLabel = "legion"

// legionManagedStatusSQL answers whether the Legion daemon owns the status of the issue keyed by
// $1: whether that issue, or any issue of its own project on its parent chain, carries the legion
// label. The walk climbs only to a parent of the issue's project, so it ends at the first
// ancestor of another project as at a root: the label hands an issue to its own project's daemon,
// and a parent of another project passes on none. It lives here because it is one more
// parent-chain walk bounded by parentDepthCap, like the ones above.
// Labels keep the case they were typed in and are only de-duplicated where strings.EqualFold
// matches them (labelKey), so `Legion` is a real stored value and the match is on lower(label)
// in the collation "C". In "C", lower() changes only A-Z, which for the six ASCII letters of
// legion is exactly what strings.EqualFold matches, so the walk and holdsLegionLabel agree on
// every database. In a glibc collation such as en_US.UTF-8, lower() also changes U+0130 (İ) to
// i, and the walk would read LEGİON as legion. The walk starts at the issue itself, so one query
// answers both its own labels and its ancestors'. A chain deeper than the cap answers false. A
// claim never reaches that case - loadIssue refuses such a chain before this runs - but a write's
// advice does (computeWriteAdvice, write_advice.go), and there it means the issue is treated as
// one Legion does not run: the "still in triage" line prints. A chain that deep needs a reparent
// that refuseParentTooDeep would have refused.
var legionManagedStatusSQL = legionWalkSQL(`select i.key, i.parent_key, i.labels, i.project_key, 0 as depth from issues i where i.key = $1`)

// legionManagedStatusOfRowSQL is legionManagedStatusSQL walking from an issue row given rather
// than read - $1 its parent_key, $2 its labels, $3 its project - for a write that reads its
// advice before it writes that row (writeAdviceBefore). The walk above it, and the depth cap,
// are the same.
var legionManagedStatusOfRowSQL = legionWalkSQL(`select null::text as key, $1::text as parent_key, $2::text[] as labels, $3::text as project_key, 0 as depth`)

// legionManagedStatusWithLabelsSQL is legionManagedStatusSQL that answers the stored labels and
// project of the issue keyed by $1 too, in the same statement: its labels, its project, then
// whether it is a Legion issue. R26's refusal reads all three from the row as it is stored under
// the caller's owner locks (legion_status_scope.go).
var legionManagedStatusWithLabelsSQL = `select i.labels, i.project_key, (` + legionManagedStatusSQL + `) from issues i where i.key = $1`

// issueChainSQL builds the chain used by the label, status-writer and document-writer checks.
// start selects the key, parent, labels, project and depth of the subject row. The chain stays
// in the subject's project and stops at parentDepthCap links. Set the same collation on both
// recursive terms: the seed can take labels from a parameter with a different collation.
func issueChainSQL(start string) string {
	return `chain as (
		select s.key, s.parent_key, s.labels collate "C" as labels, s.project_key, s.depth from (
			` + start + `
		) s
		union
		select p.key, p.parent_key, p.labels collate "C", c.project_key, c.depth + 1
		from issues p join chain c on p.key = c.parent_key and p.project_key = c.project_key
		where c.depth < ` + parentDepthCapSQL + `
	)`
}

// legionWalkSQL answers whether the row start selects or an ancestor carries the legion label.
func legionWalkSQL(start string) string {
	return `with recursive ` + issueChainSQL(start) + ` select ` + legionOnChainSQL
}

// legionLabelSQL is the match of one label, named label in the statement that splices it, with the
// label legion, in any ASCII case: lower(label) in the "C" collation. Every answer
// to "Legion runs" reads this one match - the walk up a chain (legionOnChainSQL) and the labelled
// issues of a moved subtree (labelledIssuesSQL, spec_writer.go) - so no answer folds the case
// another way.
const legionLabelSQL = `lower(label collate "C") = '` + legionLabel + `'`

// legionOnChainSQL answers whether a row of issueChainSQL's chain carries the label legion, in any
// case it was typed in (legionManagedStatusSQL, legionLabelSQL).
const legionOnChainSQL = `exists (
		select 1 from chain c, unnest(c.labels) as label where ` + legionLabelSQL + `
	)`

// legionManagedStatus runs legionManagedStatusSQL inside the caller's transaction. That keeps the
// issue's own row as the caller locked it, and nothing more: an ancestor's labels, and a reparent
// above the issue, are read as they stand. Claim and write advice use the answer as advice.
// The status guard checks the stored row and the proposed row (legion_status_scope.go).
// The document-writer guard also checks whether a write takes the issue out of Legion
// (spec_writer.go). Neither check locks the ancestors' labels.
func legionManagedStatus(ctx context.Context, tx pgx.Tx, key string) (bool, error) {
	var managed bool
	if err := tx.QueryRow(ctx, legionManagedStatusSQL, key).Scan(&managed); err != nil {
		return false, fmt.Errorf("resolve whether Legion manages the status of %s: %w", key, err)
	}
	return managed, nil
}

// legionManagedStatusOfRow is legionManagedStatus for key's row with its parent, labels and
// project given rather than read (legionManagedStatusOfRowSQL): the row as a write leaves it,
// before the write writes it (writeAdviceBefore), or the row after a call that R26's refusal
// judges (legion_status_scope.go).
func legionManagedStatusOfRow(ctx context.Context, tx pgx.Tx, key, project string, parent *string, labels []string) (bool, error) {
	var managed bool
	if err := tx.QueryRow(ctx, legionManagedStatusOfRowSQL, parent, labels, project).Scan(&managed); err != nil {
		return false, fmt.Errorf("resolve whether Legion manages the status of %s: %w", key, err)
	}
	return managed, nil
}

func normalizeIssueBlockers(targets []string) ([]string, error) {
	if len(targets) > contracts.MaxIssueBlockers {
		return nil, countExceededError(apicode.BlockedByInput, "blocked_by", len(targets), contracts.MaxIssueBlockers)
	}
	slices.Sort(targets)
	return slices.Compact(targets), nil
}

// lockProjectIssueDependencies serializes blocker and parent changes before any owner, row or rank
// lock. The two-key namespace is separate from Dispatch's one-key rank, document and event locks.
// It is the one statement of a project's dependency lock: lockIssueDependencyWrite takes each
// project's lock through it, and its id is hashtext of the project key, so a caller holding
// several takes them in docs.ProjectsInLockOrder's order.
func lockProjectIssueDependencies(ctx context.Context, tx pgx.Tx, project string) error {
	if _, err := tx.Exec(ctx, `
		select pg_advisory_xact_lock(hashtext('dispatch-issue-dependencies'), hashtext($1))
	`, project); err != nil {
		return fmt.Errorf("lock project issue dependencies: %w", err)
	}
	return nil
}

// lockDeploymentIssueDependencies is the one dependency lock that is not a project's, and the
// last dependency lock every dependency write takes. Every writer of this build takes it
// exclusively, so no two dependency walks of this build ever run at once and no ring can be
// closed by two writes of this build that each walk a graph the other is changing — whatever
// projects their components touch, and with nothing read before a lock deciding it. What it
// costs is that dependency writes — a blocked_by write and a reparent — serialize across the
// deployment rather than per project; every other write takes neither this lock nor a project's.
//
// It serializes every dependency write only while one build serves: a server of the build
// before this one takes only its own project's dependency lock and never this one. The
// deployment this repository describes holds that, because gcp:Dispatch rolls the app
// Deployment with the Recreate strategy at one replica (infra/gcp/dispatch.go,
// dispatchStrategyFor, minDispatchReplicas), so the old pod stops before the new one starts. A
// deployment that serves two builds at once — a rolling update above one replica — must not
// serve a build before this one beside it, since such a writer shares no lock with a writer of
// this build across three projects.
//
// The project locks are kept for two reasons. Beside the exclusive deployment lock they decide
// nothing among writers of this build; what they still do is fence a writer of an earlier
// build, which holds only its project's lock, on every project this write names (its own and
// each target's), so even with two builds serving only a ring through a project this write does
// not name could close. And the test gates use them: a caller that holds a project's dependency
// lock fences that project's dependency writes
// (TestDependencyWaitDoesNotHoldIssueRankOrNumberLocks, issue_dependencies_test.go).
//
// The key is its own name rather than lockProjectIssueDependencies' namespace with a literal
// second key, so a project whose key happens to hash to that literal cannot collide with it.
func lockDeploymentIssueDependencies(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `
		select pg_advisory_xact_lock(hashtext('dispatch-issue-dependencies-deployment'), 0)
	`); err != nil {
		return fmt.Errorf("lock deployment issue dependencies: %w", err)
	}
	return nil
}

// lockIssueDependencyWrite takes the dependency locks a blocker or parent write needs, in the one
// order every such write takes them: the project lock of the issue's own project and of each
// project a target or the parent of this write belongs to, each through lockProjectIssueDependencies in
// docs.ProjectsInLockOrder's order (ascending lock id, then key), and then the deployment-wide
// dependency lock (lockDeploymentIssueDependencies). Every other lock of the write follows: its
// owner locks and its rank-allocation lock, and its row locks in its write phase. Its reads are
// plain and its locks are advisory transaction locks, so none of it assigns the transaction an id
// (INFRA-636): it is the first step of the write's read phase. project is the issue's own project,
// targets are its blocked_by targets, parent is the parent it names ("" for none), and caller is
// the one project the request's credential may write, empty for a credential that names none.
//
// The project locks are taken in lock id order, not key order, because the lock id is what
// Postgres waits on: two key sets whose keys collide in hashtext would, in key order, take the
// same two ids in opposite orders. Two keys that collide take the same id twice in a row, which
// is harmless because an advisory lock is re-entrant within one session. That order never
// inverts, because every dependency write takes its project locks first, in id order, and the
// deployment lock last, so no two writes can hold what the other waits for.
//
// The reads it makes run with no lock held, and nothing they say decides which locks are taken:
// the projects they name are locked on top of the issue's own, and the deployment lock is taken
// exclusively whatever they said. Reading a target's or the parent's project unlocked is sound
// because no route moves an issue between projects (project_scope.go's file doc), and a key
// created between that read and the lock is still refused by the write's own read inside the
// transaction (writeBlockedBy's, validateIssueParent's and the create's read of its parent).
//
// A caller whose credential names one project is refused a target or a parent outside it here,
// before any lock is taken (foreignDependencyTargets): the pre-handler check reads the project of
// the object the path names and no key of the body (project_scope.go), and a caller that may not
// write at all would otherwise take the deployment-wide lock first. A create runs the same check
// earlier still, before its near-copy gate writes anything (createIssueFrom), and a PATCH before a
// close settles the issue's documents (writeIssue). The write refuses it again under the locks,
// which is the authoritative refusal: writeBlockedBy for a target, and validateIssueParent or the
// create's own read for the parent. Each pair words it through one function,
// refuseBlockedByOutsideCallerProject and refuseParentOutsideCallerProject.
func lockIssueDependencyWrite(ctx context.Context, tx pgx.Tx, project, caller string, targets []string, parent string) error {
	foreign, err := foreignDependencyTargets(ctx, tx, project, caller, targets, parent)
	if err != nil {
		return err
	}
	projects := make([]string, 0, len(foreign)+1)
	projects = append(projects, project)
	for _, target := range foreign {
		projects = append(projects, target.project)
	}
	ordered, err := docs.ProjectsInLockOrder(ctx, tx, projects)
	if err != nil {
		return err
	}
	for _, p := range ordered {
		if err := lockProjectIssueDependencies(ctx, tx, p); err != nil {
			return err
		}
	}
	return lockDeploymentIssueDependencies(ctx, tx)
}

// foreignDependencyTargets is every blocked_by target of targets and the parent ("" for none)
// that belongs to a project other than project, the issue's own, targets first, read with no
// lock held. caller is the one project the request's credential may write, empty for a
// credential that names none; such a caller is refused the first target outside it, then a
// parent outside it, before anything else is read. The lock step, a create's check before its
// near-copy gate and a PATCH's check before a close's settlement all run it, so each refuses the
// same key in the same words.
func foreignDependencyTargets(ctx context.Context, q queryer, project, caller string, targets []string, parent string) ([]foreignTarget, error) {
	foreign, err := foreignTargets(ctx, q, project, targets)
	if err != nil {
		return nil, err
	}
	if caller != "" && len(foreign) > 0 {
		return nil, refuseBlockedByOutsideCallerProject(caller, foreign[0].key, foreign[0].project)
	}
	if parent == "" {
		return foreign, nil
	}
	foreignParent, err := foreignTargets(ctx, q, project, []string{parent})
	if err != nil {
		return nil, err
	}
	if caller != "" && len(foreignParent) > 0 {
		return nil, refuseParentOutsideCallerProject(caller, parent, foreignParent[0].project)
	}
	return append(foreign, foreignParent...), nil
}

// foreignTarget is a blocked_by target or a parent of a project other than the issue's own, and
// the project it belongs to.
type foreignTarget struct{ key, project string }

// foreignTargets is every target belonging to a project other than project, in key order, which
// is the order writeBlockedBy reads its targets in, so a scoped caller is refused the same target
// by both. A target with no row is left out: the write's own read refuses it inside the
// transaction, which is where an unknown key is answered.
func foreignTargets(ctx context.Context, q queryer, project string, targets []string) ([]foreignTarget, error) {
	if len(targets) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `
		select key, project_key from issues where key = any($1) and project_key <> $2 order by key
	`, targets, project)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (foreignTarget, error) {
		var target foreignTarget
		return target, row.Scan(&target.key, &target.project)
	})
}

// refuseBlockedByOutsideCallerProject is what a caller whose credential names one project is
// answered for a blocked_by target outside it. Two steps of the same write refuse it — the lock
// step before any lock is taken, and writeBlockedBy under the locks — so the wording lives here
// once and the two cannot drift.
func refuseBlockedByOutsideCallerProject(caller, target, targetProject string) error {
	return errorf(apicode.ProjectScope,
		"this request's credential may write project %s; blocked_by issue %s belongs to project %s",
		caller, target, targetProject)
}

// refuseParentOutsideCallerProject is what a caller whose credential names one project is
// answered for a parent outside it, as refuseBlockedByOutsideCallerProject is for a blocked_by
// target: a parent may be an issue of any project, but a credential that may write one project
// does not hang its issue under another's. The lock step refuses it before any lock is taken,
// and validateIssueParent or the create's read of its parent under the locks, so the wording
// lives here once.
func refuseParentOutsideCallerProject(caller, parent, parentProject string) error {
	return errorf(apicode.ProjectScope,
		"this request's credential may write project %s; parent issue %s belongs to project %s",
		caller, parent, parentProject)
}

// lockIssueOwners takes the owner locks of key and of the issues in others in one statement, in the
// lock-key order LockOwners takes event owners in (store.LockOwners), and then answers key's parent
// as it stands under them. A PATCH locks every issue it names through one call, so two writes
// naming the same issues queue one behind the other instead of each holding one while waiting for
// the other. A key with no row costs a lock nobody else takes; the caller's own read of that key
// refuses it. The parent is read in the statement after the locks, never in the same one, so it
// is the parent the lock's previous holder committed (store.LockOwnersAndRead): every writer of
// parent_key holds the issue's lock.
func lockIssueOwners(ctx context.Context, tx pgx.Tx, key string, others ...string) (*string, error) {
	owners := make([]store.Owner, 0, len(others)+1)
	owners = append(owners, store.IssueOwner(key))
	for _, other := range others {
		owners = append(owners, store.IssueOwner(other))
	}
	var parent *string
	err := store.LockOwnersAndRead(ctx, tx, owners, `select parent_key from issues where key = $1`, []any{key}, &parent)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	return parent, nil
}

// parentLockAttempts bounds how many times lockPatchOwnersAndParent reads the issue's parent and
// locks it: the first, and one redo for a parent a concurrent reparent changed in between.
const parentLockAttempts = 2

// lockPatchOwners takes the owner locks of the issue and of the issues in named (a proposed parent,
// rank neighbours) in one lock order, for a PATCH that appends no event to the issue's current
// parent.
func lockPatchOwners(ctx context.Context, tx pgx.Tx, key string, named ...string) error {
	_, err := lockIssueOwners(ctx, tx, key, named...)
	return err
}

// lockPatchOwnersAndParent is lockPatchOwners for a PATCH that appends an event to the issue's
// current parent (a status change, a reparent), which it locks in the same lock order. The parent
// can only be read before the lock, and a concurrent reparent of the issue, which holds the
// issue's lock, can change it in between, so the lock reads it again. A parent that moved releases
// every lock the attempt took (a savepoint, whose rollback gives back the advisory locks taken
// under it) and the attempt starts over with the parent that stands, so no lock is ever taken out
// of order. A parent that moves at every attempt is 409 PARENT_CONTENDED, with nothing applied.
//
// It does not use resolveThenLock, whose resolve half runs with nothing held and whose write half
// owns its own transaction: both halves here run inside the PATCH's transaction, after its
// advisory locks, so the locks they take commit with the PATCH's writes.
func lockPatchOwnersAndParent(ctx context.Context, tx pgx.Tx, key string, named ...string) error {
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
		locked, err := lockIssueOwners(ctx, attempt, key, rows...)
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
	return errorf(apicode.ParentContended,
		"%s's parent changed at each of %d attempts while this request waited to lock it, so nothing was applied: read the issue and retry", key, parentLockAttempts)
}

// validateIssueParent reads the ancestor chain while the dependency locks are held, and refuses a
// parent that would make key its own ancestor or put key's subtree past parentDepthCap. The
// parent may be an issue of any project, and the chain it reads runs through every project it
// crosses. caller is the one project the request's credential may write, empty for a credential
// that names none: a parent outside it is refused (refuseParentOutsideCallerProject), which is
// the authoritative refusal of a write lockIssueDependencyWrite already refused before its locks.
func validateIssueParent(ctx context.Context, tx pgx.Tx, key, caller, parent string) error {
	if parent == key {
		return errorf(apicode.ParentInput, "an issue cannot be its own parent")
	}
	var parentProject string
	err := tx.QueryRow(ctx, `select project_key from issues where key = $1`, parent).Scan(&parentProject)
	if errors.Is(err, pgx.ErrNoRows) {
		return errorf(apicode.ParentInput, "parent issue %s not found", parent)
	}
	if err != nil {
		return err
	}
	if caller != "" && parentProject != caller {
		return refuseParentOutsideCallerProject(caller, parent, parentProject)
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
			return errorf(apicode.ParentCycle, "would create a cycle: %s", path)
		}
	}
	return refuseParentTooDeep(ctx, tx, key, parent)
}

// refuseParentTooDeep refuses a parent link that would put an issue, or the deepest issue under
// it, more than parentDepthCap links below its root: 409 PARENT_INPUT under /api/v1, and
// PARENT_TOO_DEEP under /api/v2, which tells it from a parent loop (apicode.ParentTooDeep). child
// is the issue being moved, or "" for one being created, which has nothing under it. The chain may
// cross projects, and the cap counts every link of it. The caller holds the dependency locks
// (lockIssueDependencyWrite), which every create with a parent and every reparent takes, and the
// deployment-wide one among them orders every such write whatever projects it names, so two
// writes cannot each pass alone and together pass the cap. Both walks stop one link past the
// cap, so a chain that loops or is already too deep ends them.
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
		return errorf(apicode.ParentTooDeep,
			"an issue under %s would sit %s below its root; an issue sits at most %d links below its root", parent, depth, parentDepthCap)
	}
	return errorf(apicode.ParentTooDeep,
		"moving %s under %s would put the deepest issue under it %s below its root; an issue sits at most %d links below its root", child, parent, depth, parentDepthCap)
}

// foreignChildOpen is one child of another project that holds an acceptance write back: its key,
// its project and its status.
type foreignChildOpen struct {
	Key     string `json:"key"`
	Project string `json:"project"`
	Status  string `json:"status"`
}

// foreignChildOpenError is the 409 FOREIGN_CHILD_OPEN body PATCH /api/v1/issues/{key} and PATCH
// /api/v2/issues/{key} answer an acceptance write that refuseOpenForeignChildren refuses, naming
// each open child of another project in key order.
type foreignChildOpenError struct {
	Code   string             `json:"code"`
	Error  string             `json:"error"`
	Issues []foreignChildOpen `json:"issues"`
}

// openForeignChildrenError is refuseOpenForeignChildren's refusal, which the route answers with
// the body it carries (foreignChildOpenError) rather than the plain error body.
type openForeignChildrenError struct {
	issue    string
	children []foreignChildOpen
}

func (e openForeignChildrenError) Error() string {
	named := make([]string, len(e.children))
	for index, child := range e.children {
		named[index] = fmt.Sprintf("%s (%s, %s)", child.Key, child.Project, child.Status)
	}
	return fmt.Sprintf("%s has an open child of another project: %s; it is accepted or closed only once each is done or in icebox",
		e.issue, strings.Join(named, ", "))
}

// body is the refusal's body under code, the name the request's version gives FOREIGN_CHILD_OPEN
// (refusalPair).
func (e openForeignChildrenError) body(code string) foreignChildOpenError {
	return foreignChildOpenError{Code: code, Error: e.Error(), Issues: e.children}
}

// refuseOpenForeignChildren refuses an acceptance write - a status of retro or done that carries
// refuse_open_foreign_children, which the Legion daemon's sign_off and close_root send - while key
// has an open child of another project than project, key's own. Open is what the daemon's
// record.ChildOpen says of a child its records do not hold, which a child of another project
// always is: any status but done and icebox. A child of key's own project is its own daemon's to
// judge, from the children it records, so it is not read here.
//
// The caller holds the deployment dependency lock (lockIssueDependencyWrite), which every create
// with a parent and every reparent takes, so no child is filed under key, or moved under it,
// between this read and the status the write sets.
func refuseOpenForeignChildren(ctx context.Context, tx pgx.Tx, key, project string) error {
	rows, err := tx.Query(ctx, `
		select key, project_key, status from issues
		where parent_key = $1 and project_key <> $2 and status not in ('done', 'icebox')
		order by key
	`, key, project)
	if err != nil {
		return err
	}
	children, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (foreignChildOpen, error) {
		var child foreignChildOpen
		err := row.Scan(&child.Key, &child.Project, &child.Status)
		return child, err
	})
	if err != nil {
		return err
	}
	if len(children) == 0 {
		return nil
	}
	return openForeignChildrenError{issue: key, children: children}
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
// names none (callerProject). A target outside it is refused here, under the locks, which is the
// authoritative refusal: lockIssueDependencyWrite refuses the same caller before any lock is
// taken, and both word it through refuseBlockedByOutsideCallerProject.
func writeBlockedBy(ctx context.Context, tx pgx.Tx, issue, project, caller string, targets []string) error {
	if slices.Contains(targets, issue) {
		return errorf(apicode.BlockedByInput, "%s cannot be blocked by itself", issue)
	}
	for _, target := range targets {
		var targetProject string
		err := tx.QueryRow(ctx, `select project_key from issues where key = $1`, target).Scan(&targetProject)
		if errors.Is(err, pgx.ErrNoRows) {
			return errorf(apicode.BlockedByInput, "blocked_by issue %s not found", target)
		}
		if err != nil {
			return err
		}
		if targetProject != project && caller != "" {
			return refuseBlockedByOutsideCallerProject(caller, target, targetProject)
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

// assertNoWaitPath refuses a wait whose head reaches its tail, stopping at parentDepthCap.
// The caller serializes blocker and parent changes, so traversal needs only plain reads.
func assertNoWaitPath(ctx context.Context, tx pgx.Tx, from []waitNode, goal waitNode) error {
	seen := map[waitNode]bool{}
	frontier := from
	for depth := 0; len(frontier) > 0; depth++ {
		if depth > parentDepthCap {
			return errorf(apicode.DependencyCycle, "the dependency graph is deeper than %d links", parentDepthCap)
		}
		var starts, dones []string
		for _, node := range frontier {
			halves := []waitNode{node, {key: node.key}}
			if !node.done {
				halves = halves[:1]
			}
			for _, half := range halves {
				if half == goal {
					return errorf(apicode.DependencyCycle, "%s would wait on itself through blocked_by and parent links", goal.key)
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
		rows, err := tx.Query(ctx, `
			select parent_key, false from issues where key = any($1) and parent_key is not null
			union
			select target_key, true from issue_links where issue_key = any($1) and kind = 'blocked_by'
			union
			select key, true from issues where parent_key = any($2)
		`, starts, dones)
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
