package api

// status_change.go is POST /api/v1/issues/{key}/status-change: a status change of one issue that
// names, with an option, what happens to the open issues under it or to the issues above it, so
// that every parent's status stays in line with its children's (R24, parent_status_rule.go).
//
// The call is a sequence of single-issue writes, each one the PATCH's own write (writeIssue) in a
// transaction of its own, in an order where every write keeps the rule: the ancestors that
// include_parent moves, highest first; the unlinks of unlink_children; for a status that is done
// or not started, the descendants that include_children moves, deepest first; the issue the call
// names; and for a status in work, the descendants that include_children moves, highest first.
// Before the first write, the call checks its whole plan under the owner locks of every issue the
// plan writes (planStatusChange). Each write then decides what it writes from what it reads under
// its own lock, so a write that another writer made unneeded writes nothing.
//
// A keyed call keeps its progress in its request key's recovery point (statusChangeRecovery):
// each step that writes adds its entry there in its own transaction, right after its write phase,
// with its issue as that write answered it, and the call marks the point finished after its last
// step, with the answer's issue. A repeat of a call that finished is answered from the point
// (replayStatusChange). A repeat that finds the holder gone before the mark takes the key over
// (claimRequestKey), and that attempt runs only the steps the point does not hold, answering the
// stored entries as their steps answered them.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// statusChangeRoute is the route's own name, which the advice of each write names.
const statusChangeRoute = "POST /api/v1/issues/{key}/status-change"

// The three options of a status change, as its body names them.
const (
	optionIncludeChildren = "include_children"
	optionUnlinkChildren  = "unlink_children"
	optionIncludeParent   = "include_parent"
)

// statusChangeRuleSentence is the rule as each comment of an option states it.
const statusChangeRuleSentence = "Dispatch keeps a parent's status in line with its children's: a parent is done only when " +
	"each child is done, and while a child is in work its parent is in work too."

// statusChangeInput is the body of POST /api/v1/issues/{key}/status-change.
type statusChangeInput struct {
	// Status is the status the call writes on the issue it names.
	Status string `json:"status" required:"true"`
	// IncludeChildren writes the status on each open issue under the issue, at any depth, that is
	// not in the status's group.
	IncludeChildren bool `json:"include_children,omitempty"`
	// UnlinkChildren unlinks each open child of the issue, so each one becomes a root.
	UnlinkChildren bool `json:"unlink_children,omitempty"`
	// IncludeParent moves each ancestor of the issue that is not in work to in_progress.
	IncludeParent bool `json:"include_parent,omitempty"`
	// DryRun answers what the call would change, and writes nothing.
	DryRun bool `json:"dry_run,omitempty"`
	// ExpectedSeq makes the call conditional, as on the PATCH: 409 ISSUE_CHANGED when the
	// issue's last event is no longer that sequence.
	ExpectedSeq *int64 `json:"expected_sequence"`
	// Actor names the writer, as on the PATCH.
	Actor *model.Actor `json:"actor"`
}

// statusChangeAnswer is the 200 answer of the route: the issue the call names, as the PATCH
// answers it, and each issue the call changed.
type statusChangeAnswer struct {
	Issue   model.Issue         `json:"issue"`
	Changed []statusChangeEntry `json:"changed"`
}

// statusChangeEntry is one issue whose status or parent the call changed, in the order of the
// writes: the status and the parent the call left it with, its last_seq as its own write read it
// under its owner lock, and the issue as that write answered it. A dry run's entry holds the
// status and the parent the call would leave, the issue's last_seq now, and no issue.
type statusChangeEntry struct {
	Key             string       `json:"key"`
	Status          string       `json:"status"`
	Parent          *string      `json:"parent"`
	PreviousLastSeq int          `json:"previous_last_seq"`
	Issue           *model.Issue `json:"issue,omitempty"`
}

// answeredStatusChangeEntry is the entry of a step whose write answered issue: the status and the
// parent the write left it with, previousLastSeq its last_seq as the write read it under its owner
// lock, and the issue itself.
func answeredStatusChangeEntry(issue model.Issue, previousLastSeq int) statusChangeEntry {
	return statusChangeEntry{Key: issue.Key, Status: issue.Status, Parent: issue.Parent, PreviousLastSeq: previousLastSeq, Issue: &issue}
}

// statusChangeRefusal is every refusal of the route but 409 PARENT_STATUS_CONFLICT, the handler's
// and its request key's (refuseStatusChangeKey): its code and its message, and the entries of the
// writes that committed before it, [] when none did. Every other layer's refusal reads as this
// shape too (addChangedToRefusals).
type statusChangeRefusal struct {
	Code    string              `json:"code"`
	Error   string              `json:"error"`
	Changed []statusChangeEntry `json:"changed"`
}

// statusChangeConflict is the route's 409 PARENT_STATUS_CONFLICT: the rule's refusal
// (parentStatusConflictBody), and the entries of the writes that committed before it.
type statusChangeConflict struct {
	Code    string                      `json:"code"`
	Error   string                      `json:"error"`
	Issues  []parentStatusConflictIssue `json:"issues"`
	Changed []statusChangeEntry         `json:"changed"`
}

