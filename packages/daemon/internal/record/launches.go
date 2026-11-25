package record

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// LaunchKey names one launch of an agent: its claim and the generation the launch runs at.
type LaunchKey struct {
	Claim      claim.Token
	Generation uint64
}

// LaunchRead is one read of a launch's totals, as its shim answered, and the moment the daemon
// received the answer. The shim raises Totals.Read at each read, so of two reads of one launch the
// one with the higher Read is the newer.
type LaunchRead struct {
	Totals shimwire.Totals `json:"totals"`
	At     time.Time       `json:"at"`
}

// StartTotals is what a step's start read of its agent.
type StartTotals struct {
	// Generation is the agent claim's generation at the start.
	Generation uint64 `json:"generation"`
	// Live says the claim recorded a process at that generation.
	Live bool `json:"live"`
	// Read is the read the start used. It is nil for a live launch with no usable read.
	Read *LaunchRead `json:"read,omitempty"`
	// FromZero is the first generation whose launch counts from zero (AgentLaunches.StartTotals).
	FromZero uint64 `json:"fromZero"`
}

// Launch is one launch row of a claim: its generation, the newest read a step used, and the final
// totals its shim sent when its harness exited. Each is nil where the store holds none.
type Launch struct {
	Generation    uint64
	Newest, Final *LaunchRead
}

// AgentLaunches is what the store holds of one agent claim's launches.
type AgentLaunches struct {
	// Generation is the claim's generation now.
	Generation uint64
	// Live says the claim records a process now: its row holds a locator.
	Live bool
	// Launches is each launch row of the claim from the generation asked for on, by generation.
	Launches []Launch
}

// StartTotals is what a step's start keeps of the agent a, given the read the start used of a's
// live launch, nil where it has none. A launch after the start counts from zero, and FromZero is
// the first generation whose launch does. A live launch is the start's own, so it is the next
// generation. A claim with no process ran its generation's launch before the step where a launch
// row of that generation exists, so it is the next generation then. With no such row that
// generation has not launched, so it is the generation itself. The rule reads a's launch of its
// own generation, so a must be read from that generation or an earlier one.
func (a AgentLaunches) StartTotals(used *LaunchRead) StartTotals {
	start := StartTotals{Generation: a.Generation, Live: a.Live, Read: used, FromZero: a.Generation}
	if a.Live {
		start.FromZero = a.Generation + 1
		return start
	}
	for _, launch := range a.Launches {
		if launch.Generation == a.Generation {
			start.FromZero = a.Generation + 1
		}
	}
	return start
}

// AgentLaunches reads the claim agent: its generation now, whether its row records a process, and
// each of its launch rows from the generation since on. A claim the store does not hold has
// generation 0, no process and no launch.
func (s *Postgres) AgentLaunches(ctx context.Context, tx pgx.Tx, agent claim.Token, since uint64) (AgentLaunches, error) {
	var launches AgentLaunches
	var generation int64
	err := tx.QueryRow(ctx, `select generation, locator is not null from claims where token = $1`, string(agent)).Scan(&generation, &launches.Live)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return AgentLaunches{}, fmt.Errorf("read the claim %s: %w", agent, err)
	}
	launches.Generation = uint64(generation)
	if since > math.MaxInt64 {
		return launches, nil
	}
	rows, err := tx.Query(ctx, `select generation, newest, final from claim_launches where claim = $1 and generation >= $2
		order by generation`, string(agent), int64(since))
	if err != nil {
		return AgentLaunches{}, fmt.Errorf("read the launches of %s from %d: %w", agent, since, err)
	}
	defer rows.Close()
	for rows.Next() {
		var launch Launch
		var launched int64
		var newest, final []byte
		if err := rows.Scan(&launched, &newest, &final); err != nil {
			return AgentLaunches{}, fmt.Errorf("read a launch of %s: %w", agent, err)
		}
		launch.Generation = uint64(launched)
		if launch.Newest, err = launchReadOf(newest); err != nil {
			return AgentLaunches{}, fmt.Errorf("read the newest read of %s at %d: %w", agent, launch.Generation, err)
		}
		if launch.Final, err = launchReadOf(final); err != nil {
			return AgentLaunches{}, fmt.Errorf("read the final totals of %s at %d: %w", agent, launch.Generation, err)
		}
		launches.Launches = append(launches.Launches, launch)
	}
	if err := rows.Err(); err != nil {
		return AgentLaunches{}, fmt.Errorf("read the launches of %s from %d: %w", agent, since, err)
	}
	return launches, nil
}

