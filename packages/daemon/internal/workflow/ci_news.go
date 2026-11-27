package workflow

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The CI news of a task (record.CINews) keeps three purposes apart: a report of the GitHub
// listener's new runs and statuses at the task's head (reportRuns), a prompt to read Dispatch's
// state again when what Dispatch holds of a check at that head changed (promptReaders), and the
// fix rule's one repair assignment of a check at a head (assignRepair). None of them decides
// another: no report and no prompt assigns work, starts a fix or allows an approval, since
// Dispatch's state of each check decides and each reader reads it itself.

// ciReader is one current reader of a task's CI news: the role, the issue whose role it is (the
// owning architect's for the owner), and the work it reads for (record.CIReader), which a row
// carries so that the outbox finishes it undelivered once that work is no longer current.
type ciReader struct {
	role   claim.Role
	issue  string
	reader record.CIReader
}

// ciReaders is the current readers of issue's task, w being the walk of its run: the role of the
// step the issue stands in where that step runs (its phases row holds an open entry), with that
// entry, the implementer in the retro and the merger in merging among them; the owner where the
// issue stands in accepting, whose acceptance starts no worker, so its work is the acceptance
// alone; and the implementer where a fix of the task runs, with the fix's number and entry. A
// task held at its end runs no step, so a hold for its CI or its fix prompts no retro that was
// never made: the hold's own release decides. A released claim is a reader all the same, which
// the row wakes.
func (e *Engine) ciReaders(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk) ([]ciReader, error) {
	if recipients, err := e.ciReceivers(ctx, tx, issue); err != nil {
		return nil, err
	} else {
		readers := make([]ciReader, 0, len(recipients))
		for _, to := range recipients {
			var entry int64
			if to.role != claim.RoleArchitect {
				row, err := e.phaseRow(ctx, tx, issue.Key, to.role)
				if err != nil {
					return nil, err
				}
				entry = row.Entry
			}
			readers = append(readers, ciReader{role: to.role, issue: to.issue, reader: record.CIReader{Phase: record.StandsAt(issue), Entry: entry}})
		}
		return readers, nil
	}
	var readers []ciReader
	standsAt := record.StandsAt(issue)
	switch role := RoleFor(standsAt); {
	case atTaskEnd(issue):
	case standsAt == phase.Accepting:
		owner, err := e.owningArchitect(ctx, tx, issue)
		if err != nil {
			return nil, err
		}
		readers = append(readers, ciReader{role: claim.RoleArchitect, issue: owner, reader: record.CIReader{Phase: phase.Accepting}})
	case role != "" && role != claim.RoleArchitect:
		entry, err := e.stepEntry(ctx, tx, issue.Key, role)
		if err != nil {
			return nil, err
		}
		if entry > 0 {
			readers = append(readers, ciReader{role: role, issue: issue.Key, reader: record.CIReader{Phase: standsAt, Entry: entry}})
		}
	}
	if !w.Stored {
		return readers, nil
	}
	fix, err := e.store.RunningFix(ctx, tx, issue.Key, issue.Generation, w.Task)
	if err != nil {
		return nil, err
	}
	if fix != nil && fix.State == record.FixRunning {
		readers = append(readers, ciReader{role: claim.RoleImplementer, issue: issue.Key, reader: record.CIReader{Fix: fix.Fix, Entry: fix.Entry}})
	}
	return readers, nil
}

// readerOf is the work of to among readers, nil where to is no current reader.
func readerOf(readers []ciReader, to roleRecipient) *record.CIReader {
	for _, r := range readers {
		if r.role == to.role && r.issue == to.issue {
			reader := r.reader
			return &reader
		}
	}
	return nil
}

