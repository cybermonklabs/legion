package api

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/schemanodes"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// decisionDocument is one document a decision test writes, as the store addresses its blocks:
// its id and its ref_key, which a pointer at one of its blocks spells (`<ref_key>#<block>`, a
// shaped tuple's far end), and the id and ref_key of the artifact its blocks' rows and nodes are
// keyed under - the document itself, or for a document converted into its issue's room the room's
// tree artifact (T12a of the issue-tree plan).
type decisionDocument struct {
	id, refKey, room, nodeKey string
}

// address is a block's node address in the document.
func (d decisionDocument) address(block string) string { return d.nodeKey + "#" + block }

// pointed is the node a pointer at a block of the document resolves to, which a shaped tuple's far
// end and a decision's link name: a tuple's ends are nodes, and the node of a block of a document
// converted into its issue's room is the room's (address), whatever address the pointer spells.
func (d decisionDocument) pointed(block string) string { return d.address(block) }

// readDecisionDocument reads the id and ref_key of the issue's document slug, and those of the
// artifact its blocks are keyed under (refs.DocumentRoomSQL).
func readDecisionDocument(t *testing.T, database *store.Store, issueKey, slug string) decisionDocument {
	t.Helper()
	var document decisionDocument
	if err := database.Pool.QueryRow(context.Background(), `
		select a.id::text, a.ref_key, room.id::text, room.ref_key
		from artifacts a join artifacts room on room.id = `+refs.DocumentRoomSQL("a.id")+`
		where a.issue_key = $1 and a.slug = $2
	`, issueKey, slug).Scan(&document.id, &document.refKey, &document.room, &document.nodeKey); err != nil {
		t.Fatalf("read the document %s of %s: %v", slug, issueKey, err)
	}
	return document
}

// uploadAs uploads content as the issue's document name, as login, and fails unless the upload
// is taken. An upload over a document the issue already holds under name names the token of the
// version it replaces, and a first upload names none (uploadBaseNamed).
func uploadAs(t *testing.T, handler http.Handler, issueKey, name, content, login string) {
	t.Helper()
	path := "/api/v1/issues/" + issueKey + "/artifacts"
	response := dispatchRequest(t, handler, http.MethodPost, path, map[string]any{
		"name": name, "content": content, "precondition": uploadBaseNamed(t, handler, path, name),
	}, login)
	if response.Code != http.StatusCreated {
		t.Fatalf("upload %s as %s: status=%d body=%s", name, login, response.Code, response.Body.String())
	}
}

// versionEvent is the id of the event that recorded version number of the document: the
// artifact.created of the upload that made it, for its first version, and an artifact.version
// for every later one (api's storeArtifact).
func versionEvent(t *testing.T, database *store.Store, artifactID string, number int) int64 {
	t.Helper()
	var id int64
	if err := database.Pool.QueryRow(context.Background(), `
		select id from events
		where (type = 'artifact.created' and $2 = 1 and payload->'artifact'->>'id' = $1)
		   or (type = 'artifact.version' and payload->>'artifact_id' = $1 and (payload->'version'->>'number')::int = $2)
	`, artifactID, number).Scan(&id); err != nil {
		t.Fatalf("read the event of version %d of %s: %v", number, artifactID, err)
	}
	return id
}

// currentTies is what a block node states now under kind (graphstore.CurrentOut).
func currentTies(t *testing.T, database *store.Store, address, kind string) []graphstore.Tuple {
	t.Helper()
	held, err := graphstore.CurrentOut(context.Background(), database.Pool, refs.BlockNodeKind, address, []string{kind})
	if err != nil {
		t.Fatalf("read the %s of %s: %v", kind, address, err)
	}
	return held
}

// requireDecidedBy fails unless the decision's node holds exactly one decided_by, to the person
// login, printing the stored tuple and the login it holds.
func requireDecidedBy(t *testing.T, database *store.Store, what, address, login string) graphstore.Tuple {
	t.Helper()
	return requireDecidedByNode(t, database, what, address, graphstore.KindPerson, login)
}

// requireDecidedByNode fails unless the decision's node holds exactly one decided_by, to the node
// kind/id - a person or the session of an agent - printing the stored tuple and what it holds.
func requireDecidedByNode(t *testing.T, database *store.Store, what, address, kind, id string) graphstore.Tuple {
	t.Helper()
	held := currentTies(t, database, address, graphstore.TupleDecidedBy)
	if len(held) != 1 || held[0].ToKind != kind || held[0].ToID != id {
		var logins []string
		for _, tuple := range held {
			logins = append(logins, tuple.ToKind+":"+tuple.ToID)
		}
		t.Errorf("%s: the stored decided_by of %s is %+v, holding the login %v; want the writer %s:%s, whose credential the write proved",
			what, address, held, logins, kind, id)
		return graphstore.Tuple{}
	}
	return held[0]
}

// requireValueCaused fails unless the newest tuple of the node's attribute carries the writer who
// and the event the write appended.
func requireValueCaused(t *testing.T, database *store.Store, address, attribute, who string, event int64) {
	t.Helper()
	history, err := graphstore.History(context.Background(), database.Pool, refs.BlockNodeKind, address, attribute)
	if err != nil || len(history) == 0 {
		t.Fatalf("read the history of %s of %s: %v (%d tuples)", attribute, address, err, len(history))
	}
	requireCaused(t, attribute+" of "+address, history[len(history)-1], who, event)
}

// decidingSession is the session sessionActor names, whose node an agent's decision is decided by.
const decidingSession = "session-0123456789abcdef"

// requireAgentWrite fails unless response is the success status want of an agent's write.
func requireAgentWrite(t *testing.T, what string, response *httptest.ResponseRecorder, want int) {
	t.Helper()
	if response.Code != want {
		t.Fatalf("%s: status=%d body=%s, want %d", what, response.Code, response.Body.String(), want)
	}
}

