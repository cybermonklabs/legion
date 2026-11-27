package record

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/signals"
)

// ModelRequestsReport is one report of a host's model requests as admission keeps it (an
// intake.ModelRequests): the claim whose launch at Generation its host runs, and the requests.
type ModelRequestsReport struct {
	Token      claim.Token
	Generation uint64
	Requests   []claim.ModelRequest
}

// KeepModelRequests keeps the requests of report as rows of model_requests: each once by the
// claim, the generation and the request's own id, so a request two reports carry, or one report
// delivered twice, is one row and the first one kept stays as it was kept, but for its end. Each
// row keeps the session, the model, when the request ended, its uncached input tokens or none,
// and whether counts takes its model (the project's signal_token_model_pattern as the daemon was
// configured when it kept it), so a changed selector reads no kept request again. It takes the
// wire's own request, as KeepMemoryKill takes the wire's frame.
//
// A request reported again with a later end moves to that end: the Claude Code bridge reports a
// request when its newest block is a while old, and a block Claude Code writes after that report
// is a later end of the same request, which it reports again under the same id
// (claudebridge's modelRequests). The request's tokens, or its count among the requests of no
// token count (untold), leave the minute of its earlier end and join that of its later one, so
// each request counts in the minute of its last block, never of a block before it. An earlier or
// equal end moves nothing, so a retry of an older report never takes a request back.
//
// A request that ended before the first minute a read at now or later reaches
// (modelRequestsReach) is kept as no row, since no read counts it: a host that could not report it
// for an hour still has it answered kept (api's modelRequests) and sends it no more. It then
// deletes every request of project that ended before that minute, so the rows a read meets stay
// bounded by an hour of requests rather than the project's whole history.
//
// It keeps no mark of a write admission reads (RecordsWritten), whatever it keeps: a report comes
// every few seconds from every session, and most change no hold and no value a pass would record,
// so admission decides by the series-moved pass rule whether the report runs the pass (admit's
// signalsMoved, NoteSignalsMoved).
func (s *Postgres) KeepModelRequests(ctx context.Context, tx pgx.Tx, project string, report ModelRequestsReport, counts func(model string) bool, now time.Time) error {
	reach := modelRequestsReach(now)
	batch := &pgx.Batch{}
	for _, request := range report.Requests {
		if request.EndedAt.Before(reach) {
			continue
		}
		// The request adds to its minute's sum (model_request_minutes) only where this statement
		// kept it anew or moved it to another minute, and a move takes it out of its earlier
		// minute's sum, so a request kept already adds nothing and a moved one counts once. Every
		// part reads the table as it stood before the statement: prior is the row kept before, and
		// kept the row the statement wrote, with the tokens and the selector of the first report.
		batch.Queue(`with prior as (
				select date_trunc('minute', ended_at at time zone 'UTC') at time zone 'UTC' as minute,
					case when counted then coalesce(uncached, 0) else 0 end as uncached, (counted and uncached is null)::int as untold
				from model_requests where project = $1 and claim = $2 and generation = $3 and id = $4
			), kept as (
				insert into model_requests (project, claim, generation, id, session, model, ended_at, uncached, counted)
				values ($1, $2, $3, $4, $5, $6, $7, $8, $9)
				on conflict (project, claim, generation, id) do update set ended_at = excluded.ended_at
				where model_requests.ended_at < excluded.ended_at
				returning case when counted then coalesce(uncached, 0) else 0 end as uncached, (counted and uncached is null)::int as untold
			), left_minute as (
				update model_request_minutes m set uncached = m.uncached - prior.uncached, untold = m.untold - prior.untold
				from prior, kept where m.project = $1 and m.minute = prior.minute and prior.minute <> $10
			)
			insert into model_request_minutes (project, minute, uncached, untold)
			select $1, $10, kept.uncached, kept.untold
			from kept where not exists (select from prior where prior.minute = $10)
			on conflict (project, minute) do update set uncached = model_request_minutes.uncached + excluded.uncached,
				untold = model_request_minutes.untold + excluded.untold`,
			project, string(report.Token), int64(report.Generation), request.ID, request.Session, request.Model, request.EndedAt,
			request.UncachedInputTokens, counts(request.Model), request.EndedAt.UTC().Truncate(time.Minute))
	}
	if batch.Len() > 0 {
		if err := tx.SendBatch(ctx, batch).Close(); err != nil {
			return fmt.Errorf("keep the model requests of %s at generation %d: %w", report.Token, report.Generation, err)
		}
	}
	if _, err := tx.Exec(ctx, "delete from model_requests where project = $1 and ended_at < $2", project, reach); err != nil {
		return fmt.Errorf("forget the model requests of %s before %s: %w", project, reach, err)
	}
	if _, err := tx.Exec(ctx, "delete from model_request_minutes where project = $1 and minute < $2", project, reach); err != nil {
		return fmt.Errorf("forget the minutes of the model requests of %s before %s: %w", project, reach, err)
	}
	return nil
}

