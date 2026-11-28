package storetest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testcleanup"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The rows of this file start copies of this test binary (os.Args[0], -test.run=^…$, docs/testing.md
// "A Go test that starts a copy of its own test binary") with STORETEST_OPEN_SET naming a scheme
// and a fake migration set of the row's own (rowOpenSet), so each copy's TestMain finds or makes,
// and each copy's Open clones, a template of the row's and never a real one. A row reads what a
// copy wrote (resolveOpen's line, clone's, Run's listing) and the server's catalogs; the order of
// its events comes from those, never from how long anything took.

// rowChildTimeout is each copy's -timeout, a hang guard only, which no verdict reads. The copy's
// TestMain, which makes or finds the row's template and in
// TestTwoProcessesThatStartTogetherMakeOneTemplate waits on its gate or on the build lock, gives up
// 90 s into the copy's run (testwait.MainGiveUpAt, testwait's margin before it), and its tests'
// waits at the same distance from its deadline. In the pod each row of this file took at most
// 2.8 s, its copies included, under -race too.
const rowChildTimeout = 2 * time.Minute

// openTemplateLine is a copy's line for Open's template (resolveOpen): its name, the server's host
// and port, and whether the copy made or found it.
var openTemplateLine = regexp.MustCompile(`storetest: Open's template (\S+) on (\S+) (made|found) in \S+`)

// cloneLine is a copy's line for each clone (clone, under -test.v): the clone and its template.
var cloneLine = regexp.MustCompile(`storetest: (dispatch_test_[0-9a-f]+) cloned from (\S+) in \S+`)

// sharedTemplatesLine is a copy's listing after its tests (listSharedTemplates): the server's host
// and port, and the databases under the copy's scheme's prefix.
var sharedTemplatesLine = regexp.MustCompile(`storetest: shared templates on (\S+) after this binary's tests: (.*)`)

// rowChild is a copy of this test binary a row started (startRowChild).
type rowChild struct {
	name string
	cmd  *exec.Cmd
	// output is what the copy writes to stdout and to stderr, in the order it wrote it: one writer
	// for both, so exec hands the copy one pipe for them, and TestMain's line on stderr and
	// testing's === RUN on stdout keep the copy's order. A row reads it while the copy runs, so it
	// is locked (LockedLog).
	output LockedLog
	// exited is closed once the copy has exited, with err its exit's error.
	exited chan struct{}
	err    error
}

// startRowChild starts a copy of this test binary, which the row calls name, with args and
// -test.timeout rowChildTimeout, and with set as STORETEST_OPEN_SET and env added to the
// environment it inherits. It refuses to start one from a copy, which a row never runs in. The copy
// runs on t's waits' context (testwait.Context), so one still running where they give up is
// killed. When t ends a copy still running is killed and waited for, then each clone and each
// template of the copy's own that its clone lines name is dropped on admin, where the copy left it.
func startRowChild(t *testing.T, admin *pgx.Conn, name string, set rowOpenSet, args []string, env ...string) *rowChild {
	t.Helper()
	if os.Getenv(openSetEnv) != "" {
		t.Fatalf("this process is a copy a row started (%s is set), and a copy starts none of its own", openSetEnv)
	}
	encoded, err := json.Marshal(set)
	if err != nil {
		t.Fatalf("encode the row's set: %v", err)
	}
	child := &rowChild{name: name, exited: make(chan struct{})}
	child.cmd = exec.CommandContext(testwait.Context(t), os.Args[0], slices.Concat(args, []string{"-test.timeout=" + rowChildTimeout.String()})...)
	child.cmd.Env = slices.Concat(os.Environ(), []string{openSetEnv + "=" + string(encoded)}, env)
	child.cmd.Stdout, child.cmd.Stderr = &child.output, &child.output
	if err := child.cmd.Start(); err != nil {
		t.Fatalf("start the copy %s: %v", name, err)
	}
	go func() {
		child.err = child.cmd.Wait()
		close(child.exited)
	}()
	testcleanup.Add(t, func() {
		var left []string
		for _, clone := range cloneLines(child.output.String()) {
			left = append(left, clone.database)
			if strings.HasPrefix(clone.template, "dispatch_template_") {
				left = append(left, clone.template)
			}
		}
		dropDatabases(t, admin, left...)
	})
	testcleanup.Add(t, func() {
		select {
		case <-child.exited:
		default:
			_ = child.cmd.Process.Kill()
			testwait.Receive(t, "the copy "+name+", killed as the row ended, to exit", child.exited)
		}
	})
	return child
}

