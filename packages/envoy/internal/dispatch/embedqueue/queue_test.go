package embedqueue

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/embed/embedtest"
	"github.com/sjawhar/envoy/internal/dispatch/retry"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/pglisten"
	"github.com/sjawhar/envoy/internal/testwait"
)

// embedqueueTestParallelism is how many of this package's tests run at once when the command line
// names no -parallel. Each parallel test has a database of its own (storetest.Open's clone) and
// waits on Postgres far more than on a core, at about 4 connections a test; go test's default of
// GOMAXPROCS is four on CI's runner but hundreds on a large machine, where every test's database
// at once would open more connections than the test server allows. On 4 CPUs the package took
// 67.6 and 78.1 s at 4 and 69.8 to 71.4 s at 8 (build-cluster runs at one head, one node), so 4.
//
// go test parses its flags inside m.Run, so a -parallel on the command line still wins.
const embedqueueTestParallelism = 4

// TestMain sets the package's default -parallel and removes the databases the tests' pools are
// cloned on (storetest.Main).
func TestMain(m *testing.M) {
	if err := flag.Set("test.parallel", strconv.Itoa(embedqueueTestParallelism)); err != nil {
		panic("set this package's default -parallel: " + err.Error())
	}
	os.Exit(storetest.Main(m))
}

// bedrockShaped gives a test embedder the Identity and Limits embed.Embedder asks for: Bedrock's
// own, so the batch size, byte budget and token ceiling the tests below count against are
// Bedrock's. Every fake in this file embeds it.
type bedrockShaped struct{}

func (bedrockShaped) Identity() string     { return embed.BedrockModel }
func (bedrockShaped) Limits() embed.Limits { return new(embed.BedrockClient).Limits() }

// fakeEmbedder is a controllable stand-in for Cohere: Embed answers vectors or err, and records
// every call so a test can assert how many batches ran.
type fakeEmbedder struct {
	bedrockShaped
	vector func(text string) []float32
	err    error
	calls  [][]string
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string, _ embed.InputType) ([][]float32, error) {
	f.calls = append(f.calls, append([]string(nil), texts...))
	if f.err != nil {
		return nil, f.err
	}
	vectors := make([][]float32, len(texts))
	for i, text := range texts {
		if f.vector != nil {
			vectors[i] = f.vector(text)
		} else {
			vectors[i] = oneVector()
		}
	}
	return vectors, nil
}

func oneVector() []float32 {
	v := make([]float32, embed.Dimension)
	for i := range v {
		v[i] = 1
	}
	return v
}

// fakeVectors returns n healthy, identical vectors - the success path every fake embedder in
// this file shares once it has decided a call succeeds.
func fakeVectors(n int) [][]float32 {
	vectors := make([][]float32, n)
	for i := range vectors {
		vectors[i] = oneVector()
	}
	return vectors
}

// throttleThenSucceedEmbedder throttles its first throttleFor calls, then delegates to inner -
// standing in for a provider that recovers partway through an unattended run.
type throttleThenSucceedEmbedder struct {
	bedrockShaped
	inner       embed.Embedder
	throttleFor int
	calls       int
}

func (e *throttleThenSucceedEmbedder) Embed(ctx context.Context, texts []string, inputType embed.InputType) ([][]float32, error) {
	e.calls++
	if e.calls <= e.throttleFor {
		return nil, embedtest.FakeThrottleError{Code: "ThrottlingException"}
	}
	return e.inner.Embed(ctx, texts, inputType)
}

// seedIssue inserts an issue directly (bypassing the API, which this package does not import),
// keyed exactly as given - e.g. seedIssue(t, db, "EMBQ", "EMBQ-1", "A title") - with its project
// created on first use. number is the next free one for projectKey, not a hardcoded 1, so a
// test that seeds several issues under one project (the bisection tests' shared batches) does
// not collide on issues_project_key_number_key.
func seedIssue(t *testing.T, database *store.Store, projectKey, issueKey, title string) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ($1, 'Test', 'alice') on conflict (key) do nothing
	`, projectKey); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ($1, $2, coalesce((select max(number) + 1 from issues where project_key = $2), 1), $3, '{"kind":"user","id":"alice"}', 'A')
	`, issueKey, projectKey, title); err != nil {
		t.Fatalf("seed issue %s: %v", issueKey, err)
	}
}

// contentHashOf reads a row's real content_hash: claimRenewal.renew matches on it (Rev's own
// should: a renewal must never touch a row a concurrent write already changed), so a test
// constructing a pendingRow by hand for renewal, rather than through scanPending, needs the real
// value - the zero-value empty string never matches what embeddings_enqueue's trigger wrote.
func contentHashOf(t *testing.T, database *store.Store, kind, id string) string {
	t.Helper()
	var hash string
	if err := database.Pool.QueryRow(context.Background(),
		`select content_hash from embeddings where kind = $1 and id = $2`, kind, id,
	).Scan(&hash); err != nil {
		t.Fatalf("read content_hash for %s %s: %v", kind, id, err)
	}
	return hash
}

func pendingCount(t *testing.T, database *store.Store) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from embeddings where embedded_hash is distinct from content_hash
	`).Scan(&count); err != nil {
		t.Fatalf("count pending embeddings: %v", err)
	}
	return count
}

func forceEligible(t *testing.T, database *store.Store, kind, id string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		update embeddings set next_attempt_at = now() - interval '1 second' where kind = $1 and id = $2
	`, kind, id); err != nil {
		t.Fatalf("force %s/%s eligible: %v", kind, id, err)
	}
}

// databaseNow is the database's clock_timestamp(): the clock the queue's own stamps (a claim's
// next_attempt_at, pending_since, embedded_at) are read on.
func databaseNow(t *testing.T, database *store.Store) time.Time {
	t.Helper()
	var now time.Time
	if err := database.Pool.QueryRow(context.Background(), `select clock_timestamp()`).Scan(&now); err != nil {
		t.Fatalf("read the database's clock: %v", err)
	}
	return now
}

// newQueueClock is a fake for the queue's clock (Deps.Clock) that stands at the database's clock
// now until the test moves it, so a moment the queue stamps from it (retryRows' next_attempt_at)
// reads beside the database's own.
func newQueueClock(t *testing.T, database *store.Store) *clocktest.Fake {
	t.Helper()
	return clocktest.New(databaseNow(t, database))
}

// stagedAhead is how far past the database's clock a row of this package stages a moment the
// product judges against that clock: a row's backoff, or another process's claim on a row. It is
// staged state, not a wait: longer than any `go test -timeout` CI gives this package, so what the
// row staged reads as not yet passed however long the run takes, where a backoff's second or
// claimWindow's two minutes could pass on a loaded machine.
const stagedAhead = 24 * time.Hour

// TestProcessBatchEmbedsAPendingRowTheWriteTimeTriggerEnqueued proves the end-to-end path a real
// write exercises: inserting an issue (0073_issues_embeddings_trigger.up.sql's trigger) leaves a
// pending embeddings row with no vector, and ProcessBatch fills it in from the Embedder, without
// any application code calling embeddings_enqueue directly.
func TestProcessBatchEmbedsAPendingRowTheWriteTimeTriggerEnqueued(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	seedIssue(t, database, "EMBQ", "EMBQ-1", "A title to embed")
	if got := pendingCount(t, database); got != 1 {
		t.Fatalf("pending after insert = %d, want 1 (the trigger's own enqueue)", got)
	}

	embedder := &fakeEmbedder{}
	succeeded, failed, blocked, throttled, err := ProcessBatch(context.Background(), Deps{Store: database, Embedder: embedder})
	if err != nil || blocked || throttled {
		t.Fatalf("ProcessBatch = %d, %d, %v, %v, %v", succeeded, failed, blocked, throttled, err)
	}
	if succeeded != 1 || failed != 0 {
		t.Fatalf("ProcessBatch succeeded, failed = %d, %d, want 1, 0", succeeded, failed)
	}
	if len(embedder.calls) != 1 || embedder.calls[0][0] != "A title to embed" {
		t.Fatalf("embedder calls = %v, want one call embedding the issue's title", embedder.calls)
	}
	if got := pendingCount(t, database); got != 0 {
		t.Errorf("pending after ProcessBatch = %d, want 0", got)
	}
	var model string
	var embeddedAt *time.Time
	if err := database.Pool.QueryRow(context.Background(), `
		select model, embedded_at from embeddings where kind = 'issue' and id = 'EMBQ-1'
	`).Scan(&model, &embeddedAt); err != nil {
		t.Fatalf("read embedded row: %v", err)
	}
	if model != embedder.Identity() || embeddedAt == nil {
		t.Errorf("model=%q embeddedAt=%v, want model=%q (the embedder's Identity) and a timestamp", model, embeddedAt, embedder.Identity())
	}
}

// TestProcessBatchRetriesWithBackoffOnEmbedFailure is the durable-retry-queue acceptance
// criterion: a write never fails because the embedder did, and a failed attempt leaves the row
// pending, retried later rather than lost.
//
// The backoff is counted from the queue's clock (Deps.Clock, retryRows), a fake that stands
// stagedAhead past the database's clock: the row's next_attempt_at is then exactly retry.Delay(1)
// past the fake's moment, and still ahead of the database's now() when the database reads it and
// when the second batch runs, however slow the row is, where a second's backoff on the wall clock
// could pass before either.
func TestProcessBatchRetriesWithBackoffOnEmbedFailure(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	seedIssue(t, database, "RETR", "RETR-1", "A title")
	embedder := &fakeEmbedder{err: errors.New("bedrock: simulated outage")}
	fake := clocktest.New(databaseNow(t, database).Add(stagedAhead))
	deps := Deps{Store: database, Embedder: embedder, Clock: fake}
	succeeded, failed, blocked, throttled, err := ProcessBatch(context.Background(), deps)
	if err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}
	if succeeded != 0 || failed != 1 || blocked || throttled {
		t.Fatalf("ProcessBatch succeeded, failed, blocked, throttled = %d, %d, %v, %v, want 0, 1, false, false", succeeded, failed, blocked, throttled)
	}
	if got := pendingCount(t, database); got != 1 {
		t.Fatalf("pending after a failed attempt = %d, want 1 (still pending, not lost)", got)
	}
	var attempts int
	var nextAttemptAt time.Time
	var backedOff bool
	// next_attempt_at is judged on the database's clock, so the database is what judges it.
	if err := database.Pool.QueryRow(context.Background(), `
		select attempt_count, next_attempt_at, next_attempt_at > now() from embeddings where kind = 'issue' and id = 'RETR-1'
	`).Scan(&attempts, &nextAttemptAt, &backedOff); err != nil {
		t.Fatalf("read retry state: %v", err)
	}
	if attempts != 1 {
		t.Errorf("attempt_count = %d, want 1", attempts)
	}
	if want := fake.Now().Add(retry.Delay(1)); !nextAttemptAt.Equal(want) {
		t.Errorf("next_attempt_at = %v, want %v: retry.Delay(1) (%v) past the queue's clock as the batch read it", nextAttemptAt, want, retry.Delay(1))
	}
	if !backedOff {
		t.Errorf("next_attempt_at = %v is not past the database's now(), want it backed off into the future", nextAttemptAt)
	}
	// Immediately re-processing finds nothing: the row is backed off, not eligible yet.
	succeeded, failed, _, _, err = ProcessBatch(context.Background(), deps)
	if err != nil || succeeded != 0 || failed != 0 {
		t.Fatalf("ProcessBatch immediately after backoff = %d, %d, %v, want 0, 0, nil", succeeded, failed, err)
	}
}

// TestProcessBatchNeverDeadLettersAThrottledBatchHoweverManyTimesItRecurs is the round-3
// blocking finding: a whole-batch throttle from the model service is infrastructure, never
// evidence about a row's content, so it must never count toward the dead-letter threshold - a
// sustained throttle episode outlasting retry.DeadLetterAttempts cycles must still be retrying
// afterward, not permanently abandoned.
func TestProcessBatchNeverDeadLettersAThrottledBatchHoweverManyTimesItRecurs(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	seedIssue(t, database, "THRO", "THRO-1", "A title")
	embedder := &fakeEmbedder{err: embedtest.FakeThrottleError{Code: "ThrottlingException"}}
	deps := Deps{Store: database, Embedder: embedder}
	ctx := context.Background()

	for i := range retry.DeadLetterAttempts + 5 {
		forceEligible(t, database, "issue", "THRO-1")
		succeeded, failed, blocked, throttled, err := ProcessBatch(ctx, deps)
		if err != nil || blocked {
			t.Fatalf("ProcessBatch attempt %d = %d, %d, %v, %v", i, succeeded, failed, blocked, err)
		}
		if !throttled {
			t.Fatalf("ProcessBatch attempt %d: throttled = false, want true", i)
		}
	}

	var dead bool
	var attempts int
	if err := database.Pool.QueryRow(ctx, `select dead, attempt_count from embeddings where kind = 'issue' and id = 'THRO-1'`).Scan(&dead, &attempts); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if dead {
		t.Errorf("dead = true after %d throttled attempts (more than retry.DeadLetterAttempts=%d), want false: throttling is never permanent", attempts, retry.DeadLetterAttempts)
	}

	// The model service recovers: the very next attempt, once eligible, succeeds normally.
	embedder.err = nil
	forceEligible(t, database, "issue", "THRO-1")
	succeeded, failed, blocked, throttled, err := ProcessBatch(ctx, deps)
	if err != nil || blocked || throttled || succeeded != 1 || failed != 0 {
		t.Fatalf("ProcessBatch after recovery = %d, %d, %v, %v, %v, want 1, 0, false, false, nil", succeeded, failed, blocked, throttled, err)
	}
}