// ciReceivers is who is told of a settlement of issue's pull request, as main tells it: the
// architect that owns the issue (owningArchitect), the role of the step the issue stands in, and
// the implementer, whose code the checks ran on, except in merging, where the merge and what
// breaks it are the merger's (cybermonklabs/legion#4979). A phase the architect itself holds
// (accepting) is the owner's own step, so the owner is told once, as the owner.
func (e *Engine) ciReceivers(ctx context.Context, tx pgx.Tx, issue record.Issue) ([]roleRecipient, error) {
	owner, err := e.owningArchitect(ctx, tx, issue)
	if err != nil {
		return nil, err
	}
	recipients := []roleRecipient{{issue: owner, role: claim.RoleArchitect, row: issue.Key}}
	roles := []claim.Role{RoleFor(record.StandsAt(issue))}
	if !record.StandsIn(issue, phase.Merging) {
		roles = append(roles, claim.RoleImplementer)
	}
	for _, role := range roles {
		if role == "" || role == claim.RoleArchitect || slices.ContainsFunc(recipients, func(r roleRecipient) bool { return r.role == role }) {
			continue
		}
		recipients = append(recipients, roleRecipient{issue: issue.Key, role: role, row: issue.Key})
	}
	return recipients, nil
}

// ciNextOf is what Legion itself does next for a task, as one receiver of a settlement is told it
// (ciNext): Next, "" for nothing, the task, and the fix, check and head the act is of; Tested is the
// head the tester's completion reported where Next is testing-move and that head is not the pull
// request's, and testedHoldsSet whether the daemon holds a set of checks there.
type ciNextOf struct {
	Next                record.CINext
	Task                string
	Fix                 int
	Check, Head, Tested string
	testedHoldsSet      bool
}

// ciNext is what Legion itself does next for issue's task, w being the walk of its run, as to, a
// receiver of a settlement, is told it: the first of these that applies. fix-runs: a fix of the
// task reads its job or runs, and to is the implementer, which runs the fix, or the task is not
// held at its end (a hold beside the fix, from a step before the end, among them). end-hold-ci:
// the task is held before its retro for its CI. end-hold-fix: it is held there for its fix, for
// every receiver but the fix's own implementer. testing-move: the issue stands in testing with its
// tester's completion written, so the testing move waits for Dispatch's states. Else nothing. So
// no receiver beside a running fix, and not the fix's own agent, is told that its own act is
// asked for, and none is told that nothing is asked of it while it runs the fix.
func (e *Engine) ciNext(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, to claim.Role) (ciNextOf, error) {
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil {
		return ciNextOf{}, err
	}
	next := ciNextOf{Task: w.Task}
	_, next.Head = taskHead(pr)
	var fix *record.TaskFix
	if w.Stored {
		if fix, err = e.store.RunningFix(ctx, tx, issue.Key, issue.Generation, w.Task); err != nil {
			return ciNextOf{}, err
		}
	}
	switch {
	case fix != nil && (to == claim.RoleImplementer || !atTaskEnd(issue)):
		next.Next = record.CINextFixRuns
	case atTaskEnd(issue) && record.HeldFor(issue, record.HoldForCI):
		next.Next = record.CINextEndHoldCI
		return next, nil
	case atTaskEnd(issue):
		next.Next = record.CINextEndHoldFix
	case issue.Phase == phase.Testing:
		return e.testingNext(ctx, tx, issue, pr, next)
	default:
		return ciNextOf{}, nil
	}
	if fix == nil && w.Stored {
		if fix, err = e.store.LatestFix(ctx, tx, issue.Key, issue.Generation, w.Task); err != nil {
			return ciNextOf{}, err
		}
	}
	if fix != nil {
		next.Fix, next.Check, next.Head = fix.Fix, fix.Check, fix.Head
	}
	return next, nil
}

