package launcher

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// The pod's cache trimmer: the interval loop that reads the volume, the sweep that deletes the
// oldest entries of the four reclaimable roots, the episode number a report is counted under,
// and the report itself. It runs in the launcher process, which is PID 1 of an issue pod's
// roles container, beside the six roles whose caches fill the one 50 GiB volume they share
// (INFRA-2009: a pod filled 45 GiB of 49, and a role's work was deleted by hand mid-round).
//
// Everything the trimmer reads of the machine is a field or a package-level var, so a test
// drives a tree, a /proc and a clock of its own: the volume reading is readVolumeUsage
// (volumeusage_linux.go), and the rest are the fields of the trimmer below.

// volumeTrimLog begins every line the trimmer writes. The lines are what a person reads in the
// pod after a deploy — the launcher's log is the only place this work is visible, since the
// daemon hears nothing but a report of a sweep that ended above the low mark — so they are all
// behind one prefix a `grep` can take.
const volumeTrimLog = "volume trim: "

const (
	// volumeSweepBudget bounds one sweep from its start, the listing included, because on a
	// near-full volume the listing is the larger half of the work: it is one newestModTime per
	// entry, which for a `go-build` tree or a bun cache entry is a walk of the whole tree, and
	// the pod measured 11,331 `-a` names in GOCACHE alone (volumepaths.go). A bound that began
	// counting at the first deletion would let the listing run for as long as it liked, and the
	// sweep holds nothing back while it runs: the roles keep writing into the roots it is
	// listing, so a listing that overran would be deciding on a volume that has moved on.
	volumeSweepBudget = 5 * time.Minute
	// volumeSweepReadEvery is how many deletions pass between two readings of the volume. The
	// progress of a sweep is read from the volume and never counted from the bytes unlinked
	// (sweep), and a statfs is a syscall against a hundred unlinks, so reading it per deletion
	// would make the measurement a measurable part of the sweep's own cost.
	volumeSweepReadEvery = 100
	// volumeSweepStallReadings is how many readings in a row showing no decrease end a sweep.
	// One is not enough: the kernel accounts a deletion asynchronously, so a single reading can
	// miss a hundred unlinks that did free space. Three in a row over three hundred deletions
	// is a volume whose freed space is being taken by the roles as fast as the sweep frees it,
	// or one whose entries are hardlinked elsewhere (bun), and in both cases deleting more of
	// the cache buys the pod nothing.
	volumeSweepStallReadings = 3
	// volumeUsedPathsMaxAge is how stale the used-path set may be while a sweep runs. A sweep
	// lists thousands of entries and may run for minutes, and a tool that starts in that time —
	// a `bun install` taking its flock, a Claude Code launch — holds paths the set read at the
	// start of the sweep cannot know. One second bounds how long such a tool is unprotected
	// against one pass over /proc, which costs a read of six entries per process of the pod.
	volumeUsedPathsMaxAge = time.Second
	// volumeQuietIntervals is how many intervals pass after a sweep that freed less than
	// volumeSweepMinFreedPoints before another sweep may start. A sweep that freed nothing has
	// no eligible entry left or is losing to the roles writing, and repeating it every interval
	// would spend the listing's cost — minutes of walking the volume — for nothing, on the
	// volume that is already full. The interval reading continues through the pause, because it
	// is what ends an episode (episodeOf).
	volumeQuietIntervals = 10
	// volumeSweepMinFreedPoints is what a sweep must free, in percentage points of the volume,
	// not to engage the pause above.
	volumeSweepMinFreedPoints = 1.0
	// volumeRisePoints is how far above an episode's anchor a reading must rise to raise the
	// episode number (episodeOf).
	volumeRisePoints = 5.0
	// volumeFaultLimit is how many recovered panics in a row stop the trimmer for the life of
	// the launcher process.
	volumeFaultLimit = 3
)

// The ranges the trimmer holds the daemon's settings to, a second time on its own side of the
// wire. The daemon validates them too, but it is another binary across a connection this design
// expects to carry version skew both ways, and the cost of believing a bad one here is paid by
// the whole pod: time.NewTicker panics on a non-positive period, in the process that is PID 1 of
// the roles container with six live roles under it, and a high mark of 0 would sweep the caches
// of a volume that is empty.
const (
	volumeMarkMin     = 1
	volumeMarkMax     = 99
	volumeMinAgeMin   = 1
	volumeIntervalMin = 1
	volumeIntervalMax = 3600
)

