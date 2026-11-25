package api

// node_state.go serves the node state (spec requirements r-proposed and r-state-closed):
// one write of a node's state (POST /api/v1/issues/{key}/nodes/{id}/state), the settle of a
// proposed node (POST /api/v1/issues/{key}/nodes/{id}/settle), and the proposed nodes of an issue
// (GET /api/v1/issues/{key}/proposed).
//
// The node state takes five values (graphstore.NodeStates) and no other: a word that is none of
// them is graphstore's 400 INVALID_ATTRIBUTE_VALUE, refused before anything is read or stored. The
// state route takes proposed, overtaken and live on any node the store holds that is not gone but
// the block of an `:::ask` block, and gone on a node that belongs to no document alone - a lens, a
// finding, a risk or a constraint - because a document's own write removes its block. gone is
// final: every write on a gone node is 409 NODE_GONE, naming it (nodeGone), the state route's and
// the settle's, and the reasoning routes' write of a lens, a finding, a risk or a pointer on it
// (findWrittenNode). The write that takes a node to gone takes its whole part_of subtree to gone
// with it, at every depth and of every kind, those nodes the caller may not see among them
// (writeNodeState, spec requirement r-proposed), so that move is a person's alone on a lens, a
// finding, a risk and a constraint alike, whatever stands under the node (goneNotPermitted).
// settled goes through the settle alone, which a person makes on any proposal and any other caller
// on a proposal its own credential wrote (maySettle). The state route holds every other move of a
// node into or out of proposed or settled to that one rule too, and refuses any other caller 403
// STATE_NOT_PERMITTED: a move out of settled undoes what a settle decided, a move out of proposed
// ends a proposal and counts a proposed check or plan task as serving its requirement as a settle
// would, and a move into proposed takes a requirement's gaps out of the coverage read and the
// approval request. Both routes share one body (writeNodeStateAs), which records one writer for
// one person, and write through writeNodeState, at graphstore.NoEvent: no event is appended and no
// delivery is sent, as for every node of these kinds (reasoning_write.go), and the reads answer
// the new state at once, since every one of them reads the node's row.
//
// On an issue Legion runs, only a person or an architect of the issue or of an issue above it
// writes the state of a requirement, criterion or check node: each route holds such a write to the
// check of who writes one (refuseSpecWriterOfNode, spec_writer.go), past its own rule and under the
// issue's owner lock, and refuses any other writer 403 DOCUMENT_WRITER_NOT_ARCHITECT.
//
// A caller who is no person (docs.CallerIsPerson) sets no person's decision (docs.PersonDecisionOf)
// overtaken or live (R32 of the issue tree's spec): neither an answered ask's own node nor a
// decision a person decided, so an agent neither marks a person's decision overtaken nor undoes a
// person's own overtaken. The refusal is 409 ANSWERED_ASK_KEPT, quoting the ask's question or the
// decision's words and naming the node, and nothing is stored (refusePersonDecisionState). The
// route's other refusals come first and keep their codes. On an answered ask's own node,
// `ask/<uuid>`, the refusal comes before the settle's author rule and covers proposed too: that
// node's author is the agent that asked, whom the author rule would let through, and its state is
// the person's answer's. The `:::ask` block of an answered ask is 409 ASK_HOLDS_STATE first, as
// every write on such a block is.
//
// A block of an ask, a comment or a message is a node every read answers, and no node the store
// holds: no writer, the backfill among them, writes a graph_nodes row for one, so every read
// answers it live (refs.StoredStateAndVisibility). The state route refuses it every value, naming
// why (itemBlockRefusal), and the settle refuses it as a node that is not proposed.
//
// A document's `:::ask` block is a node (`<ref_key>#<block id>`, dispatch://<KEY>/spec#b-<block id>
// for the spec's). It marks the question's place in its document and is not a block of the ask's
// own text. Only the ask node (`ask/<uuid>`, dispatch://<KEY>/ask/<uuid>) holds the question's state.
// Both routes refuse a write on the block with 409 ASK_HOLDS_STATE and write nothing
// (askHoldsState). The refusal names the ask that holds the question's state: the block's own
// ask, or the source ask for a copy. If it has neither, the refusal says no ask holds the
// question yet. A write by the ask's own address writes the ask's node as it writes any other.
//
// `{id}` is the node's store address, one percent-encoded path segment that FindNodeUnderIssue
// decodes (node_address.go). A node the caller may not see, a node of another issue and an address
// that names nothing all answer that function's one 404 NOT_FOUND. An interaction stands under no
// issue, so neither route finds one, and a run's state is written by the going path alone.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// Codes of the two routes' own refusals, and of a write on a gone node.
const (
	// codeStateNotWritable is a value of the node state the state route does not write on the
	// node it names: settled anywhere, gone on a node of a document, and any value on a node the
	// store holds no row of.
	codeStateNotWritable = "NODE_STATE_NOT_WRITABLE"
	// codeNodeGone is a write on a node the store holds gone: no write moves such a node, settles
	// it, or writes a lens, a finding, a risk or a pointer on it while it is gone.
	codeNodeGone = "NODE_GONE"
	// codeNotProposed is a settle of a node that is not at proposed.
	codeNotProposed = "NODE_NOT_PROPOSED"
	// codeSettleNotPermitted is a settle by a caller who is neither a person nor the proposal's
	// own author.
	codeSettleNotPermitted = "SETTLE_NOT_PERMITTED"
	// codeStateNotPermitted is a move of a lens, a finding, a risk or a constraint into gone
	// through the state route by a caller who is no person (goneNotPermitted), and any other move
	// of a node into or out of proposed or settled by a caller who is neither a person nor the
	// node's own author (maySettle).
	codeStateNotPermitted = "STATE_NOT_PERMITTED"
	// codeAskHoldsState is a write of any value, the settle's among them, on the block node of an
	// `:::ask` block of a document: the block is only the question's place in its document, and
	// the question's state is its ask node's alone (askHoldsState), so the write is refused.
	codeAskHoldsState = "ASK_HOLDS_STATE"
)

