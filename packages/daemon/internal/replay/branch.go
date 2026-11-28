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
	// Name is "main" or "main+<knob>=<value>", the main branch with one knob changed, or with
	// several, each "+<knob>=<value>" in turn (BranchNamed).
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
	// Conflicts is where the conflicts of the replay come from (spec R2, assumption 3):
	// "recorded", the main branch's, the record's own conflicts made through the daemon's path
	// where the record shows them and no other, drawn work keeping its source's; "git", each read
	// of a head's merge state answered from git's verdicts (GitInput), and each read git cannot
	// answer from the conflict generator; and "drawn", every conflict drawn from the generator.
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
	// conflictsGit is Conflicts' value that answers each read of a head's merge state from git's
	// verdicts, and conflictsDrawn the one that draws every conflict from the generator.
	conflictsGit   = "git"
	conflictsDrawn = "drawn"
)

// MainBranch is the branch a replay runs unless it is asked for another: events and answers at
// their recorded offsets, work after a send-back and first starts as recorded, no work for an
// issue that never started, and the record's own conflicts.
var MainBranch = Branch{
	Name:          "main",
	Timing:        "offset",
	Answer:        "offset",
	AfterSendBack: "recorded",
	FirstStarts:   "recorded",
	NeverStarted:  "none",
	Conflicts:     "recorded",
}

// knob is one knob of a branch: its JSON name, the field of a Branch it sets, and its values,
// the main branch's first.
type knob struct {
	name   string
	field  func(*Branch) *string
	values []string
}

// knobs is every knob of a branch, in the order of design section 7, each with its values, the
// source of the conflicts last.
var knobs = []knob{
	{name: "timing", field: func(b *Branch) *string { return &b.Timing }, values: []string{MainBranch.Timing, valueWall}},
	{name: "answer", field: func(b *Branch) *string { return &b.Answer }, values: []string{MainBranch.Answer, valueWall}},
	{name: "afterSendBack", field: func(b *Branch) *string { return &b.AfterSendBack }, values: []string{MainBranch.AfterSendBack, lessSlotWait}},
	{name: "firstStarts", field: func(b *Branch) *string { return &b.FirstStarts }, values: []string{MainBranch.FirstStarts, firstStartsRank}},
	{name: "neverStarted", field: func(b *Branch) *string { return &b.NeverStarted },
		values: []string{MainBranch.NeverStarted, neverStartedDrawn, neverStartedDrawnDone}},
	{name: "conflicts", field: func(b *Branch) *string { return &b.Conflicts }, values: []string{MainBranch.Conflicts, conflictsGit, conflictsDrawn}},
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

// Branches is every branch of design section 7 at every slowdown factor, 27 in all: at each
// factor of Slowdowns, the main branch and each change of one knob to one of its other values,
// the branch that asks git (main+conflicts=git) and the branch that draws every conflict
// (main+conflicts=drawn) among them.
func Branches() []BranchAt {
	changed := []Branch{MainBranch}
	for _, k := range knobs {
		changed = append(changed, k.changes()...)
	}
	branches := make([]BranchAt, 0, len(Slowdowns)*len(changed))
	for _, f := range Slowdowns {
		for _, branch := range changed {
			branches = append(branches, BranchAt{Branch: branch, Slowdown: f})
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

// BranchNamed is the branch name names: "main", the main branch, or
// "main+<knob>=<value>+<knob>=<value>…", the main branch with each knob whose JSON name a change
// names set to that change's value, so a fixture can name a branch that changes more than one knob
// (main+conflicts=git+neverStarted=drawn). The branch keeps name as its Name. It refuses any other
// name, a knob no Branch holds, a knob named twice and a value its knob does not take.
func BranchNamed(name string) (Branch, error) {
	branch := MainBranch
	if name == MainBranch.Name {
		return branch, nil
	}
	malformed := func() error {
		return fmt.Errorf("replay: the branch %q is neither main nor main+<knob>=<value>, with one or more such changes", name)
	}
	changes, ok := strings.CutPrefix(name, MainBranch.Name+"+")
	if !ok {
		return Branch{}, malformed()
	}
	var named uint // bit i: knobs[i] is set by a change already
	for change := range strings.SplitSeq(changes, "+") {
		knobName, value, set := strings.Cut(change, "=")
		if !set || value == "" {
			return Branch{}, malformed()
		}
		at := slices.IndexFunc(knobs, func(k knob) bool { return k.name == knobName })
		switch {
		case at < 0:
			return Branch{}, fmt.Errorf("replay: the branch %q names no knob of a replay's branch in %q", name, change)
		case named&(1<<at) != 0:
			return Branch{}, fmt.Errorf("replay: the branch %q sets %s twice", name, knobName)
		case !slices.Contains(knobs[at].values, value):
			return Branch{}, fmt.Errorf("replay: the branch %q sets %s to %q, which is none of its values %v", name, knobName, value, knobs[at].values)
		}
		named |= 1 << at
		*knobs[at].field(&branch) = value
	}
	branch.Name = name
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
// recorded, the main branch's).
func (b Branch) recordedConflicts() bool { return b.Conflicts == MainBranch.Conflicts }

// asksGit says whether b answers each read of a head's merge state from git's verdicts (Conflicts
// git), the reads git cannot answer going to the conflict generator.
func (b Branch) asksGit() bool { return b.Conflicts == conflictsGit }
