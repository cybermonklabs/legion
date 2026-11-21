package store

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// word_questions.go stores INFRA-3231's R8 records: each text a person wrote that the records job
// (internal/dispatch/wordrecords) read, with the selection prompt's verdict once it has one, and
// each Define use. Migration 20261119050003 says what each column holds. A word question is a
// record whose verdict is one of the three about words (about_words): the person asked what a word
// means, said they could not understand the text, or complained about its words.

// The sources of a record.
const (
	// WordQuestionFromEvent is a text a person wrote, read from the event log.
	WordQuestionFromEvent = "event"
	// WordQuestionFromDefine is a person's Define use (RecordDefineUse).
	WordQuestionFromDefine = "define"
)

// The states of a record.
const (
	WordQuestionPending = "pending"
	WordQuestionDone    = "done"
	WordQuestionFailed  = "failed"
)

// WordRecordsEventTypes are the events that can hold a text a person wrote, the only ones the
// records job is handed a payload of (WordRecordsIntake).
var WordRecordsEventTypes = []string{
	"comment.created", "comment.answered", "message.created", "message.answered", "ask.opened",
	"ask.answered", "ask.resolved", "artifact.approved", "artifact.changes_requested",
}

// WordRecordsEvent is one event of the log as the records job reads it. Actor and Payload are the
// stored JSON, and Payload is nil for an event whose type is not one of WordRecordsEventTypes.
// Project is the project of the event's issue or document, "" for neither.
type WordRecordsEvent struct {
	ID        int64
	Type      string
	Actor     json.RawMessage
	Payload   json.RawMessage
	Project   string
	CreatedAt time.Time
}

// WordRecordsText is a text a person wrote, as the records job found it in one event: the key that
// makes it one record (a broadcast's copies share one), the person's login ("" where the event's
// credential names no person) and the kind of text.
type WordRecordsText struct {
	Key    string
	Person string
	Kind   string
}

