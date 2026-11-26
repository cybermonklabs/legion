package api

// node_delete.go serves the delete of a node (R7 of the issue-tree spec): DELETE
// /api/v1/issues/{key}/nodes/{id}, which takes the node {id} names and its part_of subtree out in
// one transaction, and its dry run, DELETE /api/v1/issues/{key}/nodes/{id}?dry_run=true, which
// writes nothing and answers how many nodes the delete would take that a read of the tree of {key}
// returns to the caller. Both answer 200 {"count": <n>}. `{id}` is read as the move reads it
// (node_move.go): the node's store address under {key}, one percent-encoded path segment; a node
// the caller may not see there is 404 NOT_FOUND.
//
// The delete takes the node and every node its current part_of tuples hold beneath it, at any
// depth, less each comment, message and issue node of that subtree, each ask and `:::ask` block, and
// each document, with what stands under each: another writer's words, another issue, a question
// whose block stays in its document, and a node with a route of its own (deleteTakesStatement). Of
// what it takes, each block of a document leaves the text of the document that holds it, a block of
// another issue's room included, in one direct write of each document's tree under the room locks
// (docs.Service.DeleteBlocksText), which lifts each block it does not take out of the text of one it
// takes to where that text stood; each such document writes one version, with its artifact.version,
// which moves the document's open approval request, and that version's index pass states each
// removed block gone and orphans each comment anchored in its text. Each lens, finding, risk and
// constraint it takes goes gone as the state route writes it (graphstore.PlanState), and each
// comment whose node anchor names one of them is orphaned in the same write
// (docs.ReadGoneNodeAnchors). Every current tuple out of each node it takes is retracted, of every
// kind, but those the version's index pass retracts itself out of a block it removes (the links its
// pairs state and a requirement's constrained_by); a tuple into one from a node that stays stands.
// Where that changes a query's answer, the batch marks the issue's queries owed and the drain
// writes their versions after the COMMIT (docs.Service.DrainOwedQueries). The tuples and states are
// written at graphstore.NoEvent, as every node route writes its own.
//
// The refusals, in the order the route reads them: a node {id} names that the delete never takes -
// an ask or an `:::ask` block, a comment, a message, a result, an issue node or a document - is 400
// NODE_NOT_DELETABLE, after R32's refusal of an agent whose {id} is the block of an answered ask
// (409 ANSWERED_ASK_KEPT); then, on the delete itself, no If-Match is 428 PRECONDITION_REQUIRED and
// no expected_count 400 DELETE_INPUT; under the owner locks of {key} and of every issue a node of the
// subtree stands under (lockNodeRouteOwners), a node under an issue the delete did not lock at each
// of two attempts is 409 PARENT_CONTENDED, and a placement_seq no If-Match tag names 412
// PRECONDITION_FAILED. Then, on the dry run as on the delete: a subtree that holds a result, 409
// NODE_HOLDS_RESULT; R28's writer check on each requirement, criterion or check it would take, 403
// DOCUMENT_WRITER_NOT_ARCHITECT; from a caller that is no person, a person's decision it would take,
// 409 ANSWERED_ASK_KEPT, and a lens, a finding, a risk or a constraint it would take, 403
// STATE_NOT_PERMITTED, each naming a node only where the caller may see it. Last, on the delete, an
// expected_count that is not the count the delete takes is 409 DELETE_COUNT_MOVED, carrying the count
// now; and, on the dry run as on the delete, a text the delete would leave holding a callout, a list
// item or a decision it keeps with no block of its own, or a list item with no first paragraph of
// its own, is 409 CONTAINER_WOULD_EMPTY, naming that block and its kind, which the dry run reads off
// the document's tree with no lock. A refused delete stores nothing, and a dry run takes no write
// lock.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Codes of the delete's own refusals.
const (
	// codeNodeNotDeletable is a delete of a node it never takes: an ask or an `:::ask` block, a
	// comment, a message, a result, an issue node or a document, each of which has a route of its own.
	codeNodeNotDeletable = "NODE_NOT_DELETABLE"
	// codeDeleteInput is a delete with no expected_count, or a query value the route cannot read.
	codeDeleteInput = "DELETE_INPUT"
	// codeNodeHoldsResult is a delete whose subtree holds a recorded result, which is retracted
	// and never deleted.
	codeNodeHoldsResult = "NODE_HOLDS_RESULT"
	// codeDeleteCountMoved is a delete whose expected_count is no longer the count it takes.
	codeDeleteCountMoved = "DELETE_COUNT_MOVED"
)

// nodeDeleted is the answer of a delete that is taken, and of its dry run: the number of nodes the
// delete takes, or would take, that a read of the tree of {key} returns to the caller.
type nodeDeleted struct {
	Count int `json:"count"`
}

// deleteCountMoved is the body of the 409 DELETE_COUNT_MOVED of a delete whose expected_count is no
// longer the number it would take: its code, its message, and that number now, which a fresh dry
// run answers too.
type deleteCountMoved struct {
	Code  string `json:"code"`
	Error string `json:"error"`
	Count int    `json:"count"`
}

// deleteCountMovedError is the refusal deleteNode answers for a count that moved, which
// writeHandlerError writes as its body (deleteCountMoved).
type deleteCountMovedError struct {
	named           string
	count, expected int
}

func (e *deleteCountMovedError) Error() string {
	return fmt.Sprintf("the delete of %s takes %d nodes a read of the tree returns now, and expected_count names %d: what stands under it has changed since the count was read, so nothing was deleted; read the count again (dry_run=true) and confirm it",
		e.named, e.count, e.expected)
}

