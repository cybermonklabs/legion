package replay

import (
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// processEnds is the lines supervise writes, in its order, as key's role's claim's process ends at
// at in its turn, by how the turn ends: its death (died, then what the death ended (recordGoneEnd),
// the task taken back from the turn (interrupted) and the death charged (chargeDeath)), its
// displacement through no fault of its own (displaced, then what it ended and the task taken
// back), or the let-go of a relaunch onto the current address (repoint, the task taken back, then
// the let-go's record of what it ended). The claim is then launched again a minute later.
func processEnds(b *windowBuilder, at time.Time, key string, role claim.Role, by TurnEnd) {
	b.t.Helper()
	token := b.token(key, role)
	delivery := fmt.Sprintf("outbox:%d", b.turns[token])
	incarnation := fmt.Sprintf("5e0c1a%02x", b.claims[token])
	delete(b.turns, token)
	ended := func(cause string) {
		b.line(at, slog.LevelWarn, workEndedLine, "claim", string(token), "cause", cause,
			"generation", b.generation(key), "turn", true, "taskTurn", true, "background", false, "kinds", []string{}, "reported", false,
			"delivery", delivery)
	}
	switch by {
	case TurnEndDied:
		b.line(at, slog.LevelWarn, processDiedLine, "claim", string(token), "incarnation", incarnation, "observed", "gone",
			"detail", "the worker's container exited", "podEnd", "", "chargedBefore", false)
		ended(`the process died: "gone"`)
		b.line(at, slog.LevelWarn, "supervise: the process died in the task's turn; the task waits for the relaunch", "claim", string(token),
			"delivery", delivery)
		b.line(at, slog.LevelWarn, "supervise: the agent died with work outstanding", "claim", string(token), "deaths", 1, "limit", 3,
			"pending", delivery+"-r1", "turnOpen", true)
	case TurnEndDisplaced:
		b.line(at, slog.LevelWarn, processDisplacedLine,
			"claim", string(token), "incarnation", incarnation, "process", "pod/"+string(token), "observed", "gone",
			"detail", "the daemon replaced the worker's pod")
		ended("its pod was replaced")
		b.line(at, slog.LevelWarn, "supervise: the process was displaced mid-turn, its pod replaced by the daemon or it another runtime's process this daemon takes over; the task waits for the relaunch",
			"claim", string(token), "delivery", delivery)
	case TurnEndLetGo:
		b.line(at, slog.LevelWarn, "supervise: the process is alive at a stale address; replacing it with one at the current address",
			"claim", string(token), "incarnation", incarnation, "detail", "the worker stream's address moved")
		b.line(at, slog.LevelWarn, "supervise: the process at a stale address is replaced mid-turn; the task waits for the relaunch",
			"claim", string(token), "delivery", delivery)
		ended("its address changed")
	default:
		b.t.Fatalf("no process end writes a turn's end %q", by)
	}
	b.launched(at.Add(time.Minute), key, role, true)
}

// A turn whose process ends under it writes no `supervise: a turn ended`: its end is the line of
// the process's end (ProcessEnded). Three implementers each run a turn from 09:00 that the end of
// their process ends — ACME-1's by its death at 09:30, ACME-2's by its displacement at 09:40,
// ACME-3's by the let-go of a relaunch onto the current address at 09:50 — and each is launched
// again a minute later and starts its next turn a minute after that. The trace holds each first
// turn ended at its process's end, by that end, and the next turn from its own start, and places
// every line. The trace reads the three kinds of line that end a turn and none of those the same
// decisions write beside them.
func TestATurnEndsWhereItsProcessEnds(t *testing.T) {
	t.Parallel()
	b := newWindowBuilder(t, clock(8, 0), clock(12, 0))
	b.boot(clock(8, 0))
	ends := []struct {
		key string
		at  time.Time
		by  TurnEnd
	}{
		{"ACME-1", clock(9, 30), TurnEndDied},
		{"ACME-2", clock(9, 40), TurnEndDisplaced},
		{"ACME-3", clock(9, 50), TurnEndLetGo},
	}
	for i, end := range ends {
		rootThrough(b, end.key, fmt.Sprintf("a%d", i+1), clock(8, 5+5*i), clock(8, 20+5*i),
			builtMove{clock(9, 0), phase.Implementing, workflow.TriggerPlannerCompleted})
		processEnds(b, end.at, end.key, claim.RoleImplementer, end.by)
		b.turnStarts(end.at.Add(2*time.Minute), end.key, claim.RoleImplementer)
	}
	trace := placedTrace(t, b.build())
	for _, end := range ends {
		var turns []Turn
		for _, turn := range trace.Issues[end.key].Turns {
			if turn.Claim.Role == claim.RoleImplementer {
				turns = append(turns, turn)
			}
		}
		if len(turns) != 2 || !turns[0].Start.Equal(clock(9, 0)) || !turns[0].End.Equal(end.at) || turns[0].EndedBy != end.by ||
			!turns[1].Start.Equal(end.at.Add(2*time.Minute)) || !turns[1].End.IsZero() || turns[1].EndedBy != TurnEndOpen {
			t.Errorf("%s's implementer turns are %s; want one from 09:00 ended at %s by %s, then one from %s open at the window's end",
				end.key, describeReading(turns), end.at.Format("15:04"), end.by, end.at.Add(2*time.Minute).Format("15:04"))
		}
	}

	for _, msg := range []string{processDiedLine, processDisplacedLine, workEndedLine} {
		if !TraceReads(msg) {
			t.Errorf("the trace does not read %q, which ends a turn", msg)
		}
	}
	for _, msg := range []string{
		"supervise: the process died in the task's turn; the task waits for the relaunch",
		"supervise: the agent died with work outstanding",
		"supervise: the process was displaced mid-turn, its pod replaced by the daemon or it another runtime's process this daemon takes over; the task waits for the relaunch",
		"supervise: the turn an earlier daemon saw start is over",
		"supervise: launch cut short; its context ended",
	} {
		if TraceReads(msg) {
			t.Errorf("the trace reads %q, which ends no turn of its own", msg)
		}
	}
}

func TestClaimCauseTraceDatesTheSeededDeathFromItsOwnLine(t *testing.T) {
	t.Parallel()
	from := clock(10, 0)
	b := newWindowBuilder(t, from, clock(12, 0))
	b.reaches(2 * time.Hour)
	token := b.token("ACME-1", claim.RoleImplementer)
	other := b.token("ACME-2", claim.RoleImplementer)
	for _, at := range []time.Time{clock(9, 0), clock(9, 15), from} {
		b.line(at, slog.LevelWarn, processDiedLine, "claim", string(token), "observed", "gone", "detail", "the process exited")
	}
	b.line(clock(9, 20), slog.LevelWarn, launchFailedLine, "claim", string(token), "error", "the launch was refused")
	b.line(clock(9, 25), slog.LevelWarn, processDiedLine, "claim", string(other), "observed", "gone")

	trace, err := BuildTrace(b.build())
	if err != nil {
		t.Fatal(err)
	}
	got := trace.claimCauses[token]
	if len(got) != 4 || got[2] != (claimCause{at: clock(9, 20), text: "the launch was refused", startFailure: true}) {
		t.Fatalf("claim causes = %+v, want three deaths and a dated start failure in order", got)
	}
	live := supervise.Claim{Token: token, State: supervise.StateWorking, LastReady: clock(9, 45)}
	trace.seedDeath(&live, from)
	if live.LastDeathCause != "gone: the process exited" || !live.LastDeathAt.Equal(clock(9, 15)) {
		t.Fatalf("seeded death = %q at %s, want the most recent death strictly before the start", live.LastDeathCause, live.LastDeathAt)
	}
	if live.LastStartFailure != "" || !live.LastStartFailureAt.IsZero() {
		t.Fatalf("a ready claim kept the start failure %q at %s", live.LastStartFailure, live.LastStartFailureAt)
	}
}
