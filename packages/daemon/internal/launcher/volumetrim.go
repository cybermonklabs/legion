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
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/redact"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// The pod's cache trimmer: the interval loop that reads the volume, the sweep that deletes the
// oldest entries of the four reclaimable roots, and the episode number a report is counted
// under. The report itself is volumereport.go's and the size walk it carries volumewalk.go's.
// It runs in the launcher process, which is PID 1 of an issue pod's roles container, beside the
// six roles whose caches fill the one volume they share, 50 GiB by default (INFRA-2009: a pod
// filled 45 GiB of 49, and a role's work was deleted by hand mid-round).
//
// Everything the trimmer reads of the machine is a field of the trimmer below, so a test drives a
// tree, a volume reading, a /proc and a clock of its own.

// volumeTrimLog begins every line the trimmer writes, but the stack lines below a recovered
// panic's own line (fault), which do not carry it. In an issue pod every line the trimmer
// writes, the stack lines too, reaches the pod's log behind the prefix the launcher gives the
// lines of the role the trimmer is bound to, the architect (serveVolumeTrim, prefixWriter). The
// lines are what a person reads in the pod after a deploy — the launcher's log is the only place
// most of this work is visible, since the daemon hears nothing but a report of a sweep that
// ended above the low mark and the report that the trimmer switched itself off — so they carry
// one prefix a `grep` can take.
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
	// goCacheMtimeSlack is how much further back than the minimum age an entry of the Go build
	// cache is judged, and that rule's entries alone. Go moves a cache entry's modification
	// time on use at most once an hour — `mtimeInterval = 1 * time.Hour`, with markUsed's `if
	// now := c.now(); now.Sub(info.ModTime()) >= mtimeInterval { os.Chtimes(file, now, now) }`
	// (cmd/go/internal/cache/cache.go:348 and :352-372, read in this pod's own go1.27.3) — so
	// an entry used a minute ago can read 59 minutes old. The used-path guard cannot close that
	// gap: on a cache hit the build writes the entry's path into its importcfg and opens it
	// only when the compile or the link that needs it runs, so for the length of a build those
	// entries are in use with no handle a /proc read can see, and the sweep runs at the high
	// mark, which is when the roles are building. Go's own trim makes the same allowance
	// against the same imprecision: "We subtract an additional mtimeInterval to account for the
	// imprecision of our “last used” mtimes", `cutoff := now.Add(-trimLimit - mtimeInterval)`
	// (cache.go:427-429).
	//
	// No other rule has it, because only Go writes its mtimes this way. A Go build tree of
	// $TMPDIR is not of this class either: it is Go's working directory for one build, written
	// as that build runs, and nothing ever marks it used — its time is the time it was last
	// written.
	goCacheMtimeSlack = time.Hour
	// volumeQuietIntervals is how many intervals pass after a sweep that freed less than
	// volumeSweepMinFreedPoints before another sweep may start, so the next sweep is the
	// eleventh interval after that one. A sweep that freed nothing has no eligible entry left or
	// is losing to the roles writing, and repeating it every interval would spend the listing's
	// cost — minutes of walking the volume — for nothing, on the volume that is already full. The
	// interval reading continues through the pause, because a reading inside it can end an
	// episode, and no sweep's last reading is taken there (episodeOf).
	//
	// The pause follows every sweep that freed under a point, but one the settings stopped,
	// which returns the trimmer to its wait and sets no pause (tick); and so it follows each
	// sweep that deleted nothing at all: one whose deleter would not start, one a refused first
	// /proc read stopped, and one whose listing its budget cut (sweep). The last
	// sets a limit nothing else states: a listing reads every name of every entry's tree under
	// the roots (newestModTime), so a sweep whose reclaimable roots hold more names than the
	// listing reads in volumeSweepBudget is cut while it lists, deletes nothing, pauses, and is
	// cut the same way the next time, and that volume is never swept. In a test process of this
	// issue pod on 2026-11-15, kernel 4.19.0-gvisor, under go1.27.3, the listing read 8,400 to
	// 9,700 names a second over 400 entries of five directories of ten files each, three runs,
	// which is about 2.5 million names in the five minutes at the slowest, for a tree of that
	// shape.
	volumeQuietIntervals = 10
	// volumeSweepMinFreedPoints is what a sweep must free, in percentage points of the volume,
	// not to engage the pause above.
	volumeSweepMinFreedPoints = 1.0
	// volumeRisePoints is how far above an episode's anchor the lower of the last two reported
	// sweep starts must stand to raise the episode number (riseAt).
	volumeRisePoints = 5.0
	// volumeFaultLimit is how many recovered panics in a row of one kind of the trimmer's work
	// stop the trimmer for the life of the launcher process: a row of its readings, which a
	// reading run to its end ends, or a row of its sweeps and their reports, which a sweep run to
	// its end ends (faultRow, fault, tick).
	volumeFaultLimit = 3
	// volumeSettingsWait is how often a trimmer with no settings it can run asks for settings
	// again (awaitSettings). It is no part of how fast an operator's change reaches the pod:
	// the launcher redials once a second (reconnectDelay) and an acknowledgement is put in
	// force the moment it arrives (manager.applyAck), so this is only how long a pod goes on
	// guarding nothing after the settings it may run by are already in force — ten seconds
	// against an interval that is sixty by default, and against a volume that took hours to
	// fill.
	volumeSettingsWait = 10 * time.Second
	// volumePassedOverLines is how many entries of one root a sweep names in the pod's log as
	// passed over before it only counts them (passedOver): enough to show an operator which
	// names and which reasons, where a root a role filled with links would otherwise write a
	// line per link on every sweep.
	volumePassedOverLines = 10
)

// refusedTrimSettings is every reason the settings cannot be run, each naming the key as the
// daemon's YAML writes it — high_percent, low_percent, min_age_minutes and interval_seconds of
// runtime.kubernetes.volume_trim, where the operator sets them, and not the wire's names for
// them (shimwire.LauncherVolumeTrim) — and the value that was sent, and empty when they can. A
// trimmer that refuses its settings stays off rather than starting its ticker: there is no safe
// default to fall back to, since every default is a guess at how full this operator's volume may
// get.
//
// This is the ranges checked a second time on the trimmer's own side of the wire. The daemon
// validates them too, but it is another binary across a connection this design expects to carry
// version skew both ways, and the cost of believing a bad one here is paid by the whole pod:
// time.NewTicker panics on a non-positive period, a fault on each start of the interval loop, so
// three in a row would switch off the one guard of the volume six live roles share (run), a high
// mark of 0 would sweep the caches of a volume that is empty, and an age of 153,722,868 minutes
// or more can wrap the sweep's cutoff past the sweep's own start, which makes every entry of
// every root eligible however recently it was written (sweep, shimwire.VolumeMinAgeMax). The
// ranges themselves are the wire's own (shimwire.VolumeMarkMin and the five beside it). The
// daemon's loader reads the three maxima among them, and its minimum of 1 is its own
// positive-integer rule (readPositiveInteger, internal/config), which equals the three minima
// here: the second check is deliberate, and the maxima at least are one source, not two values
// that could disagree.
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
	if set.MinAgeMinutes < shimwire.VolumeMinAgeMin || set.MinAgeMinutes > shimwire.VolumeMinAgeMax {
		refused = append(refused, fmt.Sprintf("min_age_minutes is %d, want a whole number from %d to %d", set.MinAgeMinutes, shimwire.VolumeMinAgeMin, shimwire.VolumeMinAgeMax))
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
	// flight before its next removal begins, with its deletions standing, sends no report, and
	// discards a report it was holding — and the trimmer then waits here for settings to come
	// back rather than ending with the pod (awaitSettings). Settings the trimmer refuses stop it
	// the same way, except that a report it was holding is tried rather than discarded
	// (awaitSettings).
	settings func() *shimwire.LauncherVolumeTrim
	// trimming answers whether settings the trimmer can run are in force (refusedTrimSettings),
	// which is the whole of what a sweep asks of them (halted). It is asked once per listed
	// entry, once per directory of a listed entry's walk and once per batch of that directory's
	// names after its first, and once per swept entry — tens of thousands of times over one
	// sweep of the measured pod's caches — where settings takes the launcher's mutex and returns
	// a freshly allocated copy of the struct.
	trimming func() bool
	// send writes one report on the pod's bound connection. An error holds the report, which
	// the trimmer sends on the next attempt: no connection is up, or the connection that is up
	// answered the hello with no settings and so has no case for a report
	// (manager.sendVolumeReport).
	send func(shimwire.LauncherVolumeReport) error
	now  func() time.Time
	// ticks is the interval clock; a test drives it. The runner takes the channel and a stop.
	// The size walk's sixty-second budget comes from it as well (sizeWalk.after), because a
	// walk that blocks inside one measurement cannot be cut by a clock a loop reads.
	ticks func(time.Duration) (<-chan time.Time, func())
	logf  func(format string, args ...any)
	// volumeRoot, procRoot, cover, self and roots are the machine, each an argument so a test
	// drives a tree of its own: VolumeRoot, "/proc", the cover each sweep's deleter builds its
	// own namespace over (roleCoverFor of the launcher's --roles-dir), os.Getpid() and
	// ReclaimableRoots in the pod.
	volumeRoot, procRoot string
	cover                RoleCover
	self                 int
	roots                []ReclaimableRoot
	// readUsage is one reading of the volume at volumeRoot: readVolumeUsage (volumeusage.go) in
	// the pod, and a test's own reading, whose calls it counts, in a test.
	readUsage func(path string) (volumeUsage, error)
	// boundReport is how a report is bounded where it is made and where it replaces a held one
	// (report, switchOff, deliver): the trimmer's own bound in the pod, and in a test a bound that
	// panics, so the case can read that what the report was to carry stays with the trimmer for the
	// next one.
	boundReport func(report shimwire.LauncherVolumeReport) shimwire.LauncherVolumeReport
	// measure is one target of the size walk, the seam a test blocks to drive the walk's
	// budget (measureTree, volumewalk.go). It is given the handle the walk holds on the
	// directory that holds the target and the target's own name in it, never a path: a name
	// with no separator is the one read a handle resolves by its descriptor alone, which is
	// what keeps the walk inside the volume whatever a role renames while it runs
	// (sizeWalk.run).
	measure func(ctx context.Context, dir *dirHandle, name string, at time.Time) measured
	// removals starts the deleter one sweep makes its deletions through, which in the pod is a
	// child in a mount namespace of its own (startRemover, volumedelete.go). It is a field so
	// that a case about something other than containment can give the sweep a remover that
	// deletes in the test process, as the machine above is a field; the pod never replaces it,
	// and a sweep whose deleter will not start deletes nothing at all (sweep).
	removals func(cover RoleCover, dirs []*sweepDir) (entryRemover, error)

	// Everything below is the trimmer's own state across intervals. run is one goroutine and
	// nothing else reads these, so they carry no lock: the launcher's connection goroutine
	// reaches the trimmer through settings and send alone, which are the caller's to make safe.
	episode   int     // 0 until the first reading at or above the high mark
	raisedBy  string  // which of shimwire's three raised the episode above
	aboveHigh bool    // a reading has been at or above the high mark since the last recovery
	anchor    float64 // what a rise is measured against: the lowest reported start, or a rise's floor (riseAt)
	anchored  bool    // whether the episode has its anchor yet: from its first reported sweep, or its rise
	lastStart float64 // the start of the last reported sweep, the first of the two a rise reads
	pause     int     // intervals left of the pause after a sweep that freed under a point
	last      volumeUsage
	faults    []string // the newest recovered panics the next report carries (shimwire.VolumeReportFaults)
	// faultsDropped is how many older ones the rule above left out, which the next report says
	// rather than dropping them silently.
	faultsDropped int
	// refusedReads is the refused /proc reads no report has carried yet, which the next report
	// carries (carryRefused): a sweep puts each here the moment it meets it (sweep), so a sweep
	// that sends no report — one that ends at or below the low mark, or one that panics after the
	// refusal, in itself, in its interval or in its report — leaves it for the next, and a
	// process the trimmer could not read is one the operator must hear of. refusedDropped is how
	// many the bound on a report's list left out, which that report says.
	refusedReads   []string
	refusedDropped int
	// doing is the kind of work in flight, whose row a panic of that work adds to (fault): the
	// readings' unless a sweep's work runs, which sets it for as long as that work runs (tick,
	// flush), and the readings' again once that work has run to its end or its panic has been
	// counted (ran, fault, run).
	doing faultRow
	// inARow is each row's recovered panics since the last work of its kind that ran to its end
	// (ran), indexed by faultRow, which volumeFaultLimit bounds (fault).
	inARow [sweepRow + 1]int
	held   *shimwire.LauncherVolumeReport
	walk   walkResult // the last size walk, which a report between walks carries
	// said is the last standing state the trimmer wrote, set once its lines are written, which
	// news reads so that a state it is still in is not written once per interval.
	said string
	// saidRead is the same memory for the one standing state that is not a settings state: a
	// volume this process cannot read. It is its own field because said is cleared on every
	// interval whose settings the trimmer can run (saidNothing), and a failing statfs happens on
	// those intervals as much as on any other.
	saidRead string
}

