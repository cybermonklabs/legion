package record

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// Fence is the run and the phase entry of one issue that a queued effect is for, and the pod it
// acts on (R17 and R20 of dispatch://INFRA-3320; decision 10 of dispatch://INFRA-4730): every
// effect the daemon queues for an issue names the fence the issue stood at, and its executor
// checks it against the fence the issue stands at where the effect commits (LateAt), so a late
// effect that acts does nothing in a later run or at a later phase entry, a late notice tells
// nobody of a later run (the architect's ruling of 2026-11-21 on dispatch://INFRA-3320), and a
// cleanup does nothing to a pod the issue no longer runs.
//
// Generation is the issue's run (Issue.Generation; 1 for a record written with none, the first
// run every writer of this release starts). Entry is the issue's phase entry: one more than the
// phase entries its roles have been handed in all (PhaseRow.Entry, summed over its phases rows),
// so it is 1 before any phase entry and rises by one with each one a start raises — a move into a
// phase, a return to work (R20 raises it as a move back does), a promotion, an architect's new
// work — and never falls, since no phase entry is ever reset. PodUID is the pod the issue's claims
// run in (Store.IssuePodUID), "" where they record none: a tmux claim's, or one with no locator.
//
// An issue the daemon records nothing of stands at FirstFence, the fence its first record will
// give it.
type Fence struct {
	Generation uint64
	Entry      int64
	PodUID     string
}

// FirstFence is the fence of an issue the daemon records nothing of: the first run, before any
// phase entry, which is where its first record puts it.
var FirstFence = Fence{Generation: 1, Entry: 1}

// IssueFence is the fence issue stands at, with rows its phases rows (Store.Phases); the pod is
// not read here (Store.IssuePodUID).
func IssueFence(issue Issue, rows []PhaseRow) Fence {
	fence := Fence{Generation: max(issue.Generation, 1), Entry: 1}
	for _, row := range rows {
		fence.Entry += row.Entry
	}
	return fence
}

// ReadFence is the fence of key as tx holds its record and its phases rows (IssueFence), and
// FirstFence for an issue the daemon records nothing of. withPod reads the pod its claims run in
// too (Store.IssuePodUID), which only a kind that acts on a pod asks for.
func ReadFence(ctx context.Context, tx pgx.Tx, store Store, key string, withPod bool) (Fence, error) {
	fence, _, err := readFence(ctx, tx, store, key, withPod)
	return fence, err
}

// readFence is ReadFence, and the record it read the fence from: nil for an issue the daemon
// records nothing of.
func readFence(ctx context.Context, tx pgx.Tx, store Store, key string, withPod bool) (Fence, *Issue, error) {
	issue, err := store.Issue(ctx, tx, key)
	if err != nil {
		return Fence{}, nil, err
	}
	fence := FirstFence
	if issue != nil {
		rows, err := store.Phases(ctx, tx, key)
		if err != nil {
			return Fence{}, nil, err
		}
		fence = IssueFence(*issue, rows)
	}
	if withPod {
		if fence.PodUID, err = store.IssuePodUID(ctx, tx, key); err != nil {
			return Fence{}, nil, err
		}
	}
	return fence, issue, nil
}

// Outlives says whether an effect queued at queued does nothing to an issue standing at f: the
// effect names an earlier or another run, or another phase entry. A part the effect names none
// of (zero: a row a release before this one queued, which the first boot's conversion has not
// fenced, or a kind that carries no entry) fences nothing. The pod is the executor's to compare
// (OutlivesPod), against the pod it acts on.
func (f Fence) Outlives(queued Fence) bool {
	return queued.Generation != 0 && queued.Generation != f.Generation || queued.Entry != 0 && queued.Entry != f.Entry
}

// OutlivesPod says whether an effect that acts on the pod queued names does nothing to pod, the
// pod the issue or its claim runs in now: it names another. An effect that names no pod, and an
// issue whose claims record none now, fence nothing. The rule is the runtime's
// (runtime.OutlivesPod), which a runtime that deletes an issue's pod reads under its own launch
// turn as well.
func OutlivesPod(queued, pod string) bool { return runtime.OutlivesPod(queued, pod) }

