package record

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// holderAt is the instant every holder of TestHolderState is timed from.
var holderAt = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

// holderMinutesBefore is the instant minutes before holderAt, as the pointer a time HolderFacts
// may not hold is carried in.
func holderMinutesBefore(minutes int) *time.Time {
	at := holderAt.Add(-time.Duration(minutes) * time.Minute)
	return &at
}

// holderFactsAt is a slot holder standing at stands that took its slot three hours before
// holderAt and of which nothing else is known: a root of its own tree, handed to Legion.
func holderFactsAt(stands phase.Phase) HolderFacts {
	return HolderFacts{
		Issue: Issue{Key: "ACME-1", Tree: "ACME-1", Project: "ACME", Title: "ACME-1", Phase: stands, Generation: 1,
			Status: "in_progress", DispatchStatus: "in_progress", Rank: "a", HandedOver: true},
		AdmittedAt: *holderMinutesBefore(180),
	}
}

// heldHolderFacts is a holder in phase held, held from implementing, whose hold began at since.
func heldHolderFacts(since *time.Time) HolderFacts {
	facts := holderFactsAt(phase.Held)
	facts.Issue.Hold = &Hold{From: phase.Implementing}
	facts.HeldSince = since
	return facts
}

// awaitingHolderFacts is a root in awaiting_person on what waiting names, whose wait began at
// since.
func awaitingHolderFacts(waiting Awaiting, since *time.Time) HolderFacts {
	facts := holderFactsAt(phase.AwaitingPerson)
	waiting.Since = EventMark{Seq: 4, At: *since}
	facts.Issue.Awaiting = &waiting
	facts.Person, facts.PersonSince = true, since
	return facts
}

// holderTimeSaid is a time HolderStateOf may give, as a failure message reads it.
func holderTimeSaid(at *time.Time) string {
	if at == nil {
		return "no time"
	}
	return at.Format(time.RFC3339)
}

