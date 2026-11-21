package store

import (
	"context"
	"fmt"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
)

// DeveloperBoxTokens is how many of the broker's live access tokens a developer box holds that
// list one project, and when the last of them expires: the zero time when none is live.
type DeveloperBoxTokens struct {
	Live          int
	LastExpiresAt time.Time
}

// LiveDeveloperBoxTokens counts the access tokens that list project and act as their person (the
// project scope oauthwire.ProjectScopePerson), which are a developer box's: only a machine
// login's trade and a person's sign-in on a machine mint a token that acts as its person and
// lists a project (internal/cipher/oauth's projectAccess). A token counts while it is live, not
// revoked and not past its expiry, so the count falls to zero within one token life of the
// exchange refusing every trade of the project, and at once once those tokens are revoked.
//
// The claims are read where the exchange stores them, the stored session's extra claims
// (internal/cipher/oauth's ClaimProjectScope and ClaimProjects, the list spelled as
// oauthwire.JoinProjects spells it), as RevokePersonSignInTx reads the person there; the
// exchange's own tests count a token it minted, which holds this reading to the one it writes.
func (s *Store) LiveDeveloperBoxTokens(ctx context.Context, project string) (DeveloperBoxTokens, error) {
	var count int
	var last *time.Time
	err := s.Pool.QueryRow(ctx, `select count(*), max(expires_at) from oauth_access_tokens
		where active and expires_at > now()
			and request #>> '{session,extra,project_scope}' = $2
			and $1 = any(string_to_array(request #>> '{session,extra,projects}', ' '))`,
		project, oauthwire.ProjectScopePerson).Scan(&count, &last)
	if err != nil {
		return DeveloperBoxTokens{}, fmt.Errorf("count the live developer-box tokens of %s: %w", project, err)
	}
	answer := DeveloperBoxTokens{Live: count}
	if last != nil {
		answer.LastExpiresAt = *last
	}
	return answer, nil
}
