//go:build farm

package legionbench

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// The R8 Job's measurement of the board's reads of the stored facts at root R14's size (spec
// Design, "How R8 is measured"): the median execution time of each of the two statements the
// board's request ran (the drive takes them from Postgres's statement log, with their arguments:
// the project for the read of the open rows, and the project and the board's keys and roots for
// the read of the closed rows) under its generic plan, over boardReadRuns EXPLAIN ANALYZE runs,
// and the two medians' sum, as api's TestTheBoardsFactsReadUnderAGenericPlanStaysWithinItsBudget
// measures them at the test rig's size; and their plans, of which no arm may scan rows of issues
// the reads do not answer. It runs in the R8 Job only, built with the farm tag, against the Job's
// database:
//
//	LEGIONBENCH_DATABASE_URL=<url> LEGIONBENCH_FACTS_READ=<drive's facts-read.json> \
//	  go test -tags farm -run '^TestFarmBoardFactsRead$' -v ./internal/dispatch/legion/legionbench

// boardReadRuns is how many times the read runs under its generic plan, of which the median is
// taken: the board's read-path test's boardReadRuns.
const boardReadRuns = 11

// planNode is one node of an `EXPLAIN (ANALYZE, FORMAT JSON)` plan, in the members the plan check
// reads, with the nodes under it.
type planNode struct {
	Type          string     `json:"Node Type"`
	Relation      string     `json:"Relation Name"`
	Index         string     `json:"Index Name"`
	IndexCond     string     `json:"Index Cond"`
	RecheckCond   string     `json:"Recheck Cond"`
	Filter        string     `json:"Filter"`
	Rows          float64    `json:"Actual Rows"`
	Loops         float64    `json:"Actual Loops"`
	RemovedFilter float64    `json:"Rows Removed by Filter"`
	RemovedCheck  float64    `json:"Rows Removed by Index Recheck"`
	Total         float64    `json:"Actual Total Time"`
	Plans         []planNode `json:"Plans"`
}

// walk is n and every node under it, depth first.
func (n planNode) walk() []planNode {
	nodes := []planNode{n}
	for _, child := range n.Plans {
		nodes = append(nodes, child.walk()...)
	}
	return nodes
}

// outline is the plan one node a line, each under its parent: its type, the relation and the
// index it reads, its conditions, and what it ran.
func (n planNode) outline(depth int, lines *[]string) {
	line := strings.Repeat("  ", depth) + n.Type
	if n.Relation != "" {
		line += " on " + n.Relation
	}
	if n.Index != "" {
		line += " using " + n.Index
	}
	if n.IndexCond != "" {
		line += " cond=" + n.IndexCond
	}
	if n.Filter != "" {
		line += " filter=" + n.Filter
	}
	*lines = append(*lines, line+fmt.Sprintf(" (rows=%g loops=%g removed_by_filter=%g time=%.3f ms)", n.Rows, n.Loops, n.RemovedFilter, n.Total))
	for _, child := range n.Plans {
		child.outline(depth+1, lines)
	}
}

