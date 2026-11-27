package workflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// statusCall is the workflow's half of an API call that writes the status of issues in Dispatch:
// the controller's set_status, an architect's rerun_child and an architect's release_children
// (intake.StatusCall; decision 5 of dispatch://INFRA-4730). The Mirror has written what the route
// read of Dispatch into the copy, in this same transaction, so the whole call is decided over the
// copy as it stands now (record.ReadChains), each issue before anything is written: a call one of
// whose issues is refused writes nothing, and its refusal answers the route (judgeCallIssue).
//
// Then each write is queued as a status row on the issue's lane, in the call's order, and counted
// on the issue's record before Dispatch echoes it (countCallWrite, R16 of dispatch://INFRA-3320):
// a status out of work is counted before its row is queued, since the leave it makes drops every
// status write the daemon queued for the issue before it, and a todo after, so the status of the
// phase its return or its new run reaches is written over it. Each row is fenced by the run and
// the phase entry the issue stands at once the fact has settled (record.Store.RefenceFactRows). A
// child release_children writes under a parent the same call writes waits for that parent's row
// (record.StatusWrite.After), since Dispatch takes the child's todo only once the parent is in work.
// A write the controller asked for names it (record.StatusWrite.Controller), so Dispatch's
// refusal of it is told to the controller (decision 12).
func (e *Engine) statusCall(ctx context.Context, tx pgx.Tx, call intake.StatusCall) (intake.Result, error) {
	keys := make([]string, len(call.Issues))
	for i, issue := range call.Issues {
		keys[i] = issue.Key
	}
	chains, err := record.ReadChains(ctx, tx, e.store, e.cfg.Project, keys)
	if err != nil {
		return intake.Result{}, err
	}
	writes := make([]callWrite, 0, len(call.Issues))
	written := map[string]bool{}
	for _, issue := range call.Issues {
		write, refusal, err := e.judgeCallIssue(ctx, tx, chains, call, issue, written)
		if err != nil || refusal != nil {
			return intake.Result{Refusal: refusal}, err
		}
		if write.status != "" {
			written[issue.Key] = true
		}
		writes = append(writes, write)
	}
	rows := map[string]int64{}
	for _, write := range writes {
		if err := e.applyCallWrite(ctx, tx, call, write, rows); err != nil {
			return intake.Result{}, err
		}
	}
	var queued []intake.QueuedWrite
	for _, write := range writes {
		if id, ok := rows[write.issue.Key]; ok {
			queued = append(queued, intake.QueuedWrite{Issue: write.issue.Key, Status: write.status, Row: id})
		}
	}
	e.logOnCommit(ctx, "workflow: a status call queued its writes", "call", call.Op, "architect", call.Architect, "writes", queued)
	return intake.Result{Queued: queued}, nil
}

// callWrite is one issue of a status call as judgeCallIssue decided it: the status the call
// writes, "" for none; the status Dispatch shows of it as the copy holds it, which the row observes
// until it runs; the daemon's record of it, nil for an issue it does not record; whether
// rerun_child opens a new run of it; and the parent of the same call whose row this one follows,
// "" for none.
type callWrite struct {
	issue    intake.StatusCallIssue
	status   string
	observed string
	record   *record.Issue
	newRun   bool
	follows  string
}

