package delivery

// Which stored time a read of GitHub may trust: only one that a read of GitHub wrote (the head
// and the state of a pull request come from a read of GitHub). A release before that change wrote
// a row's head, state and updated_at from a
// `pull_request` envelope, whose fields a caller sets, and a rolling deploy runs that release
// beside this one on one database, so it can still write such a row after the migration has run.
// Such a row must stop no read of GitHub and win against none of GitHub's answers.

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The red team's probe of cybermonklabs/legion#4209 at 4b2d1e83f85c, taken in unchanged but for
// its layout. A stored row the release before wrote, at a stale open head, against GitHub's
// answer that the pull request is closed at another head. The control is a row with an honest,
// older time, which the event and the refresh both repair. The other row differs in its time
// alone, an hour ahead: before the fix, the event read GitHub 0 times, the refresh read it once
// and changed nothing, and the stale head stayed a basis.
func TestRedTeamLegacyFutureRowVersusGitHub(t *testing.T) {
	for _, legacyFuture := range []bool{false, true} {
		name := "honest_old_row_control"
		if legacyFuture {
			name = "legacy_future_event_time"
		}
		t.Run(name, func(t *testing.T) {
			fastPageRetries(t)
			pool, ctx := deliveryTestPool(t)
			issue := seedHeadIssue(t, ctx, pool, 98767)
			fake := newFakeGitHub(t)
			gh := serveGitHubPulls(t, fake, "acme/widgets")
			client := fake.newTestClient()
			intake := newIntake(nil, pool, nil, client, nil, nil)
			now := time.Now().UTC().Truncate(time.Second)
			eventAt := now.Add(-time.Minute)
			if legacyFuture {
				eventAt = now.Add(time.Hour)
			}
			const stale = "1111111111111111111111111111111111111111"
			const actual = "2222222222222222222222222222222222222222"
			branch, body := "legion/"+issue, "Dispatch: "+issue
			// A row of an older release, which stored its verdict: the branch's issue.
			err := UpsertPullRequestHead(ctx, pool, PullRequestHead{
				Repo: "acme/widgets", Number: 98767, HeadSHA: stale,
				HeadRef: branch, BaseRef: "main", State: "open",
				EventAt: &eventAt, CheckedAt: &now,
			})
			if err != nil {
				t.Fatal(err)
			}
			seedStoredIssue(t, ctx, pool, "acme/widgets", 98767, &issue, "branch")
			gh.set("acme/widgets", 98767, "closed", actual, branch, body, now)
			_, err = intake.handlePullRequestEnvelope(ctx, nil,
				prEnvelope("acme/widgets", 98767, "closed", actual, branch, body, now))
			if err != nil {
				t.Fatal(err)
			}
			afterEvent := readHead(t, ctx, pool, "acme/widgets", 98767)
			eventReads := gh.reads.Load()
			_, _, err = RefreshPullRequestHead(ctx, pool, client, nil, "acme/widgets", 98767, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			afterRefresh := readHead(t, ctx, pool, "acme/widgets", 98767)
			staleBasis, err := HeadAttributedTo(ctx, pool, issue, stale)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("after event: head=%s state=%s github_reads=%d; after refresh: head=%s state=%s total_github_reads=%d stale_head_is_basis=%v",
				afterEvent.HeadSHA, afterEvent.State, eventReads, afterRefresh.HeadSHA,
				afterRefresh.State, gh.reads.Load(), staleBasis)
			if afterEvent.HeadSHA != actual || afterEvent.State != "closed" || afterRefresh.HeadSHA != actual || afterRefresh.State != "closed" || staleBasis {
				t.Error("GitHub says closed at 2222, but a legacy row still makes the old open head a basis")
			}
		})
	}
}

// releaseBeforeUpsertHeadRow is upsertHeadRow as the release before this change runs
// it, word for word (main at 5bd14c323bdf): it names no column it does not know, so it leaves
// whatever else the row holds as it was, and it orders its own writes by the time it is handed.
// That release handed it an envelope's updated_at, head and state.
const releaseBeforeUpsertHeadRow = `
		insert into pull_request_heads (
			repo, number, head_sha, head_ref, base_ref, state, issue_key, attribution_source,
			title, url, event_at, checked_at, attribution_rule
		)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $14)
		on conflict (repo, number) do update set
			head_sha = excluded.head_sha,
			head_ref = excluded.head_ref,
			base_ref = excluded.base_ref,
			state = excluded.state,
			issue_key = case when $13::boolean
				then excluded.issue_key else pull_request_heads.issue_key end,
			attribution_source = case when $13::boolean
				then excluded.attribution_source else pull_request_heads.attribution_source end,
			title = excluded.title,
			url = excluded.url,
			event_at = coalesce(excluded.event_at, pull_request_heads.event_at),
			checked_at = coalesce(excluded.checked_at, pull_request_heads.checked_at),
			attribution_rule = case when excluded.checked_at is null
				then pull_request_heads.attribution_rule else excluded.attribution_rule end
		where excluded.event_at is null
		   or pull_request_heads.event_at is null
		   or excluded.event_at >= pull_request_heads.event_at
	`

// releaseBeforeAttributionRule is the attribution_rule that the release before stamps on its
// writes.
const releaseBeforeAttributionRule = 2

// releaseBeforeAttribution is the issue the release before's write carries with a head: the issue
// and the source its own attribution gave, and whether that attribution read every source, which
// lets the write replace the stored issue. This build stores no issue with a head, so a test of a
// row that release writes gives the issue here.
type releaseBeforeAttribution struct {
	issue    *string
	source   string
	complete bool
}

// noAttribution is the release before's write of an attribution that named nobody and read
// no source: it keeps the issue the row held.
var noAttribution = releaseBeforeAttribution{source: string(sourceNone)}

// writeHeadAsTheReleaseBefore stores head with the release before's own statement, as that
// release's envelope writer handed it: the envelope's head, state, branch, title and updated_at,
// and the issue of that release's attribution.
func writeHeadAsTheReleaseBefore(t *testing.T, ctx context.Context, pool *store.Pool, head PullRequestHead, attribution releaseBeforeAttribution) {
	t.Helper()
	if _, err := pool.Exec(ctx, releaseBeforeUpsertHeadRow,
		head.Repo, head.Number, head.HeadSHA, head.HeadRef, head.BaseRef, head.State,
		attribution.issue, attribution.source, head.Title, head.URL, head.EventAt, head.CheckedAt,
		attribution.complete, releaseBeforeAttributionRule,
	); err != nil {
		t.Fatalf("write the head of %s#%d as the release before does: %v", head.Repo, head.Number, err)
	}
}

// A row the release before writes beside this build, after the migration has run, stops no read
// of GitHub and wins against none of its answers. This build has read the pull request from
// GitHub: open, at its real head. The release before then takes an envelope GitHub never sent —
// a merge at another head, an hour ahead of GitHub's clock, naming no head repository — and
// writes it as that release writes every envelope: its head, its state and its time, with the
// issue the row held kept, since a payload that names no head repository is an incomplete
// attribution that deletes nobody's issue. That write owes a read (pull_request_heads_unread_write),
// and while the read is owed no reader takes the forged head for a basis of the issue's check.
//
// From there GitHub's answer must win at the first read of it this build makes: a later event of
// the pull request reads GitHub and writes its answer, though the stored time is later than
// GitHub's. With no event at all, the release before's write owes a read
// (pull_request_heads_unread_write). The reconcile's next pass makes that read once over REST, and
// writes GitHub's answer. The fake counts a batched query of the facts line apart from that read.
// Before the fix the event read nothing (headReadCovers took the forged time for GitHub's), and
// the pass would have refused GitHub's older time. A write that keeps the forged head, or a pass
// that reads the owed row more than once over REST, makes this test fail. The write of GitHub's
// answer over the forged row owes one more read, so GitHub's head stands after the pass that
// reads it, and not before.
func TestARowTheReleaseBeforeWritesBesideThisBuildStopsNoReadOfGitHub(t *testing.T) {
	for _, repair := range []string{"a later event of GitHub", "the reconcile's next pass"} {
		t.Run(repair, func(t *testing.T) {
			fastPageRetries(t)
			pool, ctx := deliveryTestPool(t)
			issue := seedHeadIssue(t, ctx, pool, 98769)
			fake := newFakeGitHub(t)
			github := serveGitHubPulls(t, fake, "acme/widgets")
			client := fake.newTestClient()
			intake := newIntake(nil, pool, nil, client, nil, nil)
			const (
				real   = "6969696969696969696969696969696969696969"
				forged = "6060606060606060606060606060606060606060"
			)
			branch, body := "legion/"+issue, "Dispatch: "+issue
			opened := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
			github.set("acme/widgets", 98769, "open", real, branch, body, opened)
			if _, err := intake.handlePullRequestEnvelope(ctx, nil, prEnvelope("acme/widgets", 98769, "opened", real, branch, body, opened)); err != nil {
				t.Fatalf("handle GitHub's own opened envelope: %v", err)
			}
			if attributed, err := HeadAttributedTo(ctx, pool, issue, real); err != nil || !attributed {
				t.Fatalf("HeadAttributedTo(%s, GitHub's head) after the opening = %v, %v; want true", issue, attributed, err)
			}

			// The release before keeps the issue the row held, and no writer of this build stores
			// one: the row stores the issue as a release before stored it.
			seedStoredIssue(t, ctx, pool, "acme/widgets", 98769, &issue, "branch")
			ahead := opened.Add(time.Hour)
			writeHeadAsTheReleaseBefore(t, ctx, pool, PullRequestHead{
				Repo: "acme/widgets", Number: 98769, HeadSHA: forged, HeadRef: branch, BaseRef: "main",
				State: "merged", Title: "Change 98769",
				URL: "https://github.com/acme/widgets/pull/98769", EventAt: &ahead,
			}, noAttribution)
			// The precondition: the release before's write is the row B3 of the review names. It
			// owes a read (pull_request_heads_unread_write), and while that read is owed no reader
			// takes the forged head for a basis of the issue's check.
			if owed, held := readOwedRow(t, ctx, pool, "acme/widgets", 98769); !held || owed.reason != owedFailure {
				t.Fatalf("after the release before's write #98769 is owed %+v (held %v), want a failure: the fixture is not the row under test", owed, held)
			}
			if attributed, err := HeadAttributedTo(ctx, pool, issue, forged); err != nil || attributed {
				t.Fatalf("HeadAttributedTo(%s, the forged head) while its read is owed = %v, %v; want false: no head stands while a read is owed", issue, attributed, err)
			}

			readsBefore := github.reads.Load()
			switch repair {
			case "a later event of GitHub":
				labeled := opened.Add(time.Minute)
				github.set("acme/widgets", 98769, "open", real, branch, body, labeled)
				if _, err := intake.handlePullRequestEnvelope(ctx, nil, prEnvelope("acme/widgets", 98769, "labeled", real, branch, body, labeled)); err != nil {
					t.Fatalf("handle GitHub's labeled envelope: %v", err)
				}
				if reads := github.reads.Load() - readsBefore; reads != 1 {
					t.Errorf("the labeled event read the pull request from GitHub %d times, want once: a time the release before took from an envelope stopped the read", reads)
				}
			case "the reconcile's next pass":
				NewReconcile(pool, client, events.NewBroker()).runOnce(ctx)
				if reads := github.reads.Load() - readsBefore; reads != 1 {
					t.Errorf("the pass read the pull request from GitHub over REST %d times, want once: the read that the release before's write owes", reads)
				}
			}

			got := readHead(t, ctx, pool, "acme/widgets", 98769)
			if got.HeadSHA != real || got.State != "open" {
				t.Errorf("#98769 after %s = head %s, state %s; want GitHub's %s, open", repair, got.HeadSHA, got.State, real)
			}
			if attributed, err := HeadAttributedTo(ctx, pool, issue, forged); err != nil || attributed {
				t.Errorf("HeadAttributedTo(%s, the forged head) after %s = %v, %v; want false: a head GitHub never sent stands", issue, repair, attributed, err)
			}
			// The repair's own write lands GitHub's answer over a row that no read wrote and whose
			// time is later, so its trigger owes one more read (pull_request_heads_older_read). While
			// that read is owed, GitHub's head is not read either. The next pass reads it once more,
			// over a row a read wrote now, owes nothing and settles it.
			if _, held := readOwedRow(t, ctx, pool, "acme/widgets", 98769); !held {
				t.Errorf("after %s no read of #98769 is owed, want the one that the repair's own write owes", repair)
			}
			if attributed, err := HeadAttributedTo(ctx, pool, issue, real); err != nil || attributed {
				t.Errorf("HeadAttributedTo(%s, GitHub's head) after %s = %v, %v; want false while its read is owed", issue, repair, attributed, err)
			}
			NewReconcile(pool, client, events.NewBroker()).runOnce(ctx)
			if owed, held := readOwedRow(t, ctx, pool, "acme/widgets", 98769); held {
				t.Errorf("after %s and one more pass #98769 is owed %+v, want none: that pass read it", repair, owed)
			}
			if attributed, err := HeadAttributedTo(ctx, pool, issue, real); err != nil || !attributed {
				t.Errorf("HeadAttributedTo(%s, GitHub's head) after %s and one more pass = %v, %v; want true", issue, repair, attributed, err)
			}
		})
	}
}