// newTrimmer is the pod's trimmer: the volume at VolumeRoot, the container's own /proc, the four
// reclaimable roots and the real clock, with an identifier minted for the life of this launcher
// process (randomID, as the launcher mints a connection's). Each sweep's deleter starts through
// guard, the launcher's pidGuard (startRemover).
func newTrimmer(
	settings func() *shimwire.LauncherVolumeTrim,
	trimming func() bool,
	send func(shimwire.LauncherVolumeReport) error,
	logf func(format string, args ...any),
	cover RoleCover,
	guard *pidGuard,
) *trimmer {
	t := &trimmer{
		id:         randomID(),
		settings:   settings,
		trimming:   trimming,
		send:       send,
		now:        time.Now,
		ticks:      tickerChannel,
		logf:       logf,
		volumeRoot: VolumeRoot,
		procRoot:   "/proc",
		cover:      cover,
		self:       os.Getpid(),
		roots:      ReclaimableRoots,
		readUsage:  readVolumeUsage,
		measure:    measureTree,
		removals:   startRemover(guard),
	}
	t.boundReport = t.bound
	return t
}

// tickerChannel is the interval clock of the pod: one ticker, and its stop.
func tickerChannel(every time.Duration) (<-chan time.Time, func()) {
	ticker := time.NewTicker(every)
	return ticker.C, ticker.Stop
}

// trimEnding is how one interval, and one loop, of the trimmer ended. Only trimOff ends the
// trimmer while the launcher process lives — trimStopping ends run too, but with the container
// that process is PID 1 of: everything else leaves the trimmer in this launcher process with
// work still to do, which is what R1 of the design asks of it — a trimmer runs in each issue pod
// whenever settings are in force, for the life of that pod (dispatch://INFRA-2009 spec) — and
// what the settings reaching every pod at its next connection is for.
type trimEnding int

const (
	// trimGoesOn is no ending at all: the interval loop takes its next tick.
	trimGoesOn trimEnding = iota
	// trimWaits is the settings gone or refused, in an acknowledgement that carried none or
	// carried values outside the trimmer's ranges, at an interval or in the middle of a sweep:
	// the interval loop ends and run waits for settings it may run by again. A pod lives through
	// `enabled: false` and the operator turning it back on, and through a daemon rolled back to a
	// release without this work and the roll-forward after it, and keeps its trimmer.
	trimWaits
	// trimOff is the one ending that keeps the trimmer off while the launcher process lives:
	// volumeFaultLimit recovered panics in a row of one kind of its work have switched the
	// trimmer off (faultRow), and nothing starts another before the launcher process restarts
	// (fault, run).
	// The report saying so is all that is left to do, and run makes it and tries it before it
	// returns (switchOff, deliverLast).
	trimOff
	// trimStopping is the container going: ctx has ended, and run returns with it.
	trimStopping
)

// run is the trimmer's whole life in this launcher process: its work (live), which is the wait
// for settings it may run by, the interval loop those settings start and the wait again when
// they go away, and, once the faults have switched it off, its last report (switchOff,
// deliverLast). It returns on the container's stop, and on the faults once that report has been
// sent, discarded or has panicked, and never on settings alone, because a pod takes the settings
// of its bound connection's latest answer and an operator who turned the trimmer off may turn it
// on again in the pod this process is PID 1 of.
//
// It is where the trimmer contains every panic that tick does not recover (tick says why it
// recovers at all), and it sits here because run is the whole of what the trimmer's goroutine
// runs: serveVolumeTrim's goroutine waits for the first acknowledgement and calls podTrimmer,
// which calls run, and nothing else. So one recovery here holds every path of that goroutine
// outside tick's own: the wait for settings and the held report it tries (awaitSettings, flush),
// the interval loop's clock and lines (intervals), what tick's deferred handler runs once it has
// recovered (fault), and the last report.
//
// A panic of the work is a fault as tick's are (fault): written with its stack, kept for the next
// report and added to the row of the kind of work it interrupted (faultRow), toward
// volumeFaultLimit. The panics run catches outside tick's own recovery are counted so:
//   - the resend of a held report in the wait for settings adds to the sweep row, as the resend
//     before an interval's reading does: a held report is a sweep's report, and its resend that
//     report's delivery (flush);
//   - the wait's own reading of the settings and its lines, and the interval loop's clock and its
//     started: line, add to the reading row, the readings' being every work of the goroutine but a
//     sweep's. Nothing between two starts of the loop ends that row, so a clock or a line that
//     panics each time the loop starts switches the trimmer off at its third panic, where a panic
//     counted in no row would start the loop again for ever;
//   - a panic of the line tick's handler writes of a fault adds to the row of that fault. It is
//     raised in the recovery of that work, which is not passed over until its fault is counted and
//     written (fault), so it is one more panic of that work, kept with its stack for the next
//     report beside the panic it was writing about.
//
// The work then starts again from its wait for settings, the readings' work, unless that fault is
// the third in a row of its kind, which switches the trimmer off as tick's third does. A panic of
// the line run itself writes of a fault is counted in no row: run drops it, since the logger that
// raised it is the one place there is to write it, and the fault that line was about is counted
// already (fault). A panic in the last report, as switchOff makes and sends it or as deliverLast
// tries it again, is written with its stack and counted in no row: nothing is left to carry a
// fault to the daemon or to count it toward. It ends the trimmer, and that report is not tried
// again, since a report that panicked once can panic again, unless switchOff raised it once
// deliver held that report: the send has answered by then and what is left of deliver is its
// lines, whose panic only the logger can raise (deliver), so run writes that panic and still
// tries the held report (deliverLast), where a panic ends the trimmer but one of the line of a
// failed attempt, which deliverLast drops before it tries the report again. The line that says the
// trimmer is off ends nothing: run writes it once that report has been sent, held or has
// panicked, and drops its panic, as it drops the panic of the line of a fault and of the lines it
// writes of a panic of the last report. Each step runs inside contain and none inside a deferred
// handler, so a panic raised while another is being handled, in tick's handler or in the counting
// here, meets a recovery too and never escapes.
func (t *trimmer) run(ctx context.Context) {
	ending := trimGoesOn
	for ending == trimGoesOn {
		if recovered, stack := contain(func() { ending = t.live(ctx) }); recovered != nil {
			// fault counts the panic before it writes its line, so a panic of that line, which
			// only the logger can raise, leaves the fault counted and is dropped: the logger is
			// the one place there is to write it. The wait the work starts again from is the
			// readings', which fault leaves in force once its line is written and which is put
			// back here for a line that panicked.
			contain(func() { t.fault(recovered, stack) })
			t.doing = readingRow
			if _, off := t.switchedOffBy(); off {
				ending = trimOff
			}
		}
	}
	if ending != trimOff {
		return
	}
	recovered, stack := contain(t.switchOff)
	// The line that says the trimmer is off comes once its last report has been sent, held or has
	// panicked, so a panic of that line, which only the logger can raise, loses no report: it is
	// dropped, as above, and a report held is still tried (deliverLast), whether switchOff returned
	// or panicked once it had held it (below).
	contain(func() {
		row, _ := t.switchedOffBy()
		t.logf(volumeTrimLog+"%d panics in a row of %s: the trimmer is off for the life of this launcher process", t.inARow[row], row)
	})
	if recovered != nil && t.held != nil && t.held.SwitchedOff {
		// switchOff panicked once deliver held its report: the send had answered, so the panic is
		// of a line written after it, which only the logger can raise (deliver), and the report is
		// still tried, as one held without a panic is. This line says so, and its own panic is
		// dropped, as above.
		contain(func() {
			t.logf(volumeTrimLog+"the last report of the switched-off trimmer is held, and a line written once it was held panicked; the report is still tried until a connection takes it, the container stops or an answer with no settings discards it: %v\n%s",
				recovered, stack)
		})
		recovered = nil
	}
	if recovered == nil {
		recovered, stack = contain(func() { t.deliverLast(ctx) })
	}
	if recovered != nil {
		// A panic of this line too, which only the logger can raise, is dropped, as above.
		contain(func() {
			t.logf(volumeTrimLog+"the last report of the switched-off trimmer panicked, and the trimmer ends here without trying it again: %v\n%s",
				recovered, stack)
		})
	}
}

// live is the trimmer at work: the wait for settings it may run by, the interval loop those
// settings start, and the wait again when they go away, until the container stops or the faults
// switch the trimmer off.
func (t *trimmer) live(ctx context.Context) trimEnding {
	for {
		inForce, ending := t.awaitSettings(ctx)
		if ending != trimGoesOn {
			return ending
		}
		if ending := t.intervals(ctx, inForce); ending != trimWaits {
			return ending
		}
	}
}

// contain runs work and answers the panic it raised, with the stack it was raised on, or nil
// when it raised none (run).
func contain(work func()) (recovered any, stack []byte) {
	defer func() {
		if recovered = recover(); recovered != nil {
			stack = debug.Stack()
		}
	}()
	work()
	return nil, nil
}

