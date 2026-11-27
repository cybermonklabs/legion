package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/signals"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

const queueProject = "LEGION"

func loadQueueTimelineFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "queue", name))
	if err != nil {
		t.Fatalf("read queue fixture %s: %v", name, err)
	}
	return data
}

// queueTimelineRig wires admission, the workflow engine and the outbox executor's queue timeline
// read against a test GitHub HTTP server and an isolated Postgres pool.
type queueTimelineRig struct {
	t       *testing.T
	pool    *store.Pool
	records *record.Postgres
	w       *workflowRuntime
	runner  *outbox
	now     time.Time
}

func newQueueTimelineRig(t *testing.T, githubURL string, now time.Time, renames ghrepo.Renames) *queueTimelineRig {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	repos := newRepositories(queueProject, renames)
	repos.fill(ghrepo.MustParse("cybermonklabs/legion"))
	engine := workflow.New(records, workflow.Config{Project: queueProject, Repositories: testRepositories,
		RepositoryRenames: renames, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
	admission := admit.New(records, engine, 2, queueProject, quietLogger())
	rig := &queueTimelineRig{t: t, pool: pool, records: records, now: now}
	admission.UseClock(func() time.Time { return rig.now })
	handlers := admit.Handlers(records, queueProject, engine, admission)
	w := &workflowRuntime{
		pool: pool, records: records, tokens: roleTokens{}, handlers: handlers,
		dispatchProject: queueProject, githubAPI: githubURL, repositories: repos,
		etags: nil, log: quietLogger(), mergeStateRetry: 10 * time.Millisecond,
	}
	runner := &outbox{
		pool: pool, records: records, handlers: handlers, dispatchProject: queueProject,
		tokens: roleTokens{}, githubAPI: githubURL, repositories: repos, log: quietLogger(),
		readQueueTimeline: w.readQueueTimeline, mergeStateWait: w.mergeStateWait,
		now: func() time.Time { return rig.now },
	}
	rig.w, rig.runner = w, runner
	return rig
}

func (r *queueTimelineRig) seedIssuePR(key, project, repo string, number int, branch string) {
	r.t.Helper()
	ctx := context.Background()
	if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := r.records.PutIssue(ctx, tx, record.Issue{Key: key, Repo: repo, DefaultBranch: "main", GitHubRepo: repo,
			Tree: key, Project: project, Title: key, Phase: phase.Merging, Generation: 1, Status: "in_progress", Rank: "A", HandedOver: true}); err != nil {
			return err
		}
		return r.records.PutPullRequest(ctx, tx, record.PullRequest{Issue: key, Repo: repo, Number: number,
			Branch: branch, HeadSHA: "head-1", Base: "main", State: record.PullRequestOpen, Failing: []string{}})
	}); err != nil {
		r.t.Fatalf("seed %s #%d: %v", key, number, err)
	}
}

func (r *queueTimelineRig) apply(eventID string, fact intake.Fact) {
	r.t.Helper()
	if _, err := intake.ApplyFact(context.Background(), r.pool, "github", eventID, fact, r.w.handlers...); err != nil {
		r.t.Fatalf("apply %s (%T): %v", eventID, fact, err)
	}
}

func (r *queueTimelineRig) drainTimelineRows() int {
	r.t.Helper()
	ctx := context.Background()
	ran := 0
	for {
		var due []record.OutboxRow
		if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			due, err = r.records.DueRows(ctx, tx, queueProject, "", []record.OutboxKind{record.OutboxKindQueueTimelineRead}, r.now)
			return err
		}); err != nil {
			r.t.Fatalf("list due queue_timeline_read rows: %v", err)
		}
		if len(due) == 0 {
			return ran
		}
		for _, row := range due {
			_ = r.runner.runRow(ctx, row)
			ran++
		}
	}
}

// settleTimelineRows runs the queue timeline rows until none is left, the clock moved a minute on
// past each read's wait between rounds: a read whose timeline does not show its move yet is read
// again up to mergeStateLadderReads attempts in all, and the last keeps what the timeline shows, so
// no row is left after that many rounds.
func (r *queueTimelineRig) settleTimelineRows() {
	r.t.Helper()
	for range mergeStateLadderReads {
		r.drainTimelineRows()
		r.now = r.now.Add(time.Minute)
	}
	if left := outboxRowsOfKindAnywhere(r.t, r.pool, record.OutboxKindQueueTimelineRead); left != 0 {
		r.t.Fatalf("after %d rounds %d queue timeline rows are left, want none", mergeStateLadderReads, left)
	}
}

// timelineMove is a merge-queue event of a timeline of cybermonklabs/legion as GitHub's REST
// timeline API serves it: its id, its kind and its created_at.
func timelineMove(id int64, event, at string) string {
	return fmt.Sprintf(`{"id":%d,"event":%q,"created_at":%q,"url":"https://api.github.com/repos/cybermonklabs/legion/issues/events/%d"}`, id, event, at, id)
}

// timelineOf is a page of a timeline that holds moves, in GitHub's order.
func timelineOf(moves ...string) string { return "[" + strings.Join(moves, ",") + "]" }

// scriptedTimeline serves the timeline of cybermonklabs/legion#5150: each read takes the first of
// the answers last given to answer, and the last of them answers every read after it. It counts
// the reads.
func scriptedTimeline(t *testing.T) (string, func(bodies ...string), *atomic.Int32) {
	t.Helper()
	var mu sync.Mutex
	var answers []string
	reads := &atomic.Int32{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/cybermonklabs/legion/issues/5150/timeline" {
			http.NotFound(w, r)
			return
		}
		reads.Add(1)
		mu.Lock()
		body := answers[0]
		if len(answers) > 1 {
			answers = answers[1:]
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(api.Close)
	answer := func(bodies ...string) {
		mu.Lock()
		answers = bodies
		mu.Unlock()
	}
	return api.URL, answer, reads
}

// queueWait is the wait of a pull request that entered the queue at added, RFC 3339, and left it
// at left, or stands in it where left is "".
func queueWait(t *testing.T, added, left string) signals.Wait {
	t.Helper()
	at := func(s string) time.Time {
		parsed, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return parsed.UTC()
	}
	wait := signals.Wait{Added: at(added)}
	if left != "" {
		wait.Left = at(left)
	}
	return wait
}

// readAgainThenKept runs the one queue timeline row of LEGION-5150, queued for the notified move,
// whose first read GitHub answers with a timeline that does not show the move yet: that read is
// read again, and the next one, the clock a minute on, keeps what the timeline shows by then,
// which leaves the waits want.
func (r *queueTimelineRig) readAgainThenKept(notified string, want []signals.Wait) {
	r.t.Helper()
	if ran := r.drainTimelineRows(); ran != 1 {
		r.t.Errorf("the %s's read ran %d rows, want 1", notified, ran)
	}
	if remaining := outboxRowsOfKind(r.t, r.pool, "LEGION-5150", record.OutboxKindQueueTimelineRead); remaining != 1 {
		r.t.Errorf("after a read whose timeline shows only the moves before the %s, %d queue timeline rows remain, want the one read again", notified, remaining)
	}
	r.now = r.now.Add(time.Minute)
	r.drainTimelineRows()
	if remaining := outboxRowsOfKind(r.t, r.pool, "LEGION-5150", record.OutboxKindQueueTimelineRead); remaining != 0 {
		r.t.Errorf("after GitHub's timeline showed the %s, %d queue timeline rows remain, want 0", notified, remaining)
	}
	if got := r.waitsAt(r.now); !reflect.DeepEqual(got, want) {
		r.t.Errorf("after the %s the waits = %+v, want %+v", notified, got, want)
	}
}

func (r *queueTimelineRig) waitsAt(at time.Time) []signals.Wait {
	r.t.Helper()
	var events []signals.QueueEvent
	if err := store.Transact(context.Background(), r.pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		events, err = r.records.QueueEventsAt(ctx, tx, queueProject, at)
		return err
	}); err != nil {
		r.t.Fatalf("read queue events at %s: %v", at, err)
	}
	return signals.WaitsOf(events)
}

