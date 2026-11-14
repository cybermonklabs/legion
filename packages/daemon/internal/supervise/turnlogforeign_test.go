package supervise

import "testing"

// R6 of dispatch://INFRA-1132 binds both turn lines alike: each carries the claim, its generation
// and, where the turn is a task's, the task's number. A turn that is no task's names no task when
// it starts, and so it names none when it ends either, whatever the claim holds pending then.
//
// The path below is one a worker meets. A task is queued and its value written, and the worker
// has not taken it: the agent starts a turn of its own first — an Envoy notice, a person's steer —
// which is no task's, so the start line names no task. An end line that named the task still
// waiting for its take would give a reader pairing the two an end for a turn that never started,
// and a task read as run when it is not.
func TestTheTurnLogEndLineNamesNoDeliveryForATurnThatIsNoTasks(t *testing.T) {
	h := newHarness(t)
	h.manualTakes()
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	if p := h.pending(); p.taken() {
		t.Fatalf("the task reads taken: %+v; the turn below would be its own", p)
	}

	h.must(StreamTurnStart{Claim: testToken})
	h.wantState(StateWorking)
	wantTurnLine(t, h, startedLine, h.generation(), 0)

	h.mustQuiet(StreamTurnEnd{Claim: testToken})
	wantTurnLine(t, h, endedLine, h.generation(), 0)
}

// The pair holds for every turn, not for the untaken case alone: the end line names what its own
// start line named (Machine.turnsTask, startTurn, endTurn), never what the claim holds pending
// when the turn ends, because the claim can hold a task whose turn ran while a turn that is not
// that task's runs.
//
// The window is the daemon's own: a turn of the take's ends and no turn-outcome report has named
// its number yet, so the task is not finished, the claim goes idle holding it, and the end
// frame's bound waits (TimerFinish). A turn of the agent's own then — a background task
// finishing, an Envoy notice, a person's steer — is no task's: no take whose turn has yet to
// start stands, so its start line names no task. An end line read from the claim would name the
// task that is waiting for its report, and a reader pairing the two would read that task's turn
// as having run twice.
func TestTheTurnLogEndLineNamesWhatItsOwnStartLineNamed(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	number := h.pending().Number

	// The take's own turn, start to end, with no report behind it yet.
	h.must(StreamTurnStart{Claim: testToken})
	h.mustQuiet(StreamTurnEnd{Claim: testToken})

	p := h.claim().Pending
	if _, waits := h.m.timers[TimerFinish]; p == nil || !p.turnStarted() || !waits {
		t.Fatalf("the task is not waiting for its finish: pending %+v, state %s, finish bound armed %v", p, h.state(), waits)
	}

	// A turn of the agent's own in that window.
	h.must(StreamTurnStart{Claim: testToken})
	h.wantState(StateWorking)
	h.mustQuiet(StreamTurnEnd{Claim: testToken})

	starts, ends := h.records.of(startedLine), h.records.of(endedLine)
	if len(starts) != 2 || len(ends) != 2 {
		t.Fatalf("the log holds %d starts and %d ends, want two of each", len(starts), len(ends))
	}
	for i := range starts {
		start, started := starts[i].attr("task")
		end, ended := ends[i].attr("task")
		if started != ended || (started && start.Uint64() != end.Uint64()) {
			t.Fatalf("turn %d started naming task %v (present %v) and ended naming %v (present %v)",
				i+1, start, started, end, ended)
		}
	}
	if start, ok := starts[0].attr("task"); !ok || start.Uint64() != number {
		t.Fatalf("the take's own turn started naming task %v (present %v), want %d", start, ok, number)
	}
	if _, ok := starts[1].attr("task"); ok {
		t.Fatal("the agent's own turn started naming a task, want none")
	}
}
