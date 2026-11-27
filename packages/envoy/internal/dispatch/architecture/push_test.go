package architecture

//ci:reads packages/envoy/scripts/fixtures/github/push.json push os.ReadFile(pushFixture)

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// waitForPushWorkers returns once nothing of importer's runs: no push worker and no flight. A
// worker is counted in importer.running from before its goroutine starts until it returns, and a
// flight until it has answered, so once every Pushed call has returned this is "every push worker
// has returned". It fails the test rather than hang when one never does.
func waitForPushWorkers(t *testing.T, importer *Importer) {
	t.Helper()
	idle := make(chan struct{})
	go func() {
		importer.running.wait()
		close(idle)
	}()
	testwait.Receive(t, "every push worker and flight to return", idle)
}

// pendingPushes counts the sources whose push worker importer still holds an entry for.
func pendingPushes(importer *Importer) int {
	importer.pushMu.Lock()
	defer importer.pushMu.Unlock()
	return len(importer.pushes)
}

// capturePushLogs makes a buffer the default logger's sink, as JSON lines, until the test ends,
// and answers it. The push workers log from goroutines of their own, so the buffer is locked.
func capturePushLogs(t *testing.T) *logtest.Buffer {
	t.Helper()
	logged := &logtest.Buffer{}
	logtest.SetDefault(t, slog.NewJSONHandler(logged, nil))
	return logged
}

// pushWarning is the one WARN line of logged whose message is msg, with want's fields as want
// holds them, and fails the test where the log holds no such line or more than one.
func pushWarning(t *testing.T, logged *logtest.Buffer, msg string, want map[string]any) {
	t.Helper()
	var found []map[string]any
	for _, line := range logtest.JSONLines(t, logged.String()) {
		if line["level"] == "WARN" && line["msg"] == msg {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the log holds %d WARN lines %q; want 1. It holds:\n%s", len(found), msg, logged.String())
	}
	for field, value := range want {
		if got, held := found[0][field]; !held || got != value {
			t.Fatalf("the WARN line %q holds %s=%v; want %v. It is %v", msg, field, got, value, found[0])
		}
	}
	for _, field := range []string{"project", "repo", "branch", "pushes"} {
		if _, wanted := want[field]; !wanted {
			if got, held := found[0][field]; held {
				t.Fatalf("the WARN line %q holds %s=%v; want no such field. It is %v", msg, field, got, found[0])
			}
		}
	}
}

// headClock is a fake GitHub's handler that keeps when its latest branch-head lookup arrived. An
// import covers only the pushes recorded no later than it issued its head lookup
// (syncFlight.headAt), and a push is recorded after the test asked for it, so an import that
// covered a push looked the head up after the push was asked for. A last lookup that arrived
// before the newest ask therefore proves a lost push. A last lookup that arrived after it is a
// necessary condition only, and no proof that every push was covered.
type headClock struct {
	next http.Handler

	mu     sync.Mutex
	latest time.Time
}

func (c *headClock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/commits/") {
		c.mu.Lock()
		c.latest = time.Now()
		c.mu.Unlock()
	}
	c.next.ServeHTTP(w, r)
}

func (c *headClock) latestLookup() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.latest
}

