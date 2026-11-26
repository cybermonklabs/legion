package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The Decisions read's entries: one entry for each node that holds a
// question the issue asked or a decision of one of its documents, read from the store.

// readEntries is the entries member of GET /api/v1/issues/{key}/decisions, as alice reads it.
func readEntries(t *testing.T, handler http.Handler, key string) []decisionEntry {
	t.Helper()
	return readIssueDecisions(t, handler, key).Entries
}

// entryKey is an entry's node, written kind:id.
func entryKey(entry decisionEntry) string { return entry.Node.Kind + ":" + entry.Node.ID }

// entriesByNode keys entries by their node. A node more than one entry names keeps the last of
// them; a check that counts a node's entries reads the list itself.
func entriesByNode(entries []decisionEntry) map[string]decisionEntry {
	byNode := make(map[string]decisionEntry, len(entries))
	for _, entry := range entries {
		byNode[entryKey(entry)] = entry
	}
	return byNode
}

// requireEntry is the entry of node, failing with every entry the read answered where there is
// none.
func requireEntry(t *testing.T, entries []decisionEntry, what, node string) decisionEntry {
	t.Helper()
	entry, found := entriesByNode(entries)[node]
	if !found {
		t.Fatalf("the Decisions read answers no entry for %s (%s); it answered %s", what, node, entryJSON(t, entries))
	}
	return entry
}

// entryJSON is value as the read answers it, for a failure to print.
func entryJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode %T: %v", value, err)
	}
	return string(encoded)
}

// shapedTargets is the node of each spec node an entry's shaped answers, sorted.
func shapedTargets(entry decisionEntry) []string {
	targets := make([]string, 0, len(entry.Shaped))
	for _, link := range entry.Shaped {
		targets = append(targets, link.To.ID)
	}
	sort.Strings(targets)
	return targets
}

// shapedLink is entry's link to the spec node target.
func shapedLink(t *testing.T, entry decisionEntry, target string) model.DecisionLink {
	t.Helper()
	for _, link := range entry.Shaped {
		if link.To.ID == target {
			return link
		}
	}
	t.Fatalf("the entry %s answers no link to %s; it answers %v", entryKey(entry), target, shapedTargets(entry))
	return model.DecisionLink{}
}

// decisionMemberLink is the link to the spec node target of the decision at ref in the read's old
// decisions member.
func decisionMemberLink(t *testing.T, read issueDecisions, ref, target string) model.DecisionLink {
	t.Helper()
	for _, decision := range read.Decisions {
		if decision.Ref != ref {
			continue
		}
		for _, link := range decision.Links {
			if link.To.ID == target {
				return link
			}
		}
		t.Fatalf("the decisions member's %s answers no link to %s; it answers %s", ref, target, entryJSON(t, decision.Links))
	}
	t.Fatalf("the decisions member answers no decision %s; it answers %s", ref, entryJSON(t, read.Decisions))
	return model.DecisionLink{}
}

// agentUploadAs uploads content as the issue's document name through the agent's session,
// carrying body's other members beside name, content and actor. An upload over a document the
// issue already holds under name names the token of the version it replaces, and a first upload
// names none (uploadBaseNamed).
func agentUploadAs(t *testing.T, handler http.Handler, issueKey, name, content string, body map[string]any) {
	t.Helper()
	path := "/api/v1/issues/" + issueKey + "/artifacts"
	request := map[string]any{
		"name": name, "content": content, "actor": sessionActor(),
		"precondition": uploadBaseNamed(t, handler, path, name),
	}
	for member, value := range body {
		request[member] = value
	}
	requireAgentWrite(t, "the agent's "+name, sessionRequest(t, handler, http.MethodPost, path, request), http.StatusCreated)
}

// versionCreatedAt is when version number of the document was written, by the database's clock.
func versionCreatedAt(t *testing.T, database *store.Store, artifactID string, number int) time.Time {
	t.Helper()
	var at time.Time
	if err := database.Pool.QueryRow(context.Background(),
		`select created_at from artifact_versions where artifact_id = $1::uuid and number = $2`, artifactID, number).Scan(&at); err != nil {
		t.Fatalf("read when version %d of %s was written: %v", number, artifactID, err)
	}
	return at
}

// rowChangedVersion is the changed_version of the block's document_blocks row: the version its
// whole text, readings included, last changed in.
func rowChangedVersion(t *testing.T, database *store.Store, document decisionDocument, block string) int {
	t.Helper()
	var changed int
	if err := database.Pool.QueryRow(context.Background(),
		`select changed_version from document_blocks where artifact_id = $1::uuid and block_id = $2`, document.id, block).Scan(&changed); err != nil {
		t.Fatalf("read the row of %s: %v", document.address(block), err)
	}
	return changed
}

// The questions and the decisions of c-decisions-view's issue.
const (
	viewAnswer      = "Plain HTTP until a client streams."
	viewDeclineNote = "Not mine to choose: ask the design lead."
	viewD1Words     = "REST for every service this quarter."
	viewD1Reason    = "A client that needs streaming would change it."
	// viewAskA1 is the answered ask, which shaped the first requirement r1 that the agent's d1
	// shapes too. The issue the test creates is CORE-1, the first of its project.
	viewAskA1 = ":::ask{#a1 urgency=\"med\" multiple=\"false\" shaped=\"dispatch://CORE-1/spec#b-r1\"}\nWhich transport?\n\n- REST: Plain HTTP\n- gRPC: Streaming\n:::\n"
	viewSpec  = ":::requirement{#r1}\nOne.\n:::\n\n:::requirement{#r2}\nTwo.\n:::\n\n:::requirement{#r3}\nThree.\n:::\n\n" +
		":::requirement{#r4}\nFour.\n:::\n\n" + viewAskA1 + "\n" +
		":::ask{#a3 urgency=\"low\" multiple=\"false\"}\nWhich logo?\n\n- Blue: The old one\n- Red: A new one\n:::\n\n" +
		":::ask{#a4 urgency=\"high\" multiple=\"false\"}\nWho owns the rollout?\n\n- Ops: The ops team\n- Dev: The service team\n:::\n"
)

// requireEntriesReadOwnDocuments fails unless the plan of the entries statement for issue key
// reaches every node and tuple it reads by its own key: each read of graph_nodes and of
// graph_tuples is an index scan whose condition binds a node's id (id, from_id or to_id), so the
// statement reads no node of another issue - no scan of graph_nodes, nor of all the store's
// decision nodes - and no table is scanned whole. The tables are analyzed and sequential scans,
// bitmap scans, hash joins and merge joins turned off, so the plan is the one the statement's own
// predicates admit rather than one a table of a few rows makes cheaper.
func requireEntriesReadOwnDocuments(t *testing.T, database *store.Store, key string) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `analyze artifacts, asks, graph_nodes, graph_tuples, document_blocks, block_refs, document_block_versions`); err != nil {
		t.Fatalf("analyze the tables the plan reads: %v", err)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("open the transaction the plan is read in: %v", err)
	}
	defer tx.Rollback(ctx)
	for _, setting := range []string{"enable_seqscan", "enable_bitmapscan", "enable_hashjoin", "enable_mergejoin"} {
		if _, err := tx.Exec(ctx, `set local `+setting+` = off`); err != nil {
			t.Fatalf("turn %s off: %v", setting, err)
		}
	}
	rows, err := tx.Query(ctx, `explain (costs off) `+decisionEntriesRead(), key,
		refs.VisibilityArg(model.Actor{Kind: "user", ID: "alice"}), entryFactKinds)
	if err != nil {
		t.Fatalf("read the plan of the entries statement: %v", err)
	}
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan a line of the plan: %v", err)
		}
		plan = append(plan, line)
	}
	rows.Close()
	text := strings.Join(plan, "\n")
	t.Logf("the plan of the entries statement:\n%s", text)
	if strings.Contains(text, "Seq Scan") {
		t.Errorf("the entries statement scans a table past the issue's own rows:\n%s", text)
	}
	for index, line := range plan {
		if !strings.Contains(line, " on graph_nodes") && !strings.Contains(line, " on graph_tuples") {
			continue
		}
		condition := ""
		if index+1 < len(plan) {
			condition = plan[index+1]
		}
		if !strings.Contains(condition, "Index Cond:") ||
			!strings.Contains(condition, "(id = ") && !strings.Contains(condition, "(from_id = ") && !strings.Contains(condition, "(to_id = ") {
			t.Errorf("the plan reads %q with %q, which binds no node's id", strings.TrimSpace(line), strings.TrimSpace(condition))
		}
	}
}

