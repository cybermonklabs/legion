package daemon

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// The sessions the boot's listing names as claim holders: the one the daemon records in the
// issue's own tree, one it records in a tree of its own, and one it records nowhere.
const (
	bootOwnSession     = "ses-own-tree"
	bootOtherSession   = "ses-another-tree"
	bootUnknownSession = "ses-stranger"
	// bootOtherTree is the tree bootOtherSession claims in, which no listed issue stands in.
	bootOtherTree = "CAPTURE-900"
)

// bootClaimFixture is one row of the boot's listing: what Dispatch lists for the issue, what the
// daemon records for it, what the issue's own read shows the release row when it runs, and what
// the pass and that row must come to.
type bootClaimFixture struct {
	key string
	// status is the issue's Dispatch status, in the listing and in its own read.
	status string
	// listedClaim is the claim the listing carries, as Claim.Holder writes it, and listedUnread
	// says the listing showed a claim naming no actor.
	listedClaim  string
	listedUnread bool
	// recorded is the daemon's own record of the issue; nil is an issue it records nothing for.
	recorded *record.Issue
	// liveClaim is the claim the issue's own read shows when the release row runs, nil for none.
	liveClaim *dispatch.Claim
	// slotted holds the issue's admission slot and opens its tree under the workflow, which a
	// root in a phase must have: admission takes an unslotted root out of the workflow
	// (LeaveUnslotted), which queues a release row of its own.
	slotted bool
	// operatorHolds opens the issue's tree under the operator's authority.
	operatorHolds bool
	// queued is whether the boot queues a claim release row for the issue, and deleted whether
	// Dispatch is sent the DELETE once that row runs.
	queued, deleted bool
}

// bootParked is the record of an issue the daemon took out of the workflow without closing it:
// the one state whose Dispatch claim the boot releases.
func bootParked(key string) *record.Issue {
	return bootRecord(key, "backlog", phase.Done)
}

// bootRecord is key as the daemon records it: a root of its own tree, at status and phase.
func bootRecord(key, status string, at phase.Phase) *record.Issue {
	return &record.Issue{Key: key, Tree: key, Project: "CAPTURE", Title: key, Phase: at, Generation: 1,
		Status: status, Rank: "A", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
}

// bootClaimListing is the board the boot reads: every shape a claim on it can have, each on an
// issue of its own, and a parked issue carrying no claim at all.
func bootClaimListing() []bootClaimFixture {
	return []bootClaimFixture{
		{key: "CAPTURE-1", status: "backlog", listedClaim: "session:" + bootOwnSession,
			recorded: bootParked("CAPTURE-1"), liveClaim: sessionClaim(bootOwnSession), queued: true, deleted: true},
		{key: "CAPTURE-2", status: "backlog", listedClaim: "user:alice",
			recorded: bootParked("CAPTURE-2"), liveClaim: &dispatch.Claim{Kind: "user", ID: "alice"}, queued: true},
		{key: "CAPTURE-3", status: "backlog", listedClaim: "session:" + bootOtherSession,
			recorded: bootParked("CAPTURE-3"), liveClaim: sessionClaim(bootOtherSession), queued: true},
		{key: "CAPTURE-4", status: "backlog", listedClaim: "session:" + bootUnknownSession,
			recorded: bootParked("CAPTURE-4"), liveClaim: sessionClaim(bootUnknownSession), queued: true},
		{key: "CAPTURE-5", status: "backlog", listedClaim: "session:" + bootOwnSession,
			liveClaim: sessionClaim(bootOwnSession)},
		{key: "CAPTURE-6", status: "done", listedClaim: "session:" + bootOwnSession,
			recorded: bootRecord("CAPTURE-6", "done", phase.Done), liveClaim: sessionClaim(bootOwnSession)},
		{key: "CAPTURE-7", status: "in_progress", listedClaim: "session:" + bootOwnSession, slotted: true,
			recorded: bootRecord("CAPTURE-7", "in_progress", phase.Implementing), liveClaim: sessionClaim(bootOwnSession)},
		{key: "CAPTURE-8", status: "backlog", listedUnread: true,
			recorded: bootParked("CAPTURE-8"), liveClaim: sessionClaim(bootOwnSession), queued: true, deleted: true},
		{key: "CAPTURE-9", status: "backlog", listedClaim: "session:" + bootOwnSession,
			recorded: bootParked("CAPTURE-9"), liveClaim: sessionClaim(bootOwnSession), operatorHolds: true, queued: true},
		{key: "CAPTURE-10", status: "backlog", recorded: bootParked("CAPTURE-10")},
	}
}

// bootClaimDispatch is Dispatch as the boot's pass and the rows it queues meet it: the project
// listing the boot reads, the issue each row's own read answers with, and every DELETE sent. It
// is read while the runner runs, which outboxDispatch, whose other methods it keeps, is not made
// for.
type bootClaimDispatch struct {
	*outboxDispatch
	mu      sync.Mutex
	listing []dispatch.IssueSummary
	// shown is the issue each key's own read answers with; a release Dispatch takes clears its
	// claim, as Dispatch's own DELETE does.
	shown map[string]dispatch.Issue
	// answer is what the DELETE numbered n, counting from zero, gets back; nil is the release
	// Dispatch took. A nil answer takes every release.
	answer   func(n int) error
	released []string
}

func (d *bootClaimDispatch) ListIssues(context.Context, string, []string) ([]dispatch.IssueSummary, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.listing), nil
}

