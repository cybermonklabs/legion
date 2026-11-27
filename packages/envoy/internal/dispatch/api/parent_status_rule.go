package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// group is one of the three groups that R24 puts each issue status in. The rule holds two
// things between a parent and each of its children: a parent is done only when each child is
// done, and while a child is in work, its parent is in work. The zero group is no group:
// statusGroup gives it for a string that is not a status.
type group string

const (
	// groupNotStarted holds the statuses of work that nobody started: triage, icebox and
	// backlog.
	groupNotStarted group = "not started"
	// groupInWork holds the statuses of work that is planned or that runs: todo, in_progress,
	// testing, needs_review and retro.
	groupInWork group = "in work"
	// groupDone holds done alone.
	groupDone group = "done"
)

// statusGroup is the group of an issue status of model.IssueStatuses.
func statusGroup(status string) group {
	switch status {
	case model.IssueStatusTriage, "icebox", "backlog":
		return groupNotStarted
	case "todo", "in_progress", "testing", "needs_review", "retro":
		return groupInWork
	case "done":
		return groupDone
	}
	return ""
}

// pairBreaksTheRule reports whether a child in childStatus under a parent in parentStatus breaks
// R24: the parent is done and the child is not, or the child is in work and the parent is not.
func pairBreaksTheRule(childStatus, parentStatus string) bool {
	child, parent := statusGroup(childStatus), statusGroup(parentStatus)
	return (parent == groupDone && child != groupDone) || (child == groupInWork && parent != groupInWork)
}

// ParentStatusRule is the setting DISPATCH_PARENT_STATUS_RULE as the server holds it. While it
// is on, a write that breaks R24 is refused 409 PARENT_STATUS_CONFLICT. Its one field is
// unexported, so outside this package only the zero value, which is off, can be made.
type ParentStatusRule struct{ on bool }

// On reports whether the rule refuses a write that breaks it.
func (rule ParentStatusRule) On() bool { return rule.on }

// parentStatusRuleActor is Dispatch's own system actor for R24 and R25. R25's fix writes as it,
// and it is the author of the comment that each option of a status change and each change of
// the fix leaves. A status that an option writes stays the caller's.
var parentStatusRuleActor = model.Actor{Kind: "system", ID: "parent-status-rule"}

// issueWriter is who makes a write of an issue, as refuseIssueWritePlan judges it.
type issueWriter struct {
	// actor is the actor the write records (actorFrom).
	actor model.Actor
	// credential is the credential of the caller that optionalActor resolved, which actor no
	// longer carries (model.Actor.Recorded). It is nil for the fix, which has no request.
	credential *model.Credential
	// callerProject is the one project the request's credential names (callerProject), empty
	// for a credential that names none and for the fix. A PATCH (writeIssue) and a status change
	// (planStatusChangeUnderLocks) ask access.CredentialCovers about each issue of another
	// project they write with it, and R24's conflict and the open-child check leave out of
	// their answers each issue of a private project that the credential does not name
	// (seenConflictIssues, seenForeignChildren).
	callerProject string
	// human is true only for a request that presents no token and that the browser identity
	// names: a person at a browser (optionalActor's second answer).
	human bool
	// system is true only for R25's fix (parentStatusFixWriter). No request sets it.
	system bool
}

// requestWriter is the writer of a request: the actor that actorFrom records, the credential of
// the caller that optionalActor resolved, the one project that credential names, and whether that
// caller is a person at a browser. It resolves the caller once and refuses what actorFrom refuses.
func (s *server) requestWriter(r *http.Request, supplied *model.Actor) (issueWriter, error) {
	caller, human, err := s.optionalActor(r)
	if err != nil {
		return issueWriter{}, err
	}
	actor, err := recordedActor(r, caller, human, supplied)
	if err != nil {
		return issueWriter{}, err
	}
	callerProject, _ := s.callerProject(r)
	return issueWriter{actor: actor, credential: caller.Credential, callerProject: callerProject, human: human}, nil
}

// parentStatusFixWriter is the writer of R25's fix, which only the boot fix uses: Dispatch's
// system actor, with no request and no credential.
func parentStatusFixWriter() issueWriter {
	return issueWriter{actor: parentStatusRuleActor, system: true}
}

