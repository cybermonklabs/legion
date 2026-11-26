package api

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// The coverage read and the results read answer over the store's tuples: each reads the issue's
// tree (docs.WalkIssueTree) and the serves tuples into its requirements, so a requirement or a
// check stands wherever the tree placed it, with no document's markdown walked.

// The words a check's newest standing result leaves it in (model.RequirementProof.Proves).
const (
	provesNone     = "none"
	provesFail     = "fail"
	provesReworded = "reworded"
	provesPass     = "pass"
)

// tupleCheck is what the coverage read over the tuples holds of one node that serves a
// requirement: its type, its issue, and its current text sum.
type tupleCheck struct {
	typ, issue, sum string
}

// issueCoverageOverTuples answers what each requirement node of the issue's tree lacks, read over
// the tuples. The requirements are the requirement nodes of the issue's tree in the tree's own
// order, each the reader may see. What serves each is read by walking serves inbound
// from the requirement (servesInto, over graph_tuples_as_of_in), so a check serving three
// requirements is listed once under each of them; a node of the issue or of an issue under it
// serves where it is live, whatever state a node above it holds, and a proposed or an overtaken
// one serves nothing (readServingChecks). A requirement no live check serves is a gap; one no
// check of which holds a passing result against the words the check holds now is unproven - a
// check holding no result, holding only a fail, or holding a pass recorded against a text sum it no
// longer holds - and a retracted result counts as none; one a check proves is proved. A requirement
// whose node or a node above it is overtaken - a question by its ask node's state, its `:::ask`
// block's included (readTreeRequirements) - is closed with its subtree: it lists no gap and no
// proof row, and it still counts the checks and the plan tasks that serve it, since the count is a
// fact and closed means only that no gap is listed (INFRA-1253's architect,
// dispatch://INFRA-1253/comment/cb7311d9-ca4b-4c63-b083-d8f49a9a7efa). A proposed requirement is
// a question still open and raises no gap.
//
// On an issue that is a view (docs.ViewOf, read with the walk: IssueTree.ViewOf) the read lists
// the requirement nodes of the ancestor's tree, in that tree's order, that a check of the view or
// of an issue under it serves, each read over what the view and the issues under it hold; and it
// names each requirement node of the view's own tree, unless it is closed, in the one line
// `<block id> is a requirement on a view` of the gaps, with no entry among the requirements. A
// block the block index has not reached has no node, so the read answers it once that index runs.
// While a document of the issue owes its block index, the read names it among OwedDocuments and
// raises no missing check or plan task, and its gaps hold that document's line in their place
// (owedGap). Every caller reads on the pool, outside any transaction, so it sends no
// `set local jit = off` (refs.TurnJITOff), which would change nothing there.
func (s *server) issueCoverageOverTuples(ctx context.Context, q queryer, who model.Actor, issueKey string, trees *displayTreeReader) (model.IssueCoverage, error) {
	coverage, _, err := s.issueCoverageOverTuplesAndOwedGaps(ctx, q, who, issueKey, trees)
	return coverage, err
}

