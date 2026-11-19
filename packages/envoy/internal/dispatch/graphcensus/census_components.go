package graphcensus

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// componentProjections are the census's kinds of the architecture components' relations
// (projections), each held triple by triple (projection.tuples) against the tuples the store
// states now (dispatch://INFRA-1253 R9): components.parent against each component's part_of,
// each component_depends row against the depends_on of its pair, and each issue_component_members
// row against the affects of its issue and its component. A row that names no node is a row a
// process of the release before wrote, which graph_edges' old arm answers until the backfill names
// its node (componentBackfills), and which the census counts as a class of its own. A deployment
// with no architecture source, or no component with a parent, a dependency or an issue holding it,
// holds no row, no tuple and no unmarked row of a kind, which is no refusal (projection.emptyAgrees),
// as it is none for an issue's parent or its waits.
var componentProjections = []projection{{
	name:     "component_parent",
	nodeKind: graphstore.KindComponent,
	table:    "components.parent",
	note: "a component's parent: the row side is every marked components row with a parent as the triple of its node," +
		" part_of and its parent component, against every part_of to a component a present component node states now",
	rows: `
		select 'component ' || c.project_key || '/' || c.id, c.node_kind, c.node_address,
		       '` + graphstore.TuplePartOf + `', '` + graphstore.KindComponent + `', c.project_key || '/' || c.parent
		from components c
		where c.node_kind is not null and c.parent is not null`,
	tuples: heldTuples(`t.kind = '`+graphstore.TuplePartOf+`' and t.to_kind = '`+graphstore.KindComponent+`'`, graphstore.KindComponent),
	unmarked: `
		select count(*) from components c where c.node_kind is null and c.parent is not null`,
	emptyAgrees: true,
}, {
	name:     "component_depends",
	nodeKind: graphstore.KindComponent,
	table:    "component_depends",
	note: "a component's dependencies: the row side is every marked component_depends row as the triple of its" +
		" component's node, depends_on and the component it depends on, against every depends_on a present component node states now",
	rows: `
		select 'component_depends ' || d.project_key || '/' || d.from_id || ' ' || d.to_id, d.node_kind, d.node_address,
		       '` + graphstore.TupleDependsOn + `', '` + graphstore.KindComponent + `', d.project_key || '/' || d.to_id
		from component_depends d
		where d.node_kind is not null`,
	tuples: heldTuples(`t.kind = '`+graphstore.TupleDependsOn+`' and t.to_kind = '`+graphstore.KindComponent+`'`, graphstore.KindComponent),
	unmarked: `
		select count(*) from component_depends d where d.node_kind is null`,
	emptyAgrees: true,
}, {
	name:     "issue_component_member",
	nodeKind: graphstore.KindIssue,
	table:    "issue_component_members",
	note: "an issue's own components: the row side is every marked issue_component_members row as the triple of its" +
		" issue's node, affects and the component, against every affects to a component a present issue node states now",
	rows: `
		select 'issue_component_member ' || m.issue_key || ' ' || m.project_key || '/' || m.component_id, m.node_kind, m.node_address,
		       '` + graphstore.TupleAffects + `', '` + graphstore.KindComponent + `', m.project_key || '/' || m.component_id
		from issue_component_members m
		where m.node_kind is not null`,
	tuples: heldTuples(`t.kind = '`+graphstore.TupleAffects+`' and t.to_kind = '`+graphstore.KindComponent+`'`, graphstore.KindIssue),
	unmarked: `
		select count(*) from issue_component_members m where m.node_kind is null`,
	emptyAgrees: true,
}}

// componentBackfills are the backfill's passes over the architecture components (backfillPasses),
// after the project pass, whose object pass gave every issue row its node: the components a
// process of the release before wrote, each its node and its part_of (backfillComponents), then
// their dependencies (backfillComponentDepends), which hang from those nodes, and the components
// of an issue (backfillIssueComponentMembers).
var componentBackfills = []backfillPass{
	oneKind(backfillComponents), oneKind(backfillComponentDepends), oneKind(backfillIssueComponentMembers),
}

// componentRowUnmarked is the test that the components row aliased c is one its marker does not
// mark: it names no node, or it names a parent its node's part_of does not name
// (store.ComponentParentMoved, R9 of dispatch://INFRA-1253), which graph_edges' old part_of arm
// answers. A row past the store's bound, its own address or its parent's, is no node of the store
// and is left out (componentFits).
const componentRowUnmarked = `(c.node_kind is null or (c.parent is not null and ` + store.ComponentParentMoved + `))`

