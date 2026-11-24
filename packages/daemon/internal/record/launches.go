package record

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// The declarations of the launch store with no behaviour, so the tests compile and fail on what
// they assert: this tree is the red run's, never merged.

type LaunchKey struct {
	Claim      claim.Token
	Generation uint64
}

type LaunchRead struct {
	Totals shimwire.Totals `json:"totals"`
	At     time.Time       `json:"at"`
}

type StartTotals struct {
	Generation uint64      `json:"generation"`
	Live       bool        `json:"live"`
	Read       *LaunchRead `json:"read,omitempty"`
	FromZero   uint64      `json:"fromZero"`
}

type Launch struct {
	Generation    uint64
	Newest, Final *LaunchRead
}

type AgentLaunches struct {
	Generation uint64
	Live       bool
	Launches   []Launch
}

func (a AgentLaunches) StartTotals(used *LaunchRead) StartTotals { return StartTotals{} }

func (s *Postgres) AgentLaunches(ctx context.Context, tx pgx.Tx, agent claim.Token, since uint64) (AgentLaunches, error) {
	return AgentLaunches{}, nil
}

func (s *Postgres) KeepRead(ctx context.Context, tx pgx.Tx, key LaunchKey, read LaunchRead) error {
	return nil
}

func (s *Postgres) KeepFinal(ctx context.Context, tx pgx.Tx, key LaunchKey, final LaunchRead) error {
	return nil
}

func (s *Postgres) StepAgentClaims(ctx context.Context, tx pgx.Tx, issue string) ([]claim.Token, error) {
	return nil, nil
}
