package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// tieTime is one of the two times the spec tie compares, read from the
// store and never from a time a writer passed: the event that wrote it, and the database's clock
// at that write, which a failure prints and no comparison reads.
type tieTime struct {
	event int64
	at    time.Time
}

func (t tieTime) String() string {
	if t.event != graphstore.NoEvent {
		return "event " + strconv.FormatInt(t.event, 10)
	}
	return "no event, the database's clock at " + t.at.Format(time.RFC3339Nano)
}

// laterThan reports whether later was written after earlier, by the event sequence alone. Every
// tie state this test writes carries the event that wrote it, so one that carries none fails the
// test, printing both, and is never ordered by the clock instead.
func laterThan(t *testing.T, later, earlier tieTime) bool {
	t.Helper()
	if later.event == graphstore.NoEvent || earlier.event == graphstore.NoEvent {
		t.Fatalf("%s and %s are ordered by the event that wrote each, and one carries none", later, earlier)
	}
	return later.event > earlier.event
}

// versionWrittenBy is the event that wrote version number of the document: its artifact.version,
// or for version 1 the document's artifact.created or the issue.created of the issue whose spec it
// is (docs' versionEventID), and that version's created_at, which the database's clock wrote.
func versionWrittenBy(t *testing.T, database *store.Store, artifactID string, number int) tieTime {
	t.Helper()
	var written tieTime
	err := database.Pool.QueryRow(context.Background(), `
		select coalesce((
			select e.id from events e
			join artifacts a on a.id = $1::text::uuid
			where (e.issue_key = a.issue_key or e.artifact_id = a.id)
			  and (e.type = 'artifact.version' and e.payload->>'artifact_id' = $1::text and e.payload->'version'->>'number' = $2::int::text
			       or $2::int = 1 and e.type = 'artifact.created' and e.payload->'artifact'->>'id' = $1::text
			       or $2::int = 1 and e.type = 'issue.created' and e.issue_key = a.issue_key and a.is_primary)
			order by e.id desc limit 1), 0),
		       v.created_at
		from artifact_versions v where v.artifact_id = $1::text::uuid and v.number = $2::int
	`, artifactID, number).Scan(&written.event, &written.at)
	if err != nil {
		t.Fatalf("read what wrote version %d of %s: %v", number, artifactID, err)
	}
	return written
}

// textChangedAt is the text-change time of a spec node: the event that wrote the version in which
// the block's text last changed (document_blocks.changed_version, a version of the block's own
// document on the row its room keys), with that version's created_at. No field of the node says
// whether its text changed since anything.
func textChangedAt(t *testing.T, database *store.Store, document decisionDocument, block string) tieTime {
	t.Helper()
	var changed int
	if err := database.Pool.QueryRow(context.Background(), `
		select changed_version from document_blocks where artifact_id = $1::uuid and block_id = $2
	`, document.room, block).Scan(&changed); err != nil {
		t.Fatalf("read the version %s's text last changed in: %v", document.address(block), err)
	}
	return versionWrittenBy(t, database, document.id, changed)
}

// shapedTime is a shaped tuple's own time: the event it carries, with its at.
func shapedTime(tuple graphstore.Tuple) tieTime {
	return tieTime{event: tuple.SourceEvent, at: tuple.At}
}

// shapedInto is every shaped tuple the store states now into the block address.
func shapedInto(t *testing.T, database *store.Store, address string) []graphstore.Tuple {
	t.Helper()
	held, err := graphstore.CurrentIn(context.Background(), database.Pool, refs.BlockNodeKind, address, []string{refs.TupleShaped})
	if err != nil {
		t.Fatalf("read the shaped tuples into %s: %v", address, err)
	}
	return held
}

// tuplesText prints tuples as the failures of this file name them.
func tuplesText(tuples []graphstore.Tuple) string {
	if len(tuples) == 0 {
		return "none"
	}
	parts := make([]string, len(tuples))
	for index, tuple := range tuples {
		parts[index] = fmt.Sprintf("%s:%s %s %s:%s asserted=%t seq=%d source_event=%d",
			tuple.FromKind, tuple.FromID, tuple.Kind, tuple.ToKind, tuple.ToID, tuple.Asserted, tuple.Seq, tuple.SourceEvent)
	}
	return strings.Join(parts, "; ")
}

