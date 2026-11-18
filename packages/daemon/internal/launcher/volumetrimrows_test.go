//go:build linux

package launcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The cases of what a panic in the trimmer's own work costs it: the two rows its faults are
// counted in (faultRow), the refused /proc reads a sweep met before it panicked, the rise a
// reported sweep's start raises though the sweep then panicked, the clock a sweep that panicked
// still has made again, the pause an interval spends whatever its reading, its resend of a held
// report or its line of new settings did, a held report whose resend panics in the wait for
// settings, tried once over one refusal, and what a line that panics leaves standing: the pause
// after its sweep and its countdown, the pause or the recovery after a sweep whose line written
// once its outcome was settled panicked, a refusal and the held report it tries, the line of a
// volume that cannot be read, what a tick decided before its line, and the switched-off trimmer's
// last report, whether the line that says it is off panics or one written once that report is
// held.
// The file builds only under Linux because its cases drive volumetrim_test.go's fixture, which
// does.

// R1 has the trimmer switch itself off "after three in a row", and a reading run to its end ends
// the reading row (faultRow, tick), so a pod whose reading panics now and then, with readings run
// to their end between, stays guarded however many such panics come. The architect's tester drove
// the rule this replaces as PID 1: a volume at 50% whose reading panicked once every 200 intervals
// was switched off at its third panic (INFRA-2009, comment ccc812fc). Here the reading of every
// third interval panics, four times, with two readings run to their end before each, and the volume
// stays below the high mark, so no sweep ever runs. At b1b8e361 the second of them was counted as
// the second in a row and the third switched the trimmer off.
func TestAReadingThatPanicsNowAndThenWithReadingsBetweenLeavesTheTrimmerOn(t *testing.T) {
	const every, panics = 3, 4
	f := newTrimFixture(t, 0)
	f.setOnRead(func(call int) {
		if call%every == 0 {
			panic(fmt.Sprintf("the volume reader fell over on call %d", call))
		}
	})
	f.useVolume(steady(50)) // below the high mark: every interval only reads the volume
	f.setSettings(trimSettings())
	_, done := f.start(f.trimmer())

	for fault := 1; fault <= panics; fault++ {
		for range every - 1 {
			f.tick("nothing to sweep")
		}
		f.tick("recovered a panic")
		if first := f.logged("(1 in a row"); first != fault {
			t.Fatalf("after reading panic %d the trimmer wrote %q, want each counted as the first in a row: %d readings ran to their end before it",
				fault, f.linesWritten(), every-1)
		}
		if f.logged("off for the life of this launcher process") != 0 {
			t.Fatalf("the trimmer switched itself off at reading panic %d, want it on: no two of them were in a row", fault)
		}
	}
	if row := f.logged("(1 in a row of readings)"); row != panics {
		t.Errorf("the trimmer wrote %q, want each of the %d panics counted as the first of the reading row", f.linesWritten(), panics)
	}
	f.tick("nothing to sweep")
	select {
	case <-done:
		t.Fatal("the trimmer's loop returned, want it still reading the volume")
	default:
	}
	if sent := len(f.reports()); sent != 0 {
		t.Errorf("the trimmer sent %d reports, want none: it never swept and never switched itself off", sent)
	}
}

// Three readings in a row that panic switch the trimmer off, and a panic of the sweep row counts
// toward none of them: R1's "after three in a row" is three panics of one kind of work (faultRow).
// The case's first two sweeps free nothing and pause, and the send of each one's report panics,
// which puts the sweep row at two; every reading after the second pause then panics. The reading
// row reaches three at the third of those, and only then is the trimmer off, with the newest three
// faults, the readings', in its last report and the two of the sweeps counted as left out. At
// b1b8e361 the one row held the two report panics across the pauses, and the first reading panic
// was the third in a row and switched the trimmer off.
func TestThreeReadingPanicsInARowSwitchTheTrimmerOffApartFromTheSweepRow(t *testing.T) {
	const fell = "the connection fell over sending the report"
	f := newTrimFixture(t, 0) // no entry: every sweep frees nothing, reports and pauses
	f.useVolume(steady(90))
	f.setSettings(trimSettings())
	// Two sweeps and their pauses are this many readings, and every reading after them panics.
	readings := 2 * (1 + volumeQuietIntervals)
	f.setOnRead(func(call int) {
		if call > readings {
			panic(fmt.Sprintf("the volume reader fell over on call %d", call))
		}
	})
	tr := f.trimmer()
	tr.send = func(report shimwire.LauncherVolumeReport) error {
		if !report.SwitchedOff {
			panic(fell)
		}
		return f.send(report)
	}
	_, done := f.start(tr)

	sweepsAndPauses(f, 2)
	if f.logged("(2 in a row") != 1 {
		t.Fatalf("the trimmer wrote %q, want the second report's panic counted as the second in a row: the case would stand no row of the sweeps", f.linesWritten())
	}
	// Written out rather than read off volumeFaultLimit, as the case of three faults with nothing
	// between them counts them (TestAPanicIsRecoveredAndThreeInARowSwitchTheTrimmerOff).
	const faultLimit = 3
	for fault := 1; fault < faultLimit; fault++ {
		f.tick("recovered a panic")
		if row := fmt.Sprintf("(%d in a row of readings)", fault); f.logged(row) != 1 {
			t.Fatalf("after reading panic %d the trimmer wrote %q, want it counted %s: the two panics of the sweep row before it are not of its row",
				fault, f.linesWritten(), row)
		}
		if f.logged("off for the life of this launcher process") != 0 {
			t.Fatalf("the trimmer switched itself off at reading panic %d, want it on until the %dth", fault, faultLimit)
		}
	}
	f.tick("recovered a panic")
	awaitClosed(t, done, "the trimmer's loop to return once the third reading panic switched it off and its last report was sent")

	reports := f.reports()
	if len(reports) != 1 {
		t.Fatalf("the trimmer sent %d reports, want the one last report of a trimmer that switched itself off", len(reports))
	}
	report := reports[0]
	switch {
	case !report.SwitchedOff:
		t.Error("the last report does not say the trimmer switched itself off")
	case !strings.Contains(report.StopReason, fmt.Sprintf("%d panics in a row of readings", faultLimit)):
		t.Errorf("the last report's stop reason is %q, want the %d panics of the reading row that stopped it", report.StopReason, faultLimit)
	case len(report.Faults) != faultLimit || report.FaultsOmitted != 2:
		t.Errorf("the last report carries the faults %q and counts %d more, want the %d reading panics and the 2 of the reports counted", report.Faults, report.FaultsOmitted, faultLimit)
	}
	for at, fault := range report.Faults {
		if !strings.HasPrefix(fault, "the volume reader fell over") {
			t.Errorf("fault %d of the last report is %q, want a panic of the reading", at+1, fault)
		}
	}
	if err := report.Validate(); err != nil {
		t.Errorf("the daemon would refuse the last report: %v", err)
	}
}

// A panic of a sweep's work leaves the readings' work to follow it (fault, doing), so a reading
// that panics after a report whose send panicked adds to the reading row and not to the sweep
// row: R1 as the architect's comment ccc812fc on INFRA-2009 words it, "A panic in a reading adds
// to the reading row". tick sets nothing before its reading (flush, intervalReading), so that
// reading's panic is counted in the row the fault before it left in force. The case's one sweep
// frees nothing and pauses, the send of its report panics, and the first two readings of its
// pause panic too: the trimmer counts the first panic in the sweep row and the other two in the
// reading row, and stays on. The negative control is a copy of fault that leaves doing as the
// panic found it, under which the two reading panics are the second and the third of the sweep
// row and switch the trimmer off.
func TestTwoReadingPanicsAfterAReportsPanicAreCountedInTheReadingRow(t *testing.T) {
	const fell = "the connection fell over sending the report"
	f := newTrimFixture(t, 0) // no entry: the sweep frees nothing, reports and pauses
	f.useVolume(steady(90))
	f.setSettings(trimSettings())
	// The sweep's reading is the first, and the first two of its pause, the second and the third, panic.
	f.setOnRead(func(call int) {
		if call == 2 || call == 3 {
			panic(fmt.Sprintf("the volume reader fell over on call %d", call))
		}
	})
	tr := f.trimmer()
	tr.send = func(report shimwire.LauncherVolumeReport) error {
		if !report.SwitchedOff {
			panic(fell)
		}
		return f.send(report)
	}
	_, done := f.start(tr)

	for at, row := range []string{"(1 in a row of sweeps and reports)", "(1 in a row of readings)", "(2 in a row of readings)"} {
		f.tick("recovered a panic")
		if f.logged(row) != 1 {
			t.Fatalf("after panic %d the trimmer wrote %q, want it counted %s: a reading's panic after the report's adds to the reading row",
				at+1, f.linesWritten(), row)
		}
		if f.logged("off for the life of this launcher process") != 0 {
			t.Fatalf("the trimmer switched itself off at panic %d, want it on: neither row holds three", at+1)
		}
	}
	f.tick("no sweep for another") // the third reading of the pause, which runs to its end
	select {
	case <-done:
		t.Fatal("the trimmer's loop returned, want it still reading the volume")
	default:
	}
	if sent := len(f.reports()); sent != 0 {
		t.Errorf("the trimmer sent %d reports, want none: its one report panicked and it never switched itself off", sent)
	}
}

