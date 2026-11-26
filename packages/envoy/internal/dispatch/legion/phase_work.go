package legion

// phase_work.go holds the one read of the phase-work half of tree.go's rule: which issues carry
// Legion phase work. Its two readers ask it differently. The metrics read it over the whole log,
// with the rest of their population (PhaseWorkKeys, legion/metrics). The Legion board reads it
// only for the issues its tree walk can climb onto that the daemon reports nothing of, and only
// on a board read that meets one (PhaseWorkOf, api/legion_board.go's readPhaseWork). Both come
// through one read (readPhaseWork), under one title rule, and the board's is that read over the
// issues it names rather than a second rule, so an issue carries the same phase work whichever
// asks, and the two place it in the same tree.

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// PhaseWorkKeys is every issue that carries Legion phase work, over the whole log of every
// project: the issues a handoff was recorded for, and the issues an event of a session titled
// `Legion <role> · <that issue's key>` was written on. The metrics read it, once per answer beside
// the rest of their population (legion/metrics' readPhaseWork). It takes no project, so it answers
// every project's issues and the metrics look up their own project's among them; and it takes no
// window, since an issue's tree is a fact of its project rather than of the window a metric
// measures (tree.go).
//
// The title rule is this package's own (WorkerTitle and RoleSessionWorks, session_title.go), and
// the titles are resolved the one way Dispatch resolves them, each session's newest
// (delivery.ResolveSessionTitles), so a session renamed after it wrote, or an event that carried
// no title of its own, counts under the title the session has now.
func PhaseWorkKeys(ctx context.Context, pool *store.Pool) (map[string]struct{}, error) {
	return readPhaseWork(ctx, pool, nil)
}

// PhaseWorkOf is which of issues carry Legion phase work: PhaseWorkKeys's answer for those issues
// alone, read from their own rows. An issue's phase work is the handoffs recorded for it and the
// events written on it, each under its session's newest title, so the answer for one issue reads
// no row of any other, and the two reads agree on every issue both answer. An empty list reads
// nothing.
func PhaseWorkOf(ctx context.Context, pool *store.Pool, issues []string) (map[string]struct{}, error) {
	if len(issues) == 0 {
		return map[string]struct{}{}, nil
	}
	return readPhaseWork(ctx, pool, issues)
}

// readPhaseWork is the one read both answer from: every issue it read that carries Legion phase
// work, of the issues issues names, or of every issue where it is nil.
func readPhaseWork(ctx context.Context, pool *store.Pool, issues []string) (map[string]struct{}, error) {
	keys := map[string]struct{}{}
	if err := readHandoffIssues(ctx, pool, issues, keys); err != nil {
		return nil, err
	}
	written, sessions, err := readSessionIssues(ctx, pool, issues)
	if err != nil {
		return nil, err
	}
	titles, err := delivery.ResolveSessionTitles(ctx, pool, sessions)
	if err != nil {
		return nil, fmt.Errorf("resolve the session titles of the Legion phase work: %w", err)
	}
	for _, one := range written {
		if RoleSessionWorks(titles[one.session], one.issue) {
			keys[one.issue] = struct{}{}
		}
	}
	return keys, nil
}

// readHandoffIssues takes every issue a handoff was recorded for into keys: of issues alone where
// it names any, by handoffs' unique (issue_key, phase, version), and of the whole table where it
// is nil.
func readHandoffIssues(ctx context.Context, pool *store.Pool, issues []string, keys map[string]struct{}) error {
	query, args := `select distinct issue_key from handoffs`, []any(nil)
	if issues != nil {
		query, args = `select distinct issue_key from handoffs where issue_key = any($1)`, []any{issues}
	}
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("read the issues a handoff was recorded for: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return fmt.Errorf("scan an issue a handoff was recorded for: %w", err)
		}
		keys[key] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the issues a handoff was recorded for: %w", err)
	}
	return nil
}

// sessionIssue is one session that wrote an event on one issue, a candidate for phase work until
// the session's title is resolved.
type sessionIssue struct{ issue, session string }

// sessionWrote is the condition an event a session wrote on an issue meets, the one both shapes of
// readSessionIssues read.
const sessionWrote = `issue_key is not null and actor->>'kind' = 'session' and actor->>'id' is not null`

// readSessionIssues is every (issue, session) pair an event names, once each, except the Legion
// daemon's session, whose title can never make it a Legion role. It returns those sessions as a
// sorted list for the title read: of issues alone where it names any, and of the whole log where
// it is nil. One row per pair is all the rule needs: whether a session the title rule makes a
// Legion role wrote on the issue at all.
//
// The narrowed read takes those issues' rows by events' unique (issue_key, seq) first and keeps
// the sessions' among them after. Written as one condition beside sessionWrote, the planner, which
// holds no statistics on actor's fields, takes the partial index on a session's id for a
// selective one and scans every session event in the log to intersect it with the issues' rows:
// over a million events, PhaseWorkOf of three issues of fifty events each took 119 ms that way
// and takes 1.6 ms this one.
func readSessionIssues(ctx context.Context, pool *store.Pool, issues []string) ([]sessionIssue, []string, error) {
	query, args := `select distinct issue_key, actor->>'id' from events where `+sessionWrote, []any(nil)
	if issues != nil {
		query, args = `
			with scoped as materialized (select issue_key, actor from events where issue_key = any($1))
			select distinct issue_key, actor->>'id' from scoped where `+sessionWrote, []any{issues}
	}
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("read the sessions that wrote on the issues: %w", err)
	}
	defer rows.Close()
	var written []sessionIssue
	sessions := map[string]struct{}{}
	for rows.Next() {
		var one sessionIssue
		if err := rows.Scan(&one.issue, &one.session); err != nil {
			return nil, nil, fmt.Errorf("scan a session that wrote on an issue: %w", err)
		}
		// The daemon's own session has no title and cannot be a Legion role (DaemonSession).
		if DaemonSession(one.session) {
			continue
		}
		written = append(written, one)
		sessions[one.session] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("read the sessions that wrote on the issues: %w", err)
	}
	return written, slices.Sorted(maps.Keys(sessions)), nil
}
