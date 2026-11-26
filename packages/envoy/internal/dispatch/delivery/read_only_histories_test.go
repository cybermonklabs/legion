package delivery

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The histories of a read-only caller's results read (read_only_fold_test.go says what the read
// keeps and what it may not): each read of the results path, given the writes the request's
// read-throughs kept (HeadAnswers), answers what the writer's path answers once it has written
// the same steps and committed them, and the read stores nothing.

// The fixture issues every history's database holds (readOnlyDatabase): HEAD-701 of deliveryRepo,
// and GDGT-702 of gadgetsRepo, the repository oldco/gadgets was renamed to, which links
// deliveryRepo's pull request 7 (readOnlyLinkedPull). The link ranks above every other source, so
// pull request 7 is GDGT-702's whatever its texts say, and the histories credit pull requests 5
// and 6 to HEAD-701 by their titles.
const (
	readOnlyIssue        = "HEAD-701"
	readOnlyLinkingIssue = "GDGT-702"
	readOnlyLinkedPull   = 7
)

// readOnlyDatabase is a new database of the test's own with the fixture issues, the link and the
// rename pair, and the Attributor of that pair, as a process builds its own at boot (renamesOf).
// A history makes two: one the read-only request reads, and one the writer's path writes the
// same steps into and commits, whose reads are the oracle.
func readOnlyDatabase(t *testing.T) (*store.Pool, context.Context, *Attributor) {
	t.Helper()
	pool, ctx := deliveryTestPool(t)
	if key := seedHeadIssue(t, ctx, pool, 701); key != readOnlyIssue {
		t.Fatalf("seeded %s, want %s", key, readOnlyIssue)
	}
	seedRenamedGadgets(t, ctx, pool)
	if key := seedGadgetsIssue(t, ctx, pool, 702); key != readOnlyLinkingIssue {
		t.Fatalf("seeded %s, want %s", key, readOnlyLinkingIssue)
	}
	link := fmt.Sprintf("https://github.com/%s/pull/%d", deliveryRepo, readOnlyLinkedPull)
	if _, err := pool.Exec(ctx, `insert into issue_external_links (issue_key, url, kind) values ($1, $2, 'url')`,
		readOnlyLinkingIssue, link); err != nil {
		t.Fatalf("link %s to %s: %v", readOnlyLinkingIssue, link, err)
	}
	return pool, ctx, NewAttributor(renamesOf(t, ctx, pool))
}

// readOnlyStep is one write a writer's read makes: a head and its texts (Attributor.writeFacts),
// of a read asked under the name under ("" for the head's own repository), or the settle of the
// reads owed for a pull request (settleOwedRead), with the owed reads the read found before it
// asked GitHub (resolve), as readThroughCommitPulls reads them first.
type readOnlyStep struct {
	head   *PullRequestHead
	under  string
	settle *PullRequestKey
	owed   []owedRead
}

// write makes the step's write on db, with the writer's own code.
func (step readOnlyStep) write(ctx context.Context, a *Attributor, db refs.Pool) error {
	if step.head != nil {
		_, err := a.writeFacts(ctx, db, *step.head, cmp.Or(step.under, step.head.Repo), false)
		return err
	}
	for _, read := range step.owed {
		if err := settleOwedRead(ctx, db, read.Repo, read.Number, read.Revision); err != nil {
			return err
		}
	}
	return nil
}

// keep makes the step's write for a read-only caller's request, as its read-throughs make it
// (keepFacts, facts.write): in a transaction of pool that holds answers' writes, rolled back, and
// kept in answers.
func (step readOnlyStep) keep(ctx context.Context, a *Attributor, pool *store.Pool, answers *HeadAnswers) error {
	f := facts{pool: pool, answers: answers}
	if step.head != nil {
		_, err := a.keepFacts(ctx, f, *step.head, cmp.Or(step.under, step.head.Repo), false)
		return err
	}
	_, err := f.write(ctx, func(ctx context.Context, db refs.Pool) (bool, error) {
		return false, step.write(ctx, a, db)
	})
	return err
}