// requireShapedFrom fails unless the store states, of the shaped tuples into address, exactly one
// from each of the near ends want names, written kind:id, and no other, printing what it found.
func requireShapedFrom(t *testing.T, database *store.Store, what, address string, want ...string) map[string]graphstore.Tuple {
	t.Helper()
	held := shapedInto(t, database, address)
	byNear := map[string]graphstore.Tuple{}
	for _, tuple := range held {
		byNear[tuple.FromKind+":"+tuple.FromID] = tuple
	}
	matched := len(held) == len(want)
	for _, near := range want {
		if _, found := byNear[near]; !found {
			matched = false
		}
	}
	if !matched {
		t.Fatalf("%s: the store states the shaped tuples %s into %s, want one from each of %v", what, tuplesText(held), address, want)
	}
	return byNear
}

// requireShapedBacklinks fails unless GET /api/v1/references?to= of address answers, under the
// kind shaped, exactly the near ends want names, written kind:id.
func requireShapedBacklinks(t *testing.T, handler http.Handler, what, reference string, want ...string) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/references?to="+url.QueryEscape(reference), nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("%s: read the backlinks of %s: status=%d body=%s", what, reference, response.Code, response.Body.String())
	}
	var shaped []string
	for _, edge := range decodeBody[model.GraphReferences](t, response).Edges {
		if edge.Kind != refs.TupleShaped {
			continue
		}
		near := edge.Node.Kind + ":" + edge.Node.ID
		if edge.Block != nil {
			near = edge.Block.Kind + ":" + edge.Block.ID
		}
		shaped = append(shaped, near)
	}
	matched := len(shaped) == len(want)
	for _, near := range want {
		found := false
		for _, read := range shaped {
			found = found || read == near
		}
		matched = matched && found
	}
	if !matched {
		t.Fatalf("%s: the backlink read of %s answers %v under shaped, want %v: %s", what, reference, shaped, want, response.Body.String())
	}
	t.Logf("%s: the backlink read of %s answers %v under shaped", what, reference, shaped)
}

// textAtTheTie is the text the store held for the block of document when the tuple was written:
// the latest document_block_texts row of the block, on the rows its room keys, at or before the
// newest version of the document whose writing event is no later than the tuple's own.
func textAtTheTie(t *testing.T, database *store.Store, document decisionDocument, block string, tuple graphstore.Tuple) string {
	t.Helper()
	ctx := context.Background()
	rows, err := database.Pool.Query(ctx, `select number from artifact_versions where artifact_id = $1::uuid order by number`, document.id)
	if err != nil {
		t.Fatalf("read the versions of %s: %v", document.refKey, err)
	}
	numbers, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		t.Fatalf("read the versions of %s: %v", document.refKey, err)
	}
	held := 0
	for _, number := range numbers {
		if !laterThan(t, versionWrittenBy(t, database, document.id, number), shapedTime(tuple)) {
			held = number
		}
	}
	if held == 0 {
		t.Fatalf("no version of %s was written by an event at or before the tuple's %d", document.refKey, tuple.SourceEvent)
	}
	var text *string
	err = database.Pool.QueryRow(ctx, `
		select text from document_block_texts
		where artifact_id = $1::uuid and block_id = $2 and version <= $3
		order by version desc limit 1
	`, document.room, block, held).Scan(&text)
	if errors.Is(err, pgx.ErrNoRows) || text == nil {
		t.Fatalf("the store holds no text of %s at version %d, the version the tuple %d was written at", document.address(block), held, tuple.SourceEvent)
	}
	if err != nil {
		t.Fatalf("read the text of %s at version %d: %v", document.address(block), held, err)
	}
	return *text
}

// shapedHistory is every shaped tuple the node ever stated, asserted or retracted.
func shapedHistory(t *testing.T, database *store.Store, address string) []graphstore.Tuple {
	t.Helper()
	history, err := graphstore.History(context.Background(), database.Pool, refs.BlockNodeKind, address, refs.TupleShaped)
	if err != nil {
		t.Fatalf("read the shaped history of %s: %v", address, err)
	}
	return history
}

