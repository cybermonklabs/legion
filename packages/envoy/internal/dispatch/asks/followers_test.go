package asks

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// refusingDatabase fails every statement and every read with err, as a database that refused
// them does.
type refusingDatabase struct{ err error }

func (d refusingDatabase) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, d.err
}

func (d refusingDatabase) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, d.err
}

// A follow, a read of whether a session follows, and an unfollow that the database refuses each
// fail naming the ask and carrying the cause, and never name the session: a handler logs the
// failure, and has the session from its own request.
func TestAFollowerWriteOrReadTheDatabaseRefusesNamesTheAskAndNotTheSession(t *testing.T) {
	const ask, session = "ask-5c1e", "ses-unnamed-follower"
	refused := errors.New("the statement was refused")
	database := refusingDatabase{refused}
	ctx := context.Background()
	_, follows := Follows(ctx, database, ask, session)
	_, unfollow := Unfollow(ctx, database, ask, session)
	for what, err := range map[string]error{
		"a follow":                     FollowAuthor(ctx, database, ask, model.Actor{Kind: "session", ID: session}),
		"a read of whether it follows": follows,
		"an unfollow":                  unfollow,
	} {
		if !errors.Is(err, refused) || !strings.Contains(err.Error(), ask) {
			t.Errorf("%s refused = %v, want the refusal, naming ask %s", what, err, ask)
		} else if strings.Contains(err.Error(), session) {
			t.Errorf("%s refused names the session %s: %v", what, session, err)
		}
	}
}