// wait waits for the copy to exit and answers its exit's error.
func (c *rowChild) wait(t *testing.T) error {
	t.Helper()
	testwait.Receive(t, "the copy "+c.name+" to exit", c.exited)
	return c.err
}

// mustRun fails t where the copy has exited, with what it wrote: for a wait on a fact the copy's
// run makes, before, which a copy that exited never will.
func (c *rowChild) mustRun(t *testing.T, before string) {
	t.Helper()
	select {
	case <-c.exited:
		t.Fatalf("the copy %s exited (%v) before %s:\n%s", c.name, c.err, before, c.output.String())
	default:
	}
}

// report is the copy's exit and what it wrote, for a failure.
func (c *rowChild) report() string {
	select {
	case <-c.exited:
		return fmt.Sprintf("the copy %s (exit: %v):\n%s", c.name, c.err, c.output.String())
	default:
		return fmt.Sprintf("the copy %s (running):\n%s", c.name, c.output.String())
	}
}

// templateBackend answers the pid of the copy's template connection, read on conn by its
// application_name (templateApplicationName of the copy's pid), and false while the copy has none.
func (c *rowChild) templateBackend(t *testing.T, conn *pgx.Conn) (int, bool) {
	t.Helper()
	pids, err := templateBackendsOf(conn, c.cmd.Process.Pid)
	if err != nil {
		t.Fatalf("read the template connection of the copy %s: %v", c.name, err)
	}
	if len(pids) != 1 {
		return 0, false
	}
	return pids[0], true
}

// templateBackendsOf lists the backends whose application_name is the template connection's of the
// process pid (templateApplicationName).
func templateBackendsOf(conn *pgx.Conn, pid int) ([]int, error) {
	rows, err := conn.Query(context.Background(), "select pid from pg_stat_activity where application_name = $1", templateApplicationName(pid))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int])
}

// openLine is a copy's line for Open's template.
type openLine struct{ template, host, how string }

// openLines reads each line output holds for Open's template, in order.
func openLines(output string) []openLine {
	var lines []openLine
	for _, match := range openTemplateLine.FindAllStringSubmatch(output, -1) {
		lines = append(lines, openLine{match[1], match[2], match[3]})
	}
	return lines
}

// clonedFrom is a copy's line for one clone.
type clonedFrom struct{ database, template string }

// cloneLines reads each clone line output holds, in order.
func cloneLines(output string) []clonedFrom {
	var lines []clonedFrom
	for _, match := range cloneLine.FindAllStringSubmatch(output, -1) {
		lines = append(lines, clonedFrom{match[1], match[2]})
	}
	return lines
}

// rowSet is the STORETEST_OPEN_SET of a row's copies: s, the set of digest, and the gate file the
// copies' fake migrate waits for, or "".
func rowSet(s scheme, digest, gate string) rowOpenSet {
	return rowOpenSet{Prefix: s.prefix, BuildClass: s.buildClass, HoldClass: s.holdClass, Digest: digest, Gate: gate}
}

// openOnceChildEnv selects TestACopyOpensOneStoreChild in a copy of this test binary.
const openOnceChildEnv = "STORETEST_OPEN_ONCE_CHILD"