// c-spec-tie. A shaped tuple ties a decision to each spec node it shaped, from the decision's own
// node: the answered ask's node for a person's decision, and the decision's block node for an
// agent's. The spec holds five requirements. The answered ask shaped the first and the fourth; the
// agent's decision d1 shaped the first three. Each reading below is of the store - its tuples, its
// nodes, its block texts and the events that wrote them - and of the backlink read
// `GET /api/v1/references?to=`, and every time compared is an event the store holds: a tie state
// that carries none fails the test, and no time a writer passed, nor the clock, orders two of them.
func TestAShapedTupleTiesADecisionToEachSpecNodeItShaped(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	documents := deps.Docs.(*docs.Service)
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", ":::requirement{#r1}\nOne.\n:::\n\n:::requirement{#r2}\nTwo.\n:::\n\n"+
		":::requirement{#r3}\nThree.\n:::\n\n:::requirement{#r4}\nFour.\n:::\n\n:::requirement{#r5}\nFive.\n:::\n")
	spec := readDecisionDocument(t, database, issue.Key, "spec")
	pointer := func(block string) string { return "dispatch://" + issue.Key + "/spec#b-" + block }

	// The person's decision: an ask shaping the first and the fourth requirement, answered by bob.
	uploadAs(t, handler, issue.Key, "asks.md", ":::ask{#a1 urgency=\"med\" multiple=\"false\" shaped=\""+pointer("r1")+" "+pointer("r4")+"\"}\n"+
		"Which transport?\n\n- REST: Plain HTTP\n- gRPC: Streaming\n:::\n", "alice")
	settleIssue(t, documents, issue.Key)
	askID := blockAskID(t, handler, issue.Key, "a1")
	answerDecisionBlock(t, handler, askID, "bob", []string{"REST"}, "Plain HTTP until a client streams.", nil)
	ask := graphstore.KindAsk + ":" + askID

	// The agent's decision, shaping the first three, written by an agent's session.
	agentUpload := func(what, content string) {
		t.Helper()
		requireAgentWrite(t, what, sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
			"name": "choices.md", "content": content, "actor": sessionActor(),
		}), http.StatusCreated)
	}
	decision := func(id, shaped, text string) string {
		return ":::decision{#" + id + " shaped=\"" + shaped + "\"}\n" + text + "\n:::\n"
	}
	const d1Text = "REST for every service this quarter."
	agentUpload("the agent's decision", decision("d1", pointer("r1")+" "+pointer("r2")+" "+pointer("r3"), d1Text))
	choices := readDecisionDocument(t, database, issue.Key, "choices-md")
	d1 := refs.BlockNodeKind + ":" + choices.address("d1")

	// The fourth requirement, which the answered ask alone shaped: one tuple, from the ask's node
	// and not the ask's block node, whose block_refs row - the one shaped row into it - carries the
	// ask's node; the node holds the person's answer, its words and its selection, as the store
	// holds them, and the backlink read answers the ask.
	fourth := requireShapedFrom(t, database, "the fourth requirement", spec.address("r4"), ask)[ask]
	rows, err := database.Pool.Query(ctx, `
		select coalesce(node_kind, ''), coalesce(node_address, ''), from_node from block_refs
		where kind = $1 and to_kind = $2 and to_id = $3
	`, refs.TupleShaped, refs.BlockNodeKind, spec.address("r4"))
	if err != nil {
		t.Fatalf("read the block_refs rows of the ties to the fourth requirement: %v", err)
	}
	type tieRow struct{ nodeKind, nodeAddress, fromNode string }
	tieRows, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (tieRow, error) {
		var read tieRow
		err := row.Scan(&read.nodeKind, &read.nodeAddress, &read.fromNode)
		return read, err
	})
	if err != nil {
		t.Fatalf("read the block_refs rows of the ties to the fourth requirement: %v", err)
	}
	if len(tieRows) != 1 || tieRows[0].nodeKind != graphstore.KindAsk || tieRows[0].nodeAddress != askID {
		t.Fatalf("the shaped block_refs rows into the fourth requirement are %+v, want one, carrying the ask's node (ask, %s)", tieRows, askID)
	}
	// The ask's node holds no question: the question stays on the asks row, and the node holds
	// the answer, its selection and who gave it (asks.Settle). So the decision's question is read
	// off the row of the node's ask, and the person's answer off the node.
	var question string
	if err := database.Pool.QueryRow(ctx, `select question from asks where id = $1::uuid`, askID).Scan(&question); err != nil {
		t.Fatalf("read the question of the ask at the tie's near end: %v", err)
	}
	answer, stated, err := graphstore.CurrentValue(ctx, database.Pool, graphstore.KindAsk, askID, graphstore.TupleAnswer)
	if err != nil || !stated || question != "Which transport?" || answer != "Plain HTTP until a client streams." {
		t.Fatalf("the ask at the tie's near end holds the question %q and the answer %q (stated %t, %v), want the question and bob's answer",
			question, answer, stated, err)
	}
	selection, stated, err := graphstore.CurrentValue(ctx, database.Pool, graphstore.KindAsk, askID, graphstore.TupleSelected)
	var selected []string
	if err == nil && stated {
		err = json.Unmarshal([]byte(selection), &selected)
	}
	if err != nil || !stated || len(selected) != 1 || selected[0] != "REST" {
		t.Fatalf("the ask at the tie's near end holds the selection %q (stated %t, %v), want bob's, [\"REST\"]", selection, stated, err)
	}
	requireShapedBacklinks(t, handler, "the fourth requirement", pointer("r4"), ask)

	// The first requirement, which the ask and the agent's decision both shaped: one tuple from
	// each, and the backlink read answers both.
	first := requireShapedFrom(t, database, "the first requirement", spec.address("r1"), ask, d1)
	requireShapedBacklinks(t, handler, "the first requirement", pointer("r1"), ask, d1)
	tie := first[d1]

	// The tie stands across versions, and the text-change time moves with the text alone.
	changedBefore := textChangedAt(t, database, spec, "r1")
	if !laterThan(t, shapedTime(tie), changedBefore) {
		t.Fatalf("the tie's own time %s is not later than the text-change time %s of the requirement it shaped", shapedTime(tie), changedBefore)
	}
	untouched := editSpec(t, handler, issue.Key, []map[string]any{{"op": "replace", "find": "Five.", "with": "Five, reworded."}})
	if held := requireShapedFrom(t, database, "after a version that left the first requirement alone", spec.address("r1"), ask, d1); held[d1].Seq != tie.Seq || held[ask].Seq != first[ask].Seq {
		t.Fatalf("after a version that left the first requirement alone the store states %s into it, want the tuples %s it held before",
			tuplesText([]graphstore.Tuple{held[ask], held[d1]}), tuplesText([]graphstore.Tuple{first[ask], tie}))
	}
	if changed := textChangedAt(t, database, spec, "r1"); changed != changedBefore {
		t.Fatalf("version %d left the first requirement's text alone and moved its text-change time from %s to %s", untouched.Number, changedBefore, changed)
	}
	rewritten := editSpec(t, handler, issue.Key, []map[string]any{{"op": "replace", "find": "One.", "with": "One, rewritten."}})
	if held := requireShapedFrom(t, database, "after a version that rewrote the first requirement", spec.address("r1"), ask, d1); held[d1].Seq != tie.Seq || held[ask].Seq != first[ask].Seq {
		t.Fatalf("after a version that rewrote the first requirement the store states %s into it, want the tuples %s it held before",
			tuplesText([]graphstore.Tuple{held[ask], held[d1]}), tuplesText([]graphstore.Tuple{first[ask], tie}))
	}
	changedAfter := textChangedAt(t, database, spec, "r1")
	if want := versionWrittenBy(t, database, spec.id, rewritten.Number); changedAfter != want || !laterThan(t, changedAfter, shapedTime(tie)) {
		t.Fatalf("version %d rewrote the first requirement, and its text-change time reads %s against the tie's own %s, want %s, the version's own and later than the tie",
			rewritten.Number, changedAfter, shapedTime(tie), want)
	}
	t.Logf("the first requirement's text changed at %s, after the tie's %s: the shaped text changed since the decision", changedAfter, shapedTime(tie))

	// The tie belongs to the versions the decision covers: the version that left the requirement
	// alone was written after the decision and holds its tie, and a decision written after that
	// version ties to no node of it.
	covered := versionWrittenBy(t, database, spec.id, untouched.Number)
	if laterThan(t, shapedTime(tie), covered) {
		t.Fatalf("the tie's own time %s is later than %s, which wrote version %d the decision covers", shapedTime(tie), covered, untouched.Number)
	}
	agentUpload("the agent's later decision", decision("d1", pointer("r1")+" "+pointer("r2")+" "+pointer("r3"), d1Text)+"\n"+
		decision("d2", pointer("r1"), "gRPC for the streaming service."))
	d2 := refs.BlockNodeKind + ":" + choices.address("d2")
	later := requireShapedFrom(t, database, "the first requirement after a later decision", spec.address("r1"), ask, d1, d2)[d2]
	if !laterThan(t, shapedTime(later), covered) {
		t.Fatalf("the later decision's tie carries %s, no later than %s, which wrote version %d: that version would hold a tie written after it",
			shapedTime(later), covered, untouched.Number)
	}

	// The fourth requirement goes in a new version: the ask's tie to it stands, its node is gone,
	// and the store still holds the words it had when the tie was written.
	editSpec(t, handler, issue.Key, []map[string]any{{"op": "delete", "block": "r4"}})
	if changed := textChangedAt(t, database, spec, "r1"); changed != changedAfter {
		t.Fatalf("the version that deleted the fourth requirement left the first one's text alone and moved its text-change time from %s to %s",
			changedAfter, changed)
	}
	if held := requireShapedFrom(t, database, "after the fourth requirement went", spec.address("r4"), ask)[ask]; held.Seq != fourth.Seq {
		t.Fatalf("after the fourth requirement went the store states %s into it, want the ask's tie %s still asserted", tuplesText([]graphstore.Tuple{held}), tuplesText([]graphstore.Tuple{fourth}))
	}
	node, held, err := graphstore.ReadNode(ctx, database.Pool, refs.BlockNodeKind, spec.address("r4"))
	if err != nil || !held || node.State != graphstore.StateGone {
		t.Fatalf("the fourth requirement's node is held %t in the state %q (%v), want it gone", held, node.State, err)
	}
	if text := textAtTheTie(t, database, spec, "r4", fourth); !strings.Contains(text, "Four.") {
		t.Fatalf("the store holds the fourth requirement's text %q at the version its tie was written at, want the words it had then, Four.", text)
	}

	// The tie follows coverage and not placement: a move of d1 writes no shaped tuple, and a
	// retarget of its pointer moves the tie and writes no part_of.
	for _, requirement := range []string{"r1", "r2", "r3"} {
		requireShapedFromIncluding(t, database, "before the move", spec.address(requirement), d1)
	}
	shapedBefore := shapedHistory(t, database, choices.address("d1"))
	agentUpload("the move of the agent's decision", "::::callout{#c1 kind=\"note\"}\n"+
		decision("d1", pointer("r1")+" "+pointer("r2")+" "+pointer("r3"), d1Text)+"::::\n\n"+decision("d2", pointer("r1"), "gRPC for the streaming service."))
	parents := currentTies(t, database, choices.address("d1"), graphstore.TuplePartOf)
	if len(parents) != 1 || parents[0].ToKind != refs.BlockNodeKind || parents[0].ToID != choices.address("c1") {
		t.Fatalf("after the move d1 states the part_of %s, want one, to the callout %s", tuplesText(parents), choices.address("c1"))
	}
	for _, requirement := range []string{"r1", "r2", "r3"} {
		requireShapedFromIncluding(t, database, "after the move", spec.address(requirement), d1)
	}
	if moved := shapedHistory(t, database, choices.address("d1")); len(moved) != len(shapedBefore) {
		t.Fatalf("the move wrote shaped tuples: d1's shaped history is %s, and was %s before it", tuplesText(moved), tuplesText(shapedBefore))
	}
	partOfBefore, err := graphstore.History(ctx, database.Pool, refs.BlockNodeKind, choices.address("d1"), graphstore.TuplePartOf)
	if err != nil {
		t.Fatalf("read d1's part_of history: %v", err)
	}
	agentUpload("the retarget of the agent's decision", "::::callout{#c1 kind=\"note\"}\n"+
		decision("d1", pointer("r1")+" "+pointer("r5")+" "+pointer("r3"), d1Text)+"::::\n\n"+decision("d2", pointer("r1"), "gRPC for the streaming service."))
	if into := shapedInto(t, database, spec.address("r2")); len(into) != 0 {
		t.Fatalf("after the retarget the store states %s into the second requirement, want d1's tie to it retracted", tuplesText(into))
	}
	requireShapedFrom(t, database, "the fifth requirement after the retarget", spec.address("r5"), d1)
	partOfAfter, err := graphstore.History(ctx, database.Pool, refs.BlockNodeKind, choices.address("d1"), graphstore.TuplePartOf)
	if err != nil {
		t.Fatalf("read d1's part_of history: %v", err)
	}
	if parents := currentTies(t, database, choices.address("d1"), graphstore.TuplePartOf); len(partOfAfter) != len(partOfBefore) ||
		len(parents) != 1 || parents[0].ToID != choices.address("c1") {
		t.Fatalf("the retarget wrote part_of tuples: d1's part_of history is %s and was %s, and it states the part_of %s, want the callout %s its move gave it",
			tuplesText(partOfAfter), tuplesText(partOfBefore), tuplesText(parents), choices.address("c1"))
	}
}

// requireShapedFromIncluding fails unless the store states one shaped tuple from near into
// address, among whatever others, printing every tuple it found there.
func requireShapedFromIncluding(t *testing.T, database *store.Store, what, address, near string) {
	t.Helper()
	held := shapedInto(t, database, address)
	count := 0
	for _, tuple := range held {
		if tuple.FromKind+":"+tuple.FromID == near {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("%s: the store states the shaped tuples %s into %s, want one from %s", what, tuplesText(held), address, near)
	}
}
