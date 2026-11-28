package graphcensus

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
)

// onePass is a backfillRun of one pass of the backfill alone, so a hold the batch under test does
// not take is never taken by another pass.
func onePass(write func(context.Context, *store.Store) (KindBackfill, error)) backfillRun {
	return func(ctx context.Context, database *store.Store) (BackfillReport, error) {
		done, err := write(ctx, database)
		return BackfillReport{Kinds: []KindBackfill{done}}, err
	}
}

// seedLockSource writes project LOCK, its one architecture source and a snapshot of it imported
// two days ago, and answers the source's id, the snapshot's id and its imported_at.
func seedLockSource(t *testing.T, database *store.Store) (source, snapshot int64, importedAt time.Time) {
	t.Helper()
	ctx := context.Background()
	seedLockProject(t, database)
	if err := database.Pool.QueryRow(ctx, `
		insert into architecture_sources (project_key, repo, branch, installation_id, created_by)
		values ('LOCK', 'lock/arch', 'main', 1, '{"kind":"user","id":"alice"}') returning id
	`).Scan(&source); err != nil {
		t.Fatalf("seed the architecture source: %v", err)
	}
	if err := database.Pool.QueryRow(ctx, `
		insert into architecture_snapshots (project_key, commit, imported_at, files)
		values ('LOCK', 'one', now() - interval '2 days', '{}') returning id, imported_at
	`).Scan(&snapshot, &importedAt); err != nil {
		t.Fatalf("seed the snapshot: %v", err)
	}
	return source, snapshot, importedAt
}

// holdLockSource holds project LOCK's architecture source row, the owner row every component batch
// takes first, at the statement lockComponentSources takes it with and before any node or tuple,
// and project LOCK's row, which no component batch takes.
func holdLockSource(t *testing.T, database *store.Store) (owner, project *lockstep.Hold) {
	t.Helper()
	owner = lockstep.HoldRow(t, database, &lockstep.Hold{
		Name: "source lock/arch's row", Step: lockstep.StepOwner, Taken: true, Kind: "architecture_source", ID: "LOCK",
		Statement: "select 1 from architecture_sources where project_key = any($1::text[]) order by project_key, id for update",
		Unlocked:  []string{"graph_nodes", "graph_tuples"},
	}, `select 1 from architecture_sources where project_key = 'LOCK' for update`)
	project = lockstep.HoldRow(t, database, &lockstep.Hold{Name: "project LOCK's row", Step: lockstep.StepEvent},
		`select 1 from projects where key = $1 for update`, "LOCK")
	return owner, project
}