// body is the refusal's answer: its code, its message and the count now.
func (e *deleteCountMovedError) body() deleteCountMoved {
	return deleteCountMoved{Code: codeDeleteCountMoved, Error: e.Error(), Count: e.count}
}

// keptKinds are the kinds of node, as the store keys them, that a delete takes nothing of, nor of
// what stands under one (R7): an ask and a block of an ask, a comment and a message and a block of
// either, another writer's words; an issue node, another issue; a document and a version, each
// with a route of its own. An `:::ask` block of a document is one too (keptBranchSQL), whose ask
// keeps its block, its state and its answer.
var keptKinds = []string{
	graphstore.KindAsk, refs.ItemAsk.BlockKind(), graphstore.KindComment, refs.ItemComment.BlockKind(),
	graphstore.KindMessage, refs.ItemMessage.BlockKind(), graphstore.KindIssue, graphstore.KindArtifact, graphstore.KindVersion,
}

// keptBranchSQL is whether the node alias.kind and alias.id name, as the store keys it, is one the
// delete keeps with all that stands under it: a node of keptKinds, or a block whose document_blocks
// row holds an `:::ask` block.
func keptBranchSQL(alias string) string {
	return `(` + alias + `.kind in (` + store.QuoteLiterals(keptKinds) + `)
	         or exists (select 1 from document_blocks asked where asked.node_kind = ` + alias + `.kind
	                    and asked.node_address = ` + alias + `.id and asked.type = 'ask'))`
}

// deleteTakesStatement is the read of the delete of the node the store keys as $1 and $2: that node
// and every node its current part_of tuples hold beneath it, at any depth, as nodesBeneath walks
// them, each with the node it was reached through, stepping into no node the delete keeps
// (keptBranchSQL); each with whether the delete keeps it, the issue it stands under
// (refs.GoneStandingIssueSQL), whether every read answers it as gone (refs.GoneOrStaleRow), and for
// a block of a document its type, its block id and the document whose text holds it
// (refs.BlockDocumentSQL).
var deleteTakesStatement = `
	with recursive beneath (kind, id, parent_kind, parent_id) as (
		select $1::text, $2::text, null::text, null::text
		union
		select placed.from_kind, placed.from_id, beneath.kind, beneath.id
		from beneath
		cross join lateral (
			` + graphstore.CurrentTupleRead(`t.to_kind, t.from_kind, t.from_id`, `t.from_kind, t.from_id, t.asserted`,
	`t.to_kind in (beneath.kind, `+graphstore.AddressKindSQL("beneath.kind", "beneath.id")+`)
				and t.to_id = beneath.id and t.kind = '`+graphstore.TuplePartOf+`' and t.to_kind <> '`+graphstore.KindValue+`'`) + `
		) placed
		where placed.asserted and not ` + keptBranchSQL("beneath") + `
	)
	select reached.kind, reached.id, coalesce(reached.parent_kind, ''), coalesce(reached.parent_id, ''),
	       ` + keptBranchSQL("reached") + `,
	       coalesce(` + refs.GoneStandingIssueSQL("reached.kind", "reached.id") + `, ''),
	       coalesce((select ` + refs.GoneOrStaleRow("held") + ` from graph_nodes held
	                 where held.kind = ` + refs.StoredKindSQL("reached.kind") + ` and held.id = reached.id), false),
	       coalesce(d.type, ''), coalesce(d.block_id, ''), coalesce(` + refs.BlockDocumentSQL("d") + `::text, '')
	from beneath reached
	left join document_blocks d on d.node_kind = reached.kind and d.node_address = reached.id
	order by reached.kind, reached.id`

// deleteReached is one node the delete's read reaches (deleteTakesStatement): its kind as the graph
// addresses it and the kind and the id the store keys it by, the node, as the store keys it, it was
// reached through, whether the delete keeps it and what stands under it, the issue it stands under,
// whether every read answers it as gone, and for a block of a document its type, its block id and
// the document whose text holds it, each "" for any other node.
type deleteReached struct {
	kind, stored, id       string
	parentStored, parentID string
	kept                   bool
	issue                  string
	gone                   bool
	typ, block, document   string
}

// key is the node as the store keys it.
func (n deleteReached) key() graphstore.NodeKey { return graphstore.NodeKey{Kind: n.stored, ID: n.id} }

// documentBlock reports whether the node is a block of a document, whose text a version holds.
func (n deleteReached) documentBlock() bool { return n.document != "" }

// nodeDeleteRequest is one delete as the route read its request: the issue and the node's address,
// who reads (who) and who writes (writer), whether the caller is a person, whether it is the dry
// run, the expected_count it named (nil for none), the query's own refusal (400 DELETE_INPUT, nil
// for a query the route reads), and the If-Match tags.
type nodeDeleteRequest struct {
	key, id  string
	who      model.Actor
	writer   model.Actor
	person   bool
	dryRun   bool
	expected *int
	shape    error
	tags     []string
}

// readQuery reads dry_run and expected_count off query into request: dry_run is true or false, and
// on the delete itself expected_count a count, a whole number from 0; any other value of either is
// the request's 400 DELETE_INPUT, naming it. The dry run reads no expected_count.
func (request *nodeDeleteRequest) readQuery(query url.Values) {
	dryRun, named, valid := parseQueryBool(query, "dry_run", false, true)
	if named && !valid {
		request.shape = errorf(http.StatusBadRequest, codeDeleteInput, "dry_run takes true or false, not %q", query.Get("dry_run"))
		return
	}
	request.dryRun = dryRun
	if dryRun || !query.Has("expected_count") {
		return
	}
	value := query.Get("expected_count")
	expected, err := strconv.Atoi(value)
	if err != nil || expected < 0 {
		request.shape = errorf(http.StatusBadRequest, codeDeleteInput,
			"expected_count takes the count the dry run answered, a whole number from 0, not %q", value)
		return
	}
	request.expected = &expected
}