// issueCoverageOverTuplesAndOwedGaps is issueCoverageOverTuples together with the gap line of each
// document of the issue that owes its block index (owedGap), which the answer's gaps hold wherever
// it covers a requirement and an approval request's answer carries in place of the missing checks
// (approvalCoverage).
func (s *server) issueCoverageOverTuplesAndOwedGaps(ctx context.Context, q queryer, who model.Actor, issueKey string, trees *displayTreeReader) (model.IssueCoverage, []string, error) {
	coverage := model.IssueCoverage{Issue: issueKey, Requirements: []model.RequirementCoverage{}, Gaps: []string{}}
	// The issue's spec, whether it has a plan and the documents that owe their block index, with
	// their gap lines (queueOwedDocumentGaps), in one batch.
	var owedGaps []string
	batch := &pgx.Batch{}
	batch.Queue(`
		select coalesce((select a.id::text from artifacts a where a.issue_key = $1 and a.is_primary), ''),
		       (`+issueQueryDocumentSQL("$1", graphstore.QueryPlan)+`) is not null
	`, issueKey).QueryRow(func(row pgx.Row) error {
		if err := row.Scan(&coverage.SpecArtifactID, &coverage.HasPlan); err != nil {
			return fmt.Errorf("read the documents of %s: %w", issueKey, err)
		}
		return nil
	})
	queueOwedDocumentGaps(batch, issueKey, &coverage.OwedDocuments, &owedGaps)
	if err := q.SendBatch(ctx, batch).Close(); err != nil {
		return model.IssueCoverage{}, nil, err
	}
	owed := len(owedGaps) > 0
	tree, err := docs.WalkIssueTree(ctx, q, issueKey, refs.TreeCut{})
	if err != nil {
		return model.IssueCoverage{}, nil, err
	}
	own, err := readTreeRequirements(ctx, q, who, tree)
	if err != nil {
		return model.IssueCoverage{}, nil, err
	}
	if tree.ViewOf == "" && len(own.visible) == 0 {
		coverage.Gaps = []string{noRequirementsGap}
		return coverage, owedGaps, nil
	}
	issues, err := coverageIssues(ctx, q, who, issueKey)
	if err != nil {
		return model.IssueCoverage{}, nil, err
	}
	if tree.ViewOf == "" {
		requirements, wholes, err := coverTreeRequirements(ctx, q, who, issueKey, own, issues, coverage.HasPlan, false, owed)
		if err != nil {
			return model.IssueCoverage{}, nil, err
		}
		coverage.Requirements, coverage.Gaps = requirements, append(requirementGaps(requirements), owedGaps...)
		registerTreeTexts(trees, coverage.Requirements, wholes)
		return coverage, owedGaps, nil
	}
	above, err := docs.WalkIssueTree(ctx, q, tree.ViewOf, refs.TreeCut{})
	if err != nil {
		return model.IssueCoverage{}, nil, err
	}
	ancestor, err := readTreeRequirements(ctx, q, who, above)
	if err != nil {
		return model.IssueCoverage{}, nil, err
	}
	requirements, wholes, err := coverTreeRequirements(ctx, q, who, tree.ViewOf, ancestor, issues, coverage.HasPlan, true, owed)
	if err != nil {
		return model.IssueCoverage{}, nil, err
	}
	coverage.Requirements = append(coverage.Requirements, requirements...)
	coverage.Gaps = append(requirementGaps(coverage.Requirements), owedGaps...)
	for _, target := range own.visible {
		if !own.closed(target) {
			_, blockID, _ := strings.Cut(target.ID, "#")
			coverage.Gaps = append(coverage.Gaps, blockID+" "+viewRequirementReason)
		}
	}
	if len(coverage.Requirements) == 0 && len(own.visible) == 0 {
		coverage.Gaps = []string{noRequirementsGap}
	}
	registerTreeTexts(trees, coverage.Requirements, wholes)
	return coverage, owedGaps, nil
}

// treeRequirements is what the coverage read over the tuples reads of the requirement nodes of one
// issue's tree: the tree's cut; each requirement node the reader may see, in the tree's own order,
// with its id; the walk's parent of each node; the node of each block of the tree as the reader may
// see it (refs.Nodes) and of each ask of its conversation; and the ask whose node holds the
// question's state of each `:::ask` block above a requirement (questions). These tell whether a
// requirement is closed.
type treeRequirements struct {
	cut       refs.TreeCut
	visible   []refs.Target
	ids       []string
	parents   map[refs.Target]refs.Target
	nodes     map[refs.Target]model.GraphNode
	questions map[refs.Target]refs.Target
}

// readTreeRequirements is the treeRequirements of tree for who. A question's state is its ask
// node's alone, never its `:::ask` block's (R2 and R3 of INFRA-1150; api's askHoldsState refuses
// a write on the block). The ask nodes come from the walk's own read of the conversation
// (IssueTree.Conversation), which holds every ask of the issue as its loader answers it and leaves
// none out for any reader, since no ask is reserved for its author: they cost no read. An
// `:::ask` block above a requirement is read to its ask (questionAsks), in one statement sent only
// where a requirement stands under such a block.
func readTreeRequirements(ctx context.Context, q queryer, who model.Actor, tree docs.IssueTree) (treeRequirements, error) {
	read := treeRequirements{cut: tree.Cut, parents: make(map[refs.Target]refs.Target, len(tree.Nodes))}
	var chain []refs.Target
	var requirements []refs.Target
	askBlocks := map[refs.Target]bool{}
	for _, node := range tree.Nodes {
		target := refs.Target{Kind: node.Kind, ID: node.ID}
		read.parents[target] = node.Parent
		if node.Kind == refs.BlockNodeKind {
			chain = append(chain, target)
			switch node.Type {
			case requirementBlockType:
				requirements = append(requirements, target)
			case graphstore.SupertagAsk:
				askBlocks[target] = true
			}
		}
	}
	if len(requirements) == 0 {
		return read, nil
	}
	nodes, err := refs.Nodes(ctx, q, who, chain)
	if err != nil {
		return treeRequirements{}, err
	}
	for target, excerpted := range tree.Conversation {
		if target.Kind == graphstore.KindAsk {
			nodes[target] = excerpted.Node
		}
	}
	read.nodes = nodes
	var questions []string
	for _, requirement := range requirements {
		if _, seen := nodes[requirement]; !seen {
			continue
		}
		read.visible = append(read.visible, requirement)
		read.ids = append(read.ids, requirement.ID)
		for at, steps := read.parents[requirement], 0; at != (refs.Target{}) && steps <= len(read.parents); steps++ {
			if askBlocks[at] && !slices.Contains(questions, at.ID) {
				questions = append(questions, at.ID)
			}
			at = read.parents[at]
		}
	}
	if len(questions) == 0 {
		return read, nil
	}
	if read.questions, err = questionAsks(ctx, q, questions, read.cut); err != nil {
		return treeRequirements{}, err
	}
	return read, nil
}

