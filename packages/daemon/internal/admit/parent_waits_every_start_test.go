package admit

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/checkstatetest"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// pwRoundCap is the review_round_cap of the deployment these cases run under (pwAdmission).
const pwRoundCap = 3

// pwBackFromRetro is the reason of the implementer's move back out of retro, the one move from a
// phase past the daemon's acceptance that starts implementing.
const pwBackFromRetro = "the retro found a defect to fix"

// pwReviewSummary is what the reviewer reported with its completion of the round a review decides.
const pwReviewSummary = "every thread of the round read"

// pwAdmission is the admission pass of these cases, quiet as quiet builds one, over a workflow
// engine of the test project whose deployment counts review rounds to a review_round_cap of
// pwRoundCap and carries a hook on every move back (`every: 1`, no `moves`, prompting the
// implementer), configured as the workflow package's own tests configure a deployment hook
// (TestAHeldMoveCountsAsTodayAndFiresNoHook). The facts apply through this engine and the pass
// moves parents through it, as in the daemon, where both are one engine.
func pwAdmission(t *testing.T, cap int) *Admission {
	t.Helper()
	admission := quiet(t, cap)
	admission.engine = workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
		Steps: config.DesignGateRootIssues.Steps(), ReviewRoundCap: pwRoundCap, Clock: func() time.Time { return fixedNow },
		Hooks: []workflow.Hook{{Name: "back", On: workflow.HookEventWorkBack, Every: 1, Roles: []claim.Role{claim.RoleImplementer},
			Prompt: "Read the failing check first."}}}, nil)
	return admission
}

// pwApply applies fact as the daemon applies one: through the mirror of Dispatch's issues, the
// workflow engine and the admission pass, in one transaction (intake.ApplyFact), and answers what
// the handlers answered, a refusal included.
func pwApply(t *testing.T, pool *pgxpool.Pool, admission *Admission, source, id string, fact intake.Fact) intake.Result {
	t.Helper()
	result, err := intake.ApplyFact(context.Background(), pool, source, id, fact,
		blockers.NewMirror(record.NewStore(), testProject), admission.engine, admission)
	if err != nil {
		t.Fatalf("apply %s: %v", id, err)
	}
	return result
}

// pwApplied is pwApply for a fact the workflow takes: a refused fact changed nothing, and would
// prove nothing about a move.
func pwApplied(t *testing.T, pool *pgxpool.Pool, admission *Admission, source, id string, fact intake.Fact) {
	t.Helper()
	if result := pwApply(t, pool, admission, source, id, fact); result.Refusal != nil {
		t.Fatalf("%s was refused %s: %s", id, result.Refusal.Code, result.Refusal.Message)
	}
}

// pwStatesFirst is fact as a tester's completion makes its move now: the completion names the
// head it tested and no verdict, and the check state Dispatch publishes of k1 at that head,
// passing where tested is "pass" and failing where it is "fail" (checkstatetest.Tested), decides
// the testing move. The state is applied first and the completion names its serial, so the
// completion moves the issue at once, as one whose test record's fence the states have reached.
// With tested "" the fact is answered as it is.
func pwStatesFirst(t *testing.T, pool *pgxpool.Pool, admission *Admission, fact intake.Fact, tested string) intake.Fact {
	t.Helper()
	if tested == "" {
		return fact
	}
	completion, ok := fact.(intake.HandoffComplete)
	if !ok || completion.Role != claim.RoleTester {
		t.Fatalf("a tested state decides a tester's completion, not %#v", fact)
	}
	state, id := checkstatetest.Tested(t, pool, completion.Issue, "acme/widgets", completion.Commit, tested == "pass")
	pwApplied(t, pool, admission, "dispatch", id, state)
	completion.StatesSerial = state.Serial
	return completion
}

// pwRefused fails unless result is the refusal code.
func pwRefused(t *testing.T, result intake.Result, code, what string) {
	t.Helper()
	if result.Refusal == nil || result.Refusal.Code != code {
		t.Fatalf("%s = refusal %+v, want %s", what, result.Refusal, code)
	}
}

// pwHead is the head of the parent's pull request, and the commit every completion of these
// tests names. It is forty lowercase hexadecimal digits because a check state names the commit
// it stands at, and the daemon reads no state whose head is not written as a commit is.
const pwHead = "4242424242424242424242424242424242424242"

// pwHeadBefore is the head the branch stood at before pwHead: the head a merge forward was asked
// of where it has landed since.
const pwHeadBefore = "4141414141414141414141414141414141414141"

// pwPullRequest records key's pull request #42, at pwHead on base main, in state, with
// GitHub's last answer for that head mergeable and clean, so an approval of it ends its round.
func pwPullRequest(t *testing.T, pool *pgxpool.Pool, key string, state record.PullRequestState) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		pr := record.PullRequest{Issue: key, Repo: "acme/widgets", Number: 42, Branch: "legion/" + key, HeadSHA: pwHead,
			Generation: 1, State: state, Base: "main", Mergeability: record.MergeabilityMergeable, MergeState: record.MergeStateClean}
		if err := record.NewStore().PutPullRequest(context.Background(), tx, pr); err != nil {
			t.Fatalf("record the pull request of %s: %v", key, err)
		}
	})
}

// pwTask is the task the worker of key's phase at was given for the entry it stands on.
func pwTask(key string, at phase.Phase) string {
	return "Continue " + key + ". Issue: " + key + ". Phase: " + string(at) + ". Reason: a review asked for changes."
}

// pwMidPhase records key as the root of a running tree whose worker of at is in the middle of its
// phase: the admission slot that phase keeps, its pull request open, the role's claim in state
// (suspended by by) given the phase's entry 3, and the role's phases row on that entry, raised an
// hour before with the task the worker was given. It answers the claim's token.
func pwMidPhase(t *testing.T, pool *pgxpool.Pool, key string, at phase.Phase, state supervise.ClaimState, by supervise.SuspendSource) claim.Token {
	t.Helper()
	spSeedParent(t, pool, key, at, true, nil)
	pwPullRequest(t, pool, key, record.PullRequestOpen)
	role := phase.RoleFor(at)
	token := putWorkerClaim(t, pool, key, key, role, 1, state, by)
	markGivenEntry(t, pool, token, 3)
	spSeedPhase(t, pool, record.PhaseRow{Issue: key, Role: role, Claim: token, Entry: 3, EntryTask: pwTask(key, at),
		EntryRaisedAt: fixedNow.Add(-time.Hour)})
	return token
}

// pwDeliver records start, a phase worker's start row of key, as delivered to token's claim, as
// the outbox leaves a claim it ran that row on (supervise's queue): the claim holds the row's task
// for its phase, run and entry, confirmed by a turn, it was given that entry where the row carries
// one (a start at no entry leaves the entry the claim was given as it stands), and the row is
// finished.
func pwDeliver(t *testing.T, pool *pgxpool.Pool, key string, token claim.Token, start record.SuperviseRequest) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `insert into pending_task_deliveries (claim_token, delivery_id, task, queued_at, generation, phase, entry, confirmed_at)
		values ($1, 'outbox:delivered', $2, $3, $4, $5, $6, $3)`,
		string(token), start.Task, fixedNow, int64(start.Generation), string(start.Phase), start.Entry); err != nil {
		t.Fatalf("record the task the claim %s was given: %v", token, err)
	}
	if start.Entry > 0 {
		markGivenEntry(t, pool, token, start.Entry)
	}
	inTx(t, pool, func(tx pgx.Tx) {
		var id int64
		if err := tx.QueryRow(ctx, `select id from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'start'
			and payload->>'role' = $2 order by id desc limit 1`, key, string(start.Role)).Scan(&id); err != nil {
			t.Fatalf("read the %s's start of %s: %v", start.Role, key, err)
		}
		if err := record.NewStore().FinishOutbox(ctx, tx, id, ""); err != nil {
			t.Fatalf("finish the %s's start %d of %s: %v", start.Role, id, key, err)
		}
	})
}

// pwLate is the completion role reports of the phase it was in, under its claim token, naming the
// handoff record rec: for a phase the wait stopped, the late completion R10 refuses. A tester's
// names no verdict, as no phase completion does: its commit is the head it tested.
func pwLate(key string, role claim.Role, token claim.Token, rec string) intake.HandoffComplete {
	return intake.HandoffComplete{Generation: 1, Issue: key, Role: role, Claim: token, Summary: "the stopped phase's work",
		Commit: pwHead, HandoffRecord: rec, HandoffVersion: 1}
}

// pwReviewed records the reviewer's completion of its round on the parent's head, whose decision
// the review fact brings: the summary the replaced phase-finished notice would have carried.
func pwReviewed(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	spSeedPhase(t, pool, record.PhaseRow{Issue: spParent, Role: claim.RoleReviewer, Claim: "reviewer", Summary: pwReviewSummary,
		HandoffCommit: pwHead, HandoffRecord: "review-1", HandoffVersion: 1, CompletedAt: fixedNow.Add(-time.Hour),
		Entry: 1, EntryRaisedAt: fixedNow.Add(-2 * time.Hour)})
}

// pwReview is a review of the parent's head submitted by a maintainer with write access to the
// repository, delivered by its webhook: a review that decides the round (workflow's DecidesRound).
func pwReview(state, body string) intake.PullRequestReview {
	return intake.PullRequestReview{Repo: "acme/widgets", Number: 42, ID: 7, SubmittedAt: fixedNow.Add(-time.Minute), State: state,
		CommitID: pwHead, HeadSHA: pwHead, Author: "a-maintainer", AuthorCanWrite: true, Body: body}
}

// pwNoticeKinds is how many architect notices are queued for key, by kind.
func pwNoticeKinds(t *testing.T, pool *pgxpool.Pool, key string) map[record.NoticeKind]int {
	t.Helper()
	kinds := map[record.NoticeKind]int{}
	for _, notice := range pwPayloads[record.Notice](t, pool, key) {
		kinds[notice.Kind]++
	}
	return kinds
}

// pwPayloads is every outbox payload of type P queued for key, in outbox order.
func pwPayloads[P record.OutboxPayload](t *testing.T, pool *pgxpool.Pool, key string) []P {
	t.Helper()
	var got []P
	for _, effect := range effects(t, pool) {
		if payload, ok := effect.payload.(P); ok && effect.issue == key {
			got = append(got, payload)
		}
	}
	return got
}

// pwWorkerStarts is every start row key holds for a phase worker, oldest first: its architect's
// own starts left out.
func pwWorkerStarts(t *testing.T, pool *pgxpool.Pool, key string) []midPhaseStart {
	t.Helper()
	var starts []midPhaseStart
	for _, start := range startsOf(t, pool, key) {
		if start.role != claim.RoleArchitect {
			starts = append(starts, start)
		}
	}
	return starts
}

// pwClaimState is the state of token's claim, and the source of its suspension.
func pwClaimState(t *testing.T, pool *pgxpool.Pool, token claim.Token) (supervise.ClaimState, supervise.SuspendSource) {
	t.Helper()
	var state, by string
	if err := pool.QueryRow(context.Background(), `select state, coalesce(suspended_by, '') from claims where token = $1`,
		string(token)).Scan(&state, &by); err != nil {
		t.Fatalf("read the claim %s: %v", token, err)
	}
	return supervise.ClaimState(state), supervise.SuspendSource(by)
}

// The words of R10 on a merge forward that a move into awaiting_children drops before it landed
// (dispatch://INFRA-3320 R10), each sentence once: the implementer that has begun the merge
// finishes it, pushes it and does nothing more, and one that has not begun it begins none; the
// daemon does not ask for that merge forward again, and the implementer merges the base forward
// only once GitHub reads the pull request CONFLICTING, since the tester's merge with the base finds
// a conflict that is still there and fails its round; any other worker writes and pushes nothing
// more. They are the words record.PhaseEnded.Message gives and the implementer's role message
// carries.
// internal/record's TestAPhaseEndedMessageIntoAwaitingChildrenGivesTheWaitsOwnWords reads the
// three forms of the phase-ended message word for word, and internal/daemon's
// TestParentWaitsHoldsEveryPhaseStart reads the published texts.
const (
	// pwCommitAndPush is what the plain form and the implementer's form tell the worker of the
	// phase that ended (%s).
	pwCommitAndPush = "Commit and push what you have to the issue's branch, and do no more work on %s."
	// pwWriteNothing is what a worker other than the implementer is told in place of
	// pwCommitAndPush when the move dropped a merge forward asked of the implementer that had not
	// landed; %s is the phase that ended.
	pwWriteNothing = "Write and push nothing more, because the implementer may be merging in the workspace you share: put what you found on the Dispatch issue, and do no more work on %s."
	// pwDroppedForward names the dropped merge forward to the role it was asked of, %[1]s being
	// the issue, %[2]s the base and %[3]s the head, and gives it the begun and not-begun rule.
	pwDroppedForward = "The daemon dropped the merge forward of %[2]s onto head %[3]s that it asked of you for %[1]s: if you have begun that merge, finish it, push it and do nothing more; if you have not begun it, begin none."
	// pwNoAskAgain tells the role a dropped merge forward was asked of when a merge of the base
	// (%s) forward is its to make again.
	pwNoAskAgain = "The daemon does not ask for that merge forward again: in a later task of the issue, merge %s forward only once GitHub reads its pull request CONFLICTING."
	// pwMovedIntoTheWait opens the role message of the role a dropped merge forward was asked of,
	// %s being the issue.
	pwMovedIntoTheWait = "%s: the issue moved into awaiting_children, where it waits on its open Dispatch children."
	// pwNextTask is when the issue's next task comes, in every form.
	pwNextTask = "The daemon gives the issue its next task once its children are finished."
	// pwLeaveUncommitted is the words no phase-ended message carries: each form says what the
	// worker commits and pushes, or that it writes and pushes nothing more.
	pwLeaveUncommitted = "leave whatever you have uncommitted"
)

// pwDropped is what the role dropped was asked of (dropped.Role) is told of it, dropped being a
// merge forward of issue's pull request that the move into awaiting_children dropped before it
// landed: the merge forward named, the begun and not-begun rule, and when a merge forward is that
// role's to make again.
func pwDropped(issue string, dropped record.DroppedMergeForward) []string {
	return []string{fmt.Sprintf(pwDroppedForward, issue, dropped.Base, dropped.Head), fmt.Sprintf(pwNoAskAgain, dropped.Base)}
}

// pwDroppedBody is the body of the role message the role dropped was asked of is sent when a move
// into awaiting_children that stops no phase of that role's drops it: that the issue waits on its
// children, what pwDropped says, and when the issue's next task comes.
func pwDroppedBody(issue string, dropped record.DroppedMergeForward) string {
	return strings.Join(append(append([]string{fmt.Sprintf(pwMovedIntoTheWait, issue)}, pwDropped(issue, dropped)...), pwNextTask), " ")
}