func (d *bootClaimDispatch) GetIssue(_ context.Context, key string) (dispatch.Issue, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.shown[key], nil
}

func (d *bootClaimDispatch) ReleaseClaim(_ context.Context, key string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.released = append(d.released, key)
	if d.answer != nil {
		if err := d.answer(len(d.released) - 1); err != nil {
			return err
		}
	}
	shown := d.shown[key]
	shown.Claim, shown.ClaimUnread = nil, false
	d.shown[key] = shown
	return nil
}

// releases is each key a DELETE was sent for, in order.
func (d *bootClaimDispatch) releases() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.released)
}

// releasesOf is how many DELETEs were sent for key.
func (d *bootClaimDispatch) releasesOf(key string) int {
	count := 0
	for _, released := range d.releases() {
		if released == key {
			count++
		}
	}
	return count
}

// bootClaimRig is the daemon's workflow runtime over the fixtures' board, with a runner of its
// own that drains the rows the boot queues on a clock the test holds.
type bootClaimRig struct {
	t      *testing.T
	pool   *pgxpool.Pool
	w      *workflowRuntime
	client *bootClaimDispatch
	runner *outbox
	clock  *heldClock
}

// newBootClaimRig seeds each fixture's record, the claims that attribute the sessions the board
// names, and the operator's hold where a fixture asks for one, and builds the runtime over the
// listing they make.
func newBootClaimRig(t *testing.T, fixtures []bootClaimFixture, answer func(int) error) *bootClaimRig {
	t.Helper()
	client := &bootClaimDispatch{outboxDispatch: &outboxDispatch{}, shown: map[string]dispatch.Issue{}, answer: answer}
	for _, fixture := range fixtures {
		client.listing = append(client.listing, dispatch.IssueSummary{Key: fixture.key, Title: fixture.key,
			Status: fixture.status, Rank: "A", HandedOver: true, ClaimedBy: fixture.listedClaim, ClaimUnread: fixture.listedUnread})
		client.shown[fixture.key] = dispatch.Issue{Key: fixture.key, Status: fixture.status,
			Claim: fixture.liveClaim, ClaimUnread: fixture.listedUnread && fixture.liveClaim == nil}
	}
	pool, w := catchUpRig(t, client, 1)
	w.nats = heldConsumers(bootClaimConsumers(t, pool))
	w.repositories = newRepositories("CAPTURE", ghrepo.Renames{})
	records := record.NewStore()
	for _, fixture := range fixtures {
		if fixture.recorded == nil {
			continue
		}
		putOutboxIssue(t, pool, records, *fixture.recorded)
		seedSessionClaim(t, pool, fixture.key+"-architect", "CAPTURE", fixture.key, fixture.key, bootOwnSession)
		seedCatchUp(t, pool, func(ctx context.Context, tx pgx.Tx, records *record.Postgres) error {
			if fixture.operatorHolds {
				token, err := claim.ProjectToken("CAPTURE")
				if err != nil {
					return err
				}
				_, err = records.OpenTreeLifecycle(ctx, tx, token, fixture.key, treelifecycle.AuthorityOperator)
				return err
			}
			if !fixture.slotted {
				return nil
			}
			if err := records.PutSlot(ctx, tx, record.Slot{Issue: fixture.key, Index: 0, AdmittedAt: catchUpClock()}); err != nil {
				return err
			}
			return openTreeTx(ctx, tx, "CAPTURE", fixture.key)
		})
	}
	seedSessionClaim(t, pool, "elsewhere-architect", "CAPTURE", bootOtherTree, bootOtherTree, bootOtherSession)
	clock := microClock()
	return &bootClaimRig{t: t, pool: pool, w: w, client: client, clock: clock,
		runner: &outbox{pool: pool, dispatchProject: "CAPTURE", records: records, dispatch: client,
			log: quietLogger(), now: clock.Now}}
}

