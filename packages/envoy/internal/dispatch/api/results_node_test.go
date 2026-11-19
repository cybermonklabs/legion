package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// The names the store holds a result under (graphstore's check_result.go) - its node's kind and
// supertag, its tuples' kinds and its retraction's value - and the census's names of the two
// projections (graphcensus' census_results.go).
const (
	resultKind            = graphstore.KindResult
	resultOfKind          = graphstore.TupleResultOf
	resultRunSessionKind  = graphstore.TupleRunSession
	resultScreenshotKind  = graphstore.TupleScreenshot
	resultRetractedValue  = graphstore.AttributeRetracted
	resultCheckProjection = "check_result"
	resultRetractionKind  = "check_result_retraction"
)

// resultValueNames are the twelve values a result node carries, as its value tuples name them.
var resultValueNames = []string{graphstore.AttributeVerdict, graphstore.AttributeSurface, graphstore.AttributeRun,
	graphstore.AttributeObserved, graphstore.AttributeCommit, graphstore.AttributeRole, graphstore.AttributeFailed,
	graphstore.AttributeSkipped, graphstore.AttributeExcluded, graphstore.AttributeNegativeControl,
	graphstore.AttributeCheckVersion, graphstore.AttributeCheckSHA256}

// resultNodeSpec is a spec of three requirements, and resultNodeChecks a Verification document of
// key whose one check serves all three, with words; the test writes it twice, so the check's words
// last changed in version 2 of that document.
const resultNodeSpec = ":::requirement{#r1}\nThe first requirement.\n:::\n\n" +
	":::requirement{#r2}\nThe second requirement.\n:::\n\n" +
	":::requirement{#r3}\nThe third requirement.\n:::\n"

func resultNodeChecks(key, words string) string {
	return fmt.Sprintf(":::check{#c1 serves=\"dispatch://%[1]s/spec#b-r1 dispatch://%[1]s/spec#b-r2 dispatch://%[1]s/spec#b-r3\" "+
		"surface=\"cli\" negative_control=\"Break the check and watch it fail.\"}\n%[2]s\n:::\n", key, words)
}

// resultNodeRow is one check_results row, every column of it.
type resultNodeRow struct {
	model.CheckResult
	Actor json.RawMessage
}

// readResultNodeRow reads every column of the check_results row of id.
func readResultNodeRow(t *testing.T, database *store.Store, id string) resultNodeRow {
	t.Helper()
	var row resultNodeRow
	if err := database.Pool.QueryRow(context.Background(), `
		select id::text, issue_key, check_issue_key, check_artifact_id::text, check_block_id, check_version, check_sha256,
		       role, surface, run, observed, commit_sha, negative_control, failed, skipped, excluded, verdict,
		       screenshots, session, actor, created_at, event_seq
		from check_results where id = $1::uuid`, id).Scan(
		&row.ID, &row.IssueKey, &row.CheckIssueKey, &row.CheckArtifactID, &row.CheckBlockID, &row.CheckVersion,
		&row.CheckSHA256, &row.Role, &row.Surface, &row.Run, &row.Observed, &row.CommitSHA, &row.NegativeControl,
		&row.Failed, &row.Skipped, &row.Excluded, &row.Verdict, &row.Screenshots, &row.Session, &row.Actor,
		&row.CreatedAt, &row.EventSeq); err != nil {
		t.Fatalf("read the check_results row of %s: %v", id, err)
	}
	return row
}

// resultNode is what the store holds of one result: its node row, its current values and its
// current relations, each written kind>to_kind:to_id.
type resultNode struct {
	held                 bool
	supertag, state      string
	visibility, project  string
	madeAt               time.Time
	madeBy               json.RawMessage
	sourceEvent          int64
	values               map[string]string
	links                []string
	retracted            string
	retractedAt          time.Time
	retractedBy          json.RawMessage
	retractedSourceEvent int64
}