// nodeGone is the 409 NODE_GONE of a write on the node of kind kind that address names, which
// the store holds gone, in words that hold for every node it answers: no write writes on it, and
// for a lens, a finding, a risk or a constraint (refs.StandingKinds), whose gone no write undoes,
// gone is final. A block of a document a version dropped is gone too, and a later version that
// holds the block again restores it, so its refusal says nothing of finality.
func nodeGone(kind, address string) error {
	if slices.Contains(refs.StandingKinds(), kind) {
		return errorf(http.StatusConflict, codeNodeGone, "the %s %s is gone, and gone is final: no write writes on it", kind, address)
	}
	return errorf(http.StatusConflict, codeNodeGone, "the %s %s is gone: no write writes on it", kind, address)
}

// goneNotPermitted is the 403 STATE_NOT_PERMITTED of a move of target - a lens, a finding, a risk
// or a constraint, the nodes the state route takes to gone (stateRefusal) - into gone by a caller
// who is no person (spec requirement r-proposed). That move takes the node's whole part_of subtree
// to gone with it, those nodes the caller may not see among them, so it is a person's alone: a
// rule that read what stands under the node would tell the caller what stands there. So the
// refusal reads nothing under the node and names the node alone, in the same words whatever stands
// under it, and the node's own author is refused it as every other caller is; an agent marks a
// node it wrote overtaken instead.
func goneNotPermitted(target stateTarget) error {
	return errorf(http.StatusForbidden, codeStateNotPermitted,
		"this credential may not move the %s %s to gone: a person alone takes a lens, a finding, a risk or a constraint to gone, with everything that stands under it, and its author marks it overtaken instead",
		target.kind, target.address)
}

// itemBlockRefusal is the 409 NODE_STATE_NOT_WRITABLE of a write of asked on target, where target
// is a block of an ask, a comment or a message, which a tuple names by its item's block kind
// (refs.ItemKind.BlockKind, farEndOf) and the store holds no row of, so every read answers it
// live; nil for any other node. With no row there is nothing for the write to move.
func itemBlockRefusal(target stateTarget, asked string) error {
	item, _, _, isItemBlock := refs.ParseItemBlockNode(target.end.id)
	if !isItemBlock || item.BlockKind() != target.end.kind {
		return nil
	}
	return errorf(http.StatusConflict, codeStateNotWritable,
		"the %s %s is %s and takes no %s: the store keeps no node of a block of an ask, a comment or a message, so every read answers such a block live and no write stores its state",
		target.kind, target.address, graphstore.StateLive, asked)
}

