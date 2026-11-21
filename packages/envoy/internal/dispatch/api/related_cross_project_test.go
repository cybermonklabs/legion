package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// crossProjectQuestion is the question every case of this file checks with. The fake embedder
// answers queryVector() for it, so a seeded item's cosine with the check's text is exactly the
// similarity the case gave that item (angledVector).
const crossProjectQuestion = "Which cache do we drop first when the backlog grows?"

// crossProjectIssueSpec is the spec of the issue the create form of the check files, so that
// create's own text is known and can be mapped to the query vector too.
const crossProjectIssueSpec = "Weekly planning notes."

// crossProjectQueryTexts is every text one of the five writes embeds for crossProjectQuestion.
// An ask create, a new decision block and both forms of the related route embed the question
// alone; an issue create embeds its title, a newline and its document as it was stored, which
// this maps in each shape the store may hold it (with and without the trailing newline), so the
// create's vector is the query's whatever the serializer writes.
func crossProjectQueryTexts() []string {
	return []string{
		crossProjectQuestion,
		crossProjectQuestion + "\n",
		crossProjectQuestion + "\n" + crossProjectIssueSpec,
		crossProjectQuestion + "\n" + crossProjectIssueSpec + "\n",
	}
}

// crossProjectEmbedder is fakeEmbedder with a hold a case turns on once it has seeded: the seed
// writes run their own checks, and a hold set up front would cost each of them the meaning
// allowance. Embed reads the hold under the lock hold() writes it under, since the check calls
// it from a goroutine of its own.
type crossProjectEmbedder struct {
	*fakeEmbedder
	mu   sync.Mutex
	held time.Duration
}

func newCrossProjectEmbedder() *crossProjectEmbedder {
	return &crossProjectEmbedder{fakeEmbedder: &fakeEmbedder{vectors: map[string][]float32{}}}
}

