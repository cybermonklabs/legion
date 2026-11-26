package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// digestSpec is the spec of the rig's issue, CORE-1: the memo callout, a criterion, two
// requirements under it, and a paragraph no query takes.
const digestSpec = ":::callout{#memo}\nWe build the urgent list, because asks wait, and the list proves it.\n:::\n\n" +
	":::criterion{#k1}\nI want the urgent asks first.\n:::\n\n" +
	":::requirement{#r1 serves=\"dispatch://CORE-1/spec#b-k1\"}\nAn urgent ask comes first.\n:::\n\n" +
	":::requirement{#r2 serves=\"dispatch://CORE-1/spec#b-k1\"}\nAn ask keeps its urgency.\n:::\n\n" +
	"A paragraph no query takes.\n"

// digestChecks is the rig's Verification document: two checks of r1, words as given.
func digestChecks(first, second string) string {
	var checks strings.Builder
	checks.WriteString(":::check{#c1 serves=\"dispatch://CORE-1/spec#b-r1\" surface=\"cli\" run=\"go test ./...\"}\n" + first + "\n:::\n")
	if second != "" {
		checks.WriteString("\n:::check{#c2 serves=\"dispatch://CORE-1/spec#b-r1\" surface=\"cli\" run=\"go test ./...\"}\n" + second + "\n:::\n")
	}
	return checks.String()
}

// digestRig is the server and the issue the steps of TestApprovalCoversTheDigestOfOneQuerysRecord
// share: CORE-1, born converted, with digestSpec, digestChecks and a plan of one plan node.
type digestRig struct {
	handler   http.Handler
	database  *store.Store
	documents *docs.Service
	issue     model.Issue
	spec      decisionDocument
}

// digestWriter is who a step's own tuples are written by.
var digestWriter = model.Actor{Kind: "user", ID: "alice"}

// walk is the walk of the issue key's tree at the head.
func (r *digestRig) walk(t *testing.T, key string) docs.IssueTree {
	t.Helper()
	tree, err := docs.WalkIssueTree(context.Background(), r.database.Pool, key, refs.TreeCut{})
	if err != nil {
		t.Fatalf("walk the tree of %s: %v", key, err)
	}
	return tree
}

// record is the record of the query of the issue key at the head.
func (r *digestRig) record(t *testing.T, key, query string) docs.QueryRecord {
	t.Helper()
	return r.recordAt(t, key, query, refs.TreeCut{})
}

// recordAt is the record of the query of the issue key at cut.
func (r *digestRig) recordAt(t *testing.T, key, query string, cut refs.TreeCut) docs.QueryRecord {
	t.Helper()
	record, err := docs.ReadQueryRecord(context.Background(), r.database.Pool, key, query, cut)
	if err != nil {
		t.Fatalf("read the %s record of %s at %+v: %v", query, key, cut, err)
	}
	if want := contentHash(string(record.Record)); record.Digest != want {
		t.Fatalf("the %s record of %s answers the digest %s, want the sha256 of its bytes, %s", query, key, record.Digest, want)
	}
	return record
}

// block is the store address of block id of the rig's spec, which the issue's room keys.
func (r *digestRig) block(id string) string { return r.spec.address(id) }

// state appends tuples straight into the store, as a write that states a relation or a value and
// no word of any node would.
func (r *digestRig) state(t *testing.T, tuples ...graphstore.Tuple) {
	t.Helper()
	r.write(t, nil, tuples)
}

// write writes nodes and tuples straight into the store through its writer, in one transaction of
// their own.
func (r *digestRig) write(t *testing.T, nodes []graphstore.Node, tuples []graphstore.Tuple) {
	t.Helper()
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, r.database.Pool, func(tx pgx.Tx) error { return graphstore.Write(ctx, tx, nodes, tuples) }); err != nil {
		t.Fatalf("write the nodes %+v and the tuples %+v: %v", nodes, tuples, err)
	}
}

// stateDrained appends tuples straight into the store as state does, marks the queries of the
// issue key owed in the same batch (docs.QueueQueryMark), as every writer that moves a record does,
// and runs the drain once the write has committed (docs.Service.DrainOwedQueries), which writes a
// version of each query whose record moved: the version whose digest every gate reads.
func (r *digestRig) stateDrained(t *testing.T, key string, tuples ...graphstore.Tuple) {
	t.Helper()
	ctx := context.Background()
	tx, err := r.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	var marked bool
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		graphstore.QueueWrite(b, nil, tuples)
		graphstore.QueueHeads(b, graphstore.HeadsOf(nil, tuples))
		docs.QueueQueryMark(b, key, graphstore.NoEvent, &marked)
		return nil
	}); err != nil {
		t.Fatalf("write %+v: %v", tuples, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit %+v: %v", tuples, err)
	}
	if !marked {
		t.Fatalf("the write of %+v marked no query of %s owed: %s is no converted issue", tuples, key, key)
	}
	r.documents.DrainOwedQueries(ctx, key)
	var owed bool
	if err := r.database.Pool.QueryRow(ctx, `select exists (select 1 from issue_query_marks where issue_key = $1)`, key).Scan(&owed); err != nil || owed {
		t.Fatalf("the drain of %s left its mark standing (%t, %v)", key, owed, err)
	}
}

// blockTuple is a tuple out of the block node at address: a value where toKind is the value kind.
func blockTuple(address, kind, toKind, toID string, asserted bool) graphstore.Tuple {
	return graphstore.Tuple{
		FromKind: refs.BlockNodeKind, FromID: address, Kind: kind, ToKind: toKind, ToID: toID,
		At: time.Now().UTC(), Who: digestWriter, SourceEvent: graphstore.NoEvent, Asserted: asserted,
	}
}

// blockValue is the value tuple of attribute on the block node at address.
func blockValue(address, attribute, value string) graphstore.Tuple {
	return blockTuple(address, attribute, graphstore.KindValue, value, true)
}

// walkAddresses is each node's store address, `<kind>:<id>`, in order.
func walkAddresses(nodes []docs.TreeNode) []string {
	named := make([]string, len(nodes))
	for index, node := range nodes {
		named[index] = node.Address()
	}
	return named
}

// blockAddress is the store address of the block node id.
func blockAddress(id string) string { return refs.BlockNodeKind + ":" + id }

// walkIndex is where the node kind:id stands in tree's walk, -1 where it is absent.
func walkIndex(tree docs.IssueTree, address string) int {
	return slices.IndexFunc(tree.Nodes, func(node docs.TreeNode) bool { return node.Address() == address })
}

// requireWalkOrder fails unless tree's walk takes each node after its parent, and the ranked
// children of each parent in increasing rank, byte for byte, a tie by the lower address. A node of
// the walk's second part whose parent is a node of an issue above has no parent in the walk.
func requireWalkOrder(t *testing.T, tree docs.IssueTree) {
	t.Helper()
	ranked := map[string][]docs.TreeNode{}
	for index, node := range tree.Nodes {
		if index == 0 {
			continue
		}
		parent := node.Parent.Kind + ":" + node.Parent.ID
		if at := walkIndex(tree, parent); (at < 0 && !node.Above) || at >= index {
			t.Errorf("the walk of %s takes %s at %d, and its parent %s at %d, want the parent first", tree.Issue, node.Address(), index, parent, at)
		}
		if node.Ranked {
			ranked[parent] = append(ranked[parent], node)
		}
	}
	for parent, children := range ranked {
		for index := 1; index < len(children); index++ {
			before, after := children[index-1], children[index]
			if before.Rank > after.Rank || before.Rank == after.Rank && before.Address() > after.Address() {
				t.Errorf("under %s the walk takes %s (rank %q) before %s (rank %q), want increasing rank, byte for byte",
					parent, before.Address(), before.Rank, after.Address(), after.Rank)
			}
		}
	}
}

// createDigestIssue creates, as alice, a root issue of project with spec, born converted unless
// unconverted (createDigestChild).
func createDigestIssue(t *testing.T, handler http.Handler, database *store.Store, project, title, spec string, unconverted bool) model.Issue {
	t.Helper()
	return createDigestChild(t, handler, database, project, title, spec, "", unconverted)
}

// createDigestChild creates, as alice, an issue of project with spec under the issue parent, a
// root issue where parent is "", born converted unless unconverted, in which case a live process
// of the release before is beside the server for the create alone (holdIssuesUnconverted, taken
// back once the issue is written).
func createDigestChild(t *testing.T, handler http.Handler, database *store.Store, project, title, spec, parent string, unconverted bool) model.Issue {
	t.Helper()
	var held string
	if unconverted {
		held = holdIssuesUnconverted(t, database)
	}
	issue, tree := createRoomIssue(t, handler, database, project, title, spec, parent)
	if unconverted {
		if _, err := database.Pool.Exec(context.Background(), `delete from server_processes where instance = $1::uuid`, held); err != nil {
			t.Fatalf("take back the process of the release before: %v", err)
		}
	}
	if converted := tree != ""; converted == unconverted {
		t.Fatalf("issue %s holds the tree %q, want it converted %t", issue.Key, tree, !unconverted)
	}
	return issue
}

// insertPersonComment writes, as alice, a comment of issue with the id and the time given and,
// where replyTo is not empty, as a reply to that comment, and indexes its blocks as a route does.
// It writes no node for it, as a process of the release before writes none.
func insertPersonComment(t *testing.T, database *store.Store, issue, id, replyTo, body string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	var reply *string
	if replyTo != "" {
		reply = &replyTo
	}
	if _, err := tx.Exec(ctx, `insert into comments (id, issue_key, author, body, reply_to, created_at) values ($1::uuid, $2, '{"kind":"user","id":"alice"}', $3, $4::uuid, $5)`,
		id, issue, body, reply, at); err != nil {
		t.Fatalf("insert comment %s: %v", id, err)
	}
	indexItemAsARouteDoes(t, tx, refs.ItemComment, id, body, at)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit comment %s: %v", id, err)
	}
}

// requireUnder fails unless tree takes the node at address under parent, after it.
func requireUnder(t *testing.T, what string, tree docs.IssueTree, address, parent string) {
	t.Helper()
	at := walkIndex(tree, address)
	if at < 0 {
		t.Fatalf("%s: the walk of %s holds no %s: %v", what, tree.Issue, address, walkAddresses(tree.Nodes))
	}
	held := tree.Nodes[at].Parent.Kind + ":" + tree.Nodes[at].Parent.ID
	if held != parent || walkIndex(tree, parent) >= at {
		t.Errorf("%s: the walk of %s takes %s at %d under %s, want it under %s (at %d), after it", what, tree.Issue, address, at, held, parent, walkIndex(tree, parent))
	}
}

