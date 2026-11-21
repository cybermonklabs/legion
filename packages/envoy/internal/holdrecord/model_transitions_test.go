package holdrecord

import (
	"fmt"
	"strconv"
)

// ---------------------------------------------------------------- transitions

type modelEdge struct {
	to     modelState
	label  string
	op     bool   // an operator step, which liveness may use
	unsafe string // a safety violation on this step
}

// released is why the row's release in s is unsafe, or "". Abort never releases over a restored
// disk, but after a done prove under HubAbortAfterStop: there it releases over the disks that prove
// proved, the residual the hold's design states (clients reach the restored and proven hub before
// its census, until a streams-census hold runs clean), and a release over a disk the done prove did
// not prove is unsafe.
func released(r modelRules, s modelState, abort bool) string {
	switch {
	case !abort:
		return ""
	case r.HubAbortAfterStop && s.Proved:
		for tg := range targets {
			if s.Cur[tg].ID != expected(s, tg) {
				return "abort released over a disk the done prove did not prove"
			}
		}
		return ""
	case s.Cur[0].Snap != 0 || s.Cur[1].Snap != 0:
		return "abort released over a restored disk"
	}
	return ""
}

// hold set's steps. It runs past its intent, in held, from its row move on.
const (
	setRead    = 1 + iota // step 1's reads and the record's insert
	setPre                // the preflight
	setIntent             // its intent, by Admit, with the row prepare (SetCommand)
	setMove               // the row's move from prepare to held
	setRestore            // v59 hub-restore: snapshot restore's intent
	setAge                // v59 hub-restore: the age check, then snapshot restore's done
	setSteps              // steps 2 to 6 in held, then its done entry (SetCommand)
)

func runs(sl modelSlot) bool {
	switch sl.Cmd {
	case "", cmdAbortPrepare:
		return false
	case cmdSet:
		return sl.Step >= setRestore
	}
	return sl.Step >= 1
}

func running(s modelState) int {
	n := 0
	for _, sl := range s.Slots {
		if runs(sl) {
			n++
		}
	}
	return n
}

// othersRunning reports whether a process other than k is past its intent.
func othersRunning(s modelState, k int) bool {
	for j, sl := range s.Slots {
		if j != k && runs(sl) {
			return true
		}
	}
	return false
}

func idle(s modelState, k int) modelState { s.Slots[k] = modelSlot{}; return s }

