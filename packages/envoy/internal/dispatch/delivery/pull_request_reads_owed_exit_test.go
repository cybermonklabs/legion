package delivery

// The permanent case of an owed refusal, and its way out (B5 of the plan's review 11). Where the
// App cannot read a repository, GitHub refuses each read of its pull requests, so the read stays
// owed. Each reader of a head then says that the head is not read, and names the refusal and its
// time. The way out is that the App can read the repository again: the next pass reads the pull
// request, writes its head and settles the row.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// owedReadRow is one row of pull_request_reads_owed as a test reads it.
type owedReadRow struct {
	reason       owedReason
	reads        int
	refusedAs    *string
	refusedSince *time.Time
}

// readOwedRow is the row of pull_request_reads_owed of repo#number. It answers false where no read
// of that pull request is owed.
func readOwedRow(t *testing.T, ctx context.Context, pool *store.Pool, repo string, number int) (owedReadRow, bool) {
	t.Helper()
	var row owedReadRow
	err := pool.QueryRow(ctx, `
		select reason, reads, refused_as, refused_since from pull_request_reads_owed where repo = $1 and number = $2
	`, repo, number).Scan(&row.reason, &row.reads, &row.refusedAs, &row.refusedSince)
	if errors.Is(err, pgx.ErrNoRows) {
		return owedReadRow{}, false
	}
	if err != nil {
		t.Fatalf("read the owed read of %s#%d: %v", repo, number, err)
	}
	return row, true
}

// attributedHeadOf is the entry of pull request number in HeadsAttributedTo(issue). The test stops
// where the issue lists no such pull request.
func attributedHeadOf(t *testing.T, ctx context.Context, pool *store.Pool, issue string, number int) AttributedHead {
	t.Helper()
	heads, err := HeadsAttributedTo(ctx, pool, issue)
	if err != nil {
		t.Fatalf("HeadsAttributedTo(%s): %v", issue, err)
	}
	for _, head := range heads {
		if head.Number == number {
			return head
		}
	}
	t.Fatalf("HeadsAttributedTo(%s) = %+v, which lists no #%d", issue, heads, number)
	return AttributedHead{}
}

// shownTime is at as a failure shows it, or "none" where at is nil.
func shownTime(at *time.Time) string {
	if at == nil {
		return "none"
	}
	return at.UTC().Format(time.RFC3339Nano)
}

// wantAttributedHead fails the test where got and want differ in the number, the head, whether the
// head is known, the word of the refusal or the time of the first refusal.
func wantAttributedHead(t *testing.T, step string, got, want AttributedHead) {
	t.Helper()
	sameSince := got.RefusedSince == nil && want.RefusedSince == nil ||
		got.RefusedSince != nil && want.RefusedSince != nil && got.RefusedSince.Equal(*want.RefusedSince)
	if got.Number != want.Number || got.HeadSHA != want.HeadSHA || got.HeadKnown != want.HeadKnown || got.RefusedAs != want.RefusedAs || !sameSince {
		t.Errorf("%s: HeadsAttributedTo lists #%d with head %s, HeadKnown %v, RefusedAs %q and RefusedSince %s; want #%d with head %s, HeadKnown %v, RefusedAs %q and RefusedSince %s",
			step, got.Number, got.HeadSHA, got.HeadKnown, got.RefusedAs, shownTime(got.RefusedSince),
			want.Number, want.HeadSHA, want.HeadKnown, want.RefusedAs, shownTime(want.RefusedSince))
	}
}