// deleteNodeRoute is DELETE /api/v1/issues/{key}/nodes/{id}: the delete of the node {id} names and
// its part_of subtree, or with dry_run=true the count it would take, by the caller (node_delete.go's
// head). The body is optional and names the actor alone, for a caller whose credential proves none.
func (s *server) deleteNodeRoute(w http.ResponseWriter, r *http.Request) {
	proving, resolved, ok := s.resolveCaller(w, r)
	if !ok {
		return
	}
	var input actorInput
	if !s.decodeOptionalJSON(w, resolved, &input) {
		return
	}
	writer, person, ok := s.nodeRouteWriter(w, proving, resolved, input.Actor)
	if !ok {
		return
	}
	request := nodeDeleteRequest{
		key: r.PathValue("key"), id: r.PathValue("id"), who: proving.Recorded(), writer: writer, person: person,
		tags: r.Header.Values("If-Match"),
	}
	request.readQuery(r.URL.Query())
	if request.dryRun {
		count, err := s.dryRunNodeDelete(r.Context(), request)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, nodeDeleted{Count: count})
		return
	}
	count, published, marked, err := s.deleteNode(r.Context(), request)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(r.Context(), append(appendedEvents(published...), s.drainOwedQueries(r.Context(), marked)...)...)
	WriteJSON(w, http.StatusOK, nodeDeleted{Count: count})
}

// resultUnderIssueStatement reads whether the recorded result whose id is $1 stands under the issue
// $2, for the reader bound at $3 (refs.VisibilityArg): its node's part_of names its check, a check
// of that issue (check_results.check_issue_key), and the reader may see that issue. A result is
// project-visible (its registry entry), so the reader may see it where it may see its issue.
var resultUnderIssueStatement = `select exists (select 1 from check_results where id::text = $1 and check_issue_key = $2
	and ` + refs.VisibleKind(graphstore.KindIssue, "check_results.check_issue_key", "$3") + `)`

// findDeleteRoot is the node request names, as the delete reads it, read in q, once it is found
// under the issue for the caller (findWrittenNode: 404 NOT_FOUND for a node the caller may not see,
// 409 NODE_GONE for a gone one), and is a node the delete takes. A recorded result, which no read
// of the registry resolves, is read by its address `result/<id>` off its row, and is 404 NOT_FOUND
// to a caller who may not see the issue it stands under. R32 refuses an agent's delete whose {id}
// is the `:::ask` block of an answered ask (409 ANSWERED_ASK_KEPT, quoting its question), and every
// node the delete never takes is then 400 NODE_NOT_DELETABLE, naming it and its kind, whoever the
// caller is; then the query's own refusal (400 DELETE_INPUT).
func findDeleteRoot(ctx context.Context, q queryer, request nodeDeleteRequest) (moveNode, error) {
	if kind, id, addressed := strings.Cut(request.id, "/"); addressed && kind == graphstore.KindResult {
		var held bool
		if err := q.QueryRow(ctx, resultUnderIssueStatement, id, request.key, refs.VisibilityArg(request.who)).Scan(&held); err != nil {
			return moveNode{}, fmt.Errorf("read whether the result %s stands under %s: %w", id, request.key, err)
		}
		if !held {
			return moveNode{}, nodeNotFound(request.key, request.id)
		}
		return moveNode{}, errorf(http.StatusBadRequest, codeNodeNotDeletable,
			"the result %s is not deleted: a recorded result is retracted, with the reason it is wrong (POST /api/v1/results/{id}/retract), and stays readable",
			request.id)
	}
	found, end, err := findWrittenNode(ctx, q, request.who, request.key, request.id)
	if err != nil {
		return moveNode{}, err
	}
	root := moveNodeOf(found, end, request.id)
	if err := readBlockType(ctx, q, &root); err != nil {
		return moveNode{}, err
	}
	if !request.person && root.kind == refs.BlockNodeKind && root.typ == "ask" {
		decision, decided, err := docs.PersonDecisionOfBlock(ctx, q, root.stored, root.id, root.typ)
		if err != nil {
			return moveNode{}, err
		}
		if decided {
			return moveNode{}, errorf(http.StatusConflict, codeAnsweredAskKept,
				"%s %s, %q, is a person's decision, and an agent cannot take it out: a person deletes what holds it, or an agent acting under a token that holds the person scope, and even then the delete leaves the block in its document",
				decision.What, root.named, decision.Words)
		}
	}
	if slices.Contains(keptKinds, root.stored) || root.typ == "ask" {
		return moveNode{}, errorf(http.StatusBadRequest, codeNodeNotDeletable,
			"the %s %s is not deleted through this route: %s, so it has a route of its own",
			root.word(), root.named, undeletableReason(root))
	}
	if request.shape != nil {
		return moveNode{}, request.shape
	}
	return root, nil
}

