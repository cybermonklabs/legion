package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// newQueryMarkRig is a server whose settlements are held an hour away, so only a request's own
// mark and drain write a query version, and on it an issue of CORE born converted, whose spec holds
// one requirement r1 and no other block a query takes.
func newQueryMarkRig(t *testing.T) *digestRig {
	t.Helper()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	createReferenceAPIProject(t, handler, "CORE")
	rig := &digestRig{handler: handler, database: database, documents: deps.Docs.(*docs.Service)}
	rig.issue = createDigestIssue(t, handler, database, "CORE", "An issue whose queries a write moves",
		":::requirement{#r1}\nAn urgent ask comes first.\n:::\n", false)
	rig.spec = readDecisionDocument(t, database, rig.issue.Key, "spec")
	return rig
}

// decisionsQueryDocument is the id of the decisions query of the issue key, which an issue born
// converted holds.
func decisionsQueryDocument(t *testing.T, database *store.Store, key string) string {
	t.Helper()
	var id string
	if err := database.Pool.QueryRow(context.Background(),
		`select a.id::text from artifacts a where a.issue_key = $1 and `+refs.DecisionsQuerySQL("a"), key).Scan(&id); err != nil {
		t.Fatalf("read the decisions query of %s: %v", key, err)
	}
	return id
}

// requireQueryVersionFollows fails unless, once the write named what has answered, no mark of the
// issue key stands, the document artifactID of query holds version want as its newest, and that
// version's digest is the digest of the query's record at the head, which moved from was.
func requireQueryVersionFollows(t *testing.T, rig *digestRig, what, key, query, artifactID string, want int, was string) docs.QueryRecord {
	t.Helper()
	requireNoQueryMark(t, rig.database, "once "+what+" answered", key)
	record := rig.record(t, key, query)
	if record.Digest == was {
		t.Fatalf("%s left the %s record of %s at the digest %s, want it moved", what, query, key, was)
	}
	if newest := newestVersionOf(t, rig.database, artifactID); newest != want {
		t.Fatalf("%s left version %d of the %s query of %s newest, want %d: the write's mark owes the query a version", what, newest, query, key, want)
	}
	awaitDigestsFilled(t, rig.database, artifactID)
	if digest := versionDigestOf(t, rig.database, artifactID, want); digest != record.Digest {
		t.Errorf("after %s version %d of the %s query of %s answers the digest %s, want the record's %s", what, want, query, key, digest, record.Digest)
	}
	return record
}

// requireUnmarkedWriteWritesNoVersion is the control of a mark: tuple, stated straight into the
// store with no mark, moves the record of query of the issue key from was, and the document
// artifactID of that query stays at version standing, whose digest is no longer the record's.
func requireUnmarkedWriteWritesNoVersion(t *testing.T, rig *digestRig, key, query, artifactID string, standing int, was string, tuple graphstore.Tuple) {
	t.Helper()
	rig.state(t, tuple)
	moved := rig.record(t, key, query)
	if moved.Digest == was {
		t.Fatalf("the control's value %s on %s:%s leaves the %s record of %s at %s, want it moved", tuple.Kind, tuple.FromKind, tuple.FromID, query, key, was)
	}
	if newest := newestVersionOf(t, rig.database, artifactID); newest != standing {
		t.Errorf("the control's write, which queues no mark, left version %d of the %s query of %s newest, want %d", newest, query, key, standing)
	}
	if digest := versionDigestOf(t, rig.database, artifactID, standing); digest == moved.Digest {
		t.Errorf("version %d of the %s query of %s answers the moved record's digest %s with no version written", standing, query, key, digest)
	}
}

// askValue is a value tuple of attribute on the node of the ask id, as a write straight into the
// store states one.
func askValue(id, attribute, value string) graphstore.Tuple {
	return graphstore.Tuple{
		FromKind: graphstore.KindAsk, FromID: id, Kind: attribute, ToKind: graphstore.KindValue, ToID: value,
		At: time.Now().UTC(), Who: digestWriter, SourceEvent: graphstore.NoEvent, Asserted: true,
	}
}