// TestProcessBatchRefusesToMarkStaleContentEmbedded guards commitEmbeddings' content_hash
// guard: if the row's text changes between the scan and the commit (another write re-enqueued
// it), the stale embedding must not be recorded as current.
func TestProcessBatchRefusesToMarkStaleContentEmbedded(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	seedIssue(t, database, "STAL", "STAL-1", "Original title")
	ctx := context.Background()
	deps := Deps{Store: database, Embedder: &fakeEmbedder{}}
	rows, settled, err := scanPending(ctx, deps)
	if err != nil || len(rows) != 1 || settled != 0 {
		t.Fatalf("scanPending = %v, %d settled, %v, want one row", rows, settled, err)
	}
	stale := rows[0]
	// A concurrent write changes the title (and so the trigger re-enqueues with a new hash)
	// between the scan above and the commit below.
	if _, err := database.Pool.Exec(ctx, `update issues set title = $1 where key = 'STAL-1'`, "Changed title"); err != nil {
		t.Fatalf("update title: %v", err)
	}
	succeeded, permanent, err := commitEmbeddings(ctx, deps, []pendingRow{stale}, [][]float32{oneVector()})
	if err != nil {
		t.Fatalf("commitEmbeddings: %v", err)
	}
	if succeeded != 0 || len(permanent) != 0 {
		t.Fatalf("commitEmbeddings = %d succeeded, %d permanent, want 0, 0 (silently skipped, not retried)", succeeded, len(permanent))
	}
	if got := pendingCount(t, database); got != 1 {
		t.Errorf("pending after a stale commit = %d, want 1 (the new text is still unembedded)", got)
	}
	var snapshot string
	if err := database.Pool.QueryRow(ctx, `select text_snapshot from embeddings where kind = 'issue' and id = 'STAL-1'`).Scan(&snapshot); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if snapshot != "Changed title" {
		t.Errorf("text_snapshot = %q, want the newer title", snapshot)
	}
}

// TestRetryRowsMarksARowDeadAfterRepeatedPermanentFailuresAndStopsRetrying is the dead-letter
// acceptance criterion for the one failure class that does dead-letter: past
// retry.DeadLetterAttempts *permanent* failures, a row stops consuming poller cycles rather than
// retrying with a five-minute ceiling forever, records the identity it died under, and is still
// visible and still recoverable (a text change clears dead and dead_model).
func TestRetryRowsMarksARowDeadAfterRepeatedPermanentFailuresAndStopsRetrying(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	seedIssue(t, database, "DEAD", "DEAD-1", "A title")
	ctx := context.Background()
	embedder := &fakeEmbedder{}
	deps := Deps{Store: database, Embedder: embedder}
	row := pendingRow{kind: "issue", id: "DEAD-1"}
	if err := database.Pool.QueryRow(ctx, `select content_hash from embeddings where kind = 'issue' and id = 'DEAD-1'`).Scan(&row.contentHash); err != nil {
		t.Fatalf("read content_hash: %v", err)
	}
	for row.attempts, row.confirmedFailures = 0, 0; row.confirmedFailures < retry.DeadLetterAttempts; row.attempts, row.confirmedFailures = row.attempts+1, row.confirmedFailures+1 {
		retryRows(ctx, deps, []pendingRow{row}, true)
	}

	var dead bool
	var attempts int
	var deadModel *string
	if err := database.Pool.QueryRow(ctx, `select dead, attempt_count, dead_model from embeddings where kind = 'issue' and id = 'DEAD-1'`).Scan(&dead, &attempts, &deadModel); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if !dead {
		t.Fatalf("dead = false after %d permanent attempts (retry.DeadLetterAttempts=%d), want true", attempts, retry.DeadLetterAttempts)
	}
	if deadModel == nil || *deadModel != embedder.Identity() {
		t.Errorf("dead_model = %v, want the embedder's Identity %q", deadModel, embedder.Identity())
	}

	rows, _, err := scanPending(ctx, deps)
	if err != nil {
		t.Fatalf("scanPending: %v", err)
	}
	for _, r := range rows {
		if r.kind == "issue" && r.id == "DEAD-1" {
			t.Error("scanPending returned a dead row; dead rows must not be retried automatically")
		}
	}

	// Recoverable: a fresh write (a title change) clears dead and re-admits the row.
	if _, err := database.Pool.Exec(ctx, `update issues set title = $1 where key = 'DEAD-1'`, "Recovered title"); err != nil {
		t.Fatalf("update title: %v", err)
	}
	if err := database.Pool.QueryRow(ctx, `select dead, dead_model from embeddings where kind = 'issue' and id = 'DEAD-1'`).Scan(&dead, &deadModel); err != nil {
		t.Fatalf("read dead after recovery write: %v", err)
	}
	if dead || deadModel != nil {
		t.Errorf("dead = %t, dead_model = %v after the row's own text changed; embeddings_enqueue should have cleared both", dead, deadModel)
	}
}

// poisonTextEmbedder fails non-throttled (ValidationException, the shape a content a model
// refuses outright comes back as) whenever poison is among the texts in a call, and succeeds
// otherwise - whether or not poison is present in that call's batch, siblings that do not carry
// it embed cleanly, exactly as a real "this one row's content is invalid" failure would behave
// isolated from the rest.
type poisonTextEmbedder struct {
	bedrockShaped
	poison string
	calls  [][]string
}

func (p *poisonTextEmbedder) Embed(_ context.Context, texts []string, _ embed.InputType) ([][]float32, error) {
	p.calls = append(p.calls, append([]string(nil), texts...))
	for _, text := range texts {
		if text == p.poison {
			return nil, errors.New("ValidationException: the model refuses this input")
		}
	}
	return fakeVectors(len(texts)), nil
}

// TestProcessBatchBisectsANonThrottledFailureToIsolateTheOffendingRow proves
// a non-throttled whole-batch failure must not retry every row in the batch
// forever just because one row's content is what the API actually refuses. Three issues share one
// batch; one's title is content the embedder refuses outright (non-throttled); bisection must
// isolate that one row, embed the other two normally in the same ProcessBatch call, and leave only
// the offending row's attempt_count incremented (not yet dead - that still takes
// retry.DeadLetterAttempts worth of calls, exactly as any permanent failure does).
func TestProcessBatchBisectsANonThrottledFailureToIsolateTheOffendingRow(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	seedIssue(t, database, "BSCT", "BSCT-1", "A good title one")
	seedIssue(t, database, "BSCT", "BSCT-2", "The poisoned title")
	seedIssue(t, database, "BSCT", "BSCT-3", "A good title two")
	embedder := &poisonTextEmbedder{poison: "The poisoned title"}
	deps := Deps{Store: database, Embedder: embedder}

	succeeded, failed, blocked, throttled, err := ProcessBatch(context.Background(), deps)
	if err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}
	if blocked {
		t.Error("blocked = true, want false")
	}
	if throttled {
		t.Error("throttled = true, want false - a ValidationException is not a throttle")
	}
	if succeeded != 2 {
		t.Errorf("succeeded = %d, want 2 (the two good rows, embedded despite sharing a batch with the poisoned one)", succeeded)
	}
	if failed != 1 {
		t.Errorf("failed = %d, want 1 (only the poisoned row)", failed)
	}

	for _, id := range []string{"BSCT-1", "BSCT-3"} {
		var embeddedHash, contentHash string
		if err := database.Pool.QueryRow(context.Background(),
			`select coalesce(embedded_hash, ''), content_hash from embeddings where kind = 'issue' and id = $1`, id,
		).Scan(&embeddedHash, &contentHash); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if embeddedHash != contentHash {
			t.Errorf("%s: embedded_hash = %q, content_hash = %q, want them equal - it should have embedded despite its sibling's poison", id, embeddedHash, contentHash)
		}
	}

	var attempts int
	var dead bool
	if err := database.Pool.QueryRow(context.Background(),
		`select attempt_count, dead from embeddings where kind = 'issue' and id = 'BSCT-2'`,
	).Scan(&attempts, &dead); err != nil {
		t.Fatalf("read BSCT-2: %v", err)
	}
	if attempts != 1 {
		t.Errorf("BSCT-2 attempt_count = %d, want 1 after a single ProcessBatch call", attempts)
	}
	if dead {
		t.Error("BSCT-2 dead = true after only 1 attempt, want false - dead-lettering still takes retry.DeadLetterAttempts")
	}

	// The isolation itself: more than one call was made (bisection split the batch), and at
	// least one later call embedded the poisoned row alone - proof bisection actually narrowed
	// down to it rather than only ever retrying the whole batch together. Splitting an odd batch
	// (here 1 row versus 2) can still try the poisoned row alongside one sibling once more before
	// that pair is itself split to size 1 - every recursive halving does that at whichever level
	// still holds more than one row - so the achievable invariant is eventual isolation, not zero
	// intermediate bundling.
	if len(embedder.calls) < 2 {
		t.Fatalf("embedder was called %d time(s), want more than 1 - bisection should have split the batch", len(embedder.calls))
	}
	isolated := false
	for _, call := range embedder.calls[1:] {
		if len(call) == 1 && call[0] == embedder.poison {
			isolated = true
		}
	}
	if !isolated {
		t.Errorf("no call after the first embedded the poisoned row alone - bisection never isolated it down to size 1: %v", embedder.calls)
	}
}

// TestProcessBatchBisectionAbortsOnAThrottleMidway proves a throttle encountered partway through
// bisection is never treated as evidence against any row: every row in the throttled subtree is
// reported for an ordinary retry (never permanent), and ProcessBatch's own throttled return value
// is true, so the caller still backs off its batch cadence exactly as an outright-throttled whole
// batch would.
func TestProcessBatchBisectionAbortsOnAThrottleMidway(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	seedIssue(t, database, "BSCA", "BSCA-1", "A good title one")
	seedIssue(t, database, "BSCA", "BSCA-2", "The poisoned title")
	poison := &poisonTextEmbedder{poison: "The poisoned title"}
	embedder := &throttleThenSucceedEmbedder{inner: poison, throttleFor: 1}
	deps := Deps{Store: database, Embedder: embedder}

	succeeded, failed, _, throttled, err := ProcessBatch(context.Background(), deps)
	if err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}
	if !throttled {
		t.Error("throttled = false, want true - the top-level call was throttled")
	}
	if succeeded != 0 || failed != 2 {
		t.Errorf("succeeded, failed = %d, %d, want 0, 2 - a top-level throttle retries the whole batch, never bisects", succeeded, failed)
	}
	var dead bool
	if err := database.Pool.QueryRow(context.Background(),
		`select dead from embeddings where kind = 'issue' and id = 'BSCA-2'`,
	).Scan(&dead); err != nil {
		t.Fatalf("read BSCA-2: %v", err)
	}
	if dead {
		t.Error("BSCA-2 dead = true after a throttled batch, want false - a throttle is never evidence about any row")
	}
}

// TestProcessBatchNeverDeadLettersAUniformNonThrottledFailureHoweverManyTimesItRecurs proves an
// outage embed.IsThrottled does not
// recognize (expired credentials, a retired model id, an uncoded 5xx) fails every row the same
// way, non-throttled, all the way down to size 1 - and nothing in the whole batch ever embeds,
// including every confirmSoloFailure canary probe, so none of it is ever confirmed as any row's
// own content. Driven for more cycles than retry.DeadLetterAttempts, after which nothing is dead.
func TestProcessBatchNeverDeadLettersAUniformNonThrottledFailureHoweverManyTimesItRecurs(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ids := []string{"UNIF-1", "UNIF-2", "UNIF-3", "UNIF-4", "UNIF-5", "UNIF-6", "UNIF-7", "UNIF-8"}
	for _, id := range ids {
		seedIssue(t, database, "UNIF", id, "Title for "+id)
	}
	embedder := &fakeEmbedder{err: errors.New("embed: expired credentials (simulated systemic outage)")}
	deps := Deps{Store: database, Embedder: embedder}

	for i := range retry.DeadLetterAttempts + 5 {
		for _, id := range ids {
			forceEligible(t, database, "issue", id)
		}
		succeeded, failed, _, throttled, err := ProcessBatch(context.Background(), deps)
		if err != nil {
			t.Fatalf("ProcessBatch cycle %d: %v", i, err)
		}
		if succeeded != 0 {
			t.Errorf("cycle %d: succeeded = %d, want 0 - the embedder never succeeds", i, succeeded)
		}
		if failed != len(ids) {
			t.Errorf("cycle %d: failed = %d, want %d", i, failed, len(ids))
		}
		if throttled {
			t.Errorf("cycle %d: throttled = true, want false - this error is not one of IsThrottled's codes", i)
		}
	}
	for _, id := range ids {
		var dead bool
		if err := database.Pool.QueryRow(context.Background(),
			`select dead from embeddings where kind = 'issue' and id = $1`, id,
		).Scan(&dead); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if dead {
			t.Errorf("%s dead = true after %d uniform-failure cycles, want false - nothing in the batch ever embedded, so this is never evidence about any row's content", id, retry.DeadLetterAttempts+5)
		}
	}
}

// cancelAfterNCallsEmbedder fails every call non-throttled, cancelling its own context exactly
// once, on call number cancelOn, to stand in for a context ending mid-bisection (as
// embed.RateLimitedEmbedder's pacing wait ending early does in production) without needing real
// timing or concurrency.
type cancelAfterNCallsEmbedder struct {
	bedrockShaped
	cancelOn int
	cancel   context.CancelFunc
	calls    int
}

