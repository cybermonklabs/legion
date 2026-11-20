package api

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// results_rows.go holds the two tables of results: check_results, a role's, and
// ci_check_results, a CI job's. It holds their column lists, the one writer of each
// (insertCheckResult, insertCIJobCheckResult), and the one read that loads both for a
// Verification document (loadCheckResults), in one shape (scanCheckResult).

// checkResultColumns is the column list every check_results read scans, in scanCheckResult's
// order. check_results holds what a role recorded, so a row names no branch and no statement.
// commit_repo is the repository other than the check's issue's that the record route took the
// row's commit to be of (model.CheckResult.CommitRepo), null for every other row.
const checkResultColumns = `id::text, issue_key, check_issue_key, check_artifact_id::text, check_block_id,
	check_version, check_sha256, role, surface, run, observed, commit_sha, negative_control,
	failed, skipped, excluded, verdict, screenshots, session, actor, created_at, event_seq,
	''::text, null::jsonb, commit_repo`

// ciCheckResultColumns is the same column list read from ci_check_results, which holds what a CI
// job recorded (insertCIJobCheckResult): its role is ciResultRole, a job counts no case and
// attaches no screenshot, and every row names its branch and its statement.
const ciCheckResultColumns = `id::text, issue_key, check_issue_key, check_artifact_id::text, check_block_id,
	check_version, check_sha256, '` + ciResultRole + `'::text, surface, run, observed, commit_sha, negative_control,
	0, 0, 0, verdict, '{}'::text[], session, actor, created_at, event_seq,
	branch, attestation, commit_repo`

// insertCheckResult writes one role's result as a row of check_results and fills in the
// created_at the row takes, which the record route answers with: the pointer is the caller's own
// value, not a copy of it.
func insertCheckResult(ctx context.Context, tx pgx.Tx, result *model.CheckResult) error {
	actorJSON, err := encodeJSON(result.Actor)
	if err != nil {
		return err
	}
	return tx.QueryRow(ctx, `
		insert into check_results (
			id, issue_key, check_issue_key, check_artifact_id, check_block_id, check_version,
			check_sha256, role, surface, run, observed, commit_sha, negative_control,
			failed, skipped, excluded, verdict, screenshots, session, actor, event_seq, commit_repo
		)
		values ($1::uuid, $2, $3, $4::uuid, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22)
		returning created_at
	`,
		result.ID, result.IssueKey, result.CheckIssueKey, result.CheckArtifactID, result.CheckBlockID,
		result.CheckVersion, result.CheckSHA256, result.Role, result.Surface, result.Run, result.Observed,
		result.CommitSHA, result.NegativeControl, result.Failed, result.Skipped, result.Excluded,
		result.Verdict, result.Screenshots, result.Session, actorJSON, result.EventSeq, result.CommitRepo,
	).Scan(&result.CreatedAt)
}

// insertCIJobCheckResult is the one writer of ci_check_results: it writes one CI job's result
// (ciJobCheckResult) with its statement, whose recorded_at is the row's own created_at
// (stampedAttestation), and fills in that created_at and the stored statement, so the record's
// answer is what the read answers. The table is the job's alone, and no build before this one
// reads it, so no job's result ever counts as a role's record of its check.
func insertCIJobCheckResult(ctx context.Context, tx pgx.Tx, result *model.CheckResult) error {
	actorJSON, err := encodeJSON(result.Actor)
	if err != nil {
		return err
	}
	if result.Attestation == nil {
		return fmt.Errorf("store a CI job's result of check %s: it carries no statement", result.CheckBlockID)
	}
	attestation, err := json.Marshal(result.Attestation)
	if err != nil {
		return fmt.Errorf("encode the statement of a result: %w", err)
	}
	var stored []byte
	if err := tx.QueryRow(ctx, `
		insert into ci_check_results (
			id, issue_key, check_issue_key, check_artifact_id, check_block_id, check_version,
			check_sha256, run, surface, negative_control, observed, commit_sha, branch, verdict,
			attestation, session, actor, event_seq, commit_repo
		)
		values ($1::uuid, $2, $3, $4::uuid, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
			`+stampedAttestation("$15")+`, $16, $17, $18, $19)
		returning created_at, attestation
	`,
		result.ID, result.IssueKey, result.CheckIssueKey, result.CheckArtifactID, result.CheckBlockID,
		result.CheckVersion, result.CheckSHA256, result.Run, result.Surface, result.NegativeControl,
		result.Observed, result.CommitSHA, result.Branch, result.Verdict, attestation,
		result.Session, actorJSON, result.EventSeq, result.CommitRepo,
	).Scan(&result.CreatedAt, &stored); err != nil {
		return err
	}
	return decodeAttestation(stored, &result.Attestation)
}

// loadCheckResults is every result recorded against the checks of one Verification document,
// from any issue, by the block id of the check each names, newest first within each: a role's
// from check_results and a CI job's from ci_check_results, in one list. One query serves the
// whole document: a Verification document holding twenty checks is one round trip, not twenty.
func loadCheckResults(ctx context.Context, q queryer, artifactID string) (map[string][]model.CheckResult, error) {
	rows, err := q.Query(ctx, `
		select `+checkResultColumns+`
		from check_results
		where check_artifact_id = $1::uuid
		union all
		select `+ciCheckResultColumns+`
		from ci_check_results
		where check_artifact_id = $1::uuid
		order by check_block_id, created_at desc, id desc
	`, artifactID)
	if err != nil {
		return nil, fmt.Errorf("read the results of document %s: %w", artifactID, err)
	}
	defer rows.Close()
	results := map[string][]model.CheckResult{}
	for rows.Next() {
		result, err := scanCheckResult(rows)
		if err != nil {
			return nil, fmt.Errorf("read the results of document %s: %w", artifactID, err)
		}
		results[result.CheckBlockID] = append(results[result.CheckBlockID], result)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the results of document %s: %w", artifactID, err)
	}
	return results, nil
}

func scanCheckResult(row pgx.Row) (model.CheckResult, error) {
	var result model.CheckResult
	var actorJSON, attestationJSON []byte
	if err := row.Scan(
		&result.ID, &result.IssueKey, &result.CheckIssueKey, &result.CheckArtifactID, &result.CheckBlockID,
		&result.CheckVersion, &result.CheckSHA256, &result.Role, &result.Surface, &result.Run,
		&result.Observed, &result.CommitSHA, &result.NegativeControl, &result.Failed, &result.Skipped,
		&result.Excluded, &result.Verdict, &result.Screenshots, &result.Session, &actorJSON,
		&result.CreatedAt, &result.EventSeq, &result.Branch, &attestationJSON, &result.CommitRepo,
	); err != nil {
		return model.CheckResult{}, err
	}
	if err := json.Unmarshal(actorJSON, &result.Actor); err != nil {
		return model.CheckResult{}, fmt.Errorf("decode a result's actor: %w", err)
	}
	if err := decodeAttestation(attestationJSON, &result.Attestation); err != nil {
		return model.CheckResult{}, err
	}
	return result, nil
}

// decodeAttestation reads a stored statement into *into: nil for a null one, which every role's
// row reads as (checkResultColumns).
func decodeAttestation(stored []byte, into **model.ResultAttestation) error {
	if stored == nil {
		*into = nil
		return nil
	}
	statement := new(model.ResultAttestation)
	if err := json.Unmarshal(stored, statement); err != nil {
		return fmt.Errorf("decode a result's statement: %w", err)
	}
	*into = statement
	return nil
}