// statusChange is a status change's body as the route checked it.
type statusChange struct {
	status                                         string
	includeChildren, unlinkChildren, includeParent bool
	dryRun                                         bool
	expectedSeq                                    *int64
}

// parseStatusChange checks the body: a status of the lifecycle, and at most one of the two
// options that act on the issues under the issue.
func parseStatusChange(input statusChangeInput) (statusChange, error) {
	status := strings.TrimSpace(input.Status)
	if status == "" {
		return statusChange{}, errorf(http.StatusBadRequest, "STATUS_CHANGE_INPUT", "status is required")
	}
	if !model.IsIssueStatus(status) {
		return statusChange{}, errorf(http.StatusBadRequest, "INVALID_STATUS", "status is not in the Legion lifecycle")
	}
	if input.IncludeChildren && input.UnlinkChildren {
		return statusChange{}, errorf(http.StatusBadRequest, "STATUS_CHANGE_INPUT",
			"include_children and unlink_children cannot both be set: pick what happens to the open issues under the issue")
	}
	return statusChange{
		status:          status,
		includeChildren: input.IncludeChildren,
		unlinkChildren:  input.UnlinkChildren,
		includeParent:   input.IncludeParent,
		dryRun:          input.DryRun,
		expectedSeq:     input.ExpectedSeq,
	}, nil
}

// changeIssueStatus is POST /api/v1/issues/{key}/status-change. It plans the call and checks the
// plan under its owner locks (planStatusChange), answers a dry run from that plan, and otherwise
// runs each step through the PATCH's write core. A refused step stops the call: no later step
// runs, and the refusal carries the entries of the steps that committed before it. An attempt
// that took over a key an earlier attempt left unanswered resumes the call from that attempt's
// recovery point (resumedStatusChange).
func (s *server) changeIssueStatus(w http.ResponseWriter, r *http.Request) {
	none := []statusChangeEntry{}
	var input statusChangeInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeStatusChangeRefusal(w, err, none)
		return
	}
	writer, err := s.requestWriter(r, input.Actor)
	if err != nil {
		s.writeStatusChangeRefusal(w, err, none)
		return
	}
	change, err := parseStatusChange(input)
	if err != nil {
		s.writeStatusChangeRefusal(w, err, none)
		return
	}
	ctx, key := r.Context(), r.PathValue("key")
	resumed, err := s.resumedStatusChange(ctx)
	if err != nil {
		s.writeStatusChangeRefusal(w, err, none)
		return
	}
	callerProject, _ := s.callerProject(r)
	run := &statusChangeRun{
		server: s, key: key, change: change, writer: writer, callerName: statusChangeCallerName(writer.actor),
		callerProject: callerProject, outliving: outlivingTheClient(r), resumed: resumed, pointed: resumed != nil,
	}
	plan, err := s.planStatusChange(ctx, key, change, writer, resumed)
	// A dry run answers what the call answers with the rule on, whatever the setting.
	if err == nil && plan.conflict != nil && (change.dryRun || s.deps.ParentStatusRule.On()) {
		err = plan.conflict
	}
	if err == nil && change.dryRun {
		WriteJSON(w, http.StatusOK, plan.dryRunAnswer())
		return
	}
	var ran []statusChangeEntry
	if err == nil {
		ran, err = run.steps(ctx, plan.steps)
	}
	run.answer(ctx, w, ran, err)
}

// statusChangeStep is one step of a status change's plan: the write of one issue, the option that
// makes it (empty for the named issue's own step), and the issue's last_seq as the plan read it,
// which a dry run answers.
type statusChangeStep struct {
	issueWriteStep
	option  string
	lastSeq int
}

// statusChangePlan is a status change as its planning transaction read it under its owner locks:
// the issue the call names, every step in the order the steps run, and the rule's conflict with
// the whole plan, nil when the plan keeps the rule.
type statusChangePlan struct {
	named    model.Issue
	steps    []statusChangeStep
	conflict *parentStatusConflict
}

// dryRunAnswer is what a dry run answers: each issue the call would change with the status and
// the parent the call would leave and its last_seq now.
func (plan statusChangePlan) dryRunAnswer() statusChangeAnswer {
	changed := make([]statusChangeEntry, 0, len(plan.steps))
	for _, step := range plan.steps {
		entry := statusChangeEntry{Key: step.key, Status: step.statusBefore, Parent: step.parentBefore, PreviousLastSeq: step.lastSeq}
		if step.statusAfter != nil {
			entry.Status = *step.statusAfter
		}
		if step.reparent {
			entry.Parent = step.parentAfter
		}
		changed = append(changed, entry)
	}
	return statusChangeAnswer{Issue: plan.named, Changed: changed}
}

// statusChangeRead is what a status change's plan is made from: the issue the call names, its
// ancestors, parent first, when the call passes include_parent, and every issue under it when
// the call passes include_children or unlink_children.
type statusChangeRead struct {
	named       ruleIssue
	ancestors   []ruleIssue
	descendants []refs.IssueDescendant
}

