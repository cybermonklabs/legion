package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// holdIt is the spec's second callout, written with its id, hold-it.
func holdIt(body string) string {
	return ":::callout{#hold-it kind=\"note\" title=\"\"}\n" + body + "\n:::"
}

// readIssueDecisions is GET /api/v1/issues/{key}/decisions as alice.
func readIssueDecisions(t *testing.T, handler http.Handler, key string) issueDecisions {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/decisions", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/issues/%s/decisions: status=%d body=%s", key, response.Code, response.Body.String())
	}
	return decodeBody[issueDecisions](t, response)
}

// decisionNamed is the decision whose heading is heading, or a failure naming what the view holds.
func decisionNamed(t *testing.T, view issueDecisions, heading string) model.Decision {
	t.Helper()
	for _, decision := range view.Decisions {
		if decision.Heading == heading {
			return decision
		}
	}
	headings := make([]string, 0, len(view.Decisions))
	for _, decision := range view.Decisions {
		headings = append(headings, decision.Heading)
	}
	t.Fatalf("no decision %q; the view holds %v", heading, headings)
	return model.Decision{}
}

// staleTargets is each spec block a decision's links mark out of date, by block id.
func staleTargets(decision model.Decision) []string {
	stale := []string{}
	for _, link := range decision.Links {
		if link.Stale {
			stale = append(stale, link.To.BlockID)
		}
	}
	return stale
}

// linkTargets is each spec block a decision links, by block id, in the order the view lists them.
func linkTargets(decision model.Decision) []string {
	targets := make([]string, 0, len(decision.Links))
	for _, link := range decision.Links {
		targets = append(targets, link.To.BlockID)
	}
	return targets
}

// decisionsFixture is one issue whose spec holds two linkable blocks and whose decisions.md holds
// an agent decision linking both, a person's decision linking an answered ask and one of them, an
// unsure decision, and a replaced one.
type decisionsFixture struct {
	handler   http.Handler
	database  *store.Store
	deps      Deps
	issue     model.Issue
	decisions model.Artifact
	askID     string
}

const (
	tableDecision   = "Store notices in a table"
	nameDecision    = "Use the name the person gave"
	cacheDecision   = "Keep the cache warm"
	fileDecision    = "Store notices in a file"
	tableParagraph  = "Chosen for the simplest storage."
	changedTableSay = "Chosen for the simplest storage, after measuring."
)

func newDecisionsFixture(t *testing.T) decisionsFixture {
	t.Helper()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE",
		shipIt("Ship it.")+"\n\n"+holdIt("Hold it.")+"\n\nOther paragraph.\n")
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Which name do we use?",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("open the ask: status=%d body=%s", created.Code, created.Body.String())
	}
	askID := decodeBody[struct {
		ID string `json:"id"`
	}](t, created).ID
	spec := "dispatch://" + issue.Key + "/spec"
	markdown := fmt.Sprintf(`## %s

%s Shapes %s#b-ship%%5Fit and %s#b-hold-it.

## %s

Decided in dispatch://%s/ask/%s. Shapes %s#b-hold-it.

## %s

Unsure: a measurement of the hit rate would change this.

## Replaced

### %s

Replaced by the table above.
`, tableDecision, tableParagraph, spec, spec, nameDecision, issue.Key, askID, spec, cacheDecision, fileDecision)
	decisions := uploadIssueDocument(t, handler, issue.Key, "decisions.md", markdown)
	return decisionsFixture{handler: handler, database: database, deps: deps, issue: issue, decisions: decisions, askID: askID}
}

// specVersionCount is how many versions the issue's spec has.
func specVersionCount(t *testing.T, database *store.Store, artifactID string) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from artifact_versions where artifact_id = $1`, artifactID).Scan(&count); err != nil {
		t.Fatalf("count the spec's versions: %v", err)
	}
	return count
}

// The view reads decisions.md field by field: each `##` section in document order, the `###`
// sections under `## Replaced` marked replaced, a decision that mentions an ask the person's, a
// paragraph that starts `Unsure:` marking its decision unsure, and every link to a spec block.
// Right after the decisions are written no link is stale, and writing decisions.md wrote the spec
// no version of its own.
func TestIssueDecisionsReadsEachDecisionOfTheDocument(t *testing.T) {
	fixture := newDecisionsFixture(t)
	specVersions := specVersionCount(t, fixture.database, fixture.issue.PrimaryArtifactID)

	view := readIssueDecisions(t, fixture.handler, fixture.issue.Key)
	want := "dispatch://" + fixture.issue.Key + "/artifact/" + fixture.decisions.Slug
	if view.Document == nil || *view.Document != want {
		t.Fatalf("document = %v, want %q", view.Document, want)
	}
	headings := make([]string, 0, len(view.Decisions))
	for _, decision := range view.Decisions {
		headings = append(headings, decision.Heading)
	}
	wantHeadings := []string{tableDecision, nameDecision, cacheDecision, fileDecision}
	if fmt.Sprint(headings) != fmt.Sprint(wantHeadings) {
		t.Fatalf("headings = %v, want %v", headings, wantHeadings)
	}

	table := decisionNamed(t, view, tableDecision)
	if table.Kind != "agent" || table.Unsure || table.Replaced || len(table.Asks) != 0 {
		t.Fatalf("the agent decision = %#v, want an agent decision that is neither unsure nor replaced", table)
	}
	if table.Kept != nil || !table.WaitingReview {
		t.Fatalf("the agent decision's kept = %#v, waiting_review = %t, want null and true", table.Kept, table.WaitingReview)
	}
	if table.Ref != want+"#b-"+table.BlockID {
		t.Fatalf("ref = %q, want %q", table.Ref, want+"#b-"+table.BlockID)
	}
	if table.ChangedVersion != 1 || table.ChangedAt == nil {
		t.Fatalf("changed_version = %d, changed_at = %v, want 1 and the version's time", table.ChangedVersion, table.ChangedAt)
	}
	if got := fmt.Sprint(linkTargets(table)); got != "[hold-it ship_it]" && got != "[ship_it hold-it]" {
		t.Fatalf("the agent decision's links = %v, want both spec blocks", linkTargets(table))
	}
	if !strings.Contains(table.Text, tableParagraph) || !strings.Contains(table.Text, "## "+tableDecision) {
		t.Fatalf("text = %q, want the heading and its paragraph", table.Text)
	}

	name := decisionNamed(t, view, nameDecision)
	if name.Kind != "person" || len(name.Asks) != 1 || name.Asks[0] != fixture.askID {
		t.Fatalf("the person's decision = %#v, want kind person naming ask %s", name, fixture.askID)
	}
	if got := fmt.Sprint(linkTargets(name)); got != "[hold-it]" {
		t.Fatalf("the person's decision's links = %v, want hold-it", linkTargets(name))
	}
	if name.WaitingReview {
		t.Fatalf("a person's decision waits for no review: %#v", name)
	}

	cache := decisionNamed(t, view, cacheDecision)
	if !cache.Unsure || cache.Replaced || len(cache.Links) != 0 {
		t.Fatalf("the unsure decision = %#v, want unsure, not replaced, with no link", cache)
	}

	file := decisionNamed(t, view, fileDecision)
	if !file.Replaced || file.WaitingReview {
		t.Fatalf("the replaced decision = %#v, want replaced and waiting for no review", file)
	}

	for _, decision := range view.Decisions {
		if stale := staleTargets(decision); len(stale) != 0 {
			t.Fatalf("%q marks %v out of date right after the decisions were written", decision.Heading, stale)
		}
	}
	if got := specVersionCount(t, fixture.database, fixture.issue.PrimaryArtifactID); got != specVersions {
		t.Fatalf("the spec has %d versions, want %d: writing decisions.md writes the spec none", got, specVersions)
	}
	// The view answers each decision's text with the reading Go made of it, so the panel that
	// lays a decision out block by block takes those blocks from this tree rather than splitting
	// the markdown in the browser.
	requireDisplayTree(t, fixture.handler, "/api/v1/issues/"+fixture.issue.Key+"/decisions",
		"decisions.0.text", "decisions.0.text_tree")
}

