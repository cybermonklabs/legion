package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// changeRequestOfGate is what an artifact event of key carries of key's change request, read
// before the event is applied, since the workflow performs no I/O, when key's spec gate is under
// the rule of a change request (R23, R25), and nil for every other gate: for a version of the
// gate's own document, the read at that version; for an approval of that document, and for a
// version or an approval of any other document of the issue, the read at the gate's current
// version. The request reads its checks from the issue's Verification document, and an event names
// no document's role, so every other document's event is read, and the read's Verification
// version says whether that document moved (the workflow's verificationRead). A changes request
// reads nothing: it closes the gate whatever the request holds. Another event of the gate's own
// document — a review of an older version — reads at the gate's version too, and the workflow
// takes nothing from a read of a version it does not apply.
//
// The read is nil too for a Dispatch older than the route (dispatch.NoRoute), and for a read
// Dispatch refuses for good (dispatch.PermanentRefusal: a version whose text does not parse,
// CHANGE_REQUEST_UNREADABLE, or one Dispatch does not hold, VERSION_NOT_FOUND), which is logged:
// no later read of that version answers otherwise, so the version closes the gate as a version
// always has, rather than wait unapplied while the gate stays open at an older version. Any other
// read Dispatch fails is an error, so the caller reads again rather than apply the event unread.
// The live event's preparation, the gate seed and the boot catch-up all read through it, so every
// path that applies an event of such an issue reads it the same way.
func changeRequestOfGate(ctx context.Context, log *slog.Logger, pool *store.Pool, records record.Store, client dispatch.Client,
	fact intake.DispatchArtifact) (*intake.ChangeRequestRead, error) {
	if fact.Kind == intake.DispatchArtifactChangesRequested {
		return nil, nil
	}
	key := fact.Key
	var gate *record.DesignGate
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		gates, err := records.GatesOf(ctx, tx, key)
		gate = record.SpecGate(gates)
		return err
	}); err != nil {
		return nil, fmt.Errorf("read the spec gate of %s: %w", key, err)
	}
	if gate == nil || gate.Rule != record.GateRuleChangeRequest {
		return nil, nil
	}
	version := gate.LatestVersion
	if fact.ArtifactID == gate.ArtifactID && fact.Kind == intake.DispatchArtifactVersion {
		version = fact.Version
	}
	read, err := client.ChangeRequest(ctx, key, version)
	if dispatch.NoRoute(err) {
		return nil, nil
	}
	if refusal, permanent := dispatch.PermanentRefusal(err); permanent {
		log.Warn("the change request of a version is refused by Dispatch for good; the event is applied unread",
			"issue", key, "artifact", fact.ArtifactID, "event", fact.Kind, "version", version, "status", refusal.Status, "code", refusal.Code, "error", err)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the change request of %s at version %d from Dispatch: %w", key, version, err)
	}
	return intake.ChangeRequestReadOf(read), nil
}

// endingSubject is what a read of the ending condition of key's change request is held to: the
// issue as the record holds it (nil when it holds none), its pull request's head ("" for none),
// and whether its tree's spec gate stands open by exemption (record.GateExempt), all read in one
// transaction just before the read acts, as every fence of a row is.
func (r *outbox) endingSubject(ctx context.Context, key string) (*record.Issue, string, bool, error) {
	var issue *record.Issue
	var head string
	var exempt bool
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		if issue, err = r.records.Issue(ctx, tx, key); err != nil || issue == nil {
			return err
		}
		pr, err := r.records.PullRequest(ctx, tx, key)
		if err != nil {
			return err
		}
		if pr != nil {
			head = pr.HeadSHA
		}
		gates, err := r.records.GatesOf(ctx, tx, issue.Tree)
		if err != nil {
			return err
		}
		exempt = record.SpecGateExempt(gates)
		return nil
	}); err != nil {
		return nil, "", false, fmt.Errorf("read the ending subject of %s: %w", key, err)
	}
	return issue, head, exempt, nil
}

// applyEnding applies what Dispatch answered of the ending condition of issue's change request at
// head (intake.SpecEndingRead), under eventID, through the workflow, which records it and tells
// whom it concerns. start marks the merger's start fence's own reading, which tells the merger
// nothing: the task its start gives names the ending step (startTask).
func (r *outbox) applyEnding(ctx context.Context, eventID, issue, head string, ending dispatch.SpecEnding, cause record.SpecEndingCause, start bool) error {
	fact := intake.SpecEndingRead{Issue: issue, Head: head, State: ending.State, Reasons: ending.Reasons, Version: ending.Version, Cause: cause, Start: start}
	if _, err := intake.ApplyFact(ctx, r.pool, "outbox", eventID, fact, r.handlers...); err != nil {
		return fmt.Errorf("apply the ending condition of %s at %s: %w", issue, head, err)
	}
	return nil
}

