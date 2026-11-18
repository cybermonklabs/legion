package delivery

// No time value proves what a read of GitHub covers: an older event does not replace a newer head,
// and Dispatch keeps which pull request changed until a read succeeds. A read deletes an owed read
// only at the revision it read before it read GitHub, and
// an answer of the same GitHub second as the stored one covers no event and owes a read where its
// facts differ. Each test below is one reviewer's reproduction of cybermonklabs/legion#4209 at
// 57443e201d, under a name of its own, with its control.

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// statementKey carries a statement's text from TraceQueryStart to TraceQueryEnd.
type statementKey struct{}

// clockStepTracer stands in for a database clock that steps back by step during the intake's read
// of GitHub (the review of cybermonklabs/legion#4209 at 57443e201d, H1): once a write of a head row
// on the intake's pool has committed, it moves every owed read of the pull request back by step,
// which is the owed_at that the trigger pull_request_heads_older_read would have written inside
// that write from a clock step behind the one the read began by.
type clockStepTracer struct {
	t      *testing.T
	pool   *store.Pool
	repo   string
	number int
	step   time.Duration
}

func (c clockStepTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, statementKey{}, data.SQL)
}

func (c clockStepTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	statement, _ := ctx.Value(statementKey{}).(string)
	if c.step == 0 || data.Err != nil || !strings.Contains(statement, "insert into pull_request_heads") {
		return
	}
	if _, err := c.pool.Exec(context.WithoutCancel(ctx), `
		update pull_request_reads_owed set owed_at = owed_at - make_interval(secs => $3)
		where repo = $1 and number = $2
	`, c.repo, c.number, c.step.Seconds()); err != nil {
		c.t.Errorf("step the owed read's clock back: %v", err)
	}
}

// pauseTracer holds the first statement whose text holds match until release is closed, and
// says on arrived that it got there.
type pauseTracer struct {
	match   string
	arrived chan<- struct{}
	release <-chan struct{}
}

func (p pauseTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, p.match) {
		select {
		case p.arrived <- struct{}{}:
			<-p.release
		default:
		}
	}
	return ctx
}

func (pauseTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// waitOn fails the test where ch gives nothing within a minute.
func waitOn(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Minute):
		t.Fatalf("%s did not happen within a minute", what)
	}
}

// An owe that a database clock behind the read's dates before the read began outlives that read
// (the review at 57443e201d, H1). During a roll the release before stores GitHub's newer answer,
// closed, from an envelope; this build's slower read lands GitHub's older answer, open, over it,
// and the trigger owes the pull request a read in that write. The database's clock then reads
// 3 s behind the moment the read began. At 57443e201d the intake's delete drew its line from the
// clock at the read's start less 1 s, so it deleted that owe, and the older open head stayed a
// basis after the reconcile's next pass (the reviewer's probe, TestReview5ABackwardClockStepDuring
// TheRead: `owed_after_intake=false`). The control steps the clock by 0 s.
func TestAnOweThatADatabaseClockBehindTheReadDatesEarlierOutlivesTheRead(t *testing.T) {
	for _, step := range []time.Duration{0, 3 * time.Second} {
		t.Run(fmt.Sprintf("clock_step_back_%s", step), func(t *testing.T) {
			fastPageRetries(t)
			pool, ctx := deliveryTestPool(t)
			const number = 88601
			issue := seedHeadIssue(t, ctx, pool, number)
			fake := newFakeGitHub(t)
			github := serveGitHubPulls(t, fake, "acme/widgets")
			client := fake.newTestClient()
			traced := storetest.Traced(t, pool, clockStepTracer{t: t, pool: pool, repo: "acme/widgets", number: number, step: step}, nil)
			intake := newIntake(nil, traced, nil, client, nil, nil)
			older, newer := fakeSHA("a", number), fakeSHA("b", number)
			branch, body := "legion/"+issue, "Dispatch: "+issue
			now := time.Now().UTC().Truncate(time.Second)
			t1, t2 := now.Add(-10*time.Minute), now.Add(-5*time.Minute)
			writeHeadAsTheReleaseBefore(t, ctx, pool, PullRequestHead{Repo: "acme/widgets", Number: number,
				HeadSHA: newer, HeadRef: branch, BaseRef: "main", State: "closed", EventAt: &t2, CheckedAt: &now},
				releaseBeforeAttribution{issue: &issue, source: string(sourceBranch), complete: true})
			github.set("acme/widgets", number, "open", older, branch, body, t1)
			if _, err := intake.handlePullRequestEnvelope(ctx, nil,
				prEnvelope("acme/widgets", number, "synchronize", older, branch, body, t1)); err != nil {
				t.Fatal(err)
			}
			owedAfterIntake := owedCount(t, ctx, pool, "acme/widgets", number)

			github.set("acme/widgets", number, "closed", newer, branch, body, t2)
			NewReconcile(pool, client, events.NewBroker()).runOnce(ctx)
			got := readHead(t, ctx, pool, "acme/widgets", number)
			olderBasis, err := HeadAttributedTo(ctx, pool, issue, older)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("clock_step_back=%s owed_after_intake=%d final_head=%s state=%s older_head_is_basis=%v",
				step, owedAfterIntake, got.HeadSHA[:6], got.State, olderBasis)
			if owedAfterIntake != 1 {
				t.Errorf("after the intake wrote the older answer over the newer row, %d reads are owed, want the one its own write owed", owedAfterIntake)
			}
			if got.HeadSHA != newer || got.State != "closed" || olderBasis {
				t.Errorf("after the reconcile's next pass the row is head %s state %s and the older head is a basis = %v, want GitHub's current %s closed and no basis at %s",
					got.HeadSHA[:6], got.State, olderBasis, newer[:6], older[:6])
			}
		})
	}
}