// One push imports every enabled source of every project on its repository and branch: the
// repository as GitHub's case names it matches the lower-cased stored one, and the branch matches
// exactly. A disabled source, a source on another branch (one only its case tells apart
// included) and a source on another repository do not import; the fake would answer each of
// them, so it is the match that leaves them out.
func TestAPushImportsEveryEnabledSourceOfItsRepositoryAndBranch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := &fakeSource{t: t, files: validFiles()}
	importer, database := newImporterFixture(t, fake)
	addSource(t, database, fake, infraRepo, commitInfraOne, infraFiles())
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee)
		values ('OPS', 'Operations', 'alice'), ('OLD', 'Retired', 'alice'), ('DEV', 'Development', 'alice'), ('CAPS', 'Capitals', 'alice');
		insert into architecture_sources (project_key, repo, branch, installation_id, created_by)
		values ('OPS', 'legion/arch', 'main', 11, '{"kind":"user","id":"alice"}'),
		       ('OLD', 'legion/arch', 'main', 11, '{"kind":"user","id":"alice"}'),
		       ('DEV', 'legion/arch', 'release', 11, '{"kind":"user","id":"alice"}'),
		       ('CAPS', 'legion/arch', 'Main', 11, '{"kind":"user","id":"alice"}');
		update architecture_sources set enabled = false where project_key = 'OLD';
	`); err != nil {
		t.Fatalf("seed sources: %v", err)
	}

	if err := importer.Pushed(ctx, "Legion/ARCH", "main"); err != nil {
		t.Fatalf("Pushed answered %v; want nil", err)
	}
	waitForPushWorkers(t, importer)

	rows, err := database.Pool.Query(ctx, `
		select project_key || ' ' || repo || '@' || branch || ' ' || coalesce(last_commit, 'never imported')
		from architecture_sources order by project_key, id
	`)
	if err != nil {
		t.Fatalf("read sources: %v", err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read sources: %v", err)
	}
	want := []string{
		"CAPS legion/arch@Main never imported",
		"CORE legion/arch@main " + commitOne,
		"CORE acme/infra@main never imported",
		"DEV legion/arch@release never imported",
		"OLD legion/arch@main never imported",
		"OPS legion/arch@main " + commitOne,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("after a push of legion/arch's main, the sources are\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if installations, heads := fake.lookups(); installations != 2 || heads != 2 {
		t.Fatalf("one push cost %d installation lookups and %d head lookups; want 2 and 2, one each for CORE's and OPS's source", installations, heads)
	}
	if left := pendingPushes(importer); left != 0 {
		t.Fatalf("%d sources still hold pushes after every worker returned; want none", left)
	}
}

// A push that reaches an importer whose Close has begun starts nothing: Pushed answers nil before
// it reads a source, no worker is left to run, and the fake GitHub sees no request. The push comes
// with a context that is already cancelled, so a read of the sources would fail and Pushed would
// answer that error: the nil answer shows that Pushed checked Close before it read. The lookups
// alone cannot show that check, because the count that refuses a worker once Close has begun
// (syncCount.begin) keeps every worker out without it. The push is not dropped silently: Pushed
// logs one WARN line naming its repository and branch, and no project, since it read no source.
func TestAPushAfterCloseStartsNothing(t *testing.T) {
	// Not parallel: it replaces the process's default logger (logtest.SetDefault, through
	// capturePushLogs).
	fake := &fakeSource{t: t, files: validFiles()}
	importer, _ := newImporterFixture(t, fake)
	logged := capturePushLogs(t)
	importer.Close()

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := importer.Pushed(cancelled, archRepo, "main"); err != nil {
		t.Fatalf("Pushed after Close answered %v; want nil, with no read of the sources", err)
	}
	waitForPushWorkers(t, importer)
	if left := pendingPushes(importer); left != 0 {
		t.Fatalf("%d sources hold pushes after Close; want none", left)
	}
	if installations, heads := fake.lookups(); installations != 0 || heads != 0 {
		t.Fatalf("a push after Close cost %d installation lookups and %d head lookups; want none", installations, heads)
	}
	pushWarning(t, logged, pushAfterCloseMessage, map[string]any{"repo": archRepo, "branch": "main"})
}

// A push worker that Close stops says which pushes no import covered. Its push's import is held
// after its head lookup, and Close cancels i.owner, which ends the held read and the worker's wait
// on that import alike: syncAfter answers the worker the zero time it answers once its context is
// done, which covers no push. Should the cut-short import's own answer reach the worker first, its
// cancel error covers no push either; that is coverPushes' rule for an answer Close may have cut
// short, which this test does not need, and which
// TestCoverPushesLetsNoAnswerCloseMayHaveCutShortCoverAPush holds. The worker, returning on the
// process's stop, logs one WARN line naming the source, its branch and the one push, which the
// next process's five-minute import repairs, and no line of a failed import
// (importAfterPushFailedMessage), since the error syncAfter answered it is the stop's own. Close
// waits for the worker, so the lines are logged once Close has returned. It fails where
// importPushes logs the failure line of an import the stop cut short.
func TestAPushWorkerStoppedByCloseSaysWhichPushesNoImportCovered(t *testing.T) {
	// Not parallel: it replaces the process's default logger (logtest.SetDefault, through
	// capturePushLogs).
	fake, importer, database, before := movedSource(t)
	logged := capturePushLogs(t)

	if err := importer.Pushed(context.Background(), archRepo, "main"); err != nil {
		t.Fatalf("Pushed answered %v; want nil", err)
	}
	fake.blobs.waitHeld(t)
	if _, heads := fake.lookups(); heads-before != 1 {
		t.Fatalf("the push's import held in its blob read cost %d head lookups; want 1: it looked the head up before the hold", heads-before)
	}
	// Close ends the held read by cancelling its request, and waits for the worker.
	closed := make(chan struct{})
	go func() {
		importer.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(30 * time.Second):
		t.Fatal("Close did not return within 30s of a push's import held in its blob read; want it to end the import and wait for the worker")
	}

	if left := pendingPushes(importer); left != 0 {
		t.Fatalf("%d sources hold pushes after Close; want none", left)
	}
	if recorded := recordedImport(t, database); recorded != commitOne {
		t.Fatalf("after Close cut the push's import short the source records %s; want %s, the import before the push", recorded, commitOne)
	}
	pushWarning(t, logged, pushesUncoveredMessage,
		map[string]any{"project": "CORE", "repo": archRepo, "branch": "main", "pushes": float64(1)})
	if imported := logged.Lines(importedAfterPushMessage); imported != "" {
		t.Fatalf("the log names an import after the push; want none, since Close cut it short:\n%s", imported)
	}
	if failed := logged.Lines(importAfterPushFailedMessage); failed != "" {
		t.Fatalf("the log names a failed import after the push; want none, since the error is the stop's own:\n%s", failed)
	}
}

// A push whose import fails while the process runs says so: the worker logs one WARN line naming
// the source, its branch and the one push the failed import covered. The fake answers the
// branch's head as GitHub answers a branch it does not have (branchMissing), so the import fails
// after its head lookup, whose moment is later than the push. While the process runs, a failed
// import's answer covers the pushes recorded no later than that moment, as any answer does
// (coverPushes), so the worker returns with no push left and logs no line of uncovered pushes.
// The failure is on the source row too, as the five-minute import's failures are. It fails where
// importPushes logs no failure line for an import that failed while the process ran.
func TestAPushWhoseImportFailsWhileTheProcessRunsSaysSo(t *testing.T) {
	// Not parallel: it replaces the process's default logger (logtest.SetDefault, through
	// capturePushLogs).
	fake := &fakeSource{t: t, files: validFiles(), branchMissing: true}
	importer, database := newImporterFixture(t, fake)
	logged := capturePushLogs(t)

	if err := importer.Pushed(context.Background(), archRepo, "main"); err != nil {
		t.Fatalf("Pushed answered %v; want nil", err)
	}
	waitForPushWorkers(t, importer)

	if _, heads := fake.lookups(); heads != 1 {
		t.Fatalf("a push whose import failed cost %d head lookups; want 1: one import, which looked the head up and failed", heads)
	}
	if left := pendingPushes(importer); left != 0 {
		t.Fatalf("%d sources hold pushes after the push's import failed; want none, since the failed import covered the push", left)
	}
	if recorded := recordedImport(t, database); !strings.HasPrefix(recorded, "never imported failed: ") {
		t.Fatalf("after the push's import failed the source records %s; want no commit and the failure", recorded)
	}
	pushWarning(t, logged, importAfterPushFailedMessage,
		map[string]any{"project": "CORE", "repo": archRepo, "branch": "main", "pushes": float64(1)})
	if uncovered := logged.Lines(pushesUncoveredMessage); uncovered != "" {
		t.Fatalf("the log names pushes no import covered; want none, since the process did not stop:\n%s", uncovered)
	}
}

// seedPush records one push of main against CORE's source from archRepo on importer, at at, as
// recordPush records the push that starts a worker, but starts none: the test plays the worker's
// part. It answers the source's key.
func seedPush(importer *Importer, at time.Time) string {
	key := syncKey("CORE", archRepo)
	importer.pushMu.Lock()
	defer importer.pushMu.Unlock()
	importer.pushes[key] = &pushedSource{branch: "main", at: []time.Time{at}}
	return key
}

// An answer Close may have cut short covers no push: while the process stops, coverPushes counts
// the push as one no import covered, although the answer's moment is later than it. A flight that
// Close cut short, or one whose begin the count refused, answers such a moment beside the cancel
// error, and the worker is handed that answer only where its own wait takes the flight's answer
// before it sees i.owner end; otherwise the wait answers a zero time, which covers nothing
// (TestAPushWorkerStoppedByCloseSaysWhichPushesNoImportCovered). No import a test runs reaches the
// rule on demand, so the test calls coverPushes itself, on a seeded push and with no worker. A
// whole answer (nil or ErrNoSource) covers the push while the process stops, and any answer covers
// it while the process runs. In each case the worker has no push left to import, so the entry is
// removed and the worker returns. It fails where coverPushes lets an answer Close may have cut
// short cover a push.
func TestCoverPushesLetsNoAnswerCloseMayHaveCutShortCoverAPush(t *testing.T) {
	t.Parallel()
	pushed := time.Now()
	servedAt := pushed.Add(time.Second)
	for _, c := range []struct {
		name               string
		stopping, whole    bool
		covered, uncovered int
	}{
		{"an answer Close may have cut short, while the process stops", true, false, 0, 1},
		{"a whole answer, while the process stops", true, true, 1, 0},
		{"a failed answer, while the process runs", false, false, 1, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			importer := NewImporter(nil, nil, nil)
			t.Cleanup(importer.Close)
			key := seedPush(importer, pushed)
			if c.stopping {
				importer.abandon()
			}
			covered, uncovered, stopping, again := importer.coverPushes(key, servedAt, c.whole)
			if covered != c.covered || uncovered != c.uncovered || stopping != c.stopping || again {
				t.Fatalf("coverPushes answered covered %d, uncovered %d, stopping %v, again %v; want %d, %d, %v, false",
					covered, uncovered, stopping, again, c.covered, c.uncovered, c.stopping)
			}
			if left := pendingPushes(importer); left != 0 {
				t.Fatalf("%d sources hold pushes after coverPushes answered that the worker returns; want none", left)
			}
		})
	}
}

// A push whose worker the count refuses, because Close has begun, is not dropped silently:
// recordPush records nothing, starts no worker, and logs one WARN line naming the source's project
// and repository and the push's branch. Pushed checks Close before it reads a source, so the test
// calls recordPush itself, as a Pushed that Close began behind, once it had read the sources,
// would. It fails where recordPush drops such a push with no line.
func TestARefusedBeginSaysThePushIsDropped(t *testing.T) {
	// Not parallel: it replaces the process's default logger (logtest.SetDefault, through
	// capturePushLogs).
	importer := NewImporter(nil, nil, nil)
	logged := capturePushLogs(t)
	importer.Close()

	importer.recordPush(EnabledSource{Project: "CORE", Repo: archRepo}, "main")
	if left := pendingPushes(importer); left != 0 {
		t.Fatalf("%d sources hold pushes after a push Close refused; want none", left)
	}
	pushWarning(t, logged, pushRefusedMessage, map[string]any{"project": "CORE", "repo": archRepo, "branch": "main"})
}

// Many pushes that arrive while a source's worker makes its last look are each covered: the
// worker's decision to return and a push's record are taken under one mutex, so a push either
// reaches the worker before it decides, and the worker imports once more, or finds it gone and
// starts a worker of its own. When every worker has returned, no push is newer than the last
// import's head lookup, and no source still holds a push. Each round ends with the worker's
// return, and the pushers of the round are still pushing as it nears it. The race half of this
// check needs a `CGO_ENABLED=1 go test -race` run of this test, which CI does not make: the
// envoy-go-race job does not test this package.
func TestAPushDuringAWorkersLastLookIsNotLost(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := &fakeSource{t: t, files: validFiles()}
	_, database := newImporterFixture(t, fake)
	// The fixture's own importer stays idle: this one asks the same fake through a clock of its
	// head lookups.
	heads := &headClock{next: fake.handler()}
	importer := NewImporter(database, newGitHubClient(t, heads), events.NewBroker())
	t.Cleanup(importer.Close)
	// After the first import every import is the unchanged-head refresh, a short one, so a round
	// holds several imports and several last looks for the pushes to race.
	if _, err := importer.Sync(ctx, "CORE", archRepo); err != nil {
		t.Fatalf("first import: %v", err)
	}

	const rounds, pushers, pushesEach = 10, 8, 20
	for round := range rounds {
		// asked[p] is when pusher p last asked for a push: no earlier than that push's record.
		asked := make([]time.Time, pushers)
		var group sync.WaitGroup
		for p := range pushers {
			group.Go(func() {
				for range pushesEach {
					asked[p] = time.Now()
					if err := importer.Pushed(ctx, archRepo, "main"); err != nil {
						t.Errorf("round %d: Pushed answered %v; want nil", round, err)
						return
					}
				}
			})
		}
		group.Wait()
		waitForPushWorkers(t, importer)

		newest := slices.MaxFunc(asked, time.Time.Compare)
		if looked := heads.latestLookup(); looked.Before(newest) {
			t.Fatalf("round %d: the last import's head lookup arrived %s before the newest push was asked for; want every push covered by an import whose head lookup came after it",
				round, newest.Sub(looked))
		}
		if left := pendingPushes(importer); left != 0 {
			t.Fatalf("round %d: %d sources still hold pushes after every worker returned; want none", round, left)
		}
	}
}

// pushFixture is GitHub's push event as the webhook's fixtures hold it (packages/envoy/scripts).
const pushFixture = "../../../scripts/fixtures/github/push.json"

// pushBus is the way a push of GitHub's reaches the importer in the server: a NATS server, the
// delivery intakes (delivery.StartIntakes), which hand each push of a branch to the importer, and
// a publisher that owns the notification stream, as Envoy's webhook does.
type pushBus struct {
	publisher *bus.Client
	// sent counts the pushes published so far; it numbers each push's delivery id.
	sent int
}

// startPushBus starts a NATS server of the test's own and the real intakes on it, with importer
// as what the delivery intake hands a push of a branch to, and returns once that intake has bound
// its durable, so a push published after it is delivered at once. No delivery settings are saved:
// the delivery intake binds its durable and routes a push whatever the settings say (delivery's
// Intake.Run), and a push's handler reads none of them (delivery's handlePushEnvelope). The
// intakes read GitHub through the importer's own App client, as the server's do, and a push costs
// them no call of theirs.
// architecture.Run is not started, so no timer pass imports anything: every import the test sees
// is one a push or the test itself started. The intakes stop before the importer closes, and the
// pool with it, because newImporterFixture registered its cleanups first.
func startPushBus(t *testing.T, importer *Importer, database *store.Store) *pushBus {
	t.Helper()
	_, natsURL := testnats.Start(t)
	publisher, err := bus.ConnectOwningStream([]string{natsURL})
	if err != nil {
		t.Fatalf("connect the publisher: %v", err)
	}
	t.Cleanup(publisher.Close)
	callers, err := database.Pool.Leases()
	if err != nil {
		t.Fatalf("open the lease pool: %v", err)
	}
	leases, err := delivery.ReserveIntakeLeases(callers)
	if err != nil {
		t.Fatalf("reserve the intakes' callers of the lease pool: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	stop, err := delivery.StartIntakes(ctx, func(name string) (*bus.Client, error) {
		return bus.Connect([]string{natsURL}, bus.WithName(name))
	}, database.Pool, leases, importer.github, events.NewBroker(), nil, nil, importer, delivery.NewAttributor(refs.Renames{}))
	if err != nil {
		cancel()
		t.Fatalf("start the intakes: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		stop()
	})
	pushes := &pushBus{publisher: publisher}
	testwait.UntilPolling(t, 50*time.Millisecond, "the delivery intake to bind its durable", func() (string, bool) {
		info, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		return pushes.durable(), err == nil && info.PushBound
	})
	return pushes
}

// push publishes the one envelope Envoy's webhook makes of GitHub's push of ref to owner/name: the
// push fixture with its repository and ref set to these, normalized and checked as the webhook
// normalizes and checks it (contracts.GithubEnvelopes, Envelope.Validate), on the envelope's own
// topic. Each push takes a delivery id of its own: the bus stores a GitHub envelope under a
// message id made of its dedupe key, which holds the delivery id, and its topic, so two pushes of
// one ref under one id would be stored once. The ids need only differ within this bus's stream,
// which no other test shares.
func (b *pushBus) push(t *testing.T, owner, name, ref string) {
	t.Helper()
	raw, err := os.ReadFile(pushFixture)
	if err != nil {
		t.Fatalf("read the push fixture: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode the push fixture: %v", err)
	}
	body["ref"] = ref
	body["repository"] = map[string]any{
		"name": name, "full_name": owner + "/" + name, "owner": map[string]any{"login": owner},
	}
	b.sent++
	id := fmt.Sprintf("push-%d", b.sent)
	items := contracts.GithubEnvelopes(contracts.GithubEnvelopeInput{
		Event: "push", Delivery: id, Body: body, EventID: "event-" + id, TraceID: "trace-" + id,
	}, "@legion")
	if len(items) != 1 {
		t.Fatalf("a push of %s to %s/%s makes %d envelopes, want 1", ref, owner, name, len(items))
	}
	if err := items[0].Validate(); err != nil {
		t.Fatalf("the envelope of a push of %s to %s/%s: %v", ref, owner, name, err)
	}
	if err := b.publisher.Publish(items[0]); err != nil {
		t.Fatalf("publish %s: %v", items[0].Topic, err)
	}
}

// awaitHandedOver returns once the delivery intake has been handed every message the stream holds
// and has acknowledged each: its durable has nothing pending, nothing awaiting acknowledgement, and
// its acknowledgement floor at the stream's last sequence. The intake acknowledges a push only
// after the importer's Pushed has returned, and Pushed counts each worker it starts before it
// returns, so once this returns every push published so far has reached the importer, and
// waitForPushWorkers then waits out every import those pushes started. A later push is no such
// fence for an earlier one, because the intake handles the pushes of different repositories (two
// intake keys) at the same time.
func (b *pushBus) awaitHandedOver(t *testing.T) {
	t.Helper()
	stream, err := b.publisher.JS().StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	last := stream.State.LastSeq
	testwait.UntilPolling(t, 50*time.Millisecond, fmt.Sprintf("the delivery intake to acknowledge through stream sequence %d", last), func() (string, bool) {
		info, err := b.publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		return b.durable(), err == nil && info.NumPending == 0 && info.NumAckPending == 0 && info.AckFloor.Stream >= last
	})
}

// durable says what the NATS server holds of the delivery intake's durable, for a failure's text.
func (b *pushBus) durable() string {
	info, err := b.publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("push_bound=%t delivered=%d ack_floor=%d pending=%d ack_pending=%d",
		info.PushBound, info.Delivered.Stream, info.AckFloor.Stream, info.NumPending, info.NumAckPending)
}

// recordedImport is what CORE's source from archRepo last recorded: its commit, or "never
// imported", and its error, when it holds one.
func recordedImport(t *testing.T, database *store.Store) string {
	t.Helper()
	var recorded string
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce(last_commit, 'never imported') || coalesce(' failed: ' || last_error, '')
		from architecture_sources where project_key = 'CORE' and repo = $1
	`, archRepo).Scan(&recorded); err != nil {
		t.Fatalf("read CORE's source: %v", err)
	}
	return recorded
}