func (e *cancelAfterNCallsEmbedder) Embed(_ context.Context, _ []string, _ embed.InputType) ([][]float32, error) {
	e.calls++
	if e.calls == e.cancelOn {
		e.cancel()
	}
	return nil, errors.New("simulated: a non-throttled embed failure")
}

// TestProcessBatchNeverMarksARowPermanentWhenItsContextEndsMidBisection proves a
// cancellation case: whatever error text comes back, a call that failed because its own context
// ended while it was in flight is never evidence about any row's content, independent of whether
// some other row already succeeded.
func TestProcessBatchNeverMarksARowPermanentWhenItsContextEndsMidBisection(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ids := []string{"CNCL-1", "CNCL-2", "CNCL-3", "CNCL-4"}
	for i, id := range ids {
		seedIssue(t, database, "CNCL", id, fmt.Sprintf("A title %d", i))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	embedder := &cancelAfterNCallsEmbedder{cancelOn: 2, cancel: cancel}
	deps := Deps{Store: database, Embedder: embedder}

	succeeded, failed, _, throttled, err := ProcessBatch(ctx, deps)
	if err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}
	if succeeded != 0 {
		t.Errorf("succeeded = %d, want 0", succeeded)
	}
	if failed != len(ids) {
		t.Errorf("failed = %d, want %d", failed, len(ids))
	}
	if throttled {
		t.Error("throttled = true, want false - this is a cancellation, not a throttle")
	}
	for _, id := range ids {
		var dead bool
		if err := database.Pool.QueryRow(context.Background(),
			`select dead from embeddings where kind = 'issue' and id = $1`, id,
		).Scan(&dead); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if dead {
			t.Errorf("%s dead = true after a context cancellation mid-bisection, want false - a cancellation is never evidence about any row's content", id)
		}
	}
}

// TestClaimRenewalExtendsNextAttemptAtPastClaimWindow proves a renewal due (its own last renewal
// older than renewClaimInterval) pushes next_attempt_at at least claimWindow past the moment it
// runs - needed since bisecting a batch every one of whose rows fails can itself take minutes,
// longer than claimWindow. The claim's age is read on the queue's clock (Deps.Clock), a fake the
// test moves past the interval; the renewal stamps the row on the database's clock (now() +
// claimWindow), so the moment it is judged against is that clock's, read before the renewal ran.
func TestClaimRenewalExtendsNextAttemptAtPastClaimWindow(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	seedIssue(t, database, "RNWL", "RNWL-1", "A title")
	ctx := context.Background()
	fake := clocktest.Held()
	renewal := newClaimRenewal(Deps{Store: database, Clock: fake}, []pendingRow{{kind: "issue", id: "RNWL-1", contentHash: contentHashOf(t, database, "issue", "RNWL-1")}}, fake.Now())
	fake.Advance(renewClaimInterval + time.Second)
	before := databaseNow(t, database)
	renewal.renew(ctx, 0)
	var nextAttempt time.Time
	if err := database.Pool.QueryRow(ctx,
		`select next_attempt_at from embeddings where kind = 'issue' and id = 'RNWL-1'`,
	).Scan(&nextAttempt); err != nil {
		t.Fatalf("read next_attempt_at: %v", err)
	}
	if nextAttempt.Before(before.Add(claimWindow)) {
		t.Errorf("next_attempt_at = %v, want at least claimWindow (%v) past %v, the database's clock before the renewal", nextAttempt, claimWindow, before)
	}
}

// TestReserveTokensSharesItsBudgetAcrossConnections proves the cross-process design:
// embeddings_rate_limit (migration 0078) is one row in the shared database, not in-process
// memory, so two distinct Deps values - standing in for the pollers of two envoy-dispatch
// processes, which share nothing else - draw from, and wait out debt against, the very same
// budget.
func TestReserveTokensSharesItsBudgetAcrossConnections(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()
	const ceiling = 120 // tokens/minute = 2 tokens/second

	// A direct write stands in for a first process's own reservation having already returned,
	// leaving the shared row at a 2-token deficit - deliberately not reserveTokens' own blocking
	// call, which would wait out exactly the debt it created and read back near zero by the time
	// a second reservation ran right after it, proving nothing about whether the row's state is
	// actually shared. The write holds the row locked until the reservation's statement has
	// started and queued behind it, and only then stamps the refill time, on the database's clock,
	// and commits: the reservation refills from its statement's start (now()), which is before
	// that stamp, so it refills nothing, however long the machine takes between the two.
	seed, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the seeding transaction: %v", err)
	}
	defer func() { _ = seed.Rollback(ctx) }()
	if _, err := seed.Exec(ctx, `update embeddings_rate_limit set tokens_available = -2`); err != nil {
		t.Fatalf("seed an existing deficit: %v", err)
	}

	// A second, distinct Deps value - sharing no in-memory state with whatever wrote that
	// deficit, standing in for a second OS process - reserves 2 more tokens and must wait out
	// the existing deficit plus its own. The wait runs on the queue's clock (Deps.Clock), a fake
	// that never moves by itself, so the deadline it arms is exactly the wait the reservation
	// computed.
	fake := newQueueClock(t, database)
	start := fake.Now()
	reserved := make(chan struct{})
	go func() {
		defer close(reserved)
		reserveTokens(ctx, Deps{Store: database, Clock: fake}, 2, ceiling, nil)
	}()
	storetest.WaitForLockWaiters(t, seed, 1)
	var seeded time.Time
	if err := seed.QueryRow(ctx, `update embeddings_rate_limit set last_refill_at = clock_timestamp() returning last_refill_at`).Scan(&seeded); err != nil {
		t.Fatalf("stamp the deficit's refill time: %v", err)
	}
	if err := seed.Commit(ctx); err != nil {
		t.Fatalf("commit the deficit: %v", err)
	}
	armed := fake.AwaitArmed(t, "the wait the reservation owes the shared budget", 1)
	// The reservation's own record of what it owed is the shared row it wrote, on the database's
	// clock: it refilled the seeded deficit for the time between the seed's stamp and its own
	// statement's start (none, or less than none), took its own 2 tokens from that, and armed the
	// wait the deficit left at the ceiling's rate. A reservation that started fresh would leave a
	// deficit of its own 2 tokens alone.
	var available, refilled float64
	if err := database.Pool.QueryRow(ctx, `
		select tokens_available, extract(epoch from last_refill_at - $1::timestamptz) * ($2::double precision / 60.0)
		from embeddings_rate_limit`, seeded, float64(ceiling)).Scan(&available, &refilled); err != nil {
		t.Fatalf("read the shared budget the reservation wrote: %v", err)
	}
	if want := -2 + refilled - 2; math.Abs(available-want) > 1e-6 {
		t.Errorf("the reservation left the shared budget at %.4f tokens, want %.4f: the seeded 2-token deficit, refilled by %.4f since, less its own 2 - it should have read the shared row's existing debt from a separate writer, not started fresh",
			available, want, refilled)
	}
	if owed, want := armed[0].Sub(start), time.Duration(-available/(float64(ceiling)/60.0)*float64(time.Second)); owed != want {
		t.Errorf("the reservation waits %v out, want %v: the %.4f-token deficit it left, at %d tokens a minute", owed, want, -available, ceiling)
	}
	fake.Set(armed[0])
	testwait.Receive(t, "the reservation to return once its wait is out", reserved)
}

// TestWaitOutDebtRenewsTheClaimMidWaitSoNoSecondWorkerClaimsIt proves a single reservation's own
// wait can run past claimWindow (a full batch of maximum-length documents can need far more than
// one minute's token ceiling to wait out), so waitOutDebt must renew the claim partway through a
// long wait, not only before or after it. The wait runs on the queue's clock (Deps.Clock), a fake
// the test moves one slice at a time once the loop has armed it, so the wait of four
// renewClaimInterval takes no real time while exercising the identical loop: a claim renewed just
// before the wait is renewed again inside it, before the wait ends, and that renewal reaches the
// database. The renewals are read on the fake; the row's stamp on the database's clock, against
// a reading of that clock taken before the wait.
func TestWaitOutDebtRenewsTheClaimMidWaitSoNoSecondWorkerClaimsIt(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	seedIssue(t, database, "LONG", "LONG-1", "A title")
	ctx := context.Background()
	fake := newQueueClock(t, database)
	renewal := newClaimRenewal(Deps{Store: database, Clock: fake}, []pendingRow{{kind: "issue", id: "LONG-1", contentHash: contentHashOf(t, database, "issue", "LONG-1")}}, fake.Now())
	var renewed []time.Duration
	write := renewal.extend
	before := fake.Now()
	renewal.extend = func(ctx context.Context, rows []pendingRow) error {
		renewed = append(renewed, fake.Now().Sub(before))
		return write(ctx, rows)
	}
	databaseBefore := databaseNow(t, database)

	const waitSlices = 4
	waited := make(chan struct{})
	go func() {
		defer close(waited)
		waitOutDebt(ctx, fake, waitSlices*renewClaimInterval, renewal)
	}()
	for range waitSlices {
		fake.Set(fake.AwaitArmed(t, "a slice of the debt wait", 1)[0])
	}
	testwait.Receive(t, "the debt wait to return once every slice is out", waited)
	ended := fake.Now().Sub(before)

	if len(renewed) == 0 || renewed[0] >= ended-renewClaimInterval {
		t.Errorf("renewals at %v in a wait that ended at %v; want one inside the wait, an interval or more before its end", renewed, ended)
	}
	var nextAttempt time.Time
	if err := database.Pool.QueryRow(ctx,
		`select next_attempt_at from embeddings where kind = 'issue' and id = 'LONG-1'`,
	).Scan(&nextAttempt); err != nil {
		t.Fatalf("read next_attempt_at: %v", err)
	}
	if nextAttempt.Before(databaseBefore.Add(claimWindow)) {
		t.Errorf("next_attempt_at = %v, want at least claimWindow (%v) past %v, the database's clock before the wait - the mid-wait renewal should have pushed it out, so a concurrent scanner never reclaims LONG-1 while this reservation is still waiting", nextAttempt, claimWindow, databaseBefore)
	}
}

// TestClaimRenewalResolveStopsRenewingACommittedRow proves resolve removes a row from the set a
// claimRenewal still protects: a resolved row's next_attempt_at is untouched by a later renewal,
// while an unresolved sibling's is still pushed out. The claim's age is read on the queue's clock
// (Deps.Clock), a fake the test moves past renewClaimInterval; the sibling's stamp is the
// database's (now() + claimWindow), judged against that clock read before the renewal.
func TestClaimRenewalResolveStopsRenewingACommittedRow(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	seedIssue(t, database, "RSLV", "RSLV-1", "A good title")
	seedIssue(t, database, "RSLV", "RSLV-2", "A pending title")
	ctx := context.Background()

	sentinel := time.Now().Add(-time.Hour).Truncate(time.Microsecond)
	if _, err := database.Pool.Exec(ctx,
		`update embeddings set next_attempt_at = $1 where kind = 'issue' and id = 'RSLV-1'`, sentinel,
	); err != nil {
		t.Fatalf("seed a stale sentinel on RSLV-1: %v", err)
	}

	fake := clocktest.Held()
	renewal := newClaimRenewal(Deps{Store: database, Clock: fake}, []pendingRow{
		{kind: "issue", id: "RSLV-1", contentHash: contentHashOf(t, database, "issue", "RSLV-1")},
		{kind: "issue", id: "RSLV-2", contentHash: contentHashOf(t, database, "issue", "RSLV-2")},
	}, fake.Now())
	renewal.resolve([]pendingRow{{kind: "issue", id: "RSLV-1"}})
	fake.Advance(renewClaimInterval + time.Second)
	before := databaseNow(t, database)
	renewal.renew(ctx, 0)

	var rslv1Next, rslv2Next time.Time
	if err := database.Pool.QueryRow(ctx,
		`select next_attempt_at from embeddings where kind = 'issue' and id = 'RSLV-1'`,
	).Scan(&rslv1Next); err != nil {
		t.Fatalf("read RSLV-1: %v", err)
	}
	if err := database.Pool.QueryRow(ctx,
		`select next_attempt_at from embeddings where kind = 'issue' and id = 'RSLV-2'`,
	).Scan(&rslv2Next); err != nil {
		t.Fatalf("read RSLV-2: %v", err)
	}
	if !rslv1Next.Equal(sentinel) {
		t.Errorf("RSLV-1 next_attempt_at = %v, want unchanged (%v) - it was resolved, so this renewal should never have touched it", rslv1Next, sentinel)
	}
	if rslv2Next.Before(before.Add(claimWindow)) {
		t.Errorf("RSLV-2 next_attempt_at = %v, want at least claimWindow (%v) past %v, the database's clock before the renewal - it was never resolved, so this renewal should still protect it", rslv2Next, claimWindow, before)
	}
}

// seedManyIssues bulk-inserts count issues under projectKey (created first if needed), numbered
// sequentially from the project's current max, each titled by titleFor(i) for i in [0, count) -
// one round trip, not one per row, for a test that needs many rows at once.
func seedManyIssues(t *testing.T, database *store.Store, projectKey string, count int, titleFor func(i int) string) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ($1, 'Test', 'alice') on conflict (key) do nothing
	`, projectKey); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	var base int
	if err := database.Pool.QueryRow(ctx,
		`select coalesce(max(number), 0) from issues where project_key = $1`, projectKey,
	).Scan(&base); err != nil {
		t.Fatalf("read current max issue number: %v", err)
	}
	keys := make([]string, count)
	numbers := make([]int, count)
	titles := make([]string, count)
	for i := range count {
		numbers[i] = base + i + 1
		keys[i] = fmt.Sprintf("%s-%d", projectKey, numbers[i])
		titles[i] = titleFor(i)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		select k, $1, n, t, '{"kind":"user","id":"alice"}', 'A'
		from unnest($2::text[], $3::int[], $4::text[]) as v(k, n, t)
	`, projectKey, keys, numbers, titles); err != nil {
		t.Fatalf("bulk seed issues: %v", err)
	}
}

