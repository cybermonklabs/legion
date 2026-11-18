package delivery

// Review round 2 of cybermonklabs/legion#4209: what an owed read may wait behind (B5), and a read
// of this build whose answer is older than the row it replaces (B6). The first and third tests
// are the reviewer's probes, taken in under names of their own; the second states the order and
// the kinds of owed reads that close B5.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// fakeSHA is a 40-character commit made of prefix and n, padded with zeros.
func fakeSHA(prefix string, n int) string {
	s := fmt.Sprintf("%s%d", prefix, n)
	return s + strings.Repeat("0", 40-len(s))
}

// merge makes GitHub hold pull request number as merged.
func (g *githubPulls) merge(number int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pulls[number]["merged"] = true
	g.pulls[number]["state"] = "closed"
}

func owedCount(t *testing.T, ctx context.Context, pool *store.Pool, repo string, number int) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `select count(*) from pull_request_reads_owed where repo = $1 and number = $2`, repo, number).Scan(&count); err != nil {
		t.Fatalf("count the owed reads of %s#%d: %v", repo, number, err)
	}
	return count
}

// A pull request a GitHub outage owed a read is read by the reconcile's first pass after GitHub
// answers again, however many refusals were owed before the outage, where no pass ran during it.
// Every envelope that names a pull request GitHub does not have owes a refusal, and any Envoy /v1
// bearer can publish such envelopes, so the owed reads of refusals must
// take no read from the reads a failure owes. The forged envelopes go through the real intake and
// are owed before the real one; the real one's read fails as it does in an outage (a 500), and the
// delivery's own record owes it (Intake.oweEnvelopesRead). forged_0 is the control. The reviewer's probe
// (TestReview2OwedQueueRealReadWaitsBehindForgedRefusals) measured, before the fix:
// `forged=250 … pass_pull_reads=200 owed_after_pass=251 real_head_is_basis_after_one_pass=false`.
func TestAnOutagesOwedReadIsNotHeldBehindForgedRefusals(t *testing.T) {
	for _, forged := range []int{0, 250} {
		t.Run(fmt.Sprintf("forged_%d", forged), func(t *testing.T) {
			fastPageRetries(t)
			pool, ctx := deliveryTestPool(t)
			issue := seedHeadIssue(t, ctx, pool, 88001)
			fake := newFakeGitHub(t)
			github := serveGitHubPulls(t, fake, "acme/widgets")
			client := fake.newTestClient()
			intake := newIntake(nil, pool, nil, client, nil, nil)
			at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
			for i := range forged {
				n := 500000 + i
				if _, err := intake.handlePullRequestEnvelope(ctx, nil,
					prEnvelope("acme/widgets", n, "opened", fakeSHA("f", n), "feature/x", "", at)); err != nil {
					t.Fatalf("handle the forged envelope of #%d: %v", n, err)
				}
			}
			const real = 88001
			head := fakeSHA("88", real)
			branch, body := "legion/"+issue, "Dispatch: "+issue
			github.set("acme/widgets", real, "open", head, branch, body, at)
			github.mu.Lock()
			github.failing = true
			github.mu.Unlock()
			envelope := prEnvelope("acme/widgets", real, "opened", head, branch, body, at)
			if _, err := intake.handlePullRequestEnvelope(ctx, nil, envelope); err == nil {
				t.Fatal("the real envelope's read during the outage = nil, want ErrHeadNotStored")
			}
			raw := fmt.Sprintf(`{"kind":"pr","repo":"acme/widgets","number":"%d"}`, real)
			if _, _, err := intake.oweEnvelopesRead(ctx, raw); err != nil {
				t.Fatalf("owe the real pull request's read as its delivery does: %v", err)
			}
			github.mu.Lock()
			github.failing = false
			github.mu.Unlock()

			readsBefore := github.reads.Load()
			NewReconcile(pool, client, events.NewBroker()).runOnce(ctx)
			passReads := github.reads.Load() - readsBefore
			attributed, err := HeadAttributedTo(ctx, pool, issue, head)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("forged=%d pass_pull_reads=%d real_head_is_basis_after_one_pass=%v", forged, passReads, attributed)
			if !attributed {
				t.Fatalf("after the reconcile's first pass with %d older refusals owed, the pull request the outage owed has no head", forged)
			}
			if want := int32(1 + min(forged, RefusedReadsPerPass)); passReads != want {
				t.Errorf("the pass read GitHub %d times, want %d: the failure, and the refusals' own budget of %d", passReads, want, RefusedReadsPerPass)
			}
		})
	}
}

