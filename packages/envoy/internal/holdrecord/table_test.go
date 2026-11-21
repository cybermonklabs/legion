package holdrecord

import (
	"errors"
	"testing"
)

// admittedOps is a record in which command c of w has every entry it needs before its admission:
// hold set's intent and done, a done hold prove first for an epochal command, and each command it
// requires done.
func admittedOps(w Workflow, c string) []Operation {
	ops := []Operation{{Command: CommandSet, State: stateIntent}, {Command: CommandSet, State: stateDone}}
	if Epochal(w, c) {
		ops = append(ops, Operation{Command: CommandProve, State: stateIntent}, Operation{Command: CommandProve, State: stateDone})
	}
	for _, q := range Requires(w, c) {
		if q == CommandProve && Epochal(w, c) {
			continue
		}
		ops = append(ops, Operation{Command: q, State: stateIntent}, Operation{Command: q, State: stateDone})
	}
	return ops
}

// No command strands a hold. For every workflow and every command, once the command's intent is
// written and the command can never succeed, its own stop is admitted (it is Stoppable), and after
// that stop hold abort is admitted, but in hub-restore after a done hold prove, whose own rule
// excludes abort (a stopped census there is ended by another prove and census, or not at all); and
// while any other command's intent is pending, abort is refused ErrBusy. Fails if a command a check
// can refuse after its intent is not Stoppable: the count of stranded commands is not 0.
func TestEveryCommandHasAWayOut(t *testing.T) {
	stranded, checked := 0, 0
	for _, w := range current.Workflows() {
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
			live := Done{Workflow: w, Ops: append(append([]Operation{}, before.Ops...), Operation{Command: c, State: stateIntent})}
			if !Stoppable(w, c) && Decide(live, Request{Command: CommandRelease}) != nil && Decide(live, Request{Command: CommandAbort}) != nil {
				stranded++
				t.Errorf("%s's %q strands the hold: it is not Stoppable, and with its intent pending release and abort are refused", w, c)
				continue
			}
			stopped := Done{Workflow: w, Ops: append(append([]Operation{}, live.Ops...), Operation{Command: c, State: stateStop})}
			_, proved := Proven(stopped)
			err := Decide(stopped, Request{Command: CommandAbort})
			switch {
			case w == WorkflowHubRestore && proved:
				if !errors.Is(err, ErrExcluded) {
					t.Errorf("hub-restore's %q stopped after a done prove: abort answered %v, want hub-restore's own rule, ErrExcluded", c, err)
				}
			case err != nil:
				t.Errorf("%s's %q stopped: abort answered %v, want admitted", w, c, err)
			}
			for _, x := range Commands(w) {
				if x == c || x == CommandAbort || Decide(stopped, Request{Command: x}) != nil {
					continue
				}
				busy := Done{Workflow: w, Ops: append(append([]Operation{}, stopped.Ops...), Operation{Command: x, State: stateIntent})}
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
	set := []Operation{op(CommandSet, stateIntent), op(CommandSet, stateDone)}
	cutover := append(append([]Operation{}, set...),
		op("create-bucket", stateIntent), op("create-bucket", stateDone),
		op("convert", stateIntent), op("convert", stateDone),
		op("streams census", stateIntent), op("streams census", stateDone),
		op(CommandRelease, stateIntent), op(CommandRelease, stateDone))
	for _, complete := range []bool{true, false} {
		d := Done{Workflow: WorkflowCutover, Ops: cutover, Complete: complete}
		if err := Decide(d, Request{Command: "replay"}); !errors.Is(err, ErrAlreadyDone) {
			t.Errorf("replay after a complete cutover and a done release (complete %v) answered %v, want ErrAlreadyDone", complete, err)
		}
	}
	ended := 0
	for _, w := range current.Workflows() {
		for _, end := range []string{CommandRelease, CommandAbort} {
			ops := append(append([]Operation{}, set...), op(end, stateIntent), op(end, stateDone))
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
