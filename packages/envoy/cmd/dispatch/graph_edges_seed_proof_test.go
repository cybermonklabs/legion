package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/startup"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// seedDatabaseEnv names a database restored from a deployment's dump, at any earlier migration,
// which TestTheIssueTreesStepKeepsEveryEdgeOverASeed reads a copy of. A cluster Job sets it; no
// other run does.
const seedDatabaseEnv = "DISPATCH_SEED_DATABASE_URL"

// seedWaitEnv bounds how long the test waits for the copy's boot to migrate it and for its
// backfills to owe nothing, as a Go duration; empty is 90 minutes.
const seedWaitEnv = "DISPATCH_SEED_WAIT"

// The issue tree's step keeps every edge over a deployment's data (dispatch://INFRA-1098's T7b,
// c-view-one-select): over a copy of the database seedDatabaseEnv names, made with CREATE DATABASE
// … TEMPLATE, brought through 0116 and given a default assignee for each project that names none
// (nameDefaultAssignees), the root's build boots, migrating it to this head; the test waits until its
// backfills owe nothing - the boot's own run, then graphcensus.Backfill until every pass is
// complete, and the block index owed by no document - and reads graph_edges. It then announces a
// build that lists every kind this build lists, stores_moved among them, as a fleet of a build of
// this release would (the guard of dispatch://INFRA-1253's arms, startup.StoresMovedKinds), boots
// the root's build again, whose step deletes dispatch://INFRA-1150's and dispatch://INFRA-1253's
// arms and the tuple arm's reads of a document's artifacts and refs rows, and reads graph_edges
// again: each read EXCEPT ALL the other is empty, both ways. It prints both counts and every line
// the step logged, and fails where the issue tree's step kept its arms, naming what held them. It
// drops its own copy and nothing else.
//
// Without seedDatabaseEnv it does not run. With it, anything missing fails it.
func TestTheIssueTreesStepKeepsEveryEdgeOverASeed(t *testing.T) {
	seedURL := os.Getenv(seedDatabaseEnv)
	if seedURL == "" {
		t.Skipf("%s names no seed database; a cluster Job that restores one runs this test", seedDatabaseEnv)
	}
	wait := 90 * time.Minute
	if raw := os.Getenv(seedWaitEnv); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			t.Fatalf("%s=%q is no duration: %v", seedWaitEnv, raw, err)
		}
		wait = parsed
	}
	ctx := context.Background()
	copyURL := copySeedDatabase(t, seedURL)
	database := openSeedStore(t, copyURL)
	// A deployment's dump can predate 0116, which adds projects.default_assignee, and its people
	// need not have signed in on this server, which the migration that makes that column not
	// null reads (20261111210103): the copy is brought through 0116 first, and each project that
	// names no default assignee is given one, as an operator does before deploying again.
	if err := database.MigrateThrough(ctx, defaultAssigneeMigration); err != nil {
		t.Fatalf("migrate the copy through %04d: %v", defaultAssigneeMigration, err)
	}
	nameDefaultAssignees(t, ctx, database)
	rootBuild := cmdtest.BuildStamped(t, "envoy-dispatch-seed-root", oneSelectRootBuild)

	first := startDispatchProcessWith(t, copyURL, dispatchOptions{Binary: rootBuild, SigningKey: multiprocessSigningKey})
	if err := first.healthyWithin(wait); err != nil {
		t.Fatal(err)
	}
	waitForTheBackfills(t, ctx, database, first, wait)
	stopDeployedProcess(t, first)
	scanSeed := func(what, statement string) int64 {
		t.Helper()
		var n int64
		if err := database.Pool.QueryRow(ctx, statement).Scan(&n); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		t.Logf("seed proof: %s = %d", what, n)
		return n
	}
	if _, err := database.Pool.Exec(ctx, `create table seed_proof_graph_edges as select * from graph_edges`); err != nil {
		t.Fatalf("read graph_edges before the step: %v", err)
	}
	scanSeed("graph_edges rows before the step", `select count(*) from seed_proof_graph_edges`)
	scanSeed("refs rows", `select count(*) from refs`)
	scanSeed("refs rows past the store's bound", `select count(*) from refs where octet_length(to_id) > 1024`)
	scanSeed("document mentions before the step", `select count(*) from seed_proof_graph_edges where from_kind = 'artifact' and kind = 'mentions'`)
	scanSeed("attached_to before the step", `select count(*) from seed_proof_graph_edges where from_kind = 'artifact' and kind = 'attached_to'`)

	if _, err := database.Pool.Exec(ctx, `insert into server_build_announcements (commit, tuple_kinds) values ($1, $2)`,
		oneSelectChildrenRelease, refs.TupleKinds()); err != nil {
		t.Fatalf("announce the children's release: %v", err)
	}
	second := startDispatchProcessWith(t, copyURL, dispatchOptions{Binary: rootBuild, SigningKey: multiprocessSigningKey})
	if err := second.healthyWithin(wait); err != nil {
		t.Fatal(err)
	}
	var logged []string
	for _, line := range strings.Split(second.Output.String(), "\n") {
		if strings.Contains(line, "retirement") {
			logged = append(logged, line)
		}
	}
	t.Logf("seed proof: the step logged:\n%s", strings.Join(logged, "\n"))
	if !strings.Contains(second.Output.String(), deletedLine(startup.IssueTreeArmsRetirement)) {
		t.Errorf("the issue tree's step kept its arms over the seed; its lines above name what held them")
	}
	gone := scanSeed("before EXCEPT ALL after", `select count(*) from (select * from seed_proof_graph_edges except all select * from graph_edges) d`)
	came := scanSeed("after EXCEPT ALL before", `select count(*) from (select * from graph_edges except all select * from seed_proof_graph_edges) d`)
	scanSeed("graph_edges rows after the step", `select count(*) from graph_edges`)
	if gone != 0 || came != 0 {
		t.Errorf("graph_edges lost %d rows and gained %d over the step, want none either way", gone, came)
	}
	stopDeployedProcess(t, second)
}

