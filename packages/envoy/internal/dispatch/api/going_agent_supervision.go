package api

// going_agent_supervision.go is what the process supervisor (cmd/dispatch's agent_supervisor.go)
// needs of the server's end of agent/v1: when the agent last renewed, so an agent stuck as a whole
// is killed; the bound on the runs one agent instance holds, which the supervisor halves after
// kills for memory no run alone caused; and the agent's death, whose runs are freed at once and
// counted against a run that the agent held alone (background-runs design, section 5).

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/going/agentwire"
)

// goingRunKills is how many deaths of the agent a run may cause while the agent works on it alone
// before it fails with goingRunKilledTheAgent.
const goingRunKills = 3

// goingRunKilledTheAgent is the stop reason of a run that the agent died working on alone
// goingRunKills times.
const goingRunKilledTheAgent = "AGENT_KILLED: the agent process died 3 times while it worked on this run alone, so the run stops here"

// LastRenewal is when the agent's latest renew arrived, the zero time before any.
func (a *AgentListener) LastRenewal() time.Time {
	nanos := a.renewed.Load()
	if nanos == 0 {
		return time.Time{}
	}
	return time.Unix(0, nanos)
}

// SetRunSlots bounds how many runs one agent instance holds at once: a claim of an instance that
// holds slots runs takes none until one of them ends. 0 sets no bound.
func (a *AgentListener) SetRunSlots(slots int) {
	a.runSlots.Store(int64(slots))
	a.s.goingRuns.Wake()
}

