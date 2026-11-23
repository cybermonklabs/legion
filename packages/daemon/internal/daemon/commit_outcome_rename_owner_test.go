package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// renameDeleteMatch is the statement of the rename write whose new name would hold another issue's
// pull request branch twice: the write that deletes its pull request row.
const renameDeleteMatch = "delete from pull_requests where issue ="

// renameRig is the rename pass of INFRA's pair over two done issues on one branch, LEGION-1 on the
// pair's `from` and LEGION-2 on its `to`, through a faulted pool, keeping no issue: LEGION-1's
// write moves its record and deletes its pull request row, which LEGION-2's holds under the new
// name.
type renameRig struct {
	w      *workflowRuntime
	pass   renamePass
	direct *store.Pool
	fault  *testdb.CommitFault
	logs   *syncBuffer
}

func newRenameRig(t *testing.T) *renameRig {
	direct, faulted, fault := faultedPools(t)
	seedRenameRecords(t, direct, "LEGION", renameSeed{key: "LEGION-1", repo: renameFrom, branch: "release/shared"},
		renameSeed{key: "LEGION-2", repo: renameTo, branch: "release/shared"})
	logs, log := watchedLog()
	w := &workflowRuntime{pool: faulted, dispatchProject: "LEGION", stateDir: t.TempDir(), repositories: transferredRepositories(),
		liveWake: make(chan struct{}, 16), log: log}
	return &renameRig{w: w, pass: w.renamePass(func(string, []string) string { return "" }), direct: direct, fault: fault, logs: logs}
}

// run is one rename pass and its failure: the line it logs for a write that failed.
func (r *renameRig) run() error {
	before := count(r.logs, renameFailedLine)
	r.pass.run(context.Background(), nil)
	if count(r.logs, renameFailedLine) > before {
		return errors.New("the rename pass's write failed")
	}
	return nil
}

func (r *renameRig) work() map[string]int {
	return map[string]int{"deletion lines": count(r.logs, renameDeletedLine), "live record reads asked": len(r.w.liveWake)}
}

// renameWriteOwner is the rename pass's write of one issue, whose line naming the pull request row
// it deleted, and whose ask for a read of the live records, are hooks of its commit
// (renamePass.write).
func renameWriteOwner(t *testing.T) *ownerRig {
	r := newRenameRig(t)
	return &ownerRig{
		fault:   r.fault,
		run:     func(*testing.T) error { return r.run() },
		changed: func(t *testing.T) bool { return namedAll(storedRepositoryNames(t, r.direct, "LEGION-1"), renameTo) },
		work:    func(*testing.T) map[string]int { return r.work() },
	}
}

// A rename write whose outcome the store cannot say logs no deletion and asks for no read yet. Once
// the store says it committed, its deletion line and its ask follow, once; the next pass finds no
// row on the old name and adds neither.
func renameWriteUnknown(t *testing.T) {
	r := newRenameRig(t)
	fired := r.fault.Cut(renameDeleteMatch, testdb.CutAfterCommitRefusing)
	if err := r.run(); err == nil {
		t.Errorf("the pass whose write's outcome the store cannot say = nil, want its failed write's line")
	}
	testdb.Met(t, fired)
	if !strings.Contains(r.logs.String(), store.ErrCommitOutcomeUnknown.Error()) {
		t.Errorf("the pass's log names no unknown outcome: %q", r.logs.String())
	}
	if work := r.work(); work["deletion lines"] != 0 || work["live record reads asked"] != 0 {
		t.Fatalf("before the store answers: %v, want none", work)
	}
	r.fault.Refuse(false)
	testwait.WithinPolling(t, testwait.Bounded(t, 10*time.Second, time.Second), testwait.Poll,
		"the write's deletion line and read once the store says it committed", func() bool {
			work := r.work()
			return work["deletion lines"] == 1 && work["live record reads asked"] == 1
		})
	if err := r.run(); err != nil {
		t.Fatalf("the next pass = %v, want nil", err)
	}
	if work := r.work(); work["deletion lines"] != 1 || work["live record reads asked"] != 1 {
		t.Errorf("after the next pass: %v, want each once", work)
	}
}
