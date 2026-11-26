package record

// R10 of the tool's spec, a P0 issue that starts at once outside the admission cap, is the daemon's
// own since cybermonklabs/legion#5488: whether admission reads an issue as P0 (Issue.StartsNow), and
// how many of a project's slot holders are P0 and how many the cap counts (SlotHolders, HoldersOf
// and SlotHolders.Add). A rule turns that start on or off (SlotRule.StartsNow), and every reader of
// it under a rule asks the two answers here and decides neither again: whether an issue starts
// outside the cap under the rule (SlotRule.StartsOutsideCap), and how many of the holders the cap
// counts under it (SlotRule.CapCounted). Admit's capLets, slotFree, promoteHolds and lineCanMove
// read them, and the P0 bound a pass admits under limits the P0 holders (admit's capLets). Under a
// rule whose field is on, each answer is the daemon's own; under one whose field is off, no issue
// starts outside the cap and the cap counts every holder, as the daemon admitted before R10.

// StartsOutsideCap says whether issue starts outside the admission cap under r, and holds its slot
// there: r starts a P0 issue outside the cap (StartsNow, MainRule's where r names no rule) and
// admission reads issue as P0 (Issue.StartsNow: the priority Dispatch resolves for it, else its own,
// and never its tree root's). Such an issue takes no slot of the cap: a P0 bound, where one is in
// force, limits how many of them hold a slot at once (SlotHolders.P0), and with none every one that
// waits only for a slot starts.
func (r SlotRule) StartsOutsideCap(issue Issue) bool {
	return r.OrMain().StartsNow && issue.StartsNow()
}

// CapCounted is how many of held the cap counts under r: the holders below P0 (SlotHolders.Capped)
// under a rule that starts a P0 issue outside the cap (StartsNow), and every holder
// (SlotHolders.Total) under one that starts none there, where a P0 holder took one of the cap's
// slots as any holder did.
func (r SlotRule) CapCounted(held SlotHolders) int {
	if r.OrMain().StartsNow {
		return held.Capped
	}
	return held.Total()
}
