package api

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// chain.go answers GET /api/v1/issues/{key}/chain, the chain of dispatch://INFRA-1098's spec
// requirement R14: from each decision to the requirement it shaped, to each check that serves that
// requirement, to each result of that check, and the same walk read backwards from a result. It is
// one read of the issue, in one read-only transaction, and it writes nothing and stores nothing.
//
// The nodes, their placement, what the reader may see of them and the tuples each states now are
// the tree read's (issueTreeBuilder): a requirement, a check, a plan task or a decision is a node
// of the issue's tree, or a check or a plan task of an issue under it that serves one of the
// issue's requirements, each as the reader may see it. The hop from a requirement to a check is the
// check's `serves` tuple, and the hop from a decision to a requirement the decision's `shaped`
// tuple, each read off the node's facts at the tree's cut; the parent, part_of, is read apart and
// answered as each node's placement, so one check serving three requirements is one node with one
// parent that each of the three names. A check's words and its results are the results read's over
// the tuples (verificationChecksOverTuples, loadCheckResults, loadCheckResultRetractions,
// deriveCIResults), and whether a pass of it stands is that read's comparison (comparisonReader),
// so this file computes no standing of its own: it only says, from the fields the comparison
// answered, why a check that is not proved stands unproven. A recorded result is held to the
// registry's predicate of its node, as every read of the store is, so a result reserved for its
// author is no evidence for anyone else.

// The words of a gap line, after the block id of the node the gap is of (noCheckReason is the
// coverage read's own).
const (
	noDecisionReason    = "has no decision"
	noResultReason      = "has no result"
	servesNothingReason = "serves nothing"
)

// The kinds of gap the chain names (model.IssueChainGap).
const (
	chainGapNoDecision    = "no_decision"
	chainGapNoCheck       = "no_check"
	chainGapNoResult      = "no_result"
	chainGapServesNothing = "serves_nothing"
)

// Why a check that is not proved stands unproven (model.IssueChainUnproven).
const (
	unprovenNoResult        = "no_result"
	unprovenComparedNothing = "compared_nothing"
	unprovenFailed          = "failed"
	unprovenRetracted       = "retracted"
	unprovenWordsMoved      = "check_words_moved"
	unprovenOtherCommit     = "other_commit"
	unprovenCommitUnknown   = "commit_unknown"
)

// visibleResultsStatement is each id of $1 whose result node the reader bound at $2 may see, by the
// registry's predicate of a result (refs.VisibleKind): a result reserved for its author is read by
// that author alone, and one whose node the store does not hold is visible, as every read of the
// store answers such a node.
var visibleResultsStatement = `select r.id from unnest($1::text[]) as r (id) where ` +
	refs.VisibleKind(graphstore.KindResult, `r.id`, `$2`)

// GET /api/v1/issues/{key}/chain  (any authenticated caller)
//
// The chain of the issue as the caller may see it. ?at=<commit> compares every check with that one
// commit, as the results read's ?at= does; with none, each check is compared with what the results
// read compares it with.
func (s *server) getIssueChain(w http.ResponseWriter, r *http.Request) {
	who, ok := s.actingReader(w, r)
	if !ok {
		return
	}
	at := r.URL.Query().Get("at")
	if at != noCommitNamed && !resultCommitSHA.MatchString(at) {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest,
			"at must be a commit of 40 lowercase hex characters")
		return
	}
	key := r.PathValue("key")
	if err := s.requireIssueExists(r.Context(), key); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	chain, err := s.readIssueChain(r.Context(), who, key, at)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, chain)
}

// readIssueChain is the chain of the issue key for who, its checks compared with at, or with what
// the results read compares each with where at is noCommitNamed. Every read of the store is made
// in one read-only transaction at repeatable read, so the chain is one state of the store; what
// the comparison asks of pull request heads and of GitHub is asked after it, as the results read
// asks it.
func (s *server) readIssueChain(ctx context.Context, who model.Actor, key, at string) (model.IssueChain, error) {
	tx, err := s.deps.Store.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return model.IssueChain{}, fmt.Errorf("open the chain read of %s: %w", key, err)
	}
	defer tx.Rollback(ctx)
	chain := &chainRead{tree: &issueTreeBuilder{ctx: ctx, q: tx, who: who, key: key}}
	if err := chain.tree.build(0); err != nil {
		return model.IssueChain{}, err
	}
	chain.take()
	if err := chain.readServes(); err != nil {
		return model.IssueChain{}, err
	}
	if err := chain.readChecks(); err != nil {
		return model.IssueChain{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.IssueChain{}, fmt.Errorf("close the chain read of %s: %w", key, err)
	}
	if err := chain.judge(s, at); err != nil {
		return model.IssueChain{}, err
	}
	return chain.answer(at), nil
}