// A link added to an existing decision is not out of date; a version that changes the decision's
// own text makes every link it already held out of date; and a later version of one linked spec
// block clears that link alone.
func TestDecisionLinksGoStaleWithTheDecisionAndClearWithTheirBlock(t *testing.T) {
	fixture := newDecisionsFixture(t)
	handler, key := fixture.handler, fixture.issue.Key

	// A link added to an existing decision: the paragraph that adds it is new, so the decision
	// changed in the version that wrote it, which is the version the new pair was created in.
	added := "Also shapes dispatch://" + key + "/spec#b-ship%5Fit."
	editIssueDocument(t, handler, key, fixture.decisions.Slug, []map[string]any{
		{"op": "insert", "markdown": added, "before": "## " + cacheDecision},
	})
	name := decisionNamed(t, readIssueDecisions(t, handler, key), nameDecision)
	for _, link := range name.Links {
		if link.To.BlockID == "ship_it" && link.Stale {
			t.Fatalf("the link a version added is out of date: %#v", link)
		}
	}

	// A version that changes the agent decision's own text: both links it already held are out of
	// date, the spec not having changed since.
	editIssueDocument(t, handler, key, fixture.decisions.Slug, []map[string]any{
		{"op": "replace", "find": tableParagraph, "with": changedTableSay},
	})
	table := decisionNamed(t, readIssueDecisions(t, handler, key), tableDecision)
	if got := len(staleTargets(table)); got != 2 {
		t.Fatalf("after the decision changed its links mark %v out of date, want both spec blocks", staleTargets(table))
	}

	// A later version of one of those spec blocks clears its link alone.
	editSpec(t, handler, key, []map[string]any{{"op": "replace", "find": "Ship it.", "with": "Ship it, soon."}})
	table = decisionNamed(t, readIssueDecisions(t, handler, key), tableDecision)
	if got := fmt.Sprint(staleTargets(table)); got != "[hold-it]" {
		t.Fatalf("after the spec block changed the decision marks %v out of date, want hold-it alone", staleTargets(table))
	}
}

// A `#` heading after a decision ends it, as the block walk ends it, so text under that heading
// is no part of the decision and changing it marks nothing; and a heading the document carries
// twice - a decision made again after a replaced one of the same name - still names each of its
// decisions, so the live one's links go out of date with its own text.
func TestTheVersionCutReadsTheSameDecisionsAsTheBlockWalk(t *testing.T) {
	fixture := newDecisionsFixture(t)
	handler, key := fixture.handler, fixture.issue.Key

	// A `#` heading and a paragraph written after the agent decision, before the next one.
	editIssueDocument(t, handler, key, fixture.decisions.Slug, []map[string]any{
		{"op": "insert", "markdown": "# Notes\n\nA note.", "before": "## " + nameDecision},
	})
	table := decisionNamed(t, readIssueDecisions(t, handler, key), tableDecision)
	if strings.Contains(table.Text, "A note.") {
		t.Fatalf("the `#` heading did not end the decision: %q", table.Text)
	}
	if stale := staleTargets(table); len(stale) != 0 {
		t.Fatalf("a `#` heading written after the decision marks %v out of date", stale)
	}
	editIssueDocument(t, handler, key, fixture.decisions.Slug, []map[string]any{
		{"op": "replace", "find": "A note.", "with": "A changed note."},
	})
	if table = decisionNamed(t, readIssueDecisions(t, handler, key), tableDecision); len(staleTargets(table)) != 0 {
		t.Fatalf("text under the `#` heading marks %v out of date", staleTargets(table))
	}

	// The same heading under `## Replaced`: the document now carries it twice, and the live
	// decision's own change still marks the blocks it links.
	editIssueDocument(t, handler, key, fixture.decisions.Slug, []map[string]any{
		{"op": "insert", "markdown": "### " + tableDecision + "\n\nThe file store we tried first.", "after": "## Replaced"},
	})
	editIssueDocument(t, handler, key, fixture.decisions.Slug, []map[string]any{
		{"op": "replace", "find": tableParagraph, "with": changedTableSay},
	})
	view := readIssueDecisions(t, handler, key)
	live := view.Decisions[0]
	if live.Heading != tableDecision || live.Replaced {
		t.Fatalf("the document's first decision = %#v, want the live %q", live, tableDecision)
	}
	if got := len(staleTargets(live)); got != 2 {
		t.Fatalf("beside a replaced decision of its own heading the live one marks %v out of date, want both spec blocks",
			staleTargets(live))
	}
}

// Keep records a person's agreement with the text they read, which stops the decision waiting for
// review; a later change of the decision asks for review again, and a Keep that carries the text
// before that change is refused. A bearer may not keep, and a block that is no decision heading
// is not a target.
func TestKeepRecordsAgreementUntilTheDecisionChanges(t *testing.T) {
	fixture := newDecisionsFixture(t)
	handler, key := fixture.handler, fixture.issue.Key
	table := decisionNamed(t, readIssueDecisions(t, handler, key), tableDecision)

	keepPath := "/api/v1/artifacts/" + fixture.decisions.ID + "/blocks/" + table.BlockID + "/keep"
	kept := dispatchRequest(t, handler, http.MethodPost, keepPath,
		map[string]any{"text_sha256": table.TextSHA256}, "alice")
	if kept.Code != http.StatusOK {
		t.Fatalf("keep: status=%d body=%s", kept.Code, kept.Body.String())
	}
	keep := decodeBody[model.DecisionKeep](t, kept)
	if keep.BlockID != table.BlockID || keep.ChangedVersion != table.ChangedVersion || keep.KeptBy != "alice" {
		t.Fatalf("keep = %#v, want %s at version %d by alice", keep, table.BlockID, table.ChangedVersion)
	}
	after := decisionNamed(t, readIssueDecisions(t, handler, key), tableDecision)
	if after.Kept == nil || after.Kept.ChangedVersion != table.ChangedVersion || after.WaitingReview {
		t.Fatalf("after Keep the decision = %#v, want it kept and waiting for no review", after)
	}

	editIssueDocument(t, handler, key, fixture.decisions.Slug, []map[string]any{
		{"op": "replace", "find": tableParagraph, "with": changedTableSay},
	})
	changed := decisionNamed(t, readIssueDecisions(t, handler, key), tableDecision)
	if changed.Kept == nil || !changed.WaitingReview {
		t.Fatalf("after the decision changed it = %#v, want it kept at the older version and waiting for review", changed)
	}

	// The Keep of the text the person read before that change is refused, and writes nothing: a
	// Keep that stood would record an agreement with words nobody read.
	stale := dispatchRequest(t, handler, http.MethodPost, keepPath,
		map[string]any{"text_sha256": table.TextSHA256}, "alice")
	if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), "DECISION_CHANGED") {
		t.Fatalf("a Keep of the text before the change: status=%d body=%s, want 409 DECISION_CHANGED",
			stale.Code, stale.Body.String())
	}
	if again := decisionNamed(t, readIssueDecisions(t, handler, key), tableDecision); !again.WaitingReview {
		t.Fatalf("the refused Keep was recorded: %#v", again)
	}
	keepBlock(t, handler, fixture.decisions.ID, changed.BlockID, changed.TextSHA256)
	if now := decisionNamed(t, readIssueDecisions(t, handler, key), tableDecision); now.WaitingReview {
		t.Fatalf("after a Keep of the text as it stands the decision = %#v, want it kept", now)
	}

	// A Keep is the person's own agreement, so the route needs the person scope: the deployment's
	// shared token holds none and is refused, naming it.
	bearer := sessionRequest(t, handler, http.MethodPost, keepPath, map[string]any{"actor": sessionActor()})
	if bearer.Code != http.StatusForbidden || !strings.Contains(bearer.Body.String(), codeScopeRequired) {
		t.Fatalf("a bearer's keep: status=%d body=%s, want 403 %s", bearer.Code, bearer.Body.String(), codeScopeRequired)
	}

	blocks := blockIDsByText(t, handler, fixture.decisions.ID)
	paragraph := blocks[changedTableSay+" Shapes dispatch://"+key+"/spec#b-ship%5Fit and dispatch://"+key+"/spec#b-hold-it."]
	if paragraph == "" {
		t.Fatalf("the decision's paragraph has no block id; blocks = %v", blocks)
	}
	notHeading := dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+fixture.decisions.ID+"/blocks/"+paragraph+"/keep",
		map[string]any{"text_sha256": table.TextSHA256}, "alice")
	if notHeading.Code != http.StatusNotFound || !strings.Contains(notHeading.Body.String(), "TARGET_NOT_FOUND") {
		t.Fatalf("keep on a block that is no decision heading: status=%d body=%s, want 404 TARGET_NOT_FOUND",
			notHeading.Code, notHeading.Body.String())
	}
}