// judgeCallIssue decides issue of call over chains, the copy as this transaction reads it, before
// anything of the call is written. written is the issues of the call judged before it that the
// call writes. It answers the write, or the refusal that refuses the whole call.
//
// A chain the copy cannot show to its top is refused CHAIN_UNKNOWN, and an architect's call is held
// to the ownership rule (intake.CallRefusal): rerun_child and release_children take only an issue under
// the caller's own on the Dispatch chain. Then, by call:
//   - rerun_child runs again an issue the daemon records that is out of Legion's work, which it
//     writes todo: a child Dispatch shows in backlog or icebox, whoever wrote that status, opens a
//     new run (decision 8), and any other returns to work as a person's todo would (backInWork). An
//     issue the daemon does not record is refused ISSUE_NOT_FOUND, one still in work CHILD_RUNNING;
//   - release_children writes todo on a child Dispatch shows in triage, backlog or icebox, and
//     writes nothing on one it shows todo; any other is refused ISSUE_ALREADY_RELEASED;
//   - set_status writes backlog or icebox, or a todo on an issue out of Legion's work; a todo on an
//     issue in work writes no status, and is that issue's todo as a person's would be (a held
//     issue's release, a failed root architect's restart: backInWork).
//
// Neither rerun_child nor release_children takes an issue beneath an ancestor the daemon has
// already accepted (CHILD_UNDER_ACCEPTED_PARENT). Dispatch's parent rule asks for the rest (R24 of
// dispatch://INFRA-3320): a backlog or icebox that would leave a child in work under the issue
// takes set_status's children argument (CHILDREN_ARGUMENT_REQUIRED, naming each such child); a
// todo that would put the issue in work under a parent that is not takes the parent argument of
// set_status and rerun_child (PARENT_ARGUMENT_REQUIRED, naming each ancestor not in work up to the
// first that is); and release_children, which takes none, writes a child only under a parent in
// work or one the same call writes before it (PARENT_NOT_RELEASED).
func (e *Engine) judgeCallIssue(ctx context.Context, tx pgx.Tx, chains *record.Chains, call intake.StatusCall, issue intake.StatusCallIssue,
	written map[string]bool) (callWrite, *intake.Refusal, error) {
	key := issue.Key
	switch call.Op {
	case intake.RerunChildCall, intake.ReleaseChildrenCall:
		rule := intake.RerunCall
		if call.Op == intake.ReleaseChildrenCall {
			rule = intake.ReleaseCall
		}
		if refusal, err := intake.CallRefusal(ctx, tx, e.store, e.cfg.Project, chains, call.Architect, "", key, rule); err != nil || refusal != nil {
			return callWrite{}, refusal, err
		}
	default:
		if chain := chains.Chain(key); !chain.Known() {
			return callWrite{}, intake.ChainUnknown(key, chain), nil
		}
	}
	observed, _ := chains.Status(key)
	write := callWrite{issue: issue, observed: observed}
	if issue, recorded := chains.Record(key); recorded {
		write.record = &issue
	}
	switch call.Op {
	case intake.RerunChildCall:
		switch {
		case write.record == nil:
			return callWrite{}, &intake.Refusal{Status: 404, Code: "ISSUE_NOT_FOUND", Message: fmt.Sprintf(
				"%s is not recorded: rerun_child runs again an issue the daemon has run, and release_children releases one it has not", key)}, nil
		case record.InLegionsWork(*write.record):
			return callWrite{}, &intake.Refusal{Status: 409, Code: "CHILD_RUNNING", Message: childRunning(*write.record, observed)}, nil
		}
		write.status = "todo"
		write.newRun = observed == "backlog" || observed == "icebox"
	case intake.ReleaseChildrenCall:
		switch observed {
		case "triage", "backlog", "icebox":
			write.status = "todo"
		case "todo":
		default:
			return callWrite{}, &intake.Refusal{Status: 409, Code: "ISSUE_ALREADY_RELEASED", Message: fmt.Sprintf(
				"%s is %s; release_children releases only a triage, backlog, or icebox child", key, observed)}, nil
		}
	default:
		switch {
		case issue.Status != "todo":
			write.status = issue.Status
			if breach := childrenInWork(chains, key); len(breach) > 0 && issue.Children == "" {
				return callWrite{}, &intake.Refusal{Status: 409, Code: "CHILDREN_ARGUMENT_REQUIRED", Message: fmt.Sprintf(
					"%s %s would leave %s in work under an issue out of work, which Dispatch's parent rule refuses; set %s %s again with children include, which moves them with it, or unlink, which takes them out from under it",
					issue.Status, key, strings.Join(breach, ", "), key, issue.Status)}, nil
			}
		case write.record == nil || !record.InLegionsWork(*write.record):
			write.status = "todo"
		}
	}
	if call.Op != intake.SetStatusCall {
		if ancestor := record.AcceptedAncestorIn(chains, key, ""); ancestor != "" {
			again := "run it again"
			if call.Op == intake.ReleaseChildrenCall {
				again = "release it again"
			}
			return callWrite{}, &intake.Refusal{Status: 409, Code: "CHILD_UNDER_ACCEPTED_PARENT", Message: ChildUnderAcceptedParent(key, ancestor, again)}, nil
		}
	}
	if write.status != "todo" {
		return write, nil, nil
	}
	links := chains.Chain(key).Links
	switch {
	case len(links) == 0:
	case call.Op == intake.ReleaseChildrenCall:
		parent := links[0]
		if written[parent] {
			write.follows = parent
		} else if status := callStatus(chains, parent); !record.InWorkStatus(status) {
			return callWrite{}, &intake.Refusal{Status: 409, Code: "PARENT_NOT_RELEASED", Message: fmt.Sprintf(
				"%s is under %s, which is %s and not in work, and Dispatch's parent rule takes an issue into work only under a parent in work; release %s in the same release_children, before %s, or first",
				key, parent, status, parent, key)}, nil
		}
	case issue.Parent == "":
		if ancestors := ancestorsOutOfWork(chains, links); len(ancestors) > 0 {
			return callWrite{}, &intake.Refusal{Status: 409, Code: "PARENT_ARGUMENT_REQUIRED", Message: fmt.Sprintf(
				"todo on %s would put it in work under %s, not in work, which Dispatch's parent rule refuses; call %s again with parent include, which moves each of them to in_progress with it",
				key, strings.Join(ancestors, ", "), call.Op)}, nil
		}
	}
	return write, nil, nil
}