// An owe that a writer's transaction makes after the intake's read began outlives that read,
// however long before the read the transaction began (the deep audit at 57443e201d, finding 1).
// A stored row the release before wrote says open at an older head. Writer A holds GitHub's older
// answer, open there, and begins UpsertPullRequestHead's transaction; it waits there, as a writer
// that waits on a connection or a lock does. Intake B begins its read of GitHub, and GitHub's
// answer to it is still the older one. Before B's answer arrives, the release before writes
// GitHub's newer answer, closed. A then replaces that row with its older answer, and the trigger
// owes the pull request a read inside A's write. B then writes the older answer too. At 57443e201d
// the owe took owed_at from now(), the start of A's transaction, so where A's transaction began
// more than 1 s before B's read, B's delete removed it, and after the reconcile's next pass the
// closed pull request's older head was a basis (the auditor's TestPairConcurrentBackdatedOwedRead:
// `wait=2s owed_after_B=0 final_state=open older_head_is_basis=true`). The control's transaction
// begins 0 s before B's read.
func TestAnOweOfATransactionThatBeganBeforeTheReadOutlivesTheRead(t *testing.T) {
	for _, age := range []time.Duration{0, 2 * time.Second} {
		t.Run(fmt.Sprintf("transaction_age_%s", age), func(t *testing.T) {
			fastPageRetries(t)
			pool, ctx := deliveryTestPool(t)
			const number = 88602
			issue := seedHeadIssue(t, ctx, pool, number)
			older, newer := fakeSHA("a", number), fakeSHA("b", number)
			branch, body := "legion/"+issue, "Dispatch: "+issue
			now := time.Now().UTC().Truncate(time.Second)
			t1, t2 := now.Add(-10*time.Minute), now.Add(-5*time.Minute)
			fake := newFakeGitHub(t)
			bReading, bRelease := make(chan struct{}, 1), make(chan struct{})
			github := serveGitHubPulls(t, fake, "acme/widgets")
			github.holdFirst = func() {
				bReading <- struct{}{}
				<-bRelease
			}
			client := fake.newTestClient()
			github.set("acme/widgets", number, "open", older, branch, body, t1)
			writeHeadAsTheReleaseBefore(t, ctx, pool, PullRequestHead{Repo: "acme/widgets", Number: number,
				HeadSHA: older, HeadRef: branch, BaseRef: "main", State: "open", EventAt: &t1, CheckedAt: &now},
				releaseBeforeAttribution{issue: &issue, source: string(sourceBranch), complete: true})

			// Writer A writes GitHub's answer with its texts, as each writer of this build does, so
			// its write is one transaction; it pauses at the upsert of pull_request_heads, the first
			// statement of that transaction.
			aWaiting, aRelease := make(chan struct{}, 1), make(chan struct{})
			poolA := storetest.Traced(t, pool, pauseTracer{match: "insert into pull_request_heads", arrived: aWaiting, release: aRelease}, nil)
			aDone := make(chan error, 1)
			go func() {
				title := fmt.Sprintf("Change %d", number)
				aDone <- UpsertPullRequestHead(ctx, poolA, PullRequestHead{Repo: "acme/widgets",
					Number: number, HeadSHA: older, HeadRef: branch, BaseRef: "main", State: "open", Title: title,
					URL: fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number), EventAt: &t1, CheckedAt: &now,
					GitHubReadAt: &now, Texts: &PullRequestTexts{HeadRepo: "acme/widgets", BaseRef: "main",
						Title: title, Body: body, CommitMessages: []string{}, MessagesRead: true}})

			}()
			waitOn(t, aWaiting, "writer A's transaction")
			time.Sleep(age)

			intakeB := newIntake(nil, pool, nil, client, nil, nil)
			bDone := make(chan error, 1)
			go func() {
				_, err := intakeB.handlePullRequestEnvelope(ctx, nil,
					prEnvelope("acme/widgets", number, "synchronize", older, branch, body, t1))
				bDone <- err
			}()
			waitOn(t, bReading, "intake B's read of GitHub")
			github.set("acme/widgets", number, "closed", newer, branch, body, t2)
			writeHeadAsTheReleaseBefore(t, ctx, pool, PullRequestHead{Repo: "acme/widgets", Number: number,
				HeadSHA: newer, HeadRef: branch, BaseRef: "main", State: "closed", EventAt: &t2, CheckedAt: &now},
				releaseBeforeAttribution{issue: &issue, source: string(sourceBranch), complete: true})
			close(aRelease)
			if err := <-aDone; err != nil {
				t.Fatal(err)
			}
			owedBeforeB := owedCount(t, ctx, pool, "acme/widgets", number)
			close(bRelease)
			if err := <-bDone; err != nil {
				t.Fatal(err)
			}
			owedAfterB := owedCount(t, ctx, pool, "acme/widgets", number)

			NewReconcile(pool, client, events.NewBroker()).runOnce(ctx)
			got := readHead(t, ctx, pool, "acme/widgets", number)
			olderBasis, err := HeadAttributedTo(ctx, pool, issue, older)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("transaction_age=%s owed_before_B_writes=%d owed_after_B=%d final_head=%s state=%s older_head_is_basis=%v",
				age, owedBeforeB, owedAfterB, got.HeadSHA[:6], got.State, olderBasis)
			if owedBeforeB != 1 || owedAfterB != 1 {
				t.Errorf("A's write owed %d reads and B's write left %d, want the one A's write owed kept through B's", owedBeforeB, owedAfterB)
			}
			if got.HeadSHA != newer || got.State != "closed" || olderBasis {
				t.Errorf("after the reconcile's next pass the row is head %s state %s and the older head is a basis = %v, want GitHub's current %s closed and no basis at %s",
					got.HeadSHA[:6], got.State, olderBasis, newer[:6], older[:6])
			}
		})
	}
}

