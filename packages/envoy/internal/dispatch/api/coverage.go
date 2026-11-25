package api

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net/http"
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

// coverage.go answers what each requirement of an issue's spec lacks. The requirements are the
// `requirement` blocks of the issue's spec, in document order. A requirement has a check when a
// `check` block of any document of that issue or of any descendant issue, at any status, names
// it in `serves`, and a plan task when a `plan_task` block of that set does; the descendants are
// read with refs.IssueDescendants, the server's one recursive `child_of` subtree walk. The
// missing-plan-task gap is named only once the issue has a plan document, since the plan comes
// after the spec's approval. A requirement whose block's node state is proposed is a question
// still open (spec requirement r-proposed), so it raises neither gap here and the
// approval request's answer names none for it; one at settled or live is counted as every
// requirement is, and one at overtaken is closed (R7 of dispatch://INFRA-1253), so it raises
// neither gap either. A check or a plan task
// whose block is proposed is a question still open too, so it serves nothing yet and its
// requirement is counted without it (queueServingBlockCounts). An issue that is a view on its
// ancestor's spec (docs.ViewOf) holds no requirement of its own: its coverage lists the
// requirements of that ancestor's spec that a check of the view, or of an issue under it, serves,
// each counted over what the view and the issues under it hold (viewCoverage). A requirement block
// of the view's own spec - one that reached it through its room, as a block a person pasted in the
// browser, since every write route refuses one - is listed as no requirement of the view and
// raises no missing check or plan task: it is named in the gaps alone, as a requirement on a view,
// so it is moved into the ancestor's spec. While a document of the issue owes its block index the
// counts may not hold what serves a requirement yet, so no requirement lacks a check or a plan task
// then: the gaps name each such document instead (owedGap, spec requirement r-owed-coverage).
// Nothing is ever refused for a gap: three readers show it — the route below for the Spec tab,
// `dispatch read --issue`, which prints each gap after `Coverage: `, and an approval request's
// answer, which names the missing checks alone, and the owed documents in their place.

