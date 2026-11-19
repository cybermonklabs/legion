//go:build basebuild

// The basebuild tag, and how a test of it is run: store_census_test.go.

package main

// dispatch://INFRA-1950's c-read-cost: the reads this child touches get no slower over four
// shapes, read at this head and at the base build in one run.
//
// The test is TestThePredicateAndTheLensReadsGetNoSlower at the end of this file; everything above
// it is the four shapes it writes and the nine reads it measures over each of them.
// graph_cost_test.go holds the two arms, the clock and the order statistic every cost measurement
// shares, and read_cost_test.go and write_cost_test.go the corpus writers these shapes are built
// from.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	// predicateCostProject is the project every shape hangs under.
	predicateCostProject = "LCOST"
	// predicateCostHeavyBlocks is the first shape: an issue whose main document holds this many
	// blocks, nesting writeCostNesting levels deep, with 40 pointers for each 1,000 blocks of it.
	predicateCostHeavyBlocks = 8_001
	// predicateCostInbound is the second shape: one block with this many tuples into it, which
	// are the pointers of a document written for the purpose.
	predicateCostInbound = 5_000
	// predicateCostHistoryRows is the third shape: a person read while the store holds this many
	// rows of history that are not theirs, written as predicateCostHistoryDocuments documents by
	// readCostOther. predicateCostPersonComments is how many comments the person wrote, so the
	// reads of the shape answer rows of their own out of that history.
	predicateCostHistoryRows      = 50_000
	predicateCostHistoryDocuments = 7
	predicateCostPersonComments   = 200
	// predicateCostChain is the fourth shape, this head's alone: a spec with a chain of this many
	// triggered lenses, half of them reserved for their author. It is the lens read's own depth
	// (api's lensChainDepth), the longest chain that read lists whole, and no scale changes it.
	predicateCostChain = 50
	// predicateCostRequirements is how many requirements each shape's spec marks, each served by
	// one check and one plan task, so the coverage read of every shape reads the node state of
	// each requirement and of what serves it, which is the part of that read this child changed
	// (api/coverage.go's queueRequirementStates and queueServingBlockCounts). The coverage of a
	// spec that marks none answers before either statement runs, so it would measure nothing this
	// child did.
	predicateCostRequirements = 20
	// predicateCostChildren is how many child issues each shape's issue holds, so the issue tree,
	// the list of an issue's children (GET /api/v1/issues?parent=), answers rows of every shape:
	// the tree of an issue with no child answers an empty list, and would measure nothing.
	predicateCostChildren = 20
	// predicateCostRatio is the bound: a read whose p50 at this head is more than this many times
	// its p50 at the base build, in the same run, fails the test.
	predicateCostRatio = 1.25
	// predicateCostPhrase starts the word each shape's main document carries and no other
	// document of the corpus does, so the search of a shape matches that shape's corpus.
	predicateCostPhrase = "quillon"
	// predicateCostIndexWait bounds the wait for the block-index reconciliation to index a corpus.
	predicateCostIndexWait = 30 * time.Minute
	// predicateCostScaleTestEnv gates the run, as api/inbox_dismissed_scale_test.go's
	// dismissedScaleEnv gates a14: building the base commit, writing about 70,000 blocks into
	// each of two databases and taking about 12,000 timed readings costs tens of minutes, which
	// no ordinary run should pay. No CI job builds the basebuild tag, so CI never runs it.
	predicateCostScaleTestEnv = "LEGION_SCALE_TEST"
	// predicateCostScaleEnv is the share of the check's sizes a run writes and reads: a fraction
	// above 0 and at most 1, and 1 where it is unset, which is the check. Below 1 the run is a
	// smoke of this test and no reading of the check: every shape it states carries the size it
	// was written at, and once it has measured and printed every reading it ends in SKIP, which
	// the check's run string refuses.
	predicateCostScaleEnv = "LEGION_READ_COST_SCALE"
	// predicateCostControlEnv is the check's negative control, which this test reads and no
	// server does. Set to predicateCostDropInbound, the run drops predicateCostInboundIndexes
	// from this head's database once its corpus is written and analysed, and prints the plan of
	// the tuples-into-a-node lookup before and after the drop. Unset, the run drops nothing. A
	// run with it set is no reading of the check: one whose every read holds the bound ends in
	// SKIP, a control that did not bite, which the check's run string refuses.
	predicateCostControlEnv  = "LEGION_READ_COST_CONTROL"
	predicateCostDropInbound = "drop-inbound-indexes"
)

// predicateCostInboundIndexes are the indexes the check's negative control drops. The check
// names graph_tuples_inbound alone, but graph_tuples_as_of_in keys the same (to_kind, to_id,
// kind) under the same partial predicate (migration 20261109013400_graph_store), so a run that
// dropped the first would read the second and pay one sort more: the control drops both.
var predicateCostInboundIndexes = []string{"graph_tuples_inbound", "graph_tuples_as_of_in"}

// predicateCostInboundLookup is the lookup the tuples-into-a-node read makes of one block, $1 its
// node id, read as a value before the read as readEdgeRows reads it: the edges of graph_edges into
// it, newest first, as refs' readEdgeRows selects them, less the predicate of each far end, which
// reads graph_nodes and no index of graph_tuples.
const predicateCostInboundLookup = `
	select g.kind, g.from_kind, g.from_id, g.created_at, g.source_seq
	from graph_edges g where g.to_kind = 'block' and g.to_id = (select $1::text)
	order by g.created_at desc, g.kind, g.from_kind, g.from_id`

// predicateCostControlOf reads LEGION_READ_COST_CONTROL: empty where it is unset, which is the
// check, and predicateCostDropInbound where it names the check's negative control.
func predicateCostControlOf(t *testing.T) string {
	t.Helper()
	switch control := strings.TrimSpace(os.Getenv(predicateCostControlEnv)); control {
	case "", predicateCostDropInbound:
		return control
	default:
		t.Fatalf("%s=%q: want %q, the check's negative control, or nothing", predicateCostControlEnv, control, predicateCostDropInbound)
		return ""
	}
}

// predicateCostKinds are the lens_kind ids of the catalogue the fourth shape's chain takes in turn.
var predicateCostKinds = []string{"trade-off", "inversion", "botec", "second-order"}

