package main

import (
	"context"
	"fmt"
	"io"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// accessEventsScrubBatch is how many events, in id order, one statement of
// `envoy-dispatch scrub-access-events` reads.
const accessEventsScrubBatch = 10_000

// scrubAccessEventsCommand runs `envoy-dispatch scrub-access-events`: it rewrites every
// project.access_changed event stored with the project's whole access - the shape the release of
// migration 20261116015736_project_access stored - to the project and the change alone
// (scrubAccessEvents), then writes the one access_events_scrubbed row, which a switch of a project
// to private waits for. It runs beside the server, after the deploy whose builds append only that
// shape, and is safe to run again: an event already of that shape is not written. In a deployment
// that rolls, its operator runs it once the last old pod is gone. It exits 0 once it has read every
// event, 1 when the database cannot be opened or written, and 2 for an argument, which it takes
// none of.
func scrubAccessEventsCommand(ctx context.Context, args []string, databaseURL string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintf(stderr, "scrub-access-events: unexpected argument %q; it takes none\n", args[0])
		return 2
	}
	database, ok := openMigrated(ctx, "scrub-access-events", databaseURL, stderr)
	if !ok {
		return 1
	}
	defer database.Pool.Close()
	report, err := scrubAccessEvents(store.WithTransactionTracking(ctx), database.Pool, accessEventsScrubBatch)
	if err != nil {
		fmt.Fprintf(stderr, "scrub-access-events: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "scrub-access-events: events=%d rewritten=%d scrubbed\n", report.events, report.rewritten)
	return 0
}

// accessEventsScrub is what a scrub did: the events it read, and the ones it rewrote.
type accessEventsScrub struct {
	events, rewritten int64
}

// scrubAccessEvents reads every event in id order, batch at a time, one statement and so one
// transaction each, and rewrites each project.access_changed whose payload holds anything but its
// project and its change to jsonb_build_object('project', ..., 'change', ...). What such a payload
// named - its principal, and the owner and visibility the change left - goes first into the
// server's own record of the event (project_access_changes), where a live resume reads it, unless a
// row holds it already. It reads by id, the primary key, because no index of events leads with
// type; the batch reads each event's type with its id, so only an access event is read again for
// its payload. Once a batch comes back short, every event is read, and it writes the one
// access_events_scrubbed row, unless a run before it did.
func scrubAccessEvents(ctx context.Context, pool *store.Pool, batch int) (accessEventsScrub, error) {
	var report accessEventsScrub
	var after int64
	for {
		var last *int64
		var read, rewritten int64
		if err := pool.QueryRow(ctx, `
			with batch as (
				select id, type from events where id > $1 order by id limit $2
			), stored as (
				select e.id, e.payload
				from batch b join events e on e.id = b.id
				where b.type = 'project.access_changed'
					and e.payload <> jsonb_build_object('project', e.payload->'project', 'change', e.payload->'change')
			), recorded as (
				insert into project_access_changes (event_id, principal, owner, visibility)
				select s.id, coalesce(s.payload->>'principal', ''), coalesce(s.payload->>'owner', ''), coalesce(s.payload->>'visibility', '')
				from stored s
				on conflict (event_id) do nothing
			), rewritten as (
				update events e
				set payload = jsonb_build_object('project', s.payload->'project', 'change', s.payload->'change')
				from stored s
				where e.id = s.id
				returning 1
			)
			select (select max(id) from batch), (select count(*) from batch), (select count(*) from rewritten)
		`, after, batch).Scan(&last, &read, &rewritten); err != nil {
			return report, fmt.Errorf("scrub the access events after id %d: %w", after, err)
		}
		report.events += read
		report.rewritten += rewritten
		if read < int64(batch) {
			break
		}
		after = *last
	}
	if _, err := pool.Exec(ctx, `
		insert into access_events_scrubbed (scrubbed_at)
		select now() where not exists (select 1 from access_events_scrubbed)
	`); err != nil {
		return report, fmt.Errorf("record that every access event is scrubbed: %w", err)
	}
	return report, nil
}