// stateTarget is the node a write of its state names: its kind as the graph addresses it, the
// address a refusal names it by, and the kind and the id the store keys it by.
type stateTarget struct {
	kind, address string
	end           storedEnd
}

// askHoldsState refuses a write on the block node of an `:::ask` block with 409 ASK_HOLDS_STATE
// and returns nil for other nodes. It reads in the write's transaction (q) only for nodes the
// graph addresses as blocks (graphstore.AddressKind), not lens findings, which are also stored
// as blocks. The refusal names the block and the ask that holds the question's state: the
// block's own ask, or the source ask for a copy (docs.AskOfBlockSQL). Both are under the route's
// issue key: findWrittenNode checks the block's issue and settlement takes copy sources from
// the same owner's documents (docs' copiedAskSourcesQuery). If there is no ask yet, the refusal
// says so. In either case the write stores nothing.
func askHoldsState(ctx context.Context, q refs.Queryer, key string, target stateTarget) error {
	if graphstore.AddressKind(target.end.kind, target.end.id) != refs.BlockNodeKind {
		return nil
	}
	var askID *string
	err := q.QueryRow(ctx, docs.AskOfBlockSQL, target.end.kind, target.end.id).Scan(&askID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the ask of the block %s %s: %w", target.end.kind, target.end.id, err)
	}
	if askID == nil {
		return errorf(http.StatusConflict, codeAskHoldsState,
			"the %s %s is an :::ask block, and an ask holds the question's state, not the block: no ask holds this question yet, so nothing was written",
			target.kind, target.address)
	}
	ask := refs.AskRef(text.Ref{IssueKey: key, ID: *askID})
	return errorf(http.StatusConflict, codeAskHoldsState,
		"the %s %s is an :::ask block: the ask %s holds the question's state. Write the state on the ask. Nothing was written.",
		target.kind, target.address, ask)
}

// goneNodeUnderIssueQuery is whether the store holds the node whose stored kind and id are $1 and
// $2 as one every read answers as gone - gone, or a stale node of a standing kind - standing under
// the issue $3 (refs.GoneStandingIssueSQL), for a reader ($4) the node's own kind's predicate
// admits but for that (refs.VisibleGoneRow). A node whose walk ends at a removed document, whose
// row is deleted, stands under the issue the document's address spells: a lens on it and every
// node under it, and the document's own node.
var goneNodeUnderIssueQuery = `
	select true from graph_nodes held
	where held.kind = $1 and held.id = $2
	  and ` + refs.GoneStandingIssueSQL(`held.kind`, `held.id`) + ` = $3
	  and ` + refs.VisibleGoneRow("held", "$4")

// findWrittenNode is the node a write is made on, which id names under the issue key, read for
// who: the node whose state the write moves, or the node a lens is invoked on, a finding is added
// to, a risk is written under or a pointer goes out of. It is FindNodeUnderIssue's node, with the
// end a tuple names it by (findEndUnderIssue). Where that function answers the node does not exist
// and the store holds it gone, or stale (refs.StaleSQL), under the issue for a reader who may see
// it, which every read of the graph leaves out, the write is 409 NODE_GONE, naming the node as id
// names it (nodeGone): gone is final, so no write is made on a gone node, and a stale node and
// everything under it are answered as gone ones. That holds of a lens on a removed document and
// of everything under it too, whichever build removed the document, since the test reads the
// issue a removed document's address spells (goneNodeUnderIssueQuery). Anything else is
// FindNodeUnderIssue's own 404, word for word, so a refusal tells nothing of a node the caller may
// not see, gone or not. A far end of a tuple a write appends - the lens a lens is triggered by, a
// pointer's target - is found through findEndUnderIssue or pointerEnd instead, so a gone one keeps
// that 404.
func findWrittenNode(ctx context.Context, q refs.Queryer, who model.Actor, key, id string) (model.GraphNode, storedEnd, error) {
	node, end, err := findEndUnderIssue(ctx, q, who, key, id)
	var refusal *apiError
	if err == nil || !errors.As(err, &refusal) || refusal.status != http.StatusNotFound {
		return node, end, err
	}
	kind, address, ok := decodeNodeAddress(id)
	if !ok {
		return model.GraphNode{}, storedEnd{}, err
	}
	stored := storedEnd{kind: kind, id: address}.stored()
	var gone bool
	readErr := q.QueryRow(ctx, goneNodeUnderIssueQuery, stored.kind, stored.id, key, refs.VisibilityArg(who)).Scan(&gone)
	if errors.Is(readErr, pgx.ErrNoRows) {
		return model.GraphNode{}, storedEnd{}, err
	}
	if readErr != nil {
		return model.GraphNode{}, storedEnd{}, fmt.Errorf("read whether %s %s is gone under %s: %w", stored.kind, stored.id, key, readErr)
	}
	return model.GraphNode{}, storedEnd{}, nodeGone(kind, id)
}

