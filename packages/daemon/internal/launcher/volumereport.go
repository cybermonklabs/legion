package launcher

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/redact"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// The trimmer's report half: the report a sweep or the switched-off trimmer makes, the bound it is
// carried under, and its delivery on the pod's bound connection, held while no connection takes
// it. The trimmer that makes them is volumetrim.go's.

// flush tries the report the last attempt could not send. A report the daemon never got is a
// volume nobody was told about, so the trimmer keeps it rather than dropping it. A held report
// that fails again stays held for the next attempt, unless the report deliver tries next fails too
// and replaces it, carrying what only the held one carries (carryHeld).
//
// The resend is a sweep's work wherever it is tried, before an interval's reading (tick), in the
// wait for settings (awaitSettings) or before a new report's send (deliver): a held report is a
// sweep's report, and its resend is that report's delivery. So a panic in it adds to the sweep
// row (faultRow), and a held report whose resend panics at every interval switches the trimmer
// off at its third panic, as a report whose send panics every time does; the work it was tried
// in goes on as that work's once it returns. The switched-off trimmer resends nothing here:
// switchOff takes a held report into its own last report first, so deliver's flush finds none.
func (t *trimmer) flush() {
	if t.held == nil {
		return
	}
	was := t.doing
	t.doing = sweepRow
	if err := t.send(*t.held); err != nil {
		t.logf(volumeTrimLog+"the held report could not be sent either (%v); it stays held for the next attempt, unless a newer report that cannot be sent either replaces it", err)
	} else {
		t.held = nil
	}
	t.doing = was
}

// deliver sends one report, holding it when the connection refuses it. A report made while an
// older one is still held replaces it once the older one has been tried and the newer one has
// failed too: the daemon counts episodes and not reports, and of two reports of the same episode
// the newer reading is the one worth carrying. What only the older one carries — the faults and
// the refused /proc reads the trimmer handed it and cleared from itself (report, switchOff) — is
// carried into the newer one first, and the bound that follows keeps the newest
// shimwire.VolumeReportFaults faults and the first shimwire.VolumeReportRefusedReads refused
// reads and counts the rest, so the replacement drops none of it without counting it: R1's
// fault and R3's refused read reach the next report sent, kept or counted. Held is replaced only
// once that bounded replacement exists (replace). A newer report that is sent leaves the older one
// held, to be tried at the next attempt with its own faults and refused reads.
//
// A report that is neither sent nor held gives what it carries back to the trimmer (takeBack),
// which a panic in the flush of the held report, in the send or in the making of the replacement
// leaves it, as that panic goes on to the recovery that counts it (tick, run): report and
// switchOff took those faults and refused reads from the trimmer once the report was bounded, so
// nothing else would carry them. A report that was sent or is held carries them already and gives
// nothing back: once the send has answered nil, or the report or its replacement is held, a panic
// in the line that says so or in saidLeftOut leaves them where they are. The switched-off
// trimmer's report goes through here under the same rule, with nothing held by then: switchOff
// has taken a held report into it (replace), so the flush below tries no report before it.
//
// For each report it is given it writes at most one line of what the bound left out, of the
// report as it is sent or held (saidLeftOut): a report that replaces a held one is bounded
// twice, once where it is made and once with the held one's faults and refused reads in it, and
// its line is written once, with the counts of the second.
func (t *trimmer) deliver(report shimwire.LauncherVolumeReport) {
	carried := false // the report was sent, or it or its replacement is held
	defer func() {
		if !carried {
			t.takeBack(report)
		}
	}()
	t.flush()
	err := t.send(report)
	carried = err == nil
	kept := report
	switch {
	case err == nil:
	case t.held == nil:
		t.held, carried = &kept, true
		t.logf(volumeTrimLog+"the report could not be sent (%v); holding it for the next attempt", err)
	default:
		held := *t.held
		kept = t.replace(held, report)
		t.held, carried = &kept, true
		t.logf(volumeTrimLog+"the report could not be sent (%v); it replaces the held report, taking in that report's %d recovered faults and %d refused /proc reads with its own, each kept or counted under the report's bound, and is held for the next attempt",
			err, len(held.Faults)+held.FaultsOmitted, len(held.RefusedReads)+held.RefusedReadsOmitted)
	}
	t.saidLeftOut(kept)
}

