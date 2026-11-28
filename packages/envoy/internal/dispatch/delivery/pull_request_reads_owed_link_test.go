package delivery

// A link by hand owes a read of the pull request that it links where Dispatch stores no head of
// that pull request (R5 of the results lookup). Where a head is stored, the link owes nothing.
// A head can still be stored beside a link's owe: a writer whose read began before the link owed
// the read stores the head, and its settle at its own earlier revision leaves the link's owe. A
// link says nothing of the head, so that owe hides no stored head from the readers by issue
// (owedReadJoin). Each other owe says that the head can have moved, so it hides the head,
// whichever of the two owes comes first. A read that began before the later owe settles neither.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// linkByHand owes the read of acme/widgets#number as the link write of that pull request's URL
// does, in a transaction of its own (OweLinkedPullRequestReads).
func linkByHand(t *testing.T, ctx context.Context, pool *store.Pool, number int) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the link write: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := unrenamed().OweLinkedPullRequestReads(ctx, tx, []string{fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number)}); err != nil {
		t.Fatalf("owe the read of the link to #%d: %v", number, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the link write: %v", err)
	}
}

// linkOwedBeforeTheHeadWas leaves acme/widgets#number with the owe that a link by hand wrote while
// no head row of it was stored, beside the head row that a writer whose read began before the owe
// stored after it: the owe's own write (recordLinkedOwe), in a transaction of its own, as the link
// write of that pull request's URL makes it when the head is not yet held.
func linkOwedBeforeTheHeadWas(t *testing.T, ctx context.Context, pool *store.Pool, number int) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the link write: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := recordLinkedOwe(ctx, tx, "acme/widgets", number); err != nil {
		t.Fatalf("owe the read of the link to #%d: %v", number, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the link write: %v", err)
	}
}

// storeReadHead makes acme/widgets#number an open pull request of issue whose head row a read of
// GitHub wrote at the head first. It answers GitHub's pull requests of acme/widgets and the client
// that reads them.
func storeReadHead(t *testing.T, ctx context.Context, pool *store.Pool, number int, first string) (*githubPulls, *fakeGitHub) {
	t.Helper()
	issue := fmt.Sprintf("HEAD-%d", number)
	fake := newFakeGitHub(t)
	github := serveGitHubPulls(t, fake, "acme/widgets")
	opened := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Second)
	github.set("acme/widgets", number, "open", first, "legion/"+issue, "Dispatch: "+issue, opened)
	if _, err := newIntake(tuningFor(t), nil, pool, nil, fake.newTestClient(), nil, nil, unrenamed()).handlePullRequestEnvelope(ctx, nil,
		prEnvelope("acme/widgets", number, "opened", first, "legion/"+issue, "Dispatch: "+issue, opened)); err != nil {
		t.Fatalf("handle the envelope that opens #%d: %v", number, err)
	}
	if row, owed := readOwedRow(t, ctx, pool, "acme/widgets", number); owed {
		t.Fatalf("SETUP: #%d is owed a %s read after a read of GitHub wrote its head, want none", number, row.reason)
	}
	return github, fake
}