// keepBlock is a person's Keep of the decision that heading block opens, at the text sum the
// view served them.
func keepBlock(t *testing.T, handler http.Handler, artifactID, blockID, sum string) model.DecisionKeep {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+artifactID+"/blocks/"+blockID+"/keep",
		map[string]any{"text_sha256": sum}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("keep %s: status=%d body=%s", blockID, response.Code, response.Body.String())
	}
	return decodeBody[model.DecisionKeep](t, response)
}

// A kept decision whose text changes without its changed version moving waits for review again,
// and the spec blocks it linked are out of date. The version is the greatest of the blocks the
// decision holds now, so a block taken out of it, and one moved from it into another decision,
// leave both decisions' versions where they were while their text changed: the text, not the
// version, is what Keep records and what the out-of-date rule reads.
func TestAKeptDecisionWaitsForReviewAgainWhenABlockLeavesOrJoinsIt(t *testing.T) {
	fixture := newDecisionsFixture(t)
	handler, key, id := fixture.handler, fixture.issue.Key, fixture.decisions.ID

	// A paragraph written into the agent decision: its text changed after it linked both spec
	// blocks, so both are out of date, and a Keep agrees with the text as it now stands.
	const measured = "Measured against the file store."
	editIssueDocument(t, handler, key, fixture.decisions.Slug, []map[string]any{
		{"op": "insert", "markdown": measured, "before": "## " + nameDecision},
	})
	table := decisionNamed(t, readIssueDecisions(t, handler, key), tableDecision)
	if got := fmt.Sprint(staleTargets(table)); got != "[hold-it ship_it]" && got != "[ship_it hold-it]" {
		t.Fatalf("after a block joined it the decision marks %v out of date, want both spec blocks",
			staleTargets(table))
	}
	keepBlock(t, handler, id, table.BlockID, table.TextSHA256)
	if kept := decisionNamed(t, readIssueDecisions(t, handler, key), tableDecision); kept.WaitingReview {
		t.Fatalf("after the Keep the decision = %#v, want it waiting for no review", kept)
	}

	// The paragraph taken out again: the decision's text is the text it linked those blocks
	// with, so neither is out of date any more, while the Keep, which agreed with the
	// paragraph, no longer holds - and the decision's changed version moved for neither.
	editIssueDocument(t, handler, key, fixture.decisions.Slug, []map[string]any{
		{"op": "delete", "find": measured},
	})
	removed := decisionNamed(t, readIssueDecisions(t, handler, key), tableDecision)
	if strings.Contains(removed.Text, measured) {
		t.Fatalf("the paragraph is still in the decision: %q", removed.Text)
	}
	if removed.Kept == nil || removed.Kept.ChangedVersion < removed.ChangedVersion {
		t.Fatalf("the removal moved the decision's changed version: kept = %#v, decision = %#v",
			removed.Kept, removed)
	}
	if !removed.WaitingReview {
		t.Fatalf("after a block left it the decision = %#v, want it waiting for review", removed)
	}
	if stale := staleTargets(removed); len(stale) != 0 {
		t.Fatalf("the decision's text is what it linked those blocks with, and it marks %v", stale)
	}

	// Both decisions kept as they now stand, then the agent decision's paragraph moved into the
	// unsure one: no block of either changed, so neither changed version moves.
	view := readIssueDecisions(t, handler, key)
	table = decisionNamed(t, view, tableDecision)
	cache := decisionNamed(t, view, cacheDecision)
	keepBlock(t, handler, id, table.BlockID, table.TextSHA256)
	keepBlock(t, handler, id, cache.BlockID, cache.TextSHA256)
	blocks := blockIDsByText(t, handler, id)
	paragraph := blocks[tableParagraph+" Shapes dispatch://"+key+"/spec#b-ship%5Fit and dispatch://"+key+"/spec#b-hold-it."]
	if paragraph == "" {
		t.Fatalf("the decision's paragraph has no block id; blocks = %v", blocks)
	}
	editIssueDocument(t, handler, key, fixture.decisions.Slug, []map[string]any{
		{"op": "move", "block": paragraph, "before": "## Replaced"},
	})
	moved := readIssueDecisions(t, handler, key)
	left := decisionNamed(t, moved, tableDecision)
	joined := decisionNamed(t, moved, cacheDecision)
	if strings.Contains(left.Text, tableParagraph) || !strings.Contains(joined.Text, tableParagraph) {
		t.Fatalf("the move left the paragraph where it was: %q and %q", left.Text, joined.Text)
	}
	for _, decision := range []model.Decision{left, joined} {
		if decision.Kept == nil || decision.Kept.ChangedVersion < decision.ChangedVersion {
			t.Fatalf("the move moved %q's changed version: kept = %#v, decision = %#v",
				decision.Heading, decision.Kept, decision)
		}
		if !decision.WaitingReview {
			t.Fatalf("after the move %q = %#v, want it waiting for review", decision.Heading, decision)
		}
	}
	// The links moved with the paragraph, and both spec blocks are out of date under the
	// decision that holds them now: the decision they joined changed when they arrived.
	if got := fmt.Sprint(staleTargets(joined)); got != "[hold-it ship_it]" && got != "[ship_it hold-it]" {
		t.Fatalf("the decision the paragraph joined marks %v out of date, want both spec blocks",
			staleTargets(joined))
	}
	if len(left.Links) != 0 {
		t.Fatalf("the decision the paragraph left still holds %v", linkTargets(left))
	}
}

// listDecision is the decision of the renderer change tests: an agent decision whose paragraph
// links the spec block hold-it and which ends in a loose list, whose blank lines between items a
// renderer change spells otherwise (respellVersion).
const listDecision = "Ship the notices in three steps"