// callStatus is the status of key as the daemon decides on it: its record's, which holds the
// daemon's own write before Dispatch echoes it (R16 of dispatch://INFRA-3320), and otherwise
// Dispatch's as the copy holds it.
func callStatus(chains *record.Chains, key string) string {
	if issue, recorded := chains.Record(key); recorded {
		return issue.Status
	}
	status, _ := chains.Status(key)
	return status
}

// childrenInWork is each child of key on its Dispatch chain that is in work (record.InWorkStatus),
// as "KEY (status)" in key order: the children a backlog or icebox of key would leave in work under
// an issue that is not, which Dispatch's parent rule refuses. A child's status is its record's
// where the daemon records it, as callStatus reads it, and otherwise the status its row of the copy
// holds (record.Chains.Children), which the walk of the chains need not have read.
func childrenInWork(chains *record.Chains, key string) []string {
	var breach []string
	for _, child := range chains.Children(key) {
		status := child.Status
		if issue, recorded := chains.Record(child.Key); recorded {
			status = issue.Status
		}
		if record.InWorkStatus(status) {
			breach = append(breach, fmt.Sprintf("%s (%s)", child.Key, status))
		}
	}
	return breach
}

// ancestorsOutOfWork is each issue of links, an issue's chain from its parent up, that is not in
// work, as "KEY (status)", up to the first that is: the issues a todo of the issue would leave out
// of work above it, which Dispatch's include_parent moves to in_progress. A parent in work leaves
// none.
func ancestorsOutOfWork(chains *record.Chains, links []string) []string {
	var out []string
	for _, link := range links {
		status := callStatus(chains, link)
		if record.InWorkStatus(status) {
			break
		}
		out = append(out, fmt.Sprintf("%s (%s)", link, status))
	}
	return out
}

// applyCallWrite queues write's status row and counts it on the record (countCallWrite), in the
// order statusCall says, and keeps the row's id in rows under the issue's key for a later write of
// the call that follows it. A write of no status is set_status's todo on an issue in Legion's work,
// which is that issue's own todo (backInWork), or release_children's child Dispatch shows todo
// already, which is recorded from Dispatch's view of it where the daemon does not record it yet
// (recordChildUnderParentAtWork), as its todo event would record it: a child the same call writes
// under it is recorded after it, since the call is in its wave order.
func (e *Engine) applyCallWrite(ctx context.Context, tx pgx.Tx, call intake.StatusCall, write callWrite, rows map[string]int64) error {
	key := write.issue.Key
	if write.status == "" {
		switch {
		case call.Op == intake.SetStatusCall && write.record != nil:
			return e.backInWork(ctx, tx, *write.record, ownWrite(*write.record, "todo"))
		case write.record == nil && write.issue.View != nil:
			_, err := e.recordChildUnderParentAtWork(ctx, tx, *write.issue.View)
			return err
		}
		return nil
	}
	payload := record.OrdinaryStatusWrite(write.status, write.observed, "")
	payload.IncludeChildren = write.issue.Children == intake.IncludeArgument
	payload.UnlinkChildren = write.issue.Children == intake.UnlinkArgument
	payload.IncludeParent = write.issue.Parent == intake.IncludeArgument
	payload.Controller = call.Op == intake.SetStatusCall
	payload.After = rows[write.follows]
	queue := func() error {
		row, err := queuedRow(ctx, tx, e.store, key, payload, e.now())
		if err != nil {
			return err
		}
		id, err := e.store.EnqueueID(ctx, tx, row)
		rows[key] = id
		return err
	}
	if record.OutOfWorkflow(write.status) {
		if err := e.countCallWrite(ctx, tx, write); err != nil {
			return err
		}
		return queue()
	}
	if err := queue(); err != nil {
		return err
	}
	return e.countCallWrite(ctx, tx, write)
}

