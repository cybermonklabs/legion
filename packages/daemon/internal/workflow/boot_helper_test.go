package workflow

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// bootStates is, for each test's pool, what the boot pass these tests run before a fact keeps
// between its calls (bootBefore).
var bootStates sync.Map

// bootState is what bootBefore keeps of one pool. booted is whether the daemon of the scenario has
// booted: the first pass is the boot, and opens what it finds. left is each issue's row and plan
// rows as the last fact (applyFacts, factTx) left them (issueRows): an issue whose rows differ from
// them at the next pass was written by the test between the two facts, which the daemon's own
// facts never are.
type bootState struct {
	mu     sync.Mutex
	booted bool
	left   map[string]string
}

// bootStateOf is pool's state, made at its first use and dropped with the test.
func bootStateOf(t *testing.T, pool *pgxpool.Pool) *bootState {
	t.Helper()
	state, loaded := bootStates.LoadOrStore(pool, &bootState{})
	if !loaded {
		t.Cleanup(func() { bootStates.Delete(pool) })
	}
	return state.(*bootState)
}

// bootBefore runs the daemon's boot pass that opens each run no start of this release counted
// (Engine.OpenUncountedRuns) before a fact. The daemon runs that pass once, before it applies any
// fact (internal/daemon's openUncountedRuns), and these tests seed an issue standing in a step
// between their facts, a run only an earlier release could have started, which a boot before the
// next fact would open; so the pass runs before each fact. Only its first run in a scenario is the
// boot. A later run that opens a run of an issue whose row is as the last fact left it fails the
// test (bootPass): that fact moved the issue into a step and counted no start, which production
// would never open, so it would record nothing of that run. A test that boots the daemon again
// says so (rebooted).
func bootBefore(t *testing.T, pool *pgxpool.Pool, engine *Engine) {
	t.Helper()
	if err := bootPass(t.Context(), pool, engine, bootStateOf(t, pool)); err != nil {
		t.Fatal(err)
	}
}

// bootPass is bootBefore's pass and its check, answering the check's refusal.
func bootPass(ctx context.Context, pool *pgxpool.Pool, engine *Engine, state *bootState) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	written, err := issueRows(ctx, pool)
	if err != nil {
		return err
	}
	before, err := startRows(ctx, pool)
	if err != nil {
		return err
	}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		_, err := engine.OpenUncountedRuns(ctx, tx)
		return err
	}); err != nil {
		return fmt.Errorf("open the uncounted runs: %w", err)
	}
	after, err := startRows(ctx, pool)
	if err != nil {
		return err
	}
	booted := state.booted
	state.booted = true
	if !booted {
		return nil
	}
	var unseeded []string
	for place, row := range after {
		issue, _, _ := strings.Cut(place, " ")
		if before[place] == row {
			continue
		}
		if left, ok := state.left[issue]; ok && left == written[issue] {
			unseeded = append(unseeded, place)
		}
	}
	if len(unseeded) > 0 {
		slices.Sort(unseeded)
		return fmt.Errorf("the boot pass before a fact opened %v, of issues that stand as the last fact left them: "+
			"that fact moved them into a step and counted no start, and the daemon, whose boot ran before it, would record nothing of that run", unseeded)
	}
	return nil
}

// factLeft keeps each issue's row as the fact that just committed left it (bootState.left).
func factLeft(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	rows, err := issueRows(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	state := bootStateOf(t, pool)
	state.mu.Lock()
	defer state.mu.Unlock()
	state.left = rows
}

// rebooted says the scenario's daemon boots again, as a restart or a newer image's boot over a
// rolled-back store does: the next pass is a boot, and opens what it finds.
func rebooted(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	state := bootStateOf(t, pool)
	state.mu.Lock()
	defer state.mu.Unlock()
	state.booted = false
}

// issueRows is each issue's row and its plan rows, the record the walk of its run reads
// (Engine.walk), as a digest of all their columns, by key. A plan row names its issue by key alone,
// and the store has plan rows of an issue it has not recorded (an owner's plan at generation 0),
// which no issue row digests.
func issueRows(ctx context.Context, pool *pgxpool.Pool) (map[string]string, error) {
	return keyedRows(ctx, pool, `select key, md5(i::text
		|| coalesce((select string_agg(p::text, ',' order by p::text) from issue_plans p where p.issue = i.key), '')
		|| coalesce((select string_agg(p::text, ',' order by p::text) from plan_tasks p where p.issue = i.key), '')
		|| coalesce((select string_agg(p::text, ',' order by p::text) from plan_steps p where p.issue = i.key), '')) from issues i`)
}

// startRows is each step_starts row's count and run, by its issue, task, kind and place.
func startRows(ctx context.Context, pool *pgxpool.Pool) (map[string]string, error) {
	return keyedRows(ctx, pool, `select issue || ' ' || task || ' ' || kind || ' ' || place, starts || ' ' || run from step_starts`)
}

// keyedRows reads query's two text columns as a map.
func keyedRows(ctx context.Context, pool *pgxpool.Pool, query string) (map[string]string, error) {
	rows, err := pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", query, err)
	}
	keyed := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		keyed[key] = value
	}
	return keyed, rows.Err()
}
