//go:build linux

package launcher

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The pod's cache trimmer as the launcher runs it: which connection decides whether it runs,
// what it runs by and where its reports go, and when Run may return (launcher.go's
// serveVolumeTrim). What the trimmer does with the settings once it has them is
// volumetrim_test.go's; every case here drives the real launcher of an issue pod — six roles,
// six connections, six answers — against the package's own fake daemon rig, and reads only what
// the wiring decides.
//
// The file builds only under Linux, as volumetrim_test.go does, because the machine the trimmer
// reads here is that file's fixture: its volume tree, its /proc, its clock and its ticker, put
// into the trimmer the launcher built rather than into one the case built itself.

// podRoles is an issue pod's six roles in the order its --role list carries them. The first is
// the architect, whose connection the pod's one trimmer is bound to: that role exists for the
// life of the pod, and the architect is who the daemon tells about a volume filling up.
var podRoles = []string{"architect", "planner", "implementer", "tester", "reviewer", "merger"}

// otherTrimSettings is the answer every role but the first is given. Its high mark is above any
// volume these cases model, so a trimmer reading one of the other five answers would sweep
// nothing at all; its interval is one no case ever ticks, so a trimmer waiting on it would take
// no tick; and its low mark and minimum age are its own, so any line it wrote would name them.
func otherTrimSettings() *shimwire.LauncherVolumeTrim {
	return &shimwire.LauncherVolumeTrim{HighPercent: 99, LowPercent: 95, MinAgeMinutes: 1, IntervalSeconds: 7}
}

// laterTrimSettings is the answer a daemon gives the bound connection's second hello: the same
// marks as trimSettings with a low mark the sweep in flight has already passed and an interval
// of its own, so a case reads off which sweep they reached.
func laterTrimSettings() *shimwire.LauncherVolumeTrim {
	return &shimwire.LauncherVolumeTrim{HighPercent: 85, LowPercent: 80, MinAgeMinutes: 30, IntervalSeconds: 45}
}

// launchedTrimmer is the pod's trimmer as the launcher ran it: the trimmer Run built, with the
// case's own machine put into it, and the channel that closes when the loop Run waits on has
// returned — which is what a case asserts on, rather than Run having returned.
type launchedTrimmer struct {
	t       *testing.T
	started chan *trimmer
	stopped chan struct{}
}

// useLauncherTrimmer puts f's machine into whatever trimmer the launcher builds, and holds the
// loop it runs. It is called before the launcher starts, and so before the rig that stops it,
// because the cleanups run in reverse: the launcher is stopped and waited for while this seam is
// still in place.
//
// prepare, where a case gives one, is run on that trimmer before its loop starts: it is the only
// place a seam of the trimmer the launcher built can be set, since the case first sees that
// trimmer through await, by which time its own goroutine is already reading these fields.
func useLauncherTrimmer(t *testing.T, f *trimFixture, prepare ...func(trim *trimmer)) *launchedTrimmer {
	t.Helper()
	l := &launchedTrimmer{t: t, started: make(chan *trimmer, 1), stopped: make(chan struct{})}
	previous := podTrimmer
	podTrimmer = func(ctx context.Context, trim *trimmer) {
		trim.volumeRoot, trim.procRoot, trim.self, trim.roots = f.root, f.proc, f.self, f.roots
		trim.now, trim.ticks = f.clock.now, f.ticks.make
		// The sweeps of these cases delete in this process, as the sweeps of a case that drives
		// the trimmer directly do and for the same reason (trimFixture.trimmer): what they are
		// about is the trimmer's life in the launcher process, and the deleter the pod runs is
		// a child of `legion`, which a test binary is not.
		trim.rolesDir, trim.removals = f.rolesDir, inProcessRemovals
		// The pod's own line is kept and the case reads a copy: the lines a person greps for in
		// the container's log are the launcher's (manager.logf), and the fixture's record is
		// what holds each interval to one at a time (trimFixture.tick).
		pod := trim.logf
		trim.logf = func(format string, args ...any) {
			pod(format, args...)
			f.log(format, args...)
		}
		for _, one := range prepare {
			one(trim)
		}
		defer close(l.stopped)
		l.started <- trim
		trim.run(ctx)
	}
	t.Cleanup(func() { podTrimmer = previous })
	return l
}

// await is the trimmer the launcher started, once it has.
func (l *launchedTrimmer) await() *trimmer {
	l.t.Helper()
	select {
	case trim := <-l.started:
		return trim
	case <-time.After(testwait.Bounded(l.t, 30*time.Second, time.Second)):
		l.t.Fatal("the launcher started no trimmer")
		return nil
	}
}

// never fails unless the launcher has started no trimmer, waiting long enough for one it had
// started to have run. why names the reason none is owed.
func (l *launchedTrimmer) never(why string) {
	l.t.Helper()
	select {
	case trim := <-l.started:
		l.t.Fatalf("the launcher started trimmer %s, want none: %s", trim.id, why)
	case <-time.After(500 * time.Millisecond):
	}
}

// hasStopped says whether the loop Run waits on has returned.
func (l *launchedTrimmer) hasStopped() bool {
	select {
	case <-l.stopped:
		return true
	default:
		return false
	}
}