// predicateCostSizes is what one run writes and reads: the check's sizes, or the share of them
// LEGION_READ_COST_SCALE names.
type predicateCostSizes struct {
	scale                                            float64
	heavyBlocks, inbound, historyRows, historyBlocks int
	personComments                                   int
	// readings is how many readings each read takes over each shape on each build:
	// costReadReadings, 200, at the check's sizes.
	readings int
}

// predicateCostSizesOf reads LEGION_READ_COST_SCALE and answers the sizes of this run. Each share
// keeps a floor under it that keeps the shape's own structure: the eight levels with a pointer in
// each document, more than one pointer at the inbound node, and twenty readings to a cell.
func predicateCostSizesOf(t *testing.T) predicateCostSizes {
	t.Helper()
	scale := 1.0
	if raw := strings.TrimSpace(os.Getenv(predicateCostScaleEnv)); raw != "" {
		parsed, err := strconv.ParseFloat(raw, 64)
		if err != nil || parsed <= 0 || parsed > 1 {
			t.Fatalf("%s=%q: want a fraction above 0 and at most 1, the share of the check's sizes this run writes",
				predicateCostScaleEnv, raw)
		}
		scale = parsed
	}
	share := func(full, floor int) int { return max(int(math.Round(float64(full)*scale)), floor) }
	historyRows := share(predicateCostHistoryRows, predicateCostHistoryDocuments*100)
	return predicateCostSizes{
		scale:          scale,
		heavyBlocks:    share(predicateCostHeavyBlocks, 100),
		inbound:        share(predicateCostInbound, 10),
		historyRows:    historyRows,
		historyBlocks:  (historyRows + predicateCostHistoryDocuments - 1) / predicateCostHistoryDocuments,
		personComments: share(predicateCostPersonComments, 10),
		readings:       share(costReadReadings, 20),
	}
}

// The four shapes as the check states them, at the sizes of this run.
func (s predicateCostSizes) heavyName() string {
	return "an issue of " + predicateCostCount(s.heavyBlocks) + " blocks nested eight deep"
}

func (s predicateCostSizes) inboundName() string {
	return "a node with " + predicateCostCount(s.inbound) + " inbound tuples"
}

func (s predicateCostSizes) historyName() string {
	return "a person with " + predicateCostCount(s.historyRows) + " rows of unrelated history"
}

func (predicateCostSizes) lensName() string {
	return "a spec with a chain of " + strconv.Itoa(predicateCostChain) +
		" triggered lenses, half of them reserved for their author"
}

// predicateCostShape is one shape on one build and what the nine reads address on it.
type predicateCostShape struct {
	// name is the shape as the check states it, at the size this run wrote it.
	name string
	// headOnly marks the fourth shape, which only this head writes: a lens is this child's kind.
	headOnly bool
	// issue is the issue the shape hangs under, document its main document (by artifact id) and
	// documentRef that document's dispatch:// address.
	issue, document, documentRef string
	// linksTo is the document the main document's pointers reach, the far end of the list of
	// its links, and the main document itself where it points at no other.
	linksTo string
	// into is the node the tuples-into read reads, and outOf the node the tuples-out read reads.
	into, outOf string
	// phrase is the word only this shape's main document carries, which its search asks for.
	phrase string
	// lenses are the fourth shape's lenses, in the order they were invoked.
	lenses []string
	// holds asserts the shape is the one its name states, on one build, and answers what the
	// shape holds of its own beyond the rows every shape prints.
	holds func(t *testing.T, arm *costArm, shape *predicateCostShape) []string
	// rows is what the shape holds, printed before any reading is taken.
	rows []string
}

// ───── the four shapes ────────────────────────────────────────────────────

// seedPredicateCost writes the three shapes both builds hold through the arm's own API, the same
// requests on each build, and answers them in the order they are measured. No row here is written
// by SQL: each build holds what its own code writes for these requests.
func seedPredicateCost(t *testing.T, arm *costArm, sizes predicateCostSizes) []*predicateCostShape {
	t.Helper()
	started := time.Now()
	arm.server.call(t, http.MethodPost, "/api/v1/projects",
		`{"key":"`+predicateCostProject+`","name":"The predicate and lens read-cost shapes"}`, http.StatusCreated)
	shapes := []*predicateCostShape{
		seedPredicateCostHeavy(t, arm, sizes),
		seedPredicateCostInbound(t, arm, sizes),
		seedPredicateCostHistory(t, arm, sizes),
	}
	t.Logf("%s: the three shapes both builds hold were written in %s", arm.name, time.Since(started).Round(time.Second))
	return shapes
}

// predicateCostIssue creates the issue one shape hangs under and answers its key and its spec's
// artifact id. Its spec marks predicateCostRequirements requirements and carries phrase where the
// shape's search reads the spec, a Verification document and a plan hold one check and one plan
// task serving each requirement, and predicateCostChildren child issues stand under it, which the
// issue tree lists. The spec and each child are forced past the near-copy gate, as
// createCostIssue's spec is: the four specs differ only in their titles, and so do the children.
func predicateCostIssue(t *testing.T, arm *costArm, title, phrase string) (string, string) {
	t.Helper()
	var spec strings.Builder
	fmt.Fprintf(&spec, "# %s\n\nThe corpus of one cost measurement hangs under this issue.\n\n", title)
	if phrase != "" {
		fmt.Fprintf(&spec, "%s is in this document and in no other of the corpus.\n\n", phrase)
	}
	for index := range predicateCostRequirements {
		fmt.Fprintf(&spec, ":::requirement{#r%d}\nRequirement %d of the issue this shape hangs under.\n:::\n\n",
			index+1, index+1)
	}
	body, err := json.Marshal(map[string]any{
		"project": predicateCostProject, "title": title, "force": true, "spec": spec.String(),
	})
	if err != nil {
		t.Fatalf("encode the issue %q: %v", title, err)
	}
	created := arm.server.call(t, http.MethodPost, "/api/v1/issues", string(body), http.StatusCreated)
	var issue struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}
	if err := json.Unmarshal(created, &issue); err != nil || issue.Key == "" || issue.PrimaryArtifactID == "" {
		t.Fatalf("%s: create the issue %q: %v: %s", arm.name, title, err, created)
	}
	var checks, tasks strings.Builder
	for index := range predicateCostRequirements {
		serves := fmt.Sprintf("dispatch://%s/spec#b-r%d", issue.Key, index+1)
		fmt.Fprintf(&checks, ":::check{#c%d serves=\"%s\" surface=\"api\" run=\"go test ./...\"}\n"+
			"The check that proves requirement %d.\n:::\n\n", index+1, serves, index+1)
		fmt.Fprintf(&tasks, ":::plan_task{#t%d serves=\"%s\"}\nThe work requirement %d asks for.\n:::\n\n",
			index+1, serves, index+1)
	}
	costUploadDocument(t, arm, issue.Key, "verification.md", checks.String())
	costUploadDocument(t, arm, issue.Key, "plan.md", tasks.String())
	for index := range predicateCostChildren {
		child, err := json.Marshal(map[string]any{
			"project": predicateCostProject, "title": fmt.Sprintf("Child %d of %s", index+1, title),
			"parent": issue.Key, "force": true,
		})
		if err != nil {
			t.Fatalf("encode child %d of the issue %q: %v", index+1, title, err)
		}
		arm.server.call(t, http.MethodPost, "/api/v1/issues", string(child), http.StatusCreated)
	}
	return issue.Key, issue.PrimaryArtifactID
}

