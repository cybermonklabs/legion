package graphstore

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Scope is the project, or the parties, one node belongs to: what FillScopes states of a node a
// build before this one stored with neither.
type Scope struct {
	Kind, ID string
	Project  string
	Parties  []string
}

// FillScopes states the project, or the parties, of each node of scopes that carries neither, and
// answers how many it filled. It is the backfill's write over the comment and block nodes the
// writers stored before a node named its project: each such node takes the base attribute
// `project` or `parties` (Node), stated as the node's own write would have stated it - at the
// node's made_at, by its made_by, under its source_event - and the column that projects it.
//
// A node that carries a project or parties already is left as it is, read on the newest version
// of its row: a writer that stated one meanwhile wins, and a node's project never moves
// (ErrProjectMoved). Nothing else of the node moves, so its state and its visibility stay what
// their newest writer left them.
//
// The rows are locked in ascending (kind, id) before the update, in a statement of their own, as
// every write of this store takes them (queueNodes): an update over an unnest takes its rows in
// whatever order the plan reads them. The lock and the fill are one batch of their own
// (store.RunWrite), and the fill states each attribute it moved in the same statement as its
// update, read off the rows the update returns, in ascending (kind, id), the order the rows were
// locked in, so the log reads the facts in that order and no Go code runs between the update and
// its facts. It answers the count the statement reads off those rows.
func FillScopes(ctx context.Context, tx pgx.Tx, scopes []Scope) (int64, error) {
	if len(scopes) == 0 {
		return 0, nil
	}
	kinds := make([]string, len(scopes))
	ids := make([]string, len(scopes))
	projects := make([]*string, len(scopes))
	parties := make([]*string, len(scopes))
	for index, scope := range scopes {
		kinds[index], ids[index] = scope.Kind, scope.ID
		if scope.Project != "" {
			projects[index] = &scopes[index].Project
		}
		value, err := PartiesValue(scope.Parties)
		if err != nil {
			return 0, fmt.Errorf("fill the scope of node %s/%s: %w", scope.Kind, scope.ID, err)
		}
		if value != "" {
			parties[index] = &value
		}
	}
	var filled int64
	err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		b.Queue("lock the nodes whose scope the write fills", `
			select 1 from graph_nodes held
			join unnest($1::text[], $2::text[]) as named(kind, id) on named.kind = held.kind and named.id = held.id
			order by held.kind, held.id
			for no key update of held
		`, kinds, ids)
		b.Queue("fill the scope of graph nodes", `
			with filled as (
				update graph_nodes set
					project_key = scope.project,
					parties = string_to_array(scope.parties, ' ')
				from unnest($1::text[], $2::text[], $3::text[], $4::text[]) as scope(kind, id, project, parties)
				where graph_nodes.kind = scope.kind and graph_nodes.id = scope.id
				  and graph_nodes.project_key is null and graph_nodes.parties is null
				  and (scope.project is not null or scope.parties is not null)
				returning graph_nodes.kind, graph_nodes.id, graph_nodes.project_key, graph_nodes.parties,
					graph_nodes.made_at, graph_nodes.made_by, graph_nodes.source_event
			),
			stated as (
				insert into graph_tuples (`+tupleWriteColumns+`)
				select kind, id,
				       case when project_key is not null then '`+AttributeProject+`' else '`+AttributeParties+`' end,
				       '`+KindValue+`', coalesce(project_key, array_to_string(parties, ' ')),
				       made_at, made_by, source_event, null::bigint, true
				from filled
				order by kind, id
				on conflict do nothing
			)
			select count(*) from filled
		`, kinds, ids, projects, parties).QueryRow(func(row pgx.Row) error { return row.Scan(&filled) })
		return nil
	})
	if err != nil {
		return 0, err
	}
	return filled, nil
}
