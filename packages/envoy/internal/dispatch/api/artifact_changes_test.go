package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// GET /api/v1/artifacts/{id}/changes answers what changed between two versions of one
// document, read off their markdown as the one renderer answers it, with every ask block cut
// out: whether the cut is the whole change, whether nothing changed at all, each `##` section
// whose text differs or that moved, named by its heading and occurrence, each run of the text
// outside every `##` section that differs or moved, named under the heading "" with the place it
// stands at, and each typed block other than an ask whose text or any attribute differs, named
// by its id. The daemon reads it to tell the session holding an issue what an approved new
// version changed, so these tests read the answer as that caller reads the JSON, field by field.

// servedChanges is the change set as a caller decodes the route's JSON.
type servedChanges struct {
	ArtifactID   string           `json:"artifact_id"`
	From         int              `json:"from"`
	To           int              `json:"to"`
	DecisionOnly bool             `json:"decision_only"`
	Unchanged    bool             `json:"unchanged"`
	Sections     []servedSection  `json:"sections"`
	Blocks       []servedBlockRef `json:"blocks"`
}

// servedSection is one changed section, or one changed run of the text outside every section, as
// the route names it. The place fields are pointers so that a field the route leaves out reads
// as absent rather than as "" or 0.
type servedSection struct {
	Heading         string  `json:"heading"`
	Occurrence      int     `json:"occurrence"`
	Change          string  `json:"change"`
	Place           string  `json:"place,omitempty"`
	AfterHeading    *string `json:"after_heading,omitempty"`
	AfterOccurrence *int    `json:"after_occurrence,omitempty"`
}

// String is the entry as the route's JSON spells it, so a failure prints the place a run stands
// at rather than the addresses its pointers hold.
func (section servedSection) String() string {
	encoded, err := json.Marshal(section)
	if err != nil {
		return fmt.Sprintf("unencodable section %q: %v", section.Heading, err)
	}
	return string(encoded)
}

// runBeforeTheFirstSection is the run of text outside every section that stands above the first
// `##` section, named with change.
func runBeforeTheFirstSection(change string) servedSection {
	return servedSection{Heading: "", Occurrence: 0, Change: change, Place: "before_first_section"}
}

// runAfterSection is the run of text outside every section that stands directly after the `##`
// section heading, occurrence, named with change.
func runAfterSection(heading string, occurrence int, change string) servedSection {
	return servedSection{
		Heading: "", Occurrence: 0, Change: change,
		Place: "after_section", AfterHeading: &heading, AfterOccurrence: &occurrence,
	}
}

// servedBlockRef is one changed typed block as the route names it.
type servedBlockRef struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Change string `json:"change"`
}

// changesPath is the route's address for one pair of versions of a document.
func changesPath(artifactID string, from, to int) string {
	return fmt.Sprintf("/api/v1/artifacts/%s/changes?from=%d&to=%d", artifactID, from, to)
}

// readChanges reads the change set of one pair of versions as alice, failing t unless the route
// answers 200, and logs the answer whole so a run with -v shows the JSON a caller is handed.
func readChanges(t *testing.T, handler http.Handler, artifactID string, from, to int) servedChanges {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, changesPath(artifactID, from, to), nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("changes from %d to %d: status=%d body=%s", from, to, response.Code, response.Body.String())
	}
	t.Logf("GET %s answered %s", changesPath(artifactID, from, to), response.Body.String())
	return decodeBody[servedChanges](t, response)
}

// uploadDocumentVersions uploads each text in turn as one document of a new issue, so text i is
// version i+1, and answers the document's id.
func uploadDocumentVersions(t *testing.T, handler http.Handler, name string, texts ...string) string {
	t.Helper()
	return uploadVersionsTo(t, handler, createArtifactIssue(t, handler).Key, name, texts...)
}

// uploadVersionsTo uploads each text in turn as one document of the issue issueKey, which holds
// no document of that name yet, so text i is version i+1, and answers the document's id.
func uploadVersionsTo(t *testing.T, handler http.Handler, issueKey, name string, texts ...string) string {
	t.Helper()
	var artifactID string
	for index, text := range texts {
		uploaded := uploadDocument(t, handler, "/api/v1/issues/"+issueKey, name, text)
		if uploaded.Version.Number != index+1 {
			t.Fatalf("upload %d of %s = version %d, want %d", index+1, name, uploaded.Version.Number, index+1)
		}
		artifactID = uploaded.Artifact.ID
	}
	return artifactID
}

// requireChanges fails t unless the change set names exactly sections and blocks, and is neither
// ask-only (decision_only) nor unchanged.
func requireChanges(t *testing.T, changes servedChanges, sections []servedSection, blocks []servedBlockRef) {
	t.Helper()
	if changes.DecisionOnly || changes.Unchanged {
		t.Fatalf("decision_only = %t, unchanged = %t, want both false for a change set that names what changed: %+v",
			changes.DecisionOnly, changes.Unchanged, changes)
	}
	if !reflect.DeepEqual(changes.Sections, sections) {
		t.Fatalf("sections = %v, want %v", changes.Sections, sections)
	}
	if !reflect.DeepEqual(changes.Blocks, blocks) {
		t.Fatalf("blocks = %+v, want %+v", changes.Blocks, blocks)
	}
}