// testingNext is ciNext of an issue in testing, next holding its task and its pull request's head:
// testing-move once the tester's completion is written, naming the head that completion reported
// where it is not the pull request's (testedHead), and whether the daemon holds a set of checks
// there; nothing while the tester works.
func (e *Engine) testingNext(ctx context.Context, tx pgx.Tx, issue record.Issue, pr *record.PullRequest, next ciNextOf) (ciNextOf, error) {
	row, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleTester)
	if err != nil || !row.Completed() {
		return ciNextOf{}, err
	}
	next.Next = record.CINextTestingMove
	repo, tested := testedHead(issue, row, pr)
	if tested == "" || tested == next.Head {
		return next, nil
	}
	held, err := e.store.HeadStates(ctx, tx, issue.Key, repo, tested)
	if err != nil {
		return ciNextOf{}, err
	}
	next.Tested, next.testedHoldsSet = tested, held.Checks != nil
	return next, nil
}

// nextSentence is the sentence that says what Legion does next (next.Next), and names no act of
// the one told: "" where Legion does nothing next. It is the one builder of these sentences, which
// the reports (runsBody) and the wake of a released claim (settlementDetail) both read.
func nextSentence(next ciNextOf) string {
	switch next.Next {
	case record.CINextEndHoldCI:
		return fmt.Sprintf("Legion holds task %s before its retro until each check that a CI job runs reads yes at %s in Dispatch. "+
			"A failure there starts a fix where the task takes one; otherwise Legion tells the owner once. Nothing is asked of you now.", next.Task, next.Head)
	case record.CINextEndHoldFix:
		return fmt.Sprintf("Legion holds task %s before its retro while fix %d runs for check %s at %s. Nothing is asked of you now.",
			next.Task, next.Fix, next.Check, next.Head)
	case record.CINextFixRuns:
		return fmt.Sprintf("Fix %d of task %s runs for check %s at %s. That failure is the fix's.", next.Fix, next.Task, next.Check, next.Head)
	case record.CINextTestingMove:
		if next.Tested != "" && !next.testedHoldsSet {
			return fmt.Sprintf("Legion holds the testing step of task %s: %s.", next.Task, offHeadWaitReason(next.Tested, next.Head))
		}
		at := next.Head
		if next.Tested != "" {
			at = next.Tested
		}
		return fmt.Sprintf("Legion decides the testing step of task %s by Dispatch's states at %s. Nothing is asked of you now.", next.Task, at)
	}
	return ""
}

