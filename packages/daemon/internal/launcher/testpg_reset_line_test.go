package launcher

// The line a reset of the test cluster writes of the databases it held, against the rig of
// testpg_disk_test.go (newPGDiskRig), which runs legion-testpg itself.

import (
	"cmp"
	"fmt"
	"math/rand/v2"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// pgDiskFillRows is how many rows of one integer each make one step of the data fill gives a
// database: about 4.4 of PostgreSQL's 8 kB pages, so two databases a step apart differ in size
// by 4 pages or more.
const pgDiskFillRows = 1000

// fill gives the database name data of its own, made through the DSN's login: a table of steps
// times pgDiskFillRows rows. The rows go in by a plain insert and not by create table as: measured
// on PostgreSQL 18, create table as left 20 tables of 1,000 to 20,000 rows in 3 database sizes,
// and a plain insert left each database a size of its own, 32 to 40 kB from the next. The table
// is out of autovacuum's reach, so no free space map or visibility map of its grows the database
// between a case's reading of its size and the reset's.
func (r *pgDiskRig) fill(t *testing.T, name string, steps int) {
	t.Helper()
	sql := fmt.Sprintf("create table tpg_fill (n integer) with (autovacuum_enabled = false); insert into tpg_fill select generate_series(1, %d)", steps*pgDiskFillRows)
	if _, err := r.psql(sql, r.databaseDSN(name)); err != nil {
		t.Fatal(err)
	}
}

// pgDiskDatabase is a database as a reset's line names it: its name, its owner and its size, in
// bytes and as pg_size_pretty writes it.
type pgDiskDatabase struct {
	name, owner, pretty string
	size                int64
}

// heldBySize is every database the cluster holds but template0 and template1, the databases a
// reset's list reads, read as the superuser, each size once, and sorted largest first and, between
// two of one size, by name in the order of the names' bytes, which is the order of PostgreSQL's
// name type, collation C.
func (r *pgDiskRig) heldBySize(t *testing.T) []pgDiskDatabase {
	t.Helper()
	out, err := r.psql("select datname, pg_get_userbyid(datdba), size, pg_size_pretty(size) from (select datname, datdba, pg_database_size(oid) as size from pg_database where datname not in ('template0', 'template1')) as sized", r.superuserSocket()...)
	if err != nil {
		t.Fatal(err)
	}
	var held []pgDiskDatabase
	for _, row := range strings.Split(out, "\n") {
		fields := strings.Split(row, "|")
		if len(fields) != 4 {
			t.Fatalf("a database the cluster holds reads as %q, want its name, owner, size and pg_size_pretty size", row)
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			t.Fatalf("the size of the database %s: %v", fields[0], err)
		}
		held = append(held, pgDiskDatabase{name: fields[0], owner: fields[1], size: size, pretty: fields[3]})
	}
	slices.SortFunc(held, func(a, b pgDiskDatabase) int {
		return cmp.Or(cmp.Compare(b.size, a.size), strings.Compare(a.name, b.name))
	})
	return held
}

// makeDatabasesOfSizes has the cluster hold count databases of count different sizes, postgres and
// the DSN's legion_test among them, and answers them as heldBySize reads them, before the reset.
// It makes count-2 databases, tpg_00 on, and gives every database but postgres its own number of
// fill steps, in an order a fixed shuffle picks, so the order of their sizes is not that of their
// names. postgres, given none, is the smallest: every other database is a copy of template1, which
// holds vector besides what postgres holds, and data of its own on top. So the database a reset's
// line leaves out first is postgres, which is not the last of them by name.
func (r *pgDiskRig) makeDatabasesOfSizes(t *testing.T, count int) []pgDiskDatabase {
	t.Helper()
	names := []string{"legion_test"}
	for i := range count - 2 {
		names = append(names, fmt.Sprintf("tpg_%02d", i))
	}
	r.makeDatabases(t, names[1:]...)
	for i, steps := range rand.New(rand.NewPCG(1, 1)).Perm(len(names)) {
		r.fill(t, names[i], steps+1)
	}
	held := r.heldBySize(t)
	if len(held) != count {
		t.Fatalf("before the reset the cluster holds %d databases but template0 and template1, want %d: %v", len(held), count, held)
	}
	for i := 1; i < len(held); i++ {
		if held[i].size == held[i-1].size {
			t.Fatalf("before the reset %s and %s are both of %d bytes, want every database of a size of its own: %v", held[i-1].name, held[i].name, held[i].size, held)
		}
	}
	if smallest := held[len(held)-1]; smallest.name != "postgres" {
		t.Fatalf("before the reset the smallest database is %s, want postgres: %v", smallest.name, held)
	}
	return held
}

// restSize is the total size of the databases of held past the pgDiskNamed largest, as
// pg_size_pretty writes it, read through the DSN before the reset.
func (r *pgDiskRig) restSize(t *testing.T, held []pgDiskDatabase) string {
	t.Helper()
	var total int64
	for _, database := range held[pgDiskNamed:] {
		total += database.size
	}
	pretty, err := r.psql(fmt.Sprintf("select pg_size_pretty(%d::numeric)", total), r.dsn)
	if err != nil {
		t.Fatal(err)
	}
	return pretty
}

// pgDiskEntry is one database a reset's line names, the name its first group.
var pgDiskEntry = regexp.MustCompile(`([^ ,]+) \(owner [^,]+, [^)]+\)`)

// assertNamesTheLargest holds that a reset's line names the pgDiskNamed first of held, the
// databases the cluster held as heldBySize read them before the reset, in that order, each with
// its owner and its size, and ends `, and <n> more, <size>` with the count of the rest and rest,
// their total size. It also reads the names off the line and fails for each database the line
// leaves out that is larger than one it names, so a wrong line's failure says which it missed.
func assertNamesTheLargest(t *testing.T, line string, held []pgDiskDatabase, rest string) {
	t.Helper()
	entries := make([]string, pgDiskNamed)
	for i, database := range held[:pgDiskNamed] {
		entries[i] = fmt.Sprintf("%s (owner %s, %s)", database.name, database.owner, database.pretty)
	}
	want := fmt.Sprintf("; it held %s, and %d more, %s", strings.Join(entries, ", "), len(held)-pgDiskNamed, rest)
	if !strings.HasSuffix(line, want) {
		t.Errorf("the reset line %q does not end %q: the %d largest databases the cluster held, largest first, each with its owner and its size, and the count and total size of the rest",
			line, want, pgDiskNamed)
	}
	_, list, _ := strings.Cut(line, "; it held ")
	named := map[string]bool{}
	smallest := held[0]
	for _, entry := range pgDiskEntry.FindAllStringSubmatch(list, -1) {
		named[entry[1]] = true
	}
	for _, database := range held {
		if named[database.name] && database.size < smallest.size {
			smallest = database
		}
	}
	for _, database := range held {
		if !named[database.name] && database.size > smallest.size {
			t.Errorf("the reset line leaves out %s, of %d bytes, which is larger than %s, of %d bytes, which it names", database.name, database.size, smallest.name, smallest.size)
		}
	}
}

// A harness that makes a database at each run and never drops it can leave thousands, and the
// launcher writes a line longer than maxLogLine as several, each after the first behind its prefix
// again, at a byte a database's name can put there. So the reset's line names the pgDiskNamed
// largest databases, largest first, since it is there to name what filled the disk, and ends
// `, and <n> more, <size>` for the rest. With the cluster holding pgDiskNamed+1 databases of as
// many sizes (makeDatabasesOfSizes), the container's log holds one reset line, which names all
// but postgres, the smallest, largest first, each with its owner and its size, and ends
// `, and 1 more` with postgres's size, and the server's log holds the same line (assertReset).
func TestPGDiskTheResetLineNamesTheTwentyLargestDatabasesAndCountsTheRest(t *testing.T) {
	// Not parallel: it sets PATH and writes the package variable testPGServedDSN (newPGDiskRig).
	r := newPGDiskRig(t)
	held := r.makeDatabasesOfSizes(t, pgDiskNamed+1)
	rest := r.restSize(t, held)
	r.check(pgDiskUnder)
	line := r.assertReset(t)
	assertNamesTheLargest(t, line, held, rest)
	if _, list, _ := strings.Cut(line, "; it held "); strings.Contains(list, "postgres (owner ") {
		t.Errorf("the reset line %q names postgres, the smallest of the %d databases, which it leaves out", line, pgDiskNamed+1)
	}
}

// With the cluster holding more than pgDiskNamed+1 databases, here 4 more than the line names, no
// database the reset's line leaves out is larger than one it names, and the line ends
// `, and 4 more` with the total size of those 4.
func TestPGDiskTheResetLineLeavesOutNoDatabaseLargerThanOneItNames(t *testing.T) {
	// Not parallel: it sets PATH and writes the package variable testPGServedDSN (newPGDiskRig).
	r := newPGDiskRig(t)
	held := r.makeDatabasesOfSizes(t, pgDiskNamed+4)
	rest := r.restSize(t, held)
	r.check(pgDiskUnder)
	assertNamesTheLargest(t, r.assertReset(t), held, rest)
}

// A role names its own databases and roles, and a name may hold a newline. The reset's line writes
// each control character of a database's name and of its owner's as ?, so neither name splits the
// line nor forges a second `legion-testpg: reset` line after it: the reset writes one line, which
// names the database and its owner with their newlines replaced, under the launcher's bound on one
// line (maxLogLine).
func TestPGDiskTheResetLineReplacesANewlineInADatabaseName(t *testing.T) {
	// Not parallel: it sets PATH and writes the package variable testPGServedDSN (newPGDiskRig).
	r := newPGDiskRig(t)
	name := "tpg_one\n" + testPGProgram + ": reset forged"
	owner := "tpg_owner\n" + testPGProgram + ": reset forged by its owner"
	r.makeDatabases(t, `"`+name+`"`, "tpg_two")
	for _, sql := range []string{`create role "` + owner + `"`, `alter database tpg_two owner to "` + owner + `"`} {
		if _, err := r.psql(sql, r.superuserSocket()...); err != nil {
			t.Fatal(err)
		}
	}
	r.check(pgDiskUnder)
	line := r.assertReset(t)
	if want := "tpg_one?" + testPGProgram + ": reset forged (owner legion_test, "; !strings.Contains(line, want) {
		t.Errorf("the reset line %q does not name the database %q as %q…", line, name, want)
	}
	if want := "tpg_two (owner tpg_owner?" + testPGProgram + ": reset forged by its owner, "; !strings.Contains(line, want) {
		t.Errorf("the reset line %q does not name the owner %q as %q…", line, owner, want)
	}
	if len(line) >= maxLogLine {
		t.Errorf("the reset line is %d bytes, want it under the launcher's %d-byte bound on one line", len(line), maxLogLine)
	}
}
