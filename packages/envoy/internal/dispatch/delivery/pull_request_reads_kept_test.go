package delivery

// The record of an owed read is what lets the intake let go, and a refusal ends what R4 keeps on
// evidence and never by its age (review and red team of cybermonklabs/legion#4209 at ff73c7358d):
// a read of GitHub that fails loses no head, and Dispatch keeps which pull request changed until a
// read succeeds, also for a pull request with no stored head.

import (
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/ratelimit"
)

// A read that fails holds none of the intake's intakeMaxAckPending in-flight slots, which every
// repository and every installation shares. acme/widgets is installation 1 and other/tools
// installation 2, and the client keeps GitHub's limits as the server's does
// (githubapp.WithLimits). Four `opened` envelopes of acme/widgets are published, then one of
// other/tools#5, whose head must be written within the 5 s of v-head-no-settings. The first case
// is the control, with no fault. In the second GitHub answers installation 1 with a rate limit
// (429, Retry-After 120): at ff73c7358d each acme envelope was handed back after the limit's
// wait and held its slot for it (the reviewer's B3 probe,
// TestReview6ALimitOnOneInstallationHoldsBackAnotherInstallationsHead). In the third GitHub
// answers acme/widgets with a 500: at ff73c7358d each envelope was handed back every 10 s up to
// its 30 deliveries. In both, each acme pull request is owed its read, so no head is lost.
func TestAFailedReadHoldsNoSlotThatOtherRepositoriesShare(t *testing.T) {
	for _, fault := range []string{"none", "rate_limit_on_installation_1", "server_error_of_acme_widgets"} {
		t.Run(fault, func(t *testing.T) {
			fastPageRetries(t)
			pool, ctx := deliveryTestPool(t)
			fake := newFakeGitHub(t)
			fake.installationOf = map[string]int64{"other/tools": 2}
			acme := serveGitHubPulls(t, fake, "acme/widgets")
			other := serveGitHubPulls(t, fake, "other/tools")
			client := fake.newTestClient(githubapp.WithLimits(ratelimit.New(pool)))
			at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
			for n := 1; n <= intakeMaxAckPending; n++ {
				acme.set("acme/widgets", n, "open", fakeSHA("a", n), fmt.Sprintf("feature/%d", n), "", at)
			}
			other.set("other/tools", 5, "open", fakeSHA("b", 5), "feature/5", "", at)
			acme.mu.Lock()
			acme.limited = fault == "rate_limit_on_installation_1"
			acme.failing = fault == "server_error_of_acme_widgets"
			acme.mu.Unlock()

			natsClient := intakeTestClient(t)
			startIntake(t, newIntake(natsClient, pool, deliveryTestLeases(t, pool), client, events.NewBroker(), loadedSettingsCache(t, ctx, pool)))
			awaitBoundDurable(t, natsClient)
			for n := 1; n <= intakeMaxAckPending; n++ {
				publishPullRequestEnvelope(t, natsClient, "acme", "widgets", n,
					prEnvelope("acme/widgets", n, "opened", fakeSHA("a", n), fmt.Sprintf("feature/%d", n), "", at))
			}
			time.Sleep(2 * time.Second)
			published := time.Now()
			publishPullRequestEnvelope(t, natsClient, "other", "tools", 5,
				prEnvelope("other/tools", 5, "opened", fakeSHA("b", 5), "feature/5", "", at))
			var took time.Duration
			for time.Since(published) < 15*time.Second {
				if headRowCount(t, ctx, &Intake{pool: pool}, "other/tools", 5) == 1 {
					took = time.Since(published)
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			var owedAcme int
			if err := pool.QueryRow(ctx, `select count(*) from pull_request_reads_owed where repo = 'acme/widgets'`).Scan(&owedAcme); err != nil {
				t.Fatal(err)
			}
			info := deliveryConsumerInfo(t, natsClient)
			t.Logf("fault=%s other_tools_5_head_after=%v (0 = none within 15 s) acme_owed=%d acme_pull_reads=%d num_ack_pending=%d num_pending=%d",
				fault, took.Round(time.Millisecond), owedAcme, acme.reads.Load(), info.NumAckPending, info.NumPending)
			if took == 0 || took > 5*time.Second {
				t.Errorf("other/tools#5, whose repository and installation have no fault, has no head within 5 s of its event (took %v; 0 is none within 15 s): the envelopes of acme/widgets hold the intake's slots", took)
			}
			if want := map[bool]int{true: 0, false: intakeMaxAckPending}[fault == "none"]; owedAcme != want {
				t.Errorf("acme/widgets is owed %d reads, want %d: each pull request whose read failed is kept", owedAcme, want)
			}
		})
	}
}

// The envelope is the only record of its change while the database refuses the owed read, so it
// is handed back for as long as that lasts, with no count of deliveries that ends it; once the
// database takes the record, the envelope is acknowledged, and the head is written once GitHub
// answers, with no new event. GitHub answers 500 for the whole run, and another session holds
// pull_request_reads_owed locked over the first two deliveries, so each of their records waits out
// owedReadWriteTimeout and fails. At ff73c7358d the shipped cap of 30 kept handing the envelope
// back after the third delivery recorded its read; with a cap of 2, the second delivery
// acknowledged it with nothing recorded, and the head was never written.
func TestAnOwedRecordTheDatabaseRefusesAtSomeDeliveriesLosesNoHead(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	issue := seedHeadIssue(t, ctx, pool, 4212)
	const (
		number = 4212
		head   = "4212421242124212421242124212421242124212"
	)
	fake := newFakeGitHub(t)
	github := serveGitHubPulls(t, fake, "acme/widgets")
	client := fake.newTestClient()
	branch, body := "legion/"+issue, "Dispatch: "+issue
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	github.set("acme/widgets", number, "open", head, branch, body, at)
	github.mu.Lock()
	github.failing = true
	github.mu.Unlock()

	lock, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock.Exec(ctx, `lock table pull_request_reads_owed in access exclusive mode`); err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			_ = lock.Rollback(ctx)
		}
	}
	t.Cleanup(release)

	natsClient := intakeTestClient(t)
	startIntake(t, newIntake(natsClient, pool, deliveryTestLeases(t, pool), client, events.NewBroker(), loadedSettingsCache(t, ctx, pool)))
	awaitBoundDurable(t, natsClient)
	before := deliveryConsumerInfo(t, natsClient).Delivered.Consumer
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", number,
		prEnvelope("acme/widgets", number, "opened", head, branch, body, at))

	// The second delivery has been made; its record waits owedReadWriteTimeout and fails.
	waitFor(t, 60*time.Second, func() bool { return deliveryConsumerInfo(t, natsClient).Delivered.Consumer-before >= 2 }, func() string {
		return fmt.Sprintf("the envelope was not delivered a second time: %+v", deliveryConsumerInfo(t, natsClient))
	})
	time.Sleep(owedReadWriteTimeout + 3*time.Second)
	release()
	t.Logf("pull_request_reads_owed was locked over %d deliveries", deliveryConsumerInfo(t, natsClient).Delivered.Consumer-before)

	var info string
	waitFor(t, 60*time.Second, func() bool {
		stream, err := natsClient.JS().StreamInfo(bus.Stream)
		if err != nil {
			return false
		}
		consumer := deliveryConsumerInfo(t, natsClient)
		info = fmt.Sprintf("%+v", consumer)
		return consumer.AckFloor.Stream >= stream.State.LastSeq && consumer.NumAckPending == 0
	}, func() string {
		return "the intake did not acknowledge the envelope within 60 s of the database taking its owed read: " + info
	})
	delivered := deliveryConsumerInfo(t, natsClient).Delivered.Consumer - before
	owed := owedCount(t, ctx, pool, "acme/widgets", number)
	github.mu.Lock()
	github.failing = false
	github.mu.Unlock()
	NewReconcile(pool, client, events.NewBroker()).runOnce(ctx)
	attributed, err := HeadAttributedTo(ctx, pool, issue, head)
	t.Logf("deliveries=%d owed_after_acknowledgement=%d head_is_basis_after_one_pass=%v", delivered, owed, attributed)
	if err != nil || !attributed {
		t.Fatalf("HeadAttributedTo(%s, #%d's head) after GitHub answers again and one reconcile pass = %v, %v, want true: the database refused the owed record at some deliveries and took it at a later one", issue, number, attributed, err)
	}
	if owed != 1 {
		t.Errorf("#%d was owed %d read(s) when the envelope was acknowledged, want 1: the envelope may go only once its record is written", number, owed)
	}
}