// componentFits is the test that the components row aliased c addresses a node of the store: its
// own address and its parent's are inside the store's bound.
var componentFits = `octet_length(c.project_key || '/' || c.id) <= ` + addressBound +
	` and (c.parent is null or octet_length(c.project_key || '/' || c.parent) <= ` + addressBound + `)`

// componentRow is one components row the backfill reads: its project and id, its parent, and the
// imported_at of its snapshot, which graph_edges' old arm dates its edges at.
type componentRow struct {
	project, id string
	parent      *string
	importedAt  time.Time
}

// backfillComponents states, for every components row its marker does not mark, the component's
// node and its part_of to the parent its row names, dated at its snapshot's imported_at, as
// graph_edges' old arm dates the edge, and names the node on the row in the transaction that
// states them, so a second run reads no row the first one wrote (dispatch://INFRA-1149's
// r-view-end contract). It walks the rows in their primary key's order, one batch a transaction
// (writeComponentBatch).
func backfillComponents(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: "component"}
	var afterProject, afterID string
	for {
		rows, err := database.Pool.Query(ctx, `
			select c.project_key, c.id, c.parent, s.imported_at
			from components c join architecture_snapshots s on s.id = c.snapshot_id
			where `+componentRowUnmarked+` and `+componentFits+` and (c.project_key, c.id) > ($1, $2)
			order by c.project_key, c.id
			limit $3
		`, afterProject, afterID, backfillBatch)
		if err != nil {
			return done, fmt.Errorf("read the components their marker does not mark: %w", err)
		}
		batch, err := pgx.CollectRows(rows, scanComponentRow)
		if err != nil {
			return done, fmt.Errorf("read the components their marker does not mark: %w", err)
		}
		if len(batch) == 0 {
			break
		}
		afterProject, afterID = batch[len(batch)-1].project, batch[len(batch)-1].id
		written, stated, err := writeComponentBatch(ctx, database, batch)
		if err != nil {
			return done, err
		}
		done.Rows += written
		done.Nodes += written
		done.Tuples += stated
		if len(batch) < backfillBatch {
			break
		}
	}
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from components c where `+componentRowUnmarked+` and `+componentFits,
	).Scan(&done.Remaining); err != nil {
		return done, fmt.Errorf("count the components their marker still does not mark: %w", err)
	}
	done.Complete = done.Remaining == 0
	return done, nil
}

func scanComponentRow(row pgx.CollectableRow) (componentRow, error) {
	var component componentRow
	err := row.Scan(&component.project, &component.id, &component.parent, &component.importedAt)
	return component, err
}

// writeComponentBatch states one batch's components and names each one's node on its row, in one
// transaction and in the store's lock order: the source rows of every project of the batch first,
// in ascending (project, id), which every writer of a component's rows and node takes first (the
// importer's lockProjectSources, a source's removal), then the batch's rows read again under them,
// each with the parent it names now, then the nodes and their part_of in one graphstore.Write, then
// the rows that name the nodes. The batch was read outside this transaction, so a component an
// import replaced or dropped meanwhile, or whose parent moved, is read as it stands now: a row no
// longer there, or one an import of this release named, states nothing, and the part_of stated is
// the parent the row names under the lock, against the part_of the node holds now
// (graphstore.SetChanges), so a parent moved in place retracts the one the node held. It answers
// how many rows it named and how many tuples it stated: the base attributes of each node and each
// part_of that moved.
func writeComponentBatch(ctx context.Context, database *store.Store, batch []componentRow) (int64, int64, error) {
	projects := make([]string, len(batch))
	ids := make([]string, len(batch))
	for index, component := range batch {
		projects[index], ids[index] = component.project, component.id
	}
	var written, stated int64
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if err := lockComponentSources(ctx, tx, projects); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			select c.project_key, c.id, c.parent, s.imported_at
			from components c
			join unnest($1::text[], $2::text[]) as batch (project_key, id) on batch.project_key = c.project_key and batch.id = c.id
			join architecture_snapshots s on s.id = c.snapshot_id
			where `+componentRowUnmarked+` and `+componentFits+`
			order by c.project_key, c.id
		`, projects, ids)
		if err != nil {
			return fmt.Errorf("read a batch of components again under their sources: %w", err)
		}
		held, err := pgx.CollectRows(rows, scanComponentRow)
		if err != nil {
			return fmt.Errorf("read a batch of components again under their sources: %w", err)
		}
		if len(held) == 0 {
			return nil
		}
		addresses := make([]string, len(held))
		heldProjects := make([]string, len(held))
		heldIDs := make([]string, len(held))
		for index, component := range held {
			addresses[index] = graphstore.ComponentAddress(component.project, component.id)
			heldProjects[index], heldIDs[index] = component.project, component.id
		}
		relations, err := graphstore.ComponentRelations(ctx, tx, addresses)
		if err != nil {
			return err
		}
		nodes := make([]graphstore.Node, len(held))
		var tuples []graphstore.Tuple
		for index, component := range held {
			address := addresses[index]
			nodes[index] = graphstore.ComponentNode(component.project, component.id, backfillActor, component.importedAt)
			var parent []string
			if component.parent != nil {
				parent = []string{graphstore.ComponentAddress(component.project, *component.parent)}
			}
			tuples = append(tuples, graphstore.SetChanges(relations[address], graphstore.TuplePartOf, parent,
				func(to string, asserted bool) graphstore.Tuple {
					return graphstore.ComponentRelation(graphstore.KindComponent, address, graphstore.TuplePartOf, to, asserted, backfillActor, component.importedAt)
				})...)
		}
		if err := graphstore.Write(ctx, tx, nodes, tuples); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			update components c set node_kind = $3, node_address = c.project_key || '/' || c.id
			from unnest($1::text[], $2::text[]) as batch (project_key, id)
			where c.project_key = batch.project_key and c.id = batch.id
		`, heldProjects, heldIDs, graphstore.KindComponent); err != nil {
			return err
		}
		written, stated = int64(len(held)), statedOf(nodes)+int64(len(tuples))
		return nil
	}); err != nil {
		return 0, 0, fmt.Errorf("write a batch of components: %w", err)
	}
	return written, stated, nil
}

// lockComponentSources takes the row of every architecture source of projects `for update`, in
// ascending (project, id): the owner rows of a project's components, which the importer takes
// for a project in that order (lockProjectSources) before it writes a component's node or row.
func lockComponentSources(ctx context.Context, tx pgx.Tx, projects []string) error {
	if _, err := tx.Exec(ctx, `
		select 1 from architecture_sources where project_key = any($1::text[]) order by project_key, id for update
	`, projects); err != nil {
		return fmt.Errorf("lock the architecture sources of a batch: %w", err)
	}
	return nil
}

// componentDependsRow is one component_depends row a process of the release before wrote, naming
// no node, with the imported_at of its component's snapshot, which graph_edges' old arm dates the
// edge at.
type componentDependsRow struct {
	project, from, to string
	importedAt        time.Time
}

// componentDependsFits is componentFits for the component_depends row aliased d: both of its ends
// are inside the store's bound.
var componentDependsFits = `octet_length(d.project_key || '/' || d.from_id) <= ` + addressBound +
	` and octet_length(d.project_key || '/' || d.to_id) <= ` + addressBound

// backfillComponentDepends states, for every component_depends row naming no node, the depends_on
// tuple it is the projection of, dated at its component's snapshot's imported_at and at the
// sentinel, as graph_edges' old arm dates the edge, and names the component's node on the row in
// the transaction that states the tuple. It walks the rows in their primary key's order, one batch
// a transaction (writeComponentDependsBatch).
func backfillComponentDepends(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: "component_depends"}
	var afterProject, afterFrom, afterTo string
	for {
		rows, err := database.Pool.Query(ctx, `
			select d.project_key, d.from_id, d.to_id, s.imported_at
			from component_depends d
			join components c on c.project_key = d.project_key and c.id = d.from_id
			join architecture_snapshots s on s.id = c.snapshot_id
			where d.node_kind is null and `+componentDependsFits+` and (d.project_key, d.from_id, d.to_id) > ($1, $2, $3)
			order by d.project_key, d.from_id, d.to_id
			limit $4
		`, afterProject, afterFrom, afterTo, backfillBatch)
		if err != nil {
			return done, fmt.Errorf("read the component dependencies naming no node: %w", err)
		}
		batch, err := pgx.CollectRows(rows, scanComponentDependsRow)
		if err != nil {
			return done, fmt.Errorf("read the component dependencies naming no node: %w", err)
		}
		if len(batch) == 0 {
			break
		}
		last := batch[len(batch)-1]
		afterProject, afterFrom, afterTo = last.project, last.from, last.to
		written, err := writeComponentDependsBatch(ctx, database, batch)
		if err != nil {
			return done, err
		}
		done.Rows += written
		done.Tuples += written
		if len(batch) < backfillBatch {
			break
		}
	}
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from component_depends d where d.node_kind is null and `+componentDependsFits,
	).Scan(&done.Remaining); err != nil {
		return done, fmt.Errorf("count the component dependencies still naming no node: %w", err)
	}
	done.Complete = done.Remaining == 0
	return done, nil
}

