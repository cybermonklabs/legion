package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/clock"
	"github.com/sjawhar/legion/daemon/internal/clock/clocktest"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// faultyStore is a record store whose next live-record snapshot fails once, and whose write of
// pauseKey's record waits at reached until release closes.
type faultyStore struct {
	record.Store
	failIssues bool
	pauseKey   string
	reached    chan struct{}
	release    chan struct{}
}

func (s *faultyStore) Issues(ctx context.Context, tx pgx.Tx) ([]record.Issue, error) {
	if s.failIssues {
		s.failIssues = false
		return nil, errors.New("the database refused the live-record snapshot")
	}
	return s.Store.Issues(ctx, tx)
}

func (s *faultyStore) PutIssue(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
	if issue.Key == s.pauseKey {
		s.pauseKey = ""
		close(s.reached)
		<-s.release
	}
	return s.Store.PutIssue(ctx, tx, issue)
}

// mappingRig is a workflow runtime over a fresh database and NATS whose project maps acme/widgets
// (its default) and zeta/tools, each App installed on each but those missing names ("<role>
// <owner/name>"), with GitHub intake hearing what the boot's mapping serves. Its clock is a fake
// that stands still until the test moves it (clocktest.New), so the refresher reads the mapping
// again of its own only when the test fires its wait (recheck).
func mappingRig(t *testing.T, missing map[string]bool) (*workflowRuntime, *mappingDispatch, *installations, jetstream.Stream) {
	t.Helper()
	return mappingRigWith(t, &installations{missing: missing})
}

// mappingRigWith is mappingRig answering each installation read from installs, with each of opts
// applied to the runtime before its boot reads the mapping.
func mappingRigWith(t *testing.T, installs *installations, opts ...func(*workflowRuntime)) (*workflowRuntime, *mappingDispatch, *installations, jetstream.Stream) {
	t.Helper()
	w, client, installs, stream, _ := mappingRigJS(t, installs, opts...)
	return w, client, installs, stream
}

// mappingRigJS is mappingRigWith with the JetStream it opened, for a test that publishes on it.
func mappingRigJS(t *testing.T, installs *installations, opts ...func(*workflowRuntime)) (*workflowRuntime, *mappingDispatch, *installations, jetstream.Stream, jetstream.JetStream) {
	t.Helper()
	return mappingRigDriven(t, installs, nil, opts...)
}

// mappingRigDriven is mappingRigJS whose boot reads the mapping (loadMapping, then nameMapping) on
// a goroutine of its own while drive, when set, runs on the test goroutine: a boot that waits on
// the rig's fake clock (waitOutLimits) is moved past that wait by drive, and the rig is returned
// once the boot has read the mapping. A boot still waiting when the test ends is stopped then.
func mappingRigDriven(t *testing.T, installs *installations, drive func(*workflowRuntime), opts ...func(*workflowRuntime)) (*workflowRuntime, *mappingDispatch, *installations, jetstream.Stream, jetstream.JetStream) {
	t.Helper()
	ctx := context.Background()
	conn := testnats.Connect(t, workflowNATS(t))
	js, err := jetstream.New(conn, testnats.RequestTimeout(t))
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.Stream(ctx, "ENVOY_NOTIFICATIONS")
	if err != nil {
		t.Fatal(err)
	}
	github := newNamedGitHub(t, map[string]string{"acme/widgets": "acme/widgets@main", "zeta/tools": "zeta/tools@main"})
	client := &mappingDispatch{mappings: []dispatch.RepoProject{{Repo: "acme/widgets", Project: "LEGION", Default: true}, {Repo: "zeta/tools", Project: "LEGION"}}}
	if installs.missing == nil {
		installs.missing = map[string]bool{}
	}
	w := &workflowRuntime{pool: isolatedOutboxPool(t), records: record.NewStore(), dispatch: client, dispatchProject: "LEGION", installs: installs, clock: clocktest.New(time.Now()),
		repositories: newRepositories("LEGION", ghrepo.Renames{}), defaultBranches: newDefaultBranches(roleTokens{}, nil, github.url, defaultBranchTTL, time.Now), log: quietLogger(),
		mappingChanged: make(chan struct{}, 1)}
	engine := workflow.New(w.records, workflow.Config{Project: "LEGION", Repositories: w, NoteUnread: w.noteUnread, HearRepository: w.hearRecorded,
		Steps: config.DesignGateOff.Steps()}, quietLogger())
	w.engine, w.admission = engine, admit.New(w.records, engine, 3, "LEGION", quietLogger())
	w.handlers = []intake.Handler{w.engine, w.admission}
	for _, opt := range opts {
		opt(w)
	}
	booting, stopBoot := context.WithCancel(ctx)
	t.Cleanup(stopBoot)
	booted := make(chan error, 1)
	go func() {
		err := w.loadMapping(booting)
		if err == nil {
			err = w.nameMapping(booting)
		}
		booted <- err
	}()
	if drive != nil {
		drive(w)
	}
	if err := testwait.Receive(t, "the rig's boot to read the mapping", booted); err != nil {
		t.Fatal(err)
	}
	heard, err := w.heardAtBoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if w.consumers, err = intake.OpenConsumers(ctx, js, w.pool, intake.ConsumerSpec{Project: "LEGION", Repositories: heard, Logger: quietLogger(),
		AckWait: time.Second, NakDelay: 10 * time.Millisecond, PrepareRepository: w.prepareRepository, RepositoryMapping: w.mappingChange}); err != nil {
		t.Fatal(err)
	}
	running, stop := context.WithCancel(ctx)
	refreshed := make(chan struct{})
	go func() { w.refreshOnMappingChange(running); close(refreshed) }()
	t.Cleanup(func() { stop(); <-refreshed })
	return w, client, installs, stream, js
}

// rigHandlers is the rig's engine and admission (of three slots), wired to the rig's runtime as the
// boot wires the daemon's before the refresher starts, so w's own applies and recoveries run
// through both.
func rigHandlers(w *workflowRuntime) (*workflow.Engine, *admit.Admission) {
	return w.engine, w.admission
}

// prepareAsDelivered prepares fact as the Dispatch consumer does, delivering it again after a
// held-back preparation (intake.RetryLater) until it is prepared or fails.
func prepareAsDelivered(t *testing.T, w *workflowRuntime, fact intake.DispatchIssue) (intake.DispatchIssue, error) {
	t.Helper()
	var prepared intake.DispatchIssue
	err := testwait.Until(t, fact.Key+"'s preparation, delivered again while it is held back (intake.RetryLater)", func() (error, bool) {
		var err error
		prepared, err = w.prepareRepository(context.Background(), fact)
		var later *intake.RetryLater
		return err, !errors.As(err, &later)
	})
	return prepared, err
}

func githubFilterSubjects(t *testing.T, stream jetstream.Stream) []string {
	t.Helper()
	consumer, err := stream.Consumer(context.Background(), "legion-go-LEGION-github")
	if err != nil {
		t.Fatal(err)
	}
	info, err := consumer.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return info.Config.FilterSubjects
}

func recordedIssue(t *testing.T, w *workflowRuntime, key string) *record.Issue {
	t.Helper()
	var got *record.Issue
	if err := pgx.BeginFunc(context.Background(), w.pool, func(tx pgx.Tx) error {
		var err error
		got, err = w.records.Issue(context.Background(), tx, key)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return got
}

// A read of the mapping publishes a repository as served only once GitHub intake hears it: a read
// that fails before its subscription update leaves the repository left out, so no issue is
// recorded on it, and the next read covers the issue that came up for admission. An issue is never
// recorded on a repository GitHub intake does not hear.
func TestAFailedRefreshServesNothingUntilARetrySubscribesIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail bool
	}{
		{"a refresh that succeeds", false},
		{"a refresh whose live-record snapshot fails once", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			w, client, installs, stream := mappingRig(t, map[string]bool{"review zeta/tools": true})
			client.issue = dispatch.Issue{Key: "LEGION-301", Status: "todo", HandedOver: true, ResolvedRepo: "zeta/tools", LastSeq: 1}
			installs.missing["review zeta/tools"] = false
			w.records = &faultyStore{Store: w.records, failIssues: tc.fail}
			fact := intake.DispatchIssue{Key: "LEGION-301", Seq: 1, Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: "zeta/tools"}
			if tc.fail {
				if err := w.refreshMapping(ctx); err == nil {
					t.Fatal("the failed snapshot did not fail the read")
				}
				if !w.repositories.lookup("zeta/tools").leftOut {
					t.Fatal("the failed refresh published zeta/tools as served")
				}
				if filters := githubFilterSubjects(t, stream); slices.Contains(filters, "notifications.github.zeta.tools.>") {
					t.Fatalf("GitHub filters = %v after a failed refresh, want zeta/tools not yet heard", filters)
				}
			}
			if _, err := prepareAsDelivered(t, w, fact); err != nil {
				t.Fatal(err)
			}
			engine := workflow.New(w.records, workflow.Config{Project: "LEGION", Repositories: w, Steps: config.DesignGateOff.Steps()}, quietLogger())
			admission := admit.New(w.records, engine, 1, "LEGION", quietLogger())
			if _, err := intake.ApplyFact(ctx, w.pool, "test", "admission", fact, engine, admission); err != nil {
				t.Fatal(err)
			}
			if got := recordedIssue(t, w, fact.Key); got == nil || got.Repo != "zeta/tools" {
				t.Fatalf("LEGION-301 = %+v, want it recorded on zeta/tools", got)
			}
			if filters := githubFilterSubjects(t, stream); !slices.Contains(filters, "notifications.github.zeta.tools.>") {
				t.Fatalf("GitHub filters = %v, want zeta/tools heard once an issue is recorded on it", filters)
			}
		})
	}
}

// A refresh that unmaps a repository and an admission recording an issue on it are serialized: an
// admission that resolved the repository before the refresh commits before the refresh reads the
// records, so the repository it recorded stays heard. A refresh never unsubscribes a
// repository an admission is about to commit an issue on.
func TestARefreshThatUnmapsARepositoryKeepsHearingAnIssueAdmittedOnIt(t *testing.T) {
	ctx := context.Background()
	w, client, _, stream := mappingRig(t, map[string]bool{})
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		if err := w.records.PutIssue(ctx, tx, record.Issue{Key: "LEGION-300", Tree: "LEGION-300", Project: "LEGION", Title: "root", Phase: phase.Planning,
			Generation: 1, Status: "in_progress", Rank: "A", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}); err != nil {
			return err
		}
		if err := w.records.PutSlot(ctx, tx, record.Slot{Issue: "LEGION-300", Index: 0, AdmittedAt: time.Now()}); err != nil {
			return err
		}
		return openTreeTx(ctx, tx, "LEGION", "LEGION-300")
	}); err != nil {
		t.Fatal(err)
	}
	store := &faultyStore{Store: w.records, pauseKey: "LEGION-301", reached: make(chan struct{}), release: make(chan struct{})}
	w.records = store
	engine := workflow.New(store, workflow.Config{Project: "LEGION", Repositories: w, Steps: config.DesignGateOff.Steps()}, quietLogger())
	child := intake.DispatchIssue{Key: "LEGION-301", Parent: "LEGION-300", Seq: 1, Status: "todo", Title: "child", Rank: "A", ResolvedRepo: "zeta/tools"}
	if _, err := w.prepareRepository(ctx, child); err != nil {
		t.Fatal(err)
	}
	admitted := make(chan error, 1)
	go func() {
		_, err := intake.ApplyFact(ctx, w.pool, "test", "child", child, engine)
		admitted <- err
	}()
	testwait.Receive(t, "the admission to reach its record write", store.reached)
	client.set(dispatch.RepoProject{Repo: "acme/widgets", Project: "LEGION", Default: true})
	refreshed := make(chan error, 1)
	go func() { refreshed <- w.refreshMapping(ctx) }()
	// The refresh reads the records under the fact serializer the paused admission holds, so it
	// waits on that lock until the admission commits. Without the serialization it would end
	// here, while the admission waits.
	var early error
	ended := false
	testwait.Eventually(t, "the refresh to wait on the fact serializer the paused admission holds", func() bool {
		select {
		case early = <-refreshed:
			ended = true
			return true
		default:
		}
		return serializerWaiters(t, w) > 0
	})
	close(store.release)
	if ended {
		t.Fatalf("the refresh ended (%v) while the admission held the fact serializer, want it to wait for the admission", early)
	}
	if err := testwait.Receive(t, "the admission to commit", admitted); err != nil {
		t.Fatal(err)
	}
	if err := testwait.Receive(t, "the refresh to end once the admission committed", refreshed); err != nil {
		t.Fatal(err)
	}
	if got := recordedIssue(t, w, child.Key); got == nil || got.Repo != "zeta/tools" {
		t.Fatalf("LEGION-301 = %+v, want it recorded on zeta/tools", got)
	}
	if filters := githubFilterSubjects(t, stream); !slices.Contains(filters, "notifications.github.zeta.tools.>") {
		t.Fatalf("GitHub filters = %v, want zeta/tools heard: a live child is recorded on it", filters)
	}
}

// serializerWaiters is how many transactions of w's database wait for an advisory lock, the fact
// serializer (intake.SerializeFacts) among them.
func serializerWaiters(t *testing.T, w *workflowRuntime) int {
	t.Helper()
	var waiting int
	if err := w.pool.QueryRow(context.Background(), `select count(*) from pg_locks where locktype = 'advisory' and not granted
		and database = (select oid from pg_database where datname = current_database())`).Scan(&waiting); err != nil {
		t.Fatal(err)
	}
	return waiting
}

// heldSubscription stands in for the runtime's subscription update (workflowRuntime's
// setRepositories), which a test cannot otherwise hold: intake's SetRepositories is a method of a
// concrete type the runtime owns. Its first call says it is in flight at reached and waits there
// until free, and every call after it — the refresh's second one, and any the refresher makes of
// its own — goes to intake untouched, so the test still reads what intake really hears.
type heldSubscription struct {
	w       *workflowRuntime
	held    atomic.Bool
	reached chan struct{}
	release chan struct{}
	freed   sync.Once
}

// newHeldSubscription is a stand-in ready to be installed, as mappingRigWith's option.
func newHeldSubscription() *heldSubscription {
	return &heldSubscription{reached: make(chan struct{}), release: make(chan struct{})}
}

