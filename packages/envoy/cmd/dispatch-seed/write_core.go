package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// seedActor is the author every seeded row carries that is not a session's. It is a user actor,
// the shape `author`, `created_by` and `actor` hold everywhere in the schema.
const seedActor = `{"kind":"user","id":"seed@dispatch.invalid","name":"Seed"}`

// seedAuthor is seedActor as the model reads it, for the event payloads the seed builds through
// model rather than as text (askOpenedPayload). It is decoded from the one constant instead of
// written out a second time, so the author a payload names cannot drift from the author its row
// holds; model.Actor carries no display name, so the payload holds the kind and the id alone,
// which is what a server's own payload holds.
var seedAuthor = decodeSeedActor()

func decodeSeedActor() model.Actor {
	var actor model.Actor
	if err := json.Unmarshal([]byte(seedActor), &actor); err != nil {
		panic(fmt.Sprintf("dispatch-seed: seedActor is not an actor: %v", err))
	}
	return actor
}

// sessionActor is the actor of a session, as events.actor carries one: the shape
// `events_session_actor_created_at_idx` is partial on and `agentLastActivity` reads.
func sessionActor(session string) string {
	return fmt.Sprintf(`{"kind":"session","id":%q}`, session)
}

// writeProjects writes the projects. Each project has a default repository (default_repo), which
// an issue with no repository of its own resolves to. The issues that carry a repository of their
// own name a second one (projectPlan.OwnRepo). Each project also names the seed's own person as
// its default assignee, the column every project fills
// (store/migrations/20261111210103_projects_default_assignee_not_null.up.sql), and that person
// is written first as somebody who has signed in (0068_people): every write that stores a
// default holds it to a people row (allowedLogin, internal/dispatch/api/issue_assignee.go), so
// the person a seeded project's unowned questions reach is one the server knows, offers in the
// assignee picker and can hand a question back to. The row is written as
// store.PgPeopleStore.Record writes one, and left alone where somebody signed in under that
// address before the seed ran.
func (r *seedRun) writeProjects(ctx context.Context) error {
	return r.inTransaction(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			insert into people (email, signed_in_at) values ($1, $2) on conflict (email) do nothing
		`, seedAuthor.ID, baseTime); err != nil {
			return fmt.Errorf("person %s: %w", seedAuthor.ID, err)
		}
		for index, project := range r.plan.projects {
			if _, err := tx.Exec(ctx, `
				insert into projects (key, name, default_assignee, default_repo, next_number, created_at, last_seq)
				values ($1, $2, $3, $4, 1, $5, 0)
			`, project.Key, project.Name, seedAuthor.ID, project.Repo, timeAt(index, len(r.plan.projects))); err != nil {
				return fmt.Errorf("project %s: %w", project.Key, err)
			}
		}
		return nil
	})
}

// writeArchitecture writes one architecture source and one snapshot for each project, the
// components of that snapshot, and the dependencies between them: the three tables the
// `component` arms of graph_edges read. The components are written as an import writes them
// (architecture's stateComponents): each component's node, its part_of its parent and its
// depends_on, through graphstore.Write in the transaction that then inserts the rows, each row
// naming its node. A database migrated from a release before the components' node columns
// carries none, and its rows are written as that release wrote them, naming no node, which the
// backfill states (carriesNodeMarker).
func (r *seedRun) writeArchitecture(ctx context.Context) error {
	marked, err := r.carriesNodeMarker(ctx, "components")
	if err != nil {
		return err
	}
	return r.inTransaction(ctx, func(ctx context.Context, tx pgx.Tx) error {
		for index, project := range r.plan.projects {
			snapshot := int64(index + 1)
			if _, err := tx.Exec(ctx, `
				insert into architecture_sources (project_key, repo, branch, installation_id, created_by, created_at, id)
				values ($1, $2, 'main', 1, $3::jsonb, $4, $5)
			`, project.Key, project.Repo, seedActor, baseTime, snapshot); err != nil {
				return fmt.Errorf("architecture source of %s: %w", project.Key, err)
			}
			if _, err := tx.Exec(ctx, `
				insert into architecture_snapshots (id, project_key, commit, imported_at, files)
				values ($1, $2, $3, $4, '[]'::jsonb)
			`, snapshot, project.Key, fmt.Sprintf("%040x", snapshot), baseTime); err != nil {
				return fmt.Errorf("architecture snapshot of %s: %w", project.Key, err)
			}
		}
		if marked {
			nodes, tuples := r.plan.componentFacts()
			if err := graphstore.Write(ctx, tx, nodes, tuples); err != nil {
				return fmt.Errorf("the components' nodes: %w", err)
			}
		}
		for _, component := range r.plan.components {
			project := r.plan.projects[component.Project]
			snapshot := int64(component.Project + 1)
			var parent *string
			if component.Parent != "" {
				parent = &component.Parent
			}
			title, prose := r.plan.vocabulary.phrase(r.random, 3), r.plan.vocabulary.phrase(r.random, 12)
			statement := `
				insert into components (project_key, id, title, prose, parent, paths, snapshot_id, source_id)
				values ($1, $2, $3, $4, $5, '{}', $6, $6)`
			args := []any{project.Key, component.ID, title, prose, parent, snapshot}
			if marked {
				statement = `
					insert into components (project_key, id, title, prose, parent, paths, snapshot_id, source_id, node_kind, node_address)
					values ($1, $2, $3, $4, $5, '{}', $6, $6, $7, $8)`
				args = append(args, componentNodeOf(project.Key, component)...)
			}
			if _, err := tx.Exec(ctx, statement, args...); err != nil {
				return fmt.Errorf("component %s/%s: %w", project.Key, component.ID, err)
			}
		}
		for _, component := range r.plan.components {
			if component.Depends == "" {
				continue
			}
			project := r.plan.projects[component.Project].Key
			statement := `insert into component_depends (project_key, from_id, to_id) values ($1, $2, $3)`
			args := []any{project, component.ID, component.Depends}
			if marked {
				// A dependency names its component's node where the node write stated its
				// depends_on, as an import's does: the component is a node and the far end is
				// inside the address bound.
				statement = `insert into component_depends (project_key, from_id, to_id, node_kind, node_address) values ($1, $2, $3, $4, $5)`
				if graphstore.AddressFits(graphstore.ComponentAddress(project, component.Depends)) {
					args = append(args, componentNodeOf(project, component)...)
				} else {
					args = append(args, nil, nil)
				}
			}
			if _, err := tx.Exec(ctx, statement, args...); err != nil {
				return fmt.Errorf("component dependency %s: %w", component.ID, err)
			}
		}
		return nil
	})
}

// componentNodeOf is the node a components row of project names, kind and address, or two nulls
// for a component the store holds no node of (componentStated).
func componentNodeOf(project string, component componentPlan) []any {
	if !componentStated(project, component) {
		return []any{nil, nil}
	}
	return []any{graphstore.KindComponent, graphstore.ComponentAddress(project, component.ID)}
}

// componentStated reports whether the store holds component of project as a node: neither its
// address nor its parent's is past the store's bound (graphstore.AddressFits), as an import reads
// it (architecture's stateComponents).
func componentStated(project string, component componentPlan) bool {
	return graphstore.AddressFits(graphstore.ComponentAddress(project, component.ID)) &&
		(component.Parent == "" || graphstore.AddressFits(graphstore.ComponentAddress(project, component.Parent)))
}

// componentFacts is what the store states of the plan's components, as an import of a first
// model states it (architecture's stateComponents): each component's node in its project,
// restated live, its part_of its parent and its depends_on, dated at the seed's base time and
// signed by the seed's author, at the sentinel, since the seed records no event of an import. A
// component the store holds no node of (componentStated) is left out, and a dependency on an end
// past the bound is stated of none.
func (p *seedPlan) componentFacts() ([]graphstore.Node, []graphstore.Tuple) {
	var nodes []graphstore.Node
	var tuples []graphstore.Tuple
	for _, component := range p.components {
		project := p.projects[component.Project].Key
		if !componentStated(project, component) {
			continue
		}
		address := graphstore.ComponentAddress(project, component.ID)
		nodes = append(nodes, graphstore.ComponentNode(project, component.ID, seedAuthor, baseTime))
		if component.Parent != "" {
			tuples = append(tuples, graphstore.ComponentRelation(graphstore.KindComponent, address, graphstore.TuplePartOf, graphstore.ComponentAddress(project, component.Parent), true, seedAuthor, baseTime))
		}
		if depends := graphstore.ComponentAddress(project, component.Depends); component.Depends != "" && graphstore.AddressFits(depends) {
			tuples = append(tuples, graphstore.ComponentRelation(graphstore.KindComponent, address, graphstore.TupleDependsOn, depends, true, seedAuthor, baseTime))
		}
	}
	return nodes, tuples
}

// writeIssues writes every issue, parents before children, so issues_parent_key_fkey holds on
// each row as it lands. The search vector, the title lexemes and the embedding queue row are the
// triggers' work, as they are on every other write. Each batch writes its issues' nodes first, in
// the transaction that copies their rows and in each issue's project, with the child_of of each
// issue that has a parent, and each row names its node (node_kind, node_address), as the server's
// issue create does (api's createIssue); the seed records no event of an issue, so what the node
// write states carries the sentinel. A database migrated from a release before the marker migration
// (20261116092210) carries no node columns on issues, and its rows are written without them
// (carriesNodeMarker), and without a child_of, which the backfill states with the row's node.
func (r *seedRun) writeIssues(ctx context.Context) error {
	marked, err := r.carriesNodeMarker(ctx, "issues")
	if err != nil {
		return err
	}
	columns := []string{
		"key", "project_key", "number", "title", "status", "parent_key", "created_by",
		"created_at", "updated_at", "closed_at", "rank", "repo", "priority",
	}
	if marked {
		columns = append(columns, "node_kind", "node_address")
	}
	issues := r.plan.issues
	return r.inBatches(ctx, "issues", len(issues), batchRows, func(ctx context.Context, tx pgx.Tx, from, to int) error {
		rows := make([][]any, 0, to-from)
		nodes := make([]graphstore.Node, 0, to-from)
		var tuples []graphstore.Tuple
		for index := from; index < to; index++ {
			issue := issues[index]
			project := r.plan.projects[issue.Project].Key
			var parent *string
			if issue.Parent >= 0 {
				parent = &issues[issue.Parent].Key
			}
			var repo *string
			if issue.OwnRepo {
				repo = &r.plan.projects[issue.Project].OwnRepo
			}
			var closedAt any
			if issue.Status == "done" {
				closedAt = issue.CreatedAt.Add(seedSpan / 10)
			}
			row := []any{
				issue.Key, project, issue.Number,
				r.plan.vocabulary.phrase(r.random, 7), issue.Status, parent, seedActor,
				issue.CreatedAt, issue.CreatedAt, closedAt, issue.Rank, repo, int16(index % 4),
			}
			if marked {
				row = append(row, graphstore.KindIssue, issue.Key)
			}
			rows = append(rows, row)
			nodes = append(nodes, graphstore.IssueNode(issue.Key, project, seedAuthor, issue.CreatedAt))
			if marked && parent != nil {
				tuples = append(tuples, graphstore.IssueChildOf(issue.Key, *parent, seedAuthor, issue.CreatedAt))
			}
		}
		// The nodes go in before the rows, which name them, and their head rows after the rows,
		// since an issue's node is held under the issue whose row it names: one head write.
		if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
			graphstore.QueueWrite(b, nodes, tuples)
			return nil
		}); err != nil {
			return fmt.Errorf("the issues' nodes: %w", err)
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"issues"}, columns, pgx.CopyFromRows(rows)); err != nil {
			return err
		}
		return graphstore.WriteHeads(ctx, tx, graphstore.HeadsOf(nodes, tuples))
	})
}

// carriesNodeMarker reads whether table carries the node pair migration 20261116092210 adds
// (node_kind, node_address). The measurement protocol seeds a database migrated from the release
// before this one with --no-migrate, whose issues and artifacts carry neither column, and an
// insert naming them fails there; on such a database a row is written as that release's server
// writes it, naming no node, and its marker is the backfill's to write.
func (r *seedRun) carriesNodeMarker(ctx context.Context, table string) (bool, error) {
	var carries bool
	if err := r.database.Pool.QueryRow(ctx, `select exists (
		select 1 from information_schema.columns
		where table_schema = current_schema() and table_name = $1 and column_name = 'node_address'
	)`, table).Scan(&carries); err != nil {
		return false, fmt.Errorf("read whether %s carries the node columns: %w", table, err)
	}
	return carries, nil
}

// writeIssueLinks writes the waits between issues. The heavily linked issue takes HubLinks of them,
// so it waits on that many issues; the rest are spread so that the total is exactly the figure the
// spec gives and a known share of the issues carry three or more. Each batch states the issues'
// needs tuples through graphstore.Write and then copies their rows, in one transaction, as the
// server's issue write does (api's writeIssueNeeds): the row keeps the one kind issue_links admits,
// blocked_by, and names the waiting issue's node; the tuple, out of that node, is dated at the
// row's created_at and carries the sentinel, since the seed records no event of an issue. A
// database migrated from a release before the marker migration carries no node columns on
// issue_links, and its rows are written as that release wrote them, which the backfill states.
func (r *seedRun) writeIssueLinks(ctx context.Context) error {
	marked, err := r.carriesNodeMarker(ctx, "issue_links")
	if err != nil {
		return err
	}
	columns := []string{"issue_key", "kind", "target_key", "created_at"}
	if marked {
		columns = append(columns, "node_kind", "node_address")
	}
	links := r.plan.blockedByLinks()
	return r.inBatches(ctx, "issue links", len(links), batchRows, func(ctx context.Context, tx pgx.Tx, from, to int) error {
		rows := make([][]any, 0, to-from)
		tuples := make([]graphstore.Tuple, 0, to-from)
		for _, link := range links[from:to] {
			issue, target := r.plan.issues[link[0]], r.plan.issues[link[1]]
			row := []any{issue.Key, "blocked_by", target.Key, issue.CreatedAt}
			if marked {
				row = append(row, graphstore.KindIssue, issue.Key)
				tuples = append(tuples, graphstore.IssueNeeds(issue.Key, target.Key, true, seedAuthor, issue.CreatedAt))
			}
			rows = append(rows, row)
		}
		if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
			graphstore.QueueWrite(b, nil, tuples)
			graphstore.QueueHeads(b, graphstore.HeadsOf(nil, tuples))
			return nil
		}); err != nil {
			return fmt.Errorf("the issues' needs: %w", err)
		}
		_, err := tx.CopyFrom(ctx, pgx.Identifier{"issue_links"}, columns, pgx.CopyFromRows(rows))
		return err
	})
}

// blockedByLinks is every (issue, blocker) pair the seed writes, in issue order. The hub's are
// first; then each issue takes its share of what is left, which gives a quarter of them three
// blockers and so the "three or more blockers" the spec asks for.
func (p *seedPlan) blockedByLinks() [][2]int {
	links := make([][2]int, 0, p.sizes.BlockedBy)
	seen := make(map[[2]int]struct{}, p.sizes.BlockedBy)
	add := func(issue, blocker int) bool {
		if issue == blocker || issue < 0 || blocker < 0 {
			return false
		}
		pair := [2]int{issue, blocker}
		if _, held := seen[pair]; held {
			return false
		}
		seen[pair] = struct{}{}
		links = append(links, pair)
		return true
	}
	for step := 1; step <= p.sizes.HubLinks && len(links) < p.sizes.BlockedBy; step++ {
		add(p.hub, p.hub+step)
	}
	// Four blockers on every fourth issue and one on every other gives 1.75 per four issues,
	// close to the 19,800 links over 20,000 issues the spec gives, with a quarter of the issues
	// above the "three or more" line.
	for index := range p.issues {
		if len(links) >= p.sizes.BlockedBy {
			break
		}
		blockers := 1
		if index%4 == 3 {
			blockers = 4
		}
		project := p.projects[p.issues[index].Project]
		for step := 1; step <= blockers && len(links) < p.sizes.BlockedBy; step++ {
			if project.Issues < 2 {
				break
			}
			target := project.FirstIssue + (p.issues[index].Number-1+step*7)%project.Issues
			add(index, target)
		}
	}
	return links
}

// writeIssueComponents puts the explicit component rows on the issues that have children, so an
// issue's components resolve up its parent chain, and gives each of them members: the `affects`
// arm of graph_edges. Each batch states the issues' affects tuples through graphstore.Write and
// then copies their rows, in one transaction, as the server's write of an issue's components
// does (api's writeIssueComponents): a member whose component's address is inside the store's
// bound names the issue's node, and its tuple, out of that node to the component's, is dated at
// the issue_components row's updated_at and carries the sentinel. A database migrated from a
// release before the members' node columns carries none, and its rows are written as that release
// wrote them, which the backfill states.
func (r *seedRun) writeIssueComponents(ctx context.Context) error {
	marked, err := r.carriesNodeMarker(ctx, "issue_component_members")
	if err != nil {
		return err
	}
	columns := []string{"issue_key", "project_key", "component_id"}
	if marked {
		columns = append(columns, "node_kind", "node_address")
	}
	var ancestors []int
	for index, issue := range r.plan.issues {
		if issue.Children > 0 {
			ancestors = append(ancestors, index)
		}
	}
	perProject := max(1, len(r.plan.components)/len(r.plan.projects))
	return r.inBatches(ctx, "issue components", len(ancestors), batchRows, func(ctx context.Context, tx pgx.Tx, from, to int) error {
		owners := make([][]any, 0, to-from)
		members := make([][]any, 0, 2*(to-from))
		var tuples []graphstore.Tuple
		for position, index := range ancestors[from:to] {
			issue := r.plan.issues[index]
			project := r.plan.projects[issue.Project]
			owners = append(owners, []any{issue.Key, "explicit", nil, issue.CreatedAt, issue.CreatedAt})
			for step := range min(2, perProject) {
				component := fmt.Sprintf("component-%d", (from+position+step)%perProject+1)
				member := []any{issue.Key, project.Key, component}
				if marked {
					if address := graphstore.ComponentAddress(project.Key, component); graphstore.AddressFits(address) {
						member = append(member, graphstore.KindIssue, issue.Key)
						tuples = append(tuples, graphstore.ComponentRelation(graphstore.KindIssue, issue.Key, graphstore.TupleAffects, address, true, seedAuthor, issue.CreatedAt))
					} else {
						member = append(member, nil, nil)
					}
				}
				members = append(members, member)
			}
		}
		if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
			graphstore.QueueWrite(b, nil, tuples)
			graphstore.QueueHeads(b, graphstore.HeadsOf(nil, tuples))
			return nil
		}); err != nil {
			return fmt.Errorf("the issues' components: %w", err)
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"issue_components"},
			[]string{"issue_key", "mode", "reason", "created_at", "updated_at"},
			pgx.CopyFromRows(owners)); err != nil {
			return err
		}
		_, err := tx.CopyFrom(ctx, pgx.Identifier{"issue_component_members"}, columns, pgx.CopyFromRows(members))
		return err
	})
}
