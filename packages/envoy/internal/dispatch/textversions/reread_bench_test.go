package textversions

import (
	"context"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/textversions/textversionstest"
)

// The sizes of BenchmarkTheRulesReRead's log, which its environment can set: the events of the
// log (KEYWORD_REREAD_EVENTS, production's count by default), the posts of each kind that are
// edited and deleted (KEYWORD_REREAD_POSTS), the documents holding a deleted comment's pending
// insert (KEYWORD_REREAD_INSERTS), and the pending inserts' comments on one of them a pass may
// delete while it runs (KEYWORD_REREAD_DELETES).
const (
	rereadEventsEnv  = "KEYWORD_REREAD_EVENTS"
	rereadPostsEnv   = "KEYWORD_REREAD_POSTS"
	rereadInsertsEnv = "KEYWORD_REREAD_INSERTS"
	rereadDeletesEnv = "KEYWORD_REREAD_DELETES"
)

// rereadBenchIssues is how many issues the log's events are spread over.
const rereadBenchIssues = 1000

// BenchmarkTheRulesReRead measures one re-read pass of this build over a log a fill of rules
// version 1 wrote, with the cursor at its newest event and rules 1 stored: every batch it takes
// (filler.batch, each a re-read batch, rereadBatch) up to the one that raises the version.
//
// The log is seeded in SQL: KEYWORD_REREAD_EVENTS events (900,000 by default, about production's
// count) of types that change no searchable text, each with a payload of about 1.5 KB of
// incompressible text, over 1,000 issues; then KEYWORD_REREAD_POSTS comments and as many issue
// messages (100 each by default), each created, edited and deleted, one in ten with a body an
// event cuts (about 35 KB), with their body history, so 400 events of the types version 2 added
// or that change a post's text and their posts' creates; and KEYWORD_REREAD_INSERTS issues (10 by
// default) whose primary document's version 2 holds a deleted comment's pending insert, the
// first of them with KEYWORD_REREAD_DELETES more pending comments on that insert (200 by
// default). The keyword table holds the rows a fill of rules version 1 wrote of them: the creates
// and the comments' edits, each cut body read from its deleted post's row, and each version 2's
// issue row from its stored markdown.
//
// Each iteration puts the table, the cursor row and the pending comments back as the seed left
// them, untimed, and times the pass, while on another connection those comments are deleted one
// after another as this build's delete does under the lock of their issue's owner, whose rows the
// pass's last batch rewrites (deletePendingInsert). It reports, beside the wall time of a pass
// (ns/op), the time the database took to answer its statements as the client measured each
// (db-ms/op), the part of that its reads of the log took (log-read-ms/op), how long its batches
// held owner locks, from the answer to the statement that took them to the answer to the commit
// that released them (owner-lock-held-ms/op), the longest any one delete waited for its owner's
// lock over every pass (delete-wait-max-ms/op) and the deletes a pass ran beside it (deletes/op),
// its batches (batches/op) and the rows it wrote again (rows/op, redone/op, masked/op). It logs,
// once, the plan of the re-read's read of the log on the seeded fixture, run with ANALYZE and
// BUFFERS, which `go test` prints whole with -v alone (it cuts a benchmark's log at ten lines
// otherwise). Each round of the benchmark seeds its own log, so -benchtime Nx seeds twice, once
// for its first round of one. Run it in a build Job:
//
//	DISPATCH_TEST_DATABASE_URL=... go test ./internal/dispatch/textversions -run '^$' -bench BenchmarkTheRulesReRead -benchtime 3x -v -timeout 2h
//
// and to check it works on a small log: KEYWORD_REREAD_EVENTS=2000 KEYWORD_REREAD_POSTS=5
// KEYWORD_REREAD_INSERTS=2 with -benchtime 1x.
func BenchmarkTheRulesReRead(b *testing.B) {
	eventCount := benchSize(b, rereadEventsEnv, 900_000)
	postCount := benchSize(b, rereadPostsEnv, 100)
	insertCount := benchSize(b, rereadInsertsEnv, 10)
	deleteCount := benchSize(b, rereadDeletesEnv, 200)
	database := storetest.Open(b)
	textversionstest.SeedProject(b, database)
	ctx := context.Background()
	seedStarted := time.Now()
	newest, wantAgain := seedRereadLog(b, database, eventCount, postCount, insertCount)
	contended := textversionstest.Project + "-1"
	var document string
	var pending []string
	if insertCount > 0 {
		document, pending = seedPendingInserts(b, database, contended, deleteCount)
	}
	b.Logf("seeded %d events, %d posts of each kind, %d documents holding a deleted insert and %d pending inserts' comments in %s",
		newest, postCount, insertCount, len(pending), time.Since(seedStarted).Round(time.Millisecond))
	if _, err := database.Pool.Exec(ctx, `
		create table keyword_reread_seed as select * from search_text_versions;
		analyze events; analyze search_text_versions; analyze comments; analyze messages; analyze artifact_versions;
		analyze post_body_history`); err != nil {
		b.Fatalf("keep the seeded keyword rows: %v", err)
	}
	rereadPlanLogged.Do(func() { logRereadPlan(b, database) })
	tracer := &statementTimer{}
	traced := storetest.TracedStore(b, database, tracer)

	var batches, rows, redone, masked, deletes int
	var deleteWait time.Duration
	b.ResetTimer()
	for range b.N {
		b.StopTimer()
		if _, err := database.Pool.Exec(ctx, `
			delete from search_text_versions;
			insert into search_text_versions select * from keyword_reread_seed;
			update search_text_versions_fill set rules = 1 where id = 1`); err != nil {
			b.Fatalf("put the seeded keyword rows back: %v", err)
		}
		for _, statement := range []string{
			`update comments set body = 'Add this word.', deleted_at = null, deleted_by = null where id = any($1::text[]::uuid[])`,
			`delete from post_body_history where comment_id = any($1::text[]::uuid[])`,
		} {
			if _, err := database.Pool.Exec(ctx, statement, pending); err != nil {
				b.Fatalf("put the pending inserts' comments back: %v", err)
			}
		}
		fill := newFiller(traced.Pool, nil)
		deleter := startDeleter(database, contended, document, pending)
		b.StartTimer()
		for {
			result, err := fill.batch(ctx, newest)
			if err != nil {
				b.Fatalf("a re-read batch: %v", err)
			}
			if !result.reread {
				b.Fatalf("batch %d of the pass is no re-read batch", batches+1)
			}
			batches++
			rows, redone, masked = rows+result.rows, redone+result.redone, masked+result.masked
			if result.raised {
				break
			}
		}
		b.StopTimer()
		deleted := deleter.finish()
		if deleted.err != nil {
			b.Fatalf("a delete beside the pass: %v", deleted.err)
		}
		deletes, deleteWait = deletes+deleted.count, max(deleteWait, deleted.maxWait)
		var stored, again int
		if err := database.Pool.QueryRow(ctx, `
			select (select rules from search_text_versions_fill where id = 1),
			       (select count(*) from search_text_versions v join events e on e.id = v.event_id
			         where e.type in ('comment.deleted', 'message.edited', 'message.deleted'))`).Scan(&stored, &again); err != nil {
			b.Fatalf("read what the pass wrote: %v", err)
		}
		if stored != rulesVersion || again != wantAgain {
			b.Fatalf("after the pass the cursor row holds rules %d and the table %d rows of the types version 2 added; want %d and %d",
				stored, again, rulesVersion, wantAgain)
		}
		b.StartTimer()
	}
	b.StopTimer()
	n := float64(b.N)
	statements, logReads, lockHeld := tracer.totals()
	b.ReportMetric(float64(statements.Milliseconds())/n, "db-ms/op")
	b.ReportMetric(float64(logReads.Milliseconds())/n, "log-read-ms/op")
	b.ReportMetric(float64(lockHeld.Milliseconds())/n, "owner-lock-held-ms/op")
	b.ReportMetric(float64(deleteWait.Microseconds())/1000, "delete-wait-max-ms/op")
	b.ReportMetric(float64(deletes)/n, "deletes/op")
	b.ReportMetric(float64(batches)/n, "batches/op")
	b.ReportMetric(float64(rows)/n, "rows/op")
	b.ReportMetric(float64(redone)/n, "redone/op")
	b.ReportMetric(float64(masked)/n, "masked/op")
}

