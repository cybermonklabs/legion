package api

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func allEvents(t *testing.T, database *store.Store) int64 {
	t.Helper()
	return countRows(t, database, `select count(*) from events`)
}

func projectOwnerOf(t *testing.T, database *store.Store, key string) string {
	t.Helper()
	var owner string
	if err := database.Pool.QueryRow(context.Background(), `select coalesce(owner, '') from projects where key = $1`, key).Scan(&owner); err != nil {
		t.Fatalf("read %s's owner: %v", key, err)
	}
	return owner
}