// readResultNode reads the result node of id as the store holds it now.
func readResultNode(t *testing.T, database *store.Store, id string) resultNode {
	t.Helper()
	ctx := context.Background()
	node := resultNode{values: map[string]string{}}
	var project *string
	err := database.Pool.QueryRow(ctx, `
		select supertag, state, visibility, project_key, made_at, made_by, source_event
		from graph_nodes where kind = $1 and id = $2`, resultKind, id).Scan(
		&node.supertag, &node.state, &node.visibility, &project, &node.madeAt, &node.madeBy, &node.sourceEvent)
	if err != nil {
		return node
	}
	node.held = true
	if project != nil {
		node.project = *project
	}
	for _, name := range resultValueNames {
		value, held, err := graphstore.CurrentValue(ctx, database.Pool, resultKind, id, name)
		if err != nil {
			t.Fatalf("read the %s of result %s: %v", name, id, err)
		}
		if held {
			node.values[name] = value
		}
	}
	relations, err := graphstore.CurrentOut(ctx, database.Pool, resultKind, id, nil)
	if err != nil {
		t.Fatalf("read the relations out of result %s: %v", id, err)
	}
	for _, relation := range relations {
		node.links = append(node.links, relation.Kind+">"+relation.ToKind+":"+relation.ToID)
	}
	slices.Sort(node.links)
	_ = database.Pool.QueryRow(ctx, `
		select to_id, at, who, source_event from graph_tuples
		where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = 'value' and asserted
		order by seq desc limit 1`, resultKind, id, resultRetractedValue).Scan(
		&node.retracted, &node.retractedAt, &node.retractedBy, &node.retractedSourceEvent)
	return node
}

// screenshotAddress is the address of the image a stored screenshot reference names, read as the
// record route read it (text.ParseRef): the document's ref_key.
func screenshotAddress(t *testing.T, reference string) string {
	t.Helper()
	ref, ok := text.ParseRef(reference, "")
	if !ok || ref.Kind != "artifact" || ref.IssueKey == "" {
		t.Fatalf("the stored screenshot %q names no document", reference)
	}
	return ref.IssueKey + "/" + ref.ID
}

// refKeyOf is the ref_key of one artifact.
func refKeyOf(t *testing.T, database *store.Store, artifactID string) string {
	t.Helper()
	var refKey string
	if err := database.Pool.QueryRow(context.Background(), `select ref_key from artifacts where id = $1::uuid`, artifactID).Scan(&refKey); err != nil {
		t.Fatalf("read the ref_key of %s: %v", artifactID, err)
	}
	return refKey
}