// chainRead is one chain read while it is built.
type chainRead struct {
	tree *issueTreeBuilder
	// requirements, decisions, checks and planTasks are the chain's nodes, each in the tree's order.
	requirements, decisions, checks, planTasks []refs.Target
	// serves is, for each check and plan task, the id of each block its serves names now that the
	// reader may see, in the order its facts answer them.
	serves map[refs.Target][]string
	// blocks is each check as the results read answers it, index for index with checks.
	blocks []model.CheckBlock
	// hidden is each recorded result of those checks the reader may not see, by id.
	hidden map[string]bool
}

// take sorts the nodes the tree read holds into the chain's: the issue's own requirements, the
// decisions the decisions read answered, and every check and plan task, the issue's own and those
// of an issue under it that the tree read reached through serves. The context the tree read
// carries is none of them.
func (c *chainRead) take() {
	for _, target := range c.tree.answered() {
		entry := c.tree.entries[target]
		if entry.role == treeRoleContext {
			continue
		}
		if _, decided := c.tree.decisions[target]; decided {
			c.decisions = append(c.decisions, target)
			continue
		}
		if target.Kind != refs.BlockNodeKind {
			continue
		}
		switch entry.typ {
		case requirementBlockType:
			if entry.issue == "" {
				c.requirements = append(c.requirements, target)
			}
		case graphstore.BlockTypeCheck:
			c.checks = append(c.checks, target)
		case planTaskBlockType:
			c.planTasks = append(c.planTasks, target)
		}
	}
}

// readServes reads, for each check and plan task, the blocks its serves names now that the reader
// may see: a node the tree read holds, or one outside it that the registry's loader answers the
// reader (refs.Nodes), such as a requirement of an issue above.
func (c *chainRead) readServes() error {
	serving := slices.Concat(c.checks, c.planTasks)
	outside := map[refs.Target]bool{}
	var unread []refs.Target
	for _, target := range serving {
		for _, relation := range c.tree.facts[target].Relations {
			if relation.Kind == servesTupleKind && relation.To.Kind == refs.BlockNodeKind &&
				!c.tree.kept[relation.To] && !outside[relation.To] {
				outside[relation.To] = true
				unread = append(unread, relation.To)
			}
		}
	}
	seen, err := refs.Nodes(c.tree.ctx, c.tree.q, c.tree.who, true, unread)
	if err != nil {
		return err
	}
	c.serves = make(map[refs.Target][]string, len(serving))
	for _, target := range serving {
		served := []string{}
		for _, relation := range c.tree.facts[target].Relations {
			if relation.Kind != servesTupleKind || relation.To.Kind != refs.BlockNodeKind {
				continue
			}
			if _, visible := seen[relation.To]; visible || c.tree.kept[relation.To] {
				served = append(served, relation.To.ID)
			}
		}
		c.serves[target] = served
	}
	return nil
}

// issueOf is the issue a node of the chain belongs to: the one its address names, else the issue
// read.
func (c *chainRead) issueOf(target refs.Target) string {
	return cmp.Or(treeIssueOf(target), c.tree.key)
}