// R14 for the asks of a converted issue: the decisions query takes each question by its node, and
// its record holds every value and relation out of that node, so each write of a question's node
// owes that query a version. An unanchored question opened on a clean spec, which writes no version
// of any document, is in the decisions record at once, and the create's own request marks the
// issue's queries owed and drains the mark before it answers (docs.Ledger.MarkAskQueries): the
// decisions query holds a new version whose digest is the record's, holding the ask, and no mark
// stands. Its control states a value on the ask's node with no mark: the record moves, and the
// decisions query stays at its version. A person's answer, a settle of the ask's answer state
// (docs.Ledger.PlanSettle), writes the next version the same way, the control's value with it, and
// its own control again moves the record and writes no version.
func TestAQuestionsCreateAndAnswerEachWriteADecisionsQueryVersion(t *testing.T) {
	rig := newQueryMarkRig(t)
	key := rig.issue.Key
	decisions := decisionsQueryDocument(t, rig.database, key)
	awaitDigestsFilled(t, rig.database, decisions)
	opened := newestVersionOf(t, rig.database, decisions)
	before := rig.record(t, key, graphstore.QueryDecisions)

	ask := createIssueAsk(t, rig.handler, key, "Does the list hold answered asks?")
	created := requireQueryVersionFollows(t, rig, "the ask's create", key, graphstore.QueryDecisions, decisions, opened+1, before.Digest)
	if !slices.Contains(walkAddresses(created.Nodes), graphstore.KindAsk+":"+ask) {
		t.Errorf("the decisions record of %s takes %v, want the ask %s", key, walkAddresses(created.Nodes), ask)
	}
	requireUnmarkedWriteWritesNoVersion(t, rig, key, graphstore.QueryDecisions, decisions, opened+1, created.Digest, askValue(ask, "probe", "unmarked"))
	controlled := rig.record(t, key, graphstore.QueryDecisions)

	answered := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/asks/"+ask+"/answer", map[string]any{
		"selected": []string{"Yes"}, "expected_edited_at": nil,
	}, "alice")
	if answered.Code != http.StatusOK {
		t.Fatalf("answer the ask %s as alice: status=%d body=%s", ask, answered.Code, answered.Body.String())
	}
	settled := requireQueryVersionFollows(t, rig, "the ask's answer", key, graphstore.QueryDecisions, decisions, opened+2, controlled.Digest)
	requireUnmarkedWriteWritesNoVersion(t, rig, key, graphstore.QueryDecisions, decisions, opened+2, settled.Digest, askValue(ask, "probe", "unmarked again"))
}

// R14 for the state route: a node's state is one of the values a query's record holds, so a person
// moving the spec's requirement to overtaken through POST /api/v1/issues/{key}/nodes/{id}/state
// moves the spec query's record, and the route marks the issue's queries owed in its write and
// drains them before it answers (writeNodeState): the spec holds a new version whose digest is the
// record's, and no mark stands. Its control states a value on the requirement's node with no mark:
// the record moves, and the spec stays at its version.
func TestARequirementMovedToOvertakenWritesASpecQueryVersion(t *testing.T) {
	rig := newQueryMarkRig(t)
	key := rig.issue.Key
	awaitDigestsFilled(t, rig.database, rig.spec.id)
	standing := newestVersionOf(t, rig.database, rig.spec.id)
	before := rig.record(t, key, graphstore.QuerySpec)
	requirement := rig.block("r1")

	if response := writeStateAs(t, rig.handler, key, requirement, graphstore.StateOvertaken, "alice"); response.Code != http.StatusOK {
		t.Fatalf("move %s to overtaken as alice: status=%d body=%s", requirement, response.Code, response.Body.String())
	}
	overtaken := requireQueryVersionFollows(t, rig, "the requirement's move to overtaken", key, graphstore.QuerySpec, rig.spec.id, standing+1, before.Digest)
	requireUnmarkedWriteWritesNoVersion(t, rig, key, graphstore.QuerySpec, rig.spec.id, standing+1, overtaken.Digest, blockValue(requirement, "probe", "unmarked"))
}

