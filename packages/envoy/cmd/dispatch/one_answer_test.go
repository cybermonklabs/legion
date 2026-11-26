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
	"regexp"
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

// answerAttributes are the four of them that hold an answer: who and when, the selection and the
// words.
var answerAttributes = []string{"answered_by", "answered_at", "selected", "answer"}

// answerWordAttributes are the two of those that hold the answer itself, the selection and the
// words, which no version written on a fleet of one build holds (R5).
var answerWordAttributes = []string{"selected", "answer"}

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

// openTab opens a browser's tab on the document, signed in as the test's person, and waits for the
// room to send it a document holding holding.
func openTab(t *testing.T, p *dispatchProcess, artifactID, holding string) *docstest.Peer {
	t.Helper()
	tab := docstest.Dial(t, p.documentURL(artifactID), http.Header{"X-Dispatch-User": []string{dispatchTestLogin}}, artifactID, crdt.New())
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(markdown(tab), holding) {
		if time.Now().After(deadline) {
			tab.Close()
			t.Fatalf("the tab never read a document holding %q:\n%s", holding, markdown(tab))
		}
		time.Sleep(50 * time.Millisecond)
	}
	return tab
}

// editAsBrowser opens a browser's tab on the document (openTab) and makes change on its tree in one
// transaction, which the tab sends the room as a browser's own update: a write no route refuses.
func editAsBrowser(t *testing.T, p *dispatchProcess, artifactID, holding string, change func(*pmdoc.Node) error) {
	t.Helper()
	editor := openTab(t, p, artifactID, holding)
	defer editor.Close()
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
// waits until the document owes no settlement. A document that still owes one after 30s fails the
// test with each line of the process's output that names it.
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
			var named []string
			for line := range strings.SplitSeq(p.Output.String(), "\n") {
				if strings.Contains(line, artifactID) {
					named = append(named, line)
				}
			}
			t.Fatalf("%s still owes a settlement after 30s; the process's output names it in:\n%s", artifactID, strings.Join(named, "\n"))
		}
	}
}