// ackPod answers every role's hello: the bound connection's with set, which is what the pod's
// trimmer must run by, and every other role's with settings no case runs, so an answer the
// trimmer wrongly read would show in what it did.
func ackPod(pod *fleet, set *shimwire.LauncherVolumeTrim) {
	pod.t.Helper()
	pod.role(podRoles[0]).acknowledgeWith(shimwire.LauncherHelloAck{VolumeTrim: set})
	for _, role := range podRoles[1:] {
		pod.role(role).acknowledgeWith(shimwire.LauncherHelloAck{VolumeTrim: otherTrimSettings()})
	}
}

// offerTick delivers one interval tick on the clock the trimmer is waiting on and returns as
// soon as it is taken, where the fixture's own tick waits for the line that ends the interval: a
// case that blocks the sweep this tick starts cannot wait for its end.
func offerTick(t *testing.T, f *trimFixture) {
	t.Helper()
	offerTickOf(t, f, f.interval(), "an interval clock")
}

// offerSettingsTick delivers one tick of the clock a trimmer with no settings it can run asks
// for settings on (volumeSettingsWait, trimmer.awaitSettings), which is how a case drives the
// wait without waiting the period out.
func offerSettingsTick(t *testing.T, f *trimFixture) {
	t.Helper()
	offerTickOf(t, f, volumeSettingsWait, "the clock it asks for settings on")
}

func offerTickOf(t *testing.T, f *trimFixture, period time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(testwait.Bounded(t, 30*time.Second, time.Second))
	for time.Now().Before(deadline) {
		at, live := f.ticks.live(period)
		if !live {
			time.Sleep(testwait.Poll)
			continue
		}
		if f.ticks.offer(t, at, 200*time.Millisecond) {
			return
		}
	}
	t.Fatalf("the trimmer took no tick: it is not waiting on %s", what)
}

// awaitClosed waits for a channel the trimmer's own work closes, naming what it waited for.
func awaitClosed(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(testwait.Bounded(t, 30*time.Second, time.Second)):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// assertIntervals holds every clock the trimmer made to the periods want names, the size walk's
// own budget aside: a trimmer running by another connection's answer would be waiting on a
// period of that answer's.
func assertIntervals(t *testing.T, f *trimFixture, want ...time.Duration) {
	t.Helper()
	for at := range f.ticks.count() {
		period := f.ticks.periodOf(at)
		if period == volumeWalkBudget || slices.Contains(want, period) {
			continue
		}
		t.Errorf("the trimmer made an interval clock of %s, want one of %v: its settings came from a connection that is not the one it is bound to", period, want)
	}
}

// awaitVolumeReport is the trimmer's next report as the daemon reads it off the wire, passing
// over the frames the launcher sends of its own accord.
func (g *roleConn) awaitVolumeReport() shimwire.LauncherVolumeReport {
	g.t.Helper()
	for {
		if report, ok := g.next().(shimwire.LauncherVolumeReport); ok {
			return report
		}
	}
}

// refuseVolumeReport fails unless no report at all reaches the daemon on this connection within
// bound. Every other frame is read and passed over, and the bound passing with nothing decoded
// is what the case reads: a frame that never arrives cannot be waited for any other way.
func (g *roleConn) refuseVolumeReport(bound time.Duration) {
	g.t.Helper()
	if err := g.conn.SetReadDeadline(time.Now().Add(bound)); err != nil {
		g.t.Fatal(err)
	}
	defer func() { _ = g.conn.SetReadDeadline(time.Time{}) }()
	for {
		line, err := g.reader.ReadBytes('\n')
		if os.IsTimeout(err) {
			return
		}
		if err != nil {
			g.t.Fatalf("reading the connection that must carry no report: %v", err)
		}
		frame, err := shimwire.Decode(line)
		if err != nil {
			g.t.Fatal(err)
		}
		if report, ok := frame.(shimwire.LauncherVolumeReport); ok {
			g.t.Fatalf("the daemon was sent %#v, want no report at all", report)
		}
	}
}

// The pod's trimmer runs by the first role's answer alone: it starts when that connection's
// answer carries settings and not before, however many other roles have answered, and the five
// other answers — which arrive both before and after it — change nothing about the sweep it
// makes or the clock it waits on. One connection decides the settings and carries the reports,
// so a report can never meet a daemon that did not send the settings that produced it.
func TestThePodsTrimmerRunsByTheFirstRolesAnswerAlone(t *testing.T) {
	f := newTrimFixture(t, 10) // forty entries, four points: the sweep ends above the low mark and reports
	f.useVolume(f.modelled(90))
	f.setSettings(trimSettings())
	trims := useLauncherTrimmer(t, f)

	pod := newFleetDialed(t, 2*time.Second, podRoles...)
	for _, role := range podRoles[1:3] {
		pod.role(role).acknowledgeWith(shimwire.LauncherHelloAck{VolumeTrim: otherTrimSettings()})
	}
	trims.never("the answers so far are of roles the trimmer is not bound to")

	pod.role(podRoles[0]).acknowledgeWith(shimwire.LauncherHelloAck{VolumeTrim: trimSettings()})
	trim := trims.await()
	started := volumeTrimLog + "started: trimmer " + trim.id + ", high 85%, low 70%, minimum age 30 min, interval 30 s"
	testwait.Eventually(t, "the trimmer to start on the first role's settings", func() bool {
		return f.logged(started) == 1
	})

	// The last three answers arrive once it is running: a trimmer taking whichever answer came
	// last would be running by them from its next interval on.
	for _, role := range podRoles[3:] {
		pod.role(role).acknowledgeWith(shimwire.LauncherHelloAck{VolumeTrim: otherTrimSettings()})
	}

	f.tick("the sweep ended")
	switch {
	case f.logged("at or above the high mark 85%, sweeping to 70%") != 1:
		t.Errorf("the sweep did not run by the first role's marks; the trimmer wrote %q", f.linesWritten())
	case f.logged("new settings") != 0:
		t.Errorf("the trimmer took new settings from another role's answer; it wrote %q", f.linesWritten())
	case len(f.gone()) != len(f.entries):
		t.Errorf("the sweep deleted %d of the %d entries, want every one of them under the first role's marks", len(f.gone()), len(f.entries))
	}
	assertIntervals(t, f, settingsInterval(*trimSettings()))

	// The report goes to the connection the settings came from, and the daemon reads it as the
	// frame it is rather than as a struct this case built.
	report := pod.role(podRoles[0]).awaitVolumeReport()
	switch {
	case report.Trimmer != trim.id:
		t.Errorf("the report names trimmer %q, want the %q this launcher minted", report.Trimmer, trim.id)
	case report.UsedBytes != int64(percentBytes(86)):
		t.Errorf("the report carries %d used bytes, want the %d the sweep left", report.UsedBytes, percentBytes(86))
	case report.Episode != 1:
		t.Errorf("the report carries episode %d, want the first", report.Episode)
	}
	if err := report.Validate(); err != nil {
		t.Errorf("the daemon would refuse this report: %v", err)
	}
	f.assertProtected()
	f.assertPrefixed()
}

// Run returns only once the pod's trimmer has stopped, never while a sweep of it is still
// deleting: the trimmer watches the roles' own context and ends beside them, and the container's
// termination grace is what bounds the whole of it. A launcher that returned under a running
// sweep would leave the pod's volume part way through a deletion as the container went.
func TestRunReturnsOnlyOnceThePodsTrimmerHasStopped(t *testing.T) {
	f := newTrimFixture(t, 25)
	f.useVolume(f.modelled(95))
	f.setSettings(trimSettings())
	trims := useLauncherTrimmer(t, f)

	pod := newFleetDialed(t, 2*time.Second, podRoles...)
	ackPod(pod, trimSettings())
	trims.await()

	sweeping, release := make(chan struct{}), make(chan struct{})
	releaseSweep := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseSweep)
	blocked := false
	f.clock.setHook(func(_ *fakeClock) {
		if blocked || len(f.gone()) < 3 {
			return
		}
		blocked = true
		close(sweeping)
		<-release
	})
	offerTick(t, f)
	awaitClosed(t, sweeping, "the sweep to reach its third deletion")

	pod.cancel()
	select {
	case err := <-pod.result:
		pod.result <- err
		t.Fatalf("Run returned %v while the pod's trimmer was still deleting", err)
	case <-time.After(300 * time.Millisecond):
	}
	if trims.hasStopped() {
		t.Fatal("the trimmer's loop returned while its sweep was still blocked: the case proves nothing")
	}

	releaseSweep()
	if err := pod.stop(); !errors.Is(err, context.Canceled) {
		t.Errorf("Run = %v, want the container's own context ending it", err)
	}
	if !trims.hasStopped() {
		t.Fatal("Run returned with the pod's trimmer still running: the container would go with a sweep of it part way through")
	}
	// The stop was taken at an entry boundary, as the trimmer's own cases hold it to: what it
	// deleted stands and nothing is half removed.
	f.assertNothingHalfRemoved()
	f.assertProtected()
}