// undeletableReason is why the delete never takes root, a node of keptKinds or an `:::ask` block,
// for its refusal.
func undeletableReason(root moveNode) string {
	switch {
	case root.typ == "ask" || root.kind == graphstore.KindAsk || root.kind == refs.ItemAsk.BlockKind():
		return "an ask keeps its block, its state and its answer, and closes through the ask's own routes"
	case root.kind == graphstore.KindIssue:
		return "an issue node is another issue, which closes through its own routes"
	case root.kind == graphstore.KindArtifact || root.kind == graphstore.KindVersion:
		return "a document is removed through its own route (DELETE /api/v1/artifacts/{id})"
	}
	return "a comment and a message are another writer's words, which their author edits or deletes"
}

// deletePlan is what a delete decides of the node it takes, read in its transaction: the node, the
// nodes it takes - each node its read reaches that it keeps not and that is not gone yet, in the
// read's order - and the count of them a read of the tree of {key} returns to the caller. visible
// and named are, of each node it takes, whether the caller may see it and the words a refusal names
// it by, and rows, of each block of a document it takes, its row (refs.BlockRow), which holds the
// band of that document.
type deletePlan struct {
	root    moveNode
	takes   []deleteReached
	visible map[graphstore.NodeKey]bool
	named   map[graphstore.NodeKey]string
	rows    map[string]refs.BlockRow
	count   int
}

// readDeleteTakes reads deleteTakesStatement for root, through q, and answers each node the delete
// takes, each once, in the read's order.
func readDeleteTakes(ctx context.Context, q queryer, root moveNode) ([]deleteReached, error) {
	rows, err := q.Query(ctx, deleteTakesStatement, root.stored, root.id)
	if err != nil {
		return nil, fmt.Errorf("read the subtree of %s: %w", root.named, err)
	}
	var takes []deleteReached
	seen := map[graphstore.NodeKey]bool{}
	for rows.Next() {
		var reached deleteReached
		if err := rows.Scan(&reached.stored, &reached.id, &reached.parentStored, &reached.parentID, &reached.kept, &reached.issue, &reached.gone,
			&reached.typ, &reached.block, &reached.document); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read the subtree of %s: %w", root.named, err)
		}
		reached.kind = graphstore.AddressKind(reached.stored, reached.id)
		reached.stored = refs.StoredKindOf(reached.stored)
		if reached.kept || reached.gone || seen[reached.key()] {
			continue
		}
		seen[reached.key()] = true
		takes = append(takes, reached)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the subtree of %s: %w", root.named, err)
	}
	return takes, nil
}

// planNodeDelete reads what the delete of root decides, through q, and holds it to the refusals the
// delete reads before it writes, in their order (node_delete.go's head): a result, R28's writer
// check, and, from a caller who is no person, a person's decision and a lens, a finding, a risk or a
// constraint. The dry run reads it with no lock held; the delete, under the owner locks.
func planNodeDelete(ctx context.Context, q queryer, request nodeDeleteRequest, root moveNode) (deletePlan, error) {
	takes, err := readDeleteTakes(ctx, q, root)
	if err != nil {
		return deletePlan{}, err
	}
	plan := deletePlan{root: root, takes: takes}
	if err := plan.readVisible(ctx, q, request.who); err != nil {
		return deletePlan{}, err
	}
	if err := plan.refuse(ctx, q, request); err != nil {
		return deletePlan{}, err
	}
	if err := plan.countRead(ctx, q, request.key); err != nil {
		return deletePlan{}, err
	}
	return plan, nil
}

// readVisible reads, for who, whether who may see each node the plan takes, and the words a refusal
// names it by: its dispatch:// address where who may see it and it has one, else its store address.
// A block of a document is read through the block loader with its row (refs.BlockNodesAndRows),
// whose band it keeps whether who may see the block or not, any other node of a kind the registry
// resolves through its kind's loader (refs.Nodes), each kind in one statement; a node of a kind no
// read resolves - a result - is project-visible, as the tree read reads it.
func (plan *deletePlan) readVisible(ctx context.Context, q queryer, who model.Actor) error {
	plan.visible = make(map[graphstore.NodeKey]bool, len(plan.takes))
	plan.named = make(map[graphstore.NodeKey]string, len(plan.takes))
	var blocks []string
	var targets []refs.Target
	for _, node := range plan.takes {
		plan.named[node.key()] = storeAddress(node.kind, node.id)
		switch {
		case node.documentBlock():
			blocks = append(blocks, node.id)
		case refs.ResolvesNodeKind(node.kind):
			targets = append(targets, refs.Target{Kind: node.kind, ID: node.id})
		default:
			plan.visible[node.key()] = true
		}
	}
	blockNodes, blockRows, err := refs.BlockNodesAndRows(ctx, q, who, blocks)
	if err != nil {
		return fmt.Errorf("read the blocks the delete of %s takes: %w", plan.root.named, err)
	}
	nodes, err := refs.Nodes(ctx, q, who, targets)
	if err != nil {
		return fmt.Errorf("read the nodes the delete of %s takes: %w", plan.root.named, err)
	}
	plan.rows = blockRows
	for _, node := range plan.takes {
		var held model.GraphNode
		var seen bool
		if node.documentBlock() {
			excerpted, found := blockNodes[node.id]
			held, seen = excerpted.Node, found
		} else if refs.ResolvesNodeKind(node.kind) {
			held, seen = nodes[refs.Target{Kind: node.kind, ID: node.id}]
		} else {
			continue
		}
		plan.visible[node.key()] = seen
		if seen && held.Ref != "" {
			plan.named[node.key()] = held.Ref
		}
	}
	return nil
}

