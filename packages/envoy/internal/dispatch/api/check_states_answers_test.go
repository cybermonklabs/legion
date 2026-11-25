// check_states_answers_test.go holds the settler to how GitHub's answer about a pull request's
// candidate former heads is applied (verifyFormerHeads): an answer applies only to the candidates
// it read, while the claim it was read under is still the one in force and the pull request is
// still as it was when the read began; any other answer, positive or negative, applies nothing,
// and the candidates are read again. And it holds the settler to counting one verification apart
// from the requests the verification sent. The rig is check_states_test.go's.
package api

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/logtest"
)

// settleInTheBackground runs one settle of issue by settler on a goroutine of its own and answers
// a channel that closes once it ends.
func settleInTheBackground(settler *checkStatesSettler, issue string) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		settler.settle(store.WithTransactionTracking(context.Background()), issue)
	}()
	return done
}

// settleEveryMark brings every mark of the rig due and settles until none is due.
func settleEveryMark(t *testing.T, rig *checkStatesRig) []checkStatesEvent {
	t.Helper()
	if _, err := rig.database.Pool.Exec(context.Background(), `update check_states_owed set due_at = now()`); err != nil {
		t.Fatalf("bring every mark due: %v", err)
	}
	return settleOwed(t, rig)
}

// TestAMembershipAnswerAppliesOnlyToTheCandidateItRead: the tester recorded k1 passing at H1 and
// #70 stands at H2, so H1 is #70's candidate. A settle reads #70's commits, which GitHub lists as
// H2 alone, and is held after GitHub answered and before it applies the answer.
//
// Where nothing changed meanwhile, the answer applies: H1 is dropped and nothing is published
// there. Where #70 closed meanwhile, the tester of another issue recorded k1 passing at H1 after
// the close, #70 reopened at H3 under that issue, and that issue's settle left H1 as a candidate
// of the new lifecycle, which GitHub now lists, the old answer is about a candidate that is no
// more: it applies nothing, and the new issue's settle reads #70 again and publishes k1 yes at H1.
func TestAMembershipAnswerAppliesOnlyToTheCandidateItRead(t *testing.T) {
	for _, reopen := range []bool{false, true} {
		name := "the candidate the read named is unchanged"
		if reopen {
			name = "the pull request closed and reopened under another issue meanwhile"
		}
		t.Run(name, func(t *testing.T) {
			rig := newCheckStatesRig(t, checkStatesTwoChecks)
			eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH1)
			upsertHead(t, rig, 70, eachHeadH2, rig.issue, "A change")
			if got := candidatesOf(t, rig); !maps.Equal(got, map[string]bool{eachHeadH1: false}) {
				t.Fatalf("#70 holds the former heads %v, want H1 unverified: this test holds nothing", got)
			}
			rig.github.setPullCommits(70, eachHeadH2)
			entered, release := rig.github.holdPullCommitsAnswer(t, 70)
			first := settleInTheBackground(rig.settler, rig.issue)
			ciAwait(t, "the first settle's read of #70's commits", entered)
			other := rig.issue
			if reopen {
				other = rig.fileIssue(t, "The reopened pull request's issue")
				uploadDocument(t, rig.handler, "/api/v1/issues/"+other, "verification.md", checkStatesTwoChecks)
				writeHead(t, rig.database, ciJobRepository, 70, eachHeadH2, "closed", rig.issue)
				eachHeadResult(t, rig.handler, other, "k1", "tester", "pass", eachHeadH1)
				writeHead(t, rig.database, ciJobRepository, 70, eachHeadH3, "open", other)
				rig.github.setPullCommits(70, eachHeadH1, eachHeadH3)
				second := rig.newSettler(t, "check states of the reopened pull request's issue")
				if ran, failed, _ := second.settle(store.WithTransactionTracking(t.Context()), other); !ran || failed {
					t.Fatalf("the settle of %s answered ran=%v failed=%v, want a settle that ran", other, ran, failed)
				}
			}
			release()
			ciAwait(t, "the first settle to end", first)
			settleEveryMark(t, rig)
			if !reopen {
				if got := candidatesOf(t, rig); len(got) != 0 {
					t.Fatalf("#70 holds the former heads %v after GitHub's answer left H1 out, want none", got)
				}
				if got := stateAt(t, rig, rig.issue, eachHeadH1, "k1"); got != "" {
					t.Fatalf("k1 is published %q at H1, which #70's commits do not list, want nothing", got)
				}
				return
			}
			if got := stateAt(t, rig, other, eachHeadH1, "k1"); got != model.PassStandsYes {
				t.Fatalf("k1 at H1, a candidate of the reopened #70 that GitHub lists, is published %q at %s, want yes: the answer about the closed lifecycle's candidate deleted it", got, other)
			}
		})
	}
}

