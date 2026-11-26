package store

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// movedTables are the four tables the move of dispatch://INFRA-1098's R32 takes into the graph
// store: the priority store's nodes and tuples and the workflow store's.
var movedTables = []string{"priority_nodes", "priority_tuples", "workflow_nodes", "workflow_tuples"}

// movedTable matches a name of movedTables as a word, as grep -w reads one: a name inside a longer
// identifier, such as a constraint's name that begins with a table's, is none.
var movedTable = regexp.MustCompile(`\b(?:` + strings.Join(movedTables, "|") + `)\b`)

// movedTablesInventoryRoots are the directories of the checkout the inventory reads.
var movedTablesInventoryRoots = []string{"packages", "scripts", ".dispatch"}

// movedTablesNamer is one file the inventory lets name a moved table, by its path from the
// checkout's root, with the reason it may.
type movedTablesNamer struct {
	path, reason string
}

// historyKeeps is the reason of a migration that created or edited a moved table before this
// change: the runner keeps the text of every migration it applied, so the file stays as it was.
const historyKeeps = "a migration that created or edited the table before dispatch://INFRA-1098's R32; the runner keeps an applied migration's text as it was"

// theMove is the reason of the move at boot's own code: it reads the four tables, copies them,
// drops them and creates the views of their names, for one release.
const theMove = "the move at boot (storesmove), which reads, copies and drops the four tables and creates the views of their names"

// beforeTheMove is the reason of a test or a test helper that writes the four tables as the
// release before wrote them, before the move it holds to what it must do.
const beforeTheMove = "a test of the move, or its helper, which writes the old tables in the release before's shapes before the move"

// storesMovedCheck is the reason of the check c-stores-moved's driver, its parser test and their
// fixtures: the driver boots the release before, which writes the four tables, and reads the
// tables, their locks and the census's plans of them while this head moves them.
const storesMovedCheck = "c-stores-moved's driver (stores-moved.sh), which boots the release before over the four tables and reads them, their locks and the census's plans of them while this head moves them"

// storesMovedFixture is the reason of a fixture of that driver's parsers: a log or an output the
// real systems wrote during the move, captured as it was, which names the tables as it wrote them.
const storesMovedFixture = storesMovedCheck + ": a fixture of its parsers (stores-moved.test.sh), captured as the real systems wrote it"

// movedReadCostCheck is the reason of the check c-moved-read-cost's test and its corpus: the corpus
// writes the four tables in the release before's shapes, and the test times that release's reads
// of them beside this head's reads of the graph store.
const movedReadCostCheck = "c-moved-read-cost's test (TestNoMovedReadGetsSlower), which writes the four tables in the release before's shapes and times that release's reads of them beside this head's reads of the graph store"