// WordRecordsIntake reads one page of the event log into word_questions, in one transaction that
// holds word_records_cursor's row for update, so two processes never read one page twice. The page
// is at most page events after the cursor and at or below safe, the event log's safe horizon
// (events.Horizon): event ids are not commit order, so an event above it may still commit below
// an id already read. A cursor still null starts at the first event created at or after from,
// which is the backfill; an event created before from is never read. texts answers the person's
// texts among one event (none for most), and each is inserted as a pending record, once by its key.
// The cursor moves to the page's last event, or to safe when the page was not full, and full says
// whether more events may be waiting.
func (s *Store) WordRecordsIntake(ctx context.Context, from time.Time, safe int64, page int, texts func(WordRecordsEvent) []WordRecordsText) (full bool, err error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("records intake: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var cursor *int64
	if err := tx.QueryRow(ctx, `select event_id from word_records_cursor for update`).Scan(&cursor); err != nil {
		return false, fmt.Errorf("records intake: read the cursor: %w", err)
	}
	if cursor == nil {
		// The first event created at or after from, read up the primary key from the log's
		// start: the walk stops at the first such event, and finding none starts at safe.
		var first *int64
		if err := tx.QueryRow(ctx, `select min(id) from events where id <= $2 and created_at >= $1`, from, safe).Scan(&first); err != nil {
			return false, fmt.Errorf("records intake: find the first event at or after %s: %w", from.Format(time.RFC3339), err)
		}
		start := safe
		if first != nil {
			start = *first - 1
		}
		cursor = &start
	}
	rows, err := tx.Query(ctx, `
		select e.id, e.type, e.actor, case when e.type = any($4) then e.payload end, e.created_at,
			coalesce(e.project_key, i.project_key, ar.project_key, ai.project_key, '')
		from events e
		left join issues i on i.key = e.issue_key
		left join artifacts ar on ar.id = e.artifact_id
		left join issues ai on ai.key = ar.issue_key
		where e.id > $1 and e.id <= $2
		order by e.id limit $3`, *cursor, safe, page, WordRecordsEventTypes)
	if err != nil {
		return false, fmt.Errorf("records intake: read the event log: %w", err)
	}
	var read []WordRecordsEvent
	for rows.Next() {
		var event WordRecordsEvent
		if err := rows.Scan(&event.ID, &event.Type, &event.Actor, &event.Payload, &event.CreatedAt, &event.Project); err != nil {
			rows.Close()
			return false, fmt.Errorf("records intake: read the event log: %w", err)
		}
		read = append(read, event)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("records intake: read the event log: %w", err)
	}
	batch := &pgx.Batch{}
	for _, event := range read {
		if event.Payload == nil || event.CreatedAt.Before(from) {
			continue
		}
		for _, text := range texts(event) {
			batch.Queue(`
				insert into word_questions (source, text_key, event_id, person, project, written_at, day, kind)
				values ('event', $1, $2, nullif($3, ''), nullif($4, ''), $5, ($5 at time zone 'UTC')::date, $6)
				on conflict (text_key) do nothing`,
				text.Key, event.ID, text.Person, event.Project, event.CreatedAt, text.Kind)
		}
	}
	if batch.Len() > 0 {
		if err := tx.SendBatch(ctx, batch).Close(); err != nil {
			return false, fmt.Errorf("records intake: record the texts: %w", err)
		}
	}
	full = len(read) == page
	next := safe
	if full {
		next = read[len(read)-1].ID
	}
	// The cursor never moves back: a horizon read before another process moved it stands lower.
	if _, err := tx.Exec(ctx, `update word_records_cursor set event_id = greatest($1::bigint, coalesce(event_id, $1::bigint))`, max(next, *cursor)); err != nil {
		return false, fmt.Errorf("records intake: move the cursor: %w", err)
	}
	return full, tx.Commit(ctx)
}

// WordQuestion is one record the records job classifies.
type WordQuestion struct {
	ID        int64
	EventID   int64
	Person    string
	Kind      string
	WrittenAt time.Time
	Attempts  int
}

// DueWordQuestions answers at most limit records whose classification is due, pending or failed
// with their next attempt at or before now, the longest due first.
func (s *Store) DueWordQuestions(ctx context.Context, limit int) ([]WordQuestion, error) {
	rows, err := s.Pool.Query(ctx, `
		select id, event_id, coalesce(person, ''), coalesce(kind, ''), written_at, attempts
		from word_questions
		where state <> 'done' and next_attempt_at <= now()
		order by next_attempt_at, id limit $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("read the records due: %w", err)
	}
	defer rows.Close()
	var due []WordQuestion
	for rows.Next() {
		var q WordQuestion
		if err := rows.Scan(&q.ID, &q.EventID, &q.Person, &q.Kind, &q.WrittenAt, &q.Attempts); err != nil {
			return nil, fmt.Errorf("read the records due: %w", err)
		}
		due = append(due, q)
	}
	return due, rows.Err()
}

// WordVerdict is the selection prompt's verdict on one record: the verdict, the words the person
// asked about (none for a verdict not about words), the model the provider served, and the agent
// text the record answers, "" for none found.
type WordVerdict struct {
	Verdict string
	Words   []string
	Model   string
	Answers string
}

// RecordWordVerdict makes a record done with its verdict.
func (s *Store) RecordWordVerdict(ctx context.Context, id int64, verdict WordVerdict) error {
	words := verdict.Words
	if words == nil {
		words = []string{}
	}
	tag, err := s.Pool.Exec(ctx, `
		update word_questions
		set state = 'done', verdict = $2, words = $3, model = nullif($4, ''), answers = nullif($5, ''),
			attempts = attempts + 1, last_error = null
		where id = $1 and state <> 'done'`, id, verdict.Verdict, words, verdict.Model, verdict.Answers)
	if err != nil {
		return fmt.Errorf("record the verdict of record %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("record the verdict of record %d: no such record waits for one", id)
	}
	return nil
}

// RecordWordFailure keeps a record whose classification failed for another attempt after
// retryAfter, with the error and the agent text it answers, "" for none found.
func (s *Store) RecordWordFailure(ctx context.Context, id int64, answers, failure string, retryAfter time.Duration) error {
	if _, err := s.Pool.Exec(ctx, `
		update word_questions
		set state = 'failed', attempts = attempts + 1, last_error = $2, answers = nullif($3, ''),
			next_attempt_at = now() + make_interval(secs => $4::float8)
		where id = $1 and state <> 'done'`, id, failure, answers, retryAfter.Seconds()); err != nil {
		return fmt.Errorf("record the failed classification of record %d: %w", id, err)
	}
	return nil
}

// DefineUse is one Define use: the definition's id, which makes it one record, the person who
// asked, the project of the text, the words they asked about, the agent text they asked about
// ("ask:<id>", "version:<document id>:<number>"), the model that defined them, and when.
type DefineUse struct {
	DefinitionID string
	Person       string
	Project      string
	Words        []string
	Answers      string
	Model        string
	At           time.Time
}

// RecordDefineUse records a person's Define use as their question about those words: a done record
// with the verdict asks_word_meaning, so the reader's words and the counts read it beside the
// texts the records job classifies. A second call for one definition records nothing.
//
// The record belongs to the request that made the definition (POST /api/v1/definitions), so it
// commits in a transaction of the pool: under a keyed request's context the commit step marks the
// request's Idempotency-Key with it, as it marked the key with the definition, rather than
// committing a row no mark covers.
func (s *Store) RecordDefineUse(ctx context.Context, use DefineUse) error {
	if use.DefinitionID == "" || use.Person == "" {
		return errors.New("record a Define use: the definition and the person are required")
	}
	words := use.Words
	if words == nil {
		words = []string{}
	}
	at := cmp.Or(use.At, time.Now())
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("record a Define use: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		insert into word_questions (source, text_key, person, project, written_at, day, answers, verdict, words, model, state, attempts)
		values ('define', 'define:' || $1, $2, nullif($3, ''), $4, ($4 at time zone 'UTC')::date, nullif($5, ''),
			'asks_word_meaning', $6, nullif($7, ''), 'done', 1)
		on conflict (text_key) do nothing`,
		use.DefinitionID, use.Person, use.Project, at, use.Answers, words, use.Model); err != nil {
		return fmt.Errorf("record a Define use: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("record a Define use: %w", err)
	}
	return nil
}

// ReaderWord is one word a reader asked about: the word as they last wrote it, how many of their
// records name it, and when they last asked.
type ReaderWord struct {
	Word  string    `json:"word"`
	Times int       `json:"times"`
	Last  time.Time `json:"last"`
}

// readerWordsLimit is the most words ReaderWords answers, the ones asked about last.
const readerWordsLimit = 200

// readerWordsAsked is each word of login's ($1) word questions, Define uses included, written
// before $2 where it is not null, with when it was written.
const readerWordsAsked = `
	with asked as (
		select btrim(word) as word, q.written_at
		from word_questions q, unnest(q.words) as word
		where q.person = $1 and q.state = 'done' and q.about_words
			and ($2::timestamptz is null or q.written_at < $2) and btrim(word) <> ''
	)`

// ReaderWords answers the words the person login asked about in their word questions, Define uses
// included, written before before where it is not zero: each word once, however its case differs
// between records, the most recently asked first, readerWordsLimit of them at most.
func (s *Store) ReaderWords(ctx context.Context, login string, before time.Time) ([]ReaderWord, error) {
	words, _, err := s.ReaderWordsPage(ctx, login, before, readerWordsLimit, 0)
	return words, err
}

// ReaderWordsPage is one page of ReaderWords's list, limit words from offset, and total, how many
// words the list holds.
func (s *Store) ReaderWordsPage(ctx context.Context, login string, before time.Time, limit, offset int) (words []ReaderWord, total int, err error) {
	var until *time.Time
	if !before.IsZero() {
		until = &before
	}
	// The count over every word rides on each word of the page, so the page and its total are one
	// reading. A page past the last word holds no word to carry it, and is counted on its own.
	rows, err := s.Pool.Query(ctx, readerWordsAsked+`
		select (array_agg(word order by written_at desc))[1], count(*)::int, max(written_at), count(*) over ()::int
		from asked group by lower(word)
		order by max(written_at) desc, 1 limit $3 offset $4`, login, until, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("read the words a person asked about: %w", err)
	}
	defer rows.Close()
	words = []ReaderWord{}
	for rows.Next() {
		var word ReaderWord
		if err := rows.Scan(&word.Word, &word.Times, &word.Last, &total); err != nil {
			return nil, 0, fmt.Errorf("read the words a person asked about: %w", err)
		}
		words = append(words, word)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("read the words a person asked about: %w", err)
	}
	if len(words) == 0 && offset > 0 {
		if err := s.Pool.QueryRow(ctx, readerWordsAsked+`
			select count(distinct lower(word))::int from asked`, login, until).Scan(&total); err != nil {
			return nil, 0, fmt.Errorf("count the words a person asked about: %w", err)
		}
	}
	return words, total, nil
}