// currentComponentRelations is every current part_of and depends_on out of a component node of
// project LOCK and affects out of an issue, as "<from> <kind> <to> <at>", sorted.
func currentComponentRelations(t *testing.T, database *store.Store) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select held.from_id || ' ' || held.kind || ' ' || held.to_id, held.at from (
			select distinct on (t.from_kind, t.from_id, t.kind, t.to_kind, t.to_id) t.from_id, t.kind, t.to_id, t.at, t.asserted
			from graph_tuples t
			where t.to_kind = 'component' and t.to_id like 'LOCK/%' and t.kind in ('part_of', 'depends_on', 'affects')
			order by t.from_kind, t.from_id, t.kind, t.to_kind, t.to_id, t.seq desc
		) held where held.asserted`)
	if err != nil {
		t.Fatalf("read the components' relations: %v", err)
	}
	var relations []string
	var relation string
	var at time.Time
	if _, err := pgx.ForEachRow(rows, []any{&relation, &at}, func() error {
		relations = append(relations, relation+" "+at.UTC().Format(time.RFC3339Nano))
		return nil
	}); err != nil {
		t.Fatalf("read the components' relations: %v", err)
	}
	slices.Sort(relations)
	return relations
}

// TestTheComponentBatchesTakeTheStoreLockOrder holds the three batches of the components' backfill
// (R9 of the issue-tree spec) to the store's lock order, as
// TestTheIssueLinkBatchTakesTheStoreLockOrder holds writeIssueLinkBatch: the owner row first - the
// architecture source's row for a component and its dependency, the issue's row for an issue's
// component - then the node rows where the batch writes any, then the rows that name the node; the
// tuples are stated at the sentinel, which no repeat key holds, so no hold stands on them. After
// each run the rows name their node and the store states what each row projects, dated as
// graph_edges' old arm dates the edge.
//
// The component batch reads each row's parent under the source's row: beside two components the
// release before wrote, naming no node, envoy's row names its node, whose part_of names legion,
// while the row names daemon, a parent moved in place. The batch reads it as unmarked, and its
// node then states daemon alone, the part_of to legion retracted.
func TestTheComponentBatchesTakeTheStoreLockOrder(t *testing.T) {
	ctx := context.Background()
	alice := model.Actor{Kind: "user", ID: "alice"}
	stamp := func(at time.Time) string { return at.UTC().Format(time.RFC3339Nano) }

	t.Run("writeComponentBatch", func(t *testing.T) {
		database := storetest.Open(t)
		source, snapshot, importedAt := seedLockSource(t, database)
		drivenTx(t, database, "seed the components", func(tx pgx.Tx) error {
			if err := graphstore.Write(ctx, tx, []graphstore.Node{graphstore.ComponentNode("LOCK", "envoy", alice, importedAt)},
				[]graphstore.Tuple{graphstore.ComponentRelation(graphstore.KindComponent, "LOCK/envoy", graphstore.TuplePartOf, "LOCK/legion", true, alice, importedAt)}); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `
				insert into components (project_key, id, title, prose, parent, snapshot_id, source_id, node_kind, node_address) values
					('LOCK', 'legion', 'Legion', 'The swarm.', null, $1, $2, null, null),
					('LOCK', 'daemon', 'Daemon', 'The daemon.', 'legion', $1, $2, null, null),
					('LOCK', 'envoy', 'Envoy', 'Routing.', 'daemon', $1, $2, 'component', 'LOCK/envoy')
			`, snapshot, source)
			return err
		})

		owner, project := holdLockSource(t, database)
		node := &lockstep.Hold{
			Name: "component LOCK/daemon's node row", Step: lockstep.StepNode, Taken: true, Kind: graphstore.KindComponent, ID: "LOCK/daemon",
			Statement: "insert into graph_nodes (",
		}
		lockstep.Begin(t, database, node)
		if err := graphstore.Write(ctx, node.Tx, []graphstore.Node{graphstore.ComponentNode("LOCK", "daemon", alice, importedAt)}, nil); err != nil {
			t.Fatalf("hold component LOCK/daemon's node row: %v", err)
		}
		row := lockstep.HoldRow(t, database, &lockstep.Hold{
			Name: "component legion's row", Step: lockstep.StepProjection, Taken: true,
			Statement: "update components c set node_kind",
		}, `select 1 from components where project_key = 'LOCK' and id = $1 for no key update`, "legion")

		report := followTheRun(t, database, "writeComponentBatch", onePass(backfillComponents), []*lockstep.Hold{owner, project, node, row})
		for _, id := range []string{"legion", "daemon", "envoy"} {
			if kind, address := rowMarker(t, database, `select node_kind, node_address from components where project_key = 'LOCK' and id = $1`, id); kind != graphstore.KindComponent || address != "LOCK/"+id {
				t.Errorf("component %s's row names node %q/%q, want %s/LOCK/%s", id, kind, address, graphstore.KindComponent, id)
			}
		}
		if got, want := currentComponentRelations(t, database), []string{
			"LOCK/daemon part_of LOCK/legion " + stamp(importedAt), "LOCK/envoy part_of LOCK/daemon " + stamp(importedAt),
		}; !slices.Equal(got, want) {
			t.Errorf("the components state %q, want %q: each the parent its row names, dated at its snapshot's imported_at", got, want)
		}
		if done := reportedKind(t, report, "component"); done.Rows != 3 || !done.Complete {
			t.Errorf("the backfill reports %#v for components, want 3 rows and every row marked", done)
		}
	})

	t.Run("writeComponentDependsBatch", func(t *testing.T) {
		database := storetest.Open(t)
		source, snapshot, importedAt := seedLockSource(t, database)
		drivenTx(t, database, "seed the components and the dependency", func(tx pgx.Tx) error {
			if err := graphstore.Write(ctx, tx, []graphstore.Node{
				graphstore.ComponentNode("LOCK", "daemon", alice, importedAt), graphstore.ComponentNode("LOCK", "envoy", alice, importedAt),
			}, nil); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				insert into components (project_key, id, title, prose, parent, snapshot_id, source_id, node_kind, node_address) values
					('LOCK', 'daemon', 'Daemon', 'The daemon.', null, $1, $2, 'component', 'LOCK/daemon'),
					('LOCK', 'envoy', 'Envoy', 'Routing.', null, $1, $2, 'component', 'LOCK/envoy')
			`, snapshot, source); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `insert into component_depends (project_key, from_id, to_id) values ('LOCK', 'daemon', 'envoy')`)
			return err
		})

		owner, project := holdLockSource(t, database)
		row := lockstep.HoldRow(t, database, &lockstep.Hold{
			Name: "the dependency's component_depends row", Step: lockstep.StepProjection, Taken: true,
			Statement: "update component_depends d set node_kind",
		}, `select 1 from component_depends where project_key = 'LOCK' and from_id = $1 and to_id = 'envoy' for no key update`, "daemon")

		report := followTheRun(t, database, "writeComponentDependsBatch", onePass(backfillComponentDepends), []*lockstep.Hold{owner, project, row})
		if kind, address := rowMarker(t, database, `select node_kind, node_address from component_depends where project_key = 'LOCK' and from_id = $1`, "daemon"); kind != graphstore.KindComponent || address != "LOCK/daemon" {
			t.Errorf("the dependency names node %q/%q, want %s/LOCK/daemon", kind, address, graphstore.KindComponent)
		}
		if got, want := currentComponentRelations(t, database), []string{"LOCK/daemon depends_on LOCK/envoy " + stamp(importedAt)}; !slices.Equal(got, want) {
			t.Errorf("the components state %q, want %q: the dependency, dated at its component's snapshot's imported_at", got, want)
		}
		if done := reportedKind(t, report, "component_depends"); done.Rows != 1 || done.Tuples != 1 || !done.Complete {
			t.Errorf("the backfill reports %#v for component dependencies, want 1 row, 1 tuple and every row marked", done)
		}
	})

	t.Run("writeIssueComponentMemberBatch", func(t *testing.T) {
		database := storetest.Open(t)
		seedLockProject(t, database)
		var updatedAt time.Time
		drivenTx(t, database, "seed the issue and its component", func(tx pgx.Tx) error {
			if err := graphstore.Write(ctx, tx, []graphstore.Node{graphstore.IssueNode("LOCK-1", "LOCK", alice, time.Now().UTC())}, nil); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				insert into issues (key, project_key, number, title, created_by, rank, node_kind, node_address)
				values ('LOCK-1', 'LOCK', 1, 'Holds envoy', '{"kind":"user","id":"alice"}', 'U', 'issue', 'LOCK-1')
			`); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `
				insert into issue_components (issue_key, mode, updated_at) values ('LOCK-1', 'explicit', now() - interval '1 day')
				returning updated_at
			`).Scan(&updatedAt); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `insert into issue_component_members (issue_key, project_key, component_id) values ('LOCK-1', 'LOCK', 'envoy')`)
			return err
		})

		owner, project := holdLockOwner(t, database)
		row := lockstep.HoldRow(t, database, &lockstep.Hold{
			Name: "the issue's issue_component_members row", Step: lockstep.StepProjection, Taken: true,
			Statement: "update issue_component_members m set node_kind",
		}, `select 1 from issue_component_members where issue_key = $1 and component_id = 'envoy' for no key update`, "LOCK-1")

		report := followTheRun(t, database, "writeIssueComponentMemberBatch", onePass(backfillIssueComponentMembers), []*lockstep.Hold{owner, project, row})
		if kind, address := rowMarker(t, database, `select node_kind, node_address from issue_component_members where issue_key = $1`, "LOCK-1"); kind != graphstore.KindIssue || address != "LOCK-1" {
			t.Errorf("the issue's component names node %q/%q, want %s/LOCK-1", kind, address, graphstore.KindIssue)
		}
		if got, want := currentComponentRelations(t, database), []string{"LOCK-1 affects LOCK/envoy " + stamp(updatedAt)}; !slices.Equal(got, want) {
			t.Errorf("the store states %q, want %q: the issue's affects, dated at its issue_components row's updated_at", got, want)
		}
		if done := reportedKind(t, report, "issue_component_member"); done.Rows != 1 || done.Tuples != 1 || !done.Complete {
			t.Errorf("the backfill reports %#v for issues' components, want 1 row, 1 tuple and every row marked", done)
		}
	})
}

// TestTheComponentDependsPassOwesNoRowOfAComponentNoPassCanMark holds the depends pass of the
// components' backfill to the rows its writer can mark. Component daemon's own address fits the
// store's bound and its parent's does not, so the component pass leaves its row out
// (componentFits) and the row names no node after any run; daemon's dependency on envoy fits the
// bound at both ends, and writeComponentDependsBatch marks a dependency only under a component
// whose row names its node. So the depends pass neither reads nor counts that dependency, as the
// component pass neither reads nor counts daemon: after the two passes each reports every row it
// owes marked, and the dependency keeps its null node columns, which graph_edges' old depends_on
// arm answers. Counted, it stood remaining after every run, and the backfill never reported the
// store complete.
func TestTheComponentDependsPassOwesNoRowOfAComponentNoPassCanMark(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	source, snapshot, _ := seedLockSource(t, database)
	parent := strings.Repeat("p", graphstore.MaxAddressBytes)
	if graphstore.AddressFits(graphstore.ComponentAddress("LOCK", parent)) {
		t.Fatalf("the parent's address fits the store's bound, so this proves nothing")
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into components (project_key, id, title, prose, parent, snapshot_id, source_id) values
			('LOCK', $3, 'Long', 'A parent past the bound.', null, $1, $2),
			('LOCK', 'daemon', 'Daemon', 'The daemon.', $3, $1, $2),
			('LOCK', 'envoy', 'Envoy', 'Routing.', null, $1, $2)
	`, snapshot, source, parent); err != nil {
		t.Fatalf("seed the components: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `insert into component_depends (project_key, from_id, to_id) values ('LOCK', 'daemon', 'envoy')`); err != nil {
		t.Fatalf("seed daemon's dependency on envoy: %v", err)
	}

	components, err := backfillComponents(ctx, database)
	if err != nil {
		t.Fatalf("run the component pass: %v", err)
	}
	if components.Rows != 1 || components.Remaining != 0 || !components.Complete {
		t.Fatalf("the component pass reports %#v, want envoy alone marked and none remaining: daemon's parent and the parent itself are past the bound",
			components)
	}
	depends, err := backfillComponentDepends(ctx, database)
	if err != nil {
		t.Fatalf("run the depends pass: %v", err)
	}
	if depends.Rows != 0 || depends.Remaining != 0 || !depends.Complete {
		t.Errorf("the depends pass reports %#v, want no row marked and none remaining: daemon's row names no node after any run", depends)
	}
	if kind, address := rowMarker(t, database, `select node_kind, node_address from component_depends where project_key = 'LOCK' and from_id = $1`, "daemon"); kind != "" || address != "" {
		t.Errorf("daemon's dependency names node %q/%q, want none: daemon's row names none", kind, address)
	}
	var answered int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from graph_edges
		where from_kind = 'component' and from_id = 'LOCK/daemon' and kind = 'depends_on' and to_kind = 'component' and to_id = 'LOCK/envoy'
	`).Scan(&answered); err != nil {
		t.Fatalf("read daemon's dependency out of graph_edges: %v", err)
	}
	if answered != 1 {
		t.Errorf("graph_edges answers daemon's dependency on envoy %d times, want once, by its old arm", answered)
	}
}