// replace is the report that takes held's place (deliver, switchOff): report with what held
// carries put into it, bounded again (carryHeld, bound). If making it panics, held stays held: in
// deliver, which gives what report carried back to the trimmer (takeBack), since report is then
// neither sent nor held, and in switchOff, whose panic ends the trimmer (run).
func (t *trimmer) replace(held, report shimwire.LauncherVolumeReport) shimwire.LauncherVolumeReport {
	return t.boundReport(carryHeld(held, report))
}

// takeBack puts what a report that was neither sent nor held carried back on the trimmer, for the
// next report to carry (deliver): its faults before the trimmer's own, since they are older, under
// the cap fault keeps them to, and its refused reads by carryRefused's rule, with what the report
// counted as left out of each added to the trimmer's count of it.
func (t *trimmer) takeBack(report shimwire.LauncherVolumeReport) {
	t.faults = append(slices.Clone(report.Faults), t.faults...)
	t.faultsDropped += report.FaultsOmitted
	t.keepNewestFaults()
	t.refusedDropped += report.RefusedReadsOmitted
	t.carryRefused(report.RefusedReads)
}

// carryHeld is report with the faults and the refused /proc reads of held put into it, before
// report replaces held (deliver, switchOff): held's faults before report's own, since they are
// older and the bound keeps the newest, and held's refused reads before report's own, a refusal
// already carried word for word not carried twice, as carryRefused carries them. The Omitted
// counts add up, and the bound that follows cuts the lists to their caps and counts what it leaves
// out.
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
// last the daemon hears from it until the launcher restarts. run calls it once the third fault in
// a row of one kind of work is counted (fault, faultRow), outside any deferred handler, and writes
// the line that says the trimmer is off and which row stopped it once this report has been sent,
// held or has panicked, so the report goes first. It carries no sizes, because the volume may be
// perfectly healthy and a size walk would cost a minute of the pod for a number nobody asked for.
// It goes through deliver, so a connection that cannot take it holds it as any other report is
// held, and run sends it once the connection is up again (deliverLast): R2 has the trimmer hold
// its report until that connection is up again, and this report is the one that tells the daemon
// the pod's volume is no longer guarded. A panic here before deliver holds this report, or one in
// deliverLast but of the line of a failed attempt, which deliverLast drops and tries the report
// again, ends the trimmer, and this report is not tried again (run). One once deliver holds it, of
// the line that says so or of saidLeftOut's, which only the logger can raise, comes after the
// send has answered and loses nothing: run writes it and still tries the held report
// (deliverLast), as it does where the line that says the trimmer is off panics, which ends
// nothing either, since run drops it. One in deliver gives what the report carried back to the
// trimmer under deliver's rule, as any report's panic does, and nothing carries it further: the
// trimmer makes no report after this one.
//
// A report the trimmer still holds goes into this one before deliver is given it, as a newer
// report that cannot be sent takes a held one's place (replace), and is held no more. So the
// switched-off trimmer sends no report before this one and tries no other after it: deliver's
// flush finds nothing to resend, which leaves no resend to panic before this report is tried —
// a held report whose resend panics at every interval is what switches the trimmer off in the
// sweep row — and what deliverLast tries is this report or nothing. The held report's sizes,
// readings and stop reason are not sent, as they are not where a newer report replaces it
// (deliver).
//
// It carries the faults no report has carried yet, the newest shimwire.VolumeReportFaults of them
// (keepNewestFaults), and the refused /proc reads no report has carried yet, as a sweep's report
// does, and takes them from the trimmer at the same moment (report): it is the last report there
// is to carry them. A held report's faults and refused reads go into it before its own, each kept
// or counted under the bound (carryHeld, bound). The faults end with the one that switched the
// trimmer off, and either of the two before it can be of the other row, since a panic of one
// kind neither ends the other's row nor adds to it (fault): two sweep panics, two reading
// panics, and then a reading run to its end whose sweep panics switch the trimmer off in the
// sweep row with the two reading panics among the newest three.
func (t *trimmer) switchOff() {
	row, _ := t.switchedOffBy()
	report := shimwire.LauncherVolumeReport{
		Trimmer:             t.id,
		Episode:             max(t.episode, 1),
		RaisedBy:            t.raisedBy,
		UsedBytes:           int64(t.last.UsedBytes),
		FreeBytes:           int64(t.last.AvailableBytes),
		StopReason:          fmt.Sprintf("the trimmer switched itself off after %d panics in a row of %s", t.inARow[row], row),
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
	report = t.boundReport(report)
	t.faults, t.faultsDropped = nil, 0
	t.refusedReads, t.refusedDropped = nil, 0
	if t.held != nil {
		report = t.replace(*t.held, report)
		t.held = nil
	}
	t.deliver(report)
}

// deliverLast is the switched-off trimmer's one remaining duty: the report switchOff could not
// send, tried once every volumeSettingsWait until a connection takes it. It ends when the report
// is sent, when the container stops, and on an answer with no settings, which discards it as R2
// has such an answer discard any report the trimmer was holding: that daemon has no case for a
// report (manager.sendVolumeReport).
//
// A failed attempt is written when its reason changes and not once per attempt, for the reason
// a refusal is (news): a connection that is down answers the same way every ten seconds for as
// long as it is down. Its line is written inside contain, so a panic of it, which only the logger
// can raise, is dropped and the report stays held for the next attempt, where one left to run
// would end the trimmer with the report held and never sent; the reason is remembered only once
// that line is written, so a line that panicked is written at the next attempt that fails. A
// panic anywhere else here ends the trimmer (run), one of the line that the report was sent or
// discarded included, which loses nothing: the report was sent, or R2 discards it.
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
			if why := err.Error(); t.news(why) {
				contain(func() {
					t.logf(volumeTrimLog+"the last report of the switched-off trimmer could not be sent (%v); holding it for the next attempt", err)
					t.said = why
				})
			}
			continue
		}
		t.held = nil
		t.logf(volumeTrimLog + "the last report of the switched-off trimmer was sent")
		return
	}
}

