package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/modeltest"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A decision block an agent writes in a document (INFRA-3231's R5, Decisions 3 and 23): with
// DISPATCH_REVIEWER_DECISION_BLOCKS=hold it shows in its document at once and stays out of its
// reader's Inbox and the open-ask counts until it posts; its writer gets the findings; its
// post shows it, as written or rewritten through the one decision-block rewrite path.

// decisionBlockDocument is a document holding one decision block whose question the stand-in
// flags.
const decisionBlockDocument = "# Rollout\n\nThe plan for the rollout.\n\n:::ask{urgency=\"med\"}\nShip the reviewer?\n" +
	vacuousPhrase + "\n\n- Ship it: Release it today.\n- Do not file: Wait.\n:::\n"

// uploadDecisionBlock uploads decisionBlockDocument as the writer, the person-typed header set
// where typed says so, and answers the id of the ask its settlement opens.
func (rig *reviewRig) uploadDecisionBlock(typed bool) string {
	rig.t.Helper()
	return rig.uploadDecisionBlockOf(decisionBlockDocument, typed)
}

// uploadDecisionBlockOf is uploadDecisionBlock of content, a document holding one decision block.
func (rig *reviewRig) uploadDecisionBlockOf(content string, typed bool) string {
	rig.t.Helper()
	body, err := json.Marshal(map[string]any{
		"name": "rollout.md", "content": content,
		"actor": map[string]any{"kind": "session", "id": heldWriter},
	})
	if err != nil {
		rig.t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/issues/"+rig.issue+"/artifacts", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer agent-token")
	if typed {
		request.Header.Set(contracts.PersonTypedHeader, "1")
	}
	response := httptest.NewRecorder()
	rig.handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		rig.t.Fatalf("the document with a decision block: %d %s", response.Code, response.Body.String())
	}
	return rig.blockAsk()
}

// blockAsk waits for the ask of the one decision block a write put in a document of the rig's
// issue, which its settlement opens, and answers its id.
func (rig *reviewRig) blockAsk() string {
	rig.t.Helper()
	var askID string
	pollUntil(rig.t, 20*time.Second, "the decision block's ask", func() bool {
		return rig.database.Pool.QueryRow(context.Background(), `
			select id::text from asks where block_id is not null and issue_key = $1`, rig.issue).Scan(&askID) == nil
	})
	return askID
}

// aliceReads is alice's read of target, which must answer 200.
func (rig *reviewRig) aliceReads(target string) string {
	rig.t.Helper()
	response := dispatchRequest(rig.t, rig.handler, http.MethodGet, target, nil, "alice")
	if response.Code != http.StatusOK {
		rig.t.Fatalf("alice's read of %s: %d %s", target, response.Code, response.Body.String())
	}
	return response.Body.String()
}

// projectOpenAsks is the open-ask count of project RVW as alice's project list gives it.
func (rig *reviewRig) projectOpenAsks() int {
	rig.t.Helper()
	var projects []model.Project
	if err := json.Unmarshal([]byte(rig.aliceReads("/api/v1/projects")), &projects); err != nil {
		rig.t.Fatal(err)
	}
	for _, project := range projects {
		if project.Key == "RVW" {
			return project.OpenAsks
		}
	}
	rig.t.Fatal("alice's project list holds no RVW")
	return 0
}

