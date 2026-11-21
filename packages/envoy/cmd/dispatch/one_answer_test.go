//go:build basebuild

// The basebuild tag, and how a test of it is run: store_census_test.go. This file's tests read the
// base release the Verification document's preamble builds once per head (baseRelease), and are
// run as
//
//	go test -tags basebuild -timeout 30m ./cmd/dispatch -run '^TestAnAnswerIsHeldOnceOnTheAskNode$' -count=1 -v

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// storedAskAttributes are the seven server-owned attributes of an ask block a version can store:
// the eighth, invalid, never reaches stored bytes (pmdoc's typedAttributeValues).
var storedAskAttributes = []string{"state", "answered_by", "answered_at", "selected", "answer", "copied_from", "copied_from_document"}

// answerAttributes are the four of them that hold an answer: the words, the selection, who and
// when.
var answerAttributes = []string{"answer", "selected", "answered_by", "answered_at"}

// oneAnswerSettleDelay is how long the processes here wait after an edit before they settle it:
// long enough that the pending settlement an answer arms on a copy's document is there to read
// once the answer has answered, short enough that waiting it out costs the run nothing.
const oneAnswerSettleDelay = 300 * time.Millisecond

// announcedBaseCommit waits for the base release serving the database alone to announce its build
// and answers the commit it announced, which is not this head's stamp.
func announcedBaseCommit(t *testing.T, ctx context.Context, database *pgx.Conn) string {
	t.Helper()
	var commit string
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if err := database.QueryRow(ctx, `select coalesce((select commit from server_build_announcements order by seq desc limit 1), '')`).Scan(&commit); err != nil {
			t.Fatalf("read the base release's announcement: %v", err)
		}
		if commit != "" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if commit == "" || commit == headBuildStamp {
		t.Fatalf("the base release announced the build %q, want its own stamp, which is not this head's %q", commit, headBuildStamp)
	}
	return commit
}

// stopProcess stops p in order (SIGTERM) and waits for it to exit.
func stopProcess(t *testing.T, p *dispatchProcess) {
	t.Helper()
	p.Terminate(t)
	p.WaitExit(t, "SIGTERM")
}

// readJSON reads path from p as the test's person and decodes it into into.
func readJSON(t *testing.T, p *dispatchProcess, path string, into any) {
	t.Helper()
	answer := p.call(t, http.MethodGet, path, "", http.StatusOK)
	if err := json.Unmarshal(answer, into); err != nil {
		t.Fatalf("decode GET %s: %v: %s", path, err, answer)
	}
}

// encodeJSON is value as a request body.
func encodeJSON(t *testing.T, value any) string {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode a request body: %v", err)
	}
	return string(body)
}