// pendingDeleter deletes, one after another on its own connection, the pending inserts' comments
// it was given, until it is finished or has deleted them all (startDeleter).
type pendingDeleter struct {
	stop chan struct{}
	done chan deletesRun
}

// deletesRun is what a pendingDeleter did: how many comments it deleted, the longest any one of
// those deletes waited for its owner's lock, and the error that stopped it, if one did.
type deletesRun struct {
	count   int
	maxWait time.Duration
	err     error
}

// startDeleter starts deleting comments, pending inserts' comments on issue's primary document,
// one after another (deletePendingInsert).
func startDeleter(database *store.Store, issue, document string, comments []string) *pendingDeleter {
	deleter := &pendingDeleter{stop: make(chan struct{}), done: make(chan deletesRun, 1)}
	go func() {
		var run deletesRun
		defer func() { deleter.done <- run }()
		for _, comment := range comments {
			select {
			case <-deleter.stop:
				return
			default:
			}
			wait, err := deletePendingInsert(context.Background(), database, issue, document, comment)
			if err != nil {
				run.err = err
				return
			}
			run.count, run.maxWait = run.count+1, max(run.maxWait, wait)
		}
	}()
	return deleter
}

// finish stops the deleter after the delete it is running and answers what it did.
func (d *pendingDeleter) finish() deletesRun {
	close(d.stop)
	return <-d.done
}