// readStatusChange reads what the plan of change on the issue key is made from, with plain reads
// through q. An issue no row has is pgx.ErrNoRows, which answers 404 as the PATCH answers it.
func readStatusChange(ctx context.Context, q queryer, key string, change statusChange) (statusChangeRead, error) {
	read := statusChangeRead{named: ruleIssue{key: key}}
	if err := q.QueryRow(ctx, `select status, parent_key from issues where key = $1`, key).Scan(&read.named.status, &read.named.parent); err != nil {
		return statusChangeRead{}, err
	}
	if change.includeParent {
		above, err := refs.IssueAncestors(ctx, q, key)
		if err != nil {
			return statusChangeRead{}, err
		}
		rows, err := readIssueWriteRows(ctx, q, above)
		if err != nil {
			return statusChangeRead{}, err
		}
		for _, ancestor := range above {
			if row, ok := rows[ancestor]; ok {
				read.ancestors = append(read.ancestors, ruleIssue{key: ancestor, status: row.status, parent: row.parent})
			}
		}
	}
	if change.includeChildren || change.unlinkChildren {
		descendants, err := refs.IssueDescendantRows(ctx, q, everyReader, key)
		if err != nil {
			return statusChangeRead{}, err
		}
		read.descendants = descendants
	}
	return read, nil
}

// steps is the plan's steps over read, in the order they run. Each option acts whenever the call
// passes it, whether or not the rule needs it.
func (read statusChangeRead) steps(change statusChange) []statusChangeStep {
	var steps []statusChangeStep
	if change.includeParent {
		inProgress := "in_progress"
		for index := len(read.ancestors) - 1; index >= 0; index-- {
			ancestor := read.ancestors[index]
			if statusGroup(ancestor.status) == groupInWork {
				continue
			}
			steps = append(steps, statusChangeStep{option: optionIncludeParent, issueWriteStep: issueWriteStep{
				key: ancestor.key, statusBefore: ancestor.status, statusAfter: &inProgress, parentBefore: ancestor.parent,
			}})
		}
	}
	named := read.named.key
	if change.unlinkChildren {
		for _, child := range read.descendants {
			if child.Depth != 1 || statusGroup(child.Status) == groupDone {
				continue
			}
			steps = append(steps, statusChangeStep{option: optionUnlinkChildren, issueWriteStep: issueWriteStep{
				key: child.Key, statusBefore: child.Status, parentBefore: &named, reparent: true,
			}})
		}
	}
	var moved []statusChangeStep
	if change.includeChildren {
		for _, descendant := range read.descendants {
			if statusGroup(descendant.Status) == groupDone || statusGroup(descendant.Status) == statusGroup(change.status) {
				continue
			}
			parent := descendant.Parent
			moved = append(moved, statusChangeStep{option: optionIncludeChildren, issueWriteStep: issueWriteStep{
				key: descendant.Key, statusBefore: descendant.Status, statusAfter: &change.status, parentBefore: &parent,
			}})
		}
	}
	intoWork := statusGroup(change.status) == groupInWork
	if !intoWork {
		// Deepest first, so each issue's children are moved before it is.
		depths := make(map[string]int, len(read.descendants))
		for _, descendant := range read.descendants {
			depths[descendant.Key] = descendant.Depth
		}
		slices.SortStableFunc(moved, func(a, b statusChangeStep) int { return cmp.Compare(depths[b.key], depths[a.key]) })
		steps = append(steps, moved...)
	}
	steps = append(steps, statusChangeStep{issueWriteStep: issueWriteStep{
		key: named, statusBefore: read.named.status, statusAfter: &change.status, parentBefore: read.named.parent,
	}})
	if intoWork {
		steps = append(steps, moved...)
	}
	return steps
}

// planStatusChange is the plan of change on the issue key, checked under its owner locks before
// any step writes. It reads the plan with plain reads, then, in a transaction of its own that
// writes nothing, takes the owner locks of every step's issue and of each step's current and
// new parent in one store.LockOwners call. Under them it loads the issue the call names and
// decides expected_sequence first (409 ISSUE_CHANGED), then the closed-issue gate as the PATCH has
// it (409 ISSUE_CLOSED), reads the plan again, and checks the whole plan (refuseIssueWritePlan). A
// plan whose issues moved between the two reads, so that it needs a lock the transaction does not
// hold, starts over with what the second read found, as lockPatchOwnersAndParent does, and a plan
// that moves at every attempt is 409 PARENT_CONTENDED with nothing applied. The transaction rolls
// back before the first step, and each step takes its own locks in its own transaction. resumed is
// the recovery point of an earlier attempt that this attempt resumes, nil for a first attempt: it
// changes the fence, the gate and the plan as planStatusChangeUnderLocks says.
func (s *server) planStatusChange(
	ctx context.Context, key string, change statusChange, writer issueWriter, resumed *statusChangeRecovery,
) (statusChangePlan, error) {
	pool, err := s.readyPool()
	if err != nil {
		return statusChangePlan{}, err
	}
	read, err := readStatusChange(ctx, pool, key, change)
	if err != nil {
		return statusChangePlan{}, err
	}
	for range parentLockAttempts {
		plan, moved, err := s.planStatusChangeUnderLocks(ctx, key, change, writer, read, resumed)
		if err != nil || moved == nil {
			return plan, err
		}
		read = *moved
	}
	return statusChangePlan{}, errorf(http.StatusConflict, parentContended,
		"the issues that the status change of %s writes changed at each of %d attempts while this request waited to lock them, so nothing was applied: read the issue and retry",
		key, parentLockAttempts)
}