// nodeStateWritten is what one write of a node's state did: the node as the write leaves it - its
// address, its kind, the state it holds now and its visibility - and the state it held before,
// which is its state still where the write found it there and wrote nothing.
type nodeStateWritten struct {
	Node     reasoningEnd `json:"node"`
	Previous string       `json:"previous" enum:"live,gone,proposed,settled,overtaken"`
}

// stateWritten is the answer for a write of target's state that left the store's node as node and
// found it at previous.
func stateWritten(target stateTarget, node graphstore.Node, previous string) nodeStateWritten {
	return nodeStateWritten{
		Node:     reasoningEnd{Address: target.address, Kind: target.kind, State: node.State, Visibility: node.Visibility},
		Previous: previous,
	}
}

// --- the one body of both routes ----------------------------------------------------------------

// nodeStateRule is what tells the two routes of a node's state apart, past the refusals
// writeNodeState makes of every write: refused is the route's rule over the node as the store
// holds it, for the credential proving that proved the caller, nil where the route writes the
// value, read in the write's transaction q under the issue's owner lock; and unheld is its refusal
// of a node the store holds no row of (errNodeNotHeld), which every read answers live.
type nodeStateRule struct {
	refused func(ctx context.Context, q queryer, target stateTarget, stored graphstore.Node, proving model.Actor) error
	unheld  func(target stateTarget) error
}

// nodeWriterCheck is the check of who writes the node a write of its state names, which each route
// passes in its own call of writeNodeStateAs (refuseSpecWriterOfNode): the source guard reads that
// call, so it holds each route to the check it names (spec_writer_route_guard_test.go).
type nodeWriterCheck func(ctx context.Context, q queryer, writer specWriter, key string, end storedEnd, address string) error

// writeNodeStateAs is the one body of POST /api/v1/issues/{key}/nodes/{id}/state and of its
// settle: one write of asked on the node {id} names, by the caller, under rule. The caller is
// resolved once, and its writer by the one rule of both routes: a person - a browser's cookie, or a
// token holding the person scope - as the person it acts for (actingPerson), whichever of her
// credentials made the write, and any other caller as the actor it records (actorFrom), so one
// person's move of a node into proposed and her settle of it name one writer. The value is held to
// the five the node state takes (graphstore.CheckAttributeValue), so a word that is none of them is
// 400 INVALID_ATTRIBUTE_VALUE, naming the attribute and the word, and nothing is read or stored.
// The node is found under the issue for the caller (findWrittenNode): 404 NOT_FOUND for a node the
// caller may not see, and 409 NODE_GONE for a gone one. The block node of an `:::ask` block is then
// 409 ASK_HOLDS_STATE, naming the ask whose node holds the question's state, or saying no ask
// holds it yet (askHoldsState). The check runs in the same transaction and before anything is
// written, so the refusal stores no tuple and no row. A write by the ask's own address goes
// through writeNodeState as on any other node, and the answer includes its state before and
// after the write. Past the route's rule, under the issue's owner lock writeNodeState takes, the
// write is held to refuseWriter, the check of who writes a requirement, criterion or check node:
// 403 DOCUMENT_WRITER_NOT_ARCHITECT on such a node of an issue Legion runs from a writer who is no
// person and no architect of the issue or of an issue above it, and nothing is stored.
func (s *server) writeNodeStateAs(w http.ResponseWriter, r *http.Request, supplied *model.Actor, asked string, rule nodeStateRule, refuseWriter nodeWriterCheck) {
	proving, resolved, ok := s.resolveCaller(w, r)
	if !ok {
		return
	}
	var writer model.Actor
	if docs.CallerIsPerson(proving) {
		writer, ok = s.actingPerson(w, resolved)
	} else {
		writer, ok = s.requireActor(w, resolved, supplied)
	}
	if !ok {
		return
	}
	if err := graphstore.CheckAttributeValue(graphstore.AttributeState, asked); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	ctx, key, id := r.Context(), r.PathValue("key"), r.PathValue("id")
	tx, err := s.begin(ctx)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	at, err := transactionNow(ctx, tx)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	found, end, err := findWrittenNode(ctx, tx, proving.Recorded(), key, id)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	target := stateTarget{kind: found.Kind, address: cmp.Or(found.Ref, id), end: end.stored()}
	if err := askHoldsState(ctx, tx, key, target); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	checked := specWriter{actor: writer, person: docs.CallerIsPerson(proving)}
	node, previous, err := writeNodeState(ctx, tx, nodeStateWrite{
		issue: key, target: target, state: asked, who: writer, at: at,
		allowed: func(stored graphstore.Node) error {
			if err := rule.refused(ctx, tx, target, stored, proving); err != nil {
				return err
			}
			return refuseWriter(ctx, tx, checked, key, target.end, target.address)
		},
	})
	if errors.Is(err, errNodeNotHeld) {
		err = rule.unheld(target)
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, stateWritten(target, node, previous))
}