func next(r modelRules, t modelTable, sc modelScenario, s modelState, bound int) []modelEdge {
	var out []modelEdge
	for k := range sc.Slots {
		sl := s.Slots[k]
		if sl.Cmd == "" {
			switch s.Row {
			case 'P':
				if a2(s, cmdSet) && (!r.SetCommand || !s.Rec ||
					t.decide(toDone(sc, s), modelRequest{Request: Request{Command: cmdSet}}) == nil) {
					ns := s
					ns.Slots[k] = modelSlot{Cmd: cmdSet, Step: setRead}
					out = append(out, modelEdge{to: ns, label: "hold set: started", op: true})
				}
				if a2(s, cmdAbortPrepare) {
					ns := s
					ns.Slots[k] = modelSlot{Cmd: cmdAbortPrepare, Step: 1}
					out = append(out, modelEdge{to: ns, label: "hold abort from prepare: started", op: true})
				}
			case 'H':
				all := sc.Ops
				if r.SetCommand {
					all = append([]modelOp{{cmd: cmdSet}}, sc.Ops...)
				}
				for _, o := range all {
					if !admitted(r, t, sc, s, o) {
						continue
					}
					out = append(out, admit(r, sc, s, k, o))
				}
			}
			continue
		}
		out = append(out, steps(r, t, sc, s, k)...)
		if s.Crashes < bound {
			cs := idle(s, k)
			cs.Crashes++
			out = append(out, modelEdge{to: cs, label: "crash in " + modelOp{sl.Cmd, sl.Set}.label()})
		}
	}
	if s.Row == 'H' && s.RowGen < 1 {
		ns := s
		ns.RowGen++
		out = append(out, modelEdge{to: ns, label: "outside: the row is lost and restored held at a new gen"})
	}
	if sc.Hub && s.Row == 'H' {
		// Assumption A1: no restore lands between hold abort's or hold release's last disk check and
		// its row move. Such a restore is a restore outside any hold, which the restore procedure
		// forbids and no hold can stop.
		window := false
		for _, sl := range s.Slots {
			if (sl.Cmd == CommandAbort && sl.Step >= 2) || (sl.Cmd == CommandRelease && sl.Step == 2) {
				window = true
			}
		}
		if s.Restores < bound && !window {
			for tg, n := range targets {
				for _, snap := range []byte{'H', 'F'} {
					ns := s
					ns.Cur[tg] = modelDisk{ID: s.NextID, Snap: snap}
					ns.NextID++
					ns.Restores++
					from := map[byte]string{'H': "the hold's snapshot", 'F': "another snapshot"}[snap]
					out = append(out, modelEdge{to: ns, label: fmt.Sprintf("outside: a restore lands on %s from %s", n, from)})
				}
			}
		}
		// Once a command has failed for good after a done prove under HubAbortAfterStop, the
		// operator may restore a target back onto its expected disk (the done prove's recorded one,
		// or step 1's for a target it did not name), a restore the table already admits after the
		// failed re-prove: then abort, which compares each disk with the prove's record, passes after
		// a disk moved and no prove can prove the new one. If that disk is gone, abort's stop names
		// it and the operator restores onto another disk (below). The step is the
		// operator's, so liveness may use it, and A1 holds it out of the window between abort's or
		// release's disk check and its row move, as it holds a restore.
		if sc.Fail && s.Doomed != "" && r.HubAbortAfterStop && s.Proved && !window {
			for tg, n := range targets {
				if want := expected(s, tg); s.Cur[tg].ID != want && s.Lost&(1<<tg) == 0 {
					ns := s
					ns.Cur[tg] = modelDisk{ID: want, Snap: 'H'}
					if s.ProvenSet&(1<<tg) == 0 {
						ns.Cur[tg] = s.Step1[tg]
					}
					out = append(out, modelEdge{to: ns, label: "operator: restores " + n + " back onto the done prove's disk", op: true})
				}
			}
		}
		// After a re-prove failed for good (rule (1)'s premise), a done prove's disk, detached by a
		// later restore, may be lost (deleted) once a run, an outside event: the restore back onto it
		// is then no way out.
		if sc.Fail && s.Proved && (s.Doomed == CommandProve || s.Doomed == "streams census") && s.Losts < 1 && !window {
			for tg, n := range targets {
				if s.ProvenSet&(1<<tg) != 0 && s.Lost&(1<<tg) == 0 && s.Cur[tg].ID != s.Proven[tg] {
					ns := s
					ns.Lost, ns.Losts = s.Lost|1<<tg, s.Losts+1
					out = append(out, modelEdge{to: ns, label: "outside: the done prove's disk of " + n + " is lost"})
				}
			}
		}
		// After abort's stop names a gone disk (rule (1)), the operator restores that target onto
		// another disk, where a new prove can pass and open a new epoch. The other disk is one fixed
		// disk per target, anotherDisk, so the cycle of stop and restore stays in a finite space.
		if r.DiskGoneRestore && s.GoneStop && !window {
			for tg, n := range targets {
				if s.Lost&(1<<tg) != 0 && s.Cur[tg].ID != anotherDisk+tg {
					ns := s
					ns.Cur[tg] = modelDisk{ID: anotherDisk + tg, Snap: 'F'}
					ns.GoneStop = false
					out = append(out, modelEdge{to: ns, label: "operator: restores " + n + " onto another disk", op: true})
				}
			}
		}
	}
	return out
}

// anotherDisk is the id of the disk the operator restores target 0 onto after its done prove's disk
// is gone (target 1's is one above): above every id NextID gives at the model's bounds.
const anotherDisk = 90

// admit starts a command on a held row: one compare-and-swap that writes its intent, or, without
// AtomicAdmit, a read whose intent is written later.
func admit(r modelRules, sc modelScenario, s modelState, k int, o modelOp) modelEdge {
	ns := s
	ns.Slots[k] = modelSlot{Cmd: o.cmd, Set: o.set, Gen: s.RowGen}
	if sc.Hub {
		if r.Step1Recorded {
			ns.Slots[k].Base = s.Step1
		} else {
			ns.Slots[k].Base = s.Cur // the command's own step-1 reading, at its start
		}
		for tg := range targets {
			if o.set&(1<<tg) != 0 {
				ns.Slots[k].Named[tg] = s.Cur[tg].ID
			}
		}
	}
	first := 1
	if o.cmd == cmdSet {
		first = setSteps // its rerun in held: the row move is done, so it runs steps 2 to 6 again
	}
	lock := o.cmd != CommandRelease || r.Serialize
	switch {
	case !lock: // v53's hold release writes no entry: its check is its admission
		ns.Slots[k].Step = first
		return modelEdge{to: ns, label: o.label() + ": checked", op: true}
	case r.AtomicAdmit:
		ns = writeIntent(ns, o.cmd)
		ns.Slots[k].Step = first
		return modelEdge{to: ns, label: o.label() + ": admitted, intent written", op: true}
	}
	return modelEdge{to: ns, label: o.label() + ": admitted", op: true}
}