// requireCaused fails unless tuple carries who as its writer and event as its cause.
func requireCaused(t *testing.T, what string, tuple graphstore.Tuple, who string, event int64) {
	t.Helper()
	if tuple.Who.Kind != "user" || tuple.Who.ID != who || tuple.SourceEvent != event {
		t.Errorf("%s carries who %+v and source_event %d, want %s and the event %d that write appended", what, tuple.Who, tuple.SourceEvent, who, event)
	}
}

// requireSupertag fails unless the block's node has kind block and the supertag want.
func requireSupertag(t *testing.T, database *store.Store, address, want string) {
	t.Helper()
	var supertag string
	if err := database.Pool.QueryRow(context.Background(),
		`select supertag from graph_nodes where kind = $1 and id = $2`, refs.BlockNodeKind, address).Scan(&supertag); err != nil {
		t.Fatalf("read the node of %s: %v", address, err)
	}
	if supertag != want {
		t.Errorf("the node of %s has the supertag %q, want %q", address, supertag, want)
	}
}

// requireValue fails unless the node's attribute holds want, or holds nothing for want "".
func requireValue(t *testing.T, database *store.Store, address, attribute, want string) {
	t.Helper()
	value, held, err := graphstore.CurrentValue(context.Background(), database.Pool, refs.BlockNodeKind, address, attribute)
	if err != nil {
		t.Fatalf("read %s of %s: %v", attribute, address, err)
	}
	if want == "" && held {
		t.Errorf("%s of %s holds %q, want it absent: its author recorded none", attribute, address, value)
	}
	if want != "" && (!held || value != want) {
		t.Errorf("%s of %s holds %q (held %t), want %q", attribute, address, value, held, want)
	}
}

// settleIssue runs every settlement the issue's documents owe, on this goroutine.
func settleIssue(t *testing.T, documents *docs.Service, issueKey string) {
	t.Helper()
	if err := documents.SettleIssueDocuments(context.Background(), issueKey); err != nil {
		t.Fatalf("settle the documents of %s: %v", issueKey, err)
	}
}

// decisionNode is a decision block a browser's editor writes, holding text.
func decisionNode(id, text string, attributes pmdoc.Attrs) *pmdoc.Node {
	attrs := pmdoc.Attrs{pmdoc.BlockIDAttr: id}
	for name, value := range attributes {
		attrs[name] = value
	}
	return &pmdoc.Node{Type: "decision", Attrs: attrs, Children: []*pmdoc.Node{
		{Type: "paragraph", Children: []*pmdoc.Node{{Type: "text", Text: text}}},
	}}
}