func scanComponentDependsRow(row pgx.CollectableRow) (componentDependsRow, error) {
	var dependency componentDependsRow
	err := row.Scan(&dependency.project, &dependency.from, &dependency.to, &dependency.importedAt)
	return dependency, err
}

// writeComponentDependsBatch states one batch's depends_on tuples and names their component's node
// on each row, in one transaction and in the store's lock order: the source rows of every project
// of the batch first (lockComponentSources), then the batch's rows read again under them, then the
// tuples (graphstore.Write, which writes no node: each component's node is backfillComponents'),
// then the rows that name the node. A dependency an import replaced meanwhile is not read again
// and states nothing; nor is one of a component whose own row names no node yet, which
// backfillComponents reaches first on its next run. It answers how many rows it named, one tuple
// each.
func writeComponentDependsBatch(ctx context.Context, database *store.Store, batch []componentDependsRow) (int64, error) {
	projects := make([]string, len(batch))
	froms := make([]string, len(batch))
	tos := make([]string, len(batch))
	for index, dependency := range batch {
		projects[index], froms[index], tos[index] = dependency.project, dependency.from, dependency.to
	}
	var written int64
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if err := lockComponentSources(ctx, tx, projects); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			select d.project_key, d.from_id, d.to_id, s.imported_at
			from component_depends d
			join unnest($1::text[], $2::text[], $3::text[]) as batch (project_key, from_id, to_id)
			  on batch.project_key = d.project_key and batch.from_id = d.from_id and batch.to_id = d.to_id
			join components c on c.project_key = d.project_key and c.id = d.from_id and c.node_kind is not null
			join architecture_snapshots s on s.id = c.snapshot_id
			where d.node_kind is null
			order by d.project_key, d.from_id, d.to_id
		`, projects, froms, tos)
		if err != nil {
			return fmt.Errorf("read a batch of component dependencies again under their sources: %w", err)
		}
		held, err := pgx.CollectRows(rows, scanComponentDependsRow)
		if err != nil {
			return fmt.Errorf("read a batch of component dependencies again under their sources: %w", err)
		}
		if len(held) == 0 {
			return nil
		}
		tuples := make([]graphstore.Tuple, len(held))
		heldProjects := make([]string, len(held))
		heldFroms := make([]string, len(held))
		heldTos := make([]string, len(held))
		for index, dependency := range held {
			tuples[index] = graphstore.ComponentRelation(graphstore.KindComponent,
				graphstore.ComponentAddress(dependency.project, dependency.from), graphstore.TupleDependsOn,
				graphstore.ComponentAddress(dependency.project, dependency.to), true, backfillActor, dependency.importedAt)
			heldProjects[index], heldFroms[index], heldTos[index] = dependency.project, dependency.from, dependency.to
		}
		if err := graphstore.Write(ctx, tx, nil, tuples); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			update component_depends d set node_kind = $4, node_address = d.project_key || '/' || d.from_id
			from unnest($1::text[], $2::text[], $3::text[]) as batch (project_key, from_id, to_id)
			where d.project_key = batch.project_key and d.from_id = batch.from_id and d.to_id = batch.to_id
		`, heldProjects, heldFroms, heldTos, graphstore.KindComponent); err != nil {
			return err
		}
		written = int64(len(held))
		return nil
	}); err != nil {
		return 0, fmt.Errorf("write a batch of component dependencies: %w", err)
	}
	return written, nil
}

