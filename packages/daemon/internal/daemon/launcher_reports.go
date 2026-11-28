package daemon

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// memoryGuardKills turns one process a pod's memory guard ended, as the pod's launcher reported
// it, into the fact the workflow applies: intake.MemoryGuardKill for an issue pod's (killed) and
// intake.ControllerMemoryGuardKill for the controller pod's (controllerKilled). It is the seam
// between the Kubernetes runtime factory (sandboxRuntime), which prepareSandbox builds before the
// workflow opens, and the fact path, which exists only after it: prepareSandbox makes one and
// hands it to the factory, and the boot fills it the moment the workflow is open
// (plan.memoryGuardKills, daemon.run). A holder nobody filled refuses every report with
// errMemoryGuardKillsUnbound, which the launcher session logs with the report it drops.
//
// It is filled once, in the boot's own goroutine, before the runtime is built and so before any
// launcher can connect: there is nothing to lock, and nothing ever replaces what was filled.
type memoryGuardKills struct {
	pool     *store.Pool
	handlers []intake.Handler
}

// errMemoryGuardKillsUnbound is what a holder the boot never filled answers every report with. Its
// text names the line that fills it, since a log line is then the only place the drop shows: the
// role or the controller whose process was ended is told nothing. An unbound holder can only be a
// boot bug, never a daemon that runs no workflow: prepareSandbox makes the holder only under
// runtime: kubernetes, and configuration load refuses a runtime: kubernetes file that names no
// dispatch_url (config's checkKubernetesKeys, "the workflow is what launches every pod"), so every
// daemon with a holder opens the workflow whose boot fills it.
var errMemoryGuardKillsUnbound = errors.New("the daemon's memory guard kill holder was never bound to the workflow " +
	"(daemon.go, plan.memoryGuardKills.bind), so this report and every later one is dropped")

// bind gives the holder the daemon's guarded pool (store.Pool) and the handlers a fact applies
// through, which is what the workflow has and the factory does not.
func (m *memoryGuardKills) bind(pool *store.Pool, handlers []intake.Handler) {
	m.pool, m.handlers = pool, handlers
}

// killed applies one kill of issue's pod as one intake fact (runtime.MemoryGuardKilled), crediting
// it to token, the role whose connection carried the report, and to podUID, the pod that launcher
// runs in, both the session's and never the report's. It runs on the launcher's own frame loop
// under that loop's bounded context, so an apply that cannot be made is returned to the launcher
// session, which logs it and drops the report. Every launcher's frame loop runs beside the others
// and beside the daemon's own loops, so each report applies under a hold mark of its own
// (store.ForConcurrentWorker): this is a fact source, and a fact source marks its context where
// it starts (store.ErrNestedAcquire).
//
// The event id is the kill's own identity, the guard's id and its sequence
// (memoryGuardKillEventID), and never the daemon's receipt: a launcher that wrote a report on a
// connection it then lost sends it again on the next, and ApplyFact applies one id once, so the
// role is told of one kill once however often its report arrives.
func (m *memoryGuardKills) killed(ctx context.Context, issue string, token claim.Token, podUID string, report shimwire.LauncherMemoryKill) error {
	return m.apply(ctx, report, intake.MemoryGuardKill{Issue: issue, Token: token, PodUID: podUID, Report: report}, issue, podUID)
}

// controllerKilled applies one kill of the controller's pod as one intake fact
// (runtime.ControllerMemoryGuardKilled), crediting it to token, the claim whose connection carried
// the report, and to podUID, the pod that launcher runs in, both the session's and never the
// report's. It runs where killed runs, under the same bounded context and hold mark, and answers
// a holder nobody bound with the same errMemoryGuardKillsUnbound, for the same reason.
//
// The event id is the one killed applies under (memoryGuardKillEventID): the guard's id is minted
// by each launcher process, so a kill of the controller's pod and one of an issue pod never share
// it, and a report the controller's launcher sent again on its next connection is applied once, so
// the controller is told of one kill once.
func (m *memoryGuardKills) controllerKilled(ctx context.Context, token claim.Token, podUID string, report shimwire.LauncherMemoryKill) error {
	return m.apply(ctx, report, intake.ControllerMemoryGuardKill{Token: token, PodUID: podUID, Report: report}, "the controller", podUID)
}