// install puts the stand-in in the runtime's seam, before the rig's boot: the boot opens its
// consumers itself, so nothing is held until a mapping read asks for a subscription update.
func (h *heldSubscription) install(w *workflowRuntime) {
	h.w = w
	w.setRepositories = h.set
}

func (h *heldSubscription) set(ctx context.Context, repositories []ghrepo.Repository) error {
	if !h.held.Swap(true) {
		close(h.reached)
		<-h.release
	}
	return h.w.consumers.SetRepositories(ctx, repositories)
}

// free lets the held call go on; a second free does nothing.
func (h *heldSubscription) free() { h.freed.Do(func() { close(h.release) }) }

// await waits for the stand-in's call to be in flight. The call is freed when the test ends, if
// the test has not freed it, so a test that fails while it is held leaves no refresh behind.
func (h *heldSubscription) await(t *testing.T) {
	t.Helper()
	t.Cleanup(h.free)
	testwait.Receive(t, "the mapping read to reach its subscription update", h.reached)
}

// A mapping read holds no transaction across its stream calls. The subscription update reaches
// JetStream, and a durable consumer that is missing has it read the pool the transaction would be
// on, so a read that waited on it inside a transaction asked that pool for a second connection
// while holding one; worse, the transaction holds the fact serializer, so a NATS that answered
// slowly stopped every fact of the project for as long as the call took, and the production pool
// sat idle in transaction to its last connection (INFRA-3169). A stand-in holds the call, and
// while it is held a fact's transaction, which takes the same serializer, still commits; a
// refresh that holds a transaction again leaves that transaction waiting on the serializer, which
// fails this at once instead of hanging.
func TestTheMappingRefreshHoldsNoTransactionAcrossItsStreamCalls(t *testing.T) {
	ctx := context.Background()
	held := newHeldSubscription()
	w, client, _, _ := mappingRigWith(t, &installations{}, held.install)
	client.set(dispatch.RepoProject{Repo: "acme/widgets", Project: "LEGION", Default: true})
	refreshed := make(chan error, 1)
	go func() { refreshed <- w.refreshMapping(ctx) }()
	held.await(t)
	serialized := make(chan error, 1)
	go func() {
		serialized <- pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error { return intake.SerializeFacts(ctx, tx) })
	}()
	waited := false
	err := testwait.Until(t, "a fact's transaction to take the serializer while the mapping read waits on its stream call", func() (error, bool) {
		select {
		case err := <-serialized:
			return err, true
		default:
		}
		waited = serializerWaiters(t, w) > 0
		return nil, waited
	})
	if waited {
		held.free()
		t.Fatal("a fact's transaction waits on the serializer while the mapping read waits on its stream call: the read holds a transaction across it")
	}
	if err != nil {
		t.Fatal(err)
	}
	held.free()
	if err := testwait.Receive(t, "the mapping read to end", refreshed); err != nil {
		t.Fatal(err)
	}
}

// An admission that resolved a repository of the old mapping and committed before the read's
// record snapshot leaves that repository heard: the snapshot reads the record it wrote, and the
// set the read narrows the subscription to carries it. The admission commits here while the
// read's first stream call is held, which is the window the read's superset covers — the old
// mapping still serves zeta/tools, so an admission in it can record an issue there — and before
// the snapshot, which is the serializer's own ordering.
func TestAnAdmissionAgainstTheOldMappingStaysHeard(t *testing.T) {
	ctx := context.Background()
	held := newHeldSubscription()
	w, client, _, stream := mappingRigWith(t, &installations{}, held.install)
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		if err := w.records.PutIssue(ctx, tx, record.Issue{Key: "LEGION-300", Tree: "LEGION-300", Project: "LEGION", Title: "root", Phase: phase.Planning,
			Generation: 1, Status: "in_progress", Rank: "A", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}); err != nil {
			return err
		}
		if err := w.records.PutSlot(ctx, tx, record.Slot{Issue: "LEGION-300", Index: 0, AdmittedAt: time.Now()}); err != nil {
			return err
		}
		return openTreeTx(ctx, tx, "LEGION", "LEGION-300")
	}); err != nil {
		t.Fatal(err)
	}
	// The child resolves its repository against the mapping as it stands, which serves zeta/tools.
	child := intake.DispatchIssue{Key: "LEGION-301", Parent: "LEGION-300", Seq: 1, Status: "todo", Title: "child", Rank: "A", ResolvedRepo: "zeta/tools"}
	if _, err := w.prepareRepository(ctx, child); err != nil {
		t.Fatal(err)
	}
	client.set(dispatch.RepoProject{Repo: "acme/widgets", Project: "LEGION", Default: true})
	refreshed := make(chan error, 1)
	go func() { refreshed <- w.refreshMapping(ctx) }()
	held.await(t)
	engine := workflow.New(w.records, workflow.Config{Project: "LEGION", Repositories: w, Steps: config.DesignGateOff.Steps()}, quietLogger())
	_, err := intake.ApplyFact(testwait.Context(t), w.pool, "test", "child", child, engine)
	if err != nil {
		held.free()
		t.Fatalf("the admission against the old mapping: %v", err)
	}
	held.free()
	if err := testwait.Receive(t, "the mapping read to end", refreshed); err != nil {
		t.Fatal(err)
	}
	if got := recordedIssue(t, w, child.Key); got == nil || got.Repo != "zeta/tools" {
		t.Fatalf("LEGION-301 = %+v, want it recorded on zeta/tools", got)
	}
	if filters := githubFilterSubjects(t, stream); !slices.Contains(filters, "notifications.github.zeta.tools.>") {
		t.Fatalf("GitHub filters = %v, want zeta/tools heard: an admission against the old mapping recorded an issue on it", filters)
	}
}

// gatedInstallations is installations whose reads, once armed, say they are in flight at entered
// and wait there until release closes: a read of the mapping held in its installation check, with
// mappingMu held, for as long as a test needs it held.
type gatedInstallations struct {
	*installations
	armed   atomic.Bool
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (g *gatedInstallations) InstalledOn(ctx context.Context, role appauth.AppRole, repo ghrepo.Repository) (bool, error) {
	if g.armed.Load() {
		g.once.Do(func() { close(g.entered) })
		select {
		case <-g.release:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	return g.installations.InstalledOn(ctx, role, repo)
}

// A recording on a repository GitHub intake already hears waits on no read of the mapping. A read
// holds mappingMu for its whole run — its Dispatch listing, its installation reads of up to
// installReadAttempt each, its reads of GitHub's names — and the recording's commit hook took
// that lock before it looked at what intake hears, so every admission, a mapped repository's
// among them, stood still on the Dispatch consumer's goroutine for as long as a read ran. Here a
// read is held in its installation check while a root is admitted on acme/widgets, which the
// mapping serves and intake hears: the admission must finish while the read is still held.
func TestARecordingOnAHeardRepositoryWaitsOnNoReadOfTheMapping(t *testing.T) {
	ctx := context.Background()
	gate := &gatedInstallations{installations: &installations{}, entered: make(chan struct{}), release: make(chan struct{})}
	w, client, _, _ := mappingRigWith(t, gate.installations, func(w *workflowRuntime) { w.installs = gate })
	release := sync.OnceFunc(func() { close(gate.release) })
	t.Cleanup(release)
	gate.armed.Store(true)
	w.requestMappingRead()
	testwait.Receive(t, "the read of the mapping to reach its installation check", gate.entered)

	root := issue("LEGION-990", "acme/widgets", "todo", true, "", 1)
	client.show(root)
	fact := intake.DispatchIssue{Key: root.Key, Seq: 1, Type: "issue.updated", Status: "todo", Title: root.Title, Rank: root.Rank, HandedOver: true, ResolvedRepo: "acme/widgets"}
	admitted, done := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(done)
		prepared, err := w.prepareRepository(ctx, fact)
		if err == nil {
			_, err = intake.ApplyFact(ctx, w.pool, "test", root.Key+"-todo-1", prepared, w.handlers...)
		}
		admitted <- err
	}()
	t.Cleanup(func() { release(); <-done })
	// The read stays held until the test ends, so a recording that waits on the read's lock never
	// finishes, and this wait names that at the run's guard.
	if err := testwait.Receive(t, "the admission on acme/widgets, which intake hears, to finish while a read of the mapping is held: a recording that waits on the read's lock never does", admitted); err != nil {
		t.Fatalf("the admission on acme/widgets: %v", err)
	}
	if got := recordedIssue(t, w, root.Key); got == nil || got.Repo != "acme/widgets" {
		t.Fatalf("%s = %+v, want it admitted on acme/widgets", root.Key, got)
	}
}

// narrowingHeld stands in for the runtime's subscription update (workflowRuntime's
// setRepositories): it holds the first call whose set does not name zeta/tools — a read of the
// mapping narrowing what intake hears once the mapping stops serving zeta/tools — at reached
// until release closes, and hands every call to intake, so the test reads what intake hears.
type narrowingHeld struct {
	w       *workflowRuntime
	held    atomic.Bool
	reached chan struct{}
	release chan struct{}
}

func (h *narrowingHeld) set(ctx context.Context, repositories []ghrepo.Repository) error {
	if !slices.Contains(repositories, ghrepo.MustParse("zeta/tools")) && !h.held.Swap(true) {
		close(h.reached)
		<-h.release
	}
	return h.w.consumers.SetRepositories(ctx, repositories)
}

// A record on a repository the mapping stops serving, committed while the read of the mapping
// that stops serving it stands between its record snapshot and its narrowing, stays heard. The
// recording finds the repository outside the set the read holds from its snapshot on, so it waits
// for the read and adds the repository after the narrowing. A recording that answered from the
// set intake was last told — the read's superset, until its narrowing has run — would find
// zeta/tools there and add nothing, and the narrowing would then drop it from what intake hears
// while a live record names it: the plain fast path, which this guards against.
func TestARecordCommittedBeforeAReadsNarrowingStaysHeard(t *testing.T) {
	ctx := context.Background()
	held := &narrowingHeld{reached: make(chan struct{}), release: make(chan struct{})}
	w, client, _, stream := mappingRigWith(t, &installations{}, func(w *workflowRuntime) { held.w, w.setRepositories = w, held.set })
	release := sync.OnceFunc(func() { close(held.release) })
	t.Cleanup(release)
	client.set(dispatch.RepoProject{Repo: "acme/widgets", Project: "LEGION", Default: true})
	refreshed, read := make(chan error, 1), make(chan struct{})
	go func() { defer close(read); refreshed <- w.refreshMapping(ctx) }()
	t.Cleanup(func() { release(); <-read })
	testwait.Receive(t, "the read of the mapping to reach its narrowing", held.reached)

	// The read has published the mapping that no longer serves zeta/tools and holds its
	// narrowing, so a root handed over on zeta/tools now is on a repository no mapping names:
	// read from GitHub, and recorded on what GitHub answers.
	root := issue("LEGION-995", "zeta/tools", "todo", true, "", 1)
	client.show(root)
	fact := intake.DispatchIssue{Key: root.Key, Seq: 1, Type: "issue.updated", Status: "todo", Title: root.Title, Rank: root.Rank, HandedOver: true, ResolvedRepo: "zeta/tools"}
	admitted, done := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(done)
		prepared, err := w.prepareRepository(ctx, fact)
		if err == nil {
			_, err = intake.ApplyFact(ctx, w.pool, "test", root.Key+"-todo-1", prepared, w.handlers...)
		}
		admitted <- err
	}()
	t.Cleanup(func() { release(); <-done })
	// The commit hook adds the repository to the cache set and then looks at what intake hears,
	// so once the cache set names zeta/tools the record has committed and the hook is at its look.
	// A recording that waits for the read parks on the read's lock (hearRepository's mappingMu),
	// which the goroutines' stacks show; one that answered from the set intake was last told
	// never parks there, and this wait names that at the run's guard.
	testwait.Eventually(t, root.Key+"'s commit hook running", func() bool {
		return slices.Contains(repositoryNames(w.cacheRepositories()), "zeta/tools")
	})
	testwait.Eventually(t, root.Key+"'s recording to wait for the read on its lock (hearRepository's mappingMu)", func() bool {
		return parkedOnALockIn(".(*workflowRuntime).hearRepository(")
	})
	release()
	if err := <-refreshed; err != nil {
		t.Fatalf("the read of the mapping: %v", err)
	}
	if err := <-admitted; err != nil {
		t.Fatalf("the admission on zeta/tools: %v", err)
	}
	if got := recordedIssue(t, w, root.Key); got == nil || got.Repo != "zeta/tools" {
		t.Fatalf("%s = %+v, want it recorded on zeta/tools", root.Key, got)
	}
	if filters := githubFilterSubjects(t, stream); !slices.Contains(filters, "notifications.github.zeta.tools.>") {
		t.Fatalf("GitHub filters = %v, want zeta/tools heard: %s is recorded on it", filters, root.Key)
	}
}

// GitHub intake hears every repository a record of the project names, whatever its tree's state:
// a root that closed on a repository the mapping no longer serves is heard across the refresh
// that unmaps it and across a restart, so a person moving it back to todo re-admits it on a
// repository whose pull request facts still arrive.
func TestAClosedTreesRepositoryStaysHeardAfterItIsUnmapped(t *testing.T) {
	ctx := context.Background()
	w, client, _, stream := mappingRig(t, map[string]bool{})
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		return w.records.PutIssue(ctx, tx, record.Issue{Key: "LEGION-300", Tree: "LEGION-300", Project: "LEGION", Title: "root", Phase: phase.Done,
			Generation: 1, Status: "done", Rank: "A", Repo: "zeta/tools", DefaultBranch: "main", GitHubRepo: "zeta/tools"})
	}); err != nil {
		t.Fatal(err)
	}
	client.set(dispatch.RepoProject{Repo: "acme/widgets", Project: "LEGION", Default: true})
	if err := w.refreshMapping(ctx); err != nil {
		t.Fatal(err)
	}
	if filters := githubFilterSubjects(t, stream); !slices.Contains(filters, "notifications.github.zeta.tools.>") {
		t.Fatalf("GitHub filters = %v after unmapping zeta/tools, want it heard: a closed root is recorded on it", filters)
	}
	heard, err := w.heardAtBoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := repositoryNames(heard), []string{"acme/widgets", "zeta/tools"}; !slices.Equal(got, want) {
		t.Fatalf("a restart hears %v, want %v", got, want)
	}
	reopened := intake.DispatchIssue{Key: "LEGION-300", Seq: 2, Status: "todo", Title: "root", Rank: "A", HandedOver: true, ResolvedRepo: "zeta/tools"}
	if reopened, err = w.prepareRepository(ctx, reopened); err != nil {
		t.Fatal(err)
	}
	engine := workflow.New(w.records, workflow.Config{Project: "LEGION", Repositories: w, Steps: config.DesignGateOff.Steps()}, quietLogger())
	admission := admit.New(w.records, engine, 1, "LEGION", quietLogger())
	if _, err := intake.ApplyFact(ctx, w.pool, "test", "reopen", reopened, engine, admission); err != nil {
		t.Fatal(err)
	}
	if got := recordedIssue(t, w, "LEGION-300"); got == nil || got.Phase != phase.Admitted || got.Repo != "zeta/tools" {
		t.Fatalf("LEGION-300 = %+v, want it re-admitted on zeta/tools", got)
	}
	if filters := githubFilterSubjects(t, stream); !slices.Contains(filters, "notifications.github.zeta.tools.>") {
		t.Fatalf("GitHub filters = %v, want zeta/tools heard for the re-admitted root", filters)
	}
}