// resolve is steps with each settle's owed reads, as the read of pool finds them before the
// request asks GitHub. A settle with no owed read fails the test: the history would then hold no
// settle at all.
func resolve(t *testing.T, ctx context.Context, a *Attributor, pool *store.Pool, steps []readOnlyStep) []readOnlyStep {
	t.Helper()
	resolved := slices.Clone(steps)
	for index, step := range resolved {
		if step.settle == nil {
			continue
		}
		resolved[index].owed = a.owedReadsIn(ctx, pool, step.settle.Repo)[step.settle.Number]
		if len(resolved[index].owed) == 0 {
			t.Fatalf("SETUP: step %d settles the reads owed for %s#%d, and none is owed", index, step.settle.Repo, step.settle.Number)
		}
	}
	return resolved
}

// roAt is minute minutes past 9:00 on 2026-11-20, in UTC: the times of the histories.
func roAt(minute int) *time.Time {
	moment := time.Date(2026, 11, 20, 9, minute, 0, 0, time.UTC)
	return &moment
}

// roSHA is a commit of forty times digit.
func roSHA(digit string) string {
	return strings.Repeat(digit, 40)
}

// roHead is pull request number of repo at commit, open on main, its title naming no issue and its
// commit messages read and naming none, with no time.
func roHead(repo string, number int, commit string) PullRequestHead {
	return PullRequestHead{
		Repo: repo, Number: number, HeadSHA: commit, HeadRef: "work", BaseRef: "main", State: "open",
		Title: "Tidy the build", URL: fmt.Sprintf("https://github.com/%s/pull/%d", repo, number),
		Texts: &PullRequestTexts{HeadRepo: repo, BaseRef: "main", Title: "Tidy the build", CommitMessages: []string{}, MessagesRead: true},
	}
}

// roPull is roHead of deliveryRepo.
func roPull(number int, commit string) PullRequestHead {
	return roHead(deliveryRepo, number, commit)
}

// roTitled is head with a title that names issue, which credits it (attributedTerms).
func roTitled(head PullRequestHead, issue string) PullRequestHead {
	head.Title = issue + " the work"
	texts := *head.Texts
	texts.Title = head.Title
	head.Texts = &texts
	return head
}

// roTimed is head with the event time event and the read time read, nil for none.
func roTimed(head PullRequestHead, event, read *time.Time) PullRequestHead {
	head.EventAt, head.CheckedAt, head.GitHubReadAt = event, read, read
	return head
}

// roUnread is head of a read that read no commit message: its texts carry none, and it stamps no
// read time.
func roUnread(head PullRequestHead) PullRequestHead {
	texts := *head.Texts
	texts.CommitMessages, texts.MessagesRead = nil, false
	head.Texts, head.CheckedAt = &texts, nil
	return head
}

// roMerged is head merged into base by the merge commit commit.
func roMerged(head PullRequestHead, base, commit string) PullRequestHead {
	head.State, head.BaseRef, head.MergeCommitSHA = "merged", base, commit
	texts := *head.Texts
	texts.BaseRef = base
	head.Texts = &texts
	return head
}

// roClosed is head closed unmerged.
func roClosed(head PullRequestHead) PullRequestHead {
	head.State = "closed"
	return head
}

// roAnswer is a step that writes head under its own repository's name.
func roAnswer(head PullRequestHead) readOnlyStep {
	return readOnlyStep{head: &head}
}

// roAnswerUnder is a step that writes head of a read asked under the name under.
func roAnswerUnder(under string, head PullRequestHead) readOnlyStep {
	return readOnlyStep{head: &head, under: under}
}

// roSettles is a step that settles the reads owed for deliveryRepo's pull request number.
func roSettles(number int) readOnlyStep {
	return readOnlyStep{settle: &PullRequestKey{Repo: deliveryRepo, Number: number}}
}