// pwToldR10 fails unless ended is the one message the move sends the role whose from phase it
// ended, the R10 message: the phase ended and no task follows, a completion of it is refused, the
// issue's next task comes once its children are finished, and its reason is the held start's and
// names the open child. dropped is the merge forward the move dropped before it landed, which the
// message carries (record.PhaseEnded.MergeForward), and nil where it dropped none, and it chooses
// the form:
//
//   - none: the worker commits and pushes what it has and does no more work on the phase, and the
//     message names no merge forward (the plain form);
//   - one, to the implementer: the plain words, with the dropped merge forward named, the begun
//     and not-begun rule and when a merge forward is its to make again;
//   - one, to any other worker: it writes and pushes nothing more, since the implementer may be
//     merging in the workspace they share, and puts what it found on the Dispatch issue, and it is
//     not told to commit and push.
//
// No form carries the words pwLeaveUncommitted: each says what the worker does with what it has.
func pwToldR10(t *testing.T, ended []record.PhaseEnded, role claim.Role, from phase.Phase, reason string, dropped *record.DroppedMergeForward) {
	t.Helper()
	if len(ended) != 1 || ended[0].Role != role || ended[0].From != from || ended[0].To != phase.AwaitingChildren ||
		ended[0].Reason != reason {
		t.Fatalf("phase-ended messages = %+v, want one to the %s out of %s into awaiting_children, no task following, for %q",
			ended, role, from, reason)
	}
	message := ended[0].Message(spParent)
	want := []string{
		"the daemon ended your " + string(from) + " phase, and the issue is in awaiting_children now: it waits on its open Dispatch children, and no task of yours follows.",
		"A completion of " + string(from) + " is refused (HANDOFF_NOT_CURRENT_PHASE).",
	}
	var unsaid string
	switch {
	case dropped == nil:
		want, unsaid = append(want, fmt.Sprintf(pwCommitAndPush, from)), "merge forward"
	case role == claim.RoleImplementer:
		want = append(append(want, fmt.Sprintf(pwCommitAndPush, from)), pwDropped(spParent, *dropped)...)
	default:
		want, unsaid = append(want, fmt.Sprintf(pwWriteNothing, from)), "Commit and push"
	}
	for _, said := range append(want, pwNextTask, spChild+" (todo)") {
		if !strings.Contains(message, said) {
			t.Fatalf("the %s was told %q, want it to say %q", role, message, said)
		}
	}
	if unsaid != "" && strings.Contains(message, unsaid) {
		t.Fatalf("the %s was told %q, want no %q in it", role, message, unsaid)
	}
	if strings.Contains(message, pwLeaveUncommitted) {
		t.Fatalf("the %s was told %q, want no %q in it: each form says what the worker does with what it has", role, message, pwLeaveUncommitted)
	}
	if !reflect.DeepEqual(ended[0].MergeForward, dropped) {
		t.Fatalf("the phase-ended message names the dropped merge forward %+v, want %+v", ended[0].MergeForward, dropped)
	}
}

// pwRoleToken is the claim token of key's role, whose role topic a message to that role goes to
// (workflow's addressRoles).
func pwRoleToken(t *testing.T, key string, role claim.Role) claim.Token {
	t.Helper()
	project, err := claim.ProjectToken(testProject)
	if err != nil {
		t.Fatalf("project token of %s: %v", testProject, err)
	}
	token, err := claim.NewToken(project, key, role)
	if err != nil {
		t.Fatalf("claim token of the %s of %s: %v", role, key, err)
	}
	return token
}

// pwToldImplementer fails, and lets the case go on to its next reading, unless the role messages
// queued for the parent's implementer are the one that names dropped (pwDroppedBody), a merge
// forward the move into awaiting_children dropped before it landed, or none where dropped is nil.
func pwToldImplementer(t *testing.T, pool *pgxpool.Pool, dropped *record.DroppedMergeForward) {
	t.Helper()
	token := string(pwRoleToken(t, spParent, claim.RoleImplementer))
	var told, want []record.RoleMessage
	for _, message := range pwPayloads[record.RoleMessage](t, pool, spParent) {
		if message.Role == token {
			told = append(told, message)
		}
	}
	if dropped != nil {
		want = []record.RoleMessage{{Role: token, Body: pwDroppedBody(spParent, *dropped)}}
	}
	if !reflect.DeepEqual(told, want) {
		t.Errorf("role messages to the implementer = %+v, want %+v", told, want)
	}
}

// pwAsked is the merge forward the parent's pull request record holds, asked in at, of pwHead
// where it has not landed, and of pwHeadBefore with pwHead the head it landed at where landed.
func pwAsked(at phase.Phase, landed bool) *record.MergeForward {
	why := record.ConflictRulesUnread
	if at == phase.Accepting {
		why = record.ConflictMergeNext
	}
	asked := &record.MergeForward{Head: pwHead, Base: "main", Phase: at, Why: why, AskedAt: fixedNow.Add(-30 * time.Minute)}
	if landed {
		asked.Head, asked.Landed = pwHeadBefore, pwHead
	}
	return asked
}

// pwSeedForward records asked as the merge forward of the parent's pull request, which
// pwPullRequest recorded, leaving the record as it is where asked is nil.
func pwSeedForward(t *testing.T, pool *pgxpool.Pool, asked *record.MergeForward) {
	t.Helper()
	if asked == nil {
		return
	}
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		pr, err := records.PullRequest(context.Background(), tx, spParent)
		if err != nil || pr == nil {
			t.Fatalf("read the pull request of %s = %+v, %v; want the one recorded", spParent, pr, err)
		}
		pr.MergeForward = asked
		if err := records.PutPullRequest(context.Background(), tx, *pr); err != nil {
			t.Fatalf("record the merge forward of %s: %v", spParent, err)
		}
	})
}

// pwConflicting records the parent's pull request, which pwPullRequest recorded, as the daemon's
// two reads of GitHub a minute apart leave a head they judge conflicting with its base when its
// base's rules require nothing (workflow's mergeability): CONFLICTING and dirty at pwHead, and
// that head held as the confirmed conflict (record.PullRequest.ConflictingHead), which is what a
// conflict round is read against (classify.ConflictRound).
func pwConflicting(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		pr, err := records.PullRequest(context.Background(), tx, spParent)
		if err != nil || pr == nil {
			t.Fatalf("read the pull request of %s = %+v, %v; want the one recorded", spParent, pr, err)
		}
		pr.Mergeability, pr.MergeState, pr.ConflictingHead = record.MergeabilityConflicting, record.MergeStateDirty, pr.HeadSHA
		if err := records.PutPullRequest(context.Background(), tx, *pr); err != nil {
			t.Fatalf("record the conflict of %s: %v", spParent, err)
		}
	})
}

// pwForwardOf is the merge forward the record of the parent's pull request holds.
func pwForwardOf(t *testing.T, pool *pgxpool.Pool) *record.MergeForward {
	t.Helper()
	var asked *record.MergeForward
	inTx(t, pool, func(tx pgx.Tx) {
		pr, err := record.NewStore().PullRequest(context.Background(), tx, spParent)
		if err != nil || pr == nil {
			t.Fatalf("read the pull request of %s = %+v, %v; want the one recorded", spParent, pr, err)
		}
		asked = pr.MergeForward
	})
	return asked
}

// pwInProgressOver is the dispatch_status rows a move into awaiting_children writes for a parent
// whose record shows status: in_progress over another status, and none over in_progress.
func pwInProgressOver(status string) []record.StatusWrite {
	if status == "in_progress" {
		return nil
	}
	return []record.StatusWrite{record.OrdinaryStatusWrite("in_progress", status, "")}
}

// pwStatuses fails unless the dispatch_status rows queued for key are want.
func pwStatuses(t *testing.T, pool *pgxpool.Pool, key string, want []record.StatusWrite) {
	t.Helper()
	if got := statusWritesOf(t, pool, key); len(got) != len(want) || (len(want) > 0 && !reflect.DeepEqual(got, want)) {
		t.Fatalf("status writes of %s = %+v, want %+v", key, got, want)
	}
}

// pwNotice fails unless one awaiting-children notice is queued for key, to its architect, out of
// from, naming the parent, the start it holds and what that start was for, and each open child with
// its Dispatch status, and answers it.
func pwNotice(t *testing.T, pool *pgxpool.Pool, from, start phase.Phase, cause string, open ...string) record.Notice {
	t.Helper()
	told := spNotices(t, pool, spParent)
	if len(told) != 1 {
		t.Fatalf("awaiting-children notices = %+v, want one", told)
	}
	notice := told[0]
	if notice.Role != claim.RoleArchitect || notice.Phase != from {
		t.Fatalf("the notice = %+v, want it to the architect, of %s", notice, from)
	}
	for _, want := range []string{
		spParent + " is in awaiting_children, holding the start of " + string(start) + ": " + cause + ".",
		"No phase of " + spParent + " starts while Dispatch shows an open child under it: " + strings.Join(open, ", ") + ".",
	} {
		if !strings.Contains(notice.Reason, want) {
			t.Fatalf("the notice's reason %q does not say %q", notice.Reason, want)
		}
	}
	return notice
}

// pwHeld fails unless key waits in awaiting_children holding the start of start since the engine's
// clock, its reason cause followed by each open child with its Dispatch status, showing
// in_progress, holding no admission slot and no start of a worker, and answers the held start.
func pwHeld(t *testing.T, pool *pgxpool.Pool, key string, start phase.Phase, cause string, open ...string) record.ChildrenWait {
	t.Helper()
	got := issue(t, pool, key)
	wait := got.ChildrenWait
	if got.Phase != phase.AwaitingChildren || wait == nil || wait.Start != start {
		t.Fatalf("%s = phase %s, held start %+v, worker starts %+v; want awaiting_children holding the start of %s, since %v is open",
			key, got.Phase, wait, pwWorkerStarts(t, pool, key), start, open)
	}
	if want := cause + ", while these Dispatch children of " + key + " were open: " + strings.Join(open, ", "); wait.Reason != want {
		t.Fatalf("the held start's reason = %q, want %q", wait.Reason, want)
	}
	if !wait.Since.Equal(fixedNow) || !strings.Contains(wait.Task, "Phase: "+string(start)+".") {
		t.Fatalf("the held start = %+v, want the task of %s, held since %s", wait, start, fixedNow)
	}
	if got.Status != "in_progress" || got.ChildrenWaitedAt == nil || !got.ChildrenWaitedAt.Equal(fixedNow) {
		t.Fatalf("%s = status %s, waited at %v; want in_progress, %s", key, got.Status, got.ChildrenWaitedAt, fixedNow)
	}
	if holdsSlot(t, pool, key) {
		t.Fatalf("slots = %+v, want none for %s: awaiting_children keeps none", slots(t, pool), key)
	}
	if starts := pwWorkerStarts(t, pool, key); len(starts) != 0 {
		t.Fatalf("worker starts of %s = %+v, want none: no phase of a waiting parent starts", key, starts)
	}
	return *wait
}

// pwMove is one of the ten moves R9 holds for an open child: the phase it leaves, what seeds it
// beyond the parent, its pull request and its open child, the fact that makes it, how the check
// state that decides a tester's completion reads (tested, pwStatesFirst), and what the held
// start, the replaced notice and the counts read after it.
type pwMove struct {
	name    string
	from    phase.Phase
	seed    func(*testing.T, *pgxpool.Pool)
	source  string
	fact    intake.Fact
	tested  string
	start   phase.Phase
	cause   string
	summary string
	pass    int
	rounds  int
	pairs   map[string]int
}

// pwFailedCause is the cause the held start of a testing move back names: the testing move's own
// sentence, each check that does not pass at the tested head (workflow's testingMove).
const pwFailedCause = "these checks do not pass at " + pwHead + ": k1: no (fail)"

// pwMoves is the eight moves of the ten k12 names that a start follows, the planner's completion
// first: it is the case the negative control fails at. The ninth, a conflicting head from
// testing, starts no phase since cybermonklabs/legion#4464 (the issue keeps its phase while the
// implementer merges the base forward), so it holds nothing; the test that follows the table
// reads what it does instead. The tenth, the tester's own move back out of testing, is refused
// now that a testing step ends by the states of its checks, so it starts no phase either, and
// the case after the table reads that too.
func pwMoves(t *testing.T) []pwMove {
	t.Helper()
	complete := func(role claim.Role, summary, rec string) intake.HandoffComplete {
		return intake.HandoffComplete{Generation: 1, Issue: spParent, Role: role, Claim: claim.Token(string(role)), Summary: summary,
			Commit: pwHead, HandoffRecord: rec, HandoffVersion: 1}
	}
	// The architect's own move names the claim that asked for it, which the request's refusals
	// read (intake's BackwardMove.Request): a worker's move is judged by its role alone.
	architect := pwRoleToken(t, spParent, claim.RoleArchitect)
	back := func(by claim.Role, to phase.Phase, reason string) intake.BackwardMove {
		move := intake.BackwardMove{Issue: spParent, Requester: by, To: to, Reason: reason}
		if by == claim.RoleArchitect {
			move.By = architect
		}
		return move
	}
	return []pwMove{
		{name: "the planner's completion", from: phase.Planning, source: "api",
			fact:  complete(claim.RolePlanner, "planned", "plan-1"),
			start: phase.Implementing, cause: "the planner completed planning", summary: "planned"},
		{name: "the implementer's completion", from: phase.Implementing, source: "api",
			fact:  complete(claim.RoleImplementer, "implemented", "implement-1"),
			start: phase.Testing, cause: "the implementer completed implementing", summary: "implemented"},
		{name: "the testing step's pass", from: phase.Testing, source: "api",
			fact: complete(claim.RoleTester, "every check passed", "test-1"), tested: "pass",
			start: phase.Reviewing, cause: "the tester completed testing", summary: "every check passed"},
		// The deployment's hook stands on every move back with `every: 1`: the fail counts its
		// pair, and the hook writes no prompt and posts no firing message while the parent waits.
		{name: "the testing step's fail counts its pair and fires no hook", from: phase.Testing, source: "api",
			fact: complete(claim.RoleTester, "a check failed", "test-1"), tested: "fail",
			start: phase.Implementing, cause: pwFailedCause, summary: "a check failed",
			pass: 1, pairs: map[string]int{"testing-implementing": 1}},
		{name: "the reviewer's approval", from: phase.Reviewing, seed: func(t *testing.T, pool *pgxpool.Pool) { pwReviewed(t, pool) },
			source: "github", fact: pwReview("APPROVED", "the change is right"),
			start: phase.Accepting, cause: "a review approved the head", summary: pwReviewSummary},
		{name: "the reviewer's request for changes counts one round", from: phase.Reviewing,
			seed:   func(t *testing.T, pool *pgxpool.Pool) { pwReviewed(t, pool) },
			source: "github", fact: pwReview("CHANGES_REQUESTED", "rename the widget"),
			start: phase.Implementing, cause: "a review asked for changes: rename the widget", summary: pwReviewSummary,
			pass: 1, rounds: 1, pairs: map[string]int{"reviewing-implementing": 1}},
		{name: "the architect's request_backward_move from accepting to implementing", from: phase.Accepting, source: "api",
			fact:  back(claim.RoleArchitect, phase.Implementing, "the acceptance found a gap"),
			start: phase.Implementing, cause: "moved back to implementing by the architect: the acceptance found a gap",
			pass: 1, pairs: map[string]int{"accepting-implementing": 1}},
		{name: "the architect's request_backward_move from accepting to testing", from: phase.Accepting, source: "api",
			fact:  back(claim.RoleArchitect, phase.Testing, "the checks must run again"),
			start: phase.Testing, cause: "moved back to testing by the architect: the checks must run again",
			pass: 1, pairs: map[string]int{"accepting-testing": 1}},
	}
}