// refusedTrimSettings is every reason the settings cannot be run, each naming the key as it is
// written on the wire and the value that was sent, and empty when they can. A trimmer that
// refuses its settings stays off rather than starting its ticker: there is no safe default to
// fall back to, since every default is a guess at how full this operator's volume may get.
func refusedTrimSettings(set shimwire.LauncherVolumeTrim) []string {
	var refused []string
	mark := func(key string, value int) {
		if value < volumeMarkMin || value > volumeMarkMax {
			refused = append(refused, fmt.Sprintf("%s is %d, want a whole number from %d to %d", key, value, volumeMarkMin, volumeMarkMax))
		}
	}
	mark("high_percent", set.HighPercent)
	mark("low_percent", set.LowPercent)
	if set.LowPercent >= set.HighPercent {
		refused = append(refused, fmt.Sprintf("low_percent is %d and high_percent is %d, want the low mark below the high one", set.LowPercent, set.HighPercent))
	}
	if set.MinAgeMinutes < volumeMinAgeMin {
		refused = append(refused, fmt.Sprintf("min_age_minutes is %d, want at least %d", set.MinAgeMinutes, volumeMinAgeMin))
	}
	if set.IntervalSeconds < volumeIntervalMin || set.IntervalSeconds > volumeIntervalMax {
		refused = append(refused, fmt.Sprintf("interval_seconds is %d, want a whole number from %d to %d", set.IntervalSeconds, volumeIntervalMin, volumeIntervalMax))
	}
	return refused
}

// trimmer is the pod's cache trimmer: one per launcher process.
type trimmer struct {
	// id is minted once per launcher process and kept across every reconnection: a rolling
	// daemon restart flips the bound connection repeatedly, and an identifier per connection
	// would multiply the daemon's three-notice bound.
	id string
	// settings is what is in force now, read before every interval reading. Nil stops the
	// trimmer: it ends a sweep in flight at its next entry boundary with its deletions
	// standing, sends no report, and discards a report it was holding.
	settings func() *shimwire.LauncherVolumeTrim
	// send writes one report on the pod's bound connection. An error holds the report, which
	// the trimmer sends on the next attempt, as the launcher's bundleRestore holds its own.
	send func(shimwire.LauncherVolumeReport) error
	now  func() time.Time
	// ticks is the interval clock; a test drives it. The runner takes the channel and a stop.
	// The size walk's sixty-second budget comes from it as well (sizeWalk.after), because a
	// walk that blocks inside one measurement cannot be cut by a clock a loop reads.
	ticks func(time.Duration) (<-chan time.Time, func())
	logf  func(format string, args ...any)
	// volumeRoot, procRoot, self and roots are the machine, each an argument so a test drives a
	// tree of its own: VolumeRoot, "/proc", os.Getpid() and ReclaimableRoots in the pod.
	volumeRoot, procRoot string
	self                 int
	roots                []ReclaimableRoot
	// measure is one directory of the size walk, the seam a test blocks to drive the walk's
	// budget (measureTree, volumewalk.go).
	measure func(ctx context.Context, path string, at time.Time) measured

	// Everything below is the trimmer's own state across intervals. run is one goroutine and
	// nothing else reads these, so they carry no lock: the launcher's connection goroutine
	// reaches the trimmer through settings and send alone, which are the caller's to make safe.
	episode     int     // 0 until the first reading at or above the high mark
	raisedBy    string  // which of shimwire's three raised the episode above
	aboveHigh   bool    // a reading has been at or above the high mark since the last recovery
	anchor      float64 // the highest pre-sweep reading of a sweep that reported in this episode
	anchored    bool    // whether a sweep has reported in this episode at all
	pause       int     // intervals left of the pause after a sweep that freed under a point
	last        volumeUsage
	faults      []string // recovered panics the next report carries
	consecutive int      // recovered panics in a row, which volumeFaultLimit bounds
	held        *shimwire.LauncherVolumeReport
	walk        walkResult // the last size walk, which a report between walks carries
	walked      bool
}