// TestACopyOpensOneStoreChild is the copies' test of
// TestTwoProcessesThatStartTogetherMakeOneTemplate and TestMainMakesOpensTemplateBeforeTheFirstTest:
// it opens one store, whose clone line names the template the copy's TestMain resolved. It runs
// only in a copy one of them starts.
func TestACopyOpensOneStoreChild(t *testing.T) {
	if os.Getenv(openOnceChildEnv) == "" {
		t.Skip(openOnceChildEnv + " is not set: this test is the copies' of TestTwoProcessesThatStartTogetherMakeOneTemplate and TestMainMakesOpensTemplateBeforeTheFirstTest")
	}
	Open(t)
}

// TestTwoProcessesThatStartTogetherMakeOneTemplate: two test processes that start together against
// one server make its template once, and both clone it (R1; D3, the look again under the build
// lock). Copy A starts first, and its fake migrate waits on a gate file while A holds the build
// lock; the row waits until pg_locks grants A's template connection that lock, then starts copy B,
// and waits until B's template connection queues on it; then it writes the gate. Afterwards the
// row's scheme must hold exactly one database, every name under its prefix counted (templates and
// builds), the template; A must have printed made and B found for it; and each copy's test must
// have cloned it. Without the look under the lock, B builds again once it has the lock, its rename
// fails on the name A made, B prints "could not be made", and the row fails naming both databases
// of the set, the template and B's build: each failure names every database under the prefix.
//
// It does not take t.Parallel: it acts between its copies' events rather than only waiting on
// them.
func TestTwoProcessesThatStartTogetherMakeOneTemplate(t *testing.T) {
	s := rowScheme(t)
	admin := rowAdmin(t, s)
	digest := rowDigest(t)
	template := s.templateName(digest, currentLogin(t, admin))
	gate := filepath.Join(t.TempDir(), "gate")
	set := rowSet(s, digest, gate)
	run := []string{"-test.run=^TestACopyOpensOneStoreChild$", "-test.v"}
	a := startRowChild(t, admin, "A", set, run, openOnceChildEnv+"=1")
	testwait.Until(t, "copy A's template connection to hold the build lock "+buildLock(s)+", its migrate waiting on the gate", func() (string, bool) {
		a.mustRun(t, "its template connection held the build lock")
		pid, ok := a.templateBackend(t, admin)
		if !ok {
			return "copy A has no template connection", false
		}
		state := buildLockOf(t, admin, s, pid)
		return state.String(), state.granted
	})
	b := startRowChild(t, admin, "B", set, run, openOnceChildEnv+"=1")
	testwait.Until(t, "copy B's template connection to queue on the build lock "+buildLock(s)+" copy A holds", func() (string, bool) {
		a.mustRun(t, "copy B queued on the build lock")
		b.mustRun(t, "its template connection queued on the build lock")
		pid, ok := b.templateBackend(t, admin)
		if !ok {
			return "copy B has no template connection", false
		}
		state := buildLockOf(t, admin, s, pid)
		return state.String(), state.queued
	})
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatalf("write the gate file: %v", err)
	}
	errA, errB := a.wait(t), b.wait(t)
	held := rowDatabases(t, admin, s.prefix)
	report := fmt.Sprintf("the databases under the row's prefix: %s\n%s\n%s", namesOrNone(held), a.report(), b.report())
	if !slices.Equal(held, []string{template}) {
		t.Fatalf("the row's scheme holds %d database(s); want one, the template %s, which A made and B found\n%s", len(held), template, report)
	}
	if errA != nil || errB != nil {
		t.Fatalf("a copy failed (A: %v, B: %v)\n%s", errA, errB, report)
	}
	host := shared.server.Host
	for _, each := range []struct {
		copy *rowChild
		how  string
	}{{a, "made"}, {b, "found"}} {
		output := each.copy.output.String()
		if lines := openLines(output); !slices.Equal(lines, []openLine{{template, host, each.how}}) {
			t.Fatalf("copy %s's lines for Open's template are %+v; want one, %s on %s %s\n%s", each.copy.name, lines, template, host, each.how, report)
		}
		if clones := cloneLines(output); len(clones) != 1 || clones[0].template != template {
			t.Fatalf("copy %s's clone lines are %+v; want one, cloned from %s\n%s", each.copy.name, clones, template, report)
		}
	}
	t.Logf("copy A made %s while copy B waited on the build lock, and B found it; both cloned it", template)
}

