package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
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
	report, err := graphcensus.StoreCensus(context.Background(), database.Pool.Config().ConnString(), false, docs.ReturnDeclined)
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
// (lockIssueOwners), then the lock of the check's issue's room (docs.LockQueryRooms), then
// result.recorded, the check_results row, whose foreign key takes the check's artifact row `for key
// share`, then the result's node and the recorder's in one node write, and the tuples that event
// causes. Each run records a result of its own under an id drawn in the run, so the row, the node
// and the tuples are held as the tables they are written to. The event is an issue's, so it takes
// no lock on projects (eventProjectLocks). The rig's issue is born converted, so the result moves
// the record of its Verification query, and the route last appends a version of that document
// (reviseQueriesUnder): the version's row, its node and its version_renders row, and the node's
// tuples under result.recorded.
func (r *storeWriterRig) resultRecord() storeWriter {
	uploaded := false
	return storeWriter{
		name: "recordCheckResult",
		steps: []storeStep{
			ownerLockStep(store.IssueOwner(r.issue)),
			r.roomLockStep(),
			resultEventStep(),
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
			revisedVersionRowStep(),
			revisedRenderRowStep(),
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
// the owner lock of the check's issue (store.LockOwner) and then its room's lock
// (docs.LockQueryRooms), reads whether the store holds the result's node, then appends
// result.retracted, and writes, in one batch, the check_result_retractions row and the result's
// retracted value that event causes, a tuple alone (graphstore.QueueWrite). Each run retracts a
// result its ready recorded; the retraction's row and the value are held as the tables they are
// written to. The retraction moves the record of the issue's Verification query, so the route last
// appends a version of that document (reviseQueriesUnder): the version's row, its node, its
// version_renders row, and the node's tuples under result.retracted.
func (r *storeWriterRig) resultRetraction() storeWriter {
	uploaded := false
	return storeWriter{
		name: "retractCheckResult",
		steps: []storeStep{
			ownerLockStep(store.IssueOwner(r.issue)),
			r.roomLockStep(),
			resultEventStep(),
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
			revisedVersionRowStep(),
			{
				name: "the revised version's node", order: nodeStep,
				hold: holdTableWrites("graph_nodes"), at: nodeWriteStatement,
				takes: []tableLock{{"graph_nodes", rowExclusiveMode}},
			},
			revisedRenderRowStep(),
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

// resultEventStep is the event a result's write appends (eventAppendStep), with the issue's row its
// append updates (events.Broker moves the owner row's last_seq): a write held at an earlier step
// that has locked that row already took its event, which the room's lock comes before.
func resultEventStep() storeStep {
	step := eventAppendStep()
	step.takes = append(slices.Clone(step.takes), tableLock{"issues", rowExclusiveMode})
	return step
}

// roomLockStep is the lock of the rig's issue's room (docs.LockQueryRooms), which a route that goes
// on to append a version of one of the issue's query documents takes right after its owner lock
// and before its event: held here as a writer of the room's stored history holds it, the advisory
// lock of the issue's tree (lockDocumentRoom's key). It locks no table and no row.
func (r *storeWriterRig) roomLockStep() storeStep {
	return storeStep{
		name: "the issue room's lock", order: roomStep,
		hold: func(t *testing.T, ctx context.Context, holder pgx.Tx, _ storeWriterRun) {
			t.Helper()
			tag, err := holder.Exec(ctx, `select pg_advisory_xact_lock(hashtext(artifact_id::text)) from issue_trees where issue_key = $1`, r.issue)
			if err != nil {
				t.Fatalf("hold the lock of %s's room: %v", r.issue, err)
			}
			if tag.RowsAffected() != 1 {
				t.Fatalf("hold the lock of %s's room: it took %d room locks, want the one of the issue's tree", r.issue, tag.RowsAffected())
			}
		},
		at: "select pg_advisory_xact_lock(hashtext(t.artifact_id::text)) from issue_trees t where t.issue_key = $1",
	}
}

// revisedVersionRowStep is the row of the version a result's write appends of the Verification
// document whose record it moved (docs.Service.ReviseQueries), whose insert's trigger owes the
// document its block index (document_block_versions_owe); the version's number is drawn in the
// run, so the table is held.
func revisedVersionRowStep() storeStep {
	return storeStep{
		name: "the revised version's row", order: projectionStep,
		hold: holdTableWrites("artifact_versions"), at: versionStatement,
		takes: []tableLock{{"artifact_versions", rowExclusiveMode}, {"document_block_versions", rowExclusiveMode}},
	}
}

// revisedRenderRowStep is the version_renders row of that version, holding its record's digest
// (docs.QueueVersionRender), after the version's node.
func revisedRenderRowStep() storeStep {
	return storeStep{
		name: "the revised version's render row", order: projectionStep,
		hold: holdTableWrites("version_renders"), at: "insert into version_renders (",
		takes: []tableLock{{"version_renders", rowExclusiveMode}},
	}
}

// revisedSpec is the spec of the issues whose queries TestEveryWriteThatMovesAQuerysRecordAppendsAVersionOfIt
// revises: a paragraph and one requirement; revisedChecks is their Verification document, one
// check.
const (
	revisedSpec   = "The spec's opening words.\n\n:::requirement{#r1}\nThe list comes first.\n:::\n"
	revisedChecks = ":::check{#c1 surface=\"cli\"}\nThe list reads the urgent asks first.\n:::\n"
)

// queryVersion is the newest version of a document and its version_renders row: the version's
// number and stored markdown, and the row's digest, time and seq, each nil where it holds none.
type queryVersion struct {
	number   int
	markdown string
	digest   *string
	at       *time.Time
	readSeq  *int64
}

// newestQueryVersion reads the newest version of the document artifactID and its row.
func newestQueryVersion(t *testing.T, database *store.Store, artifactID string) queryVersion {
	t.Helper()
	var version queryVersion
	if err := database.Pool.QueryRow(context.Background(), `
		select v.number, coalesce(v.markdown, ''), r.record_sha256, r.at, r.read_seq
		from artifact_versions v
		left join version_renders r on r.artifact_id = v.artifact_id and r.number = v.number
		where v.artifact_id = $1
		order by v.number desc limit 1`, artifactID).Scan(&version.number, &version.markdown, &version.digest, &version.at, &version.readSeq); err != nil {
		t.Fatalf("read the newest version of %s: %v", artifactID, err)
	}
	return version
}

// requireRecordedDigest fails unless version, the newest version of a document of issue whose
// query is query, holds a version_renders row whose digest is the digest docs builds of that
// query's record at the row's own cut and at the head, and answers the digest.
func requireRecordedDigest(t *testing.T, database *store.Store, what, issue, query string, version queryVersion) string {
	t.Helper()
	if version.digest == nil || version.at == nil || version.readSeq == nil {
		t.Fatalf("%s: version %d of the %s holds no version_renders row", what, version.number, query)
	}
	ctx := context.Background()
	recorded, err := docs.ReadQueryRecord(ctx, database.Pool, issue, query, refs.TreeCut{At: *version.at, ReadSeq: *version.readSeq})
	if err != nil {
		t.Fatalf("%s: read the %s record of %s at the row's cut: %v", what, query, issue, err)
	}
	head, err := docs.ReadQueryRecord(ctx, database.Pool, issue, query, refs.TreeCut{})
	if err != nil {
		t.Fatalf("%s: read the %s record of %s at the head: %v", what, query, issue, err)
	}
	if *version.digest != recorded.Digest || *version.digest != head.Digest {
		t.Errorf("%s: version %d of the %s records the digest %s, want its record's, %s at the row's cut and %s at the head",
			what, version.number, query, *version.digest, recorded.Digest, head.Digest)
	}
	return *version.digest
}

// writeThroughTheRoom appends markdown to the document artifactID of issueKey through its room, as
// alice's browser writes it (connectBrowserPeer), and settles the issue's documents (settleIssue).
func writeThroughTheRoom(t *testing.T, handler http.Handler, documents *docs.Service, issueKey, artifactID, markdown string) {
	t.Helper()
	peer := connectBrowserPeer(t, handler, documents, artifactID)
	defer peer.close()
	peer.appendParagraph(t, markdown)
	peer.barrier(t)
	settleIssue(t, documents, issueKey)
}

// requireRevisedVersion fails unless version number of the document of refKey, which artifactID
// names, is a query version holding the definition_version of this build's record layout, whose
// node the event event caused, and renders from the tree as its stored bytes with no difference
// the census of the rendered versions reads at it.
func requireRevisedVersion(t *testing.T, database *store.Store, what, artifactID, refKey string, number int, event int64) {
	t.Helper()
	ctx := context.Background()
	if supertag, held := versionDefinition(t, database, refKey, number); supertag != graphstore.SupertagQueryVersion || definitionHeld(held) != fmt.Sprint(graphstore.QueryDefinitionVersion) {
		t.Errorf("%s: version %d of %s is a %s holding definition_version %s, want a query version holding %d",
			what, number, refKey, supertag, definitionHeld(held), graphstore.QueryDefinitionVersion)
	}
	var cause int64
	if err := database.Pool.QueryRow(ctx, `select source_event from graph_nodes where kind = $1 and id = $2`,
		graphstore.KindVersion, graphstore.VersionID(refKey, number)).Scan(&cause); err != nil {
		t.Fatalf("%s: read the node of version %d of %s: %v", what, number, refKey, err)
	}
	if cause != event {
		t.Errorf("%s: the node of version %d of %s is caused by the event %d, want the write's own %d", what, number, refKey, cause, event)
	}
	rendering, err := docs.RenderVersion(ctx, database.Pool, model.Actor{Kind: "user", ID: "alice"}, artifactID, number)
	if err != nil {
		t.Fatalf("%s: render version %d of %s: %v", what, number, refKey, err)
	}
	var stored string
	if err := database.Pool.QueryRow(ctx, `select markdown from artifact_versions where artifact_id = $1 and number = $2`, artifactID, number).Scan(&stored); err != nil {
		t.Fatalf("%s: read version %d of %s: %v", what, number, refKey, err)
	}
	if rendering.Source != docs.RenderedFromTree || rendering.Markdown != stored {
		t.Errorf("%s: version %d of %s renders %q from %s, want its stored %q from the tree", what, number, refKey, rendering.Markdown, rendering.Source, stored)
	}
	census, err := docs.ReadVersionRenderCensus(ctx, database.Pool)
	if err != nil {
		t.Fatalf("%s: read the census of the rendered versions: %v", what, err)
	}
	for _, difference := range census.Reported {
		if difference.At == "version "+graphstore.VersionID(refKey, number) {
			t.Errorf("%s: the census of the rendered versions reads a difference at %s: %s", what, difference.At, difference.Cause)
		}
	}
}

// issueVersionCounts is the newest version number of each document of issueKey, by slug, read
// through q.
func issueVersionCounts(t *testing.T, q queryer, issueKey string) map[string]int {
	t.Helper()
	rows, err := q.Query(context.Background(), `
		select a.slug, coalesce(max(v.number), 0) from artifacts a
		left join artifact_versions v on v.artifact_id = a.id
		where a.issue_key = $1 group by a.slug`, issueKey)
	if err != nil {
		t.Fatalf("read the versions of %s: %v", issueKey, err)
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var slug string
		var number int
		if err := rows.Scan(&slug, &number); err != nil {
			t.Fatalf("read the versions of %s: %v", issueKey, err)
		}
		counts[slug] = number
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the versions of %s: %v", issueKey, err)
	}
	return counts
}

// Every write that moves a query's record or its document's rendering leaves its document a
// version whose version_renders row records the record's digest, on an issue born converted:
//   - a requirement written into the spec through the issue's room: its settlement writes one spec
//     version, whose pass records its record's digest, and no version of the Verification document;
//   - a requirement written into the Verification document's fragment: the settlement versions
//     that document, and the spec's record moves while its rendering does not, so the settlement
//     appends one spec version holding the bytes of the one before it, the record's digest on its
//     row, its node caused by the Verification document's artifact.version;
//   - prose alone written into the spec: the rendering moved, so the settlement writes one spec
//     version, and the record did not, so its digest is the one the approval before it pinned (R14);
//   - a result recorded on the check appends one Verification version, the bytes of the one before
//     it, holding the record's new digest, its node caused by result.recorded; its retraction
//     appends another, caused by result.retracted; each is a query version that renders from the
//     tree as its stored bytes, with no difference the census reads at it, and neither moves the
//     spec.
//
// An issue that has not converted takes no version from a result recorded on its check, and
// ReviseQueries answers nothing for it and writes nothing.
func TestEveryWriteThatMovesAQuerysRecordAppendsAVersionOfIt(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour, browserPeers: true})
	documents := deps.Docs.(*docs.Service)
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "REV")
	issue := createDigestIssue(t, handler, database, "REV", "The revised issue", revisedSpec, false)
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", revisedChecks)
	spec, verification := readDecisionDocument(t, database, issue.Key, "spec"), readDecisionDocument(t, database, issue.Key, "verification-md")

	created, uploaded := newestQueryVersion(t, database, spec.id), newestQueryVersion(t, database, verification.id)
	createdDigest := requireRecordedDigest(t, database, "the spec as the create wrote it", issue.Key, graphstore.QuerySpec, created)
	requireRecordedDigest(t, database, "the Verification document as uploaded", issue.Key, graphstore.QueryVerification, uploaded)

	writeThroughTheRoom(t, handler, documents, issue.Key, spec.id, requirementMarkdown("r2", "An ask keeps its urgency."))
	worded := newestQueryVersion(t, database, spec.id)
	if worded.number != created.number+1 {
		t.Fatalf("a requirement written into the spec through the room leaves the spec at version %d, want one version after %d", worded.number, created.number)
	}
	wordedDigest := requireRecordedDigest(t, database, "a requirement written into the spec", issue.Key, graphstore.QuerySpec, worded)
	if wordedDigest == createdDigest {
		t.Errorf("a requirement written into the spec leaves the spec's digest at %s", wordedDigest)
	}
	if held := newestQueryVersion(t, database, verification.id); held.number != uploaded.number {
		t.Errorf("a requirement written into the spec gave the Verification document version %d, want it at %d", held.number, uploaded.number)
	}

	writeThroughTheRoom(t, handler, documents, issue.Key, verification.id, requirementMarkdown("r3", "A requirement written beside the checks."))
	checks := newestQueryVersion(t, database, verification.id)
	if checks.number != uploaded.number+1 {
		t.Fatalf("a requirement written into the Verification document leaves it at version %d, want %d", checks.number, uploaded.number+1)
	}
	moved := newestQueryVersion(t, database, spec.id)
	if moved.number != worded.number+1 || moved.markdown != worded.markdown {
		t.Fatalf("a requirement written into the Verification document leaves the spec at version %d holding %q, want one version after %d holding its bytes, %q",
			moved.number, moved.markdown, worded.number, worded.markdown)
	}
	movedDigest := requireRecordedDigest(t, database, "a requirement written beside the checks", issue.Key, graphstore.QuerySpec, moved)
	if movedDigest == wordedDigest {
		t.Errorf("a requirement written beside the checks leaves the spec's digest at %s", movedDigest)
	}
	requireRevisedVersion(t, database, "the spec a room write revised", spec.id, spec.refKey, moved.number,
		eventOf(t, database, "artifact.version", "payload->>'artifact_id'", verification.id))
	checksDigest := requireRecordedDigest(t, database, "the Verification document a room write versioned", issue.Key, graphstore.QueryVerification, checks)

	review := reviewDocument(t, handler, spec.id, "alice", "approved", "")
	if review.ContentSHA256 == nil || *review.ContentSHA256 != movedDigest {
		t.Fatalf("the approval of version %d of the spec pins %v, want its record's digest %s", moved.number, review.ContentSHA256, movedDigest)
	}
	const prose = "A closing paragraph of prose."
	writeThroughTheRoom(t, handler, documents, issue.Key, spec.id, prose+"\n")
	proseVersion := newestQueryVersion(t, database, spec.id)
	if proseVersion.number != moved.number+1 || !strings.Contains(proseVersion.markdown, prose) {
		t.Fatalf("prose written into the spec leaves it at version %d holding %q, want one version after %d holding the prose", proseVersion.number, proseVersion.markdown, moved.number)
	}
	if digest := requireRecordedDigest(t, database, "prose written into the spec", issue.Key, graphstore.QuerySpec, proseVersion); digest != *review.ContentSHA256 {
		t.Errorf("prose written into the spec moves its digest from the approved %s to %s, want it standing (R14)", *review.ContentSHA256, digest)
	}

	recorded := recordResult(t, handler, issue.Key, resultBody("c1", "fail", headCommit, map[string]any{"failed": 1}))
	if recorded.Code != http.StatusCreated {
		t.Fatalf("record a result of c1: status=%d body=%s", recorded.Code, recorded.Body.String())
	}
	result := decodeBody[recordedResult](t, recorded).Result
	withResult := newestQueryVersion(t, database, verification.id)
	if withResult.number != checks.number+1 || withResult.markdown != checks.markdown {
		t.Fatalf("a recorded result leaves the Verification document at version %d holding %q, want one version after %d holding its bytes, %q",
			withResult.number, withResult.markdown, checks.number, checks.markdown)
	}
	resultDigest := requireRecordedDigest(t, database, "a recorded result", issue.Key, graphstore.QueryVerification, withResult)
	if resultDigest == checksDigest {
		t.Errorf("a recorded result leaves the Verification digest at %s", resultDigest)
	}
	requireRevisedVersion(t, database, "the Verification document a result revised", verification.id, verification.refKey, withResult.number,
		eventOf(t, database, "result.recorded", "payload->>'result_id'", result.ID))

	retraction := dispatchRequest(t, handler, http.MethodPost, "/api/v1/results/"+result.ID+"/retract",
		map[string]any{"reason": "The run measured the wrong build."}, "alice")
	if retraction.Code != http.StatusCreated {
		t.Fatalf("retract the result: status=%d body=%s", retraction.Code, retraction.Body.String())
	}
	retracted := newestQueryVersion(t, database, verification.id)
	if retracted.number != withResult.number+1 || retracted.markdown != withResult.markdown {
		t.Fatalf("a retraction leaves the Verification document at version %d holding %q, want one version after %d holding its bytes, %q",
			retracted.number, retracted.markdown, withResult.number, withResult.markdown)
	}
	if digest := requireRecordedDigest(t, database, "a retraction", issue.Key, graphstore.QueryVerification, retracted); digest == resultDigest {
		t.Errorf("a retraction leaves the Verification digest at %s", digest)
	}
	requireRevisedVersion(t, database, "the Verification document a retraction revised", verification.id, verification.refKey, retracted.number,
		eventOf(t, database, "result.retracted", "payload->>'result_id'", result.ID))
	if held := newestQueryVersion(t, database, spec.id); held.number != proseVersion.number {
		t.Errorf("a result and its retraction gave the spec version %d, want it at %d", held.number, proseVersion.number)
	}

	unconverted := createDigestIssue(t, handler, database, "REV", "An issue that has not converted", revisedSpec, true)
	uploadDocument(t, handler, "/api/v1/issues/"+unconverted.Key, "verification.md", revisedChecks)
	before := issueVersionCounts(t, database.Pool, unconverted.Key)
	if response := recordResult(t, handler, unconverted.Key, resultBody("c1", "fail", headCommit, map[string]any{"failed": 1})); response.Code != http.StatusCreated {
		t.Fatalf("record a result of %s's c1: status=%d body=%s", unconverted.Key, response.Code, response.Body.String())
	}
	if after := issueVersionCounts(t, database.Pool, unconverted.Key); !maps.Equal(after, before) {
		t.Errorf("a result recorded on %s, which has not converted, leaves its documents at the versions %v, want %v", unconverted.Key, after, before)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	revised, err := documents.ReviseQueries(ctx, tx, unconverted.Key, model.Actor{Kind: "user", ID: "alice"})
	if err != nil {
		t.Fatalf("revise the queries of %s: %v", unconverted.Key, err)
	}
	if len(revised.Nodes) != 0 || len(revised.Tuples) != 0 {
		t.Errorf("ReviseQueries of %s, which has not converted, answers %d nodes and %d tuples, want none", unconverted.Key, len(revised.Nodes), len(revised.Tuples))
	}
	if inside := issueVersionCounts(t, tx, unconverted.Key); !maps.Equal(inside, before) {
		t.Errorf("ReviseQueries of %s, which has not converted, leaves its documents at the versions %v, want %v", unconverted.Key, inside, before)
	}
}

// specWriterNodeTypes are the types of the nodes only a person or an architect writes on an issue
// Legion runs - a criterion, a requirement and a check - as the issue's tree types each node
// (refs.TreeNodeType: a block by the typed block its text opens).
var specWriterNodeTypes = []string{"criterion", "requirement", "check"}

// specWriterNodeColumns are the columns of a node's graph_nodes row readSpecWriterNodes reads, in
// the order its statement reads them, each as text.
var specWriterNodeColumns = []string{"supertag", "state", "visibility", "attributes", "project_key", "parties", "made_at", "made_by", "source_event"}

// specWriterNode is what the store holds of one criterion, requirement or check node of an issue:
// its type, and its type and address as a failure names it; each column of its graph_nodes row,
// with its parent and its rank as the issue's tree walks them; and every tuple out of it, each
// graph_tuples row from it by seq, written kind>to_kind:to_id - a value's to_id being the value -
// with whether it asserts.
type specWriterNode struct {
	typ, named string
	columns    map[string]string
	tuples     map[int64]string
}

// readSpecWriterNodes reads, at the store, every criterion, requirement and check node of the tree
// of issueKey at the head (docs.WalkIssueTree), by store address.
func readSpecWriterNodes(t *testing.T, database *store.Store, issueKey string) map[string]specWriterNode {
	t.Helper()
	ctx := context.Background()
	tree, err := docs.WalkIssueTree(ctx, database.Pool, issueKey, refs.TreeCut{})
	if err != nil {
		t.Fatalf("walk the tree of %s: %v", issueKey, err)
	}
	nodes := map[string]specWriterNode{}
	for _, walked := range tree.Nodes {
		if !slices.Contains(specWriterNodeTypes, walked.Type) {
			continue
		}
		node := specWriterNode{typ: walked.Type, named: walked.Type + " " + walked.Address(), tuples: map[int64]string{},
			columns: map[string]string{"parent": walked.Parent.Kind + ":" + walked.Parent.ID, "rank": "none"}}
		if walked.Ranked {
			node.columns["rank"] = fmt.Sprintf("%q", walked.Rank)
		}
		values := make([]string, len(specWriterNodeColumns))
		targets := make([]any, len(values))
		for index := range values {
			targets[index] = &values[index]
		}
		if err := database.Pool.QueryRow(ctx, `
			select supertag, state, visibility, attributes::text, coalesce(project_key, ''), coalesce(parties::text, ''),
			       made_at::text, made_by::text, source_event::text
			from graph_nodes where kind = $1 and id = $2`, walked.Kind, walked.ID).Scan(targets...); err != nil {
			t.Fatalf("read the graph_nodes row of the %s: %v", node.named, err)
		}
		for index, column := range specWriterNodeColumns {
			node.columns[column] = values[index]
		}
		rows, err := database.Pool.Query(ctx, `
			select seq, kind, to_kind, to_id, asserted from graph_tuples where from_kind = $1 and from_id = $2`, walked.Kind, walked.ID)
		if err != nil {
			t.Fatalf("read the tuples out of the %s: %v", node.named, err)
		}
		for rows.Next() {
			var seq int64
			var kind, toKind, toID string
			var asserted bool
			if err := rows.Scan(&seq, &kind, &toKind, &toID, &asserted); err != nil {
				rows.Close()
				t.Fatalf("read the tuples out of the %s: %v", node.named, err)
			}
			node.tuples[seq] = fmt.Sprintf("%s>%s:%q asserted=%t", kind, toKind, toID, asserted)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("read the tuples out of the %s: %v", node.named, err)
		}
		nodes[walked.Address()] = node
	}
	return nodes
}

// documentVersion is one version of a document of an issue: its document's slug, and whether the
// version holds its version_renders row.
type documentVersion struct {
	slug     string
	rendered bool
}

// revisionSnapshot is what TestARecordedResultAndItsRetractionChangeNoRequirementCriterionOrCheck
// reads at the store around each write: the issue's criterion, requirement and check nodes
// (readSpecWriterNodes); the store address, <kind>:<id>, of every node the store holds; and every
// version of every document of the issue, by its version node's id (graphstore.VersionID).
type revisionSnapshot struct {
	authored map[string]specWriterNode
	nodes    map[string]bool
	versions map[string]documentVersion
}

// readRevisionSnapshot reads the snapshot of issueKey as the store holds it now.
func readRevisionSnapshot(t *testing.T, database *store.Store, issueKey string) revisionSnapshot {
	t.Helper()
	ctx := context.Background()
	snapshot := revisionSnapshot{authored: readSpecWriterNodes(t, database, issueKey), nodes: map[string]bool{}, versions: map[string]documentVersion{}}
	rows, err := database.Pool.Query(ctx, `select kind, id from graph_nodes`)
	if err != nil {
		t.Fatalf("read the store's nodes: %v", err)
	}
	for rows.Next() {
		var kind, id string
		if err := rows.Scan(&kind, &id); err != nil {
			rows.Close()
			t.Fatalf("read the store's nodes: %v", err)
		}
		snapshot.nodes[kind+":"+id] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the store's nodes: %v", err)
	}
	rows, err = database.Pool.Query(ctx, `
		select a.ref_key, a.slug, v.number, r.artifact_id is not null
		from artifacts a
		join artifact_versions v on v.artifact_id = a.id
		left join version_renders r on r.artifact_id = v.artifact_id and r.number = v.number
		where a.issue_key = $1`, issueKey)
	if err != nil {
		t.Fatalf("read the versions of %s: %v", issueKey, err)
	}
	for rows.Next() {
		var refKey string
		var number int
		var version documentVersion
		if err := rows.Scan(&refKey, &version.slug, &number, &version.rendered); err != nil {
			rows.Close()
			t.Fatalf("read the versions of %s: %v", issueKey, err)
		}
		snapshot.versions[graphstore.VersionID(refKey, number)] = version
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the versions of %s: %v", issueKey, err)
	}
	return snapshot
}

// requireTheRevisionAlone fails unless what, a write read before and after, changed no criterion,
// requirement or check node of the issue - each stands with every column, its parent, its rank
// and every tuple out of it as before - and the only nodes the store gained are the node of
// result, where result is not "", and the version node of each version the issue's documents
// gained, each a version of the spec, the Verification document or the plan holding its
// version_renders row, the Verification document gaining one. Each failure names the node and
// what moved.
func requireTheRevisionAlone(t *testing.T, what string, before, after revisionSnapshot, result string) {
	t.Helper()
	for _, address := range slices.Sorted(maps.Keys(after.authored)) {
		if _, held := before.authored[address]; !held {
			t.Errorf("%s: the issue's tree holds a new %s", what, after.authored[address].named)
		}
	}
	for _, address := range slices.Sorted(maps.Keys(before.authored)) {
		was := before.authored[address]
		now, held := after.authored[address]
		if !held {
			t.Errorf("%s: the %s is gone from the issue's tree", what, was.named)
			continue
		}
		for _, column := range slices.Sorted(maps.Keys(was.columns)) {
			if now.columns[column] != was.columns[column] {
				t.Errorf("%s: the %s moved its %s from %s to %s", what, was.named, column, was.columns[column], now.columns[column])
			}
		}
		for _, seq := range slices.Sorted(maps.Keys(now.tuples)) {
			if prior, held := was.tuples[seq]; !held {
				t.Errorf("%s: the %s holds a new tuple out of it, %s (seq %d)", what, was.named, now.tuples[seq], seq)
			} else if prior != now.tuples[seq] {
				t.Errorf("%s: the tuple at seq %d out of the %s reads %s, where it read %s", what, seq, was.named, now.tuples[seq], prior)
			}
		}
		for _, seq := range slices.Sorted(maps.Keys(was.tuples)) {
			if _, held := now.tuples[seq]; !held {
				t.Errorf("%s: the tuple %s (seq %d) out of the %s is gone", what, was.tuples[seq], seq, was.named)
			}
		}
	}

	gained := map[string]bool{}
	if result != "" {
		gained[graphstore.KindResult+":"+result] = true
	}
	revised := false
	for _, id := range slices.Sorted(maps.Keys(after.versions)) {
		if _, held := before.versions[id]; held {
			continue
		}
		version := after.versions[id]
		if !slices.Contains([]string{"spec", model.ArtifactSlugVerification, model.ArtifactSlugPlan}, version.slug) {
			t.Errorf("%s: the issue's document %s gained the version %s, and it is no query document", what, version.slug, id)
		}
		if !version.rendered {
			t.Errorf("%s: the version %s of the issue's %s holds no version_renders row, so the query revision did not write it", what, id, version.slug)
		}
		revised = revised || version.slug == model.ArtifactSlugVerification
		gained[graphstore.KindVersion+":"+id] = true
	}
	if !revised {
		t.Errorf("%s: the Verification document gained no version, so the query revision wrote nothing and nothing here holds it", what)
	}
	for _, address := range slices.Sorted(maps.Keys(after.nodes)) {
		if !before.nodes[address] && !gained[address] {
			t.Errorf("%s: the store gained the node %s, want only %v", what, address, slices.Sorted(maps.Keys(gained)))
		}
	}
	for _, address := range slices.Sorted(maps.Keys(gained)) {
		if before.nodes[address] || !after.nodes[address] {
			t.Errorf("%s: the store did not gain the node %s", what, address)
		}
	}
	for _, address := range slices.Sorted(maps.Keys(before.nodes)) {
		if !after.nodes[address] {
			t.Errorf("%s: the store lost the node %s", what, address)
		}
	}
}

// A result recorded on a check through POST /api/v1/issues/{key}/results, and its retraction
// through POST /api/v1/results/{id}/retract, change no criterion, requirement or check node of the
// issue: on an issue born converted whose spec holds a criterion and two requirements under it and
// whose Verification document holds one check serving the first, the test reads at the store,
// around each write, each such node's graph_nodes row, its parent and its rank as the tree walks
// them, and every tuple out of it, and requires each to stand as it stood. The only nodes the store
// gains are the result's, at its record, and the version node of each version the write's query
// revision (docs.Service.ReviseQueries) appended, each holding its version_renders row, the
// Verification document taking one at each write. This is what keeps those versions outside the
// spec-writer check (specWriterRevisions): a result is no requirement, criterion or check node,
// and its tuple to its check is a relation out of the result. A failure names the node and the
// column or the tuple that moved.
func TestARecordedResultAndItsRetractionChangeNoRequirementCriterionOrCheck(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createDigestIssue(t, handler, database, "CORE", "The revised issue", digestSpec, false)
	if issue.Key != "CORE-1" {
		t.Fatalf("the issue is %s, want CORE-1, which its spec's references name", issue.Key)
	}
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", digestChecks("The list reads urgent asks first.", ""))

	before := readRevisionSnapshot(t, database, issue.Key)
	types := map[string]int{}
	for _, node := range before.authored {
		types[node.typ]++
	}
	if want := map[string]int{"criterion": 1, "requirement": 2, "check": 1}; !maps.Equal(types, want) {
		t.Fatalf("the tree of %s holds the criterion, requirement and check nodes %v, want %v: the spec's criterion and two requirements and the Verification document's check",
			issue.Key, types, want)
	}

	recorded := recordResult(t, handler, issue.Key, resultBody("c1", "fail", headCommit, map[string]any{"failed": 1}))
	if recorded.Code != http.StatusCreated {
		t.Fatalf("record a result of c1: status=%d body=%s", recorded.Code, recorded.Body.String())
	}
	result := decodeBody[recordedResult](t, recorded).Result
	withResult := readRevisionSnapshot(t, database, issue.Key)
	requireTheRevisionAlone(t, "a recorded result", before, withResult, result.ID)

	retraction := dispatchRequest(t, handler, http.MethodPost, "/api/v1/results/"+result.ID+"/retract",
		map[string]any{"reason": "The run measured the wrong build."}, "alice")
	if retraction.Code != http.StatusCreated {
		t.Fatalf("retract the result: status=%d body=%s", retraction.Code, retraction.Body.String())
	}
	requireTheRevisionAlone(t, "its retraction", withResult, readRevisionSnapshot(t, database, issue.Key), "")
}