// newTrimmer is the pod's trimmer: the volume at VolumeRoot, the container's own /proc, the four
// reclaimable roots and the real clock, with an identifier minted for the life of this launcher
// process (randomID, as the launcher mints a connection's).
func newTrimmer(
	settings func() *shimwire.LauncherVolumeTrim,
	send func(shimwire.LauncherVolumeReport) error,
	logf func(format string, args ...any),
) *trimmer {
	return &trimmer{
		id:         randomID(),
		settings:   settings,
		send:       send,
		now:        time.Now,
		ticks:      tickerChannel,
		logf:       logf,
		volumeRoot: VolumeRoot,
		procRoot:   "/proc",
		self:       os.Getpid(),
		roots:      ReclaimableRoots,
		measure:    measureTree,
	}
}

// tickerChannel is the interval clock of the pod: one ticker, and its stop.
func tickerChannel(every time.Duration) (<-chan time.Time, func()) {
	ticker := time.NewTicker(every)
	return ticker.C, ticker.Stop
}

// run is the interval loop. It returns when ctx ends, when the settings stop the trimmer, or
// when the trimmer has switched itself off, and it starts no ticker at all on settings it
// refuses: time.NewTicker panics on a non-positive period, and this goroutine's panic is PID 1's
// panic (tick).
func (t *trimmer) run(ctx context.Context) {
	set := t.settings()
	if set == nil {
		t.logf(volumeTrimLog + "no settings are in force, so the trimmer stays off")
		return
	}
	if refusals := refusedTrimSettings(*set); len(refusals) > 0 {
		t.refuse(refusals)
		return
	}
	inForce := *set
	t.logf(volumeTrimLog+"started: trimmer %s, high %d%%, low %d%%, minimum age %d min, interval %d s, volume %s",
		t.id, inForce.HighPercent, inForce.LowPercent, inForce.MinAgeMinutes, inForce.IntervalSeconds, t.volumeRoot)
	ticks, stop := t.ticks(settingsInterval(inForce))
	defer func() { stop() }()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
		restart, off := t.tick(ctx, &inForce)
		if off {
			return
		}
		if restart {
			// A new ticker rather than the old one's next tick, for the two reasons a sweep and
			// a settings change give: the next reading must come one interval after the sweep
			// ends and not one interval after it began, and an interval the operator shortened
			// must take effect without waiting out the old one.
			stop()
			ticks, stop = t.ticks(settingsInterval(inForce))
		}
	}
}

// refuse logs settings the trimmer will not run, one line per reason with its key and value.
func (t *trimmer) refuse(refusals []string) {
	for _, refusal := range refusals {
		t.logf(volumeTrimLog+"refusing the settings: %s", refusal)
	}
	t.logf(volumeTrimLog + "the trimmer stays off until the daemon sends settings it can run: it starts no ticker on refused settings")
}

// settingsInterval is the period of the interval clock.
func settingsInterval(set shimwire.LauncherVolumeTrim) time.Duration {
	return time.Duration(set.IntervalSeconds) * time.Second
}