// GitHub intake hears a repository as GitHub spells it, which Envoy's subjects carry, whatever
// spelling an issue's record keeps for its workspace: a record the backfill filled with a clone's
// spelling is heard once, under the served repository's GitHub spelling, and a record on a
// repository the mapping no longer serves is heard under the GitHub spelling its record keeps, with
// no read of GitHub.
func TestGitHubIntakeHearsEachRepositoryAsGitHubSpellsIt(t *testing.T) {
	ctx := context.Background()
	w, _, _, _ := mappingRig(t, map[string]bool{})
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		for index, issue := range []record.Issue{
			{Key: "LEGION-300", Repo: "ACME/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"},
			{Key: "LEGION-301", Repo: "orbit/gadgets", DefaultBranch: "main", GitHubRepo: "Orbit/Gadgets"},
		} {
			issue.Tree, issue.Project, issue.Title, issue.Phase, issue.Generation, issue.Status, issue.Rank = issue.Key, "LEGION", issue.Key, phase.Planning, 1, "in_progress", "A"
			if err := w.records.PutIssue(ctx, tx, issue); err != nil {
				return err
			}
			if err := w.records.PutSlot(ctx, tx, record.Slot{Issue: issue.Key, Index: index, AdmittedAt: time.Now()}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	heard, err := w.heardAtBoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := repositoryNames(heard), []string{"acme/widgets", "Orbit/Gadgets", "zeta/tools"}; !slices.Equal(got, want) {
		t.Fatalf("heard %v, want %v", got, want)
	}
}

// A project whose every mapped repository lacks an App boots serving none: GitHub intake hears no
// repository's events, and an issue handed to Legion on one is not admitted, the daemon saying why
// on it. Once the App is installed, the issue's next hand-over is held for a read of the mapping,
// which serves the repository and records the issue on it, which GitHub intake then hears.
func TestAProjectServingNoRepositoryStartsServingOnceItsAppIsInstalled(t *testing.T) {
	ctx := context.Background()
	w, client, installs, stream := mappingRig(t, map[string]bool{"review acme/widgets": true, "review zeta/tools": true})
	client.issue = dispatch.Issue{Key: "LEGION-301", Status: "todo", HandedOver: true, ResolvedRepo: "acme/widgets", LastSeq: 2}
	if served := w.repositories.Accepted(); len(served) != 0 {
		t.Fatalf("served %v, want none", served)
	}
	if filters := githubFilterSubjects(t, stream); !slices.Equal(filters, []string{"notifications.github.-.-"}) {
		t.Fatalf("GitHub filters = %v, want the one no repository publishes on", filters)
	}
	engine := workflow.New(w.records, workflow.Config{Project: "LEGION", Repositories: w, Steps: config.DesignGateOff.Steps()}, quietLogger())
	admission := admit.New(w.records, engine, 1, "LEGION", quietLogger())
	handOver := func(seq int64) {
		t.Helper()
		fact := intake.DispatchIssue{Key: "LEGION-301", Seq: seq, Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: "acme/widgets"}
		prepared, err := prepareAsDelivered(t, w, fact)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := intake.ApplyFact(ctx, w.pool, "test", "todo-"+string(rune('0'+seq)), prepared, engine, admission); err != nil {
			t.Fatal(err)
		}
	}
	handOver(1)
	if got := recordedIssue(t, w, "LEGION-301"); got != nil {
		t.Fatalf("LEGION-301 recorded as %+v while no repository is served", got)
	}
	var notes []string
	rows, err := w.pool.Query(ctx, "select payload->>'body' from outbox where issue = 'LEGION-301' and kind = 'dispatch_message'")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var note string
		if err := rows.Scan(&note); err != nil {
			t.Fatal(err)
		}
		notes = append(notes, note)
	}
	rows.Close()
	if len(notes) != 1 || !strings.Contains(notes[0], "the review App is not installed on acme/widgets") {
		t.Fatalf("notes on LEGION-301 = %q, want one naming the review App acme/widgets lacks", notes)
	}

	installs.missing["review acme/widgets"] = false
	handOver(2)
	if got := recordedIssue(t, w, "LEGION-301"); got == nil || got.Repo != "acme/widgets" {
		t.Fatalf("LEGION-301 = %+v once the App is installed, want it recorded on acme/widgets", got)
	}
	if filters := githubFilterSubjects(t, stream); !slices.Equal(filters, []string{"notifications.github.acme.widgets.>"}) {
		t.Fatalf("GitHub filters = %v, want acme/widgets heard", filters)
	}
}

// An issue handed to Legion on a repository no mapping names is not refused for the want of a
// mapping entry: its preparation reads GitHub for it and records it on what GitHub answers (R4).
// Its event is prepared as Dispatch now shows the issue, as every admission event of an
// unrecorded issue is: still handed over, it is recorded on its repository; withdrawn meanwhile
// (moved to backlog, the label taken off), it records nothing; and on another repository with no
// event of its own (an ancestor's or the project's default changed), it is recorded on the one it
// resolves to now. The repository is heard from the record that names it, with no read of the
// mapping (R5).
func TestAnIssueOnARepositoryNoMappingNamesIsAdmittedAsDispatchNowShowsIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		now    dispatch.Issue
		wantOn string
	}{
		{"still handed over in todo", dispatch.Issue{Key: "LEGION-302", Status: "todo", HandedOver: true, ResolvedRepo: "zeta/tools", LastSeq: 2}, "zeta/tools"},
		{"withdrawn while it was held", dispatch.Issue{Key: "LEGION-302", Status: "backlog", ResolvedRepo: "zeta/tools", LastSeq: 2}, ""},
		{"on another repository with no event of its own", dispatch.Issue{Key: "LEGION-302", Status: "todo", HandedOver: true, ResolvedRepo: "acme/widgets", LastSeq: 1}, "acme/widgets"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			w, client, _, stream := mappingRig(t, map[string]bool{})
			client.set(dispatch.RepoProject{Repo: "acme/widgets", Project: "LEGION", Default: true})
			if err := w.refreshMapping(ctx); err != nil {
				t.Fatal(err)
			}
			client.issue = tc.now
			fact := intake.DispatchIssue{Key: "LEGION-302", Seq: 1, Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: "zeta/tools"}
			prepared, err := prepareAsDelivered(t, w, fact)
			if err != nil {
				t.Fatal(err)
			}
			if len(w.awaiting) != 0 {
				t.Fatalf("held %v after the event was prepared, want none", w.awaiting)
			}
			engine := workflow.New(w.records, workflow.Config{Project: "LEGION", Repositories: w, HearRepository: w.hearRecorded, Steps: config.DesignGateOff.Steps()}, quietLogger())
			admission := admit.New(w.records, engine, 1, "LEGION", quietLogger())
			if _, err := intake.ApplyFact(ctx, w.pool, "test", "handover", prepared, engine, admission); err != nil {
				t.Fatal(err)
			}
			got := recordedIssue(t, w, "LEGION-302")
			if tc.wantOn == "" {
				if got != nil {
					t.Fatalf("LEGION-302 = %+v, want nothing recorded: the person withdrew it while its event was held", got)
				}
				return
			}
			if got == nil || got.Repo != tc.wantOn {
				t.Fatalf("LEGION-302 = %+v, want it recorded on %s", got, tc.wantOn)
			}
			if filters := githubFilterSubjects(t, stream); !slices.Contains(filters, "notifications.github."+strings.ReplaceAll(tc.wantOn, "/", ".")+".>") {
				t.Fatalf("GitHub filters = %v, want %s heard", filters, tc.wantOn)
			}
		})
	}
}

// An admission event whose preparation failed for any reason (here, the repository's default
// branch read from GitHub), held for the mapping first or not, is delivered again after the
// issue's newer events, so it is prepared as Dispatch now shows the issue: a person who withdrew
// it meanwhile is not overruled, and one still handed over is recorded.
func TestAnAdmissionWhosePreparationFailedIsPreparedAsDispatchNowShowsIt(t *testing.T) {
	for _, tc := range []struct {
		name      string
		heldFirst bool
		now       dispatch.Issue
		admitted  bool
	}{
		{"withdrawn after a failed branch read", false, dispatch.Issue{Key: "LEGION-305", Status: "backlog", ResolvedRepo: "acme/widgets", LastSeq: 2}, false},
		{"withdrawn after a hold and a failed branch read", true, dispatch.Issue{Key: "LEGION-305", Status: "backlog", ResolvedRepo: "zeta/tools", LastSeq: 2}, false},
		{"still handed over after a failed branch read", false, dispatch.Issue{Key: "LEGION-305", Status: "todo", HandedOver: true, ResolvedRepo: "acme/widgets", LastSeq: 2}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			w, client, installs, _ := mappingRig(t, map[string]bool{})
			repo := "acme/widgets"
			if tc.heldFirst {
				repo = "zeta/tools"
				// zeta/tools is mapped with a role App the daemon's last read found missing, so
				// an admission on it is held for a read of the mapping; the App is installed
				// before that read is made.
				installs.setMissing(true, "review zeta/tools")
				if err := w.refreshMapping(ctx); err != nil {
					t.Fatal(err)
				}
				installs.setMissing(false, "review zeta/tools")
			}
			github := &failingGitHub{named: map[string]string{"acme/widgets": "acme/widgets@main", "zeta/tools": "zeta/tools@main"}}
			w.defaultBranches = newDefaultBranches(roleTokens{}, w.repositories, github.start(t), 0, time.Now)
			client.issue = dispatch.Issue{Key: "LEGION-305", Status: "todo", HandedOver: true, ResolvedRepo: repo, LastSeq: 1}
			fact := intake.DispatchIssue{Key: "LEGION-305", Seq: 1, Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: repo}
			if tc.heldFirst {
				var later *intake.RetryLater
				if _, err := w.prepareRepository(ctx, fact); !errors.As(err, &later) {
					t.Fatalf("the first delivery = %v, want it held for the mapping", err)
				}
				testwait.Eventually(t, "the mapping read the held event asked for", func() bool {
					return w.repositories.lookup("zeta/tools").refusal == ""
				})
			}
			github.fail.Store(true)
			if _, err := w.prepareRepository(ctx, fact); err == nil {
				t.Fatal("the delivery whose branch read failed was prepared, want it retried")
			}
			github.fail.Store(false)
			client.issue = tc.now
			prepared, err := prepareAsDelivered(t, w, fact)
			if err != nil {
				t.Fatal(err)
			}
			engine := workflow.New(w.records, workflow.Config{Project: "LEGION", Repositories: w, Steps: config.DesignGateOff.Steps()}, quietLogger())
			admission := admit.New(w.records, engine, 1, "LEGION", quietLogger())
			if _, err := intake.ApplyFact(ctx, w.pool, "test", "handover", prepared, engine, admission); err != nil {
				t.Fatal(err)
			}
			if got := recordedIssue(t, w, "LEGION-305"); (got != nil) != tc.admitted {
				t.Fatalf("LEGION-305 = %+v, want recorded %t", got, tc.admitted)
			}
			if len(w.awaiting) != 0 {
				t.Fatalf("held %v after the event was prepared, want none", w.awaiting)
			}
		})
	}
}

// Dispatch's outbox puts off an event whose publish failed and publishes the issue's later events
// first, so an admission event can reach the daemon on its first delivery after a person withdrew
// the issue. It is prepared as Dispatch now shows the issue, as a redelivered one is: the
// withdrawal stands.
func TestALateFirstDeliveryOfAnAdmissionEventIsPreparedAsDispatchNowShowsIt(t *testing.T) {
	ctx := context.Background()
	w, client, _, _ := mappingRig(t, map[string]bool{})
	engine := workflow.New(w.records, workflow.Config{Project: "LEGION", Repositories: w, Steps: config.DesignGateOff.Steps()}, quietLogger())
	admission := admit.New(w.records, engine, 1, "LEGION", quietLogger())
	client.issue = dispatch.Issue{Key: "LEGION-307", Status: "backlog", Title: "new", Rank: "A", ResolvedRepo: "acme/widgets", LastSeq: 2}
	withdrawal := intake.DispatchIssue{Key: "LEGION-307", Seq: 2, Status: "backlog", Title: "new", Rank: "A", ResolvedRepo: "acme/widgets"}
	late := intake.DispatchIssue{Key: "LEGION-307", Seq: 1, Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: "acme/widgets"}
	for _, delivery := range []struct {
		id    string
		event intake.DispatchIssue
	}{{"withdrawal", withdrawal}, {"late-handover", late}} {
		prepared, err := w.prepareRepository(ctx, delivery.event)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := intake.ApplyFact(ctx, w.pool, "test", delivery.id, prepared, engine, admission); err != nil {
			t.Fatal(err)
		}
	}
	if got := recordedIssue(t, w, "LEGION-307"); got != nil {
		t.Fatalf("LEGION-307 = %+v, want nothing recorded: the person withdrew it before its hand-over reached the daemon", got)
	}
}