// c-decisions-view. The issue's spec holds four requirements, an ask bob answers (a1), an ask
// alice declines with a note (a3), an open ask (a4), and a decision the agent adds (d9). The
// second document, the agent's notes.md, holds an open ask (a2), a copy of the answered ask
// (a1), and a decision of its own under the spec's decision's block id (d9). The agent's
// choices.md holds the decision that shaped three requirements (d1). A second issue's spec of
// 2,001 blocks, above docs.deferBlockIndexBlocks, holds one decision whose block index never runs.
func TestTheDecisionsReadAnswersEveryDecisionInOrder(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	documents := deps.Docs.(*docs.Service)
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", viewSpec)
	if issue.Key != "CORE-1" {
		t.Fatalf("the seeded issue is %s, want CORE-1, which the answered ask's shaped link names", issue.Key)
	}
	spec := readDecisionDocument(t, database, issue.Key, "spec")
	pointer := func(block string) string { return "dispatch://" + issue.Key + "/spec#b-" + block }
	settleIssue(t, documents, issue.Key)
	answered, declined, openInSpec := blockAskID(t, handler, issue.Key, "a1"), blockAskID(t, handler, issue.Key, "a3"), blockAskID(t, handler, issue.Key, "a4")
	answerDecisionBlock(t, handler, answered, "bob", []string{"REST"}, viewAnswer, nil)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+declined+"/decline", map[string]any{"note": viewDeclineNote}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("alice declines a3: status=%d body=%s", response.Code, response.Body.String())
	}
	agentEdit(t, handler, spec.id, latestVersion(t, database, spec.id).Number+1,
		map[string]string{"op": "insert", "after": "end", "markdown": ":::decision{#d9}\nThe spec's own choice: one region.\n:::\n"})
	agentUploadAs(t, handler, issue.Key, "notes.md",
		":::ask{#a2 urgency=\"high\" multiple=\"false\"}\nShip on Friday?\n\n- Yes: Ship it\n- No: Wait a week\n:::\n\n"+viewAskA1+"\n"+
			":::decision{#d9 uncertainty=\"medium\" uncertainty_reason=\"A second region's cost would change it.\"}\nThe notes' own choice: two regions.\n:::\n", nil)
	settleIssue(t, documents, issue.Key)
	notes := readDecisionDocument(t, database, issue.Key, "notes-md")
	openInNotes := blockAskID(t, handler, issue.Key, "a2")
	choicesText := func(words, readings string) string {
		return "::::decision{#d1 shaped=\"" + pointer("r1") + " " + pointer("r2") + " " + pointer("r3") +
			"\" uncertainty=\"low\" uncertainty_reason=\"" + viewD1Reason + "\"}\n" + words + "\n" + readings + "::::\n"
	}
	agentUploadAs(t, handler, issue.Key, "choices.md", choicesText(viewD1Words, ""), nil)
	choices := readDecisionDocument(t, database, issue.Key, "choices-md")

	// The fixture holds the copy it needs: notes.md's a1 opened no ask of its own and names its
	// source's node by its copied_from.
	if copied := currentTies(t, database, notes.address("a1"), graphstore.TupleCopiedFrom); len(copied) != 1 || copied[0].ToKind != graphstore.KindAsk || copied[0].ToID != answered {
		t.Fatalf("notes.md's a1 states the copied_from %s, want one, to the answered ask %s: the copy this case reads", tuplesText(copied), answered)
	}

	entries := readEntries(t, handler, issue.Key)
	byNode := entriesByNode(entries)
	t.Logf("the Decisions read answers %d entries, keyed by node id: %v", len(entries), slices.Collect(func(yield func(string) bool) {
		for _, entry := range entries {
			if !yield(entryKey(entry)) {
				return
			}
		}
	}))

	// Every open ask comes before every other entry, and no other order is asserted.
	opens := map[string]bool{graphstore.KindAsk + ":" + openInSpec: true, graphstore.KindAsk + ":" + openInNotes: true}
	closedSeen := ""
	for _, entry := range entries {
		if opens[entryKey(entry)] {
			if closedSeen != "" {
				t.Errorf("the open ask %s comes after %s, which is no open ask: every open ask comes first", entryKey(entry), closedSeen)
			}
			continue
		}
		if closedSeen == "" {
			closedSeen = entryKey(entry)
		}
	}
	for node := range opens {
		if entry := requireEntry(t, entries, "an open ask", node); entry.AnswerState == nil || *entry.AnswerState != "open" {
			t.Errorf("the open ask %s reads the answer state %v, want open", node, entry.AnswerState)
		}
	}

	// The answered ask: its question, its options and the person's answer, character for
	// character as the store holds them, and the answer once in the whole read.
	ask := requireEntry(t, entries, "the answered ask", graphstore.KindAsk+":"+answered)
	// The answered ask shaped r1, through the shaped tuple its ask node holds: its entry answers
	// that one spec node, from its own block.
	if targets := shapedTargets(ask); !slices.Equal(targets, []string{spec.address("r1")}) || ask.Shaped[0].FromBlock != "a1" ||
		ask.Shaped[0].Event == nil {
		t.Errorf("the answered ask's entry answers the shaped links %s, want one, to %s, from its block a1, with the tuple's event",
			entryJSON(t, ask.Shaped), spec.address("r1"))
	}
	// The options are compared with the ones the fixture wrote into a1's block, never with the
	// asks row's column, which is where the read itself takes them from.
	wantOptions := []model.AskOption{{Label: "REST", Description: "Plain HTTP"}, {Label: "gRPC", Description: "Streaming"}}
	var question string
	if err := database.Pool.QueryRow(ctx, `select question from asks where id = $1::uuid`, answered).Scan(&question); err != nil {
		t.Fatalf("read the answered ask's row: %v", err)
	}
	storedAnswer, stated, err := graphstore.CurrentValue(ctx, database.Pool, graphstore.KindAsk, answered, graphstore.TupleAnswer)
	if err != nil || !stated {
		t.Fatalf("read the answered ask's answer off its node: stated %t, %v", stated, err)
	}
	if ask.Question == nil || *ask.Question != question || question != "Which transport?" || !reflect.DeepEqual(ask.Options, wantOptions) ||
		ask.Answer == nil || *ask.Answer != storedAnswer || storedAnswer != viewAnswer {
		t.Errorf("the answered ask's entry = %s, want the question %q as the store holds it, the options %+v the fixture wrote and the answer %q",
			entryJSON(t, ask), question, wantOptions, storedAnswer)
	}
	if held := strings.Count(entryJSON(t, entries), viewAnswer); held != 1 {
		t.Errorf("the read holds the answer %q %d times, want once, in the answered ask's answer:\n%s", viewAnswer, held, entryJSON(t, entries))
	}
	// The copy in the second document is no entry of its own.
	askEntries := 0
	for _, entry := range entries {
		if entry.Node == (decisionEntryNode{Kind: graphstore.KindAsk, ID: answered}) {
			askEntries++
		}
		if entry.Document != nil && entry.Document.ArtifactID == notes.id && entry.Document.BlockID == "a1" {
			t.Errorf("the copy of the answered ask in notes.md is an entry of its own: %s", entryJSON(t, entry))
		}
	}
	if askEntries != 1 {
		t.Errorf("the answered ask is %d entries, want one: its copy is none", askEntries)
	}

	// The declined ask: an entry of its own, with the person's note, marked declined, and no
	// field of it an answer.
	refusal, found := byNode[graphstore.KindAsk+":"+declined]
	if !found || !refusal.Declined || refusal.Kind != "declined" || refusal.DeclinedNote == nil || *refusal.DeclinedNote != viewDeclineNote ||
		refusal.Answer != nil || refusal.Selected != nil || refusal.AnsweredBy != nil || refusal.AnsweredAt != nil || refusal.DecidedBy != nil {
		t.Errorf("the declined ask's entry (found %t) = %s, want one marked declined with the note %q and no answer",
			found, entryJSON(t, refusal), viewDeclineNote)
	}
	t.Logf("the declined ask's entry as the read answers it: %s", entryJSON(t, refusal))

	// The agent's decision: its text, its uncertainty, its reason and its author, the far end
	// of the stored decided_by tuple, the agent's session whose credential wrote it, which
	// stamped no title and so carries none.
	d1 := requireEntry(t, entries, "the agent's decision", refs.BlockNodeKind+":"+choices.address("d1"))
	decidedBy := requireDecidedByNode(t, database, "the agent's decision", choices.address("d1"), graphstore.KindSession, decidingSession)
	if !strings.Contains(d1.Text, viewD1Words) || d1.Kind != "agent" || d1.Uncertainty == nil || *d1.Uncertainty != "low" ||
		d1.UncertaintyReason == nil || *d1.UncertaintyReason != viewD1Reason || d1.DecidedBy == nil ||
		*d1.DecidedBy != (decisionDecider{Kind: decidedBy.ToKind, ID: decidedBy.ToID}) {
		t.Errorf("the agent's decision's entry = %s, want its words %q, uncertainty low, the reason %q and the author %s:%s",
			entryJSON(t, d1), viewD1Words, viewD1Reason, decidedBy.ToKind, decidedBy.ToID)
	}

	// Each entry names the node that holds it, and the ask whose block is in notes.md names a node
	// of that document.
	for _, entry := range entries {
		switch entry.Node.Kind {
		case graphstore.KindAsk:
			if !strings.HasSuffix(entry.Ref, "/ask/"+entry.Node.ID) {
				t.Errorf("the ask entry %s links %q, want the ask's own address", entryKey(entry), entry.Ref)
			}
		case refs.BlockNodeKind:
			if entry.Document == nil || entry.Document.Block != entry.Node.ID {
				t.Errorf("the decision entry %s names the place %s, want its own node", entryKey(entry), entryJSON(t, entry.Document))
			}
		default:
			t.Errorf("the entry %s is held by a node of kind %q, want an ask or a decision's block", entryKey(entry), entry.Node.Kind)
		}
	}
	inNotes := requireEntry(t, entries, "the ask notes.md holds", graphstore.KindAsk+":"+openInNotes)
	if inNotes.Document == nil || inNotes.Document.ArtifactID != notes.id || inNotes.Document.Block != notes.address("a2") {
		t.Errorf("the ask whose block is in notes.md names the place %s, want notes.md's block %s and not the spec's", entryJSON(t, inNotes.Document), notes.address("a2"))
	}

	// Two decisions under one block id, in the spec and in notes.md, are two entries; a Keep on
	// the spec's leaves the other waiting for review.
	specD9 := requireEntry(t, entries, "the spec's d9", refs.BlockNodeKind+":"+spec.address("d9"))
	notesD9 := requireEntry(t, entries, "notes.md's d9", refs.BlockNodeKind+":"+notes.address("d9"))
	if specD9.Node == notesD9.Node || !specD9.WaitingReview || !notesD9.WaitingReview {
		t.Fatalf("the two d9 decisions read %s and %s, want two nodes, each waiting for review", entryJSON(t, specD9), entryJSON(t, notesD9))
	}
	keepBlock(t, handler, spec.id, "d9", specD9.TextSHA256)
	entries = readEntries(t, handler, issue.Key)
	if kept, other := requireEntry(t, entries, "the spec's d9", entryKey(specD9)), requireEntry(t, entries, "notes.md's d9", entryKey(notesD9)); kept.WaitingReview || !other.WaitingReview {
		t.Errorf("after a Keep of the spec's d9 it reads waiting_review %t and notes.md's d9 %t, want false and true", kept.WaitingReview, other.WaitingReview)
	}

	// One decision under three spec nodes is one entry, with one node id, answering the three.
	requirements := []string{spec.address("r1"), spec.address("r2"), spec.address("r3")}
	var shapers []string
	for _, entry := range entries {
		if entry.Node.ID == choices.address("d1") {
			shapers = append(shapers, fmt.Sprintf("%s shaping %v", entryKey(entry), shapedTargets(entry)))
		}
	}
	if len(shapers) != 1 {
		t.Fatalf("the Decisions read answers %d entries for the decision that shaped three requirement nodes: %v; want one, with the node id %s",
			len(shapers), shapers, choices.address("d1"))
	}
	if d1 = requireEntry(t, entries, "the agent's decision", entryKey(d1)); !slices.Equal(shapedTargets(d1), requirements) {
		t.Fatalf("the decision answers the spec nodes %v, want the three it shaped, %v", shapedTargets(d1), requirements)
	}
	for _, link := range d1.Shaped {
		if link.FromBlock != "d1" {
			t.Errorf("the link %s of %s names the block %q it is written from, want d1, the decision's own block", link.To.ID, entryKey(d1), link.FromBlock)
		}
	}
	t.Logf("the agent's decision's entry as the read answers it: %s", entryJSON(t, d1))
	requireEntriesReadOwnDocuments(t, database, issue.Key)

	// A link carries two facts (model.DecisionLink). changed_since_tie goes true when the
	// requirement's words change after the tie; stale, the release before's rule, goes true when
	// the decision's own words change after the tie and the requirement has not changed since they
	// did. The decision's own version moves only with its own words.
	if link := shapedLink(t, d1, spec.address("r1")); link.Stale || link.ChangedSinceTie || link.Event == nil || link.TextChangedEvent == nil {
		t.Fatalf("the link %s of %s reads %s before the requirement changed, want stale and changed_since_tie false, with both events", spec.address("r1"), entryKey(d1), entryJSON(t, link))
	}
	wroteD1, wroteD1At := d1.ChangedVersion, d1.ChangedAt
	if wroteD1 != 1 || wroteD1At == nil || !wroteD1At.Equal(versionCreatedAt(t, database, choices.id, 1)) {
		t.Fatalf("the decision reads changed_version %d at %v, want version 1, which wrote it, at that version's created_at", wroteD1, wroteD1At)
	}
	reworded := editSpec(t, handler, issue.Key, []map[string]any{{"op": "replace", "find": "One.", "with": "One, rewritten."}})
	d1 = requireEntry(t, readEntries(t, handler, issue.Key), "the agent's decision", entryKey(d1))
	if link := shapedLink(t, d1, spec.address("r1")); !link.ChangedSinceTie || link.Stale || link.TextChangedEvent == nil || link.Event == nil || *link.TextChangedEvent <= *link.Event {
		t.Errorf("the link %s of %s reads %s after version %d rewrote the requirement, want changed_since_tie true, its text changed after the tie, and stale false, the decision's words unchanged since the tie",
			spec.address("r1"), entryKey(d1), entryJSON(t, link), reworded.Number)
	}
	if link := shapedLink(t, d1, spec.address("r2")); link.Stale || link.ChangedSinceTie {
		t.Errorf("the link %s reads %s after a version that left it and the decision alone, want stale and changed_since_tie false", spec.address("r2"), entryJSON(t, link))
	}
	if d1.ChangedVersion != wroteD1 || d1.ChangedAt == nil || !d1.ChangedAt.Equal(*wroteD1At) {
		t.Errorf("after the requirement's words changed the decision reads changed_version %d at %v, want %d at %v, unchanged",
			d1.ChangedVersion, d1.ChangedAt, wroteD1, wroteD1At)
	}
	agentUploadAs(t, handler, issue.Key, "choices.md", choicesText("REST for every public service this quarter.", ""), nil)
	ownWords := latestVersion(t, database, choices.id)
	view := readIssueDecisions(t, handler, issue.Key)
	d1 = requireEntry(t, view.Entries, "the agent's decision", entryKey(d1))
	if d1.ChangedVersion != ownWords.Number || d1.ChangedAt == nil || !d1.ChangedAt.Equal(versionCreatedAt(t, database, choices.id, ownWords.Number)) {
		t.Errorf("after its own words changed in version %d the decision reads changed_version %d at %v, want that version and its created_at %v",
			ownWords.Number, d1.ChangedVersion, d1.ChangedAt, versionCreatedAt(t, database, choices.id, ownWords.Number))
	}
	// The decision reworded after its tie, with r2's words unchanged since the tie: stale, and not
	// changed since the tie. r1, rewritten after the tie and before the decision's new words, is
	// both.
	if link := shapedLink(t, d1, spec.address("r2")); !link.Stale || link.ChangedSinceTie {
		t.Errorf("the link %s of %s reads %s after version %d reworded the decision, want stale true, the decision changed after the tie and the requirement not since, and changed_since_tie false",
			spec.address("r2"), entryKey(d1), entryJSON(t, link), ownWords.Number)
	}
	if link := shapedLink(t, d1, spec.address("r1")); !link.Stale || !link.ChangedSinceTie {
		t.Errorf("the link %s of %s reads %s after version %d reworded the decision, want stale and changed_since_tie true",
			spec.address("r1"), entryKey(d1), entryJSON(t, link), ownWords.Number)
	}
	// The same links read through the old decisions member read the same two facts.
	for _, link := range d1.Shaped {
		if old := decisionMemberLink(t, view, d1.Ref, link.To.ID); old.Stale != link.Stale || old.ChangedSinceTie != link.ChangedSinceTie {
			t.Errorf("the link %s of %s reads stale %t and changed_since_tie %t in the decisions member and %t and %t in entries, want the same",
				link.To.ID, d1.Ref, old.Stale, old.ChangedSinceTie, link.Stale, link.ChangedSinceTie)
		}
	}
	// A reading written under it moves its block's row and none of its words: the version its
	// words last changed in stands.
	agentUploadAs(t, handler, issue.Key, "choices.md", choicesText("REST for every public service this quarter.",
		"\n:::reading{#g1}\nThe agent reads it as REST for the public services.\n:::\n"), nil)
	if moved := rowChangedVersion(t, database, choices, "d1"); moved <= ownWords.Number {
		t.Fatalf("the reading's version left d1's row at version %d; this case needs the row moved past %d", moved, ownWords.Number)
	}
	read := requireEntry(t, readEntries(t, handler, issue.Key), "the agent's decision", entryKey(d1))
	if read.ChangedVersion != ownWords.Number || read.ChangedAt == nil || !read.ChangedAt.Equal(*d1.ChangedAt) || read.TextSHA256 != d1.TextSHA256 {
		t.Errorf("after a reading was written under it the decision reads changed_version %d at %v summed %s, want %d at %v summed %s: a reading never changes the decision",
			read.ChangedVersion, read.ChangedAt, read.TextSHA256, ownWords.Number, d1.ChangedAt, d1.TextSHA256)
	}
	// The reading edited, and then removed, moves the row again and none of the words.
	for _, step := range []struct{ what, reading string }{
		{"edited", "\n:::reading{#g1}\nThe agent reads it as REST for every public service.\n:::\n"},
		{"removed", ""},
	} {
		agentUploadAs(t, handler, issue.Key, "choices.md", choicesText("REST for every public service this quarter.", step.reading), nil)
		read := requireEntry(t, readEntries(t, handler, issue.Key), "the agent's decision", entryKey(d1))
		if read.ChangedVersion != ownWords.Number || read.ChangedAt == nil || !read.ChangedAt.Equal(*d1.ChangedAt) || read.TextSHA256 != d1.TextSHA256 {
			t.Errorf("after its reading was %s the decision reads changed_version %d at %v summed %s, want %d at %v summed %s",
				step.what, read.ChangedVersion, read.ChangedAt, read.TextSHA256, ownWords.Number, d1.ChangedAt, d1.TextSHA256)
		}
	}
	// The words change back to those d1 was tied at: the sum of its words at the tie is the sum as
	// they stand, so no link is stale, though the last change into those words came after the tie
	// and after r1's rewrite, and the decisions member answers the same.
	agentUploadAs(t, handler, issue.Key, "choices.md", choicesText(viewD1Words, ""), nil)
	changedBack := readIssueDecisions(t, handler, issue.Key)
	d1 = requireEntry(t, changedBack.Entries, "the agent's decision", entryKey(d1))
	if !strings.Contains(d1.Text, viewD1Words) {
		t.Fatalf("the decision reads %q after its words changed back, want %q", d1.Text, viewD1Words)
	}
	for _, link := range d1.Shaped {
		if link.Stale {
			t.Errorf("the link %s of %s reads %s after the words changed back to those it was tied at, want stale false", link.To.ID, entryKey(d1), entryJSON(t, link))
		}
		if old := decisionMemberLink(t, changedBack, d1.Ref, link.To.ID); old.Stale {
			t.Errorf("the link %s of %s reads stale in the decisions member after the words changed back, want false", link.To.ID, d1.Ref)
		}
	}
	// A reading written, edited and removed under the answered ask moves the ask block's row and
	// none of its own words, its question and its options.
	askBefore := requireEntry(t, readEntries(t, handler, issue.Key), "the answered ask", graphstore.KindAsk+":"+answered)
	if askBefore.ChangedVersion != 1 || askBefore.ChangedAt == nil || !askBefore.ChangedAt.Equal(versionCreatedAt(t, database, spec.id, 1)) {
		t.Errorf("the answered ask reads changed_version %d at %v, want version 1, which wrote its block, at its created_at", askBefore.ChangedVersion, askBefore.ChangedAt)
	}
	for _, step := range []struct{ what, reading string }{
		{"written", "The agent reads the answer as REST for every service."},
		{"edited", "The agent reads the answer as REST for every public service."},
		{"removed", ""},
	} {
		uploadAs(t, handler, issue.Key, "spec.md", withAskReading(t, documentMarkdown(t, handler, spec.id), "a1", "ga", step.reading), "alice")
		settleIssue(t, documents, issue.Key)
		if moved := rowChangedVersion(t, database, spec, "a1"); moved <= askBefore.ChangedVersion {
			t.Fatalf("the reading %s left a1's row at version %d; this case needs the row moved past %d", step.what, moved, askBefore.ChangedVersion)
		}
		read := requireEntry(t, readEntries(t, handler, issue.Key), "the answered ask", graphstore.KindAsk+":"+answered)
		want := []string{}
		if step.reading != "" {
			want = []string{spec.address("ga") + "<-" + graphstore.KindPerson + ":alice"}
		}
		if read.ChangedVersion != askBefore.ChangedVersion || read.ChangedAt == nil || !read.ChangedAt.Equal(*askBefore.ChangedAt) ||
			!slices.Equal(readingsOf(read), want) {
			t.Errorf("after a reading was %s in the answered ask it reads changed_version %d at %v with the readings %v, want %d at %v with %v",
				step.what, read.ChangedVersion, read.ChangedAt, readingsOf(read), askBefore.ChangedVersion, askBefore.ChangedAt, want)
		}
	}

	// Membership waits on no projection: a spec of 2,001 blocks, whose block index is owed and
	// never runs here, answers its one decision.
	var heavy strings.Builder
	for item := 1; item <= itemsUnderTheBlockBound; item++ {
		heavy.WriteString("- x\n")
	}
	heavy.WriteString("\n:::decision{#h1}\nThe heavy spec decides once.\n:::\n")
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "CORE", "title": "A heavy spec", "actor": sessionActor(), "spec": heavy.String(),
	})
	requireAgentWrite(t, "the agent's heavy issue", created, http.StatusCreated)
	heavyIssue := decodeBody[model.Issue](t, created)
	heavySpec := readDecisionDocument(t, database, heavyIssue.Key, "spec")
	text := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+heavySpec.id+"/text?blocks=true", nil, "alice")
	if text.Code != http.StatusOK {
		t.Fatalf("read the heavy spec's blocks: status=%d body=%s", text.Code, text.Body.String())
	}
	if blocks := decodeBody[artifactText](t, text).Blocks; blocks == nil || len(*blocks) != blocksOverTheBound {
		t.Fatalf("the heavy spec holds %v blocks, want %d, one over the bound", blocks, blocksOverTheBound)
	}
	var rows int
	if err := database.Pool.QueryRow(ctx, `select count(*) from document_blocks where artifact_id = $1::uuid`, heavySpec.id).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("document_blocks holds %d rows of the heavy spec (%v), want none: its block index is owed", rows, err)
	}
	if indexed := blockIndexVersion(t, database, heavySpec.id); indexed != nil {
		t.Fatalf("the heavy spec's blocks are indexed at version %d, want them owed", *indexed)
	}
	heavyEntries := readEntries(t, handler, heavyIssue.Key)
	if len(heavyEntries) != 1 || heavyEntries[0].Node != (decisionEntryNode{Kind: refs.BlockNodeKind, ID: heavySpec.address("h1")}) ||
		!strings.Contains(heavyEntries[0].Text, "The heavy spec decides once.") || heavyEntries[0].ChangedVersion != 1 {
		t.Fatalf("the heavy spec's Decisions read answers %s, want its one decision %s, its words, at version 1", entryJSON(t, heavyEntries), heavySpec.address("h1"))
	}
}

