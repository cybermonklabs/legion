package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestAListWriteNamingAGroupWaitsForTheGroupsLock (written by the A1 tester): the routes that put
// a group on a project's list and take it off must take the group's name lock before anything
// else, the order a delete of that group takes (access.LockForAccessWrite). The access package's
// own race tests (access/group_delete_race_test.go) call LockForAccessWrite themselves, so they
// pass even when a route drops the group from its lock call. Driven through the real server, such
// a route left 83 orphan list rows and 7 deadlocks (40P01, answered 500) in 200 rounds of a list
// write racing the group's delete. Here another transaction holds crew's name lock, as a delete of
// crew holds it while it sweeps; each list write naming crew must wait for that lock, and land
// once it is released.
func TestAListWriteNamingAGroupWaitsForTheGroupsLock(t *testing.T) {
	handler, database := newAccessServer(t)
	if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/access-groups/crew",
		map[string]any{"members": []string{accessCarol}}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("alice makes group crew: %d %s", response.Code, response.Body.String())
	}
	for _, write := range []struct{ method, name string }{
		{http.MethodPut, "put crew on ACC's list"},
		{http.MethodDelete, "take crew off ACC's list"},
	} {
		ctx := context.Background()
		held, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the transaction that holds crew's lock: %v", err)
		}
		if err := access.LockGroupName(ctx, held, "crew"); err != nil {
			_ = held.Rollback(ctx)
			t.Fatalf("hold crew's lock: %v", err)
		}
		answered := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			request := httptest.NewRequest(write.method, "/api/v1/projects/ACC/access/groups/crew", nil)
			request.Header.Set("X-Dispatch-User", "alice")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			answered <- response
		}()
		// The write waits for crew's lock once a backend waits on LockGroupName's statement; it
		// fails here if it answers first, and ten seconds with neither is a hang.
		deadline := time.Now().Add(10 * time.Second)
		for {
			select {
			case response := <-answered:
				_ = held.Rollback(ctx)
				t.Fatalf("%s answered %d %s while another transaction held crew's name lock; want it to wait for that lock, "+
					"which a delete of crew takes first, or the two race to an orphan row or a deadlock",
					write.name, response.Code, response.Body.String())
			default:
			}
			waiting, err := storetest.CountLockWaits(ctx, database, "%'access_group:'%")
			if err != nil {
				_ = held.Rollback(ctx)
				t.Fatalf("count the waiters on crew's name lock: %v", err)
			}
			if waiting > 0 {
				break
			}
			if time.Now().After(deadline) {
				_ = held.Rollback(ctx)
				t.Fatalf("%s neither answered nor waited on crew's name lock within 10s", write.name)
			}
			time.Sleep(5 * time.Millisecond)
		}
		if err := held.Rollback(ctx); err != nil {
			t.Fatalf("release crew's lock: %v", err)
		}
		select {
		case response := <-answered:
			if response.Code != http.StatusOK {
				t.Fatalf("%s once crew's lock was released: %d %s, want 200", write.name, response.Code, response.Body.String())
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("%s never answered after crew's lock was released", write.name)
		}
	}
}