// specEndingRead runs one read of the ending condition (record.SpecEndingRead, D10): it reads
// Dispatch once at the row's head and applies the answer, under an event id that names the row, so
// a row run again after its finish was lost applies nothing twice. It first finishes the row
// without reading, with one line naming why, once the read would tell nobody anything: the issue
// is not recorded or not in merging, its pull request's head moved past the row (the move queued a
// read of its own), or its tree's spec gate stands open by exemption no more (a closed gate holds
// the merger itself). A row queued because the gate turned open by exemption
// (record.SpecEndingCauseExempted) is the read that tells a merger already running the ending
// step, and the move's own read may find the reading unchanged and tell it nothing, so such a row
// whose head moved reads the pull request's current head instead. A Dispatch older than the route
// finishes the row too, since it will not answer this read: the merger's start fence and the
// merger's own reading before each merge still hold the merge. A read Dispatch fails is retried on
// the row's backoff.
func (r *outbox) specEndingRead(ctx context.Context, row record.OutboxRow, payload record.SpecEndingRead) error {
	if r.dispatch == nil {
		return fmt.Errorf("spec ending read executor has no Dispatch client")
	}
	issue, head, exempt, err := r.endingSubject(ctx, row.Issue)
	if err != nil {
		return err
	}
	at := payload.Head
	if payload.Cause == record.SpecEndingCauseExempted && head != "" {
		at = head
	}
	attributes := []any{"row", row.ID, "issue", row.Issue, "head", payload.Head, "at", at, "cause", payload.Cause}
	var why string
	switch {
	case issue == nil:
		why = "the issue is not recorded"
	case issue.Phase != phase.Merging:
		why = fmt.Sprintf("the issue stands in %s, not merging", issue.Phase)
	case head != at:
		why = "the pull request's head moved past the row"
	case !exempt:
		why = "the tree's spec gate stands open by exemption no more"
	}
	if why != "" {
		r.log.Info("outbox spec ending read finished without reading", append(attributes, "why", why)...)
		return nil
	}
	ending, err := r.dispatch.SpecEnding(ctx, issue.Tree, at)
	if dispatch.NoRoute(err) {
		r.log.Warn("outbox spec ending read finished without reading: Dispatch has no route for the ending condition", attributes...)
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the ending condition of %s at %s from Dispatch: %w", issue.Tree, at, err)
	}
	r.log.Info("outbox spec ending read", append(attributes, "state", ending.State, "reasons", len(ending.Reasons))...)
	return r.applyEnding(ctx, fmt.Sprintf("spec-ending-read:%d", row.ID), issue.Key, at, ending, payload.Cause, false)
}

// mergerWaitsForTheEnding is the merger's start fence of a tree whose spec gate stands open by
// exemption (D10): it reads the ending condition of the tree root's change request at the merging
// issue's pull request head, applies the answer, so the reading is recorded and the root's
// architect told each reason, and answers a wait while the answer waits. A start of any other
// role, and a tree whose gate is open some other way, read nothing. A merging issue whose record
// holds no head waits for one, and so does a Dispatch older than the route: the gate opened by a
// read of that route's release, and nothing may land unjudged. The fact's event id names the row
// and its attempt, since each attempt is a new read. Its reading tells the merger nothing, since
// the merger is not running yet and the task its start gives names the ending step (startTask).
func (r *outbox) mergerWaitsForTheEnding(ctx context.Context, row record.OutboxRow, issue record.Issue, role claim.Role) error {
	if role != claim.RoleMerger {
		return nil
	}
	recorded, head, exempt, err := r.endingSubject(ctx, issue.Key)
	if err != nil || recorded == nil || !exempt {
		return err
	}
	if head == "" {
		return wait.Errorf("the merger of %s waits for a pull request head to read the ending condition of %s's change request at", issue.Key, issue.Tree)
	}
	if r.dispatch == nil {
		return fmt.Errorf("the merger's start fence has no Dispatch client to read the ending condition with")
	}
	ending, err := r.dispatch.SpecEnding(ctx, issue.Tree, head)
	if dispatch.NoRoute(err) {
		return wait.Errorf("the merger of %s waits for a Dispatch that answers the ending condition of %s's change request", issue.Key, issue.Tree)
	}
	if err != nil {
		return fmt.Errorf("read the ending condition of %s at %s from Dispatch: %w", issue.Tree, head, err)
	}
	if err := r.applyEnding(ctx, fmt.Sprintf("spec-ending-fence:%d:%d", row.ID, row.Attempts), issue.Key, head, ending, record.SpecEndingCauseHead, true); err != nil {
		return err
	}
	if ending.Ready() {
		return nil
	}
	codes := make([]string, 0, len(ending.Reasons))
	for _, reason := range ending.Reasons {
		codes = append(codes, reason.Code)
	}
	return wait.Errorf("the merger of %s waits for the ending condition of %s's change request to hold at head %s: %s",
		issue.Key, issue.Tree, head, strings.Join(codes, ", "))
}

