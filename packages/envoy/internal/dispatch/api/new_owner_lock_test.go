package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A write that creates an owner - a project, or an issue - appends its creation event under the
// new owner's lock, and nothing took that lock in its read phase, since the owner did not exist
// yet: the write phase's take (events' lockEventOwner, store.QueueLockOwner) is the first. A
// transaction that holds that lock's key - one that named the key before the owner was made, or
// another owner whose key hashes to the same value - makes the take wait. It waits no longer than
// any other lock a write waits for (the store's lockWait): it is refused 503 DATABASE_BUSY with
// Retry-After, and the write lands once the lock is free. An issue create's write phase runs in
// its ledger's commit, whose own bound (store.CommitContext) ends that wait too; a project create
// appends on its own transaction, where nothing else ends it (INFRA-592, tester round 47).
func TestACreateBehindItsNewOwnersHeldLockIsRefusedBusyAtItsBound(t *testing.T) {
	t.Parallel()
	const bound = time.Second
	for _, row := range []struct {
		name string
		// project, when it is set, is made before the lock is taken, and the create makes the
		// project's next issue.
		project string
		// owner is the owner the create makes, when project is empty.
		owner        store.Owner
		path         string
		body         map[string]any
		createdCount string
	}{
		{
			name:         "a project create behind its new project's lock",
			owner:        store.ProjectOwner("NEWP"),
			path:         "/api/v1/projects",
			body:         map[string]any{"key": "NEWP", "name": "Made behind a held lock"},
			createdCount: `select count(*) from projects where key = 'NEWP'`,
		},
		{
			name:         "an issue create behind its new issue's lock",
			project:      "TEST",
			path:         "/api/v1/issues",
			body:         map[string]any{"project": "TEST", "title": "Made behind a held lock", "force": true},
			createdCount: `select count(*) from issues where project_key = 'TEST'`,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
			ctx := context.Background()
			owner := row.owner
			if row.project != "" {
				if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
					"key": row.project, "name": row.project,
				}, "alice"); response.Code != http.StatusCreated {
					t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
				}
				var next int
				if err := database.Pool.QueryRow(ctx, `select next_number from projects where key = $1`, row.project).Scan(&next); err != nil {
					t.Fatalf("read the project's next issue number: %v", err)
				}
				owner = store.IssueOwner(row.project + "-" + strconv.Itoa(next))
			}
			send := func() *httptest.ResponseRecorder {
				data, err := json.Marshal(row.body)
				if err != nil {
					t.Fatalf("encode the create: %v", err)
				}
				request := httptest.NewRequest(http.MethodPost, row.path, strings.NewReader(string(data))).
					WithContext(store.WithLockWaitForTest(context.Background(), bound))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("X-Dispatch-User", "alice")
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				return response
			}
			count := func() int {
				t.Helper()
				var n int
				if err := database.Pool.QueryRow(ctx, row.createdCount).Scan(&n); err != nil {
					t.Fatalf("count what the create made: %v", err)
				}
				return n
			}
			before := count()

			holder, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin the holder: %v", err)
			}
			defer func() { _ = holder.Rollback(ctx) }()
			if _, err := holder.Exec(ctx, `select pg_advisory_xact_lock($1, hashtext($2::text))`, store.OwnerLockNamespace, owner.LockKey()); err != nil {
				t.Fatalf("hold %s's lock: %v", owner.LockKey(), err)
			}

			answered := make(chan *httptest.ResponseRecorder, 1)
			started := time.Now()
			go func() { answered <- send() }()
			if waiting := storetest.WaitForLockWaiters(t, holder, 1); waiting < 1 {
				t.Fatalf("the create never queued behind the lock of %s", owner.LockKey())
			}
			var response *httptest.ResponseRecorder
			select {
			case response = <-answered:
			case <-time.After(20 * time.Second):
				waited := time.Since(started)
				_ = holder.Rollback(ctx)
				<-answered
				t.Fatalf("the create was still waiting on the lock of %s, the owner it makes, %s after it was sent, past a write's lock "+
					"bound of %s and its commit's: the write phase's take of a new owner's lock waits until its holder lets go",
					owner.LockKey(), waited.Round(time.Millisecond), bound)
			}
			waited := time.Since(started)
			if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"code":"`+admission.CodeDatabaseBusy+`"`) ||
				response.Header().Get("Retry-After") == "" {
				t.Fatalf("the create behind its new owner's held lock: status=%d Retry-After=%q body=%s, want 503 %s with Retry-After",
					response.Code, response.Header().Get("Retry-After"), response.Body.String(), admission.CodeDatabaseBusy)
			}
			if waited < bound {
				t.Fatalf("the create was refused %s after it was sent, inside its bound of %s", waited.Round(time.Millisecond), bound)
			}
			if after := count(); after != before {
				t.Fatalf("the refused create left %d rows where there were %d", after, before)
			}

			if err := holder.Rollback(ctx); err != nil {
				t.Fatalf("let the lock go: %v", err)
			}
			if again := send(); again.Code != http.StatusCreated {
				t.Fatalf("the create sent again once the lock was free: status=%d body=%s, want 201", again.Code, again.Body.String())
			}
		})
	}
}