// A refusal ends on evidence and never by its age (the red team's N1,
// TestRedTeamRefusalBeforeSuccess). GitHub's 404 is final where one answer of GitHub, asked with
// the token of the refused read, shows the repository readable and the pull request absent: then
// the repository has no such pull request, at one more call, a GraphQL answer. Where the App
// cannot read the repository, Dispatch cannot tell a missing pull request from one it is not let
// see, so the read stays owed, however old.
//
// The first case is the App not installed on acme/widgets for 6 minutes (the owed record dated by
// SQL), and then installed, as an owner adds a repository to an installation: at ff73c7358d the
// refusal settled at the pass after 5 minutes, and the head GitHub then answers was never written.
// The second is a number acme/widgets does not have, in a repository the App reads: at ff73c7358d
// it was read again until it had been owed 5 minutes. The third is the control of the cost of a
// read that stays: a number that never exists in a repository the App cannot read costs no call
// to the repository and one read of the row a pass.
func TestARefusalEndsOnEvidenceAndNeverByItsAge(t *testing.T) {
	type pass struct{ pullReads, absenceQueries int32 }
	for _, kase := range []string{"not_readable_for_6m_then_readable", "readable_and_the_number_does_not_exist", "not_readable_and_the_number_never_exists"} {
		t.Run(kase, func(t *testing.T) {
			fastPageRetries(t)
			pool, ctx := deliveryTestPool(t)
			issue := seedHeadIssue(t, ctx, pool, 99001)
			fake := newFakeGitHub(t)
			github := serveGitHubPulls(t, fake, "acme/widgets")
			client := fake.newTestClient()
			intake := newIntake(nil, pool, nil, client, nil, nil)
			now := time.Now().UTC().Truncate(time.Second)
			const (
				number = 99001
				head   = "9900199001990019900199001990019900199001"
			)
			branch, body := "legion/"+issue, "Dispatch: "+issue
			readable := kase == "readable_and_the_number_does_not_exist"
			if !readable {
				fake.notInstalled.Store("acme/widgets", true)
			}
			if _, err := intake.handlePullRequestEnvelope(ctx, nil, prEnvelope("acme/widgets", number, "opened", head, branch, body, now)); err != nil {
				t.Fatal(err)
			}
			if owed := owedCount(t, ctx, pool, "acme/widgets", number); owed != 1 {
				t.Fatalf("#%d is owed %d reads after the intake's refused read, want 1", number, owed)
			}
			// The record of a repository the App cannot read is dated 6 minutes old, past the
			// reconcile period that ended a refusal at ff73c7358d; the readable case's is fresh.
			if !readable {
				if _, err := pool.Exec(ctx, "update pull_request_reads_owed set owed_at = now() - interval '6 minutes' where number = $1", number); err != nil {
					t.Fatal(err)
				}
			}
			reconcile := NewReconcile(pool, client, events.NewBroker())
			runPass := func() pass {
				pulls, asked := github.reads.Load(), github.absenceQueries.Load()
				reconcile.runOnce(ctx)
				return pass{github.reads.Load() - pulls, github.absenceQueries.Load() - asked}
			}
			switch kase {
			case "not_readable_for_6m_then_readable":
				first := runPass()
				afterRefusal := owedCount(t, ctx, pool, "acme/widgets", number)
				fake.notInstalled.Delete("acme/widgets")
				github.set("acme/widgets", number, "open", head, branch, body, now)
				second := runPass()
				basis, err := HeadAttributedTo(ctx, pool, issue, head)
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("age=6m owed_after_refusal=%d first_pass=%+v second_pass=%+v head_rows=%d head_is_basis=%v owed_after=%d",
					afterRefusal, first, second, headRowCount(t, ctx, intake, "acme/widgets", number), basis, owedCount(t, ctx, pool, "acme/widgets", number))
				if afterRefusal != 1 || !basis {
					t.Errorf("R4: GitHub now answers #%d, but after a refusal of a repository the App could not read, owed %d, the head is no basis (%v)", number, afterRefusal, basis)
				}
			case "readable_and_the_number_does_not_exist":
				first := runPass()
				owed := owedCount(t, ctx, pool, "acme/widgets", number)
				t.Logf("first_pass=%+v owed_after=%d head_rows=%d", first, owed, headRowCount(t, ctx, intake, "acme/widgets", number))
				if owed != 0 || first != (pass{1, 1}) {
					t.Errorf("a number acme/widgets does not have is owed %d read(s) after the first pass, which made %+v calls; want none, at two calls: the pull request's and the one GraphQL answer", owed, first)
				}
			default:
				var reads int
				for i := 1; i <= 3; i++ {
					calls := runPass()
					if err := pool.QueryRow(ctx, "select reads from pull_request_reads_owed where number = $1", number).Scan(&reads); err != nil {
						t.Fatalf("pass %d: the read is no longer owed: %v", i, err)
					}
					t.Logf("pass=%d calls=%+v row_reads=%d", i, calls, reads)
					if calls != (pass{}) || reads != i {
						t.Errorf("pass %d made %+v calls to the repository and counts %d reads of the row, want no call and %d: at most one read a pass", i, calls, reads, i)
					}
				}
			}
		})
	}
}