// A session that decided is named by the title it stamped once Envoy's registry no longer lists
// it: the Decisions read's decided_by carries title, the newest title the session stamped on
// anything it wrote (delivery.ResolveSessionTitles), where its write stamped one. A session whose
// writes stamped none, and a person, carry no title at all. The decided_by members are read as
// the response holds them, so an empty title the answer spelled would fail as a title it held.
func TestTheDecisionsReadNamesASessionDeciderByTheTitleItStamped(t *testing.T) {
	t.Parallel()
	const (
		titledSession = "session-titled-0123456789"
		title         = "Legion planner · CORE-1"
	)
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", ":::decision{#p1}\nAlice's spec decides.\n:::\n")
	agentUploadAs(t, handler, issue.Key, "titled.md", ":::decision{#t1}\nThe titled session decides.\n:::\n",
		map[string]any{"actor": map[string]any{"kind": "session", "id": titledSession, "origin": map[string]any{"session_title": title}}})
	agentUploadAs(t, handler, issue.Key, "untitled.md", ":::decision{#u1}\nThe untitled session decides.\n:::\n", nil)

	person := readDecisionDocument(t, database, issue.Key, "spec").address("p1")
	titled := readDecisionDocument(t, database, issue.Key, "titled-md").address("t1")
	untitled := readDecisionDocument(t, database, issue.Key, "untitled-md").address("u1")
	requireDecidedBy(t, database, "the person's decision", person, "alice")
	requireDecidedByNode(t, database, "the titled session's decision", titled, graphstore.KindSession, titledSession)
	requireDecidedByNode(t, database, "the untitled session's decision", untitled, graphstore.KindSession, decidingSession)

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/decisions", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/issues/%s/decisions: status=%d body=%s", issue.Key, response.Code, response.Body.String())
	}
	read := decodeBody[struct {
		Entries []struct {
			Node      decisionEntryNode `json:"node"`
			DecidedBy map[string]any    `json:"decided_by"`
		} `json:"entries"`
	}](t, response)
	deciders := map[string]map[string]any{}
	for _, entry := range read.Entries {
		if entry.Node.Kind == refs.BlockNodeKind {
			deciders[entry.Node.ID] = entry.DecidedBy
		}
	}
	for _, want := range []struct {
		what, node string
		decider    map[string]any
	}{
		{"the titled session's decision", titled, map[string]any{"kind": graphstore.KindSession, "id": titledSession, "title": title}},
		{"the untitled session's decision", untitled, map[string]any{"kind": graphstore.KindSession, "id": decidingSession}},
		{"the person's decision", person, map[string]any{"kind": graphstore.KindPerson, "id": "alice"}},
	} {
		decider, found := deciders[want.node]
		if !found || !reflect.DeepEqual(decider, want.decider) {
			t.Errorf("%s (%s): the read answers the decided_by %v (an entry %t), want %v", want.what, want.node, decider, found, want.decider)
		}
	}
}