func TestFarmBoardFactsRead(t *testing.T) {
	url, path := os.Getenv("LEGIONBENCH_DATABASE_URL"), os.Getenv("LEGIONBENCH_FACTS_READ")
	if url == "" || path == "" {
		t.Fatal("set LEGIONBENCH_DATABASE_URL to the R8 Job's database and LEGIONBENCH_FACTS_READ to the drive's facts-read.json")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the board's facts reads: %v", err)
	}
	type captured struct {
		SQL    string   `json:"sql"`
		Params []string `json:"params"`
	}
	var reads struct {
		Open   captured `json:"open"`
		Closed captured `json:"closed"`
	}
	if err := json.Unmarshal(data, &reads); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if len(reads.Open.Params) != 1 || reads.Open.SQL == "" || len(reads.Closed.Params) != 3 || reads.Closed.SQL == "" {
		t.Fatalf("%s holds reads of %d and %d parameters, want the read of the open rows' project and the read of the closed rows' project, keys and roots",
			path, len(reads.Open.Params), len(reads.Closed.Params))
	}
	if reads.Open.Params[0] != reads.Closed.Params[0] {
		t.Fatalf("%s holds reads of two projects, %s and %s", path, reads.Open.Params[0], reads.Closed.Params[0])
	}
	ctx := t.Context()
	database, err := store.Open(ctx, url)
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	defer database.Pool.Close()
	if _, err := database.Pool.Exec(ctx, `analyze issue_facts`); err != nil {
		t.Fatalf("analyze issue_facts: %v", err)
	}
	project, keys, roots := reads.Closed.Params[0], reads.Closed.Params[1]+"::text[]", reads.Closed.Params[2]+"::text[]"
	var rows int64
	var sets armSets
	if err := database.Pool.QueryRow(ctx, fmt.Sprintf(`
		select (select count(*) from issue_facts where project_key = %[1]s),
		       (select count(*) from issue_facts where project_key = %[1]s and closed_at is null),
		       (select count(*) from issue_facts where project_key = %[1]s and root_key = any(%[3]s)),
		       (select count(*) from issue_facts where project_key = %[1]s and issue_key = any(%[2]s)),
		       cardinality(%[2]s), cardinality(%[3]s)`, project, keys, roots)).
		Scan(&rows, &sets.open, &sets.trees, &sets.cards, &sets.keys, &sets.roots); err != nil {
		t.Fatalf("count the stored rows the reads name: %v", err)
	}
	connection, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the generic plans: %v", err)
	}
	defer connection.Release()
	statements := []struct {
		name, sql        string
		types, literals  []string
		median           float64
		fastest, slowest float64
		root             planNode
	}{
		{name: "open", sql: reads.Open.SQL, types: []string{"text"}, literals: []string{project}},
		{name: "closed", sql: reads.Closed.SQL, types: []string{"text", "text[]", "text[]"}, literals: []string{project, keys, roots}},
	}
	generic := func(name, statement string, types, literals []string) planNode {
		plan := explaintest.AnalyzedGenericPlan(t, ctx, connection, name, statement, types, literals, "set local enable_seqscan = off")
		var root planNode
		if err := json.Unmarshal(plan, &root); err != nil {
			t.Fatalf("decode the plan: %v", err)
		}
		return root
	}

	var median, answered float64
	var failures []string
	for index := range statements {
		statement := &statements[index]
		taken := make([]float64, 0, boardReadRuns)
		for run := range boardReadRuns {
			statement.root = generic(fmt.Sprintf("board_facts_read_%s_%d", statement.name, run), statement.sql, statement.types, statement.literals)
			taken = append(taken, statement.root.Total)
		}
		slices.Sort(taken)
		statement.median, statement.fastest, statement.slowest = taken[len(taken)/2], taken[0], taken[len(taken)-1]
		median += statement.median
		answered += statement.root.Rows * statement.root.Loops
		failures = append(failures, checkArms(statement.root, sets, true)...)
	}
	open, closed := statements[0], statements[1]
	fmt.Printf("RESULT r8 facts_read runs=%d median_ms=%.3f open_median_ms=%.3f open_fastest_ms=%.3f open_slowest_ms=%.3f closed_median_ms=%.3f closed_fastest_ms=%.3f closed_slowest_ms=%.3f stored_rows=%d open_rows=%d keys=%d roots=%d tree_rows=%d card_rows=%d\n",
		boardReadRuns, median, open.median, open.fastest, open.slowest, closed.median, closed.fastest, closed.slowest,
		rows, sets.open, sets.keys, sets.roots, sets.trees, sets.cards)
	for _, statement := range statements {
		fmt.Printf("RESULT r8 facts_read plan of the %s rows' read (generic, the last run):\n", statement.name)
		var outline []string
		statement.root.outline(0, &outline)
		for _, line := range outline {
			fmt.Println("    " + line)
		}
	}
	for _, failure := range failures {
		fmt.Println("FAIL r8 facts_read " + failure)
	}
	fmt.Printf("RESULT r8 facts_read plan_verdict=%s answered_rows=%g\n", map[bool]string{true: "pass", false: "fail"}[len(failures) == 0], answered)

	// The control: the read of the closed rows with its trees' arm written so no index answers its
	// list (the root key read through an expression), whose plan reads the project's rows past the
	// trees the read names. The check must refuse that plan, or it cannot see the arm it is there
	// to catch.
	wide := strings.Replace(closed.sql, "root_key = any($3)", "root_key || '' = any($3)", 1)
	if wide == closed.sql {
		fmt.Println("FAIL r8 facts_read control: the closed rows' read holds no `root_key = any($3)`, so the widened read was not built")
		t.Fail()
	} else if caught := checkArms(generic("board_facts_read_wide", wide, closed.types, closed.literals), sets, false); len(caught) == 0 {
		fmt.Println("FAIL r8 facts_read control: the plan check passed a read whose trees' arm reads past the trees it names")
		t.Fail()
	} else {
		fmt.Printf("RESULT r8 facts_read control caught=%q\n", caught)
	}
	if len(failures) > 0 {
		t.Fail()
	}
}

