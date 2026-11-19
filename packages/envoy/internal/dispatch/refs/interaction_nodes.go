package refs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// loadInteractionNodes resolves interaction nodes: each one run of the command bar
// (dispatch://INFRA-1950/spec#b-r-interaction), addressed `dispatch://interaction/<uuid>` by the
// run's own uuid and written by the going POST (api's recordGoingRequest). A run stands under no
// issue and on no node, so it answers no IssueKey and no Parent, and a route of
// `/issues/{key}/nodes/{id}/…` finds none under any issue; its Project is the project it was asked
// under, none for a run asked of every project. Its text is the words of the question, which its
// row carries (graphstore.AttributeWords), so an edge at it shows the head of what was asked, cut
// at excerptRunes as every edge's excerpt is, and a read of the run's own address answers every
// word of them as the node's Text (Node, resolvedNode's wholeText), through the same predicate.
//
// A run is reserved for its author, and its kind's predicate compares the whole credential
// (credentialAuthoredKind), so a run is answered to the credential that asked it and to no other:
// the same person's cookie reads none of what their personal token asked, and a run whose author
// proves nobody is answered to nobody. One the store holds gone is absent, as a node that does
// not exist is.
//
// The loader reads no frame of a run: an edge whose far end is a run - the authored_by of every
// run a person asked, the changed_by_run of every node a run changed - carries the run's excerpt
// and no frames, since a run is kept for ever and a read of edges has no paging. Only a read of
// the run's own address answers them (Node, interactionFrames).
func loadInteractionNodes(ctx context.Context, q Queryer, who model.Actor, _ string, _ bool, ids []string, nodes map[[2]string]resolvedNode) error {
	rows, err := q.Query(ctx, `
		select id, made_by, made_at, state, visibility, project_key, coalesce(attributes->>'`+graphstore.AttributeWords+`', '')
		from graph_nodes held
		where kind = $1 and id = any($2) and state <> $3 and `+VisibleKindRow(graphstore.KindInteraction, "held", "$4"),
		graphstore.KindInteraction, ids, graphstore.StateGone, VisibilityArg(who))
	if err != nil {
		return fmt.Errorf("load interaction nodes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		node := model.GraphNode{Kind: graphstore.KindInteraction}
		var madeBy []byte
		var madeAt time.Time
		var project *string
		var words string
		if err := rows.Scan(&node.ID, &madeBy, &madeAt, &node.State, &node.Visibility, &project, &words); err != nil {
			return fmt.Errorf("scan interaction node: %w", err)
		}
		node.Ref = StoreRef(graphstore.KindInteraction, node.ID)
		if project != nil {
			node.Project = *project
		}
		if err := setMadeAndChanged(&node, madeBy, madeAt, nil, nil, storedActors); err != nil {
			return err
		}
		nodes[[2]string{graphstore.KindInteraction, node.ID}] = resolvedNode{GraphNode: node, text: words, wholeText: true}
	}
	return rows.Err()
}

// interactionFrames is the frames of the run whose uuid is id, for who, which a read of the run's
// own address answers as the node's Frames (Node): its `frame` value tuples
// (graphstore.FrameTuple), in the order of their sequence, each value the text
// graphstore.FrameValueSQL wrote, decoded with its numbers kept as written. They are read through
// the run's own predicate over its row, as the loader reads the run, so a reader the run is not
// answered to reads none of them. The stream reads the same frames from going_events, their
// projection (`GET /api/v1/going/requests/{id}/stream`, under the same whole-credential test).
func interactionFrames(ctx context.Context, q Queryer, who model.Actor, id string) ([]model.GraphFrame, error) {
	var frames *string
	if err := q.QueryRow(ctx, `
		select jsonb_agg(frame.to_id::jsonb order by (frame.to_id::jsonb ->> 'sequence')::integer, frame.seq)::text
		from graph_nodes held
		join graph_tuples frame on frame.from_kind = held.kind and frame.from_id = held.id and frame.kind = $5
			and frame.to_kind = '`+graphstore.KindValue+`' and frame.asserted
		where held.kind = $1 and held.id = $2 and held.state <> $3 and `+VisibleKindRow(graphstore.KindInteraction, "held", "$4"),
		graphstore.KindInteraction, id, graphstore.StateGone, VisibilityArg(who), graphstore.AttributeFrame).Scan(&frames); err != nil {
		return nil, fmt.Errorf("read the frames of interaction %s: %w", id, err)
	}
	if frames == nil {
		return nil, nil
	}
	var decoded []model.GraphFrame
	decoder := json.NewDecoder(strings.NewReader(*frames))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode the frames of interaction %s: %w", id, err)
	}
	return decoded, nil
}