// A decision whose decided_by names a person is that person's and waits for nobody's review
// (spec R4: the author the read answers is the one its decided_by tuple holds), and an agent's,
// whose decided_by names its session, waits until a person keeps it. Both are `decision` blocks
// nobody kept, so no Keep tells them apart: the person's is the spec's p1, which alice's
// credential wrote, and the agent's the a1 the agent's session uploaded. Each stays an entry of
// kind `agent`, the node's shape. The members are read as the response holds them, so an answer
// that left waiting_review out fails as neither value.
func TestADecisionAPersonDecidedWaitsForNoReviewAndAnAgentsDoes(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", ":::decision{#p1}\nAlice's spec decides.\n:::\n")
	agentUploadAs(t, handler, issue.Key, "agent.md", ":::decision{#a1}\nThe agent decides.\n:::\n", nil)

	person := readDecisionDocument(t, database, issue.Key, "spec").address("p1")
	agent := readDecisionDocument(t, database, issue.Key, "agent-md").address("a1")
	requireDecidedBy(t, database, "the person's decision", person, "alice")
	requireDecidedByNode(t, database, "the agent's decision", agent, graphstore.KindSession, decidingSession)

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/decisions", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/issues/%s/decisions: status=%d body=%s", issue.Key, response.Code, response.Body.String())
	}
	type readEntry struct {
		Node          decisionEntryNode `json:"node"`
		Kind          string            `json:"kind"`
		DecidedBy     map[string]any    `json:"decided_by"`
		Kept          map[string]any    `json:"kept"`
		WaitingReview *bool             `json:"waiting_review"`
	}
	read := decodeBody[struct {
		Entries []readEntry `json:"entries"`
	}](t, response)
	byNode := map[string]readEntry{}
	for _, entry := range read.Entries {
		if entry.Node.Kind == refs.BlockNodeKind {
			byNode[entry.Node.ID] = entry
		}
	}
	for _, want := range []struct {
		what, node string
		decider    map[string]any
		waiting    bool
	}{
		{"the person's decision", person, map[string]any{"kind": graphstore.KindPerson, "id": "alice"}, false},
		{"the agent's decision", agent, map[string]any{"kind": graphstore.KindSession, "id": decidingSession}, true},
	} {
		entry, found := byNode[want.node]
		switch {
		case !found:
			t.Errorf("%s (%s): the read answers no entry for it", want.what, want.node)
		case entry.Kind != "agent" || entry.Kept != nil || !reflect.DeepEqual(entry.DecidedBy, want.decider):
			t.Errorf("%s (%s): the entry reads %s, want kind agent, the node's shape, no Keep and the decided_by %v",
				want.what, want.node, entryJSON(t, entry), want.decider)
		case entry.WaitingReview == nil || *entry.WaitingReview != want.waiting:
			t.Errorf("%s (%s): the entry reads %s, want waiting_review %t", want.what, want.node, entryJSON(t, entry), want.waiting)
		}
	}
}

// A reading's state of the store: the reading g2 is inserted by SQL, outside the server's write
// path, with its part_of and no authored_by.
func insertBareReading(t *testing.T, database *store.Store, address, parent string) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event, project_key)
		values ($1, $2, $3, $4, $5, '{}'::jsonb, now(), '{"kind":"user","id":"alice"}'::jsonb, 0, 'CORE')
	`, refs.BlockNodeKind, address, refs.ReadingSupertag, graphstore.VisibilityProject, graphstore.StateLive); err != nil {
		t.Fatalf("insert the reading node %s: %v", address, err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
		values ($1, $2, $3, $1, $4, now(), '{"kind":"user","id":"alice"}'::jsonb, 0, true)
	`, refs.BlockNodeKind, address, graphstore.TuplePartOf, parent); err != nil {
		t.Fatalf("insert the part_of of %s: %v", address, err)
	}
}

// decisionReadOf is what the c-reading-under-decision test reads three times of a decision: its
// words, its text sum and its Keep.
type decisionReadOf struct {
	words, sum, keptBy, keptSum string
	keptAt                      time.Time
	keptVersion                 int
}

func (read decisionReadOf) same(other decisionReadOf) bool {
	return read.words == other.words && read.sum == other.sum && read.keptBy == other.keptBy &&
		read.keptSum == other.keptSum && read.keptAt.Equal(other.keptAt) && read.keptVersion == other.keptVersion
}

func readOfEntry(entry decisionEntry) decisionReadOf {
	read := decisionReadOf{words: entry.Text, sum: entry.TextSHA256}
	if entry.KeptBy != nil {
		read.keptBy = *entry.KeptBy
	}
	if entry.KeptTextSum != nil {
		read.keptSum = *entry.KeptTextSum
	}
	if entry.Kept != nil {
		read.keptAt, read.keptVersion = entry.Kept.KeptAt, entry.Kept.ChangedVersion
	}
	return read
}

// readingsOf is the node of each reading an entry answers, with its author, written
// node<-kind:id, or node<-nobody.
func readingsOf(entry decisionEntry) []string {
	held := make([]string, 0, len(entry.Readings))
	for _, reading := range entry.Readings {
		author := "nobody"
		if reading.AuthoredBy != nil {
			author = reading.AuthoredBy.Kind + ":" + reading.AuthoredBy.ID
		}
		held = append(held, reading.Node.ID+"<-"+author)
	}
	sort.Strings(held)
	return held
}

// The questions and the decisions of c-reading-under-decision's issue.
const (
	readingAsks = ":::ask{#a2 urgency=\"med\" multiple=\"false\"}\nShip on Friday?\n\n- Yes: Ship it\n- No: Wait a week\n:::\n"
	readingA1   = "ask{#a1 urgency=\"med\" multiple=\"false\"}\nWhich transport?\n\n- REST: Plain HTTP\n- gRPC: Streaming\n"
	readingD2   = ":::decision{#d2 replaced=\"true\"}\nWe shipped gRPC first.\n:::\n"
)

