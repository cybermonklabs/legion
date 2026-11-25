package api

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// coverage.go answers what each requirement of an issue's spec lacks. The requirements are the
// `requirement` blocks of the issue's spec, in document order. A requirement has a check when a
// `check` block of any document of that issue or of any descendant issue, at any status, names
// it in `serves`, and a plan task when a `plan_task` block of that set does; the descendants are
// read with refs.IssueDescendants, the server's one recursive `child_of` subtree walk. The
// missing-plan-task gap is named only once the issue has a plan document, since the plan comes
// after the spec's approval. A requirement whose block's node state is proposed is a question
// still open (spec requirement r-proposed), so it raises neither gap here and the
// approval request's answer names none for it; one at settled or live is counted as every
// requirement is, and cybermonklabs/legion#4180 gives overtaken its meaning. A check or a plan task
// whose block is proposed is a question still open too, so it serves nothing yet and its
// requirement is counted without it (queueServingBlockCounts). Nothing is ever refused
// for a gap: three readers show it — the route below for the Spec tab, `dispatch read --issue`,
// which prints each gap after `Coverage: `, and an approval request's answer, which names the
// missing checks alone.

// noRequirementsGap is the one gap line a spec that marks no requirement answers, in place of a
// line per requirement. noCheckReason and noPlanTaskReason are what one requirement lacks, as
// RequirementCoverage.Gaps carries it; a document-level gap line is the requirement's block id
// and one of these, which is what noCheckGap builds.
const (
	noRequirementsGap = "the spec marks no requirements"
	noCheckReason     = "has no check"
	noPlanTaskReason  = "has no plan task"
)

// GET /api/v1/issues/{key}/coverage  (any authenticated caller)
//
// What each requirement of the issue's spec lacks. An issue with no spec, or a spec that marks
// no requirements, answers no requirements and the single gap line. A caller that sent
// `Dispatch-Trees: 1` is given each requirement's whole content as its text_tree.
func (s *server) getIssueCoverage(w http.ResponseWriter, r *http.Request) {
	who, ok := s.actingReader(w, r)
	if !ok {
		return
	}
	issueKey := r.PathValue("key")
	if err := s.requireIssueExists(r.Context(), issueKey); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	trees := s.readDisplayTrees(r.Context(), wantsDisplayTrees(r))
	coverage, err := s.issueCoverage(r.Context(), s.deps.Store.Pool, who, issueKey, trees)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	trees.resolve()
	WriteJSON(w, http.StatusOK, coverage)
}

