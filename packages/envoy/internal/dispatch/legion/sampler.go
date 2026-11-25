package legion

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// SampleLease is the lease a pass of the sampler runs under. However many replicas serve the
// database, one of them reads each period's states and records each change once (duty.Exclusive).
const SampleLease = "legion-state"

// SampleInterval is how often each configured daemon's state is read: one minute
// (dispatch://INFRA-1077). It is a timer and not a woken loop because nothing commits to announce
// a remote daemon's state — the daemon has no feed Dispatch listens to, so a tick is the only way
// a change is ever found.
const SampleInterval = time.Minute

// Sampler reads each configured project's daemon state and records what moved. One loop runs one
// Sampler, so its client cache needs no lock.
type Sampler struct {
	pool *store.Pool
	now  func() time.Time
	// clients is one daemon client per state URL, kept across passes so a minute's read reuses
	// the connection the last one left rather than dialling afresh, and dropped as soon as no
	// setting names its URL.
	clients map[string]*Client
}

// NewSampler is the sampler RunSampler runs; a test drives its Pass itself.
func NewSampler(pool *store.Pool) *Sampler {
	return &Sampler{pool: pool, now: time.Now, clients: map[string]*Client{}}
}

// RunSampler reads every configured project's daemon state every SampleInterval until ctx ends,
// each pass under SampleLease. The first tick is at start, so a fresh deployment's board is
// populated at boot rather than a minute later.
func RunSampler(ctx context.Context, pool *store.Pool, leases duty.Leases) {
	duty.RunLeased(ctx, leases, SampleLease, SampleInterval, duty.TickAtStart, NewSampler(pool).Pass)
}

// Pass is one pass: every configured project's daemon read in turn, each recorded whatever the
// others did, and then the reads older than ReadRetention deleted. A project whose read fails —
// the daemon did not answer its state, or Dispatch could not store the answer — keeps the
// observations it already has: the failure is in legion_reads, and the board's header names it
// there.
func (s *Sampler) Pass(ctx context.Context) {
	settings, err := ListSettings(ctx, s.pool)
	if err != nil {
		slog.Warn("dispatch: read the Legion settings", "error", err)
		return
	}
	s.keepClients(settings)
	for _, setting := range settings {
		s.read(ctx, setting)
	}
	// Once a pass, under the same lease, as the delivery reconcile prunes its own change log:
	// the table gains a row per project per minute and the board reads only the newest of each.
	if pruned, err := PruneReads(ctx, s.pool); err != nil {
		slog.Warn("dispatch: prune the Legion state reads", "error", err)
	} else if pruned > 0 {
		slog.Info("dispatch: pruned Legion state reads", "rows", pruned)
	}
}

// read is one project's read, the record it leaves, and the notification that wakes every open
// Legion board of the project (StateChannel). A read the daemon answered is recorded with its
// observations in one transaction (record); a read that failed, and one whose record Postgres
// refused, are recorded as a failed read (RecordFailedRead), the refusal as `record: <why>`, so the
// last good read stands and the board's header names what failed. The notification goes out after
// a failed read too: the board's header carries the failure.
func (s *Sampler) read(ctx context.Context, setting Settings) {
	state, bytes, readErr := s.clients[setting.StateURL].State(ctx)
	at := s.now().UTC()
	defer func() {
		if err := NotifyRead(ctx, s.pool, setting.Project); err != nil {
			slog.Warn("dispatch: announce a Legion state read", "project", setting.Project, "error", err)
		}
	}()
	if readErr != nil {
		slog.Warn("dispatch: read a Legion daemon's state", "project", setting.Project, "bytes", bytes, "error", readErr)
	} else {
		written, err := s.record(ctx, setting.Project, at, bytes, state)
		if err == nil {
			if written > 0 {
				slog.Info("dispatch: recorded Legion state changes", "project", setting.Project, "rows", written, "issues", len(state.Issues))
			}
			return
		}
		slog.Warn("dispatch: record a Legion daemon's state", "project", setting.Project, "error", err)
		readErr = fmt.Errorf("record: %w", err)
	}
	if err := RecordFailedRead(ctx, s.pool, setting.Project, at, bytes, readErr); err != nil {
		slog.Warn("dispatch: record a Legion state read", "project", setting.Project, "error", err)
	}
}

// record records a read the daemon answered as project's (RecordRead), cut to what project's
// record keeps (State.keepProject): the issues Dispatch holds as project's, and nothing of any
// other project the daemon reports. A daemon that serves several projects reports all of their
// issues, and a project's setting may name any daemon, so what decides which issues are project's
// is Dispatch's own record of each key (projectIssues), never the daemon's answer.
func (s *Sampler) record(ctx context.Context, project string, at time.Time, bytes int, state State) (int64, error) {
	own, err := projectIssues(ctx, s.pool, project, state.issueKeys())
	if err != nil {
		return 0, err
	}
	state.keepProject(project, own)
	return RecordRead(ctx, s.pool, project, at, bytes, state.Observations())
}

// keepClients leaves one client per state URL the settings name, keeping the client each URL
// already had. A read presents no credential: `GET /legion/v1/state` authenticates nobody.
func (s *Sampler) keepClients(settings []Settings) {
	kept := make(map[string]*Client, len(settings))
	for _, setting := range settings {
		client, held := s.clients[setting.StateURL]
		if !held {
			client = New(setting.StateURL)
		}
		kept[setting.StateURL] = client
	}
	s.clients = kept
}