// modelRequestsReach is the start of the first minute a read of the provider's record at now or
// later reaches: the state shows the hour of minutes before a moment's own, and the headroom
// reads the minute before it (claim.ModelRequestsSpan, signals.ProviderAt), so no read at now or
// after reads a minute before now's less that hour.
func modelRequestsReach(now time.Time) time.Time {
	return now.UTC().Truncate(time.Minute).Add(-claim.ModelRequestsSpan)
}

// KeepTokenLimit records limit, the project's token limit a minute (signal_token_limit_per_minute,
// 0 where the daemon's configuration names none), as the limit of each of project's minutes that
// begins at or after at, where it differs from the limit in force at at: each boot of the daemon
// calls it with its configuration's limit (admit's keepSignalFacts on intake.DaemonBooted), so a
// minute is read against the limit configured when it began and a changed limit reads no earlier
// minute again. A limit it records is a write admission reads (RecordsWritten).
//
// It then deletes every limit of the project older than the one in force at the first minute a
// read at at or later reaches (modelRequestsReach), which no such minute reads. Only a boot adds a
// limit, so a boot is when one can be left behind.
func (s *Postgres) KeepTokenLimit(ctx context.Context, tx pgx.Tx, project string, limit int64, at time.Time) error {
	var kept *int64
	if limit > 0 {
		kept = &limit
	}
	tag, err := tx.Exec(ctx, `insert into signal_token_limits (project, since, token_limit)
		select $1, $2, $3::bigint
		where (select token_limit from signal_token_limits where project = $1 and since <= $2 order by since desc limit 1) is distinct from $3::bigint
		on conflict (project, since) do update set token_limit = excluded.token_limit`, project, at, kept)
	if err != nil {
		return fmt.Errorf("record %s's token limit from %s: %w", project, at, err)
	}
	noteRowsWritten(ctx, tag)
	reach := modelRequestsReach(at)
	if _, err := tx.Exec(ctx, `delete from signal_token_limits where project = $1
		and since < (select max(since) from signal_token_limits where project = $1 and since <= $2)`, project, reach); err != nil {
		return fmt.Errorf("forget the token limits of %s before %s: %w", project, reach, err)
	}
	return nil
}

// ProviderMinutes is the provider's record of a project over a range of whole minutes, as
// Postgres.ProviderMinutes reads it: whether the daemon keeps the project's model requests at all,
// each minute every request of which is known to have been reported, by its start, and how many
// counted requests of each minute their host reported with no token count.
type ProviderMinutes struct {
	kept    bool
	minutes map[int64]signals.Minute
	untold  map[int64]int64
}

// Kept says whether the daemon began keeping the project's model requests (the provider's row of
// signal_inputs): where it did not, the record holds no minute.
func (m ProviderMinutes) Kept() bool { return m.kept }

// Read is the minute that starts at start and whether the record holds it, the reading a
// signals.Series is: a minute the record does not hold is not recorded, never zero. The record
// holds a minute every request of which was reported (Reported) with its token count (Untold).
func (m ProviderMinutes) Read(start time.Time) (signals.Minute, bool) {
	if !start.Equal(start.Truncate(time.Minute)) {
		return signals.Minute{}, false
	}
	minute, reported := m.minutes[start.Unix()]
	return minute, reported && m.untold[start.Unix()] == 0
}

// Reported says whether every request of the minute that starts at start is known to have been
// reported, whatever the token counts of those reports: the daemon kept the requests from before
// the minute began, and no turn of a host that reports none overlaps it.
func (m ProviderMinutes) Reported(start time.Time) bool {
	_, reported := m.minutes[start.Unix()]
	return reported && start.Equal(start.Truncate(time.Minute))
}

// Untold is how many requests of the minute that starts at start, of a model the project counts,
// their host reported with no token count (no uncachedInputTokens): the minute's tokens are not
// known, so the record does not hold it (Read).
func (m ProviderMinutes) Untold(start time.Time) int64 {
	if !start.Equal(start.Truncate(time.Minute)) {
		return 0
	}
	return m.untold[start.Unix()]
}

