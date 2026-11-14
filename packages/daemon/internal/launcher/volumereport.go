package launcher

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// The trimmer's report half: the report a sweep or the switched-off trimmer makes, the bound it is
// carried under, and its delivery on the pod's bound connection, held while no connection takes
// it. The trimmer that makes them is volumetrim.go's.

// flush tries the report the last attempt could not send. A report the daemon never got is a
// volume nobody was told about, so the trimmer keeps it rather than dropping it, as
// bundleRestore keeps its own confirmation across every connection of the pod (restore.go).
func (t *trimmer) flush() {
	if t.held == nil {
		return
	}
	if err := t.send(*t.held); err != nil {
		t.logf(volumeTrimLog+"the held report could not be sent either (%v); holding it for the next attempt", err)
		return
	}
	t.held = nil
}

// deliver sends one report, holding it when the connection refuses it. A report made while an
// older one is still held replaces it once the older one has been tried: the daemon counts
// episodes and not reports, and of two reports of the same episode the newer reading is the one
// worth carrying.
func (t *trimmer) deliver(report shimwire.LauncherVolumeReport) {
	t.flush()
	if err := t.send(report); err != nil {
		t.logf(volumeTrimLog+"the report could not be sent (%v); holding it for the next attempt", err)
		t.held = &report
	}
}

// switchOff makes the one report no sweep makes: the trimmer has stopped itself, and this is the
// last the daemon hears from it until the launcher restarts. It carries no sizes, because the
// volume may be perfectly healthy and a size walk would cost a minute of the pod for a number
// nobody asked for. It goes through deliver, so a connection that cannot take it holds it as any
// other report is held, and run sends it once the connection is up again (deliverLast): R2 has
// the trimmer hold its report until that connection is up again, and this report is the one
// that tells the daemon the pod's volume is no longer guarded.
//
// It carries the faults that stopped the trimmer and the refused /proc reads no report has
// carried yet, as a sweep's report does (report), and it is the last report there is to carry
// them.
func (t *trimmer) switchOff() {
	report := shimwire.LauncherVolumeReport{
		Trimmer:             t.id,
		Episode:             max(t.episode, 1),
		RaisedBy:            t.raisedBy,
		UsedBytes:           int64(t.last.UsedBytes),
		FreeBytes:           int64(t.last.AvailableBytes),
		StopReason:          fmt.Sprintf("the trimmer switched itself off after %d panics in a row", t.consecutive),
		RefusedReads:        t.refusedReads,
		RefusedReadsOmitted: t.refusedDropped,
		Faults:              t.faults,
		FaultsOmitted:       t.faultsDropped,
		SwitchedOff:         true,
	}
	if report.RaisedBy == "" {
		// The trimmer never saw a reading at the high mark, so no episode was ever raised; the
		// frame's own rule is that an episode is at least 1, which the daemon applies to every
		// report it reads (LauncherVolumeReport.Validate, sandbox's volumeReported), and this is
		// the first thing the daemon hears of this trimmer.
		report.RaisedBy = shimwire.VolumeEpisodeFirst
	}
	t.faults, t.faultsDropped = nil, 0
	t.refusedReads, t.refusedDropped = nil, 0
	t.deliver(t.bound(report))
}

// deliverLast is the switched-off trimmer's one remaining duty: the report switchOff could not
// send, tried once every volumeSettingsWait until a connection takes it. It ends when the report
// is sent, when the container stops, and on an answer with no settings, which discards it as R2
// has such an answer discard any report the trimmer was holding: that daemon has no case for a
// report (manager.sendVolumeReport).
//
// A failed attempt is written when its reason changes and not once per attempt, for the reason
// a refusal is (says): a connection that is down answers the same way every ten seconds for as
// long as it is down.
func (t *trimmer) deliverLast(ctx context.Context) {
	if t.held == nil {
		return
	}
	ticks, stop := t.ticks(volumeSettingsWait)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
		if t.settingsInForce() == nil {
			t.logf(volumeTrimLog + "no settings are in force: the last report of the switched-off trimmer is discarded")
			return
		}
		if err := t.send(*t.held); err != nil {
			if t.says(err.Error()) {
				t.logf(volumeTrimLog+"the last report of the switched-off trimmer could not be sent (%v); holding it for the next attempt", err)
			}
			continue
		}
		t.held = nil
		t.logf(volumeTrimLog + "the last report of the switched-off trimmer was sent")
		return
	}
}