// c-reading-under-decision. The issue's asks.md holds an ask bob answers (a1) and an open one
// (a2); the agent's choices.md holds its decision d1 and a replaced decision d2. Every write
// below is the agent's unless it names a person.
func TestAReadingIsAChildNodeOfTheDecisionItReads(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	documents := deps.Docs.(*docs.Service)
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "A spec.\n")
	uploadAs(t, handler, issue.Key, "asks.md", ":::"+readingA1+":::\n\n"+readingAsks, "alice")
	settleIssue(t, documents, issue.Key)
	answered, open := blockAskID(t, handler, issue.Key, "a1"), blockAskID(t, handler, issue.Key, "a2")
	answerDecisionBlock(t, handler, answered, "bob", []string{"REST"}, "REST, until a client streams.", nil)
	asks := readDecisionDocument(t, database, issue.Key, "asks-md")
	choicesText := func(words, reading string) string {
		if reading == "" {
			return ":::decision{#d1}\n" + words + "\n:::\n\n" + readingD2
		}
		return "::::decision{#d1}\n" + words + "\n\n" + reading + "::::\n\n" + readingD2
	}
	const words = "We ship REST first."
	agentUploadAs(t, handler, issue.Key, "choices.md", choicesText(words, ""), nil)
	choices := readDecisionDocument(t, database, issue.Key, "choices-md")
	d1, g1 := choices.address("d1"), choices.address("g1")
	decisionNode := refs.BlockNodeKind + ":" + d1
	readD1 := func(what string) decisionEntry {
		t.Helper()
		return requireEntry(t, readEntries(t, handler, issue.Key), what, decisionNode)
	}

	// Keep acts on the decision's own node: waiting for review, then a person keeps it.
	unkept := readD1("the agent's decision before its Keep")
	if !unkept.WaitingReview || unkept.Kept != nil {
		t.Fatalf("the agent's decision before any Keep reads %s, want it waiting for review and kept by nobody", entryJSON(t, unkept))
	}
	keepBlock(t, handler, choices.id, "d1", unkept.TextSHA256)
	keptBy := currentTies(t, database, d1, refs.TupleKeptBy)
	keptSum, keptSumHeld, err := graphstore.CurrentValue(ctx, database.Pool, refs.BlockNodeKind, d1, refs.ValueKeptTextSum)
	if err != nil || len(keptBy) != 1 || keptBy[0].ToKind != graphstore.KindPerson || keptBy[0].ToID != "alice" || !keptSumHeld || keptSum != unkept.TextSHA256 {
		t.Fatalf("alice's Keep left the kept_by %s and the kept_text_sum %q (held %t, %v), want one kept_by to alice and the decision's sum %s",
			tuplesText(keptBy), keptSum, keptSumHeld, err, unkept.TextSHA256)
	}
	before := readD1("the agent's decision after its Keep")
	if before.WaitingReview || before.Kept == nil || before.Kept.KeptBy != "alice" || before.KeptBy == nil || *before.KeptBy != "alice" {
		t.Fatalf("the kept decision reads %s, want it kept by alice and waiting for no review", entryJSON(t, before))
	}
	first := readOfEntry(before)

	// One reading of the agent's decision, written by the agent with a body that claims alice.
	agentUploadAs(t, handler, issue.Key, "choices.md",
		choicesText(words, ":::reading{#g1}\nThe agent reads it as REST for every service.\n:::\n"),
		map[string]any{"authored_by": map[string]any{"kind": "user", "id": "alice"}})
	requireSupertag(t, database, g1, refs.ReadingSupertag)
	parents := currentTies(t, database, g1, graphstore.TuplePartOf)
	if len(parents) != 1 || parents[0].ToKind != refs.BlockNodeKind || parents[0].ToID != d1 {
		var found []string
		for _, parent := range parents {
			found = append(found, parent.ToKind+":"+parent.ToID)
		}
		t.Fatalf("the reading states the parent %v, want one, the decision %s", found, decisionNode)
	}
	out, err := graphstore.CurrentOut(ctx, database.Pool, refs.BlockNodeKind, g1, nil)
	if err != nil {
		t.Fatalf("read what the reading states: %v", err)
	}
	for _, tuple := range out {
		if tuple.ToID == d1 && tuple.Kind != graphstore.TuplePartOf {
			t.Errorf("the reading points at the decision through %s as well as its part_of: the part_of carries the tie", tuple.Kind)
		}
		if tuple.Kind == graphstore.TupleDecidedBy {
			t.Errorf("the reading holds the decided_by %s:%s, which R1 keeps for who decided", tuple.ToKind, tuple.ToID)
		}
	}
	var attributes string
	if err := database.Pool.QueryRow(ctx, `select attributes::text from graph_nodes where kind = $1 and id = $2`, refs.BlockNodeKind, g1).Scan(&attributes); err != nil || strings.Contains(attributes, d1) {
		t.Errorf("the reading's node holds the attributes %s (%v), want none naming the decision", attributes, err)
	}
	authors := currentTies(t, database, g1, graphstore.TupleAuthoredBy)
	if len(authors) != 1 || authors[0].ToKind != graphstore.KindSession || authors[0].ToID != decidingSession {
		t.Errorf("the reading states the authored_by %s, want one, the agent's session %s, whose credential the write proved, and not alice whom the body claimed",
			tuplesText(authors), decidingSession)
	}
	if second := readOfEntry(readD1("the agent's decision with its reading")); !second.same(first) {
		t.Errorf("the decision's words, sum and Keep read %+v once the reading hangs under it, and %+v before: a reading never changes the decision", second, first)
	}

	// The Decisions read answers the reading under that decision alone, with its author.
	entries := readEntries(t, handler, issue.Key)
	for _, entry := range entries {
		want := []string{}
		if entryKey(entry) == decisionNode {
			want = []string{g1 + "<-" + graphstore.KindSession + ":" + decidingSession}
		}
		if held := readingsOf(entry); !slices.Equal(held, want) {
			t.Errorf("the entry %s answers the readings %v, want %v", entryKey(entry), held, want)
		}
	}

	// A reading inserted by SQL with its part_of and no authored_by is answered with no author.
	g2 := choices.address("g2")
	insertBareReading(t, database, g2, d1)
	t.Logf("inserted the reading %s into graph_nodes and graph_tuples by SQL, with its part_of to %s and no authored_by: the state a row written outside the server's write path leaves", g2, d1)
	if held, want := readingsOf(readD1("the agent's decision with a reading no write path wrote")),
		[]string{g1 + "<-" + graphstore.KindSession + ":" + decidingSession, g2 + "<-nobody"}; !slices.Equal(held, want) {
		t.Errorf("the decision answers the readings %v, want %v: the bare reading with no author, neither left out nor given one", held, want)
	}

	// A reading inside the person's answered ask, after its own content: its parent is the ask's
	// node, and the ask reads as it did.
	askNode := graphstore.KindAsk + ":" + answered
	answeredBefore := requireEntry(t, readEntries(t, handler, issue.Key), "the answered ask", askNode)
	agentUploadAs(t, handler, issue.Key, "asks.md",
		"::::"+readingA1+"\n:::reading{#g3}\nThe agent reads the answer as REST for every service.\n:::\n::::\n\n"+readingAsks, nil)
	g3 := asks.address("g3")
	requireTypedNode(t, database, "the reading in the answered ask", g3, refs.ReadingSupertag, graphstore.KindAsk, answered)
	settleIssue(t, documents, issue.Key)
	answeredAfter := requireEntry(t, readEntries(t, handler, issue.Key), "the answered ask", askNode)
	if held := readingsOf(answeredAfter); !slices.Equal(held, []string{g3 + "<-" + graphstore.KindSession + ":" + decidingSession}) {
		t.Errorf("the answered ask answers the readings %v, want the reading %s, by the agent", held, g3)
	}
	t.Logf("the answered ask's entry with its reading as the read answers it: %s", entryJSON(t, answeredAfter))
	if !reflect.DeepEqual(answeredAfter.Question, answeredBefore.Question) || !reflect.DeepEqual(answeredAfter.Options, answeredBefore.Options) ||
		!reflect.DeepEqual(answeredAfter.Answer, answeredBefore.Answer) || !reflect.DeepEqual(answeredAfter.Selected, answeredBefore.Selected) ||
		answeredAfter.AnswerState == nil || *answeredAfter.AnswerState != "answered" || answeredAfter.WaitingReview {
		t.Errorf("the answered ask reads %s once a reading hangs in it and settlement ran, and read %s before: its question, options and answer stand",
			entryJSON(t, answeredAfter), entryJSON(t, answeredBefore))
	}

	// A reading inside the open ask, which holds no answer, is refused and writes no version.
	asksVersion := latestVersion(t, database, asks.id).Number
	refused := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"name": "asks.md", "actor": sessionActor(), "precondition": uploadBase(t, handler, asks.id),
		"content": "::::" + readingA1 + "\n:::reading{#g3}\nThe agent reads the answer as REST for every service.\n:::\n::::\n\n" +
			"::::ask{#a2 urgency=\"med\" multiple=\"false\"}\nShip on Friday?\n\n- Yes: Ship it\n- No: Wait a week\n\n:::reading{#g4}\nThe agent reads it as shipping.\n:::\n::::\n",
	})
	if refused.Code < http.StatusBadRequest || !strings.Contains(refused.Body.String(), `\"g4\"`) ||
		!strings.Contains(refused.Body.String(), "dispatch://"+issue.Key+"/ask/"+open) {
		t.Errorf("a reading in the open ask answered status=%d body=%s, want a refusal naming the reading g4 and the ask dispatch://%s/ask/%s",
			refused.Code, refused.Body.String(), issue.Key, open)
	}
	if after := latestVersion(t, database, asks.id).Number; after != asksVersion {
		t.Errorf("the refused reading wrote version %d of asks.md, which stood at version %d", after, asksVersion)
	}

	// The reading is in no document of its own: the issue holds the documents the fixture made.
	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/artifacts", nil, "alice")
	var slugs []string
	for _, artifact := range decodeBody[[]artifactRead](t, listed) {
		slugs = append(slugs, artifact.Slug)
	}
	sort.Strings(slugs)
	if want := []string{"asks-md", "choices-md", "spec"}; !slices.Equal(slugs, want) {
		t.Errorf("the issue lists the documents %v, want %v: a reading sits in no document of its own", slugs, want)
	}

	// The reading goes, and the decision's words, sum and Keep read as they did before it.
	agentUploadAs(t, handler, issue.Key, "choices.md", choicesText(words, ""), nil)
	if third := readOfEntry(readD1("the agent's decision after its reading went")); !third.same(first) {
		t.Errorf("the decision's words, sum and Keep read %+v after its reading went, and %+v before it was written", third, first)
	}

	// Its own words change: it waits for review again, and a Keep of the old sum is refused.
	agentUploadAs(t, handler, issue.Key, "choices.md", choicesText("We ship gRPC first.", ""), nil)
	if changed := readD1("the agent's decision after its words changed"); !changed.WaitingReview || changed.TextSHA256 == first.sum {
		t.Errorf("the decision reads %s after its words changed, want a new sum and waiting for review again", entryJSON(t, changed))
	}
	keptHistory := func() (int, int) {
		t.Helper()
		by, err := graphstore.History(ctx, database.Pool, refs.BlockNodeKind, d1, refs.TupleKeptBy)
		if err != nil {
			t.Fatalf("read d1's kept_by history: %v", err)
		}
		sums, err := graphstore.History(ctx, database.Pool, refs.BlockNodeKind, d1, refs.ValueKeptTextSum)
		if err != nil {
			t.Fatalf("read d1's kept_text_sum history: %v", err)
		}
		return len(by), len(sums)
	}
	byBefore, sumsBefore := keptHistory()
	expectRefusal(t, "a Keep of the sum d1 had before its words changed", dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+choices.id+"/blocks/d1/keep", map[string]any{"text_sha256": first.sum}, "alice"),
		http.StatusConflict, "DECISION_CHANGED")
	if byAfter, sumsAfter := keptHistory(); byAfter != byBefore || sumsAfter != sumsBefore {
		t.Errorf("the refused Keep wrote: d1 holds %d kept_by and %d kept_text_sum tuples, and held %d and %d", byAfter, sumsAfter, byBefore, sumsBefore)
	}

	// A decision marked replaced waits for no review and names no successor.
	replaced := requireEntry(t, readEntries(t, handler, issue.Key), "the replaced decision", refs.BlockNodeKind+":"+choices.address("d2"))
	if !replaced.Replaced || replaced.WaitingReview || strings.Contains(entryJSON(t, replaced), d1) {
		t.Errorf("the replaced decision reads %s, want it replaced, waiting for no review, and naming no decision beside it", entryJSON(t, replaced))
	}
	successors, err := graphstore.CurrentOut(ctx, database.Pool, refs.BlockNodeKind, choices.address("d2"), nil)
	if err != nil {
		t.Fatalf("read what the replaced decision states: %v", err)
	}
	for _, tuple := range successors {
		if tuple.ToKind == refs.BlockNodeKind && tuple.ToID == d1 {
			t.Errorf("the replaced decision names the decision %s through %s", d1, tuple.Kind)
		}
	}

	// Through `dispatch doc-edit`, run as this tree's CLI with the agent's bearer against this
	// test's own server, the agent inserts a reading into a person's answered ask of a spec a person
	// approved, with the insert whose into names the ask's block. The write is taken, the Decisions
	// read answers the reading under the ask with the agent as its author, and the spec's approval
	// still reads approved at the version the write stored.
	approvedIssue := createSpecIssue(t, handler, "CORE", "An approved spec.\n\n"+
		":::ask{#s1 urgency=\"med\" multiple=\"false\"}\nWhich store holds the queue?\n\n- Postgres: The one we run\n- Redis: The faster one\n:::\n")
	settleIssue(t, documents, approvedIssue.Key)
	storeAsk := blockAskID(t, handler, approvedIssue.Key, "s1")
	answerDecisionBlock(t, handler, storeAsk, "bob", []string{"Postgres"}, "", nil)
	spec := readDecisionDocument(t, database, approvedIssue.Key, "spec")
	approval := reviewDocument(t, handler, spec.id, "alice", "approved", "")
	insert, err := json.Marshal([]map[string]string{{
		"op": "insert", "into": "block:s1",
		"markdown": ":::reading{#g5}\nThe agent reads the answer as Postgres for every queue.\n:::\n",
	}})
	if err != nil {
		t.Fatalf("encode the insert: %v", err)
	}
	served := httptest.NewServer(handler)
	t.Cleanup(served.Close)
	edited := runDispatchCLI(t, served.URL, "doc-edit", "--issue", approvedIssue.Key, "--ops-json", string(insert))
	storeAskRef := "dispatch://" + approvedIssue.Key + "/ask/" + storeAsk
	if written := fmt.Sprintf("Applied 1 ops (version %d)", approval.Version+1); edited.code != 0 || !strings.Contains(edited.stdout, written) {
		t.Fatalf("dispatch doc-edit of a reading into the answered ask %s, block s1: %s\nwant it taken, printing %q", storeAskRef, edited, written)
	}
	t.Logf("dispatch doc-edit, run as this tree's CLI with the agent's bearer, printed:\n%s", edited.stdout)
	settleIssue(t, documents, approvedIssue.Key)
	g5 := spec.address("g5")
	requireTypedNode(t, database, "the reading the CLI inserted into the answered ask", g5, refs.ReadingSupertag, graphstore.KindAsk, storeAsk)
	underAsk := requireEntry(t, readEntries(t, handler, approvedIssue.Key), "the answered ask of the approved spec", graphstore.KindAsk+":"+storeAsk)
	if held, want := readingsOf(underAsk), []string{g5 + "<-" + graphstore.KindSession + ":" + decidingSession}; !slices.Equal(held, want) {
		t.Errorf("the answered ask %s answers the readings %v after the CLI's insert, want %v, by the agent", storeAskRef, held, want)
	}
	if got := readApproval(t, handler, spec.id).Approval; got == nil || got.State != "approved" || got.Version == nil ||
		*got.Version != got.LatestVersion || got.LatestVersion != approval.Version+1 {
		t.Errorf("GET /api/v1/artifacts/%s answers the approval %s after the CLI's insert, want it approved at version %d, the latest, carried from alice's approval of version %d",
			spec.id, entryJSON(t, got), approval.Version+1, approval.Version)
	}
}