func writeIntent(s modelState, c string) modelState {
	s.Intent = addSet(s.Intent, c)
	s.StopLatest, s.StopBefore = delSet(s.StopLatest, c), delSet(s.StopBefore, c)
	if c != CommandRelease {
		s.Complete = false
	}
	return s
}

// finish writes the command's done entry, and completes the record in the same write when the
// command is final.
func finish(t modelTable, sc modelScenario, s modelState, k int) modelState {
	sl := s.Slots[k]
	c := sl.Cmd
	s.Ever, s.Last = addSet(s.Ever, c), c
	if c == CommandProve {
		if sl.Set != 0 { // a plain prove keeps the named set and records the same disks again
			s.ProvenSet = sl.Set
		}
		s.Proved, s.Since = true, ""
		s.Lost, s.GoneStop = 0, false          // the new done prove's disks are those attached
		for _, x := range list(s.StopLatest) { // stops written so far now precede the latest done prove
			s.StopBefore = addSet(s.StopBefore, x)
		}
		s.StopLatest = ""
		for tg := range targets {
			s.Proven[tg] = 0
			if s.ProvenSet&(1<<tg) != 0 {
				s.Proven[tg] = sl.Checked[tg]
			}
		}
	} else {
		s.Since = addSet(s.Since, c)
	}
	s.Intent = delSet(s.Intent, c)
	s.StopLatest, s.StopBefore = delSet(s.StopLatest, c), delSet(s.StopBefore, c)
	if t.final(sc.W, c) {
		s.Complete, s.HoldGen = true, s.RowGen
	}
	return idle(s, k)
}

// refuse ends a command whose own check refused: with a stop entry when the command is Stoppable,
// which keeps complete, else with its intent left. rerun marks a refusal whose stated way out is the
// command's own rerun, which ErrBusy admits. Its stop records nothing of what the command left,
// which counts as changed (refuseLeft records it).
func refuse(r modelRules, t modelTable, sc modelScenario, s modelState, k int, why string, rerun bool) modelEdge {
	return refuseLeft(r, t, sc, s, k, why, rerun, 'C')
}

// refuseLeft is refuse whose stop records what the command left: 'U' nothing changed, 'C' a change.
// In the rule sets since the bundle (stopsRecorded) the record keeps the stop, and a stop of a
// command other than hold set and hold release joins Left, which hold release's rule reads; the
// bundle's rule sets never set either, so their states are as the bundle's.
func refuseLeft(r modelRules, t modelTable, sc modelScenario, s modelState, k int, why string, rerun bool, left byte) modelEdge {
	c := s.Slots[k].Cmd
	ns := idle(s, k)
	if t.stoppable(sc.W, c) && inSet(s.Intent, c) {
		ns.Intent = delSet(ns.Intent, c)
		if stopsRecorded(r) {
			ns.StopLatest, ns.StopBefore = addSet(ns.StopLatest, c), delSet(ns.StopBefore, c)
		}
		label := c + ": " + why + ", stop entry"
		if stopsRecorded(r) && c != cmdSet && c != CommandRelease {
			if left == 'C' || ns.Left == 0 {
				ns.Left = left
			}
			label += map[byte]string{'U': " left unchanged", 'C': " left changed"}[left]
		}
		return modelEdge{to: ns, label: label, op: true}
	}
	suffix := ""
	switch {
	case !inSet(s.Intent, c):
	case rerun:
		suffix = ", its intent pending for its rerun"
	case r.Serialize:
		suffix = ", its intent left pending"
	}
	return modelEdge{to: ns, label: c + ": " + why + suffix, op: true}
}

