package api

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// rollbackEdge is one edge graph_edges answers, by its ends and its kind.
type rollbackEdge struct{ fromKind, fromID, kind, toKind, toID string }

// viewAnswers is how many rows graph_edges answers edge with, and the created_at of the last.
func viewAnswers(t *testing.T, database *store.Store, edge rollbackEdge) (int, time.Time) {
	t.Helper()
	var rows int
	var at *time.Time
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*), max(created_at) from graph_edges
		where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4 and to_id = $5
	`, edge.fromKind, edge.fromID, edge.kind, edge.toKind, edge.toID).Scan(&rows, &at); err != nil {
		t.Fatalf("read %v out of graph_edges: %v", edge, err)
	}
	if at == nil {
		return rows, time.Time{}
	}
	return rows, *at
}

// graphEdgesArm is the one arm of graph_edges, as the catalogue prints the view back, whose text
// holds every one of marks: a statement of its own, which an edge is read through to tell which arm
// answers it. The pretty form puts each top-level UNION ALL on a line of its own, and no arm holds
// a union of its own, so splitting on that line splits the arms.
func graphEdgesArm(t *testing.T, database *store.Store, what string, marks ...string) string {
	t.Helper()
	var definition string
	if err := database.Pool.QueryRow(context.Background(),
		`select pg_get_viewdef('graph_edges'::regclass, true)`).Scan(&definition); err != nil {
		t.Fatalf("read the graph_edges definition: %v", err)
	}
	var found []string
	for _, arm := range strings.Split(strings.TrimSuffix(strings.TrimRight(definition, "\n"), ";"), "\nUNION ALL\n") {
		if !slices.ContainsFunc(marks, func(mark string) bool { return !strings.Contains(arm, mark) }) {
			found = append(found, arm)
		}
	}
	if len(found) != 1 {
		t.Fatalf("graph_edges holds %d arms that read as %s (%q), want one:\n%s", len(found), what, marks, definition)
	}
	return found[0]
}

// armAnswers is how many rows arm, one arm of graph_edges read on its own, answers edge with.
func armAnswers(t *testing.T, database *store.Store, arm string, edge rollbackEdge) int {
	t.Helper()
	var rows int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from (`+arm+`) arm (from_kind, from_id, kind, to_kind, to_id, created_at, source_seq)
		where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4 and to_id = $5
	`, edge.fromKind, edge.fromID, edge.kind, edge.toKind, edge.toID).Scan(&rows); err != nil {
		t.Fatalf("read %v out of one arm of graph_edges: %v", edge, err)
	}
	return rows
}

// outboundAnswers is how many edges of kind GET /api/v1/references?from=<issue> answers to target.
func outboundAnswers(t *testing.T, handler http.Handler, issue, kind, targetKind, target string) int {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/references?from="+url.QueryEscape("dispatch://"+issue), nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the references out of %s: status=%d body=%s", issue, response.Code, response.Body.String())
	}
	answered := 0
	for _, edge := range decodeBody[graphReferencesRead](t, response).Edges {
		if edge.Kind == kind && edge.Node.Kind == targetKind && edge.Node.ID == target {
			answered++
		}
	}
	return answered
}

// rollbackComponentFiles is the model of project ROLL's one architecture source: legion holds
// daemon and envoy, daemon holds workspace and depends on envoy, and envoy holds listener and
// server, which depends on listener.
func rollbackComponentFiles() map[string]string {
	return map[string]string{
		"legion.md":    "---\ntitle: Legion\n---\nThe swarm.\n",
		"daemon.md":    "---\ntitle: Daemon\nparent: legion\ndepends_on: [envoy]\n---\nThe daemon.\n",
		"envoy.md":     "---\ntitle: Envoy\nparent: legion\n---\nRouting.\n",
		"workspace.md": "---\ntitle: Workspace\nparent: daemon\n---\nIssue workspaces.\n",
		"listener.md":  "---\ntitle: Listener\nparent: envoy\n---\nThe listener.\n",
		"server.md":    "---\ntitle: Server\nparent: envoy\ndepends_on: [listener]\n---\nThe server.\n",
	}
}

// movedInPlace is one relation the release before moved in place on a marked row: the edge its
// new value names and the edge its old value named, the column's owner for a failure, and the old
// arm that answers the row while its marker does not mark it.
type movedInPlace struct {
	what       string
	now, left  rollbackEdge
	oldArmName string
	oldArm     string
}

// rollbackRelationTables are the tables graph_edges reads an issue's and a component's relations
// out of: each old arm reads its own, and the tuple arm probes the one whose row projects an edge.
var rollbackRelationTables = []string{"issues", "issue_links", "components", "component_depends", "issue_component_members"}

// explainRollbackRead is statement's plan made for its arguments, run under EXPLAIN (ANALYZE,
// BUFFERS, FORMAT JSON), which the checks read, inside a transaction it rolls back; its text, which
// a failure prints, is explainDismissedScaleText's.
func explainRollbackRead(t *testing.T, connection *pgx.Conn, name, statement string, args []any) json.RawMessage {
	t.Helper()
	ctx := context.Background()
	tx, err := connection.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the explain of %s: %v", name, err)
	}
	defer tx.Rollback(ctx)
	var planJSON []byte
	if err := tx.QueryRow(ctx, "explain (analyze, buffers, format json) "+statement, args...).Scan(&planJSON); err != nil {
		t.Fatalf("explain %s: %v", name, err)
	}
	return explaintest.OnePlan(t, planJSON)
}