// tick is one interval: the settings in force, a held report, one reading of the volume, the
// episode that reading raises, and the sweep it asks for. restart says the interval clock must
// be made again, and off that the loop must end.
//
// It recovers from a panic in its own work, which is a deliberate departure from this daemon's
// practice — no production path of it recovers a panic, because a process that cannot keep its
// invariants should die and be restarted — because this goroutine runs in the process that is
// PID 1 of the roles container: its panic takes the container down and costs six live roles
// their round, over a cache sweep that nothing depends on. The fault is logged with its stack
// and carried into the next report, and three in a row stop the trimmer for good (fault).
func (t *trimmer) tick(ctx context.Context, inForce *shimwire.LauncherVolumeTrim) (restart, off bool) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			t.consecutive = 0
			return
		}
		restart, off = false, t.fault(recovered, debug.Stack())
	}()

	next := t.settings()
	if next == nil {
		t.held = nil
		t.logf(volumeTrimLog + "the trimmer is stopped: no settings are in force, and a report it was holding is discarded")
		return false, true
	}
	if refusals := refusedTrimSettings(*next); len(refusals) > 0 {
		// The settings in force stay in force. Switching the trimmer off here would let one bad
		// frame from a daemon of another version leave the pod's volume unguarded for the life
		// of the container, where keeping the last settings the trimmer accepted leaves it
		// doing what the operator last asked for.
		t.refuse(refusals)
	} else if *next != *inForce {
		t.logf(volumeTrimLog+"new settings: high %d%%, low %d%%, minimum age %d min, interval %d s",
			next.HighPercent, next.LowPercent, next.MinAgeMinutes, next.IntervalSeconds)
		restart = next.IntervalSeconds != inForce.IntervalSeconds
		*inForce = *next
	}
	set := *inForce

	t.flush()
	reading, err := readVolumeUsage(t.volumeRoot)
	if err != nil {
		t.logf(volumeTrimLog+"the volume at %s could not be read: %v", t.volumeRoot, err)
		return restart, false
	}
	t.last = reading
	percent := reading.Percent()
	t.episodeOf(percent, set)
	switch {
	case t.pause > 0:
		t.logf(volumeTrimLog+"the volume is %.1f%% full (used %d B, free %d B): no sweep for another %d intervals, since the last one freed under %.0f point",
			percent, reading.UsedBytes, reading.AvailableBytes, t.pause, volumeSweepMinFreedPoints)
		t.pause--
		return restart, false
	case percent < float64(set.HighPercent):
		t.logf(volumeTrimLog+"the volume is %.1f%% full (used %d B, free %d B): below the high mark %d%%, nothing to sweep",
			percent, reading.UsedBytes, reading.AvailableBytes, set.HighPercent)
		return restart, false
	}
	t.logf(volumeTrimLog+"the volume is %.1f%% full (used %d B, free %d B): at or above the high mark %d%%, sweeping to %d%%",
		percent, reading.UsedBytes, reading.AvailableBytes, set.HighPercent, set.LowPercent)

	out := t.sweep(ctx, set, reading)
	t.last = out.last
	if out.settingsStop {
		t.held = nil
		t.logf(volumeTrimLog + "the trimmer is stopped: the sweep ended at an entry boundary with its deletions standing, no report is sent, and a report it was holding is discarded")
		return false, true
	}
	// The reading is written as the sweep left it rather than as another "the volume is" line,
	// which is the interval reading alone: an operator reading the pod's log counts the
	// readings to see the trimmer is alive.
	t.logf(volumeTrimLog+"the sweep ended: %s. %d entries deleted, %d B freed, leaving the volume at %.1f%% (used %d B, free %d B)",
		out.reason, out.deletions, out.freed, out.last.Percent(), out.last.UsedBytes, out.last.AvailableBytes)
	if out.freedPoints < volumeSweepMinFreedPoints {
		t.pause = volumeQuietIntervals
		t.logf(volumeTrimLog+"the sweep freed %.2f points of the volume, under %.0f: no new sweep for the next %d intervals",
			out.freedPoints, volumeSweepMinFreedPoints, volumeQuietIntervals)
	}
	if out.report {
		t.anchorAt(percent)
		t.deliver(t.report(ctx, out))
	}
	return true, false
}

// episodeOf raises the episode number on the reading, when the reading is one of the two things
// that raise it, and leaves it alone otherwise.
//
// An episode is the run of readings the daemon's three-notice bound counts against, so what
// raises it decides when the issue's architect is told again. A recovery is the volume coming
// back under the low mark after having reached the high one: the next time it fills is news. A
// rise is the volume filling five points past the worst this episode has already reported,
// which is a volume filling faster than the trimmer frees it.
//
// The rise is measured against the highest reading at the start of a sweep that reported, never
// against the figure a report carried, which is a post-sweep low point: anchored there, the
// ordinary growth from the low point back to the high mark would read as a five-point
// deterioration and wake the architect on every cycle.
func (t *trimmer) episodeOf(percent float64, set shimwire.LauncherVolumeTrim) {
	switch {
	case t.episode == 0:
		if percent < float64(set.HighPercent) {
			return
		}
		t.episode, t.raisedBy = 1, shimwire.VolumeEpisodeFirst
		t.logf(volumeTrimLog+"episode %d (%s): the volume reached the high mark %d%% at %.1f%%",
			t.episode, t.raisedBy, set.HighPercent, percent)
	case t.aboveHigh && percent < float64(set.LowPercent):
		t.episode, t.raisedBy, t.aboveHigh, t.anchored = t.episode+1, shimwire.VolumeEpisodeRecovery, false, false
		t.logf(volumeTrimLog+"episode %d (%s): the volume fell to %.1f%%, below the low mark %d%%",
			t.episode, t.raisedBy, percent, set.LowPercent)
	case t.anchored && percent >= t.anchor+volumeRisePoints:
		anchor := t.anchor
		t.episode, t.raisedBy, t.anchored = t.episode+1, shimwire.VolumeEpisodeRise, false
		t.logf(volumeTrimLog+"episode %d (%s): the volume is %.1f%%, %.0f points above the %.1f%% a reported sweep of the last episode started from",
			t.episode, t.raisedBy, percent, volumeRisePoints, anchor)
	}
	if percent >= float64(set.HighPercent) {
		t.aboveHigh = true
	}
}