// seedPredicateCostHeavy is the first shape: an issue whose main document holds 8,001 blocks
// nesting eight deep, with 40 pointers at blocks of another document of the issue for each 1,000
// blocks of it.
func seedPredicateCostHeavy(t *testing.T, arm *costArm, sizes predicateCostSizes) *predicateCostShape {
	t.Helper()
	phrase := predicateCostShapePhrase("heavy")
	issue, _ := predicateCostIssue(t, arm, "The issue of "+predicateCostCount(sizes.heavyBlocks)+" blocks", "")
	pointers := costPointerCount(sizes.heavyBlocks)
	targets := costPointerTargets(t, arm, issue, "heavy-targets.md", pointers)
	// costDocumentText writes one block fewer than the shape, and the paragraph carrying the
	// shape's phrase is the last block of it.
	document := costUploadDocument(t, arm, issue, "heavy.md",
		costDocumentText(sizes.heavyBlocks-1, writeCostNesting, pointers, targets, 0, -1)+
			"\n"+phrase+" is in this document and in no other of the corpus.\n")
	return &predicateCostShape{
		name:        sizes.heavyName(),
		issue:       issue,
		document:    document,
		documentRef: costDocumentAddress(t, arm, document),
		linksTo:     predicateCostDocumentOf(targets[0]),
		into:        targets[0],
		phrase:      phrase,
		holds: func(t *testing.T, arm *costArm, shape *predicateCostShape) []string {
			t.Helper()
			blocks := costBlockCount(t, arm, shape.document)
			deep := costNestingDepth(t, arm, shape.document)
			if blocks != sizes.heavyBlocks || deep != writeCostNesting {
				t.Fatalf("%s: the main document of %q holds %d block(s) nested %d deep, and the shape states %d nested %d deep",
					arm.name, shape.name, blocks, deep, sizes.heavyBlocks, writeCostNesting)
			}
			return []string{fmt.Sprintf("a main document of %d blocks nested %d deep with %d pointers", blocks, deep, pointers)}
		},
	}
}

// seedPredicateCostInbound is the second shape: one block with 5,000 tuples into it, which are the
// pointers of a document of 5,000 items written at it.
func seedPredicateCostInbound(t *testing.T, arm *costArm, sizes predicateCostSizes) *predicateCostShape {
	t.Helper()
	phrase := predicateCostShapePhrase("inbound")
	issue, _ := predicateCostIssue(t, arm, "The node of "+predicateCostCount(sizes.inbound)+" inbound tuples", "")
	target := costPointerTargets(t, arm, issue, "inbound-target.md", 1)[0]
	var text strings.Builder
	text.WriteString("# The document that points at one block\n\n")
	text.WriteString(phrase + " is in this document and in no other of the corpus.\n\n")
	for index := range sizes.inbound {
		fmt.Fprintf(&text, "- item %d points at %s\n", index, target)
	}
	document := costUploadDocument(t, arm, issue, "inbound.md", text.String())
	return &predicateCostShape{
		name:        sizes.inboundName(),
		issue:       issue,
		document:    document,
		documentRef: costDocumentAddress(t, arm, document),
		linksTo:     predicateCostDocumentOf(target),
		into:        target,
		phrase:      phrase,
		holds: func(t *testing.T, arm *costArm, shape *predicateCostShape) []string {
			t.Helper()
			block := predicateCostBlockID(shape.into)
			references := arm.count(t, `
				select count(*) from block_refs r
				join document_blocks b on b.node_id = r.to_id
				join artifacts a on a.id = b.artifact_id
				where r.to_kind = 'block' and a.issue_key = $1 and b.block_id = $2`, shape.issue, block)
			if references != int64(sizes.inbound) {
				t.Fatalf("%s: %d block reference(s) point at the node of %q, and the shape states %d",
					arm.name, references, shape.name, sizes.inbound)
			}
			rows := []string{fmt.Sprintf("%d block reference(s) into the node the tuples-into read reads", references)}
			if !arm.head {
				return rows
			}
			tuples := arm.count(t, `
				select count(*) from graph_tuples g
				join document_blocks b on g.to_kind = 'block' and g.to_id = b.node_id
				join artifacts a on a.id = b.artifact_id
				where g.asserted and a.issue_key = $1 and b.block_id = $2`, shape.issue, block)
			if tuples < int64(sizes.inbound) {
				t.Fatalf("%s: the store holds %d tuple(s) into the node of %q, and the shape states %d",
					arm.name, tuples, shape.name, sizes.inbound)
			}
			return append(rows, fmt.Sprintf("%d asserted tuple(s) into that node", tuples))
		},
	}
}

