package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// coverage.go answers what each requirement of an issue's spec lacks. The requirements are the
// `requirement` nodes of the issue's tree in the tree's own order (issueCoverageOverTuples). A
// requirement has a check when a `check` node of that issue or of any descendant issue, at any
// status, names it in `serves`, and a plan task when a `plan_task` node of that set does; the
// descendants are read with refs.IssueDescendants, the server's one recursive `child_of` subtree
// walk. The missing-plan-task gap is named only once the issue has a plan, since the plan comes
// after the spec's approval. A requirement whose node state is proposed is a question still open
// (spec requirement r-proposed), so it raises neither gap here and the approval request's answer
// names none for it; one at settled or live is counted as every requirement is, and one at
// overtaken is closed with its subtree (R7 of the issue-tree spec), so it raises neither gap
// either. A check or a plan task counts where it is live: one whose node is proposed is a question
// still open and one at overtaken is closed, so neither serves anything and its requirement is
// counted without it, while a live one counts whatever state a node above it holds
// (readServingChecks; INFRA-1253's architect,
// dispatch://INFRA-1253/comment/c797249f-ab6c-4b52-a848-724932e12668). An issue that is a
// view on its ancestor's spec (docs.ViewOf) holds no requirement of its own: its coverage lists the
// requirements of that ancestor's tree that a check of the view, or of an issue under it, serves,
// each counted over what the view and the issues under it hold. A requirement node of the view's
// own tree - one that reached it through its room, as a block a person pasted in the browser,
// since every write route refuses one - is listed as no requirement of the view and raises no
// missing check or plan task: it is named in the gaps alone, as a requirement on a view, so it is
// moved into the ancestor's spec. While a document of the issue owes its block index the counts
// may not hold what serves a requirement yet, so no requirement lacks a check or a plan task then:
// the gaps name each such document instead (owedGap, spec requirement r-owed-coverage). Nothing is
// ever refused for a gap: three readers show it — the route below for the Spec tab, `dispatch read
// --issue`, which prints each gap after `Coverage: `, and an approval request's answer, which
// names the missing checks alone, and the owed documents in their place.

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
	coverage, err := s.issueCoverageOverTuples(r.Context(), s.deps.Store.Pool, who, issueKey, trees)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	trees.resolve()
	WriteJSON(w, http.StatusOK, coverage)
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

// noCheckGap is the gap line a requirement with no check earns, written in one place: the
// route's gap list, which builds it from the requirement's own reason, and the approval
// request's answer (missingCheckGaps) carry the same words.
func noCheckGap(blockID string) string {
	return blockID + " " + noCheckReason
}

// missingCheckGaps is the gap lines an approval request's answer carries: one per requirement
// with no check, as its own gaps say (coverTreeRequirements), and never a missing plan task, since the
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

// approvalCoverage is the coverage an approval request's answer carries for the document id
// names, read for who, the requester, and whether that coverage could not be read. The gaps are
// the issue's, when that document is the issue's spec (isSpecDocument), and none for any other
// document: its missing checks, or while a document of the issue owes its block index, that
// document's line in their place (missingCheckGaps).
// It is read before the request opens its transaction, so no owner lock is held across it. Nothing
// the lock protects is settled here - the coverage is a report the answer carries, and a gap
// refuses nothing.
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
	coverage, owedGaps, err := s.issueCoverageOverTuplesAndOwedGaps(ctx, q, who, *artifact.IssueKey, nil)
	if err != nil {
		slog.Warn("dispatch: read the coverage an approval request reports", "artifact", artifact.ID, "error", err)
		return []string{}, true, nil
	}
	return missingCheckGaps(coverage, owedGaps), false, nil
}
