package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/rank"
)

type rankInput struct {
	Before *string `json:"before"`
	After  *string `json:"after"`
}

func lockProjectRankAllocation(ctx context.Context, tx pgx.Tx, project string) error {
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext('issue-rank:' || $1))`, project); err != nil {
		return fmt.Errorf("lock project rank allocation: %w", err)
	}
	return nil
}

func (s *server) rankForInput(ctx context.Context, q queryer, project, issueKey string, input rankInput) (string, error) {
	var (
		previous string
		next     string
		err      error
	)
	if input.After != nil {
		previous, err = rankBoundary(ctx, q, project, issueKey, *input.After)
		if err != nil {
			return "", err
		}
	}
	if input.Before != nil {
		next, err = rankBoundary(ctx, q, project, issueKey, *input.Before)
		if err != nil {
			return "", err
		}
	}
	if previous != "" && next != "" && previous >= next {
		return "", errorf(http.StatusBadRequest, "RANK_INPUT", "rank neighbors are not ordered")
	}
	if previous != "" {
		next, err = nextProjectRank(ctx, q, project, issueKey, previous, next)
	} else if next != "" {
		previous, err = previousProjectRank(ctx, q, project, issueKey, next)
	} else {
		previous, err = lastProjectRank(ctx, q, project, issueKey)
	}
	if err != nil {
		return "", err
	}
	return rank.Between(previous, next), nil
}

func nextProjectRank(ctx context.Context, q queryer, project, issueKey, previous, next string) (string, error) {
	var rank string
	err := q.QueryRow(ctx, `
		select rank from issues
		where project_key = $1 and key != $2 and rank > $3 and ($4 = '' or rank < $4)
		order by rank
		limit 1
	`, project, issueKey, previous, next).Scan(&rank)
	if errors.Is(err, pgx.ErrNoRows) {
		return next, nil
	}
	return rank, err
}

func previousProjectRank(ctx context.Context, q queryer, project, issueKey, next string) (string, error) {
	var rank string
	err := q.QueryRow(ctx, `
		select rank from issues
		where project_key = $1 and key != $2 and rank < $3
		order by rank desc
		limit 1
	`, project, issueKey, next).Scan(&rank)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return rank, err
}

func lastProjectRank(ctx context.Context, q queryer, project, issueKey string) (string, error) {
	var rank string
	err := q.QueryRow(ctx, `
		select rank from issues
		where project_key = $1 and key != $2
		order by rank desc
		limit 1
	`, project, issueKey).Scan(&rank)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return rank, err
}

// rankBoundary reads a rank neighbour the PATCH already locked under this same trimmed key
// (lockPatchOwners), so it takes no lock of its own.
func rankBoundary(ctx context.Context, q queryer, project, issueKey, key string) (string, error) {
	if key == "" || key == issueKey {
		return "", errorf(http.StatusBadRequest, "RANK_INPUT", "rank neighbor must name another issue")
	}
	var (
		neighborProject string
		neighborRank    string
	)
	if err := q.QueryRow(ctx, `select project_key, rank from issues where key = $1`, key).Scan(&neighborProject, &neighborRank); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", errorf(http.StatusBadRequest, "RANK_INPUT", "rank neighbor does not exist")
		}
		return "", err
	}
	if neighborProject != project {
		return "", errorf(http.StatusBadRequest, "RANK_INPUT", "rank neighbor must be in the same project")
	}
	return neighborRank, nil
}

// issueRankPlace is an issue's place in its project's ranking: its rank, and the issues just
// before it (After, an update's rank `after`) and just after it (Before, its `before`) among every
// issue of the project in rank order, the ordering an update places an issue in (rankForInput). A
// neighbour is nil where the issue is first or last. An update that ranks the issue answers the
// place it found and the place it left (issueUpdated), and a write back of the rank names the
// neighbours of the place it found.
type issueRankPlace struct {
	Rank   string  `json:"rank"`
	After  *string `json:"after"`
	Before *string `json:"before"`
}

// readIssueRankPlace reads the issue's place in its project's ranking through q: an update's own
// transaction, which holds the project's rank allocation (lockProjectRankAllocation), or the pool.
func readIssueRankPlace(ctx context.Context, q queryer, key string) (issueRankPlace, error) {
	var place issueRankPlace
	err := q.QueryRow(ctx, `
		select i.rank,
		  (select p.key from issues p where p.project_key = i.project_key and p.key <> i.key and p.rank < i.rank
		     order by p.rank desc limit 1),
		  (select n.key from issues n where n.project_key = i.project_key and n.key <> i.key and n.rank > i.rank
		     order by n.rank limit 1)
		from issues i where i.key = $1
	`, key).Scan(&place.Rank, &place.After, &place.Before)
	if err != nil {
		return issueRankPlace{}, fmt.Errorf("read %s's place in its project's ranking: %w", key, err)
	}
	return place, nil
}
