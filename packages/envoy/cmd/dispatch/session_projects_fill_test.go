package main

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// appendEach appends each event in a transaction of its own through the broker, as a route does.
func appendEach(t *testing.T, database *store.Store, list ...model.Event) {
	t.Helper()
	ctx := context.Background()
	broker := events.NewBroker()
	for _, event := range list {
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := broker.Append(ctx, tx, event); err != nil {
			t.Fatalf("append %s by %+v: %v", event.Type, event.Actor, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
}
