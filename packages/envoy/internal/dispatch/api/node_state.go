package api

// node_state.go serves the node state of dispatch://INFRA-1950 (spec r-proposed, r-state-closed):
// one write of a node's state (POST /api/v1/issues/{key}/nodes/{id}/state), the settle of a
// proposed node (POST /api/v1/issues/{key}/nodes/{id}/settle), and the proposed nodes of an issue
// (GET /api/v1/issues/{key}/proposed).
//
// The node state takes five values (graphstore.NodeStates) and no other: a word that is none of
// them is graphstore's 400 INVALID_ATTRIBUTE_VALUE, refused before anything is read or stored. The
// state route takes proposed and overtaken on any node the store holds, live on any such node that
// is not gone, and gone on a node that belongs to no document alone - a lens, a finding, a risk or
// a constraint - because a document's own write removes its block. settled goes through the settle
// alone, which a person makes on any proposal and any other caller on a proposal its own
// credential wrote (maySettle). The state route holds a move out of settled, and a move into
// proposed, to that one rule too, and refuses any other caller 403 STATE_NOT_PERMITTED: the first
// undoes what a settle decided, and the second takes a requirement's gaps out of the coverage read
// and the approval request. Both routes write through writeNodeState, at graphstore.NoEvent: no
// event is appended and no delivery is sent, as for every node of these kinds (reasoning_write.go),
// and the reads answer the new state at once, since every one of them reads the node's row.
//
// A block of an ask, a comment or a message is a node every read answers, and no node the store
// holds: no writer, the backfill among them, writes a graph_nodes row for one, so every read
// answers it live (refs.StoredStateAndVisibility). The state route refuses it every value, naming
// why (itemBlockRefusal), and the settle refuses it as a node that is not proposed.
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

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// Codes of the two routes' own refusals.
const (
	// codeStateNotWritable is a value of the node state the state route does not write on the
	// node it names: settled anywhere, gone on a node of a document, and any value on a gone node.
	codeStateNotWritable = "NODE_STATE_NOT_WRITABLE"
	// codeNotProposed is a settle of a node that is not at proposed.
	codeNotProposed = "NODE_NOT_PROPOSED"
	// codeSettleNotPermitted is a settle by a caller who is neither a person nor the proposal's
	// own author.
	codeSettleNotPermitted = "SETTLE_NOT_PERMITTED"
	// codeStateNotPermitted is a move of a node out of settled, or into proposed, through the
	// state route by a caller who is neither a person nor the node's own author (maySettle).
	codeStateNotPermitted = "STATE_NOT_PERMITTED"
)

// itemBlockRefusal is the 409 NODE_STATE_NOT_WRITABLE of a write of asked on target, held at held,
// where target is a block of an ask, a comment or a message, which a tuple names by its item's
// block kind (refs.ItemKind.BlockKind, farEndOf) and the store holds no row of; nil for any other
// node. With no row there is nothing for the write to move.
func itemBlockRefusal(target stateTarget, held, asked string) error {
	item, _, _, isItemBlock := refs.ParseItemBlockNode(target.end.id)
	if !isItemBlock || item.BlockKind() != target.end.kind {
		return nil
	}
	return errorf(http.StatusConflict, codeStateNotWritable,
		"the %s %s is %s and takes no %s: the store keeps no node of a block of an ask, a comment or a message, so every read answers such a block live and no write stores its state",
		target.kind, target.address, held, asked)
}

// stateTarget is the node a write of its state names: its kind as the graph addresses it, the
// address a refusal names it by, and the kind and the id the store keys it by.
type stateTarget struct {
	kind, address string
	end           storedEnd
}

// goneNodeUnderIssueQuery is whether the store holds gone ($3) the node whose stored kind and id
// are $1 and $2, standing under the issue $4 (refs.StandingIssueSQL), for a reader ($5) the node's
// own kind's predicate admits (refs.VisibleRow).
var goneNodeUnderIssueQuery = `
	select true from graph_nodes held
	where held.kind = $1 and held.id = $2 and held.state = $3
	  and ` + refs.StandingIssueSQL(`held.kind`, `held.id`) + ` = $4
	  and ` + refs.VisibleRow("held", "$5")

