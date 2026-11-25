package replay

//ci:reads docs/site/src/content/docs/legion/replay.md TestTheDocsPageGivesEveryAssumption reads replayPage, the tool's page under ../../../..

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// listsAssumption says whether the tool's list of assumptions (Assumptions) holds the one numbered
// number.
func listsAssumption(number int) bool {
	return slices.ContainsFunc(Assumptions, func(a Assumption) bool { return a.Number == number })
}

// specAssumptions starts with the eighteen assumptions in the spec's Design and includes the
// plan's assumptions 19 to 25, D10's 38 to 44 and the signal assumptions 45 to 48.
// Assumption 3 is as the plan's D7 restates it, with a term for each phase a stretch can end in, and
// as the architect's D14 gives it a term for each release part of merging and a term for each
// phase's stretches that span a boot, at which such a stretch draws, the pool of loops every one
// that ends before the run-out's end, and what a run-out leaves of it.
var specAssumptions = []Assumption{
	{Number: 1,
		Text:      "A work segment takes its recorded time at any start and at any number in flight.",
		Direction: "Doubtful: if load slows work, a rule with more issues in flight is favoured. The tool takes the slowdown factor f, which scales a segment by the difference between the replay's issues in flight and the record's."},
	{Number: 2,
		Text:      "A stretch past the review keeps its recorded length and its outcome.",
		Direction: "Doubtful: if architects and mergers are slower with more issues past review, a rule that keeps slots past review is favoured. The tool keeps it, and the comparison says so."},
	{Number: 3,
		Text:      "Send-backs keep their count and cause, but a conflict, which a stretch draws at the rate fitted on the window for the phase it ends in, for the release part its end falls in where it ends in merging, and for the merges into its base, at the first boot inside the stretch at its phase's chance for a stretch that spans a boot, and otherwise at its end.",
		Direction: "Doubtful for conflicts: the first replay saw more conflicts after a longer slot wait, so if a longer wait brings more conflicts than the fitted rate gives, a rule under which branches wait longer is favoured. The tool fits the rate on the window with a base, a term for each phase a stretch can end in, a term for each release part of the stretches that end in merging, a term for each phase's stretches that span a boot and a merges term, a phase or a part taking a term of its own only where both groups the term sets apart hold a conflict and a stretch without one; it draws conflicts from it, with a branch that sets the merges term to zero and keeps the other terms, and keeps the other loops; a rule moves conflicts through the merges term alone, since each stretch ends in the phase the record shows, at the record's release parts and boots. Merging's rate is each release part's own: a release that posts the merger's conflicts records them as conflicts, and one that does not records them as send-backs, kept as recorded loops, so one rate over both parts would count the earlier part's conflicts twice; in window one the merger sent work back on about 47% of its visits in both parts, and only the split between send-backs and conflicts moved. A daemon's boot reads each pull request again, so a stretch under way at a boot, whatever piece of its work runs there, draws once, at the first boot inside it, at its phase's chance for a stretch that spans a boot, reading no later boot, and conflicts there; if a boot raises the chance more or less than the window shows, the replay misstates how many returned issues wait for a slot at once. A drawn conflict's work is a recorded loop drawn among every one that ends before the run-out's end, the reach's among them; a loop still open at the run-out's end has no known length and is left out, so the longest loops are drawn a little less often than the record ran them. Each stretch draws apart from the record's outcome, so a stretch the record ended in a conflict can draw none, and its issue goes on past its recorded loop into its later recorded work, and a stretch the record ended otherwise can draw one, which pushes its later work later; a passed-over loop's acts on other issues keep their recorded times, and its issue does not close before a child the record filed under it before its own close has entered; each run counts the recorded loops it passes over and the conflicts it adds. A window's run-out holds the record's work after the window's end, which an issue that passes over a loop runs into; work recorded after the run-out's end is not in the replay, so such an issue stands in its last recorded work from where that work ends to the run-out's end, and an approval that an added loop pushes past the window's end is not counted."},
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
		Text:      "A validation of the recorded rule draws the record's conflicts again, so the first-slot test of R5 rests on the replay's own noise.",
		Direction: "Doubtful: two seeds differ in an issue's first slot by about half as much as the replay differs from the record, so the first-slot test can pass or fail on the draws alone. The tool runs the diagnostic branch main+conflicts=recorded beside R5's tests, which keeps the record's conflicts and draws none, and never reads it in place of the main branch."},
	{Number: 45,
		Text:      "The three modelled signals depend on the work in flight alone.",
		Direction: "Doubtful: even at a full cap, one window had 299, 88, 57 and 110 memory kills on successive days. Unmodelled changes can favour or disfavour a signal rule. Compare each model's daily reading with the record at the recorded work in flight."},
	{Number: 46,
		Text:      "A refusal by the provider, or one the record does not split, is kept as recorded and counted as R11 says.",
		Direction: "A provider refusal stays as recorded. An unsplit refusal counts as the project's own and the result says so. This can overstate the project's own refusals if some came from the provider. Check against a record that separates the counts."},
	{Number: 47,
		Text:      "A signal with no record in a span does not bind there.",
		Direction: "If an unrecorded signal would exceed its limit, the replay favours a rule that starts work in that span. Check the decision on a span where that signal is recorded."},
	{Number: 48,
		Text:      "The provider's limit holds from one observation to the next.",
		Direction: "Doubtful: a change between observations is read late, so headroom is misread over that stretch, high after a cut and low after a raise. Use the limit in each 429 line's \"of its N\" and in each recorded change, naming the source of each value. Before the first observation headroom is unrecorded, does not bind and is not fitted. Check any recorded limit change against the next 429's N."},
}