// c-decision-node. A decision is a node of kind block and of the supertag decision, which the
// store's own supertag path declares with its values and its tuple kinds beside the types main
// carries. One write of a decision appends its shaped, from_ask and decided_by tuples and its
// certainty values, each carrying its writer and the event that write appended; the markdown
// read back renders shaped as serves is and never decided_by; the same markdown uploaded again
// retracts no shaped tuple; and an index pass leaves from_ask and decided_by standing. The author
// is the one the credential proved, never the one the markdown claimed, on each of the four
// paths a write takes - the upload, the editor operation, the issue's first document and the
// browser's settlement - and a later write, by the same writer or another, moves nothing of it.
// An answered ask holds no uncertainty: a write giving it one is refused, naming the attribute.
func TestDecisionNodeCarriesItsTuplesAndItsCertainty(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour, browserPeers: true})
	documents := deps.Docs.(*docs.Service)
	ctx := context.Background()

	// The schema the store installs is the one blocks.json declares at this head, with both
	// types, beside the five main carries, written through the store's supertag path.
	installed, err := schemanodes.Read(ctx, database.Pool)
	if err != nil {
		t.Fatalf("read the schema the store's nodes build: %v", err)
	}
	if installed.Version != pmdoc.BuiltSchemaVersion() {
		t.Fatalf("the installed schema is version %d, want %d, the version blocks.json declares", installed.Version, pmdoc.BuiltSchemaVersion())
	}
	types := map[string]pmdoc.BlockTypeSchema{}
	for _, typ := range installed.Types {
		types[typ.Name] = typ
	}
	var supertags []string
	rows, err := database.Pool.Query(ctx, `select id from graph_nodes where kind = 'supertag' order by id`)
	if err != nil {
		t.Fatalf("read the supertag nodes: %v", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan a supertag node: %v", err)
		}
		supertags = append(supertags, id)
	}
	rows.Close()
	for _, name := range []string{"callout", "ask", "requirement", "plan_task", "check", "decision", "reading"} {
		if _, declared := types[name]; !declared || !slices.Contains(supertags, name) {
			t.Errorf("the type %q: declared by the installed schema %t, a supertag node of the store %t; want both", name, declared, slices.Contains(supertags, name))
		}
	}
	if reading := types["reading"]; !slices.Equal(reading.Parents, []string{"decision", "ask"}) {
		t.Errorf("reading declares the parents %v, want [decision ask]", reading.Parents)
	}
	if ask := types["ask"]; ask.Content != "paragraph+ bullet_list?" {
		t.Errorf("ask declares the content rule %q, want main's paragraph+ bullet_list?", ask.Content)
	}
	decisionType := types["decision"].Attributes
	for name, kind := range map[string]pmdoc.BlockAttributeKind{
		"uncertainty": pmdoc.BlockAttributeEnum, "uncertainty_reason": pmdoc.BlockAttributeString,
		"shaped": pmdoc.BlockAttributeBlockList, "from_ask": pmdoc.BlockAttributeAsk, "decided_by": pmdoc.BlockAttributeActor,
	} {
		if attribute, declared := decisionType.Lookup(name); !declared || attribute.Kind != kind {
			t.Errorf("decision declares %s as %q (declared %t), want %q", name, attribute.Kind, declared, kind)
		}
	}
	if decidedBy := decisionType.Get("decided_by"); !decidedBy.Server || decidedBy.InMarkdown() {
		t.Errorf("decision's decided_by is server %t and in markdown %t, want the server's and never in markdown", decidedBy.Server, decidedBy.InMarkdown())
	}
	if shaped, declared := types["ask"].Attributes.Lookup("shaped"); !declared || shaped.Kind != pmdoc.BlockAttributeBlockList {
		t.Errorf("ask declares shaped as %q (declared %t), want block[]", shaped.Kind, declared)
	}

	// The issue's first document: three requirements, and a decision claiming bob.
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", ":::requirement{#r1}\nOne.\n:::\n\n:::requirement{#r2}\nTwo.\n:::\n\n"+
		":::requirement{#r3}\nThree.\n:::\n\n:::decision{#f1 decided_by=\"bob\"}\nThe first document decides.\n:::\n")
	spec := readDecisionDocument(t, database, issue.Key, "spec")
	requireDecidedBy(t, database, "the issue's first document", spec.address("f1"), "alice")
	requireValue(t, database, spec.address("f1"), refs.ValueUncertainty, "")
	requireValue(t, database, spec.address("f1"), refs.ValueUncertaintyReason, "")

	// A person's answered ask, which the decision below settles. The write that creates its block
	// carries shaped: the settlement that opens the ask writes the ask's row and, in the same
	// transaction, the shaped tuple on the ask's node and its block_refs row marked with that
	// node, before any later index of the document.
	uploadAs(t, handler, issue.Key, "asks.md", ":::ask{#a1 urgency=\"med\" multiple=\"false\" shaped=\"dispatch://"+issue.Key+"/spec#b-r1\"}\n"+
		"Which transport?\n\n- REST: Plain HTTP\n- gRPC: Streaming\n:::\n", "alice")
	settleIssue(t, documents, issue.Key)
	askID := blockAskID(t, handler, issue.Key, "a1")
	askShaped, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindAsk, askID, []string{refs.TupleShaped})
	if err != nil {
		t.Fatalf("read the shaped of the ask %s: %v", askID, err)
	}
	var opened int64
	var openedPayload model.ReferenceChangesPayload
	if err := database.Pool.QueryRow(ctx, `select id, payload from events where type = 'ask.opened' and payload->>'id' = $1`, askID).Scan(&opened, &openedPayload); err != nil {
		t.Fatalf("read the ask.opened of %s: %v", askID, err)
	}
	// The event names the block whose backlink count the new tie moved, as a version event names
	// what its block index moved, so a page reading counts off the event refreshes it.
	if !slices.ContainsFunc(openedPayload.ReferencesChanged, func(changed model.ChangedReference) bool {
		return changed.Kind == refs.BlockNodeKind && changed.ID == spec.pointed("r1")
	}) {
		t.Errorf("the ask.opened of %s names the changed references %+v, want the block %s its shaped tie reaches", askID, openedPayload.ReferencesChanged, spec.pointed("r1"))
	}
	if len(askShaped) != 1 || askShaped[0].ToKind != refs.BlockNodeKind || askShaped[0].ToID != spec.pointed("r1") {
		t.Fatalf("the ask %s states the shaped %+v once its settlement opened it, want one tuple to %s", askID, askShaped, spec.pointed("r1"))
	}
	requireCaused(t, "the new ask's shaped", askShaped[0], "alice", opened)
	var marked int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from block_refs
		where block_id = 'a1' and kind = 'shaped' and node_kind = $1 and node_address = $2 and to_id = $3
	`, graphstore.KindAsk, askID, spec.pointed("r1")).Scan(&marked); err != nil {
		t.Fatalf("read the new ask's shaped row: %v", err)
	}
	if marked != 1 {
		t.Fatalf("the new ask's shaped has %d block_refs rows marked ('ask', %s), want one", marked, askID)
	}
	answerDecisionBlock(t, handler, askID, "alice", []string{"REST"}, "", nil)
	expectRefusal(t, "uncertainty on the answered ask", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"name": "asks.md", "content": ":::ask{#a1 urgency=\"med\" multiple=\"false\" uncertainty=\"low\"}\nWhich transport?\n\n- REST: Plain HTTP\n- gRPC: Streaming\n:::\n",
		"precondition": uploadBaseNamed(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", "asks.md"),
	}, "alice"), http.StatusBadRequest, "INVALID_MARKDOWN", `attribute "uncertainty"`)
	refusal := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"name": "asks.md", "content": ":::ask{#a1 urgency=\"med\" multiple=\"false\" uncertainty=\"low\"}\nWhich transport?\n\n- REST: Plain HTTP\n- gRPC: Streaming\n:::\n",
		"precondition": uploadBaseNamed(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", "asks.md"),
	}, "alice")
	for _, never := range []string{"alice", "answer", "content", "paragraph"} {
		if strings.Contains(refusal.Body.String(), never) {
			t.Errorf("the refusal of uncertainty on the answered ask names %q, want the undeclared attribute alone: %s", never, refusal.Body.String())
		}
	}

	// The upload: one decision carrying each attribute, with a reading, and one claiming bob and
	// carrying no certainty at all.
	shaped := "dispatch://" + issue.Key + "/spec#b-r1 dispatch://" + issue.Key + "/spec#b-r2 dispatch://" + issue.Key + "/spec#b-r3"
	decided := "::::decision{#d1 shaped=\"" + shaped + "\" from_ask=\"dispatch://" + issue.Key + "/ask/" + askID +
		"\" uncertainty=\"low\" uncertainty_reason=\"A client that needs streaming would change it.\" decided_by=\"bob\"}\nWe chose REST.\n\n" +
		":::reading{#g1}\nThe agent reads it as REST for every service.\n:::\n::::\n\n"
	upload := decided + ":::decision{#u1 decided_by=\"bob\"}\nThe upload decides.\n:::\n"
	uploadAs(t, handler, issue.Key, "choices.md", upload, "alice")
	decisions := readDecisionDocument(t, database, issue.Key, "choices-md")
	uploaded := versionEvent(t, database, decisions.id, 1)
	d1 := decisions.address("d1")
	requireSupertag(t, database, d1, "decision")
	requireSupertag(t, database, decisions.address("g1"), "reading")
	shapedTies := currentTies(t, database, d1, refs.TupleShaped)
	if len(shapedTies) != 3 {
		t.Fatalf("d1 states %d shaped tuples, want one for each of the three requirements: %+v", len(shapedTies), shapedTies)
	}
	for index, tuple := range shapedTies {
		if want := spec.pointed("r" + string(rune('1'+index))); tuple.ToKind != refs.BlockNodeKind || tuple.ToID != want {
			t.Errorf("shaped tuple %d of d1 reaches %s/%s, want block %s", index+1, tuple.ToKind, tuple.ToID, want)
		}
		requireCaused(t, "a shaped tuple of d1", tuple, "alice", uploaded)
	}
	// d1's from_ask names the ask of an ask block of asks.md, a document of its issue, so d1 stands
	// under that ask's node (R2 of the issue-tree spec), while its shaped stays a relation of its own.
	if parents := currentTies(t, database, d1, graphstore.TuplePartOf); len(parents) != 1 || parents[0].ToKind != graphstore.KindAsk || parents[0].ToID != askID {
		t.Errorf("d1 states the part_of %+v, want one, to the ask %s it settles: its placement and its shaped stay apart", parents, askID)
	}
	fromAsk := currentTies(t, database, d1, refs.TupleFromAsk)
	if len(fromAsk) != 1 || fromAsk[0].ToKind != graphstore.KindAsk || fromAsk[0].ToID != askID {
		t.Fatalf("d1 states the from_ask %+v, want the ask %s", fromAsk, askID)
	}
	requireCaused(t, "d1's from_ask", fromAsk[0], "alice", uploaded)
	requireCaused(t, "d1's decided_by", requireDecidedBy(t, database, "the upload", d1, "alice"), "alice", uploaded)
	// The Decisions read answers the question d1 settles on d1's own entry, from its from_ask tuple,
	// and an ask's own entry settles none.
	entries := readEntries(t, handler, issue.Key)
	if entry := requireEntry(t, entries, "d1", refs.BlockNodeKind+":"+d1); !slices.Equal(entry.FromAsk, []string{askID}) {
		t.Errorf("d1's entry answers the from_ask %v, want the ask %s its from_ask tuple reaches", entry.FromAsk, askID)
	}
	if entry := requireEntry(t, entries, "the ask d1 settles", graphstore.KindAsk+":"+askID); entry.FromAsk == nil || len(entry.FromAsk) != 0 {
		t.Errorf("the ask's own entry answers the from_ask %v, want [] (not null)", entry.FromAsk)
	}
	requireValue(t, database, d1, refs.ValueUncertainty, "low")
	requireValue(t, database, d1, refs.ValueUncertaintyReason, "A client that needs streaming would change it.")
	requireValueCaused(t, database, d1, refs.ValueUncertainty, "alice", uploaded)
	requireValueCaused(t, database, d1, refs.ValueUncertaintyReason, "alice", uploaded)
	if parents := currentTies(t, database, decisions.address("g1"), graphstore.TuplePartOf); len(parents) != 1 || parents[0].ToID != d1 {
		t.Errorf("the reading g1 states the part_of %+v, want d1, the decision it reads", parents)
	}
	if authors := currentTies(t, database, decisions.address("g1"), graphstore.TupleAuthoredBy); len(authors) != 1 || authors[0].ToID != "alice" {
		t.Errorf("the reading g1 states the authored_by %+v, want alice, whose credential the write proved", authors)
	}
	requireDecidedBy(t, database, "the upload", decisions.address("u1"), "alice")
	requireValue(t, database, decisions.address("u1"), refs.ValueUncertainty, "")
	requireValue(t, database, decisions.address("u1"), refs.ValueUncertaintyReason, "")

	// An agent's upload of a decision claiming a person, with no uncertainty at all - the write a
	// forged author needs: the stored author is the agent's session, which its credential proved.
	requireAgentWrite(t, "an agent's upload", sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"name": "agent.md", "content": ":::decision{#s1 decided_by=\"alice\"}\nThe agent decides.\n:::\n", "actor": sessionActor(),
	}), http.StatusCreated)
	agent := readDecisionDocument(t, database, issue.Key, "agent-md")
	requireDecidedByNode(t, database, "an agent's upload claiming alice", agent.address("s1"), graphstore.KindSession, decidingSession)
	requireValue(t, database, agent.address("s1"), refs.ValueUncertainty, "")
	requireValue(t, database, agent.address("s1"), refs.ValueUncertaintyReason, "")

	// The markdown read back renders shaped as serves is rendered, and no decided_by; the same
	// markdown uploaded again retracts no shaped tuple.
	markdown := documentMarkdown(t, handler, decisions.id)
	if !strings.Contains(markdown, `shaped="`+shaped+`"`) || strings.Contains(markdown, "decided_by") {
		t.Fatalf("choices.md reads back as\n%s\nwant d1's shaped rendered as serves is, %q, and no decided_by", markdown, `shaped="`+shaped+`"`)
	}
	uploadAs(t, handler, issue.Key, "choices.md", markdown, "alice")
	history, err := graphstore.History(ctx, database.Pool, refs.BlockNodeKind, d1, refs.TupleShaped)
	if err != nil {
		t.Fatalf("read the history of d1's shaped: %v", err)
	}
	for _, tuple := range history {
		if !tuple.Asserted {
			t.Errorf("uploading choices.md's own markdown retracted d1's shaped tuple to %s", tuple.ToID)
		}
	}
	if len(history) != 3 || len(currentTies(t, database, d1, refs.TupleShaped)) != 3 {
		t.Errorf("d1's shaped history holds %d tuples, want the three its first write appended and no other", len(history))
	}

	// The upload tries to alter the stored author, and another writer edits the decision.
	uploadAs(t, handler, issue.Key, "choices.md", decided+":::decision{#u1 decided_by=\"carol\"}\nThe upload decides again.\n:::\n", "alice")
	requireDecidedBy(t, database, "an upload claiming carol", decisions.address("u1"), "alice")
	uploadAs(t, handler, issue.Key, "choices.md", decided+":::decision{#u1}\nBob rewords the upload's decision.\n:::\n", "bob")
	requireDecidedBy(t, database, "another writer's upload", decisions.address("u1"), "alice")

	// An index pass over the document leaves from_ask and decided_by standing. The pass is read by
	// what it did: it indexes choices.md, the one document owed, at its newest version, so a pass
	// that indexed nothing cannot stand in for it.
	if _, err := database.Pool.Exec(ctx, `update document_block_versions set version = null where artifact_id = $1`, decisions.id); err != nil {
		t.Fatalf("leave choices.md owed its block index: %v", err)
	}
	var owed int
	if err := database.Pool.QueryRow(ctx, `select count(*) from document_block_versions where version is null`).Scan(&owed); err != nil || owed != 1 {
		t.Fatalf("%d documents are owed their block index (%v), want choices.md alone", owed, err)
	}
	tick, err := documents.ReconcileBlockIndex(ctx, docs.BlockIndexCursors{}, nil)
	if err != nil {
		t.Fatalf("run the block-index reconciliation: %v", err)
	}
	newest := latestVersion(t, database, decisions.id).Number
	var indexed int
	if err := database.Pool.QueryRow(ctx, `select coalesce(version, 0) from document_block_versions where artifact_id = $1`, decisions.id).Scan(&indexed); err != nil {
		t.Fatalf("read the version choices.md is indexed at: %v", err)
	}
	if tick.Documents != 1 || tick.LiveAhead != 0 || tick.OwedDocuments != 0 || indexed != newest {
		t.Fatalf("the index pass read %+v and left choices.md indexed at version %d (0 for owed), want it to index that one document at its newest version %d and leave none owed",
			tick, indexed, newest)
	}
	t.Logf("the index pass indexed %d document, choices.md at version %d, and left %d owed", tick.Documents, indexed, tick.OwedDocuments)
	if held := currentTies(t, database, d1, refs.TupleFromAsk); len(held) != 1 || held[0].ToID != askID {
		t.Errorf("after an index pass d1 states the from_ask %+v, want the ask %s still standing", held, askID)
	}
	requireDecidedBy(t, database, "an index pass", d1, "alice")
	requireDecidedBy(t, database, "an index pass", decisions.address("u1"), "alice")

	// The first document's decision: a later write claiming carol moves nothing of it. The issue
	// is created once, so the claim reaches the same node through an edit of its spec.
	editSpec(t, handler, issue.Key, []map[string]any{
		{"op": "retype", "block": "f1", "type": "decision", "attributes": map[string]any{"decided_by": "carol"}},
		{"op": "insert", "after": "end", "markdown": "The spec grows.\n"},
	})
	requireDecidedBy(t, database, "the first document's decision after a claim of carol", spec.address("f1"), "alice")

	// An agent's issue: its first document holds a decision claiming alice, with no uncertainty,
	// and the stored author is the agent's session.
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "CORE", "title": "Quarterly hiring forecast", "actor": sessionActor(),
		"spec": ":::decision{#s3 decided_by=\"alice\"}\nThe agent's first document decides.\n:::\n",
	})
	requireAgentWrite(t, "an agent's issue", created, http.StatusCreated)
	agentSpec := readDecisionDocument(t, database, decodeBody[model.Issue](t, created).Key, "spec")
	requireDecidedByNode(t, database, "an agent's first document claiming alice", agentSpec.address("s3"), graphstore.KindSession, decidingSession)
	requireValue(t, database, agentSpec.address("s3"), refs.ValueUncertainty, "")

	// The editor operation: a paragraph retyped into a decision claiming bob, then a retype
	// claiming carol, then an edit by another writer.
	uploadAs(t, handler, issue.Key, "editor.md", "The editor decides.\n", "alice")
	editor := readDecisionDocument(t, database, issue.Key, "editor-md")
	paragraph := blockIDsByText(t, handler, editor.id)["The editor decides."]
	editIssueDocument(t, handler, issue.Key, "editor-md", []map[string]any{{"op": "retype", "block": paragraph, "type": "decision", "attributes": map[string]any{"decided_by": "bob"}}})
	requireDecidedBy(t, database, "the editor operation", editor.address(paragraph), "alice")
	requireValue(t, database, editor.address(paragraph), refs.ValueUncertainty, "")
	editIssueDocument(t, handler, issue.Key, "editor-md", []map[string]any{
		{"op": "retype", "block": paragraph, "type": "decision", "attributes": map[string]any{"decided_by": "carol"}},
		{"op": "insert", "after": "end", "markdown": "More.\n"},
	})
	requireDecidedBy(t, database, "an editor operation claiming carol", editor.address(paragraph), "alice")
	editIssueDocumentAs(t, handler, issue.Key, "editor-md", "bob", []map[string]any{{"op": "replace", "find": "The editor decides.", "with": "Bob rewords the editor's decision."}})
	if markdown := documentMarkdown(t, handler, editor.id); !strings.Contains(markdown, "Bob rewords the editor's decision.") {
		t.Fatalf("bob's edit of the decision's own words left editor.md as\n%s", markdown)
	}
	requireDecidedBy(t, database, "another writer's edit of the decision's words", editor.address(paragraph), "alice")

	// An agent's editor operation: a decision it inserts claiming alice, with no uncertainty.
	requireAgentWrite(t, "an agent's editor operation", sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts/editor-md/edits", map[string]any{
		"ops":   []map[string]any{{"op": "insert", "after": "end", "markdown": ":::decision{#s2 decided_by=\"alice\"}\nThe agent's edit decides.\n:::\n"}},
		"actor": sessionActor(),
	}), http.StatusOK)
	requireDecidedByNode(t, database, "an agent's editor operation claiming alice", editor.address("s2"), graphstore.KindSession, decidingSession)
	requireValue(t, database, editor.address("s2"), refs.ValueUncertainty, "")

	// The browser's settlement: alice's editor writes a decision claiming bob and a Keep nobody
	// made, then sets carol on it; settlement stores each version, moves nothing of the author,
	// and overwrites the live tree's server-owned attributes with what the store holds, so no
	// editor reads a value the store does not hold.
	uploadAs(t, handler, issue.Key, "browser.md", "Before.\n", "alice")
	browser := readDecisionDocument(t, database, issue.Key, "browser-md")
	peer := connectBrowserPeer(t, handler, documents, browser.id)
	t.Cleanup(peer.close)
	peer.edit(t, func(tree *pmdoc.Node) error {
		tree.Children = append(tree.Children, decisionNode("b1", "The browser decides.", pmdoc.Attrs{
			"decided_by": "bob", "kept_by": "mallory", "kept_text_sum": "forged",
		}))
		return nil
	})
	peer.barrier(t)
	settleIssue(t, documents, issue.Key)
	if latestVersion(t, database, browser.id).Number < 2 {
		t.Fatalf("settlement wrote no version of the browser's decision")
	}
	requireDecidedBy(t, database, "the browser's settlement", browser.address("b1"), "alice")
	requireValue(t, database, browser.address("b1"), refs.ValueUncertainty, "")
	// The store held no author, Keep or kept sum of b1 when the settlement read it, so none of the
	// forged values stands in the live tree.
	requireLiveDecision(t, database, peer, browser.id, "b1", map[string]any{})
	before := latestVersion(t, database, browser.id).Number
	peer.edit(t, func(tree *pmdoc.Node) error {
		for index, child := range tree.Children {
			if child.Type == "decision" {
				tree.Children[index] = decisionNode("b1", "The browser decides again.", pmdoc.Attrs{"decided_by": "carol", "kept_by": "mallory"})
			}
		}
		return nil
	})
	peer.barrier(t)
	settleIssue(t, documents, issue.Key)
	if latestVersion(t, database, browser.id).Number <= before {
		t.Fatalf("settlement wrote no version of the browser's rewritten decision")
	}
	requireDecidedBy(t, database, "a browser's settlement claiming carol", browser.address("b1"), "alice")
	// The store holds alice as b1's author and no Keep, so the live tree reads alice and no
	// kept_by.
	requireLiveDecision(t, database, peer, browser.id, "b1", map[string]any{"decided_by": "alice"})
}

// requireLiveDecision fails unless the decision block id carries exactly want of its
// server-owned attributes, decided_by, kept_by and kept_text_sum, in the live document: as its
// stored history holds it (docs.StoredTreeForTest) and as the browser's editor holds it.
func requireLiveDecision(t *testing.T, database *store.Store, peer *syncedPeer, artifactID, id string, want map[string]any) {
	t.Helper()
	stored, err := docs.StoredTreeForTest(context.Background(), database, artifactID)
	if err != nil {
		t.Fatalf("read the stored tree of %s: %v", artifactID, err)
	}
	peer.barrier(t)
	editor, err := pmdoc.Read(peer.doc.GetXmlFragment(peer.fragment))
	if err != nil {
		t.Fatalf("read the browser editor's tree: %v", err)
	}
	for source, tree := range map[string]*pmdoc.Node{"the stored document": stored, "the browser's editor": editor} {
		var decision *pmdoc.Node
		pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
			if node.Type == "decision" && node.Attrs[pmdoc.BlockIDAttr] == id {
				decision = node
			}
			return decision == nil
		})
		if decision == nil {
			t.Fatalf("%s holds no decision %s", source, id)
		}
		got := map[string]any{}
		for _, name := range []string{"decided_by", "kept_by", "kept_text_sum"} {
			if value, held := decision.Attrs[name]; held && value != nil {
				got[name] = value
			}
		}
		if !maps.Equal(got, want) {
			t.Errorf("%s shows decision %s with the server-owned attributes %v, want %v, which the store holds", source, id, got, want)
		}
	}
}

// The tie a write introduces to something of another issue is refused on every write that takes
// markdown - the upload, the editor operation and the issue's first document - quoting the
// attribute and the value: a decision's from_ask naming an ask of another issue, and a shaped
// naming a block of another issue. A browser's write is never refused, and settlement stores it
// with no tie to the other issue stated, while a tie of the same write inside the issue stands.
func TestATieToAnotherIssueIsRefusedOnEveryWritePath(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour, browserPeers: true})
	documents := deps.Docs.(*docs.Service)
	createReferenceAPIProject(t, handler, "CORE")
	requirement := ":::requirement{#r1}\nA requirement.\n:::\n"
	titled := func(title string) model.Issue {
		response := createIssueRequest(t, handler, map[string]any{"project": "CORE", "title": title, "spec": requirement})
		if response.Code != http.StatusCreated {
			t.Fatalf("create the issue %q: status=%d body=%s", title, response.Code, response.Body.String())
		}
		return decodeBody[model.Issue](t, response)
	}
	other := titled("Another team's transport")
	issue := titled("Our storage engine")
	const foreignAsk = "33333333-3333-4333-8333-333333333333"
	fromAsk := "dispatch://" + other.Key + "/ask/" + foreignAsk
	shaped := "dispatch://" + other.Key + "/spec#b-r1"
	for _, tie := range []struct {
		attribute, value, markdown string
	}{
		{"from_ask", fromAsk, ":::decision{#d1 from_ask=\"" + fromAsk + "\"}\nWe chose theirs.\n:::\n"},
		{"shaped", shaped, ":::decision{#d1 shaped=\"" + shaped + "\"}\nWe chose theirs.\n:::\n"},
	} {
		t.Run(tie.attribute, func(t *testing.T) {
			names := []string{`"` + tie.attribute + `"`, tie.value, other.Key, issue.Key}
			expectRefusal(t, "an upload", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
				"name": "ties-" + tie.attribute + ".md", "content": tie.markdown,
			}, "alice"), http.StatusBadRequest, "INVALID_MARKDOWN", names...)
			expectRefusal(t, "an editor operation", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts/spec/edits", map[string]any{
				"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": tie.markdown}},
			}, "alice"), http.StatusBadRequest, "INVALID_OP", names[:2]...)
			expectRefusal(t, "the issue's first document", createIssueRequest(t, handler, map[string]any{
				"project": "CORE", "title": "Quarterly hiring plan " + tie.attribute, "spec": tie.markdown,
			}), http.StatusBadRequest, "INVALID_MARKDOWN", names[:3]...)
		})
	}

	// The browser's settlement: the write is stored, its tie inside the issue stands, and no tie to
	// the other issue is stated.
	uploadAs(t, handler, issue.Key, "browser.md", "Before.\n", "alice")
	browser := readDecisionDocument(t, database, issue.Key, "browser-md")
	own := "dispatch://" + issue.Key + "/spec#b-r1"
	peer := connectBrowserPeer(t, handler, documents, browser.id)
	t.Cleanup(peer.close)
	peer.edit(t, func(tree *pmdoc.Node) error {
		tree.Children = append(tree.Children, decisionNode("b1", "The browser ties elsewhere.", pmdoc.Attrs{
			"from_ask": fromAsk, "shaped": []string{own, shaped},
		}))
		return nil
	})
	peer.barrier(t)
	settleIssue(t, documents, issue.Key)
	if latestVersion(t, database, browser.id).Number < 2 {
		t.Fatalf("settlement refused the browser's write, want it stored")
	}
	address := browser.address("b1")
	if held := currentTies(t, database, address, refs.TupleShaped); len(held) != 1 || held[0].ToID != readDecisionDocument(t, database, issue.Key, "spec").pointed("r1") {
		t.Errorf("the browser's decision states the shaped %+v, want its tie inside %s alone", held, issue.Key)
	}
	if held := currentTies(t, database, address, refs.TupleFromAsk); len(held) != 0 {
		t.Errorf("the browser's decision states the from_ask %+v, want none to an ask of %s", held, other.Key)
	}
	var rows int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from block_refs where artifact_id = $1 and block_id = 'b1' and (to_id = $2 or to_id like $3)
	`, browser.room, foreignAsk, other.Key+"/%").Scan(&rows); err != nil {
		t.Fatalf("count the browser's rows into %s: %v", other.Key, err)
	}
	if rows != 0 {
		t.Errorf("the browser's decision holds %d block_refs rows into %s, want none", rows, other.Key)
	}
	requireDecidedBy(t, database, "the browser's tied decision", address, "alice")
}