// The same holds where every fault's line panics, which only the logger can raise. fault counts
// the panic before it writes its line, so it never reaches the end where it puts the readings'
// work back (fault), and run puts it back once that line's panic reaches it, before it starts the
// loop again from the wait for settings (run). The case's one sweep frees nothing and pauses, the
// send of its report panics, and the logger writes each fault line and then panics: tick's
// handler counts the report's panic as the first of the sweep row, run counts its line's as the
// second, drops the panic of its own line and starts the loop again. The first reading of the
// pause then panics, and it and its line's panic are the first and the second of the reading row;
// the loop starts again and reads the volume at its next tick, in the pause. The negative control
// is a copy of run that does not put the readings' work back, under which the reading's panic and
// its line's are the third and the fourth of the sweep row and switch the trimmer off.
func TestAReadingPanicAfterAReportsPanicCountsInTheReadingRowThoughEveryFaultLinePanics(t *testing.T) {
	const fell = "the connection fell over sending the report"
	f := newTrimFixture(t, 0) // no entry: the sweep frees nothing, reports and pauses
	f.useVolume(steady(90))
	f.setSettings(trimSettings())
	// The sweep's reading is the first, and the first of its pause, the second, panics.
	f.setOnRead(func(call int) {
		if call == 2 {
			panic(fmt.Sprintf("the volume reader fell over on call %d", call))
		}
	})
	tr := f.trimmer()
	tr.send = func(report shimwire.LauncherVolumeReport) error {
		if !report.SwitchedOff {
			panic(fell)
		}
		return f.send(report)
	}
	// The logger writes each fault line before it panics, so the case reads the row each panic
	// was counted in.
	tr.logf = func(format string, args ...any) {
		f.log(format, args...)
		if strings.Contains(format, "recovered a panic") {
			panic("the log fell over writing the line of a fault")
		}
	}
	_, done := f.start(tr)
	testwait.Eventually(t, "the interval loop to start", func() bool { return f.logged("started:") == 1 })

	f.tick("started:") // the report's panic and its line's, after which the loop starts again
	for _, row := range []string{"(1 in a row of sweeps and reports)", "(2 in a row of sweeps and reports)"} {
		if f.logged(row) != 1 {
			t.Fatalf("after the report's panic the trimmer wrote %q, want it or its line's panic counted %s", f.linesWritten(), row)
		}
	}

	f.tick("recovered a panic") // the reading's panic, then its line's
	testwait.Eventually(t, "the loop to start again or the trimmer to switch itself off", func() bool {
		return f.logged("started:") == 3 || f.logged("off for the life of this launcher process") != 0
	})
	for _, row := range []string{"(1 in a row of readings)", "(2 in a row of readings)"} {
		if f.logged(row) != 1 {
			t.Fatalf("after the reading's panic the trimmer wrote %q, want it or its line's panic counted %s: run puts the readings' work back after a fault whose line panicked",
				f.linesWritten(), row)
		}
	}
	if f.logged("off for the life of this launcher process") != 0 {
		t.Fatalf("the trimmer switched itself off, want its loop started again from the wait for settings: neither row holds three")
	}
	f.tick("no sweep for another") // the next reading, the pause's, which runs to its end
	select {
	case <-done:
		t.Fatal("the trimmer's loop returned, want it still reading the volume")
	default:
	}
	if sent := len(f.reports()); sent != 0 {
		t.Errorf("the trimmer sent %d reports, want none: its one report panicked and it never switched itself off", sent)
	}
}

// A sweep the settings stopped ends the sweep row as a sweep run to its end with its report does
// (tick): it ran without a fault, and it makes no report. The case's first two sweeps free
// nothing and pause, and the send of each one's report panics, which puts the sweep row at two;
// an answer with no settings then stops the third sweep at its first entry boundary, the settings
// come back, and the send of the next sweep's report panics too. That panic is the first of a new
// row, and the trimmer stays on. The negative control is a copy whose settings' branch of tick
// ends no row, under which that panic is the third in a row and switches the trimmer off. At
// b1b8e361 the one row of faults ended there too, and its line named no row.
func TestASweepTheSettingsStoppedEndsTheSweepRow(t *testing.T) {
	const fell = "the connection fell over sending the report"
	f := newTrimFixture(t, 0) // no entry it may delete: every sweep frees nothing and takes no reading of its own
	for _, root := range f.roots {
		if root.Entry == ImmediateChildren {
			// One entry too young to delete: the listing asks the settings before it reads an
			// entry's age (list), so each sweep has an entry boundary the answer stops it at, and
			// the sweeps that run delete nothing.
			mkdirFixture(t, filepath.Join(root.Path, "tool-fresh"))
		}
	}
	f.useVolume(steady(90))
	f.setSettings(trimSettings())
	var stopping atomic.Bool
	f.clock.setHook(func(*fakeClock) {
		// The clock's first reading of an interval is the sweep's own start (sweep): the answer
		// with no settings arrives there, after the interval's own read of the settings.
		if stopping.CompareAndSwap(true, false) {
			f.setSettings(nil)
		}
	})
	tr := f.trimmer()
	tr.send = func(report shimwire.LauncherVolumeReport) error {
		if !report.SwitchedOff {
			panic(fell)
		}
		return f.send(report)
	}
	f.start(tr)
	testwait.Eventually(t, "the interval loop to start", func() bool { return f.logged("started:") == 1 })

	sweepsAndPauses(f, 2)
	if f.logged("(2 in a row") != 1 {
		t.Fatalf("the trimmer wrote %q, want the second report's panic counted as the second in a row: the case would stand no row of the sweeps", f.linesWritten())
	}

	waited, started := f.logged("no settings are in force, so the trimmer stays off"), f.logged("started:")
	stopping.Store(true)
	f.tick("the settings stopped the trimmer")
	testwait.Eventually(t, "the trimmer to wait for settings it can run", func() bool {
		return f.logged("no settings are in force, so the trimmer stays off") == waited+1
	})
	f.setSettings(trimSettings())
	offerSettingsTicksUntil(t, f, "the interval loop to start again", func() bool { return f.logged("started:") == started+1 })

	f.tick("the sweep ended")
	testwait.Eventually(t, "the panic of the report after the stopped sweep to be recovered", func() bool {
		return f.logged("recovered a panic") == 3
	})
	if first := f.logged("(1 in a row of sweeps and reports)"); first != 2 {
		t.Fatalf("the trimmer wrote %q, want the panic of the report after the stopped sweep counted as the first of a new sweep row, as the first report's was", f.linesWritten())
	}
	f.tick("no sweep for another") // the first interval of its pause: the trimmer is still on
	if f.logged("off for the life of this launcher process") != 0 {
		t.Errorf("the trimmer switched itself off, want it on: the sweep the settings stopped ended the row of the two report panics before it")
	}
}

// closeFallsOver is a remover whose close panics while falling holds, which is how a case makes
// the end of a sweep panic after the sweep has met everything it meets.
type closeFallsOver struct {
	inProcessRemover
	falling *atomic.Bool
}

func (r closeFallsOver) close() {
	if r.falling.CompareAndSwap(true, false) {
		panic("the sweep's deleter fell over as it was closed")
	}
}

// R3 has a refused /proc read name "the process it could not read" and be "carried into the next
// report", so a refusal a sweep met is the trimmer's from that moment (carryRefused, sweep), and a
// panic after it — in the sweep's last reading, in the close of the sweep's deleter, or in the
// first line the interval writes after the sweep — loses none of it. Each subtest stages a process
// whose fd entry is a file, which the reader refuses whoever it runs as, with the volume at 90%,
// makes one panic after the sweep met the refusal, and reads the report of the next sweep, which
// reads a /proc it is refused nothing in: that report must name the refusal. At b1b8e361 the
// refusals lived in the sweep's outcome until the report or the interval took them, and the panic
// took the outcome with it, so the next report named none.
func TestARefusedProcReadReachesTheNextReportThoughItsSweepPanicsAfterIt(t *testing.T) {
	// whileSweeping has the process refused from the sweep's first refresh of the used-path set,
	// after its first deletion, and answers the pid it placed once the case has run the sweep.
	whileSweeping := func(t *testing.T, f *trimFixture) func() string {
		t.Helper()
		first := f.oldestFirst()[0]
		var placed string
		f.clock.setHook(func(clock *fakeClock) {
			if placed != "" || f.exists(first.path) {
				return
			}
			placed = strconv.Itoa(placeRefusedProcess(t, f.proc, 5404, filepath.Join(f.root, "scratch")))
			clock.advance(volumeUsedPathsMaxAge + time.Millisecond)
		})
		return func() string {
			t.Helper()
			if placed == "" {
				t.Fatal("the case never reached the moment it stages")
			}
			return placed
		}
	}
	// nextReportNames runs the interval after the one that panicked, on a /proc the trimmer is
	// refused nothing in, and holds its report to naming the process refused.
	nextReportNames := func(t *testing.T, f *trimFixture, tr *trimmer, refused string) {
		t.Helper()
		if f.logged("recovered a panic") != 1 || len(f.reports()) != 0 {
			t.Fatalf("the trimmer wrote %q and sent %d reports, want the panic recovered and no report", f.linesWritten(), len(f.reports()))
		}
		if err := os.RemoveAll(filepath.Join(f.proc, refused)); err != nil {
			t.Fatal(err)
		}
		tr.procRoot = f.proc
		tr.pause = 0 // a pause a sweep before it set, which would hold off the next sweep
		set := *trimSettings()
		tr.tick(context.Background(), &set)
		sent := f.reports()
		if len(sent) != 1 {
			t.Fatalf("the trimmer sent %d reports from the sweep after the one that panicked, want one", len(sent))
		}
		if named := strings.Join(sent[0].RefusedReads, "; "); !strings.Contains(named, "process "+refused+":") {
			t.Errorf("the report names the refused reads %q and counts %d more, want the refusal of process %s the sweep before met: the panic after it lost it",
				sent[0].RefusedReads, sent[0].RefusedReadsOmitted, refused)
		}
		if strings.Contains(sent[0].StopReason, "refused") {
			t.Errorf("the second sweep stopped because %q, want it to have read /proc in full", sent[0].StopReason)
		}
	}

	t.Run("in the sweep's last reading", func(t *testing.T) {
		f := newTrimFixture(t, 6)
		placed := whileSweeping(t, f)
		f.useVolume(steady(90))
		// The interval's own reading is the first; the sweep's last, after its one deletion and
		// the refusal, is the second.
		f.setOnRead(func(call int) {
			if call == 2 {
				panic("the volume reader fell over on the sweep's last reading")
			}
		})
		f.setSettings(trimSettings())
		tr := f.trimmer()
		set := *trimSettings()
		if _, ending := tr.tick(context.Background(), &set); ending != trimGoesOn {
			t.Fatalf("the interval whose sweep panicked ended the trimmer (%v), want it passed over", ending)
		}
		refused := placed()
		if f.logged("a /proc read the kernel refused while the sweep ran") != 0 || !strings.Contains(strings.Join(f.linesWritten(), "\n"), "the sweep's last reading") {
			t.Fatalf("the trimmer wrote %q, want the sweep stopped on the refusal and its last reading fallen over before any line of the interval", f.linesWritten())
		}
		nextReportNames(t, f, tr, refused)
	})

	t.Run("in the close of the sweep's deleter", func(t *testing.T) {
		f := newTrimFixture(t, 6)
		placed := whileSweeping(t, f)
		f.useVolume(steady(90))
		f.setSettings(trimSettings())
		tr := f.trimmer()
		var falling atomic.Bool
		falling.Store(true)
		tr.removals = func(RoleCover, []*sweepDir) (entryRemover, error) {
			return closeFallsOver{falling: &falling}, nil
		}
		set := *trimSettings()
		if _, ending := tr.tick(context.Background(), &set); ending != trimGoesOn {
			t.Fatalf("the interval whose sweep panicked ended the trimmer (%v), want it passed over", ending)
		}
		refused := placed()
		if !strings.Contains(strings.Join(f.linesWritten(), "\n"), "the sweep's deleter fell over as it was closed") {
			t.Fatalf("the trimmer wrote %q, want the panic of the deleter's close recovered", f.linesWritten())
		}
		nextReportNames(t, f, tr, refused)
	})

	t.Run("in the first line after the sweep", func(t *testing.T) {
		f := newTrimFixture(t, 0)
		f.useVolume(steady(90))
		f.setSettings(trimSettings())
		tr := f.trimmer()
		refusing, pids := writeProcFixture(t, fixtureProcess{pid: 4242, fdIsFile: true})
		tr.procRoot = refusing
		var falling atomic.Bool
		falling.Store(true)
		tr.logf = func(format string, args ...any) {
			if strings.Contains(format, "the sweep ended") && falling.CompareAndSwap(true, false) {
				panic("the log fell over writing the line of the sweep")
			}
			f.log(format, args...)
		}
		set := *trimSettings()
		if _, ending := tr.tick(context.Background(), &set); ending != trimGoesOn {
			t.Fatalf("the interval whose line panicked ended the trimmer (%v), want it passed over", ending)
		}
		nextReportNames(t, f, tr, strconv.Itoa(pids[0]))
	})
}

