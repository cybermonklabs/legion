package store

import "context"

// LivePersonSignIns counts person's sign-ins on a machine that are not revoked: the rows
// PersonSignIns lists, counted instead of read. person is a login as record.CanonicalLogin spells
// it, which is how person_signins holds it. GET /v1/person-access answers the count to Dispatch,
// which keeps a signed-out person's refresh token while any of these is live, since each goes on
// trading tokens that name the person.
func (s *Store) LivePersonSignIns(ctx context.Context, person string) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `select count(*) from person_signins where person = $1 and revoked_at is null`, person).Scan(&n)
	return n, err
}
