package store

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/pgmigrate"
	"github.com/sjawhar/envoy/internal/testwait"
)

// ladderTestSet is a set of two migrations for a ladder of a test's own, the second building on
// the first, so a build through version 2 applies two migrations and one through version 1 one.
var ladderTestSet = fstest.MapFS{
	"migrations/0001_things.up.sql": {Data: []byte(`create table things (id int primary key)`)},
	"migrations/0002_parts.up.sql":  {Data: []byte(`create table parts (id int primary key, thing_id int references things (id))`)},
}

// ladderOver is a template ladder of the test's own over set, files named
// migrations/<version>_<name>.up.sql, whose templates are dropped when the test ends.
func ladderOver(t *testing.T, admin url.URL, set fstest.MapFS) *templateLadder {
	t.Helper()
	migrations, err := pgmigrate.Load(set, "migrations")
	if err != nil {
		t.Fatalf("load the ladder's migrations: %v", err)
	}
	ladder := newTemplateLadder(func(*testing.T) []pgmigrate.Migration { return migrations })
	t.Cleanup(func() {
		if err := ladder.dropAll(admin); err != nil {
			t.Errorf("drop the ladder's templates: %v", err)
		}
	})
	return ladder
}

// databasesNamed counts the databases called name on the server admin names.
func databasesNamed(t *testing.T, admin url.URL, name string) int {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin.String())
	if err != nil {
		t.Fatalf("connect to the Postgres server at %s: %v", admin.Host, err)
	}
	defer conn.Close(ctx)
	var count int
	if err := conn.QueryRow(ctx, `select count(*) from pg_database where datname = $1`, name).Scan(&count); err != nil {
		t.Fatalf("count the databases named %s: %v", name, err)
	}
	return count
}

// Two asks for one version at once make one template, and both answer it: the first makes the
// version's entry and builds it, and the other finds the entry and waits for that build. A ladder
// that let both build would record two names and leave two databases. Each ask is a subtest that
// a goroutine of its own runs, so the two start together and a failure ends the ask's own test.
func TestTheLadderBuildsOneTemplateForTwoAsksOfOneVersion(t *testing.T) {
	t.Parallel()
	admin := adminDatabaseURL(t)
	ladder := ladderOver(t, admin, ladderTestSet)
	var answers [2]string
	returned := make(chan struct{}, len(answers))
	for i := range answers {
		go func() {
			defer func() { returned <- struct{}{} }()
			t.Run(fmt.Sprintf("ask %d", i+1), func(t *testing.T) { answers[i] = ladder.template(t, admin, 2) })
		}()
	}
	for range answers {
		testwait.Receive(t, "an ask for version 2 to return", returned)
	}
	if t.Failed() {
		t.FailNow()
	}
	ladder.mu.Lock()
	made := slices.Clone(ladder.made)
	ladder.mu.Unlock()
	if len(made) != 1 || answers[0] != made[0] || answers[1] != made[0] {
		t.Fatalf("the two asks answered %q and %q, and the ladder made %q: want one template, which both answer", answers[0], answers[1], made)
	}
	if got := databasesNamed(t, admin, made[0]); got != 1 {
		t.Errorf("the server holds %d databases named %s, want 1", got, made[0])
	}
}

// An ask waits for no build it does not start from. The test holds the build of version 2 just
// before it applies migration 2, then asks for version 1, which starts from no template, and that
// ask returns while the build of version 2 is still held. A ladder that held one lock through
// every build kept the ask for version 1 behind the held build until the wait for it gave up.
// Each ask is a subtest, the one for version 2 run by a goroutine of the test's own and the one for
// version 1 by the goroutine testwait.Returns starts, so a failure ends the ask's own test.
func TestALadderAskWaitsForNoBuildItDoesNotStartFrom(t *testing.T) {
	t.Parallel()
	admin := adminDatabaseURL(t)
	ladder := ladderOver(t, admin, ladderTestSet)
	reached, release := make(chan struct{}), make(chan struct{})
	ladder.applying = func(t *testing.T, migration pgmigrate.Migration) {
		if migration.Version == 2 {
			close(reached)
			testwait.Receive(t, "the test to let the build of version 2 apply migration 2", release)
		}
	}
	var releasing sync.Once
	letGo := func() { releasing.Do(func() { close(release) }) }
	defer letGo()
	asked := make(chan struct{})
	go func() {
		defer close(asked)
		t.Run("version 2", func(t *testing.T) { ladder.template(t, admin, 2) })
	}()
	testwait.Receive(t, "the build of version 2 to reach migration 2", reached)
	testwait.Returns(t, "the ask for version 1 while the build of version 2 is held before migration 2", func() {
		t.Run("version 1", func(t *testing.T) { ladder.template(t, admin, 1) })
	})
	ladder.mu.Lock()
	held := ladder.rungs[2]
	ladder.mu.Unlock()
	select {
	case <-held.built:
		t.Fatal("the build of version 2 ended while the test held it, so the ask for version 1 proves nothing")
	default:
	}
	letGo()
	testwait.Receive(t, "the ask for version 2 to return once the test let its build go on", asked)
	if t.Failed() {
		t.FailNow()
	}
	ladder.mu.Lock()
	made := len(ladder.made)
	ladder.mu.Unlock()
	if made != 2 {
		t.Errorf("the ladder made %d templates for the asks for versions 1 and 2, want 2", made)
	}
}

