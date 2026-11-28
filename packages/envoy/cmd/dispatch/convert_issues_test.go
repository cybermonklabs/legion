package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestTheConversionRefusesWhileAnotherBuildIsLive(t *testing.T) {
	database := conversionCommandDatabase(t)
	conversionCommandExec(t, database, `update issues set status = 'done', closed_at = now()`)
	conversionCommandExec(t, database, `insert into server_processes (instance, commit, signing_keys, schema_version, schema_creatable, heartbeat_at, owner_lock_protocol)
		values ('00000000-0000-4000-8000-000000000001', 'another-build', '{fingerprint}', $1, $2, now(), $3)`,
		pmdoc.SchemaVersion(), pmdoc.SchemaCreatable(), store.OwnerLockProtocol)
	code, output := runConversionCommand(t, database, "--project", "CUT")
	if code != 1 || !strings.Contains(output, "another-build") || !strings.Contains(output, "00000000-0000-4000-8000-000000000001") || strings.Contains(output, "CUT-1 passed over") {
		t.Fatalf("exit %d, want refusal before the done issues, naming the live build and instance:\n%s", code, output)
	}
}

func conversionCommandDatabase(t *testing.T) *store.Store {
	t.Helper()
	database := storetest.Open(t)
	old := buildCommit
	buildCommit = "conversion-test-build"
	t.Cleanup(func() { buildCommit = old })
	conversionCommandExec(t, database, `insert into server_build_announcements (commit) values ($1)`, buildCommit)
	conversionCommandExec(t, database, `insert into projects (key, name, default_assignee) values ('CUT', 'Cutover', 'alice')`)
	conversionCommandExec(t, database, `insert into issues (key, project_key, number, title, created_by, rank) values
		('CUT-1', 'CUT', 1, 'Large issue', '{"kind":"user","id":"alice"}', 'U'),
		('CUT-2', 'CUT', 2, 'Small issue', '{"kind":"user","id":"alice"}', 'V')`)
	return database
}

func conversionCommandExec(t *testing.T, database *store.Store, sql string, args ...any) {
	t.Helper()
	if _, err := database.Pool.Exec(t.Context(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func runConversionCommand(t *testing.T, database *store.Store, args ...string) (int, string) {
	t.Helper()
	var out bytes.Buffer
	code := convertIssues(t.Context(), args, envGetter(map[string]string{"DATABASE_URL": database.Pool.Config().ConnString()}), &out, &out)
	return code, out.String()
}