// keptAcrossARendererChange is newDecisionsFixture with listDecision written into decisions.md,
// kept by alice, and then left as a build of another renderer version leaves a decision it kept:
// the version that wrote the decision stored in that renderer's spelling of the same tree,
// stamped stamp (respellVersion), and the keep holding the sum of the decision's text in that
// spelling beside that version. A keep holds no text, so the view reads the kept text from that
// version. It fails t unless the view reads the decision kept, waiting for no review, with its
// link not out of date, since this build reads that text as the decision's own: the renderer
// changed and nobody changed the decision.
func keptAcrossARendererChange(t *testing.T, stamp *int) (decisionsFixture, model.Decision) {
	t.Helper()
	fixture := newDecisionsFixture(t)
	handler, database, key, id := fixture.handler, fixture.database, fixture.issue.Key, fixture.decisions.ID
	version := editIssueDocument(t, handler, key, fixture.decisions.Slug, []map[string]any{{
		"op":       "insert",
		"markdown": "## " + listDecision + "\n\nShapes dispatch://" + key + "/spec#b-hold-it.\n\n- first\n\n- second\n\n- third",
		"before":   "## Replaced",
	}})
	written := decisionNamed(t, readIssueDecisions(t, handler, key), listDecision)
	if written.ChangedVersion != version.Number || fmt.Sprint(linkTargets(written)) != "[hold-it]" {
		t.Fatalf("the decision = %#v, want it changed at version %d and linking hold-it", written, version.Number)
	}
	keepBlock(t, handler, id, written.BlockID, written.TextSHA256)

	stored := respellVersion(t, database, id, version.Number, stamp)
	first, second := strings.Index(written.Text, "- first\n"), strings.Index(written.Text, "- second\n")
	if first < 0 || second < first {
		t.Fatalf("the decision's text = %q, want a list whose first two items are first and second", written.Text)
	}
	kept := written.Text[:first] + "- first\n" + written.Text[second:]
	if !strings.Contains(stored, kept+"\n") {
		t.Fatalf("version %d = %q, want it to hold the decision as %q", version.Number, stored, kept)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		update decision_keeps set text_sha256 = $3 where artifact_id = $1 and block_id = $2
	`, id, written.BlockID, decisionTextSum(kept)); err != nil {
		t.Fatalf("keep the decision as another renderer version spelled it: %v", err)
	}

	decision := decisionNamed(t, readIssueDecisions(t, handler, key), listDecision)
	if decision.Text != written.Text {
		t.Fatalf("the decision's text = %q, want %q: the version's spelling is no part of it", decision.Text, written.Text)
	}
	if decision.Kept == nil || decision.Kept.ChangedVersion != version.Number || decision.WaitingReview {
		t.Fatalf("the decision kept as another renderer version spelled it = %#v, want it kept at version %d and waiting for no review",
			decision, version.Number)
	}
	if stale := staleTargets(decision); len(stale) != 0 {
		t.Fatalf("the decision kept as another renderer version spelled it marks %v out of date", stale)
	}
	return fixture, decision
}

// A renderer change (pmdoc.RendererVersion) leaves every version stored before it in its
// renderer's spelling of the document, and a keep holds only the sum of the text it agreed with,
// so a decision kept before the change sums otherwise in this build's spelling. The version its
// keep names, stamped with another renderer version or with none, holds the kept text, and this
// build reads that text as the decision's: the decision stays kept, waiting for no review, and the
// spec block it links stays up to date (keptAcrossARendererChange).
func TestADecisionKeptBeforeARendererChangeStaysKept(t *testing.T) {
	another := pmdoc.RendererVersion - 1
	for _, test := range []struct {
		name  string
		stamp *int
	}{
		{"stamped with another renderer version", &another},
		{"stamped with none", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			keptAcrossARendererChange(t, test.stamp)
		})
	}
}

// A decision kept before a renderer change whose content changed since waits for review again,
// and the spec block it links is out of date: the kept text its keep's version holds reads as
// another text than the decision's now.
func TestADecisionKeptBeforeARendererChangeWaitsOnceItsContentChanges(t *testing.T) {
	another := pmdoc.RendererVersion - 1
	fixture, kept := keptAcrossARendererChange(t, &another)
	handler, key := fixture.handler, fixture.issue.Key
	editIssueDocument(t, handler, key, fixture.decisions.Slug, []map[string]any{
		{"op": "replace", "find": "third", "with": "third, after measuring"},
	})
	changed := decisionNamed(t, readIssueDecisions(t, handler, key), listDecision)
	if !strings.Contains(changed.Text, "third, after measuring") {
		t.Fatalf("the decision's text = %q, want the changed item", changed.Text)
	}
	if changed.Kept == nil || changed.Kept.ChangedVersion != kept.Kept.ChangedVersion || !changed.WaitingReview {
		t.Fatalf("after its content changed the decision = %#v, want it kept at version %d and waiting for review",
			changed, kept.Kept.ChangedVersion)
	}
	if got := fmt.Sprint(staleTargets(changed)); got != "[hold-it]" {
		t.Fatalf("after its content changed the decision marks %v out of date, want hold-it", staleTargets(changed))
	}
}

// A decision kept before a renderer change whose heading changed since waits for review again:
// the version its keep names holds no decision of that heading, so it holds no kept text of this
// decision to compare.
func TestADecisionKeptBeforeARendererChangeWaitsOnceItsKeepsVersionHoldsNoSuchHeading(t *testing.T) {
	another := pmdoc.RendererVersion - 1
	fixture, kept := keptAcrossARendererChange(t, &another)
	handler, key := fixture.handler, fixture.issue.Key
	const renamed = listDecision + ", one a week"
	editIssueDocument(t, handler, key, fixture.decisions.Slug, []map[string]any{
		{"op": "replace", "find": listDecision, "with": renamed},
	})
	decision := decisionNamed(t, readIssueDecisions(t, handler, key), renamed)
	if decision.BlockID != kept.BlockID || decision.Kept == nil ||
		decision.Kept.ChangedVersion != kept.Kept.ChangedVersion || !decision.WaitingReview {
		t.Fatalf("after its heading changed the decision = %#v, want %s kept at version %d and waiting for review",
			decision, kept.BlockID, kept.Kept.ChangedVersion)
	}
}

// footnotedDecision is the decision of the footnote test: an agent decision whose paragraph links
// the spec block hold-it and cites the footnote 1, which the section after it defines.
const footnotedDecision = "Count the notices by footnote"

// Whether `[^1]` in a decision's text reads as a footnote reference or as text depends on the
// footnotes the whole document defines, and the text of a decision cut from a stored version is
// read alone, away from the section that defines 1. alice keeps a decision at a version that a
// build of another renderer version stored with the decision's `[^1]` as literal text, escaped
// `\[^1]` because another section defines 1, and the decision's text now holds the reference
// `[^1]`. Read alone, the two read as one, though the decision holds another tree, so the
// decision is compared by its text's sum (pmdoc.FragmentReadsAs): it waits for review, and the
// spec block it links is out of date.
func TestADecisionKeptAsFootnoteShapedTextWaitsOnceItCitesTheFootnote(t *testing.T) {
	fixture := newDecisionsFixture(t)
	handler, database, key, id := fixture.handler, fixture.database, fixture.issue.Key, fixture.decisions.ID
	const reference, literal = "See [^1] here.", `See \[^1] here.`
	version := editIssueDocument(t, handler, key, fixture.decisions.Slug, []map[string]any{{
		"op":       "insert",
		"markdown": "## " + footnotedDecision + "\n\nShapes dispatch://" + key + "/spec#b-hold-it. " + reference + "\n\n## Notes\n\n[^1]: The count.",
		"before":   "## Replaced",
	}})
	written := decisionNamed(t, readIssueDecisions(t, handler, key), footnotedDecision)
	if written.ChangedVersion != version.Number || !strings.HasSuffix(written.Text, reference) || fmt.Sprint(linkTargets(written)) != "[hold-it]" {
		t.Fatalf("the decision = %#v, want it changed at version %d, ending %q and linking hold-it", written, version.Number, reference)
	}
	keepBlock(t, handler, id, written.BlockID, written.TextSHA256)

	ctx := context.Background()
	stored := versionText(t, database, id, version.Number)
	kept := strings.TrimSuffix(written.Text, reference) + literal
	// That literal and the reference read alone as one
	// (TestAStoredTextReadsAsAnotherSpellingOfTheSameTree), which is the case FragmentReadsAs refuses.
	if !strings.Contains(stored, written.Text+"\n") {
		t.Fatalf("version %d = %q; the test needs it to hold the decision as %q", version.Number, stored, written.Text)
	}
	another := pmdoc.RendererVersion - 1
	storeVersionAs(t, database, id, version.Number, strings.Replace(stored, written.Text+"\n", kept+"\n", 1), &another)
	if _, err := database.Pool.Exec(ctx, `
		update decision_keeps set text_sha256 = $3 where artifact_id = $1 and block_id = $2
	`, id, written.BlockID, decisionTextSum(kept)); err != nil {
		t.Fatalf("keep the decision as footnote-shaped text: %v", err)
	}

	decision := decisionNamed(t, readIssueDecisions(t, handler, key), footnotedDecision)
	if decision.Text != written.Text {
		t.Fatalf("the decision's text = %q, want %q", decision.Text, written.Text)
	}
	if decision.Kept == nil || decision.Kept.ChangedVersion != version.Number || !decision.WaitingReview {
		t.Errorf("the decision kept as footnote-shaped text = %#v, want it kept at version %d and waiting for review", decision, version.Number)
	}
	if got := fmt.Sprint(staleTargets(decision)); got != "[hold-it]" {
		t.Errorf("the decision kept as footnote-shaped text marks %v out of date, want hold-it", staleTargets(decision))
	}
}

// calloutDecision is the decision of the typed block test: an agent decision whose paragraph
// links the spec block hold-it and which ends in a loose list, standing inside a callout in one
// version and outside it in the other.
const calloutDecision = "Ship the notices beside the callout"

// A heading line inside a typed block starts at column 0, as the typed block's lines carry no
// prefix, so a decision cut from a stored version can start inside one, and its text read alone
// loses the callout it stood in. alice keeps a decision at a version that a build of another
// renderer version stored, its loose list spelled otherwise: inside a callout, a heading inside
// the callout ending it, where the decision now stands outside the callout; or outside it, where
// the decision now stands inside. Each text reads alone as the other, though the decision moved
// across the callout, so the decision is compared by its text's sum: it waits for review, and
// the spec block it links is out of date. So it does where a paragraph opening with a code span
// of three backticks, which holds two in a row, stands before the callout and another inside it
// before the decision: such a line opens no fenced code, and the callout's opening line between
// them is read. So it does too where a list item opening with fenced code stands before the
// callout and another inside it before the decision: each item's code ends at its own closing
// line, indented two spaces, which read alone would open code, and the parser places the callout
// and the decision inside it as they stand.
func TestADecisionKeptOnOneSideOfATypedBlockWaitsOnceItStandsOnTheOther(t *testing.T) {
	for _, test := range []struct {
		name       string
		keptInside bool
		// prefix stands before the callout and inside it before the decision.
		prefix string
	}{
		{"kept inside a callout and standing outside it now", true, ""},
		{"kept outside a callout and standing inside it now", false, ""},
		{"kept inside a callout after paragraphs opening with a code span, and standing outside it now", true, "```a``b``` x\n\n"},
		{"kept inside a callout after list items opening with fenced code, and standing outside it now", true, "- ```\n  code\n  ```\n\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newDecisionsFixture(t)
			handler, database, key, id := fixture.handler, fixture.database, fixture.issue.Key, fixture.decisions.ID
			section := "## " + calloutDecision + "\n\nShapes dispatch://" + key + "/spec#b-hold-it.\n\n- first\n\n- second\n\n- third"
			kept := strings.Replace(section, "- first\n\n- second", "- first\n- second", 1)
			inside := func(text string) string {
				return test.prefix + ":::callout{#aside kind=\"note\" title=\"\"}\n" + test.prefix + text + "\n\n# Notes\n:::"
			}
			outside := func(text string) string { return text + "\n\n# Notes" }
			now, then := outside(section), inside(kept)
			if !test.keptInside {
				now, then = inside(section), outside(kept)
			}
			version := editIssueDocument(t, handler, key, fixture.decisions.Slug, []map[string]any{{
				"op": "insert", "markdown": now, "before": "## Replaced",
			}})
			written := decisionNamed(t, readIssueDecisions(t, handler, key), calloutDecision)
			if written.ChangedVersion != version.Number || written.Text != section || fmt.Sprint(linkTargets(written)) != "[hold-it]" {
				t.Fatalf("the decision = %#v, want it changed at version %d, written %q and linking hold-it", written, version.Number, section)
			}
			keepBlock(t, handler, id, written.BlockID, written.TextSHA256)

			ctx := context.Background()
			stored := versionText(t, database, id, version.Number)
			// The kept text and the text now read alone as one, which is the case a cut inside a
			// typed block refuses.
			if !strings.Contains(stored, now+"\n") {
				t.Fatalf("version %d = %q; the test needs it to hold %q", version.Number, stored, now)
			}
			another := pmdoc.RendererVersion - 1
			storeVersionAs(t, database, id, version.Number, strings.Replace(stored, now+"\n", then+"\n", 1), &another)
			if _, err := database.Pool.Exec(ctx, `
				update decision_keeps set text_sha256 = $3 where artifact_id = $1 and block_id = $2
			`, id, written.BlockID, decisionTextSum(kept)); err != nil {
				t.Fatalf("keep the decision as that version spelled it: %v", err)
			}

			decision := decisionNamed(t, readIssueDecisions(t, handler, key), calloutDecision)
			if decision.Text != section {
				t.Fatalf("the decision's text = %q, want %q", decision.Text, section)
			}
			if decision.Kept == nil || decision.Kept.ChangedVersion != version.Number || !decision.WaitingReview {
				t.Errorf("the decision kept across the callout = %#v, want it kept at version %d and waiting for review", decision, version.Number)
			}
			if got := fmt.Sprint(staleTargets(decision)); got != "[hold-it]" {
				t.Errorf("the decision kept across the callout marks %v out of date, want hold-it", staleTargets(decision))
			}
		})
	}
}

// A version is cut into its decisions at the headings and typed blocks pmdoc's parser places
// (decisionCuts), so a heading inside fenced code is the code's text. A paragraph that opens with a
// code span of three backticks holding two in a row opens no code: the decision after it is cut,
// and a callout opened after another such paragraph holds a decision inside it. A line of backticks
// that also holds an info string closes no code: a heading after it inside the code opens no
// decision. A list item, bullet or ordered, that opens with fenced code ends its code at its own
// closing line, indented past the item's marker, which read alone would open code: a decision
// after the item is cut, standing alone, and a callout after another such item holds a decision
// inside it. A list item's typed block inside a callout ends at its own indented fence, which
// leaves the callout, whose fence the renderer writes longer, holding the decision after it.
func TestDecisionCutsReadCodeFencesAsTheParserDoes(t *testing.T) {
	for _, test := range []struct {
		name, markdown, heading, text string
		alone                         bool
	}{
		{
			name:     "a paragraph that opens with a code span",
			markdown: "```a``b``` x\n\n## D\n\nText.\n",
			heading:  "D",
			text:     "## D\n\nText.",
			alone:    true,
		},
		{
			name:     "a callout after a paragraph that opens with a code span",
			markdown: "```a``b``` x\n\n:::callout{#aside kind=\"note\" title=\"\"}\n```a``b``` x\n\n## D\n\nText.\n:::\n",
			heading:  "D",
			text:     "## D\n\nText.\n:::",
		},
		{
			name:     "a line of backticks with an info string inside code",
			markdown: "## D\n\n```\ncode\n```go\n## X\n```\n",
			heading:  "D",
			text:     "## D\n\n```\ncode\n```go\n## X\n```",
			alone:    true,
		},
		{
			name:     "a callout after a list item that opens with fenced code",
			markdown: "- ```\n  code\n  ```\n\n:::callout{#aside kind=\"note\" title=\"\"}\n- ```\n  code\n  ```\n\n## D\n\nText.\n\n# Notes\n:::",
			heading:  "D",
			text:     "## D\n\nText.",
		},
		{
			name:     "a decision after a list item that opens with fenced code",
			markdown: "- ```\n  code\n  ```\n\n## E\n\nText.\n",
			heading:  "E",
			text:     "## E\n\nText.",
			alone:    true,
		},
		{
			name:     "a decision after an ordered list item that opens with fenced code",
			markdown: "1. ```\n   code\n   ```\n\n## E\n\nText.\n",
			heading:  "E",
			text:     "## E\n\nText.",
			alone:    true,
		},
		{
			name:     "a callout holding a list item whose typed block closes indented",
			markdown: "::::callout{#aside kind=\"note\" title=\"\"}\n- :::callout{#inner kind=\"note\" title=\"\"}\n  Inner.\n  :::\n\n## D\n\nText.\n::::\n",
			heading:  "D",
			text:     "## D\n\nText.\n::::",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			cuts, read := decisionCuts(test.markdown)
			cut, ok := cuts[sectionKey{heading: test.heading}]
			if !read || len(cuts) != 1 || !ok || cut.text != test.text || cut.alone != test.alone {
				t.Fatalf("decisionCuts(%q) = %#v, %t, want the one decision %s, alone %t, written %q",
					test.markdown, cuts, read, test.heading, test.alone, test.text)
			}
		})
	}
}