// R14 for the Keep: the decisions query takes an agent's decision, and its record holds the kept_by
// and the kept_text_sum a person's Keep states on the decision's node, so a Keep through
// POST /api/v1/artifacts/{id}/blocks/{block_id}/keep marks the issue's queries owed in its batch and
// drains them before it answers (keepDecisionNode): the decisions query holds a new version whose
// digest is the record's, and no mark stands. Its control states a value on the decision's node
// with no mark: the record moves, and the decisions query stays at its version.
func TestAKeepWritesADecisionsQueryVersion(t *testing.T) {
	rig := newQueryMarkRig(t)
	key := rig.issue.Key
	agentUploadAs(t, rig.handler, key, "choices.md", "::::decision{#d1}\nWe ship REST first.\n::::\n", nil)
	choices := readDecisionDocument(t, rig.database, key, "choices-md")
	decision := choices.address("d1")
	decisions := decisionsQueryDocument(t, rig.database, key)
	awaitDigestsFilled(t, rig.database, decisions)
	standing := newestVersionOf(t, rig.database, decisions)
	before := rig.record(t, key, graphstore.QueryDecisions)
	if !slices.Contains(walkAddresses(before.Nodes), blockAddress(decision)) {
		t.Fatalf("the decisions record of %s takes %v, want the decision %s", key, walkAddresses(before.Nodes), decision)
	}
	entry := requireEntry(t, readEntries(t, rig.handler, key), "the agent's decision", blockAddress(decision))

	kept := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/artifacts/"+choices.id+"/blocks/d1/keep",
		map[string]any{"text_sha256": entry.TextSHA256}, "alice")
	if kept.Code != http.StatusOK {
		t.Fatalf("keep the decision %s as alice: status=%d body=%s", decision, kept.Code, kept.Body.String())
	}
	record := requireQueryVersionFollows(t, rig, "the Keep", key, graphstore.QueryDecisions, decisions, standing+1, before.Digest)
	requireUnmarkedWriteWritesNoVersion(t, rig, key, graphstore.QueryDecisions, decisions, standing+1, record.Digest, blockValue(decision, "probe", "unmarked"))
}

// The fence of expected_sequence (PATCH /api/v1/issues/{key}, and each step of a status change)
// passes over a version of the issue's decisions query, which only a query drain writes, after the
// write that moved the query's record committed and appended its own event, and it still reads a
// document's version. A version the drain writes of the decisions query between the read and the
// PATCH - a value of the ask's node stated with its mark, and the drain run - leaves the PATCH
// taken at the sequence read before it; a version of the spec written between a second read and
// a second PATCH refuses that PATCH 409 ISSUE_CHANGED.
func TestAFencePassesOverADrainsDecisionsQueryVersionAndReadsADocumentVersion(t *testing.T) {
	rig := newQueryMarkRig(t)
	key := rig.issue.Key
	ask := createIssueAsk(t, rig.handler, key, "Does the fence read the drain's version?")
	requireNoQueryMark(t, rig.database, "once the ask's create answered", key)
	decisions := decisionsQueryDocument(t, rig.database, key)
	read := readIssueDetail(t, rig.handler, key).LastSeq
	standing := newestVersionOf(t, rig.database, decisions)
	rig.stateDrained(t, key, askValue(ask, "probe", "drained"))
	if newest := newestVersionOf(t, rig.database, decisions); newest != standing+1 {
		t.Fatalf("the drain left version %d of the decisions query of %s newest, want %d", newest, key, standing+1)
	}
	if after := readIssueDetail(t, rig.handler, key).LastSeq; after != read+1 {
		t.Fatalf("the issue %s answers last_seq %d after the drain, want %d: the decisions query's version past the read", key, after, read+1)
	}
	requireDecisionsVersionEventAt(t, rig.database, key, read+1, decisions)
	if patched := dispatchRequest(t, rig.handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{
		"title": "A title the fenced write takes", "expected_sequence": read,
	}, "alice"); patched.Code != http.StatusOK {
		t.Fatalf("the PATCH fenced at %d past which a drain wrote the decisions query's version: status=%d body=%s, want 200", read, patched.Code, patched.Body.String())
	}

	read = readIssueDetail(t, rig.handler, key).LastSeq
	editSpec(t, rig.handler, key, []map[string]any{{"op": "insert", "after": "end", "markdown": "A paragraph written past the read.\n"}})
	refused := dispatchRequest(t, rig.handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{
		"title": "A title the fence refuses", "expected_sequence": read,
	}, "alice")
	if refused.Code != http.StatusConflict || !strings.Contains(refused.Body.String(), `"code":"ISSUE_CHANGED"`) {
		t.Fatalf("the PATCH fenced at %d past which the spec took a version: status=%d body=%s, want 409 ISSUE_CHANGED", read, refused.Code, refused.Body.String())
	}
}