// countingGroupEmbedder fails any call whose texts include poison (simulating one bad row's
// group) and otherwise succeeds, recording every call's own size so a test can prove exactly how
// many Embed calls ran and how many total rows they carried. Its limits are Bedrock's with no byte
// budget, so the token ceiling is what closes a group of its test.
type countingGroupEmbedder struct {
	bedrockShaped
	poison   string
	calls    int
	rowsSent int
}

func (*countingGroupEmbedder) Limits() embed.Limits {
	limits := new(embed.BedrockClient).Limits()
	limits.MaxBatchBytes = 0
	return limits
}

func (e *countingGroupEmbedder) Embed(_ context.Context, texts []string, _ embed.InputType) ([][]float32, error) {
	e.calls++
	e.rowsSent += len(texts)
	for _, text := range texts {
		if text == e.poison {
			return nil, errors.New("simulated: a non-throttled embed failure")
		}
	}
	return fakeVectors(len(texts)), nil
}

// TestChunkForBudgetSplitsAnOverCeilingBatchAndEmbedRowsCommitsEachGroupOnlyOnce proves both
// halves of embedRows' own contract at once: chunkForBudget actually produces more than one
// group once a batch's combined estimated tokens exceed the embedder's BackgroundTokensPerMinute
// (Bedrock's 200,000: 25 rows at embed.MaxInputRunes, each truncated to exactly 8,000 estimated tokens,
// fill one 200,000-token group exactly; a 26th, poisoned row starts a second), and a later
// group's failure never re-embeds an earlier group's already-committed rows: Embed is called
// exactly three times (once for each group, plus confirmSoloFailure's own canary probe once the
// poisoned row is isolated to one - never once per row or once per bisection level), carrying 26
// real rows plus the one canary.
func TestChunkForBudgetSplitsAnOverCeilingBatchAndEmbedRowsCommitsEachGroupOnlyOnce(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	const healthyCount = 25                    // 25 * 8,000 = 200,000 = Bedrock's BackgroundTokensPerMinute exactly
	healthyTitle := strings.Repeat("h", 32000) // embed.MaxInputRunes: truncates to exactly 8,000 estimated tokens
	poisonTitle := "poisoned"
	seedManyIssues(t, database, "CHNK", healthyCount+1, func(i int) string {
		if i == healthyCount {
			return poisonTitle
		}
		return healthyTitle
	})
	embedder := &countingGroupEmbedder{poison: poisonTitle}
	deps := Deps{Store: database, Embedder: embedder}

	succeeded, failed, _, throttled, err := ProcessBatch(context.Background(), deps)
	if err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}
	if succeeded != healthyCount {
		t.Errorf("succeeded = %d, want %d", succeeded, healthyCount)
	}
	if failed != 1 {
		t.Errorf("failed = %d, want 1", failed)
	}
	if throttled {
		t.Error("throttled = true, want false - this is a non-throttled failure")
	}
	if embedder.calls != 3 {
		t.Errorf("Embed was called %d times, want exactly 3 - chunkForBudget should produce exactly two groups (the 25 healthy rows filling one 200,000-token group, the poisoned row starting a second) plus confirmSoloFailure's one canary probe, and the healthy group's success must never trigger a re-embed of it", embedder.calls)
	}
	if embedder.rowsSent != healthyCount+2 {
		t.Errorf("rows sent across all Embed calls = %d, want %d (26 real rows, no row re-sent, plus the one-row canary probe)", embedder.rowsSent, healthyCount+2)
	}
}

// canaryAwareEmbedder shares one success path (oneVector(), for both a real row's embed and
// embedCanary's own) across every round-9 test: rowOK decides a non-canary call's own outcome
// (nil means always succeed), canaryOK decides embedCanary's own outcome (nil means always
// succeed) - so a test only ever supplies the one or two closures it actually needs to vary.
type canaryAwareEmbedder struct {
	bedrockShaped
	rowOK    func(texts []string) bool
	canaryOK func() bool
	calls    int
}

func (e *canaryAwareEmbedder) Embed(_ context.Context, texts []string, _ embed.InputType) ([][]float32, error) {
	e.calls++
	ok := true
	if len(texts) == 1 && texts[0] == embedCanary {
		if e.canaryOK != nil {
			ok = e.canaryOK()
		}
	} else if e.rowOK != nil {
		ok = e.rowOK(texts)
	}
	if !ok {
		return nil, errors.New("simulated: a non-throttled embed failure")
	}
	return fakeVectors(len(texts)), nil
}

// sequencedEmbedder scripts outcomes by call index alone, regardless of content or whether a
// call is a row's own or embedCanary's - Deep's and Qual's own probes are both specified this
// way ("call 1 fails, call 2 succeeds, ...").
type sequencedEmbedder struct {
	bedrockShaped
	results   []bool // results[i] is call i's outcome (0-indexed)
	afterward bool   // every call past len(results)
	calls     int
}

func (e *sequencedEmbedder) Embed(_ context.Context, texts []string, _ embed.InputType) ([][]float32, error) {
	idx := e.calls
	e.calls++
	ok := e.afterward
	if idx < len(e.results) {
		ok = e.results[idx]
	}
	if !ok {
		return nil, errors.New("simulated: a non-throttled embed failure")
	}
	return fakeVectors(len(texts)), nil
}

// TestProcessBatchANewOutageAfterAnEarlierSuccessNeverConfirmsAnything proves confirmSoloFailure
// never lets an earlier row's unrelated success confirm a later, unrelated recurrence of a
// systemic outage: each solo failure is decided by its own fresh canary probe, run at the
// instant of that failure, never by anything an earlier row in the same call did.
func TestProcessBatchANewOutageAfterAnEarlierSuccessNeverConfirmsAnything(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	healthyIDs := []string{"FLAP-1", "FLAP-2", "FLAP-3", "FLAP-4"}
	outageIDs := []string{"FLAP-5", "FLAP-6", "FLAP-7", "FLAP-8"}
	for i, id := range healthyIDs {
		seedIssue(t, database, "FLAP", id, fmt.Sprintf("Healthy title %d", i))
	}
	for i, id := range outageIDs {
		seedIssue(t, database, "FLAP", id, fmt.Sprintf("Outage title %d", i))
	}
	embedder := &canaryAwareEmbedder{
		rowOK: func(texts []string) bool {
			for _, text := range texts {
				if strings.HasPrefix(text, "Outage title") {
					return false
				}
			}
			return true
		},
		canaryOK: func() bool { return false }, // the new outage breaks the canary too - nothing it probes is ever confirmed
	}
	deps := Deps{Store: database, Embedder: embedder}

	succeeded, failed, _, _, err := ProcessBatch(context.Background(), deps)
	if err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}
	if succeeded != 4 {
		t.Errorf("succeeded = %d, want 4", succeeded)
	}
	if failed != 4 {
		t.Errorf("failed = %d, want 4", failed)
	}
	rows, qErr := database.Pool.Query(context.Background(),
		`select id, dead from embeddings where kind = 'issue' and id = any($1::text[])`, outageIDs)
	if qErr != nil {
		t.Fatalf("read embeddings: %v", qErr)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var dead bool
		if err := rows.Scan(&id, &dead); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if dead {
			t.Errorf("%s dead = true, want false - every failure against it was the new outage, never its own content, and its own canary also failed every time", id)
		}
	}
}

// TestProcessBatchRevRound6ProbeStillGreen replays round 6's own scenario (an outage that clears
// mid-bisection) against the canary redesign: realCalls counts only rows' own embed attempts (not
// embedCanary's), so confirmSoloFailure's canary probe reads the condition "right now" without
// itself advancing it - the row isolated while the outage is still active sees a failing canary
// and is never confirmed; the outage clearing from the 5th real call on lets everything else
// through.
func TestProcessBatchRevRound6ProbeStillGreen(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ids := []string{"REV6-1", "REV6-2", "REV6-3", "REV6-4", "REV6-5", "REV6-6", "REV6-7", "REV6-8"}
	for i, id := range ids {
		seedIssue(t, database, "REV6", id, fmt.Sprintf("Title %d", i))
	}
	realCalls := 0
	embedder := &canaryAwareEmbedder{
		rowOK: func(_ []string) bool {
			realCalls++
			return realCalls > 4
		},
		canaryOK: func() bool { return realCalls > 4 },
	}
	deps := Deps{Store: database, Embedder: embedder}

	succeeded, failed, _, throttled, err := ProcessBatch(context.Background(), deps)
	if err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}
	if succeeded != 7 {
		t.Errorf("succeeded = %d, want 7", succeeded)
	}
	if failed != 1 {
		t.Errorf("failed = %d, want 1", failed)
	}
	if throttled {
		t.Error("throttled = true, want false")
	}
	rows, qErr := database.Pool.Query(context.Background(),
		`select id, dead from embeddings where kind = 'issue' and id = any($1::text[])`, ids)
	if qErr != nil {
		t.Fatalf("read embeddings: %v", qErr)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var dead bool
		if err := rows.Scan(&id, &dead); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if dead {
			t.Errorf("%s dead = true, want false", id)
		}
	}
}

// TestProcessBatchDeepsFlappingProbeNeverWronglyConfirms is Deep's own probe: 2 rows, the
// embedder fails, succeeds, fails (by call index alone, regardless of content) - every call past
// that third one keeps failing. Neither row's own content is ever at fault; a genuine flap must
// never be mistaken for one.
func TestProcessBatchDeepsFlappingProbeNeverWronglyConfirms(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ids := []string{"DEEP-1", "DEEP-2"}
	for i, id := range ids {
		seedIssue(t, database, "DEEP", id, fmt.Sprintf("Title %d", i))
	}
	embedder := &sequencedEmbedder{results: []bool{false, true, false}, afterward: false}
	deps := Deps{Store: database, Embedder: embedder}

	if _, _, _, _, err := ProcessBatch(context.Background(), deps); err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}
	rows, qErr := database.Pool.Query(context.Background(),
		`select id, dead from embeddings where kind = 'issue' and id = any($1::text[])`, ids)
	if qErr != nil {
		t.Fatalf("read embeddings: %v", qErr)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var dead bool
		if err := rows.Scan(&id, &dead); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if dead {
			t.Errorf("%s dead = true, want false - a flapping embedder is never evidence against any row's own content", id)
		}
	}
}

// TestProcessBatchQualsFlappingProbeNeverWronglyConfirms is Qual's own probe: call 1 fails, call
// 2 succeeds, every later call fails.
func TestProcessBatchQualsFlappingProbeNeverWronglyConfirms(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ids := []string{"QUAL-1", "QUAL-2"}
	for i, id := range ids {
		seedIssue(t, database, "QUAL", id, fmt.Sprintf("Title %d", i))
	}
	embedder := &sequencedEmbedder{results: []bool{false, true}, afterward: false}
	deps := Deps{Store: database, Embedder: embedder}

	if _, _, _, _, err := ProcessBatch(context.Background(), deps); err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}
	rows, qErr := database.Pool.Query(context.Background(),
		`select id, dead from embeddings where kind = 'issue' and id = any($1::text[])`, ids)
	if qErr != nil {
		t.Fatalf("read embeddings: %v", qErr)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var dead bool
		if err := rows.Scan(&id, &dead); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if dead {
			t.Errorf("%s dead = true, want false - a flapping embedder is never evidence against any row's own content", id)
		}
	}
}

// TestProcessBatchACanaryThatFailsMarksNothing isolates confirmSoloFailure's own two outcomes
// directly: a single row whose own call fails, and whose canary also fails, must never be
// confirmed permanent - it goes back to retry, exactly like any other systemic failure.
func TestProcessBatchACanaryThatFailsMarksNothing(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	seedIssue(t, database, "CNRY", "CNRY-1", "A title")
	embedder := &canaryAwareEmbedder{
		rowOK:    func(_ []string) bool { return false },
		canaryOK: func() bool { return false },
	}
	deps := Deps{Store: database, Embedder: embedder}

	succeeded, failed, _, throttled, err := ProcessBatch(context.Background(), deps)
	if err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}
	if succeeded != 0 || failed != 1 || throttled {
		t.Errorf("succeeded, failed, throttled = %d, %d, %v, want 0, 1, false", succeeded, failed, throttled)
	}
	var dead bool
	var confirmedFailures int
	if err := database.Pool.QueryRow(context.Background(),
		`select dead, confirmed_failures from embeddings where kind = 'issue' and id = 'CNRY-1'`,
	).Scan(&dead, &confirmedFailures); err != nil {
		t.Fatalf("read CNRY-1: %v", err)
	}
	if dead {
		t.Error("CNRY-1 dead = true, want false - its canary also failed, so nothing about it was confirmed")
	}
	if confirmedFailures != 0 {
		t.Errorf("CNRY-1 confirmed_failures = %d, want 0", confirmedFailures)
	}
}

