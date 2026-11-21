package events

import (
	"context"
	"fmt"
	"slices"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Registrations records the projects the listener answers a session's registration with
// (model.SessionProjects) in session_projects, proven_by `registration`, beside the pairs an event's
// proved session records (queueSessionProject): the listener's answer is the one proof a session
// that registered under a project and wrote no event there has, and access.SessionClause reads
// every pair alike. The envoy client calls it for every session it answers whose list names a
// project (dispatch/envoy.WithRegistrations), before the call returns.
type Registrations struct {
	pool *store.Pool
}

// SessionRegistrations is the recorder of the listener's answers that writes through pool.
func SessionRegistrations(pool *store.Pool) *Registrations {
	return &Registrations{pool: pool}
}

// recordRegistrationsSQL inserts, in one statement, each (session, project) pair of $1 and $2,
// position by position, that no row holds, proven_by `registration`. session_projects has no
// unique key, on purpose (queueSessionProject says why), so a pair is written only where no
// committed row holds it: two calls open at once may both write one, and every reader asks only
// whether a row exists. A pair is never removed here; more pairs only hide a session from more
// viewers.
const recordRegistrationsSQL = `
	insert into session_projects (session_id, project_key, proven_by)
	select pair.session_id, pair.project_key, 'registration'
	from unnest($1::text[], $2::text[]) as pair(session_id, project_key)
	where not exists (
		select 1 from session_projects recorded
		where recorded.session_id = pair.session_id and recorded.project_key = pair.project_key)
`

// Record writes the pairs of answers no row holds, in one statement however many sessions answers
// names: the envoy client's Sessions answers every session the listener holds, and makes one call.
// A session whose list is empty - an all-projects token, or a record from before the listener kept
// each session's projects - and a pair named twice record nothing more. An error is the call's,
// and the envoy client returns it as its own read's, so nothing that resolves a session through the
// listener goes on without the row.
func (r *Registrations) Record(ctx context.Context, answers []model.SessionProjects) error {
	var sessions, projects []string
	seen := map[[2]string]bool{}
	for _, answer := range answers {
		if answer.SessionID == "" {
			continue
		}
		for _, project := range answer.Projects {
			pair := [2]string{answer.SessionID, project}
			if project == "" || seen[pair] {
				continue
			}
			seen[pair] = true
			sessions = append(sessions, answer.SessionID)
			projects = append(projects, project)
		}
	}
	if len(sessions) == 0 {
		return nil
	}
	if _, err := r.pool.Exec(ctx, recordRegistrationsSQL, sessions, projects); err != nil {
		return fmt.Errorf("record the projects the listener answered for %d sessions: %w", len(slices.Compact(slices.Sorted(slices.Values(sessions)))), err)
	}
	return nil
}
