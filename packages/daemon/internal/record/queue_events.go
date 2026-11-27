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
// It then counts the entries and the exits that each of those pull requests holds kept, and keeps
// for each count the greater of it and the one queue_progress holds already, so the counts hold
// every move the store has kept of the pull request, those whose events the delete below removed
// among them: a notified move is checked against them (QueueProgress.Needs), and a timeline read
// that still shows only the moves before it is read again. A timeline read is the whole of GitHub's
// timeline, so a read after the delete keeps the old moves anew, and they are never counted twice.
//
// It then deletes every pull request of project whose newest event is an exit at or before now
// less signals.QueueExitSpan: the queue signal of a moment counts the exits of the 6 hours before
// it (signals.QueueWait), and no read is of a moment before now, so no read counts those again.
// The rows a read meets are so bounded by the pull requests that left in the 6 hours and by those
// still in the queue, and never by the project's whole history. A pull request with no exit stays
// however old its entry is. A timeline older than that, read late, is so deleted as it is kept.
// The counts of queue_progress stay, one row of each pull request, which only QueueProgressOf
// reads, by its key.
func (s *Postgres) KeepQueueEvents(ctx context.Context, tx pgx.Tx, project string, events []signals.QueueEvent, now time.Time) error {
	batch := &pgx.Batch{}
	for i, e := range events {
		if e.ID <= 0 || e.Repo == "" || e.PullRequest <= 0 {
			return fmt.Errorf("keep queue event %+v of %s: it needs a positive event id, a repository and a pull request", e, project)
		}
		batch.Queue(`insert into queue_events (project, event_id, repo, pull_request, at, enters, position)
			values ($1, $2, $3, $4, $5, $6, $7)
			on conflict (project, event_id) do update set repo = excluded.repo where queue_events.repo <> excluded.repo`,
			project, e.ID, e.Repo, e.PullRequest, e.At.UTC(), e.Enters, i)
	}
	// One round trip keeps every event, each statement's rows marked in the order queued.
	if batch.Len() > 0 {
		results := tx.SendBatch(ctx, batch)
		for _, e := range events {
			tag, err := results.Exec()
			if err != nil {
				_ = results.Close()
				return fmt.Errorf("keep queue event %d (%s#%d) of %s: %w", e.ID, e.Repo, e.PullRequest, project, err)
			}
			noteRowsWritten(ctx, tag)
		}
		if err := results.Close(); err != nil {
			return fmt.Errorf("keep the queue events of %s: %w", project, err)
		}
		repos, pulls := make([]string, len(events)), make([]int, len(events))
		for i, e := range events {
			repos[i], pulls[i] = e.Repo, e.PullRequest
		}
		if _, err := tx.Exec(ctx, `insert into queue_progress (project, repo, pull_request, entered, exited)
			select $1, repo, pull_request, count(*) filter (where enters), count(*) filter (where not enters)
			from queue_events
			where project = $1 and (repo, pull_request) in (select * from unnest($2::text[], $3::integer[]))
			group by repo, pull_request
			on conflict (project, pull_request, repo) do update
			set entered = greatest(queue_progress.entered, excluded.entered), exited = greatest(queue_progress.exited, excluded.exited)
			where queue_progress.entered < excluded.entered or queue_progress.exited < excluded.exited`,
			project, repos, pulls); err != nil {
			return fmt.Errorf("count the queue moves %s keeps: %w", project, err)
		}
	}
	if _, err := tx.Exec(ctx, `delete from queue_events
		where project = $1 and (repo, pull_request) in (
			select repo, pull_request from (`+newestQueueEvents+`) newest
			where not enters and at <= $2)`,
		project, now.UTC().Add(-signals.QueueExitSpan)); err != nil {
		return fmt.Errorf("forget the queue events of %s that left the queue 6 hours before %s: %w", project, now, err)
	}
	return nil
}

// newestQueueEvents is the newest kept event of each pull request of project $1, by GitHub's time,
// then its place in GitHub's timeline, then its id: its repository, number, direction and time,
// which says whether the pull request stands in the queue and since when it left.
const newestQueueEvents = `select distinct on (repo, pull_request) repo, pull_request, enters, at
	from queue_events
	where project = $1
	order by repo, pull_request, at desc, position desc, event_id desc`