// No trimmer runs where none is owed: not when the bound connection's answer carries no
// settings, whatever the other five carry, and not in the single-role form the controller's pod
// uses, whose volume belongs to its sandbox rather than to an issue and whose report no
// architect would read.
func TestNoTrimmerRunsWithoutSettingsOrInTheControllersPod(t *testing.T) {
	t.Run("the bound connection's answer carries no settings", func(t *testing.T) {
		f := newTrimFixture(t, 5)
		f.useVolume(f.modelled(95))
		f.setSettings(trimSettings())
		trims := useLauncherTrimmer(t, f)

		pod := newFleetDialed(t, 2*time.Second, podRoles...)
		pod.role(podRoles[0]).acknowledgeWith(shimwire.LauncherHelloAck{})
		for _, role := range podRoles[1:] {
			pod.role(role).acknowledgeWith(shimwire.LauncherHelloAck{VolumeTrim: trimSettings()})
		}

		trims.await()
		testwait.Eventually(t, "the trimmer to stay off", func() bool {
			return f.logged("no settings are in force, so the trimmer stays off") == 1
		})
		// The trimmer waits rather than ending with the pod, and waits on the one clock it
		// polls the settings with: an interval ticker here would be a trimmer sweeping a volume
		// the daemon asked it to leave alone.
		if trims.hasStopped() {
			t.Error("the trimmer's loop returned when the answer carried no settings, want it waiting for settings to come back")
		}
		for at := range f.ticks.count() {
			if period := f.ticks.periodOf(at); period != volumeSettingsWait {
				t.Errorf("the trimmer made a clock of %s, want none but the %s it waits for settings on", period, volumeSettingsWait)
			}
		}
		if reads := f.readCount(); reads != 0 {
			t.Errorf("the trimmer read the volume %d times where no settings are in force, want none", reads)
		}
		if gone := len(f.gone()); gone != 0 {
			t.Errorf("%d entries were deleted by a trimmer that was never to run", gone)
		}
	})

	t.Run("the controller's pod, whose launcher serves one role", func(t *testing.T) {
		f := newTrimFixture(t, 5)
		f.useVolume(f.modelled(95))
		f.setSettings(trimSettings())
		trims := useLauncherTrimmer(t, f)

		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		stderr := &lockedBuffer{}
		go func() {
			result <- Run(ctx, Config{
				Connect: "tcp://" + listener.Addr().String(), Token: "launcher-token", Sandbox: "legion-legion-legion-208",
				Role: podRoles[0], PodUID: "pod-1", PrivateDir: t.TempDir(), Stderr: stderr, StopGrace: 2 * time.Second,
			})
		}()
		t.Cleanup(func() {
			cancel()
			select {
			case <-result:
			case <-time.After(testwait.Bounded(t, 10*time.Second, time.Second)):
				t.Errorf("the launcher did not leave after its context ended; its log: %s", stderr.String())
			}
		})
		// The answer carries settings a trimmer would run by, so it is the form of the launcher
		// and nothing else that keeps this pod from having one.
		acceptRole(t, listener).acknowledgeWith(shimwire.LauncherHelloAck{VolumeTrim: trimSettings()})
		trims.never("the controller's pod runs one role, and its volume is its sandbox's")
	})
}

