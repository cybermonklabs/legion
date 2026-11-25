// Package explaintest reads an `EXPLAIN (FORMAT JSON)` plan tree for the things every
// index-pinning test in this module asks it: whether a named relation is sequentially scanned
// anywhere in the plan, whether any scan in it reads a relation without an index condition,
// which index each index scan reads through, under which condition, which relations the plan
// reads at all, how often each and under which subplan (Scans), every node with the conditions
// it tests, what an analyzed plan measured of it, the rows its Filter and a join's Join Filter
// removed each time it ran among them, and the shared buffers it hit and read (Nodes), and each
// bitmap heap scan with the bitmap it reads its rows through (BitmapHeapScans). It also answers,
// from the catalog, the first key column of a named index and whether a scan's condition names
// it, which is what tells a read served by an index from one that reads the whole index and
// filters it, and it holds a whole plan to that (RequireIndexKeyed) and builds the generic plan a
// prepared statement settles on, which is the plan an index is lost in, explained (GenericPlan)
// or run and measured (AnalyzedGenericPlan), and the plan made for a statement's values, run and
// measured (AnalyzedPlan). A test that holds such a plan to a check it writes itself holds the
// same check to failing a control's plan (RequireControlFails, through Failures). For a statement
// over a batch of rows (batch.go) it runs the statement a writer sent, after the statements it
// sent first, under each plan-cache mode (ExplainAfter), and holds the analyzed plan to reading
// the batch once and each named table by a probe of one row's key (UnprobedReads,
// RequireEachRowProbedOnce), with a control that must fail it (RequireTheCheckFails), and it runs
// any statement under a plan-cache mode with the planner's own settings and explains it with the
// shared buffers each node hit and read (ExplainBuffered).
// dispatch/api, dispatch/refs, dispatch/graphstore, dispatch/sessionanswers and cipher/requests
// each pin a plan with these checks, so they live here once rather than as a second (or third)
// hand copy of the same recursive walk; it is an ordinary package rather than a _test.go file
// because Go cannot share test-only code across packages (see internal/stacktest's identical
// reasoning).
package explaintest

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// indexScanTypes is every plan node that reads a relation through an index, and so every node
// that can carry an "Index Cond".
var indexScanTypes = []string{"Index Scan", "Index Only Scan", "Bitmap Index Scan"}

// planNode is what the checks below read of one "Plan" node of `EXPLAIN (FORMAT JSON)`: its
// type, the relation and index it names, the index condition it carries - nil when it carries
// none, which is not the same as an empty one - the condition it tests every row it reads by
// (Filter) and the one a bitmap heap scan rechecks a row by, a join's type, the condition a hash
// or a merge join matches its two sides by (Hash Cond, Merge Cond) and the one a join tests each
// pair it joins by (Join Filter), the name of the subplan it begins and its place under its own
// parent, what an analyzed plan measured of it (its loops and the rows each answered, the rows an
// index-only scan fetched from the heap, and the rows its Filter and a join's Join Filter removed
// each time it ran, all 0 in a plan explained without ANALYZE), the shared buffers it and the
// nodes under it hit and read (0 in a plan explained without BUFFERS), the CTE it scans, the alias
// the statement gives the relation it reads, and its children.
type planNode struct {
	NodeType                string            `json:"Node Type"`
	RelationName            string            `json:"Relation Name"`
	IndexName               string            `json:"Index Name"`
	IndexCond               *string           `json:"Index Cond"`
	Filter                  string            `json:"Filter"`
	RecheckCond             string            `json:"Recheck Cond"`
	JoinType                string            `json:"Join Type"`
	HashCond                string            `json:"Hash Cond"`
	MergeCond               string            `json:"Merge Cond"`
	JoinFilter              string            `json:"Join Filter"`
	SubplanName             string            `json:"Subplan Name"`
	ParentRelationship      string            `json:"Parent Relationship"`
	ActualLoops             float64           `json:"Actual Loops"`
	ActualRows              float64           `json:"Actual Rows"`
	HeapFetches             float64           `json:"Heap Fetches"`
	RowsRemovedByFilter     float64           `json:"Rows Removed by Filter"`
	RowsRemovedByJoinFilter float64           `json:"Rows Removed by Join Filter"`
	SharedHitBlocks         float64           `json:"Shared Hit Blocks"`
	SharedReadBlocks        float64           `json:"Shared Read Blocks"`
	CTEName                 string            `json:"CTE Name"`
	Alias                   string            `json:"Alias"`
	Plans                   []json.RawMessage `json:"Plans"`

	// subplan is the subplan this node runs under, which the walk carries down from the node
	// that began it (nodesUnder), and so is no field of the plan's own JSON.
	subplan string
	// parent is the position in the walk of the node this one is directly under, -1 for the
	// root, which the walk records as it descends (nodesUnder).
	parent int
}