func (e *crossProjectEmbedder) Embed(ctx context.Context, texts []string, inputType embed.InputType) ([][]float32, error) {
	e.mu.Lock()
	held := e.held
	e.mu.Unlock()
	if held > 0 {
		select {
		case <-time.After(held):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return e.fakeEmbedder.Embed(ctx, texts, inputType)
}

// hold makes every later call wait that long before it answers.
func (e *crossProjectEmbedder) hold(delay time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.held = delay
}

// failingCrossProjectRead is a test seam of the cross-project leg (server.beforeCrossProjectRead)
// that makes the leg's read fail. A case installs it before it seeds and arms it for the one
// check it measures, so every seed write's own check runs the leg as production runs it.
type failingCrossProjectRead struct {
	mu  sync.Mutex
	err error
}

// read is what the leg calls before it reads: it fails once the case armed it to.
func (f *failingCrossProjectRead) read(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

func (f *failingCrossProjectRead) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// crossProjectAsker seeds the project that asks: a target issue the check can only find by
// meaning, since it shares no word with the question, a host issue the question is written on,
// and an answered question of the asking project's own, nearer the question than anything the
// other project holds. That last one is what the leg's project rule has to leave out: without
// it, a leg reading its own project too would answer exactly as this one does.
//
// It answers the target's key, the own-question issue's key, the host's key and the host's
// primary document.
func crossProjectAsker(
	t *testing.T, handler http.Handler, database *store.Store, embedder *crossProjectEmbedder, project string,
) (target, own, host, hostArtifact string) {
	t.Helper()
	createSuggestionProject(t, handler, project)
	const targetTitle, targetSpec = "Harbour crane rota drifts", "The crane rota slips by one shift each month."
	embedder.vectors[issueEmbeddingText(targetTitle, targetSpec)] = angledVector(0.85, 9)
	target = createSearchIssue(t, handler, project, targetTitle, targetSpec)
	own = createSearchIssue(t, handler, project, "Our own settled questions", "Questions this project settled itself.")
	seedAnsweredQuestion(t, handler, database, embedder, own,
		"Which cache does this project drop first when the backlog grows?",
		"We drop the rendered page cache first, and keep the index cache.", 0.95, 10)
	spec := "# Spec\n\n" + crossProjectIssueSpec + "\n"
	issue := createAdviceIssue(t, handler, project, "Weekly rota planning", &spec)
	return target, own, issue.Key, issue.PrimaryArtifactID
}

// seedAnsweredQuestion creates one question ask of issueKey, answers it with text, and maps
// every window of it to a vector whose cosine with the check's text is similarity.
func seedAnsweredQuestion(
	t *testing.T, handler http.Handler, database *store.Store, embedder *crossProjectEmbedder,
	issueKey, question, answer string, similarity float64, axis int,
) string {
	t.Helper()
	id := createSearchAsk(t, handler, issueKey, question, "Yes", "No")
	answerSearchAsk(t, handler, id, map[string]any{"text": answer})
	embedWindowsAs(t, database, embedder.fakeEmbedder, "ask_part", "ask/"+id, angledVector(similarity, axis))
	return id
}

// withoutSearchableEvalsDocument takes project's seeded evals-md (dispatch://INFRA-2916, R5) out of
// the project's keyword search: it deletes the document's document_search row, the row the related
// check's document arm matches words on. It waits for the seed's settlement first, since a version
// a settlement wrote would put the row back (20261101182454's trigger). The document stays, and so
// do its windows, which the meaning arms read and which the fake embedder maps to defaultVector,
// below the meaning floor of every query here.
func withoutSearchableEvalsDocument(t *testing.T, database *store.Store, project string) {
	t.Helper()
	var id string
	if err := database.Pool.QueryRow(context.Background(),
		`select id::text from artifacts where project_key = $1 and slug = $2`, project, evals.DocumentSlug,
	).Scan(&id); err != nil {
		t.Fatalf("read %s's %s: %v", project, evals.DocumentSlug, err)
	}
	awaitSettled(t, database, id)
	removed, err := database.Pool.Exec(context.Background(), `delete from document_search where artifact_id = $1`, id)
	if err != nil {
		t.Fatalf("take %s's %s out of keyword search: %v", project, evals.DocumentSlug, err)
	}
	if removed.RowsAffected() != 1 {
		t.Fatalf("took %d search rows of %s's %s out, want its one", removed.RowsAffected(), project, evals.DocumentSlug)
	}
}

// crossProjectCheck runs one check whose write is a question, as an ask create runs it.
func crossProjectCheck(srv *server, project string) *model.Suggestions {
	return srv.relatedCheck(context.Background(), relatedInput{
		project: project, text: crossProjectQuestion, title: crossProjectQuestion, question: true,
	})
}

// crossProjectItemIDs is the ask each offered cross-project item stands for, in order.
func crossProjectItemIDs(items []model.WriteSuggestion) []string {
	ids := make([]string, len(items))
	for index, item := range items {
		ids[index] = item.ID
	}
	return ids
}

// relatedItemIDs is the item each same-project suggestion stands for, in order.
func relatedItemIDs(items []model.WriteSuggestion) []string {
	ids := make([]string, len(items))
	for index, item := range items {
		ids[index] = item.Kind + ":" + item.ID
	}
	return ids
}

// crossAdvice reads a write response's advice.suggestions whole, cross-project items included.
func crossAdvice(t *testing.T, what string, response *httptest.ResponseRecorder) *model.Suggestions {
	t.Helper()
	if response.Code != http.StatusCreated && response.Code != http.StatusOK {
		t.Fatalf("%s: status=%d body=%s", what, response.Code, response.Body.String())
	}
	var envelope struct {
		Advice *struct {
			Suggestions *model.Suggestions `json:"suggestions"`
		} `json:"advice"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode %s: %v", what, err)
	}
	if envelope.Advice == nil || envelope.Advice.Suggestions == nil {
		t.Fatalf("%s: advice.suggestions missing: %s", what, response.Body.String())
	}
	return envelope.Advice.Suggestions
}

// crossRouteSuggestions reads the no-write related route's answer whole.
func crossRouteSuggestions(t *testing.T, what string, response *httptest.ResponseRecorder) *model.Suggestions {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("%s: status=%d body=%s", what, response.Code, response.Body.String())
	}
	return decodeBody[*model.Suggestions](t, response)
}

// askStateAndAnswer is the row the cross-project statement judges an ask by: its kind, its state
// and the text of its answer.
func askStateAndAnswer(t *testing.T, database *store.Store, askID string) (kind, state, text string) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(),
		`select kind, state, coalesce(answer->>'text', '') from asks where id = $1`, askID,
	).Scan(&kind, &state, &text); err != nil {
		t.Fatalf("read the ask %s: %v", askID, err)
	}
	return kind, state, text
}

// embeddedAskWindows is how many windows of kind ask_part the ask holds that carry a vector: the
// rows the cross-project statement reads. A row of embeddings of kind ask_part is a window of
// neighbouring parts, not one part (itemblocks.WindowTextChars).
func embeddedAskWindows(t *testing.T, database *store.Store, askID string) int {
	t.Helper()
	var windows int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from embeddings
		 where kind = 'ask_part' and split_part(id, '#', 1) = $1 and embedding is not null`,
		"ask/"+askID).Scan(&windows); err != nil {
		t.Fatalf("count the windows of the ask %s: %v", askID, err)
	}
	return windows
}

// askWindowsHold reports whether any window of the ask holds phrase: what the leg matches on is
// the window's text, not the ask's row, so a case that moves the row reads this to show the
// window still matches.
func askWindowsHold(t *testing.T, database *store.Store, askID, phrase string) bool {
	t.Helper()
	for _, text := range windowTexts(t, database, "ask_part", "ask/"+askID) {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

// A question write offers the answered question of another project, and a write that is not a
// question offers none. The five forms the check runs in are all here: an ask create, a new
// decision block and the POST form of the related route are questions to a person; an issue
// create and the GET form are not.
//
// Each form's answer also says it searched by meaning and keywords and lists the asking
// project's target issue, which shares no word with the question and can only be found by
// meaning: that is what says the check embedded the text this case mapped, so a form offering no
// cross-project item is one the rule left out rather than one whose vector missed.
func TestCrossProjectRelatedOffersAnAnsweredQuestionToQuestionWritesOnly(t *testing.T) {
	embedder := newCrossProjectEmbedder()
	for _, text := range crossProjectQueryTexts() {
		embedder.vectors[text] = queryVector()
	}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	target, own, host, hostArtifact := crossProjectAsker(t, handler, database, embedder, "XASK")
	// XASK holds its seeded evals-md (dispatch://INFRA-2916, R5), whose methods hold the question's
	// "first" and "backlog". Searchable, it would be a same-project item of every check here, and
	// the check below that leaves XASK's issues out would find it and answer found whatever XOTH
	// offers: the one answer that cannot show the outcome is the same-project result's alone. A
	// project's document is not removed (DELETE /api/v1/artifacts/{id} answers
	// ARTIFACT_PROJECT_DOCUMENT), so the fixture takes it out of the keyword search instead.
	withoutSearchableEvalsDocument(t, database, "XASK")
	createSuggestionProject(t, handler, "XOTH")
	owner := createSearchIssue(t, handler, "XOTH", "Backlog runbook", "What to do when the backlog grows.")
	offered := seedAnsweredQuestion(t, handler, database, embedder, owner,
		"When the backlog grows, which store do we empty first?",
		"Empty the page store first; the index store costs more to rebuild.", 0.9, 2)
	refreshTermStats(t, database, "XASK")
	processAllPending(t, database, embedder)

	// The GET form, which the new-issue dialog runs: no cross-project item.
	params := url.Values{"title": {crossProjectQuestion}}
	got := crossRouteSuggestions(t, "GET related", dispatchRequest(t, handler, http.MethodGet,
		"/api/v1/projects/XASK/related?"+params.Encode(), nil, "alice"))
	if got.Search != relatedMeaningAndKeywords || !slices.Contains(relatedOwnerKeys(got.Related), target) {
		t.Fatalf("the GET form = %+v, want it searched by meaning and keywords and listed %s", got, target)
	}
	if len(got.OtherProjects) != 0 {
		t.Fatalf("the GET form offered %+v, want no answered question of another project", got.OtherProjects)
	}

	// The POST form, which `dispatch related` runs: the same same-project items, and the
	// answered question of the other project beside them.
	posted := crossRouteSuggestions(t, "POST related", dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/projects/XASK/related", map[string]any{"title": crossProjectQuestion}, "alice"))
	if posted.Search != relatedMeaningAndKeywords || posted.Outcome != relatedFound {
		t.Fatalf("the POST form = %+v, want found by meaning and keywords", posted)
	}
	if !slices.Equal(relatedItemIDs(posted.Related), relatedItemIDs(got.Related)) || len(posted.Related) == 0 {
		t.Fatalf("the POST form's items = %v, want the GET form's %v unchanged and ahead of the other project's",
			relatedItemIDs(posted.Related), relatedItemIDs(got.Related))
	}
	if ids := crossProjectItemIDs(posted.OtherProjects); !slices.Equal(ids, []string{offered}) {
		t.Fatalf("the POST form offered %v, want the one answered question %s of XOTH", ids, offered)
	}
	item := posted.OtherProjects[0]
	if item.Kind != "ask" || item.Owner.Project != "XOTH" || item.Owner.Key != owner {
		t.Fatalf("the offered item = %+v, want the ask of %s, carrying its project XOTH", item, owner)
	}
	if item.Ask == nil || item.Ask.Answer == nil || strings.TrimSpace(deref(item.Ask.Answer.Text)) == "" {
		t.Fatalf("the offered item = %+v, want the ask state and the answer the row brought with it", item)
	}
	for _, related := range posted.Related {
		if related.ID == offered || related.Owner.Project == "XOTH" {
			t.Fatalf("related = %+v, want the other project's answered question in other_projects alone", posted.Related)
		}
	}
	// The asking project holds an answered question of its own, nearer than XOTH's, which the
	// leg reads no project of its own to find: it is a same-project item and nothing else.
	for _, item := range posted.OtherProjects {
		if item.Owner.Project == "XASK" {
			t.Fatalf("offered %+v, want nothing of the asking project: the leg reads the other projects", item)
		}
	}
	if !slices.Contains(relatedOwnerKeys(posted.Related), own) {
		t.Fatalf("related = %v, want the asking project's own answered question (%s) among the same-project items",
			relatedOwnerKeys(posted.Related), own)
	}

	// The outcome is the same-project result's alone: with the one same-project item left out,
	// a check that still offers the other project's answered question says it found nothing.
	none := crossRouteSuggestions(t, "POST related excluding the target", dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/projects/XASK/related", map[string]any{
			"title": crossProjectQuestion, "exclude_issues": []string{target, own},
		}, "alice"))
	if none.Outcome != relatedNone || len(none.Related) != 0 {
		t.Fatalf("the check with %s left out = %+v, want the outcome none its own project decides", target, none)
	}
	if ids := crossProjectItemIDs(none.OtherProjects); !slices.Equal(ids, []string{offered}) {
		t.Fatalf("the check with %s left out offered %v, want %s still", target, ids, offered)
	}

	// An ask create.
	created := crossAdvice(t, "the ask create", dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/issues/"+host+"/asks", map[string]any{"question": crossProjectQuestion}, "alice"))
	if created.Search != relatedMeaningAndKeywords || !slices.Contains(relatedOwnerKeys(created.Related), target) {
		t.Fatalf("the ask create = %+v, want it searched by meaning and keywords and listed %s", created, target)
	}
	if ids := crossProjectItemIDs(created.OtherProjects); !slices.Equal(ids, []string{offered}) {
		t.Fatalf("the ask create offered %v, want %s", ids, offered)
	}

	// An issue create, which asks nobody anything.
	filed := crossAdvice(t, "the issue create", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "XASK", "title": crossProjectQuestion, "spec": crossProjectIssueSpec, "force": true,
	}, "alice"))
	if filed.Search != relatedMeaningAndKeywords || !slices.Contains(relatedOwnerKeys(filed.Related), target) {
		t.Fatalf("the issue create = %+v, want it searched by meaning and keywords and listed %s", filed, target)
	}
	if len(filed.OtherProjects) != 0 {
		t.Fatalf("the issue create offered %+v, want no answered question of another project", filed.OtherProjects)
	}

	// A new decision block.
	checks, present := decisionBlockChecksOf(t, editDocument(t, handler, hostArtifact,
		insertAtEnd(":::ask{#evict}\n"+crossProjectQuestion+"\n:::")))
	if !present || len(checks) != 1 || checks[0].Suggestions == nil {
		t.Fatalf("decision_block_checks = %+v (present %t), want one with its suggestions", checks, present)
	}
	block := checks[0].Suggestions
	if ids := crossProjectItemIDs(block.OtherProjects); !slices.Equal(ids, []string{offered}) {
		t.Fatalf("the decision block offered %v, want %s", ids, offered)
	}
	t.Logf("measured: the ask create, the decision block and the POST form each offered the ask %s of XOTH at cosine 0.90; "+
		"the issue create and the GET form offered none, both having searched by meaning and keywords and listed %s",
		offered, target)
}

// Only an answered question whose answer carries text is offered. Six asks of the other project
// stand nearer the question than the one that qualifies and none of them is offered: one nobody
// answered, one answered with a chosen option and no text, one answered with a blank text, one
// resolved after it was answered with text, one request to approve a document, and one
// answered question below the meaning floor.
//
// Each of the six carries a vector of its own, which the case counts first: what leaves them out
// is the statement's rule, not a missing row.
func TestCrossProjectRelatedSkipsAsksThatDoNotQualify(t *testing.T) {
	embedder := newCrossProjectEmbedder()
	for _, text := range crossProjectQueryTexts() {
		embedder.vectors[text] = queryVector()
	}
	handler, database, deps := newTestServer(t, testServerOptions{embedder: embedder})
	target, _, _, _ := crossProjectAsker(t, handler, database, embedder, "XQUAL")
	createSuggestionProject(t, handler, "XANS")
	spec := "# Spec\n\nHow the backlog is drained.\n"
	owner := createAdviceIssue(t, handler, "XANS", "Backlog drain notes", &spec)

	unanswered := createSearchAsk(t, handler, owner.Key, "Which queue do we drain first at peak?", "Yes", "No")
	embedWindowsAs(t, database, embedder.fakeEmbedder, "ask_part", "ask/"+unanswered, angledVector(0.95, 2))

	optionOnly := createSearchAsk(t, handler, owner.Key, "Do we drain the queue nightly?", "Yes", "No")
	answerSearchAsk(t, handler, optionOnly, map[string]any{"selected": []string{"Yes"}})
	embedWindowsAs(t, database, embedder.fakeEmbedder, "ask_part", "ask/"+optionOnly, angledVector(0.94, 3))

	blankText := createSearchAsk(t, handler, owner.Key, "Do we drain the queue each morning?", "Yes", "No")
	answerSearchAsk(t, handler, blankText, map[string]any{"selected": []string{"Yes"}, "text": "   "})
	embedWindowsAs(t, database, embedder.fakeEmbedder, "ask_part", "ask/"+blankText, angledVector(0.93, 4))

	resolved := seedAnsweredQuestion(t, handler, database, embedder, owner.Key,
		"Which buffer do we flush when the backlog grows?",
		"Flush the write buffer; the read buffer refills itself.", 0.92, 5)
	// The state a settlement retraction of an answered ask leaves behind (search_ask_test's
	// legacy settlement case): resolved, its answer left in its ask.answered event alone, since
	// the schema admits an answer on an answered row only (asks_answer_state_check). The
	// windows the answer was indexed into stay as they are, so this ask still stands at cosine
	// 0.92 of the question through the answer's own words; its state is what leaves it out. No
	// route writes this row, so it is written here.
	if tag, err := database.Pool.Exec(context.Background(), `
		update asks set state = 'resolved', answer = null,
		    resolution = jsonb_build_object('kind', 'resolved', 'reason', 'the document moved on',
		      'actor', jsonb_build_object('kind', 'user', 'id', 'alice'), 'at', now())
		 where id = $1`, resolved); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("resolve the answered ask %s: %v (%d rows)", resolved, err, tag.RowsAffected())
	}
	// A write of an ask's answer marks it owed a block index and owed its windows
	// (item_blocks_owe, migration 20261107194001), and the statement above writes that column.
	// The marks are this statement's own artefact: no route writes this row, so nothing in the
	// server owes the arms anything for it, and they are cleared here so the parts and the
	// windows the answer was indexed into stay as they are, which is what this ask stands for.
	// Left standing, the window arm would read the row's parts, find them out of step with the
	// part rows the index holds - the answer part this statement took off the row - and window
	// the ask to nothing (refs.WriteItemWindows, currentItemBlockIDs).
	if tag, err := database.Pool.Exec(context.Background(),
		`update ask_block_state set owed_at = null, windows_owed_at = null where ask_id = $1`,
		resolved); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("clear the index marks of the settled ask %s: %v (%d rows)", resolved, err, tag.RowsAffected())
	}

	requested := sessionRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+owner.PrimaryArtifactID+"/approval-requests", map[string]any{"actor": sessionActor()})
	if requested.Code != http.StatusCreated {
		t.Fatalf("request approval: status=%d body=%s", requested.Code, requested.Body.String())
	}
	approval := decodeBody[struct {
		Ask struct {
			ID string `json:"id"`
		} `json:"ask"`
	}](t, requested).Ask.ID
	if answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+approval+"/answer", map[string]any{
		"selected": []string{"Request changes"}, "text": "Say which cache the drain empties first.",
	}, "alice"); answered.Code != http.StatusOK {
		t.Fatalf("answer the approval request: status=%d body=%s", answered.Code, answered.Body.String())
	}
	embedWindowsAs(t, database, embedder.fakeEmbedder, "ask_part", "ask/"+approval, angledVector(0.91, 6))

	belowFloor := seedAnsweredQuestion(t, handler, database, embedder, owner.Key,
		"Which log do we trim when the disk fills?",
		"Trim the debug log first; the audit log is kept whole.", 0.1, 7)
	keeper := seedAnsweredQuestion(t, handler, database, embedder, owner.Key,
		"When the backlog grows, which store do we empty first?",
		"Empty the page store first; the index store costs more to rebuild.", 0.5, 8)
	refreshTermStats(t, database, "XQUAL")
	processAllPending(t, database, embedder)

	for _, ask := range []struct {
		what string
		id   string
	}{
		{"the unanswered question", unanswered}, {"the question answered with an option alone", optionOnly},
		{"the question answered with a blank text", blankText}, {"the resolved question", resolved},
		{"the request to approve a document", approval}, {"the question below the meaning floor", belowFloor},
		{"the question that qualifies", keeper},
	} {
		if windows := embeddedAskWindows(t, database, ask.id); windows == 0 {
			t.Fatalf("%s (%s) carries no embedded window; the case would prove nothing about the rule", ask.what, ask.id)
		}
	}
	if kind, state, _ := askStateAndAnswer(t, database, resolved); kind != "question" || state != "resolved" {
		t.Fatalf("the resolved ask = (%s, %s), want a resolved question", kind, state)
	}
	if !askWindowsHold(t, database, resolved, "the read buffer refills itself") {
		t.Fatalf("the resolved ask %s holds no window carrying its answer; "+
			"its state is then not what leaves it out of the offer", resolved)
	}
	if kind, state, text := askStateAndAnswer(t, database, approval); kind != "approval" || state != "answered" || text == "" {
		t.Fatalf("the approval ask = (%s, %s, answer text %q), want an answered approval carrying text", kind, state, text)
	}
	if _, _, text := askStateAndAnswer(t, database, blankText); strings.TrimSpace(text) != "" {
		t.Fatalf("the blank-text answer holds %q, want only whitespace", text)
	}

	suggestions := crossProjectCheck(directServer(deps), "XQUAL")
	if suggestions.Search != relatedMeaningAndKeywords || !slices.Contains(relatedOwnerKeys(suggestions.Related), target) {
		t.Fatalf("suggestions = %+v, want searched by meaning and keywords, listing %s", suggestions, target)
	}
	if ids := crossProjectItemIDs(suggestions.OtherProjects); !slices.Equal(ids, []string{keeper}) {
		t.Fatalf("offered %v, want the one answered question with text, %s, alone", ids, keeper)
	}
	t.Logf("measured: of seven asks of XANS, each carrying an embedded window, only %s (cosine 0.50) was offered; "+
		"the unanswered (0.95), option-only (0.94), blank-text (0.93), resolved (0.92) and approval (0.91) asks all "+
		"stood nearer and were left out, and the answered question at cosine 0.10 is under the %.2f floor",
		keeper, searchMeaningFloor)
}

