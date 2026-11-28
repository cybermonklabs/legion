package replay

//ci:reads docs/site/src/content/docs/legion/replay.md `packages/daemon/internal/replay/assumptions_test.go#TestTheDocsPageGivesEveryAssumption` reads replayPage under ../../../..

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

// specAssumptions is the number of each assumption the tool's list must hold: the eighteen in the
// spec's Design, the plan's 19 to 25, D10's 38 to 44 and the signal assumptions 45 to 49. Their
// words are the list's own (Assumptions), which the docs page gives
// (TestTheDocsPageGivesEveryAssumption), and the replay's tests hold what each says the tool does.
var specAssumptions = []int{
	1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18,
	19, 20, 21, 22, 23, 24, 25,
	38, 39, 40, 41, 42, 43, 44,
	45, 46, 47, 48, 49,
}

// The list must hold each numbered assumption above, and cannot repeat a number.
// TestTheDocsPageGivesEveryAssumption also checks assumptions the tool adds.
func TestTheAssumptionsHoldTheSpecsList(t *testing.T) {
	t.Parallel()
	listed := make(map[int]bool, len(Assumptions))
	for _, assumption := range Assumptions {
		if listed[assumption.Number] {
			t.Errorf("Assumptions numbers %d twice", assumption.Number)
		}
		listed[assumption.Number] = true
	}
	for _, number := range specAssumptions {
		if !listed[number] {
			t.Errorf("Assumptions lacks assumption %d", number)
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
	t.Parallel()
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
	t.Parallel()
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