// A section whose words changed is named by its heading, a section only the later version holds
// is added and one only the earlier holds is removed; a section left as it was, and the title
// above every section, are not named. The answer names the document and the versions it read.
func TestTheChangeSetNamesAChangedSectionByItsHeading(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	artifactID := uploadDocumentVersions(t, handler, "plan.md",
		"# Plan\n\n## Goal\n\nShip the service behind a flag.\n\n## Scope\n\nTwo regions.\n\n## Risks\n\nNone known.\n",
		"# Plan\n\n## Goal\n\nShip the service behind a flag and a canary.\n\n## Scope\n\nTwo regions.\n\n## Rollout\n\nOne region a day.\n")

	changes := readChanges(t, handler, artifactID, 1, 2)
	if changes.ArtifactID != artifactID || changes.From != 1 || changes.To != 2 {
		t.Fatalf("the answer names artifact %q versions %d to %d, want %q versions 1 to 2",
			changes.ArtifactID, changes.From, changes.To, artifactID)
	}
	requireChanges(t, changes, []servedSection{
		{Heading: "Goal", Occurrence: 0, Change: "changed"},
		{Heading: "Rollout", Occurrence: 0, Change: "added"},
		{Heading: "Risks", Occurrence: 0, Change: "removed"},
	}, []servedBlockRef{})
}

// A change to the text above the first `##` section, and to nothing else, is named: that run of
// the text outside every `##` section is named under the heading "" and occurrence 0, at its
// place before the first section, and the `##` section after it, left as it was, is not named.
func TestAChangeBeforeTheFirstSectionNamesTheRunBeforeTheFirstSection(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	artifactID := uploadDocumentVersions(t, handler, "plan.md",
		"# Plan\n\nThe plan ships in May.\n\n## Goal\n\nShip the service behind a flag.\n",
		"# Plan\n\nThe plan ships in June.\n\n## Goal\n\nShip the service behind a flag.\n")

	requireChanges(t, readChanges(t, handler, artifactID, 1, 2),
		[]servedSection{runBeforeTheFirstSection("changed")}, []servedBlockRef{})
}

// A requirement only the later version holds is added, and one only the earlier holds is
// removed; a requirement left as it was is not named. The section holding them changed with
// them, and is named too.
func TestTheChangeSetNamesAnAddedAndARemovedRequirement(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	artifactID := uploadDocumentVersions(t, handler, "requirements.md",
		"# Spec\n\n## Requirements\n\n:::requirement{#r1}\nThe read succeeds.\n:::\n\n:::requirement{#r2}\nThe read is fast.\n:::\n",
		"# Spec\n\n## Requirements\n\n:::requirement{#r1}\nThe read succeeds.\n:::\n\n:::requirement{#r3}\nThe read is logged.\n:::\n")

	requireChanges(t, readChanges(t, handler, artifactID, 1, 2),
		[]servedSection{{Heading: "Requirements", Occurrence: 0, Change: "changed"}},
		[]servedBlockRef{
			{ID: "r3", Type: "requirement", Change: "added"},
			{ID: "r2", Type: "requirement", Change: "removed"},
		})
}

// A check whose `run` changed, one whose words changed, and one that names another CI run in its
// `ci_check` are each named changed; a check left as it was is not. The checks stand outside every
// `##` section, and the document has none, so its whole text is one run before the first section,
// the one section named.
func TestTheChangeSetNamesACheckWhoseRunOrWordsChanged(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	const ciControl = `negative_control="Break the build and watch the run fail."`
	check := func(id, attributes, words string) string {
		return ":::check{#" + id + " " + attributes + "}\n" + words + "\n:::\n\n"
	}
	artifactID := uploadDocumentVersions(t, handler, "verification.md",
		check("k1", `surface="cli" run="go test ./a/"`, "The read returns 200.")+
			check("k2", `surface="api" run="go test ./b/"`, "The write returns 201.")+
			check("k3", ciControl+` ci_check="build"`, "The server builds at the head.")+
			check("k4", `surface="cli" run="go test ./d/"`, "The list is sorted."),
		check("k1", `surface="cli" run="go test ./a/ -run TestRead"`, "The read returns 200.")+
			check("k2", `surface="api" run="go test ./b/"`, "The write returns 201 with the row.")+
			check("k3", ciControl+` ci_check="test"`, "The server builds at the head.")+
			check("k4", `surface="cli" run="go test ./d/"`, "The list is sorted."))

	requireChanges(t, readChanges(t, handler, artifactID, 1, 2), []servedSection{runBeforeTheFirstSection("changed")}, []servedBlockRef{
		{ID: "k1", Type: "check", Change: "changed"},
		{ID: "k2", Type: "check", Change: "changed"},
		{ID: "k3", Type: "check", Change: "changed"},
	})
}

// decisionSpec is a spec with one section of prose and an ask block in a section of its own. The
// tests of an answered ask answer that block.
const decisionSpec = "# Plan\n\n## Rollout\n\nThe service ships behind a flag.\n\n## Decisions\n\n" + waveAsk

// answeredDecisionSpec writes decisionSpec into a new issue's spec, has alice answer its ask
// block, and waits for the version settlement writes her answer into: version 2.
func answeredDecisionSpec(t *testing.T, handler http.Handler) string {
	t.Helper()
	issue := createInteractionIssue(t, handler, "TEST", "Change set of an answer", decisionSpec)
	asks := indexedDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, waveBlock)
	answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "", nil)
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 2)
	if versionMarkdown(t, handler, issue.PrimaryArtifactID, 1) == versionMarkdown(t, handler, issue.PrimaryArtifactID, 2) {
		t.Fatal("version 2 holds version 1's text: the answer never reached the stored document")
	}
	return issue.PrimaryArtifactID
}

// An answer a person gave to an ask block is the whole difference between the version before
// it and the version it wrote: the change set is ask-only, and names no section and no block,
// the section that holds the block included.
func TestAnAnsweredAskAloneIsADecisionOnlyChange(t *testing.T) {
	t.Parallel()
	handler := approvalCarryHandler(t)
	artifactID := answeredDecisionSpec(t, handler)

	changes := readChanges(t, handler, artifactID, 1, 2)
	if !changes.DecisionOnly {
		t.Fatalf("decision_only = false, want true: %+v", changes)
	}
	if len(changes.Sections) != 0 || len(changes.Blocks) != 0 || changes.Sections == nil || changes.Blocks == nil {
		t.Fatalf("sections = %#v, blocks = %#v, want both empty lists", changes.Sections, changes.Blocks)
	}
}

