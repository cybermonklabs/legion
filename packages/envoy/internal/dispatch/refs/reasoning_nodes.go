package refs

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// The nodes a piece of work is reasoned about with (dispatch://INFRA-1950/spec#b-r-lens,
// #b-r-constraint-risk): a lens, invoked on a node and logged under it, `dispatch://lens/<uuid>`;
// a finding of a lens, a block node whose id names its lens, `dispatch://lens/<uuid>#b-<id>`; and
// a risk under a pre-mortem, `dispatch://risk/<uuid>`. Each is a node of the store alone, with no
// projection row, so each is read from graph_nodes, through the predicate of its kind, which
// compares the author a credential proved, so a node reserved for its author reaches that author
// alone. One the store holds gone is absent, as a node that does not exist is.
//
// Each stands on another node through its current part_of tuple: a lens on the node it was
// invoked on - the issue, a document or a block of one, an ask, a comment or a message or a block
// of one, or another lens, a finding, a risk or a constraint (P9) - and a finding or a risk under
// its lens. So each is located by walking that tuple up to the first node that is none of these,
// and that node's issue is the node's own (StandingIssueSQL): FindNodeUnderIssue finds a lens, a
// finding or a risk under the issue its walk ends at, and under no other.

// standingKinds are the kinds of node that stand on another through their part_of tuple and no
// projection row, as the graph addresses each, sorted: the kinds standingText states the text of.
// The walk of StandingIssueSQL passes through every one of them to the node at the foot of the
// chain.
var standingKinds = slices.Sorted(maps.Keys(standingText))

// StandingIssueSQL is the key of the issue the node whose kind and store id are the SQL kindExpr
// and idExpr stands under, as a scalar expression, null where it stands under none. kindExpr may
// be the kind the graph addresses the node by or the kind the store keys it by: a finding of a
// lens is a lens_block to one and a block to the other (graphstore.AddressKindSQL reads it).
//
// A node of a standing kind (standingKinds) stands where its current part_of tuple says, so the
// walk follows that tuple, the newest asserted one of each node, up through every node of those
// kinds - a lens on a lens, a finding under its lens, a risk under a pre-mortem - to the first
// node of any other kind, whose issue is the answer: an issue is its own, a document's is the
// issue it belongs to, a block's its document's, an ask's, a comment's or a message's the issue
// it belongs to, and an item's block its item's. A walk that meets a node a second time stops
// there, so a chain a move closed into a loop answers no issue rather than reading forever. The
// walk reads each node's own tuples and no node's visibility: where a node stands is its own
// fact, and whether a reader may see the nodes it stands on is each of those nodes' own
// predicate's to say where a read answers them.
func StandingIssueSQL(kindExpr, idExpr string) string {
	address := graphstore.AddressKindSQL(`standing.kind`, `standing.id`)
	return `(with recursive standing (kind, id) as (
			select (` + kindExpr + `)::text, (` + idExpr + `)::text
			union
			select placed.to_kind, placed.to_id
			from standing
			cross join lateral (
				` + CurrentFarEndSQL(`newest.to_kind, newest.to_id`, storedKindSQL(`standing.kind`), `standing.id`, graphstore.TuplePartOf) + `
			) placed
			where ` + address + ` in (` + quotedKinds(standingKinds) + `)
		)
		select ` + issueOfSQL(`standing.kind`, `standing.id`) + `
		from standing
		where ` + address + ` not in (` + quotedKinds(standingKinds) + `)
		limit 1)`
}

