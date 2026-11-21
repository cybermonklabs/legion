package ompsessions

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// dropRole drops role, which the test login made and granted itself with the SET option. First
// it runs revokes, the statements that take back what another role granted role, since DROP
// OWNED BY takes back only what the role running it may: the test login's grants on the database
// and on its public schema, and the tables' owner's grants on its tables. Then, as the role
// itself, it drops what role owns, and last it drops the role, as its creator. DROP OWNED BY
// needs the role's privileges, which a login that is no superuser and made the role with
// CREATEROLE holds only through SET, since the membership CREATE ROLE gives such a creator does
// not inherit (createrole_self_grant is empty by default). A Legion pod's test login is one
// (packages/daemon/docker/legion-testpg).
func dropRole(t *testing.T, admin *pgx.Conn, role string, revokes ...string) {
	t.Helper()
	statements := append(slices.Clone(revokes), "SET ROLE "+role, "DROP OWNED BY "+role, "RESET ROLE", "DROP ROLE "+role)
	for _, statement := range statements {
		if _, err := admin.Exec(context.Background(), statement); err != nil {
			t.Errorf("drop the test's role: %s: %v", statement, err)
			_, _ = admin.Exec(context.Background(), "RESET ROLE")
		}
	}
}

// The plants, each by the name its run raises (public.plant_ran).
const (
	plantedRight  = "public.right(text, double precision)"
	plantedFiles  = "the view omp_session_files in the schema named for the login"
	plantedParts  = "the view omp_session_files_parts in the schema named for the login"
	plantedPolicy = "the row-security policy on public.omp_session_files"
	plantedView   = "the view at public.omp_session_files"
)

// plantRan is what a run of a plant raises: `PLANT RAN: <plant>`, from public.plant_ran, which pgx
// reports as `ERROR: PLANT RAN: <plant> (SQLSTATE P0001)`.
var plantRan = regexp.MustCompile(`PLANT RAN: (.*?) \(SQLSTATE P0001\)`)

// ranPlant is the plant err names as having run, "" where it names none.
func ranPlant(err error) string {
	if err == nil {
		return ""
	}
	if match := plantRan.FindStringSubmatch(err.Error()); match != nil {
		return match[1]
	}
	return ""
}

// planted is a database another login has planted objects in, and what reads it.
type planted struct {
	// conn is the package's own client, ConnectURL, signed in as the test's login.
	conn *Conn
	// admin is a second connection as the same login, for the test's own reads and writes.
	admin *pgx.Conn
}

// plantDatabase is a new database that a role of its own, other than the test's login, plants
// objects in: the role holds CREATE on the database and on its public schema, as the session
// database's agent login and its owner do, and it owns the function public.plant_ran(what), which
// raises `PLANT RAN: <what>`, so a statement that runs a plant fails naming it, read-only or not.
// With tables the database holds Oh My Pi's two tables in public first, as Oh My Pi makes them
// (createTables). plant is the role's statements, given the test's login as an identifier. The
// role and everything it made are dropped when the test ends.
func plantDatabase(t *testing.T, prefix string, tables bool, plant func(login string) []string) planted {
	t.Helper()
	ctx := context.Background()
	dsn := testdb.Database(t, prefix)
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	conn, err := ConnectURL(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	var login, database string
	if err := admin.QueryRow(ctx, "SELECT current_user, current_database()").Scan(&login, &database); err != nil {
		t.Fatal(err)
	}
	planter, err := testdb.RandomName("legion_ompsessions_planter")
	if err != nil {
		t.Fatal(err)
	}
	role := pgx.Identifier{planter}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE ROLE "+role+" NOLOGIN"); err != nil {
		t.Fatal(err)
	}
	// What the role made is in this database alone, so dropping it here frees the role to be
	// dropped, before testdb.Database's cleanup, registered earlier and so run later, drops the
	// database.
	t.Cleanup(func() {
		dropRole(t, admin, role, "REVOKE ALL ON DATABASE "+pgx.Identifier{database}.Sanitize()+" FROM "+role,
			"REVOKE ALL ON SCHEMA public FROM "+role)
	})
	var statements []string
	if tables {
		statements = append(statements, createTables...)
	}
	statements = append(statements,
		// A login that is no superuser may SET ROLE only to a role it is a member of with the SET
		// option, which creating the role does not give it from PostgreSQL 16 on.
		"GRANT "+role+" TO CURRENT_USER WITH INHERIT FALSE",
		"GRANT CREATE ON DATABASE "+pgx.Identifier{database}.Sanitize()+" TO "+role,
		"GRANT CREATE ON SCHEMA public TO "+role,
		"SET ROLE "+role,
		"CREATE FUNCTION public.plant_ran(what text) RETURNS boolean LANGUAGE plpgsql SET search_path = pg_catalog "+
			"AS $$ BEGIN RAISE EXCEPTION 'PLANT RAN: %', what; END $$")
	statements = append(statements, plant(pgx.Identifier{login}.Sanitize())...)
	statements = append(statements, "RESET ROLE")
	for _, statement := range statements {
		if _, err := admin.Exec(ctx, statement); err != nil {
			t.Fatalf("plant the database: %s: %v", statement, err)
		}
	}
	return planted{conn: conn, admin: admin}
}

