package holdrecord

import (
	"errors"
	"strings"
	"testing"
)

// workflows is every workflow of the hold's table, in the design's order.
func workflows() []Workflow {
	return []Workflow{WorkflowCutover, WorkflowRollback, WorkflowSelectionRecovery, WorkflowBucketsCreate, WorkflowBucketsRecreate,
		WorkflowBucketsRestore, WorkflowStreamsUpdate, WorkflowHubRestore, WorkflowStreamsCensus}
}

// Assumption A2, as Decide stands: a command's own pending intent admits it again (its rerun after a
// crash), so a second process running the same command at once is admitted, while any other command
// is refused ErrBusy. The journal does not hold the operator to one command at a time; Admit's and
// CommandAbort's docs say so. Fails if Decide refuses a command its own intent: a rerun after a
// crash would then be refused for good.
func TestASecondProcessRunningTheSameCommandIsAdmitted(t *testing.T) {
	running := Done{Workflow: WorkflowCutover, Ops: []Operation{{Command: CommandSet, State: StateIntent},
		{Command: CommandSet, State: StateDone}, {Command: "replay", State: StateIntent}}}
	if err := Decide(running, Request{Command: "replay"}); err != nil {
		t.Errorf("replay with its own intent pending answered %v, want admitted", err)
	}
	if err := Decide(running, Request{Command: "create-bucket"}); !errors.Is(err, ErrBusy) {
		t.Errorf("create-bucket with replay's intent pending answered %v, want ErrBusy", err)
	}
}

// admittedOps is a record in which command c of w has every entry it needs before its admission:
// hold set's intent and done, a done hold prove first for an epochal command, and each command it
// requires done.
func admittedOps(w Workflow, c string) []Operation {
	ops := []Operation{{Command: CommandSet, State: StateIntent}, {Command: CommandSet, State: StateDone}}
	if Epochal(w, c) {
		ops = append(ops, Operation{Command: CommandProve, State: StateIntent}, Operation{Command: CommandProve, State: StateDone})
	}
	for _, q := range Requires(w, c) {
		if q == CommandProve && Epochal(w, c) {
			continue
		}
		ops = append(ops, Operation{Command: q, State: StateIntent}, Operation{Command: q, State: StateDone})
	}
	return ops
}

