package replay

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
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// specAssumptions is the list a replay's assumptions start from: the eighteen of the tool's spec
// (its Design, "The assumptions"), each with what it assumes and the direction it moves a result if
// it is wrong, and the seven its plan adds, 19 to 25.
var specAssumptions = []Assumption{
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

// Every assumption a replay makes is the tool's to state (spec R3), starting from its spec's list:
// the tool's list (Assumptions) holds each of the spec's eighteen and the plan's seven by its
// number, with the text and the direction this test holds, and numbers none twice. It fails for a
// number the tool's list lacks and for a text or a direction that differs. A number the tool adds
// past these is the tool's own, which TestTheDocsPageGivesEveryAssumption still requires a section
// for.
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
		if !slices.ContainsFunc(Assumptions, func(a Assumption) bool { return a.Number == number }) {
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
	dsn := os.Getenv("LEGION_TEST_PG_DSN")
	if dsn == "" {
		t.Skip(testdb.SkipMessage)
	}
	rule, ok := record.SlotRuleNamed(record.MainRule.Name)
	if !ok {
		t.Fatal("no slot rule is named main")
	}
	branch := MainBranch
	branch.Name, branch.NeverStarted = "main+neverStarted=drawn", "drawn"
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
		if !slices.ContainsFunc(Assumptions, func(a Assumption) bool { return a.Number == number }) {
			t.Errorf("the run names assumption %d, which Assumptions lacks", number)
		}
	}
	for _, used := range []int{1, 3, 8, 15} {
		if !slices.Contains(card.Assumptions, used) {
			t.Errorf("the run names the assumptions %v, which lack %d, one its window cannot be replayed without", card.Assumptions, used)
		}
	}

	written, err := os.ReadFile(filepath.Join(out, "scorecard.json"))
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
		if err != nil || entry.IsDir() || entry.Name() == logFile || entry.Name() == "segments.jsonl" {
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