// --- POST /api/v1/issues/{key}/nodes/{id}/state -------------------------------------------------

// nodeStateInput is the body of one write of a node's state.
type nodeStateInput struct {
	Actor *model.Actor `json:"actor"`
	// State is the value the node's state is written at: proposed, overtaken or live on any node
	// the store holds that is not gone, but not on the block of an `:::ask` block of a document.
	// Only a person can write gone, on a lens, a finding, a risk or a constraint, taking the node's
	// whole part_of subtree with it. The settle alone writes settled. No other word is a state.
	State string `json:"state" required:"true" enum:"live,gone,proposed,overtaken"`
}

// stateRefusal is the 409 NODE_STATE_NOT_WRITABLE of a write of asked on target, which the store
// holds at held, naming the node's kind, its state and the value asked for; nil where the route
// writes it.
func stateRefusal(target stateTarget, held, asked string) error {
	refused := func(why string) error {
		return errorf(http.StatusConflict, codeStateNotWritable, "the %s %s is %s and takes no %s here: %s",
			target.kind, target.address, held, asked, why)
	}
	switch {
	case asked == graphstore.StateSettled:
		return refused("a proposed node is settled by POST /api/v1/issues/{key}/nodes/{id}/settle alone")
	case asked == graphstore.StateGone && !slices.Contains(refs.StandingKinds(), target.kind):
		return refused("a document's own write removes its block and its own routes take out an issue, a document, an ask, " +
			"a comment or a message, so this route writes gone only on a lens, a finding, a risk or a constraint")
	}
	return nil
}