// Each of three owes comes before or after the owe of a link of the same pull request, whose head
// row a read of GitHub wrote: the owe of an event whose read failed, the owe of a read that GitHub
// refused, and the owe of a write of the release before that changes the head. A link written
// now owes nothing, since the head is held. The link's owe here is the one it wrote before the
// head was stored (linkOwedBeforeTheHeadWas). With the link's owe alone, HeadsAttributedTo lists
// the stored head as known. After both owes, in either order, it lists the head as not read, and a
// refusal keeps its word. A settle at the revision of the first owe, the one a read that began
// between the two owes took, deletes nothing.
func TestALinksOwedReadHidesNoStoredHeadAndEachOtherOweHidesItInEitherOrder(t *testing.T) {
	others := []struct {
		name      string
		refusedAs string
		owe       func(t *testing.T, ctx context.Context, pool *store.Pool, number int)
	}{
		{name: "an_event_whose_read_failed", owe: func(t *testing.T, ctx context.Context, pool *store.Pool, number int) {
			if err := unrenamed().owePullRequestRead(ctx, pool, "acme/widgets", number, owedFailure, ""); err != nil {
				t.Fatalf("owe the failed read of an event of #%d: %v", number, err)
			}
		}},
		{name: "a_read_that_github_refused", refusedAs: model.RefusedAsAppNotInstalled, owe: func(t *testing.T, ctx context.Context, pool *store.Pool, number int) {
			if err := unrenamed().owePullRequestRead(ctx, pool, "acme/widgets", number, owedRefusal, model.RefusedAsAppNotInstalled); err != nil {
				t.Fatalf("owe the refused read of #%d: %v", number, err)
			}
		}},
		{name: "a_write_of_the_release_before", owe: func(t *testing.T, ctx context.Context, pool *store.Pool, number int) {
			// The release before writes an envelope's head over the row with no read of GitHub,
			// and the issue that it judged. The trigger pull_request_heads_unread_write owes a read
			// in the same statement.
			if _, err := pool.Exec(ctx, `update pull_request_heads set head_sha = $2, issue_key = $3 where repo = 'acme/widgets' and number = $1`,
				number, fakeSHA("c", number), fmt.Sprintf("HEAD-%d", number)); err != nil {
				t.Fatalf("write the head of #%d as the release before does: %v", number, err)
			}
		}},
	}
	for i, other := range others {
		for _, linkFirst := range []bool{true, false} {
			order := "the_link_last"
			if linkFirst {
				order = "the_link_first"
			}
			t.Run(other.name+"/"+order, func(t *testing.T) {
				fastPageRetries(t)
				pool, ctx := deliveryTestPool(t)
				number := 770 + 2*i
				if linkFirst {
					number++
				}
				issue := seedHeadIssue(t, ctx, pool, number)
				first := fakeSHA("a", number)
				storeReadHead(t, ctx, pool, number, first)
				linkByHand(t, ctx, pool, number)
				if owed := owedCount(t, ctx, pool, "acme/widgets", number); owed != 0 {
					t.Fatalf("after a link to #%d, whose head is held, %d reads of it are owed; want none", number, owed)
				}
				var before *owedRead
				if linkFirst {
					linkOwedBeforeTheHeadWas(t, ctx, pool, number)
					if owedCount(t, ctx, pool, "acme/widgets", number) != 1 {
						t.Fatalf("after the link's owe of #%d, no read of it is owed; want one", number)
					}
					wantAttributedHead(t, "after the link alone", attributedHeadOf(t, ctx, pool, issue, number),
						AttributedHead{Number: number, HeadSHA: first, HeadKnown: true})
					before = owedRevision(ctx, pool, "acme/widgets", number)
					other.owe(t, ctx, pool, number)
				} else {
					other.owe(t, ctx, pool, number)
					before = owedRevision(ctx, pool, "acme/widgets", number)
					linkOwedBeforeTheHeadWas(t, ctx, pool, number)
				}
				if before == nil {
					t.Fatalf("SETUP: no revision of the first owe of #%d", number)
				}
				hidden := func(step string) {
					t.Helper()
					head := attributedHeadOf(t, ctx, pool, issue, number)
					if head.HeadKnown || head.RefusedAs != other.refusedAs {
						t.Errorf("%s: HeadsAttributedTo lists #%d with HeadKnown %v and RefusedAs %q; want the head not read, with RefusedAs %q",
							step, number, head.HeadKnown, head.RefusedAs, other.refusedAs)
					}
				}
				hidden("after both owes")
				if err := settleOwedRead(ctx, pool, before.Repo, number, before.Revision); err != nil {
					t.Fatalf("settle the read of #%d at the first owe's revision: %v", number, err)
				}
				if owedCount(t, ctx, pool, "acme/widgets", number) != 1 {
					t.Errorf("a settle at the first owe's revision deleted the owed read of #%d; want it kept for the second owe", number)
				}
				hidden("after a settle at the first owe's revision")
			})
		}
	}
}