// blockAskOf waits for the ask settlement indexed from the ask block blockID of the issue's
// documents, and answers its id.
func blockAskOf(t *testing.T, p *dispatchProcess, issueKey, blockID string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var listed []struct {
			ID      string  `json:"id"`
			BlockID *string `json:"block_id"`
		}
		readJSON(t, p, "/api/v1/issues/"+issueKey+"/asks?state=all", &listed)
		for _, ask := range listed {
			if ask.BlockID != nil && *ask.BlockID == blockID {
				return ask.ID
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no ask was indexed from the block %s of %s within 30s: %+v", blockID, issueKey, listed)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// editAsBrowser opens a browser's tab on the document, signed in as the test's person, waits for
// the room to send it a document holding holding, and makes change on its tree in one
// transaction, which the tab sends the room as a browser's own update: a write no route refuses.
func editAsBrowser(t *testing.T, p *dispatchProcess, artifactID, holding string, change func(*pmdoc.Node) error) {
	t.Helper()
	editor := docstest.Dial(t, p.documentURL(artifactID), http.Header{"X-Dispatch-User": []string{dispatchTestLogin}}, artifactID, crdt.New())
	defer editor.Close()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(markdown(editor), holding) {
		if time.Now().After(deadline) {
			t.Fatalf("the tab never read a document holding %q:\n%s", holding, markdown(editor))
		}
		time.Sleep(50 * time.Millisecond)
	}
	fragment := editor.Doc.GetXmlFragment("prosemirror")
	var changeErr error
	if _, err := editor.Send(func(txn *crdt.Transaction) {
		tree, err := pmdoc.ReadInTransaction(txn, fragment)
		if err != nil {
			changeErr = err
			return
		}
		if changeErr = change(tree); changeErr == nil {
			changeErr = pmdoc.Update(txn, fragment, tree)
		}
	}); err != nil || changeErr != nil {
		t.Fatalf("the browser's edit: %v %v", err, changeErr)
	}
}

// settledDocument closes every live document of the process, which settles what each owes, and
// waits until the document owes no settlement.
func settledDocument(t *testing.T, ctx context.Context, p *dispatchProcess, database *pgx.Conn, artifactID string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		time.Sleep(2 * oneAnswerSettleDelay)
		p.call(t, http.MethodPost, "/api/v1/artifacts/_test/quiesce", "", http.StatusOK)
		var owed bool
		if err := database.QueryRow(ctx, `select exists (select 1 from doc_settlements_pending where artifact_id = $1::uuid)`, artifactID).Scan(&owed); err != nil {
			t.Fatalf("read whether %s owes a settlement: %v", artifactID, err)
		}
		if !owed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s still owes a settlement after 30s", artifactID)
		}
	}
}

// latestMarkdown is the newest version of the document, its number and its stored text, as the
// process's API reads them (GET /api/v1/artifacts/{id} and its version route).
func latestMarkdown(t *testing.T, p *dispatchProcess, artifactID string) (int, string) {
	t.Helper()
	var artifact struct {
		Versions []struct {
			Number int `json:"number"`
		} `json:"versions"`
	}
	readJSON(t, p, "/api/v1/artifacts/"+artifactID, &artifact)
	number := 0
	for _, version := range artifact.Versions {
		number = max(number, version.Number)
	}
	var version struct {
		Markdown string `json:"markdown"`
	}
	readJSON(t, p, "/api/v1/artifacts/"+artifactID+"/versions/"+strconv.Itoa(number), &version)
	return number, version.Markdown
}

// liveAskAttributesOf is what each ask block of the document holds in the process's live tree, by
// block id (GET /api/v1/artifacts/{id}/_test/ask-attributes, docs' AskBlockAttributes).
func liveAskAttributesOf(t *testing.T, p *dispatchProcess, artifactID string) map[string]map[string]any {
	t.Helper()
	var live struct {
		Blocks map[string]map[string]any `json:"blocks"`
	}
	readJSON(t, p, "/api/v1/artifacts/"+artifactID+"/_test/ask-attributes", &live)
	return live.Blocks
}

// storedAskBlock is the ask block blockID of markdown, a stored version, as this build reads it.
func storedAskBlock(t *testing.T, markdown, blockID string) *pmdoc.Node {
	t.Helper()
	tree, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		t.Fatalf("read a stored version: %v\n%s", err, markdown)
	}
	var found *pmdoc.Node
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if id, _ := node.Attrs[pmdoc.BlockIDAttr].(string); node.Type == "ask" && id == blockID {
			found = node
		}
		return found == nil
	})
	if found == nil {
		t.Fatalf("the stored version holds no ask block %q:\n%s", blockID, markdown)
	}
	return found
}

// documentApproval is the approval GET /api/v1/artifacts/{id} answers, the read the daemon's design
// gate makes (HTTPClient.Approval).
type documentApproval struct {
	State         string `json:"state"`
	Version       *int   `json:"version"`
	LatestVersion int    `json:"latest_version"`
	Reason        string `json:"reason"`
}

// approvedLatest reports whether the approval stands at the document's latest version.
func (a documentApproval) approvedLatest() bool {
	return a.State == "approved" && a.Version != nil && *a.Version == a.LatestVersion
}

func approvalOf(t *testing.T, p *dispatchProcess, artifactID string) documentApproval {
	t.Helper()
	var artifact struct {
		Approval documentApproval `json:"approval"`
	}
	readJSON(t, p, "/api/v1/artifacts/"+artifactID, &artifact)
	return artifact.Approval
}

// approvalState is what an approval reads as, the state at the version it names against the
// document's latest version, for two reads to be compared and a failure to print.
func approvalState(a documentApproval) string {
	version := "none"
	if a.Version != nil {
		version = strconv.Itoa(*a.Version)
	}
	return fmt.Sprintf("%s at version %s of %d", a.State, version, a.LatestVersion)
}

// standingReview is the newest review GET /api/v1/artifacts/{id}/reviews lists: which review it
// is, its state and the version it names.
type standingReview struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Version int    `json:"version"`
}

func standingReviewOf(t *testing.T, p *dispatchProcess, artifactID string) standingReview {
	t.Helper()
	var reviews []standingReview
	readJSON(t, p, "/api/v1/artifacts/"+artifactID+"/reviews", &reviews)
	if len(reviews) == 0 {
		t.Fatalf("document %s lists no review", artifactID)
	}
	return reviews[0]
}

// answerAsk answers the ask as the test's person through the answer route and answers the body.
func answerAsk(t *testing.T, p *dispatchProcess, askID, selected, text string) []byte {
	t.Helper()
	return p.call(t, http.MethodPost, "/api/v1/asks/"+askID+"/answer",
		encodeJSON(t, map[string]any{"selected": []string{selected}, "text": text, "expected_edited_at": nil}), http.StatusOK)
}

// answeredEvent is the newest ask.answered of the ask.
func answeredEvent(t *testing.T, ctx context.Context, database *pgx.Conn, askID string) int64 {
	t.Helper()
	var event int64
	if err := database.QueryRow(ctx, `select coalesce(max(id), 0) from events where type = 'ask.answered' and payload->>'id' = $1`, askID).Scan(&event); err != nil || event == 0 {
		t.Fatalf("read the ask.answered of ask %s: %d, %v", askID, event, err)
	}
	return event
}