// readOnlyHistory is the rows the writers stored (stored, and the reads owed after them, owes) and
// then the writes one read-only request's read-throughs made (request), and the commits whose
// place on a branch the history asks (branches: repository, branch, commit). matters is whether
// the request's writes change an answer, which the control holds.
type readOnlyHistory struct {
	name     string
	stored   []readOnlyStep
	owes     []int
	request  []readOnlyStep
	branches [][3]string
	matters  bool
}

// readOnlyHistories are main's fold histories in this design's form, and the settle and the
// unread messages of the read-through. Each is built anew by each call.
func readOnlyHistories() []readOnlyHistory {
	merge := roSHA("e")
	onRelease := [][3]string{{deliveryRepo, "release", merge}}
	credited := func(number int, commit string, event, read *time.Time) PullRequestHead {
		return roTimed(roTitled(roPull(number, commit), readOnlyIssue), event, read)
	}
	return []readOnlyHistory{
		{name: "a writer's row and no answer",
			stored: []readOnlyStep{roAnswer(credited(5, roSHA("1"), roAt(1), roAt(10)))}},
		{name: "an answer and no row", matters: true,
			request: []readOnlyStep{roAnswer(credited(5, roSHA("1"), roAt(1), roAt(20)))}},
		{name: "an answer over a row that credits nobody", matters: true, branches: onRelease,
			stored: []readOnlyStep{roAnswer(roTimed(roPull(5, roSHA("1")), roAt(1), roAt(10)))},
			request: []readOnlyStep{roAnswer(roTimed(roTitled(roMerged(roPull(5, roSHA("1")), "release", merge), readOnlyIssue),
				roAt(2), roAt(20)))}},
		{name: "an answer over a writer's read-through row of the same event",
			stored:  []readOnlyStep{roAnswer(credited(5, roSHA("1"), roAt(1), roAt(10)))},
			request: []readOnlyStep{roAnswer(credited(5, roSHA("1"), roAt(1), roAt(20)))}},
		{name: "an answer whose messages were not read over the linked row", matters: true,
			stored:  []readOnlyStep{roAnswer(roTimed(roPull(readOnlyLinkedPull, roSHA("1")), roAt(1), roAt(10)))},
			request: []readOnlyStep{roAnswer(roUnread(roTimed(roPull(readOnlyLinkedPull, roSHA("2")), roAt(2), nil)))}},
		{name: "a complete answer with no read time over a stored row", matters: true,
			stored:  []readOnlyStep{roAnswer(credited(5, roSHA("1"), roAt(1), roAt(10)))},
			request: []readOnlyStep{roAnswer(credited(5, roSHA("2"), roAt(2), nil))}},
		{name: "an answer with no event time over a stored row", matters: true,
			stored:  []readOnlyStep{roAnswer(credited(5, roSHA("1"), roAt(3), roAt(10)))},
			request: []readOnlyStep{roAnswer(credited(5, roSHA("2"), nil, roAt(20)))}},
		{name: "a merged pull request answered beside an open one stored", matters: true, branches: onRelease,
			stored: []readOnlyStep{roAnswer(credited(6, roSHA("6"), roAt(1), roAt(20)))},
			request: []readOnlyStep{roAnswer(roTimed(roTitled(roMerged(roPull(5, roSHA("1")), "release", merge), readOnlyIssue),
				roAt(1), roAt(20)))}},
		{name: "two answers of the request for one pull request", matters: true,
			request: []readOnlyStep{
				roAnswer(credited(5, roSHA("1"), roAt(1), roAt(20))),
				roAnswer(roUnread(roTimed(roPull(5, roSHA("2")), roAt(2), nil))),
			}},
		{name: "answers of the request for two pull requests interleaved", matters: true,
			request: []readOnlyStep{
				roAnswer(credited(5, roSHA("1"), roAt(1), roAt(20))),
				roAnswer(credited(6, roSHA("6"), roAt(3), roAt(22))),
				roAnswer(credited(5, roSHA("2"), roAt(2), roAt(25))),
			}},
		{name: "an answer of a read asked under a name its repository was renamed away from", matters: true,
			stored: []readOnlyStep{roAnswer(roTimed(roTitled(roHead(gadgetsOldRepo, 9, roSHA("7")), readOnlyLinkingIssue), roAt(1), roAt(10)))},
			request: []readOnlyStep{roAnswerUnder(gadgetsOldRepo,
				roTimed(roTitled(roHead(gadgetsRepo, 9, roSHA("8")), readOnlyLinkingIssue), roAt(2), roAt(20)))}},
		// What main's "many answers for one pull request" held that no other history does: a
		// closed answer, and a merged head read under the name its repository was renamed away
		// from.
		{name: "a closed answer and then a merged one, each of a read asked under a name its repository was renamed away from", matters: true,
			stored: []readOnlyStep{roAnswer(roTimed(roTitled(roHead(gadgetsOldRepo, 9, roSHA("7")), readOnlyLinkingIssue), roAt(1), roAt(10)))},
			request: []readOnlyStep{
				roAnswerUnder(gadgetsOldRepo,
					roTimed(roClosed(roTitled(roHead(gadgetsRepo, 9, roSHA("8")), readOnlyLinkingIssue)), roAt(2), roAt(20))),
				roAnswerUnder(gadgetsOldRepo,
					roTimed(roTitled(roMerged(roHead(gadgetsRepo, 9, roSHA("8")), "main", merge), readOnlyLinkingIssue), roAt(3), roAt(25))),
			}},
		{name: "a stored row whose event is newer than the request's answer",
			stored:  []readOnlyStep{roAnswer(roTimed(roPull(5, roSHA("1")), roAt(3), roAt(10)))},
			request: []readOnlyStep{roAnswer(credited(5, roSHA("2"), roAt(2), roAt(20)))}},
		{name: "two answers of the request out of event order", matters: true,
			request: []readOnlyStep{
				roAnswer(credited(5, roSHA("1"), roAt(3), roAt(20))),
				roAnswer(credited(5, roSHA("2"), roAt(2), roAt(25))),
			}},
		{name: "two answers equal but for their event times around a stored row's event", matters: true,
			stored: []readOnlyStep{roAnswer(credited(5, roSHA("1"), roAt(2), roAt(10)))},
			request: []readOnlyStep{
				roAnswer(credited(5, roSHA("2"), roAt(3), roAt(20))),
				roAnswer(credited(5, roSHA("2"), roAt(1), roAt(25))),
			}},
		{name: "the moved head", matters: true,
			stored:  []readOnlyStep{roAnswer(credited(5, roSHA("1"), roAt(1), roAt(10)))},
			request: []readOnlyStep{roAnswer(credited(5, roSHA("2"), roAt(2), roAt(20)))}},
		{name: "an answer settles the read its pull request is owed", matters: true,
			stored:  []readOnlyStep{roAnswer(credited(5, roSHA("1"), roAt(1), roAt(10)))},
			owes:    []int{5},
			request: []readOnlyStep{roAnswer(credited(5, roSHA("2"), roAt(2), roAt(20))), roSettles(5)}},
		{name: "an answer whose messages are not read is not known", matters: true,
			request: []readOnlyStep{roAnswer(roUnread(roTimed(roPull(6, roSHA("6")), roAt(1), nil)))}},
		{name: "a merged pull request the linking issue links answers its merged-in branch", matters: true, branches: onRelease,
			request: []readOnlyStep{roAnswer(roTimed(roMerged(roPull(readOnlyLinkedPull, roSHA("1")), "release", merge), roAt(1), roAt(20)))}},
	}
}