// reportOnly says whether payload only tells somebody something: an architect's notice (Notice),
// the project's controller's (ControllerNotice), or the message on an issue that names its
// undelivered hook prompts (MessagePost.HookPrompts), which tells the issue what words reached no
// session. It names the run and the phase entry it was queued at, as every effect does (R17 of
// dispatch://INFRA-3320): NewFencedRow stamps both and NewOutboxRow requires them; its delivery is
// fenced by the run alone (the architect's ruling of 2026-11-21 on dispatch://INFRA-3320): a
// report of an earlier phase entry of the run its issue stands in is still delivered, and one of
// an earlier run is not. Hook prompts fire at moves back, each of which opens a phase entry, so an
// entry fence would drop that message in silence after the very moves that make it. Every other
// kind acts — it changes a claim, a pod, Dispatch or GitHub — and does nothing at a later phase
// entry either, every other message among them. A notice starts and resumes no claim: its
// delivery waits for an architect claim a stop suspended, and wakes only one an idle release
// suspended (the daemon's deliverNotice).
func reportOnly(payload OutboxPayload) bool {
	switch value := payload.(type) {
	case Notice, ControllerNotice:
		return true
	case MessagePost:
		return value.HookPrompts != nil
	}
	return false
}

// LateAt says whether payload, an effect queued at the fence it names (FenceOf), does nothing to an
// issue standing at now when its executor runs: the one statement of what delivery checks. A
// report (reportOnly) is late in a later run alone; every other fenced kind in a later run or at
// another phase entry (Fence.Outlives). A kind that carries no fence is late at nothing.
func LateAt(now Fence, payload OutboxPayload) bool {
	queued, carries := FenceOf(payload)
	if !carries {
		return false
	}
	if reportOnly(payload) {
		queued.Entry = 0
	}
	return now.Outlives(queued)
}

// Stops says whether the request stops a claim of its issue's run: a suspend (a stop or an
// interrupted run), a step stop (the process of the step a stopped task ran, workflow's stopTask),
// an issue close, or an idle release's role stop. Each is fenced by its issue's run, its phase
// entry and the pod its claim ran in (SuperviseRequest.Entry, PodUID); a start keeps its own
// fences (its phase and its task's entry), and a pause's stop and a task's retirement theirs (the
// pause; the run and the entry of the task it retires).
func (r SuperviseRequest) Stops() bool {
	return r.Op == "suspend" || r.Op == "step_stop" || r.Op == "issue_close" || r.Op == "release"
}

// FenceOf is the fence payload names, and whether its kind carries one at all: the parts of
// Fence it carries, each zero where it names none.
func FenceOf(payload OutboxPayload) (Fence, bool) {
	switch value := payload.(type) {
	case StatusWrite:
		return Fence{Generation: value.Generation, Entry: value.Entry}, true
	case MessagePost:
		return Fence{Generation: value.Generation, Entry: value.Entry}, true
	case Notice:
		return Fence{Generation: value.Generation, Entry: value.Entry}, true
	case ControllerNotice:
		return Fence{Generation: value.Generation, Entry: value.Entry}, true
	case SuperviseRequest:
		if !value.Stops() {
			return Fence{}, false
		}
		return Fence{Generation: value.Generation, Entry: value.Entry, PodUID: value.PodUID}, true
	case GateSeed:
		return Fence{Generation: value.Generation}, true
	case PendingBasisSeed:
		return Fence{Generation: value.Generation}, true
	case RetentionEnd:
		return Fence{Generation: value.Generation, Entry: value.Entry}, true
	case IssueSuspend:
		return Fence{Generation: value.Generation, Entry: value.Entry, PodUID: value.PodUID}, true
	case IssueWorkspaceCheck:
		return Fence{Generation: value.Generation, Entry: value.Entry, PodUID: value.PodUID}, true
	case IssueRelease:
		return Fence{Generation: value.Generation, Entry: value.Entry, PodUID: value.PodUID}, true
	case IssueBranch:
		return Fence{Generation: value.Generation}, true
	case CheckWakeDue:
		return Fence{Generation: value.Generation}, true
	case CheckLook:
		return Fence{Generation: value.Generation}, true
	case CheckBound:
		return Fence{Generation: value.Generation}, true
	case RoleMessage:
		return Fence{Generation: value.Generation, Entry: value.Entry}, true
	case PhaseEnded:
		return Fence{Generation: value.Generation, Entry: value.Entry}, true
	case HookPrompt:
		return Fence{Generation: value.Generation, Entry: value.Entry}, true
	case UpdateAsk:
		return Fence{Generation: value.Generation, Entry: value.Entry}, true
	case MergeForwardMessage:
		return Fence{Generation: value.Generation, Entry: value.Entry}, true
	case MergeQueueRemoval:
		return Fence{Generation: value.Generation, Entry: value.Entry}, true
	case ClaimRelease:
		return Fence{Generation: value.Generation, Entry: value.Entry}, true
	}
	return Fence{}, false
}