// copySeedDatabase makes a copy of the database seedURL names under a name of its own, with
// CREATE DATABASE … TEMPLATE, and answers its URL. When the test ends it drops that copy, by its
// name and with (force), from a connection to the server's postgres database, never to the copy
// itself; the test's own connections to the copy are closed before that (t.Cleanup runs in the
// reverse order, and every connection to the copy is opened after this). seedURL must be a
// postgres:// URL, whose path names the database.
func copySeedDatabase(t *testing.T, seedURL string) string {
	t.Helper()
	ctx := context.Background()
	parsed, err := url.Parse(seedURL)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		t.Fatalf("%s is no postgres:// URL", seedDatabaseEnv)
	}
	seed := strings.TrimPrefix(parsed.Path, "/")
	if seed == "" {
		t.Fatalf("%s names no database", seedDatabaseEnv)
	}
	at := func(database string) string {
		named := *parsed
		named.Path = "/" + database
		return named.String()
	}
	copied := fmt.Sprintf("seed_proof_%d", time.Now().UnixNano())
	admin, err := pgx.Connect(ctx, at("postgres"))
	if err != nil {
		t.Fatalf("connect to the seed's server: %v", err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, `create database `+pgx.Identifier{copied}.Sanitize()+` template `+pgx.Identifier{seed}.Sanitize()); err != nil {
		t.Fatalf("copy the seed database %s: %v", seed, err)
	}
	t.Logf("seed proof: copied %s to %s", seed, copied)
	t.Cleanup(func() {
		conn, err := pgx.Connect(context.Background(), at("postgres"))
		if err != nil {
			t.Errorf("connect to drop the copy %s: %v", copied, err)
			return
		}
		defer conn.Close(context.Background())
		if _, err := conn.Exec(context.Background(), `drop database if exists `+pgx.Identifier{copied}.Sanitize()+` with (force)`); err != nil {
			t.Errorf("drop the copy %s: %v", copied, err)
		}
	})
	return at(copied)
}

// defaultAssigneeMigration is 0116_project_default_assignee, which adds projects.default_assignee,
// the column the migration that makes it not null (20261111210103) refuses to leave empty.
const defaultAssigneeMigration = 116

// seedProofAssignee is the default assignee a project takes when nothing in the seed names a
// person: the migration needs only a non-empty lower-case login, and no write of this test is
// made as it.
const seedProofAssignee = "seed-proof@example.invalid"