// seedRollbackScale writes into project ROLL, by SQL, 500 issues, ROLL-1001 to ROLL-1500, each but
// the first a child of the issue at half its number and each waiting on the 100 issues after it,
// 50,000 issue links; 5,000 components of ROLL's source, bulk-1 to bulk-5000, each a part of the
// component at half its number (bulk-1 of legion) and each depending on the 10 after it, 50,000
// dependencies; and 100 components each issue holds as its own, 50,000 issue components. The rows
// of an even issue and of an even component are the ones this build's writers leave: each names
// its node, which stands, made as issueNode's and componentNode's were, and so does the tuple each
// row projects, which the tuple arm answers. The rows of an odd one are the ones a build of the
// release before leaves, naming no node, which the old arms answer. Then it analyzes the database.
func seedRollbackScale(t *testing.T, database *store.Store, issueNode, componentNode string) {
	t.Helper()
	ctx := context.Background()
	const who = `'{"kind":"user","id":"alice"}'::jsonb`
	const nodeColumns = `kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event, project_key, parties`
	for _, statement := range []struct {
		what, sql string
		args      []any
	}{
		{"the issues", `
			insert into issues (key, project_key, number, title, created_by, rank, parent_key, node_kind, node_address)
			select 'ROLL-' || (1000 + n), 'ROLL', 1000 + n, 'Bulk issue ' || n, ` + who + `, 'z' || lpad(n::text, 4, '0'),
			       case when n > 1 then 'ROLL-' || (1000 + n / 2) end,
			       case when n % 2 = 0 then 'issue' end, case when n % 2 = 0 then 'ROLL-' || (1000 + n) end
			from generate_series(1, 500) n`, nil},
		{"the issues' nodes", `
			insert into graph_nodes (` + nodeColumns + `)
			select node.kind, i.node_address, node.supertag, node.visibility, node.state, node.attributes, node.made_at, node.made_by, 0,
			       node.project_key, node.parties
			from issues i join graph_nodes node on node.kind = 'issue' and node.id = $1
			where i.project_key = 'ROLL' and i.number > 1000 and i.node_kind is not null`, []any{issueNode}},
		{"the issues' child_of", `
			insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
			select 'issue', i.key, 'child_of', 'issue', i.parent_key, i.created_at, ` + who + `, 0, true
			from issues i
			where i.project_key = 'ROLL' and i.number > 1000 and i.node_kind is not null and i.parent_key is not null`, nil},
		{"the issue links", `
			insert into issue_links (issue_key, kind, target_key, created_at, node_kind, node_address)
			select i.key, 'blocked_by', 'ROLL-' || (1001 + (i.number - 1001 + k) % 500), now() - interval '1 day', i.node_kind, i.node_address
			from issues i cross join generate_series(1, 100) k
			where i.project_key = 'ROLL' and i.number > 1000`, nil},
		{"the issue links' needs", `
			insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
			select 'issue', l.issue_key, 'needs', 'issue', l.target_key, l.created_at, ` + who + `, 0, true
			from issue_links l join issues i on i.key = l.issue_key
			where i.project_key = 'ROLL' and i.number > 1000 and l.node_kind is not null`, nil},
		{"the components", `
			insert into components (project_key, id, title, prose, parent, snapshot_id, source_id, node_kind, node_address)
			select 'ROLL', 'bulk-' || n, 'Bulk ' || n, 'A bulk component.', case when n > 1 then 'bulk-' || (n / 2) else 'legion' end,
			       legion.snapshot_id, legion.source_id,
			       case when n % 2 = 0 then 'component' end, case when n % 2 = 0 then 'ROLL/bulk-' || n end
			from components legion cross join generate_series(1, 5000) n
			where legion.project_key = 'ROLL' and legion.id = 'legion'`, nil},
		{"the components' nodes", `
			insert into graph_nodes (` + nodeColumns + `)
			select node.kind, c.node_address, node.supertag, node.visibility, node.state, node.attributes, node.made_at, node.made_by, 0,
			       node.project_key, node.parties
			from components c join graph_nodes node on node.kind = 'component' and node.id = $1
			where c.project_key = 'ROLL' and c.id like 'bulk-%' and c.node_kind is not null`, []any{componentNode}},
		{"the components' part_of", `
			insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
			select 'component', c.node_address, 'part_of', 'component', 'ROLL/' || c.parent, s.imported_at, ` + who + `, 0, true
			from components c join architecture_snapshots s on s.id = c.snapshot_id
			where c.project_key = 'ROLL' and c.id like 'bulk-%' and c.node_kind is not null`, nil},
		{"the component dependencies", `
			insert into component_depends (project_key, from_id, to_id, node_kind, node_address)
			select 'ROLL', 'bulk-' || n, 'bulk-' || (1 + (n - 1 + k) % 5000),
			       case when n % 2 = 0 then 'component' end, case when n % 2 = 0 then 'ROLL/bulk-' || n end
			from generate_series(1, 5000) n cross join generate_series(1, 10) k`, nil},
		{"the component dependencies' depends_on", `
			insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
			select 'component', d.node_address, 'depends_on', 'component', 'ROLL/' || d.to_id, s.imported_at, ` + who + `, 0, true
			from component_depends d
			join components c on c.project_key = d.project_key and c.id = d.from_id
			join architecture_snapshots s on s.id = c.snapshot_id
			where d.project_key = 'ROLL' and d.from_id like 'bulk-%' and d.node_kind is not null`, nil},
		{"the issues' components", `
			insert into issue_components (issue_key, mode)
			select i.key, 'explicit' from issues i where i.project_key = 'ROLL' and i.number > 1000`, nil},
		{"the issues' component members", `
			insert into issue_component_members (issue_key, project_key, component_id, node_kind, node_address)
			select i.key, 'ROLL', 'bulk-' || (1 + ((i.number - 1001) * 100 + k - 1) % 5000), i.node_kind, i.node_address
			from issues i cross join generate_series(1, 100) k
			where i.project_key = 'ROLL' and i.number > 1000`, nil},
		{"the issue component members' affects", `
			insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
			select 'issue', m.issue_key, 'affects', 'component', m.project_key || '/' || m.component_id, c.updated_at, ` + who + `, 0, true
			from issue_component_members m join issue_components c using (issue_key)
			where m.component_id like 'bulk-%' and m.node_kind is not null`, nil},
		{"the statistics", `analyze`, nil},
	} {
		if _, err := database.Pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("write %s at scale: %v", statement.what, err)
		}
	}
}

