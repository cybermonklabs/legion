package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// The retentions of writing_check_events, past which the daily sweep (SweepWritingCheckEvents)
// deletes a row: WritingCheckCountedRetention for a row R8's counts read (countedRow), the 366 days
// the counts route reads at most, so a range that ends today counts its rows whole, and
// WritingCheckRetention for every other row.
const (
	WritingCheckRetention        = 90 * 24 * time.Hour
	WritingCheckCountedRetention = 366 * 24 * time.Hour
)

// The outcomes of the rows R8's counts read (countWritingCheckRows), the one list the counts'
// query and the sweep's are both written from: rowOutcomes, counted one row each, rewrite_requested
// as RewriteUses and reviewerOutcomes in the reviewer's columns of WritingCheckRow, in their
// order; and the rows of a text posted over a finding, counted once a write as PostedOverFinding
// (overFindingRow): overFindingOutcomes, and reviewed_after where it names a rule, since a review
// after the post that found nothing posts over none.
const (
	outcomeRewriteRequested = "rewrite_requested"
	outcomeReviewedAfter    = "reviewed_after"
)

var (
	reviewerOutcomes    = []string{"held", "rewrite_posted", "passed", "posted_unreviewed"}
	rowOutcomes         = append([]string{outcomeRewriteRequested}, reviewerOutcomes...)
	overFindingOutcomes = []string{"findings", "posted_as_written"}
)

// sqlStrings is values as a comma list of SQL string literals.
func sqlStrings(values ...string) string {
	quoted := make([]string, len(values))
	for i, value := range values {
		quoted[i] = "'" + strings.ReplaceAll(value, "'", "''") + "'"
	}
	return strings.Join(quoted, ", ")
}

// countOf is the SQL count of a group's rows of each of outcomes, one column each.
func countOf(outcomes ...string) string {
	counts := make([]string, len(outcomes))
	for i, outcome := range outcomes {
		counts[i] = "count(*) filter (where outcome = " + sqlStrings(outcome) + ")"
	}
	return strings.Join(counts, ", ")
}

// overFindingRow is the SQL predicate of a row of a text posted over a finding, and countedRow
// that of a row R8's counts read: each outcome a literal, so the partial index of the rows they do
// not read (writing_check_events_uncounted_idx, migration 20261119050001, whose predicate is
// countedRow negated as written here) serves the sweep's delete of them on every plan.
var (
	overFindingRow = fmt.Sprintf(`(outcome in (%s) or (outcome = %s and cardinality(rules) > 0))`,
		sqlStrings(overFindingOutcomes...), sqlStrings(outcomeReviewedAfter))
	countedRow = fmt.Sprintf(`(outcome in (%s) or %s)`, sqlStrings(rowOutcomes...), overFindingRow)
)

// WritingCheckEvent is one row of writing_check_events: what the writing check, or the reviewer,
// decided for one write. Every string the write did not carry is "", stored as null.
type WritingCheckEvent struct {
	Route      string
	Method     string
	ActorKind  string
	ActorID    string
	ActSession string
	TokenID    string
	Outcome    string
	Rules      []string
	Fields     []string
	Detail     string
	// Reader is the login of the person who reads the write's text, as the server decided it from
	// the record; "" where no person reads it. A Rewrite request (rewrite_requested) names the
	// person who asked for it, who reads the text. The counts give each row to its reader.
	Reader string
	// Project is the project of the text's issue or document, "" where the writer did not record
	// it; the counts give each row to its project.
	Project string
	// WriteID is the id of the write the row decides, which each row of a write that more than
	// one row records names: a held text's id, else an id the server made for the write's request;
	// "" for a row that is its write's only one. The counts count each write's text once by it.
	WriteID string
}