// seedPredicateCostHistory is the third shape: a person read while the store holds 50,000 rows of
// history that are not theirs, seven documents of another person's, beside the person's own
// document and 200 comments of their own, so that the reads of the shape answer rows out of that
// pile rather than nothing.
func seedPredicateCostHistory(t *testing.T, arm *costArm, sizes predicateCostSizes) *predicateCostShape {
	t.Helper()
	phrase := predicateCostShapePhrase("person")
	issue, _ := predicateCostIssue(t, arm,
		"The person read against "+predicateCostCount(sizes.historyRows)+" rows of other history", "")
	pointers := costPointerCount(sizes.historyBlocks)
	targets := costPointerTargets(t, arm, issue, "history-targets.md", pointers)
	for index := range predicateCostHistoryDocuments {
		costUploadDocumentAs(t, arm, readCostOther, issue, fmt.Sprintf("history%d.md", index),
			costDocumentText(sizes.historyBlocks, writeCostNesting, pointers, targets, index, -1))
	}
	document := costUploadDocument(t, arm, issue, "person.md",
		"# The document of the person's own shape\n\n"+
			phrase+" is in this document and in no other of the corpus.\n\n"+
			"A paragraph the person's own page hangs beside.\n")
	for index := range sizes.personComments {
		body, err := json.Marshal(map[string]any{
			"body": fmt.Sprintf("Comment %d of the person whose rows this shape reads.", index),
		})
		if err != nil {
			t.Fatalf("encode the person's comment: %v", err)
		}
		arm.server.call(t, http.MethodPost, "/api/v1/issues/"+issue+"/comments", string(body), http.StatusCreated)
	}
	documentRef := costDocumentAddress(t, arm, document)
	return &predicateCostShape{
		name:        sizes.historyName(),
		issue:       issue,
		document:    document,
		documentRef: documentRef,
		linksTo:     documentRef,
		into:        targets[0],
		phrase:      phrase,
		holds: func(t *testing.T, arm *costArm, shape *predicateCostShape) []string {
			t.Helper()
			history := arm.count(t, `
				select count(*) from document_blocks b join artifacts a on a.id = b.artifact_id
				where a.issue_key = $1 and a.created_by->>'id' = $2`, shape.issue, readCostOther)
			if history < int64(sizes.historyRows) {
				t.Fatalf("%s: %q holds %d document block(s) of %s's, and the shape states %d rows of unrelated history",
					arm.name, shape.name, history, readCostOther, sizes.historyRows)
			}
			return []string{fmt.Sprintf("%d document block(s) of %s's history in %d documents, and the person's own document and %d comments",
				history, readCostOther, predicateCostHistoryDocuments, sizes.personComments)}
		},
	}
}

// seedPredicateCostLenses is the fourth shape, which only this head writes: a spec with a chain
// of 50 lenses invoked on it, each after the first triggered by the one before it, and every
// other one, from the second, reserved for its author, so half of them are and every step of the
// chain crosses from one visibility to the other. Each lens carries a verdict and one finding, as
// an analysis does. The spec is the shape's main document and carries its phrase.
func seedPredicateCostLenses(t *testing.T, arm *costArm, sizes predicateCostSizes) *predicateCostShape {
	t.Helper()
	started := time.Now()
	phrase := predicateCostShapePhrase("lens")
	issue, spec := predicateCostIssue(t, arm, "The spec with a chain of triggered lenses", phrase)
	specRef := "dispatch://" + issue + "/spec"
	lenses := make([]string, 0, predicateCostChain)
	for index := range predicateCostChain {
		input := map[string]any{
			"on":         specRef,
			"kind":       predicateCostKinds[index%len(predicateCostKinds)],
			"verdict":    fmt.Sprintf("Lens %d of the chain holds.", index+1),
			"findings":   []string{fmt.Sprintf("The finding of lens %d of the chain.", index+1)},
			"visibility": predicateCostVisibility(index),
		}
		if index > 0 {
			input["triggered_by"] = lenses[index-1]
		}
		body, err := json.Marshal(input)
		if err != nil {
			t.Fatalf("encode lens %d of the chain: %v", index+1, err)
		}
		created := arm.server.call(t, http.MethodPost, "/api/v1/issues/"+issue+"/lenses", string(body), http.StatusCreated)
		var lens struct {
			Address string `json:"address"`
		}
		if err := json.Unmarshal(created, &lens); err != nil || lens.Address == "" {
			t.Fatalf("%s: invoke lens %d of the chain: %v: %s", arm.name, index+1, err, created)
		}
		lenses = append(lenses, lens.Address)
	}
	t.Logf("%s: the fourth shape was written in %s", arm.name, time.Since(started).Round(time.Second))
	return &predicateCostShape{
		name:        sizes.lensName(),
		headOnly:    true,
		issue:       issue,
		document:    spec,
		documentRef: specRef,
		linksTo:     specRef,
		into:        specRef,
		// The lens in the middle of the chain, which is one of those reserved for its author:
		// its tuples out are its part_of, its has_kind, its triggered_by and its authored_by.
		outOf:  lenses[predicateCostChain/2],
		phrase: phrase,
		lenses: lenses,
		holds:  predicateCostLensesHold,
	}
}

// predicateCostVisibility is the visibility of the chain's lens at index: every other one, from
// the second, is reserved for its author.
func predicateCostVisibility(index int) string {
	if index%2 == 1 {
		return "author"
	}
	return "project"
}

// predicateCostLensRead is what the fourth shape's guard reads of GET /api/v1/lenses: each lens's
// address and visibility, and each chain with the lens its walk stopped at.
type predicateCostLensRead struct {
	Lenses []predicateCostLens `json:"lenses"`
	Chains []struct {
		Lenses    []predicateCostLens `json:"lenses"`
		StoppedAt *string             `json:"stopped_at"`
	} `json:"chains"`
}

type predicateCostLens struct {
	Address    string `json:"address"`
	Visibility string `json:"visibility"`
}