// A snapshot of GitHub that is written late covers no later event of its own second (the deep
// audit at 57443e201d, finding 2). GitHub answers the intake's read of a close, and reopens the
// pull request in the same second as soon as it has taken that answer; the answer then takes
// delay to arrive. The reopen's own event follows, of the same second. At 57443e201d a stored
// answer covered an event of its own second where it was written 5 s after that second, so the
// late write of the closed answer spared the reopen's read, nothing was owed, the reconcile reads
// no closed row, and the reopened pull request was no basis (the auditor's TestPairLateWriteDoes
// NotCoverANewerSameSecondEvent: `pull_reads=1 … reopened_head_is_basis=false`). The control's
// answer arrives at once.
func TestALateWriteOfASnapshotCoversNoLaterEventOfItsSecond(t *testing.T) {
	for _, delay := range []time.Duration{0, 6 * time.Second} {
		t.Run(fmt.Sprintf("response_delay_%s", delay), func(t *testing.T) {
			fastPageRetries(t)
			pool, ctx := deliveryTestPool(t)
			const number = 88603
			issue := seedHeadIssue(t, ctx, pool, number)
			head := fakeSHA("c", number)
			branch, body := "legion/"+issue, "Dispatch: "+issue
			second := time.Now().UTC().Truncate(time.Second)
			fake := newFakeGitHub(t)
			github := serveGitHubPulls(t, fake, "acme/widgets")
			github.holdFirst = func() {
				github.set("acme/widgets", number, "open", head, branch, body, second)
				time.Sleep(delay)
			}
			github.set("acme/widgets", number, "closed", head, branch, body, second)
			client := fake.newTestClient()
			intake := newIntake(nil, pool, nil, client, nil, nil)
			for _, action := range []string{"closed", "reopened"} {
				if _, err := intake.handlePullRequestEnvelope(ctx, nil,
					prEnvelope("acme/widgets", number, action, head, branch, body, second)); err != nil {
					t.Fatalf("handle the %s event: %v", action, err)
				}
			}
			NewReconcile(pool, client, events.NewBroker()).runOnce(ctx)
			got := readHead(t, ctx, pool, "acme/widgets", number)
			basis, err := HeadAttributedTo(ctx, pool, issue, head)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("response_delay=%s pull_reads=%d state_after_reopened_and_reconcile=%s reopened_head_is_basis=%v owed=%d",
				delay, github.reads.Load(), got.State, basis, owedCount(t, ctx, pool, "acme/widgets", number))
			if got.State != "open" || !basis {
				t.Errorf("after the reopen's event and the reconcile's next pass the row is %s and its head is a basis = %v, want GitHub's open and a basis", got.State, basis)
			}
		})
	}
}

