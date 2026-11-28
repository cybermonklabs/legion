package store

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// agentTokenScopesP0Version is the migration that lets agent_tokens.scopes hold p0
// (INFRA-5637).
const agentTokenScopesP0Version = 20261126005333

// TestTheP0MigrationKeepsEveryStoredTokensScopesAndTakesP0: on a database at the migration before
// it, holding personal tokens of each set an earlier release stores (person, person and approve,
// none) and a read-only token holding read and person, the census counts no row the new check
// refuses, and the migration gives no stored token p0: each row holds what it held. After it a
// token may hold p0 alone or beside person and approve, and a token that names no scopes still
// takes person and approve from the column's default, never p0. The check still refuses a word
// that is no scope, approve without person, and read beside p0, by its own name
// (agent_tokens_scopes_check, which api's TestNoMintHoldsLegionStatus reads), and the read-only
// table still refuses p0.
func TestTheP0MigrationKeepsEveryStoredTokensScopesAndTakesP0(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openMigratedTestStore(t, agentTokenScopesP0Version-1)
	stored := map[string][]string{
		"person":             {"person"},
		"person and approve": {"person", "approve"},
		"no scope":           {},
	}
	for name, scopes := range stored {
		if err := insertAgentTokenRow(ctx, store, "agent_tokens", name, scopes); err != nil {
			t.Fatalf("seed the token holding %s before the migration: %v", name, err)
		}
	}
	readOnly := []string{"read", "person"}
	if err := insertAgentTokenRow(ctx, store, "agent_read_tokens", "read and person", readOnly); err != nil {
		t.Fatalf("seed the read-only token before the migration: %v", err)
	}

	if got := censusCount(t, store, agentTokenScopesP0Version); got != 0 {
		t.Fatalf("census = %d, want 0: the check being replaced is narrower, so no stored token breaks the new one", got)
	}
	if err := store.migrate(ctx, migrationsThrough(t, agentTokenScopesP0Version)); err != nil {
		t.Fatalf("migrate to %d: %v", agentTokenScopesP0Version, err)
	}

	for name, want := range stored {
		if got := agentTokenRowScopes(t, store, "agent_tokens", name); !slices.Equal(got, want) {
			t.Errorf("the token holding %s holds %q after the migration, want %q: no migration gives a stored token p0", name, got, want)
		}
	}
	if got := agentTokenRowScopes(t, store, "agent_read_tokens", "read and person"); !slices.Equal(got, readOnly) {
		t.Errorf("the read-only token holds %q after the migration, want %q", got, readOnly)
	}

	for name, scopes := range map[string][]string{
		"p0 alone":            {"p0"},
		"person, approve, p0": {"person", "approve", "p0"},
	} {
		if err := insertAgentTokenRow(ctx, store, "agent_tokens", name, scopes); err != nil {
			t.Errorf("an insert of a token holding %s after the migration: %v, want it written", name, err)
			continue
		}
		if got := agentTokenRowScopes(t, store, "agent_tokens", name); !slices.Equal(got, scopes) {
			t.Errorf("the token holding %s holds %q, want %q", name, got, scopes)
		}
	}

	if _, err := store.Pool.Exec(ctx, `
		insert into agent_tokens (owner, name, token_hash, prefix)
		values ('alice', 'the default', $1, 'probe')
	`, []byte("the default")); err != nil {
		t.Fatalf("an insert that names no scopes after the migration: %v", err)
	}
	if got, want := agentTokenRowScopes(t, store, "agent_tokens", "the default"), []string{"person", "approve"}; !slices.Equal(got, want) {
		t.Errorf("a token that names no scopes holds %q, want the column's default %q: no default gives p0", got, want)
	}

	for name, scopes := range map[string][]string{
		"a word that is no scope": {"admin"},
		"approve alone":           {"approve"},
		"read and p0":             {"read", "p0"},
	} {
		err := insertAgentTokenRow(ctx, store, "agent_tokens", name, scopes)
		var refusal *pgconn.PgError
		if !errors.As(err, &refusal) || refusal.Code != "23514" || refusal.ConstraintName != "agent_tokens_scopes_check" {
			t.Errorf("an insert of a token holding %s (%q): %v, want SQLSTATE 23514 naming agent_tokens_scopes_check", name, scopes, err)
		}
	}
	if err := insertAgentTokenRow(ctx, store, "agent_read_tokens", "read and p0", []string{"read", "p0"}); !isSQLState(err, "23514") {
		t.Errorf("an insert of a read-only token holding read and p0: %v, want SQLSTATE 23514", err)
	}
}

// insertAgentTokenRow writes one of alice's tokens named name, holding scopes, straight into table
// (agent_tokens or agent_read_tokens), as an earlier release's mint or a repair by hand would. The
// name is the token's hash too, so each row of a test is a token of its own.
func insertAgentTokenRow(ctx context.Context, store *Store, table, name string, scopes []string) error {
	statement := `insert into agent_tokens (owner, name, token_hash, prefix, scopes) values ('alice', $1, $2, 'probe', $3)`
	if table == "agent_read_tokens" {
		statement = `insert into agent_read_tokens (owner, name, token_hash, prefix, scopes) values ('alice', $1, $2, 'probe', $3)`
	}
	_, err := store.Pool.Exec(ctx, statement, name, []byte(name), scopes)
	return err
}

// agentTokenRowScopes is the scopes the token named name holds in table.
func agentTokenRowScopes(t *testing.T, store *Store, table, name string) []string {
	t.Helper()
	statement := `select scopes from agent_tokens where name = $1`
	if table == "agent_read_tokens" {
		statement = `select scopes from agent_read_tokens where name = $1`
	}
	var scopes []string
	if err := store.Pool.QueryRow(context.Background(), statement, name).Scan(&scopes); err != nil {
		t.Fatalf("read the scopes of %s's token %q: %v", table, name, err)
	}
	return scopes
}