// asNode is n as the exported checks answer it, its index condition empty where it carries none.
func (n planNode) asNode() Node {
	found := Node{
		Type: n.NodeType, Relation: n.RelationName, Index: n.IndexName,
		CTE: n.CTEName, Alias: n.Alias, Subplan: n.subplan,
		Loops: n.ActualLoops, Rows: n.ActualRows,
		Filter: n.Filter, RecheckCond: n.RecheckCond,
		JoinType: n.JoinType, HashCond: n.HashCond, MergeCond: n.MergeCond, JoinFilter: n.JoinFilter,
		HeapFetches:         n.HeapFetches,
		RowsRemovedByFilter: n.RowsRemovedByFilter, RowsRemovedByJoinFilter: n.RowsRemovedByJoinFilter,
		SharedHitBlocks: n.SharedHitBlocks, SharedReadBlocks: n.SharedReadBlocks,
	}
	if n.IndexCond != nil {
		found.IndexCond = *n.IndexCond
	}
	return found
}

// OnePlan is the one "Plan" node of raw, an `EXPLAIN (FORMAT JSON)` answer, which is what every
// check below is given. A statement explains to a top-level array of one plan, so an answer
// holding any other number is the explain the caller did not ask for, and the whole answer is
// printed with the refusal.
func OnePlan(t testing.TB, raw []byte) json.RawMessage {
	t.Helper()
	var plans []struct {
		Plan json.RawMessage `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil {
		t.Fatalf("decode the explain answer: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("explain answered %d plans; want one:\n%s", len(plans), raw)
	}
	return plans[0].Plan
}

// planNodes flattens planJSON - one "Plan" node of `EXPLAIN (FORMAT JSON)`'s top-level array -
// into every node of the tree under it, each node before its children and each carrying the
// subplan it runs under and the position of its parent. Every check this package exports is a
// filter over this one walk, so the tree is descended in one place rather than once per check.
func planNodes(t testing.TB, planJSON json.RawMessage) []planNode {
	t.Helper()
	return nodesUnder(t, planJSON, "", -1, nil)
}

// nodesUnder is that walk of one node and the tree below it, appended to nodes, the walk so far,
// whose node at position parent the node is directly under (-1 for the root): a node whose
// parent relationship is SubPlan begins a subplan, and every node under it belongs to that one,
// since a plan under a subplan runs as often as the subplan does.
func nodesUnder(t testing.TB, planJSON json.RawMessage, subplan string, parent int, nodes []planNode) []planNode {
	t.Helper()
	var node planNode
	if err := json.Unmarshal(planJSON, &node); err != nil {
		t.Fatalf("decode plan node: %v", err)
	}
	if node.ParentRelationship == "SubPlan" {
		subplan = node.SubplanName
		if subplan == "" {
			subplan = node.NodeType
		}
	}
	node.subplan = subplan
	node.parent = parent
	position := len(nodes)
	nodes = append(nodes, node)
	for _, child := range node.Plans {
		nodes = nodesUnder(t, child, subplan, position, nodes)
	}
	return nodes
}

// SeqScansRelation reports whether planJSON — one "Plan" node of `EXPLAIN (FORMAT JSON)`'s
// top-level array — contains a "Seq Scan" node naming relation, at any depth.
func SeqScansRelation(t testing.TB, planJSON json.RawMessage, relation string) bool {
	t.Helper()
	for _, node := range planNodes(t, planJSON) {
		if node.NodeType == "Seq Scan" && node.RelationName == relation {
			return true
		}
	}
	return false
}

// IndexScan is one index-reading node of a plan, with what a caller needs to judge the read: the
// node type, the relation (empty on a "Bitmap Index Scan", which reports only its index), the
// index the node reads through, and the index condition the plan wrote, empty when the node
// carries none.
type IndexScan struct {
	NodeType string
	Relation string
	Index    string
	Cond     string
}

// String names the scan as a failure message cites it, the way ScansWithoutIndexCond names one.
func (s IndexScan) String() string {
	return describeScan(s.NodeType, s.Relation, s.Index)
}

// IndexScans names every index scan in planJSON — one "Plan" node of `EXPLAIN (FORMAT JSON)`'s
// top-level array — at any depth, in the order the walk meets them. ScansWithoutIndexCond holds
// a scan to having some index condition; whether that condition keys the index's own first
// column, the only column an index scan actually narrows on, is a question the catalog answers,
// so a caller that asks it reads each scan's index and condition from here and judges the pair
// itself.
func IndexScans(t testing.TB, planJSON json.RawMessage) []IndexScan {
	t.Helper()
	var scans []IndexScan
	for _, node := range planNodes(t, planJSON) {
		if !slices.Contains(indexScanTypes, node.NodeType) {
			continue
		}
		scan := IndexScan{NodeType: node.NodeType, Relation: node.RelationName, Index: node.IndexName}
		if node.IndexCond != nil {
			scan.Cond = *node.IndexCond
		}
		scans = append(scans, scan)
	}
	return scans
}

// ScansWithoutIndexCond names every scan in planJSON — one "Plan" node of `EXPLAIN (FORMAT
// JSON)`'s top-level array — that reads a relation no index condition keys, at any depth: a
// "Seq Scan", and an "Index Scan", "Index Only Scan" or "Bitmap Index Scan" carrying no "Index
// Cond". A statement written to reach every table through an index is held to that by its plan,
// and such a scan is a read no index serves. Each name carries the node type, the relation and
// the index the node reports, so a failure says which read is unkeyed rather than only that one
// is.
func ScansWithoutIndexCond(t testing.TB, planJSON json.RawMessage) []string {
	t.Helper()
	var unkeyed []string
	for _, node := range planNodes(t, planJSON) {
		unkeyedScan := node.NodeType == "Seq Scan" ||
			(slices.Contains(indexScanTypes, node.NodeType) && node.IndexCond == nil)
		if unkeyedScan {
			unkeyed = append(unkeyed, describeScan(node.NodeType, node.RelationName, node.IndexName))
		}
	}
	return unkeyed
}

// Scan is one node of a plan that reads a relation: which relation, through which index (empty
// when the node names none), under which subplan (empty: none, so the node runs with the plan
// rather than once per row of what drives it), how many times it ran and how many rows it
// answered each time. Loops and Rows are what `EXPLAIN (ANALYZE)` measured and are 0 in a plan
// explained without it, so a caller that reads them explains with ANALYZE.
type Scan struct {
	Relation, Index, Subplan string
	Loops, Rows              float64
}

// Scans names every scan of a relation in planJSON — one "Plan" node of `EXPLAIN (FORMAT
// JSON)`'s top-level array — at any depth, in the order the walk meets them. A statement
// written to touch only what it must is held to that by its plan: which relations it reads at
// all, which of them it reads more than once or once per row of another, and how many rows the
// read it is driven by answered.
func Scans(t testing.TB, planJSON json.RawMessage) []Scan {
	t.Helper()
	var scans []Scan
	for _, node := range planNodes(t, planJSON) {
		if node.RelationName == "" {
			continue
		}
		scans = append(scans, Scan{
			Relation: node.RelationName, Index: node.IndexName, Subplan: node.subplan,
			Loops: node.ActualLoops, Rows: node.ActualRows,
		})
	}
	return scans
}

// Node is one node of a plan, whatever that node reads: its type, the relation, index and CTE
// it names (each empty when the node names none), the subplan it runs under (empty: none, so
// it runs with the plan rather than once per row of what drives it), the conditions it tests,
// and what `EXPLAIN (ANALYZE)` measured of it, 0 in a plan explained without ANALYZE.
//
// Filter is the condition the node tests every row it reads by, IndexCond the one an index scan
// reads its index by, and RecheckCond the one a bitmap heap scan tests a row by where its bitmap
// cannot vouch for the row (a page the bitmap holds lossily, or an index that answers lossily).
// JoinType is a join's type (Inner, Left, Semi, Anti and so on), HashCond and MergeCond the
// condition a hash join and a merge join match their two sides by, and JoinFilter the one a join
// tests each pair it joins by. Each is the plan's own text, empty when the node carries none.
// Loops is how many times the node ran and Rows how many rows it answered each time; HeapFetches
// is how many rows an index-only scan fetched from the heap, since the visibility map did not mark
// their pages all-visible. RowsRemovedByFilter is how many rows the node's Filter removed each
// time it ran, and RowsRemovedByJoinFilter how many rows a join's Join Filter removed each time it
// ran: each is the per-loop figure `EXPLAIN (ANALYZE)` prints ("Rows Removed by Filter", "Rows
// Removed by Join Filter"), the rows removed over every loop divided by Loops and rounded to a
// whole number, and 0 where the node prints none, as a node carrying no such filter does. Alias is
// the name the statement gives the relation the node reads, which a condition of another node
// spells that relation's columns with. SharedHitBlocks and SharedReadBlocks are the shared buffers
// the node and the nodes under it hit in the cache and read into it, which a plan explained with
// the BUFFERS option carries, and 0 in a plan explained without it.
type Node struct {
	Type, Relation, Index, CTE, Alias, Subplan   string
	Loops, Rows                                  float64
	Filter, IndexCond, RecheckCond               string
	JoinType, HashCond, MergeCond, JoinFilter    string
	HeapFetches                                  float64
	RowsRemovedByFilter, RowsRemovedByJoinFilter float64
	SharedHitBlocks, SharedReadBlocks            float64
}

// String names the node as a failure message cites it: its type, and whichever of the relation,
// the index and the CTE it names.
func (n Node) String() string {
	description := describeScan(n.Type, n.Relation, n.Index)
	if n.CTE != "" {
		description += " on " + n.CTE
	}
	return description
}

// Nodes is every node of planJSON — one "Plan" node of `EXPLAIN (FORMAT JSON)`'s top-level
// array — at any depth, in the order the walk meets them. Scans is this walk narrowed to the
// nodes that name a relation; a caller reads the whole of it when the node it must judge reads
// no relation at all, as a "CTE Scan" does: a CTE scanned more than once is a side the
// statement pays for per row of whatever drives it, and its loops are the only place the plan
// says so (store.TestTheEmbeddingProjectKeyBatchReadsOneBoundedIndexScan).
func Nodes(t testing.TB, planJSON json.RawMessage) []Node {
	t.Helper()
	var nodes []Node
	for _, node := range planNodes(t, planJSON) {
		nodes = append(nodes, node.asNode())
	}
	return nodes
}

// BitmapHeapScan is one "Bitmap Heap Scan" of a plan with the bitmap it reads its rows through:
// the scan itself; Bitmap, the node directly under it that builds the bitmap, a "Bitmap Index
// Scan" of one index or a "BitmapOr" or "BitmapAnd" that combines the bitmaps under it; and
// BitmapIndexScans, every "Bitmap Index Scan" that bitmap is built from, in the order the walk
// meets them.
type BitmapHeapScan struct {
	Scan, Bitmap     Node
	BitmapIndexScans []Node
}

// OrOf reports whether s reads its rows through a BitmapOr whose Bitmap Index Scans read each of
// indexes, among any others.
func (s BitmapHeapScan) OrOf(indexes ...string) bool {
	if s.Bitmap.Type != "BitmapOr" {
		return false
	}
	for _, index := range indexes {
		if !slices.ContainsFunc(s.BitmapIndexScans, func(scan Node) bool { return scan.Index == index }) {
			return false
		}
	}
	return true
}

// BitmapHeapScans is every "Bitmap Heap Scan" of relation in planJSON — one "Plan" node of
// `EXPLAIN (FORMAT JSON)`'s top-level array — at any depth, in the order the walk meets them,
// each with its bitmap. A statement written to read a relation by one bitmap over several
// indexes is held to that by these: which node builds each scan's bitmap, and which indexes it
// reads. A scan's bitmap is its one child of parent relationship Outer, any other child being a
// subplan its conditions run; a "BitmapOr" or "BitmapAnd" holds the bitmaps it combines as its
// children of parent relationship Member, so the bitmap's index scans are the Bitmap Index Scans
// reached from the scan's Outer child through Member children alone.
func BitmapHeapScans(t testing.TB, planJSON json.RawMessage, relation string) []BitmapHeapScan {
	t.Helper()
	nodes := planNodes(t, planJSON)
	var scans []BitmapHeapScan
	// scanAt is the place in scans of the Bitmap Heap Scan at each position of the walk that
	// holds one of relation. The walk meets a node before its children, so a scan is in it before
	// any node of its bitmap is read.
	scanAt := map[int]int{}
	for position, node := range nodes {
		if node.NodeType == "Bitmap Heap Scan" && node.RelationName == relation {
			scanAt[position] = len(scans)
			scans = append(scans, BitmapHeapScan{Scan: node.asNode()})
			continue
		}
		if node.ParentRelationship == "Outer" {
			if at, ok := scanAt[node.parent]; ok {
				scans[at].Bitmap = node.asNode()
			}
		}
		if node.NodeType != "Bitmap Index Scan" {
			continue
		}
		top := position
		for nodes[top].ParentRelationship == "Member" {
			top = nodes[top].parent
		}
		if nodes[top].ParentRelationship == "Outer" {
			if at, ok := scanAt[nodes[top].parent]; ok {
				scans[at].BitmapIndexScans = append(scans[at].BitmapIndexScans, node.asNode())
			}
		}
	}
	return scans
}

// describeScan names one scan node as a failure message cites it. A Bitmap Index Scan reports no
// relation and a Seq Scan no index, so each part is left out when the plan does not carry it.
func describeScan(nodeType, relation, index string) string {
	description := nodeType
	if relation != "" {
		description += " on " + relation
	}
	if index != "" {
		description += " using " + index
	}
	return description
}

// Queryer is the one call the catalog reads below need, shared by a pool and a transaction.
type Queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// IndexKeys reads the first key column of each index a plan names, which is the one column an
// index scan narrows on: a scan whose condition names a column its index does not lead with
// reads the whole index and filters it, and carries an "Index Cond" all the same. Each index is
// read from the catalog once and kept, since one test reads the same indexes over and over.
type IndexKeys struct {
	queryer Queryer
	keys    map[string]string
}

// NewIndexKeys reads its indexes through queryer.
func NewIndexKeys(queryer Queryer) *IndexKeys {
	return &IndexKeys{queryer: queryer, keys: map[string]string{}}
}

// FirstKey is the first key column of the index named index, as the index itself declares it: a
// plain column reads `session_id`, and an expression index reads its expression,
// `((ask_id)::text)`.
func (k *IndexKeys) FirstKey(t testing.TB, index string) string {
	t.Helper()
	if definition, ok := k.keys[index]; ok {
		return definition
	}
	var definition string
	statement := `select pg_get_indexdef(c.oid, 1, true) from pg_class c where c.relname = $1`
	if err := k.queryer.QueryRow(context.Background(), statement, index).Scan(&definition); err != nil {
		t.Fatalf("read the first key column of %s: %v", index, err)
	}
	k.keys[index] = definition
	return definition
}

// indexKeyNoise matches what a first key column's definition writes that names no column: a
// string literal, and the type a cast names.
var indexKeyNoise = regexp.MustCompile(`'[^']*'|::"?[a-z_][a-z0-9_]*"?`)

// indexKeyWord matches one lower-case identifier of a key definition or of a plan's Index Cond.
// A keyword Postgres deparses is upper case and so matches nothing, and a word of a longer
// identifier is not one of these: `parent` does not match inside `parent_key`.
var indexKeyWord = regexp.MustCompile(`[a-z_][a-z0-9_]*`)

// CondNamesFirstKey reports whether cond, a scan's Index Cond, reads firstKey, its index's first
// key column as FirstKey reads it. The two are compared by the identifiers of the key rather
// than as strings, because the plan writes the same expression its own way: it qualifies each
// column with the query's alias and spaces and casts as it likes, so the names are all the two
// spellings share. Dropping the string literals and cast types of the key definition leaves its
// columns and its lower-case function names, and one of those appearing as a whole word of cond
// is the scan reading the key.
func CondNamesFirstKey(cond, firstKey string) bool {
	words := indexKeyWord.FindAllString(cond, -1)
	for _, identifier := range indexKeyWord.FindAllString(indexKeyNoise.ReplaceAllString(firstKey, " "), -1) {
		if slices.Contains(words, identifier) {
			return true
		}
	}
	return false
}

// AnyIndex is RequireIndexKeyed's index for a plan held to no index in particular: every index
// scan of it is held to the first key column of whatever index it reads, and a plan holding no
// index scan at all is no failure.
const AnyIndex = ""

// CondRule is what RequireIndexKeyed makes of a scan carrying no index condition at all.
type CondRule int

const (
	// CondRequired fails such a scan: it reads the whole of its index.
	CondRequired CondRule = iota
	// CondOptional leaves it to the caller: a read whose index is partial on the read's own
	// filter wants the whole of that index, and a caller that has already named every unkeyed
	// read through ScansWithoutIndexCond would otherwise name it twice.
	CondOptional
)

// RequireIndexKeyed holds every index scan of plan that reads through index to that index's own
// first key column, the one column an index scan narrows on: a scan whose condition names a
// column its index does not lead with reads the whole index and filters it, and carries an
// "Index Cond" all the same. A plan that searches index nowhere is a failure too, since a read
// pinned to an index that no longer reaches it would otherwise pass this check in silence; pass
// AnyIndex to hold each scan to its own index instead and require none. Every failure names
// subject, the read under test, and prints the plan.
func (k *IndexKeys) RequireIndexKeyed(t testing.TB, plan json.RawMessage, index, subject string, rule CondRule) {
	t.Helper()
	searched := false
	for _, scan := range IndexScans(t, plan) {
		if index != AnyIndex && scan.Index != index {
			continue
		}
		searched = true
		if scan.Cond == "" {
			if rule == CondRequired {
				t.Errorf("%s reads all of %s, under no index condition:\n%s", subject, scan.Index, plan)
			}
			continue
		}
		if firstKey := k.FirstKey(t, scan.Index); !CondNamesFirstKey(scan.Cond, firstKey) {
			t.Errorf("%s reads %s on `%s`, which names nothing of that index's first key column `%s`:\n%s",
				subject, scan, scan.Cond, firstKey, plan)
		}
	}
	if index != AnyIndex && !searched {
		t.Errorf("%s searches no %s:\n%s", subject, index, plan)
	}
}

// GenericPlan is the plan Postgres builds for statement without knowing the values it will be
// executed with, the plan a prepared statement settles on. It is the plan at risk: an index
// whose eligibility depends on the bound value is lost there, and a fixture small enough for a
// test never reaches a generic plan on its own, so the mode is forced.
//
// The statement is prepared under name with the parameter types parameters, and EXECUTE takes
// no bind parameter, so literals are written into it. settings are the `set local` statements
// the plan is read under, the caller's own: sequential scans off is every caller's, and a plan
// the planner can hash or merge a whole relation into needs those two off as well. The prepared
// statement outlives the transaction that explains it and is deallocated by name on connection,
// the one connection that holds it.
func GenericPlan(
	t testing.TB,
	ctx context.Context,
	connection *pgxpool.Conn,
	name, statement string,
	parameters, literals []string,
	settings ...string,
) json.RawMessage {
	t.Helper()
	return explainGenericPlan(t, ctx, connection, "format json", true, name, statement, parameters, literals, settings)
}

// AnalyzedGenericPlan is GenericPlan's plan run and measured: the prepared statement is explained
// with ANALYZE and BUFFERS, so each node carries the loops and rows it ran and the shared buffers
// it hit and read (Nodes). ANALYZE executes the statement, so the transaction it runs in is
// rolled back rather than committed, and nothing the statement wrote is left behind. A prepared
// statement belongs to the session rather than to the transaction, so the rollback keeps it, and
// it is deallocated by name on connection after it, as GenericPlan's is.
func AnalyzedGenericPlan(
	t testing.TB,
	ctx context.Context,
	connection *pgxpool.Conn,
	name, statement string,
	parameters, literals []string,
	settings ...string,
) json.RawMessage {
	t.Helper()
	return explainGenericPlan(t, ctx, connection, "analyze, buffers, format json", false, name, statement, parameters, literals, settings)
}

// AnalyzedPlan is the plan Postgres makes for statement with the values arguments, run and
// measured (EXPLAIN (ANALYZE, BUFFERS)) under settings, the `set local` statements the caller
// names (none: the planner's default settings), in a transaction of db that it rolls back. An
// EXPLAIN that binds its own parameters is planned for their values, a custom plan, the plan
// AnalyzedGenericPlan's forced mode leaves out.
func AnalyzedPlan(t testing.TB, ctx context.Context, db Beginner, statement string, settings []string, arguments ...any) json.RawMessage {
	t.Helper()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the plan's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	for _, setting := range settings {
		if _, err := tx.Exec(ctx, setting); err != nil {
			t.Fatalf("%s: %v", setting, err)
		}
	}
	var raw []byte
	if err := tx.QueryRow(ctx, "explain (analyze, buffers, format json) "+statement, arguments...).Scan(&raw); err != nil {
		t.Fatalf("explain the statement: %v", err)
	}
	return OnePlan(t, raw)
}

// Failures is the testing.TB a control's plan is run through a check by (RequireControlFails). It
// keeps each failure that the check reports through Errorf rather than failing the test, so the
// test can hold the check to failing the control. Every other call goes to the test, a fatal
// failure of the test's own making among them.
type Failures struct {
	testing.TB
	failures []string
}

func (f *Failures) Errorf(format string, args ...any) {
	f.failures = append(f.failures, fmt.Sprintf(format, args...))
}

// FirstLines is the first line of each failure, which names the read without the plan it prints.
func (f *Failures) FirstLines() []string {
	lines := make([]string, 0, len(f.failures))
	for _, failure := range f.failures {
		line, _, _ := strings.Cut(failure, "\n")
		lines = append(lines, line)
	}
	return lines
}

// RequireControlFails runs check, a check of plan that reports each failure through Errorf, on
// Failures, and fails t where the check reports none, since the check would then be one that
// cannot fail. subject names the plan, control names the plan's part ("the control", or "a
// control" where there are several), and cannotTell is what such a check cannot tell apart: the
// failure says all three and prints plan. Where the check fails, as the control must, it logs the
// first line of each failure.
func RequireControlFails(t testing.TB, subject, control, cannotTell string, plan json.RawMessage, check func(testing.TB)) {
	t.Helper()
	failures := &Failures{TB: t}
	check(failures)
	lines := failures.FirstLines()
	if len(lines) == 0 {
		t.Errorf("%s, %s, passes the check, so the check cannot tell %s:\n%s", subject, control, cannotTell, plan)
		return
	}
	t.Logf("%s fails the check, as the control must:\n  %s", subject, strings.Join(lines, "\n  "))
}

// explainGenericPlan is the body GenericPlan and AnalyzedGenericPlan share: it prepares statement
// under name, explains its EXECUTE with options under settings and plan_cache_mode
// force_generic_plan, commits the transaction it does that in where commit is set and rolls it
// back where it is not, deallocates the statement, and answers the plan.
func explainGenericPlan(
	t testing.TB,
	ctx context.Context,
	connection *pgxpool.Conn,
	options string,
	commit bool,
	name, statement string,
	parameters, literals, settings []string,
) json.RawMessage {
	t.Helper()
	tx, err := connection.Begin(ctx)
	if err != nil {
		t.Fatalf("begin %s's transaction: %v", name, err)
	}
	defer tx.Rollback(ctx)
	for _, setting := range append(slices.Clone(settings), "set local plan_cache_mode = force_generic_plan") {
		if _, err := tx.Exec(ctx, setting); err != nil {
			t.Fatalf("%s: %v", setting, err)
		}
	}
	if _, err := tx.Exec(ctx, "prepare "+name+" ("+strings.Join(parameters, ", ")+") as "+statement); err != nil {
		t.Fatalf("prepare %s: %v", name, err)
	}
	var raw []byte
	if err := tx.QueryRow(ctx, "explain ("+options+") execute "+name+"("+strings.Join(literals, ", ")+")").Scan(&raw); err != nil {
		t.Fatalf("explain %s: %v", name, err)
	}
	if commit {
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit %s's transaction: %v", name, err)
		}
	} else if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll back %s's transaction: %v", name, err)
	}
	if _, err := connection.Exec(ctx, "deallocate "+name); err != nil {
		t.Fatalf("deallocate %s: %v", name, err)
	}
	return OnePlan(t, raw)
}