// TestProcessBatchStillDeadLettersAGenuinelyPoisonedRowAmongHealthyOnes proves the canary
// redesign is not overcorrected into never dead-lettering anything: a row whose own content
// genuinely, repeatedly breaks the embed call, confirmed each time by a successful canary probe
// proving the service itself works, still crosses the dead-letter threshold.
func TestProcessBatchStillDeadLettersAGenuinelyPoisonedRowAmongHealthyOnes(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	healthyIDs := []string{"POIS-1", "POIS-2", "POIS-3"}
	for i, id := range healthyIDs {
		seedIssue(t, database, "POIS", id, fmt.Sprintf("Healthy title %d", i))
	}
	seedIssue(t, database, "POIS", "POIS-4", "the poisoned title")
	if _, err := database.Pool.Exec(context.Background(),
		`update embeddings set confirmed_failures = 9 where kind = 'issue' and id = 'POIS-4'`,
	); err != nil {
		t.Fatalf("prime confirmed_failures: %v", err)
	}
	embedder := &canaryAwareEmbedder{
		rowOK: func(texts []string) bool {
			for _, text := range texts {
				if text == "the poisoned title" {
					return false
				}
			}
			return true
		},
		canaryOK: func() bool { return true },
	}
	deps := Deps{Store: database, Embedder: embedder}

	succeeded, failed, _, throttled, err := ProcessBatch(context.Background(), deps)
	if err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}
	if succeeded != 3 {
		t.Errorf("succeeded = %d, want 3", succeeded)
	}
	if failed != 1 {
		t.Errorf("failed = %d, want 1", failed)
	}
	if throttled {
		t.Error("throttled = true, want false")
	}
	var dead bool
	if err := database.Pool.QueryRow(context.Background(),
		`select dead from embeddings where kind = 'issue' and id = 'POIS-4'`,
	).Scan(&dead); err != nil {
		t.Fatalf("read POIS-4: %v", err)
	}
	if !dead {
		t.Error("POIS-4 dead = false, want true - its canary succeeded every time, proving the service works, so its own repeated failure should cross the dead-letter threshold")
	}
}

// TestProcessBatchALonePoisonedRowWithNoBatchSiblingsStillDeadLetters proves confirmSoloFailure
// needs no sibling at all: a row that is the entire batch, every cycle, with no other row ever
// pending beside it, is confirmed by its own canary exactly as a row bisection isolated to one
// would be, and dead-letters once confirmed_failures reaches retry.DeadLetterAttempts.
func TestProcessBatchALonePoisonedRowWithNoBatchSiblingsStillDeadLetters(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	seedIssue(t, database, "LONE", "LONE-1", "the poisoned title")
	embedder := &canaryAwareEmbedder{
		rowOK:    func(_ []string) bool { return false },
		canaryOK: func() bool { return true },
	}
	deps := Deps{Store: database, Embedder: embedder}

	for i := 0; i < retry.DeadLetterAttempts; i++ {
		succeeded, failed, _, throttled, err := ProcessBatch(context.Background(), deps)
		if err != nil {
			t.Fatalf("cycle %d: ProcessBatch: %v", i, err)
		}
		if succeeded != 0 || failed != 1 || throttled {
			t.Fatalf("cycle %d: succeeded, failed, throttled = %d, %d, %v, want 0, 1, false", i, succeeded, failed, throttled)
		}
		forceEligible(t, database, "issue", "LONE-1")
	}
	var dead bool
	var confirmedFailures int
	if err := database.Pool.QueryRow(context.Background(),
		`select dead, confirmed_failures from embeddings where kind = 'issue' and id = 'LONE-1'`,
	).Scan(&dead, &confirmedFailures); err != nil {
		t.Fatalf("read LONE-1: %v", err)
	}
	if !dead {
		t.Error("LONE-1 dead = false, want true - its canary succeeded every cycle, proving the service works, and it was never bisected (it is the whole batch every time)")
	}
	if confirmedFailures != retry.DeadLetterAttempts {
		t.Errorf("LONE-1 confirmed_failures = %d, want %d", confirmedFailures, retry.DeadLetterAttempts)
	}
}

// TestProcessBatchALoneHealthyRowDuringAnOutageNeverDeadLetters is the mirror of the test above:
// a row that is the entire batch every cycle, whose own content is fine, but whose canary also
// fails every cycle (a genuine outage, not this row's fault) is never confirmed and never
// dead-letters, however many cycles it runs for - run here for more cycles than
// retry.DeadLetterAttempts would take to confirm a genuinely poisoned row.
func TestProcessBatchALoneHealthyRowDuringAnOutageNeverDeadLetters(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	seedIssue(t, database, "LONE", "LONE-2", "a perfectly good title")
	embedder := &canaryAwareEmbedder{
		rowOK:    func(_ []string) bool { return false },
		canaryOK: func() bool { return false },
	}
	deps := Deps{Store: database, Embedder: embedder}

	for i := 0; i < retry.DeadLetterAttempts+5; i++ {
		succeeded, failed, _, throttled, err := ProcessBatch(context.Background(), deps)
		if err != nil {
			t.Fatalf("cycle %d: ProcessBatch: %v", i, err)
		}
		if succeeded != 0 || failed != 1 || throttled {
			t.Fatalf("cycle %d: succeeded, failed, throttled = %d, %d, %v, want 0, 1, false", i, succeeded, failed, throttled)
		}
		forceEligible(t, database, "issue", "LONE-2")
	}
	var dead bool
	var confirmedFailures int
	if err := database.Pool.QueryRow(context.Background(),
		`select dead, confirmed_failures from embeddings where kind = 'issue' and id = 'LONE-2'`,
	).Scan(&dead, &confirmedFailures); err != nil {
		t.Fatalf("read LONE-2: %v", err)
	}
	if dead {
		t.Error("LONE-2 dead = true, want false - its canary failed every cycle too, so nothing about its own content was ever confirmed")
	}
	if confirmedFailures != 0 {
		t.Errorf("LONE-2 confirmed_failures = %d, want 0", confirmedFailures)
	}
}

// TestProcessBatchACanaryThatIsItselfThrottledNeverConfirmsAndPropagatesThrottled exercises
// confirmSoloFailure's one remaining branch no other committed test reaches: the canary probe
// itself throttles (rather than failing outright). Nothing is confirmed - a throttled canary is
// exactly as systemic as a throttled row - and ProcessBatch reports throttled so the caller backs
// off its own cadence exactly as an outright-throttled call would.
func TestProcessBatchACanaryThatIsItselfThrottledNeverConfirmsAndPropagatesThrottled(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	seedIssue(t, database, "CNRY", "CNRY-2", "a title")
	deps := Deps{Store: database, Embedder: canaryThrottledEmbedder{}}

	succeeded, failed, _, throttled, err := ProcessBatch(context.Background(), deps)
	if err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}
	if succeeded != 0 || failed != 1 {
		t.Errorf("succeeded, failed = %d, %d, want 0, 1", succeeded, failed)
	}
	if !throttled {
		t.Error("throttled = false, want true - the canary itself was throttled, so the caller should back off exactly as an outright-throttled call would")
	}
	var dead bool
	var confirmedFailures int
	if err := database.Pool.QueryRow(context.Background(),
		`select dead, confirmed_failures from embeddings where kind = 'issue' and id = 'CNRY-2'`,
	).Scan(&dead, &confirmedFailures); err != nil {
		t.Fatalf("read CNRY-2: %v", err)
	}
	if dead {
		t.Error("CNRY-2 dead = true, want false - a throttled canary confirms nothing")
	}
	if confirmedFailures != 0 {
		t.Errorf("CNRY-2 confirmed_failures = %d, want 0", confirmedFailures)
	}
}

// canaryThrottledEmbedder fails every row's own call non-throttled, and throttles embedCanary's
// call specifically - confirmSoloFailure's one branch no other committed test exercises.
type canaryThrottledEmbedder struct{ bedrockShaped }

func (canaryThrottledEmbedder) Embed(_ context.Context, texts []string, _ embed.InputType) ([][]float32, error) {
	if len(texts) == 1 && texts[0] == embedCanary {
		return nil, embedtest.FakeThrottleError{Code: "ThrottlingException"}
	}
	return nil, errors.New("simulated: a non-throttled embed failure")
}

// TestProcessBatchActuallyCallsRenewalAndReservesTokens is the first of Rev's two named test
// gaps: no test proved renewal.renew or reserveBackgroundTokens were actually wired into the
// embed path, as opposed to merely existing and passing their own isolated unit tests - deleting
// either call site left the full suite green. The queue runs on a fake clock here (Deps.Clock),
// which the fake embedder moves on between its two calls, where a real embedder's own network
// latency would be, so the claim is old enough on that clock for the renewal before the second
// call to be due. The first call reads the claim the scan wrote and the second requires it raised
// since - the one signal that survives retryRows' own final overwrite of next_attempt_at at the
// end of the call, which would otherwise hide an intermediate renewal from a check made after
// ProcessBatch returns. The scan's own claim is no renewal: it already sets next_attempt_at
// claimWindow on, so only a later claim than the scan's proves one. The token budget is checked
// the ordinary way: it must be measurably drawn down.
type wiringCheckEmbedder struct {
	bedrockShaped
	t          *testing.T
	database   *store.Store
	clock      *clocktest.Fake
	ids        []string
	calls      int
	claimed    time.Time
	sawRenewal bool
}

func (e *wiringCheckEmbedder) Embed(ctx context.Context, texts []string, _ embed.InputType) ([][]float32, error) {
	e.calls++
	var minNext time.Time
	if err := e.database.Pool.QueryRow(ctx,
		`select min(next_attempt_at) from embeddings where kind = 'issue' and id = any($1::text[])`, e.ids,
	).Scan(&minNext); err != nil {
		e.t.Fatalf("read next_attempt_at mid-call: %v", err)
	}
	if e.calls == 1 {
		e.claimed = minNext
		// Where a real embedder's round trip would be: on the queue's clock, the age at which a
		// call renews the claim before it goes out (claimRenewal.due with a call's need, callBound
		// plus callSlack), and short of callBound, so this call's own bound does not end it.
		e.clock.Advance(claimWindow - callBound - callSlack)
		return nil, errors.New("simulated: a non-throttled embed failure")
	}
	if minNext.After(e.claimed) {
		e.sawRenewal = true
	}
	return fakeVectors(len(texts)), nil
}

func TestProcessBatchActuallyCallsRenewalAndReservesTokens(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ids := []string{"WIRE-1", "WIRE-2"}
	for i, id := range ids {
		seedIssue(t, database, "WIRE", id, fmt.Sprintf("Title %d", i))
	}
	fake := clocktest.Held()

	var tokensBefore float64
	if err := database.Pool.QueryRow(context.Background(),
		`select tokens_available from embeddings_rate_limit`,
	).Scan(&tokensBefore); err != nil {
		t.Fatalf("read tokens_available before: %v", err)
	}
	embedder := &wiringCheckEmbedder{t: t, database: database, clock: fake, ids: ids}
	deps := Deps{Store: database, Embedder: embedder, Clock: fake}

	if _, _, _, _, err := ProcessBatch(context.Background(), deps); err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}
	if !embedder.sawRenewal {
		t.Error("the second embed call never observed a prior renewal - renewal.renew does not appear to be wired into the embed path")
	}
	var tokensAfter float64
	if err := database.Pool.QueryRow(context.Background(),
		`select tokens_available from embeddings_rate_limit`,
	).Scan(&tokensAfter); err != nil {
		t.Fatalf("read tokens_available after: %v", err)
	}
	if tokensAfter >= tokensBefore {
		t.Errorf("tokens_available = %v after ProcessBatch, want less than %v before it - reserveBackgroundTokens does not appear to be wired into the embed path", tokensAfter, tokensBefore)
	}
}

// claimLeftEmbedder embeds every call, and at the first reads until when the claim on ids holds:
// the earliest next_attempt_at, a moment of the database's own clock.
type claimLeftEmbedder struct {
	bedrockShaped
	database *store.Store
	ids      []string
	calls    int
	until    time.Time
	err      error
}

func (e *claimLeftEmbedder) Embed(ctx context.Context, texts []string, _ embed.InputType) ([][]float32, error) {
	e.calls++
	if e.calls == 1 {
		e.err = e.database.Pool.QueryRow(ctx, `
			select min(next_attempt_at) from embeddings
			where kind = 'issue' and id = any($1::text[])`, e.ids,
		).Scan(&e.until)
	}
	return fakeVectors(len(texts)), nil
}

