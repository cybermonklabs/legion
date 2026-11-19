package graphstore

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// planBatchNodes is how many nodes the node write under test carries, and planBatchRoles how many
// roles the claims under test claim, two claims each. A plan that reads the batch again for each
// row of what drives it reads it that many times, which is what the checks below count; the size
// only has to be far past one, and small enough that the controls, which run the quadratic plan,
// finish in seconds.
const (
	planBatchNodes = 1000
	planBatchRoles = 200
)

// TestTheNodeWritesAppendProbesEachNodeOfItsBatchOnce explains the statement queueAppend queues
// after a node write of planBatchNodes block nodes, as each kind of writer queues it, over a
// graph_nodes analyzed while it held the migrations' seed rows alone: the planner then estimates
// the mark the node write left on the batch's rows at one row of the table, as it does in
// production for a batch the table's statistics do not count yet. With hash and merge joins off,
// a nested loop is all the planner has left, and the plan must read the batch once and
// graph_nodes through its primary key alone, one probe per node (unprobedReads).
//
// The statement is the one the writer itself queued, with the arguments it bound (queuedBy), run
// after the node write the same writer queued before it, in a transaction rolled back. Each
// control is that statement with one of its two joins of the batch to graph_nodes as it stood
// before (markedBefore, settledBefore), and the check must fail on it, which is what shows the
// check can fail.
//
// The move as it stood is a control of the write with its cause in hand alone: for a write held
// for its cause it also moves every node still at the sentinel, a value the statistics say every
// seed row holds, so the planner estimated the move at every row of the table and probed by key
// with no shape holding it to that.
func TestTheNodeWritesAppendProbesEachNodeOfItsBatchOnce(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	pool := database.Pool
	analyzeNearEmpty(t, ctx, pool)
	keys := explaintest.NewIndexKeys(pool)

	at := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	who := model.Actor{Kind: "user", ID: "alice"}
	const document, cause = "PLAN-1/spec", int64(9700001)
	nodes := make([]Node, planBatchNodes)
	memberships := make([]Tuple, planBatchNodes)
	for index := range nodes {
		id := fmt.Sprintf("%s#b%05d", document, index)
		nodes[index] = Node{
			Kind: "block", ID: id, Supertag: "block",
			Visibility: VisibilityProject, State: StateLive,
			MadeAt: at, MadeBy: who, SourceEvent: NoEvent, Restates: true,
		}
		memberships[index] = Tuple{
			FromKind: "block", FromID: id, Kind: "in_document", ToKind: "artifact", ToID: document,
			At: at, Who: who, SourceEvent: NoEvent, Asserted: true,
		}
	}
	held := PlanUncaused(nodes, memberships)
	inHand := make([]Node, len(nodes))
	inHandTuples := make([]Tuple, len(memberships))
	for index := range nodes {
		inHand[index], inHandTuples[index] = nodes[index], memberships[index]
		inHand[index].SourceEvent, inHandTuples[index].SourceEvent = cause, cause
	}

	for _, writer := range []struct {
		name     string
		write    func(*store.WriteBatch) error
		controls []statementPart
	}{
		{
			name: "a write held for its cause, as the block index writes a version",
			write: func(b *store.WriteBatch) error {
				if err := QueueNodes(b, held); err != nil {
					return err
				}
				return QueueCaused(b, held, cause)
			},
			controls: []statementPart{markedBefore},
		},
		{
			name:     "a write with its cause in hand, as the backfill and the seed write",
			write:    func(b *store.WriteBatch) error { return QueueWrite(b, inHand, inHandTuples) },
			controls: []statementPart{markedBefore, settledBefore},
		},
	} {
		t.Run(writer.name, func(t *testing.T) {
			queued := queuedBy(t, writer.write)
			if len(queued) != 3 {
				t.Fatalf("the writer queued %d statements, want the node write's two and the append", len(queued))
			}
			nodeWrite, appended := queued[:2], queued[2]
			requireEachRowProbedOnce(t, ctx, pool, keys, "the append", nodeWrite, appended)
			for _, part := range writer.controls {
				control := recordedStatement{sql: part.splicedInto(t, appended.sql), args: appended.args}
				requireTheCheckFails(t, ctx, pool, keys, "the append with "+part.name+" as it stood", nodeWrite, control)
			}
		})
	}
}