// An ask whose question and whose answer both match is offered once. Its parts are longer than
// one window holds (itemblocks.WindowTextChars is 1200), so it has more than one row of
// embeddings of kind ask_part and the leg must fold them to the ask; the case counts those rows
// first and fails below two, since a short ask makes one window and would pass without
// exercising the fold.
func TestCrossProjectRelatedOffersAnAskMatchedThroughTwoPartsOnce(t *testing.T) {
	embedder := newCrossProjectEmbedder()
	for _, text := range crossProjectQueryTexts() {
		embedder.vectors[text] = queryVector()
	}
	handler, database, deps := newTestServer(t, testServerOptions{embedder: embedder})
	target, _, _, _ := crossProjectAsker(t, handler, database, embedder, "XTWO")
	createSuggestionProject(t, handler, "XLONG")
	owner := createSearchIssue(t, handler, "XLONG", "Cache sizing review", "How each cache is sized.")
	question := "Which cache do we drop first when the backlog grows? " +
		strings.Repeat("The page cache holds the rendered pages a reader asks for most often. ", 20)
	answer := "Drop the page cache first. " +
		strings.Repeat("The index cache is dearer to rebuild, since every shard is read again. ", 20)
	if len(question) < itemblocks.WindowTextChars || len(answer) < itemblocks.WindowTextChars {
		t.Fatalf("the seeded question (%d chars) and answer (%d chars) must each be longer than a window of %d",
			len(question), len(answer), itemblocks.WindowTextChars)
	}
	offered := seedAnsweredQuestion(t, handler, database, embedder, owner, question, answer, 0.9, 2)
	refreshTermStats(t, database, "XTWO")
	processAllPending(t, database, embedder)

	windows := embeddedAskWindows(t, database, offered)
	if windows < 2 {
		t.Fatalf("the ask %s holds %d embedded ask_part window(s), want at least two: "+
			"one window would let the case pass without exercising the fold", offered, windows)
	}
	suggestions := crossProjectCheck(directServer(deps), "XTWO")
	if suggestions.Search != relatedMeaningAndKeywords || !slices.Contains(relatedOwnerKeys(suggestions.Related), target) {
		t.Fatalf("suggestions = %+v, want searched by meaning and keywords, listing %s", suggestions, target)
	}
	if ids := crossProjectItemIDs(suggestions.OtherProjects); !slices.Equal(ids, []string{offered}) {
		t.Fatalf("offered %v, want the ask %s once", ids, offered)
	}
	t.Logf("measured: the ask %s holds %d windows of kind ask_part, each at cosine 0.90, and is offered once", offered, windows)
}