// noRequirementsGap is the one gap line a spec that marks no requirement answers, in place of a
// line per requirement. noCheckReason and noPlanTaskReason are what one requirement lacks, as
// RequirementCoverage.Gaps carries it, and viewRequirementReason what a requirement block of a
// view is; a document-level gap line is the requirement's block id and one of these, which is
// what noCheckGap builds.
const (
	noRequirementsGap     = "the spec marks no requirements"
	noCheckReason         = "has no check"
	noPlanTaskReason      = "has no plan task"
	viewRequirementReason = "is a requirement on a view"
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
// alone were before the states were read. A spec edited in its issue's room has its blocks' rows
// and nodes keyed by the room's tree artifact (refs.DocumentRoomSQL), so they are read there.
// Both statements start from the requirements the live spec marks, each through a primary key,
// so the plan Postgres makes without their values costs little and a connection soon keeps it
// rather than planning each read again (queueServingBlockCounts). The issue's documents, whether it
// is a view and which of its documents owe their block index are read in one batch too
// (readCoverageSpecAndView), and an issue that is a view (docs.ViewOf) answers the coverage
// viewCoverage reads. The answer names each document that owes its index (OwedDocuments), whose
// checks and requirements the counts may not hold yet, whether the issue is a view or not; while
// one does, no requirement lacks a check or a plan task (coverRequirements), and the gaps hold one
// line for each such document in place of those (owedGap).
//
// trees is the reader of a request that answers the requirements' trees, nil for every other
// caller: each requirement the answer lists registers with it (registerRequirementTrees), in the
// row the answer carries, the markdown typedBlocksIn reads the block's tree from, which is the
// rule GET /api/v1/reference-targets reads the same block's tree by, and the caller resolves it.
// The rows are filled in place, so each registered pointer is into the answer itself.
func (s *server) issueCoverage(ctx context.Context, q queryer, who model.Actor, issueKey string, trees *displayTreeReader) (model.IssueCoverage, error) {
	coverage, _, err := s.issueCoverageAndOwedGaps(ctx, q, who, issueKey, trees)
	return coverage, err
}

// issueCoverageAndOwedGaps is issueCoverage's answer and, apart, the gap line of each document of
// the issue that owes its block index (owedGap), which the answer's gaps hold wherever it covers a
// requirement and an approval request's answer carries in place of the missing checks
// (approvalCoverage).
func (s *server) issueCoverageAndOwedGaps(ctx context.Context, q queryer, who model.Actor, issueKey string, trees *displayTreeReader) (model.IssueCoverage, []string, error) {
	coverage := model.IssueCoverage{Issue: issueKey, Requirements: []model.RequirementCoverage{}, Gaps: []string{}}
	spec, view, err := readCoverageSpecAndView(ctx, q, issueKey)
	if err != nil {
		return model.IssueCoverage{}, nil, err
	}
	coverage.SpecArtifactID, coverage.HasPlan, coverage.OwedDocuments = spec.artifactID, spec.hasPlan, spec.owed
	if spec.artifactID == "" {
		coverage.Gaps = []string{noRequirementsGap}
		return coverage, spec.owedGaps, nil
	}
	if view != "" {
		coverage, err = s.viewCoverage(ctx, q, who, coverage, view, spec.owedGaps, trees)
		return coverage, spec.owedGaps, err
	}
	requirements, contents, err := s.requirementBlocksIn(ctx, spec.artifactID, trees != nil)
	if err != nil {
		return model.IssueCoverage{}, nil, err
	}
	if len(requirements) == 0 {
		coverage.Gaps = []string{noRequirementsGap}
		return coverage, spec.owedGaps, nil
	}
	issues, err := coverageIssues(ctx, q, who, issueKey)
	if err != nil {
		return model.IssueCoverage{}, nil, err
	}
	if err := coverRequirements(ctx, q, who, issueKey, spec, requirements, issues, coverage.HasPlan, len(spec.owed) > 0); err != nil {
		return model.IssueCoverage{}, nil, err
	}
	coverage.Requirements = requirements
	coverage.Gaps = append(requirementGaps(requirements), spec.owedGaps...)
	registerRequirementTrees(trees, coverage.Requirements, contents)
	return coverage, spec.owedGaps, nil
}

// viewCoverage is the coverage of the view coverage.Issue on the spec of ancestor (docs.ViewOf),
// read for who, coverage holding the view's spec and whether it has a plan: the requirements of
// the ancestor's spec, in that spec's order, that a check of the view or of an issue under it
// serves, each counted over what the view and the issues under it hold, as issueCoverage counts
// the requirements of an issue that is no view; then owedGaps, the line of each document of the
// view that owes its block index, which leaves no listed requirement lacking a plan task
// (coverRequirements); then, for each requirement block of the view's own spec, the one line
// `<block id> is a requirement on a view` in the gaps and nothing else: such a block is listed as
// no requirement of the view and lacks no check and no plan task. A view that lists no requirement
// and holds none answers the single gap line.
func (s *server) viewCoverage(ctx context.Context, q queryer, who model.Actor, coverage model.IssueCoverage, ancestor string, owedGaps []string, trees *displayTreeReader) (model.IssueCoverage, error) {
	above, err := readCoverageSpec(ctx, q, ancestor)
	if err != nil {
		return model.IssueCoverage{}, err
	}
	var contents []string
	if above.artifactID != "" {
		requirements, texts, err := s.requirementBlocksIn(ctx, above.artifactID, trees != nil)
		if err != nil {
			return model.IssueCoverage{}, err
		}
		issues, err := coverageIssues(ctx, q, who, coverage.Issue)
		if err != nil {
			return model.IssueCoverage{}, err
		}
		if err := coverRequirements(ctx, q, who, ancestor, above, requirements, issues, coverage.HasPlan, len(owedGaps) > 0); err != nil {
			return model.IssueCoverage{}, err
		}
		for index, requirement := range requirements {
			if requirement.Checks == 0 {
				continue
			}
			coverage.Requirements = append(coverage.Requirements, requirement)
			if texts != nil {
				contents = append(contents, texts[index])
			}
		}
	}
	pasted, _, err := s.requirementBlocksIn(ctx, coverage.SpecArtifactID, false)
	if err != nil {
		return model.IssueCoverage{}, err
	}
	coverage.Gaps = append(requirementGaps(coverage.Requirements), owedGaps...)
	for _, requirement := range pasted {
		coverage.Gaps = append(coverage.Gaps, requirement.BlockID+" "+viewRequirementReason)
	}
	if len(coverage.Requirements) == 0 && len(pasted) == 0 {
		coverage.Gaps = []string{noRequirementsGap}
	}
	registerRequirementTrees(trees, coverage.Requirements, contents)
	return coverage, nil
}

// coverageSpec is what the coverage read reads of one issue's documents: its spec's id, the
// ref_key its blocks' nodes are keyed under - the spec's own, or its room's where it is edited in
// its issue's room (refs.DocumentRoomSQL) - and whether the issue has a plan document. An issue
// with no spec holds "" in both. Where readCoverageSpecAndView reads it, owed holds each document
// of the issue that owes its block index and owedGaps that document's gap line at the same index
// (queueOwedDocumentGaps); readCoverageSpec, which reads the spec of a view's ancestor, reads
// neither.
type coverageSpec struct {
	artifactID, nodeKey string
	hasPlan             bool
	owed                []model.OwedDocument
	owedGaps            []string
}

// coverageSpecStatement reads the coverageSpec of the issue $1: its spec's id, the ref_key its
// blocks' nodes are keyed under and whether it has a plan document.
var coverageSpecStatement = `
		select coalesce((select a.id::text from artifacts a where a.issue_key = $1 and a.is_primary), ''),
		       coalesce((select keyed.ref_key from artifacts a join artifacts keyed on keyed.id = ` + refs.DocumentRoomSQL("a.id") + `
		                 where a.issue_key = $1 and a.is_primary), ''),
		       exists (select 1 from artifacts a where a.issue_key = $1 and ` + aliasedPlanRole + `)
	`

// readCoverageSpec is the coverageSpec of the issue issueKey, read through q.
func readCoverageSpec(ctx context.Context, q queryer, issueKey string) (coverageSpec, error) {
	var spec coverageSpec
	if err := q.QueryRow(ctx, coverageSpecStatement, issueKey).Scan(&spec.artifactID, &spec.nodeKey, &spec.hasPlan); err != nil {
		return coverageSpec{}, fmt.Errorf("read the documents of %s: %w", issueKey, err)
	}
	return spec, nil
}

// readCoverageSpecAndView is readCoverageSpec of the issue issueKey, with the documents of the
// issue that owe their block index and their gap lines (queueOwedDocumentGaps), and the issue it
// is a view of (docs.ViewOf), "" where it is none, read through q in one batch, one round trip:
// the coverage read of an issue reads them before anything else, and an approval request of a spec
// reads that coverage, so neither the view nor the owed documents cost the request a round trip
// of its own. The view is read for an issue with no spec too, whose coverage reads nothing of it.
func readCoverageSpecAndView(ctx context.Context, q queryer, issueKey string) (coverageSpec, string, error) {
	var spec coverageSpec
	var view string
	batch := &pgx.Batch{}
	batch.Queue(coverageSpecStatement, issueKey).QueryRow(func(row pgx.Row) error {
		if err := row.Scan(&spec.artifactID, &spec.nodeKey, &spec.hasPlan); err != nil {
			return fmt.Errorf("read the documents of %s: %w", issueKey, err)
		}
		return nil
	})
	docs.QueueViewOf(batch, issueKey, &view)
	queueOwedDocumentGaps(batch, issueKey, &spec.owed, &spec.owedGaps)
	if err := q.SendBatch(ctx, batch).Close(); err != nil {
		return coverageSpec{}, "", err
	}
	return spec, view, nil
}

// owedDocumentsStatement reads each document of the issue $1 - each artifact it holds, its tree
// artifact among them - that owes its block index by either mark the block-index reconciliation
// lists a document by (docs' owedDocuments): its document_block_versions.version null, or
// store_owed_at set, with the number of its newest markdown version, 0 where it holds none, and
// its name, in the order the documents were made.
const owedDocumentsStatement = `
	select a.id::text, coalesce((select max(v.number) from artifact_versions v where v.artifact_id = a.id and v.markdown is not null), 0), a.name
	from artifacts a
	join document_block_versions b on b.artifact_id = a.id
	where a.issue_key = $1 and (b.version is null or b.store_owed_at is not null)
	order by a.created_at, a.id`

// queueOwedDocuments queues, in b, the read of the documents of the issue issueKey that owe their
// block index (owedDocumentsStatement), and fills owed with them once the batch is read, nil where
// none owes one, so the answer carries no member (model.OwedDocument). The tree read takes it, and
// the coverage read takes it with each document's gap line (queueOwedDocumentGaps), each in a
// batch of reads it makes anyway, so the member costs neither a round trip.
func queueOwedDocuments(b *pgx.Batch, issueKey string, owed *[]model.OwedDocument) {
	queueOwedDocumentGaps(b, issueKey, owed, nil)
}

// queueOwedDocumentGaps is queueOwedDocuments that fills gaps too, where it is not nil, with the
// gap line of each document it fills owed with, at the same index (owedGap). Where gaps is nil the
// document's name is not scanned at all.
func queueOwedDocumentGaps(b *pgx.Batch, issueKey string, owed *[]model.OwedDocument, gaps *[]string) {
	b.Queue(owedDocumentsStatement, issueKey).Query(func(rows pgx.Rows) error {
		var name string
		var named any // nil: pgx skips the column
		if gaps != nil {
			named = &name
		}
		for rows.Next() {
			var document model.OwedDocument
			if err := rows.Scan(&document.ArtifactID, &document.Version, named); err != nil {
				return fmt.Errorf("read the documents of %s that owe their block index: %w", issueKey, err)
			}
			*owed = append(*owed, document)
			if gaps != nil {
				*gaps = append(*gaps, owedGap(name, document.Version))
			}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read the documents of %s that owe their block index: %w", issueKey, err)
		}
		return nil
	})
}

// owedGap is the gap line of a document of the issue that owes its block index, named by its name
// and the version whose blocks wait (spec requirement r-owed-coverage). Until the block-index
// reconciliation indexes it, the counts of what serves each requirement may not hold what the
// document holds, so the coverage read and an approval request's answer write this line in place
// of every missing check and plan task, and draw no requirement as lacking one from an index that
// is owed.
func owedGap(name string, version int) string {
	return name + " version " + strconv.Itoa(version) + " is still being indexed; its coverage shows once that ends."
}

// coverageIssues is the issue issueKey and every issue below it that who may see
// (refs.IssueDescendants): the issues whose checks and plan tasks serve its coverage.
func coverageIssues(ctx context.Context, q queryer, who model.Actor, issueKey string) ([]string, error) {
	descendants, err := refs.IssueDescendants(ctx, q, who, issueKey)
	if err != nil {
		return nil, err
	}
	return append([]string{issueKey}, descendants...), nil
}

// coverRequirements fills in each of requirements, the requirement blocks of the spec of the issue
// issueKey, for who: its reference, its node's state (queueRequirementStates), how many checks and
// plan tasks of the documents of issues serve it (queueServingBlockCounts), both read in one batch,
// and its gaps. A proposed requirement is a question still open and lacks nothing, an overtaken one
// is closed (R7 of dispatch://INFRA-1253) and lacks nothing either, and so does every requirement
// while owed, while a document of the issue owes its block index, whose checks and plan tasks the
// counts may not hold yet (owedGap names that document instead); any other lacks a check where none
// serves it, and a plan task where hasPlan and none serves it.
func coverRequirements(ctx context.Context, q queryer, who model.Actor, issueKey string, spec coverageSpec, requirements []model.RequirementCoverage, issues []string, hasPlan, owed bool) error {
	batch := &pgx.Batch{}
	states := make(map[string]string, len(requirements))
	served := map[string]servingCounts{}
	queueRequirementStates(batch, who, spec.nodeKey, requirements, states)
	queueServingBlockCounts(batch, who, spec.artifactID, requirements, issues, served)
	if err := q.SendBatch(ctx, batch).Close(); err != nil {
		return err
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
		// A proposed requirement is a question still open, and an overtaken one closed, never work
		// left undone; and while a document owes its block index, what serves a requirement is not
		// counted yet.
		if requirement.State != graphstore.StateProposed && requirement.State != graphstore.StateOvertaken && !owed {
			if requirement.Checks == 0 {
				requirement.Gaps = append(requirement.Gaps, noCheckReason)
			}
			if hasPlan && requirement.PlanTasks == 0 {
				requirement.Gaps = append(requirement.Gaps, noPlanTaskReason)
			}
		}
	}
	return nil
}

// requirementGaps is the gap line of each gap of each of requirements, in their order: the
// requirement's block id and the reason.
func requirementGaps(requirements []model.RequirementCoverage) []string {
	gaps := []string{}
	for _, requirement := range requirements {
		for _, reason := range requirement.Gaps {
			gaps = append(gaps, requirement.BlockID+" "+reason)
		}
	}
	return gaps
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
// whose blocks' nodes are keyed under refKey (requirementStatesQuery) for who - the spec's own
// ref_key, or its room's where the spec is edited in its issue's room - and fills states with each
// by block id once the batch is read. A requirement that query answers no row for reads live: one
// the store holds no row of, as every read of a node answers one (refs.StoredStateAndVisibility),
// and one the predicate hides from who, so who is told nothing of the state of a node they may not
// see.
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
// with no check, as its own gaps say (coverRequirements), and never a missing plan task, since the
// plan comes after the approval; then owedGaps, the line of each document of the issue that owes
// its block index, which the route's gaps hold too and while which no requirement lacks a check. A
// requirement at proposed is a question still open and carries none, as the route's gaps carry
// none for it. A spec that marks no requirements carries the one line that says so, as the route
// does, so the approver is told that too; a spec every requirement of which has a check, with no
// document owed, carries none.
func missingCheckGaps(coverage model.IssueCoverage, owedGaps []string) []string {
	if len(coverage.Requirements) == 0 {
		return []string{noRequirementsGap}
	}
	gaps := []string{}
	for _, requirement := range coverage.Requirements {
		if slices.Contains(requirement.Gaps, noCheckReason) {
			gaps = append(gaps, noCheckGap(requirement.BlockID))
		}
	}
	return append(gaps, owedGaps...)
}

// requirementBlocksIn is every `requirement` block of one document, in document order, with its
// words, read live out of the document's markdown (typedBlocksIn), and, where withContents is set,
// each one's content beside it: the markdown typedBlocksIn hands over from the block's stored text,
// which registerRequirementTrees registers as the requirement's text_tree, so a requirement answers
// the tree the batched reference read answers for it.
func (s *server) requirementBlocksIn(ctx context.Context, artifactID string, withContents bool) ([]model.RequirementCoverage, []string, error) {
	var requirements []model.RequirementCoverage
	var contents []string
	err := s.typedBlocksIn(ctx, artifactID, "requirement", withContents, func(node *pmdoc.Node, id, content string) {
		requirements = append(requirements, model.RequirementCoverage{BlockID: id, Text: blockWords(node)})
		if withContents {
			contents = append(contents, content)
		}
	})
	if err != nil {
		return nil, nil, err
	}
	return requirements, contents, nil
}

// registerRequirementTrees registers with trees, the request's own reader, nil for a caller that
// asked for no tree, each of contents as the text_tree of the requirement at its index of
// requirements, the rows the answer carries. It registers once the rows have stopped growing,
// since what is registered is a pointer into the row.
func registerRequirementTrees(trees *displayTreeReader, requirements []model.RequirementCoverage, contents []string) {
	if trees == nil {
		return
	}
	for index, content := range contents {
		trees.text(&requirements[index].TextTree, content)
	}
}

// servingCounts is how many `check` and `plan_task` blocks serve one requirement.
type servingCounts struct {
	checks    int
	planTasks int
}

// queueServingBlockCounts queues, in b, the count, for each requirement of the spec, of the `check`
// and `plan_task` blocks of the documents of issues that name it in `serves`, and adds each to
// served once the batch is read. The spec's blocks are the rows of the room it is edited in whose
// document is the spec (refs.DocumentRoomSQL, refs.BlockDocumentSQL): its own rows where it is its
// own room. Both ends are current blocks of their documents: deleting the check deletes its row,
// so its requirement is uncovered again. A check or a plan task whose block's node state is
// proposed is a question still open and serves nothing yet (spec requirement r-proposed), so it is
// not counted; one at any other state is, and
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
// document_blocks (the artifact of the room the spec is edited in, and each requirement's block
// id). A requirement block of the indexed spec that the live spec does not mark is counted for
// nobody, which changes no answer: the route reads the counts of the live spec's requirements
// alone. Starting there is what lets Postgres keep one plan of the statement. pgx prepares it, and
// Postgres plans a prepared statement's first five executions on a connection with their values;
// from then on it uses the plan made without them once that plan's estimated cost is below the
// average cost of the plans it made with them on that connection, and keeps it. The plan made
// without the values prices each array at ten elements. Started from the spec's artifact alone,
// that plan took the spec for the average document of the store, thousands of blocks, so it cost
// more and every execution was planned again: on the 8,001-block shape of check c-read-cost, about
// 1.3 ms of planning a read against 0.5 ms of execution. Started from the requirements, it is kept
// once a connection's reads average about five requirements, and requirementStatesQuery's once
// they average about eight (measured on that shape).
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

// servingBlockCountsQuery counts, for each requirement block $2 of the spec $1, read in the room
// the spec is edited in (refs.DocumentRoomSQL, refs.BlockDocumentSQL), the `check` and `plan_task`
// blocks of the documents of the issues $3 that serve it and that are not proposed for the reader
// $4 (queueServingBlockCounts).
var servingBlockCountsQuery = `
	select t.block_id, s.type, count(*)
	from document_blocks t
	join block_refs r on r.to_kind = 'block' and r.to_id = t.node_id
	join document_blocks s on s.artifact_id = r.artifact_id and s.block_id = r.block_id
	join artifacts a on a.id = r.artifact_id
	where t.artifact_id = ` + refs.DocumentRoomSQL("$1::uuid") + ` and t.block_id = any($2) and t.type = 'requirement'
	  and ` + refs.BlockDocumentSQL("t") + ` = $1::uuid
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
// document: its missing checks, or while a document of the issue owes its block index, that
// document's line in their place (missingCheckGaps).
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
	coverage, owedGaps, err := s.issueCoverageAndOwedGaps(ctx, q, who, *artifact.IssueKey, nil)
	if err != nil {
		slog.Warn("dispatch: read the coverage an approval request reports", "artifact", artifact.ID, "error", err)
		return []string{}, true, nil
	}
	return missingCheckGaps(coverage, owedGaps), false, nil
}