// refuse holds the plan to the refusals the delete reads of what it takes, in their order: a subtree
// that holds a result (409 NODE_HOLDS_RESULT, naming the node and, where the caller may see it, the
// result); R28's writer check on each requirement, criterion or check (403
// DOCUMENT_WRITER_NOT_ARCHITECT), each judged on the issue it stands under; and, from a caller who is
// no person as R32 reads one, a decision a person decided (409 ANSWERED_ASK_KEPT, quoting its words
// and naming it, or saying only that the delete would take a person's decision where the caller may
// not see it), and a lens, a finding, a risk or a constraint (403 STATE_NOT_PERMITTED, the state
// route's refusal, naming the first the caller may see, or none).
func (plan *deletePlan) refuse(ctx context.Context, q queryer, request nodeDeleteRequest) error {
	for _, node := range plan.takes {
		if node.stored != graphstore.KindResult {
			continue
		}
		result := "a recorded result the caller may not see"
		if plan.visible[node.key()] {
			result = "the result " + plan.named[node.key()]
		}
		return errorf(http.StatusConflict, codeNodeHoldsResult,
			"the delete of %s would take %s, and a result is retracted, never deleted (POST /api/v1/results/{id}/retract): retract it first, or delete what stands under %s alone; nothing was deleted",
			plan.root.named, result, plan.root.named)
	}
	// One check for each issue and document the check judges: it reads the writer, the issue and the
	// document alone, so a second node of either passes or is refused as the first was. A node the
	// caller may not see is named by where it stands, under the node the delete names.
	writer := specWriter{actor: request.writer, person: request.person}
	judged := map[[2]string]bool{}
	for _, node := range plan.takes {
		document, held := specWriterNodeDocuments[node.typ]
		issue := cmp.Or(node.issue, request.key)
		if !node.documentBlock() || !held || judged[[2]string{issue, document}] {
			continue
		}
		judged[[2]string{issue, document}] = true
		named := plan.named[node.key()]
		if !plan.visible[node.key()] {
			named = "under " + plan.root.named
		}
		if err := refuseSpecWriterOfNodeType(ctx, q, writer, issue, node.typ, named); err != nil {
			return err
		}
	}
	if request.person {
		return nil
	}
	for _, node := range plan.takes {
		if !node.documentBlock() || node.typ != refs.DecisionSupertag {
			continue
		}
		decision, decided, err := docs.PersonDecisionOfBlock(ctx, q, node.stored, node.id, node.typ)
		if err != nil {
			return err
		}
		if !decided {
			continue
		}
		if !plan.visible[node.key()] {
			return errorf(http.StatusConflict, codeAnsweredAskKept,
				"the delete of %s would take a person's decision, and an agent cannot take one out: a person deletes it, or an agent acting under a token that holds the person scope; nothing was deleted",
				plan.root.named)
		}
		return errorf(http.StatusConflict, codeAnsweredAskKept,
			"the delete of %s would take the decision %s, %q, a person's decision, and an agent cannot take one out: a person deletes it, or an agent acting under a token that holds the person scope; nothing was deleted",
			plan.root.named, plan.named[node.key()], decision.Words)
	}
	// The first lens, finding, risk or constraint the delete takes that the caller may see names the
	// refusal; where it may see none, the refusal names none.
	standing, seen := false, -1
	for index, node := range plan.takes {
		if !standingKind(node.kind) {
			continue
		}
		standing = true
		if plan.visible[node.key()] {
			seen = index
			break
		}
	}
	if !standing {
		return nil
	}
	if seen >= 0 {
		node := plan.takes[seen]
		return goneNotPermitted(stateTarget{kind: kindWord(node.kind), address: plan.named[node.key()]})
	}
	return errorf(http.StatusForbidden, codeStateNotPermitted,
		"this credential may not delete %s: what stands under it holds a node a person alone takes gone, a lens, a finding, a risk or a constraint, so nothing was deleted",
		plan.root.named)
}

// countRead is the number of nodes the plan takes that a read of the tree of key returns to the
// caller (R7, R8): each node the walk of key takes at the head (docs.WalkIssueTree), so no node of
// another issue, which that walk passes over; that the caller may see (readVisible); and that the
// read answers, as the tree read leaves a node out (issueTreeBuilder.excluded): no block of a role
// document - the spec, the Verification document, the plan - that is no typed block, such as a typed
// block's own paragraph, and no block of another of the issue's documents, which the read with no
// view does not open (plan.rows). The walk is read only where some node is left to count.
func (plan *deletePlan) countRead(ctx context.Context, q queryer, key string) error {
	counted := map[graphstore.NodeKey]bool{}
	for _, node := range plan.takes {
		if !plan.visible[node.key()] {
			continue
		}
		if node.documentBlock() {
			switch plan.rows[node.id].Band {
			case refs.RankBandSpec, refs.RankBandVerification, refs.RankBandPlan:
				if !pmdoc.IsTypedBlock(node.typ) {
					continue
				}
			case refs.RankBandOther:
				if node.issue == key {
					continue
				}
			}
		}
		counted[node.key()] = true
	}
	if len(counted) == 0 {
		return nil
	}
	tree, err := docs.WalkIssueTree(ctx, q, key, refs.TreeCut{})
	if err != nil {
		return err
	}
	// Each counted node once, however often the walk names it.
	for _, node := range tree.Nodes {
		if walked := (graphstore.NodeKey{Kind: refs.StoredKindOf(node.Kind), ID: node.ID}); counted[walked] {
			plan.count++
			delete(counted, walked)
		}
	}
	return nil
}

// blockAddresses are the node addresses of the blocks of documents the plan takes.
func (plan deletePlan) blockAddresses() []string {
	var addresses []string
	for _, node := range plan.takes {
		if node.documentBlock() {
			addresses = append(addresses, node.id)
		}
	}
	return addresses
}

