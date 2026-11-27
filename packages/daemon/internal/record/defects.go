package record

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// DefectKind is what a reason of a next change is: a check of the issue's Verification document
// that Dispatch computes as a fail, a production check's fail, or a defect of the change a role
// reported.
type DefectKind string

const (
	// DefectKindCheck is a check whose state Dispatch computes as a fail. Its check's state in the
	// newest read answers it: it holds nothing once that check passes.
	DefectKindCheck DefectKind = "check"
	// DefectKindProduction is a production check's fail. The production check of the newest merge
	// answers it.
	DefectKindProduction DefectKind = "production"
	// DefectKindDefect is a defect of the change a role reported (report_defect). It holds the
	// close until a merged change carries it or a withdrawal ends it.
	DefectKindDefect DefectKind = "defect"
)

// DefectClosedByPerson is the withdrawal reason of each defect still open when a person's done
// closed its issue (CloseDefectsByPerson): the person's word ended it, and no change carried it.
const DefectClosedByPerson = "closed_by_person"

// Defect is one reason of a next change of an issue, a row of defects. Generation is the
// generation of the issue it was recorded in. Role is the role that reported a defect, and empty
// for a reason the daemon read. Head is the head of the pull request the role saw the defect at,
// for a reported defect. Check is the block id of the check it is about: always for a check's
// reason, and where the role named one for a reported defect. Result is the id of the deciding
// result of a check's reason. CarriedBy is the generation of the change that carries it, 0 for
// none yet. MergedAt is when that change merged, WithdrawnAt when a withdrawal ended it, with its
// reason.
type Defect struct {
	ID             int64
	Issue          string
	Generation     uint64
	Kind           DefectKind
	Text           string
	Role           claim.Role
	Head           string
	Check          string
	Result         string
	ReportedAt     time.Time
	CarriedBy      uint64
	MergedAt       *time.Time
	WithdrawnAt    *time.Time
	WithdrawReason string
}

// Open says whether the defect still holds: no withdrawal ended it and no merged change carried
// it.
func (d Defect) Open() bool { return d.WithdrawnAt == nil && d.MergedAt == nil }

// CheckRead is one read of Dispatch's states of an issue's checks, as the close read stores it:
// the generation it was read for, the moment its row began its first Dispatch call (ReadAt), and
// what Dispatch answered, kept as the read's fact carried it (Answer: the results, the issue's
// children and the Verification document's approval). A read that failed has no answer and says
// why (Failure). RecordedAt is when the read was stored.
type CheckRead struct {
	Issue      string
	Generation uint64
	ReadAt     time.Time
	Failure    string
	Answer     json.RawMessage
	RecordedAt time.Time
}

const defectColumns = `id, issue, generation, kind, text, role, head, check_block, result_id, reported_at, coalesce(carried_by, 0),
	merged_at, withdrawn_at, withdraw_reason`

func (s *Postgres) PutDefect(ctx context.Context, tx pgx.Tx, defect Defect) (int64, error) {
	switch {
	case defect.Issue == "":
		return 0, errors.New("record a defect: it names the issue it is of")
	case strings.TrimSpace(defect.Text) == "":
		return 0, fmt.Errorf("record a defect of %s: it says what is wrong", defect.Issue)
	case defect.ReportedAt.IsZero():
		return 0, fmt.Errorf("record a defect of %s: it says when it was reported", defect.Issue)
	case defect.Generation > maxInt64 || defect.CarriedBy > maxInt64:
		return 0, fmt.Errorf("record a defect of %s: generation %d or %d does not fit a bigint", defect.Issue, defect.Generation, defect.CarriedBy)
	}
	var carriedBy any
	if defect.CarriedBy != 0 {
		carriedBy = int64(defect.CarriedBy)
	}
	var id int64
	if err := tx.QueryRow(ctx, `insert into defects (issue, generation, kind, text, role, head, check_block, result_id, reported_at, carried_by)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) returning id`,
		defect.Issue, int64(defect.Generation), string(defect.Kind), defect.Text, string(defect.Role), defect.Head, defect.Check,
		defect.Result, defect.ReportedAt, carriedBy).Scan(&id); err != nil {
		return 0, fmt.Errorf("record a %s defect of %s: %w", defect.Kind, defect.Issue, err)
	}
	return id, nil
}

func (s *Postgres) OpenDefects(ctx context.Context, tx pgx.Tx, issue string) ([]Defect, error) {
	rows, err := tx.Query(ctx, "select "+defectColumns+` from defects
		where issue = $1 and withdrawn_at is null and merged_at is null order by id`, issue)
	if err != nil {
		return nil, fmt.Errorf("read the open defects of %s: %w", issue, err)
	}
	defer rows.Close()
	open := []Defect{}
	for rows.Next() {
		defect, err := scanDefect(rows)
		if err != nil {
			return nil, fmt.Errorf("read the open defects of %s: %w", issue, err)
		}
		open = append(open, defect)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the open defects of %s: %w", issue, err)
	}
	return open, nil
}

