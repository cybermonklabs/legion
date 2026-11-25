package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// AdmissionCapChange is one change of the admission cap the project's controller or the operator
// made while the daemon runs (dispatch://INFRA-5616), as
// migrations/20261124040825_admission_cap.up.sql defines its row: the cap it set, 0 for a clear,
// the caps the daemon admitted under before it and from it on, the deployment's ceiling then, who
// made it, in a pause's actor shape, why, and when. Project is the project's Dispatch key. ID is
// the row's, 0 before the row is written.
type AdmissionCapChange struct {
	ID        int64
	Project   string
	Cap       int
	CapBefore int
	CapAfter  int
	Ceiling   int
	By        PauseActor
	Reason    string
	At        time.Time
}

// Cleared is whether the change is a clear, which returns the project to the starting cap,
// admission_cap. The zero change, which AdmissionCapTx answers where no change was ever made, is
// one too.
func (c AdmissionCapChange) Cleared() bool { return c.Cap == 0 }

// AdmissionCapChangesShown is how many changes the state lists, newest first
// (AdmissionCapChangesTx). The table keeps every change, and the daemon's log names each.
const AdmissionCapChangesShown = 50

// AdmissionCapReasonMax is the most characters a change's reason holds, as the give-up discard's
// reason does: the state serves the reasons of the newest AdmissionCapChangesShown changes on every
// read, and Dispatch reads a bounded answer of the daemon.
const AdmissionCapReasonMax = 2048

// EffectiveAdmissionCap is the cap a pass admits under: start, the deployment's starting cap
// (legion.yaml's admission_cap, else LEGION_ADMISSION_CAP), where no cap set while the daemon
// runs stands; else the smaller of that cap and ceiling, the deployment's admission_ceiling. The
// smaller of the two, and not the cap set, because the routes refuse only a cap above the ceiling
// of their own boot, and a later configuration may have lowered the ceiling under it. standing is
// the project's newest change (AdmissionCapTx), the zero change where none was ever made. A cap
// that stands keeps holding when a later configuration changes start, until a person clears it or
// sets another.
func EffectiveAdmissionCap(start, ceiling int, standing AdmissionCapChange) int {
	if standing.Cleared() {
		return start
	}
	return min(standing.Cap, ceiling)
}

// admissionCapColumns is a change row as scanAdmissionCapChange reads it.
const admissionCapColumns = `id, project, coalesce(cap, 0), cap_before, cap_after, ceiling, by_kind, by_session, reason, changed_at`

func scanAdmissionCapChange(row pgx.Row) (AdmissionCapChange, error) {
	var c AdmissionCapChange
	var kind string
	if err := row.Scan(&c.ID, &c.Project, &c.Cap, &c.CapBefore, &c.CapAfter, &c.Ceiling, &kind, &c.By.Session,
		&c.Reason, &c.At); err != nil {
		return AdmissionCapChange{}, err
	}
	c.By.Kind, c.At = PauseActorKind(kind), c.At.UTC()
	return c, nil
}

// AdmissionCapTx is, through tx, project's newest change of the cap: the cap set while the daemon
// runs where it set one (EffectiveAdmissionCap reads it), a clear where it was one, and the zero
// change, a clear too, where no change was ever made. project is the Dispatch key.
func AdmissionCapTx(ctx context.Context, tx pgx.Tx, project string) (AdmissionCapChange, error) {
	c, err := scanAdmissionCapChange(tx.QueryRow(ctx, `select `+admissionCapColumns+` from project_admission_cap_changes
		where project = $1 order by id desc limit 1`, project))
	if errors.Is(err, pgx.ErrNoRows) {
		return AdmissionCapChange{}, nil
	}
	if err != nil {
		return AdmissionCapChange{}, fmt.Errorf("read the admission cap of %s: %w", project, err)
	}
	return c, nil
}

// AdmissionCapChangesTx is, through tx, project's newest changes of the cap, newest first, at most
// limit of them: the state's `admission.capChanges`. Never nil.
func AdmissionCapChangesTx(ctx context.Context, tx pgx.Tx, project string, limit int) ([]AdmissionCapChange, error) {
	rows, err := tx.Query(ctx, `select `+admissionCapColumns+` from project_admission_cap_changes
		where project = $1 order by id desc limit $2`, project, limit)
	if err != nil {
		return nil, fmt.Errorf("read the admission cap changes of %s: %w", project, err)
	}
	defer rows.Close()
	changes := []AdmissionCapChange{}
	for rows.Next() {
		c, err := scanAdmissionCapChange(rows)
		if err != nil {
			return nil, fmt.Errorf("read the admission cap changes of %s: %w", project, err)
		}
		changes = append(changes, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the admission cap changes of %s: %w", project, err)
	}
	return changes, nil
}

// RecordAdmissionCapChangeTx appends change through tx and answers it with its id. It is the one
// writer of project_admission_cap_changes, and admission's setAdmissionCap its one caller
// (internal/textcheck's admission_cap_writers_test.go holds both). Every fact applies under
// intake's one lock (intake.SerializeFacts), so two changes of one project never read the same cap
// before. It refuses a cap below 0, a reason that is only white space or longer than
// AdmissionCapReasonMax characters, and an actor of neither shape before the table's checks would.
func RecordAdmissionCapChangeTx(ctx context.Context, tx pgx.Tx, change AdmissionCapChange) (AdmissionCapChange, error) {
	if change.Cap < 0 {
		return AdmissionCapChange{}, fmt.Errorf("change the admission cap of %s to %d: a cap is at least 1, and a clear names none", change.Project, change.Cap)
	}
	if strings.TrimSpace(change.Reason) == "" {
		return AdmissionCapChange{}, fmt.Errorf("change the admission cap of %s: a change says why it is made", change.Project)
	}
	if n := utf8.RuneCountInString(change.Reason); n > AdmissionCapReasonMax {
		return AdmissionCapChange{}, fmt.Errorf("change the admission cap of %s: the reason is %d characters, and a reason holds at most %d",
			change.Project, n, AdmissionCapReasonMax)
	}
	if err := change.By.validate(); err != nil {
		return AdmissionCapChange{}, fmt.Errorf("change the admission cap of %s: %w", change.Project, err)
	}
	var named *int
	if !change.Cleared() {
		named = &change.Cap
	}
	if err := tx.QueryRow(ctx, `insert into project_admission_cap_changes
		(project, cap, cap_before, cap_after, ceiling, by_kind, by_session, reason, changed_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9) returning id`,
		change.Project, named, change.CapBefore, change.CapAfter, change.Ceiling, string(change.By.Kind), change.By.Session,
		change.Reason, change.At).Scan(&change.ID); err != nil {
		return AdmissionCapChange{}, fmt.Errorf("change the admission cap of %s: %w", change.Project, err)
	}
	return change, nil
}