// writeNodeStateRoute is POST /api/v1/issues/{key}/nodes/{id}/state: one write of the state of the
// node {id} names, by the caller (writeNodeStateAs). A node the caller may not see under the issue
// is 404 NOT_FOUND, and a gone node 409 NODE_GONE: gone is final, since proposed, overtaken or live
// would bring back a node its writer took out, and a block of a document a version dropped would
// then read as a present block no version holds. The block node of an `:::ask` block is 409
// ASK_HOLDS_STATE whatever the value, naming the block's own ask or the source ask for a copy,
// or saying no ask holds the question yet (askHoldsState). Nothing is written. The node's kind
// then decides for every other node (stateRefusal): the route writes proposed, overtaken and live
// on any other node the store holds, and gone on a lens, a finding, a risk or a constraint alone,
// and refuses settled and gone on any other node
// with 409 NODE_STATE_NOT_WRITABLE. A node the store holds no row of is the same 409: a block of
// an ask, a comment or a message, of which the store keeps none (itemBlockRefusal), and a node
// written before the store existed that the backfill has not reached yet. A move into gone, which
// the route makes on a lens, a finding, a risk or a constraint alone, is a person's alone
// (goneNotPermitted), read off the credential and nothing under the node, since it takes the
// node's whole part_of subtree with it. On an answered ask's own node, `ask/<uuid>`, a caller who
// is no person is then refused proposed, overtaken and live alike, 409 ANSWERED_ASK_KEPT
// (refusePersonDecisionState, R32): the ask's author is the agent that asked, whom the settle's
// author rule would let through, and the node's state is the person's answer's, so this refusal
// comes before that rule. Any other move of the node into or out of proposed or settled
// (takesTheSettleRule) then takes the settle's caller rule over the node's made_by (maySettle).
// Any other caller is 403 STATE_NOT_PERMITTED, naming the node, and nothing is stored, so the
// shared bearer, which proves no author, makes no such move. A caller who is no person then sets
// no decision a person decided overtaken or live, 409 ANSWERED_ASK_KEPT (refusePersonDecisionState,
// R32), so an agent neither marks a person's decision overtaken nor undoes her own overtaken. On a
// requirement, criterion or check node of an issue Legion runs, a writer who is no person and no
// architect of the issue or of an issue above it is then 403 DOCUMENT_WRITER_NOT_ARCHITECT
// (refuseSpecWriterOfNode), and nothing is stored. The write appends one state tuple, written by
// the caller, and a gone one on each node of the subtree it takes with it too (writeNodeState); a
// write of the state the node holds appends none.
func (s *server) writeNodeStateRoute(w http.ResponseWriter, r *http.Request) {
	var input nodeStateInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.writeNodeStateAs(w, r, input.Actor, input.State, nodeStateRule{
		refused: func(ctx context.Context, q queryer, target stateTarget, stored graphstore.Node, proving model.Actor) error {
			if err := stateRefusal(target, stored.State, input.State); err != nil {
				return err
			}
			person := docs.CallerIsPerson(proving)
			if input.State == graphstore.StateGone {
				if !person {
					return goneNotPermitted(target)
				}
				return nil
			}
			askNode := target.end.kind == graphstore.KindAsk
			if askNode && !person {
				if err := refusePersonDecisionState(ctx, q, target, input.State); err != nil {
					return err
				}
			}
			if takesTheSettleRule(stored.State, input.State) && !maySettle(proving, stored.MadeBy) {
				return errorf(http.StatusForbidden, codeStateNotPermitted,
					"this credential may not move the %s %s from %s to %s: a person makes any move into or out of proposed or settled, and any other caller only on a node whose author is the author this credential proves",
					target.kind, target.address, stored.State, input.State)
			}
			if !askNode && !person && (input.State == graphstore.StateOvertaken || input.State == graphstore.StateLive) {
				return refusePersonDecisionState(ctx, q, target, input.State)
			}
			return nil
		},
		unheld: func(target stateTarget) error {
			if err := stateRefusal(target, graphstore.StateLive, input.State); err != nil {
				return err
			}
			if err := itemBlockRefusal(target, input.State); err != nil {
				return err
			}
			return errorf(http.StatusConflict, codeStateNotWritable,
				"the %s %s is %s and takes no %s yet: the store holds no row of it, which envoy-dispatch backfill-graph-store writes",
				target.kind, target.address, graphstore.StateLive, input.State)
		},
	}, refuseSpecWriterOfNode)
}

// refusePersonDecisionState is R32's refusal of a write of asked on target, by a caller who is no
// person, where target is a person's decision (docs.PersonDecisionOf): an answered ask's own node or
// a decision a person decided. It is 409 ANSWERED_ASK_KEPT, quoting the ask's question or the
// decision's words and naming the node, and nil for any other node. The caller may see the node,
// since findWrittenNode found it for them, so the refusal names it. It reads in the write's
// transaction q, under the issue's owner lock, before anything is written.
func refusePersonDecisionState(ctx context.Context, q queryer, target stateTarget, asked string) error {
	decision, held, err := docs.PersonDecisionOf(ctx, q, target.end.kind, target.end.id)
	if err != nil || !held {
		return err
	}
	return errorf(http.StatusConflict, codeAnsweredAskKept,
		"%s %s, %q, is a person's decision, and an agent cannot set its state to %s: a person sets it, or an agent acting under a token that holds the person scope. Nothing was written.",
		decision.What, target.address, decision.Words, asked)
}