// holderTimesEqual says whether two times HolderStateOf may give are the same: both absent, or
// both the same instant.
func holderTimesEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// The state of a slot holder and the time it entered it (spec R6), one row for each row of the
// plan's state table, read in its order of precedence: working, stopped, person, issue, idle.
// Three rows follow for a state whose start the record holds no time for, which a holder whose
// state began before the record kept that time is: the state stands and its time is absent.
//
// The precedence itself is taken from record.HolderStates as data: for every pair of states of
// that list, a holder for which both hold is given the earlier one, with the earlier one's time,
// so a list reordered or a state added moves the rows with it, and a state of the list no
// builder below can make fails here by its name.
func TestHolderState(t *testing.T) {
	type row struct {
		name  string
		facts HolderFacts
		state HolderState
		since *time.Time
	}
	rows := []row{
		func() row {
			facts := holderFactsAt(phase.Implementing)
			facts.InTurn = []time.Time{*holderMinutesBefore(20), *holderMinutesBefore(50)}
			return row{name: "a role's claim working, the oldest lastTurnStart of two claims in a turn",
				facts: facts, state: HolderWorking, since: holderMinutesBefore(50)}
		}(),
		func() row {
			facts := holderFactsAt(phase.Implementing)
			facts.Starting = []time.Time{*holderMinutesBefore(15)}
			return row{name: "no claim in a turn and a start of one of its roles queued",
				facts: facts, state: HolderWorking, since: holderMinutesBefore(15)}
		}(),
		func() row {
			facts := holderFactsAt(phase.Implementing)
			facts.PendingTasks = []time.Time{*holderMinutesBefore(10)}
			return row{name: "no turn and a claim holding a task no turn has taken",
				facts: facts, state: HolderWorking, since: holderMinutesBefore(10)}
		}(),
		{name: "phase held", facts: heldHolderFacts(holderMinutesBefore(40)), state: HolderStopped, since: holderMinutesBefore(40)},
		func() row {
			facts := holderFactsAt(phase.Implementing)
			facts.Stranded, facts.StoppedAt = true, holderMinutesBefore(35)
			return row{name: "a claim stranded, claim_stopped in the issue's why, no turn",
				facts: facts, state: HolderStopped, since: holderMinutesBefore(35)}
		}(),
		{name: "a root in awaiting_person on an ask, no turn",
			facts: awaitingHolderFacts(Awaiting{Ask: "ask-1"}, holderMinutesBefore(120)), state: HolderPerson, since: holderMinutesBefore(120)},
		{name: "a root in awaiting_person on its design gate, no turn",
			facts: awaitingHolderFacts(Awaiting{GateVersion: 2, Artifact: "spec-ACME-1"}, holderMinutesBefore(90)), state: HolderPerson,
			since: holderMinutesBefore(90)},
		func() row {
			facts := holderFactsAt(phase.Implementing)
			facts.WaitsOnIssue, facts.WaitSince = true, holderMinutesBefore(25)
			return row{name: "a holder with an unfinished blocker or an open child keeping its slot under the rule, no turn",
				facts: facts, state: HolderIssue, since: holderMinutesBefore(25)}
		}(),
		func() row {
			facts := holderFactsAt(phase.Implementing)
			facts.InTurn = []time.Time{*holderMinutesBefore(5)}
			facts.WaitsOnIssue, facts.WaitSince = true, holderMinutesBefore(25)
			return row{name: "a holder in a turn that also waits on an issue",
				facts: facts, state: HolderWorking, since: holderMinutesBefore(5)}
		}(),
		func() row {
			facts := heldHolderFacts(holderMinutesBefore(40))
			facts.InTurn = []time.Time{*holderMinutesBefore(3)}
			return row{name: "a held holder whose claim is in a turn",
				facts: facts, state: HolderWorking, since: holderMinutesBefore(3)}
		}(),
		func() row {
			facts := holderFactsAt(phase.Implementing)
			facts.LastTurnEnd = holderMinutesBefore(45)
			return row{name: "none of the above, its turns ended",
				facts: facts, state: HolderIdle, since: holderMinutesBefore(45)}
		}(),
		{name: "none of the above, no turn yet", facts: holderFactsAt(phase.Implementing), state: HolderIdle,
			since: holderMinutesBefore(180)},

		{name: "phase held, the hold's start unknown to the record", facts: heldHolderFacts(nil), state: HolderStopped},
		func() row {
			facts := holderFactsAt(phase.Implementing)
			facts.Stranded = true
			return row{name: "a claim stranded, its stop unknown to the record", facts: facts, state: HolderStopped}
		}(),
		func() row {
			facts := holderFactsAt(phase.Implementing)
			facts.WaitsOnIssue = true
			return row{name: "a wait on an issue, its start unknown to the record", facts: facts, state: HolderIssue}
		}(),
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			state, since := HolderStateOf(tc.facts)
			if state != tc.state || !holderTimesEqual(since, tc.since) {
				t.Errorf("HolderStateOf = %q at %s, want %q at %s", state, holderTimeSaid(since), tc.state, holderTimeSaid(tc.since))
			}
		})
	}

	// One way to make each state hold for a holder, touching no field another of them touches,
	// and the time it gives that state. Stopped is made by a stranded claim rather than by
	// phase held, because person sets the phase too.
	makes := map[HolderState]func(*HolderFacts) *time.Time{
		HolderWorking: func(facts *HolderFacts) *time.Time {
			at := holderMinutesBefore(7)
			facts.InTurn = append(facts.InTurn, *at)
			return at
		},
		HolderStopped: func(facts *HolderFacts) *time.Time {
			facts.Stranded, facts.StoppedAt = true, holderMinutesBefore(33)
			return facts.StoppedAt
		},
		HolderPerson: func(facts *HolderFacts) *time.Time {
			at := holderMinutesBefore(95)
			facts.Issue.Phase = phase.AwaitingPerson
			facts.Issue.Awaiting = &Awaiting{Ask: "ask-1", Since: EventMark{Seq: 4, At: *at}}
			facts.Person, facts.PersonSince = true, at
			return at
		},
		HolderIssue: func(facts *HolderFacts) *time.Time {
			facts.WaitsOnIssue, facts.WaitSince = true, holderMinutesBefore(27)
			return facts.WaitSince
		},
		HolderIdle: func(facts *HolderFacts) *time.Time {
			facts.LastTurnEnd = holderMinutesBefore(44)
			return facts.LastTurnEnd
		},
	}
	if len(HolderStates) != len(makes) {
		t.Errorf("record.HolderStates holds %d states and this test makes %d: a state added to the list needs a way to make it here, and a state taken out its maker taken out",
			len(HolderStates), len(makes))
	}
	for i, earlier := range HolderStates {
		for _, later := range HolderStates[i+1:] {
			makeEarlier, makeLater := makes[earlier], makes[later]
			if makeEarlier == nil || makeLater == nil {
				t.Fatalf("record.HolderStates names %q or %q, which this test has no way to make", earlier, later)
			}
			t.Run(string(earlier)+" and "+string(later), func(t *testing.T) {
				facts := holderFactsAt(phase.Implementing)
				makeLater(&facts)
				since := makeEarlier(&facts)
				state, got := HolderStateOf(facts)
				if state != earlier || !holderTimesEqual(got, since) {
					t.Errorf("a holder for which %s and %s both hold is %q at %s, want %q at %s: the earlier of the two in record.HolderStates",
						earlier, later, state, holderTimeSaid(got), earlier, holderTimeSaid(since))
				}
			})
		}
	}
}
