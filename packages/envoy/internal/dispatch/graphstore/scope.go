package graphstore

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
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
// whatever order the plan reads them.
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
		project, partiesColumn, err := scopeColumns(&scopes[index].Project, scope.Parties)
		if err != nil {
			return 0, fmt.Errorf("fill the scope of node %s/%s: %w", scope.Kind, scope.ID, err)
		}
		projects[index], parties[index] = project, partiesColumn
	}
	if _, err := tx.Exec(ctx, `
		select 1 from graph_nodes held
		join unnest($1::text[], $2::text[]) as named(kind, id) on named.kind = held.kind and named.id = held.id
		order by held.kind, held.id
		for no key update of held
	`, kinds, ids); err != nil {
		return 0, fmt.Errorf("lock the nodes whose scope the write fills: %w", err)
	}
	rows, err := tx.Query(ctx, `
		update graph_nodes set
			project_key = scope.project,
			parties = string_to_array(scope.parties, ' ')
		from unnest($1::text[], $2::text[], $3::text[], $4::text[]) as scope(kind, id, project, parties)
		where graph_nodes.kind = scope.kind and graph_nodes.id = scope.id
		  and graph_nodes.project_key is null and graph_nodes.parties is null
		  and (scope.project is not null or scope.parties is not null)
		returning graph_nodes.kind, graph_nodes.id, graph_nodes.project_key, graph_nodes.parties,
			graph_nodes.made_at, graph_nodes.made_by, graph_nodes.source_event
	`, kinds, ids, projects, parties)
	if err != nil {
		return 0, fmt.Errorf("fill the scope of graph nodes: %w", err)
	}
	defer rows.Close()
	var stated []Tuple
	var filled int64
	for rows.Next() {
		var kind, id string
		var project *string
		var held []string
		var madeAt time.Time
		var madeBy []byte
		var event int64
		if err := rows.Scan(&kind, &id, &project, &held, &madeAt, &madeBy, &event); err != nil {
			return 0, fmt.Errorf("fill the scope of graph nodes: %w", err)
		}
		var who model.Actor
		if err := json.Unmarshal(madeBy, &who); err != nil {
			return 0, fmt.Errorf("decode the writer of node %s/%s: %w", kind, id, err)
		}
		filled++
		fact := Tuple{FromKind: kind, FromID: id, ToKind: KindValue, At: madeAt, Who: who, SourceEvent: event, Asserted: true}
		if project != nil {
			fact.Kind, fact.ToID = AttributeProject, *project
		} else {
			fact.Kind, fact.ToID = AttributeParties, strings.Join(held, " ")
		}
		stated = append(stated, fact)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("fill the scope of graph nodes: %w", err)
	}
	// In the store's order, so the log reads the facts in the order the rows were locked.
	slices.SortFunc(stated, func(left, right Tuple) int {
		if order := strings.Compare(left.FromKind, right.FromKind); order != 0 {
			return order
		}
		return strings.Compare(left.FromID, right.FromID)
	})
	if err := Write(ctx, tx, nil, stated); err != nil {
		return 0, err
	}
	return filled, nil
}