// sharedHeading is the heading of the tests whose decisions.md writes one heading twice: once
// inside a quote or a list item, and once at the document's level.
const sharedHeading = "Ship the notices once"

// decisionsNamed is each decision of the view whose heading is heading, in document order.
func decisionsNamed(view issueDecisions, heading string) []model.Decision {
	named := []model.Decision{}
	for _, decision := range view.Decisions {
		if decision.Heading == heading {
			named = append(named, decision)
		}
	}
	return named
}

// afterAQuotedHeading is newDecisionsFixture with a quote that holds a sharedHeading heading
// written into decisions.md, and after it a decision of that heading at the document's level that
// links hold-it and ends in a loose list. It returns the version that wrote them and the
// decision at the document's level as the view then read it.
func afterAQuotedHeading(t *testing.T) (decisionsFixture, model.Version, model.Decision) {
	t.Helper()
	fixture := newDecisionsFixture(t)
	key := fixture.issue.Key
	section := "## " + sharedHeading + "\n\nShapes dispatch://" + key + "/spec#b-hold-it.\n\n- first\n\n- second\n\n- third"
	version := editIssueDocument(t, fixture.handler, key, fixture.decisions.Slug, []map[string]any{{
		"op": "insert", "markdown": "> ## " + sharedHeading + "\n>\n> Quoted.\n\n" + section, "before": "## Replaced",
	}})
	named := decisionsNamed(readIssueDecisions(t, fixture.handler, key), sharedHeading)
	if len(named) != 2 || named[1].Text != section || named[1].ChangedVersion != version.Number ||
		fmt.Sprint(linkTargets(named[1])) != "[hold-it]" {
		t.Fatalf("the decisions headed %q = %#v, want the quoted one and then one written %q at version %d linking hold-it",
			sharedHeading, named, section, version.Number)
	}
	return fixture, version, named[1]
}