// connectPeerAs connects login's browser to artifactID's room, through a document server of its
// own over documentService, binding the fragment the artifact read through handler answers.
func connectPeerAs(t *testing.T, handler http.Handler, documentService *docs.Service, artifactID, login string) *syncedPeer {
	t.Helper()
	sockets := &servedSockets{finished: make(map[string]chan struct{})}
	server := httptest.NewServer(sockets.serve(documentService.ServeHTTP))
	t.Cleanup(server.Close)
	peer := &syncedPeer{
		wsURL: "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/doc/" + artifactID, sockets: sockets,
		headers: http.Header{"X-Dispatch-User": []string{login}}, artifactID: artifactID,
		fragment: artifactFragment(t, handler, artifactID), doc: crdt.New(),
	}
	peer.connect(t)
	return peer
}

// requireTypedNode fails unless the block's node has the supertag want and states one part_of,
// to toKind/toID.
func requireTypedNode(t *testing.T, database *store.Store, what, address, want, toKind, toID string) {
	t.Helper()
	requireSupertag(t, database, address, want)
	if parents := currentTies(t, database, address, graphstore.TuplePartOf); len(parents) != 1 || parents[0].ToKind != toKind || parents[0].ToID != toID {
		t.Errorf("%s: %s states the part_of %+v, want one, to %s/%s", what, address, parents, toKind, toID)
	}
}