// planStatusChangeUnderLocks is one attempt of planStatusChange, which locks the owners the plan
// over read needs. It answers the plan, or, when the second read needs a lock the attempt does not
// hold, that read, for the next attempt to lock.
//
// An attempt that resumes a call from resumed, its recovery point, decides the fence by event ids
// rather than by the PATCH's equality, which the earlier attempt's own events moved: an event on
// the named issue after expected_sequence that the point does not hold is 409 ISSUE_CHANGED, as
// the uninterrupted call would have answered at its next step. It plans every step of the call
// whose entry the point does not hold, since a step counts as made only when the point holds its
// entry, and runs no closed-issue gate on the named issue when the point holds its entry: the
// call's own close closed it.
func (s *server) planStatusChangeUnderLocks(
	ctx context.Context, key string, change statusChange, writer issueWriter, read statusChangeRead, resumed *statusChangeRecovery,
) (statusChangePlan, *statusChangeRead, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return statusChangePlan{}, nil, err
	}
	defer tx.Rollback(ctx)
	held := issueWriteLocks(read.steps(change))
	if err := lockIssueWriteOwners(ctx, tx, held); err != nil {
		return statusChangePlan{}, nil, err
	}
	named, err := s.loadIssue(ctx, tx, key)
	if err != nil {
		return statusChangePlan{}, nil, err
	}
	if change.expectedSeq != nil {
		if resumed != nil {
			if err := resumed.fence(*change.expectedSeq).refuseForeignEvents(ctx, tx, key); err != nil {
				return statusChangePlan{}, nil, err
			}
		} else if *change.expectedSeq != int64(named.LastSeq) {
			return statusChangePlan{}, nil, errIssueChanged()
		}
	}
	if named.ClosedAt != nil && change.status == "done" && !resumed.holds(key) {
		return statusChangePlan{}, nil, errorf(http.StatusConflict, "ISSUE_CLOSED", "issue is closed")
	}
	again, err := readStatusChange(ctx, tx, key, change)
	if err != nil {
		return statusChangePlan{}, nil, err
	}
	steps := slices.DeleteFunc(again.steps(change), func(step statusChangeStep) bool { return resumed.holds(step.key) })
	if !locksHeld(held, steps) {
		return statusChangePlan{}, &again, nil
	}
	keys := make([]string, 0, len(steps))
	for _, step := range steps {
		keys = append(keys, step.key)
	}
	rows, err := readIssueWriteRows(ctx, tx, keys)
	if err != nil {
		return statusChangePlan{}, nil, err
	}
	writes := issueWritePlan{steps: make([]issueWriteStep, 0, len(steps)), includeChildren: change.includeChildren}
	for index := range steps {
		row := rows[steps[index].key]
		steps[index].title, steps[index].lastSeq = row.title, row.lastSeq
		writes.steps = append(writes.steps, steps[index].issueWriteStep)
	}
	conflict, err := s.refuseIssueWritePlan(ctx, tx, writer, writes)
	if err != nil {
		return statusChangePlan{}, nil, err
	}
	// The check of who writes an issue Legion runs (spec_writer.go), for every step of the plan
	// under the plan's locks and before any step writes, so a call whose later step the check
	// refuses writes no earlier step either. Each step's writeIssue checks again under its own
	// locks.
	specWriter := writer.specWriter()
	for _, step := range writes.steps {
		if err := refuseSpecWriterOfIssueWrite(ctx, tx, specWriter, step); err != nil {
			return statusChangePlan{}, nil, err
		}
	}
	return statusChangePlan{named: named, steps: steps, conflict: conflict}, nil, nil
}

// statusChangeRun is one attempt of a status change that runs its steps. callerName is the caller
// the comments of its steps name (statusChangeCallerName). resumed is the recovery point of the
// earlier attempt this one resumes (resumedStatusChange), nil for a first attempt. pointed is that
// the request key holds a recovery point, which the call marks finished after its last step: the
// point this attempt resumed, or one a step of this keyed attempt wrote. named is the result of
// the named issue's own step, when this attempt ran it (namedRan).
type statusChangeRun struct {
	server        *server
	key           string
	change        statusChange
	writer        issueWriter
	callerName    string
	callerProject string
	outliving     context.Context
	resumed       *statusChangeRecovery
	pointed       bool
	named         issueWriteResult
	namedRan      bool
}