// statusChangeOptions is the three options of a status change (POST
// /api/v1/issues/{key}/status-change): include_children, unlink_children and include_parent. Each
// check of a plan reads them: R24's refusal of include_children with done from a token, and R26's
// owning architect's write, which the options narrow (legion_status_scope.go). A PATCH and R25's
// fix carry none.
type statusChangeOptions struct {
	// includeChildren is that the call moves the open issues under the issue it names with that
	// issue.
	includeChildren bool
	// unlinkChildren is that the call removes each open child from the issue it names.
	unlinkChildren bool
	// includeParent is that the call moves each ancestor that is not in work to in_progress.
	includeParent bool
}

// issueWritePlan is a write of one or more issues, step by step, as refuseIssueWritePlan judges it
// before the first step writes. A step may write the status, the parent, the labels, or none of
// them (the plan of a PATCH that writes the title alone).
type issueWritePlan struct {
	steps []issueWriteStep
	// options is the options of the status change that the plan is, or of the status change that
	// the plan's one step is a step of (issueWriteCallStep.options). A PATCH and R25's fix carry
	// none.
	options statusChangeOptions
	// unjudged is that the caller refuses no conflict with R24, so refuseIssueWritePlan reads
	// none: the PATCH's plan with the rule off. The route and the fix never set it, since a dry
	// run answers the conflict whatever the setting and the fix writes with the rule on.
	unjudged bool
}

// issueWriteStep is the write of one issue in a plan: the issue as the plan read it under its
// owner lock, and what the step writes.
type issueWriteStep struct {
	key   string
	title string
	// project is the issue's project, which no write changes. The check of who writes a
	// Legion-run issue's spec reads the issue's chain only up to a parent of that project
	// (refuseSpecWriterOfIssueWrite).
	project      string
	statusBefore string
	// statusAfter is the status the step writes, or nil when the step writes no status.
	statusAfter *string
	// parentBefore is the parent the issue has before the step.
	parentBefore *string
	// reparent is that the step writes the parent: parentAfter, or no parent (an unlink) when
	// parentAfter is nil.
	reparent    bool
	parentAfter *string
	// labelsAfter is the labels the step writes, or nil when the step keeps them. Only a PATCH
	// writes labels.
	labelsAfter *[]string
}

// changesStatus reports whether the step writes a status other than the one the issue has.
func (step issueWriteStep) changesStatus() bool {
	return step.statusAfter != nil && *step.statusAfter != step.statusBefore
}

// changesParent reports whether the step writes a parent other than the one the issue has.
func (step issueWriteStep) changesParent() bool {
	return step.reparent && !stringPointersEqual(step.parentBefore, step.parentAfter)
}

// issueWritePlanOfPatch is the plan of a PATCH: one step, from the status, the parent and the
// labels of its body, over the issue as the PATCH read it under its owner locks. status is nil
// when the body names no status, and labels is nil when the body names no labels.
func issueWritePlanOfPatch(before model.Issue, status *string, parentProvided bool, parent *string, labels *[]string) issueWritePlan {
	return issueWritePlan{steps: []issueWriteStep{{
		key:          before.Key,
		title:        before.Title,
		project:      before.Project,
		statusBefore: before.Status,
		statusAfter:  status,
		parentBefore: before.Parent,
		reparent:     parentProvided,
		parentAfter:  parent,
		labelsAfter:  labels,
	}}}
}

// plannedWrite is a step of a plan that its builder checks under the plan's owner locks: an
// issueWriteStep, or a step that embeds one (statusChangeStep, parentStatusFixStep).
type plannedWrite interface{ issueWrite() issueWriteStep }

// issueWrite is the step itself, which a step that embeds it answers too.
func (step issueWriteStep) issueWrite() issueWriteStep { return step }