// armSets is how many of the project's stored rows each arm of the board's read names by its
// index condition: the open rows, the rows of the trees whose roots it names, and the rows of the
// cards it names; and how many keys and roots the read names.
type armSets struct {
	open, trees, cards, keys, roots int64
}

// checkArms answers what is wrong with a plan of the board's read of the stored facts, nothing for
// a plan whose every arm reads its rows through an index condition naming one of the read's three
// sets (the open rows, the trees' rows by root, the cards' rows by key) and reads no more rows than
// that set holds. Every row of each set is a row the read answers: an open row through the open
// arm, a tree's closed row through the trees' arm, and a card's closed row through the trees' arm
// or the cards'. So an arm within its set reads no row of an issue the read does not answer, the
// rows another arm answers and this one's filter drops included, and an arm past its set, or under
// no such condition, does. The rows an arm reads are those it answered and those its filter or its
// recheck dropped, over every loop. print prints a line for each arm.
func checkArms(root planNode, sets armSets, print bool) []string {
	var failures []string
	arms := 0
	for _, node := range root.walk() {
		if node.Relation != "issue_facts" {
			continue
		}
		arms++
		read := (node.Rows + node.RemovedFilter + node.RemovedCheck) * node.Loops
		condition := node.IndexCond + " " + node.RecheckCond
		set, bound := "", int64(-1)
		switch {
		case strings.Contains(condition, "closed_at IS NULL"):
			set, bound = "open", sets.open
		case strings.Contains(condition, "root_key = ANY"):
			set, bound = "trees", sets.trees
		case strings.Contains(condition, "issue_key = ANY"):
			set, bound = "cards", sets.cards
		}
		if print {
			fmt.Printf("RESULT r8 facts_read arm node=%q index=%q cond=%q filter=%q set=%s set_rows=%d rows=%g loops=%g removed_by_filter=%g removed_by_recheck=%g read=%g\n",
				node.Type, node.Index, strings.TrimSpace(condition), node.Filter, set, bound, node.Rows, node.Loops, node.RemovedFilter,
				node.RemovedCheck, read)
		}
		switch {
		case strings.Contains(node.Type, "Seq Scan"):
			failures = append(failures, fmt.Sprintf("an arm scans issue_facts whole (%s)", node.Type))
		case set == "":
			failures = append(failures, fmt.Sprintf("an arm (%s on %q) reads under no condition naming the open rows, the trees or the cards: it scans rows of issues the read does not answer", node.Type, strings.TrimSpace(condition)))
		case read > float64(bound):
			failures = append(failures, fmt.Sprintf("the %s arm (%s) reads %g rows, more than the %d its set holds: it scans rows of issues the read does not answer", set, node.Type, read, bound))
		}
	}
	if arms == 0 {
		failures = append(failures, "the plan reads issue_facts in no node, so no arm was judged")
	}
	return failures
}