// stopsRecorded reports whether the record keeps each command's latest stop, as the code writes
// it: in every rule set with a rule added since the frozen bundle. The bundle's profiles leave the
// stops out, as the bundle did, so their counts stay the bundle's.
func stopsRecorded(r modelRules) bool {
	return r.WayOut || r.EveryCommandStoppable || r.EndIsFinal || r.BackupCommand || r.HubAbortAfterStop || r.AbortAfterOwnStop
}

// transient lists the refusals the hold's design states for a command, whose way out is the
// command's own rerun once the operator has done the step it names.
var transient = map[string]string{
	"streams move backup":        "stops, an absent stream's verified archive is missing",
	"streams move restore":       "stops, a present stream differs from its backup",
	"streams census":             "refuses, the run is not clean",
	"convert":                    "stops for K, F's info cannot be read",
	"create-bucket":              "stops as UNKNOWN, the bucket's configuration or content is not this hold's",
	"create-bucket --after-loss": "stops, the inputs or the bucket are not the selection record's",
	"restore --finish":           "refuses, a server's id is still the one restore --prepare recorded",
	commandBackup:                "stops, a bucket's snapshot cannot be read",
}

// leftBy is what a command's stop after its change records: 'U' for buckets backup, which writes
// only its archive, outside the hub, and 'C' for every other command, which changed the hub.
func leftBy(c string) byte {
	if c == commandBackup {
		return 'U'
	}
	return 'C'
}

// leftOf is what a stop of c records it left, own being what this run left: a change when this
// run changed something, and, by LeftUnchanged's rule, when an earlier run of c in this hold did,
// a run cut after its change among them; nothing changed otherwise. Under OwnRunOnly, the control,
// it records own alone.
func leftOf(sc modelScenario, s modelState, c string, own byte) byte {
	if own == 'C' || (!sc.OwnRunOnly && inSet(s.Wrote, c)) {
		return 'C'
	}
	return 'U'
}

func setStep(r modelRules, t modelTable, sc modelScenario, s modelState, k int) []modelEdge {
	sl := s.Slots[k]
	ns := s
	to := func(step int, label string) modelEdge {
		n := ns
		n.Slots[k].Step = step
		return modelEdge{to: n, label: label, op: true}
	}
	switch sl.Step {
	case setRead:
		if s.Rec {
			return []modelEdge{to(setPre, "hold set: its record found")}
		}
		ins := ns
		ins.Rec = true
		if sc.Hub {
			ins.Step1 = s.Cur
		}
		ins.Slots[k].Step = setPre
		out := []modelEdge{{to: ins, label: "hold set: step 1 read, record inserted", op: true}}
		if sc.Hub {
			for tg, n := range targets {
				if r.Step1Whole {
					out = append(out, refuse(r, t, sc, s, k, "target "+n+"'s disk cannot be read, UNKNOWN, refused", false))
					continue
				}
				p := ins
				p.Step1[tg] = modelDisk{}
				out = append(out, modelEdge{to: p, label: "hold set: target " + n + " unread, record inserted without it", op: true})
			}
		}
		return out
	case setPre:
		nx := setMove
		if r.SetCommand {
			nx = setIntent
		}
		return []modelEdge{refuse(r, t, sc, s, k, "the preflight refuses", false), to(nx, "hold set: the preflight passes")}
	case setIntent:
		if s.Row != 'P' || t.decide(toDone(sc, s), modelRequest{Request: Request{Command: cmdSet}}) != nil {
			return []modelEdge{refuse(r, t, sc, s, k, "Admit refuses", false)}
		}
		ns = writeIntent(ns, cmdSet)
		return []modelEdge{to(setMove, "hold set: admitted with the row prepare, intent written")}
	case setMove:
		if s.Row != 'P' {
			return []modelEdge{refuse(r, t, sc, s, k, "409, the row moved", false)}
		}
		ns.Row = 'H'
		if !r.SetCommand && sc.Hub {
			return []modelEdge{to(setRestore, "hold set: row moved to held")}
		}
		return []modelEdge{to(setSteps, "hold set: row moved to held")}
	case setRestore: // v59: Admit of snapshot restore, by hold set's process
		if t.decide(toDone(sc, s), modelRequest{Request: Request{Command: cmdSnapRestore}}) != nil {
			return []modelEdge{refuse(r, t, sc, s, k, "Admit refuses snapshot restore", false)}
		}
		ns = writeIntent(ns, cmdSnapRestore)
		return []modelEdge{to(setAge, cmdSnapRestore+": admitted, intent written")}
	case setAge: // v59: snapshot restore's done, its own write after its intent's
		ns.Ever, ns.Last, ns.Since = addSet(ns.Ever, cmdSnapRestore), cmdSnapRestore, addSet(ns.Since, cmdSnapRestore)
		ns.Intent = delSet(ns.Intent, cmdSnapRestore)
		return []modelEdge{to(setSteps, cmdSnapRestore+": done")}
	case setSteps:
		out := []modelEdge{refuse(r, t, sc, s, k, "a step of 2 to 6 fails", false)}
		if r.SetCommand {
			return append(out, modelEdge{to: finish(t, sc, ns, k), label: "hold set: steps 2 to 6 pass, done", op: true})
		}
		return append(out, modelEdge{to: idle(ns, k), label: "hold set: steps 2 to 6 pass, the report printed", op: true})
	}
	panic("holdmodel: no hold set step " + strconv.Itoa(sl.Step))
}