// steps runs each step of the plan through the PATCH's write core, in a transaction of its own,
// and answers the entry of each issue it changed. A step refused stops the call: it answers that
// refusal, with the entries of the steps that committed before it. A call that sent
// expected_sequence is fenced at each step (issueWriteFence), with every event an earlier step
// appended on the issue the call names counted as the call's own, those of the point it resumes
// included. Each step of a keyed call that writes adds its entry to the call's recovery point in
// its own transaction (writeIssue, queueStatusChangeStep), under the request key's claim that
// each step carries (issueWriteCallStep.claim). The close of the issue the call names leaves its
// ask sweep to the call's answer (statusChangeRun.answer).
func (run *statusChangeRun) steps(ctx context.Context, steps []statusChangeStep) ([]statusChangeEntry, error) {
	s := run.server
	ran := []statusChangeEntry{}
	var fence *issueWriteFence
	if run.change.expectedSeq != nil {
		fence = run.resumed.fence(*run.change.expectedSeq)
	}
	var claim *requestKeyClaim
	if held, keyed := ctx.Value(requestKeyClaimKey{}).(requestKeyClaim); keyed {
		claim = &held
	}
	for index, step := range steps {
		written, err := s.writeIssue(ctx, run.call(step, fence, claim))
		if err != nil {
			return ran, err
		}
		if step.key == run.key {
			run.named, run.namedRan = written, true
		}
		if !written.wrote {
			continue
		}
		if fence != nil {
			for _, id := range written.namedEvents {
				fence.own[id] = true
			}
		}
		ran = append(ran, answeredStatusChangeEntry(written.issue, written.previousLastSeq))
		run.pointed = run.pointed || claim != nil
		if s.statusChangeAfterStep != nil {
			s.statusChangeAfterStep(ctx, index)
		}
	}
	return ran, nil
}

// answer ends the attempt, refused by err or not, with ran the entries of its own steps. After the
// last step it takes the answer's issue (namedIssue) and marks the call's recovery point finished
// with it, when the request key holds one (markStatusChangeFinished). Then it runs the ask sweep of
// the named issue's close (sweep), since a close that committed is owed its sweep whatever the
// answer, and answers changed: the entries of the point it resumed, as their steps answered them,
// followed by ran. An attempt whose key another attempt took over sweeps nothing: the attempt that
// holds the key sweeps instead.
func (run *statusChangeRun) answer(ctx context.Context, w http.ResponseWriter, ran []statusChangeEntry, err error) {
	s := run.server
	var issue model.Issue
	if err == nil {
		issue, err = run.namedIssue()
	}
	if err == nil && run.pointed {
		err = s.markStatusChangeFinished(ctx, issue)
	}
	if !errors.Is(err, errRequestKeyTakenOver) {
		run.sweep()
	}
	changed := []statusChangeEntry{}
	if run.resumed != nil {
		changed = append(changed, run.resumed.Changed...)
	}
	changed = append(changed, ran...)
	if err != nil {
		s.writeStatusChangeRefusal(w, err, changed)
		return
	}
	WriteJSON(w, http.StatusOK, statusChangeAnswer{Issue: issue, Changed: changed})
}

// namedIssue is the answer's issue, the named issue as its own step answered it, as the
// uninterrupted call answers it: this attempt's step's, when it ran that step (namedRan), and
// otherwise the entry of that step in the point this attempt resumed, which holds it, since an
// attempt that resumes plans no step its point holds. A point that holds no such entry is a fault
// of the server's own.
func (run *statusChangeRun) namedIssue() (model.Issue, error) {
	if run.namedRan {
		return run.named.issue, nil
	}
	if run.resumed != nil {
		for _, entry := range run.resumed.Changed {
			if entry.Key == run.key && entry.Issue != nil {
				return *entry.Issue, nil
			}
		}
	}
	return model.Issue{}, fmt.Errorf("the status change of %s ran no step of it, and its recovery point holds no entry of that step with its issue", run.key)
}

// sweep runs the ask sweep of the named issue's close (sweepClosedIssueAsks), with no transaction
// open: of the close this attempt's own step made, with the asks it withdrew, or of the close the
// point it resumed holds, with the ids of the asks that close withdrew, which are all the sweep
// reads of them (sweepClosedIssueAsksTx).
func (run *statusChangeRun) sweep() {
	s := run.server
	switch {
	case run.named.closed:
		s.sweepClosedIssueAsks(run.outliving, run.key, run.named.withdrawn, run.writer.actor)
	case run.resumed.closes(run.key):
		withdrawn := make([]model.Ask, 0, len(run.resumed.Withdrawn))
		for _, id := range run.resumed.Withdrawn {
			withdrawn = append(withdrawn, model.Ask{ID: id})
		}
		s.sweepClosedIssueAsks(run.outliving, run.key, withdrawn, run.writer.actor)
	}
}

