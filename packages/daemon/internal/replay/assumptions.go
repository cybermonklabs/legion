package replay

// Assumption is one assumption a replay makes about a recorded window under a rule other than the
// one that made it: what it assumes, and the direction in which it moves a result if it is wrong.
// Each run names the assumptions its measures rest on by number (Scorecard.Assumptions), and the
// tool's page in the docs (docs/site/src/content/docs/legion/replay.md) gives each one a section
// with its evidence in a recorded window, its direction for each measure and the reading of a
// window that would test it, so a reader of a result can weigh what it rests on.
type Assumption struct {
	// Number is the assumption's number: 1 to 18 are the list of the tool's spec, in its order,
	// 19 to 25 were added by its plan, 26 to 37 are the tool's own, and 38 on were added to the
	// plan (Assumptions).
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
// outside blockers (25), then the tool's own: each a reason a fact is left out of a replay or an
// outbox row is finished without acting (26 to 33: FactSources, OutboxHandling), the trace's
// inference of a slot an issue held from before the window's reach (34: SlotSpan.Inferred), the
// recorded conflict a stretch keeps whose loop runs to the window's end (35: keepConflict), the
// rank a tree's root is shown at under the recorded order of first starts (36: firstStartRanks),
// and, under main and the candidates built on it, the answer of the daemon the replay runs to a
// conflict past the review (37: conflictFor); then the seven the plan's addition D10 added from a
// review of the tool's replays of a recorded window (38 to 44: the merge queue, a person's hours,
// the slowdown factor's fit, work done outside Legion, main's base rules, answer offsets and the
// noise of a validation's redrawn conflicts); then the four of R11's signals (45 to 48: the
// models' forms, the refusals the record keeps as recorded, a signal with no record, and the
// provider's limit between two observations of it, the architect's D13). A run names the ones its
// measures rest on by these numbers.
var Assumptions = []Assumption{
	{Number: 1,
		Text:      "A work segment takes its recorded time at any start and at any number in flight.",
		Direction: "Doubtful: if load slows work, a rule with more issues in flight is favoured. The tool takes the slowdown factor f, which scales a segment by the difference between the replay's issues in flight and the record's."},
	{Number: 2,
		Text:      "A stretch past the review keeps its recorded length and its outcome.",
		Direction: "Doubtful: if architects and mergers are slower with more issues past review, a rule that keeps slots past review is favoured. The tool keeps it, and the comparison says so."},
	{Number: 3,
		Text:      "Send-backs keep their count, cause and length except where a conflict branch changes them. Main keeps recorded conflicts. The git branch uses stored verdicts when it can, and the drawn branch uses the generator.",
		Direction: "A missed conflict favours the rule that caused it. The generator draws once per live stretch, using its phase, boot, release part and touching merges. With git input, a stretch without a touching merge draws no conflict. Repositories without git input keep the fit and count of all merges inside the stretch."},
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
		Text:      "A waiting issue costs no pod after the idle release.",
		Direction: "Doubtful: on 2026-11-24, 43 of 110 waiting issues with a pod had had no role turn for more than 10 minutes. The replay counts no pod, so the result names the pods of waiting issues as a cost that it does not measure."},
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
		Direction: "Doubtful, because a planner files children after its parent starts: children filed at their recorded times favour a rule that starts parents later. The tool moves a child's filing with its parent's start, and never loses one: where the replay passes over, drops or never opens the work a child is filed in before the parent leaves the workflow, the filing enters at its recorded time, or then where that time has passed. A worker's other events on another issue, an edit, a link it sets or takes off, a child it moves out, keep their recorded times."},
	{Number: 18,
		Text:      "A conflict under the release that a project runs next behaves as recorded.",
		Direction: "Wrong for a project that moves to a release with cybermonklabs/legion#4464, where a conflicting head keeps its phase, and its recorded conflict loops overstate the conflicts of that release. The tool takes the conflict behaviour of the release as part of the rule."},
	{Number: 19,
		Text:      "Under the release at 9542c6fb, whose workflow has the phase awaiting_merge, the time in awaiting_merge is folded into merging.",
		Direction: "A slot held in awaiting_merge counts as held in merging, so a rule that keeps a slot in merging is charged that time and a rule that keeps none is not."},
	{Number: 20,
		Text:      "Under the release at 9542c6fb, the merger's ready move is a mark inside merging, where the rule legion-v19.57.3 gives the slot back.",
		Direction: "If that release gave the slot back at another moment of merging, the slot-hours past the review under legion-v19.57.3 move by the time between the two."},
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
	{Number: assumeNoticesChangeNothing,
		Text:      "A notice, a message or a prompt the daemon sends a worker or posts on Dispatch changes nothing the record's workers did after it: their work is the record's segments.",
		Direction: "A notice that changed what a worker did, or how soon, would move that work by the replay's own timing of the notice; under the recorded rule each notice falls where the record's did, and under another rule the work it would have moved stays where the record put it, which favours the recorded rule's timing."},
	{Number: assumePublicationsDecideNothing,
		Text:      "What the daemon publishes on Dispatch for people and tools to read, the gated steps, the design gates, the role holders and the step and plan records, is read back by no decision a replay runs.",
		Direction: "Sound: the daemon decides from its own record and Dispatch's events, and a person who acted on a publication did so in a recorded event the replay applies."},
	{Number: assumeWorkspaceInSegment,
		Text:      "The work of a pod or a workspace, a branch made, a pod stopped, bundled, released or removed, and a role that a pause or an idle pod's release stops, lies inside the recorded work segments and the record's own pause.",
		Direction: "A rule that starts more issues at once may wait longer for pods and workspaces than the record did, which the recorded lengths do not carry, and that favours such a rule; the slowdown factor f is the tool's reading of it."},
	{Number: assumeNoGitHub,
		Text:      "The replay makes no live GitHub requests. Recorded moves supply pull request outcomes other than conflicts, and the replay keeps its own pull request records.",
		Direction: "A changed wait may change GitHub's reports. The git branch uses stored verdicts and draws unanswered reads. Main keeps recorded conflicts and the drawn branch draws from the generator. A missed check failure or queue drop favours a rule that makes heads wait longer."},
	{Number: assumeNoProductionCheck,
		Text:      "A production check after a merge, its looks and its wakes, changes nothing of a replay: it runs after the issue is done.",
		Direction: "Sound for the measures, which end at an issue's done; a check that reopened the issue is the record's own reopen, which the replay applies."},
	{Number: assumeSupervisionSettles,
		Text:      "A worker's claim settles as the record shows it: it launches, is ready and confirms its task at its start, its turns are the record's, and no silence, lost session, memory kill or full disk of supervision's moves the issue.",
		Direction: "A supervision fault the record holds shows as the claim failure, retry or hold it led to, which the replay enters from the record; a fault a rule would bring on more or less often is not drawn, which favours a rule that runs more at once."},
	{Number: assumeCompletionPassed,
		Text:      "A worker's completion passes the checks a live one passes before its move, its claim's run, its handoff record and its review's head, since the record shows it passed them; the move enters at the end of its segment.",
		Direction: "Sound under the recorded rule; under another, a completion whose check would have failed on a later head is entered as passed, which favours a rule under which more completions meet a moved head."},
	{Number: assumeBootRepairsNothing,
		Text:      "The repairs a boot or a read of the repository mapping makes, a merger the cutover left released, a design gate's request found again, a repository's mapping changed, change nothing of a window.",
		Direction: "Sound for a window recorded under one release and one mapping: a repair the record made shows as its own move or start, which the replay enters as recorded."},
	{Number: assumeSlotFromBeforeTheReach,
		Text:      "An issue the record first shows in a phase that keeps a slot, whose first slot line gives a slot back or which no slot line names, held a slot from before the window's reach, until that line or, with none, until the rule gives the slot back; and an issue that stands at the window's start in a worker's phase that keeps a slot, with no slot line holding it there and no wait that gives the slot back, whose worker started in that run of phases after its last slot went back, held one from its last entry into that run until the rule gives it back.",
		Direction: "Doubtful where the reach begins with slots in use and no line names them, or a slot is taken again by a way that writes no slot line: the slot is read from the phase, and at the window's start from the worker's start, alone. If such an issue held no slot, the slots held, with no worker in a turn above all, are overstated by its span; started work that waits for a slot is understated, its time read as held; in a replay its slot keeps another issue from starting until it goes back, which delays that issue and understates the issues finished; the priority taken by holders is overstated where it ranks below an issue that waits; and of the other delays, a link wait it opens counts from its slot, which lengthens the waits, the loops being read from their own lines. A holder the inference misses moves each the other way. The reading that would test it is the slots in use the slot line at the window's start carries, the first at or after it, against the slots the trace holds right after that line: the start state reports the difference and the inferred holders it wrote, and the run carries both in its divergence."},
	{Number: assumeKeptOpenLoop,
		Text:      "A recorded open-loop conflict enters at its end even if the live stretch drew clean earlier or git answers clean now. The move is the record's, not a draw.",
		Direction: "Doubtful, and the direction is unknown: the record holds no work of the issue after such a stretch but its loop, so a draw of no conflict, or of another loop, would leave the issue idle to the window's end; kept, the stretch conflicts in every replay, more often than the fitted rate gives, which overstates the conflicts of every rule alike. The tool counts the conflicts it keeps."},
	{Number: assumeRootRanksByNextStart,
		Text:      "Under the recorded order of first starts, a tree's root ranks at the earliest recorded first start among the issues of its tree that can still take their first start in the replay.",
		Direction: "A choice of the main branch, not a fact of the record: only the first slot of a run is a first start, a start the replay cannot make holds no root at its rank, and a tree's next child takes the place its own recorded start gives it among every tree's work, so the branch starts children in the order the record started them, ahead of a root's later work where the record did so. The branches under rank keep the board's ranks and show how much the choice moves the result."},
	{Number: assumeConflictToMerger,
		Text:      "Under main and the candidates built on it, whose conflict path is main's, a conflict past the review is answered as the daemon the replay runs answers it, which leaves it to the merger (cybermonklabs/legion#4979): in accepting or retro the issue keeps its phase, and the merger merges the base forward once the issue moves into merging; in merging the merger merges it forward at once. A rule that names another path answers it by that path, as the release that ran the rule did: legion-v20.42.5 and legion-v19.57.3 move the issue back to implementing from accepting, retro and merging, and legion-v22.14.1 keeps the phase and asks the implementer to merge the base forward at once. A version or a registration of a root's design in the work the issue goes on with keeps its recorded time.",
		Direction: "The tool's own, and it covers main and the candidates built on it alone: a rule that names another conflict path plays a conflict past the review by that path, so a replay of a window under the rule that recorded it plays that conflict's move back or merge forward as the record does. For main it is the daemon's own answer. For a candidate it is wrong where the candidate would ask the implementer at once, as a daemon before cybermonklabs/legion#4979 did; a merge forward lasts as long whichever role runs it, so the direction is none. Where a conflict in accepting or retro waits for the merger, the work the issue goes on with opens at once, and a version or a registration of the root's design in that work keeps its recorded time, so the tree's design gate opens and closes at the record's times and holds the tree's first starts and its merger as the record did. The tool counts the conflicts it leaves to the merger."},
	{Number: 38,
		Text:      "A stretch in merging keeps its recorded length and outcome. The merge queue's wait under another rule comes from the fitted model, which only a signal rule reads.",
		Direction: "Doubtful: in the record a pull request that entered merging behind more of them was sent back more often and merged later, so a rule or a cap that brings more work to merging at once is favoured. A signal rule reads the modelled wait for admission, not to change the stretch."},
	{Number: 39,
		Text:      "A person answers at any hour of the day, and answers any number of questions at once.",
		Direction: "Doubtful: in the record no answer came in some hours of each day, a question asked then waited many hours, and a question waited longer where many waited, so a rule that moves questions into those hours or asks more of them at once is favoured. The tool places each answer at its recorded time or offset (assumption 5)."},
	{Number: 40,
		Text:      "The slowdown factor f cannot be fitted on a window whose cap did not move, so f comes from outside the window.",
		Direction: "Doubtful: if load slows work, a rule or a cap with more issues in flight is favoured at f = 1. The tool runs each branch at f = 1, 1.2 and 1.6 and searches the break-even f, and fits no f on a window whose cap stayed the same."},
	{Number: 41,
		Text:      "Work done outside Legion is the same under every rule: an issue the record shows closed outside Legion closes at its recorded time.",
		Direction: "Doubtful, and the direction is unknown: a person who does work that waits for a slot does less of it under a rule that starts that work sooner, and the replay gives no rule that credit or that cost. The tool gives the issues done by anyone beside Legion's done."},
	{Number: 42,
		Text:      "Under main, the merge forward of a drawn conflict reads no rules of the base branch, so the engine asks for it at once.",
		Direction: "Wrong where the base branch's rules let a conflicting head wait: main is charged a merge forward that its daemon asks later or not at all, which disfavours main and the rules built on it. The tool gives the engine no rules of the base branch."},
	{Number: 43,
		Text:      "An answer at its recorded offset from its question can come at an hour at which the record shows no answers.",
		Direction: "Doubtful, and the direction is unknown: an answer moved into a person's night comes earlier than it would, and one moved out of it later. Run R0, the replay of the rule that made the record, also on main+timing=wall and main+answer=wall, where each answer and each end of a wait keeps its recorded time."},
	{Number: 44,
		Text:      "Redrawing conflicts can change the first-slot test of a replay of the recorded rule.",
		Direction: "Two seeds can give an issue different first slots. Main keeps recorded conflicts, so this noise does not affect its test. The git and drawn branches face the same gaps as main."},
	{Number: 45,
		Text: "Each modelled signal moves with the work in flight only as its model's form says.",
		Direction: "Doubtful: a minute's tokens scale with the issues in flight, a kill moves with the work segment of its issue, work the record never ran takes the day's kills for each hour of an issue in flight, and each repository's queue is served by its recorded exits and rate; " +
			"every other cause, such as a release or a queue's checks, stays as recorded, and one window had 299, 88, 57 and 110 memory kills on successive days at a full cap. " +
			"Compare the rate each model rests on across two records whose work in flight differs, and read it in each range of 20 issues in flight."},
	{Number: 46,
		Text:      "A refusal by the provider, or one the record does not split, is kept as recorded and counted as R11 says.",
		Direction: "A provider refusal stays as recorded, and so do both kinds in a minute with no record of the project's limit. An unsplit refusal counts as the project's own and the result says so. This can overstate the project's own refusals if some came from the provider. Check against a record that separates the counts."},
	{Number: 47,
		Text:      "A signal with no record in a span does not bind there.",
		Direction: "If an unrecorded signal would exceed its limit, the replay favours a rule that starts work in that span. Check the decision on a span where that signal is recorded."},
	{Number: 48,
		Text:      "The provider's limit holds from one observation to the next.",
		Direction: "Doubtful: a change between observations is read late, so headroom is misread over that stretch, high after a cut and low after a raise. Use the limit in each 429 line's \"of its N\" and in each recorded change, naming the source of each value. Before the first observation headroom is unrecorded and does not bind, and the tokens model tests no refused minute there. Check any recorded limit change against the next 429's N."},
}