// A held decision block shows in its document and in its issue's conversation at once, and is out
// of alice's Inbox and the project's open-ask count until its writer posts it as written; the post
// shows it and appends the quiet ask.reviewed an open Inbox refetches on.
func TestADecisionBlockHeldShowsInItsDocumentAndIsOutOfTheInboxUntilReviewed(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{DecisionBlocks: ReviewHold, WriterWindow: time.Minute}, nil)
	askID := rig.uploadDecisionBlock(false)
	if !strings.Contains(rig.aliceReads("/api/v1/issues/"+rig.issue+"/asks"), askID) {
		t.Fatal("the issue's conversation does not show the held decision block")
	}
	if !strings.Contains(rig.aliceReads("/api/v1/issues/"+rig.issue+"/artifacts/rollout.md/text"), "Ship the reviewer?") {
		t.Fatal("the document does not show the held decision block")
	}
	row := rig.waitHeld(askID, heldAwaitingWriter)
	if len(row.findings) != 1 || row.kind != "decision_block" {
		t.Fatalf("the held block: kind %s findings %+v", row.kind, row.findings)
	}
	// Each read INFRA-3552's R5 lists leaves it out: the Inbox, the project's open-ask count, and
	// the issue's own open asks (its "Waiting on you"), while the conversation shows it.
	if strings.Contains(rig.aliceReads("/api/v1/inbox"), askID) {
		t.Fatal("alice's Inbox lists the held decision block")
	}
	if open := rig.projectOpenAsks(); open != 0 {
		t.Fatalf("the project counts %d open asks while the block is held, want 0", open)
	}
	if waiting := rig.issueOpenAsks(); len(waiting) != 0 {
		t.Fatalf("the issue's open asks list %v while the block is held, want none", waiting)
	}
	if body := rig.waitFrame(askID); !strings.Contains(body, "out of its reader's Inbox") || !strings.Contains(body, "dispatch held-post "+askID) {
		t.Fatalf("the held block's frame reads %q", body)
	}
	posted := rig.asWriter(http.MethodPut, "/api/v1/held/"+askID+"/state", map[string]any{"state": "posted", "reason": "The reader asked for the question as written."})
	if posted.Code != http.StatusOK {
		t.Fatalf("the writer's post: %d %s", posted.Code, posted.Body.String())
	}
	rig.waitHeld(askID, heldPosted)
	if !strings.Contains(rig.aliceReads("/api/v1/inbox"), askID) {
		t.Fatal("alice's Inbox does not list the decision block once its hold ended")
	}
	if open := rig.projectOpenAsks(); open != 1 {
		t.Fatalf("the project counts %d open asks once the hold ended, want 1", open)
	}
	if waiting := rig.issueOpenAsks(); len(waiting) != 1 || waiting[0] != askID {
		t.Fatalf("the issue's open asks list %v once the hold ended, want the block", waiting)
	}
	if reviewed := countRows(t, rig.database, `
		select count(*) from events where type = 'ask.reviewed' and payload->>'id' = $1 and not notify`, askID); reviewed != 1 {
		t.Fatalf("%d quiet ask.reviewed events of the block, want 1", reviewed)
	}
}

// A held decision block whose writer rewords it, which starts its review and its writer's window
// again, stays out of alice's Inbox, the open asks and the project's open-ask count past the moment
// its first hold would have run out (its ask's held_until set into the past here), for as long as
// its held text is open: the hold's own state decides. Once it posts, each of them shows it.
func TestADecisionBlockHeldPastItsFirstBoundStaysOutUntilItPosts(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{DecisionBlocks: ReviewHold, WriterWindow: time.Minute}, nil)
	askID := rig.uploadDecisionBlock(false)
	rig.waitHeld(askID, heldAwaitingWriter)
	rig.rewordBlock(askID, "Ship the reviewer?", "Ship the reviewer this week?", false)
	pollUntil(t, 20*time.Second, "the reworded block's second review", func() bool {
		row := rig.heldRow(askID)
		return row.revision == 2 && row.state == heldAwaitingWriter
	})
	if _, err := rig.database.Pool.Exec(context.Background(), `
		update asks set held_until = now() - interval '1 minute' where id = $1::uuid`, askID); err != nil {
		t.Fatalf("run the block's first hold out: %v", err)
	}
	openAsks := func() []string {
		t.Helper()
		var read openAsksRead
		if err := json.Unmarshal([]byte(rig.aliceReads("/api/v1/asks/open?project=RVW")), &read); err != nil {
			t.Fatal(err)
		}
		return listedOpenAsks(t, read)
	}
	if strings.Contains(rig.aliceReads("/api/v1/inbox"), askID) {
		t.Fatal("alice's Inbox lists the reworded block while its held text is open")
	}
	if ids := openAsks(); len(ids) != 0 {
		t.Fatalf("alice's open asks list %v while the reworded block is held, want none", ids)
	}
	if open := rig.projectOpenAsks(); open != 0 {
		t.Fatalf("the project counts %d open asks while the reworded block is held, want 0", open)
	}
	rig.postHeldAsWritten(askID)
	rig.waitHeld(askID, heldPosted)
	if !strings.Contains(rig.aliceReads("/api/v1/inbox"), askID) {
		t.Fatal("alice's Inbox does not list the block once it posted")
	}
	if ids := openAsks(); len(ids) != 1 || ids[0] != askID {
		t.Fatalf("alice's open asks list %v once the block posted, want the block %s", ids, askID)
	}
}

