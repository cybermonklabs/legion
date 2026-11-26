package projection

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// slotsProject is the Dispatch project every slots reading test records its issues under.
const slotsProject = "ACME"

// slotsAt is the instant the slots reading tests project at, and every time of their records is
// counted back from it.
var slotsAt = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

// slotsBefore is the instant minutes before slotsAt.
func slotsBefore(minutes int) time.Time {
	return slotsAt.Add(-time.Duration(minutes) * time.Minute)
}

// slotsBeforePtr is slotsBefore as the pointer a time the record may not hold is carried in.
func slotsBeforePtr(minutes int) *time.Time {
	at := slotsBefore(minutes)
	return &at
}

// slotsRoot is a root of its own tree under slotsProject, built by lineIssue, this package's one
// builder of a record with the fields each judgement of the pass reads.
func slotsRoot(key, status string, stands phase.Phase, rank string) record.Issue {
	issue := lineIssue(key, key, status, stands, rank)
	issue.Project = slotsProject
	return issue
}

// slotsChild is a child of the root tree, hanging directly under it.
func slotsChild(key, tree, status string, stands phase.Phase, rank string) record.Issue {
	issue := lineIssue(key, tree, status, stands, rank)
	issue.Project = slotsProject
	issue.Parent = record.ParentOf(tree)
	return issue
}

// linkWaitKeepsTheSlot is the daemon's own rule with one choice changed: a started issue that
// waits on an unfinished Dispatch blocked_by link keeps its slot through the wait, which is the
// rule under which a slot holder waits on another issue and the holder state `issue` is read.
func linkWaitKeepsTheSlot() record.SlotRule {
	rule := record.MainRule
	rule.Name = "main with a link wait that keeps the slot"
	rule.KeepsSlotIn = slices.Clone(rule.KeepsSlotIn)
	rule.LinkWaitGivesBack = false
	return rule
}

// slotsFixture is one project's records as the projection reads them, built issue by issue: the
// store, the claims of the moment, the claims the daemon judges nothing will wake, the slot rule
// and the admission cap.
type slotsFixture struct {
	store    projectionStore
	claims   []supervise.Claim
	stranded []supervise.Claim
	rule     record.SlotRule
	cap      int
}

// runs opens tree: its lifecycle stands open under the workflow and its root's design gate
// stands open, approved ten hours before slotsAt, unless the fixture gave that root a gate of
// its own already.
func (f *slotsFixture) runs(tree string) {
	if !slices.Contains(f.store.lifecycles, tree) {
		f.store.lifecycles = append(f.store.lifecycles, tree)
	}
	if f.store.gates == nil {
		f.store.gates = map[string]map[record.DesignStep]record.DesignGate{}
	}
	if _, gated := f.store.gates[tree]; !gated {
		f.store.gates[tree] = openGate(tree, slotsBefore(600))
	}
}

// add records issue, holding no slot.
func (f *slotsFixture) add(issue record.Issue) {
	f.store.issues = append(f.store.issues, issue)
}

// hold records issue as a slot holder: its record, its slot taken at admitted and its tree
// running (runs).
func (f *slotsFixture) hold(issue record.Issue, admitted time.Time) {
	f.add(issue)
	f.store.slots = append(f.store.slots, record.Slot{Issue: issue.Key, Index: len(f.store.slots), AdmittedAt: admitted})
	f.runs(issue.Tree)
}

// waitsOn records that issue, already recorded, is blocked_by blocker on Dispatch, its own row at
// the status its record shows Dispatch, and blocker's row at status.
func (f *slotsFixture) waitsOn(issue, blocker, status string) {
	own := ""
	for _, recorded := range f.store.issues {
		if recorded.Key == issue {
			own = recorded.DispatchStatus
		}
	}
	f.store.mirror = append(f.store.mirror,
		record.DispatchIssueRow{Key: issue, Project: slotsProject, Status: own, Blockers: []string{blocker}})
	for _, row := range f.store.mirror {
		if row.Key == blocker {
			return
		}
	}
	f.store.mirror = append(f.store.mirror, record.DispatchIssueRow{Key: blocker, Project: slotsProject, Status: status})
}