// deletePendingInsert deletes comment, anchored on the insert of issue's primary document, with
// the locks and the writes of this build's delete (api's deleteComment): in one READ COMMITTED
// transaction, the lock of the issue's owner (store.LockOwner), then the versions holding the
// insert's words masked as they read once it commits (docs.MaskedVersionsOnDelete), the body it
// clears kept in post_body_history, the comment cleared and marked deleted, and the keyword rows
// of those versions rewritten (store.MaskTextVersions, the statement QueueTextVersionsMask
// queues). It appends no event: the pass reads the log up to its cursor. It answers how long the
// lock took.
func deletePendingInsert(ctx context.Context, database *store.Store, issue, document, comment string) (time.Duration, error) {
	tx, err := database.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("begin the delete of comment %s: %w", comment, err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	asked := time.Now()
	if err := store.LockOwner(ctx, tx, store.IssueOwner(issue)); err != nil {
		return 0, fmt.Errorf("lock %s for the delete of comment %s: %w", issue, comment, err)
	}
	wait := time.Since(asked)
	numbers, texts, err := docs.MaskedVersionsOnDelete(ctx, tx, document, insertMark, comment)
	if err != nil {
		return 0, err
	}
	for _, statement := range []string{
		`insert into post_body_history (comment_id, body, edited_by)
		 select id, body, '{"kind":"user","id":"alice"}' from comments where id = $1::uuid`,
		`update comments set body = '', deleted_at = now(), deleted_by = '{"kind":"user","id":"alice"}' where id = $1::uuid`,
	} {
		if _, err := tx.Exec(ctx, statement, comment); err != nil {
			return 0, fmt.Errorf("delete comment %s: %w", comment, err)
		}
	}
	if _, err := store.MaskTextVersions(ctx, tx, document, numbers, texts); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit the delete of comment %s: %w", comment, err)
	}
	return wait, nil
}

// benchSize reads the size the environment variable name sets, else fallback.
func benchSize(b *testing.B, name string, fallback int) int {
	b.Helper()
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	size, err := strconv.Atoi(raw)
	if err != nil || size < 0 {
		b.Fatalf("%s=%q is not a count", name, raw)
	}
	return size
}

// rereadPlanLogged logs the plan of the re-read's read of the log once in a process, whatever
// the rounds the benchmark runs.
var rereadPlanLogged sync.Once

