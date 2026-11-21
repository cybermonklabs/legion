package refs

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// mainClosureQuery is main's closure statement at commit d36289cb84b1, the eight-hop walk the
// references read ran there, held here letter for letter as the oracle of the comparisons below.
// Every fragment it was built from (documentOfTarget, documentBlockHeld, documentTarget, and the
// from and the where of its read of an ask's current parts) is written out, so it names no
// identifier of the package: a statement built from those would move with them, and the two
// sides would agree however the reach of the read changed.
const mainClosureQuery = `
		with recursive closure as (
			select case when r.to_kind = 'block' then split_part(r.to_id, '#', 1) else r.to_id end as ref_key, 1 as depth, r.from_kind as via_kind, r.from_id as via_id
			from refs r
			left join lateral (
				select 1 as held from document_blocks d
				where r.to_kind = 'block' and d.node_id = r.to_id
				limit 1
			) block_target on true
			where (r.to_kind in ('artifact', 'block') and (r.to_kind = 'artifact' or block_target.held is not null)) and (
				  (r.from_kind = 'ask'      and r.from_id in (select id::text from asks      where issue_key = $1))
				or (r.from_kind = 'comment'  and r.from_id in (select id::text from comments  where issue_key = $1))
				or (r.from_kind = 'message'  and r.from_id in (select id::text from messages  where issue_key = $1))
				or (r.from_kind = 'artifact' and r.from_id in (select id::text from artifacts where issue_key = $1))
			)
			union
			select case when r.to_kind = 'block' then split_part(r.to_id, '#', 1) else r.to_id end, 1, 'ask', r.ask_id::text
		from ask_block_refs r
		join ask_blocks b on b.ask_id = r.ask_id and b.block_id = r.block_id
			left join lateral (
				select 1 as held from document_blocks d
				where r.to_kind = 'block' and d.node_id = r.to_id
				limit 1
			) block_target on true
		where b.position is not null
			  and r.ask_id in (select id from asks where issue_key = $1)
			  and (r.to_kind in ('artifact', 'block') and (r.to_kind = 'artifact' or block_target.held is not null))
			union
			select case when r.to_kind = 'block' then split_part(r.to_id, '#', 1) else r.to_id end, c.depth + 1, 'artifact', a.id::text
			from closure c
			join artifacts a on a.ref_key = c.ref_key
			join refs r on r.from_kind = 'artifact' and r.from_id = a.id::text
			left join lateral (
				select 1 as held from document_blocks d
				where r.to_kind = 'block' and d.node_id = r.to_id
				limit 1
			) block_target on true
			where c.depth < 9 and (r.to_kind in ('artifact', 'block') and (r.to_kind = 'artifact' or block_target.held is not null))
		)
		select distinct on (c.ref_key) c.ref_key, c.depth, c.via_kind, c.via_id
		from closure c
		join artifacts a on a.ref_key = c.ref_key
		where a.issue_key is distinct from $1
		order by c.ref_key, c.depth, c.via_kind, c.via_id
	`

// closureRow is one row of the closure statement, before Closure reduces it to a Member.
type closureRow struct {
	refKey  string
	depth   int
	viaKind string
	viaID   string
}

// closureFixture is the data both tests below read: an issue whose comment cites a document and
// two document blocks, one of which no current version holds, and a chain of documents ten deep,
// which main's walk follows to its ninth hop and the one-hop read to its first document alone.
type closureFixture struct {
	database *store.Store
	// refKeys names the ref_key of each document the closure reaches, in the order the walk
	// reaches them: the comment's own targets first, then the chain.
	heldBlockDocument string
	droppedBlock      string
	comment           string
}

// newClosureFixture writes the fixture and commits it, so that the plan test can read the
// statement's plan on a connection of its own.
func newClosureFixture(t *testing.T) closureFixture {
	t.Helper()
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the closure fixture: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "CORE")
	createReferenceIssue(t, tx, "CORE-1", "CORE")
	createReferenceArtifact(t, tx, "CORE-1", "CORE", "spec")

	// A chain of ten documents: the comment reaches the first, and each cites the next, so main's
	// walk reaches the tenth at depth ten, where the one-hop read reaches the first alone.
	ids := make(map[string]string)
	refs := make(map[string]string)
	for index := 1; index <= 10; index++ {
		slug := "d" + strconv.Itoa(index)
		ids[slug], refs[slug] = createReferenceArtifact(t, tx, "", "CORE", slug)
	}
	for index := 1; index < 10; index++ {
		insertReference(t, tx, "artifact", ids["d"+strconv.Itoa(index)], "artifact", refs["d"+strconv.Itoa(index+1)])
	}

	// Two block targets of the same shape, one held by a current version of its document and
	// one not: the held one reaches its document and the dropped one reaches nothing, which is
	// the answer the document_blocks probe decides.
	held, heldRef := createReferenceArtifact(t, tx, "", "CORE", "held-md")
	_, droppedRef := createReferenceArtifact(t, tx, "", "CORE", "dropped-md")
	insertDocumentBlock(t, tx, held, "b1", heldRef+"#b1")
	comment := createReferenceComment(t, tx, "CORE-1")
	insertReference(t, tx, "comment", comment, "artifact", refs["d1"])
	insertReference(t, tx, "comment", comment, "block", heldRef+"#b1")
	insertReference(t, tx, "comment", comment, "block", droppedRef+"#b1")
	// The recursive term meets a block target too: the first document cites a block of the
	// held document, and a block of the dropped one.
	insertReference(t, tx, "artifact", ids["d1"], "block", heldRef+"#b1")
	insertReference(t, tx, "artifact", ids["d1"], "block", droppedRef+"#b1")
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the closure fixture: %v", err)
	}
	return closureFixture{database: database, heldBlockDocument: heldRef, droppedBlock: droppedRef, comment: comment}
}

func insertDocumentBlock(t *testing.T, tx pgx.Tx, artifactID, blockID, nodeID string) {
	t.Helper()
	if _, err := tx.Exec(context.Background(), `
		insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
		values ($1::uuid, $2, $3, 'paragraph', 'sha', 'excerpt', 1, now())
	`, artifactID, blockID, nodeID); err != nil {
		t.Fatalf("index block %q: %v", nodeID, err)
	}
}

// closureStatementRows runs statement and answers its rows in the order it gave them.
func closureStatementRows(t *testing.T, database *store.Store, statement, issueKey string) []closureRow {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), statement, issueKey)
	if err != nil {
		t.Fatalf("run the closure statement: %v", err)
	}
	defer rows.Close()
	var got []closureRow
	for rows.Next() {
		var row closureRow
		if err := rows.Scan(&row.refKey, &row.depth, &row.viaKind, &row.viaID); err != nil {
			t.Fatalf("scan a closure row: %v", err)
		}
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the closure rows: %v", err)
	}
	return got
}

// Closure's members are main's rows of depth 1 over the same data, each at depth 1 with main's
// via: a block its document still holds reaches that document, one no version holds reaches
// nothing, and the documents of the chain past its first, which main's walk answered at depths
// two to ten, are no member.
func TestTheClosureAnswersWhatMainsStatementDid(t *testing.T) {
	fixture := newClosureFixture(t)
	wantMembers := mainsDepthOneMembers(t, fixture.database, "CORE-1")
	references, err := Closure(context.Background(), fixture.database.Pool, "CORE-1")
	if err != nil {
		t.Fatalf("read the closure: %v", err)
	}
	members := references.Members
	if !reflect.DeepEqual(members, wantMembers) {
		t.Fatalf("Closure = %#v; want main's rows of depth 1 %#v", members, wantMembers)
	}
	for _, member := range members {
		if member.RefKey == fixture.droppedBlock {
			t.Errorf("the closure holds %s, whose only citation names a block no current version holds", fixture.droppedBlock)
		}
	}
}

// The out statement of the read, the one statement of it that reduces a link to a document's
// block to its document, reaches document_blocks through document_blocks_node_id, the hash index
// on exactly the column it probes, and never reads that table whole. An `exists` probe written in
// the predicate may be planned as a hashed subplan instead, which reads all of document_blocks to
// build its hash and cannot use that index at all; the lateral join is what keeps the probe a
// keyed one. Every read of refs is keyed too.
func TestTheClosureProbesDocumentBlocksThroughItsNodeIndex(t *testing.T) {
	fixture := newClosureFixture(t)
	ctx := context.Background()
	tx, err := fixture.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the plan's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	// A fixture this small is read fastest whole, so sequential scans are off: what is left is
	// the index each read is written for, and a read no index serves still shows as a scan
	// carrying no index condition.
	if _, err := tx.Exec(ctx, "set local enable_seqscan = off"); err != nil {
		t.Fatalf("set local enable_seqscan = off: %v", err)
	}
	var raw []byte
	if err := tx.QueryRow(ctx, "explain (format json) "+linksOutQuery, "CORE-1").Scan(&raw); err != nil {
		t.Fatalf("explain the out statement: %v", err)
	}
	plan := explaintest.OnePlan(t, raw)

	if explaintest.SeqScansRelation(t, plan, "document_blocks") {
		t.Errorf("the closure reads document_blocks whole:\n%s", plan)
	}
	explaintest.NewIndexKeys(fixture.database.Pool).RequireIndexKeyed(t, plan,
		"document_blocks_node_id", "the closure", explaintest.CondRequired)
	for _, scan := range explaintest.IndexScans(t, plan) {
		if scan.Relation == "refs" && scan.Cond == "" {
			t.Errorf("the closure reads %s with no index condition, so it reads every refs row:\n%s", scan, plan)
		}
	}
	if explaintest.SeqScansRelation(t, plan, "refs") {
		t.Errorf("the closure reads refs whole:\n%s", plan)
	}
}

// head is the statement's first line of SQL after any leading comment, which names it in a log.
func (s recordedStatement) head() string {
	for line := range strings.Lines(s.sql) {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "/*") {
			return line
		}
	}
	return ""
}

// oneHopBound is the bound the references route reads an issue's references under
// (api.referencesClosureBound, cybermonklabs/legion#4554), written as `set local statement_timeout`
// takes it.
const oneHopBound = "2s"