// An answer beside a change to a section's words is no ask-only change: the changed section
// is named, and neither the ask block nor the section holding it is.
func TestAnAskAnsweredBesideAChangedSectionIsNoDecisionOnlyChange(t *testing.T) {
	t.Parallel()
	handler := approvalCarryHandler(t)
	artifactID := answeredDecisionSpec(t, handler)
	reviseDocument(t, handler, artifactID, "behind a flag.", "behind a flag and a canary.", 3)

	requireChanges(t, readChanges(t, handler, artifactID, 1, 3),
		[]servedSection{{Heading: "Rollout", Occurrence: 0, Change: "changed"}}, []servedBlockRef{})
}

// Two sections under one heading are two sections, told apart by how many before each carry that
// heading: a change to the second names occurrence 1 and leaves the first unnamed.
func TestTwoSectionsOfOneHeadingAreToldApartByOccurrence(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	artifactID := uploadDocumentVersions(t, handler, "steps.md",
		"# Steps\n\n## Step\n\nBuild it.\n\n## Step\n\nShip it.\n",
		"# Steps\n\n## Step\n\nBuild it.\n\n## Step\n\nShip it on a Tuesday.\n")

	requireChanges(t, readChanges(t, handler, artifactID, 1, 2),
		[]servedSection{{Heading: "Step", Occurrence: 1, Change: "changed"}}, []servedBlockRef{})
}

// The `##` sections the reorder tests put in one order or another, each holding the same text
// wherever it stands.
const (
	goalSection    = "## Goal\n\nShip the service behind a flag.\n"
	scopeSection   = "## Scope\n\nTwo regions.\n"
	risksSection   = "## Risks\n\nNone known.\n"
	rolloutSection = "## Rollout\n\nOne region a day.\n"
)

// sectionedPlan is a plan under its title holding sections, in the order given.
func sectionedPlan(sections ...string) string {
	return "# Plan\n\n" + strings.Join(sections, "\n")
}

// A version that only swaps two sections changes the document, so it is no ask-only change,
// and it names exactly one of the two moved: the one the later version holds second, the other
// keeping its place among the sections both versions hold. The section after them is not named.
func TestTheChangeSetNamesOneOfTwoSwappedSectionsMoved(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	artifactID := uploadDocumentVersions(t, handler, "plan.md",
		sectionedPlan(goalSection, scopeSection, risksSection),
		sectionedPlan(scopeSection, goalSection, risksSection))

	requireChanges(t, readChanges(t, handler, artifactID, 1, 2),
		[]servedSection{{Heading: "Goal", Occurrence: 0, Change: "moved"}}, []servedBlockRef{})
}

// A section lifted from last to first is the only section named: the three it passed keep their
// order, so none of them moved, though each stands one place lower.
func TestTheChangeSetNamesASectionLiftedToTheTopAsTheOnlyOneMoved(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	artifactID := uploadDocumentVersions(t, handler, "plan.md",
		sectionedPlan(goalSection, scopeSection, risksSection, rolloutSection),
		sectionedPlan(rolloutSection, goalSection, scopeSection, risksSection))

	requireChanges(t, readChanges(t, handler, artifactID, 1, 2),
		[]servedSection{{Heading: "Rollout", Occurrence: 0, Change: "moved"}}, []servedBlockRef{})
}

// A section that moved and whose text changed is named changed, once, and never moved: its text
// is what a reader must read again. Two pairs of sections swap here, and the one moved of the
// first pair is the one whose words changed, while the one moved of the second kept its words and
// is named moved.
func TestAMovedSectionWhoseTextChangedIsNamedChangedNotMoved(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	artifactID := uploadDocumentVersions(t, handler, "plan.md",
		sectionedPlan(goalSection, scopeSection, risksSection, rolloutSection),
		sectionedPlan(scopeSection, "## Goal\n\nShip the service behind a flag and a canary.\n", rolloutSection, risksSection))

	requireChanges(t, readChanges(t, handler, artifactID, 1, 2), []servedSection{
		{Heading: "Goal", Occurrence: 0, Change: "changed"},
		{Heading: "Risks", Occurrence: 0, Change: "moved"},
	}, []servedBlockRef{})
}

// A reorder beside an added section names both, in the later version's order: the moved section
// where the later version holds it, and the added one after it.
func TestTheChangeSetNamesAReorderBesideAnAddedSection(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	artifactID := uploadDocumentVersions(t, handler, "plan.md",
		sectionedPlan(goalSection, scopeSection, risksSection),
		sectionedPlan(scopeSection, goalSection, rolloutSection, risksSection))

	requireChanges(t, readChanges(t, handler, artifactID, 1, 2), []servedSection{
		{Heading: "Goal", Occurrence: 0, Change: "moved"},
		{Heading: "Rollout", Occurrence: 0, Change: "added"},
	}, []servedBlockRef{})
}

// appendixRun is a top-level `#` heading with its text. What it opens is text outside every `##`
// section, one run of it, standing wherever the heading stands among the sections.
const appendixRun = "# Appendix\n\nBackground.\n"

// setextScopeSection is a `##` section holding words, whose heading holds a line break. The
// renderer writes such a heading in setext form, its two lines and a `---` underline, as the
// browser editor writes it (renderer.heading), so the stored text writes no `#` for it.
func setextScopeSection(words string) string {
	return "Scope\\\nwide\n---\n\n" + words + "\n"
}

// setextAppendixRun is a top-level `#` heading holding a line break, with words under it: the
// setext form underlines it `===`. What it opens is text outside every `##` section, as what
// appendixRun's heading opens is.
func setextAppendixRun(words string) string {
	return "Appendix\\\nnotes\n===\n\n" + words + "\n"
}

