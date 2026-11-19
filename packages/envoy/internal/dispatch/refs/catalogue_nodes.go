package refs

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The catalogue of lens kinds and the constraints a requirement or a kind is held to
// (dispatch://INFRA-1950/spec#b-r-lens-catalogue): a lens_kind node is a kind of the catalogue,
// addressed `dispatch://lens_kind/<slug>`, and a constraint node is addressed
// `dispatch://constraint/<uuid>`. Both are nodes of the store alone, with no projection row, so
// each is read from graph_nodes and its text from the attributes its row carries: a lens kind's
// words, and a constraint's standard or, for one that holds a measurement, the measurement.

// loadLensKindNodes resolves lens_kind nodes. A kind of the catalogue stands under no issue and in
// no project, has no parent, and is always project-visible, so every viewer reads it; it is read
// through the predicate all the same, as every node is. One the store holds gone is absent, as a
// node that does not exist is.
func loadLensKindNodes(ctx context.Context, q Queryer, who model.Actor, _ string, _ bool, ids []string, nodes map[[2]string]resolvedNode) error {
	rows, err := q.Query(ctx, `
		select id, made_by, made_at, state, visibility, coalesce(attributes->>'`+graphstore.AttributeWords+`', '')
		from graph_nodes held
		where kind = $1 and id = any($2) and state <> $3 and `+VisibleKindRow(graphstore.KindLensKind, "held", "$4"),
		graphstore.KindLensKind, ids, graphstore.StateGone, VisibilityArg(who))
	if err != nil {
		return fmt.Errorf("load lens_kind nodes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		node := model.GraphNode{Kind: graphstore.KindLensKind}
		var madeBy []byte
		var madeAt time.Time
		var words string
		if err := rows.Scan(&node.ID, &madeBy, &madeAt, &node.State, &node.Visibility, &words); err != nil {
			return fmt.Errorf("scan lens_kind node: %w", err)
		}
		node.Ref = StoreRef(graphstore.KindLensKind, node.ID)
		if err := setMadeAndChanged(&node, madeBy, madeAt, nil, nil, storedActors); err != nil {
			return err
		}
		nodes[[2]string{graphstore.KindLensKind, node.ID}] = resolvedNode{GraphNode: node, text: words}
	}
	return rows.Err()
}

// loadConstraintNodes resolves constraint nodes. A constraint written under an issue stands under
// it through its current part_of tuple, so it is located by that issue - its IssueKey, its parent,
// and the project its row names - and a route of `/issues/{key}/nodes/{id}/…` finds it there; a
// constraint of the catalogue, the far end of a lens kind's constrained_by tuple, stands under no
// issue and answers none of the three. Its text is its standard, or its measurement where it holds
// no standard (standingText). Through the predicate of its kind, which compares the author a
// credential proved, a constraint reserved for its author reaches that author alone. One the store
// holds gone is absent.
func loadConstraintNodes(ctx context.Context, q Queryer, who model.Actor, _ string, _ bool, ids []string, nodes map[[2]string]resolvedNode) error {
	rows, err := q.Query(ctx, `
		select id, made_by, made_at, state, visibility, project_key,
		       `+standingText[graphstore.KindConstraint]("held", "$4")+`,
		       placed.to_id
		from graph_nodes held
		left join lateral (
			select newest.to_id from (
				`+graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_id, t.asserted`,
		`t.from_kind = held.kind and t.from_id = held.id and t.kind = '`+graphstore.TuplePartOf+`'
					and t.to_kind = '`+graphstore.KindIssue+`' and t.to_kind <> '`+graphstore.KindValue+`'`)+`
			) newest
			where newest.asserted
			order by newest.to_id
			limit 1
		) placed on true
		where kind = $1 and id = any($2) and state <> $3 and `+VisibleKindRow(graphstore.KindConstraint, "held", "$4"),
		graphstore.KindConstraint, ids, graphstore.StateGone, VisibilityArg(who))
	if err != nil {
		return fmt.Errorf("load constraint nodes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		node := model.GraphNode{Kind: graphstore.KindConstraint}
		var madeBy []byte
		var madeAt time.Time
		var project *string
		var text string
		if err := rows.Scan(&node.ID, &madeBy, &madeAt, &node.State, &node.Visibility, &project, &text, &node.IssueKey); err != nil {
			return fmt.Errorf("scan constraint node: %w", err)
		}
		node.Ref = StoreRef(graphstore.KindConstraint, node.ID)
		if project != nil {
			node.Project = *project
		}
		node.Parent = issueParent(node.IssueKey)
		if err := setMadeAndChanged(&node, madeBy, madeAt, nil, nil, storedActors); err != nil {
			return err
		}
		nodes[[2]string{graphstore.KindConstraint, node.ID}] = resolvedNode{GraphNode: node, text: text}
	}
	return rows.Err()
}

// CatalogueKind is one kind of the lens catalogue as LensCatalogue reads it: its id, the slug its
// address `dispatch://lens_kind/<id>` names, its words, and the constraint each of its current
// constrained_by tuples reaches, which for a kind of the catalogue is exactly one.
type CatalogueKind struct {
	ID      string
	Words   string
	Sources []CatalogueSource
}

// CatalogueSource is a constraint a lens kind is held to: its id, and the standard and the
// reference it holds, which for a kind of the catalogue are the kind's primary source and that
// source's reference.
type CatalogueSource struct {
	ID        string `json:"id"`
	Standard  string `json:"standard"`
	Reference string `json:"reference"`
}

// LensCatalogue is every kind of the lens catalogue the store holds, by id, each with its words and
// the constraints its current constrained_by tuples reach, in one statement over the store, read
// for who through the predicate of each kind: so a kind added at runtime is read by the next call
// of any process, and no copy of the catalogue is kept. A kind or a constraint the store holds gone
// is absent.
func LensCatalogue(ctx context.Context, q Queryer, who model.Actor) ([]CatalogueKind, error) {
	rows, err := q.Query(ctx, `
		select catalogued.id, coalesce(catalogued.attributes->>'`+graphstore.AttributeWords+`', ''), coalesce(tied.sources, '[]')
		from graph_nodes catalogued
		left join lateral (
			select jsonb_agg(jsonb_build_object(
				'id', source.id,
				'standard', coalesce(source.attributes->>'`+graphstore.AttributeStandard+`', ''),
				'reference', coalesce(source.attributes->>'`+graphstore.AttributeReference+`', ''))
				order by source.id) as sources
			from (
				`+graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_kind, t.to_id, t.asserted`,
		`t.from_kind = catalogued.kind and t.from_id = catalogued.id and t.kind = '`+graphstore.TupleConstrainedBy+`'
					and t.to_kind = '`+graphstore.KindConstraint+`' and t.to_kind <> '`+graphstore.KindValue+`'`)+`
			) newest
			join graph_nodes source on source.kind = newest.to_kind and source.id = newest.to_id
			where newest.asserted and source.state <> $2 and `+VisibleKindRow(graphstore.KindConstraint, "source", "$3")+`
		) tied on true
		where catalogued.kind = $1 and catalogued.state <> $2 and `+VisibleKindRow(graphstore.KindLensKind, "catalogued", "$3")+`
		order by catalogued.id`,
		graphstore.KindLensKind, graphstore.StateGone, VisibilityArg(who))
	if err != nil {
		return nil, fmt.Errorf("read the lens catalogue: %w", err)
	}
	defer rows.Close()
	var kinds []CatalogueKind
	for rows.Next() {
		var kind CatalogueKind
		var sources []byte
		if err := rows.Scan(&kind.ID, &kind.Words, &sources); err != nil {
			return nil, fmt.Errorf("scan a kind of the lens catalogue: %w", err)
		}
		if err := json.Unmarshal(sources, &kind.Sources); err != nil {
			return nil, fmt.Errorf("read the sources of lens kind %s: %w", kind.ID, err)
		}
		kinds = append(kinds, kind)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the lens catalogue: %w", err)
	}
	return kinds, nil
}