// issueComponentMemberRow is one issue_component_members row a process of the release before
// wrote, naming no node, with the updated_at of its issue_components row, which graph_edges' old
// arm dates the edge at.
type issueComponentMemberRow struct {
	issue, project, component string
	updatedAt                 time.Time
}

// memberFits is the test that the issue_component_members row aliased m names a component inside
// the store's bound.
var memberFits = `octet_length(m.project_key || '/' || m.component_id) <= ` + addressBound

// backfillIssueComponentMembers states, for every issue_component_members row naming no node, the
// affects tuple it is the projection of, dated at its issue_components row's updated_at and at the
// sentinel, as graph_edges' old arm dates the edge, and names the issue's node on the row in the
// transaction that states the tuple. It walks the rows in their primary key's order, one batch a
// transaction (writeIssueComponentMemberBatch).
func backfillIssueComponentMembers(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: "issue_component_member"}
	var afterIssue, afterComponent string
	for {
		rows, err := database.Pool.Query(ctx, `
			select m.issue_key, m.project_key, m.component_id, c.updated_at
			from issue_component_members m join issue_components c using (issue_key)
			where m.node_kind is null and `+memberFits+` and (m.issue_key, m.component_id) > ($1, $2)
			order by m.issue_key, m.component_id
			limit $3
		`, afterIssue, afterComponent, backfillBatch)
		if err != nil {
			return done, fmt.Errorf("read the issue components naming no node: %w", err)
		}
		batch, err := pgx.CollectRows(rows, scanIssueComponentMemberRow)
		if err != nil {
			return done, fmt.Errorf("read the issue components naming no node: %w", err)
		}
		if len(batch) == 0 {
			break
		}
		afterIssue, afterComponent = batch[len(batch)-1].issue, batch[len(batch)-1].component
		written, err := writeIssueComponentMemberBatch(ctx, database, batch)
		if err != nil {
			return done, err
		}
		done.Rows += written
		done.Tuples += written
		if len(batch) < backfillBatch {
			break
		}
	}
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from issue_component_members m where m.node_kind is null and `+memberFits,
	).Scan(&done.Remaining); err != nil {
		return done, fmt.Errorf("count the issue components still naming no node: %w", err)
	}
	done.Complete = done.Remaining == 0
	return done, nil
}

func scanIssueComponentMemberRow(row pgx.CollectableRow) (issueComponentMemberRow, error) {
	var member issueComponentMemberRow
	err := row.Scan(&member.issue, &member.project, &member.component, &member.updatedAt)
	return member, err
}

// writeIssueComponentMemberBatch states one batch's affects tuples and names their issue's node on
// each row, in one transaction and in the store's lock order (api/inbox_read.go's
// markInboxReadThreads): the issue rows the batch's rows stand under first, each once, in
// ascending key (events.LockOwnerRows), which an issue's components write takes first too (its
// create's and its PATCH's lock of the issue's row), then the batch's rows read again under them,
// then the tuples (graphstore.Write, which writes no node: each issue's node is the object
// pass's), then the rows that name the node. A row the issue's own write replaced meanwhile is not
// read again and states nothing; nor is one of an issue whose own row names no node yet, which the
// object pass reaches first on its next run. It answers how many rows it named, one tuple each.
func writeIssueComponentMemberBatch(ctx context.Context, database *store.Store, batch []issueComponentMemberRow) (int64, error) {
	var written int64
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		owners := events.OwnerRows{}
		issues := make([]string, len(batch))
		components := make([]string, len(batch))
		for index, member := range batch {
			owners.Add(member.issue, "", "issue "+member.issue)
			issues[index], components[index] = member.issue, member.component
		}
		if err := events.LockOwnerRows(ctx, tx, owners); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			select m.issue_key, m.project_key, m.component_id, c.updated_at
			from issue_component_members m
			join unnest($1::text[], $2::text[]) as batch (issue_key, component_id)
			  on batch.issue_key = m.issue_key and batch.component_id = m.component_id
			join issue_components c on c.issue_key = m.issue_key
			join issues i on i.key = m.issue_key and i.node_kind is not null
			where m.node_kind is null
			order by m.issue_key, m.component_id
		`, issues, components)
		if err != nil {
			return fmt.Errorf("read a batch of issue components again under their issues: %w", err)
		}
		held, err := pgx.CollectRows(rows, scanIssueComponentMemberRow)
		if err != nil {
			return fmt.Errorf("read a batch of issue components again under their issues: %w", err)
		}
		if len(held) == 0 {
			return nil
		}
		tuples := make([]graphstore.Tuple, len(held))
		heldIssues := make([]string, len(held))
		heldComponents := make([]string, len(held))
		for index, member := range held {
			tuples[index] = graphstore.ComponentRelation(graphstore.KindIssue, member.issue, graphstore.TupleAffects,
				graphstore.ComponentAddress(member.project, member.component), true, backfillActor, member.updatedAt)
			heldIssues[index], heldComponents[index] = member.issue, member.component
		}
		if err := graphstore.Write(ctx, tx, nil, tuples); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			update issue_component_members m set node_kind = $3, node_address = m.issue_key
			from unnest($1::text[], $2::text[]) as batch (issue_key, component_id)
			where m.issue_key = batch.issue_key and m.component_id = batch.component_id
		`, heldIssues, heldComponents, graphstore.KindIssue); err != nil {
			return err
		}
		written = int64(len(held))
		return nil
	}); err != nil {
		return 0, fmt.Errorf("write a batch of issue components: %w", err)
	}
	return written, nil
}