// requireDrainedBy fails unless, once the write named what has answered, the query document
// artifactID of the issue key holds version standing+1 as its newest, written by want alone, and
// its artifact.version names want as its actor: the drain names the version after the writer of
// the write that marked the issue (R7).
func requireDrainedBy(t *testing.T, rig *digestRig, what, key, artifactID string, standing int, want model.Actor) {
	t.Helper()
	requireNoQueryMark(t, rig.database, "once "+what+" answered", key)
	number := newestVersionOf(t, rig.database, artifactID)
	if number != standing+1 {
		t.Fatalf("%s left version %d of the query document %s of %s newest, want %d", what, number, artifactID, key, standing+1)
	}
	var encoded []byte
	if err := rig.database.Pool.QueryRow(context.Background(),
		`select authors from artifact_versions where artifact_id = $1::uuid and number = $2`, artifactID, number).Scan(&encoded); err != nil {
		t.Fatalf("read the authors of version %d of %s: %v", number, artifactID, err)
	}
	var authors []model.Actor
	if err := json.Unmarshal(encoded, &authors); err != nil {
		t.Fatalf("decode the authors %s of version %d of %s: %v", encoded, number, artifactID, err)
	}
	if len(authors) != 1 || authors[0].Kind != want.Kind || authors[0].ID != want.ID {
		t.Errorf("after %s the drained version %d of the query document %s of %s is written by %+v, want %s %s alone", what, number, artifactID, key, authors, want.Kind, want.ID)
	}
	if _, actor := requireVersionEvent(t, rig.database, artifactID, number); actor.Kind != want.Kind || actor.ID != want.ID {
		t.Errorf("after %s the artifact.version of version %d of the query document %s of %s names %+v, want %s %s", what, number, artifactID, key, actor, want.Kind, want.ID)
	}
}

// R7 for the marks of the decisions query: a drain names each version it writes after the writer
// of the write that marked the issue, never after whoever wrote the newest tuple of the store
// before that write. Before each write here alice states a value straight into the store, on the
// issue's own node, which no query takes, so hers is the newest tuple there is. Then the asking
// session rewords its question, a session follows the question, and carol keeps an agent's
// decision: each drained version of the decisions query is written by that write's own writer,
// and its artifact.version names it.
func TestADrainedVersionNamesTheWriterOfTheWriteThatMarkedIt(t *testing.T) {
	rig := newQueryMarkRig(t)
	key := rig.issue.Key
	ask := createIssueAsk(t, rig.handler, key, "Which writer does the drain name?")
	agentUploadAs(t, rig.handler, key, "choices.md", "::::decision{#d1}\nWe name the writer.\n::::\n", nil)
	choices := readDecisionDocument(t, rig.database, key, "choices-md")
	decisions := decisionsQueryDocument(t, rig.database, key)
	aliceLast := func(value string) {
		t.Helper()
		rig.state(t, graphstore.Tuple{
			FromKind: graphstore.KindIssue, FromID: key, Kind: "probe", ToKind: graphstore.KindValue, ToID: value,
			At: time.Now().UTC(), Who: digestWriter, SourceEvent: graphstore.NoEvent, Asserted: true,
		})
	}

	asker := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	aliceLast("before the reword")
	standing := newestVersionOf(t, rig.database, decisions)
	if reworded := sessionRequest(t, rig.handler, http.MethodPatch, "/api/v1/asks/"+ask, map[string]any{
		"question": "Which writer does the drain name, now?", "actor": sessionActor(),
	}); reworded.Code != http.StatusOK {
		t.Fatalf("reword the ask %s as its asker: status=%d body=%s", ask, reworded.Code, reworded.Body.String())
	}
	requireDrainedBy(t, rig, "the asker's reword", key, decisions, standing, asker)

	follower := model.Actor{Kind: "session", ID: "session-follower-of-the-drain"}
	aliceLast("before the follow")
	standing = newestVersionOf(t, rig.database, decisions)
	if followed := sessionRequest(t, rig.handler, http.MethodPut, "/api/v1/asks/"+ask+"/followers/"+follower.ID, map[string]any{
		"actor": map[string]any{"kind": follower.Kind, "id": follower.ID},
	}); followed.Code != http.StatusNoContent {
		t.Fatalf("follow the ask %s as %s: status=%d body=%s", ask, follower.ID, followed.Code, followed.Body.String())
	}
	requireDrainedBy(t, rig, "the session's follow", key, decisions, standing, follower)

	keeper := model.Actor{Kind: "user", ID: "carol"}
	entry := requireEntry(t, readEntries(t, rig.handler, key), "the agent's decision", blockAddress(choices.address("d1")))
	aliceLast("before the Keep")
	standing = newestVersionOf(t, rig.database, decisions)
	if kept := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/artifacts/"+choices.id+"/blocks/d1/keep",
		map[string]any{"text_sha256": entry.TextSHA256}, keeper.ID); kept.Code != http.StatusOK {
		t.Fatalf("keep the decision d1 as carol: status=%d body=%s", kept.Code, kept.Body.String())
	}
	requireDrainedBy(t, rig, "carol's Keep", key, decisions, standing, keeper)
}