// CurrentFarEndSQL is the read of the far end of the current tuple of kind out of the node whose
// kind and id, as the store keys them, are the SQL fromKind and fromID, for a relation that holds
// one far end at a time - a node's part_of, its authored_by - as the subquery of a lateral join:
// the newest tuple to each far end, kept while it asserts (graphstore.CurrentTupleRead), and of
// those the first by far end where the store holds several. columns is what it answers, written
// over newest: its to_kind, its to_id and its seq.
func CurrentFarEndSQL(columns, fromKind, fromID, kind string) string {
	return `select ` + columns + ` from (
					` + graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_kind, t.to_id, t.seq, t.asserted`,
		`t.from_kind = `+fromKind+` and t.from_id = `+fromID+` and t.kind = '`+kind+`'
						and t.to_kind <> '`+graphstore.KindValue+`'`) + `
				) newest
				where newest.asserted
				order by newest.to_kind, newest.to_id
				limit 1`
}

// issueOfSQL is the issue key of the node of no standing kind whose kind and store id are the
// SQL kindExpr and idExpr, as every loader of those kinds answers its IssueKey: an issue is its
// own key; a document (by its ref_key, as the store keys it) belongs to its issue; an ask, a
// comment or a message to its issue; and a block, named `<owner>#<block id>`, belongs to its
// owner's issue, the item whose kind and uuid the owner spells (`comment/<uuid>`) or else the
// document whose ref_key it is, whichever block kind names it. A kind of no node that stands
// under an issue answers null.
func issueOfSQL(kindExpr, idExpr string) string {
	owner := `split_part(` + idExpr + `, '#', 1)`
	var items, blocks strings.Builder
	for _, item := range itemKinds {
		tables, _ := item.tables()
		items.WriteString(` when '` + string(item) + `' then (select owner_item.issue_key from ` + tables.items +
			` owner_item where owner_item.id::text = ` + idExpr + `)`)
		blocks.WriteString(` when ` + owner + ` like '` + string(item) + `/%' then (select owner_item.issue_key from ` + tables.items +
			` owner_item where owner_item.id::text = split_part(` + owner + `, '/', 2))`)
	}
	return `(case ` + kindExpr + `
			when '` + graphstore.KindIssue + `' then ` + idExpr + `
			when '` + graphstore.KindArtifact + `' then (select owner_document.issue_key from artifacts owner_document where owner_document.ref_key = ` + idExpr + `)` +
		items.String() + `
			else case when ` + kindExpr + ` in ` + blockNodeKinds + ` then case` + blocks.String() + `
				else (select owner_document.issue_key from artifacts owner_document where owner_document.ref_key = ` + owner + `) end end
		end)`
}

// standingText is the SQL of the text of a node of each standing kind (standingKinds), over its
// graph_nodes row aliased alias, for the reader bound at param: the text its loader answers, which
// an edge at it shows an excerpt of, and its row on a person's page shows the first line of
// (StandingTextSQL).
//
//   - A lens's is its verdict (graphstore.AttributeVerdict), and where it holds none, the words of
//     its kind (LensKindSQL), so a lens with no verdict yet still reads as the kind of analysis it
//     is; one with neither reads empty.
//   - A finding's is the newest value of its `text` tuple (graphstore.AttributeText).
//   - A risk's is its heading (graphstore.AttributeHeading).
//   - A constraint's is its standard, or its measurement where it holds no standard.
var standingText = map[string]func(alias, param string) string{
	graphstore.KindLens: func(alias, param string) string {
		return `coalesce(nullif(` + alias + `.attributes->>'` + graphstore.AttributeVerdict + `', ''), (
			` + LensKindSQL(alias, `kind_node.attributes->>'`+graphstore.AttributeWords+`'`, param) + `), '')`
	},
	graphstore.KindLensBlock: func(alias, _ string) string {
		return `coalesce((
			select case when said.asserted then said.to_id end
			from graph_tuples said
			where said.from_kind = ` + alias + `.kind and said.from_id = ` + alias + `.id
				and said.kind = '` + graphstore.AttributeText + `' and said.to_kind = '` + graphstore.KindValue + `'
			order by said.seq desc
			limit 1), '')`
	},
	graphstore.KindRisk: func(alias, _ string) string {
		return `coalesce(` + alias + `.attributes->>'` + graphstore.AttributeHeading + `', '')`
	},
	graphstore.KindConstraint: func(alias, _ string) string {
		return `coalesce(` + alias + `.attributes->>'` + graphstore.AttributeStandard + `', ` +
			alias + `.attributes->>'` + graphstore.AttributeMeasurement + `', '')`
	},
}