// assumeSlotFromBeforeTheReach is the assumption the trace's own inference rests on, that an issue
// the record first shows in a phase that keeps a slot held one from before the window's reach
// (inferSlots), and that an issue standing at the window's start in a worker's phase that keeps a
// slot, whose worker started there with no slot line holding it, held one (inferStartSlots), each
// a SlotSpan.Inferred, which the start state and a run name where they rest on it.
const assumeSlotFromBeforeTheReach = 34

// assumeKeptOpenLoop is the assumption a stretch that keeps its recorded conflict rests on, its loop
// running to the window's end in the record (driver.keepsOpenLoop), which a run names where it
// keeps one (Divergence.KeptOpenLoops).
const assumeKeptOpenLoop = 35

// assumeRootRanksByNextStart is the assumption the main branch's order of first starts rests on,
// that a tree's root ranks at its tree's next recorded first start (firstStartRanks, driver.rerank),
// which a run under FirstStarts recorded names where any issue ranks by its first start.
const assumeRootRanksByNextStart = 36

// assumeConflictToMerger is the assumption a conflict past the review rests on under main and the
// candidates built on it, which the daemon the replay runs leaves to the merger
// (driver.conflictFor), and which a run names where it leaves one (Divergence.ConflictsToMerger).
const assumeConflictToMerger = 37