// questionAsksStatement is, at the cut whose seq bound $2 is, the ask of each `:::ask` block node
// $1 names, as refs' QuestionMap finds it off the tuples: the block's own ask, whose asked_on names
// the block, read into the block through graph_tuples_as_of_in, else, for a copy, its source, which
// the block's copied_from names, read out of it through graph_tuples_as_of_out (INFRA-1098's
// architect, dispatch://INFRA-1253/comment/2aa81020-a1e3-4037-b43b-d4b3104ab195). Of each relation
// the newest tuple at or below the bound is kept while it asserts. Each block's rows come own ask
// first, then newest first.
const questionAsksStatement = `
	select question.block, question.ask
	from (
		select newest.block, newest.ask, true as own, newest.seq
		from (
			select distinct on (t.to_id, t.from_id) t.to_id as block, t.from_id as ask, t.seq, t.asserted
			from graph_tuples t
			where t.to_kind = '` + refs.BlockNodeKind + `' and t.to_id = any($1) and t.kind = '` + graphstore.TupleAskedOn + `'
			  and t.from_kind = '` + graphstore.KindAsk + `' and t.seq <= $2
			order by t.to_id, t.from_id, t.seq desc
		) newest
		where newest.asserted
		union all
		select newest.block, newest.ask, false, newest.seq
		from (
			select distinct on (t.from_id, t.to_id) t.from_id as block, t.to_id as ask, t.seq, t.asserted
			from graph_tuples t
			where t.from_kind = '` + refs.BlockNodeKind + `' and t.from_id = any($1) and t.kind = '` + graphstore.TupleCopiedFrom + `'
			  and t.to_kind = '` + graphstore.KindAsk + `' and t.seq <= $2
			order by t.from_id, t.to_id, t.seq desc
		) newest
		where newest.asserted
	) question
	order by question.block, question.own desc, question.seq desc`

// questionAsks is the ask whose node holds the question's state of each of blocks, `:::ask` block
// nodes, at cut's seq, the head's where it is a dated cut (questionAsksStatement): the first row of
// each block. A copy whose node holds no copied_from yet, whose source only its opening line names,
// gets none, and its block reads by its own state: the walk keeps no block's opening line
// (refs.TreeChild holds the type it gives).
func questionAsks(ctx context.Context, q queryer, blocks []string, cut refs.TreeCut) (map[refs.Target]refs.Target, error) {
	seq, _ := treeCutBounds(cut)
	rows, err := q.Query(ctx, questionAsksStatement, blocks, seq)
	if err != nil {
		return nil, fmt.Errorf("read the asks of %d question blocks: %w", len(blocks), err)
	}
	defer rows.Close()
	questions := make(map[refs.Target]refs.Target, len(blocks))
	for rows.Next() {
		var block, ask string
		if err := rows.Scan(&block, &ask); err != nil {
			return nil, fmt.Errorf("read the ask of a question block: %w", err)
		}
		if target := (refs.Target{Kind: refs.BlockNodeKind, ID: block}); questions[target] == (refs.Target{}) {
			questions[target] = refs.Target{Kind: graphstore.KindAsk, ID: ask}
		}
	}
	return questions, rows.Err()
}