// A settlement's version lists every person who edited the document since the version before,
// and that list names nobody's decision: alice and bob both edit before one settlement - alice an
// unrelated paragraph while both are connected, bob, once alone, a decision holding a reading -
// and the decision's decided_by and the reading's authored_by name bob, who introduced the block,
// never alice, whom the version lists first. The author is the one the room's update observer
// recorded for the update that introduced the block (docs' askBlockSources.author).
func TestADecisionInASettlementOfSeveralWritersNamesTheOneWhoWroteIt(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour, browserPeers: true})
	documents := deps.Docs.(*docs.Service)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "A spec.\n")
	uploadAs(t, handler, issue.Key, "browser.md", "Before.\n", "alice")
	browser := readDecisionDocument(t, database, issue.Key, "browser-md")

	bob := connectPeerAs(t, handler, documents, browser.id, "bob")
	t.Cleanup(bob.close)
	alice := connectPeerAs(t, handler, documents, browser.id, "alice")
	alice.appendParagraph(t, "Alice adds an unrelated line.")
	alice.barrier(t)
	alice.closeAndWait(t)
	bob.barrier(t)
	decision := decisionNode("b1", "Bob decides.", nil)
	decision.Children = append(decision.Children, &pmdoc.Node{Type: "reading", Attrs: pmdoc.Attrs{pmdoc.BlockIDAttr: "g1"}, Children: []*pmdoc.Node{
		{Type: "paragraph", Children: []*pmdoc.Node{{Type: "text", Text: "Bob reads his own decision."}}},
	}})
	bob.edit(t, func(tree *pmdoc.Node) error {
		tree.Children = append(tree.Children, decision)
		return nil
	})
	bob.barrier(t)
	settleIssue(t, documents, issue.Key)

	settled := latestVersion(t, database, browser.id)
	var authors []string
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce(array_agg(a->>'id' order by a->>'id'), '{}') from artifact_versions v, jsonb_array_elements(v.authors) a
		where v.artifact_id = $1 and v.number = $2
	`, browser.id, settled.Number).Scan(&authors); err != nil {
		t.Fatalf("read the settled version's authors: %v", err)
	}
	if !slices.Equal(authors, []string{"alice", "bob"}) {
		t.Fatalf("the settled version %d lists the authors %v, want alice and bob: the settlement of several writers this case needs", settled.Number, authors)
	}
	requireDecidedBy(t, database, "a settlement alice and bob both wrote", browser.address("b1"), "bob")
	if held := currentTies(t, database, browser.address("g1"), graphstore.TupleAuthoredBy); len(held) != 1 || held[0].ToKind != graphstore.KindPerson || held[0].ToID != "bob" {
		t.Errorf("the reading g1 states the authored_by %+v, want bob, who wrote it; the version lists alice first", held)
	}
}

// A reading inside an ask stands under the ask the block shows (R2: the parent of a reading inside
// an ask is the ask node). A copy of an answered ask, on another document of the issue, opens no
// row of its own and shows its source's answer, so a reading in it is a reading node whose part_of
// is the source ask's node, its document_blocks row still naming the copy's block. A browser's new
// ask block holding a reading shows no ask at all - no row, no source - and settlement flags it
// invalid, so the reading in it is a block like any other: supertag block, placed in the ask block.
func TestAReadingInAnAskBlockStandsUnderTheAskTheBlockShows(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour, browserPeers: true})
	documents := deps.Docs.(*docs.Service)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "A spec.\n")
	const ask = ":::ask{#a1 urgency=\"med\" multiple=\"false\"}\nWhich transport?\n\n- REST: Plain HTTP\n- gRPC: Streaming\n:::\n"
	uploadAs(t, handler, issue.Key, "asks.md", ask, "alice")
	settleIssue(t, documents, issue.Key)
	askID := blockAskID(t, handler, issue.Key, "a1")
	answerDecisionBlock(t, handler, askID, "alice", []string{"REST"}, "", nil)

	t.Run("a copy of an answered ask", func(t *testing.T) {
		uploadAs(t, handler, issue.Key, "copy.md", "::::ask{#a1 urgency=\"med\" multiple=\"false\"}\nWhich transport?\n\n- REST: Plain HTTP\n- gRPC: Streaming\n\n"+
			":::reading{#g1}\nThe agent reads the copied answer as REST.\n:::\n::::\n", "alice")
		copied := readDecisionDocument(t, database, issue.Key, "copy-md")
		var rows int
		if err := database.Pool.QueryRow(context.Background(), `select count(*) from asks where block_artifact_id = $1`, copied.id).Scan(&rows); err != nil {
			t.Fatalf("count the copy's asks: %v", err)
		}
		if rows != 0 {
			t.Fatalf("the copy opened %d asks of its own, want none: it shows its source's", rows)
		}
		requireTypedNode(t, database, "a reading in a copy of an answered ask", copied.address("g1"), "reading", graphstore.KindAsk, askID)
		var parent *string
		if err := database.Pool.QueryRow(context.Background(), `
			select parent_block_id from document_blocks where artifact_id = $1 and block_id = 'g1'
		`, copied.id).Scan(&parent); err != nil {
			t.Fatalf("read the reading's row: %v", err)
		}
		if parent == nil || *parent != "a1" {
			t.Errorf("the reading's row names the parent %v, want the copy's ask block a1, the projection of its part_of", parent)
		}
	})

	t.Run("a browser's new ask block", func(t *testing.T) {
		uploadAs(t, handler, issue.Key, "browser.md", "Before.\n", "alice")
		browser := readDecisionDocument(t, database, issue.Key, "browser-md")
		writeBrowserDocument(t, handler, documents, browser.id, "Before.\n\n::::ask{#a9 urgency=\"med\" multiple=\"false\"}\nShould we ship?\n\n- Ship: Release it\n- Hold: Wait\n\n"+
			":::reading{#g9}\nThe agent reads it as shipping.\n:::\n::::\n")

		settleIssue(t, documents, issue.Key)
		if latestVersion(t, database, browser.id).Number < 2 {
			t.Fatalf("settlement wrote no version of the browser's ask block")
		}
		var rows int
		if err := database.Pool.QueryRow(context.Background(), `select count(*) from asks where block_artifact_id = $1`, browser.id).Scan(&rows); err != nil {
			t.Fatalf("count the browser document's asks: %v", err)
		}
		if rows != 0 {
			t.Fatalf("the browser's ask block holding a reading opened %d asks, want none: settlement flags it", rows)
		}
		requireTypedNode(t, database, "a reading in a new ask block that shows no ask", browser.address("g9"), refs.BlockSupertag, refs.BlockNodeKind, browser.address("a9"))
	})
}