// awaitSettings is the wait for settings the trimmer may run by: it answers the first ones it
// can run, or the container's stop. Nothing of the trimmer's own counting is touched here — the
// identifier, the episode, the anchor, the last reported start and the pause are this pod's
// counting line and go on across a trimmer that was turned off and on again — and no interval
// ticker is made, so a trimmer waiting sweeps nothing and reads nothing, which is what R2 asks of
// settings outside the ranges. The one thing it does change is a report the trimmer is still
// holding, which an answer with no settings discards (settingsInForce) and refused settings have
// tried, as R2 has it: the interval loop stops on either and leaves them to this wait (tick).
//
// It polls rather than being woken: the seam is a read of what the latest acknowledgement left
// in force (manager.volumeTrim) and nothing signals a change of it. It says what it is waiting
// for when that changes — the first time, and again when the reason changes — and never once
// per poll, since the pod's log is what a person reads and a line every volumeSettingsWait
// would bury the rest of the launcher's. The memory that holds it to that is the trimmer's own
// (news, said), which the switched-off trimmer's failed sends share (deliverLast).
func (t *trimmer) awaitSettings(ctx context.Context) (shimwire.LauncherVolumeTrim, trimEnding) {
	var ticks <-chan time.Time
	stop := func() {}
	defer func() { stop() }()
	for {
		set := t.settingsInForce()
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
		if t.news(reason) {
			if refusals == nil {
				t.logf(volumeTrimLog + "no settings are in force, so the trimmer stays off until the daemon sends settings it can run")
			} else {
				t.refuse(refusals)
			}
			// Remembered once its lines are written, and not before: a panic of one of them, which
			// only the logger can raise, is a fault of the reading row, after which run starts this
			// wait again (run), and the reason is still news there, so its lines are written again
			// and a held report is tried; a logger that panics on every such line switches the
			// trimmer off at its third panic. The held report is tried once the reason is
			// remembered, because run starts the wait again at once, with no tick between: a resend
			// that panics, a fault of the sweep row (flush), would otherwise be tried three times
			// in a row over one refusal and switch the trimmer off. Remembered first, that panic is
			// counted once, and the report stays held for the first interval of settings the
			// trimmer can run, as it does when its send fails.
			t.said = reason
			if refusals != nil {
				// R2 has only an answer with the member absent discard a held report, because
				// that daemon has no case for one; the daemon that sent these settings does
				// (manager.sendVolumeReport writes on any connection whose answer carries
				// settings), and a held report is a volume nobody was told about. A send that
				// fails keeps it held, for the first interval of settings the trimmer can run
				// (tick's flush) or for an answer with no settings, which discards it.
				t.flush()
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

// settingsInForce is the settings the latest acknowledgement left in force, with R2's rule for an
// answer that carries none applied to a report the trimmer is holding: it is discarded, because
// that daemon has no case for a report (manager.sendVolumeReport). The waiting trimmer
// (awaitSettings) and the switched-off one (deliverLast) both read their settings here.
func (t *trimmer) settingsInForce() *shimwire.LauncherVolumeTrim {
	set := t.settings()
	if set == nil {
		t.held = nil
	}
	return set
}

// intervals is the interval loop those settings start: one reading per tick and the sweep a
// reading at the high mark asks for, until the container stops, the faults stop the trimmer, or
// the settings go away and run waits for them again. It is entered with settings the trimmer
// has already accepted and starts its ticker on nothing else: time.NewTicker panics on a
// non-positive period, and a panic here, of the clock or of the started: line, is a fault of the
// reading row (faultRow) that run counts toward switching the trimmer off, where settings outside
// the ranges are only refused (awaitSettings, run).
func (t *trimmer) intervals(ctx context.Context, inForce shimwire.LauncherVolumeTrim) trimEnding {
	t.logf(volumeTrimLog+"started: trimmer %s, high %d%%, low %d%%, minimum age %d min, interval %d s, volume %s",
		t.id, inForce.HighPercent, inForce.LowPercent, inForce.MinAgeMinutes, inForce.IntervalSeconds, t.volumeRoot)
	// A fresh run of this loop is a new standing state, as a fresh set of settings is for said:
	// a volume this process still cannot read is news again under the settings that came back,
	// and without this the started: line above would be the last thing the pod's log carried for
	// as long as the fault lasts. It is cleared here rather than in saidNothing because tick
	// calls that on every interval whose settings are runnable, and a failing statfs happens on
	// those intervals as much as on any other: clearing it there would put the read-error line
	// back to one per interval and undo what TestAVolumeTheTrimmerCannotReadIsSaidOnce pins.
	t.saidRead = ""
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

// news is whether a standing state of the trimmer's is news: one other than the last it wrote
// (said). A refusal is a state rather than an event — a daemon that widened a maximum this
// launcher does not know sends the same settings with every acknowledgement, and the waiting
// trimmer meets them once every volumeSettingsWait (awaitSettings) — so saying it per poll would
// put at least two lines (refuse) into the roles container's log every ten seconds for the life of
// the pod, and that log is the only place this work is visible to a person after a deploy. The
// caller remembers the state in said once it has written its lines, so a line of the wait for
// settings that panics leaves the state news for the wait run starts again (awaitSettings).
// deliverLast remembers its state in the same order and drops a panic of its line, so a line of a
// failed attempt that panicked is written at the next attempt that fails (deliverLast).
func (t *trimmer) news(state string) bool {
	return state != t.said
}

// saidNothing clears said, which settings the trimmer can run do: the pod is out of the state it
// was in, so the same refusal arriving later is news again rather than the one the log already
// carries.
func (t *trimmer) saidNothing() {
	t.said = ""
}

// settingsInterval is the period of the interval clock.
func settingsInterval(set shimwire.LauncherVolumeTrim) time.Duration {
	return time.Duration(set.IntervalSeconds) * time.Second
}

// tick is one interval: the settings in force, a held report, one reading of the volume, the
// episode that reading raises, the sweep it asks for, the recovery that sweep's last reading
// raises where it leaves the volume at or below the low mark, and the rise that sweep's start
// raises where it is a reported sweep (riseAt). restart says the interval clock must be made
// again, and the ending what the interval loop must do now (trimEnding).
//
// It recovers from any panic in its own work, which is a deliberate departure from this daemon's
// practice: a process that cannot keep its invariants should die and be restarted, so no
// production path of it outside this trimmer recovers a panic it did not expect, but in two cases.
// One is leakguard's compileRules, which turns the panic regexp.MustCompile raises on a pattern of
// the deployment's private-name rule file into an error. The other is the memory guard, which
// recovers its loop (Guard.step) and its scan (scanLargest), as the microVM guest watch recovers
// its loop (Guest.step) and its wake's reader (readModified), for this trimmer's reason: that code
// runs in PID 1 of the pod. This goroutine runs in the process that is PID 1 of the roles
// container, so its panic takes the container down and costs six live roles their round, over a
// cache sweep that nothing depends on. The fault is logged with its stack and carried into the
// next report, and three in a row of one kind of work stop the trimmer until the launcher process
// restarts (fault, run), which no other ending of the four does.
//
// Each kind keeps a row of its own (faultRow). The reading's is the interval's reading of the
// settings and of the volume, with the line that says what the interval decided
// (intervalReading), and a reading run to its end ends it, one that failed to read the volume
// included, since that is a reading that answered rather than one that panicked. The sweep's is
// the sweep, what this tick does with its outcome and its report, and the resend of a held report
// before the reading (flush), and a sweep run to its end without a fault ends it, with its report
// where it makes one, one the settings stopped included. A panic of one kind neither ends the
// other's row nor adds to it. So a volume whose reading panics now and then, with readings run
// to their end between, stays guarded however many such panics come, and a trimmer whose every
// report panics, with the pause after each sweep, still switches itself off at its third panic:
// the readings of the pause end the readings' row and not the sweeps'.
//
// The recovery is here and not in run alone, which contains every panic of the goroutine, so a
// panic passes over this interval and no more: the interval loop goes on with its clock and takes
// its next tick, where a panic left to run would start the loop again from the wait for settings,
// with a started: line and a clock of its own for every fault. A panic leaves restart and the
// ending as this tick set them before it (the handler below): a tick that changed
// interval_seconds and then panicked still has the interval clock made again at the new period,
// and one that found the settings gone ends the loop for the wait all the same. The row the panic
// is counted in is the work in flight's (doing), which this tick sets as it moves from the
// reading to the sweep and which run reads the same way. The rest of the goroutine, this
// recovery's own handler included, is run's to contain (run).
func (t *trimmer) tick(ctx context.Context, inForce *shimwire.LauncherVolumeTrim) (restart bool, ending trimEnding) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}
		// restart and ending are left as this tick set them before the panic. restart is true
		// where these settings changed interval_seconds, and inForce already holds the new period
		// then, so a clock kept at the old one would tick at a period no settings name any more
		// for the rest of the loop, and where a sweep has begun, whatever step of it panicked,
		// whose next reading comes one interval after this tick and not on the clock the sweep
		// began on (intervals). ending is trimWaits where the settings were found gone before the
		// line that says so, at the interval or in the sweep, and the loop ends for the wait,
		// which deals with a held report as R2 has it (awaitSettings). Every other restart and
		// ending is set by a return, which a panic never reaches.
		if t.fault(recovered, debug.Stack()) {
			ending = trimOff
		}
	}()

	next := t.settings()
	if next == nil {
		// Decided before its line, so a panic of the line, which only the logger can raise,
		// still ends the loop for the wait rather than keeping it on its clock for an interval.
		ending = trimWaits
		t.logf(volumeTrimLog + "no settings are in force: the trimmer sweeps nothing more until they come back, and a report it was holding is discarded")
		return false, trimWaits
	}
	if len(refusedTrimSettings(*next)) > 0 {
		return false, trimWaits // said, and a held report tried, in the wait (awaitSettings)
	}
	t.saidNothing()
	// The interval begins here, once settings it can run by are in force, so a tick that waits
	// for settings spends none of the pause. The pause is counted in intervals and not in
	// readings, as R1 has it ("ten intervals, counted from the end of that sweep"): counted down
	// here, before every step of the interval that can fail or panic — the line of new settings,
	// the resend of a held report (flush), the statfs and the line of the pause — so an interval
	// that fails or panics at any of them still spends one of the pause. The line of the pause
	// names the intervals left as it did, this one among them (intervalReading).
	left := t.pause
	if left > 0 {
		t.pause--
	}
	if *next != *inForce {
		// In force before their line, so a panic of the line, which only the logger can raise,
		// still has this tick's clock made again at a new period and the next interval run by
		// them; the line is then not written, as a rise's is not where its line panics (riseAt).
		restart = next.IntervalSeconds != inForce.IntervalSeconds
		*inForce = *next
		t.logf(volumeTrimLog+"new settings: high %d%%, low %d%%, minimum age %d min, interval %d s",
			next.HighPercent, next.LowPercent, next.MinAgeMinutes, next.IntervalSeconds)
	}
	set := *inForce

	t.flush()
	reading, sweeping := t.intervalReading(set, left)
	t.ran(readingRow)
	if !sweeping {
		return restart, trimGoesOn
	}

	t.doing = sweepRow
	// The clock is made again for a sweep, so the next reading comes one interval after this tick
	// ends (intervals) and not on the clock the sweep began on, whose tick may have come due while
	// the sweep ran: set here, before the sweep, so a panic at any step of it, of a line about it
	// or of its report still has the clock made again.
	restart = true
	out := t.sweep(ctx, set, reading)
	t.last = out.last
	// The sweep's last reading is the trimmer's own reading as the interval's is, so the episode
	// rules read it too: a sweep that leaves the volume at or below the low mark ends the episode,
	// where the next interval's reading may already stand above that mark again. Only a recovery
	// can come of it (episodeOf), which writes a line only where the sweep reached the low mark
	// and so never for a reported sweep.
	t.episodeOf(out.last.Percent(), set)
	if out.settingsStop {
		// A sweep the settings cut sends no report of its own: the report would need a size walk
		// of up to a minute (sizes) from a trimmer that is off. What the settings do to a held
		// report is the wait's (awaitSettings).
		// The ending is decided before the line, as for an answer with no settings above; the
		// sweep row is ended after it, since the line is the sweep's work and its panic a fault
		// of that work.
		ending = trimWaits
		t.logf(volumeTrimLog + "the settings stopped the trimmer: the sweep ended at an entry boundary with its deletions standing, and no report is sent for it")
		t.ran(sweepRow) // a sweep run to its end without a fault, which makes no report: it ends the sweep row, as below
		return false, trimWaits
	}
	// The pause is set before any line about the sweep is written, the rise's included, so a
	// panic of one of those lines, which only the logger can raise, still holds the next ten
	// intervals off sweeping, as R1 has it follow every sweep that freed under a point; its own
	// lines stay below the sweep's. The recovery line episodeOf can write above comes only of a
	// sweep that started at or above the high mark and ended at or below the low one, which freed
	// a point or more and sets no pause.
	if out.freedPoints < volumeSweepMinFreedPoints {
		t.pause = volumeQuietIntervals
	}
	if out.report {
		// Read here, once the sweep has returned and before this tick writes its lines about the
		// sweep or makes its report, so a reported sweep whose line here or whose report then
		// panics has been read all the same, as riseAt says of every reported sweep. The lines
		// the sweep writes itself come before this: a panic of one written before the sweep has
		// settled why it ends is the sweep's own, and that sweep never returns, so it is not read,
		// where one written after is dropped and the sweep is read (sweep). The sweep's refused
		// /proc reads need no hand-over here or below: the sweep gave each to the trimmer the
		// moment it met it (sweep, carryRefused).
		t.riseAt(reading.Percent())
	}
	// The reading is written as the sweep left it rather than as another "the volume is" line,
	// which is the interval reading alone: an operator reading the pod's log counts the
	// readings to see the trimmer is alive. A sweep whose last reading is above its first freed
	// nothing, and this line and the pause's below say the volume grew by the difference while
	// the sweep ran, as the notice of its report says it (internal/workflow's
	// volumeNoticeReason).
	freed := fmt.Sprintf("%d B freed", out.freed)
	if out.freed < 0 {
		// The magnitude as uint64, which holds it for every int64, the least one included.
		freed = fmt.Sprintf("the volume grew by %d B while the sweep ran", uint64(-out.freed))
	}
	t.logf(volumeTrimLog+"the sweep ended: %s. %d entries deleted, %s, leaving the volume at %.1f%% (used %d B, free %d B)",
		out.reason, out.deletions, freed, out.last.Percent(), out.last.UsedBytes, out.last.AvailableBytes)
	if out.freedPoints < volumeSweepMinFreedPoints {
		if out.freedPoints < 0 {
			t.logf(volumeTrimLog+"the volume grew by %.2f points while the sweep ran, so the sweep freed under %.0f: no new sweep for the next %d intervals",
				-out.freedPoints, volumeSweepMinFreedPoints, volumeQuietIntervals)
		} else {
			t.logf(volumeTrimLog+"the sweep freed %.2f points of the volume, under %.0f: no new sweep for the next %d intervals",
				out.freedPoints, volumeSweepMinFreedPoints, volumeQuietIntervals)
		}
	}
	if out.report {
		t.deliver(t.report(ctx, out))
	}
	// The sweep, and its report where it made one, ran to their end without a fault, which ends
	// the sweep row (faultRow), and nothing else does: a reading, in a pause, below the high mark
	// or one that failed, ends the readings' row alone. So a trimmer whose every report panics,
	// with the pause after each sweep, still switches itself off at its third panic and tries its
	// switched-off report (R1: "after three in a row it stays off and sends one last report
	// saying so").
	t.ran(sweepRow)
	return true, trimGoesOn
}

// intervalReading is the interval's reading of the volume: the statfs, the episode it raises, the
// line that says what the interval decided, and whether that is a sweep, which it is at or above
// the high mark outside a pause. left is the intervals of the pause left, this one among them,
// which tick counted down as the interval began, so an interval whose reading fails or panics has
// spent one of the pause already, as one whose line of the pause panics has. A volume it cannot
// read asks for no sweep. It is the readings' work as tick counts its faults (faultRow): a panic
// in it adds to the reading row, and its return, whatever it answers, is a reading run to its
// end, which ends that row (tick).
func (t *trimmer) intervalReading(set shimwire.LauncherVolumeTrim, left int) (volumeUsage, bool) {
	reading, err := t.readUsage(t.volumeRoot)
	if err != nil {
		// Said when it changes and not once per interval, for the reason a refusal is (news): a
		// volume this process cannot read is a standing state, and the statfs behind it answers
		// the same way every interval for as long as it lasts. Remembered once its line is
		// written, as a refusal is (awaitSettings), so a panic of the line, which only the logger
		// can raise, leaves the error news for the next interval, which writes it.
		if why := err.Error(); why != t.saidRead {
			t.logf(volumeTrimLog+"the volume at %s could not be read: %v", t.volumeRoot, err)
			t.saidRead = why
		}
		return volumeUsage{}, false
	}
	t.saidRead = ""
	t.last = reading
	percent := reading.Percent()
	t.episodeOf(percent, set)
	switch {
	case left > 0:
		t.logf(volumeTrimLog+"the volume is %.1f%% full (used %d B, free %d B): no sweep for another %d intervals, since the last one freed under %.0f point",
			percent, reading.UsedBytes, reading.AvailableBytes, left, volumeSweepMinFreedPoints)
		return reading, false
	case percent < float64(set.HighPercent):
		t.logf(volumeTrimLog+"the volume is %.1f%% full (used %d B, free %d B): below the high mark %d%%, nothing to sweep",
			percent, reading.UsedBytes, reading.AvailableBytes, set.HighPercent)
		return reading, false
	}
	t.logf(volumeTrimLog+"the volume is %.1f%% full (used %d B, free %d B): at or above the high mark %d%%, sweeping to %d%%",
		percent, reading.UsedBytes, reading.AvailableBytes, set.HighPercent, set.LowPercent)
	return reading, true
}

// episodeOf raises the episode number on the reading, when the reading is the first at the high
// mark or a recovery, and leaves it alone otherwise. A rise, the third thing that raises it, is
// read at the start of a reported sweep alone (riseAt).
//
// An episode is the run of readings the daemon's three-notice bound counts against, so raising it
// starts that count over for the reports that follow and sends nothing itself: a report is still a
// sweep's, and only of one that ends above the low mark (tick), the one the trimmer sends when it
// switches itself off aside (switchOff). A recovery is a reading of the trimmer's own at or below
// the low mark after one at or above the high mark: the next time the volume fills is news. The
// boundary is the sweep's own, which stops at the first reading at or below the low mark and
// reports only where it ends above it (sweep), so a sweep that ends exactly at the mark is a
// recovery and sends no report, as one that ends under it is. Every interval reading is read
// here, one inside a pause and one below the high mark included, because either can be the first
// at the high mark or a recovery, and so is each sweep's last reading (tick), which follows an
// interval reading at the high mark and so can only be a recovery; none of them raises a rise. A
// fall to the low mark or under it while the trimmer waits for settings it can run
// (awaitSettings) is read by neither, so when the trimmer runs again the episode and its chain go
// on as it left them, unless its first reading then is at or below the low mark, which is a
// recovery.
//
// Each recovery starts a new chain, anchored afresh (riseAt), so a volume that cycles can be told
// about each cycle: its recovery episode's reports wait for the pod's hour, and a rise, raised in
// each cycle where two reported sweeps in a row start five points or more above the lowest start
// of that cycle's reported sweeps before them, is told without waiting. Each rise episode of a
// cycle costs up to three notices that skip the hour (internal/workflow's volumeNoticeHeldBy), and
// a cycle whose anchor is A at its first rise holds up to (100 − A) / 5 rise episodes, rounded
// down (riseAt), so it costs at most 3 × ⌊(100 − A) / 5⌋ such notices, nine at the default high
// mark of 85, besides its recovery episode's reports, which come at most one an hour. What fills
// of a given shape cost a pod in a day, measured through this loop and those bounds, is in
// docs/kubernetes.md, in the paragraph on `runtime.kubernetes.volume_trim` under "Configuration"
// of "Kubernetes runtime: the Go daemon on Agent Sandbox".
func (t *trimmer) episodeOf(percent float64, set shimwire.LauncherVolumeTrim) {
	// Marked before either line below is written, so a panic of that line leaves the volume
	// marked: a reading at or above the high mark can raise no recovery, the low mark being below
	// the high one, so the order changes nothing else.
	if percent >= float64(set.HighPercent) {
		t.aboveHigh = true
	}
	switch {
	case t.episode == 0:
		if percent < float64(set.HighPercent) {
			return
		}
		t.episode, t.raisedBy = 1, shimwire.VolumeEpisodeFirst
		t.logf(volumeTrimLog+"episode %d (%s): the volume reached the high mark %d%% at %.1f%%",
			t.episode, t.raisedBy, set.HighPercent, percent)
	case t.aboveHigh && percent <= float64(set.LowPercent):
		t.episode, t.raisedBy, t.aboveHigh, t.anchored = t.episode+1, shimwire.VolumeEpisodeRecovery, false, false
		t.logf(volumeTrimLog+"episode %d (%s): the volume fell to %.1f%%, at or below the low mark %d%%",
			t.episode, t.raisedBy, percent, set.LowPercent)
	}
}

// riseAt reads a rise at start, the reading a reported sweep started from. A reported sweep, here
// and wherever this trimmer's comments and shimwire's VolumeEpisodeRise name one, is a sweep that
// ended above the low mark and that the settings did not stop: tick reads its start here once the
// sweep has returned and the settings are known not to have stopped it, before tick writes its
// own lines about the sweep and before its report is made, so the report carries the episode its
// start raised, and so a sweep whose line in tick or whose report then panics, which tick recovers
// and counts as a fault, is a reported sweep too, read although it sends no report. A sweep that
// panics before it returns, in one of the lines it writes before it has settled why it ends
// included, is never read; a panic of a line it writes after that is dropped, and the sweep is
// read (sweep). The other two sweeps are not read, their start neither anchoring nor entering the
// two a rise reads: one that ended at or below the low mark, since the trimmer brought that volume
// down, and one the settings stopped (tick).
//
// A rise is read where the lower of the last two reported sweep starts, this one and the one
// before it, stands five points or more above the episode's anchor. An episode that no rise
// raised, the first and each after a recovery, is anchored at the lowest start of its reported
// sweeps: the first reported sweep's start anchors it, and each later one that starts lower lowers
// it, so its second reported sweep can never rise. An episode a rise raised is anchored at the
// floor that raised it, the lower of the two starts read, and that anchor never moves; the rise
// sweep's own start is the first of the two its next rise reads. So one high start raises nothing
// by itself, which keeps a peak that recurs at sweep starts from raising a rise each time; a
// transient reading at the first start of an episode no rise raised is lowered by the first later
// start that reads under it, so it cannot silence a later climb; and a rise's anchor is the lower
// of two starts, never one transient reading, where the anchor of an episode no rise raised is its
// lowest single start, which one low start lowers. A transient that two reported starts in a row
// read does raise a rise anchored at its own level, after which a fill from lower down is read
// only once two starts stand five points above that level, and never where the level is above
// 95%. The cost of the two starts is that a rise of a volume that keeps filling is read one
// reported sweep after the first reported sweep that starts five points or more above the anchor:
// an interval later where that sweep frees a point or more and leaves the volume at or above the
// high mark, and an interval and its pause, eleven intervals, where it frees less.
//
// A chain is the run of episodes from the first, or from a recovery, to the next recovery. Its
// anchor is lowered only before its first rise; each rise then lifts it five points or more, and
// it never comes down, and a reading is at most 100%, so a chain whose anchor stood at A when its
// first rise was read holds at most (100 − A) / 5 rises, rounded down. Every anchor is a reported
// sweep's start, which is at or above the high mark that sweep ran by, so a chain anchored at the
// high mark holds at most (100 − high mark) / 5: three at the default of 85. One anchored under a
// mark raised later can hold more than the new mark's figure, up to its own anchor's: anchored at
// 50 under a high mark of 50 and then run at 85, up to ten. The first reported sweep under the new
// mark pairs its start with the last one under the old (min(prev, start)), so starts of 50,
// 52 and 80 under a mark of 50 and then two each of 85, 90, 95 and 100 under 85 raise five rises,
// the first at the floor of 80% and 85%. None holds more than nineteen, from an anchor of 2, the
// lowest high mark the settings allow. An anchor above 95% can never be passed, and from one of
// 95% a rise needs two reported sweeps in a row that start at exactly 100.0%, not one reading.
// Only a recovery starts a new chain, anchored afresh (episodeOf).
//
// The anchor is a reading taken before a sweep, never the figure a report carried, which is a
// post-sweep low point: anchored there, the ordinary growth from the low point back to the high
// mark would read as a five-point deterioration and wake the architect on every cycle.
func (t *trimmer) riseAt(start float64) {
	prev := t.lastStart
	t.lastStart = start
	switch floor := min(prev, start); {
	case !t.anchored:
		t.anchor, t.anchored = start, true
	case floor >= t.anchor+volumeRisePoints:
		anchor := t.anchor
		t.episode, t.raisedBy, t.anchor = t.episode+1, shimwire.VolumeEpisodeRise, floor
		t.logf(volumeTrimLog+"episode %d (%s): the floor of the last two reported sweep starts, %.1f%% and %.1f%%, is %.1f%%, %.1f points above the %.1f%% the last episode was anchored at, where %.0f points raise a new episode",
			t.episode, t.raisedBy, prev, start, floor, floor-anchor, anchor, volumeRisePoints)
	case t.raisedBy != shimwire.VolumeEpisodeRise:
		t.anchor = min(t.anchor, start)
	}
}

// faultRow is a kind of the trimmer's work, which counts its own row of recovered panics toward
// volumeFaultLimit (fault): R1 has the trimmer switch itself off "after three in a row", and a
// panic of one kind with work of the other kind run between is not in a row with it. One row
// that only a sweep ended, round 35's rule, switched off a volume below the high mark, which never
// sweeps, at its third reading panic however many readings ran between (INFRA-2009: the
// architect's comment ccc812fc, which reports that run and sets these two rows, round 36's rule);
// one that any reading ended, round 34's, let a trimmer whose every report panics fault for the
// life of the pod, since the readings of the pause after each sweep would end it.
type faultRow int

const (
	// readingRow is the readings' work, which is every work of the trimmer's goroutine but a
	// sweep's: the interval's reading of the settings and of the volume and the line that says
	// what it decided (intervalReading), the interval loop's clock and its started: line
	// (intervals), and the wait for settings (awaitSettings). A reading run to its end ends the
	// row (tick).
	readingRow faultRow = iota
	// sweepRow is a sweep's work: the sweep, what tick does with its outcome, its report, and
	// the resend of a held report wherever it is tried, before an interval's reading or in the
	// wait for settings, since a held report is a sweep's report and its resend that report's
	// delivery (flush). A sweep run to its end without a fault ends the row, with its report
	// where it makes one, one the settings stopped included (tick).
	sweepRow
)

// String is the row as the fault line and the switched-off trimmer's report name it.
func (row faultRow) String() string {
	if row == sweepRow {
		return "sweeps and reports"
	}
	return "readings"
}

// fault records one recovered panic in the row of the work it interrupted (doing) and answers
// whether that row has now reached volumeFaultLimit, which keeps the trimmer off: tick then ends
// the interval loop with trimOff, and run switches the trimmer off and makes its last report
// (switchOff), outside any deferred handler. It counts the panic before it writes its line, so a
// panic of the line leaves the fault counted. Where tick's handler called it, that line's panic
// goes on to run, which counts it in the same row, since the work is passed over only once its
// fault is written (run); where run called it, run drops that line's panic, which is counted in
// no row (run). What follows is the readings' work (doing), which run puts back where the line
// panicked.
//
// A panic in a line the logger writes can skip what follows that line in the same interval, and
// PID 1 keeps running. It is counted as a fault in its row, but for four kinds of line, which are
// counted in no row (run, sweep): the lines run writes of a panic it caught, which run drops,
// since the logger that raised it is the one place there is to write it and the panic each was
// about is counted already or is the last report's; the line that says the trimmer is off, which
// run drops as well; the four kinds of line a sweep writes once it has settled why it ends — that
// its deleter could not be started or ended, that the volume could not be read at its end, and,
// one line for each root past the bound, how many more of its entries it passed over than it
// named — which the sweep drops, so tick still acts on that sweep's outcome; and the lines of the
// last report, as switchOff makes and sends it or as deliverLast tries it again, where a panic
// ends the trimmer unless that report is still held when it comes, which the trimmer then still
// tries: run after a line of switchOff's, and deliverLast at its next attempt after the line of a
// failed one (switchOff, deliverLast). Where a line written once the last report was sent or
// discarded panics, saidLeftOut's line of what the bound left out, deliverLast's line that says
// the report was sent or its line that says an answer with no settings discarded it, run's
// closing line says the report panicked and was not tried again, although the daemon has it or,
// for the last of those lines, R2 had it discarded.
//
// Each row counts the recovered panics of its kind since the last work of that kind ran to its
// end (ran, tick), a panic tick recovered and one of the work run contained alike, and a panic of
// the other kind neither ends it nor adds to it. So a trimmer whose every report panics, with the
// pause after each sweep, switches itself off at its third panic instead of faulting for the life
// of the pod with nothing beyond its log to show for it, since the readings of the pause end the
// readings' row and not the sweeps'; and a volume whose reading panics now and then, with
// readings run to their end between, is not switched off by those panics however many come,
// where three readings in a row that panic switch it off.
//
// Only the newest shimwire.VolumeReportFaults stacks are kept, of both rows together, and what
// they displace is counted (keepNewestFaults).
func (t *trimmer) fault(recovered any, stack []byte) bool {
	row := t.doing
	t.inARow[row]++
	t.faults = append(t.faults, fmt.Sprintf("%v\n%s", recovered, stack))
	t.keepNewestFaults()
	t.logf(volumeTrimLog+"recovered a panic in the trimmer's own work (%d in a row of %s), passing over what it was doing: %v\n%s",
		t.inARow[row], row, recovered, stack)
	t.doing = readingRow
	return t.inARow[row] >= volumeFaultLimit
}

// ran ends the row of the work that just ran to its end without a panic, and what follows is the
// readings' work (doing): a reading's row at the reading's end, and a sweep's once the sweep and
// its report where it makes one have run to theirs, one the settings stopped included (tick).
func (t *trimmer) ran(row faultRow) {
	t.inARow[row] = 0
	t.doing = readingRow
}

// switchedOffBy is the row that has reached volumeFaultLimit, which switches the trimmer off (run,
// switchOff), and false while neither has.
func (t *trimmer) switchedOffBy() (faultRow, bool) {
	for row, faults := range t.inARow {
		if faults >= volumeFaultLimit {
			return faultRow(row), true
		}
	}
	return readingRow, false
}

// keepNewestFaults cuts the faults the trimmer holds to the newest shimwire.VolumeReportFaults and
// counts what it cuts (fault, takeBack). The faults are cleared when a report carries them
// (report), so a pod whose volume stays healthy never reports and would otherwise keep every stack
// it ever recovered for the life of the launcher process, kilobytes each, in the process that is
// PID 1 of the roles container.
func (t *trimmer) keepNewestFaults() {
	if over := len(t.faults) - shimwire.VolumeReportFaults; over > 0 {
		t.faultsDropped += over
		t.faults = slices.Delete(t.faults, 0, over)
	}
}

// cutEach is list with every entry cut to at most runes, and nil for an empty one: a report
// carries no list where the trimmer had none.
func cutEach(list []string, runes int) []string {
	if len(list) == 0 {
		return nil
	}
	out := make([]string, len(list))
	for at, one := range list {
		out[at] = redact.Runes(one, runes)
	}
	return out
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
	if line := walkShortfall(t.walk); line != "" {
		t.logf(volumeTrimLog+"%s", line)
	}
	return t.walk
}

// walkShortfall is the line a walk that measured less than the whole volume writes, and "" for
// one that measured all of it. It says each of three facts only where the walk met it, since
// they are three different things to a person reading the pod's log: a walk its budget or the
// container's stop cut (walkResult.Partial), each size it could read only in part, named with
// the first read under it the walk did not make — one the kernel refused, or a directory deeper
// than the walk descends (LauncherVolumeSize.Refused) — and each target it measured none of,
// or the volume root or an immediate child of it that it did not list in full, with the read
// the kernel refused where there was one (walkResult.Missing). How many sizes it measured, and
// how many of them only in part, count the sizes the walk dropped from its fifty too
// (walkResult.SizesOmitted, PartlyOmitted).
//
// The names are the pod's own: a role may make a directory or a file of any name two levels
// under the volume root, and any number of them, so each list takes the bound the frame takes
// (bound) and each path is quoted, rather than joining a list of a size and a content nothing
// here chose. A reader who needs the whole of either list has the report's own, bounded the same
// way.
func walkShortfall(walk walkResult) string {
	// The walk keeps no more sizes than a report carries (walkResult.keep).
	var partly []string
	for _, size := range walk.Sizes {
		if size.Refused != "" {
			partly = append(partly, fmt.Sprintf("%q at %q",
				redact.Runes(size.Path, volumeReportPathRunes), redact.Runes(size.Refused, volumeReportPathRunes)))
		}
	}
	partlyAll := len(partly) + walk.PartlyOmitted
	if !walk.Partial && partlyAll == 0 && len(walk.Missing) == 0 {
		return ""
	}
	var line strings.Builder
	line.WriteString("the size walk ")
	if walk.Partial {
		line.WriteString("was cut by its budget or the container's stop before it reached every target: it ")
	}
	fmt.Fprintf(&line, "measured %d sizes", len(walk.Sizes)+walk.SizesOmitted)
	if partlyAll > 0 {
		fmt.Fprintf(&line, ", %d of them only in part, a read under each refused by the kernel or deeper than the walk descends", partlyAll)
		if len(partly) > 0 {
			fmt.Fprintf(&line, " (%s", strings.Join(partly, ", "))
			if walk.PartlyOmitted > 0 {
				fmt.Fprintf(&line, ", and %d more", walk.PartlyOmitted)
			}
			line.WriteString(")")
		}
	}
	if len(walk.Missing) > 0 {
		// The walk keeps no more of the list than a report names (walkResult.miss).
		missing := make([]string, len(walk.Missing))
		for at, one := range walk.Missing {
			missing[at] = strconv.Quote(redact.Runes(one.Path, volumeReportPathRunes))
			if one.Reason != "" {
				missing[at] += " (" + strconv.Quote(redact.Runes(one.Reason, volumeReportPathRunes)) + ")"
			}
		}
		and := " and"
		if partlyAll > 0 {
			and = ", and"
		}
		fmt.Fprintf(&line, "%s left %d unmeasured (%s", and, len(walk.Missing)+walk.MissingOmitted, strings.Join(missing, ", "))
		if walk.MissingOmitted > 0 {
			fmt.Fprintf(&line, ", and %d more", walk.MissingOmitted)
		}
		line.WriteString(")")
	}
	return line.String()
}

// sweepOutcome is one sweep as the interval reads it.
type sweepOutcome struct {
	last        volumeUsage // the sweep's last reading, which the report carries
	freed       int64       // the first reading's used bytes less the last's
	freedPoints float64     // the same difference in percentage points of the volume
	reason      string      // why the sweep stopped, in the trimmer's own words
	deletions   int
	report      bool // the sweep ended with the volume still above the low mark
	// settingsStop is the one ending the interval must act on itself: the settings stopped the
	// trimmer mid-sweep, so the deletions stand, no report is sent and the loop ends.
	settingsStop bool
}

// sweepDir is one directory the listing read entries from, with the handle it opened on it: a
// reclaimable root, or a shard directory of the Go build cache under one.
//
// The handle is what makes the sweep safe. Every check after the listing used to be made of the
// entry's path string and os.RemoveAll resolved that string once more, so each of them was its
// own moment for a role to rename a cache root — /legion/scratch is writable by the role uid,
// `drwx--S--- legion legion`, measured in an issue pod of this deployment at
// 2026-11-11T08:05Z — and leave a symbolic link in its place. The launcher runs as the roles'
// own uid in the container's mount namespace, where /var/run/legion/roles holds every role's
// private, state and agent-secrets directories, so a link left there aims the sweep at another
// role's files (INFRA-2009, review 6). A handle holds the descriptor the listing opened, which
// keeps naming that directory whatever the path later names, and it is opened one element at a
// time with the flags that refuse a link and never wait on a FIFO (dirHandle, volumedir.go).
//
// It is the handle of the directory that holds the entry and not of the root above it, and the
// entries are named in it by their own name alone, because a name with a separator in it is
// resolved component by component and follows a symbolic link it meets on the way: measured
// under go1.27.3, a Root on a directory holding `escape -> /elsewhere` deleted /elsewhere/x for
// RemoveAll("escape/x"), with no error. A name with no separator cannot be resolved through
// anything but the descriptor itself.
//
// Below the entry's own name the removal walks the entry through descriptors of its own and
// follows no link (removeEntryAt, volumedir_linux.go), and it is made in the deletions' own mount
// namespace, which bounds what a removal that did follow one would reach; the descriptor below
// is what the deleter is handed (volumedelete.go).
//
// Nothing asks again afterwards whether the handle still names the directory the listing read.
// The handle holds the descriptor that listing opened and checked, so a stat through it can
// only answer yes; the platforms where a handle could keep a name rather than a descriptor —
// GOOS=plan9 and GOOS=js, where os.Root does (`go doc os.Root`) — run no sweep at all, because
// this binary opens no directory of the volume outside Linux and the reading every interval
// starts from refuses by name there (volumedir_other.go, and readVolumeUsage through
// volumestat.Read, volumestat_other.go).
type sweepDir struct {
	root  ReclaimableRoot // the row of the table whose entries this directory holds
	shard string          // its name under that root, "" when the directory is the root itself
	// handle is open from the moment the listing opens this directory until the sweep ends,
	// whichever way it ends, a panic in the listing included (closeDirs, sweep), and every read
	// of an entry listed here goes through it, as does the deleter's own removal: the child is
	// handed this very descriptor.
	handle *dirHandle
}

// pathOf is the absolute path of one name in this directory, which the used-path guard and
// every line of the log read: /proc names what the pod's processes hold by absolute path, and
// an operator reading the pod's log needs the path too.
func (d *sweepDir) pathOf(name string) string {
	return filepath.Join(d.root.Path, d.shard, name)
}

// sweepEntry is one eligible entry as the listing read it.
type sweepEntry struct {
	path string
	// dir is the directory the listing read the entry from and name is the entry's own name in
	// it: together they are how every later read and the removal reach it (remove). The rule
	// the entry is judged by is the dir's root's own (sweepDir).
	//
	// After each refresh of the used-path set the delete loop asks whether the root above the
	// entry is held (usedPaths.Holds) as well as whether the entry is (usedPaths.Covers),
	// because Covers does not count a process holding an ancestor of the entry: a root nothing
	// held when the listing passed it may be locked by the time the loop reaches its entries.
	dir  *sweepDir
	name string
	// at is the newest modification time anywhere in the entry when it was listed, which is
	// both what the entries are ordered by and what the pre-delete re-read compares against.
	at    time.Time
	isDir bool
}

// sweep deletes the oldest entries of every root, oldest first across the roots, until a fresh
// reading is at or below the low mark or no entry is eligible, or until one of its other
// endings: three readings in a row that show no decrease (volumeSweepStallReadings), a /proc
// read the kernel refused (before anything is deleted, or at a refresh of the used-path set
// while it deletes), a deleter that will not start or that ends, a volume it cannot read, and
// the three that reach it from outside — the budget, the settings and the container's stop.
// Those three cut the listing where it stands, inside the walk of one entry included
// (newestModTime). Once deleting has begun they are taken at the next entry boundary or inside
// an entry's pre-delete re-read (newerThan), which comes before that entry's removal begins; a
// /proc read the kernel refused at a refresh of the used-path set is taken at the next entry
// boundary or after an entry's pre-delete re-read and before its removal begins (guard,
// remove); and every other ending at the next entry boundary but a deleter that ends, which the
// sweep meets only as the removal of one entry fails and can leave that entry half removed
// (remove), so no other ending of the sweep's own leaves an entry half removed. The container's
// kill at the end of its grace is none of them: it can land inside the removal of one large
// entry, which is then left part-removed on the volume and is an ordinary entry to the next
// sweep (remove).
//
// first is the interval's own reading, which is the sweep's first: it was taken moments ago, and
// the difference between it and the sweep's last reading is what the sweep freed. Progress is
// read from the volume and never counted from the bytes unlinked, because bun hardlinks out of
// its install cache into a workspace's node_modules, so unlinking the cached copy frees nothing
// at all while those links stand — a sweep counting bytes would empty the cache and report a
// volume it had not changed.
//
// Four kinds of line are written once the sweep has settled why it ends, with nothing after them
// but its last reading and the sums of what it freed: that its deleter could not be started, that
// its deleter ended, that the volume could not be read at its end, and, one line for each root
// whose entries passed over ran past the bound, how many more of them it passed over than it
// named, written as the sweep ends (passedOver.said, whose lines are contained together, so a
// panic of one root's line also skips the lines of the roots after it). Each is written inside
// contain, as deliverLast writes the line of a failed attempt, so a panic of one, which only the
// logger can raise, is dropped and counted in no row, and the sweep still returns its outcome:
// tick reads its last reading for a recovery, sets the pause after a sweep that freed under a
// point, and reads a reported sweep's start for a rise and makes its report, as it does for a
// sweep whose lines were written (tick). Every other line the sweep writes comes before it has
// settled why it ends, and a panic of one ends the sweep, which tick recovers and counts as a
// fault of the sweep row.
func (t *trimmer) sweep(ctx context.Context, set shimwire.LauncherVolumeTrim, first volumeUsage) sweepOutcome {
	start := t.now()
	deadline := start.Add(volumeSweepBudget)
	cutoff := start.Add(-time.Duration(set.MinAgeMinutes) * time.Minute)
	out := sweepOutcome{last: first}
	// The roots are all the used-path set is asked about (readUsedPaths).
	under := make([]string, len(t.roots))
	for at, root := range t.roots {
		under[at] = filepath.Clean(root.Path)
	}
	passed := &passedOver{logf: t.logf, roots: t.roots, count: map[string]int{}}
	// Written as the sweep ends, whichever way it ends, and once a sweep that returns has settled
	// its outcome, so its panic is dropped (above). Where the sweep itself panicked, the panic
	// that goes on to tick is the sweep's own, and not this line's.
	defer contain(passed.said)

	used := readUsedPaths(t.procRoot, under, t.self)
	usedAt := t.now()
	if len(used.Refused) > 0 {
		// Nothing is deleted. A refused read is a process whose open files are unknown, and on
		// the first read of a sweep that is every process of the pod as far as this sweep can
		// tell: deleting under it would be deleting a cache a role may be holding open. The
		// refusal goes to the trimmer now, for the next report (carryRefused), so no panic after
		// it, in the rest of the sweep, in its interval or in its report, can lose it.
		t.carryRefused(used.Refused)
		out.reason = fmt.Sprintf("a /proc read the kernel refused before anything was deleted, so nothing was (%s)", refusalsSaid(used.Refused))
		out.report = first.Percent() > float64(set.LowPercent)
		return out
	}

	// The handles the listing opens are closed when the sweep ends, whichever way it ends, a panic
	// in the listing itself included (tick and run recover one): dirs is the sweep's before the
	// listing begins, and the listing adds each handle to it the moment the handle is opened
	// (list, listShard), so no handle is ever open outside it. The closure reads dirs as it stands
	// when the sweep ends. The sweep runs in the launcher process, which is PID 1 of the roles
	// container for the life of the pod, and a descriptor leaked once an interval is a descriptor
	// leaked for ever.
	var dirs []*sweepDir
	defer func() { closeDirs(dirs) }()
	entries, cut, bySettings := t.list(ctx, used, cutoff, deadline, passed, &dirs)
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
	// The deleter every removal of this sweep goes through: one child, started once the listing
	// has opened every directory it will delete out of and ended with the sweep, whichever way
	// the sweep ends. It is started here and not before the listing because the descriptors it
	// is handed are the listing's own, and not after the first eligible entry because a child
	// per entry would be a fork and an exec per deletion (volumedelete.go).
	//
	// There is no fallback. A deleter that will not start leaves the sweep deleting nothing: it
	// says so in one line of the pod's log and carries the reason into its report, because the
	// deletions it would have made in the launcher's own view are the deletions this change
	// exists to stop. That costs the pod more than the one sweep: a sweep that deleted nothing
	// freed under a point, so the ten-interval pause follows it (volumeQuietIntervals) and the
	// next sweep is eleven intervals later — eleven minutes, at the default interval of sixty
	// seconds, of a volume that is already full.
	var remover entryRemover
	if len(entries) > 0 {
		started, err := t.removals(t.cover, dirs)
		if err != nil {
			out.reason = fmt.Sprintf("the sweep's deleter could not be started, so nothing was deleted: %v", err)
			out.report = first.Percent() > float64(set.LowPercent)
			contain(func() { t.logf(volumeTrimLog+"%s", out.reason) }) // its panic dropped (above)
			return out
		}
		remover = started
		defer remover.close()
	}

	previous := first
	stalled := 0
	sinceRead := 0
	readFailed := false
	halt := t.haltCheck(ctx, deadline)
	// heldSaid is the roots this sweep has already reported as taken open, so the line below is
	// one per root: there are four roots and tens of thousands of entries under them.
	var heldSaid []string
	// guard is the used-path set asked of one entry: whether the entry may go, and the ending
	// of the sweep where the set read again is one it knows to be incomplete. The set is read
	// again first whenever it is volumeUsedPathsMaxAge old. The sweep asks it at every entry
	// boundary, and remove asks it again after the entry's pre-delete re-read and immediately
	// before the removal: that re-read walks the entry's tree (newerThan), which takes as long as
	// the tree is large, so a set asked at the boundary alone could be older than
	// volumeUsedPathsMaxAge by the time the removal acts on its answer.
	guard := func(entry sweepEntry) (bool, error) {
		if t.now().Sub(usedAt) >= volumeUsedPathsMaxAge {
			// The deleter is left out with the trimmer: it holds every directory the listing
			// opened, and read as a use it was a hold on the root it deletes from (readUsedPaths).
			used = readUsedPaths(t.procRoot, under, t.self, remover.process())
			usedAt = t.now()
			if len(used.Refused) > 0 {
				// Later than the first read: what the sweep deleted stands, and it stops here
				// rather than deleting on a reading of the pod's processes it knows to be
				// incomplete. The refusal goes to the trimmer now, as the first read's does:
				// the sweep's last reading and the remover's close come after it.
				t.carryRefused(used.Refused)
				return false, &sweepHalt{reason: fmt.Sprintf("a /proc read the kernel refused while the sweep ran, so it kept what it had deleted and stopped (%s)", refusalsSaid(used.Refused))}
			}
		}
		if used.Holds(entry.dir.root.Path) {
			// The root was not held when the listing passed it — one that was is skipped whole
			// there — so a process has taken it since: the `bun install` taking its flock on
			// its cache root that the refresh above exists to catch (volumeUsedPathsMaxAge),
			// and the entries under the root are the ones that install is filling. Covers
			// cannot answer this, which is why the question is asked of the root: it ignores a
			// process holding an ancestor of the entry by design, and Holds counts only a
			// descriptor open on the root itself, since the sweep would otherwise stop at the
			// first role that stood in a cache root (Covers and Holds, volumeguard.go).
			//
			// Once per root and not once per entry: the roles container's log is the only
			// place this work is visible to a person, and a line per entry would bury it. It
			// is the reason a standing state of the trimmer's is said once (news). The line
			// speaks of the hold and not of the rest of the root, because this question is
			// asked again at every entry against the latest reading (volumeUsedPathsMaxAge): a
			// lock released mid-sweep lets the entries after it be deleted, and a second hold
			// of the same root writes no second line.
			if !slices.Contains(heldSaid, entry.dir.root.Path) {
				heldSaid = append(heldSaid, entry.dir.root.Path)
				t.logf(volumeTrimLog+"the reclaimable root %s was taken open by a process of the pod after the listing and its entries are skipped while it is held", entry.dir.root.Path)
			}
			return false, nil
		}
		return !used.Covers(entry.path), nil
	}
	for _, entry := range entries {
		if reason, settingsStop := t.halted(ctx, deadline); reason != "" {
			out.reason, out.settingsStop = reason, settingsStop
			break
		}
		clear, err := guard(entry)
		if err != nil {
			out.reason = err.Error()
			break
		}
		if !clear {
			continue
		}
		removed, ended := t.remove(remover, entry, passed, halt, guard)
		var stopped *sweepHalt
		if errors.As(ended, &stopped) {
			// An ending met inside the entry, before its removal began — one of the three in
			// its pre-delete re-read, or a refused /proc read where the set was read again
			// after it — so the entry stands whole and the sweep ends on it, as it would have
			// at the boundary after.
			out.reason, out.settingsStop = stopped.reason, stopped.bySettings
			break
		}
		if ended != nil {
			// The deleter, and not the removal: what the sweep deleted stands and it stops
			// here, as it does for a /proc read the kernel refused while it ran. Deleting the
			// rest in the launcher's own view is the one thing it must not do.
			out.reason = fmt.Sprintf("the sweep's deleter ended after %d deletions, so the sweep kept what it had deleted and stopped: %v", out.deletions, ended)
			contain(func() { t.logf(volumeTrimLog+"%s", out.reason) }) // its panic dropped (above)
			break
		}
		if !removed {
			continue
		}
		out.deletions++
		sinceRead++
		if sinceRead < volumeSweepReadEvery {
			continue
		}
		reading, err := t.readUsage(t.volumeRoot)
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
		if reading, err := t.readUsage(t.volumeRoot); err != nil {
			contain(func() { t.logf(volumeTrimLog+"the volume could not be read at the end of the sweep: %v", err) }) // its panic dropped (above)
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
// entry boundary, at every entry of the listing, and before every directory that the listing's
// walk of an entry and an entry's pre-delete re-read open and every batch of that directory's
// names after its first (haltCheck, walkEntries, newestModTime, newerThan), and never once an
// entry's removal has begun: so none of the three endings leaves an entry half removed, and the
// listing is cut at the entry where it stands. A root's or a Go cache shard's own listing is read
// whole before any of these is asked (list, listShard). Nothing asks the container's kill at the end of its grace,
// which can land inside a removal (sweep).
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

// sweepHalt is an ending the sweep meets inside the handling of one entry, as the error that
// ends it: halted's answer to a walk of the entry, the listing's (newestModTime) or the
// pre-delete re-read's (newerThan), and the refused /proc read of the used-path set read again
// after that re-read (sweep's guard, remove). The sweep ends with the reason it carries.
type sweepHalt struct {
	reason     string
	bySettings bool
}

func (h *sweepHalt) Error() string { return h.reason }

// haltCheck is halted asked as a walk asks it: nil while the sweep may go on, and the ending as
// a sweepHalt once it may not.
func (t *trimmer) haltCheck(ctx context.Context, deadline time.Time) func() error {
	return func() error {
		if reason, bySettings := t.halted(ctx, deadline); reason != "" {
			return &sweepHalt{reason: reason, bySettings: bySettings}
		}
		return nil
	}
}

// refusalsSaid is the refused /proc reads a sweep's reason names: the first
// shimwire.VolumeReportRefusedReads of them, the most a report names, and how many more there
// were. The reason is a line of the pod's log, and a role that starts processes the launcher
// cannot read chooses how many there are (carryRefused bounds the report's list the same way).
func refusalsSaid(refused []string) string {
	if len(refused) <= shimwire.VolumeReportRefusedReads {
		return strings.Join(refused, "; ")
	}
	return fmt.Sprintf("%s; and %d more", strings.Join(refused[:shimwire.VolumeReportRefusedReads], "; "), len(refused)-shimwire.VolumeReportRefusedReads)
}

// passedOver is what one sweep passes over, counted root by root: the entries its listing or
// its delete loop would not judge or would not delete, each for a reason of its own (list,
// remove). The first volumePassedOverLines of each root are named in the pod's log, one line
// each, and the rest are counted and said in one line per root when the sweep ends (said): a
// root a role fills with links or unreadable names would otherwise put one line per name into
// the log on every sweep. The count is a map keyed by the root's path, so it holds no more keys
// than the sweep has roots.
type passedOver struct {
	logf  func(format string, args ...any)
	roots []ReclaimableRoot
	count map[string]int
}

// say counts one entry of root passed over and writes its line while the root is under the
// bound.
func (p *passedOver) say(root ReclaimableRoot, format string, args ...any) {
	p.count[root.Path]++
	if p.count[root.Path] <= volumePassedOverLines {
		p.logf(format, args...)
	}
}

// said writes the one line for each root whose entries passed over ran past the bound.
func (p *passedOver) said() {
	for _, root := range p.roots {
		if more := p.count[root.Path] - volumePassedOverLines; more > 0 {
			p.logf(volumeTrimLog+"%d more entries of the reclaimable root %s were passed over in this sweep, past the first %d named above", more, root.Path, volumePassedOverLines)
		}
	}
}

// list is every eligible entry of every root, in root order, with the newest modification time
// anywhere in it. Every directory it opens to read them (sweepDir) is added to dirs the moment it
// is opened, and the sweep closes them when it ends, however the listing ends, a panic in it
// included (sweep). cut is the reason the listing was ended early, and bySettings that it was the
// settings that ended it; a cut listing ends the sweep, since the three things that cut it —
// the budget, a stop and the container's stop — are each a reason to stop deleting as well.
// They are asked before each entry, before each directory of an entry's walk and before each
// batch of that directory's names after its first (haltCheck, walkEntries), and an entry whose
// walk they cut is not eligible: its newest time was never read.
//
// A root that is absent, that cannot be opened, that a process holds open or that cannot be
// listed is skipped and reported rather than being an error: a pod whose roles have not started
// yet has no bun cache, and nothing about that should keep the sweep away from the three roots
// that are there. A root a process holds open is skipped whole (usedPaths.Holds), and every
// entry listed carries the directory it came from, which the delete loop asks the held question
// of again on each refreshed reading. An entry passed over is said through passed, which names
// the first of each root's and counts the rest (passedOver).
func (t *trimmer) list(ctx context.Context, used usedPaths, cutoff, deadline time.Time, passed *passedOver, dirs *[]*sweepDir) (entries []sweepEntry, cut string, bySettings bool) {
	halt := t.haltCheck(ctx, deadline)
	add := func(from *sweepDir, name string, isDir bool) bool {
		if reason, settingsStop := t.halted(ctx, deadline); reason != "" {
			cut, bySettings = reason, settingsStop
			return false
		}
		path := from.pathOf(name)
		switch ok, why, err := entryKindOK(from.handle, name, from.root.Entry); {
		case errors.Is(err, fs.ErrNotExist):
			return true
		case err != nil:
			passed.say(from.root, volumeTrimLog+"%q could not be read and is passed over: %v", path, err)
			return true
		case !ok:
			passed.say(from.root, volumeTrimLog+"%q is passed over: %s", path, why)
			return true
		}
		at, err := newestModTime(from.handle, name, halt)
		var stopped *sweepHalt
		switch {
		case errors.As(err, &stopped):
			cut, bySettings = stopped.reason, stopped.bySettings
			return false
		case errors.Is(err, fs.ErrNotExist):
			return true
		case err != nil:
			passed.say(from.root, volumeTrimLog+"the age of %q could not be read and it is passed over: %v", path, err)
			return true
		}
		// The Go build cache's entries are judged an hour further back than the rest, because
		// Go moves their mtimes on use at most once an hour (goCacheMtimeSlack); every other
		// rule is judged by the minimum age exactly, since nothing else on the volume writes
		// its times that way. The allowance is asked of the rule and not carried as a field of
		// ReclaimableRoot, because it is a fact about the program that writes those times and
		// not a choice about one path: two rows of the table naming GoCacheEntries could
		// otherwise disagree about Go's own interval, and a row could be given an hour for a
		// cache whose tool never marks an entry used.
		eligible := cutoff
		if from.root.Entry == GoCacheEntries {
			eligible = cutoff.Add(-goCacheMtimeSlack)
		}
		if at.After(eligible) {
			return true // inside the minimum age, or for the Go cache that and Go's hour: too recent
		}
		entries = append(entries, sweepEntry{path: path, dir: from, name: name, at: at, isDir: isDir})
		return true
	}

	for _, reclaimable := range t.roots {
		// The handle is opened before the checks that decide whether to list the root, and
		// every one of them reads through it, so the inode that is checked, the inode that is
		// listed and the inode that is deleted out of are one and not one path string resolved
		// three times (sweepDir).
		//
		// It is opened one element at a time, each refusing a link and none waiting on a FIFO
		// (openVolumeDir): the elements above a reclaimable root are the roles' own, and a
		// path resolved in one call follows a link a role left at any of them. That is why the
		// lstat of the same path this check used to carry is gone rather than kept — it
		// followed those very elements too, so for a `scratch` renamed away and replaced by a
		// link the two agreed and the sweep listed and deleted in the link's target, with no
		// line in the log at all. Measured against this very code on 2026-11-12, kernel
		// 4.19.0-gvisor, under go1.27.3
		// (TestAReclaimableRootUnderALinkedElementIsSkippedAndTheRestAreSwept; INFRA-2009,
		// review 7). A link anywhere in the path now refuses the open instead, and the root is
		// skipped with the line below naming it.
		handle, err := openVolumeDir(reclaimable.Path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			t.logf(volumeTrimLog+"the reclaimable root %s is not there and is skipped", reclaimable.Path)
			continue
		case err != nil:
			// A root that cannot be opened is skipped and reported, as an absent one is: the
			// regular file an interrupted `flock` leaves where a cache directory belongs reads
			// as ENOTDIR here, which is the root that is not a directory, and so does the FIFO
			// a role may leave in its place — the open answers at once rather than waiting on
			// a writer that never comes (volumeOpenFlags, volumedir_linux.go).
			t.logf(volumeTrimLog+"the reclaimable root %s could not be opened and is skipped: %v", reclaimable.Path, err)
			continue
		}
		root := &sweepDir{root: reclaimable, handle: handle}
		*dirs = append(*dirs, root) // closed when the sweep ends, whichever way it ends
		if used.Holds(reclaimable.Path) {
			// A process holds a descriptor open on the root itself, which is what a `flock` on
			// it holds and what the rendered bun install takes on its cache root for the length
			// of the install: the entries under it are what that install is filling, and the
			// root is left alone until it lets go (usedPaths.Holds).
			t.logf(volumeTrimLog+"the reclaimable root %s is held open by a process of the pod and is skipped whole", reclaimable.Path)
			continue
		}
		children, err := handle.ReadDir()
		if err != nil {
			t.logf(volumeTrimLog+"the reclaimable root %s could not be listed and is skipped: %v", reclaimable.Path, err)
			continue
		}
		if reclaimable.Entry == GoCacheEntries {
			if others := goCacheNotShards(children); others > 0 {
				t.logf(volumeTrimLog+"the Go build cache root %s holds %d directories not named as a shard is, two lowercase hex digits, and they are passed over", reclaimable.Path, others)
			}
		}
		for _, child := range children {
			switch reclaimable.Entry {
			case GoBuildTrees:
				// Go names these and owns them, and makes one for every `go build` and every
				// `go test` because the pod sets no GOTMPDIR. Everything else in $TMPDIR is a
				// role's own work under a name the role chose, a `go-build-repro` of its own
				// among them (isGoBuildTree).
				if !isGoBuildTree(child.Name()) {
					continue
				}
			case GoCacheEntries:
				// The entries are the names ending `-a` or `-d` inside a shard directory. The
				// cache's own `trim.txt` and `README` sit in the cache root rather than in a
				// shard, so a rule that reaches only into shard directories never reaches them,
				// and a shard is a directory named as Go names its 256: two lowercase hex
				// digits, `fmt.Sprintf("%02x", id[0])` (cmd/go/internal/cache/cache.go:118, and
				// :103-108 and :430-433, where Go's own Open makes those 256 and its own trim
				// visits those 256 and no other). Every shard is held open until the sweep ends,
				// so a directory of any other name a role makes in the root is passed over
				// (goCacheNotShards) rather than costing PID 1 a handle for the sweep.
				if !child.IsDir() || !isGoCacheShard(child.Name()) {
					continue
				}
				listed, shard := t.listShard(root, child.Name(), dirs)
				if shard == nil {
					continue
				}
				for _, name := range listed {
					if !strings.HasSuffix(name.Name(), "-a") && !strings.HasSuffix(name.Name(), "-d") {
						continue
					}
					if !add(shard, name.Name(), name.IsDir()) {
						return entries, cut, bySettings
					}
				}
				continue
			case ImmediateChildren:
				if slices.Contains(reclaimable.Except, child.Name()) {
					continue
				}
			}
			if !add(root, child.Name(), child.IsDir()) {
				return entries, cut, bySettings
			}
		}
	}
	return entries, cut, bySettings
}

// isGoCacheShard is whether name is a shard directory's name as Go names its 256: two lowercase
// hex digits (list).
func isGoCacheShard(name string) bool {
	const hex = "0123456789abcdef"
	return len(name) == 2 && strings.IndexByte(hex, name[0]) >= 0 && strings.IndexByte(hex, name[1]) >= 0
}

// isGoBuildTree is whether name is one Go gives a build's working directory: `go-build` and one
// or more decimal digits, which is what `os.MkdirTemp(cfg.Getenv("GOTMPDIR"), "go-build")` makes
// (cmd/go/internal/work/action.go:298, with os.MkdirTemp appending nextRandom's
// strconv.FormatUint(uint64(uint32(runtime_rand())), 10), os/tempfile.go:22-23, read in this
// pod's own go1.27.3). A name that only begins `go-build` — a role's `go-build-repro`,
// `go-builder` or `go-build.log` — is a role's own and never an entry (list).
func isGoBuildTree(name string) bool {
	digits, found := strings.CutPrefix(name, "go-build")
	if !found || digits == "" {
		return false
	}
	for _, digit := range []byte(digits) {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

// goCacheNotShards is how many of a Go build cache root's directories are not named as a shard
// is, which the listing passes over and says in one line per sweep: a role may make any number
// of them, and a line each would be as many lines.
func goCacheNotShards(children []fs.DirEntry) int {
	others := 0
	for _, child := range children {
		if child.IsDir() && !isGoCacheShard(child.Name()) {
			others++
		}
	}
	return others
}

// listShard opens one shard directory of a Go build cache root and reads its names, through the
// root's own handle: the shard is a directory of the root, so the sweep reaches it as it reaches
// an entry, by its name in a directory it holds open. The handle it returns is the one every
// read and the removal of the entries below will go through. It is added to dirs the moment it is
// opened, before its names are read, so the sweep closes it with the rest however the listing
// ends, a panic in this read included (sweep); a shard that cannot be opened or read is skipped
// and reported, and no name comes back.
//
// A shard the listing meets is a directory as the root's own listing read it (child.IsDir, which
// is the directory's own record of the kind and no symbolic link). One a role swapped for a link
// between that reading and this open is refused: the open carries O_NOFOLLOW and no O_DIRECTORY,
// which on this kernel is what refuses a link rather than following it, and what the open holds
// is a directory because its own fstat says so (dirHandle.open, volumedir.go). Root.OpenRoot,
// which this used, followed a relative link that stayed lexically inside the root and then
// resolved that link's own elements with the open this kernel does follow a link under, so
// `0f -> zz/sh` beside `zz -> /bait` left the sweep listing and deleting in /bait/sh — measured
// against this very code on 2026-11-12, kernel 4.19.0-gvisor, under go1.27.3
// (TestAShardSwappedForARelativeLinkCrossingAnAbsoluteOneIsSkipped; INFRA-2009, review 7).
func (t *trimmer) listShard(root *sweepDir, name string, dirs *[]*sweepDir) ([]fs.DirEntry, *sweepDir) {
	path := root.pathOf(name)
	handle, err := root.handle.open(name)
	if err != nil {
		t.logf(volumeTrimLog+"the shard %q could not be opened and is skipped: %v", path, err)
		return nil, nil
	}
	shard := &sweepDir{root: root.root, shard: name, handle: handle}
	*dirs = append(*dirs, shard) // closed when the sweep ends, as the roots are
	children, err := handle.ReadDir()
	if err != nil {
		t.logf(volumeTrimLog+"the shard %q could not be listed and is skipped: %v", path, err)
		return nil, shard
	}
	return children, shard
}

// closeDirs closes every handle the listing opened (sweep).
func closeDirs(dirs []*sweepDir) {
	for _, dir := range dirs {
		_ = dir.handle.Close()
	}
}

// remove is the two reads the sweep makes of one entry immediately before it deletes it, the
// used-path set asked of it again, and the removal itself. removed is whether the entry went;
// ended is what ends the sweep where it stands (sweep) and which no later entry can be asked
// of: the deleter rather than the removal, or an ending met before the removal began
// (sweepHalt) — one of the sweep's three endings as halt answered it inside the pre-delete
// re-read (moved), or a refused /proc read of guard. Each read goes through the handle on the
// directory the listing read the entry from, under the entry's own name, and the removal is
// asked of that same descriptor — the deleter was handed it with the rest of the listing's and
// removes the entry from it by that name alone (removeOneRequest, volumedelete.go) — so the two
// reads and the removal reach the inode the listing judged (sweepDir). Every reason not to
// delete is the sweep's to pass over rather than to stop for, as it was when each of these
// resolved a path of its own, and each is said through passed (passedOver).
//
// guard is the used-path set the sweep asked of the entry at its boundary, asked again after
// the re-read and immediately before the removal, and read again first where the re-read took
// it past volumeUsedPathsMaxAge (sweep): the removal acts on a reading of the pod's processes
// no older than that, however long the entry's tree took to read.
func (t *trimmer) remove(remover entryRemover, entry sweepEntry, passed *passedOver, halt func() error, guard func(sweepEntry) (bool, error)) (removed bool, ended error) {
	switch ok, why, err := entryKindOK(entry.dir.handle, entry.name, entry.dir.root.Entry); {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil // a role's own tool deleted it between the listing and now
	case err != nil:
		passed.say(entry.dir.root, volumeTrimLog+"%q could not be read and is passed over: %v", entry.path, err)
		return false, nil
	case !ok:
		passed.say(entry.dir.root, volumeTrimLog+"%q is passed over: %s", entry.path, why)
		return false, nil
	}
	var stopped *sweepHalt
	switch moved, err := t.moved(entry, halt); {
	case errors.As(err, &stopped):
		return false, err
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		passed.say(entry.dir.root, volumeTrimLog+"%q could not be read again before the delete and is passed over: %v", entry.path, err)
		return false, nil
	case moved:
		// Something wrote in it between the listing and this moment: a build that started
		// while the sweep listed, which the age guard would have kept out had it run now.
		return false, nil
	}
	if clear, err := guard(entry); err != nil || !clear {
		return false, err
	}
	if err := remover.removeAll(entry.dir, entry.name); err != nil {
		if errors.Is(err, errRemoverGone) {
			return false, err
		}
		// A removal that failed part way leaves what it could not remove. It is reported
		// and passed over, and the part-removed entry a later sweep meets is an ordinary
		// entry of that root, judged by its age as any other is: each unlink the removal made
		// inside it moved its newest modification time, and a removal that unlinked nothing
		// moved none.
		passed.say(entry.dir.root, volumeTrimLog+"removing %q failed part way and the sweep passed it over: %v", entry.path, err)
		return false, nil
	}
	return true, nil
}

// moved is whether anything in the entry was written since the listing read it, asked again
// immediately before the delete: a build that started while the sweep listed must keep its
// cache. A directory is asked with newerThan, which stops at the first file newer than the
// recorded time instead of walking the tree again and asks halt before each directory it opens
// and before each batch of its names after the first, and a single file with the lstat it needs
// anyway.
//
// It compares against entry.at, the time the listing read, and not against any cutoff, which is
// why Go's hour belongs in the listing and not here: a modification time only moves forward, so
// any movement at all since the listing already makes this skip, and subtracting the hour from
// the same reference a second time would call every entry moved and delete nothing of the Go
// cache ever. The listing's own allowance is what makes this comparison sound for that cache:
// with the shifted cutoff (goCacheMtimeSlack) every Go cache entry listed already reads more
// than an hour old, so Go's markUsed bumps its mtime on the next use rather than leaving it
// where it was, and a use between the listing and this moment is therefore visible here.
func (t *trimmer) moved(entry sweepEntry, halt func() error) (bool, error) {
	if entry.isDir {
		return newerThan(entry.dir.handle, entry.name, entry.at, halt)
	}
	info, err := entry.dir.handle.Lstat(entry.name)
	if err != nil {
		return false, err
	}
	return info.ModTime().After(entry.at), nil
}