// issueWriteLocks is the owner locks that the steps of a plan need, in key order: each step's
// issue, and its current and new parent.
func issueWriteLocks[Step plannedWrite](steps []Step) []string {
	var keys []string
	for _, planned := range steps {
		step := planned.issueWrite()
		keys = append(keys, step.key)
		if step.parentBefore != nil {
			keys = append(keys, *step.parentBefore)
		}
		if step.parentAfter != nil {
			keys = append(keys, *step.parentAfter)
		}
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}

// locksHeld reports whether held, in key order, holds every owner lock that steps need
// (issueWriteLocks).
func locksHeld[Step plannedWrite](held []string, steps []Step) bool {
	for _, lock := range issueWriteLocks(steps) {
		if _, locked := slices.BinarySearch(held, lock); !locked {
			return false
		}
	}
	return true
}

// lockIssueWriteOwners takes the owner locks of the issues keys in one store.LockOwners call, the
// lock order every write of many owners takes: the planning transaction of a status change and
// of R25's fix, which checks the whole plan under them.
func lockIssueWriteOwners(ctx context.Context, tx pgx.Tx, keys []string) error {
	owners := make([]store.Owner, 0, len(keys))
	for _, key := range keys {
		owners = append(owners, store.IssueOwner(key))
	}
	return store.LockOwners(ctx, tx, owners...)
}

// ruleIssue is an issue as a builder of a plan reads it for R24: its key, its status and its
// parent.
type ruleIssue struct {
	key, status string
	parent      *string
}

// issueWriteRow is an issue's row as the builder of a plan reads it to fill in its steps. project
// is the project the issue belongs to, and private whether that project is private, which a status
// change asks access.CredentialCovers about (planStatusChangeUnderLocks).
type issueWriteRow struct {
	key, title, status, project string
	parent                      *string
	lastSeq                     int
	private                     bool
}

// readIssueWriteRows reads the rows of the issues keys, by key, each with its project's visibility
// in the same statement.
func readIssueWriteRows(ctx context.Context, q queryer, keys []string) (map[string]issueWriteRow, error) {
	rows, err := q.Query(ctx, `
		select i.key, i.title, i.status, i.project_key, i.parent_key, i.last_seq, p.visibility = 'private'
		from issues i
		join projects p on p.key = i.project_key
		where i.key = any($1)
	`, keys)
	if err != nil {
		return nil, fmt.Errorf("read the issues of a plan: %w", err)
	}
	collected, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (issueWriteRow, error) {
		var read issueWriteRow
		err := row.Scan(&read.key, &read.title, &read.status, &read.project, &read.parent, &read.lastSeq, &read.private)
		return read, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the issues of a plan: %w", err)
	}
	read := make(map[string]issueWriteRow, len(collected))
	for _, row := range collected {
		read[row.key] = row
	}
	return read, nil
}

// unlinkFrom makes call the unlink of its issue from parent, a step of a status change or of
// R25's fix: it writes no parent, and it leaves an issue that is done or that is no longer under
// parent as it stands.
func (call *issueWriteCall) unlinkFrom(parent string) {
	call.fields.Parent = json.RawMessage("null")
	call.step.leaves = func(before model.Issue) bool {
		return statusGroup(before.Status) == groupDone || before.Parent == nil || *before.Parent != parent
	}
}

// refuseIssueWritePlan is the one check of a plan, which its caller runs before the plan's first
// write, with the owner lock of each step's issue held, and for a step that writes a status or a
// parent, the locks of its current and new parent too.
// It refuses with an error R26's refusal, while its setting is on, and what no setting lets
// through. Then it answers the conflict with R24 (parentStatusConflictOf), or nil when the plan
// keeps the rule. An unjudged plan answers nil without R24's read, since its caller refuses no
// conflict; R26 judges it all the same. Every other caller refuses the conflict only while the
// rule is on, but for a dry run of the route. who is the reader the write reads its issues as
// (issueWriteCall.who): R24 judges the pairs of the issues who sees.
func (s *server) refuseIssueWritePlan(ctx context.Context, q pgx.Tx, writer issueWriter, who model.Actor, plan issueWritePlan) (*parentStatusConflict, error) {
	// R26 (legion_status_scope.go) comes first, so a call that R24 refuses too answers R26's code.
	// It runs with the parent-status rule off too: an unjudged plan returns after it.
	if err := refuseLegionStatusWrite(ctx, q, s.deps.LegionStatusScope, writer, plan); err != nil {
		return nil, err
	}
	if plan.options.includeChildren && !writer.human && plan.writesStatus("done") {
		return nil, errorf(http.StatusForbidden, "BROWSER_SIGN_IN_REQUIRED",
			"Only a person who is signed in at a browser can close the open issues under an issue together with it (include_children with done). This request presents a token.")
	}
	if plan.unjudged {
		return nil, nil
	}
	return parentStatusConflictOf(ctx, q, who, plan, writer.callerProject)
}

// writesStatus reports whether a step of the plan writes status.
func (plan issueWritePlan) writesStatus(status string) bool {
	for _, step := range plan.steps {
		if step.statusAfter != nil && *step.statusAfter == status {
			return true
		}
	}
	return false
}

// plannedIssue is an issue that a plan involves, as q read it and with each step of the plan
// written over it, with its project and whether that project is private.
type plannedIssue struct {
	key, title, status, project string
	parent                      *string
	private                     bool
}

// parentStatusConflictOf judges the pairs that the plan changes on the state after every step:
// it reads the issues involved (each judged step's issue, its parent, and the direct children of
// each issue whose status a step changes) and writes each step's status and parent over them. A
// status change judges the issue with its parent and with each direct child. A change of parent
// judges the issue with its new parent. A pair that no step changes is not judged, so a plan that
// changes no pair reads nothing. Every writer of a pair holds the parent's owner lock, so two
// writes of one pair are judged one after the other. The issues are listed in the order of the
// steps, a parent before the children, and the children in the order of their numbers.
//
// The issues are read as who reads them (access.ProjectClause), as refuseOpenForeignChildren reads
// the children it counts: the rule decides with what its writer knows. An issue of a project who
// does not see is neither named in the conflict nor counted, so it decides no 409, and neither a
// refusal nor a write that it lets through tells the writer anything of it; a pair with such an
// issue at one end is not judged, and the write may leave it out of line. caller is the one
// project the request's credential names: an issue who sees of a private project that caller does
// not name is judged, and the answer leaves it out (seenConflictIssues).
func parentStatusConflictOf(ctx context.Context, q pgx.Tx, who model.Actor, plan issueWritePlan, caller string) (*parentStatusConflict, error) {
	var judged []issueWriteStep
	var parentsOfChildren []string
	for _, step := range plan.steps {
		if !step.changesStatus() && !step.changesParent() {
			continue
		}
		judged = append(judged, step)
		if step.changesStatus() {
			parentsOfChildren = append(parentsOfChildren, step.key)
		}
	}
	if len(judged) == 0 {
		return nil, nil
	}
	keys := issueWriteLocks(judged)
	rows, err := q.Query(ctx, `
		select i.key, i.title, i.status, i.parent_key, i.project_key, p.visibility = 'private'
		from issues i
		join projects p on p.key = i.project_key
		where (i.key = any($1) or i.parent_key = any($2))
		  and `+access.ProjectClause("i.project_key", "$3")+`
		order by i.project_key, i.number
	`, keys, parentsOfChildren, refs.VisibilityArg(who))
	if err != nil {
		return nil, fmt.Errorf("read the issues of a write's parent-status pairs: %w", err)
	}
	read, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*plannedIssue, error) {
		var issue plannedIssue
		err := row.Scan(&issue.key, &issue.title, &issue.status, &issue.parent, &issue.project, &issue.private)
		return &issue, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the issues of a write's parent-status pairs: %w", err)
	}
	issues := make(map[string]*plannedIssue, len(read))
	for _, issue := range read {
		issues[issue.key] = issue
	}
	for _, step := range plan.steps {
		issue, ok := issues[step.key]
		if !ok {
			// A step's issue is one the caller writes, so it is no issue the caller cannot see.
			issue = &plannedIssue{key: step.key, title: step.title, status: step.statusBefore, parent: step.parentBefore, project: step.project}
			issues[step.key] = issue
		}
		if step.statusAfter != nil {
			issue.status = *step.statusAfter
		}
		if step.reparent {
			issue.parent = step.parentAfter
		}
	}
	conflict := &parentStatusConflict{}
	listed := map[[2]string]bool{}
	list := func(holder *plannedIssue, relation string, of *plannedIssue, reason string) {
		if listed[[2]string{of.key, holder.key}] {
			return
		}
		listed[[2]string{of.key, holder.key}] = true
		conflict.issues = append(conflict.issues, parentStatusConflictIssue{
			Key: holder.key, Title: holder.title, Status: holder.status, Relation: relation, of: of.key, reason: reason,
			project: holder.project, private: holder.private,
		})
	}
	for _, step := range judged {
		issue := issues[step.key]
		if issue.parent != nil {
			if parent, ok := issues[*issue.parent]; ok && pairBreaksTheRule(issue.status, parent.status) {
				list(parent, relationParent, issue, parentReason(parent.status))
			}
		}
		if !step.changesStatus() {
			continue
		}
		for _, child := range read {
			if child.parent != nil && *child.parent == issue.key && pairBreaksTheRule(child.status, issue.status) {
				list(child, relationChild, issue, childReason(issue.status))
			}
		}
	}
	if len(conflict.issues) == 0 {
		return nil, nil
	}
	conflict.issues = seenConflictIssues(conflict.issues, caller)
	return conflict, nil
}