// issueCoverage reads the issue's requirements and what serves each, for who: an issue below it
// who may not see serves nothing here (refs.IssueDescendants). The documents' blocks are read
// from the block index (document_blocks and block_refs), which every accepted write leaves in
// step with the document it wrote; the requirements' order and words come from the live spec,
// and each one's node state from the store (queueRequirementStates). The node states and the
// counts of what serves each requirement are read in one batch, one round trip, as the counts
// alone were before the states were read. Both statements start from the requirements the live
// spec marks, each through a primary key, so the plan Postgres makes without their values costs
// little and a connection soon keeps it rather than planning each read again
// (queueServingBlockCounts).
//
// trees is the reader of a request that answers the requirements' trees, nil for every other
// caller: each requirement registers with it (requirementBlocksIn), in the row the answer
// carries, the markdown typedBlocksIn reads the block's tree from, which is the rule GET
// /api/v1/reference-targets reads the same block's tree by, and the caller resolves it. The rows
// are filled in place, so each registered pointer is into the answer itself.
func (s *server) issueCoverage(ctx context.Context, q queryer, who model.Actor, issueKey string, trees *displayTreeReader) (model.IssueCoverage, error) {
	coverage := model.IssueCoverage{Issue: issueKey, Requirements: []model.RequirementCoverage{}, Gaps: []string{}}
	var specRefKey string
	err := q.QueryRow(ctx, `
		select coalesce((select a.id::text from artifacts a where a.issue_key = $1 and a.is_primary), ''),
		       coalesce((select a.ref_key from artifacts a where a.issue_key = $1 and a.is_primary), ''),
		       exists (select 1 from artifacts a where a.issue_key = $1 and `+aliasedPlanRole+`)
	`, issueKey).Scan(&coverage.SpecArtifactID, &specRefKey, &coverage.HasPlan)
	if err != nil {
		return model.IssueCoverage{}, fmt.Errorf("read the documents of %s: %w", issueKey, err)
	}
	if coverage.SpecArtifactID == "" {
		coverage.Gaps = []string{noRequirementsGap}
		return coverage, nil
	}
	requirements, err := s.requirementBlocksIn(ctx, coverage.SpecArtifactID, trees)
	if err != nil {
		return model.IssueCoverage{}, err
	}
	if len(requirements) == 0 {
		coverage.Gaps = []string{noRequirementsGap}
		return coverage, nil
	}
	descendants, err := refs.IssueDescendants(ctx, q, who, issueKey)
	if err != nil {
		return model.IssueCoverage{}, err
	}
	batch := &pgx.Batch{}
	states := make(map[string]string, len(requirements))
	served := map[string]servingCounts{}
	queueRequirementStates(batch, who, specRefKey, requirements, states)
	queueServingBlockCounts(batch, who, coverage.SpecArtifactID, requirements, append([]string{issueKey}, descendants...), served)
	if err := q.SendBatch(ctx, batch).Close(); err != nil {
		return model.IssueCoverage{}, err
	}
	for index := range requirements {
		requirement := &requirements[index]
		requirement.Ref, _ = text.FormatBlockRef(text.Ref{
			Kind: "block", IssueKey: issueKey, ID: "spec", Block: requirement.BlockID,
		})
		counts := served[requirement.BlockID]
		requirement.Checks, requirement.PlanTasks = counts.checks, counts.planTasks
		requirement.State = cmp.Or(states[requirement.BlockID], graphstore.StateLive)
		requirement.Gaps = []string{}
		// A proposed requirement is a question still open, never work left undone.
		if requirement.State != graphstore.StateProposed {
			if requirement.Checks == 0 {
				requirement.Gaps = append(requirement.Gaps, noCheckReason)
			}
			if coverage.HasPlan && requirement.PlanTasks == 0 {
				requirement.Gaps = append(requirement.Gaps, noPlanTaskReason)
			}
		}
		for _, reason := range requirement.Gaps {
			coverage.Gaps = append(coverage.Gaps, requirement.BlockID+" "+reason)
		}
	}
	coverage.Requirements = requirements
	return coverage, nil
}

// requirementStatesQuery reads, for a reader ($2), the node state the store holds of each
// requirement block whose node id is in $1 (`<ref_key>#<block id>`): each row of one the store
// holds, read by the primary key of `graph_nodes`, that passes the block kind's predicate over
// that row (refs.VisibleKindRow). It answers no row for a requirement the store holds no row of,
// and none for one the predicate hides from the reader.
var requirementStatesQuery = `
	select held.id, held.state
	from graph_nodes held
	where held.kind = '` + refs.StoredKindOf(refs.BlockNodeKind) + `' and held.id = any($1)
	  and ` + refs.VisibleKindRow(refs.BlockNodeKind, "held", "$2")

// queueRequirementStates queues, in b, the read of the node state of each requirement of the spec
// whose ref_key is refKey (requirementStatesQuery) for who, and fills states with each by block
// id once the batch is read. A requirement that query answers no row for reads live: one the store
// holds no row of, as every read of a node answers one (refs.StoredStateAndVisibility), and one
// the predicate hides from who, so who is told nothing of the state of a node they may not see.
func queueRequirementStates(b *pgx.Batch, who model.Actor, refKey string, requirements []model.RequirementCoverage, states map[string]string) {
	prefix := refKey + "#"
	ids := make([]string, len(requirements))
	for index, requirement := range requirements {
		ids[index] = prefix + requirement.BlockID
	}
	b.Queue(requirementStatesQuery, ids, refs.VisibilityArg(who)).Query(func(rows pgx.Rows) error {
		for rows.Next() {
			var id, state string
			if err := rows.Scan(&id, &state); err != nil {
				return fmt.Errorf("read the node state of the requirements of %s: %w", refKey, err)
			}
			states[strings.TrimPrefix(id, prefix)] = state
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read the node state of the requirements of %s: %w", refKey, err)
		}
		return nil
	})
}