// anchorAt records the reading a reported sweep started from, which the rise above is measured
// against: the highest such reading of this episode.
func (t *trimmer) anchorAt(percent float64) {
	if !t.anchored || percent > t.anchor {
		t.anchor, t.anchored = percent, true
	}
}

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

// fault records one recovered panic and answers whether the trimmer must now stay off.
func (t *trimmer) fault(recovered any, stack []byte) bool {
	t.consecutive++
	t.faults = append(t.faults, fmt.Sprintf("%v\n%s", recovered, stack))
	t.logf(volumeTrimLog+"recovered a panic in the trimmer's own work (%d in a row), passing over this interval: %v\n%s",
		t.consecutive, recovered, stack)
	if t.consecutive < volumeFaultLimit {
		return false
	}
	t.logf(volumeTrimLog+"%d panics in a row: the trimmer is off for the life of this launcher process", t.consecutive)
	t.switchOff()
	return true
}

// switchOff sends the one report no sweep makes: the trimmer has stopped itself, and this is the
// last the daemon hears from it until the launcher restarts. It carries no sizes, because the
// volume may be perfectly healthy and a size walk would cost a minute of the pod for a number
// nobody asked for; and it is sent whatever else is true, which is why it goes through send
// directly rather than through deliver, whose holding has no next attempt to be made on.
func (t *trimmer) switchOff() {
	report := shimwire.LauncherVolumeReport{
		Trimmer:     t.id,
		Episode:     max(t.episode, 1),
		RaisedBy:    t.raisedBy,
		UsedBytes:   int64(t.last.UsedBytes),
		FreeBytes:   int64(t.last.AvailableBytes),
		StopReason:  fmt.Sprintf("the trimmer switched itself off after %d panics in a row", t.consecutive),
		Faults:      t.faults,
		SwitchedOff: true,
	}
	if report.RaisedBy == "" {
		// The trimmer never saw a reading at the high mark, so no episode was ever raised; the
		// frame's own rule is that an episode is at least 1 (LauncherVolumeReport.Validate), and
		// this is the first thing the daemon hears of this trimmer.
		report.RaisedBy = shimwire.VolumeEpisodeFirst
	}
	t.flush()
	if err := t.send(report); err != nil {
		t.logf(volumeTrimLog+"the last report of the switched-off trimmer could not be sent: %v", err)
	}
	t.faults = nil
}

// report is one sweep as the daemon hears it, with the sizes of a fresh walk or of the last one
// with the time it was read (sizes). It takes the faults the trimmer recovered since the last
// report with it, whether or not the send gets across: a held report carries them, so holding
// them twice would report them twice.
func (t *trimmer) report(ctx context.Context, out sweepOutcome) shimwire.LauncherVolumeReport {
	walk := t.sizes(ctx)
	faults := t.faults
	t.faults = nil
	return shimwire.LauncherVolumeReport{
		Trimmer:      t.id,
		Episode:      t.episode,
		RaisedBy:     t.raisedBy,
		UsedBytes:    int64(out.last.UsedBytes),
		FreeBytes:    int64(out.last.AvailableBytes),
		FreedBytes:   out.freed,
		StopReason:   out.reason,
		RefusedReads: out.refused,
		Sizes:        walk.Sizes,
		SizesAt:      walk.At,
		SizesPartial: walk.Partial,
		SizesMissing: walk.Missing,
		Faults:       faults,
	}
}

// sizes is the size walk for one report: a fresh walk, or the last one when it is less than an
// hour old. The walk stats every file of the volume and is bounded at a minute (volumewalk.go),
// which is a minute of the pod's one spare process, so a pod sweeping every interval walks once
// an hour and sends the same numbers in between with the time they were read.
func (t *trimmer) sizes(ctx context.Context) walkResult {
	if t.walked && t.now().Sub(t.walk.At) < volumeWalkInterval {
		return t.walk
	}
	t.walk = sizeWalk{
		root:    t.volumeRoot,
		now:     t.now,
		after:   t.ticks,
		budget:  volumeWalkBudget,
		measure: t.measure,
	}.run(ctx)
	t.walked = true
	if t.walk.Partial {
		t.logf(volumeTrimLog+"the size walk ended early: it measured %d directories and left %d unmeasured (%s)",
			len(t.walk.Sizes), len(t.walk.Missing), strings.Join(t.walk.Missing, ", "))
	}
	return t.walk
}