// requireTheRowIsTheNode holds every column of a check_results row to the result node it
// projects and that node's tuples: the values the node carries, its made_at and made_by, the check
// its result_of and its part_of name at the address the row's key and block give it, its
// authored_by, the session of its run, one screenshot per image the row names, its project, and
// the event that caused it, which is the row's event and names the row's two issues.
func requireTheRowIsTheNode(t *testing.T, database *store.Store, row resultNodeRow, node resultNode, project string) {
	t.Helper()
	ctx := context.Background()
	if !node.held {
		t.Fatalf("the store holds no node of result %s, whose check_results row stands", row.ID)
	}
	if node.supertag != resultKind || node.state != graphstore.StateLive || node.visibility != graphstore.VisibilityProject || node.project != project {
		t.Errorf("result %s is a node of supertag %q, state %q, visibility %q, project %q, want %s, live, project-visible, in %s",
			row.ID, node.supertag, node.state, node.visibility, node.project, resultKind, project)
	}
	want := map[string]string{
		"verdict": row.Verdict, "surface": row.Surface, "run": row.Run, "observed": row.Observed, "commit": row.CommitSHA,
		"role": row.Role, "failed": fmt.Sprint(row.Failed), "skipped": fmt.Sprint(row.Skipped), "excluded": fmt.Sprint(row.Excluded),
		"negative_control": row.NegativeControl, "check_version": fmt.Sprint(row.CheckVersion), "check_sha256": row.CheckSHA256,
	}
	for _, name := range resultValueNames {
		if got, held := node.values[name]; !held || got != want[name] {
			t.Errorf("result %s carries %s %q (held %t), want the row's %q", row.ID, name, got, held, want[name])
		}
	}
	if len(node.values) != len(resultValueNames) {
		t.Errorf("result %s carries %d values, want the %d a result holds: %v", row.ID, len(node.values), len(resultValueNames), node.values)
	}
	if !node.madeAt.Equal(row.CreatedAt) || !sameJSON(node.madeBy, row.Actor) {
		t.Errorf("result %s is made at %s by %s, want the row's %s by %s", row.ID, node.madeAt, node.madeBy, row.CreatedAt, row.Actor)
	}
	check := "block:" + refKeyOf(t, database, row.CheckArtifactID) + "#" + row.CheckBlockID
	var actor model.Actor
	if err := json.Unmarshal(row.Actor, &actor); err != nil {
		t.Fatalf("decode the row's actor: %v", err)
	}
	authorKind, authorID, named := graphstore.AuthorOf(actor)
	links := []string{"part_of>" + check, resultOfKind + ">" + check}
	if named {
		links = append(links, "authored_by>"+authorKind+":"+authorID)
	}
	if row.Session != "" {
		links = append(links, resultRunSessionKind+">session:"+row.Session)
	}
	for _, screenshot := range row.Screenshots {
		link := resultScreenshotKind + ">artifact:" + screenshotAddress(t, screenshot)
		if !slices.Contains(links, link) {
			links = append(links, link)
		}
	}
	slices.Sort(links)
	if !slices.Equal(node.links, links) {
		t.Errorf("result %s states the relations %v, want exactly %v: the check, the recorder, the session and each screenshot, one tuple each",
			row.ID, node.links, links)
	}
	if row.EventSeq != nil {
		var seq int64
		var issue, payloadIssue string
		if err := database.Pool.QueryRow(ctx, `select seq, issue_key, payload->>'issue' from events where id = $1`, node.sourceEvent).Scan(&seq, &issue, &payloadIssue); err != nil {
			t.Fatalf("read the event that caused result %s (%d): %v", row.ID, node.sourceEvent, err)
		}
		if seq != *row.EventSeq || issue != row.CheckIssueKey || payloadIssue != row.IssueKey {
			t.Errorf("result %s was caused by event %d, seq %d on %s recorded on %s, want the row's seq %d on %s recorded on %s",
				row.ID, node.sourceEvent, seq, issue, payloadIssue, *row.EventSeq, row.CheckIssueKey, row.IssueKey)
		}
	}
}

// sameJSON is whether two JSON documents hold the same value.
func sameJSON(a, b json.RawMessage) bool {
	var left, right any
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		return false
	}
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return string(leftJSON) == string(rightJSON)
}

// censusKindOf is the store census's line for one kind, failing where the census reads none.
func censusKindOf(t *testing.T, report *graphcensus.StoreReport, name string) graphcensus.KindReport {
	t.Helper()
	for _, kind := range report.Kinds {
		if kind.Kind == name {
			return kind
		}
	}
	t.Fatalf("the store census reads no kind %s: the check_results rows are no projection it holds the store to", name)
	return graphcensus.KindReport{}
}

// resultCensus takes the store census, as `envoy-dispatch census --store` takes it.
func resultCensus(t *testing.T, database *store.Store) *graphcensus.StoreReport {
	t.Helper()
	report, err := graphcensus.StoreCensus(context.Background(), database.Pool.Config().ConnString(), false)
	if err != nil {
		t.Fatalf("take the store census: %v", err)
	}
	return report
}

// requireTheResultCensusAgrees fails unless the census of the results and of their retractions
// reads rows rows and retractions retraction rows and no difference in either direction.
func requireTheResultCensusAgrees(t *testing.T, what string, database *store.Store, rows, retractions int64) {
	t.Helper()
	report := resultCensus(t, database)
	for name, want := range map[string]int64{resultCheckProjection: rows, resultRetractionKind: retractions} {
		kind := censusKindOf(t, report, name)
		if kind.Rows != want || kind.Nodes != want || kind.RowDifferences != 0 || kind.NodeDifferences != 0 {
			t.Errorf("%s: the census of %s reads %d rows, %d nodes, %d row and %d node differences (%v %v), want %d of each and none",
				what, name, kind.Rows, kind.Nodes, kind.RowDifferences, kind.NodeDifferences, kind.ReportedRows, kind.ReportedNodes, want)
		}
	}
}