// askNodeHolds is what the node of the ask holds now of the answer: each kind asks.Settle states,
// the newest tuple of each value and each far end of each relation that still asserts.
func askNodeHolds(t *testing.T, ctx context.Context, database *pgx.Conn, askID string) map[string]graphstore.Tuple {
	t.Helper()
	rows, err := database.Query(ctx, `
		select kind, to_kind, to_id, source_event from (
			select distinct on (kind, case when to_kind = 'value' then '' else to_id end)
			       kind, to_kind, to_id, source_event, asserted
			from graph_tuples where from_kind = 'ask' and from_id = $1 and kind = any($2)
			order by kind, case when to_kind = 'value' then '' else to_id end, seq desc) newest
		where asserted
	`, askID, append(asks.AnswerKinds(), "invalid"))
	if err != nil {
		t.Fatalf("read the node of ask %s: %v", askID, err)
	}
	defer rows.Close()
	held := map[string]graphstore.Tuple{}
	for rows.Next() {
		var tuple graphstore.Tuple
		if err := rows.Scan(&tuple.Kind, &tuple.ToKind, &tuple.ToID, &tuple.SourceEvent); err != nil {
			t.Fatalf("read a tuple of the node of ask %s: %v", askID, err)
		}
		held[tuple.Kind] = tuple
	}
	return held
}

// insertAskBlock has the test's agent append an ask block to the document, an edit the document
// versions at once, and waits for the settlement that indexes it; it answers the ask's id.
func insertAskBlock(t *testing.T, p *dispatchProcess, issueKey, artifactID, blockID, question string) string {
	t.Helper()
	block := ":::ask{#" + blockID + " urgency=\"med\" multiple=\"false\"}\n" + question + "\n\n- Ship: Now.\n- Hold: Later.\n:::\n"
	p.call(t, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits",
		encodeJSON(t, map[string]any{"ops": []map[string]string{{"op": "insert", "after": "end", "markdown": block}}}), http.StatusOK)
	return blockAskOf(t, p, issueKey, blockID)
}

// oneAnswerSpec is the fixture spec: prose, the ask a person answers, and the ask the second
// document copies.
const oneAnswerSpec = "# One answer\n\nThe service ships behind a flag.\n\n" +
	":::ask{#own urgency=\"high\" multiple=\"false\"}\nShip the flag first?\n\n- Ship: Now.\n- Hold: Later.\n:::\n\n" +
	":::ask{#source urgency=\"low\" multiple=\"false\"}\nWhich region goes first?\n\n- East: The quiet one.\n- West: The busy one.\n:::\n"