// apply applies fact, the intake fact of report, under the kill's own event id
// (memoryGuardKillEventID) and a hold mark of its own (store.ForConcurrentWorker), which is what
// killed and controllerKilled share. An apply that fails names the kill's pod in its error: owner
// is whose pod podUID is, an issue's key or `the controller`.
func (m *memoryGuardKills) apply(ctx context.Context, report shimwire.LauncherMemoryKill, fact intake.Fact, owner, podUID string) error {
	if m.pool == nil || len(m.handlers) == 0 {
		// Nothing bound the holder to the workflow, so there is no fact path to apply the report
		// through. That is a boot that skipped plan.memoryGuardKills.bind, never a daemon that runs
		// no workflow: only a runtime: kubernetes daemon has a holder, and configuration load refuses
		// one whose file names no dispatch_url (errMemoryGuardKillsUnbound). Every report of every
		// pod meets it, so it is returned rather than swallowed: the launcher session logs it with
		// the report it dropped (sandbox's memoryKilled), which is what makes the drop findable.
		return errMemoryGuardKillsUnbound
	}
	if _, err := intake.ApplyFact(store.ForConcurrentWorker(ctx), m.pool, "launcher", memoryGuardKillEventID(report), fact, m.handlers...); err != nil {
		return fmt.Errorf("apply the memory guard's kill of pid %d in %s's pod %s: %w", report.PID, owner, podUID, err)
	}
	return nil
}

// memoryGuardKillEventID is the event id a memory guard's kill report is applied under:
// `memory-guard-kill:<guard>:<sequence>`, the guard's own id (minted once per launcher process)
// and its count of kills, which together name one kill of one pod's guard.
func memoryGuardKillEventID(report shimwire.LauncherMemoryKill) string {
	return fmt.Sprintf("memory-guard-kill:%s:%d", report.Guard, report.Sequence)
}

// volumeTrimReports turns one issue pod's volume sweep, as its launcher's cache trimmer reported
// it, into the fact the workflow applies (intake.VolumeNearFull). It is the seam between the
// Kubernetes runtime, which is built before the workflow opens, and the fact path, which exists
// only after it: prepareSandbox makes one and hands it to the factory, and the boot fills it the
// moment the workflow is open (plan.volumeReports, daemon.Run). A holder nobody filled refuses
// every report with errVolumeReportsUnbound, which the launcher session logs with the report it
// drops.
//
// It is filled once, in the boot's own goroutine, before the runtime is built and so before any
// launcher can connect: there is nothing to lock, and nothing ever replaces what was filled.
type volumeTrimReports struct {
	pool     *store.Pool
	handlers []intake.Handler
}

// errVolumeReportsUnbound is what a holder the boot never filled answers every report with. Its
// text names the line that fills it, since a log line is the only place the drop shows: no
// architect is told, and the trimmer goes on sweeping.
var errVolumeReportsUnbound = errors.New("the daemon's volume report holder was never bound to the workflow " +
	"(daemon.go, plan.volumeReports.bind), so this report and every later one is dropped")

// bind gives the holder the daemon's guarded pool (store.Pool) and the handlers a fact applies
// through, which is what the workflow has and the factory does not.
func (v *volumeTrimReports) bind(pool *store.Pool, handlers []intake.Handler) {
	v.pool, v.handlers = pool, handlers
}