// TestTheClaimArmsProbeEachClaimOfTheirBatchOnce explains the claim arms WriteArms builds for
// planBatchRoles roles - each role's page part_of the role, and the role held_by a session other
// than the one the log holds it by, as the publisher's record and the role backfill state them -
// over a graph_tuples analyzed while it held the migrations' seed rows alone, so the planner
// estimates the repeat key's rows and each claim's far ends at one row. The log's held_by tuples
// are appended in the explaining transaction, before the arms, so every held_by claim retracts
// one far end and asserts another. The plan must read the batch once and graph_tuples through
// an index keyed on what it probes, one probe per row (unprobedReads); the control is the arms as
// they stood before (claimArmsBefore), which joined the claims to the rows they read of
// themselves and to graph_tuples, and the check must fail on it.
func TestTheClaimArmsProbeEachClaimOfTheirBatchOnce(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	pool := database.Pool
	analyzeNearEmpty(t, ctx, pool)
	keys := explaintest.NewIndexKeys(pool)

	at := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	who := model.Actor{Kind: "system", ID: "dispatch-outbox"}
	const before, cause = int64(9800001), int64(9800002)
	logged := make([]Tuple, 0, planBatchRoles)
	claims := make([]Claim, 0, 2*planBatchRoles)
	for index := range planBatchRoles {
		role := fmt.Sprintf("plan-role-%04d", index)
		logged = append(logged, Tuple{
			FromKind: KindRole, FromID: role, Kind: TupleHeldBy, ToKind: KindSession, ToID: "plan-session-before",
			At: at, Who: who, SourceEvent: before, Asserted: true,
		})
		claims = append(claims,
			Claim{
				FromKind: KindPage, FromID: RolePageID(role), Kind: TuplePartOf, ToKind: KindRole, ToID: role,
				At: at, Who: who, SourceEvent: cause,
			},
			Claim{
				FromKind: KindRole, FromID: role, Kind: TupleHeldBy, ToKind: KindSession, ToID: "plan-session-now",
				At: at, Who: who, SourceEvent: cause,
			})
	}
	prior := queuedBy(t, func(b *store.WriteBatch) error { return QueueWrite(b, nil, logged) })
	arms, params, err := WriteArms(1, nil, nil, claims)
	if err != nil {
		t.Fatalf("build the claim arms: %v", err)
	}
	requireEachRowProbedOnce(t, ctx, pool, keys, "the claim arms", prior,
		recordedStatement{sql: "with " + arms + "\nselect 1", args: params})
	requireTheCheckFails(t, ctx, pool, keys, "the claim arms as they stood", prior,
		recordedStatement{sql: "with " + claimArmsBefore + "\nselect 1", args: params})
}

// analyzeNearEmpty analyzes the store's two tables as the migrations left them, before the test
// writes anything: the statistics a production store holds of rows a batch has not reached yet.
func analyzeNearEmpty(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `analyze graph_nodes, graph_tuples`); err != nil {
		t.Fatalf("analyze the store's tables: %v", err)
	}
}

// requireEachRowProbedOnce holds statement, explained after before, to reading the batch once and
// the store's tables by probes of their keys, and records how it read each.
func requireEachRowProbedOnce(
	t *testing.T, ctx context.Context, pool *store.Pool, keys *explaintest.IndexKeys,
	name string, before []recordedStatement, statement recordedStatement,
) {
	t.Helper()
	forbidden, allowed := unprobedReads(t, keys, explainAfter(t, ctx, pool, before, statement, "json"))
	if len(forbidden) > 0 {
		t.Errorf("%s reads the batch more than once or a store table other than by its key:\n  %s\nplan:\n%s",
			name, strings.Join(forbidden, "\n  "), explainAfter(t, ctx, pool, before, statement, "text"))
		return
	}
	t.Logf("%s probes each row of its batch once:\n  %s", name, strings.Join(allowed, "\n  "))
}