// findStateTarget is the node the `{id}` of a state route names under the issue key, read for who:
// FindNodeUnderIssue's node, or, where that function answers it does not exist, a node the store
// holds gone under the issue that who may see, which every read of the graph leaves out and the
// state route still refuses by name. Anything else is FindNodeUnderIssue's own 404, word for word,
// so a refusal tells nothing of a node the caller may not see.
func findStateTarget(ctx context.Context, q refs.Queryer, who model.Actor, key, id string) (stateTarget, error) {
	node, end, err := findEndUnderIssue(ctx, q, who, key, id)
	if err == nil {
		return stateTarget{kind: node.Kind, address: cmp.Or(node.Ref, id), end: end.stored()}, nil
	}
	var refusal *apiError
	if !errors.As(err, &refusal) || refusal.status != http.StatusNotFound {
		return stateTarget{}, err
	}
	kind, address, ok := decodeNodeAddress(id)
	if !ok {
		return stateTarget{}, err
	}
	stored := storedEnd{kind: kind, id: address}.stored()
	var gone bool
	readErr := q.QueryRow(ctx, goneNodeUnderIssueQuery, stored.kind, stored.id, graphstore.StateGone, key, refs.VisibilityArg(who)).Scan(&gone)
	if errors.Is(readErr, pgx.ErrNoRows) {
		return stateTarget{}, err
	}
	if readErr != nil {
		return stateTarget{}, fmt.Errorf("read whether %s %s is gone under %s: %w", stored.kind, stored.id, key, readErr)
	}
	return stateTarget{kind: kind, address: id, end: stored}, nil
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

// --- POST /api/v1/issues/{key}/nodes/{id}/state -------------------------------------------------

// nodeStateInput is the body of one write of a node's state.
type nodeStateInput struct {
	Actor *model.Actor `json:"actor"`
	// State is the value the node's state is written at: proposed or overtaken on any node the
	// store holds, live on any such node that is not gone, gone on a lens, a finding, a risk or a
	// constraint. settled is written by the settle alone, and any other word is no value of the
	// node state.
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
	case held == graphstore.StateGone:
		return refused("a gone node takes no state through this route, since writing one would bring back a node its writer took out")
	case asked == graphstore.StateGone && !slices.Contains(refs.StandingKinds(), target.kind):
		return refused("a document's own write removes its block and its own routes take out an issue, a document, an ask, " +
			"a comment or a message, so this route writes gone only on a lens, a finding, a risk or a constraint")
	}
	return nil
}