// A reported sweep has been read for a rise before tick writes its lines about the sweep (riseAt,
// tick), so one whose first line after the sweep panics anchors or pairs as one that wrote its
// lines does: the texts that define a reported sweep, a sweep that ended above the low mark and
// that the settings did not stop, name no exception for the logger. The sweeps start at 86%, 91%
// and 92%, and the line of the second ("the sweep ended") panics, so it sends no report: the 86%
// anchors episode 1, the 91% pairs with the 92%, and the third sweep's report carries the rise. At
// b1b8e361 riseAt ran after that line, so the 91% was never read, the 92% paired with the 86%, and
// the third report carried episode 1.
func TestAReportedSweepWhoseFirstLineAfterItPanicsHasBeenReadForARise(t *testing.T) {
	const fell = "the log fell over writing the line of a sweep"
	f := newTrimFixture(t, 0) // no entry: every sweep frees nothing and takes no reading of its own
	f.useVolume(script(86, 91, 92))
	f.setSettings(trimSettings())
	tr := f.trimmer()
	var falling atomic.Bool
	tr.logf = func(format string, args ...any) {
		if strings.Contains(format, "the sweep ended") && falling.CompareAndSwap(true, false) {
			panic(fell)
		}
		f.log(format, args...)
	}
	set := *trimSettings()
	for sweep := range 3 {
		falling.Store(sweep == 1)
		tr.pause = 0 // the pause after a sweep that freed nothing (tick), which would hold off the next sweep
		if _, ending := tr.tick(context.Background(), &set); ending != trimGoesOn {
			t.Fatalf("sweep %d ended the trimmer (%v), want it passed over at worst", sweep+1, ending)
		}
	}
	if faults := f.logged("recovered a panic"); faults != 1 || !strings.Contains(strings.Join(f.linesWritten(), "\n"), fell) {
		t.Fatalf("the trimmer wrote %q, want the line of the second sweep fallen over and recovered", f.linesWritten())
	}

	assertReportEpisodes(t, f, []reportEpisode{
		{1, shimwire.VolumeEpisodeFirst}, // from 86%: anchors episode 1 at 86%
		{2, shimwire.VolumeEpisodeRise},  // from 92%, after the 91% whose line panicked: a floor of 91%, the rise
	})
	assertRises(t, f, "the floor of the last two reported sweep starts, 91.0% and 92.0%, is 91.0%, 5.0 points above the 86.0%")
	if faults := f.reports()[1].Faults; len(faults) != 1 || !strings.HasPrefix(faults[0], fell) {
		t.Errorf("the report carries the faults %q, want the panic of the second sweep's line", faults)
	}
}

// A reported sweep's start is recorded before the line of the rise it raises (riseAt), so a rise
// whose line panics, which only the logger can raise, leaves that sweep's start the first of the
// two the next rise reads, as the texts that define a reported sweep say of every line about it.
// The sweeps start at 86%, 91%, 97%, 97% and 97%, and the rise line of the third panics, once:
// the 86% anchors episode 1, the 91% pairs with the third sweep's 97% for a floor of 91%, which
// raises episode 2 and anchors it there, and the fourth sweep's 97% pairs with the third's for a
// floor of 97%, six points above, which raises episode 3; the fifth raises nothing. At 339f0681
// riseAt recorded the start only after that line, so its panic left the 91% in the third's place:
// the fourth sweep paired with the 91% and raised nothing, and the rise came one reported sweep
// late, at the fifth.
func TestARiseWhoseLinePanicsHasRecordedItsStartForTheNextRise(t *testing.T) {
	const fell = "the log fell over writing the line of a rise"
	f := newTrimFixture(t, 0) // no entry: every sweep frees nothing and takes no reading of its own
	f.useVolume(script(86, 91, 97, 97, 97))
	f.setSettings(trimSettings())
	tr := f.trimmer()
	var fallen atomic.Bool
	tr.logf = func(format string, args ...any) {
		if strings.Contains(format, "the floor of the last two reported sweep starts") && fallen.CompareAndSwap(false, true) {
			panic(fell)
		}
		f.log(format, args...)
	}
	set := *trimSettings()
	for sweep := range 5 {
		tr.pause = 0 // the pause after a sweep that freed nothing (tick), which would hold off the next sweep
		if _, ending := tr.tick(context.Background(), &set); ending != trimGoesOn {
			t.Fatalf("sweep %d ended the trimmer (%v), want it passed over at worst", sweep+1, ending)
		}
	}
	if faults := f.logged("recovered a panic"); faults != 1 || !strings.Contains(strings.Join(f.linesWritten(), "\n"), fell) {
		t.Fatalf("the trimmer wrote %q, want the rise line of the third sweep fallen over and recovered", f.linesWritten())
	}

	assertReportEpisodes(t, f, []reportEpisode{
		{1, shimwire.VolumeEpisodeFirst}, // from 86%: anchors episode 1 at 86%
		{1, shimwire.VolumeEpisodeFirst}, // from 91%, after the 86%: a floor of 86%, no rise
		{3, shimwire.VolumeEpisodeRise},  // from 97%, after the 97% whose rise line panicked: a floor of 97%, the next rise
		{3, shimwire.VolumeEpisodeRise},  // from 97%, after the 97%: a floor of 97%, no rise
	})
	assertRises(t, f, "the floor of the last two reported sweep starts, 97.0% and 97.0%, is 97.0%, 6.0 points above the 91.0%")
	if faults := f.reports()[2].Faults; len(faults) != 1 || !strings.HasPrefix(faults[0], fell) {
		t.Errorf("the report carries the faults %q, want the panic of the third sweep's rise line", faults)
	}
}

// The pause after a sweep that freed under a point is set before tick writes any line about the
// sweep (tick), so a sweep whose rise line or whose `the sweep ended` line panics, which only the
// logger can raise, still holds the next ten intervals off sweeping: R1 has a sweep that frees
// less than one percent of the volume start no new sweep for the next ten intervals, and the texts
// name no exception for the logger. Each subtest's sweeps free nothing, the line of one of them
// panics once, and the ten readings after that sweep must each pause and the eleventh sweep. In
// the rise's subtest the sweeps start at 86%, 91% and 97%, and the third, whose floor of 91% stands
// five points above the 86% anchor, raises the rise whose line panics. At c322d6f7 tick set the
// pause after both lines, so their panic left it at 0 and the next reading swept (INFRA-2009,
// review round 39).
func TestASweepWhoseLinePanicsStillPausesTheNextTenIntervals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		line   string // what the format of the line that panics holds
		volume func(int) volumeUsage
		before int // the sweeps before the one whose line panics
	}{
		{"its sweep ended line", "the sweep ended", steady(90), 0},
		{"its rise line", "the floor of the last two reported sweep starts", script(86, 91, 97), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fell := "the log fell over writing " + tc.name
			f := newTrimFixture(t, 0) // no entry: every sweep frees nothing and takes no reading of its own
			f.useVolume(tc.volume)
			f.setSettings(trimSettings())
			tr := f.trimmer()
			var falling atomic.Bool
			tr.logf = func(format string, args ...any) {
				if strings.Contains(format, tc.line) && falling.CompareAndSwap(true, false) {
					panic(fell)
				}
				f.log(format, args...)
			}
			set := *trimSettings()
			interval := func(what string) {
				t.Helper()
				if _, ending := tr.tick(context.Background(), &set); ending != trimGoesOn {
					t.Fatalf("%s ended the trimmer (%v), want it passed over at worst", what, ending)
				}
			}
			for range tc.before {
				interval("a sweep before the one whose line panics")
				tr.pause = 0 // the pause after a sweep that freed nothing, which would hold off the next sweep
			}
			falling.Store(true)
			interval("the sweep whose line panics")
			if falling.Load() || f.logged("recovered a panic") != 1 || !strings.Contains(strings.Join(f.linesWritten(), "\n"), fell) {
				t.Fatalf("the trimmer wrote %q, want the line fallen over and recovered", f.linesWritten())
			}

			// Written out rather than read off volumeQuietIntervals: R1's ten intervals.
			const pause = 10
			swept, paused := f.logged("sweeping"), f.logged("no sweep for another")
			for reading := 1; reading <= pause; reading++ {
				interval("a reading of the pause")
				if f.logged("sweeping") != swept || f.logged("no sweep for another") != paused+reading {
					t.Fatalf("reading %d after the sweep whose line panicked did not pause, want each of the %d readings after it to pause: %q",
						reading, pause, f.linesWritten())
				}
			}
			interval("the reading after the pause")
			if f.logged("sweeping") != swept+1 {
				t.Errorf("the reading after the pause wrote %q, want it to sweep: the pause is ten intervals and no more", f.linesWritten())
			}
		})
	}
}

// The first reading at or above the high mark marks the volume above it before the line that
// raises episode 1 is written, so a panic of that line, which tick recovers, still leaves a later
// reading at or below the low mark read as a recovery. The readings are 90% and then 65%, and the
// line of the first episode panics once. At c7700af9 episodeOf marked the volume only after that
// line, so the fall to 65% raised no recovery and the episode whose notices were spent went on.
func TestAPanicOfTheFirstEpisodesLineLeavesALaterFallARecovery(t *testing.T) {
	const fell = "the log fell over writing the line of the first episode"
	f := newTrimFixture(t, 0)
	f.useVolume(script(90, 65))
	f.setSettings(trimSettings())
	tr := f.trimmer()
	var fallen atomic.Bool
	tr.logf = func(format string, args ...any) {
		if strings.Contains(format, "the volume reached the high mark") && fallen.CompareAndSwap(false, true) {
			panic(fell)
		}
		f.log(format, args...)
	}
	set := *trimSettings()
	for reading := range 2 {
		if _, ending := tr.tick(context.Background(), &set); ending != trimGoesOn {
			t.Fatalf("reading %d ended the trimmer (%v), want it passed over at worst", reading+1, ending)
		}
	}
	if !fallen.Load() || f.logged("recovered a panic") != 1 {
		t.Fatalf("the trimmer wrote %q, want the line of the first episode fallen over and recovered", f.linesWritten())
	}
	if tr.episode != 2 || tr.raisedBy != shimwire.VolumeEpisodeRecovery {
		t.Errorf("after the fall to 65%% the trimmer is in episode %d raised by %q, want 2 by %q: the first reading at the high mark marks the volume above it before its line",
			tr.episode, tr.raisedBy, shimwire.VolumeEpisodeRecovery)
	}
}

