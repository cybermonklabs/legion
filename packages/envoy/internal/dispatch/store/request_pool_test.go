package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/asof"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/testpg/testdb"
)

// requestRole is the role the request pool runs as (plan D1, D7).
const requestRole = "dispatch_app"

// The viewers of the request pool's rows: seesBoth sees PRIV and PUB, and seesPublic, an open
// viewer of a deployment that holds a private project, sees PUB alone.
var (
	seesBoth   = model.Viewer{Person: "bob@example.com", Open: true, Sees: []string{"PRIV", "PUB"}, AnyPrivate: true}
	seesPublic = model.Viewer{Person: "carol@example.com", Open: true, Sees: []string{"PUB"}, AnyPrivate: true}
)

// requestPoolFixture is the database the request pool's rows share: a migrated database whose
// pool is the system pool, PUB (public) and PRIV (private, owned by alice) each holding an issue
// and a row of each shadowed kind (seedRequestPoolProject), and the request pool opened beside it.
type requestPoolFixture struct {
	system  *Store
	request *Pool
	opened  error
	// login is the login the system pool runs as, which owns every table.
	login string
	// made is when each seeded row and event was written; cut is a cut after it.
	made time.Time
	cut  asof.Cut
	// documents names each project's seeded document by its id.
	documents map[string]string
}

// requestPool is the fixture's request pool, failing t where it did not open.
func (f *requestPoolFixture) requestPool(t *testing.T) *Pool {
	t.Helper()
	if f.opened != nil {
		t.Fatalf("open the request pool beside the system pool: %v", f.opened)
	}
	return f.request
}