// A rate limit on one installation ends no read of the reconcile's pass for another: after the
// change above every failed read is the reconcile's, so a pass that stopped at the first limit
// would hold every other installation's owed heads behind it. Twenty failures of acme/widgets
// (installation 1, which GitHub answers with a rate limit) are owed before one of other/tools
// (installation 2), more than the pass reads at once (reconcilePartialConcurrency). At
// ff73c7358d the pass returned at the first limit, and other/tools#5 had no head after it. The
// client keeps GitHub's limits as the server's does, so the limited installation's reads after
// the limit is recorded are refused before they are sent: GitHub is asked at most by the reads
// already in flight.
func TestARateLimitOnOneInstallationEndsNoReadOfTheReconcileForAnother(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	issue := seedHeadIssue(t, ctx, pool, 5)
	fake := newFakeGitHub(t)
	fake.installationOf = map[string]int64{"other/tools": 2}
	acme := serveGitHubPulls(t, fake, "acme/widgets")
	other := serveGitHubPulls(t, fake, "other/tools")
	client := fake.newTestClient(githubapp.WithLimits(ratelimit.New(pool)))
	acme.mu.Lock()
	acme.limited = true
	acme.mu.Unlock()
	const limited = 20
	for n := 1; n <= limited; n++ {
		if err := owePullRequestRead(ctx, pool, "acme/widgets", n, owedFailure, ""); err != nil {
			t.Fatal(err)
		}
	}
	head := fakeSHA("c", 5)
	other.set("other/tools", 5, "open", head, "legion/"+issue, "Dispatch: "+issue, time.Now().UTC().Add(-time.Minute).Truncate(time.Second))
	if err := owePullRequestRead(ctx, pool, "other/tools", 5, owedFailure, ""); err != nil {
		t.Fatal(err)
	}
	passErr := NewReconcile(pool, client, events.NewBroker()).reconcilePullRequestHeads(ctx, time.Now(), newAbsenceBudget())
	if _, ok := githubapp.AsRateLimit(passErr); !ok {
		t.Errorf("the head pass answered %v, want installation 1's rate limit: a pass that met a limit fails with it", passErr)
	}
	var counted int
	if err := pool.QueryRow(ctx, `select count(*) from pull_request_reads_owed where repo = 'acme/widgets' and reads = 1`).Scan(&counted); err != nil {
		t.Fatal(err)
	}
	rows := headRowCount(t, ctx, &Intake{pool: pool}, "other/tools", 5)
	t.Logf("limited_owed=%d acme_pull_reads=%d acme_rows_counted=%d other_pull_reads=%d other_head_rows=%d other_owed=%d",
		limited, acme.reads.Load(), counted, other.reads.Load(), rows, owedCount(t, ctx, pool, "other/tools", 5))
	if rows != 1 || owedCount(t, ctx, pool, "other/tools", 5) != 0 {
		t.Errorf("other/tools#5, whose installation no limit holds, has %d head rows after the pass, want 1: installation 1's limit ended the pass", rows)
	}
	if counted != limited || acme.reads.Load() > reconcilePartialConcurrency {
		t.Errorf("the pass counted %d of acme/widgets' %d owed reads and asked GitHub %d times, want every one counted and at most %d asks: the limit is obeyed, and kept", counted, limited, acme.reads.Load(), reconcilePartialConcurrency)
	}
}
