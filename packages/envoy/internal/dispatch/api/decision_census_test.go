package api

import (
	"context"
	"crypto/rand"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// requireTheCensusAgreesOver takes the store census over database as `envoy-dispatch census
// --store` takes it and fails unless it read no difference in either direction over any kind,
// every far end of a kind no table projects resolves, and each kind of populated read rows and
// live nodes, since zero differences over a kind that compared nothing prove nothing of it. It
// logs every kind it read, so a failure says which side of which kind drifted.
func requireTheCensusAgreesOver(t *testing.T, database *store.Store, what string, populated ...string) *graphcensus.StoreReport {
	t.Helper()
	report, err := graphcensus.StoreCensus(context.Background(), database.Pool.Config().ConnString(), false, docs.ReturnDeclined)
	if err != nil {
		t.Fatalf("take the store census %s: %v", what, err)
	}
	read := map[string]graphcensus.KindReport{}
	for _, kind := range report.Kinds {
		read[kind.Kind] = kind
		t.Logf("%s: kind=%s rows=%d nodes=%d row differences=%d node differences=%d",
			what, kind.Kind, kind.Rows, kind.Nodes, kind.RowDifferences, kind.NodeDifferences)
	}
	for _, name := range populated {
		if kind := read[name]; kind.Rows == 0 || kind.Nodes == 0 {
			t.Errorf("the store census %s read %d rows and %d live nodes of kind %s, which this test wrote: it compared nothing there",
				what, kind.Rows, kind.Nodes, name)
		}
	}
	if report.Differences() != 0 || report.Unresolved() != 0 {
		var printed strings.Builder
		report.Write(&printed)
		t.Fatalf("the store census %s reads %d difference(s) and %d far end(s) that do not resolve:\n%s",
			what, report.Differences(), report.Unresolved(), printed.String())
	}
	return report
}

// requireReadingParent fails unless the document_blocks row of the reading names the block want
// as its parent: the ask block holding it, which its part_of to the ask's node projects. The row is
// keyed by the artifact the document's blocks are keyed under, its issue room's tree for a
// converted document (decisionDocument.room).
func requireReadingParent(t *testing.T, database *store.Store, document decisionDocument, reading, want string) {
	t.Helper()
	var parent *string
	if err := database.Pool.QueryRow(context.Background(), `
		select parent_block_id from document_blocks where artifact_id = $1 and block_id = $2
	`, document.room, reading).Scan(&parent); err != nil {
		t.Fatalf("read the row of the reading %s: %v", document.address(reading), err)
	}
	if parent == nil || *parent != want {
		t.Fatalf("the row of the reading %s names the parent %v, want the ask block %s holding it", document.address(reading), parent, want)
	}
}

// A reading inside an answered ask hangs on the ask's node (its part_of), while its
// document_blocks row keeps the id of the ask block holding it as its parent_block_id, which is
// that tuple's projection (refs' reconcileBlockParents). The census reads the two as one link: the
// block kind and reading_parent each read the ask's node as the ask block of the reading's own
// document, for a reading in the ask's own document and for one in a copy of the ask on another
// document of the issue, whose block keeps its source's id. A census that read the part_of as it
// stands reads every such reading as a row and a node that disagree, and refuses.
func TestTheCensusReadsAReadingInAnAnsweredAskAsTheAskBlocksChild(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	documents := deps.Docs.(*docs.Service)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", ":::requirement{#r1}\nOne.\n:::\n")
	const question = "Which transport?\n\n- REST: Plain HTTP\n- gRPC: Streaming\n"
	uploadAs(t, handler, issue.Key, "asks.md", ":::ask{#a1 urgency=\"med\" multiple=\"false\" shaped=\"dispatch://"+issue.Key+"/spec#b-r1\"}\n"+question+":::\n", "alice")
	settleIssue(t, documents, issue.Key)
	askID := blockAskID(t, handler, issue.Key, "a1")
	answerDecisionBlock(t, handler, askID, "bob", []string{"REST"}, "Plain HTTP for now.", nil)

	uploadAs(t, handler, issue.Key, "asks.md", "::::ask{#a1 urgency=\"med\" multiple=\"false\" shaped=\"dispatch://"+issue.Key+"/spec#b-r1\"}\n"+question+
		"\n:::reading{#g1}\nThe agent reads it as REST for every service.\n:::\n::::\n", "alice")
	own := readDecisionDocument(t, database, issue.Key, "asks-md")
	requireTypedNode(t, database, "a reading in the answered ask", own.address("g1"), refs.ReadingSupertag, graphstore.KindAsk, askID)
	requireReadingParent(t, database, own, "g1", "a1")

	uploadAs(t, handler, issue.Key, "copy.md", "::::ask{#a1 urgency=\"med\" multiple=\"false\"}\n"+question+
		"\n:::reading{#g2}\nThe agent reads the copied answer as REST.\n:::\n::::\n", "alice")
	copied := readDecisionDocument(t, database, issue.Key, "copy-md")
	requireTypedNode(t, database, "a reading in a copy of the answered ask", copied.address("g2"), refs.ReadingSupertag, graphstore.KindAsk, askID)
	requireReadingParent(t, database, copied, "g2", "a1")

	report := requireTheCensusAgreesOver(t, database, "over readings in an answered ask and in its copy",
		"block", "reading_parent", "shaped", "ask_parent", "ask_anchor", "ask_answer")
	for _, kind := range report.Kinds {
		if kind.Kind == "reading_parent" && kind.Rows != 2 {
			t.Errorf("reading_parent read %d rows, want the two readings this test wrote", kind.Rows)
		}
	}
}

// An agent whose only acts are writing a decision and a reading is a node of the store all the
// same: the typed-node pass writes its session's node beside the decision's decided_by, which a
// block_refs row projects, and the reading's authored_by, which no table projects, and, for a
// session the server proved a person's, that person's node and the alias_of tying the two
// (graphstore.AuthorFacts). The session here writes nothing else - no comment, no ask, no follow -
// and the person it belongs to writes nothing at all, the project and the issue being another
// person's, so a census whose session and person row sides read only those other tables reads
// both as a live node no row projects.
func TestTheCensusHoldsAnAgentWhoseOnlyActsAreADecisionAndAReading(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	ctx := context.Background()
	asBob := func(method, path string, body any) {
		t.Helper()
		if response := rig.serve(withPerson(brokerRequest(t, method, path, body, "", ""), "bob")); response.Code != http.StatusCreated {
			t.Fatalf("%s %s as bob: status=%d body=%s", method, path, response.Code, response.Body.String())
		}
	}
	asBob(http.MethodPost, "/api/v1/projects", map[string]any{"key": brokerTestProject, "name": "Broker"})
	asBob(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": brokerTestProject, "title": "A decision an agent writes", "spec": ":::requirement{#r1}\nOne.\n:::\n",
	})
	issue := brokerTestProject + "-1"

	caller := newBrokerCaller(t)
	const token = "a-token-for-the-deciding-agent"
	rig.broker.Says(token, liveNamedToken(caller.jkt))
	if response := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues/"+issue+"/artifacts", token, map[string]any{
		"name": "agent.md",
		"content": "::::decision{#d1 shaped=\"dispatch://" + issue + "/spec#b-r1\"}\nThe agent decides.\n\n" +
			":::reading{#g1}\nThe agent reads it so.\n:::\n::::\n",
	})); response.Code != http.StatusCreated {
		t.Fatalf("the agent's upload: status=%d body=%s", response.Code, response.Body.String())
	}
	agent := readDecisionDocument(t, rig.database, issue, "agent-md")
	requireDecidedByNode(t, rig.database, "the agent's decision", agent.address("d1"), graphstore.KindSession, brokerTestSession)
	if authors := currentTies(t, rig.database, agent.address("g1"), graphstore.TupleAuthoredBy); len(authors) != 1 ||
		authors[0].ToKind != graphstore.KindSession || authors[0].ToID != brokerTestSession {
		t.Fatalf("the agent's reading states the authored_by %+v, want the session %s", authors, brokerTestSession)
	}
	owners, err := graphstore.CurrentOut(ctx, rig.database.Pool, graphstore.KindSession, brokerTestSession, []string{graphstore.TupleAliasOf})
	if err != nil || len(owners) != 1 || owners[0].ToID != brokerTestPerson {
		t.Fatalf("the agent's session states the alias_of %+v (%v), want one to %s, the person the broker proved it belongs to", owners, err, brokerTestPerson)
	}
	var otherActs int
	if err := rig.database.Pool.QueryRow(ctx, `
		select (select count(*) from comments where author->>'id' in ($1, $2) or author->>'owner' = $2)
		     + (select count(*) from asks where author->>'id' in ($1, $2) or author->>'owner' = $2)
		     + (select count(*) from ask_followers where session_id = $1)
	`, brokerTestSession, brokerTestPerson).Scan(&otherActs); err != nil {
		t.Fatalf("count the other acts of the agent and its person: %v", err)
	}
	if otherActs != 0 {
		t.Fatalf("the agent or its person wrote %d comment, ask or follower row(s), want none: the decision and the reading are their only acts", otherActs)
	}
	requireTheCensusAgreesOver(t, rig.database, "over an agent whose only acts are a decision and a reading",
		graphstore.KindSession, graphstore.KindPerson, graphstore.TupleDecidedBy, refs.TupleShaped, "reading_parent")
}