// owes records that issue owes its worker a start whose row was written at created.
func (f *slotsFixture) owes(issue string, created time.Time) {
	f.store.owed = append(f.store.owed, record.OwedStart{Issue: issue, ID: int64(len(f.store.owed) + 1), CreatedAt: created})
}

// claim records the claim of role on issue in state, shaped by shape where it is not nil.
func (f *slotsFixture) claim(t *testing.T, issue, tree string, role claim.Role, state supervise.ClaimState, shape func(*supervise.Claim)) supervise.Claim {
	t.Helper()
	project, err := claim.ProjectToken(slotsProject)
	if err != nil {
		t.Fatal(err)
	}
	token, err := claim.NewToken(project, issue, role)
	if err != nil {
		t.Fatal(err)
	}
	current := supervise.Claim{Token: token, Project: project, Tree: tree, Issue: issue, Role: role, Generation: 1,
		TreeEpoch: 1, State: state, Session: "ses_" + string(token)}
	if shape != nil {
		shape(&current)
	}
	f.claims = append(f.claims, current)
	return current
}

// project is the state Project builds from the fixture at slotsAt.
func (f *slotsFixture) project(t *testing.T) api.State {
	t.Helper()
	state, err := Project(context.Background(), Inputs{Store: f.store, Project: slotsProject, Claims: f.claims,
		Stranded: f.stranded, Now: slotsAt, SlotRule: f.rule, Cap: f.cap})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	return state
}

// slotsReadingOf is the state's slots reading, failing the test where the state carries none.
func slotsReadingOf(t *testing.T, state api.State) record.SlotsReading {
	t.Helper()
	if state.Admission.Slots == nil {
		t.Fatalf("the state carries no admission.slots: the projection reads no slots (admission.active %v, admission.waiting %v)",
			state.Admission.Active, state.Admission.Waiting)
	}
	return *state.Admission.Slots
}

// countSaid is one count of the reading as a failure message reads it.
func countSaid(count record.StateCount) string {
	if count.Oldest == nil {
		return fmt.Sprintf("%d with no oldest time", count.Count)
	}
	return fmt.Sprintf("%d, oldest %s", count.Count, count.Oldest.Format(time.RFC3339))
}

// countsEqual says whether two counts of a reading are the same number with the same oldest time.
func countsEqual(a, b record.StateCount) bool {
	return a.Count == b.Count && sameInstant(a.Oldest, b.Oldest)
}

// sinceSaid is a holder's time as a failure message reads it.
func sinceSaid(at *time.Time) string {
	if at == nil {
		return "no time"
	}
	return at.Format(time.RFC3339)
}

// stateCounts is the five holder-state counts of a reading, by state.
func stateCounts(reading record.SlotsReading) map[record.HolderState]record.StateCount {
	return map[record.HolderState]record.StateCount{
		record.HolderWorking: reading.Working,
		record.HolderStopped: reading.Stopped,
		record.HolderPerson:  reading.Person,
		record.HolderIssue:   reading.Issue,
		record.HolderIdle:    reading.Idle,
	}
}

