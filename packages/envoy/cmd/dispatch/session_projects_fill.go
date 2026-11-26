package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// sessionProjectsFillBatch is how many events, in id order, one statement of
// `envoy-dispatch backfill-session-projects` reads.
const sessionProjectsFillBatch = 10_000

// backfillSessionProjects runs `envoy-dispatch backfill-session-projects [--reset]`: it records in
// session_projects the (session, project) pair, and in session_owners the (session, person) pair,
// of every event written before the live insert existed (fillSessionProjects), then writes the one
// session_projects_filled row. It runs after the deploy whose events.Broker.QueueAppend records
// each new event's pairs, so every event is covered by the one or the other. It is safe to run
// again, and at any time: a pair a row holds already is not written again.
//
// --reset first empties session_projects and session_projects_filled, each in a statement of its
// own, as migration 20261116081157_session_projects_proven does at boot, and then fills. A
// deployment that rolls keeps an old build recording pairs under the old rule while the new one
// starts, and a fill never takes a pair away, so its operator runs it once the last old pod is
// gone. From the empty to the marker's row, no viewer of a deployment that holds a private project,
// open or not, reads a session but its own (access.SessionClause). session_owners is not emptied: no
// build recorded its pairs under another rule. The reset, each batch that writes a pair and the
// marker's row announce themselves on events.SessionAccessChannel, since the session content rule
// reads all three, so every server's document sockets ask their rule again before their next
// frame. It exits 0 once it has read every event, 1 when the database cannot be opened or
// written, and 2 for an argument it does not take.
func backfillSessionProjects(ctx context.Context, args []string, databaseURL string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("backfill-session-projects", flag.ContinueOnError)
	flags.SetOutput(stderr)
	reset := flags.Bool("reset", false, "empty session_projects and its marker first, then fill")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "backfill-session-projects: unexpected argument %q; the one flag is --reset\n", flags.Arg(0))
		return 2
	}
	database, ok := openMigrated(ctx, "backfill-session-projects", databaseURL, stderr)
	if !ok {
		return 1
	}
	defer database.Pool.Close()
	ctx = store.WithTransactionTracking(ctx)
	if *reset {
		for _, statement := range []string{`delete from session_projects`, `delete from session_projects_filled`, `select ` + events.AnnounceSessionAccess} {
			if _, err := database.Pool.Exec(ctx, statement); err != nil {
				fmt.Fprintf(stderr, "backfill-session-projects: --reset: %s: %v\n", statement, err)
				return 1
			}
		}
	}
	report, err := fillSessionProjects(ctx, database.Pool, sessionProjectsFillBatch)
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
// for an event whose actor is a session its credential proved - `kind` session, a non-empty `id`,
// and an `act` naming that same session and no personal token (events.provedSession) - the
// session's project, which is the event's own project_key, else its issue's for an issue's event,
// else its document's for a document's event, and the person its `owner` names, canonical by
// model.CanonicalLogin's rule. A person's event and a session a request body named record nothing,
// and an event of no project records no project. The project is the one the issue or document is
// in when the fill reads it. A pair is written only where no row holds it, as the live insert
// writes it, since neither table has a unique key (migrations 20261116015736_project_access and
// 20261121130000_session_owners). Once a batch comes back short, every event is read, and it
// writes the one session_projects_filled row, unless a run before it did.
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
					and b.actor->'act'->>'session' = b.actor->>'id'
					and coalesce(b.actor->'act'->>'token_id', '') = ''
			), inserted as (
				insert into session_projects (session_id, project_key)
				select p.session_id, p.project_key
				from pairs p
				where p.project_key is not null
					and not exists (
						select 1 from session_projects recorded
						where recorded.session_id = p.session_id and recorded.project_key = p.project_key)
				returning 1
			), owners as (
				-- The person each proved session's credential acts for, as the live write records
				-- it (events.queueSessionProject), canonical by model.CanonicalLogin's rule.
				select distinct b.actor->>'id' as session_id, `+graphcensus.CanonicalLoginSQL(`b.actor->>'owner'`)+` as login
				from batch b
				where b.actor->>'kind' = 'session' and b.actor->>'id' <> ''
					and b.actor->'act'->>'session' = b.actor->>'id'
					and coalesce(b.actor->'act'->>'token_id', '') = ''
			), owned as (
				insert into session_owners (session_id, login)
				select o.session_id, o.login
				from owners o
				where o.login <> ''
					and not exists (
						select 1 from session_owners recorded
						where recorded.session_id = o.session_id and recorded.login = o.login)
				returning 1
			)
			select (select max(id) from batch), (select count(*) from batch), (select count(*) from inserted),
				(select `+events.AnnounceSessionAccess+` from (select 1 from inserted union all select 1 from owned) written limit 1)
		`, after, batch).Scan(&last, &read, &written, nil); err != nil {
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
		with filled as (
			insert into session_projects_filled (filled_at)
			select now() where not exists (select 1 from session_projects_filled)
			returning 1
		)
		select `+events.AnnounceSessionAccess+` from filled
	`); err != nil {
		return report, fmt.Errorf("record that every event's session project is filled: %w", err)
	}
	return report, nil
}