// The record and the order an approval covers (the approval's own steps join the list below): an
// approval covers the digest of one query's record, in the tree's own order. The record holds each
// node before its children and siblings in increasing rank, byte for byte, a tie by the lower node
// id; the spec query takes the memo callout and the criteria and requirements, the verification
// query the checks and the results under them, the plan query the plan nodes. A relation alone - a
// retargeted serves, a moved check, a recorded result, a removed check - moves the verification
// digest, and so does a value alone: a check's visibility, another's state, a third's placement
// under the same parent; a word of a check moves it and leaves the spec's; a word of a requirement
// moves the spec's. The first version of a spec created converted, which the create writes, is a
// query version holding its query's definition_version, and the review approving it names it.
// Every field carries its length, so a value that holds the record's own separators and a length
// prefix of its own reads apart from the store that splits those bytes another way. The head and a
// version are read through one builder, and a version's record read after a later comment leaves
// that comment out. Sibling comments with no rank, and a reply after its head, stand by made_at
// whatever their ids, before the issue's ranked children but its memo, which stands first (R8 at
// spec v246); a requirement under a comment's block of its own issue stands in the walk's
// first part after its comment; a criterion of a child under its parent's comment stands in the
// child's spec record after the walk from the child's tree node, and a write of it moves the
// child's digest and leaves the parent's; and a document comment anchored inside a decision's
// paragraph stands under that paragraph, the node whose text it quotes (R31), after it, on a
// converted issue, through a moved address, and on one that has not converted. The test refuses to
// run where select 'a' < 'B' is false.
func TestApprovalCoversTheDigestOfOneQuerysRecord(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	ctx := context.Background()
	var collated bool
	if err := database.Pool.QueryRow(ctx, `select 'a' < 'B'`).Scan(&collated); err != nil {
		t.Fatalf("read the database's collation of a and B: %v", err)
	}
	if !collated {
		t.Fatal("this database orders 'B' before 'a' by its own collation, so it cannot tell a record that orders ranks byte for byte from one that forgot to: run this test where select 'a' < 'B' is true")
	}
	createReferenceAPIProject(t, handler, "CORE")
	rig := &digestRig{handler: handler, database: database, documents: deps.Docs.(*docs.Service)}
	rig.issue = createDigestIssue(t, handler, database, "CORE", "The digest's issue", digestSpec, false)
	if rig.issue.Key != "CORE-1" {
		t.Fatalf("the rig's issue is %s, want CORE-1, which its spec's references name", rig.issue.Key)
	}
	rig.spec = readDecisionDocument(t, database, rig.issue.Key, "spec")
	owner := "/api/v1/issues/" + rig.issue.Key
	uploadDocument(t, handler, owner, "verification.md", digestChecks("The list reads urgent asks first.", "The list reads an ask's urgency back."))
	uploadDocument(t, handler, owner, "plan.md", ":::plan_task{#t1 serves=\"dispatch://CORE-1/spec#b-r2\"}\nBuild the order.\n:::\n")

	for _, step := range []struct {
		name string
		run  func(t *testing.T, rig *digestRig)
	}{
		{"the walk takes each node before its children, siblings by rank, and each query takes its own nodes", digestStepOrderAndTakes},
		{"a word of a check moves the verification digest alone, and a word of a requirement the spec's", digestStepWords},
		{"the first version of a spec created converted is a query version holding its definition, and its review names it", digestStepBornConvertedSpec},
		{"a relation alone moves the verification digest", digestStepRelations},
		{"a value of a check alone - its visibility, its state, its placement - moves the verification digest", digestStepValues},
		{"a move in the text alone - a text_rank, a text_parent - moves the spec digest and stales its approval", digestStepTextOrder},
		{"ranks compare byte for byte, and one rank takes the lower node id first", digestStepRanks},
		{"every field carries its length", digestStepLengths},
		{"the head and a version are read through one builder, and a later comment stands in no earlier version", digestStepVersions},
		{"comments with no rank, and a reply, stand by made_at whatever their ids", digestStepUnranked},
		{"a requirement under a comment's block stands in the walk's first part after its comment", digestStepRequirementUnderComment},
		{"a criterion of a child under its parent's comment stands in the child's record alone", digestStepChildCriterion},
		{"a comment anchored inside a decision's paragraph stands under that paragraph", digestStepAnchoredComment},
		{"an approval stands only while its review stands on the digest its version answers", digestStepGate},
		{"a version whose record did not move leaves the approval standing, and one whose record moved stales it", digestStepRecordUnmoved},
		{"a result carries the approval to the version it writes, in the person's name", digestStepResultCarry},
		{"a carry refuses two versions whose definition_version differ", digestStepDefinitionControl},
		{"a request states one approves tuple to each query version node, which its row projects", digestStepApproves},
		{"the row the release before writes is admitted, and the review's column and triggers stand", digestStepRowAndColumns},
		{"the record of the approved version, rebuilt at its cut, digests to the review's content_sha256", digestStepApprovedRecord},
		{"two results of one check stand by made_at whatever their addresses", digestStepResultOrder},
		{"a block retyped later is typed at an earlier cut as its text then opens", digestStepRetype},
		{"the migration set runs no statement against artifact_reviews", digestStepMigrations},
		{"an approval given before the move reads approved after it by the carry, and only a review after the cutover compares a digest", digestStepCarriedAcrossTheMove},
		{"a person's answer to an ask block carries the approval before the move and after it, with the same verdict", digestStepAnswerCarryAcrossTheMove},
		// Last: it runs graphcensus.Backfill over the whole database.
		{"the request's query version holds its definition, and the backfill's version of a later query holds none", digestStepDefinitionNodes},
	} {
		t.Run(step.name, func(t *testing.T) { step.run(t, rig) })
	}
}

func digestStepOrderAndTakes(t *testing.T, rig *digestRig) {
	tree := rig.walk(t, rig.issue.Key)
	if len(tree.Nodes) == 0 || tree.Nodes[0].Address() != graphstore.KindIssue+":"+rig.issue.Key {
		t.Fatalf("the walk of %s begins %v, want the issue's node", rig.issue.Key, walkAddresses(tree.Nodes[:min(len(tree.Nodes), 1)]))
	}
	requireWalkOrder(t, tree)
	if tree.Memo < 0 || tree.Nodes[tree.Memo].Address() != blockAddress(rig.block("memo")) {
		t.Errorf("the walk's memo is %d, want the callout %s, the issue's first child", tree.Memo, rig.block("memo"))
	}
	for _, want := range []struct {
		query string
		nodes []string
	}{
		{graphstore.QuerySpec, []string{rig.block("memo"), rig.block("k1"), rig.block("r1"), rig.block("r2")}},
		{graphstore.QueryVerification, []string{rig.block("c1"), rig.block("c2")}},
		{graphstore.QueryPlan, []string{rig.block("t1")}},
	} {
		record := rig.record(t, rig.issue.Key, want.query)
		wanted := make([]string, len(want.nodes))
		for index, id := range want.nodes {
			wanted[index] = blockAddress(id)
		}
		if got := walkAddresses(record.Nodes); !slices.Equal(got, wanted) {
			t.Errorf("the %s query takes %v, want %v", want.query, got, wanted)
		}
		if len(record.Record) == 0 || !bytes.HasPrefix(record.Record, []byte(strconv.Itoa(len(wanted[0]))+" "+wanted[0])) {
			t.Errorf("the %s record begins %q, want the first node's address, its length before it", want.query, record.Record[:min(len(record.Record), 64)])
		}
	}
	if _, err := docs.ReadQueryRecord(context.Background(), rig.database.Pool, rig.issue.Key, "unknown", refs.TreeCut{}); err == nil {
		t.Error("a record of a query no definition names is read, want it refused")
	}
}

func digestStepWords(t *testing.T, rig *digestRig) {
	spec, verification := rig.record(t, rig.issue.Key, graphstore.QuerySpec), rig.record(t, rig.issue.Key, graphstore.QueryVerification)
	uploadDocument(t, rig.handler, "/api/v1/issues/"+rig.issue.Key, "verification.md",
		digestChecks("The list reads the urgent asks first.", "The list reads an ask's urgency back."))
	worded := rig.record(t, rig.issue.Key, graphstore.QueryVerification)
	if worded.Digest == verification.Digest {
		t.Errorf("a word of c1 leaves the verification digest at %s", worded.Digest)
	}
	if after := rig.record(t, rig.issue.Key, graphstore.QuerySpec); after.Digest != spec.Digest {
		t.Errorf("a word of c1 moves the spec digest from %s to %s, want it standing", spec.Digest, after.Digest)
	}
	editSpec(t, rig.handler, rig.issue.Key, []map[string]any{{"op": "replace", "find": "An ask keeps its urgency.", "with": "An ask keeps the urgency it was asked with."}})
	if after := rig.record(t, rig.issue.Key, graphstore.QuerySpec); after.Digest == spec.Digest {
		t.Errorf("a word of r2 leaves the spec digest at %s", after.Digest)
	}
	if after := rig.record(t, rig.issue.Key, graphstore.QueryVerification); after.Digest != worded.Digest {
		t.Errorf("a word of r2 moves the verification digest from %s to %s, want it standing", worded.Digest, after.Digest)
	}
}

// digestStepBornConvertedSpec reads the definition_version of each of the rig issue's four query
// nodes, the spec, the verification, the plan and the decisions, and finds the one this build's
// record layout holds. It then creates a second issue, born converted, so its spec's node is a
// query node from the create on: the version 1 of that spec, which the create itself writes, is a
// query version holding the query's definition_version, and the review approving it names that
// node by its artifact_id and its version.
func digestStepBornConvertedSpec(t *testing.T, rig *digestRig) {
	ctx := context.Background()
	want := strconv.Itoa(graphstore.QueryDefinitionVersion)
	rows, err := rig.database.Pool.Query(ctx, `
		select attributes->>$3, id from graph_nodes where kind = $1 and supertag = $2 and id like $4 order by id
	`, graphstore.KindArtifact, graphstore.SupertagQuery, graphstore.AttributeQuery, rig.issue.Key+"/%")
	if err != nil {
		t.Fatalf("read the query nodes of %s: %v", rig.issue.Key, err)
	}
	queries, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ Query, RefKey string }])
	if err != nil {
		t.Fatalf("read the query nodes of %s: %v", rig.issue.Key, err)
	}
	nodes := map[string]string{}
	for _, query := range queries {
		nodes[query.Query] = query.RefKey
	}
	for _, query := range []string{graphstore.QuerySpec, graphstore.QueryVerification, graphstore.QueryPlan, graphstore.QueryDecisions} {
		refKey, found := nodes[query]
		if !found {
			t.Errorf("%s holds no %s query node: %+v", rig.issue.Key, query, queries)
			continue
		}
		value, stated, err := graphstore.CurrentValue(ctx, rig.database.Pool, graphstore.KindArtifact, refKey, graphstore.AttributeDefinitionVersion)
		if err != nil {
			t.Fatalf("read the definition_version of %s: %v", refKey, err)
		}
		if !stated {
			value = "none"
		}
		if value != want {
			t.Errorf("query node %s holds definition_version %s, want %s", refKey, value, want)
		}
	}

	issue := createDigestIssue(t, rig.handler, rig.database, "CORE", "A spec created converted",
		":::requirement{#r1}\nThe second issue's list comes first.\n:::\n", false)
	spec := readDecisionDocument(t, rig.database, issue.Key, "spec")
	if supertag, held := versionDefinition(t, rig.database, spec.refKey, 1); supertag != graphstore.SupertagQueryVersion || valueOrNone(held) != want {
		t.Errorf("the version 1 of %s, a spec created converted, is a %s holding definition_version %s, want a query version holding %s",
			spec.refKey, supertag, valueOrNone(held), want)
	}
	review := reviewDocument(t, rig.handler, spec.id, "alice", "approved", "")
	if review.ArtifactID != spec.id || review.Version != 1 {
		t.Errorf("the review %s names %s at version %d, want version 1 of %s (%s)", review.ID, review.ArtifactID, review.Version, spec.refKey, spec.id)
	}
}

// digestGates records the rig issue's design gates - the spec step on its spec, the criteria step
// on its Verification document - as the daemon publishes them, and answers the Verification
// document.
func digestGates(t *testing.T, rig *digestRig) decisionDocument {
	t.Helper()
	verification := readDecisionDocument(t, rig.database, rig.issue.Key, "verification-md")
	publishDesignGates(t, rig.handler, rig.issue.Key, []issueDesignGate{
		{Step: model.GatedStepCriteria, ArtifactID: verification.id},
		{Step: model.GatedStepSpec, ArtifactID: rig.spec.id},
	})
	return verification
}

// approveStanding approves the newest version of the document gated on step as alice and reads it
// approved on every gate (requireGate).
func approveStanding(t *testing.T, rig *digestRig, key, step, artifactID, before string) {
	t.Helper()
	reviewDocument(t, rig.handler, artifactID, "alice", "approved", "")
	requireGate(t, rig, key, step, model.AskApprovalDocument{ArtifactID: artifactID}, true, "approved before "+before)
}