// carryRefused keeps the refused /proc reads of a sweep that sent no report for the next report
// the trimmer sends (report, switchOff): R3 has a refused read carried into the next report,
// because a process the trimmer cannot read may be using anything. A process already carried is
// not carried twice, and past the most a report names (volumeReportRefusedReads) a refusal is
// counted rather than kept, so a pod whose sweeps keep meeting one unreadable process holds one
// line for it, and a pod that never reports holds no more than the bound for the life of the
// launcher process.
func (t *trimmer) carryRefused(refused []string) {
	for _, one := range refused {
		switch {
		case slices.Contains(t.refusedReads, one):
		case len(t.refusedReads) < volumeReportRefusedReads:
			t.refusedReads = append(t.refusedReads, one)
		default:
			t.refusedDropped++
		}
	}
}

// report is one sweep as the daemon hears it, with the sizes of a fresh walk or of the last one
// with the time it was read (sizes). It takes the faults the trimmer recovered and the refused
// /proc reads it carried since the last report with it, whether or not the send gets across: a
// held report carries them, so holding them twice would report them twice.
func (t *trimmer) report(ctx context.Context, out sweepOutcome) shimwire.LauncherVolumeReport {
	walk := t.sizes(ctx)
	faults, dropped := t.faults, t.faultsDropped
	t.faults, t.faultsDropped = nil, 0
	t.carryRefused(out.refused)
	refused, refusedDropped := t.refusedReads, t.refusedDropped
	t.refusedReads, t.refusedDropped = nil, 0
	return t.bound(shimwire.LauncherVolumeReport{
		Trimmer:             t.id,
		Episode:             t.episode,
		RaisedBy:            t.raisedBy,
		UsedBytes:           int64(out.last.UsedBytes),
		FreeBytes:           int64(out.last.AvailableBytes),
		FreedBytes:          out.freed,
		StopReason:          out.reason,
		RefusedReads:        refused,
		RefusedReadsOmitted: refusedDropped,
		Sizes:               walk.Sizes,
		SizesAt:             walk.At,
		SizesPartial:        walk.Partial,
		SizesMissing:        walk.Missing,
		Faults:              faults,
		FaultsOmitted:       dropped,
	})
}

// What one report may carry. Every list and every string below is the pod's own and not this
// package's: a directory name two levels under /legion is whatever a role made, and
// /legion/scratch is writable by the role uid (measured in an issue pod of this deployment at
// 2026-11-11T08:05Z: `drwx--S--- legion legion /legion/scratch`, the role process is uid 1000,
// and `test -w /legion/scratch` succeeds). So the size of the frame must be one this package
// chose rather than one the pod's roles happened to make, as Observation.PodEnd is bounded
// where it is built (runtime.PodEndRunes).
//
// The bound is here, where the report is assembled, so the frame can never reach the writer's
// own refusal: a line over shimwire.MaxFrameBytes is refused by the writer, deliver then holds
// that report, flush tries the held one first and keeps it on failure, and every later report of
// that hour would carry the same oversized walk — the trimmer would go permanently silent on
// exactly the pod the notice exists to describe.
const (
	// volumeReportPathRunes bounds each path of Sizes and of SizesMissing. The longest this
	// layout makes is `/legion/scratch/bun`, and the paths a reader acts on are of that order,
	// while one directory component may be 255 bytes and a role may make it: a few hundred
	// runes keeps every real path whole and cuts a made-up one.
	volumeReportPathRunes = 200
	// volumeReportSizes is how many measured directories a report carries. The walk measures
	// every directory two levels under the volume root in a deliberate order — the scratch
	// children, then the go children, then the rest (volumewalk.go) — so the first of them are
	// the ones a reader acts on, and a pod's own layout makes about fifteen. Fifty is room for
	// a layout that grows and a cut for the thousands a role can make under /legion/scratch.
	volumeReportSizes = 50
	// volumeReportSizesMissing is how many unmeasured directories a report names. The missing
	// list is the same population as the one above — the walk's own targets, from where its
	// minute ran out — so it takes the same bound.
	volumeReportSizesMissing = 50
	// volumeReportRefusedReads is how many refused /proc reads a report names, one per process
	// of the pod whose /proc the kernel refused. A pod runs six roles and their children, and
	// an operator who must go and look needs a handful of pids rather than one per process.
	volumeReportRefusedReads = 25
	// volumeReportStopReasonRunes bounds the sweep's own reason, which embeds the joined text
	// of every refused /proc read. It is the same few hundred runes runtime.PodEndRunes gives a
	// kubelet's reason and message, and for the same reason: the sentence is stored in an outbox
	// row and published to a role topic.
	volumeReportStopReasonRunes = 400
	// volumeReportFaults is how many recovered panics a report carries, the newest ones. Three
	// in a row is what stops the trimmer (volumeFaultLimit), so the newest three are the run
	// that stopped it, and an older one has already been reported or was separated by an
	// interval that worked.
	volumeReportFaults = volumeFaultLimit
	// volumeReportFaultRunes bounds each fault. A fault is a recovered panic with the whole
	// stack of this goroutine behind it, and the frames that name where it fell over are the
	// first of them: two thousand runes is the trimmer's own frames and its caller's.
	volumeReportFaultRunes = 2000
	// volumeReportTrimmerRunes bounds the two strings this package mints rather than reads off
	// the pod: the trimmer's identifier, which randomID writes as 32 hex characters, and what
	// raised the episode, which is one of shimwire's three words. Neither can be long today, so
	// the cut changes no value this package writes; it is here so that every string of the
	// report leaves this launcher under a bound this package chose, as the pod's own strings do.
	// It is no protection of the sentence the architect's session reads, and is not meant as
	// one: a bound the launcher applies runs only where the sender is this launcher. That
	// sentence is protected where it is built, by the daemon's own quoting
	// (internal/workflow's volumeNoticeReason), and its size by the frame's own line limit
	// (shimwire.MaxFrameBytes), since the frame's rule checks only that the identifier is present
	// and the episode at least 1 (shimwire.LauncherVolumeReport.Validate).
	volumeReportTrimmerRunes = 100
)