// carryRefused keeps refused /proc reads on the trimmer for the next report it sends (report,
// switchOff): each a sweep meets, put here the moment the sweep meets it (sweep), so no panic
// after it, in the rest of the sweep, in its interval or in the making of its report, can lose it,
// and those of a report that was neither sent nor held (takeBack). A sweep's refusal reaches it
// once, from where the sweep met it, and nothing after the sweep hands it on again, so a refusal
// past the bound below is counted once for each sweep that meets it: a refusal counted is not
// kept, so a later sweep that meets it again counts it again. R3 has a refused read carried into
// the next report, because a process the trimmer cannot read may be using anything. A refusal
// already carried, word for word, is not carried twice, and past the most a report names
// (shimwire.VolumeReportRefusedReads) a refusal is counted rather than kept, so a pod whose sweeps
// keep meeting one unreadable process the same way holds one line for it, or, where the refusal
// is past the bound, one more in the count for each such sweep, and a pod that never reports holds
// no more than the bound for the life of the launcher process.
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
// with the time it was read (sizes). The sweep's refused /proc reads are on the trimmer already,
// with those it carried since the last report, put there as the sweep met each (carryRefused).
// report takes them and the faults the trimmer recovered once the bounded report that holds them
// exists, so a panic before that, in the walk or the bound, leaves them with the trimmer for the
// next report beside that panic's own fault (tick). It takes them before the send, whether or not
// that gets across: a held report carries them, so holding them twice would report them twice,
// and a report neither sent nor held gives them back (deliver).
func (t *trimmer) report(ctx context.Context, out sweepOutcome) shimwire.LauncherVolumeReport {
	walk := t.sizes(ctx)
	report := t.boundReport(shimwire.LauncherVolumeReport{
		Trimmer:             t.id,
		Episode:             t.episode,
		RaisedBy:            t.raisedBy,
		UsedBytes:           int64(out.last.UsedBytes),
		FreeBytes:           int64(out.last.AvailableBytes),
		FreedBytes:          out.freed,
		StopReason:          out.reason,
		RefusedReads:        t.refusedReads,
		RefusedReadsOmitted: t.refusedDropped,
		Sizes:               walk.Sizes,
		SizesOmitted:        walk.SizesOmitted,
		SizesAt:             walk.At,
		SizesPartial:        walk.Partial,
		SizesMissing:        walk.Missing,
		SizesMissingOmitted: walk.MissingOmitted,
		Faults:              t.faults,
		FaultsOmitted:       t.faultsDropped,
	})
	t.faults, t.faultsDropped = nil, 0
	t.refusedReads, t.refusedDropped = nil, 0
	return report
}

