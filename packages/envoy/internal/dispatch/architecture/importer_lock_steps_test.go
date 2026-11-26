package architecture

import (
	"context"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
)

// TestTheImportTakesTheStoreLockOrder holds an import's write of the store (stateComponents,
// R9 of the issue-tree spec) to the store's lock order, written on api/inbox_read.go's
// markInboxReadThreads: the owner rows first - the row of every source of the project, which every
// writer of a component's node and rows takes first - then the component nodes, then the
// components rows it replaces, and the project of its event last. The tuples are stated at the
// sentinel, which no repeat key holds, so no hold stands on them. The import runs on one
// connection of its own, whose backend the test watches, and it re-imports a model it imported
// before at another commit: api's dependency on store and store itself are dropped, so the node
// rows it writes are there to hold. After it each row names its node, store's node is gone, and
// the store states api's part_of to server alone.
func TestTheImportTakesTheStoreLockOrder(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{t: t, files: validFiles()}
	importer, database := newImporterFixture(t, fake)
	if _, err := importer.Sync(ctx, "CORE", archRepo); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	fake.commit = commitTwo
	fake.files = map[string]string{
		"api.md":    "---\ntitle: HTTP API\nparent: server\npaths: [internal/api]\n---\nThe API.\n",
		"server.md": "---\ntitle: Server\n---\nThe server.\n",
	}
	single := storetest.SingleConnection(t, database)
	process := NewImporter(single, newGitHubClient(t, fake.handler()), events.NewBroker())
	t.Cleanup(process.Close)
	var pid uint32
	if err := single.Pool.QueryRow(ctx, `select pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read the import's backend: %v", err)
	}

	owner := lockstep.HoldRow(t, database, &lockstep.Hold{
		Name: "source legion/arch's row", Step: lockstep.StepOwner, Taken: true, Kind: "architecture_source", ID: "CORE",
		Statement: "select id, repo, branch, last_commit, last_error, xmin::text <> $2 from architecture_sources where project_key = $1 order by id for update",
		Unlocked:  []string{"graph_nodes", "graph_tuples", "component_depends", "component_facts", "events"},
	}, `select 1 from architecture_sources where project_key = 'CORE' for update`)
	node := lockstep.HoldRow(t, database, &lockstep.Hold{
		Name: "component CORE/api's node row", Step: lockstep.StepNode, Taken: true, Kind: graphstore.KindComponent, ID: "CORE/api",
		Statement: "insert into graph_nodes (",
		Unlocked:  []string{"component_depends", "component_facts", "events"},
	}, `select 1 from graph_nodes where kind = 'component' and id = $1 for update`, "CORE/api")
	row := lockstep.HoldRow(t, database, &lockstep.Hold{
		Name: "component server's row", Step: lockstep.StepProjection, Taken: true,
		Statement: "delete from components where project_key = $1 and source_id = $2",
		Unlocked:  []string{"component_facts", "events"},
	}, `select 1 from components where project_key = 'CORE' and id = $1 for no key update`, "server")
	// The project's row `for no key update`, which the event's owner lock takes and the foreign
	// keys of the snapshot and the components rows, which take it `for key share`, do not wait on.
	project := lockstep.HoldRow(t, database, &lockstep.Hold{
		Name: "project CORE's row", Step: lockstep.StepEvent, Taken: true,
		Statement: "select key, pg_current_xact_id() from projects where key = $1 for no key update",
	}, `select 1 from projects where key = $1 for no key update`, "CORE")

	done := make(chan error, 1)
	go func() {
		_, err := process.Sync(context.Background(), "CORE", archRepo)
		done <- err
	}()
	lockstep.Follow(t, database, "stateComponents", pid, done, []*lockstep.Hold{owner, node, row, project})

	rows, err := database.Pool.Query(ctx, `
		select id || ' ' || coalesce(node_kind, '-') || ' ' || coalesce(node_address, '-') from components where project_key = 'CORE'
		union all
		select n.id || ' ' || n.state from graph_nodes n where n.kind = 'component'
		union all
		select t.from_id || ' ' || t.kind || ' ' || t.to_id from (
			select distinct on (t.from_id, t.kind, t.to_id) t.from_id, t.kind, t.to_id, t.asserted
			from graph_tuples t where t.from_kind = 'component' and t.to_kind = 'component'
			order by t.from_id, t.kind, t.to_id, t.seq desc
		) t where t.asserted`)
	if err != nil {
		t.Fatalf("read the components and their store: %v", err)
	}
	var got []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("read the components and their store: %v", err)
		}
		got = append(got, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the components and their store: %v", err)
	}
	slices.Sort(got)
	if want := []string{
		"CORE/api live", "CORE/api part_of CORE/server", "CORE/server live", "CORE/store gone",
		"api component CORE/api", "server component CORE/server",
	}; !slices.Equal(got, want) {
		t.Errorf("after the import the components and their store are %q, want %q", got, want)
	}
}
