package api

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// nearCopyPlanStatement is one statement of the near-copy gate as a plan test prepares it: the
// placeholder the creator's viewer binds, and the parameter types and literals its prepared form
// is explained with.
type nearCopyPlanStatement struct {
	name, sql, viewer    string
	parameters, literals []string
}

// nearCopyPlanStatements are the gate's two statements, each bound to an open full member who
// sees one project (planViewer), so no arm of the project clause is folded away.
func nearCopyPlanStatements() []nearCopyPlanStatement {
	viewer := "'" + string(planViewer) + "'"
	options := "'" + duplicateHeadlineOptions + "'"
	return []nearCopyPlanStatement{{
		name: "the scoring statement", sql: duplicateScoreQuery, viewer: "$8",
		parameters: []string{"text", "text[]", "float8[]", "text[]", "float8", "text", "text", "jsonb"},
		literals:   []string{"'TEST'", "array['astrolab']", "array[1]::float8[]", "'{}'", "0.6", options, "'Astrolabe readings'", viewer},
	}, {
		name: "the equal-lexemes statement", sql: duplicateEqualLexemesQuery, viewer: "$6",
		parameters: []string{"text", "text", "text", "text[]", "text", "jsonb"},
		literals:   []string{"'TEST'", "'Astrolabe readings'", "''", "'{}'", options, viewer},
	}}
}

// The near-copy gate reads one project, $1, so each of its statements decides once whether the
// creator's viewer sees that project, never once per row: its project clause tests the parameter
// rather than a row's project_key. A prepared statement runs, from a connection's sixth run on,
// the generic plan Postgres makes without its arguments, and there a clause over the row's column
// is a filter of every issue whose share Postgres guesses, the viewer being a parameter it cannot
// read; over the parameter alone it is a One-Time Filter, decided before any row is read and
// counted in no estimate, so the generic plan reads issues as it would with no viewer at all.
//
// The test reads the generic plan of each statement and holds every condition that tests a row to
// naming no viewer parameter, and requires a One-Time Filter that does, so a plan that bound no
// viewer at all cannot pass. Its control is each statement with the clause over the row's project,
// as the gate wrote it before: the same check must find the viewer in a row's filter there, or it
// is a check that cannot fail.
func TestTheNearCopyGateDecidesItsViewerOncePerStatement(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	connection, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the plans: %v", err)
	}
	defer connection.Release()
	for number, statement := range nearCopyPlanStatements() {
		t.Run(statement.name, func(t *testing.T) {
			plan := explaintest.GenericPlan(t, ctx, connection, fmt.Sprintf("near_copy_%d", number),
				statement.sql, statement.parameters, statement.literals)
			perRow, once := viewerConditions(t, plan, statement.viewer)
			if len(perRow) > 0 {
				t.Errorf("%s's generic plan tests rows by the viewer %s: %q\n%s", statement.name, statement.viewer, perRow, plan)
			}
			if once == 0 {
				t.Errorf("%s's generic plan decides the viewer %s in no One-Time Filter:\n%s", statement.name, statement.viewer, plan)
			}

			rowClause := access.ProjectClause("i.project_key", statement.viewer)
			control := strings.ReplaceAll(statement.sql, access.ProjectClause("$1", statement.viewer), rowClause)
			if control == statement.sql {
				t.Fatalf("the control of %s is the statement itself: it holds no project clause over $1", statement.name)
			}
			controlPlan := explaintest.GenericPlan(t, ctx, connection, fmt.Sprintf("near_copy_control_%d", number),
				control, statement.parameters, statement.literals)
			if perRow, _ := viewerConditions(t, controlPlan, statement.viewer); len(perRow) == 0 {
				t.Errorf("the control of %s, its clause over each row's project, tests no row by the viewer %s, so the check cannot fail:\n%s",
					statement.name, statement.viewer, controlPlan)
			}
		})
	}
}

// viewerConditions reads plan, one "Plan" node of `EXPLAIN (FORMAT JSON)`, for the conditions that
// name viewer, the viewer's placeholder: every one a node tests rows by (a Filter, a Join Filter,
// and an index, hash, merge or recheck condition), each as its key and text, and the count of
// One-Time Filters, which a node tests once before it reads a row.
func viewerConditions(t *testing.T, plan json.RawMessage, viewer string) ([]string, int) {
	t.Helper()
	named := regexp.MustCompile(regexp.QuoteMeta(viewer) + `\b`)
	var perRow []string
	once := 0
	for _, condition := range planConditions(t, plan) {
		switch {
		case !named.MatchString(condition.text):
		case condition.key == "One-Time Filter":
			once++
		default:
			perRow = append(perRow, condition.key+": "+condition.text)
		}
	}
	return perRow, once
}

// planCondition is one condition of a plan node: its key, "Filter", "Index Cond", "One-Time
// Filter" and the like, and the plan's own text of it.
type planCondition struct{ key, text string }