// requireTheCheckFails holds control, a statement as it stood before, to failing the check
// requireEachRowProbedOnce makes, and records the reads it fails on: a check that passed its
// control would be a check that cannot fail.
func requireTheCheckFails(
	t *testing.T, ctx context.Context, pool *store.Pool, keys *explaintest.IndexKeys,
	name string, before []recordedStatement, control recordedStatement,
) {
	t.Helper()
	forbidden, _ := unprobedReads(t, keys, explainAfter(t, ctx, pool, before, control, "json"))
	if len(forbidden) == 0 {
		t.Errorf("%s, the control, passes the check, so the check cannot tell a quadratic plan from this one:\n%s",
			name, explainAfter(t, ctx, pool, before, control, "text"))
		return
	}
	t.Logf("%s fails the check, as the control must:\n  %s", name, strings.Join(forbidden, "\n  "))
}

// storeTables are the tables a batch of the store's writers is probed against.
var storeTables = map[string]bool{"graph_nodes": true, "graph_tuples": true}

// unprobedReads reads plan, an EXPLAIN (ANALYZE, FORMAT JSON) plan, for the two reads the store's
// batch statements may not make, and answers each it found (forbidden) and every other read of the
// batch or of a store table, with how often it ran (allowed). It reads loops and never time.
//
// A scan of the batch - the function scan of its arrays, or the scan of a CTE of its rows - runs
// once: one that runs once per row of what drives it is the batch rescanned whole for each row,
// which is quadratic in the batch. A read of graph_nodes or graph_tuples is an index scan whose
// condition keys its index's first column, a probe of the rows one batch row names, and on
// graph_nodes the index is its primary key: anything else - a sequential scan, an index read end
// to end, a bitmap - reads the table, or one of its indexes, whole, which on the outer side of a
// nested loop over the batch is the quadratic plan the other way round.
func unprobedReads(t *testing.T, keys *explaintest.IndexKeys, plan json.RawMessage) (forbidden, allowed []string) {
	t.Helper()
	for _, node := range explaintest.Nodes(t, plan) {
		switch {
		case node.Type == "Function Scan" || node.Type == "CTE Scan":
			read := fmt.Sprintf("%s ran %.0f time(s), %.0f row(s) each", node, node.Loops, node.Rows)
			if node.Loops > 1 {
				forbidden = append(forbidden, read+": the batch is read again for each row of what drives it")
				continue
			}
			allowed = append(allowed, read)
		case storeTables[node.Relation] && node.Type != "ModifyTable":
			read := fmt.Sprintf("%s ran %.0f time(s)", node, node.Loops)
			keyed := (node.Type == "Index Scan" || node.Type == "Index Only Scan") && node.IndexCond != "" &&
				explaintest.CondNamesFirstKey(node.IndexCond, keys.FirstKey(t, node.Index))
			switch {
			case !keyed:
				forbidden = append(forbidden, read+": "+node.Relation+" is read other than by a probe of an index's key")
			case node.Relation == "graph_nodes" && node.Index != "graph_nodes_pkey":
				forbidden = append(forbidden, read+": graph_nodes is probed other than by its primary key")
			default:
				allowed = append(allowed, read+" under "+node.IndexCond)
			}
		}
	}
	return forbidden, allowed
}

// queuedStatements is the transaction a writer's batch is sent to here: it keeps every statement
// of the batch with the arguments the writer bound, in the order the batch holds them, and runs
// none of them, so a plan is read off the statement the writer built rather than off a copy kept
// in this file.
type queuedStatements struct {
	pgx.Tx
	queued []recordedStatement
}

func (q *queuedStatements) SendBatch(_ context.Context, batch *pgx.Batch) pgx.BatchResults {
	for _, query := range batch.QueuedQueries {
		q.queued = append(q.queued, recordedStatement{sql: query.SQL, args: query.Arguments})
	}
	return unsentBatch{}
}