// LensKindSQL is the read of the kind of the lens whose graph_nodes row alias names, for the
// reader bound at param: the lens_kind node its current has_kind tuple names, where the store
// holds that node, not gone, and the reader may see it through its kind's predicate, the first by
// id where the lens names several. It answers columns, written over that node's row, kind_node,
// as a scalar subquery or a lateral join reads it. Every read of a lens's kind is this one - the
// lens read's kind of each lens, and a lens's text where it holds no verdict (standingText) - so
// no two reads give one lens two kinds, or the words of a kind that is gone.
func LensKindSQL(alias, columns, param string) string {
	return `select ` + columns + `
			from (
				` + graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_kind, t.to_id, t.asserted`,
		`t.from_kind = `+alias+`.kind and t.from_id = `+alias+`.id and t.kind = '`+graphstore.TupleHasKind+`'
					and t.to_kind <> '`+graphstore.KindValue+`'`) + `
			) chosen
			join graph_nodes kind_node on kind_node.kind = chosen.to_kind and kind_node.id = chosen.to_id
			where chosen.asserted and chosen.to_kind = '` + graphstore.KindLensKind + `' and kind_node.state <> '` + graphstore.StateGone + `'
				and ` + VisibleKindRow(graphstore.KindLensKind, "kind_node", param) + `
			order by chosen.to_id
			limit 1`
}

// StandingTextSQL is the text of the node of a standing kind whose graph_nodes row alias names,
// for the reader bound at param, whichever of those kinds it is (standingText), read off the row's
// own kind and id (graphstore.AddressKindSQL), and null for a row of any other kind: what a read
// listing nodes of several kinds at once - a person's page - shows of each.
func StandingTextSQL(alias, param string) string {
	var cases strings.Builder
	for _, kind := range standingKinds {
		cases.WriteString(` when '` + kind + `' then ` + standingText[kind](alias, param))
	}
	return `(case ` + graphstore.AddressKindSQL(alias+`.kind`, alias+`.id`) + cases.String() + ` end)`
}

// StandingKinds is every kind of node that stands on another through its part_of tuple alone, as
// the graph addresses each (standingKinds): a constraint, a lens, a finding of a lens and a risk.
func StandingKinds() []string { return slices.Clone(standingKinds) }

// StoreRef is the `dispatch://` address of a node the store addresses by its own id alone - an
// interaction, a lens, a finding of a lens, a risk, a constraint, a kind of the lens catalogue -
// from its kind, as the graph addresses it, and its store id: `dispatch://<kind>/<id>`, and for a
// finding, whose id `lens/<uuid>#<block id>` names its lens, its lens's address with the block id
// as a document block's reference writes one (`dispatch://lens/<uuid>#b-<id>`).
func StoreRef(kind, id string) string {
	if kind == graphstore.KindLensBlock {
		lens, block, _ := strings.Cut(id, "#")
		return BlockRef("dispatch://"+lens, block)
	}
	return "dispatch://" + kind + "/" + id
}

// standingAddressKind is the kind the graph addresses a node by whose kind and id a part_of tuple
// names, as the registry keys its loader: the kind itself, or for a block, the block kind of the
// lens or the item its id names (graphstore.AddressKind, ParseItemBlockNode), so a writer that
// named an item's block or a finding as a block still reaches the node's own loader.
func standingAddressKind(kind, id string) string {
	if kind != BlockNodeKind {
		return kind
	}
	if addressed := graphstore.AddressKind(kind, id); addressed != kind {
		return addressed
	}
	if item, _, _, ok := ParseItemBlockNode(id); ok {
		return item.BlockKind()
	}
	return kind
}

// placedNode is one node read by loadReasoningNodes, with the far end of its current part_of
// tuple and whether the reader may see that end.
type placedNode struct {
	node                 resolvedNode
	parentKind, parentID string
	parentSeen           bool
}