// documents is, by the id of each document whose text holds a block the plan takes, the block ids
// of those blocks, which the delete takes out of its text (docs.Service.DeleteBlocksText).
func (plan deletePlan) documents() map[string][]string {
	documents := map[string][]string{}
	for _, node := range plan.takes {
		if node.documentBlock() {
			documents[node.document] = append(documents[node.document], node.block)
		}
	}
	return documents
}

// standingKeys are, as the store keys them, the lenses, findings, risks and constraints the plan
// takes, whose comments' node anchors the delete orphans (docs.ReadGoneNodeAnchors).
func (plan deletePlan) standingKeys() []graphstore.NodeKey {
	var keys []graphstore.NodeKey
	for _, node := range plan.takes {
		if standingKind(node.kind) {
			keys = append(keys, node.key())
		}
	}
	return keys
}

// goneNodes is the node write that takes gone each node the plan takes that no version states gone:
// every one that is no block of a document - a lens, a finding, a risk, a constraint - written as
// the state route writes gone (graphstore.PlanState), each as the store holds it but for its state,
// keeping its cause, at the sentinel, dated at and signed by writer. PlanState takes everything
// beneath each root; only the nodes the plan takes are kept of what it answers, so a comment that
// stands on one of them keeps its state, as the delete keeps every comment. The roots are the nodes
// the plan reached through no other node it writes gone, each read once.
func (plan deletePlan) goneNodes(ctx context.Context, tx pgx.Tx, at time.Time, writer model.Actor) ([]graphstore.Node, error) {
	written := map[graphstore.NodeKey]bool{}
	for _, node := range plan.takes {
		if !node.documentBlock() {
			written[node.key()] = true
		}
	}
	var roots []graphstore.Node
	for _, node := range plan.takes {
		if node.documentBlock() || written[graphstore.NodeKey{Kind: node.parentStored, ID: node.parentID}] {
			continue
		}
		held, found, err := graphstore.ReadNode(ctx, tx, node.stored, node.id)
		if err != nil {
			return nil, err
		}
		if found {
			roots = append(roots, held)
		}
	}
	if len(roots) == 0 {
		return nil, nil
	}
	planned, err := graphstore.PlanState(ctx, tx, roots, graphstore.StateGone, at, writer)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(planned, func(node graphstore.Node) bool {
		return !written[graphstore.NodeKey{Kind: node.Kind, ID: node.ID}]
	}), nil
}

// deleteOutStatement reads every current relation out of each node the arrays $1 and $2 name by its
// stored kind and its id (graphstore.CurrentTupleRead, out of a node): its kind and its far end.
var deleteOutStatement = `
	select n.kind, n.id, held.kind, held.to_kind, held.to_id
	from unnest($1::text[], $2::text[]) as n (kind, id)
	cross join lateral (
		` + graphstore.CurrentTupleRead(`t.kind, t.to_kind, t.to_id`, `t.kind, t.to_kind, t.to_id, t.asserted`,
	`t.from_kind = n.kind and t.from_id = n.id and t.to_kind <> '`+graphstore.KindValue+`'`) + `
	) held
	where held.asserted
	order by n.kind, n.id, held.kind, held.to_kind, held.to_id`