// actsOnPod says whether payload's kind names the pod it acts on (Fence.PodUID).
func actsOnPod(payload OutboxPayload) bool {
	switch value := payload.(type) {
	case SuperviseRequest:
		return value.Stops()
	case IssueSuspend, IssueWorkspaceCheck, IssueRelease:
		return true
	}
	return false
}

// entryFenced says whether payload's kind names the phase entry it was queued at: every kind
// FenceOf fences but the gate's seeds, which belong to the run across a stop, the branch, which
// is the run's, and the production check's rows, which follow the close (R17 of
// dispatch://INFRA-3320). A notice names its entry too, as every effect does; what its delivery
// checks of it is LateAt's.
func entryFenced(payload OutboxPayload) bool {
	switch payload.(type) {
	case GateSeed, PendingBasisSeed, IssueBranch, CheckWakeDue, CheckLook, CheckBound:
		return false
	}
	_, carries := FenceOf(payload)
	return carries
}

// FenceKeys is what a row of one kind carries of its fence (FenceOf), by the parts a row the
// release before queued may lack: its run, its phase entry and the pod it acts on. A supervise
// row is fenced only by the ops that stop (SuperviseRequest.Stops), which Ops names; every other
// kind carries its fence whatever its payload says, and its Ops is nil.
type FenceKeys struct {
	Kind                      OutboxKind
	Ops                       []SuperviseOp
	Generation, Entry, PodUID bool
}

// fencedPayloads is a zero payload of each kind FenceOf fences whatever its payload says. The
// record's tests hold it to the kinds whose fence the fence inventory decides, so a kind fenced
// later is listed here before any row of it is stamped (FencedKinds).
var fencedPayloads = []OutboxPayload{
	StatusWrite{}, MessagePost{}, Notice{}, ControllerNotice{}, GateSeed{}, PendingBasisSeed{}, RetentionEnd{},
	IssueSuspend{}, IssueWorkspaceCheck{}, IssueRelease{}, IssueBranch{}, CheckWakeDue{}, CheckLook{}, CheckBound{},
	RoleMessage{}, PhaseEnded{}, HookPrompt{}, UpdateAsk{}, MergeForwardMessage{}, MergeQueueRemoval{}, ClaimRelease{},
}

// FencedKinds is every kind whose rows carry a fence, with the parts of it each carries, read off
// FenceOf, entryFenced and actsOnPod, the one statement of each: what the first boot's conversion
// stamps on a row the release before queued without its fence (StampReleaseBeforeFences), so a
// decoder reads every row afterwards with its fence.
func FencedKinds() []FenceKeys {
	keys := make([]FenceKeys, 0, len(fencedPayloads)+1)
	for _, payload := range fencedPayloads {
		keys = append(keys, FenceKeys{Kind: payload.OutboxKind(), Generation: true, Entry: entryFenced(payload), PodUID: actsOnPod(payload)})
	}
	var stops []SuperviseOp
	for _, op := range superviseOps {
		if (SuperviseRequest{Op: op}).Stops() {
			stops = append(stops, op)
		}
	}
	stop := SuperviseRequest{Op: stops[0]}
	return append(keys, FenceKeys{Kind: OutboxKindSupervise, Ops: stops, Generation: true, Entry: entryFenced(stop), PodUID: actsOnPod(stop)})
}