// A heading inside a quote starts no cut (decisionCuts), so the cut names a decision at the
// document's level after a quoted decision of its heading as the first decision of that heading,
// in every version and in the document as it stands. alice keeps that decision, and a build of
// another renderer version stored the version her keep names, its loose list spelled otherwise:
// the decision stays kept, and the spec block it links is not out of date.
func TestADecisionAfterAQuotedHeadingOfItsNameStaysKeptAcrossARendererChange(t *testing.T) {
	fixture, version, written := afterAQuotedHeading(t)
	handler, database, key, id := fixture.handler, fixture.database, fixture.issue.Key, fixture.decisions.ID
	keepBlock(t, handler, id, written.BlockID, written.TextSHA256)
	another := pmdoc.RendererVersion - 1
	respellVersion(t, database, id, version.Number, &another)
	kept := strings.Replace(written.Text, "- first\n\n- second", "- first\n- second", 1)
	if _, err := database.Pool.Exec(context.Background(), `
		update decision_keeps set text_sha256 = $3 where artifact_id = $1 and block_id = $2
	`, id, written.BlockID, decisionTextSum(kept)); err != nil {
		t.Fatalf("keep the decision as another renderer version spelled it: %v", err)
	}

	decision := decisionsNamed(readIssueDecisions(t, handler, key), sharedHeading)[1]
	if decision.BlockID != written.BlockID || decision.Kept == nil || decision.Kept.ChangedVersion != version.Number || decision.WaitingReview {
		t.Errorf("the decision after the quoted heading = %#v, want %s kept at version %d and waiting for no review",
			decision, written.BlockID, version.Number)
	}
	if stale := staleTargets(decision); len(stale) != 0 {
		t.Errorf("the decision after the quoted heading marks %v out of date", stale)
	}
}

// A decision at the document's level after a quoted decision of its heading is read by its own
// cut for its links too: once its content changes, the spec block it links is out of date.
func TestADecisionAfterAQuotedHeadingOfItsNameMarksItsLinkOnceItsContentChanges(t *testing.T) {
	fixture, _, written := afterAQuotedHeading(t)
	handler, key := fixture.handler, fixture.issue.Key
	editIssueDocument(t, handler, key, fixture.decisions.Slug, []map[string]any{
		{"op": "replace", "find": "third", "with": "third, after measuring"},
	})
	changed := decisionsNamed(readIssueDecisions(t, handler, key), sharedHeading)[1]
	if changed.BlockID != written.BlockID || !strings.Contains(changed.Text, "third, after measuring") {
		t.Fatalf("the decision after the quoted heading = %#v, want %s holding the changed item", changed, written.BlockID)
	}
	if got := fmt.Sprint(staleTargets(changed)); got != "[hold-it]" {
		t.Errorf("after its content changed the decision after the quoted heading marks %v out of date, want hold-it", staleTargets(changed))
	}
}

// A decision at the document's level after a list item that opens with fenced code is cut, and
// read by its cut for its links: the item's code ends at its own closing line, indented two
// spaces, which read alone would open code that holds the decision's heading. A build of another
// renderer version stored the version that wrote the decision, its loose list spelled otherwise,
// and once the decision's content changes, the spec block it links is out of date.
func TestADecisionAfterAListItemOpeningWithCodeMarksItsLinkOnceItsContentChanges(t *testing.T) {
	fixture := newDecisionsFixture(t)
	handler, database, key, id := fixture.handler, fixture.database, fixture.issue.Key, fixture.decisions.ID
	section := "## " + listDecision + "\n\nShapes dispatch://" + key + "/spec#b-hold-it.\n\n- first\n\n- second\n\n- third"
	version := editIssueDocument(t, handler, key, fixture.decisions.Slug, []map[string]any{{
		"op": "insert", "markdown": "- ```\n  code\n  ```\n\n" + section, "before": "## Replaced",
	}})
	written := decisionNamed(t, readIssueDecisions(t, handler, key), listDecision)
	if written.ChangedVersion != version.Number || written.Text != section || fmt.Sprint(linkTargets(written)) != "[hold-it]" {
		t.Fatalf("the decision = %#v, want it changed at version %d, written %q and linking hold-it", written, version.Number, section)
	}
	another := pmdoc.RendererVersion - 1
	respellVersion(t, database, id, version.Number, &another)

	editIssueDocument(t, handler, key, fixture.decisions.Slug, []map[string]any{
		{"op": "replace", "find": "third", "with": "third, after measuring"},
	})
	changed := decisionNamed(t, readIssueDecisions(t, handler, key), listDecision)
	if changed.BlockID != written.BlockID || !strings.Contains(changed.Text, "third, after measuring") {
		t.Fatalf("the decision after the list item = %#v, want %s holding the changed item", changed, written.BlockID)
	}
	if got := fmt.Sprint(staleTargets(changed)); got != "[hold-it]" {
		t.Errorf("after its content changed the decision after the list item marks %v out of date, want hold-it", staleTargets(changed))
	}
}