// noCheckGap is the gap line a requirement with no check earns, written in one place: the
// route's gap list, which builds it from the requirement's own reason, and the approval
// request's answer (missingCheckGaps) carry the same words.
func noCheckGap(blockID string) string {
	return blockID + " " + noCheckReason
}

// missingCheckGaps is the gap lines an approval request's answer carries: one per requirement
// with no check, and never a missing plan task, since the plan comes after the approval. A
// requirement at proposed is a question still open and carries none, as the route's gaps carry
// none for it. A spec that marks no requirements carries the one line that says so, as the route
// does, so the approver is told that too; a spec every requirement of which has a check carries
// none.
func missingCheckGaps(coverage model.IssueCoverage) []string {
	if len(coverage.Requirements) == 0 {
		return []string{noRequirementsGap}
	}
	gaps := []string{}
	for _, requirement := range coverage.Requirements {
		if requirement.Checks == 0 && requirement.State != graphstore.StateProposed {
			gaps = append(gaps, noCheckGap(requirement.BlockID))
		}
	}
	return gaps
}

// requirementBlocksIn is every `requirement` block of one document, in document order, with its
// words, read live out of the document's markdown (typedBlocksIn). trees is the request's own
// reader, nil for a caller that asked for no tree: each requirement registers with it, as its
// text_tree, the markdown typedBlocksIn hands over from the block's stored text, so a requirement
// answers here the tree the batched reference read answers for it. It registers once the slice
// has stopped growing, since what is registered is a pointer into the row, and the caller fills
// in the rest of each row in place.
func (s *server) requirementBlocksIn(ctx context.Context, artifactID string, trees *displayTreeReader) ([]model.RequirementCoverage, error) {
	var requirements []model.RequirementCoverage
	var contents []string
	err := s.typedBlocksIn(ctx, artifactID, "requirement", trees != nil, func(node *pmdoc.Node, id, content string) {
		requirements = append(requirements, model.RequirementCoverage{BlockID: id, Text: blockWords(node)})
		if trees != nil {
			contents = append(contents, content)
		}
	})
	if err != nil {
		return nil, err
	}
	for index, content := range contents {
		trees.text(&requirements[index].TextTree, content)
	}
	return requirements, nil
}

// servingCounts is how many `check` and `plan_task` blocks serve one requirement.
type servingCounts struct {
	checks    int
	planTasks int
}

// queueServingBlockCounts queues, in b, the count, for each requirement of the spec, of the `check`
// and `plan_task` blocks of the documents of issues that name it in `serves`, and adds each to
// served once the batch is read. Both ends are current blocks of their documents: deleting the
// check deletes its row, so its requirement is uncovered again. A check or a plan task whose
// block's node state is proposed is a question still open and serves nothing yet
// (spec requirement r-proposed), so it is not counted; one at any other state is, and
// one the store holds no row of reads live, as every read of a node answers one. Its state is read
// for who in the same statement: from the store's row of the block (kind block, keyed by the row's
// node_id), through the block kind's predicate over that row (refs.VisibleKindRow), with the
// reader bound. So a block the predicate hides from who answers who no state and is counted as
// live, and the count tells who nothing of the state of a node who may not see.
//
// That test is a scalar subquery and not a `not exists`, so it stays a probe of the row per
// serving block, as the registry's own predicates are (refs.VisibleKind), rather than an anti-join
// the planner weighs against every other join of the statement. The state it probes for is the
// literal `proposed`, the predicate of the partial index graph_nodes_proposed
// (20261116172615_graph_nodes_proposed_index.up.sql), so the probe reads that index of the few
// proposed rows whatever plan Postgres holds.
//
// The statement starts from the requirements the live spec marks, read by the primary key of
// document_blocks (the spec's artifact and each requirement's block id). A requirement block of
// the indexed spec that the live spec does not mark is counted for nobody, which changes no
// answer: the route reads the counts of the live spec's requirements alone. Starting there is
// what lets Postgres keep one plan of the statement. pgx prepares it, and Postgres plans a
// prepared statement's first five executions on a connection with their values; from then on it
// uses the plan made without them once that plan's estimated cost is below the average cost of
// the plans it made with them on that connection, and keeps it. The plan made without the values
// prices each array at ten elements. Started from the spec's artifact alone, that plan took the
// spec for the average document of the store, thousands of blocks, so it cost more and every
// execution was planned again: on the 8,001-block shape of check c-read-cost, about 1.3 ms of
// planning a read against 0.5 ms of execution. Started from the requirements, it is kept once a
// connection's reads average about five requirements, and requirementStatesQuery's once they
// average about eight (measured on that shape).
func queueServingBlockCounts(b *pgx.Batch, who model.Actor, specArtifactID string, requirements []model.RequirementCoverage, issues []string, served map[string]servingCounts) {
	blockIDs := make([]string, len(requirements))
	for index, requirement := range requirements {
		blockIDs[index] = requirement.BlockID
	}
	b.Queue(servingBlockCountsQuery, specArtifactID, blockIDs, issues, refs.VisibilityArg(who)).Query(func(rows pgx.Rows) error {
		for rows.Next() {
			var blockID, blockType string
			var count int
			if err := rows.Scan(&blockID, &blockType, &count); err != nil {
				return fmt.Errorf("read what serves the requirements of %s: %w", specArtifactID, err)
			}
			counts := served[blockID]
			if blockType == "check" {
				counts.checks += count
			} else {
				counts.planTasks += count
			}
			served[blockID] = counts
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read what serves the requirements of %s: %w", specArtifactID, err)
		}
		return nil
	})
}