// readChecks reads each check of the chain as the results read over the tuples answers it, from the
// checks of the Verification query of its issue (verificationChecksOverTuples), matched by the
// document and the block id its row names, and every result recorded against it with the
// retraction that stands against each (loadCheckResults, loadCheckResultRetractions), one read of
// each document. A check that read does not answer takes its words and its row from its own node.
// It then reads which of those results the reader may not see (visibleResultsStatement).
func (c *chainRead) readChecks() error {
	ctx, q := c.tree.ctx, c.tree.q
	var issues []string
	for _, target := range c.checks {
		if issue := c.issueOf(target); !slices.Contains(issues, issue) {
			issues = append(issues, issue)
		}
	}
	read := map[[2]string]model.CheckBlock{}
	for _, issue := range issues {
		checks, err := verificationChecksOverTuples(ctx, q, issue, nil)
		if err != nil {
			return err
		}
		for _, check := range checks {
			read[[2]string{check.ArtifactID, check.BlockID}] = check
		}
	}
	c.blocks = make([]model.CheckBlock, len(c.checks))
	var documents []string
	for index, target := range c.checks {
		row := c.tree.blocks[target.ID]
		check, held := read[[2]string{row.document, row.blockID}]
		if !held {
			_, blockID, _ := strings.Cut(target.ID, "#")
			check = model.CheckBlock{
				BlockID: blockID, IssueKey: c.issueOf(target), ArtifactID: row.document, Version: row.version, SHA256: row.sum,
				Text: typedBlockWords(c.tree.facts[target].Values[graphstore.AttributeText], graphstore.BlockTypeCheck),
			}
		}
		check.Results = []model.CheckResult{}
		c.blocks[index] = check
		if check.ArtifactID != "" && !slices.Contains(documents, check.ArtifactID) {
			documents = append(documents, check.ArtifactID)
		}
	}
	recorded := map[string]map[string][]model.CheckResult{}
	retracted := map[string]model.CheckResultRetracted{}
	for _, document := range documents {
		results, err := loadCheckResults(ctx, q, document)
		if err != nil {
			return err
		}
		recorded[document] = results
		retractions, err := loadCheckResultRetractions(ctx, q, document)
		if err != nil {
			return err
		}
		for id, retraction := range retractions {
			retracted[id] = retraction
		}
	}
	var ids []string
	for index := range c.blocks {
		check := &c.blocks[index]
		if results := recorded[check.ArtifactID][check.BlockID]; results != nil {
			check.Results = results
		}
		for result := range check.Results {
			if retraction, held := retracted[check.Results[result].ID]; held {
				check.Results[result].Retracted = &retraction
			}
			ids = append(ids, check.Results[result].ID)
		}
	}
	c.hidden = map[string]bool{}
	if len(ids) == 0 {
		return nil
	}
	rows, err := q.Query(ctx, visibleResultsStatement, ids, refs.VisibilityArg(c.tree.who))
	if err != nil {
		return fmt.Errorf("read which results of %s the reader may see: %w", c.tree.key, err)
	}
	visible, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("read which results of %s the reader may see: %w", c.tree.key, err)
	}
	for _, id := range ids {
		c.hidden[id] = true
	}
	for _, id := range visible {
		delete(c.hidden, id)
	}
	return nil
}

// judge answers each check's standing as the results read answers it: the result a check CI proves
// derives from its run, read for the checks of each issue at that issue's pull request head
// (deriveCIResults) over every result recorded against it, as the results read reads it; then,
// with the results the reader may not see taken out, what each check is compared with, where each
// result's commit stands, whether the check's words moved since, and whether a pass of it stands
// (comparisonReader), with every head learned before the first check is answered.
func (c *chainRead) judge(s *server, at string) error {
	ctx := c.tree.ctx
	var issues []string
	byIssue := map[string][]int{}
	for index, check := range c.blocks {
		if _, held := byIssue[check.IssueKey]; !held {
			issues = append(issues, check.IssueKey)
		}
		byIssue[check.IssueKey] = append(byIssue[check.IssueKey], index)
	}
	for _, issue := range issues {
		group := make([]model.CheckBlock, len(byIssue[issue]))
		for place, index := range byIssue[issue] {
			group[place] = c.blocks[index]
		}
		if err := s.deriveCIResults(ctx, issue, group); err != nil {
			return err
		}
		for place, index := range byIssue[issue] {
			c.blocks[index] = group[place]
		}
	}
	for index := range c.blocks {
		c.blocks[index].Results = slices.DeleteFunc(c.blocks[index].Results, func(result model.CheckResult) bool {
			return c.hidden[result.ID]
		})
	}
	reader := newComparisonReader(s, at)
	reader.learn(ctx, c.blocks)
	for index := range c.blocks {
		reader.answer(ctx, &c.blocks[index])
	}
	return nil
}

// countsInChain is whether a node at state serves and raises gaps: a node at proposed is a
// question still open and one at overtaken is closed, so neither serves anything nor lacks
// anything, as the coverage read counts them.
func countsInChain(state string) bool {
	return state != graphstore.StateProposed && state != graphstore.StateOvertaken
}