// listedOpenAsks is the ids an answer of GET /api/v1/asks/open lists, which its counts must agree
// with.
func listedOpenAsks(t *testing.T, read openAsksRead) []string {
	t.Helper()
	ids := []string{}
	for _, ask := range read.Asks {
		ids = append(ids, ask.ID)
	}
	if read.Count != len(ids) || read.WaitingOnHuman+read.WaitingOnAgent != len(ids) {
		t.Fatalf("the open asks %v are counted %d (%d on a person, %d on an agent)",
			ids, read.Count, read.WaitingOnHuman, read.WaitingOnAgent)
	}
	return ids
}

// GET /api/v1/asks/open, which `dispatch open-asks` reads, leaves a held decision block out of its
// rows, its counts and opened_since until the block posts, in both its scopes and for every
// reader: alice's read of the project, an agent's read of it, and the writer session's read of
// its own open asks. Once the block posts, each of them lists it.
func TestADecisionBlockHeldIsOutOfTheOpenAsksUntilItPosts(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{DecisionBlocks: ReviewHold, WriterWindow: time.Minute}, nil)
	// The writer's since is the database's clock, read once before the block is written.
	var since time.Time
	if err := rig.database.Pool.QueryRow(context.Background(), `select statement_timestamp()`).Scan(&since); err != nil {
		t.Fatal(err)
	}
	const writers = "the writer's read of its own open asks"
	reads := func() map[string]openAsksRead {
		t.Helper()
		var alices openAsksRead
		if err := json.Unmarshal([]byte(rig.aliceReads("/api/v1/asks/open?project=RVW")), &alices); err != nil {
			t.Fatal(err)
		}
		return map[string]openAsksRead{
			"alice's read of the project":    alices,
			"an agent's read of the project": listProjectOpenAsks(t, rig.handler, "RVW"),
			writers:                          listOpenAsks(t, rig.handler, heldWriter, since.UTC().Format(time.RFC3339Nano)),
		}
	}
	for name, read := range reads() {
		if ids := listedOpenAsks(t, read); len(ids) != 0 {
			t.Fatalf("%s lists %v before the block, want none", name, ids)
		}
	}
	askID := rig.uploadDecisionBlock(false)
	rig.waitHeld(askID, heldAwaitingWriter)
	for name, read := range reads() {
		if ids := listedOpenAsks(t, read); len(ids) != 0 || read.OpenedSince {
			t.Fatalf("%s lists %v (opened_since %t) while the block is held, want none", name, ids, read.OpenedSince)
		}
	}
	rig.postHeldAsWritten(askID)
	rig.waitHeld(askID, heldPosted)
	posted := reads()
	for name, read := range posted {
		if ids := listedOpenAsks(t, read); len(ids) != 1 || ids[0] != askID {
			t.Fatalf("%s lists %v once the block posted, want the block %s", name, ids, askID)
		}
	}
	if !posted[writers].OpenedSince {
		t.Fatalf("%s says no ask opened since %s once the block posted", writers, since)
	}
}

// The Decisions view's open questions are what waits on its reader, which the page counts: a
// decision block the writer adds to the issue's spec is out of them while the reviewer holds it,
// though the spec shows it, and in them once it posts.
func TestADecisionBlockHeldIsOutOfTheDecisionsViewUntilItPosts(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{DecisionBlocks: ReviewHold, WriterWindow: time.Minute}, nil)
	questions := func() []string {
		t.Helper()
		ids := []string{}
		for _, question := range readIssueDecisions(t, rig.handler, rig.issue).OpenQuestions {
			ids = append(ids, question.ID)
		}
		return ids
	}
	if open := questions(); len(open) != 0 {
		t.Fatalf("the Decisions view lists %v before the block, want none", open)
	}
	block := decisionBlockDocument[strings.Index(decisionBlockDocument, ":::ask"):]
	edit := rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/artifacts/spec/edits", map[string]any{
		"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": block}},
	})
	if edit.Code != http.StatusOK {
		t.Fatalf("the writer's edit of the spec: %d %s", edit.Code, edit.Body.String())
	}
	askID := rig.blockAsk()
	rig.waitHeld(askID, heldAwaitingWriter)
	if !strings.Contains(rig.aliceReads("/api/v1/issues/"+rig.issue+"/artifacts/spec/text"), "Ship the reviewer?") {
		t.Fatal("the spec does not show the held decision block")
	}
	if open := questions(); len(open) != 0 {
		t.Fatalf("the Decisions view lists %v while the block is held, want none", open)
	}
	rig.postHeldAsWritten(askID)
	rig.waitHeld(askID, heldPosted)
	if open := questions(); len(open) != 1 || open[0] != askID {
		t.Fatalf("the Decisions view lists %v once the block posted, want the block %s", open, askID)
	}
}

