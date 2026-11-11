package launcher

import (
	"cmp"
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
	// volumeSettingsWait is how often a trimmer with no settings it can run asks for settings
	// again (awaitSettings). It is no part of how fast an operator's change reaches the pod:
	// the launcher redials once a second (reconnectDelay) and an acknowledgement is put in
	// force the moment it arrives (manager.applyAck), so this is only how long a pod goes on
	// guarding nothing after the settings it may run by are already in force — ten seconds
	// against an interval that is sixty by default, and against a volume that took hours to
	// fill.
	volumeSettingsWait = 10 * time.Second
)

// refusedTrimSettings is every reason the settings cannot be run, each naming the key as it is
// written on the wire and the value that was sent, and empty when they can. A trimmer that
// refuses its settings stays off rather than starting its ticker: there is no safe default to
// fall back to, since every default is a guess at how full this operator's volume may get.
//
// This is the ranges checked a second time on the trimmer's own side of the wire. The daemon
// validates them too, but it is another binary across a connection this design expects to carry
// version skew both ways, and the cost of believing a bad one here is paid by the whole pod:
// time.NewTicker panics on a non-positive period, in the process that is PID 1 of the roles
// container with six live roles under it, and a high mark of 0 would sweep the caches of a
// volume that is empty. The ranges themselves are the wire's own (shimwire.VolumeMarkMin and
// the four beside it), which the daemon's loader reads too: the second check is deliberate, two
// values that could disagree are not.
func refusedTrimSettings(set shimwire.LauncherVolumeTrim) []string {
	var refused []string
	mark := func(key string, value int) {
		if value < shimwire.VolumeMarkMin || value > shimwire.VolumeMarkMax {
			refused = append(refused, fmt.Sprintf("%s is %d, want a whole number from %d to %d", key, value, shimwire.VolumeMarkMin, shimwire.VolumeMarkMax))
		}
	}
	mark("high_percent", set.HighPercent)
	mark("low_percent", set.LowPercent)
	if set.LowPercent >= set.HighPercent {
		refused = append(refused, fmt.Sprintf("low_percent is %d and high_percent is %d, want the low mark below the high one", set.LowPercent, set.HighPercent))
	}
	if set.MinAgeMinutes < shimwire.VolumeMinAgeMin {
		refused = append(refused, fmt.Sprintf("min_age_minutes is %d, want at least %d", set.MinAgeMinutes, shimwire.VolumeMinAgeMin))
	}
	if set.IntervalSeconds < shimwire.VolumeIntervalMin || set.IntervalSeconds > shimwire.VolumeIntervalMax {
		refused = append(refused, fmt.Sprintf("interval_seconds is %d, want a whole number from %d to %d", set.IntervalSeconds, shimwire.VolumeIntervalMin, shimwire.VolumeIntervalMax))
	}
	return refused
}

// trimmer is the pod's cache trimmer: one per launcher process.
type trimmer struct {
	// id is minted once per launcher process and kept across every reconnection: a rolling
	// daemon restart flips the bound connection repeatedly, and an identifier per connection
	// would multiply the daemon's three-notice bound.
	id string
	// settings is what is in force now, read before every interval reading and again while the
	// trimmer waits for settings it may run by. Nil stops the sweeping: it ends a sweep in
	// flight at its next entry boundary with its deletions standing, sends no report, and
	// discards a report it was holding — and the trimmer then waits here for settings to come
	// back rather than ending with the pod (awaitSettings).
	settings func() *shimwire.LauncherVolumeTrim
	// trimming answers whether settings are in force at all, which is the whole of what a
	// sweep asks of them (halted). It is asked once per listed entry and once per swept entry
	// — tens of thousands of times over one sweep of the measured pod's caches — where
	// settings takes the launcher's mutex and returns a freshly allocated copy of the struct
	// for a pointer compared with nil and dropped. Both seams read the one field under the one
	// lock (manager.volumeTrim, manager.volumeTrimming), so nothing of the lifecycle changes.
	trimming func() bool
	// send writes one report on the pod's bound connection. An error holds the report, which
	// the trimmer sends on the next attempt, as the launcher's bundleRestore holds its own: no
	// connection is up, or the connection that is up answered the hello with no settings and so
	// has no case for a report (manager.sendVolumeReport).
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
	episode   int     // 0 until the first reading at or above the high mark
	raisedBy  string  // which of shimwire's three raised the episode above
	aboveHigh bool    // a reading has been at or above the high mark since the last recovery
	anchor    float64 // the highest pre-sweep reading of a sweep that reported in this episode
	anchored  bool    // whether a sweep has reported in this episode at all
	pause     int     // intervals left of the pause after a sweep that freed under a point
	last      volumeUsage
	faults    []string // the newest recovered panics the next report carries (volumeReportFaults)
	// faultsDropped is how many older ones the rule above left out, which the next report says
	// rather than dropping them silently.
	faultsDropped int
	consecutive   int // recovered panics in a row, which volumeFaultLimit bounds
	held          *shimwire.LauncherVolumeReport
	walk          walkResult // the last size walk, which a report between walks carries
	// said is the last standing state the trimmer wrote, which says keeps so that a state it is
	// still in is not written once per interval.
	said string
	// saidRead is the same memory for the one standing state that is not a settings state: a
	// volume this process cannot read. It is its own field because says is cleared on every
	// interval whose settings the trimmer can run (saidNothing), which is every interval a
	// failing statfs happens on.
	saidRead string
}

