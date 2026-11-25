package refs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// loadInteractionNodes resolves interaction nodes: each one run of the command bar
// (spec requirement r-interaction), addressed `dispatch://interaction/<uuid>` by the
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
// and no frames, since a run holds as many frames as its answer wrote and a read of edges has no
// paging. Only a read of the run's own address answers them (Node, interactionFrames). A run is
// readable for one hour; then the sweep states its node gone (api's sweepGoingRequests), and every
// read here answers it as absent.
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

// interactionWithdrawalSequence is the sequence the stream writes the withdrawal of the run whose
// uuid is run.id at (api's goingWithdrawal): its stored ending's (graphstore.RunEndingSQL), or,
// where it has stored none, the one its ending takes when the stream stores it
// (graphstore.RunNextSequenceSQL, through api's endGoingRun).
var interactionWithdrawalSequence = `coalesce(` + graphstore.RunEndingSQL("run.id") + `, ` + graphstore.RunNextSequenceSQL("run.id") + `)`

// interactionFramesOf is the frames the run's own address answers, over its node's graph_nodes row
// held and its uuid run.id, with $5 the frame attribute, $6 the withdrawal's name and $7 its
// payload's JSON (interactionFrames): for a run whose going_requests row is gone
// (graphstore.RunRowGoneSQL), null, no frame; for a withdrawn run (graphstore.RunWithdrawnSQL),
// the withdrawal alone, its value written as graphstore.FrameValueSQL writes every frame's; for
// any other, every frame value its node holds, in sequence, and null where it holds none.
var interactionFramesOf = `case when ` + graphstore.RunRowGoneSQL("run.id") + ` then null
		when ` + graphstore.RunWithdrawnSQL("run.id") + `
		then jsonb_build_array(` + graphstore.FrameValueSQL(interactionWithdrawalSequence, "$6::text", "$7::jsonb") + `::jsonb)
		else (select jsonb_agg(frame.to_id::jsonb order by (frame.to_id::jsonb ->> 'sequence')::integer, frame.seq)
		      from graph_tuples frame
		      where frame.from_kind = held.kind and frame.from_id = held.id and frame.kind = $5
		        and frame.to_kind = '` + graphstore.KindValue + `' and frame.asserted)
		end`

// interactionFrames is the frames of the run whose uuid is id, for who, which a read of the run's
// own address answers as the node's Frames (Node): its `frame` value tuples
// (graphstore.FrameTuple), in the order of their sequence, each value the text
// graphstore.FrameValueSQL wrote, decoded with its numbers kept as written. They are read through
// the run's own predicate over its row, as the loader reads the run, so a reader the run is not
// answered to reads none of them. The stream reads the same frames from going_events, their
// projection (`GET /api/v1/going/requests/{id}/stream`, under the same whole-credential test).
//
// A withdrawn run answers what its stream answers, since this read is a second view of the frames
// the stream decides on and does not decide it again (api's goingWithdrawal): once a comment or an
// issue message whose text the run gave its model no longer stands (graphstore.RunWithdrawnSQL,
// the one test the stream reads too), the node's frames are the one frame the stream writes, the
// withdrawal (graphstore.RunWithdrawal), and none of the frames the run stored, which may quote the
// post. The withdrawal is encoded as every stored frame is (graphstore.FrameValueSQL over its name
// and its payload's JSON, graphstore.RunWithdrawalData), at the sequence of the run's stored
// ending, where the stream writes it; where the run has stored no ending yet, at the sequence the
// stream's own ending of the run would take (interactionWithdrawalSequence), and the read writes
// nothing. The row test, the verdict, the ending and the frames are read in one statement, under
// the one predicate over the run's row, so a reader the run is not answered to reads no withdrawal
// either.
//
// A run whose going_requests row is gone answers no frame (graphstore.RunRowGoneSQL). A process of
// the release before removes that row an hour after the run was asked, and leaves the node live.
// Its stream answers 404 GOING_REQUEST_NOT_FOUND for it. Its going_request_posts rows went with the
// row, so no verdict on its posts can be read (graphstore.RunWithdrawnSQL). None of the frames it
// stored, which may quote a deleted post, is answered. Its node stays live until this build's
// sweep reads it as rowless (api's goingRowlessRuns) and states it gone.
func interactionFrames(ctx context.Context, q Queryer, who model.Actor, id string) ([]model.GraphFrame, error) {
	var frames *string
	err := q.QueryRow(ctx, `
		select (`+interactionFramesOf+`)::text
		from graph_nodes held
		cross join lateral (select `+store.CanonicalUUID("held.id")+` as id) run
		where held.kind = $1 and held.id = $2 and held.state <> $3 and `+VisibleKindRow(graphstore.KindInteraction, "held", "$4"),
		graphstore.KindInteraction, id, graphstore.StateGone, VisibilityArg(who), graphstore.AttributeFrame,
		graphstore.RunWithdrawal.Name, graphstore.RunWithdrawalData).Scan(&frames)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
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
