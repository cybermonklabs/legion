package api

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// --- A version of any size writes its requirements, checks and plan tasks at once ---

// acceptedBlock is one typed block a test writes: its block id, its type, the requirements of its
// issue's spec it serves, the block of the spec its parent attribute names, whether it stands in an
// item of a list and the heading above that list, and, for a decision, the reading it closes with.
type acceptedBlock struct {
	id, typ string
	serves  []string
	parent  string
	listed  bool
	heading string
	reading string
}

func acceptedRequirement(id string) acceptedBlock { return acceptedBlock{id: id, typ: "requirement"} }

func acceptedCheck(id string, serves ...string) acceptedBlock {
	return acceptedBlock{id: id, typ: "check", serves: serves}
}

func acceptedPlanTask(id string, serves ...string) acceptedBlock {
	return acceptedBlock{id: id, typ: "plan_task", serves: serves}
}

// typedBlockMarkdown is block as its document writes it, its serves and its parent naming blocks
// of the spec of issueKey; a listed block stands in the second paragraph of a list's one item.
func typedBlockMarkdown(issueKey string, block acceptedBlock) string {
	var line strings.Builder
	line.WriteString(":::" + block.typ + "{#" + block.id)
	if len(block.serves) > 0 {
		targets := make([]string, len(block.serves))
		for index, requirement := range block.serves {
			targets[index] = "dispatch://" + issueKey + "/spec#b-" + requirement
		}
		line.WriteString(` serves="` + strings.Join(targets, " ") + `"`)
	}
	if block.parent != "" {
		line.WriteString(` parent="dispatch://` + issueKey + `/spec#b-` + block.parent + `"`)
	}
	if block.typ == "check" {
		line.WriteString(` surface="api" run="go test ./x"`)
	}
	fence := ":::"
	if block.reading != "" {
		fence = "::::"
	}
	written := strings.Replace(line.String(), ":::", fence, 1) + "}\nThe " + strings.ReplaceAll(block.typ, "_", " ") + " " + block.id + ".\n"
	if block.reading != "" {
		written += "\n:::reading{#" + block.reading + "}\nThe agent reads " + block.id + ".\n:::\n"
	}
	written += fence + "\n"
	if !block.listed {
		return written
	}
	listed := "- An item holding " + block.id + ".\n\n  " + strings.ReplaceAll(strings.TrimSuffix(written, "\n"), "\n", "\n  ") + "\n"
	if block.heading != "" {
		listed = "## " + block.heading + "\n\n" + listed
	}
	return listed
}

// typedDocument is a document of the issue issueKey holding a paragraph and blocks, after a list of
// more items than a write indexes in its own request where padded.
func typedDocument(issueKey string, padded bool, blocks ...acceptedBlock) string {
	var body strings.Builder
	if padded {
		body.WriteString(deferralList(itemsOverTheBlockBound, "end") + "\n")
	}
	body.WriteString("A paragraph no write indexes beside its typed blocks.\n")
	for _, block := range blocks {
		body.WriteString("\n" + typedBlockMarkdown(issueKey, block))
	}
	return body.String()
}

// blockKeyOf is the ref_key the blocks of document artifactID are keyed under: its own, or its
// issue room's tree artifact's where it is a fragment of one (refs.DocumentRoomSQL).
func blockKeyOf(t *testing.T, database *store.Store, artifactID string) string {
	t.Helper()
	var key string
	if err := database.Pool.QueryRow(context.Background(),
		`select keyed.ref_key from artifacts keyed where keyed.id = `+refs.DocumentRoomSQL("$1::uuid"), artifactID).Scan(&key); err != nil {
		t.Fatalf("read the key the blocks of %s are keyed under: %v", artifactID, err)
	}
	return key
}