// No command strands a hold. For every workflow and every command, once the command's intent is
// written and the command can never succeed, its own stop is admitted (it is Stoppable), and after
// that stop hold abort is admitted, in hub-restore after a done hold prove too; and while any other
// command's intent is pending, abort is refused ErrBusy. Fails if a command a check can refuse after
// its intent is not Stoppable: the count of stranded commands is not 0.
func TestEveryCommandHasAWayOut(t *testing.T) {
	stranded, checked := 0, 0
	for _, w := range workflows() {
		for _, c := range Commands(w) {
			if c == CommandAbort {
				continue
			}
			before := Done{Workflow: w, Ops: admittedOps(w, c)}
			if err := Decide(before, Request{Command: c}); err != nil {
				t.Errorf("%s's %q is not admitted after the entries it needs: %v", w, c, err)
				continue
			}
			checked++
			live := Done{Workflow: w, Ops: append(append([]Operation{}, before.Ops...), Operation{Command: c, State: StateIntent})}
			if !Stoppable(w, c) && Decide(live, Request{Command: CommandRelease}) != nil && Decide(live, Request{Command: CommandAbort}) != nil {
				stranded++
				t.Errorf("%s's %q strands the hold: it is not Stoppable, and with its intent pending release and abort are refused", w, c)
				continue
			}
			stopped := Done{Workflow: w, Ops: append(append([]Operation{}, live.Ops...), Operation{Command: c, State: StateStop})}
			if err := Decide(stopped, Request{Command: CommandAbort}); err != nil {
				t.Errorf("%s's %q stopped: abort answered %v, want admitted", w, c, err)
			}
			for _, x := range Commands(w) {
				if x == c || x == CommandAbort || Decide(stopped, Request{Command: x}) != nil {
					continue
				}
				busy := Done{Workflow: w, Ops: append(append([]Operation{}, stopped.Ops...), Operation{Command: x, State: StateIntent})}
				if err := Decide(busy, Request{Command: CommandAbort}); !errors.Is(err, ErrBusy) {
					t.Errorf("%s's %q stopped and %q live: abort answered %v, want ErrBusy", w, c, x, err)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no command was checked")
	}
	t.Logf("%d commands checked, %d stranded", checked, stranded)
}

// No command is admitted once the hold has ended: a done hold release or hold abort ends the
// journal, and Decide refuses every command after it, so a process that read the row as held
// before another process ended the hold writes no intent: the journal's compare-and-swap refuses
// its write, and its second decision refuses the command. Fails if Decide reads a done release or
// abort as an entry like any other: replay after a complete cutover and a done release is admitted.
func TestNoCommandIsAdmittedAfterTheHoldEnded(t *testing.T) {
	op := func(c, s string) Operation { return Operation{Command: c, State: s} }
	set := []Operation{op(CommandSet, StateIntent), op(CommandSet, StateDone)}
	cutover := append(append([]Operation{}, set...),
		op("create-bucket", StateIntent), op("create-bucket", StateDone),
		op("convert", StateIntent), op("convert", StateDone),
		op("streams census", StateIntent), op("streams census", StateDone),
		op(CommandRelease, StateIntent), op(CommandRelease, StateDone))
	for _, complete := range []bool{true, false} {
		d := Done{Workflow: WorkflowCutover, Ops: cutover, Complete: complete}
		if err := Decide(d, Request{Command: "replay"}); !errors.Is(err, ErrAlreadyDone) {
			t.Errorf("replay after a complete cutover and a done release (complete %v) answered %v, want ErrAlreadyDone", complete, err)
		}
	}
	ended := 0
	for _, w := range workflows() {
		for _, end := range []string{CommandRelease, CommandAbort} {
			ops := append(append([]Operation{}, set...), op(end, StateIntent), op(end, StateDone))
			d := Done{Workflow: w, Ops: ops, Complete: end == CommandAbort}
			for _, c := range append([]string{CommandSet, CommandRelease}, Commands(w)...) {
				ended++
				if err := Decide(d, Request{Command: c}); !errors.Is(err, ErrAlreadyDone) {
					t.Errorf("%s after a done %s: %q answered %v, want ErrAlreadyDone", w, end, c, err)
				}
			}
		}
	}
	t.Logf("%d commands asked after a done release or abort", ended)
}

// A run cut after its intent shows in the journal as two intents of one command in a row, with no
// stop or done between: the first run ended with no entry, so what it left is unknown, and the
// rerun's stop cannot speak for it. Decide counts such a command as changed, so hold release is
// refused over a stop recording unchanged, and hold abort still ends the hold. Fails if unchanged
// reads intents as nothing: release is admitted.
func TestACutRunCountsAsAChange(t *testing.T) {
	op := func(c, s string) Operation { return Operation{Command: c, State: s} }
	for _, x := range []struct {
		w Workflow
		c string
	}{{WorkflowCutover, "replay"}, {WorkflowBucketsCreate, "buckets create"}, {WorkflowStreamsUpdate, "streams update"}} {
		stop := Operation{Command: x.c, State: StateStop, Outputs: map[string]string{stopLeftKey: LeftUnchanged}}
		cut := Done{Workflow: x.w, Ops: []Operation{op(CommandSet, StateIntent), op(CommandSet, StateDone),
			op(x.c, StateIntent), op(x.c, StateIntent), stop}}
		if err := Decide(cut, Request{Command: CommandRelease}); !errors.Is(err, ErrOutOfOrder) {
			t.Errorf("%s: release after a cut %q run and its rerun's unchanged stop answered %v, want ErrOutOfOrder", x.w, x.c, err)
		}
		if err := Decide(cut, Request{Command: CommandAbort}); err != nil {
			t.Errorf("%s: abort after the rerun's stop answered %v, want admitted", x.w, err)
		}
		whole := Done{Workflow: x.w, Ops: []Operation{op(CommandSet, StateIntent), op(CommandSet, StateDone), op(x.c, StateIntent), stop}}
		if err := Decide(whole, Request{Command: CommandRelease}); err != nil {
			t.Errorf("%s: release after one %q run's unchanged stop answered %v, want admitted", x.w, x.c, err)
		}
	}
}

// Decide refuses in the order its doc gives: each case below meets two refusals, or one that a
// later check would answer otherwise, and the first in the doc's order must be the answer. Fails if
// the code's order and the doc's part: with EndIsFinal's check after ErrBusy, the first case
// answers ErrBusy.
func TestDecideRefusesInItsDocumentedOrder(t *testing.T) {
	op := func(c, s string) Operation { return Operation{Command: c, State: s} }
	set := []Operation{op(CommandSet, StateIntent), op(CommandSet, StateDone)}
	with := func(ops ...Operation) []Operation { return append(append([]Operation{}, set...), ops...) }
	for _, x := range []struct {
		name    string
		d       Done
		command string
		want    error
		names   string
	}{
		{"the hold ended, though another command's intent is pending", Done{Workflow: WorkflowCutover,
			Ops: with(op("replay", StateIntent), op(CommandRelease, StateIntent), op(CommandRelease, StateDone))}, "create-bucket", ErrAlreadyDone, "the hold has ended"},
		{"busy, though release is out of order too", Done{Workflow: WorkflowCutover, Ops: with(op("create-bucket", StateIntent))},
			CommandRelease, ErrBusy, "create-bucket"},
		{"release before completion", Done{Workflow: WorkflowCutover, Ops: with(op("replay", StateIntent), op("replay", StateDone))},
			CommandRelease, ErrOutOfOrder, "not complete"},
		{"a second hold set", Done{Workflow: WorkflowCutover, Ops: with()}, CommandSet, ErrAlreadyDone, CommandSet},
		{"before hold set's done, though the command is not admitted either", Done{Workflow: WorkflowCutover,
			Ops: []Operation{op(CommandSet, StateIntent), op(CommandSet, StateStop)}}, "buckets create", ErrOutOfOrder, CommandSet},
		{"not admitted", Done{Workflow: WorkflowCutover, Ops: with()}, "buckets create", ErrNotAdmitted, "cutover"},
		{"after completion, though the command is done too", Done{Workflow: WorkflowBucketsCreate,
			Ops: with(op("buckets create", StateIntent), op("buckets create", StateDone)), Complete: true}, "buckets create", ErrAlreadyDone, "the record is complete"},
		{"excluded", Done{Workflow: WorkflowHubRestore, Ops: with(op(CommandProve, StateIntent), op(CommandProve, StateDone))},
			CommandAbort, ErrExcluded, CommandProve},
		{"abort with no command stopped", Done{Workflow: WorkflowCutover, Ops: with()}, CommandAbort, ErrOutOfOrder, "no command has stopped"},
		{"a required command with no done entry", Done{Workflow: WorkflowCutover, Ops: with()}, "streams census", ErrOutOfOrder, "convert"},
		{"already done", Done{Workflow: WorkflowCutover, Ops: with(op("create-bucket", StateIntent), op("create-bucket", StateDone))},
			"create-bucket", ErrAlreadyDone, "create-bucket"},
	} {
		if err := Decide(x.d, Request{Command: x.command}); !errors.Is(err, x.want) || !strings.Contains(err.Error(), x.names) {
			t.Errorf("%s: Decide(%q) answered %v, want %v naming %q", x.name, x.command, err, x.want, x.names)
		}
	}
}

// In hub-restore, after a done hold prove, hold abort is admitted once a command's latest entry
// after that prove is a stop, as in every other workflow, and hold release stays refused:
// hub-restore's completion includes its clean census. With no stop since the prove, a done prove
// still excludes abort, and another command's stop from before the latest prove counts for
// nothing. Fails if the exclusion stands after a stop: abort answers ErrExcluded; and if any stop
// lifts it: the census's stop from before a second prove admits abort.
func TestHubRestoreAbortFollowsAStopAfterAProve(t *testing.T) {
	op := func(c, s string) Operation { return Operation{Command: c, State: s} }
	proved := []Operation{op(CommandSet, StateIntent), op(CommandSet, StateDone), op(CommandProve, StateIntent), op(CommandProve, StateDone)}
	stopped := Done{Workflow: WorkflowHubRestore, Ops: append(append([]Operation{}, proved...), op("streams census", StateIntent),
		Operation{Command: "streams census", State: StateStop, Outputs: map[string]string{stopLeftKey: "UNKNOWN: a stream's info cannot be read"}})}
	if err := Decide(stopped, Request{Command: CommandAbort}); err != nil {
		t.Errorf("abort after a done prove and a stopped census answered %v, want admitted", err)
	}
	if err := Decide(stopped, Request{Command: CommandRelease}); !errors.Is(err, ErrOutOfOrder) {
		t.Errorf("release after a done prove and a stopped census answered %v, want ErrOutOfOrder", err)
	}
	if err := Decide(Done{Workflow: WorkflowHubRestore, Ops: proved}, Request{Command: CommandAbort}); !errors.Is(err, ErrExcluded) {
		t.Errorf("abort after a done prove and no stop answered %v, want ErrExcluded", err)
	}
	reproved := Done{Workflow: WorkflowHubRestore, Ops: append(append([]Operation{}, stopped.Ops...), op(CommandProve, StateIntent),
		op(CommandProve, StateDone))}
	if err := Decide(reproved, Request{Command: CommandAbort}); !errors.Is(err, ErrExcluded) {
		t.Errorf("abort after the census's stop and a later done prove answered %v, want ErrExcluded", err)
	}
}

// Rule (1) and rule (2) of the hold's design (V67r3-1), as the table admits them. An abort whose
// disk check found the done prove's disk gone writes a stop naming it; after that stop a new hold
// prove is admitted (restored onto another disk), and its done opens a new epoch that Proven reads;
// and abort is admitted again after its own stop, whether written before that new prove or after.
// Fails if abort's own stop does not lift a done prove's exclusion: the abort after the new prove
// answers ErrExcluded.
func TestHubRestoreAbortReturnsAfterItsOwnStop(t *testing.T) {
	op := func(c, s string) Operation { return Operation{Command: c, State: s} }
	gone := Operation{Command: CommandAbort, State: StateStop, Outputs: map[string]string{stopLeftKey: LeftUnchanged,
		"reason": "the done prove's disk of A is gone: disk-7 answers 404"}}
	d := Done{Workflow: WorkflowHubRestore, Ops: []Operation{op(CommandSet, StateIntent), op(CommandSet, StateDone),
		op(CommandProve, StateIntent), {Command: CommandProve, State: StateDone, Outputs: map[string]string{"disk_id.A": "7"}},
		op(CommandAbort, StateIntent), gone}}
	if err := Decide(d, Request{Command: CommandProve}); err != nil {
		t.Fatalf("a new prove after abort's gone-disk stop answered %v, want admitted", err)
	}
	d.Ops = append(d.Ops, op(CommandProve, StateIntent), Operation{Command: CommandProve, State: StateDone, Outputs: map[string]string{"disk_id.A": "9"}})
	if p, ok := Proven(d); !ok || p.DiskIDs["A"] != "9" {
		t.Fatalf("after the new prove Proven answers %v, %v; want the new epoch's disk 9", p.DiskIDs, ok)
	}
	if err := Decide(d, Request{Command: CommandAbort}); err != nil {
		t.Errorf("abort after its own stop and the new prove answered %v, want admitted", err)
	}
}
