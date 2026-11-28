package api

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// tupleIndexesRequirements and tupleIndexesChecks are how many requirement blocks and check blocks
// the coverage seed holds, tupleIndexesServed how many requirements each check serves, and
// tupleIndexesTexts how many text values each block's history holds.
const (
	tupleIndexesRequirements = 100
	tupleIndexesChecks       = 400
	tupleIndexesServed       = 3
	tupleIndexesTexts        = 95
)

// seedTupleIndexesCoverage writes the store the coverage walk is planned over: COVER-1, whose spec
// holds tupleIndexesRequirements requirement blocks and whose Verification document holds
// tupleIndexesChecks check blocks, each check serving tupleIndexesServed requirements, so each
// requirement is served by tupleIndexesChecks*tupleIndexesServed/tupleIndexesRequirements checks;
// each block with its part_of, its in_document and a history of text. It holds about 50,000
// tuples over about 500 nodes, analyzed, as c-tuple-indexes states, and answers the seq it was
// read at and the requirements' addresses.
func seedTupleIndexesCoverage(t *testing.T, database *store.Store) (int64, []string) {
	t.Helper()
	ctx := context.Background()
	requirements, checks := fmt.Sprint(tupleIndexesRequirements), fmt.Sprint(tupleIndexesChecks)
	blocks := `(select 'COVER-1/spec#r' || i as id, 'COVER-1/spec' as document from generate_series(1, ` + requirements + `) i
		 union all
		 select 'COVER-1/verification#c' || i, 'COVER-1/verification' from generate_series(1, ` + checks + `) i) block`
	for _, statement := range []string{
		`insert into graph_nodes (kind, id, supertag, visibility, state, made_at, made_by, source_event)
		 values ('issue', 'COVER-1', 'issue', 'project', 'live', now() - interval '2 days', '{}', 0),
		        ('artifact', 'COVER-1/spec', 'document', 'project', 'live', now() - interval '2 days', '{}', 0),
		        ('artifact', 'COVER-1/verification', 'document', 'project', 'live', now() - interval '2 days', '{}', 0)`,
		`insert into graph_nodes (kind, id, supertag, visibility, state, made_at, made_by, source_event)
		 select 'block', block.id, 'block', 'project', 'live', now() - interval '1 day', '{}', 0 from ` + blocks,
		`insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event)
		 select 'block', 'COVER-1/verification#c' || i, '` + servesTupleKind + `', 'block',
		        'COVER-1/spec#r' || ((i + k) % ` + requirements + ` + 1), now() - interval '1 day', '{}'::jsonb, 0
		 from generate_series(1, ` + checks + `) i, generate_series(0, ` + fmt.Sprint(tupleIndexesServed-1) + `) k
		 union all
		 select 'block', block.id, 'part_of', 'issue', 'COVER-1', now() - interval '1 day', '{}'::jsonb, 0 from ` + blocks + `
		 union all
		 select 'block', block.id, '` + refs.TupleInDocument + `', 'artifact', block.document, now() - interval '1 day', '{}'::jsonb, 0 from ` + blocks + `
		 union all
		 select 'block', block.id, 'text', 'value', 'The words of ' || block.id || ', written ' || version || ' times.',
		        now() - interval '1 day' + version * interval '1 minute', '{}'::jsonb, 0
		 from ` + blocks + `, generate_series(1, ` + fmt.Sprint(tupleIndexesTexts) + `) version`,
		`analyze graph_nodes`,
		`analyze graph_tuples`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed the store: %v\n%s", err, statement)
		}
	}
	var seq int64
	var tuples, nodes int
	if err := database.Pool.QueryRow(ctx, `select max(seq), count(*), (select count(*) from graph_nodes) from graph_tuples`).Scan(&seq, &tuples, &nodes); err != nil {
		t.Fatalf("read the seed's size: %v", err)
	}
	t.Logf("the seed holds %d tuples over %d nodes, read at seq %d", tuples, nodes, seq)
	addresses := make([]string, tupleIndexesRequirements)
	for index := range addresses {
		addresses[index] = fmt.Sprintf("COVER-1/spec#r%d", index+1)
	}
	return seq, addresses
}

// tupleIndexesRead is one read of the coverage walk: its name, the statement with the arguments
// its reader binds, and the indexes that serve it.
type tupleIndexesRead struct {
	name      string
	statement string
	args      []any
	through   []string
}

// tupleIndexesUnkeyed is what the test judges a read no index serves in plan: each Seq Scan of
// graph_tuples or graph_nodes, each index scan of either with no index condition that answers
// more than one row a loop, which reads the whole index, and each scan through an index of either
// on a condition that names nothing of that index's first key column, which reads the whole index
// or skips over its first column (explaintest.CondNamesFirstKey).
func tupleIndexesUnkeyed(t *testing.T, keys *explaintest.IndexKeys, plan json.RawMessage) []string {
	t.Helper()
	var unkeyed []string
	for _, node := range explaintest.Nodes(t, plan) {
		graph := strings.HasPrefix(node.Index, "graph_tuples") || strings.HasPrefix(node.Index, "graph_nodes")
		switch {
		case node.Type == "Seq Scan" && (node.Relation == "graph_tuples" || node.Relation == "graph_nodes"):
			unkeyed = append(unkeyed, fmt.Sprintf("%s by a Seq Scan", node.Relation))
		case graph && node.IndexCond == "" && node.Rows > 1:
			unkeyed = append(unkeyed, fmt.Sprintf("%s with no index condition, %v rows a loop: the whole index", node, node.Rows))
		case graph && node.IndexCond != "":
			if firstKey := keys.FirstKey(t, node.Index); !explaintest.CondNamesFirstKey(node.IndexCond, firstKey) {
				unkeyed = append(unkeyed, fmt.Sprintf("%s on `%s`, which names nothing of its first key column `%s`", node, node.IndexCond, firstKey))
			}
		}
	}
	return unkeyed
}

