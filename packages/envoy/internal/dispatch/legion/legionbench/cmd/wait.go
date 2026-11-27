package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The wait before the drive appends (R8's "Before step 4"): the server's Legion state intake has
// read the stand-in whole once and recorded the read (legion_reads), and, where the server runs
// the Legion facts consumer (this head), the consumer has read the project whole under the running
// build (issue_facts_computed names it), its cursors stand at the heads of its three feeds, and the
// outbox holds nothing to drain. It is evals/evalbench's `measure wait` of #3976, narrowed to the
// facts consumer.

// pollEvery is how often the wait reads the consumer's state. The consumer is a woken loop: an
// event, an observation or a drained change wakes its next pass as soon as the running one ends,
// so a cursor that stood still for a whole poll had a pass in which to move.
const pollEvery = 5 * time.Second

// lagPolls is how many polls old the heads a cursor is held to are: each cursor must stand at or
// past where its feed stood lagPolls polls before. The rows the consumer writes log changes after
// its own cursor, so its change cursor reaches a head only on its next pass, which the drain of
// those changes wakes as soon as the pass that wrote them ends.
const lagPolls = 3

// heads is where each feed the consumer reads stands.
type heads struct {
	events, observations, changes int64
}

// consumerState is what one poll read: the feeds' heads, the newest read of the project's daemon,
// and, where the database has the consumer's tables, its cursors by name, the build its record of
// the project's whole read names, and the outbox's rows.
type consumerState struct {
	heads    heads
	readOK   bool
	readAt   *time.Time
	facts    map[string]int64
	computed string
	pending  int64
}

// serverBuild is the build the server at url says it is (/healthz's commit), which its consumer
// records against the project it reads whole.
func serverBuild(ctx context.Context, url string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/healthz", nil)
	if err != nil {
		return "", err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("read the server's health: %w", err)
	}
	defer response.Body.Close()
	var health struct {
		Commit string `json:"commit"`
	}
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		return "", fmt.Errorf("read the server's health: %w", err)
	}
	if health.Commit == "" {
		return "", fmt.Errorf("the server answers no commit: build it with -ldflags \"-X main.buildCommit=<commit>\", since an unstamped binary is a build of its own process and its record of the whole read names no commit")
	}
	return health.Commit, nil
}

// waitConsumers polls until the server has caught up (caughtUp) on two polls in a row. Once
// timeout has passed, the first poll that finds something still owed fails the wait with what is
// owed and the bound (`<what is still owed> after <timeout>`), so a server that will not catch up
// ends the arm rather than holding the Job to its deadline. facts says whether the server runs
// the facts consumer.
func waitConsumers(ctx context.Context, r *report, pool *store.Pool, project, build string, facts bool, timeout time.Duration) error {
	started := time.Now()
	var seen []heads
	held := false
	for {
		state, err := readConsumers(ctx, pool, project, facts)
		if err != nil {
			return err
		}
		var previous *heads
		if len(seen) >= lagPolls {
			previous = &seen[len(seen)-lagPolls]
		}
		missing := state.caughtUp(project, build, facts, previous)
		fmt.Printf("WAIT %s after %s: %s\n", time.Now().UTC().Format(time.RFC3339), time.Since(started).Round(time.Second), state.summary())
		if len(missing) == 0 && held {
			r.result("catchup seconds=%.0f %s", time.Since(started).Seconds(), state.summary())
			return nil
		}
		held = len(missing) == 0
		if !held {
			fmt.Printf("WAIT still owed: %s\n", strings.Join(missing, "; "))
			if time.Since(started) > timeout {
				return fmt.Errorf("%s after %s", strings.Join(missing, "; "), timeout)
			}
		}
		seen = append(seen, state.heads)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollEvery):
		}
	}
}