// An unrecorded issue's admission event is prepared from Dispatch's view outside the fact lock,
// and release_children may record the issue before that view applies; release_children's own view
// may likewise follow an event that recorded it. A view never reaches a recorded issue: the event
// it was prepared in place of applies as it was delivered, with its actor (here fenced below the
// record), and release_children's, prepared in place of no event, applies nothing. Either way the
// record stays as it was, and the issue's later events, each with its own actor, carry it on.
func TestAViewPreparedBeforeTheIssueWasRecordedNeverAppliesToItsRecord(t *testing.T) {
	for _, tc := range []struct {
		name          string
		releasedFirst bool
	}{
		{"an event's view, release_children recording the issue first", true},
		{"release_children's view, an event recording the issue first", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			w, client, _, _ := mappingRig(t, map[string]bool{})
			engine := workflow.New(w.records, workflow.Config{Project: "LEGION", Repositories: w, Steps: config.DesignGateOff.Steps()}, quietLogger())
			admission := admit.New(w.records, engine, 1, "LEGION", quietLogger())
			apply := func(id string, fact intake.DispatchIssue) {
				t.Helper()
				if _, err := intake.ApplyFact(ctx, w.pool, "test", id, fact, engine, admission); err != nil {
					t.Fatal(err)
				}
			}
			atSix := dispatch.Issue{Key: "LEGION-308", Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: "acme/widgets", LastSeq: 6}
			var stale intake.DispatchIssue
			if tc.releasedFirst {
				// A session's backlog at sequence 7, whose own event the daemon has not yet read.
				client.issue = dispatch.Issue{Key: "LEGION-308", Status: "backlog", Title: "new", Rank: "A", ResolvedRepo: "acme/widgets", LastSeq: 7}
				handOver := intake.DispatchIssue{Key: "LEGION-308", Seq: 5, Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: "acme/widgets"}
				var err error
				if stale, err = w.prepareRepository(ctx, handOver); err != nil {
					t.Fatal(err)
				}
				released, err := w.prepareObserved(ctx, intake.ObservedIssue(atSix))
				if err != nil {
					t.Fatal(err)
				}
				apply("release", released)
			} else {
				var err error
				if stale, err = w.prepareObserved(ctx, intake.ObservedIssue(dispatch.Issue{Key: "LEGION-308", Status: "todo", Title: "renamed", Rank: "A", HandedOver: true, ResolvedRepo: "acme/widgets", LastSeq: 7})); err != nil {
					t.Fatal(err)
				}
				client.issue = atSix
				handOver := intake.DispatchIssue{Key: "LEGION-308", Seq: 6, Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: "acme/widgets"}
				event, err := w.prepareRepository(ctx, handOver)
				if err != nil {
					t.Fatal(err)
				}
				apply("handover", event)
			}
			before := recordedIssue(t, w, "LEGION-308")
			if before == nil || before.LastDispatchSeq != 6 {
				t.Fatalf("LEGION-308 = %+v, want it recorded at sequence 6", before)
			}
			apply("stale-view", stale)
			after := recordedIssue(t, w, "LEGION-308")
			if after.LastDispatchSeq != 6 || after.Status != before.Status || after.DispatchStatus != before.DispatchStatus || after.Title != before.Title {
				t.Fatalf("LEGION-308 = %+v after the view prepared before it was recorded, want it as recorded: %+v", after, before)
			}
		})
	}
}

// An admission event held for a read of the mapping stays held under the sequence it was
// delivered at while Dispatch moves on (a comment on the issue), so its redeliveries wait for the
// one read it asked for rather than asking again. A withdrawal meanwhile ends the hold: the
// redelivery, prepared as Dispatch now shows the issue, records nothing and leaves no hold.
func TestAHeldEventKeepsItsDeliveredSequenceAndAWithdrawalEndsItsHold(t *testing.T) {
	ctx := context.Background()
	// zeta/tools is mapped with a role App the daemon's boot found missing, so an admission on it
	// is held for a read of the mapping.
	w, client, _, _ := mappingRig(t, map[string]bool{"review zeta/tools": true})
	// Every read of the mapping fails from here, so the hold's read never covers it.
	client.mu.Lock()
	client.err = errors.New("Dispatch is restarting")
	client.mu.Unlock()
	fact := intake.DispatchIssue{Key: "LEGION-306", Seq: 1, Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: "zeta/tools"}
	var later *intake.RetryLater
	client.issue = dispatch.Issue{Key: "LEGION-306", Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: "zeta/tools", LastSeq: 1}
	if _, err := w.prepareRepository(ctx, fact); !errors.As(err, &later) {
		t.Fatalf("the first delivery = %v, want it held for a read of the mapping", err)
	}
	client.issue.LastSeq = 2
	if _, err := w.prepareRepository(ctx, fact); !errors.As(err, &later) {
		t.Fatalf("the redelivery after a comment = %v, want it still held", err)
	}
	if requested := w.mappingRequested.Load(); requested != 1 {
		t.Fatalf("%d reads of the mapping requested, want one: the hold stays under the sequence its event was delivered at", requested)
	}
	client.issue = dispatch.Issue{Key: "LEGION-306", Status: "backlog", Title: "new", Rank: "A", ResolvedRepo: "zeta/tools", LastSeq: 3}
	prepared, err := w.prepareRepository(ctx, fact)
	if err != nil || prepared.Status != "backlog" {
		t.Fatalf("the redelivery after the withdrawal = %s, %v; want it prepared as Dispatch shows it, in backlog", prepared.Status, err)
	}
	if len(w.awaiting) != 0 {
		t.Fatalf("held %v after the withdrawal, want no hold left", w.awaiting)
	}
}

// failingGitHub answers GET /repos/{owner}/{repo} as newNamedGitHub does, and 502 while fail is
// set: for every path, or only for those containing only when it is set.
type failingGitHub struct {
	named map[string]string
	only  string
	fail  atomic.Bool
}