// servingBlockCountsQuery counts, for each requirement block $2 of the spec $1, the `check` and
// `plan_task` blocks of the documents of the issues $3 that serve it and that are not proposed for
// the reader $4 (queueServingBlockCounts).
var servingBlockCountsQuery = `
	select t.block_id, s.type, count(*)
	from document_blocks t
	join block_refs r on r.to_kind = 'block' and r.to_id = t.node_id
	join document_blocks s on s.artifact_id = r.artifact_id and s.block_id = r.block_id
	join artifacts a on a.id = r.artifact_id
	where t.artifact_id = $1::uuid and t.block_id = any($2) and t.type = 'requirement'
	  and r.kind = 'serves' and a.issue_key = any($3) and s.type in ('check', 'plan_task')
	  and not coalesce((
	      select true from graph_nodes serving
	      where serving.kind = '` + refs.StoredKindOf(refs.BlockNodeKind) + `' and serving.id = s.node_id
	        and serving.state = '` + graphstore.StateProposed + `'
	        and ` + refs.VisibleKindRow(refs.BlockNodeKind, "serving", "$4") + `), false)
	group by t.block_id, s.type`

// approvalCoverage is the coverage an approval request's answer carries for the document id
// names, read for who, the requester, and whether that coverage could not be read. The gaps are
// the issue's, when that document is the issue's spec (isSpecDocument), and none for any other
// document.
//
// It is read before the request opens its transaction. Reading it folds the issue's live spec,
// which for a document no room holds is a cold read bounded by a minute (docs/read_cache.go),
// and no owner lock is held across a read that long: four writers parked on one issue's row
// lock already cost a read of an unrelated issue the whole seventeen seconds the lock was held
// (store/pool.go). Nothing the lock protects is settled here - the coverage is a report the
// answer carries, and a gap refuses nothing.
//
// A coverage read that fails does not fail the request: it is logged here, the request is
// recorded as it was before the answer carried any coverage at all, and unread says the answer
// could not count the gaps rather than that it found none. Reading the document itself is the
// request's error, as it is inside the transaction.
func (s *server) approvalCoverage(ctx context.Context, q queryer, who model.Actor, id string) (gaps []string, unread bool, err error) {
	artifact, err := s.loadArtifact(ctx, q, id)
	if err != nil {
		return nil, false, err
	}
	if !isSpecDocument(artifact) {
		return []string{}, false, nil
	}
	coverage, err := s.issueCoverage(ctx, q, who, *artifact.IssueKey, nil)
	if err != nil {
		slog.Warn("dispatch: read the coverage an approval request reports", "artifact", artifact.ID, "error", err)
		return []string{}, true, nil
	}
	return missingCheckGaps(coverage), false, nil
}