// linkTables is every table a link is read from: a mention of a document, an ask, a comment or a
// message in refs, and a mention or a typed pointer of one of their blocks in the other four.
var linkTables = []string{"refs", "block_refs", "ask_block_refs", "comment_block_refs", "message_block_refs"}

// joinNodeTypes is every plan node that can carry a Join Filter.
var joinNodeTypes = []string{"Nested Loop", "Hash Join", "Merge Join"}

// artifactProbeKeys is every column a read of artifacts may probe by: a document's ref_key, its id
// (as the uuid or as its text) and the issue's key.
var artifactProbeKeys = []string{"ref_key", "id", "issue_key"}

// planCounts is what TestTheReferencesReadReadsOnlyTheIssuesOwnLinks reads of analyzed plans.
// linkRows is its first count: the rows every scan of a link table answered and its Filter
// removed, over every loop. nodeRows is its second: the rows every node answered and its Filter
// and Join Filter removed, over every loop. Each margin is the precision of the plan's own
// figures: 0.005 for each loop of an Actual Rows, which EXPLAIN rounds to two decimals, and 0.5
// for each loop of a Rows Removed, which it rounds to a whole number. unkeyed is its third: each
// read of a table that is no probe of an index by its first key column.
type planCounts struct {
	linkRows, linkMargin float64
	nodeRows, nodeMargin float64
	unkeyed              []string
	relations            []string
}

func (c *planCounts) add(other planCounts) {
	c.linkRows += other.linkRows
	c.linkMargin += other.linkMargin
	c.nodeRows += other.nodeRows
	c.nodeMargin += other.nodeMargin
	c.unkeyed = append(c.unkeyed, other.unkeyed...)
	for _, relation := range other.relations {
		if !slices.Contains(c.relations, relation) {
			c.relations = append(c.relations, relation)
		}
	}
}

// countPlan reads the three counts of one analyzed plan. A table is read keyed when an Index Scan
// or an Index Only Scan of it, or each Bitmap Index Scan under a Bitmap Heap Scan of it, carries
// an Index Cond naming its index's first key column; artifacts only by ref_key, id or the issue's
// key. Any other scan of a relation, a sequential scan first of all, is unkeyed.
func countPlan(t *testing.T, keys *explaintest.IndexKeys, plan json.RawMessage) planCounts {
	t.Helper()
	var counts planCounts
	var bitmapRelations []string
	for _, node := range explaintest.Nodes(t, plan) {
		rowsMargin := 0.005 * node.Loops
		var filterMargin, joinMargin float64
		if node.Filter != "" {
			filterMargin = 0.5 * node.Loops
		}
		if slices.Contains(joinNodeTypes, node.Type) {
			joinMargin = 0.5 * node.Loops
		}
		counts.nodeRows += (node.Rows + node.RowsRemovedByFilter + node.RowsRemovedByJoinFilter) * node.Loops
		counts.nodeMargin += rowsMargin + filterMargin + joinMargin
		if slices.Contains(linkTables, node.Relation) {
			counts.linkRows += (node.Rows + node.RowsRemovedByFilter) * node.Loops
			counts.linkMargin += rowsMargin + filterMargin
		}
		if node.Relation == "" {
			continue
		}
		if !slices.Contains(counts.relations, node.Relation) {
			counts.relations = append(counts.relations, node.Relation)
		}
		switch node.Type {
		case "Index Scan", "Index Only Scan":
			if problem := unkeyedProbe(t, keys, node.Relation, node.String(), node.Index, node.IndexCond); problem != "" {
				counts.unkeyed = append(counts.unkeyed, problem)
			}
		case "Bitmap Heap Scan":
			if !slices.Contains(bitmapRelations, node.Relation) {
				bitmapRelations = append(bitmapRelations, node.Relation)
			}
		default:
			counts.unkeyed = append(counts.unkeyed, node.String()+" reads its table through no index")
		}
	}
	for _, relation := range bitmapRelations {
		for _, scan := range explaintest.BitmapHeapScans(t, plan, relation) {
			if len(scan.BitmapIndexScans) == 0 {
				counts.unkeyed = append(counts.unkeyed, scan.Scan.String()+" reads a bitmap no index scan builds")
			}
			for _, index := range scan.BitmapIndexScans {
				if problem := unkeyedProbe(t, keys, relation, scan.Scan.String()+" through "+index.String(), index.Index, index.IndexCond); problem != "" {
					counts.unkeyed = append(counts.unkeyed, problem)
				}
			}
		}
	}
	return counts
}

// unkeyedProbe says why a scan of relation through index under cond is no keyed probe, or "" when
// it is one.
func unkeyedProbe(t *testing.T, keys *explaintest.IndexKeys, relation, scan, index, cond string) string {
	t.Helper()
	if cond == "" {
		return scan + " carries no Index Cond, so it reads its whole index"
	}
	firstKey := keys.FirstKey(t, index)
	if !explaintest.CondNamesFirstKey(cond, firstKey) {
		return fmt.Sprintf("%s reads on `%s`, which names nothing of its index's first key column `%s`", scan, cond, firstKey)
	}
	if relation == "artifacts" && !slices.ContainsFunc(artifactProbeKeys, func(key string) bool {
		return explaintest.CondNamesFirstKey(key, firstKey)
	}) {
		return fmt.Sprintf("%s probes artifacts by `%s`, not by ref_key, id or the issue's key", scan, firstKey)
	}
	return ""
}

// boundedPlan explains statement with ANALYZE, in a transaction of its own under a
// statement_timeout of timeout and settings, and answers its plan, or the error that ended it.
// TIMING OFF leaves the rows and loops every count reads, and spares the clock reads of each row.
func boundedPlan(t *testing.T, database *store.Store, statement recordedStatement, timeout string, settings ...string) (json.RawMessage, error) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the plan's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	for _, setting := range append([]string{"set local statement_timeout = '" + timeout + "'"}, settings...) {
		if _, err := tx.Exec(ctx, setting); err != nil {
			t.Fatalf("%s: %v", setting, err)
		}
	}
	var raw []byte
	if err := tx.QueryRow(ctx, "explain (analyze, timing off, format json) "+statement.sql, statement.args...).Scan(&raw); err != nil {
		return nil, err
	}
	return explaintest.OnePlan(t, raw), nil
}

// queryCanceled is whether err is a statement Postgres cancelled, as statement_timeout does
// (SQLSTATE 57014).
func queryCanceled(err error) bool {
	var pgError *pgconn.PgError
	return errors.As(err, &pgError) && pgError.Code == "57014"
}

// oneHopLayers is the number of documents at each depth of INFRA-1098's closure on 2026-11-20 at
// 01:32Z, depth 1 first: the closure the walk this read replaces held the database with.
var oneHopLayers = []int{25, 97, 112, 391, 1307, 2108, 1808, 1264}

// oneHopMinimumRows is the fewest rows each table a recorded statement reads holds, so that the
// planner plans the default settings on tables of a real size, most of whose rows are outside the
// issue.
const oneHopMinimumRows = 10000

// fixtureTable is one table a bulk fixture writes and the columns it writes of it.
type fixtureTable struct {
	name    string
	columns []string
}