// readOnlyQuestions is the answer of each read of the results path a read-only caller's request
// passes its answers to, on pool, in one line each, for both fixture issues: the heads the
// issue's checks compare with (HeadsAttributedTo), the newest head a check CI proves is derived
// at (NewestPullRequestHead), the branch an `after_merge` check compares with
// (BaseBranchMergedFor), the linked pull requests whose head is not held (LinkedHeadsUnread); the
// branch the linking issue's pull request merged into in deliveryRepo (BaseBranchMergedIn); the
// pull requests of deliveryRepo and of gadgetsRepo whose answer is not known
// (PullRequestsNotKnown); and each commit of branches on its branch (Branches.ReadCommitOnBranch,
// with a branch cache and a carry of its own, and client). asked, where not nil, gets each read's
// answeredReads name.
func readOnlyQuestions(
	ctx context.Context, pool *store.Pool, a *Attributor, client *githubapp.Client, branches [][3]string,
	answers HeadAnswers, asked map[string]bool,
) (string, error) {
	note := func(name string) {
		if asked != nil {
			asked[name] = true
		}
	}
	var lines []string
	for _, issue := range []string{readOnlyIssue, readOnlyLinkingIssue} {
		heads, err := a.HeadsAttributedTo(ctx, pool, issue, answers)
		if err != nil {
			return "", err
		}
		note("Attributor.HeadsAttributedTo")
		for _, head := range heads {
			lines = append(lines, fmt.Sprintf("%s head %s#%d %.7s known=%t linked=%q",
				issue, head.Repo, head.Number, head.HeadSHA, head.HeadKnown, head.LinkedRepo))
		}
		newest, found, err := a.NewestPullRequestHead(ctx, pool, issue, answers)
		if err != nil {
			return "", err
		}
		note("Attributor.NewestPullRequestHead")
		lines = append(lines, fmt.Sprintf("%s newest found=%t %s#%d %.7s %s owed=%t",
			issue, found, newest.Repo, newest.Number, newest.HeadSHA, newest.State, newest.ReadOwed))
		merged, err := a.BaseBranchMergedFor(ctx, pool, issue, answers)
		if err != nil {
			return "", err
		}
		note("Attributor.BaseBranchMergedFor")
		lines = append(lines, fmt.Sprintf("%s merged into %q", issue, merged))
		unread, err := a.LinkedHeadsUnread(ctx, pool, issue, answers)
		if err != nil {
			return "", err
		}
		note("Attributor.LinkedHeadsUnread")
		for _, link := range unread {
			lines = append(lines, fmt.Sprintf("%s linked unread %s#%d", issue, link.Repo, link.Number))
		}
	}
	mergedIn, err := a.BaseBranchMergedIn(ctx, pool, readOnlyLinkingIssue, deliveryRepo, answers)
	if err != nil {
		return "", err
	}
	note("Attributor.BaseBranchMergedIn")
	lines = append(lines, fmt.Sprintf("%s merged in %s into %q", readOnlyLinkingIssue, deliveryRepo, mergedIn))
	for _, repo := range []string{deliveryRepo, gadgetsRepo} {
		notKnown, err := a.PullRequestsNotKnown(ctx, pool, repo, answers)
		if err != nil {
			return "", err
		}
		note("Attributor.PullRequestsNotKnown")
		for _, pull := range notKnown {
			lines = append(lines, fmt.Sprintf("not known %s#%d %.7s %s", repo, pull.Number, pull.HeadSHA, pull.State))
		}
	}
	for _, branch := range branches {
		on := (&Branches{}).ReadCommitOnBranch(ctx, pool, client, answers, &BranchCarry{}, branch[0], branch[1], branch[2], time.Now())
		note("Branches.ReadCommitOnBranch")
		lines = append(lines, fmt.Sprintf("%s@%.7s on %s: %v", branch[0], branch[2], branch[1], on))
	}
	return strings.Join(lines, "\n\t"), nil
}