// sweepOutcome is one sweep as the interval reads it.
type sweepOutcome struct {
	last        volumeUsage // the sweep's last reading, which the report carries
	freed       int64       // the first reading's used bytes less the last's
	freedPoints float64     // the same difference in percentage points of the volume
	reason      string      // why the sweep stopped, in the trimmer's own words
	refused     []string    // every /proc read the kernel refused while it ran
	deletions   int
	report      bool // the sweep ended with the volume still above the low mark
	// settingsStop is the one ending the interval must act on itself: the settings stopped the
	// trimmer mid-sweep, so the deletions stand, no report is sent and the loop ends.
	settingsStop bool
}

// sweepEntry is one eligible entry as the listing read it.
type sweepEntry struct {
	path string
	rule EntryRule
	// at is the newest modification time anywhere in the entry when it was listed, which is
	// both what the entries are ordered by and what the pre-delete re-read compares against.
	at  time.Time
	dir bool
}

// sweep deletes the oldest entries of every root, oldest first across the roots, until a fresh
// reading is at or below the low mark, no entry is eligible, or one of the three endings cuts
// it: the budget, the settings and the container's stop, each taken at the next entry boundary
// once deleting has begun, so nothing is left half removed.
//
// first is the interval's own reading, which is the sweep's first: it was taken moments ago, and
// the difference between it and the sweep's last reading is what the sweep freed. Progress is
// read from the volume and never counted from the bytes unlinked, because bun hardlinks out of
// its install cache into a workspace's node_modules, so unlinking the cached copy frees nothing
// at all while those links stand — a sweep counting bytes would empty the cache and report a
// volume it had not changed.
func (t *trimmer) sweep(ctx context.Context, set shimwire.LauncherVolumeTrim, first volumeUsage) sweepOutcome {
	start := t.now()
	deadline := start.Add(volumeSweepBudget)
	cutoff := start.Add(-time.Duration(set.MinAgeMinutes) * time.Minute)
	out := sweepOutcome{last: first}

	used := readUsedPaths(t.procRoot, t.self)
	usedAt := t.now()
	if len(used.Refused) > 0 {
		// Nothing is deleted. A refused read is a process whose open files are unknown, and on
		// the first read of a sweep that is every process of the pod as far as this sweep can
		// tell: deleting under it would be deleting a cache a role may be holding open.
		out.refused = used.Refused
		out.reason = fmt.Sprintf("a /proc read the kernel refused before anything was deleted, so nothing was (%s)", strings.Join(used.Refused, "; "))
		out.report = first.Percent() > float64(set.LowPercent)
		return out
	}

	entries, cut, bySettings := t.list(ctx, used, cutoff, deadline)
	if cut != "" {
		out.reason = cut
		out.settingsStop = bySettings
		out.report = first.Percent() > float64(set.LowPercent)
		return out
	}
	// Oldest first across the roots and not root by root: the roots are swept in one list, so a
	// month-old bun cache entry goes before a go-build tree from this morning whatever order the
	// roots are listed in. The path breaks a tie so that two entries of the same second are
	// deleted in an order a test can predict.
	slices.SortStableFunc(entries, func(a, b sweepEntry) int {
		if by := a.at.Compare(b.at); by != 0 {
			return by
		}
		return strings.Compare(a.path, b.path)
	})

	previous := first
	stalled := 0
	sinceRead := 0
	readFailed := false
	for _, entry := range entries {
		if reason, settingsStop := t.halted(ctx, deadline); reason != "" {
			out.reason, out.settingsStop = reason, settingsStop
			break
		}
		if t.now().Sub(usedAt) >= volumeUsedPathsMaxAge {
			used = readUsedPaths(t.procRoot, t.self)
			usedAt = t.now()
			if len(used.Refused) > 0 {
				// Later than the first read: what the sweep deleted stands, and it stops here
				// rather than deleting on a reading of the pod's processes it knows to be
				// incomplete.
				out.refused = append(out.refused, used.Refused...)
				out.reason = fmt.Sprintf("a /proc read the kernel refused while the sweep ran, so it kept what it had deleted and stopped (%s)", strings.Join(used.Refused, "; "))
				break
			}
		}
		if used.Covers(entry.path) {
			continue
		}
		switch ok, why, err := entryKindOK(entry.path, entry.rule); {
		case errors.Is(err, fs.ErrNotExist):
			continue // a role's own tool deleted it between the listing and now
		case err != nil:
			t.logf(volumeTrimLog+"%s could not be read and is passed over: %v", entry.path, err)
			continue
		case !ok:
			t.logf(volumeTrimLog+"%s is passed over: %s", entry.path, why)
			continue
		}
		switch moved, err := t.moved(entry); {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			t.logf(volumeTrimLog+"%s could not be read again before the delete and is passed over: %v", entry.path, err)
			continue
		case moved:
			// Something wrote in it between the listing and this moment: a build that started
			// while the sweep listed, which the age guard would have kept out had it run now.
			continue
		}
		if err := os.RemoveAll(entry.path); err != nil {
			// A removal that failed part way leaves what it could not remove. It is reported
			// and passed over, and the part-removed entry a later sweep meets is an ordinary
			// entry of that root: the removal moved its modification time, so it is eligible
			// again one minimum age from now.
			t.logf(volumeTrimLog+"removing %s failed part way and the sweep passed it over: %v", entry.path, err)
			continue
		}
		out.deletions++
		sinceRead++
		if sinceRead < volumeSweepReadEvery {
			continue
		}
		reading, err := readVolumeUsage(t.volumeRoot)
		if err != nil {
			out.reason = fmt.Sprintf("the volume could not be read after %d deletions: %v", out.deletions, err)
			readFailed = true
			break
		}
		sinceRead = 0
		out.last = reading
		if reading.Percent() <= float64(set.LowPercent) {
			out.reason = fmt.Sprintf("the volume is back at the low mark %d%%", set.LowPercent)
			break
		}
		if reading.UsedBytes >= previous.UsedBytes {
			stalled++
		} else {
			stalled = 0
		}
		previous = reading
		if stalled >= volumeSweepStallReadings {
			out.reason = fmt.Sprintf("%d readings in a row showed no decrease, so deleting more of the cache frees nothing", stalled)
			break
		}
	}
	if out.reason == "" {
		out.reason = fmt.Sprintf("no entry is eligible: every one of the %d the sweep listed was deleted or skipped", len(entries))
	}
	if sinceRead > 0 && !readFailed {
		// The sweep's last reading: the deletions since the last one are not in any reading yet,
		// and the report's freed bytes are the difference between the first reading and this.
		if reading, err := readVolumeUsage(t.volumeRoot); err != nil {
			t.logf(volumeTrimLog+"the volume could not be read at the end of the sweep: %v", err)
		} else {
			out.last = reading
		}
	}
	out.freed = int64(first.UsedBytes) - int64(out.last.UsedBytes)
	out.freedPoints = first.Percent() - out.last.Percent()
	out.report = out.last.Percent() > float64(set.LowPercent)
	return out
}