// requireAnIssueReadReadsNoRelationTableWhole is c-view-arms' plan of a graph_edges read of one
// issue at scale. Over seedRollbackScale's rows it takes, under explain (analyze, buffers), the
// statement each read of one issue the references route makes sends over graph_edges - GET
// /api/v1/references?from=<issue> and ?to=<issue>, which refs.Edges answers - and finds no Seq Scan
// of any of rollbackRelationTables in either plan. enable_seqscan stays on (0f8b389c): the hashed
// `= any` form the planner may take for a probe of a projection row that does not end in
// `offset 0` reads the probed table whole by a Seq Scan, and with the setting off that scan is
// priced out and the form hidden. The issue read, ROLL-1250, is a row this build's writers leave,
// and the issues waiting on it are as many of either build's: graph_edges answers its child_of,
// its 100 waits and its 100 components out of it, and its one child and the 100 waits on it, 50 by
// the old blocked_by arm and 50 by the tuple arm, into it.
func requireAnIssueReadReadsNoRelationTableWhole(t *testing.T, database *store.Store, issueNode string) {
	t.Helper()
	ctx := context.Background()
	seedRollbackScale(t, database, issueNode, "ROLL/legion")
	const issue = "ROLL-1250"
	for _, side := range []struct {
		name string
		want map[string]int
	}{
		{"from", map[string]int{"child_of": 1, "blocked_by": 100, "affects": 100}},
		{"to", map[string]int{"child_of": 1, "blocked_by": 100}},
	} {
		rows, err := database.Pool.Query(ctx, `
			select kind, count(*)::int from graph_edges where `+side.name+`_kind = 'issue' and `+side.name+`_id = $1 group by kind
		`, issue)
		if err != nil {
			t.Fatalf("count the edges %s %s: %v", side.name, issue, err)
		}
		answered := map[string]int{}
		var kind string
		var count int
		if _, err := pgx.ForEachRow(rows, []any{&kind, &count}, func() error {
			answered[kind] = count
			return nil
		}); err != nil {
			t.Fatalf("count the edges %s %s: %v", side.name, issue, err)
		}
		if !maps.Equal(answered, side.want) {
			t.Fatalf("graph_edges answers the edges %s %s by kind as %v, want %v", side.name, issue, answered, side.want)
		}
	}

	connection, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the plans: %v", err)
	}
	defer connection.Release()
	for _, direction := range []string{"out", "in"} {
		kept := &keepingQueryer{queryer: database.Pool}
		if _, err := refs.Edges(ctx, kept, model.Actor{Kind: "user", ID: "alice"}, refs.Query{Direction: direction, Kind: graphstore.KindIssue, ID: issue}); err != nil {
			t.Fatalf("read the %s edges of %s: %v", direction, issue, err)
		}
		var reads []sentStatement
		for _, sent := range kept.sent {
			if strings.Contains(sent.sql, "from graph_edges") {
				reads = append(reads, sent)
			}
		}
		if len(reads) != 1 {
			t.Fatalf("the %s read of %s sends %d statements over graph_edges, want one", direction, issue, len(reads))
		}
		name := "the " + direction + " read of " + issue
		plan := explainRollbackRead(t, connection.Conn(), name, reads[0].sql, reads[0].args)
		text := explainDismissedScaleText(t, connection.Conn(), name, reads[0].sql, reads[0].args)
		for _, table := range rollbackRelationTables {
			if explaintest.SeqScansRelation(t, plan, table) {
				t.Errorf("%s reads %s by a Seq Scan, want each of its rows the read reaches reached through an index:\n%s", name, table, text)
			}
		}
	}
}

