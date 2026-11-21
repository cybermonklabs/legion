package legion

import (
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/logtest"
)

// TestTheWholeReadLineNamesTheSequenceUnderDispatchsOwnHandler: each whole read is logged with its
// reason and the daemon.stateSeq it read (wholeReadLine), and cmd/dispatch logs through a text
// handler (main.go's slog.NewTextHandler), not the JSON handler the other tests here install. The
// line a person reads in Dispatch's log names the sequence as a number.
func TestTheWholeReadLineNamesTheSequenceUnderDispatchsOwnHandler(t *testing.T) {
	logs := &logtest.Buffer{}
	logtest.SetDefault(t, slog.NewTextHandler(logs, nil))
	daemon := newFakeDaemon(t, daemonState("INFRA", 15, planning("INFRA")))
	database, setting := configured(t, daemon.URL)
	if err := ReadWhole(t.Context(), database.Pool, setting, ReadReasonStart); err != nil {
		t.Fatalf("read INFRA's daemon whole: %v", err)
	}
	line := logs.Lines(wholeReadLine)
	if !strings.Contains(line, "stateSeq=15 ") {
		t.Errorf("the whole read's line under Dispatch's text handler is %q, want it to name stateSeq=15", strings.TrimSpace(line))
	}
}

// TestAMessageOfADaemonTheSettingNoLongerNamesLeavesTheNamedDaemonsEventsApplied: R3 reads a
// project whole when its Legion setting names another daemon, so that the observations follow
// the daemon the setting names from that read on. A message the daemon named before published
// while the setting still named it reaches Dispatch after the move, as one does that was in
// flight, held back by a take-over, or redelivered. That daemon's counter is its own, far above
// the new daemon's, whose database is new. The message is no event of the daemon the setting names
// and writes nothing that daemon did, and the events the named daemon publishes after it are
// applied, each one in turn, with no event skipped.
func TestAMessageOfADaemonTheSettingNoLongerNamesLeavesTheNamedDaemonsEventsApplied(t *testing.T) {
	rig := newIntakeRig(t)
	previous := newFakeDaemon(t, daemonState("INFRA", 2000000, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", previous.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")
	requireLastSeq(t, rig.database, "INFRA", 2000000)

	// The setting moves to a daemon on a database of its own, five events in.
	named := newFakeDaemon(t, daemonState("INFRA", 5, strings.Replace(planning("INFRA"), `"planning"`, `"implementing"`, 1)))
	if _, err := PutSettings(t.Context(), rig.database.Pool, "INFRA", named.URL, model.Actor{Kind: "user", ID: "alice@example.com"}); err != nil {
		t.Fatalf("point INFRA at the named daemon: %v", err)
	}
	rig.wake <- struct{}{}
	rig.awaitWholeReads(2)
	requireLastSeq(t, rig.database, "INFRA", 5)

	// A message of the daemon the setting no longer names, under that daemon's run.
	rig.publishOf("INFRA", "0b6f2d1e-7c4a-4e93-a8d5-3f1b9e6c2a70", time.Now().UTC().Truncate(time.Microsecond),
		change(2000001, "INFRA-1", moved("phase", "merging")))
	rig.settled()
	// The named daemon's next events.
	rig.publish("INFRA", change(6, "INFRA-1", moved("phase", "testing")))
	rig.publish("INFRA", change(7, "INFRA-1", moved("phase", "reviewing")))
	rig.settled()

	if phase := currentValues(t, rig.database, "INFRA")["INFRA-1/phase"]; phase != `"reviewing"` {
		t.Errorf("INFRA-1's phase is %s, want the named daemon's newest event's reviewing: its events after the other daemon's message were skipped", phase)
	}
	for _, value := range history(t, rig.database, "INFRA", "INFRA-1", FieldPhase) {
		if value == `"merging"` {
			t.Errorf("INFRA-1's phase history %v holds merging, which only the daemon the setting no longer names wrote", history(t, rig.database, "INFRA", "INFRA-1", FieldPhase))
			break
		}
	}
	requireLastSeq(t, rig.database, "INFRA", 7)
}

// TestAWholeReadOfADaemonThatRunsNoWorkflowNamesThatReason: R2's last sentence. A daemon with no
// dispatch_url runs no outbox, so it never commits a state row and serves daemon.stateSeq 0, as
// the daemon's state route does for a project with no state row (records' StateSeq). Dispatch
// records each whole read of such a daemon as a failed read that names that reason: its settings
// carry no dispatch_url, so it runs no workflow.
func TestAWholeReadOfADaemonThatRunsNoWorkflowNamesThatReason(t *testing.T) {
	rig := newIntakeRig(t)
	noWorkflow := strings.Replace(daemonState("INFRA", 0, planning("INFRA")), `"dispatch_url":"http://dispatch.internal.example",`, ``, 1)
	daemon := newFakeDaemon(t, noWorkflow)
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitWholeReads(1)

	reads := readRecords(t, rig.database, "INFRA")
	requireWholeRead(t, reads[0], ReadReasonStart, false)
	if reads[0].error == nil || !strings.Contains(*reads[0].error, ErrNoDispatchURL.Error()) {
		t.Errorf("the whole read of a daemon that runs no workflow recorded the error %v, want it to name that reason: %q", orNone(reads[0].error), ErrNoDispatchURL.Error())
	}
}

// TestAFailedWholeReadSetsNoRun: only a good whole read takes the run that served it as the
// cursor's run (recordWhole). A read that fails, here one answered with another project's state
// served by another run, keeps the run of the cursor's last good read: a cursor that took the
// failed read's run would take a later boot of that run, at or below the cursor, for one it already
// holds, and read nothing where a daemon whose counter went back needs a whole read.
func TestAFailedWholeReadSetsNoRun(t *testing.T) {
	const otherRun = "9e3c5a71-2f4d-4b86-a0c9-6d1e8b7f3a52"
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")

	// A gap whose read the daemon refuses: the read is owed under the rig's run, and the message
	// that called for it goes back to the stream until the read is due.
	daemon.refuses(http.StatusServiceUnavailable)
	rig.publish("INFRA", change(20, "INFRA-1", moved("phase", "implementing")))
	rig.settledHolding(1)
	if c := cursorOf(t, rig.database, "INFRA"); !c.readOwed || c.run == nil || *c.run != rigRun {
		t.Fatalf("after the refused gap read the cursor is %+v under the run %s, want a read owed under %s", c, orNone(c.run), rigRun)
	}

	// The owed read, tried again a minute later, is answered with another project's state, which
	// another run serves: a failed read.
	other := strings.Replace(daemonState("INFRA", 25, planning("INFRA")), rigRun, otherRun, 1)
	other = strings.Replace(other, `"daemon":{"project":"INFRA"`, `"daemon":{"project":"LINUX"`, 1)
	daemon.serves(other)
	rig.clock.advance(ReadRetryAfter + time.Second)
	rig.intake.SettingsChanged(t.Context())
	rig.awaitWholeReads(3)

	reads := readRecords(t, rig.database, "INFRA")
	requireWholeRead(t, reads[len(reads)-1], ReadReasonRetry, false)
	if c := cursorOf(t, rig.database, "INFRA"); c.run == nil || *c.run != rigRun || !c.readOwed {
		t.Errorf("after a failed read served by %s the cursor follows the run %s with a read owed %v, want %s, its last good read's, and the read still owed",
			otherRun, orNone(c.run), c.readOwed, rigRun)
	}
}