// call is the write of one step: the status or the unlink the step writes, as the caller writes
// it, and, for a step an option makes, the comment of Dispatch's system actor that names the call.
// claim is the request key's claim of a keyed call, nil for a call with no key.
func (run *statusChangeRun) call(step statusChangeStep, fence *issueWriteFence, claim *requestKeyClaim) issueWriteCall {
	call := issueWriteCall{
		writer:        run.writer,
		key:           step.key,
		callerProject: run.callerProject,
		outliving:     run.outliving,
		adviceRoute:   statusChangeRoute,
		step:          &issueWriteCallStep{named: run.key, fence: fence, claim: claim, sweepsLater: step.key == run.key},
	}
	if step.reparent {
		call.unlinkFrom(run.key)
	} else {
		status := *step.statusAfter
		call.fields.Status = &status
	}
	if step.option == "" {
		return call
	}
	call.comment = &issueComment{author: parentStatusRuleActor}
	named, status := run.key, run.change.status
	because := fmt.Sprintf("because %s's status change of %s passed `%s`. %s", run.callerName, named, step.option, statusChangeRuleSentence)
	switch step.option {
	case optionIncludeChildren:
		call.step.leaves = func(before model.Issue) bool {
			return statusGroup(before.Status) == groupDone || statusGroup(before.Status) == statusGroup(status)
		}
		call.comment.words = func(before model.Issue) string {
			return fmt.Sprintf("Dispatch moved this issue from `%s` to `%s` with %s, %s", before.Status, status, named, because)
		}
	case optionIncludeParent:
		call.step.leaves = func(before model.Issue) bool { return statusGroup(before.Status) == groupInWork }
		call.comment.words = func(before model.Issue) string {
			return fmt.Sprintf("Dispatch moved this issue from `%s` to `%s`, %s", before.Status, *step.statusAfter, because)
		}
	case optionUnlinkChildren:
		call.comment.words = func(model.Issue) string {
			return fmt.Sprintf("Dispatch removed this issue from %s, %s", named, because)
		}
	}
	return call
}

// statusChangeCallerName is the caller a status change's comments name, from what the request's
// credential proved alone, as plain text: a person's login, the person a token acts for (Owner),
// the service a token proves (Service), or a session a broker-issued token proved (Proven). Any
// other caller is a session that only the request's body named, under a credential that proves
// none (the deployment's shared token): its id is the caller's own text, in a comment that
// Dispatch's system actor signs, so the comment quotes it as code (claimedSessionName).
func statusChangeCallerName(actor model.Actor) string {
	switch {
	case actor.Kind == "user":
		return actor.ID
	case actor.Owner != nil:
		return *actor.Owner
	case actor.Service != nil:
		return *actor.Service
	case actor.Proven:
		return actor.ID
	}
	return claimedSessionName(actor.ID)
}

// claimedSessionNameMax is the most runes of a session's id that claimedSessionName quotes: room
// for an id as long as a uuid (36 characters) and then some, and a bound on what a caller's own
// text adds to a comment that Dispatch signs.
const claimedSessionNameMax = 64

// claimedSessionName is the id of a session that only a request's body named, as a comment quotes
// it: an inline code span of the id with every backtick, every line ending and every other control
// character removed, its spaces at both ends trimmed, and cut to claimedSessionNameMax runes, so
// no text of the caller's leaves the span as a link, a new line or a block. An id with nothing
// left is "a session".
func claimedSessionName(id string) string {
	quoted := strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == '`' || unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return -1
		}
		return r
	}, id))
	quoted = strings.TrimSpace(text.HeadRunes(quoted, claimedSessionNameMax))
	if quoted == "" {
		return "a session"
	}
	return "`" + quoted + "`"
}

// writeStatusChangeRefusal answers err as every other route answers it (writeAuthenticationError,
// which answers every error that is not an identity's as writeHandlerError does), with changed
// beside the refusal: the entries of the steps that committed before it, [] when changed is nil.
// It shapes the handler's refusals and the keyed layer's (refuseStatusChangeKey); every other
// layer's gets changed [] (addChangedToRefusals).
func (s *server) writeStatusChangeRefusal(w http.ResponseWriter, err error, changed []statusChangeEntry) {
	if changed == nil {
		changed = []statusChangeEntry{}
	}
	var conflict *parentStatusConflict
	if errors.As(err, &conflict) {
		body := conflict.body()
		WriteJSON(w, http.StatusConflict, statusChangeConflict{Code: body.Code, Error: body.Error, Issues: body.Issues, Changed: changed})
		return
	}
	refused := &errorAnswer{header: http.Header{}, status: http.StatusOK}
	s.writeAuthenticationError(refused, err)
	var body errorBody
	if decodeErr := json.Unmarshal(refused.body.Bytes(), &body); decodeErr != nil {
		// A server fault: writeHandlerError answers it 500 {code, error}, so it recurses once.
		s.writeStatusChangeRefusal(w, fmt.Errorf("read the refusal of a status change: %w", decodeErr), changed)
		return
	}
	maps.Copy(w.Header(), refused.header)
	WriteJSON(w, refused.status, statusChangeRefusal{Code: body.Code, Error: body.Error, Changed: changed})
}

// refuseStatusChangeKey is the route's keyedRoute.refuse: a refusal the request-key layer decides
// itself, err, answered as writeStatusChangeRefusal answers one, with changed the entries recovery
// holds, and [] where recovery is nil. A KEY_IN_FLIGHT answered before the repeat's body is judged
// (answerDrainedRepeat's Expect: 100-continue and drain-error answers) names the running request's
// point whatever the repeat's body; elsewhere a point is passed only once the digest matched.
// recovery is nil where the layer read none, where the row is another request's (KEY_REUSED), and
// where it is past its window.
func (s *server) refuseStatusChangeKey(w http.ResponseWriter, err error, recovery json.RawMessage) {
	var point statusChangeRecovery
	if recovery != nil {
		if decodeErr := json.Unmarshal(recovery, &point); decodeErr != nil {
			s.writeStatusChangeRefusal(w, fmt.Errorf("read the recovery point of a status change: %w", decodeErr), nil)
			return
		}
	}
	s.writeStatusChangeRefusal(w, err, point.Changed)
}