// R6's first sentence: every slot in use is counted under exactly one of the five holder states,
// so the five numbers add up to the slots in use, which is every issue `admission.active` names,
// beside the cap. Three projects: one holder in each state with a slot left free, every holder
// at work under a full cap, and no holder at all while a root waits.
func TestTheFiveStatesAddUpToTheSlotsInUse(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(t *testing.T, f *slotsFixture)
		cap   int
		want  map[record.HolderState]int
	}{
		{
			name: "one holder in each state, a slot left free",
			cap:  6,
			build: func(t *testing.T, f *slotsFixture) {
				f.hold(slotsRoot("ACME-1", "in_progress", phase.Implementing, "a"), slotsBefore(180))
				f.claim(t, "ACME-1", "ACME-1", claim.RoleImplementer, supervise.StateWorking, func(c *supervise.Claim) {
					c.LastTurnStart = slotsBefore(30)
				})
				held := slotsRoot("ACME-2", "in_progress", phase.Held, "b")
				held.Hold = &record.Hold{From: phase.Implementing}
				f.hold(held, slotsBefore(180))
				asking := slotsRoot("ACME-3", "in_progress", phase.AwaitingPerson, "c")
				asking.Awaiting = &record.Awaiting{Ask: "ask-3", Since: record.EventMark{Seq: 5, At: slotsBefore(120)}}
				f.hold(asking, slotsBefore(180))
				f.hold(slotsRoot("ACME-4", "in_progress", phase.Implementing, "d"), slotsBefore(180))
				f.waitsOn("ACME-4", "ACME-90", "in_progress")
				f.hold(slotsRoot("ACME-5", "in_progress", phase.Implementing, "e"), slotsBefore(180))
			},
			want: map[record.HolderState]int{record.HolderWorking: 1, record.HolderStopped: 1, record.HolderPerson: 1,
				record.HolderIssue: 1, record.HolderIdle: 1},
		},
		{
			name: "every holder at work, the cap full",
			cap:  3,
			build: func(t *testing.T, f *slotsFixture) {
				for i, key := range []string{"ACME-11", "ACME-12", "ACME-13"} {
					f.hold(slotsRoot(key, "in_progress", phase.Implementing, key), slotsBefore(180))
					f.claim(t, key, key, claim.RoleImplementer, supervise.StateWorking, func(c *supervise.Claim) {
						c.LastTurnStart = slotsBefore(10 * (i + 1))
					})
				}
				f.add(slotsRoot("ACME-14", "todo", phase.Admitted, "z"))
			},
			want: map[record.HolderState]int{record.HolderWorking: 3},
		},
		{
			name: "no holder while a root waits",
			cap:  2,
			build: func(t *testing.T, f *slotsFixture) {
				f.add(slotsRoot("ACME-21", "todo", phase.Admitted, "a"))
			},
			want: map[record.HolderState]int{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &slotsFixture{rule: linkWaitKeepsTheSlot(), cap: tc.cap}
			tc.build(t, f)
			state := f.project(t)
			holders := 0
			for _, n := range tc.want {
				holders += n
			}
			if len(state.Admission.Active) != holders {
				t.Fatalf("fixture: admission.active = %v, want the %d holders the fixture records", state.Admission.Active, holders)
			}

			reading := slotsReadingOf(t, state)
			if reading.InUse != len(state.Admission.Active) || reading.Cap != tc.cap {
				t.Errorf("the reading says %d of %d in use, want %d of %d: every issue admission.active names, of the cap",
					reading.InUse, reading.Cap, len(state.Admission.Active), tc.cap)
			}
			sum := 0
			counts := stateCounts(reading)
			for _, holderState := range record.HolderStates {
				count := counts[holderState]
				sum += count.Count
				if count.Count != tc.want[holderState] {
					t.Errorf("%s = %s, want %d", holderState, countSaid(count), tc.want[holderState])
				}
			}
			if sum != reading.InUse {
				t.Errorf("the five states count %d holders and the reading %d slots in use: each holder is in exactly one state", sum, reading.InUse)
			}
		})
	}
}