// unsentBatch answers the one call store.WriteBatch.Send makes of a batch's results, its Close,
// for a batch nothing ran.
type unsentBatch struct{ pgx.BatchResults }

func (unsentBatch) Close() error { return nil }

// queuedBy is every statement write queues on a write phase's batch, as it queued them.
func queuedBy(t *testing.T, write func(*store.WriteBatch) error) []recordedStatement {
	t.Helper()
	recorder := &queuedStatements{}
	if err := store.RunWrite(context.Background(), recorder, write); err != nil {
		t.Fatalf("queue the write: %v", err)
	}
	return recorder.queued
}

// nestedLoopsOnly is what every plan here is read under. With the two join strategies that read a
// whole side once off, a join of the batch to a table is a nested loop, and the plan says which
// side it rescans per row of the other, which is what the checks read.
var nestedLoopsOnly = []string{"set local enable_hashjoin = off", "set local enable_mergejoin = off"}

// explainAfter runs before and then EXPLAIN (ANALYZE, FORMAT format) of statement, with the
// arguments each bound, in one transaction it rolls back, under nestedLoopsOnly, and answers the
// plan: the one plan node for json, every line for text. Each call is a transaction of its own,
// since the explained statement writes what a second explain would read.
func explainAfter(t *testing.T, ctx context.Context, pool *store.Pool, before []recordedStatement, statement recordedStatement, format string) []byte {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction a plan is read in: %v", err)
	}
	defer tx.Rollback(ctx)
	for _, earlier := range before {
		if _, err := tx.Exec(ctx, earlier.sql, earlier.args...); err != nil {
			t.Fatalf("run the statement queued before the one explained: %v", err)
		}
	}
	for _, setting := range nestedLoopsOnly {
		if _, err := tx.Exec(ctx, setting); err != nil {
			t.Fatalf("%s: %v", setting, err)
		}
	}
	rows, err := tx.Query(ctx, "explain (analyze, format "+format+") "+statement.sql, statement.args...)
	if err != nil {
		t.Fatalf("explain the statement: %v", err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("explain the statement: %v", err)
	}
	answer := []byte(strings.Join(lines, "\n"))
	if format == "json" {
		return explaintest.OnePlan(t, answer)
	}
	return answer
}

// statementPart is one part of a statement as it stood before: the text that opens it, the text
// that opens what follows it, and its own text, which runs up to the second.
type statementPart struct {
	name, opens, closes, text string
}

// splicedInto is sql with p in place of the part between p's opening text and its closing one,
// with p's markParameter and sentinelParameter replaced by the parameters sql binds its node
// write's mark and its sentinel flag at, read off sql's own probe (appendParameters).
func (p statementPart) splicedInto(t *testing.T, sql string) string {
	t.Helper()
	start := strings.Index(sql, p.opens)
	end := strings.Index(sql, p.closes)
	if start < 0 || end < start {
		t.Fatalf("the statement holds no %q followed by %q to put %s as it stood between:\n%s", p.opens, p.closes, p.name, sql)
	}
	mark, sentinel := appendParameters(t, sql)
	text := strings.NewReplacer(markParameter, mark, sentinelParameter, sentinel).Replace(p.text)
	spliced := sql[:start] + text + sql[end:]
	if spliced == sql {
		t.Fatalf("%s as it stood is the statement's own text: the control controls nothing", p.name)
	}
	return spliced
}

// markParameter and sentinelParameter stand in a control's text for the parameters the append
// binds its mark and its sentinel flag at, which follow its node arrays, so their numbers move
// with the base attributes those arrays carry.
const markParameter, sentinelParameter = "$MARK", "$SENTINEL"

// appendParameters reads, off the append's own probe, the parameters it binds its mark and its
// sentinel flag at.
func appendParameters(t *testing.T, sql string) (mark, sentinel string) {
	t.Helper()
	marked := regexp.MustCompile(`\(-held\.source_event\) / \d+ = (\$\d+)::bigint`).FindStringSubmatch(sql)
	flagged := regexp.MustCompile(`or (\$\d+)::boolean and held\.source_event`).FindStringSubmatch(sql)
	if marked == nil || flagged == nil {
		t.Fatalf("the append names no parameter of its mark or of its sentinel flag:\n%s", sql)
	}
	return marked[1], flagged[1]
}