// readOnlyStored is every row of each table a writer's read writes, whole, one row a line in one
// order: the heads, their texts, the reads owed and the branch answers, so a read that stored
// anything changes it.
func readOnlyStored(t *testing.T, ctx context.Context, pool *store.Pool) string {
	t.Helper()
	var stored string
	if err := pool.QueryRow(ctx, `
		select coalesce(string_agg(row, E'\n' order by row), '')
		from (
			select 'head ' || to_jsonb(h)::text as row from pull_request_heads h
			union all select 'texts ' || to_jsonb(x)::text from pull_request_texts x
			union all select 'owed ' || to_jsonb(o)::text from pull_request_reads_owed o
			union all select 'branch ' || to_jsonb(b)::text from branch_commits b
		) held
	`).Scan(&stored); err != nil {
		t.Fatalf("read the stored rows: %v", err)
	}
	return stored
}

// TestAReadOnlyReadReadsTheRowsTheWritersWritesLeave holds each read of the results path a
// read-only caller's request passes its answers to (readOnlyQuestions) to the writer's own path,
// history by history (readOnlyHistories: main's fold histories, in this design's form). Each
// history has two databases with the same rows. On one, the writers' steps are written and
// committed, and then the request's steps are kept as the read-only read keeps them
// (readOnlyStep.keep). On the other, the oracle, the writers' steps and then the request's steps
// are written with the writer's own code and committed. The read-only reads, with the request's
// answers, must give the oracle's answers, and the first database's rows must be what the writers
// stored, so the request stored nothing. Every read answeredReads names a read of the results
// path is asked.
//
// The controls: in every history where the request's writes change an answer (matters), the reads
// with no answers must not give the oracle's answer, so a read that ignored its answers fails
// here; where they change none, they must give it. In a history with a settle, the newest head of
// the issue must read owed before the request and settled in the oracle, and the request's writes
// with the settle left out must not give the oracle's answer, so a read that dropped the settle
// fails here.
func TestAReadOnlyReadReadsTheRowsTheWritersWritesLeave(t *testing.T) {
	t.Parallel()
	for _, history := range readOnlyHistories() {
		t.Run(history.name, func(t *testing.T) {
			t.Parallel()
			read, ctx, a := readOnlyDatabase(t)
			oracle, _, writer := readOnlyDatabase(t)
			client := newFakeGitHub(t).newTestClient()
			for _, database := range []struct {
				pool *store.Pool
				a    *Attributor
			}{{read, a}, {oracle, writer}} {
				for index, step := range history.stored {
					if err := step.write(ctx, database.a, database.pool); err != nil {
						t.Fatalf("SETUP: the writers' step %d: %v", index, err)
					}
				}
				for _, number := range history.owes {
					if err := database.a.owePullRequestRead(ctx, database.pool, deliveryRepo, number, owedFailure, ""); err != nil {
						t.Fatalf("SETUP: owe a read of #%d: %v", number, err)
					}
				}
			}
			kept := resolve(t, ctx, a, read, history.request)
			written := resolve(t, ctx, writer, oracle, history.request)
			before := readOnlyStored(t, ctx, read)
			ask := func(t *testing.T, answers HeadAnswers, asked map[string]bool) string {
				t.Helper()
				answered, err := readOnlyQuestions(ctx, read, a, client, history.branches, answers, asked)
				if err != nil {
					t.Fatalf("the read-only reads: %v", err)
				}
				return answered
			}
			atStart := ask(t, HeadAnswers{}, nil)

			var answers HeadAnswers
			for index, step := range kept {
				if err := step.keep(ctx, a, read, &answers); err != nil {
					t.Fatalf("keep the request's step %d: %v", index, err)
				}
			}
			for index, step := range written {
				if err := step.write(ctx, writer, oracle); err != nil {
					t.Fatalf("the oracle's step %d: %v", index, err)
				}
			}
			want, err := readOnlyQuestions(ctx, oracle, writer, client, history.branches, HeadAnswers{}, nil)
			if err != nil {
				t.Fatalf("the oracle's reads: %v", err)
			}
			asked := map[string]bool{}
			got := ask(t, answers, asked)
			t.Logf("the read-only reads answered\n\t%s", got)
			if got != want {
				t.Errorf("the read-only reads answered\n\t%s\nwant the writer's path's\n\t%s", got, want)
			}
			if after := readOnlyStored(t, ctx, read); after != before {
				t.Errorf("the read-only request stored rows: before\n\t%s\nafter\n\t%s", before, after)
			}
			for _, key := range slices.Sorted(maps.Keys(answeredReads)) {
				if answeredReads[key] == readAsked && !asked[key] && (key != "Branches.ReadCommitOnBranch" || len(history.branches) > 0) {
					t.Errorf("readOnlyQuestions asks nothing of %s, which answeredReads names a read of the results path", key)
				}
			}
			dropped := ask(t, HeadAnswers{}, nil)
			if history.matters && dropped == want {
				t.Errorf("CONTROL: the reads with no answers answered the writer's path's\n\t%s\nso this history cannot tell a read "+
					"that drops the request's writes from one that makes them", want)
			}
			if !history.matters && dropped != want {
				t.Errorf("the request's writes change nothing here, yet the reads with no answers answered\n\t%s\nwant\n\t%s", dropped, want)
			}
			if !slices.ContainsFunc(kept, func(step readOnlyStep) bool { return step.settle != nil }) {
				return
			}
			owedLine := fmt.Sprintf("%s newest found=true %s#5 %.7s open owed=true", readOnlyIssue, deliveryRepo, roSHA("1"))
			if !strings.Contains(atStart, owedLine) {
				t.Fatalf("SETUP: before the request the reads answered\n\t%s\nwant the line %q", atStart, owedLine)
			}
			if strings.Contains(want, "owed=true") {
				t.Errorf("SETUP: the oracle answered\n\t%s\nwant the owed read settled", want)
			}
			var unsettled HeadAnswers
			for index, step := range kept {
				if step.settle != nil {
					continue
				}
				if err := step.keep(ctx, a, read, &unsettled); err != nil {
					t.Fatalf("keep the request's step %d without the settle: %v", index, err)
				}
			}
			if withoutSettle := ask(t, unsettled, nil); withoutSettle == want {
				t.Errorf("CONTROL: the request's writes with the settle left out answered the writer's path's\n\t%s\nso this history "+
					"cannot tell a read that drops the settle", want)
			}
		})
	}
}

