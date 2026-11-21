package replay

// Assumption is one assumption a replay makes about a recorded window under a rule other than the
// one that made it: what it assumes, and the direction in which it moves a result if it is wrong.
// Each run names the assumptions its measures rest on by number (Scorecard.Assumptions), and the
// tool's page in the docs (docs/site/src/content/docs/legion/replay.md) gives each one a section
// with its evidence in a recorded window, its direction for each measure and the reading of a
// window that would test it, so a reader of a result can weigh what it rests on.
type Assumption struct {
	// Number is the assumption's number: 1 to 18 are the list of the tool's spec, in its order,
	// and the numbers after them were added by its plan.
	Number int `json:"number"`
	// Text is what the replay assumes.
	Text string `json:"text"`
	// Direction is the way the assumption moves a result if it is wrong, with the verdict the
	// review of the first replay gave it where it gave one, and what the tool does about it.
	Direction string `json:"direction"`
}

// Assumptions is every assumption a replay makes, by number, in order: the spec's eighteen, then
// the seven the plan added, from the mapping of the release at 9542c6fb onto today's workflow (19
// to 22), the conflict path of the rule main (23), a daemon's downtime (24) and its reading of
// outside blockers (25). A run names the ones its measures rest on by these numbers.
var Assumptions = []Assumption{
	{Number: 1,
		Text:      "A work segment takes its recorded time at any start and at any number in flight.",
		Direction: "Doubtful: if load slows work, a rule with more issues in flight is favoured. The tool takes the slowdown factor f, which scales a segment by the difference between the replay's issues in flight and the record's."},
	{Number: 2,
		Text:      "A stretch past the review keeps its recorded length and its outcome.",
		Direction: "Doubtful: if architects and mergers are slower with more issues past review, a rule that keeps slots past review is favoured. The tool keeps it, and the comparison says so."},
	{Number: 3,
		Text:      "Send-backs keep their count and cause.",
		Direction: "Doubtful for conflicts: the first replay saw more conflicts after a longer slot wait, so if a longer wait brings more conflicts than the fitted rate gives, a rule under which branches wait longer is favoured. The tool draws conflicts from the fitted rate, with a branch that sets its merges term to zero, and keeps the other loops."},
	{Number: 4,
		Text:      "A link removal, a child moved out, a gate closing and an outside blocker's close happen at their recorded time, or at their recorded offset from the wait's start.",
		Direction: "Doubtful, and the direction is unknown. The tool runs both as branches."},
	{Number: 5,
		Text:      "A person answers at the recorded time, or at the recorded offset.",
		Direction: "Doubtful, and the direction is unknown. The tool runs both as branches."},
	{Number: 6,
		Text:      "The issues in flight at the window's start enter in their recorded state.",
		Direction: "Sound for one window, and not for a steady state: the start state carries the recorded rule's choices into every rule's first day, which favours the recorded rule. The tool reports the first day apart."},
	{Number: 7,
		Text:      "An issue may start once its todo, its links, its gate, its ancestors and its tree allow it, at the times recorded.",
		Direction: "Wrong in the first replay, which read only todo and links: a hold the replay misses starts an issue early, which favours a rule that starts more issues. The tool reads every hold."},
	{Number: 8,
		Text:      "The work of an issue that never started in the window is unknown.",
		Direction: "A scenario that gives such an issue work that finishes favours a rule that starts more issues, and the scenario that gives it none disfavours that rule. The tool names the scenario and never calls it a bound."},
	{Number: 9,
		Text:      "The line orders returned work first and then by rank.",
		Direction: "Sound: a replay runs the line's own code, so it orders the line as the daemon does, and a rule that orders it otherwise is that rule's own code."},
	{Number: 10,
		Text:      "A link or a child has the lifetime that its events give it.",
		Direction: "Wrong in the first replay, which merged them over the window: a lifetime merged over the window lengthens each wait on it, which favours a rule under which a wait gives its slot back. The tool keeps each one's lifetime."},
	{Number: 11,
		Text:      "A slot that comes free is given out in the same pass.",
		Direction: "Sound for the record, where the median was 18 microseconds and the longest 88 seconds: a slower hand-out would lengthen every rule's waits for a slot alike."},
	{Number: 12,
		Text:      "A waiting slot costs no pod after the idle release.",
		Direction: "Sound after 10 minutes: a pod kept longer would cost a rule that holds idle slots more than the replay counts."},
	{Number: 13,
		Text:      "Work after a send-back takes its recorded time, or that time less its wait for a slot.",
		Direction: "Doubtful: the recorded time holds the recorded rule's own wait for a slot, which favours that rule. The tool runs both as branches."},
	{Number: 14,
		Text:      "A cover under spec v4's rule.",
		Direction: "Wrong in the first replay, which left out some covers and kept others too long, and either misstates the rule with covers. The tool runs the rule's own code."},
	{Number: 15,
		Text:      "An issue that stops working at the window's end would not have finished.",
		Direction: "Wrong: it counts the end of the window as a stop, which disfavours a rule that leaves more work under way at the end. The tool reports issues finished and work left, apart."},
	{Number: 16,
		Text:      "An issue that reopens keeps its later work.",
		Direction: "Wrong in the first replay, which dropped it: dropping the later work understates the issues finished and the slots held under every rule. The tool keeps it."},
	{Number: 17,
		Text:      "The links and children of an issue are the same under every rule.",
		Direction: "Doubtful, because a planner files children after its parent starts: children filed at their recorded times favour a rule that starts parents later. The tool moves a child's filing with its parent's start."},
	{Number: 18,
		Text:      "A conflict under the release that a project runs next behaves as recorded.",
		Direction: "Wrong for a project that moves to a release with cybermonklabs/legion#4464, where a conflicting head keeps its phase, and its recorded conflict loops overstate the conflicts of that release. The tool takes the conflict behaviour of the release as part of the rule."},
	{Number: 19,
		Text:      "Under the release at 9542c6fb, whose workflow has the phase awaiting_merge, the time in awaiting_merge is folded into merging.",
		Direction: "A slot held in awaiting_merge counts as held in merging, so a rule that keeps a slot in merging is charged that time and a rule that keeps none is not."},
	{Number: 20,
		Text:      "Under the release at 9542c6fb, the merger's ready move is a mark inside merging, where the rule infra-w2 gives the slot back.",
		Direction: "If that release gave the slot back at another moment of merging, the slot-hours past the review under infra-w2 move by the time between the two."},
	{Number: 21,
		Text:      "Under the release at 9542c6fb, checks_red from any phase is a move back to implementing.",
		Direction: "A checks_red that did not send the work back would count a send-back loop that never ran, which disfavours that release's rule."},
	{Number: 22,
		Text:      "Under the release at 9542c6fb, red checks and conflicts are one cause of a send-back.",
		Direction: "A window of that release cannot tell how a rule changes conflicts, so a forecast across the change scores the cause only inside the total of all send-back loops, and the result says so."},
	{Number: 23,
		Text:      "Merge-forward work under main lasts as long as the implementing round of a recorded conflict loop.",
		Direction: "A merge forward shorter than a conflict's implementing round overstates the conflict cost of main, which disfavours main."},
	{Number: 24,
		Text:      "A daemon down between two boots runs no pass.",
		Direction: "Sound for the record: what arrives while it is down waits for the next boot, so a replay that ran a pass then would start issues earlier than any daemon could."},
	{Number: 25,
		Text:      "An outside blocker's close is seen at the next tick or boot, as the daemon sees it.",
		Direction: "A close the daemon saw sooner would end link waits sooner, so reading it at the tick lengthens link waits, which disfavours a rule that keeps its slot through a wait."},
}