// bootClaimConsumers opens this test's own durable consumers, which the reconcile reads the
// notification stream's target and the Dispatch consumer's position from.
func bootClaimConsumers(t *testing.T, pool *pgxpool.Pool) *intake.Consumers {
	t.Helper()
	conn, err := nats.Connect(workflowNATS(t), nats.Timeout(time.Second))
	if err != nil {
		t.Fatalf("connect NATS: %v", err)
	}
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	consumers, err := intake.OpenConsumers(context.Background(), js, pool, intake.ConsumerSpec{
		Project: "CAPTURE", Repositories: []ghrepo.Repository{ghrepo.MustParse("acme/widgets")},
		AckWait: time.Second, NakDelay: time.Millisecond, Logger: quietLogger(),
	})
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	return consumers
}

// boot runs the daemon's boot reconcile over the fixtures' listing.
func (r *bootClaimRig) boot() {
	r.t.Helper()
	if err := r.w.reconcile(context.Background()); err != nil {
		r.t.Fatalf("boot reconcile: %v", err)
	}
}

// queued is each issue the boot queued a Dispatch claim release for, in the order the rows were
// written.
func (r *bootClaimRig) queued() []string {
	r.t.Helper()
	rows, err := r.pool.Query(context.Background(), "select issue from outbox where kind = $1 order by id",
		string(record.OutboxKindClaimRelease))
	if err != nil {
		r.t.Fatalf("read the claim release rows: %v", err)
	}
	defer rows.Close()
	queued := []string{}
	for rows.Next() {
		var issue string
		if err := rows.Scan(&issue); err != nil {
			r.t.Fatalf("read the claim release rows: %v", err)
		}
		queued = append(queued, issue)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatalf("read the claim release rows: %v", err)
	}
	return queued
}

// drain runs every claim release row the boot queued to its end, moving the clock past each
// backoff a row that waits on its holder arms. The clock stays well inside the hour a release is
// given (claimReleaseWait), so nothing here gives up on a holder.
func (r *bootClaimRig) drain() {
	r.t.Helper()
	for range 8 {
		r.clock.advance(time.Minute)
		if err := r.runner.RunOnce(context.Background()); err != nil {
			r.t.Fatalf("drain the outbox: %v", err)
		}
		if len(r.queued()) == 0 {
			return
		}
	}
	r.t.Fatalf("claim release rows left after eight drains = %v, want every row ended", r.queued())
}

// TestBootReleasesParkedClaims is the boot's claim release pass (workflowRuntime's
// releaseParkedClaims) over the real store: which of a Dispatch listing's claims it queues a
// release row for, what each of those rows then does to the board, that a boot run again while a
// row still waits on its holder queues a second one, and that the repository recovery starts no
// pass at all.
func TestBootReleasesParkedClaims(t *testing.T) {
	t.Run("one boot over the board", testBootQueuesTheParkedClaims)
	t.Run("a second boot while the first row waits", testASecondBootQueuesASecondRelease)
	t.Run("the repository recovery", testTheRecoveryStartsNoClaimReleasePass)
}