func steps(r modelRules, t modelTable, sc modelScenario, s modelState, k int) []modelEdge {
	sl := s.Slots[k]
	c := sl.Cmd
	ns := s
	adv := func(ns modelState, label string) modelEdge {
		ns.Slots[k].Step++
		return modelEdge{to: ns, label: label, op: true}
	}
	if sl.Step == 0 { // a two-write admission writes its intent now, whatever was written since its read
		ns = writeIntent(ns, c)
		if c == cmdSet {
			ns.Slots[k].Step = setSteps
			return []modelEdge{{to: ns, label: c + ": intent written", op: true}}
		}
		return []modelEdge{adv(ns, modelOp{c, sl.Set}.label()+": intent written")}
	}
	switch c {
	case cmdSet:
		return setStep(r, t, sc, s, k)
	case cmdAbortPrepare:
		if sl.Step == 1 {
			return []modelEdge{adv(ns, "hold abort from prepare: cohort deleted")}
		}
		if s.Row != 'P' {
			return []modelEdge{{to: idle(ns, k), label: "hold abort from prepare: 409, the row moved", op: true}}
		}
		m := idle(ns, k)
		m.Row = 'R'
		u := ""
		if othersRunning(m, k) {
			u = "the row is released while another command runs"
		}
		return []modelEdge{{to: m, label: "hold abort from prepare: row released", op: true, unsafe: u}}
	}
	putRow := func(ns modelState, abort bool) (modelState, string) {
		ns.Row = 'R'
		u := ""
		if sc.Hub {
			u = released(r, ns, abort)
		}
		if u == "" && othersRunning(ns, k) {
			u = "the row is released while another command runs"
		}
		return ns, u
	}
	reread := func() string {
		if sc.Hub && sl.Base != s.Step1 {
			return "a disk judged by a re-read step-1 id"
		}
		return ""
	}
	lacks := func() bool { // a target the step-1 map lacks, which Step1Whole reads as UNKNOWN
		return r.Step1Whole && (sl.Base[0].ID == 0 || sl.Base[1].ID == 0)
	}
	switch {
	case c == CommandRelease:
		if sl.Step == 1 && !r.CompletionSurvivesRowRestore && s.Complete && s.HoldGen != s.RowGen {
			return []modelEdge{refuse(r, t, sc, s, k, "refused, hold_gen is not the row's gen", false)}
		}
		if !sc.Hub || sl.Step == 2 {
			if sc.Hub && s.RowGen != sl.Gen {
				return []modelEdge{refuse(r, t, sc, s, k, "409 gen_mismatch, the row was restored since its read", true)}
			}
			m, u := putRow(idle(ns, k), false)
			m.Intent = delSet(m.Intent, c)
			if u == "" && !m.Complete && m.Wrote != "" {
				// Release on a record that is not complete follows only stops that left nothing
				// changed, so a run's change outside the journal that still stands is one a stop
				// failed to record.
				u = releasedOverAChange
			}
			return []modelEdge{{to: m, label: c + ": PUT", op: true, unsafe: u}}
		}
		if r.Step1Whole && (s.Step1[0].ID == 0 || s.Step1[1].ID == 0) {
			return []modelEdge{refuse(r, t, sc, s, k, "UNKNOWN, a target the step-1 map lacks", false)}
		}
		ok := s.Proved
		for tg := range targets {
			ok = ok && s.Cur[tg].ID == expected(s, tg)
		}
		if !ok {
			return []modelEdge{refuse(r, t, sc, s, k, "refused, a disk is not the expected one", false)}
		}
		ns.Slots[k].Gen = s.RowGen
		return []modelEdge{adv(ns, c+": disks checked")}
	case !sc.Hub && c != CommandAbort:
		// Under Fail any command may fail for good, one per run: a doomed command's every later run
		// fails before any further change, and only while no command is doomed can one become so.
		doomable := sc.Fail && s.Doomed == ""
		if sl.Step == 1 {
			if sc.Fail && s.Doomed == c {
				return []modelEdge{refuseLeft(r, t, sc, s, k, "fails again, nothing more changed", false, leftOf(sc, s, c, 'U'))}
			}
			switch {
			case c == "restore --finish" && s.Unresolved:
				return []modelEdge{refuse(r, t, sc, s, k, "stops, a name has no start", false)}
			case c == "restore --prepare" && s.Unresolved && !r.PrepareMarksUnresolved:
				return []modelEdge{refuse(r, t, sc, s, k, "stops, a name has no start", false)}
			case c == "convert" && s.NoStart:
				return []modelEdge{refuse(r, t, sc, s, k, "stops, no safe start for K", false)}
			}
			switch c {
			case "restore --key":
				ns.Unresolved = false
			case "replay":
				ns.NoStart = false // replay --key gives K a start
			}
			if sc.Fail && c != commandBackup {
				ns.Wrote = addSet(ns.Wrote, c) // its change outside the journal, which only its stop can record
			}
			out := []modelEdge{adv(ns, c+": effect")}
			if why, ok := transient[c]; ok {
				out = append(out, refuseLeft(r, t, sc, s, k, why, true, leftOf(sc, s, c, leftBy(c))))
			}
			if doomable {
				ds := s
				ds.Doomed = c
				out = append(out, refuseLeft(r, t, sc, ds, k, "fails for good before any change", false, leftOf(sc, ds, c, 'U')))
			}
			return out
		}
		if t.final(sc.W, c) && r.PendingBlocksComplete && otherPendingAdmitted(r, t, sc, s, c) {
			return []modelEdge{{to: idle(ns, k), label: c + ": refuses to complete, another command's intent has no done", op: true}}
		}
		label := c + ": done"
		if t.final(sc.W, c) {
			label += ", complete"
		}
		out := []modelEdge{{to: finish(t, sc, ns, k), label: label, op: true}}
		if doomable { // it fails for good after its change: a recreate that deleted the bucket
			ds := s
			ds.Doomed = c
			out = append(out, refuseLeft(r, t, sc, ds, k, "fails for good after its change", false, leftBy(c)))
		}
		return out
	case c == CommandProve:
		if sl.Step == 1 { // restart and check
			// Under Fail a prove after a done prove may fail for good, as hub-restore's other
			// command after a prove, the census, may: it changes nothing outside the journal.
			if sc.Fail && s.Doomed == c && s.Cur == s.DoomedOn {
				return []modelEdge{refuseLeft(r, t, sc, s, k, "fails again, nothing more changed", false, 'U')}
			}
			if lacks() {
				return []modelEdge{refuse(r, t, sc, s, k, "UNKNOWN, a target the step-1 map lacks", false)}
			}
			ok := true
			u := ""
			for tg := range targets {
				if sl.Set == 0 { // the plain prove: every disk the expected one
					ok = ok && s.Cur[tg].ID == expected(s, tg)
				} else if sl.Set&(1<<tg) != 0 {
					ok = ok && s.Cur[tg].ID == sl.Named[tg] && s.Cur[tg].ID != sl.Base[tg].ID
					if sl.Base[tg].ID == 0 {
						u = "a disk judged against a target the step-1 map lacks"
					}
				} else {
					ok = ok && s.Cur[tg].ID == sl.Base[tg].ID
				}
			}
			if u == "" {
				u = reread()
			}
			if !ok {
				e := refuse(r, t, sc, ns, k, "check fails", false)
				if u != "a disk judged against a target the step-1 map lacks" {
					e.unsafe = u
				}
				return []modelEdge{e}
			}
			ns.Slots[k].Checked = [2]int{s.Cur[0].ID, s.Cur[1].ID}
			e := adv(ns, c+": check passes")
			e.unsafe = u
			out := []modelEdge{e}
			if sc.Fail && s.Proved && s.Doomed == "" {
				ds := s
				ds.Doomed, ds.DoomedOn = c, s.Cur
				out = append(out, refuseLeft(r, t, sc, ds, k, "fails for good before any change", false, 'U'))
			}
			return out
		}
		return []modelEdge{{to: finish(t, sc, ns, k), label: c + ": done", op: true}}
	case c == "streams census":
		if sl.Step == 1 { // a run that is not clean is Stoppable here, and the epoch's rerun is the way out
			// Under Fail the census, which runs only after a done prove, may fail for good, before
			// its removals or after them, and every later run of it fails again.
			if sc.Fail && s.Doomed == c {
				return []modelEdge{refuseLeft(r, t, sc, s, k, "fails again, nothing more changed", false, leftOf(sc, s, c, 'U'))}
			}
			out := []modelEdge{adv(ns, c+": read and removals"), refuse(r, t, sc, s, k, "refuses, the run is not clean", true)}
			if sc.Fail && s.Doomed == "" {
				ds := s
				ds.Doomed = c
				out = append(out, refuseLeft(r, t, sc, ds, k, "fails for good before any change", false, leftOf(sc, ds, c, 'U')))
			}
			return out
		}
		if r.PendingBlocksComplete && otherPendingAdmitted(r, t, sc, s, c) {
			return []modelEdge{{to: idle(ns, k), label: c + ": refuses to complete, another command's intent has no done", op: true}}
		}
		out := []modelEdge{{to: finish(t, sc, ns, k), label: c + ": done, complete", op: true}}
		if sc.Fail && s.Doomed == "" {
			ds := s
			ds.Doomed = c
			out = append(out, refuseLeft(r, t, sc, ds, k, "fails for good after its change", false, 'C'))
		}
		return out
	case c == CommandAbort:
		moveRow := func(ns modelState) ([]modelEdge, bool) {
			if s.RowGen != sl.Gen {
				return []modelEdge{refuse(r, t, sc, s, k, "409 gen_mismatch, the row was restored since its read", true)}, true
			}
			return nil, false
		}
		switch sl.Step {
		case 1:
			if !sc.Hub { // outside hub-restore abort reads no disk: it ends the hold as the record stands
				return []modelEdge{adv(ns, c+": nothing to check outside hub-restore")}
			}
			if lacks() {
				return []modelEdge{refuse(r, t, sc, s, k, "UNKNOWN, a target the step-1 map lacks", false)}
			}
			// After a done prove under HubAbortAfterStop, abort compares each disk with the done
			// prove's record (Expected), which the hold's command does; before one, with step 1's.
			proved := r.HubAbortAfterStop && s.Proved
			ok := true
			for tg := range targets {
				if proved {
					ok = ok && s.Cur[tg].ID == expected(s, tg)
				} else {
					ok = ok && s.Cur[tg].ID == sl.Base[tg].ID && s.Cur[tg].Snap != 'H'
				}
			}
			if !ok {
				why := "refused, a disk is not step 1's"
				gone := ""
				if proved {
					why = "refused, a disk is not the done prove's"
					for tg, n := range targets {
						if s.Lost&(1<<tg) != 0 {
							gone = n
						}
					}
					if gone != "" { // rule (1): the read finds the done prove's disk gone, and the stop names it
						why = "refused, the done prove's disk of " + gone + " is gone"
					}
				}
				e := refuse(r, t, sc, s, k, why, false)
				if gone != "" {
					e.to.GoneStop = true
				}
				e.unsafe = reread()
				return []modelEdge{e}
			}
			e := adv(ns, c+": disks checked")
			e.unsafe = reread()
			return []modelEdge{e}
		case 2:
			if r.AbortMoveBeforeDone {
				if e, stop := moveRow(ns); stop {
					return e
				}
				m, u := putRow(ns, true)
				m.Slots[k].Step = 3
				return []modelEdge{{to: m, label: c + ": row released", op: true, unsafe: u}}
			}
			d := finish(t, sc, ns, k)
			d.Slots[k] = sl
			d.Slots[k].Step = 3
			return []modelEdge{{to: d, label: c + ": done, complete", op: true}}
		default:
			if r.AbortMoveBeforeDone {
				return []modelEdge{{to: finish(t, sc, ns, k), label: c + ": done, complete", op: true}}
			}
			if e, stop := moveRow(ns); stop {
				return e
			}
			m, u := putRow(idle(ns, k), true)
			return []modelEdge{{to: m, label: c + ": row released", op: true, unsafe: u}}
		}
	}
	panic("holdmodel: no steps for " + c)
}