// The members of a status change's recovery point (statusChangeRecovery), each named here once:
// every statement that writes the point or reads it names them by these constants, and so does
// the point's decoding (statusChangeRecovery.UnmarshalJSON).
const (
	// pointChanged is the entry of each step that wrote, which recoveryPointRead gathers from
	// request_key_steps.
	pointChanged = "changed"
	// pointEvents is the ids of the events the steps appended on the issue the call names.
	pointEvents = "events"
	// pointWithdrawn is the ids of the asks the named issue's own close withdrew.
	pointWithdrawn = "withdrawn"
	// pointFinished is the mark that every step of the call ran.
	pointFinished = "finished"
	// pointIssue is the answer's issue, written with the mark.
	pointIssue = "issue"
)

// statusChangeRecovery is the recovery point of a keyed status change (request_keys.go): what
// the steps that committed made, from which a repeat finishes the call. Each step that writes adds
// to it in its own transaction, right after its write phase, with a statement that builds the
// head's members in its SQL and adds its entry as a row of request_key_steps
// (queueStatusChangeStep), and the call marks it finished, with the answer's issue, once its last
// step has run (markStatusChangeFinished). Every reader reads it with the steps' entries under
// changed (recoveryPointRead). Go only reads it, by the members' names above (UnmarshalJSON).
type statusChangeRecovery struct {
	// Changed is the entry of each step that wrote, in the order the steps committed, each with
	// its issue as its step's write answered it, which every answer from the point carries.
	Changed []statusChangeEntry
	// Events is the ids of every event the steps appended on the issue the call names, which the
	// fence of a resumed attempt counts as the call's own.
	Events []int64
	// Withdrawn is the ids of the asks that the close of the issue the call names withdrew, which
	// that close's ask sweep takes.
	Withdrawn []string
	// Finished is the mark that every step of the call ran.
	Finished bool
	// Issue is the answer's issue (statusChangeRun.namedIssue), written with the mark, which a
	// replay answers.
	Issue *model.Issue
}

// UnmarshalJSON reads the point by its members' names, the constants above that every statement
// writing or reading the point names too, so a reader and a writer cannot name a member apart and
// read it as empty. A member the point does not hold stays empty.
func (point *statusChangeRecovery) UnmarshalJSON(data []byte) error {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		return err
	}
	for name, member := range members {
		var into any
		switch name {
		case pointChanged:
			into = &point.Changed
		case pointEvents:
			into = &point.Events
		case pointWithdrawn:
			into = &point.Withdrawn
		case pointFinished:
			into = &point.Finished
		case pointIssue:
			into = &point.Issue
		default:
			continue
		}
		if err := json.Unmarshal(member, into); err != nil {
			return fmt.Errorf("read %s of a status change's recovery point: %w", name, err)
		}
	}
	return nil
}

// holds reports whether the point holds the entry of key's step, which counts that step as made.
// A nil point holds none.
func (point *statusChangeRecovery) holds(key string) bool {
	return point != nil && slices.ContainsFunc(point.Changed, func(entry statusChangeEntry) bool { return entry.Key == key })
}

// closes reports whether the point holds the close of named, the issue the call names, by its own
// step: an entry of named at done, which a step writes only when it closes the issue, since the
// PATCH's gate refuses done on an issue already closed.
func (point *statusChangeRecovery) closes(named string) bool {
	return point != nil && slices.ContainsFunc(point.Changed, func(entry statusChangeEntry) bool {
		return entry.Key == named && entry.Status == "done"
	})
}

// fence is the fence of a call that sent expected_sequence sequence, with every event the point
// holds counted as the call's own. A nil point holds none.
func (point *statusChangeRecovery) fence(sequence int64) *issueWriteFence {
	own := map[int64]bool{}
	if point != nil {
		for _, id := range point.Events {
			own[id] = true
		}
	}
	return &issueWriteFence{sequence: sequence, own: own}
}

// resumedStatusChange is the recovery point that an earlier attempt of the keyed request ctx
// carries left, when this attempt took that request's key over (claimRequestKey), with the steps'
// entries (recoveryPointRead): the call this attempt finishes. It is nil for every other request:
// one with no key, a first attempt, and an attempt that took over a key with no point, which runs
// the call as a first attempt does.
func (s *server) resumedStatusChange(ctx context.Context) (*statusChangeRecovery, error) {
	claim, keyed := ctx.Value(requestKeyClaimKey{}).(requestKeyClaim)
	if !keyed || claim.attempt == 1 {
		return nil, nil
	}
	pool, err := s.readyPool()
	if err != nil {
		return nil, err
	}
	var recovery json.RawMessage
	err = pool.QueryRow(ctx, `
		select `+recoveryPointRead+` from request_keys where login = $1 and idempotency_key = $2 and attempt = $3
	`, claim.login, claim.key, claim.attempt).Scan(&recovery)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errRequestKeyTakenOver
	}
	if err != nil {
		return nil, fmt.Errorf("read the recovery point of a status change: %w", err)
	}
	if recovery == nil {
		return nil, nil
	}
	var point statusChangeRecovery
	if err := json.Unmarshal(recovery, &point); err != nil {
		return nil, fmt.Errorf("read the recovery point of a status change: %w", err)
	}
	return &point, nil
}