// nameDefaultAssignees gives each project of database that names no default assignee one, printed:
// the person its issues are assigned to most; else the person who created most of its issues;
// else the person the seed's issues are assigned to most; else seedProofAssignee. INFRA-639's
// seed assigns no issue, so its project takes the person who created most of its issues.
func nameDefaultAssignees(t *testing.T, ctx context.Context, database *store.Store) {
	t.Helper()
	rows, err := database.Pool.Query(ctx, `
		with held as (
			select project_key, lower(assignee) as person, count(*) as issues
			from issues where coalesce(assignee, '') <> ''
			group by project_key, lower(assignee)
		), made as (
			select project_key, lower(created_by->>'id') as person, count(*) as issues
			from issues where coalesce(created_by->>'id', '') <> ''
			group by project_key, lower(created_by->>'id')
		), overall as (
			select person from held group by person order by sum(issues) desc, person limit 1
		)
		select p.key, picked.person, picked.source
		from projects p
		cross join lateral (
			select person, source from (
				(select h.person, 'its issues'' most frequent assignee' as source, 1 as rank
				 from held h where h.project_key = p.key order by h.issues desc, h.person limit 1)
				union all
				(select m.person, 'no issue of it is assigned, so the person who created most of its issues', 2
				 from made m where m.project_key = p.key order by m.issues desc, m.person limit 1)
				union all
				(select o.person, 'nothing of it names a person, so the seed''s most frequent assignee', 3 from overall o)
				union all
				select $1::text, 'nothing in the seed names a person, so the test''s own placeholder', 4
			) candidates order by rank limit 1
		) picked
		where p.default_assignee is null
		order by p.key
	`, seedProofAssignee)
	if err != nil {
		t.Fatalf("read the projects that name no default assignee: %v", err)
	}
	type named struct {
		project, person, source string
	}
	projects, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (named, error) {
		var each named
		err := row.Scan(&each.project, &each.person, &each.source)
		return each, err
	})
	if err != nil {
		t.Fatalf("read the projects that name no default assignee: %v", err)
	}
	for _, each := range projects {
		t.Logf("seed proof: project %s takes the default assignee %s (%s)", each.project, each.person, each.source)
		if _, err := database.Pool.Exec(ctx, `update projects set default_assignee = $2 where key = $1`, each.project, each.person); err != nil {
			t.Fatalf("name the default assignee of %s: %v", each.project, err)
		}
	}
}

// openSeedStore opens a store on the copy, closed when the test ends.
func openSeedStore(t *testing.T, databaseURL string) *store.Store {
	t.Helper()
	database, err := store.Open(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("open the copy: %v", err)
	}
	t.Cleanup(database.Pool.Close)
	return database
}

// waitForTheBackfills waits up to within for process's boot to have run the graph store's
// backfill, then runs graphcensus.Backfill until every pass is complete, and waits until no
// document owes its block index, failing on what is still owed when the time is up.
func waitForTheBackfills(t *testing.T, ctx context.Context, database *store.Store, process *dispatchProcess, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !strings.Contains(process.Output.String(), bootBackfillDoneLine) {
		if time.Now().After(deadline) {
			t.Fatalf("the boot never logged %q in %s:\n%s", bootBackfillDoneLine, within, process.Output.String())
		}
		time.Sleep(time.Second)
	}
	for {
		report, err := graphcensus.Backfill(ctx, database, io.Discard)
		if err != nil {
			if _, only := graphcensus.OnlyProjectMoved(err); !only {
				t.Fatalf("backfill the copy: %v", err)
			}
		}
		var owed []string
		for _, kind := range report.Kinds {
			if !kind.Complete {
				owed = append(owed, fmt.Sprintf("%s remaining=%d", kind.Kind, kind.Remaining))
			}
		}
		var unindexed int64
		if err := database.Pool.QueryRow(ctx, `
			select count(*) from document_block_versions where version is null or store_owed_at is not null
		`).Scan(&unindexed); err != nil {
			t.Fatalf("count the documents owed their block index: %v", err)
		}
		if len(owed) == 0 && unindexed == 0 {
			t.Logf("seed proof: the backfills owe nothing")
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after %s the backfills still owe %v and %d documents their block index", within, owed, unindexed)
		}
		time.Sleep(10 * time.Second)
	}
}