func digestStepRelations(t *testing.T, rig *digestRig) {
	c1, c2, r1, r2 := rig.block("c1"), rig.block("c2"), rig.block("r1"), rig.block("r2")
	verification := digestGates(t, rig)
	before := rig.record(t, rig.issue.Key, graphstore.QueryVerification)
	spec := rig.record(t, rig.issue.Key, graphstore.QuerySpec)
	approveStanding(t, rig, rig.issue.Key, model.GatedStepSpec, rig.spec.id, "the retarget")

	// The retarget: c2's serves leaves r1 for r2, and no word, value, node or place moves.
	approveStanding(t, rig, rig.issue.Key, model.GatedStepCriteria, verification.id, "the retarget")
	rig.stateDrained(t, rig.issue.Key, blockTuple(c2, "serves", refs.BlockNodeKind, r2, true), blockTuple(c2, "serves", refs.BlockNodeKind, r1, false))
	retargeted := rig.record(t, rig.issue.Key, graphstore.QueryVerification)
	if retargeted.Digest == before.Digest {
		t.Errorf("the retarget of c2's serves from %s to %s leaves the verification digest at %s", r1, r2, before.Digest)
	}
	if !slices.Equal(walkAddresses(retargeted.Nodes), walkAddresses(before.Nodes)) {
		t.Errorf("the retarget moves the verification query's nodes from %v to %v, want them where they stood", walkAddresses(before.Nodes), walkAddresses(retargeted.Nodes))
	}
	if after := rig.record(t, rig.issue.Key, graphstore.QuerySpec); after.Digest != spec.Digest {
		t.Errorf("the retarget of c2 moves the spec digest from %s to %s", spec.Digest, after.Digest)
	}
	requireGate(t, rig, rig.issue.Key, model.GatedStepCriteria, model.AskApprovalDocument{ArtifactID: verification.id}, false, "the retarget of c2's serves")
	requireGate(t, rig, rig.issue.Key, model.GatedStepSpec, model.AskApprovalDocument{ArtifactID: rig.spec.id}, true, "the retarget of c2's serves, which moves no node of the spec query")

	// The move: c2's part_of leaves r1 for r2, every word unchanged.
	approveStanding(t, rig, rig.issue.Key, model.GatedStepCriteria, verification.id, "the move")
	rig.stateDrained(t, rig.issue.Key, blockTuple(c2, graphstore.TuplePartOf, refs.BlockNodeKind, r2, true), blockTuple(c2, graphstore.TuplePartOf, refs.BlockNodeKind, r1, false))
	moved := rig.record(t, rig.issue.Key, graphstore.QueryVerification)
	if moved.Digest == retargeted.Digest {
		t.Errorf("the move of c2 under %s leaves the verification digest at %s", r2, moved.Digest)
	}
	if index := slices.IndexFunc(moved.Nodes, func(node docs.TreeNode) bool { return node.ID == c2 }); index < 0 || moved.Nodes[index].Parent.ID != r2 {
		t.Errorf("after its move c2 stands in the verification record as %+v, want it under %s", moved.Nodes, r2)
	}
	requireWalkOrder(t, rig.walk(t, rig.issue.Key))
	requireGate(t, rig, rig.issue.Key, model.GatedStepCriteria, model.AskApprovalDocument{ArtifactID: verification.id}, false, "the move of c2 under "+r2)

	// A result: a node under c1, in the verification record, moving its digest. carryVerdict
	// carries the approval across it (digestStepResultCarry reads the carried review).
	recorded := recordResult(t, rig.handler, rig.issue.Key, resultBody("c1", "pass", headCommit, map[string]any{"surface": "cli"}))
	if recorded.Code != http.StatusCreated {
		t.Fatalf("record a result of c1: status=%d body=%s", recorded.Code, recorded.Body.String())
	}
	result := decodeBody[recordedResult](t, recorded).Result
	resulted := rig.record(t, rig.issue.Key, graphstore.QueryVerification)
	if resulted.Digest == moved.Digest {
		t.Errorf("a recorded result leaves the verification digest at %s", moved.Digest)
	}
	index := slices.IndexFunc(resulted.Nodes, func(node docs.TreeNode) bool { return node.Address() == graphstore.KindResult+":"+result.ID })
	if index < 0 || resulted.Nodes[index].Parent.ID != c1 || resulted.Nodes[index].Type != graphstore.SupertagResult {
		t.Errorf("the verification record takes %v, want the result %s under %s", walkAddresses(resulted.Nodes), result.ID, c1)
	}

	// The removal: c2 goes, the digest moves and the approval is superseded.
	approveStanding(t, rig, rig.issue.Key, model.GatedStepCriteria, verification.id, "the removal")
	response := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/issues/"+rig.issue.Key+"/artifacts", map[string]any{
		"name": "verification.md", "content": digestChecks("The list reads the urgent asks first.", ""),
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("upload the Verification document without c2: status=%d body=%s", response.Code, response.Body.String())
	}
	if advice := decodeBody[struct {
		Advice *testWriteAdvice `json:"advice"`
	}](t, response).Advice; advice == nil || advice.SupersededApproval == nil {
		t.Errorf("the removal of c2 answers advice %+v, want the approval of the verification query it superseded", advice)
	}
	removed := rig.record(t, rig.issue.Key, graphstore.QueryVerification)
	if removed.Digest == resulted.Digest {
		t.Errorf("the removal of c2 leaves the verification digest at %s", removed.Digest)
	}
	if slices.Contains(walkAddresses(removed.Nodes), blockAddress(c2)) {
		t.Errorf("the verification record still takes %s after its removal: %v", c2, walkAddresses(removed.Nodes))
	}
	requireGate(t, rig, rig.issue.Key, model.GatedStepCriteria, model.AskApprovalDocument{ArtifactID: verification.id}, false, "the removal of c2")
}

// digestStepValues changes one value of one check at a time - the visibility of one, the state of
// another, the placement of a third, whose part_of stays as it was - and reads the verification
// digest moved by each, with every node of the record where it stood: the record holds every value
// of a node but its rank and its text, which fields of their own carry.
func digestStepValues(t *testing.T, rig *digestRig) {
	ctx := context.Background()
	uploadDocument(t, rig.handler, "/api/v1/issues/"+rig.issue.Key, "verification.md", digestChecks("The list reads the urgent asks first.", "")+
		"\n:::check{#c3 serves=\"dispatch://CORE-1/spec#b-r2\" surface=\"cli\" run=\"go test ./...\"}\nThe list reads an ask's size.\n:::\n"+
		"\n:::check{#c4 serves=\"dispatch://CORE-1/spec#b-r2\" surface=\"cli\" run=\"go test ./...\"}\nThe list reads an ask's age.\n:::\n")
	// other is whichever of one and another held is not.
	other := func(held, one, another string) string {
		if held == one {
			return another
		}
		return one
	}
	for _, change := range []struct {
		check, attribute string
		to               func(held string) string
	}{
		{"c1", graphstore.AttributeVisibility, func(held string) string {
			return other(held, graphstore.VisibilityAuthor, graphstore.VisibilityProject)
		}},
		{"c3", graphstore.AttributeState, func(string) string { return graphstore.StateProposed }},
		{"c4", refs.AttributePlacement, func(held string) string { return other(held, refs.PlacementAuthored, refs.PlacementText) }},
	} {
		t.Run(change.attribute, func(t *testing.T) {
			check := rig.block(change.check)
			held, _, err := graphstore.CurrentValue(ctx, rig.database.Pool, refs.BlockNodeKind, check, change.attribute)
			if err != nil {
				t.Fatalf("read the %s of %s: %v", change.attribute, check, err)
			}
			to := change.to(held)
			if to == held {
				t.Fatalf("%s already holds the %s %q this step changes it to", check, change.attribute, to)
			}
			before := rig.record(t, rig.issue.Key, graphstore.QueryVerification)
			verification := digestGates(t, rig)
			approveStanding(t, rig, rig.issue.Key, model.GatedStepCriteria, verification.id, "the change of "+change.attribute)
			at := slices.IndexFunc(before.Nodes, func(node docs.TreeNode) bool { return node.ID == check })
			if at < 0 {
				t.Fatalf("the verification record takes no %s: %v", check, walkAddresses(before.Nodes))
			}
			rig.stateDrained(t, rig.issue.Key, blockValue(check, change.attribute, to))
			after := rig.record(t, rig.issue.Key, graphstore.QueryVerification)
			if after.Digest == before.Digest {
				t.Errorf("the verification digest held still across a change of %s of %s", change.attribute, blockAddress(check))
			}
			if !slices.Equal(walkAddresses(after.Nodes), walkAddresses(before.Nodes)) || after.Nodes[at].Parent != before.Nodes[at].Parent {
				t.Errorf("the change of %s of %s moves the verification record's nodes from %+v to %+v, want each where it stood",
					change.attribute, check, before.Nodes, after.Nodes)
			}
			requireGate(t, rig, rig.issue.Key, model.GatedStepCriteria, model.AskApprovalDocument{ArtifactID: verification.id}, false,
				"the change of "+change.attribute+" of "+blockAddress(check))
		})
	}
}

// editThroughTheArtifactRoute applies ops to the document artifactID through
// POST /api/v1/artifacts/{id}/edits as alice, as a named version, and fails unless the route takes
// them.
func editThroughTheArtifactRoute(t *testing.T, handler http.Handler, artifactID string, ops []map[string]any) {
	t.Helper()
	response := editDocument(t, handler, artifactID, map[string]any{"ops": ops, "summary": "edit"})
	if response.Code != http.StatusOK {
		t.Fatalf("edit %s: status=%d body=%s", artifactID, response.Code, response.Body.String())
	}
}

// digestStepTextOrder pins the text order, which the tree does not hold (R1, R14). A spec holds a
// criterion k1, the issue's first child, so the callout after it is no memo; the callout; a
// paragraph; and a requirement whose parent attribute names k1. The spec query is approved. A move
// of the requirement before the paragraph in the spec's text, through the edits route, leaves
// every node's place in the walk, its parent and its rank as they stood, and moves the
// requirement's text_rank alone: the spec digest moves and the approval reads stale. Approved
// again, a move of the requirement into the callout's text, the tree again unchanged, makes the
// callout its text_parent: the digest moves and the approval reads stale again.
func digestStepTextOrder(t *testing.T, rig *digestRig) {
	ctx := context.Background()
	issue := createDigestIssue(t, rig.handler, rig.database, "CORE", "A spec in its author's order",
		":::criterion{#k1}\nI want the urgent list.\n:::\n\n:::callout{#box kind=\"note\" title=\"\"}\nThe box words.\n:::\n\nA paragraph between.\n", false)
	spec := readDecisionDocument(t, rig.database, issue.Key, "spec")
	requirement := ":::requirement{#r1 parent=\"dispatch://" + issue.Key + "/spec#b-k1\"}\nAn urgent ask comes first.\n:::\n"
	editSpec(t, rig.handler, issue.Key, []map[string]any{{"op": "insert", "after": "end", "markdown": requirement}})
	r1 := spec.address("r1")
	// placed is each block of the spec the walk takes, by its address, with its parent and its rank
	// (a version's node, which each edit adds, is none of them); textRanks the text_rank of each.
	placed := func() map[string]string {
		nodes := map[string]string{}
		for _, node := range rig.walk(t, issue.Key).Nodes {
			if node.Kind == refs.BlockNodeKind && strings.HasPrefix(node.ID, spec.nodeKey+"#") {
				nodes[node.ID] = node.Parent.Kind + ":" + node.Parent.ID + " at " + node.Rank
			}
		}
		return nodes
	}
	textRanks := func() map[string]string {
		held := map[string]string{}
		for _, node := range rig.walk(t, issue.Key).Nodes {
			if node.Kind != refs.BlockNodeKind || !strings.HasPrefix(node.ID, spec.nodeKey+"#") {
				continue
			}
			value, _, err := graphstore.CurrentValue(ctx, rig.database.Pool, refs.BlockNodeKind, node.ID, refs.AttributeTextRank)
			if err != nil {
				t.Fatalf("read the text_rank of %s: %v", node.ID, err)
			}
			held[node.ID] = value
		}
		return held
	}
	approve := func() {
		t.Helper()
		reviewDocument(t, rig.handler, spec.id, "alice", "approved", "")
		if got := readApproval(t, rig.handler, spec.id); got.Approval == nil || got.Approval.State != "approved" {
			t.Fatalf("the spec of %s reads %+v after its approval, want approved", issue.Key, got.Approval)
		}
	}
	move := func(change string, ops []map[string]any) {
		t.Helper()
		walked, ranks, before := placed(), textRanks(), rig.record(t, issue.Key, graphstore.QuerySpec)
		requireUnder(t, "before the move in the text", rig.walk(t, issue.Key), blockAddress(r1), blockAddress(spec.address("k1")))
		editThroughTheArtifactRoute(t, rig.handler, spec.id, ops)
		// Every block that stands before and after the move stands under its parent at its rank
		// as it did; the criterion, the requirement and the callout stand after it.
		after := placed()
		for id, was := range walked {
			if now, stands := after[id]; stands && now != was {
				t.Errorf("the move of %s in the text moves %s from under %s to under %s, want every parent and rank as it stood", r1, id, was, now)
			}
		}
		for _, id := range []string{spec.address("k1"), r1, spec.address("box")} {
			if _, stands := after[id]; !stands {
				t.Errorf("after the move of %s in the text the walk takes no %s", r1, id)
			}
		}
		if change == refs.AttributeTextRank {
			for id, held := range textRanks() {
				if moved := held != ranks[id]; moved != (id == r1) {
					t.Errorf("the move of %s in the text moves the text_rank of %s from %q to %q, want the requirement's alone moved", r1, id, ranks[id], held)
				}
			}
		}
		if after := rig.record(t, issue.Key, graphstore.QuerySpec); after.Digest == before.Digest {
			t.Errorf("the spec digest held still across a change of %s of %s", change, blockAddress(r1))
		}
		if got := readApproval(t, rig.handler, spec.id); got.Approval == nil || got.Approval.State != "stale" {
			t.Errorf("the approval of the spec of %s reads %+v after the change of %s of %s, want stale", issue.Key, got.Approval, change, blockAddress(r1))
		}
	}

	approve()
	move(refs.AttributeTextRank, []map[string]any{{"op": "move", "block": "r1", "after": "block:box"}})
	approve()
	// Into the callout's text, after the callout's own paragraph: the move op sets a block beside a
	// top-level block, so the requirement goes in by a delete and an insert into the callout,
	// whose markdown carries the requirement's id and parent attribute. The paragraph inside the
	// requirement, whose id no markdown writes, takes a new id.
	move(refs.TupleTextParent, []map[string]any{{"op": "delete", "block": "r1"}, {"op": "insert", "into": "block:box", "markdown": requirement}})
	held, err := graphstore.CurrentOut(ctx, rig.database.Pool, refs.BlockNodeKind, r1, []string{refs.TupleTextParent})
	if err != nil || len(held) != 1 || held[0].ToID != spec.address("box") {
		t.Errorf("after the move into the callout %s's text parents are %+v (%v), want the callout %s", r1, held, err, spec.address("box"))
	}
}

