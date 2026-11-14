package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// testDecisionBlockCheck is one entry of an edit's advice.decision_block_checks.
type testDecisionBlockCheck struct {
	BlockID     string             `json:"block_id"`
	Question    string             `json:"question"`
	Suggestions *model.Suggestions `json:"suggestions"`
}

// decisionBlockChecksOf reads an edit response's advice.decision_block_checks, and whether the
// advice holds the field at all.
func decisionBlockChecksOf(t *testing.T, response *httptest.ResponseRecorder) ([]testDecisionBlockCheck, bool) {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("edit: status=%d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Advice map[string]json.RawMessage `json:"advice"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode edit response: %v", err)
	}
	raw, present := envelope.Advice["decision_block_checks"]
	if !present {
		return nil, false
	}
	var checks []testDecisionBlockCheck
	if err := json.Unmarshal(raw, &checks); err != nil {
		t.Fatalf("decode decision_block_checks %s: %v", raw, err)
	}
	return checks, true
}

// decisionBlockRow is one related_checks row of a decision block source.
type decisionBlockRow struct {
	IssueKey   *string
	ArtifactID *string
	BlockID    string
	Outcome    string
	Reason     *string
	ActorID    string
}

func readDecisionBlockRows(t *testing.T, database *store.Store) []decisionBlockRow {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select source_issue_key, source_artifact_id::text, source_block_id, outcome, reason, actor_id
		  from related_checks where source_kind = 'decision_block' order by created_at, source_block_id`)
	if err != nil {
		t.Fatalf("read decision block checks: %v", err)
	}
	defer rows.Close()
	var found []decisionBlockRow
	for rows.Next() {
		var row decisionBlockRow
		if err := rows.Scan(&row.IssueKey, &row.ArtifactID, &row.BlockID, &row.Outcome, &row.Reason, &row.ActorID); err != nil {
			t.Fatalf("scan decision block check: %v", err)
		}
		found = append(found, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read decision block checks: %v", err)
	}
	return found
}

// suggestedOwnerKeys is the issue each related item of suggestions stands for.
func suggestedOwnerKeys(suggestions *model.Suggestions) []string {
	if suggestions == nil {
		return nil
	}
	return relatedOwnerKeys(suggestions.Related)
}

func editDocument(t *testing.T, handler http.Handler, artifactID string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", body, "alice")
}

func insertAtEnd(markdown string) map[string]any {
	return map[string]any{"ops": []map[string]any{{"op": "insert", "markdown": markdown, "after": "end"}}}
}

// A decision block an edit adds is checked like an ask: its question finds the issue that holds
// its rare words, the answer names the block, and the check is recorded against the document's
// issue. An edit after it that adds no block - plain text, a reworded block - answers no check.
func TestDecisionBlockCheckAnswersTheEditsNewBlock(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "DBLK")
	target := createSearchIssue(t, handler, "DBLK", "Dashboard ledger for zymurgy batches", "")
	spec := "# Spec\n\nPlanning notes.\n"
	issue := createAdviceIssue(t, handler, "DBLK", "Quarterly planning", &spec)
	refreshTermStats(t, database, "DBLK")

	const question = "Should zymurgy ledger totals round down?"
	checks, present := decisionBlockChecksOf(t, editDocument(t, handler, issue.PrimaryArtifactID,
		insertAtEnd(":::ask{#rounding}\n"+question+"\n:::")))
	if !present || len(checks) != 1 {
		t.Fatalf("decision_block_checks = %+v (present %t), want one", checks, present)
	}
	check := checks[0]
	if check.BlockID != "rounding" || check.Question != question {
		t.Fatalf("check = %+v, want the block rounding and its question", check)
	}
	if check.Suggestions == nil || check.Suggestions.Outcome != "found" || !slices.Contains(suggestedOwnerKeys(check.Suggestions), target) {
		t.Fatalf("check suggestions = %+v, want %s found", check.Suggestions, target)
	}
	if slices.Contains(suggestedOwnerKeys(check.Suggestions), issue.Key) {
		t.Fatalf("check suggestions = %+v, want the document's own issue %s left out", check.Suggestions, issue.Key)
	}
	rows := readDecisionBlockRows(t, database)
	if len(rows) != 1 || rows[0].BlockID != "rounding" || rows[0].IssueKey == nil || *rows[0].IssueKey != issue.Key ||
		rows[0].ArtifactID != nil || rows[0].Outcome != "found" || rows[0].ActorID != "alice" {
		t.Fatalf("decision block rows = %+v, want one found check of rounding on %s", rows, issue.Key)
	}

	for name, body := range map[string]map[string]any{
		"plain text": insertAtEnd("A plain revision."),
		"a reworded block": {"ops": []map[string]any{{
			"op": "replace", "find": question, "with": "Should zymurgy ledger totals round up?",
		}}},
	} {
		checks, present := decisionBlockChecksOf(t, editDocument(t, handler, issue.PrimaryArtifactID, body))
		if present {
			t.Fatalf("%s: decision_block_checks = %+v, want the field absent", name, checks)
		}
	}
	if rows := readDecisionBlockRows(t, database); len(rows) != 1 {
		t.Fatalf("decision block rows = %+v, want only the first edit's check", rows)
	}
}

// An edit adding six decision blocks checks the first five, in document order, and answers the
// sixth that its check was not made, recording that too.
func TestDecisionBlockChecksPastFiveAreNotMade(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "SIX")
	createSearchIssue(t, handler, "SIX", "Dashboard ledger for zymurgy batches", "")
	spec := "# Spec\n\nPlanning notes.\n"
	issue := createAdviceIssue(t, handler, "SIX", "Six decisions", &spec)
	refreshTermStats(t, database, "SIX")

	var markdown []string
	for index := range 6 {
		markdown = append(markdown, fmt.Sprintf(":::ask{#choice%d}\nWhich zymurgy ledger layout %d?\n:::", index, index))
	}
	checks, present := decisionBlockChecksOf(t, editDocument(t, handler, issue.PrimaryArtifactID, insertAtEnd(strings.Join(markdown, "\n\n"))))
	if !present || len(checks) != 6 {
		t.Fatalf("decision_block_checks = %+v (present %t), want six", checks, present)
	}
	for index, check := range checks {
		if check.BlockID != fmt.Sprintf("choice%d", index) || check.Question != fmt.Sprintf("Which zymurgy ledger layout %d?", index) {
			t.Fatalf("check %d = %+v, want block choice%d in document order", index, check, index)
		}
		if check.Suggestions == nil {
			t.Fatalf("check %d has no suggestions", index)
		}
		if index < 5 && check.Suggestions.Outcome == "not_made" {
			t.Fatalf("check %d = %+v, want it made", index, check.Suggestions)
		}
	}
	sixth := checks[5].Suggestions
	if sixth.Outcome != "not_made" || sixth.Missing != "more than five new decision blocks in one edit" ||
		sixth.Related == nil || len(sixth.Related) != 0 {
		t.Fatalf("sixth check = %+v, want not made for too many blocks", sixth)
	}
	rows := readDecisionBlockRows(t, database)
	if len(rows) != 6 {
		t.Fatalf("decision block rows = %+v, want six", rows)
	}
	var tooMany []string
	for _, row := range rows {
		if row.Reason != nil && *row.Reason == "too_many_blocks" {
			if row.Outcome != "not_made" {
				t.Fatalf("too_many_blocks row = %+v, want not made", row)
			}
			tooMany = append(tooMany, row.BlockID)
		}
	}
	if !slices.Equal(tooMany, []string{"choice5"}) {
		t.Fatalf("too_many_blocks rows = %v, want choice5 alone", tooMany)
	}
}

// A conditional edit's gate is released at its commit: a second conditional edit of the same
// document completes while the first is still waiting on its decision block's check, here held
// behind a lock on the term statistics every check reads.
func TestDecisionBlockCheckDoesNotHoldTheConditionalEditGate(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "GATE")
	spec := "# Spec\n\nPlanning notes.\n"
	issue := createAdviceIssue(t, handler, "GATE", "Gated edits", &spec)

	lockTx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin lock tx: %v", err)
	}
	t.Cleanup(func() { _ = lockTx.Rollback(context.Background()) })
	if _, err := lockTx.Exec(context.Background(), "lock table search_term_stats in access exclusive mode"); err != nil {
		t.Fatalf("lock search_term_stats: %v", err)
	}

	const question = "Which zymurgy deployment window?"
	first := insertAtEnd(":::ask{#window}\n" + question + "\n:::")
	first["precondition"] = map[string]string{"document": readDocumentPrecondition(t, handler, issue.PrimaryArtifactID).Token}
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- editDocument(t, handler, issue.PrimaryArtifactID, first) }()

	// The first edit has committed once the document holds its block, and its check is running
	// once something waits on the lock. (Another waiter, the term statistics refresh the issue's
	// own check started, may be the one seen: the commit is what the second edit needs.) The second
	// edit is guarded by the paragraph it rewrites, which the first block's settlement leaves alone.
	deadline := time.Now().Add(10 * time.Second)
	var second map[string]any
	for {
		var waiting int
		if err := database.Pool.QueryRow(context.Background(), `
			select count(*) from pg_locks where relation = 'search_term_stats'::regclass and not granted
		`).Scan(&waiting); err != nil {
			t.Fatalf("read lock waits: %v", err)
		}
		if waiting > 0 && strings.Contains(readDocumentPrecondition(t, handler, issue.PrimaryArtifactID).Markdown, question) {
			paragraph := readDocumentPreconditionBlocks(t, handler, issue.PrimaryArtifactID)[1]
			second = map[string]any{
				"ops":          []map[string]string{{"op": "replace", "find": "Planning notes.", "with": "Planning notes, revised."}},
				"precondition": map[string]any{"blocks": []map[string]string{{"id": paragraph.ID, "token": paragraph.Token}}},
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first edit never committed with a check waiting on the term statistics")
		}
		time.Sleep(10 * time.Millisecond)
	}

	secondResponse := editDocument(t, handler, issue.PrimaryArtifactID, second)
	select {
	case response := <-firstDone:
		t.Fatalf("the first edit answered (status %d) before the second, which waited on its gate", response.Code)
	default:
	}
	if secondResponse.Code != http.StatusOK {
		t.Fatalf("second edit: status=%d body=%s", secondResponse.Code, secondResponse.Body.String())
	}

	if err := lockTx.Rollback(context.Background()); err != nil {
		t.Fatalf("release the lock: %v", err)
	}
	checks, present := decisionBlockChecksOf(t, <-firstDone)
	if !present || len(checks) != 1 || checks[0].BlockID != "window" {
		t.Fatalf("first edit's decision_block_checks = %+v, want the block window", checks)
	}
}

// A decision block on a project document, which has no issue, is checked in the document's
// project with the document itself left out, though it holds the block's words, and is recorded
// against that document.
func TestDecisionBlockCheckOnAProjectDocumentLeavesTheDocumentOut(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "PDBL")
	target := createSearchIssue(t, handler, "PDBL", "Dashboard ledger for zymurgy batches", "")
	upload := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/PDBL/artifacts", map[string]string{
		"name": "zymurgy-notes.md", "content": "# Zymurgy ledger notes\n\nThe zymurgy ledger totals drift.\n",
	}, "alice")
	if upload.Code != http.StatusCreated {
		t.Fatalf("project upload: status=%d body=%s", upload.Code, upload.Body.String())
	}
	document := decodeBody[struct {
		Artifact struct {
			ID string `json:"id"`
		} `json:"artifact"`
	}](t, upload).Artifact.ID
	refreshTermStats(t, database, "PDBL")

	checks, present := decisionBlockChecksOf(t, editDocument(t, handler, document,
		insertAtEnd(":::ask{#rounding}\nShould zymurgy ledger totals round down?\n:::")))
	if !present || len(checks) != 1 || checks[0].BlockID != "rounding" || checks[0].Suggestions == nil {
		t.Fatalf("decision_block_checks = %+v (present %t), want the block rounding checked", checks, present)
	}
	suggestions := checks[0].Suggestions
	if suggestions.Outcome != "found" || !slices.Contains(suggestedOwnerKeys(suggestions), target) {
		t.Fatalf("suggestions = %+v, want %s found", suggestions, target)
	}
	for _, item := range suggestions.Related {
		if item.ID == document || item.Owner.ArtifactID == document {
			t.Fatalf("suggestions = %+v, want the document %s itself left out", suggestions, document)
		}
	}
	rows := readDecisionBlockRows(t, database)
	if len(rows) != 1 || rows[0].IssueKey != nil || rows[0].ArtifactID == nil || *rows[0].ArtifactID != document ||
		rows[0].BlockID != "rounding" || rows[0].Outcome != "found" {
		t.Fatalf("decision block rows = %+v, want one found check of rounding on the document %s", rows, document)
	}
}

// routeWeightOf is the admission weight the table gives one route: the most request-pool
// connections a request of it may hold at once (apiRoute.Weight, a weight below one being one).
func routeWeightOf(t *testing.T, method, pattern string) int32 {
	t.Helper()
	for _, route := range (&server{}).routes() {
		if route.Method == method && route.Pattern == pattern {
			return int32(max(route.Weight, 1))
		}
	}
	t.Fatalf("no route %s %s in the table", method, pattern)
	return 0
}

// An edit holds no more request-pool connections than its route's admission weight, whatever its
// decision blocks: its checks run one after another, so only one check's reads are in flight,
// where five at once asked for three connections each against a route admission counted one
// connection for. The one check the edit is running is held where each of its three reads has
// taken its connection - the keyword search in its open rows (relatedSearchEntered), the meaning
// stage in its scan transaction (meaningSearchEntered) and the cross-project leg in its own
// (crossProjectScanEntered) - and each is let go only when this test lets it go, so what the
// edit holds is read while all three stand still rather than sampled for a peak.
//
// The server has an embedder: without one a check stops at its keyword stage
// (relatedReasonNoEmbedder) and holds one connection, which would leave the weight the route is
// admitted for measured by a third of the check it is for. Its settlement cannot fire while the
// three are held (settle), so the check's own reads are every connection of the request pool
// there is to count.
func TestAnEditHoldsNoMoreConnectionsThanItsRouteWeighs(t *testing.T) {
	embedder := &fakeEmbedder{}
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{embedder: embedder, settle: time.Hour})
	createSuggestionProject(t, handler, "WGHT")
	createSearchIssue(t, handler, "WGHT", "Dashboard ledger for zymurgy batches", "")
	spec := "# Spec\n\nPlanning notes.\n"
	issue := createAdviceIssue(t, handler, "WGHT", "Five decisions", &spec)
	refreshTermStats(t, database, "WGHT")
	// The seeds' own checks are over before the marks below are armed, and the pool holds
	// nothing again, so every connection counted while they are held is this edit's.
	awaitQuietRequestPool(t, database.Pool)

	release := make(chan struct{})
	var released sync.Once
	releaseChecks := func() { released.Do(func() { close(release) }) }
	defer releaseChecks()
	// Each mark reports the first read that reaches it and holds that read, its connection
	// taken, until the test releases every one of them. The later checks' reads pass straight
	// through a closed release.
	mark := func() (<-chan struct{}, func()) {
		entered, once := make(chan struct{}), &sync.Once{}
		return entered, func() {
			once.Do(func() { close(entered) })
			<-release
		}
	}
	keyword, holdKeyword := mark()
	meaning, holdMeaning := mark()
	cross, holdCross := mark()
	served.relatedSearchEntered = holdKeyword
	served.meaningSearchEntered = holdMeaning
	served.crossProjectScanEntered = holdCross

	markdown := make([]string, 0, decisionBlockCheckLimit)
	for index := range decisionBlockCheckLimit {
		markdown = append(markdown, fmt.Sprintf(":::ask{#choice%d}\nWhich zymurgy ledger layout %d?\n:::", index, index))
	}
	edited := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		edited <- editDocument(t, handler, issue.PrimaryArtifactID, insertAtEnd(strings.Join(markdown, "\n\n")))
	}()
	awaitMark := func(what string, entered <-chan struct{}) {
		t.Helper()
		select {
		case <-entered:
		case <-time.After(20 * time.Second):
			t.Fatalf("the edit's check never reached its %s read with a connection in hand", what)
		}
	}
	awaitMark("keyword", keyword)
	awaitMark("meaning", meaning)
	awaitMark("cross-project", cross)

	// All three reads of the one check the edit is running are held, so this is what the edit
	// holds at its most, read rather than raced for.
	held := database.Pool.Stat().AcquiredConns()
	if weight := routeWeightOf(t, http.MethodPost, "/api/v1/artifacts/{id}/edits"); held != weight {
		t.Errorf("an edit adding %d decision blocks held %d request-pool connections with its check's keyword, meaning and cross-project reads all held, want exactly its route's weight %d",
			decisionBlockCheckLimit, held, weight)
	}

	releaseChecks()
	checks, present := decisionBlockChecksOf(t, <-edited)
	if !present || len(checks) != decisionBlockCheckLimit {
		t.Fatalf("decision_block_checks = %+v (present %t), want %d", checks, present, decisionBlockCheckLimit)
	}
	for index, check := range checks {
		if check.Suggestions == nil || check.Suggestions.Outcome == "not_made" {
			t.Fatalf("check %d = %+v, want it made once the reads were let go", index, check.Suggestions)
		}
	}
}

// Every one of an edit's blocks is checked whole, and each reads its items' ask states and
// writes its record: every block whose question restates an ask is offered that ask with its
// state on the item, and every block's check is recorded. The checks ran at once once, and on
// the request's one connection mark one check's open cursor had the pool refuse another's
// ask-state read and record (store.ErrNestedAcquire), which dropped the state and the row; they
// run one after another now (decisionBlockChecks), each on a mark of its own. Every ask shares
// a word with every block and carries a long answer, so each check reads every ask's state over
// several network reads.
func TestDecisionBlockChecksEachReadTheirAskStatesAndRecord(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "RACE")
	const shared = " For the aerostat programme."
	blocks := []struct{ id, title, asked, restated string }{
		{"hangar", "Storage planning", "Which zeppelin hangar should store the blimps?", "Which zeppelin hangar stores the blimps?"},
		{"lacquer", "Paint planning", "Which lacquer coats the dirigible gondolas?", "Which lacquer should coat dirigible gondolas?"},
		{"pilot", "Crew planning", "Who pilots the airship on weekends?", "Who should pilot the airship at weekends?"},
		{"fjord", "Route planning", "Should the balloon cross the fjord at dawn?", "Does the balloon cross the fjord at dawn?"},
		{"harbour", "Dock planning", "Which harbour moors the hovercraft?", "Which harbour should moor the hovercraft?"},
	}
	asks := map[string]string{}
	for _, block := range blocks {
		owner := createSearchIssue(t, handler, "RACE", block.title, "")
		opened := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+owner+"/asks", map[string]any{
			"question": block.asked + shared,
			"options":  []map[string]string{{"label": "Brass"}, {"label": "Steel"}},
		}, "alice")
		if opened.Code != http.StatusCreated {
			t.Fatalf("open the %s ask: status=%d body=%s", block.id, opened.Code, opened.Body.String())
		}
		asks[block.id] = decodeBody[testAskWithSuggestions](t, opened).ID
		answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+asks[block.id]+"/answer",
			map[string]any{"selected": []string{"Brass"}, "text": strings.Repeat("ballast ", contracts.DispatchEventBodyMax/len("ballast "))}, "alice")
		if answered.Code != http.StatusOK {
			t.Fatalf("answer the %s ask: status=%d body=%s", block.id, answered.Code, answered.Body.String())
		}
	}
	spec := "# Spec\n\nPlanning notes.\n"
	issue := createAdviceIssue(t, handler, "RACE", "Fleet decisions", &spec)
	refreshTermStats(t, database, "RACE")

	var markdown []string
	for _, block := range blocks {
		markdown = append(markdown, ":::ask{#"+block.id+"}\n"+block.restated+shared+"\n:::")
	}
	checks, present := decisionBlockChecksOf(t, editDocument(t, handler, issue.PrimaryArtifactID, insertAtEnd(strings.Join(markdown, "\n\n"))))
	if !present || len(checks) != len(blocks) {
		t.Fatalf("decision_block_checks = %+v (present %t), want %d", checks, present, len(blocks))
	}
	for index, check := range checks {
		block := blocks[index]
		if check.BlockID != block.id || check.Suggestions == nil {
			t.Fatalf("check %d = %+v, want block %s checked", index, check, block.id)
		}
		listed := false
		for _, item := range check.Suggestions.Related {
			if item.Kind == "ask" && item.ID == asks[block.id] {
				listed = item.Ask != nil && item.Ask.State == "answered"
			}
		}
		if !listed {
			t.Fatalf("block %s: related = %+v, want the ask %s listed with its answered state", block.id, check.Suggestions.Related, asks[block.id])
		}
	}
	var recorded []string
	for _, row := range readDecisionBlockRows(t, database) {
		recorded = append(recorded, row.BlockID)
	}
	slices.Sort(recorded)
	if want := []string{"fjord", "hangar", "harbour", "lacquer", "pilot"}; !slices.Equal(recorded, want) {
		t.Fatalf("recorded decision blocks = %v, want %v", recorded, want)
	}
}