// R7 for the mark of a room's pass under a race: a write of a document of the issue's room whose
// pass states tuples marks the issue's queries owed last in its batch, naming its own last tuple,
// so a tuple another writer commits between that write's append and its mark is never the one the
// drain names its version after. bob rewords a requirement the plan holds, which moves the spec's
// record; his write is held at the requirement's head row, after its tuples and before its mark,
// while alice commits a tuple of her own; once he goes on, the drained version of the spec is
// written by bob, and its artifact.version names him.
func TestARoomMarkNamesItsOwnWriterPastATupleCommittedWhileItWaits(t *testing.T) {
	rig := newQueryMarkRig(t)
	key := rig.issue.Key
	ctx := context.Background()
	uploadDocument(t, rig.handler, "/api/v1/issues/"+key, "plan.md", ":::requirement{#r9}\nThe plan's requirement.\n:::\n")
	requirement := readDecisionDocument(t, rig.database, key, "plan-md").address("r9")
	awaitDigestsFilled(t, rig.database, rig.spec.id)
	requireNoQueryMark(t, rig.database, "once the plan's upload answered", key)
	standing := newestVersionOf(t, rig.database, rig.spec.id)

	holder, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the hold of the requirement's head row: %v", err)
	}
	defer holder.Rollback(ctx)
	var held, holderPID int
	if err := holder.QueryRow(ctx, `
		select count(*), pg_backend_pid() from (
			select 1 from issue_head_nodes where issue_key = $1 and node_kind = $2 and node_address = $3 for update
		) row`, key, refs.BlockNodeKind, requirement).Scan(&held, &holderPID); err != nil || held != 1 {
		t.Fatalf("hold the head row of %s: %d rows (%v), want its one", requirement, held, err)
	}
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/issues/"+key+"/artifacts/plan-md/edits", map[string]any{
			"ops":     []map[string]any{{"op": "replace", "find": "The plan's requirement.", "with": "The plan's requirement, reworded."}},
			"summary": "edit",
		}, "bob")
	}()
	waiting := testwait.UntilPolling(t, 20*time.Millisecond, "bob's edit to wait at the head row of "+requirement, func() (string, bool) {
		var statement string
		err := rig.database.Pool.QueryRow(ctx, `
			select query from pg_stat_activity where $1::int = any(pg_blocking_pids(pid))`, holderPID).Scan(&statement)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("read the backend the hold blocks: %v", err)
		}
		return statement, err == nil
	})
	if !strings.Contains(waiting, "with head_nodes as materialized (") {
		t.Fatalf("bob's edit waits on the head row at %q, want the head write", waiting)
	}
	// alice's tuple takes a sequence past every one bob's tuples drew, and commits while his write
	// stands between its append and its mark. A lock of his it waited for would end the test here,
	// at the lock timeout, rather than hang it.
	alice, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin alice's write: %v", err)
	}
	defer alice.Rollback(ctx)
	if _, err := alice.Exec(ctx, `set local lock_timeout = '20s'`); err != nil {
		t.Fatalf("bound alice's write: %v", err)
	}
	if err := graphstore.Write(ctx, alice, nil, []graphstore.Tuple{{
		FromKind: graphstore.KindIssue, FromID: key, Kind: "probe", ToKind: graphstore.KindValue, ToID: "committed while bob waits",
		At: time.Now().UTC(), Who: digestWriter, SourceEvent: graphstore.NoEvent, Asserted: true,
	}}); err != nil {
		t.Fatalf("write alice's tuple while bob's edit waits: %v", err)
	}
	if err := alice.Commit(ctx); err != nil {
		t.Fatalf("commit alice's tuple: %v", err)
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("let bob's edit go on: %v", err)
	}
	if response := <-answered; response.Code != http.StatusOK {
		t.Fatalf("bob's edit of the plan's requirement: status=%d body=%s", response.Code, response.Body.String())
	}
	requireDrainedBy(t, rig, "bob's reword of the plan's requirement", key, rig.spec.id, standing, model.Actor{Kind: "user", ID: "bob"})
}