// A later answer on the bound connection replaces the settings in force, and reaches the next
// sweep rather than one in flight: the sweep running when it arrives finishes by the marks it
// started under, and the interval after it is the new settings' own. A daemon that restarts
// mid-sweep must not move the mark the sweep is working towards under it.
func TestALaterAnswerAppliesToTheNextSweepAndNotOneInFlight(t *testing.T) {
	f := newTrimFixture(t, 50) // two hundred entries, twenty points: the low mark 70% is reachable from 90%
	f.useVolume(f.modelled(90))
	f.setSettings(trimSettings())
	trims := useLauncherTrimmer(t, f)

	pod := newFleetDialed(t, 2*time.Second, podRoles...)
	ackPod(pod, trimSettings())
	trims.await()

	sweeping, release := make(chan struct{}), make(chan struct{})
	releaseSweep := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseSweep)
	blocked := false
	f.clock.setHook(func(_ *fakeClock) {
		if blocked || len(f.gone()) < 3 {
			return
		}
		blocked = true
		close(sweeping)
		<-release
	})
	offerTick(t, f)
	awaitClosed(t, sweeping, "the sweep to reach its third deletion")

	// The daemon restarts under the running sweep and answers the new hello with a low mark the
	// sweep has already passed: a sweep taking it would stop at its next reading.
	_ = pod.role(podRoles[0]).conn.Close()
	again := acceptRole(t, pod.listener)
	again.acknowledgeWith(shimwire.LauncherHelloAck{VolumeTrim: laterTrimSettings()})
	pod.conns[podRoles[0]] = again
	releaseSweep()

	f.tick("nothing to sweep")
	switch {
	case f.logged("the volume is back at the low mark 70%") != 1:
		t.Errorf("the sweep in flight did not run to the low mark it started under; the trimmer wrote %q", f.linesWritten())
	case len(f.gone()) != len(f.entries):
		t.Errorf("the sweep deleted %d of the %d entries, want every one of them: the later answer reached a sweep in flight", len(f.gone()), len(f.entries))
	case f.logged("new settings: high 85%, low 80%, minimum age 30 min, interval 45 s") != 1:
		t.Errorf("the interval after the sweep did not take the later answer's settings; the trimmer wrote %q", f.linesWritten())
	case f.logged("below the high mark 85%") != 1:
		t.Errorf("the interval after the sweep did not read the volume under the settings in force; the trimmer wrote %q", f.linesWritten())
	}
	if _, live := f.ticks.live(settingsInterval(*laterTrimSettings())); !live {
		t.Error("the trimmer is on no clock of the later answer's interval: a shortened interval must not wait the old one out")
	}
	assertIntervals(t, f, settingsInterval(*trimSettings()), settingsInterval(*laterTrimSettings()))
	f.assertProtected()
}

// A connection that drops and redials leaves the settings in force: the trimmer sweeps through
// the outage, holds the report it cannot write, and sends it on the connection that comes back,
// where the daemon reads it as a launcher_volume_report frame. A daemon restart is not an
// operator turning the trimmer off, and a report the daemon never got is a volume nobody was
// told about (bundleRestore keeps its own confirmation the same way).
func TestADroppedConnectionLeavesTheSettingsInForceAndHoldsTheReport(t *testing.T) {
	f := newTrimFixture(t, 25) // a hundred entries, ten points: the sweep ends below the high mark and above the low one
	f.useVolume(f.modelled(90))
	f.setSettings(trimSettings())
	trims := useLauncherTrimmer(t, f)

	pod := newFleetDialed(t, 2*time.Second, podRoles...)
	ackPod(pod, trimSettings())
	trim := trims.await()

	// The daemon goes away. The redial, which this case accepts and leaves unanswered, is what
	// says the launcher has seen the drop: until an answer is applied there is no writer, and
	// the trimmer has nowhere to send a report.
	_ = pod.role(podRoles[0]).conn.Close()
	again := acceptRole(t, pod.listener)
	if again.hello.LauncherID != pod.role(podRoles[0]).hello.LauncherID {
		t.Errorf("the redial is launcher %s, want %s again", again.hello.LauncherID, pod.role(podRoles[0]).hello.LauncherID)
	}

	f.tick("the sweep ended")
	if gone := len(f.gone()); gone != len(f.entries) {
		t.Errorf("the sweep deleted %d of the %d entries through the outage, want every one of them: a drop leaves the settings in force", gone, len(f.entries))
	}
	testwait.Eventually(t, "the trimmer to hold the report the outage refused", func() bool {
		return f.logged("holding it for the next attempt") == 1
	})

	again.acknowledgeWith(shimwire.LauncherHelloAck{VolumeTrim: trimSettings()})
	pod.conns[podRoles[0]] = again
	f.tick("nothing to sweep")

	report := again.awaitVolumeReport()
	switch {
	case report.Trimmer != trim.id:
		t.Errorf("the report names trimmer %q, want the %q this launcher minted before any connection", report.Trimmer, trim.id)
	case report.FreedBytes != int64(percentBytes(90)-percentBytes(80)):
		t.Errorf("the report carries %d freed bytes, want the %d of the sweep that was held through the outage", report.FreedBytes, percentBytes(90)-percentBytes(80))
	case report.UsedBytes != int64(percentBytes(80)):
		t.Errorf("the report carries %d used bytes, want the %d the held sweep left", report.UsedBytes, percentBytes(80))
	}
	if err := report.Validate(); err != nil {
		t.Errorf("the daemon would refuse this report: %v", err)
	}
	f.assertProtected()
}

