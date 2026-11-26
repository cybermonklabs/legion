package supervise

import (
	"context"
	"fmt"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// The claim's value in the task bucket (dispatch://INFRA-2811): the record writes it under the
// pending task's number (writeValue, putValue) and deletes it once the record owes that task no
// more (deleteValue, dropPending), writes it again where the worker asked at the latest revision
// and the task is free to take (offerPending), compares it with the bucket at the daemon's boot
// and at each change its watch hands over (valueRead), records the newest revision the worker
// reports seeing (taskSeen), and reads it for a worker that registers or asks (CurrentTask). The
// take of the task the value names is take.go's.

// TaskValues is the task bucket as a machine writes it: one value per claim, keyed on the claim's
// token, naming the number of the task the record owes it. The daemon's implementation is the
// legion_tasks bucket on NATS (internal/daemon's task bucket); a fake stands in for it in tests.
//
// A write answers the bucket's epoch and the revision the write made, which the claim records as
// the daemon's own last write (Claim.TaskWrittenRevision). A write and a delete are writes of the
// machine's own, made under the machine's lock and the claim-write bound alone (writeBucket): the
// bucket is no store of the pool, so the wait for a connection a claim write is also given has
// nothing to count here. A read takes neither, since it is made for a worker that registers or
// asks and copies no claim (CurrentTask).
type TaskValues interface {
	// Write puts the claim's value under number, and answers the epoch of the bucket it wrote
	// to and the revision the write made.
	Write(ctx context.Context, token claim.Token, number uint64) (epoch string, revision uint64, err error)
	// Delete deletes the claim's value.
	Delete(ctx context.Context, token claim.Token) error
	// Read is the claim's current value, and false where the bucket holds none for it.
	Read(ctx context.Context, token claim.Token) (TaskValue, bool, error)
}

// TaskValue is one claim's value as the bucket holds it: the task number, and the epoch and
// revision of the write that put it there. A revision is read only beside its epoch, because a
// bucket created again counts its revisions from 1 again (TaskRevision).
type TaskValue struct {
	Number   uint64
	Epoch    string
	Revision uint64
}

// revision is the revision of the write that put the value in the bucket.
func (v TaskValue) revision() TaskRevision {
	return TaskRevision{Epoch: v.Epoch, Revision: v.Revision}
}

// TaskRevision is one revision of a claim's value in the task bucket: the bucket's epoch, and the
// revision within it. A revision is read only beside its epoch, because a bucket created again
// counts its revisions from 1 again, so the two are one value and are compared as one: equal only
// where both are (==), and ordered only within one epoch (after, before). The zero value is no
// revision, which a claim that has seen or written nothing holds. The store keeps the two in
// columns of their own, and the wire in fields of its own (claim.TaskValue, claim.TakeRequest).
type TaskRevision struct {
	Epoch    string
	Revision uint64
}

// after is whether r is a revision o does not cover: a later one of o's epoch, or one of another
// epoch, which a record of o's epoch has seen and written nothing of.
func (r TaskRevision) after(o TaskRevision) bool {
	return r.Epoch != o.Epoch || r.Revision > o.Revision
}

// before is whether r is an earlier revision of o's epoch.
func (r TaskRevision) before(o TaskRevision) bool {
	return r.Epoch == o.Epoch && r.Revision < o.Revision
}

// TaskValueRead is what the task bucket holds for the claim's key, as the daemon read it
// (internal/daemon's task bucket): once at its boot, for every claim it supervises, again for
// every claim when the bucket was created again, and from then on at each change its watch of the
// whole bucket hands over on a key of this daemon's project. The machine compares it with the
// record and writes the record's value again where the two differ (valueRead), so the record
// stays the one truth and the bucket its delivered copy (R3 of dispatch://INFRA-2811).
//
// Value is the bucket's entry: the task its value names, the bucket's epoch, and the entry's
// revision. Held is false where the bucket holds no value for the key: one deleted, whose
// Revision is the delete's, and one never written or since purged, whose Revision is the bucket's
// last revision when the daemon read it, so a read made before the daemon's own last write is
// told apart from one made after. Cause is TaskValueBoot or TaskValueRepair, the cause a write it
// makes is logged under.
type TaskValueRead struct {
	Claim claim.Token
	Value TaskValue
	Held  bool
	Cause string
}

// The causes a write a TaskValueRead makes is logged under: the boot's read of the bucket, and
// the watch's, which reads the bucket again whole once it was created again.
const (
	TaskValueBoot   = "boot"
	TaskValueRepair = "repair"
)

// RequestTaskSeen is the worker reporting that it was handed the claim's value at this revision,
// whether or not its host is idle. Number is the task the value names. The claim records the
// newest such report (Claim.TaskSeenRevision), and a revision it had not recorded leaves no take
// at it yet (Claim.TaskTakeState). Generation is the launch the reporting process runs at, and
// Session its session where the caller names one: the boot form of the claim's route names none,
// since its boot token already names the launch.
type RequestTaskSeen struct {
	Claim      claim.Token
	Generation uint64
	Session    string
	Number     uint64
	Revision   TaskRevision
}

func (RequestTaskSeen) isEvent() {}
func (TaskValueRead) isEvent()   {}

// The causes a write of the claim's value is logged under (valueWrittenLine), beside TaskValueBoot
// and TaskValueRepair: the task's first queue (queue), the end of a hold that refused a take
// (offerPending), a task taken back in this same process that can be taken again (offerPending,
// Machine.rewrite), a task whose take this same process failed to decide or record, once the task
// is free to take again (offerPending, Machine.rewrite), and the first bound passing over a value
// nobody reported seeing, or one whose seen report this daemon read from the store
// (takeBoundOver).
const (
	TaskValueQueued     = "queued"
	TaskValueHoldEnded  = "hold-ended"
	TaskValueRetake     = "retake"
	TaskValueTakeFailed = "take-failed"
	TaskValueUnseen     = "unseen"
)

// The causes a delete of the claim's value is logged under (valueDeletedLine): a task finished
// (finishTask), a task the record dropped (dropPending: a phase the issue left, an older daemon's
// ask of an update round, a root task that names nothing any more), a phase task a daemon
// suspension ended (settle), a round's ask a retirement dropped (retire), and the task of a phase
// the workflow stopped under its worker (RetireStoppedTask). A value the record owes nothing is
// deleted under the cause of the read that found it (valueRead).
const (
	valueDeletedFinished  = "finished"
	valueDeletedDropped   = "dropped"
	valueDeletedSuspended = "suspended"
	valueDeletedRetired   = "retired"
	valueDeletedStopped   = "stopped"
)

// offerPending writes the claim's value again under its pending task's number where the worker
// asked at the latest revision the daemon wrote and the task is free to take now: the worker
// asks once per revision, so a value its take was refused at — by a hold, or because the daemon
// failed to decide or record the take (undecided) — or one whose take a provider error, a
// compaction or a death took back says nothing new until it is written again. It is the end of
// every hold that refused a take, and runs at each point one can end: the agent's ready, the
// shim's hello, the sweep's observation, the end of a turn on this claim or another of its issue,
// a start's arrival, the end of a provider error's wait and of a compaction, the end of an update
// round. Which hold ended is not asked: takeable says whether any still stands, and the value is
// written only where none does, so a hold that still stands costs the worker nothing and the
// sweep never writes for one.
//
// A value the worker has not reported seeing at that revision is left alone: the worker has not
// asked at it. A write the bucket refused is made here at the next chance, whatever the worker
// asked (valueUnwritten), and so is a delete the bucket refused while the record owes the claim
// no task (valueUndeleted, deleteAgain).
//
// The write's cause is the one the latest event that left one names (Machine.rewrite):
// TaskValueTakeFailed where this same process failed to decide or record the take at that
// revision, TaskValueRetake where the task was taken back in this same process and can be taken
// again now — after a provider error's wait, a compaction, the second wait — and
// TaskValueHoldEnded for every other write here.
//
// A take-failed write waits while the claim owes a write the store did not take (owedWrite): the
// store is still away, so a take at the new revision would fail its own write in turn, and a task
// whose takeable reads nothing of the store — one of no phase — would go round a value write, a
// take and a failed answer at every sweep for as long as the store is away. The write is made at
// the first chance after the owed write lands, which the next decision's repair makes.
func (m *Machine) offerPending(ctx context.Context) error { return m.offer(ctx, false) }

// offer is offerPending, told whether this decision has already asked the record whether it still
// owes the pending task and was told that it does (owed: queue's own drop test, dropUnowed), so
// takeable does not ask the record that again: nothing between the two asks changes the answer.
func (m *Machine) offer(ctx context.Context, owed bool) error {
	p := m.claim.Pending
	if p == nil {
		m.deleteAgain(ctx)
		return nil
	}
	if p.taken() {
		return nil
	}
	c := &m.claim
	asked := c.sawLatestWrite() && (c.TaskTakeState == TakeAccepted || c.TaskTakeState == TakeRefused)
	if !asked && !m.valueUnwritten {
		return nil
	}
	cause := TaskValueHoldEnded
	if r := m.rewrite; r.number == p.Number && r.cause != "" {
		cause = r.cause
	}
	if cause == TaskValueTakeFailed && m.owedWrite != nil {
		m.log.Debug("supervise: the value of a take that failed on the store waits for the store's owed write to land",
			"task", p.Number)
		return nil
	}
	hold, dropped, err := m.takeable(ctx, owed)
	if err != nil || dropped || hold != "" {
		return err
	}
	return m.writeValue(ctx, cause)
}

// valueRewrite is a pending task whose next value write at a hold's end is logged under a cause of
// its own (Machine.rewrite): the task's number, and the cause.
type valueRewrite struct {
	number uint64
	cause  string
}

// writeValue writes the claim's value under the pending task's number and records the write as
// the daemon's own last (Claim.TaskWrittenRevision). A write the bucket refuses fails nothing of
// the decision: it is logged, and made again at the claim's next chance (valueUnwritten,
// offerPending).
func (m *Machine) writeValue(ctx context.Context, cause string) error {
	p := m.claim.Pending
	if p == nil {
		return nil
	}
	revision, written := m.putValue(ctx, *p, cause)
	if !written {
		return nil
	}
	m.claim.TaskWrittenRevision = revision
	return m.persist(ctx)
}

// putValue writes d's number as the claim's value, under the claim-write deadline, and reports
// the write: a claim the daemon has no bucket for writes nothing and reports none. The line it
// logs (valueWrittenLine) is read off the claim before the write: whether the claim had finished
// the last task written to it in the session it runs (Claim.finishedLastTask), which only the
// queue of a new number can find true — a write of the number pending finds that number written
// and not finished — and that session.
func (m *Machine) putValue(ctx context.Context, d Delivery, cause string) (TaskRevision, bool) {
	if m.deps.TaskValues == nil || d.Number == 0 {
		return TaskRevision{}, false
	}
	finished := m.claim.finishedLastTask()
	var revision TaskRevision
	err := m.writeBucket(ctx, func(ctx context.Context) error {
		var err error
		revision.Epoch, revision.Revision, err = m.deps.TaskValues.Write(ctx, m.claim.Token, d.Number)
		return err
	})
	if err != nil {
		m.valueUnwritten = true
		m.log.Warn("supervise: the task value could not be written; it is written again at the claim's next chance",
			"task", d.Number, "origin", d.Origin, "cause", cause, "error", err)
		return TaskRevision{}, false
	}
	m.valueUnwritten, m.valueWrittenAt, m.rewrite, m.seenFromStore = false, m.deps.Clock.Now(), valueRewrite{}, false
	m.log.Info(valueWrittenLine, "task", d.Number, "origin", d.Origin, "finished", finished,
		"session", m.claim.Session, "epoch", revision.Epoch, "revision", revision.Revision, "cause", cause)
	return revision, true
}

// finishedLastTask is whether the claim had finished the last task written to it, in the session
// it runs now: the record has written it a task, the last task it finished is that one, and the
// session it finished it in is the session it holds (dispatch://INFRA-2811 R7, the waited figure
// of R9). A task written to such a claim is written to a worker whose phase stood waiting, which
// is what the cost reading counts (ReadTaskCost); workflow's finishedInItsSession is the same
// question asked of a claim that holds no task.
func (c Claim) finishedLastTask() bool {
	return c.TaskWritten != 0 && c.TaskFinished == c.TaskWritten && c.TaskFinishedSession == c.Session
}

// deleteValue deletes the claim's value: the record no longer owes the task it named. A delete
// the bucket refuses is logged and marked (valueUndeleted), and made again at the claim's next
// chance while the record still owes it no task (offerPending, deleteAgain): the daemon's own
// watch of the bucket leaves a value that is its own last write alone (valueRead), so nothing
// else would delete it before the claim's next task or the daemon's next boot, and every new
// holder of the role would be handed it meanwhile.
func (m *Machine) deleteValue(ctx context.Context, number uint64, cause string) {
	if m.deps.TaskValues == nil {
		return
	}
	if err := m.writeBucket(ctx, func(ctx context.Context) error { return m.deps.TaskValues.Delete(ctx, m.claim.Token) }); err != nil {
		m.valueUndeleted = valueDelete{number: number, cause: cause}
		m.log.Warn("supervise: the task value could not be deleted; it is deleted again at the claim's next chance",
			"task", number, "cause", cause, "error", err)
		return
	}
	m.valueUndeleted = valueDelete{}
	m.log.Info(valueDeletedLine, "task", number, "cause", cause)
}

// valueDelete is a delete of the claim's value the bucket refused (Machine.valueUndeleted): the
// number the value named and the delete's cause, which its line names again when it is made.
type valueDelete struct {
	number uint64
	cause  string
}

// deleteAgain makes the delete of the claim's value the bucket refused, where one is marked
// (valueUndeleted). It is called only while the record owes the claim no task (offerPending): a
// task queued since the refused delete has its own value in the bucket, which this must not
// delete, and that task ends in a delete of its own, which replaces the mark either way. Any value
// the bucket holds while the record owes nothing is one the record has dropped (R1).
func (m *Machine) deleteAgain(ctx context.Context) {
	if d := m.valueUndeleted; d != (valueDelete{}) {
		m.deleteValue(ctx, d.number, d.cause)
	}
}

// dropPending retires the pending task because the record no longer owes it, and deletes its
// value: the take that meets it is answered dropped, and a worker reading the bucket finds
// nothing to ask about.
func (m *Machine) dropPending(ctx context.Context, cause string) error {
	number := m.claim.Pending.Number
	if err := m.retirePending(ctx); err != nil {
		return err
	}
	m.deleteValue(ctx, number, cause)
	return nil
}

// taskSeen records the newest revision of the claim's value its worker reports seeing. A revision
// of its epoch it had already recorded, or an older one, changes nothing: the take state is the
// newest revision's (Claim.TaskTakeState), and a new revision has had no take yet. A report that
// moves the revision is logged (taskSeenLine), with its epoch, and is one this process received,
// so the first wait on it charges as any does (Machine.seenFromStore).
func taskSeen(m *Machine, ctx context.Context, ev Event) error {
	seen := ev.(RequestTaskSeen)
	c := &m.claim
	if !seen.Revision.after(c.TaskSeenRevision) {
		return nil
	}
	c.TaskSeenRevision, c.TaskTakeState = seen.Revision, ""
	m.seenFromStore = false
	m.log.Info(taskSeenLine, "task", seen.Number, "epoch", seen.Revision.Epoch, "revision", seen.Revision.Revision)
	return m.persist(ctx)
}

// valueRead compares the bucket's value for the claim with the record (TaskValueRead). A read
// older than the daemon's own last write of the value — its epoch, an earlier revision — is one the
// record has moved past, and changes nothing: the watch hands over each change in order, and the
// daemon may have written again before this read reached the machine. Otherwise:
//
//   - a claim the record owes a task has its value written again, under the pending task's own
//     number, wherever the bucket holds anything but the daemon's own last write: no value,
//     another number, a later revision someone else wrote, or another epoch, which is the bucket
//     created again (writeValue). Nothing of the record's numbers moves; the write only records
//     the daemon's new last write (Claim.TaskWrittenRevision);
//   - where the bucket holds that last write, nothing is written, but for the one value R3 names:
//     at the boot, a task the record owes again after a take accepted at its latest revision —
//     a provider error's wait or a compaction the restart cut off took it back — is written once,
//     since its worker asks once per revision and has asked at this one (owedAgainAfterTake). A
//     take that revision's hold refused is written at that hold's end instead (offerPending), and
//     a value still unseen or seen and not asked about is left as it stands;
//   - a claim the record owes nothing has the value the bucket holds deleted, unless it is the
//     daemon's own last write, whose delete is already on its way; at the boot, whatever it is,
//     since no write of this daemon's is in flight then.
func valueRead(m *Machine, ctx context.Context, ev Event) error {
	read := ev.(TaskValueRead)
	c := &m.claim
	if read.Value.revision().before(c.TaskWrittenRevision) {
		return nil
	}
	own := read.Held && read.Value.revision() == c.TaskWrittenRevision
	p := c.Pending
	if p == nil || p.Number == 0 {
		if read.Held && (!own || read.Cause == TaskValueBoot) {
			m.log.Info("supervise: the task bucket holds a value for a claim the record owes nothing; it is deleted",
				"task", read.Value.Number, "epoch", read.Value.Epoch, "revision", read.Value.Revision, "cause", read.Cause)
			m.deleteValue(ctx, read.Value.Number, read.Cause)
		}
		return nil
	}
	if own && read.Value.Number == p.Number {
		if read.Cause == TaskValueBoot && m.owedAgainAfterTake() {
			m.log.Info("supervise: the task was taken back after a take at its latest revision, and its worker asks once per revision; it is written again",
				"task", p.Number, "revision", c.TaskWrittenRevision.Revision)
			return m.writeValue(ctx, read.Cause)
		}
		m.log.Debug("supervise: the task bucket holds the value the record wrote", "task", p.Number,
			"epoch", read.Value.Epoch, "revision", read.Value.Revision, "cause", read.Cause)
		return nil
	}
	m.log.Info("supervise: the task bucket holds another value than the record's; it is written again from the record",
		"task", p.Number, "held", read.Held, "heldTask", read.Value.Number, "epoch", read.Value.Epoch,
		"revision", read.Value.Revision, "written", c.TaskWrittenRevision.Revision, "writtenEpoch", c.TaskWrittenRevision.Epoch, "cause", read.Cause)
	return m.writeValue(ctx, read.Cause)
}

// owedAgainAfterTake is whether the claim owes its pending task again after a take accepted at
// the latest revision the daemon wrote of its value: the worker reported that revision seen and
// its take was accepted, and the take has since gone (takeBack) with the task still pending. Its
// worker asks once per revision, so nothing but a new revision makes it ask again (R3).
func (m *Machine) owedAgainAfterTake() bool {
	c := &m.claim
	return c.TaskTakeState == TakeAccepted && c.sawLatestWrite() && !c.Pending.taken()
}

// sawLatestWrite is whether the newest revision of the claim's value its worker reported seeing is
// the daemon's own last write of it (Claim.TaskSeenRevision against TaskWrittenRevision).
func (c Claim) sawLatestWrite() bool {
	return c.TaskSeenRevision == c.TaskWrittenRevision
}

// CurrentTask is what a claim's worker is told of its task when it registers or asks: the value
// the bucket holds for the claim now, nil where it holds none, and the number of the last task
// the claim finished.
type CurrentTask struct {
	Value    *TaskValue
	Finished uint64
}

// CurrentTask reads the claim's value from the bucket and the finished number from the claim as
// its machine last published it, waiting on no decision and copying no claim (View, WaitingWake).
func (m *Machine) CurrentTask(ctx context.Context) (CurrentTask, error) {
	view := m.view.Load()
	current := CurrentTask{Finished: view.TaskFinished}
	if m.deps.TaskValues == nil {
		return current, nil
	}
	value, held, err := m.deps.TaskValues.Read(ctx, view.Token)
	if err != nil {
		return CurrentTask{}, fmt.Errorf("read the task value of %s: %w", view.Token, err)
	}
	if held {
		current.Value = &value
	}
	return current, nil
}