// runLeavesChildEnv selects TestRunLeavesOpensTemplateAndDropsTheBinarysOwnChild in a copy of this
// test binary.
const runLeavesChildEnv = "STORETEST_RUN_LEAVES_CHILD"

// TestRunLeavesOpensTemplateAndDropsTheBinarysOwnChild is the copy's test of
// TestRunLeavesOpensTemplateAndDropsTheBinarysOwn: it opens a store of each kind, in order: Open's,
// OpenFrom's of a key of its own, and OpenFilled's of a Filled of its own (openRowFilled). It runs
// only in a copy that row starts.
func TestRunLeavesOpensTemplateAndDropsTheBinarysOwnChild(t *testing.T) {
	if os.Getenv(runLeavesChildEnv) == "" {
		t.Skip(runLeavesChildEnv + " is not set: this test is the copy's of TestRunLeavesOpensTemplateAndDropsTheBinarysOwn")
	}
	Open(t)
	OpenFrom(t, "run leaves open's template: OpenFrom", rowMigrate)
	openRowFilled(t, "run leaves open's template: Filled")
}

// openRowFilled opens a store with OpenFilled, of a Filled of key whose Fill writes one row into
// the table the set's fake migrate made, storetest_row_set, and fails t unless the clone holds
// that row: the Filled's template is a copy of the set's.
func openRowFilled(t *testing.T, key string) {
	t.Helper()
	filled := OpenFilled(t, Filled{Key: key, Fill: func(ctx context.Context, database *store.Store) error {
		_, err := database.Pool.Exec(ctx, "insert into storetest_row_set values (true)")
		return err
	}})
	var rows int
	if err := filled.Pool.QueryRow(context.Background(), "select count(*) from storetest_row_set").Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("OpenFilled's clone holds %d rows of its Fill (%v); want the 1 it wrote into the copy of Open's template", rows, err)
	}
}

// TestRunLeavesOpensTemplateAndDropsTheBinarysOwn: Run leaves Open's template, the server's, for
// the next test process, and drops what the binary made for itself (R1: each test's clone, and a
// template its own test code makes or fills). A copy opens a store of each kind (Open, OpenFrom,
// OpenFilled) and exits. Afterwards the set's template must be on the server; the three clones, the
// OpenFrom template and the Filled template, which the copy's clone lines name, must not be; the
// Filled template must be a database other than the shared one; and the copy's last listing must
// name the shared template alone.
func TestRunLeavesOpensTemplateAndDropsTheBinarysOwn(t *testing.T) {
	t.Parallel()
	s := rowScheme(t)
	admin := rowAdmin(t, s)
	digest := rowDigest(t)
	template := s.templateName(digest, currentLogin(t, admin))
	child := startRowChild(t, admin, "of the row", rowSet(s, digest, ""), []string{"-test.run=^TestRunLeavesOpensTemplateAndDropsTheBinarysOwnChild$", "-test.v"}, runLeavesChildEnv+"=1")
	if err := child.wait(t); err != nil {
		t.Fatalf("the copy failed: %v\n%s", err, child.report())
	}
	output := child.output.String()
	if !strings.Contains(output, "--- PASS: TestRunLeavesOpensTemplateAndDropsTheBinarysOwnChild (") {
		t.Fatalf("the copy's test did not pass, so it opened no store of each kind\n%s", child.report())
	}
	clones := cloneLines(output)
	if len(clones) != 3 {
		t.Fatalf("the copy wrote %d clone lines, %+v; want 3, one for each kind of store\n%s", len(clones), clones, child.report())
	}
	openFrom, filled := clones[1].template, clones[2].template
	if clones[0].template != template {
		t.Fatalf("the copy's Open cloned %s; want the set's template %s\n%s", clones[0].template, template, child.report())
	}
	if !strings.HasPrefix(openFrom, "dispatch_template_") || !strings.HasPrefix(filled, "dispatch_template_") || openFrom == filled || filled == template {
		t.Fatalf("the copy's OpenFrom cloned %s and its OpenFilled %s; want two templates of the binary's own, dispatch_template_…, other than each other and than %s\n%s", openFrom, filled, template, child.report())
	}
	if !databaseExists(t, admin, template) {
		t.Fatalf("the set's template %s is gone after the copy exited: Run dropped Open's template\n%s", template, child.report())
	}
	for _, name := range []string{clones[0].database, clones[1].database, clones[2].database, openFrom, filled} {
		if databaseExists(t, admin, name) {
			t.Fatalf("%s, which the copy made for itself, is still on the server after it exited\n%s", name, child.report())
		}
	}
	listings := sharedTemplatesLine.FindAllStringSubmatch(output, -1)
	if len(listings) == 0 {
		t.Fatalf("the copy listed no shared templates after its tests\n%s", child.report())
	}
	if last := listings[len(listings)-1]; last[1] != shared.server.Host || last[2] != template {
		t.Fatalf("the copy's last listing names %s on %s; want %s on %s\n%s", last[2], last[1], template, shared.server.Host, child.report())
	}
	t.Logf("the copy left %s and dropped its clones, its OpenFrom template %s and its Filled template %s", template, openFrom, filled)
}

