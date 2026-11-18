package delivery

// What ends an owed refusal, and what a pass spends on refusals, read through the reconcile's own
// pass (review and second reader of cybermonklabs/legion#4209 at ceeec40f46, D1 and N6): one
// answer of GitHub can show that a repository is readable and that it holds no such pull request,
// and that answer is a read that succeeded, after which Dispatch keeps nothing for that number.

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// One answer of GitHub, asked with the token of the read GitHub refused, is what ends an owed
// refusal, and no memory of the pass ends one (the second reader's D1). acme/widgets is owed a
// refusal of #99000, which it does not have, and of #99001, a real pull request with no stored
// head. In one pass GitHub refuses the read of #99000 and is asked about that refusal; then the
// installation loses the repository, and GitHub refuses the read of #99001, made with the token
// minted before, too. After the pass the installation has the repository again, with no new
// event. At ceeec40f46 the pass remembered the repository as readable from #99000's question and
// ended #99001's read on that memory, so nothing was owed after the pass and no later pass wrote
// #99001's head.
func TestAReadRefusedAfterTheRepositoryLeftTheInstallationStaysOwed(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	const (
		absent = 99000
		real   = 99001
	)
	issue := seedHeadIssue(t, ctx, pool, real)
	fake := newFakeGitHub(t)
	github := serveGitHubPulls(t, fake, "acme/widgets")
	client := fake.newTestClient()
	head := fakeSHA("d1", real)
	branch, body := "legion/"+issue, "Dispatch: "+issue
	github.set("acme/widgets", real, "open", head, branch, body, time.Now().UTC().Add(-time.Minute).Truncate(time.Second))
	for _, number := range []int{absent, real} {
		if err := owePullRequestRead(ctx, pool, "acme/widgets", number, owedRefusal, model.RefusedAsPullRequestNotFound); err != nil {
			t.Fatal(err)
		}
	}
	// The first read of #99001 waits until GitHub has been asked about #99000's refusal, in
	// whichever way the build asks, and then the installation loses the repository before
	// #99001's own answer is taken.
	asked := func() int32 { return github.absenceQueries.Load() + github.repoReads.Load() }
	github.mu.Lock()
	github.before = map[int]func(){real: sync.OnceFunc(func() {
		deadline := time.Now().Add(10 * time.Second)
		for asked() == 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if asked() == 0 {
			t.Errorf("#%d's read waited 10 s, and GitHub was never asked about #%d's refusal", real, absent)
		}
		github.mu.Lock()
		github.hidden = true
		github.mu.Unlock()
	})}
	github.mu.Unlock()

	reconcile := NewReconcile(pool, client, events.NewBroker())
	reconcile.runOnce(ctx)
	absentOwed, realOwed := owedCount(t, ctx, pool, "acme/widgets", absent), owedCount(t, ctx, pool, "acme/widgets", real)
	github.mu.Lock()
	github.hidden = false
	github.mu.Unlock()
	reconcile.runOnce(ctx)
	basis, err := HeadAttributedTo(ctx, pool, issue, head)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("after_pass_1: absent_owed=%d real_owed=%d absence_queries=%d repository_reads=%d; after_pass_2: real_head_rows=%d real_head_is_basis=%v real_owed=%d",
		absentOwed, realOwed, github.absenceQueries.Load(), github.repoReads.Load(),
		headRowCount(t, ctx, &Intake{pool: pool}, "acme/widgets", real), basis, owedCount(t, ctx, pool, "acme/widgets", real))
	if absentOwed != 0 {
		t.Errorf("#%d, a number the readable repository does not have, is owed %d read(s) after its first pass, want none: GitHub's one answer ended it", absent, absentOwed)
	}
	if realOwed != 1 || !basis {
		t.Errorf("R4: #%d is owed %d read(s) after the pass that read it while the installation had lost the repository, want 1, and its head is no basis after the installation has the repository again (%v): a memory of the pass ended a real pull request's read", real, realOwed, basis)
	}
}

// A pass looks an installation up once for each repository (passInstallations): GitHub's answer
// that the App is not installed on a repository costs the pass no further call, and the
// repository's rows stay owed, since an installation that comes back has the pull request again.
// 60 refusals of ghost/nowhere, which the App is not installed on, are read over three passes. At
// ceeec40f46 the reviewer's probe (TestReview7AnOwedRefusalOfARepositoryWithNoInstallation, N6)
// counted 15, 10 and 8 lookups. The control is 60 numbers acme/widgets, which the App reads, does
// not have: they end at their first reads, RefusedReadsPerPass of them a pass.
func TestARepositoryWithNoInstallationCostsAPassOneLookup(t *testing.T) {
	const rows = 60
	for _, repo := range []string{"acme/widgets", "ghost/nowhere"} {
		t.Run(strings.ReplaceAll(repo, "/", "_"), func(t *testing.T) {
			fastPageRetries(t)
			pool, ctx := deliveryTestPool(t)
			fake := newFakeGitHub(t)
			github := serveGitHubPulls(t, fake, "acme/widgets")
			fake.notInstalled.Store("ghost/nowhere", true)
			client := fake.newTestClient()
			// GitHub refuses a number that acme/widgets does not have with 404, and the
			// installation lookup of ghost/nowhere with 404.
			refusedAs := map[string]string{"acme/widgets": model.RefusedAsPullRequestNotFound, "ghost/nowhere": model.RefusedAsAppNotInstalled}[repo]
			for n := 1; n <= rows; n++ {
				if err := owePullRequestRead(ctx, pool, repo, n, owedRefusal, refusedAs); err != nil {
					t.Fatal(err)
				}
			}
			reconcile := NewReconcile(pool, client, events.NewBroker())
			for pass := 1; pass <= 3; pass++ {
				lookups, pulls, asked := fake.lookupsOf(repo), github.reads.Load(), github.absenceQueries.Load()
				reconcile.runOnce(ctx)
				var owed, read int
				if err := pool.QueryRow(ctx, `select count(*), coalesce(sum(reads), 0) from pull_request_reads_owed where repo = $1`, repo).Scan(&owed, &read); err != nil {
					t.Fatal(err)
				}
				lookedUp := fake.lookupsOf(repo) - lookups
				t.Logf("repo=%s pass=%d installation_lookups=%d pull_reads=%d absence_queries=%d owed_after=%d reads_counted=%d",
					repo, pass, lookedUp, github.reads.Load()-pulls, github.absenceQueries.Load()-asked, owed, read)
				if repo == "ghost/nowhere" {
					if lookedUp != 1 || owed != rows || read != pass*RefusedReadsPerPass {
						t.Errorf("pass %d looked ghost/nowhere's installation up %d times and leaves %d rows owed, read %d times in all; want 1 lookup, all %d rows, and %d reads: one lookup a pass, and no row ended",
							pass, lookedUp, owed, read, rows, pass*RefusedReadsPerPass)
					}
					continue
				}
				if want := max(rows-pass*RefusedReadsPerPass, 0); owed != want {
					t.Errorf("pass %d leaves %d of acme/widgets' rows owed, want %d: a number a readable repository does not have ends at its first read", pass, owed, want)
				}
			}
		})
	}
}