// A sweep writes four lines once its outcome is settled (sweep): that its deleter could not be
// started, that its deleter ended, that the volume could not be read at its end, and how many more
// entries of a root it passed over than it named (passedOver.said). Each is written inside
// contain, so a panic of it, which only the logger can raise, is dropped and counted in no row,
// and tick still acts on that outcome: R1 has a sweep that frees less than one percent of the
// volume start no new sweep for the next ten intervals, and a reading of the trimmer's own at or
// below the low mark after one at or above the high mark is a recovery (episodeOf). Each subtest's
// line panics once. In a pause's subtest the sweep frees nothing: the ten readings after it must
// each pause, the eleventh must sweep, and both sweeps must report. In a recovery's the sweep
// deletes and its last reading is 65%: the trimmer must be in episode 2, raised by a recovery,
// with no report sent. Only the passed-over count and the line of a deleter that ended can follow
// deletions that reach the low mark: a deleter that could not be started deletes nothing, and a
// sweep whose last reading failed keeps the last one it took, which stood above the low mark or
// the sweep would have ended there. At 3b696b83 each line's panic unwound out of the sweep, so
// tick set no pause and the next reading swept, and read no last reading, so the fall to 65%
// raised no recovery (INFRA-2009, round 46).
func TestASweepWhoseLinePanicsOnceItsOutcomeIsSettledStillPausesOrRecovers(t *testing.T) {
	const fell = "the log fell over writing a line of a sweep whose outcome is settled"
	// passOver leaves eleven links in bun's install cache, one past the ten a sweep names
	// (volumePassedOverLines), so the sweep counts the eleventh in its last line (said).
	passOver := func(t *testing.T, f *trimFixture, _ *trimmer) {
		t.Helper()
		bun := f.roots[3]
		if bun.Entry != ImmediateChildren {
			t.Fatalf("the fourth reclaimable root is %+v, want bun's install cache", bun)
		}
		target := t.TempDir()
		for at := range 11 {
			if err := os.Symlink(target, filepath.Join(bun.Path, fmt.Sprintf("link-%02d", at))); err != nil {
				t.Fatal(err)
			}
		}
	}
	// dies has each sweep's deleter end once it has removed after entries (dyingRemover).
	dies := func(after int) func(*testing.T, *trimFixture, *trimmer) {
		return func(_ *testing.T, _ *trimFixture, tr *trimmer) {
			closed := 0
			tr.removals = func(RoleCover, []*sweepDir) (entryRemover, error) {
				return &dyingRemover{after: after, err: fmt.Errorf("%w: read its answer: EOF", errRemoverGone), closed: &closed}, nil
			}
		}
	}
	for _, tc := range []struct {
		name     string
		line     string // what the line that panics holds, as written
		perRoot  int    // the fixture's entries in each root
		volume   func(int) volumeUsage
		stage    func(*testing.T, *trimFixture, *trimmer)
		recovery bool // the sweep's last reading is at the low mark or under it, where the others pause
	}{
		{"the passed-over count of a sweep that pauses", "more entries of the reclaimable root", 0, steady(90), passOver, false},
		{"the passed-over count of a sweep that recovers", "more entries of the reclaimable root", 1, script(90, 65), passOver, true},
		{"the deleter that could not be started", volumeTrimLog + "the sweep's deleter could not be started", 1, steady(90), func(_ *testing.T, _ *trimFixture, tr *trimmer) {
			tr.removals = func(RoleCover, []*sweepDir) (entryRemover, error) {
				return nil, errors.New("start the sweep's deleter in a mount namespace of its own: operation not permitted")
			}
		}, false},
		{"the deleter that ended in a sweep that pauses", volumeTrimLog + "the sweep's deleter ended", 1, steady(90), dies(0), false},
		{"the deleter that ended in a sweep that recovers", volumeTrimLog + "the sweep's deleter ended", 1, script(90, 65), dies(1), true},
		{"the last reading that failed", "the volume could not be read at the end of the sweep", 1, steady(90), func(t *testing.T, _ *trimFixture, _ *trimmer) {
			// The interval's own reading is the tick's first; the sweep's last, after its four
			// deletions, is the second. The case calls tick itself, on its own goroutine, so the
			// count needs no lock.
			fixture, reads := readVolumeUsage, 0
			readVolumeUsage = func(path string) (volumeUsage, error) {
				if reads++; reads == 2 {
					return volumeUsage{}, errors.New("launcher: statfs " + path + ": input/output error")
				}
				return fixture(path)
			}
			t.Cleanup(func() { readVolumeUsage = fixture })
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTrimFixture(t, tc.perRoot)
			f.useVolume(tc.volume)
			f.setSettings(trimSettings())
			tr := f.trimmer()
			var falling atomic.Bool
			falling.Store(true)
			tr.logf = func(format string, args ...any) {
				if strings.Contains(fmt.Sprintf(format, args...), tc.line) && falling.CompareAndSwap(true, false) {
					panic(fell)
				}
				f.log(format, args...)
			}
			tc.stage(t, f, tr)
			set := *trimSettings()
			interval := func(what string) {
				t.Helper()
				if _, ending := tr.tick(context.Background(), &set); ending != trimGoesOn {
					t.Fatalf("%s ended the trimmer (%v), want it to go on", what, ending)
				}
			}
			interval("the sweep whose line panics")
			if falling.Load() || f.logged("sweeping") != 1 {
				t.Fatalf("the trimmer wrote %q, want one sweep, whose line fell over", f.linesWritten())
			}

			if tc.recovery {
				if tr.episode != 2 || tr.raisedBy != shimwire.VolumeEpisodeRecovery {
					t.Errorf("after the sweep that left 65%% the trimmer is in episode %d raised by %q, want 2 by %q: its last reading is read for a recovery though its line panicked; it wrote %q",
						tr.episode, tr.raisedBy, shimwire.VolumeEpisodeRecovery, f.linesWritten())
				}
				if sent := len(f.reports()); sent != 0 {
					t.Errorf("the trimmer sent %d reports, want none: the sweep reached the low mark", sent)
				}
			} else {
				// Written out rather than read off volumeQuietIntervals: R1's ten intervals.
				const pause = 10
				swept, paused := f.logged("sweeping"), f.logged("no sweep for another")
				for reading := 1; reading <= pause; reading++ {
					interval("a reading of the pause")
					if f.logged("sweeping") != swept || f.logged("no sweep for another") != paused+reading {
						t.Fatalf("reading %d after the sweep whose line panicked did not pause, want each of the %d readings after it to pause: %q",
							reading, pause, f.linesWritten())
					}
				}
				interval("the reading after the pause")
				if f.logged("sweeping") != swept+1 {
					t.Errorf("the reading after the pause wrote %q, want it to sweep: the pause is ten intervals and no more", f.linesWritten())
				}
				if sent := len(f.reports()); sent != 2 {
					t.Errorf("the trimmer sent %d reports, want 2: the sweep whose line panicked and the one after its pause, each ended above the low mark", sent)
				}
			}
			if faults := f.logged("recovered a panic"); faults != 0 {
				t.Errorf("the trimmer recovered %d panics, want the line's dropped: it comes once the sweep's outcome is settled, and is counted in no row; it wrote %q", faults, f.linesWritten())
			}
		})
	}
}

// A held report whose resend panics at three intervals in a row switches the trimmer off in the
// sweep row, and R1 then has the trimmer send one last report saying so. The resend here panics
// on the held report alone (a report that is not the switched-off one), so the switched-off
// report can be sent whatever the fix. At c31c1148 switchOff hands its report to deliver, whose
// flush tries the held report first: that resend panics again, run's last contain ends the
// trimmer, and the switched-off report is never tried (INFRA-2009, tester's round 36).
func TestAHeldReportWhoseResendPanicsLeavesTheSwitchedOffReportSent(t *testing.T) {
	f := newTrimFixture(t, 10) // forty entries, four points: the first sweep leaves 86% and sets no pause
	f.useVolume(f.modelled(90))
	f.setSettings(trimSettings())
	f.refuseSends(errors.New("no daemon connection is up to carry it"))
	tr := f.trimmer()
	const fell = "the connection fell over resending the held report"
	var falling atomic.Bool
	tr.send = func(report shimwire.LauncherVolumeReport) error {
		if falling.Load() && !report.SwitchedOff {
			panic(fell)
		}
		return f.send(report)
	}
	_, done := f.start(tr)
	f.tick("the sweep ended")
	testwait.Eventually(t, "the trimmer to hold the report its send refused", func() bool {
		return f.logged("holding it for the next attempt") == 1
	})

	falling.Store(true)
	f.refuseSends(nil)
	for range volumeFaultLimit {
		f.tick("recovered a panic")
	}
	testwait.Eventually(t, "the trimmer to switch itself off", func() bool {
		return f.logged("in a row of sweeps and reports: the trimmer is off") == 1
	})
	testwait.Eventually(t, "the switched-off report to be sent, or the trimmer to end", func() bool {
		for _, report := range f.reports() {
			if report.SwitchedOff {
				return true
			}
		}
		select {
		case <-done:
			return true
		default:
			return f.logged("the last report of the switched-off trimmer panicked") > 0
		}
	})
	var last *shimwire.LauncherVolumeReport
	for _, report := range f.reports() {
		if report.SwitchedOff {
			last = &report
		}
	}
	switch {
	case last == nil:
		t.Fatalf("the trimmer sent %d reports and none was the switched-off report, want the last report saying it switched itself off: the held report's resend took its send", len(f.reports()))
	case !strings.Contains(last.StopReason, "in a row of sweeps and reports"):
		t.Errorf("the switched-off report says %q, want the sweep row named", last.StopReason)
	case len(last.Faults) != volumeFaultLimit || !strings.HasPrefix(last.Faults[0], fell):
		t.Errorf("the switched-off report carries the faults %q, want the %d panics of the resend", last.Faults, volumeFaultLimit)
	}
}