// An answer with no settings is a daemon rolled back to a release that does not know this work:
// it stops the trimmer for good. A sweep in flight ends at its next entry boundary with its
// deletions standing, no report is sent for it, and a report the trimmer was holding is
// discarded rather than sent to a daemon that would close the connection on it — once a second
// for every role of the pod.
func TestAnAnswerWithNoSettingsStopsTheSweepInFlightAndDiscardsTheHeldReport(t *testing.T) {
	f := newTrimFixture(t, 25) // a hundred entries: the first sweep ends on its budget and leaves the rest
	f.useVolume(func(call int) volumeUsage {
		if call == 1 {
			return usageAt(90) // the interval reading that starts the first sweep
		}
		// Every reading after it: still above the high mark, so the interval after the first
		// sweep starts a second one, and two points below the first, so the first sweep freed
		// enough not to engage the quiet intervals.
		return usageAt(88)
	})
	f.setSettings(trimSettings())
	trims := useLauncherTrimmer(t, f)

	pod := newFleetDialed(t, 2*time.Second, podRoles...)
	ackPod(pod, trimSettings())
	trim := trims.await()

	// The first sweep runs with the daemon away, so its report is held; the second is blocked
	// where it stands, with seven entries deleted.
	const deleted = 7
	sweeping, release := make(chan struct{}), make(chan struct{})
	releaseSweep := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseSweep)
	spent, blocked := false, false
	f.clock.setHook(func(clock *fakeClock) {
		switch {
		case blocked:
		case !spent && len(f.gone()) >= 3:
			spent = true
			clock.advance(volumeSweepBudget + time.Minute)
		case spent && len(f.gone()) >= deleted:
			blocked = true
			close(sweeping)
			<-release
		}
	})

	_ = pod.role(podRoles[0]).conn.Close()
	again := acceptRole(t, pod.listener)
	f.tick("the sweep ended")
	testwait.Eventually(t, "the trimmer to hold the report the outage refused", func() bool {
		return f.logged("holding it for the next attempt") == 1
	})

	offerTick(t, f)
	awaitClosed(t, sweeping, "the second sweep to reach its seventh deletion")
	again.acknowledgeWith(shimwire.LauncherHelloAck{})
	pod.conns[podRoles[0]] = again
	releaseSweep()

	testwait.Eventually(t, "the trimmer to stop sweeping on an answer that carries no settings", func() bool {
		return f.logged("the sweep ended at an entry boundary with its deletions standing, no report is sent, and a report it was holding is discarded") == 1
	})
	testwait.Eventually(t, "the trimmer to wait for settings it can run", func() bool {
		return f.logged("no settings are in force, so the trimmer stays off until the daemon sends settings it can run") == 1
	})
	if trims.hasStopped() {
		t.Error("the trimmer's loop returned on an answer that carried no settings, want it waiting for the answer that carries them again")
	}

	// The entry the sweep was removing when the answer arrived is never interrupted, so the
	// deletions that stand are the seven it had made and that one.
	if gone := len(f.gone()); gone != deleted+1 {
		t.Errorf("%d entries are gone, want the %d that stood when the settings stopped the sweep", gone, deleted+1)
	}
	if gone := len(f.gone()); gone >= len(f.entries) {
		t.Errorf("every one of the %d entries is gone: the sweep ran to its end rather than stopping at a boundary", len(f.entries))
	}
	if trim.held != nil {
		t.Errorf("the trimmer still holds %+v after being stopped, want it discarded", trim.held)
	}
	again.refuseVolumeReport(500 * time.Millisecond)
	f.assertNothingHalfRemoved()
	f.assertProtected()
}