func (r *queueTimelineRig) keptEvents() []signals.QueueEvent {
	r.t.Helper()
	var events []signals.QueueEvent
	if err := store.Transact(context.Background(), r.pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		events, err = r.records.QueueEventsAt(ctx, tx, queueProject, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
		return err
	}); err != nil {
		r.t.Fatalf("read kept queue events: %v", err)
	}
	return events
}

// TestSignalQueueTimeline exercises the merge-queue timeline path end to end from real captured
// GitHub timelines (#5150 and #4910): GitHub's own timestamps are kept, duplicate deliveries keep
// each event once, add/remove/re-add makes two waits, merge plus same-second removal ends the wait
// once, out-of-order deliveries fold cleanly, renamed repositories record the canonical event URL
// name, another project's pull request is refused, a Dispatch-attributed pull request whose branch
// names no issue keeps nothing, and a timeline read that lacks the notified move retries.
func TestSignalQueueTimeline(t *testing.T) {
	t.Parallel()
	fixture5150 := loadQueueTimelineFixture(t, "timeline-5150.json")
	fixture4910 := loadQueueTimelineFixture(t, "timeline-4910.json")
	mustTime := func(s string) time.Time {
		tm, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return tm.UTC()
	}

	t.Run("merge plus same-second removal and duplicate delivery", func(t *testing.T) {
		t.Parallel()
		var calls atomic.Int32
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/repos/cybermonklabs/legion/issues/5150/timeline" {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(fixture5150)
				return
			}
			http.NotFound(w, r)
		}))
		defer api.Close()
		at := mustTime("2026-11-23T19:00:00Z")
		rig := newQueueTimelineRig(t, api.URL, at, ghrepo.Renames{})
		rig.seedIssuePR("LEGION-5150", queueProject, "cybermonklabs/legion", 5150, "legion/LEGION-5150")
		rig.apply("github.enq-1", intake.PullRequestEnqueued{Repo: "cybermonklabs/legion", Number: 5150, Branch: "legion/LEGION-5150"})
		rig.apply("github.enq-1", intake.PullRequestEnqueued{Repo: "cybermonklabs/legion", Number: 5150, Branch: "legion/LEGION-5150"})
		rig.apply("github.merged-1", intake.PullRequestMerged{Repo: "cybermonklabs/legion", Number: 5150, MergeSHA: "m1", HeadSHA: "h1"})
		rig.drainTimelineRows()
		if calls.Load() == 0 {
			t.Fatal("timeline endpoint was never called")
		}
		want := []signals.Wait{{Added: mustTime("2026-11-23T02:27:46Z"), Left: mustTime("2026-11-23T18:25:55Z")}}
		if got := rig.waitsAt(at); !reflect.DeepEqual(got, want) {
			t.Fatalf("waits = %+v, want %+v", got, want)
		}
		if events := rig.keptEvents(); len(events) != 3 {
			t.Fatalf("kept %d distinct event rows, want 3 (add, merge, remove): %+v", len(events), events)
		}
	})

	t.Run("add remove re-add", func(t *testing.T) {
		t.Parallel()
		// Serve #4910 split across two pages to also prove pagination.
		var raw []json.RawMessage
		if err := json.Unmarshal(fixture4910, &raw); err != nil {
			t.Fatal(err)
		}
		mid := len(raw) / 2
		page1, _ := json.Marshal(raw[:mid])
		page2, _ := json.Marshal(raw[mid:])
		var api *httptest.Server
		api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/repos/cybermonklabs/legion/issues/4910/timeline" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Query().Get("page") == "2" {
				_, _ = w.Write(page2)
				return
			}
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/cybermonklabs/legion/issues/4910/timeline?per_page=100&page=2>; rel="next"`, api.URL))
			_, _ = w.Write(page1)
		}))
		defer api.Close()
		at := mustTime("2026-11-23T03:00:00Z")
		rig := newQueueTimelineRig(t, api.URL, at, ghrepo.Renames{})
		rig.seedIssuePR("LEGION-4910", queueProject, "cybermonklabs/legion", 4910, "legion/LEGION-4910")
		rig.apply("github.enq-4910", intake.PullRequestEnqueued{Repo: "cybermonklabs/legion", Number: 4910, Branch: "legion/LEGION-4910"})
		// The store has kept no move of #4910, and its timeline ends in an exit, so the entry's read
		// is read through the ladder and then keeps what the timeline shows.
		rig.settleTimelineRows()
		want := []signals.Wait{
			{Added: mustTime("2026-11-22T19:51:26Z"), Left: mustTime("2026-11-23T01:39:21Z")},
			{Added: mustTime("2026-11-23T01:39:23Z"), Left: mustTime("2026-11-23T02:46:55Z")},
		}
		if got := rig.waitsAt(at); !reflect.DeepEqual(got, want) {
			t.Fatalf("waits = %+v, want %+v", got, want)
		}
	})

	t.Run("delivery out of order", func(t *testing.T) {
		t.Parallel()
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/repos/cybermonklabs/legion/issues/5150/timeline" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(fixture5150)
				return
			}
			http.NotFound(w, r)
		}))
		defer api.Close()
		at := mustTime("2026-11-23T19:00:00Z")
		rig := newQueueTimelineRig(t, api.URL, at, ghrepo.Renames{})
		rig.seedIssuePR("LEGION-5150", queueProject, "cybermonklabs/legion", 5150, "legion/LEGION-5150")
		// Merged arrives before enqueued.
		rig.apply("github.merged-ooo", intake.PullRequestMerged{Repo: "cybermonklabs/legion", Number: 5150, MergeSHA: "m1", HeadSHA: "h1"})
		rig.drainTimelineRows()
		rig.apply("github.enq-ooo", intake.PullRequestEnqueued{Repo: "cybermonklabs/legion", Number: 5150, Branch: "legion/LEGION-5150"})
		rig.drainTimelineRows()
		want := []signals.Wait{{Added: mustTime("2026-11-23T02:27:46Z"), Left: mustTime("2026-11-23T18:25:55Z")}}
		if got := rig.waitsAt(at); !reflect.DeepEqual(got, want) {
			t.Fatalf("waits = %+v, want %+v", got, want)
		}
	})

	t.Run("renamed repository keeps event url canonical name", func(t *testing.T) {
		t.Parallel()
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/repos/cybermonklabs/legion/issues/5150/timeline" || r.URL.Path == "/repos/sjawhar/legion/issues/5150/timeline" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(fixture5150)
				return
			}
			http.NotFound(w, r)
		}))
		defer api.Close()
		renames, err := ghrepo.NewRenames([]ghrepo.Rename{ghrepo.MustRename("sjawhar/legion", "cybermonklabs/legion")})
		if err != nil {
			t.Fatal(err)
		}
		at := mustTime("2026-11-23T19:00:00Z")
		rig := newQueueTimelineRig(t, api.URL, at, renames)
		rig.seedIssuePR("LEGION-5150", queueProject, "sjawhar/legion", 5150, "legion/LEGION-5150")
		rig.apply("github.enq-oldname", intake.PullRequestEnqueued{Repo: "sjawhar/legion", Number: 5150, Branch: "legion/LEGION-5150"})
		// The store has kept no move of #5150, and its timeline ends in an exit, so the entry's read
		// is read through the ladder and then keeps what the timeline shows.
		rig.settleTimelineRows()
		events := rig.keptEvents()
		if len(events) != 3 {
			t.Fatalf("kept %d events, want 3", len(events))
		}
		for _, e := range events {
			if e.Repo != "cybermonklabs/legion" {
				t.Errorf("kept repo = %q, want canonical cybermonklabs/legion from event url", e.Repo)
			}
		}
	})

	// A repository renamed between two reads of one pull request's timeline: the first read's
	// events name the old repository in their urls, and the second's name the new one for the same
	// event ids. Each event keeps the name GitHub gives it now, so the pull request is one pull
	// request and its wait closes, never an old-named entry left open beside a new-named exit.
	t.Run("repository renamed between two reads", func(t *testing.T) {
		t.Parallel()
		var items []map[string]any
		if err := json.Unmarshal(fixture5150, &items); err != nil {
			t.Fatal(err)
		}
		var added []map[string]any
		for _, item := range items {
			if item["event"] == "added_to_merge_queue" {
				added = append(added, item)
			}
		}
		beforeRename, err := json.Marshal(added)
		if err != nil {
			t.Fatal(err)
		}
		beforeRename = bytes.ReplaceAll(beforeRename, []byte("/repos/cybermonklabs/legion/"), []byte("/repos/sjawhar/legion/"))
		var reads atomic.Int32
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/repos/cybermonklabs/legion/issues/5150/timeline" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if reads.Add(1) == 1 {
				_, _ = w.Write(beforeRename)
				return
			}
			_, _ = w.Write(fixture5150)
		}))
		defer api.Close()
		at := mustTime("2026-11-23T19:00:00Z")
		rig := newQueueTimelineRig(t, api.URL, at, ghrepo.Renames{})
		rig.seedIssuePR("LEGION-5150", queueProject, "cybermonklabs/legion", 5150, "legion/LEGION-5150")
		rig.apply("github.enq-before-rename", intake.PullRequestEnqueued{Repo: "cybermonklabs/legion", Number: 5150, Branch: "legion/LEGION-5150"})
		rig.drainTimelineRows()
		rig.apply("github.merged-after-rename", intake.PullRequestMerged{Repo: "cybermonklabs/legion", Number: 5150, MergeSHA: "m1", HeadSHA: "h1"})
		rig.drainTimelineRows()
		want := []signals.Wait{{Added: mustTime("2026-11-23T02:27:46Z"), Left: mustTime("2026-11-23T18:25:55Z")}}
		if got := rig.waitsAt(at); !reflect.DeepEqual(got, want) {
			t.Fatalf("waits = %+v, want %+v: the entry read under the old name must close with the exit read under the new one", got, want)
		}
		for _, e := range rig.keptEvents() {
			if e.Repo != "cybermonklabs/legion" {
				t.Errorf("event %d keeps repository %q, want the name GitHub gives it now, cybermonklabs/legion", e.ID, e.Repo)
			}
		}
	})

	t.Run("another projects pull request refused", func(t *testing.T) {
		t.Parallel()
		var calls atomic.Int32
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			_, _ = w.Write(fixture5150)
		}))
		defer api.Close()
		at := mustTime("2026-11-23T19:00:00Z")
		rig := newQueueTimelineRig(t, api.URL, at, ghrepo.Renames{})
		rig.seedIssuePR("OTHER-5150", "OTHER", "cybermonklabs/legion", 5150, "legion/OTHER-5150")
		rig.apply("github.other-enq", intake.PullRequestEnqueued{Repo: "cybermonklabs/legion", Number: 5150, Branch: "legion/OTHER-5150"})
		rig.apply("github.other-deq", intake.PullRequestDequeued{Repo: "cybermonklabs/legion", Number: 5150, HeadSHA: "h1"})
		rig.apply("github.other-merged", intake.PullRequestMerged{Repo: "cybermonklabs/legion", Number: 5150, MergeSHA: "m1", HeadSHA: "h1"})
		rig.drainTimelineRows()
		if calls.Load() != 0 {
			t.Fatalf("read GitHub %d times for another project's PR, want 0", calls.Load())
		}
		if got := rig.keptEvents(); len(got) != 0 {
			t.Fatalf("kept %+v for another project's PR, want none", got)
		}
	})

	t.Run("dispatch-attributed pull request whose branch names no issue keeps nothing", func(t *testing.T) {
		t.Parallel()
		var calls atomic.Int32
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			_, _ = w.Write(fixture5150)
		}))
		defer api.Close()
		at := mustTime("2026-11-23T19:00:00Z")
		rig := newQueueTimelineRig(t, api.URL, at, ghrepo.Renames{})
		// Seed a recorded PR whose branch is NOT a legion/<KEY> issue branch.
		rig.seedIssuePR("LEGION-77", queueProject, "cybermonklabs/legion", 5150, "feature/custom-branch")
		rig.apply("github.noissue-enq", intake.PullRequestEnqueued{Repo: "cybermonklabs/legion", Number: 5150, Branch: "feature/custom-branch"})
		rig.apply("github.noissue-deq", intake.PullRequestDequeued{Repo: "cybermonklabs/legion", Number: 5150, HeadSHA: "h1"})
		rig.apply("github.noissue-merged", intake.PullRequestMerged{Repo: "cybermonklabs/legion", Number: 5150, MergeSHA: "m1", HeadSHA: "h1"})
		rig.drainTimelineRows()
		if calls.Load() != 0 {
			t.Fatalf("read GitHub %d times for a non-issue branch PR, want 0", calls.Load())
		}
		if got := rig.keptEvents(); len(got) != 0 {
			t.Fatalf("kept %+v for a non-issue branch PR, want none", got)
		}
	})

	t.Run("timeline read lacking notified move retries", func(t *testing.T) {
		t.Parallel()
		var calls atomic.Int32
		// First read returns no queue events yet; second read returns the add event.
		addOnly := `[{"id":36963393320,"event":"added_to_merge_queue","created_at":"2026-11-23T02:27:46Z","url":"https://api.github.com/repos/cybermonklabs/legion/issues/events/36963393320"}]`
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/repos/cybermonklabs/legion/issues/5150/timeline" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if calls.Add(1) == 1 {
				_, _ = io.WriteString(w, `[{"event":"committed"}]`)
				return
			}
			_, _ = io.WriteString(w, addOnly)
		}))
		defer api.Close()
		at := mustTime("2026-11-23T03:00:00Z")
		rig := newQueueTimelineRig(t, api.URL, at, ghrepo.Renames{})
		rig.seedIssuePR("LEGION-5150", queueProject, "cybermonklabs/legion", 5150, "legion/LEGION-5150")
		rig.apply("github.enq-retry", intake.PullRequestEnqueued{Repo: "cybermonklabs/legion", Number: 5150, Branch: "legion/LEGION-5150"})
		// First run sees missing move and schedules a retry instead of finishing.
		if ran := rig.drainTimelineRows(); ran != 1 {
			t.Fatalf("first drain ran %d rows, want 1", ran)
		}
		if calls.Load() != 1 {
			t.Fatalf("calls after first attempt = %d, want 1", calls.Load())
		}
		if remaining := outboxRowsOfKind(t, rig.pool, "LEGION-5150", record.OutboxKindQueueTimelineRead); remaining != 1 {
			t.Fatalf("after stale timeline read, remaining outbox rows = %d, want 1 waiting for retry", remaining)
		}
		// Advance clock past retry delay and run again: now the move is present and kept.
		rig.now = rig.now.Add(time.Minute)
		if ran := rig.drainTimelineRows(); ran != 1 {
			t.Fatalf("second drain ran %d rows, want 1", ran)
		}
		if calls.Load() != 2 {
			t.Fatalf("calls after retry = %d, want 2", calls.Load())
		}
		if remaining := outboxRowsOfKind(t, rig.pool, "LEGION-5150", record.OutboxKindQueueTimelineRead); remaining != 0 {
			t.Fatalf("after retry succeeded, remaining outbox rows = %d, want 0", remaining)
		}
		want := []signals.Wait{{Added: mustTime("2026-11-23T02:27:46Z")}}
		if got := rig.waitsAt(rig.now); !reflect.DeepEqual(got, want) {
			t.Fatalf("waits after retry = %+v, want %+v", got, want)
		}
	})

	// A read GitHub refuses is read again for as long as it refuses, never dropped: a dropped exit
	// leaves its wait open, and an open wait reads as the longest wait of the queue until the next
	// boot. Only a read GitHub answers, with a timeline that lacks the notified move, has a bound.
	t.Run("a failed read is read again until GitHub answers", func(t *testing.T) {
		t.Parallel()
		const refused = mergeStateLadderReads + 3
		var calls atomic.Int32
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/repos/cybermonklabs/legion/issues/5150/timeline" {
				http.NotFound(w, r)
				return
			}
			if calls.Add(1) <= refused {
				http.Error(w, `{"message":"Bad Gateway"}`, http.StatusBadGateway)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture5150)
		}))
		defer api.Close()
		at := mustTime("2026-11-23T19:00:00Z")
		rig := newQueueTimelineRig(t, api.URL, at, ghrepo.Renames{})
		rig.seedIssuePR("LEGION-5150", queueProject, "cybermonklabs/legion", 5150, "legion/LEGION-5150")
		rig.apply("github.enq-refused", intake.PullRequestEnqueued{Repo: "cybermonklabs/legion", Number: 5150, Branch: "legion/LEGION-5150"})
		for attempt := 1; attempt <= refused+1; attempt++ {
			if ran := rig.drainTimelineRows(); ran != 1 {
				t.Fatalf("attempt %d ran %d rows, want the one row still owed", attempt, ran)
			}
			rig.now = rig.now.Add(time.Minute)
		}
		if got := calls.Load(); got != refused+1 {
			t.Fatalf("GitHub was read %d times, want %d refused reads and the one that answered", got, refused)
		}
		if remaining := outboxRowsOfKind(t, rig.pool, "LEGION-5150", record.OutboxKindQueueTimelineRead); remaining != 0 {
			t.Fatalf("after GitHub answered, %d queue timeline rows remain, want 0", remaining)
		}
		want := []signals.Wait{{Added: mustTime("2026-11-23T02:27:46Z"), Left: mustTime("2026-11-23T18:25:55Z")}}
		if got := rig.waitsAt(rig.now); !reflect.DeepEqual(got, want) {
			t.Fatalf("waits = %+v, want %+v", got, want)
		}
	})

	t.Run("a timeline that never shows the notified move is kept as read once the reads are spent", func(t *testing.T) {
		t.Parallel()
		var calls atomic.Int32
		addOnly := `[{"id":36963393320,"event":"added_to_merge_queue","created_at":"2026-11-23T02:27:46Z","url":"https://api.github.com/repos/cybermonklabs/legion/issues/events/36963393320"}]`
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, addOnly)
		}))
		defer api.Close()
		at := mustTime("2026-11-23T19:00:00Z")
		rig := newQueueTimelineRig(t, api.URL, at, ghrepo.Renames{})
		rig.seedIssuePR("LEGION-5150", queueProject, "cybermonklabs/legion", 5150, "legion/LEGION-5150")
		rig.apply("github.deq-lag", intake.PullRequestDequeued{Repo: "cybermonklabs/legion", Number: 5150, HeadSHA: "h1"})
		for attempt := 1; attempt <= mergeStateLadderReads; attempt++ {
			if ran := rig.drainTimelineRows(); ran != 1 {
				t.Fatalf("attempt %d ran %d rows, want 1", attempt, ran)
			}
			rig.now = rig.now.Add(time.Minute)
		}
		if got := calls.Load(); got != mergeStateLadderReads {
			t.Fatalf("GitHub was read %d times, want the ladder's %d", got, mergeStateLadderReads)
		}
		if remaining := outboxRowsOfKind(t, rig.pool, "LEGION-5150", record.OutboxKindQueueTimelineRead); remaining != 0 {
			t.Fatalf("after the ladder %d queue timeline rows remain, want 0", remaining)
		}
		want := []signals.Wait{{Added: mustTime("2026-11-23T02:27:46Z")}}
		if got := rig.waitsAt(rig.now); !reflect.DeepEqual(got, want) {
			t.Fatalf("waits = %+v, want the entry GitHub's timeline showed, kept with no exit invented", got)
		}
	})

	// The store forgets a pull request's events 6 hours after its last exit, and keeps how many
	// entries and exits of it it has kept (record.Store.KeepQueueEvents). A re-entry after that,
	// and the exit after the re-entry, are each checked against those counts: a read whose timeline
	// still shows only the moves before the notified one is read again rather than taken as
	// current, and the move is kept once GitHub's timeline shows it.
	t.Run("a re-entry and its exit more than six hours after the last exit wait for the timeline to show them", func(t *testing.T) {
		t.Parallel()
		add1 := timelineMove(101, "added_to_merge_queue", "2026-11-23T02:00:00Z")
		remove1 := timelineMove(102, "removed_from_merge_queue", "2026-11-23T03:00:00Z")
		add2 := timelineMove(103, "added_to_merge_queue", "2026-11-23T10:00:00Z")
		remove2 := timelineMove(104, "removed_from_merge_queue", "2026-11-23T11:00:00Z")
		url, answer, _ := scriptedTimeline(t)
		rig := newQueueTimelineRig(t, url, mustTime("2026-11-23T03:00:05Z"), ghrepo.Renames{})
		rig.seedIssuePR("LEGION-5150", queueProject, "cybermonklabs/legion", 5150, "legion/LEGION-5150")
		answer(timelineOf(add1, remove1))
		rig.apply("github.deq-first", intake.PullRequestDequeued{Repo: "cybermonklabs/legion", Number: 5150, HeadSHA: "h1"})
		rig.drainTimelineRows()
		if got := rig.keptEvents(); len(got) != 2 {
			t.Fatalf("after the first exit the store keeps %+v, want the entry and the exit", got)
		}
		// Seven hours on, another pull request's timeline is kept, and the store forgets #5150's
		// events, whose newest is an exit more than 6 hours old; that pull request's are forgotten as
		// they are kept.
		rig.now = mustTime("2026-11-23T10:00:05Z")
		rig.apply("github.other-timeline", intake.QueueTimeline{Repo: "cybermonklabs/legion", Number: 6001, Events: []signals.QueueEvent{
			{ID: 201, Repo: "cybermonklabs/legion", PullRequest: 6001, At: mustTime("2026-11-23T01:00:00Z"), Enters: true},
			{ID: 202, Repo: "cybermonklabs/legion", PullRequest: 6001, At: mustTime("2026-11-23T02:00:00Z"), Enters: false},
		}})
		if got := rig.keptEvents(); len(got) != 0 {
			t.Fatalf("7 hours after #5150's exit the store keeps %+v, want its events forgotten", got)
		}

		answer(timelineOf(add1, remove1), timelineOf(add1, remove1, add2))
		rig.apply("github.enq-again", intake.PullRequestEnqueued{Repo: "cybermonklabs/legion", Number: 5150, Branch: "legion/LEGION-5150"})
		rig.readAgainThenKept("re-entry", []signals.Wait{
			queueWait(t, "2026-11-23T02:00:00Z", "2026-11-23T03:00:00Z"), queueWait(t, "2026-11-23T10:00:00Z", "")})

		rig.now = mustTime("2026-11-23T11:00:05Z")
		answer(timelineOf(add1, remove1, add2), timelineOf(add1, remove1, add2, remove2))
		rig.apply("github.deq-again", intake.PullRequestDequeued{Repo: "cybermonklabs/legion", Number: 5150, HeadSHA: "h2"})
		rig.readAgainThenKept("exit", []signals.Wait{
			queueWait(t, "2026-11-23T02:00:00Z", "2026-11-23T03:00:00Z"), queueWait(t, "2026-11-23T10:00:00Z", "2026-11-23T11:00:00Z")})
	})

	// A pull request whose earlier moves the store never kept, such as moves before the first boot
	// of the release that keeps them, has no count a notified move needs one more than: its read
	// is current only where the timeline's newest move is of the notified direction
	// (record.QueueTimelineRead.Unkept). An entry long after an exit the store never kept, read
	// while the timeline still ends in that exit, is read again; so is an exit read while the
	// timeline still ends in an entry the store never kept. Each timeline's last move is the one
	// notified, which the first read does not show yet.
	add1 := timelineMove(101, "added_to_merge_queue", "2026-11-23T02:00:00Z")
	remove1 := timelineMove(102, "removed_from_merge_queue", "2026-11-23T03:00:00Z")
	add2 := timelineMove(103, "added_to_merge_queue", "2026-11-23T10:00:00Z")
	for _, tc := range []struct {
		move     string
		notified intake.Fact
		at       string
		moves    []string
		want     []signals.Wait
	}{
		{move: "entry", notified: intake.PullRequestEnqueued{Repo: "cybermonklabs/legion", Number: 5150, Branch: "legion/LEGION-5150"},
			at: "2026-11-23T10:00:05Z", moves: []string{add1, remove1, add2},
			want: []signals.Wait{queueWait(t, "2026-11-23T02:00:00Z", "2026-11-23T03:00:00Z"), queueWait(t, "2026-11-23T10:00:00Z", "")}},
		{move: "exit", notified: intake.PullRequestDequeued{Repo: "cybermonklabs/legion", Number: 5150, HeadSHA: "h2"},
			at: "2026-11-23T11:00:05Z", moves: []string{add1, remove1, add2, timelineMove(104, "removed_from_merge_queue", "2026-11-23T11:00:00Z")},
			want: []signals.Wait{queueWait(t, "2026-11-23T02:00:00Z", "2026-11-23T03:00:00Z"), queueWait(t, "2026-11-23T10:00:00Z", "2026-11-23T11:00:00Z")}},
	} {
		t.Run("a first "+tc.move+" of a pull request whose earlier moves the store never kept waits for the timeline to show it", func(t *testing.T) {
			t.Parallel()
			url, answer, _ := scriptedTimeline(t)
			rig := newQueueTimelineRig(t, url, mustTime(tc.at), ghrepo.Renames{})
			rig.seedIssuePR("LEGION-5150", queueProject, "cybermonklabs/legion", 5150, "legion/LEGION-5150")
			answer(timelineOf(tc.moves[:len(tc.moves)-1]...), timelineOf(tc.moves...))
			rig.apply("github.first-"+tc.move, tc.notified)
			rig.readAgainThenKept(tc.move, tc.want)
		})
	}

	// A pull request the store has kept no move of that leaves the queue before its entry's read
	// runs shows an exit last. The entry's read is read again to the end of the ladder, and then
	// keeps what the timeline shows; the exit's own read, whose newest move is that exit, keeps the
	// entry and the exit at once, so the queue reads the wait right, and no read waits for longer
	// than the ladder.
	t.Run("a pull request that leaves the queue before its entry's read keeps its wait, and no read waits for ever", func(t *testing.T) {
		t.Parallel()
		url, answer, reads := scriptedTimeline(t)
		answer(timelineOf(timelineMove(101, "added_to_merge_queue", "2026-11-23T10:00:00Z"),
			timelineMove(102, "removed_from_merge_queue", "2026-11-23T10:00:20Z")))
		rig := newQueueTimelineRig(t, url, mustTime("2026-11-23T10:00:30Z"), ghrepo.Renames{})
		rig.seedIssuePR("LEGION-5150", queueProject, "cybermonklabs/legion", 5150, "legion/LEGION-5150")
		rig.apply("github.enq-quick", intake.PullRequestEnqueued{Repo: "cybermonklabs/legion", Number: 5150, Branch: "legion/LEGION-5150"})
		rig.apply("github.deq-quick", intake.PullRequestDequeued{Repo: "cybermonklabs/legion", Number: 5150, HeadSHA: "h1"})
		if ran := rig.drainTimelineRows(); ran != 2 {
			t.Fatalf("the first drain ran %d rows, want the entry's and the exit's", ran)
		}
		want := []signals.Wait{queueWait(t, "2026-11-23T10:00:00Z", "2026-11-23T10:00:20Z")}
		if got := rig.waitsAt(rig.now); !reflect.DeepEqual(got, want) {
			t.Fatalf("after the exit's read the waits = %+v, want %+v", got, want)
		}
		if remaining := outboxRowsOfKind(t, rig.pool, "LEGION-5150", record.OutboxKindQueueTimelineRead); remaining != 1 {
			t.Fatalf("after the first reads %d queue timeline rows remain, want the entry's, read again", remaining)
		}
		rig.settleTimelineRows()
		if got := reads.Load(); got != mergeStateLadderReads+1 {
			t.Fatalf("GitHub was read %d times, want the entry's ladder of %d and the exit's one read", got, mergeStateLadderReads)
		}
		if got := rig.waitsAt(rig.now); !reflect.DeepEqual(got, want) {
			t.Fatalf("after the entry's ladder the waits = %+v, want %+v", got, want)
		}
	})

	// GitHub answering that a pull request's timeline is not there (404) is an answer no later read
	// changes: the row's read forgets the events kept of the pull request, as the boot's read does,
	// rather than leave its entry open for good.
	t.Run("a timeline GitHub answers is gone forgets the pull request's kept events", func(t *testing.T) {
		t.Parallel()
		var calls atomic.Int32
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}))
		defer api.Close()
		rig := newQueueTimelineRig(t, api.URL, mustTime("2026-11-23T19:00:00Z"), ghrepo.Renames{})
		rig.seedIssuePR("LEGION-5150", queueProject, "cybermonklabs/legion", 5150, "legion/LEGION-5150")
		rig.apply("github.kept-entry", intake.QueueTimeline{Repo: "cybermonklabs/legion", Number: 5150, Events: []signals.QueueEvent{
			{ID: 36963393320, Repo: "cybermonklabs/legion", PullRequest: 5150, At: mustTime("2026-11-23T02:27:46Z"), Enters: true},
		}})
		rig.apply("github.deq-gone", intake.PullRequestDequeued{Repo: "cybermonklabs/legion", Number: 5150, HeadSHA: "h1"})
		if ran := rig.drainTimelineRows(); ran != 1 || calls.Load() != 1 {
			t.Fatalf("ran %d rows that read GitHub %d times, want the one row read once", ran, calls.Load())
		}
		if remaining := outboxRowsOfKind(t, rig.pool, "LEGION-5150", record.OutboxKindQueueTimelineRead); remaining != 0 {
			t.Fatalf("after GitHub answered the timeline is gone, %d queue timeline rows remain, want 0", remaining)
		}
		if got := rig.keptEvents(); len(got) != 0 {
			t.Fatalf("after GitHub answered the timeline is gone the store keeps %+v, want the pull request's events forgotten", got)
		}
	})
}

// TestSignalBootReconciles verifies the boot merge-queue reconciliation: reading the paged GraphQL
// mergeQueue snapshot to find queued PRs of this project (ignoring other projects and non-issue
// branches), reading timelines for queued PRs and for previously open waits no longer in the
// snapshot, preserving waits across a restart over the same store, and marking the queue input
// absent with a reason on a failed read rather than reporting an empty queue.
func TestSignalBootReconciles(t *testing.T) {
	t.Parallel()
	fixture5150 := loadQueueTimelineFixture(t, "timeline-5150.json")
	mustTime := func(s string) time.Time {
		tm, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return tm.UTC()
	}

	t.Run("queue", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		var mu sync.Mutex
		failGraphQL := false
		// PR 6001 is currently queued (returned across page 2 of GraphQL snapshot).
		// PR 5150 was an open wait in the store that merged while the daemon was down (not in snapshot).
		// PR 7001 is another project's PR in the snapshot (must not have its timeline read).
		// PR 7002 is a non-issue branch PR in the snapshot (must not have its timeline read).
		var timelineReads []string
		timeline6001 := `[{"id":990001,"event":"added_to_merge_queue","created_at":"2026-11-23T18:00:00Z","url":"https://api.github.com/repos/cybermonklabs/legion/issues/events/990001"}]`
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			failing := failGraphQL
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/graphql" {
				if failing {
					http.Error(w, `{"message":"boom"}`, http.StatusBadGateway)
					return
				}
				var req struct {
					Variables map[string]any `json:"variables"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				cursor, _ := req.Variables["after"].(string)
				if cursor == "" {
					_, _ = io.WriteString(w, `{"data":{"repository":{"mergeQueue":{"entries":{"pageInfo":{"hasNextPage":true,"endCursor":"c1"},"nodes":[`+
						`{"enqueuedAt":"2026-11-23T17:00:00Z","pullRequest":{"number":7001,"headRefName":"legion/OTHER-1"}},`+
						`{"enqueuedAt":"2026-11-23T17:05:00Z","pullRequest":{"number":7002,"headRefName":"fix/unrelated"}}`+
						`]}}}}}`)
					return
				}
				_, _ = io.WriteString(w, `{"data":{"repository":{"mergeQueue":{"entries":{"pageInfo":{"hasNextPage":false,"endCursor":"c2"},"nodes":[`+
					`{"enqueuedAt":"2026-11-23T18:00:00Z","pullRequest":{"number":6001,"headRefName":"legion/LEGION-6001"}}`+
					`]}}}}}`)
				return
			}
			mu.Lock()
			timelineReads = append(timelineReads, r.URL.Path)
			mu.Unlock()
			switch r.URL.Path {
			case "/repos/cybermonklabs/legion/issues/6001/timeline":
				_, _ = io.WriteString(w, timeline6001)
			case "/repos/cybermonklabs/legion/issues/5150/timeline":
				_, _ = w.Write(fixture5150)
			default:
				http.NotFound(w, r)
			}
		}))
		defer api.Close()

		bootAt := mustTime("2026-11-23T19:00:00Z")
		rig := newQueueTimelineRig(t, api.URL, bootAt, ghrepo.Renames{})
		// Seed an open wait for #5150 before this boot, and seed a repo/branch serving the project.
		rig.seedIssuePR("LEGION-5150", queueProject, "cybermonklabs/legion", 5150, "legion/LEGION-5150")
		if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
			return rig.records.KeepQueueEvents(ctx, tx, queueProject, []signals.QueueEvent{
				{ID: 36963393320, Repo: "cybermonklabs/legion", PullRequest: 5150, At: mustTime("2026-11-23T02:27:46Z"), Enters: true},
			}, bootAt)
		}); err != nil {
			t.Fatalf("seed open wait for 5150: %v", err)
		}

		// Boot 1: fails GraphQL -> coverage reads absent with a reason, never an empty queue.
		mu.Lock()
		failGraphQL = true
		mu.Unlock()
		if _, err := intake.ApplyFact(ctx, rig.pool, "daemon", "boot:LEGION:1", intake.DaemonBooted{Boot: 1}, rig.w.handlers...); err != nil {
			t.Fatalf("apply boot 1: %v", err)
		}
		if rig.w.readQueueOnce(ctx, "1.1") == nil {
			t.Fatal("the read of the merge queue succeeded while GitHub refused the query, want it failed")
		}
		var since time.Time
		var recorded bool
		var readErr string
		if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			since, recorded, readErr, err = rig.records.SignalInputStatus(ctx, tx, queueProject, signals.Queue)
			return err
		}); err != nil {
			t.Fatalf("read signal input status after failed boot: %v", err)
		}
		if !recorded || !since.Equal(bootAt) {
			t.Fatalf("after boot 1 kept_since = %s (recorded %t), want %s", since, recorded, bootAt)
		}
		if strings.TrimSpace(readErr) == "" {
			t.Fatal("after failed boot read, queue input read_error is empty; want a reason so queue reads absent rather than empty")
		}

		// Boot 2: succeeds -> reads paged snapshot, reads timelines for #6001 and stale open wait #5150, clears read_error.
		mu.Lock()
		failGraphQL = false
		timelineReads = nil
		mu.Unlock()
		rig.now = bootAt.Add(time.Minute)
		if _, err := intake.ApplyFact(ctx, rig.pool, "daemon", "boot:LEGION:2", intake.DaemonBooted{Boot: 2}, rig.w.handlers...); err != nil {
			t.Fatalf("apply boot 2: %v", err)
		}
		if err := rig.w.readQueueOnce(ctx, "2.1"); err != nil {
			t.Fatalf("the read of the merge queue failed, want it to succeed once GitHub answers: %v", err)
		}
		if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			since, recorded, readErr, err = rig.records.SignalInputStatus(ctx, tx, queueProject, signals.Queue)
			return err
		}); err != nil {
			t.Fatalf("read signal input status after boot 2: %v", err)
		}
		if !recorded || !since.Equal(bootAt) || readErr != "" {
			t.Fatalf("after boot 2 status = (since %s, recorded %t, err %q), want (%s, true, \"\")", since, recorded, readErr, bootAt)
		}
		wantWaits := []signals.Wait{
			{Added: mustTime("2026-11-23T02:27:46Z"), Left: mustTime("2026-11-23T18:25:55Z")},
			{Added: mustTime("2026-11-23T18:00:00Z")},
		}
		if got := rig.waitsAt(rig.now); !reflect.DeepEqual(got, wantWaits) {
			t.Fatalf("waits after boot 2 = %+v, want %+v", got, wantWaits)
		}
		mu.Lock()
		reads := append([]string(nil), timelineReads...)
		mu.Unlock()
		for _, path := range reads {
			if strings.Contains(path, "/7001/") || strings.Contains(path, "/7002/") {
				t.Errorf("boot read timeline for out-of-scope PR: %s", path)
			}
		}

		// Boot 3 (restart over same store): gives the exact same waits.
		rig.now = rig.now.Add(time.Minute)
		if _, err := intake.ApplyFact(ctx, rig.pool, "daemon", "boot:LEGION:3", intake.DaemonBooted{Boot: 3}, rig.w.handlers...); err != nil {
			t.Fatalf("apply boot 3: %v", err)
		}
		if err := rig.w.readQueueOnce(ctx, "3.1"); err != nil {
			t.Fatalf("the read of the merge queue after a restart failed, want it to succeed: %v", err)
		}
		if got := rig.waitsAt(rig.now); !reflect.DeepEqual(got, wantWaits) {
			t.Fatalf("waits after restart (boot 3) = %+v, want %+v", got, wantWaits)
		}
	})

	// The boot's read is made again for as long as it fails, so a queue it could not read is read
	// once GitHub answers, and not at the next boot: until then the queue input holds the reason.
	// The first reads fail on the queue's timeline of a pull request, not on the queue itself, so
	// the empty set of a snapshot that listed it is never taken for a queue with no entry.
	t.Run("queue-retries", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		var timelineRefusals atomic.Int32
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/graphql":
				_, _ = io.WriteString(w, `{"data":{"repository":{"mergeQueue":{"entries":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[`+
					`{"enqueuedAt":"2026-11-23T02:27:46Z","pullRequest":{"number":5150,"headRefName":"legion/LEGION-5150"}}]}}}}}`)
			case "/repos/cybermonklabs/legion/issues/5150/timeline":
				if timelineRefusals.Add(1) <= 2 {
					http.Error(w, `{"message":"Bad Gateway"}`, http.StatusBadGateway)
					return
				}
				_, _ = w.Write(fixture5150)
			default:
				http.NotFound(w, r)
			}
		}))
		defer api.Close()
		bootAt := mustTime("2026-11-23T19:00:00Z")
		rig := newQueueTimelineRig(t, api.URL, bootAt, ghrepo.Renames{})
		rig.seedIssuePR("LEGION-5150", queueProject, "cybermonklabs/legion", 5150, "legion/LEGION-5150")
		if _, err := intake.ApplyFact(ctx, rig.pool, "daemon", "boot:LEGION:1", intake.DaemonBooted{Boot: 1}, rig.w.handlers...); err != nil {
			t.Fatalf("apply the boot: %v", err)
		}
		status := func() string {
			var readError string
			if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				_, _, readError, err = rig.records.SignalInputStatus(ctx, tx, queueProject, signals.Queue)
				return err
			}); err != nil {
				t.Errorf("read the status of the queue input: %v", err)
			}
			return readError
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			retryUntilApplied(ctx, 10*time.Millisecond, 20*time.Millisecond, func(ctx context.Context, attempt int) error {
				return rig.w.readQueueOnce(ctx, fmt.Sprintf("1.%d", attempt))
			}, nil)
		}()
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("the boot's read of the merge queue never succeeded")
		}
		if got := timelineRefusals.Load(); got != 3 {
			t.Fatalf("GitHub was asked for the timeline %d times, want the 2 it refused and the one it answered", got)
		}
		if readError := status(); readError != "" {
			t.Fatalf("after the read succeeded the queue input holds the reason %q, want none", readError)
		}
		want := []signals.Wait{{Added: mustTime("2026-11-23T02:27:46Z"), Left: mustTime("2026-11-23T18:25:55Z")}}
		if got := rig.waitsAt(bootAt); !reflect.DeepEqual(got, want) {
			t.Fatalf("waits = %+v, want %+v", got, want)
		}
	})

	// A failed read is never an empty queue: the reason it holds is what the queue input shows, and
	// the waits the store holds stand as they were.
	t.Run("queue-failed-read-keeps-the-waits-and-says-why", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/graphql" {
				_, _ = io.WriteString(w, `{"data":{"repository":null}}`)
				return
			}
			http.NotFound(w, r)
		}))
		defer api.Close()
		bootAt := mustTime("2026-11-23T19:00:00Z")
		rig := newQueueTimelineRig(t, api.URL, bootAt, ghrepo.Renames{})
		rig.seedIssuePR("LEGION-5150", queueProject, "cybermonklabs/legion", 5150, "legion/LEGION-5150")
		if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
			return rig.records.KeepQueueEvents(ctx, tx, queueProject, []signals.QueueEvent{
				{ID: 36963393320, Repo: "cybermonklabs/legion", PullRequest: 5150, At: mustTime("2026-11-23T02:27:46Z"), Enters: true},
			}, bootAt)
		}); err != nil {
			t.Fatalf("seed the open wait: %v", err)
		}
		if rig.w.readQueueOnce(ctx, "1.1") == nil {
			t.Fatal("the read succeeded though GitHub answered no repository, want it failed")
		}
		var readError string
		if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			_, _, readError, err = rig.records.SignalInputStatus(ctx, tx, queueProject, signals.Queue)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(readError, "no repository") {
			t.Fatalf("the queue input holds the reason %q, want the one GitHub's answer gave", readError)
		}
		want := []signals.Wait{{Added: mustTime("2026-11-23T02:27:46Z")}}
		if got := rig.waitsAt(bootAt); !reflect.DeepEqual(got, want) {
			t.Fatalf("waits after the failed read = %+v, want the open wait as it stood", got)
		}
	})

	// A timeline GitHub answers is not there (404) at the boot's read is an answer no later read
	// changes: the read forgets the events kept of that pull request and goes on to the next one, so
	// the queue is recorded, and the next boot does not read that timeline again.
	t.Run("queue-gone-open-wait", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		var mu sync.Mutex
		var timelineReads []string
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/graphql" {
				_, _ = io.WriteString(w, `{"data":{"repository":{"mergeQueue":{"entries":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[`+
					`{"enqueuedAt":"2026-11-23T18:00:00Z","pullRequest":{"number":6001,"headRefName":"legion/LEGION-6001"}}]}}}}}`)
				return
			}
			mu.Lock()
			timelineReads = append(timelineReads, r.URL.Path)
			mu.Unlock()
			if r.URL.Path == "/repos/cybermonklabs/legion/issues/6001/timeline" {
				_, _ = io.WriteString(w, `[{"id":990001,"event":"added_to_merge_queue","created_at":"2026-11-23T18:00:00Z","url":"https://api.github.com/repos/cybermonklabs/legion/issues/events/990001"}]`)
				return
			}
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}))
		defer api.Close()
		bootAt := mustTime("2026-11-23T19:00:00Z")
		rig := newQueueTimelineRig(t, api.URL, bootAt, ghrepo.Renames{})
		rig.seedIssuePR("LEGION-5150", queueProject, "cybermonklabs/legion", 5150, "legion/LEGION-5150")
		if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
			return rig.records.KeepQueueEvents(ctx, tx, queueProject, []signals.QueueEvent{
				{ID: 36963393320, Repo: "cybermonklabs/legion", PullRequest: 5150, At: mustTime("2026-11-23T02:27:46Z"), Enters: true},
			}, bootAt)
		}); err != nil {
			t.Fatalf("seed the open wait: %v", err)
		}
		readError := func() string {
			var readError string
			if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				_, _, readError, err = rig.records.SignalInputStatus(ctx, tx, queueProject, signals.Queue)
				return err
			}); err != nil {
				t.Fatalf("read the status of the queue input: %v", err)
			}
			return readError
		}
		for boot := 1; boot <= 2; boot++ {
			mu.Lock()
			timelineReads = nil
			mu.Unlock()
			if _, err := intake.ApplyFact(ctx, rig.pool, "daemon", fmt.Sprintf("boot:LEGION:%d", boot), intake.DaemonBooted{Boot: int64(boot)}, rig.w.handlers...); err != nil {
				t.Fatalf("apply boot %d: %v", boot, err)
			}
			if err := rig.w.readQueueOnce(ctx, fmt.Sprintf("%d.1", boot)); err != nil {
				t.Fatalf("boot %d: the read of the merge queue failed, want it to succeed past the timeline GitHub answers is gone: %v", boot, err)
			}
			if got := readError(); got != "" {
				t.Fatalf("after boot %d the queue input holds the reason %q, want the queue recorded", boot, got)
			}
			if got, want := rig.waitsAt(bootAt), []signals.Wait{{Added: mustTime("2026-11-23T18:00:00Z")}}; !reflect.DeepEqual(got, want) {
				t.Fatalf("after boot %d the waits = %+v, want #6001's alone, #5150's forgotten", boot, got)
			}
			mu.Lock()
			reads := append([]string(nil), timelineReads...)
			mu.Unlock()
			wantReads := []string{"/repos/cybermonklabs/legion/issues/6001/timeline"}
			if boot == 1 {
				wantReads = append([]string{"/repos/cybermonklabs/legion/issues/5150/timeline"}, wantReads...)
			}
			if !reflect.DeepEqual(reads, wantReads) {
				t.Fatalf("boot %d read the timelines %v, want %v", boot, reads, wantReads)
			}
		}
	})

	// The boot's read of Dispatch for the approvals signal (signal_approvals_test.go).
	t.Run("asks", testSignalBootReconcilesAsks)
}