// The claim scanPending makes is dated before the scan's statement (ProcessBatch reads claimed
// first), so a scan slow to return leaves the claim reading no younger than it is. The database
// dates the claim by its now(), the scan's transaction's start; here the statement then waits on a
// lock the test holds, which a trigger on the claim's column takes, while the test moves the
// queue's clock (Deps.Clock), on which ProcessBatch dates the claim and the renewal reads its age,
// by scanHeld, 5/4 of renewClaimInterval. So the claim is due when the first call goes out and is
// renewed first: the call reads the claim pushed out claimWindow from the renewal's now(), later
// than the held scan's start. Dated after the scan, the claim read scanHeld younger, was not
// renewed, and the call read the scan's own claim: claimWindow past the held scan's start,
// exactly. The held scan's start is read from pg_stat_activity while it waits.
func TestAClaimIsDatedBeforeTheScanThatMadeIt(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ids := []string{"SCAN-1", "SCAN-2"}
	for i, id := range ids {
		seedIssue(t, database, "SCAN", id, fmt.Sprintf("Title %d", i))
	}
	const scanHeld = 5 * renewClaimInterval / 4
	mustExec(t, database.Pool, `create function hold_claim() returns trigger language plpgsql as $$
		begin perform pg_advisory_xact_lock(1); return new; end $$`)
	mustExec(t, database.Pool, `create trigger hold_claim before update of next_attempt_at on embeddings
		for each row execute function hold_claim()`)
	ctx := context.Background()
	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holder: %v", err)
	}
	defer holder.Rollback(ctx)
	mustExec(t, holder, `select pg_advisory_xact_lock(1)`)

	fake := newQueueClock(t, database)
	embedder := &claimLeftEmbedder{database: database, ids: ids}
	done := make(chan error, 1)
	go func() {
		_, _, _, _, err := ProcessBatch(ctx, Deps{Store: database, Embedder: embedder, Clock: fake})
		done <- err
	}()
	storetest.WaitForLockWaiters(t, holder, 1)
	var scanStarted time.Time
	if err := database.Pool.QueryRow(ctx, `
		select a.xact_start from pg_locks l join pg_stat_activity a on a.pid = l.pid
		where l.locktype = 'advisory' and not l.granted and a.datname = current_database()`,
	).Scan(&scanStarted); err != nil {
		t.Fatalf("read when the held scan's transaction started: %v", err)
	}
	fake.Advance(scanHeld)
	if err := holder.Rollback(ctx); err != nil {
		t.Errorf("release the scan: %v", err)
	}
	if err := testwait.Receive(t, "ProcessBatch to return once the scan is let go", done); err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}
	if embedder.calls == 0 || embedder.err != nil {
		t.Fatalf("%d calls; reading the claim in the first: %v", embedder.calls, embedder.err)
	}
	if scanned := scanStarted.Add(claimWindow); !embedder.until.After(scanned) {
		t.Errorf("the first call went out with its claim until %v, want after %v, claimWindow past the held scan's start: no renewal came before the call, though the claim was %v old on the queue's clock once the scan returned, past renewClaimInterval (%v)",
			embedder.until, scanned, scanHeld, renewClaimInterval)
	}
}

// TestMergeBisectResultsCopiesIntoFreshSlicesNotTheSharedBackingArray is the second of Rev's two
// named test gaps: reverting mergeBisectResults back to append(left.X, right.X...) left the
// whole suite green, because no test manufactured the exact shape that corrupts - a result built
// straight from a slice of the original rows (handleGroupFailure's own size-1 and
// throttled/cancelled returns both are) that has spare capacity extending into a part of the
// backing array neither side's result actually names. rows[:1] (left's slice here) always has
// spare capacity through the rest of a larger backing array; appending onto it in place would
// write into backing[1], which belongs to neither left's nor right's result (right's own slice is
// backing[2:3]) - detectable only by reading the shared backing array again afterward, exactly
// what claimRenewal.rows does for as long as a bisection runs.
func TestMergeBisectResultsCopiesIntoFreshSlicesNotTheSharedBackingArray(t *testing.T) {
	t.Parallel()
	backing := make([]pendingRow, 4, 8)
	for i := range backing {
		backing[i] = pendingRow{kind: "issue", id: fmt.Sprintf("ALIAS-%d", i)}
	}
	left := bisectResult{permanent: backing[0:1]}
	right := bisectResult{permanent: backing[2:3]}

	merged := mergeBisectResults(left, right)

	if got := backing[1].id; got != "ALIAS-1" {
		t.Fatalf("backing[1].id = %q after merge, want unchanged %q - the merge wrote into memory that belongs to a row neither result named", got, "ALIAS-1")
	}
	gotIDs := make([]string, len(merged.permanent))
	for i, row := range merged.permanent {
		gotIDs[i] = row.id
	}
	wantIDs := []string{"ALIAS-0", "ALIAS-2"}
	if len(gotIDs) != len(wantIDs) || gotIDs[0] != wantIDs[0] || gotIDs[1] != wantIDs[1] {
		t.Errorf("merged.permanent ids = %v, want %v", gotIDs, wantIDs)
	}
}

// syncEmbedder is a fake embedder safe for Run's two loops to call at once: every call answers
// vectors unless err is set, and it counts the canary calls it sees.
type syncEmbedder struct {
	identity string
	mu       sync.Mutex
	err      error
	canaries int
}

func newSyncEmbedder(identity string) *syncEmbedder { return &syncEmbedder{identity: identity} }

func (e *syncEmbedder) Identity() string     { return e.identity }
func (e *syncEmbedder) Limits() embed.Limits { return new(embed.BedrockClient).Limits() }

func (e *syncEmbedder) Embed(_ context.Context, texts []string, _ embed.InputType) ([][]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, text := range texts {
		if text == embedCanary {
			e.canaries++
		}
	}
	if e.err != nil {
		return nil, e.err
	}
	return fakeVectors(len(texts)), nil
}

func (e *syncEmbedder) setErr(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.err = err
}

func (e *syncEmbedder) canaryCalls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.canaries
}

// runQueue runs Run with deps until the test ends, or until the stop it returns is called, and
// waits for Run to return once its context ends, bounded only by the test binary's deadline
// (testwait.Receive). A test that must observe the state Run leaves behind, rather than the state
// it keeps working on, stops it itself and reads afterwards; the cleanup stops it either way, and
// a second stop finds Run returned.
func runQueue(t *testing.T, deps Deps) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	return runQueueUntil(t, ctx, cancel, deps)
}

// runQueueUntil is runQueue on ctx, which cancel ends.
func runQueueUntil(t *testing.T, ctx context.Context, cancel context.CancelFunc, deps Deps) (stop func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(ctx, deps)
	}()
	stop = func() {
		t.Helper()
		cancel()
		testwait.Receive(t, "Run to return once its context ended", done)
	}
	t.Cleanup(stop)
	return stop
}

// scanLoop drives Run's scan loop on the queue's clock (Deps.Clock), a fake that moves only where
// the test moves it. The loop reports each wait it is about to make (Deps.scanWaiting,
// duty.WokenLoop.Waiting) and stays in the report until the test lets it on: the loop arms its one
// timer before it reports, and the test moves the fake only while the loop is in a report or at
// rest, never between the loop's reading of the clock and its arming, so the timer stands at
// exactly the moment reported, and the test moves the fake there (fire) or not (release) before
// the loop waits. A wait for a moment the fake has already reached is not reported: its timer
// fired as it was armed, as the boot pass's does, and a wake's once its gap is past. Once Run's
// context has ended a report holds nothing, so the loop goes on to its wait and returns from it as
// it does with no report set.
//
// enqueued is the loop's wake channel (Deps.enqueued), made here so that no relay stands between a
// signal and the loop. The loop takes a wake already waiting before it reports a rest, and while
// it is in that report nothing takes a signal from the channel: an empty channel then means every
// signal sent so far has had its pass, and a signal there is owed one once the loop is let on.
type scanLoop struct {
	fake     *clocktest.Fake
	enqueued chan struct{}
	waits    chan scanWait
	resume   chan struct{}
	// ended is closed once the context Run runs on has ended (start).
	ended <-chan struct{}
	// calls counts the loop's reports, the ones for a moment already reached among them.
	calls atomic.Int64
}

// scanWait is one wait the scan loop reported: at is the moment its timer is armed for, and armed
// is false for a rest, which holds no timer: nothing owed, nothing due and no wake waiting.
type scanWait struct {
	at    time.Time
	armed bool
}

// String is the wait as a failure names it.
func (w scanWait) String() string {
	if !w.armed {
		return "a rest"
	}
	return "its timer armed for " + w.at.String()
}

func newScanLoop(fake *clocktest.Fake) *scanLoop {
	return &scanLoop{fake: fake, enqueued: make(chan struct{}, 1), waits: make(chan scanWait), resume: make(chan struct{}, 1)}
}

// start runs Run with deps on the loop's fake, its report and its wake channel, on a context
// derived from ctx, as runQueue runs it, and returns its stop.
func (l *scanLoop) start(t *testing.T, ctx context.Context, deps Deps) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	l.ended = ctx.Done()
	deps.Clock, deps.scanWaiting, deps.enqueued = l.fake, l.waiting, l.enqueued
	return runQueueUntil(t, ctx, cancel, deps)
}

// waiting is the loop's report (Deps.scanWaiting), which runs on the loop's goroutine.
func (l *scanLoop) waiting(at time.Time, armed bool) {
	l.calls.Add(1)
	if armed && !at.After(l.fake.Now()) {
		return
	}
	select {
	case l.waits <- scanWait{at: at, armed: armed}:
	case <-l.ended:
		return
	}
	select {
	case <-l.resume:
	case <-l.ended:
	}
}

// next waits for the loop's next report, which holds the loop until release or fire.
func (l *scanLoop) next(t *testing.T, what string) scanWait {
	t.Helper()
	return testwait.Receive(t, what, l.waits)
}

// release lets the loop on from the report next returned, into its wait.
func (l *scanLoop) release() { l.resume <- struct{}{} }

// fire moves the fake to the moment the loop's timer is armed for, which fires it, and lets the
// loop on: the pass that timer starts begins at that moment on the queue's clock.
func (l *scanLoop) fire(wait scanWait) {
	l.fake.Set(wait.at)
	l.release()
}

// letOn lets the loop on from wait, a report it made: it fires the timer the wait armed (fire),
// or lets a rest on (release).
func (l *scanLoop) letOn(wait scanWait) {
	if wait.armed {
		l.fire(wait)
		return
	}
	l.release()
}

// wake signals the loop's wake channel as Cover and the relay do (Deps.signalEnqueued), without
// blocking: a signal already waiting there earns the same pass.
func (l *scanLoop) wake() {
	select {
	case l.enqueued <- struct{}{}:
	default:
	}
}

// rest lets the loop on through every wait it reports until it rests with no wake waiting, firing
// each timer it arms on the way, and returns once the loop is let on into that rest. Once nothing
// else can signal the loop - the coverage loop has parked, and no hub is connected or every
// notification has had its wake - that rest lasts until the test acts.
func (l *scanLoop) rest(t *testing.T, what string) {
	t.Helper()
	for {
		wait := l.next(t, what)
		if wait.armed {
			l.fire(wait)
			continue
		}
		owed := len(l.enqueued) > 0
		l.release()
		if !owed {
			return
		}
	}
}

// until waits until read holds, letting the loop on through each wait it reports meanwhile and
// firing each timer it arms: the gaps and due times a real clock passes by itself. It moves the
// fake to no other timer, so the coverage loop's recheck never comes by it.
func (l *scanLoop) until(t *testing.T, what string, read func() (string, bool)) {
	t.Helper()
	testwait.Until(t, what, func() (string, bool) {
		select {
		case wait := <-l.waits:
			l.letOn(wait)
		default:
		}
		return read()
	})
}

// requireRecheck fails t unless at, a moment armed on the queue's clock, is the coverage loop's
// next identity recheck after a pass that began at began with the clock standing still:
// identityRecheckInterval on, plus less than a tenth of it again. The tenth is duty.Jittered's
// draw, which the queue cannot fix, so the moment is held to that range and not to one value.
func requireRecheck(t *testing.T, at, began time.Time) {
	t.Helper()
	low, high := began.Add(identityRecheckInterval), began.Add(identityRecheckInterval+identityRecheckInterval/10)
	if at.Before(low) || !at.Before(high) {
		t.Fatalf("the coverage loop's recheck is armed for %v, want from %v to before %v: identityRecheckInterval (%v) after its pass began at %v, plus less than a tenth again",
			at, low, high, identityRecheckInterval, began)
	}
}

// armedRecheck is the one timer armed on fake once Run's scan loop rests and no call of Run's is
// in flight: the coverage loop's next identity recheck after the pass that began at began
// (requireRecheck).
func armedRecheck(t *testing.T, fake *clocktest.Fake, began time.Time) time.Time {
	t.Helper()
	armed := fake.Armed()
	if len(armed) != 1 {
		t.Fatalf("the queue's clock holds %d timers (%v), want the coverage loop's next recheck alone: the scan loop rests and no call is in flight", len(armed), armed)
	}
	requireRecheck(t, armed[0], began)
	return armed[0]
}

// requireNoTimer fails t if a timer is still armed on fake: once Run has returned, each of its
// loops has stopped its own.
func requireNoTimer(t *testing.T, fake *clocktest.Fake) {
	t.Helper()
	if armed := fake.Armed(); len(armed) != 0 {
		t.Errorf("Run returned with %d timers still armed on its clock (%v), want none: each loop stops its own as it returns", len(armed), armed)
	}
}

// What Run promises once its context ends: its loops wait on nothing that does not also end with
// it, so a statement in flight is cancelled and each loop returns from its wait at once. The three
// tests below hold each part of that promise as an order of events, and none as a span of time.
// Run runs on a fake clock (Deps.Clock) that the test never moves once the context has ended, so
// no timer of Run's own - the scan loop's gap or due time, the coverage loop's recheck, a pause, a
// debt wait, a call's bound - fires after the cancel: a Run that waited on one, or on anything
// else its context does not end, never returns, and the wait for its return (testwait.Receive)
// fails naming it. Once Run has returned its clock holds no timer. The first test has Run waiting,
// the second a scan pass inside an embed, the third a scan pass inside a statement.

