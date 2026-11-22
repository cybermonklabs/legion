package store

import (
	"context"
	"fmt"
)

// FleetAlertLine is one of R6's lines that is open (the table fleet_alert_lines): the case's word
// (internal/fleetalert's Case.Word) written for Claim, "" for a case of the whole project. The
// daemon's fleet alert writer keeps it here from the line's write to its case's end, so the daemon
// that boots next writes no line of a case still open.
type FleetAlertLine struct {
	Project string
	Case    string
	Claim   string
}

// FleetAlertLines is every open line of project, in order of case and claim.
func (s *Store) FleetAlertLines(ctx context.Context, project string) ([]FleetAlertLine, error) {
	rows, err := s.pool.Query(ctx, `select case_word, claim_token from fleet_alert_lines
		where project = $1 order by case_word, claim_token`, project)
	if err != nil {
		return nil, fmt.Errorf("read the open fleet alert lines of %s: %w", project, err)
	}
	defer rows.Close()
	var lines []FleetAlertLine
	for rows.Next() {
		line := FleetAlertLine{Project: project}
		if err := rows.Scan(&line.Case, &line.Claim); err != nil {
			return nil, fmt.Errorf("read the open fleet alert lines of %s: %w", project, err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the open fleet alert lines of %s: %w", project, err)
	}
	return lines, nil
}

// PutFleetAlertLine keeps line open. A line already open stays as it was.
func (s *Store) PutFleetAlertLine(ctx context.Context, line FleetAlertLine) error {
	if _, err := s.pool.Exec(ctx, `insert into fleet_alert_lines (project, case_word, claim_token) values ($1, $2, $3)
		on conflict (project, case_word, claim_token) do nothing`, line.Project, line.Case, line.Claim); err != nil {
		return fmt.Errorf("keep the %s fleet alert line of %s open: %w", line.Case, line.Project, err)
	}
	return nil
}

// DeleteFleetAlertLine forgets line, whose case has ended. A line not open changes nothing.
func (s *Store) DeleteFleetAlertLine(ctx context.Context, line FleetAlertLine) error {
	if _, err := s.pool.Exec(ctx, `delete from fleet_alert_lines where project = $1 and case_word = $2 and claim_token = $3`,
		line.Project, line.Case, line.Claim); err != nil {
		return fmt.Errorf("forget the %s fleet alert line of %s: %w", line.Case, line.Project, err)
	}
	return nil
}