// cliRun is one run of this tree's dispatch command.
type cliRun struct {
	code           int
	stdout, stderr string
}

func (r cliRun) String() string {
	return fmt.Sprintf("exit %d\nstdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
}

// runDispatchCLI runs this tree's dispatch command, packages/envoy-client/bin/dispatch.ts under
// bun, as cmd/dispatch/twobuild_cli_test.go runs it: against url, with the agent's bearer, as the
// session sessionActor names. Its environment is written here, never inherited, so a Legion pane's
// own Dispatch URL and token never reach it, and its state lives in a directory of its own. A host
// with no bun fails the test: the case it runs is a check's, and a skip would pass it.
func runDispatchCLI(t *testing.T, url string, args ...string) cliRun {
	t.Helper()
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Fatalf("this case runs the dispatch command under bun, which is not on PATH: %v", err)
	}
	entry, err := filepath.Abs(filepath.Join("..", "..", "..", "..", "envoy-client", "bin", "dispatch.ts"))
	if err != nil {
		t.Fatalf("resolve the dispatch command: %v", err)
	}
	home := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bun, append([]string{entry}, args...)...)
	cmd.Dir = home
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"DISPATCH_STATE_DIR=" + home,
		"DISPATCH_HOST=omp",
		"DISPATCH_SESSION_ID=" + decidingSession,
		"DISPATCH_URL=" + url,
		"DISPATCH_TOKEN=agent-token",
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	run := cliRun{stdout: stdout.String(), stderr: stderr.String()}
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		run.code = exit.ExitCode()
	case err != nil:
		t.Fatalf("run dispatch %v: %v", args, err)
	}
	return run
}

// withAskReading is markdown with its ask block blockID holding, after its own content, the one
// reading readingID of words, or no reading for words "": the block's opening line and its own
// content kept as they stand.
func withAskReading(t *testing.T, markdown, blockID, readingID, words string) string {
	t.Helper()
	at := strings.Index(markdown, "ask{#"+blockID+" ")
	if at < 0 {
		at = strings.Index(markdown, "ask{#"+blockID+"}")
	}
	if at < 0 {
		t.Fatalf("the document holds no ask block %s:\n%s", blockID, markdown)
	}
	start := strings.LastIndex(markdown[:at], "\n") + 1
	fence := markdown[start:at]
	lineEnd := start + strings.Index(markdown[start:], "\n")
	closing := strings.Index(markdown[lineEnd:], "\n"+fence+"\n")
	if closing < 0 {
		t.Fatalf("the ask block %s holds no closing %s:\n%s", blockID, fence, markdown)
	}
	closing += lineEnd
	opener, content := markdown[start+len(fence):lineEnd], markdown[lineEnd:closing]
	if cut := strings.Index(content, "\n\n:::reading{"); cut >= 0 {
		content = content[:cut]
	}
	block := ":::" + opener + content + "\n:::\n"
	if words != "" {
		block = "::::" + opener + content + "\n\n:::reading{#" + readingID + "}\n" + words + "\n:::\n::::\n"
	}
	return markdown[:start] + block + markdown[closing+len(fence)+2:]
}

// versionMarkdownAt is version number of the document as stored.
func versionMarkdownAt(t *testing.T, database *store.Store, artifactID string, number int) string {
	t.Helper()
	var markdown string
	if err := database.Pool.QueryRow(context.Background(),
		`select markdown from artifact_versions where artifact_id = $1::uuid and number = $2`, artifactID, number).Scan(&markdown); err != nil {
		t.Fatalf("read version %d of %s: %v", number, artifactID, err)
	}
	return markdown
}

// An answered ask's changed_version is the version that last changed its own words, its question
// and its options: a version that writes the server's attributes into its block, as the roll
// window does while the release before serves, the first write on one build that takes the
// answer's words and selection out again and keeps its state, answerer and time (R10's kept form,
// keptAskState), and a write that gives its block a `shaped` pointer, as the conversion does, each
// move the block's row and leave the ask at the version that wrote it.
func TestAnAnsweredAsksVersionMovesWithItsOwnWordsAloneAndNotWithTheServersAttributes(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	documents := deps.Docs.(*docs.Service)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "A spec.\n\n:::requirement{#r1}\nThe transport is chosen once.\n:::\n")
	uploadAs(t, handler, issue.Key, "asks.md", ":::ask{#a1 urgency=\"med\" multiple=\"false\"}\nWhich transport?\n\n- REST: Plain HTTP\n- gRPC: Streaming\n:::\n\nA note.\n", "alice")
	settleIssue(t, documents, issue.Key)
	asks := readDecisionDocument(t, database, issue.Key, "asks-md")
	askID := blockAskID(t, handler, issue.Key, "a1")
	opened := rowChangedVersion(t, database, asks, "a1")
	openedAt := versionCreatedAt(t, database, asks.id, opened)
	requireAtOpened := func(what string) {
		t.Helper()
		entry := requireEntry(t, readEntries(t, handler, issue.Key), "the answered ask", graphstore.KindAsk+":"+askID)
		if entry.ChangedVersion != opened || entry.ChangedAt == nil || !entry.ChangedAt.Equal(openedAt) {
			t.Errorf("%s the answered ask reads changed_version %d at %v, want %d at %v, the version that wrote its question and its options",
				what, entry.ChangedVersion, entry.ChangedAt, opened, openedAt)
		}
	}
	whileTheReleaseBeforeServes(t, database, func() {
		answerDecisionBlock(t, handler, askID, "bob", []string{"REST"}, "Plain HTTP until a client streams.", nil)
		settleIssue(t, documents, issue.Key)
	})
	inStep := rowChangedVersion(t, database, asks, "a1")
	if stored := versionMarkdownAt(t, database, asks.id, inStep); inStep <= opened || !strings.Contains(stored, `state="answered"`) {
		t.Fatalf("the roll window left a1's row at version %d, holding\n%s\nwant a version past %d whose ask block holds the answer's state", inStep, stored, opened)
	}
	requireAtOpened("after the roll window wrote the answer into its block,")
	editIssueDocumentAs(t, handler, issue.Key, "asks-md", "alice", []map[string]any{{"op": "replace", "find": "A note.", "with": "A note, edited."}})
	settleIssue(t, documents, issue.Key)
	oneBuild := rowChangedVersion(t, database, asks, "a1")
	if stored := versionMarkdownAt(t, database, asks.id, oneBuild); oneBuild <= inStep || strings.Contains(stored, `selected="`) ||
		strings.Contains(stored, ` answer="`) || !strings.Contains(stored, `state="answered" answered_by="bob" answered_at="`) {
		t.Fatalf("the first write on one build left a1's row at version %d, holding\n%s\nwant a version past %d whose ask block holds the answer's state, answerer and time and none of its words", oneBuild, stored, inStep)
	}
	requireAtOpened("after the first write on one build took them out,")
	pointed := strings.Replace(versionMarkdownAt(t, database, asks.id, oneBuild), `:::ask{#a1 `, `:::ask{#a1 shaped="dispatch://`+issue.Key+`/spec#b-r1" `, 1)
	uploadAs(t, handler, issue.Key, "asks.md", pointed, "alice")
	settleIssue(t, documents, issue.Key)
	shaped := rowChangedVersion(t, database, asks, "a1")
	if stored := versionMarkdownAt(t, database, asks.id, shaped); shaped <= oneBuild || !strings.Contains(stored, `shaped="dispatch://`+issue.Key+`/spec#b-r1"`) {
		t.Fatalf("the write of a shaped pointer left a1's row at version %d, holding\n%s\nwant a version past %d whose ask block holds the pointer", shaped, stored, oneBuild)
	}
	requireAtOpened("after a write that gave its block a shaped pointer,")
}