// QueueEventsAt returns the merge-queue timeline events of project that the queue signal at at
// reads through signals.WaitsOf: every kept event of each pull request that stands in the queue
// (its newest event enters it, however old) or left it in the 6 hours before at
// (signals.QueueExitSpan), ordered for each pull request by GitHub's time, then its place in
// GitHub's timeline, then its id. KeepQueueEvents keeps no other pull request, so the read is
// bounded by the window and not by the project's history.
func (s *Postgres) QueueEventsAt(ctx context.Context, tx pgx.Tx, project string, at time.Time) ([]signals.QueueEvent, error) {
	rows, err := tx.Query(ctx, `
		with newest as (`+newestQueueEvents+`),
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
	rows, err := tx.Query(ctx, `select repo, pull_request from (`+newestQueueEvents+`) newest
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

// QueueProgress is what the store holds of one pull request's merge-queue moves: whether it has
// kept any (Kept, a row of queue_progress), how many entries and how many exits it has kept, the
// moves whose events the 6-hour delete removed among them (KeepQueueEvents), and whether the newest
// kept event is an entry, which is a pull request standing in the queue. A pull request the store
// has kept no move of may still have moves on GitHub's timeline, such as those before the first
// boot of the release that keeps them, which its counts of 0 cannot tell from a move just notified
// (QueueTimelineRead.Unkept).
type QueueProgress struct {
	Kept            bool
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

// QueueProgressOf is the progress of pull request number of project, over the counts and the
// events kept under any of repos (compared without case): a notification names the repository as
// the record spells it, and a kept event names the one GitHub's event url does, which differ after
// a transfer. The counts are queue_progress's, the greatest kept under any of the names, and Kept
// is whether it holds a row under one of them, so a pull request whose events the 6-hour delete
// removed still counts the moves they were; it has no event left only where its newest one was an
// exit, or where GitHub has no timeline of it (ForgetQueuePull), and reads as out of the queue.
func (s *Postgres) QueueProgressOf(ctx context.Context, tx pgx.Tx, project string, repos []string, number int) (QueueProgress, error) {
	var progress QueueProgress
	var newest *bool
	if err := tx.QueryRow(ctx, `select count(*) > 0, coalesce(max(entered), 0), coalesce(max(exited), 0),
			(select enters from queue_events
				where project = $1 and lower(repo) = any($2::text[]) and pull_request = $3
				order by at desc, position desc, event_id desc limit 1)
		from queue_progress
		where project = $1 and pull_request = $3 and lower(repo) = any($2::text[])`,
		project, lowerRepos(repos), number).Scan(&progress.Kept, &progress.Entered, &progress.Exited, &newest); err != nil {
		return QueueProgress{}, fmt.Errorf("read the queue moves of %v#%d of %s: %w", repos, number, project, err)
	}
	progress.Open = newest != nil && *newest
	return progress, nil
}

// ForgetQueuePull deletes every kept event of pull request number of project under any of repos
// (compared without case, as QueueProgressOf reads them): GitHub answered that its timeline is not
// there (404 or 410), which no later read answers otherwise, so an entry kept with no exit would
// read as the longest wait of the queue for good, and the boot would read the timeline again at
// each start. Its counts of queue_progress stay, which the events it deletes are part of.
func (s *Postgres) ForgetQueuePull(ctx context.Context, tx pgx.Tx, project string, repos []string, number int) error {
	tag, err := tx.Exec(ctx, `delete from queue_events where project = $1 and lower(repo) = any($2::text[]) and pull_request = $3`,
		project, lowerRepos(repos), number)
	if err != nil {
		return fmt.Errorf("forget the queue events of %v#%d of %s: %w", repos, number, project, err)
	}
	noteRowsWritten(ctx, tag)
	return nil
}

// lowerRepos is each of repos in lower case, as the queue's reads under several names compare them.
func lowerRepos(repos []string) []string {
	lowered := make([]string, len(repos))
	for i, repo := range repos {
		lowered[i] = strings.ToLower(repo)
	}
	return lowered
}

// SetSignalInputReadError records readError as why the boot's read of input for project failed, or
// clears it with "" once a read succeeds, on the row BeginSignalInput writes. A read that fails
// before any boot recorded the input writes the row at at, so the reason is never lost, and a
// later call moves the reason alone, never the moment the input began.
func (s *Postgres) SetSignalInputReadError(ctx context.Context, tx pgx.Tx, project, input, readError string, at time.Time) error {
	tag, err := tx.Exec(ctx, `insert into signal_inputs (project, input, kept_since, read_error)
		values ($1, $2, $3, $4)
		on conflict (project, input) do update set read_error = excluded.read_error
		where signal_inputs.read_error <> excluded.read_error`,
		project, input, at.UTC(), readError)
	if err != nil {
		return fmt.Errorf("record why the read of %s for %s failed: %w", input, project, err)
	}
	noteRowsWritten(ctx, tag)
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