// copyFixture copies each table's rows in one COPY, in the order of tables, each after every table
// its foreign keys name, with every user trigger of those tables off, then commits and analyzes
// them, so the planner plans the default settings from the tables' real sizes. The triggers a
// write of an item, an issue, a version or a link fires (its search text, the queues of owed work,
// the embeddings) read nothing a references read reads. One test login owns every table, so it
// turns them off around the copy, as embedqueue's withTableTriggersOff does, and the copy writes
// the ref_key the artifacts trigger would.
func copyFixture(t *testing.T, database *store.Store, tables []fixtureTable, rows map[string][][]any) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the fixture: %v", err)
	}
	defer tx.Rollback(ctx)
	names := make([]string, len(tables))
	for index, table := range tables {
		names[index] = table.name
		if _, err := tx.Exec(ctx, "alter table "+table.name+" disable trigger user"); err != nil {
			t.Fatalf("turn off the triggers of %s: %v", table.name, err)
		}
	}
	for _, table := range tables {
		if len(rows[table.name]) == 0 {
			continue
		}
		copied, err := tx.CopyFrom(ctx, pgx.Identifier{table.name}, table.columns, pgx.CopyFromRows(rows[table.name]))
		if err != nil {
			t.Fatalf("copy %d rows into %s: %v", len(rows[table.name]), table.name, err)
		}
		if copied != int64(len(rows[table.name])) {
			t.Fatalf("%d of %d rows landed in %s", copied, len(rows[table.name]), table.name)
		}
	}
	for _, name := range names {
		if _, err := tx.Exec(ctx, "alter table "+name+" enable trigger user"); err != nil {
			t.Fatalf("turn on the triggers of %s: %v", name, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the fixture: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, "analyze "+strings.Join(names, ", ")); err != nil {
		t.Fatalf("analyze the fixture: %v", err)
	}
}

// oneHopTables is every table the one-hop fixture writes and the columns it writes of each, in
// the order it copies them.
var oneHopTables = []fixtureTable{
	{"projects", []string{"key", "name", "default_assignee"}},
	{"architecture_sources", []string{"project_key", "id", "repo", "branch", "installation_id", "created_by"}},
	{"architecture_snapshots", []string{"id", "project_key", "commit", "files"}},
	{"issues", []string{"key", "project_key", "number", "title", "created_by", "rank", "parent_key"}},
	{"events", []string{"issue_key", "seq", "type", "actor", "payload", "notify", "published_at"}},
	{"artifacts", []string{"id", "issue_key", "project_key", "slug", "name", "kind", "created_by", "ref_key"}},
	{"artifact_versions", []string{"artifact_id", "number", "authors"}},
	{"document_indexed_versions", []string{"artifact_id", "version"}},
	{"document_blocks", []string{"artifact_id", "block_id", "node_id", "type", "text_sha256", "excerpt", "changed_version", "changed_at"}},
	{"document_block_texts", []string{"artifact_id", "block_id", "version", "text"}},
	{"asks", []string{"id", "issue_key", "author", "question"}},
	{"ask_blocks", []string{"ask_id", "block_id", "node_id", "position", "type", "text_sha256", "excerpt", "changed_revision", "changed_at"}},
	{"comments", []string{"id", "issue_key", "author", "body"}},
	{"comment_blocks", []string{"comment_id", "block_id", "node_id", "position", "type", "text_sha256", "excerpt", "changed_revision", "changed_at"}},
	{"messages", []string{"id", "issue_key", "author", "body"}},
	{"message_blocks", []string{"message_id", "block_id", "node_id", "position", "type", "text_sha256", "excerpt", "made_at", "changed_at"}},
	{"components", []string{"project_key", "id", "title", "prose", "parent", "snapshot_id", "source_id"}},
	{"component_facts", []string{"project_key", "id", "made_at", "changed_at"}},
	{"refs", []string{"from_kind", "from_id", "to_kind", "to_id", "kind"}},
	{"block_refs", []string{"artifact_id", "block_id", "from_node", "to_kind", "to_id", "kind", "created_version", "to_types"}},
	{"ask_block_refs", []string{"ask_id", "block_id", "from_node", "to_kind", "to_id", "kind"}},
	{"comment_block_refs", []string{"comment_id", "block_id", "from_node", "to_kind", "to_id", "kind"}},
	{"message_block_refs", []string{"message_id", "block_id", "from_node", "to_kind", "to_id", "kind"}},
}

// The actor every row of the one-hop fixture is made by, and the authors of every version.
const (
	fixtureActor   = `{"kind":"user","id":"alice"}`
	fixtureAuthors = `[{"kind":"user","id":"alice"}]`
)

// fixtureDocument is one document of a fixture: its id, its ref_key, which a link to it stores,
// and its name, the words the graph read shows for it.
type fixtureDocument struct {
	id     uuid.UUID
	refKey string
	name   string
}

// fixtureBlock is one block a fixture document holds: its id and its type.
type fixtureBlock struct{ id, kind string }

// relatedRows gathers the rows one field of the references read must answer, by the node at the
// far end: each with the distinct kinds of its links and the lowest via of their near ends.
type relatedRows map[[2]string]*relatedRow

type relatedRow struct {
	name  string
	kinds []string
	via   model.ReferenceVia
}

func (rows relatedRows) add(kind, id, name, linkKind string, via model.ReferenceVia) {
	key := [2]string{kind, id}
	row, found := rows[key]
	if !found {
		rows[key] = &relatedRow{name: name, kinds: []string{linkKind}, via: via}
		return
	}
	if !slices.Contains(row.kinds, linkKind) {
		row.kinds = append(row.kinds, linkKind)
		slices.Sort(row.kinds)
	}
	if viaBefore(via, row.via) {
		row.via = via
	}
}

// summaries is every row as relatedSummary writes it, sorted.
func (rows relatedRows) summaries() []string {
	summaries := make([]string, 0, len(rows))
	for key, row := range rows {
		summaries = append(summaries, relatedSummary(key[0], key[1], row.name, row.kinds, row.via))
	}
	slices.Sort(summaries)
	return summaries
}

// viaBefore is whether via a sorts before via b in the database's order: by kind, then by id.
// Every id the fixtures compare within one kind is a uuid, an issue key or a ref_key that differ
// first in a letter or a digit, which every collation orders as bytes do.
func viaBefore(a, b model.ReferenceVia) bool {
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	return a.ID < b.ID
}

// relatedSummary is one row of links or linked_from as the tests compare it.
func relatedSummary(kind, id, name string, kinds []string, via model.ReferenceVia) string {
	return fmt.Sprintf("%s %s %q [%s] via %s %s", kind, id, name, strings.Join(kinds, ","), via.Kind, via.ID)
}

// lineDiff says, of two sorted lists, how many lines each holds that the other does not, and the
// first few of each.
func lineDiff(got, want []string) string {
	var missing, unexpected []string
	for _, line := range want {
		if _, found := slices.BinarySearch(got, line); !found {
			missing = append(missing, line)
		}
	}
	for _, line := range got {
		if _, found := slices.BinarySearch(want, line); !found {
			unexpected = append(unexpected, line)
		}
	}
	first := func(lines []string) string {
		if len(lines) > 5 {
			return strings.Join(lines[:5], "; ") + "; …"
		}
		return strings.Join(lines, "; ")
	}
	return fmt.Sprintf("%d missing [%s], %d unexpected [%s]", len(missing), first(missing), len(unexpected), first(unexpected))
}

// memberLines is each member as "<ref_key> depth <depth> via <kind> <id>", sorted.
func memberLines(members []Member) []string {
	lines := make([]string, len(members))
	for index, member := range members {
		lines[index] = fmt.Sprintf("%s depth %d via %s %s", member.RefKey, member.Depth, member.Via.Kind, member.Via.ID)
	}
	slices.Sort(lines)
	return lines
}

// oneHopFixture is the graph of INFRA-1098's shape that the references read is held to.
type oneHopFixture struct {
	database *store.Store
	issue    string
	// links and linkedFrom are the rows each field must hold, as relatedSummary writes them,
	// sorted; members is the documents of links, each at depth 1 with its via, by ref_key.
	links, linkedFrom []string
	members           []Member
	// own is every node of the issue and its own content as a graph node names it, which neither
	// field may list.
	own map[[2]string]bool
	// reached is the ref_key of each document of the first layer, every one of which the issue's
	// own content links to.
	reached []string
	// namedKinds is the number of node kinds the two fields name, each of which the read may
	// send one statement to name.
	namedKinds int
}

// newOneHopFixture writes, in bulk and with every user trigger of the tables it writes off, the
// graph of INFRA-1098's closure on 2026-11-20 at 01:32Z, and commits and analyzes it.
//
// Its out side is eight layers of documents of the sizes of oneHopLayers. Document j of layer k+1
// is cited by document (j mod the size of layer k) of layer k, and every document cites three
// documents of the next layer and three of the earlier layers, chosen from a fixed seed. The first
// document of the first layer belongs to the issue's parent and holds a requirement block. The
// issue's own content cites the 25 documents of the first layer: a comment no block index has
// reached, a block of a comment, a message no block index has reached, a block of a message, an
// ask's question, an option part of the ask and the issue's two documents, and a check block of
// the issue's spec points with serves at the parent's requirement block. The comment also cites
// an issue of another project and its ask, the issue itself and the issue's spec.
//
// whole adds the rest of c-one-hop's fixture. The in side: 400 sources outside the issue, which
// write 600 links into the issue node and every kind of its own content; 30 of them are check
// blocks of the issue's children's documents pointing serves at its requirement blocks, and 200
// others write two links each. The outside graph: 10,000 issues of another project with their
// events, 3,000 of their documents citing the targets the issue's spec cites and each other,
// 10,000 asks, comments and messages, each with a block citing a document of the first layer or
// another of its kind, and 10,000 components with their facts, so that every table a recorded
// statement reads holds oneHopMinimumRows or more. Without whole, the check block's link is a
// plain mention of the requirement block from the spec's first block, since a fixture main's
// statement is compared on holds no typed pointer.
func newOneHopFixture(t *testing.T, whole bool) oneHopFixture {
	t.Helper()
	database := storetest.Open(t)
	built := time.Now()
	at := time.Date(2026, 11, 20, 1, 32, 0, 0, time.UTC)
	random := rand.New(rand.NewPCG(4704, 1098))
	newID := func() uuid.UUID {
		var id uuid.UUID
		binary.BigEndian.PutUint64(id[:8], random.Uint64())
		binary.BigEndian.PutUint64(id[8:], random.Uint64())
		id[6] = id[6]&0x0f | 0x40
		id[8] = id[8]&0x3f | 0x80
		return id
	}
	pgID := func(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }
	nullable := func(value string) any {
		if value == "" {
			return nil
		}
		return value
	}

	rows := map[string][][]any{}
	add := func(table string, values ...any) { rows[table] = append(rows[table], values) }
	written := map[[4]string]bool{}
	ref := func(fromKind, fromID, toKind, toID string) {
		key := [4]string{fromKind, fromID, toKind, toID}
		if !written[key] {
			written[key] = true
			add("refs", fromKind, fromID, toKind, toID, "mentions")
		}
	}
	names := map[[2]string]string{}
	issue := func(key, project string, number int, title, parent string) {
		add("issues", key, project, number, title, fixtureActor, fmt.Sprintf("%s%08d", project, number), nullable(parent))
		if whole {
			add("events", key, 1, "issue.created", fixtureActor, "{}", false, at)
		}
		names[[2]string{"issue", key}] = title
	}
	documents := map[string]fixtureDocument{}
	document := func(owner, project, slug, name string, blocks ...fixtureBlock) fixtureDocument {
		prefix := project
		if owner != "" {
			prefix = owner
		}
		doc := fixtureDocument{id: newID(), refKey: prefix + "/" + slug, name: name}
		add("artifacts", pgID(doc.id), nullable(owner), project, slug, name, "doc", fixtureActor, doc.refKey)
		add("artifact_versions", pgID(doc.id), 1, fixtureAuthors)
		add("document_indexed_versions", pgID(doc.id), 1)
		for _, block := range blocks {
			add("document_blocks", pgID(doc.id), block.id, doc.refKey+"#"+block.id, block.kind, "sha", name, 1, at)
			add("document_block_texts", pgID(doc.id), block.id, 1, name)
		}
		documents[doc.refKey] = doc
		return doc
	}
	// docLink writes a link of block of doc: the block's row of block_refs, of kind (mentions, or
	// a pointer's kind with the types it allows), and the mention the document's own text holds. A
	// link written twice is one row of each.
	blockLinks := map[[5]string]bool{}
	docLink := func(doc fixtureDocument, block, kind, toKind, toID string, toTypes []string) {
		ref("artifact", doc.id.String(), toKind, toID)
		key := [5]string{doc.id.String(), block, toKind, toID, kind}
		if blockLinks[key] {
			return
		}
		blockLinks[key] = true
		var types any
		if toTypes != nil {
			types = toTypes
		}
		add("block_refs", pgID(doc.id), block, doc.refKey+"#"+block, toKind, toID, kind, 1, types)
	}
	// item writes an ask, a comment or a message of issueKey with one block for each of parts,
	// each of that part's type, b1 first.
	item := func(kind ItemKind, issueKey, words string, parts ...string) uuid.UUID {
		id := newID()
		tables, err := kind.tables()
		if err != nil {
			t.Fatal(err)
		}
		add(tables.items, pgID(id), issueKey, fixtureActor, words)
		for position, part := range parts {
			blockID := "b" + strconv.Itoa(position+1)
			node := string(kind) + "/" + id.String() + "#" + blockID
			if kind == ItemMessage {
				add(tables.blocks, pgID(id), blockID, node, position, part, "sha", words, at, at)
			} else {
				add(tables.blocks, pgID(id), blockID, node, position, part, "sha", words, 1, at)
			}
		}
		names[[2]string{string(kind), id.String()}] = words
		return id
	}
	// itemLink writes a mention of the item's block (none: of the item's own row alone): the
	// block's row, and the item's own refs row, which a comment and a message keep for every
	// block's mentions and an ask for its question's alone.
	itemLink := func(kind ItemKind, id uuid.UUID, block, toKind, toID string) {
		if block == "" || kind != ItemAsk || block == "b1" {
			ref(string(kind), id.String(), toKind, toID)
		}
		if block != "" {
			tables, err := kind.tables()
			if err != nil {
				t.Fatal(err)
			}
			add(tables.refs, pgID(id), block, string(kind)+"/"+id.String()+"#"+block, toKind, toID, "mentions")
		}
	}

	add("projects", "CORE", "CORE project", "alice")
	add("projects", "OPS", "OPS project", "alice")
	issue("CORE-1", "CORE", 1, "The parent", "")
	issue("CORE-2", "CORE", 2, "The issue", "CORE-1")
	others := 1
	if whole {
		for number := 3; number <= 32; number++ {
			issue("CORE-"+strconv.Itoa(number), "CORE", number, "Child "+strconv.Itoa(number), "CORE-2")
		}
		others = oneHopMinimumRows
	}
	for number := 1; number <= others; number++ {
		issue("OPS-"+strconv.Itoa(number), "OPS", number, "Other issue "+strconv.Itoa(number), "")
	}

	// The layers. Without whole, only the first layer's blocks are written: the links read
	// through a block are the issue's own content's, which reach the first layer alone.
	layers := make([][]fixtureDocument, len(oneHopLayers))
	for k, size := range oneHopLayers {
		for j := range size {
			var blocks []fixtureBlock
			if whole || k == 0 {
				blocks = []fixtureBlock{{"b1", "paragraph"}}
			}
			owner := ""
			if k == 0 && j == 0 {
				owner = "CORE-1"
				blocks = append(blocks, fixtureBlock{"r1", "requirement"})
			}
			layers[k] = append(layers[k], document(owner, "CORE", fmt.Sprintf("l%d-%d", k+1, j),
				fmt.Sprintf("Layer %d document %d", k+1, j), blocks...))
		}
	}
	cite := func(from, to fixtureDocument) { ref("artifact", from.id.String(), "artifact", to.refKey) }
	var earlier []fixtureDocument
	for k, layer := range layers {
		for j, doc := range layer {
			if k > 0 {
				cite(layers[k-1][j%len(layers[k-1])], doc)
				for range 3 {
					cite(doc, earlier[random.IntN(len(earlier))])
				}
			}
			if k+1 < len(layers) {
				for range 3 {
					cite(doc, layers[k+1][random.IntN(len(layers[k+1]))])
				}
			}
		}
		earlier = append(earlier, layer...)
	}
	first := layers[0]
	parentRequirement := first[0].refKey + "#r1"

	// The issue's own content.
	spec := document("CORE-2", "CORE", "spec", "The issue's spec",
		fixtureBlock{"b1", "paragraph"}, fixtureBlock{"r1", "requirement"}, fixtureBlock{"r2", "requirement"}, fixtureBlock{"chk", "check"})
	notes := document("CORE-2", "CORE", "notes", "The issue's notes", fixtureBlock{"b1", "paragraph"})
	question := item(ItemAsk, "CORE-2", "Which documents of the first layer does the issue rest on?", AskQuestionPart, AskOptionPart)
	second := item(ItemAsk, "CORE-2", "Is the issue's spec complete?", AskQuestionPart)
	unindexed := item(ItemComment, "CORE-2", "Cites the first layer, another project's issue and its ask, the issue and its spec.")
	indexed := item(ItemComment, "CORE-2", "Intro.\n\nCites the first layer.", "paragraph", "paragraph")
	plain := item(ItemMessage, "CORE-2", "Cites the first layer.")
	blocked := item(ItemMessage, "CORE-2", "Intro.\n\nCites the first layer too.", "paragraph", "paragraph")
	otherAsk := item(ItemAsk, "OPS-1", "The other project's ask", AskQuestionPart)
	own := map[[2]string]bool{
		{"issue", "CORE-2"}: true, {"artifact", spec.id.String()}: true, {"artifact", notes.id.String()}: true,
		{"ask", question.String()}: true, {"ask", second.String()}: true,
		{"comment", unindexed.String()}: true, {"comment", indexed.String()}: true,
		{"message", plain.String()}: true, {"message", blocked.String()}: true,
	}

	// Each link out of the issue's content, and the row of links it gives: a block at the far end
	// counts as its document, and a link to the issue or its own content gives none.
	links := relatedRows{}
	out := func(via model.ReferenceVia, kind, toKind, toID string) {
		farKind, farID, name := toKind, toID, names[[2]string{toKind, toID}]
		if toKind == "artifact" || toKind == "block" {
			doc := documents[strings.SplitN(toID, "#", 2)[0]]
			farKind, farID, name = "artifact", doc.id.String(), doc.name
		}
		if !own[[2]string{farKind, farID}] {
			links.add(farKind, farID, name, kind, via)
		}
	}
	specVia := model.ReferenceVia{Kind: "artifact", ID: spec.id.String()}
	for _, j := range []int{20, 21, 22, 23, 8} {
		docLink(spec, "b1", "mentions", "artifact", first[j].refKey, nil)
		out(specVia, "mentions", "artifact", first[j].refKey)
	}
	docLink(spec, "b1", "mentions", "block", first[24].refKey+"#b1", nil)
	out(specVia, "mentions", "block", first[24].refKey+"#b1")
	if whole {
		docLink(spec, "chk", "serves", "block", parentRequirement, []string{"requirement"})
		out(specVia, "serves", "block", parentRequirement)
	} else {
		docLink(spec, "b1", "mentions", "block", parentRequirement, nil)
		out(specVia, "mentions", "block", parentRequirement)
	}
	docLink(notes, "b1", "mentions", "artifact", first[3].refKey, nil)
	out(model.ReferenceVia{Kind: "artifact", ID: notes.id.String()}, "mentions", "artifact", first[3].refKey)
	itemLinks := func(kind ItemKind, id uuid.UUID, block string, js ...int) {
		for _, j := range js {
			itemLink(kind, id, block, "artifact", first[j].refKey)
			out(model.ReferenceVia{Kind: string(kind), ID: id.String()}, "mentions", "artifact", first[j].refKey)
		}
	}
	itemLinks(ItemComment, unindexed, "", 1, 2, 3, 4)
	itemLinks(ItemComment, indexed, "b2", 4, 5, 6, 7)
	itemLinks(ItemMessage, plain, "", 8, 9, 10)
	itemLinks(ItemMessage, blocked, "b2", 11, 12, 13)
	itemLinks(ItemAsk, question, "b1", 14, 15, 16)
	itemLinks(ItemAsk, question, "b2", 17, 18, 19)
	commentVia := model.ReferenceVia{Kind: "comment", ID: unindexed.String()}
	for _, target := range [][2]string{{"issue", "OPS-1"}, {"ask", otherAsk.String()}, {"issue", "CORE-2"}, {"artifact", spec.refKey}} {
		itemLink(ItemComment, unindexed, "", target[0], target[1])
		out(commentVia, "mentions", target[0], target[1])
	}

	linkedFrom := relatedRows{}
	if whole {
		// The in side: every target a link into the issue's content can end at, with the via a
		// row of linked_from names for it.
		artifactVia := func(doc fixtureDocument) model.ReferenceVia {
			return model.ReferenceVia{Kind: "artifact", ID: doc.refKey}
		}
		itemVia := func(kind ItemKind, id uuid.UUID) model.ReferenceVia {
			return model.ReferenceVia{Kind: string(kind), ID: id.String()}
		}
		blockOf := func(kind ItemKind, id uuid.UUID, block string) string {
			return string(kind) + "/" + id.String() + "#" + block
		}
		type ownTarget struct {
			kind, id string
			via      model.ReferenceVia
		}
		targets := []ownTarget{
			{"issue", "CORE-2", model.ReferenceVia{Kind: "issue", ID: "CORE-2"}},
			{"artifact", spec.refKey, artifactVia(spec)},
			{"artifact", notes.refKey, artifactVia(notes)},
			{"block", spec.refKey + "#b1", artifactVia(spec)},
			{"block", spec.refKey + "#r1", artifactVia(spec)},
			{"block", spec.refKey + "#r2", artifactVia(spec)},
			{"block", spec.refKey + "#chk", artifactVia(spec)},
			{"block", notes.refKey + "#b1", artifactVia(notes)},
			{"ask", question.String(), itemVia(ItemAsk, question)},
			{"ask", second.String(), itemVia(ItemAsk, second)},
			{"ask_block", blockOf(ItemAsk, question, "b1"), itemVia(ItemAsk, question)},
			{"ask_block", blockOf(ItemAsk, question, "b2"), itemVia(ItemAsk, question)},
			{"ask_block", blockOf(ItemAsk, second, "b1"), itemVia(ItemAsk, second)},
			{"comment", unindexed.String(), itemVia(ItemComment, unindexed)},
			{"comment", indexed.String(), itemVia(ItemComment, indexed)},
			{"comment_block", blockOf(ItemComment, indexed, "b1"), itemVia(ItemComment, indexed)},
			{"comment_block", blockOf(ItemComment, indexed, "b2"), itemVia(ItemComment, indexed)},
			{"message", plain.String(), itemVia(ItemMessage, plain)},
			{"message", blocked.String(), itemVia(ItemMessage, blocked)},
			{"message_block", blockOf(ItemMessage, blocked, "b1"), itemVia(ItemMessage, blocked)},
			{"message_block", blockOf(ItemMessage, blocked, "b2"), itemVia(ItemMessage, blocked)},
		}
		for source := range 400 {
			count := 1
			if source >= 30 && source < 230 {
				count = 2
			}
			picks := []ownTarget{targets[source*5%len(targets)], targets[(source*5+11)%len(targets)]}[:count]
			owner := "OPS-" + strconv.Itoa(source+2)
			switch {
			case source < 30:
				child := "CORE-" + strconv.Itoa(source+3)
				doc := document(child, "CORE", "checks", "Checks of "+child, fixtureBlock{"k1", "check"})
				requirement := []string{"r1", "r2"}[source%2]
				docLink(doc, "k1", "serves", "block", spec.refKey+"#"+requirement, []string{"requirement"})
				linkedFrom.add("artifact", doc.id.String(), doc.name, "serves", artifactVia(spec))
			case source < 150:
				doc := document(owner, "OPS", "cites", "The document of "+owner+" that cites the issue", fixtureBlock{"b1", "paragraph"})
				for _, pick := range picks {
					docLink(doc, "b1", "mentions", pick.kind, pick.id, nil)
					linkedFrom.add("artifact", doc.id.String(), doc.name, "mentions", pick.via)
				}
			case source < 220:
				words := "The ask of " + owner + " that cites the issue"
				id := item(ItemAsk, owner, words, AskQuestionPart, AskOptionPart)
				for index, pick := range picks {
					itemLink(ItemAsk, id, "b"+strconv.Itoa(index+1), pick.kind, pick.id)
					linkedFrom.add("ask", id.String(), words, "mentions", pick.via)
				}
			default:
				kind := ItemComment
				if source >= 310 {
					kind = ItemMessage
				}
				words := "The " + string(kind) + " of " + owner + " that cites the issue"
				id := item(kind, owner, words, "paragraph")
				for _, pick := range picks {
					itemLink(kind, id, "b1", pick.kind, pick.id)
					linkedFrom.add(string(kind), id.String(), words, "mentions", pick.via)
				}
			}
		}

		// The outside graph. Its documents cite the targets the issue's spec cites, so that a
		// read of block_refs or refs by target alone meets them, and each other.
		outside := make([]fixtureDocument, 3000)
		for index := range outside {
			owner := "OPS-" + strconv.Itoa(index+1)
			outside[index] = document(owner, "OPS", "d"+strconv.Itoa(index), "Outside document "+strconv.Itoa(index), fixtureBlock{"b1", "paragraph"})
		}
		for index, doc := range outside {
			docLink(doc, "b1", "mentions", "artifact", first[20+index%5].refKey, nil)
			docLink(doc, "b1", "mentions", "block", []string{first[24].refKey + "#b1", parentRequirement, first[8].refKey + "#b1"}[index%3], nil)
			docLink(doc, "b1", "mentions", "artifact", earlier[random.IntN(len(earlier))].refKey, nil)
			docLink(doc, "b1", "mentions", "artifact", outside[(index+1)%len(outside)].refKey, nil)
		}
		// Asks, comments and messages of the other project, each with a block that cites a
		// document of the first layer or another item of its kind.
		for _, kind := range []ItemKind{ItemAsk, ItemComment, ItemMessage} {
			part := "paragraph"
			if kind == ItemAsk {
				part = AskQuestionPart
			}
			ids := make([]uuid.UUID, oneHopMinimumRows)
			for index := range ids {
				ids[index] = item(kind, "OPS-"+strconv.Itoa(index+1), "Outside "+string(kind)+" "+strconv.Itoa(index), part)
			}
			for index, id := range ids {
				next := ids[(index+1)%len(ids)]
				switch index % 3 {
				case 0:
					itemLink(kind, id, "b1", "artifact", first[index%len(first)].refKey)
				case 1:
					itemLink(kind, id, "b1", kind.BlockKind(), string(kind)+"/"+next.String()+"#b1")
				default:
					itemLink(kind, id, "b1", string(kind), next.String())
				}
			}
		}
		add("architecture_sources", "OPS", int64(1), "ops/architecture", "main", int64(1), fixtureActor)
		add("architecture_snapshots", int64(1), "OPS", "c0ffee", "{}")
		for index := range oneHopMinimumRows {
			id := "c" + strconv.Itoa(index)
			parent := ""
			if index > 0 {
				parent = "c" + strconv.Itoa(index/2)
			}
			add("components", "OPS", id, "Component "+strconv.Itoa(index), "prose", nullable(parent), int64(1), int64(1))
			add("component_facts", "OPS", id, at, at)
		}
	}

	copyFixture(t, database, oneHopTables, rows)
	copiedRows := 0
	for _, table := range rows {
		copiedRows += len(table)
	}
	t.Logf("the one-hop fixture took %s to write, copy and analyze %d rows", time.Since(built).Round(time.Millisecond), copiedRows)

	fixture := oneHopFixture{
		database: database, issue: "CORE-2", own: own,
		links: links.summaries(), linkedFrom: linkedFrom.summaries(),
	}
	kinds := map[string]bool{}
	for key, row := range links {
		kinds[key[0]] = true
		if key[0] == "artifact" {
			for _, doc := range first {
				if doc.id.String() == key[1] {
					fixture.members = append(fixture.members, Member{RefKey: doc.refKey, Depth: 1, Via: row.via})
				}
			}
		}
	}
	for key := range linkedFrom {
		kinds[key[0]] = true
	}
	fixture.namedKinds = len(kinds)
	sort.Slice(fixture.members, func(left, right int) bool { return fixture.members[left].RefKey < fixture.members[right].RefKey })
	for _, doc := range first {
		fixture.reached = append(fixture.reached, doc.refKey)
	}
	return fixture
}

// oneHopSizes is what the fixture's two first counts are held to, counted by keyed reads of each
// table. out is the rows of the five link tables whose source is the issue's own content, and in
// their rows whose target is the issue or its own content, a row of both kinds counted in each;
// held is the mentions of either set that a block of their source holds, which heldByABlock reads
// again in that block's table; own is the issue and every node of its own content. A block or a
// part of an item is own content, and a source, while it is current.
type oneHopSizes struct{ out, in, held, own int }

func (f oneHopFixture) sizes(t *testing.T) oneHopSizes {
	t.Helper()
	var sizes oneHopSizes
	if err := f.database.Pool.QueryRow(context.Background(), `
		with own_documents as (select id, ref_key from artifacts where issue_key = $1),
		own_asks as (select id from asks where issue_key = $1),
		own_comments as (select id from comments where issue_key = $1),
		own_messages as (select id from messages where issue_key = $1),
		own_sources (kind, id) as (
			select 'artifact', id::text from own_documents
			union all select 'ask', id::text from own_asks
			union all select 'comment', id::text from own_comments
			union all select 'message', id::text from own_messages
		),
		own_targets (kind, id) as (
			select 'issue', $1
			union all select 'artifact', ref_key from own_documents
			union all select 'block', d.node_id from document_blocks d join own_documents o on o.id = d.artifact_id
			union all select 'ask', id::text from own_asks
			union all select 'ask_block', b.node_id from ask_blocks b join own_asks o on o.id = b.ask_id where b.position is not null
			union all select 'comment', id::text from own_comments
			union all select 'comment_block', b.node_id from comment_blocks b join own_comments o on o.id = b.comment_id where b.position is not null
			union all select 'message', id::text from own_messages
			union all select 'message_block', b.node_id from message_blocks b join own_messages o on o.id = b.message_id where b.position is not null
		),
		out_refs as (select r.* from refs r join own_sources s on s.kind = r.from_kind and s.id = r.from_id),
		in_refs as (select r.* from refs r join own_targets o on o.kind = r.to_kind and o.id = r.to_id)
		select
			(select count(*) from out_refs)
			+ (select count(*) from block_refs r join own_documents o on o.id = r.artifact_id)
			+ (select count(*) from ask_block_refs r join ask_blocks b on b.ask_id = r.ask_id and b.block_id = r.block_id
			   where b.position is not null and r.ask_id in (select id from own_asks))
			+ (select count(*) from comment_block_refs r join comment_blocks b on b.comment_id = r.comment_id and b.block_id = r.block_id
			   where b.position is not null and r.comment_id in (select id from own_comments))
			+ (select count(*) from message_block_refs r join message_blocks b on b.message_id = r.message_id and b.block_id = r.block_id
			   where b.position is not null and r.message_id in (select id from own_messages)),
			(select count(*) from in_refs)
			+ (select count(*) from block_refs r join own_targets o on o.kind = r.to_kind and o.id = r.to_id)
			+ (select count(*) from ask_block_refs r join own_targets o on o.kind = r.to_kind and o.id = r.to_id)
			+ (select count(*) from comment_block_refs r join own_targets o on o.kind = r.to_kind and o.id = r.to_id)
			+ (select count(*) from message_block_refs r join own_targets o on o.kind = r.to_kind and o.id = r.to_id),
			(select count(*) from (select * from out_refs union all select * from in_refs) r where
			   (r.from_kind = 'artifact' and exists (select 1 from block_refs b
			     where b.artifact_id::text = r.from_id and b.to_kind = r.to_kind and b.to_id = r.to_id))
			or (r.from_kind = 'ask' and exists (select 1 from ask_block_refs b
			     where b.ask_id::text = r.from_id and b.to_kind = r.to_kind and b.to_id = r.to_id))
			or (r.from_kind = 'comment' and exists (select 1 from comment_block_refs b
			     where b.comment_id::text = r.from_id and b.to_kind = r.to_kind and b.to_id = r.to_id))
			or (r.from_kind = 'message' and exists (select 1 from message_block_refs b
			     where b.message_id::text = r.from_id and b.to_kind = r.to_kind and b.to_id = r.to_id))),
			(select count(*) from own_targets)
	`, f.issue).Scan(&sizes.out, &sizes.in, &sizes.held, &sizes.own); err != nil {
		t.Fatalf("count the links of %s's own content: %v", f.issue, err)
	}
	return sizes
}

// checkAnswer holds the read's answer to the rows the fixture's links make.
func (f oneHopFixture) checkAnswer(t *testing.T, references References) {
	t.Helper()
	for _, field := range []struct {
		name string
		got  []Related
		want []string
	}{{"links", references.Links, f.links}, {"linked_from", references.LinkedFrom, f.linkedFrom}} {
		got := make([]string, 0, len(field.got))
		for _, row := range field.got {
			if f.own[[2]string{row.Node.Kind, row.Node.ID}] {
				t.Errorf("%s lists the issue's own %s %s", field.name, row.Node.Kind, row.Node.ID)
			}
			got = append(got, relatedSummary(row.Node.Kind, row.Node.ID, row.Name, row.Kinds, row.Via))
		}
		slices.Sort(got)
		if !slices.Equal(got, field.want) {
			t.Errorf("%s holds %d rows; want %d, one for each node at the far end of a link: %s",
				field.name, len(got), len(field.want), lineDiff(got, field.want))
		}
	}
	members := slices.Clone(references.Members)
	sort.Slice(members, func(left, right int) bool { return members[left].RefKey < members[right].RefKey })
	deeper := slices.DeleteFunc(slices.Clone(members), func(member Member) bool { return member.Depth == 1 })
	if len(deeper) > 0 {
		t.Errorf("members hold %d documents beyond depth 1, the first %s at depth %d; every member is at depth 1",
			len(deeper), deeper[0].RefKey, deeper[0].Depth)
	}
	if !reflect.DeepEqual(members, f.members) {
		t.Errorf("members hold %d documents; want the %d documents of links, each at depth 1 with its via: %s",
			len(members), len(f.members), lineDiff(memberLines(members), memberLines(f.members)))
	}
}

// The references read reads one hop in each direction, keyed by the issue and its own content,
// however large the graph beyond them. On a graph of the size and shape of INFRA-1098's closure
// (newOneHopFixture), Closure answers under the route's 2 s bound: its links hold the 25
// documents of the first layer, the other project's issue and its ask, each once with the kinds
// of its links and the lowest of its near ends as via, the parent's document by its pointer's kind
// alone; its linked_from holds each of the 400 sources outside the issue once, a block or a part
// as its document or item, with the lowest of its targets as via; neither lists the issue or its
// own content, and the members are the 25 documents, each at depth 1. It sends the out statement,
// the in statement, and at most one statement for each kind of node the two fields name.
//
// Each statement it sent is then explained, analyzed, under enable_seqscan off and under the
// default settings, and read for three counts (planCounts): the rows of the link tables it reads
// are at most R, the rows the fixture's link tables hold out of and into the issue's own content
// and the mentions a block holds (oneHopSizes), so a read of any link table not keyed to the
// issue's own sources or targets meets the outside graph and fails; the rows of every plan node
// are at most 20 times the issue's own content, its links out and in and the rows of both fields;
// and every table is read through an index by its first key column, artifacts by ref_key, id or
// the issue's key. Every table a statement reads holds oneHopMinimumRows or more, so the planner
// plans the default settings on tables of a real size.
//
// The control runs main's statement on the same fixture under the same bound: it must end at the
// bound (SQLSTATE 57014) or read ten times the rows of plan nodes the out statement reads, which
// shows the fixture is as hard as the real graph. Every count of every statement, and of main's,
// is printed.
func TestTheReferencesReadReadsOnlyTheIssuesOwnLinks(t *testing.T) {
	fixture := newOneHopFixture(t, true)
	ctx := context.Background()
	sizes := fixture.sizes(t)
	bound := sizes.out + sizes.in + sizes.held
	scale := sizes.own + sizes.out + sizes.in + len(fixture.links) + len(fixture.linkedFrom)
	t.Logf("R = %d: %d rows out of the issue's own content, %d rows into it, %d mentions a block holds; "+
		"count two's bound is 20 × %d (%d own nodes, the rows out and in, and %d rows of links and linked_from)",
		bound, sizes.out, sizes.in, sizes.held, scale, sizes.own, len(fixture.links)+len(fixture.linkedFrom))

	tx, err := fixture.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the read's transaction: %v", err)
	}
	if _, err := tx.Exec(ctx, "set local statement_timeout = '"+oneHopBound+"'"); err != nil {
		t.Fatalf("set the read's bound: %v", err)
	}
	recorder := &statementRecorder{Queryer: tx}
	read := time.Now()
	references, err := Closure(ctx, recorder, fixture.issue)
	took := time.Since(read)
	_ = tx.Rollback(ctx)
	if err != nil {
		t.Errorf("Closure did not answer within the read's %s bound (after %s): %v", oneHopBound, took.Round(time.Millisecond), err)
	} else {
		t.Logf("Closure answered in %s", took.Round(time.Millisecond))
	}
	fixture.checkAnswer(t, references)

	statements := recorder.statements
	for index, statement := range statements {
		t.Logf("statement %d: %s", index+1, statement.head())
	}
	if len(statements) < 2 || len(statements)-2 > fixture.namedKinds {
		t.Errorf("Closure sent %d statements; want the out statement, the in statement and at most one statement for each of the %d kinds of node it names",
			len(statements), fixture.namedKinds)
	}
	for index := range min(2, len(statements)) {
		sent := 0
		for _, statement := range statements {
			if statement.sql == statements[index].sql {
				sent++
			}
		}
		if sent > 1 {
			t.Errorf("Closure sent statement %d (%s) %d times; want once", index+1, statements[index].head(), sent)
		}
	}

	keys := explaintest.NewIndexKeys(fixture.database.Pool)
	var readTables []string
	outNodeRows := -1.0
	for _, setting := range []struct {
		name string
		set  []string
	}{
		{"enable_seqscan off", []string{"set local enable_seqscan = off"}},
		{"the default settings", nil},
	} {
		var total planCounts
		for index, statement := range statements {
			plan, err := boundedPlan(t, fixture.database, statement, oneHopBound, setting.set...)
			if err != nil {
				t.Errorf("under %s, statement %d (%s) did not run within the %s bound, so none of its counts is read: %v",
					setting.name, index+1, statement.head(), oneHopBound, err)
				continue
			}
			counts := countPlan(t, keys, plan)
			t.Logf("under %s, statement %d: count one %.2f rows of the link tables, count two %.2f rows of plan nodes, count three %d unkeyed reads",
				setting.name, index+1, counts.linkRows, counts.nodeRows, len(counts.unkeyed))
			for _, unkeyed := range counts.unkeyed {
				t.Errorf("count three, under %s, statement %d: %s", setting.name, index+1, unkeyed)
			}
			if index == 0 && setting.set == nil {
				outNodeRows = counts.nodeRows
			}
			total.add(counts)
		}
		t.Logf("under %s, every statement: count one %.2f (R = %d, margin %.3f), count two %.2f (bound %d, margin %.3f)",
			setting.name, total.linkRows, bound, total.linkMargin, total.nodeRows, 20*scale, total.nodeMargin)
		if total.linkRows > float64(bound)+total.linkMargin {
			t.Errorf("count one, under %s: the statements read %.2f rows of the link tables; want at most R = %d, plus %.3f for the plans' rounding",
				setting.name, total.linkRows, bound, total.linkMargin)
		}
		if total.nodeRows > float64(20*scale)+total.nodeMargin {
			t.Errorf("count two, under %s: the statements' plan nodes read %.2f rows; want at most 20 × %d = %d, plus %.3f for the plans' rounding",
				setting.name, total.nodeRows, scale, 20*scale, total.nodeMargin)
		}
		for _, relation := range total.relations {
			if !slices.Contains(readTables, relation) {
				readTables = append(readTables, relation)
			}
		}
	}
	slices.Sort(readTables)
	for _, relation := range readTables {
		var held int
		if err := fixture.database.Pool.QueryRow(ctx, "select count(*) from "+pgx.Identifier{relation}.Sanitize()).Scan(&held); err != nil {
			t.Fatalf("count the rows of %s: %v", relation, err)
		}
		if held < oneHopMinimumRows {
			t.Errorf("the fixture's %s holds %d rows, which a recorded statement reads; every such table holds %d or more", relation, held, oneHopMinimumRows)
		}
	}

	// The control: main's statement on the same fixture, under the same bound.
	mainStatement := recordedStatement{sql: mainClosureQuery, args: []any{fixture.issue}}
	control, err := fixture.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the control's transaction: %v", err)
	}
	if _, err := control.Exec(ctx, "set local statement_timeout = '"+oneHopBound+"'"); err != nil {
		t.Fatalf("set the control's bound: %v", err)
	}
	started := time.Now()
	mainRows, mainErr := control.Query(ctx, mainStatement.sql, mainStatement.args...)
	if mainErr == nil {
		for mainRows.Next() {
		}
		mainRows.Close()
		mainErr = mainRows.Err()
	}
	mainTook := time.Since(started).Round(time.Millisecond)
	_ = control.Rollback(ctx)
	// main's counts, for the record of what this head reads against it: its whole walk, which the
	// bound cuts, under a deadline of its own.
	mainNodeRows := -1.0
	walked := time.Now()
	if plan, err := boundedPlan(t, fixture.database, mainStatement, "60s"); err != nil {
		t.Logf("mainClosureQuery's counts are not read: its plan did not run within 60s: %v", err)
	} else {
		counts := countPlan(t, keys, plan)
		mainNodeRows = counts.nodeRows
		t.Logf("mainClosureQuery, under the default settings and no bound (%s): count one %.2f rows of the link tables, count two %.2f rows of plan nodes, count three %d unkeyed reads",
			time.Since(walked).Round(time.Millisecond), counts.linkRows, counts.nodeRows, len(counts.unkeyed))
	}
	switch {
	case queryCanceled(mainErr):
		t.Logf("control: main's statement ran past the %s bound after %s (SQLSTATE 57014)", oneHopBound, mainTook)
	case mainErr != nil:
		t.Fatalf("control: run main's statement: %v", mainErr)
	case mainNodeRows >= 0 && outNodeRows >= 0 && mainNodeRows >= 10*outNodeRows:
		t.Logf("control: main's statement answered within the bound, in %s, and read %.2f rows of plan nodes, ten times or more the out statement's %.2f",
			mainTook, mainNodeRows, outNodeRows)
	default:
		t.Errorf("control: main's statement answered within the %s bound, in %s, and its plan nodes read %.2f rows, not ten times the out statement's %.2f (-1: not read): the fixture is not as hard as the real graph",
			oneHopBound, mainTook, mainNodeRows, outNodeRows)
	}
}

