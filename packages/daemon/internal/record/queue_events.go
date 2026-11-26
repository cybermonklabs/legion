package record

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/signals"
)

// OpenQueuePull identifies one pull request with an open wait in queue_events: its canonical
// repository and number, which the boot reconciliation uses to read timelines for open waits that
// GitHub's live mergeQueue snapshot no longer shows.
type OpenQueuePull struct {
	Repo   string
	Number int
}

// KeepQueueEvents keeps events, the merge-queue timeline events of one pull request of project in
// GitHub's timeline order, once each by GitHub's event id (project, event_id). An event's place in
// events is kept beside its time, so events in the same second (a merge and the merge queue's
// removal) stay in GitHub's order. An event kept already keeps its time, direction and place, so
// a duplicate delivery and a boot read that overlaps a webhook's are one row each, and takes the
// repository the newer read names: GitHub names an event by the repository as it is called now,
// so after a rename between two reads every event of the pull request is under the new name, and
// an entry read under the old one is not left open beside an exit read under the new one.
//
// It then deletes every pull request of project whose newest event is an exit at or before now
// less signals.QueueExitSpan: the queue signal of a moment counts the exits of the 6 hours before
// it (signals.QueueWait), and no read is of a moment before now, so no read counts those again.
// The rows a read meets are so bounded by the pull requests that left in the 6 hours and by those
// still in the queue, and never by the project's whole history. A pull request with no exit stays
// however old its entry is. A timeline older than that, read late, is so deleted as it is kept.
func (s *Postgres) KeepQueueEvents(ctx context.Context, tx pgx.Tx, project string, events []signals.QueueEvent, now time.Time) error {
	for i, e := range events {
		if e.ID <= 0 || e.Repo == "" || e.PullRequest <= 0 {
			return fmt.Errorf("keep queue event %+v of %s: it needs a positive event id, a repository and a pull request", e, project)
		}
		if _, err := tx.Exec(ctx, `insert into queue_events (project, event_id, repo, pull_request, at, enters, position)
			values ($1, $2, $3, $4, $5, $6, $7)
			on conflict (project, event_id) do update set repo = excluded.repo where queue_events.repo <> excluded.repo`,
			project, e.ID, e.Repo, e.PullRequest, e.At.UTC(), e.Enters, i); err != nil {
			return fmt.Errorf("keep queue event %d (%s#%d) of %s: %w", e.ID, e.Repo, e.PullRequest, project, err)
		}
	}
	if _, err := tx.Exec(ctx, `delete from queue_events
		where project = $1 and (repo, pull_request) in (
			select repo, pull_request from (
				select distinct on (repo, pull_request) repo, pull_request, enters, at
				from queue_events
				where project = $1
				order by repo, pull_request, at desc, position desc, event_id desc
			) newest
			where not enters and at <= $2)`,
		project, now.UTC().Add(-signals.QueueExitSpan)); err != nil {
		return fmt.Errorf("forget the queue events of %s that left the queue 6 hours before %s: %w", project, now, err)
	}
	return nil
}

// QueueEventsAt returns the merge-queue timeline events of project that the queue signal at at
// reads through signals.WaitsOf: every kept event of each pull request that stands in the queue
// (its newest event enters it, however old) or left it in the 6 hours before at
// (signals.QueueExitSpan), ordered for each pull request by GitHub's time, then its place in
// GitHub's timeline, then its id. KeepQueueEvents keeps no other pull request, so the read is
// bounded by the window and not by the project's history.
func (s *Postgres) QueueEventsAt(ctx context.Context, tx pgx.Tx, project string, at time.Time) ([]signals.QueueEvent, error) {
	rows, err := tx.Query(ctx, `
		with newest as (
			select distinct on (repo, pull_request) repo, pull_request, enters
			from queue_events
			where project = $1
			order by repo, pull_request, at desc, position desc, event_id desc
		),
		relevant as (
			select repo, pull_request from newest where enters
			union
			select repo, pull_request from queue_events where project = $1 and not enters and at > $2
		)
		select q.event_id, q.repo, q.pull_request, q.at, q.enters
		from queue_events q
		join relevant r on r.repo = q.repo and r.pull_request = q.pull_request
		where q.project = $1
		order by q.repo, q.pull_request, q.at, q.position, q.event_id`,
		project, at.UTC().Add(-signals.QueueExitSpan))
	if err != nil {
		return nil, fmt.Errorf("read the queue events of %s at %s: %w", project, at, err)
	}
	events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (signals.QueueEvent, error) {
		var e signals.QueueEvent
		if err := row.Scan(&e.ID, &e.Repo, &e.PullRequest, &e.At, &e.Enters); err != nil {
			return signals.QueueEvent{}, err
		}
		e.At = e.At.UTC()
		return e, nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the queue events of %s at %s: %w", project, at, err)
	}
	return events, nil
}