// acme/widgets#765 holds a head that a read of GitHub wrote. Then the installation loses the
// repository, so GitHub answers the lookup of the installation with 404 (githubapp.ErrNoInstallation).
// One envelope of #765 with a second head reaches the intake, which owes a refusal. After the
// envelope and after each of five passes, the row is owed as a refusal that each pass reads once.
// HeadsAttributedTo lists #765 at its stored head with HeadKnown false, RefusedAs app_not_installed,
// and RefusedSince the time of the first refusal. Then GitHub serves the repository again. The
// next pass reads #765, writes its second head and settles the row. Control in the same test: with
// the repository readable all along, the intake reads the second head, and nothing is owed.
func TestARefusedReadEndsWhenTheAppCanReadTheRepositoryAgain(t *testing.T) {
	for _, kase := range []string{"control_readable_all_along", "the_installation_loses_the_repository"} {
		t.Run(kase, func(t *testing.T) {
			fastPageRetries(t)
			pool, ctx := deliveryTestPool(t)
			const number = 765
			issue := seedHeadIssue(t, ctx, pool, number)
			fake := newFakeGitHub(t)
			github := serveGitHubPulls(t, fake, "acme/widgets")
			branch, body := "legion/"+issue, "Dispatch: "+issue
			opened := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Second)
			pushed := opened.Add(time.Minute)
			first, second := fakeSHA("a", number), fakeSHA("b", number)

			// A read of GitHub writes the first head.
			github.set("acme/widgets", number, "open", first, branch, body, opened)
			if _, err := newIntake(nil, pool, nil, fake.newTestClient(), nil, nil).handlePullRequestEnvelope(ctx, nil,
				prEnvelope("acme/widgets", number, "opened", first, branch, body, opened)); err != nil {
				t.Fatalf("handle the envelope that opens #%d: %v", number, err)
			}
			var read bool
			if err := pool.QueryRow(ctx, `
				select github_read_at is not null from pull_request_heads where repo = 'acme/widgets' and number = $1 and head_sha = $2
			`, number, first).Scan(&read); err != nil || !read {
				t.Fatalf("#%d holds no head %s that a read of GitHub wrote (read %v): %v", number, first, read, err)
			}
			if row, owed := readOwedRow(t, ctx, pool, "acme/widgets", number); owed {
				t.Fatalf("#%d is owed a %s read after a read of GitHub wrote its head, want none", number, row.reason)
			}

			// GitHub moves #765 to a second head. The client below has not looked up the
			// installation of the repository yet. Each client looks it up again when
			// installationsCacheTTL ends. Where the installation lost the repository, GitHub
			// answers that lookup with 404.
			github.set("acme/widgets", number, "open", second, branch, body, pushed)
			lost := kase == "the_installation_loses_the_repository"
			if lost {
				fake.notInstalled.Store("acme/widgets", true)
			}
			client := fake.newTestClient()
			if _, err := newIntake(nil, pool, nil, client, nil, nil).handlePullRequestEnvelope(ctx, nil,
				prEnvelope("acme/widgets", number, "synchronize", second, branch, body, pushed)); err != nil {
				t.Fatalf("handle the envelope of the second head of #%d: %v", number, err)
			}
			settled := AttributedHead{Number: number, HeadSHA: second, HeadKnown: true}
			if !lost {
				if row, owed := readOwedRow(t, ctx, pool, "acme/widgets", number); owed {
					t.Errorf("control: #%d is owed a %s read after the intake read its second head, want none", number, row.reason)
				}
				wantAttributedHead(t, "control: after the envelope of the second head", attributedHeadOf(t, ctx, pool, issue, number), settled)
				return
			}

			row, owed := readOwedRow(t, ctx, pool, "acme/widgets", number)
			if !owed || row.reason != owedRefusal || row.refusedSince == nil {
				t.Fatalf("after the refused read of the intake, #%d is owed %v (%+v), want a refusal with the time of the refusal", number, owed, row)
			}
			firstRefusal := *row.refusedSince
			refused := AttributedHead{Number: number, HeadSHA: first, RefusedAs: model.RefusedAsAppNotInstalled, RefusedSince: &firstRefusal}
			wantAttributedHead(t, "after the refused envelope", attributedHeadOf(t, ctx, pool, issue, number), refused)

			reconcile := NewReconcile(pool, client, events.NewBroker())
			for pass := 1; pass <= 5; pass++ {
				lookups := fake.lookupsOf("acme/widgets")
				reconcile.runOnce(ctx)
				step := fmt.Sprintf("pass %d", pass)
				row, owed := readOwedRow(t, ctx, pool, "acme/widgets", number)
				refusedAs := "none"
				if row.refusedAs != nil {
					refusedAs = *row.refusedAs
				}
				t.Logf("%s: owed=%v reason=%s reads=%d refused_as=%s refused_since=%s installation_lookups=%d",
					step, owed, row.reason, row.reads, refusedAs, shownTime(row.refusedSince), fake.lookupsOf("acme/widgets")-lookups)
				if !owed || row.reason != owedRefusal || row.reads != pass || refusedAs != model.RefusedAsAppNotInstalled || !sameTime(row.refusedSince, firstRefusal) {
					t.Errorf("%s: #%d is owed %v as %q, read %d times, refused as %s since %s; want a refusal read %d times, refused as %s since %s",
						step, number, owed, row.reason, row.reads, refusedAs, shownTime(row.refusedSince), pass, model.RefusedAsAppNotInstalled, shownTime(&firstRefusal))
				}
				wantAttributedHead(t, step, attributedHeadOf(t, ctx, pool, issue, number), refused)
			}

			fake.notInstalled.Delete("acme/widgets")
			reconcile.runOnce(ctx)
			if row, owed := readOwedRow(t, ctx, pool, "acme/widgets", number); owed {
				t.Errorf("after the pass that reads #%d from a readable repository, #%d is still owed a %s read (%+v), want none", number, number, row.reason, row)
			}
			wantAttributedHead(t, "the pass after GitHub serves the repository again", attributedHeadOf(t, ctx, pool, issue, number), settled)
		})
	}
}