// blockNodeCount is how many block nodes the store holds under the ref_key key.
func blockNodeCount(t *testing.T, database *store.Store, key string) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from graph_nodes where kind = $1 and id like $2 || '#%'`, refs.BlockNodeKind, key).Scan(&count); err != nil {
		t.Fatalf("count the block nodes under %s: %v", key, err)
	}
	return count
}

// writtenBlock is one requirement, check or plan task of a document, as a test expects the store to
// hold it: the block, the ref_key its node is keyed under, its document and the version whose write
// wrote its text.
type writtenBlock struct {
	acceptedBlock
	key, document string
	version       int
}

// address is the block's node id.
func (block writtenBlock) address() string { return block.key + "#" + block.id }

// heldTypedBlock is what the store holds of one block: its node's state and supertag, its current
// text value, its document_blocks row's type, text_sha256, changed_version and issue_room_blocks
// document, and how many text values, part_of tuples, rank values, serves tuples, serves rows,
// rows and in_document tuples it holds.
type heldTypedBlock struct {
	state, supertag, text, rowType, rowSum, room                            string
	texts, partOf, ranks, serves, refs, rows, inDocument, changed, textRows int
}

// readHeldTypedBlock reads heldTypedBlock of the block node address.
func readHeldTypedBlock(t *testing.T, database *store.Store, address string) heldTypedBlock {
	t.Helper()
	var held heldTypedBlock
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce(n.state, ''), coalesce(n.supertag, ''),
		       coalesce((select t.to_id from graph_tuples t where t.from_kind = 'block' and t.from_id = $1 and t.kind = 'text'
		                 and t.to_kind = 'value' and t.asserted order by t.seq desc limit 1), ''),
		       coalesce(d.type, ''), coalesce(d.text_sha256, ''), coalesce(r.document_id::text, ''),
		       (select count(*) from graph_tuples t where t.from_kind = 'block' and t.from_id = $1 and t.kind = 'text' and t.to_kind = 'value'),
		       (select count(*) from graph_tuples t where t.from_kind = 'block' and t.from_id = $1 and t.kind = 'part_of'),
		       (select count(*) from graph_tuples t where t.from_kind = 'block' and t.from_id = $1 and t.kind = 'rank' and t.to_kind = 'value'),
		       (select count(*) from graph_tuples t where t.from_kind = 'block' and t.from_id = $1 and t.kind = 'serves'),
		       (select count(*) from block_refs b where b.from_node = $1 and b.kind = 'serves'),
		       (select count(*) from document_blocks c where c.node_id = $1),
		       (select count(*) from graph_tuples t where t.from_kind = 'block' and t.from_id = $1 and t.kind = 'in_document'),
		       coalesce(d.changed_version, 0),
		       (select count(*) from document_block_texts x join document_blocks c on c.artifact_id = x.artifact_id and c.block_id = x.block_id
		        where c.node_id = $1 and x.text is not null)
		from (select 1) one
		left join graph_nodes n on n.kind = 'block' and n.id = $1
		left join document_blocks d on d.node_id = $1
		left join issue_room_blocks r on r.tree_artifact_id = d.artifact_id and r.block_id = d.block_id
	`, address).Scan(&held.state, &held.supertag, &held.text, &held.rowType, &held.rowSum, &held.room,
		&held.texts, &held.partOf, &held.ranks, &held.serves, &held.refs, &held.rows, &held.inDocument, &held.changed, &held.textRows); err != nil {
		t.Fatalf("read the node and the rows of %s: %v", address, err)
	}
	return held
}

// blockRows is what the block index holds of one block: its document_blocks row, each column of
// it, and its document_block_texts rows, each version with the md5 of its text.
type blockRows struct {
	changed                                                                       int
	typ, sum, excerpt, changedAt, createdAt, nodeKind, nodeAddress, parent, texts string
}

func (rows blockRows) String() string {
	return fmt.Sprintf("changed_version %d, document_block_texts at [%s], type %s, text_sha256 %s, excerpt %.40q, changed_at %s, created_at %s, node %s %s, parent_block_id %q",
		rows.changed, rows.texts, rows.typ, rows.sum, rows.excerpt, rows.changedAt, rows.createdAt, rows.nodeKind, rows.nodeAddress, rows.parent)
}

// readBlockRows reads blockRows of the block node address, failing t unless the index holds a
// document_blocks row of it.
func readBlockRows(t *testing.T, database *store.Store, address string) blockRows {
	t.Helper()
	var rows blockRows
	if err := database.Pool.QueryRow(context.Background(), `
		select d.changed_version, d.type, d.text_sha256, d.excerpt, d.changed_at::text, d.created_at::text,
		       coalesce(d.node_kind, ''), coalesce(d.node_address, ''), coalesce(d.parent_block_id, ''),
		       coalesce((select string_agg(x.version || ' ' || coalesce(md5(x.text), 'null'), ', ' order by x.version)
		                 from document_block_texts x where x.artifact_id = d.artifact_id and x.block_id = d.block_id), '')
		from document_blocks d where d.node_id = $1
	`, address).Scan(&rows.changed, &rows.typ, &rows.sum, &rows.excerpt, &rows.changedAt, &rows.createdAt,
		&rows.nodeKind, &rows.nodeAddress, &rows.parent, &rows.texts); err != nil {
		t.Fatalf("read the document_blocks row and the document_block_texts rows of %s: %v", address, err)
	}
	return rows
}