// A heading that holds a line break cuts the document as every heading of its level does, though
// the store writes it in setext form, with no `#`. A `##` one opens a section, named by its
// heading's text with the line break a line feed, so a change under it is named under it and not
// under the section before it. A `#` one closes the section before it, so a change under it is the
// run of text after that section, and that section, left as it was, is not named. The first read
// holds the test to the setext form: a store that wrote these headings with `#`s would test
// nothing here.
func TestAChangeUnderASetextHeadingIsNamedUnderThatHeading(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	artifactID := uploadDocumentVersions(t, handler, "plan.md",
		sectionedPlan(goalSection, setextScopeSection("Two regions."), risksSection, setextAppendixRun("Background.")),
		sectionedPlan(goalSection, setextScopeSection("Three regions."), risksSection, setextAppendixRun("Background.")),
		sectionedPlan(goalSection, setextScopeSection("Three regions."), risksSection, setextAppendixRun("Background and history.")))
	stored := versionMarkdown(t, handler, artifactID, 1)
	if !strings.Contains(stored, "\nScope\\\nwide\n---\n") || !strings.Contains(stored, "\nAppendix\\\nnotes\n===\n") {
		t.Fatalf("version 1 is stored as %q, want both headings in setext form", stored)
	}

	requireChanges(t, readChanges(t, handler, artifactID, 1, 2),
		[]servedSection{{Heading: "Scope\nwide", Occurrence: 0, Change: "changed"}}, []servedBlockRef{})
	requireChanges(t, readChanges(t, handler, artifactID, 2, 3),
		[]servedSection{runAfterSection("Risks", 0, "changed")}, []servedBlockRef{})
}

// A section's place is among every top-level unit of the document: the `##` sections and each run
// of text outside every section, which a `#` heading opens. Each run is named on its own, at the
// place it stands. So a `# Appendix` run lifted above a section, its words the same, is that run
// moved, named after the section the later version holds it after. Lifted to the top, it joins
// the title's run: that run's text changed before the first section, and the run the earlier
// version held after Risks is removed. Lifted with its words changed, it shares neither its text
// nor its place with a run of the earlier version, so it is added where it now stands and the
// earlier run is removed where it stood. A section moved across the heading swaps places with the
// run, and of the two the one the later version holds second is named: the run, after Scope, when
// Scope moves above the heading, and Scope when it moves below. A swap or a lift of `##` sections
// beside such a run names the section it names without one, and the run keeps its place among the
// units, so it is not named although another section now stands before it. Each case is one pair
// of consecutive versions of one document, compared on its own.
func TestATopLevelHeadingLiftedAboveASectionNamesEachRunWhereItStands(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		before, after string
		want          []servedSection
	}{
		{
			name:   "two sections swapped beside a `#` heading",
			before: sectionedPlan(goalSection, scopeSection, appendixRun, risksSection),
			after:  sectionedPlan(scopeSection, goalSection, appendixRun, risksSection),
			want:   []servedSection{{Heading: "Goal", Occurrence: 0, Change: "moved"}},
		},
		{
			name:   "a section moved below a `#` heading",
			before: sectionedPlan(goalSection, scopeSection, appendixRun, risksSection),
			after:  sectionedPlan(goalSection, appendixRun, scopeSection, risksSection),
			want:   []servedSection{{Heading: "Scope", Occurrence: 0, Change: "moved"}},
		},
		{
			name:   "a `#` heading lifted above two sections",
			before: sectionedPlan(goalSection, scopeSection, risksSection, appendixRun),
			after:  sectionedPlan(goalSection, appendixRun, scopeSection, risksSection),
			want:   []servedSection{runAfterSection("Goal", 0, "moved")},
		},
		{
			name:   "a `#` heading lifted to the top, joining the title's run",
			before: sectionedPlan(goalSection, scopeSection, risksSection, appendixRun),
			after:  sectionedPlan(appendixRun, goalSection, scopeSection, risksSection),
			want:   []servedSection{runBeforeTheFirstSection("changed"), runAfterSection("Risks", 0, "removed")},
		},
		{
			name:   "a `#` heading lifted with its words changed",
			before: sectionedPlan(goalSection, scopeSection, risksSection, appendixRun),
			after:  sectionedPlan(goalSection, "# Appendix\n\nMore background.\n", scopeSection, risksSection),
			want:   []servedSection{runAfterSection("Goal", 0, "added"), runAfterSection("Risks", 0, "removed")},
		},
		{
			name:   "a section moved above a `#` heading",
			before: sectionedPlan(goalSection, appendixRun, scopeSection, risksSection),
			after:  sectionedPlan(goalSection, scopeSection, appendixRun, risksSection),
			want:   []servedSection{runAfterSection("Scope", 0, "moved")},
		},
		{
			name:   "a section lifted from last to first beside a `#` heading",
			before: sectionedPlan(goalSection, scopeSection, risksSection, rolloutSection, appendixRun),
			after:  sectionedPlan(rolloutSection, goalSection, scopeSection, risksSection, appendixRun),
			want:   []servedSection{{Heading: "Rollout", Occurrence: 0, Change: "moved"}},
		},
	}
	texts := make([]string, 0, 2*len(cases))
	for _, tc := range cases {
		texts = append(texts, tc.before, tc.after)
	}
	handler := newTestHandler(t)
	artifactID := uploadDocumentVersions(t, handler, "plan.md", texts...)

	for index, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requireChanges(t, readChanges(t, handler, artifactID, 2*index+1, 2*index+2), tc.want, []servedBlockRef{})
		})
	}
}