// launchReadOf is the read a launch column holds, nil for SQL null.
func launchReadOf(column []byte) (*LaunchRead, error) {
	if column == nil {
		return nil, nil
	}
	var read LaunchRead
	if err := json.Unmarshal(column, &read); err != nil {
		return nil, err
	}
	return &read, nil
}

// KeepRead keeps read as the newest read of the launch key where its Read is above the one the
// store holds, or the store holds none. Two facts can read one launch in either order, so the read
// that commits later can be the older. A launch with no row, one that started under the release
// before, gets its row with read.
func (s *Postgres) KeepRead(ctx context.Context, tx pgx.Tx, key LaunchKey, read LaunchRead) error {
	return keepLaunchRead(ctx, tx, key, read, "newest", `on conflict (claim, generation) do update set newest = excluded.newest
		where claim_launches.newest is null
		or (claim_launches.newest->'totals'->>'read')::numeric < (excluded.newest->'totals'->>'read')::numeric`)
}

// KeepFinal keeps final as the final totals of the launch key where the store holds none. The shim
// sends them once, when its harness exits, so the first one kept stands. A launch with no row, one
// that started under the release before, gets its row with final.
func (s *Postgres) KeepFinal(ctx context.Context, tx pgx.Tx, key LaunchKey, final LaunchRead) error {
	return keepLaunchRead(ctx, tx, key, final, "final", `on conflict (claim, generation) do update set final = excluded.final
		where claim_launches.final is null`)
}

// keepLaunchRead writes read into column of the launch key's row, inserting the row where it is
// missing. Where the row stands, onConflict decides whether read replaces what column holds.
func keepLaunchRead(ctx context.Context, tx pgx.Tx, key LaunchKey, read LaunchRead, column, onConflict string) error {
	if key.Generation == 0 || key.Generation > math.MaxInt64 {
		return fmt.Errorf("keep the %s totals of %s: generation %d names no launch", column, key.Claim, key.Generation)
	}
	encoded, err := json.Marshal(read)
	if err != nil {
		return fmt.Errorf("keep the %s totals of %s at %d: %w", column, key.Claim, key.Generation, err)
	}
	if _, err := tx.Exec(ctx, `insert into claim_launches (claim, generation, `+column+`) values ($1, $2, $3) `+onConflict,
		string(key.Claim), int64(key.Generation), encoded); err != nil {
		return fmt.Errorf("keep the %s totals of %s at %d: %w", column, key.Claim, key.Generation, err)
	}
	return nil
}

// StepAgentClaims is each claim not retired whose agent a fact of issue can start or end a step
// of: the issue's own claims and the architect of its tree.
func (s *Postgres) StepAgentClaims(ctx context.Context, tx pgx.Tx, issue string) ([]claim.Token, error) {
	rows, err := tx.Query(ctx, `select token from claims where state <> 'retired' and (issue = $1 or
		(role = 'architect' and tree = (select tree from issues where key = $1))) order by token`, issue)
	if err != nil {
		return nil, fmt.Errorf("read the agents of the steps of %s: %w", issue, err)
	}
	tokens, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the agents of the steps of %s: %w", issue, err)
	}
	agents := make([]claim.Token, len(tokens))
	for i, token := range tokens {
		agents[i] = claim.Token(token)
	}
	return agents, nil
}