func digestStepRanks(t *testing.T, rig *digestRig) {
	r1, r2 := rig.block("r1"), rig.block("r2")
	order := func(want ...string) {
		t.Helper()
		var got []string
		for _, node := range rig.record(t, rig.issue.Key, graphstore.QuerySpec).Nodes {
			if node.ID == r1 || node.ID == r2 {
				got = append(got, node.ID)
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("the spec record takes %v, want %v", got, want)
		}
	}
	held := func(address string) string {
		value, _, err := graphstore.CurrentValue(context.Background(), rig.database.Pool, refs.BlockNodeKind, address, refs.AttributeRank)
		if err != nil {
			t.Fatalf("read the rank of %s: %v", address, err)
		}
		return value
	}
	first, second := held(r1), held(r2)
	order(r1, r2)
	// Two ranks that differ only in case at one position: B before a, byte for byte.
	rig.state(t, blockValue(r1, refs.AttributeRank, "1a"), blockValue(r2, refs.AttributeRank, "1B"))
	order(r2, r1)
	// The two re-ranked the other way round.
	rig.state(t, blockValue(r1, refs.AttributeRank, "1B"), blockValue(r2, refs.AttributeRank, "1a"))
	order(r1, r2)
	// One rank: the lower node id first.
	rig.state(t, blockValue(r1, refs.AttributeRank, "1m"), blockValue(r2, refs.AttributeRank, "1m"))
	if r1 >= r2 {
		t.Fatalf("%s is not the lower id of %s", r1, r2)
	}
	order(r1, r2)
	requireWalkOrder(t, rig.walk(t, rig.issue.Key))
	rig.state(t, blockValue(r1, refs.AttributeRank, first), blockValue(r2, refs.AttributeRank, second))
	order(r1, r2)
}

func digestStepLengths(t *testing.T, rig *digestRig) {
	r1 := rig.block("r1")
	// One store holds, on r1, the value x2 ab - the record's separator and a length prefix of its
	// own - under probe and y under probf; the other x2 abprobfy under probe and no probf. Written
	// with no lengths the two run to one byte string, probex2 abprobfy.
	rig.state(t, blockValue(r1, "probe", "x2 ab"), blockValue(r1, "probf", "y"))
	split := rig.record(t, rig.issue.Key, graphstore.QuerySpec)
	rig.state(t, blockValue(r1, "probe", "x2 abprobfy"), blockTuple(r1, "probf", graphstore.KindValue, "y", false))
	joined := rig.record(t, rig.issue.Key, graphstore.QuerySpec)
	if split.Digest == joined.Digest {
		t.Errorf("the values x2 ab and y and the one value x2 abprobfy read one digest, %s", split.Digest)
	}
	if !slices.Equal(walkAddresses(split.Nodes), walkAddresses(joined.Nodes)) {
		t.Errorf("the spec query takes %v and then %v, want the same nodes", walkAddresses(split.Nodes), walkAddresses(joined.Nodes))
	}
	rig.state(t, blockTuple(r1, "probe", graphstore.KindValue, "x2 abprobfy", false))
}

func digestStepVersions(t *testing.T, rig *digestRig) {
	ctx := context.Background()
	cut, err := refs.HeadCut(ctx, rig.database.Pool)
	if err != nil {
		t.Fatalf("read the head's cut: %v", err)
	}
	head := rig.record(t, rig.issue.Key, graphstore.QuerySpec)
	version := rig.recordAt(t, rig.issue.Key, graphstore.QuerySpec, cut)
	if !bytes.Equal(head.Record, version.Record) || head.Digest != version.Digest || version.Cut != cut {
		t.Errorf("the head's record (%s) and the record at the cut %+v (%s, read at %+v) differ with nothing written between", head.Digest, cut, version.Digest, version.Cut)
	}
	later, _ := personComment(t, rig.handler, rig.database, rig.issue.Key, "A remark after the version.")
	walked, err := docs.WalkIssueTree(ctx, rig.database.Pool, rig.issue.Key, cut)
	if err != nil {
		t.Fatalf("walk %s at %+v: %v", rig.issue.Key, cut, err)
	}
	if at := walkIndex(walked, graphstore.KindComment+":"+later); at >= 0 {
		t.Errorf("the walk of the version at %+v takes the later comment %s", cut, later)
	}
	if at := walkIndex(rig.walk(t, rig.issue.Key), graphstore.KindComment+":"+later); at < 0 {
		t.Errorf("the walk at the head takes no comment %s", later)
	}
	if again := rig.recordAt(t, rig.issue.Key, graphstore.QuerySpec, cut); again.Digest != version.Digest {
		t.Errorf("the version's record read after a later comment answers %s, want %s", again.Digest, version.Digest)
	}
}

func digestStepUnranked(t *testing.T, rig *digestRig) {
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	// The earlier comment carries the greater id, the later one the lower; a reply carries an id
	// lower than its head's.
	const earlier, later = "eeeeeeee-0000-4000-8000-000000000001", "11111111-0000-4000-8000-000000000002"
	const head, reply = "dddddddd-0000-4000-8000-000000000003", "00000000-0000-4000-8000-000000000004"
	insertPersonComment(t, rig.database, rig.issue.Key, earlier, "", "The first remark.", at)
	insertPersonComment(t, rig.database, rig.issue.Key, later, "", "The second remark.", at.Add(time.Second))
	insertPersonComment(t, rig.database, rig.issue.Key, head, "", "A thread's head.", at.Add(2*time.Second))
	insertPersonComment(t, rig.database, rig.issue.Key, reply, head, "Its reply.", at.Add(3*time.Second))
	tree := rig.walk(t, rig.issue.Key)
	requireWalkOrder(t, tree)
	issue := graphstore.KindIssue + ":" + rig.issue.Key
	first, second := walkIndex(tree, graphstore.KindComment+":"+earlier), walkIndex(tree, graphstore.KindComment+":"+later)
	if first < 0 || second < 0 || first >= second {
		t.Errorf("the walk takes the earlier comment at %d and the later at %d, want the earlier first whatever their ids", first, second)
	}
	// Spec v246, R8: "Among a node's children the conversation nodes come first ... Then come the
	// ranked siblings, by rank", and the memo stands first, before any conversation node, so every
	// ranked child of the issue but the memo now stands after its last comment, where it stood
	// before the first.
	conversation := -1
	for index, node := range tree.Nodes {
		if refs.OrderedAsConversation(node.Kind) && node.Parent.Kind+":"+node.Parent.ID == issue {
			conversation = index
		}
	}
	if tree.Memo != 1 {
		t.Errorf("the walk's memo is %d, want 1: the callout stands first under the issue, before its comments", tree.Memo)
	}
	for index, node := range tree.Nodes {
		if node.Ranked && node.Parent.Kind+":"+node.Parent.ID == issue && index != tree.Memo && index < conversation {
			t.Errorf("the walk takes the issue's ranked child %s at %d, before its conversation node %s at %d", node.Address(), index, tree.Nodes[conversation].Address(), conversation)
		}
	}
	requireUnder(t, "a reply", tree, graphstore.KindComment+":"+reply, graphstore.KindComment+":"+head)
}

func digestStepRequirementUnderComment(t *testing.T, rig *digestRig) {
	comment, block := personComment(t, rig.handler, rig.database, rig.issue.Key, "We need the list sized.\n\nThen ordered.")
	editSpec(t, rig.handler, rig.issue.Key, []map[string]any{{"op": "insert", "after": "end",
		"markdown": ":::requirement{#r9 parent=\"" + block + "\"}\nEach ask carries its size.\n:::\n"}})
	tree := rig.walk(t, rig.issue.Key)
	requireWalkOrder(t, tree)
	r9 := blockAddress(rig.block("r9"))
	at, said := walkIndex(tree, r9), walkIndex(tree, graphstore.KindComment+":"+comment)
	if at < 0 || said < 0 || said >= at {
		t.Fatalf("the walk takes the comment at %d and r9 at %d, want the comment first", said, at)
	}
	if tree.Nodes[at].Above || tree.Nodes[at].Parent.Kind != refs.ItemComment.BlockKind() {
		t.Errorf("r9 stands as %+v, want it in the walk's first part under the comment's block", tree.Nodes[at])
	}
	if !slices.Contains(walkAddresses(rig.record(t, rig.issue.Key, graphstore.QuerySpec).Nodes), r9) {
		t.Errorf("the spec record takes no r9")
	}
}

func digestStepChildCriterion(t *testing.T, rig *digestRig) {
	// The child is no view, so its spec query takes its criterion and requirement nodes: a view's
	// takes its memo alone (docs' queryDefinitions). A child born converted under the rig's issue,
	// whose requirements stand, is a view and may hold no requirement of its own, so this child is
	// one the release before created, holding a requirement of its own, as the owning child of the
	// tree read's view test is.
	child := createDigestChild(t, rig.handler, rig.database, "CORE", "The owning child",
		":::requirement{#cr1}\nThe child's own requirement.\n:::\n", rig.issue.Key, true)
	_, block := personComment(t, rig.handler, rig.database, rig.issue.Key, "I want it quick.\n\nAnd right.")
	parent := rig.record(t, rig.issue.Key, graphstore.QuerySpec)
	childBefore := rig.record(t, child.Key, graphstore.QuerySpec)
	created(t, "a criterion of the child under its parent's comment", uploadChecks(t, rig.handler, child.Key,
		":::criterion{#ck1 parent=\""+block+"\"}\nI want it quick.\n:::\n"))
	criterion := blockAddress(readDecisionDocument(t, rig.database, child.Key, "checks-md").address("ck1"))
	tree := rig.walk(t, child.Key)
	at := walkIndex(tree, criterion)
	if at < 0 || !tree.Nodes[at].Above {
		t.Fatalf("the child's walk takes %s at %d as %+v, want it in the second part", criterion, at, tree.Nodes[max(at, 0)])
	}
	for index, node := range tree.Nodes[:at] {
		if node.Above {
			t.Errorf("the child's walk takes %s at %d in its second part, before %s", node.Address(), index, criterion)
		}
	}
	record := rig.record(t, child.Key, graphstore.QuerySpec)
	if got := walkAddresses(record.Nodes); len(got) == 0 || got[len(got)-1] != criterion {
		t.Errorf("the child's spec record takes %v, want the criterion last", got)
	}
	if record.Digest == childBefore.Digest {
		t.Errorf("the criterion's write leaves the child's spec digest at %s", record.Digest)
	}
	if after := rig.record(t, rig.issue.Key, graphstore.QuerySpec); after.Digest != parent.Digest || slices.Contains(walkAddresses(after.Nodes), criterion) {
		t.Errorf("the child's criterion moves the parent's spec digest from %s to %s (nodes %v)", parent.Digest, after.Digest, walkAddresses(after.Nodes))
	}
	if walkIndex(rig.walk(t, rig.issue.Key), criterion) >= 0 {
		t.Errorf("the parent's walk takes the child's criterion %s", criterion)
	}
	created(t, "the child's criterion reworded", uploadChecks(t, rig.handler, child.Key,
		":::criterion{#ck1 parent=\""+block+"\"}\nI want it quick, and right.\n:::\n"))
	if reworded := rig.record(t, child.Key, graphstore.QuerySpec); reworded.Digest == record.Digest {
		t.Errorf("a word of the child's criterion leaves the child's spec digest at %s", record.Digest)
	}
	if after := rig.record(t, rig.issue.Key, graphstore.QuerySpec); after.Digest != parent.Digest {
		t.Errorf("a word of the child's criterion moves the parent's spec digest from %s to %s", parent.Digest, after.Digest)
	}
}

func digestStepAnchoredComment(t *testing.T, rig *digestRig) {
	const words = "We ship the urgent list first."
	for _, unconverted := range []bool{false, true} {
		issue := createDigestIssue(t, rig.handler, rig.database, "CORE", "A decided issue "+strconv.FormatBool(unconverted),
			"An opening paragraph.\n\n:::decision{#d1}\n"+words+"\n:::\n", unconverted)
		spec := readDecisionDocument(t, rig.database, issue.Key, "spec")
		paragraph := blockIDOfText(t, rig.handler, issue.PrimaryArtifactID, words)
		artifact := "spec"
		if !unconverted {
			artifact = issue.PrimaryArtifactID
		}
		comment := createBlockAnchoredComment(t, rig.handler, issue.Key, map[string]any{"artifact": artifact, "quote": "urgent list", "block_id": paragraph})
		tree := rig.walk(t, issue.Key)
		requireWalkOrder(t, tree)
		// The comment is a node under the node whose text it quotes (R31): its part_of names the
		// paragraph its anchored_in names, which stands under the decision.
		inside := blockAddress(spec.address(paragraph))
		requireUnder(t, "a comment anchored inside the decision's paragraph (converted "+strconv.FormatBool(!unconverted)+")",
			tree, graphstore.KindComment+":"+comment.ID, inside)
		requireUnder(t, "the decision's paragraph (converted "+strconv.FormatBool(!unconverted)+")",
			tree, inside, blockAddress(spec.address("d1")))
		if unconverted {
			continue
		}
		// A comment whose anchor names an old address of the paragraph, which a moved_to carries to
		// the paragraph's node in the room, stands under the paragraph too. Its anchored_in and its
		// part_of move together, as every writer of its anchored_in moves them (R31).
		moved := createBlockAnchoredComment(t, rig.handler, issue.Key, map[string]any{"artifact": artifact, "quote": "urgent list", "block_id": paragraph})
		old := issue.Key + "/spec-before#" + paragraph
		now := time.Now().UTC()
		rig.state(t, slices.Concat(
			graphstore.AnchorTuples(nil, moved.ID, refs.BlockNodeKind, spec.address(paragraph), true, false, digestWriter, now, graphstore.NoEvent),
			graphstore.AnchorTuples(nil, moved.ID, refs.BlockNodeKind, old, true, true, digestWriter, now, graphstore.NoEvent),
			[]graphstore.Tuple{blockTuple(old, graphstore.TupleMovedTo, refs.BlockNodeKind, spec.address(paragraph), true)})...)
		requireUnder(t, "a comment anchored at an old address of the decision's paragraph", rig.walk(t, issue.Key),
			graphstore.KindComment+":"+moved.ID, inside)
	}
}

// pinnedRecord is the spec record of PIN-1, whose tree holds one requirement, in the record's
// layout: the node's address, its type, its parent's address, its rank, its values field -
// placement, probe, size, state, text_rank and visibility, in the byte order of their names, each a
// name and a value - its body text, and its relations field - its part_of, its serves and its
// text_parent, by kind and then far end, each a kind and a far end - each field its byte length,
// one space, then its bytes. The rank and the text are the values the record leaves out of its
// values field, since fields of their own carry them; the text_rank is a value like any other.
const pinnedRecord = "19 block:PIN-1/spec#r1" +
	"11 requirement" +
	"11 issue:PIN-1" +
	"2 1V" +
	"94 9 placement8 authored5 probe3 a b4 size3 1.55 state4 live9 text_rank2 1W10 visibility7 project" +
	"35 :::requirement{#r1}\nThe words.\n:::\n" +
	"89 7 part_of11 issue:PIN-16 serves19 block:PIN-1/spec#k111 text_parent19 artifact:PIN-1/spec"

// pinnedDigest is the sha256 of pinnedRecord, in lower-case hex.
const pinnedDigest = "3a9e0148761490e37d1e31ebd7166196392a8f1dec3f1ac79b972e400f92f56e"

// The record's layout is pinned byte for byte: a tree of one requirement whose every field is
// written straight into the store reads back as pinnedRecord, with pinnedDigest as its digest. A
// change to the layout - a field's order, its prefix, a value left in or out, the order of the
// values or the relations - fails it.
func TestTheRecordOfOneNodeIsItsLayoutByteForByte(t *testing.T) {
	database := storetest.Open(t)
	const requirement = "PIN-1/spec#r1"
	(&digestRig{database: database}).state(t,
		blockTuple(requirement, graphstore.TuplePartOf, graphstore.KindIssue, "PIN-1", true),
		blockValue(requirement, refs.AttributeRank, "1V"),
		blockValue(requirement, graphstore.AttributeText, ":::requirement{#r1}\nThe words.\n:::\n"),
		blockValue(requirement, "size", "1.5"),
		blockValue(requirement, "probe", "a b"),
		blockValue(requirement, refs.AttributePlacement, refs.PlacementAuthored),
		blockValue(requirement, graphstore.AttributeState, graphstore.StateLive),
		blockValue(requirement, graphstore.AttributeVisibility, graphstore.VisibilityProject),
		blockValue(requirement, refs.AttributeTextRank, "1W"),
		blockTuple(requirement, "serves", refs.BlockNodeKind, "PIN-1/spec#k1", true),
		blockTuple(requirement, refs.TupleTextParent, graphstore.KindArtifact, "PIN-1/spec", true),
	)
	record, err := docs.ReadQueryRecord(context.Background(), database.Pool, "PIN-1", graphstore.QuerySpec, refs.TreeCut{})
	if err != nil {
		t.Fatalf("read the spec record of PIN-1: %v", err)
	}
	if string(record.Record) != pinnedRecord {
		t.Errorf("the spec record of PIN-1 reads\n%q\nwant\n%q", record.Record, pinnedRecord)
	}
	if record.Digest != pinnedDigest {
		t.Errorf("the spec record of PIN-1 digests to %s, want %s", record.Digest, pinnedDigest)
	}
}

// versionDigestOf is the digest artifact_version_text_sha256 answers for version number of the
// document artifactID: the digest an approval of the version pins and a gate compares.
func versionDigestOf(t *testing.T, database *store.Store, artifactID string, number int) string {
	t.Helper()
	var digest *string
	if err := database.Pool.QueryRow(context.Background(), `select artifact_version_text_sha256($1::uuid, $2)`, artifactID, number).Scan(&digest); err != nil || digest == nil {
		t.Fatalf("read the digest version %d of %s answers (%v): %v", number, artifactID, digest, err)
	}
	return *digest
}

// requireGate fails unless every place that decides whether the query document names stands
// approved answers want: the issue read's design gate - GET /api/v1/issues/{key}, whose
// design_gates name the document a gated step records and whose artifacts carry that document's
// approval, which the gate reads - the artifact's approval read, and docs.NamedDocumentsApproved
// over the document's newest version. Each failure quotes the query and the digest the gate
// compared, the digest artifact_version_text_sha256 answers for the newest version.
func requireGate(t *testing.T, rig *digestRig, key, step string, document model.AskApprovalDocument, want bool, when string) {
	t.Helper()
	ctx := context.Background()
	var newest int
	var query *string
	if err := rig.database.Pool.QueryRow(ctx, `
		select (select max(number) from artifact_versions where artifact_id = a.id),
		       (select n.attributes->>'`+graphstore.AttributeQuery+`' from graph_nodes n where n.kind = 'artifact' and n.id = a.ref_key)
		from artifacts a where a.id = $1::uuid`, document.ArtifactID).Scan(&newest, &query); err != nil {
		t.Fatalf("%s: read the newest version of %s: %v", when, document.ArtifactID, err)
	}
	named := "document"
	if query != nil {
		named = *query + " query"
	}
	digest := versionDigestOf(t, rig.database, document.ArtifactID, newest)
	approved := func(state string) bool { return state == "approved" }

	type gatedArtifact struct {
		ID       string `json:"id"`
		Approval *struct {
			State string `json:"state"`
		} `json:"approval"`
	}
	read := decodeBody[struct {
		DesignGates []issueDesignGate `json:"design_gates"`
		Artifacts   []gatedArtifact   `json:"artifacts"`
	}](t, dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice"))
	gate := slices.IndexFunc(read.DesignGates, func(gate issueDesignGate) bool { return gate.Step == step })
	if gate < 0 || read.DesignGates[gate].ArtifactID != document.ArtifactID {
		t.Fatalf("%s: the design gates of %s read %+v, want the step %s on %s", when, key, read.DesignGates, step, document.ArtifactID)
	}
	held := slices.IndexFunc(read.Artifacts, func(artifact gatedArtifact) bool { return artifact.ID == document.ArtifactID })
	if held < 0 || read.Artifacts[held].Approval == nil || approved(read.Artifacts[held].Approval.State) != want {
		var state any = "no document"
		if held >= 0 {
			state = read.Artifacts[held].Approval
		}
		t.Errorf("%s: the issue read's design gate %s of %s reads the %s of version %d approved %t (%+v), want %t, comparing the digest %s",
			when, step, key, named, newest, !want, state, want, digest)
	}
	document.Version = newest
	if decided, err := docs.NamedDocumentsApproved(ctx, rig.database.Pool, []model.AskApprovalDocument{document}, "", false); err != nil || decided != want {
		t.Errorf("%s: docs.NamedDocumentsApproved reads %t (%v) for version %d of the %s of %s, want %t, comparing the digest %s",
			when, decided, err, newest, named, key, want, digest)
	}
	if got := readApproval(t, rig.handler, document.ArtifactID); got.Approval == nil || approved(got.Approval.State) != want {
		t.Errorf("%s: the approval read of the %s of %s reads %+v at version %d, want approved %t, comparing the digest %s",
			when, named, key, got.Approval, newest, want, digest)
	}
}

// digestStepGate pins the gate by digest (R14): a query is approved only while the review stands
// on the digest its version answers now, and not only while the version numbers match. A spec
// holds version 1, which the version write stated with its record's digest, and a person's
// approval of it pins that digest. A version 2 written by SQL, as an older build wrote one, holds
// the stored markdown's text of version 1 and no version_renders row, so it answers the sha256 of
// that markdown, and a person's approval of it pins that digest; the design gate and the approval
// read both read it approved. The pass that states it then writes its row with its record's digest,
// which the version answers from then on, and the approval of version 2 reads stale in the approval
// read, the design gate and docs.NamedDocumentsApproved, each quoting the digest it compared,
// while the review itself stays approved at version 2. Once the review's row of
// artifact_review_carries holds the record's digest, as the fill and the carry of a pass write
// it, the approval stands on that digest.
func digestStepGate(t *testing.T, rig *digestRig) {
	ctx := context.Background()
	issue := createDigestIssue(t, rig.handler, rig.database, "CORE", "A gated spec", ":::requirement{#r1}\nThe gated list comes first.\n:::\n", false)
	spec := readDecisionDocument(t, rig.database, issue.Key, "spec")
	publishDesignGates(t, rig.handler, issue.Key, []issueDesignGate{{Step: model.GatedStepSpec, ArtifactID: spec.id}})
	first := model.AskApprovalDocument{ArtifactID: spec.id, Version: 1}
	second := model.AskApprovalDocument{ArtifactID: spec.id, Version: 2}

	review := reviewDocument(t, rig.handler, spec.id, "alice", "approved", "")
	if want := versionDigestOf(t, rig.database, spec.id, 1); review.ContentSHA256 == nil || *review.ContentSHA256 != want {
		t.Fatalf("the review of version 1 of %s pins %v, want the digest %s the version answers", spec.refKey, review.ContentSHA256, want)
	}
	requireGate(t, rig, issue.Key, model.GatedStepSpec, first, true, "version 1, approved after the pass that stated it")

	// Version 2, written by SQL with version 1's text: no pass states it, so it answers the sha256
	// of its stored markdown, which the person's approval of it pins.
	markdown := queryVersionMarkdown(t, rig.database, spec.id, 1)
	if _, err := rig.database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1::uuid, 2, $2, '[{"kind":"user","id":"alice"}]')`, spec.id, markdown); err != nil {
		t.Fatalf("write version 2 of %s as an older build writes one: %v", spec.refKey, err)
	}
	stored := contentHash(markdown)
	if got := versionDigestOf(t, rig.database, spec.id, 2); got != stored {
		t.Fatalf("version 2 of %s, which no pass has stated, answers the digest %s, want the sha256 of its stored markdown, %s", spec.refKey, got, stored)
	}
	approved := reviewDocument(t, rig.handler, spec.id, "alice", "approved", "")
	if approved.Version != 2 || approved.ContentSHA256 == nil || *approved.ContentSHA256 != stored {
		t.Fatalf("the review %s of version 2 of %s names version %d and pins %v, want version 2 and the markdown's digest %s", approved.ID, spec.refKey, approved.Version, approved.ContentSHA256, stored)
	}
	requireGate(t, rig, issue.Key, model.GatedStepSpec, second, true, "version 2, approved before any pass states it")

	// The pass that states version 2 writes its row with its record's digest, which the version
	// answers from then on and the review does not hold.
	record := rig.record(t, issue.Key, graphstore.QuerySpec).Digest
	if record == stored {
		t.Fatalf("the record of %s digests to the sha256 of version 2's markdown, %s, so no pass could tell the two digests apart", spec.refKey, record)
	}
	if _, err := rig.database.Pool.Exec(ctx, `
		insert into version_renders (artifact_id, number, record_sha256, at, read_seq) values ($1::uuid, 2, $2, now(), 0)`, spec.id, record); err != nil {
		t.Fatalf("write the version_renders row of version 2 of %s: %v", spec.refKey, err)
	}
	if got := versionDigestOf(t, rig.database, spec.id, 2); got != record {
		t.Fatalf("version 2 of %s, whose pass recorded its record's digest, answers %s, want %s", spec.refKey, got, record)
	}
	requireGate(t, rig, issue.Key, model.GatedStepSpec, second, false, "version 2, whose pass recorded a digest the review did not pin")
	// A historical review answers for its own version alone: this one is still an approval of
	// version 2, and reads so.
	if reviews := listReviews(t, rig.handler, spec.id); len(reviews) == 0 || reviews[0].ID != approved.ID || reviews[0].State != "approved" || reviews[0].Version != 2 {
		t.Errorf("the reviews of %s read %+v, want the approval %s of version 2 still newest and approved", spec.refKey, reviews, approved.ID)
	}

	// The fill and the carry of a pass repin a carried review on its row of artifact_review_carries,
	// which every comparison reads where it holds a digest.
	if _, err := rig.database.Pool.Exec(ctx, `
		insert into artifact_review_carries (review_id, carried_from_version, record_sha256) values ($1::uuid, 1, $2)`, approved.ID, record); err != nil {
		t.Fatalf("repin the review %s on its row of artifact_review_carries: %v", approved.ID, err)
	}
	requireGate(t, rig, issue.Key, model.GatedStepSpec, second, true, "version 2, whose review's carry row holds the record's digest")
}

// digestVerifiedIssue creates an issue born converted with a spec of one requirement r1 and a
// Verification document of two checks, c1 and c2, each serving r1, and answers the issue and its
// Verification document.
func digestVerifiedIssue(t *testing.T, rig *digestRig, title string) (model.Issue, decisionDocument) {
	t.Helper()
	issue := createDigestIssue(t, rig.handler, rig.database, "CORE", title, ":::requirement{#r1}\nThe list comes first.\n:::\n", false)
	serves := "dispatch://" + issue.Key + "/spec#b-r1"
	uploadDocument(t, rig.handler, "/api/v1/issues/"+issue.Key, "verification.md",
		":::check{#c1 serves=\""+serves+"\" surface=\"cli\" run=\"go test ./...\"}\nThe list reads urgent asks first.\n:::\n\n"+
			":::check{#c2 serves=\""+serves+"\" surface=\"cli\" run=\"go test ./...\"}\nThe list reads an ask's urgency back.\n:::\n")
	return issue, readDecisionDocument(t, rig.database, issue.Key, "verification-md")
}

// recordDigestResult records a passed result of check on issue as alice and answers how many
// versions the Verification document holds after it.
func recordDigestResult(t *testing.T, rig *digestRig, issue, verification, check string) int {
	t.Helper()
	recorded := recordResult(t, rig.handler, issue, resultBody(check, "pass", headCommit, map[string]any{"surface": "cli"}))
	if recorded.Code != http.StatusCreated {
		t.Fatalf("record a result of %s on %s: status=%d body=%s", check, issue, recorded.Code, recorded.Body.String())
	}
	return len(documentVersions(t, rig.handler, verification))
}

// digestStepResultCarry pins the result carry (R14): a result recorded on a check moves the
// Verification record by an appended result node alone, the drain writes the version that holds it,
// and Dispatch writes a review in alice's name at that version, whose reason opens as every carried
// reason does and which names the version she approved. The review pins the record's digest, which
// the version answers, its row of artifact_review_carries names the version it carries from, and
// alice's own review of the version before reads as her own act.
func digestStepResultCarry(t *testing.T, rig *digestRig) {
	issue, verification := digestVerifiedIssue(t, rig, "A carried verification")
	own := reviewDocument(t, rig.handler, verification.id, "alice", "approved", "")
	before := len(documentVersions(t, rig.handler, verification.id))
	if after := recordDigestResult(t, rig, issue.Key, verification.id, "c1"); after != before+1 {
		t.Fatalf("a result of c1 left %d versions of the Verification document of %s, want %d: the drain writes the version that holds the result", after, issue.Key, before+1)
	}
	latest := before + 1
	reviews := listReviews(t, rig.handler, verification.id)
	if len(reviews) != 2 || reviews[1].ID != own.ID {
		t.Fatalf("the reviews of %s read %+v after the result, want alice's review %s and one carried review of it", verification.refKey, reviews, own.ID)
	}
	carried := reviews[0]
	if carried.Version != latest || carried.State != "approved" || carried.Kind != docs.ReviewKindPerson || carried.Actor.ID != "alice" ||
		carried.Reason == nil || !strings.HasPrefix(*carried.Reason, docs.CarriedReviewReasonPrefix) {
		t.Errorf("the carried review of %s reads %+v, want alice's approval of version %d with a reason opening %q", verification.refKey, carried, latest, docs.CarriedReviewReasonPrefix)
	}
	// The reason names the result that arrived, by its check's block id and its verdict, after the
	// opening the daemon matches.
	wantReason := fmt.Sprintf("%s%d: the results of c1 (pass), which arrived after it, were the whole change since it.", docs.CarriedReviewReasonPrefix, own.Version)
	if carried.Reason == nil || *carried.Reason != wantReason {
		t.Errorf("the carried review of %s states the reason %v, want %q: the carried review states the results that arrived after it",
			verification.refKey, carried.Reason, wantReason)
	}
	if carried.CarriedFromVersion == nil || *carried.CarriedFromVersion != own.Version || reviews[1].CarriedFromVersion != nil {
		t.Errorf("the carried review names version %v and alice's own review names %v, want %d and none", carried.CarriedFromVersion, reviews[1].CarriedFromVersion, own.Version)
	}
	if want := versionDigestOf(t, rig.database, verification.id, latest); carried.ContentSHA256 == nil || *carried.ContentSHA256 != want {
		t.Errorf("the carried review pins %v, want the digest %s version %d answers", carried.ContentSHA256, want, latest)
	}
	if from, held := carriedFromRow(t, rig.database, carried.ID); !held || from != own.Version {
		t.Errorf("the carried review %s holds %s, want the artifact_review_carries row naming version %d", carried.ID, carriedFromRowWords(from, held), own.Version)
	}
	if got := readApproval(t, rig.handler, verification.id); got.Approval == nil || got.Approval.State != "approved" || got.Approval.Version == nil || *got.Approval.Version != latest {
		t.Errorf("the approval of %s reads %+v after the result, want approved at version %d", verification.refKey, got.Approval, latest)
	}
}

// digestStepDefinitionControl is c-approval-bytes' ninth control (R14). The verification approval
// stands at a version holding definition_version 1. The query node is restated with
// definition_version 2, and a result recorded on a check writes a version that differs from the
// approved one by that appended result alone, which a carry would otherwise carry: the version
// holds definition_version 2, so no carry crosses the two definitions, no review is written, and the
// approval reads stale.
func digestStepDefinitionControl(t *testing.T, rig *digestRig) {
	issue, verification := digestVerifiedIssue(t, rig, "A verification whose definition changes")
	own := reviewDocument(t, rig.handler, verification.id, "alice", "approved", "")
	if supertag, held := versionDefinition(t, rig.database, verification.refKey, own.Version); supertag != graphstore.SupertagQueryVersion || valueOrNone(held) != "1" {
		t.Fatalf("the approved version %d of %s is a %s holding definition_version %s, want a query version holding 1", own.Version, verification.refKey, supertag, valueOrNone(held))
	}
	node := graphstore.QueryNode(verification.refKey, "CORE", graphstore.QueryVerification, digestWriter, time.Now().UTC())
	node.Attributes[graphstore.AttributeDefinitionVersion] = graphstore.QueryDefinitionVersion + 1
	rig.write(t, []graphstore.Node{node}, graphstore.AttributeFacts(nil, node))
	before := len(documentVersions(t, rig.handler, verification.id))
	if after := recordDigestResult(t, rig, issue.Key, verification.id, "c2"); after != before+1 {
		t.Fatalf("a result of c2 left %d versions of the Verification document of %s, want %d", after, issue.Key, before+1)
	}
	latest := before + 1
	if supertag, held := versionDefinition(t, rig.database, verification.refKey, latest); supertag != graphstore.SupertagQueryVersion || valueOrNone(held) != "2" {
		t.Fatalf("version %d of %s, which the result wrote, is a %s holding definition_version %s, want a query version holding 2", latest, verification.refKey, supertag, valueOrNone(held))
	}
	if reviews := listReviews(t, rig.handler, verification.id); len(reviews) != 1 || reviews[0].ID != own.ID {
		t.Errorf("the approval of verification at version %d carried to version %d, whose definition_version is 2, not 1: the reviews of %s read %+v", own.Version, latest, verification.refKey, reviews)
	}
	if got := readApproval(t, rig.handler, verification.id); got.Approval == nil || got.Approval.State != "stale" {
		t.Errorf("the approval of verification at version %d reads %+v at version %d, whose definition_version is 2, want stale", own.Version, got.Approval, latest)
	}
}

// requireApprovesStandForRow fails unless the node of the approval ask states, for each entry of
// its row's approval.documents, one approves tuple to the query version node that entry names
// (<ref_key>@v<version>), and none besides: the tuples are the record, and the row its projection.
func requireApprovesStandForRow(t *testing.T, rig *digestRig, ask string, refKeys map[string]string, when string) {
	t.Helper()
	ctx := context.Background()
	var encoded []byte
	if err := rig.database.Pool.QueryRow(ctx, `select approval -> 'documents' from asks where id = $1::uuid`, ask).Scan(&encoded); err != nil {
		t.Fatalf("%s: read the documents of the request %s: %v", when, ask, err)
	}
	var documents []model.AskApprovalDocument
	if err := json.Unmarshal(encoded, &documents); err != nil {
		t.Fatalf("%s: decode the documents of the request %s: %v", when, ask, err)
	}
	var want []string
	for _, document := range documents {
		want = append(want, graphstore.KindVersion+":"+refKeys[document.ArtifactID]+"@v"+strconv.Itoa(document.Version))
	}
	held, err := graphstore.CurrentOut(ctx, rig.database.Pool, graphstore.KindAsk, ask, []string{graphcensus.TupleApproves})
	if err != nil {
		t.Fatalf("%s: read the approves tuples of the request %s: %v", when, ask, err)
	}
	var got []string
	for _, tuple := range held {
		got = append(got, tuple.ToKind+":"+tuple.ToID)
		var exists bool
		if err := rig.database.Pool.QueryRow(ctx, `select exists (select 1 from graph_nodes where kind = $1 and id = $2)`, tuple.ToKind, tuple.ToID).Scan(&exists); err != nil || !exists {
			t.Errorf("%s: the request %s approves %s:%s, which the store holds no node of (%v)", when, ask, tuple.ToKind, tuple.ToID, err)
		}
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("%s: the node of the request %s states approves tuples to %v, want the version nodes %v that approval.documents %+v names", when, ask, got, want, documents)
	}
}

// digestStepApproves pins the request's own pointer as a tuple (R14): a request over the spec and
// the Verification document appends one approves tuple from its ask's node to each query version
// node it names, pins the store revision it was taken at as a value of that node, and its row's
// approval.documents holds the artifact uuid and the version integer of those version nodes. The
// approval ask's read answers the first document's artifact_id and version beside requested_version.
// A version of the spec moves the request to it, and the tuples move with the row: the approves of
// the version it left is retracted and one to the new version is asserted.
func digestStepApproves(t *testing.T, rig *digestRig) {
	ctx := context.Background()
	issue, verification := digestVerifiedIssue(t, rig, "An approval request over two queries")
	spec := readDecisionDocument(t, rig.database, issue.Key, "spec")
	refKeys := map[string]string{spec.id: spec.refKey, verification.id: verification.refKey}
	cut, err := refs.HeadCut(ctx, rig.database.Pool)
	if err != nil {
		t.Fatalf("read the head's cut: %v", err)
	}
	requested := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/artifacts/"+spec.id+"/approval-requests", map[string]any{"include_verification": true}, "alice")
	if requested.Code != http.StatusCreated {
		t.Fatalf("request the approval of the spec and the Verification document of %s: status=%d body=%s", issue.Key, requested.Code, requested.Body.String())
	}
	ask := decodeBody[struct {
		Ask struct {
			ID string `json:"id"`
		} `json:"ask"`
	}](t, requested).Ask.ID
	requireApprovesStandForRow(t, rig, ask, refKeys, "after the request opened")
	var documents int
	if err := rig.database.Pool.QueryRow(ctx, `select jsonb_array_length(approval -> 'documents') from asks where id = $1::uuid`, ask).Scan(&documents); err != nil || documents != 2 {
		t.Fatalf("the request %s names %d documents (%v), want the spec and the Verification document", ask, documents, err)
	}

	// The store revision the request was taken at, beside the digest.
	pinned, stated, err := graphstore.CurrentValue(ctx, rig.database.Pool, graphstore.KindAsk, ask, graphcensus.AttributeRequestReadSeq)
	if err != nil || !stated {
		t.Fatalf("read the %s of the request %s (%v): the request pins no store revision", graphcensus.AttributeRequestReadSeq, ask, err)
	}
	seq, err := strconv.ParseInt(pinned, 10, 64)
	now, nowErr := refs.HeadCut(ctx, rig.database.Pool)
	if err != nil || nowErr != nil || seq < cut.ReadSeq || seq > now.ReadSeq {
		t.Errorf("the request %s pins the store revision %q (%v, %v), want a graph_tuples seq between %d, the head before the request, and %d, the head after it", ask, pinned, err, nowErr, cut.ReadSeq, now.ReadSeq)
	}

	// The approval ask's read answers the first document's artifact id and version.
	read := decodeBody[struct {
		Ask struct {
			Approval struct {
				ArtifactID       string `json:"artifact_id"`
				Version          int    `json:"version"`
				RequestedVersion int    `json:"requested_version"`
			} `json:"approval"`
		} `json:"ask"`
	}](t, dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/asks/"+ask, nil, "alice"))
	if got := read.Ask.Approval; got.ArtifactID != spec.id || got.Version != 1 || got.RequestedVersion != 1 {
		t.Errorf("the read of the request %s answers %+v, want artifact_id %s at version 1, requested at 1", ask, got, spec.id)
	}

	// A version of the spec moves the request to it, and the tuples move with the row.
	editSpec(t, rig.handler, issue.Key, []map[string]any{{"op": "replace", "find": "The list comes first.", "with": "The list comes first, always."}})
	requireApprovesStandForRow(t, rig, ask, refKeys, "after a version of the spec moved the request")
	var moved int
	if err := rig.database.Pool.QueryRow(ctx, `select (approval -> 'documents' -> 0 ->> 'version')::int from asks where id = $1::uuid`, ask).Scan(&moved); err != nil || moved != 2 {
		t.Errorf("the request %s names version %d of the spec (%v) after its edit, want version 2", ask, moved, err)
	}
}

// digestStepRowAndColumns pins what the tuples leave as they were (R14): an approval row inserted in
// the shape the release before writes, with the four top-level fields and no documents array, is
// admitted by asks_approval_kind_check, whose normalizing trigger writes the array; and
// artifact_reviews keeps content_sha256 as text under the hex check, with the insert trigger that
// pins it and the update trigger that refuses to move it.
func digestStepRowAndColumns(t *testing.T, rig *digestRig) {
	ctx := context.Background()
	issue := createDigestIssue(t, rig.handler, rig.database, "CORE", "A row the release before wrote", ":::requirement{#r1}\nThe list comes first.\n:::\n", false)
	spec := readDecisionDocument(t, rig.database, issue.Key, "spec")
	var ask string
	if err := rig.database.Pool.QueryRow(ctx, `
		insert into asks (issue_key, author, question, options, urgency, kind, approval)
		values ($1, '{"kind":"session","id":"asker"}', 'Approve spec.md (version 1)?',
		        '[{"label":"Approve"},{"label":"Request changes"}]', 'high', 'approval',
		        jsonb_build_object('artifact_id', $2::text, 'name', 'spec.md', 'version', 1, 'requested_version', 1))
		returning id::text`, issue.Key, spec.id).Scan(&ask); err != nil {
		t.Fatalf("insert an approval row in the shape the release before writes: %v", err)
	}
	// The release before appended the row's ask.opened event with it, as every release does; a
	// row no event opened is no state any release leaves, and every read of the ask after this one -
	// the approval's review below, an inbox read of a later step - fails on it (applyOpenedEventIDs).
	seedAskOpenedEvent(t, rig.database, issue.Key, ask)
	var documents int
	var first string
	if err := rig.database.Pool.QueryRow(ctx, `
		select jsonb_array_length(approval -> 'documents'), approval -> 'documents' -> 0 ->> 'artifact_id' from asks where id = $1::uuid`, ask).Scan(&documents, &first); err != nil ||
		documents != 1 || first != spec.id {
		t.Errorf("the row the release before writes holds %d documents, the first %q (%v), want the one document %s asks_normalize_approval_documents writes", documents, first, err, spec.id)
	}

	catalog := func(what, statement string, args ...any) string {
		t.Helper()
		var read string
		if err := rig.database.Pool.QueryRow(ctx, statement, args...).Scan(&read); err != nil {
			t.Fatalf("read %s out of the catalog: %v", what, err)
		}
		return read
	}
	check := catalog("asks_approval_kind_check", `select pg_get_constraintdef(oid) from pg_constraint where conrelid = 'asks'::regclass and conname = 'asks_approval_kind_check'`)
	for _, member := range []string{"documents", "artifact_id", "requested_version"} {
		if !strings.Contains(check, member) {
			t.Errorf("asks_approval_kind_check reads %s, want it to hold %s of every document as the release before left it", check, member)
		}
	}
	trigger := catalog("asks_normalize_approval_documents", `select pg_get_triggerdef(oid) from pg_trigger where tgrelid = 'asks'::regclass and tgname = 'asks_normalize_approval_documents' and not tgisinternal`)
	if !strings.Contains(strings.ToUpper(trigger), "BEFORE INSERT OR UPDATE OF APPROVAL ON") {
		t.Errorf("asks_normalize_approval_documents reads %s, want the trigger before an insert or an update of approval", trigger)
	}
	if column := catalog("artifact_reviews.content_sha256", `select data_type from information_schema.columns where table_name = 'artifact_reviews' and column_name = 'content_sha256'`); column != "text" {
		t.Errorf("artifact_reviews.content_sha256 is %s, want text", column)
	}
	if hexCheck := catalog("the hex check of content_sha256", `select pg_get_constraintdef(oid) from pg_constraint where conrelid = 'artifact_reviews'::regclass and conname = 'artifact_reviews_content_sha256_hex'`); !strings.Contains(hexCheck, "^[0-9a-f]{64}$") {
		t.Errorf("artifact_reviews_content_sha256_hex reads %s, want the check ^[0-9a-f]{64}$", hexCheck)
	}
	for _, name := range []string{"artifact_reviews_content_sha256_insert", "artifact_reviews_content_sha256_update"} {
		if found := catalog(name, `select count(*)::text from pg_trigger where tgrelid = 'artifact_reviews'::regclass and tgname = $1 and not tgisinternal`, name); found != "1" {
			t.Errorf("artifact_reviews holds %s triggers named %s, want the one the release before left", found, name)
		}
	}
	// The update trigger refuses any statement that moves a pinned digest.
	review := reviewDocument(t, rig.handler, spec.id, "alice", "approved", "")
	if _, err := rig.database.Pool.Exec(ctx, `update artifact_reviews set content_sha256 = $2 where id = $1::uuid`, review.ID, strings.Repeat("0", 64)); err == nil {
		t.Errorf("an update of content_sha256 of the review %s is taken, want the update trigger to refuse it", review.ID)
	}
}

// awaitDigestsFilled waits until no version of the document artifactID owes its record digest
// (version_renders.digest_owed): the fill runs right after the write's COMMIT, and a gate reads the
// digest it records.
func awaitDigestsFilled(t *testing.T, database *store.Store, artifactID string) {
	t.Helper()
	testwait.UntilPolling(t, 20*time.Millisecond, "the versions of "+artifactID+" to owe no record digest", func() (bool, bool) {
		var owed bool
		if err := database.Pool.QueryRow(context.Background(),
			`select coalesce(bool_or(digest_owed), false) from version_renders where artifact_id = $1::uuid`, artifactID).Scan(&owed); err != nil {
			t.Fatalf("read the owed digests of %s: %v", artifactID, err)
		}
		return owed, !owed
	})
}

// digestStepRecordUnmoved pins the rule for a version whose record did not move (R14): "A
// paragraph moved before a requirement leaves both standing, although the version's bytes
// changed", "a historical review authorizes no later version whose digest differs", and a version
// whose record digest equals the approved one's is carried as the result carry is. A spec holds a
// criterion k1, a paragraph and a requirement r1 under k1, in that text order, its spec query gated
// and approved. Moving the requirement before the paragraph through the edits route gives the
// requirement a new text_rank: the new version's digest moves, no carried review is written, and
// the approval reads stale on every gate. Approved again, moving the paragraph before the
// requirement gives the paragraph, which the spec query does not take, the new text_rank: the new
// version's digest is the approved version's, and the write carries alice's approval to it
// (docs.RecordCarryReason), its artifact.approved before the version's artifact.version, so every
// gate reads approved at a version equal to latest_version, which the Legion daemon reads.
func digestStepRecordUnmoved(t *testing.T, rig *digestRig) {
	issue := createDigestIssue(t, rig.handler, rig.database, "CORE", "A spec whose paragraph moves",
		":::criterion{#k1}\nI want the urgent list.\n:::\n\nA paragraph between.\n", false)
	spec := readDecisionDocument(t, rig.database, issue.Key, "spec")
	editSpec(t, rig.handler, issue.Key, []map[string]any{{"op": "insert", "after": "end",
		"markdown": ":::requirement{#r1 parent=\"dispatch://" + issue.Key + "/spec#b-k1\"}\nAn urgent ask comes first.\n:::\n"}})
	awaitDigestsFilled(t, rig.database, spec.id)
	publishDesignGates(t, rig.handler, issue.Key, []issueDesignGate{{Step: model.GatedStepSpec, ArtifactID: spec.id}})
	gate := model.AskApprovalDocument{ArtifactID: spec.id}
	paragraph := blockIDOfText(t, rig.handler, spec.id, "A paragraph between.")

	// The requirement moved before the paragraph: its text_rank moves, and so does the digest.
	approveStanding(t, rig, issue.Key, model.GatedStepSpec, spec.id, "the requirement's move")
	approved := newestVersionOf(t, rig.database, spec.id)
	editThroughTheArtifactRoute(t, rig.handler, spec.id, []map[string]any{{"op": "move", "block": "r1", "after": "block:k1"}})
	awaitDigestsFilled(t, rig.database, spec.id)
	if moved := newestVersionOf(t, rig.database, spec.id); moved != approved+1 || versionDigestOf(t, rig.database, spec.id, moved) == versionDigestOf(t, rig.database, spec.id, approved) {
		t.Errorf("the move of r1 before the paragraph wrote version %d holding the digest %s, want version %d whose digest moved from %s",
			moved, versionDigestOf(t, rig.database, spec.id, moved), approved+1, versionDigestOf(t, rig.database, spec.id, approved))
	}
	requireGate(t, rig, issue.Key, model.GatedStepSpec, gate, false, "the requirement moved before the paragraph")
	if reviews := listReviews(t, rig.handler, spec.id); len(reviews) == 0 || reviews[0].Version != approved || reviews[0].CarriedFromVersion != nil {
		t.Errorf("the move of r1 left the reviews of %s %+v, want alice's own approval of version %d newest: no carry crosses a moved record", spec.refKey, reviews, approved)
	}

	// The paragraph moved before the requirement: the paragraph takes the new text_rank, the spec
	// query's record does not move, and the write carries the approval to its version.
	approveStanding(t, rig, issue.Key, model.GatedStepSpec, spec.id, "the paragraph's move")
	approved = newestVersionOf(t, rig.database, spec.id)
	reviews := len(listReviews(t, rig.handler, spec.id))
	editThroughTheArtifactRoute(t, rig.handler, spec.id, []map[string]any{{"op": "move", "block": paragraph, "after": "block:k1"}})
	awaitDigestsFilled(t, rig.database, spec.id)
	moved := newestVersionOf(t, rig.database, spec.id)
	if moved != approved+1 {
		t.Fatalf("the move of the paragraph before r1 left version %d of %s newest, want %d", moved, spec.refKey, approved+1)
	}
	if after := listReviews(t, rig.handler, spec.id); len(after) != reviews+1 {
		t.Errorf("the move of the paragraph left %d reviews of %s, want the %d before it and the record carry", len(after), spec.refKey, reviews)
	}
	requireApprovalCarriedByRecord(t, rig.handler, rig.database, spec.id, approved, moved)
	requireGate(t, rig, issue.Key, model.GatedStepSpec, gate, true, "the paragraph moved before the requirement")
	if got := readApproval(t, rig.handler, spec.id); got.Approval == nil || got.Approval.Version == nil || *got.Approval.Version != moved || got.Approval.LatestVersion != moved {
		t.Errorf("the approval of %s reads %+v, want approved at version %d, its latest version", spec.refKey, got.Approval, moved)
	}
}

// versionCut is the cut the version_renders row of version number of artifactID records, and that
// row's record digest, failing where the row holds no time, no seq or no digest.
func versionCut(t *testing.T, database *store.Store, artifactID string, number int) (refs.TreeCut, string) {
	t.Helper()
	row := versionFillRowOf(t, database, artifactID, number)
	if row.at == nil || row.readSeq == nil || row.digest == nil {
		t.Fatalf("the version_renders row of version %d of %s holds at %v, read_seq %v and digest %v, want all three", number, artifactID, row.at, row.readSeq, row.digest)
	}
	return refs.TreeCut{At: *row.at, ReadSeq: *row.readSeq}, *row.digest
}

// newestVersionOf is the newest version number of the document artifactID.
func newestVersionOf(t *testing.T, database *store.Store, artifactID string) int {
	t.Helper()
	var number int
	if err := database.Pool.QueryRow(context.Background(), `select max(number) from artifact_versions where artifact_id = $1::uuid`, artifactID).Scan(&number); err != nil {
		t.Fatalf("read the newest version of %s: %v", artifactID, err)
	}
	return number
}

// digestStepApprovedRecord pins c-approval-bytes' opening (R14): an issue's spec query is approved,
// the review pins content_sha256, the digest artifact_version_text_sha256 takes of that version's
// record, and the test builds the record again at the cut the version's row records and gets that
// digest. It is a review taken after the cutover, its version's pass and its digest fill, over a
// query left untouched, so the head's record digests to it too.
func digestStepApprovedRecord(t *testing.T, rig *digestRig) {
	issue := createDigestIssue(t, rig.handler, rig.database, "CORE", "An approved record", ":::requirement{#r1}\nThe approved list comes first.\n:::\n", false)
	spec := readDecisionDocument(t, rig.database, issue.Key, "spec")
	awaitDigestsFilled(t, rig.database, spec.id)
	review := reviewDocument(t, rig.handler, spec.id, "alice", "approved", "")
	cut, stated := versionCut(t, rig.database, spec.id, review.Version)
	record := rig.recordAt(t, issue.Key, graphstore.QuerySpec, cut)
	if review.ContentSHA256 == nil || record.Digest != *review.ContentSHA256 || stated != record.Digest {
		t.Errorf("the spec record of %s at version %d, built again at its cut %+v, digests to %s and its row records %s, want both the review's content_sha256 %v",
			issue.Key, review.Version, cut, record.Digest, stated, review.ContentSHA256)
	}
	if head := rig.record(t, issue.Key, graphstore.QuerySpec); review.ContentSHA256 == nil || head.Digest != *review.ContentSHA256 {
		t.Errorf("the spec record of %s, untouched since the approval of version %d, digests to %s, want the review's %v", issue.Key, review.Version, head.Digest, review.ContentSHA256)
	}
}

// digestStepResultOrder is control seven's step (R14): two results of one check written straight
// into the store, the second made a second later than the first and at an address that sorts
// before the first's, stand in the verification record first-made first, because the walk takes
// the children that hold no rank by the time each was made and then by address.
func digestStepResultOrder(t *testing.T, rig *digestRig) {
	ctx := context.Background()
	issue, verification := digestVerifiedIssue(t, rig, "Two results of one check")
	const first, second = "ffffffff-0000-4000-8000-0000000000f1", "00000000-0000-4000-8000-0000000000f2"
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	if err := pgx.BeginFunc(ctx, rig.database.Pool, func(tx pgx.Tx) error {
		for index, id := range []string{first, second} {
			nodes, tuples, ok := graphstore.ResultFacts(graphstore.Result{
				ID: id, Project: "CORE", Check: verification.address("c1"), Verdict: "pass", Surface: "cli", Commit: headCommit,
				Role: "implementer", Recorder: digestWriter, At: at.Add(time.Duration(index) * time.Second), Event: graphstore.NoEvent,
			})
			if !ok {
				return fmt.Errorf("the result %s states nothing", id)
			}
			if err := graphstore.Write(ctx, tx, nodes, tuples); err != nil {
				return fmt.Errorf("write the result %s: %w", id, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("write the two results: %v", err)
	}
	record := rig.record(t, issue.Key, graphstore.QueryVerification)
	made, later := graphstore.KindResult+":"+first, graphstore.KindResult+":"+second
	a, b := slices.Index(walkAddresses(record.Nodes), made), slices.Index(walkAddresses(record.Nodes), later)
	if a < 0 || b < 0 {
		t.Fatalf("the verification record of %s takes %v, want both results %s and %s", issue.Key, walkAddresses(record.Nodes), made, later)
	}
	if b < a {
		t.Errorf("the verification record holds result %s before result %s, which was made first", later, made)
	}
}

// digestStepRetype is control five's step (R14, R26): a requirement retyped into a check through a
// version of the spec restates the block's node with the supertag check, and the walk at the cut of
// the version before the retype still types that block requirement, as its text at that cut opens;
// the spec record at that version holds the block and digests to the digest taken at that version.
func digestStepRetype(t *testing.T, rig *digestRig) {
	ctx := context.Background()
	issue := createDigestIssue(t, rig.handler, rig.database, "CORE", "A requirement retyped", ":::requirement{#rt}\nThe retyped words.\n:::\n", false)
	spec := readDecisionDocument(t, rig.database, issue.Key, "spec")
	awaitDigestsFilled(t, rig.database, spec.id)
	before := newestVersionOf(t, rig.database, spec.id)
	cut, digest := versionCut(t, rig.database, spec.id, before)
	editThroughTheArtifactRoute(t, rig.handler, spec.id, []map[string]any{{"op": "delete", "block": "rt"},
		{"op": "insert", "after": "end", "markdown": ":::check{#rt surface=\"cli\" run=\"go test ./...\"}\nThe retyped words.\n:::\n"}})
	awaitDigestsFilled(t, rig.database, spec.id)
	address := blockAddress(spec.address("rt"))
	var supertag string
	if err := rig.database.Pool.QueryRow(ctx, `select supertag from graph_nodes where kind = $1 and id = $2`, refs.BlockNodeKind, spec.address("rt")).Scan(&supertag); err != nil || supertag != graphstore.BlockTypeCheck {
		t.Fatalf("after the retype %s holds the supertag %q (%v), want check: no version restated it", address, supertag, err)
	}
	walked, err := docs.WalkIssueTree(ctx, rig.database.Pool, issue.Key, cut)
	if err != nil {
		t.Fatalf("walk %s at the cut of version %d: %v", issue.Key, before, err)
	}
	if at := walkIndex(walked, address); at < 0 {
		t.Errorf("the spec record at version %d holds no block %s", before, address)
	} else if typ := walked.Nodes[at].Type; typ != "requirement" {
		t.Errorf("the walk at version %d types block %s as %s, want requirement", before, address, typ)
	}
	record := rig.recordAt(t, issue.Key, graphstore.QuerySpec, cut)
	if !slices.Contains(walkAddresses(record.Nodes), address) {
		t.Errorf("the spec record at version %d holds no block %s", before, address)
	}
	if record.Digest != digest {
		t.Errorf("the spec record at version %d digests to %s, want %s", before, record.Digest, digest)
	}
}

// digestMigrationName is the name every migration of the issue-tree set takes, as the store's own
// test of the set finds it (store/issue_tree_migrations_test.go's issueTreeMigrationName).
var digestMigrationName = regexp.MustCompile(`^[0-9]{14}_issue_tree_[a-z0-9_]+\.up\.sql$`)

// digestStepMigrations pins the column's last sentence (R14): "The migration set runs no statement
// against that table." It reads every migration of the issue-tree set from the store's migration
// directory and fails on any statement, its comments taken out, that names artifact_reviews.
func digestStepMigrations(t *testing.T, _ *digestRig) {
	directory := filepath.Join("..", "store", "migrations")
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read the migrations under %s: %v", directory, err)
	}
	comment := regexp.MustCompile(`--[^\n]*`)
	names := regexp.MustCompile(`(?i)\bartifact_reviews\b`)
	read := 0
	for _, entry := range entries {
		if !digestMigrationName.MatchString(entry.Name()) {
			continue
		}
		body, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatalf("read the migration %s: %v", entry.Name(), err)
		}
		read++
		for _, statement := range strings.Split(comment.ReplaceAllString(string(body), ""), ";") {
			if names.MatchString(statement) {
				t.Errorf("the migration %s runs %q against artifact_reviews: the migration set runs no statement against that table",
					entry.Name(), strings.TrimSpace(statement))
			}
		}
	}
	if read == 0 {
		t.Fatalf("no migration under %s is named as the issue-tree set's are (%s)", directory, digestMigrationName)
	}
}

// digestStepDefinitionNodes holds controls eight and ten's steps (R14). A spec approved through an
// approval request: the query version node the review names by its artifact_id and version holds
// definition_version 1, and the request the review's ask_id names states an approves tuple to that
// same node. Then a version written by SQL, in the shape the release before stores, of a document
// whose node is no query node and that holds no version node; its node is restated as a query node
// and graphcensus.Backfill runs: the version node the backfill writes is a query version holding no
// definition_version.
func digestStepDefinitionNodes(t *testing.T, rig *digestRig) {
	ctx := context.Background()
	issue := createDigestIssue(t, rig.handler, rig.database, "CORE", "A requested spec", ":::requirement{#r1}\nThe requested list comes first.\n:::\n", false)
	spec := readDecisionDocument(t, rig.database, issue.Key, "spec")
	requested := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/artifacts/"+spec.id+"/approval-requests", map[string]any{}, "alice")
	if requested.Code != http.StatusCreated {
		t.Fatalf("request the approval of the spec of %s: status=%d body=%s", issue.Key, requested.Code, requested.Body.String())
	}
	ask := decodeBody[struct {
		Ask struct {
			ID string `json:"id"`
		} `json:"ask"`
	}](t, requested).Ask.ID
	answered := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/asks/"+ask+"/answer",
		map[string]any{"selected": []string{docs.ApprovalOptionApprove}, "expected_edited_at": nil}, "alice")
	if answered.Code != http.StatusOK {
		t.Fatalf("approve the request %s: status=%d body=%s", ask, answered.Code, answered.Body.String())
	}
	reviews := listReviews(t, rig.handler, spec.id)
	if len(reviews) == 0 || reviews[0].AskID == nil || *reviews[0].AskID != ask {
		t.Fatalf("the reviews of %s read %+v, want the approval that answers the request %s", spec.refKey, reviews, ask)
	}
	node := graphstore.VersionID(spec.refKey, reviews[0].Version)
	if supertag, held := versionDefinition(t, rig.database, spec.refKey, reviews[0].Version); supertag != graphstore.SupertagQueryVersion || valueOrNone(held) != "1" {
		t.Errorf("the query version %s that the review of %s names holds definition_version %s, want 1", node, ask, valueOrNone(held))
	}
	approves, err := graphstore.CurrentOut(ctx, rig.database.Pool, graphstore.KindAsk, ask, []string{graphcensus.TupleApproves})
	if err != nil {
		t.Fatalf("read the approves tuples of the request %s: %v", ask, err)
	}
	if !slices.ContainsFunc(approves, func(tuple graphstore.Tuple) bool { return tuple.ToKind == graphstore.KindVersion && tuple.ToID == node }) {
		t.Errorf("the request %s states approves tuples %+v, want one to the query version %s its review names", ask, approves, node)
	}

	// The tenth control's version: written by SQL before its document was a query node.
	notes := uploadDocument(t, rig.handler, "/api/v1/issues/"+issue.Key, "notes.md", "# Notes\n\nA document no query covers.\n")
	var refKey string
	if err := rig.database.Pool.QueryRow(ctx, `select ref_key from artifacts where id = $1::uuid`, notes.Artifact.ID).Scan(&refKey); err != nil {
		t.Fatalf("read the ref_key of notes.md: %v", err)
	}
	written := newestVersionOf(t, rig.database, notes.Artifact.ID) + 1
	if _, err := rig.database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1::uuid, $2, '# Notes\n\nA version the release before wrote.\n', '[{"kind":"user","id":"alice"}]')`, notes.Artifact.ID, written); err != nil {
		t.Fatalf("write version %d of notes.md as the release before writes one: %v", written, err)
	}
	var noded bool
	if err := rig.database.Pool.QueryRow(ctx, `select exists (select 1 from graph_nodes where kind = $1 and id = $2)`,
		graphstore.KindVersion, graphstore.VersionID(refKey, written)).Scan(&noded); err != nil || noded {
		t.Fatalf("version %d of %s, written by SQL, holds a version node already (%t, %v), want none", written, refKey, noded, err)
	}
	restated := graphstore.QueryNode(refKey, "CORE", graphstore.QueryPlan, digestWriter, time.Now().UTC())
	rig.write(t, []graphstore.Node{restated}, graphstore.AttributeFacts(nil, restated))
	if _, err := graphcensus.Backfill(ctx, rig.database, io.Discard); err != nil {
		t.Fatalf("run the backfill: %v", err)
	}
	if supertag, held := versionDefinition(t, rig.database, refKey, written); supertag != graphstore.SupertagQueryVersion || held != nil {
		t.Errorf("the query version %s, which the backfill wrote for a version written before its document was a query, holds definition_version %s, want none (a %s node)",
			graphstore.VersionID(refKey, written), valueOrNone(held), supertag)
	}
}