// predicateCostLensesHold holds the fourth shape to its statement, through the lens read its
// author takes: the 50 lenses on the spec in the order they were invoked, one chain of all 50
// that the walk lists whole with no stop, and 25 of them reserved for their author. A chain that
// did not form would otherwise be measured as a fast one.
func predicateCostLensesHold(t *testing.T, arm *costArm, shape *predicateCostShape) []string {
	t.Helper()
	read := arm.server.call(t, http.MethodGet, "/api/v1/lenses?on="+url.QueryEscape(shape.documentRef), "", http.StatusOK)
	var answer predicateCostLensRead
	if err := json.Unmarshal(read, &answer); err != nil {
		t.Fatalf("%s: read the lenses on %s: %v: %s", arm.name, shape.documentRef, err, read)
	}
	addresses := func(lenses []predicateCostLens) []string {
		listed := make([]string, len(lenses))
		for index, lens := range lenses {
			listed[index] = lens.Address
		}
		return listed
	}
	if got := strings.Join(addresses(answer.Lenses), " "); got != strings.Join(shape.lenses, " ") {
		t.Fatalf("%s: the lens read of %s lists %d lens(es), and the shape invoked %d on it in this order: %v",
			arm.name, shape.documentRef, len(answer.Lenses), len(shape.lenses), shape.lenses)
	}
	if len(answer.Chains) != 1 || answer.Chains[0].StoppedAt != nil ||
		strings.Join(addresses(answer.Chains[0].Lenses), " ") != strings.Join(shape.lenses, " ") {
		t.Fatalf("%s: the lens read of %s answers %d chain(s), and the shape is one chain of all %d lenses listed whole with no stop: %s",
			arm.name, shape.documentRef, len(answer.Chains), predicateCostChain, read)
	}
	authorOnly := 0
	for _, lens := range answer.Lenses {
		if lens.Visibility == "author" {
			authorOnly++
		}
	}
	if authorOnly != predicateCostChain/2 {
		t.Fatalf("%s: %d of the %d lenses on %s are reserved for their author, and the shape states half of them",
			arm.name, authorOnly, predicateCostChain, shape.documentRef)
	}
	triggers := arm.count(t, `
		select count(*) from graph_tuples g join graph_nodes n on n.kind = g.from_kind and n.id = g.from_id
		where g.kind = 'triggered_by' and g.asserted and n.kind = 'lens'`)
	return []string{
		fmt.Sprintf("%d lenses on the spec, which the lens read lists as one chain of %d with no stop",
			len(answer.Lenses), len(answer.Chains[0].Lenses)),
		fmt.Sprintf("%d of them reserved for their author", authorOnly),
		fmt.Sprintf("%d triggered_by tuple(s) between them", triggers),
	}
}

// ───── what each shape holds ──────────────────────────────────────────────

// settlePredicateCost finishes each shape once its build has indexed it: it resolves the node the
// tuples-out read starts from, which is the server's own answer and not a guess, holds the shape
// to its statement, to the coverage and to the children it was written with, and prints what it
// holds.
func settlePredicateCost(t *testing.T, arm *costArm, shapes []*predicateCostShape) {
	t.Helper()
	for _, shape := range shapes {
		if shape.outOf == "" {
			shape.outOf = readCostPointerAt(t, arm, shape.into, readCostEdges(t, arm, "to", shape.into))
		}
		shape.rows = append(shape.holds(t, arm, shape),
			fmt.Sprintf("%d block(s) of the issue's documents", arm.count(t,
				`select count(*) from document_blocks b join artifacts a on a.id = b.artifact_id where a.issue_key = $1`, shape.issue)),
			fmt.Sprintf("%d block reference(s)", arm.count(t,
				`select count(*) from block_refs r join artifacts a on a.id = r.artifact_id where a.issue_key = $1`, shape.issue)),
			fmt.Sprintf("%d comment(s)", arm.count(t, `select count(*) from comments where issue_key = $1`, shape.issue)),
			predicateCostCoverageHolds(t, arm, shape),
			predicateCostTreeHolds(t, arm, shape))
		t.Logf("%s: the shape %q holds %s", arm.name, shape.name, strings.Join(shape.rows, ", "))
	}
}

// predicateCostCoverageHolds holds the shape's issue to the coverage it was written with - every
// requirement of its spec served by a check and a plan task - so the coverage read measured is one
// that reads what serves each requirement, and answers that as a row of the shape.
func predicateCostCoverageHolds(t *testing.T, arm *costArm, shape *predicateCostShape) string {
	t.Helper()
	read := arm.server.call(t, http.MethodGet, "/api/v1/issues/"+shape.issue+"/coverage", "", http.StatusOK)
	var coverage struct {
		Requirements []struct {
			BlockID   string `json:"block_id"`
			Checks    int    `json:"checks"`
			PlanTasks int    `json:"plan_tasks"`
		} `json:"requirements"`
	}
	if err := json.Unmarshal(read, &coverage); err != nil {
		t.Fatalf("%s: read the coverage of %s: %v: %s", arm.name, shape.issue, err, read)
	}
	served := 0
	for _, requirement := range coverage.Requirements {
		if requirement.Checks > 0 && requirement.PlanTasks > 0 {
			served++
		}
	}
	if len(coverage.Requirements) != predicateCostRequirements || served != predicateCostRequirements {
		t.Fatalf("%s: the coverage of %s answers %d requirement(s), %d of them served by a check and a plan task, and the shape's spec marks %d, each served: %s",
			arm.name, shape.issue, len(coverage.Requirements), served, predicateCostRequirements, read)
	}
	return fmt.Sprintf("%d requirement(s) of its spec, each served by a check and a plan task", served)
}

// predicateCostTreePath is the issue tree of the shape's issue, the list of its children: GET
// /api/v1/issues with parent, which listIssues filters on the parent's key.
func predicateCostTreePath(shape *predicateCostShape) string {
	return "/api/v1/issues?parent=" + url.QueryEscape(shape.issue)
}

// predicateCostTreeHolds holds the shape's issue to the children it was written with, through the
// issue tree the reads measure: the predicateCostChildren issues the seed wrote under it, each
// naming it as its parent, so the tree measured answers rows of the shape.
func predicateCostTreeHolds(t *testing.T, arm *costArm, shape *predicateCostShape) string {
	t.Helper()
	read := arm.server.call(t, http.MethodGet, predicateCostTreePath(shape), "", http.StatusOK)
	var children []struct {
		Key    string  `json:"key"`
		Parent *string `json:"parent"`
	}
	if err := json.Unmarshal(read, &children); err != nil {
		t.Fatalf("%s: read the issue tree of %s: %v: %s", arm.name, shape.issue, err, read)
	}
	for _, child := range children {
		if child.Parent == nil || *child.Parent != shape.issue {
			t.Fatalf("%s: the issue tree of %s lists %s, whose parent is not %s: %s", arm.name, shape.issue, child.Key, shape.issue, read)
		}
	}
	if len(children) != predicateCostChildren {
		t.Fatalf("%s: the issue tree of %s lists %d child issue(s), and the shape wrote %d under it: %s",
			arm.name, shape.issue, len(children), predicateCostChildren, read)
	}
	return fmt.Sprintf("%d child issue(s), which the issue tree lists", len(children))
}