// A decision's words and version are read past where the block index stands. A document indexed
// at its first version and owed its index since reads the newest version's words, and the version
// that wrote them, off the versions' own markdown; a reading written under it in a later owed
// version moves neither; and once one pass of the block-index reconciliation indexes the newest
// version, and records the decision's whole text as changed there, the walk still reads the
// version between, which that pass covered without recording, as the one that changed its words.
// A document no pass ever indexed reads its newest version's words too, not those of the version
// that first wrote its node.
func TestADecisionsWordsAndVersionAreReadPastWhereTheBlockIndexStands(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "A spec.\n")
	const before, after = "The lag decides one way.", "The lag decides another way."
	document := func(items int, words, reading string) string {
		var body strings.Builder
		for range items {
			body.WriteString("- x\n")
		}
		if reading == "" {
			body.WriteString("\n:::decision{#d1}\n" + words + "\n:::\n")
		} else {
			body.WriteString("\n::::decision{#d1}\n" + words + "\n\n:::reading{#g1}\n" + reading + "\n:::\n::::\n")
		}
		return body.String()
	}
	read := func(what string, doc decisionDocument, words string, version int) {
		t.Helper()
		entry := requireEntry(t, readEntries(t, handler, issue.Key), what, refs.BlockNodeKind+":"+doc.address("d1"))
		if want := versionCreatedAt(t, database, doc.id, version); !strings.Contains(entry.Text, words) ||
			entry.ChangedVersion != version || entry.ChangedAt == nil || !entry.ChangedAt.Equal(want) {
			t.Errorf("%s: the decision reads %q at changed_version %d at %v, want its words %q, changed in version %d at %v",
				what, entry.Text, entry.ChangedVersion, entry.ChangedAt, words, version, want)
		}
	}

	// Indexed at its first version by its own write, then owed.
	agentUploadAs(t, handler, issue.Key, "lag.md", document(itemsUnderTheBlockBound-8, before, ""), nil)
	lag := readDecisionDocument(t, database, issue.Key, "lag-md")
	if indexed := blockIndexVersion(t, database, lag.id); indexed == nil || *indexed != 1 {
		t.Fatalf("the first version of lag.md is indexed at %v, want 1, by its own write", indexed)
	}
	agentUploadAs(t, handler, issue.Key, "lag.md", document(itemsOverTheBlockBound, after, ""), nil)
	if indexed := blockIndexVersion(t, database, lag.id); indexed != nil {
		t.Fatalf("the second version of lag.md left its blocks indexed at %d, want them owed", *indexed)
	}
	read("a document owed its index since its first version", lag, after, 2)
	agentUploadAs(t, handler, issue.Key, "lag.md", document(itemsOverTheBlockBound, after, "The agent reads it the other way."), nil)
	read("a reading written in an owed version", lag, after, 2)
	if _, err := deps.Docs.(*docs.Service).ReconcileBlockIndex(ctx, docs.BlockIndexCursors{}, nil); err != nil {
		t.Fatalf("one pass of the block-index reconciliation: %v", err)
	}
	requireIndexedAt(t, database, lag.id, 3)
	if row := rowChangedVersion(t, database, lag, "d1"); row != 3 {
		t.Fatalf("the pass recorded d1's text as changed in version %d, want 3, where the reading made it differ from version 2", row)
	}
	read("a pass that indexed versions 2 and 3 at once", lag, after, 2)

	// Never indexed: the node's first version and the newest differ.
	agentUploadAs(t, handler, issue.Key, "heavy.md", document(itemsOverTheBlockBound, before, ""), nil)
	agentUploadAs(t, handler, issue.Key, "heavy.md", document(itemsOverTheBlockBound, after, ""), nil)
	heavy := readDecisionDocument(t, database, issue.Key, "heavy-md")
	if indexed := blockIndexVersion(t, database, heavy.id); indexed != nil {
		t.Fatalf("heavy.md is indexed at %d, want it owed since its first version", *indexed)
	}
	read("a document no pass ever indexed", heavy, after, 2)
}

// An answered ask whose block left its document keeps its row, which still names that document,
// and its entry names no document: no block of it holds the ask.
func TestAnAnsweredAskWhoseBlockLeftItsDocumentNamesNoDocument(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	documents := deps.Docs.(*docs.Service)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "A spec.\n")
	uploadAs(t, handler, issue.Key, "asks.md", ":::ask{#a1 urgency=\"med\" multiple=\"false\"}\nWhich transport?\n\n- REST: Plain HTTP\n- gRPC: Streaming\n:::\n\nOther.\n", "alice")
	settleIssue(t, documents, issue.Key)
	askID := blockAskID(t, handler, issue.Key, "a1")
	answerDecisionBlock(t, handler, askID, "bob", []string{"REST"}, "Plain HTTP until a client streams.", nil)
	asks := readDecisionDocument(t, database, issue.Key, "asks-md")
	if held := requireEntry(t, readEntries(t, handler, issue.Key), "the answered ask", graphstore.KindAsk+":"+askID); held.Document == nil ||
		held.Document.Block != asks.address("a1") {
		t.Fatalf("the answered ask names the place %s while its block stands, want %s", entryJSON(t, held.Document), asks.address("a1"))
	}
	uploadAs(t, handler, issue.Key, "asks.md", "Other.\n", "alice")
	settleIssue(t, documents, issue.Key)
	var rowDocument *string
	if err := database.Pool.QueryRow(context.Background(), `select block_artifact_id::text from asks where id = $1::uuid`, askID).Scan(&rowDocument); err != nil ||
		rowDocument == nil || *rowDocument != asks.id {
		t.Fatalf("the ask's row names the document %v (%v), want asks.md, %s, whose block it left", rowDocument, err, asks.id)
	}
	left := requireEntry(t, readEntries(t, handler, issue.Key), "the answered ask", graphstore.KindAsk+":"+askID)
	if left.Document != nil || left.AnswerState == nil || *left.AnswerState != "answered" {
		t.Errorf("the answered ask whose block left asks.md reads %s, want it answered and naming no document", entryJSON(t, left))
	}
}

// Test round 1: the same ask, whose block left, after a block of another type took
// its block id in the same document. The ask's row still names that document and that id, but no
// block there holds the ask: the entry names no document, as it does when the id stands nowhere,
// and never the callout. Measured on a real server of this head: a person's edit of SEED-1's spec
// that deleted the answered ask q-kept and put a callout with the id q-kept in its place left the
// entry naming SEED-1/spec#q-kept, the callout, until the next tick restored the ask.
func TestAnAnsweredAskWhoseBlockIdABlockOfAnotherTypeTookNamesNoDocument(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	documents := deps.Docs.(*docs.Service)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "A spec.\n")
	uploadAs(t, handler, issue.Key, "asks.md", ":::ask{#a1 urgency=\"med\" multiple=\"false\"}\nWhich transport?\n\n- REST: Plain HTTP\n- gRPC: Streaming\n:::\n\nOther.\n", "alice")
	settleIssue(t, documents, issue.Key)
	askID := blockAskID(t, handler, issue.Key, "a1")
	answerDecisionBlock(t, handler, askID, "bob", []string{"REST"}, "Plain HTTP until a client streams.", nil)
	asks := readDecisionDocument(t, database, issue.Key, "asks-md")
	uploadAs(t, handler, issue.Key, "asks.md", ":::callout{#a1 kind=\"note\" title=\"\"}\nThe transport is plain HTTP.\n:::\n\nOther.\n", "alice")
	settleIssue(t, documents, issue.Key)
	var rowDocument, rowBlock *string
	if err := database.Pool.QueryRow(context.Background(), `select block_artifact_id::text, block_id from asks where id = $1::uuid`, askID).Scan(&rowDocument, &rowBlock); err != nil ||
		rowDocument == nil || *rowDocument != asks.id || rowBlock == nil || *rowBlock != "a1" {
		t.Fatalf("the ask's row names the document %v and the block %v (%v), want asks.md, %s, and a1, the id the callout took", rowDocument, rowBlock, err, asks.id)
	}
	left := requireEntry(t, readEntries(t, handler, issue.Key), "the answered ask", graphstore.KindAsk+":"+askID)
	if left.Document != nil || left.AnswerState == nil || *left.AnswerState != "answered" {
		t.Errorf("the answered ask whose block id a callout of asks.md took reads %s, want it answered and naming no document: %s holds the callout, not the ask", entryJSON(t, left), asks.address("a1"))
	}
}