// planConditions is every condition of plan, one "Plan" node of `EXPLAIN (FORMAT JSON)`, at any
// depth: each member of a node whose key ends in "Filter" or "Cond".
func planConditions(t *testing.T, plan json.RawMessage) []planCondition {
	t.Helper()
	var decoded any
	if err := json.Unmarshal(plan, &decoded); err != nil {
		t.Fatalf("decode the plan: %v\n%s", err, plan)
	}
	var conditions []planCondition
	var walk func(any)
	walk = func(value any) {
		switch typed := value.(type) {
		case map[string]any:
			for key, field := range typed {
				if text, ok := field.(string); ok && (strings.HasSuffix(key, "Filter") || strings.HasSuffix(key, "Cond")) {
					conditions = append(conditions, planCondition{key: key, text: text})
				}
				walk(field)
			}
		case []any:
			for _, item := range typed {
				walk(item)
			}
		}
	}
	walk(decoded)
	return conditions
}

// planParameter matches a statement's parameter as a plan writes it: a plan made for the
// statement's own arguments writes each as its value, and only a generic plan names one.
var planParameter = regexp.MustCompile(`\$\d+\b`)

// TestTheNearCopyScoreIsPlannedForItsArgumentsOnEveryRun runs the gate's read planRuns times on
// one held connection of the server's own pool, as TestPrioritiesReadKeepsItsPlans runs the
// priorities read, and holds the scoring statement of every run, as auto_explain reports it, to a
// plan made for that run's arguments: one no condition of which names a parameter. A pooled
// statement Postgres may answer from its sixth run with the one generic plan it then keeps, made
// without the arguments, whose conditions name $1 and the rest.
//
// The control is the same statement with the same arguments sent the way the pool sends a
// statement by default, prepared once and cached on the same connection: Postgres must answer
// some run of it with a generic plan, or this fixture could not tell the two plans apart.
func TestTheNearCopyScoreIsPlannedForItsArgumentsOnEveryRun(t *testing.T) {
	handler, database, deps, _ := newRegisteredTestServer(t, testServerOptions{})
	fixture := newDuplicateVocabulary(t, handler, "PLANS", duplicateNudgeScore)
	ctx := context.Background()
	if _, err := refreshSearchTermStats(ctx, database.Pool, fixture.project); err != nil {
		t.Fatalf("refresh %s's term statistics: %v", fixture.project, err)
	}
	plans := &explainedStatements{}
	held, backend := explainingConnection(t, database, plans)
	heldDeps := deps
	heldDeps.Store = held
	served := directServer(heldDeps)

	// scoringPlan is the plan of the one scoring statement the held connection reported since the
	// last take.
	scoringPlan := func(run int, read string) json.RawMessage {
		t.Helper()
		var found []json.RawMessage
		for _, statement := range plans.take(t) {
			if statement.backend != backend {
				t.Fatalf("run %d of %s: a plan came from server process %d, want the held connection's %d", run, read, statement.backend, backend)
			}
			if statement.text == duplicateScoreQuery {
				found = append(found, statement.plan)
			}
		}
		if len(found) != 1 {
			t.Fatalf("run %d of %s reported %d plans of the scoring statement, want 1", run, read, len(found))
		}
		return found[0]
	}
	parameters := func(plan json.RawMessage) []string {
		var named []string
		for _, condition := range planConditions(t, plan) {
			if planParameter.MatchString(condition.text) {
				named = append(named, condition.key+": "+condition.text)
			}
		}
		return named
	}

	for run := 1; run <= planRuns; run++ {
		plans.take(t)
		if _, err := served.readDuplicateCandidates(ctx, everyReader, fixture.project, fixture.title, fixture.spec, nil,
			nearCopyVerdictBound(t), duplicateNudgeScore); err != nil {
			t.Fatalf("run %d of the gate's read: %v", run, err)
		}
		if named := parameters(scoringPlan(run, "the gate's read")); len(named) > 0 {
			t.Errorf("run %d of the gate's read ran the scoring statement under a generic plan: %q", run, named)
		}
	}

	picked, err := served.pickRelatedTerms(ctx, held.Pool, everyReader, fixture.project,
		duplicateGateText(fixture.title, fixture.spec), fixture.title, relatedExclusion{}, priceUnknownTerms)
	if err != nil {
		t.Fatalf("pick the control's terms: %v", err)
	}
	keyLexemes, err := duplicateKeyLexemes(ctx, held.Pool, fixture.project)
	if err != nil {
		t.Fatalf("read %s's key lexemes: %v", fixture.project, err)
	}
	lexemes, weights := nearCopyScoreTerms(picked, keyLexemes)
	// The gate's own arguments less the first, the mode that has each run planned for them.
	cached := duplicateScoreArguments(fixture.project, lexemes, weights, []string{}, duplicateNudgeScore, fixture.title, everyReader)[1:]
	generic := 0
	for run := 1; run <= planRuns; run++ {
		plans.take(t)
		rows, err := held.Pool.Query(ctx, duplicateScoreQuery, cached...)
		if err != nil {
			t.Fatalf("run %d of the control: %v", run, err)
		}
		if _, err := scanDuplicateCandidates(rows); err != nil {
			t.Fatalf("read run %d of the control: %v", run, err)
		}
		if len(parameters(scoringPlan(run, "the control"))) > 0 {
			generic++
		}
	}
	if generic == 0 {
		t.Fatalf("the control, the scoring statement prepared and cached on the held connection, ran no generic plan in %d runs, "+
			"so this fixture cannot tell a generic plan from one made for the arguments", planRuns)
	}
	t.Logf("the control ran a generic plan in %d of its %d runs", generic, planRuns)
}
