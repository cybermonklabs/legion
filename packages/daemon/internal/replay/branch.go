package replay

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// Branch is one value of each knob of a replay that the record does not give, so that a result
// that rests on one is run under each of its values and read side by side. A knob's values are
// words, each named below with the assumption of the spec it stands for. The slowdown factor f,
// the seventh knob, travels beside the branch as RunConfig.Slowdown, the one place a run reads
// it.
type Branch struct {
	// Name is "main" or "main+<knob>=<value>", the main branch with one knob changed.
	Name string `json:"name"`
	// Timing is when a link removal, a child moved out, a gate closing and an outside blocker's
	// close happen in the replay: "wall", at their recorded time, or "offset", at their recorded
	// offset from the start of the wait they end (assumption 4).
	Timing string `json:"timing"`
	// Answer is when a person answers: "wall" or "offset", as Timing (assumption 5).
	Answer string `json:"answer"`
	// AfterSendBack is how long work after a send-back takes: "recorded", its recorded time, or
	// "less-slot-wait", that time less its recorded wait for a slot (assumption 13).
	AfterSendBack string `json:"afterSendBack"`
	// FirstStarts is the order of first starts: "recorded", the order the record started issues
	// in, or "rank", the board's rank.
	FirstStarts string `json:"firstStarts"`
	// NeverStarted is the work of an issue that never started in the window: "none", it never
	// finishes; "drawn", drawn from the window's started issues; "drawn-done", drawn from its
	// finished issues (assumption 8).
	NeverStarted string `json:"neverStarted"`
	// Merges is the merges term of the conflict draw: "fitted", the rate fitted to the window's
	// base branches, or "zero" (assumption 3).
	Merges string `json:"merges"`
	// Conflicts is where the conflicts of the replay come from: "drawn", each stretch drawing its
	// own at the fitted rate (assumption 3), or "recorded", the record's own conflicts made through
	// the daemon's path where the record shows them and no other drawn (assumption 44). It is a
	// diagnostic knob (knob.diagnostic): a validation runs its other value at every slowdown factor
	// as R5's branch of the recorded conflicts, which names each issue its replay of the recorded
	// rule starts that the record did not and each the record filed that it leaves out, and gates
	// nothing; no comparison runs it.
	Conflicts string `json:"conflicts"`
}

// The values of the knobs other than the main branch's, as the driver reads them.
const (
	// valueWall is Timing's and Answer's value that keeps an event at its recorded time.
	valueWall = "wall"
	// lessSlotWait is AfterSendBack's value that takes the recorded wait for a slot off the work
	// after a send-back.
	lessSlotWait = "less-slot-wait"
	// firstStartsRank is FirstStarts' value that keeps the board's own ranks.
	firstStartsRank = "rank"
	// neverStartedDrawn and neverStartedDrawnDone are NeverStarted's values that draw the work of a
	// started, or a finished, issue of the window.
	neverStartedDrawn     = "drawn"
	neverStartedDrawnDone = "drawn-done"
	// mergesZero is Merges' value that sets the merges term of the conflict draw to zero.
	mergesZero = "zero"
	// conflictsRecorded is Conflicts' value that keeps the record's conflicts and draws none.
	conflictsRecorded = "recorded"
)

// MainBranch is the branch a replay runs unless it is asked for another: events and answers at
// their recorded offsets, work after a send-back and first starts as recorded, no work for an
// issue that never started, the fitted merges term and drawn conflicts.
var MainBranch = Branch{
	Name:          "main",
	Timing:        "offset",
	Answer:        "offset",
	AfterSendBack: "recorded",
	FirstStarts:   "recorded",
	NeverStarted:  "none",
	Merges:        "fitted",
	Conflicts:     "drawn",
}

// knob is one knob of a branch: its JSON name, the field of a Branch it sets, and its values,
// the main branch's first. A diagnostic knob is one only a validation runs, as R5's branch of the
// recorded conflicts beside R5's tests (DiagnosticBranches): no comparison runs it (Branches), and
// what it names gates nothing.
type knob struct {
	name       string
	field      func(*Branch) *string
	values     []string
	diagnostic bool
}

// knobs is every knob of a branch, in the order of design section 7, each with its values, and
// then the diagnostic knob of the conflicts' source.
var knobs = []knob{
	{name: "timing", field: func(b *Branch) *string { return &b.Timing }, values: []string{MainBranch.Timing, valueWall}},
	{name: "answer", field: func(b *Branch) *string { return &b.Answer }, values: []string{MainBranch.Answer, valueWall}},
	{name: "afterSendBack", field: func(b *Branch) *string { return &b.AfterSendBack }, values: []string{MainBranch.AfterSendBack, lessSlotWait}},
	{name: "firstStarts", field: func(b *Branch) *string { return &b.FirstStarts }, values: []string{MainBranch.FirstStarts, firstStartsRank}},
	{name: "neverStarted", field: func(b *Branch) *string { return &b.NeverStarted },
		values: []string{MainBranch.NeverStarted, neverStartedDrawn, neverStartedDrawnDone}},
	{name: "merges", field: func(b *Branch) *string { return &b.Merges }, values: []string{MainBranch.Merges, mergesZero}},
	{name: "conflicts", field: func(b *Branch) *string { return &b.Conflicts }, values: []string{MainBranch.Conflicts, conflictsRecorded},
		diagnostic: true},
}