// A held report's resend is a sweep's work, and the work it was tried in goes on as that work's
// once it returns (flush): tick tries the held report before the interval's reading, so a reading
// that panics after a resend that worked adds to the reading row and not to the sweep row. The
// case's one sweep frees nothing and pauses, and its report's send is refused, so the report is
// held; the connection is then up, the next tick sends the held report, and that tick's reading
// panics. The negative control is a copy of flush that leaves the sweep row in force after the
// resend, under which that reading's panic is the first of the sweep row (INFRA-2009, tester's
// round 38: no case failed without flush's "t.doing = was").
func TestAReadingPanicAfterAHeldReportsResendCountsInTheReadingRow(t *testing.T) {
	f := newTrimFixture(t, 0) // no entry: the sweep frees nothing, reports and pauses
	f.useVolume(steady(90))
	f.setSettings(trimSettings())
	// The sweep's reading is the first, and the first of its pause, the second, panics.
	f.setOnRead(func(call int) {
		if call == 2 {
			panic(fmt.Sprintf("the volume reader fell over on call %d", call))
		}
	})
	f.refuseSends(errors.New("no daemon connection is up to carry it"))
	tr := f.trimmer()
	_, done := f.start(tr)
	f.tick("holding it for the next attempt")

	f.refuseSends(nil)
	f.tick("recovered a panic")
	if f.logged("(1 in a row of readings)") != 1 || f.logged("in a row of sweeps and reports") != 0 {
		t.Fatalf("the trimmer wrote %q, want the reading's panic after the held report's resend counted as the first of the reading row", f.linesWritten())
	}
	if sent := len(f.reports()); sent != 1 {
		t.Errorf("the trimmer sent %d reports, want the held one, sent before the reading", sent)
	}
	select {
	case <-done:
		t.Fatal("the trimmer's loop returned, want it still reading the volume")
	default:
	}
}

// Refused settings are remembered as said only once their lines are written (awaitSettings), so a
// refusal line that panics, which only the logger can raise and run counts in the reading row,
// leaves the refusal news: the wait run starts again writes the lines and tries a held report, as
// R2 has the trimmer log the key and the value of settings it refuses and the daemon that sent
// them has a case for a report. A logger that panics on every such line then switches the trimmer
// off at its third panic, as run's text has every panic of the wait's lines do. The settings are a
// low mark of 80 under a high one of 70. At c322d6f7 the refusal was remembered before its lines,
// so after one panic the wait wrote no refusal and tried no held report while those settings
// stood, and a logger that panicked on every refusal line cost one fault and left the trimmer
// waiting for ever (INFRA-2009, review round 39).
func TestARefusalWhoseLinePanicsIsWrittenAgainAndTriesTheHeldReport(t *testing.T) {
	refused := *trimSettings()
	refused.HighPercent, refused.LowPercent = 70, 80
	const refusal = "refusing the settings: low_percent is 80 and high_percent is 70"
	const fell = "the log fell over writing a refusal"

	t.Run("one panic of its first line", func(t *testing.T) {
		f := newTrimFixture(t, 25) // a hundred entries, ten points: the sweep ends above the low mark and reports
		f.useVolume(f.modelled(90))
		f.setSettings(trimSettings())
		f.refuseSends(errors.New("no daemon connection is up to carry it"))
		tr := f.trimmer()
		var falling atomic.Bool
		tr.logf = func(format string, args ...any) {
			if strings.Contains(format, "refusing the settings") && falling.CompareAndSwap(true, false) {
				panic(fell)
			}
			f.log(format, args...)
		}
		_, done := f.start(tr)
		f.tick("the sweep ended")
		testwait.Eventually(t, "the trimmer to hold the report the outage refused", func() bool {
			return f.logged("holding it for the next attempt") == 1
		})

		falling.Store(true)
		f.refuseSends(nil)
		f.setSettings(&refused)
		f.tick("recovered a panic")
		// The wait asks for its clock only once it has said why it waits and tried a held report,
		// so a live clock is a wait that has done both or found it had nothing to say.
		testwait.Eventually(t, "the trimmer to wait for settings again after the panic", func() bool {
			_, waiting := f.ticks.live(volumeSettingsWait)
			return waiting
		})
		if falling.Load() || f.logged("(1 in a row of readings)") != 1 {
			t.Fatalf("the trimmer wrote %q, want the refusal line fallen over and counted as the first of the reading row", f.linesWritten())
		}
		if said := f.logged(refusal); said != 1 {
			t.Errorf("the trimmer wrote %d lines holding %q, want the refusal written once in the wait after the panic: %q", said, refusal, f.linesWritten())
		}
		sent := f.reports()
		switch {
		case len(sent) != 1:
			t.Errorf("the trimmer sent %d reports, want the held one, tried in the wait after the panic", len(sent))
		case sent[0].UsedBytes != int64(percentBytes(80)):
			t.Errorf("the report sent carries %d used bytes, want the %d of the sweep that was held", sent[0].UsedBytes, percentBytes(80))
		}
		select {
		case <-done:
			t.Error("the trimmer's loop returned after one fault, want it waiting for settings it can run")
		default:
		}
	})

	t.Run("a logger that panics on every refusal line", func(t *testing.T) {
		f := newTrimFixture(t, 0)
		f.setSettings(&refused)
		tr := f.trimmer()
		var refusals atomic.Int32
		tr.logf = func(format string, args ...any) {
			if strings.Contains(format, "refusing the settings") {
				refusals.Add(1)
				panic(fell)
			}
			f.log(format, args...)
		}
		_, done := f.start(tr)
		testwait.Eventually(t, "the trimmer to end, or to wait for settings", func() bool {
			select {
			case <-done:
				return true
			default:
			}
			_, waiting := f.ticks.live(volumeSettingsWait)
			return waiting
		})
		if asked := refusals.Load(); asked != 3 {
			t.Fatalf("the logger was asked for %d refusal lines, want 3: each wait run starts again after a panic of one writes the refusal again, and the third panic switches the trimmer off; it wrote %q",
				asked, f.linesWritten())
		}
		awaitClosed(t, done, "the trimmer's loop to return once the third panic switched it off and its last report was sent")
		sent := f.reports()
		if len(sent) != 1 {
			t.Fatalf("the trimmer sent %d reports, want the one last report of a trimmer that switched itself off", len(sent))
		}
		switch report := sent[0]; {
		case !report.SwitchedOff:
			t.Error("the last report does not say the trimmer switched itself off")
		case !strings.Contains(report.StopReason, "3 panics in a row of readings"):
			t.Errorf("the last report's stop reason is %q, want the three panics of the reading row that stopped it", report.StopReason)
		case len(report.Faults) != 3 || !strings.HasPrefix(report.Faults[2], fell):
			t.Errorf("the last report carries the faults %q, want the three panics of the refusal line", report.Faults)
		}
	})
}

// Refused settings are remembered as said before the wait tries a held report (awaitSettings),
// so a resend that panics, a fault of the sweep row (flush), is tried once over one refusal: run
// starts the wait again at once, with no tick between, and the refusal is no longer news there,
// so the report stays held for the first interval of settings the trimmer can run, as it does
// where its send fails. The case holds a report through an outage, and every resend of it then
// panics while the daemon sends settings the trimmer refuses, a low mark of 80 under a high one
// of 70. Remembered after the resend, the reason would still be news in the wait run starts
// again, which would try the report again, and its third panic would switch the trimmer off over
// that one refusal (INFRA-2009, review 5954632411).
func TestAHeldReportWhoseResendPanicsInTheWaitIsTriedOnceOverOneRefusal(t *testing.T) {
	refused := *trimSettings()
	refused.HighPercent, refused.LowPercent = 70, 80
	const refusal = "refusing the settings: low_percent is 80 and high_percent is 70"
	const fell = "the connection fell over resending the held report"
	f := newTrimFixture(t, 25) // a hundred entries, ten points: the sweep ends above the low mark and reports
	f.useVolume(f.modelled(90))
	f.setSettings(trimSettings())
	f.refuseSends(errors.New("no daemon connection is up to carry it"))
	tr := f.trimmer()
	var falling atomic.Bool
	tr.send = func(report shimwire.LauncherVolumeReport) error {
		if falling.Load() && !report.SwitchedOff {
			panic(fell)
		}
		return f.send(report)
	}
	stop, done := f.start(tr)
	f.tick("the sweep ended")
	testwait.Eventually(t, "the trimmer to hold the report the outage refused", func() bool {
		return f.logged("holding it for the next attempt") == 1
	})

	falling.Store(true)
	f.refuseSends(nil)
	f.setSettings(&refused)
	f.tick("recovered a panic")
	// The wait asks for its clock only once it has said why it waits and tried a held report,
	// so a live clock is a wait that has done both or found it had nothing to say.
	testwait.Eventually(t, "the trimmer to wait for settings again after the panic, or to end", func() bool {
		select {
		case <-done:
			return true
		default:
		}
		_, waiting := f.ticks.live(volumeSettingsWait)
		return waiting
	})
	select {
	case <-done:
		t.Fatalf("the trimmer's loop returned, want it waiting for settings it can run: one refusal tries the held report once; it wrote %q", f.linesWritten())
	default:
	}
	if faults := f.logged("recovered a panic"); faults != 1 || f.logged("(1 in a row of sweeps and reports)") != 1 {
		t.Errorf("the trimmer recovered %d panics, want the one resend's, counted as the first of the sweep row: %q", faults, f.linesWritten())
	}
	if said := f.logged(refusal); said != 1 {
		t.Errorf("the trimmer wrote %d lines holding %q, want the refusal written once: %q", said, refusal, f.linesWritten())
	}
	if f.logged("off for the life of this launcher process") != 0 {
		t.Errorf("the trimmer switched itself off over one refusal: %q", f.linesWritten())
	}
	stop()
	awaitClosed(t, done, "the trimmer's loop to return once the case stopped it")
	if tr.held == nil {
		t.Error("the trimmer holds no report, want the one whose resend panicked held for the first interval of settings it can run")
	}
	if sent := len(f.reports()); sent != 0 {
		t.Errorf("the trimmer sent %d reports, want none: the one it holds panicked on its resend", sent)
	}
}