// Readings (a) and (b) of started work that waits without a slot (spec R4), each with its
// number and its oldest time. Reading (a) is the members of the line's returned part that hold no
// slot and stand in no row of admission.blocked: here an owed start, and a root released for a
// wait whose wait has ended, which keeps the kind waiting_on_issue until its promotion, so the
// kind does not decide it. Reading (b) is every member of the returned part that holds no slot:
// those two and a root released for a wait that still waits on a link. A root a person answered
// that holds its slot is a returned member in neither; an issue past its review owing a start is
// in no line and in neither, and so are the members of the starting part. The oldest time of each
// reading is the place in the line of its oldest member, which the issue past its review would
// move if it were counted.
func TestReadingsOfStartedWorkWithoutASlot(t *testing.T) {
	f := &slotsFixture{cap: 4}
	// A running tree whose root holds a slot at work, its child owing its tester a start, and a
	// second child whose first phase waits in the starting part.
	f.hold(slotsRoot("ACME-10", "in_progress", phase.Implementing, "b"), slotsBefore(400))
	f.add(slotsChild("ACME-12", "ACME-10", "testing", phase.Testing, "d"))
	f.owes("ACME-12", slotsBefore(180))
	f.add(slotsChild("ACME-11", "ACME-10", "todo", phase.Admitted, "c"))
	// A root a person answered, holding the slot its wait kept.
	answeredAt := slotsBefore(150)
	answered := slotsRoot("ACME-20", "in_progress", phase.AwaitingPerson, "e")
	answered.Awaiting = &record.Awaiting{Ask: "ask-20", Since: record.EventMark{Seq: 5, At: slotsBefore(300)},
		AnsweredAt: &answeredAt, AnsweredBy: record.AnsweredByAsk}
	f.hold(answered, slotsBefore(400))
	// A root released for a wait on a link that has since finished, and one whose link is still
	// open.
	ended := slotsRoot("ACME-30", "in_progress", phase.Implementing, "f")
	ended.ReleasedForWaitAt = slotsBeforePtr(240)
	f.add(ended)
	f.runs("ACME-30")
	f.waitsOn("ACME-30", "ACME-91", "done")
	waiting := slotsRoot("ACME-40", "in_progress", phase.Implementing, "g")
	waiting.ReleasedForWaitAt = slotsBeforePtr(300)
	f.add(waiting)
	f.runs("ACME-40")
	f.waitsOn("ACME-40", "ACME-92", "in_progress")
	// An issue past its review that owes a start, older than every member of the line.
	f.add(slotsRoot("ACME-50", "needs_review", phase.Merging, "h"))
	f.runs("ACME-50")
	f.owes("ACME-50", slotsBefore(360))
	// A root waiting for its tree to be opened.
	f.add(slotsRoot("ACME-60", "todo", phase.Admitted, "z"))

	state := f.project(t)
	for key, want := range map[string]string{
		"ACME-12": "returned returning holds no slot",
		"ACME-20": "returned answered holds a slot",
		"ACME-30": "returned waiting_on_issue holds no slot",
		"ACME-40": "returned waiting_on_issue holds no slot",
		"ACME-11": "starting first_phase holds no slot",
		"ACME-60": "starting queued_root holds no slot",
	} {
		view := state.Issues[key].WaitingLine
		if view == nil {
			t.Fatalf("fixture: %s stands in no line (admission.waiting %v), want %s", key, state.Admission.Waiting, want)
		}
		holds := "holds no slot"
		if view.HoldsSlot != nil && *view.HoldsSlot {
			holds = "holds a slot"
		}
		if got := view.Part + " " + view.Kind + " " + holds; got != want {
			t.Fatalf("fixture: %s stands in the line as %q, want %q", key, got, want)
		}
	}
	if state.Issues["ACME-50"].WaitingLine != nil {
		t.Fatalf("fixture: ACME-50, past its review, stands in the line: %v", state.Admission.Waiting)
	}
	blocked := map[string]bool{}
	for _, row := range state.Admission.Blocked {
		blocked[row.Issue] = true
	}
	if !blocked["ACME-40"] || blocked["ACME-30"] {
		t.Fatalf("fixture: admission.blocked = %v, want ACME-40, whose link is open, and not ACME-30, whose link finished", state.Admission.Blocked)
	}

	reading := slotsReadingOf(t, state)
	if want := (record.StateCount{Count: 2, Oldest: slotsBeforePtr(240)}); !countsEqual(reading.WaitOnlyForSlot, want) {
		t.Errorf("reading (a), waitOnlyForSlot = %s, want %s: the owed start ACME-12 and ACME-30, released for a wait that has ended",
			countSaid(reading.WaitOnlyForSlot), countSaid(want))
	}
	if want := (record.StateCount{Count: 3, Oldest: slotsBeforePtr(300)}); !countsEqual(reading.StartedWithoutSlot, want) {
		t.Errorf("reading (b), startedWithoutSlot = %s, want %s: ACME-12, ACME-30 and ACME-40, which still waits on its link",
			countSaid(reading.StartedWithoutSlot), countSaid(want))
	}
}