// Each run of text outside every `##` section is named at the place it stands, so a reader is
// sent to that run and not to the top of the document. A `#` heading whose words changed where it
// stands is one run changed after the section before it; one added between two sections is added
// there; one removed is removed after the section the earlier version held it after. Two runs
// that change at once are each named, in document order: the title's before the first section,
// then the `#` heading's after its section. A `#` heading the document holds twice, with the same
// words, is two runs: removing one copy names that copy removed where it stood, and adding a copy
// names it added where it now stands, and the other copy, which keeps its words and its place, is
// not named, nor is any section that kept its place.
func TestTheChangeSetNamesEachRunOutsideTheSectionsWhereItStands(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		before, after string
		want          []servedSection
	}{
		{
			name:   "a `#` heading's words changed where it stands",
			before: sectionedPlan(goalSection, appendixRun, scopeSection),
			after:  sectionedPlan(goalSection, "# Appendix\n\nMore background.\n", scopeSection),
			want:   []servedSection{runAfterSection("Goal", 0, "changed")},
		},
		{
			name:   "a `#` heading added between two sections",
			before: sectionedPlan(goalSection, scopeSection),
			after:  sectionedPlan(goalSection, appendixRun, scopeSection),
			want:   []servedSection{runAfterSection("Goal", 0, "added")},
		},
		{
			name:   "a `#` heading removed after the last section",
			before: sectionedPlan(goalSection, scopeSection, appendixRun),
			after:  sectionedPlan(goalSection, scopeSection),
			want:   []servedSection{runAfterSection("Scope", 0, "removed")},
		},
		{
			name:   "the title and a `#` heading's words changed together",
			before: sectionedPlan(goalSection, appendixRun, scopeSection),
			after:  "# Plan for May\n\n" + goalSection + "\n# Appendix\n\nMore background.\n\n" + scopeSection,
			want:   []servedSection{runBeforeTheFirstSection("changed"), runAfterSection("Goal", 0, "changed")},
		},
		{
			name:   "one of two copies of a `#` heading removed",
			before: sectionedPlan(goalSection, appendixRun, scopeSection, appendixRun, risksSection),
			after:  sectionedPlan(goalSection, scopeSection, appendixRun, risksSection),
			want:   []servedSection{runAfterSection("Goal", 0, "removed")},
		},
		{
			name:   "a second copy of a `#` heading added above the first",
			before: sectionedPlan(goalSection, scopeSection, appendixRun, risksSection),
			after:  sectionedPlan(goalSection, appendixRun, scopeSection, appendixRun, risksSection),
			want:   []servedSection{runAfterSection("Goal", 0, "added")},
		},
	}
	texts := make([]string, 0, 2*len(cases))
	for _, tc := range cases {
		texts = append(texts, tc.before, tc.after)
	}
	handler := newTestHandler(t)
	artifactID := uploadDocumentVersions(t, handler, "plan.md", texts...)

	for index, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requireChanges(t, readChanges(t, handler, artifactID, 2*index+1, 2*index+2), tc.want, []servedBlockRef{})
		})
	}
}

// R2 of dispatch://INFRA-175 sends the session holding the work a task naming what an approved new
// version changed, read off this change set. decision_only (ask blocks alone) tells nobody anything
// of the work, and unchanged is no change at all; neither sends a task. Every other change set
// must name a section or a block, since one that names neither tells the session the document
// changed and nothing of where to read. This test drives the route over each kind of pair this
// tree has met, and holds each to that invariant - exactly one of ask-only, unchanged, and
// at least one item named - and to the one of the three its case must be. The store renders every
// upload, so a version that differs from the one before it in blank lines alone is stored as the
// same text, and is unchanged as an identical version is.
func TestTheChangeSetEitherNamesWhatChangedOrReportsNoChange(t *testing.T) {
	t.Parallel()
	const (
		decisionOnly = "ask-only"
		unchanged    = "unchanged"
		named        = "named"
	)
	requirements := func(words string) string {
		return "# Spec\n\n## Requirements\n\n:::requirement{#r1}\n" + words + "\n:::\n"
	}
	cases := []struct {
		name          string
		before, after string
		want          string
	}{
		{
			name:   "an identical version",
			before: sectionedPlan(goalSection, scopeSection, risksSection),
			after:  sectionedPlan(goalSection, scopeSection, risksSection),
			want:   unchanged,
		},
		{
			name:   "a version that differs in blank lines alone",
			before: sectionedPlan(goalSection, scopeSection, risksSection),
			after:  "# Plan\n\n\n\n" + goalSection + "\n\n\n" + scopeSection + "\n\n" + risksSection + "\n\n\n",
			want:   unchanged,
		},
		{
			name:   "two sections swapped",
			before: sectionedPlan(goalSection, scopeSection, risksSection),
			after:  sectionedPlan(scopeSection, goalSection, risksSection),
			want:   named,
		},
		{
			name:   "a section lifted to the top",
			before: sectionedPlan(goalSection, scopeSection, risksSection, rolloutSection),
			after:  sectionedPlan(rolloutSection, goalSection, scopeSection, risksSection),
			want:   named,
		},
		{
			name:   "a section moved across a `#` heading",
			before: sectionedPlan(goalSection, appendixRun, scopeSection, risksSection),
			after:  sectionedPlan(goalSection, scopeSection, appendixRun, risksSection),
			want:   named,
		},
		{
			name:   "a `#` heading lifted above two sections",
			before: sectionedPlan(goalSection, scopeSection, risksSection, appendixRun),
			after:  sectionedPlan(goalSection, appendixRun, scopeSection, risksSection),
			want:   named,
		},
		{
			name:   "a `#` heading lifted to the top",
			before: sectionedPlan(goalSection, scopeSection, risksSection, appendixRun),
			after:  sectionedPlan(appendixRun, goalSection, scopeSection, risksSection),
			want:   named,
		},
		{
			name:   "a reworded requirement",
			before: requirements("The read succeeds."),
			after:  requirements("The read succeeds at once."),
			want:   named,
		},
		{
			name:   "an added section",
			before: sectionedPlan(goalSection, scopeSection),
			after:  sectionedPlan(goalSection, scopeSection, risksSection),
			want:   named,
		},
		{
			name:   "one of two copies of a `#` heading removed",
			before: sectionedPlan(goalSection, appendixRun, scopeSection, appendixRun, risksSection),
			after:  sectionedPlan(goalSection, scopeSection, appendixRun, risksSection),
			want:   named,
		},
		{
			name:   "a second copy of a `#` heading added",
			before: sectionedPlan(goalSection, scopeSection, appendixRun, risksSection),
			after:  sectionedPlan(goalSection, appendixRun, scopeSection, appendixRun, risksSection),
			want:   named,
		},
	}
	// state is which of the three a change set is, failing t when it is more than one or none.
	state := func(t *testing.T, changes servedChanges) string {
		t.Helper()
		namesSome := len(changes.Sections)+len(changes.Blocks) > 0
		switch {
		case changes.DecisionOnly && (changes.Unchanged || namesSome), changes.Unchanged && namesSome:
			t.Fatalf("the change set is more than one of ask-only, unchanged and naming what changed: %+v", changes)
		case changes.DecisionOnly:
			return decisionOnly
		case changes.Unchanged:
			return unchanged
		case !namesSome:
			t.Fatalf("the change set is neither ask-only nor unchanged and names no section and no block, so it says the document changed and nothing of where: %+v", changes)
		}
		return named
	}
	handler := approvalCarryHandler(t)
	decided := answeredDecisionSpec(t, handler)
	texts := make([]string, 0, 2*len(cases))
	for _, tc := range cases {
		texts = append(texts, tc.before, tc.after)
	}
	artifactID := uploadVersionsTo(t, handler, createPlainIssue(t, handler, "CHG"), "plan.md", texts...)

	t.Run("an answered ask block", func(t *testing.T) {
		if got := state(t, readChanges(t, handler, decided, 1, 2)); got != decisionOnly {
			t.Fatalf("the change set is %s, want %s", got, decisionOnly)
		}
	})
	for index, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := state(t, readChanges(t, handler, artifactID, 2*index+1, 2*index+2)); got != tc.want {
				t.Fatalf("the change set is %s, want %s", got, tc.want)
			}
		})
	}
}

