package texts

import (
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// The Design's list of the low kinds ("Which kinds are low"): every case below is Low, and every
// other kind, reader and variant of the list is Med. A task has no urgency.
func TestEachKindHasTheUrgencyItsRuleGives(t *testing.T) {
	architect, controller := claim.RoleArchitect, claim.RoleController
	type low struct {
		kind   Kind
		reader claim.Role
		v      Variant
	}
	lows := []low{
		{PhaseFinished, architect, Variant{Role: claim.RoleImplementer, Phase: phase.Implementing, To: phase.Testing}},
		{PhaseFinished, architect, Variant{Role: claim.RoleImplementer, Phase: phase.Retro, To: phase.Merging}},
		{PhaseFinished, architect, Variant{Role: claim.RoleTester, Phase: phase.Testing, To: phase.Reviewing}},
		{PhaseFinished, architect, Variant{Role: claim.RoleTester, Phase: phase.Testing, To: phase.Implementing}},
		{PhaseFinished, architect, Variant{Role: claim.RoleReviewer, Phase: phase.Reviewing, To: phase.Implementing}},
		{PhaseFinished, architect, Variant{Role: claim.RoleMerger, Phase: phase.Merging, To: phase.Done}},
		{PhaseFinished, architect, Variant{Phase: phase.Admitted, To: phase.Planning}},
		{PhaseFinished, architect, Variant{Role: architect, Phase: phase.Accepting, To: phase.Implementing}},
		{Held, architect, Variant{Phase: phase.Testing, Hold: HoldEscalation}},
		{ChildStatus, architect, Variant{Change: ChangeTodo}},
		{ChildStatus, architect, Variant{Change: ChangeJoined}},
		{ChildStatus, architect, Variant{Change: ChangeStarted}},
		{PRMerged, architect, Variant{}},
		{PlanNotTaken, architect, Variant{}},
		{RetroHeld, architect, Variant{}},
		{AwaitingPerson, controller, Variant{}},
	}
	workers := map[phase.Phase]claim.Role{
		phase.Planning: claim.RolePlanner, phase.Implementing: claim.RoleImplementer, phase.Retro: claim.RoleImplementer,
		phase.Testing: claim.RoleTester, phase.Reviewing: claim.RoleReviewer, phase.Merging: claim.RoleMerger,
	}
	ciPhases := []phase.Phase{phase.Planning, phase.Implementing, phase.Testing, phase.Reviewing, phase.Retro, phase.Merging,
		phase.Accepting, phase.Held, phase.Admitted, phase.AwaitingPerson}
	for _, p := range ciPhases {
		for _, reader := range toPodRoles {
			worker, held := workers[p]
			isLow := reader == architect && held || reader != architect && reader != worker
			if isLow {
				lows = append(lows, low{CISettled, reader, Variant{Phase: p}})
			}
		}
		// checks-red is low to the architect while a worker holds the phase the issue stays in,
		// and med in accepting, which the architect holds itself, and wherever no worker holds it:
		// a held issue stays in the phase its hold came from, and its notice names the hold. Every
		// row this release writes names its head; a row that names none is an earlier release's.
		if _, held := workers[p]; held {
			lows = append(lows, low{ChecksRed, architect, Variant{Phase: p, Head: "c0de"}})
		}
	}
	// The two variants the architect's ruling names, read on their own: low while a worker holds
	// the phase, med in accepting.
	if got := UrgencyOf(ChecksRed, architect, Variant{Phase: phase.Testing, Head: "c0de"}); got != Low {
		t.Errorf("checks-red to the architect in testing is %q, want low: the tester holds the phase and the merge forward is asked", got)
	}
	if got := UrgencyOf(ChecksRed, architect, Variant{Phase: phase.Accepting, Head: "c0de"}); got != Med {
		t.Errorf("checks-red to the architect in accepting is %q, want med: the architect holds the phase and must not sign off the head", got)
	}
	// An issue held from a worker's phase, by that worker's failed claim, by the architect's
	// escalation, by its stopped task or for the issues beneath it, has no worker at that phase:
	// med, as the CI settlement of a held issue is. A row an earlier release queued names no head
	// and no hold, as main's row of an issue held from testing does (workflow's mergeForward at
	// f8a5195a: phase, role and reason alone): med in every phase, by the architect's ruling that a
	// row that does not show that the architect's act can wait is med.
	for p := range workers {
		for _, hold := range []string{HoldFailure, HoldEscalation, HoldStoppedTask, HoldChildren} {
			if got := UrgencyOf(ChecksRed, architect, Variant{Phase: p, Hold: hold, Head: "c0de"}); got != Med {
				t.Errorf("checks-red to the architect of an issue held (%s) from %s is %q, want med: no worker holds the phase", hold, p, got)
			}
		}
		if got := UrgencyOf(ChecksRed, architect, Variant{Role: architect, Phase: p}); got != Med {
			t.Errorf("checks-red to the architect with no head in %s, an earlier release's row, is %q, want med: it may be of a held issue", p, got)
		}
	}

	variants := []Variant{{}}
	for _, l := range lows {
		variants = append(variants, l.v)
	}
	variants = append(variants,
		Variant{Role: claim.RolePlanner, Phase: phase.Planning, To: phase.Implementing},
		Variant{Role: claim.RoleReviewer, Phase: phase.Reviewing, To: phase.Accepting},
		Variant{Role: claim.RoleReviewer, Phase: phase.Reviewing},
		Variant{Phase: phase.Testing, Hold: HoldFailure},
		Variant{Phase: phase.Testing, Hold: HoldCarried},
		Variant{Phase: phase.Merging, Hold: HoldFailure},
		Variant{Phase: phase.Retro, Hold: HoldEscalation},
		Variant{Phase: phase.Testing, Hold: HoldFailure, Head: "c0de"},
		Variant{Role: claim.RoleArchitect, Phase: phase.Testing},
		Variant{Phase: phase.Merging},
		Variant{Change: ChangeLeft}, Variant{Change: ChangeBeneathAccepted}, Variant{Change: ChangeReparented})
	// read is the part of a variant that the Design's case for kind reads.
	read := func(kind Kind, v Variant) Variant {
		switch kind {
		case PhaseFinished:
			if v.Phase == phase.Admitted {
				return Variant{Phase: phase.Admitted}
			}
			return Variant{Role: v.Role, Phase: v.Phase, To: v.To}
		case Held:
			return Variant{Hold: v.Hold}
		case ChildStatus:
			return Variant{Change: v.Change}
		case CISettled:
			return Variant{Phase: v.Phase}
		case ChecksRed:
			// The rule reads whether the row names a head, not which head it names.
			named := Variant{Phase: v.Phase, Hold: v.Hold}
			if v.Head != "" {
				named.Head = "named"
			}
			return named
		}
		return Variant{}
	}
	isLow := func(kind Kind, reader claim.Role, v Variant) bool {
		for _, l := range lows {
			if l.kind == kind && l.reader == reader && read(kind, l.v) == read(kind, v) {
				return true
			}
		}
		return false
	}
	for _, entry := range Entries() {
		if len(entry.Readers) == 0 {
			t.Errorf("%s names no reader", entry.Kind)
		}
		for _, reader := range entry.Readers {
			for _, v := range variants {
				if entry.Kind == CISettled && !slices.Contains(ciPhases, v.Phase) {
					continue
				}
				got := UrgencyOf(entry.Kind, reader, v)
				want := Med
				switch {
				case entry.Form == Task:
					want = ""
				case isLow(entry.Kind, reader, v):
					want = Low
				}
				if got != want {
					t.Errorf("%s to the %s with %+v is %q; the Design says %q", entry.Kind, reader, v, got, want)
				}
			}
		}
	}
}