// TestARecordedResultIsANodeWithItsTuples is c-result-node. A result recorded through
// recordCheckResult, by a session, with two screenshots, on an issue born converted whose one check
// serves three requirements, is a node of supertag result keyed by the result's id, carrying its
// twelve values, with one tuple each to its check (result_of and part_of, at the check's address
// in the issue's room, `<tree ref_key>#<block>`), its recorder, its run's session and each
// screenshot, and none to a requirement: the check holds one result node, and no requirement any.
// The check_results row written in the same transaction equals the node and its tuples column by
// column; its check_artifact_id is the tree artifact, the row's key, and its check_version a
// version of the Verification document. The results read answers it under its check, and a
// retraction appends the node's retracted value beside its check_result_retractions row.
//
// A transaction that rolls back leaves neither the node, nor a tuple, nor the row. A row an earlier
// release wrote, which names no node, is the negative control: the store census counts one
// projection row the store does not hold, naming the result's id, until the backfill writes its
// node, dated at the row's created_at, after which the census reads none. A stored screenshot
// reference with trailing punctuation is read as the route read it, so it is no census difference.
// Last, a ci_check given to the recorded check of the converted issue is refused INVALID_CI_CHECK:
// the results are found through the Verification document's room.
func TestARecordedResultIsANodeWithItsTuples(t *testing.T) {
	ctx := context.Background()
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "RNODE", "A result is a node", resultNodeSpec)
	tree, err := issueTreeOf(ctx, database.Pool, issue.Key)
	if err != nil || tree == "" {
		t.Fatalf("the tree of %s is %q (%v), want the issue born converted", issue.Key, tree, err)
	}
	verification := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md",
		resultNodeChecks(issue.Key, "A result is a node of the store."))
	reworded := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md",
		resultNodeChecks(issue.Key, "A recorded result is a node of the store, with its tuples."))
	if reworded.Artifact.ID != verification.Artifact.ID || reworded.Version.Number != 2 {
		t.Fatalf("the second upload of verification.md is %s version %d, want version 2 of %s",
			reworded.Artifact.ID, reworded.Version.Number, verification.Artifact.ID)
	}
	var room string
	if err := database.Pool.QueryRow(ctx, `select room::text from document_rooms where artifact_id = $1::uuid`, verification.Artifact.ID).Scan(&room); err != nil || room != tree {
		t.Fatalf("verification.md is edited in room %q (%v), want the issue's tree %s", room, err, tree)
	}
	first := uploadImage(t, handler, issue.Key, "first-shot.png", "the first screenshot")
	second := uploadImage(t, handler, issue.Key, "second-shot.png", "the second screenshot")
	treeRefKey := refKeyOf(t, database, tree)
	checkAddress := treeRefKey + "#c1"

	recorded := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/results", resultBody("c1", "fail", headCommit, map[string]any{
		"actor": sessionActor(), "role": "tester", "surface": "cli", "run": "go test ./results -run TestNode",
		"observed": "FAIL: 2 failed, 1 skipped, 3 excluded", "failed": 2, "skipped": 1, "excluded": 3,
		"negative_control": "Skipped the node and the census named the row.",
		// The second reference trails a full stop, which the route reads past (text.ParseRef).
		"screenshots": []string{"dispatch://" + issue.Key + "/artifact/" + first.Artifact.Slug,
			"dispatch://" + issue.Key + "/artifact/" + second.Artifact.Slug + "."},
	}))
	if recorded.Code != http.StatusCreated {
		t.Fatalf("record the result: status=%d body=%s", recorded.Code, recorded.Body.String())
	}
	result := decodeBody[recordedResult](t, recorded).Result

	// The row names the tree artifact, its key, and the check's block; its check_version is the
	// Verification document's version that last changed the check's words.
	row := readResultNodeRow(t, database, result.ID)
	if row.CheckArtifactID != tree || row.CheckBlockID != "c1" || row.CheckVersion != 2 {
		t.Errorf("the row names check %s#%s at version %d, want the tree %s, block c1 and version 2 of verification.md",
			row.CheckArtifactID, row.CheckBlockID, row.CheckVersion, tree)
	}
	var indexed int
	var indexedSum string
	if err := database.Pool.QueryRow(ctx, `
		select d.changed_version, d.text_sha256 from document_blocks d
		where d.artifact_id = $1::uuid and d.block_id = 'c1'`, tree).Scan(&indexed, &indexedSum); err != nil {
		t.Fatalf("read the check's row: %v", err)
	}
	if row.CheckVersion != indexed || row.CheckSHA256 != indexedSum {
		t.Errorf("the row holds check_version %d and check_sha256 %s, want the check's row's %d and %s",
			row.CheckVersion, row.CheckSHA256, indexed, indexedSum)
	}
	if result.CheckArtifactID != row.CheckArtifactID || result.Session != "session-0123456789abcdef" || len(row.Screenshots) != 2 {
		t.Errorf("the record answers check_artifact_id %q and session %q, and the row holds %d screenshots, want %s, the recording session and two",
			result.CheckArtifactID, result.Session, len(row.Screenshots), row.CheckArtifactID)
	}

	// A result is a node, with its values and one tuple each to its check, its recorder, its run's
	// session and each screenshot.
	node := readResultNode(t, database, result.ID)
	requireTheRowIsTheNode(t, database, row, node, "RNODE")
	for _, link := range []string{
		"part_of>block:" + checkAddress, resultOfKind + ">block:" + checkAddress, "authored_by>session:session-0123456789abcdef",
		resultRunSessionKind + ">session:session-0123456789abcdef",
		resultScreenshotKind + ">artifact:" + first.Artifact.RefKey, resultScreenshotKind + ">artifact:" + second.Artifact.RefKey,
	} {
		if !slices.Contains(node.links, link) {
			t.Errorf("result %s states no %s: it states %v", result.ID, link, node.links)
		}
	}

	// The result points at the check and at no requirement, though its check serves three: the
	// check holds one result node, and no requirement holds any.
	for _, requirement := range []string{"r1", "r2", "r3"} {
		var into int
		if err := database.Pool.QueryRow(ctx, `
			select count(*) from graph_tuples where from_kind = $1 and to_kind = 'block' and to_id like '%#' || $2`,
			resultKind, requirement).Scan(&into); err != nil {
			t.Fatalf("read the tuples into %s: %v", requirement, err)
		}
		if into != 0 {
			t.Errorf("a result states %d tuples into requirement %s, want none: a result points at its check alone", into, requirement)
		}
	}
	var under int
	if err := database.Pool.QueryRow(ctx, `
		select count(distinct from_id) from graph_tuples where from_kind = $1 and kind = $2 and to_kind = 'block' and to_id = $3 and asserted`,
		resultKind, resultOfKind, checkAddress).Scan(&under); err != nil {
		t.Fatalf("read the results of c1: %v", err)
	}
	if under != 1 {
		t.Errorf("check c1 holds %d result nodes, want one", under)
	}
	read := readResults(t, handler, issue.Key)
	if results := checkOf(t, read, "c1").Results; len(results) != 1 || results[0].ID != result.ID {
		t.Errorf("the results read answers %+v under c1, want the one result %s", results, result.ID)
	}

	// A retraction appends the result's retracted value, with the reason, in the transaction that
	// writes its check_result_retractions row, caused by its event; the node is unchanged otherwise.
	retracted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/results/"+result.ID+"/retract",
		map[string]any{"reason": "The run measured the wrong build."}, "alice")
	if retracted.Code != http.StatusCreated {
		t.Fatalf("retract the result: status=%d body=%s", retracted.Code, retracted.Body.String())
	}
	retraction := decodeBody[model.CheckResultRetraction](t, retracted)
	after := readResultNode(t, database, result.ID)
	var retractionEvent int64
	if err := database.Pool.QueryRow(ctx, `select id from events where issue_key = $1 and seq = $2`, issue.Key, *retraction.EventSeq).Scan(&retractionEvent); err != nil {
		t.Fatalf("read the retraction's event: %v", err)
	}
	if after.retracted != retraction.Reason || !after.retractedAt.Equal(retraction.CreatedAt) || after.retractedSourceEvent != retractionEvent {
		t.Errorf("result %s carries retracted %q at %s caused by %d, want %q at %s caused by the retraction's event %d",
			result.ID, after.retracted, after.retractedAt, after.retractedSourceEvent, retraction.Reason, retraction.CreatedAt, retractionEvent)
	}
	requireTheRowIsTheNode(t, database, row, after, "RNODE")
	if got := checkOf(t, readResults(t, handler, issue.Key), "c1").Results; len(got) != 1 || got[0].Retracted == nil || got[0].Retracted.Reason != retraction.Reason {
		t.Errorf("the results read answers %+v under c1, want the result retracted with its reason", got)
	}

	// A transaction that rolls back leaves neither the node, nor a tuple, nor the row: a deferred
	// trigger refuses the row at the record's commit, after the route wrote all three.
	if _, err := database.Pool.Exec(ctx, `
		create function refuse_rolled_back_result() returns trigger language plpgsql as $$
		begin raise exception 'this test rolls the result back'; end $$;
		create constraint trigger refuse_rolled_back_result after insert on check_results
		deferrable initially deferred for each row when (new.observed = 'rolled back')
		execute function refuse_rolled_back_result();`); err != nil {
		t.Fatalf("install the trigger that rolls a result back: %v", err)
	}
	var before int64
	if err := database.Pool.QueryRow(ctx, `select count(*) from graph_tuples`).Scan(&before); err != nil {
		t.Fatalf("count the tuples: %v", err)
	}
	rolled := recordResult(t, handler, issue.Key, resultBody("c1", "fail", headCommit, map[string]any{"observed": "rolled back", "failed": 1}))
	if rolled.Code != http.StatusInternalServerError {
		t.Fatalf("a record whose commit the trigger refuses answers %d %s, want 500", rolled.Code, rolled.Body.String())
	}
	var rows, nodes, tuples, orphaned int64
	if err := database.Pool.QueryRow(ctx, `
		select (select count(*) from check_results where observed = 'rolled back'),
		       (select count(*) from graph_nodes where kind = $1 and attributes->>'observed' = 'rolled back'),
		       (select count(*) from graph_tuples) - $2,
		       (select count(*) from graph_tuples t where t.from_kind = $1
		          and not exists (select 1 from graph_nodes n where n.kind = t.from_kind and n.id = t.from_id))`,
		resultKind, before).Scan(&rows, &nodes, &tuples, &orphaned); err != nil {
		t.Fatalf("read what the rolled-back record left: %v", err)
	}
	if rows != 0 || nodes != 0 || tuples != 0 || orphaned != 0 {
		t.Errorf("the rolled-back record left %d rows, %d result nodes, %d tuples and %d tuples out of no result node, want none",
			rows, nodes, tuples, orphaned)
	}
	if _, err := database.Pool.Exec(ctx, `drop trigger refuse_rolled_back_result on check_results; drop function refuse_rolled_back_result();`); err != nil {
		t.Fatalf("drop the trigger: %v", err)
	}

	// A row an earlier release wrote, on an issue created as before the issue room, keyed by its own
	// Verification document, and naming no node: the census counts one projection row the store
	// does not hold, naming the result's id. That is the negative control, a row the record route
	// wrote with its node skipped.
	holdIssuesUnconverted(t, database)
	earlier := createInteractionIssue(t, handler, "RNOLD", "A result an earlier release wrote", resultNodeSpec)
	earlierVerification := uploadDocument(t, handler, "/api/v1/issues/"+earlier.Key, "verification.md",
		resultNodeChecks(earlier.Key, "A result an earlier release wrote."))
	earlierShot := uploadImage(t, handler, earlier.Key, "earlier-shot.png", "an earlier screenshot")
	var old resultNodeRow
	if err := database.Pool.QueryRow(ctx, `
		insert into check_results (issue_key, check_issue_key, check_artifact_id, check_block_id, check_version, check_sha256,
			role, surface, run, observed, commit_sha, negative_control, verdict, screenshots, session, actor, created_at)
		select $1, $1, $2::uuid, 'c1', d.changed_version, d.text_sha256, 'implementer', 'cli', 'go test ./old', 'ok',
		       $3, 'Broke it and it failed.', 'pass', array[$4], '', '{"kind":"user","id":"alice"}', now() - interval '3 days'
		from document_blocks d where d.artifact_id = $2::uuid and d.block_id = 'c1'
		returning id::text`, earlier.Key, earlierVerification.Artifact.ID, headCommit,
		"dispatch://"+earlier.Key+"/artifact/"+earlierShot.Artifact.Slug+"),").Scan(&old.ID); err != nil {
		t.Fatalf("write the row an earlier release wrote: %v", err)
	}
	report := resultCensus(t, database)
	kind := censusKindOf(t, report, resultCheckProjection)
	wantAt := resultKind + " " + old.ID
	if kind.RowDifferences != 1 || len(kind.ReportedRows) != 1 || kind.ReportedRows[0].At != wantAt ||
		kind.ReportedRows[0].Cause != "the row names a node the store does not hold" || kind.NodeDifferences != 0 {
		t.Errorf("the census of %s reads %d row differences %v and %d node differences %v, want the one row %q naming a node the store does not hold",
			resultCheckProjection, kind.RowDifferences, kind.ReportedRows, kind.NodeDifferences, kind.ReportedNodes, wantAt)
	}

	// The backfill writes the old row's node from the row, dated at its created_at, and the census
	// then reads no difference over the two results and the one retraction.
	if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("run the backfill: %v", err)
	}
	old = readResultNodeRow(t, database, old.ID)
	oldNode := readResultNode(t, database, old.ID)
	requireTheRowIsTheNode(t, database, old, oldNode, "RNOLD")
	oldCheck := refKeyOf(t, database, earlierVerification.Artifact.ID) + "#c1"
	if !slices.Contains(oldNode.links, resultOfKind+">block:"+oldCheck) {
		t.Errorf("the old result states %v, want its result_of the check of its own document, %s", oldNode.links, oldCheck)
	}
	if oldNode.sourceEvent != graphstore.NoEvent {
		t.Errorf("the backfill's node of the old result is caused by %d, want the sentinel", oldNode.sourceEvent)
	}
	requireTheResultCensusAgrees(t, "after the backfill", database, 2, 1)

	// The ci_check a write gives the recorded check of the converted issue is refused: the results
	// keyed by the tree are found through verification.md's room.
	expectRefusal(t, "a retype giving the recorded check a ci_check", applyOp(t, handler, verification.Artifact.ID, map[string]any{
		"op": "retype", "block": "c1", "type": "check",
		"attributes": map[string]any{"negative_control": ciCheckNegativeControl, "ci_check": ciCheckRun},
	}), http.StatusBadRequest, "INVALID_CI_CHECK", ciCheckRefusal("c1", ciCheckRecordedRule))
	if strings.Contains(documentMarkdown(t, handler, verification.Artifact.ID), "ci_check") {
		t.Errorf("verification.md holds a ci_check after the refused retype")
	}
}

