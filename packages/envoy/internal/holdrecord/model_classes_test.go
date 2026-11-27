package holdrecord

import (
	"errors"
	"fmt"
	"strings"
)

// ---------------------------------------------------------------- invariants and classes

// pendingAtCompletion is the intents that may not be pending at completion: all but hold
// release's own lock, hold abort's after a done prove while the row is held, and hold set's on a
// row lost in prepare whose hold abort on the released row completed the record (PrepLost 'E'):
// hold set can no longer move that row from prepare.
func pendingAtCompletion(s modelState) modelCmds {
	var out modelCmds
	for c := range s.Intent.all() {
		if c == cmdRelease || (c == cmdAbort && s.Proved && s.Row == 'H') || (c == cmdSet && s.PrepLost == 'E') {
			continue
		}
		out = out.with(c)
	}
	return out
}

// modelViolation is one counterexample: the state's workflow, the invariant it breaks, its class,
// and the shortest trace to it.
type modelViolation struct {
	Workflow, Kind, Class string
	Trace                 []string
}

// journalWrite reports whether a trace step writes the journal or the row.
func journalWrite(label string) bool {
	for _, w := range []string{"intent written", ": done", "stop entry", ": PUT", "row released", "record inserted", "row moved to held"} {
		if strings.Contains(label, w) {
			return true
		}
	}
	return false
}

// raced reports whether a command was admitted by a read, and another write came before its
// intent's write: the two-write admission's race.
func raced(trace []string) bool {
	admittedAt := map[string]int{}
	for i, tl := range trace {
		if strings.HasPrefix(tl, "crash in ") { // a crash before the intent's write ends that admission
			delete(admittedAt, strings.TrimPrefix(tl, "crash in "))
			continue
		}
		if strings.HasSuffix(tl, ": admitted") {
			admittedAt[strings.TrimSuffix(tl, ": admitted")] = i
			continue
		}
		if strings.HasSuffix(tl, ": intent written") && !strings.HasSuffix(tl, "admitted, intent written") &&
			!strings.HasSuffix(tl, "prepare, intent written") {
			c := strings.TrimSuffix(tl, ": intent written")
			if a, ok := admittedAt[c]; ok {
				for _, between := range trace[a+1 : i] {
					if journalWrite(between) {
						return true
					}
				}
			}
		}
	}
	for c, a := range admittedAt { // admitted, and the trace ends before its intent, after another write
		written := false
		for _, tl := range trace[a+1:] {
			if tl == c+": intent written" {
				written = true
			}
		}
		if !written {
			for _, between := range trace[a+1:] {
				if journalWrite(between) {
					return true
				}
			}
		}
	}
	return false
}

func has(trace []string, label string) bool {
	for _, tl := range trace {
		if tl == label {
			return true
		}
	}
	return false
}

func hasPrefix(trace []string, p string) bool {
	for _, tl := range trace {
		if strings.HasPrefix(tl, p) {
			return true
		}
	}
	return false
}

func refusal(label string) bool {
	for _, w := range []string{"refus", "UNKNOWN", "check fails", "stops"} {
		if strings.Contains(label, w) {
			return true
		}
	}
	return false
}

// setRaced reports whether another command was admitted while hold set ran in held: v59's hold set
// holds no lock.
func setRaced(trace []string) bool {
	in := false
	for _, tl := range trace {
		switch {
		case tl == "hold set: row moved to held":
			in = true
		case strings.HasPrefix(tl, "hold set: steps 2 to 6") || tl == "crash in hold set" ||
			(strings.HasPrefix(tl, "hold set: ") && refusal(tl)):
			in = false
		case in && (strings.HasSuffix(tl, ": admitted") || strings.HasSuffix(tl, ": admitted, intent written")) &&
			!strings.HasPrefix(tl, cmdSnapRestore.String()):
			return true
		}
	}
	return false
}

func setRuns(s modelState) bool {
	for _, sl := range s.Slots {
		if sl.Cmd == cmdSet && runs(sl) {
			return true
		}
	}
	return false
}

