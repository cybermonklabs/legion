package store

import (
	"context"
	"testing"
)

func TestPgSessionStoreAdvancesGenerationOnRevocation(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t)
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	sessions := NewPgSessionStore(database.Pool)
	login := "session-" + randomDatabaseSuffix(t)
	// A new session names an address people holds (migration 20261118041916's key), as every
	// sign-in records the person before it makes the session.
	if _, err := database.Pool.Exec(ctx, `insert into people (email) values ($1)`, login); err != nil {
		t.Fatalf("record the person: %v", err)
	}

	generation, err := sessions.EnsureSession(ctx, login)
	if err != nil {
		t.Fatalf("ensure initial generation: %v", err)
	}
	if generation != 0 {
		t.Fatalf("initial generation = %d, want 0", generation)
	}
	if err := sessions.RevokeSessions(ctx, login); err != nil {
		t.Fatalf("revoke sessions: %v", err)
	}
	generation, found, err := sessions.CurrentSessionGeneration(ctx, login)
	if err != nil {
		t.Fatalf("read revoked generation: %v", err)
	}
	if !found || generation != 1 {
		t.Fatalf("revoked generation = %d, found=%t, want 1, true", generation, found)
	}
}