func (g *failingGitHub) start(t *testing.T) string {
	t.Helper()
	named := newNamedGitHub(t, g.named)
	return githubtest.Server(t, githubtest.Spec{Credential: aLease, Handle: func(w http.ResponseWriter, r *http.Request) {
		if g.fail.Load() && (g.only == "" || strings.Contains(r.URL.Path, g.only)) {
			http.Error(w, `{"message":"Server Error"}`, http.StatusBadGateway)
			return
		}
		// The read goes on to newNamedGitHub under the daemon's own lease, which it holds the
		// read to (aLease).
		forward, err := http.NewRequestWithContext(r.Context(), http.MethodGet, named.url+r.URL.Path, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		forward.Header.Set("Authorization", r.Header.Get("Authorization"))
		proxied, err := http.DefaultClient.Do(forward)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer proxied.Body.Close()
		w.WriteHeader(proxied.StatusCode)
		_, _ = io.Copy(w, proxied.Body)
	}})
}

// rigClock is the fake the mapping rig's runtime runs on (mappingRigJS).
func rigClock(w *workflowRuntime) *clocktest.Fake { return w.clock.(*clocktest.Fake) }

// refresherWait is the deadline of the wait before a read of its own the refresher
// (refreshOnMappingChange) has armed on w's fake clock, and whether one is armed. The refresher's
// waits are installReadRetry's, 30 s or more, or one cut to a held recovery's bound, which at the
// production intervals falls 90 s after the third of them; every other timer of the rig is
// shorter: the bound of a read in flight (appMintAttempt, installReadAttempt) and the hold
// poller's period (defaultHoldPollInterval).
func refresherWait(w *workflowRuntime) (time.Time, bool) {
	clk := rigClock(w)
	now := clk.Now()
	for _, at := range clk.Armed() {
		if at.Sub(now) >= installReadRetry.Initial {
			return at, true
		}
	}
	return time.Time{}, false
}

// recheck fires the refresher's wait before its next read of its own on w's fake clock, once the
// refresher has armed it, and returns that wait. The refresher arms its wait only once its run
// before has ended, so recheck waits for that run too.
func recheck(t *testing.T, w *workflowRuntime) time.Duration {
	t.Helper()
	clk := rigClock(w)
	now := clk.Now()
	var due time.Time
	testwait.Until(t, "the refresher's wait before a read of its own, armed on the fake clock", func() ([]time.Time, bool) {
		var armed bool
		due, armed = refresherWait(w)
		return clk.Armed(), armed
	})
	clk.Set(due)
	return due.Sub(now)
}

// refreshUntil fires each wait the refresher arms before a read of its own on w's fake clock
// (refresherWait) until done reports true, so a test waits for what the refresher's reads of its
// own bring about however many of them it takes, as one on the wall clock makes them a backoff
// apart. what names what done waits for.
func refreshUntil(t *testing.T, w *workflowRuntime, what string, done func() bool) {
	t.Helper()
	clk := rigClock(w)
	testwait.Eventually(t, what, func() bool {
		if done() {
			return true
		}
		if due, armed := refresherWait(w); armed {
			clk.Set(due)
		}
		return false
	})
}

// moveThroughSlowReads moves clk through reads that each take slow on it until done reports true.
// Each move waits until every bound in flight (a deadline one of bounds past clk's present, armed
// as a read starts) has a slow answer armed beside it, so no read is caught between arming its
// bound and its answer, and then moves clk by the shorter of slow and the bounds, which ends
// those reads, answered or cut off; a read that starts after a move starts at the moment it
// reached. what names what done waits for.
func moveThroughSlowReads(t *testing.T, clk *clocktest.Fake, slow time.Duration, bounds []time.Duration, what string, done func() bool) {
	t.Helper()
	step := slow
	for _, bound := range bounds {
		step = min(step, bound)
	}
	for {
		now := clk.Now()
		finished := false
		testwait.Until(t, what+", or every read in flight to wait on its slow answer", func() ([]time.Time, bool) {
			if done() {
				finished = true
				return nil, true
			}
			armed := clk.Armed()
			answers, bounded := 0, 0
			for _, at := range armed {
				switch {
				case at.Sub(now) == slow:
					answers++
				case slices.Contains(bounds, at.Sub(now)):
					bounded++
				}
			}
			return armed, answers > 0 && answers == bounded
		})
		if finished {
			return
		}
		clk.Set(now.Add(step))
	}
}

// served is whether w serves repo now.
func served(w *workflowRuntime, repo string) bool {
	return w.repositories.lookup(repo).refusal == ""
}

// repositoryNotes is every repository note the daemon queued on key.
func repositoryNotes(t *testing.T, w *workflowRuntime, key string) []string {
	t.Helper()
	rows, err := w.pool.Query(context.Background(), "select payload->>'body' from outbox where issue = $1 and kind = 'dispatch_message'", key)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var notes []string
	for rows.Next() {
		var note string
		if err := rows.Scan(&note); err != nil {
			t.Fatal(err)
		}
		notes = append(notes, note)
	}
	return notes
}

// A repository whose installation read failed at boot is read again of the daemon's own, with no
// mapping change, until GitHub answers; it is then served as a mapping change's read serves it,
// GitHub intake hearing it, with no restart. An issue handed to Legion on it meanwhile is not
// refused and nothing is said on it; once the repository is served, the daemon recovers it from
// Dispatch's listing and admits it on it. A served repository whose read fails stays served on its
// last answer and holds nothing else back; once GitHub answers for it too, the daemon stops
// reading again. Each read of its own is the refresher's wait on the rig's fake clock, which the
// test fires (recheck).
func TestARepositoryWhoseInstallationReadFailedIsServedOnceARetryReadsIt(t *testing.T) {
	ctx := context.Background()
	installs := &installations{}
	installs.setFailing(errors.New("read whether the implement App is installed on zeta/tools failed (403)"), "implement zeta/tools")
	w, client, installs, stream := mappingRigWith(t, installs)
	if !served(w, "acme/widgets") {
		t.Fatal("acme/widgets is not served, want it served beside the repository whose read failed")
	}
	if held := w.repositories.lookup("zeta/tools"); !held.leftOut || !held.unread || !strings.Contains(held.refusal, "the implement App's installation on zeta/tools could not be read") {
		t.Fatalf("zeta/tools refusal = %q (left out %v), want it left out for its failed read", held.refusal, held.leftOut)
	}
	for range 2 {
		recheck(t, w)
	}
	testwait.Eventually(t, "zeta/tools' installation read again", func() bool { return installs.reads("implement zeta/tools") >= 3 })
	if served(w, "zeta/tools") {
		t.Fatal("zeta/tools served while its read still fails")
	}

	engine, admission := rigHandlers(w)
	client.issue = dispatch.Issue{Key: "LEGION-301", Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: "zeta/tools", LastSeq: 1}
	client.mu.Lock()
	client.listed = []dispatch.IssueSummary{{Key: "LEGION-301", Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: "zeta/tools", LastSeq: 1}}
	client.mu.Unlock()
	fact := intake.DispatchIssue{Key: "LEGION-301", Seq: 1, Status: "todo", Title: "new", Rank: "A", HandedOver: true, ResolvedRepo: "zeta/tools"}
	prepared, err := w.prepareRepository(ctx, fact)
	if err != nil {
		t.Fatalf("prepare LEGION-301 on zeta/tools = %v, want it prepared to be applied and acknowledged", err)
	}
	if _, err := intake.ApplyFact(ctx, w.pool, "test", "todo-1", prepared, engine, admission); err != nil {
		t.Fatal(err)
	}
	if got := recordedIssue(t, w, "LEGION-301"); got != nil {
		t.Fatalf("LEGION-301 recorded as %+v while zeta/tools' read fails", got)
	}
	if notes := repositoryNotes(t, w, "LEGION-301"); len(notes) != 0 {
		t.Fatalf("notes on LEGION-301 = %q while it is held, want none", notes)
	}

	installs.setFailing(errors.New("GitHub answered 502"), "review acme/widgets")
	installs.setFailing(nil, "implement zeta/tools")
	recheck(t, w)
	testwait.Eventually(t, "zeta/tools served while acme/widgets' read fails", func() bool { return served(w, "zeta/tools") })
	if !served(w, "acme/widgets") {
		t.Fatalf("acme/widgets left out once its read failed, want it served on its last answer; refused %v", w.repositories.Refused())
	}
	if waiting := w.repositories.awaitingRead(); len(waiting) != 1 || !strings.HasPrefix(waiting[0], "acme/widgets: ") {
		t.Fatalf("awaiting a read = %q, want acme/widgets alone", waiting)
	}
	if filters := githubFilterSubjects(t, stream); !slices.Contains(filters, "notifications.github.zeta.tools.>") || !slices.Contains(filters, "notifications.github.acme.widgets.>") {
		t.Fatalf("GitHub filters = %v, want acme/widgets and zeta/tools heard", filters)
	}
	testwait.Eventually(t, "LEGION-301 admitted on zeta/tools with no one moving it", func() bool {
		got := recordedIssue(t, w, "LEGION-301")
		return got != nil && got.Repo == "zeta/tools"
	})
	if notes := repositoryNotes(t, w, "LEGION-301"); len(notes) != 0 {
		t.Fatalf("notes on LEGION-301 = %q, want none: it was left, then recovered", notes)
	}

	installs.setFailing(nil, "review acme/widgets")
	recheck(t, w)
	testwait.Eventually(t, "acme/widgets read again", func() bool { return len(w.repositories.awaitingRead()) == 0 })
	// The refresher reads again of its own only while something awaits it (refreshWaits), so with
	// nothing left it arms no wait: its reads again stop.
	if waiting := w.refreshWaits(); len(waiting) != 0 {
		t.Fatalf("awaiting a read or a recovery once GitHub answered = %q, want none, so the reads again stop", waiting)
	}
}

// A mapped repository an App is not installed on (GitHub's 404) is left out with the App named,
// and the daemon reads it again of its own only while another repository's failed read is read
// again: once that one is served, nothing is read again until a mapping change or an issue asks.
// The refusal is nothing the issue is held to, though: the next preparation of an admission on it
// asks for a read of the mapping, which reads the installation again, so an App installed since
// admits the issue with no restart and no change in Dispatch's mapping (R4).
func TestANotInstalledRepositoryIsReadAgainAtTheNextPreparation(t *testing.T) {
	ctx := context.Background()
	installs := &installations{missing: map[string]bool{"review acme/widgets": true}}
	installs.setFailing(&appauth.TransientError{Err: errors.New("GitHub answered 502")}, "review zeta/tools")
	w, client, installs, _ := mappingRigWith(t, installs)
	engine, admission := rigHandlers(w)
	if refused := readFailures(w.repositories.Refused()); len(refused) != 1 || refused[0].Repo != "zeta/tools" {
		t.Fatalf("refused for a failed read = %#v, want zeta/tools alone", refused)
	}
	installs.setFailing(nil, "review zeta/tools")
	recheck(t, w)
	testwait.Eventually(t, "zeta/tools served and recovered", func() bool { return served(w, "zeta/tools") && len(w.recoveringReasons()) == 0 })
	if held := w.repositories.lookup("acme/widgets"); !held.leftOut || held.unread || held.refusal != "the review App is not installed on acme/widgets" {
		t.Fatalf("acme/widgets refusal = %q (left out %v), want the App it lacks", held.refusal, held.leftOut)
	}
	// The refresher reads again of its own only while something awaits it (refreshWaits); a 404
	// is an answer, so with zeta/tools served nothing awaits one, and it arms no wait.
	if waiting := w.refreshWaits(); len(waiting) != 0 {
		t.Fatalf("awaiting a read or a recovery with only acme/widgets' 404 left = %q, want none, so acme/widgets is not read again of the daemon's own", waiting)
	}
	reads := installs.reads("review acme/widgets")

	issue, fact := handedOver("LEGION-311", "acme/widgets", 1)
	client.issue = issue
	installs.setMissing(false, "review acme/widgets")
	prepared, err := prepareAsDelivered(t, w, fact)
	if err != nil {
		t.Fatalf("prepare LEGION-311 once the App is installed = %v, want its preparation to read the installation again", err)
	}
	if got := installs.reads("review acme/widgets"); got <= reads {
		t.Fatalf("the preparation made %d reads of the review App's installation, want it read again", got-reads)
	}
	if _, err := intake.ApplyFact(ctx, w.pool, "test", "todo-311", prepared, engine, admission); err != nil {
		t.Fatal(err)
	}
	if got := recordedIssue(t, w, "LEGION-311"); got == nil || got.Repo != "acme/widgets" {
		t.Fatalf("LEGION-311 = %+v, want it admitted on acme/widgets with no restart and no mapping change", got)
	}
}

// A labelled todo root the boot's listing shows on a repository whose installation read failed is
// not refused: no note is said on it, and once a read again serves the repository the daemon
// recovers it from Dispatch's listing and admits it on it with no person's move. A root on a
// repository GitHub says lacks an App is refused at boot with its note, as before.
func TestABootListingsRootOnARepositoryWhoseReadFailedIsAdmittedOnceItIsServed(t *testing.T) {
	ctx := context.Background()
	installs := &installations{missing: map[string]bool{"review acme/widgets": true}}
	installs.setFailing(errors.New("read whether the implement App is installed on zeta/tools failed (403)"), "implement zeta/tools")
	w, client, installs, _ := mappingRigWith(t, installs)
	rigHandlers(w)
	w.bootID = "listing-boot"
	client.mu.Lock()
	client.listed = []dispatch.IssueSummary{
		{Key: "LEGION-401", ResolvedRepo: "zeta/tools", Title: "on the unread repository", Status: "todo", Rank: "A", HandedOver: true, LastSeq: 1},
		{Key: "LEGION-402", ResolvedRepo: "acme/widgets", Title: "on the repository missing an App", Status: "todo", Rank: "B", HandedOver: true, LastSeq: 1},
	}
	client.mu.Unlock()
	client.issue = dispatch.Issue{Key: "LEGION-401", ResolvedRepo: "zeta/tools", Title: "on the unread repository", Status: "todo", Rank: "A", HandedOver: true, LastSeq: 1}
	if err := w.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := recordedIssue(t, w, "LEGION-401"); got != nil {
		t.Fatalf("LEGION-401 recorded as %+v while zeta/tools' read fails", got)
	}
	if notes := repositoryNotes(t, w, "LEGION-401"); len(notes) != 0 {
		t.Fatalf("notes on LEGION-401 = %q while it is held, want none", notes)
	}
	if notes := repositoryNotes(t, w, "LEGION-402"); len(notes) != 1 || !strings.Contains(notes[0], "the review App is not installed on acme/widgets") {
		t.Fatalf("notes on LEGION-402 = %q, want its refusal naming the App acme/widgets lacks", notes)
	}

	installs.setFailing(nil, "implement zeta/tools")
	recheck(t, w)
	testwait.Eventually(t, "LEGION-401 admitted on zeta/tools and zeta/tools recovered", func() bool {
		got := recordedIssue(t, w, "LEGION-401")
		return got != nil && got.Repo == "zeta/tools" && len(w.recoveringReasons()) == 0
	})
	if notes := repositoryNotes(t, w, "LEGION-401"); len(notes) != 0 {
		t.Fatalf("notes on LEGION-401 = %q, want none: it was held, then admitted", notes)
	}
	if got := recordedIssue(t, w, "LEGION-402"); got != nil {
		t.Fatalf("LEGION-402 recorded as %+v, want it left refused", got)
	}
}

// A live admission event of an issue on a repository whose installation read failed is
// acknowledged, not left on the stream: the Dispatch consumer reaches the position the boot's
// admission waits for, so a root on a served repository, which the boot's listing held for that
// position, is admitted while the failing repository still fails, and the issue is recovered once
// GitHub answers for its repository, with no note on it. Left on the stream, the event kept the
// consumer's acknowledgement floor below the boot's target, and no root of the boot's listing was
// admitted on any repository until the failing one healed.
func TestAnUnreadEventIsAcknowledgedSoTheBootAdmitsRootsOnServedRepositories(t *testing.T) {
	ctx := context.Background()
	installs := &installations{}
	installs.setFailing(errors.New("read whether the implement App is installed on zeta/tools failed (403)"), "implement zeta/tools")
	w, client, installs, _, js := mappingRigJS(t, installs)
	engine, admission := rigHandlers(w)
	w.bootID = "held-event-boot"
	passes := triggeringRuntime(w)
	failing, served := dispatch.Issue{Key: "LEGION-501", ResolvedRepo: "zeta/tools", Title: "LEGION-501", Status: "todo", Rank: "A", HandedOver: true, LastSeq: 1},
		dispatch.Issue{Key: "LEGION-502", ResolvedRepo: "acme/widgets", Title: "LEGION-502", Status: "todo", Rank: "B", HandedOver: true, LastSeq: 1}
	client.mu.Lock()
	client.issues = map[string]dispatch.Issue{failing.Key: failing, served.Key: served}
	client.listed = []dispatch.IssueSummary{
		{Key: failing.Key, ResolvedRepo: failing.ResolvedRepo, Title: failing.Title, Status: "todo", Rank: "A", HandedOver: true, LastSeq: 1},
		{Key: served.Key, ResolvedRepo: served.ResolvedRepo, Title: served.Title, Status: "todo", Rank: "B", HandedOver: true, LastSeq: 1},
	}
	client.mu.Unlock()
	repo := "zeta/tools"
	if _, err := js.Publish(ctx, "notifications.dispatch.issue.LEGION-501.issue.updated",
		retainedIssueEnvelope(t, failing.Key, 501, 1, "todo", map[string]string{"kind": "user", "id": "a-person"}, &repo)); err != nil {
		t.Fatal(err)
	}
	if err := w.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !admission.Held() {
		t.Fatal("the boot's admission holds nothing for the consumer, want the listing held for the event still on the stream")
	}
	run, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- w.consumers.Run(run, engine, admission) }()
	polled := make(chan struct{})
	go func() { w.pollHoldRelease(run); close(polled) }()
	t.Cleanup(func() { cancel(); <-done; <-polled })

	passes.until(t, passHoldRelease, served.Key+" on served acme/widgets admitted while zeta/tools' read fails", func() bool {
		return recordedIssue(t, w, served.Key) != nil
	})
	if got := recordedIssue(t, w, failing.Key); got != nil {
		t.Fatalf("%s recorded as %+v while zeta/tools' read fails", failing.Key, got)
	}
	if notes := repositoryNotes(t, w, failing.Key); len(notes) != 0 {
		t.Fatalf("notes on %s = %q while it is held, want none", failing.Key, notes)
	}

	installs.setFailing(nil, "implement zeta/tools")
	recheck(t, w)
	testwait.Eventually(t, failing.Key+" admitted on zeta/tools", func() bool {
		got := recordedIssue(t, w, failing.Key)
		return got != nil && got.Repo == "zeta/tools"
	})
}

// A served repository kept on its last answer, its installation read failing, is kept with the
// name GitHub last gave it and asks GitHub nothing more: its name read failing too, once the read
// it holds is old, fails no read of the mapping, so another repository whose installation read
// fails at boot is still served once GitHub answers for it.
func TestAServedRepositoryKeptOnItsLastAnswerHoldsNoOtherRepositoryBack(t *testing.T) {
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	github := &failingGitHub{named: map[string]string{"acme/widgets": "acme/widgets@main", "zeta/tools": "zeta/tools@main"}, only: "/acme/widgets"}
	url := github.start(t)
	w, _, installs, _ := mappingRigWith(t, installs, func(w *workflowRuntime) {
		// Every read of a repository's name asks GitHub again, as one older than its TTL does.
		w.defaultBranches = newDefaultBranches(roleTokens{}, w.repositories, url, 0, time.Now)
	})
	if !served(w, "acme/widgets") || served(w, "zeta/tools") {
		t.Fatalf("served %v at boot, want acme/widgets alone", w.repositories.Accepted())
	}
	github.fail.Store(true)
	installs.setFailing(errors.New("GitHub answered 502"), "review acme/widgets")
	installs.setFailing(nil, "implement zeta/tools")
	recheck(t, w)
	testwait.Until(t, "zeta/tools served while acme/widgets' reads fail (the reading is zeta/tools' installation reads so far)", func() (int, bool) {
		return installs.reads("implement zeta/tools"), served(w, "zeta/tools")
	})
	if !served(w, "acme/widgets") {
		t.Fatalf("acme/widgets left out, want it kept on its last answer; refused %v", w.repositories.Refused())
	}
	if resolved, err := w.ResolveRepository("acme/widgets"); err != nil || resolved.Repo != "acme/widgets" || resolved.DefaultBranch != "main" {
		t.Fatalf("ResolveRepository(acme/widgets) = %#v, %v; want its last read, acme/widgets@main", resolved, err)
	}
	// An admission on the kept repository reads no default branch from GitHub: it takes the last
	// read, so it waits for no read that is failing.
	if _, err := w.prepareUnrecorded(context.Background(), intake.DispatchIssue{Key: "LEGION-801", Seq: 1, Status: "todo", Title: "kept", Rank: "A", HandedOver: true, ResolvedRepo: "acme/widgets"}, false, 0, false); err != nil {
		t.Fatalf("prepare an admission on kept acme/widgets = %v, want its last read with no GitHub read", err)
	}
}

