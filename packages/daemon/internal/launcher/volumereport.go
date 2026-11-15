package launcher

import (
	"cmp"
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
// bundleRestore keeps its own confirmation across every connection of the pod (restore.go). A
// held report that fails again stays held for the next attempt, unless the report deliver tries
// next fails too and replaces it, carrying what only the held one carries (carryHeld).
func (t *trimmer) flush() {
	if t.held == nil {
		return
	}
	if err := t.send(*t.held); err != nil {
		t.logf(volumeTrimLog+"the held report could not be sent either (%v); it stays held for the next attempt, unless a newer report that cannot be sent either replaces it", err)
		return
	}
	t.held = nil
}

// deliver sends one report, holding it when the connection refuses it. A report made while an
// older one is still held replaces it once the older one has been tried and the newer one has
// failed too: the daemon counts episodes and not reports, and of two reports of the same episode
// the newer reading is the one worth carrying. What only the older one carries — the faults and
// the refused /proc reads the trimmer handed it and cleared from itself (report, switchOff) — is
// carried into the newer one first, so the replacement loses none of it and R1's fault and R3's
// refused read still reach the next report sent. A newer report that is sent leaves the older
// one held, to be tried at the next attempt with its own faults and refused reads.
func (t *trimmer) deliver(report shimwire.LauncherVolumeReport) {
	t.flush()
	err := t.send(report)
	switch {
	case err == nil:
		return
	case t.held == nil:
		t.logf(volumeTrimLog+"the report could not be sent (%v); holding it for the next attempt", err)
	default:
		held := *t.held
		report = t.bound(carryHeld(held, report))
		t.logf(volumeTrimLog+"the report could not be sent (%v); it replaces the held report, carrying that report's %d recovered faults and %d refused /proc reads with its own, and is held for the next attempt",
			err, len(held.Faults)+held.FaultsOmitted, len(held.RefusedReads)+held.RefusedReadsOmitted)
	}
	t.held = &report
}

// carryHeld is report with the faults and the refused /proc reads of held put into it, before
// report replaces held (deliver): held's faults before report's own, since they are older and
// the bound keeps the newest, and held's refused reads before report's own, a refusal already
// carried word for word not carried twice, as carryRefused carries them. The Omitted counts add
// up, and the bound that follows cuts the lists to their caps and counts what it leaves out.
func carryHeld(held, report shimwire.LauncherVolumeReport) shimwire.LauncherVolumeReport {
	report.Faults = append(slices.Clone(held.Faults), report.Faults...)
	report.FaultsOmitted += held.FaultsOmitted
	refused := slices.Clone(held.RefusedReads)
	for _, one := range report.RefusedReads {
		if !slices.Contains(refused, one) {
			refused = append(refused, one)
		}
	}
	report.RefusedReads = refused
	report.RefusedReadsOmitted += held.RefusedReadsOmitted
	return report
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
// because a process the trimmer cannot read may be using anything. A refusal already carried,
// word for word, is not carried twice, and past the most a report names
// (shimwire.VolumeReportRefusedReads) a refusal is counted rather than kept, so a pod whose
// sweeps keep meeting one unreadable process the same way holds one line for it, and a pod that
// never reports holds no more than the bound for the life of the launcher process.
func (t *trimmer) carryRefused(refused []string) {
	for _, one := range refused {
		switch {
		case slices.Contains(t.refusedReads, one):
		case len(t.refusedReads) < shimwire.VolumeReportRefusedReads:
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
		SizesMissingOmitted: walk.MissingOmitted,
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
// where it is built (runtime.PodEndRunes). How many entries each list may hold is shimwire's
// (shimwire.VolumeReportSizes and the three beside it), which the daemon holds a report to as
// well (shimwire.LauncherVolumeReport.Validate); the runes each string may hold are below.
//
// The bound is here, where the report is assembled, so the frame can never reach the writer's
// own refusal: a line over shimwire.MaxFrameBytes is refused by the writer, deliver then holds
// that report, flush tries the held one first and keeps it on failure, and every later report of
// that hour would carry the same oversized walk — the trimmer would go permanently silent on
// exactly the pod the notice exists to describe.
const (
	// volumeReportPathRunes bounds each path of Sizes, the read each names as refused
	// (LauncherVolumeSize.Refused), each path of SizesMissing and its reason, and each refused
	// /proc read. Of the directories this layout makes two levels under the volume root, the
	// fixed ones are at most 26 runes (`/legion/scratch/leak-check`) and a repository owner's,
	// under `/legion/workspaces` or `/legion/feed`, at most 58 under GitHub's 39-character owner,
	// and the paths a reader acts on are of that order, while one directory component may be 255
	// bytes and a role may make it: a few hundred runes keeps every real path whole and cuts a
	// made-up one. A reason takes the same bound (unreadReason, volumewalk.go).
	volumeReportPathRunes = 200
	// volumeReportStopReasonRunes bounds the sweep's own reason, which names at most the first
	// shimwire.VolumeReportRefusedReads refused /proc reads and how many more there were
	// (refusalsSaid, volumetrim.go). It is the same few hundred runes runtime.PodEndRunes gives a
	// kubelet's reason and message, and for the same reason: the sentence is stored in an outbox
	// row and published to a role topic.
	volumeReportStopReasonRunes = 400
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
	// (shimwire.MaxFrameBytes) and the lists' caps, since the frame's rule checks the identifier,
	// the episode and how many entries each list holds, and no string's runes
	// (shimwire.LauncherVolumeReport.Validate).
	volumeReportTrimmerRunes = 100
)

// bound is the report as the bound above lets it be carried: every list cut to its cap with the
// report saying how many entries it left out, and every string cut in runes with an ellipsis, as
// runtime.PodEnd cuts — the strings the pod chose, and the two this package mints beside them
// (volumeReportTrimmerRunes). It bounds both reports — a sweep's (report) and the switched-off
// trimmer's (switchOff) — and again a report that replaced a held one and took on its faults and
// refused reads (deliver, carryHeld); it copies rather than cutting in place, because the sizes
// belong to the walk the trimmer keeps for the hour.
//
// One line says what it cut, so a person reading the pod's log knows the report the daemon got
// is not the whole of what the trimmer measured.
func (t *trimmer) bound(report shimwire.LauncherVolumeReport) shimwire.LauncherVolumeReport {
	sizes := report.Sizes
	if over := len(sizes) - shimwire.VolumeReportSizes; over > 0 {
		sizes, report.SizesOmitted = largestSizes(sizes, shimwire.VolumeReportSizes), report.SizesOmitted+over
	}
	report.Sizes = nil
	if len(sizes) > 0 {
		report.Sizes = make([]shimwire.LauncherVolumeSize, len(sizes))
		for at, size := range sizes {
			size.Path = cutRunes(size.Path, volumeReportPathRunes)
			size.Refused = cutRunes(size.Refused, volumeReportPathRunes)
			report.Sizes[at] = size
		}
	}

	missing := report.SizesMissing
	if over := len(missing) - shimwire.VolumeReportSizesMissing; over > 0 {
		missing, report.SizesMissingOmitted = missing[:shimwire.VolumeReportSizesMissing], report.SizesMissingOmitted+over
	}
	report.SizesMissing = nil
	if len(missing) > 0 {
		report.SizesMissing = make([]shimwire.LauncherVolumeMissing, len(missing))
		for at, one := range missing {
			report.SizesMissing[at] = shimwire.LauncherVolumeMissing{
				Path:   cutRunes(one.Path, volumeReportPathRunes),
				Reason: cutRunes(one.Reason, volumeReportPathRunes),
			}
		}
	}

	refused := report.RefusedReads
	if over := len(refused) - shimwire.VolumeReportRefusedReads; over > 0 {
		refused, report.RefusedReadsOmitted = refused[:shimwire.VolumeReportRefusedReads], report.RefusedReadsOmitted+over
	}
	report.RefusedReads = cutEach(refused, volumeReportPathRunes)

	report.StopReason = cutRunes(report.StopReason, volumeReportStopReasonRunes)
	report.Trimmer = cutRunes(report.Trimmer, volumeReportTrimmerRunes)
	report.RaisedBy = cutRunes(report.RaisedBy, volumeReportTrimmerRunes)

	faults := report.Faults
	if over := len(faults) - shimwire.VolumeReportFaults; over > 0 {
		// The newest, where the two lists above keep their first: the walk's unmeasured
		// directories are in the order a reader acts on (volumewalk.go's targets) and the
		// refused reads in the order they were met, while of the faults it is the run that
		// stopped the trimmer that says why it stopped.
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

// largestSizes is the keep sizes of the most bytes, in the order the walk measured them, where
// sizes holds more than keep: a size the walk reached late is a size like any other to a reader
// looking for what filled the volume, and a role that makes fifty empty directories under
// /legion/scratch, which the walk measures before /legion/go, must not push the build cache out
// of the report. Of sizes of equal bytes the one measured first is kept.
func largestSizes(sizes []shimwire.LauncherVolumeSize, keep int) []shimwire.LauncherVolumeSize {
	order := make([]int, len(sizes))
	for at := range order {
		order[at] = at
	}
	slices.SortStableFunc(order, func(a, b int) int { return cmp.Compare(sizes[b].Bytes, sizes[a].Bytes) })
	kept := order[:keep]
	slices.Sort(kept)
	out := make([]shimwire.LauncherVolumeSize, keep)
	for at, from := range kept {
		out[at] = sizes[from]
	}
	return out
}
