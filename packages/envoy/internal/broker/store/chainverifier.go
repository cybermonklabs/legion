package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/record"
)

// ChainVerifier builds the record.ChainVerifier that re-verifies records of one kind
// (record.KindAgentSecret for requests.Machine's grants, record.KindLauncherCredential for
// enroll.Service.AuthenticateLauncher) straight from this store: FetchRecord finds a record only
// of that kind, so a record of one kind never backs the other's credential, and FetchDecisions
// reads every terminal decision event the record carries, oldest first. Its rows map onto
// record.TerminalEvent by column name, never by position: which arm of the chain check admits an
// approval is read off actor (record.ChainVerifier.Verify), and actor and login are both strings
// in the same select list, so a select read by position would still compile with the two columns
// swapped. login is the one column of the three that is nullable (0006_approval_by_login.up.sql:
// an event no human decided has none), so it alone is coalesced, and the coalesce is given the
// column's own name to be matched by.
func (s *Store) ChainVerifier(kind, audience string, skew time.Duration) *record.ChainVerifier {
	return &record.ChainVerifier{
		Kind:     kind,
		Audience: audience,
		Skew:     skew,
		FetchRecord: func(ctx context.Context, recordID string) (string, time.Time, bool, error) {
			var body string
			var createdAt time.Time
			err := s.Pool.QueryRow(ctx, `select body, created_at from credential_requests where id=$1 and kind=$2`, recordID, kind).Scan(&body, &createdAt)
			if errors.Is(err, pgx.ErrNoRows) {
				return "", time.Time{}, false, nil
			}
			if err != nil {
				return "", time.Time{}, false, err
			}
			return body, createdAt, true, nil
		},
		FetchDecisions: func(ctx context.Context, recordID string) ([]record.TerminalEvent, error) {
			rows, err := s.Pool.Query(ctx, `select event, coalesce(login, '') as login, actor from credential_request_events
				where record_id=$1 and event = any($2) order by id`, recordID, record.TerminalEventNames())
			if err != nil {
				return nil, err
			}
			return pgx.CollectRows(rows, pgx.RowToStructByName[record.TerminalEvent])
		},
	}
}