// startTask is the task a start row gives its role: the row's own (record.SuperviseRequest.Task),
// and for a merger's start of an issue in merging whose tree's spec gate stands open by exemption
// as the start runs, that task with the paragraph of the ending step at its end
// (workflow.MergerExemptionParagraph, D10). The paragraph is added here, where the start runs,
// and not where the task was written, since a task can be written before the gate opened with no
// person: the resume task a re-admission writes while the re-admitted tree holds no gate (admit's
// startMidPhaseMembers), or a transition's task while the gate is closed, whose start the gate
// holds until it opens. So every merger that starts under such a gate is given the step, and the
// start fence's own reading of the ending condition tells it nothing beside it.
func (r *outbox) startTask(ctx context.Context, issue record.Issue, payload record.SuperviseRequest) (string, error) {
	if payload.Role != claim.RoleMerger || payload.Task == "" || issue.Phase != phase.Merging {
		return payload.Task, nil
	}
	_, _, exempt, err := r.endingSubject(ctx, issue.Key)
	if err != nil || !exempt {
		return payload.Task, err
	}
	return payload.Task + " " + workflow.MergerExemptionParagraph(issue.Tree), nil
}

// endingHoldsMerger says whether the recorded ending condition holds issue's released merger: its
// tree's spec gate stands open by exemption and the reading the record holds waits at the issue's
// pull request's current head (record.SpecEnding.WaitsAt). It reads the record alone, never
// Dispatch: the reading is the start fence's or the re-read's, and a head with no reading yet holds
// nothing here, since its read is queued and the start fence reads it anyway.
func (r *outbox) endingHoldsMerger(ctx context.Context, issue record.Issue) (bool, error) {
	var held bool
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		gates, err := r.records.GatesOf(ctx, tx, issue.Tree)
		if err != nil || !record.SpecGateExempt(gates) {
			return err
		}
		pr, err := r.records.PullRequest(ctx, tx, issue.Key)
		if err != nil || pr == nil {
			return err
		}
		ending, err := r.records.SpecEnding(ctx, tx, issue.Key)
		held = ending.WaitsAt(pr.HeadSHA)
		return err
	}); err != nil {
		return false, fmt.Errorf("read the ending condition of %s: %w", issue.Key, err)
	}
	return held, nil
}

// staleRequestSource is the processed-event source of the controller's copies of a
// spec-request-stale notice nobody of its tree hears. Its event id is the dedupe key the
// undelivered notice went under (record.Notice.Published), so one notice writes one copy however
// many rows and runs carry it.
const staleRequestSource = "spec-request-stale"

// staleRequestToController sends the controller a spec-request-stale notice whose architect will
// not hold its role again, or whose issue the record cannot route (D8): the controller names an
// owner who writes the request again. It runs in a transaction of its own that marks key
// processed, so a second run or a re-held copy queues no second notice, and owns the commit hook
// the enqueue registers, so the controller's lane wakes as it commits.
func (r *outbox) staleRequestToController(ctx context.Context, issue string, notice record.Notice, key string, row int64) error {
	queued, err := record.NewOutboxRow(issue, workflow.StaleRequestForController(issue, notice), r.now().UTC())
	if err != nil {
		return err
	}
	fresh := false
	scoped, committed := intake.WithCommitHooks(ctx)
	if err := pgx.BeginFunc(scoped, r.pool, func(tx pgx.Tx) error {
		marked, err := r.records.MarkProcessed(scoped, tx, staleRequestSource, key)
		if err != nil || !marked {
			return err
		}
		fresh = true
		return r.records.Enqueue(scoped, tx, queued)
	}); err != nil {
		return fmt.Errorf("send the controller the stale request %s carried on %s: %w", key, issue, err)
	}
	committed()
	if fresh {
		r.log.Info("outbox stale change request sent to the controller: its architect will not hold its role", "row", row, "issue", issue, "key", key)
	} else {
		r.log.Info("outbox stale change request handled before under this key", "row", row, "issue", issue, "key", key)
	}
	return nil
}