// InsertWritingCheckEvent records one decision of the writing check. It runs in a transaction of
// its own, never inside the write's own transaction, so the caller logs a failure and lets the
// write go on.
//
// The row is the check's record of a write, and no part of the request's own write, so its
// transaction takes no commit step a keyed request's context carries (WithoutBeforeCommit): that
// step marks the request's Idempotency-Key committed, which only the request's own write does.
func (s *Store) InsertWritingCheckEvent(ctx context.Context, event WritingCheckEvent) error {
	rules, fields := event.Rules, event.Fields
	if rules == nil {
		rules = []string{}
	}
	if fields == nil {
		fields = []string{}
	}
	tx, err := s.Pool.Begin(WithoutBeforeCommit(ctx))
	if err != nil {
		return fmt.Errorf("record writing check event: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		insert into writing_check_events (
			route, method, actor_kind, actor_id, act_session, token_id, outcome,
			rules, fields, detail, reader, project, write_id
		) values ($1, $2, $3, $4, nullif($5, ''), nullif($6, ''), $7, $8, $9, nullif($10, ''), nullif($11, ''), nullif($12, ''), nullif($13, ''))
	`, event.Route, event.Method, event.ActorKind, event.ActorID, event.ActSession, event.TokenID, event.Outcome,
		rules, fields, event.Detail, event.Reader, event.Project, event.WriteID); err != nil {
		return fmt.Errorf("record writing check event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("record writing check event: %w", err)
	}
	return nil
}

// The sweep's two deletes (SweepWritingCheckEvents), each of which reads only the rows it
// deletes: every row past WritingCheckCountedRetention, on the created_at index, and every row the
// counts do not read (countedRow) past WritingCheckRetention, on the partial index of those rows,
// writing_check_events_uncounted_idx. $1 is the retention as an interval.
var (
	sweepPastCountedRetention = `delete from writing_check_events where created_at < now() - $1::interval`
	sweepUncountedRows        = `delete from writing_check_events where created_at < now() - $1::interval and not ` + countedRow
)

// SweepWritingCheckEvents deletes every writing_check_events row past its retention:
// WritingCheckCountedRetention for a row R8's counts read, WritingCheckRetention for the rest, and
// answers how many it deleted.
func (s *Store) SweepWritingCheckEvents(ctx context.Context) (int64, error) {
	var deleted int64
	for _, sweep := range []struct {
		sql       string
		retention time.Duration
	}{{sweepPastCountedRetention, WritingCheckCountedRetention}, {sweepUncountedRows, WritingCheckRetention}} {
		tag, err := s.Pool.Exec(ctx, sweep.sql, fmt.Sprintf("%d seconds", int64(sweep.retention/time.Second)))
		if err != nil {
			return deleted, fmt.Errorf("sweep writing check events: %w", err)
		}
		deleted += tag.RowsAffected()
	}
	return deleted, nil
}

// WritingCheckCounts is R8's counts of the UTC days First to Last, both YYYY-MM-DD: Days, each day
// of writing_check_events counted by outcome and by route, every day of the range listed; and one
// page of the counts of each day, project and person that has any: Items, Limit of them from
// Offset, and Total, how many there are.
type WritingCheckCounts struct {
	First  string            `json:"first"`
	Last   string            `json:"last"`
	Days   []WritingCheckDay `json:"days"`
	Items  []WritingCheckRow `json:"items"`
	Total  int               `json:"total"`
	Limit  int               `json:"limit"`
	Offset int               `json:"offset"`
}

// WritingCheckDay is one UTC day of writing_check_events, counted two ways: by outcome and by route
// ("<method> <pattern>").
type WritingCheckDay struct {
	Day      string         `json:"day"`
	Total    int            `json:"total"`
	Outcomes map[string]int `json:"outcomes"`
	Routes   map[string]int `json:"routes"`
}

// WritingCheckRow is R8's counts for one UTC day, one project ("" for a text on no project, or one
// whose writer recorded none) and one person ("" for a person the record does not name: a text a
// person typed in a session whose credential names nobody, or a reviewer's row with no reader).
// The texts are those people wrote, as the records job read them (word_questions): WordQuestions
// those whose verdict is about words, Texts every one, TextsUnread those still waiting for a
// verdict, and DefineUses the person's Define uses. AsksAnswered is the asks the person answered
// that day, and AsksRepliedBeforeAnswer those of them a person had commented on in the ask's thread
// before the answer, read from the event log. The rest are writing_check_events rows given to their
// reader: RewriteUses the Rewrites the person asked for (rewrite_requested), the reviewer's held,
// rewrote (rewrite_posted), passed and posted-unreviewed texts, and PostedOverFinding the texts that
// posted over a finding (findings, posted_as_written, and reviewed_after with findings).
type WritingCheckRow struct {
	Day                      string `json:"day"`
	Project                  string `json:"project"`
	Person                   string `json:"person"`
	WordQuestions            int    `json:"word_questions"`
	Texts                    int    `json:"texts"`
	TextsUnread              int    `json:"texts_unread"`
	AsksAnswered             int    `json:"asks_answered"`
	AsksRepliedBeforeAnswer  int    `json:"asks_replied_before_answer"`
	RewriteUses              int    `json:"rewrite_uses"`
	DefineUses               int    `json:"define_uses"`
	ReviewerHeld             int    `json:"reviewer_held"`
	ReviewerRewrote          int    `json:"reviewer_rewrote"`
	ReviewerPassed           int    `json:"reviewer_passed"`
	ReviewerPostedUnreviewed int    `json:"reviewer_posted_unreviewed"`
	PostedOverFinding        int    `json:"posted_over_finding"`
}

// countWritingCheckRows is WritingCheckRow's counts, each from the rows of its own source over the
// days $1 to $2 (dates) and the times $3 to $4 (their first midnight and the midnight after the
// last), summed by day, project and person. An ask answered and an ask commented on are a person's
// own act: an actor of kind user with no session acting for them, as a record's person is. A text
// posted over a finding is counted once however many rows record a finding of it - the writing
// check's findings and the reviewer's own, which name one write (WritingCheckEvent.WriteID) - on
// the day, project and person of the first of its rows that names a project. That row is picked
// among all the write's rows, inside the range or not, and the text is counted where the row falls
// in the range, so a text whose rows lie on either side of a midnight is counted on one day
// whichever range is asked: a row that names no write is its own text, and the rows of a write
// that has a row in the range are read by write_id (writing_check_events_write_id_idx). The
// outcomes it reads of writing_check_events are rowOutcomes and overFindingRow's. It orders
// nothing: CountWritingCheck pages it in day, project and person order.
var countWritingCheckRows = `
select day::text, project, person,
	sum(word_questions)::int, sum(texts)::int, sum(texts_unread)::int, sum(asks_answered)::int,
	sum(asks_replied)::int, sum(rewrite_uses)::int, sum(define_uses)::int, sum(held)::int,
	sum(rewrote)::int, sum(passed)::int, sum(unreviewed)::int, sum(over_finding)::int
from (
	select day, coalesce(project, '') as project, coalesce(person, '') as person,
		count(*) filter (where source = 'event' and about_words) as word_questions,
		count(*) filter (where source = 'event') as texts,
		count(*) filter (where source = 'event' and state <> 'done') as texts_unread,
		0 as asks_answered, 0 as asks_replied, 0 as rewrite_uses,
		count(*) filter (where source = 'define') as define_uses,
		0 as held, 0 as rewrote, 0 as passed, 0 as unreviewed, 0 as over_finding
	from word_questions
	where day >= $1::date and day <= $2::date
	group by 1, 2, 3
	union all
	select (e.created_at at time zone 'UTC')::date,
		coalesce(e.project_key, i.project_key, ar.project_key, ai.project_key, ''),
		lower(btrim(e.actor->>'id')),
		0, 0, 0,
		count(distinct e.payload->>'id'),
		count(distinct e.payload->>'id') filter (where exists (
			select 1 from comments c
			where c.ask_id = (e.payload->>'id')::uuid and c.created_at < e.created_at
				and c.author->>'kind' = 'user' and coalesce(c.author->'act'->>'session', '') = '')),
		0, 0, 0, 0, 0, 0, 0
	from events e
	left join issues i on i.key = e.issue_key
	left join artifacts ar on ar.id = e.artifact_id
	left join issues ai on ai.key = ar.issue_key
	where e.type = 'ask.answered' and e.actor->>'kind' = 'user'
		and coalesce(e.actor->'act'->>'session', '') = ''
		and e.created_at >= $3 and e.created_at < $4
	group by 1, 2, 3
	union all
	select (created_at at time zone 'UTC')::date, coalesce(project, ''), coalesce(reader, ''),
		0, 0, 0, 0, 0, ` + countOf(outcomeRewriteRequested) + `, 0, ` + countOf(reviewerOutcomes...) + `, 0
	from writing_check_events
	where created_at >= $3 and created_at < $4 and outcome in (` + sqlStrings(rowOutcomes...) + `)
	group by 1, 2, 3
	union all
	select (created_at at time zone 'UTC')::date, project, person, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, count(*)
	from (
		select created_at, coalesce(project, '') as project, coalesce(reader, '') as person
		from writing_check_events
		where created_at >= $3 and created_at < $4 and write_id is null and ` + overFindingRow + `
		union all
		(select distinct on (write_id) created_at, coalesce(project, ''), coalesce(reader, '')
		from writing_check_events
		where write_id is not null and ` + overFindingRow + ` and write_id in (
			select write_id from writing_check_events
			where created_at >= $3 and created_at < $4 and write_id is not null and ` + overFindingRow + `)
		order by write_id, project is null, created_at, id)
	) over_finding
	where created_at >= $3 and created_at < $4
	group by 1, 2, 3
) counted
group by 1, 2, 3`

// CountWritingCheck counts the UTC calendar days first to last, both included: from first's
// midnight to the midnight after last. Its Items are limit of the rows from offset, in day,
// project and person order.
func (s *Store) CountWritingCheck(ctx context.Context, first, last time.Time, limit, offset int) (WritingCheckCounts, error) {
	from := time.Date(first.Year(), first.Month(), first.Day(), 0, 0, 0, 0, time.UTC)
	through := time.Date(last.Year(), last.Month(), last.Day(), 0, 0, 0, 0, time.UTC)
	until := through.AddDate(0, 0, 1)
	counts := WritingCheckCounts{
		First: from.Format(time.DateOnly), Last: through.Format(time.DateOnly), Items: []WritingCheckRow{},
		Limit: limit, Offset: offset,
	}
	byDay := map[string]*WritingCheckDay{}
	for day := from; day.Before(until); day = day.AddDate(0, 0, 1) {
		counts.Days = append(counts.Days, WritingCheckDay{Day: day.Format(time.DateOnly), Outcomes: map[string]int{}, Routes: map[string]int{}})
	}
	for i := range counts.Days {
		byDay[counts.Days[i].Day] = &counts.Days[i]
	}
	rows, err := s.Pool.Query(ctx, `
		select (created_at at time zone 'UTC')::date::text, outcome, method || ' ' || route, count(*)::int
		from writing_check_events
		where created_at >= $1 and created_at < $2
		group by 1, 2, 3
	`, from, until)
	if err != nil {
		return WritingCheckCounts{}, fmt.Errorf("count writing check events: %w", err)
	}
	for rows.Next() {
		var day, outcome, route string
		var n int
		if err := rows.Scan(&day, &outcome, &route, &n); err != nil {
			rows.Close()
			return WritingCheckCounts{}, fmt.Errorf("count writing check events: %w", err)
		}
		if counted := byDay[day]; counted != nil {
			counted.Total += n
			counted.Outcomes[outcome] += n
			counted.Routes[route] += n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return WritingCheckCounts{}, fmt.Errorf("count writing check events: %w", err)
	}
	// The count over every row rides on each row of the page, so the page and its total are one
	// reading. A page past the last row holds no row to carry it, and is counted on its own.
	rows, err = s.Pool.Query(ctx, `select *, count(*) over ()::int from (`+countWritingCheckRows+`) listed
		order by 1, 2, 3 limit $5 offset $6`, from, through, from, until, limit, offset)
	if err != nil {
		return WritingCheckCounts{}, fmt.Errorf("count the records: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var row WritingCheckRow
		if err := rows.Scan(&row.Day, &row.Project, &row.Person, &row.WordQuestions, &row.Texts, &row.TextsUnread,
			&row.AsksAnswered, &row.AsksRepliedBeforeAnswer, &row.RewriteUses, &row.DefineUses, &row.ReviewerHeld,
			&row.ReviewerRewrote, &row.ReviewerPassed, &row.ReviewerPostedUnreviewed, &row.PostedOverFinding,
			&counts.Total); err != nil {
			return WritingCheckCounts{}, fmt.Errorf("count the records: %w", err)
		}
		counts.Items = append(counts.Items, row)
	}
	if err := rows.Err(); err != nil {
		return WritingCheckCounts{}, fmt.Errorf("count the records: %w", err)
	}
	if len(counts.Items) == 0 && offset > 0 {
		if err := s.Pool.QueryRow(ctx, `select count(*)::int from (`+countWritingCheckRows+`) listed`,
			from, through, from, until).Scan(&counts.Total); err != nil {
			return WritingCheckCounts{}, fmt.Errorf("count the records: %w", err)
		}
	}
	return counts, nil
}