// settledOnTheNextLoad settles a document an issue's close or reopen left owing a settlement,
// which neither arms: the room's next load, a timer or the resumption runs it
// (docs.MarkReopenOwedAskDocuments, onLoadDocument). Every room is closed first, so the tab a
// browser opens on the document, holding holding, is that next load, and the test waits for the
// settlement it arms (settledDocument).
func settledOnTheNextLoad(t *testing.T, ctx context.Context, p *dispatchProcess, database *pgx.Conn, artifactID, holding string) {
	t.Helper()
	p.call(t, http.MethodPost, "/api/v1/artifacts/_test/quiesce", "", http.StatusOK)
	openTab(t, p, artifactID, holding).Close()
	settledDocument(t, ctx, p, database, artifactID)
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

// opensBlock reports whether line, the opening line of an ask block, opens the block blockID.
func opensBlock(line, blockID string) bool {
	return strings.Contains(line, "{#"+blockID+" ") || strings.Contains(line, "{#"+blockID+"}")
}

// blockOpener is the opening line of the ask block blockID in markdown, or "".
func blockOpener(markdown, blockID string) string {
	for _, opener := range askOpener.FindAllString(markdown, -1) {
		if opensBlock(opener, blockID) {
			return opener
		}
	}
	return ""
}

// withoutBlockAttributes is markdown with the attributes names taken off the opening line of the
// ask block blockID, and every other byte as it is: what two versions hold in common once the one
// change they may differ by, those attributes of that block, is taken out of each. A value holds
// no raw quote, which the renderer writes as a reference.
func withoutBlockAttributes(markdown, blockID string, names ...string) string {
	attribute := regexp.MustCompile(` (` + strings.Join(names, "|") + `)="[^"]*"`)
	return askOpener.ReplaceAllStringFunc(markdown, func(line string) string {
		if !opensBlock(line, blockID) {
			return line
		}
		return attribute.ReplaceAllString(line, "")
	})
}

// requireOnlyChange fails t unless after, the document's version number, is the one version after
// before, the version beforeNumber, and differs from it only in the attributes names of the ask
// block blockID.
func requireOnlyChange(t *testing.T, what string, beforeNumber int, before string, number int, after, blockID string, names ...string) {
	t.Helper()
	if number != beforeNumber+1 {
		t.Errorf("%s moved the document from version %d to version %d, want one version, %d:\nbefore:\n%s\nafter:\n%s", what, beforeNumber, number, beforeNumber+1, before, after)
	}
	if withoutBlockAttributes(after, blockID, names...) != withoutBlockAttributes(before, blockID, names...) {
		t.Errorf("%s: version %d differs from version %d otherwise than in the block %s's %s:\nbefore:\n%s\nafter:\n%s",
			what, number, beforeNumber, blockID, strings.Join(names, ", "), before, after)
	}
}

// requireNoAnswerOnBlock fails t, printing each it finds, where the ask block blockID of markdown,
// the stored version number, holds the answer's words or its selection.
func requireNoAnswerOnBlock(t *testing.T, what string, number int, markdown, blockID string) {
	t.Helper()
	block := storedAskBlock(t, markdown, blockID)
	for _, name := range answerWordAttributes {
		if value, held := block.Attrs[name]; held {
			t.Errorf("%s: the version %d holds %s=%s on the block %s, want no answer and no selection", what, number, name, mustJSON(t, value), blockID)
		}
	}
}

// requireNoAnswerInTheLiveTree fails t, printing each it finds, where the ask block blockID of the
// process's live tree of the document holds the answer's words or its selection.
func requireNoAnswerInTheLiveTree(t *testing.T, what string, p *dispatchProcess, artifactID, blockID string) {
	t.Helper()
	live := liveAskAttributesOf(t, p, artifactID)[blockID]
	for _, name := range answerWordAttributes {
		if value, held := live[name]; held {
			t.Errorf("%s: the live tree's block %s holds %s=%s, want no answer and no selection", what, blockID, name, mustJSON(t, value))
		}
	}
}

// requireCarried fails t unless the document reads approved at number, its latest version, by a
// review Dispatch carried there from the approval the person gave of the version approved.
func requireCarried(t *testing.T, what string, p *dispatchProcess, artifactID string, number, approved int) {
	t.Helper()
	approval, carried := approvalOf(t, p, artifactID), newestReviewOf(t, p, artifactID)
	prefix := fmt.Sprintf("Carried forward from the approval of version %d:", approved)
	if !approval.approvedLatest() || approval.LatestVersion != number || carried.Version != number || carried.Reason == nil || !strings.HasPrefix(*carried.Reason, prefix) {
		t.Errorf("%s: the document reads %s with newest review %s, want it approved at version %d, its latest, by a review whose reason opens %q",
			what, approvalState(approval), mustJSON(t, carried), number, prefix)
	}
}

// requireAnswerHeldOnce is the check's case over one open ask of a document on a fleet of one
// build: a person approves the document at its latest version and answers the ask through the
// answer route. The answer is on the ask's node, each fact under the ask.answered that caused it,
// and on the asks row. The version the answer wrote is the one after the version before, and
// differs from it only in the block's state, answered_by and answered_at, each as the node holds
// it (requireKeptForm), which R5 keeps for one release. The block holds no answer and no
// selection, in that version and in the live tree. The approval is carried to that version, and
// the answer reports no superseded approval.
//
// releaseBeforeOpen names the other open ask blocks of a document the base release wrote, whose
// opener holds the state="open" that release wrote and no write of this build has taken off yet.
// The answer's version is then the document's first accepted write on this build, which takes
// that state off each of them as R5 has it, so each must hold state="open" before and no state
// after, and that is the one other difference the comparison allows.
func requireAnswerHeldOnce(t *testing.T, ctx context.Context, p *dispatchProcess, database *pgx.Conn, artifactID, askID, blockID, what string, releaseBeforeOpen ...string) {
	t.Helper()
	settledDocument(t, ctx, p, database, artifactID)
	beforeNumber, before := latestMarkdown(t, p, artifactID)
	p.call(t, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/reviews", `{"state":"approved"}`, http.StatusCreated)
	if approved := approvalOf(t, p, artifactID); !approved.approvedLatest() || approved.LatestVersion != beforeNumber {
		t.Fatalf("%s: the document reads %s once the person approved it, want it approved at version %d, its latest", what, approvalState(approved), beforeNumber)
	}

	const text = "Ship it once the flag is on."
	if reply := answerAsk(t, p, askID, "Ship", text); strings.Contains(string(reply), "superseded_approval") {
		t.Errorf("%s: the answer reports a superseded approval: %s", what, reply)
	}
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
	wrote := fmt.Sprintf("%s: the version %d the answer wrote", what, afterNumber)
	compared := before
	for _, open := range releaseBeforeOpen {
		if opener := blockOpener(before, open); !strings.Contains(opener, ` state="open"`) {
			t.Errorf("%s: before the answer the block %s opens %q, want the state=\"open\" the base release wrote", what, open, opener)
		}
		if state, held := storedAskBlock(t, after, open).Attrs["state"]; held {
			t.Errorf("%s: the version %d the answer wrote holds state=%s on the open ask block %s, want none (R5's first write on this build)",
				what, afterNumber, mustJSON(t, state), open)
		}
		compared = withoutBlockAttributes(compared, open, "state")
	}
	if len(releaseBeforeOpen) > 0 {
		t.Logf("%s: the answer's version %d, the document's first accepted write on this build, takes the base release's state=\"open\" off %v", what, afterNumber, releaseBeforeOpen)
	}
	requireOnlyChange(t, what+": the answer", beforeNumber, compared, afterNumber, after, blockID, "state", "answered_by", "answered_at")
	requireKeptForm(t, ctx, database, wrote, after, keptBlock{block: blockID, ask: askID})
	requireNoAnswerOnBlock(t, what+": the answer", afterNumber, after, blockID)
	requireNoAnswerInTheLiveTree(t, what+": the answer", p, artifactID, blockID)
	requireCarried(t, wrote, p, artifactID, afterNumber, beforeNumber)
	t.Logf("%s: the answer wrote version %d, whose block opens %q, and the document reads %s", what, afterNumber, blockOpener(after, blockID), approvalState(approvalOf(t, p, artifactID)))
}

// A person's answer is held once, on the ask's node (c-one-answer). The test registers no other
// build, so processes.Live shows the whole fleet on this build: the answer route appends the
// answer to the ask's node, the asks row is its projection, and the version the answer writes
// changes only the block's state, answered_by and answered_at, which R5 keeps for one release,
// and carries an approval. While a process of the base release is registered, the roll window
// keeps the eight attributes of the block in step with the asks row; once it is gone the helper
// writes nothing. A block the browser's room writes, which fails the strict read and claims an
// answer, puts none on the node and none in the stored version. A resolve and a decline each write
// one version changing only the block's state. An issue's close and reopen each write one version
// of each document, changing only its open ask's state; the close withdraws an approval request
// and leaves an approval stale, and the reopen restores the request with the person and carries
// the approval back. A copy is one copied_from tuple and no attribute on any node; its stored block
// keeps its provenance, which the text route takes off, and it settles again once its source is
// answered. The whole case holds again over asks the base release wrote and this head's boot step
// carried forward. Last, the source of docs and pmdoc declares neither deleted writer.
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
// release wrote the spec, which this head has not written since.
type oneAnswerCase struct {
	what, issue, spec   string
	own, ownBlock       string
	source, sourceBlock string
	sourceMarkdown      string
	baseWritten         bool
}

// requireOneAnswerCase is the whole of c-one-answer over c, in the order its words give: the
// answer held once (requireAnswerHeldOnce); while a process of the base release is registered, the
// roll window keeping all eight attributes of a block answered meanwhile in step with its asks row;
// once it is gone, an answer whose version holds the block's state, answered_by and answered_at and
// no answer and no selection; a block the browser's room writes, which fails the strict read and
// claims an answer, putting none on the node and none in the stored version; a resolve and a
// decline, each one version changing only the block's state; the close and the reopen
// (requireCloseAndReopen); and a copy of source on a second document, which is one copied_from
// tuple and no attribute on any node, whose stored block keeps copied_from and copied_from_document
// and whose text serves it without them, which names the spec through the source's parent tuple,
// and which, once the source is answered, owes a settlement and settles again against that answer.
func requireOneAnswerCase(t *testing.T, ctx context.Context, current *dispatchProcess, database *pgx.Conn, databaseURL, baseCommit string, c oneAnswerCase) {
	t.Helper()
	// In the repeat over the asks the base release wrote, the answer is the first accepted write of
	// the spec on this build: the decisions step's third phase writes no version of it, since that
	// phase works only on an issue with an answered ask whose block left its spec or with a
	// decisions document (graphcensus.ReadRestoreWork, DecisionStep.RestoreAndConvert), and this
	// issue has neither; its backfill and its catch-up state what the release before wrote on the
	// asks' nodes alone. So the answer's version also takes the state="open" the base release wrote
	// off the source ask's block, which is still open (R5).
	var releaseBeforeOpen []string
	if c.baseWritten {
		releaseBeforeOpen = []string{c.sourceBlock}
	}
	requireAnswerHeldOnce(t, ctx, current, database, c.spec, c.own, c.ownBlock, c.what, releaseBeforeOpen...)

	// While a process of the base release is registered, the roll window keeps the eight attributes
	// of a block answered meanwhile in step with its asks row, as the release before does.
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
	_, rolledMarkdown := latestMarkdown(t, current, c.spec)
	requireBlockInStepWithRow(t, ctx, database, rolledMarkdown, "roll", c.spec, "")
	t.Logf("%s: while a process of %s is registered the block reads %v in the live tree and opens %q in the stored version",
		c.what, baseCommit, rolledBlock, blockOpener(rolledMarkdown, "roll"))
	roll.stop()

	// The process is gone, so the fleet is one build again and the helper writes nothing: the
	// version the next answer writes holds that block's state, answered_by and answered_at and no
	// answer and no selection.
	last := insertAskBlock(t, current, c.issue, c.spec, "last", "One more question?")
	settledDocument(t, ctx, current, database, c.spec)
	lastNumber, lastBefore := latestMarkdown(t, current, c.spec)
	answerAsk(t, current, last, "Hold", "Not yet.")
	settledDocument(t, ctx, current, database, c.spec)
	lastWhat := c.what + ": the answer once the fleet is one build again"
	number, markdown := latestMarkdown(t, current, c.spec)
	requireOnlyChange(t, lastWhat, lastNumber, lastBefore, number, markdown, "last", "state", "answered_by", "answered_at")
	requireKeptForm(t, ctx, database, fmt.Sprintf("%s: the version %d", lastWhat, number), markdown, keptBlock{block: "last", ask: last})
	requireNoAnswerOnBlock(t, lastWhat, number, markdown, "last")
	requireNoAnswerInTheLiveTree(t, lastWhat, current, c.spec, "last")

	// The browser's room, which takes a document socket peer's update as a browser's own, takes an
	// ask block that fails the strict read and claims an answer and an answerer: an agent's upload
	// cannot plant the claim, since the agent boundary strips every server-owned attribute
	// (asUploaded). Neither reaches the node, and the stored version settlement writes over the
	// block holds nothing the browser wrote.
	probe := insertAskBlock(t, current, c.issue, c.spec, "probe", "Probe the forgery?")
	settledDocument(t, ctx, current, database, c.spec)
	probeBefore, _ := latestMarkdown(t, current, c.spec)
	claims := map[string]any{
		"state": asks.StateAnswered, "answered_by": "mallory", "answered_at": time.Now().UTC().Format(time.RFC3339Nano),
		"selected": []string{"Ship"}, "answer": "Forged.",
	}
	editAsBrowser(t, current, c.spec, "Probe the forgery?", func(tree *pmdoc.Node) error {
		for _, node := range tree.Children {
			if id, _ := node.Attrs[pmdoc.BlockIDAttr].(string); node.Type == "ask" && id == "probe" {
				node.Children[0].Children = nil
				for name, value := range claims {
					node.Attrs[name] = value
				}
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
	var nodeValues []string
	if err := database.QueryRow(ctx, `
		select coalesce(array_agg(to_id), '{}') from graph_tuples where from_kind = 'ask' and from_id = $1 and to_kind = 'value'
	`, probe).Scan(&nodeValues); err != nil {
		t.Fatalf("%s: read the values of the probe ask's node: %v", c.what, err)
	}
	for name, value := range claims {
		claimed, _ := value.(string)
		if name == "selected" {
			claimed = asks.SelectedValue(value.([]string))
		}
		if slices.Contains(nodeValues, claimed) {
			t.Errorf("%s: the browser's claim %s=%s is a value the probe ask's node holds (%v), want each claim one it does not", c.what, name, claimed, nodeValues)
		}
	}
	probeNumber, probeMarkdown := latestMarkdown(t, current, c.spec)
	if probeNumber <= probeBefore {
		t.Errorf("%s: settlement wrote no version over the browser's block probe: the document stays at version %d", c.what, probeNumber)
	}
	stored := storedAskBlock(t, probeMarkdown, "probe")
	for _, name := range answerAttributes {
		value, held := stored.Attrs[name]
		switch {
		case !held:
		case mustJSON(t, value) == mustJSON(t, claims[name]):
			t.Errorf("%s: the stored version %d holds the browser's %s=%s on the block probe, which fails the strict read; want nothing the browser wrote",
				c.what, probeNumber, name, mustJSON(t, value))
		default:
			t.Errorf("%s: the stored version %d holds %s=%s on the block probe, whose node holds no answer", c.what, probeNumber, name, mustJSON(t, value))
		}
	}
	if state, held := stored.Attrs["state"]; held && state != probeHeld[graphstore.TupleAnswerState].ToID {
		t.Errorf("%s: the stored version %d holds state=%s on the block probe, want its node's %q or none", c.what, probeNumber, mustJSON(t, state), probeHeld[graphstore.TupleAnswerState].ToID)
	}
	t.Logf("%s: settlement wrote version %d over the browser's block, which opens %q", c.what, probeNumber, blockOpener(probeMarkdown, "probe"))

	// A resolve and a decline each write one version, whose only change from the version before is
	// the state of the ask's block, equal to its node's.
	closings := []struct{ what, block, ask, route, body string }{
		{what: "the resolve", block: "resolve-it", route: "resolve", body: `{"kind":"resolved","reason":"Settled in the thread."}`},
		{what: "the decline", block: "decline-it", route: "decline", body: `{"note":"Not this release."}`},
	}
	for index := range closings {
		closings[index].ask = insertAskBlock(t, current, c.issue, c.spec, closings[index].block, "Close "+closings[index].block+"?")
	}
	for _, closing := range closings {
		settledDocument(t, ctx, current, database, c.spec)
		beforeNumber, before := latestMarkdown(t, current, c.spec)
		current.call(t, http.MethodPost, "/api/v1/asks/"+closing.ask+"/"+closing.route, closing.body, http.StatusOK)
		settledDocument(t, ctx, current, database, c.spec)
		number, markdown := latestMarkdown(t, current, c.spec)
		what := c.what + ": " + closing.what
		requireOnlyChange(t, what, beforeNumber, before, number, markdown, closing.block, "state")
		state := storedAskBlock(t, markdown, closing.block).Attrs["state"]
		if nodeState := askNodeHolds(t, ctx, database, closing.ask)[graphstore.TupleAnswerState].ToID; nodeState == "" || nodeState == asks.StateOpen || state != nodeState {
			t.Errorf("%s: the version %d holds state=%s on the block %s, want the state its node holds, %q, which is not open", what, number, mustJSON(t, state), closing.block, nodeState)
		}
		t.Logf("%s wrote version %d, whose block opens %q", what, number, blockOpener(markdown, closing.block))
	}

	requireCloseAndReopen(t, ctx, current, database, c)

	// A copy of the source ask on a second document - the same block id and the same wording, as
	// a copy carries them - is one copied_from tuple at the source ask and no copied_from_document
	// attribute on any node.
	copyDocument := uploadedID(t, uploadDocument(t, current, c.issue, "copy.md", "# Copy\n\n"+c.sourceMarkdown))
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
	var documentTuples, documentNodes int
	if err := database.QueryRow(ctx, `
		select (select count(*) from graph_tuples where kind = 'copied_from_document'),
		       (select count(*) from graph_nodes where attributes ->> 'copied_from_document' is not null)
	`).Scan(&documentTuples, &documentNodes); err != nil {
		t.Fatalf("%s: count the copied_from_document attributes of the nodes: %v", c.what, err)
	}
	if documentTuples != 0 || documentNodes != 0 {
		t.Errorf("%s: %d tuples and %d nodes hold a copied_from_document attribute, want none on any node", c.what, documentTuples, documentNodes)
	}
	// The copy's block in the stored version keeps copied_from and copied_from_document, as R5
	// keeps them until the kept-for-one-release item stored-ask-state removes them, and the text
	// route, read with `Dispatch-Asks: 1` as this build's CLI reads it, serves the block without
	// them.
	sourceAddress := "dispatch://" + c.issue + "/spec"
	copied := keptBlock{block: c.sourceBlock, ask: c.source, copied: sourceAddress}
	copyNumber, copyMarkdown := latestMarkdown(t, current, copyDocument)
	requireKeptForm(t, ctx, database, fmt.Sprintf("%s: the copy's version %d", c.what, copyNumber), copyMarkdown, copied)
	servedWithoutAttributes := func(when string) servedText {
		t.Helper()
		text := servedTextOf(t, current, "/api/v1/artifacts/"+copyDocument+"/text")
		if opener := blockOpener(text.Markdown, c.sourceBlock); opener == "" || askServerAttribute.MatchString(opener) {
			t.Errorf("%s: %s, GET /api/v1/artifacts/{id}/text read with Dispatch-Asks: 1 opens the copy's block %q, want it holding none of the server-owned attributes:\n%s",
				c.what, when, opener, text.Markdown)
		}
		return text
	}
	servedWithoutAttributes("once the copy settled")
	t.Logf("%s: the copy's version %d opens its block %q", c.what, copyNumber, blockOpener(copyMarkdown, c.sourceBlock))
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
	// its stored block holds the source's answer in the form R5 keeps, and the copy's entry in the
	// asks member of the text an agent reads holds that answer, naming the source and its
	// document, while the copy's opening line in that text holds none of the server-owned
	// attributes.
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
	settledNumber, settledMarkdown := latestMarkdown(t, current, copyDocument)
	requireKeptForm(t, ctx, database, fmt.Sprintf("%s: the copy's version %d, settled again", c.what, settledNumber), settledMarkdown, copied)
	text := servedWithoutAttributes("settled again")
	entry := textAskOf(text.Asks, c.sourceBlock)
	if entry == nil || entry.State != asks.StateAnswered || entry.AnsweredBy != dispatchTestLogin || entry.Answer == nil || *entry.Answer != "East first." ||
		entry.CopiedFrom != c.source || entry.CopiedFromDocument != sourceAddress {
		t.Errorf("%s: the copy's text, settled again, answers its block %s in its asks as %s, want it answered by %s with East first., naming %s on %s",
			c.what, c.sourceBlock, mustJSON(t, entry), dispatchTestLogin, c.source, sourceAddress)
	}
}

// closeApprovedDocument and closeRequestedDocument are the two new documents the close and the
// reopen run over, each holding one open ask: a person approves the first, and an agent asks the
// person to approve the second.
const (
	closeApprovedDocument = "# Approved\n\nThe person approves this document.\n\n" +
		":::ask{#approved-open urgency=\"med\" multiple=\"false\"}\nShip the approved one?\n\n- Ship: Now.\n- Hold: Later.\n:::\n"
	closeRequestedDocument = "# Requested\n\nAn agent asks the person to approve this document.\n\n" +
		":::ask{#requested-open urgency=\"med\" multiple=\"false\"}\nShip the requested one?\n\n- Ship: Now.\n- Hold: Later.\n:::\n"
)

// closedRequest is an approval request as GET /api/v1/asks/{id} reads it once a close may have
// withdrawn it: its state, the kind of its resolution, and the version of its document it names
// and the version it was handed to the person at.
type closedRequest struct {
	State      string `json:"state"`
	Resolution *struct {
		Kind string `json:"kind"`
	} `json:"resolution"`
	Approval struct {
		Version          int `json:"version"`
		RequestedVersion int `json:"requested_version"`
	} `json:"approval"`
}

func closedRequestOf(t *testing.T, p *dispatchProcess, askID string) closedRequest {
	t.Helper()
	var read struct {
		Ask closedRequest `json:"ask"`
	}
	readJSON(t, p, "/api/v1/asks/"+askID, &read)
	return read.Ask
}

// requireCloseAndReopen is the close and the reopen of c's issue. An agent writes two new
// documents of the issue, each with one open ask; a person approves the first at its latest
// version, and an agent requests approval of the second, so the request waits on the person. The
// issue closes while both asks are open: the close writes one version of each, whose only change
// from the version before is state="resolved" on the open ask's block; it withdraws the request,
// which its version does not move; and the first reads no approval of its latest version, stale,
// as closed_settlement_reopen_approval_test.go holds. The issue reopens: the reopen writes one
// version of each, whose only change from the close's is that state is gone from that block again;
// the request is restored, naming that version of the second and requested at it, so it still
// waits on the person; and the first reads approved at that version, its latest, carried from the
// person's approval.
func requireCloseAndReopen(t *testing.T, ctx context.Context, p *dispatchProcess, database *pgx.Conn, c oneAnswerCase) {
	t.Helper()
	approvedID := agentUpload(t, p, c.issue, "approved.md", closeApprovedDocument)
	requestedID := agentUpload(t, p, c.issue, "requested.md", closeRequestedDocument)
	blockAskOf(t, p, c.issue, "approved-open")
	blockAskOf(t, p, c.issue, "requested-open")
	approved := approveAndCapture(t, ctx, p, database, c.issue, approvedID, "the first new document")
	if approval := approvalOf(t, p, approvedID); !approval.approvedLatest() || approval.LatestVersion != approved.version {
		t.Fatalf("%s: the first new document reads %s once the person approved it, want it approved at version %d, its latest", c.what, approvalState(approval), approved.version)
	}
	settledDocument(t, ctx, p, database, requestedID)
	requestedNumber, requestedText := latestMarkdown(t, p, requestedID)
	request := openedRequestID(t, asAgentSession(t, p, http.MethodPost, "/api/v1/artifacts/"+requestedID+"/approval-requests", map[string]any{}, http.StatusCreated))
	if read := approvalRequestOf(t, p, request); !read.waitsOnThePersonAt(requestedNumber) {
		t.Fatalf("%s: the second new document's request reads %s, want it waiting on the person at version %d", c.what, read, requestedNumber)
	}
	documents := []struct {
		what, id, block, question string
		number                    int
		markdown                  string
	}{
		{"the first new document", approvedID, "approved-open", "Ship the approved one?", approved.version, approved.markdown},
		{"the second new document", requestedID, "requested-open", "Ship the requested one?", requestedNumber, requestedText},
	}
	t.Logf("%s: the first new document %s is approved at version %d; the second, %s, has the request %s waiting on the person at version %d",
		c.what, approvedID, approved.version, requestedID, request, requestedNumber)

	p.call(t, http.MethodPatch, "/api/v1/issues/"+c.issue, `{"status":"done"}`, http.StatusOK)
	for index := range documents {
		document := &documents[index]
		settledOnTheNextLoad(t, ctx, p, database, document.id, document.question)
		number, markdown := latestMarkdown(t, p, document.id)
		what := c.what + ": the close's version of " + document.what
		requireOnlyChange(t, what, document.number, document.markdown, number, markdown, document.block, "state")
		if before, after := storedAskBlock(t, document.markdown, document.block).Attrs["state"], storedAskBlock(t, markdown, document.block).Attrs["state"]; before != nil || after != asks.StateResolved {
			t.Errorf("%s: the block %s holds state=%s in version %d and state=%s in version %d, want none and %q",
				what, document.block, mustJSON(t, before), document.number, mustJSON(t, after), number, asks.StateResolved)
		}
		document.number, document.markdown = number, markdown
	}
	if read := closedRequestOf(t, p, request); read.State != asks.StateResolved || read.Resolution == nil || read.Resolution.Kind != model.AskResolutionWithdrawn ||
		read.Approval.Version != requestedNumber || read.Approval.RequestedVersion != requestedNumber {
		t.Errorf("%s: after the close the second new document's request reads %s, want it withdrawn, still naming version %d and requested at it",
			c.what, mustJSON(t, read), requestedNumber)
	}
	if approval := approvalOf(t, p, approvedID); approval.approvedLatest() || approval.State != "stale" || approval.LatestVersion != documents[0].number {
		t.Errorf("%s: after the close the first new document reads %s, want no approval of its latest version, %d, so that it reads stale",
			c.what, approvalState(approval), documents[0].number)
	}

	p.call(t, http.MethodPatch, "/api/v1/issues/"+c.issue, `{"status":"todo"}`, http.StatusOK)
	for index := range documents {
		document := &documents[index]
		settledOnTheNextLoad(t, ctx, p, database, document.id, document.question)
		number, markdown := latestMarkdown(t, p, document.id)
		what := c.what + ": the reopen's version of " + document.what
		requireOnlyChange(t, what, document.number, document.markdown, number, markdown, document.block, "state")
		if state, held := storedAskBlock(t, markdown, document.block).Attrs["state"]; held {
			t.Errorf("%s: the block %s holds state=%s in version %d, want none, its ask open again", what, document.block, mustJSON(t, state), number)
		}
		document.number, document.markdown = number, markdown
	}
	if read := approvalRequestOf(t, p, request); !read.waitsOnThePersonAt(documents[1].number) {
		t.Errorf("%s: after the reopen the second new document's request reads %s, want it waiting on the person at version %d, requested at it",
			c.what, read, documents[1].number)
	}
	requireCarried(t, c.what+": after the reopen the first new document", p, approvedID, documents[0].number, approved.version)
	t.Logf("%s: the close and the reopen left the first new document at version %d reading %s, and the second's request %s",
		c.what, documents[0].number, approvalState(approvalOf(t, p, approvedID)), approvalRequestOf(t, p, request))
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