// A snapshot of GitHub that lands after a newer answer of its own second leaves the pull request
// owed a read (cell 16 of the interleavings in the report of this change). Intake A reads the
// close of a pull request, and GitHub reopens it in the same second as soon as it has taken that
// answer. Before A's answer lands, intake B, another holder of the durable during a hold change,
// reads the reopen's event and writes the open answer. A's closed answer then lands: the guard
// applies an answer of the stored second, and nothing tells which of the two is the newer. Nothing
// reads a closed row again, so without an owed read the reopened pull request stays closed and its
// head is no basis. The control's closed answer lands before B reads.
func TestASnapshotThatLandsAfterANewerAnswerOfItsSecondIsReadAgain(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(map[bool]string{false: "closed_answer_lands_first", true: "closed_answer_lands_last"}[late], func(t *testing.T) {
			fastPageRetries(t)
			pool, ctx := deliveryTestPool(t)
			const number = 88604
			issue := seedHeadIssue(t, ctx, pool, number)
			head := fakeSHA("d", number)
			branch, body := "legion/"+issue, "Dispatch: "+issue
			second := time.Now().UTC().Truncate(time.Second)
			earlier := second.Add(-10 * time.Minute)
			fake := newFakeGitHub(t)
			aReading, aRelease := make(chan struct{}, 1), make(chan struct{})
			github := serveGitHubPulls(t, fake, "acme/widgets")
			github.holdFirst = func() {
				github.set("acme/widgets", number, "open", head, branch, body, second)
				aReading <- struct{}{}
				<-aRelease
			}
			github.set("acme/widgets", number, "closed", head, branch, body, second)
			client := fake.newTestClient()
			if err := UpsertPullRequestHead(ctx, pool, PullRequestHead{Repo: "acme/widgets", Number: number,
				HeadSHA: head, HeadRef: branch, BaseRef: "main", State: "open", EventAt: &earlier,
				CheckedAt: &earlier, GitHubReadAt: &earlier, Texts: &PullRequestTexts{HeadRepo: "acme/widgets",
					BaseRef: "main", Body: body, CommitMessages: []string{}, MessagesRead: true}}); err != nil {
				t.Fatal(err)
			}
			intakeA, intakeB := newIntake(nil, pool, nil, client, nil, nil), newIntake(nil, pool, nil, client, nil, nil)
			aDone := make(chan error, 1)
			go func() {
				_, err := intakeA.handlePullRequestEnvelope(ctx, nil, prEnvelope("acme/widgets", number, "closed", head, branch, body, second))
				aDone <- err
			}()
			waitOn(t, aReading, "intake A's read of GitHub")
			if !late {
				close(aRelease)
				if err := <-aDone; err != nil {
					t.Fatal(err)
				}
			}
			if _, err := intakeB.handlePullRequestEnvelope(ctx, nil, prEnvelope("acme/widgets", number, "reopened", head, branch, body, second)); err != nil {
				t.Fatal(err)
			}
			if late {
				close(aRelease)
				if err := <-aDone; err != nil {
					t.Fatal(err)
				}
			}
			owedAfterBoth := owedCount(t, ctx, pool, "acme/widgets", number)
			NewReconcile(pool, client, events.NewBroker()).runOnce(ctx)
			got := readHead(t, ctx, pool, "acme/widgets", number)
			basis, err := HeadAttributedTo(ctx, pool, issue, head)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("late=%v owed_after_both=%d pull_reads=%d final_state=%s reopened_head_is_basis=%v owed_after_pass=%d",
				late, owedAfterBoth, github.reads.Load(), got.State, basis, owedCount(t, ctx, pool, "acme/widgets", number))
			if got.State != "open" || !basis {
				t.Errorf("after both answers and the reconcile's next pass the row is %s and its head is a basis = %v, want GitHub's open and a basis", got.State, basis)
			}
		})
	}
}