// movedTablesNamers is every file of the checkout that may name a moved table, each with why. A
// file joins it in the commit that writes what it names a table for, and leaves it in the commit
// that takes that out, so it holds only files that stand and name a table.
var movedTablesNamers = []movedTablesNamer{
	{"packages/envoy/internal/dispatch/store/migrations/20261112024320_priority_nodes.up.sql", historyKeeps},
	{"packages/envoy/internal/dispatch/store/migrations/20261112024320_priority_nodes.census.sql", historyKeeps + " (its census)"},
	{"packages/envoy/internal/dispatch/store/migrations/20261116235202_workflow_nodes.up.sql", historyKeeps},
	{"packages/envoy/internal/dispatch/store/migrations/20261116235202_workflow_nodes.census.sql", historyKeeps + " (its census)"},
	{"packages/envoy/internal/dispatch/store/moved_tables_inventory_test.go", "this test, which names the four tables it looks for (movedTables)"},
	{"packages/envoy/internal/dispatch/storesmove/storesmove.go", theMove},
	{"packages/envoy/internal/dispatch/storesmove/copy.go", theMove + ": its copy"},
	{"packages/envoy/internal/dispatch/storesmove/swap.go", theMove + ": its swap and the views"},
	{"packages/envoy/internal/dispatch/storesmove/census.go", theMove + ": its census before the drop"},
	{"packages/envoy/internal/dispatch/storesmove/rehearse.go", theMove + ": the pre-deploy census's rehearsal of it"},
	{"packages/envoy/cmd/dispatch/census.go", "the pre-deploy census, which rehearses the move while priority_nodes is a table"},
	{"packages/envoy/internal/dispatch/graphstore/stores_moved.go", "the two triggers that complete what the release before writes through the views workflow_nodes and workflow_tuples, kept for one release"},
	{"packages/envoy/internal/dispatch/storesmove/storesmovetest/storesmovetest.go", beforeTheMove},
	{"packages/envoy/internal/dispatch/storesmove/storesmove_test.go", beforeTheMove},
	{"packages/envoy/internal/dispatch/api/stores_moved_revision_test.go", beforeTheMove + " (TestARevisionReadBeforeTheMoveIsRefusedAfterIt)"},
	{"packages/envoy/cmd/dispatch/stores_rehearsal_test.go", beforeTheMove + " (TestThePreDeployCensusRehearsesTheMoveAndChangesNothing)"},
	{"packages/envoy/cmd/dispatch/main_test.go", "a test that brings a database back to migration 52, before the migrations that create the four tables, so it drops them as that version held none (TestCensusSubcommandExitsOneWhenAPendingMigrationsCensusCountsRows)"},
	{"packages/envoy/internal/dispatch/startup/retire_stores_moved.go", "the boot step that drops the views of the four names, with the two triggers, once no build a rollback reaches writes through them (startup.StoresMovedRetirement)"},
	{"packages/envoy/internal/dispatch/startup/retire_stores_moved_test.go", "the test of that boot step, which reads in the catalog that the four names are views, tables or gone (TestTheBootStepDropsTheMovedStoresObjectsOnlyAfterStoresMoved)"},
	{"packages/envoy/internal/dispatch/store/migrations/20261126000500_graph_tuples_workflow_inbound_index.up.sql", "the concurrent build of the index the release before's read of an issue's runs and plans takes through the view workflow_tuples, kept for one release and dropped with the views by startup.StoresMovedRetirement"},
	{"packages/envoy/internal/dispatch/store/migrations/20261126000500_graph_tuples_workflow_inbound_index.down.sql", "the by-hand rollback of that index, which names the view the index serves"},
	{"packages/envoy/internal/dispatch/store/migrations/20261126000700_graph_tuples_kinds_statistics.up.sql", "the extended statistics over graph_tuples (from_kind, kind, to_kind), kept after the views go: its header comment names the view priority_tuples, whose OR the release before's advances reads plan through"},
	{"packages/envoy/internal/dispatch/api/stores_moved_drop_test.go", "a test that reads that the boot step left none of the four names before every writer of the three kinds writes (TestTheMovedStoresWriteAfterTheDropStep)"},
	{"packages/envoy/internal/dispatch/graphcensus/one_writer_test.go", "c-one-writer, which names the four views, with the two triggers, as the one write of the store outside graphstore while they stand (TestEveryGraphKindHasOneWriter)"},
	{"packages/dispatch/e2e/acceptance/root-deletes.txt", "the list of what this change keeps for one release, the four views among them"},
	{"packages/dispatch/e2e/acceptance/stores-moved.sh", storesMovedCheck},
	{"packages/dispatch/e2e/acceptance/stores-moved.test.sh", storesMovedCheck + ": the test of its parsers"},
	{"packages/dispatch/e2e/acceptance/testdata/stores-moved/base-handler-failure.txt", storesMovedFixture + " (the release before's log of the seventh control, which reads priority_tuples after the drop)"},
	{"packages/dispatch/e2e/acceptance/testdata/stores-moved/census-explain.out", storesMovedFixture + " (the rehearsal's plans of the move's census statements, with DISPATCH_TEST_STORES_MOVE_EXPLAIN=1)"},
	{"packages/dispatch/e2e/acceptance/testdata/stores-moved/census-pending-search-text.out", storesMovedFixture + " (the census's rehearsal, which skips a pending migration of another tree that touches none of the six tables)"},
	{"packages/dispatch/e2e/acceptance/testdata/stores-moved/head-first.txt", storesMovedFixture + " (the first runner's boot log, whose swap waits on priority_nodes)"},
	{"packages/dispatch/e2e/acceptance/testdata/stores-moved/head-last.txt", storesMovedFixture + " (a boot log whose one swap attempt gives up on priority_nodes)"},
	{"packages/dispatch/e2e/acceptance/testdata/stores-moved/head-roll.txt", storesMovedFixture + " (this head's boot log over the roll)"},
	{"packages/dispatch/e2e/acceptance/testdata/stores-moved/head-second.txt", storesMovedFixture + " (the second runner's boot log, which finds priority_nodes a view and moves nothing)"},
	{"packages/dispatch/e2e/acceptance/testdata/stores-moved/locks-wait-on-priority-nodes.csv", storesMovedFixture + " (the swap's locks while it waits on priority_nodes)"},
	{"packages/envoy/internal/dispatch/api/moved_read_cost_test.go", movedReadCostCheck},
	{"packages/envoy/internal/dispatch/api/testdata/moved-read-cost-corpus.sql", movedReadCostCheck + ": its corpus"},
	{"packages/envoy/internal/dispatch/store/stores_moved_production_test.go", "c-moved-recorded's test (TestNoMigrationProductionLacksTouchesTheStores), which names the six tables the rehearsal reads and writes (storesMoveTables) and fails on each migration production lacks that touches one"},
}