// classifyR28 names the classes of round 28, which the earlier rules carry by construction, and ""
// for any other.
func classifyR28(r modelRules, sc modelScenario, kind string, s modelState, trace []string) string {
	switch {
	case kind == "a disk judged against a target the step-1 map lacks":
		return classR28H
	case !r.SetCommand && (setRaced(trace) || ((kind == "two commands past their intent at once" ||
		kind == "the row is released while another command runs") && setRuns(s))):
		return classR28ASet
	case kind != "liveness":
		return ""
	case sc.Hub && !r.Step1Whole && s.Rec && (s.Step1[0].ID == 0 || s.Step1[1].ID == 0):
		return classR28H // a target the map lacks: hold abort's disk check can never pass
	case !r.SetCommand && s.Intent.has(cmdSnapRestore):
		return classR28ASet
	case !r.ConvertStoppable && s.NoStart && s.Intent.has(cmdConvert):
		return classR28AConv
	case !r.PrepareMarksUnresolved && s.Unresolved && s.Intent.has(cmdRestorePrepare):
		return classR28APrep
	case !r.CompletionSurvivesRowRestore && s.Complete && s.HoldGen != s.RowGen:
		return classR28C
	case sc.Hub && !r.ExpectedMap && s.Proved:
		for tg := range targets {
			if s.ProvenSet&(1<<tg) == 0 && s.Cur[tg].ID != s.Step1[tg].ID {
				return classR28B
			}
		}
	}
	return ""
}

func classify(r modelRules, sc modelScenario, kind string, s modelState, trace []string) string {
	// A row lost in prepare and restored released left the hold's cohort, and its journal when
	// written, and no command ends them: the class ReleasedPrepareAbort and ReleasedNoJournalAbort
	// fix, whichever command failed before.
	if kind == "liveness" && (s.PrepLost == 'O' || s.PrepLost == 'N') {
		return classOrphan
	}
	// A command failed for good after its intent, and no command the table admits ends the hold:
	// the class WayOut and EveryCommandStoppable fix. Only the profiles that model the failure
	// (Fail) reach it.
	if kind == "liveness" && sc.Fail && s.Doomed != noCmd {
		return classWayOut
	}
	if kind == releasedOverAChange {
		return classEveryRun
	}
	if c := classifyR28(r, sc, kind, s, trace); c != "" {
		return c
	}
	switch {
	case raced(trace):
		return classTwoWrites
	case has(trace, "hold abort: done, complete") && !r.AbortMoveBeforeDone:
		return classAbortDone
	case kind == "liveness" && !r.AbortMoveBeforeDone && hasPrefix(trace, "hold abort: admitted"):
		return classAbortDone // its done entry will come before a row move that a restored row's new gen refuses
	}
	switch kind {
	case "two commands past their intent at once":
		return classTwoRun
	case "the row is released while another command runs":
		return classOutside
	case "a disk judged by a re-read step-1 id", "abort released over a restored disk":
		return classR27CReread
	case "no false completion":
		if has(trace, "hold abort: done, complete") && !r.AbortMoveBeforeDone {
			return classAbortDone
		}
		return classPending
	}
	// liveness
	finalDone, reopened, admittedEarly, keyDone, stopMissing := false, false, false, false, false
	pendingAdmission := map[string]bool{}
	for _, tl := range trace {
		if strings.HasSuffix(tl, ": admitted") {
			pendingAdmission[strings.TrimSuffix(tl, ": admitted")] = true
		}
		if strings.HasSuffix(tl, "done, complete") {
			finalDone = true
		}
		if strings.HasSuffix(tl, ": intent written") {
			c := strings.TrimSuffix(tl, ": intent written")
			if finalDone && pendingAdmission[c] {
				admittedEarly = true
			}
			if finalDone {
				reopened = true
			}
		}
		if (strings.HasSuffix(tl, ": admitted, intent written") || strings.HasSuffix(tl, ": admitted")) && finalDone &&
			!strings.HasPrefix(tl, CommandRelease) {
			reopened = true
		}
		if tl == "restore --key: done" {
			keyDone = true
		}
		if strings.HasSuffix(tl, "its intent left pending") && refusal(tl) {
			stopMissing = true
		}
	}
	switch {
	case admittedEarly:
		return classTwoWrites
	case stopMissing, r.Serialize && !r.StopEntries:
		return classStop
	}
	if !sc.Hub {
		switch {
		case reopened && sc.Name == "selection-recovery":
			return classR27ASel
		case reopened:
			return "R27-A (" + sc.Name + ": a command admitted after the final command completed the record)"
		case sc.Name == "rollback" && s.Unresolved && !keyDone:
			return classR27F
		}
		return "UNCLASSIFIED (liveness)"
	}
	for i, tl := range trace {
		if tl == "hold abort: done, complete" && i+1 < len(trace) && trace[i+1] == "crash in hold abort" {
			return classAbortCrash
		}
	}
	provedAt, checkedAt, restoreAfterProve, restoreLanded := -1, -1, false, false
	for i, tl := range trace {
		switch {
		case tl == "hold prove: done":
			provedAt = i
		case tl == "hold prove: check passes":
			checkedAt = i
		case strings.HasPrefix(tl, "outside: a restore lands"):
			restoreLanded = true
			if provedAt >= 0 || checkedAt >= 0 { // after a prove's check, its done entry records a disk no longer attached
				restoreAfterProve = true
			}
		}
	}
	switch {
	case restoreAfterProve:
		return classR27E
	case provedAt < 0 && restoreLanded:
		return classR27CNoProve
	}
	return "UNCLASSIFIED (liveness)"
}