// The boot queues a release row for each listed issue that carries a claim while the daemon's own
// record says it stopped, and for no other: not for a claim of a closed issue, which Dispatch
// released at the close; not for one of an issue the daemon runs; not for one of an issue it
// records nothing for; and not for a listing row that carries no claim at all, which the pass
// passes over before it reads anything. Which holder the claim has, and whose the tree is, are no
// part of the decision — the row's own executor reads both when it runs — so a person's claim, a
// session of another tree, a holder no claims row accounts for and a tree the operator holds each
// get a row whose DELETE is never sent.
func testBootQueuesTheParkedClaims(t *testing.T) {
	fixtures := bootClaimListing()
	rig := newBootClaimRig(t, fixtures, nil)
	rig.boot()

	want := []string{}
	for _, fixture := range fixtures {
		if fixture.queued {
			want = append(want, fixture.key)
		}
	}
	if got := rig.queued(); !slices.Equal(got, want) {
		t.Fatalf("the boot queued claim releases for %v, want %v", got, want)
	}

	rig.drain()
	for _, fixture := range fixtures {
		deletes := 0
		if fixture.deleted {
			deletes = 1
		}
		if got := rig.client.releasesOf(fixture.key); got != deletes {
			t.Fatalf("Dispatch was sent %d claim releases of %s, want %d", got, fixture.key, deletes)
		}
	}
}

// A boot that finds a release row of its own still waiting on a holder that has not stopped (a
// 409 ISSUE_CLAIMED) queues a second row all the same: the pass reads Dispatch's listing and the
// daemon's record, never the outbox, and a second row costs one read of an issue whose claim is
// already gone. Once the holder stops, one of the two rows takes the claim off the board and the
// other finds it gone and ends, so the board is left exactly as one row would have left it.
func testASecondBootQueuesASecondRelease(t *testing.T) {
	parked := bootClaimFixture{key: "CAPTURE-1", status: "backlog", listedClaim: "session:" + bootOwnSession,
		recorded: bootParked("CAPTURE-1"), liveClaim: sessionClaim(bootOwnSession)}
	// The holder answers the first DELETE still holding the claim; it has stopped by the next one.
	rig := newBootClaimRig(t, []bootClaimFixture{parked}, answersInTurn(claimConflict(dispatch.IssueClaimed)))
	rig.boot()
	rig.clock.advance(time.Minute)
	if err := rig.runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("drain the outbox: %v", err)
	}
	if got, want := rig.queued(), []string{parked.key}; !slices.Equal(got, want) {
		t.Fatalf("rows after the first boot's release waited on its holder = %v, want %v", got, want)
	}
	if got := rig.client.releasesOf(parked.key); got != 1 {
		t.Fatalf("Dispatch was sent %d claim releases before the second boot, want the one it refused", got)
	}

	rig.boot()
	if got, want := rig.queued(), []string{parked.key, parked.key}; !slices.Equal(got, want) {
		t.Fatalf("rows after the second boot = %v, want the waiting row and a second one", got)
	}

	rig.drain()
	if got := rig.client.releasesOf(parked.key); got != 2 {
		t.Fatalf("Dispatch was sent %d claim releases in all, want the refused one and the one it took", got)
	}
	if shown, _ := rig.client.GetIssue(context.Background(), parked.key); shown.Claim != nil {
		t.Fatalf("%s is still claimed by %s, want the claim off the board", parked.key, shown.Claim.Holder())
	}
}

// The repository recovery applies a listing of its own (recoverRepositories, which passes boot
// false), and starts no claim release pass: the pass is the boot's one-off, and a park the daemon
// sees queues its own row.
func testTheRecoveryStartsNoClaimReleasePass(t *testing.T) {
	fixtures := bootClaimListing()
	rig := newBootClaimRig(t, fixtures, nil)
	keep := func(map[string]*record.Issue, dispatch.IssueSummary) bool { return true }
	if err := rig.w.reconcileListing(context.Background(), rig.client.listing, keep, false); err != nil {
		t.Fatalf("recovery reconcile: %v", err)
	}
	if got := rig.queued(); len(got) != 0 {
		t.Fatalf("the recovery queued claim releases for %v, want none", got)
	}
}