// A person whose answer was taken off an ask stays a node of the store, as an author does: their
// node was made by the answer (asks.Settle), and a node is history and never goes. bob, who writes
// nothing else, answers alice's ask, and the ask is then opened again through the one writer of
// its answer state (asks.Settle with asks.Reopened, the change an issue's reopen makes), which
// takes the answer off the node and the row. No row names bob after the reopen; the store's log
// still holds his answer, and the census reads zero differences before and after, the person kind
// counting alice, who asked, and bob both times.
func TestTheCensusHoldsAPersonWhoseAnswerWasTakenOff(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	documents := deps.Docs.(*docs.Service)
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "A spec.\n")
	uploadAs(t, handler, issue.Key, "asks.md", ":::ask{#a1 urgency=\"med\" multiple=\"false\"}\nWhich transport?\n\n- REST: Plain HTTP\n- gRPC: Streaming\n:::\n", "alice")
	settleIssue(t, documents, issue.Key)
	askID := blockAskID(t, handler, issue.Key, "a1")
	bobNamed := func(what string) {
		t.Helper()
		var answered, node bool
		if err := database.Pool.QueryRow(ctx, `
			select coalesce((select answer->>'user' = 'bob' from asks where id = $1::uuid), false),
			       exists (select 1 from graph_nodes where kind = $2 and id = 'bob' and state = $3)
		`, askID, graphstore.KindPerson, graphstore.StateLive).Scan(&answered, &node); err != nil {
			t.Fatalf("%s: read bob's answer and node: %v", what, err)
		}
		if !node {
			t.Fatalf("%s: bob holds no live node, so this test proves nothing of his", what)
		}
		t.Logf("%s: the asks row names bob's answer %t, and bob's node is live", what, answered)
	}

	answerDecisionBlock(t, handler, askID, "bob", []string{"REST"}, "Plain HTTP for now.", nil)
	bobNamed("after bob's answer")
	requireTheCensusAgreesOver(t, database, "after bob's answer", graphstore.KindPerson, "ask_answer")

	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the reopen: %v", err)
	}
	defer tx.Rollback(ctx)
	change := asks.Reopened()
	change.Who, change.At, change.Event = model.Actor{Kind: "user", ID: "alice"}, time.Now().UTC(), graphstore.NoEvent
	if settled, err := asks.Settle(ctx, tx, askID, change); err != nil || settled.State != asks.StateOpen || settled.Answer != nil {
		t.Fatalf("reopen the ask: %+v, %v; want it open with no answer", settled, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the reopen: %v", err)
	}
	bobNamed("after the reopen")
	report := requireTheCensusAgreesOver(t, database, "after the reopen", graphstore.KindPerson)
	for _, kind := range report.Kinds {
		switch {
		case kind.Kind == graphstore.KindPerson && kind.Rows != 2:
			t.Errorf("after the reopen the person kind reads %d rows, want alice, who asked, and bob, whose answer the reopen took off", kind.Rows)
		case kind.Kind == "ask_answer" && (kind.Rows != 0 || kind.Nodes != 0):
			t.Errorf("after the reopen ask_answer reads %d rows and %d nodes, want none: the reopen took the answer off both", kind.Rows, kind.Nodes)
		}
	}
}