// The owe that a link wrote before a read of GitHub stored the head of acme/widgets#779
// (linkOwedBeforeTheHeadWas). The reconcile's read of it fails at the first pass, and GitHub
// refuses it at the second, as an installation that loses the repository does. Neither says that
// the head moved, so HeadsAttributedTo lists the stored head as known after each pass, and the
// read stays owed. When GitHub serves the pull request again, the next pass reads it and settles
// the owe.
func TestAFailedOrRefusedReadOfALinksOweHidesNoStoredHead(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	const number = 779
	issue := seedHeadIssue(t, ctx, pool, number)
	first := fakeSHA("a", number)
	github, fake := storeReadHead(t, ctx, pool, number, first)
	known := AttributedHead{Number: number, HeadSHA: first, HeadKnown: true}
	linkOwedBeforeTheHeadWas(t, ctx, pool, number)
	client := fake.newTestClient()
	reconcile := NewReconcile(pool, client, events.NewBroker(), unrenamed())

	github.mu.Lock()
	github.failing = true
	github.mu.Unlock()
	reconcile.runOnce(ctx)
	if row, owed := readOwedRow(t, ctx, pool, "acme/widgets", number); !owed || row.reason != owedFailure || row.reads != 1 {
		t.Errorf("after a pass whose read of #%d failed, it is owed %v (%+v); want a failure read one time", number, owed, row)
	}
	wantAttributedHead(t, "after the failed read", attributedHeadOf(t, ctx, pool, issue, number), known)

	github.mu.Lock()
	github.failing = false
	github.mu.Unlock()
	fake.notInstalled.Store("acme/widgets", true)
	reconcile = NewReconcile(pool, fake.newTestClient(), events.NewBroker(), unrenamed())
	reconcile.runOnce(ctx)
	if row, owed := readOwedRow(t, ctx, pool, "acme/widgets", number); !owed || row.reason != owedRefusal || row.reads != 2 {
		t.Errorf("after a pass whose read of #%d GitHub refused, it is owed %v (%+v); want a refusal read two times", number, owed, row)
	}
	wantAttributedHead(t, "after the refused read", attributedHeadOf(t, ctx, pool, issue, number), known)

	fake.notInstalled.Delete("acme/widgets")
	reconcile = NewReconcile(pool, fake.newTestClient(), events.NewBroker(), unrenamed())
	reconcile.runOnce(ctx)
	if row, owed := readOwedRow(t, ctx, pool, "acme/widgets", number); owed {
		t.Errorf("after the pass that reads #%d, it is still owed a %s read (%+v); want none", number, row.reason, row)
	}
	wantAttributedHead(t, "after the read that GitHub answers", attributedHeadOf(t, ctx, pool, issue, number), known)
}

// acme/widgets#781 has a timeline row of HEAD-781 and no results row, and a link owes its read.
// GitHub refuses the read, since the App is not installed on the repository. There is no stored
// head to hide, so HeadsAttributedTo lists #781 as a head not read with the word of the refusal
// and its time, as for each other owed refusal (owedReadJoin).
func TestALinksRefusedReadOfAPullRequestWithNoResultsRowNamesTheRefusal(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	const number = 781
	issue := seedHeadIssue(t, ctx, pool, number)
	if _, err := pool.Exec(ctx, `insert into delivery_pull_requests (repo, number, title, url, author, issue_key) values ('acme/widgets', $1, 'A change', $2, 'octocat', $3)`,
		number, fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number), issue); err != nil {
		t.Fatalf("store the timeline row of #%d: %v", number, err)
	}
	fake := newFakeGitHub(t)
	serveGitHubPulls(t, fake, "acme/widgets")
	fake.notInstalled.Store("acme/widgets", true)
	linkByHand(t, ctx, pool, number)
	NewReconcile(pool, fake.newTestClient(), events.NewBroker(), unrenamed()).runOnce(ctx)
	row, owed := readOwedRow(t, ctx, pool, "acme/widgets", number)
	if !owed || row.reason != owedRefusal || row.refusedAs == nil || *row.refusedAs != model.RefusedAsAppNotInstalled || row.refusedSince == nil {
		t.Fatalf("after a pass whose read of #%d GitHub refused, it is owed %v (%+v); want a refusal as %s with its time", number, owed, row, model.RefusedAsAppNotInstalled)
	}
	wantAttributedHead(t, "after the refused read", attributedHeadOf(t, ctx, pool, issue, number),
		AttributedHead{Number: number, RefusedAs: model.RefusedAsAppNotInstalled, RefusedSince: row.refusedSince})
}