// markStatusChangeFinished writes into the recovery point of the keyed status change ctx carries
// the mark that every step of the call ran, with issue, the answer's issue, once the last step has
// run and before the call answers. A repeat that finds the mark is answered from the point, with
// that issue, and never takes the key over. It runs in a transaction of its own that s.begin
// opens, whose only write is to request_keys, which the commit step does not count
// (markRequestKeyCommitted). The statement is scoped to the request's own claim, so an attempt
// whose key another attempt took over is refused (errRequestKeyTakenOver). A request with no key
// marks nothing.
func (s *server) markStatusChangeFinished(ctx context.Context, issue model.Issue) error {
	claim, keyed := ctx.Value(requestKeyClaimKey{}).(requestKeyClaim)
	if !keyed {
		return nil
	}
	encoded, err := json.Marshal(issue)
	if err != nil {
		return fmt.Errorf("mark a status change finished: %w", err)
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `
		update request_keys set recovery = recovery || jsonb_build_object('`+pointFinished+`', true, '`+pointIssue+`', $4::jsonb)
		where login = $1 and idempotency_key = $2 and attempt = $3 and status is null
	`, claim.login, claim.key, claim.attempt, encoded)
	if err != nil {
		return fmt.Errorf("mark a status change finished: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errRequestKeyTakenOver
	}
	return tx.Commit(ctx)
}

// queueStatusChangeStep queues on b, the batch that runs right after the write phase of one step
// of a keyed status change (docs.Ledger.AfterWrites), the statement that writes the step into the
// call's recovery point, under the request key's claim the step carries (step.claim). It adds to
// the head the key holds, which is the call's point so far, since only the attempt that holds the
// key writes it: the ids of the events on the issue the call names (step.named) after
// namedLastSeq, its last_seq as the step read it under that issue's owner lock, when the step holds
// that lock (holdsNamed), and withdrawn, the ids of the asks that the named issue's own close
// withdrew. It adds entry, the step's entry with its issue as the step's write answered it, as a
// row of request_key_steps. The event ids exist, and the issue's last_seq and a close's closed_at
// are known, only once the batch has run, so the statement derives the ids in its own SQL, after
// the step's last event insert, and its callback reads them into events, the step's own events on
// the named issue (issueWriteResult.namedEvents). The statement is scoped to the request's own
// claim, so a step whose key another attempt took over rolls back (errRequestKeyTakenOver).
func queueStatusChangeStep(
	b *store.WriteBatch, step *issueWriteCallStep, entry statusChangeEntry, namedLastSeq int, holdsNamed bool,
	withdrawn []string, events *[]int64,
) error {
	encoded, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	claim := step.claim
	b.Queue("write the recovery point of a status change", `
		with named as (
			select coalesce(array_agg(id order by id), '{}') as ids from events
			where $4 and issue_key = $5 and seq > $6
		), head as (
			update request_keys set recovery = jsonb_build_object(
				'`+pointEvents+`', coalesce(recovery -> '`+pointEvents+`', '[]') || to_jsonb(named.ids),
				'`+pointWithdrawn+`', coalesce(recovery -> '`+pointWithdrawn+`', '[]') || coalesce(to_jsonb($8::text[]), '[]'))
			from named
			where login = $1 and idempotency_key = $2 and attempt = $3 and status is null
			returning named.ids
		), step as (
			insert into request_key_steps (login, idempotency_key, entry) select $1, $2, $7::jsonb from head
		)
		select ids from head
	`, claim.login, claim.key, claim.attempt, holdsNamed, step.named, namedLastSeq, encoded, withdrawn,
	).QueryRow(func(row pgx.Row) error {
		err := row.Scan(events)
		if errors.Is(err, pgx.ErrNoRows) {
			return errRequestKeyTakenOver
		}
		return err
	})
	return nil
}

// replayStatusChange answers a repeat of a keyed status change from its recovery point: a call
// whose point carries the mark, or one that answered a success beside its point. It answers the
// 200 of the call with what the point stores, the entries as their steps answered them and the
// answer's issue the mark was written with, and reads nothing. No step and no fence runs again, so
// a later write of an issue the call changed, its close's ask sweep among them, changes no answer
// of a call that finished. A point with no answer's issue is a fault of the server's own, since
// every finished point this build writes holds one, and is answered 500.
func (s *server) replayStatusChange(w http.ResponseWriter, _ *http.Request, point statusChangeRecovery, _ []byte) {
	if point.Issue == nil {
		s.writeStatusChangeRefusal(w, errors.New("the recovery point of a finished status change holds no answer's issue"), point.Changed)
		return
	}
	WriteJSON(w, http.StatusOK, statusChangeAnswer{Issue: *point.Issue, Changed: point.Changed})
}