// movedSource is newImporterFixture's source imported once, at commitOne, whose branch has since
// moved to commitTwo with another tree, and whose fake holds every blob read from here on. So the
// next import of it looks the head up, reads the new tree and stops at the hold: held after its
// head lookup, where a push that arrives is no longer one that import covers (Pushed). It answers
// the head lookups made so far, from which the caller counts its own.
func movedSource(t *testing.T) (*fakeSource, *Importer, *store.Store, int) {
	t.Helper()
	fake := &fakeSource{t: t, files: validFiles()}
	importer, database := newImporterFixture(t, fake)
	if _, err := importer.Sync(context.Background(), "CORE", archRepo); err != nil {
		t.Fatalf("import the source at %s: %v", commitOne, err)
	}
	// Once Sync has answered, no request to the fake is in flight, so its fields change under no
	// reader.
	fake.commit, fake.files, fake.holdArchBlobs = commitTwo, infraFiles(), true
	_, heads := fake.lookups()
	return fake, importer, database, heads
}

// A push of a component source's branch, published on the bus as Envoy's webhook publishes it,
// starts that source's import through the delivery intake, with no delivery settings saved
// (startPushBus) and no timer pass running, once.
// A push to another branch, a push of a tag of the branch's name and a push of the same branch to
// another repository start none. Pushes that arrive while the source's import runs, after its
// head lookup, start one more import between them, whether a push or the five-minute import
// started the running one. Each subtest has a database, a fake GitHub, a NATS server and intakes
// of its own, because the fake's hold on blob reads is released once and for good, and each
// counts imports by the fake's head lookups.
func TestAPushToAComponentSourceStartsItsImport(t *testing.T) {
	t.Parallel()
	t.Run("a push to the source's branch imports it once", func(t *testing.T) {
		fake := &fakeSource{t: t, files: validFiles()}
		importer, database := newImporterFixture(t, fake)
		pushes := startPushBus(t, importer, database)

		pushes.push(t, "legion", "arch", "refs/heads/main")
		testwait.UntilPolling(t, 50*time.Millisecond, "an import of legion/arch after a push of its main: a head lookup at the fake GitHub", func() (string, bool) {
			if _, heads := fake.lookups(); heads > 0 {
				return "", true
			}
			return "no head lookup; the intake's durable is " + pushes.durable(), false
		})
		pushes.awaitHandedOver(t)
		waitForPushWorkers(t, importer)
		if _, heads := fake.lookups(); heads != 1 {
			t.Fatalf("one push cost %d head lookups; want 1: one import", heads)
		}
		if recorded := recordedImport(t, database); recorded != fake.commit {
			t.Fatalf("after the push's import the source records %s; want %s, the branch's head", recorded, fake.commit)
		}
	})

	t.Run("a push to another branch, of a tag or of another repository imports nothing", func(t *testing.T) {
		fake := &fakeSource{t: t, files: validFiles()}
		importer, database := newImporterFixture(t, fake)
		pushes := startPushBus(t, importer, database)

		for _, other := range []struct{ owner, name, ref string }{
			{"legion", "arch", "refs/heads/release"},
			{"legion", "arch", "refs/tags/main"},
			{"legion", "other", "refs/heads/main"},
		} {
			pushes.push(t, other.owner, other.name, other.ref)
			pushes.awaitHandedOver(t)
		}
		waitForPushWorkers(t, importer)
		if _, heads := fake.lookups(); heads != 0 {
			t.Fatalf("pushes of legion/arch's release, of its tag main and of legion/other's main cost %d head lookups; want none: no source is on those", heads)
		}

		pushes.push(t, "legion", "arch", "refs/heads/main")
		pushes.awaitHandedOver(t)
		waitForPushWorkers(t, importer)
		if _, heads := fake.lookups(); heads != 1 {
			t.Fatalf("a push of legion/arch's main after them cost %d head lookups; want 1: the import that shows this test sees one", heads)
		}
	})

	t.Run("three pushes while its import runs start one more import", func(t *testing.T) {
		fake, importer, database, before := movedSource(t)
		pushes := startPushBus(t, importer, database)

		pushes.push(t, "legion", "arch", "refs/heads/main")
		fake.blobs.waitHeld(t)
		for range 3 {
			pushes.push(t, "legion", "arch", "refs/heads/main")
		}
		// Every one of the three has reached the importer while the import the first push
		// started is still held.
		pushes.awaitHandedOver(t)
		if _, heads := fake.lookups(); heads-before != 1 {
			t.Fatalf("while the first push's import is held, four pushes cost %d head lookups; want 1: the held import's", heads-before)
		}
		fake.blobs.releaseHeld(t)
		waitForPushWorkers(t, importer)
		if _, heads := fake.lookups(); heads-before != 2 {
			t.Fatalf("a push and three more after its import looked the head up cost %d head lookups; want 2: that import, and one more for the three it did not cover", heads-before)
		}
		if recorded := recordedImport(t, database); recorded != commitTwo {
			t.Fatalf("after the imports the source records %s; want %s, the branch's head", recorded, commitTwo)
		}
	})

	t.Run("pushes while the five-minute import runs start one more import", func(t *testing.T) {
		fake, importer, database, before := movedSource(t)
		pushes := startPushBus(t, importer, database)

		// The timer's pass imports each source by Sync (syncAll), as this import does.
		timed := syncInBackground(importer)
		fake.blobs.waitHeld(t)
		for range 3 {
			pushes.push(t, "legion", "arch", "refs/heads/main")
		}
		// Every one of the three has reached the importer while the five-minute import is held.
		pushes.awaitHandedOver(t)
		if _, heads := fake.lookups(); heads-before != 1 {
			t.Fatalf("while the five-minute import is held, three pushes cost %d head lookups; want 1: the held import's", heads-before)
		}
		fake.blobs.releaseHeld(t)
		if answer := waitForSync(t, timed, "the five-minute import"); answer.err != nil {
			t.Fatalf("the five-minute import answered %v; want nil", answer.err)
		}
		waitForPushWorkers(t, importer)
		if _, heads := fake.lookups(); heads-before != 2 {
			t.Fatalf("three pushes after the five-minute import looked the head up cost %d head lookups with it; want 2: that import, and one more for the three it did not cover", heads-before)
		}
		if recorded := recordedImport(t, database); recorded != commitTwo {
			t.Fatalf("after the imports the source records %s; want %s, the branch's head", recorded, commitTwo)
		}
	})
}