// TestGraphEdgesKeepsEveryRelationAcrossARollback holds the arms of dispatch://INFRA-1253's split
// of the issue relations and of a component's (c-view-arms): the arms over issues (child_of),
// issue_links (blocked_by), artifacts (attached_to), components (part_of), component_depends
// (depends_on) and issue_component_members (affects) answer the rows that name no node, and the
// store answers the rest while the projection row that names each edge carries its node. A
// database is seeded as the release before this one leaves it: beside a stamped issue link, a
// stamped parent, a stamped component's parent, a stamped component dependency and a stamped
// issue's component written through this build's routes, the same five that release wrote naming
// no node, the same five stamped that release deleted with no retraction, a stamped parent and a
// stamped component's parent that release moved in place, setting the column to a new value and
// leaving the row's node columns as they were, and a document on an issue it wrote naming no node.
// graph_edges, refs.BacklinkCounts and the outbound references read each answer every standing
// relation once and every deleted one nowhere; graph_edges names the issue link blocked_by, as a
// process of the release before reads it, in its old arm and in the tuple arm, and the references
// read answers it as needs. Each moved relation reads once, to its new value, through its old arm
// alone, and its old value nowhere (R9: the row counts as unmarked while its node's newest tuple
// naming the column's value does not assert it). After this child's backfill each relation the
// release before wrote is answered once, by the store, dated at the time its old arm dates it - a
// row's own created_at, a component's snapshot's imported_at, an issue's components' updated_at -
// and the old arms answer none of them: each moved relation reads once through the tuple arm, and
// the store holds its new value alone, the old one retracted. The cases of an artifact's citations
// and the deletion of dispatch://INFRA-1149's four arms are T8c's and T9's.
//
// A document's citations, T8c's: the release before's document, naming no node, cites the target
// by a refs row that release wrote, which the arm over refs answers once before the backfill and
// the tuple arm once after it, dated at the row's created_at, as the outbound references read of
// the document answers it once in both; and a document this build uploads cites another issue, a
// citation it states in the store, whose refs row the release before deletes with no retraction,
// which no arm answers. The target's backlink count holds the release before's citation.
//
// Last, over 50,000 issue links, 50,000 component dependencies and 50,000 issue components, half
// of them of either build, a read of one issue reads no table of an issue's or a component's
// relations by a Seq Scan (requireAnIssueReadReadsNoRelationTableWhole).
func TestGraphEdgesKeepsEveryRelationAcrossARollback(t *testing.T) {
	fake := &fakeGitHubApp{
		installations: map[string]string{"legion/roll": "read"},
		repos:         map[string]*fakeArchitectureRepo{"legion/roll": {commit: shaOne, files: rollbackComponentFiles()}},
	}
	options := architectureAppOptions(t, fake)
	options.settle = time.Hour
	handler, database, _ := newTestServer(t, options)
	ctx := context.Background()
	keys := createListedIssues(t, handler, "ROLL", 5)
	parent, target, oldWaiter, oldChild, movedTo := keys[0], keys[1], keys[2], keys[3], keys[4]
	create := func(title string, fields map[string]any) model.Issue {
		t.Helper()
		body := map[string]any{"project": "ROLL", "title": title, "force": true}
		for name, value := range fields {
			body[name] = value
		}
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", body, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create %q: status=%d body=%s", title, response.Code, response.Body.String())
		}
		return decodeBody[model.Issue](t, response)
	}
	stamped := create("Stamped: waits on the target under the parent", map[string]any{"parent": parent, "needs": []string{target}})
	deletedLink := create("Its link deleted by the release before", map[string]any{"needs": []string{target}})
	deletedParent := create("Its parent cleared by the release before", map[string]any{"parent": parent})
	moved := create("Its parent moved in place by the release before", map[string]any{"parent": parent})
	// The components, their parents and their dependencies, through this build's import, and the
	// component the stamped issue and the issue whose link is deleted each hold as their own,
	// through this build's PATCH.
	putSource(t, handler, "ROLL", "legion/roll", "main")
	if synced := syncSource(t, handler, "ROLL", "legion/roll"); synced.LastError != nil {
		t.Fatalf("sync rejected ROLL's model: %s", *synced.LastError)
	}
	for _, attach := range []struct{ issue, component string }{{stamped.Key, "daemon"}, {deletedLink.Key, "envoy"}} {
		if response := patchComponents(t, handler, attach.issue, map[string]any{"mode": "explicit", "ids": []string{attach.component}}); response.Code != http.StatusOK {
			t.Fatalf("attach %s to %s: status=%d body=%s", attach.issue, attach.component, response.Code, response.Body.String())
		}
	}

	// What the release before writes and deletes, by SQL, as it writes with no node and deletes
	// with no retraction.
	for _, statement := range []struct {
		what, sql string
		args      []any
	}{
		{"its link, naming no node", `insert into issue_links (issue_key, kind, target_key, created_at) values ($1, 'blocked_by', $2, now() - interval '1 day')`, []any{oldWaiter, target}},
		{"its parent, on a row naming no node", `update issues set parent_key = $2, node_kind = null, node_address = null where key = $1`, []any{oldChild, parent}},
		{"its delete of a stamped link", `delete from issue_links where issue_key = $1 and target_key = $2`, []any{deletedLink.Key, target}},
		{"its clear of a stamped parent", `update issues set parent_key = null where key = $1`, []any{deletedParent.Key}},
		{"its move of a stamped parent, in place", `update issues set parent_key = $2 where key = $1`, []any{moved.Key, movedTo}},
		{"its component, naming no node", `
			insert into components (project_key, id, title, prose, parent, snapshot_id, source_id)
			select project_key, 'relay', 'Relay', 'The relay.', 'legion', snapshot_id, source_id
			from components where project_key = 'ROLL' and id = 'legion'`, nil},
		{"its component dependency, naming no node", `insert into component_depends (project_key, from_id, to_id) values ('ROLL', 'daemon', 'listener')`, nil},
		{"its delete of a stamped component", `delete from components where project_key = 'ROLL' and id = 'workspace'`, nil},
		{"its delete of a stamped component dependency", `delete from component_depends where project_key = 'ROLL' and from_id = 'server' and to_id = 'listener'`, nil},
		{"its move of a stamped component's parent, in place", `update components set parent = 'daemon' where project_key = 'ROLL' and id = 'listener'`, nil},
		{"its issue's components, naming no node", `insert into issue_components (issue_key, mode) values ($1, 'explicit')`, []any{oldWaiter}},
		{"its issue's component, naming no node", `insert into issue_component_members (issue_key, project_key, component_id) values ($1, 'ROLL', 'envoy')`, []any{oldWaiter}},
		{"its delete of a stamped issue's component", `delete from issue_component_members where issue_key = $1`, []any{deletedLink.Key}},
	} {
		if _, err := database.Pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("write %s as the release before: %v", statement.what, err)
		}
	}
	var document string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by, created_at)
		values ($1, 'ROLL', 'old-notes-md', 'old-notes.md', 'doc', '{"kind":"user","id":"alice"}', now() - interval '1 day')
		returning id::text
	`, stamped.Key).Scan(&document); err != nil {
		t.Fatalf("write a document as the release before: %v", err)
	}
	// The release before's document cites the target, through the refs row that release writes; a
	// document this build uploads cites oldChild and states the citation in the store, and the
	// release before deletes that citation's refs row with no retraction.
	if _, err := database.Pool.Exec(ctx, `
		insert into refs (from_kind, from_id, to_kind, to_id, created_at) values ('artifact', $1, 'issue', $2, now() - interval '1 day')
	`, document, target); err != nil {
		t.Fatalf("write the release before's document's citation of %s: %v", target, err)
	}
	cited := uploadDocument(t, handler, "/api/v1/issues/"+deletedLink.Key, "cited.md", "Cites "+oldChild+".\n")
	if held, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindArtifact, cited.Artifact.RefKey, []string{graphstore.TupleMentions}); err != nil ||
		len(held) != 1 || held[0].ToID != oldChild {
		t.Fatalf("the uploaded document states the mentions %v (%v), want its one citation of %s", held, err, oldChild)
	}
	if tag, err := database.Pool.Exec(ctx, `
		delete from refs where from_kind = 'artifact' and from_id = $1 and to_kind = 'issue' and to_id = $2
	`, cited.Artifact.ID, oldChild); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("delete the uploaded document's citation as the release before: %d rows, %v", tag.RowsAffected(), err)
	}
	documentAddress := stamped.Key + "/artifact/old-notes-md"
	// The stamped issue is born converted, so beside its spec it is made with its tree and its
	// decisions query, each attached to it (part_of its issue), and the release before's document
	// is a fourth artifact attached to it. Its tree and its decisions query are no backlinks of it
	// (refs' issueOwnArtifact), so its count of attachments is its spec and that document.
	var issueArtifacts int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from artifacts where issue_key = $1 and kind in ('doc', 'tree')
	`, stamped.Key).Scan(&issueArtifacts); err != nil || issueArtifacts != 4 {
		t.Fatalf("the artifacts of %s = %d (%v), want its spec, its tree, its decisions query and the release before's document",
			stamped.Key, issueArtifacts, err)
	}

	standing := []rollbackEdge{
		{"issue", stamped.Key, "blocked_by", "issue", target},
		{"issue", oldWaiter, "blocked_by", "issue", target},
		{"issue", stamped.Key, "child_of", "issue", parent},
		{"issue", oldChild, "child_of", "issue", parent},
		{"artifact", document, "attached_to", "issue", stamped.Key},
		{"issue", moved.Key, "child_of", "issue", movedTo},
		{"component", "ROLL/envoy", "part_of", "component", "ROLL/legion"},
		{"component", "ROLL/relay", "part_of", "component", "ROLL/legion"},
		{"component", "ROLL/listener", "part_of", "component", "ROLL/daemon"},
		{"component", "ROLL/daemon", "depends_on", "component", "ROLL/envoy"},
		{"component", "ROLL/daemon", "depends_on", "component", "ROLL/listener"},
		{"issue", stamped.Key, "affects", "component", "ROLL/daemon"},
		{"issue", oldWaiter, "affects", "component", "ROLL/envoy"},
		{"artifact", document, "mentions", "issue", target},
	}
	deleted := []rollbackEdge{
		{"issue", deletedLink.Key, "blocked_by", "issue", target},
		{"issue", deletedParent.Key, "child_of", "issue", parent},
		{"issue", moved.Key, "child_of", "issue", parent},
		{"component", "ROLL/workspace", "part_of", "component", "ROLL/daemon"},
		{"component", "ROLL/server", "depends_on", "component", "ROLL/listener"},
		{"component", "ROLL/listener", "part_of", "component", "ROLL/envoy"},
		{"issue", deletedLink.Key, "affects", "component", "ROLL/envoy"},
		{"artifact", cited.Artifact.ID, "mentions", "issue", oldChild},
	}
	tupleArm := graphEdgesArm(t, database, "the arm over the store's tuples", "edge.from_kind", "graph_tuples tuple")
	refsArm := graphEdgesArm(t, database, "the arm over refs", "\n   FROM refs r")
	citation, deletedCitation := standing[13], deleted[7]
	// throughRefs holds the release before's citation to one arm: the arm over refs before the
	// backfill and the tuple arm after it, never both; and the citation deleted with no retraction
	// to neither.
	throughRefs := func(stage string, viaRefs bool) {
		t.Helper()
		name, arm, otherName, other := "arm over refs", refsArm, "tuple arm", tupleArm
		if !viaRefs {
			name, arm, otherName, other = otherName, other, name, arm
		}
		if rows := armAnswers(t, database, arm, citation); rows != 1 {
			t.Errorf("%s the %s answers %v %d times, want once", stage, name, citation, rows)
		}
		if rows := armAnswers(t, database, other, citation); rows != 0 {
			t.Errorf("%s the %s answers %v %d times, want none: the %s answers it", stage, otherName, citation, rows, name)
		}
		for _, each := range []string{refsArm, tupleArm} {
			if rows := armAnswers(t, database, each, deletedCitation); rows != 0 {
				t.Errorf("%s an arm of graph_edges answers %v, whose refs row the release before deleted, %d times, want none", stage, deletedCitation, rows)
			}
		}
	}
	movedRelations := []movedInPlace{{
		what: moved.Key + "'s parent", now: standing[5], left: deleted[2],
		oldArmName: "old child_of arm", oldArm: graphEdgesArm(t, database, "the child_of arm over issues", "'child_of'::text AS kind", "\n   FROM issues"),
	}, {
		what: "component ROLL/listener's parent", now: standing[8], left: deleted[5],
		oldArmName: "old part_of arm over components", oldArm: graphEdgesArm(t, database, "the part_of arm over components", "'part_of'::text AS kind", "\n   FROM components c"),
	}}
	// throughArm holds each moved relation to one arm: its new value is answered once by the old
	// arm before the backfill and by the tuple arm after it, never by the other, and its old value
	// by neither.
	throughArm := func(stage string, viaOld bool) {
		t.Helper()
		for _, moved := range movedRelations {
			name, arm, otherName, other := moved.oldArmName, moved.oldArm, "tuple arm", tupleArm
			if !viaOld {
				name, arm, otherName, other = otherName, other, name, arm
			}
			if rows := armAnswers(t, database, arm, moved.now); rows != 1 {
				t.Errorf("%s the %s answers %v %d times, want once: %s moved to %s %s in place",
					stage, name, moved.now, rows, moved.what, moved.now.toKind, moved.now.toID)
			}
			if rows := armAnswers(t, database, other, moved.now); rows != 0 {
				t.Errorf("%s the %s answers %v %d times, want none: the %s answers it", stage, otherName, moved.now, rows, name)
			}
			for _, each := range []string{arm, other} {
				if rows := armAnswers(t, database, each, moved.left); rows != 0 {
					t.Errorf("%s an arm of graph_edges answers %v, which %s left, %d times, want none", stage, moved.left, moved.what, rows)
				}
			}
		}
	}
	read := func(stage string) {
		t.Helper()
		for _, edge := range standing {
			if rows, _ := viewAnswers(t, database, edge); rows != 1 {
				t.Errorf("%s graph_edges answers %v %d times, want once", stage, edge, rows)
			}
		}
		for _, edge := range deleted {
			if rows, _ := viewAnswers(t, database, edge); rows != 0 {
				t.Errorf("%s graph_edges answers %v, which the release before deleted, %d times, want none", stage, edge, rows)
			}
		}
		alice := model.Actor{Kind: "user", ID: "alice"}
		counts, err := refs.BacklinkCounts(ctx, database.Pool, alice, "issue", []string{target, parent, movedTo}, []string{"attached_to"})
		if err != nil {
			t.Fatalf("%s count the backlinks: %v", stage, err)
		}
		// The target is waited on by the stamped issue and the release before's waiter, and cited by
		// the release before's document; the parent holds the stamped child and the release before's
		// child, and not the child moved away from it, which the new parent holds alone. Each holds
		// its own spec too, which the attached_to count reads below.
		if counts[target] != 3 || counts[parent] != 2 || counts[movedTo] != 1 {
			t.Errorf("%s the backlink counts are %v, want 3 at %s, 2 at %s and 1 at %s", stage, counts, target, parent, movedTo)
		}
		attached, err := refs.BacklinkCounts(ctx, database.Pool, alice, "issue", []string{stamped.Key}, nil)
		if err != nil {
			t.Fatalf("%s count the attachments: %v", stage, err)
		}
		if attached[stamped.Key] != 2 {
			t.Errorf("%s %s counts %d backlinks, want its spec and the release before's document, and neither its tree nor its decisions query",
				stage, stamped.Key, attached[stamped.Key])
		}
		// legion holds daemon and envoy, stamped, and the release before's relay; daemon holds
		// listener, moved in place to it, and not workspace, deleted, and the stamped issue's
		// affects; envoy holds server and not listener, moved away from it, daemon's stamped
		// dependency and the release before's issue's affects, and not the deleted one; listener is
		// the release before's dependency of daemon's, and not server's deleted one.
		components, err := refs.BacklinkCounts(ctx, database.Pool, alice, "component",
			[]string{"ROLL/legion", "ROLL/daemon", "ROLL/envoy", "ROLL/listener"}, nil)
		if err != nil {
			t.Fatalf("%s count the components' backlinks: %v", stage, err)
		}
		if want := map[string]int{"ROLL/legion": 3, "ROLL/daemon": 2, "ROLL/envoy": 3, "ROLL/listener": 1}; !maps.Equal(components, want) {
			t.Errorf("%s the components' backlink counts are %v, want %v", stage, components, want)
		}
		for _, outbound := range []struct {
			from, kind, targetKind, target string
			want                           int
		}{
			{stamped.Key, model.EdgeKindNeeds, "issue", target, 1},
			{oldWaiter, model.EdgeKindNeeds, "issue", target, 1},
			{deletedLink.Key, model.EdgeKindNeeds, "issue", target, 0},
			{stamped.Key, "child_of", "issue", parent, 1},
			{oldChild, "child_of", "issue", parent, 1},
			{deletedParent.Key, "child_of", "issue", parent, 0},
			{moved.Key, "child_of", "issue", movedTo, 1},
			{moved.Key, "child_of", "issue", parent, 0},
			{"ROLL/component/envoy", "part_of", "component", "ROLL/legion", 1},
			{"ROLL/component/relay", "part_of", "component", "ROLL/legion", 1},
			{"ROLL/component/listener", "part_of", "component", "ROLL/daemon", 1},
			{"ROLL/component/listener", "part_of", "component", "ROLL/envoy", 0},
			{"ROLL/component/daemon", "depends_on", "component", "ROLL/envoy", 1},
			{"ROLL/component/daemon", "depends_on", "component", "ROLL/listener", 1},
			{"ROLL/component/server", "depends_on", "component", "ROLL/listener", 0},
			{stamped.Key, "affects", "component", "ROLL/daemon", 1},
			{oldWaiter, "affects", "component", "ROLL/envoy", 1},
			{deletedLink.Key, "affects", "component", "ROLL/envoy", 0},
			{documentAddress, "mentions", "issue", target, 1},
		} {
			if got := outboundAnswers(t, handler, outbound.from, outbound.kind, outbound.targetKind, outbound.target); got != outbound.want {
				t.Errorf("%s GET /api/v1/references?from=%s answers %s to %s %d times, want %d", stage, outbound.from, outbound.kind, outbound.target, got, outbound.want)
			}
		}
		if got := outboundAnswers(t, handler, stamped.Key, "blocked_by", "issue", target); got != 0 {
			t.Errorf("%s the references read answers the wait as blocked_by %d times, want it answered as needs alone", stage, got)
		}
	}
	read("before the backfill:")
	throughArm("before the backfill:", true)
	throughRefs("before the backfill:", true)
	// The deleted citation's tuple stands with no row: the census of a document's references names
	// it as a tuple no row projects until the backfill retracts it.
	referenceDifferences := func(stage string) (rows, tuples int64) {
		t.Helper()
		report, err := graphcensus.StoreCensus(ctx, database.Pool.Config().ConnString(), false, docs.ReturnDeclined)
		if err != nil {
			t.Fatalf("%s take the store census: %v", stage, err)
		}
		for _, kind := range report.Kinds {
			if kind.Kind == "artifact_reference" {
				return kind.RowDifferences, kind.NodeDifferences
			}
		}
		t.Fatalf("%s the census reports no kind of a document's references", stage)
		return 0, 0
	}
	if rows, tuples := referenceDifferences("before the backfill:"); rows != 0 || tuples != 1 {
		t.Errorf("before the backfill the census reads %d row and %d tuple differences of a document's references, want the deleted citation's tuple alone",
			rows, tuples)
	}

	if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	read("after the backfill:")
	throughArm("after the backfill:", false)
	throughRefs("after the backfill:", false)
	if rows, tuples := referenceDifferences("after the backfill:"); rows != 0 || tuples != 0 {
		t.Errorf("after the backfill the census reads %d row and %d tuple differences of a document's references, want none: the backfill retracts the deleted citation's tuple",
			rows, tuples)
	}
	if held, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindArtifact, cited.Artifact.RefKey, []string{graphstore.TupleMentions}); err != nil || len(held) != 0 {
		t.Errorf("after the backfill the uploaded document states the mentions %v (%v), want none: its row is gone", held, err)
	}
	var movedCreated, listenerImported time.Time
	if err := database.Pool.QueryRow(ctx, `select created_at from issues where key = $1`, moved.Key).Scan(&movedCreated); err != nil {
		t.Fatalf("read %s's row: %v", moved.Key, err)
	}
	if held := currentRelations(t, database, moved.Key, graphstore.TupleChildOf); len(held) != 1 || held[0].ToID != movedTo || !held[0].At.Equal(movedCreated) {
		t.Errorf("after the backfill %s states child_of %v, want the one parent its row names, %s, dated at its row's created_at %s",
			moved.Key, held, movedTo, movedCreated)
	}
	const componentImported = `select s.imported_at from components c join architecture_snapshots s on s.id = c.snapshot_id
		where c.project_key = 'ROLL' and c.id = $1`
	if err := database.Pool.QueryRow(ctx, componentImported, "listener").Scan(&listenerImported); err != nil {
		t.Fatalf("read listener's snapshot: %v", err)
	}
	held, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindComponent, "ROLL/listener", []string{graphstore.TuplePartOf})
	if err != nil {
		t.Fatalf("read listener's part_of: %v", err)
	}
	if len(held) != 1 || held[0].ToID != "ROLL/daemon" || !held[0].At.Equal(listenerImported) {
		t.Errorf("after the backfill ROLL/listener states part_of %v, want the one parent its row names, ROLL/daemon, dated at its snapshot's imported_at %s",
			held, listenerImported)
	}
	for _, written := range []struct {
		edge              rollbackEdge
		createdAt, oldArm string
		args              []any
	}{
		{standing[1], `select created_at from issue_links where issue_key = $1 and target_key = $2`,
			`select count(*) from issue_links where issue_key = $1 and target_key = $2 and node_kind is null`, []any{oldWaiter, target}},
		{standing[3], `select created_at from issues where key = $1`,
			`select count(*) from issues where key = $1 and node_kind is null`, []any{oldChild}},
		{standing[4], `select created_at from artifacts where id = $1::uuid`,
			`select count(*) from artifacts where id = $1::uuid and node_kind is null`, []any{document}},
		{standing[7], componentImported,
			`select count(*) from components where project_key = 'ROLL' and id = $1 and node_kind is null`, []any{"relay"}},
		{standing[10], `select s.imported_at from component_depends d
				join components c on c.project_key = d.project_key and c.id = d.from_id
				join architecture_snapshots s on s.id = c.snapshot_id
				where d.project_key = 'ROLL' and d.from_id = $1 and d.to_id = $2`,
			`select count(*) from component_depends where project_key = 'ROLL' and from_id = $1 and to_id = $2 and node_kind is null`,
			[]any{"daemon", "listener"}},
		{standing[12], `select c.updated_at from issue_component_members m join issue_components c using (issue_key)
				where m.issue_key = $1 and m.component_id = $2`,
			`select count(*) from issue_component_members where issue_key = $1 and component_id = $2 and node_kind is null`,
			[]any{oldWaiter, "envoy"}},
		{citation, `select created_at from refs where from_kind = 'artifact' and from_id = $1 and to_kind = 'issue' and to_id = $2`,
			`select count(*) from refs r join artifacts a on a.id::text = r.from_id
				where r.from_kind = 'artifact' and r.from_id = $1 and r.to_kind = 'issue' and r.to_id = $2 and a.node_kind is null`,
			[]any{document, target}},
	} {
		var createdAt time.Time
		if err := database.Pool.QueryRow(ctx, written.createdAt, written.args...).Scan(&createdAt); err != nil {
			t.Fatalf("read the row behind %v: %v", written.edge, err)
		}
		if _, at := viewAnswers(t, database, written.edge); !at.Equal(createdAt) {
			t.Errorf("after the backfill graph_edges dates %v at %s, want the time its old arm dates it, %s", written.edge, at, createdAt)
		}
		var unmarked int
		if err := database.Pool.QueryRow(ctx, written.oldArm, written.args...).Scan(&unmarked); err != nil {
			t.Fatalf("read the marker behind %v: %v", written.edge, err)
		}
		if unmarked != 0 {
			t.Errorf("after the backfill the row behind %v names no node, so its old arm still answers it", written.edge)
		}
	}

	// The plan of a read of one issue over 50,000 issue links, 50,000 component dependencies and
	// 50,000 issue components, of either build (c-view-arms).
	t.Run("a read of one issue at scale scans no relation table", func(t *testing.T) {
		requireAnIssueReadReadsNoRelationTableWhole(t, database, stamped.Key)
	})
}
