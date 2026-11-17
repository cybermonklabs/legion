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
// counted in (faultRow), the refused /proc reads a sweep met before it panicked, and the rise a
// reported sweep's start raises though the sweep then panicked. The file builds only under Linux
// because its cases drive volumetrim_test.go's fixture, which does.

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