// The drop removes each template the ladder made, and names each one it cannot drop: after it, the
// server holds no database of the name of the ladder's one template; and on a server that refuses
// the connection, its error names the ladder's template.
func TestTheLadderDropLeavesNoTemplateAndNamesOneItCannotDrop(t *testing.T) {
	t.Parallel()
	admin := adminDatabaseURL(t)
	ladder := ladderOver(t, admin, ladderTestSet)
	name := ladder.template(t, admin, 1)
	if got := databasesNamed(t, admin, name); got != 1 {
		t.Fatalf("control: before the drop the server holds %d databases named %s, want 1", got, name)
	}
	if err := ladder.dropAll(admin); err != nil {
		t.Fatalf("drop the ladder's templates: %v", err)
	}
	if got := databasesNamed(t, admin, name); got != 0 {
		t.Errorf("after the drop the server holds %d databases named %s, want none", got, name)
	}

	unreachable := ladderOver(t, admin, ladderTestSet)
	recorded := "dispatch_template_" + randomDatabaseSuffix(t)
	unreachable.made = []string{recorded}
	refusing := admin
	refusing.Host = "127.0.0.1:1"
	err := unreachable.dropAll(refusing)
	if err == nil || !strings.Contains(err.Error(), recorded) {
		t.Errorf("the drop on a server that refuses the connection returned %v, want an error naming %s", err, recorded)
	}
}

// The ladder records pgmigrate.CensusTables of each migration it applies, read on the template
// just before it applies it. On the package's ladder, once the newest template is built, the
// record holds one entry for each shipped migration. On a ladder over a set whose third migration
// deletes from parent, which a foreign key of things cascades from, the entry of that migration
// names things, which no statement of it names: a reading of the text alone would miss it.
func TestTheLadderRecordsTheCensusTablesOfEachMigrationItApplies(t *testing.T) {
	t.Parallel()
	admin := adminDatabaseURL(t)
	t.Run("every shipped migration", func(t *testing.T) {
		migratedTemplates.template(t, admin, latestVersion)
		var want []int
		for _, migration := range shippedMigrations(t) {
			want = append(want, migration.Version)
		}
		migratedTemplates.mu.Lock()
		got := slices.Sorted(maps.Keys(migratedTemplates.census))
		migratedTemplates.mu.Unlock()
		if !slices.Equal(got, want) {
			missing := slices.DeleteFunc(slices.Clone(want), func(version int) bool { return slices.Contains(got, version) })
			extra := slices.DeleteFunc(slices.Clone(got), func(version int) bool { return slices.Contains(want, version) })
			t.Errorf("the package's ladder recorded the census tables of %d migrations, want one entry for each of the %d shipped: no entry for %v, an entry for %v, which is not shipped", len(got), len(want), missing, extra)
		}
	})
	t.Run("a table a cascade reaches", func(t *testing.T) {
		ladder := ladderOver(t, admin, fstest.MapFS{
			"migrations/0001_tables.up.sql":  {Data: []byte(`create table parent (id int primary key); create table things (id int primary key, parent_id int);`)},
			"migrations/0002_cascade.up.sql": {Data: []byte(`alter table things add foreign key (parent_id) references parent (id) on delete cascade;`)},
			"migrations/0003_delete.up.sql":  {Data: []byte(`delete from parent;`)},
		})
		ladder.template(t, admin, 3)
		tables, recorded := ladder.censusTables(3)
		if !recorded {
			t.Fatal("the ladder recorded no census tables for migration 0003")
		}
		if !slices.Contains(tables, pgmigrate.TouchedTable{Name: "things", Through: "parent"}) {
			t.Errorf("the census tables of migration 0003 are %+v, want things, reached through parent", tables)
		}
	})
}