// A stored version whose markdown this build's parser refuses is one the view could not read, as a
// version the document does not hold is, not one that holds no decision's text: a link that names
// it stays as it is. The agent decision links both spec blocks at version 1, and a later version
// changes another decision. A build of another renderer version stored version 1 with the
// decision's text unchanged and a link reference definition ahead of it, which pmdoc refuses
// (refusals.go): neither link is out of date.
func TestALinkWhoseVersionTheParserRefusesStaysAsItIs(t *testing.T) {
	fixture := newDecisionsFixture(t)
	handler, database, key, id := fixture.handler, fixture.database, fixture.issue.Key, fixture.decisions.ID
	table := decisionNamed(t, readIssueDecisions(t, handler, key), tableDecision)
	for _, link := range table.Links {
		if link.CreatedVersion != 1 {
			t.Fatalf("the decision's links = %#v, want each created at version 1", table.Links)
		}
	}
	editIssueDocument(t, handler, key, fixture.decisions.Slug, []map[string]any{
		{"op": "replace", "find": "Replaced by the table above.", "with": "Replaced by the table above, in May."},
	})

	stored := versionText(t, database, id, 1)
	refused := "[spec]: https://example.com/spec\n\n" + stored
	if !strings.Contains(refused, table.Text+"\n") {
		t.Fatalf("version 1 = %q; the test needs it to hold the decision as %q", stored, table.Text)
	}
	if _, err := pmdoc.ParseRendering(refused); err == nil {
		t.Fatalf("pmdoc reads %q; the test needs markdown it refuses", refused)
	}
	another := pmdoc.RendererVersion - 1
	storeVersionAs(t, database, id, 1, refused, &another)

	decision := decisionNamed(t, readIssueDecisions(t, handler, key), tableDecision)
	if decision.Text != table.Text || len(decision.Links) != 2 {
		t.Fatalf("the decision = %#v, want %q linking both spec blocks", decision, table.Text)
	}
	if stale := staleTargets(decision); len(stale) != 0 {
		t.Errorf("a link whose version this build refuses marks %v out of date, want it left as it is", stale)
	}
}

// A decisions view parses each markdown it reads once, and the next view parses none of them
// again: the process keeps each cut, the parser's refusal included (documentCuts). alice keeps
// listDecision at the version that wrote it and its link to hold-it, which a build of another
// renderer version stored in its own spelling (respellVersion), so the keeps' pass and the links'
// pass both name that version. She keeps tableDecision at version 1 under a sum its text does not
// have, and its links were created at version 1, which a build of another renderer version stored
// as markdown the parser refuses: both passes name that version too. The first view parses the
// document as it stands once, and no later view parses it again (readDecisions). Of the two views
// after the keeps, the first parses each of the two versions once, and the second parses neither.
// Both read the decisions alike.
func TestADecisionsViewParsesEachStoredVersionOnce(t *testing.T) {
	fixture := newDecisionsFixture(t)
	handler, database, key, id := fixture.handler, fixture.database, fixture.issue.Key, fixture.decisions.ID
	version := editIssueDocument(t, handler, key, fixture.decisions.Slug, []map[string]any{{
		"op":       "insert",
		"markdown": "## " + listDecision + "\n\nShapes dispatch://" + key + "/spec#b-hold-it.\n\n- first\n\n- second\n\n- third",
		"before":   "## Replaced",
	}})
	parsed := map[string]int{}
	decisionCutsObserver = func(markdown string) { parsed[markdown]++ }
	t.Cleanup(func() { decisionCutsObserver = nil })
	live := versionText(t, database, id, version.Number)
	written := readIssueDecisions(t, handler, key)
	if parsed[live] != 1 {
		t.Fatalf("the first view parsed the document as it stands %d times, want 1", parsed[live])
	}
	steps, table := decisionNamed(t, written, listDecision), decisionNamed(t, written, tableDecision)
	if steps.ChangedVersion != version.Number || len(steps.Links) != 1 || steps.Links[0].CreatedVersion != version.Number {
		t.Fatalf("the decision = %#v, want it changed at version %d and linking one spec block since then", steps, version.Number)
	}
	for _, link := range table.Links {
		if link.CreatedVersion != 1 {
			t.Fatalf("the decision's links = %#v, want each created at version 1", table.Links)
		}
	}
	if keep := keepBlock(t, handler, id, steps.BlockID, steps.TextSHA256); keep.ChangedVersion != version.Number {
		t.Fatalf("the keep of %q = %#v, want it at version %d", listDecision, keep, version.Number)
	}
	if keep := keepBlock(t, handler, id, table.BlockID, table.TextSHA256); keep.ChangedVersion != 1 {
		t.Fatalf("the keep of %q = %#v, want it at version 1", tableDecision, keep)
	}

	ctx := context.Background()
	another := pmdoc.RendererVersion - 1
	respelled := respellVersion(t, database, id, version.Number, &another)
	refused := "[spec]: https://example.com/spec\n\n" + versionText(t, database, id, 1)
	if _, read := decisionCuts(refused); read {
		t.Fatalf("the parser reads %q; the test needs markdown it refuses", refused)
	}
	storeVersionAs(t, database, id, 1, refused, &another)
	kept := strings.Replace(steps.Text, "- first\n\n- second", "- first\n- second", 1)
	for blockID, sum := range map[string]string{steps.BlockID: decisionTextSum(kept), table.BlockID: decisionTextSum(table.Text + " Unread.")} {
		if _, err := database.Pool.Exec(ctx, `
			update decision_keeps set text_sha256 = $3 where artifact_id = $1 and block_id = $2
		`, id, blockID, sum); err != nil {
			t.Fatalf("keep %s under the sum %s: %v", blockID, sum, err)
		}
	}

	for _, view := range []struct {
		name string
		want int
	}{{"first", 1}, {"second", 0}} {
		clear(parsed)
		read := readIssueDecisions(t, handler, key)
		steps, table := decisionNamed(t, read, listDecision), decisionNamed(t, read, tableDecision)
		if steps.WaitingReview || len(staleTargets(steps)) != 0 {
			t.Errorf("the %s view reads %q as %#v, want it kept, waiting for no review, and its link up to date",
				view.name, listDecision, steps)
		}
		if !table.WaitingReview || len(table.Links) != 2 || len(staleTargets(table)) != 0 {
			t.Errorf("the %s view reads %q as %#v, want it waiting for review and both its links as they are",
				view.name, tableDecision, table)
		}
		other := 0
		for markdown, times := range parsed {
			if markdown != respelled && markdown != refused {
				other += times
			}
		}
		if parsed[respelled] != view.want || parsed[refused] != view.want || other != 0 {
			t.Errorf("the %s view parsed version %d %d times, version 1 (refused) %d times and any other markdown, the document as it stands among them, %d times, want %d, %d and 0",
				view.name, version.Number, parsed[respelled], parsed[refused], other, view.want, view.want)
		}
	}
}