// requireWrittenAtOnce fails t unless the store holds each of blocks as the write that accepted
// its version writes it, with no block-index reconciliation run: its
// node live, one text value opening with its block's line, one part_of, its document_blocks row of
// its type with the sha256 of that text and its version as changed_version, one serves tuple and
// one block_refs row for each requirement it serves, one in_document, for a block of an issue room
// its document in issue_room_blocks, and no rank.
func requireWrittenAtOnce(t *testing.T, database *store.Store, room bool, blocks []writtenBlock) {
	t.Helper()
	for _, block := range blocks {
		held := readHeldTypedBlock(t, database, block.address())
		opening := ":::" + block.typ + "{#" + block.id
		wantRoom := ""
		if room {
			wantRoom = block.document
		}
		switch {
		case held.state != graphstore.StateLive:
			t.Errorf("the %s %s reads node state %q right after its write, want live", block.typ, block.address(), held.state)
		case held.texts != 1 || !strings.HasPrefix(strings.TrimLeft(held.text, " "), opening):
			t.Errorf("the %s %s holds %d text values, the current %.40q, right after its write: want one opening %q", block.typ, block.address(), held.texts, held.text, opening)
		case held.partOf != 1:
			t.Errorf("the %s %s holds %d part_of tuples right after its write, want one", block.typ, block.address(), held.partOf)
		case held.rows != 1 || held.rowType != block.typ || held.rowSum != refs.BlockTextSum(held.text) || held.changed != block.version:
			t.Errorf("the %s %s holds %d document_blocks rows (type %q, text_sha256 %s, changed_version %d) right after its write, want one of type %s, the sha256 of its text value and version %d",
				block.typ, block.address(), held.rows, held.rowType, held.rowSum, held.changed, block.typ, block.version)
		case held.serves != len(block.serves) || held.refs != len(block.serves):
			t.Errorf("the %s %s holds %d serves tuples and %d serves rows right after its write, want %d of each (%v)", block.typ, block.address(), held.serves, held.refs, len(block.serves), block.serves)
		case held.inDocument != 1 || held.textRows != 1:
			t.Errorf("the %s %s holds %d in_document tuples and %d document_block_texts rows right after its write, want one of each", block.typ, block.address(), held.inDocument, held.textRows)
		case held.room != wantRoom:
			t.Errorf("issue_room_blocks names the document %q of the %s %s right after its write, want %q", held.room, block.typ, block.address(), wantRoom)
		case held.ranks != 0:
			t.Errorf("the %s %s holds %d rank values right after its write, want none until the reconciliation ranks it", block.typ, block.address(), held.ranks)
		}
	}
}

// requireIndexedOnce fails t unless, once the block-index reconciliation has run, the store holds
// each of blocks once: its node live, one text value, one document_block_texts row, one part_of,
// one row, one in_document and one serves tuple and row per requirement it serves - the pass the
// reconciliation ran appended none of them again - and the first rank the reconciliation wrote.
func requireIndexedOnce(t *testing.T, database *store.Store, blocks []writtenBlock) {
	t.Helper()
	for _, block := range blocks {
		held := readHeldTypedBlock(t, database, block.address())
		if held.state != graphstore.StateLive || held.texts != 1 || held.textRows != 1 || held.partOf != 1 || held.rows != 1 || held.ranks != 1 ||
			held.inDocument != 1 || held.serves != len(block.serves) || held.refs != len(block.serves) {
			t.Errorf("after the reconciliation the %s %s reads %q with %d text values, %d document_block_texts rows, %d part_of tuples, %d rows, %d ranks, %d in_document tuples, %d serves tuples and %d serves rows: want it live with one of each and %d serves",
				block.typ, block.address(), held.state, held.texts, held.textRows, held.partOf, held.rows, held.ranks, held.inDocument, held.serves, held.refs, len(block.serves))
		}
	}
}

// requireCoverageListsThem fails t unless the coverage read of issueKey lists each requirement of
// blocks and counts, under each requirement, every check and plan task of blocks that serves it,
// naming the first one it does not count.
func requireCoverageListsThem(t *testing.T, handler http.Handler, issueKey string, blocks []writtenBlock) {
	t.Helper()
	coverage := readCoverage(t, handler, issueKey)
	listed := make(map[string]model.RequirementCoverage, len(coverage.Requirements))
	for _, requirement := range coverage.Requirements {
		listed[requirement.BlockID] = requirement
	}
	serving := map[string][]writtenBlock{}
	for _, block := range blocks {
		if block.typ == "requirement" {
			if _, held := listed[block.id]; !held {
				t.Errorf("the coverage read of %s does not list the requirement %s: %+v", issueKey, block.id, coverage.Requirements)
			}
			continue
		}
		for _, requirement := range block.serves {
			serving[requirement] = append(serving[requirement], block)
		}
	}
	for _, requirement := range slices.Sorted(maps.Keys(serving)) {
		var checks, tasks []string
		for _, block := range serving[requirement] {
			if block.typ == "check" {
				checks = append(checks, block.id)
			} else {
				tasks = append(tasks, block.id)
			}
		}
		read := listed[requirement]
		if read.Checks < len(checks) {
			t.Errorf("the coverage read of %s does not list the check %s under %s: it counts %d of the checks %v serving it", issueKey, checks[read.Checks], requirement, read.Checks, checks)
		}
		if read.PlanTasks < len(tasks) {
			t.Errorf("the coverage read of %s does not list the plan task %s under %s: it counts %d of the plan tasks %v serving it", issueKey, tasks[read.PlanTasks], requirement, read.PlanTasks, tasks)
		}
	}
}

