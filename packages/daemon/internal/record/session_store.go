package record

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// StoreFailingClaim is one claim whose session's last report says its Oh My Pi session store
// refuses its writes, as the workflow decides a notice of it (workflow's sessionStoreFailed): the
// claim, the issue and role it holds, its session and its launch; when its session first read the
// failure and the store's error (supervise.Claim.StoreFailedSince and StoreFailedError); and when
// the daemon last told anybody of this claim's failure, and whom (StoreFailureToldAt and
// StoreFailureToldTo), the zero time and "" for a claim nobody was told of.
type StoreFailingClaim struct {
	Token      claim.Token
	Issue      string
	Role       claim.Role
	Session    string
	Generation uint64
	Since      time.Time
	Error      string
	ToldAt     time.Time
	ToldTo     supervise.StoreFailureTold
}

// ProjectSessionStoreFailures reads the claims of the project token's claim is of whose store
// failure stands, without a row lock: the told mark is written by the workflow alone, whose facts
// apply one at a time, and a report that clears a failure between this read and the notice clears
// a failure that had stood a minute, which is still the notice's to tell.
func (s *Postgres) ProjectSessionStoreFailures(ctx context.Context, tx pgx.Tx, token claim.Token) ([]StoreFailingClaim, error) {
	rows, err := tx.Query(ctx, `select token, issue, role, session, generation, store_failed_since, store_failed_error,
		store_failure_told_at, store_failure_told_to
		from claims where project = (select project from claims where token = $1) and store_failed_since is not null
		order by store_failed_since, token`, string(token))
	if err != nil {
		return nil, fmt.Errorf("read the session store failures of the project of %s: %w", token, err)
	}
	defer rows.Close()
	var failing []StoreFailingClaim
	for rows.Next() {
		var (
			c                      StoreFailingClaim
			claimToken, role, told string
			generation             int64
			toldAt                 *time.Time
		)
		if err := rows.Scan(&claimToken, &c.Issue, &role, &c.Session, &generation, &c.Since, &c.Error, &toldAt, &told); err != nil {
			return nil, fmt.Errorf("read a session store failure of the project of %s: %w", token, err)
		}
		c.Token, c.Role, c.Generation, c.ToldTo = claim.Token(claimToken), claim.Role(role), uint64(generation), supervise.StoreFailureTold(told)
		if toldAt != nil {
			c.ToldAt = *toldAt
		}
		failing = append(failing, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the session store failures of the project of %s: %w", token, err)
	}
	return failing, nil
}

// TellSessionStoreFailures marks each claim of tokens told of its store's failure at at, to told,
// and touches no other column but the row's update time: the mark is no part of the claim's
// machine state, and no claim write names it (the claim store's upsertClaim), so a launch keeps
// it.
func (s *Postgres) TellSessionStoreFailures(ctx context.Context, tx pgx.Tx, tokens []claim.Token, told supervise.StoreFailureTold, at time.Time) error {
	if len(tokens) == 0 {
		return nil
	}
	if told != supervise.StoreFailureToldArchitect && told != supervise.StoreFailureToldController {
		return fmt.Errorf("mark %d claims told of their session stores: %q is neither the architect nor the controller", len(tokens), told)
	}
	names := make([]string, len(tokens))
	for i, token := range tokens {
		names[i] = string(token)
	}
	if _, err := tx.Exec(ctx, `update claims set store_failure_told_at = $2, store_failure_told_to = $3, updated_at = now()
		where token = any($1)`, names, at.UTC(), string(told)); err != nil {
		return fmt.Errorf("mark %d claims told of their session stores: %w", len(tokens), err)
	}
	return nil
}