// TestAnAnswerOfAClaimAnotherSettleTookOverAppliesNothing: the tester recorded k1 passing at H1 on
// two issues, and #70 stands at H2 on the first, so H1 is #70's candidate. The first issue's settle
// claims #70's read and reads its commits, which GitHub lists as H2 alone, and is held after
// GitHub answered. Its claim runs out meanwhile, #70 moves on to H3 and to the second issue, and
// GitHub now lists H1 and H3. The second issue's settle takes the claim over and reads #70's
// commits, and is held after GitHub answered too. The first answer is then applied before the
// second: it applies nothing, since its claim was taken over, and the second applies, so k1 is
// published yes at H1 on the second issue.
//
// A claimant whose settle stopped mid-read is taken over all the same: the second settle reads
// #70 and publishes k1 yes at H1.
func TestAnAnswerOfAClaimAnotherSettleTookOverAppliesNothing(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		name := "the first claimant applies its answer after the takeover"
		if stopped {
			name = "the first claimant stopped mid-read"
		}
		t.Run(name, func(t *testing.T) {
			rig := newCheckStatesRig(t, checkStatesTwoChecks)
			other := rig.fileIssue(t, "Where the pull request moves")
			uploadDocument(t, rig.handler, "/api/v1/issues/"+other, "verification.md", checkStatesTwoChecks)
			eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH1)
			eachHeadResult(t, rig.handler, other, "k1", "tester", "pass", eachHeadH1)
			upsertHead(t, rig, 70, eachHeadH2, rig.issue, "A change")
			if got := candidatesOf(t, rig); !maps.Equal(got, map[string]bool{eachHeadH1: false}) {
				t.Fatalf("#70 holds the former heads %v, want H1 unverified: this test holds nothing", got)
			}
			rig.github.setPullCommits(70, eachHeadH2)
			firstEntered, firstRelease := rig.github.holdPullCommitsAnswer(t, 70)
			secondEntered, secondRelease := rig.github.holdPullCommitsAnswer(t, 70)
			ctx, cancel := context.WithCancel(store.WithTransactionTracking(context.Background()))
			defer cancel()
			first := make(chan struct{})
			go func() {
				defer close(first)
				rig.settler.settle(ctx, rig.issue)
			}()
			ciAwait(t, "the first settle's read of #70's commits", firstEntered)
			if stopped {
				cancel()
				ciAwait(t, "the first settle to stop", first)
			}
			if _, err := rig.database.Pool.Exec(context.Background(),
				`update pull_request_former_heads_reads set until = now() - interval '1 second'`); err != nil {
				t.Fatalf("let the first settle's claim run out: %v", err)
			}
			rig.github.setPullCommits(70, eachHeadH1, eachHeadH3)
			upsertHead(t, rig, 70, eachHeadH3, other, "A change")
			second := settleInTheBackground(rig.newSettler(t, "check states of the other issue"), other)
			ciAwait(t, "the second settle's read of #70's commits", secondEntered)
			if !stopped {
				firstRelease()
				ciAwait(t, "the first settle to end", first)
			}
			secondRelease()
			ciAwait(t, "the second settle to end", second)
			settleEveryMark(t, rig)
			if got := stateAt(t, rig, other, eachHeadH1, "k1"); got != model.PassStandsYes {
				t.Fatalf("k1 at H1, which the claim's new holder read as #70's, is published %q at %s, want yes", got, other)
			}
		})
	}
}

// TestAPositiveAnswerAboutAnEarlierHeadAppliesNothing: the tester recorded k1 passing at H1 and
// #70 stands at H2, so H1 is #70's candidate. A settle reads #70's commits, which GitHub lists as
// H1 and H2, and is held after GitHub answered. Meanwhile #70's branch is force-pushed to H3, and
// GitHub lists H3 alone. The answer is about #70 as it was at H2: it applies nothing, and the
// settle reads #70 again, which leaves H1 out, so H1 is dropped and nothing is published there.
func TestAPositiveAnswerAboutAnEarlierHeadAppliesNothing(t *testing.T) {
	rig := newCheckStatesRig(t, checkStatesTwoChecks)
	eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH1)
	upsertHead(t, rig, 70, eachHeadH2, rig.issue, "A change")
	if got := candidatesOf(t, rig); !maps.Equal(got, map[string]bool{eachHeadH1: false}) {
		t.Fatalf("#70 holds the former heads %v, want H1 unverified: this test holds nothing", got)
	}
	rig.github.setPullCommits(70, eachHeadH1, eachHeadH2)
	entered, release := rig.github.holdPullCommitsAnswer(t, 70)
	first := settleInTheBackground(rig.settler, rig.issue)
	ciAwait(t, "the settle's read of #70's commits", entered)
	rig.github.setPullCommits(70, eachHeadH3)
	upsertHead(t, rig, 70, eachHeadH3, rig.issue, "A change")
	release()
	ciAwait(t, "the settle to end", first)
	settleEveryMark(t, rig)
	if got := stateAt(t, rig, rig.issue, eachHeadH1, "k1"); got != "" {
		t.Fatalf("k1 is published %q at H1, accepted on an answer about #70 at H2 after #70 was force-pushed to H3, which does not hold H1; want nothing", got)
	}
	if reads := rig.github.pullCommitReadsOf(70); reads != 2 {
		t.Fatalf("#70's commits were read %d times, want twice: the answer about the earlier head, and the read again", reads)
	}
}