// A repository's recovery records its children parents first, as the boot listing does: a child
// under a child of a live tree, both on a repository whose read failed, is recorded under the tree
// once GitHub answers, whatever order their keys sort in (LEGION-100 sorts before LEGION-99) or
// the listing's: the workflow records a child only under a recorded parent.
func TestARecoveryRecordsChildrenParentsFirst(t *testing.T) {
	ctx := context.Background()
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	w, client, installs, _ := mappingRigWith(t, installs)
	engine, admission := rigHandlers(w)
	w.bootID = "parents-first-boot"
	root := dispatch.Issue{Key: "LEGION-98", ResolvedRepo: "acme/widgets", Title: "root", Status: "todo", Rank: "A", HandedOver: true, LastSeq: 1}
	parent := dispatch.Issue{Key: "LEGION-99", ResolvedRepo: "zeta/tools", Title: "parent", Status: "todo", Rank: "B", Parent: new(root.Key), LastSeq: 1}
	child := dispatch.Issue{Key: "LEGION-100", ResolvedRepo: "zeta/tools", Title: "child", Status: "todo", Rank: "C", Parent: new(parent.Key), LastSeq: 1}
	client.mu.Lock()
	client.issues = map[string]dispatch.Issue{root.Key: root, parent.Key: parent, child.Key: child}
	client.mu.Unlock()
	prepared, err := prepareAsDelivered(t, w, intake.DispatchIssue{Key: root.Key, Seq: 1, Status: "todo", Title: "root", Rank: "A", HandedOver: true, ResolvedRepo: "acme/widgets"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := intake.ApplyFact(ctx, w.pool, "test", "root-todo", prepared, engine, admission); err != nil {
		t.Fatal(err)
	}
	if got := recordedIssue(t, w, root.Key); got == nil {
		t.Fatalf("%s not admitted on acme/widgets", root.Key)
	}
	client.mu.Lock()
	client.listed = []dispatch.IssueSummary{
		{Key: child.Key, ResolvedRepo: "zeta/tools", Title: "child", Status: "todo", Rank: "C", Parent: child.Parent, LastSeq: 1},
		{Key: parent.Key, ResolvedRepo: "zeta/tools", Title: "parent", Status: "todo", Rank: "B", Parent: parent.Parent, LastSeq: 1},
	}
	client.mu.Unlock()
	if err := w.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for _, key := range []string{parent.Key, child.Key} {
		if got := recordedIssue(t, w, key); got != nil {
			t.Fatalf("%s recorded as %+v while zeta/tools' read fails", key, got)
		}
	}

	installs.setFailing(nil, "implement zeta/tools")
	recheck(t, w)
	testwait.Until(t, child.Key+" recorded in "+root.Key+"'s tree once zeta/tools is served (the reading is "+parent.Key+" and "+child.Key+")", func() ([]*record.Issue, bool) {
		got := recordedIssue(t, w, child.Key)
		return []*record.Issue{recordedIssue(t, w, parent.Key), got}, got != nil && got.Tree == root.Key
	})
	if got := recordedIssue(t, w, parent.Key); got == nil || got.Tree != root.Key {
		t.Fatalf("%s = %+v, want it recorded in %s's tree", parent.Key, got, root.Key)
	}
}

// A GitHub that answers each installation read slowly, but within installReadAttempt, is answered
// for on a read of the mapping too, however long the whole check takes: only a read longer than
// installReadAttempt is a failed read. A refresh bounded as a whole by appMintAttempt cut such a
// check off, so a repository whose read failed once was never served. Here Dispatch's listing and
// each installation read take two thirds of a bound on the rig's fake clock, so each answers within
// its own bound and the refresh, the listing and then the reads, outlasts appMintAttempt.
func TestASlowButAnsweringGitHubIsServedOnARefresh(t *testing.T) {
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	var listing *slowListing
	w, _, installs, _ := mappingRigWith(t, installs, func(w *workflowRuntime) {
		listing = &slowListing{Client: w.dispatch, clock: w.clock}
		w.dispatch = listing
	})
	if served(w, "zeta/tools") {
		t.Fatal("zeta/tools served at boot while its read fails")
	}
	clk := rigClock(w)
	slow := installReadAttempt * 2 / 3
	listing.slow.Store(int64(slow))
	installs.mu.Lock()
	installs.delay, installs.clock = slow, clk
	installs.mu.Unlock()
	installs.setFailing(nil, "implement zeta/tools")
	recheck(t, w)
	start := clk.Now()
	moveThroughSlowReads(t, clk, slow, []time.Duration{appMintAttempt, installReadAttempt}, "zeta/tools served, or the refresher waiting for its next read", func() bool {
		_, waiting := refresherWait(w)
		return served(w, "zeta/tools") || waiting
	})
	if !served(w, "zeta/tools") {
		t.Fatalf("zeta/tools not served after %d slow reads, want it served once GitHub answers", installs.reads("implement zeta/tools"))
	}
	if took := clk.Now().Sub(start); took <= appMintAttempt {
		t.Fatalf("the refresh took %s of the fake clock, want longer than appMintAttempt (%s), so a bound on the refresh as a whole would have cut it", took, appMintAttempt)
	}
}

// slowListing is a Dispatch whose repository mapping answers slow later on clock, once slow is
// set: a Dispatch that answers slowly.
type slowListing struct {
	dispatch.Client
	clock clock.Clock
	slow  atomic.Int64
}

func (d *slowListing) ListRepoProjects(ctx context.Context) ([]dispatch.RepoProject, error) {
	if slow := time.Duration(d.slow.Load()); slow > 0 {
		if err := d.clock.Sleep(ctx, slow); err != nil {
			return nil, err
		}
	}
	return d.Client.ListRepoProjects(ctx)
}

// applyTodo prepares and applies key's labelled todo event at seq on repo, as the Dispatch
// consumer does.
func applyTodo(t *testing.T, w *workflowRuntime, engine *workflow.Engine, admission *admit.Admission, key, repo string, seq int64) {
	t.Helper()
	fact := intake.DispatchIssue{Key: key, Seq: seq, Status: "todo", Title: key, Rank: "A", HandedOver: true, ResolvedRepo: repo}
	prepared, err := prepareAsDelivered(t, w, fact)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := intake.ApplyFact(context.Background(), w.pool, "test", fmt.Sprintf("%s-todo-%d", key, seq), prepared, engine, admission); err != nil {
		t.Fatal(err)
	}
}

// show makes Dispatch show each of issues as it is, in its issue detail and in the project's
// listing, in place of what it showed of the same key.
func (d *mappingDispatch) show(issues ...dispatch.Issue) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.issues == nil {
		d.issues = map[string]dispatch.Issue{}
	}
	for _, issue := range issues {
		d.issues[issue.Key] = issue
		summary := dispatch.IssueSummary{Key: issue.Key, Title: issue.Title, Status: issue.Status, Parent: issue.Parent, Rank: issue.Rank,
			HandedOver: issue.HandedOver, LastSeq: issue.LastSeq, ResolvedRepo: issue.ResolvedRepo}
		d.listed = slices.DeleteFunc(d.listed, func(listed dispatch.IssueSummary) bool { return listed.Key == issue.Key })
		d.listed = append(d.listed, summary)
	}
}

// deliver shows issue in Dispatch and delivers its event, at its sequence, as the Dispatch consumer
// prepares and applies one through w's handlers.
func deliver(t *testing.T, w *workflowRuntime, client *mappingDispatch, issue dispatch.Issue) {
	t.Helper()
	client.show(issue)
	fact := intake.DispatchIssue{Key: issue.Key, Seq: issue.LastSeq, Type: "issue.updated", Status: issue.Status, Title: issue.Title, Rank: issue.Rank,
		Parent: parentKey(issue.Parent), HandedOver: issue.HandedOver, ResolvedRepo: issue.ResolvedRepo}
	prepared, err := prepareAsDelivered(t, w, fact)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := intake.ApplyFact(context.Background(), w.pool, "test", fmt.Sprintf("%s-%s-%d", issue.Key, issue.Status, issue.LastSeq), prepared, w.handlers...); err != nil {
		t.Fatal(err)
	}
}

// issue is a Dispatch issue key on repo in status, labelled for Legion when handed, under parent
// ("" for none), at sequence seq.
func issue(key, repo, status string, handed bool, parent string, seq int64) dispatch.Issue {
	shown := dispatch.Issue{Key: key, ResolvedRepo: repo, Title: key, Status: status, Rank: "A" + key, HandedOver: handed, LastSeq: seq}
	if parent != "" {
		shown.Parent = new(parent)
	}
	return shown
}

// liveRoot admits key as a labelled todo root on served acme/widgets, so its tree is live.
func liveRoot(t *testing.T, w *workflowRuntime, client *mappingDispatch, key string) {
	t.Helper()
	deliver(t, w, client, issue(key, "acme/widgets", "todo", true, "", 1))
	if recordedIssue(t, w, key) == nil {
		t.Fatalf("%s not admitted on acme/widgets", key)
	}
}

// rowCount is the number of rows query counts in w's database.
func rowCount(t *testing.T, w *workflowRuntime, query string) int {
	t.Helper()
	var n int
	if err := w.pool.QueryRow(context.Background(), query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// serveAndRecover lets GitHub answer zeta/tools' implement App read again, fires the refresher's
// wait before its next read of its own (recheck), and waits until the daemon serves zeta/tools and
// has nothing left to recover.
func serveAndRecover(t *testing.T, w *workflowRuntime, installs *installations) {
	t.Helper()
	installs.setFailing(nil, "implement zeta/tools")
	recheck(t, w)
	testwait.Eventually(t, "zeta/tools served and recovered", func() bool {
		return served(w, "zeta/tools") && len(w.recoveringReasons()) == 0
	})
}

// idle fails t unless the refresher has nothing to read again of its own: nothing it arms a wait
// for a read of its own for (refreshWaits) and no event held for a read of the mapping. With
// none, nothing more is read, listed or applied until a mapping change or an event asks.
func idle(t *testing.T, w *workflowRuntime) {
	t.Helper()
	if waiting := w.refreshWaits(); len(waiting) != 0 {
		t.Fatalf("awaiting a read or a recovery = %q, want none, so the refresher reads nothing more of its own", waiting)
	}
	w.awaitingMu.Lock()
	held := len(w.awaiting)
	w.awaitingMu.Unlock()
	if held != 0 {
		t.Fatalf("%d events held for a read of the mapping, want none", held)
	}
}

// A repository whose installation read fails at boot is recovered once GitHub answers for it: the
// boot's listing admits a root on a served repository while it fails, and once it is served the
// daemon takes the issues on it from Dispatch's listing once, as the boot would have had it been
// served: a labelled root admitted, a child of a live tree and that child's own child recorded
// under the tree, and a person's sub-issue under a person's epic left as it is. After that the
// daemon reads nothing more and applies nothing more, and tells the controller nothing. The
// per-issue hold kept the person's sub-issue held for good, reading and applying it at every
// read of the mapping.
func TestARepositoryUnreadAtBootIsRecoveredOnceServed(t *testing.T) {
	ctx := context.Background()
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	w, client, installs, _ := mappingRigWith(t, installs)
	rigHandlers(w)
	w.bootID = "recovery-boot"
	const tree, root, child, grandchild, epic, sub = "LEGION-1000", "LEGION-1001", "LEGION-1002", "LEGION-1003", "LEGION-1004", "LEGION-1005"
	client.show(
		issue(tree, "acme/widgets", "todo", true, "", 1),
		issue(root, "zeta/tools", "todo", true, "", 1),
		issue(child, "zeta/tools", "todo", false, tree, 1),
		issue(grandchild, "zeta/tools", "todo", false, child, 1),
		issue(epic, "zeta/tools", "in_progress", false, "", 1),
		issue(sub, "zeta/tools", "todo", false, epic, 1),
	)
	if err := w.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if recordedIssue(t, w, tree) == nil {
		t.Fatalf("%s on served acme/widgets not admitted while zeta/tools' read fails", tree)
	}
	for _, key := range []string{root, child, grandchild, epic, sub} {
		if got := recordedIssue(t, w, key); got != nil {
			t.Fatalf("%s recorded as %+v while zeta/tools' read fails", key, got)
		}
	}
	notices := rowCount(t, w, "select count(*) from outbox where kind = 'controller_notice'")

	serveAndRecover(t, w, installs)
	if recordedIssue(t, w, root) == nil {
		t.Fatalf("%s, a labelled root on zeta/tools, not admitted once zeta/tools is served", root)
	}
	for _, key := range []string{child, grandchild} {
		if got := recordedIssue(t, w, key); got == nil || got.Tree != tree {
			t.Fatalf("%s = %+v once zeta/tools is served, want it recorded in %s's tree", key, got, tree)
		}
	}
	for _, key := range []string{epic, sub} {
		if got := recordedIssue(t, w, key); got != nil {
			t.Fatalf("%s, a person's issue, recorded as %+v, want it left as it is", key, got)
		}
	}
	idle(t, w)
	if got := rowCount(t, w, "select count(*) from outbox where kind = 'controller_notice'"); got != notices {
		t.Fatalf("%d controller notices queued by the recovery, want none", got-notices)
	}
}

// Events applied while a repository's read fails say nothing and keep nothing, and the recovery
// once it is served ends each issue where its events would have, had it been served: a root handed
// over during the outage is admitted; one moved back to backlog during it is not, and its later
// move to todo admits it once; one whose event was prepared while GitHub said an App was missing
// and applied after the read failed is admitted too.
func TestEventsDuringAnOutageEndWhereTheyWouldHaveServed(t *testing.T) {
	installs := &installations{missing: map[string]bool{"review zeta/tools": true}}
	w, client, installs, _ := mappingRigWith(t, installs)
	rigHandlers(w)
	const handed, withdrawn, prepared404 = "LEGION-1100", "LEGION-1101", "LEGION-1102"
	z := issue(prepared404, "zeta/tools", "todo", true, "", 1)
	client.show(z)
	prepared, err := prepareAsDelivered(t, w, intake.DispatchIssue{Key: z.Key, Seq: 1, Status: "todo", Title: z.Title, Rank: z.Rank, HandedOver: true, ResolvedRepo: "zeta/tools"})
	if err != nil {
		t.Fatal(err)
	}
	installs.mu.Lock()
	delete(installs.missing, "review zeta/tools")
	installs.mu.Unlock()
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	read := w.requestMappingRead()
	testwait.Eventually(t, "the read that finds zeta/tools unread", func() bool { return w.mappingApplied.Load() >= read })
	if !w.repositories.lookup("zeta/tools").unread {
		t.Fatal("zeta/tools is not unread after the read whose installation read failed")
	}
	if _, err := intake.ApplyFact(context.Background(), w.pool, "test", z.Key+"-todo-1", prepared, w.handlers...); err != nil {
		t.Fatal(err)
	}
	deliver(t, w, client, issue(handed, "zeta/tools", "todo", true, "", 1))
	deliver(t, w, client, issue(withdrawn, "zeta/tools", "todo", true, "", 1))
	deliver(t, w, client, issue(withdrawn, "zeta/tools", "backlog", true, "", 2))
	for _, key := range []string{handed, withdrawn, prepared404} {
		if got := recordedIssue(t, w, key); got != nil {
			t.Fatalf("%s recorded as %+v while zeta/tools' read fails", key, got)
		}
	}

	serveAndRecover(t, w, installs)
	for _, key := range []string{handed, prepared404} {
		if recordedIssue(t, w, key) == nil {
			t.Fatalf("%s not admitted once zeta/tools is served", key)
		}
	}
	if got := recordedIssue(t, w, withdrawn); got != nil {
		t.Fatalf("%s, moved to backlog during the outage, recorded as %+v", withdrawn, got)
	}
	deliver(t, w, client, issue(withdrawn, "zeta/tools", "todo", true, "", 3))
	if got := recordedIssue(t, w, withdrawn); got == nil || got.LastDispatchSeq != 3 {
		t.Fatalf("%s = %+v after its move to todo, want it admitted at that move", withdrawn, got)
	}
	for _, key := range []string{handed, withdrawn, prepared404} {
		if notes := repositoryNotes(t, w, key); len(notes) != 0 {
			t.Fatalf("notes on %s = %q, want none", key, notes)
		}
	}
}

// A child on a served repository under a parent on a repository whose read fails, the parent a todo
// child of a live tree, is recovered with its parent: the child's own event recorded nothing, its
// parent then unrecorded, and the recovery records both, parent first. The per-issue hold held
// only the parent, and the child, already todo in Dispatch, was never recorded.
func TestAChildOnAServedRepositoryUnderAnUnreadParentIsRecovered(t *testing.T) {
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	w, client, installs, _ := mappingRigWith(t, installs)
	rigHandlers(w)
	const root, parent, child = "LEGION-1200", "LEGION-1201", "LEGION-1202"
	liveRoot(t, w, client, root)
	deliver(t, w, client, issue(parent, "zeta/tools", "todo", false, root, 1))
	deliver(t, w, client, issue(child, "acme/widgets", "todo", false, parent, 1))
	for _, key := range []string{parent, child} {
		if got := recordedIssue(t, w, key); got != nil {
			t.Fatalf("%s recorded as %+v while zeta/tools' read fails", key, got)
		}
	}
	serveAndRecover(t, w, installs)
	for _, key := range []string{parent, child} {
		if got := recordedIssue(t, w, key); got == nil || got.Tree != root {
			t.Fatalf("%s = %+v once zeta/tools is served, want it recorded in %s's tree", key, got, root)
		}
	}
}

// A recovery whose read of Dispatch's listing fails stays to recover, and is tried again on the
// refresher's backoff: the next listing records the issue, and none is read after it.
func TestARecoveryWhoseListingFailsIsTriedAgain(t *testing.T) {
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	w, client, installs, _ := mappingRigWith(t, installs)
	rigHandlers(w)
	const key = "LEGION-1300"
	deliver(t, w, client, issue(key, "zeta/tools", "todo", true, "", 1))
	client.mu.Lock()
	client.listFailures = 1
	client.mu.Unlock()
	installs.setFailing(nil, "implement zeta/tools")
	// The first read serves zeta/tools and its recovery's listing fails; the refresher's next
	// wait runs the recovery again.
	recheck(t, w)
	recheck(t, w)
	testwait.Eventually(t, "zeta/tools served and recovered", func() bool {
		return served(w, "zeta/tools") && len(w.recoveringReasons()) == 0
	})
	if recordedIssue(t, w, key) == nil {
		t.Fatalf("%s not admitted by the recovery's second listing", key)
	}
	idle(t, w)
	if got := client.listingCount(); got != 2 {
		t.Fatalf("Dispatch listed %d times, want two: the failed listing and the one that recovered", got)
	}
}

// A recovery while the Dispatch consumer is behind the stream holds its listing as the boot's is,
// and admits the root once the consumer catches up, once; a second recovery in the same boot, on a
// stream the consumer has caught up with, admits at once.
func TestARecoveryWhileTheConsumerIsBehindAdmitsOnceItCatchesUp(t *testing.T) {
	ctx := context.Background()
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	w, client, installs, _, js := mappingRigJS(t, installs)
	engine, admission := rigHandlers(w)
	w.bootID = "behind-boot"
	passes := triggeringRuntime(w)
	const first, second, unrelated = "LEGION-1400", "LEGION-1401", "LEGION-1499"
	client.show(issue(first, "zeta/tools", "todo", true, "", 1), issue(unrelated, "acme/widgets", "backlog", false, "", 1))
	repo := "acme/widgets"
	if _, err := js.Publish(ctx, "notifications.dispatch.issue."+unrelated+".issue.updated",
		retainedIssueEnvelope(t, unrelated, 1499, 1, "backlog", map[string]string{"kind": "user", "id": "a-person"}, &repo)); err != nil {
		t.Fatal(err)
	}
	serveAndRecover(t, w, installs)
	if !admission.Held() || recordedIssue(t, w, first) != nil {
		t.Fatalf("admission held %t, %s recorded %t; want the recovery held for the consumer behind the stream", admission.Held(), first, recordedIssue(t, w, first) != nil)
	}
	run, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- w.consumers.Run(run, engine, admission) }()
	polled := make(chan struct{})
	go func() { w.pollHoldRelease(run); close(polled) }()
	t.Cleanup(func() { cancel(); <-done; <-polled })
	passes.until(t, passHoldRelease, first+" admitted once the consumer caught up", func() bool { return recordedIssue(t, w, first) != nil })
	if n := rowCount(t, w, "select count(*) from slots where issue = '"+first+"'"); n != 1 {
		t.Fatalf("%s holds %d slots, want one", first, n)
	}

	// A served repository whose read fails is kept on its last answer, so the second outage is
	// zeta/tools mapped again after a person unmapped it, while GitHub fails its read.
	readMappingNow := func(what string, done func() bool) {
		t.Helper()
		read := w.requestMappingRead()
		testwait.Eventually(t, what, func() bool { return w.mappingApplied.Load() >= read && done() })
	}
	client.set(dispatch.RepoProject{Repo: "acme/widgets", Project: "LEGION", Default: true})
	readMappingNow("zeta/tools unmapped", func() bool {
		held := w.repositories.lookup("zeta/tools")
		return held.refusal != "" && !held.leftOut
	})
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	client.set(dispatch.RepoProject{Repo: "acme/widgets", Project: "LEGION", Default: true}, dispatch.RepoProject{Repo: "zeta/tools", Project: "LEGION"})
	readMappingNow("zeta/tools mapped again and unread", func() bool { return w.repositories.lookup("zeta/tools").unread })
	client.show(issue(second, "zeta/tools", "todo", true, "", 1))
	serveAndRecover(t, w, installs)
	testwait.Eventually(t, second+" admitted by the second recovery", func() bool { return recordedIssue(t, w, second) != nil })
}

// The refresher reads the mapping again of its own while a repository's installation read fails, 30
// s doubling to 10 min, and no more; once nothing waits it reads nothing of its own, and the next
// failed read starts again at 30 s. Each wait is armed on the rig's fake clock, which the test
// moves past it (recheck).
func TestTheUnreadRepositoryBackoffDoublesToItsCapAndResets(t *testing.T) {
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	w, _, installs, _ := mappingRigWith(t, installs)
	rigHandlers(w)
	var waits []time.Duration
	for range 8 {
		waits = append(waits, recheck(t, w))
	}
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute, 10 * time.Minute}
	if !slices.Equal(waits, want) {
		t.Fatalf("waits = %v, want %v", waits, want)
	}

	installs.setFailing(nil, "implement zeta/tools")
	w.requestMappingRead()
	testwait.Eventually(t, "zeta/tools served and recovered", func() bool { return served(w, "zeta/tools") && len(w.recoveringReasons()) == 0 })
	idle(t, w)
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	w.requestMappingRead()
	if got := recheck(t, w); got != installReadRetry.Initial {
		t.Fatalf("the first wait after the read failed again = %s, want %s", got, installReadRetry.Initial)
	}
}

// treeNotices is the payload, as JSON text, of every notice queued on tree, in queue order.
func treeNotices(t *testing.T, w *workflowRuntime, tree string) []string {
	t.Helper()
	rows, err := w.pool.Query(context.Background(), "select payload::text from outbox where issue = $1 and kind = 'notice' order by id", tree)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var notices []string
	for rows.Next() {
		var notice string
		if err := rows.Scan(&notice); err != nil {
			t.Fatal(err)
		}
		notices = append(notices, notice)
	}
	return notices
}

// A labelled todo root whose event the daemon applied silently while its repository was unread is
// recovered even when Dispatch resolves it to another repository by the time GitHub answers (it
// took its repository from the project's default, or from a parent, and that changed, with no
// event of its own): the event's silence is noted with the repository it was silent for, and that
// repository's recovery takes the issue in whatever it resolves to. Scoped by its resolution at
// the recovery alone, it waited for the next boot.
func TestAnIssueSilentForAnUnreadRepositoryIsRecoveredWhereverItResolvesThen(t *testing.T) {
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	w, client, installs, _ := mappingRigWith(t, installs)
	const key = "LEGION-1500"
	deliver(t, w, client, issue(key, "zeta/tools", "todo", true, "", 1))
	if recordedIssue(t, w, key) != nil {
		t.Fatalf("%s recorded while zeta/tools' read fails", key)
	}
	// The project's default moves to acme/widgets: Dispatch resolves the issue there now, and sends
	// no event of it.
	moved := issue(key, "acme/widgets", "todo", true, "", 1)
	client.show(moved)
	serveAndRecover(t, w, installs)
	if got := recordedIssue(t, w, key); got == nil || got.Repo != "acme/widgets" {
		t.Fatalf("%s = %+v once zeta/tools is recovered, want it admitted on acme/widgets, where it resolves now", key, got)
	}
}

// A repository's recovery tells the architect that owns each child's parent, else the tree root's
// architect, once, which children a release_children left for the repository the recovery still
// did not record, with what to release: here C, todo under a backlog parent P, which the recovery
// records no child under. A child the recovery recorded, E, is not named. Before, such a child
// stranded silently.
func TestARecoveryTellsTheArchitectOfTheChildrenItDidNotRecord(t *testing.T) {
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	w, client, installs, _ := mappingRigWith(t, installs)
	const tree, p, c, e = "LEGION-1600", "LEGION-1601", "LEGION-1602", "LEGION-1603"
	liveRoot(t, w, client, tree)
	client.show(issue(p, "zeta/tools", "backlog", false, tree, 1), issue(c, "zeta/tools", "todo", false, p, 1), issue(e, "zeta/tools", "todo", false, tree, 1))
	w.awaitRecovery("zeta/tools", tree, c, p)
	w.awaitRecovery("zeta/tools", tree, e, tree)
	before := len(treeNotices(t, w, tree))
	serveAndRecover(t, w, installs)
	if got := recordedIssue(t, w, e); got == nil || got.Tree != tree {
		t.Fatalf("%s = %+v, want the recovery to record it in %s's tree", e, got, tree)
	}
	if got := recordedIssue(t, w, c); got != nil {
		t.Fatalf("%s recorded as %+v under backlog %s", c, got, p)
	}
	notices := treeNotices(t, w, tree)[before:]
	if len(notices) != 1 || !strings.Contains(notices[0], "child-release-stalled") ||
		!strings.Contains(notices[0], c+" (its parent "+p+" is not recorded: release "+p+" first, then "+c+")") || strings.Contains(notices[0], e) {
		t.Fatalf("notices to %s's architect after the recovery = %q, want one naming %s and %s, not %s", tree, notices, c, p, e)
	}
	idle(t, w)
	if got := treeNotices(t, w, tree)[before:]; len(got) != 1 {
		t.Fatalf("notices after the recovery = %d, want the one only", len(got))
	}
}

// A child release_children left for unread zeta/tools under a parent todo on served acme/widgets
// that the daemon had not recorded yet is recorded with its parent once both happen: the parent's
// own event records the parent, the recovery records the child, and the architect is told
// nothing, since nothing is left unrecorded.
func TestARecoveryTellsNothingOnceAChildUnderALaggingTodoParentIsRecorded(t *testing.T) {
	installs := &installations{}
	installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	w, client, installs, _ := mappingRigWith(t, installs)
	const tree, parent, child = "LEGION-1700", "LEGION-1701", "LEGION-1702"
	liveRoot(t, w, client, tree)
	client.show(issue(child, "zeta/tools", "todo", false, parent, 1))
	w.awaitRecovery("zeta/tools", tree, child, parent)
	deliver(t, w, client, issue(parent, "acme/widgets", "todo", false, tree, 1))
	before := len(treeNotices(t, w, tree))
	serveAndRecover(t, w, installs)
	for _, key := range []string{parent, child} {
		if got := recordedIssue(t, w, key); got == nil || got.Tree != tree {
			t.Fatalf("%s = %+v once zeta/tools is recovered, want it recorded in %s's tree", key, got, tree)
		}
	}
	idle(t, w)
	if got := treeNotices(t, w, tree)[before:]; len(got) != 0 {
		t.Fatalf("notices after the recovery = %q, want none: nothing is left unrecorded", got)
	}
}

// childEnvelope is the Dispatch todo event of key, a child of parent on repo, at seq, as the
// Dispatch consumer reads it off the stream.
func childEnvelope(t *testing.T, key, parent, repo string, id, seq int64) []byte {
	t.Helper()
	issue := map[string]any{"key": key, "status": "todo", "title": key, "rank": "A", "parent": parent, "labels": []string{}, "resolved_repo": repo}
	event, err := json.Marshal(map[string]any{"id": id, "issue_key": key, "seq": seq, "type": "issue.updated", "actor": map[string]string{"kind": "user", "id": "a-person"}, "notify": true, "payload": issue})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(map[string]any{"event_id": fmt.Sprintf("dispatch-%s-%d", key, seq), "source": "dispatch", "source_event_id": fmt.Sprint(id), "topic": "issue",
		"dedupe_key": fmt.Sprintf("dispatch-%s-%d", key, seq), "issued_at": 1, "payload_summary": "issue updated", "payload": string(event), "trace_id": "child"})
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

// A recovery whose listing stays held, here because the todo parent's own event waits on a paused
// Dispatch consumer and keeps it below the stream's last position, tells the architect of the child
// its release left under that parent once the hold has lasted recoveryHoldBound, and only once;
// when the consumer runs, the parent's event records the parent, the hold releases, the child is
// recorded, and nothing more is told. Before, the notice waited for the hold to release, so while
// the hold lasted the child was neither recorded nor named.
func TestARecoveryHeldPastItsBoundTellsItsReleaseWaitsOnce(t *testing.T) {
	h := newHeldRecovery(t)
	before := h.notices(t)
	h.behind(t, heldParent)
	held := h.serve(t)
	h.rechecksToTheBound(t, held, before, nil)
	testwait.Eventually(t, "the release wait told at the bound", func() bool { return h.notices(t) > before })
	notices := treeNotices(t, h.w, heldTree)[before:]
	if len(notices) != 1 || !strings.Contains(notices[0], heldChild+" (its parent "+heldParent+" is not recorded: release "+heldParent+" first, then "+heldChild+")") {
		t.Fatalf("notices once the hold outlasted its bound = %q, want one naming %s and %s", notices, heldChild, heldParent)
	}
	h.runAfterTheBound(t)
	if n := h.notices(t) - before; n != 1 || !h.admission.Held() {
		t.Fatalf("notices = %d with the hold still %t after a run past the bound, want the one only while the hold lasts", n, h.admission.Held())
	}

	h.consume()
	h.passes.until(t, passHoldRelease, "parent and child recorded once the consumer catches up", func() bool {
		p, c := recordedIssue(t, h.w, heldParent), recordedIssue(t, h.w, heldChild)
		return p != nil && p.Tree == heldTree && c != nil && c.Tree == heldTree
	})
	idle(t, h.w)
	if n := h.notices(t) - before; n != 1 {
		t.Fatalf("notices after the hold released = %d, want the one only", n)
	}
}

// A repository a pass would let go while the recovery's listing is held is put back, its release
// waits untold, only until the hold has lasted recoveryHoldBound: then its architect is told, the
// hold still standing, on the one clock the recovery's own waits are told on. Put back for as long
// as the hold stood, a hold that never releases kept those waits untold, and the repository listed
// again on the backoff, for the life of the daemon. The bound is the rig's fake clock's: no run
// before it tells the waits, and the refresher cuts its wait to end at it (rechecksToTheBound).
func TestALetGoHeldPastItsBoundTellsItsReleaseWaits(t *testing.T) {
	h := newHeldRecovery(t)
	const let, stranger = "LEGION-1903", "stranger/tools"
	before := h.notices(t)
	// The paused consumer falls behind the stream, so a recovery's listing holds a key it shows
	// behind: LEGION-1903, below.
	h.behind(t, heldParent)
	h.client.show(issue(let, stranger, "backlog", false, heldTree, 1))
	// What a preparation GitHub failed for and a release that left LEGION-1903 to the recovery
	// leave, written in one step so that no pass lets the repository go between the two: no
	// mapping names stranger/tools and no todo issue is left on it, so a pass lets it go.
	h.w.recoveringMu.Lock()
	if h.w.unread == nil {
		h.w.unread = map[string]string{}
	}
	h.w.unread[stranger] = "the implement App's installation on stranger/tools could not be read: GitHub answered 502; Legion reads it again"
	if h.w.releaseWaits == nil {
		h.w.releaseWaits = map[string]map[string]releaseWait{}
	}
	h.w.releaseWaits[stranger] = map[string]releaseWait{let: {tree: heldTree, parent: heldTree}}
	h.w.recoveringMu.Unlock()
	// The clock stands still until the test moves it, so the first held pass, the one the read
	// below runs, starts the hold's clock at this moment.
	held := rigClock(h.w).Now()
	h.w.requestMappingRead()
	testwait.Eventually(t, "the recovery's listing held with "+stranger+" put back", func() bool {
		left, holding := h.w.recoveryHoldLeft()
		return holding && left == recoveryHoldBound
	})

	letGo := func() []string {
		var told []string
		for _, notice := range treeNotices(t, h.w, heldTree)[before:] {
			if strings.Contains(notice, "the daemon no longer waits for GitHub to answer for "+stranger) {
				told = append(told, notice)
			}
		}
		return told
	}
	h.rechecksToTheBound(t, held, before, nil)
	testwait.Eventually(t, heldTree+"'s architect told of "+let+" once the hold outlasted its bound", func() bool { return len(letGo()) != 0 })
	if !h.admission.Held() {
		t.Fatal("the hold released before the let-go was told, want it told while the hold stands")
	}
	if notices := letGo(); len(notices) != 1 || !strings.Contains(notices[0], let+" (backlog in Dispatch now: release it again once it is todo)") {
		t.Fatalf("let-go notices = %q, want one naming %s", notices, let)
	}
	h.w.recoveringMu.Lock()
	left := len(h.w.releaseWaits[stranger])
	marker, unread := h.w.unread[stranger]
	h.w.recoveringMu.Unlock()
	if left != 0 || unread {
		t.Fatalf("%d release waits and a marker %q are left on %s, want both gone with the telling", left, marker, stranger)
	}
}

// heldRecovery is a rig whose unread zeta/tools left the child C, under P (todo on acme/widgets,
// never recorded, its event never sent), of the live tree T to the recovery. The boot's hold
// poller runs throughout; the Dispatch consumer runs only between consume and pause, so an event
// published while it is paused keeps it behind the stream and holds the recovery's listing, and
// its run catches it up and releases the hold. (An apply that fails naks its event, and between
// redeliveries the consumer reads as idle, which would release the hold at random; one that
// blocks would hold the fact lock the recovery's listing takes.)
type heldRecovery struct {
	w      *workflowRuntime
	client *mappingDispatch
	js     jetstream.JetStream
	// passes runs a pass of the hold poller when this rig's test needs the hold released: the
	// poller's own period is the production one, far longer than a test.
	passes     *passer
	installs   *installations
	admission  *admit.Admission
	cancel     context.CancelFunc
	done       chan error
	seq        int64
	failNotice atomic.Bool
}

// noticeFailingStore is the rig's record store, its Enqueue failing for a notice while fail is set:
// the write of a recovery's notice fails, as a database refusing it would.
type noticeFailingStore struct {
	record.Store
	fail *atomic.Bool
}

func (s noticeFailingStore) Enqueue(ctx context.Context, tx pgx.Tx, row record.OutboxRow) error {
	if s.fail.Load() && row.Kind == "notice" {
		return errors.New("the notice's write fails")
	}
	return s.Store.Enqueue(ctx, tx, row)
}

const heldTree, heldParent, heldChild = "LEGION-1900", "LEGION-1901", "LEGION-1902"

// newHeldRecovery opens the rig, the consumer paused. The hold poller reads the consumer's
// position, or reader's when one is given.
func newHeldRecovery(t *testing.T, reader ...positionReader) *heldRecovery {
	t.Helper()
	h := &heldRecovery{installs: &installations{}}
	h.installs.setFailing(errors.New("GitHub answered 502"), "implement zeta/tools")
	h.w, h.client, h.installs, _, h.js = mappingRigJS(t, h.installs)
	_, h.admission = rigHandlers(h.w)
	h.w.records = noticeFailingStore{Store: h.w.records, fail: &h.failNotice}
	h.w.bootID = "held-clock-boot"
	h.passes = triggeringRuntime(h.w)
	liveRoot(t, h.w, h.client, heldTree)
	h.client.show(issue(heldParent, "acme/widgets", "todo", false, heldTree, 1), issue(heldChild, "zeta/tools", "todo", false, heldParent, 1))
	h.w.awaitRecovery("zeta/tools", heldTree, heldChild, heldParent)
	var poll positionReader = h.w.consumers
	if len(reader) != 0 {
		poll = reader[0]
	}
	run, cancel := context.WithCancel(context.Background())
	polled := make(chan struct{})
	go func() { h.w.pollHoldReleaseWith(run, poll); close(polled) }()
	t.Cleanup(func() { h.pause(); cancel(); <-polled })
	return h
}

// behind publishes a todo event of key, a child of T on acme/widgets, for the paused consumer.
func (h *heldRecovery) behind(t *testing.T, key string) {
	t.Helper()
	h.seq++
	h.client.show(issue(key, "acme/widgets", "todo", false, heldTree, 1))
	if _, err := h.js.Publish(context.Background(), "notifications.dispatch.issue."+key+".issue.updated", childEnvelope(t, key, heldTree, "acme/widgets", 2000+h.seq, 1)); err != nil {
		t.Fatal(err)
	}
}

// consume runs the Dispatch consumer until pause.
func (h *heldRecovery) consume() {
	run, cancel := context.WithCancel(context.Background())
	h.cancel, h.done = cancel, make(chan error, 1)
	go func() { h.done <- h.w.consumers.Run(run, h.w.handlers...) }()
}

func (h *heldRecovery) pause() {
	if h.cancel != nil {
		h.cancel()
		<-h.done
		h.cancel = nil
	}
}

// serve lets GitHub answer zeta/tools, asks for a read of the mapping at once, and waits until
// that read serves zeta/tools and its recovery's listing is held for the consumer behind the
// stream. It returns the moment of the rig's fake clock the hold began, which the recovery's
// bound counts from.
func (h *heldRecovery) serve(t *testing.T) time.Time {
	t.Helper()
	held := rigClock(h.w).Now()
	h.installs.setFailing(nil, "implement zeta/tools")
	h.w.requestMappingRead()
	testwait.Eventually(t, "the recovery's listing held", func() bool { return served(h.w, "zeta/tools") && h.admission.Held() })
	return held
}

func (h *heldRecovery) notices(t *testing.T) int {
	t.Helper()
	return len(treeNotices(t, h.w, heldTree))
}

// rechecksToTheBound fires the refresher's waits of a recovery first held at held, each once the
// run before it has ended (recheck), until one ends at held+recoveryHoldBound: the wait the
// refresher cut to the bound, whose run tells the release waits. No run before it may tell them
// (before is the count of notices before the hold), and no wait may end past the bound: a
// refresher that waited out its backoff over the bound, rather than cutting the wait to it, fails
// here. between, when set, runs after each run before the bound.
func (h *heldRecovery) rechecksToTheBound(t *testing.T, held time.Time, before int, between func()) {
	t.Helper()
	clk := rigClock(h.w)
	bound := held.Add(recoveryHoldBound)
	for {
		due := testwait.Until(t, "the refresher's wait before a read of its own, armed on the fake clock", func() (time.Time, bool) {
			return refresherWait(h.w)
		})
		if n := h.notices(t) - before; n != 0 {
			t.Fatalf("%d notices told %s after the hold began, before its %s bound", n, clk.Now().Sub(held), recoveryHoldBound)
		}
		if due.After(bound) {
			t.Fatalf("the refresher's next wait ends %s after the hold began, past its %s bound: want the wait cut to end at the bound", due.Sub(held), recoveryHoldBound)
		}
		if between != nil && due.Before(bound) {
			between()
		}
		clk.Set(due)
		if due.Equal(bound) {
			return
		}
	}
}

// runAfterTheBound fires the refresher's wait after the run that told the release waits (recheck)
// and waits for its run to end the recovery: the waits told, it has nothing left to tell.
func (h *heldRecovery) runAfterTheBound(t *testing.T) {
	t.Helper()
	recheck(t, h.w)
	testwait.Eventually(t, "the run after the bound to end the recovery", func() bool { return len(h.w.recoveringReasons()) == 0 })
}

// A recovery held with release waits is told at its bound, not at the refresher's next backoff:
// the refresher cut its wait to the time left to the bound, so the run that tells comes at the
// bound (rechecksToTheBound); before, the bound was checked only when the backoff next fired, up to
// a backoff later.
func TestARecoveryHeldWithReleaseWaitsIsToldAtItsBoundNotItsBackoff(t *testing.T) {
	h := newHeldRecovery(t)
	before := h.notices(t)
	h.behind(t, "LEGION-1950")
	held := h.serve(t)
	h.rechecksToTheBound(t, held, before, nil)
	testwait.Eventually(t, "the release wait told at the bound", func() bool { return h.notices(t) > before })
	if n := h.notices(t) - before; n != 1 {
		t.Fatalf("notices = %d, want one", n)
	}
}

// A hold that flaps (held at each recovery run, released between them) is told at the bound all
// the same: the clock runs from the first held run, so the notice comes once, at the bound. Here
// the consumer stays behind the stream, so each run's listing is held, and the hold poller reads a
// position that has reached it, so a pass of it releases each hold before the next run. A clock
// that started anew at each hold released and made again never reached the bound: its waits ran
// on past it (rechecksToTheBound).
func TestARecoveryWhoseHoldFlapsIsToldOnceAtTheBound(t *testing.T) {
	caughtUp := &failingPositionReader{position: intake.DispatchConsumerPosition{AckFloorStream: 1 << 40, Idle: true}}
	h := newHeldRecovery(t, caughtUp)
	before := h.notices(t)
	h.behind(t, "LEGION-1960")
	held := h.serve(t)
	h.rechecksToTheBound(t, held, before, func() {
		h.passes.until(t, passHoldRelease, "the recovery's listing released between runs", func() bool { return !h.admission.Held() })
	})
	testwait.Eventually(t, "the release wait told at the bound", func() bool { return h.notices(t) > before })
	h.runAfterTheBound(t)
	if n := h.notices(t) - before; n != 1 {
		t.Fatalf("notices = %d, want one", n)
	}
}

// failPastTheBound holds the recovery's listing, lets the bound pass with fault set, and then
// checks the refresher waits its backoff between runs, not a busy loop: after the run at the
// bound, the refresher arms its next wait with at most that run's Dispatch listing in between. It
// then clears the fault with clear, fires that wait, and the notice comes once.
func failPastTheBound(t *testing.T, fault, clear func(h *heldRecovery)) {
	t.Helper()
	h := newHeldRecovery(t)
	before := h.notices(t)
	h.behind(t, "LEGION-1970")
	held := h.serve(t)
	fault(h)
	h.rechecksToTheBound(t, held, before, nil)
	listings := h.client.listingCount()
	testwait.Until(t, "the refresher's wait after the run at the bound, armed on the fake clock", func() (time.Time, bool) {
		if n := h.client.listingCount() - listings; n > 1 {
			t.Fatalf("%d Dispatch listings since the run at the bound with no wait between them, want the refresher to wait its backoff", n)
		}
		return refresherWait(h.w)
	})
	if n := h.notices(t) - before; n != 0 {
		t.Fatalf("notices while the fault lasts = %d, want none", n)
	}
	clear(h)
	recheck(t, h.w)
	testwait.Eventually(t, "the release wait told once the fault cleared", func() bool { return h.notices(t) > before })
	h.runAfterTheBound(t)
	if n := h.notices(t) - before; n != 1 {
		t.Fatalf("notices = %d, want one", n)
	}
}

// A held recovery whose Dispatch listing keeps failing once its bound has passed waits the
// refresher's backoff between runs, and tells its waits once the listing answers. Before, the wait
// cut to the bound's zero time left made every failed run re-run at once.
func TestARecoveryWhoseListingFailsPastItsBoundWaitsItsBackoff(t *testing.T) {
	failPastTheBound(t, func(h *heldRecovery) {
		h.client.mu.Lock()
		h.client.listFailures = 1 << 30
		h.client.mu.Unlock()
	}, func(h *heldRecovery) {
		h.client.mu.Lock()
		h.client.listFailures = 0
		h.client.mu.Unlock()
	})
}

// The same when the write of the recovery's notice keeps failing past the bound.
func TestARecoveryWhoseNoticeFailsPastItsBoundWaitsItsBackoff(t *testing.T) {
	failPastTheBound(t, func(h *heldRecovery) { h.failNotice.Store(true) }, func(h *heldRecovery) { h.failNotice.Store(false) })
}