// acceptedViews are the views of the tree read each type of block is a node of.
var acceptedViews = map[string]string{"criterion": "?view=spec", "requirement": "?view=spec", "check": "?view=verification", "plan_task": "?view=plan"}

// requireInTheViews fails t unless each of blocks reads in its view of the tree read of issueKey
// (acceptedViews) as a node of its type with, for a block of an issue room, the document member
// naming its document; and in the read with no view as a node holding no rank after each ranked
// sibling, with the serves of each requirement it serves (the spec's blocks keyed under specKey).
func requireInTheViews(t *testing.T, handler http.Handler, issueKey, specKey string, room bool, blocks []writtenBlock) {
	t.Helper()
	reads := map[string]treeAnswer{}
	for _, block := range blocks {
		query := acceptedViews[block.typ]
		read, held := reads[query]
		if !held {
			read = treeRead(t, handler, issueKey, query, "alice", false)
			reads[query] = read
		}
		node, found := blockNodeOf(read, block.id)
		switch {
		case !found:
			t.Errorf("%s of %s answers no node of the %s %s", query, issueKey, block.typ, block.id)
			continue
		case node.Supertag != block.typ:
			t.Errorf("%s of %s answers the %s %s as a %q", query, issueKey, block.typ, block.id, node.Supertag)
		case room && (node.Document == nil || node.Document.ArtifactID != block.document):
			t.Errorf("%s of %s answers the %s %s with the document %+v, want %s", query, issueKey, block.typ, block.id, node.Document, block.document)
		}
	}
	whole := treeRead(t, handler, issueKey, "", "alice", false)
	for _, block := range blocks {
		at := slices.IndexFunc(whole.Nodes, func(node model.IssueTreeNode) bool {
			return node.Kind == refs.BlockNodeKind && node.BlockID == block.id
		})
		if at < 0 {
			t.Errorf("the tree read of %s answers no node of the %s %s", issueKey, block.typ, block.id)
			continue
		}
		node := whole.Nodes[at]
		for _, requirement := range block.serves {
			tuple := model.IssueTreeTuple{Kind: "serves", From: model.IssueTreeEnd{Kind: node.Kind, ID: node.ID},
				To: model.IssueTreeEnd{Kind: refs.BlockNodeKind, ID: specKey + "#" + requirement}}
			if !slices.Contains(whole.Tuples, tuple) {
				t.Errorf("the tree read of %s answers no serves from the %s %s to the requirement %s", issueKey, block.typ, block.id, requirement)
			}
		}
		if node.Rank != nil {
			t.Errorf("the tree read of %s answers the %s %s with the rank %s, want none until the reconciliation ranks it", issueKey, block.typ, block.id, *node.Rank)
		}
		for index, sibling := range whole.Nodes {
			if index > at && sibling.Rank != nil && sibling.Parent != nil && node.Parent != nil && *sibling.Parent == *node.Parent {
				t.Errorf("the tree read of %s answers the %s %s, which holds no rank, before its ranked sibling %s", issueKey, block.typ, block.id, sibling.ID)
			}
		}
	}
}

// requireOwedNamed fails t unless the coverage read and the tree read of issueKey each name the
// document artifactID owed at version.
func requireOwedNamed(t *testing.T, handler http.Handler, issueKey, artifactID string, version int, what string) {
	t.Helper()
	want := model.OwedDocument{ArtifactID: artifactID, Version: version}
	if owed := readCoverage(t, handler, issueKey).OwedDocuments; !slices.Contains(owed, want) {
		t.Errorf("after %s the coverage read of %s names the owed documents %+v, want %+v among them", what, issueKey, owed, want)
	}
	if owed := treeRead(t, handler, issueKey, "", "alice", false).OwedDocuments; !slices.Contains(owed, want) {
		t.Errorf("after %s the tree read of %s names the owed documents %+v, want %+v among them", what, issueKey, owed, want)
	}
}