// Run returns once its context ends from its loops' waits: the scan loop at rest and the coverage
// loop parked on its recheck, the state a running process spends its time in.
func TestRunReturnsWithinItsBoundOnceItsContextEnds(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	seedIssue(t, database, "STOP", "STOP-1", "Embedded at boot")
	fake := newQueueClock(t, database)
	booted := fake.Now()
	loop := newScanLoop(fake)
	parked := make(chan struct{})
	stop := loop.start(t, context.Background(), Deps{
		Store:          database,
		Embedder:       newSyncEmbedder(configuredIdentity),
		coverageParked: closeOnceSettled(parked),
	})
	testwait.Receive(t, "Run's coverage loop to report itself parked", parked)
	loop.rest(t, "Run's scan loop to rest once its boot drain is done")
	if got := pendingCount(t, database); got != 0 {
		t.Fatalf("pending once Run's scan loop rests = %d, want STOP-1 embedded at boot", got)
	}
	// The one wait of Run's that holds a timer: the coverage loop's, for its next recheck.
	armedRecheck(t, fake, booted)

	stop()
	requireNoTimer(t, fake)
}

// blockingEmbedder answers the canary at once and holds every other call until its context ends,
// signalling entered (once, without blocking) when a call starts holding: a scan pass inside its
// embed, the state a slow model service leaves a running process in.
type blockingEmbedder struct {
	bedrockShaped
	entered chan struct{}
}

func (e *blockingEmbedder) Embed(ctx context.Context, texts []string, _ embed.InputType) ([][]float32, error) {
	if len(texts) == 1 && texts[0] == embedCanary {
		return fakeVectors(1), nil
	}
	select {
	case e.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// Run returns once its context ends while a scan pass waits inside an embed that answers only when
// its context ends: the cancel reaches the call in flight. The call's own bound (callBound) is
// armed on the queue's clock, which never reaches it, so the cancel alone ends the call.
func TestRunReturnsWithinItsBoundWhenItsContextEndsDuringAnEmbed(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	seedIssue(t, database, "HOLD", "HOLD-1", "Embedded by a model service that does not answer")
	embedder := &blockingEmbedder{entered: make(chan struct{}, 1)}
	fake := newQueueClock(t, database)
	booted := fake.Now()
	stop := runQueue(t, Deps{Store: database, Embedder: embedder, Clock: fake})
	testwait.Receive(t, "a scan pass to hold inside its embed", embedder.entered)
	fake.AwaitArmedAt(t, "the bound of the call the scan pass holds inside", booted.Add(callBound))

	stop()
	requireNoTimer(t, fake)
}

// Run returns once its context ends while a scan pass waits inside a statement: the scan's claim
// queues behind a lock on embeddings that the test holds, and Run returns while the test still
// holds it, so the cancel is what ended the statement. A statement on a context of its own, or one
// the cancel does not reach, waits for the lock, and Run does not return.
func TestRunReturnsWhenItsContextEndsDuringAStatement(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()
	seedIssue(t, database, "LOCK", "LOCK-1", "Embedded at boot")
	fake := newQueueClock(t, database)
	loop := newScanLoop(fake)
	parked := make(chan struct{})
	stop := loop.start(t, ctx, Deps{
		Store:          database,
		Embedder:       newSyncEmbedder(configuredIdentity),
		coverageParked: closeOnceSettled(parked),
	})
	testwait.Receive(t, "Run's coverage loop to report itself parked", parked)
	loop.rest(t, "Run's scan loop to rest once its boot drain is done")

	hold, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction that holds embeddings: %v", err)
	}
	defer func() { _ = hold.Rollback(ctx) }()
	if _, err := hold.Exec(ctx, `lock table embeddings in access exclusive mode`); err != nil {
		t.Fatalf("lock embeddings: %v", err)
	}
	// The test's signal earns a scan, which the loop holds to its gap on the queue's clock; the
	// test moves the clock there, and the scan's claim, its first statement on embeddings, queues
	// behind the lock.
	loop.wake()
	wait := loop.next(t, "the scan loop's wait for the scan the test's signal earns")
	if !wait.armed {
		t.Fatalf("the scan loop rests after the test's signal, want the scan the signal earns armed")
	}
	loop.fire(wait)
	storetest.WaitForLockWaiters(t, hold, 1)

	stop()
	requireNoTimer(t, fake)
	// Run has returned, and the lock is still the test's: its transaction is open until here.
	if _, err := hold.Exec(ctx, `select 1`); err != nil {
		t.Fatalf("the transaction that holds embeddings ended before Run returned: %v", err)
	}
	if err := hold.Rollback(ctx); err != nil {
		t.Fatalf("let embeddings go: %v", err)
	}
}

// closeOnceSettled is a Deps.coverageParked that closes parked the first time the coverage loop
// settles with nothing owed, and passes over its wait on the embedder's first answer.
func closeOnceSettled(parked chan struct{}) func(awaitingAnswer bool) {
	settled := sync.OnceFunc(func() { close(parked) })
	return func(awaitingAnswer bool) {
		if !awaitingAnswer {
			settled()
		}
	}
}

// runHub runs hub until the test ends, and then waits for its Run to return through the shared
// wait (testwait.Receive), so a hub that never returns fails the test naming it.
func runHub(t *testing.T, hub *pglisten.Hub) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		hub.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		testwait.Receive(t, "the pglisten hub's Run to return once its context ended", done)
	})
}

// insertCommentWithWindow writes a comment on issueKey and the one window the block-index
// tick's window arm writes for it (refs.WriteItemWindows), which is what enqueues it now that
// the comment's own write-time trigger is gone.
func insertCommentWithWindow(t *testing.T, q querier, issueKey, body string) {
	t.Helper()
	var comment string
	if err := q.QueryRow(context.Background(), `
		insert into comments (issue_key, author, body) values ($1, '{"kind":"user","id":"alice"}', $2) returning id::text
	`, issueKey, body).Scan(&comment); err != nil {
		t.Fatalf("insert the comment: %v", err)
	}
	enqueueWindow(t, q, "comment_block", "comment/"+comment, "b1", body, "b1")
}

// Once Run has embedded what was pending at boot and its coverage pass has run, nothing
// changes, so Run makes no statement between one identity recheck and the next: no scan timer,
// no due time, no further pass. The absence is proved from what reaches the code, in turn, and a
// span of real time is held only for what none of that reaches.
//
// The state. The test waits for the two ends of Run's own work, which Run reports, rather than
// for a window in which nothing happened to be running. The coverage loop reports itself parked
// (Deps.coverageParked) with its pass done, nothing owed and its next recheck
// identityRecheckInterval away, so it will write no more rows and signal no more wakes; and the
// scan loop reports the state the woken shape exists for - nothing owed, nothing due, no timer
// armed, no wake waiting (duty.WokenLoop.Idle).
//
// An idle report made before the coverage loop's last write says nothing about the rows it
// wrote, and with nothing left to do the scan loop would never report again. So once the
// coverage loop has parked, the test signals the scan loop's own wake channel (Deps.enqueued,
// which scanLoop makes, so no relay stands between the signal and the loop) and takes the scan
// loop's next idle report while that report's call holds the loop. With the loop held nothing
// takes a signal from the channel, so an empty channel then means the test's signal, and any
// signal Cover left there before it, was taken by a pass that ended before this report; a signal
// still there arrived after the report's check, and the test lets the loop run on to the next
// report. Both loops run on the queue's clock (Deps.Clock), a fake here: the pass a signal earns
// is held to its gap on that clock, and the test moves the clock to it (scanLoop.fire). That move
// fires every timer due by then, so a timer Run armed on Deps.Clock beside its loops' own, a
// further scan pass due at the same gap among them, would run its work inside the move, before
// the test reads its counts. So each time the scan loop reports its timer armed, before the test
// moves the clock, the test reads every timer on that clock: the scan loop's and the coverage
// loop's next recheck, identityRecheckInterval after the boot pass began plus its jitter
// (requireRecheck), and no other. A third timer fails the test there, whatever its moment. With
// both reports in, that clock holds the recheck and no other timer (armedRecheck), so no timer
// Run armed after the test's last move is left either.
//
// A later action. The test moves the queue's clock to just before that recheck, the whole of the
// time Run would wait: no timer fires, no statement is made and neither loop reports again.
//
// The span. What neither reaches is code on no clock of the test: a timer of Run's that runs on
// the wall clock rather than on Deps.Clock, or work no state reports. Every statement Run makes
// is one on a Postgres pool, which no synctest bubble can hold. So with the queue's clock standing
// still the test holds 7 s of real time, retryInterval and 2 s more, so that a scan held to the
// loop's gap on the wall clock falls inside it, and reads the statements counted and the reports
// made before the move and after the span.
func TestRunMakesNoStatementOnceItsBootDrainAndPassAreDone(t *testing.T) {
	// Not parallel: it holds a span of real time, 7 s in which Run must make no statement
	// (storetest.RequireNoStatementsBecause below, which says what no state or later action reaches).
	database := storetest.Open(t)
	seedIssue(t, database, "IDLE", "IDLE-1", "Enqueued by its trigger")
	insertCommentWithWindow(t, database.Pool, "IDLE-1", "A comment its indexer windowed")
	fillCounts(t, database)
	counter := &storetest.StatementCounter{}
	counted := &store.Store{Pool: storetest.Traced(t, database.Pool, counter, nil)}
	fake := newQueueClock(t, database)
	booted := fake.Now()
	loop := newScanLoop(fake)
	parked := make(chan struct{})
	settle := closeOnceSettled(parked)
	var idleReports, parkReports atomic.Int64
	// Each idle report the test takes holds the scan loop until the test closes the report's
	// channel. Reports before the test asks for them, and after it has its answer, hold nothing.
	var asking atomic.Bool
	idles := make(chan chan struct{})
	answered := make(chan struct{})
	answer := sync.OnceFunc(func() { close(answered) })
	loop.start(t, context.Background(), Deps{
		Store:    counted,
		Embedder: newSyncEmbedder(configuredIdentity),
		coverageParked: func(awaitingAnswer bool) {
			parkReports.Add(1)
			settle(awaitingAnswer)
		},
		idle: func() {
			idleReports.Add(1)
			if !asking.Load() {
				return
			}
			release := make(chan struct{})
			select {
			case idles <- release:
				<-release
			case <-answered:
			}
		},
	})
	// Registered after the stop of Run, so it runs first: a test that fails while asking lets the
	// loop go before Run is stopped.
	t.Cleanup(answer)
	testwait.Receive(t, "Run's coverage loop to report itself parked", parked)
	asking.Store(true)
	loop.wake()
	settled := false
	testwait.Until(t, "Run's scan loop to report itself idle with the test's signal taken (the signals waiting in its wake channel)", func() (int, bool) {
		select {
		case release := <-idles:
			if settled = len(loop.enqueued) == 0; settled {
				answer()
			}
			close(release)
		case wait := <-loop.waits:
			// The pass a signal earns is held to its gap on the queue's clock, which the test moves
			// there; a rest the loop reported before it took the signal is let on to take it. Before
			// the move the clock holds the scan loop's timer and the recheck, and no other.
			if wait.armed {
				armed := fake.Armed()
				if len(armed) != 2 || !armed[0].Equal(wait.at) {
					t.Fatalf("the queue's clock holds %d timers (%v) while Run's scan loop waits with %v, want that timer and the coverage loop's next recheck after it, and no other: the coverage loop is parked and the scan loop holds in its report, so no pass or call of Run's is in flight to arm one", len(armed), armed, wait)
				}
				requireRecheck(t, armed[1], booted)
			}
			loop.letOn(wait)
		default:
		}
		return len(loop.enqueued), settled
	})
	// From its idle report the loop goes on to its rest, which it reports too: no wake waits there
	// either, and it holds no timer.
	if rest := loop.next(t, "Run's scan loop to rest after its idle report"); rest.armed {
		t.Fatalf("after its idle report Run's scan loop armed its timer for %v, want a rest", rest.at)
	}
	loop.release()
	items, current, _, _, _ := coverage(t, database, configuredIdentity)
	if items != 2 || current != 2 {
		t.Fatalf("coverage = %d items, %d current; want both items embedded once Run is idle", items, current)
	}
	recheck := armedRecheck(t, fake, booted)

	reports := func() [3]int64 { return [3]int64{loop.calls.Load(), idleReports.Load(), parkReports.Load()} }
	statements, reported := counter.Count(), reports()
	fake.Set(recheck.Add(-time.Nanosecond))
	storetest.RequireNoStatementsBecause(t, counter, 7*time.Second, "Run with nothing pending and nothing changing",
		"the reports and the queue's clock account for every timer of Run's on Deps.Clock: the coverage loop parked, the scan loop at rest with no wake waiting, the recheck the one timer armed, and that clock moved to just before it. Neither that state nor that later action reaches a statement from code on no clock of the test, a timer of Run's on the wall clock rather than Deps.Clock or work no state reports, and synctest.Wait cannot hold Run's real Postgres connections; 7 s is retryInterval and 2 s more, so a scan held to the loop's gap on the wall clock falls inside it")
	if made := counter.Count() - statements; made != 0 {
		t.Errorf("Run made %d statements once its clock moved to just before the recheck (%v), want none", made, recheck)
	}
	if now := reports(); now != reported {
		t.Errorf("Run's loops reported again once its clock moved to just before the recheck: the scan loop's waits, its idle reports and the coverage loop's parks went from %v to %v, want no new report", reported, now)
	}
}

