//go:build basebuild

// The basebuild tag, and how a test of it is run: store_census_test.go.

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

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
