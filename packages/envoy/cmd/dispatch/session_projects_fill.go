package main

import (
	"context"
	"fmt"
	"io"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// sessionProjectsFillBatch is how many events, in id order, one statement of
// `envoy-dispatch backfill-session-projects` reads.
const sessionProjectsFillBatch = 10_000

// backfillSessionProjects runs `envoy-dispatch backfill-session-projects`: it records in
// session_projects the (session, project) pair of every event written before the live insert
// existed (fillSessionProjects), then writes the one session_projects_filled row. It runs after the
// deploy whose events.Broker.QueueAppend records each new event's pair, so every event is covered by
// the one or the other. It is safe to run again, and at any time: a pair a row holds already is
// not written again. It exits 0 once it has read every event, and 1 when the database cannot be
// opened or written.
func backfillSessionProjects(ctx context.Context, databaseURL string, stdout, stderr io.Writer) int {
	database, ok := openMigrated(ctx, "backfill-session-projects", databaseURL, stderr)
	if !ok {
		return 1
	}
	defer database.Pool.Close()
	report, err := fillSessionProjects(store.WithTransactionTracking(ctx), database.Pool, sessionProjectsFillBatch)
	if err != nil {
		fmt.Fprintf(stderr, "backfill-session-projects: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "backfill-session-projects: events=%d pairs=%d filled\n", report.events, report.pairs)
	return 0
}

// sessionProjectsFill is what a fill did: the events it read, and the pairs it wrote.
type sessionProjectsFill struct {
	events, pairs int64
}

// fillSessionProjects reads every event in id order, batch at a time, one statement and so one
// transaction each, and records each pair the live insert (events.queueSessionProject) records:
// an event whose actor is a session (`kind` session and a non-empty `id`; a personal token's actor
// has an empty one) on a project, which is the event's own project_key, else its issue's for an
// issue's event, else its document's for a document's event. A person's event, and an event of no
// project, records nothing. The project is the one the issue or document is in when the fill reads
// it. A pair is written only where no row holds it, as the live insert writes it, since
// session_projects has no unique key (migration 20261116015736_project_access). Once a batch comes
// back short, every event is read, and it writes the one session_projects_filled row, unless a
// run before it did.
func fillSessionProjects(ctx context.Context, pool *store.Pool, batch int) (sessionProjectsFill, error) {
	var report sessionProjectsFill
	var after int64
	for {
		var last *int64
		var read, written int64
		if err := pool.QueryRow(ctx, `
			with batch as (
				select id, issue_key, artifact_id, project_key, actor
				from events
				where id > $1
				order by id
				limit $2
			), pairs as (
				select distinct b.actor->>'id' as session_id,
					coalesce(b.project_key, i.project_key, a.project_key) as project_key
				from batch b
				left join issues i on i.key = b.issue_key
				left join artifacts a on a.id = b.artifact_id
				where b.actor->>'kind' = 'session' and b.actor->>'id' <> ''
			), inserted as (
				insert into session_projects (session_id, project_key)
				select p.session_id, p.project_key
				from pairs p
				where p.project_key is not null
					and not exists (
						select 1 from session_projects recorded
						where recorded.session_id = p.session_id and recorded.project_key = p.project_key)
				returning 1
			)
			select (select max(id) from batch), (select count(*) from batch), (select count(*) from inserted)
		`, after, batch).Scan(&last, &read, &written); err != nil {
			return report, fmt.Errorf("record the sessions' projects of the events after id %d: %w", after, err)
		}
		report.events += read
		report.pairs += written
		if read < int64(batch) {
			break
		}
		after = *last
	}
	if _, err := pool.Exec(ctx, `
		insert into session_projects_filled (filled_at)
		select now() where not exists (select 1 from session_projects_filled)
	`); err != nil {
		return report, fmt.Errorf("record that every event's session project is filled: %w", err)
	}
	return report, nil
}
