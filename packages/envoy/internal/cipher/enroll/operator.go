package enroll

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrNoEnrollment is an enrollment id no row has, such as one gone since a proof naming it
// verified.
var ErrNoEnrollment = errors.New("no such enrollment")

// EnrollmentOperator is the person enrollment id acts for, its operator column, and the empty
// string for one that acts for nobody: a service's pod, or a machine's own session. It reads the
// row whether it is live or ended, in one statement, and is ErrNoEnrollment for an id no row has.
// The API's session routes ask it for the person whose hold the gate is asked about.
func (s *Service) EnrollmentOperator(ctx context.Context, id string) (string, error) {
	var operator string
	err := s.Store.Pool.QueryRow(ctx, `select coalesce(operator, '') from enrollments where id = $1`, id).Scan(&operator)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: %s", ErrNoEnrollment, id)
	}
	return operator, err
}