// answer is the chain as the response carries it: the requirements with the decisions, checks and
// plan tasks that name them, each decision with the requirements it shaped, each check with its
// standing and its results, each result with the walk back to its requirements and decisions, and
// the gaps.
func (c *chainRead) answer(at string) model.IssueChain {
	chain := model.IssueChain{
		Issue: c.tree.key, Requirements: []model.IssueChainRequirement{}, Decisions: []model.IssueChainDecision{},
		Checks: []model.IssueChainCheck{}, PlanTasks: []model.IssueChainPlanTask{}, Results: []model.IssueChainResult{},
		Gaps: []model.IssueChainGap{},
	}
	if at != noCommitNamed {
		chain.At = &at
	}
	required := map[string]int{}
	for _, target := range c.requirements {
		node := c.tree.node(target)
		requirement := model.IssueChainRequirement{
			ID: target.ID, BlockID: node.BlockID, State: node.State, Parent: node.Parent,
			Text:      typedBlockWords(c.tree.facts[target].Values[graphstore.AttributeText], requirementBlockType),
			Decisions: []string{}, Checks: []string{}, PlanTasks: []string{},
		}
		requirement.Ref, _ = text.FormatBlockRef(text.Ref{
			Kind: "block", IssueKey: c.tree.key, ID: cmp.Or(c.tree.blocks[target.ID].slug, "spec"), Block: node.BlockID,
		})
		required[target.ID] = len(chain.Requirements)
		chain.Requirements = append(chain.Requirements, requirement)
	}
	for _, target := range c.decisions {
		shaped := []string{}
		for _, relation := range c.tree.facts[target].Relations {
			if relation.Kind != refs.TupleShaped || relation.To.Kind != refs.BlockNodeKind || slices.Contains(shaped, relation.To.ID) {
				continue
			}
			if index, held := required[relation.To.ID]; held {
				shaped = append(shaped, relation.To.ID)
				chain.Requirements[index].Decisions = append(chain.Requirements[index].Decisions, target.ID)
			}
		}
		if len(shaped) == 0 {
			continue
		}
		// An agent's decision is its block's words; a person's is the question the ask put and the
		// answer the person gave, which its node holds (the decisions read's text is the question).
		decision := model.IssueChainDecision{Kind: target.Kind, ID: target.ID, Text: c.tree.decisions[target].Text, Shaped: shaped}
		if target.Kind == refs.BlockNodeKind {
			decision.BlockID = c.tree.node(target).BlockID
			decision.Text = typedBlockWords(c.tree.facts[target].Values[graphstore.AttributeText], refs.DecisionSupertag)
		} else {
			decision.Answer = c.tree.facts[target].Values[graphstore.TupleAnswer]
		}
		chain.Decisions = append(chain.Decisions, decision)
	}
	for _, target := range c.planTasks {
		node := c.tree.node(target)
		task := model.IssueChainPlanTask{
			ID: target.ID, BlockID: node.BlockID, Issue: c.issueOf(target), State: node.State, Parent: node.Parent,
			Text:   typedBlockWords(c.tree.facts[target].Values[graphstore.AttributeText], planTaskBlockType),
			Serves: c.serves[target],
		}
		task.Ref, _ = text.FormatBlockRef(text.Ref{
			Kind: "block", IssueKey: task.Issue, ID: c.tree.blocks[target.ID].slug, Block: node.BlockID,
		})
		if countsInChain(task.State) {
			for _, id := range task.Serves {
				if index, held := required[id]; held {
					chain.Requirements[index].PlanTasks = append(chain.Requirements[index].PlanTasks, target.ID)
				}
			}
		}
		chain.PlanTasks = append(chain.PlanTasks, task)
	}
	for index, target := range c.checks {
		block, node := c.blocks[index], c.tree.node(target)
		check := model.IssueChainCheck{
			ID: target.ID, BlockID: block.BlockID, Ref: block.Ref, Issue: cmp.Or(block.IssueKey, c.issueOf(target)),
			Text: block.Text, When: block.When, State: node.State, Parent: node.Parent, Serves: c.serves[target],
			Version: block.Version, SHA256: block.SHA256, ComparedWith: block.ComparedWith,
			ComparedNothing: block.ComparedNothing, PassStands: block.PassStands, Results: []string{},
			Proved: block.PassStands.Answer == model.PassStandsYes, Unproven: chainUnproven(block),
		}
		served, decisions := []string{}, []string{}
		if countsInChain(check.State) {
			served = check.Serves
			for _, id := range served {
				place, held := required[id]
				if !held {
					continue
				}
				requirement := &chain.Requirements[place]
				requirement.Checks = append(requirement.Checks, target.ID)
				requirement.Proved = requirement.Proved || check.Proved
				for _, decision := range requirement.Decisions {
					if !slices.Contains(decisions, decision) {
						decisions = append(decisions, decision)
					}
				}
			}
		}
		for _, result := range block.Results {
			check.Results = append(check.Results, result.ID)
			chain.Results = append(chain.Results, model.IssueChainResult{
				Result: result, Check: target.ID, Requirements: served, Decisions: decisions,
				Proves: result.Verdict == "pass" && standingCommit(result) == model.CommitAtBasis,
			})
		}
		chain.Checks = append(chain.Checks, check)
	}
	chain.Gaps = chainGaps(chain)
	return chain
}