// The window after the sweep is guarded as the sweep itself is. A sweep ends at an entry
// boundary with a report to send, and the report is then built: the fresh size walk behind it is
// bounded at a minute (volumeWalkBudget), and an acknowledgement that carries no settings can
// arrive inside that minute, on the very connection the report would go out on. Such a daemon is
// one rolled back below this contract: it has no case for a launcher_volume_report, and
// ServeLauncher's switch closes the connection on it, which costs the pod a redial for every
// role. The write takes the writer and the settings together (manager.sendVolumeReport), so no
// frame at all reaches that connection; the control is the same window with an answer that
// carries settings, where the report goes.
func TestAReportIsNeverWrittenOnAConnectionWhoseAnswerCarriesNoSettings(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  *shimwire.LauncherVolumeTrim
		sent bool
	}{
		{"the answer that arrives while the report is built carries no settings", nil, false},
		{"the ordinary path, where that answer carries settings", trimSettings(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTrimFixture(t, 10) // forty entries, four points: the sweep ends above the low mark and reports
			f.useVolume(f.modelled(90))
			f.setSettings(trimSettings())

			// The size walk the report is built from, blocked at its first directory: this is
			// the window between the sweep's last entry boundary, where the settings are read
			// (trimmer.halted), and the write.
			walking, release := make(chan struct{}), make(chan struct{})
			reached := sync.OnceFunc(func() { close(walking) })
			releaseWalk := sync.OnceFunc(func() { close(release) })
			t.Cleanup(releaseWalk)
			trims := useLauncherTrimmer(t, f, func(trim *trimmer) {
				measure := trim.measure
				trim.measure = func(ctx context.Context, dir *dirHandle, name string, at time.Time) measured {
					reached()
					<-release
					return measure(ctx, dir, name, at)
				}
			})

			pod := newFleetDialed(t, 2*time.Second, podRoles...)
			ackPod(pod, trimSettings())
			trim := trims.await()

			f.tick("the sweep ended")
			awaitClosed(t, walking, "the size walk the report is built from")

			// The daemon goes away with the report half made, and its answer to the redial is
			// what this case turns on. acknowledgeWith reads the state the launcher writes
			// after it has applied the answer, so the walk is released into a connection whose
			// settings are already in force or already gone.
			_ = pod.role(podRoles[0]).conn.Close()
			again := acceptRole(t, pod.listener)
			again.acknowledgeWith(shimwire.LauncherHelloAck{VolumeTrim: tc.set})
			pod.conns[podRoles[0]] = again
			releaseWalk()

			if !tc.sent {
				// The frame first, since it is the claim: a bound that passes with nothing
				// decoded is the only way a frame that never arrives can be waited for. The
				// line the trimmer wrote is read after it, so a send that merely came late
				// still fails here.
				again.refuseVolumeReport(500 * time.Millisecond)
				testwait.Eventually(t, "the trimmer to hold the report that connection refused", func() bool {
					return f.logged("carries no trimmer settings") == 1
				})
				f.assertProtected()
				return
			}
			report := again.awaitVolumeReport()
			switch {
			case report.Trimmer != trim.id:
				t.Errorf("the report names trimmer %q, want the %q this launcher minted", report.Trimmer, trim.id)
			case report.UsedBytes != int64(percentBytes(86)):
				t.Errorf("the report carries %d used bytes, want the %d the sweep left", report.UsedBytes, percentBytes(86))
			}
			if err := report.Validate(); err != nil {
				t.Errorf("the daemon would refuse this report: %v", err)
			}
			f.assertProtected()
		})
	}
}

// plantEntries puts count more entries into the pod's `go-build` root, aged as the fixture ages
// its own: a role filling its caches again while the trimmer has no settings to run by. The
// modelled volume counts them, so the reading rises with them as a real one would.
func plantEntries(t *testing.T, f *trimFixture, count int) {
	t.Helper()
	for _, root := range f.roots {
		if root.Entry != GoBuildTrees {
			continue
		}
		for at := range count {
			f.addTree(filepath.Join(root.Path, fmt.Sprintf("go-build%09d", 1_000_000+at)), root.Entry)
		}
		return
	}
	t.Fatal("the fixture has no go-build root to fill")
}