// A link owes a read of acme/widgets#783, a pull request GitHub holds, while the App is not
// installed on acme/widgets. The first pass reads it, GitHub refuses the read, and the row is
// recorded as unreadable at its revision. The next two passes do not read it: its count of reads
// stays one, and it is in neither owed count. Then the App is installed on acme/widgets, and the
// next pass, whose installations list the repository, reads the pull request, stores its head and
// settles the read. Control in the same test: a read that an event owes and GitHub refuses the
// same way is read every pass, since an event says that GitHub sent something of that pull
// request.
func TestAnUnreadableLinkIsReadAgainWhenTheAppIsInstalledOnItsRepository(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	const linked, evented = 783, 785
	issue := seedHeadIssue(t, ctx, pool, linked)
	fake := newFakeGitHub(t)
	github := serveGitHubPulls(t, fake, "acme/widgets")
	opened := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Second)
	head := fakeSHA("a", linked)
	github.set("acme/widgets", linked, "open", head, "legion/"+issue, "Dispatch: "+issue, opened)
	fake.notInstalled.Store("acme/widgets", true)
	linkByHand(t, ctx, pool, linked)
	if err := unrenamed().owePullRequestRead(ctx, pool, "acme/widgets", evented, owedRefusal, model.RefusedAsAppNotInstalled); err != nil {
		t.Fatalf("owe the refused read of an event of #%d: %v", evented, err)
	}
	reconcile := NewReconcile(pool, fake.newTestClient(), events.NewBroker(), unrenamed())
	for pass := 1; pass <= 3; pass++ {
		lookups := fake.lookupsOf("acme/widgets")
		reconcile.runOnce(ctx)
		row, owed := readOwedRow(t, ctx, pool, "acme/widgets", linked)
		other, _ := readOwedRow(t, ctx, pool, "acme/widgets", evented)
		counts, err := unrenamed().ReadAttributionCounts(ctx, pool)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("pass %d: #%d owed=%v reason=%s reads=%d; #%d reads=%d; installation lookups=%d; owed_refusals=%d",
			pass, linked, owed, row.reason, row.reads, evented, other.reads, fake.lookupsOf("acme/widgets")-lookups, counts.OwedRefusals)
		if !owed || row.reason != owedRefusal || row.reads != 1 || row.refusedAs == nil || *row.refusedAs != model.RefusedAsAppNotInstalled {
			t.Errorf("after pass %d, #%d is owed %v (%+v); want a refusal as %s read one time", pass, linked, owed, row, model.RefusedAsAppNotInstalled)
		}
		if other.reads != pass {
			t.Errorf("control: after pass %d, the read an event owed of #%d was read %d times; want %d", pass, evented, other.reads, pass)
		}
		if counts.OwedRefusals != 1 {
			t.Errorf("after pass %d, owed_refusals counts %d; want 1, the event's: the unreadable link is owed no read", pass, counts.OwedRefusals)
		}
	}

	fake.notInstalled.Delete("acme/widgets")
	NewReconcile(pool, fake.newTestClient(), events.NewBroker(), unrenamed()).runOnce(ctx)
	if row, owed := readOwedRow(t, ctx, pool, "acme/widgets", linked); owed {
		t.Errorf("after the pass that follows the App's installation on acme/widgets, #%d is still owed a %s read (%+v); want none", linked, row.reason, row)
	}
	wantAttributedHead(t, "after the pass that follows the installation", attributedHeadOf(t, ctx, pool, issue, linked),
		AttributedHead{Number: linked, HeadSHA: head, HeadKnown: true})
}