// TestEveryAsOfReadOfThisChildUsesAnIndexOfTheStore is the half of c-tuple-indexes in package api,
// which holds the coverage walk over serves (refs.TestEveryAsOfReadOfThisChildUsesAnIndexOfTheStore
// plans the rest; the check's run, over ./..., runs both). Over a seeded store of about 50,000
// tuples on about 500 nodes, analyzed, the coverage walk from each requirement (servesInto, the
// read GET /api/v1/issues/{key}/coverage walks) and the count of the checks that serve each, as
// the tree read counts them (requirementChecksStatement), each as its reader binds it, are
// explained with analyze and buffers: each reaches graph_tuples through graph_tuples_as_of_in, as
// an Index Scan, an Index Only Scan or a Bitmap Index Scan on a condition that names its first key
// column, with no Seq Scan of graph_tuples or graph_nodes and no whole index read of either. The
// walk answers every requirement with the checks the seed wrote. The walk with its kind left out is
// written in as the control the test must judge unkeyed.
func TestEveryAsOfReadOfThisChildUsesAnIndexOfTheStore(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seq, requirements := seedTupleIndexesCoverage(t, database)
	cut := refs.TreeCut{ReadSeq: seq}

	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction the coverage walk is recorded in: %v", err)
	}
	defer tx.Rollback(ctx)
	recorded := &explaintest.RecordedTx{Tx: tx}
	served, err := servesInto(ctx, recorded, requirements, cut)
	if err != nil {
		t.Fatalf("walk serves into the requirements of COVER-1: %v", err)
	}
	perRequirement := tupleIndexesChecks * tupleIndexesServed / tupleIndexesRequirements
	for _, requirement := range requirements {
		if got := served[requirement]; len(got) != perRequirement || slices.ContainsFunc(got, func(from refs.Target) bool {
			return from.Kind != refs.BlockNodeKind || !strings.HasPrefix(from.ID, "COVER-1/verification#c")
		}) {
			t.Errorf("the coverage walk answers %s served by %v, want the %d checks the seed wrote", requirement, got, perRequirement)
		}
	}
	if len(recorded.Sent) != 1 {
		t.Fatalf("the coverage walk sent %d statements, want one", len(recorded.Sent))
	}
	walk := recorded.Sent[0]
	reads := []tupleIndexesRead{
		{"the coverage walk over serves, from each requirement (servesInto)", walk.SQL, walk.Args, []string{"graph_tuples_as_of_in"}},
		{"the count of the checks that serve each requirement, as the coverage walk counts them (requirementChecksStatement)", requirementChecksStatement(),
			[]any{requirements, seq, refs.VisibilityArg(model.Actor{}), []string{"COVER-1"}}, []string{"graph_tuples_as_of_in"}},
	}

	keys := explaintest.NewIndexKeys(database.Pool)
	for _, read := range reads {
		plan := tupleIndexesPlan(t, database, read.statement, read.args)
		if unkeyed := tupleIndexesUnkeyed(t, keys, plan); len(unkeyed) > 0 {
			t.Errorf("%s reads %s, where the stated index serves a read that binds its kind:\n%s", read.name, strings.Join(unkeyed, "; "), plan)
		}
		scans := explaintest.IndexScans(t, plan)
		if !slices.ContainsFunc(scans, func(scan explaintest.IndexScan) bool { return slices.Contains(read.through, scan.Index) }) {
			t.Errorf("%s reads no part of its plan through %v, which serve it; it reads %v", read.name, read.through, scans)
		}
		t.Logf("%s reads %v", read.name, scans)
	}

	const kindBound = "t.to_kind = '" + refs.BlockNodeKind + "' and "
	if !strings.Contains(walk.SQL, kindBound) {
		t.Fatalf("the coverage walk binds no %q, which the control leaves out:\n%s", kindBound, walk.SQL)
	}
	control := strings.Replace(walk.SQL, kindBound, "", 1)
	if plan := tupleIndexesPlan(t, database, control, walk.Args); len(tupleIndexesUnkeyed(t, keys, plan)) == 0 {
		t.Errorf("the control, the coverage walk with the kind of the requirement left out, plans as a read an index serves, so the test cannot see a read that loses its kind bind:\n%s", plan)
	}
}

// tupleIndexesPlan is the plan of statement under args, explained with analyze and buffers.
func tupleIndexesPlan(t *testing.T, database *store.Store, statement string, args []any) json.RawMessage {
	t.Helper()
	var raw []byte
	if err := database.Pool.QueryRow(context.Background(), "explain (analyze, buffers, format json) "+statement, args...).Scan(&raw); err != nil {
		t.Fatalf("explain %s: %v", statement, err)
	}
	return explaintest.OnePlan(t, raw)
}
