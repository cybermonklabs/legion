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
// one row an issue, derived from its rows at this read. It reads the issue's snapshot, its
// pull_requests row, its production_checks row and every cleared_pull_requests row its issue
// column names, each as the schema holds it (readMergeSchema), so it reads the record of the
// release before this one too, whose issues hold no snapshot: a column the catalog shows absent
// holds no fact. The clocks are aggregated by issue before the join, so an issue of many clocks is
// still one row. It writes nothing.
func mergeHistory(ctx context.Context, tx pgx.Tx, where string, args ...any) ([]MergeHistoryRow, error) {
	schema, err := readMergeSchema(ctx, tx)
	if err != nil {
		return nil, err
	}
	snapshotColumns := "null::integer, null::text, null::text, null::text, null::text, null::timestamptz"
	if schema.snapshot {
		snapshotColumns = "i.merge_number, i.merge_head, i.merge_sha, i.merge_base, i.merge_repo, i.merged_at"
	}
	checkColumns, checkJoin := "null::integer, null::text, null::text", ""
	if schema.checks {
		checkColumns, checkJoin = "pc.pr_number, pc.repo, pc.merge_sha", " left join production_checks pc on pc.issue = i.key"
	}
	clockColumns, clockJoin := "null::text[], null::integer[]", ""
	if schema.cleared {
		clockColumns, clockJoin = "cl.repos, cl.numbers", ` left join (select issue, array_agg(repo order by repo, number) as repos,
			array_agg(number order by repo, number) as numbers from cleared_pull_requests group by issue) cl on cl.issue = i.key`
	}
	rows, err := tx.Query(ctx, `select i.key, i.repo, i.phase, i.dispatch_status, `+snapshotColumns+`,
		pr.repo, pr.number, pr.state, pr.base, pr.head_sha, pr.merge_sha, `+checkColumns+`, `+clockColumns+`
		from issues i left join pull_requests pr on pr.issue = i.key`+checkJoin+clockJoin+`
		where `+where+` order by i.key`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var read []MergeHistoryRow
	for rows.Next() {
		var (
			row                                                              MergeHistoryRow
			phaseValue, dispatchStatus                                       string
			number, prNumber, checkNumber                                    *int
			head, sha, base, repo                                            *string
			prRepo, prState, prBase, prHead, prMergeSHA, checkRepo, checkSHA *string
			mergedAt                                                         *time.Time
			clockRepos                                                       []string
			clockNumbers                                                     []int
		)
		if err := rows.Scan(&row.Key, &row.Repo, &phaseValue, &dispatchStatus, &number, &head, &sha, &base, &repo, &mergedAt,
			&prRepo, &prNumber, &prState, &prBase, &prHead, &prMergeSHA, &checkNumber, &checkRepo, &checkSHA,
			&clockRepos, &clockNumbers); err != nil {
			return nil, err
		}
		var snapshotMerge *Merge
		if number != nil {
			snapshotMerge = &Merge{Number: *number, Head: textOf(head), SHA: textOf(sha), Base: textOf(base), Repo: textOf(repo),
				MergedAt: utcOrZero(mergedAt)}
		}
		var pullRequest *MergeEvidencePullRequest
		if prNumber != nil {
			pullRequest = &MergeEvidencePullRequest{Repo: textOf(prRepo), Number: *prNumber, State: PullRequestState(textOf(prState)),
				Base: textOf(prBase), Head: textOf(prHead), MergeSHA: textOf(prMergeSHA)}
		}
		var check *MergeEvidenceCheck
		if checkNumber != nil {
			check = &MergeEvidenceCheck{Repo: textOf(checkRepo), Number: *checkNumber, MergeSHA: textOf(checkSHA)}
		}
		if len(clockRepos) != len(clockNumbers) {
			return nil, fmt.Errorf("the clocks of %s read %d repositories and %d numbers", row.Key, len(clockRepos), len(clockNumbers))
		}
		var clocks []MergeEvidenceClock
		for i, clockRepo := range clockRepos {
			clocks = append(clocks, MergeEvidenceClock{Repo: clockRepo, Number: clockNumbers[i]})
		}
		row.Evidence = mergeEvidenceOf(snapshotMerge, pullRequest, check, clocks)
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

// mergeEvidenceOf is the evidence of an issue's rows: its snapshot, its pull request row, its
// production check row and its clocks, each nil or empty where it holds none. Positive evidence is
// the snapshot, a merged row, or a check of a nonzero pull request, and the known merge is the
// first of them that holds. Every other row bears on the merge and proves none (OtherRows).
func mergeEvidenceOf(snapshot *Merge, pullRequest *MergeEvidencePullRequest, check *MergeEvidenceCheck, clocks []MergeEvidenceClock) MergeEvidence {
	e := MergeEvidence{PullRequest: pullRequest, Check: check, Clocks: clocks, OtherRows: len(clocks) > 0}
	if snapshot != nil {
		e.Snapshot, e.Merge = true, *snapshot
	}
	if pullRequest != nil {
		if pullRequest.State == PullRequestMerged {
			e.MergedRow = true
			if !e.Merge.Merged() {
				e.Merge = Merge{Number: pullRequest.Number, Head: pullRequest.Head, SHA: pullRequest.MergeSHA, Base: pullRequest.Base, Repo: pullRequest.Repo}
			}
		} else {
			e.OtherRows = true
		}
	}
	if check != nil {
		if check.Number > 0 {
			e.MergeOnlyCheck = true
			if !e.Merge.Merged() {
				e.Merge = Merge{Number: check.Number, SHA: check.MergeSHA, Repo: check.Repo}
			}
		} else {
			e.OtherRows = true
		}
	}
	return e
}

// mergeSchema is what the catalog shows of the parts of the record the projection reads that a
// release before this one can lack: the merge snapshot's columns of issues (snapshot), and the
// tables cleared_pull_requests (cleared) and production_checks (checks).
type mergeSchema struct {
	snapshot, cleared, checks bool
}

// readMergeSchema reads the catalog before the projection reads the record, so a part the catalog
// shows absent is read as holding no row. pg_attribute and to_regclass answer whatever the reader
// may select. A failed read, and a catalog that shows no issues or pull_requests table, is an
// error: the record is unread, never a record of no merge.
func readMergeSchema(ctx context.Context, tx pgx.Tx) (mergeSchema, error) {
	var (
		schema               mergeSchema
		issues, pullRequests bool
	)
	if err := tx.QueryRow(ctx, `select to_regclass('issues') is not null, to_regclass('pull_requests') is not null,
		exists (select 1 from pg_attribute where attrelid = to_regclass('issues') and attname = 'merge_number' and attnum > 0 and not attisdropped),
		to_regclass('cleared_pull_requests') is not null, to_regclass('production_checks') is not null`).
		Scan(&issues, &pullRequests, &schema.snapshot, &schema.cleared, &schema.checks); err != nil {
		return mergeSchema{}, fmt.Errorf("read the record's catalog: %w", err)
	}
	if !issues || !pullRequests {
		return mergeSchema{}, fmt.Errorf("read the record's catalog: it shows no issues or pull_requests table, so the database holds no record")
	}
	return schema, nil
}

// textOf is a nullable column's text, empty for null.
func textOf(text *string) string {
	if text == nil {
		return ""
	}
	return *text
}