// A pod keeps its trimmer for the life of the pod: an answer that carries no settings stops the
// sweeping, and the next answer that carries settings starts it again, in the same launcher
// process and under the same trimmer identifier. That is `enabled: false` and the operator
// turning it back on, and a daemon rolled back to a release without this work and the
// roll-forward after it —
// neither of which replaces a pod, which is why the settings are no part of the pod shape record.
// A trimmer that ended with the first such answer would leave the volume of a pod that lives for
// days unguarded with nothing told to anybody.
func TestThePodsTrimmerSweepsAgainWhenTheSettingsComeBack(t *testing.T) {
	f := newTrimFixture(t, 25) // a hundred entries, ten points: the first sweep ends above the low mark
	f.useVolume(f.modelled(90))
	f.setSettings(trimSettings())
	trims := useLauncherTrimmer(t, f)

	pod := newFleetDialed(t, 2*time.Second, podRoles...)
	ackPod(pod, trimSettings())
	trim := trims.await()

	f.tick("the sweep ended")
	first := pod.role(podRoles[0]).awaitVolumeReport()
	if first.Episode != 1 {
		t.Fatalf("the first report carries episode %d, want the first", first.Episode)
	}

	// The daemon is rolled back to a release that does not know this work, and its answer to
	// the redial carries no settings.
	_ = pod.role(podRoles[0]).conn.Close()
	rolledBack := acceptRole(t, pod.listener)
	rolledBack.acknowledgeWith(shimwire.LauncherHelloAck{})
	pod.conns[podRoles[0]] = rolledBack
	f.tick("sweeps nothing more")
	testwait.Eventually(t, "the trimmer to wait for settings it can run", func() bool {
		return f.logged("no settings are in force, so the trimmer stays off until the daemon sends settings it can run") == 1
	})
	if trims.hasStopped() {
		t.Fatal("the trimmer's loop returned on an answer that carried no settings, want it waiting: the pod keeps its trimmer for the life of the pod")
	}

	// The roles fill the volume again while nothing is guarding it, and the roll-forward
	// answers the next redial with settings.
	plantEntries(t, f, 60) // six points on top of the eighty the first sweep left: above the high mark again
	_ = pod.role(podRoles[0]).conn.Close()
	rolledForward := acceptRole(t, pod.listener)
	rolledForward.acknowledgeWith(shimwire.LauncherHelloAck{VolumeTrim: trimSettings()})
	pod.conns[podRoles[0]] = rolledForward

	started := volumeTrimLog + "started: trimmer " + trim.id
	offerSettingsTick(t, f)
	testwait.Eventually(t, "the trimmer to start again on the settings that came back", func() bool {
		return f.logged(started) == 2
	})
	f.tick("the sweep ended")
	if gone := len(f.gone()); gone != len(f.entries) {
		t.Errorf("the sweep deleted %d of the %d entries on the volume, want every one of them: the trimmer must sweep again once the settings are back",
			gone, len(f.entries))
	}

	// Nothing of the trimmer's own counting line was reset by the wait: the same identifier and
	// the same episode, which is what the daemon's three-notice bound counts against.
	second := rolledForward.awaitVolumeReport()
	switch {
	case second.Trimmer != trim.id:
		t.Errorf("the report after the settings came back names trimmer %q, want the %q this launcher minted", second.Trimmer, trim.id)
	case second.Episode != first.Episode:
		t.Errorf("the report after the settings came back carries episode %d, want the %d the pod was counting: the wait resets nothing", second.Episode, first.Episode)
	}
	assertIntervals(t, f, settingsInterval(*trimSettings()), volumeSettingsWait)
	f.assertProtected()
}

// A trimmer the first answer gave nothing to run by — no settings at all, or settings outside
// the ranges it refuses — starts when an answer carrying settings it can run arrives. It sweeps
// nothing and reads nothing while it waits, which is what R2 asks of a value outside the ranges:
// the trimmer stays off rather than starting its ticker.
func TestATrimmerGivenNothingItCanRunStartsWhenSettingsArrive(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first *shimwire.LauncherVolumeTrim
	}{
		{"an answer that carries no settings", nil},
		{"an answer whose interval would panic time.NewTicker",
			&shimwire.LauncherVolumeTrim{HighPercent: 85, LowPercent: 70, MinAgeMinutes: 30, IntervalSeconds: 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTrimFixture(t, 25)
			f.useVolume(f.modelled(90))
			f.setSettings(trimSettings())
			trims := useLauncherTrimmer(t, f)

			pod := newFleetDialed(t, 2*time.Second, podRoles...)
			ackPod(pod, tc.first)
			trim := trims.await()
			testwait.Eventually(t, "the trimmer to stay off on the answer it cannot run", func() bool {
				return f.logged("stays off") > 0
			})
			switch {
			case trims.hasStopped():
				t.Fatal("the trimmer's loop returned, want it waiting for settings it can run")
			case f.readCount() != 0:
				t.Errorf("the trimmer read the volume %d times with nothing it can run by, want none", f.readCount())
			case len(f.gone()) != 0:
				t.Errorf("%d entries were deleted by a trimmer with nothing it can run by", len(f.gone()))
			}

			_ = pod.role(podRoles[0]).conn.Close()
			again := acceptRole(t, pod.listener)
			again.acknowledgeWith(shimwire.LauncherHelloAck{VolumeTrim: trimSettings()})
			pod.conns[podRoles[0]] = again
			offerSettingsTick(t, f)
			testwait.Eventually(t, "the trimmer to start on the settings it can run", func() bool {
				return f.logged(volumeTrimLog+"started: trimmer "+trim.id) == 1
			})

			f.tick("the sweep ended")
			if gone := len(f.gone()); gone != len(f.entries) {
				t.Errorf("the sweep deleted %d of the %d entries, want every one of them once the settings arrived", gone, len(f.entries))
			}
			if report := again.awaitVolumeReport(); report.Trimmer != trim.id {
				t.Errorf("the report names trimmer %q, want the %q this launcher minted before any answer", report.Trimmer, trim.id)
			}
			f.assertProtected()
		})
	}
}