// seenConflictIssues is issues without each issue of a private project that caller, the one
// project the request's credential names, does not name (seenBy). Every builder of a conflict that
// can list such an issue reads only the issues its writer's reader sees and answers through here
// (parentStatusConflictOf, openChildrenOfAClose), so a caller whose credential names no project is
// answered each issue listed. The rule has judged each pair of those issues already, so a conflict
// refuses the write whatever this leaves, and its list can be empty. The parent of a new issue
// (createParentConflict) is one that the create's read of its parent, as its writer reads it,
// already covered.
func seenConflictIssues(issues []parentStatusConflictIssue, caller string) []parentStatusConflictIssue {
	if caller == "" {
		return issues
	}
	return seenBy(caller, issues, func(issue parentStatusConflictIssue) (string, bool) { return issue.project, issue.private })
}

// seenBy is items without each item of a private project that caller, the one project a
// request's credential names, does not name (access.CredentialCovers), projectOf reading an item's
// project and whether that project is private. An answer that can name an issue of such a project
// leaves it out through here: the issues of a 409 PARENT_STATUS_CONFLICT (seenConflictIssues), the
// children of a 409 FOREIGN_CHILD_OPEN (seenForeignChildren), and the checks that a result's
// check value matches (resolveCheck). Each caller answers a caller that names no project, "",
// before it calls this, since such a caller sees every item.
func seenBy[T any](caller string, items []T, projectOf func(T) (string, bool)) []T {
	seen := make([]T, 0, len(items))
	for _, item := range items {
		if project, private := projectOf(item); access.CredentialCovers(access.Credential{Listed: []string{caller}}, project, private) {
			seen = append(seen, item)
		}
	}
	return seen
}