// requirePredicateCostAuthorNodes holds the first three shapes to holding no node reserved for its
// author: no block of a document takes `author`, so every node of the store reserved for its
// author is a lens of the fourth shape or a finding of one (a block whose id names its lens).
func requirePredicateCostAuthorNodes(t *testing.T, head *costArm) {
	t.Helper()
	elsewhere := head.count(t, `
		select count(*) from graph_nodes
		where visibility = 'author' and kind <> 'lens' and not (kind = 'block' and id like 'lens/%')`)
	lensNodes := head.count(t, `
		select count(*) from graph_nodes
		where visibility = 'author' and (kind = 'lens' or (kind = 'block' and id like 'lens/%'))`)
	t.Logf("%s: the first three shapes hold %d node(s) reserved for its author, because no block of a document takes author; "+
		"the fourth holds %d, its author-only lenses and their findings", head.name, elsewhere, lensNodes)
	if elsewhere != 0 {
		t.Fatalf("%s: the store holds %d node(s) reserved for its author outside the fourth shape's lenses and findings, and the first three shapes state none",
			head.name, elsewhere)
	}
}

// dropPredicateCostInboundIndexes is the check's negative control on this head's database: it
// drops predicateCostInboundIndexes, and prints the plan of the lookup the tuples-into-a-node read
// makes of the node of shape before the drop and after it, so the run shows the read losing the
// index rather than assuming it. A plan that read neither index before the drop would make the
// drop no control of this read, and fails the run.
func dropPredicateCostInboundIndexes(t *testing.T, head *costArm, shape *predicateCostShape) {
	t.Helper()
	var node string
	if err := head.pool.QueryRow(context.Background(), `
		select b.node_id from document_blocks b join artifacts a on a.id = b.artifact_id
		where a.issue_key = $1 and b.block_id = $2`, shape.issue, predicateCostBlockID(shape.into)).Scan(&node); err != nil {
		t.Fatalf("%s: read the node id of %s, the node of %q: %v", head.name, shape.into, shape.name, err)
	}
	reads := func(plan string) []string {
		return slices.DeleteFunc(slices.Clone(predicateCostInboundIndexes), func(index string) bool { return !strings.Contains(plan, index) })
	}
	before := predicateCostInboundPlan(t, head, node)
	t.Logf("%s=%s: %s's plan of the tuples into %s, the node of %q, before the drop (it reads %v):\n%s",
		predicateCostControlEnv, predicateCostDropInbound, head.name, shape.into, shape.name, reads(before), before)
	if len(reads(before)) == 0 {
		t.Fatalf("%s=%s: %s's plan of the tuples into %s reads neither of %v before the drop, so dropping them is no control of that read",
			predicateCostControlEnv, predicateCostDropInbound, head.name, shape.into, predicateCostInboundIndexes)
	}
	for _, index := range predicateCostInboundIndexes {
		if _, err := head.pool.Exec(context.Background(), "drop index "+index); err != nil {
			t.Fatalf("%s=%s: drop %s from %s's database: %v", predicateCostControlEnv, predicateCostDropInbound, index, head.name, err)
		}
	}
	after := predicateCostInboundPlan(t, head, node)
	t.Logf("%s=%s: %s's plan of the tuples into %s after %v were dropped from its database:\n%s",
		predicateCostControlEnv, predicateCostDropInbound, head.name, shape.into, predicateCostInboundIndexes, after)
	if left := reads(after); len(left) != 0 {
		t.Fatalf("%s=%s: %s's plan of the tuples into %s still reads %v after the drop", predicateCostControlEnv, predicateCostDropInbound,
			head.name, shape.into, left)
	}
}