// Every block the change set names carries an id its own stored version names. The parse gives a
// block that repeats an earlier block's id a fresh id of its own, and a typed block that shares its
// id with an ask before it is one such block, so a version that holds either is read with ids in
// neither document. No block is named under such an id: named, it would be an addition and a
// removal of blocks neither version holds. The block whose id the stored text does name is
// compared under it as any other. The texts are compared as the route compares two stored
// versions (compareVersions): no write path stores a repeated id, so none can be uploaded.
func TestAChangeSetNamesNoBlockUnderAnIDNeitherStoredVersionCarries(t *testing.T) {
	t.Parallel()
	requirement := func(id, words string) string { return ":::requirement{#" + id + "}\n" + words + "\n:::\n\n" }
	for _, tc := range []struct {
		name          string
		before, after string
		want          []artifactBlockChange
	}{
		{
			name: "a repeated requirement id",
			before: "# Spec\n\n## Requirements\n\n" + requirement("r1", "The read succeeds.") +
				requirement("r1", "The read is fast."),
			after: "# Spec\n\n## Requirements\n\n" + requirement("r1", "The read succeeds at once.") +
				requirement("r1", "The read is fast."),
			want: []artifactBlockChange{{ID: "r1", Type: "requirement", Change: "changed"}},
		},
		{
			name:   "a requirement that shares its id with an ask",
			before: "# Spec\n\n## Decisions\n\n" + waveAsk + "\n" + requirement("wave", "The read succeeds."),
			after: "# Spec\n\n## Decisions\n\n" + waveAsk + "\n" + requirement("wave", "The read succeeds at once.") +
				requirement("r2", "The read is logged."),
			want: []artifactBlockChange{{ID: "r2", Type: "requirement", Change: "added"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changes, err := compareVersions(tc.before, tc.after)
			if err != nil {
				t.Fatalf("compare the two versions: %v", err)
			}
			if !reflect.DeepEqual(changes.Blocks, tc.want) {
				t.Fatalf("the change set names the blocks %+v, want %+v: every id named must be one a stored version carries", changes.Blocks, tc.want)
			}
		})
	}
}

// A pair whose from is not below its to is refused 400 INVALID_VERSION, as a version number that
// is not positive is, and the refusal is the error envelope every artifact route answers.
func TestTheChangeSetRefusesFromNotBelowTo(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	artifactID := uploadDocumentVersions(t, handler, "plan.md", "# Plan\n\nOne.\n", "# Plan\n\nTwo.\n")

	for _, query := range []string{"from=2&to=2", "from=2&to=1", "from=0&to=2", "from=one&to=2", "to=2"} {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/changes?"+query, nil, "alice")
		if response.Code != http.StatusBadRequest {
			t.Fatalf("?%s: status=%d body=%s, want 400", query, response.Code, response.Body.String())
		}
		if refusal := decodeBody[refusedAnswer](t, response); refusal.Code != "INVALID_VERSION" || refusal.Error == "" {
			t.Fatalf("?%s: refusal = %+v, want INVALID_VERSION with a message", query, refusal)
		}
	}
}

// A version the document does not have is 404 NOT_FOUND, the answer GET …/versions/{number}
// gives it, and the version it names is in the message; an id that names no artifact is the 404
// ARTIFACT_NOT_FOUND every artifact route answers.
func TestTheChangeSetAnswersAMissingVersionNotFound(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	artifactID := uploadDocumentVersions(t, handler, "plan.md", "# Plan\n\nOne.\n", "# Plan\n\nTwo.\n")

	missing := dispatchRequest(t, handler, http.MethodGet, changesPath(artifactID, 1, 3), nil, "alice")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("version 3 of a document of two: status=%d body=%s, want 404", missing.Code, missing.Body.String())
	}
	if refusal := decodeBody[refusedAnswer](t, missing); refusal.Code != "NOT_FOUND" || refusal.Error != "the document has no version 3" {
		t.Fatalf("version 3 of a document of two: refusal = %+v, want NOT_FOUND naming version 3", refusal)
	}
	if direct := dispatchRequest(t, handler, http.MethodGet, fmt.Sprintf("/api/v1/artifacts/%s/versions/3", artifactID), nil, "alice"); direct.Code != http.StatusNotFound || decodeBody[refusedAnswer](t, direct).Code != "NOT_FOUND" {
		t.Fatalf("the version route on version 3: status=%d body=%s, want the same 404 NOT_FOUND", direct.Code, direct.Body.String())
	}

	unknown := dispatchRequest(t, handler, http.MethodGet, changesPath("00000000-0000-0000-0000-00000000c0de", 1, 2), nil, "alice")
	if unknown.Code != http.StatusNotFound || decodeBody[refusedAnswer](t, unknown).Code != "ARTIFACT_NOT_FOUND" {
		t.Fatalf("an id that names no artifact: status=%d body=%s, want 404 ARTIFACT_NOT_FOUND", unknown.Code, unknown.Body.String())
	}
}

