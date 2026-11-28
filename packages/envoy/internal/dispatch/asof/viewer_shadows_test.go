package asof_test

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/asof"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// shadowKeyTables are, for each shadowed table, the tables with row security whose rows its shadow
// reads in its own statement, under the viewer's row security: the present rows that give the
// shadow its rows and its keys (plan D6, "Reads at a past time"). Any other table with row
// security, the event log above all, holds rows of other scopes, and a shadow reads it only
// through an owner-run function keyed by a row it read here (F2), so the planner keeps the event
// log's indexes and no row of another scope reaches the answer. A shadow added later fails
// TestEveryShadowKeepsTheViewer until its key tables are reviewed here.
var shadowKeyTables = map[string][]string{
	"events":                   {"events"},
	"issues":                   {"issues", "artifacts", "artifact_versions"},
	"issue_links":              {"issues", "issue_links"},
	"issue_external_links":     {"issues"},
	"issue_components":         {"issues", "issue_components"},
	"priority_tuples":          {"priority_tuples"},
	"projects":                 {"projects", "repo_projects"},
	"repo_projects":            {"repo_projects"},
	"asks":                     {"asks", "comments"},
	"ask_followers":            {"asks", "comments"},
	"comments":                 {"comments", "post_body_history"},
	"messages":                 {"messages", "post_body_history"},
	"comment_deliveries":       {"comment_deliveries", "comments"},
	"message_deliveries":       {"message_deliveries", "messages"},
	"artifacts":                {"artifacts"},
	"artifact_versions":        {"artifact_versions"},
	"artifact_reviews":         {"artifact_reviews"},
	"check_results":            {"check_results"},
	"check_result_retractions": {"check_result_retractions"},
}

// planRelations names, sorted and once each, every relation a plan of EXPLAIN (format json) scans,
// in its subplans and init plans too.
func planRelations(t *testing.T, explained []byte) []string {
	t.Helper()
	var plans []map[string]any
	if err := json.Unmarshal(explained, &plans); err != nil || len(plans) == 0 {
		t.Fatalf("read the plan %s: %v", explained, err)
	}
	found := map[string]bool{}
	var walk func(node map[string]any)
	walk = func(node map[string]any) {
		if name, ok := node["Relation Name"].(string); ok {
			found[name] = true
		}
		children, _ := node["Plans"].([]any)
		for _, child := range children {
			if plan, ok := child.(map[string]any); ok {
				walk(plan)
			}
		}
	}
	root, _ := plans[0]["Plan"].(map[string]any)
	walk(root)
	return slices.Sorted(maps.Keys(found))
}

// TestEveryShadowKeepsTheViewer (plan D6, "Reads at a past time"; R3): a read at a cut keeps the
// request's viewer. Each shadow reads, in its own statement and under the viewer's row security,
// only the present rows that give it its keys (shadowKeyTables) and tables that hold no project's
// content (no row security); the rows of other scopes, the event log's above all, it finds through
// an owner-run function keyed by those rows, so a plan under the request role reads no other table
// with row security. At a cut of now, each shadow answers a viewer exactly the rows its table
// answers that viewer at the present: none of a private project the viewer does not see, so no
// owner-run step hands it a row of another scope, and every one of a project it sees. Each table a
// read at a cut reads with no shadow (asof.CutReadTables) is read under its own row security.
//
// The seed is a row of every shadowed table in the project ASOF (seedEveryShadowedTable), made
// private.
func TestEveryShadowKeepsTheViewer(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	var unreviewed []string
	for _, table := range asof.ShadowedTables() {
		if _, ok := shadowKeyTables[table]; !ok {
			unreviewed = append(unreviewed, table)
		}
	}
	if len(unreviewed) > 0 {
		t.Errorf("shadows whose key tables are not reviewed (shadowKeyTables): %s", strings.Join(unreviewed, ", "))
	}

	rowSecurity := map[string]bool{}
	rows, err := w.database.Pool.Query(ctx, `
		select c.relname::text, c.relrowsecurity and exists (select from pg_policy p where p.polrelid = c.oid and p.polname = 'viewer')
		from pg_class c where c.relnamespace = 'public'::regnamespace and c.relkind in ('r', 'p')`)
	if err != nil {
		t.Fatalf("read the tables' row security: %v", err)
	}
	for rows.Next() {
		var name string
		var secured bool
		if err := rows.Scan(&name, &secured); err != nil {
			t.Fatalf("read a table's row security: %v", err)
		}
		rowSecurity[name] = secured
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the tables' row security: %v", err)
	}
	var unsecured []string
	for _, table := range slices.Sorted(maps.Keys(asof.CutReadTables)) {
		if !rowSecurity[table] {
			unsecured = append(unsecured, table)
		}
	}
	if len(unsecured) > 0 {
		t.Errorf("tables a read at a cut reads with no shadow (asof.CutReadTables) that hold no row security with a viewer policy, so a read at a cut reads every scope's rows of them: %s",
			strings.Join(unsecured, ", "))
	}

	seedEveryShadowedTable(t, w)
	w.exec(`update projects set visibility = 'private' where key = 'ASOF'`)
	var now string
	if err := w.database.Pool.QueryRow(ctx, `select to_char(clock_timestamp() at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')`).Scan(&now); err != nil {
		t.Fatalf("read the database's now: %v", err)
	}
	cut := mustCut(t, now)
	request := storetest.Request(t, w.database)
	blind := store.WithViewer(ctx, model.Viewer{Person: "dave@example.com", Open: true, Sees: []string{}, AnyPrivate: true})
	seeing := store.WithViewer(ctx, model.Viewer{Person: "carol@example.com", Open: true, Sees: []string{"ASOF"}, AnyPrivate: true})

	for _, table := range asof.ShadowedTables() {
		t.Run("shadow of "+table, func(t *testing.T) {
			var explained []byte
			if err := request.Pool.QueryRow(blind, "explain (format json) with "+asof.Shadows(cut, table)+"\nselect * from "+table).Scan(&explained); err != nil {
				t.Fatalf("plan the %s shadow under the request role: %v", table, err)
			}
			var outside []string
			for _, relation := range planRelations(t, explained) {
				if rowSecurity[relation] && !slices.Contains(shadowKeyTables[table], relation) {
					outside = append(outside, relation)
				}
			}
			if len(outside) > 0 {
				t.Errorf("the %s shadow reads %s in its own statement, under the viewer's row security and keyed by no row it read as its own: read them through an owner-run function keyed by such a row (F2)",
					table, strings.Join(outside, ", "))
			}
			for name, viewed := range map[string]context.Context{"a viewer who does not see ASOF": blind, "a viewer who sees ASOF": seeing} {
				var present, atTheCut int
				if err := request.Pool.QueryRow(viewed, "select count(*) from "+table).Scan(&present); err != nil {
					t.Fatalf("%s: count %s: %v", name, table, err)
				}
				if err := request.Pool.QueryRow(asof.WithCut(viewed, cut), "select count(*) from "+table).Scan(&atTheCut); err != nil {
					t.Fatalf("%s: count %s at a cut of now: %v", name, table, err)
				}
				if atTheCut != present {
					t.Errorf("%s: the %s shadow at a cut of now answers %d rows, and the table at the present %d: a read at a cut must keep the viewer",
						name, table, atTheCut, present)
				}
			}
		})
	}
}