// pwHoldsMove runs one move a start follows (the eight of pwMoves, and the tester's fail at a
// conflicting head) on a live tree whose root is the parent, holding the slot
// its phase keeps, with its pull request open and one child filed and not released, and reads what
// R9 and R10 say the move writes: the parent in awaiting_children holding the move's start with its
// reason, its slot freed and no worker started; one awaiting-children notice, carrying what the
// notice it replaces would have carried, and no phase-finished notice; the status
// written in_progress where the record showed another; the counts the trigger makes today and no
// more; no hook prompt or firing message; and no phase-ended message, since the role that made the
// move ended its own phase. A second pass sends nothing again.
func pwHoldsMove(t *testing.T, move pwMove) {
	pool := migratedPool(t)
	admission := pwAdmission(t, 2)
	spSeedParent(t, pool, spParent, move.from, record.PhaseKeepsSlot(move.from), nil)
	pwPullRequest(t, pool, spParent, record.PullRequestOpen)
	seedMirrorChild(t, pool, spChild, "todo", spParent)
	if move.seed != nil {
		move.seed(t, pool)
	}

	pwApplied(t, pool, admission, move.source, "move", pwStatesFirst(t, pool, admission, move.fact, move.tested))

	pwHeld(t, pool, spParent, move.start, move.cause, spChild+" (todo)")
	notice := pwNotice(t, pool, move.from, move.start, move.cause, spChild+" (todo)")
	if notice.Summary != move.summary {
		t.Fatalf("the notice = %+v, want the summary %q of the notice it replaces", notice, move.summary)
	}
	want := map[record.NoticeKind]int{workflow.AwaitingChildrenNoticeKind: 1}
	if kinds := pwNoticeKinds(t, pool, spParent); !maps.Equal(kinds, want) {
		t.Fatalf("notices by kind = %v, want %v: no phase-finished notice", kinds, want)
	}
	pwStatuses(t, pool, spParent, pwInProgressOver(workflow.PhaseStatus(move.from)))
	implementer := spPhaseRow(t, pool, spParent, claim.RoleImplementer)
	if implementer.Pass != move.pass || implementer.Rounds != move.rounds || !maps.Equal(implementer.BackwardMoves, move.pairs) {
		t.Fatalf("the implementer's counts = pass %d, rounds %d, moves %v; want pass %d, rounds %d, moves %v", implementer.Pass,
			implementer.Rounds, implementer.BackwardMoves, move.pass, move.rounds, move.pairs)
	}
	if prompts := pwPayloads[record.HookPrompt](t, pool, spParent); len(prompts) != 0 {
		t.Fatalf("hook prompts = %+v, want none while the parent waits", prompts)
	}
	if posts := pwPayloads[record.MessagePost](t, pool, spParent); len(posts) != 0 {
		t.Fatalf("messages = %+v, want none: no hook's firing message, and no cap message below the cap", posts)
	}
	if ended := pwPayloads[record.PhaseEnded](t, pool, spParent); len(ended) != 0 {
		t.Fatalf("phase-ended messages = %+v, want none: the role that made the move ended its own phase", ended)
	}

	spPass(t, pool, admission)
	if kinds := pwNoticeKinds(t, pool, spParent); !maps.Equal(kinds, want) {
		t.Fatalf("notices by kind after a second pass = %v, want still %v", kinds, want)
	}
	if again := pwPayloads[record.PhaseEnded](t, pool, spParent); len(again) != 0 {
		t.Fatalf("phase-ended messages after a second pass = %+v, want still none", again)
	}
}

// pwStopTester is the fifth parent the check names: its tester in the middle of testing when an
// admission pass finds a child of it filed and not released. It answers the tester's claim token.
func pwStopTester(t *testing.T) (*pgxpool.Pool, *Admission, claim.Token) {
	t.Helper()
	pool := migratedPool(t)
	admission := pwAdmission(t, 2)
	token := pwMidPhase(t, pool, spParent, phase.Testing, supervise.StateIdle, "")
	seedMirrorChild(t, pool, spChild, "todo", spParent)
	spPass(t, pool, admission)
	return pool, admission, token
}

// pwPath is one path into awaiting_children (workflow's awaitChildren), which R10's sentence on a
// dropped merge forward reaches: the phase the parent stands in, or is held from, when the move
// takes it, which is the phase a merge forward of its pull request was asked in; the worker the
// move stops in the middle of its phase, "" for none; seed, which records the parent, its slot
// and its pull request (pwPullRequest) and answers the admission pass the case runs under; and the
// move itself, made once a child is filed under the parent.
type pwPath struct {
	name  string
	at    phase.Phase
	ended claim.Role
	seed  func(*testing.T, *pgxpool.Pool) *Admission
	move  func(*testing.T, *pgxpool.Pool, *Admission)
}

// pwHeldFromTesting records the parent held from testing after its tester's claim failed, as the
// architect's retry and the operator's restart find it, and answers the admission pass.
func pwHeldFromTesting(t *testing.T, pool *pgxpool.Pool) *Admission {
	t.Helper()
	admission := pwAdmission(t, 2)
	spSeedParent(t, pool, spParent, phase.Testing, true, nil)
	pwPullRequest(t, pool, spParent, record.PullRequestOpen)
	pwApplied(t, pool, admission, "supervisor", "failed", intake.ClaimFailed{Issue: spParent, Role: claim.RoleTester,
		Reason: "the launch budget ran out"})
	if got := issue(t, pool, spParent); got.Phase != phase.Held || got.Hold == nil || got.Hold.From != phase.Testing {
		t.Fatalf("the parent after its tester's claim failed = phase %s, hold %+v; want held from testing", got.Phase, got.Hold)
	}
	return admission
}

// pwPaths is every path into awaiting_children, 14 in all: the eight transition triggers of
// pwMoves, which reach it through the transition's guard (holdForChildren); the three acts that
// release a hold (retryPhase): the architect's retry, the operator's restart (restartPhase, the
// fact `legion claims retry` applies) and a todo on a held root; and the admission pass's three
// cases (StopForChildren): a parent in accepting, a start written and not delivered, and a worker
// in the middle of its phase. A path added to awaitChildren later goes into this table.
func pwPaths(t *testing.T) []pwPath {
	t.Helper()
	var paths []pwPath
	for _, move := range pwMoves(t) {
		paths = append(paths, pwPath{name: move.name, at: move.from,
			seed: func(t *testing.T, pool *pgxpool.Pool) *Admission {
				admission := pwAdmission(t, 2)
				spSeedParent(t, pool, spParent, move.from, record.PhaseKeepsSlot(move.from), nil)
				pwPullRequest(t, pool, spParent, record.PullRequestOpen)
				if move.seed != nil {
					move.seed(t, pool)
				}
				return admission
			},
			move: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				pwApplied(t, pool, admission, move.source, "move", pwStatesFirst(t, pool, admission, move.fact, move.tested))
			}})
	}
	return append(paths,
		pwPath{name: "the architect's retry of a parent held from testing", at: phase.Testing, seed: pwHeldFromTesting,
			move: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				pwApplied(t, pool, admission, "api", "retry", intake.RetryOrEscalate{Issue: spParent, Decision: intake.RetryDecision,
					Reason: "the runner is back"})
			}},
		pwPath{name: "the operator's restart of a parent held from testing", at: phase.Testing, seed: pwHeldFromTesting,
			move: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				pwApplied(t, pool, admission, "api", "restart", intake.RestartPhase{Issue: spParent, Role: claim.RoleTester})
			}},
		pwPath{name: "a todo on a root held from testing", at: phase.Testing,
			seed: func(t *testing.T, pool *pgxpool.Pool) *Admission {
				spSeedParent(t, pool, spParent, phase.Held, true, func(held *record.Issue) {
					held.Hold, held.Status, held.DispatchStatus, held.LastDispatchSeq = &record.Hold{From: phase.Testing}, "testing", "testing", 4
				})
				pwPullRequest(t, pool, spParent, record.PullRequestOpen)
				return pwAdmission(t, 2)
			},
			move: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				pwApplied(t, pool, admission, "dispatch", "todo", intake.DispatchIssue{Key: spParent, ResolvedRepo: "acme/widgets", Seq: 5,
					Type: "issue.updated", Status: "todo", Title: spParent, Rank: spParent, HandedOver: true})
			}},
		pwPath{name: "the pass on a parent in accepting", at: phase.Accepting,
			seed: func(t *testing.T, pool *pgxpool.Pool) *Admission {
				spSeedParent(t, pool, spParent, phase.Accepting, false, nil)
				pwPullRequest(t, pool, spParent, record.PullRequestOpen)
				return pwAdmission(t, 2)
			},
			move: spPass},
		// The cap's one slot is the filler's, so the tester's start, written before the child is
		// filed, waits for it and is not delivered.
		pwPath{name: "the pass on a start of testing written and not delivered", at: phase.Testing,
			seed: func(t *testing.T, pool *pgxpool.Pool) *Admission {
				admission := pwAdmission(t, 1)
				seedStarted(t, pool, spFiller, phase.Implementing, nil)
				spSeedParent(t, pool, spParent, phase.Testing, false, nil)
				pwPullRequest(t, pool, spParent, record.PullRequestOpen)
				spSeedPhase(t, pool, record.PhaseRow{Issue: spParent, Role: claim.RoleTester, Claim: "tester", Entry: 2,
					EntryTask: pwTask(spParent, phase.Testing), EntryRaisedAt: fixedNow.Add(-time.Hour)})
				inTx(t, pool, func(tx pgx.Tx) {
					if err := workflow.StartOrOwe(context.Background(), tx, record.NewStore(), spParent, record.SuperviseRequest{Op: "start",
						Tree: spParent, Role: claim.RoleTester, Task: pwTask(spParent, phase.Testing), Generation: 1,
						Phase: phase.Testing, Entry: 2}, fixedNow); err != nil {
						t.Fatalf("write the tester's start: %v", err)
					}
				})
				spPass(t, pool, admission)
				if starts := pwWorkerStarts(t, pool, spParent); len(starts) != 1 || !starts[0].needsSlot || starts[0].claimable || holdsSlot(t, pool, spParent) {
					t.Fatalf("starts of %s before the child = %+v, want the tester's waiting for the slot the cap does not give it", spParent, starts)
				}
				return admission
			},
			move: spPass},
		pwPath{name: "the pass on a tester in the middle of testing", at: phase.Testing, ended: claim.RoleTester,
			seed: func(t *testing.T, pool *pgxpool.Pool) *Admission {
				pwMidPhase(t, pool, spParent, phase.Testing, supervise.StateIdle, "")
				return pwAdmission(t, 2)
			},
			move: spPass},
	)
}

// pwForward is one state of the merge forward the parent's pull request record holds when a path
// takes the parent into awaiting_children: asked, the record's merge forward for an ask made in a
// phase, nil for none; and dropped, the merge forward the move drops before it landed as the
// messages name it, nil where it drops none.
type pwForward struct {
	name    string
	asked   func(phase.Phase) *record.MergeForward
	dropped *record.DroppedMergeForward
}

// pwForwards is the three states: no merge forward, one asked and not landed, and one that
// landed, which the move drops with no word of it.
func pwForwards() []pwForward {
	return []pwForward{
		{name: "no merge forward", asked: func(phase.Phase) *record.MergeForward { return nil }},
		{name: "a merge forward asked and not landed", asked: func(at phase.Phase) *record.MergeForward { return pwAsked(at, false) },
			dropped: &record.DroppedMergeForward{Head: pwHead, Base: "main", Role: claim.RoleImplementer}},
		{name: "a merge forward that landed", asked: func(at phase.Phase) *record.MergeForward { return pwAsked(at, true) }},
	}
}

// pwDropsForward runs path under forward: the parent recorded as path seeds it, its pull request's
// merge forward as forward has it, asked in the phase the parent stands in or is held from (or in
// implementing for a parent in planning, which no merge forward is asked in), and one child filed
// and not released. It reads that the move took the parent into awaiting_children and dropped the
// merge forward, and what the move tells (dispatch://INFRA-3320 R10): with one that had not
// landed, the implementer is sent one role message that names it, and the worker the move stops
// is told in the phase-ended message's form for that worker; with none, or one that landed, the
// implementer is sent no role message and the stopped worker is told in the plain form.
func pwDropsForward(t *testing.T, path pwPath, forward pwForward) {
	pool := migratedPool(t)
	admission := path.seed(t, pool)
	asked := path.at
	if !classify.MergeForwardStands(record.Issue{Phase: asked}, phase.Order) {
		asked = phase.Implementing
	}
	pwSeedForward(t, pool, forward.asked(asked))
	seedMirrorChild(t, pool, spChild, "todo", spParent)

	path.move(t, pool, admission)

	got := issue(t, pool, spParent)
	if got.Phase != phase.AwaitingChildren || got.ChildrenWait == nil {
		t.Fatalf("%s = phase %s, held start %+v; want awaiting_children, since %s (todo) is open", spParent, got.Phase, got.ChildrenWait, spChild)
	}
	if left := pwForwardOf(t, pool); left != nil {
		t.Fatalf("the merge forward of %s = %+v, want it dropped by the move", spParent, left)
	}
	pwToldImplementer(t, pool, forward.dropped)
	ended := pwPayloads[record.PhaseEnded](t, pool, spParent)
	if path.ended == "" {
		if len(ended) != 0 {
			t.Fatalf("phase-ended messages = %+v, want none: the move stopped no worker in the middle of its phase", ended)
		}
		return
	}
	pwToldR10(t, ended, path.ended, path.at, got.ChildrenWait.Reason, forward.dropped)
}