// The count of holders below the first issue that waits for a slot, and that issue's key
// (spec R6). An issue waits for a slot where it is a member of the line, in either part, that
// holds no slot, that no wait holds back (blockers.LineWaitsOn gives it nothing) and that stands
// in neither admission.blocked nor admission.held, and the first is the first such member in the
// line's own order. A holder ranks below it where it comes after it in record.RankLess order over
// its tree root's rank and then its own, so a child is placed by its tree root first. Each row's
// ranks are chosen so the count it wants differs from the count of a reading that took the wrong
// member as first, or compared the holders' own ranks alone.
func TestHoldersBelowTheFirstWaitingIssue(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(f *slotsFixture)
		want  record.BelowFirstWaiting
	}{
		{
			name: "a returned issue first",
			build: func(f *slotsFixture) {
				f.add(slotsRoot("ACME-20", "testing", phase.Testing, "m"))
				f.runs("ACME-20")
				f.owes("ACME-20", slotsBefore(60))
				f.hold(slotsRoot("ACME-21", "in_progress", phase.Implementing, "a"), slotsBefore(180))
				f.hold(slotsRoot("ACME-22", "in_progress", phase.Implementing, "t"), slotsBefore(180))
				f.hold(slotsRoot("ACME-23", "in_progress", phase.Implementing, "z"), slotsBefore(180))
				f.add(slotsRoot("ACME-24", "todo", phase.Admitted, "u"))
			},
			want: record.BelowFirstWaiting{Count: 2, First: "ACME-20"},
		},
		{
			name: "reading (a) empty and a first-phase child waiting for the cap first",
			build: func(f *slotsFixture) {
				// The tree whose child waits: its root holds a slot at admitted under an open
				// gate, and a second child of it holds one at work, ranked after the waiting one.
				f.hold(slotsRoot("ACME-30", "in_progress", phase.Admitted, "m"), slotsBefore(180))
				f.add(slotsChild("ACME-31", "ACME-30", "todo", phase.Admitted, "p"))
				f.hold(slotsChild("ACME-32", "ACME-30", "in_progress", phase.Implementing, "q"), slotsBefore(180))
				f.hold(slotsRoot("ACME-33", "in_progress", phase.Implementing, "a"), slotsBefore(180))
				f.hold(slotsRoot("ACME-34", "in_progress", phase.Implementing, "x"), slotsBefore(180))
				// A tree ranked above, whose child's own rank is after the waiting child's.
				f.hold(slotsRoot("ACME-36", "in_progress", phase.Implementing, "c"), slotsBefore(180))
				f.hold(slotsChild("ACME-35", "ACME-36", "in_progress", phase.Implementing, "z"), slotsBefore(180))
				// A returned member a link holds back, and a root waiting for its tree.
				released := slotsRoot("ACME-37", "in_progress", phase.Implementing, "b")
				released.ReleasedForWaitAt = slotsBeforePtr(100)
				f.add(released)
				f.runs("ACME-37")
				f.waitsOn("ACME-37", "ACME-93", "in_progress")
				f.add(slotsRoot("ACME-39", "todo", phase.Admitted, "s"))
			},
			want: record.BelowFirstWaiting{Count: 2, First: "ACME-31"},
		},
		{
			name: "reading (a) empty and a queued root waiting for the cap first",
			build: func(f *slotsFixture) {
				f.add(slotsRoot("ACME-41", "todo", phase.Admitted, "k"))
				f.hold(slotsRoot("ACME-42", "in_progress", phase.Implementing, "c"), slotsBefore(180))
				f.hold(slotsRoot("ACME-43", "in_progress", phase.Implementing, "r"), slotsBefore(180))
				f.hold(slotsRoot("ACME-44", "in_progress", phase.Implementing, "w"), slotsBefore(180))
				f.hold(slotsRoot("ACME-45", "in_progress", phase.Admitted, "n"), slotsBefore(180))
				f.add(slotsChild("ACME-46", "ACME-45", "todo", phase.Admitted, "o"))
			},
			want: record.BelowFirstWaiting{Count: 3, First: "ACME-41"},
		},
		{
			name: "a line whose first member a link holds back, so the next member is first",
			build: func(f *slotsFixture) {
				f.add(slotsRoot("ACME-51", "todo", phase.Admitted, "d"))
				f.waitsOn("ACME-51", "ACME-94", "in_progress")
				f.add(slotsRoot("ACME-52", "todo", phase.Admitted, "h"))
				f.hold(slotsRoot("ACME-53", "in_progress", phase.Implementing, "b"), slotsBefore(180))
				f.hold(slotsRoot("ACME-54", "in_progress", phase.Implementing, "f"), slotsBefore(180))
				f.hold(slotsRoot("ACME-55", "in_progress", phase.Implementing, "k"), slotsBefore(180))
			},
			want: record.BelowFirstWaiting{Count: 1, First: "ACME-52"},
		},
		{
			name: "no issue waits, so the count is zero and the line names none",
			build: func(f *slotsFixture) {
				f.add(slotsRoot("ACME-61", "todo", phase.Admitted, "d"))
				f.waitsOn("ACME-61", "ACME-95", "in_progress")
				f.hold(slotsRoot("ACME-62", "in_progress", phase.Implementing, "a"), slotsBefore(180))
				f.hold(slotsRoot("ACME-63", "in_progress", phase.Implementing, "z"), slotsBefore(180))
			},
			want: record.BelowFirstWaiting{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &slotsFixture{cap: 8}
			tc.build(f)
			state := f.project(t)
			if tc.want.First != "" {
				if view := state.Issues[tc.want.First].WaitingLine; view == nil || (view.HoldsSlot != nil && *view.HoldsSlot) {
					t.Fatalf("fixture: %s is no member of the line holding no slot (admission.waiting %v)", tc.want.First, state.Admission.Waiting)
				}
				for _, row := range state.Admission.Blocked {
					if row.Issue == tc.want.First {
						t.Fatalf("fixture: %s stands in admission.blocked, waiting on %v", tc.want.First, row.BlockedBy)
					}
				}
			}

			reading := slotsReadingOf(t, state)
			if reading.BelowFirstWaiting != tc.want {
				t.Errorf("belowFirstWaiting = %+v, want %+v (admission.waiting %v, admission.active %v)",
					reading.BelowFirstWaiting, tc.want, state.Admission.Waiting, state.Admission.Active)
			}
			if reading.Line == "" || reading.Line != record.SlotsLine(reading) {
				t.Errorf("the reading's line is %q, want the line record.SlotsLine writes of it, %q", reading.Line, record.SlotsLine(reading))
			}
			if tc.want.First == "" {
				if !strings.Contains(reading.Line, "No issue waits for a slot.") || strings.Contains(reading.Line, "which waits for a slot") {
					t.Errorf("the line %q names an issue that waits for a slot, want it to say no issue waits", reading.Line)
				}
			} else if !strings.Contains(reading.Line, " "+tc.want.First+", which waits for a slot.") {
				t.Errorf("the line %q does not name %s as the issue that waits for a slot", reading.Line, tc.want.First)
			}
		})
	}
}