// The line that says the trimmer is off is written once its last report has been sent or held
// (run), so a panic of that line, which only the logger can raise, loses no report: R1 has the
// trimmer that stops itself send "one last report saying so", and that report is what tells the
// daemon the pod's volume is no longer guarded. Three readings in a row panic, which switches the
// trimmer off, and the line then panics once. In the first subtest the connection takes the report
// at once; in the second it refuses it at first, so the report is held, and the next attempt
// (deliverLast) sends it once the connection takes it. At c322d6f7 switchOff wrote that line
// before it made the report, so its panic ended the trimmer with no report made (INFRA-2009, the
// architect's round 39).
func TestASwitchOffWhoseLinePanicsStillSendsTheLastReport(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refused bool // the connection refuses the first send of the last report
	}{
		{"the connection takes the report", false},
		{"the connection refuses it at first", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTrimFixture(t, 0)
			f.setOnRead(func(call int) { panic(fmt.Sprintf("the volume reader fell over on call %d", call)) })
			f.useVolume(steady(50))
			f.setSettings(trimSettings())
			if tc.refused {
				f.refuseSends(errors.New("no daemon connection is up to carry it"))
			}
			tr := f.trimmer()
			const fell = "the log fell over writing the line of the switch-off"
			var fallen atomic.Bool
			tr.logf = func(format string, args ...any) {
				if strings.Contains(format, "the trimmer is off") && fallen.CompareAndSwap(false, true) {
					panic(fell)
				}
				f.log(format, args...)
			}
			_, done := f.start(tr)
			// Written out rather than read off volumeFaultLimit, as the other cases of the switch-off count them.
			for range 3 {
				f.tick("recovered a panic")
			}
			if tc.refused {
				testwait.Eventually(t, "the trimmer to hold its last report, or to end", func() bool {
					select {
					case <-done:
						return true
					default:
					}
					return f.logged("holding it for the next attempt") == 1
				})
				f.refuseSends(nil)
				select {
				case <-done:
				default:
					offerSettingsTick(t, f)
				}
			}
			awaitClosed(t, done, "the trimmer's loop to return once its last report was sent")

			sent := f.reports()
			if len(sent) != 1 {
				t.Fatalf("the trimmer sent %d reports, want the one last report of a trimmer that switched itself off: the panic of its line lost it; it wrote %q",
					len(sent), f.linesWritten())
			}
			switch report := sent[0]; {
			case !report.SwitchedOff:
				t.Error("the last report does not say the trimmer switched itself off")
			case !strings.Contains(report.StopReason, "3 panics in a row of readings"):
				t.Errorf("the last report's stop reason is %q, want the three panics of the reading row that stopped it", report.StopReason)
			}
			if !fallen.Load() || f.logged("the trimmer is off") != 0 {
				t.Errorf("the trimmer wrote %q, want the line that says it is off fallen over", f.linesWritten())
			}
			if said := f.logged("the last report of the switched-off trimmer panicked"); said != 0 {
				t.Errorf("the trimmer wrote %q, want no panic of the last report: only its line fell over", f.linesWritten())
			}
		})
	}
}

// The switched-off trimmer's last report is held before deliver writes the lines that say so
// (deliver), so a panic of one of them, which only the logger can raise, comes once the send has
// answered and the report is held: run still tries it (deliverLast), since R2 has the trimmer
// hold its report until the connection is up again, and this report is the one that tells the
// daemon the pod's volume is no longer guarded. The case is the refused subtest of
// TestASwitchOffWhoseLinePanicsStillSendsTheLastReport with one reading more: four readings
// panic, with one run to its end after the first, so the last three switch the trimmer off and
// its last report leaves out the oldest of the four faults, which has deliver write saidLeftOut's
// line too; the connection refuses that report's first send. In the first subtest the line that
// says the report is held panics once, and in the second saidLeftOut's line after it. At 2dc1f390
// run tried the held report only where switchOff returned, so either panic ended the trimmer with
// the report held and never sent (INFRA-2009, review 5954632411).
func TestASwitchedOffReportWhoseLinePanicsOnceItIsHeldIsStillSent(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string // what the format of the line that panics holds
	}{
		{"the line that holds it", "holding it for the next attempt"},
		{"the line of what it leaves out", "leaves out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTrimFixture(t, 0)
			f.setOnRead(func(call int) {
				if call != 2 { // the second reading runs to its end, which ends the reading row
					panic(fmt.Sprintf("the volume reader fell over on call %d", call))
				}
			})
			f.useVolume(steady(50))
			f.setSettings(trimSettings())
			f.refuseSends(errors.New("no daemon connection is up to carry it"))
			tr := f.trimmer()
			const fell = "the log fell over writing a line of the held last report"
			var fallen atomic.Bool
			tr.logf = func(format string, args ...any) {
				if strings.Contains(format, tc.line) && fallen.CompareAndSwap(false, true) {
					panic(fell)
				}
				f.log(format, args...)
			}
			_, done := f.start(tr)
			f.tick("recovered a panic")
			f.tick("nothing to sweep")
			// Written out rather than read off volumeFaultLimit, as the other cases of the switch-off count them.
			for range 3 {
				f.tick("recovered a panic")
			}
			testwait.Eventually(t, "the line of the held last report to fall over, and the trimmer to wait to send that report or to end", func() bool {
				if !fallen.Load() {
					return false
				}
				select {
				case <-done:
					return true
				default:
				}
				_, waiting := f.ticks.live(volumeSettingsWait)
				return waiting
			})
			f.refuseSends(nil)
			select {
			case <-done:
			default:
				offerSettingsTick(t, f)
			}
			awaitClosed(t, done, "the trimmer's loop to return once its last report was sent")

			sent := f.reports()
			if len(sent) != 1 || tr.held != nil {
				t.Fatalf("the trimmer ended with held=%t reports=%d, want held=false reports=1: the last report held before its line fell over is sent once the connection takes it; it wrote %q",
					tr.held != nil, len(sent), f.linesWritten())
			}
			switch report := sent[0]; {
			case !report.SwitchedOff:
				t.Error("the last report does not say the trimmer switched itself off")
			case !strings.Contains(report.StopReason, "3 panics in a row of readings"):
				t.Errorf("the last report's stop reason is %q, want the three panics of the reading row that stopped it", report.StopReason)
			case len(report.Faults) != 3 || report.FaultsOmitted != 1:
				t.Errorf("the last report carries the faults %q and counts %d more, want the newest three of the four and the oldest counted", report.Faults, report.FaultsOmitted)
			}
			if f.logged("the report is still tried") != 1 || f.logged(fell) != 1 {
				t.Errorf("the trimmer wrote %q, want one line saying the line of the held report fell over and the report is still tried", f.linesWritten())
			}
			if said := f.logged("ends here without trying it again"); said != 0 {
				t.Errorf("the trimmer wrote %q, want no line ending it without trying the held report", f.linesWritten())
			}
			if said := f.logged("the last report of the switched-off trimmer was sent"); said != 1 {
				t.Errorf("the trimmer wrote %d lines saying its last report was sent, want one: %q", said, f.linesWritten())
			}
		})
	}
}

// The line of a failed attempt of the switched-off trimmer's last report is written inside
// contain (deliverLast), so a panic of it, which only the logger can raise, is dropped and the
// report stays held for the next attempt, which sends it once the connection takes it, as R2 has
// the trimmer hold its report until that connection is up again. The reason is remembered only
// once that line is written, so the next attempt that fails for it writes the line, once. Three
// readings panic, which switches the trimmer off; the connection refuses the report's first send,
// so it is held, and then its first retry, whose line panics once, and the retries after it, until
// the connection takes it. At 0c49568f the panic of that line ended the trimmer with the report
// held and never sent (INFRA-2009, round 42).
func TestASwitchedOffReportWhoseFailedAttemptsLinePanicsIsStillSent(t *testing.T) {
	const failed = "the last report of the switched-off trimmer could not be sent"
	const fell = "the log fell over writing a failed attempt of the last report"
	f := newTrimFixture(t, 0)
	f.setOnRead(func(call int) { panic(fmt.Sprintf("the volume reader fell over on call %d", call)) })
	f.useVolume(steady(50))
	f.setSettings(trimSettings())
	f.refuseSends(errors.New("no daemon connection is up to carry it"))
	tr := f.trimmer()
	var fallen atomic.Bool
	tr.logf = func(format string, args ...any) {
		if strings.Contains(format, failed) && fallen.CompareAndSwap(false, true) {
			panic(fell)
		}
		f.log(format, args...)
	}
	_, done := f.start(tr)
	// Written out rather than read off volumeFaultLimit, as the other cases of the switch-off count them.
	for range 3 {
		f.tick("recovered a panic")
	}
	// deliverLast asks for its clock only once switchOff has held the report, so a live clock is
	// the switched-off trimmer waiting to try it again.
	testwait.Eventually(t, "the trimmer to hold its last report and wait to try it again", func() bool {
		_, waiting := f.ticks.live(volumeSettingsWait)
		return waiting && f.logged("holding it for the next attempt") == 1
	})
	ended := func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
	offerSettingsTicksUntil(t, f, "the line of a failed attempt to fall over and a later failed attempt to write it, or the trimmer to end", func() bool {
		return ended() || fallen.Load() && f.logged(failed) == 1
	})
	if ended() {
		t.Fatalf("the trimmer ended with held=%t reports=%d once the line of a failed attempt fell over, want it holding its last report for the next attempt; it wrote %q",
			tr.held != nil, len(f.reports()), f.linesWritten())
	}
	f.refuseSends(nil)
	offerSettingsTicksUntil(t, f, "the trimmer to send its last report and end", ended)
	awaitClosed(t, done, "the trimmer's loop to return once its last report was sent")

	sent := f.reports()
	if len(sent) != 1 || tr.held != nil {
		t.Fatalf("the trimmer ended with held=%t reports=%d, want held=false reports=1: the last report whose failed attempt's line fell over is sent once the connection takes it; it wrote %q",
			tr.held != nil, len(sent), f.linesWritten())
	}
	if !sent[0].SwitchedOff {
		t.Error("the last report does not say the trimmer switched itself off")
	}
	if said := f.logged("ends here without trying it again"); said != 0 {
		t.Errorf("the trimmer wrote %q, want no line ending it without trying the held report", f.linesWritten())
	}
	if said := f.logged(failed); said != 1 {
		t.Errorf("the trimmer wrote %d lines holding %q, want the line that fell over written once, at a later failed attempt: %q", said, failed, f.linesWritten())
	}
	if said := f.logged("the last report of the switched-off trimmer was sent"); said != 1 {
		t.Errorf("the trimmer wrote %d lines saying its last report was sent, want one: %q", said, f.linesWritten())
	}
}