// The faults are the one ending nothing comes back from: three recovered panics in a row stop
// the pod's trimmer for the life of the launcher process, and an answer carrying settings after
// that starts nothing. A trimmer that came back from them would recover a panic, sweep, recover
// it again and go on for ever in the process that is PID 1 of the roles container.
func TestThreeFaultsInARowEndThePodsTrimmerForTheLifeOfTheProcess(t *testing.T) {
	f := newTrimFixture(t, 5)
	f.setOnRead(func(call int) { panic(fmt.Sprintf("the volume reader fell over on call %d", call)) })
	f.useVolume(steady(50))
	f.setSettings(trimSettings())
	trims := useLauncherTrimmer(t, f)

	pod := newFleetDialed(t, 2*time.Second, podRoles...)
	ackPod(pod, trimSettings())
	trim := trims.await()

	for range volumeFaultLimit {
		f.tick("recovered a panic")
	}
	awaitClosed(t, trims.stopped, "the trimmer's loop to return for good")
	report := pod.role(podRoles[0]).awaitVolumeReport()
	switch {
	case !report.SwitchedOff:
		t.Errorf("the last report %+v does not say the trimmer switched itself off", report)
	case report.Trimmer != trim.id:
		t.Errorf("the last report names trimmer %q, want %q", report.Trimmer, trim.id)
	}

	// Settings that arrive afterwards start nothing: the launcher builds one trimmer per
	// process (serveVolumeTrim), and this one is off.
	_ = pod.role(podRoles[0]).conn.Close()
	again := acceptRole(t, pod.listener)
	again.acknowledgeWith(shimwire.LauncherHelloAck{VolumeTrim: laterTrimSettings()})
	pod.conns[podRoles[0]] = again
	again.refuseVolumeReport(500 * time.Millisecond)
	if started := f.logged(volumeTrimLog + "started: trimmer " + trim.id); started != 1 {
		t.Errorf("the trimmer started %d times, want the one start before the faults stopped it", started)
	}
	if gone := len(f.gone()); gone != 0 {
		t.Errorf("%d entries were deleted after the faults stopped the trimmer", gone)
	}

	// Run still returns only once that loop has returned, which it has.
	if err := pod.stop(); !errors.Is(err, context.Canceled) {
		t.Errorf("Run = %v, want the container's own context ending it", err)
	}
	if !trims.hasStopped() {
		t.Fatal("Run returned with the pod's trimmer still running")
	}
}

// A volume this process cannot read is said once per run of the interval loop, so it is said
// again when the settings go away and come back: `enabled: false` and the operator turning it
// back on, and the daemon rolled back and rolled forward of the case above, neither of which
// replaces the pod. The memory that holds the error to one line (trimmer.saidRead) is cleared
// by a reading that worked and by the loop's own entry, and by nothing else.
//
// Without that second clearing the pod's log carries the started: line of the run that came
// back and then nothing at all for as long as the fault lasts: no reading line, because the
// interval returns at the failed read, and no read-error line, because the memory still holds
// the text from before the settings went away. The roles container's log is the only place this
// work is visible to a person.
//
// The whole loop is driven and not tick alone (volumetrim_test.go's
// TestAVolumeTheTrimmerCannotReadIsSaidOnce), because the clearing is on the loop's entry,
// which is the one path an interval never takes.
func TestAVolumeTheTrimmerCannotReadIsSaidAgainWhenTheSettingsComeBack(t *testing.T) {
	f := newTrimFixture(t, 1)
	f.useVolume(steady(50)) // below the high mark: a reading that got through would sweep nothing
	f.setSettings(trimSettings())

	// The reader the case drives, over the fixture's own: the volume cannot be read for the
	// whole of this case, as the statfs behind a real fault answers the same way every minute
	// for as long as it lasts.
	working := readVolumeUsage
	readVolumeUsage = func(path string) (volumeUsage, error) {
		return volumeUsage{}, errors.New("launcher: statfs " + path + ": input/output error")
	}
	t.Cleanup(func() { readVolumeUsage = working })
	trims := useLauncherTrimmer(t, f)

	pod := newFleetDialed(t, 2*time.Second, podRoles...)
	ackPod(pod, trimSettings())
	trim := trims.await()

	const unreadable = "could not be read"
	started := volumeTrimLog + "started: trimmer " + trim.id
	testwait.Eventually(t, "the trimmer to start on the settings the pod was answered with", func() bool {
		return f.logged(started) == 1
	})
	f.tick(unreadable)
	if said := f.logged(unreadable); said != 1 {
		t.Fatalf("the trimmer wrote %d lines about a volume it cannot read in its first interval, want the one a standing state gets; it wrote %q",
			said, f.linesWritten())
	}

	// The settings go away and come back, in the pod and the launcher process the trimmer has
	// run in all along, with the volume no more readable than before.
	_ = pod.role(podRoles[0]).conn.Close()
	rolledBack := acceptRole(t, pod.listener)
	rolledBack.acknowledgeWith(shimwire.LauncherHelloAck{})
	pod.conns[podRoles[0]] = rolledBack
	f.tick("sweeps nothing more")
	testwait.Eventually(t, "the trimmer to wait for settings it can run", func() bool {
		return f.logged("no settings are in force, so the trimmer stays off until the daemon sends settings it can run") == 1
	})

	_ = pod.role(podRoles[0]).conn.Close()
	rolledForward := acceptRole(t, pod.listener)
	rolledForward.acknowledgeWith(shimwire.LauncherHelloAck{VolumeTrim: trimSettings()})
	pod.conns[podRoles[0]] = rolledForward
	offerSettingsTick(t, f)
	testwait.Eventually(t, "the trimmer to start again on the settings that came back", func() bool {
		return f.logged(started) == 2
	})

	// The interval that must say it again, and a second offer the loop takes only once that
	// interval has ended: an interval that writes no line is exactly what a trimmer gone silent
	// here would give, and the fixture's own tick waits for a line (trimFixture.tick).
	offerTick(t, f)
	offerTick(t, f)
	if said := f.logged(unreadable); said != 2 {
		t.Errorf("the trimmer wrote %d lines about the volume it still cannot read, want one for the run before the settings went away and one for the run after they came back; it wrote %q",
			said, f.linesWritten())
	}
	if swept := len(f.gone()); swept != 0 {
		t.Errorf("%d entries were deleted by a trimmer that never once read the volume", swept)
	}
	assertIntervals(t, f, settingsInterval(*trimSettings()), volumeSettingsWait)
	f.assertProtected()
	f.assertPrefixed()
}
