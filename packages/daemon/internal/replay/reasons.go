package replay

// candidateReasons is why each candidate added to the spec's rules is compared, by the rule's name
// (plan D6): the comparison's document gives it in the rule's row of the trade against main
// (writeDecision). A rule the spec names — main, the rules the recorded windows' daemons ran,
// keep-past-review, blockers-first, returned-by-rank and cover — has none. A candidate added to
// record.SlotRules adds its reason here, one line, in plain words that name no deployment, issue or
// person, since the repository is public.
var candidateReasons = map[string]string{
	"wait-grace":             "A wait of one minute sent a started issue to the end of the returned part of the line, and an operator asked that a wait shorter than a slot's next free moment keep the slot. Five minutes is window one's median time from a slot's release to the next free moment (4 m 44 s); 114 waits gave a slot back in window one, 1 of them under five minutes.",
	"wait-grace-1h":          "The grace is a choice, so the comparison shows how the result moves with it.",
	"returned-by-first-slot": "Issues whose wait had cleared stood at the end of the returned part and could not reach a slot, and an operator asked that a cleared wait return an issue to the place it held. The daemon records no first slot, so a replay keeps one in a table of its own (`replay_first_slots`), and the daemon would need that record to run this rule.",
	"rework-first":           "An owner doubted that the board's rank is kept up to date, and asked for first in, first out: work that comes back from rework is admitted before new work, work in progress is burned down in the order it arrived, and rank decides only after that.",
	"rework-first-keep":      "The owner's rule with the slot kept past the review, so the cost of returning work is read with and without the spec's second candidate.",
	"arrival-order":          "The plain first-in, first-out form of the same request: the line in the order its issues arrived, with no rank.",
	"before-p0":              "The reference the spec asks for: today's rule with no P0 issue started outside the cap, so a P0 issue waits in the line as any issue does. It is the same as main until the daemon itself starts a P0 issue outside the cap (cybermonklabs/legion#5488).",
	"p0-at-once":             "The owner's direction: a P0 issue starts at once, outside the cap, and today's rule holds below P0. The replay reads what that costs the rest of the line.",
	"idle-return-1h":         "An owner asked why urgent work cannot start at once: a slot that no role has used for an hour, while its issue waits on its children or on a person, goes to work that can start. Today's rule already gives a started parent's slot back while it waits for its children, in the phase awaiting_children, so this rule differs from it only for an issue that waits on a person.",
	"p0-idle":                "The two answers together, to read whether they add up or overlap.",
	"signals":                "The owner's direction that admission by signals replace the cap: the cap becomes a ceiling only, and below P0 the next issue in the line starts only while each signal that has a limit is under it, at the pass of an event or of a signal's deadline and never at the tick. A P0 issue still starts at once, and no slot is taken back because a signal is over its limit.",
	"signals-backoff":        "The same rule with an allowance in place of the ceiling: the allowance falls by a tenth, rounded down and never below 1, at each refusal by the project's own limit and at each kill, and rises by one for each hour with neither, up to the ceiling, so admission backs off when the project is refused and returns slowly.",
}
