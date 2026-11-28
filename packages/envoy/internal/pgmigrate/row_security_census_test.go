package pgmigrate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/testpg/testdb"
)

// The three statement forms of plan D7's row-security migrations, each on the catalog-only
// fixture's big (above the one-byte limit) or its view big_view over big.
const (
	enableRowSecurity  = "alter table big enable row level security"
	createViewerPolicy = "create policy viewer on big for all to public using (kind <> '') with check (kind <> '')"
	viewAsInvoker      = "alter view big_view set (security_invoker = on)"
)

// TestTheCensusAdmitsRowSecurityOnALargeTable (plan D7, R2): ALTER TABLE … ENABLE ROW LEVEL
// SECURITY, CREATE POLICY … ON and ALTER VIEW … SET (security_invoker = …) change the catalog
// alone, so a migration of them on a table above the limit is admitted with no refusal, the
// census still sizing the table; a policy is read as touching its table, a view is no table the
// size rule reads (none of its rows is counted); each keeps the catalog reads of a later pending
// migration, whose added column is still admitted. FORCE ROW LEVEL SECURITY is no form of these
// migrations, and is still held to the limit, alone and beside ENABLE.
func TestTheCensusAdmitsRowSecurityOnALargeTable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := catalogOnlyDatabase(t, ctx, 1)
	for _, row := range []struct {
		name string
		sql  string
		// kind is the exemption words the migration must carry; "" where any words do.
		kind string
	}{
		{"enable row level security", enableRowSecurity, "alter table"},
		{"create policy on the table", createViewerPolicy, ""},
		{"alter view set security_invoker, beside an added column", paired(viewAsInvoker + ";"), ""},
		{"all three in one migration", enableRowSecurity + "; " + createViewerPolicy + "; " + viewAsInvoker, ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			entry := censusOf(t, ctx, conn, []string{row.sql})[0]
			if entry.TableSizeExemption == "" || row.kind != "" && entry.TableSizeExemption != row.kind || len(entry.Refusals) != 0 {
				t.Fatalf("%s: exemption %q, refusals %q; want it exempt as catalog-only (%q) and no refusal", entry.Migration.Name, entry.TableSizeExemption, entry.Refusals, row.kind)
			}
			if !sizesATableAboveTheLimit(entry) {
				t.Fatalf("%s sized no table above the limit, so the exemption proved nothing: %+v", entry.Migration.Name, entry.Tables)
			}
			for _, table := range entry.Tables {
				if table.Name == "big_view" && table.Rows != 0 {
					t.Errorf("%s counted %d rows of the view big_view: a view is no table the size rule reads", entry.Migration.Name, table.Rows)
				}
			}
		})
	}
	for _, form := range []struct{ name, sql string }{
		{"enable row level security", enableRowSecurity},
		{"create policy", createViewerPolicy},
		{"alter view set security_invoker", viewAsInvoker},
	} {
		t.Run("an added column after "+form.name+" keeps its catalog reads", func(t *testing.T) {
			pending := censusOf(t, ctx, conn, []string{form.sql, addColumn})
			if first := pending[0]; first.TableSizeExemption == "" || len(first.Refusals) != 0 {
				t.Fatalf("%s: exemption %q, refusals %q; want it exempt with no refusal", first.Migration.Name, first.TableSizeExemption, first.Refusals)
			}
			if entry := pending[1]; entry.TableSizeExemption != "alter table" || len(entry.Refusals) != 0 || !sizesATableAboveTheLimit(entry) {
				t.Fatalf("the added column after %s: exemption %q, refusals %q; want it exempt as alter table with no refusal, as alone: the form must keep the later catalog reads",
					form.name, entry.TableSizeExemption, entry.Refusals)
			}
		})
	}
	for _, refused := range []struct{ name, sql string }{
		{"force row level security", "alter table big force row level security"},
		{"enable and force row level security", "alter table big enable row level security, force row level security"},
	} {
		t.Run(refused.name+" is still refused", func(t *testing.T) {
			entry := censusOf(t, ctx, conn, []string{refused.sql})[0]
			if entry.TableSizeExemption != "" || !refusalSays(entry, aboveTheLimit) {
				t.Fatalf("%s: exemption %q, refusals %q; want no exemption and a refusal on size", entry.Migration.Name, entry.TableSizeExemption, entry.Refusals)
			}
		})
	}
}

// ownersRowSecurityFixtureSQL is the database the bounded-write rows of row security judge their
// writes against: three tables above the one-byte limit, each with row security and a policy no
// row passes. owned_rls is the login's and does not force it; forced_rls is the login's and forces
// it; other_rls is given to <role>, another role, after the fixture lets that role create a table.
const ownersRowSecurityFixtureSQL = `
create table schema_migrations (version bigint primary key);
insert into schema_migrations (version) values (1);
create table owned_rls (id int primary key, note text not null);
create table forced_rls (id int primary key, note text not null);
create table other_rls (id int primary key, note text not null);
insert into owned_rls select g, 'x' from generate_series(1, 10) g;
insert into forced_rls select g, 'x' from generate_series(1, 10) g;
insert into other_rls select g, 'x' from generate_series(1, 10) g;
alter table owned_rls enable row level security;
create policy viewer on owned_rls using (false) with check (false);
alter table forced_rls enable row level security, force row level security;
create policy viewer on forced_rls using (false) with check (false);
alter table other_rls enable row level security;
create policy viewer on other_rls using (false) with check (false);
grant create on schema public to <role>;
alter table other_rls owner to <role>;
`