// report applies one sweep of issue's pod as one intake fact (runtime.VolumeReported). It runs on
// the launcher's own frame loop under that loop's bounded context, so an apply that cannot be
// made is returned to the launcher session, which logs it and drops the report; the trimmer's
// next sweep that ends above the low mark reports the volume again, and a dropped report of a
// trimmer that switched itself off is not sent again. Every launcher's frame loop runs beside
// the others and beside the daemon's own loops, so each report applies under a hold mark of its
// own (store.ForConcurrentWorker): this is a fact source, and a fact source marks its context
// where it starts (store.ErrNestedAcquire).
//
// The event id is unique per report and never per sweep: nothing in the frame numbers a report,
// and ApplyFact applies one id exactly once — an id built from the report's own contents would
// silently drop the second report of an episode whose numbers had not moved, which is precisely
// the pod that is full and not changing. So the id carries the daemon's own receipt — the issue,
// the pod, the trimmer and the episode — with a random tail, as api.requestFactID gives each of
// the API's own requests, which is unique across processes as well as within one.
func (v *volumeTrimReports) report(ctx context.Context, issue, podUID string, report shimwire.LauncherVolumeReport) error {
	if v.pool == nil || len(v.handlers) == 0 {
		// Nothing bound the holder to the workflow, so there is no fact path to apply the report
		// through. That is a boot that skipped plan.volumeReports.bind, and every report of every
		// pod meets it, so it is returned rather than swallowed: the launcher session logs it with
		// the report it dropped (sandbox's volumeReported), which is what makes the drop findable.
		return errVolumeReportsUnbound
	}
	fact := intake.VolumeNearFull{Issue: issue, PodUID: podUID, Report: record.VolumeNearFull{
		Trimmer: report.Trimmer, Episode: report.Episode, RaisedBy: report.RaisedBy,
		UsedBytes: report.UsedBytes, FreeBytes: report.FreeBytes, FreedBytes: report.FreedBytes,
		StopReason: report.StopReason, RefusedReads: report.RefusedReads, RefusedReadsOmitted: report.RefusedReadsOmitted,
		Sizes: volumeSizes(report.Sizes), SizesOmitted: report.SizesOmitted,
		SizesAt: report.SizesAt, SizesPartial: report.SizesPartial,
		SizesMissing: volumeMissing(report.SizesMissing), SizesMissingOmitted: report.SizesMissingOmitted,
		Faults: report.Faults, FaultsOmitted: report.FaultsOmitted, SwitchedOff: report.SwitchedOff,
	}}
	eventID := fmt.Sprintf("volume-near-full:%s:%s:%s:%d:%s", issue, podUID, report.Trimmer, report.Episode, rand.Text())
	if _, err := intake.ApplyFact(store.ForConcurrentWorker(ctx), v.pool, "launcher", eventID, fact, v.handlers...); err != nil {
		return fmt.Errorf("apply the volume report of %s's pod %s: %w", issue, podUID, err)
	}
	return nil
}

// volumeSizes and volumeMissing are the wire's size walk as the record keeps it. The two shapes
// are the same members by design — the record's is durable and the wire's is not — and these are
// the one place they are copied across.
func volumeSizes(sizes []shimwire.LauncherVolumeSize) []record.VolumeSize {
	if len(sizes) == 0 {
		return nil
	}
	kept := make([]record.VolumeSize, 0, len(sizes))
	for _, size := range sizes {
		kept = append(kept, record.VolumeSize{Path: size.Path, Bytes: size.Bytes,
			BytesOverHour: size.BytesOverHour, BytesOverDay: size.BytesOverDay,
			Refused: size.Refused, Refusals: size.Refusals})
	}
	return kept
}

// volumeMissing is the walk's unmeasured targets, and the volume root or an immediate child of
// it where the walk did not list one in full, each with its reason, as volumeSizes says.
func volumeMissing(missing []shimwire.LauncherVolumeMissing) []record.VolumeMissing {
	if len(missing) == 0 {
		return nil
	}
	kept := make([]record.VolumeMissing, 0, len(missing))
	for _, one := range missing {
		kept = append(kept, record.VolumeMissing{Path: one.Path, Reason: one.Reason})
	}
	return kept
}
