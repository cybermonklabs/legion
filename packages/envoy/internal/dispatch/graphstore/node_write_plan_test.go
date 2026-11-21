package graphstore

import (
	"context"
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
// a nested loop is all the planner has left, and under each plan-cache mode the plan must read
// the batch once and graph_nodes through its primary key alone, one probe per node
// (explaintest.RequireEachRowProbedOnce).
//
// The statement is the one the writer itself queued, with the arguments it bound (queuedBy), run
// after the node write the same writer queued before it, in a transaction rolled back. Each
// control is that statement with one of its two joins of the batch to graph_nodes as it stood
// before (markedBefore, settledBefore), written with the helpers the append is written with and
// bound at the parameters the statement binds its mark and its sentinel at (appendMark,
// appendSentinel), so it cannot drift from the code it stands in for. It must plan and run, and
// the check must fail on it, which is what shows the check can fail. A control is held to failing
// under the custom plan alone: the generic plan cannot see the arrays and estimates each at ten
// rows, so for the join as it stood it put the array outside and probed, and the quadratic plan
// is the custom one, which a prepared statement takes for its first five executions and whenever
// the generic plan costs more than they did. The control's generic reading is recorded beside it.
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
		controls []appendControl
	}{
		{
			name: "a write held for its cause, as the block index writes a version",
			write: func(b *store.WriteBatch) error {
				QueueNodes(b, held)
				QueueCaused(b, held, cause)
				return nil
			},
			controls: []appendControl{markedBefore},
		},
		{
			name: "a write with its cause in hand, as the backfill and the seed write",
			write: func(b *store.WriteBatch) error {
				QueueWrite(b, inHand, inHandTuples)
				return nil
			},
			controls: []appendControl{markedBefore, settledBefore},
		},
	} {
		t.Run(writer.name, func(t *testing.T) {
			queued := queuedBy(t, writer.write)
			if len(queued) != 4 {
				t.Fatalf("the writer queued %d statements, want the supertag check, the node write's two and the append", len(queued))
			}
			nodeWrite, appended := queued[:3], queued[3]
			explaintest.RequireEachRowProbedOnce(t, ctx, pool, keys, storeKeys, "the append", nodeWrite, appended)
			mark := explaintest.Parameter(t, appended.SQL, appendMark)
			sentinel := explaintest.Parameter(t, appended.SQL, appendSentinel)
			for _, control := range writer.controls {
				part := control(mark, sentinel)
				explaintest.RequireTheCheckFails(t, ctx, pool, keys, storeKeys, "the append with "+part.Name+" as it stood",
					[]string{"force_custom_plan"}, nodeWrite,
					explaintest.Statement{SQL: explaintest.Spliced(t, appended.SQL, part), Args: appended.Args})
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
// one far end and asserts another. Under each plan-cache mode the plan must read the batch once
// and graph_tuples through an index keyed on what it probes, one probe per row
// (explaintest.RequireEachRowProbedOnce); the control is the arms as they stood before
// (claimArmsBefore), which joined the claims to the rows they read of themselves and to
// graph_tuples, and the check must fail on it under both.
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
	prior := queuedBy(t, func(b *store.WriteBatch) error {
		QueueWrite(b, nil, logged)
		return nil
	})
	const first = 1
	arms, params, err := WriteArms(first, nil, nil, claims)
	if err != nil {
		t.Fatalf("build the claim arms: %v", err)
	}
	claimArms := explaintest.Statement{SQL: "with " + arms + "\nselect 1", Args: params}
	explaintest.RequireEachRowProbedOnce(t, ctx, pool, keys, storeKeys, "the claim arms", prior, claimArms)
	explaintest.RequireTheCheckFails(t, ctx, pool, keys, storeKeys, "the claim arms as they stood",
		explaintest.PlanCacheModes, prior, explaintest.Statement{SQL: "with " + claimArmsBefore(first) + "\nselect 1", Args: params})
}

// analyzeNearEmpty analyzes the store's two tables as the migrations left them, before the test
// writes anything: the statistics a production store holds of rows a batch has not reached yet.
func analyzeNearEmpty(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `analyze graph_nodes, graph_tuples`); err != nil {
		t.Fatalf("analyze the store's tables: %v", err)
	}
}

// storeKeys are the tables a batch of the store's writers is probed against, each with the one
// index a probe of it must take where it has one: graph_nodes its primary key, graph_tuples any
// index whose first column the probe keys (explaintest.UnprobedReads).
var storeKeys = map[string]string{"graph_nodes": "graph_nodes_pkey", "graph_tuples": explaintest.AnyIndex}

// queuedBy is every statement write queues on a write phase's batch, as it queued them, none of
// them run (explaintest.BatchedBy).
func queuedBy(t *testing.T, write func(*store.WriteBatch) error) []explaintest.Statement {
	t.Helper()
	return explaintest.BatchedBy(t, func(tx pgx.Tx) error { return store.RunWrite(context.Background(), tx, write) })
}

// appendControl is one part of the append as queueAppend wrote it before its probe, given the
// parameters the append binds its mark and its sentinel at (appendMark, appendSentinel).
type appendControl func(mark, sentinel string) explaintest.Part

// appendMark and appendSentinel find the parameters of the mark and of the sentinel in the append
// a writer queued: the read of the marks and the move test the probed row by markedBy, and the
// move reads the sentinel as a boolean beside that row's source_event. Read off the statement, a
// control binds them where the statement does, whatever number of arrays the append binds first.
var (
	appendMark = regexp.MustCompile(strings.Replace(regexp.QuoteMeta(markedBy("held", "PARAMETER")),
		"PARAMETER", `(\$[0-9]+)`, 1))
	appendSentinel = regexp.MustCompile(`(\$[0-9]+)::boolean and held\.source_event = `)
)

// markedBefore is the append's read of the marks as queueAppend wrote it before its probe: the
// batch joined to graph_nodes, written with the helpers the append is written with.
func markedBefore(mark, _ string) explaintest.Part {
	return explaintest.Part{Name: "the read of the marks", Opens: "marked as (", Closes: "based as (", Text: `marked as (
			select written.*, ` + markAttributes("held") + ` & ~written.skip as attributes
			from written join graph_nodes held on held.kind = written.kind and held.id = written.id
			where ` + markedBy("held", mark) + `
		),
		`}
}

// settledBefore is the append's move as queueAppend wrote it before its probe: graph_nodes
// updated from the batch joined to it, written with the helpers the append is written with.
func settledBefore(mark, sentinel string) explaintest.Part {
	return explaintest.Part{Name: "the move", Opens: "settled as (", Closes: "insert into graph_tuples", Text: `settled as (
			update graph_nodes set source_event = ` + causeOf("written.cause") + `
			from written
			where graph_nodes.kind = written.kind and graph_nodes.id = written.id
			  and (` + markedBy("graph_nodes", mark) + `
			       or ` + sentinel + `::boolean and graph_nodes.source_event = ` + strconv.FormatInt(NoEvent, 10) + `
			          and written.cause <> ` + strconv.FormatInt(NoEvent, 10) + `)
		)
		`}
}

// claimArmsBefore is claimArms as it stood before its probes, written with the helpers claimArms
// is written with: the claims joined to the far ends they read, and their stated rows to
// themselves and to graph_tuples.
func claimArmsBefore(first int) string {
	return `graph_claims as (
	select claim.from_kind, claim.from_id, claim.kind, claim.to_kind, claim.to_id, claim.at, claim.who,
	       ` + causeOf("claim.source_event") + ` as source_event, claim.position
	from unnest(` + claimArrays(first) + `)
		with ordinality as claim(from_kind, from_id, kind, to_kind, to_id, at, who, source_event, position)
),
graph_claims_held as (
	select claim.position, held.to_kind, held.to_id
	from graph_claims claim
	join lateral (
		` + CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_kind, t.to_id, t.asserted`,
		`t.from_kind = claim.from_kind and t.from_id = claim.from_id
			and t.kind = claim.kind and t.to_kind <> '`+KindValue+`'`) + `
	) held on held.asserted
),
graph_claims_stated (` + tupleWriteColumns + `, position, step) as (
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
	insert into graph_tuples (` + tupleWriteColumns + `)
	select ` + tupleWriteColumns + ` from graph_claims_stated stated
	where not exists (
		select 1 from graph_claims_stated restated
		join graph_tuples once
			on once.from_kind = restated.from_kind and once.from_id = restated.from_id
			and once.kind = restated.kind and once.to_kind = restated.to_kind and once.to_id = restated.to_id
			and once.source_event = restated.source_event and once.asserted = restated.asserted
			and once.to_kind <> '` + KindValue + `' and once.source_event <> 0
		where restated.position = stated.position
	)
	order by position, step` + relationRepeatKey + `
)`
}