// mainsDepthOneMembers is main's rows of depth 1 for issueKey, as the members they make, by
// ref_key.
func mainsDepthOneMembers(t *testing.T, database *store.Store, issueKey string) []Member {
	t.Helper()
	members := []Member{}
	for _, row := range closureStatementRows(t, database, mainClosureQuery, issueKey) {
		if row.depth == 1 {
			members = append(members, Member{RefKey: row.refKey, Depth: 1, Via: model.ReferenceVia{Kind: row.viaKind, ID: row.viaID}})
		}
	}
	sort.Slice(members, func(left, right int) bool { return members[left].RefKey < members[right].RefKey })
	return members
}

// depthOneDiff compares Closure's members of issueKey with main's rows of depth 1 over the same
// data, and says how they differ, "" when they do not, with Closure's members by ref_key. It first
// holds main to answering each document of reached at depth 1: a fixture that reaches nothing
// decides nothing, and fails the test.
func depthOneDiff(t *testing.T, database *store.Store, issueKey string, reached ...string) (string, []Member) {
	t.Helper()
	want := mainsDepthOneMembers(t, database, issueKey)
	for _, refKey := range reached {
		if !slices.ContainsFunc(want, func(member Member) bool { return member.RefKey == refKey }) {
			t.Fatalf("main's statement answers %s at no depth 1 for %s, so the fixture decides nothing; its rows of depth 1: %v", refKey, issueKey, want)
		}
	}
	references, err := Closure(context.Background(), database.Pool, issueKey)
	if err != nil {
		t.Fatalf("read %s's references: %v", issueKey, err)
	}
	got := slices.Clone(references.Members)
	sort.Slice(got, func(left, right int) bool { return got[left].RefKey < got[right].RefKey })
	var problems []string
	if deeper := slices.DeleteFunc(slices.Clone(got), func(member Member) bool { return member.Depth == 1 }); len(deeper) > 0 {
		problems = append(problems, fmt.Sprintf("%d members beyond depth 1, the first %s at depth %d, where every member is at depth 1",
			len(deeper), deeper[0].RefKey, deeper[0].Depth))
	}
	if !reflect.DeepEqual(got, want) {
		problems = append(problems, fmt.Sprintf("%d members against main's %d rows of depth 1: %s",
			len(got), len(want), lineDiff(memberLines(got), memberLines(want))))
	}
	return strings.Join(problems, "; "), got
}

