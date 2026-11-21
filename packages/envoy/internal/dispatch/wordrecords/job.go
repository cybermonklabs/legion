package wordrecords

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Lease is the records job's lease (duty.RunLeased): one process runs a pass at a time.
const Lease = "word-records"

// The job's pace, each a Config field's value where it is zero.
const (
	// DefaultInterval is the time between two passes' starts.
	DefaultInterval = 10 * time.Second
	// DefaultBackoff is the wait before a failed text is read again, doubled after each failure
	// up to DefaultMaxBackoff.
	DefaultBackoff    = 30 * time.Second
	DefaultMaxBackoff = time.Hour
	// defaultPage is the most events one transaction of the intake reads.
	defaultPage = 500
	// defaultBatch is the most texts one pass classifies, and defaultConcurrency how many of
	// those calls are in flight at once.
	defaultBatch       = 100
	defaultConcurrency = 4
)

// Config configures the records job.
type Config struct {
	Store *store.Store
	// Horizon is the server's one events.Horizon, the safe horizon the intake reads up to.
	Horizon    *events.Horizon
	Classifier Classifier
	// From is the time the first pass reads the event log from (DISPATCH_WORD_RECORDS_FROM): the
	// first event created at or after it, which is the backfill. No text written before it is
	// read.
	From time.Time
	// Interval, Backoff and MaxBackoff are the job's pace (DefaultInterval, DefaultBackoff,
	// DefaultMaxBackoff); a test runs it faster.
	Interval   time.Duration
	Backoff    time.Duration
	MaxBackoff time.Duration
	// Page, Batch and Concurrency bound one pass's work (defaultPage, defaultBatch,
	// defaultConcurrency).
	Page        int
	Batch       int
	Concurrency int
}

// Job is the records job of R8. Each pass reads the event log after the job's cursor
// (word_records_cursor) up to the safe horizon, records each text a person wrote as a pending
// word_questions row, and then classifies the rows that are due with the selection prompt. It reads
// only what has committed, after the write's own transaction, so no write waits on it or on the
// model; a text whose classification fails is kept, failed, and read again after a backoff until
// a verdict comes.
type Job struct {
	cfg    Config
	reader reader
}

// New builds the job cfg describes.
func New(cfg Config) *Job {
	cfg.Interval = cmp.Or(cfg.Interval, DefaultInterval)
	cfg.Backoff = cmp.Or(cfg.Backoff, DefaultBackoff)
	cfg.MaxBackoff = cmp.Or(cfg.MaxBackoff, DefaultMaxBackoff)
	cfg.Page = cmp.Or(cfg.Page, defaultPage)
	cfg.Batch = cmp.Or(cfg.Batch, defaultBatch)
	cfg.Concurrency = cmp.Or(cfg.Concurrency, defaultConcurrency)
	return &Job{cfg: cfg, reader: reader{pool: cfg.Store.Pool}}
}

// Run runs a pass every Interval under the lease Lease, the first at start, until ctx ends: one
// process serves each period however many run (duty.RunLeased). A pass that fails is logged, and
// the next one starts again from the cursor and the rows still due.
func (j *Job) Run(ctx context.Context, leases duty.Leases) {
	duty.RunLeased(ctx, leases, Lease, j.cfg.Interval, duty.TickAtStart, func(ctx context.Context) {
		if err := j.Pass(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("dispatch: a pass of the records job failed; the next pass goes on from where it stopped", "error", err)
		}
	})
}

// Pass is one pass: the intake of every event up to the safe horizon, page by page, each page in a
// transaction of its own (store.WordRecordsIntake), then the classification of up to Batch texts
// that are due.
//
// A safe horizon of 0 is one that has proven nothing yet (events.Horizon): a process's first pass
// takes one with Await instead, as the suggestion outcome sweep does, rather than read nothing.
func (j *Job) Pass(ctx context.Context) error {
	safe, err := j.cfg.Horizon.Safe(ctx)
	if err == nil && safe == 0 {
		safe, err = j.cfg.Horizon.Await(ctx)
	}
	if err != nil {
		return fmt.Errorf("read the event log's safe horizon: %w", err)
	}
	for {
		full, err := j.cfg.Store.WordRecordsIntake(ctx, j.cfg.From, safe, j.cfg.Page, PersonTexts)
		if err != nil {
			return err
		}
		if !full {
			break
		}
	}
	due, err := j.cfg.Store.DueWordQuestions(ctx, j.cfg.Batch)
	if err != nil {
		return err
	}
	calls := make(chan struct{}, j.cfg.Concurrency)
	var wg sync.WaitGroup
	for _, q := range due {
		select {
		case calls <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-calls }()
			j.classify(ctx, q)
		}()
	}
	wg.Wait()
	return nil
}

// classify gives one text its verdict and records it, or records the failure with the time of the
// next attempt. A call a shutdown cut short records nothing: the text stays due.
func (j *Job) classify(ctx context.Context, q store.WordQuestion) {
	input, answers, err := j.reader.input(ctx, q)
	if err == nil {
		var verdict Verdict
		if verdict, err = j.cfg.Classifier.Classify(ctx, input); err == nil {
			if err := j.cfg.Store.RecordWordVerdict(ctx, q.ID, store.WordVerdict{
				Verdict: verdict.Verdict, Words: verdict.Words, Model: verdict.Model, Answers: answers,
			}); err != nil && ctx.Err() == nil {
				slog.Warn("dispatch: the records job could not record a verdict; the text is read again", "record", q.ID, "error", err)
			}
			return
		}
	}
	if ctx.Err() != nil {
		return
	}
	retry := j.backoff(q.Attempts + 1)
	slog.Warn("dispatch: the records job could not classify a text; it reads it again later",
		"record", q.ID, "attempt", q.Attempts+1, "retry_in", retry, "error", err)
	if err := j.cfg.Store.RecordWordFailure(ctx, q.ID, answers, err.Error(), retry); err != nil && ctx.Err() == nil {
		slog.Warn("dispatch: the records job could not record a failed classification", "record", q.ID, "error", err)
	}
}

// backoff is the wait before attempt (counted from 1 for the first failure) is read again:
// Backoff, doubled after each failure up to MaxBackoff.
func (j *Job) backoff(attempt int) time.Duration {
	wait := j.cfg.Backoff
	for range min(attempt-1, 32) {
		if wait >= j.cfg.MaxBackoff {
			break
		}
		wait *= 2
	}
	return min(wait, j.cfg.MaxBackoff)
}