// logRereadPlan logs the plan of the re-read's first read of the log on the seeded fixture
// (rereadEventsQuery, with the arguments a re-read batch gives it), with ANALYZE and BUFFERS.
func logRereadPlan(b *testing.B, database *store.Store) {
	b.Helper()
	changes := currentRules.changeTypes(rereadTypes(1, rulesVersion))
	rows, err := database.Pool.Query(context.Background(), `explain (analyze, buffers) `+rereadEventsQuery,
		int64(0), int64(math.MaxInt64), batchEvents, int64(batchBytes), changes, []string(nil))
	if err != nil {
		b.Fatalf("explain the re-read's read of the log: %v", err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		b.Fatalf("explain the re-read's read of the log: %v", err)
	}
	b.Logf("the plan of the re-read's read of the log:\n%s", strings.Join(lines, "\n"))
}

// statementTimer sums the time each statement of the pool it traces took, from the client's send
// to its answer: every statement's, and the re-read's reads of the log's (rereadEventsQuery); and
// the time from the answer to each statement that took owner locks (store.OwnerLockStatement,
// which LockOwners runs) to the answer to the commit or rollback that ended its transaction,
// which released them. Only the passes' batches run on that pool; the benchmark's own statements
// and its deletes run on another.
type statementTimer struct {
	mu                      sync.Mutex
	all, readLogs, lockHeld time.Duration
	open                    map[*pgx.Conn]statementStart
	locked                  map[*pgx.Conn]time.Time
}

// statementStart is a statement in flight on one connection: when it was sent, and whether it is
// a read of the log, the statement that takes owner locks, or the end of a transaction.
type statementStart struct {
	at                  time.Time
	readLog, lock, ends bool
}

func (s *statementTimer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open == nil {
		s.open, s.locked = map[*pgx.Conn]statementStart{}, map[*pgx.Conn]time.Time{}
	}
	statement := strings.TrimSpace(data.SQL)
	s.open[conn] = statementStart{
		at:      time.Now(),
		readLog: data.SQL == rereadEventsQuery,
		lock:    data.SQL == store.OwnerLockStatement,
		ends:    statement == "commit" || statement == "rollback",
	}
	return ctx
}

func (s *statementTimer) TraceQueryEnd(_ context.Context, conn *pgx.Conn, _ pgx.TraceQueryEndData) {
	s.mu.Lock()
	defer s.mu.Unlock()
	start, ok := s.open[conn]
	if !ok {
		return
	}
	delete(s.open, conn)
	now := time.Now()
	took := now.Sub(start.at)
	s.all += took
	switch {
	case start.readLog:
		s.readLogs += took
	case start.lock:
		s.locked[conn] = now
	case start.ends:
		if at, held := s.locked[conn]; held {
			s.lockHeld += now.Sub(at)
			delete(s.locked, conn)
		}
	}
}

// totals is the time of every statement traced, of the reads of the log among them, and the time
// owner locks were held.
func (s *statementTimer) totals() (time.Duration, time.Duration, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.all, s.readLogs, s.lockHeld
}

// seedRereadLog writes BenchmarkTheRulesReRead's log and the rows a fill of rules version 1 wrote
// of it, with the cursor row at its newest event and rules 1, and answers the newest event and
// how many rows a re-read writes of the types version 2 added.
func seedRereadLog(b *testing.B, database *store.Store, eventCount, postCount, insertCount int) (int64, int) {
	b.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		select $1 || '-' || n, $1, n, 'Rotate the signing keys of region ' || n, '{"kind":"user","id":"alice"}', 'A'
		  from generate_series(1, $2::integer) n`, textversionstest.Project, rereadBenchIssues); err != nil {
		b.Fatalf("seed the issues: %v", err)
	}
	// The events of the types that change no searchable text: about 1.5 KB of md5 digests each,
	// which no compression shrinks, as a payload of production's carries prose.
	if _, err := database.Pool.Exec(ctx, `
		insert into events (issue_key, seq, type, actor, payload, notify)
		select $1 || '-' || ((g - 1) % $3 + 1), (g - 1) / $3 + 1,
		       (array['issue.claimed', 'comment.resolved', 'message.delivery', 'comment.delivery', 'user_thread_state.updated'])[g % 5 + 1],
		       '{"kind":"user","id":"alice"}',
		       jsonb_build_object('id', md5(g::text)::uuid::text, 'issue_key', $1 || '-' || ((g - 1) % $3 + 1),
		         'note', (select string_agg(md5(g::text || ':' || part::text), '') from generate_series(1, 47) part)),
		       false
		  from generate_series(1, $2::integer) g`, textversionstest.Project, eventCount, rereadBenchIssues); err != nil {
		b.Fatalf("seed the events: %v", err)
	}
	// next is the seq the next event of issue takes, after the seeded ones and those appended since.
	seq := map[string]int32{}
	next := func(issue string) int32 {
		if _, seen := seq[issue]; !seen {
			var last int32
			if err := database.Pool.QueryRow(ctx, `select coalesce(max(seq), 0) from events where issue_key = $1`, issue).Scan(&last); err != nil {
				b.Fatalf("read the seqs of %s: %v", issue, err)
			}
			seq[issue] = last
		}
		seq[issue]++
		return seq[issue]
	}
	long := longCommentBody()
	actor := model.Actor{Kind: "user", ID: "alice"}
	appendEvent := func(tx pgx.Tx, issue, eventType string, payload map[string]any) int64 {
		var id int64
		if err := tx.QueryRow(ctx, `
			insert into events (issue_key, seq, type, actor, payload, notify) values ($1, $2, $3, $4, $5, false) returning id
		`, issue, next(issue), eventType, actor, payload).Scan(&id); err != nil {
			b.Fatalf("append %s on %s: %v", eventType, issue, err)
		}
		return id
	}
	postPayload := func(id, issue, body string) map[string]any {
		carried, truncated := model.EventBody(body)
		payload := map[string]any{"id": id, "issue_key": issue, "body": carried}
		if truncated {
			payload["body_truncated"] = true
		}
		return payload
	}
	again := 0
	for index := range 2 * postCount {
		kind, table, column := kindComment, "comments", "comment_id"
		if index%2 == 1 {
			kind, table, column = kindMessage, "messages", "message_id"
		}
		issue := fmt.Sprintf("%s-%d", textversionstest.Project, index%rereadBenchIssues+1)
		id := uuid.NewString()
		first, second := fmt.Sprintf("The first plan of post %d.", index), fmt.Sprintf("The second plan of post %d.", index)
		if index%10 == 0 {
			first = fmt.Sprintf("The first plan of post %d.\n\n", index) + long
		}
		textversionstest.Commit(b, database, func(tx pgx.Tx) {
			if _, err := tx.Exec(ctx, `
				insert into `+table+` (id, issue_key, author, body, deleted_at, deleted_by)
				values ($1::uuid, $2, '{"kind":"user","id":"alice"}', '', now(), '{"kind":"user","id":"alice"}')
			`, id, issue); err != nil {
				b.Fatalf("insert %s %s: %v", kind, id, err)
			}
			if _, err := tx.Exec(ctx, `
				insert into post_body_history (`+column+`, body, edited_by)
				select $1::uuid, body, '{"kind":"user","id":"alice"}' from unnest($2::text[]) with ordinality as h (body, n) order by n
			`, id, []string{first, second}); err != nil {
				b.Fatalf("keep the bodies of %s %s: %v", kind, id, err)
			}
			created := appendEvent(tx, issue, kind+".created", postPayload(id, issue, first))
			edited := appendEvent(tx, issue, kind+".edited", postPayload(id, issue, second))
			appendEvent(tx, issue, kind+".deleted", postPayload(id, issue, ""))
			// The rows a fill of rules version 1 wrote: the create, a cut body read from the
			// deleted post's row, and a comment's edit; a message's edit and both deletes it
			// passed over.
			body, from := first, bodyFromEvent
			if _, truncated := model.EventBody(first); truncated {
				body, from = "", bodyFromRow
			}
			writeSeedRow(b, tx, kind, id, created, body, from)
			again++
			if kind == kindComment {
				writeSeedRow(b, tx, kind, id, edited, second, bodyFromEvent)
			} else {
				again++
			}
		})
	}
	for index := range insertCount {
		seedDeletedInsert(b, database, fmt.Sprintf("%s-%d", textversionstest.Project, index+1), appendEvent)
	}
	var newest int64
	if err := database.Pool.QueryRow(ctx, `
		insert into search_text_versions_fill (id, event_id, event_at, rules)
		select 1, e.id, e.created_at, 1 from events e order by e.id desc limit 1
		returning event_id`).Scan(&newest); err != nil {
		b.Fatalf("write the cursor row: %v", err)
	}
	return newest, again
}

// writeSeedRow writes the keyword row of a post's event as a fill writes it: its vector made from
// body as the fill's insert makes a post's.
func writeSeedRow(b *testing.B, tx pgx.Tx, kind, id string, event int64, body, from string) {
	b.Helper()
	if _, err := tx.Exec(context.Background(), `
		insert into search_text_versions (kind, id, issue_key, project_key, event_id, event_seq, started_at, body, body_from, search)
		select $1, $2, e.issue_key, $3, e.id, e.seq, e.created_at, $5, $6, search_vector('', search_text($5))
		  from events e where e.id = $4`, kind, id, textversionstest.Project, event, body, from); err != nil {
		b.Fatalf("write the row of event %d: %v", event, err)
	}
}

// seedDeletedInsert gives issue a primary document whose version 2 holds a pending insert's
// words (insertVersionText), a comment anchored on the insert that is deleted, with the body its
// delete kept in post_body_history, and the issue row a fill of rules version 1 wrote of version
// 2, from its stored markdown.
func seedDeletedInsert(b *testing.B, database *store.Store, issue string, appendEvent func(pgx.Tx, string, string, map[string]any) int64) {
	b.Helper()
	ctx := context.Background()
	markdown, insertTree := insertVersionText(b)
	textversionstest.Commit(b, database, func(tx pgx.Tx) {
		var document string
		if err := tx.QueryRow(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			values ($1, $2, 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}') returning id::text
		`, issue, textversionstest.Project).Scan(&document); err != nil {
			b.Fatalf("insert the primary document of %s: %v", issue, err)
		}
		if _, err := tx.Exec(ctx, `
			insert into artifact_versions (artifact_id, number, markdown, authors, insert_tree)
			values ($1::uuid, 1, 'The brown fox jumps', '[]', null), ($1::uuid, 2, $2, '[]', $3)
		`, document, markdown, insertTree); err != nil {
			b.Fatalf("insert the versions of %s's document: %v", issue, err)
		}
		event := appendEvent(tx, issue, "artifact.version",
			map[string]any{"artifact_id": document, "version": map[string]any{"number": 2, "size": len(markdown)}})
		if _, err := tx.Exec(ctx, `
			with deleted as (
			       insert into comments (issue_key, author, body, anchor, deleted_at, deleted_by)
			       values ($1, '{"kind":"user","id":"alice"}', '', jsonb_build_object('artifact_id', $2::text, 'mark_id', $3::text),
			               now(), '{"kind":"user","id":"alice"}')
			       returning id)
			insert into post_body_history (comment_id, body, edited_by)
			select id, 'Add this word.', '{"kind":"user","id":"alice"}' from deleted
		`, issue, document, insertMark); err != nil {
			b.Fatalf("insert the deleted suggestion on %s: %v", issue, err)
		}
		if _, err := tx.Exec(ctx, `
			insert into search_text_versions (kind, id, issue_key, project_key, event_id, event_seq, started_at, body, body_from,
			                                  spec_artifact_id, spec_version, search)
			select 'issue', $1, $1, $2, e.id, e.seq, e.created_at, i.title, 'event', $3::uuid, 2,
			       issue_search_vector($1, i.title, $4)
			  from events e, issues i where e.id = $5 and i.key = $1`, issue, textversionstest.Project, document, markdown, event); err != nil {
			b.Fatalf("write the issue row of %s's version 2: %v", issue, err)
		}
	})
}

// seedPendingInserts writes count pending comments of alice on the insert of issue's primary
// document (insertMark), which seedDeletedInsert wrote, for the deletes BenchmarkTheRulesReRead
// runs beside its passes, and answers the document and the comments.
func seedPendingInserts(b *testing.B, database *store.Store, issue string, count int) (string, []string) {
	b.Helper()
	ctx := context.Background()
	var document string
	var comments []string
	textversionstest.Commit(b, database, func(tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `select id::text from artifacts where issue_key = $1 and is_primary`, issue).Scan(&document); err != nil {
			b.Fatalf("read the primary document of %s: %v", issue, err)
		}
		rows, err := tx.Query(ctx, `
			insert into comments (issue_key, author, body, anchor)
			select $1, '{"kind":"user","id":"alice"}', 'Add this word.',
			       jsonb_build_object('artifact_id', $2::text, 'mark_id', $3::text)
			  from generate_series(1, $4::integer)
			returning id::text`, issue, document, insertMark, count)
		if err != nil {
			b.Fatalf("insert the pending suggestions on %s: %v", issue, err)
		}
		if comments, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			b.Fatalf("insert the pending suggestions on %s: %v", issue, err)
		}
	})
	return document, comments
}