// resultLockCheck is the check of the lock-step rig's Verification document a result path records
// a result of, and resultLockVerification that document, which ready uploads to the rig's issue
// once.
const (
	resultLockCheck        = "lock-check"
	resultLockVerification = ":::check{#" + resultLockCheck + " surface=\"cli\"}\nThe writers take one lock order.\n:::\n"
)

// lockResultCheck uploads the rig issue's verification.md holding resultLockCheck, once: the issue
// is born converted, so the document joins the issue's room, and its check's row is keyed by the
// tree artifact.
func (r *storeWriterRig) lockResultCheck(t *testing.T, uploaded *bool) {
	t.Helper()
	if *uploaded {
		return
	}
	uploadDocument(t, r.handler, "/api/v1/issues/"+r.issue, "verification.md", resultLockVerification)
	*uploaded = true
}

// resultRecord is the record of a result (recordCheckResult, writeRecordedResult), through its
// route: the owner locks of the recording issue and the check's issue, here one
// (lockIssueOwners), then result.recorded, the check_results row, whose foreign key takes the
// check's artifact row `for key share`, then the result's node and the recorder's in one node
// write, and last the tuples that event causes. Each run records a result of its own under an id
// drawn in the run, so the row, the node and the tuples are held as the tables they are written
// to. The event is an issue's, so it takes no lock on projects (eventProjectLocks).
func (r *storeWriterRig) resultRecord() storeWriter {
	uploaded := false
	return storeWriter{
		name: "recordCheckResult",
		steps: []storeStep{
			ownerLockStep(store.IssueOwner(r.issue)),
			eventAppendStep(),
			{
				name: "the result's row", order: projectionStep,
				hold: holdTableWrites("check_results"), at: "insert into check_results (",
				takes: []tableLock{{"artifacts", rowShareMode}, {"check_results", rowExclusiveMode}},
			},
			{
				name: "the result's node", order: nodeStep,
				hold: holdTableWrites("graph_nodes"), at: nodeWriteStatement,
				takes: []tableLock{{"graph_nodes", rowExclusiveMode}},
			},
			tupleAppendStep(),
		},
		ready: func(t *testing.T, ctx context.Context) storeWriterRun {
			r.lockResultCheck(t, &uploaded)
			return storeWriterRun{
				made: graphstore.Node{Kind: resultKind},
				write: func() error {
					return statusOrError(dispatchRequest(t, r.call, http.MethodPost, "/api/v1/issues/"+r.issue+"/results",
						resultBody(resultLockCheck, "fail", headCommit, map[string]any{"failed": 1}), "alice"), http.StatusCreated)
				},
			}
		},
	}
}