// Two qualifying answered questions of one issue are both offered: the leg offers asks, and
// nothing folds them to their owner the way the same-project items are folded.
func TestCrossProjectRelatedOffersTwoQualifyingAsksOfOneIssue(t *testing.T) {
	embedder := newCrossProjectEmbedder()
	for _, text := range crossProjectQueryTexts() {
		embedder.vectors[text] = queryVector()
	}
	handler, database, deps := newTestServer(t, testServerOptions{embedder: embedder})
	target, _, _, _ := crossProjectAsker(t, handler, database, embedder, "XPAIR")
	createSuggestionProject(t, handler, "XONE")
	owner := createSearchIssue(t, handler, "XONE", "Backlog drain review", "One issue, two settled questions.")
	first := seedAnsweredQuestion(t, handler, database, embedder, owner,
		"When the backlog grows, which store do we empty first?",
		"Empty the page store first; the index store costs more to rebuild.", 0.9, 2)
	second := seedAnsweredQuestion(t, handler, database, embedder, owner,
		"Which buffer do we flush when the backlog grows?",
		"Flush the write buffer; the read buffer refills itself.", 0.8, 3)
	refreshTermStats(t, database, "XPAIR")
	processAllPending(t, database, embedder)

	suggestions := crossProjectCheck(directServer(deps), "XPAIR")
	if suggestions.Search != relatedMeaningAndKeywords || !slices.Contains(relatedOwnerKeys(suggestions.Related), target) {
		t.Fatalf("suggestions = %+v, want searched by meaning and keywords, listing %s", suggestions, target)
	}
	if ids := crossProjectItemIDs(suggestions.OtherProjects); !slices.Equal(ids, []string{first, second}) {
		t.Fatalf("offered %v, want both asks of %s: %s (0.90) then %s (0.80)", ids, owner, first, second)
	}
	for _, item := range suggestions.OtherProjects {
		if item.Owner.Key != owner || item.Owner.Project != "XONE" {
			t.Fatalf("the offered item %+v, want it owned by %s of XONE", item, owner)
		}
	}
	t.Logf("measured: both answered questions of %s were offered, at cosine 0.90 and 0.80", owner)
}