// ProviderMinutes is project's provider record of each whole UTC minute that begins in [from, to):
// its uncached input tokens of the models the project counts, and the token limit in force when
// it began (0 for none, under which its headroom is not recorded). Each request counts in the
// minute it ended in, as KeepModelRequests sums it into that minute's row (model_request_minutes),
// so the read is of at most one row a minute; to is a minute's start, as every reader passes it.
//
// A minute is held only where every request of it is known to have been reported (Reported): from
// the first whole minute at or after the moment the daemon began keeping the requests (the
// provider's row of signal_inputs, which one boot of this release writes), and where no turn of a
// claim whose host reports no requests overlaps it. Such a host is one that registered with a
// plugin contract below reporting, the first contract whose host reports its model requests
// (api.ModelRequestsContract): its turn's tokens reach no report, so each minute from its start to
// its end, or on while it runs, is absent, never zero. A minute is held only where its host told
// the tokens of each counted request of it, too (Untold): a request reported with no token count
// leaves its minute's tokens unknown, so that minute is absent, never the sum of its other
// requests. A held minute in which no request ran is zero.
func (s *Postgres) ProviderMinutes(ctx context.Context, tx pgx.Tx, project string, from, to time.Time, reporting int) (ProviderMinutes, error) {
	read := ProviderMinutes{minutes: map[int64]signals.Minute{}, untold: map[int64]int64{}}
	since, kept, _, err := s.SignalInputStatus(ctx, tx, project, signals.Provider)
	if err != nil || !kept {
		return read, err
	}
	read.kept = true
	first := from.UTC().Truncate(time.Minute)
	if first.Before(from) {
		first = first.Add(time.Minute)
	}
	if began := since.Truncate(time.Minute); began.Before(since) {
		since = began.Add(time.Minute)
	}
	if first.Before(since) {
		first = since
	}
	if !first.Before(to) {
		return read, nil
	}
	limits, err := s.tokenLimits(ctx, tx, project, first, to)
	if err != nil {
		return read, err
	}
	for minute := first; minute.Before(to); minute = minute.Add(time.Minute) {
		read.minutes[minute.Unix()] = signals.Minute{Limit: limits.at(minute)}
	}
	rows, err := tx.Query(ctx, `select extract(epoch from minute)::bigint, uncached, untold
		from model_request_minutes where project = $1 and minute >= $2 and minute < $3`, project, first, to)
	if err != nil {
		return read, fmt.Errorf("read the model requests of %s from %s to %s: %w", project, first, to, err)
	}
	var start, uncached, untold int64
	if _, err := pgx.ForEachRow(rows, []any{&start, &uncached, &untold}, func() error {
		minute := read.minutes[start]
		minute.Uncached = uncached
		read.minutes[start] = minute
		if untold > 0 {
			read.untold[start] = untold
		}
		return nil
	}); err != nil {
		return read, fmt.Errorf("read the model requests of %s from %s to %s: %w", project, first, to, err)
	}
	token, err := claim.ProjectToken(project)
	if err != nil {
		return read, err
	}
	rows, err = tx.Query(ctx, `select started_at, ended_at from claim_turns
		where project = $1 and plugin_contract < $2 and started_at < $4 and (ended_at is null or ended_at > $3)`, token, reporting, first, to)
	if err != nil {
		return read, fmt.Errorf("read the turns of %s's hosts that report no model requests: %w", project, err)
	}
	var started time.Time
	var ended *time.Time
	if _, err := pgx.ForEachRow(rows, []any{&started, &ended}, func() error {
		minute := started.UTC().Truncate(time.Minute)
		if minute.Before(first) {
			minute = first
		}
		for ; minute.Before(to) && (ended == nil || minute.Before(*ended)); minute = minute.Add(time.Minute) {
			delete(read.minutes, minute.Unix())
		}
		return nil
	}); err != nil {
		return read, fmt.Errorf("read the turns of %s's hosts that report no model requests: %w", project, err)
	}
	return read, nil
}

// tokenLimit is one row of signal_token_limits: the limit from since on, 0 for none.
type tokenLimit struct {
	since time.Time
	limit int64
}

// tokenLimits is each token limit of project in force at some minute of [first, to), oldest first.
type tokenLimits []tokenLimit

// at is the limit in force at the start of minute: the latest whose since is at or before it, 0
// where none is.
func (l tokenLimits) at(minute time.Time) int64 {
	var limit int64
	for _, row := range l {
		if row.since.After(minute) {
			break
		}
		limit = row.limit
	}
	return limit
}

// tokenLimits reads the limits of project in force at some minute of [first, to).
func (s *Postgres) tokenLimits(ctx context.Context, tx pgx.Tx, project string, first, to time.Time) (tokenLimits, error) {
	rows, err := tx.Query(ctx, `select since, coalesce(token_limit, 0) from signal_token_limits where project = $1 and since < $3
		and since >= coalesce((select max(since) from signal_token_limits where project = $1 and since <= $2), '-infinity')
		order by since`, project, first, to)
	if err != nil {
		return nil, fmt.Errorf("read the token limits of %s from %s to %s: %w", project, first, to, err)
	}
	limits, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (tokenLimit, error) {
		var limit tokenLimit
		err := row.Scan(&limit.since, &limit.limit)
		return limit, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the token limits of %s from %s to %s: %w", project, first, to, err)
	}
	return limits, nil
}