// loadReasoningNodes resolves the nodes of kind - lenses, the findings of lenses (lens_block) or
// risks - whose store ids are ids, for who, in one statement over graph_nodes, under the kind the
// store keys them by (StoredKindOf): each row through its kind's predicate over the row the read
// holds (VisibleKindRow), with its state and visibility, its project, its text (standingText), the
// far end of its current part_of tuple with that end's own predicate (VisibleNode), so a node
// standing on one the reader may not see names no parent, and the issue it stands under. That is
// the issue its parent stands under or belongs to, so the walk of StandingIssueSQL starts at that
// far end, the row placed has already read, rather than reading the node's part_of a second time.
// Its address is the store's (StoreRef).
//
// A finding is stored as a block, and of the store's blocks only one whose id names its lens,
// `lens/<uuid>#<block id>`, is one. A finding is no block of any document, so it answers no
// Document and no version; its BlockID is its own block id, and its text is cut as a document
// block's stored excerpt is (BlockExcerptRunes), which an edge at it shows whole.
//
// Its parent is that far end, as every loader's is the node it belongs to. A parent of a standing
// kind is named by its kind, its store id and its address; any other parent is resolved through its
// own kind's loader, in one more read for the whole batch (resolveNodes), so it carries the id and
// the address that loader answers - an artifact's uuid, a block's node id - and a parent no loader
// answers, a block no version holds now, names none.
func loadReasoningNodes(ctx context.Context, q Queryer, who model.Actor, kind string, _ bool, ids []string, nodes map[[2]string]resolvedNode) error {
	where := `held.kind = $1 and held.id = any($2) and held.state <> $3`
	if kind == graphstore.KindLensBlock {
		where += ` and held.id like '` + graphstore.KindLens + `/%#%'`
	}
	rows, err := q.Query(ctx, `
		select held.id, held.made_by, held.made_at, state, visibility, held.project_key, `+standingText[kind]("held", "$4")+`,
		       placed.to_kind, placed.to_id, placed.seen, `+StandingIssueSQL(`placed.to_kind`, `placed.to_id`)+`
		from graph_nodes held
		left join lateral (
			`+CurrentFarEndSQL(`newest.to_kind, newest.to_id, `+VisibleNode(`newest.to_kind`, `newest.to_id`, `$4`)+` as seen`,
		`held.kind`, `held.id`, graphstore.TuplePartOf)+`
		) placed on true
		where `+where+` and `+VisibleKindRow(kind, "held", "$4"),
		StoredKindOf(kind), ids, graphstore.StateGone, VisibilityArg(who))
	if err != nil {
		return fmt.Errorf("load %s nodes: %w", kind, err)
	}
	var placed []placedNode
	for rows.Next() {
		node := model.GraphNode{Kind: kind}
		var madeBy []byte
		var madeAt time.Time
		var project, parentKind, parentID *string
		var parentSeen *bool
		var body string
		if err := rows.Scan(&node.ID, &madeBy, &madeAt, &node.State, &node.Visibility, &project, &body,
			&parentKind, &parentID, &parentSeen, &node.IssueKey); err != nil {
			rows.Close()
			return fmt.Errorf("scan %s node: %w", kind, err)
		}
		node.Ref = StoreRef(kind, node.ID)
		if project != nil {
			node.Project = *project
		}
		if kind == graphstore.KindLensBlock {
			_, node.BlockID, _ = strings.Cut(node.ID, "#")
			body = text.HeadRunes(body, BlockExcerptRunes)
		}
		if err := setMadeAndChanged(&node, madeBy, madeAt, nil, nil, storedActors); err != nil {
			rows.Close()
			return err
		}
		one := placedNode{node: resolvedNode{GraphNode: node, text: body}}
		if parentKind != nil && parentID != nil && parentSeen != nil && *parentSeen {
			one.parentKind, one.parentID, one.parentSeen = standingAddressKind(*parentKind, *parentID), *parentID, true
		}
		placed = append(placed, one)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("load %s nodes: %w", kind, err)
	}
	return placeReasoningNodes(ctx, q, who, placed, nodes)
}

// placeReasoningNodes gives each node its parent, as loadReasoningNodes says, and adds it to nodes.
func placeReasoningNodes(ctx context.Context, q Queryer, who model.Actor, placed []placedNode, nodes map[[2]string]resolvedNode) error {
	var lookups []edgeRow
	for _, one := range placed {
		if one.parentSeen && !slices.Contains(standingKinds, one.parentKind) {
			lookups = append(lookups, edgeRow{otherKind: one.parentKind, otherID: one.parentID})
		}
	}
	var parents map[[2]string]resolvedNode
	if len(lookups) > 0 {
		var err error
		if parents, err = resolveNodes(ctx, q, who, true, lookups); err != nil {
			return err
		}
	}
	for _, one := range placed {
		node := one.node
		switch {
		case !one.parentSeen:
		case slices.Contains(standingKinds, one.parentKind):
			node.Parent = &model.NodeParent{Kind: one.parentKind, ID: one.parentID, Ref: StoreRef(one.parentKind, one.parentID)}
		default:
			if parent, found := parents[[2]string{one.parentKind, one.parentID}]; found {
				node.Parent = &model.NodeParent{Kind: parent.Kind, ID: parent.ID, Ref: parent.Ref}
			}
		}
		nodes[[2]string{node.Kind, node.ID}] = node
	}
	return nil
}