// takesTheSettleRule is whether a write of asked on a node the store holds at held is a move the
// settle's caller rule decides (maySettle): a move of the node into or out of proposed or settled,
// other than the settle itself, which this route never writes (stateRefusal). A move into gone,
// out of proposed or settled or not, never reaches this rule: the route holds it to a person alone
// first (goneNotPermitted). A move out of settled undoes what a settle decided; a move out of
// proposed ends a proposal, and counts a proposed check or plan task as serving its requirement as
// a settle would; and a move into proposed takes a requirement's gaps out of the coverage read and
// the approval request. A write of the state the node already holds is none of these: it moves
// nothing and appends no tuple (writeNodeState), so it changes no decision and no count.
func takesTheSettleRule(held, asked string) bool {
	return held != asked && (held == graphstore.StateProposed || held == graphstore.StateSettled ||
		asked == graphstore.StateProposed || asked == graphstore.StateSettled)
}

// --- POST /api/v1/issues/{key}/nodes/{id}/settle ------------------------------------------------

// maySettle is whether the caller whose credential is proving settles a proposal whose made_by is
// author (spec requirement r-proposed): a person settles any proposal - a browser's
// cookie, or a token holding the person scope, the credential actingPerson acts for a person on -
// and any other caller only a proposal whose recorded author is the author its credential proves,
// by the root's one author rule (graphstore.SameAuthor, the Go half of graphstore.AuthorIsSQL,
// which every read binds): the same person for a personal token, the same session for a session
// the broker verified, and the same workload for a pod's service-account token, whose write the
// rule reads as that workload's whether it reserved the node for itself or wrote it project-visible
// under a session its body named. No rule here reads a record any other way. The deployment's
// shared bearer proves no author (graphstore.ProvesAuthor), and every holder of it is one
// credential, so it settles nothing. The state route holds each move takesTheSettleRule names to
// this same rule, so the callers who may settle a node are the callers who may move it into or
// out of proposed or settled.
func maySettle(proving, author model.Actor) bool {
	return docs.CallerIsPerson(proving) || graphstore.SameAuthor(author, proving)
}

// settleNode is POST /api/v1/issues/{key}/nodes/{id}/settle: the one write that settles a proposed
// node, appending one state tuple at settled written by the caller (writeNodeStateAs). A node the
// caller may not see under the issue is 404 NOT_FOUND, the same words a node nobody wrote answers;
// a gone node is 409 NODE_GONE; the block node of an `:::ask` block is 409 ASK_HOLDS_STATE, naming
// the ask whose node holds the question's state (its own or the source of a copy), or saying no
// ask holds it yet, and writing nothing (askHoldsState); any other node not at proposed, a node
// the store holds no row of among them, which every read answers live, is 409 NODE_NOT_PROPOSED;
// and a caller who is neither a person nor the proposal's own author (maySettle) is 403
// SETTLE_NOT_PERMITTED. A node's author is its made_by: for a block of a document the writer of
// the version that first held the block, not whoever marked it proposed. On a requirement,
// criterion or check node of an issue Legion runs, a writer who is no person and no architect of
// the issue or of an issue above it is then 403 DOCUMENT_WRITER_NOT_ARCHITECT
// (refuseSpecWriterOfNode), its own proposal among them, and nothing is stored.
func (s *server) settleNode(w http.ResponseWriter, r *http.Request) {
	var input actorInput
	if !s.decodeOptionalJSON(w, r, &input) {
		return
	}
	notProposed := func(target stateTarget, held string) error {
		return errorf(http.StatusConflict, codeNotProposed, "the %s %s is %s, and only a proposed node is settled",
			target.kind, target.address, held)
	}
	s.writeNodeStateAs(w, r, input.Actor, graphstore.StateSettled, nodeStateRule{
		refused: func(_ context.Context, _ queryer, target stateTarget, stored graphstore.Node, proving model.Actor) error {
			if stored.State != graphstore.StateProposed {
				return notProposed(target, stored.State)
			}
			if !maySettle(proving, stored.MadeBy) {
				return errorf(http.StatusForbidden, codeSettleNotPermitted,
					"this credential may not settle the %s %s: a person settles any proposal, and any other caller only a proposal whose author is the author this credential proves",
					target.kind, target.address)
			}
			return nil
		},
		unheld: func(target stateTarget) error { return notProposed(target, graphstore.StateLive) },
	}, refuseSpecWriterOfNode)
}