// At most two are offered, and they are the two nearest in meaning: four answered questions of
// the other project qualify, at cosine 0.90, 0.80, 0.70 and 0.60.
func TestCrossProjectRelatedOffersAtMostTwoAndTheNearest(t *testing.T) {
	embedder := newCrossProjectEmbedder()
	for _, text := range crossProjectQueryTexts() {
		embedder.vectors[text] = queryVector()
	}
	handler, database, deps := newTestServer(t, testServerOptions{embedder: embedder})
	target, _, _, _ := crossProjectAsker(t, handler, database, embedder, "XBEST")
	createSuggestionProject(t, handler, "XFOUR")
	owner := createSearchIssue(t, handler, "XFOUR", "Backlog questions", "Four settled questions.")
	questions := []struct {
		question, answer string
		similarity       float64
		axis             int
	}{
		{"When the backlog grows, which store do we empty first?", "Empty the page store first.", 0.9, 2},
		{"Which buffer do we flush when the backlog grows?", "Flush the write buffer first.", 0.8, 3},
		{"Which log do we trim when the backlog grows?", "Trim the debug log first.", 0.7, 4},
		{"Which shard do we move when the backlog grows?", "Move the coldest shard first.", 0.6, 5},
	}
	seeded := make([]string, 0, len(questions))
	for _, seed := range questions {
		seeded = append(seeded, seedAnsweredQuestion(t, handler, database, embedder, owner,
			seed.question, seed.answer, seed.similarity, seed.axis))
	}
	refreshTermStats(t, database, "XBEST")
	processAllPending(t, database, embedder)

	suggestions := crossProjectCheck(directServer(deps), "XBEST")
	if suggestions.Search != relatedMeaningAndKeywords || !slices.Contains(relatedOwnerKeys(suggestions.Related), target) {
		t.Fatalf("suggestions = %+v, want searched by meaning and keywords, listing %s", suggestions, target)
	}
	if ids := crossProjectItemIDs(suggestions.OtherProjects); !slices.Equal(ids, seeded[:suggestionOtherProjectCount]) {
		t.Fatalf("offered %v, want the %d nearest of the four: %v", ids, suggestionOtherProjectCount, seeded[:suggestionOtherProjectCount])
	}
	t.Logf("measured: of four qualifying asks at cosine 0.90, 0.80, 0.70 and 0.60, the %d offered were the 0.90 and the 0.80",
		suggestionOtherProjectCount)
}