// anotherOwner creates a NOLOGIN role for the test, grants it to the test's login with SET, so the
// login may give it a table, and drops it when the test ends. The cleanup is registered before
// the caller makes its database, so the database's drop, which takes the role's table and grant
// with it, runs first. A login that may not create a role skips the test, and fails it on GitHub
// Actions, whose server's login may (testdb.SkipOutsideCI).
func anotherOwner(t *testing.T, ctx context.Context) string {
	t.Helper()
	raw := os.Getenv("DISPATCH_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("DISPATCH_TEST_DATABASE_URL must be set to run the census's row-security bounded-write tests")
	}
	adminURL, err := url.Parse(raw)
	if err != nil {
		// url.Parse's own error quotes the URL, which carries a credential.
		t.Fatal("DISPATCH_TEST_DATABASE_URL is not a URL")
	}
	adminURL.Path = "/postgres"
	admin := connect(t, ctx, testdb.WithConnectDeadline(t, adminURL.String()))
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	role := "pgmigrate_rls_owner_" + hex.EncodeToString(suffix)
	if _, err := admin.Exec(ctx, "create role "+role+" nologin"); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42501" {
			testdb.SkipOutsideCI(t, "the test server's login may not create a role (it needs CREATEROLE): %v", err)
		}
		t.Fatalf("create the role %s: %v", role, err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "drop role if exists "+role); err != nil {
			t.Errorf("drop the role %s: %v", role, err)
		}
	})
	if _, err := admin.Exec(ctx, "grant "+role+" to current_user with set true"); err != nil {
		t.Fatalf("grant the role %s to the test's login: %v", role, err)
	}
	return role
}

// TestABoundedWriteIntoAnOwnersRowSecurityTableIsAdmitted (plan D7, R2): a bounded write into a
// table above the limit that has row security is admitted when the migrating login owns the table
// and the table does not force row security, since the owner's write meets no policy; the
// migration then runs on the fixture as the owner, past a policy no row passes. The same write is
// still refused into a table that forces row security, and into one another role owns, whose
// policies the write would meet for each row.
func TestABoundedWriteIntoAnOwnersRowSecurityTableIsAdmitted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	role := anotherOwner(t, ctx)
	conn := fixtureDatabase(t, ctx, "pgmigrate_rls_bounded", "row-security bounded-write tests", strings.ReplaceAll(ownersRowSecurityFixtureSQL, "<role>", role))
	for _, row := range []struct{ name, sql, table string }{
		{"an insert of a literal row into the owner's table", "insert into owned_rls (id, note) values (11, 'y')", "owned_rls"},
		{"an update by primary key of the owner's table", "update owned_rls set note = 'z' where id = 1", "owned_rls"},
	} {
		t.Run(row.name, func(t *testing.T) {
			entry := censusOf(t, ctx, conn, []string{row.sql})[0]
			table, ok := tableNamed(entry, row.table)
			if !ok || !table.Exists || table.Bytes <= 1 {
				t.Fatalf("the census read %s as %+v (found %v); want it in the database and above the one-byte limit", row.table, table, ok)
			}
			if entry.TableSizeExemption != "" || len(entry.Refusals) != 0 || table.BoundedWrites != 1 {
				t.Fatalf("exemption %q, refusals %q, bounded writes %d; want no exemption, no refusal and %s admitted for 1 bounded write",
					entry.TableSizeExemption, entry.Refusals, table.BoundedWrites, row.table)
			}
			tx, err := conn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			tag, err := tx.Exec(ctx, row.sql)
			if err != nil || tag.RowsAffected() != 1 {
				t.Fatalf("the admitted write on the fixture, as the table's owner: %v, %d rows; want one row written past the policy", err, tag.RowsAffected())
			}
		})
	}
	for _, row := range []struct{ name, sql, table string }{
		{"an insert into a table that forces row security", "insert into forced_rls (id, note) values (11, 'y')", "forced_rls"},
		{"an update by primary key of a table that forces row security", "update forced_rls set note = 'z' where id = 1", "forced_rls"},
		{"an insert into another role's table", "insert into other_rls (id, note) values (11, 'y')", "other_rls"},
		{"an update by primary key of another role's table", "update other_rls set note = 'z' where id = 1", "other_rls"},
	} {
		t.Run(row.name+" is refused", func(t *testing.T) {
			entry := censusOf(t, ctx, conn, []string{row.sql})[0]
			table, ok := tableNamed(entry, row.table)
			if !ok || !table.Exists || table.Bytes <= 1 {
				t.Fatalf("the census read %s as %+v (found %v); want it in the database and above the one-byte limit", row.table, table, ok)
			}
			if entry.TableSizeExemption != "" || table.BoundedWrites != 0 || !refusalSays(entry, row.table+" is ", aboveTheLimit, boundedWritesRule, "row-level security") {
				t.Fatalf("exemption %q, bounded writes %d, refusals %q; want %s refused on its size for its row-level security",
					entry.TableSizeExemption, table.BoundedWrites, entry.Refusals, row.table)
			}
		})
	}
}