// --- GET /api/v1/issues/{key}/proposed ----------------------------------------------------------

// proposedNode is one proposed node of an issue: the node as every read of the graph answers it -
// its kind, its address, its state and its visibility among the rest - and the excerpt an edge at
// it shows, a block's stored excerpt or the head of the node's own text, null where it has none.
type proposedNode struct {
	Node    model.GraphNode     `json:"node"`
	Excerpt *model.GraphExcerpt `json:"excerpt"`
}

// issueProposed is the nodes under one issue whose state is proposed.
type issueProposed struct {
	Issue string         `json:"issue"`
	Nodes []proposedNode `json:"nodes"`
}

// issueProposedQuery reads the store's nodes at proposed that stand under the issue $1, for a
// reader ($2), in the order of their kind and their id. A node under the issue is one whose
// standing issue is $1 (refs.StandingIssueSQL): the issue itself, a document of the issue and each
// of its blocks, an ask, a comment or a message of the issue and each of their blocks, and the
// lenses, findings, risks and constraints whose part_of walk ends at one of those. The state is
// the literal `proposed`, the predicate of the partial index graph_nodes_proposed
// (20261116172615_graph_nodes_proposed_index.up.sql), so the read takes the few proposed rows the
// store holds through that index rather than every node of the store; the walk runs for those rows
// alone. Each row passes the predicate of its own kind (refs.VisibleRow).
var issueProposedQuery = `
	select held.kind, held.id
	from graph_nodes held
	where held.state = '` + graphstore.StateProposed + `'
	  and ` + refs.StandingIssueSQL(`held.kind`, `held.id`) + ` = $1
	  and ` + refs.VisibleRow("held", "$2") + `
	order by held.kind, held.id`

// listIssueProposed is GET /api/v1/issues/{key}/proposed: each node under the issue whose state is
// proposed and that the caller may see (issueProposedQuery), each answered through the read
// registry's loader of its kind (refs.ExcerptedNodes), which binds that kind's predicate again and
// answers the node whole with its excerpt. It takes no parameter but the issue. An issue nobody
// holds is 404 NOT_FOUND; an issue that holds a proposed node exists, so only an empty list asks
// whether the issue does.
func (s *server) listIssueProposed(w http.ResponseWriter, r *http.Request) {
	reader, ok := s.actingReader(w, r)
	if !ok {
		return
	}
	ctx, key := r.Context(), r.PathValue("key")
	pool, err := s.readyPool()
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	rows, err := pool.Query(ctx, issueProposedQuery, key, refs.VisibilityArg(reader))
	if err != nil {
		s.writeHandlerError(w, fmt.Errorf("read the proposed nodes of %s: %w", key, err))
		return
	}
	var targets []refs.Target
	for rows.Next() {
		var kind, id string
		if err := rows.Scan(&kind, &id); err != nil {
			rows.Close()
			s.writeHandlerError(w, fmt.Errorf("scan a proposed node of %s: %w", key, err))
			return
		}
		targets = append(targets, refs.Target{Kind: graphstore.AddressKind(kind, id), ID: id})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, fmt.Errorf("read the proposed nodes of %s: %w", key, err))
		return
	}
	if len(targets) == 0 {
		if err := s.requireIssueExists(ctx, key); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	// The store keys a document by its ref_key, which is the id its loader reads it by.
	resolved, err := refs.ExcerptedNodes(ctx, pool, reader, targets)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	read := issueProposed{Issue: key, Nodes: []proposedNode{}}
	for _, target := range targets {
		// A node its loader answers none of - a block no version holds now - is no node of the
		// graph, and the read leaves it out as every read of the graph does.
		if node, found := resolved[target]; found {
			read.Nodes = append(read.Nodes, proposedNode{Node: node.Node, Excerpt: node.Excerpt})
		}
	}
	WriteJSON(w, http.StatusOK, read)
}