// The leg reads the embedding the stage already made: one check whose cross-project item is
// offered costs exactly one call of the embedder.
func TestCrossProjectRelatedMakesNoSecondEmbeddingCall(t *testing.T) {
	embedder := newCrossProjectEmbedder()
	for _, text := range crossProjectQueryTexts() {
		embedder.vectors[text] = queryVector()
	}
	handler, database, deps := newTestServer(t, testServerOptions{embedder: embedder})
	crossProjectAsker(t, handler, database, embedder, "XONCE")
	createSuggestionProject(t, handler, "XCALL")
	owner := createSearchIssue(t, handler, "XCALL", "Backlog runbook", "What to do when the backlog grows.")
	offered := seedAnsweredQuestion(t, handler, database, embedder, owner,
		"When the backlog grows, which store do we empty first?",
		"Empty the page store first; the index store costs more to rebuild.", 0.9, 2)
	refreshTermStats(t, database, "XONCE")
	processAllPending(t, database, embedder)

	before := embedderCalls(embedder.fakeEmbedder)
	suggestions := crossProjectCheck(directServer(deps), "XONCE")
	calls := embedderCalls(embedder.fakeEmbedder) - before
	if ids := crossProjectItemIDs(suggestions.OtherProjects); !slices.Equal(ids, []string{offered}) {
		t.Fatalf("offered %v, want %s: a check that offered nothing would say nothing about the call count", ids, offered)
	}
	if calls != 1 {
		t.Fatalf("the check called the embedder %d times, want exactly one: the leg reuses the stage's embedding", calls)
	}
	t.Logf("measured: one check that offered the ask %s cost %d call of the embedder", offered, calls)
}

// With the embedder answering nothing inside the allowance, the check has no vector: it answers
// its keyword search's result, says the meaning search did not answer in time, and offers no
// answered question of another project.
func TestCrossProjectRelatedWithoutAnEmbeddingOffersNothing(t *testing.T) {
	embedder := newCrossProjectEmbedder()
	for _, text := range crossProjectQueryTexts() {
		embedder.vectors[text] = queryVector()
	}
	handler, database, deps := newTestServer(t, testServerOptions{embedder: embedder})
	createSuggestionProject(t, handler, "XQUIET")
	keyword := createSearchIssue(t, handler, "XQUIET", "Cache drop order for the backlog", "")
	createSuggestionProject(t, handler, "XSILENT")
	owner := createSearchIssue(t, handler, "XSILENT", "Backlog runbook", "What to do when the backlog grows.")
	offered := seedAnsweredQuestion(t, handler, database, embedder, owner,
		"When the backlog grows, which store do we empty first?",
		"Empty the page store first; the index store costs more to rebuild.", 0.9, 2)
	refreshTermStats(t, database, "XQUIET")
	processAllPending(t, database, embedder)
	// Proof the corpus is reachable while the embedder answers, so the silent run below is the
	// embedder's doing and not an empty corpus.
	if ids := crossProjectItemIDs(crossProjectCheck(directServer(deps), "XQUIET").OtherProjects); !slices.Equal(ids, []string{offered}) {
		t.Fatalf("offered %v while the embedder answered, want %s", ids, offered)
	}

	embedder.hold(time.Minute)
	started := time.Now()
	suggestions := crossProjectCheck(directServer(deps), "XQUIET")
	elapsed := time.Since(started)
	if suggestions.Search != relatedKeywordsOnly ||
		suggestions.SearchNote != "meaning search did not answer within "+relatedMeaningAllowance.String() {
		t.Fatalf("suggestions = %+v, want the keyword result and the note that meaning search did not answer", suggestions)
	}
	if !slices.Contains(relatedOwnerKeys(suggestions.Related), keyword) {
		t.Fatalf("related = %+v, want the keyword match %s to stand", suggestions.Related, keyword)
	}
	if len(suggestions.OtherProjects) != 0 {
		t.Fatalf("offered %+v, want none: with no vector there is nothing to search another project by", suggestions.OtherProjects)
	}
	t.Logf("measured: with the embedder held, the check answered in %s with the keyword item %s, the note %q and no cross-project item",
		elapsed.Round(time.Millisecond), keyword, suggestions.SearchNote)
}

