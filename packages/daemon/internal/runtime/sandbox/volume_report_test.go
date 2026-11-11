package sandbox

// What the launcher's cache trimmer reports of its pod's volume, and what the daemon does with
// the frame: it serves it, where a frame type the switch does not name closes the connection, and
// it hands one report per sweep to the seam the daemon's fact path hangs off
// (Options.VolumeReported, runtime.VolumeReported).

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// volumeSeam is the daemon's VolumeReported as a test holds it: every report it was given, in
// order, and the error it answers with.
type volumeSeam struct {
	reports chan volumeReport
	err     error
}

type volumeReport struct {
	issue  string
	podUID string
	report shimwire.LauncherVolumeReport
}

func newVolumeSeam() *volumeSeam { return &volumeSeam{reports: make(chan volumeReport, 8)} }

func (v *volumeSeam) reported(_ context.Context, issue, podUID string, report shimwire.LauncherVolumeReport) error {
	v.reports <- volumeReport{issue: issue, podUID: podUID, report: report}
	return v.err
}

// took is the next report the seam was given, or a failure naming what the test waited for.
func (v *volumeSeam) took(t *testing.T, what string) volumeReport {
	t.Helper()
	select {
	case report := <-v.reports:
		return report
	case <-time.After(5 * time.Second):
		t.Fatalf("the seam was given no report while the test waited for %s", what)
		return volumeReport{}
	}
}