// incompressibleText is n lower-case alphanumeric bytes no B-tree compresses below n: a run of one
// byte compresses, so an address meant to stand past the store's bound needs bytes like these.
func incompressibleText(t *testing.T, n int) string {
	t.Helper()
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	text := make([]byte, n)
	if _, err := rand.Read(text); err != nil {
		t.Fatalf("read random bytes: %v", err)
	}
	for index, b := range text {
		text[index] = alphabet[int(b)%len(alphabet)]
	}
	return string(text)
}

// An ask one of whose documents is past the store's address bound is left out of the store's
// relations, and its row names no node, so graph_edges' old arms answer every edge of it
// (graphstore.AskPlaces.Fit); its writer writes its node, its authored_by and its author's node
// all the same (graphstore.AskFacts). bob, who writes nothing else, asks on a project document
// whose ref_key is past the bound: the census counts the ask apart, as an address past the bound,
// and holds bob to his node through the authored_by the ask's node states, so it reads zero
// differences where an author arm reading only the rows that name a node would read bob as a
// live node no row projects.
func TestTheCensusHoldsTheAuthorOfAnAskPastTheBound(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "LKY")
	// ref_key LKY/<slug> is 2,692 incompressible bytes: past the store's bound and inside the
	// B-tree bound artifacts_ref_key_key holds its own rows to.
	slug := incompressibleText(t, 2688)
	var document, refKey string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (project_key, slug, name, kind, created_by)
		values ('LKY', $1, $1, 'doc', '{"kind":"user","id":"alice"}')
		returning id::text, ref_key
	`, slug).Scan(&document, &refKey); err != nil {
		t.Fatalf("create the long-keyed document: %v", err)
	}
	if graphstore.AddressFits(refKey) {
		t.Fatalf("the document's ref_key fits the store's bound, so this proves nothing")
	}
	asked := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document+"/asks", map[string]any{
		"question": "Keep the document's long name?",
	}, "bob")
	if asked.Code != http.StatusCreated {
		t.Fatalf("bob asks on the long-keyed document: status=%d body=%s", asked.Code, asked.Body.String())
	}
	askID := decodeBody[writtenRecord](t, asked).ID
	var marked bool
	if err := database.Pool.QueryRow(ctx, `select node_kind is not null from asks where id = $1::uuid`, askID).Scan(&marked); err != nil {
		t.Fatalf("read the ask's row: %v", err)
	}
	authors, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindAsk, askID, []string{graphstore.TupleAuthoredBy})
	if err != nil {
		t.Fatalf("read the ask's authored_by: %v", err)
	}
	_, bobHeld, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindPerson, "bob")
	if err != nil || marked || !bobHeld || len(authors) != 1 || authors[0].ToID != "bob" {
		t.Fatalf("the ask's row names its node %t, bob holds a node %t (%v), and the ask states the authored_by %+v; want an unmarked row, bob's node and one authored_by to him",
			marked, bobHeld, err, authors)
	}
	report := requireTheCensusAgreesOver(t, database, "over an ask past the bound", graphstore.KindPerson)
	for _, kind := range report.Kinds {
		if kind.Kind == "ask_parent" && (kind.PastBound != 1 || kind.Unmarked != 0) {
			t.Errorf("ask_parent counts %d ask(s) past the bound and %d unmarked, want the one ask past the bound and none unmarked",
				kind.PastBound, kind.Unmarked)
		}
	}
}