// TestAVerificationIsCountedApartFromTheRequestsItSent: #70 holds 502 commits and stands at the
// last; the install left the 501st, where the tester recorded k1 passing, as its candidate. GitHub
// compares #70's base revision and head in pages of 100, five of them, past which the settle reads
// no page, and answers the second page 502 once, so it is asked again; the 501st, which no page
// names, is then decided by two comparisons of its own after one read of #70. That is one
// verification and nine requests: six of the comparison's pages, one of #70, two comparisons. The
// verification's log line names the requests apart from the five pages it read, and the settle's
// line names the one verification.
func TestAVerificationIsCountedApartFromTheRequestsItSent(t *testing.T) {
	records := &logtest.Recorder{}
	logtest.SetDefault(t, records)
	rig := newCheckStatesRig(t, checkStatesTwoChecks)
	commits := make([]string, 502)
	for index := range commits {
		commits[index] = fmt.Sprintf("%040x", 0xc0000+index)
	}
	h1, h2 := commits[500], commits[501]
	eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", h1)
	// A first verification of another pull request, at a commit its comparison names, so the read
	// under test finds the installation's token held and sends no request for it.
	eachHeadResult(t, rig.handler, rig.issue, "k2", "tester", "pass", eachHeadH1)
	upsertHead(t, rig, 71, eachHeadH2, rig.issue, "Another change")
	rig.github.setPullCommits(71, eachHeadH1, eachHeadH2)
	settleOwed(t, rig)
	if rig.github.pullCommitReadsOf(71) == 0 {
		t.Fatal("#71's commits were never read, so the token the read under test uses was never asked for")
	}
	upsertHead(t, rig, 70, h2, rig.issue, "A change")
	forgetCandidates(t, rig)
	settleOwed(t, rig)
	rig.github.setPullCommits(70, commits...)
	rig.github.setPull(70, h2, "main", ciJobBaseSHA)
	rig.github.setCompare(h2, h1, ciJobCompare{status: "behind"})
	rig.github.setCompare(ciJobBaseSHA, h1, ciJobCompare{status: "ahead"})
	rig.github.failPullCommitsPageOnce(2, 502)
	seedInstallCandidates(t, rig, h1)
	before := len(records.Records())
	settleOwed(t, rig)
	if got := stateAt(t, rig, rig.issue, h1, "k1"); got != model.PassStandsYes {
		t.Fatalf("k1 at the 501st commit is published %q, want yes: this test holds nothing", got)
	}
	sent := rig.github.pullCommitReadsOf(70) + rig.github.pullReadsOf(70) + rig.github.compareReadsOf(h2, h1) + rig.github.compareReadsOf(ciJobBaseSHA, h1)
	if sent != 9 {
		t.Fatalf("GitHub was asked %d times about #70's commits, want 9: six pages of the comparison, one of them twice, #70 itself and two comparisons", sent)
	}
	attrs := func(record slog.Record) map[string]any {
		held := map[string]any{}
		record.Attrs(func(attr slog.Attr) bool {
			held[attr.Key] = attr.Value.Any()
			return true
		})
		return held
	}
	var verified, settled map[string]any
	for _, record := range records.Records()[before:] {
		held := attrs(record)
		switch {
		case record.Message == "dispatch: check states verified the candidate former heads of a pull request" && held["number"] == int64(70):
			verified = held
		case record.Message == "dispatch: check states settled" && verified != nil && settled == nil:
			settled = held
		}
	}
	if verified == nil || settled == nil {
		t.Fatalf("no log line of #70's verification and its settle was written: verification %v, settle %v", verified, settled)
	}
	if verified["pages"] != int64(5) || verified["requests"] != int64(sent) {
		t.Fatalf("the verification's line names pages=%v requests=%v, want pages=5, the logical pages of the comparison, and requests=%d, what GitHub was asked", verified["pages"], verified["requests"], sent)
	}
	if settled["verifications"] != int64(1) || settled["requests"] != int64(sent) {
		t.Fatalf("the settle's line names verifications=%v requests=%v, want one verification and its %d requests", settled["verifications"], settled["requests"], sent)
	}
}