// While the reviewer holds a decision block, the reads that show it say so (held_for_review): the
// issue's ask list, from which the page's tree marks what waits on its reader, and the ask's own
// read, which answers the block's row to a person and to a session alike rather than 409
// HELD_TEXT, since its document shows it. Its writer reads the held block at GET
// /api/v1/held/{id}, which a person at a browser is refused. Once the block posts, neither row
// carries the field.
func TestADecisionBlockHeldIsMarkedHeldForReviewWhereItShows(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{DecisionBlocks: ReviewHold, WriterWindow: time.Minute}, nil)
	askID := rig.uploadDecisionBlock(false)
	rig.waitHeld(askID, heldAwaitingWriter)
	type markedAsk struct {
		ID            string `json:"id"`
		HeldForReview *bool  `json:"held_for_review"`
	}
	marks := func() map[string]*bool {
		t.Helper()
		var rows []markedAsk
		if err := json.Unmarshal([]byte(rig.aliceReads("/api/v1/issues/"+rig.issue+"/asks")), &rows); err != nil {
			t.Fatal(err)
		}
		marks := map[string]*bool{}
		for _, row := range rows {
			if row.ID == askID {
				marks["the issue's ask list"] = row.HeldForReview
			}
		}
		reads := map[string]*httptest.ResponseRecorder{
			"alice's read of the ask":     dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/asks/"+askID, nil, "alice"),
			"a session's read of the ask": sessionRequest(t, rig.handler, http.MethodGet, "/api/v1/asks/"+askID, nil),
		}
		for name, response := range reads {
			if response.Code != http.StatusOK {
				t.Fatalf("%s: %d %s, want the block's row", name, response.Code, response.Body.String())
			}
			var read struct {
				Ask markedAsk `json:"ask"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &read); err != nil || read.Ask.ID != askID {
				t.Fatalf("%s answers %s (%v), want the block %s", name, response.Body.String(), err, askID)
			}
			marks[name] = read.Ask.HeldForReview
		}
		if len(marks) != 3 {
			t.Fatalf("the reads that show the block are %v, want the ask list and both reads of the ask", marks)
		}
		return marks
	}
	for name, mark := range marks() {
		if mark == nil || !*mark {
			t.Fatalf("%s does not mark the block held for review while the reviewer holds it", name)
		}
	}

	held := rig.asWriter(http.MethodGet, "/api/v1/held/"+askID, nil)
	if held.Code != http.StatusOK {
		t.Fatalf("the writer's read of its held block: %d %s", held.Code, held.Body.String())
	}
	var read struct {
		ID       string            `json:"id"`
		Kind     string            `json:"kind"`
		State    string            `json:"state"`
		Writer   string            `json:"writer"`
		Findings []json.RawMessage `json:"findings"`
	}
	if err := json.Unmarshal(held.Body.Bytes(), &read); err != nil {
		t.Fatal(err)
	}
	if read.ID != askID || read.Kind != "decision_block" || read.State != heldAwaitingWriter || read.Writer != heldWriter ||
		len(read.Findings) != 1 || !strings.Contains(held.Body.String(), "Ship the reviewer?") {
		t.Fatalf("the writer reads its held block as %s", held.Body.String())
	}
	if refused := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/held/"+askID, nil, "alice"); refused.Code != http.StatusForbidden {
		t.Fatalf("alice's read of the held block at /held: %d %s, want 403", refused.Code, refused.Body.String())
	}

	rig.postHeldAsWritten(askID)
	rig.waitHeld(askID, heldPosted)
	for name, mark := range marks() {
		if mark != nil {
			t.Fatalf("%s carries held_for_review %t once the block posted, want it absent", name, *mark)
		}
	}
}

// A held decision block its writer leaves past the window posts the reviewer's rewrite through the
// one decision-block rewrite path: a new version of its document holds the rewrite, and its
// ask.edited holds the original wording, the reviewer as who asked, and its findings as why.
func TestADecisionBlockLeftToTheWindowPostsTheRewriteAsANewVersion(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{DecisionBlocks: ReviewHold, WriterWindow: 300 * time.Millisecond, Unedited: UneditedRewrite}, nil)
	askID := rig.uploadDecisionBlock(false)
	rig.waitHeld(askID, heldAwaitingWriter)
	versionsBefore := countRows(t, rig.database, `select count(*) from artifact_versions v join artifacts a on a.id = v.artifact_id where a.name = 'rollout.md'`)
	time.Sleep(400 * time.Millisecond)
	rig.served.sweepHeld(t.Context())
	if posted := rig.waitHeld(askID, heldPosted); posted.outcome != reviewRewritePosted {
		t.Fatalf("the block posted %s (%s), want rewrite_posted", posted.outcome, posted.detail)
	}
	var question string
	if err := rig.database.Pool.QueryRow(context.Background(), `select question from asks where id = $1::uuid`, askID).Scan(&question); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(question, vacuousPhrase) || !strings.HasPrefix(question, "Ship the reviewer?") {
		t.Fatalf("the rewritten block asks %q", question)
	}
	var edited struct {
		Previous model.AskEditPrevious `json:"previous"`
		Rewrite  *model.AskRewriteEdit `json:"rewrite"`
	}
	var payload []byte
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select payload from events where type = 'ask.edited' and payload->>'id' = $1 order by id desc limit 1`, askID).Scan(&payload); err != nil {
		t.Fatalf("read the rewrite's ask.edited: %v", err)
	}
	if err := json.Unmarshal(payload, &edited); err != nil {
		t.Fatal(err)
	}
	if edited.Rewrite == nil || edited.Rewrite.By != model.RewriteByReviewer || edited.Rewrite.AskedBy != reviewerActor ||
		!strings.Contains(edited.Rewrite.Reason, vacuousPhrase) || !strings.Contains(edited.Previous.Question, vacuousPhrase) {
		t.Fatalf("the rewrite's ask.edited holds previous %q rewrite %+v", edited.Previous.Question, edited.Rewrite)
	}
	pollUntil(t, 20*time.Second, "a new version of the document holding the rewrite", func() bool {
		var markdown string
		err := rig.database.Pool.QueryRow(context.Background(), `
			select coalesce(v.markdown, '') from artifact_versions v join artifacts a on a.id = v.artifact_id
			where a.name = 'rollout.md' order by v.number desc limit 1`).Scan(&markdown)
		return err == nil && !strings.Contains(markdown, vacuousPhrase) &&
			countRows(t, rig.database, `select count(*) from artifact_versions v join artifacts a on a.id = v.artifact_id where a.name = 'rollout.md'`) > versionsBefore
	})
	if !strings.Contains(rig.aliceReads("/api/v1/inbox"), askID) {
		t.Fatal("alice's Inbox does not list the rewritten block")
	}
}

// A held decision block whose question holds a picture, left to the window, where the reviewer's
// rewrite writes the picture as a bare address: the block cannot hold that rewrite, so the
// decision posts the block as written, saying why, and records posted_as_written rather than
// rewrite_posted, which R8's counts read. The document keeps the picture and the ask its question.
func TestADecisionBlockWhoseRewriteDropsItsPicturePostsAsWritten(t *testing.T) {
	const picture = "![the wait](https://ci.test/chart.png)"
	isReview := func(request modeltest.Request) bool { return strings.Contains(request.Text, reviewMarker) }
	stand := modeltest.New(t,
		modeltest.Rule{
			Match: func(request modeltest.Request) bool {
				return isReview(request) && strings.Contains(reviewedText(request), vacuousPhrase)
			},
			Reply: modeltest.Reply{Text: standInReview(t, vacuousPhrase, map[string]any{
				"body": "Ship the reviewer? https://ci.test/chart.png The test of the held ask fails on its first call.",
				"options": []map[string]string{
					{"label": "Ship it", "description": "Release it today, with the failing test."},
					{"label": "Do not file", "description": "Wait until the test passes."},
				},
				"multiple": false,
			})},
		},
		modeltest.Rule{Match: isReview, Reply: modeltest.Reply{Text: noFindings}},
	)
	rig := newReviewRig(t, ReviewerConfig{DecisionBlocks: ReviewHold, WriterWindow: 300 * time.Millisecond, Unedited: UneditedRewrite}, stand)
	askID := rig.uploadDecisionBlockOf("# Rollout\n\nThe plan for the rollout.\n\n:::ask{urgency=\"med\"}\nShip the reviewer? "+picture+"\n"+
		vacuousPhrase+"\n\n- Ship it: Release it today.\n- Do not file: Wait.\n:::\n", false)
	if held := rig.waitHeld(askID, heldAwaitingWriter); held.rewrite == nil || held.notApplied != "" {
		t.Fatalf("the review gave rewrite %+v not applied %q, want a rewrite that keeps what a rewrite keeps", held.rewrite, held.notApplied)
	}
	time.Sleep(400 * time.Millisecond)
	rig.served.sweepHeld(t.Context())
	posted := rig.waitHeld(askID, heldPosted)
	if posted.outcome != reviewPostedAsWritten || !strings.Contains(posted.detail, `it drops the picture "https://ci.test/chart.png"`) {
		t.Fatalf("the block posted %s (%s), want posted_as_written naming the picture", posted.outcome, posted.detail)
	}
	for outcome, want := range map[string]int64{reviewRewritePosted: 0, reviewPostedAsWritten: 1} {
		if got := countRows(t, rig.database, `select count(*) from writing_check_events where outcome = $1`, outcome); got != want {
			t.Fatalf("%d writing_check_events rows record %s, want %d", got, outcome, want)
		}
	}
	var question string
	if err := rig.database.Pool.QueryRow(context.Background(), `select question from asks where id = $1::uuid`, askID).Scan(&question); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(question, vacuousPhrase) {
		t.Fatalf("the block asks %q, want its writer's question", question)
	}
	var markdown string
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select coalesce(v.markdown, '') from artifact_versions v join artifacts a on a.id = v.artifact_id
		where a.name = 'rollout.md' order by v.number desc limit 1`).Scan(&markdown); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(markdown, picture) {
		t.Fatalf("the document lost its picture:\n%s", markdown)
	}
}

// A held decision block whose document cannot hold the reviewer's rewrite at its post - the
// rewrite would grow the document past the 1 MiB one upload may hold, the document's own refusal,
// which no later post clears - posts as its writer wrote it rather than failing (releaseDecisionBlock,
// rewriteRefusal): the held text posts and the block keeps its writer's question.
func TestADecisionBlockTheDocumentRefusesToRewritePostsAsWritten(t *testing.T) {
	longer := "Ship the reviewer? " + strings.Repeat("The test of the held ask fails on its first call. ", 100)
	isReview := func(request modeltest.Request) bool { return strings.Contains(request.Text, reviewMarker) }
	stand := modeltest.New(t,
		modeltest.Rule{
			Match: func(request modeltest.Request) bool {
				return isReview(request) && strings.Contains(reviewedText(request), vacuousPhrase)
			},
			Reply: modeltest.Reply{Text: standInReview(t, vacuousPhrase, map[string]any{
				"body": longer,
				"options": []map[string]string{
					{"label": "Ship it", "description": "Release it today."}, {"label": "Do not file", "description": "Wait."},
				},
				"multiple": false,
			})},
		},
		modeltest.Rule{Match: isReview, Reply: modeltest.Reply{Text: noFindings}},
	)
	rig := newReviewRig(t, ReviewerConfig{DecisionBlocks: ReviewHold, WriterWindow: 300 * time.Millisecond, Unedited: UneditedRewrite}, stand)
	// The document leaves 1,500 bytes under the cap, which the block's settlement takes a little
	// of, and the rewrite adds about 4,900.
	padding := strings.TrimSuffix(strings.Repeat("word ", (pmdoc.MaxDocumentBytes-len(decisionBlockDocument)-1500)/len("word ")), " ")
	askID := rig.uploadDecisionBlockOf(decisionBlockDocument+"\n"+padding+"\n", false)
	if held := rig.waitHeld(askID, heldAwaitingWriter); held.rewrite == nil || held.notApplied != "" {
		t.Fatalf("the review gave rewrite %+v not applied %q, want a rewrite that keeps what a rewrite keeps", held.rewrite, held.notApplied)
	}
	time.Sleep(400 * time.Millisecond)
	rig.served.sweepHeld(t.Context())
	if decided := rig.waitDecided(askID); decided.state != heldPosted {
		t.Fatalf("the block is %s (%s: %s), want it posted as its writer wrote it", decided.state, decided.outcome, decided.detail)
	}
	var question string
	if err := rig.database.Pool.QueryRow(context.Background(), `select question from asks where id = $1::uuid`, askID).Scan(&question); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(question, vacuousPhrase) {
		t.Fatalf("the block asks %q, want its writer's question", question)
	}
}

// A decision block a person typed in a session (the document write carried
// Dispatch-Person-Typed: 1) opens unheld and unreviewed under hold, recorded exempt_person_typed,
// and its ask.opened is marked person_typed.
func TestAPersonTypedDecisionBlockOpensUnheld(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{DecisionBlocks: ReviewHold, WriterWindow: time.Minute}, nil)
	askID := rig.uploadDecisionBlock(true)
	if held := countRows(t, rig.database, `select count(*) from held_texts where id = $1`, askID); held != 0 {
		t.Fatal("the typed decision block is held")
	}
	if !strings.Contains(rig.aliceReads("/api/v1/inbox"), askID) {
		t.Fatal("alice's Inbox does not list the typed decision block")
	}
	if marked := countRows(t, rig.database, `
		select count(*) from events where type = 'ask.opened' and payload->>'id' = $1 and (payload->>'person_typed')::boolean`, askID); marked != 1 {
		t.Fatalf("%d ask.opened events of the typed block carry person_typed, want 1", marked)
	}
	pollUntil(t, 10*time.Second, "the typed block's exempt_person_typed row", func() bool {
		return countRows(t, rig.database, `
			select count(*) from writing_check_events where outcome = 'exempt_person_typed' and route = 'decision block'`) == 1
	})
	rig.served.detached.wait()
	if calls := rig.reviewCalls("Ship the reviewer?"); calls != 0 {
		t.Fatalf("the reviewer read the typed decision block %d times", calls)
	}
}

// issueOpenAsks is the ids of the issue's open asks as alice's read of the issue gives them: its
// "Waiting on you".
func (rig *reviewRig) issueOpenAsks() []string {
	rig.t.Helper()
	var detail struct {
		OpenAsks []struct {
			ID string `json:"id"`
		} `json:"open_asks"`
	}
	if err := json.Unmarshal([]byte(rig.aliceReads("/api/v1/issues/"+rig.issue)), &detail); err != nil {
		rig.t.Fatal(err)
	}
	ids := []string{}
	for _, ask := range detail.OpenAsks {
		ids = append(ids, ask.ID)
	}
	return ids
}

// rewordBlock replaces from with to in the decision block's document as the writer session, the
// person-typed header set where typed says so, and waits for the settlement that rewords its ask.
func (rig *reviewRig) rewordBlock(askID, from, to string, typed bool) {
	rig.t.Helper()
	body, err := json.Marshal(map[string]any{
		"ops":   []map[string]string{{"op": "replace", "find": from, "with": to}},
		"actor": map[string]any{"kind": "session", "id": heldWriter},
	})
	if err != nil {
		rig.t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/issues/"+rig.issue+"/artifacts/rollout.md/edits", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer agent-token")
	if typed {
		request.Header.Set(contracts.PersonTypedHeader, "1")
	}
	response := httptest.NewRecorder()
	rig.handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		rig.t.Fatalf("the edit of the decision block: %d %s", response.Code, response.Body.String())
	}
	pollUntil(rig.t, 20*time.Second, "the settlement that rewords the block", func() bool {
		var question string
		err := rig.database.Pool.QueryRow(context.Background(), `select question from asks where id = $1::uuid`, askID).Scan(&question)
		return err == nil && strings.Contains(question, to)
	})
}

// With DISPATCH_REVIEWER_DECISION_BLOCKS=after a decision block shows at once, in its document and
// in alice's Inbox, and is reviewed after its settlement: its findings reach the writer, recorded
// reviewed_after, and nothing is held. An agent's later rewording is reviewed after it in the same
// way, with no hold; a rewording a person typed in a session is reviewed by nobody, recorded
// exempt_person_typed, and its ask.edited is marked person_typed.
func TestADecisionBlockUnderAfterIsReviewedAfterItsSettlementAndRewordings(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{DecisionBlocks: ReviewAfter, WriterWindow: time.Minute}, nil)
	askID := rig.uploadDecisionBlock(false)
	if !strings.Contains(rig.aliceReads("/api/v1/inbox"), askID) {
		t.Fatal("alice's Inbox does not list the decision block at once under after")
	}
	if body := rig.waitFrame(askID); !strings.Contains(body, "Dispatch posted your decision block") || !strings.Contains(body, vacuousPhrase) {
		t.Fatalf("the block's frame reads %q", body)
	}
	pollUntil(t, 10*time.Second, "the block's reviewed_after row", func() bool { return rig.outcomeCount(reviewReviewedAfter) == 1 })
	if held := countRows(t, rig.database, `select count(*) from held_texts`); held != 0 {
		t.Fatalf("%d held texts under after", held)
	}

	// The agent rewords the block in its document: reviewed after the settlement, with no hold.
	rig.rewordBlock(askID, "Ship the reviewer?", "Ship the reviewer now?", false)
	pollUntil(t, 20*time.Second, "the rewording's review", func() bool {
		return rig.reviewCalls("Ship the reviewer now?") == 1 && rig.outcomeCount(reviewReviewedAfter) == 2
	})
	if !strings.Contains(rig.aliceReads("/api/v1/inbox"), askID) {
		t.Fatal("alice's Inbox lost the block after its rewording")
	}

	// The writer's person types a rewording in the session: no review, recorded apart.
	rig.rewordBlock(askID, "Ship the reviewer now?", "Ship the reviewer this week?", true)
	pollUntil(t, 10*time.Second, "the typed rewording's exempt_person_typed row", func() bool {
		return countRows(t, rig.database, `
			select count(*) from writing_check_events where outcome = 'exempt_person_typed' and route = 'decision block'`) == 1
	})
	if marked := countRows(t, rig.database, `
		select count(*) from events where type = 'ask.edited' and payload->>'id' = $1
			and (payload->>'person_typed')::boolean and payload->>'question' like '%this week%'`, askID); marked != 1 {
		t.Fatalf("%d ask.edited events of the typed rewording carry person_typed, want 1", marked)
	}
	rig.served.detached.wait()
	if calls := rig.reviewCalls("Ship the reviewer this week?"); calls != 0 {
		t.Fatalf("the reviewer read the typed rewording %d times", calls)
	}
}

// A held decision block that the writer's person rewords by typing in the session ends its hold
// with the block as they left it: posted as written, shown in alice's Inbox, and never rewritten
// over by the reviewer.
func TestAPersonTypedRewordingEndsADecisionBlocksHold(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{DecisionBlocks: ReviewHold, WriterWindow: time.Hour, Unedited: UneditedRewrite}, nil)
	askID := rig.uploadDecisionBlock(false)
	rig.waitHeld(askID, heldAwaitingWriter)
	rig.rewordBlock(askID, "Ship the reviewer?", "Ship the reviewer this week?", true)
	posted := rig.waitHeld(askID, heldPosted)
	if posted.outcome != reviewPostedAsWritten {
		t.Fatalf("the typed rewording left the block %s, want posted_as_written", posted.outcome)
	}
	var question string
	if err := rig.database.Pool.QueryRow(context.Background(), `select question from asks where id = $1::uuid`, askID).Scan(&question); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(question, "this week") {
		t.Fatalf("the block asks %q, want the person's wording", question)
	}
	if !strings.Contains(rig.aliceReads("/api/v1/inbox"), askID) {
		t.Fatal("alice's Inbox does not list the block once the person's edit ended its hold")
	}
}
