package main

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// `broker versions` is the check an operator runs before an image with another set of migrations
// rolls out, where the broker's settings reach its database (operate.md, "Running a subcommand"):
// it reads those settings, signs in as a broker pod does, and
// says what this image's next boot would do with each recorded migration, writing nothing. The
// control is a database this image migrated, which reads ok, exit 0; a row recording another file
// under a version reads REFUSED, exit 1, and stays as it was; an unmigrated database reads as
// nothing checked, exit 1, and gains no table; and an argument is a usage error, exit 2.
func TestTheBinaryVersionsJudgesTheRecordedMigrationsAndWritesNothing(t *testing.T) {
	ctx := context.Background()
	binary := buildBroker(t)

	unmigrated := storetest.DatabaseURL(t)
	out := runBrokerCommand(t, binary, unmigrated, "http://127.0.0.1:9", 1, versionsCommand)
	if !strings.Contains(out, "versions: NOTHING CHECKED: broker_schema_migrations records no version") {
		t.Errorf("an unmigrated database does not read as nothing checked:\n%s", out)
	}
	st, err := store.Open(ctx, unmigrated)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var tables int
	if err := st.Pool.QueryRow(ctx, `select count(*) from information_schema.tables where table_schema = 'public'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Errorf("the subcommand left %d tables behind, want a database it never migrated", tables)
	}

	migrated := storetest.DatabaseURL(t)
	m, err := store.Open(ctx, migrated)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	out = runBrokerCommand(t, binary, migrated, "http://127.0.0.1:9", 0, versionsCommand)
	if !strings.HasSuffix(strings.TrimSpace(out), "versions: ok") {
		t.Errorf("control, a database this image migrated, does not read ok:\n%s", out)
	}

	if _, err := m.Pool.Exec(ctx, "update broker_schema_migrations set name = '0001_another_branch.up.sql' where version = 1"); err != nil {
		t.Fatalf("record another file under version 1: %v", err)
	}
	out = runBrokerCommand(t, binary, migrated, "http://127.0.0.1:9", 1, versionsCommand)
	if !strings.Contains(out, "versions: REFUSED version 1 is recorded as 0001_another_branch.up.sql") {
		t.Errorf("a row recording another file is not refused:\n%s", out)
	}
	var name string
	if err := m.Pool.QueryRow(ctx, "select name from broker_schema_migrations where version = 1").Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "0001_another_branch.up.sql" {
		t.Errorf("the check changed the row it refused to %q; it must write nothing", name)
	}

	out = runBrokerCommand(t, binary, migrated, "http://127.0.0.1:9", 2, versionsCommand, "extra")
	if !strings.Contains(out, "usage: broker versions, with no argument") {
		t.Errorf("an argument is not refused with the usage line:\n%s", out)
	}
}