// No phase of a parent starts while an open Dispatch child stands under it (dispatch://INFRA-3320
// R9, R10, R11, and its check dispatch://INFRA-3320/artifact/verification-md#b-k12): every case is
// one live tree whose root, the parent, holds the slot its phase keeps, run through the workflow
// engine and the admission pass as the daemon runs them.
func TestParentWaitsHoldsEveryPhaseStart(t *testing.T) {
	for _, move := range pwMoves(t) {
		t.Run(move.name, func(t *testing.T) { pwHoldsMove(t, move) })
	}

	// The one move back whose counts differ: a move made while the record holds the pull request's
	// own head conflicting with its base counts the implementer's pass and no pair, and fires no
	// hook (workflow's recordPass, classify.ConflictRound), whether a child holds its start or not
	// (internal/workflow's TestEveryBackwardMoveIsCountedByItsPair reads the move with no child).
	// The tester's fail stands for it, under the deployment's hook on every move back.
	t.Run("the tester's fail at a head conflicting with its base counts the pass, no pair and no hook", func(t *testing.T) {
		pwHoldsMove(t, pwMove{name: "the tester's fail at a head conflicting with its base", from: phase.Testing, seed: pwConflicting,
			source: "api", fact: intake.HandoffComplete{Generation: 1, Issue: spParent, Role: claim.RoleTester, Claim: "tester",
				Summary: "a check failed", Commit: pwHead, HandoffRecord: "test-1", HandoffVersion: 1}, tested: "fail",
			start: phase.Implementing, cause: pwFailedCause, summary: "a check failed", pass: 1})
	})

	// The tester has no move back out of testing on this line: a testing step ends by the states
	// of its checks at the head the tester tested, so the move is refused and starts no phase for
	// a child to hold (workflow's backwardRefusal). The move from testing to implementing a start
	// does follow is the tester's fail above.
	t.Run("the tester's request_backward_move to implementing is refused and starts nothing", func(t *testing.T) {
		pool := migratedPool(t)
		admission := pwAdmission(t, 2)
		spSeedParent(t, pool, spParent, phase.Testing, record.PhaseKeepsSlot(phase.Testing), nil)
		pwPullRequest(t, pool, spParent, record.PullRequestOpen)
		seedMirrorChild(t, pool, spChild, "todo", spParent)

		result := pwApply(t, pool, admission, "api", "move", intake.BackwardMove{Issue: spParent,
			Requester: claim.RoleTester, To: phase.Implementing, Reason: "the build is red"})
		pwRefused(t, result, "BACKWARD_REFUSED", "the tester's move back to implementing")

		if got := issue(t, pool, spParent); got.Phase != phase.Testing || got.ChildrenWait != nil {
			t.Fatalf("the parent after the refused move = phase %s, held start %+v; want testing, nothing held", got.Phase, got.ChildrenWait)
		}
		if kinds := pwNoticeKinds(t, pool, spParent); len(kinds) != 0 {
			t.Fatalf("notices by kind = %v, want none: the refused move started no phase", kinds)
		}
	})

	// R10's "every move into awaiting_children" over a merge forward of the parent's pull request:
	// each of the 14 paths into awaiting_children (pwPaths), under each of the three states of the
	// merge forward its record holds (pwForwards), 42 cases in all (pwDropsForward).
	t.Run("every move into awaiting_children tells the implementer of a merge forward it drops", func(t *testing.T) {
		paths := pwPaths(t)
		if len(paths) != 14 {
			t.Fatalf("the paths into awaiting_children = %d, want the 14 there are", len(paths))
		}
		for _, path := range paths {
			for _, forward := range pwForwards() {
				t.Run(path.name+", "+forward.name, func(t *testing.T) { pwDropsForward(t, path, forward) })
			}
		}
	})

	// The tenth move k12 names, a confirmed conflict in testing, starts no phase since
	// cybermonklabs/legion#4464. With the base's rules unread, classify.ConflictNeedsMerge gives a
	// reason (record.ConflictRulesUnread), so the daemon asks the implementer to merge the base
	// forward and tells the tester to hold while the issue keeps testing (workflow's
	// mergeForward): nothing is held. The tester's hold is delivered, and the next admission pass,
	// which finds a child filed under the parent, stops the tester into awaiting_children, holding
	// testing, and drops the record's merge forward before it landed (R11: no role of a waiting
	// parent works). The ask row still queued then finishes undelivered and wakes no role. The
	// implementer is sent a role message that names the dropped merge forward (the daemon's
	// TestParentWaitsHoldsEveryPhaseStart reads that a released implementer gets nothing of it),
	// and the tester, whose hold was delivered, is told in the R10 message to write and push
	// nothing more, with no `Commit and push`.
	t.Run("a confirmed conflict in testing with the base's rules unread starts no phase, and the next pass stops the tester and drops the merge forward", func(t *testing.T) {
		pool := migratedPool(t)
		admission := pwAdmission(t, 2)
		pwMidPhase(t, pool, spParent, phase.Testing, supervise.StateIdle, "")
		inTx(t, pool, func(tx pgx.Tx) {
			pr := record.PullRequest{Issue: spParent, Repo: "acme/widgets", Number: 42, Branch: "legion/" + spParent, HeadSHA: pwHead,
				Generation: 1, State: record.PullRequestOpen, Base: "main", Failing: []string{}, ConflictingHead: pwHead}
			if err := record.NewStore().PutPullRequest(context.Background(), tx, pr); err != nil {
				t.Fatalf("record the confirmed conflict of %s: %v", spParent, err)
			}
		})

		pwApplied(t, pool, admission, "github", "conflict", intake.PullRequestMergeability{Repo: "acme/widgets", Number: 42, Base: "main",
			HeadSHA: pwHead, Mergeable: record.MergeabilityConflicting, MergeState: record.MergeStateDirty, Rules: nil})

		if got := issue(t, pool, spParent); got.Phase != phase.Testing || got.ChildrenWait != nil {
			t.Fatalf("the parent after the conflict = phase %s, held start %+v; want testing, nothing held", got.Phase, got.ChildrenWait)
		}
		if starts := pwWorkerStarts(t, pool, spParent); len(starts) != 0 {
			t.Fatalf("worker starts = %+v, want none: the conflict started no phase", starts)
		}
		asked := pwPayloads[record.MergeForwardMessage](t, pool, spParent)
		if len(asked) != 2 || asked[0].Role != claim.RoleImplementer || asked[0].Step != record.MergeForwardAsk ||
			asked[1].Role != claim.RoleTester || asked[1].Step != record.MergeForwardHold ||
			asked[0].Phase != phase.Testing || asked[1].Phase != phase.Testing || asked[0].Why != record.ConflictRulesUnread {
			t.Fatalf("merge-forward messages = %+v, want the implementer's ask and the tester's hold, both in testing, for the base's rules unread", asked)
		}
		// The tester's hold is delivered: its lane finishes the row once it has published it.
		inTx(t, pool, func(tx pgx.Tx) {
			ctx := context.Background()
			var id int64
			if err := tx.QueryRow(ctx, `select id from outbox where issue = $1 and kind = $2 and payload->>'step' = $3`,
				spParent, string(record.OutboxKindMergeForward), string(record.MergeForwardHold)).Scan(&id); err != nil {
				t.Fatalf("read the tester's hold: %v", err)
			}
			if err := record.NewStore().FinishOutbox(ctx, tx, id, ""); err != nil {
				t.Fatalf("finish the tester's hold %d: %v", id, err)
			}
		})
		seedMirrorChild(t, pool, spChild, "todo", spParent)

		spPass(t, pool, admission)

		const cause = "the admission pass stopped the tester's testing phase"
		wait := pwHeld(t, pool, spParent, phase.Testing, cause, spChild+" (todo)")
		if starts := pwWorkerStarts(t, pool, spParent); len(starts) != 0 {
			t.Fatalf("worker starts = %+v, want none: the pass started no phase", starts)
		}
		queued := pwPayloads[record.MergeForwardMessage](t, pool, spParent)
		if len(queued) != 1 || queued[0] != asked[0] {
			t.Fatalf("merge-forward messages queued = %+v, want the implementer's ask alone: the tester's hold was delivered", queued)
		}
		inTx(t, pool, func(tx pgx.Tx) {
			_, _, left, err := workflow.MergeForwardAt(context.Background(), record.NewStore(), tx, spParent, queued[0].Role, queued[0].Step, queued[0].Head)
			if err != nil || left != "the record holds no merge forward" {
				t.Fatalf("the queued ask to the implementer is left %q (%v), want it undelivered: the record holds no merge forward", left, err)
			}
		})
		dropped := &record.DroppedMergeForward{Head: pwHead, Base: "main", Role: claim.RoleImplementer}
		pwToldImplementer(t, pool, dropped)
		pwToldR10(t, pwPayloads[record.PhaseEnded](t, pool, spParent), claim.RoleTester, phase.Testing, wait.Reason, dropped)
	})

	// The implementer whose phase a pass stops after a merge forward's ask reached it, the ask made
	// in implementing and not landed when the architect releases the parent's child, is told of
	// the dropped merge forward in the one phase-ended message the move sends it, which keeps
	// `Commit and push` and gives the begun and not-begun rule, and no role message of the issue
	// is written (dispatch://INFRA-3320 R10).
	t.Run("a pass that stops implementing after a merge forward's ask reached the implementer", func(t *testing.T) {
		pool := migratedPool(t)
		admission := pwAdmission(t, 2)
		pwMidPhase(t, pool, spParent, phase.Implementing, supervise.StateIdle, "")
		pwSeedForward(t, pool, pwAsked(phase.Implementing, false))

		pwApplied(t, pool, admission, "dispatch", "released", intake.DispatchIssue{Key: spChild, ResolvedRepo: "acme/widgets", Seq: 1,
			Type: "issue.updated", Status: "todo", Title: spChild, Parent: spParent, Rank: spChild, HandedOver: true})

		wait := pwHeld(t, pool, spParent, phase.Implementing, "the admission pass stopped the implementer's implementing phase", spChild+" (todo)")
		if left := pwForwardOf(t, pool); left != nil {
			t.Fatalf("the merge forward of %s = %+v, want it dropped by the move", spParent, left)
		}
		if messages := pwPayloads[record.RoleMessage](t, pool, spParent); len(messages) != 0 {
			t.Errorf("role messages = %+v, want none: the implementer is told in its phase-ended message", messages)
		}
		pwToldR10(t, pwPayloads[record.PhaseEnded](t, pool, spParent), claim.RoleImplementer, phase.Implementing, wait.Reason,
			&record.DroppedMergeForward{Head: pwHead, Base: "main", Role: claim.RoleImplementer})
	})

	// R10's message about a dropped merge forward goes to the role the record's ask names
	// (record.MergeForward.Asked), and not to the implementer by assumption. The record holds a
	// merger's ask made in merging and not landed, which the merger's move back out of merging left
	// there, and the tester is in the middle of a later testing entry, one no move out of merging
	// raised (its phases row names no entry_from): the pass leaves the entry that move back raised
	// where it stands (workflow's acceptedEntry), and stops the tester on this one once the
	// architect releases the parent's child. It drops the ask: the merger is sent one role message
	// on its own role topic that names it, the implementer is sent nothing, and the tester is told
	// in the R10 message to write and push nothing more, since the merger may be merging in the
	// workspace they share.
	t.Run("a pass that drops a merge forward asked of the merger tells the merger on its role topic", func(t *testing.T) {
		pool := migratedPool(t)
		admission := pwAdmission(t, 2)
		pwMidPhase(t, pool, spParent, phase.Testing, supervise.StateIdle, "")
		asked := pwAsked(phase.Merging, false)
		asked.Role, asked.Why = claim.RoleMerger, record.ConflictMergeNext
		pwSeedForward(t, pool, asked)

		pwApplied(t, pool, admission, "dispatch", "released", intake.DispatchIssue{Key: spChild, ResolvedRepo: "acme/widgets", Seq: 1,
			Type: "issue.updated", Status: "todo", Title: spChild, Parent: spParent, Rank: spChild, HandedOver: true})

		wait := pwHeld(t, pool, spParent, phase.Testing, "the admission pass stopped the tester's testing phase", spChild+" (todo)")
		if left := pwForwardOf(t, pool); left != nil {
			t.Fatalf("the merge forward of %s = %+v, want it dropped by the move", spParent, left)
		}
		dropped := record.DroppedMergeForward{Head: pwHead, Base: "main", Role: claim.RoleMerger}
		merger := string(pwRoleToken(t, spParent, claim.RoleMerger))
		want := []record.RoleMessage{{Role: merger, Body: pwDroppedBody(spParent, dropped)}}
		if told := pwPayloads[record.RoleMessage](t, pool, spParent); !reflect.DeepEqual(told, want) {
			t.Fatalf("role messages = %+v, want only %+v: the merger's role topic is told of the merge forward asked of it", told, want)
		}
		ended := pwPayloads[record.PhaseEnded](t, pool, spParent)
		if len(ended) != 1 || ended[0].Role != claim.RoleTester || ended[0].Reason != wait.Reason || !reflect.DeepEqual(ended[0].MergeForward, &dropped) {
			t.Fatalf("phase-ended messages = %+v, want one to the tester naming the dropped merge forward %+v", ended, dropped)
		}
		writeNothing := "Write and push nothing more, because the merger may be merging in the workspace you share: put what you found on the Dispatch issue, and do no more work on testing."
		if message := ended[0].Message(spParent); !strings.Contains(message, writeNothing) || strings.Contains(message, "Commit and push") {
			t.Fatalf("the tester was told %q, want %q in it and no %q", message, writeNothing, "Commit and push")
		}
	})

	// The request for changes that reaches the cap counts its round as today, and the cap's
	// message and pr-blocked notice name the parent as waiting on its children.
	t.Run("a request for changes at review_round_cap tells the architect the parent waits on its children", func(t *testing.T) {
		pool := migratedPool(t)
		admission := pwAdmission(t, 2)
		spSeedParent(t, pool, spParent, phase.Reviewing, true, nil)
		pwPullRequest(t, pool, spParent, record.PullRequestOpen)
		seedMirrorChild(t, pool, spChild, "todo", spParent)
		pwReviewed(t, pool)
		spSeedPhase(t, pool, record.PhaseRow{Issue: spParent, Role: claim.RoleImplementer, Claim: "implementer", Rounds: pwRoundCap - 1})

		pwApplied(t, pool, admission, "github", "changes", pwReview("CHANGES_REQUESTED", "rename the widget"))

		pwHeld(t, pool, spParent, phase.Implementing, "a review asked for changes: rename the widget", spChild+" (todo)")
		if rounds := spPhaseRow(t, pool, spParent, claim.RoleImplementer).Rounds; rounds != pwRoundCap {
			t.Fatalf("rounds = %d, want %d: the request for changes counts its round", rounds, pwRoundCap)
		}
		want := fmt.Sprintf("Issue reached review_round_cap=%d: %d rounds counted, 0 conflict rounds not counted. %s waits on its open Dispatch children in awaiting_children, so no implementing round started: %s (todo).",
			pwRoundCap, pwRoundCap, spParent, spChild)
		if posts := pwPayloads[record.MessagePost](t, pool, spParent); len(posts) != 1 || posts[0].Body != want {
			t.Fatalf("messages = %+v, want the cap's %q", posts, want)
		}
		capped := gateNotices(t, pool, spParent, "pr-blocked")
		if len(capped) != 1 || capped[0].Reason != want {
			t.Fatalf("pr-blocked notices = %+v, want one carrying %q", capped, want)
		}
		kinds := map[record.NoticeKind]int{workflow.AwaitingChildrenNoticeKind: 1, "pr-blocked": 1}
		if got := pwNoticeKinds(t, pool, spParent); !maps.Equal(got, kinds) {
			t.Fatalf("notices by kind = %v, want %v", got, kinds)
		}
	})

	// The pass's moves: a parent in accepting, where no worker runs, moves at the next pass and is
	// written in_progress over the needs_review it showed; nobody is sent a phase-ended message.
	t.Run("a parent in accepting moves at the next admission pass", func(t *testing.T) {
		pool := migratedPool(t)
		admission := pwAdmission(t, 2)
		spSeedParent(t, pool, spParent, phase.Accepting, false, nil)
		pwPullRequest(t, pool, spParent, record.PullRequestOpen)
		seedMirrorChild(t, pool, spChild, "todo", spParent)

		spPass(t, pool, admission)

		const cause = "the head the step before accepting passed waited in accepting for the architect's sign-off"
		pwHeld(t, pool, spParent, phase.Accepting, cause, spChild+" (todo)")
		pwNotice(t, pool, phase.Accepting, phase.Accepting, cause, spChild+" (todo)")
		pwStatuses(t, pool, spParent, pwInProgressOver("needs_review"))
		if ended := pwPayloads[record.PhaseEnded](t, pool, spParent); len(ended) != 0 {
			t.Fatalf("phase-ended messages = %+v, want none: no worker runs in accepting", ended)
		}
	})

	// A parent whose implementer's start was written and waits for the slot the full cap does not
	// give it moves at the pass, holding that start with the task the implementer's phases row
	// keeps for entry 2, the entry the start was written for, and the start's row is finished
	// there, so the start is never delivered.
	t.Run("a parent whose implementer's start was written and waits for a slot moves at the pass, the start not delivered", func(t *testing.T) {
		pool := migratedPool(t)
		admission := pwAdmission(t, 1)
		seedStarted(t, pool, spFiller, phase.Implementing, nil)
		spSeedParent(t, pool, spParent, phase.Implementing, false, nil)
		pwPullRequest(t, pool, spParent, record.PullRequestOpen)
		spSeedPhase(t, pool, record.PhaseRow{Issue: spParent, Role: claim.RoleImplementer, Claim: "implementer", Entry: 2,
			EntryTask: pwTask(spParent, phase.Implementing), EntryRaisedAt: fixedNow.Add(-time.Hour)})
		inTx(t, pool, func(tx pgx.Tx) {
			if err := workflow.StartOrOwe(context.Background(), tx, record.NewStore(), spParent, record.SuperviseRequest{Op: "start",
				Tree: spParent, Role: claim.RoleImplementer, Task: pwTask(spParent, phase.Implementing), Generation: 1,
				Phase: phase.Implementing, Entry: 2}, fixedNow); err != nil {
				t.Fatalf("write the implementer's start: %v", err)
			}
		})
		spPass(t, pool, admission)
		if starts := pwWorkerStarts(t, pool, spParent); len(starts) != 1 || !starts[0].needsSlot || starts[0].claimable || holdsSlot(t, pool, spParent) {
			t.Fatalf("starts of %s before the child = %+v, want the implementer's waiting for the slot the cap does not give it", spParent, starts)
		}
		seedMirrorChild(t, pool, spChild, "todo", spParent)

		spPass(t, pool, admission)

		const cause = "its start of implementing was written and had not reached the implementer"
		if wait := pwHeld(t, pool, spParent, phase.Implementing, cause, spChild+" (todo)"); wait.Task != pwTask(spParent, phase.Implementing) {
			t.Fatalf("the held task = %q, want the task the implementer's phases row keeps for entry 2", wait.Task)
		}
		pwNotice(t, pool, phase.Implementing, phase.Implementing, cause, spChild+" (todo)")
		if starts := startsOf(t, pool, spParent); len(starts) != 0 {
			t.Fatalf("starts of %s = %+v, want none: the written start is finished and never delivered", spParent, starts)
		}
		if ended, retired := pwPayloads[record.PhaseEnded](t, pool, spParent), spRetired(t, pool, spParent); len(ended) != 0 || len(retired) != 0 {
			t.Fatalf("phase-ended messages = %+v, task retirements = %+v; want none: no worker had the phase", ended, retired)
		}
		pwStatuses(t, pool, spParent, nil)
	})

	// Each kind of open child holds the implementer's completion of the parent, and the held
	// start's reason and the architect's notice name it with the status Dispatch shows it in.
	parent := spParent
	for _, kind := range []struct {
		name   string
		status string
		seed   func(*testing.T, *pgxpool.Pool)
	}{
		{name: "an open child in implementing holds the start", status: "in_progress", seed: func(t *testing.T, pool *pgxpool.Pool) {
			putIssue(t, pool, record.Issue{Key: spChild, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Project: testProject, Title: spChild, Tree: spParent, Parent: &parent, Phase: phase.Implementing, Generation: 1,
				Status: "in_progress", DispatchStatus: "in_progress", Rank: spChild, HandedOver: true, LastDispatchSeq: 1})
			takeSlot(t, pool, spChild)
			seedMirrorChild(t, pool, spChild, "in_progress", spParent)
		}},
		{name: "an open child released and waiting at admitted holds the start", status: "todo", seed: func(t *testing.T, pool *pgxpool.Pool) {
			putIssue(t, pool, record.Issue{Key: spChild, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Project: testProject, Title: spChild, Tree: spParent, Parent: &parent, Phase: phase.Admitted, Generation: 1,
				Status: "todo", DispatchStatus: "todo", Rank: spChild, HandedOver: true, LastDispatchSeq: 1})
			seedMirrorChild(t, pool, spChild, "todo", spParent)
		}},
		{name: "an open child the daemon never recorded, in triage, holds the start", status: "triage", seed: func(t *testing.T, pool *pgxpool.Pool) {
			seedMirrorChild(t, pool, spChild, "triage", spParent)
		}},
		{name: "an open child running as a tree of its own holds the start", status: "in_progress", seed: func(t *testing.T, pool *pgxpool.Pool) {
			seedStarted(t, pool, spChild, phase.Implementing, nil)
			seedMirrorChild(t, pool, spChild, "in_progress", spParent)
		}},
		{name: "an open child itself in awaiting_children holds the start", status: "in_progress", seed: func(t *testing.T, pool *pgxpool.Pool) {
			waited := fixedNow.Add(-time.Hour)
			putIssue(t, pool, record.Issue{Key: spChild, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Project: testProject, Title: spChild, Tree: spParent, Parent: &parent, Phase: phase.AwaitingChildren, Generation: 1,
				Status: "in_progress", DispatchStatus: "in_progress", Rank: spChild, HandedOver: true, LastDispatchSeq: 1,
				ChildrenWait: &record.ChildrenWait{Start: phase.Implementing, Task: pwTask(spChild, phase.Implementing),
					Reason: "a review asked for changes, while these Dispatch children of LEGION-301 were open: LEGION-302 (backlog)", Since: waited},
				ChildrenWaitedAt: &waited})
			seedMirrorChild(t, pool, spChild, "in_progress", spParent)
			seedMirrorChild(t, pool, "LEGION-302", "backlog", spChild)
		}},
		// The test of an open child is read whatever the tree's design gate stands at: here it is
		// closed, its document's latest version 3 above the approved version 2.
		{name: "a tree whose design gate is closed at a version above the approved one holds the start", status: "backlog",
			seed: func(t *testing.T, pool *pgxpool.Pool) {
				approved := 2
				seedGate(t, pool, record.DesignGate{Issue: spParent, ArtifactID: "5b0c2d7e-1f43-4a8e-9c6d-2e7f1a3b8d40", LatestVersion: 3,
					ApprovedVersion: &approved, EverOpened: true})
				seedMirrorChild(t, pool, spChild, "backlog", spParent)
			}},
	} {
		t.Run(kind.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := pwAdmission(t, 4)
			spSeedParent(t, pool, spParent, phase.Implementing, true, nil)
			pwPullRequest(t, pool, spParent, record.PullRequestOpen)
			kind.seed(t, pool)

			pwApplied(t, pool, admission, "api", "implemented", intake.HandoffComplete{Generation: 1, Issue: spParent,
				Role: claim.RoleImplementer, Claim: "implementer", Summary: "implemented", Commit: pwHead, HandoffRecord: "implement-1", HandoffVersion: 1})

			open := spChild + " (" + kind.status + ")"
			pwHeld(t, pool, spParent, phase.Testing, "the implementer completed implementing", open)
			pwNotice(t, pool, phase.Implementing, phase.Testing, "the implementer completed implementing", open)
		})
	}

	// A child whose own blocked_by link names the parent above it is named in the notice: that
	// wait never ends by itself.
	t.Run("a child whose blocked_by link names the parent is named in the architect's notice", func(t *testing.T) {
		pool := migratedPool(t)
		admission := pwAdmission(t, 2)
		spSeedParent(t, pool, spParent, phase.Implementing, true, nil)
		pwPullRequest(t, pool, spParent, record.PullRequestOpen)
		seedMirrorChild(t, pool, spChild, "todo", spParent, spParent)

		pwApplied(t, pool, admission, "api", "implemented", intake.HandoffComplete{Generation: 1, Issue: spParent,
			Role: claim.RoleImplementer, Claim: "implementer", Summary: "implemented", Commit: pwHead, HandoffRecord: "implement-1", HandoffVersion: 1})

		notice := pwNotice(t, pool, phase.Implementing, phase.Testing, "the implementer completed implementing", spChild+" (todo)")
		if want := workflow.ChildCycleReason(spChild, spParent) + "."; !strings.Contains(notice.Reason, want) {
			t.Fatalf("the notice's reason %q does not name the child's link: %q", notice.Reason, want)
		}
	})

	// The first parent: its implementer is in the middle of implementing when the architect
	// releases its child (the child's todo, which the engine records under the live tree). The
	// pass that fact runs moves the parent, frees its slot, holds implementing with the task its
	// implementer was given, and sends that implementer the R10 message. The completion of the
	// ended phase is the API's handoff_complete, which applies this same fact to the engine; the
	// engine's refusal is what the route answers. A second pass sends no second message.
	t.Run("an implementer in the middle of implementing when its child is released", func(t *testing.T) {
		pool := migratedPool(t)
		admission := pwAdmission(t, 2)
		token := pwMidPhase(t, pool, spParent, phase.Implementing, supervise.StateIdle, "")

		pwApplied(t, pool, admission, "dispatch", "released", intake.DispatchIssue{Key: spChild, ResolvedRepo: "acme/widgets", Seq: 1,
			Type: "issue.updated", Status: "todo", Title: spChild, Parent: spParent, Rank: spChild, HandedOver: true})

		if child := issue(t, pool, spChild); child.Tree != spParent || child.Phase != phase.Admitted {
			t.Fatalf("the released child = tree %s, phase %s; want it recorded in %s's tree at admitted", child.Tree, child.Phase, spParent)
		}
		const cause = "the admission pass stopped the implementer's implementing phase"
		wait := pwHeld(t, pool, spParent, phase.Implementing, cause, spChild+" (todo)")
		if wait.Task != pwTask(spParent, phase.Implementing) {
			t.Fatalf("the held task = %q, want the implementer's task %q", wait.Task, pwTask(spParent, phase.Implementing))
		}
		pwNotice(t, pool, phase.Implementing, phase.Implementing, cause, spChild+" (todo)")
		pwToldR10(t, pwPayloads[record.PhaseEnded](t, pool, spParent), claim.RoleImplementer, phase.Implementing, wait.Reason, nil)

		pwRefused(t, pwApply(t, pool, admission, "api", "late", pwLate(spParent, claim.RoleImplementer, token, "implement-late")),
			"HANDOFF_NOT_CURRENT_PHASE", "the implementer's completion of the ended phase")
		if row := spPhaseRow(t, pool, spParent, claim.RoleImplementer); row.HandoffRecord == "implement-late" {
			t.Fatalf("the implementer's row = %+v, want the refused completion unrecorded", row)
		}

		spPass(t, pool, admission)
		if ended := pwPayloads[record.PhaseEnded](t, pool, spParent); len(ended) != 1 {
			t.Fatalf("phase-ended messages after a second pass = %+v, want still one", ended)
		}
		if told := spNotices(t, pool, spParent); len(told) != 1 {
			t.Fatalf("awaiting-children notices after a second pass = %+v, want still one", told)
		}
	})

	// The second parent: the daemon released its implementer's idle pod, so the claim is suspended
	// by the release with its session kept. The pass moves the parent and queues the R10 message
	// to that implementer on the role-message lane; the move touches no claim. The wake is the
	// outbox's: its phase-ended executor (daemon's outbox.phaseEnded) calls wakeForDelivery
	// before it publishes, which finds the claim released, takes a pods.WakeDelivery resume
	// (store.TakePodWake, one pod_resumes row) and resumes the claim, the row waiting on its backoff
	// for the pod. The admission package cannot run that executor; the daemon's
	// TestAHookPromptForAReleasedPodWakesItAndWaits drives that same wakeForDelivery call for a hook
	// prompt to a released implementer and reads its one delivery wake.
	t.Run("an implementer whose pod the daemon released for idleness is sent the message that wakes it", func(t *testing.T) {
		pool := migratedPool(t)
		admission := pwAdmission(t, 2)
		token := pwMidPhase(t, pool, spParent, phase.Implementing, supervise.StateSuspended, supervise.SuspendedByReleased)
		seedMirrorChild(t, pool, spChild, "todo", spParent)

		spPass(t, pool, admission)

		wait := pwHeld(t, pool, spParent, phase.Implementing, "the admission pass stopped the implementer's implementing phase", spChild+" (todo)")
		pwToldR10(t, pwPayloads[record.PhaseEnded](t, pool, spParent), claim.RoleImplementer, phase.Implementing, wait.Reason, nil)
		if state, by := pwClaimState(t, pool, token); state != supervise.StateSuspended || by != supervise.SuspendedByReleased {
			t.Fatalf("the implementer's claim = %s by %q, want it released still, for the message's delivery to wake", state, by)
		}
	})

	// The fourth parent: its implementer is in a turn at the pass. It moves the same way: its slot
	// is freed, it is sent the R10 message, and its completion of the ended phase is refused.
	t.Run("an implementer in a turn at the pass", func(t *testing.T) {
		pool := migratedPool(t)
		admission := pwAdmission(t, 2)
		token := pwMidPhase(t, pool, spParent, phase.Implementing, supervise.StateWorking, "")
		seedMirrorChild(t, pool, spChild, "todo", spParent)

		spPass(t, pool, admission)

		wait := pwHeld(t, pool, spParent, phase.Implementing, "the admission pass stopped the implementer's implementing phase", spChild+" (todo)")
		pwToldR10(t, pwPayloads[record.PhaseEnded](t, pool, spParent), claim.RoleImplementer, phase.Implementing, wait.Reason, nil)
		pwRefused(t, pwApply(t, pool, admission, "api", "late", pwLate(spParent, claim.RoleImplementer, token, "implement-late")),
			"HANDOFF_NOT_CURRENT_PHASE", "the completion of the implementer whose turn ran at the pass")
	})

	// The fifth parent: its tester is in the middle of testing. It moves at the pass, written
	// in_progress over testing, its tester is sent the R10 message, and at the return the held
	// testing start comes back as testing (dispatch://INFRA-3320 R12): the tester's start names the
	// held start and its reason and the child that merged during the wait, and asks for the round
	// on the head merged with the base's tip.
	t.Run("a tester in the middle of testing, whose held testing comes back as testing", func(t *testing.T) {
		pool, admission, _ := pwStopTester(t)

		const cause = "the admission pass stopped the tester's testing phase"
		wait := pwHeld(t, pool, spParent, phase.Testing, cause, spChild+" (todo)")
		pwNotice(t, pool, phase.Testing, phase.Testing, cause, spChild+" (todo)")
		pwToldR10(t, pwPayloads[record.PhaseEnded](t, pool, spParent), claim.RoleTester, phase.Testing, wait.Reason, nil)
		pwStatuses(t, pool, spParent, pwInProgressOver("testing"))

		pnChildMerged(t, pool, spChild, spParent, 51)
		spPass(t, pool, admission)

		if back := issue(t, pool, spParent); back.Phase != phase.Testing || back.ChildrenWait != nil {
			t.Fatalf("the parent after its child is done = phase %s, held start %+v; want testing, the start it held", back.Phase, back.ChildrenWait)
		}
		starts := pwWorkerStarts(t, pool, spParent)
		if len(starts) != 1 || starts[0].role != claim.RoleTester {
			t.Fatalf("starts of %s = %+v, want one tester start", spParent, starts)
		}
		for _, want := range []string{"Phase: testing.", "start of testing", wait.Reason, pnMergedChildren(spParent, pnMerged(spChild, 51))} {
			if !strings.Contains(starts[0].task, want) {
				t.Fatalf("the tester's task = %q, want it to say %q", starts[0].task, want)
			}
		}
		pnAsksNoForwardMerge(t, "the tester's task", starts[0].task)
		pnBaseMergeTimes(t, "the tester's task", starts[0].task, 1)
	})

	// The architect's retry of a parent held from implementing, delivered to its implementer before
	// a child opens (dispatch://INFRA-3320 R9, "with the task its worker was given"): the retry's
	// start hands the implementer a task of its own, with the architect's reason, on the entry the
	// phases row already names, and leaves the row's task as the transition wrote it (workflow's
	// restart). The pass that then finds a child open holds implementing with the task the
	// implementer's claim holds, the retry's, and the return starts implementing on that task.
	t.Run("a retry delivered to the implementer, then a child opens: the held task and the return's task are the retry's", func(t *testing.T) {
		pool := migratedPool(t)
		admission := pwAdmission(t, 2)
		spSeedParent(t, pool, spParent, phase.Implementing, true, nil)
		pwPullRequest(t, pool, spParent, record.PullRequestOpen)
		token := pwRoleToken(t, spParent, claim.RoleImplementer)
		spSeedPhase(t, pool, record.PhaseRow{Issue: spParent, Role: claim.RoleImplementer, Claim: token, Entry: 3,
			EntryTask: pwTask(spParent, phase.Implementing), EntryRaisedAt: fixedNow.Add(-time.Hour)})
		pwApplied(t, pool, admission, "supervisor", "failed", intake.ClaimFailed{Issue: spParent, Role: claim.RoleImplementer,
			Reason: "the launch budget ran out"})
		pwApplied(t, pool, admission, "api", "retry", intake.RetryOrEscalate{Issue: spParent, Decision: intake.RetryDecision,
			Reason: "the runner is back"})
		retry := pnWorkerStart(t, pool, spParent, claim.RoleImplementer)
		if row := spPhaseRow(t, pool, spParent, claim.RoleImplementer); retry.Entry != 3 || !strings.Contains(retry.Task, "Reason: the runner is back.") ||
			row.Entry != 3 || row.EntryTask != pwTask(spParent, phase.Implementing) {
			t.Fatalf("the retry's start = %+v over the phases row %+v; want the retry's own task on entry 3, the row's task the transition's", retry, row)
		}
		putWorkerClaim(t, pool, spParent, spParent, claim.RoleImplementer, 1, supervise.StateWorking, "")
		pwDeliver(t, pool, spParent, token, retry)
		seedMirrorChild(t, pool, spChild, "todo", spParent)

		spPass(t, pool, admission)

		wait := pwHeld(t, pool, spParent, phase.Implementing, "the admission pass stopped the implementer's implementing phase", spChild+" (todo)")
		if wait.Task != retry.Task {
			t.Fatalf("the held task = %q, want the retry's task the implementer was given, %q, not its entry's", wait.Task, retry.Task)
		}
		want := []record.SuperviseRequest{{Op: "retire_task", Tree: spParent, Role: claim.RoleImplementer, Generation: 1,
			Phase: phase.Implementing, Entry: 3}}
		if retired := spRetired(t, pool, spParent); !reflect.DeepEqual(retired, want) {
			t.Fatalf("task retirements = %+v, want %+v: the retry's task is the one the move takes back", retired, want)
		}

		pnChildMerged(t, pool, spChild, spParent, 51)
		spPass(t, pool, admission)

		if back := issue(t, pool, spParent); back.Phase != phase.Implementing || back.ChildrenWait != nil {
			t.Fatalf("the parent after its child is done = phase %s, held start %+v; want implementing, the start it held", back.Phase, back.ChildrenWait)
		}
		work := retry.Task + " " + pnMergedChildren(spParent, pnMerged(spChild, 51)) + "."
		if starts := pwWorkerStarts(t, pool, spParent); len(starts) != 1 || starts[0].role != claim.RoleImplementer || starts[0].task != work {
			t.Fatalf("starts of %s = %+v, want one implementer start on the retry's task %q", spParent, starts, work)
		}
	})

	// The same retry, delivered, and then the turn that took it ends before a child opens: the
	// turn's end retires the claim's pending delivery (store's RetireDelivery), so the claim holds
	// no task of the phase any more. The retry's task is still the one the implementer was given
	// (dispatch://INFRA-3320 R9), so the pass holds implementing with it and not with the entry's
	// own work, and the return starts implementing on it.
	t.Run("a retry delivered to the implementer whose turn then ends, then a child opens: the held task and the return's task are the retry's", func(t *testing.T) {
		pool := migratedPool(t)
		admission := pwAdmission(t, 2)
		spSeedParent(t, pool, spParent, phase.Implementing, true, nil)
		pwPullRequest(t, pool, spParent, record.PullRequestOpen)
		token := pwRoleToken(t, spParent, claim.RoleImplementer)
		spSeedPhase(t, pool, record.PhaseRow{Issue: spParent, Role: claim.RoleImplementer, Claim: token, Entry: 3,
			EntryTask: pwTask(spParent, phase.Implementing), EntryRaisedAt: fixedNow.Add(-time.Hour)})
		pwApplied(t, pool, admission, "supervisor", "failed", intake.ClaimFailed{Issue: spParent, Role: claim.RoleImplementer,
			Reason: "the launch budget ran out"})
		pwApplied(t, pool, admission, "api", "retry", intake.RetryOrEscalate{Issue: spParent, Decision: intake.RetryDecision,
			Reason: "the runner is back"})
		retry := pnWorkerStart(t, pool, spParent, claim.RoleImplementer)
		if retry.Entry != 3 || !strings.Contains(retry.Task, "Reason: the runner is back.") || retry.Task == pwTask(spParent, phase.Implementing) {
			t.Fatalf("the retry's start = %+v; want the retry's own task on entry 3, not the entry's work", retry)
		}
		putWorkerClaim(t, pool, spParent, spParent, claim.RoleImplementer, 1, supervise.StateIdle, "")
		pwDeliver(t, pool, spParent, token, retry)
		retired, err := pool.Exec(context.Background(), `delete from pending_task_deliveries where claim_token = $1 and delivery_id = 'outbox:delivered'`,
			string(token))
		if err != nil || retired.RowsAffected() != 1 {
			t.Fatalf("retire the retry's delivery as the end of the turn that took it does = %d rows, %v; want the one delivery retired",
				retired.RowsAffected(), err)
		}
		seedMirrorChild(t, pool, spChild, "todo", spParent)

		spPass(t, pool, admission)

		wait := pwHeld(t, pool, spParent, phase.Implementing, "the admission pass stopped the implementer's implementing phase", spChild+" (todo)")
		if wait.Task != retry.Task {
			t.Fatalf("the held task = %q, want the retry's task the implementer was given, %q, not its entry's, though the turn that took it has ended",
				wait.Task, retry.Task)
		}

		pnChildMerged(t, pool, spChild, spParent, 51)
		spPass(t, pool, admission)

		if back := issue(t, pool, spParent); back.Phase != phase.Implementing || back.ChildrenWait != nil {
			t.Fatalf("the parent after its child is done = phase %s, held start %+v; want implementing, the start it held", back.Phase, back.ChildrenWait)
		}
		work := retry.Task + " " + pnMergedChildren(spParent, pnMerged(spChild, 51)) + "."
		if starts := pwWorkerStarts(t, pool, spParent); len(starts) != 1 || starts[0].role != claim.RoleImplementer || starts[0].task != work {
			t.Fatalf("starts of %s = %+v, want one implementer start on the retry's task %q", spParent, starts, work)
		}
	})

	// The architect's retry of a parent held from implementing whose phases row a re-admission
	// cleared (record.Store.ClearTreeGeneration): the clear empties the task of the entry and keeps
	// the entry, and a held member is given no promoted worker, so the row stands on entry 3 with
	// no task when the retry comes. The retry writes its own task as the one the implementer was
	// given for entry 3 in the issue's generation, and leaves the row's empty task as it stands
	// (workflow's restart). The pass that then finds a child open holds implementing with the
	// retry's task, not the phase's resume task, and the return starts implementing on it
	// (dispatch://INFRA-3320 R9, "with the task its worker was given").
	t.Run("a retry after a re-admission cleared the entry's task, then a child opens: the held task and the return's task are the retry's", func(t *testing.T) {
		pool := migratedPool(t)
		admission := pwAdmission(t, 2)
		spSeedParent(t, pool, spParent, phase.Implementing, true, nil)
		pwPullRequest(t, pool, spParent, record.PullRequestOpen)
		token := pwRoleToken(t, spParent, claim.RoleImplementer)
		spSeedPhase(t, pool, record.PhaseRow{Issue: spParent, Role: claim.RoleImplementer, Claim: token, Entry: 3,
			EntryRaisedAt: fixedNow.Add(-time.Hour)})
		pwApplied(t, pool, admission, "supervisor", "failed", intake.ClaimFailed{Issue: spParent, Role: claim.RoleImplementer,
			Reason: "the launch budget ran out"})
		pwApplied(t, pool, admission, "api", "retry", intake.RetryOrEscalate{Issue: spParent, Decision: intake.RetryDecision,
			Reason: "the runner is back"})
		retry := pnWorkerStart(t, pool, spParent, claim.RoleImplementer)
		if row := spPhaseRow(t, pool, spParent, claim.RoleImplementer); retry.Entry != 3 || !strings.Contains(retry.Task, "Reason: the runner is back.") ||
			row.Entry != 3 || row.EntryTask != "" {
			t.Fatalf("the retry's start = %+v over the phases row %+v; want the retry's own task on entry 3, the row's task still empty", retry, row)
		}
		putWorkerClaim(t, pool, spParent, spParent, claim.RoleImplementer, 1, supervise.StateWorking, "")
		pwDeliver(t, pool, spParent, token, retry)
		seedMirrorChild(t, pool, spChild, "todo", spParent)

		spPass(t, pool, admission)

		wait := pwHeld(t, pool, spParent, phase.Implementing, "the admission pass stopped the implementer's implementing phase", spChild+" (todo)")
		if wait.Task != retry.Task {
			t.Fatalf("the held task = %q, want the retry's task the implementer was given, %q, not the phase's resume task", wait.Task, retry.Task)
		}

		pnChildMerged(t, pool, spChild, spParent, 51)
		spPass(t, pool, admission)

		work := retry.Task + " " + pnMergedChildren(spParent, pnMerged(spChild, 51)) + "."
		if starts := pwWorkerStarts(t, pool, spParent); len(starts) != 1 || starts[0].role != claim.RoleImplementer || starts[0].task != work {
			t.Fatalf("starts of %s = %+v, want one implementer start on the retry's task %q", spParent, starts, work)
		}
	})

	// The phase's resume task, which the catch-up of the implementer's ready claim queues at no
	// entry, the claim being level with the entry its phases row names (workflow's
	// readyWorkerTask), delivered to that implementer before a child opens: it says to carry the
	// phase's work on and names none of that work. The pass that then finds a child open holds
	// implementing with the work of the entry the implementer stands on, not the resume task, and
	// still takes the resume task back (the retirement is bounded by that entry), and the return
	// starts implementing on the entry's work.
	t.Run("a resume task delivered to the implementer at no entry, then a child opens: the held task and the return's task are the entry's work", func(t *testing.T) {
		pool := migratedPool(t)
		admission := pwAdmission(t, 2)
		token := pwMidPhase(t, pool, spParent, phase.Implementing, supervise.StateReady, "")
		pwApplied(t, pool, admission, "supervisor", "ready", intake.ClaimReady{Issue: spParent, Role: claim.RoleImplementer, Launch: 1})
		resume := pnWorkerStart(t, pool, spParent, claim.RoleImplementer)
		if !resume.ResumeTask || resume.Entry != 0 || !strings.Contains(resume.Task, "Resume the existing phase work.") {
			t.Fatalf("the catch-up's start = %+v, want the phase's resume task at no entry", resume)
		}
		pwDeliver(t, pool, spParent, token, resume)
		if _, err := pool.Exec(context.Background(), `update claims set state = $2 where token = $1`,
			string(token), string(supervise.StateWorking)); err != nil {
			t.Fatalf("record the implementer's claim in the turn the resume task started: %v", err)
		}
		seedMirrorChild(t, pool, spChild, "todo", spParent)

		spPass(t, pool, admission)

		wait := pwHeld(t, pool, spParent, phase.Implementing, "the admission pass stopped the implementer's implementing phase", spChild+" (todo)")
		if wait.Task != pwTask(spParent, phase.Implementing) {
			t.Fatalf("the held task = %q, want the work of the entry the implementer stands on, %q, not the resume task %q",
				wait.Task, pwTask(spParent, phase.Implementing), resume.Task)
		}
		want := []record.SuperviseRequest{{Op: "retire_task", Tree: spParent, Role: claim.RoleImplementer, Generation: 1,
			Phase: phase.Implementing, Entry: 3}}
		if retired := spRetired(t, pool, spParent); !reflect.DeepEqual(retired, want) {
			t.Fatalf("task retirements = %+v, want %+v: the move takes the resume task back, bounded by the entry", retired, want)
		}

		pnChildMerged(t, pool, spChild, spParent, 51)
		spPass(t, pool, admission)

		work := pwTask(spParent, phase.Implementing) + " " + pnMergedChildren(spParent, pnMerged(spChild, 51)) + "."
		if starts := pwWorkerStarts(t, pool, spParent); len(starts) != 1 || starts[0].role != claim.RoleImplementer || starts[0].task != work {
			t.Fatalf("starts of %s = %+v, want one implementer start on the entry's work %q", spParent, starts, work)
		}
	})

	// A task the implementer's phases row keeps as given for another entry, or in another
	// generation of the issue, is no task of the entry the row stands on in this run. Each case
	// seeds such a row beside the work its entry was raised with, and the same steps follow.
	for _, tc := range []struct {
		name string
		// seed records the parent with its implementer's claim in the middle of implementing,
		// given entry 3, and answers the claim's token, which the case's phases row is written
		// against.
		seed func(*testing.T, *pgxpool.Pool) claim.Token
		// given is the entry and the generation the row keeps its task as given for: an entry
		// before the one the row stands on, or the entry it stands on in a generation of the issue
		// before the one it runs now. The case's body writes the task on it.
		given record.GivenTask
		// entry names the entry whose work the hold keeps, and givenFor the entry, and the
		// generation where it differs, the seeded task was given for, as the failure tells them.
		entry, givenFor string
	}{
		// A task a retry gave the implementer for an entry before the one its phases row stands on
		// is no task of that entry. A rollback and a roll forward leave such a task on the row: the
		// earlier release raises the entry and names none of this release's columns of the task the
		// role was given, its entry and its generation, so they keep what the retry wrote
		// (record.PhaseRow.Given, record.GivenTask). The pass that then finds a child open holds
		// implementing with the work of the entry the row stands on, and the return starts
		// implementing on it.
		{name: "a task given for an entry before the one the row stands on, then a child opens: the held task and the return's task are the entry's work",
			given: record.GivenTask{Entry: 2, Generation: 1}, entry: "entry 3, which the row stands on", givenFor: "entry 2",
			seed: func(t *testing.T, pool *pgxpool.Pool) claim.Token {
				return pwMidPhase(t, pool, spParent, phase.Implementing, supervise.StateIdle, "")
			}},
		// A task the implementer was given for the entry its phases row stands on, in a generation
		// of the issue before the one it runs now, is no task of this run
		// (record.GivenTask.Generation): the release before names none of the given columns, so a
		// generation it gives the issue leaves them beside the entry it keeps. Written straight to
		// the store here, the row keeps the work its entry was raised with too, so the given task's
		// generation alone decides the hold: the pass that finds a child open holds implementing
		// with the entry's work, not the task of the earlier generation, and the return starts
		// implementing on it.
		{name: "a task given for the entry the row stands on in an earlier generation, then a child opens: the held task and the return's task are the entry's work",
			given: record.GivenTask{Entry: 3, Generation: 1}, entry: "entry 3 in generation 2", givenFor: "entry 3 in generation 1",
			seed: func(t *testing.T, pool *pgxpool.Pool) claim.Token {
				spSeedParent(t, pool, spParent, phase.Implementing, true, func(rerun *record.Issue) { rerun.Generation = 2 })
				pwPullRequest(t, pool, spParent, record.PullRequestOpen)
				token := putWorkerClaim(t, pool, spParent, spParent, claim.RoleImplementer, 2, supervise.StateIdle, "")
				markGivenEntry(t, pool, token, 3)
				return token
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := pwAdmission(t, 2)
			token := tc.seed(t, pool)
			given := tc.given
			given.Task = "Continue " + spParent + ". Issue: " + spParent + ". Phase: implementing. Reason: the runner is back."
			spSeedPhase(t, pool, record.PhaseRow{Issue: spParent, Role: claim.RoleImplementer, Claim: token, Entry: 3,
				EntryTask: pwTask(spParent, phase.Implementing), EntryRaisedAt: fixedNow.Add(-time.Hour), Given: given})
			seedMirrorChild(t, pool, spChild, "todo", spParent)

			spPass(t, pool, admission)

			wait := pwHeld(t, pool, spParent, phase.Implementing, "the admission pass stopped the implementer's implementing phase", spChild+" (todo)")
			if wait.Task != pwTask(spParent, phase.Implementing) {
				t.Fatalf("the held task = %q, want the work of %s, %q, not the task %q given for %s",
					wait.Task, tc.entry, pwTask(spParent, phase.Implementing), given.Task, tc.givenFor)
			}

			pnChildMerged(t, pool, spChild, spParent, 51)
			spPass(t, pool, admission)

			work := pwTask(spParent, phase.Implementing) + " " + pnMergedChildren(spParent, pnMerged(spChild, 51)) + "."
			if starts := pwWorkerStarts(t, pool, spParent); len(starts) != 1 || starts[0].role != claim.RoleImplementer || starts[0].task != work {
				t.Fatalf("starts of %s = %+v, want one implementer start on the entry's work %q", spParent, starts, work)
			}
		})
	}

	// A tester stopped in the middle of testing on a task that ends with the base-merge sentence,
	// as every testing task of a parent does (workflow's withBaseMergeRun), is held with no copy of
	// it, and the return of its held testing after its pull request merged during the wait ends the
	// task with the sentence once: the raise of the entry keeps the task the tester was given as it
	// was composed, before the start added the sentence (record.PhaseRow.Given), and the
	// hold is that task, so a held task keeps no sentence that a later build's return would add its
	// own beside.
	t.Run("a tester stopped on a task ending with the base-merge sentence is held without it, and the return adds it once", func(t *testing.T) {
		pool := migratedPool(t)
		admission := pwAdmission(t, 2)
		token := pwMidPhase(t, pool, spParent, phase.Testing, supervise.StateIdle, "")
		spSeedPhase(t, pool, record.PhaseRow{Issue: spParent, Role: claim.RoleTester, Claim: token, Entry: 3,
			EntryTask: pwTask(spParent, phase.Testing) + " " + pnBaseMerge, EntryRaisedAt: fixedNow.Add(-time.Hour),
			Given: record.GivenTask{Task: pwTask(spParent, phase.Testing), Entry: 3, Generation: 1}})
		seedMirrorChild(t, pool, spChild, "todo", spParent)

		spPass(t, pool, admission)

		wait := pwHeld(t, pool, spParent, phase.Testing, "the admission pass stopped the tester's testing phase", spChild+" (todo)")
		if wait.Task != pwTask(spParent, phase.Testing) {
			t.Fatalf("the held task = %q, want the tester's task with no base-merge sentence, %q", wait.Task, pwTask(spParent, phase.Testing))
		}

		pwApplied(t, pool, admission, "github", "merged", intake.PullRequestMerged{Repo: "acme/widgets", Number: 42, MergeSHA: "merge-42"})
		pnChildMerged(t, pool, spChild, spParent, 51)
		spPass(t, pool, admission)

		work := pwTask(spParent, phase.Testing) + " " + pnMergedChildren(spParent, pnMerged(spChild, 51)) + ". " + pnBaseMerge
		if starts := pwWorkerStarts(t, pool, spParent); len(starts) != 1 || starts[0].role != claim.RoleTester || starts[0].task != work {
			t.Fatalf("starts of %s = %+v, want one tester start on the held task, the merged child and the base-merge sentence once: %q", spParent, starts, work)
		}
	})

	// The third and the sixth parent: held after the claim of their phase's worker failed, each
	// stays held at the pass that finds its child, and its architect's retry moves it to
	// awaiting_children holding the retried phase, written in_progress where the record showed
	// another status.
	for _, held := range []struct {
		name string
		from phase.Phase
	}{
		{name: "a parent held after its implementer's claim failed stays held at the pass, and the retry moves it", from: phase.Implementing},
		{name: "a parent held from testing after its tester's claim failed stays held at the pass, and the retry moves it", from: phase.Testing},
	} {
		t.Run(held.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := pwAdmission(t, 2)
			spSeedParent(t, pool, spParent, held.from, true, nil)
			pwPullRequest(t, pool, spParent, record.PullRequestOpen)
			role := phase.RoleFor(held.from)
			pwApplied(t, pool, admission, "supervisor", "failed", intake.ClaimFailed{Issue: spParent, Role: role,
				Reason: "the launch budget ran out"})
			if got := issue(t, pool, spParent); got.Phase != phase.Held || got.Hold == nil || got.Hold.From != held.from {
				t.Fatalf("the parent after its %s's claim failed = phase %s, hold %+v; want held from %s", role, got.Phase, got.Hold, held.from)
			}
			seedMirrorChild(t, pool, spChild, "todo", spParent)

			spPass(t, pool, admission)

			if got := issue(t, pool, spParent); got.Phase != phase.Held || got.ChildrenWait != nil {
				t.Fatalf("the held parent after the pass = phase %s, held start %+v; want it held still", got.Phase, got.ChildrenWait)
			}
			if told := spNotices(t, pool, spParent); len(told) != 0 {
				t.Fatalf("awaiting-children notices = %+v, want none at the pass", told)
			}

			pwApplied(t, pool, admission, "api", "retry", intake.RetryOrEscalate{Issue: spParent, Decision: intake.RetryDecision,
				Reason: "the runner is back"})

			wait := pwHeld(t, pool, spParent, held.from, "the runner is back", spChild+" (todo)")
			if !strings.Contains(wait.Task, "Reason: the runner is back.") {
				t.Fatalf("the held task = %q, want the retry's task", wait.Task)
			}
			pwNotice(t, pool, held.from, held.from, "the runner is back", spChild+" (todo)")
			pwStatuses(t, pool, spParent, pwInProgressOver(workflow.PhaseStatus(held.from)))
		})
	}

	// A todo on a root held from testing releases its hold, which with an open child moves it the
	// same way, and writes in_progress over the todo Dispatch shows. The move itself queues the
	// write keyed on the testing the record still held (Engine.status), and the release queues the
	// one keyed on the todo (workflow's releaseHeldRoot): the executor finishes the first without a
	// write, since Dispatch shows todo, so the second is the one that writes.
	t.Run("a todo on a root held from testing moves it, written in_progress over the todo", func(t *testing.T) {
		pool := migratedPool(t)
		admission := pwAdmission(t, 2)
		spSeedParent(t, pool, spParent, phase.Held, true, func(issue *record.Issue) {
			issue.Hold, issue.Status, issue.DispatchStatus, issue.LastDispatchSeq = &record.Hold{From: phase.Testing}, "testing", "testing", 4
		})
		pwPullRequest(t, pool, spParent, record.PullRequestOpen)
		seedMirrorChild(t, pool, spChild, "todo", spParent)
		spPass(t, pool, admission)
		if got := issue(t, pool, spParent); got.Phase != phase.Held {
			t.Fatalf("the held root after the pass = %s, want held still", got.Phase)
		}

		pwApplied(t, pool, admission, "dispatch", "todo", intake.DispatchIssue{Key: spParent, ResolvedRepo: "acme/widgets", Seq: 5,
			Type: "issue.updated", Status: "todo", Title: spParent, Rank: spParent, HandedOver: true})

		got := issue(t, pool, spParent)
		if got.Phase != phase.AwaitingChildren || got.ChildrenWait == nil || got.ChildrenWait.Start != phase.Testing ||
			!strings.HasSuffix(got.ChildrenWait.Reason, ", while these Dispatch children of "+spParent+" were open: "+spChild+" (todo)") {
			t.Fatalf("the released root = phase %s, held start %+v; want awaiting_children holding testing", got.Phase, got.ChildrenWait)
		}
		if told := spNotices(t, pool, spParent); len(told) != 1 {
			t.Fatalf("awaiting-children notices = %+v, want one", told)
		}
		pwStatuses(t, pool, spParent, append(pwInProgressOver("testing"), pwInProgressOver("todo")...))
	})

	// The late completion: a completion of the phase the wait stopped is refused while the parent
	// waits (HANDOFF_NOT_CURRENT_PHASE, the engine's: the tester's entry was raised before the
	// parent's move into awaiting_children, record.Issue.ChildrenWaitedAt). The return takes the
	// held testing back as testing, where the engine reads the stopped tester's completion as one of
	// the current phase, and the fact carries no entry. What refuses it after the return is the
	// completion route's entry fence (api's entryFence, behindEntryClause): the return's start is on
	// an entry of its own, raised as a move back out of awaiting_children
	// (record.PhaseRow.EntryMovedBack), so the tester's claim, which still serves the stopped entry
	// (supervise's RetireStoppedTask leaves its ServedEntry), reports a completion behind an entry a
	// move back raised, which the fence refuses HANDOFF_STALE_ENTRY whatever head it names
	// (internal/api's TestParentWaitsHoldsEveryPhaseStart drives it through the route). Here the
	// return's entry is read as the mark the fence reads.
	t.Run("a late completion of the stopped phase is refused during the wait, and the return raises the entry the route's fence refuses it behind", func(t *testing.T) {
		pool, admission, token := pwStopTester(t)
		stopped := spPhaseRow(t, pool, spParent, claim.RoleTester)

		pwRefused(t, pwApply(t, pool, admission, "api", "late-during", pwLate(spParent, claim.RoleTester, token, "test-late")),
			"HANDOFF_NOT_CURRENT_PHASE", "the tester's completion during the wait")
		if row := spPhaseRow(t, pool, spParent, claim.RoleTester); row.HandoffRecord == "test-late" || !row.CompletedAt.IsZero() {
			t.Fatalf("the tester's row = %+v, want the late completion unrecorded", row)
		}

		pnChildMerged(t, pool, spChild, spParent, 51)
		spPass(t, pool, admission)
		if back := issue(t, pool, spParent); back.Phase != phase.Testing {
			t.Fatalf("the parent after its child is done = %s, want testing", back.Phase)
		}
		if row := spPhaseRow(t, pool, spParent, claim.RoleTester); row.Entry != stopped.Entry+1 || !row.EntryMovedBack ||
			row.EntryFrom != phase.AwaitingChildren {
			t.Fatalf("the tester's row after the return = %+v, want the entry above the stopped one (%d) raised as a move back out of awaiting_children, which the route's entry fence refuses a completion behind",
				row, stopped.Entry)
		}
	})

	// The relaunch: the move that stops a worker mid-phase writes,
	// in its own transaction, the `retire_task` row that retires the task its claim holds for the
	// stopped phase, bounded by the run and by the entry the phase stood on, so the return's start
	// on an entry of its own is never taken; and the entry sweep starts no role of the waiting
	// parent. The row runs through supervise.Machine.RetireStoppedTask in the daemon's outbox,
	// which the admission package cannot drive: the daemon's
	// TestTheMovesRetirementLeavesNoTaskOfTheStoppedPhase starts the worker on the phase, runs this
	// row, brings the worker up during the wait and after the return, and reads that it is sent no
	// task of the stopped phase and only the return's own start.
	t.Run("a relaunch during the wait is given no task of the stopped phase", func(t *testing.T) {
		pool := migratedPool(t)
		admission := pwAdmission(t, 2)
		pwMidPhase(t, pool, spParent, phase.Implementing, supervise.StateIdle, "")
		seedMirrorChild(t, pool, spChild, "todo", spParent)

		spPass(t, pool, admission)

		want := []record.SuperviseRequest{{Op: "retire_task", Tree: spParent, Role: claim.RoleImplementer, Generation: 1,
			Phase: phase.Implementing, Entry: 3}}
		if retired := spRetired(t, pool, spParent); !reflect.DeepEqual(retired, want) {
			t.Fatalf("task retirements = %+v, want %+v", retired, want)
		}
		applySweep(t, pool, admission, "sweep")
		if starts := pwWorkerStarts(t, pool, spParent); len(starts) != 0 {
			t.Fatalf("worker starts of %s after the entry sweep = %+v, want none during the wait", spParent, starts)
		}
	})

	// The starts that are not held. A parent whose children are done, in icebox, or on the
	// backlog with a record that left the workflow starts the phase as today: the tester's fail
	// starts the implementer, the phase-finished notice goes out, and the deployment's hook fires
	// on the move as it does with no child open.
	for _, closed := range []struct {
		name string
		seed func(*testing.T, *pgxpool.Pool)
	}{
		{name: "a parent whose child is done starts the phase", seed: func(t *testing.T, pool *pgxpool.Pool) {
			seedMirrorChild(t, pool, spChild, "done", spParent)
		}},
		{name: "a parent whose child is in icebox starts the phase", seed: func(t *testing.T, pool *pgxpool.Pool) {
			seedMirrorChild(t, pool, spChild, "icebox", spParent)
		}},
		{name: "a parent whose child is on the backlog with a record that left the workflow starts the phase", seed: func(t *testing.T, pool *pgxpool.Pool) {
			putIssue(t, pool, record.Issue{Key: spChild, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Project: testProject, Title: spChild, Tree: spParent, Parent: &parent, Phase: phase.Done, Generation: 1,
				Status: "backlog", DispatchStatus: "backlog", Rank: spChild, HandedOver: true, LastDispatchSeq: 1})
			seedMirrorChild(t, pool, spChild, "backlog", spParent)
		}},
	} {
		t.Run(closed.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := pwAdmission(t, 2)
			spSeedParent(t, pool, spParent, phase.Testing, true, nil)
			pwPullRequest(t, pool, spParent, record.PullRequestOpen)
			closed.seed(t, pool)

			pwApplied(t, pool, admission, "api", "failed", pwStatesFirst(t, pool, admission, intake.HandoffComplete{Generation: 1, Issue: spParent,
				Role: claim.RoleTester, Claim: "tester", Summary: "a check failed", Commit: pwHead, HandoffRecord: "test-1", HandoffVersion: 1}, "fail"))

			if got := issue(t, pool, spParent); got.Phase != phase.Implementing || got.ChildrenWait != nil {
				t.Fatalf("the parent = phase %s, held start %+v; want implementing, nothing held", got.Phase, got.ChildrenWait)
			}
			if starts := pwWorkerStarts(t, pool, spParent); len(starts) != 1 || starts[0].role != claim.RoleImplementer {
				t.Fatalf("worker starts = %+v, want the implementer's", starts)
			}
			if kinds := pwNoticeKinds(t, pool, spParent); !maps.Equal(kinds, map[record.NoticeKind]int{"phase-finished": 1}) {
				t.Fatalf("notices by kind = %v, want the phase-finished notice alone", kinds)
			}
			if prompts, posts := pwPayloads[record.HookPrompt](t, pool, spParent), pwPayloads[record.MessagePost](t, pool, spParent); len(prompts) != 1 || len(posts) != 1 {
				t.Fatalf("hook prompts = %+v, messages = %+v; want the hook's one prompt and its firing message", prompts, posts)
			}
		})
	}

	// A parent whose pull request merged after its child opened and before the start: the pass the
	// merge runs leaves it in testing and sends its tester nothing, and the tester's pass then
	// starts reviewing.
	t.Run("a parent whose pull request merged after its child opened starts the phase, its worker sent nothing", func(t *testing.T) {
		pool := migratedPool(t)
		admission := pwAdmission(t, 2)
		token := pwMidPhase(t, pool, spParent, phase.Testing, supervise.StateIdle, "")
		seedMirrorChild(t, pool, spChild, "todo", spParent)

		pwApplied(t, pool, admission, "github", "merged", intake.PullRequestMerged{Repo: "acme/widgets", Number: 42, MergeSHA: "merge-42"})

		if got := issue(t, pool, spParent); got.Phase != phase.Testing || got.ChildrenWait != nil {
			t.Fatalf("the parent after the merge = phase %s, held start %+v; want testing", got.Phase, got.ChildrenWait)
		}
		if ended := pwPayloads[record.PhaseEnded](t, pool, spParent); len(ended) != 0 {
			t.Fatalf("phase-ended messages = %+v, want none: the tester of a merged parent is sent nothing", ended)
		}

		pwApplied(t, pool, admission, "api", "passed", pwStatesFirst(t, pool, admission, intake.HandoffComplete{Generation: 1, Issue: spParent,
			Role: claim.RoleTester, Claim: token, Summary: "every check passed", Commit: pwHead, HandoffRecord: "test-1", HandoffVersion: 1}, "pass"))

		if got := issue(t, pool, spParent); got.Phase != phase.Reviewing || got.ChildrenWait != nil {
			t.Fatalf("the parent = phase %s, held start %+v; want reviewing, nothing held", got.Phase, got.ChildrenWait)
		}
		if starts := pwWorkerStarts(t, pool, spParent); len(starts) != 1 || starts[0].role != claim.RoleReviewer {
			t.Fatalf("worker starts = %+v, want the reviewer's", starts)
		}
		if told, ended := spNotices(t, pool, spParent), pwPayloads[record.PhaseEnded](t, pool, spParent); len(told) != 0 || len(ended) != 0 {
			t.Fatalf("awaiting-children notices = %+v, phase-ended messages = %+v; want none", told, ended)
		}
	})

	// A parent in accepting whose pull request merged, as INFRA-14's did, does not move at the pass.
	t.Run("a parent in accepting whose pull request merged does not move at the pass", func(t *testing.T) {
		pool := migratedPool(t)
		admission := pwAdmission(t, 2)
		spSeedParent(t, pool, spParent, phase.Accepting, false, nil)
		pwPullRequest(t, pool, spParent, record.PullRequestMerged)
		seedMirrorChild(t, pool, spChild, "todo", spParent)

		spPass(t, pool, admission)

		if got := issue(t, pool, spParent); got.Phase != phase.Accepting || got.ChildrenWait != nil {
			t.Fatalf("the parent = phase %s, held start %+v; want accepting, nothing held", got.Phase, got.ChildrenWait)
		}
		if told := spNotices(t, pool, spParent); len(told) != 0 {
			t.Fatalf("awaiting-children notices = %+v, want none", told)
		}
		pwStatuses(t, pool, spParent, nil)
	})

	// A root handed over from another tree, carrying the slot it held there in implementing, waits
	// in the line for its tree to be opened (record.QueuedRoot). The pass leaves it where it stands
	// and its promotion opens its tree on the slot it carries and starts the phase it carries.
	t.Run("a root handed over carrying its slot in implementing starts the phase it carries", func(t *testing.T) {
		pool := migratedPool(t)
		admission := pwAdmission(t, 2)
		carriedRoot(t, pool, spParent, phase.Implementing, "A")
		takeSlot(t, pool, spParent)
		seedMirrorChild(t, pool, spChild, "todo", spParent)

		spPass(t, pool, admission)

		got := issue(t, pool, spParent)
		if got.Phase != phase.Implementing || got.ChildrenWait != nil || got.HandedOverFrom != "" || !treeOpen(t, pool, spParent) {
			t.Fatalf("the carried root = phase %s, held start %+v, handed over from %q, tree open %t; want implementing in a tree of its own, nothing held",
				got.Phase, got.ChildrenWait, got.HandedOverFrom, treeOpen(t, pool, spParent))
		}
		if starts := pwWorkerStarts(t, pool, spParent); len(starts) != 1 || starts[0].role != claim.RoleImplementer {
			t.Fatalf("worker starts = %+v, want the implementer's start of the phase it carries", starts)
		}
		if !holdsSlot(t, pool, spParent) {
			t.Fatalf("slots = %+v, want the one it carried", slots(t, pool))
		}
		if told := spNotices(t, pool, spParent); len(told) != 0 {
			t.Fatalf("awaiting-children notices = %+v, want none", told)
		}
	})

	// The carried-slot exemption is specific to implementing. Other carried worker phases
	// wait at the promotion itself if a child is open.
	for _, carries := range []phase.Phase{phase.Testing, phase.Planning, phase.Reviewing, phase.Implementing} {
		t.Run("a root handed over carrying its slot in "+string(carries)+" starts with no open child", func(t *testing.T) {
			pool := migratedPool(t)
			admission := pwAdmission(t, 2)
			carriedRoot(t, pool, spParent, carries, "A")
			takeSlot(t, pool, spParent)

			spPass(t, pool, admission)

			if got := issue(t, pool, spParent); got.Phase != carries || got.ChildrenWait != nil || got.HandedOverFrom != "" || !treeOpen(t, pool, spParent) {
				t.Fatalf("the promoted root = phase %s, held start %+v, handed over from %q; want %s in a tree of its own, nothing held",
					got.Phase, got.ChildrenWait, got.HandedOverFrom, carries)
			}
			if starts := pwWorkerStarts(t, pool, spParent); len(starts) != 1 || starts[0].role != phase.RoleFor(carries) || starts[0].task == "" {
				t.Fatalf("worker starts = %+v, want the %s's start of the phase the root carries", starts, phase.RoleFor(carries))
			}
		})
		if carries == phase.Implementing {
			continue // The open-child row above covers the exemption.
		}
		t.Run("a root handed over carrying its slot in "+string(carries)+" waits at promotion for its open child", func(t *testing.T) {
			pool := migratedPool(t)
			admission := pwAdmission(t, 2)
			carriedRoot(t, pool, spParent, carries, "A")
			takeSlot(t, pool, spParent)
			seedMirrorChild(t, pool, spChild, "todo", spParent)

			spPass(t, pool, admission)

			got := issue(t, pool, spParent)
			if got.Phase != phase.AwaitingChildren || got.ChildrenWait == nil || got.ChildrenWait.Start != carries {
				t.Fatalf("the promoted root = phase %s, held start %+v; want awaiting_children holding %s",
					got.Phase, got.ChildrenWait, carries)
			}
			if !reflect.DeepEqual(got.ChildrenWait.Children, []string{spChild}) || got.HandedOverFrom != "" || !treeOpen(t, pool, spParent) {
				t.Fatalf("the promoted root's wait names %v, handed over from %q; want %s alone and a tree of its own",
					got.ChildrenWait.Children, got.HandedOverFrom, spChild)
			}
			noWorkerLaunch(t, pool, spParent, phase.RoleFor(carries))
		})
	}

	// A parent in retro, with a child filed and not released, that the implementer moves back to
	// implementing starts implementing as today: the daemon has accepted it, so the rule does not
	// reach that start. k12 names this case as a head that then conflicts with its base, which
	// moves no phase since cybermonklabs/legion#4464; the implementer's own move back out of retro
	// is the move from retro that stays.
	t.Run("a parent in retro with a child filed and not released that the implementer moves back starts implementing", func(t *testing.T) {
		pool := migratedPool(t)
		admission := pwAdmission(t, 2)
		spSeedParent(t, pool, spParent, phase.Retro, false, nil)
		pwPullRequest(t, pool, spParent, record.PullRequestOpen)
		seedMirrorChild(t, pool, spChild, "backlog", spParent)

		pwApplied(t, pool, admission, "api", "back", intake.BackwardMove{Issue: spParent, Requester: claim.RoleImplementer,
			To: phase.Implementing, Reason: pwBackFromRetro})

		if got := issue(t, pool, spParent); got.Phase != phase.Implementing || got.ChildrenWait != nil {
			t.Fatalf("the parent = phase %s, held start %+v; want implementing, nothing held", got.Phase, got.ChildrenWait)
		}
		if starts := pwWorkerStarts(t, pool, spParent); len(starts) != 1 || starts[0].role != claim.RoleImplementer {
			t.Fatalf("worker starts = %+v, want the implementer's", starts)
		}
		if kinds := pwNoticeKinds(t, pool, spParent); !maps.Equal(kinds, map[record.NoticeKind]int{"phase-finished": 1}) {
			t.Fatalf("notices by kind = %v, want the phase-finished notice alone", kinds)
		}
		if ended := pwPayloads[record.PhaseEnded](t, pool, spParent); len(ended) != 0 {
			t.Fatalf("phase-ended messages = %+v, want none: the implementer ended its own retro", ended)
		}
	})

	// The exemption is the entry the move out of retro raised and no more: the implementer's
	// row stands on an entry raised from retro (record.PhaseRow.EntryFrom), so a later pass leaves
	// the parent where it stands too, and its next start, the implementer's completion into
	// testing, is a start from a phase before retro, which the open child holds.
	t.Run("after a move back out of retro the parent's next start, into testing, is held while its child is open", func(t *testing.T) {
		pool := migratedPool(t)
		admission := pwAdmission(t, 2)
		spSeedParent(t, pool, spParent, phase.Retro, false, nil)
		pwPullRequest(t, pool, spParent, record.PullRequestOpen)
		seedMirrorChild(t, pool, spChild, "backlog", spParent)
		pwApplied(t, pool, admission, "api", "back", intake.BackwardMove{Issue: spParent, Requester: claim.RoleImplementer,
			To: phase.Implementing, Reason: pwBackFromRetro})
		if row := spPhaseRow(t, pool, spParent, claim.RoleImplementer); row.EntryFrom != phase.Retro || !row.EntryMovedBack {
			t.Fatalf("the implementer's phases row = %+v, want its entry raised by the move back out of retro", row)
		}
		// The lane delivers the implementer's start, and a pass with its worker in the middle of
		// the phase leaves the parent where it stands.
		runStart(t, pool, spParent)
		spPass(t, pool, admission)
		if got := issue(t, pool, spParent); got.Phase != phase.Implementing || got.ChildrenWait != nil {
			t.Fatalf("the parent after a later pass = phase %s, held start %+v; want implementing, nothing held", got.Phase, got.ChildrenWait)
		}

		pwApplied(t, pool, admission, "api", "implemented", intake.HandoffComplete{Generation: 1, Issue: spParent,
			Role: claim.RoleImplementer, Claim: "implementer", Summary: "merged main forward", Commit: pwHead,
			HandoffRecord: "implement-2", HandoffVersion: 1})

		const cause = "the implementer completed implementing"
		pwHeld(t, pool, spParent, phase.Testing, cause, spChild+" (backlog)")
		pwNotice(t, pool, phase.Implementing, phase.Testing, cause, spChild+" (backlog)")
	})

	// A parent held on that entry is retried on it: the release of a hold raises no entry
	// (workflow's restart), so the implementer's row still stands on the entry the move out of
	// retro raised, and the architect's retry starts the phase again rather than moving the parent
	// to awaiting_children, as the pass leaves it (workflow's acceptedEntry).
	t.Run("a parent held on an entry raised out of retro is retried into its phase whatever its child", func(t *testing.T) {
		pool := migratedPool(t)
		admission := pwAdmission(t, 2)
		spSeedParent(t, pool, spParent, phase.Retro, false, nil)
		pwPullRequest(t, pool, spParent, record.PullRequestOpen)
		seedMirrorChild(t, pool, spChild, "backlog", spParent)
		pwApplied(t, pool, admission, "api", "back", intake.BackwardMove{Issue: spParent, Requester: claim.RoleImplementer,
			To: phase.Implementing, Reason: pwBackFromRetro})
		runStart(t, pool, spParent)
		pwApplied(t, pool, admission, "supervisor", "failed", intake.ClaimFailed{Issue: spParent, Role: claim.RoleImplementer,
			Reason: "the launch budget ran out"})
		if got := issue(t, pool, spParent); got.Phase != phase.Held || got.Hold == nil || got.Hold.From != phase.Implementing {
			t.Fatalf("the parent after its implementer's claim failed = phase %s, hold %+v; want held from implementing", got.Phase, got.Hold)
		}

		pwApplied(t, pool, admission, "api", "retry", intake.RetryOrEscalate{Issue: spParent, Decision: intake.RetryDecision,
			Reason: "the runner is back"})

		if got := issue(t, pool, spParent); got.Phase != phase.Implementing || got.Hold != nil || got.ChildrenWait != nil {
			t.Fatalf("the retried parent = phase %s, hold %+v, held start %+v; want implementing, nothing held", got.Phase, got.Hold, got.ChildrenWait)
		}
		starts := pwWorkerStarts(t, pool, spParent)
		if len(starts) != 1 || starts[0].role != claim.RoleImplementer || !strings.Contains(starts[0].task, "Reason: the runner is back.") {
			t.Fatalf("worker starts = %+v, want the implementer's retry", starts)
		}
		if row := spPhaseRow(t, pool, spParent, claim.RoleImplementer); row.EntryFrom != phase.Retro {
			t.Fatalf("the implementer's phases row = %+v, want it still on the entry raised out of retro", row)
		}
		if told := spNotices(t, pool, spParent); len(told) != 0 {
			t.Fatalf("awaiting-children notices = %+v, want none", told)
		}
	})
}