// Test round 1, the case Verification v41 gives c-attribute-removal: once the fleet
// is one build, the text `dispatch doc-read` prints (GET /api/v1/artifacts/{id}/text) holds none
// of the eight server-owned ask attributes, and that text sent back unchanged is what the store
// writes: the round trip the CLI's upload check asks answers it byte for byte, and the agent's
// upload of it is taken. Measured on a real server of this head: doc-read printed state, answered_by,
// answered_at, selected and answer on an answered ask block the store held as
// `:::ask{#q-drive urgency="med" multiple="false"}`, and `dispatch artifact --force` refused that
// text naming the ask's line.
func TestTheTextADocumentReadPrintsHoldsNoServerOwnedAskAttributeAndUploadsBackUnchanged(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	documents := deps.Docs.(*docs.Service)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "A spec.\n")
	uploadAs(t, handler, issue.Key, "asks.md", ":::ask{#a1 urgency=\"med\" multiple=\"false\"}\nWhich transport?\n\n- REST: Plain HTTP\n- gRPC: Streaming\n:::\n\nOther.\n", "alice")
	settleIssue(t, documents, issue.Key)
	askID := blockAskID(t, handler, issue.Key, "a1")
	answerDecisionBlock(t, handler, askID, "bob", []string{"REST"}, "Plain HTTP until a client streams.", nil)
	settleIssue(t, documents, issue.Key)
	artifactID := readDecisionDocument(t, database, issue.Key, "asks-md").id
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/text", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the text of asks.md: status=%d body=%s", response.Code, response.Body.String())
	}
	var read struct {
		Markdown string `json:"markdown"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &read); err != nil {
		t.Fatalf("decode the text of asks.md: %v: %s", err, response.Body.String())
	}
	for _, attribute := range []string{"state", "answered_by", "answered_at", "selected", "answer", "invalid", "copied_from", "copied_from_document"} {
		if strings.Contains(read.Markdown, " "+attribute+"=\"") {
			t.Errorf("the text a document read prints holds the server-owned ask attribute %s, which the store no longer writes and an upload takes off:\n%s", attribute, read.Markdown)
		}
	}
	stored := dispatchRequest(t, handler, http.MethodPost, "/api/v1/markdown/round-trip", map[string]any{"markdown": read.Markdown}, "alice")
	var roundTrip struct {
		Markdown string `json:"markdown"`
	}
	if stored.Code != http.StatusOK || json.Unmarshal(stored.Body.Bytes(), &roundTrip) != nil {
		t.Fatalf("the round trip of the read text: status=%d body=%s", stored.Code, stored.Body.String())
	}
	if strings.TrimSuffix(roundTrip.Markdown, "\n") != strings.TrimSuffix(read.Markdown, "\n") {
		t.Errorf("the store writes the text a document read prints under another spelling, so dispatch artifact refuses it:\nread:\n%s\nstored:\n%s", read.Markdown, roundTrip.Markdown)
	}
	agentUploadAs(t, handler, issue.Key, "asks.md", read.Markdown, nil)
}

// Test round 2 of this change, the defect a sibling change's implementer found at 44f1abef1605 and the
// architect sent to this round: the release before answered the text of a document with no ask
// block with no asks member at all, and this build adds "asks":[]. The field carries omitzero,
// which leaves out a nil slice and keeps an empty one, so a writer that fills the field with an
// empty slice changes the shape of every such read, which the basebuild store census reads as the
// two builds answering differently. A document with an ask block still answers its asks member.
func TestTheTextOfADocumentWithNoAskBlockAnswersNoAsksMember(t *testing.T) {
	t.Parallel()
	handler, _, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	documents := deps.Docs.(*docs.Service)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "A spec with no ask block.\n")
	settleIssue(t, documents, issue.Key)
	members := func(what, path string) map[string]json.RawMessage {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, path, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("%s: GET %s: status=%d body=%s", what, path, response.Code, response.Body.String())
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: decode GET %s: %v: %s", what, path, err, response.Body.String())
		}
		return body
	}
	for _, path := range []string{
		"/api/v1/artifacts/" + issue.PrimaryArtifactID + "/text",
		"/api/v1/artifacts/" + issue.PrimaryArtifactID + "/text?blocks=true",
		"/api/v1/issues/" + issue.Key + "/artifacts/spec/text",
	} {
		if asks, held := members("the spec with no ask block", path)["asks"]; held {
			t.Errorf("GET %s answers the member \"asks\":%s for a document with no ask block, want no asks member, as the release before answered", path, asks)
		}
	}
	uploadAs(t, handler, issue.Key, "asks.md", ":::ask{#a1 urgency=\"med\" multiple=\"false\"}\nWhich transport?\n\n- REST: Plain HTTP\n- gRPC: Streaming\n:::\n", "alice")
	settleIssue(t, documents, issue.Key)
	if _, held := members("asks.md", "/api/v1/issues/"+issue.Key+"/artifacts/asks-md/text")["asks"]; !held {
		t.Errorf("the text of asks.md, which holds the ask block a1, answers no asks member, want one")
	}
}

// Above the deferral bound no block index runs in the write, and the typed-node pass states each
// decision and reading of the document that the version no longer holds gone in that write's own
// transaction: a version that removes a decision and the reading under another leaves both out
// of the Decisions read, and one that holds no typed block at all leaves the last decision out,
// before any block-index reconciliation.
func TestADecisionADeferredVersionRemovesIsGoneBeforeTheBlockIndexRuns(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "A spec.\n")
	big := func(blocks string) string {
		var body strings.Builder
		for range itemsOverTheBlockBound {
			body.WriteString("- x\n")
		}
		return body.String() + "\n" + blocks
	}
	agentUploadAs(t, handler, issue.Key, "big.md", big("::::decision{#d1}\nKeep one.\n\n:::reading{#g1}\nThe agent reads it.\n:::\n::::\n\n"+
		":::decision{#d2}\nDrop one.\n:::\n"), nil)
	doc := readDecisionDocument(t, database, issue.Key, "big-md")
	d1, d2, g1 := refs.BlockNodeKind+":"+doc.address("d1"), refs.BlockNodeKind+":"+doc.address("d2"), doc.address("g1")
	entries := readEntries(t, handler, issue.Key)
	if first := requireEntry(t, entries, "the first decision", d1); !slices.Equal(readingsOf(first), []string{g1 + "<-" + graphstore.KindSession + ":" + decidingSession}) {
		t.Fatalf("the first decision answers the readings %v, want g1", readingsOf(first))
	}
	requireEntry(t, entries, "the second decision", d2)
	requireState := func(what, address, want string) {
		t.Helper()
		node, held, err := graphstore.ReadNode(ctx, database.Pool, refs.BlockNodeKind, address)
		if err != nil || !held || node.State != want {
			t.Errorf("%s: the node %s is held %t in the state %q (%v), want %s", what, address, held, node.State, err, want)
		}
	}
	notListed := func(what, node string) {
		t.Helper()
		if entry, listed := entriesByNode(readEntries(t, handler, issue.Key))[node]; listed {
			t.Errorf("%s: the Decisions read still lists %s", what, entryJSON(t, entry))
		}
	}

	agentUploadAs(t, handler, issue.Key, "big.md", big(":::decision{#d1}\nKeep one.\n:::\n"), nil)
	if indexed := blockIndexVersion(t, database, doc.id); indexed != nil {
		t.Fatalf("big.md is indexed at %d, want it owed: no block index runs past the bound", *indexed)
	}
	requireState("the version that removed d2", doc.address("d2"), graphstore.StateGone)
	requireState("the version that removed g1", g1, graphstore.StateGone)
	requireState("the version that kept d1", doc.address("d1"), graphstore.StateLive)
	notListed("the version that removed d2", d2)
	if kept := requireEntry(t, readEntries(t, handler, issue.Key), "the kept decision", d1); len(kept.Readings) != 0 {
		t.Errorf("the kept decision answers the readings %v after the version that removed g1, want none", readingsOf(kept))
	}

	agentUploadAs(t, handler, issue.Key, "big.md", big("A paragraph and no decision.\n"), nil)
	if indexed := blockIndexVersion(t, database, doc.id); indexed != nil {
		t.Fatalf("big.md is indexed at %d, want it owed", *indexed)
	}
	requireState("the version that holds no typed block", doc.address("d1"), graphstore.StateGone)
	notListed("the version that holds no typed block", d1)
}

// The Decisions entry of an answered ask carries, beside its `selected`, a `selected_trees` list
// holding the tree of the label at the same index, under `Dispatch-Trees: 1` and only there, as
// every read of an answer does (TestEveryReadOfAnAnswerCarriesTheTreeOfEachLabelItChose): the
// Decisions tab draws each chosen label from it. The route table's 200 schema declares the list.
func TestADecisionsEntryCarriesTheTreeOfEachLabelItsAskChose(t *testing.T) {
	t.Parallel()
	handler, issueKey, askID := chosenLabelAsk(t)
	target := "/api/v1/issues/" + issueKey + "/decisions"
	raw, held := valueAt(t, treesRequest(t, handler, target, false), "entries")
	type listed struct {
		Node decisionEntryNode `json:"node"`
	}
	var entries []listed
	if !held || json.Unmarshal(raw, &entries) != nil {
		t.Fatalf("GET %s carries no entries list: %s", target, raw)
	}
	at := slices.IndexFunc(entries, func(entry listed) bool {
		return entry.Node.Kind == graphstore.KindAsk && entry.Node.ID == askID
	})
	if at < 0 {
		t.Fatalf("GET %s lists no entry of ask %s: %s", target, askID, raw)
	}
	requireChosenLabelTrees(t, handler, target, "entries."+fmt.Sprint(at))
	node, _ := readOpenAPITree(t)
	document, _ := node.(map[string]any)
	requireTreeInAnswerSchema(t, document, "/api/v1/issues/{key}/decisions", "entries.0.selected_trees.0")
}

// staleFixture is an issue whose spec holds the requirements r1 and r2, and the agent's decision d1
// in choices.md, which ties r1 as its words are first written. A link's stale compares text sums
// (decisionread's markStale): the decision's own words as they stand against those its document
// held at the version the tie was written in, and against those it held at the version that stood
// when the requirement last changed.
type staleFixture struct {
	t       *testing.T
	handler http.Handler
	issue   model.Issue
	spec    decisionDocument
	node    string
}

func newStaleFixture(t *testing.T, words string) *staleFixture {
	t.Helper()
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	createReferenceAPIProject(t, handler, "CORE")
	fixture := &staleFixture{t: t, handler: handler}
	fixture.issue = createSpecIssue(t, handler, "CORE", ":::requirement{#r1}\nOne.\n:::\n\n:::requirement{#r2}\nTwo.\n:::\n")
	fixture.spec = readDecisionDocument(t, database, fixture.issue.Key, "spec")
	fixture.say(words)
	fixture.node = readDecisionDocument(t, database, fixture.issue.Key, "choices-md").address("d1")
	return fixture
}

// say writes the decision d1 with words, tying r1, as a new version of choices.md.
func (f *staleFixture) say(words string) {
	f.t.Helper()
	agentUploadAs(f.t, f.handler, f.issue.Key, "choices.md",
		":::decision{#d1 shaped=\"dispatch://"+f.issue.Key+"/spec#b-r1\"}\n"+words+"\n:::\n", nil)
}

// reword writes a version of the spec that changes the requirement r1 alone.
func (f *staleFixture) reword(words string) {
	f.t.Helper()
	editSpec(f.t, f.handler, f.issue.Key, []map[string]any{{"op": "replace", "find": "One.", "with": words}})
}

// requireStale fails unless the decision's link to r1 reads stale as want, and the link to r1 is
// the decision's one.
func (f *staleFixture) requireStale(what string, want bool) {
	f.t.Helper()
	entry := requireEntry(f.t, readEntries(f.t, f.handler, f.issue.Key), "the agent's decision", refs.BlockNodeKind+":"+f.node)
	if link := shapedLink(f.t, entry, f.spec.address("r1")); link.Stale != want {
		f.t.Errorf("%s: the link of %s to %s reads stale %t, want %t: %s", what, f.node, f.spec.address("r1"), link.Stale, want, entryJSON(f.t, link))
	}
}

// A decision whose words change and change back is not stale: its words as they stand are the
// words its document held at the version the tie was written in, so the sums compare equal, as
// they did on `main`. A comparison of times would read the last change into those words as a
// change after the tie. A change that stands is stale.
func TestALinkOfADecisionWhoseWordsChangedAndChangedBackIsNotStale(t *testing.T) {
	t.Parallel()
	fixture := newStaleFixture(t, "REST for every service.")
	fixture.requireStale("the words as they were tied", false)
	fixture.say("gRPC for every service.")
	fixture.requireStale("after the words changed, and the requirement did not", true)
	fixture.say("REST for every service.")
	fixture.requireStale("after the words changed back to those it was tied at", false)
	fixture.say("GraphQL for every service.")
	fixture.requireStale("after the words changed to others", true)
}

// A decision whose words change, and then change to the words it held when the requirement last
// changed, is not stale either: the requirement has not changed since the decision did. The words
// A, B, C and B again, with the requirement changed while the decision read B, read stale while
// the decision reads C, which the requirement has not answered, and not once it reads B again.
// A requirement that changes after the decision's change clears the link too.
func TestALinkOfADecisionWhoseWordsReturnToThoseTheRequirementChangedBesideIsNotStale(t *testing.T) {
	t.Parallel()
	fixture := newStaleFixture(t, "REST for every service.")
	fixture.say("gRPC for every service.")
	fixture.requireStale("after the words changed", true)
	fixture.reword("One, rewritten.")
	fixture.requireStale("after the requirement changed beside the new words", false)
	fixture.say("GraphQL for every service.")
	fixture.requireStale("after the words changed again, which the requirement has not answered", true)
	fixture.say("gRPC for every service.")
	fixture.requireStale("after the words changed back to those the requirement changed beside", false)
}
