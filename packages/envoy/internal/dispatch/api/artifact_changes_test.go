package api

import (
	"fmt"
	"net/http"
	"reflect"
	"testing"
)

// GET /api/v1/artifacts/{id}/changes answers what changed between two stored versions of one
// document, read off their markdown with every decision block cut out: whether the cut is the
// whole change, each `##` section whose text differs, named by its heading and occurrence - the
// text outside every `##` section being one section under the heading "" - and each typed block
// other than an ask whose text or any attribute differs, named by its id. The daemon reads it to
// tell the session holding an issue what an approved new version changed, so these tests read
// the answer as that caller reads the JSON, field by field.

// servedChanges is the change set as a caller decodes the route's JSON.
type servedChanges struct {
	ArtifactID   string           `json:"artifact_id"`
	From         int              `json:"from"`
	To           int              `json:"to"`
	DecisionOnly bool             `json:"decision_only"`
	Sections     []servedSection  `json:"sections"`
	Blocks       []servedBlockRef `json:"blocks"`
}

// servedSection is one changed section as the route names it.
type servedSection struct {
	Heading    string `json:"heading"`
	Occurrence int    `json:"occurrence"`
	Change     string `json:"change"`
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

// uploadVersions uploads each text in turn as one document of a new issue, so text i is version
// i+1, and answers the document's id.
func uploadVersions(t *testing.T, handler http.Handler, name string, texts ...string) string {
	t.Helper()
	issue := createArtifactIssue(t, handler)
	var artifactID string
	for index, text := range texts {
		uploaded := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, name, text)
		if uploaded.Version.Number != index+1 {
			t.Fatalf("upload %d of %s = version %d, want %d", index+1, name, uploaded.Version.Number, index+1)
		}
		artifactID = uploaded.Artifact.ID
	}
	return artifactID
}

// requireChanges fails t unless the change set names exactly sections and blocks, and is not
// decision-only.
func requireChanges(t *testing.T, changes servedChanges, sections []servedSection, blocks []servedBlockRef) {
	t.Helper()
	if changes.DecisionOnly {
		t.Fatalf("decision_only = true, want false: %+v", changes)
	}
	if !reflect.DeepEqual(changes.Sections, sections) {
		t.Fatalf("sections = %+v, want %+v", changes.Sections, sections)
	}
	if !reflect.DeepEqual(changes.Blocks, blocks) {
		t.Fatalf("blocks = %+v, want %+v", changes.Blocks, blocks)
	}
}