// halted is the reason the sweep must end now, and "" when it must not. It is asked at every
// entry boundary and at every entry of the listing, which is what makes the three endings leave
// nothing half removed: an entry being removed is never interrupted, and the listing is cut
// where it stands.
//
// The container's stop comes first so that run returns promptly on a shutdown rather than after
// the sweep's budget, and the settings' stop is read here rather than once per sweep so that a
// daemon turning the trimmer off reaches a sweep in flight.
func (t *trimmer) halted(ctx context.Context, deadline time.Time) (reason string, bySettings bool) {
	switch {
	case ctx.Err() != nil:
		return "the container is stopping", false
	case t.settings() == nil:
		return "the trimmer was stopped", true
	case !t.now().Before(deadline):
		return fmt.Sprintf("the sweep's %s budget ran out", volumeSweepBudget), false
	}
	return "", false
}

// list is every eligible entry of every root, in root order, with the newest modification time
// anywhere in it. cut is the reason the listing was ended early, and bySettings that it was the
// settings that ended it; a cut listing ends the sweep, since the three things that cut it —
// the budget, a stop and the container's stop — are each a reason to stop deleting as well.
//
// A root that is absent or is not a directory is skipped and reported rather than being an
// error: a pod whose roles have not started yet has no bun cache, and nothing about that should
// keep the sweep away from the three roots that are there. A root a process holds open is
// skipped whole (usedPaths.Holds).
func (t *trimmer) list(ctx context.Context, used usedPaths, cutoff, deadline time.Time) (entries []sweepEntry, cut string, bySettings bool) {
	add := func(path string, rule EntryRule, dir bool) bool {
		if reason, settingsStop := t.halted(ctx, deadline); reason != "" {
			cut, bySettings = reason, settingsStop
			return false
		}
		switch ok, why, err := entryKindOK(path, rule); {
		case errors.Is(err, fs.ErrNotExist):
			return true
		case err != nil:
			t.logf(volumeTrimLog+"%s could not be read and is passed over: %v", path, err)
			return true
		case !ok:
			t.logf(volumeTrimLog+"%s is passed over: %s", path, why)
			return true
		}
		at, err := newestModTime(path)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				t.logf(volumeTrimLog+"the age of %s could not be read and it is passed over: %v", path, err)
			}
			return true
		}
		if at.After(cutoff) {
			return true // inside the minimum age: something wrote in it too recently
		}
		entries = append(entries, sweepEntry{path: path, rule: rule, at: at, dir: dir})
		return true
	}

	for _, root := range t.roots {
		switch ok, why, err := entryKindOK(root.Path, ImmediateChildren); {
		case errors.Is(err, fs.ErrNotExist):
			t.logf(volumeTrimLog+"the reclaimable root %s is not there and is skipped", root.Path)
			continue
		case err != nil:
			t.logf(volumeTrimLog+"the reclaimable root %s could not be read and is skipped: %v", root.Path, err)
			continue
		case !ok:
			// ImmediateChildren is the rule asked of the root itself because what it asks of a
			// path is that it be a directory, which is what a root must be whatever its own
			// entry rule is.
			t.logf(volumeTrimLog+"the reclaimable root %s is skipped: %s", root.Path, why)
			continue
		}
		if used.Holds(root.Path) {
			// A tool has the root's own inode open, which is the `flock` every rendered install
			// takes on its cache root for the length of the install: the entries under it are
			// what that install is filling, and the root is left alone until it lets go.
			t.logf(volumeTrimLog+"the reclaimable root %s is held open by a process of the pod and is skipped whole", root.Path)
			continue
		}
		children, err := os.ReadDir(root.Path)
		if err != nil {
			t.logf(volumeTrimLog+"the reclaimable root %s could not be listed and is skipped: %v", root.Path, err)
			continue
		}
		for _, child := range children {
			path := filepath.Join(root.Path, child.Name())
			switch root.Entry {
			case GoBuildTrees:
				// Go names these and owns them, and makes one for every `go build` and every
				// `go test` because the pod sets no GOTMPDIR. Everything else in $TMPDIR is a
				// role's own work under a name the role chose.
				if !strings.HasPrefix(child.Name(), "go-build") {
					continue
				}
			case GoCacheEntries:
				// The entries are the names ending `-a` or `-d` inside a shard directory. The
				// cache's own `trim.txt` and `README` sit in the cache root rather than in a
				// shard, so a rule that reaches only into shard directories never reaches them.
				if !child.IsDir() {
					continue
				}
				shard, err := os.ReadDir(path)
				if err != nil {
					t.logf(volumeTrimLog+"the shard %s could not be listed and is skipped: %v", path, err)
					continue
				}
				for _, name := range shard {
					if !strings.HasSuffix(name.Name(), "-a") && !strings.HasSuffix(name.Name(), "-d") {
						continue
					}
					if !add(filepath.Join(path, name.Name()), root.Entry, name.IsDir()) {
						return entries, cut, bySettings
					}
				}
				continue
			case ImmediateChildren:
				if slices.Contains(root.Except, child.Name()) {
					continue
				}
			}
			if !add(path, root.Entry, child.IsDir()) {
				return entries, cut, bySettings
			}
		}
	}
	return entries, cut, bySettings
}

// moved is whether anything in the entry was written since the listing read it, asked again
// immediately before the delete: a build that started while the sweep listed must keep its
// cache. A directory is asked with newerThan, which stops at the first file newer than the
// recorded time instead of walking the tree again, and a single file with the lstat it needs
// anyway.
func (t *trimmer) moved(entry sweepEntry) (bool, error) {
	if entry.dir {
		return newerThan(entry.path, entry.at)
	}
	info, err := os.Lstat(entry.path)
	if err != nil {
		return false, err
	}
	return info.ModTime().After(entry.at), nil
}