// openChildrenOfAClose is the second read of a close with the rule on: the children of key
// that are not done, read in the close's write phase after its own update of key's row. A create
// under key holds no owner lock of key, so the close's first check cannot see a child whose create
// has not committed. The create's share lock of key's row makes the close's update wait for that
// commit, so this read, the statement after the update, sees the child. The children are read as
// who reads them (access.ProjectClause), as the first check reads them (parentStatusConflictOf): a
// child of a project who does not see is neither named nor counted. caller is the one project the
// request's credential names, empty for one that names none: the answer leaves out each child that
// caller cannot see (seenConflictIssues).
func openChildrenOfAClose(ctx context.Context, tx pgx.Tx, who model.Actor, key, caller string) (*parentStatusConflict, error) {
	rows, err := tx.Query(ctx, `
		select i.key, i.title, i.status, i.project_key, p.visibility = 'private'
		from issues i
		join projects p on p.key = i.project_key
		where i.parent_key = $1 and i.status <> 'done'
		  and `+access.ProjectClause("i.project_key", "$2")+`
		order by i.project_key, i.number
	`, key, refs.VisibilityArg(who))
	if err != nil {
		return nil, fmt.Errorf("read the open children of a close: %w", err)
	}
	children, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (parentStatusConflictIssue, error) {
		child := parentStatusConflictIssue{Relation: relationChild, of: key, reason: childReason("done")}
		err := row.Scan(&child.Key, &child.Title, &child.Status, &child.project, &child.private)
		return child, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the open children of a close: %w", err)
	}
	if len(children) == 0 {
		return nil, nil
	}
	return &parentStatusConflict{issues: seenConflictIssues(children, caller)}, nil
}