// The list must hold each numbered assumption above with its text and direction, and cannot
// repeat a number. TestTheDocsPageGivesEveryAssumption also checks assumptions the tool adds.
func TestTheAssumptionsHoldTheSpecsList(t *testing.T) {
	byNumber := make(map[int]Assumption, len(Assumptions))
	for _, assumption := range Assumptions {
		if _, twice := byNumber[assumption.Number]; twice {
			t.Errorf("Assumptions numbers %d twice", assumption.Number)
		}
		byNumber[assumption.Number] = assumption
	}
	for _, want := range specAssumptions {
		got, listed := byNumber[want.Number]
		if !listed {
			t.Errorf("Assumptions lacks assumption %d: %s", want.Number, want.Text)
			continue
		}
		if got.Text != want.Text {
			t.Errorf("assumption %d reads %q, want %q", want.Number, got.Text, want.Text)
		}
		if got.Direction != want.Direction {
			t.Errorf("assumption %d's direction reads %q, want %q", want.Number, got.Direction, want.Direction)
		}
	}
}

// replayPage is the tool's page in the docs, under the repository's root.
var replayPage = filepath.Join("..", "..", "..", "..", "docs", "site", "src", "content", "docs", "legion", "replay.md")

// r4Measures is the measures of the tool's spec (R4), each by the words a section of the docs
// page names it by: the issues that reach done; started work that waits for a slot, in its two
// readings and with the issues past their review apart; the priority holders take; the other
// delays, the send-back loops by cause and the waits by what ended them; and the slots, held with
// a worker in a turn and with none, and the issues in flight.
var r4Measures = []string{
	"Issues finished",
	"Started work that waits for a slot",
	"Priority taken by holders",
	"Other delays",
	"Slots",
}

// The three labels a section of the docs page gives its parts under.
const (
	evidenceLabel  = "**Evidence in a recorded window.**"
	directionLabel = "**If it is wrong.**"
	readingLabel   = "**The reading that would test it.**"
)

// assumptionHeading is the heading of an assumption's section: `### Assumption <n>: <title>`.
var assumptionHeading = regexp.MustCompile(`^### Assumption ([0-9]+): (\S.*)$`)

// The tool's page in the docs gives each assumption of the tool's list (Assumptions) with the
// evidence for it in a recorded window, the direction in which it moves each measure of R4 if it
// is wrong, and the reading of a window that would test it (spec R3). The test reads the page,
// docs/site/src/content/docs/legion/replay.md, as a reader sees it — an HTML comment and a fenced
// block hide what they hold — and fails for an assumption with no section, for a section that
// names no assumption of the list or one named twice, and for a section that lacks a part. The
// form it reads, which the page is written in:
//
//	### Assumption <n>: <what it assumes, in a few words>
//
//	<anything>
//
//	**Evidence in a recorded window.** <the evidence, on the label's own line>
//
//	**If it is wrong.**
//
//	- Issues finished: <the direction it moves that measure>
//	- Started work that waits for a slot: <...>
//	- Priority taken by holders: <...>
//	- Other delays: <...>
//	- Slots: <...>
//
//	**The reading that would test it.** <the reading, on the label's own line>
//
// A section runs from its heading to the next heading of level three or above; its parts may come
// in any order, and the direction's list is the bullets between its label and the next label or
// the section's end, each `- <measure>: <text>` with text after the colon.
func TestTheDocsPageGivesEveryAssumption(t *testing.T) {
	page, err := os.ReadFile(replayPage)
	if err != nil {
		t.Fatalf("read the tool's page in the docs, %s, which must give each assumption with its evidence, its direction for each measure and the reading that would test it: %v",
			replayPage, err)
	}
	sections := assumptionSections(t, visibleText(string(page)))
	for _, assumption := range Assumptions {
		section, given := sections[assumption.Number]
		if !given {
			t.Errorf("%s gives no section `### Assumption %d: …` for assumption %d: %s", replayPage, assumption.Number, assumption.Number, assumption.Text)
			continue
		}
		for _, missing := range sectionLacks(section) {
			t.Errorf("%s's section of assumption %d lacks %s", replayPage, assumption.Number, missing)
		}
	}
	for number := range sections {
		if !listsAssumption(number) {
			t.Errorf("%s gives a section of assumption %d, which Assumptions lacks", replayPage, number)
		}
	}
}

