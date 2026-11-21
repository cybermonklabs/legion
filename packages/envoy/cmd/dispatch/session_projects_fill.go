package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	dispatchenvoy "github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// sessionProjectsFillBatch is how many events, in id order, one statement of
// `envoy-dispatch backfill-session-projects` reads.
const sessionProjectsFillBatch = 10_000

// backfillSessionProjects runs `envoy-dispatch backfill-session-projects [--reset]`: it records in
// session_projects the (session, project) pair of every event written before the live insert
// existed (fillSessionProjects), then writes the one session_projects_filled row. It runs after the
// deploy whose events.Broker.QueueAppend records each new event's pair, so every event is covered by
// the one or the other. It is safe to run again, and at any time: a pair a row holds already is
// not written again.
//
// --reset first empties session_projects of every pair an event proved and session_projects_filled,
// each in a statement of its own, as migration 20261116081157_session_projects_proven does at boot,
// and then fills. A pair the listener's answer proved (proven_by `registration`,
// events.Registrations) stays: no event can write it back. A deployment that rolls keeps an old
// build recording pairs under the old rule while the new one starts, and a fill never takes a pair
// away, so its operator runs it once the last old pod is gone. From the empty to the marker's row,
// an open viewer of a deployment that holds a private project reads no session but its own
// (access.SessionClause).
//
// After the event pass it asks the listener (listener, ENVOY_URL and ENVOY_TOKEN) for each session
// a stored direct message names as its recipient, the stopped ones included, and records the
// projects each answer lists, as every read of a session through the listener records them
// (fillRegistrations). A session the listener no longer knows is printed by id with its count of
// messages, so a person reads it before a project is switched private. It exits 0 once it has read
// every event and asked about every recipient, 1 when the database cannot be opened or written or
// the listener does not answer, and 2 for an argument it does not take.
func backfillSessionProjects(ctx context.Context, args []string, databaseURL string, listener func(*store.Pool) *dispatchenvoy.Client, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("backfill-session-projects", flag.ContinueOnError)
	flags.SetOutput(stderr)
	reset := flags.Bool("reset", false, "empty session_projects of the pairs events proved, and its marker, first, then fill")
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
		for _, statement := range []string{`delete from session_projects where proven_by = 'event'`, `delete from session_projects_filled`} {
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
	var client *dispatchenvoy.Client
	if listener != nil {
		client = listener(database.Pool)
	}
	registered, err := fillRegistrations(ctx, database.Pool, client, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "backfill-session-projects: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "backfill-session-projects: events=%d pairs=%d recipients=%d unknown=%d filled\n",
		report.events, report.pairs, registered.asked, registered.unknown)
	return 0
}

// registrationsFill is what the recipients' pass did: the sessions it asked the listener about,
// and the ones the listener no longer knows.
type registrationsFill struct {
	asked, unknown int
}

// fillRegistrations asks listener, which records through the same recorder every read of a session
// does (events.SessionRegistrations), about every session a stored direct message names as its
// recipient (messages.target `session:<id>`), stopped ones included (SessionIncludingStopped), so
// each answer's projects are recorded proven_by `registration`. A session the listener answers it
// does not know is printed with its count of messages, for a person to read before a project is
// switched private: nothing Dispatch holds says which projects such a session works for, and the
// session rule passes it to no viewer of a private deployment but its own when it has no pair
// (access.SessionClause). A nil listener asks nothing.
func fillRegistrations(ctx context.Context, pool *store.Pool, listener *dispatchenvoy.Client, stdout io.Writer) (registrationsFill, error) {
	var report registrationsFill
	if listener == nil {
		return report, nil
	}
	rows, err := pool.Query(ctx, `
		select substr(target, 9), count(*)
		from messages
		where target like 'session:%' and length(target) > 8
		group by 1
		order by 1
	`)
	if err != nil {
		return report, fmt.Errorf("read the sessions stored direct messages name: %w", err)
	}
	type recipient struct {
		session  string
		messages int64
	}
	var recipients []recipient
	for rows.Next() {
		var r recipient
		if err := rows.Scan(&r.session, &r.messages); err != nil {
			rows.Close()
			return report, fmt.Errorf("read the sessions stored direct messages name: %w", err)
		}
		recipients = append(recipients, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return report, fmt.Errorf("read the sessions stored direct messages name: %w", err)
	}
	for _, r := range recipients {
		report.asked++
		_, err := listener.SessionIncludingStopped(ctx, r.session)
		switch {
		case errors.Is(err, dispatchenvoy.ErrNotFound):
			report.unknown++
			fmt.Fprintf(stdout, "backfill-session-projects: the listener does not know session %s, which %d stored messages name\n", r.session, r.messages)
		case err != nil:
			return report, fmt.Errorf("ask the listener for session %s's projects: %w", r.session, err)
		}
	}
	return report, nil
}

// fillListener is the listener client the fill asks about the recipients' registrations through:
// ENVOY_URL, its default when unset, with ENVOY_TOKEN, recording each answer through pool's
// recorder (events.SessionRegistrations), as the server's own client does.
func fillListener(env settingValues) func(*store.Pool) *dispatchenvoy.Client {
	envoyURL := env.get("ENVOY_URL")
	if envoyURL == "" {
		envoyURL = defaultEnvoyURL
	}
	token := env.get("ENVOY_TOKEN")
	return func(pool *store.Pool) *dispatchenvoy.Client {
		return dispatchenvoy.New(envoyURL, dispatchenvoy.WithToken(token),
			dispatchenvoy.WithRegistrations(events.SessionRegistrations(pool)))
	}
}

// sessionProjectsFill is what a fill did: the events it read, and the pairs it wrote.
type sessionProjectsFill struct {
	events, pairs int64
}

// fillSessionProjects reads every event in id order, batch at a time, one statement and so one
// transaction each, and records each pair the live insert (events.queueSessionProject) records:
// an event whose actor is a session its credential proved - `kind` session, a non-empty `id`, and
// an `act` naming that same session and no personal token (events.provedSession) - on a project,
// which is the event's own project_key, else its issue's for an issue's event, else its
// document's for a document's event. A person's event, a session a request body named, and an
// event of no project record nothing. The project is the one the issue or document is in when the
// fill reads it. A pair is written only where no row holds it, as the live insert writes it, since
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