// chainUnproven is why check, as the results read answered it, stands unproven, and none where a
// pass of it stands: that it is compared with nothing, that it holds no result, and for each result,
// newest first, each of the fields of its standing that keeps it from proving the check.
func chainUnproven(check model.CheckBlock) []model.IssueChainUnproven {
	unproven := []model.IssueChainUnproven{}
	if check.PassStands.Answer == model.PassStandsYes {
		return unproven
	}
	if len(check.ComparedWith) == 0 {
		unproven = append(unproven, model.IssueChainUnproven{Reason: unprovenComparedNothing})
	}
	if len(check.Results) == 0 {
		unproven = append(unproven, model.IssueChainUnproven{Reason: unprovenNoResult})
	}
	for _, result := range check.Results {
		because := func(reason string) {
			unproven = append(unproven, model.IssueChainUnproven{Reason: reason, Result: result.ID})
		}
		if result.Verdict != "pass" {
			because(unprovenFailed)
		}
		if result.Retracted != nil {
			because(unprovenRetracted)
		}
		if result.CheckWordsMoved != nil && *result.CheckWordsMoved {
			because(unprovenWordsMoved)
		}
		if result.CommitStands != nil {
			switch result.CommitStands.Answer {
			case model.CommitOtherCommit:
				because(unprovenOtherCommit)
			case model.CommitUnknown:
				because(unprovenCommitUnknown)
			}
		}
	}
	return unproven
}

// chainGaps is each gap of chain: of each requirement, no decision and no check; of each check, no
// result and nothing served; of each plan task, nothing served. A node at proposed or overtaken
// raises none (countsInChain).
func chainGaps(chain model.IssueChain) []model.IssueChainGap {
	gaps := []model.IssueChainGap{}
	gap := func(kind, node, blockID, reason string) {
		gaps = append(gaps, model.IssueChainGap{Kind: kind, Node: node, Line: blockID + " " + reason})
	}
	for _, requirement := range chain.Requirements {
		if !countsInChain(requirement.State) {
			continue
		}
		if len(requirement.Decisions) == 0 {
			gap(chainGapNoDecision, requirement.ID, requirement.BlockID, noDecisionReason)
		}
		if len(requirement.Checks) == 0 {
			gap(chainGapNoCheck, requirement.ID, requirement.BlockID, noCheckReason)
		}
	}
	for _, check := range chain.Checks {
		if !countsInChain(check.State) {
			continue
		}
		if len(check.Results) == 0 {
			gap(chainGapNoResult, check.ID, check.BlockID, noResultReason)
		}
		if len(check.Serves) == 0 {
			gap(chainGapServesNothing, check.ID, check.BlockID, servesNothingReason)
		}
	}
	for _, task := range chain.PlanTasks {
		if countsInChain(task.State) && len(task.Serves) == 0 {
			gap(chainGapServesNothing, task.ID, task.BlockID, servesNothingReason)
		}
	}
	return gaps
}

// typedBlockWords is the words of the first block of type typ in a block node's text value, with
// every link dropped (blockWords), as the coverage read over the tuples reads a requirement's, and
// "" where the text holds no such block.
func typedBlockWords(whole, typ string) string {
	parsed, err := pmdoc.ParseRendering(whole)
	if err != nil {
		return ""
	}
	words, found := "", false
	pmdoc.Walk(parsed, func(node *pmdoc.Node) bool {
		if !found && node.Type == typ {
			words, found = blockWords(node), true
		}
		return !found
	})
	return words
}
