package record

// R10 of the tool's spec, a P0 issue that starts at once outside the admission cap, is two answers,
// and every reader of it asks them here and decides neither again: whether an issue starts outside
// the cap under a rule (SlotRule.StartsOutsideCap), and how many of a project's slot holders the
// cap counts and how many it does not (SlotRule.HoldersOf and SlotRule.AddHolder, SlotHolders).
// Admit's capLets, slotFree, promoteHolds and lineCanMove read them, and the P0 bound a pass
// admits under limits the holders the cap does not count (admit's capLets).
//
// The daemon gains R10 in a pull request of its own, cybermonklabs/legion#5488, whose code gives the
// same two answers for the daemon's rule alone: Issue.StartsNow, an issue whose own priority is P0,
// and SlotHolders with HoldersOf and Add. At the forward merge that brings it, this file's
// SlotHolders goes in favour of that one, StartsOutsideCap reads Issue.StartsNow under the rule's
// field, HoldersOf counts through that HoldersOf under a rule whose field is on, and no second copy
// of either answer is left; MainRule's field turns on there. Until then the field
// (SlotRule.P0AboveCap) is off in the daemon's own rule (MainRule), in the rules the recorded
// windows ran and in before-p0, so the daemon admits as it did, and on in the candidates
// p0-at-once and p0-idle, which a replay runs.

// SlotHolders is a project's holders of an admission slot as the cap and a P0 bound count them
// under one rule (SlotRule.HoldersOf): P0, the holders that started outside the cap under it
// (SlotRule.StartsOutsideCap), which the cap does not count and a P0 bound does, and Capped,
// every other holder, which the cap counts. Under a rule that starts no issue outside the cap,
// every holder is Capped. A holder is counted by the priority its record carries at the read, so
// one raised to P0 leaves the cap's count at the next read under such a rule, and one lowered
// below P0 counts against the cap from then on, while its slot stays.
type SlotHolders struct {
	Capped int
	P0     int
}

// StartsOutsideCap says whether issue starts outside the admission cap under r, and holds its slot
// there: r starts a P0 issue outside the cap (P0AboveCap, MainRule's where r names no rule) and the
// priority that decides it is P0 (decidingPriority). Such an issue takes no slot of the cap: a P0
// bound, where one is in force, limits how many of them hold a slot at once (SlotHolders.P0), and
// with none every one that waits only for a slot starts.
func (r SlotRule) StartsOutsideCap(issue Issue) bool {
	return r.OrMain().P0AboveCap && decidingPriority(issue) == P0
}

// BelowP0 says whether issue is below P0 by the priority that decides P0 (decidingPriority): the
// entries admission by signals decides (SignalsLet), so a P0 issue the cap counts, under a rule
// that starts none outside it, starts as it does with admission by signals off (S4).
func BelowP0(issue Issue) bool {
	return decidingPriority(issue) != P0
}

// decidingPriority is the priority that decides whether issue is P0 for R10, the one place the
// seam reads a priority (StartsOutsideCap, and through it HoldersOf and AddHolder): the issue's
// own Dispatch priority (Issue.Priority), never the one PriorityOf gives it for its place in the
// line, so a P0 tree root makes none of its children P0. Spec R10 decides P0 by the priority
// Dispatch resolves for the issue (its own where set, else the top issue of its Dispatch parent
// chain), and with a Dispatch release that supplies none only an issue whose own priority is P0
// qualifies, which is this answer. The resolved priority replaces it here, once the record and a
// replay carry one.
func decidingPriority(issue Issue) Priority {
	return issue.Priority
}

// HoldersOf counts own, one project's slots (admit's ownSlots), by whether each slot's issue of
// issues, that project's records, starts outside the cap under r (StartsOutsideCap). A slot whose
// issue is not among issues is counted with the cap's, as the cap counts every slot of the
// project's. Under a rule that starts no issue outside the cap, every slot is the cap's and no
// record is read.
func (r SlotRule) HoldersOf(issues []Issue, own []Slot) SlotHolders {
	held := SlotHolders{Capped: len(own)}
	if !r.OrMain().P0AboveCap || len(own) == 0 {
		return held
	}
	holding := make(map[string]struct{}, len(own))
	for _, slot := range own {
		holding[slot.Issue] = struct{}{}
	}
	for _, issue := range issues {
		if _, holds := holding[issue.Key]; holds && r.StartsOutsideCap(issue) {
			held.Capped--
			held.P0++
		}
	}
	return held
}

// AddHolder counts issue in held as one more holder under r: with the P0 holders where it starts
// outside the cap (StartsOutsideCap), and with the cap's otherwise. It is HoldersOf for one slot,
// for a reader that counts its holders one by one or grows the count with each slot it gives.
func (r SlotRule) AddHolder(held *SlotHolders, issue Issue) {
	if r.StartsOutsideCap(issue) {
		held.P0++
		return
	}
	held.Capped++
}