// The cross-project query alone overrunning costs the check nothing but its own items: the
// answer holds the fused same-project items, says it searched by meaning and keywords, carries
// no cross-project item, and the check is recorded as made.
//
// The grace, and nothing later, ends the check's wait for the leg. The case gives the server a
// meaning allowance of an hour (server.meaningBound, with the check's own allowance raised to
// match) and holds the write's leg until it releases it, so only the grace can end that wait
// before the hour is up: a check whose grace does not bound it fails by name before the binary's
// deadline. The write took at least the grace, since no timer fires early, and when it answered
// the check had already ended the leg's context, with the meaning deadline an hour off.
func TestCrossProjectRelatedQueryOverrunningKeepsTheOneProjectAnswer(t *testing.T) {
	embedder := newCrossProjectEmbedder()
	for _, text := range crossProjectQueryTexts() {
		embedder.vectors[text] = queryVector()
	}
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{embedder: embedder})
	held := newHeldCrossProjectRead(t)
	served.beforeCrossProjectRead = held.read
	const meaningBound = time.Hour
	served.meaningBound, served.suggestionTimeout = meaningBound, meaningBound
	target, _, host, _ := crossProjectAsker(t, handler, database, embedder, "XSLOW")
	createSuggestionProject(t, handler, "XLATE")
	owner := createSearchIssue(t, handler, "XLATE", "Backlog runbook", "What to do when the backlog grows.")
	seedAnsweredQuestion(t, handler, database, embedder, owner,
		"When the backlog grows, which store do we empty first?",
		"Empty the page store first; the index store costs more to rebuild.", 0.9, 2)
	refreshTermStats(t, database, "XSLOW")
	processAllPending(t, database, embedder)

	body, err := json.Marshal(map[string]any{"question": crossProjectQuestion})
	if err != nil {
		t.Fatalf("encode the ask: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/issues/"+host+"/asks", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "alice")
	request = request.WithContext(context.WithValue(request.Context(), heldReadKey{}, true))
	started := time.Now()
	response := answeredBeforeTheBinaryDeadline(t, "the ask create", func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	})
	elapsed := time.Since(started)
	created := crossAdvice(t, "the ask create", response)
	if created.Outcome != relatedFound || created.Search != relatedMeaningAndKeywords || created.SearchNote != "" {
		t.Fatalf("suggestions = %+v, want found by meaning and keywords, with no note", created)
	}
	if !slices.Contains(relatedOwnerKeys(created.Related), target) {
		t.Fatalf("related = %+v, want the fused same-project items, %s among them", created.Related, target)
	}
	if len(created.OtherProjects) != 0 || created.Missing != "" {
		t.Fatalf("suggestions = %+v, want no cross-project item and nothing missing", created)
	}
	rows := readRelatedCheckRows(t, database, "source_ask_id = $1", decodeBody[model.Ask](t, response).ID)
	if len(rows) != 1 || rows[0].Outcome == relatedNotMade || rows[0].Reason != nil ||
		rows[0].Search == nil || *rows[0].Search != relatedMeaningAndKeywords {
		t.Fatalf("related_checks = %+v, want one row made, searched by meaning and keywords, with no reason", rows)
	}
	if elapsed < crossProjectGrace {
		t.Fatalf("the write answered in %s, under the %s grace: the check did not wait for the leg at all", elapsed, crossProjectGrace)
	}
	leg := held.next(t)
	held.release()
	if deadline, ok := leg.ctx.Deadline(); !ok || deadline.Before(started.Add(meaningBound)) || deadline.After(leg.at.Add(meaningBound)) {
		t.Fatalf("the leg ran to %v (set: %t), want the meaning deadline, %s from the check's start, which lies between %v and %v",
			deadline, ok, meaningBound, started.Add(meaningBound), leg.at.Add(meaningBound))
	}
	if err := leg.ctx.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("when the write answered, its leg's context read %v, want %v: the check did not end the leg when its grace ran out",
			err, context.Canceled)
	}
	t.Logf("measured: with the leg held and the meaning deadline an hour off, the write answered in %s, past the %s grace, "+
		"with the same-project items whole, no cross-project item and the leg's read ended",
		elapsed.Round(time.Millisecond), crossProjectGrace)
}

// A cross-project query that fails leaves the same answer, and records no reason: the leg is
// offered beside the check's own result and never decides it.
func TestCrossProjectRelatedQueryFailingKeepsTheOneProjectAnswer(t *testing.T) {
	embedder := newCrossProjectEmbedder()
	for _, text := range crossProjectQueryTexts() {
		embedder.vectors[text] = queryVector()
	}
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{embedder: embedder})
	failing := &failingCrossProjectRead{}
	served.beforeCrossProjectRead = failing.read
	target, _, host, _ := crossProjectAsker(t, handler, database, embedder, "XFAIL")
	createSuggestionProject(t, handler, "XBROKE")
	owner := createSearchIssue(t, handler, "XBROKE", "Backlog runbook", "What to do when the backlog grows.")
	seedAnsweredQuestion(t, handler, database, embedder, owner,
		"When the backlog grows, which store do we empty first?",
		"Empty the page store first; the index store costs more to rebuild.", 0.9, 2)
	refreshTermStats(t, database, "XFAIL")
	processAllPending(t, database, embedder)

	failing.fail(errors.New("the other projects' read failed"))
	started := time.Now()
	response := dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/issues/"+host+"/asks", map[string]any{"question": crossProjectQuestion}, "alice")
	elapsed := time.Since(started)
	created := crossAdvice(t, "the ask create", response)
	if created.Outcome != relatedFound || created.Search != relatedMeaningAndKeywords || created.SearchNote != "" {
		t.Fatalf("suggestions = %+v, want found by meaning and keywords, with no note", created)
	}
	if !slices.Contains(relatedOwnerKeys(created.Related), target) || len(created.OtherProjects) != 0 {
		t.Fatalf("suggestions = %+v, want the same-project items with %s among them and no cross-project item", created, target)
	}
	rows := readRelatedCheckRows(t, database, "source_ask_id = $1", decodeBody[model.Ask](t, response).ID)
	if len(rows) != 1 || rows[0].Outcome == relatedNotMade || rows[0].Reason != nil {
		t.Fatalf("related_checks = %+v, want one row made, with no reason recorded", rows)
	}
	t.Logf("measured: with the cross-project read failing, the write answered in %s, found by meaning and keywords, "+
		"with no cross-project item and no reason recorded", elapsed.Round(time.Millisecond))
}

// heldReadKey marks the context of the one check whose cross-project read heldCrossProjectRead
// holds. Every other check on the same server, a seed write's own among them, reads as it reads
// in production, however late its leg's goroutine reaches the read.
type heldReadKey struct{}

// heldRead is the read heldCrossProjectRead held: the context the leg read on and the moment the
// leg came to read.
type heldRead struct {
	ctx context.Context
	at  time.Time
}

// heldCrossProjectRead stands in for the cross-project leg's read (server.beforeCrossProjectRead)
// of the check whose context carries heldReadKey: it hands the case that read, then holds the
// leg until the case releases it, whatever its context does, as a read that has not come back
// holds it. Nothing the leg does, then, ends the check's wait for it.
type heldCrossProjectRead struct {
	reads    chan heldRead
	released chan struct{}
	free     sync.Once
}

// newHeldCrossProjectRead returns a stand-in the case's cleanup releases, so a case that fails
// before it releases the leg leaves no goroutine held.
func newHeldCrossProjectRead(t *testing.T) *heldCrossProjectRead {
	held := &heldCrossProjectRead{reads: make(chan heldRead, 1), released: make(chan struct{})}
	t.Cleanup(held.release)
	return held
}

func (h *heldCrossProjectRead) read(ctx context.Context) error {
	if ctx.Value(heldReadKey{}) == nil {
		return nil
	}
	select {
	case h.reads <- heldRead{ctx: ctx, at: time.Now()}:
	default:
	}
	<-h.released
	return errors.New("the held cross-project read was released")
}

func (h *heldCrossProjectRead) release() { h.free.Do(func() { close(h.released) }) }

// next waits for the marked check's leg to come to its read: the leg runs on a goroutine of its
// own, which the check does not wait for.
func (h *heldCrossProjectRead) next(t *testing.T) heldRead {
	t.Helper()
	select {
	case read := <-h.reads:
		return read
	case <-binaryDeadlineGuard(t):
		t.Fatalf("the check's cross-project leg had not come to its read %s before the test binary's deadline", binaryDeadlineMargin)
		return heldRead{}
	}
}