// newObservations is what a settlement newly put on the record at head: each run the record holds
// there after (pr) whose id it did not hold there before (before), and each commit status of head
// whose context, time and state it did not hold there before. Every job is kept, whether a check
// names it or not, and no run is mapped to a check; each run is named by GitHub's word where the
// settlement carried one (shownAs).
func newObservations(before, pr record.PullRequest, head string) ([]record.CIRun, []record.CIStatus) {
	held := map[int64]bool{}
	for _, run := range before.CheckRuns {
		if run.Head == head {
			held[run.ID] = true
		}
	}
	var runs []record.CIRun
	for _, run := range pr.CheckRuns {
		if run.Head == head && !held[run.ID] {
			runs = append(runs, record.CIRun{Name: run.Name, ID: run.ID, Head: head, Conclusion: shownAs(run, run.Conclusion)})
		}
	}
	slices.SortFunc(runs, func(a, b record.CIRun) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	heldStatuses := keptStatuses(before, head)
	var statuses []record.CIStatus
	for _, status := range keptStatuses(pr, head) {
		if !slices.ContainsFunc(heldStatuses, func(s record.CommitStatus) bool {
			return s.Context == status.Context && s.State == status.State && s.CreatedAt.Equal(status.CreatedAt)
		}) {
			statuses = append(statuses, record.CIStatus{Context: status.Context, State: string(status.State), CreatedAt: status.CreatedAt})
		}
	}
	return runs, statuses
}

// settledRuns is every run and commit status the record holds at the checked head of pr: what a
// settlement of a task that takes no fix says, as main's message (settledChecks) names it, which
// a report of what Legion does next carries in that message's place.
func settledRuns(pr record.PullRequest) ([]record.CIRun, []record.CIStatus) {
	return newObservations(record.PullRequest{}, pr, pr.CheckedHead)
}

// runsBody is the body of a report to a receiver of issue key's pull request number at head:
// each run with its name, its id and GitHub's conclusion, and each commit status, then that these
// are GitHub's conclusions and Dispatch's state of each check decides. Where Legion does next
// what next says, its sentence (nextSentence) follows; otherwise the reader is told to read
// Dispatch. It names no act of the reader beside that read.
func runsBody(key string, number int, head string, runs []record.CIRun, statuses []record.CIStatus, next ciNextOf) string {
	observed := make([]string, 0, len(runs)+len(statuses))
	for _, run := range runs {
		observed = append(observed, fmt.Sprintf("CI run %s %d %s", strconv.Quote(run.Name), run.ID, run.Conclusion))
	}
	for _, status := range statuses {
		observed = append(observed, fmt.Sprintf("commit status %s %s", strconv.Quote(status.Context), status.State))
	}
	body := fmt.Sprintf("Pull request #%d at %s: %s.", number, head, strings.Join(observed, ", "))
	if next.Next == record.CINextNone {
		return body + " These are GitHub's conclusions. Dispatch's state of each check decides: read it with dispatch results --issue " + key + "."
	}
	return body + " These are GitHub's conclusions; Dispatch's state of each check decides. " + nextSentence(next)
}

// eachRecipient calls tell for each of recipients the daemon can name, with its claim token, each
// token once, in the order given (addressRoles). A holder it cannot name is an error, and with it
// the fact's transaction.
func (e *Engine) eachRecipient(project string, recipients []roleRecipient, tell func(to roleRecipient, token claim.Token) error) error {
	told := make([]claim.Token, 0, len(recipients))
	for _, to := range recipients {
		addressed, err := e.addressRoles(project, []roleRecipient{to}, nil)
		if err != nil {
			return err
		}
		for _, a := range addressed {
			if slices.Contains(told, a.token) {
				continue
			}
			told = append(told, a.token)
			if err := tell(to, a.token); err != nil {
				return err
			}
		}
	}
	return nil
}

// reportRuns is the listener's report of a settlement of pr, the pull request of issue's task
// that takes a fix, w being the walk of its run, and before the record as it stood before the
// settlement was applied. A settlement at a head the task has left tells nothing. Otherwise its
// new observations at the task's head (newObservations), every job whether a check names it or
// not, go to each of main's receivers (ciReceivers) once, in one row (record.CINewsRuns) that
// names the receiver's current work where it is a current reader (ciReaders), so the row wakes
// it, and what Legion does next for it (ciNext). A settlement with no new observation, a replay
// or the same runs under a new fact id, writes nothing. The text the settlement leaves is not read
// here: a rerun that fails again says what was said, and is news. The report assigns no work and
// claims no pass of a check: Dispatch's state of each check decides, and the fix rule alone
// assigns a repair (fixRule).
func (e *Engine) reportRuns(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, before, pr record.PullRequest) error {
	_, head := taskHead(&pr)
	if head == "" || pr.CheckedHead != head {
		return nil
	}
	runs, statuses := newObservations(before, pr, head)
	if len(runs) == 0 && len(statuses) == 0 {
		return nil
	}
	readers, err := e.ciReaders(ctx, tx, issue, w)
	if err != nil {
		return err
	}
	recipients, err := e.ciReceivers(ctx, tx, issue)
	if err != nil {
		return err
	}
	return e.eachRecipient(issue.Project, recipients, func(to roleRecipient, token claim.Token) error {
		next, err := e.ciNext(ctx, tx, issue, w, to.role)
		if err != nil {
			return err
		}
		return e.enqueue(ctx, tx, to.row, record.CINews{Purpose: record.CINewsRuns, Role: string(token), Generation: issue.Generation,
			Task: w.Task, Repo: pr.Repo, PullRequest: pr.Number, Head: head, Reader: readerOf(readers, to), Next: next.Next,
			Tested: next.Tested, Runs: runs, Statuses: statuses, Body: runsBody(issue.Key, pr.Number, head, runs, statuses, next)})
	})
}

// promptReaders is the prompt to read Dispatch again (record.CINewsState) of a change that fact,
// one of Dispatch's state events of issue, made to what the daemon holds at its head: states, the
// snapshot of the check whose state changed, and checks, the head's new set of checks, nil where
// the set did not change and present where it did, empty included, so the removal of the head's
// last check reaches the reader as a set of no check. Only a task that takes a fix is prompted,
// and only at its head (taskHead): a state at a head the task has left is held and tells nobody.
// Each current reader (ciReaders) gets one pending prompt for each head and work it reads for: a
// change that comes while one waits joins it (record.Store.EnqueueReadPrompt), so ten checks of
// one run give one row and one wake. The body names the checks and the head, and no state, since a
// yes queued can be overtaken by a no before the row is delivered. A fix the same fact opens reads
// its job first, which reads no prompt; its start task names its failure.
func (e *Engine) promptReaders(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.DispatchCheckState, states []record.CIStateSnapshot, checks *[]string) error {
	if len(states) == 0 && checks == nil {
		return nil
	}
	fixesCI, err := e.store.RunFixesCI(ctx, tx, issue.Key, issue.Generation)
	if err != nil || !fixesCI {
		return err
	}
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil {
		return err
	}
	repo, head := taskHead(pr)
	if head == "" || fact.Repo != repo || fact.Head != head {
		return nil
	}
	if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
		return err
	}
	w, err := e.walk(ctx, tx, issue)
	if err != nil {
		return err
	}
	readers, err := e.ciReaders(ctx, tx, issue, w)
	if err != nil {
		return err
	}
	for _, r := range readers {
		reader := r.reader
		if err := e.eachRecipient(issue.Project, []roleRecipient{{issue: r.issue, role: r.role, row: issue.Key}}, func(to roleRecipient, token claim.Token) error {
			prompt := record.CINews{Purpose: record.CINewsState, Role: string(token), Generation: issue.Generation, Task: w.Task,
				Repo: fact.Repo, PullRequest: pr.Number, Head: fact.Head, Reader: &reader, States: states, Checks: checks}
			_, err := e.store.EnqueueReadPrompt(ctx, tx, to.row, prompt, e.now())
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

// snapshotOf is the snapshot of state a prompt to read Dispatch carries (record.CIStateSnapshot),
// for tests and logs.
func snapshotOf(state record.CheckState) record.CIStateSnapshot {
	roles := slices.Clone(state.Roles)
	if roles == nil {
		roles = []string{}
	}
	return record.CIStateSnapshot{Check: state.Check, State: state.State, Reason: state.Reason, CICase: state.CICase, Roles: roles, Seq: state.Seq}
}

// acceptedChange says whether a write of Dispatch's state of a check, after, changed what the
// daemon holds of it at its head: the store took the write (written, a higher Dispatch sequence)
// and its content differs from the state held before (before, held false for none) in its state,
// its reason, its CI case or its set of roles. A first state of the check at the head is a
// change; the same content at a higher sequence is none, and so is a lower sequence the store
// refused.
func acceptedChange(written, held bool, before, after record.CheckState) bool {
	if !written {
		return false
	}
	return !held || before.State != after.State || before.Reason != after.Reason || before.CICase != after.CICase ||
		!sameRoles(before.Roles, after.Roles)
}

// acceptedSetChange says whether a write of the head's set of checks, after, changed the set the
// daemon holds there, before (nil for none held): the store took the write and the list of checks
// differs. A first set at a head is a change, an empty one included.
func acceptedSetChange(written bool, before, after []string) bool {
	return written && (before == nil || !slices.Equal(before, after))
}

// sameRoles says whether a and b name the same roles, in whatever order.
func sameRoles(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}