// ---------------------------------------------------------------- the package's readings

// disagreement checks, at a hub-restore state of the current rules, that the package's own
// readings of the record the state stands for are the model's: Step1, Proven, Epoch, Expected and
// Moved. It returns "" when they agree, else what differs. It reads the state's journal j and its
// disks cur alone, so it is a function of the two, and a walk checks each pair it reaches once.
func disagreement(sc modelScenario, j modelJournal, cur [2]modelDisk) string {
	if !sc.Hub || !j.Rec {
		return ""
	}
	d := toDone(sc, j)
	full := j.Step1[0].ID != 0 && j.Step1[1].ID != 0
	step1, ok := Step1(d)
	if ok != (j.Step1[0].ID != 0 || j.Step1[1].ID != 0) {
		return fmt.Sprintf("Step1 answers %v for the step-1 disks %v", ok, j.Step1)
	}
	for tg, n := range targets {
		want := ""
		if j.Step1[tg].ID != 0 {
			want = itoa(j.Step1[tg].ID)
		}
		if step1["step1_disk_id."+n] != want {
			return fmt.Sprintf("Step1 holds %q for target %s, and the model %q", step1["step1_disk_id."+n], n, want)
		}
	}
	proof, proved := Proven(d)
	if proved != j.Proved {
		return fmt.Sprintf("Proven answers %v, and the model's state proved %v", proved, j.Proved)
	}
	_, ents, epoch := Epoch(d)
	if epoch != j.Proved {
		return fmt.Sprintf("Epoch answers %v, and the model's state proved %v", epoch, j.Proved)
	}
	if j.Proved {
		for tg, n := range targets {
			want := ""
			if j.ProvenSet&(1<<tg) != 0 {
				want = itoa(j.Proven[tg])
			}
			if proof.DiskIDs[n] != want {
				return fmt.Sprintf("Proven's DiskIDs hold %q for target %s, and the model %q", proof.DiskIDs[n], n, want)
			}
		}
		var since modelCmds
		for _, o := range ents {
			if o.State != StateDone {
				continue
			}
			c, known := cmdByName[o.Command]
			if !known {
				return fmt.Sprintf("Epoch's done entries name %q, which the model does not run", o.Command)
			}
			since = since.with(c)
		}
		if since != j.Since {
			return fmt.Sprintf("Epoch's done entries are %q, and the model's epoch %q", since, j.Since)
		}
	}
	if !full {
		read := map[string]string{"A": itoa(cur[0].ID), "B": itoa(cur[1].ID)}
		if _, err := Moved(d, read); !errors.Is(err, ErrUnknown) {
			return fmt.Sprintf("Moved answers %v for a step-1 map that lacks a target", err)
		}
		return ""
	}
	want, ok := Expected(d)
	if !ok {
		return "Expected answers false for a whole step-1 map"
	}
	for tg, n := range targets {
		if want[n] != itoa(j.expected(tg)) {
			return fmt.Sprintf("Expected holds %q for target %s, and the model %d", want[n], n, j.expected(tg))
		}
	}
	if j.Proved {
		read := map[string]string{"A": itoa(cur[0].ID), "B": itoa(cur[1].ID)}
		moved, err := Moved(d, read)
		if err != nil || moved != disksMoved(currentModel, j, cur) {
			return fmt.Sprintf("Moved answers %v, %v, and the model %v", moved, err, disksMoved(currentModel, j, cur))
		}
	}
	return ""
}