// createParentConflict is the conflict of a new issue, which is in triage, with a parent in
// parentStatus: only a done parent holds the rule against it.
func createParentConflict(parentKey, parentTitle, parentStatus string) *parentStatusConflict {
	if !pairBreaksTheRule(model.IssueStatusTriage, parentStatus) {
		return nil
	}
	return &parentStatusConflict{issues: []parentStatusConflictIssue{{
		Key: parentKey, Title: parentTitle, Status: parentStatus, Relation: relationParent, reason: parentReason(parentStatus),
	}}}
}

// The relations of an issue that holds R24 against a write to the issue that the write judged.
const (
	relationParent = "parent"
	relationChild  = "child"
)

// parentReason is why a parent in parentStatus holds the rule against its child: it is done, or,
// for a child in work, it is not in work.
func parentReason(parentStatus string) string {
	if statusGroup(parentStatus) == groupDone {
		return "is done"
	}
	return "is not in work"
}

// childReason is why a child holds the rule against its parent in parentStatus: the parent is
// done and the child is not, or the child is in work and the parent is not.
func childReason(parentStatus string) string {
	if statusGroup(parentStatus) == groupDone {
		return "is not done"
	}
	return "is in work"
}

// parentStatusConflict is a write that R24 refuses, with each issue that holds the rule against
// it. writeHandlerError answers it 409 PARENT_STATUS_CONFLICT. The status-change route answers it
// with the entries of the steps that committed before it (statusChangeConflict).
type parentStatusConflict struct {
	issues []parentStatusConflictIssue
}

// parentStatusConflictIssue is an issue that holds R24 against a write: the issue at the other
// end of a pair that the write would break, with its status as the write would leave it.
type parentStatusConflictIssue struct {
	Key    string `json:"key"`
	Title  string `json:"title"`
	Status string `json:"status"`
	// Relation is what this issue is to the issue that the write judged: its parent or its child.
	Relation string `json:"relation"`
	// of is the key of the issue that the write judged. It is empty for the issue that a create
	// files, which has no key yet.
	of string
	// reason is why this issue holds the rule against that issue, as the error text says it.
	reason string
	// project is the project this issue belongs to, and private whether that project is private,
	// which decide whether the answer names this issue to the caller (seenConflictIssues).
	project string
	private bool
}

// parentStatusConflictBody is the 409 PARENT_STATUS_CONFLICT body.
type parentStatusConflictBody struct {
	Code   string                      `json:"code"`
	Error  string                      `json:"error"`
	Issues []parentStatusConflictIssue `json:"issues"`
}

// Error names each issue that holds the rule with its status, so a client that reads no issues
// still shows them. Each issue is a sentence of its own.
func (conflict *parentStatusConflict) Error() string {
	var text strings.Builder
	text.WriteString("this change would leave a parent's status out of line with its children's.")
	for _, issue := range conflict.issues {
		of := issue.of
		if of == "" {
			of = "the new issue"
		}
		kin := "a child of"
		if issue.Relation == relationParent {
			kin = "the parent of"
		}
		fmt.Fprintf(&text, " %s (%s) is %s %s and %s.", issue.Key, issue.Status, kin, of, issue.reason)
	}
	text.WriteString(" A parent is done only when each child is done, and while a child is in work, its parent is in work.")
	return text.String()
}

// body is the conflict as writeHandlerError answers it.
func (conflict *parentStatusConflict) body() parentStatusConflictBody {
	return parentStatusConflictBody{
		Code:   "PARENT_STATUS_CONFLICT",
		Error:  conflict.Error(),
		Issues: conflict.issues,
	}
}