// countCallWrite counts write on the daemon's record of its issue before Dispatch echoes it (R16
// of dispatch://INFRA-3320), as the workflow takes the same status from anyone (applyStatus): a
// status out of work stops the issue, and a todo brings it back into work, a new run where
// rerun_child opens one (NewRun). The record keeps the status Dispatch shows (DispatchStatus), so
// Dispatch's refusal of the write sets the record back to it (statusRefused).
//
// An issue the daemon does not record is counted only where release_children names it with its
// view (intake.StatusCallIssue.View): it is recorded under its parent at work as a child of a todo
// is (recordChildUnderParentAtWork). Any other, the controller's todo on an issue the daemon has
// not recorded among them, is recorded when the write lands, from Dispatch's answer to it (the
// daemon's outbox, through api.ApplyOwnStatusWrite).
func (e *Engine) countCallWrite(ctx context.Context, tx pgx.Tx, write callWrite) error {
	key := write.issue.Key
	if write.record == nil {
		if write.issue.View == nil {
			return nil
		}
		view := *write.issue.View
		view.Status = write.status
		if _, err := e.recordChildUnderParentAtWork(ctx, tx, view); err != nil {
			return err
		}
		return e.keepDispatchStatus(ctx, tx, key, write.observed)
	}
	issue := *write.record
	fact := ownWrite(issue, write.status)
	var err error
	if write.newRun {
		err = e.NewRun(ctx, tx, issue, fact)
	} else {
		err = e.applyStatus(ctx, tx, issue, fact)
	}
	if err != nil {
		return err
	}
	return e.keepDispatchStatus(ctx, tx, key, issue.DispatchStatus)
}

// ownWrite is the daemon's own write of status on issue as the workflow takes a status (applyStatus,
// backInWork): the issue as its record holds it, with its own priority and the one Dispatch
// resolves it to (record.Resolved), at the sequence the record has seen, written by the daemon. It
// carries no reading of Dispatch's, so taking it moves nothing of the record but its status and
// what that status moves: an inherited P0 stays the record's (observedIssue).
func ownWrite(issue record.Issue, status string) intake.DispatchIssue {
	parent := ""
	if issue.Parent != nil {
		parent = *issue.Parent
	}
	return intake.DispatchIssue{Key: issue.Key, Seq: issue.LastDispatchSeq, Type: "issue.updated", Status: status, Title: issue.Title,
		Parent: parent, Rank: issue.Rank, Priority: issue.Priority, Resolved: issue.Resolved, HandedOver: issue.HandedOver, ResolvedRepo: issue.Repo,
		DaemonActor: true, StatusMove: true}
}

// keepDispatchStatus sets the status Dispatch shows of key on its record back to status, where the
// taking of the daemon's own write wrote its own status there (observedIssue): Dispatch has not
// taken the write yet, and the record must still name what it shows.
func (e *Engine) keepDispatchStatus(ctx context.Context, tx pgx.Tx, key, status string) error {
	issue, err := e.store.Issue(ctx, tx, key)
	if err != nil || issue == nil || issue.DispatchStatus == status {
		return err
	}
	issue.DispatchStatus = status
	return e.store.PutIssue(ctx, tx, *issue)
}

// childRunning is why rerun_child refuses child, which the record holds in Legion's work
// (CHILD_RUNNING), told as what the architect does next; shown is the status Dispatch shows of the
// child as the copy holds it, which the route read of Dispatch just now. The restart of a running
// child is the architect's backlog, then rerun_child, and the daemon takes that backlog from its
// Dispatch event (sessionStatusWrite), which the event consumer can deliver late
// (dispatch://INFRA-3178/spec R1). Where Dispatch already shows a status that takes the child out
// of the workflow (record.OutOfWorkflow), the architect wrote its move and the daemon has not taken
// it yet, so the architect calls again once read_record shows the child at that status, its phase
// kept (R20 of dispatch://INFRA-3320). That move can also be one the daemon sets back, and then
// Dispatch stops showing it, so the reason also says which writes the daemon sets back and that the
// architect then writes backlog first. Anywhere else the architect writes backlog first.
func childRunning(child record.Issue, shown string) string {
	if record.OutOfWorkflow(shown) {
		return fmt.Sprintf("Dispatch shows %s for %s, but the daemon has not taken that move yet. The record still shows %s in phase %s at status %s. "+
			"Call rerun_child again when read_record shows %s at status %s. "+
			"If dispatch read --issue %s no longer shows %s, the daemon set the move back. "+
			"In that case, write backlog on %s first (dispatch issue-update --issue %s --status backlog). "+
			"The daemon sets back a phase worker's write and a write by an architect that is not above %s on its Dispatch chain. "+
			"It also sets back a write of the status that Dispatch already showed.",
			shown, child.Key, child.Key, child.Phase, child.Status, child.Key, shown, child.Key, shown, child.Key, child.Key, child.Key)
	}
	return fmt.Sprintf("%s is in phase %s at status %s, so rerun_child cannot run it again now. The restart of a running child is backlog, then rerun_child. "+
		"Write backlog on %s first (dispatch issue-update --issue %s --status backlog). Then call rerun_child again when read_record shows %s at status backlog.",
		child.Key, child.Phase, child.Status, child.Key, child.Key, child.Key)
}