// TestAReadOnlyReadWithAConcurrentWriteReadsWhatTheCommittedWritesLeave holds a read-only read to
// the committed writes while another session writes the same pull request. The row stores #5 at
// A. The request kept a write of #5 at a newer head B (its read-through ran first). Then a
// writer's session writes #5 at C and holds its transaction open, so the read-only read, which
// makes B's write again first, waits on that session's lock of the row
// (storetest.WaitForLockWaiters names it waiting); then the session commits. The oracle is a
// database of its own where the writer's code writes A, then B, then C, and commits. The read must
// answer the oracle's in both orders of the two event times: C newer than B, where the guard keeps
// C, and C older, where it keeps B.
//
// The control is a request whose kept write is B written over the row with no guard: where C is
// newer, it reads B where the committed writes leave C, and the comparison must name the
// difference, so a read that ignored a write committed in its lock window fails here.
func TestAReadOnlyReadWithAConcurrentWriteReadsWhatTheCommittedWritesLeave(t *testing.T) {
	t.Parallel()
	credited := func(commit string, event, read *time.Time) PullRequestHead {
		return roTimed(roTitled(roPull(5, commit), readOnlyIssue), event, read)
	}
	stored := credited(roSHA("a"), roAt(0), roAt(1))
	kept := credited(roSHA("b"), roAt(20), roAt(21))
	// race answers what the read-only reads answer while C's session holds the row and then
	// commits, and what the committed writes leave.
	race := func(t *testing.T, concurrent PullRequestHead, planted bool) (got, want string) {
		t.Helper()
		read, ctx, a := readOnlyDatabase(t)
		oracle, _, writer := readOnlyDatabase(t)
		for _, step := range []readOnlyStep{roAnswer(stored), roAnswer(kept), roAnswer(concurrent)} {
			if err := step.write(ctx, writer, oracle); err != nil {
				t.Fatalf("SETUP: the oracle's write: %v", err)
			}
		}
		want, err := readOnlyQuestions(ctx, oracle, writer, nil, nil, HeadAnswers{}, nil)
		if err != nil {
			t.Fatalf("the oracle's reads: %v", err)
		}
		if err := roAnswer(stored).write(ctx, a, read); err != nil {
			t.Fatalf("SETUP: store A: %v", err)
		}
		var answers HeadAnswers
		if planted {
			answers.writes = append(answers.writes, func(ctx context.Context, db refs.Pool) error {
				if _, err := db.Exec(ctx, `update pull_request_heads set head_sha = $3, event_at = $4 where repo = $1 and number = $2`,
					kept.Repo, kept.Number, kept.HeadSHA, *kept.EventAt); err != nil {
					return err
				}
				_, err := db.Exec(ctx, `update pull_request_texts set head_sha = $3, updated_at = $4 where repo = $1 and number = $2`,
					kept.Repo, kept.Number, kept.HeadSHA, *kept.EventAt)
				return err
			})
		} else if err := roAnswer(kept).keep(ctx, a, read, &answers); err != nil {
			t.Fatalf("keep B: %v", err)
		}
		holder, err := read.Begin(ctx)
		if err != nil {
			t.Fatalf("open the concurrent writer's transaction: %v", err)
		}
		defer func() { _ = holder.Rollback(context.WithoutCancel(ctx)) }()
		if err := roAnswer(concurrent).write(ctx, a, holder); err != nil {
			t.Fatalf("write C in the concurrent session: %v", err)
		}
		type answered struct {
			text string
			err  error
		}
		done := make(chan answered, 1)
		go func() {
			text, err := readOnlyQuestions(ctx, read, a, nil, nil, answers, nil)
			done <- answered{text, err}
		}()
		storetest.WaitForLockWaiters(t, holder, 1)
		if err := holder.Commit(ctx); err != nil {
			t.Fatalf("commit C: %v", err)
		}
		result := <-done
		if result.err != nil {
			t.Fatalf("the read-only reads: %v", result.err)
		}
		return result.text, want
	}
	for _, row := range []struct {
		name       string
		concurrent PullRequestHead
		wantHead   string
	}{
		{"the concurrent write is newer", credited(roSHA("c"), roAt(30), roAt(31)), roSHA("c")},
		{"the concurrent write is older", credited(roSHA("c"), roAt(10), roAt(31)), roSHA("b")},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			got, want := race(t, row.concurrent, false)
			if !strings.Contains(want, fmt.Sprintf("%s head %s#5 %.7s ", readOnlyIssue, deliveryRepo, row.wantHead)) {
				t.Fatalf("SETUP: the committed writes leave\n\t%s\nwant #5 at %.7s", want, row.wantHead)
			}
			t.Logf("the read-only reads, waited on the concurrent write, answered\n\t%s", got)
			if got != want {
				t.Errorf("the read-only reads answered\n\t%s\nwant what the committed writes leave\n\t%s", got, want)
			}
		})
	}
	t.Run("control: a write that ignores the guard reads past the committed write", func(t *testing.T) {
		t.Parallel()
		got, want := race(t, credited(roSHA("c"), roAt(30), roAt(31)), true)
		t.Logf("the planted request answered\n\t%s\nthe committed writes leave\n\t%s", got, want)
		if got == want {
			t.Errorf("CONTROL: a request whose write ignores the guard answered the committed writes'\n\t%s\nso this test "+
				"cannot tell a read that overrides a write committed in its lock window", want)
		}
		if !strings.Contains(got, fmt.Sprintf("%s head %s#5 %.7s ", readOnlyIssue, deliveryRepo, roSHA("b"))) {
			t.Errorf("CONTROL: the planted request answered\n\t%s\nwant #5 at B, the write that overrides the committed C", got)
		}
	})
}