// The queue signal of a moment reads the pull requests that stand in the queue and those that left
// it in the 6 hours before, so the store keeps no other: a pull request that left 6 hours or more
// before the moment a timeline is kept at is forgotten, and a pull request with no exit stays
// however old its entry is. The events of one second keep GitHub's order whatever their ids, which
// is what decides whether a pull request added and removed in that second stands in the queue.
func TestSignalQueueEventsAreBoundedByTheWindow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	event := func(id int64, pull int, ago time.Duration, enters bool) signals.QueueEvent {
		return signals.QueueEvent{ID: id, Repo: "cybermonklabs/legion", PullRequest: pull, At: now.Add(-ago), Enters: enters}
	}
	keep := func(at time.Time, events ...signals.QueueEvent) {
		t.Helper()
		if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
			return records.KeepQueueEvents(ctx, tx, queueProject, events, at)
		}); err != nil {
			t.Fatalf("keep the queue events of #%d: %v", events[0].PullRequest, err)
		}
	}
	held := func() []int {
		t.Helper()
		rows, err := pool.Query(ctx, `select distinct pull_request from queue_events where project = $1 order by pull_request`, queueProject)
		if err != nil {
			t.Fatalf("list the pull requests the store holds: %v", err)
		}
		pulls, err := pgx.CollectRows(rows, pgx.RowTo[int])
		if err != nil {
			t.Fatalf("list the pull requests the store holds: %v", err)
		}
		return pulls
	}
	// #1 left 7 hours ago, #2 left 5 hours ago, and #3 has stood in the queue for three days.
	keep(now, event(1, 1, 30*time.Hour, true), event(2, 1, 7*time.Hour, false))
	keep(now, event(3, 2, 9*time.Hour, true), event(4, 2, 5*time.Hour, false))
	keep(now, event(5, 3, 72*time.Hour, true))
	if got, want := held(), []int{2, 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after the pull request that left 7 hours ago was kept, the store holds %v, want %v", got, want)
	}
	read := func(at time.Time) []signals.Wait {
		t.Helper()
		var events []signals.QueueEvent
		if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			events, err = records.QueueEventsAt(ctx, tx, queueProject, at)
			return err
		}); err != nil {
			t.Fatalf("read the queue events at %s: %v", at, err)
		}
		return signals.WaitsOf(events)
	}
	want := []signals.Wait{{Added: now.Add(-72 * time.Hour)}, {Added: now.Add(-9 * time.Hour), Left: now.Add(-5 * time.Hour)}}
	if got := read(now); !reflect.DeepEqual(got, want) {
		t.Fatalf("waits at %s = %+v, want %+v", now, got, want)
	}

	// Two hours on, #2 left 7 hours before the moment a new pull request is kept at: it is forgotten,
	// and #3, which never left, stands.
	later := now.Add(2 * time.Hour)
	keep(later, signals.QueueEvent{ID: 6, Repo: "cybermonklabs/legion", PullRequest: 4, At: later, Enters: true})
	if got, want := held(), []int{3, 4}; !reflect.DeepEqual(got, want) {
		t.Fatalf("two hours on, the store holds %v, want %v", got, want)
	}

	// #5 was added and removed in one second, the removal's id the lower of the two: the events keep
	// the order the timeline gave them, so #5 left the queue, which ordering them by id would not
	// show (the removal first would leave nothing to remove, and the entry after it would stand).
	// #4 was added at the same moment and stands.
	keep(later, signals.QueueEvent{ID: 20, Repo: "cybermonklabs/legion", PullRequest: 5, At: later, Enters: true},
		signals.QueueEvent{ID: 19, Repo: "cybermonklabs/legion", PullRequest: 5, At: later, Enters: false})
	want = []signals.Wait{{Added: now.Add(-72 * time.Hour)}, {Added: later}, {Added: later, Left: later}}
	if got := read(later); !reflect.DeepEqual(got, want) {
		t.Fatalf("waits at %s = %+v, want %+v", later, got, want)
	}
}