// requireAnswerHeldOnce is the check's case over one open ask of a document: a person answers it
// through the answer route, and the answer is on the ask's node, each fact under the ask.answered
// that caused it, and on the asks row; the document's stored bytes and its newest version do not
// move; the block holds no answer, no answerer, no time and no selection, in its stored bytes and
// in the live tree; and an approval recorded before the answer still stands after it: GET
// /api/v1/artifacts/{id} reads the same approval at the same version, the document's latest, and
// the newest review is the same review of the same version. The answer route's reply is the ask
// alone and carries no write advice, so the approval is read itself rather than through a member
// the route never sets.
func requireAnswerHeldOnce(t *testing.T, ctx context.Context, p *dispatchProcess, database *pgx.Conn, artifactID, askID, blockID, what string) {
	t.Helper()
	settledDocument(t, ctx, p, database, artifactID)
	beforeNumber, before := latestMarkdown(t, p, artifactID)
	if approval := approvalOf(t, p, artifactID); !approval.approvedLatest() {
		p.call(t, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/reviews", `{"state":"approved"}`, http.StatusCreated)
	}
	approvedBefore := approvalOf(t, p, artifactID)
	if !approvedBefore.approvedLatest() {
		t.Fatalf("%s: the document reads %s before the answer, want it approved at its latest version", what, approvalState(approvedBefore))
	}
	reviewBefore := standingReviewOf(t, p, artifactID)

	const text = "Ship it once the flag is on."
	answerAsk(t, p, askID, "Ship", text)
	event := answeredEvent(t, ctx, database, askID)
	var state string
	var row []byte
	if err := database.QueryRow(ctx, `select state, answer from asks where id = $1::uuid`, askID).Scan(&state, &row); err != nil {
		t.Fatalf("%s: read the asks row: %v", what, err)
	}
	var answer struct {
		User     string    `json:"user"`
		Selected []string  `json:"selected"`
		Text     *string   `json:"text"`
		At       time.Time `json:"at"`
	}
	if err := json.Unmarshal(row, &answer); err != nil {
		t.Fatalf("%s: decode the row's answer %s: %v", what, row, err)
	}
	held := askNodeHolds(t, ctx, database, askID)
	want := map[string]string{
		graphstore.TupleAnswerState: asks.StateAnswered,
		graphstore.TupleAnswer:      text,
		graphstore.TupleSelected:    asks.SelectedValue([]string{"Ship"}),
		graphstore.TupleAnsweredAt:  asks.AnsweredAtValue(answer.At),
		graphstore.TupleAnsweredBy:  dispatchTestLogin,
	}
	for kind, value := range want {
		got, ok := held[kind]
		if !ok || got.ToID != value || got.SourceEvent != event {
			t.Errorf("%s: the ask's node holds %s %q under event %d (held %t), want %q under the ask.answered %d",
				what, kind, got.ToID, got.SourceEvent, ok, value, event)
		}
	}
	t.Logf("%s: the ask's node holds the answer under the ask.answered %d: %v", what, event, held)
	if state != asks.StateAnswered || answer.Text == nil || *answer.Text != text || !slices.Equal(answer.Selected, []string{"Ship"}) ||
		answer.User != dispatchTestLogin || asks.AnsweredAtValue(answer.At) != want[graphstore.TupleAnsweredAt] {
		t.Errorf("%s: the asks row reads state %q and answer %s, want the values the node holds", what, state, row)
	}

	settledDocument(t, ctx, p, database, artifactID)
	afterNumber, after := latestMarkdown(t, p, artifactID)
	if afterNumber != beforeNumber || after != before {
		t.Errorf("%s: the answer moved the document from version %d to %d:\nbefore:\n%s\nafter:\n%s", what, beforeNumber, afterNumber, before, after)
	}
	stored := storedAskBlock(t, after, blockID)
	live := liveAskAttributesOf(t, p, artifactID)[blockID]
	for _, name := range answerAttributes {
		if value, ok := stored.Attrs[name]; ok {
			t.Errorf("%s: the stored block holds %s=%v, want no answer, answerer, time or selection", what, name, value)
		}
		if value, ok := live[name]; ok {
			t.Errorf("%s: the live tree's block holds %s=%v, want no answer, answerer, time or selection", what, name, value)
		}
	}
	approvedAfter, reviewAfter := approvalOf(t, p, artifactID), standingReviewOf(t, p, artifactID)
	if !approvedAfter.approvedLatest() || approvalState(approvedAfter) != approvalState(approvedBefore) || afterNumber != approvedAfter.LatestVersion ||
		reviewAfter != reviewBefore {
		t.Errorf("%s: the approval moved across the answer: the document read %s before it and %s after, and its newest review %+v before and %+v after; want both unchanged, approved at version %d",
			what, approvalState(approvedBefore), approvalState(approvedAfter), reviewBefore, reviewAfter, beforeNumber)
	}
	t.Logf("%s: the document reads %s before and after the answer, its newest review %+v", what, approvalState(approvedAfter), reviewAfter)
}

// A person's answer is held once, on the ask's node (c-one-answer). Over
// a fleet of this build alone the answer route appends the answer to the node, the asks row is
// its projection, and the document's bytes do not move, so an approval stands through it. While
// a process of the base release is registered, the roll window keeps the block in step for that
// build's pages; once it is gone the helper writes nothing. A block that fails the strict read and
// claims an answer puts none on the node, a copy is one copied_from tuple and no attribute, and the
// whole case holds over asks the base release wrote and this head's boot step carried forward.
// Last, the source of docs and pmdoc declares neither deleted writer.
func TestAnAnswerIsHeldOnceOnTheAskNode(t *testing.T) {
	ctx := context.Background()
	databaseURL := censusDatabase(t)
	base := baseRelease(t)
	head := cmdtest.BuildStamped(t, "envoy-dispatch", headBuildStamp)
	database, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect to the run's database: %v", err)
	}
	defer database.Close(ctx)

	// The base release writes two asks before the upgrade, and the test captures them.
	previous := startDispatchProcessWith(t, databaseURL, dispatchOptions{Binary: base, SigningKey: previousBuildSigningKey, SettleDelay: oneAnswerSettleDelay})
	previous.waitHealthy(t)
	baseCommit := announcedBaseCommit(t, ctx, database)
	previous.call(t, http.MethodPost, "/api/v1/projects", `{"key":"ONE","name":"One answer"}`, http.StatusCreated)
	carriedIssue, carriedSpec := createCensusIssue(t, previous, "ONE", "Asks the base release wrote",
		"# Carried\n\n:::ask{#carried urgency=\"med\" multiple=\"false\"}\nCarry the ask forward?\n\n- Ship: Now.\n- Hold: Later.\n:::\n\n"+carriedSourceBlock)
	carriedAsk := blockAskOf(t, previous, carriedIssue, "carried")
	carriedSource := blockAskOf(t, previous, carriedIssue, "carried-source")
	_, capturedMarkdown := latestMarkdown(t, previous, carriedSpec)
	type capturedAsk struct{ state, question string }
	captured := map[string]capturedAsk{}
	for _, id := range []string{carriedAsk, carriedSource} {
		var ask capturedAsk
		if err := database.QueryRow(ctx, `select state, question from asks where id = $1::uuid`, id).Scan(&ask.state, &ask.question); err != nil {
			t.Fatalf("capture the base release's ask %s: %v", id, err)
		}
		captured[id] = ask
	}
	t.Logf("captured before the upgrade: asks %v, markdown:\n%s", captured, capturedMarkdown)
	stopProcess(t, previous)

	// This head alone: the test registers no other build.
	current := startDispatchProcessWith(t, databaseURL, dispatchOptions{Binary: head, SigningKey: previousBuildSigningKey, SettleDelay: oneAnswerSettleDelay})
	current.waitHealthy(t)
	var commits []string
	if err := database.QueryRow(ctx, `select coalesce(array_agg(distinct commit), '{}') from server_processes where heartbeat_at > now() - interval '15 seconds'`).Scan(&commits); err != nil {
		t.Fatalf("read the live builds: %v", err)
	}
	if !slices.Equal(commits, []string{headBuildStamp}) {
		t.Fatalf("the live processes serve %v, want this head's build alone", commits)
	}
	issue, spec := createCensusIssue(t, current, "ONE", "One answer", oneAnswerSpec)
	requireOneAnswerCase(t, ctx, current, database, databaseURL, baseCommit, oneAnswerCase{
		what: "the head's asks", issue: issue, spec: spec,
		own: blockAskOf(t, current, issue, "own"), ownBlock: "own",
		source: blockAskOf(t, current, issue, "source"), sourceBlock: "source", sourceMarkdown: oneAnswerSourceBlock,
	})

	// The whole case again over the asks the base release wrote, compared with the capture.
	for id, want := range captured {
		var question string
		var marked bool
		if err := database.QueryRow(ctx, `select question, node_kind is not null from asks where id = $1::uuid`, id).Scan(&question, &marked); err != nil || !marked || question != want.question {
			t.Fatalf("the carried ask %s reads %q, marked %t (%v), want the captured question %q on a node the boot step stated", id, question, marked, err, want.question)
		}
		if state := askNodeHolds(t, ctx, database, id)[graphstore.TupleAnswerState].ToID; state != want.state {
			t.Errorf("the carried ask %s's node holds answer_state %q, want the captured %q", id, state, want.state)
		}
	}
	if _, markdown := latestMarkdown(t, current, carriedSpec); markdown != capturedMarkdown {
		t.Errorf("the carried document reads otherwise than the capture before the upgrade:\n%s\nwant:\n%s", markdown, capturedMarkdown)
	}
	requireOneAnswerCase(t, ctx, current, database, databaseURL, baseCommit, oneAnswerCase{
		what: "the asks the base release wrote", issue: carriedIssue, spec: carriedSpec,
		own: carriedAsk, ownBlock: "carried",
		source: carriedSource, sourceBlock: "carried-source", sourceMarkdown: carriedSourceBlock,
		baseWritten: true,
	})

	scanForTheDeletedWriters(t)
}

