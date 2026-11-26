package api

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// The coverage read and the results read over the tuples ship beside the reads they replace,
// issueCoverage, which walks the spec's markdown for its requirements, and checkBlocksOf, which
// walks the Verification document's: each twin reads the issue's tree (docs.WalkIssueTree) and
// the serves tuples into its requirements, so a requirement or a check stands wherever the tree
// placed it, and twinCensus compares each twin with the read it replaces.

// The words a check's newest standing result leaves it in (model.RequirementProof.Proves).
const (
	provesNone     = "none"
	provesFail     = "fail"
	provesReworded = "reworded"
	provesPass     = "pass"
)

// tupleCheck is what the coverage read over the tuples holds of one node that serves a
// requirement: its type, its issue, its state for the reader, and its current text sum.
type tupleCheck struct {
	typ, issue, state, sum string
}

// issueCoverageOverTuples is issueCoverage's twin: what each requirement node of the issue's tree
// lacks, read over the tuples. The requirements are the requirement nodes of the issue's tree in
// the tree's own order, each the reader may see. What serves each is read by walking serves inbound
// from the requirement (servesInto, over graph_tuples_as_of_in), so a check serving three
// requirements is listed once under each of them; a node of the issue or of an issue under it
// serves, and a proposed one serves nothing yet. A requirement no check serves is a gap; one no
// check of which holds a passing result against the words the check holds now is unproven - a
// check holding no result, holding only a fail, or holding a pass recorded against a text sum it no
// longer holds - and a retracted result counts as none; one a check proves is proved. A requirement
// whose node or a node above it is overtaken is closed with its subtree: it lists no gap and
// nothing under it. A proposed requirement is a question still open and raises no gap.
//
// On an issue that is a view (docs.ViewOf, read with the walk: IssueTree.ViewOf) the read lists,
// as issueCoverage's viewCoverage does, the requirement nodes of the ancestor's tree, in that
// tree's order, that a check of the view or of an issue under it serves, each read over what the
// view and the issues under it hold; and it names each requirement node of the view's own tree,
// unless it is closed, in the one line `<block id> is a requirement on a view` of the gaps, with
// no entry among the requirements. A block the block index has not reached has no node, so the
// read answers it once that index runs. While a document of the issue owes its block index, the
// read names it among OwedDocuments and raises no missing check or plan task, and its gaps hold
// that document's line in their place (owedGap), as issueCoverage's do. Its first statement turns
// JIT off for the rest of the transaction q reads in (refs.TurnJITOff), as every read that walks a
// tree does.
func (s *server) issueCoverageOverTuples(ctx context.Context, q queryer, who model.Actor, issueKey string, trees *displayTreeReader) (model.IssueCoverage, error) {
	coverage := model.IssueCoverage{Issue: issueKey, Requirements: []model.RequirementCoverage{}, Gaps: []string{}}
	if err := refs.TurnJITOff(ctx, q); err != nil {
		return model.IssueCoverage{}, err
	}
	// The issue's spec, whether it has a plan and the documents that owe their block index, with
	// their gap lines (queueOwedDocumentGaps), in one batch, as issueCoverage reads them.
	var owedGaps []string
	batch := &pgx.Batch{}
	batch.Queue(`
		select coalesce((select a.id::text from artifacts a where a.issue_key = $1 and a.is_primary), ''),
		       exists (select 1 from artifacts a where a.issue_key = $1 and `+aliasedPlanRole+`)
	`, issueKey).QueryRow(func(row pgx.Row) error {
		if err := row.Scan(&coverage.SpecArtifactID, &coverage.HasPlan); err != nil {
			return fmt.Errorf("read the documents of %s: %w", issueKey, err)
		}
		return nil
	})
	queueOwedDocumentGaps(batch, issueKey, &coverage.OwedDocuments, &owedGaps)
	if err := q.SendBatch(ctx, batch).Close(); err != nil {
		return model.IssueCoverage{}, err
	}
	owed := len(owedGaps) > 0
	tree, err := docs.WalkIssueTree(ctx, q, issueKey, refs.TreeCut{})
	if err != nil {
		return model.IssueCoverage{}, err
	}
	own, err := readTreeRequirements(ctx, q, who, tree)
	if err != nil {
		return model.IssueCoverage{}, err
	}
	if tree.ViewOf == "" && len(own.visible) == 0 {
		coverage.Gaps = []string{noRequirementsGap}
		return coverage, nil
	}
	issues, err := coverageIssues(ctx, q, who, issueKey)
	if err != nil {
		return model.IssueCoverage{}, err
	}
	if tree.ViewOf == "" {
		requirements, wholes, err := coverTreeRequirements(ctx, q, who, issueKey, own, issues, coverage.HasPlan, false, owed)
		if err != nil {
			return model.IssueCoverage{}, err
		}
		coverage.Requirements, coverage.Gaps = requirements, append(requirementGaps(requirements), owedGaps...)
		registerTreeTexts(trees, coverage.Requirements, wholes)
		return coverage, nil
	}
	above, err := docs.WalkIssueTree(ctx, q, tree.ViewOf, refs.TreeCut{})
	if err != nil {
		return model.IssueCoverage{}, err
	}
	ancestor, err := readTreeRequirements(ctx, q, who, above)
	if err != nil {
		return model.IssueCoverage{}, err
	}
	requirements, wholes, err := coverTreeRequirements(ctx, q, who, tree.ViewOf, ancestor, issues, coverage.HasPlan, true, owed)
	if err != nil {
		return model.IssueCoverage{}, err
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
	return coverage, nil
}

// treeRequirements is what the coverage read over the tuples reads of the requirement nodes of one
// issue's tree: the tree's cut; each requirement node the reader may see, in the tree's own order,
// with its id; and the walk's parent of each node and the node of each block of the tree as the
// reader may see it (refs.Nodes), which tell whether a requirement is closed.
type treeRequirements struct {
	cut     refs.TreeCut
	visible []refs.Target
	ids     []string
	parents map[refs.Target]refs.Target
	nodes   map[refs.Target]model.GraphNode
}

// readTreeRequirements is the treeRequirements of tree for who.
func readTreeRequirements(ctx context.Context, q queryer, who model.Actor, tree docs.IssueTree) (treeRequirements, error) {
	read := treeRequirements{cut: tree.Cut, parents: make(map[refs.Target]refs.Target, len(tree.Nodes))}
	var chain []refs.Target
	var requirements []refs.Target
	for _, node := range tree.Nodes {
		target := refs.Target{Kind: node.Kind, ID: node.ID}
		read.parents[target] = node.Parent
		if node.Kind == refs.BlockNodeKind {
			chain = append(chain, target)
			if node.Type == requirementBlockType {
				requirements = append(requirements, target)
			}
		}
	}
	nodes, err := refs.Nodes(ctx, q, who, chain)
	if err != nil {
		return treeRequirements{}, err
	}
	read.nodes = nodes
	for _, requirement := range requirements {
		if _, seen := nodes[requirement]; seen {
			read.visible = append(read.visible, requirement)
			read.ids = append(read.ids, requirement.ID)
		}
	}
	return read, nil
}

// closed says whether the node target, or a node above it in the walk, is overtaken.
func (read treeRequirements) closed(target refs.Target) bool {
	for at, steps := target, 0; at != (refs.Target{}) && steps <= len(read.parents); steps++ {
		if read.nodes[at].State == graphstore.StateOvertaken {
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
// out, as a view lists only the requirements of its ancestor that its checks serve. While owed, while
// a document of the issue owes its block index, no requirement lacks a check or a plan task, as
// coverRequirements answers then.
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
		switch {
		case read.closed(target):
			requirement.Coverage = "closed"
		default:
			proved := false
			for _, from := range serving[target.ID] {
				check, serves := checks[from]
				if !serves {
					continue
				}
				switch check.typ {
				case graphstore.BlockTypeCheck:
					requirement.Checks++
					requirement.Proof = append(requirement.Proof, model.RequirementProof{Check: from.ID, Issue: check.issue, Proves: proof[from]})
					proved = proved || proof[from] == provesPass
				case planTaskBlockType:
					requirement.PlanTasks++
				}
			}
			switch {
			case requirement.State == graphstore.StateProposed:
				requirement.Coverage = graphstore.StateProposed
			case requirement.Checks == 0:
				requirement.Coverage = "gap"
			case proved:
				requirement.Coverage = "proved"
			default:
				requirement.Coverage = "unproven"
			}
			if requirement.State != graphstore.StateProposed && !owed {
				if requirement.Checks == 0 {
					requirement.Gaps = append(requirement.Gaps, noCheckReason)
				}
				if hasPlan && requirement.PlanTasks == 0 {
					requirement.Gaps = append(requirement.Gaps, noPlanTaskReason)
				}
			}
		}
		requirements = append(requirements, requirement)
		wholes = append(wholes, whole)
	}
	return requirements, wholes, nil
}

// registerTreeTexts registers with trees, the request's own reader, nil for a caller that asked
// for no tree, the text_tree of each of requirements, the rows the answer carries, from the whole
// text at its index of wholes, once the rows have stopped growing.
func registerTreeTexts(trees *displayTreeReader, requirements []model.RequirementCoverage, wholes []string) {
	for index, whole := range wholes {
		trees.text(&requirements[index].TextTree, blockTextContent(whole, true))
	}
}

// planTaskBlockType is the typed block a plan node is.
const planTaskBlockType = "plan_task"

// readServingChecks reads each node serving names that is a check or a plan node of one of
// issues, as the reader may see it and not proposed: its type and current text sum (its
// document_blocks row), its issue, and for a check where its newest result that stands leaves it
// (checkProof), read off the result nodes under it.
func readServingChecks(ctx context.Context, q queryer, who model.Actor, serving map[string][]refs.Target, issues []string, cut refs.TreeCut) (map[refs.Target]tupleCheck, map[refs.Target]string, error) {
	var targets []refs.Target
	var ids []string
	for _, froms := range serving {
		for _, from := range froms {
			if from.Kind == refs.BlockNodeKind && !slices.Contains(targets, from) {
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
		check.state = node.State
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

// verificationChecksOverTuples is checkBlocksOf's twin: the check nodes the issue's verification
// query takes, in the tree's order (docs' IssueTree.Takes), each read off its node - its words and
// attributes from its text value, its version and text sum from its document_blocks row, its
// document from the block's own (refs.BlockDocumentSQL) - with no document's markdown walked.
// trees is the request's reader, nil for a caller that asked for none. Its first statement turns
// JIT off for the rest of the transaction q reads in (refs.TurnJITOff), as every read that walks a
// tree does.
func verificationChecksOverTuples(ctx context.Context, q queryer, issueKey string, trees *displayTreeReader) ([]model.CheckBlock, error) {
	if err := refs.TurnJITOff(ctx, q); err != nil {
		return nil, err
	}
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
		where d.node_id = any($1)`, ids)
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
		read := held[target.ID]
		_, check.BlockID, _ = strings.Cut(target.ID, "#")
		check.IssueKey, check.ArtifactID, check.RowArtifactID = issueKey, read.document, read.room
		check.Version, check.SHA256, check.Results = read.version, read.sum, []model.CheckResult{}
		check.Ref, _ = text.FormatBlockRef(text.Ref{Kind: "block", IssueKey: issueKey, ID: read.slug, Block: check.BlockID})
		checks = append(checks, check)
		contents = append(contents, blockTextContent(whole, true))
	}
	for index, content := range contents {
		trees.text(&checks[index].TextTree, content)
	}
	return checks, nil
}

// twinCensus compares, for the issue issueKey read for who, each twin with the read it replaces:
// issueCoverageOverTuples with issueCoverage, member by member of what both answer - whether the
// issue has a plan, its gap lines, and each requirement's block id, reference, words, state, the
// count of the checks and of the plan tasks that serve it, and its gap lines, in order - and
// verificationChecksOverTuples with checkBlocksOf over the issue's Verification document, each
// check's block id, reference, words, attributes, version and text sum, in order. It answers one
// line for each difference, naming the issue and what differs, and none where the two agree.
func (s *server) twinCensus(ctx context.Context, q queryer, who model.Actor, issueKey string) ([]string, error) {
	var differences []string
	differ := func(format string, args ...any) {
		differences = append(differences, issueKey+": "+fmt.Sprintf(format, args...))
	}
	replaced, err := s.issueCoverage(ctx, q, who, issueKey, nil)
	if err != nil {
		return nil, err
	}
	twin, err := s.issueCoverageOverTuples(ctx, q, who, issueKey, nil)
	if err != nil {
		return nil, err
	}
	if replaced.HasPlan != twin.HasPlan {
		differ("the coverage read answers has_plan %t and its twin %t", replaced.HasPlan, twin.HasPlan)
	}
	if !slices.Equal(replaced.Gaps, twin.Gaps) {
		differ("the coverage read answers the gaps %q and its twin %q", replaced.Gaps, twin.Gaps)
	}
	for index := range max(len(replaced.Requirements), len(twin.Requirements)) {
		var left, right model.RequirementCoverage
		if index < len(replaced.Requirements) {
			left = replaced.Requirements[index]
		}
		if index < len(twin.Requirements) {
			right = twin.Requirements[index]
		}
		if left.BlockID != right.BlockID || left.Ref != right.Ref || left.Text != right.Text || left.State != right.State ||
			left.Checks != right.Checks || left.PlanTasks != right.PlanTasks || !slices.Equal(left.Gaps, right.Gaps) {
			differ("requirement %d: the coverage read answers %s, and its twin %s", index+1, coverageLine(left), coverageLine(right))
		}
	}
	artifact, held, err := s.verificationDocument(ctx, q, issueKey, "")
	if err != nil {
		return nil, err
	}
	var replacedChecks []model.CheckBlock
	if held {
		if replacedChecks, err = s.checkBlocksOf(ctx, q, artifact, nil); err != nil {
			return nil, err
		}
	}
	twinChecks, err := verificationChecksOverTuples(ctx, q, issueKey, nil)
	if err != nil {
		return nil, err
	}
	for index := range max(len(replacedChecks), len(twinChecks)) {
		var left, right model.CheckBlock
		if index < len(replacedChecks) {
			left = replacedChecks[index]
		}
		if index < len(twinChecks) {
			right = twinChecks[index]
		}
		if checkLine(left) != checkLine(right) {
			differ("check %d: the results read answers %s, and its twin %s", index+1, checkLine(left), checkLine(right))
		}
	}
	return differences, nil
}

// coverageLine is what twinCensus compares of a requirement, in one line.
func coverageLine(requirement model.RequirementCoverage) string {
	return fmt.Sprintf("{%s %s %q %s checks=%d plan_tasks=%d gaps=%q}", requirement.BlockID, requirement.Ref, requirement.Text,
		requirement.State, requirement.Checks, requirement.PlanTasks, requirement.Gaps)
}

// checkLine is what twinCensus compares of a check, in one line.
func checkLine(check model.CheckBlock) string {
	return fmt.Sprintf("{%s %s %q when=%s surface=%s run=%q negative_control=%q ci_check=%q version=%s sha256=%s}",
		check.BlockID, check.Ref, check.Text, check.When, check.Surface, check.Run, check.NegativeControl, check.CICheck,
		strconv.Itoa(check.Version), check.SHA256)
}
