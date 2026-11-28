package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/signals"
	"github.com/sjawhar/legion/daemon/internal/store"
)

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