// writeNodeStateRoute is POST /api/v1/issues/{key}/nodes/{id}/state: one write of the state of the
// node {id} names, by the caller. The value is held to the five the node state takes first
// (graphstore.CheckAttributeValue), so a word that is none of them is 400 INVALID_ATTRIBUTE_VALUE,
// naming the attribute and the word, and nothing is read or stored. A node the caller may not see
// under the issue is 404 NOT_FOUND. The node's kind and its state then decide (stateRefusal): the
// route writes proposed and overtaken on any node the store holds, live on any such node that is
// not gone, and gone on a lens, a finding, a risk or a constraint alone, and refuses settled, gone
// on any other node and any value on a gone node with 409 NODE_STATE_NOT_WRITABLE. A gone node
// takes no value at all, live or any other, because proposed or overtaken would bring it back as
// surely as live, and a block of a document a version dropped would then read as a present block
// no version holds. A node the store holds no row of is the same 409: a block of an ask, a comment
// or a message, of which the store keeps none (itemBlockRefusal), and a node written before the
// store existed that the backfill has not reached yet. The write appends one state tuple, written
// by the caller (writeNodeState); a write of the state the node holds appends none. A move out of
// settled, and a move into proposed (takesTheSettleRule), then take the settle's caller rule over
// the node's made_by (maySettle): any other caller is 403 STATE_NOT_PERMITTED, naming the node,
// and nothing is stored, so the shared bearer, which proves no author, makes neither move.
func (s *server) writeNodeStateRoute(w http.ResponseWriter, r *http.Request) {
	var input nodeStateInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	caller, ok := s.reasoningCallerOf(w, r, input.Actor)
	if !ok {
		return
	}
	if err := graphstore.CheckAttributeValue(graphstore.AttributeState, input.State); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	ctx, key := r.Context(), r.PathValue("key")
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
	target, err := findStateTarget(ctx, tx, caller.reader(), key, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	node, previous, err := writeNodeState(ctx, tx, nodeStateWrite{
		issue: key, kind: target.end.kind, id: target.end.id, state: input.State, who: caller.recorded, at: at,
		allowed: func(stored graphstore.Node, held bool) error {
			if err := stateRefusal(target, stored.State, input.State); err != nil {
				return err
			}
			if !held {
				if err := itemBlockRefusal(target, stored.State, input.State); err != nil {
					return err
				}
				return errorf(http.StatusConflict, codeStateNotWritable,
					"the %s %s is %s and takes no %s yet: the store holds no row of it, which envoy-dispatch backfill-graph-store writes",
					target.kind, target.address, stored.State, input.State)
			}
			if takesTheSettleRule(stored.State, input.State) && !maySettle(caller.proving, stored.MadeBy) {
				return errorf(http.StatusForbidden, codeStateNotPermitted,
					"this credential may not move the %s %s from %s to %s: a person makes any move out of settled or into proposed, and any other caller only on a node whose author is the author this credential proves",
					target.kind, target.address, stored.State, input.State)
			}
			return nil
		},
	})
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

// takesTheSettleRule is whether a write of asked on a node the store holds at held is a move the
// settle's caller rule decides (maySettle): a move out of settled, which undoes what a settle
// decided, or a move into proposed, which takes a requirement's gaps out of the coverage read and
// the approval request. A write of proposed on a node already proposed is neither: it moves
// nothing and appends no tuple (writeNodeState), so it changes no decision and no count.
func takesTheSettleRule(held, asked string) bool {
	return held != asked && (held == graphstore.StateSettled || asked == graphstore.StateProposed)
}

// --- POST /api/v1/issues/{key}/nodes/{id}/settle ------------------------------------------------

// maySettle is whether the caller whose credential is proving settles a proposal whose made_by is
// author (dispatch://INFRA-1950/spec#b-r-proposed): a person settles any proposal - a browser's
// cookie, or a token holding the person scope, the credential actingPerson acts for a person on -
// and any other caller only a proposal whose recorded author is the author its credential proves,
// by the root's one author rule (graphstore.SameAuthor, the Go half of graphstore.AuthorIsSQL,
// which every read binds): the same person for a personal token, the same session for a session
// the broker verified, and the same workload for a pod's service-account token, whose write the
// rule reads as that workload's whether it reserved the node for itself or wrote it project-visible
// under a session its body named. No rule here reads a record any other way. The deployment's
// shared bearer proves no author (graphstore.ProvesAuthor), and every holder of it is one
// credential, so it settles nothing. The state route holds each move takesTheSettleRule names to
// this same rule, so the callers who may settle a node are the callers who may move it out of
// settled or into proposed.
func maySettle(proving, author model.Actor) bool {
	return proving.Credential.Holds(model.ScopePerson) || graphstore.SameAuthor(author, proving)
}

// settleNode is POST /api/v1/issues/{key}/nodes/{id}/settle: the one write that settles a proposed
// node, appending one state tuple at settled written by the caller (writeNodeState). A node the
// caller may not see under the issue is 404 NOT_FOUND, the same words a node nobody wrote answers;
// a node not at proposed is 409 NODE_NOT_PROPOSED; and a caller who is neither a person nor the
// proposal's own author (maySettle) is 403 SETTLE_NOT_PERMITTED. A node's author is its made_by:
// for a block of a document the writer of the version that first held the block, not whoever
// marked it proposed. A person's settle is written as the person (actingPerson), whichever of her
// credentials made it; any other caller's as the actor it records (actorFrom).
func (s *server) settleNode(w http.ResponseWriter, r *http.Request) {
	var input actorInput
	if !s.decodeOptionalJSON(w, r, &input) {
		return
	}
	proving, resolved, ok := s.resolveCaller(w, r)
	if !ok {
		return
	}
	var writer model.Actor
	if proving.Credential.Holds(model.ScopePerson) {
		writer, ok = s.actingPerson(w, resolved)
	} else {
		writer, ok = s.requireActor(w, resolved, input.Actor)
	}
	if !ok {
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
	target, err := findStateTarget(ctx, tx, proving.Recorded(), key, id)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	node, previous, err := writeNodeState(ctx, tx, nodeStateWrite{
		issue: key, kind: target.end.kind, id: target.end.id, state: graphstore.StateSettled, who: writer, at: at,
		allowed: func(stored graphstore.Node, _ bool) error {
			if stored.State != graphstore.StateProposed {
				return errorf(http.StatusConflict, codeNotProposed, "the %s %s is %s, and only a proposed node is settled",
					target.kind, target.address, stored.State)
			}
			if !maySettle(proving, stored.MadeBy) {
				return errorf(http.StatusForbidden, codeSettleNotPermitted,
					"this credential may not settle the %s %s: a person settles any proposal, and any other caller only a proposal whose author is the author this credential proves",
					target.kind, target.address)
			}
			return nil
		},
	})
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
	resolved, err := refs.ExcerptedNodes(ctx, pool, reader, true, targets)
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