// volumeLaunchers is a launchers whose volume seam is the test's, with the saved-bundle pair no
// test of the launcher protocol drives left nil. It is the one constructor these tests build a
// launchers with: testLaunchers (rig_test.go) is this one with no seam behind it.
func volumeLaunchers(seam runtime.VolumeReported) *launchers {
	return newLaunchers(nil, nil, seam, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// The frame the daemon had no case for closed the connection: ServeLauncher's switch ends in
// `default: return`, and the launcher of a pod whose trimmer reports redials once a second, for
// every role of that pod, for as long as the volume is filling. So the report is served like any
// other frame — the session is still connected after it, and answers the frame that follows —
// and what it carries reaches the daemon's seam.
//
// The negative control is the code at the merge base, where this switch has no
// launcher_volume_report case: the report falls to the default, ServeLauncher returns, its
// deferred close removes the session, and the state read below finds nothing connected.
func TestTheLauncherServesAVolumeReportAndHandsItToTheSeam(t *testing.T) {
	seam := newVolumeSeam()
	launchers := volumeLaunchers(seam.reported)
	session := launchers.accept(workerToken, testTree, rigNow, shimwire.LauncherHello{PodUID: "pod-uid", LauncherID: "launcher"})
	server, peer := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = peer.Close() })
	go session.ServeLauncher(server, bufio.NewReader(server), shimwire.NewWriter(server))
	writer := shimwire.NewWriter(peer)

	measured := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	sweep := shimwire.LauncherVolumeReport{
		Trimmer: "trim-1", Episode: 1, RaisedBy: shimwire.VolumeEpisodeFirst,
		UsedBytes: 48 << 30, FreeBytes: 2 << 30, FreedBytes: 1 << 30,
		StopReason:   "nothing left older than the minimum age",
		RefusedReads: []string{"pid 42"},
		Sizes: []shimwire.LauncherVolumeSize{
			{Path: "caches/uv", Bytes: 12 << 30, BytesOverHour: 11 << 30, BytesOverDay: 9 << 30},
		},
		SizesAt: measured,
	}
	if err := writer.WriteFrame(sweep); err != nil {
		t.Fatalf("write the trimmer's report: %v", err)
	}

	got := seam.took(t, "the trimmer's first report")
	if got.issue != testTree || got.podUID != "pod-uid" {
		t.Fatalf("the seam was given issue %q pod %q, want %q and the pod the launcher authenticated for", got.issue, got.podUID, testTree)
	}
	if got.report.Trimmer != "trim-1" || got.report.Episode != 1 || got.report.UsedBytes != 48<<30 ||
		got.report.FreeBytes != 2<<30 || got.report.FreedBytes != 1<<30 ||
		got.report.StopReason != sweep.StopReason || len(got.report.RefusedReads) != 1 ||
		len(got.report.Sizes) != 1 || got.report.Sizes[0] != sweep.Sizes[0] || !got.report.SizesAt.Equal(measured) {
		t.Fatalf("the seam was given %+v, want the sweep the launcher reported", got.report)
	}

	// The connection is still the session's: a frame after the report is read, and the session
	// still answers as connected. A `default: return` would have closed both.
	if err := writer.WriteFrame(shimwire.LauncherState{Child: &shimwire.LauncherChild{Generation: 7}}); err != nil {
		t.Fatalf("write the state frame after the report: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		state, connected := launchers.state(workerToken, "pod-uid")
		if connected && state.Child != nil && state.Child.Generation == 7 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the volume report the launcher is connected=%t state=%+v, want the connection still serving", connected, state)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// One report per sweep, each handed over on its own and in the order the trimmer made them: the
// daemon's notice bounds count episodes, so two reports that arrived out of order would be two
// counts against the wrong line. The frame loop is what gives the order — each report is handed
// over on it, as a restore report is — so this drives three sweeps down one connection.
func TestEverySweepsVolumeReportReachesTheSeamInOrder(t *testing.T) {
	seam := newVolumeSeam()
	launchers := volumeLaunchers(seam.reported)
	session := launchers.accept(workerToken, testTree, rigNow, shimwire.LauncherHello{PodUID: "pod-uid", LauncherID: "launcher"})
	server, peer := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = peer.Close() })
	go session.ServeLauncher(server, bufio.NewReader(server), shimwire.NewWriter(server))
	writer := shimwire.NewWriter(peer)

	for episode := 1; episode <= 3; episode++ {
		if err := writer.WriteFrame(shimwire.LauncherVolumeReport{Trimmer: "trim-1", Episode: episode,
			RaisedBy: shimwire.VolumeEpisodeFirst, UsedBytes: int64(episode) << 30}); err != nil {
			t.Fatalf("write sweep %d's report: %v", episode, err)
		}
	}
	for episode := 1; episode <= 3; episode++ {
		got := seam.took(t, "the report of sweep 3")
		if got.report.Episode != episode {
			t.Fatalf("the seam was given episode %d where sweep %d's report was next; the frame loop is what keeps them in order", got.report.Episode, episode)
		}
	}
}

// A store that does not answer is logged and the report is dropped there: the launcher cannot
// hold it again once the daemon has read it, and nothing acknowledges the frame. What must not
// happen is the connection going: the trimmer sweeps on and its next report carries the same
// volume, which is the whole reason a dropped one costs nothing.
func TestAVolumeReportTheDaemonCannotRecordLeavesTheConnection(t *testing.T) {
	seam := newVolumeSeam()
	seam.err = context.DeadlineExceeded
	launchers := volumeLaunchers(seam.reported)
	session := launchers.accept(workerToken, testTree, rigNow, shimwire.LauncherHello{PodUID: "pod-uid", LauncherID: "launcher"})
	server, peer := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = peer.Close() })
	go session.ServeLauncher(server, bufio.NewReader(server), shimwire.NewWriter(server))
	writer := shimwire.NewWriter(peer)

	if err := writer.WriteFrame(shimwire.LauncherVolumeReport{Trimmer: "trim-1", Episode: 1, RaisedBy: shimwire.VolumeEpisodeFirst}); err != nil {
		t.Fatalf("write the report the daemon cannot record: %v", err)
	}
	seam.took(t, "the report the daemon cannot record")
	if err := writer.WriteFrame(shimwire.LauncherVolumeReport{Trimmer: "trim-1", Episode: 2, RaisedBy: shimwire.VolumeEpisodeRise}); err != nil {
		t.Fatalf("write the next sweep's report: %v", err)
	}
	if got := seam.took(t, "the next sweep's report"); got.report.Episode != 2 {
		t.Fatalf("the seam was given episode %d after a failed record, want the next sweep's own report", got.report.Episode)
	}
}

// A runtime given no seam — a daemon with no workflow — reads the frame and drops it, and the
// connection stays open all the same.
func TestALauncherWithNoVolumeSeamStillServesTheReport(t *testing.T) {
	launchers := testLaunchers()
	session := launchers.accept(workerToken, testTree, rigNow, shimwire.LauncherHello{PodUID: "pod-uid", LauncherID: "launcher"})
	server, peer := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = peer.Close() })
	go session.ServeLauncher(server, bufio.NewReader(server), shimwire.NewWriter(server))
	writer := shimwire.NewWriter(peer)

	if err := writer.WriteFrame(shimwire.LauncherVolumeReport{Trimmer: "trim-1", Episode: 1, RaisedBy: shimwire.VolumeEpisodeFirst}); err != nil {
		t.Fatalf("write the report to a runtime with no seam: %v", err)
	}
	if err := writer.WriteFrame(shimwire.LauncherState{Child: &shimwire.LauncherChild{Generation: 3}}); err != nil {
		t.Fatalf("write the state frame after the report: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		state, connected := launchers.state(workerToken, "pod-uid")
		if connected && state.Child != nil && state.Child.Generation == 3 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the launcher with no volume seam is connected=%t state=%+v after a report, want the connection still serving", connected, state)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