// fencedOrComment is a fenced code block or an HTML comment, which a reader of the rendered page
// does not read as the page's text.
var fencedOrComment = regexp.MustCompile("(?s)<!--.*?-->|(?m:^```[^\n]*\n)(?:.*?\n)?(?m:^```[ \t]*$)")

// visibleText is page with every fenced block and HTML comment blanked, line for line, so the
// sections are read as a reader of the rendered page reads them.
func visibleText(page string) string {
	return fencedOrComment.ReplaceAllStringFunc(page, func(hidden string) string {
		return strings.Repeat("\n", strings.Count(hidden, "\n"))
	})
}

// assumptionSections is each section of an assumption in page, by its number: its lines after the
// heading up to the next heading of level three or above. A number given twice fails the test.
func assumptionSections(t *testing.T, page string) map[int][]string {
	t.Helper()
	sections := make(map[int][]string)
	current := 0
	for _, line := range strings.Split(page, "\n") {
		if match := assumptionHeading.FindStringSubmatch(line); match != nil {
			number, err := strconv.Atoi(match[1])
			if err != nil {
				t.Fatalf("read the heading %q: %v", line, err)
			}
			if _, twice := sections[number]; twice {
				t.Errorf("%s gives two sections of assumption %d", replayPage, number)
			}
			sections[number] = []string{}
			current = number
			continue
		}
		if strings.HasPrefix(line, "# ") || strings.HasPrefix(line, "## ") || strings.HasPrefix(line, "### ") {
			current = 0
			continue
		}
		if current != 0 {
			sections[current] = append(sections[current], line)
		}
	}
	return sections
}

// sectionLacks is each part section does not give: the evidence, the reading, and the direction
// for each measure of R4.
func sectionLacks(section []string) []string {
	var lacks []string
	labelled := func(label string) bool {
		return slices.ContainsFunc(section, func(line string) bool {
			text, found := strings.CutPrefix(strings.TrimSpace(line), label)
			return found && strings.TrimSpace(text) != ""
		})
	}
	if !labelled(evidenceLabel) {
		lacks = append(lacks, "its evidence in a recorded window ("+evidenceLabel+" and the evidence after it)")
	}
	if !labelled(readingLabel) {
		lacks = append(lacks, "the reading that would test it ("+readingLabel+" and the reading after it)")
	}
	start := slices.IndexFunc(section, func(line string) bool { return strings.TrimSpace(line) == directionLabel })
	if start < 0 {
		return append(lacks, "its direction for each measure ("+directionLabel+" on a line of its own, then one bullet for each measure)")
	}
	given := make(map[string]bool)
	for _, line := range section[start+1:] {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "**") {
			break
		}
		bullet, isBullet := strings.CutPrefix(trimmed, "- ")
		if !isBullet {
			continue
		}
		if measure, direction, named := strings.Cut(bullet, ":"); named && strings.TrimSpace(direction) != "" {
			given[strings.TrimSpace(measure)] = true
		}
	}
	for _, measure := range r4Measures {
		if !given[measure] {
			lacks = append(lacks, "its direction for the measure "+measure+" (`- "+measure+": <direction>` under "+directionLabel+")")
		}
	}
	return lacks
}