// closed says whether the node target, or a node above it in the walk, is overtaken: by its own
// node's state, or, for an `:::ask` block, by its ask node's (questions).
func (read treeRequirements) closed(target refs.Target) bool {
	for at, steps := target, 0; at != (refs.Target{}) && steps <= len(read.parents); steps++ {
		state := read.nodes[at].State
		if ask, held := read.questions[at]; held {
			state = read.nodes[ask].State
		}
		if state == graphstore.StateOvertaken {
			return true
		}
		at = read.parents[at]
	}
	return false
}

// coverTreeRequirements is the coverage over the tuples of each requirement of read, the
// requirements of the issue issueKey's tree, for who: what of the nodes of issues serves it, its
// proof, its state and its gaps, as issueCoverageOverTuples answers it, with the whole text each
// one's text_tree is read from. With servedOnly, a requirement no check of issues serves is left
// out, as a view lists only the requirements of its ancestor that its checks serve. While a
// document of the issue owes its block index, no requirement is marked as lacking a check or a plan task.
func coverTreeRequirements(ctx context.Context, q queryer, who model.Actor, issueKey string, read treeRequirements, issues []string, hasPlan, servedOnly, owed bool) ([]model.RequirementCoverage, []string, error) {
	if len(read.visible) == 0 {
		return nil, nil, nil
	}
	serving, err := servesInto(ctx, q, read.ids, read.cut)
	if err != nil {
		return nil, nil, err
	}
	checks, proof, err := readServingChecks(ctx, q, who, serving, issues, read.cut)
	if err != nil {
		return nil, nil, err
	}
	facts, err := refs.TreeFactsOf(ctx, q, read.visible, read.cut)
	if err != nil {
		return nil, nil, err
	}
	documents, err := blockDocuments(ctx, q, read.ids)
	if err != nil {
		return nil, nil, err
	}
	requirements := make([]model.RequirementCoverage, 0, len(read.visible))
	wholes := make([]string, 0, len(read.visible))
	for _, target := range read.visible {
		if servedOnly && !slices.ContainsFunc(serving[target.ID], func(from refs.Target) bool {
			return checks[from].typ == graphstore.BlockTypeCheck
		}) {
			continue
		}
		whole := facts[target].Values[graphstore.AttributeText]
		_, blockID, _ := strings.Cut(target.ID, "#")
		requirement := model.RequirementCoverage{
			BlockID: blockID, State: cmp.Or(read.nodes[target].State, graphstore.StateLive), Gaps: []string{}, Proof: []model.RequirementProof{},
		}
		requirement.Ref, _ = text.FormatBlockRef(text.Ref{Kind: "block", IssueKey: issueKey, ID: cmp.Or(documents[target.ID], "spec"), Block: blockID})
		if parsed, err := pmdoc.ParseRendering(whole); err == nil {
			pmdoc.Walk(parsed, func(node *pmdoc.Node) bool {
				if node.Type == requirementBlockType && requirement.Text == "" {
					requirement.Text = blockWords(node)
				}
				return true
			})
		}
		closed := read.closed(target)
		proved := false
		for _, from := range serving[target.ID] {
			check, serves := checks[from]
			if !serves {
				continue
			}
			switch check.typ {
			case graphstore.BlockTypeCheck:
				requirement.Checks++
				if !closed {
					requirement.Proof = append(requirement.Proof, model.RequirementProof{Check: from.ID, Issue: check.issue, Proves: proof[from]})
					proved = proved || proof[from] == provesPass
				}
			case planTaskBlockType:
				requirement.PlanTasks++
			}
		}
		switch {
		case closed:
			requirement.Coverage = "closed"
		case requirement.State == graphstore.StateProposed:
			requirement.Coverage = graphstore.StateProposed
		case requirement.Checks == 0:
			requirement.Coverage = "gap"
		case proved:
			requirement.Coverage = "proved"
		default:
			requirement.Coverage = "unproven"
		}
		if !closed && requirement.State != graphstore.StateProposed && !owed {
			if requirement.Checks == 0 {
				requirement.Gaps = append(requirement.Gaps, noCheckReason)
			}
			if hasPlan && requirement.PlanTasks == 0 {
				requirement.Gaps = append(requirement.Gaps, noPlanTaskReason)
			}
		}
		requirements = append(requirements, requirement)
		wholes = append(wholes, whole)
	}
	return requirements, wholes, nil
}

// registerTreeTexts registers with trees, the request's own reader, nil for a caller that asked
// for no tree, the text_tree of each of requirements, the rows the answer carries, from the whole
// text at its index of wholes, once the rows have stopped growing; with no reader it cuts no text.
func registerTreeTexts(trees *displayTreeReader, requirements []model.RequirementCoverage, wholes []string) {
	if trees == nil {
		return
	}
	for index, whole := range wholes {
		trees.text(&requirements[index].TextTree, blockTextContent(whole, true))
	}
}