// The pause after a sweep that freed under a point is counted down before the line of a reading
// inside it is written (tick), so a panic of that line, which only the logger can raise, still
// spends that reading of the pause: R1's pause is ten intervals, and a line that fell over adds
// none. The case's sweep frees nothing, the line of the fourth reading of its pause panics once,
// and the pause must still end after ten readings in all, that one among them, with the eleventh
// reading after the sweep sweeping; each line written still names the readings of the pause
// left, as it did. At c322d6f7 the pause was counted down after that line, so its panic left the
// count where it stood and the pause ran eleven readings (INFRA-2009, round 39).
func TestAPauseLineThatPanicsStillSpendsItsReading(t *testing.T) {
	const fell = "the log fell over writing a line of the pause"
	f := newTrimFixture(t, 0) // no entry: the sweep frees nothing and pauses
	f.useVolume(steady(90))
	f.setSettings(trimSettings())
	tr := f.trimmer()
	var falling atomic.Bool
	tr.logf = func(format string, args ...any) {
		if strings.Contains(format, "no sweep for another") && falling.CompareAndSwap(true, false) {
			panic(fell)
		}
		f.log(format, args...)
	}
	set := *trimSettings()
	interval := func(what string) {
		t.Helper()
		if _, ending := tr.tick(context.Background(), &set); ending != trimGoesOn {
			t.Fatalf("%s ended the trimmer (%v), want it passed over at worst", what, ending)
		}
	}
	interval("the sweep")
	// Written out rather than read off volumeQuietIntervals: R1's ten intervals.
	const pause = 10
	for reading := 1; reading <= pause; reading++ {
		falling.Store(reading == 4)
		interval("a reading of the pause")
		if f.logged("sweeping") != 1 {
			t.Fatalf("reading %d after the sweep swept, want each of the %d readings after it to pause: %q", reading, pause, f.linesWritten())
		}
	}
	if falling.Load() || f.logged("recovered a panic") != 1 || !strings.Contains(strings.Join(f.linesWritten(), "\n"), fell) {
		t.Fatalf("the trimmer wrote %q, want the line of the fourth reading of the pause fallen over and recovered", f.linesWritten())
	}
	interval("the eleventh reading after the sweep")
	if f.logged("sweeping") != 2 {
		t.Errorf("the eleventh reading after the sweep wrote %q, want it to sweep: the pause is ten readings, the one whose line fell over among them", f.linesWritten())
	}
	// The lines written count the pause down as they did: 10, 9 and 8, and then 6 to 1 after the
	// reading whose line fell over, which spent the seventh.
	if lines := f.logged("no sweep for another"); lines != pause-1 {
		t.Errorf("the trimmer wrote %d lines of the pause, want %d: ten readings, one of whose lines fell over", lines, pause-1)
	}
	for _, left := range []int{10, 9, 8, 6, 5, 4, 3, 2, 1} {
		if said := f.logged(fmt.Sprintf("no sweep for another %d intervals", left)); said != 1 {
			t.Errorf("the trimmer wrote %d lines saying %d readings of the pause are left, want one: %q", said, left, f.linesWritten())
		}
	}
}

// The pause after a sweep that freed under a point is counted in intervals and not in readings
// (tick): R1's pause is ten intervals, counted from the end of that sweep, so an interval whose
// statfs fails, or panics, spends one of them as an interval whose reading answered does. The
// case's sweep frees nothing; in its pause the third and the fifth readings fail and the seventh
// panics, and the eleventh interval after the sweep must still sweep. Each line of the pause
// written still names the intervals left. At 2dc1f390 the pause was counted down only once the
// statfs had answered, so those three intervals spent none of it and the pause ran thirteen
// intervals (INFRA-2009, review 5954632411).
func TestAPauseIsSpentByAnIntervalWhoseReadingFailsOrPanics(t *testing.T) {
	const fell = "the volume reader fell over in the pause"
	f := newTrimFixture(t, 0) // no entry: the sweep frees nothing and pauses
	f.useVolume(steady(90))
	f.setSettings(trimSettings())
	// The case calls tick itself, on its own goroutine, so these need no lock.
	var failing error
	panicking := false
	f.setOnRead(func(int) {
		if panicking {
			panic(fell)
		}
	})
	fixture := readVolumeUsage
	readVolumeUsage = func(path string) (volumeUsage, error) {
		if failing != nil {
			return volumeUsage{}, failing
		}
		return fixture(path)
	}
	t.Cleanup(func() { readVolumeUsage = fixture })
	tr := f.trimmer()
	set := *trimSettings()
	interval := func(what string) {
		t.Helper()
		if _, ending := tr.tick(context.Background(), &set); ending != trimGoesOn {
			t.Fatalf("%s ended the trimmer (%v), want it passed over at worst", what, ending)
		}
	}
	interval("the sweep")
	// Written out rather than read off volumeQuietIntervals: R1's ten intervals.
	const pause = 10
	for at := 1; at <= pause; at++ {
		failing, panicking = nil, at == 7
		if at == 3 || at == 5 {
			failing = errors.New("launcher: statfs " + f.root + ": input/output error")
		}
		interval("an interval of the pause")
		if f.logged("sweeping") != 1 {
			t.Fatalf("interval %d after the sweep swept, want each of the %d intervals after it to pause: %q", at, pause, f.linesWritten())
		}
	}
	failing, panicking = nil, false
	if f.logged("could not be read") != 2 || f.logged("recovered a panic") != 1 || !strings.Contains(strings.Join(f.linesWritten(), "\n"), fell) {
		t.Fatalf("the trimmer wrote %q, want the readings of two intervals of the pause failed and that of one fallen over and recovered", f.linesWritten())
	}
	interval("the eleventh interval after the sweep")
	if f.logged("sweeping") != 2 {
		t.Errorf("the eleventh interval after the sweep wrote %q, want it to sweep: the pause is ten intervals, the three whose reading failed or panicked among them", f.linesWritten())
	}
	// The lines written count the pause down as they did: 10 and 9, then 7, 5, 3, 2 and 1 after the
	// intervals whose reading failed or panicked, which spent the 8, the 6 and the 4.
	for _, left := range []int{10, 9, 7, 5, 3, 2, 1} {
		if said := f.logged(fmt.Sprintf("no sweep for another %d intervals", left)); said != 1 {
			t.Errorf("the trimmer wrote %d lines saying %d intervals of the pause are left, want one: %q", said, left, f.linesWritten())
		}
	}
}

// The pause after a sweep that freed under a point is counted down where tick begins its
// interval, before the line of new settings and before the resend of a held report (flush), so an
// interval in which either panics still spends one of the pause: R1's pause is ten intervals,
// counted from the end of that sweep, and a panic may skip what follows it in its own interval and
// no more. The resend's panic is the connection's send, not the logger's. The case's sweep frees
// nothing; at the first interval of its pause the held report's resend panics, or the line of new
// settings does, and the eleventh interval after the sweep must still sweep. Each line of the
// pause written still names the intervals left, and the interval that panicked writes none. At
// 9fd392ce the pause was counted down in the reading (intervalReading), after both, so that
// interval spent none of it: after ten intervals the pause stood at 1 and the eleventh interval
// did not sweep (INFRA-2009, review 5962332201).
func TestAPauseIsSpentByAnIntervalThatPanicsBeforeItsReading(t *testing.T) {
	// paused drives the sweep, the ten intervals of its pause, the first of which fallOver readies
	// to panic before its reading, and the interval after them.
	paused := func(t *testing.T, f *trimFixture, tr *trimmer, fallOver func(), fell string) {
		t.Helper()
		set := *trimSettings()
		interval := func(what string) {
			t.Helper()
			if _, ending := tr.tick(context.Background(), &set); ending != trimGoesOn {
				t.Fatalf("%s ended the trimmer (%v), want it passed over at worst", what, ending)
			}
		}
		interval("the sweep")
		// Written out rather than read off volumeQuietIntervals: R1's ten intervals.
		const pause = 10
		for at := 1; at <= pause; at++ {
			if at == 1 {
				fallOver()
			}
			interval("an interval of the pause")
			if f.logged("sweeping") != 1 {
				t.Fatalf("interval %d after the sweep swept, want each of the %d intervals after it to pause: %q", at, pause, f.linesWritten())
			}
		}
		if f.logged("recovered a panic") != 1 || !strings.Contains(strings.Join(f.linesWritten(), "\n"), fell) {
			t.Fatalf("the trimmer wrote %q, want the first interval of the pause fallen over before its reading and recovered", f.linesWritten())
		}
		interval("the eleventh interval after the sweep")
		if f.logged("sweeping") != 2 {
			t.Errorf("the eleventh interval after the sweep wrote %q, want it to sweep: the pause is ten intervals, the one that fell over before its reading among them", f.linesWritten())
		}
		// The lines written count the pause down from 9 to 1: the interval that fell over spent
		// the 10 and wrote no line of the pause.
		for left := 1; left <= pause; left++ {
			want := 1
			if left == pause {
				want = 0
			}
			if said := f.logged(fmt.Sprintf("no sweep for another %d intervals", left)); said != want {
				t.Errorf("the trimmer wrote %d lines saying %d intervals of the pause are left, want %d: %q", said, left, want, f.linesWritten())
			}
		}
	}

	t.Run("the held report's resend", func(t *testing.T) {
		const fell = "the connection fell over resending the held report"
		f := newTrimFixture(t, 0) // no entry: the sweep frees nothing, reports and pauses
		f.useVolume(steady(90))
		f.setSettings(trimSettings())
		f.refuseSends(errors.New("no daemon connection is up to carry it"))
		tr := f.trimmer()
		var falling atomic.Bool
		tr.send = func(report shimwire.LauncherVolumeReport) error {
			if falling.CompareAndSwap(true, false) {
				panic(fell)
			}
			return f.send(report)
		}
		paused(t, f, tr, func() {
			if f.logged("holding it for the next attempt") != 1 {
				t.Fatalf("the trimmer wrote %q, want the sweep's report held, its send refused", f.linesWritten())
			}
			f.refuseSends(nil)
			falling.Store(true)
		}, fell)
		if sent := len(f.reports()); sent != 2 {
			t.Errorf("the trimmer sent %d reports, want 2: the held one, resent at the second interval of the pause, and the report of the eleventh interval's sweep", sent)
		}
	})

	t.Run("the line of new settings", func(t *testing.T) {
		const fell = "the log fell over writing the line of new settings"
		f := newTrimFixture(t, 0) // no entry: the sweep frees nothing and pauses
		f.useVolume(steady(90))
		f.setSettings(trimSettings())
		tr := f.trimmer()
		var falling atomic.Bool
		tr.logf = func(format string, args ...any) {
			if strings.Contains(format, "new settings") && falling.CompareAndSwap(true, false) {
				panic(fell)
			}
			f.log(format, args...)
		}
		paused(t, f, tr, func() {
			changed := *trimSettings()
			changed.MinAgeMinutes++
			f.setSettings(&changed)
			falling.Store(true)
		}, fell)
	})
}

