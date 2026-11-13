package supervise

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// The run, the phase entry and the production check look a claim's worker is on, which is what a
// completion is attributed to and what says whether it is a phase completion or a report on the
// check. The order matters in all three: a task a take holds answers over what the claim was left
// serving, and a task the worker took and had taken back answers only for a claim that has served
// none — a worker holding a taken-back next task while still finishing the last one's work
// reports the last one's, which is the sequence the fencing exists for. The three accessors read
// the same three sources the same way, so one table holds them all; where they part is a task
// that carries a run but no entry — a resume, a production check's look — which moves the run on
// and leaves the entry the claim serves alone, and a task of a phase, which is no look's and takes
// the look back: its worker is in the workflow again, where its completion is its phase's.
//
// The look parts from the other two once more, in what "a claim that has served none" means for
// it: the look of a taken-back task is taken only by a claim that has served nothing at all,
// since a claim records no look until a look's task is given to it.
//
// What a task no take of the worker's has reached answers is nothing: the worker never had its
// text (Delivery.TakenWith). The completion route covers a worker of its issue's current run and
// phase holding one (api/handoff.go).
func TestTheRunAClaimServes(t *testing.T) {
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	opened := time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)
	look := CheckLook{Look: 2, OpenedAt: opened}
	firstLook := CheckLook{Look: 1, OpenedAt: opened}
	for _, tc := range []struct {
		name    string
		claim   Claim
		serving uint64
		entry   int64
		look    CheckLook
	}{
		{name: "no task, no run", claim: Claim{}},
		{name: "a run served, no task", claim: Claim{ServingGeneration: 4, ServedEntry: 4}, serving: 4, entry: 4},
		{
			name: "the task a take holds",
			claim: Claim{ServingGeneration: 4, ServedEntry: 4, TaskTaken: 2, Pending: &Delivery{
				Number: 2, Generation: 5, Entry: 5, ConfirmedAt: at}},
			serving: 5, entry: 5,
		},
		{
			name:    "a first task its worker took and had taken back",
			claim:   Claim{TaskTaken: 1, Pending: &Delivery{Number: 1, Generation: 5, Entry: 5}},
			serving: 5, entry: 5,
		},
		{
			name:    "a first task no take reached",
			claim:   Claim{Pending: &Delivery{Number: 1, Generation: 5, Entry: 5}},
			serving: 0,
		},
		{
			name:    "a first task whose number is not the one the worker took",
			claim:   Claim{TaskTaken: 1, Pending: &Delivery{Number: 2, Generation: 5, Entry: 5}},
			serving: 0,
		},
		{
			name: "a taken-back task on a claim already serving a run",
			claim: Claim{ServingGeneration: 4, ServedEntry: 4, TaskTaken: 2, Pending: &Delivery{
				Number: 2, Generation: 5, Entry: 5}},
			serving: 4, entry: 4,
		},
		{
			// A tester serving its testing run and entry, holding look 1's task, taken and taken
			// back. The look it has not begun to serve is not the claim's, or this tester's testing
			// verdict would close its issue's production check with nobody having looked at
			// production.
			name: "a taken-back look task on a claim that already serves a run",
			claim: Claim{ServingGeneration: 4, ServedEntry: 3, TaskTaken: 2, Pending: &Delivery{
				Number: 2, Generation: 4, CheckLook: firstLook}},
			serving: 4, entry: 3,
		},
		{
			name: "a taken-back look task on a claim that already serves a look",
			claim: Claim{ServingGeneration: 5, ServedEntry: 4, ServingCheckLook: firstLook, TaskTaken: 3, Pending: &Delivery{
				Number: 3, Generation: 5, CheckLook: look}},
			serving: 5, entry: 4, look: firstLook,
		},
		{
			// A claim that has served nothing at all: the look's own task is the only work it can
			// be doing, as the run and the entry are.
			name:    "a first look task its worker took and had taken back",
			claim:   Claim{TaskTaken: 1, Pending: &Delivery{Number: 1, Generation: 5, CheckLook: look}},
			serving: 5, look: look,
		},
		{
			name: "an operator's task, which belongs to no run and no entry",
			claim: Claim{ServingGeneration: 4, ServedEntry: 4, TaskTaken: 2, Pending: &Delivery{
				Number: 2, ConfirmedAt: at}},
			serving: 4, entry: 4,
		},
		{
			name: "a resume's task a take holds: a run of its own, the entry it had",
			claim: Claim{ServingGeneration: 4, ServedEntry: 4, TaskTaken: 2, Pending: &Delivery{
				Number: 2, Generation: 5, ConfirmedAt: at}},
			serving: 5, entry: 4,
		},
		{
			name: "a claim upgraded in place: a run served, no entry either side",
			claim: Claim{ServingGeneration: 4, TaskTaken: 1, Pending: &Delivery{
				Number: 1, Generation: 4, ConfirmedAt: at}},
			serving: 4,
		},
		{
			name: "a look's task a take holds: its own run and look, the entry it had",
			claim: Claim{ServingGeneration: 4, ServedEntry: 4, TaskTaken: 2, Pending: &Delivery{
				Number: 2, Generation: 5, CheckLook: look, ConfirmedAt: at}},
			serving: 5, entry: 4, look: look,
		},
		{
			name:    "a look served, and the turn that follows its task's: still that look's",
			claim:   Claim{ServingGeneration: 5, ServedEntry: 4, ServingCheckLook: look},
			serving: 5, entry: 4, look: look,
		},
		{
			name: "a phase task on a claim left serving a look: the look goes with the phase work",
			claim: Claim{ServingGeneration: 5, ServedEntry: 4, ServingCheckLook: look, TaskTaken: 2, Pending: &Delivery{
				Number: 2, Generation: 6, Entry: 6, Phase: phase.Testing, ConfirmedAt: at}},
			serving: 6, entry: 6,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.claim.ServingRun(); got != tc.serving {
				t.Fatalf("ServingRun() = %d, want %d", got, tc.serving)
			}
			if got := tc.claim.ServingEntry(); got != tc.entry {
				t.Fatalf("ServingEntry() = %d, want %d", got, tc.entry)
			}
			if got := tc.claim.ServingCheck(); got != tc.look {
				t.Fatalf("ServingCheck() = %+v, want %+v", got, tc.look)
			}
		})
	}
}