// Each holder's state and time, one holder for each way spec R6 dates a holder's state,
// read from the projected state as a reader of it reads them: on each holder's `slot` as `state`
// and `since`, and counted in `admission.slots` with each state's oldest time. Two rows' times
// are times the record does not hold yet — when a hold began, and when a holder's wait on another
// issue began — so those two holders carry their state and no time, and the stopped count's
// oldest is the stranded claim's stop, the oldest time known. The project runs a rule under which
// a link wait keeps the slot, which is what makes a holder that waits on an issue a holder.
func TestHolderStatesInTheState(t *testing.T) {
	f := &slotsFixture{rule: linkWaitKeepsTheSlot(), cap: 12}
	admitted := slotsBefore(180)
	implementing := func(key, rank string) record.Issue {
		return slotsRoot(key, "in_progress", phase.Implementing, rank)
	}
	type holder struct {
		key   string
		state record.HolderState
		since *time.Time
	}
	var holders []holder

	// A role's claim working: the oldest lastTurnStart of the two claims in a turn.
	f.hold(implementing("ACME-1", "a"), admitted)
	f.claim(t, "ACME-1", "ACME-1", claim.RoleArchitect, supervise.StateWorking, func(c *supervise.Claim) { c.LastTurnStart = slotsBefore(20) })
	f.claim(t, "ACME-1", "ACME-1", claim.RoleImplementer, supervise.StateWorking, func(c *supervise.Claim) { c.LastTurnStart = slotsBefore(50) })
	holders = append(holders, holder{"ACME-1", record.HolderWorking, slotsBeforePtr(50)})

	// No claim in a turn and a start of one of its roles queued: a claim still launching, queued
	// its task fifteen minutes ago.
	f.hold(implementing("ACME-2", "b"), admitted)
	f.claim(t, "ACME-2", "ACME-2", claim.RoleImplementer, supervise.StateLaunching, func(c *supervise.Claim) {
		c.Pending = &supervise.Delivery{ID: "task-2", Task: "Implement ACME-2.", Generation: 1, Entry: 1, Phase: phase.Implementing,
			QueuedAt: slotsBefore(15)}
		c.Handshake = supervise.EntryHandshake{Entry: 1, Generation: 1, QueuedAt: slotsBefore(15)}
	})
	holders = append(holders, holder{"ACME-2", record.HolderWorking, slotsBeforePtr(15)})

	// No turn and a claim holding a task no turn has taken, its last turn long over.
	f.hold(implementing("ACME-3", "c"), admitted)
	f.claim(t, "ACME-3", "ACME-3", claim.RoleImplementer, supervise.StateIdle, func(c *supervise.Claim) {
		c.LastTurnStart, c.LastTurnEnd = slotsBefore(70), slotsBefore(65)
		c.Pending = &supervise.Delivery{ID: "task-3", Task: "Implement ACME-3.", Generation: 1, Entry: 2, Phase: phase.Implementing,
			QueuedAt: slotsBefore(10)}
	})
	holders = append(holders, holder{"ACME-3", record.HolderWorking, slotsBeforePtr(10)})

	// Phase held, its hold's start a time the record does not hold yet.
	held := slotsRoot("ACME-4", "in_progress", phase.Held, "d")
	held.Hold = &record.Hold{From: phase.Implementing}
	f.hold(held, admitted)
	holders = append(holders, holder{"ACME-4", record.HolderStopped, nil})

	// A claim stranded, claim_stopped in the issue's why, no turn: the claim failed and nothing
	// will wake it.
	f.hold(implementing("ACME-5", "e"), admitted)
	f.stranded = append(f.stranded, f.claim(t, "ACME-5", "ACME-5", claim.RoleImplementer, supervise.StateFailed, func(c *supervise.Claim) {
		c.FailedAt = slotsBefore(35)
	}))
	holders = append(holders, holder{"ACME-5", record.HolderStopped, slotsBeforePtr(35)})

	// A root in awaiting_person on an ask, and one on its design gate, neither in a turn.
	asking := slotsRoot("ACME-6", "in_progress", phase.AwaitingPerson, "f")
	asking.Awaiting = &record.Awaiting{Ask: "ask-6", Since: record.EventMark{Seq: 5, At: slotsBefore(120)}}
	f.hold(asking, admitted)
	holders = append(holders, holder{"ACME-6", record.HolderPerson, slotsBeforePtr(120)})
	gated := slotsRoot("ACME-7", "in_progress", phase.AwaitingPerson, "g")
	gated.Awaiting = &record.Awaiting{GateVersion: 2, Artifact: "artifact-ACME-7", Since: record.EventMark{Seq: 6, At: slotsBefore(90)}}
	f.store.gates["ACME-7"] = shutGate("ACME-7")
	f.hold(gated, admitted)
	holders = append(holders, holder{"ACME-7", record.HolderPerson, slotsBeforePtr(90)})

	// A holder with an unfinished blocker, keeping its slot under the rule, no turn: when its
	// wait began is a time the record does not hold yet.
	f.hold(implementing("ACME-8", "h"), admitted)
	f.waitsOn("ACME-8", "ACME-98", "in_progress")
	holders = append(holders, holder{"ACME-8", record.HolderIssue, nil})

	// A holder in a turn that also waits on an issue.
	f.hold(implementing("ACME-9", "i"), admitted)
	f.waitsOn("ACME-9", "ACME-98", "in_progress")
	f.claim(t, "ACME-9", "ACME-9", claim.RoleImplementer, supervise.StateWorking, func(c *supervise.Claim) { c.LastTurnStart = slotsBefore(5) })
	holders = append(holders, holder{"ACME-9", record.HolderWorking, slotsBeforePtr(5)})

	// A held holder whose claim is in a turn.
	heldAtWork := slotsRoot("ACME-10", "in_progress", phase.Held, "j")
	heldAtWork.Hold = &record.Hold{From: phase.Implementing}
	f.hold(heldAtWork, admitted)
	f.claim(t, "ACME-10", "ACME-10", claim.RoleImplementer, supervise.StateWorking, func(c *supervise.Claim) { c.LastTurnStart = slotsBefore(3) })
	holders = append(holders, holder{"ACME-10", record.HolderWorking, slotsBeforePtr(3)})

	// None of the above and its turns ended: the newest lastTurnEnd of its two claims.
	f.hold(implementing("ACME-11", "k"), admitted)
	f.claim(t, "ACME-11", "ACME-11", claim.RoleArchitect, supervise.StateIdle, func(c *supervise.Claim) {
		c.LastTurnStart, c.LastTurnEnd = slotsBefore(55), slotsBefore(45)
	})
	f.claim(t, "ACME-11", "ACME-11", claim.RoleImplementer, supervise.StateIdle, func(c *supervise.Claim) {
		c.LastTurnStart, c.LastTurnEnd = slotsBefore(80), slotsBefore(60)
	})
	holders = append(holders, holder{"ACME-11", record.HolderIdle, slotsBeforePtr(45)})

	// None of the above and no turn yet: its slot's admittedAt.
	f.hold(implementing("ACME-12", "l"), admitted)
	holders = append(holders, holder{"ACME-12", record.HolderIdle, &admitted})

	state := f.project(t)
	if len(state.Admission.Active) != len(holders) {
		t.Fatalf("fixture: admission.active = %v, want the %d holders the fixture records", state.Admission.Active, len(holders))
	}
	for _, want := range holders {
		slot := state.Issues[want.key].Slot
		if slot == nil {
			t.Fatalf("fixture: %s carries no slot", want.key)
		}
		if slot.State != string(want.state) || !sameInstant(slot.Since, want.since) {
			t.Errorf("%s's slot = state %q since %s, want state %q since %s", want.key, slot.State, sinceSaid(slot.Since),
				want.state, sinceSaid(want.since))
		}
	}

	reading := slotsReadingOf(t, state)
	if reading.InUse != 12 || reading.Cap != 12 {
		t.Errorf("the reading says %d of %d in use, want 12 of 12", reading.InUse, reading.Cap)
	}
	wants := map[record.HolderState]record.StateCount{
		record.HolderWorking: {Count: 5, Oldest: slotsBeforePtr(50)},
		record.HolderStopped: {Count: 2, Oldest: slotsBeforePtr(35)},
		record.HolderPerson:  {Count: 2, Oldest: slotsBeforePtr(120)},
		record.HolderIssue:   {Count: 1},
		record.HolderIdle:    {Count: 2, Oldest: &admitted},
	}
	counts := stateCounts(reading)
	for _, holderState := range record.HolderStates {
		if got := counts[holderState]; !countsEqual(got, wants[holderState]) {
			t.Errorf("%s = %s, want %s", holderState, countSaid(got), countSaid(wants[holderState]))
		}
	}
}