// seedRequestPoolProject writes project key, of visibility, with an issue key-1 and a row of each
// shadowed kind on it - an ask, a comment, a message and the issue's primary document with one
// version - and the events their writers append, each made at made, so a read at a later cut
// answers each one. Every id ends in suffix. It answers the document's id.
func seedRequestPoolProject(t *testing.T, ctx context.Context, s *Store, key, visibility, suffix string, made time.Time) string {
	t.Helper()
	encode := func(value any) []byte {
		t.Helper()
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("encode %T: %v", value, err)
		}
		return encoded
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := s.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed %s (%s): %v", key, strings.Join(strings.Fields(sql), " "), err)
		}
	}
	alice := model.Actor{Kind: "user", ID: "alice@example.com"}
	issueKey := key + "-1"
	seq := 0
	event := func(kind string, payload any) {
		t.Helper()
		seq++
		exec(`insert into events (issue_key, seq, type, actor, payload, notify, created_at) values ($1, $2, $3, $4, $5, false, $6)`,
			issueKey, seq, kind, encode(alice), encode(payload), made)
	}

	exec(`insert into projects (key, name, default_assignee, visibility, created_at) values ($1, $1, 'alice@example.com', $2, $3)`, key, visibility, made)
	issue := model.Issue{
		Key: issueKey, Project: key, Number: 1, Title: "The issue of " + key, Status: model.IssueStatusTriage, Rank: "m",
		Labels: []string{}, BlockedBy: []string{}, Advances: []model.IssueAdvance{},
		Components:    model.IssueComponents{Mode: "inherit", IDs: []string{}, Unknown: []string{}},
		ExternalLinks: []model.ExternalLink{}, CreatedBy: &alice, CreatedAt: made, UpdatedAt: made,
	}
	exec(`insert into issues (key, project_key, number, title, status, labels, rank, created_by, created_at, updated_at)
		values ($1, $2, 1, $3, $4, '{}', 'm', $5, $6, $6)`,
		issueKey, key, issue.Title, issue.Status, encode(alice), made)
	event("issue.created", model.NewIssueEventPayload(issue, model.ReferenceChanges{}))

	askID := "00000000-0000-4000-8000-0000000000a" + suffix
	ask := model.Ask{ID: askID, IssueKey: &issueKey, Author: alice, Kind: "question", Question: "Ship it?",
		Options: []model.AskOption{{Label: "yes"}, {Label: "no"}}, Urgency: "med", State: "open", CreatedAt: made}
	exec(`insert into asks (id, issue_key, author, question, options, created_at) values ($1, $2, $3, 'Ship it?', $4, $5)`,
		askID, issueKey, encode(alice), encode(ask.Options), made)
	event("ask.opened", model.NewAskEventPayload(ask, model.ReferenceChanges{}))

	commentID := "00000000-0000-4000-8000-0000000000c" + suffix
	comment := model.Comment{ID: commentID, IssueKey: &issueKey, Author: alice, Body: "A comment of " + key, CreatedAt: made,
		Mentions: []model.Mention{}, Deliveries: []model.CommentDelivery{}}
	exec(`insert into comments (id, issue_key, author, body, created_at) values ($1, $2, $3, $4, $5)`,
		commentID, issueKey, encode(alice), comment.Body, made)
	event("comment.created", model.CommentEventPayload{Comment: comment})

	messageID := "00000000-0000-4000-8000-0000000000d" + suffix
	target := "session:s-" + suffix
	message := model.Message{ID: messageID, IssueKey: &issueKey, Author: alice, Body: "A message of " + key, Target: &target,
		CreatedAt: made, Deliveries: []model.MessageDelivery{}}
	exec(`insert into messages (id, issue_key, author, body, target, created_at) values ($1, $2, $3, $4, $5, $6)`,
		messageID, issueKey, encode(alice), message.Body, target, made)
	event("message.created", model.MessageEventPayload{Message: message})

	var document string
	if err := s.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by, created_at)
		values ($1, $2, 'spec', 'spec.md', 'doc', true, $3, $4) returning id::text`, issueKey, key, encode(alice), made).Scan(&document); err != nil {
		t.Fatalf("seed %s's document: %v", key, err)
	}
	exec(`insert into artifact_versions (artifact_id, number, markdown, created_at) values ($1, 1, $2, $3)`, document, "The spec of "+key+".", made)
	event("artifact.created", map[string]any{"artifact": map[string]any{"id": document}})
	return document
}

// bareRequestConnection is one connection opened from the request pool's own configuration, with
// the AfterConnect the pool runs on each connection it opens and no viewer setting: what a
// statement sent past the pool's wrapper would run on. It is released, and its pool closed, when t
// ends.
func bareRequestConnection(t *testing.T, ctx context.Context, request *Pool) *pgxpool.Conn {
	t.Helper()
	config := request.Config()
	config.MaxConns, config.MinConns, config.MinIdleConns = 1, 0, 0
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open a pool from the request pool's configuration: %v", err)
	}
	t.Cleanup(pool.Close)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("open a connection from the request pool's configuration: %v", err)
	}
	t.Cleanup(conn.Release)
	return conn
}

// scratchRole creates, for one row, a NOLOGIN role with attributes (superuser, bypassrls, or none)
// and grants it to the test's login with SET and INHERIT (INHERIT, which `drop owned by` needs),
// and when the row ends drops what it owns and is granted in the test's database, then the role:
// a role spans the cluster. A login that may not create such a role skips the row, and fails it
// on GitHub Actions, whose server logs in as a superuser (testdb.SkipOutsideCI).
func scratchRole(t *testing.T, ctx context.Context, s *Store, prefix, attributes string) string {
	t.Helper()
	role := prefix + "_" + randomDatabaseSuffix(t)
	if _, err := s.Pool.Exec(ctx, "create role "+role+" nologin "+attributes); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42501" {
			testdb.SkipOutsideCI(t, "the test server's login may not create a NOLOGIN role %q (it needs CREATEROLE, and a superuser for SUPERUSER or BYPASSRLS): %v", attributes, err)
		}
		t.Fatalf("create the role %s: %v", role, err)
	}
	t.Cleanup(func() {
		if _, err := s.Pool.Exec(context.Background(), "drop owned by "+role); err != nil {
			t.Errorf("drop what the role %s owns and is granted: %v", role, err)
		}
		if _, err := s.Pool.Exec(context.Background(), "drop role "+role); err != nil {
			t.Errorf("drop the role %s: %v", role, err)
		}
	})
	if _, err := s.Pool.Exec(ctx, "grant "+role+" to current_user with set true, inherit true"); err != nil {
		t.Fatalf("grant the role %s to the test's login: %v", role, err)
	}
	return role
}

// policyTables names, sorted, each table of schema public that holds a policy named viewer.
func policyTables(t *testing.T, ctx context.Context, s *Store) []string {
	t.Helper()
	rows, err := s.Pool.Query(ctx, `
		select c.relname::text from pg_class c
		where c.relnamespace = 'public'::regnamespace and c.relkind in ('r', 'p')
			and exists (select from pg_policy p where p.polrelid = c.oid and p.polname = 'viewer')
		order by 1`)
	tables := collectKeys(t)(rows, err)
	if !slices.Contains(tables, "issues") || !slices.Contains(tables, "projects") {
		t.Fatalf("the tables that hold a viewer policy are %v, which leaves out issues or projects: row security is not on", tables)
	}
	return tables
}

// countRows is how many rows of table q reads.
func countRows(t *testing.T, ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, table string) int64 {
	t.Helper()
	var count int64
	if err := q.QueryRow(ctx, "select count(*) from public."+pgx.Identifier{table}.Sanitize()).Scan(&count); err != nil {
		t.Fatalf("count the rows of %s: %v", table, err)
	}
	return count
}

// collectKeys reads every row of a query, its rows and its error as the query answers them, as
// one text column, failing t on either error: collectKeys(t)(pool.Query(ctx, sql)).
func collectKeys(t *testing.T) func(rows pgx.Rows, err error) []string {
	return func(rows pgx.Rows, err error) []string {
		t.Helper()
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("read the rows: %v", err)
		}
		return keys
	}
}

// versionBeforeTheRequestRole is the newest shipped migration before the first one that names
// dispatch_app (the row-security functions of plan D7, the readers among them), so that two
// databases migrated through it apply this release's migrations, the role's among them, at once;
// latestVersion while no migration names it.
func versionBeforeTheRequestRole(t *testing.T) int {
	t.Helper()
	previous := 0
	for _, migration := range shippedMigrations(t) {
		if strings.Contains(migration.SQL, requestRole) {
			return previous
		}
		previous = migration.Version
	}
	return latestVersion
}

// TestRequestPoolRunsAsNoBypassRLSRoleAndFailsClosed (plan D1, D2, D3, D5, D7; R2, R3): the
// request pool runs every statement as dispatch_app, a role that is no superuser, not BYPASSRLS
// and owns no table, under the viewer the context carries, in a transaction of its own that
// sends the viewer's settings first; it refuses a statement with neither a viewer nor the
// system-work mark, and every connection, copy and side pool; the database fails closed on a
// bare connection; the system pool, its side pools and every copy of its configuration run as the
// owner; boot refuses a role under which row security would not hold; and AsOwner switches to the
// owner for a rule and back. Each row is one subtest; the rows share one database, and each row's
// own writes name keys no other row reads.
func TestRequestPoolRunsAsNoBypassRLSRoleAndFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := &requestPoolFixture{system: openMigratedTestStore(t, latestVersion), documents: map[string]string{}}
	if err := f.system.Pool.QueryRow(ctx, `select session_user::text`).Scan(&f.login); err != nil {
		t.Fatalf("read the test's login: %v", err)
	}
	if _, err := f.system.Pool.Exec(ctx, `insert into people (email, member_kind) values
		('alice@example.com', 'full'), ('bob@example.com', 'full'), ('carol@example.com', 'full')`); err != nil {
		t.Fatalf("seed the people: %v", err)
	}
	f.made = time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Microsecond)
	f.cut = asof.CutAt(f.made.Add(time.Hour))
	f.documents["PUB"] = seedRequestPoolProject(t, ctx, f.system, "PUB", model.ProjectPublic, "1", f.made)
	f.documents["PRIV"] = seedRequestPoolProject(t, ctx, f.system, "PRIV", model.ProjectPrivate, "2", f.made)
	f.request, f.opened = OpenRequestPool(ctx, f.system.Pool)
	if f.opened == nil {
		t.Cleanup(f.request.Close)
	}
	const bothKeys = `select key from issues where key in ('PRIV-1', 'PUB-1') order by key`

	t.Run("role_is_dispatch_app_nosuperuser_nobypassrls_owns_no_table", func(t *testing.T) {
		var superuser, bypassRLS, canLogin bool
		var owned int
		err := f.system.Pool.QueryRow(ctx, `
			select r.rolsuper, r.rolbypassrls, r.rolcanlogin,
				(select count(*) from pg_class c where c.relowner = r.oid and c.relnamespace = 'public'::regnamespace)
			from pg_roles r where r.rolname = $1`, requestRole).Scan(&superuser, &bypassRLS, &canLogin, &owned)
		if errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("the cluster holds no role %s after the migrations", requestRole)
		}
		if err != nil {
			t.Fatalf("read the role %s: %v", requestRole, err)
		}
		if superuser || bypassRLS || canLogin || owned != 0 {
			t.Errorf("%s: superuser %t, bypassrls %t, login %t, owns %d relations of schema public; want a NOLOGIN NOBYPASSRLS role that is no superuser and owns nothing",
				requestRole, superuser, bypassRLS, canLogin, owned)
		}
	})

	t.Run("migrating_login_may_set_role", func(t *testing.T) {
		var maySet bool
		if err := f.system.Pool.QueryRow(ctx, `select pg_has_role(current_user, $1, 'SET')`, requestRole).Scan(&maySet); err != nil {
			t.Fatalf("read whether the login may set role %s: %v", requestRole, err)
		}
		if !maySet {
			t.Fatalf("the migrating login %s may not SET ROLE %s (grant %s to current_user with set true)", f.login, requestRole, requestRole)
		}
		tx, err := f.system.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		var user string
		if err := tx.QueryRow(ctx, `select set_config('role', $1, true), current_user::text`, requestRole).Scan(nil, &user); err != nil {
			t.Fatalf("set role %s: %v", requestRole, err)
		}
		if user != requestRole {
			t.Errorf("after set_config('role', %q, true) current_user is %s", requestRole, user)
		}
	})

	t.Run("request_role_reads_and_writes_every_table", func(t *testing.T) {
		var usage bool
		if err := f.system.Pool.QueryRow(ctx, `select has_schema_privilege($1, 'public', 'USAGE')`, requestRole).Scan(&usage); err != nil {
			t.Fatalf("read %s's privileges on schema public: %v", requestRole, err)
		}
		if !usage {
			t.Errorf("%s holds no USAGE on schema public", requestRole)
		}
		rows, err := f.system.Pool.Query(ctx, `
			select c.relname::text, c.relkind::text,
				case when c.relkind = 'S' then has_sequence_privilege($1, c.oid, 'USAGE') else has_table_privilege($1, c.oid, 'SELECT') end,
				case when c.relkind = 'S' then has_sequence_privilege($1, c.oid, 'SELECT') else has_table_privilege($1, c.oid, 'INSERT') end,
				case when c.relkind = 'S' then has_sequence_privilege($1, c.oid, 'UPDATE') else has_table_privilege($1, c.oid, 'UPDATE') end,
				c.relkind = 'S' or has_table_privilege($1, c.oid, 'DELETE')
			from pg_class c
			where c.relnamespace = 'public'::regnamespace and c.relkind in ('r', 'p', 'S')
			order by 1`, requestRole)
		if err != nil {
			t.Fatalf("read %s's privileges: %v", requestRole, err)
		}
		var lacking []string
		tables := 0
		for rows.Next() {
			var name, kind string
			var first, second, third, fourth bool
			if err := rows.Scan(&name, &kind, &first, &second, &third, &fourth); err != nil {
				t.Fatalf("read %s's privileges: %v", requestRole, err)
			}
			if kind != "S" {
				tables++
			}
			if !first || !second || !third || !fourth {
				lacking = append(lacking, name)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("read %s's privileges: %v", requestRole, err)
		}
		if tables == 0 {
			t.Fatal("schema public holds no table")
		}
		if len(lacking) > 0 {
			t.Errorf("%s lacks a privilege (select, insert, update and delete of a table; usage, select and update of a sequence) on %d relations of schema public: %s",
				requestRole, len(lacking), strings.Join(lacking, ", "))
		}
	})

	t.Run("a_table_made_after_the_role_takes_its_grants", func(t *testing.T) {
		if _, err := f.system.Pool.Exec(ctx, `create table rls_later_table (id bigserial primary key, note text)`); err != nil {
			t.Fatalf("make a table after the migrations: %v", err)
		}
		t.Cleanup(func() {
			if _, err := f.system.Pool.Exec(context.Background(), `drop table rls_later_table`); err != nil {
				t.Errorf("drop rls_later_table: %v", err)
			}
		})
		var tableGranted, sequenceGranted bool
		if err := f.system.Pool.QueryRow(ctx, `
			select has_table_privilege($1, 'public.rls_later_table', 'SELECT') and has_table_privilege($1, 'public.rls_later_table', 'INSERT')
					and has_table_privilege($1, 'public.rls_later_table', 'UPDATE') and has_table_privilege($1, 'public.rls_later_table', 'DELETE'),
				has_sequence_privilege($1, pg_get_serial_sequence('public.rls_later_table', 'id'), 'USAGE')
					and has_sequence_privilege($1, pg_get_serial_sequence('public.rls_later_table', 'id'), 'SELECT')
					and has_sequence_privilege($1, pg_get_serial_sequence('public.rls_later_table', 'id'), 'UPDATE')`,
			requestRole).Scan(&tableGranted, &sequenceGranted); err != nil {
			t.Fatalf("read %s's privileges on a later table: %v", requestRole, err)
		}
		if !tableGranted || !sequenceGranted {
			t.Errorf("a table the migrating login makes later grants %s its rows %t and its sequence %t: the default privileges must grant both (alter default privileges in schema public)",
				requestRole, tableGranted, sequenceGranted)
		}
	})

	t.Run("bare_request_connection_runs_as_dispatch_app_with_utc_scans", func(t *testing.T) {
		request := f.requestPool(t)
		if !request.IsRequest() {
			t.Error("the pool OpenRequestPool opened is no request pool (IsRequest)")
		}
		if f.system.Pool.IsRequest() {
			t.Error("the system pool reads as a request pool (IsRequest)")
		}
		conn := bareRequestConnection(t, ctx, request)
		var user, session string
		var now time.Time
		if err := conn.QueryRow(ctx, `select current_user::text, session_user::text, now()`).Scan(&user, &session, &now); err != nil {
			t.Fatalf("read the bare connection's role: %v", err)
		}
		if user != requestRole || session != f.login {
			t.Errorf("a connection of the request pool runs as %s with session user %s, want %s with session user %s", user, session, requestRole, f.login)
		}
		if now.Location() != time.UTC {
			t.Errorf("a connection of the request pool scans a timestamptz in %s, want %s (scanTimestampsInUTC)", now.Location(), time.UTC)
		}
	})

	t.Run("side_pools_registry_and_reopened_pools_run_as_the_owner", func(t *testing.T) {
		f.requestPool(t)
		system := f.system.Pool
		owner := func(what string, q interface {
			QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
		}) {
			t.Helper()
			var user string
			if err := q.QueryRow(ctx, `select current_user::text`).Scan(&user); err != nil {
				t.Fatalf("%s: read current_user: %v", what, err)
			}
			if user != f.login {
				t.Errorf("%s runs as %s, want the owner %s", what, user, f.login)
			}
		}
		owner("the system pool", system)
		for name, open := range map[string]func() (*pgxpool.Pool, error){
			"Rooms": system.Rooms, "RoomAppends": system.RoomAppends, "Compaction": system.Compaction, "Horizon": system.Horizon,
		} {
			pool, err := open()
			if err != nil {
				t.Fatalf("the system pool's %s: %v", name, err)
			}
			owner("the system pool's "+name, pool)
		}
		for name, open := range map[string]func() (*Pool, error){"Outbox": system.Outbox, "TextTrees": system.TextTrees} {
			pool, err := open()
			if err != nil {
				t.Fatalf("the system pool's %s: %v", name, err)
			}
			owner("the system pool's "+name, pool)
		}
		callers, err := system.Leases()
		if err != nil {
			t.Fatalf("the system pool's Leases: %v", err)
		}
		leases, err := callers.For("request pool row: the lease pool's role")
		if err != nil {
			t.Fatalf("reserve a lease connection: %v", err)
		}
		owner("the system pool's Leases", leases)
		listener, err := system.Listener(ctx)
		if err != nil {
			t.Fatalf("the system pool's Listener: %v", err)
		}
		defer listener.Close(ctx)
		owner("the system pool's Listener", listener)
		dedicated, err := system.Dedicated(ctx, "dispatch-request-pool-row")
		if err != nil {
			t.Fatalf("the system pool's Dedicated: %v", err)
		}
		defer dedicated.Close(ctx)
		owner("the system pool's Dedicated", dedicated)
		// processes.Register and storetest.Reopened each open a pool of their own from the system
		// pool's configuration (Pool.Config), as this does.
		config := system.Config()
		config.MaxConns, config.MinConns, config.MinIdleConns = 1, 0, 0
		copied, err := pgxpool.NewWithConfig(ctx, config)
		if err != nil {
			t.Fatalf("open a pool from the system pool's configuration: %v", err)
		}
		defer copied.Close()
		owner("a pool opened from the system pool's configuration (processes.Register, storetest.Reopened)", copied)
	})

	t.Run("migrations_run_on_the_system_pool", func(t *testing.T) {
		request := f.requestPool(t)
		if err := (&Store{Pool: request}).Migrate(ctx); !errors.Is(err, ErrRequestPoolRefuses) && !errors.Is(err, ErrNoViewer) {
			t.Errorf("Migrate on the request store answered %v, want the request pool's refusal: migrations take a connection of their own, which the request pool hands out to no one", err)
		}
		if err := f.system.Migrate(ctx); err != nil {
			t.Fatalf("Migrate on the system store: %v", err)
		}
		var user string
		var hidden int
		if err := request.QueryRow(WithViewer(ctx, seesPublic), `select current_user::text, (select count(*) from issues where key = 'PRIV-1')`).Scan(&user, &hidden); err != nil {
			t.Fatalf("read on the request pool after the system pool migrated: %v", err)
		}
		if user != requestRole || hidden != 0 {
			t.Errorf("after Migrate and its session reset on the system pool, a request-pool read runs as %s and reads PRIV-1 %d times, want %s and none", user, hidden, requestRole)
		}
	})

	t.Run("resolve_reads_on_the_system_pool", func(t *testing.T) {
		request := f.requestPool(t)
		// access.Resolve reads projects, people, project_access and access_group_members with no
		// viewer, before the request has one: on the system pool every row is there.
		projects := collectKeys(t)(f.system.Pool.Query(ctx, `select key from projects where key in ('PRIV', 'PUB') order by key`))
		if !slices.Equal(projects, []string{"PRIV", "PUB"}) {
			t.Errorf("the system pool with no viewer reads the projects %v, want PRIV and PUB", projects)
		}
		var people int
		if err := f.system.Pool.QueryRow(ctx, `select count(*) from people`).Scan(&people); err != nil || people != 3 {
			t.Errorf("the system pool with no viewer reads %d people (%v), want the 3 seeded", people, err)
		}
		var count int
		if err := request.QueryRow(ctx, `select count(*) from projects`).Scan(&count); !errors.Is(err, ErrNoViewer) {
			t.Errorf("the request pool with no viewer answered %v (%d projects), want ErrNoViewer: Resolve cannot run there", err, count)
		}
		if seen := countRows(t, ctx, bareRequestConnection(t, ctx, request), "projects"); seen != 0 {
			t.Errorf("a bare request connection reads %d projects, want none: Resolve cannot run there", seen)
		}
	})

	t.Run("boot_refuses_a_superuser_role", func(t *testing.T) {
		clean := requestRoleFacts{role: requestRole}
		if err := checkRequestRole(clean); err != nil {
			t.Errorf("checkRequestRole refuses %+v, the facts of dispatch_app as the migration makes it: %v", clean, err)
		}
		facts := clean
		facts.superuser = true
		if err := checkRequestRole(facts); err == nil || !strings.Contains(strings.ToLower(err.Error()), "superuser") {
			t.Errorf("checkRequestRole(%+v) = %v, want a refusal that names the superuser", facts, err)
		}
		role := scratchRole(t, ctx, f.system, "rls_boot_superuser", "superuser")
		conn := connectForTest(t, ctx, f.system)
		if _, err := conn.Exec(ctx, "set role "+role); err != nil {
			t.Fatalf("set role %s: %v", role, err)
		}
		read, err := readRequestRoleFacts(ctx, conn)
		if err != nil {
			t.Fatalf("read the facts of %s: %v", role, err)
		}
		if read.role != role || !read.superuser {
			t.Errorf("the facts of %s read %+v, want its name and superuser", role, read)
		}
		if err := checkRequestRole(read); err == nil || !strings.Contains(strings.ToLower(err.Error()), "superuser") {
			t.Errorf("boot over %s answered %v, want a refusal that names the superuser", role, err)
		}
	})

	t.Run("boot_refuses_a_bypassrls_role", func(t *testing.T) {
		clean := requestRoleFacts{role: requestRole}
		if err := checkRequestRole(clean); err != nil {
			t.Errorf("checkRequestRole refuses %+v, the facts of dispatch_app as the migration makes it: %v", clean, err)
		}
		facts := clean
		facts.bypassRLS = true
		if err := checkRequestRole(facts); err == nil || !strings.Contains(strings.ToLower(err.Error()), "bypassrls") {
			t.Errorf("checkRequestRole(%+v) = %v, want a refusal that names BYPASSRLS", facts, err)
		}
		role := scratchRole(t, ctx, f.system, "rls_boot_bypassrls", "bypassrls")
		conn := connectForTest(t, ctx, f.system)
		if _, err := conn.Exec(ctx, "set role "+role); err != nil {
			t.Fatalf("set role %s: %v", role, err)
		}
		read, err := readRequestRoleFacts(ctx, conn)
		if err != nil {
			t.Fatalf("read the facts of %s: %v", role, err)
		}
		if read.role != role || !read.bypassRLS || read.superuser {
			t.Errorf("the facts of %s read %+v, want its name and BYPASSRLS, no superuser", role, read)
		}
		if err := checkRequestRole(read); err == nil || !strings.Contains(strings.ToLower(err.Error()), "bypassrls") {
			t.Errorf("boot over %s answered %v, want a refusal that names BYPASSRLS", role, err)
		}
	})

	t.Run("boot_refuses_a_role_that_owns_a_table", func(t *testing.T) {
		clean := requestRoleFacts{role: requestRole}
		if err := checkRequestRole(clean); err != nil {
			t.Errorf("checkRequestRole refuses %+v, the facts of dispatch_app as the migration makes it: %v", clean, err)
		}
		facts := clean
		facts.ownsTable = true
		if err := checkRequestRole(facts); err == nil || !strings.Contains(strings.ToLower(err.Error()), "own") {
			t.Errorf("checkRequestRole(%+v) = %v, want a refusal that names the table it owns", facts, err)
		}
		other := clean
		other.role = "postgres"
		if err := checkRequestRole(other); err == nil || !strings.Contains(err.Error(), "postgres") {
			t.Errorf("checkRequestRole(%+v) = %v, want a refusal that names the role it runs as", other, err)
		}
		// A NOLOGIN role made to own a scratch table, as the pod's login may make it.
		role := scratchRole(t, ctx, f.system, "rls_boot_owner", "")
		if _, err := f.system.Pool.Exec(ctx, "grant create on schema public to "+role); err != nil {
			t.Fatalf("let %s create a table: %v", role, err)
		}
		conn := connectForTest(t, ctx, f.system)
		if _, err := conn.Exec(ctx, "set role "+role+"; create table rls_owned_scratch_"+randomDatabaseSuffix(t)+" (id int)"); err != nil {
			t.Fatalf("make a table %s owns: %v", role, err)
		}
		read, err := readRequestRoleFacts(ctx, conn)
		if err != nil {
			t.Fatalf("read the facts of %s: %v", role, err)
		}
		if read.role != role || !read.ownsTable || read.superuser || read.bypassRLS {
			t.Errorf("the facts of %s read %+v, want its name and a table it owns, no superuser, no BYPASSRLS", role, read)
		}
		read.role = requestRole
		if err := checkRequestRole(read); err == nil || !strings.Contains(strings.ToLower(err.Error()), "own") {
			t.Errorf("boot over a role that owns a table answered %v, want a refusal that names the table it owns", err)
		}
	})

	t.Run("bare_connection_with_no_setting_reads_no_row", func(t *testing.T) {
		request := f.requestPool(t)
		tables := policyTables(t, ctx, f.system)
		conn := bareRequestConnection(t, ctx, request)
		for _, guard := range []string{"projects", "issues", "events"} {
			if countRows(t, ctx, f.system.Pool, guard) == 0 {
				t.Fatalf("the owner reads no row of %s, so the bare connection's none proves nothing", guard)
			}
		}
		var leaked []string
		for _, table := range tables {
			if seen := countRows(t, ctx, conn, table); seen != 0 {
				leaked = append(leaked, fmt.Sprintf("%s (%d)", table, seen))
			}
		}
		if len(leaked) > 0 {
			t.Errorf("a bare connection of the request pool, with no viewer setting, reads rows of: %s", strings.Join(leaked, ", "))
		}
	})

	t.Run("reused_connection_after_a_viewer_reads_no_row", func(t *testing.T) {
		request := f.requestPool(t)
		conn := bareRequestConnection(t, ctx, request)
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `select set_config('role', $1, true), set_config('dispatch.viewer_sees', 'PRIV,PUB', true),
			set_config('dispatch.viewer_person', 'bob@example.com', true), set_config('dispatch.viewer_open', 'true', true),
			set_config('dispatch.viewer_session', '', true), set_config('dispatch.viewer_private', 'true', true)`, requestRole); err != nil {
			t.Fatalf("send a viewer's settings: %v", err)
		}
		seen := collectKeys(t)(tx.Query(ctx, bothKeys))
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(seen, []string{"PRIV-1", "PUB-1"}) {
			t.Fatalf("under the viewer's settings the connection reads %v, want PRIV-1 and PUB-1", seen)
		}
		var sees *string
		if err := conn.QueryRow(ctx, `select current_setting('dispatch.viewer_sees', true)`).Scan(&sees); err != nil {
			t.Fatal(err)
		}
		if sees == nil || *sees != "" {
			t.Errorf("after the viewer's transaction dispatch.viewer_sees reads %v, want '' (the reused connection's case)", sees)
		}
		if after := collectKeys(t)(conn.Query(ctx, bothKeys)); len(after) != 0 {
			t.Errorf("the connection, reused after a viewer's transaction, reads %v with no setting of its own, want no row", after)
		}
	})

	t.Run("pool_refuses_a_statement_with_no_viewer", func(t *testing.T) {
		request := f.requestPool(t)
		before := request.Stat().AcquireCount()
		_, err := request.Query(ctx, bothKeys)
		refusals := map[string]error{"Query": err}
		refusals["QueryRow"] = request.QueryRow(ctx, `select 1`).Scan(new(int))
		_, refusals["Exec"] = request.Exec(ctx, `select 1`)
		batch := &pgx.Batch{}
		batch.Queue(`select 1`)
		refusals["SendBatch"] = request.SendBatch(ctx, batch).Close()
		_, refusals["Begin"] = request.Begin(ctx)
		_, refusals["BeginTx"] = request.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
		for method, err := range refusals {
			if !errors.Is(err, ErrNoViewer) {
				t.Errorf("%s with no viewer answered %v, want ErrNoViewer", method, err)
			}
		}
		if after := request.Stat().AcquireCount(); after != before {
			t.Errorf("the refusals took %d connections, want none: a statement with no viewer is refused before any SQL", after-before)
		}
	})

	t.Run("a_reset_role_is_put_back_by_the_next_viewer_transaction", func(t *testing.T) {
		request := f.requestPool(t)
		viewed := WithViewer(ctx, seesPublic)
		tx, err := request.Begin(viewed)
		if err != nil {
			t.Fatal(err)
		}
		var pid uint32
		var reset string
		if err := tx.QueryRow(viewed, `select pg_backend_pid()`).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(viewed, `reset role`); err != nil {
			t.Fatalf("reset role on the request pool's connection: %v", err)
		}
		if err := tx.QueryRow(viewed, `select current_user::text`).Scan(&reset); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(viewed); err != nil {
			t.Fatal(err)
		}
		if reset != f.login {
			t.Fatalf("after reset role the transaction runs as %s, want the login %s: the row resets nothing", reset, f.login)
		}
		for range 2 * int(request.Config().MaxConns) {
			var backend uint32
			var user string
			var hidden int
			if err := request.QueryRow(viewed, `select pg_backend_pid(), current_user::text, (select count(*) from issues where key = 'PRIV-1')`).
				Scan(&backend, &user, &hidden); err != nil {
				t.Fatalf("read under the viewer after the reset: %v", err)
			}
			if backend != pid {
				continue
			}
			if user != requestRole || hidden != 0 {
				t.Errorf("the next viewer transaction on the connection whose role was reset runs as %s and reads PRIV-1 %d times, want %s and none", user, hidden, requestRole)
			}
			return
		}
		t.Fatalf("no viewer statement ran on backend %d, the connection whose role was reset", pid)
	})

	t.Run("a_reader_role_reads_every_row_with_no_viewer", func(t *testing.T) {
		tables := policyTables(t, ctx, f.system)
		role := scratchRole(t, ctx, f.system, "rls_reader", "")
		if _, err := f.system.Pool.Exec(ctx, "grant usage on schema public to "+role+"; grant select on all tables in schema public to "+role); err != nil {
			t.Fatalf("grant %s the reads of the read login: %v", role, err)
		}
		conn := connectForTest(t, ctx, f.system)
		if _, err := conn.Exec(ctx, "set role "+role); err != nil {
			t.Fatalf("set role %s: %v", role, err)
		}
		var short []string
		for _, table := range tables {
			if owned, read := countRows(t, ctx, f.system.Pool, table), countRows(t, ctx, conn, table); read != owned {
				short = append(short, fmt.Sprintf("%s (%d of %d)", table, read, owned))
			}
		}
		if len(short) > 0 {
			t.Errorf("a role other than %s with SELECT, and no viewer setting, reads fewer rows than the owner of: %s", requestRole, strings.Join(short, ", "))
		}
	})

	t.Run("two_databases_migrate_at_once_and_the_role_exists_once", func(t *testing.T) {
		before := versionBeforeTheRequestRole(t)
		databases := []*Store{openMigratedTestStore(t, before), openMigratedTestStore(t, before)}
		migrated := make(chan error, len(databases))
		for _, database := range databases {
			go func() { migrated <- database.Migrate(ctx) }()
		}
		for range databases {
			if err := <-migrated; err != nil {
				t.Errorf("one of two databases migrating at once: %v", err)
			}
		}
		var roles int
		if err := f.system.Pool.QueryRow(ctx, `select count(*) from pg_roles where rolname = $1`, requestRole).Scan(&roles); err != nil {
			t.Fatal(err)
		}
		if roles != 1 {
			t.Errorf("the cluster holds %d roles named %s, want one", roles, requestRole)
		}
		for index, database := range databases {
			var maySet, granted bool
			err := database.Pool.QueryRow(ctx, `select pg_has_role(current_user, $1, 'SET'), has_table_privilege($1, 'public.issues', 'SELECT')`, requestRole).Scan(&maySet, &granted)
			if err != nil || !maySet || !granted {
				t.Errorf("database %d after migrating: login may set role %t, %s reads issues %t (%v)", index+1, maySet, requestRole, granted, err)
			}
		}
	})

	t.Run("query_queryrow_exec_sendbatch_begin_begintx_read_only_seen_rows", func(t *testing.T) {
		request := f.requestPool(t)
		for _, view := range []struct {
			name   string
			viewer model.Viewer
			want   []string
		}{
			{"a viewer who sees PUB alone", seesPublic, []string{"PUB-1"}},
			{"a viewer who sees PRIV and PUB", seesBoth, []string{"PRIV-1", "PUB-1"}},
		} {
			viewed := WithViewer(ctx, view.viewer)
			var user string
			if err := request.QueryRow(viewed, `select current_user::text`).Scan(&user); err != nil || user != requestRole {
				t.Errorf("%s: QueryRow runs as %s (%v), want %s", view.name, user, err, requestRole)
			}
			got := map[string][]string{"Query": collectKeys(t)(request.Query(viewed, bothKeys))}
			var keys []string
			if err := request.QueryRow(viewed, `select coalesce(array_agg(key order by key), '{}') from issues where key in ('PRIV-1', 'PUB-1')`).Scan(&keys); err != nil {
				t.Fatalf("%s: QueryRow: %v", view.name, err)
			}
			got["QueryRow"] = keys
			tag, err := request.Exec(viewed, bothKeys)
			if err != nil {
				t.Fatalf("%s: Exec: %v", view.name, err)
			}
			if int(tag.RowsAffected()) != len(view.want) {
				t.Errorf("%s: Exec of the read reads %d rows, want %d", view.name, tag.RowsAffected(), len(view.want))
			}
			batch := &pgx.Batch{}
			batch.Queue(bothKeys)
			results := request.SendBatch(viewed, batch)
			got["SendBatch"] = collectKeys(t)(results.Query())
			if err := results.Close(); err != nil {
				t.Fatalf("%s: SendBatch: %v", view.name, err)
			}
			for method, options := range map[string]*pgx.TxOptions{"Begin": nil, "BeginTx read-only": {AccessMode: pgx.ReadOnly}} {
				var tx pgx.Tx
				if options == nil {
					tx, err = request.Begin(viewed)
				} else {
					tx, err = request.BeginTx(viewed, *options)
				}
				if err != nil {
					t.Fatalf("%s: %s: %v", view.name, method, err)
				}
				got[method] = collectKeys(t)(tx.Query(viewed, bothKeys))
				if err := tx.Commit(viewed); err != nil {
					t.Fatalf("%s: %s: commit: %v", view.name, method, err)
				}
			}
			for method, keys := range got {
				if !slices.Equal(keys, view.want) {
					t.Errorf("%s: %s reads %v, want %v", view.name, method, keys, view.want)
				}
			}
		}
	})

	t.Run("read_side_by_side_reads_under_the_viewer", func(t *testing.T) {
		request := f.requestPool(t)
		var first, second []string
		err := ReadSideBySide(WithViewer(ctx, seesPublic),
			func(ctx context.Context) error {
				rows, err := request.Query(ctx, bothKeys)
				if err != nil {
					return err
				}
				first, err = pgx.CollectRows(rows, pgx.RowTo[string])
				return err
			},
			func(ctx context.Context) error {
				return request.QueryRow(ctx, `select coalesce(array_agg(key order by key), '{}') from issues where key in ('PRIV-1', 'PUB-1')`).Scan(&second)
			})
		if err != nil {
			t.Fatalf("ReadSideBySide under a viewer: %v", err)
		}
		if !slices.Equal(first, []string{"PUB-1"}) || !slices.Equal(second, []string{"PUB-1"}) {
			t.Errorf("reads side by side under a viewer who sees PUB alone read %v and %v, want PUB-1 each", first, second)
		}
	})

	t.Run("a_write_commits_and_an_error_rolls_back", func(t *testing.T) {
		request := f.requestPool(t)
		viewed := WithViewer(ctx, seesBoth)
		const insertIssue = `insert into issues (key, project_key, number, title, rank, created_by) values ($1, $2, $3, 'A write of the request pool', 'm', '{"kind":"user","id":"bob@example.com"}')`
		stored := func(key string) bool {
			t.Helper()
			var count int
			if err := f.system.Pool.QueryRow(ctx, `select count(*) from issues where key = $1`, key).Scan(&count); err != nil {
				t.Fatal(err)
			}
			return count == 1
		}
		tx, err := request.Begin(viewed)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(viewed, insertIssue, "PUB-2", "PUB", 2); err != nil {
			t.Fatalf("write in a transaction: %v", err)
		}
		if err := tx.Commit(viewed); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if !stored("PUB-2") {
			t.Error("a committed transaction's write is not stored")
		}
		if _, err := request.Exec(viewed, insertIssue, "PUB-3", "PUB", 3); err != nil {
			t.Fatalf("Exec of a write: %v", err)
		}
		if !stored("PUB-3") {
			t.Error("Exec's write is not stored: its own transaction must commit")
		}
		tx, err = request.Begin(viewed)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(viewed, insertIssue, "PUB-4", "PUB", 4); err != nil {
			t.Fatalf("write in a transaction: %v", err)
		}
		if _, err := tx.Exec(viewed, `select 1 / 0`); err == nil {
			t.Fatal("a division by zero did not fail")
		}
		_ = tx.Rollback(viewed)
		if stored("PUB-4") {
			t.Error("the write of a transaction that failed is stored")
		}
		batch := &pgx.Batch{}
		batch.Queue(insertIssue, "PUB-5", "PUB", 5)
		batch.Queue(`select 1 / 0`)
		if err := request.SendBatch(viewed, batch).Close(); err == nil {
			t.Error("a batch whose second statement divides by zero did not fail")
		}
		if stored("PUB-5") {
			t.Error("the first write of a batch that failed is stored: the batch's own transaction must roll back")
		}
		if _, err := request.Exec(WithViewer(ctx, seesPublic), insertIssue, "PRIV-2", "PRIV", 2); err == nil {
			t.Error("a viewer who does not see PRIV wrote an issue of PRIV: the policy's with check must refuse it")
		}
		if stored("PRIV-2") {
			t.Error("a write the policy refused is stored")
		}
	})

	t.Run("acquire_copy_and_side_pools_are_refused_on_the_request_pool", func(t *testing.T) {
		request := f.requestPool(t)
		viewed := WithViewer(ctx, seesBoth)
		refusals := map[string]error{}
		_, refusals["Acquire"] = request.Acquire(viewed)
		refusals["AcquireFunc"] = request.AcquireFunc(viewed, func(*pgxpool.Conn) error { return nil })
		_, refusals["CopyFrom"] = request.CopyFrom(viewed, pgx.Identifier{"issues"}, []string{"key"}, pgx.CopyFromRows(nil))
		_, refusals["Listener"] = request.Listener(viewed)
		_, refusals["Dedicated"] = request.Dedicated(viewed, "dispatch-request-pool-row")
		_, refusals["Rooms"] = request.Rooms()
		_, refusals["RoomAppends"] = request.RoomAppends()
		_, refusals["Compaction"] = request.Compaction()
		_, refusals["Horizon"] = request.Horizon()
		_, refusals["Outbox"] = request.Outbox()
		_, refusals["TextTrees"] = request.TextTrees()
		_, refusals["Leases"] = request.Leases()
		_, refusals["Healthy"] = request.Healthy(viewed)
		for method, err := range refusals {
			if !errors.Is(err, ErrRequestPoolRefuses) {
				t.Errorf("%s on the request pool answered %v, want ErrRequestPoolRefuses", method, err)
			}
		}
		if idle := request.AcquireAllIdle(viewed); len(idle) != 0 {
			for _, conn := range idle {
				conn.Release()
			}
			t.Errorf("AcquireAllIdle on the request pool handed out %d connections, want none", len(idle))
		}
	})

	t.Run("a_read_at_a_cut_takes_the_viewer", func(t *testing.T) {
		kinds := []struct{ table, hidden string }{
			{"projects", `select count(*) from projects where key = 'PRIV'`},
			{"issues", `select count(*) from issues where key = 'PRIV-1'`},
			{"events", `select count(*) from events where issue_key = 'PRIV-1'`},
			{"asks", `select count(*) from asks where issue_key = 'PRIV-1'`},
			{"comments", `select count(*) from comments where issue_key = 'PRIV-1'`},
			{"messages", `select count(*) from messages where issue_key = 'PRIV-1'`},
			{"artifacts", `select count(*) from artifacts where issue_key = 'PRIV-1'`},
			{"artifact_versions", `select count(*) from artifact_versions where artifact_id = '` + f.documents["PRIV"] + `'::uuid`},
		}
		read := func(q interface {
			QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
		}, ctx context.Context, table, sql string) int {
			t.Helper()
			var count int
			if err := q.QueryRow(ctx, sql).Scan(&count); err != nil {
				t.Fatalf("read %s at the cut: %v", table, err)
			}
			return count
		}
		// Each shadowed kind holds PRIV's row at the cut for the owner, or the viewer's none
		// proves nothing.
		owned := make([]int, len(kinds))
		for index, kind := range kinds {
			if owned[index] = read(f.system.Pool, asof.WithCut(ctx, f.cut), kind.table, kind.hidden); owned[index] == 0 {
				t.Errorf("%s: the owner reads no row of PRIV at the cut, so the viewer's none proves nothing", kind.table)
			}
		}
		request := f.requestPool(t)
		for index, kind := range kinds {
			if seen := read(request, asof.WithCut(WithViewer(ctx, seesBoth), f.cut), kind.table, kind.hidden); seen != owned[index] {
				t.Errorf("%s: a viewer who sees PRIV reads %d of PRIV's rows at the cut, want the owner's %d", kind.table, seen, owned[index])
			}
			if hidden := read(request, asof.WithCut(WithViewer(ctx, seesPublic), f.cut), kind.table, kind.hidden); hidden != 0 {
				t.Errorf("%s: a viewer who does not see PRIV reads %d of PRIV's rows at the cut, want none, as for a key nobody holds", kind.table, hidden)
			}
		}
	})

	t.Run("the_system_work_mark_runs_on_the_system_pool", func(t *testing.T) {
		request := f.requestPool(t)
		marked := WithSystemWork(ctx)
		var user string
		var seen int
		if err := request.QueryRow(marked, `select current_user::text, (select count(*) from issues where key = 'PRIV-1')`).Scan(&user, &seen); err != nil {
			t.Fatalf("read under the system-work mark: %v", err)
		}
		if user != f.login || seen != 1 {
			t.Errorf("under the system-work mark the request pool runs as %s and reads PRIV-1 %d times, want the owner %s and once", user, seen, f.login)
		}
		tx, err := request.Begin(marked)
		if err != nil {
			t.Fatalf("Begin under the system-work mark: %v", err)
		}
		defer tx.Rollback(marked)
		if err := tx.QueryRow(marked, `select current_user::text`).Scan(&user); err != nil || user != f.login {
			t.Errorf("a transaction under the system-work mark runs as %s (%v), want the owner %s", user, err, f.login)
		}
	})

	t.Run("a_viewer_wins_over_the_system_work_mark", func(t *testing.T) {
		request := f.requestPool(t)
		for name, both := range map[string]context.Context{
			"the viewer over the mark": WithViewer(WithSystemWork(ctx), seesPublic),
			"the mark over the viewer": WithSystemWork(WithViewer(ctx, seesPublic)),
		} {
			var user string
			var hidden int
			if err := request.QueryRow(both, `select current_user::text, (select count(*) from issues where key = 'PRIV-1')`).Scan(&user, &hidden); err != nil {
				t.Fatalf("%s: read: %v", name, err)
			}
			if user != requestRole || hidden != 0 {
				t.Errorf("%s: the request pool runs as %s and reads PRIV-1 %d times, want %s under the viewer and none", name, user, hidden, requestRole)
			}
		}
	})

	t.Run("neither_viewer_nor_mark_is_refused", func(t *testing.T) {
		request := f.requestPool(t)
		if err := request.QueryRow(ctx, `select 1`).Scan(new(int)); !errors.Is(err, ErrNoViewer) {
			t.Errorf("QueryRow with neither a viewer nor the mark answered %v, want ErrNoViewer", err)
		}
		if _, err := request.Begin(ctx); !errors.Is(err, ErrNoViewer) {
			t.Errorf("Begin with neither a viewer nor the mark answered %v, want ErrNoViewer", err)
		}
	})

	t.Run("as_owner_sees_hidden_rows_and_switches_back_even_when_fn_fails", func(t *testing.T) {
		request := f.requestPool(t)
		viewed := WithViewer(ctx, seesPublic)
		tx, err := request.Begin(viewed)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(viewed)
		asTheViewer := func(when string) {
			t.Helper()
			var user string
			var hidden int
			if err := tx.QueryRow(viewed, `select current_user::text, (select count(*) from issues where key = 'PRIV-1')`).Scan(&user, &hidden); err != nil {
				t.Fatalf("%s: read: %v", when, err)
			}
			if user != requestRole || hidden != 0 {
				t.Errorf("%s the transaction runs as %s and reads PRIV-1 %d times, want %s and none", when, user, hidden, requestRole)
			}
		}
		asTheViewer("before AsOwner")
		ownerRead := func(tx pgx.Tx) (string, int) {
			t.Helper()
			var user string
			var seen int
			if err := tx.QueryRow(viewed, `select current_user::text, (select count(*) from issues where key = 'PRIV-1')`).Scan(&user, &seen); err != nil {
				t.Fatalf("read inside AsOwner: %v", err)
			}
			return user, seen
		}
		var user string
		var seen int
		if err := AsOwner(viewed, tx, func(tx pgx.Tx) error {
			user, seen = ownerRead(tx)
			return nil
		}); err != nil {
			t.Fatalf("AsOwner: %v", err)
		}
		if user != f.login || seen != 1 {
			t.Errorf("inside AsOwner the rule runs as %s and reads PRIV-1 %d times, want the owner %s and once", user, seen, f.login)
		}
		asTheViewer("after AsOwner")
		refused := errors.New("the rule refuses the write")
		if err := AsOwner(viewed, tx, func(tx pgx.Tx) error {
			user, seen = ownerRead(tx)
			return refused
		}); !errors.Is(err, refused) {
			t.Errorf("AsOwner over a rule that fails answered %v, want the rule's error", err)
		}
		asTheViewer("after AsOwner over a rule that failed")
	})

	t.Run("as_owner_on_a_system_transaction_leaves_the_owner_role", func(t *testing.T) {
		tx, err := f.system.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		var inside, after string
		if err := AsOwner(ctx, tx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `select current_user::text`).Scan(&inside)
		}); err != nil {
			t.Fatalf("AsOwner on a system-pool transaction: %v", err)
		}
		if err := tx.QueryRow(ctx, `select current_user::text`).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if inside != f.login || after != f.login {
			t.Errorf("AsOwner on a system-pool transaction ran as %s and left %s, want the owner %s both times", inside, after, f.login)
		}
	})

	t.Run("require_viewer_refuses_a_marked_context_with_no_viewer", func(t *testing.T) {
		if err := RequireViewer(ctx); !errors.Is(err, ErrNoViewer) {
			t.Fatalf("RequireViewer of a context with neither a viewer nor the mark answered %v, want ErrNoViewer", err)
		}
		if err := RequireViewer(WithSystemWork(ctx)); !errors.Is(err, ErrNoViewer) {
			t.Errorf("RequireViewer of a context marked as system work with no viewer answered %v, want ErrNoViewer", err)
		}
		if err := RequireViewer(WithViewer(WithSystemWork(ctx), seesPublic)); err != nil {
			t.Errorf("RequireViewer of a marked context with a viewer answered %v, want nil", err)
		}
		if viewer, ok := ViewerFrom(WithViewer(ctx, seesPublic)); !ok || viewer.Person != seesPublic.Person || !slices.Equal(viewer.Sees, seesPublic.Sees) {
			t.Errorf("ViewerFrom gives %+v, %t, want the viewer the context carries", viewer, ok)
		}
	})

	// dispatch_repo_visible, the repository rule of plan D11, read under dispatch_app for limited
	// members: an issue resolves to the repository the nearest issue of its own project on its
	// parent chain names, else its project's default repository (refs.IssueRepoTerms), so RLSP's
	// member reads org/rls-p-own, which RLSP-3 takes from its parent RLSP-2, and RLSP's default,
	// and not org/rls-q, which only RLSQ-1 names, though RLSP-1's parent is RLSQ-1.
	t.Run("dispatch_repo_visible_same_project_chain_and_default", func(t *testing.T) {
		request := f.requestPool(t)
		for _, sql := range []string{
			`insert into projects (key, name, default_assignee, visibility, default_repo, created_at) values
				('RLSP', 'RLSP', 'bob@example.com', 'private', 'org/rls-p-default', now()),
				('RLSQ', 'RLSQ', 'bob@example.com', 'private', null, now())`,
			`insert into issues (key, project_key, number, title, status, labels, rank, created_by, created_at, updated_at, parent_key, repo) values
				('RLSQ-1', 'RLSQ', 1, 'Q root', 'triage', '{}', 'm', '{}', now(), now(), null, 'org/rls-q'),
				('RLSP-2', 'RLSP', 2, 'P root', 'triage', '{}', 'm', '{}', now(), now(), null, 'org/rls-p-own'),
				('RLSP-1', 'RLSP', 1, 'P under Q', 'triage', '{}', 'm', '{}', now(), now(), 'RLSQ-1', null),
				('RLSP-3', 'RLSP', 3, 'P under P', 'triage', '{}', 'm', '{}', now(), now(), 'RLSP-2', null)`,
		} {
			if _, err := f.system.Pool.Exec(ctx, sql); err != nil {
				t.Fatalf("seed the repository rule's projects and issues: %v", err)
			}
		}
		member := model.Viewer{Person: "dora@example.com", Sees: []string{"RLSP"}, AnyPrivate: true}
		ofQ := model.Viewer{Person: "erin@example.com", Sees: []string{"RLSQ"}, AnyPrivate: true}
		nobody := model.Viewer{Person: "frank@example.com", AnyPrivate: true}
		for _, read := range []struct {
			viewer model.Viewer
			repo   string
			want   bool
		}{
			{member, "org/rls-p-own", true}, {member, "org/rls-p-default", true}, {member, "org/rls-q", false},
			{ofQ, "org/rls-q", true}, {ofQ, "org/rls-p-own", false},
			{nobody, "org/rls-p-own", false}, {nobody, "org/rls-p-default", false}, {nobody, "org/rls-q", false},
		} {
			var visible bool
			if err := request.QueryRow(WithViewer(ctx, read.viewer), `select dispatch_repo_visible($1)`, read.repo).Scan(&visible); err != nil {
				t.Fatalf("%s: dispatch_repo_visible(%s): %v", read.viewer.Person, read.repo, err)
			}
			if visible != read.want {
				t.Errorf("%s reads %s %t, want %t: an issue resolves to the repository its own project's chain names, else its project's default (refs.IssueRepoTerms), never to a parent's of another project", read.viewer.Person, read.repo, visible, read.want)
			}
		}
	})
}