// What one report may carry. Every path in the lists below is the pod's own and not this
// package's: a name two levels under /legion, a directory's or a file's, is whatever a role
// made, and /legion/scratch is writable by the role uid (measured in an issue pod of this
// deployment at 2026-11-11T08:05Z: `drwx--S--- legion legion /legion/scratch`, the role process
// is uid 1000, and `test -w /legion/scratch` succeeds). So the size of the frame must be one this
// package chose rather than one the pod's roles happened to make, as Observation.PodEnd is
// bounded where it is built (runtime.PodEndRunes). How many entries each list may hold is
// shimwire's (shimwire.VolumeReportSizes and the three beside it), which the daemon holds a
// report to as well (shimwire.LauncherVolumeReport.Validate); the runes each string may hold are
// below.
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
	// kubelet's reason and message, and for a like reason: that bound is there because a
	// worker-died's reason names the pod end whole on its one line (internal/workflow's
	// deathReason), and the volume-near-full notice's sentence quotes this reason whole on its
	// one line (internal/workflow's volumeNoticeReason).
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
// refused reads (deliver, switchOff, carryHeld); it copies rather than cutting in place, because
// the sizes belong to the walk the trimmer keeps for the hour. A walk keeps no more sizes than the
// cap already (walkResult.keep), and a list of sizes past it is cut by that same rule, the largest
// kept in the order they came.
func (t *trimmer) bound(report shimwire.LauncherVolumeReport) shimwire.LauncherVolumeReport {
	sizes := report.Sizes
	if len(sizes) > shimwire.VolumeReportSizes {
		var largest walkResult
		for _, size := range sizes {
			largest.keep(size)
		}
		sizes, report.SizesOmitted = largest.Sizes, report.SizesOmitted+largest.SizesOmitted
	}
	report.Sizes = nil
	if len(sizes) > 0 {
		report.Sizes = make([]shimwire.LauncherVolumeSize, len(sizes))
		for at, size := range sizes {
			size.Path = redact.Runes(size.Path, volumeReportPathRunes)
			size.Refused = redact.Runes(size.Refused, volumeReportPathRunes)
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
				Path:   redact.Runes(one.Path, volumeReportPathRunes),
				Reason: redact.Runes(one.Reason, volumeReportPathRunes),
			}
		}
	}

	refused := report.RefusedReads
	if over := len(refused) - shimwire.VolumeReportRefusedReads; over > 0 {
		refused, report.RefusedReadsOmitted = refused[:shimwire.VolumeReportRefusedReads], report.RefusedReadsOmitted+over
	}
	report.RefusedReads = cutEach(refused, volumeReportPathRunes)

	report.StopReason = redact.Runes(report.StopReason, volumeReportStopReasonRunes)
	report.Trimmer = redact.Runes(report.Trimmer, volumeReportTrimmerRunes)
	report.RaisedBy = redact.Runes(report.RaisedBy, volumeReportTrimmerRunes)

	faults := report.Faults
	if over := len(faults) - shimwire.VolumeReportFaults; over > 0 {
		// The newest, where the two lists above keep their first: the walk's unmeasured
		// paths are in the order a reader acts on (volumewalk.go's targets) and the
		// refused reads in the order they were met, while of the faults it is the newest
		// that say why the trimmer stopped: the newest three of both rows, which end with
		// the panic that switched it off (switchOff).
		faults, report.FaultsOmitted = faults[over:], report.FaultsOmitted+over
	}
	report.Faults = cutEach(faults, volumeReportFaultRunes)
	return report
}

// saidLeftOut writes the one line of what a report leaves out, so a person reading the pod's log
// knows the report the daemon got is not the whole of what the trimmer measured. deliver writes
// it once for each report it is given, of the report as it is sent or held.
func (t *trimmer) saidLeftOut(report shimwire.LauncherVolumeReport) {
	var cuts []string
	for _, cut := range []struct {
		omitted int
		what    string
	}{
		{report.SizesOmitted, "measured sizes"},
		{report.SizesMissingOmitted, "unmeasured paths"},
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
}