// TestMainMakesOpensTemplateBeforeTheFirstTest: the template is ready before a test of the job
// opens a database (D1): Main makes it in TestMain, before the first test, and makes nothing under
// go test -list. A copy of a fresh set under -test.list must list its test and make no template of
// the set; a copy of the same set under -test.v must then write its line that it made the set's
// template before its first === RUN, in the one stream it writes, and clone that template in its
// test.
func TestMainMakesOpensTemplateBeforeTheFirstTest(t *testing.T) {
	t.Parallel()
	s := rowScheme(t)
	admin := rowAdmin(t, s)
	digest := rowDigest(t)
	template := s.templateName(digest, currentLogin(t, admin))
	set := rowSet(s, digest, "")
	listed := startRowChild(t, admin, "under -test.list", set, []string{"-test.list=^TestACopyOpensOneStoreChild$"})
	if err := listed.wait(t); err != nil {
		t.Fatalf("the copy under -test.list failed: %v\n%s", err, listed.report())
	}
	if output := listed.output.String(); !strings.Contains(output, "TestACopyOpensOneStoreChild") || strings.Contains(output, "storetest: Open's template") {
		t.Fatalf("the copy under -test.list listed no test, or wrote a line for Open's template\n%s", listed.report())
	}
	if made := rowDatabases(t, admin, s.prefix); len(made) != 0 {
		t.Fatalf("the copy under -test.list left %s on the server; want no template of its set\n%s", namesOrNone(made), listed.report())
	}
	child := startRowChild(t, admin, "under -test.v", set, []string{"-test.run=^TestACopyOpensOneStoreChild$", "-test.v"}, openOnceChildEnv+"=1")
	if err := child.wait(t); err != nil {
		t.Fatalf("the copy failed: %v\n%s", err, child.report())
	}
	output := child.output.String()
	madeAt := openTemplateLine.FindStringSubmatchIndex(output)
	runAt := strings.Index(output, "=== RUN")
	if madeAt == nil || runAt < 0 {
		t.Fatalf("the copy wrote no line for Open's template, or ran no test\n%s", child.report())
	}
	if lines := openLines(output); !slices.Equal(lines, []openLine{{template, shared.server.Host, "made"}}) {
		t.Fatalf("the copy's lines for Open's template are %+v; want one, %s on %s made\n%s", lines, template, shared.server.Host, child.report())
	}
	if madeAt[0] > runAt {
		t.Fatalf("the copy wrote that it made %s after its first === RUN: its first test paid for the template\n%s", template, child.report())
	}
	if clones := cloneLines(output); len(clones) != 1 || clones[0].template != template {
		t.Fatalf("the copy's clone lines are %+v; want one, cloned from %s\n%s", clones, template, child.report())
	}
	t.Logf("the copy made %s before its first test, which cloned it; under -test.list it made nothing", template)
}