// A write's notification wakes the idle queue, through a real pglisten.Hub on WakeChannel: the
// window of a comment written once the queue has drained, and once the scan the hub's catch-up
// signal after its LISTEN earned has run, is embedded by the scan its notification earns, and
// nothing else would have scanned for it. embeddings_enqueue_window announces on the same channel
// as embeddings_enqueue, so a window wakes the queue exactly as a whole-text row did.
//
// The queue runs on a fake clock (Deps.Clock) and reports each wait of its scan loop (scanLoop),
// so the test reads the order of events and the moment of each scan, and waits out no span. The
// hub starts only once the queue rests, so the catch-up signal is the first wake after that rest,
// and the scan it earns is armed retryInterval after the boot pass began. After that scan the loop
// rests again, and only a wake ends a rest: the comment's notification. The scan it earns is armed
// at the later of the write's moment and retryInterval after the last pass began
// (duty.WokenLoop's gap), the comment is still pending while the loop waits for that moment, and
// the pass that timer starts embeds it. A wake that is lost leaves the loop at rest, and the wait
// for its report fails naming it.
func TestAWriteIsEmbeddedOnItsNotification(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	seedIssue(t, database, "NTFY", "NTFY-1", "Written before the queue started")
	hub := &pglisten.Hub{Owner: "embedqueue test", Dial: database.Pool.Listener}
	wake := hub.Wake(WakeChannel)
	connected := make(chan struct{})
	hub.OnConnected(sync.OnceFunc(func() { close(connected) }))
	fake := newQueueClock(t, database)
	loop := newScanLoop(fake)
	parked := make(chan struct{})
	loop.start(t, context.Background(), Deps{
		Store:          database,
		Embedder:       newSyncEmbedder(configuredIdentity),
		Wake:           wake,
		coverageParked: closeOnceSettled(parked),
	})
	testwait.Receive(t, "Run's coverage loop to report itself parked", parked)
	loop.rest(t, "Run's scan loop to rest once its boot drain is done")
	if got := pendingCount(t, database); got != 0 {
		t.Fatalf("pending once the scan loop rests = %d, want NTFY-1 embedded at boot", got)
	}

	// A pass begins at the moment the queue's clock stands at, and the clock has not moved since
	// the last one began.
	lastPass := fake.Now()
	runHub(t, hub)
	testwait.Receive(t, "the hub to connect and listen", connected)
	catchUp := loop.next(t, "the scan loop's wait for the scan the hub's catch-up signal earns")
	if want := lastPass.Add(retryInterval); !catchUp.armed || !catchUp.at.Equal(want) {
		t.Fatalf("after the hub's catch-up signal the scan loop reports %v, want its timer armed for %v, retryInterval after its last pass began", catchUp, want)
	}
	loop.fire(catchUp)
	loop.rest(t, "Run's scan loop to rest once the scan the catch-up signal earned has run")
	lastPass = fake.Now()

	insertCommentWithWindow(t, database.Pool, "NTFY-1", "Written while the queue is idle")
	if got := pendingCount(t, database); got != 1 {
		t.Fatalf("pending after the comment = %d, want 1", got)
	}
	written := fake.Now()
	notified := loop.next(t, "the scan loop's wait for the scan the comment's notification earns")
	want := lastPass.Add(retryInterval)
	if written.After(want) {
		want = written
	}
	if !notified.armed || !notified.at.Equal(want) {
		t.Fatalf("after the comment's notification the scan loop reports %v, want its timer armed for %v: the later of the write's moment (%v) and retryInterval after its last pass began (%v)",
			notified, want, written, lastPass)
	}
	if got := pendingCount(t, database); got != 1 {
		t.Fatalf("pending while the scan the notification earned waits for its moment = %d, want the comment still pending: nothing else scans for it", got)
	}
	loop.fire(notified)
	// The loop reports its next wait only once the pass that timer started has ended.
	loop.next(t, "the scan loop's next wait, once the scan the notification earned has run")
	if got := pendingCount(t, database); got != 0 {
		t.Errorf("pending once the scan the comment's notification earned has run = %d, want the comment embedded by it", got)
	}
	loop.release()
}

// A claim another process left on a row - it crashed before embedding it - is taken once the
// claim lapses, by the scan the due time the queue read after its last scan arms: no write, no
// wake and no ticker brings it.
//
// The claim is staged stagedAhead past the database's clock, so it holds there however long the
// test takes, and the queue's clock (Deps.Clock), a fake, starts retryInterval and a second short
// of it: the lapse lies past the floor a scan pass holds its due time to, so the boot pass answers
// the lapse itself, the due path this test is for. The scan loop then reports its timer armed for
// exactly the lapse (scanLoop), with the row still pending. The test brings the lapse on both
// clocks, the claim's stamp set to the database's now() and the queue's clock moved to the armed
// moment, and the pass that timer starts embeds the row. A loop that drops the due time reports a
// rest, one that arms another moment reports that moment, and a scan that takes the row before its
// lapse leaves nothing pending at the report.
func TestAClaimLeftByAnotherProcessIsTakenWhenItLapses(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()
	seedIssue(t, database, "CLAM", "CLAM-1", "Claimed by a process that crashed")
	claimed, _, err := scanPending(ctx, Deps{Store: storetest.SecondProcess(t, database), Embedder: newPlaceEmbedder(configuredIdentity)})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("the other process's claim = %v, %v; want CLAM-1", claimed, err)
	}
	var lapses time.Time
	if err := database.Pool.QueryRow(ctx, `
		update embeddings set next_attempt_at = now() + make_interval(secs => $1) where id = 'CLAM-1'
		returning next_attempt_at`, stagedAhead.Seconds()).Scan(&lapses); err != nil {
		t.Fatalf("stage the claim's lapse: %v", err)
	}
	// As if most of claimWindow had already passed since that claim.
	const lapsesIn = retryInterval + time.Second
	fake := clocktest.New(lapses.Add(-lapsesIn))
	loop := newScanLoop(fake)
	loop.start(t, ctx, Deps{Store: database, Embedder: newSyncEmbedder(configuredIdentity)})

	due := loop.next(t, "the scan loop's wait after its boot pass, which found CLAM-1 claimed")
	if !due.armed || !due.at.Equal(lapses) {
		t.Fatalf("after its boot pass the scan loop reports %v, want its timer armed for %v, the lapse of CLAM-1's claim, which the pass read as its due time", due, lapses)
	}
	fake.AwaitArmedAt(t, "the scan loop's timer for the lapse of CLAM-1's claim", lapses)
	if got := pendingCount(t, database); got != 1 {
		t.Fatalf("pending before the claim lapsed = %d, want CLAM-1 left to the process that claimed it", got)
	}
	mustExec(t, database.Pool, `update embeddings set next_attempt_at = now() where id = 'CLAM-1'`)
	loop.fire(due)
	if rest := loop.next(t, "the scan loop's wait after the pass its timer started at the lapse"); rest.armed {
		t.Fatalf("after the pass at the lapse the scan loop armed its timer for %v, want a rest: CLAM-1 embedded and nothing pending", rest.at)
	}
	if got := pendingCount(t, database); got != 0 {
		t.Errorf("pending after the pass the lapse's timer started = %d, want CLAM-1 embedded by it", got)
	}
	loop.release()
}

// scanPass returns no due time once nothing is pending; the lapse of another process's claim when
// one is; and never a time sooner than retryInterval from its end - not for a claim lapsing
// sooner, and not for a row already due that another process holds under skip locked, which the
// scan could not claim and an immediate rescan would not either. The floor is on the queue's
// clock (Deps.Clock), a fake the test sets before each pass, so the floor is exact.
//
// The other process's claim is put stagedAhead past the database's clock, so it holds however long
// the passes take to read it: claimWindow's two minutes, and a second, could pass before a pass
// and leave the row due, claimed by this scan. "Lapsing in a second" is then read on the queue's
// clock, which the test sets a second short of the lapse.
func TestScanPassReturnsWhenThePendingRowsFallDue(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()
	seedIssue(t, database, "DUE", "DUE-1", "Drained at once")
	fake := newQueueClock(t, database)
	deps := Deps{Store: database, Embedder: newPlaceEmbedder(configuredIdentity), Clock: fake}
	if next := scanPass(ctx, deps); !next.IsZero() {
		t.Errorf("after draining every row: due %v, want none", next)
	}

	seedIssue(t, database, "DUE", "DUE-2", "Claimed elsewhere")
	if claimed, _, err := scanPending(ctx, Deps{Store: storetest.SecondProcess(t, database), Embedder: newPlaceEmbedder(configuredIdentity)}); err != nil || len(claimed) != 1 {
		t.Fatalf("the other process's claim = %v, %v; want DUE-2", claimed, err)
	}
	// The other process's claim, held stagedAhead past the database's clock.
	var lapses time.Time
	if err := database.Pool.QueryRow(ctx, `
		update embeddings set next_attempt_at = now() + make_interval(secs => $1) where id = 'DUE-2' returning next_attempt_at
	`, stagedAhead.Seconds()).Scan(&lapses); err != nil {
		t.Fatalf("hold the claim stagedAhead on: %v", err)
	}
	fake.Set(databaseNow(t, database))
	if next := scanPass(ctx, deps); !next.Equal(lapses) {
		t.Errorf("with DUE-2 claimed elsewhere: due %v, want the claim's lapse %v", next, lapses)
	}

	requireFloor := func(when string, at time.Time) {
		t.Helper()
		fake.Set(at)
		floor := fake.Now().Add(retryInterval)
		if next := scanPass(ctx, deps); !next.Equal(floor) {
			t.Errorf("%s: due %v, want %v, retryInterval after the pass on the queue's clock", when, next, floor)
		}
	}
	requireFloor("a claim lapsing in a second", lapses.Add(-time.Second))

	mustExec(t, database.Pool, `update embeddings set next_attempt_at = now() - interval '1 second' where id = 'DUE-2'`)
	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holding transaction: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, `select 1 from embeddings where id = 'DUE-2' for update`); err != nil {
		t.Fatalf("hold DUE-2: %v", err)
	}
	requireFloor("a due row another process holds", databaseNow(t, database))
	if got := pendingCount(t, database); got != 1 {
		t.Errorf("pending = %d, want DUE-2 still pending: the scan skips a held row", got)
	}
}

// A throttled batch waits retryInterval plus its backoff before the next one, so a throttle
// episode never sends calls more often than the scan ticker did. The scan runs on the queue's
// clock (Deps.Clock), a fake that never moves by itself, so the pause is read as exactly the
// deadline it arms, and no second call can come until the test moves the clock there, which it
// never does: the first backoff is retry.Delay(1) plus up to half of it again (batchBackoff).
func TestAThrottledScanWaitsAtLeastRetryIntervalBeforeItsNextBatch(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	seedIssue(t, database, "THSC", "THSC-1", "A title")
	embedder := &fakeEmbedder{err: embedtest.FakeThrottleError{Code: "ThrottlingException"}}
	fake := newQueueClock(t, database)
	start := fake.Now()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		scan(ctx, Deps{Store: database, Embedder: embedder, Clock: fake})
	}()
	// The call's own bound (embed.Bounded) is armed on the same clock while the call is out, at
	// callBound; the pause is the other deadline.
	isPause := func(at time.Time) bool { return !at.Equal(start.Add(callBound)) }
	paused := testwait.Until(t, "the scan's pause after its throttled batch, armed on the queue's clock", func() ([]time.Time, bool) {
		armed := fake.Armed()
		return armed, slices.ContainsFunc(armed, isPause)
	})
	pause := paused[slices.IndexFunc(paused, isPause)].Sub(start)
	if low, high := retryInterval+retry.Delay(1), retryInterval+retry.Delay(1)+retry.Delay(1)/2; pause < low || pause > high {
		t.Errorf("the scan pauses %v after its first throttled batch, want retryInterval (%v) plus its backoff, %v to %v", pause, retryInterval, low, high)
	}
	cancel()
	testwait.Receive(t, "the scan to return once its context ended", scanned)
	if len(embedder.calls) != 1 {
		t.Errorf("%d embed calls while the scan held its pause, want 1", len(embedder.calls))
	}
}

// embeddings_enqueue announces on WakeChannel when its upsert writes a row - a new item, or a
// changed text - and not when the text is the one the row already holds. A notification carries
// no payload of its own (migration 0151), so the one that must not come is proved by one the test
// sends after it: the listener reads notifications in commit order, so a repeat write that
// announced itself would be read before the test's marker.
func TestEnqueueNotifiesOnlyWhenItWritesARow(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()
	listener, err := database.Pool.Listener(ctx)
	if err != nil {
		t.Fatalf("open a listening connection: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close(context.Background()) })
	if _, err := listener.Exec(ctx, "listen "+WakeChannel); err != nil {
		t.Fatalf("listen: %v", err)
	}
	nextPayload := func() string {
		t.Helper()
		notification, err := listener.WaitForNotification(testwait.Context(t))
		if err != nil {
			t.Fatalf("wait for a notification: %v", err)
		}
		if notification.Channel != WakeChannel {
			t.Fatalf("notification on %q, want %q", notification.Channel, WakeChannel)
		}
		return notification.Payload
	}

	mustExec(t, database.Pool, `select embeddings_enqueue('issue', 'NOTE-1', $1)`, "First text")
	if payload := nextPayload(); payload != "" {
		t.Errorf("the first write's notification carries %q, want the enqueue's own empty payload", payload)
	}
	mustExec(t, database.Pool, `select embeddings_enqueue('issue', 'NOTE-1', $1)`, "First text")
	mustExec(t, database.Pool, `select pg_notify($1, 'marker')`, WakeChannel)
	if payload := nextPayload(); payload != "marker" {
		t.Errorf("the notification read after the repeat write carries %q, want the test's marker: a write of the text the row already holds writes no row and announces nothing", payload)
	}
	mustExec(t, database.Pool, `select embeddings_enqueue('issue', 'NOTE-1', $1)`, "Changed text")
	if payload := nextPayload(); payload != "" {
		t.Errorf("the changed text's notification carries %q, want the enqueue's own empty payload", payload)
	}
}
