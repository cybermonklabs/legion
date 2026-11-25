package daemon

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// laterRig is the refresher's backfill (backfillLater) of one record that carries no repository,
// which Dispatch resolves to acme/widgets and GitHub spells Acme/Widgets, over a faulted pool: what
// it publishes to GitHub intake and how often it asks for a read of the live records.
type laterRig struct {
	w       *workflowRuntime
	direct  *store.Pool
	fault   *testdb.CommitFault
	logs    *syncBuffer
	mu      sync.Mutex
	publish int
}

const backfillRetryLine = "workflow backfill: the records without their repository columns were not read, listed or written"

func newLaterRig(t *testing.T) *laterRig {
	direct, faulted, fault := faultedPools(t)
	if _, err := direct.Exec(context.Background(), `insert into issues
		(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-REVIEW', 'LEGION-REVIEW', 'LEGION', 'review', 'planning', 1, 'in_progress', 'A', 1)`); err != nil {
		t.Fatalf("seed the record: %v", err)
	}
	github := newRepositoryGitHub(t, "main")
	github.fullName = "Acme/Widgets"
	logs, log := watchedLog()
	r := &laterRig{direct: direct, fault: fault, logs: logs}
	r.w = &workflowRuntime{pool: faulted, records: projectRecords{Store: record.NewStore(), project: "LEGION"},
		dispatch:        &listingDispatch{summaries: []dispatch.IssueSummary{{Key: "LEGION-REVIEW", ResolvedRepo: "acme/widgets"}}},
		dispatchProject: "LEGION", stateDir: t.TempDir(), repositories: newRepositories("LEGION", ghrepo.Renames{}),
		defaultBranches: newDefaultBranches(roleTokens{}, nil, github.url, defaultBranchTTL, time.Now),
		heardWake:       make(chan struct{}, 1), liveWake: make(chan struct{}, 16), log: log,
		setRepositories: func(_ context.Context, repos []ghrepo.Repository) error {
			if !slices.Equal(repositoryNames(repos), []string{"Acme/Widgets"}) {
				t.Errorf("GitHub intake told to hear %v, want Acme/Widgets alone", repositoryNames(repos))
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			r.publish++
			return nil
		}}
	return r
}

// run is one run of the refresher's backfill job, with a record still left, and its failure: the
// warning backfillLater logs in place of an error.
func (r *laterRig) run() error {
	before := count(r.logs, backfillRetryLine)
	r.w.backfillPending.Store(true)
	r.w.backfillLater(context.Background())
	if count(r.logs, backfillRetryLine) > before {
		return errors.New("the refresher's backfill was not written")
	}
	return nil
}

func (r *laterRig) work() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return map[string]int{"GitHub intake publications of Acme/Widgets": r.publish, "live record reads asked": len(r.w.liveWake)}
}

// backfillLaterOwner is the refresher's backfill, whose hear of the GitHub spelling the record now
// keeps, and whose ask for a read of the live records, are hooks of its write's commit
// (backfillRecords).
func backfillLaterOwner(t *testing.T) *ownerRig {
	r := newLaterRig(t)
	return &ownerRig{
		fault: r.fault,
		run:   func(*testing.T) error { return r.run() },
		changed: func(t *testing.T) bool {
			var spelled string
			if err := r.direct.QueryRow(context.Background(), "select coalesce(github_repo, '') from issues where key = 'LEGION-REVIEW'").Scan(&spelled); err != nil {
				t.Fatalf("read the record: %v", err)
			}
			return spelled == "Acme/Widgets"
		},
		work: func(*testing.T) map[string]int { return r.work() },
	}
}

// A refresher's backfill whose write's outcome the store cannot say publishes nothing and asks for
// no read yet, and stays pending. Once the store says the write committed, the record's repository
// is heard and the read asked for, once; the next run finds no record left and adds nothing.
func backfillLaterUnknown(t *testing.T) {
	r := newLaterRig(t)
	fired := r.fault.Cut("update issues set repo =", testdb.CutAfterCommitRefusing)
	if err := r.run(); err == nil {
		t.Errorf("the backfill whose outcome the store cannot say = nil, want its warning")
	}
	testdb.Met(t, fired)
	if !strings.Contains(r.logs.String(), store.ErrCommitOutcomeUnknown.Error()) {
		t.Errorf("the backfill's warning names no unknown outcome: %q", r.logs.String())
	}
	if !r.w.backfillPending.Load() {
		t.Errorf("the backfill is not pending after a write whose outcome is unknown, want it tried again")
	}
	wantLater(t, r, 0, "before the store answers:")
	r.fault.Refuse(false)
	testwait.WithinPolling(t, testwait.Bounded(t, 10*time.Second, time.Second), testwait.Poll,
		"the backfill's publication and read once the store says the write committed", func() bool {
			work := r.work()
			return work["GitHub intake publications of Acme/Widgets"] == 1 && work["live record reads asked"] == 1
		})
	if err := r.run(); err != nil {
		t.Fatalf("the backfill's next run = %v, want nil", err)
	}
	if r.w.backfillPending.Load() {
		t.Errorf("the backfill is pending after the run that found no record left")
	}
	wantLater(t, r, 1, "after the store answered and the next run, but")
}

// wantLater fails the test for each kind of the refresher backfill's work not counted want times.
func wantLater(t *testing.T, r *laterRig, want int, when string) {
	t.Helper()
	work := r.work()
	for _, name := range slices.Sorted(maps.Keys(work)) {
		if work[name] != want {
			t.Errorf("%s %s = %d, want %d", when, name, work[name], want)
		}
	}
}