// holdLostChildEnv selects TestACloneAfterItsHoldIsLostRemakesASweptTemplateChild in a copy of this
// test binary, and names the gate file its second test waits for.
const holdLostChildEnv = "STORETEST_HOLD_LOST_CHILD"

// holdLostStepEnv names what the second test of
// TestACloneAfterItsHoldIsLostRemakesASweptTemplateChild opens: Open or OpenFilled.
const holdLostStepEnv = "STORETEST_HOLD_LOST_STEP"

// TestACloneAfterItsHoldIsLostRemakesASweptTemplateChild is the copy's test of
// TestACloneAfterItsHoldIsLostRemakesASweptTemplate: its first test opens a store, and its second
// waits until the gate file the variable names exists, then opens another, of the kind
// STORETEST_HOLD_LOST_STEP names: Open's, or OpenFilled's of a Filled of its own (openRowFilled).
// It runs only in a copy that row starts.
func TestACloneAfterItsHoldIsLostRemakesASweptTemplateChild(t *testing.T) {
	gate := os.Getenv(holdLostChildEnv)
	if gate == "" {
		t.Skip(holdLostChildEnv + " is not set: this test is the copy's of TestACloneAfterItsHoldIsLostRemakesASweptTemplate")
	}
	step := os.Getenv(holdLostStepEnv)
	t.Run("first", func(t *testing.T) { Open(t) })
	t.Run("second", func(t *testing.T) {
		testwait.Until(t, "the gate file "+gate+" to exist", func() (string, bool) {
			_, err := os.Stat(gate)
			return fmt.Sprint(err), err == nil
		})
		switch step {
		case "Open":
			Open(t)
		case "OpenFilled":
			openRowFilled(t, "hold lost: Filled")
		default:
			t.Fatalf("%s is %q; want Open or OpenFilled", holdLostStepEnv, step)
		}
	})
}

// TestACloneAfterItsHoldIsLostRemakesASweptTemplate: a template is never dropped while a test
// process may still clone it (R1), and a process whose hold was lost with its template connection
// resolves the template again before it next copies it (D4): Open's clone, and a Filled's
// template, a copy of Open's, each in a subtest of its own, with a set and a copy of its own. Copy
// A makes the set's template in its TestMain and clones it in its first test, then waits on a gate
// file in its second. Once A's clone line is written, the row ends A's template connection
// (pg_terminate_backend of the backend named "storetest template <A's pid>"), as a lost connection
// ends, waits until its backend has let its locks go, and builds the other set of the same scheme
// on a template connection of its own, which must sweep the set's template, which no process holds
// now; the set's template is then gone. The row writes the gate: A's second test, which opens a
// store of the subtest's kind, must pass, A must have written a second made line for the set's
// template, and its second clone line, after it, must name the set's template (Open) or a template
// of A's own other than it (OpenFilled). Without the re-resolve the second copy of the set's
// template fails on 3D000.
//
// It does not take t.Parallel, and nor do its subtests: they act between their copies' events
// rather than only waiting on them.
func TestACloneAfterItsHoldIsLostRemakesASweptTemplate(t *testing.T) {
	for _, step := range []string{"Open", "OpenFilled"} {
		t.Run(step, func(t *testing.T) { cloneAfterTheHoldIsLost(t, step) })
	}
}