// newTrimmer is the pod's trimmer: the volume at VolumeRoot, the container's own /proc, the four
// reclaimable roots and the real clock, with an identifier minted for the life of this launcher
// process (randomID, as the launcher mints a connection's).
func newTrimmer(
	settings func() *shimwire.LauncherVolumeTrim,
	trimming func() bool,
	send func(shimwire.LauncherVolumeReport) error,
	logf func(format string, args ...any),
) *trimmer {
	return &trimmer{
		id:         randomID(),
		settings:   settings,
		trimming:   trimming,
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

// trimEnding is how one interval, and one loop, of the trimmer ended. Only trimOff is final:
// everything else leaves the trimmer in this launcher process with work still to do, which is
// what R1 of the design asks of it — a trimmer runs in each issue pod whenever settings are in
// force, for the life of that pod (dispatch://INFRA-2009 spec) — and what the settings reaching
// every pod at its next connection is for.
type trimEnding int

const (
	// trimGoesOn is no ending at all: the interval loop takes its next tick.
	trimGoesOn trimEnding = iota
	// trimWaits is the settings gone, in an acknowledgement that carried none or in the middle
	// of a sweep: the interval loop ends and run waits for settings it may run by again. A pod
	// lives through `enabled: false` and the operator turning it back on, and through a daemon
	// rolled back to a release without this work and the roll-forward after it, and keeps its
	// trimmer.
	trimWaits
	// trimOff is the one final ending: volumeFaultLimit recovered panics in a row have switched
	// the trimmer off, and nothing starts another before the launcher process restarts (fault).
	trimOff
	// trimStopping is the container going: ctx has ended, and run returns with it.
	trimStopping
)

// run is the trimmer's whole life in this launcher process: the wait for settings it may run
// by, the interval loop those settings start, and the wait again when they go away. It returns
// on the container's stop and on the faults, and never on settings alone, because a pod takes
// the settings of its bound connection's latest answer and an operator who turned the trimmer
// off may turn it on again in the pod this process is PID 1 of.
func (t *trimmer) run(ctx context.Context) {
	for {
		inForce, ending := t.awaitSettings(ctx)
		if ending != trimGoesOn {
			return
		}
		if t.intervals(ctx, inForce) != trimWaits {
			return
		}
	}
}

// awaitSettings is the wait for settings the trimmer may run by: it answers the first ones it
// can run, or the container's stop. Nothing of the trimmer's own state is touched here — the
// identifier, the episode, the anchor and the pause are this pod's counting line and go on
// across a trimmer that was turned off and on again — and no interval ticker is made, so a
// trimmer waiting sweeps nothing and reads nothing, which is what R2 asks of settings outside
// the ranges.
//
// It polls rather than being woken: the seam is a read of what the latest acknowledgement left
// in force (manager.volumeTrim) and nothing signals a change of it. It says what it is waiting
// for when that changes — the first time, and again when the reason changes — and never once
// per poll, since the pod's log is what a person reads and a line every volumeSettingsWait
// would bury the rest of the launcher's. The memory that holds it to that is the trimmer's own
// (says), which the interval loop's refusals share.
func (t *trimmer) awaitSettings(ctx context.Context) (shimwire.LauncherVolumeTrim, trimEnding) {
	var ticks <-chan time.Time
	stop := func() {}
	defer func() { stop() }()
	for {
		set := t.settings()
		var refusals []string
		reason := "no settings are in force"
		if set != nil {
			refusals = refusedTrimSettings(*set)
			if len(refusals) == 0 {
				t.saidNothing()
				return *set, trimGoesOn
			}
			reason = strings.Join(refusals, "; ")
		}
		if t.says(reason) {
			if refusals == nil {
				t.logf(volumeTrimLog + "no settings are in force, so the trimmer stays off until the daemon sends settings it can run")
			} else {
				t.refuse(refusals)
			}
		}
		if ticks == nil {
			ticks, stop = t.ticks(volumeSettingsWait)
		}
		select {
		case <-ctx.Done():
			return shimwire.LauncherVolumeTrim{}, trimStopping
		case <-ticks:
		}
	}
}

// intervals is the interval loop those settings start: one reading per tick and the sweep a
// reading at the high mark asks for, until the container stops, the faults stop the trimmer, or
// the settings go away and run waits for them again. It is entered with settings the trimmer
// has already accepted and starts its ticker on nothing else: time.NewTicker panics on a
// non-positive period, and this goroutine's panic is PID 1's panic (awaitSettings, tick).
func (t *trimmer) intervals(ctx context.Context, inForce shimwire.LauncherVolumeTrim) trimEnding {
	t.logf(volumeTrimLog+"started: trimmer %s, high %d%%, low %d%%, minimum age %d min, interval %d s, volume %s",
		t.id, inForce.HighPercent, inForce.LowPercent, inForce.MinAgeMinutes, inForce.IntervalSeconds, t.volumeRoot)
	ticks, stop := t.ticks(settingsInterval(inForce))
	defer func() { stop() }()
	for {
		select {
		case <-ctx.Done():
			return trimStopping
		case <-ticks:
		}
		restart, ending := t.tick(ctx, &inForce)
		if ending != trimGoesOn {
			return ending
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

// says is whether a standing state of the trimmer's is news, and remembers it when it is. A
// refusal is a state rather than an event — a daemon that widened a maximum this launcher does
// not know sends the same settings with every acknowledgement, and the trimmer meets them once
// an interval (tick) or once every volumeSettingsWait (awaitSettings) — so saying it per
// interval would put two lines a minute into the roles container's log for the life of the pod,
// and that log is the only place this work is visible to a person after a deploy.
func (t *trimmer) says(state string) bool {
	if state == t.said {
		return false
	}
	t.said = state
	return true
}

// saidNothing clears what says keeps, which settings the trimmer can run do: the pod is out of
// the state it was in, so the same refusal arriving later is news again rather than the one the
// log already carries.
func (t *trimmer) saidNothing() {
	t.said = ""
}

// settingsInterval is the period of the interval clock.
func settingsInterval(set shimwire.LauncherVolumeTrim) time.Duration {
	return time.Duration(set.IntervalSeconds) * time.Second
}

// tick is one interval: the settings in force, a held report, one reading of the volume, the
// episode that reading raises, and the sweep it asks for. restart says the interval clock must
// be made again, and the ending what the interval loop must do now (trimEnding).
//
// It recovers from a panic in its own work, which is a deliberate departure from this daemon's
// practice — no production path of it recovers a panic, because a process that cannot keep its
// invariants should die and be restarted — because this goroutine runs in the process that is
// PID 1 of the roles container: its panic takes the container down and costs six live roles
// their round, over a cache sweep that nothing depends on. The fault is logged with its stack
// and carried into the next report, and three in a row stop the trimmer for good (fault), which
// is the one ending of the four that nothing comes back from.
func (t *trimmer) tick(ctx context.Context, inForce *shimwire.LauncherVolumeTrim) (restart bool, ending trimEnding) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			t.consecutive = 0
			return
		}
		restart, ending = false, trimGoesOn
		if t.fault(recovered, debug.Stack()) {
			ending = trimOff
		}
	}()

	next := t.settings()
	if next == nil {
		t.held = nil
		t.logf(volumeTrimLog + "no settings are in force: the trimmer sweeps nothing more until they come back, and a report it was holding is discarded")
		return false, trimWaits
	}
	refusals := refusedTrimSettings(*next)
	switch {
	case len(refusals) > 0:
		// The settings in force stay in force. Switching the trimmer off here would let one bad
		// frame from a daemon of another version leave the pod's volume unguarded for the life
		// of the container, where keeping the last settings the trimmer accepted leaves it
		// doing what the operator last asked for. The refusal is written when it is news and
		// not once per interval (says): what it describes is a daemon's standing answer.
		if t.says(strings.Join(refusals, "; ")) {
			t.refuse(refusals)
		}
	default:
		t.saidNothing()
		if *next != *inForce {
			t.logf(volumeTrimLog+"new settings: high %d%%, low %d%%, minimum age %d min, interval %d s",
				next.HighPercent, next.LowPercent, next.MinAgeMinutes, next.IntervalSeconds)
			restart = next.IntervalSeconds != inForce.IntervalSeconds
			*inForce = *next
		}
	}
	set := *inForce

	t.flush()
	reading, err := readVolumeUsage(t.volumeRoot)
	if err != nil {
		// Said when it changes and not once per interval, for the reason a refusal is (says): a
		// volume this process cannot read is a standing state, and the statfs behind it answers
		// the same way every minute for as long as it lasts.
		if why := err.Error(); why != t.saidRead {
			t.saidRead = why
			t.logf(volumeTrimLog+"the volume at %s could not be read: %v", t.volumeRoot, err)
		}
		return restart, trimGoesOn
	}
	t.saidRead = ""
	t.last = reading
	percent := reading.Percent()
	t.episodeOf(percent, set)
	switch {
	case t.pause > 0:
		t.logf(volumeTrimLog+"the volume is %.1f%% full (used %d B, free %d B): no sweep for another %d intervals, since the last one freed under %.0f point",
			percent, reading.UsedBytes, reading.AvailableBytes, t.pause, volumeSweepMinFreedPoints)
		t.pause--
		return restart, trimGoesOn
	case percent < float64(set.HighPercent):
		t.logf(volumeTrimLog+"the volume is %.1f%% full (used %d B, free %d B): below the high mark %d%%, nothing to sweep",
			percent, reading.UsedBytes, reading.AvailableBytes, set.HighPercent)
		return restart, trimGoesOn
	}
	t.logf(volumeTrimLog+"the volume is %.1f%% full (used %d B, free %d B): at or above the high mark %d%%, sweeping to %d%%",
		percent, reading.UsedBytes, reading.AvailableBytes, set.HighPercent, set.LowPercent)

	out := t.sweep(ctx, set, reading)
	t.last = out.last
	if out.settingsStop {
		t.held = nil
		t.logf(volumeTrimLog + "the settings stopped the trimmer: the sweep ended at an entry boundary with its deletions standing, no report is sent, and a report it was holding is discarded")
		return false, trimWaits
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
	return true, trimGoesOn
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
//
// Only the newest volumeReportFaults stacks are kept, and what they displace is counted: the
// faults are cleared when a report carries them (report), so a pod whose volume stays healthy
// never reports and would otherwise keep every stack it ever recovered for the life of the
// launcher process, kilobytes each, in the process that is PID 1 of the roles container.
func (t *trimmer) fault(recovered any, stack []byte) bool {
	t.consecutive++
	t.faults = append(t.faults, fmt.Sprintf("%v\n%s", recovered, stack))
	if over := len(t.faults) - volumeReportFaults; over > 0 {
		t.faultsDropped += over
		t.faults = slices.Delete(t.faults, 0, over)
	}
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
//
// The one thing that stops it is the settings being gone: send refuses a report on a connection
// whose acknowledgement carried none (manager.sendVolumeReport), and that is the daemon this
// trimmer could not have told anyway — one rolled back below this contract, which closes the
// connection on a frame type it does not name.
func (t *trimmer) switchOff() {
	report := shimwire.LauncherVolumeReport{
		Trimmer:       t.id,
		Episode:       max(t.episode, 1),
		RaisedBy:      t.raisedBy,
		UsedBytes:     int64(t.last.UsedBytes),
		FreeBytes:     int64(t.last.AvailableBytes),
		StopReason:    fmt.Sprintf("the trimmer switched itself off after %d panics in a row", t.consecutive),
		Faults:        t.faults,
		FaultsOmitted: t.faultsDropped,
		SwitchedOff:   true,
	}
	if report.RaisedBy == "" {
		// The trimmer never saw a reading at the high mark, so no episode was ever raised; the
		// frame's own rule is that an episode is at least 1, which the daemon applies to every
		// report it reads (LauncherVolumeReport.Validate, sandbox's volumeReported), and this is
		// the first thing the daemon hears of this trimmer.
		report.RaisedBy = shimwire.VolumeEpisodeFirst
	}
	t.flush()
	if err := t.send(t.bound(report)); err != nil {
		t.logf(volumeTrimLog+"the last report of the switched-off trimmer could not be sent: %v", err)
	}
	t.faults, t.faultsDropped = nil, 0
}

// report is one sweep as the daemon hears it, with the sizes of a fresh walk or of the last one
// with the time it was read (sizes). It takes the faults the trimmer recovered since the last
// report with it, whether or not the send gets across: a held report carries them, so holding
// them twice would report them twice.
func (t *trimmer) report(ctx context.Context, out sweepOutcome) shimwire.LauncherVolumeReport {
	walk := t.sizes(ctx)
	faults, dropped := t.faults, t.faultsDropped
	t.faults, t.faultsDropped = nil, 0
	return t.bound(shimwire.LauncherVolumeReport{
		Trimmer:       t.id,
		Episode:       t.episode,
		RaisedBy:      t.raisedBy,
		UsedBytes:     int64(out.last.UsedBytes),
		FreeBytes:     int64(out.last.AvailableBytes),
		FreedBytes:    out.freed,
		StopReason:    out.reason,
		RefusedReads:  out.refused,
		Sizes:         walk.Sizes,
		SizesAt:       walk.At,
		SizesPartial:  walk.Partial,
		SizesMissing:  walk.Missing,
		Faults:        faults,
		FaultsOmitted: dropped,
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
		refused, report.RefusedReadsOmitted = refused[:volumeReportRefusedReads], over
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

// cutEach is list with every entry cut to at most runes, and nil for an empty one: a report
// carries no list where the trimmer had none.
func cutEach(list []string, runes int) []string {
	if len(list) == 0 {
		return nil
	}
	out := make([]string, len(list))
	for at, one := range list {
		out[at] = cutRunes(one, runes)
	}
	return out
}

// cutRunes is s cut to at most bound runes with an ellipsis where it was cut, as runtime.PodEnd
// cuts a kubelet's message at runtime.PodEndRunes: "its size must be one this package chose
// rather than one a node's message happened to have".
func cutRunes(s string, bound int) string {
	runes := []rune(s)
	if len(runes) <= bound {
		return s
	}
	return string(runes[:bound-1]) + "…"
}

// sizes is the size walk for one report: a fresh walk, or the last one when it is less than an
// hour old. The walk stats every file of the volume and is bounded at a minute (volumewalk.go),
// which is a minute of the pod's one spare process, so a pod sweeping every interval walks once
// an hour and sends the same numbers in between with the time they were read.
func (t *trimmer) sizes(ctx context.Context) walkResult {
	if !t.walk.At.IsZero() && t.now().Sub(t.walk.At) < volumeWalkInterval {
		return t.walk
	}
	t.walk = sizeWalk{
		root:    t.volumeRoot,
		now:     t.now,
		after:   t.ticks,
		budget:  volumeWalkBudget,
		measure: t.measure,
	}.run(ctx)
	if t.walk.Partial {
		// The names are the pod's own: a role may make a directory of any name two levels under
		// the volume root, so this line takes the same bounds the frame takes (bound) rather
		// than joining a list of a size and a content nothing here chose. A reader who needs
		// the whole list has the report's own SizesMissing, bounded the same way.
		missing := cutEach(t.walk.Missing, volumeReportPathRunes)
		left := 0
		if len(missing) > volumeReportSizesMissing {
			left, missing = len(missing)-volumeReportSizesMissing, missing[:volumeReportSizesMissing]
		}
		t.logf(volumeTrimLog+"the size walk ended early: it measured %d directories and left %d unmeasured (%q, and %d more)",
			len(t.walk.Sizes), len(t.walk.Missing), missing, left)
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
		return cmp.Or(a.at.Compare(b.at), strings.Compare(a.path, b.path))
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
// daemon turning the trimmer off reaches a sweep in flight. It is read through trimming, which
// answers that one question as a bool: this is the hottest read of the settings there is.
func (t *trimmer) halted(ctx context.Context, deadline time.Time) (reason string, bySettings bool) {
	switch {
	case ctx.Err() != nil:
		return "the container is stopping", false
	case !t.trimming():
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