// The numbers of the round: 10,000 refusals owed, older than one failure. The first pass after
// GitHub answers again reads the failure and RefusedReadsPerPass refusals, and the failure's
// pull request has its head after it. The refusals are written straight into the table, as
// 10,000 forged envelopes through the intake leave them (the test above drives 250 through it).
// They name numbers acme/widgets does not have, in a repository the App reads, so each refusal
// the pass reads ends at that read (pullRequestAbsent).
func TestTenThousandOwedRefusalsTakeNoReadFromAnOwedFailure(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	issue := seedHeadIssue(t, ctx, pool, 88003)
	fake := newFakeGitHub(t)
	github := serveGitHubPulls(t, fake, "acme/widgets")
	client := fake.newTestClient()
	const forged = 10000
	if _, err := pool.Exec(ctx, `
		insert into pull_request_reads_owed (repo, number, reason, owed_at)
		select 'acme/widgets', 600000 + n, 'refusal', now() - interval '1 hour' + make_interval(secs => n)
		from generate_series(1, $1) as n
	`, forged); err != nil {
		t.Fatalf("owe %d refusals: %v", forged, err)
	}
	const real = 88003
	head := fakeSHA("88", real)
	branch, body := "legion/"+issue, "Dispatch: "+issue
	github.set("acme/widgets", real, "open", head, branch, body, time.Now().UTC().Add(-time.Minute).Truncate(time.Second))
	if err := owePullRequestRead(ctx, pool, "acme/widgets", real, owedFailure); err != nil {
		t.Fatalf("owe the failure: %v", err)
	}
	started := time.Now()
	NewReconcile(pool, client, events.NewBroker()).runOnce(ctx)
	took := time.Since(started)
	attributed, err := HeadAttributedTo(ctx, pool, issue, head)
	if err != nil {
		t.Fatal(err)
	}
	var left int
	if err := pool.QueryRow(ctx, `select count(*) from pull_request_reads_owed`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	t.Logf("refusals_owed=%d pass_pull_reads=%d pass_took=%s refusals_left=%d failure_head_is_basis=%v",
		forged, github.reads.Load(), took.Round(time.Millisecond), left, attributed)
	if !attributed {
		t.Fatalf("after one pass with %d refusals owed, the failure's pull request has no head", forged)
	}
	if reads, want := github.reads.Load(), int32(1+RefusedReadsPerPass); reads != want {
		t.Errorf("the pass read GitHub %d times, want %d", reads, want)
	}
	if want := forged - RefusedReadsPerPass; left != want {
		t.Errorf("%d refusals left after the pass, want %d: each refusal of a number a readable repository does not have ends at its read", left, want)
	}
}

// Inside each kind, the row read the fewest times goes first, then the one owed longest; and a
// read that gives the other answer changes the row's kind. #91 is owed a failure and the App is
// not installed on its repository now: it becomes a refusal, read once. #92 is owed a refusal and
// GitHub now fails it (500): it becomes a failure, read once. Neither is settled: a failure
// settles only at an answer, and a refusal only where the App can read the repository.
func TestOwedReadsGoFewestReadsFirstAndTakeTheKindOfTheirLastAnswer(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliveryProject(t, ctx, pool)
	for _, row := range []struct {
		number int
		reads  int
		age    time.Duration
	}{{81, 1, 3 * time.Minute}, {82, 0, 2 * time.Minute}, {83, 0, time.Minute}} {
		if _, err := pool.Exec(ctx, `
			insert into pull_request_reads_owed (repo, number, reason, reads, owed_at)
			values ('acme/widgets', $1, 'refusal', $2, now() - make_interval(secs => $3))
		`, row.number, row.reads, row.age.Seconds()); err != nil {
			t.Fatal(err)
		}
	}
	listed, err := listOwedPullRequestReads(ctx, pool, owedRefusal, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[0].Number != 82 || listed[1].Number != 83 {
		t.Fatalf("the two refusals listed first = %+v, want #82 then #83: no row's second read before every row's first", listed)
	}
	if _, err := pool.Exec(ctx, `delete from pull_request_reads_owed`); err != nil {
		t.Fatal(err)
	}

	fake := newFakeGitHub(t)
	serveGitHubPulls(t, fake, "acme/widgets")
	failing := serveGitHubPulls(t, fake, "acme/gadgets")
	failing.mu.Lock()
	failing.failing = true
	failing.mu.Unlock()
	if err := owePullRequestRead(ctx, pool, "acme/widgets", 91, owedFailure); err != nil {
		t.Fatal(err)
	}
	if err := owePullRequestRead(ctx, pool, "acme/gadgets", 92, owedRefusal); err != nil {
		t.Fatal(err)
	}
	fake.notInstalled.Store("acme/widgets", true)
	NewReconcile(pool, fake.newTestClient(), events.NewBroker()).runOnce(ctx)
	for _, want := range []struct {
		repo   string
		number int
		reason owedReason
	}{{"acme/widgets", 91, owedRefusal}, {"acme/gadgets", 92, owedFailure}} {
		var reason owedReason
		var reads int
		if err := pool.QueryRow(ctx, `select reason, reads from pull_request_reads_owed where repo = $1 and number = $2`,
			want.repo, want.number).Scan(&reason, &reads); err != nil {
			t.Fatalf("read the owed read of %s#%d: %v", want.repo, want.number, err)
		}
		if reason != want.reason || reads != 1 {
			t.Errorf("%s#%d after one pass is owed a %s read %d times, want a %s read once", want.repo, want.number, reason, reads, want.reason)
		}
	}
}

// A read of this build whose answer is older than the row it replaces does not leave that
// answer standing: during a roll the release before writes GitHub's newer answer, unmarked, and
// this build's slower read of an older answer lands after it. The mark still wins (a time no read
// of GitHub wrote never refuses GitHub's answer), and the same write owes the pull request a read
// of kind failure, so the reconcile's next pass reads GitHub again and writes its current answer.
// The older answer is open in one row and closed in the other: the reconcile reads no closed row
// and no open row read in the last five minutes, so only the owed read can repair either.
// The reviewer's probe (TestReview2AnOlderMarkedReadAfterANewerRow) measured, before the fix:
// "an answer of GitHub updated 17:51:34 replaced one updated 17:56:34".
func TestAnOlderReadOverANewerRowOfTheReleaseBeforeIsReadAgain(t *testing.T) {
	for _, olderState := range []string{"open", "closed"} {
		t.Run("older_answer_"+olderState, func(t *testing.T) {
			fastPageRetries(t)
			pool, ctx := deliveryTestPool(t)
			seedDeliveryProject(t, ctx, pool)
			fake := newFakeGitHub(t)
			github := serveGitHubPulls(t, fake, "acme/widgets")
			const number = 88002
			now := time.Now().UTC().Truncate(time.Second)
			t1, t2 := now.Add(-10*time.Minute), now.Add(-5*time.Minute)
			older, newer := fakeSHA("a", 1), fakeSHA("b", 2)
			github.set("acme/widgets", number, "closed", newer, "topic", "", t2)
			github.merge(number)
			writeHeadAsTheReleaseBefore(t, ctx, pool, PullRequestHead{Repo: "acme/widgets", Number: number,
				HeadSHA: newer, HeadRef: "topic", BaseRef: "main", State: "merged", EventAt: &t2, CheckedAt: &now},
				releaseBeforeAttribution{source: string(sourceNone), complete: true})
			if err := UpsertPullRequestHead(ctx, pool, PullRequestHead{Repo: "acme/widgets", Number: number,
				HeadSHA: older, HeadRef: "topic", BaseRef: "main", State: olderState,
				EventAt: &t1, CheckedAt: &now, GitHubReadAt: &now}); err != nil {
				t.Fatal(err)
			}
			NewReconcile(pool, fake.newTestClient(), events.NewBroker()).runOnce(ctx)
			got := readHead(t, ctx, pool, "acme/widgets", number)
			if got.HeadSHA != newer || got.State != "merged" {
				t.Fatalf("after the reconcile's next pass the row = head %s state %s, want GitHub's current %s merged: an answer updated %s replaced one updated %s and stands",
					got.HeadSHA[:4], got.State, newer[:4], t1.Format(time.TimeOnly), t2.Format(time.TimeOnly))
			}
			if owed := owedCount(t, ctx, pool, "acme/widgets", number); owed != 0 {
				t.Errorf("the pull request is still owed a read after GitHub answered it: %d rows", owed)
			}
		})
	}
}

// No order of the two builds' writes leaves a marked row holding facts this build never wrote,
// or an unmarked row that covers an event (round 2's reviewer, TestReview2MarkedRowHoldsOnlyThis
// BuildsFacts, 1,500 random writes). It also holds B6's rule in every order: a marked write that
// replaces an unmarked row whose stored time is later than the answer's owes the pull request a
// read. Two controls, each through -overlay on a fresh database: migration 20261116163242
// without its trigger fails the first assertion within a few steps, and migration
// 20261116182209 without its trigger fails the rule of B6.
func TestNoOrderOfTwoBuildsWritesLeavesAMarkedRowWithFactsThisBuildNeverWrote(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	rng := rand.New(rand.NewPCG(4209, 2))
	base := time.Now().UTC().Truncate(time.Second)
	states := []string{"open", "closed", "merged"}
	violations, steps, ops := 0, 0, map[string]int{}
	for seq := 0; seq < 150 && violations == 0; seq++ {
		number := 89000 + seq
		ours := map[string]bool{}
		for step := range 10 {
			steps++
			now := time.Now().UTC()
			switch op := rng.IntN(4); op {
			case 0, 1:
				ops["this_build"]++
				at := base.Add(time.Duration(rng.IntN(61)-30) * time.Minute)
				var priorMarked bool
				var priorAt *time.Time
				hadRow := pool.QueryRow(ctx, `select github_read_at is not null, event_at from pull_request_heads
					where repo = 'acme/widgets' and number = $1`, number).Scan(&priorMarked, &priorAt) == nil
				head := PullRequestHead{Repo: "acme/widgets", Number: number,
					HeadSHA: fakeSHA("c", rng.IntN(1000)), State: states[rng.IntN(3)],
					EventAt: &at, CheckedAt: &now, GitHubReadAt: &now}
				ours[fmt.Sprintf("%s|%s|%d", head.HeadSHA, head.State, at.Unix())] = true
				if err := UpsertPullRequestHead(ctx, pool, head); err != nil {
					t.Fatal(err)
				}
				if hadRow && !priorMarked && priorAt != nil && priorAt.After(at) && owedCount(t, ctx, pool, "acme/widgets", number) == 0 {
					violations++
					t.Logf("seq %d step %d: a marked answer of %v replaced an unmarked row of %v and owes no read", seq, step, at, *priorAt)
				}
			case 2:
				ops["release_before_write"]++
				var at *time.Time
				switch rng.IntN(3) {
				case 1:
					v := base.Add(time.Duration(rng.IntN(61)-30) * time.Minute)
					at = &v
				case 2:
					v := base.Add(2 * time.Hour)
					at = &v
				}
				var checked *time.Time
				if rng.IntN(2) == 0 {
					checked = &now
				}
				writeHeadAsTheReleaseBefore(t, ctx, pool, PullRequestHead{Repo: "acme/widgets", Number: number,
					HeadSHA: fakeSHA("f", rng.IntN(1000)), State: states[rng.IntN(3)],
					EventAt: at, CheckedAt: checked},
					releaseBeforeAttribution{source: string(sourceNone), complete: rng.IntN(2) == 0})
			case 3:
				ops["release_before_stamp"]++
				if _, err := pool.Exec(ctx, `update pull_request_heads set checked_at = $3, attribution_rule = $4
					where repo = $1 and number = $2`, "acme/widgets", number, now, releaseBeforeAttributionRule); err != nil {
					t.Fatal(err)
				}
			}
			var marked bool
			var head, state string
			var at *time.Time
			if err := pool.QueryRow(ctx, `select github_read_at is not null, head_sha, state, event_at
				from pull_request_heads where repo = 'acme/widgets' and number = $1`, number).Scan(&marked, &head, &state, &at); err != nil {
				continue // no row yet: a stamp before any write
			}
			if marked {
				key := ""
				if at != nil {
					key = fmt.Sprintf("%s|%s|%d", head, state, at.Unix())
				}
				if !ours[key] {
					violations++
					t.Logf("seq %d step %d: a marked row holds facts this build never wrote: head %s state %s at %v", seq, step, head[:4], state, at)
				}
				continue
			}
			for _, probe := range []time.Time{base.Add(-time.Hour), base, base.Add(3 * time.Hour)} {
				covered, err := headReadCovers(ctx, pool, "acme/widgets", number, &probe)
				if err != nil {
					t.Fatal(err)
				}
				if covered {
					violations++
					t.Logf("seq %d step %d: an unmarked row covers an event at %v", seq, step, probe)
				}
			}
		}
	}
	t.Logf("steps=%d ops=%v violations=%d", steps, ops, violations)
	if violations > 0 {
		t.Errorf("%d violations in %d steps", violations, steps)
	}
}