// markedBefore and settledBefore are the append's read of the marks and its move as queueAppend
// wrote them before INFRA-1149, each a join of the batch to graph_nodes, built from the helpers the
// statement is built from (markedAttributes, markedBy, causeOf), with the parameters the statement
// binds them at (appendParameters).
var (
	markedBefore = statementPart{name: "the read of the marks", opens: "marked as (", closes: "based as (", text: `marked as (
			select written.*, ` + markedAttributes("held") + ` & ~written.skip as attributes
			from written join graph_nodes held on held.kind = written.kind and held.id = written.id
			where ` + markedBy("held", markParameter) + `
		),
		`}
	settledBefore = statementPart{name: "the move", opens: "settled as (", closes: "insert into graph_tuples", text: `settled as (
			update graph_nodes set source_event = ` + causeOf("written.cause") + `
			from written
			where graph_nodes.kind = written.kind and graph_nodes.id = written.id
			  and (` + markedBy("graph_nodes", markParameter) + `
			       or ` + sentinelParameter + `::boolean and graph_nodes.source_event = ` + strconv.FormatInt(NoEvent, 10) + `
			          and written.cause <> ` + strconv.FormatInt(NoEvent, 10) + `)
		)
		`}
)

// claimArmsBefore is claimArms(1, "") as it stood before INFRA-1149: the claims joined to the far
// ends they read, and their stated rows to themselves and to graph_tuples.
const claimArmsBefore = `graph_claims as (
	select claim.from_kind, claim.from_id, claim.kind, claim.to_kind, claim.to_id, claim.at, claim.who,
	       (case when claim.source_event = -1 then currval(pg_get_serial_sequence('events', 'id')) else claim.source_event end) as source_event, claim.position
	from unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[], $6::timestamptz[], $7::jsonb[], $8::bigint[])
		with ordinality as claim(from_kind, from_id, kind, to_kind, to_id, at, who, source_event, position)
),
graph_claims_held as (
	select claim.position, held.to_kind, held.to_id
	from graph_claims claim
	join lateral (
		select distinct on (t.to_kind, t.to_id) t.to_kind, t.to_id, t.asserted
			from graph_tuples t
			where t.from_kind = claim.from_kind and t.from_id = claim.from_id
			and t.kind = claim.kind and t.to_kind <> 'value'
			order by t.to_kind, t.to_id, t.seq desc
	) held on held.asserted
),
graph_claims_stated (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted, position, step) as (
	select claim.from_kind, claim.from_id, claim.kind, held.to_kind, held.to_id, claim.at, claim.who,
	       claim.source_event, null::bigint, false, claim.position, 0
	from graph_claims claim
	join graph_claims_held held on held.position = claim.position
	where (held.to_kind, held.to_id) <> (claim.to_kind, claim.to_id)
	union all
	select claim.from_kind, claim.from_id, claim.kind, claim.to_kind, claim.to_id, claim.at, claim.who,
	       claim.source_event, null::bigint, true, claim.position, 1
	from graph_claims claim
	where claim.to_id <> '' and not exists (
		select 1 from graph_claims_held held
		where held.position = claim.position
			and held.to_kind = claim.to_kind and held.to_id = claim.to_id
	)
),
graph_claims_written as (
	insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
	select from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted from graph_claims_stated stated
	where not exists (
		select 1 from graph_claims_stated restated
		join graph_tuples once
			on once.from_kind = restated.from_kind and once.from_id = restated.from_id
			and once.kind = restated.kind and once.to_kind = restated.to_kind and once.to_id = restated.to_id
			and once.source_event = restated.source_event and once.asserted = restated.asserted
			and once.to_kind <> 'value' and once.source_event <> 0
		where restated.position = stated.position
	)
	order by position, step
	on conflict (from_kind, from_id, kind, to_kind, to_id, source_event, asserted)
		where to_kind <> 'value' and source_event <> 0
	do nothing
)`
