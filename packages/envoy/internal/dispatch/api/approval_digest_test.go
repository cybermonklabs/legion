package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
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
	handler  http.Handler
	database *store.Store
	issue    model.Issue
	spec     decisionDocument
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
	sum := sha256.Sum256(record.Record)
	if record.Digest != hex.EncodeToString(sum[:]) {
		t.Fatalf("the %s record of %s answers the digest %s, want the sha256 of its bytes, %x", query, key, record.Digest, sum)
	}
	return record
}

// block is the store address of block id of the rig's spec, which the issue's room keys.
func (r *digestRig) block(id string) string { return r.spec.address(id) }

// state appends tuples straight into the store, as a write that states a relation or a value and
// no word of any node would.
func (r *digestRig) state(t *testing.T, tuples ...graphstore.Tuple) {
	t.Helper()
	ctx := context.Background()
	tx, err := r.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := graphstore.Write(ctx, tx, nil, tuples); err != nil {
		t.Fatalf("write %+v: %v", tuples, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit %+v: %v", tuples, err)
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
	ctx := context.Background()
	var held string
	if unconverted {
		if err := database.Pool.QueryRow(ctx, `
			insert into server_processes (instance, signing_keys, schema_version, schema_creatable, heartbeat_at)
			values (gen_random_uuid(), '{fingerprint}', $1, $2, now() + interval '1 day') returning instance::text`,
			pmdoc.SchemaVersion(), pmdoc.SchemaCreatable()).Scan(&held); err != nil {
			t.Fatalf("add a live server process that lists no issue room: %v", err)
		}
	}
	issue, tree := createRoomIssue(t, handler, database, project, title, spec, parent)
	if unconverted {
		if _, err := database.Pool.Exec(ctx, `delete from server_processes where instance = $1::uuid`, held); err != nil {
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
// whatever their ids; a requirement under a comment's block of its own issue stands in the walk's
// first part after its comment; a criterion of a child under its parent's comment stands in the
// child's spec record after the walk from the child's tree node, and a write of it moves the
// child's digest and leaves the parent's; and a document comment anchored inside a decision's
// paragraph stands under the decision, on a converted issue, through a moved address, and on one
// that has not converted. The test refuses to run where select 'a' < 'B' is false.
func TestApprovalCoversTheDigestOfOneQuerysRecord(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	ctx := context.Background()
	var collated bool
	if err := database.Pool.QueryRow(ctx, `select 'a' < 'B'`).Scan(&collated); err != nil {
		t.Fatalf("read the database's collation of a and B: %v", err)
	}
	if !collated {
		t.Fatal("this database orders 'B' before 'a' by its own collation, so it cannot tell a record that orders ranks byte for byte from one that forgot to: run this test where select 'a' < 'B' is true")
	}
	createReferenceAPIProject(t, handler, "CORE")
	rig := &digestRig{handler: handler, database: database}
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
		{"a comment anchored inside a decision's paragraph stands under the decision", digestStepAnchoredComment},
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
	if supertag, held := versionDefinition(t, rig.database, spec.refKey, 1); supertag != graphstore.SupertagQueryVersion || definitionHeld(held) != want {
		t.Errorf("the version 1 of %s, a spec created converted, is a %s holding definition_version %s, want a query version holding %s",
			spec.refKey, supertag, definitionHeld(held), want)
	}
	review := reviewDocument(t, rig.handler, spec.id, "alice", "approved", "")
	if review.ArtifactID != spec.id || review.Version != 1 {
		t.Errorf("the review %s names %s at version %d, want version 1 of %s (%s)", review.ID, review.ArtifactID, review.Version, spec.refKey, spec.id)
	}
}

func digestStepRelations(t *testing.T, rig *digestRig) {
	c1, c2, r1, r2 := rig.block("c1"), rig.block("c2"), rig.block("r1"), rig.block("r2")
	before := rig.record(t, rig.issue.Key, graphstore.QueryVerification)
	spec := rig.record(t, rig.issue.Key, graphstore.QuerySpec)

	// The retarget: c2's serves leaves r1 for r2, and no word, value, node or place moves.
	rig.state(t, blockTuple(c2, "serves", refs.BlockNodeKind, r2, true), blockTuple(c2, "serves", refs.BlockNodeKind, r1, false))
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

	// The move: c2's part_of leaves r1 for r2, every word unchanged.
	rig.state(t, blockTuple(c2, graphstore.TuplePartOf, refs.BlockNodeKind, r2, true), blockTuple(c2, graphstore.TuplePartOf, refs.BlockNodeKind, r1, false))
	moved := rig.record(t, rig.issue.Key, graphstore.QueryVerification)
	if moved.Digest == retargeted.Digest {
		t.Errorf("the move of c2 under %s leaves the verification digest at %s", r2, moved.Digest)
	}
	if index := slices.IndexFunc(moved.Nodes, func(node docs.TreeNode) bool { return node.ID == c2 }); index < 0 || moved.Nodes[index].Parent.ID != r2 {
		t.Errorf("after its move c2 stands in the verification record as %+v, want it under %s", moved.Nodes, r2)
	}
	requireWalkOrder(t, rig.walk(t, rig.issue.Key))

	// A result: a node under c1, in the verification record, moving its digest.
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

	// The removal: c2 goes, and the digest moves.
	uploadDocument(t, rig.handler, "/api/v1/issues/"+rig.issue.Key, "verification.md", digestChecks("The list reads the urgent asks first.", ""))
	removed := rig.record(t, rig.issue.Key, graphstore.QueryVerification)
	if removed.Digest == resulted.Digest {
		t.Errorf("the removal of c2 leaves the verification digest at %s", removed.Digest)
	}
	if slices.Contains(walkAddresses(removed.Nodes), blockAddress(c2)) {
		t.Errorf("the verification record still takes %s after its removal: %v", c2, walkAddresses(removed.Nodes))
	}
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
			at := slices.IndexFunc(before.Nodes, func(node docs.TreeNode) bool { return node.ID == check })
			if at < 0 {
				t.Fatalf("the verification record takes no %s: %v", check, walkAddresses(before.Nodes))
			}
			rig.state(t, blockValue(check, change.attribute, to))
			after := rig.record(t, rig.issue.Key, graphstore.QueryVerification)
			if after.Digest == before.Digest {
				t.Errorf("the verification digest held still across a change of %s of %s", change.attribute, blockAddress(check))
			}
			if !slices.Equal(walkAddresses(after.Nodes), walkAddresses(before.Nodes)) || after.Nodes[at].Parent != before.Nodes[at].Parent {
				t.Errorf("the change of %s of %s moves the verification record's nodes from %+v to %+v, want each where it stood",
					change.attribute, check, before.Nodes, after.Nodes)
			}
		})
	}
}

// editThroughTheArtifactRoute applies ops to the document artifactID through
// POST /api/v1/artifacts/{id}/edits as alice, as a named version, and fails unless the route takes
// them.
func editThroughTheArtifactRoute(t *testing.T, handler http.Handler, artifactID string, ops []map[string]any) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{
		"ops": ops, "summary": "edit",
	}, "alice")
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
	for index, node := range tree.Nodes {
		if node.Ranked && node.Parent.Kind+":"+node.Parent.ID == issue && index > first {
			t.Errorf("the walk takes the issue's ranked child %s at %d, after its unranked child %s at %d", node.Address(), index, earlier, first)
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
		var paragraph string
		for text, id := range blockIDsByText(t, rig.handler, issue.PrimaryArtifactID) {
			if strings.TrimSpace(text) == words {
				paragraph = id
			}
		}
		if paragraph == "" {
			t.Fatalf("the spec of %s lists no block of the decision's paragraph", issue.Key)
		}
		artifact := "spec"
		if !unconverted {
			artifact = issue.PrimaryArtifactID
		}
		comment := createBlockAnchoredComment(t, rig.handler, issue.Key, map[string]any{"artifact": artifact, "quote": "urgent list", "block_id": paragraph})
		tree := rig.walk(t, issue.Key)
		requireWalkOrder(t, tree)
		decision := blockAddress(spec.address("d1"))
		requireUnder(t, "a comment anchored inside the decision's paragraph (converted "+strconv.FormatBool(!unconverted)+")",
			tree, graphstore.KindComment+":"+comment.ID, decision)
		if inside := walkIndex(tree, blockAddress(spec.address(paragraph))); inside < 0 || inside > walkIndex(tree, graphstore.KindComment+":"+comment.ID) {
			t.Errorf("the walk takes the decision's paragraph at %d, want it before the comment, a ranked child before an unranked one", inside)
		}
		if unconverted {
			continue
		}
		// A comment whose anchored_in names an old address of the paragraph, which a moved_to
		// carries to the paragraph's node in the room, stands under the decision too.
		moved := createBlockAnchoredComment(t, rig.handler, issue.Key, map[string]any{"artifact": artifact, "quote": "urgent list", "block_id": paragraph})
		old := issue.Key + "/spec-before#" + paragraph
		rig.state(t,
			graphstore.AnchoredIn(moved.ID, spec.address(paragraph), false, digestWriter, time.Now().UTC(), graphstore.NoEvent),
			graphstore.AnchoredIn(moved.ID, old, true, digestWriter, time.Now().UTC(), graphstore.NoEvent),
			blockTuple(old, graphstore.TupleMovedTo, refs.BlockNodeKind, spec.address(paragraph), true))
		requireUnder(t, "a comment anchored at an old address of the decision's paragraph", rig.walk(t, issue.Key),
			graphstore.KindComment+":"+moved.ID, decision)
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
