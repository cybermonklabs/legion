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

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/textversions/textversionstest"
)

// The sizes of BenchmarkTheRulesReRead's log, which its environment can set: the events of the
// log (KEYWORD_REREAD_EVENTS, production's count by default) and the posts of each kind that are
// edited and deleted (KEYWORD_REREAD_POSTS).
const (
	rereadEventsEnv = "KEYWORD_REREAD_EVENTS"
	rereadPostsEnv  = "KEYWORD_REREAD_POSTS"
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
// or that change a post's text and their posts' creates. The keyword table holds the rows a fill
// of rules version 1 wrote of them: the creates and the comments' edits, each cut body read from
// its deleted post's row.
//
// Each iteration puts the table and the cursor row back as the seed left them, untimed, and times
// the pass. It reports, beside the wall time of a pass (ns/op), the time the database took to
// answer its statements as the client measured each (db-ms/op), the part of that its reads of the
// log took (log-read-ms/op), its batches (batches/op) and the rows it wrote again (rows/op,
// redone/op). It logs, once, the plan of the re-read's read of the log on the seeded fixture, run
// with ANALYZE and BUFFERS, which `go test` prints whole with -v alone (it cuts a benchmark's log
// at ten lines otherwise). Each round of the benchmark seeds its own log, so -benchtime Nx seeds
// twice, once for its first round of one. Run it in a build Job:
//
//	DISPATCH_TEST_DATABASE_URL=... go test ./internal/dispatch/textversions -run '^$' -bench BenchmarkTheRulesReRead -benchtime 3x -v -timeout 2h
//
// and to check it works on a small log: KEYWORD_REREAD_EVENTS=2000 KEYWORD_REREAD_POSTS=5 with
// -benchtime 1x.
func BenchmarkTheRulesReRead(b *testing.B) {
	eventCount := benchSize(b, rereadEventsEnv, 900_000)
	postCount := benchSize(b, rereadPostsEnv, 100)
	database := storetest.Open(b)
	textversionstest.SeedProject(b, database)
	ctx := context.Background()
	seedStarted := time.Now()
	newest, wantAgain := seedRereadLog(b, database, eventCount, postCount)
	b.Logf("seeded %d events and %d posts of each kind in %s",
		newest, postCount, time.Since(seedStarted).Round(time.Millisecond))
	if _, err := database.Pool.Exec(ctx, `
		create table keyword_reread_seed as select * from search_text_versions;
		analyze events; analyze search_text_versions; analyze comments; analyze messages;
		analyze post_body_history`); err != nil {
		b.Fatalf("keep the seeded keyword rows: %v", err)
	}
	rereadPlanLogged.Do(func() { logRereadPlan(b, database) })
	tracer := &statementTimer{}
	traced := storetest.TracedStore(b, database, tracer)

	var batches, rows, redone int
	b.ResetTimer()
	for range b.N {
		b.StopTimer()
		if _, err := database.Pool.Exec(ctx, `
			delete from search_text_versions;
			insert into search_text_versions select * from keyword_reread_seed;
			update search_text_versions_fill set rules = 1 where id = 1`); err != nil {
			b.Fatalf("put the seeded keyword rows back: %v", err)
		}
		fill := newFiller(traced.Pool, nil)
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
			rows, redone = rows+result.rows, redone+result.redone
			if result.raised {
				break
			}
		}
		b.StopTimer()
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
	statements, logReads := tracer.totals()
	b.ReportMetric(float64(statements.Milliseconds())/n, "db-ms/op")
	b.ReportMetric(float64(logReads.Milliseconds())/n, "log-read-ms/op")
	b.ReportMetric(float64(batches)/n, "batches/op")
	b.ReportMetric(float64(rows)/n, "rows/op")
	b.ReportMetric(float64(redone)/n, "redone/op")
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
// to its answer: every statement's, and the re-read's reads of the log's (rereadEventsQuery). Only
// the passes' batches run on that pool; the benchmark's own statements run on another.
type statementTimer struct {
	mu            sync.Mutex
	all, readLogs time.Duration
	open          map[*pgx.Conn]statementStart
}

// statementStart is a statement in flight on one connection: when it was sent, and whether it is
// a read of the log.
type statementStart struct {
	at      time.Time
	readLog bool
}

func (s *statementTimer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open == nil {
		s.open = map[*pgx.Conn]statementStart{}
	}
	s.open[conn] = statementStart{at: time.Now(), readLog: data.SQL == rereadEventsQuery}
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
	took := time.Since(start.at)
	s.all += took
	if start.readLog {
		s.readLogs += took
	}
}

// totals is the time of every statement traced and of the reads of the log among them.
func (s *statementTimer) totals() (time.Duration, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.all, s.readLogs
}

// seedRereadLog writes BenchmarkTheRulesReRead's log and the rows a fill of rules version 1 wrote
// of it, with the cursor row at its newest event and rules 1, and answers the newest event and
// how many rows a re-read writes of the types version 2 added.
func seedRereadLog(b *testing.B, database *store.Store, eventCount, postCount int) (int64, int) {
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