// bound is the report as the bound above lets it be carried: every list cut to its cap with the
// report saying how many entries it left out, and every string cut in runes with an ellipsis, as
// runtime.PodEnd cuts — the strings the pod chose, and the two this package mints beside them
// (volumeReportTrimmerRunes). It is the one place either report is bounded — a sweep's (report)
// and the switched-off trimmer's (switchOff) — and it copies rather than cutting in place,
// because the sizes belong to the walk the trimmer keeps for the hour.
//
// One line says what it cut, so a person reading the pod's log knows the report the daemon got
// is not the whole of what the trimmer measured.
func (t *trimmer) bound(report shimwire.LauncherVolumeReport) shimwire.LauncherVolumeReport {
	sizes := report.Sizes
	if over := len(sizes) - volumeReportSizes; over > 0 {
		sizes, report.SizesOmitted = sizes[:volumeReportSizes], over
	}
	report.Sizes = nil
	if len(sizes) > 0 {
		report.Sizes = make([]shimwire.LauncherVolumeSize, len(sizes))
		for at, size := range sizes {
			size.Path = cutRunes(size.Path, volumeReportPathRunes)
			report.Sizes[at] = size
		}
	}

	missing := report.SizesMissing
	if over := len(missing) - volumeReportSizesMissing; over > 0 {
		missing, report.SizesMissingOmitted = missing[:volumeReportSizesMissing], over
	}
	report.SizesMissing = cutEach(missing, volumeReportPathRunes)

	refused := report.RefusedReads
	if over := len(refused) - volumeReportRefusedReads; over > 0 {
		refused, report.RefusedReadsOmitted = refused[:volumeReportRefusedReads], report.RefusedReadsOmitted+over
	}
	report.RefusedReads = cutEach(refused, volumeReportPathRunes)

	report.StopReason = cutRunes(report.StopReason, volumeReportStopReasonRunes)
	report.Trimmer = cutRunes(report.Trimmer, volumeReportTrimmerRunes)
	report.RaisedBy = cutRunes(report.RaisedBy, volumeReportTrimmerRunes)

	faults := report.Faults
	if over := len(faults) - volumeReportFaults; over > 0 {
		// The newest, where the two lists above keep their first: a list of targets is in the
		// order a reader acts on (volumewalk.go's targets), while of the faults it is the run
		// that stopped the trimmer that says why it stopped.
		faults, report.FaultsOmitted = faults[over:], report.FaultsOmitted+over
	}
	report.Faults = cutEach(faults, volumeReportFaultRunes)

	var cuts []string
	for _, cut := range []struct {
		omitted int
		what    string
	}{
		{report.SizesOmitted, "measured directories"},
		{report.SizesMissingOmitted, "unmeasured directories"},
		{report.RefusedReadsOmitted, "refused /proc reads"},
		{report.FaultsOmitted, "recovered faults"},
	} {
		if cut.omitted > 0 {
			cuts = append(cuts, fmt.Sprintf("%d %s", cut.omitted, cut.what))
		}
	}
	if len(cuts) > 0 {
		t.logf(volumeTrimLog+"the report the daemon is sent leaves out %s, and every path, reason and stack in it is cut at this launcher's own bound: a frame over %d bytes is refused by the connection",
			strings.Join(cuts, ", "), shimwire.MaxFrameBytes)
	}
	return report
}