// TestNoFileAtTheHeadNamesTheMovedTablesOutsideItsList is the inventory of the move of the priority
// store and the workflow store into the graph store (dispatch://INFRA-1098's R32). It lists every
// file of the checkout under packages/, scripts/ and .dispatch/ that git does not ignore, tracked
// or not, and that names one of the four tables as a word (movedTable), and compares that list
// with movedTablesNamers in both directions: it fails naming each file it finds that the list
// does not hold, with the tables it names and the first line that names one, and each listed file
// that the checkout does not hold or that names no table any more.
//
// Every reader and writer of the four tables reads and writes the graph store once the move
// stands, so each file it names outside the list is one still to move: a reader or a writer of
// prioritystore, workflowstore and their callers, a test that reads the tables, a list that
// excepts them from the census, or a document that describes them.
func TestNoFileAtTheHeadNamesTheMovedTablesOutsideItsList(t *testing.T) {
	root := checkoutRoot(t)
	files := checkoutFilesUnder(t, root, movedTablesInventoryRoots)
	if len(files) == 0 {
		t.Fatalf("git ls-files lists no file under %v of %s, so the inventory would read nothing", movedTablesInventoryRoots, root)
	}
	found := map[string]movedTablesHit{}
	for _, name := range files {
		content, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if bytes.IndexByte(content[:min(len(content), 8192)], 0) >= 0 {
			continue
		}
		if hit, names := movedTablesIn(content); names {
			found[name] = hit
		}
	}
	t.Logf("read %d files under %v; %d name a moved table, and the list holds %d", len(files), movedTablesInventoryRoots, len(found), len(movedTablesNamers))

	listed := map[string]bool{}
	for _, namer := range movedTablesNamers {
		listed[namer.path] = true
		hit, names := found[namer.path]
		switch {
		case !slices.Contains(files, namer.path):
			t.Errorf("the list holds %s (%s), which the checkout does not hold: take it out of movedTablesNamers", namer.path, namer.reason)
		case !names:
			t.Errorf("the list holds %s (%s), which names no moved table any more: take it out of movedTablesNamers", namer.path, namer.reason)
		default:
			t.Logf("listed: %s names %s: %s", namer.path, hit, namer.reason)
		}
	}
	var unlisted []string
	for name := range found {
		if !listed[name] {
			unlisted = append(unlisted, name)
		}
	}
	slices.Sort(unlisted)
	for _, name := range unlisted {
		t.Errorf("%s names %s, and movedTablesNamers does not hold it: move what it reads or writes to the graph store, or list it with the reason it may name the table", name, found[name])
	}
	if len(unlisted) > 0 {
		t.Logf("%d files name a moved table outside the list:\n%s", len(unlisted), strings.Join(unlisted, "\n"))
	}
}

// movedTablesHit is what one file names of the moved tables: each table, once, in the order it
// first names them, and the first line that names one.
type movedTablesHit struct {
	tables    []string
	firstLine int
}

func (h movedTablesHit) String() string {
	return fmt.Sprintf("%s (first at line %d)", strings.Join(h.tables, ", "), h.firstLine)
}

// movedTablesIn is what content names of the moved tables, and whether it names one.
func movedTablesIn(content []byte) (movedTablesHit, bool) {
	var hit movedTablesHit
	for index, line := range strings.Split(string(content), "\n") {
		for _, table := range movedTable.FindAllString(line, -1) {
			if hit.firstLine == 0 {
				hit.firstLine = index + 1
			}
			if !slices.Contains(hit.tables, table) {
				hit.tables = append(hit.tables, table)
			}
		}
	}
	return hit, hit.firstLine != 0
}

// checkoutRoot is the checkout this package stands in, five directories above it.
func checkoutRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "packages", "envoy", "go.mod")); err != nil {
		t.Fatalf("%s holds no packages/envoy/go.mod, so it is no checkout of this repository: %v", root, err)
	}
	return root
}

// checkoutFilesUnder is every regular file of the checkout under dirs that git does not ignore,
// tracked or not, by its path from root, sorted: what a reviewer of the tree reads. A tracked file
// the working copy deleted is left out, since no copy of it stands.
func checkoutFilesUnder(t *testing.T, root string, dirs []string) []string {
	t.Helper()
	args := append([]string{"-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--"}, dirs...)
	listed, err := exec.Command("git", args...).Output()
	if err != nil {
		t.Fatalf("list the checkout's files under %v with git ls-files: %v", dirs, err)
	}
	var files []string
	for _, name := range strings.Split(string(listed), "\x00") {
		if name == "" {
			continue
		}
		if info, err := os.Lstat(filepath.Join(root, name)); err != nil || !info.Mode().IsRegular() {
			continue
		}
		files = append(files, name)
	}
	slices.Sort(files)
	return slices.Compact(files)
}