// resultRetraction is the retraction of a result (retractCheckResult, writeCheckResultRetraction),
// through its route: it reads the result's check_results row plainly (readRetractedResult), takes
// the owner lock of the check's issue (store.LockOwner), reads whether the store holds the
// result's node, then appends result.retracted, and last writes, in one batch, the
// check_result_retractions row and the result's retracted value that event causes, a tuple alone
// (graphstore.QueueWrite). Each run retracts a result its ready recorded; the retraction's row and
// the value are held as the tables they are written to.
func (r *storeWriterRig) resultRetraction() storeWriter {
	uploaded := false
	return storeWriter{
		name: "retractCheckResult",
		steps: []storeStep{
			ownerLockStep(store.IssueOwner(r.issue)),
			eventAppendStep(),
			{
				name: "the retraction's row", order: projectionStep,
				hold: holdTableWrites("check_result_retractions"), at: "insert into check_result_retractions (",
				takes: []tableLock{{"check_result_retractions", rowExclusiveMode}},
			},
			{
				name: "the result's retracted value", order: tupleStep, caused: true,
				hold: holdTableWrites("graph_tuples"), at: tupleOnlyAppendStatement,
				takes: []tableLock{{"graph_tuples", rowExclusiveMode}},
			},
		},
		ready: func(t *testing.T, ctx context.Context) storeWriterRun {
			r.lockResultCheck(t, &uploaded)
			recorded := dispatchRequest(t, r.handler, http.MethodPost, "/api/v1/issues/"+r.issue+"/results",
				resultBody(resultLockCheck, "fail", headCommit, map[string]any{"failed": 1}), "alice")
			if recorded.Code != http.StatusCreated {
				t.Fatalf("record the result the run retracts: status=%d body=%s", recorded.Code, recorded.Body.String())
			}
			id := decodeBody[recordedResult](t, recorded).Result.ID
			return storeWriterRun{
				made: graphstore.Node{Kind: resultKind, ID: id},
				write: func() error {
					return statusOrError(dispatchRequest(t, r.call, http.MethodPost, "/api/v1/results/"+id+"/retract",
						map[string]any{"reason": "The run measured the wrong build."}, "alice"), http.StatusCreated)
				},
			}
		},
	}
}