// Slowdowns is the slowdown factors f every branch is run at (design section 7; spec R3): 1.0,
// which keeps the recorded lengths, and two that slow work as more issues are in flight.
var Slowdowns = []float64{1.0, 1.2, 1.6}

// BranchAt is one branch at one slowdown factor: one of the runs a comparison of rules makes of
// each rule and window (Branches).
type BranchAt struct {
	Branch   Branch  `json:"branch"`
	Slowdown float64 `json:"slowdown"`
}

// Branches is every branch of design section 7 at every slowdown factor, 24 in all: at each
// factor of Slowdowns, the main branch and each change of one knob to one of its other values. A
// diagnostic knob changes none of them (DiagnosticBranches).
func Branches() []BranchAt {
	changed := []Branch{MainBranch}
	for _, k := range knobs {
		if !k.diagnostic {
			changed = append(changed, k.changes()...)
		}
	}
	branches := make([]BranchAt, 0, len(Slowdowns)*len(changed))
	for _, f := range Slowdowns {
		for _, branch := range changed {
			branches = append(branches, BranchAt{Branch: branch, Slowdown: f})
		}
	}
	return branches
}

// DiagnosticBranches is R5's branch of the recorded conflicts, which a validation runs beside R5's
// tests and no comparison runs: the main branch with the diagnostic knob changed to its other value,
// at each slowdown factor of Slowdowns. On it the replay of the recorded rule should start no issue
// the record did not start and file each issue the record filed in the window; its validation names
// each issue that breaks either (BranchValidation.Breakers), which gates nothing, and its table is
// read beside the main branch's, never in place of it.
func DiagnosticBranches() []BranchAt {
	var branches []BranchAt
	for _, k := range knobs {
		if !k.diagnostic {
			continue
		}
		for _, f := range Slowdowns {
			for _, branch := range k.changes() {
				branches = append(branches, BranchAt{Branch: branch, Slowdown: f})
			}
		}
	}
	return branches
}

// changes is the main branch with k changed to each of its other values, in their order, each
// named main+<knob>=<value>.
func (k knob) changes() []Branch {
	changed := make([]Branch, 0, len(k.values)-1)
	for _, value := range k.values[1:] {
		branch := MainBranch
		*k.field(&branch), branch.Name = value, MainBranch.Name+"+"+k.name+"="+value
		changed = append(changed, branch)
	}
	return changed
}

// BranchNamed is the branch name names: "main", the main branch, or "main+<knob>=<value>", the
// main branch with the one knob whose JSON name is <knob> set to <value>. It refuses any other
// name, a knob no Branch holds and a value its knob does not take.
func BranchNamed(name string) (Branch, error) {
	branch := MainBranch
	if name == MainBranch.Name {
		return branch, nil
	}
	change, ok := strings.CutPrefix(name, MainBranch.Name+"+")
	knobName, value, set := strings.Cut(change, "=")
	if !ok || !set || value == "" {
		return Branch{}, fmt.Errorf("replay: the branch %q is neither main nor main+<knob>=<value>", name)
	}
	at := slices.IndexFunc(knobs, func(k knob) bool { return k.name == knobName })
	if at < 0 {
		return Branch{}, fmt.Errorf("replay: the branch %q names no knob of a replay's branch", name)
	}
	if !slices.Contains(knobs[at].values, value) {
		return Branch{}, fmt.Errorf("replay: the branch %q sets %s to %q, which is none of its values %v", name, knobName, value, knobs[at].values)
	}
	*knobs[at].field(&branch), branch.Name = value, name
	return branch, nil
}

// check refuses a branch with a knob set to a value it does not take; a knob left empty takes the
// main branch's value.
func (b Branch) check() error {
	for _, k := range knobs {
		if value := *k.field(&b); value != "" && !slices.Contains(k.values, value) {
			return fmt.Errorf("replay: the branch %s sets %s to %q, which is none of its values %v", b.Name, k.name, value, k.values)
		}
	}
	return nil
}

// orMain is b with each knob it leaves empty at the main branch's value.
func (b Branch) orMain() Branch {
	for _, k := range knobs {
		field := k.field(&b)
		*field = cmp.Or(*field, k.values[0])
	}
	return b
}

// timingWall says whether b keeps the events that end a wait at their recorded times, not at their
// recorded offsets from the replay's start of the wait (Timing wall).
func (b Branch) timingWall() bool { return b.Timing == valueWall }

// answerWall says whether b keeps a person's answers at their recorded times, not at their recorded
// offsets from their questions (Answer wall).
func (b Branch) answerWall() bool { return b.Answer == valueWall }

// recordedConflicts says whether b keeps the record's conflicts and draws none (Conflicts
// recorded).
func (b Branch) recordedConflicts() bool { return b.Conflicts == conflictsRecorded }
