package record

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// MergeHistoryClass is what the record's rows show of an issue's merge: ANSWERED, PARTIAL or NONE.
// It is derived from the rows at each read (MergeEvidence.Class) and never stored, so no issue
// waits to be classified, and a row that moves to another issue or goes moves the class with it.
type MergeHistoryClass string

const (
	// MergeHistoryAnswered is positive merge evidence: the issue's merge snapshot, its retained pull
	// request row in state merged, or a production check row of a nonzero pull request, which only
	// a merge writes.
	MergeHistoryAnswered MergeHistoryClass = "ANSWERED"
	// MergeHistoryPartial is rows that bear on the merge and name the issue, none of them positive:
	// a pull request row that is not merged, a cleared pull request's clock, or a production check
	// of pull request 0. They are candidates of a merge read, and prove no merge either way.
	MergeHistoryPartial MergeHistoryClass = "PARTIAL"
	// MergeHistoryNone is no row that bears on the merge. Silence proves no never-merged work: a
	// clear before the snapshot was kept, or a rename, can have taken every row.
	MergeHistoryNone MergeHistoryClass = "NONE"
)

// MergeHistoryClasses are the three classes, in the order a census prints them.
var MergeHistoryClasses = []MergeHistoryClass{MergeHistoryAnswered, MergeHistoryPartial, MergeHistoryNone}

// MergeEvidencePullRequest is an issue's pull_requests row as the evidence reads it: its repository,
// number and state, and the base, final head and merge commit it holds, each empty where unread.
type MergeEvidencePullRequest struct {
	Repo     string
	Number   int
	State    PullRequestState
	Base     string
	Head     string
	MergeSHA string
}

// MergeEvidenceCheck is an issue's production_checks row as the evidence reads it, closed or open:
// the repository, the pull request number (0 for an assessment no merge opened) and the merge commit.
type MergeEvidenceCheck struct {
	Repo     string
	Number   int
	MergeSHA string
}

// MergeEvidenceClock is a cleared_pull_requests row whose issue column names the issue: the
// repository and number of a pull request row a clear deleted.
type MergeEvidenceClock struct {
	Repo   string
	Number int
}

// MergeEvidence is what one issue's rows show of its merge. Snapshot says the issue's merge_number
// is set, MergedRow that its pull_requests row is merged, and MergeOnlyCheck that its
// production_checks row names a nonzero pull request. OtherRows says a row bears on the merge and
// proves none: a pull request row that is not merged, a cleared clock the issue column gives it, or
// a production check of pull request 0. Merge is the known positive merge: the snapshot where
// Snapshot holds, else the merged row's fields, else the merge-only check's, and the zero Merge
// where none holds. A field no row holds stays empty. PullRequest, Check and Clocks are the rows
// themselves, nil or empty for none, which a merge read takes its candidates from.
type MergeEvidence struct {
	Snapshot       bool
	MergedRow      bool
	MergeOnlyCheck bool
	OtherRows      bool
	Merge          Merge
	PullRequest    *MergeEvidencePullRequest
	Check          *MergeEvidenceCheck
	Clocks         []MergeEvidenceClock
}

// Class is the merge history the evidence shows: ANSWERED where any positive evidence holds, else
// PARTIAL where another row bears on the merge, else NONE.
func (e MergeEvidence) Class() MergeHistoryClass {
	switch {
	case e.Snapshot || e.MergedRow || e.MergeOnlyCheck:
		return MergeHistoryAnswered
	case e.OtherRows:
		return MergeHistoryPartial
	}
	return MergeHistoryNone
}

// MergeHistoryRow is one issue's merge history as a read derives it: its key and repository, its
// evidence and class, its phase, whether a request stands on it (Requested: the issue is done and
// Dispatch shows it todo), and whether its known merge lacks the base or the final head that the
// branch of a next change starts from (MissingMetadata, ANSWERED alone).
type MergeHistoryRow struct {
	Key             string
	Repo            string
	Evidence        MergeEvidence
	Class           MergeHistoryClass
	Phase           phase.Phase
	Requested       bool
	MissingMetadata bool
}

// MergeHistoryOf reads one issue's merge history through the evidence projection (mergeHistory),
// and refuses a key the record holds no issue of.
func (s *Postgres) MergeHistoryOf(ctx context.Context, tx pgx.Tx, key string) (MergeHistoryRow, error) {
	rows, err := mergeHistory(ctx, tx, "i.key = $1", key)
	if err != nil {
		return MergeHistoryRow{}, fmt.Errorf("read the merge history of %s: %w", key, err)
	}
	if len(rows) == 0 {
		return MergeHistoryRow{}, fmt.Errorf("read the merge history of %s: the issue has no record", key)
	}
	return rows[0], nil
}

// MergeHistoryRows reads the merge history of each recorded issue of project through the same
// projection, in key order: of the issues on one of repos, compared without case, or of every
// issue of the project where repos is empty.
func (s *Postgres) MergeHistoryRows(ctx context.Context, tx pgx.Tx, project string, repos []string) ([]MergeHistoryRow, error) {
	lowered := make([]string, 0, len(repos))
	for _, repo := range repos {
		lowered = append(lowered, strings.ToLower(repo))
	}
	rows, err := mergeHistory(ctx, tx, "i.project = $1 and (cardinality($2::text[]) = 0 or lower(i.repo) = any($2::text[]))", project, lowered)
	if err != nil {
		return nil, fmt.Errorf("read the merge history of project %s: %w", project, err)
	}
	return rows, nil
}

// mergeHistory is the one evidence projection: each issue where selects, with args, in key order,
// one row an issue, derived from its rows at this read. It writes nothing.
func mergeHistory(ctx context.Context, tx pgx.Tx, where string, args ...any) ([]MergeHistoryRow, error) {
	rows, err := tx.Query(ctx, `select i.key, i.repo, i.phase, i.dispatch_status,
		i.merge_number, i.merge_head, i.merge_sha, i.merge_base, i.merge_repo, i.merged_at
		from issues i where `+where+` order by i.key`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var read []MergeHistoryRow
	for rows.Next() {
		var (
			row                        MergeHistoryRow
			phaseValue, dispatchStatus string
			number                     *int
			head, sha, base, repo      *string
			mergedAt                   *time.Time
		)
		if err := rows.Scan(&row.Key, &row.Repo, &phaseValue, &dispatchStatus, &number, &head, &sha, &base, &repo, &mergedAt); err != nil {
			return nil, err
		}
		if number != nil {
			row.Evidence.Snapshot = true
			row.Evidence.Merge = Merge{Number: *number, Head: textOf(head), SHA: textOf(sha), Base: textOf(base), Repo: textOf(repo),
				MergedAt: utcOrZero(mergedAt)}
		}
		row.Phase = phase.Phase(phaseValue)
		row.Class = row.Evidence.Class()
		row.Requested = row.Phase == phase.Done && dispatchStatus == "todo"
		row.MissingMetadata = row.Class == MergeHistoryAnswered && (row.Evidence.Merge.Head == "" || row.Evidence.Merge.Base == "")
		read = append(read, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return read, nil
}

// textOf is a nullable column's text, empty for null.
func textOf(text *string) string {
	if text == nil {
		return ""
	}
	return *text
}