// planTaskBlockType is the typed block a plan node is.
const planTaskBlockType = "plan_task"

// readServingChecks reads each node serving names that is a check or a plan node of one of
// issues, as the reader may see it, that serves: one at neither proposed, a question still open,
// nor overtaken, which is closed, by its own state alone, whatever state a node above it holds
// (INFRA-1253's architect, dispatch://INFRA-1253/comment/c797249f-ab6c-4b52-a848-724932e12668).
// It reads its type and current text sum (its document_blocks row), its issue, and for a check
// where its newest result that stands leaves it (checkProof), read off the result nodes under it.
func readServingChecks(ctx context.Context, q queryer, who model.Actor, serving map[string][]refs.Target, issues []string, cut refs.TreeCut) (map[refs.Target]tupleCheck, map[refs.Target]string, error) {
	var targets []refs.Target
	var ids []string
	held := map[refs.Target]bool{}
	for _, froms := range serving {
		for _, from := range froms {
			if from.Kind == refs.BlockNodeKind && !held[from] {
				held[from] = true
				targets = append(targets, from)
				ids = append(ids, from.ID)
			}
		}
	}
	checks := map[refs.Target]tupleCheck{}
	proof := map[refs.Target]string{}
	if len(targets) == 0 {
		return checks, proof, nil
	}
	nodes, err := refs.Nodes(ctx, q, who, targets)
	if err != nil {
		return nil, nil, err
	}
	rows, err := q.Query(ctx, `select node_id, type, text_sha256 from document_blocks where node_id = any($1) and type = any($2)`,
		ids, []string{graphstore.BlockTypeCheck, planTaskBlockType})
	if err != nil {
		return nil, nil, fmt.Errorf("read the nodes that serve the requirements: %w", err)
	}
	defer rows.Close()
	var checkTargets []refs.Target
	for rows.Next() {
		var id string
		var check tupleCheck
		if err := rows.Scan(&id, &check.typ, &check.sum); err != nil {
			return nil, nil, fmt.Errorf("read the nodes that serve the requirements: %w", err)
		}
		target := refs.Target{Kind: refs.BlockNodeKind, ID: id}
		node, seen := nodes[target]
		check.issue = treeIssueOf(target)
		if !seen || node.State == graphstore.StateProposed || node.State == graphstore.StateOvertaken || !slices.Contains(issues, check.issue) {
			continue
		}
		checks[target] = check
		if check.typ == graphstore.BlockTypeCheck {
			checkTargets = append(checkTargets, target)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("read the nodes that serve the requirements: %w", err)
	}
	rows.Close()
	results, err := refs.TreeChildren(ctx, q, checkTargets, cut)
	if err != nil {
		return nil, nil, err
	}
	var resultTargets []refs.Target
	for _, child := range results {
		if child.Node.Kind == graphstore.KindResult {
			resultTargets = append(resultTargets, child.Node)
		}
	}
	facts, err := refs.TreeFactsOf(ctx, q, resultTargets, cut)
	if err != nil {
		return nil, nil, err
	}
	newest := map[refs.Target]refs.TreeChild{}
	for _, child := range results {
		if child.Node.Kind != graphstore.KindResult {
			continue
		}
		if _, retracted := facts[child.Node].Values[graphstore.AttributeRetracted]; retracted {
			continue
		}
		if held, set := newest[child.Parent]; !set || child.MadeAt.After(held.MadeAt) ||
			child.MadeAt.Equal(held.MadeAt) && child.Node.ID > held.Node.ID {
			newest[child.Parent] = child
		}
	}
	for _, target := range checkTargets {
		proof[target] = checkProof(newest, facts, target, checks[target].sum)
	}
	return checks, proof, nil
}

// checkProof is where the newest result that stands of the check target leaves it, against the
// text sum the check holds now.
func checkProof(newest map[refs.Target]refs.TreeChild, facts map[refs.Target]refs.TreeFacts, target refs.Target, sum string) string {
	result, held := newest[target]
	if !held {
		return provesNone
	}
	values := facts[result.Node].Values
	switch {
	case values[graphstore.AttributeVerdict] != provesPass:
		return provesFail
	case values[graphstore.AttributeCheckSHA256] != sum:
		return provesReworded
	}
	return provesPass
}

// blockDocuments is the slug of the document each block node of ids is a block of, as the block's
// row names it (refs.BlockDocumentSQL).
func blockDocuments(ctx context.Context, q queryer, ids []string) (map[string]string, error) {
	rows, err := q.Query(ctx, `
		select d.node_id, doc.slug from document_blocks d
		cross join lateral (select `+refs.BlockDocumentSQL("d")+` as id) owner
		join artifacts doc on doc.id = owner.id
		where d.node_id = any($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("read the documents of %d blocks: %w", len(ids), err)
	}
	defer rows.Close()
	slugs := map[string]string{}
	for rows.Next() {
		var id, slug string
		if err := rows.Scan(&id, &slug); err != nil {
			return nil, fmt.Errorf("read the documents of %d blocks: %w", len(ids), err)
		}
		slugs[id] = slug
	}
	return slugs, rows.Err()
}

// verificationChecksOverTuples answers the check nodes of documentID that the issue's Verification
// query takes, in the tree's order (docs' IssueTree.Takes). It reads the words and attributes from
// each node's text value and its version and text sum from its document_blocks row.
// trees is the request's reader, nil for a caller that asked for none. Its one caller reads on the
// pool, outside any transaction, so it sends no `set local jit = off` (refs.TurnJITOff), which
// would change nothing there.
func verificationChecksOverTuples(ctx context.Context, q queryer, issueKey, documentID string, trees *displayTreeReader) ([]model.CheckBlock, error) {
	tree, err := docs.WalkIssueTree(ctx, q, issueKey, refs.TreeCut{})
	if err != nil {
		return nil, err
	}
	taken, _ := tree.Takes(graphstore.QueryVerification)
	var targets []refs.Target
	var ids []string
	for _, index := range taken {
		node := tree.Nodes[index]
		if node.Kind == refs.BlockNodeKind && node.Type == graphstore.BlockTypeCheck {
			targets = append(targets, refs.Target{Kind: node.Kind, ID: node.ID})
			ids = append(ids, node.ID)
		}
	}
	checks := []model.CheckBlock{}
	if len(targets) == 0 {
		return checks, nil
	}
	facts, err := refs.TreeFactsOf(ctx, q, targets, tree.Cut)
	if err != nil {
		return nil, err
	}
	type row struct {
		document, room, slug, sum string
		version                   int
	}
	held := map[string]row{}
	rows, err := q.Query(ctx, `
		select d.node_id, owner.id::text, d.artifact_id::text, doc.slug, d.changed_version, d.text_sha256
		from document_blocks d
		cross join lateral (select `+refs.BlockDocumentSQL("d")+` as id) owner
		join artifacts doc on doc.id = owner.id
		where d.node_id = any($1) and doc.id = $2`, ids, documentID)
	if err != nil {
		return nil, fmt.Errorf("read the checks of %s: %w", issueKey, err)
	}
	for rows.Next() {
		var id string
		var read row
		if err := rows.Scan(&id, &read.document, &read.room, &read.slug, &read.version, &read.sum); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read the checks of %s: %w", issueKey, err)
		}
		held[id] = read
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the checks of %s: %w", issueKey, err)
	}
	var contents []string
	for _, target := range targets {
		read, found := held[target.ID]
		if !found {
			continue
		}
		whole := facts[target].Values[graphstore.AttributeText]
		parsed, err := pmdoc.ParseRendering(whole)
		if err != nil {
			return nil, fmt.Errorf("read the check %s: %w", target.ID, err)
		}
		var check model.CheckBlock
		pmdoc.Walk(parsed, func(node *pmdoc.Node) bool {
			if node.Type == graphstore.BlockTypeCheck && check.BlockID == "" {
				check = docs.CheckBlockOf(node)
				check.Text = blockWords(node)
			}
			return true
		})
		_, check.BlockID, _ = strings.Cut(target.ID, "#")
		check.IssueKey, check.ArtifactID, check.RowArtifactID = issueKey, read.document, read.room
		check.Version, check.SHA256, check.Results = read.version, read.sum, []model.CheckResult{}
		check.Ref, _ = text.FormatBlockRef(text.Ref{Kind: "block", IssueKey: issueKey, ID: read.slug, Block: check.BlockID})
		checks = append(checks, check)
		if trees != nil {
			contents = append(contents, blockTextContent(whole, true))
		}
	}
	for index, content := range contents {
		trees.text(&checks[index].TextTree, content)
	}
	return checks, nil
}