// requireNothingOwed fails t unless neither the coverage read nor the tree read of issueKey answers
// owed_documents, the member absent from both bodies.
func requireNothingOwed(t *testing.T, handler http.Handler, issueKey, what string) {
	t.Helper()
	coverage := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issueKey+"/coverage", nil, "alice")
	if coverage.Code != http.StatusOK || strings.Contains(coverage.Body.String(), `"owed_documents"`) {
		t.Errorf("%s the coverage read of %s answers %d %s, want no owed_documents", what, issueKey, coverage.Code, coverage.Body.String())
	}
	if tree := treeRead(t, handler, issueKey, "", "alice", false); strings.Contains(tree.body, `"owed_documents"`) {
		t.Errorf("%s the tree read of %s answers the owed documents %+v, want no member", what, issueKey, tree.OwedDocuments)
	}
}

// requireUnstatedVersion fails t unless version of artifactID holds no stated record
// (version_renders' time), as a version whose pass the write deferred holds none (D10).
func requireUnstatedVersion(t *testing.T, database *store.Store, artifactID string, version int) {
	t.Helper()
	var stated int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from version_renders where artifact_id = $1 and number = $2 and at is not null`, artifactID, version).Scan(&stated); err != nil {
		t.Fatalf("read the record of version %d of %s: %v", version, artifactID, err)
	}
	if stated != 0 {
		t.Errorf("version %d of %s holds a stated record right after its deferred write, want none until the reconciliation", version, artifactID)
	}
}

// requireCensusRowsAgree takes the store census and fails t unless each kind of kinds reads no row
// difference: no row without the node and the tuples it projects.
func requireCensusRowsAgree(t *testing.T, database *store.Store, what string, kinds ...string) {
	t.Helper()
	report, err := graphcensus.StoreCensus(context.Background(), database.Pool.Config().ConnString(), false, docs.ReturnDeclined)
	if err != nil {
		t.Fatalf("take the store census %s: %v", what, err)
	}
	for _, kind := range report.Kinds {
		if slices.Contains(kinds, kind.Kind) && kind.RowDifferences > 0 {
			t.Errorf("the store census %s reads %d row differences of kind %s: %+v", what, kind.RowDifferences, kind.Kind, kind.ReportedRows)
		}
	}
}

// reconcileUntilNothingOwed runs passes of the block-index reconciliation until the coverage read
// of issueKey names no owed document.
func reconcileUntilNothingOwed(t *testing.T, handler http.Handler, deps Deps, issueKey string) {
	t.Helper()
	for range 8 {
		if len(readCoverage(t, handler, issueKey).OwedDocuments) == 0 {
			return
		}
		reconcileBlockIndexPass(t, deps)
	}
	t.Fatalf("eight passes of the reconciliation left %s owing %+v", issueKey, readCoverage(t, handler, issueKey).OwedDocuments)
}

// acceptedFixture is one issue whose documents a test writes, with what it expects of them, and
// the browsers holding its room owed (holdOwedBy), one per document.
type acceptedFixture struct {
	handler   http.Handler
	database  *store.Store
	documents *docs.Service
	issue     model.Issue
	converted bool
	written   []writtenBlock
	peers     map[string]*syncedPeer
}

// accept runs write, which writes a version of a document of the issue holding blocks (and
// answers that document), and fails t unless, right after it and with no reconciliation run, the
// document holds one new version whose record is unstated, both reads name it owed at that
// version, and the store holds no block node under its key but the ones of blocks it held none
// of before: a paragraph of the version has no node yet. The blocks join what the fixture expects.
func (f *acceptedFixture) accept(t *testing.T, what, artifactID string, blocks []acceptedBlock, write func() string) {
	t.Helper()
	key, before, versions := "", 0, 0
	if artifactID != "" {
		key = blockKeyOf(t, f.database, artifactID)
		before, versions = blockNodeCount(t, f.database, key), latestVersion(t, f.database, artifactID).Number
	} else if f.converted && f.issue.Key != "" {
		key = blockKeyOf(t, f.database, f.issue.PrimaryArtifactID)
		before = blockNodeCount(t, f.database, key)
	}
	added := 0
	for _, block := range blocks {
		for _, id := range []string{block.id, block.reading} {
			if id != "" && (key == "" || readHeldTypedBlock(t, f.database, key+"#"+id).state == "") {
				added++
			}
		}
	}
	document := write()
	version := latestVersion(t, f.database, document).Number
	if version != versions+1 {
		t.Fatalf("%s wrote version %d of %s, want %d", what, version, document, versions+1)
	}
	requireUnstatedVersion(t, f.database, document, version)
	requireOwedNamed(t, f.handler, f.issue.Key, document, version, what)
	key = blockKeyOf(t, f.database, document)
	for _, block := range blocks {
		f.written = slices.DeleteFunc(f.written, func(held writtenBlock) bool { return held.key == key && held.id == block.id })
		f.written = append(f.written, writtenBlock{acceptedBlock: block, key: key, document: document, version: version})
	}
	if after := blockNodeCount(t, f.database, key); after != before+added {
		t.Errorf("%s left %d block nodes under %s, want the %d before it and the %d of its typed blocks the store held none of: no other block of its version is a node yet",
			what, after, key, before, added)
	}
}

// upload uploads name to the issue holding blocks, padded with a list past the bound where padded.
func (f *acceptedFixture) upload(t *testing.T, name string, padded bool, blocks ...acceptedBlock) {
	t.Helper()
	owner := "/api/v1/issues/" + f.issue.Key
	existing := f.issue.PrimaryArtifactID
	if name != "spec" {
		existing = ""
		if err := f.database.Pool.QueryRow(context.Background(),
			`select coalesce((select id::text from artifacts where issue_key = $1 and name = $2), '')`, f.issue.Key, name).Scan(&existing); err != nil {
			t.Fatalf("read the document %s of %s: %v", name, f.issue.Key, err)
		}
	}
	f.accept(t, "the upload of "+name, existing, blocks, func() string {
		return uploadDocument(t, f.handler, owner, name, typedDocument(f.issue.Key, padded, blocks...)).Artifact.ID
	})
}

// writeAgain uploads a second version of document artifactID of the fixture's issue: its newest
// version's markdown with one paragraph more, which leaves every typed block as it stands, so the
// version holds no block the fixture does not already expect. The spec is uploaded by the name
// upload gives it.
func (f *acceptedFixture) writeAgain(t *testing.T, artifactID string) {
	t.Helper()
	var name, markdown string
	if err := f.database.Pool.QueryRow(context.Background(), `
		select a.name, v.markdown from artifacts a join artifact_versions v on v.artifact_id = a.id
		where a.id = $1::uuid order by v.number desc limit 1`, artifactID).Scan(&name, &markdown); err != nil {
		t.Fatalf("read the newest version of %s: %v", artifactID, err)
	}
	if artifactID == f.issue.PrimaryArtifactID {
		name = "spec"
	}
	f.accept(t, "the second version of "+name, artifactID, nil, func() string {
		return uploadDocument(t, f.handler, "/api/v1/issues/"+f.issue.Key, name, markdown+"\nA paragraph the second version adds.\n").Artifact.ID
	})
}

// edit inserts blocks at the end of document artifactID through POST /api/v1/artifacts/{id}/edits.
func (f *acceptedFixture) edit(t *testing.T, artifactID string, blocks ...acceptedBlock) {
	t.Helper()
	markdown := make([]string, len(blocks))
	for index, block := range blocks {
		markdown[index] = typedBlockMarkdown(f.issue.Key, block)
	}
	f.accept(t, "the edit inserting "+blocks[0].id, artifactID, blocks, func() string {
		succeeded(t, "insert "+blocks[0].id, dispatchRequest(t, f.handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{
			"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": strings.Join(markdown, "\n")}},
		}, "alice"))
		return artifactID
	})
}

// documentNamed is the id of the document of the fixture's issue named name.
func (f *acceptedFixture) documentNamed(t *testing.T, name string) string {
	t.Helper()
	var id string
	if err := f.database.Pool.QueryRow(context.Background(),
		`select id::text from artifacts where issue_key = $1 and name = $2`, f.issue.Key, name).Scan(&id); err != nil {
		t.Fatalf("read the document %s of %s: %v", name, f.issue.Key, err)
	}
	return id
}

// holdOwedBy has a browser write written - a paragraph where it is "" - at the end of the fragment
// of document artifactID of the converted issue's room, with no settlement versioning the edit
// (connectBrowserPeer), so a write of any other document of the room leaves the room owed its block
// index (docs' indexDocumentInRoom): another fragment reads otherwise than its document's latest
// version. A document no browser is open on yet takes one.
func (f *acceptedFixture) holdOwedBy(t *testing.T, artifactID, written string) {
	t.Helper()
	if written == "" {
		written = "A browser's words no settlement has versioned.\n"
	}
	peer, open := f.peers[artifactID]
	if !open {
		if f.peers == nil {
			f.peers = map[string]*syncedPeer{}
		}
		peer = connectBrowserPeer(t, f.handler, f.documents, artifactID)
		t.Cleanup(peer.close)
		f.peers[artifactID] = peer
	}
	peer.appendParagraph(t, written)
	peer.barrier(t)
}

// settleOwed settles the issue's documents, versioning the browsers' edits, and closes the
// browsers.
func (f *acceptedFixture) settleOwed(t *testing.T) {
	t.Helper()
	if len(f.peers) == 0 {
		return
	}
	settleIssue(t, f.documents, f.issue.Key)
	for _, peer := range f.peers {
		peer.close()
	}
}

// requireTwinListsRequirement fails t unless the coverage read over the tuples of issueKey, which
// takes its requirements by the walk of the issue's tree (issueCoverageOverTuples), lists the
// requirement id: the walk from the issue reaches it.
func requireTwinListsRequirement(t *testing.T, deps Deps, database *store.Store, issueKey, id string) {
	t.Helper()
	coverage, err := directServer(deps).issueCoverageOverTuples(context.Background(), database.Pool, model.Actor{Kind: "user", ID: "alice"}, issueKey, nil)
	if err != nil {
		t.Fatalf("read the coverage of %s over the tuples: %v", issueKey, err)
	}
	if !slices.ContainsFunc(coverage.Requirements, func(requirement model.RequirementCoverage) bool { return requirement.BlockID == id }) {
		t.Errorf("the coverage read of %s over the tuples does not list the requirement %s: the walk from the issue does not reach it (%+v)", issueKey, id, coverage.Requirements)
	}
}

// partOfChain is the chain the current part_of tuples climb from the node address, each far end
// with whether the store holds it as a node, up to the first end that is no block.
func partOfChain(t *testing.T, database *store.Store, address string) string {
	t.Helper()
	var chain []string
	for kind, id := refs.BlockNodeKind, address; kind == refs.BlockNodeKind && len(chain) < 8; {
		var held bool
		if err := database.Pool.QueryRow(context.Background(), `
			select coalesce(t.to_kind, ''), coalesce(t.to_id, ''),
			       exists (select 1 from graph_nodes n where n.kind = t.to_kind and n.id = t.to_id)
			from (select 1) one
			left join lateral (select g.to_kind, g.to_id, g.asserted from graph_tuples g
			                   where g.from_kind = $1 and g.from_id = $2 and g.kind = 'part_of'
			                   order by g.seq desc limit 1) t on t.asserted`, kind, id).Scan(&kind, &id, &held); err != nil {
			t.Fatalf("read the part_of of %s: %v", id, err)
		}
		if kind == "" {
			break
		}
		chain = append(chain, fmt.Sprintf("%s:%s (node held %t)", kind, id, held))
	}
	return address + " -> " + strings.Join(chain, " -> ")
}

// requireDecisionWritten fails t unless the store holds the decision of the block node address
// live, under its type's supertag: the typed-node pass's node.
func requireDecisionWritten(t *testing.T, database *store.Store, address string) {
	t.Helper()
	if held := readHeldTypedBlock(t, database, address); held.state != graphstore.StateLive || held.supertag != refs.DecisionSupertag {
		t.Errorf("the decision %s reads node state %q under the supertag %q right after its write, want a live decision", address, held.state, held.supertag)
	}
}

// acceptedDeferral is one of the three deferrals in which the tree the write wrote renders as the
// version's markdown, as the tests below write into it.
type acceptedDeferral struct {
	name                string
	converted, leftOwed bool
	// spec, verification and plan say which of those uploads carry a list past the bound: in an
	// issue room, the spec's puts the room past the bound for every write after it.
	spec, verification, plan bool
}

// acceptedDeferrals are those three deferrals: a document above the bound, an issue room above the
// bound, and a room left owed.
var acceptedDeferrals = []acceptedDeferral{
	{name: "a document above the bound", spec: true, verification: true, plan: true},
	{name: "an issue room above the bound", converted: true, spec: true},
	{name: "a room left owed", converted: true, leftOwed: true},
}

// criterionChain is a requirement two levels under a criterion that stands in a list: r5's parent
// attribute names r6, and r6's the criterion k1. headingCheck is a check in the list under a
// heading of the Verification, serving r1, which the issue's create indexed in its own request, so
// a write that wrote no check at once still holds the pointer its upload is checked by.
var (
	criterionChain = []acceptedBlock{
		{id: "k1", typ: "criterion", listed: true},
		{id: "r6", typ: "requirement", parent: "k1"},
		{id: "r5", typ: "requirement", parent: "r6"},
	}
	headingCheck = acceptedBlock{id: "c9", typ: "check", serves: []string{"r1"}, listed: true, heading: "The checks of the criterion"}
)

// acceptedScenario is what writeAcceptedVersions wrote: the server, the issue whose spec,
// Verification and plan it wrote (fixture), with the ref_key the spec's blocks are keyed under,
// and, in a deferral by size, the issue whose create wrote its spec (created).
type acceptedScenario struct {
	handler          http.Handler
	database         *store.Store
	deps             Deps
	fixture, created *acceptedFixture
	specKey          string
}

// deferred is every block of the fixture's issue the deferred writes wrote rows of: every one the
// fixture holds but the spec's r1 and r2, which the issue's create indexed in its own request.
func (s acceptedScenario) deferred() []writtenBlock {
	var blocks []writtenBlock
	for _, block := range s.fixture.written {
		if block.id != "r1" && block.id != "r2" {
			blocks = append(blocks, block)
		}
	}
	return blocks
}

// writeAcceptedVersions writes, in deferral, a spec with requirements (r3 and criterionChain), a
// Verification with checks serving them (headingCheck among them) and a plan with plan tasks
// serving them, by uploads, and a check of the Verification and a requirement of the spec by edits
// too - in a deferral by size the edit route defers its version past the bound as the upload does
// (docs.WithDeferredBlockIndex) - each write checked right after it (acceptedFixture.accept); and,
// in a deferral by size, a second issue whose create writes its spec's requirement r9 past the
// bound. A create cannot leave a room owed: the room it creates holds only what it seeds.
func writeAcceptedVersions(t *testing.T, deferral acceptedDeferral) acceptedScenario {
	t.Helper()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour, browserPeers: deferral.leftOwed})
	documents := deps.Docs.(*docs.Service)
	createReferenceAPIProject(t, handler, "OWED")
	issue := createDigestIssue(t, handler, database, "OWED", "The accepted versions of "+deferral.name,
		typedDocument("", false, acceptedRequirement("r1"), acceptedRequirement("r2")), !deferral.converted)
	fixture := &acceptedFixture{handler: handler, database: database, documents: documents, issue: issue, converted: deferral.converted}
	spec := slices.Concat([]acceptedBlock{acceptedRequirement("r1"), acceptedRequirement("r2"), acceptedRequirement("r3")}, criterionChain)
	checks := []acceptedBlock{acceptedCheck("c1", "r1"), acceptedCheck("c2", "r2"), headingCheck}
	if deferral.leftOwed {
		// A browser's edit of the plan holds the room owed while the spec and the Verification are
		// written, by uploads and edits, and one of the Verification while the plan is, by an edit:
		// an upload names the token of the version it replaces, which a document holding a browser's
		// unversioned edit no longer stands at.
		plan := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "plan.md", "The plan to come.\n").Artifact.ID
		fixture.holdOwedBy(t, plan, "")
		fixture.upload(t, "spec", false, spec...)
		fixture.upload(t, "verification.md", false, checks...)
		fixture.edit(t, fixture.documentNamed(t, "verification.md"), acceptedCheck("c3", "r1"))
		fixture.edit(t, issue.PrimaryArtifactID, acceptedRequirement("r4"))
		fixture.holdOwedBy(t, fixture.documentNamed(t, "verification.md"), "")
		fixture.edit(t, plan, acceptedPlanTask("t1", "r1"), acceptedPlanTask("t2", "r1", "r2"), acceptedPlanTask("t3", "r4"))
	} else {
		// The upload of the spec rewrites r1 and r2 as the version before held them, so only r3 and
		// the blocks of criterionChain are new.
		fixture.upload(t, "spec", deferral.spec, spec...)
		fixture.upload(t, "verification.md", deferral.verification, checks...)
		fixture.upload(t, "plan.md", deferral.plan, acceptedPlanTask("t1", "r1"), acceptedPlanTask("t2", "r1", "r2"))
		fixture.edit(t, fixture.documentNamed(t, "verification.md"), acceptedCheck("c3", "r1"))
		fixture.edit(t, issue.PrimaryArtifactID, acceptedRequirement("r4"))
	}
	scenario := acceptedScenario{handler: handler, database: database, deps: deps, fixture: fixture, specKey: blockKeyOf(t, database, issue.PrimaryArtifactID)}
	if !deferral.leftOwed {
		created := &acceptedFixture{handler: handler, database: database, documents: documents, converted: deferral.converted}
		created.accept(t, "the issue create", "", []acceptedBlock{acceptedRequirement("r9")}, func() string {
			created.issue = createDigestIssue(t, handler, database, "OWED", "The create of "+deferral.name,
				typedDocument("", true, acceptedRequirement("r9")), !deferral.converted)
			return created.issue.PrimaryArtifactID
		})
		scenario.created = created
	}
	return scenario
}

// reconcile settles the browsers' edits that hold the room owed, if any, and runs the
// reconciliation until neither issue of s owes anything.
func (s acceptedScenario) reconcile(t *testing.T) {
	t.Helper()
	s.fixture.settleOwed(t)
	reconcileUntilNothingOwed(t, s.handler, s.deps, s.fixture.issue.Key)
	if s.created != nil {
		reconcileUntilNothingOwed(t, s.handler, s.deps, s.created.issue.Key)
	}
}