// namedRunWindow is a small window whose replay rests on assumptions it must name, under a cap of
// one: ACME-1, a root, is promoted at 09:05 and runs through planning and implementing to testing,
// where its test fails at 10:45 and sends it back to implementing (a send-back loop kept as
// recorded, assumption 3), then through testing again to its review, which is under way at the
// window's end at 12:00 (work left, assumption 15). ACME-2, a root handed to Legion at 09:10, waits
// for the one slot all window and never starts (its work is the scenario's, assumption 8). Every
// segment keeps its recorded length at f 1 (assumption 1).
func namedRunWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(12, 0))
	b.capped(1)
	b.boot(clock(9, 0))
	b.file(clock(9, 5), "ACME-1", filing{Title: "Export the widget list", Status: "todo", Rank: "a0", Labels: handedToLegion})
	b.rootPromoted(clock(9, 5), "ACME-1")
	b.file(clock(9, 10), "ACME-2", filing{Title: "Count the widgets per shelf", Status: "todo", Rank: "a1", Labels: handedToLegion})
	b.rootDesigned(clock(9, 20), "ACME-1")
	b.segmentEnds(clock(9, 50), "ACME-1", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.segmentEnds(clock(10, 30), "ACME-1", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	b.segmentEnds(clock(10, 45), "ACME-1", phase.Testing, phase.Implementing, workflow.TriggerTesterFailed)
	b.segmentEnds(clock(11, 15), "ACME-1", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	b.segmentEnds(clock(11, 30), "ACME-1", phase.Testing, phase.Reviewing, workflow.TriggerTesterPassed)
	b.turnEnds(clock(11, 45), "ACME-1", claim.RoleReviewer)
	b.turnStarts(clock(11, 50), "ACME-1", claim.RoleReviewer)
	return b.build()
}

// boundWord is the word a run's output must never call a scenario of an issue's unseen work by.
var boundWord = regexp.MustCompile(`(?i)\bbounds?\b`)

// Each run's output names the assumptions it used and the scenario of R2 it gave the work an issue
// never showed, and never calls that scenario a bound (spec R2, R3). The test replays a small
// window (namedRunWindow) under the rule main on the main branch with the scenario `drawn`, and
// fails for a result whose scorecard does not name that scenario, or does not name, among the
// assumptions of the tool's list, the ones the window cannot be replayed without (1, 3, 8 and 15,
// namedRunWindow says why); for a scorecard.json in the run's output that names other assumptions
// or another scenario than the result; and for a file of the output other than the daemon's own
// log lines and the traced segments that calls anything a bound.
func TestEveryRunNamesItsAssumptionsAndScenario(t *testing.T) {
	dsn := replayDSN(t)
	rule := mainRule(t)
	branch, err := BranchNamed("main+neverStarted=drawn")
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	result, err := Run(context.Background(), RunConfig{Window: namedRunWindow(t), Rule: rule, Branch: branch, Seed: 1, Slowdown: 1,
		PGDSN: dsn, Out: out})
	if err != nil {
		t.Fatalf("replay the window under the rule main on the branch %s: %v", branch.Name, err)
	}
	card := result.Scorecard
	if card.Scenario != branch.NeverStarted {
		t.Errorf("the run names the scenario %q, want %q, its branch's", card.Scenario, branch.NeverStarted)
	}
	for _, number := range card.Assumptions {
		if !listsAssumption(number) {
			t.Errorf("the run names assumption %d, which Assumptions lacks", number)
		}
	}
	for _, used := range []int{1, 3, 8, 15} {
		if !slices.Contains(card.Assumptions, used) {
			t.Errorf("the run names the assumptions %v, which lack %d, one its window cannot be replayed without", card.Assumptions, used)
		}
	}

	written, err := os.ReadFile(filepath.Join(out, scorecardFile))
	if err != nil {
		t.Fatalf("read the run's scorecard.json: %v", err)
	}
	var printed Scorecard
	if err := json.Unmarshal(written, &printed); err != nil {
		t.Fatalf("read the run's scorecard.json: %v", err)
	}
	if printed.Scenario != card.Scenario || !slices.Equal(printed.Assumptions, card.Assumptions) {
		t.Errorf("the run's scorecard.json names the scenario %q and the assumptions %v, and its result %q and %v",
			printed.Scenario, printed.Assumptions, card.Scenario, card.Assumptions)
	}
	err = filepath.WalkDir(out, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || entry.Name() == logFile || entry.Name() == segmentsFile {
			return err
		}
		text, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if word := boundWord.Find(text); word != nil {
			t.Errorf("the run's %s calls something a %q: a run names its scenario and never calls it a bound", entry.Name(), word)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read the run's output: %v", err)
	}
}
