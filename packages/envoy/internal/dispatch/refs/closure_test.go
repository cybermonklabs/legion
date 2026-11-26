package refs

import (
	"context"
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

// closureStatementRows runs statement with args and answers its rows in the order it gave them.
func closureStatementRows(t *testing.T, database *store.Store, statement string, args ...any) []closureRow {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), statement, args...)
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
// two to ten, are no member. It reads as somebody who proves no author, and no node of this data
// is reserved for one, so the predicates it binds leave every row main answered.
func TestTheClosureAnswersWhatMainsStatementDid(t *testing.T) {
	fixture := newClosureFixture(t)
	wantMembers := mainsDepthOneMembers(t, fixture.database, "CORE-1")
	references, err := Closure(context.Background(), fixture.database.Pool, model.Actor{}, "CORE-1", "")
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
	if err := tx.QueryRow(ctx, "explain (format json) "+linksOutQuery(), closureArgs("CORE-1", model.Actor{})...).Scan(&raw); err != nil {
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

// untimedAnalyze is the explain options the plans of TestTheReferencesReadReadsOnlyTheIssuesOwnLinks
// are read with: timing off leaves the rows and loops every count reads, and spares the clock
// reads of each row.
const untimedAnalyze = "analyze, timing off, format json"

// queryCanceled is whether err is a statement Postgres cancelled, as statement_timeout does
// (SQLSTATE 57014).
func queryCanceled(err error) bool {
	var pgError *pgconn.PgError
	return errors.As(err, &pgError) && pgError.Code == "57014"
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

// membersDiff says how members differ from want, which is by ref_key and every member of which is
// at depth 1, "" when they do not, with members by ref_key: a member beyond depth 1, and a member
// or a via want does not hold. wantName names want in what it says.
func membersDiff(members, want []Member, wantName string) (string, []Member) {
	got := slices.Clone(members)
	sort.Slice(got, func(left, right int) bool { return got[left].RefKey < got[right].RefKey })
	var problems []string
	if deeper := slices.DeleteFunc(slices.Clone(got), func(member Member) bool { return member.Depth == 1 }); len(deeper) > 0 {
		problems = append(problems, fmt.Sprintf("%d members beyond depth 1, the first %s at depth %d, where every member is at depth 1",
			len(deeper), deeper[0].RefKey, deeper[0].Depth))
	}
	if !reflect.DeepEqual(got, want) {
		problems = append(problems, fmt.Sprintf("%d members against the %d %s: %s",
			len(got), len(want), wantName, lineDiff(memberLines(got), memberLines(want))))
	}
	return strings.Join(problems, "; "), got
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
	references, err := Closure(ctx, recorder, model.Actor{}, fixture.issue, "")
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
		settings := append([]string{"set local statement_timeout = '" + oneHopBound + "'"}, setting.set...)
		var total planCounts
		for index, statement := range statements {
			plan, err := explainAnalyzedWith(t, fixture.database.Pool, untimedAnalyze, statement, settings...)
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
	if plan, err := explainAnalyzedWith(t, fixture.database.Pool, untimedAnalyze, mainStatement, "set local statement_timeout = '60s'"); err != nil {
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
	references, err := Closure(context.Background(), database.Pool, model.Actor{}, issueKey, "")
	if err != nil {
		t.Fatalf("read %s's references: %v", issueKey, err)
	}
	return membersDiff(references.Members, want, "rows of depth 1 of main's statement")
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
		mentions := NewReader(ctx, tx, question.Author, "ask", question.ID, itemBlockServer).Mentions(question.Question)
		if _, err := replaceRefs(ctx, tx, "ask", question.ID, text.Refs(mentions), nil, false); err != nil {
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
			issueKey := "RAND-" + strconv.Itoa(seed+1)
			rows["issues"] = append(rows["issues"], []any{issueKey, "RAND", seed + 1, "Random graph " + strconv.Itoa(seed+1), edgesFixtureActor, fmt.Sprintf("r%08d", seed+1)})
			ids := make([]uuid.UUID, 40)
			refKeys := make([]string, 40)
			for index := range ids {
				ids[index] = seededUUID(random)
				var owner any
				slug := fmt.Sprintf("g%d-d%d", seed+1, index)
				refKeys[index] = "RAND/" + slug
				if index < 2 {
					owner = issueKey
					refKeys[index] = issueKey + "/" + slug
				}
				rows["artifacts"] = append(rows["artifacts"], []any{ids[index].String(), owner, "RAND", slug, slug, "doc", edgesFixtureActor, refKeys[index]})
				rows["document_blocks"] = append(rows["document_blocks"], []any{ids[index].String(), "b1", refKeys[index] + "#b1", "paragraph", "sha", slug, 1, time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)})
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
			comment := seededUUID(random)
			rows["comments"] = append(rows["comments"], []any{comment.String(), issueKey, edgesFixtureActor, "cites"})
			reached := 2 + random.IntN(len(ids)-2)
			ref("comment", comment.String(), "artifact", refKeys[reached])
			for range random.IntN(4) {
				kind, toID := target(random.IntN(len(ids)))
				ref("comment", comment.String(), kind, toID)
			}
			graphs[seed] = graph{issue: issueKey, reached: refKeys[reached]}
		}
		loadFixture(t, database.Pool, []copyTable{
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

// A retired document's address names its issue (loadArtifactNodes). A comment of CORE-1 links
// both CORE-2 and CORE-2's retired decisions document, and the references read lists CORE-2 once
// among the links out, with the kinds of both links; two retired documents of CORE-2 each link
// CORE-1, and the read lists CORE-2 once among the links in. Each list holds one row for each
// node, so a client keying its rows by node draws each once.
func TestTheReferencesReadListsTheIssueOfARetiredDocumentOnce(t *testing.T) {
	tx := prefixFixture(t)
	ctx := context.Background()
	if _, err := tx.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('CORE-2', 'CORE', 2, 'Retired its documents', '{"kind":"user","id":"alice"}', 'V')
	`); err != nil {
		t.Fatalf("create issue CORE-2: %v", err)
	}
	retired := func(slug string) (string, string) {
		t.Helper()
		var id, refKey string
		if err := tx.QueryRow(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by, retired_at)
			values ('CORE-2', 'CORE', $1, $1, 'doc', false, '{"kind":"user","id":"alice"}', now())
			returning id::text, ref_key
		`, slug).Scan(&id, &refKey); err != nil {
			t.Fatalf("create the retired document %s: %v", slug, err)
		}
		return id, refKey
	}
	decisions, decisionsKey := retired(RetiredDecisionsSlug)
	notes, _ := retired("notes")
	comment := createReferenceComment(t, tx, "CORE-1")
	insertReference(t, tx, "comment", comment, "issue", "CORE-2")
	insertReference(t, tx, "comment", comment, "artifact", decisionsKey)
	insertReference(t, tx, "artifact", decisions, "issue", "CORE-1")
	insertReference(t, tx, "artifact", notes, "issue", "CORE-1")

	references, err := Closure(ctx, tx, model.Actor{}, "CORE-1", "")
	if err != nil {
		t.Fatalf("read the references of CORE-1: %v", err)
	}
	listed := func(links []model.ReferenceLink) []string {
		rows := []string{}
		for _, link := range links {
			rows = append(rows, fmt.Sprintf("%s %s %v via %s %s", link.Node.Kind, link.Node.ID, link.Kinds, link.Via.Kind, link.Via.ID))
		}
		return rows
	}
	if got, want := listed(references.Links), []string{"issue CORE-2 [mentions] via comment " + comment}; !slices.Equal(got, want) {
		t.Errorf("the links out of CORE-1 = %q; want %q, the issue once for its own address and its retired document's", got, want)
	}
	if got, want := listed(references.LinkedFrom), []string{"issue CORE-2 [mentions] via issue CORE-1"}; !slices.Equal(got, want) {
		t.Errorf("the links into CORE-1 = %q; want %q, the issue once for its two retired documents", got, want)
	}
	if len(references.Members) != 0 {
		t.Errorf("the members of CORE-1 = %#v; want none, since a retired document's address names its issue", references.Members)
	}
}