// A decision whose heading stands in a list item has no cut. alice keeps such a decision, its
// heading alone, and the version her keep names, which a build of another renderer version
// stored, holds the decision at the document's level in another spelling of that heading, before
// another decision of its heading. This build reads that kept text alone as the decision's text
// now (pmdoc.FragmentReadsAs): a heading line carries no prefix of the list item, as every later
// line of a decision inside one does. The decision waits for review, since it moved into the list
// item, and the first cut of its heading now is the other decision's, which says nothing of it.
func TestADecisionKeptBeforeItMovedIntoAListItemWaitsBesideADecisionOfItsName(t *testing.T) {
	fixture := newDecisionsFixture(t)
	handler, database, key, id := fixture.handler, fixture.database, fixture.issue.Key, fixture.decisions.ID
	heading, other := "## "+sharedHeading, "## "+sharedHeading+"\n\nOther."
	now := "- Intro.\n\n  " + heading + "\n\n" + other
	version := editIssueDocument(t, handler, key, fixture.decisions.Slug, []map[string]any{{
		"op": "insert", "markdown": now, "before": "## Replaced",
	}})
	named := decisionsNamed(readIssueDecisions(t, handler, key), sharedHeading)
	if len(named) != 2 || named[0].Text != heading || named[0].ChangedVersion != version.Number || named[1].Text != other {
		t.Fatalf("the decisions headed %q = %#v, want %q in a list item, changed at version %d, and then %q",
			sharedHeading, named, heading, version.Number, other)
	}
	written := named[0]
	keepBlock(t, handler, id, written.BlockID, written.TextSHA256)

	ctx := context.Background()
	stored := versionText(t, database, id, version.Number)
	if !strings.Contains(stored, now+"\n") {
		t.Fatalf("version %d = %q; the test needs it to hold %q", version.Number, stored, now)
	}
	// The kept heading reads alone as the heading now, so the decision waits only because it
	// stands in a list item.
	kept := "##  " + sharedHeading
	if !pmdoc.FragmentReadsAs(kept, heading) {
		t.Fatalf("%q does not read alone as %q; the test needs it to", kept, heading)
	}
	then := "- Intro.\n\n" + kept + "\n\n" + other
	another := pmdoc.RendererVersion - 1
	storeVersionAs(t, database, id, version.Number, strings.Replace(stored, now+"\n", then+"\n", 1), &another)
	if _, err := database.Pool.Exec(ctx, `
		update decision_keeps set text_sha256 = $3 where artifact_id = $1 and block_id = $2
	`, id, written.BlockID, decisionTextSum(kept)); err != nil {
		t.Fatalf("keep the decision as that version held it: %v", err)
	}

	decision := decisionsNamed(readIssueDecisions(t, handler, key), sharedHeading)[0]
	if decision.BlockID != written.BlockID || decision.Kept == nil || decision.Kept.ChangedVersion != version.Number || !decision.WaitingReview {
		t.Errorf("the decision in the list item = %#v, want %s kept at version %d and waiting for review",
			decision, written.BlockID, version.Number)
	}
}

// Each person's last read version of a document: null before they read it, the version they
// recorded after, and a bearer may read neither route.
func TestDocumentReadRecordsEachPersonsLastReadVersion(t *testing.T) {
	fixture := newDecisionsFixture(t)
	handler := fixture.handler
	path := "/api/v1/me/documents/" + fixture.decisions.ID + "/read"

	first := dispatchRequest(t, handler, http.MethodGet, path, nil, "alice")
	if first.Code != http.StatusOK {
		t.Fatalf("read the read mark: status=%d body=%s", first.Code, first.Body.String())
	}
	if read := decodeBody[model.DocumentRead](t, first); read.Version != nil || read.ReadAt != nil {
		t.Fatalf("before any read the mark = %#v, want a null version", read)
	}

	stored := dispatchRequest(t, handler, http.MethodPut, path, map[string]any{"version": 1}, "alice")
	if stored.Code != http.StatusOK {
		t.Fatalf("record the read mark: status=%d body=%s", stored.Code, stored.Body.String())
	}
	if read := decodeBody[model.DocumentRead](t, stored); read.Version == nil || *read.Version != 1 {
		t.Fatalf("the recorded mark = %#v, want version 1", read)
	}
	again := decodeBody[model.DocumentRead](t, dispatchRequest(t, handler, http.MethodGet, path, nil, "alice"))
	if again.Version == nil || *again.Version != 1 || again.ReadAt == nil {
		t.Fatalf("the stored mark = %#v, want version 1 with its time", again)
	}
	if bob := decodeBody[model.DocumentRead](t, dispatchRequest(t, handler, http.MethodGet, path, nil, "bob")); bob.Version != nil {
		t.Fatalf("another person's mark = %#v, want null: the mark is one person's", bob)
	}

	for _, probe := range []struct {
		method string
		body   any
	}{
		{http.MethodGet, nil},
		{http.MethodPut, map[string]any{"version": 2, "actor": sessionActor()}},
	} {
		response := sessionRequest(t, handler, probe.method, path, probe.body)
		if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), codeScopeRequired) {
			t.Fatalf("a bearer's %s: status=%d body=%s, want 403 %s", probe.method, response.Code, response.Body.String(), codeScopeRequired)
		}
	}
}

// A decisions.md whose links the block index wrote at its first indexing marks nothing out of
// date, in both cases of a first indexing: the document's first version, written while it has no
// block rows, and the backfill over a document no block-aware build indexed.
func TestAFirstIndexingMarksNothingOutOfDate(t *testing.T) {
	fixture := newDecisionsFixture(t)
	handler, database, key := fixture.handler, fixture.database, fixture.issue.Key

	// The document's first version, written while it had no block rows: the fixture's own upload.
	for _, decision := range readIssueDecisions(t, handler, key).Decisions {
		if stale := staleTargets(decision); len(stale) != 0 {
			t.Fatalf("the first version's %q marks %v out of date", decision.Heading, stale)
		}
	}

	// The backfill: the state migration 0121 left a document no block-aware build indexed, then
	// one reconciliation tick, as the block index's own tests run it.
	ctx := context.Background()
	for _, statement := range []string{
		`delete from block_refs where artifact_id = $1`,
		`delete from document_blocks where artifact_id = $1`,
		`delete from document_block_texts where artifact_id = $1`,
		`delete from document_indexed_versions where artifact_id = $1`,
		`update document_block_versions set version = null where artifact_id = $1`,
	} {
		if _, err := database.Pool.Exec(ctx, statement, fixture.decisions.ID); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if _, err := fixture.deps.Docs.(*docs.Service).ReconcileBlockIndex(ctx, docs.BlockIndexCursors{}, nil); err != nil {
		t.Fatalf("reconcile the block index: %v", err)
	}
	view := readIssueDecisions(t, handler, key)
	linked := 0
	for _, decision := range view.Decisions {
		linked += len(decision.Links)
		if stale := staleTargets(decision); len(stale) != 0 {
			t.Fatalf("after the backfill %q marks %v out of date", decision.Heading, stale)
		}
	}
	if linked == 0 {
		t.Fatalf("the backfill left no link to judge: %s", asJSON(t, view))
	}
}

// The open questions are the issue's open asks whose block is in its spec, oldest first, and an
// issue with no decisions.md answers them with a null document and no decisions. The two asks are
// written in two versions, so one is older than the other: two that one settlement writes share
// their created_at and the order between them is their ids'.
func TestIssueDecisionsListTheSpecsOpenQuestions(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{settle: 20 * time.Millisecond})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", ":::ask{#decide_first}\nWhich store?\n:::\n\nA paragraph.\n")
	view, _ := waitForOpenQuestions(t, handler, issue.Key, 1)
	if view.Document != nil || len(view.Decisions) != 0 {
		t.Fatalf("an issue with no decisions.md = %#v, want a null document and no decisions", view)
	}

	editSpec(t, handler, issue.Key, []map[string]any{
		{"op": "insert", "after": "end", "markdown": ":::ask{#decide_second}\nWhich name?\n:::"},
	})
	_, questions := waitForOpenQuestions(t, handler, issue.Key, 2)
	if fmt.Sprint(questions) != "[Which store? Which name?]" {
		t.Fatalf("open_questions = %v, want the spec's two asks oldest first", questions)
	}
	// Each question is the ask row the ask list answers, and that row declares a question tree,
	// so this view answers one too: a caller that asked for the trees is given the same row by
	// both routes, and a client that reads either draws what the server read rather than its own
	// reading of the markdown.
	requireDisplayTree(t, handler, "/api/v1/issues/"+issue.Key+"/decisions",
		"open_questions.0.question", "open_questions.0.question_tree")
}

// waitForOpenQuestions reads the view until it holds want open questions: settlement indexes a
// spec's ask blocks, and until it has the issue holds none.
func waitForOpenQuestions(t *testing.T, handler http.Handler, key string, want int) (issueDecisions, []string) {
	t.Helper()
	var view issueDecisions
	questions := []string{}
	for range 200 {
		view = readIssueDecisions(t, handler, key)
		questions = questions[:0]
		for _, question := range view.OpenQuestions {
			questions = append(questions, question.Question)
		}
		if len(questions) == want {
			return view, questions
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("open_questions = %v, want %d of them", questions, want)
	return view, questions
}