// OpenQueuePulls lists each pull request of project whose newest kept merge-queue event enters the
// queue (an open wait), in repository and number order: the pull requests the boot reads the
// timeline of when GitHub's live mergeQueue snapshot no longer shows them.
func (s *Postgres) OpenQueuePulls(ctx context.Context, tx pgx.Tx, project string) ([]OpenQueuePull, error) {
	rows, err := tx.Query(ctx, `
		select repo, pull_request from (
			select distinct on (repo, pull_request) repo, pull_request, enters
			from queue_events
			where project = $1
			order by repo, pull_request, at desc, position desc, event_id desc
		) newest
		where enters
		order by repo, pull_request`, project)
	if err != nil {
		return nil, fmt.Errorf("read the open queue waits of %s: %w", project, err)
	}
	pulls, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (OpenQueuePull, error) {
		var p OpenQueuePull
		err := row.Scan(&p.Repo, &p.Number)
		return p, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the open queue waits of %s: %w", project, err)
	}
	return pulls, nil
}

// QueueProgress is what the store holds of one pull request's merge-queue moves: how many entries
// and how many exits, and whether the newest of them is an entry, which is a pull request standing
// in the queue.
type QueueProgress struct {
	Entered, Exited int
	Open            bool
}

// Needs is how many events in the direction of a notified move (an entry when enters, an exit
// otherwise) GitHub's timeline must hold before it shows that move, given what the store holds
// already (QueueTimelineRead.MinCount). A move in the direction the pull request already stands
// in, an entry while it is in the queue or an exit while it is out, is one the store holds: the
// notification is a duplicate, or its timeline read came first. Any other is one more than the
// store holds, and at least one where it holds none.
func (p QueueProgress) Needs(enters bool) int {
	kept := p.Exited
	if enters {
		kept = p.Entered
	}
	if enters == p.Open {
		return max(kept, 1)
	}
	return kept + 1
}

// QueueProgressOf is the progress of pull request number of project, over the events kept under
// any of repos (compared without case): a notification names the repository as the record spells
// it, and a kept event names the one GitHub's event url does, which differ after a transfer.
func (s *Postgres) QueueProgressOf(ctx context.Context, tx pgx.Tx, project string, repos []string, number int) (QueueProgress, error) {
	lowered := make([]string, len(repos))
	for i, repo := range repos {
		lowered[i] = strings.ToLower(repo)
	}
	var progress QueueProgress
	var newest *bool
	if err := tx.QueryRow(ctx, `select count(*) filter (where enters), count(*) filter (where not enters),
			(array_agg(enters order by at desc, position desc, event_id desc))[1]
		from queue_events
		where project = $1 and lower(repo) = any($2::text[]) and pull_request = $3`,
		project, lowered, number).Scan(&progress.Entered, &progress.Exited, &newest); err != nil {
		return QueueProgress{}, fmt.Errorf("read the queue moves of %v#%d of %s: %w", repos, number, project, err)
	}
	progress.Open = newest != nil && *newest
	return progress, nil
}

// SetSignalInputReadError records readError as why the boot's read of input for project failed, or
// clears it with "" once a read succeeds, on the row BeginSignalInput writes. A read that fails
// before any boot recorded the input writes the row at at, so the reason is never lost, and a
// later call moves the reason alone, never the moment the input began.
func (s *Postgres) SetSignalInputReadError(ctx context.Context, tx pgx.Tx, project, input, readError string, at time.Time) error {
	if _, err := tx.Exec(ctx, `insert into signal_inputs (project, input, kept_since, read_error)
		values ($1, $2, $3, $4)
		on conflict (project, input) do update set read_error = excluded.read_error`,
		project, input, at.UTC(), readError); err != nil {
		return fmt.Errorf("record why the read of %s for %s failed: %w", input, project, err)
	}
	return nil
}

// SignalInputStatus is the moment the daemon began keeping input for project, in UTC, whether one
// is recorded, and why the boot's read of the input failed, "" where it did not (SetSignalInputReadError).
func (s *Postgres) SignalInputStatus(ctx context.Context, tx pgx.Tx, project, input string) (time.Time, bool, string, error) {
	var since time.Time
	var readError string
	err := tx.QueryRow(ctx, "select kept_since, read_error from signal_inputs where project = $1 and input = $2", project, input).Scan(&since, &readError)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, "", nil
	}
	if err != nil {
		return time.Time{}, false, "", fmt.Errorf("read the status of %s for %s: %w", input, project, err)
	}
	return since.UTC(), true, readError, nil
}
