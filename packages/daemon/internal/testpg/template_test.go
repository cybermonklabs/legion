package testpg

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// cloneChildVariable marks the run of this test binary that does the cloning, so the parent run
// reads one child's whole stderr rather than its own, which holds whatever else the package's
// tests logged.
const cloneChildVariable = "LEGION_TESTPG_CLONE_CHILD"

// Every clone of a test process copies the one template that process migrated: the whole point of
// Clone over testdb.Database plus Migrate. The child run clones twice and its stderr must hold one
// `testpg: migrated template` line, the line prepare logs; a second line is a second migration
// run, which is the regression this guards — a template rebuilt per test, or per clone.
func TestTwoClonesInOneProcessBuildOneTemplate(t *testing.T) {
	if os.Getenv("LEGION_TEST_PG_DSN") == "" {
		t.Skip(testdb.SkipMessage)
	}
	if os.Getenv(cloneChildVariable) != "" {
		first, second := Clone(t, "legion_testpg_clone_test"), Clone(t, "legion_testpg_clone_test")
		if first == second {
			t.Fatalf("both clones are the same database: %s", first)
		}
		return
	}

	child := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.count=1")
	child.Env = append(os.Environ(), cloneChildVariable+"=1")
	var stdout, stderr bytes.Buffer
	child.Stdout, child.Stderr = &stdout, &stderr
	if err := child.Run(); err != nil {
		t.Fatalf("the cloning run of this test binary failed: %v\n%s%s", err, stdout.String(), stderr.String())
	}
	if builds := strings.Count(stderr.String(), "testpg: migrated template"); builds != 1 {
		t.Fatalf("two clones in one process built %d templates, want 1; the run's stderr was:\n%s", builds, stderr.String())
	}
}

// The admin pool holds twice the drops in flight (dropSlots), whatever pool size
// LEGION_TEST_PG_DSN sets: drops take at most half its connections, so a clone always finds one,
// which is what dropSlots' count is for. A DSN that sized it at 2 would put every connection on
// drops, and one that sized it at 64 would open that many admin connections in every package that
// clones, all on one server.
func TestTheAdminPoolHoldsTwiceTheDropsInFlightWhateverTheDSNSets(t *testing.T) {
	t.Setenv("PGPORT", "")
	t.Setenv("PGUSER", "legion_test")
	for _, dsn := range []string{
		"postgres://legion_test@127.0.0.1:5433/legion?sslmode=disable",
		"postgres://legion_test@127.0.0.1:5433/legion?pool_max_conns=2&sslmode=disable",
		"postgres://legion_test@127.0.0.1:5433/legion?pool_max_conns=64&pool_min_conns=8&sslmode=disable",
	} {
		config, err := adminConfig(dsn)
		if err != nil {
			t.Fatalf("adminConfig(%s): %v", dsn, err)
		}
		if config.MaxConns != int32(2*cap(dropSlots)) || config.MinConns != 0 {
			t.Errorf("the admin pool on %s holds %d connections (at least %d), want %d: twice the %d drops in flight, and none held open",
				dsn, config.MaxConns, config.MinConns, 2*cap(dropSlots), cap(dropSlots))
		}
	}
}

// TestTeardownContextStartsAfterTheTestsDeadline catches TestMain reading test.timeout before
// m.Run parses it. The final database teardown runs after m.Run returns, so it must retain the
// timeout plus its grace, not the unbounded five-minute fallback that a pre-parse read selects.
func TestTeardownContextStartsAfterTheTestsDeadline(t *testing.T) {
	testDeadline, bounded := t.Deadline()
	if !bounded {
		t.Skip("the test binary has -timeout 0, so it has no deadline to compare")
	}
	ctx, cancel := teardownContext()
	defer cancel()
	got, bounded := ctx.Deadline()
	if !bounded {
		t.Fatal("teardown context has no deadline in a bounded test binary")
	}
	wantFloor := testDeadline.Add(testwait.TeardownGrace - time.Second)
	if got.Before(wantFloor) {
		t.Fatalf("teardown deadline %s is before the test binary's deadline plus its grace, %s: Main read test.timeout before m.Run parsed it",
			got.Format(time.RFC3339Nano), wantFloor.Format(time.RFC3339Nano))
	}
}