// A section whose words changed is named by its heading, a section only the later version holds
// is added and one only the earlier holds is removed; a section left as it was, and the title
// above every section, are not named. The answer names the document and the versions it read.
func TestTheChangeSetNamesAChangedSectionByItsHeading(t *testing.T) {
	handler := newTestHandler(t)
	artifactID := uploadVersions(t, handler, "plan.md",
		"# Plan\n\n## Goal\n\nShip the service behind a flag.\n\n## Scope\n\nTwo regions.\n\n## Risks\n\nNone known.\n",
		"# Plan\n\n## Goal\n\nShip the service behind a flag and a canary.\n\n## Scope\n\nTwo regions.\n\n## Rollout\n\nOne region a day.\n",
	)

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

// A change to the text above the first `##` section, and to nothing else, is named: the text
// outside every `##` section is one section of its own, under the heading "" and occurrence 0,
// and the `##` section after it, left as it was, is not named.
func TestAChangeBeforeTheFirstSectionNamesTheTextOutsideEverySection(t *testing.T) {
	handler := newTestHandler(t)
	artifactID := uploadVersions(t, handler, "plan.md",
		"# Plan\n\nThe plan ships in May.\n\n## Goal\n\nShip the service behind a flag.\n",
		"# Plan\n\nThe plan ships in June.\n\n## Goal\n\nShip the service behind a flag.\n",
	)

	requireChanges(t, readChanges(t, handler, artifactID, 1, 2),
		[]servedSection{{Heading: "", Occurrence: 0, Change: "changed"}}, []servedBlockRef{})
}

// A requirement only the later version holds is added, and one only the earlier holds is
// removed; a requirement left as it was is not named. The section holding them changed with
// them, and is named too.
func TestTheChangeSetNamesAnAddedAndARemovedRequirement(t *testing.T) {
	handler := newTestHandler(t)
	artifactID := uploadVersions(t, handler, "requirements.md",
		"# Spec\n\n## Requirements\n\n:::requirement{#r1}\nThe read succeeds.\n:::\n\n:::requirement{#r2}\nThe read is fast.\n:::\n",
		"# Spec\n\n## Requirements\n\n:::requirement{#r1}\nThe read succeeds.\n:::\n\n:::requirement{#r3}\nThe read is logged.\n:::\n",
	)

	requireChanges(t, readChanges(t, handler, artifactID, 1, 2),
		[]servedSection{{Heading: "Requirements", Occurrence: 0, Change: "changed"}},
		[]servedBlockRef{
			{ID: "r3", Type: "requirement", Change: "added"},
			{ID: "r2", Type: "requirement", Change: "removed"},
		})
}

// A check whose `run` changed, one whose words changed, and one that names another CI run in its
// `ci_check` are each named changed; a check left as it was is not. The checks stand outside every
// `##` section, so the text outside the sections is the one section named.
func TestTheChangeSetNamesACheckWhoseRunOrWordsChanged(t *testing.T) {
	handler := newTestHandler(t)
	const ciControl = `negative_control="Break the build and watch the run fail."`
	check := func(id, attributes, words string) string {
		return ":::check{#" + id + " " + attributes + "}\n" + words + "\n:::\n\n"
	}
	artifactID := uploadVersions(t, handler, "verification.md",
		check("k1", `surface="cli" run="go test ./a/"`, "The read returns 200.")+
			check("k2", `surface="api" run="go test ./b/"`, "The write returns 201.")+
			check("k3", ciControl+` ci_check="build"`, "The server builds at the head.")+
			check("k4", `surface="cli" run="go test ./d/"`, "The list is sorted."),
		check("k1", `surface="cli" run="go test ./a/ -run TestRead"`, "The read returns 200.")+
			check("k2", `surface="api" run="go test ./b/"`, "The write returns 201 with the row.")+
			check("k3", ciControl+` ci_check="test"`, "The server builds at the head.")+
			check("k4", `surface="cli" run="go test ./d/"`, "The list is sorted."),
	)

	requireChanges(t, readChanges(t, handler, artifactID, 1, 2), []servedSection{{Heading: "", Occurrence: 0, Change: "changed"}}, []servedBlockRef{
		{ID: "k1", Type: "check", Change: "changed"},
		{ID: "k2", Type: "check", Change: "changed"},
		{ID: "k3", Type: "check", Change: "changed"},
	})
}

// decisionSpec is a spec with one section of prose and a decision block in a section of its own,
// the document the two decision tests answer the block of.
const decisionSpec = "# Plan\n\n## Rollout\n\nThe service ships behind a flag.\n\n## Decisions\n\n" + waveAsk

// answeredDecisionSpec writes decisionSpec into a new issue's spec, has alice answer its decision
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

// An answer a person gave to a decision block is the whole difference between the version before
// it and the version it wrote: the change set is decision-only, and names no section and no block,
// the section that holds the block included.
func TestAnAnsweredAskAloneIsADecisionOnlyChange(t *testing.T) {
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

// An answer beside a change to a section's words is no decision-only change: the changed section
// is named, and neither the decision block nor the section holding it is.
func TestAnAskAnsweredBesideAChangedSectionIsNoDecisionOnlyChange(t *testing.T) {
	handler := approvalCarryHandler(t)
	artifactID := answeredDecisionSpec(t, handler)
	reviseDocument(t, handler, artifactID, "behind a flag.", "behind a flag and a canary.", 3)

	requireChanges(t, readChanges(t, handler, artifactID, 1, 3),
		[]servedSection{{Heading: "Rollout", Occurrence: 0, Change: "changed"}}, []servedBlockRef{})
}

// Two sections under one heading are two sections, told apart by how many before each carry that
// heading: a change to the second names occurrence 1 and leaves the first unnamed.
func TestTwoSectionsOfOneHeadingAreToldApartByOccurrence(t *testing.T) {
	handler := newTestHandler(t)
	artifactID := uploadVersions(t, handler, "steps.md",
		"# Steps\n\n## Step\n\nBuild it.\n\n## Step\n\nShip it.\n",
		"# Steps\n\n## Step\n\nBuild it.\n\n## Step\n\nShip it on a Tuesday.\n",
	)

	requireChanges(t, readChanges(t, handler, artifactID, 1, 2),
		[]servedSection{{Heading: "Step", Occurrence: 1, Change: "changed"}}, []servedBlockRef{})
}

// A pair whose from is not below its to is refused 400 INVALID_VERSION, as a version number that
// is not positive is, and the refusal is the error envelope every artifact route answers.
func TestTheChangeSetRefusesFromNotBelowTo(t *testing.T) {
	handler := newTestHandler(t)
	artifactID := uploadVersions(t, handler, "plan.md", "# Plan\n\nOne.\n", "# Plan\n\nTwo.\n")

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
	handler := newTestHandler(t)
	artifactID := uploadVersions(t, handler, "plan.md", "# Plan\n\nOne.\n", "# Plan\n\nTwo.\n")

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