// An answer whose read of GitHub started before the stored row's read never replaces that row
// where the two carry the same updated_at second: GitHub's updated_at is whole seconds, so the
// order the answers arrive in cannot tell which is the newer, and a read that started later holds
// the later state: an older event does not replace a newer head.
// The slow read is each of the three writers of a head row in turn: the intake, the reconcile's
// refresh, and the read a result starts (ReadThroughCommitPulls). Its read takes GitHub's answer
// at head A; GitHub then moves the pull request to head B in the same second, and the answer is
// held. Meanwhile the intake reads the event of B and stores B; then the held answer A lands. The
// row must hold B at once, not after a reconcile pass, and the read stays owed, the repair for
// what no order can decide. The control lands A before the intake reads B, in head order. At
// 3a21edb2c9 the answer that landed last won, so the row held A until the next pass, and a pass at
// A stood 158 s and 214 s (acceptance of cybermonklabs/legion#4209 at 3a21edb2c9, D2).
func TestAnAnswerWhoseReadStartedEarlierNeverReplacesTheHeadOfItsSecond(t *testing.T) {
	const repo = "acme/widgets"
	for _, writer := range []struct {
		name string
		// commits is whether the slow read is GET /repos/{o}/{r}/commits/{sha}/pulls, and not the
		// pull request's own read.
		commits bool
		read    func(ctx context.Context, pool *store.Pool, client *githubapp.Client, number int, head string, second time.Time) error
	}{
		{name: "the_intake", read: func(ctx context.Context, pool *store.Pool, client *githubapp.Client, number int, head string, second time.Time) error {
			_, err := newIntake(nil, pool, nil, client, nil, nil).handlePullRequestEnvelope(ctx, nil,
				prEnvelope(repo, number, "synchronize", head, "feature/x", "", second))
			return err
		}},
		{name: "the_reconcile", read: func(ctx context.Context, pool *store.Pool, client *githubapp.Client, number int, _ string, _ time.Time) error {
			_, _, err := RefreshPullRequestHead(ctx, pool, client, nil, repo, number, time.Now())
			return err
		}},
		{name: "the_read_a_result_starts", commits: true, read: func(ctx context.Context, pool *store.Pool, client *githubapp.Client, _ int, head string, _ time.Time) error {
			_, _, err := ReadThroughCommitPulls(ctx, pool, client, repo, head, time.Now())
			return err
		}},
	} {
		for _, slow := range []bool{true, false} {
			t.Run(writer.name+map[bool]string{true: "_slow_read_of_the_older_head", false: "_in_head_order"}[slow], func(t *testing.T) {
				fastPageRetries(t)
				pool, ctx := deliveryTestPool(t)
				const number = 88606
				older, newer := fakeSHA("a", number), fakeSHA("b", number)
				second := time.Now().UTC().Truncate(time.Second)
				fake := newFakeGitHub(t)
				github := serveGitHubPulls(t, fake, repo)
				github.set(repo, number, "open", older, "feature/x", "", second)
				taken, release := make(chan struct{}, 1), make(chan struct{})
				// hold runs once the slow read's answer A is taken: GitHub moves to B in the same
				// second, and the answer waits for release.
				hold := func() {
					github.set(repo, number, "open", newer, "feature/x", "", second)
					taken <- struct{}{}
					<-release
				}
				if writer.commits {
					var once sync.Once
					fake.handle("GET /repos/"+repo+"/commits/{sha}/pulls", func(w http.ResponseWriter, _ *http.Request) {
						github.mu.Lock()
						answer := maps.Clone(github.pulls[number])
						github.mu.Unlock()
						once.Do(hold)
						mustEncode(t, w, []any{answer})
					})
				} else {
					github.holdFirst = hold
				}
				client := fake.newTestClient()
				done := make(chan error, 1)
				go func() { done <- writer.read(ctx, pool, client, number, older, second) }()
				waitOn(t, taken, "the slow read's answer")
				if !slow {
					close(release)
					if err := <-done; err != nil {
						t.Fatal(err)
					}
				}
				if _, err := newIntake(nil, pool, nil, client, nil, nil).handlePullRequestEnvelope(ctx, nil,
					prEnvelope(repo, number, "synchronize", newer, "feature/x", "", second)); err != nil {
					t.Fatalf("the intake's read of the event of B: %v", err)
				}
				if slow {
					close(release)
					if err := <-done; err != nil {
						t.Fatal(err)
					}
				}
				got := readHead(t, ctx, pool, repo, number)
				owed := owedCount(t, ctx, pool, repo, number)
				t.Logf("writer=%s slow=%v head_after_both=%.12s owed_after_both=%d pull_reads=%d", writer.name, slow, got.HeadSHA, owed, github.reads.Load())
				if got.HeadSHA != newer {
					t.Errorf("after both answers of second %s the row holds %.12s, want B, %.12s: the answer of the read that started first replaced the newer head", second.Format(time.RFC3339), got.HeadSHA, newer)
				}
				if owed != 1 {
					t.Errorf("after two answers of one second with two heads, %d reads are owed, want 1: the owed read is the repair for what no order can decide", owed)
				}
			})
		}
	}
}