// fenceStamp is what Fenced filled of a payload's fence: the entry, and the generation where
// the payload named none, which a fact's settle sets again to the fence the issue stands at when
// the fact commits (Postgres.RefenceFactRows).
type fenceStamp struct {
	entry, generation bool
}

// Fenced is payload carrying each part of fence its kind carries (FenceOf) and names none of
// yet: a writer that names its run or its pod itself keeps what it named.
func Fenced(payload OutboxPayload, fence Fence) OutboxPayload {
	fenced, _ := fenced(payload, fence)
	return fenced
}

// fenced is Fenced, and what it filled.
func fenced(payload OutboxPayload, fence Fence) (OutboxPayload, fenceStamp) {
	var stamp fenceStamp
	generation := func(g *uint64) {
		if *g == 0 {
			*g, stamp.generation = fence.Generation, true
		}
	}
	entry := func(e *int64) {
		if *e == 0 {
			*e, stamp.entry = fence.Entry, true
		}
	}
	pod := func(p *string) {
		if *p == "" {
			*p = fence.PodUID
		}
	}
	switch value := payload.(type) {
	case StatusWrite:
		generation(&value.Generation)
		entry(&value.Entry)
		return value, stamp
	case MessagePost:
		generation(&value.Generation)
		entry(&value.Entry)
		return value, stamp
	case Notice:
		generation(&value.Generation)
		entry(&value.Entry)
		return value, stamp
	case ControllerNotice:
		generation(&value.Generation)
		entry(&value.Entry)
		return value, stamp
	case SuperviseRequest:
		if !value.Stops() {
			return value, stamp
		}
		generation(&value.Generation)
		entry(&value.Entry)
		pod(&value.PodUID)
		return value, stamp
	case GateSeed:
		generation(&value.Generation)
		return value, stamp
	case PendingBasisSeed:
		generation(&value.Generation)
		return value, stamp
	case RetentionEnd:
		generation(&value.Generation)
		entry(&value.Entry)
		return value, stamp
	case IssueSuspend:
		generation(&value.Generation)
		entry(&value.Entry)
		pod(&value.PodUID)
		return value, stamp
	case IssueWorkspaceCheck:
		generation(&value.Generation)
		entry(&value.Entry)
		pod(&value.PodUID)
		return value, stamp
	case IssueRelease:
		generation(&value.Generation)
		entry(&value.Entry)
		pod(&value.PodUID)
		return value, stamp
	case IssueBranch:
		generation(&value.Generation)
		return value, stamp
	case CheckWakeDue:
		generation(&value.Generation)
		return value, stamp
	case CheckLook:
		generation(&value.Generation)
		return value, stamp
	case CheckBound:
		generation(&value.Generation)
		return value, stamp
	case RoleMessage:
		generation(&value.Generation)
		entry(&value.Entry)
		return value, stamp
	case PhaseEnded:
		generation(&value.Generation)
		entry(&value.Entry)
		return value, stamp
	case HookPrompt:
		generation(&value.Generation)
		entry(&value.Entry)
		return value, stamp
	case UpdateAsk:
		generation(&value.Generation)
		entry(&value.Entry)
		return value, stamp
	case MergeForwardMessage:
		generation(&value.Generation)
		entry(&value.Entry)
		return value, stamp
	case MergeQueueRemoval:
		generation(&value.Generation)
		entry(&value.Entry)
		return value, stamp
	case ClaimRelease:
		generation(&value.Generation)
		entry(&value.Entry)
		return value, stamp
	}
	return payload, stamp
}