// oneAnswerSourceBlock and carriedSourceBlock are the asks the copy steps copy, the head's and the
// base release's; each offers East, which the case answers.
const (
	oneAnswerSourceBlock = ":::ask{#source urgency=\"low\" multiple=\"false\"}\nWhich region goes first?\n\n- East: The quiet one.\n- West: The busy one.\n:::\n"
	carriedSourceBlock   = ":::ask{#carried-source urgency=\"low\" multiple=\"false\"}\nWhich team carries it first?\n\n- East: The quiet one.\n- West: The busy one.\n:::\n"
)

// oneAnswerCase is the issue and the asks requireOneAnswerCase runs the check's case over: own, an
// open ask of the issue's spec under the block ownBlock, and source, another, under the block
// sourceBlock, whose markdown, sourceMarkdown, a copy carries. baseWritten is whether the base
// release wrote the spec.
type oneAnswerCase struct {
	what, issue, spec   string
	own, ownBlock       string
	source, sourceBlock string
	sourceMarkdown      string
	baseWritten         bool
}

// requireOneAnswerCase is the whole of c-one-answer over c: the answer held once
// (requireAnswerHeldOnce); while a process of the base release is registered, the roll window
// keeping all eight attributes of a block answered meanwhile as its asks row and node hold them;
// once it is gone, an answer that moves no byte; a block that fails the strict read and claims an
// answer, which puts none on the node; and a copy of source on a second document, which is one
// copied_from tuple and no attribute, names the spec through the source's parent tuple, and, once
// the source is answered, owes a settlement, settles again and reads that answer through its own
// text.
func requireOneAnswerCase(t *testing.T, ctx context.Context, current *dispatchProcess, database *pgx.Conn, databaseURL, baseCommit string, c oneAnswerCase) {
	t.Helper()
	requireAnswerHeldOnce(t, ctx, current, database, c.spec, c.own, c.ownBlock, c.what)
	// A spec this head wrote holds none of the seven anywhere on a fleet of one build. One the base
	// release wrote keeps the state it wrote into each block until its first write on this build,
	// which c-attribute-removal reads; the answer, which moves no byte, adds no answer to any.
	held := storedAskAttributes
	if c.baseWritten {
		held = answerAttributes
	}
	for id, attributes := range liveAskAttributesOf(t, current, c.spec) {
		for _, name := range held {
			if value, ok := attributes[name]; ok {
				t.Errorf("%s: on a fleet of one build the live tree's block %s holds %s=%v", c.what, id, name, value)
			}
		}
	}

	// While a process of the base release is registered, the roll window keeps the block in step.
	roll := registerOtherBuild(t, ctx, databaseURL, baseCommit, headBuildStamp)
	rolled := insertAskBlock(t, current, c.issue, c.spec, "roll", "Roll it out?")
	answerAsk(t, current, rolled, "Ship", "Roll it out now.")
	var rolledRow []byte
	if err := database.QueryRow(ctx, `select answer from asks where id = $1::uuid`, rolled).Scan(&rolledRow); err != nil {
		t.Fatalf("%s: read the rolled ask's row: %v", c.what, err)
	}
	var rolledAnswer struct {
		User     string    `json:"user"`
		Selected []string  `json:"selected"`
		Text     *string   `json:"text"`
		At       time.Time `json:"at"`
	}
	if err := json.Unmarshal(rolledRow, &rolledAnswer); err != nil || rolledAnswer.Text == nil {
		t.Fatalf("%s: decode the rolled ask's answer %s: %v", c.what, rolledRow, err)
	}
	// The eight as the row and the node hold them: the block's own ask, so no copy of another
	// document's, and one whose block reads, whose node holds no invalid reason.
	inStep := map[string]any{
		"state": "answered", "answered_by": rolledAnswer.User, "answered_at": asks.AnsweredAtValue(rolledAnswer.At),
		"selected": rolledAnswer.Selected, "answer": *rolledAnswer.Text,
	}
	if invalid := askNodeHolds(t, ctx, database, rolled)["invalid"].ToID; invalid != "" {
		inStep["invalid"] = invalid
	}
	rolledBlock := liveAskAttributesOf(t, current, c.spec)["roll"]
	for _, name := range append(slices.Clone(storedAskAttributes), "invalid") {
		want, wanted := inStep[name]
		got, held := rolledBlock[name]
		if held != wanted || (wanted && mustJSON(t, got) != mustJSON(t, want)) {
			t.Errorf("%s: while two builds serve the block holds %s=%s (held %t), want %s (held %t), in step with the asks row",
				c.what, name, mustJSON(t, got), held, mustJSON(t, want), wanted)
		}
	}
	settledDocument(t, ctx, current, database, c.spec)
	if _, markdown := latestMarkdown(t, current, c.spec); !strings.Contains(markdown, `state="answered" answered_by="`+rolledAnswer.User+`"`) {
		t.Errorf("%s: while two builds serve the stored version does not hold the rolled answer:\n%s", c.what, markdown)
	}
	t.Logf("%s: while a process of %s is registered the block reads %v", c.what, baseCommit, rolledBlock)
	roll.stop()

	// The fleet is one build again: the helper writes nothing.
	last := insertAskBlock(t, current, c.issue, c.spec, "last", "One more question?")
	settledDocument(t, ctx, current, database, c.spec)
	lastNumber, lastBefore := latestMarkdown(t, current, c.spec)
	answerAsk(t, current, last, "Hold", "Not yet.")
	settledDocument(t, ctx, current, database, c.spec)
	if number, markdown := latestMarkdown(t, current, c.spec); number != lastNumber || markdown != lastBefore {
		for name, value := range storedAskBlock(t, markdown, "last").Attrs {
			if slices.Contains(storedAskAttributes, name) {
				t.Errorf("%s: the roll window wrote %s=%v on the block once the fleet is one build", c.what, name, value)
			}
		}
		t.Errorf("%s: once the fleet is one build the answer moved the document from version %d to %d:\n%s", c.what, lastNumber, number, markdown)
	}
	for name, value := range liveAskAttributesOf(t, current, c.spec)["last"] {
		if slices.Contains(storedAskAttributes, name) {
			t.Errorf("%s: once the fleet is one build the live tree's block last holds %s=%v", c.what, name, value)
		}
	}

	// A block that fails the strict read and claims an answer and an answerer puts neither on the node.
	probe := insertAskBlock(t, current, c.issue, c.spec, "probe", "Probe the forgery?")
	editAsBrowser(t, current, c.spec, "Probe the forgery?", func(tree *pmdoc.Node) error {
		for _, node := range tree.Children {
			if id, _ := node.Attrs[pmdoc.BlockIDAttr].(string); node.Type == "ask" && id == "probe" {
				node.Children[0].Children = nil
				node.Attrs["state"], node.Attrs["answered_by"], node.Attrs["answer"] = "answered", "mallory", "Forged."
				node.Attrs["answered_at"], node.Attrs["selected"] = time.Now().UTC().Format(time.RFC3339Nano), []string{"Ship"}
				return nil
			}
		}
		return errors.New("the document holds no ask block probe")
	})
	settledDocument(t, ctx, current, database, c.spec)
	probeHeld := askNodeHolds(t, ctx, database, probe)
	for _, kind := range []string{graphstore.TupleAnswer, graphstore.TupleAnsweredBy, graphstore.TupleSelected, graphstore.TupleAnsweredAt} {
		if tuple, held := probeHeld[kind]; held {
			t.Errorf("%s: the forged block put %s %q on the probe ask's node", c.what, kind, tuple.ToID)
		}
	}
	// The ask settlement opened for the block states no answer_state of its own until something
	// settles one; either way it is no answer.
	if state := probeHeld[graphstore.TupleAnswerState].ToID; (state != "" && state != asks.StateOpen) || probeHeld["invalid"].ToID == "" {
		t.Errorf("%s: the probe ask's node holds answer_state %q and invalid %q, want it open and the reason its block fails the strict read", c.what, state, probeHeld["invalid"].ToID)
	}

	// A copy of the source ask on a second document - the same block id and the same wording, as
	// a copy carries them - is one copied_from tuple and no attribute.
	var uploaded struct {
		Artifact struct {
			ID string `json:"id"`
		} `json:"artifact"`
	}
	copyBody := encodeJSON(t, map[string]string{"name": "copy.md", "content": "# Copy\n\n" + c.sourceMarkdown})
	if err := json.Unmarshal(current.call(t, http.MethodPost, "/api/v1/issues/"+c.issue+"/artifacts", copyBody, http.StatusCreated), &uploaded); err != nil {
		t.Fatalf("%s: upload the copy: %v", c.what, err)
	}
	copyDocument := uploaded.Artifact.ID
	settledDocument(t, ctx, current, database, copyDocument)
	var copies int
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if err := database.QueryRow(ctx, `
			select count(*) from graph_tuples where kind = $1 and to_kind = $2 and to_id = $3 and asserted
		`, graphstore.TupleCopiedFrom, graphstore.KindAsk, c.source).Scan(&copies); err != nil {
			t.Fatalf("%s: count the copies of the source ask: %v", c.what, err)
		}
		if copies > 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if copies != 1 {
		t.Errorf("%s: the source ask has %d copied_from tuples, want one", c.what, copies)
	}
	for _, document := range []string{c.spec, copyDocument} {
		_, markdown := latestMarkdown(t, current, document)
		if strings.Contains(markdown, "copied_from") {
			t.Errorf("%s: document %s stores a copied_from attribute:\n%s", c.what, document, markdown)
		}
		for id, attributes := range liveAskAttributesOf(t, current, document) {
			if value, held := attributes["copied_from_document"]; held {
				t.Errorf("%s: the live tree's block %s of %s holds copied_from_document=%v", c.what, id, document, value)
			}
		}
	}
	var sourceDocument, specKey string
	if err := database.QueryRow(ctx, `
		select (select to_id from graph_tuples where from_kind = $1 and from_id = $2 and kind = $3 and asserted order by seq desc limit 1),
		       (select ref_key from artifacts where id = $4::uuid)
	`, graphstore.KindAsk, c.source, graphstore.TuplePartOf, c.spec).Scan(&sourceDocument, &specKey); err != nil {
		t.Fatalf("%s: read the source ask's parent: %v", c.what, err)
	}
	if sourceDocument != specKey {
		t.Errorf("%s: the source ask's part_of reaches %q, want the spec %q", c.what, sourceDocument, specKey)
	}

	// The source is answered: the copy's document owes a settlement, and once it has settled again
	// the copy's entry in the asks member of the text an agent reads holds the source's answer,
	// naming the source and its document, while the copy's opening line in that text holds none
	// of the server-owned attributes.
	answerAsk(t, current, c.source, "East", "East first.")
	var owed bool
	if err := database.QueryRow(ctx, `select exists (select 1 from doc_settlements_pending where artifact_id = $1::uuid)`, copyDocument).Scan(&owed); err != nil || !owed {
		t.Errorf("%s: the answer to the source left the copy's document owing no settlement (%v), want it settled again", c.what, err)
	}
	settledDocument(t, ctx, current, database, copyDocument)
	var copiedState, copiedAnswer string
	if err := database.QueryRow(ctx, `
		with copied as (
			select t.to_id from graph_tuples t join document_blocks b on b.node_id = t.from_id and b.artifact_id = $1::uuid and b.block_id = $5
			where t.kind = $2 and t.asserted order by t.seq desc limit 1)
		select coalesce((select to_id from graph_tuples where from_kind = 'ask' and from_id = (select to_id from copied) and kind = $3 order by seq desc limit 1), ''),
		       coalesce((select to_id from graph_tuples where from_kind = 'ask' and from_id = (select to_id from copied) and kind = $4 order by seq desc limit 1), '')
	`, copyDocument, graphstore.TupleCopiedFrom, graphstore.TupleAnswerState, graphstore.TupleAnswer, c.sourceBlock).Scan(&copiedState, &copiedAnswer); err != nil {
		t.Fatalf("%s: read the copy's source answer: %v", c.what, err)
	}
	if copiedState != asks.StateAnswered || copiedAnswer != "East first." {
		t.Errorf("%s: the copy settled again reads its source %q %q, want the source's answer", c.what, copiedState, copiedAnswer)
	}
	var text struct {
		Markdown string          `json:"markdown"`
		Asks     []model.TextAsk `json:"asks"`
	}
	readJSON(t, current, "/api/v1/artifacts/"+copyDocument+"/text", &text)
	opener := ""
	for line := range strings.SplitSeq(text.Markdown, "\n") {
		if strings.HasPrefix(line, ":::ask{#"+c.sourceBlock+" ") {
			opener = line
		}
	}
	for _, name := range storedAskAttributes {
		if strings.Contains(opener, " "+name+`="`) {
			t.Errorf("%s: the copy's text, settled again, opens its block %q, holding the server-owned attribute %s:\n%s", c.what, opener, name, text.Markdown)
		}
	}
	var entry *model.TextAsk
	for index := range text.Asks {
		if text.Asks[index].BlockID == c.sourceBlock {
			entry = &text.Asks[index]
		}
	}
	if entry == nil || entry.State != asks.StateAnswered || entry.AnsweredBy != dispatchTestLogin || entry.Answer == nil || *entry.Answer != "East first." ||
		entry.CopiedFrom != c.source || entry.CopiedFromDocument != "dispatch://"+c.issue+"/spec" {
		t.Errorf("%s: the copy's text, settled again, answers its block %s in its asks as %s, want it answered by %s with East first., naming %s on dispatch://%s/spec",
			c.what, c.sourceBlock, mustJSON(t, entry), dispatchTestLogin, c.source, c.issue)
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// scanForTheDeletedWriters parses the source of the docs and pmdoc packages and fails where either
// declares setAskServerAttributes or IsAnswerAttribute. It allows rollWindowAskAttributes, the
// roll-window helper, where its fleet guard returns nothing on a fleet of one build, and prints
// that finding and the one of restoresApprovedText, which stays.
func scanForTheDeletedWriters(t *testing.T) {
	t.Helper()
	declared := map[string]string{}
	var guard string
	files := token.NewFileSet()
	for _, dir := range []string{"../../internal/dispatch/docs", "../../internal/dispatch/pmdoc"} {
		packages, err := parser.ParseDir(files, dir, func(info os.FileInfo) bool { return !strings.HasSuffix(info.Name(), "_test.go") }, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", dir, err)
		}
		for _, parsed := range packages {
			for name, file := range parsed.Files {
				for _, decl := range file.Decls {
					function, ok := decl.(*ast.FuncDecl)
					if !ok {
						continue
					}
					declared[function.Name.Name] = filepath.Base(name) + ":" + strconv.Itoa(files.Position(function.Pos()).Line)
					if function.Name.Name == "rollWindowAskAttributes" {
						guard = fleetGuard(function)
					}
				}
			}
		}
	}
	for _, deleted := range []string{"setAskServerAttributes", "IsAnswerAttribute"} {
		if at, found := declared[deleted]; found {
			t.Errorf("the scan finds %s declared at %s, want it gone", deleted, at)
		}
	}
	at, found := declared["rollWindowAskAttributes"]
	if !found || guard == "" {
		t.Errorf("the scan finds rollWindowAskAttributes at %q with fleet guard %q, want the roll-window helper returning nothing on a fleet of one build", at, guard)
	}
	t.Logf("scan: allowed rollWindowAskAttributes at %s, which keeps the copy in step with the asks row while two builds serve and %s", at, guard)
	if at, found := declared["restoresApprovedText"]; found {
		t.Logf("scan: restoresApprovedText stays, at %s", at)
	} else {
		t.Errorf("the scan finds no restoresApprovedText, which stays")
	}
}

// fleetGuard describes the fleet guard function opens with - `if !twoBuilds { return nil }` - or is
// "" where it opens with none.
func fleetGuard(function *ast.FuncDecl) string {
	if function.Body == nil || len(function.Body.List) == 0 {
		return ""
	}
	guard, ok := function.Body.List[0].(*ast.IfStmt)
	if !ok || len(guard.Body.List) != 1 {
		return ""
	}
	not, ok := guard.Cond.(*ast.UnaryExpr)
	if !ok || not.Op != token.NOT {
		return ""
	}
	fleet, ok := not.X.(*ast.Ident)
	if !ok {
		return ""
	}
	returned, ok := guard.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(returned.Results) != 1 {
		return ""
	}
	if nothing, ok := returned.Results[0].(*ast.Ident); !ok || nothing.Name != "nil" {
		return ""
	}
	return "returns nil while !" + fleet.Name
}