// A volume this process cannot read is remembered as said only once its line is written
// (intervalReading), so a panic of that line, which only the logger can raise, leaves the error
// news: the next interval writes the line, once, and the one after writes none, as a standing
// state is said once. At c322d6f7 the error was remembered before its line, so after one panic of
// it the line was not written for as long as the error lasted (INFRA-2009, round 39).
func TestAnUnreadableVolumesLineThatPanicsIsWrittenAtTheNextInterval(t *testing.T) {
	const fell = "the log fell over writing that the volume could not be read"
	f := newTrimFixture(t, 0)
	f.setSettings(trimSettings())
	working := readVolumeUsage
	readVolumeUsage = func(path string) (volumeUsage, error) {
		return volumeUsage{}, errors.New("launcher: statfs " + path + ": input/output error")
	}
	t.Cleanup(func() { readVolumeUsage = working })
	tr := f.trimmer()
	var falling atomic.Bool
	falling.Store(true)
	tr.logf = func(format string, args ...any) {
		if strings.Contains(format, "could not be read") && falling.CompareAndSwap(true, false) {
			panic(fell)
		}
		f.log(format, args...)
	}
	set := *trimSettings()
	const unreadable = "input/output error"
	for at, want := range []int{0, 1, 1} {
		if _, ending := tr.tick(context.Background(), &set); ending != trimGoesOn {
			t.Fatalf("interval %d ended the trimmer (%v), want it passed over at worst", at+1, ending)
		}
		if at == 0 && (falling.Load() || f.logged("recovered a panic") != 1) {
			t.Fatalf("the trimmer wrote %q, want the line of the volume it cannot read fallen over and recovered", f.linesWritten())
		}
		if said := f.logged("could not be read: launcher: statfs"); said != want {
			t.Fatalf("after interval %d the trimmer wrote %d lines naming the %s, want %d: the line that fell over is written at the next interval, and once; it wrote %q",
				at+1, said, unreadable, want, f.linesWritten())
		}
	}
}

// tick decides what it can before the line that says it (tick), so a panic of that line, which
// only the logger can raise, leaves the decision standing, as tick's recovery leaves restart and
// the ending as they stand: settings that changed are in force and a new period's clock is made
// again, a sweep that returned has the clock made again so the next reading comes one interval
// after it, and settings gone, before a sweep or in the middle of one, end the loop for the wait,
// which deals with a held report as R2 has it (awaitSettings). Each subtest's line panics once,
// but the one of a sweep whose own reading panics, which is of no line (below).
// At c322d6f7 each of these was decided after its line, so its panic left the settings unchanged
// and the clock at the old period for one more interval, kept the clock a sweep had begun on, or
// kept the loop on its clock for one more interval in place of the wait (INFRA-2009, round 39).
//
// In the subtest of a sweep whose own reading panics, a step other than the logger panics inside
// the sweep, and the clock is made again all the same, since tick sets restart before the sweep.
// At 2dc1f390 restart was set once the sweep had returned, so that panic kept the clock the sweep
// began on, and a tick that came due while it ran was taken at once (INFRA-2009, review
// 5954632411).
func TestATickWhoseLinePanicsKeepsWhatItDecidedBeforeIt(t *testing.T) {
	const fell = "the log fell over writing a line of the interval"
	// falls has the trimmer's logger fall over once on the line whose format holds line.
	falls := func(f *trimFixture, tr *trimmer, line string) *atomic.Bool {
		var falling atomic.Bool
		falling.Store(true)
		tr.logf = func(format string, args ...any) {
			if strings.Contains(format, line) && falling.CompareAndSwap(true, false) {
				panic(fell)
			}
			f.log(format, args...)
		}
		return &falling
	}
	// recovered holds that the line fell over and its panic was the one fault.
	recovered := func(t *testing.T, f *trimFixture, falling *atomic.Bool) {
		t.Helper()
		if falling.Load() || f.logged("recovered a panic") != 1 || !strings.Contains(strings.Join(f.linesWritten(), "\n"), fell) {
			t.Fatalf("the trimmer wrote %q, want the line fallen over and recovered", f.linesWritten())
		}
	}

	t.Run("the line of new settings", func(t *testing.T) {
		f := newTrimFixture(t, 0)
		f.useVolume(steady(50))
		shorter := *trimSettings()
		shorter.IntervalSeconds = 10
		f.setSettings(&shorter)
		tr := f.trimmer()
		falling := falls(f, tr, "new settings:")
		set := *trimSettings()
		restart, ending := tr.tick(context.Background(), &set)
		recovered(t, f, falling)
		if ending != trimGoesOn || !restart || set != shorter {
			t.Errorf("the interval whose new settings' line fell over answered restart %t and %v with %+v in force, want the clock made again at %s with %+v in force",
				restart, ending, set, settingsInterval(shorter), shorter)
		}
	})

	t.Run("the line of a sweep that returned", func(t *testing.T) {
		f := newTrimFixture(t, 0) // no entry: the sweep frees nothing, reports and pauses
		f.useVolume(steady(90))
		f.setSettings(trimSettings())
		tr := f.trimmer()
		falling := falls(f, tr, "the sweep ended")
		set := *trimSettings()
		restart, ending := tr.tick(context.Background(), &set)
		recovered(t, f, falling)
		if ending != trimGoesOn || !restart {
			t.Errorf("the interval whose sweep's line fell over answered restart %t and %v, want the clock made again: the next reading comes one interval after the sweep", restart, ending)
		}
	})

	t.Run("a sweep whose own reading panics", func(t *testing.T) {
		f := newTrimFixture(t, 3) // entries the sweep deletes, so it reads the volume after them
		f.useVolume(steady(90))
		f.setSettings(trimSettings())
		const reader = "the volume reader fell over in the sweep"
		f.setOnRead(func(call int) {
			if call >= 2 { // the first reading is the interval's, every later one the sweep's own
				panic(reader)
			}
		})
		tr := f.trimmer()
		set := *trimSettings()
		restart, ending := tr.tick(context.Background(), &set)
		if lines := strings.Join(f.linesWritten(), "\n"); f.logged("recovered a panic") != 1 || !strings.Contains(lines, reader) || !strings.Contains(lines, "(*trimmer).sweep(") {
			t.Fatalf("the trimmer wrote %q, want the sweep's own reading fallen over inside the sweep and recovered", f.linesWritten())
		}
		if ending != trimGoesOn || !restart {
			t.Errorf("the interval whose sweep's own reading panicked answered restart %t and %v, want the clock made again: the next reading comes one interval after the sweep, not on the clock it began on", restart, ending)
		}
	})

	t.Run("the line of settings gone", func(t *testing.T) {
		f := newTrimFixture(t, 0)
		f.useVolume(steady(50))
		tr := f.trimmer()
		falling := falls(f, tr, "no settings are in force")
		set := *trimSettings()
		restart, ending := tr.tick(context.Background(), &set)
		recovered(t, f, falling)
		if ending != trimWaits || restart {
			t.Errorf("the interval whose line of settings gone fell over answered restart %t and %v, want %v: the loop ends for the wait", restart, ending, trimWaits)
		}
		if reads := f.readCount(); reads != 0 {
			t.Errorf("the trimmer read the volume %d times with no settings in force, want none", reads)
		}
	})

	t.Run("the line of a sweep the settings stopped", func(t *testing.T) {
		f := newTrimFixture(t, 0) // no entry it may delete
		for _, root := range f.roots {
			if root.Entry == ImmediateChildren {
				// One entry too young to delete: the listing asks the settings before it reads an
				// entry's age (list), so the sweep has an entry boundary the answer stops it at.
				mkdirFixture(t, filepath.Join(root.Path, "tool-fresh"))
			}
		}
		f.useVolume(steady(90))
		f.setSettings(trimSettings())
		var stopping atomic.Bool
		stopping.Store(true)
		f.clock.setHook(func(*fakeClock) {
			// The clock's first reading of the interval is the sweep's own start (sweep): the
			// answer with no settings arrives there, after the interval's own read of them.
			if stopping.CompareAndSwap(true, false) {
				f.setSettings(nil)
			}
		})
		tr := f.trimmer()
		falling := falls(f, tr, "the settings stopped the trimmer")
		set := *trimSettings()
		_, ending := tr.tick(context.Background(), &set)
		recovered(t, f, falling)
		if ending != trimWaits {
			t.Errorf("the interval whose line of the sweep the settings stopped fell over answered %v, want %v: the loop ends for the wait", ending, trimWaits)
		}
	})
}

// run tries a held report after a panic of switchOff only when that report is the switched-off
// one, held by deliver before a line of it panicked (run). A panic earlier in switchOff, in the
// bound of the last report or in the fold, leaves the held report the sweep report it was, and
// that panic ends the trimmer under R1's stated rule: the last report is not tried again, and no
// report older than it is sent in its place, under a line that says the last report is held. The
// case holds the first sweep's report, makes the bound panic on the switched-off report alone, and
// makes three readings in a row panic; the connection is up from the third. The negative control
// is a copy of run that takes any held report for the last one, under which the sweep report is
// sent at the next try of the wait as the switched-off trimmer's last report (INFRA-2009, tester's
// round 42: no case failed with the switched-off test taken out).
func TestASwitchOffWhoseBoundPanicsSendsNoOlderReportInItsPlace(t *testing.T) {
	const fell = "the bound fell over on the last report"
	f := newTrimFixture(t, 10) // forty entries, four points: the first sweep leaves 86% and sets no pause
	f.useVolume(f.modelled(90))
	f.setSettings(trimSettings())
	var up atomic.Bool
	previous := boundReport
	boundReport = func(trim *trimmer, report shimwire.LauncherVolumeReport) shimwire.LauncherVolumeReport {
		if report.SwitchedOff {
			panic(fell)
		}
		return previous(trim, report)
	}
	t.Cleanup(func() { boundReport = previous })
	tr := f.trimmer()
	tr.send = func(report shimwire.LauncherVolumeReport) error {
		if !up.Load() {
			return errors.New("no daemon connection is up to carry it")
		}
		return f.send(report)
	}
	_, done := f.start(tr)
	f.tick("holding it for the next attempt")

	var panics atomic.Int32
	f.setOnRead(func(int) {
		if panics.Add(1) == volumeFaultLimit {
			up.Store(true)
		}
		panic("the volume reader fell over")
	})
	for range volumeFaultLimit {
		f.tick("recovered a panic")
	}
	// The trimmer ends, or, where run took the held report for the last one, waits to try it: a
	// try is offered so that such a copy sends it.
	deadline := time.Now().Add(testwait.Bounded(t, 10*time.Second, time.Second))
	for ended := false; !ended && time.Now().Before(deadline); {
		select {
		case <-done:
			ended = true
		default:
			if at, waiting := f.ticks.live(volumeSettingsWait); waiting {
				f.ticks.offer(t, at, 200*time.Millisecond)
			}
			time.Sleep(testwait.Poll)
		}
	}
	if sent := f.reports(); len(sent) != 0 {
		t.Errorf("the trimmer sent %d reports after its last report's bound panicked, the first with the stop reason %q, want none: the sweep report it held is no last report", len(sent), sent[0].StopReason)
	}
	if f.logged("the last report of the switched-off trimmer panicked") != 1 || f.logged("the last report of the switched-off trimmer is held") != 0 {
		t.Errorf("the trimmer wrote %q, want the last report's panic written once and no line saying a last report is held", f.linesWritten())
	}
	select {
	case <-done:
	default:
		t.Error("the trimmer's loop still runs after its last report's bound panicked, want it ended")
	}
}