// NewFencedRow is NewOutboxRow for a writer that holds the transaction the row is queued in: the
// payload carries the fence issue stands at as tx holds it (ReadFence, Fenced), every part a
// writer names itself kept, and an ordinary status row the status the issue's record last saw
// Dispatch show (StatusWrite.DispatchStatus), read off the same record. It is how every writer of
// a fenced kind queues one. A row whose phase entry it filled is marked for the settle of a fact
// that writes it (WithFencedRows): the fact's own moves may raise the issue's entry after the row
// is queued, and the row belongs to the entry the fact leaves the issue at
// (Postgres.RefenceFactRows).
func NewFencedRow(ctx context.Context, tx pgx.Tx, store Store, issue string, payload OutboxPayload, nextAt time.Time) (OutboxRow, error) {
	if _, carries := FenceOf(payload); carries {
		fence, recorded, err := readFence(ctx, tx, store, issue, actsOnPod(payload))
		if err != nil {
			return OutboxRow{}, fmt.Errorf("read the fence of %s's %s row: %w", issue, payload.OutboxKind(), err)
		}
		var stamp fenceStamp
		payload, stamp = fenced(shownTo(payload, recorded), fence)
		row, err := NewOutboxRow(issue, payload, nextAt)
		row.refence = stamp
		return row, err
	}
	return NewOutboxRow(issue, payload, nextAt)
}

// EnqueueFenced queues payload for issue in tx, due at nextAt, fenced as NewFencedRow fences it:
// the two calls every writer of a fenced row makes, in the one place, since a writer that queued a
// row the fence had not stamped would act on a run or a phase entry the issue has left.
func EnqueueFenced(ctx context.Context, tx pgx.Tx, store Store, issue string, payload OutboxPayload, nextAt time.Time) error {
	row, err := NewFencedRow(ctx, tx, store, issue, payload, nextAt)
	if err != nil {
		return err
	}
	return store.Enqueue(ctx, tx, row)
}

// fencedRowsKey is the context key of the rows one fact queued whose fence its settle sets again.
type fencedRowsKey struct{}

// fencedRow is one row a fact queued with the fence its issue stood at then (NewFencedRow), and
// what of it the stamp filled.
type fencedRow struct {
	id    int64
	issue string
	stamp fenceStamp
}

// fencedRows is every such row of one fact, in the order it queued them.
type fencedRows struct {
	rows []fencedRow
}

// WithFencedRows answers ctx in which the store keeps each row a fact queues with the fence its
// issue stood at when it was queued (NewFencedRow), for the fact's settle to set again to the
// fence the issue stands at once every handler has applied the fact (Postgres.RefenceFactRows).
// A ctx that keeps them already is answered as it is.
func WithFencedRows(ctx context.Context) context.Context {
	if _, ok := ctx.Value(fencedRowsKey{}).(*fencedRows); ok {
		return ctx
	}
	return context.WithValue(ctx, fencedRowsKey{}, &fencedRows{})
}

// noteFencedRow keeps row, queued under id, among the rows of ctx's fact whose fence the settle
// sets again. A ctx that keeps none, and a row whose stamp filled nothing, keeps nothing.
func noteFencedRow(ctx context.Context, id int64, row OutboxRow) {
	rows, ok := ctx.Value(fencedRowsKey{}).(*fencedRows)
	if !ok || row.refence == (fenceStamp{}) {
		return
	}
	rows.rows = append(rows.rows, fencedRow{id: id, issue: row.Issue, stamp: row.refence})
}

// fencedRowsOf is the rows ctx's fact queued whose fence its settle sets again, and forgets
// them, so a second settle of one fact sets nothing twice.
func fencedRowsOf(ctx context.Context) []fencedRow {
	rows, ok := ctx.Value(fencedRowsKey{}).(*fencedRows)
	if !ok {
		return nil
	}
	kept := slices.Clone(rows.rows)
	rows.rows = nil
	return kept
}