// cloneAfterTheHoldIsLost is a subtest of TestACloneAfterItsHoldIsLostRemakesASweptTemplate, whose
// copy's second test opens a store of the kind step names (holdLostStepEnv).
func cloneAfterTheHoldIsLost(t *testing.T, step string) {
	s := rowScheme(t)
	admin := rowAdmin(t, s)
	digest := rowDigest(t)
	template := s.templateName(digest, currentLogin(t, admin))
	gate := filepath.Join(t.TempDir(), "gate")
	a := startRowChild(t, admin, "A", rowSet(s, digest, ""), []string{"-test.run=^TestACloneAfterItsHoldIsLostRemakesASweptTemplateChild$", "-test.v"}, holdLostChildEnv+"="+gate, holdLostStepEnv+"="+step)
	testwait.Until(t, "copy A's first test to clone "+template, func() (string, bool) {
		a.mustRun(t, "its first test cloned "+template)
		clones := cloneLines(a.output.String())
		return fmt.Sprintf("clone lines %+v", clones), len(clones) == 1 && clones[0].template == template
	})
	pids, err := templateBackendsOf(admin, a.cmd.Process.Pid)
	if err != nil {
		t.Fatalf("read copy A's template connection: %v", err)
	}
	if len(pids) != 1 {
		t.Fatalf("copy A has %d template connections, %v; want the one its TestMain opened\n%s", len(pids), pids, a.report())
	}
	var ended bool
	if err := admin.QueryRow(context.Background(), "select pg_terminate_backend($1)", pids[0]).Scan(&ended); err != nil || !ended {
		t.Fatalf("end copy A's template connection, backend %d: %t, %v", pids[0], ended, err)
	}
	awaitBackendEnd(t, admin, pids[0], "copy A's template connection")
	other, made, swept := askRowTemplate(t, rowTemplateConn(t, shared.server), s, rowDigest(t))
	if !made || !slices.Equal(swept, []string{template}) {
		t.Fatalf("the other set's template %s, made %t, swept %s; want it made, sweeping %s, which no process holds\n%s", other, made, namesOrNone(swept), template, a.report())
	}
	if databaseExists(t, admin, template) {
		t.Fatalf("the template %s is still on the server after the sweep that reports it swept", template)
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatalf("write the gate file: %v", err)
	}
	if err := a.wait(t); err != nil {
		t.Fatalf("copy A failed: its %s after the sweep did not make the template again\n%s", step, a.report())
	}
	output := a.output.String()
	if !strings.Contains(output, "--- PASS: TestACloneAfterItsHoldIsLostRemakesASweptTemplateChild (") {
		t.Fatalf("copy A's test did not pass\n%s", a.report())
	}
	openAt := openTemplateLine.FindAllStringSubmatchIndex(output, -1)
	cloneAt := cloneLine.FindAllStringSubmatchIndex(output, -1)
	if lines := openLines(output); !slices.Equal(lines, []openLine{{template, shared.server.Host, "made"}, {template, shared.server.Host, "made"}}) {
		t.Fatalf("copy A's lines for Open's template are %+v; want two, %s on %s made, in TestMain and again for its %s\n%s", lines, template, shared.server.Host, step, a.report())
	}
	clones := cloneLines(output)
	if len(clones) != 2 || clones[0].template != template {
		t.Fatalf("copy A's clone lines are %+v; want two, the first cloned from %s\n%s", clones, template, a.report())
	}
	want, cloned := template, clones[1].template == template
	if step == "OpenFilled" {
		want = "a template of A's own, dispatch_template_…, other than " + template
		cloned = strings.HasPrefix(clones[1].template, "dispatch_template_") && clones[1].template != template
	}
	if !cloned {
		t.Fatalf("copy A's %s cloned %s; want %s\n%s", step, clones[1].template, want, a.report())
	}
	if openAt[1][0] > cloneAt[1][0] {
		t.Fatalf("copy A wrote its second clone line before it made %s again\n%s", template, a.report())
	}
	if !databaseExists(t, admin, template) {
		t.Fatalf("the template %s, which copy A made again, is not on the server", template)
	}
	t.Logf("copy A lost its hold on %s, which the other set's build swept, and made it again for its %s, which cloned %s", template, step, clones[1].template)
}