// The change set reads each of its two versions as the one renderer answers it for the caller
// (docs.RenderVersion), the text GET …/versions/{number} answers, and never a version's stored
// markdown past it. The plan of an issue born converted renders each of its versions from the
// tree, as of the version's time, and the change set of each pair of them is the one
// compareVersions makes of the two renders.
//
// No route reaches a rendered version whose stored markdown is not its render: the render leaves
// out no node for any reader (docs.RenderAt), the pass that stated the version recorded its
// version_renders row because its render as of the row's time was its stored markdown byte for
// byte, and every tuple a later write appends is dated after that time. So the test writes the
// stored markdown of the plan's version 2 by SQL, as no route can, to tell the two reads apart:
// the change sets from 1 to 2 and from 2 to 3 are still those of the renders, and those the
// stored texts make are not. A read of the stored markdown fails here, naming version 2 and the
// section only its stored text holds.
//
// A document no query covers, notes.md, holds no version_renders row: each of its versions answers
// its stored bytes, and its change set is the one those bytes make, as before. A version that
// stores no markdown (docs.NoBytes) is compared as the empty text, as the read of the stored
// column, a null coalesced to the empty string, compared it.
func TestTheChangeSetReadsEachVersionAsTheOneRendererAnswersIt(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createArtifactIssue(t, handler)
	plan := uploadVersionsTo(t, handler, issue.Key, "plan.md",
		"# Plan\n\n## Goal\n\nShip the service.\n",
		"# Plan\n\n## Goal\n\nShip the service behind a flag.\n\n## Rollout\n\nOne region a day.\n",
		"# Plan\n\n## Goal\n\nShip the service behind a flag.\n\n## Rollout\n\nTwo regions a day.\n")
	for number := 1; number <= 3; number++ {
		if rendering := renderedVersion(t, database, plan, number); rendering.Source != docs.RenderedFromTree {
			t.Fatalf("version %d of plan.md answers %q from %s, want a render from the tree, or this case proves nothing",
				number, rendering.Markdown, rendering.Source)
		}
	}
	requireChanges(t, readChanges(t, handler, plan, 1, 2), []servedSection{
		{Heading: "Goal", Occurrence: 0, Change: "changed"},
		{Heading: "Rollout", Occurrence: 0, Change: "added"},
	}, []servedBlockRef{})
	requireRenderedChanges(t, handler, database, plan, 1, 2)
	requireRenderedChanges(t, handler, database, plan, 2, 3)

	ctx := context.Background()
	render := renderedVersion(t, database, plan, 2).Markdown
	stored := render + "\n## Rollback\n\nRoll back by hand.\n"
	if _, err := database.Pool.Exec(ctx, `update artifact_versions set markdown = $3 where artifact_id = $1 and number = $2`,
		plan, 2, stored); err != nil {
		t.Fatalf("write the stored markdown of version 2 of plan.md: %v", err)
	}
	if rendering := renderedVersion(t, database, plan, 2); rendering.Source != docs.RenderedFromTree || rendering.Markdown != render {
		t.Fatalf("version 2 of plan.md answers %q from %s once its stored markdown is rewritten, want its render %q from the tree",
			rendering.Markdown, rendering.Source, render)
	}
	for _, pair := range [][2]int{{1, 2}, {2, 3}} {
		readStored, err := compareVersions(storedVersionText(t, database, plan, pair[0]), storedVersionText(t, database, plan, pair[1]))
		if err != nil {
			t.Fatalf("compare the stored texts of versions %d and %d: %v", pair[0], pair[1], err)
		}
		readRendered, err := compareVersions(renderedVersion(t, database, plan, pair[0]).Markdown, renderedVersion(t, database, plan, pair[1]).Markdown)
		if err != nil {
			t.Fatalf("compare the renders of versions %d and %d: %v", pair[0], pair[1], err)
		}
		if reflect.DeepEqual(readStored, readRendered) {
			t.Fatalf("the stored texts of versions %d and %d make the change set their renders make, %+v, so this case proves nothing",
				pair[0], pair[1], readRendered)
		}
		requireRenderedChanges(t, handler, database, plan, pair[0], pair[1])
	}

	notes := uploadVersionsTo(t, handler, issue.Key, "notes.md",
		"# Notes\n\nA draft.\n",
		"# Notes\n\n## Open\n\nOne question.\n",
		"# Notes\n\n## Open\n\nTwo questions.\n")
	// An upload no role names renders from the tree too (R15: a version of a document no query
	// record covers holds a version_renders row with a time and no digest), so each version of
	// notes.md first renders from the tree. The renderer's other branches answer a version that
	// holds no row - one written before the cutover - by its stored bytes, so the rows are taken
	// away by SQL, as c-render-at-version deletes a row and reads the stored markdown answered. No
	// route stores a document version with no markdown, so version 1's is taken away by SQL too. It
	// is not the newest version, which the document's settlement reads as a text.
	for number := 1; number <= 3; number++ {
		if rendering := renderedVersion(t, database, notes, number); rendering.Source != docs.RenderedFromTree {
			t.Fatalf("version %d of notes.md answers %q from %s, want a render from the tree (R15)", number, rendering.Markdown, rendering.Source)
		}
	}
	if _, err := database.Pool.Exec(ctx, `delete from version_renders where artifact_id = $1`, notes); err != nil {
		t.Fatalf("take the version_renders rows of notes.md away: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `update artifact_versions set markdown = null where artifact_id = $1 and number = 1`, notes); err != nil {
		t.Fatalf("take the markdown of version 1 of notes.md away: %v", err)
	}
	for number, want := range map[int]docs.RenderSource{1: docs.NoBytes, 2: docs.StoredBytes, 3: docs.StoredBytes} {
		if rendering := renderedVersion(t, database, notes, number); rendering.Source != want {
			t.Fatalf("version %d of notes.md answers %q from %s, want %s", number, rendering.Markdown, rendering.Source, want)
		}
	}
	requireChanges(t, readChanges(t, handler, notes, 2, 3), []servedSection{
		{Heading: "Open", Occurrence: 0, Change: "changed"},
	}, []servedBlockRef{})
	requireChanges(t, readChanges(t, handler, notes, 1, 2), []servedSection{
		runBeforeTheFirstSection("added"),
		{Heading: "Open", Occurrence: 0, Change: "added"},
	}, []servedBlockRef{})
	requireRenderedChanges(t, handler, database, notes, 1, 2)
	requireRenderedChanges(t, handler, database, notes, 2, 3)
}

// changesReader is who the change set test reads as: alice, as the route's caller and as the
// reader the renderer answers.
var changesReader = model.Actor{Kind: "user", ID: "alice"}

// renderedVersion is version number of the document artifactID as the one renderer answers it for
// changesReader.
func renderedVersion(t *testing.T, database *store.Store, artifactID string, number int) docs.Rendering {
	t.Helper()
	rendering, err := docs.RenderVersion(context.Background(), database.Pool, changesReader, artifactID, number)
	if err != nil {
		t.Fatalf("render version %d of %s: %v", number, artifactID, err)
	}
	return rendering
}

// storedVersionText is the stored markdown of version number of the document artifactID, the empty
// text where it stores none.
func storedVersionText(t *testing.T, database *store.Store, artifactID string, number int) string {
	t.Helper()
	var markdown string
	if err := database.Pool.QueryRow(context.Background(),
		`select coalesce(markdown, '') from artifact_versions where artifact_id = $1 and number = $2`, artifactID, number).Scan(&markdown); err != nil {
		t.Fatalf("read the stored markdown of version %d of %s: %v", number, artifactID, err)
	}
	return markdown
}

// requireRenderedChanges fails t unless the change set the route answers changesReader for
// versions from and to of the document artifactID is the one compareVersions makes of the two
// versions as the one renderer answers them for that reader. A failure names each of the two
// versions whose stored markdown is not its render, and the text where the two differ: a change
// set read off that version's stored markdown differs there.
func requireRenderedChanges(t *testing.T, handler http.Handler, database *store.Store, artifactID string, from, to int) {
	t.Helper()
	var rendered [2]string
	var apart []string
	for index, number := range []int{from, to} {
		rendering := renderedVersion(t, database, artifactID, number)
		rendered[index] = rendering.Markdown
		if stored := storedVersionText(t, database, artifactID, number); stored != rendering.Markdown {
			renderHolds, storedHolds := differingText(rendering.Markdown, stored)
			apart = append(apart, fmt.Sprintf("version %d stores %q where its render from %s holds %q",
				number, storedHolds, rendering.Source, renderHolds))
		}
	}
	computed, err := compareVersions(rendered[0], rendered[1])
	if err != nil {
		t.Fatalf("compare the renders of versions %d and %d: %v", from, to, err)
	}
	computed.ArtifactID, computed.From, computed.To = artifactID, from, to
	encoded, err := json.Marshal(computed)
	if err != nil {
		t.Fatalf("encode the change set of the renders of versions %d and %d: %v", from, to, err)
	}
	var want servedChanges
	if err := json.Unmarshal(encoded, &want); err != nil {
		t.Fatalf("decode the change set of the renders of versions %d and %d: %v", from, to, err)
	}
	if got := readChanges(t, handler, artifactID, from, to); !reflect.DeepEqual(got, want) {
		t.Errorf("the change set from version %d to %d is %+v, want %+v, the one the two versions make as the renderer answers them (%s)",
			from, to, got, want, strings.Join(apart, "; "))
	}
}

// differingText is what a and b hold where they differ: each with the beginning and the end the
// two share left off.
func differingText(a, b string) (string, string) {
	start := 0
	for start < len(a) && start < len(b) && a[start] == b[start] {
		start++
	}
	end := 0
	for end < len(a)-start && end < len(b)-start && a[len(a)-1-end] == b[len(b)-1-end] {
		end++
	}
	return a[start : len(a)-end], b[start : len(b)-end]
}