// The boundary: the same-project query answers inside the meaning allowance with less than the
// grace left. The case gives the check a grace of an hour (server.crossProjectWait) and a leg
// that never answers on its own, so less than the grace is left whatever the meaning query costs
// and only the meaning deadline can end the check's wait for the leg: a check whose grace that
// deadline does not cut fails by name before the binary's deadline. The check starts at a moment
// the case reads, so the meaning deadline is a value the case knows rather than a moment it
// races. The answer is still meaning and keywords with the same-project items whole, the leg ran
// to that deadline exactly, and that deadline had passed when the check answered: the grace was
// cut at it, not sooner.
func TestCrossProjectRelatedAnswersWithLessThanTheGraceLeft(t *testing.T) {
	embedder := newCrossProjectEmbedder()
	for _, text := range crossProjectQueryTexts() {
		embedder.vectors[text] = queryVector()
	}
	handler, database, deps := newTestServer(t, testServerOptions{embedder: embedder})
	held := newHeldCrossProjectRead(t)
	srv := directServer(deps)
	srv.beforeCrossProjectRead = held.read
	srv.crossProjectWait = time.Hour
	target, _, _, _ := crossProjectAsker(t, handler, database, embedder, "XEDGE")
	createSuggestionProject(t, handler, "XNEAR")
	owner := createSearchIssue(t, handler, "XNEAR", "Backlog runbook", "What to do when the backlog grows.")
	seedAnsweredQuestion(t, handler, database, embedder, owner,
		"When the backlog grows, which store do we empty first?",
		"Empty the page store first; the index store costs more to rebuild.", 0.9, 2)
	refreshTermStats(t, database, "XEDGE")
	processAllPending(t, database, embedder)

	started := time.Now()
	meaningDeadline := started.Add(relatedMeaningAllowance)
	marked := context.WithValue(context.Background(), heldReadKey{}, true)
	suggestions := answeredBeforeTheBinaryDeadline(t, "the check", func() *model.Suggestions {
		answer, _ := srv.runRelatedCheck(marked, relatedInput{
			project: "XEDGE", text: crossProjectQuestion, title: crossProjectQuestion, question: true,
		}, started)
		return answer
	})
	elapsed := time.Since(started)
	if suggestions.Search != relatedMeaningAndKeywords || suggestions.SearchNote != "" {
		t.Fatalf("suggestions = %+v, want meaning and keywords: the stage answered inside its allowance", suggestions)
	}
	if !slices.Contains(relatedOwnerKeys(suggestions.Related), target) || len(suggestions.OtherProjects) != 0 {
		t.Fatalf("suggestions = %+v, want the same-project items whole, %s among them, and no cross-project item", suggestions, target)
	}
	// The stage answered, so its embedding is in hand and its leg was started. Its context ended
	// before runRelatedCheck returned, which ends the meaning context the leg reads on.
	read := held.next(t)
	held.release()
	leg := read.ctx
	if deadline, ok := leg.Deadline(); !ok || !deadline.Equal(meaningDeadline) {
		t.Fatalf("the leg ran to %v (set: %t), want the meaning deadline %v, %s from the check's start: the meaning search is not bounded by its allowance",
			deadline, ok, meaningDeadline, relatedMeaningAllowance)
	}
	if err := leg.Err(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("when the check answered, its leg's context read %v, want %v: the grace ended before the meaning deadline did",
			err, context.DeadlineExceeded)
	}
	t.Logf("measured: with an hour's grace and the leg held, the check waited until the meaning deadline (%s from its start) "+
		"and answered in %s, by meaning and keywords, with the same-project items whole and no cross-project item",
		relatedMeaningAllowance, elapsed.Round(time.Millisecond))
}

// The statement filters the asks it must not offer rather than paging past them: two asks that
// do not qualify, one unanswered and one answered with no text, both stand nearer the question
// than the one that does, and the qualifying ask is still offered.
func TestCrossProjectRelatedFiltersNonQualifyingAsksRatherThanPagingPastThem(t *testing.T) {
	embedder := newCrossProjectEmbedder()
	for _, text := range crossProjectQueryTexts() {
		embedder.vectors[text] = queryVector()
	}
	handler, database, deps := newTestServer(t, testServerOptions{embedder: embedder})
	target, _, _, _ := crossProjectAsker(t, handler, database, embedder, "XDEPTH")
	createSuggestionProject(t, handler, "XABOVE")
	owner := createSearchIssue(t, handler, "XABOVE", "Backlog runbook", "What to do when the backlog grows.")
	nearer := createSearchAsk(t, handler, owner, "Which queue do we drain first at peak?", "Yes", "No")
	embedWindowsAs(t, database, embedder.fakeEmbedder, "ask_part", "ask/"+nearer, angledVector(0.95, 2))
	textFree := createSearchAsk(t, handler, owner, "Do we drain the queue nightly?", "Yes", "No")
	answerSearchAsk(t, handler, textFree, map[string]any{"selected": []string{"Yes"}})
	embedWindowsAs(t, database, embedder.fakeEmbedder, "ask_part", "ask/"+textFree, angledVector(0.94, 3))
	offered := seedAnsweredQuestion(t, handler, database, embedder, owner,
		"When the backlog grows, which store do we empty first?",
		"Empty the page store first; the index store costs more to rebuild.", 0.4, 4)
	refreshTermStats(t, database, "XDEPTH")
	processAllPending(t, database, embedder)

	suggestions := crossProjectCheck(directServer(deps), "XDEPTH")
	if suggestions.Search != relatedMeaningAndKeywords || !slices.Contains(relatedOwnerKeys(suggestions.Related), target) {
		t.Fatalf("suggestions = %+v, want searched by meaning and keywords, listing %s", suggestions, target)
	}
	if ids := crossProjectItemIDs(suggestions.OtherProjects); !slices.Equal(ids, []string{offered}) {
		t.Fatalf("offered %v, want %s, which stands behind two asks the leg must not offer", ids, offered)
	}
	t.Logf("measured: the unanswered ask (cosine 0.95) and the ask answered with no text (0.94) both stand above %s (0.40), "+
		"which the leg still offers; the statement asks for %d rows (contracts.SearchKindDepth) and filters those two "+
		"inside its candidate window rather than paging past them",
		offered, contracts.SearchKindDepth)
}