// readConsumers reads the feeds' heads, the newest read of the project's daemon and, with facts,
// the consumer's cursors, its record of the whole read and the outbox.
func readConsumers(ctx context.Context, pool *store.Pool, project string, facts bool) (consumerState, error) {
	state := consumerState{facts: map[string]int64{}}
	if err := pool.QueryRow(ctx, `
		select coalesce((select max(id) from events), 0), coalesce((select max(id) from legion_observations), 0),
		       coalesce((select max(seq) from delivery_changes), 0)
	`).Scan(&state.heads.events, &state.heads.observations, &state.heads.changes); err != nil {
		return consumerState{}, fmt.Errorf("read the feeds' heads: %w", err)
	}
	err := pool.QueryRow(ctx, `select ok, read_at from legion_reads where project = $1 order by read_at desc limit 1`, project).
		Scan(&state.readOK, &state.readAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return consumerState{}, fmt.Errorf("read the newest read of %s's daemon: %w", project, err)
	}
	if !facts {
		return state, nil
	}
	// Read after the heads, so a cursor at or past a head read above stands at or past every row
	// committed before that read.
	rows, err := pool.Query(ctx, `select name, position from issue_facts_cursor`)
	if err != nil {
		return consumerState{}, fmt.Errorf("read the Legion facts' cursors: %w", err)
	}
	var name string
	var position int64
	if _, err := pgx.ForEachRow(rows, []any{&name, &position}, func() error {
		state.facts[name] = position
		return nil
	}); err != nil {
		return consumerState{}, fmt.Errorf("read the Legion facts' cursors: %w", err)
	}
	if err := pool.QueryRow(ctx, `
		select coalesce((select computed_build from issue_facts_computed where project_key = $1), ''),
		       (select count(*) from delivery_changes_pending)
	`, project).Scan(&state.computed, &state.pending); err != nil {
		return consumerState{}, fmt.Errorf("read the Legion facts' record and the outbox: %w", err)
	}
	return state, nil
}

// caughtUp is what the server still owes, none once it has caught up: a good read of the
// project's daemon; and with facts the project read whole under build, each of the consumer's
// three cursors at or past previous, where its feed stood lagPolls polls before (a feed the
// server's own passes keep writing is never still, so a cursor is held to a head it has had whole
// passes to reach), and an empty outbox.
func (s consumerState) caughtUp(project, build string, facts bool, previous *heads) []string {
	var missing []string
	if previous == nil {
		missing = append(missing, fmt.Sprintf("%d polls, the heads the cursors are held to", lagPolls+1))
	}
	if !s.readOK {
		missing = append(missing, fmt.Sprintf("a good read of %s's daemon (the stand-in)", project))
	}
	if !facts {
		return missing
	}
	if s.computed != build {
		missing = append(missing, fmt.Sprintf("the Legion facts of %s read whole under %s (marked %q)", project, build, s.computed))
	}
	if previous != nil {
		for _, feed := range []struct {
			name string
			head int64
		}{{"events", previous.events}, {"legion_observations", previous.observations}, {"delivery_changes", previous.changes}} {
			if s.facts[feed.name] < feed.head {
				missing = append(missing, fmt.Sprintf("the %s cursor at %d, behind %d", feed.name, s.facts[feed.name], feed.head))
			}
		}
	}
	if s.pending > 0 {
		missing = append(missing, fmt.Sprintf("%d rows left in the outbox", s.pending))
	}
	return missing
}

// summary is one line of the state.
func (s consumerState) summary() string {
	read := "none"
	if s.readAt != nil {
		read = fmt.Sprintf("%s ok=%t", s.readAt.UTC().Format(time.RFC3339), s.readOK)
	}
	line := fmt.Sprintf("heads events=%d observations=%d changes=%d daemon_read=%s", s.heads.events, s.heads.observations,
		s.heads.changes, read)
	if len(s.facts) > 0 || s.computed != "" {
		line += fmt.Sprintf(" facts_cursors=%v computed_build=%q pending=%d", s.facts, s.computed, s.pending)
	}
	return line
}