// retractions is the retraction of every current tuple out of each node the plan takes, of every
// kind, a serves among them, at the sentinel, dated at and signed by writer (R7): but, out of a block
// of a document, which the version the delete writes of that document removes, a tuple of a kind
// that version's index pass retracts itself - a link its pairs state (refs.BlockRefKinds, a decision's
// decided_by, from_ask and shaped) and a constrained_by (retractGonePointers) - so no tuple is
// retracted twice. A tuple into a node it takes from a node that stays is that node's fact, and
// stands.
func (plan deletePlan) retractions(ctx context.Context, tx pgx.Tx, at time.Time, writer model.Actor) ([]graphstore.Tuple, error) {
	if len(plan.takes) == 0 {
		return nil, nil
	}
	kinds, ids := make([]string, len(plan.takes)), make([]string, len(plan.takes))
	block := map[graphstore.NodeKey]bool{}
	for index, node := range plan.takes {
		kinds[index], ids[index] = node.stored, node.id
		if node.documentBlock() {
			block[node.key()] = true
		}
	}
	var passKinds []string
	if len(block) > 0 {
		linked, err := refs.BlockRefKinds(ctx, tx)
		if err != nil {
			return nil, err
		}
		passKinds = append(linked, graphstore.TupleDecidedBy, refs.TupleFromAsk, refs.TupleShaped, graphstore.TupleConstrainedBy)
	}
	rows, err := tx.Query(ctx, deleteOutStatement, kinds, ids)
	if err != nil {
		return nil, fmt.Errorf("read the tuples out of what the delete of %s takes: %w", plan.root.named, err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (graphstore.Tuple, error) {
		tuple := graphstore.Tuple{At: at, Who: writer, SourceEvent: graphstore.NoEvent, Asserted: false, Changes: true}
		err := row.Scan(&tuple.FromKind, &tuple.FromID, &tuple.Kind, &tuple.ToKind, &tuple.ToID)
		return tuple, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the tuples out of what the delete of %s takes: %w", plan.root.named, err)
	}
	if len(block) == 0 {
		return out, nil
	}
	return slices.DeleteFunc(out, func(tuple graphstore.Tuple) bool {
		return block[graphstore.NodeKey{Kind: tuple.FromKind, ID: tuple.FromID}] && slices.Contains(passKinds, tuple.Kind)
	}), nil
}

// marks are the issues whose queries the delete marks owed: key, each issue a node it takes stands
// under, and each issue of a comment whose node anchor it orphans, each once.
func (plan deletePlan) marks(key string, anchored []string) []string {
	marks := []string{key}
	for _, node := range plan.takes {
		if node.issue != "" && !slices.Contains(marks, node.issue) {
			marks = append(marks, node.issue)
		}
	}
	for _, issue := range anchored {
		if !slices.Contains(marks, issue) {
			marks = append(marks, issue)
		}
	}
	return marks
}

// dryRunNodeDelete is the dry run of request: the count the delete would take, in a transaction
// that writes nothing and takes no write lock, after every refusal the delete reads before it
// writes, in the same order (findDeleteRoot, planNodeDelete), and last the refusal of a text the
// delete would leave holding a block with no block of its own, read off each document's tree with
// no lock, through the transaction the document service joins (docs.Service.PlanDeleteBlocksText,
// containerWouldEmpty), since a read from the pool beside the transaction's own connection would
// deadlock it. It reads neither If-Match nor expected_count, and never answers 409
// PARENT_CONTENDED.
func (s *server) dryRunNodeDelete(ctx context.Context, request nodeDeleteRequest) (int, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	documentCtx, ledger := s.deps.Docs.Join(ctx, tx)
	defer ledger.Discard()
	root, err := findDeleteRoot(ctx, tx, request)
	if err != nil {
		return 0, err
	}
	plan, err := planNodeDelete(ctx, tx, request, root)
	if err != nil {
		return 0, err
	}
	if err := containerWouldEmpty(root, plan.takes, s.deps.Docs.PlanDeleteBlocksText(documentCtx, plan.documents())); err != nil {
		return 0, err
	}
	return plan.count, nil
}

// containerWouldEmpty is err, the answer of a write of the delete of root's text or of its dry run,
// as the delete answers it: a block the delete keeps that its text would leave with no block of its
// own (pmdoc.ContainerEmptiedError) is 409 CONTAINER_WOULD_EMPTY, the move's code, naming that
// block, its kind and, where the error names the document whose text holds it, that document by
// the address of a block of it the delete takes (takes), in the delete's own words, and nothing is
// stored; any other error as it is.
func containerWouldEmpty(root moveNode, takes []deleteReached, err error) error {
	var emptied *pmdoc.ContainerEmptiedError
	if !errors.As(err, &emptied) {
		return err
	}
	kind := strings.ReplaceAll(emptied.Type, "_", " ")
	left, taken := "no block of its own", "the last block the "+kind+" holds"
	if emptied.Type == "list_item" {
		left, taken = "no first paragraph of its own", "the block the "+kind+" holds as its own first paragraph"
	}
	in := ""
	if at := slices.IndexFunc(takes, func(node deleteReached) bool {
		return emptied.Document != "" && node.document == emptied.Document
	}); at >= 0 {
		owner, _, _ := strings.Cut(takes[at].id, "#")
		in = " of " + owner
	}
	return errorf(http.StatusConflict, codeContainerWouldEmpty,
		"the delete of %s would leave the %s %q%s with %s: it takes %s; nothing was deleted, and the %s itself is deleted whole or kept",
		root.named, kind, emptied.Container, in, left, taken, kind)
}

// deleteLockRead is what the delete reads to learn its owner locks, and again under them
// (lockNodeRouteOwners): the node's subtree with the issue each of its nodes stands under, its
// placement_seq (readMove), and the issues of the comments whose node anchors name a lens, a
// finding, a risk or a constraint of that subtree (docs.NodeAnchorIssues), whose rows keep their
// issue wherever the node they are on stands; issues is all of those issues, each once.
type deleteLockRead struct {
	read   moveRead
	issues []string
}

// readDeleteLocks reads deleteLockRead for the delete of root, through q.
func readDeleteLocks(ctx context.Context, q queryer, root moveNode) (deleteLockRead, error) {
	read, err := readMove(ctx, q, root, moveNode{})
	if err != nil {
		return deleteLockRead{}, err
	}
	var standing []graphstore.NodeKey
	for _, node := range read.beneath {
		if standingKind(node.kind) {
			standing = append(standing, graphstore.NodeKey{Kind: refs.StoredKindOf(node.kind), ID: node.id})
		}
	}
	issues := read.issues()
	if len(standing) > 0 {
		anchored, err := docs.NodeAnchorIssues(ctx, q, standing)
		if err != nil {
			return deleteLockRead{}, err
		}
		for _, issue := range anchored {
			if !slices.Contains(issues, issue) {
				issues = append(issues, issue)
			}
		}
	}
	return deleteLockRead{read: read, issues: issues}, nil
}

// deleteNode makes the delete of request in one transaction, joined to the document service, and
// answers the count it took, the events its versions appended, and each issue whose queries its
// batch marked owed, which the caller drains after the COMMIT. It reads everything before its batch
// - the owner locks, then the plan and its refusals, then, under the room locks, each document's
// tree and its version - and sends every write in one batch with its COMMIT (node_delete.go's head).
func (s *server) deleteNode(ctx context.Context, request nodeDeleteRequest) (int, []*model.Event, []string, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return 0, nil, nil, err
	}
	defer tx.Rollback(ctx)
	documentCtx, ledger := s.deps.Docs.Join(ctx, tx)
	defer ledger.Discard()
	at, err := transactionNow(ctx, tx)
	if err != nil {
		return 0, nil, nil, err
	}
	root, err := findDeleteRoot(ctx, tx, request)
	if err != nil {
		return 0, nil, nil, err
	}
	if len(request.tags) == 0 {
		return 0, nil, nil, errorf(http.StatusPreconditionRequired, codePreconditionRequired,
			"a delete states the placement it read: send If-Match with the placement_seq of %s as a quoted entity tag, as the tree read answers it",
			root.named)
	}
	if request.expected == nil {
		return 0, nil, nil, errorf(http.StatusBadRequest, codeDeleteInput,
			"a delete names the count it confirms in expected_count: read it first with dry_run=true, then send it, so the delete takes that count or nothing")
	}
	locked, err := lockNodeRouteOwners(ctx, tx, request.key, func(q queryer) (deleteLockRead, []string, error) {
		read, err := readDeleteLocks(ctx, q, root)
		return read, read.issues, err
	}, s.betweenNodeRouteReads, func() error {
		return errorf(http.StatusConflict, parentContended,
			"the delete of %s met a node under an issue it had not locked at each of %d attempts while this request waited for its locks, so nothing was deleted: read the count again and confirm it",
			root.named, parentLockAttempts)
	})
	if err != nil {
		return 0, nil, nil, err
	}
	if !matchesPlacement(request.tags, locked.read.placementSeq) {
		return 0, nil, nil, errorf(http.StatusPreconditionFailed, codePreconditionFailed,
			"the %s %s stands at placement_seq %s, which no If-Match tag names: its parent has changed since it was read, so nothing was deleted; read the tree and delete it again",
			root.word(), root.named, placementWords(locked.read.placementSeq))
	}
	plan, err := planNodeDelete(ctx, tx, request, root)
	if err != nil {
		return 0, nil, nil, err
	}
	if plan.count != *request.expected {
		return 0, nil, nil, &deleteCountMovedError{named: root.named, count: plan.count, expected: *request.expected}
	}
	changed, err := s.deps.Docs.DeleteBlocksText(documentCtx, plan.documents(), request.writer)
	if err := containerWouldEmpty(root, plan.takes, err); err != nil {
		return 0, nil, nil, err
	}
	// Each version's index pass states the blocks it removed gone, and leaves a prevented_by into
	// one of them standing: the risk that holds it stays (refs.WithKeptPointerTargets).
	versionCtx := refs.WithKeptPointerTargets(documentCtx, plan.blockAddresses())
	var published []*model.Event
	for _, document := range changed {
		event, err := s.writeNodeRouteVersion(documentCtx, versionCtx, tx, ledger, document, request.writer)
		if err != nil {
			return 0, nil, nil, err
		}
		if event != nil {
			published = append(published, event)
		}
	}
	gone, err := plan.goneNodes(ctx, tx, at, request.writer)
	if err != nil {
		return 0, nil, nil, err
	}
	retractions, err := plan.retractions(ctx, tx, at, request.writer)
	if err != nil {
		return 0, nil, nil, err
	}
	orphans, err := docs.ReadGoneNodeAnchors(ctx, tx, plan.standingKeys(), request.writer, at)
	if err != nil {
		return 0, nil, nil, err
	}
	orphaned := orphans.Issues()
	for _, issue := range orphaned {
		if issue != request.key && !slices.Contains(locked.issues, issue) {
			return 0, nil, nil, errorf(http.StatusConflict, parentContended,
				"the delete of %s would orphan a comment of %s, an issue it had not locked, which was anchored while this request waited for its locks, so nothing was deleted: read the count again and confirm it",
				root.named, issue)
		}
	}
	write := nodeDeleteWrite{gone: gone, retractions: retractions, orphans: orphans, marks: plan.marks(request.key, orphaned)}
	marked := map[string]*bool{}
	if err := ledger.Queue(func(b *store.WriteBatch) error {
		queueNodeDelete(b, write, marked)
		return nil
	}); err != nil {
		return 0, nil, nil, err
	}
	if err := ledger.Commit(ctx); err != nil {
		return 0, nil, nil, err
	}
	for _, document := range changed {
		s.deps.Docs.ScheduleSettlement(document)
	}
	return plan.count, published, takenMarks(write.marks, marked), nil
}

// nodeDeleteWrite is the write a delete makes once every rule passed, at graphstore.NoEvent, dated
// at and signed by its writer, beside the versions of the documents whose text it changed: the node
// write of each node it takes gone that no version states gone (deletePlan.goneNodes), the
// retraction of every tuple out of what it takes (deletePlan.retractions), the orphaning of each
// comment whose node anchor names a node it takes gone (docs.NodeAnchorOrphans), and the mark of
// the queries of each of marks owed, which a converted issue takes (docs.QueueQueryMark).
type nodeDeleteWrite struct {
	gone        []graphstore.Node
	retractions []graphstore.Tuple
	orphans     docs.NodeAnchorOrphans
	marks       []string
}

// queueNodeDelete puts write on b: the node write and the retractions (graphstore.QueueWrite), the
// orphaned comments, the marks, after the tuples so their sequence is the last one the write drew
// (queueQueryMarks), and the head rows of what it states (graphstore.QueueHeads). marked holds, for
// each issue of marks, whether its mark took.
func queueNodeDelete(b *store.WriteBatch, write nodeDeleteWrite, marked map[string]*bool) {
	if len(write.gone) > 0 || len(write.retractions) > 0 {
		graphstore.QueueWrite(b, write.gone, write.retractions)
	}
	write.orphans.Queue(b)
	queueQueryMarks(b, write.marks, marked)
	graphstore.QueueHeads(b, graphstore.HeadsOf(write.gone, write.retractions))
}