// predicateCostInboundPlan is the plan Postgres makes on the arm's database of
// predicateCostInboundLookup for node, the node id of one block, as EXPLAIN prints it.
func predicateCostInboundPlan(t *testing.T, arm *costArm, node string) string {
	t.Helper()
	rows, err := arm.pool.Query(context.Background(), "explain "+predicateCostInboundLookup, node)
	if err != nil {
		t.Fatalf("%s: explain the tuples into %s: %v", arm.name, node, err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("%s: explain the tuples into %s: %v", arm.name, node, err)
	}
	return strings.Join(lines, "\n")
}

// ───── the nine reads ─────────────────────────────────────────────────────

// predicateCostRead is one read this child touches, measured over each of the four shapes. Its
// name says which of the check's reads it is and the route it is read through.
type predicateCostRead struct {
	name string
	// headOnly marks the read the base build does not serve at all, which this child adds: it
	// has no reading at the base build, so it is printed and bound nothing.
	headOnly bool
	path     func(shape *predicateCostShape) string
}

func predicateCostReads() []predicateCostRead {
	return []predicateCostRead{
		{name: "the list of a kind (GET /api/v1/block-links)", path: func(shape *predicateCostShape) string {
			return "/api/v1/block-links?from=" + url.QueryEscape(shape.documentRef) + "&to=" + url.QueryEscape(shape.linksTo)
		}},
		{name: "search (GET /api/v1/search)", path: func(shape *predicateCostShape) string {
			return "/api/v1/search?project=" + predicateCostProject + "&q=" + url.QueryEscape(shape.phrase)
		}},
		{name: "the tuples out of a node (GET /api/v1/references?from=)", path: func(shape *predicateCostShape) string {
			return "/api/v1/references?from=" + url.QueryEscape(shape.outOf)
		}},
		{name: "the tuples into a node (GET /api/v1/references?to=)", path: func(shape *predicateCostShape) string {
			return "/api/v1/references?to=" + url.QueryEscape(shape.into)
		}},
		{name: "the backlink counts (GET /api/v1/issues/{key})", path: func(shape *predicateCostShape) string {
			return "/api/v1/issues/" + shape.issue
		}},
		{name: "the issue tree (GET /api/v1/issues?parent=)", path: predicateCostTreePath},
		{name: "GET /api/v1/lenses with its chains", headOnly: true, path: func(shape *predicateCostShape) string {
			return "/api/v1/lenses?on=" + url.QueryEscape(shape.documentRef)
		}},
		{name: "the coverage read (GET /api/v1/issues/{key}/coverage)", path: func(shape *predicateCostShape) string {
			return "/api/v1/issues/" + shape.issue + "/coverage"
		}},
		{name: "the references panel read (GET /api/v1/issues/{key}/references)", path: func(shape *predicateCostShape) string {
			return "/api/v1/issues/" + shape.issue + "/references"
		}},
	}
}

// predicateCostTable prints every reading of the run: the nearest-rank p50, p95 and p99 of each
// read over each shape on each build, then every reading it took, in the order it was taken, so
// a reader sees the spread each percentile came out of.
func predicateCostTable(t *testing.T, readings []costReading) {
	t.Helper()
	t.Log("read\tshape\tbuild\tp50 ms\tp95 ms\tp99 ms\treadings")
	for _, reading := range readings {
		t.Logf("%s\t%s\t%s\t%.1f\t%.1f\t%.1f\t%d",
			reading.operation, reading.shape, reading.arm, costMilliseconds(reading.p50()),
			costMilliseconds(reading.percentile(0.95)), costMilliseconds(reading.p99()), len(reading.runs))
	}
	t.Log("every reading, in the order it was taken, in milliseconds:")
	for _, reading := range readings {
		t.Logf("%s\t%s\t%s\t%s", reading.operation, reading.shape, reading.arm, reading.printed())
	}
}

// predicateCostCount is a count as the check writes one, with a comma between each three digits.
func predicateCostCount(count int) string {
	digits := strconv.Itoa(count)
	var written strings.Builder
	for index, digit := range digits {
		if index > 0 && (len(digits)-index)%3 == 0 {
			written.WriteByte(',')
		}
		written.WriteRune(digit)
	}
	return written.String()
}

// predicateCostDocumentOf is the address of the document a block address `<document>#b-<id>`
// names a block of.
func predicateCostDocumentOf(ref string) string {
	if cut := strings.LastIndex(ref, "#b-"); cut >= 0 {
		return ref[:cut]
	}
	return ref
}

// predicateCostBlockID is the block id a block address `<document>#b-<id>` names.
func predicateCostBlockID(ref string) string {
	if cut := strings.LastIndex(ref, "#b-"); cut >= 0 {
		return ref[cut+len("#b-"):]
	}
	return ref
}

// predicateCostShapePhrase is the word one shape's main document carries and no other document of
// the corpus does: a search that matched every shape would be one read measured four times.
func predicateCostShapePhrase(shape string) string { return predicateCostPhrase + shape }

// ───── the run ────────────────────────────────────────────────────────────

// The reads this child touches get no slower, over the shapes that can make one ruinous.
//
// This child adds five kinds to the reads of the graph and changes what each read takes as
// present (a node not gone, rather than a node live), so a read that is unchanged on one shape can
// be ruinous on another. The run states its four shapes and prints the row counts of each: an
// issue of 8,001 blocks nested eight deep, a node with 5,000 inbound tuples and a person with
// 50,000 rows of unrelated history, which both builds write through their own APIs with the same
// requests; and, on this head alone, a spec with a chain of 50 triggered lenses, half of them
// reserved for their author. The first three shapes hold no node reserved for its author, because
// no block of a document takes `author`, and the run asserts that of this head's store.
//
// It measures nine reads over each shape: the list of a kind, search, the tuples out of a node, the
// tuples into a node, the backlink counts, the issue tree (the list of the shape's issue's
// children, each shape's issue holding predicateCostChildren), GET /api/v1/lenses with its chains,
// the coverage read and the references panel read, each named with its route. The base build is the
// commit BASELINE_SHA names (baseBuild),
// serving a database of its own (startCostArms). Both builds are read in one loop, one reading on
// each in each turn, the build that goes first alternating, after one warm-up reading on each that
// is not counted (costInterleaved). The corpus is vacuumed and analysed after the seed and the
// backfill (costArm.vacuumAnalyze), and the block-index lease of each build is held while the
// readings are taken (costHoldBlockIndex).
// It takes 200 readings of each read over each shape on each build and prints the nearest-rank
// p50, p95 and p99. A read whose p50 at this head is more than 1.25 times its p50 at the base
// build fails the test, and the failure prints the read, the shape and both builds' readings. The
// p95 and the p99 bind nothing. GET /api/v1/lenses and the fourth shape have no reading at the
// base build, so they are printed and bind nothing either.
//
// It runs only where LEGION_SCALE_TEST=1 is set, under the basebuild tag, which no CI job builds.
// LEGION_READ_COST_SCALE writes and reads a share of these sizes, for a smoke of the test that is
// no reading of the check: such a run measures and prints every reading and then ends in SKIP.
// LEGION_READ_COST_CONTROL set to drop-inbound-indexes runs the check's negative control
// (predicateCostControlEnv), which fails the run, or ends it in SKIP where no read crossed the
// bound, a control that did not bite.
func TestThePredicateAndTheLensReadsGetNoSlower(t *testing.T) {
	if os.Getenv(predicateCostScaleTestEnv) != "1" {
		t.Skipf("set %s=1 to run c-read-cost: it builds the base commit, writes about 70,000 blocks into each of two databases and takes about 12,000 timed readings",
			predicateCostScaleTestEnv)
	}
	sizes := predicateCostSizesOf(t)
	control := predicateCostControlOf(t)
	t.Log(costGVisorNote)
	t.Log("how each build is driven: both are server processes of their own build, each serving a " +
		"database of its own which that build migrated at boot, and each is driven over its own HTTP " +
		"API by this test, which encodes every request before the clock starts and reads every answer " +
		"to its end before it stops. This head is read after its backfill, so every reading of it is a " +
		"reading of the store's own arm.")
	if sizes.scale != 1 {
		t.Logf("%s=%g: this run writes and reads that share of the check's sizes, so it is a smoke of this test and "+
			"no reading of c-read-cost; every shape below states the size it was written at, and the run ends in SKIP "+
			"once every reading is printed", predicateCostScaleEnv, sizes.scale)
	}
	if control != "" {
		t.Logf("%s=%s: this run is the check's negative control, which drops %v from this head's database once its corpus "+
			"is written and analysed, and is no reading of c-read-cost", predicateCostControlEnv, control, predicateCostInboundIndexes)
	}
	t.Logf("the shapes: %q, %q and %q, which both builds write; and %q, which this head alone writes. "+
		"Each read takes %d reading(s) over each shape on each build that serves it, after %d warm-up reading(s) not counted",
		sizes.heavyName(), sizes.inboundName(), sizes.historyName(), sizes.lensName(), sizes.readings, costWarmUp)

	listener := costEnvoy(t)
	listener.Accept(true)
	head, base := startCostArms(t, listener.URL)
	builds := []*costArm{head, base}
	shapes := map[*costArm][]*predicateCostShape{}
	for _, arm := range builds {
		shapes[arm] = seedPredicateCost(t, arm, sizes)
	}
	shapes[head] = append(shapes[head], seedPredicateCostLenses(t, head, sizes))
	costRunBackfill(t, head)
	for _, arm := range builds {
		costBlockIndexDrained(t, arm, predicateCostIndexWait)
		arm.vacuumAnalyze(t)
		t.Logf("%s: the whole corpus holds %d document block(s), %d block reference(s) and %d comment(s)%s",
			arm.name,
			arm.count(t, `select count(*) from document_blocks`),
			arm.count(t, `select count(*) from block_refs`),
			arm.count(t, `select count(*) from comments`),
			costStoreCounts(t, arm))
		settlePredicateCost(t, arm, shapes[arm])
	}
	requirePredicateCostAuthorNodes(t, head)
	if control == predicateCostDropInbound {
		inbound := slices.IndexFunc(shapes[head], func(shape *predicateCostShape) bool { return shape.name == sizes.inboundName() })
		if inbound < 0 {
			t.Fatalf("%s holds no shape %q for the control to read", head.name, sizes.inboundName())
		}
		dropPredicateCostInboundIndexes(t, head, shapes[head][inbound])
	}

	// No tick of either server's own runs inside a measured window.
	for _, arm := range builds {
		defer costHoldBlockIndex(t, arm)()
	}

	reads := predicateCostReads()
	readings := []costReading{}
	held := map[string]costReading{}
	cell := func(read string, shape int, arm *costArm) string {
		return read + "\x00" + strconv.Itoa(shape) + "\x00" + arm.name
	}
	for _, read := range reads {
		for index, shape := range shapes[head] {
			measured := builds
			if read.headOnly || shape.headOnly {
				measured = []*costArm{head}
			}
			taken := costInterleaved(t, read.name, shape.name, sizes.readings, measured, func(subject *costArm, _ int) time.Duration {
				subject.ready(t)
				path := read.path(shapes[subject][index])
				reading, _ := subject.timed(t, http.MethodGet, path, "", http.StatusOK)
				return reading
			})
			for _, subject := range measured {
				readings = append(readings, taken[subject])
				held[cell(read.name, index, subject)] = taken[subject]
			}
		}
	}
	predicateCostTable(t, readings)

	t.Logf("the bound: a read whose p50 at %s is more than %.2f times its p50 at %s fails; the p95 and the p99 are printed and bind nothing",
		head.name, predicateCostRatio, base.name)
	// A smoke reads too few readings over too small a corpus to be a reading of the bound, so it
	// prints a cell over the bound in the same words and fails nothing, and ends in the SKIP below:
	// a test Go reports failed and then skipped is reported FAIL.
	overBound, crossed := t.Errorf, false
	if sizes.scale != 1 {
		overBound = t.Logf
	}
	for _, read := range reads {
		for index, shape := range shapes[head] {
			here := held[cell(read.name, index, head)]
			if read.headOnly || shape.headOnly {
				why := "the base build holds no such shape, a lens being this child's kind"
				if read.headOnly {
					why = "the base build serves no such read, which this child adds"
				}
				t.Logf("%s over %q: p50 %.1f ms, p95 %.1f ms, p99 %.1f ms at %s. It has no reading at the base build, because %s, so it is printed and binds nothing.",
					read.name, shape.name, costMilliseconds(here.p50()), costMilliseconds(here.percentile(0.95)),
					costMilliseconds(here.p99()), head.name, why)
				continue
			}
			before := held[cell(read.name, index, base)]
			ratio := float64(here.p50()) / float64(before.p50())
			t.Logf("%s over %q: p50 %.1f ms at %s against %.1f ms at %s, %.2f times it, against the %.2f bound "+
				"(p95 %.1f against %.1f ms and p99 %.1f against %.1f ms, which bind nothing)",
				read.name, shape.name, costMilliseconds(here.p50()), head.name, costMilliseconds(before.p50()), base.name,
				ratio, predicateCostRatio, costMilliseconds(here.percentile(0.95)), costMilliseconds(before.percentile(0.95)),
				costMilliseconds(here.p99()), costMilliseconds(before.p99()))
			if float64(here.p50()) > predicateCostRatio*float64(before.p50()) {
				crossed = true
				overBound("%s over %q: p50 %.1f ms at %s is %.2f times the %.1f ms at %s, more than the %.2f bound. "+
					"Every reading at %s, in milliseconds, in the order taken: %s. Every reading at %s: %s",
					read.name, shape.name, costMilliseconds(here.p50()), head.name, ratio,
					costMilliseconds(before.p50()), base.name, predicateCostRatio,
					head.name, here.printed(), base.name, before.printed())
			}
		}
	}
	if control != "" && !crossed {
		t.Skipf("%s=%s: every read held the %.2f bound with %v dropped from %s's database, so the control did not bite; "+
			"a run with it set is no reading of c-read-cost", predicateCostControlEnv, control, predicateCostRatio,
			predicateCostInboundIndexes, head.name)
	}
	if sizes.scale != 1 {
		t.Skipf("%s=%g: this run wrote and read that share of the check's sizes, a smoke of this test whose readings are printed above "+
			"and no reading of c-read-cost", predicateCostScaleEnv, sizes.scale)
	}
}