func (s *Postgres) Defect(ctx context.Context, tx pgx.Tx, issue string, id int64) (*Defect, error) {
	defect, err := scanDefect(tx.QueryRow(ctx, "select "+defectColumns+" from defects where issue = $1 and id = $2", issue, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read defect %d of %s: %w", id, issue, err)
	}
	return &defect, nil
}

func (s *Postgres) WithdrawDefect(ctx context.Context, tx pgx.Tx, issue string, id int64, reason string, at time.Time) (bool, error) {
	if strings.TrimSpace(reason) == "" {
		return false, fmt.Errorf("withdraw defect %d of %s: a withdrawal says why", id, issue)
	}
	tag, err := tx.Exec(ctx, `update defects set withdrawn_at = $4, withdraw_reason = $3
		where issue = $1 and id = $2 and kind = $5 and withdrawn_at is null and merged_at is null`,
		issue, id, reason, at, string(DefectKindDefect))
	if err != nil {
		return false, fmt.Errorf("withdraw defect %d of %s: %w", id, issue, err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Postgres) CarryDefects(ctx context.Context, tx pgx.Tx, issue string, generation uint64, ids []int64) error {
	if generation == 0 || generation > maxInt64 {
		return fmt.Errorf("carry the defects %v of %s: generation %d is no generation of a change", ids, issue, generation)
	}
	if len(ids) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `update defects set carried_by = $2
		where issue = $1 and id = any($3) and withdrawn_at is null and merged_at is null`, issue, int64(generation), ids); err != nil {
		return fmt.Errorf("carry the defects %v of %s by generation %d: %w", ids, issue, generation, err)
	}
	return nil
}

func (s *Postgres) MergeDefects(ctx context.Context, tx pgx.Tx, issue string, generation uint64, at time.Time) error {
	if generation > maxInt64 {
		return fmt.Errorf("mark the defects of %s merged: generation %d does not fit a bigint", issue, generation)
	}
	if _, err := tx.Exec(ctx, `update defects set merged_at = $3
		where issue = $1 and carried_by = $2 and withdrawn_at is null and merged_at is null`, issue, int64(generation), at); err != nil {
		return fmt.Errorf("mark the defects generation %d of %s carried merged: %w", generation, issue, err)
	}
	return nil
}

func (s *Postgres) CloseDefectsByPerson(ctx context.Context, tx pgx.Tx, issue string, at time.Time) (int64, error) {
	tag, err := tx.Exec(ctx, `update defects set withdrawn_at = $2, withdraw_reason = $3
		where issue = $1 and withdrawn_at is null and merged_at is null`, issue, at, DefectClosedByPerson)
	if err != nil {
		return 0, fmt.Errorf("mark the open defects of %s closed by a person: %w", issue, err)
	}
	return tag.RowsAffected(), nil
}

func scanDefect(row scanner) (Defect, error) {
	var defect Defect
	var generation, carriedBy int64
	var kind, role string
	var mergedAt, withdrawnAt *time.Time
	if err := row.Scan(&defect.ID, &defect.Issue, &generation, &kind, &defect.Text, &role, &defect.Head, &defect.Check, &defect.Result,
		&defect.ReportedAt, &carriedBy, &mergedAt, &withdrawnAt, &defect.WithdrawReason); err != nil {
		return Defect{}, err
	}
	if generation < 0 || carriedBy < 0 {
		return Defect{}, fmt.Errorf("defect %d of %s has a negative generation", defect.ID, defect.Issue)
	}
	defect.Generation, defect.CarriedBy = uint64(generation), uint64(carriedBy)
	defect.Kind, defect.Role = DefectKind(kind), claim.Role(role)
	defect.ReportedAt = defect.ReportedAt.UTC()
	defect.MergedAt, defect.WithdrawnAt = utcOrNil(mergedAt), utcOrNil(withdrawnAt)
	return defect, nil
}

func (s *Postgres) PutCheckRead(ctx context.Context, tx pgx.Tx, read CheckRead) (bool, error) {
	switch {
	case read.Issue == "":
		return false, errors.New("store a read of the checks: it names the issue it read")
	case read.ReadAt.IsZero() || read.RecordedAt.IsZero():
		return false, fmt.Errorf("store a read of the checks of %s: it says when it began and when it was stored", read.Issue)
	case (read.Failure == "") == (len(read.Answer) == 0):
		return false, fmt.Errorf("store a read of the checks of %s: a read holds Dispatch's answer or says why it failed, and not both", read.Issue)
	case read.Generation > maxInt64:
		return false, fmt.Errorf("store a read of the checks of %s: generation %d does not fit a bigint", read.Issue, read.Generation)
	}
	var answer any
	if len(read.Answer) > 0 {
		answer = []byte(read.Answer)
	}
	tag, err := tx.Exec(ctx, `insert into close_reads (issue, generation, read_at, failure, answer, recorded_at)
		values ($1, $2, $3, $4, $5, $6)
		on conflict (issue) do update set generation = excluded.generation, read_at = excluded.read_at, failure = excluded.failure,
			answer = excluded.answer, recorded_at = excluded.recorded_at
		where close_reads.read_at <= excluded.read_at`,
		read.Issue, int64(read.Generation), read.ReadAt, read.Failure, answer, read.RecordedAt)
	if err != nil {
		return false, fmt.Errorf("store a read of the checks of %s: %w", read.Issue, err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Postgres) NewestCheckRead(ctx context.Context, tx pgx.Tx, issue string) (*CheckRead, error) {
	read := CheckRead{Issue: issue}
	var generation int64
	var answer []byte
	err := tx.QueryRow(ctx, `select generation, read_at, failure, answer, recorded_at from close_reads where issue = $1`, issue).
		Scan(&generation, &read.ReadAt, &read.Failure, &answer, &read.RecordedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the newest read of the checks of %s: %w", issue, err)
	}
	if generation < 0 {
		return nil, fmt.Errorf("the read of the checks of %s has negative generation %d", issue, generation)
	}
	read.Generation = uint64(generation)
	read.ReadAt, read.RecordedAt = read.ReadAt.UTC(), read.RecordedAt.UTC()
	if len(answer) > 0 {
		read.Answer = json.RawMessage(answer)
	}
	return &read, nil
}