// A read whose revision of the owed read cannot be read in its bound takes no revision and deletes
// nothing, and its head is still written; the record it could not read stays, and the reconcile's
// next pass reads the pull request once more and settles it. Another session holds
// pull_request_reads_owed locked in ACCESS EXCLUSIVE mode while the intake handles the event, as
// a migration of that table would, so the read of the revision waits on the lock until its bound
// ends; the head read must not wait with it. The control holds no lock: the intake's read takes
// the revision and deletes the record, and the reconcile reads nothing.
func TestAReadThatCannotReadTheOwedRevisionDeletesNothingAndTheReconcileReadsAgain(t *testing.T) {
	for _, locked := range []bool{false, true} {
		t.Run(map[bool]string{false: "owed_table_free", true: "owed_table_locked"}[locked], func(t *testing.T) {
			fastPageRetries(t)
			pool, ctx := deliveryTestPool(t)
			const number = 88605
			issue := seedHeadIssue(t, ctx, pool, number)
			head := fakeSHA("f", number)
			branch, body := "legion/"+issue, "Dispatch: "+issue
			at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
			fake := newFakeGitHub(t)
			github := serveGitHubPulls(t, fake, "acme/widgets")
			github.set("acme/widgets", number, "open", head, branch, body, at)
			client := fake.newTestClient()
			if err := owePullRequestRead(ctx, pool, "acme/widgets", number, owedFailure, ""); err != nil {
				t.Fatal(err)
			}
			release := func() {}
			if locked {
				lock, err := pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := lock.Exec(ctx, `lock table pull_request_reads_owed in access exclusive mode`); err != nil {
					t.Fatal(err)
				}
				release = func() { _ = lock.Rollback(ctx) }
				t.Cleanup(release)
			}
			handling, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			_, err := newIntake(nil, pool, nil, client, nil, nil).handlePullRequestEnvelope(handling, nil,
				prEnvelope("acme/widgets", number, "synchronize", head, branch, body, at))
			release()
			if err != nil {
				t.Fatalf("the intake's handling with the owed table locked=%v: %v", locked, err)
			}
			written := readHead(t, ctx, pool, "acme/widgets", number)
			owedAfterIntake, readsAfterIntake := owedCount(t, ctx, pool, "acme/widgets", number), github.reads.Load()
			NewReconcile(pool, client, events.NewBroker()).runOnce(ctx)
			owedAfterPass, readsAfterPass := owedCount(t, ctx, pool, "acme/widgets", number), github.reads.Load()
			t.Logf("owed_table_locked=%v head_written=%v owed_after_intake=%d pull_reads_after_intake=%d owed_after_pass=%d pull_reads_after_pass=%d",
				locked, written.HeadSHA == head, owedAfterIntake, readsAfterIntake, owedAfterPass, readsAfterPass)
			if written.HeadSHA != head {
				t.Errorf("the intake wrote head %s, want GitHub's %s", written.HeadSHA, head)
			}
			wantOwed, wantPassReads := 0, int32(0)
			if locked {
				wantOwed, wantPassReads = 1, 1
			}
			if owedAfterIntake != wantOwed || readsAfterPass-readsAfterIntake != wantPassReads || owedAfterPass != 0 {
				t.Errorf("owed after the intake %d (want %d), the reconcile read the pull request %d more times (want %d), owed after the pass %d (want 0)",
					owedAfterIntake, wantOwed, readsAfterPass-readsAfterIntake, wantPassReads, owedAfterPass)
			}
		})
	}
}