// memberOf is the member of members whose ref_key is refKey, if any.
func memberOf(members []Member, refKey string) (Member, bool) {
	index := slices.IndexFunc(members, func(member Member) bool { return member.RefKey == refKey })
	if index < 0 {
		return Member{}, false
	}
	return members[index], true
}

// insertItemWithID inserts an ask, a comment or a message of issueKey under the id given, so a
// fixture chooses the order its ids sort in.
func insertItemWithID(t *testing.T, tx pgx.Tx, kind ItemKind, id, issueKey string) {
	t.Helper()
	tables, err := kind.tables()
	if err != nil {
		t.Fatal(err)
	}
	column := "body"
	if kind == ItemAsk {
		column = "question"
	}
	if _, err := tx.Exec(context.Background(), `insert into `+tables.items+` (id, issue_key, author, `+column+`)
		values ($1::uuid, $2, '{"kind":"user","id":"alice"}', 'cites')`, id, issueKey); err != nil {
		t.Fatalf("insert %s %s: %v", kind, id, err)
	}
}

// The references read's members are the documents the issue's own content links to, each at
// depth 1, with the via main's statement answered for it at depth 1: on each fixture below, every
// member Closure answers is at depth 1, and its members and vias are main's rows of depth 1 over
// the same data. Each fixture holds a document the issue's content reaches, which main's statement
// is first checked to answer at depth 1, and each named fixture also holds a second hop, which a
// read past one hop answers. None holds a typed pointer, which main's statement does not read.
func TestTheReferencesReadAnswersMainsDepthOneMembers(t *testing.T) {
	// The out side of TestTheReferencesReadReadsOnlyTheIssuesOwnLinks' graph, its pointer a
	// plain mention.
	t.Run("the out side of the one-hop fixture", func(t *testing.T) {
		fixture := newOneHopFixture(t, false)
		if diff, _ := depthOneDiff(t, fixture.database, fixture.issue, fixture.reached...); diff != "" {
			t.Error(diff)
		}
	})

	// Three comments cite one document, written in an order their ids do not sort in; a message,
	// an ask and a comment cite a second; a comment and a document of the issue, whose id sorts
	// last, a third. The via of each is the lowest of its kinds, then of its ids.
	t.Run("many vias at one depth", func(t *testing.T) {
		database := storetest.Open(t)
		ctx := context.Background()
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the fixture: %v", err)
		}
		defer tx.Rollback(ctx)
		createReferenceProject(t, tx, "CORE")
		createReferenceIssue(t, tx, "CORE-1", "CORE")
		_, far := createReferenceArtifact(t, tx, "", "CORE", "far-md")
		byComments, byCommentsRef := createReferenceArtifact(t, tx, "", "CORE", "by-comments-md")
		byItems, byItemsRef := createReferenceArtifact(t, tx, "", "CORE", "by-items-md")
		byDocument, byDocumentRef := createReferenceArtifact(t, tx, "", "CORE", "by-document-md")
		for _, id := range []string{"ffffffff-0000-4000-8000-000000000001", "55555555-0000-4000-8000-000000000002", "aaaaaaaa-0000-4000-8000-000000000003"} {
			insertItemWithID(t, tx, ItemComment, id, "CORE-1")
			insertReference(t, tx, "comment", id, "artifact", byCommentsRef)
		}
		for _, cite := range []struct {
			kind ItemKind
			id   string
		}{
			{ItemMessage, "99999999-0000-4000-8000-000000000004"},
			{ItemAsk, "33333333-0000-4000-8000-000000000005"},
			{ItemComment, "11111111-0000-4000-8000-000000000006"},
		} {
			insertItemWithID(t, tx, cite.kind, cite.id, "CORE-1")
			insertReference(t, tx, string(cite.kind), cite.id, "artifact", byItemsRef)
		}
		insertItemWithID(t, tx, ItemComment, "00000000-0000-4000-8000-000000000007", "CORE-1")
		insertReference(t, tx, "comment", "00000000-0000-4000-8000-000000000007", "artifact", byDocumentRef)
		const spec = "eeeeeeee-0000-4000-8000-000000000008"
		if _, err := tx.Exec(ctx, `insert into artifacts (id, issue_key, project_key, slug, name, kind, created_by)
			values ($1::uuid, 'CORE-1', 'CORE', 'spec', 'spec', 'doc', '{"kind":"user","id":"alice"}')`, spec); err != nil {
			t.Fatalf("insert the issue's document: %v", err)
		}
		insertReference(t, tx, "artifact", spec, "artifact", byDocumentRef)
		for _, id := range []string{byComments, byItems, byDocument} {
			insertReference(t, tx, "artifact", id, "artifact", far)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit the fixture: %v", err)
		}
		diff, got := depthOneDiff(t, database, "CORE-1", byCommentsRef, byItemsRef, byDocumentRef)
		if diff != "" {
			t.Error(diff)
		}
		for refKey, via := range map[string]model.ReferenceVia{
			byCommentsRef: {Kind: "comment", ID: "55555555-0000-4000-8000-000000000002"},
			byItemsRef:    {Kind: "ask", ID: "33333333-0000-4000-8000-000000000005"},
			byDocumentRef: {Kind: "artifact", ID: spec},
		} {
			if member, found := memberOf(got, refKey); !found || member.Via != via {
				t.Errorf("the member %s is %+v (found %t); want it via %s %s, the lowest of its near ends", refKey, member, found, via.Kind, via.ID)
			}
		}
	})

	// A comment cites a block its document's current version holds and a block no current version
	// holds: the first document is a member, the second is not.
	t.Run("a link to a block no current version holds", func(t *testing.T) {
		database := storetest.Open(t)
		ctx := context.Background()
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the fixture: %v", err)
		}
		defer tx.Rollback(ctx)
		createReferenceProject(t, tx, "CORE")
		createReferenceIssue(t, tx, "CORE-1", "CORE")
		_, far := createReferenceArtifact(t, tx, "", "CORE", "far-md")
		held, heldRef := createReferenceArtifact(t, tx, "", "CORE", "held-md")
		dropped, droppedRef := createReferenceArtifact(t, tx, "", "CORE", "dropped-md")
		insertDocumentBlock(t, tx, held, "b1", heldRef+"#b1")
		comment := createReferenceComment(t, tx, "CORE-1")
		insertReference(t, tx, "comment", comment, "block", heldRef+"#b1")
		insertReference(t, tx, "comment", comment, "block", droppedRef+"#b1")
		insertReference(t, tx, "artifact", held, "artifact", far)
		insertReference(t, tx, "artifact", dropped, "artifact", far)
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit the fixture: %v", err)
		}
		diff, got := depthOneDiff(t, database, "CORE-1", heldRef)
		if diff != "" {
			t.Error(diff)
		}
		if _, found := memberOf(got, droppedRef); found {
			t.Errorf("%s is a member, though the only link to it names a block no current version holds", droppedRef)
		}
	})

	// One link from each kind of the issue's own content, each to a document of its own.
	t.Run("one link from each kind of own content", func(t *testing.T) {
		database := storetest.Open(t)
		ctx := context.Background()
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the fixture: %v", err)
		}
		defer tx.Rollback(ctx)
		createReferenceProject(t, tx, "CORE")
		createReferenceIssue(t, tx, "CORE-1", "CORE")
		_, far := createReferenceArtifact(t, tx, "", "CORE", "far-md")
		reached := map[string]string{}
		for _, source := range []string{"comment", "comment-block", "message", "message-block", "question", "part", "document"} {
			id, refKey := createReferenceArtifact(t, tx, "", "CORE", "by-"+source+"-md")
			insertReference(t, tx, "artifact", id, "artifact", far)
			reached[source] = refKey
		}
		comment := createReferenceComment(t, tx, "CORE-1")
		insertReference(t, tx, "comment", comment, "artifact", reached["comment"])
		insertIndexedItem(t, tx, ItemComment, "CORE-1", "Intro.\n\nSee dispatch://CORE/artifact/by-comment-block-md.")
		const message = "77777777-0000-4000-8000-000000000001"
		insertItemWithID(t, tx, ItemMessage, message, "CORE-1")
		insertReference(t, tx, "message", message, "artifact", reached["message"])
		insertIndexedItem(t, tx, ItemMessage, "CORE-1", "Intro.\n\nSee dispatch://CORE/artifact/by-message-block-md.")
		question := insertAsk(t, tx, "CORE-1", "Ship dispatch://CORE/artifact/by-question-md first?", model.AskOption{Label: "Yes"})
		mentions := NewReader(ctx, tx, "ask", question.ID, itemBlockServer).Mentions(question.Question)
		if _, err := replaceRefs(ctx, tx, "ask", question.ID, text.Refs(mentions), nil); err != nil {
			t.Fatalf("index the ask's own references: %v", err)
		}
		indexAskPartsAt(t, tx, question, 1)
		part := insertAsk(t, tx, "CORE-1", "Which rollout do we take?",
			model.AskOption{Label: "Staged", Description: "As dispatch://CORE/artifact/by-part-md lays out."})
		indexAskPartsAt(t, tx, part, 2)
		spec, _ := createReferenceArtifact(t, tx, "CORE-1", "CORE", "spec")
		insertReference(t, tx, "artifact", spec, "artifact", reached["document"])
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit the fixture: %v", err)
		}
		refKeys := make([]string, 0, len(reached))
		for _, refKey := range reached {
			refKeys = append(refKeys, refKey)
		}
		slices.Sort(refKeys)
		if diff, _ := depthOneDiff(t, database, "CORE-1", refKeys...); diff != "" {
			t.Error(diff)
		}
	})

	// An ask and a comment that the issue's document owns, whose issue_key is null, each link to a
	// document the read does not reach, as main's statement does not.
	t.Run("items a document of the issue owns", func(t *testing.T) {
		database := storetest.Open(t)
		ctx := context.Background()
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the fixture: %v", err)
		}
		defer tx.Rollback(ctx)
		createReferenceProject(t, tx, "CORE")
		createReferenceIssue(t, tx, "CORE-1", "CORE")
		_, far := createReferenceArtifact(t, tx, "", "CORE", "far-md")
		reached, reachedRef := createReferenceArtifact(t, tx, "", "CORE", "reached-md")
		_, byAskRef := createReferenceArtifact(t, tx, "", "CORE", "by-owned-ask-md")
		_, byCommentRef := createReferenceArtifact(t, tx, "", "CORE", "by-owned-comment-md")
		spec, _ := createReferenceArtifact(t, tx, "CORE-1", "CORE", "spec")
		comment := createReferenceComment(t, tx, "CORE-1")
		insertReference(t, tx, "comment", comment, "artifact", reachedRef)
		insertReference(t, tx, "artifact", reached, "artifact", far)
		var ownedAsk, ownedComment string
		if err := tx.QueryRow(ctx, `insert into asks (artifact_id, author, question)
			values ($1::uuid, '{"kind":"user","id":"alice"}', 'Owned by the spec') returning id::text`, spec).Scan(&ownedAsk); err != nil {
			t.Fatalf("insert the ask the spec owns: %v", err)
		}
		if err := tx.QueryRow(ctx, `insert into comments (artifact_id, author, body)
			values ($1::uuid, '{"kind":"user","id":"alice"}', 'Owned by the spec') returning id::text`, spec).Scan(&ownedComment); err != nil {
			t.Fatalf("insert the comment the spec owns: %v", err)
		}
		insertReference(t, tx, "ask", ownedAsk, "artifact", byAskRef)
		insertReference(t, tx, "comment", ownedComment, "artifact", byCommentRef)
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit the fixture: %v", err)
		}
		diff, got := depthOneDiff(t, database, "CORE-1", reachedRef)
		if diff != "" {
			t.Error(diff)
		}
		for _, refKey := range []string{byAskRef, byCommentRef} {
			if _, found := memberOf(got, refKey); found {
				t.Errorf("%s is a member, though only an item the issue's document owns links to it", refKey)
			}
		}
	})

	// The issue's comment links to the issue's own document, which is no member.
	t.Run("a document of the issue its content links to", func(t *testing.T) {
		database := storetest.Open(t)
		ctx := context.Background()
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the fixture: %v", err)
		}
		defer tx.Rollback(ctx)
		createReferenceProject(t, tx, "CORE")
		createReferenceIssue(t, tx, "CORE-1", "CORE")
		_, far := createReferenceArtifact(t, tx, "", "CORE", "far-md")
		reached, reachedRef := createReferenceArtifact(t, tx, "", "CORE", "reached-md")
		_, specRef := createReferenceArtifact(t, tx, "CORE-1", "CORE", "spec")
		comment := createReferenceComment(t, tx, "CORE-1")
		insertReference(t, tx, "comment", comment, "artifact", reachedRef)
		insertReference(t, tx, "comment", comment, "artifact", specRef)
		insertReference(t, tx, "artifact", reached, "artifact", far)
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit the fixture: %v", err)
		}
		diff, got := depthOneDiff(t, database, "CORE-1", reachedRef)
		if diff != "" {
			t.Error(diff)
		}
		if _, found := memberOf(got, specRef); found {
			t.Errorf("%s, the issue's own document, is a member", specRef)
		}
	})

	// 200 random graphs of 40 documents, each graph an issue of its own whose first two documents
	// are the issue's, each document with up to six links to documents of its graph, a block its
	// document holds or a block none holds, and a comment of the issue citing one to four of them,
	// the first a document that is not the issue's, which main answers at depth 1.
	t.Run("200 random graphs", func(t *testing.T) {
		database := storetest.Open(t)
		rows := map[string][][]any{}
		written := map[[4]string]bool{}
		ref := func(fromKind, fromID, toKind, toID string) {
			if key := [4]string{fromKind, fromID, toKind, toID}; !written[key] {
				written[key] = true
				rows["refs"] = append(rows["refs"], []any{fromKind, fromID, toKind, toID})
			}
		}
		type graph struct{ issue, reached string }
		graphs := make([]graph, 200)
		rows["projects"] = [][]any{{"RAND", "RAND project", "alice"}}
		for seed := range graphs {
			random := rand.New(rand.NewPCG(uint64(seed+1), 4704))
			newID := func() uuid.UUID {
				var id uuid.UUID
				binary.BigEndian.PutUint64(id[:8], random.Uint64())
				binary.BigEndian.PutUint64(id[8:], random.Uint64())
				id[6] = id[6]&0x0f | 0x40
				id[8] = id[8]&0x3f | 0x80
				return id
			}
			issueKey := "RAND-" + strconv.Itoa(seed+1)
			rows["issues"] = append(rows["issues"], []any{issueKey, "RAND", seed + 1, "Random graph " + strconv.Itoa(seed+1), fixtureActor, fmt.Sprintf("r%08d", seed+1)})
			ids := make([]uuid.UUID, 40)
			refKeys := make([]string, 40)
			for index := range ids {
				ids[index] = newID()
				var owner any
				slug := fmt.Sprintf("g%d-d%d", seed+1, index)
				refKeys[index] = "RAND/" + slug
				if index < 2 {
					owner = issueKey
					refKeys[index] = issueKey + "/" + slug
				}
				rows["artifacts"] = append(rows["artifacts"], []any{pgtype.UUID{Bytes: ids[index], Valid: true}, owner, "RAND", slug, slug, "doc", fixtureActor, refKeys[index]})
				rows["document_blocks"] = append(rows["document_blocks"], []any{pgtype.UUID{Bytes: ids[index], Valid: true}, "b1", refKeys[index] + "#b1", "paragraph", "sha", slug, 1, time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)})
			}
			target := func(index int) (string, string) {
				switch choice := random.IntN(10); {
				case choice < 7:
					return "artifact", refKeys[index]
				case choice < 9:
					return "block", refKeys[index] + "#b1"
				default:
					return "block", refKeys[index] + "#b9"
				}
			}
			for index, id := range ids {
				for range random.IntN(7) {
					to := random.IntN(len(ids) - 1)
					if to >= index {
						to++
					}
					kind, toID := target(to)
					ref("artifact", id.String(), kind, toID)
				}
			}
			comment := newID()
			rows["comments"] = append(rows["comments"], []any{pgtype.UUID{Bytes: comment, Valid: true}, issueKey, fixtureActor, "cites"})
			reached := 2 + random.IntN(len(ids)-2)
			ref("comment", comment.String(), "artifact", refKeys[reached])
			for range random.IntN(4) {
				kind, toID := target(random.IntN(len(ids)))
				ref("comment", comment.String(), kind, toID)
			}
			graphs[seed] = graph{issue: issueKey, reached: refKeys[reached]}
		}
		copyFixture(t, database, []fixtureTable{
			{"projects", []string{"key", "name", "default_assignee"}},
			{"issues", []string{"key", "project_key", "number", "title", "created_by", "rank"}},
			{"artifacts", []string{"id", "issue_key", "project_key", "slug", "name", "kind", "created_by", "ref_key"}},
			{"document_blocks", []string{"artifact_id", "block_id", "node_id", "type", "text_sha256", "excerpt", "changed_version", "changed_at"}},
			{"comments", []string{"id", "issue_key", "author", "body"}},
			{"refs", []string{"from_kind", "from_id", "to_kind", "to_id"}},
		}, rows)
		var differ []string
		for _, graph := range graphs {
			if diff, _ := depthOneDiff(t, database, graph.issue, graph.reached); diff != "" {
				differ = append(differ, graph.issue+": "+diff)
			}
		}
		if len(differ) > 0 {
			t.Errorf("%d of %d random graphs answer other members than main's rows of depth 1; the first:\n%s",
				len(differ), len(graphs), strings.Join(differ[:min(3, len(differ))], "\n"))
		}
	})
}
