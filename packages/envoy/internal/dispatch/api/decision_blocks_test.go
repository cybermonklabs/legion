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
)

// A decision block an agent writes in a document (INFRA-3231's R5, Decisions 3 and 23): with
// DISPATCH_REVIEWER_DECISION_BLOCKS=hold it shows in its document at once and stays out of its
// reader's Inbox and the open-ask counts until its review ends; its writer gets the findings; its
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
	time.Sleep(300 * time.Millisecond)
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
	time.Sleep(300 * time.Millisecond)
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