// AgentExited is the process supervisor's report that the agent process it started has exited,
// whether it was killed or not; stopping says the server asked it to, by closing its standard
// input. Every lease the agent's instances held is freed at once, so its runs are ready for the
// next agent with no wait for the leases to run out, and its health is forgotten. Unless the agent
// was stopping, a run it held alone counts one kill (going_runs.kills), and at goingRunKills it
// fails with goingRunKilledTheAgent; each other run it held is a suspect, which the next claims
// take one at a time, alone (claimNext). It reports whether the agent held one run alone, so the
// supervisor tells a kill a run alone caused from one it did not.
func (a *AgentListener) AgentExited(ctx context.Context, stopping bool) (bool, error) {
	a.health.Store(nil)
	a.mu.Lock()
	instances := make([]string, 0, len(a.instances))
	for instance := range a.instances {
		instances = append(instances, instance)
	}
	a.mu.Unlock()
	if len(instances) == 0 {
		return false, nil
	}
	tx, err := a.s.deps.Store.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
		select id::text from going_runs where state = 'working' and lease_owner = any($1::text[]) order by id for update
	`, instances)
	if err != nil {
		return false, fmt.Errorf("read the runs the agent held: %w", err)
	}
	held, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return false, fmt.Errorf("read the runs the agent held: %w", err)
	}
	alone := len(held) == 1 && !stopping
	failed := ""
	if alone {
		var kills int
		if err := tx.QueryRow(ctx, `update going_runs set kills = kills + 1 where id = $1::uuid returning kills`, held[0]).Scan(&kills); err != nil {
			return false, fmt.Errorf("count the agent's death against run %s: %w", held[0], err)
		}
		if kills >= goingRunKills {
			if _, err := tx.Exec(ctx, `
				update going_runs set state = 'failed', stop_reason = $2, lease_token = null, lease_owner = null,
				  lease_ends_at = null, finished_at = now()
				where id = $1::uuid
			`, held[0], goingRunKilledTheAgent); err != nil {
				return false, fmt.Errorf("fail run %s, which the agent died on %d times: %w", held[0], kills, err)
			}
			failed = held[0]
		}
	}
	if len(held) > 0 {
		if _, err := tx.Exec(ctx, `
			update going_runs set state = 'queued', lease_token = null, lease_owner = null, lease_ends_at = null
			where id = any($1::uuid[]) and state = 'working'
		`, held); err != nil {
			return false, fmt.Errorf("free the runs the agent held: %w", err)
		}
		// Delivered when the transaction commits, so a claim that wakes reads the runs freed.
		if _, err := tx.Exec(ctx, `select pg_notify($1, 'ready')`, GoingRunsChannel); err != nil {
			return false, fmt.Errorf("announce the runs the agent held: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	if !stopping {
		a.mu.Lock()
		for _, run := range held {
			if run != failed {
				a.suspects[run] = true
			}
		}
		a.mu.Unlock()
	}
	return alone, nil
}

// claimNext is one claim of instance. While suspects are left it takes only a suspect, and only
// while the instance holds no other run, so a run that kills the agent again does so alone; a
// suspect no claim of this build can take any more - one its person stopped, one that ended, one
// another server's agent holds, one of a shape this build does not read - stops being one. With
// none left it takes any ready run, within the instance's run slots (SetRunSlots).
func (a *AgentListener) claimNext(ctx context.Context, instance string) (*agentwire.ClaimedRun, error) {
	a.mu.Lock()
	suspects := make([]string, 0, len(a.suspects))
	for run := range a.suspects {
		suspects = append(suspects, run)
	}
	instances := make([]string, 0, len(a.instances))
	for known := range a.instances {
		instances = append(instances, known)
	}
	a.mu.Unlock()
	if len(suspects) == 0 {
		return a.claimOne(ctx, instance, nil, a.runSlots.Load())
	}
	run, err := a.claimOne(ctx, instance, suspects, 1)
	if err != nil || run != nil {
		return run, err
	}
	rows, err := a.s.deps.Store.Pool.Query(ctx, `
		select id::text from going_runs
		where id = any($1::uuid[]) and shape <= $2
		  and (state = 'queued' or (state = 'working' and (lease_ends_at < now() or lease_owner = any($3::text[]))))
	`, suspects, goingRunShape, instances)
	if err != nil {
		return nil, fmt.Errorf("read the suspect runs: %w", err)
	}
	claimable, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the suspect runs: %w", err)
	}
	kept := make(map[string]bool, len(claimable))
	for _, run := range claimable {
		kept[run] = true
	}
	a.mu.Lock()
	for _, run := range suspects {
		if !kept[run] {
			delete(a.suspects, run)
		}
	}
	left := len(a.suspects)
	a.mu.Unlock()
	if left == 0 {
		return a.claimOne(ctx, instance, nil, a.runSlots.Load())
	}
	return nil, nil
}

// settleSuspect ends run's suspicion, once it saved a new step or ended, and wakes the claims
// waiting, which may take the next suspect, or any run once none is left.
func (a *AgentListener) settleSuspect(run string) {
	a.mu.Lock()
	suspect := a.suspects[run]
	delete(a.suspects, run)
	a.mu.Unlock()
	if suspect {
		a.s.goingRuns.Wake()
	}
}

// runEnded is the end of run's turn: it is no suspect any more, and a claim that waits for one of
// its instance's run slots looks again.
func (a *AgentListener) runEnded(run string) {
	a.settleSuspect(run)
	if a.runSlots.Load() > 0 {
		a.s.goingRuns.Wake()
	}
}

// GoingRunCounts is how many runs work, wait in the queue for an agent, and wait for their person,
// across every server process of the database.
type GoingRunCounts struct {
	Working int64
	Queued  int64
	Waiting int64
}

// goingRunCounts counts the runs in each of the three states, which /metrics shows.
func (s *server) goingRunCounts(ctx context.Context) (GoingRunCounts, error) {
	var counts GoingRunCounts
	err := s.deps.Store.Pool.QueryRow(ctx, `
		select count(*) filter (where state = 'working'), count(*) filter (where state = 'queued'),
		  count(*) filter (where state = 'waiting')
		from going_runs where state in ('working', 'queued', 'waiting')
	`).Scan(&counts.Working, &counts.Queued, &counts.Waiting)
	return counts, err
}