// unrestricted runs read in a read-only transaction pinned as the package's were before the
// restrict line (restrictRelationKinds): search_path and row_security alone. It is the controls'
// form of a read, so a plant the package's own transaction refuses is shown to run in it.
func unrestricted(ctx context.Context, conn *pgx.Conn, read func(pgx.Tx) error) error {
	return pgx.BeginTxFunc(ctx, conn, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		for _, statement := range []string{pinSearchPath, refuseRowSecurity} {
			if _, err := tx.Exec(ctx, statement); err != nil {
				return err
			}
		}
		return read(tx)
	})
}

// A login that may create in the session database's public schema, or in the database, as its
// agent login and its owner may, plants objects that a statement of the package would run, as
// whoever runs the statement, were it not pinned. Each plant calls public.plant_ran, which raises
// `PLANT RAN: <plant>`, so a statement that runs one fails naming it, in a read-only transaction
// too. The plants:
//
//   - public.right(text, double precision): FindByID's suffix compare, right(path, $1), resolves
//     to it over pg_catalog's right(text, integer), wherever pg_catalog is on the search_path,
//     because an argument of no declared type, which a statement's parameter is, takes the
//     preferred type of the category every candidate accepts, and double precision is the numeric
//     category's. (A plant of right(text, integer) itself loses to pg_catalog's, which the path
//     searches first; one of right(text, text) wins, an untyped argument preferring a string type,
//     but pgx then cannot send the Go int it binds as text, so FindByID fails rather than runs it.)
//   - a schema named for the login that runs the statement, which the default search_path,
//     "$user", public, searches before public, holding a view of each of Oh My Pi's two tables.
//   - a row-security policy the tables' owner attaches, which runs in each reader's session.
//   - a view at a table's own name in public, made before Oh My Pi's table: no search_path
//     reaches the query a view fixed when it was made, and a view carries no policy.
//
// Each has a control, a statement as the package ran it before the line that stops the plant,
// which must fail naming the plant, so the plant is live in its database; the controls log the
// error they get. Every statement the package runs then runs none of them.
func TestNoStatementRunsAnObjectAnotherLoginPlanted(t *testing.T) {
	ctx := context.Background()

	// A table of the name in a schema the search_path reaches first stands in for the one in
	// public, so a bare CREATE TABLE IF NOT EXISTS makes nothing and every later statement finds
	// no table of Oh My Pi's.
	t.Run("CreateTables makes the tables in public whatever a planted schema holds", func(t *testing.T) {
		db := plantDatabase(t, "legion_ompsessions_plant_create", false, func(login string) []string {
			return []string{
				"CREATE SCHEMA " + login,
				"GRANT USAGE ON SCHEMA " + login + " TO PUBLIC",
				"CREATE TABLE " + login + ".omp_session_files (path text)",
				"CREATE TABLE " + login + ".omp_session_files_parts (path text)",
			}
		})
		if err := CreateTables(ctx, db.conn); err != nil {
			t.Fatal(err)
		}
		var files, parts bool
		if err := db.admin.QueryRow(ctx, "SELECT pg_catalog.to_regclass('public.omp_session_files') IS NOT NULL, "+
			"pg_catalog.to_regclass('public.omp_session_files_parts') IS NOT NULL").Scan(&files, &parts); err != nil {
			t.Fatal(err)
		}
		if !files || !parts {
			t.Errorf("after CreateTables public holds omp_session_files %t and omp_session_files_parts %t, want both: "+
				"the planted schema's table of each name stood in for it", files, parts)
		}
	})

	// The tables' owner can attach a row-security policy to a table, and the policy's function
	// runs in each reader's own session, as that reader, wherever its search_path points. So the
	// planter owns the tables here, enables row security on both, and attaches a policy that runs
	// public.plant_ran, and the package's client signs in as a login of its own that neither owns
	// them nor bypasses row security, as a pod's login does. Each read refuses the policy rather
	// than run it: a read-only transaction turns row_security off, under which a query a policy
	// would affect fails. The control is Written's read in a transaction with the search_path pin
	// alone: it runs the policy. The writes are not held to this.
	t.Run("a row-security policy the tables' owner attached runs in no read", func(t *testing.T) {
		db := plantDatabase(t, "legion_ompsessions_plant_policy", false, func(string) []string {
			plant := append(slices.Clone(createTables),
				"INSERT INTO public.omp_session_files (path, content, mtime_ms, byte_len) VALUES ('"+podSessionFile+"', '{}', 1, 2)")
			for _, table := range []string{"public.omp_session_files", "public.omp_session_files_parts"} {
				plant = append(plant, "ALTER TABLE "+table+" ENABLE ROW LEVEL SECURITY",
					"CREATE POLICY planted ON "+table+" USING (public.plant_ran('"+plantedPolicy+"'))")
			}
			return plant
		})
		reader, err := testdb.RandomName("legion_ompsessions_reader")
		if err != nil {
			t.Fatal(err)
		}
		role := pgx.Identifier{reader}.Sanitize()
		const password = "a reader of this test alone"
		if _, err := db.admin.Exec(ctx, "CREATE ROLE "+role+" LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '"+password+"'"); err != nil {
			t.Fatal(err)
		}
		var owner string
		if err := db.admin.QueryRow(ctx, "SELECT tableowner FROM pg_catalog.pg_tables WHERE schemaname = 'public' AND tablename = 'omp_session_files'").Scan(&owner); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			dropRole(t, db.admin, role, "SET ROLE "+pgx.Identifier{owner}.Sanitize(),
				"REVOKE ALL ON public.omp_session_files, public.omp_session_files_parts FROM "+role, "RESET ROLE")
		})
		if _, err := db.admin.Exec(ctx, "GRANT "+role+" TO CURRENT_USER WITH INHERIT FALSE"); err != nil {
			t.Fatal(err)
		}
		for _, statement := range []string{"SET ROLE " + pgx.Identifier{owner}.Sanitize(),
			"GRANT SELECT, INSERT, UPDATE, DELETE ON public.omp_session_files, public.omp_session_files_parts TO " + role, "RESET ROLE"} {
			if _, err := db.admin.Exec(ctx, statement); err != nil {
				t.Fatalf("%s: %v", statement, err)
			}
		}
		config := db.admin.Config().Copy()
		config.User, config.Password = reader, password
		conn, err := pgx.ConnectConfig(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close(context.Background()) })
		reading := &Conn{c: conn}

		live := t.Run("control: a read with the search_path pin alone runs the policy", func(t *testing.T) {
			err := pgx.BeginTxFunc(ctx, conn, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, pinSearchPath); err != nil {
					return err
				}
				var mtime int64
				return tx.QueryRow(ctx, "SELECT mtime_ms FROM public.omp_session_files WHERE path = $1", podSessionFile).Scan(&mtime)
			})
			if ranPlant(err) != plantedPolicy {
				t.Errorf("the read with the pin alone = %v; want it to fail naming %s", err, plantedPolicy)
			}
			t.Logf("control: %v", err)
		})
		if !live {
			t.Fatal("a policy no read runs is no plant: the rows below would pass whatever the package runs")
		}
		refused := func(t *testing.T, what string, err error) {
			t.Helper()
			if err == nil || ranPlant(err) != "" || !strings.Contains(err.Error(), "row-level security") {
				t.Errorf("%s = %v; want the read refused for the row-level security policy, which it does not run", what, err)
			}
		}
		t.Run("Written", func(t *testing.T) {
			_, _, err := Written(ctx, reading, podSessionFile)
			refused(t, "Written", err)
		})
		t.Run("FindByID", func(t *testing.T) {
			_, _, err := FindByID(ctx, reading, podSessionID)
			refused(t, "FindByID", err)
		})
		t.Run("Read", func(t *testing.T) {
			_, _, err := Read(ctx, reading, podSessionFile)
			refused(t, "Read", err)
		})
	})

	// A login with CREATE on public can make a view at each of the tables' own names in a
	// database no Oh My Pi has opened yet, and the launcher reads the store before the pod's Oh My
	// Pi starts. A view's query was fixed when the view was made, so no search_path reaches it,
	// and a view carries no row-security policy, so row_security refuses nothing. Every
	// transaction of the package sets restrict_nonsystem_relation_kind to refuse a view and a
	// foreign table, and CreateTables refuses a relation at either name that is no table. The
	// control is Written's read as the package's reads ran before that line, the search_path pin
	// and row_security off alone: it runs the view's function.
	t.Run("a view at a table's own name runs in no statement", func(t *testing.T) {
		// Each view's path is computed through the plant, so any statement that reads a row's
		// path, which every statement of the package does, runs it.
		const columns = "SELECT CASE WHEN public.plant_ran('" + plantedView + "') THEN ''::text END AS path, ''::text AS content, " +
			"0::bigint AS mtime_ms, NULL::text AS title, NULL::text AS title_source, NULL::text AS title_updated_at, 0::bigint AS byte_len"
		db := plantDatabase(t, "legion_ompsessions_plant_view", false, func(string) []string {
			return []string{
				"CREATE VIEW public.omp_session_files AS " + columns,
				"CREATE VIEW public.omp_session_files_parts AS SELECT CASE WHEN public.plant_ran('" + plantedView + "_parts') " +
					"THEN ''::text END AS path, 0::bigint AS start_offset, ''::text AS content",
				"GRANT SELECT, INSERT, UPDATE, DELETE ON public.omp_session_files, public.omp_session_files_parts TO PUBLIC",
			}
		})
		live := t.Run("control: a read pinned as before the restrict line runs the view", func(t *testing.T) {
			err := unrestricted(ctx, db.conn.c, func(tx pgx.Tx) error {
				var mtime int64
				return tx.QueryRow(ctx, "SELECT mtime_ms FROM public.omp_session_files WHERE path = $1", podSessionFile).Scan(&mtime)
			})
			if ranPlant(err) != plantedView {
				t.Errorf("the read without the restrict line = %v; want it to fail naming %s", err, plantedView)
			}
			t.Logf("control: %v", err)
		})
		if !live {
			t.Fatal("a view no read runs is no plant: the rows below would pass whatever the package runs")
		}
		restricted := func(t *testing.T, what string, err error) {
			t.Helper()
			if err == nil || ranPlant(err) != "" || !strings.Contains(err.Error(), `access to non-system view "omp_session_files`) {
				t.Errorf("%s = %v; want it refused for the view, which it does not run", what, err)
			}
		}
		notATable := func(t *testing.T, what string, err error) {
			t.Helper()
			if err == nil || ranPlant(err) != "" || !strings.Contains(err.Error(), "public.omp_session_files is a view, not a table") {
				t.Errorf("%s = %v; want it refused naming the view at public.omp_session_files", what, err)
			}
		}
		t.Run("CreateTables", func(t *testing.T) { notATable(t, "CreateTables", CreateTables(ctx, db.conn)) })
		t.Run("Import", func(t *testing.T) {
			_, err := Import(ctx, db.conn, sessionPath, []byte(session), time.Now())
			notATable(t, "Import", err)
		})
		t.Run("Written", func(t *testing.T) {
			_, _, err := Written(ctx, db.conn, podSessionFile)
			restricted(t, "Written", err)
		})
		t.Run("FindByID", func(t *testing.T) {
			_, _, err := FindByID(ctx, db.conn, podSessionID)
			restricted(t, "FindByID", err)
		})
		t.Run("Read", func(t *testing.T) {
			_, _, err := Read(ctx, db.conn, podSessionFile)
			restricted(t, "Read", err)
		})
		t.Run("Replace", func(t *testing.T) {
			restricted(t, "Replace", Replace(ctx, db.conn, claudeTranscript, Absent, []byte("{}\n"), time.Now()))
		})
		t.Run("Append", func(t *testing.T) {
			restricted(t, "Append", Append(ctx, db.conn, claudeTranscript, 3, []byte("{}\n"), time.Now()))
		})
	})

	db := plantDatabase(t, "legion_ompsessions_plant", true, func(login string) []string {
		return []string{
			"CREATE FUNCTION public.right(s text, n double precision) RETURNS text LANGUAGE sql " +
				"AS $$ SELECT CASE WHEN public.plant_ran('" + plantedRight + "') THEN pg_catalog.right(s, n::integer) END $$",
			"CREATE SCHEMA " + login,
			"GRANT USAGE ON SCHEMA " + login + " TO PUBLIC",
			"CREATE VIEW " + login + ".omp_session_files WITH (security_invoker = true) AS SELECT * FROM public.omp_session_files " +
				"WHERE public.plant_ran('" + plantedFiles + "') WITH LOCAL CHECK OPTION",
			"CREATE VIEW " + login + ".omp_session_files_parts WITH (security_invoker = true) AS SELECT * FROM public.omp_session_files_parts " +
				"WHERE public.plant_ran('" + plantedParts + "') WITH LOCAL CHECK OPTION",
			"GRANT SELECT, INSERT, UPDATE, DELETE ON " + login + ".omp_session_files, " + login + ".omp_session_files_parts TO PUBLIC",
		}
	})
	if _, err := db.admin.Exec(ctx, "INSERT INTO public.omp_session_files (path, content, mtime_ms, byte_len) VALUES ($1, $2, $3, $4)",
		podSessionFile, session, podSessionWritten, len(session)); err != nil {
		t.Fatal(err)
	}

	// The controls are the package's statements as they were before their pin: Written's naming
	// its table bare, which reaches the schema named for the login; Append's insert of a part,
	// which reaches that schema's view of the parts table, whose check option runs the plant on the
	// new row (the parts table is empty, so a read of it runs nothing); and FindByID's suffix
	// compare on the table in public under the default search_path, which reaches public.right.
	// Each must fail naming its plant.
	live := t.Run("control: the statements as they were before their pin run the plants", func(t *testing.T) {
		suffix := "_" + podSessionID + ".jsonl"
		for _, control := range []struct {
			plant, sql string
			run        func() error
		}{
			{plantedFiles, "SELECT mtime_ms FROM omp_session_files WHERE path = $1", func() error {
				var mtime int64
				return db.conn.c.QueryRow(ctx, "SELECT mtime_ms FROM omp_session_files WHERE path = $1", podSessionFile).Scan(&mtime)
			}},
			{plantedParts, "INSERT INTO omp_session_files_parts (path, start_offset, content) VALUES ($1, $2, $3)", func() error {
				_, err := db.conn.c.Exec(ctx, "INSERT INTO omp_session_files_parts (path, start_offset, content) VALUES ($1, $2, $3)",
					podSessionFile, len(session), "")
				return err
			}},
			{plantedRight, "SELECT path FROM public.omp_session_files WHERE right(path, $1) = $2", func() error {
				var path string
				return db.conn.c.QueryRow(ctx, "SELECT path FROM public.omp_session_files WHERE right(path, $1) = $2 ORDER BY mtime_ms DESC, path ASC LIMIT 1",
					utf8.RuneCountInString(suffix), suffix).Scan(&path)
			}},
		} {
			err := control.run()
			if ranPlant(err) != control.plant {
				t.Errorf("%s = %v; want it to fail naming %s", control.sql, err, control.plant)
			}
			t.Logf("control: %v", err)
		}
	})
	if !live {
		t.Fatal("a plant the bare statements do not run is no plant: the rows below would pass whatever the package runs")
	}

	first, second := "{\"n\":1}\n", "{\"n\":2}\n"
	for _, client := range []struct {
		name string
		run  func(t *testing.T) error
	}{
		{"Written", func(t *testing.T) error {
			at, found, err := Written(ctx, db.conn, podSessionFile)
			if err == nil && (!found || at.UnixMilli() != podSessionWritten) {
				t.Errorf("Written = %v, %t; want the row's %d", at, found, podSessionWritten)
			}
			return err
		}},
		{"FindByID", func(t *testing.T) error {
			path, found, err := FindByID(ctx, db.conn, podSessionID)
			if err == nil && (!found || path != podSessionFile) {
				t.Errorf("FindByID = %q, %t; want %s", path, found, podSessionFile)
			}
			return err
		}},
		{"Read", func(t *testing.T) error {
			content, found, err := Read(ctx, db.conn, podSessionFile)
			if err == nil && (!found || string(content) != session) {
				t.Errorf("Read = %q, %t; want the row's content", content, found)
			}
			return err
		}},
		{"Import", func(t *testing.T) error {
			for _, want := range []Outcome{Copied, Identical} {
				outcome, err := Import(ctx, db.conn, sessionPath, []byte(session), time.Now())
				if err != nil {
					return err
				}
				if outcome != want {
					t.Errorf("Import = %q; want %q", outcome, want)
				}
			}
			return nil
		}},
		{"Replace", func(t *testing.T) error {
			return Replace(ctx, db.conn, claudeTranscript, Absent, []byte(first), time.Now())
		}},
		{"Append", func(t *testing.T) error {
			if err := Append(ctx, db.conn, claudeTranscript, int64(len(first)), []byte(second), time.Now()); err != nil {
				return err
			}
			var parts int
			if err := db.admin.QueryRow(ctx, "SELECT count(*) FROM public.omp_session_files_parts WHERE path = $1", claudeTranscript).Scan(&parts); err != nil {
				t.Fatal(err)
			}
			if parts != 2 {
				t.Errorf("public.omp_session_files_parts holds %d parts of %s, want Replace's and Append's", parts, claudeTranscript)
			}
			return nil
		}},
		{"CreateTables", func(t *testing.T) error { return CreateTables(ctx, db.conn) }},
	} {
		t.Run(client.name, func(t *testing.T) {
			if err := client.run(t); err != nil {
				if plant := ranPlant(err); plant != "" {
					t.Errorf("%s ran %s, which another login planted: %v", client.name, plant, err)
				} else {
					t.Errorf("%s = %v", client.name, err)
				}
			}
		})
	}

	// A call of the package run inside another call's transaction, on the same connection, would
	// send a second begin, which the server ignores inside a transaction, and its commit would end
	// the outer transaction, and every SET LOCAL with it: the outer function's next statement would
	// run alone, under the login's own search_path. inTransaction refuses a connection already in a
	// transaction. The control is the same nesting with today's helper as it was before that
	// refusal (unchecked): the outer statement after the inner read runs public.right.
	// The outer statement is FindByID's compare in text of its own: pgx keeps each statement it
	// prepared on the connection, with the types its parameters were given then, and FindByID's own
	// text was prepared under the pin, where $1 took pg_catalog.right's integer.
	suffix := "_" + podSessionID + ".jsonl"
	findByID := "SELECT path FROM public.omp_session_files WHERE right(path, $1) = $2 LIMIT 1"
	nested := t.Run("control: a read inside another transaction ends it, and the next statement runs unpinned", func(t *testing.T) {
		err := unchecked(ctx, db.conn.c, false, func(outer pgx.Tx) error {
			if err := unchecked(ctx, db.conn.c, true, func(inner pgx.Tx) error {
				var mtime int64
				return inner.QueryRow(ctx, "SELECT mtime_ms FROM public.omp_session_files WHERE path = $1", podSessionFile).Scan(&mtime)
			}); err != nil {
				return err
			}
			var path string
			return outer.QueryRow(ctx, findByID, utf8.RuneCountInString(suffix), suffix).Scan(&path)
		})
		if ranPlant(err) != plantedRight {
			t.Errorf("the nested transactions = %v; want the outer statement to fail naming %s", err, plantedRight)
		}
		t.Logf("control: %v", err)
	})
	if !nested {
		t.Fatal("a nesting that runs no plant shows nothing: the row below would pass whatever the helper does")
	}
	t.Run("a call inside another call's transaction is refused, and runs no plant", func(t *testing.T) {
		err := inTransaction(ctx, db.conn, false, func(p pinned) error {
			if _, _, err := Written(ctx, db.conn, podSessionFile); err != nil {
				return err
			}
			var path string
			return p.queryRow(ctx, findByID, utf8.RuneCountInString(suffix), suffix).Scan(&path)
		})
		if err == nil || ranPlant(err) != "" || !strings.Contains(err.Error(), "already in a transaction") {
			t.Errorf("Written inside inTransaction's function = %v; want it refused, the connection being already in a transaction", err)
		}
	})

	// A transaction whose context ends inside it leaves its connection closed, pgx having closed it
	// when the rollback failed, with the last status the server sent, 'T'. A call on it must fail
	// as a call on a closed connection does, not be refused as one nested in another's transaction.
	// The control is the refusal as it was first written (refusedAsNested), on the status alone: it
	// calls this connection nested.
	t.Run("a call on a connection that died inside a transaction fails as closed", func(t *testing.T) {
		dead, err := ConnectURL(ctx, db.admin.Config().ConnString())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = dead.Close(context.Background()) })
		ending, end := context.WithCancel(ctx)
		if err := inTransaction(ending, dead, false, func(p pinned) error {
			end()
			_, err := p.exec(ending, "SELECT 1")
			return err
		}); err == nil {
			t.Fatal("a transaction whose context ended inside it committed")
		}
		raw := dead.c
		if status := raw.PgConn().TxStatus(); !raw.IsClosed() || status != 'T' {
			t.Fatalf("after a transaction whose context ended inside it the connection is closed %t with status %q; "+
				"want closed, with the status 'T' the server last sent", raw.IsClosed(), status)
		}
		if refusal := refusedAsNested(raw); !strings.Contains(refusal, "already in a transaction") {
			t.Fatalf("control: the refusal as first written answers %q on the dead connection, want it to call it nested", refusal)
		} else {
			t.Logf("control: %s", refusal)
		}
		_, _, err = Written(ctx, dead, podSessionFile)
		if err == nil || strings.Contains(err.Error(), "already in a transaction") || !strings.Contains(err.Error(), "conn closed") {
			t.Errorf("Written on the dead connection = %v; want it to fail as a call on a closed connection (conn closed)", err)
		}
	})
}

// refusedAsNested is inTransaction's refusal as it was first written, on the connection's
// transaction status alone, "" where it refuses nothing. It is the dead-connection row's control.
func refusedAsNested(conn *pgx.Conn) string {
	if status := conn.PgConn().TxStatus(); status != 'I' {
		return fmt.Sprintf("open a transaction on a connection already in a transaction (status %q)", status)
	}
	return ""
}

// unchecked is inTransaction as it was before it refused a connection already in a transaction:
// the begin, the three settings, then fn. It is the nested-call control's helper.
func unchecked(ctx context.Context, conn *pgx.Conn, readOnly bool, fn func(pgx.Tx) error) error {
	access, settings := pgx.ReadWrite, []string{pinSearchPath, restrictRelationKinds}
	if readOnly {
		access, settings = pgx.ReadOnly, []string{pinSearchPath, refuseRowSecurity, restrictRelationKinds}
	}
	return pgx.BeginTxFunc(ctx, conn, pgx.TxOptions{AccessMode: access}, func(tx pgx.Tx) error {
		for _, statement := range settings {
			if _, err := tx.Exec(ctx, statement); err != nil {
				return err
			}
		}
		return fn(tx)
	})
}
