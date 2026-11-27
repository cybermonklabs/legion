package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// TestAReadOnlyResultsReadAnswersAsAWriterInEveryState is the results row's state table (plan
// D26): GET /api/v1/issues/{key}/results read by a read-only token in each state, each read
// compared with its oracle, the writer's own read of the same identity at that step. The oracle
// runs on a stack of its own, built by running every step the read-only stack ran before that
// read, at the same moments and on the same stand-in GitHub script (parityPair.read): the claims,
// settles, holds and cache entries a state needs are made on it as on the read-only stack, by the
// writer's own paths, and never set by hand. Each read asserts that the two answer one body and
// make the same GitHub calls, and that the read-only read left its stack's rows, events and
// in-process state as it found them; each row asserts what the oracle answers, which the table's
// last column says. Row 20 holds that a writer's read after a read-only read is the writer's read
// with none before it.
//
// Each row starts from fresh stacks and makes, itself, every step its state names, so no row
// reads what the row before it left and each row runs alone.
func TestAReadOnlyResultsReadAnswersAsAWriterInEveryState(t *testing.T) {
	t.Parallel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate fixture key: %v", err)
	}
	appKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))

	t.Run("1 the result's commit is the head of a pull request Dispatch stores", func(t *testing.T) {
		pair := newParityPair(t, appKey)
		issue := pair.issue(t, "ROA", parityBeforeMergeCheck)
		learned := pair.clock.now().Add(-time.Hour)
		pair.each(func(stack *parityStack) {
			storeParityHead(t, stack.rig.database, 41, headCommit, issue, learned)
		})
		pair.record(t, issue, "k1", headCommit)

		read := pair.read(t, issue, "the first read")
		stands := standsAt(t, read.writer, "k1", pair.writer.ids[0], "the writer's result at a stored head", model.CommitAtBasis, false)
		if stands.Basis == nil || stands.Basis.PullRequest == nil || *stands.Basis.PullRequest != 41 {
			t.Errorf("the writer stands the result at %+v, want pull request 41", stands.Basis)
		}
		read.callsNone(t)
	})

	// pushed is the state rows 2 to 5 read: the issue's check k1 holds a result at
	// behindHeadCommit, which no stored pull request heads, and its sibling check k3 a result at
	// headCommit, the head of the issue's pull request 61, which no stored row holds. GitHub names
	// no pull request for either commit yet.
	pushed := func(t *testing.T) (*parityPair, string) {
		t.Helper()
		pair := newParityPair(t, appKey)
		issue := pair.issue(t, "ROB", siblingChecks)
		pair.record(t, issue, "k1", behindHeadCommit)
		pair.record(t, issue, "k3", headCommit)
		return pair, issue
	}
	// opens makes GitHub name pull request 61, headed at headCommit and titled after the issue,
	// for either commit from then on.
	opens := func(pair *parityPair, issue string) {
		pair.github.answersPulls(func(string) string { return parityOpenPull(61, headCommit, issue) })
	}
	// pushedPast is row 2's state: pushed, with GitHub naming pull request 61.
	pushedPast := func(t *testing.T) (*parityPair, string) {
		t.Helper()
		pair, issue := pushed(t)
		opens(pair, issue)
		return pair, issue
	}
	atPull61 := fmt.Sprintf("pull request 61 at %.7s", headCommit)
	// fromPull61 asserts the answer of a read on rows that hold pull request 61, or whose
	// read-through learned it: both checks compare with 61 alone, k1's result is at another
	// commit and k3's stands at 61's head.
	fromPull61 := func(t *testing.T, pair *parityPair, read parityRead) {
		t.Helper()
		for _, check := range []string{"k1", "k3"} {
			if bases := parityBases(checkOf(t, read.writer, check)); !slices.Equal(bases, []string{atPull61}) {
				t.Errorf("the writer's %s compared with %v, want %s alone", check, bases, atPull61)
			}
		}
		standsAt(t, read.writer, "k1", pair.writer.ids[0], "the writer's result at the commit the push left behind", model.CommitOtherCommit, false)
		standsAtBasis(t, read.writer, "k3", pair.writer.ids[1], atPull61)
	}

	t.Run("2 a result at a commit no stored pull request heads, beside the issue's head no stored row holds", func(t *testing.T) {
		pair, issue := pushedPast(t)
		read := pair.read(t, issue, "the first read")
		// The writer asks GitHub for behindHeadCommit and stores pull request 61, whose head is
		// headCommit. headCommit is then headed by a row, so it asks GitHub nothing for it. The
		// bases are read again after the read-through (plan D32), so both results are answered on
		// the heads as they stand after it.
		fromPull61(t, pair, read)
		read.calls(t, parityPulls, 1)
	})

	t.Run("3 as row 2, read again", func(t *testing.T) {
		pair, issue := pushedPast(t)
		pair.read(t, issue, "the first read")
		pair.clock.advance(time.Minute)
		second := pair.read(t, issue, "the second read")
		// The read-only read stored nothing and took no claim, so the writer's read at this
		// moment, on the same rows, asks GitHub for behindHeadCommit again and learns pull
		// request 61 at this read.
		fromPull61(t, pair, second)
		second.calls(t, parityPulls, 1)
		if bases := checkOf(t, second.writer, "k3").ComparedWith; len(bases) != 1 || bases[0].LearnedAt == nil ||
			bases[0].LearnedAt.Kind != model.BasisLearnedReadAt || !bases[0].LearnedAt.At.Equal(pair.clock.now()) {
			t.Errorf("the writer's k3 compared with %+v, want pull request 61 alone, read at the second read, %s", bases, pair.clock.now())
		}
	})

	t.Run("4 a writer's claim for the commit stands", func(t *testing.T) {
		pair, issue := pushed(t)
		// The claim is made by a writer's read, on every stack: GitHub names no pull request for
		// either commit yet, so that read claims both (server.commitPulls.Claim, in
		// comparisonReader.readThroughHead, under the issue's canonical repository name) and
		// stores nothing.
		claimed := pair.writersRead(t, issue, "the writer's read that claims both commits")
		claimed.calls(t, parityPulls, 2)
		// A minute later pull request 61 has opened, and no event has told Dispatch of it.
		opens(pair, issue)
		pair.clock.advance(time.Minute)

		read := pair.read(t, issue, "the read inside the writer's claim")
		// The claim refuses a read-through of either commit, so the stored rows answer: no head,
		// and both results at another commit.
		for index, check := range []string{"k1", "k3"} {
			standsAt(t, read.writer, check, pair.writer.ids[index], "the writer's result of "+check, model.CommitOtherCommit, false)
			if compared := checkOf(t, read.writer, check); compared.ComparedNothing != model.ReasonNoPullRequestOrBranch {
				t.Errorf("the writer's check %s compared with %v (%q), want nothing, %q",
					check, parityBases(compared), compared.ComparedNothing, model.ReasonNoPullRequestOrBranch)
			}
		}
		read.callsNone(t)
	})

	t.Run("5 a writer's settle for the commit stands", func(t *testing.T) {
		// The read below comes delivery.CommitPullReadStaleAfter after the settle and expects the
		// default-branch cache entry, held for delivery.BranchCommitStaleAfter, to have lapsed with
		// the claim's span, so its one GET /repos/{owner}/{repo} holds only while the two are equal.
		if delivery.CommitPullReadStaleAfter != delivery.BranchCommitStaleAfter {
			t.Fatalf("delivery.CommitPullReadStaleAfter is %v and delivery.BranchCommitStaleAfter is %v: this row reads at the first and holds the default-branch read to the second, so it cannot stand while they differ",
				delivery.CommitPullReadStaleAfter, delivery.BranchCommitStaleAfter)
		}
		pair, issue := pushedPast(t)
		// The settle is made by a writer's read, on every stack: it reads behindHeadCommit
		// through, stores pull request 61, whose head is another commit, and settles
		// behindHeadCommit for delivery.CommitPullSettledFor (server.commitPulls.Settle, in
		// comparisonReader.readThroughHead).
		settled := pair.writersRead(t, issue, "the writer's read that settles behindHeadCommit")
		settled.calls(t, parityPulls, 1)
		// The read is past the claim's span and inside the settle's.
		pair.clock.advance(delivery.CommitPullReadStaleAfter)

		read := pair.read(t, issue, "the read inside the writer's settle")
		// The settle refuses a read-through of behindHeadCommit, and the stored row of 61 heads
		// headCommit, so the stored rows answer. The default-branch cache entry the writer's read
		// put lapsed with the claim's span, so the read's one call is the repository's own read.
		fromPull61(t, pair, read)
		if want := map[string]int{parityRepository: 1}; !maps.Equal(read.writerCalls, want) {
			t.Errorf("the writer's read made the GitHub calls %v, want %v", read.writerCalls, want)
		}
	})

	t.Run("6 an after_merge check whose commit a stored row places on the named branch", func(t *testing.T) {
		pair := newParityPair(t, appKey)
		issue := pair.issue(t, "ROC", parityAfterMergeCheck)
		pair.each(func(stack *parityStack) { recordMerge(t, stack.rig.database, parityRepo, 51, issue, "integration/x") })
		pair.github.answersCompare(func(base, head string) (int, string) {
			if base == "integration/x" && head == integrationCommit {
				return http.StatusOK, "behind"
			}
			return http.StatusOK, "diverged"
		})
		pair.record(t, issue, "m1", integrationCommit)
		// A writer's read asks GitHub, stores the yes and puts the default-branch cache entry.
		stored := pair.writersRead(t, issue, "the writer's read that stores the yes")
		stored.calls(t, parityCompare, 1)
		pair.clock.advance(time.Minute)

		read := pair.read(t, issue, "the read of the stored yes")
		standsAtBranch(t, read.writer, "m1", pair.writer.ids[0], "integration/x")
		read.callsNone(t)
	})

	t.Run("7 an after_merge check on a branch not the default, held as unseen by another commit's refused compare read", func(t *testing.T) {
		pair := newParityPair(t, appKey)
		issue := pair.issue(t, "ROD", parityAfterMergeCheck)
		pair.each(func(stack *parityStack) { recordMerge(t, stack.rig.database, parityRepo, 52, issue, "integration/x") })
		// integration/x is deleted, so GitHub answers every compare read against it 404. A
		// compare read of main for integrationCommit fails, and every other says diverged.
		pair.github.answersCompare(func(base, head string) (int, string) {
			switch {
			case base == "integration/x":
				return http.StatusNotFound, ""
			case head == integrationCommit:
				return http.StatusBadGateway, ""
			}
			return http.StatusOK, "diverged"
		})
		pair.record(t, issue, "m1", strayCommit)
		// The hold is made by a writer's read, on every stack: its compare read of strayCommit
		// against integration/x is refused, which holds that branch as unseen for
		// delivery.BranchCommitStaleAfter (delivery.Branches.markUnseen), and its compare read of
		// main stores strayCommit's no.
		held := pair.writersRead(t, issue, "the writer's read that holds integration/x as unseen")
		held.calls(t, parityCompare, 2)
		pair.record(t, issue, "m1", integrationCommit)
		pair.clock.advance(time.Minute)

		read := pair.read(t, issue, "the read inside the hold")
		// integrationCommit, which nothing stored places: the hold answers integration/x unseen
		// with no call, and the default branch's compare read fails, so the walk ends unknown.
		stands := standsAt(t, read.writer, "m1", pair.writer.ids[1], "the writer's result no stored row places", model.CommitUnknown, false)
		if stands.Reason != model.ReasonReadFailed {
			t.Errorf("the writer's result no stored row places is unknown for %q, want %q", stands.Reason, model.ReasonReadFailed)
		}
		standsAt(t, read.writer, "m1", pair.writer.ids[0], "the writer's result whose no is stored", model.CommitOtherCommit, false)
		// The compare read of main for integrationCommit, and none against integration/x.
		read.calls(t, parityCompare, 1)
	})

	t.Run("8 a default_branch check whose repository's own read is unreadable", func(t *testing.T) {
		pair := newParityPair(t, appKey)
		issue := pair.issue(t, "ROE", parityDefaultBranchCheck)
		pair.record(t, issue, "d1", mergeCommit)
		pair.github.loseRepository()

		read := pair.read(t, issue, "the first read")
		stands := standsAt(t, read.writer, "d1", pair.writer.ids[0], "the writer's result", model.CommitUnknown, false)
		if stands.Reason != model.ReasonRepositoryUnreadable {
			t.Errorf("the writer's result is unknown for %q, want %q", stands.Reason, model.ReasonRepositoryUnreadable)
		}
		read.calls(t, parityCompare, 0)
	})

	// refusedUnseen is row 9's state: a default_branch check's result, nothing stored, and GitHub
	// refusing every compare read as unseen (404).
	refusedUnseen := func(t *testing.T) (*parityPair, string) {
		t.Helper()
		pair := newParityPair(t, appKey)
		issue := pair.issue(t, "ROF", parityDefaultBranchCheck)
		pair.github.answersCompare(func(string, string) (int, string) { return http.StatusNotFound, "" })
		pair.record(t, issue, "d1", mergeCommit)
		return pair, issue
	}

	t.Run("9 a default_branch check, nothing stored, the compare read refused as unseen", func(t *testing.T) {
		pair, issue := refusedUnseen(t)
		read := pair.read(t, issue, "the first read")
		stands := standsAt(t, read.writer, "d1", pair.writer.ids[0], "the writer's result", model.CommitUnknown, false)
		if stands.Reason != model.ReasonReadFailed {
			t.Errorf("the writer's result is unknown for %q, want %q", stands.Reason, model.ReasonReadFailed)
		}
		read.calls(t, parityCompare, 1)
	})

	t.Run("10 a default_branch check, nothing stored, the compare read fails", func(t *testing.T) {
		pair := newParityPair(t, appKey)
		issue := pair.issue(t, "ROG", parityDefaultBranchCheck)
		pair.github.answersCompare(func(string, string) (int, string) { return http.StatusBadGateway, "" })
		pair.record(t, issue, "d1", mergeCommit)
		read := pair.read(t, issue, "the first read")
		stands := standsAt(t, read.writer, "d1", pair.writer.ids[0], "the writer's result", model.CommitUnknown, false)
		if stands.Reason != model.ReasonReadFailed {
			t.Errorf("the writer's result is unknown for %q, want %q", stands.Reason, model.ReasonReadFailed)
		}
		read.calls(t, parityCompare, 1)
	})

	t.Run("11 a default_branch check whose stored row says no, inside the window", func(t *testing.T) {
		pair := newParityPair(t, appKey)
		issue := pair.issue(t, "ROH", parityDefaultBranchCheck)
		pair.record(t, issue, "d1", strayCommit)
		// A writer's read asks GitHub, stores the no and puts the default-branch cache entry.
		stored := pair.writersRead(t, issue, "the writer's read that stores the no")
		stored.calls(t, parityCompare, 1)
		pair.clock.advance(time.Minute)

		read := pair.read(t, issue, "the read of the stored no")
		standsAt(t, read.writer, "d1", pair.writer.ids[0], "the writer's result off the trunk", model.CommitOtherCommit, false)
		read.callsNone(t)
	})

	// lapsedCache is row 12's state: a default_branch check's result at a commit main carries,
	// nothing stored, and the repository's default-branch cache entry lapsed. The entry is put by
	// a writer's read before any result is recorded, on every stack, and lapses
	// delivery.BranchCommitStaleAfter later.
	lapsedCache := func(t *testing.T) (*parityPair, string) {
		t.Helper()
		pair := newParityPair(t, appKey)
		issue := pair.issue(t, "ROI", parityDefaultBranchCheck)
		pair.github.answersCompare(func(base, head string) (int, string) {
			if base == "main" && head == mergeCommit {
				return http.StatusOK, "behind"
			}
			return http.StatusOK, "diverged"
		})
		put := pair.writersRead(t, issue, "the writer's read that puts the default-branch cache entry")
		put.calls(t, parityRepository, 1)
		pair.record(t, issue, "d1", mergeCommit)
		pair.clock.advance(delivery.BranchCommitStaleAfter)
		return pair, issue
	}

	t.Run("12 a default_branch check whose default-branch cache entry has lapsed", func(t *testing.T) {
		pair, issue := lapsedCache(t)
		// The read reads the default branch again and asks GitHub. That the entry is still lapsed
		// afterwards is the read's own check that the read-only read left the server's in-process
		// state as it found it (parityPair.read).
		read := pair.read(t, issue, "the read past the cache entry")
		standsAtBranch(t, read.writer, "d1", pair.writer.ids[0], "main")
		read.calls(t, parityRepository, 1)
		read.calls(t, parityCompare, 1)
	})

	// onIntegrationBranch is the state of row 13: an after_merge check of an issue merged into
	// integration/x, a branch that is not the repository's default, and two results on it. With
	// unseen, GitHub refuses every compare read of that branch as unseen, which a writer's read
	// holds as the pair's unseen hold; without it, GitHub answers that read, and a writer's read
	// marks the pair seen (delivery's markSeen).
	onIntegrationBranch := func(t *testing.T, unseen bool) (*parityPair, string) {
		t.Helper()
		pair := newParityPair(t, appKey)
		issue := pair.issue(t, "ROJ", parityAfterMergeCheck)
		pair.each(func(stack *parityStack) {
			recordMerge(t, stack.rig.database, parityRepo, 53, issue, "integration/x")
			// The merge's own write stamps the yes it stores for its merge commit with the wall
			// clock (delivery.UpsertPullRequestHead), so each stack's stamp is its own. Nothing
			// reads a yes's checked_at, and the row's clock stands in for it, so row 20 can
			// compare the stacks' rows whole.
			if _, err := stack.rig.database.Pool.Exec(context.Background(),
				`update branch_commits set checked_at = $1 where on_branch`, pair.clock.now()); err != nil {
				t.Fatalf("set the merge commit's branch answer to the row's clock: %v", err)
			}
		})
		pair.github.answersCompare(func(base, head string) (int, string) {
			if base == "integration/x" && unseen {
				return http.StatusNotFound, ""
			}
			return http.StatusOK, "diverged"
		})
		pair.record(t, issue, "m1", strayCommit)
		pair.record(t, issue, "m1", integrationCommit)
		return pair, issue
	}

	t.Run("13 two results on one branch not the default, the first compare read refused as unseen", func(t *testing.T) {
		pair, issue := onIntegrationBranch(t, true)

		read := pair.read(t, issue, "the first read")
		// The newer result's compare read of integration/x is refused, and the older result is
		// answered unseen there with no call; each result's compare read of main says no. That
		// no hold stands afterwards is the read's own check of the server's in-process state.
		for index := range 2 {
			standsAt(t, read.writer, "m1", pair.writer.ids[index], fmt.Sprintf("the writer's result %d", index), model.CommitOtherCommit, false)
		}
		read.calls(t, parityCompare, 3)
	})

	// mergedIntoMain is the state of rows 14 and 15: an after_merge check of an issue merged into
	// main, the repository's default branch, which the walk names outright, and two results on
	// it: strayCommit, which main does not carry, and mergeCommit, which it does. With lapsed, the
	// default-branch cache entry is put by a writer's read before any result is recorded, on every
	// stack, and has lapsed by the read; without it, the process never read the repository.
	mergedIntoMain := func(t *testing.T, lapsed bool) (*parityPair, string) {
		t.Helper()
		pair := newParityPair(t, appKey)
		issue := pair.issue(t, "ROK", parityAfterMergeCheck)
		pair.each(func(stack *parityStack) { recordMerge(t, stack.rig.database, parityRepo, 54, issue, "main") })
		pair.github.answersCompare(func(base, head string) (int, string) {
			if base == "main" && head == mergeCommit {
				return http.StatusOK, "behind"
			}
			return http.StatusOK, "diverged"
		})
		if lapsed {
			put := pair.writersRead(t, issue, "the writer's read that puts the default-branch cache entry")
			put.calls(t, parityRepository, 1)
		}
		pair.record(t, issue, "m1", strayCommit)
		pair.record(t, issue, "m1", mergeCommit)
		if lapsed {
			pair.clock.advance(delivery.BranchCommitStaleAfter)
		}
		return pair, issue
	}
	// fromMain asserts the answer of a read of mergedIntoMain's results: the default branch read
	// once, a compare read of main for each result, mergeCommit on it and strayCommit off it.
	fromMain := func(t *testing.T, pair *parityPair, read parityRead) {
		t.Helper()
		standsAtBranch(t, read.writer, "m1", pair.writer.ids[1], "main")
		standsAt(t, read.writer, "m1", pair.writer.ids[0], "the writer's result off the trunk", model.CommitOtherCommit, false)
		read.calls(t, parityRepository, 1)
		read.calls(t, parityCompare, 2)
	}

	t.Run("14 an after_merge walk naming the default branch, its cache entry lapsed", func(t *testing.T) {
		pair, issue := mergedIntoMain(t, true)
		fromMain(t, pair, pair.read(t, issue, "the read past the cache entry"))
	})

	t.Run("15 an after_merge walk naming the default branch, no cache entry at all", func(t *testing.T) {
		pair, issue := mergedIntoMain(t, false)
		fromMain(t, pair, pair.read(t, issue, "the first read"))
	})

	// The first read of a check CI proves (ciCheckAtLearnedHead): that read's read-through learns
	// pull request 5, headed at k1's commit, and the same answer names that head as k1's basis, so
	// it derives c1 from the run at that head rather than answering that Dispatch holds no head.
	t.Run("16 a check CI proves: the first read derives it at the head that read's read-through learns", func(t *testing.T) {
		pair, issue, atHead := ciCheckAtLearnedHead(t, appKey, "ROL")

		first := pair.read(t, issue, "the first read")
		standsAtBasis(t, first.writer, "k1", pair.writer.ids[0], atHead)
		derived := checkOf(t, first.writer, "c1")
		assertCI(t, derived, ciCheckRun, model.CIStatePass, ciConclusionSuccess,
			ciVerdictLineOf("c1", ciCheckRun, model.CIStatePass, headCommit))
		assertDerived(t, derived, 11, ciCheckRun, ciConclusionSuccess, model.CIStatePass, headCommit)
		first.calls(t, parityPulls, 1)
		first.calls(t, parityCheckRuns, 1)
	})

	t.Run("17 a read-through whose commit-message read is refused for a rate limit after the heads were read", func(t *testing.T) {
		pair := newParityPair(t, appKey)
		issue := pair.issue(t, "ROM", parityBeforeMergeCheck)
		pair.record(t, issue, "k1", headCommit)
		// From here GitHub names two pull requests for the result's commit: 61, headed at it and
		// titled after the issue, which its title attributes; and 62, whose title, body and
		// branch name no issue, so its commit messages are read, and GitHub refuses that read
		// for a rate limit. The read-through answers 61's row with the refusal
		// (delivery.Attributor.ReadCommitPulls), and the writer's read stores that row.
		pair.github.answersPulls(func(string) string {
			return "[" + parityOpenPullTitled(61, headCommit, issue+" the work") + "," +
				parityOpenPullTitled(62, newHeadCommit, "Tidy the build") + "]"
		})
		pair.github.limitsCommitsOf(62)

		read := pair.read(t, issue, "the first read")
		read.calls(t, parityPulls, 1)
		read.calls(t, parityPullCommits, 1)
		// The writer's partial store: the row of 61 and nothing of 62. The read-only read answers
		// what the writer answers on it.
		want := map[string]bool{fmt.Sprintf("pull request head %s#61 at %s, base main, open, issue %s", parityRepo, headCommit, issue): true}
		if held := parityRows(t, pair.writer.rig.database); !maps.Equal(held, want) {
			t.Errorf("the writer's read left the rows %v, want %v", slices.Sorted(maps.Keys(held)), slices.Sorted(maps.Keys(want)))
		}
	})

	t.Run("18 a stored row of the pull request newer than the read-through's answer", func(t *testing.T) {
		pair := newParityPair(t, appKey)
		issue := pair.issue(t, "RON", parityBeforeMergeCheck)
		// Dispatch holds pull request 5 headed at newHeadCommit, learned an hour ago.
		stored := pair.clock.now().Add(-time.Hour)
		pair.each(func(stack *parityStack) {
			storeParityHead(t, stack.rig.database, 5, newHeadCommit, issue, stored)
		})
		pair.record(t, issue, "k1", headCommit)
		// From here GitHub names pull request 5 headed at the result's commit, with an updated_at
		// days older than the stored row's event_at.
		pair.github.answersPulls(func(string) string { return parityOpenPull(5, headCommit, issue) })

		read := pair.read(t, issue, "the first read")
		// The upsert's event-time guard refuses GitHub's answer (delivery.upsertHeadRow), so the
		// stored row answers: k1 compares with 5 at newHeadCommit, and its result is at another
		// commit.
		bases := checkOf(t, read.writer, "k1").ComparedWith
		if names := parityBases(checkOf(t, read.writer, "k1")); !slices.Equal(names, []string{fmt.Sprintf("pull request 5 at %.7s", newHeadCommit)}) ||
			bases[0].LearnedAt == nil || !bases[0].LearnedAt.At.Equal(stored) {
			t.Errorf("the writer's k1 compared with %+v, want pull request 5 at %.7s alone, learned at %s", bases, newHeadCommit, stored)
		}
		standsAt(t, read.writer, "k1", pair.writer.ids[0], "the writer's result", model.CommitOtherCommit, false)
		read.calls(t, parityPulls, 1)
	})

	t.Run("19 two read-throughs of one request name one pull request, the second with the older updated_at", func(t *testing.T) {
		pair := newParityPair(t, appKey)
		issue := pair.issue(t, "ROP", siblingChecks)
		pair.record(t, issue, "k1", headCommit)
		pair.record(t, issue, "k3", newHeadCommit)
		// From here GitHub names pull request 5 for each commit: headed at headCommit for
		// headCommit, and for newHeadCommit headed there with an older updated_at. k1's result is
		// read through first.
		pair.github.answersPulls(func(sha string) string {
			if sha == headCommit {
				return "[" + parityOpenPullUpdated(5, headCommit, issue+" the work", "2026-11-07T07:15:08Z") + "]"
			}
			return "[" + parityOpenPullUpdated(5, newHeadCommit, issue+" the work", "2026-11-05T07:15:08Z") + "]"
		})

		read := pair.read(t, issue, "the first read")
		// The guard refuses the older answer laid on after the newer one, so pull request 5 is
		// headed at headCommit for both checks.
		atHead := fmt.Sprintf("pull request 5 at %.7s", headCommit)
		for _, check := range []string{"k1", "k3"} {
			if bases := parityBases(checkOf(t, read.writer, check)); !slices.Equal(bases, []string{atHead}) {
				t.Errorf("the writer's %s compared with %v, want %s alone", check, bases, atHead)
			}
		}
		standsAtBasis(t, read.writer, "k1", pair.writer.ids[0], atHead)
		standsAt(t, read.writer, "k3", pair.writer.ids[1], "the writer's result at the older head", model.CommitOtherCommit, false)
		read.calls(t, parityPulls, 2)
	})

	t.Run("20 rows 2, 9, 12 and 13's states and row 13's branch answered, read by a read-only token and then by a writer", func(t *testing.T) {
		for _, state := range []struct {
			row  string
			made func(t *testing.T) (*parityPair, string)
		}{
			{"row 2's state", pushedPast},
			{"row 9's state", refusedUnseen},
			{"row 12's state", lapsedCache},
			{"row 13's state", func(t *testing.T) (*parityPair, string) { return onIntegrationBranch(t, true) }},
			{"row 13's branch with its compare read answered", func(t *testing.T) (*parityPair, string) { return onIntegrationBranch(t, false) }},
		} {
			pair, issue := state.made(t)
			// The read itself checks that the read-only read left the rows, the events and the
			// server's in-process state as it found them.
			read := pair.read(t, issue, "the read-only read of "+state.row)
			// Its oracle is a writer's read with no read-only read before it, on a stack of its own.
			oracle := pair.writer
			after := pair.writersRead(t, issue, "the writer's read after the read-only read of "+state.row)
			if after.writerBody != read.writerBody {
				t.Errorf("%s: the writer's read after the read-only read answered\n%s\nwant the answer of a writer's read with none before it\n%s",
					state.row, after.writerBody, read.writerBody)
			}
			if got, want := parityGitHubReads(after.writerCalls), parityGitHubReads(read.writerCalls); !maps.Equal(got, want) {
				t.Errorf("%s: the writer's read after the read-only read made the GitHub reads %v, want those of a writer's read with none before it, %v",
					state.row, got, want)
			}
			if got, want := parityStored(t, pair.readOnly.rig.database), parityStored(t, oracle.rig.database); got != want {
				t.Errorf("%s: the writer's read after the read-only read left\n%s\nwant what a writer's read with none before it left\n%s",
					state.row, got, want)
			}
		}
	})

	t.Run("21 a pull request stored at one result's commit, which the other result's read-through names at a third commit", func(t *testing.T) {
		pair := newParityPair(t, appKey)
		issue := pair.issue(t, "ROQ", siblingChecks)
		// Pull request 71 is another issue's, stored headed at headCommit, where k3's result is, with
		// an updated_at older than GitHub's answer below. It is no basis of the issue's checks, so
		// only a stored head can hold k3's result off GitHub.
		var other string
		pair.each(func(stack *parityStack) {
			response := dispatchRequest(t, stack.rig.handler, http.MethodPost, "/api/v1/issues",
				map[string]any{"project": "ROQ", "title": "Another issue's work", "spec": "A spec\n"}, "alice")
			if response.Code != http.StatusCreated {
				t.Fatalf("create the other issue on the stack of %s: status=%d body=%s", stack.who, response.Code, response.Body.String())
			}
			key := decodeBody[model.Issue](t, response).Key
			if other != "" && key != other {
				t.Fatalf("the stack of %s made the other issue %s, want %s", stack.who, key, other)
			}
			other = key
			storeParityHead(t, stack.rig.database, 71, headCommit, other, time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC))
		})
		pair.record(t, issue, "k1", behindHeadCommit)
		pair.record(t, issue, "k3", headCommit)
		// From here GitHub names pull request 71 for every commit, headed at newHeadCommit and
		// titled after the other issue.
		pair.github.answersPulls(func(string) string { return parityOpenPull(71, newHeadCommit, other) })

		read := pair.read(t, issue, "the first read")
		// k1's result is read through first. The writer stores 71 at newHeadCommit, which moves the
		// row off headCommit, so no stored pull request heads k3's commit and the writer reads it
		// through too. The read-only read must ask the same: a read of the stored heads at
		// headCommit that lays on only the answers headed there still finds 71 at it (plan D33).
		read.calls(t, parityPulls, 2)
		for index, check := range []string{"k1", "k3"} {
			standsAt(t, read.writer, check, pair.writer.ids[index], "the writer's result of "+check, model.CommitOtherCommit, false)
		}
		want := map[string]bool{fmt.Sprintf("pull request head %s#71 at %s, base main, open, issue %s", parityRepo, newHeadCommit, other): true}
		if held := parityRows(t, pair.writer.rig.database); !maps.Equal(held, want) {
			t.Errorf("the writer's read left the rows %v, want %v", slices.Sorted(maps.Keys(held)), slices.Sorted(maps.Keys(want)))
		}
	})
}

// TestAReadOnlyResultsReadOfALinkedRepositoryAnswersAsAWriter is the results row's state table
// (plan D26) for results at commits of a linked repository, one the issue links a pull request in
// and does not resolve to: GET /api/v1/issues/{key}/results read by a read-only token, each read
// compared with its oracle on a stack of its own as every read of
// TestAReadOnlyResultsReadAnswersAsAWriterInEveryState is (parityPair.read), and each row asserting
// what the writer answers, as main's linked tests read it
// (TestABeforeMergeResultAtALinkedCommitIsReadThroughThatRepository and
// TestABranchChecksResultAtALinkedPullRequestsMergeCommitIsComparedInThatRepository).
//
// In both rows Dispatch holds no row of the linked pull request, and GitHub names that pull
// request for no commit until the results are recorded, so no record's read-through stores it.
// The first read's read-through learns it: the writer's read stores the row, and the read-only
// read stores nothing and reads the linked heads and the linked merge with that answer laid on
// the stored rows for the rest of its request (plan D30). A read-only read that learned nothing
// would read the stored rows alone, as the writer's does, and would prove nothing about those two
// reads.
func TestAReadOnlyResultsReadOfALinkedRepositoryAnswersAsAWriter(t *testing.T) {
	t.Parallel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate fixture key: %v", err)
	}
	appKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))

	t.Run("1 a before_merge result at the head of a linked pull request no stored row holds", func(t *testing.T) {
		// toolsHead is a commit parityLinkedRepo holds and parityRepo does not.
		const toolsHead = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
		pair := newParityPair(t, appKey)
		pair.github.linksTo(parityLinked{held: []string{toolsHead}, states: map[int]string{7: "open"}})
		issue := pair.issue(t, "ROL", parityBeforeMergeCheck)
		learned := pair.clock.now().Add(-time.Hour)
		pair.each(func(stack *parityStack) {
			linkPullRequests(t, stack.rig.handler, issue, "https://github.com/"+parityLinkedRepo+"/pull/7")
			// Dispatch holds the issue's own pull request 5, and nothing of the linked one.
			storeParityHead(t, stack.rig.database, 5, headCommit, issue, learned)
		})
		pair.record(t, issue, "k1", toolsHead)
		pair.takenAsLinked(t, 0)
		// acme/tools#7's head comes to the result's commit, and no event tells Dispatch of it.
		pair.github.namesLinkedPulls(toolsHead, parityLinkedPull(7, toolsHead, "main", ""))

		first := pair.read(t, issue, "the first read")
		// The writer asks acme/tools for the commit's pull requests, the repository the record
		// stored with the result, and never parityRepo. It stores #7, attributed to no issue, and
		// the link makes it a head k1 compares with, first since it was learned last, and the
		// head the result stands at.
		standsAtBasis(t, first.writer, "k1", pair.writer.ids[0], fmt.Sprintf("pull request 7 at %.7s of %s", toolsHead, parityLinkedRepo))
		wantBases := []string{
			fmt.Sprintf("pull request 7 at %.7s of %s", toolsHead, parityLinkedRepo),
			fmt.Sprintf("pull request 5 at %.7s", headCommit),
		}
		if bases := parityBases(checkOf(t, first.writer, "k1")); !slices.Equal(bases, wantBases) {
			t.Errorf("the writer's k1 compared with %v, want %v", bases, wantBases)
		}
		first.calls(t, parityLinkedCall(parityPulls), 1)
		first.calls(t, parityPulls, 0)

		// The read-only read stored nothing and took no claim, so the writer's read at this
		// moment, on the same rows, asks acme/tools again and answers as the first read did.
		pair.clock.advance(time.Minute)
		second := pair.read(t, issue, "the second read")
		standsAtBasis(t, second.writer, "k1", pair.writer.ids[0], fmt.Sprintf("pull request 7 at %.7s of %s", toolsHead, parityLinkedRepo))
		if bases := parityBases(checkOf(t, second.writer, "k1")); !slices.Equal(bases, wantBases) {
			t.Errorf("the writer's k1 compared with %v at the second read, want %v", bases, wantBases)
		}
		second.calls(t, parityLinkedCall(parityPulls), 1)
		second.calls(t, parityPulls, 0)
	})

	t.Run("2 a branch check's result at a linked pull request's merge commit, compared in that repository", func(t *testing.T) {
		// acme/tools#10 merged into integration/x as toolsMerge, and toolsHead was its head.
		// parityRepo holds neither commit, and only integration/x carries toolsMerge. The branch
		// is not acme/tools' default one, so the branch the read of the merge names is what
		// places k4's result: a merge into main would name a branch the walk asks anyway.
		const (
			toolsHead  = "b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1"
			toolsMerge = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
		)
		pair := newParityPair(t, appKey)
		pair.github.linksTo(parityLinked{
			held:     []string{toolsHead, toolsMerge},
			states:   map[int]string{10: "merged"},
			branches: map[string][]string{"integration/x": {toolsMerge}},
		})
		issue := pair.issue(t, "ROM", parityChecksOfEachWhen)
		pair.each(func(stack *parityStack) {
			linkPullRequests(t, stack.rig.handler, issue, "https://github.com/"+parityLinkedRepo+"/pull/10")
		})
		pair.record(t, issue, "k1", toolsHead)
		pair.record(t, issue, "k4", toolsMerge)
		pair.record(t, issue, "k5", toolsMerge)
		for index := range 3 {
			pair.takenAsLinked(t, index)
		}
		// From here GitHub names acme/tools#10, merged, for its head, and no event tells
		// Dispatch of the merge.
		pair.github.namesLinkedPulls(toolsHead, parityLinkedPull(10, toolsHead, "integration/x", toolsMerge))

		read := pair.read(t, issue, "the first read")
		// The read-through of k1's result stores #10, merged, and the yes its merge commit is on
		// integration/x, before any check is answered. k4 and k5 name their own repository's
		// main as their basis, and their results are walked in acme/tools: k4's asks the branch
		// the linked pull request merged into first, which carries the commit by the stored yes,
		// and k5's asks acme/tools' main alone, which does not carry it, in one compare read.
		standsAtBasis(t, read.writer, "k1", pair.writer.ids[0], fmt.Sprintf("pull request 10 at %.7s of %s", toolsHead, parityLinkedRepo))
		for _, check := range []string{"k4", "k5"} {
			if bases := parityBases(checkOf(t, read.writer, check)); !slices.Equal(bases, []string{"branch main"}) {
				t.Errorf("the writer's %s compared with %v, want its own repository's main alone", check, bases)
			}
		}
		standsAtBasis(t, read.writer, "k4", pair.writer.ids[1], "branch integration/x of "+parityLinkedRepo)
		offTrunk := standsAt(t, read.writer, "k5", pair.writer.ids[2], "the writer's result of k5", model.CommitOtherCommit, false)
		if offTrunk.Repo != parityLinkedRepo {
			t.Errorf("the writer's result of k5 is at another commit of %q, want of %s", offTrunk.Repo, parityLinkedRepo)
		}
		read.calls(t, parityLinkedCall(parityPulls), 1)
		read.calls(t, parityLinkedCall(parityCompare), 1)
		read.calls(t, parityPulls, 0)
		read.calls(t, parityCompare, 0)
	})
}

// ciCheckAtLearnedHead is the state of D26's ci_check row and of
// TestACheckCIProvesIsAnsweredFromTheHeadItsFirstReadLearned: a fresh pair of stacks, an issue of
// project whose check c1 CI proves sits beside k1, and k1's result recorded at headCommit. From
// here GitHub names pull request 5 for that commit, headed at it and titled after the issue, and
// lists the run c1 names at that head, passed; no record's read-through stored the pull request.
// It answers the pair, the issue and the name of pull request 5's basis.
func ciCheckAtLearnedHead(t *testing.T, appKey, project string) (pair *parityPair, issue, atHead string) {
	t.Helper()
	pair = newParityPair(t, appKey)
	issue = pair.issue(t, project, ciCheckBlock("c1", ciCheckKept)+"\n"+parityBeforeMergeCheck)
	pair.record(t, issue, "k1", headCommit)
	pair.github.answersPulls(func(string) string { return parityOpenPull(5, headCommit, issue) })
	pair.github.listsRuns(headCommit, completedRun(11, ciCheckRun, "success"))
	return pair, issue, fmt.Sprintf("pull request 5 at %.7s", headCommit)
}

// TestACheckCIProvesIsAnsweredFromTheHeadItsFirstReadLearned holds the order of the results read
// (plan D31): the comparison reader learns the heads GitHub names before a check CI proves is
// derived, so a first read derives that check at the head its own read-through learned. A writer's
// first read and a read-only read's first read are each made alone, on a fresh stack of its own
// (newParityPair's two), of an issue whose newest pull request head no stored row holds. k1's
// result is recorded at that head itself, so the read-through finds the result's commit as the
// head and reads the bases again in the same read (comparisonReader.learn). Each read must derive
// c1 from the run at that head, and stand c1's derived result and k1's result at the one pull
// request headed there. Derived before the reader learns, each read answers c1 no_head.
func TestACheckCIProvesIsAnsweredFromTheHeadItsFirstReadLearned(t *testing.T) {
	t.Parallel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate fixture key: %v", err)
	}
	appKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	pair, issue, atHead := ciCheckAtLearnedHead(t, appKey, "RON")

	for _, stack := range []*parityStack{pair.writer, pair.readOnly} {
		t.Run(stack.who, func(t *testing.T) {
			awaitSettledDocuments(t, stack.rig.database)
			before := stack.calls.snapshot()
			response := stack.rig.serve(pair.caller.presenting(t, http.MethodGet, "/api/v1/issues/"+issue+"/results", stack.token, nil))
			if response.Code != http.StatusOK {
				t.Fatalf("%s: status=%d body=%s", stack.who, response.Code, response.Body.String())
			}
			read := decodeBody[issueResults](t, response)
			calls := stack.calls.since(before)
			t.Logf("%s answered %s, with the GitHub calls %v", stack.who, stack.summary(read), calls)

			ci := checkOf(t, read, "c1")
			assertCI(t, ci, ciCheckRun, model.CIStatePass, ciConclusionSuccess,
				ciVerdictLineOf("c1", ciCheckRun, model.CIStatePass, headCommit))
			derived := assertDerived(t, ci, 11, ciCheckRun, ciConclusionSuccess, model.CIStatePass, headCommit)
			if basis := derived.CommitStands.Basis; basis == nil || parityBasisName(basis) != atHead {
				t.Errorf("%s stands c1's derived result at %+v, want %s", stack.who, basis, atHead)
			}
			if bases := parityBases(checkOf(t, read, "k1")); !slices.Equal(bases, []string{atHead}) {
				t.Errorf("%s compares k1 with %v, want %s alone", stack.who, bases, atHead)
			}
			stands := standsAt(t, read, "k1", stack.ids[0], stack.who+": k1's result", model.CommitAtBasis, false)
			if stands.Basis == nil || parityBasisName(stands.Basis) != atHead {
				t.Errorf("%s stands k1's result at %+v, want %s", stack.who, stands.Basis, atHead)
			}
			// The read-through's one read of the commit's pull requests, and the one read of the
			// check runs at the head it learned.
			if calls[parityPulls] != 1 || calls[parityCheckRuns] != 1 {
				t.Errorf("%s made the GitHub calls %v, want one of %s and one of %s",
					stack.who, calls, parityPulls, parityCheckRuns)
			}
		})
	}
}

// parityRepo is the repository the stand-in GitHub holds and the test's issues resolve to; its
// default branch is main.
const parityRepo = "acme/widgets"

// parityLinkedRepo is the second repository the stand-in GitHub holds once a test gives it one
// (parityGitHub.linksTo): one a test's issue links a pull request in and does not resolve to. Its
// default branch is main too.
const parityLinkedRepo = "acme/tools"

// behindHeadCommit is a commit a push left behind: GitHub names a pull request for it whose head
// is another commit.
const behindHeadCommit = "7777777777777777777777777777777777777777"

// The table's Verification documents: one check of each `when`, and one document holding all
// three.
const (
	parityBeforeMergeCheck   = ":::check{#k1 surface=\"cli\" run=\"go test ./...\"}\nThe server records a result against a check.\n:::\n"
	parityAfterMergeCheck    = ":::check{#m1 when=\"after_merge\"}\nThe branch the work merged into carries the commit.\n:::\n"
	parityDefaultBranchCheck = ":::check{#d1 when=\"default_branch\"}\nThe trunk carries the commit.\n:::\n"
	parityChecksOfEachWhen   = parityBeforeMergeCheck + "\n" + afterMergeAndDefaultChecks
)

// The stand-in GitHub's routes, which name the calls a read makes. A call about parityLinkedRepo
// is counted apart, under parityLinkedCall's name.
const (
	parityInstallation = "GET /repos/{owner}/{repo}/installation"
	parityToken        = "POST /app/installations/{id}/access_tokens"
	parityRepository   = "GET /repos/{owner}/{repo}"
	parityCommit       = "GET /repos/{owner}/{repo}/commits/{sha}"
	parityPulls        = "GET /repos/{owner}/{repo}/commits/{sha}/pulls"
	parityPull         = "GET /repos/{owner}/{repo}/pulls/{number}"
	parityPullCommits  = "GET /repos/{owner}/{repo}/pulls/{number}/commits"
	parityCompare      = "GET /repos/{owner}/{repo}/compare/{basehead}"
	parityCheckRuns    = "GET /repos/{owner}/{repo}/commits/{sha}/check-runs"
)

// parityLinkedCall is the name a call of route about parityLinkedRepo is counted under.
func parityLinkedCall(route string) string {
	return route + " of " + parityLinkedRepo
}

// parityTestSession is the session both tokens act for.
const parityTestSession = "ses_parity"

// The two tokens of one identity every stack's stub broker knows (parityPair.stack): a machine
// login's token holding the person scope and not narrowed, which the writer's reads present, and
// the same login's token narrowed to reading (read-person), which the read-only reads present.
// Both act for the same session and the same person
// (TestANarrowedTokenActsAsItsIdentityAndOnlyReads).
const (
	parityWriterToken   = "a-token-that-writes"
	parityReadOnlyToken = "a-token-narrowed-to-reading"
)

// parityClock is the clock every stack's results reads take each moment from
// (server.resultsClock). It stands still until the test moves it.
type parityClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *parityClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *parityClock) advance(by time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(by)
}

// set puts the clock at at, so a step run again on another stack runs at the moment it first ran
// (parityPair.replayed).
func (c *parityClock) set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = at
}

// parityGitHub is the script one stack's stand-in GitHub answers from: whether the App can read
// parityRepo, the pull requests GitHub names for a commit of it (none until the test sets any), a
// compare read's HTTP status and status field (200 and diverged until the test sets them), the
// pull requests of it whose commits GitHub refuses to list for a rate limit (limitsCommitsOf),
// the check runs it lists at each of its commits (listsRuns), and parityLinkedRepo, which the
// stand-in holds only once the test gives it one (linksTo). A row changes it through
// parityScript, which makes each change on the script of every stack.
type parityGitHub struct {
	mu         sync.Mutex
	unreadable bool
	pulls      func(sha string) string
	compare    func(base, head string) (int, string)
	limited    map[int]bool
	runs       map[string][]map[string]any
	linked     *parityLinked
}

// parityLinked is parityLinkedRepo as the script answers it, as linkedGitHub answers each of its
// repositories: the commits it holds, which parityRepo does not; the state of each of its pull
// requests by number, open or merged, any other being GitHub's 404; the pull requests GitHub
// names for each commit, a JSON array each, none until the test names any (namesLinkedPulls);
// and the commits each of its branches carries (compare).
type parityLinked struct {
	held     []string
	states   map[int]string
	pulls    map[string]string
	branches map[string][]string
}

// compare is GitHub's answer to a compare read of parityLinkedRepo: 404 for a commit it does not
// hold, which is what GitHub answers a compare naming a commit it cannot find, `behind` for a
// commit base carries, and `diverged` for every other.
func (l *parityLinked) compare(base, head string) (int, string) {
	switch {
	case !slices.Contains(l.held, head):
		return http.StatusNotFound, ""
	case slices.Contains(l.branches[base], head):
		return http.StatusOK, "behind"
	}
	return http.StatusOK, "diverged"
}

func (g *parityGitHub) answersPulls(pulls func(sha string) string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pulls = pulls
}

func (g *parityGitHub) answersCompare(compare func(base, head string) (int, string)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.compare = compare
}

// limitsCommitsOf makes GitHub refuse every read of the commits of parityRepo's pull request
// number for a rate limit from now on, as it refuses a request once an installation's budget is
// spent: 403, no requests remaining and a Retry-After.
func (g *parityGitHub) limitsCommitsOf(number int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.limited == nil {
		g.limited = map[int]bool{}
	}
	g.limited[number] = true
}

// listsRuns makes GitHub list runs as the check runs of parityRepo's commit sha from now on, on
// one page.
func (g *parityGitHub) listsRuns(sha string, runs ...map[string]any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.runs == nil {
		g.runs = map[string][]map[string]any{}
	}
	g.runs[sha] = runs
}

// loseRepository makes every read of parityRepo, and of parityLinkedRepo where the stand-in holds
// it, answer 404 from now on, as GitHub answers a repository the App is no longer installed on.
// The client keeps the token it minted, so the repository's own read is what says so.
func (g *parityGitHub) loseRepository() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.unreadable = true
}

// linksTo makes the stand-in hold parityLinkedRepo as linked says, from now on.
func (g *parityGitHub) linksTo(linked parityLinked) {
	g.mu.Lock()
	defer g.mu.Unlock()
	linked.pulls = map[string]string{}
	g.linked = &linked
}

// namesLinkedPulls makes GitHub answer pulls, a JSON array, as the pull requests of sha in
// parityLinkedRepo from now on.
func (g *parityGitHub) namesLinkedPulls(sha, pulls string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.linked.pulls[sha] = pulls
}

// parityScript is a row's GitHub script. Each change of it is a step of the row, made on the
// script of every stack (parityPair.each), so each stack's stand-in answers as the row's script
// stood when the step it is running was made.
type parityScript struct {
	pair *parityPair
}

func (s parityScript) answersPulls(pulls func(sha string) string) {
	s.pair.each(func(stack *parityStack) { stack.github.answersPulls(pulls) })
}

func (s parityScript) answersCompare(compare func(base, head string) (int, string)) {
	s.pair.each(func(stack *parityStack) { stack.github.answersCompare(compare) })
}

func (s parityScript) limitsCommitsOf(number int) {
	s.pair.each(func(stack *parityStack) { stack.github.limitsCommitsOf(number) })
}

func (s parityScript) listsRuns(sha string, runs ...map[string]any) {
	s.pair.each(func(stack *parityStack) { stack.github.listsRuns(sha, runs...) })
}

func (s parityScript) loseRepository() {
	s.pair.each(func(stack *parityStack) { stack.github.loseRepository() })
}

func (s parityScript) linksTo(linked parityLinked) {
	s.pair.each(func(stack *parityStack) { stack.github.linksTo(linked) })
}

func (s parityScript) namesLinkedPulls(sha, pulls string) {
	s.pair.each(func(stack *parityStack) { stack.github.namesLinkedPulls(sha, pulls) })
}

// holds is whether repo holds sha: parityLinkedRepo the commits the script names it, and
// parityRepo every other commit.
func (g *parityGitHub) holds(repo, sha string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	linked := g.linked != nil && slices.Contains(g.linked.held, sha)
	if repo == parityLinkedRepo {
		return linked
	}
	return !linked
}

// parityCalls counts one stand-in's requests by route, a request about parityLinkedRepo under
// parityLinkedCall's name.
type parityCalls struct {
	mu     sync.Mutex
	counts map[string]int
}

func (c *parityCalls) add(route string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts[route]++
}

func (c *parityCalls) snapshot() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return maps.Clone(c.counts)
}

// since is the requests made after before was taken, by route, each route asked at least once.
func (c *parityCalls) since(before map[string]int) map[string]int {
	made := map[string]int{}
	for route, count := range c.snapshot() {
		if count > before[route] {
			made[route] = count - before[route]
		}
	}
	return made
}

// parityStandIn serves script to one stack and counts that stack's requests in calls.
func parityStandIn(t *testing.T, script *parityGitHub, calls *parityCalls) string {
	t.Helper()
	mux := http.NewServeMux()
	repoOf := func(r *http.Request) string { return r.PathValue("owner") + "/" + r.PathValue("repo") }
	route := func(pattern string, serve func(w http.ResponseWriter, r *http.Request)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if repoOf(r) == parityLinkedRepo {
				calls.add(parityLinkedCall(pattern))
			} else {
				calls.add(pattern)
			}
			serve(w, r)
		})
	}
	// unread answers 404 for a repository the App cannot read, and reports whether it did: every
	// repository but parityRepo and, once the stand-in holds it, parityLinkedRepo.
	unread := func(w http.ResponseWriter, r *http.Request) bool {
		script.mu.Lock()
		readable := !script.unreadable &&
			(repoOf(r) == parityRepo || (repoOf(r) == parityLinkedRepo && script.linked != nil))
		script.mu.Unlock()
		if readable {
			return false
		}
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
		return true
	}
	route(parityInstallation, func(w http.ResponseWriter, r *http.Request) {
		if !unread(w, r) {
			fmt.Fprint(w, `{"id":77,"app_slug":"dispatch-test","permissions":{"contents":"read","pull_requests":"read"}}`)
		}
	})
	route(parityToken, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_fake_77","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	route(parityRepository, func(w http.ResponseWriter, r *http.Request) {
		if !unread(w, r) {
			fmt.Fprintf(w, `{"full_name":%q,"default_branch":"main"}`, repoOf(r))
		}
	})
	route(parityCommit, func(w http.ResponseWriter, r *http.Request) {
		if unread(w, r) {
			return
		}
		sha := r.PathValue("sha")
		if !script.holds(repoOf(r), sha) {
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprintf(w, `{"message":"No commit found for SHA: %s"}`, sha)
			return
		}
		fmt.Fprintf(w, `{"sha":%q}`, sha)
	})
	route(parityPulls, func(w http.ResponseWriter, r *http.Request) {
		if unread(w, r) {
			return
		}
		sha := r.PathValue("sha")
		script.mu.Lock()
		pulls, named := script.pulls, ""
		if repoOf(r) == parityLinkedRepo {
			pulls, named = nil, script.linked.pulls[sha]
		}
		script.mu.Unlock()
		if pulls != nil {
			named = pulls(sha)
		}
		if named == "" {
			named = `[]`
		}
		fmt.Fprint(w, named)
	})
	route(parityPull, func(w http.ResponseWriter, r *http.Request) {
		if unread(w, r) {
			return
		}
		number, _ := strconv.Atoi(r.PathValue("number"))
		state := ""
		script.mu.Lock()
		if repoOf(r) == parityLinkedRepo {
			state = script.linked.states[number]
		}
		script.mu.Unlock()
		switch state {
		case "open":
			fmt.Fprintf(w, `{"number":%d,"state":"open","merged":false,"merged_at":null}`, number)
		case "merged":
			fmt.Fprintf(w, `{"number":%d,"state":"closed","merged":true,"merged_at":"2026-11-14T00:00:00Z"}`, number)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
		}
	})
	route(parityPullCommits, func(w http.ResponseWriter, r *http.Request) {
		number, _ := strconv.Atoi(r.PathValue("number"))
		script.mu.Lock()
		limited := repoOf(r) == parityRepo && script.limited[number]
		script.mu.Unlock()
		if limited {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"API rate limit exceeded for installation ID 77."}`)
			return
		}
		fmt.Fprint(w, `[]`)
	})
	route(parityCheckRuns, func(w http.ResponseWriter, r *http.Request) {
		if unread(w, r) {
			return
		}
		script.mu.Lock()
		listed := script.runs[r.PathValue("sha")]
		script.mu.Unlock()
		// Every run is on the first page, and parityLinkedRepo lists none.
		runs := []map[string]any{}
		if repoOf(r) == parityRepo && r.URL.Query().Get("page") == "1" && listed != nil {
			runs = listed
		}
		body, err := json.Marshal(map[string]any{"total_count": len(runs), "check_runs": runs})
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(body)
	})
	route(parityCompare, func(w http.ResponseWriter, r *http.Request) {
		if unread(w, r) {
			return
		}
		base, head, _ := strings.Cut(r.PathValue("basehead"), "...")
		script.mu.Lock()
		compare := script.compare
		if repoOf(r) == parityLinkedRepo {
			compare = script.linked.compare
		}
		script.mu.Unlock()
		status, comparison := http.StatusOK, "diverged"
		if compare != nil {
			status, comparison = compare(base, head)
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"message":%q}`, http.StatusText(status))
			return
		}
		fmt.Fprintf(w, `{"status":%q}`, comparison)
	})
	return githubtest.Server(t, githubtest.Spec{Handle: mux.ServeHTTP})
}

// parityOpenPull is GitHub's answer naming one open pull request of parityRepo whose head is
// head, titled after issueKey, whichever commit was asked about.
func parityOpenPull(number int, head, issueKey string) string {
	return "[" + parityOpenPullTitled(number, head, issueKey+" the work") + "]"
}

// parityOpenPullTitled is one open pull request of parityRepo as GitHub's answer naming a
// commit's pull requests lists it: headed at head, titled title, from the branch legion/x of
// parityRepo into main, its body empty.
func parityOpenPullTitled(number int, head, title string) string {
	return parityOpenPullUpdated(number, head, title, "2026-11-05T07:15:08Z")
}

// parityOpenPullUpdated is parityOpenPullTitled with updatedAt as GitHub's updated_at of it.
func parityOpenPullUpdated(number int, head, title, updatedAt string) string {
	return fmt.Sprintf(`{"number":%d,"title":%q,"body":"","html_url":"https://github.com/%s/pull/%d",
		"state":"open","merged":false,"updated_at":%q,
		"head":{"ref":"legion/x","sha":%q,"repo":{"full_name":%q}},"base":{"ref":"main"}}`,
		number, title, parityRepo, number, updatedAt, head, parityRepo)
}

// parityLinkedPull is GitHub's answer naming one pull request of parityLinkedRepo whose head is
// head, from a branch of that repository into base: open where mergeCommit is "", and else merged
// as mergeCommit. Its title names no issue, so the row a read-through writes of it is attributed
// to none, and only the issue's link makes it the issue's.
func parityLinkedPull(number int, head, base, mergeCommit string) string {
	state := `"state":"open","merged_at":null,"merge_commit_sha":null`
	if mergeCommit != "" {
		state = fmt.Sprintf(`"state":"closed","merged_at":"2026-11-14T00:00:00Z","merge_commit_sha":%q`, mergeCommit)
	}
	return fmt.Sprintf(`[{"number":%d,"title":"The second half of the work","body":"","html_url":"https://github.com/%s/pull/%d",
		%s,"updated_at":"2026-11-14T00:00:00Z",
		"head":{"ref":"work","sha":%q,"repo":{"full_name":%q}},"base":{"ref":%q}}]`,
		number, parityLinkedRepo, number, state, head, parityLinkedRepo, base)
}

// parityStack is one stack a row runs: its server, its stand-in's script and calls, the token its
// own reads present, and the id of each result recorded on it, in the order recorded.
type parityStack struct {
	who    string
	rig    *brokerTokenRig
	github *parityGitHub
	calls  *parityCalls
	token  string
	ids    []string
}

// parityPair is one row's stacks, on one clock and one GitHub script: the read-only stack, which
// the row's read-only reads read, and the writer's stack, the oracle a read-only read is compared
// with. Every step the row runs on the read-only stack is kept with the moment it ran at, so the
// oracle of each read is a stack built by running the same steps the read-only stack ran before
// that read (read, replayed).
type parityPair struct {
	clock  *parityClock
	github parityScript
	caller *brokerCaller
	appKey string
	// writing and narrowed are what every stack's stub broker says of parityWriterToken and of
	// parityReadOnlyToken.
	writing, narrowed ciphertoken.Introspection
	// writer is the writer's stack: until the row's first read a stack that ran every step the
	// read-only stack ran, and from then on the oracle of the last read.
	writer   *parityStack
	readOnly *parityStack
	// steps is every step the read-only stack ran, in order.
	steps []parityStep
	// spent is whether writer has made a writer's read the read-only stack did not make, so the
	// next read's oracle is built again.
	spent bool
}

// parityStep is one step of a row and the moment of the row's clock it ran at.
type parityStep struct {
	at  time.Time
	run func(stack *parityStack)
}

// newParityPair builds a row's two first stacks, on a clock of its own and a GitHub App whose key
// is appKey (stack).
func newParityPair(t *testing.T, appKey string) *parityPair {
	t.Helper()
	pair := &parityPair{
		clock:  &parityClock{at: time.Now().UTC().Truncate(time.Second)},
		caller: newBrokerCaller(t),
		appKey: appKey,
	}
	pair.github = parityScript{pair: pair}
	pair.narrowed = ciphertokentest.NarrowedAnswers(t, pair.caller.jkt, parityTestSession, time.Now().Add(time.Hour).Unix())["machine-login"]
	pair.writing = pair.narrowed
	pair.writing.ProjectScope, pair.writing.Scope = ciphertoken.ProjectScopePerson, ""
	pair.writer = pair.stack(t, "the writer's read", parityWriterToken)
	pair.readOnly = pair.stack(t, "the read-only read", parityReadOnlyToken)
	return pair
}

// stack is a fresh stack of the row: the package's test server with the stub broker, the pair's
// GitHub App and clock, and a stand-in GitHub of its own, whose stub broker knows both tokens.
// Its own reads present token.
func (p *parityPair) stack(t *testing.T, who, token string) *parityStack {
	t.Helper()
	stack := &parityStack{who: who, token: token, github: &parityGitHub{}, calls: &parityCalls{counts: map[string]int{}}}
	stack.rig = newBrokerTokenRigOn(t, testServerOptions{
		app:           &auth.AppConfig{ClientID: "Iv1.test", PEM: p.appKey},
		githubAPIBase: parityStandIn(t, stack.github, stack.calls),
	})
	stack.rig.served.resultsClock = p.clock.now
	stack.rig.broker.Says(parityWriterToken, p.writing)
	stack.rig.broker.Says(parityReadOnlyToken, p.narrowed)
	return stack
}

// each runs step on the writer's stack and then on the read-only stack, and keeps it among the
// row's steps. Once the writer's stack has made a read of its own it runs on the read-only stack
// alone, and the next read's oracle runs it in its place among the others (replayed).
func (p *parityPair) each(step func(stack *parityStack)) {
	p.steps = append(p.steps, parityStep{at: p.clock.now(), run: step})
	if !p.spent {
		step(p.writer)
	}
	step(p.readOnly)
}

// replayed is a fresh writer's stack that has run every step of the row, each at the moment it
// ran on the read-only stack: the state the read-only stack holds, its claims, settles, holds
// and cache entries among it, made by the same steps rather than set by hand.
func (p *parityPair) replayed(t *testing.T) *parityStack {
	t.Helper()
	oracle := p.stack(t, "the writer's read", parityWriterToken)
	now := p.clock.now()
	for _, step := range p.steps {
		p.clock.set(step.at)
		step.run(oracle)
	}
	p.clock.set(now)
	return oracle
}

// issue makes the same issue of project in parityRepo on every stack, with verification as its
// Verification document, and answers its key.
func (p *parityPair) issue(t *testing.T, project, verification string) string {
	t.Helper()
	keys := map[string]bool{}
	p.each(func(stack *parityStack) {
		issue := issueInRepository(t, stack.rig.handler, project, parityRepo, "Read as a writer reads")
		uploadDocument(t, stack.rig.handler, "/api/v1/issues/"+issue.Key, "verification.md", verification)
		keys[issue.Key] = true
	})
	if len(keys) != 1 {
		t.Fatalf("the two stacks made the issues %v, want one key", slices.Collect(maps.Keys(keys)))
	}
	return slices.Collect(maps.Keys(keys))[0]
}

// record records a pass of check at commit on every stack, as a person does.
func (p *parityPair) record(t *testing.T, issueKey, check, commit string) {
	t.Helper()
	p.each(func(stack *parityStack) {
		response := recordResult(t, stack.rig.handler, issueKey, resultBody(check, "pass", commit, nil))
		if response.Code != http.StatusCreated {
			t.Fatalf("record %s at %s on %s's stack: status=%d body=%s", check, commit, stack.who, response.Code, response.Body.String())
		}
		stack.ids = append(stack.ids, decodeBody[recordedResult](t, response).Result.ID)
	})
}

// takenAsLinked asserts that each stack stored the result it recorded at index with
// parityLinkedRepo as its commit's repository, as the record route stores a result at a commit
// only a repository the issue links a pull request in holds (refuseUnknownCommit).
func (p *parityPair) takenAsLinked(t *testing.T, index int) {
	t.Helper()
	p.each(func(stack *parityStack) {
		repo := storedCommitRepo(t, stack.rig.database, model.CheckResult{ID: stack.ids[index]})
		if repo == nil || *repo != parityLinkedRepo {
			t.Fatalf("result %d, on the stack of %s, stored commit_repo %v, want %s", index, stack.who, repo, parityLinkedRepo)
		}
	})
}

// parityRead is what one writer's read gave: its answer, decoded and as compared, and the GitHub
// calls it made, by route.
type parityRead struct {
	writer      issueResults
	writerBody  string
	writerCalls map[string]int
}

// calls asserts that the writer's read made count calls of route, the table's call count.
func (s parityRead) calls(t *testing.T, route string, count int) {
	t.Helper()
	if s.writerCalls[route] != count {
		t.Errorf("the writer's read made %d calls of %s, want %d (all its calls: %v)", s.writerCalls[route], route, count, s.writerCalls)
	}
}

// callsNone asserts that the writer's read made no GitHub call at all.
func (s parityRead) callsNone(t *testing.T) {
	t.Helper()
	if len(s.writerCalls) != 0 {
		t.Errorf("the writer's read made the GitHub calls %v, want none", s.writerCalls)
	}
}

// serve reads issueKey's results on the stack with token, and answers the body and the GitHub
// calls the read made, by route.
func (s *parityStack) serve(t *testing.T, caller *brokerCaller, issueKey, token, what string) (string, map[string]int) {
	t.Helper()
	before := s.calls.snapshot()
	response := s.rig.serve(caller.presenting(t, http.MethodGet, "/api/v1/issues/"+issueKey+"/results", token, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("%s, on the stack of %s: status=%d body=%s", what, s.who, response.Code, response.Body.String())
	}
	return response.Body.String(), s.calls.since(before)
}

// answered is a writer's read of this stack, its body raw and its calls as served.
func (s *parityStack) answered(t *testing.T, raw string, calls map[string]int, what string) parityRead {
	t.Helper()
	read := parityRead{writerBody: s.normalized(t, raw), writerCalls: calls}
	if err := json.Unmarshal([]byte(raw), &read.writer); err != nil {
		t.Fatalf("decode %s, on the stack of %s: %v", what, s.who, err)
	}
	return read
}

// writersRead is a writer's read of issueKey's results at the clock's moment, made as a step of
// the row on every stack (each): it is how a row makes a state a writer's read leaves, its claims,
// settles, holds and cache entries among it. It answers that read on the read-only stack.
func (p *parityPair) writersRead(t *testing.T, issueKey, what string) parityRead {
	t.Helper()
	var made parityRead
	p.each(func(stack *parityStack) {
		awaitSettledDocuments(t, stack.rig.database)
		raw, calls := stack.serve(t, p.caller, issueKey, parityWriterToken, what)
		if stack == p.readOnly {
			made = stack.answered(t, raw, calls, what)
		}
	})
	t.Logf("%s: answered %s, with the GitHub calls %v", what, p.readOnly.summary(made.writer), made.writerCalls)
	return made
}

// read is one read-only read of issueKey's results at the clock's moment, compared with its
// oracle: the writer's read of the same identity at that moment, on a stack built by running every
// step the read-only stack ran before it (replayed). It asserts what every row asserts: the two
// answers are one body, but for each stack's own ids and record times (normalized); the two reads
// made the same GitHub calls; and the read-only read left its stack's rows, events and in-process
// state as it found them (parityStateOf). The read-only read is a step of the row from then on,
// which a later read's oracle runs too. It answers the oracle's read.
func (p *parityPair) read(t *testing.T, issueKey, what string) parityRead {
	t.Helper()
	if p.spent {
		p.writer = p.replayed(t)
	}
	p.spent = true
	awaitSettledDocuments(t, p.writer.rig.database)
	awaitSettledDocuments(t, p.readOnly.rig.database)

	writerRaw, writerCalls := p.writer.serve(t, p.caller, issueKey, p.writer.token, what)
	found := parityStateOf(t, p.readOnly)
	readOnlyRaw, readOnlyCalls := p.readOnly.serve(t, p.caller, issueKey, p.readOnly.token, what)
	left := parityStateOf(t, p.readOnly)
	p.steps = append(p.steps, parityStep{at: p.clock.now(), run: func(stack *parityStack) {
		awaitSettledDocuments(t, stack.rig.database)
		stack.serve(t, p.caller, issueKey, parityReadOnlyToken, what+", made again")
	}})

	read := p.writer.answered(t, writerRaw, writerCalls, what)
	t.Logf("%s: the writer answered %s, with the GitHub calls %v", what, p.writer.summary(read.writer), writerCalls)
	if got := p.readOnly.normalized(t, readOnlyRaw); got != read.writerBody {
		var readOnly issueResults
		if err := json.Unmarshal([]byte(readOnlyRaw), &readOnly); err != nil {
			t.Fatalf("decode %s, the read-only read's: %v", what, err)
		}
		t.Errorf("%s: the read-only read answered %s, and the writer %s; in whole, the read-only read answered\n%s\nwant the writer's answer\n%s",
			what, p.readOnly.summary(readOnly), p.writer.summary(read.writer), got, read.writerBody)
	}
	if !maps.Equal(readOnlyCalls, writerCalls) {
		t.Errorf("%s: the read-only read made the GitHub calls %v, want the writer's %v", what, readOnlyCalls, writerCalls)
	}
	if left.stored != found.stored {
		t.Errorf("%s: the read-only read changed the rows its stack holds; it found\n%s\nand left\n%s", what, found.stored, left.stored)
	}
	if left.inProcess != found.inProcess {
		t.Errorf("%s: the read-only read changed its server's in-process state; it found\n%s\nand left\n%s", what, found.inProcess, left.inProcess)
	}
	return read
}

// parityGitHubReads is calls without the installation lookup and the token mint: the App client
// keeps the installation token a read minted for every later read of its process, so a read made
// after another finds the token already minted.
func parityGitHubReads(calls map[string]int) map[string]int {
	reads := maps.Clone(calls)
	delete(reads, parityInstallation)
	delete(reads, parityToken)
	return reads
}

// parityState is what a read could change on one stack, each part as text: the rows and events
// its database holds (parityStored), and its server's in-process state that a results read reads
// and a writer's read writes (parityInProcess).
type parityState struct {
	stored, inProcess string
}

func parityStateOf(t *testing.T, stack *parityStack) parityState {
	t.Helper()
	return parityState{stored: parityStored(t, stack.rig.database), inProcess: parityInProcess(stack)}
}

// parityInProcess is the in-process state of stack's server that a results read reads and a
// writer's read writes: the writer's claims and settles (server.commitPulls), and the
// default-branch cache, the unseen holds and the seen marks (server.branches). fmt prints every
// field of the two, unexported ones too, and each map in key order, so the text changes wherever
// a read changed any of them.
func parityInProcess(stack *parityStack) string {
	return fmt.Sprintf("%+v\n%+v", &stack.rig.served.commitPulls, &stack.rig.served.branches)
}

// parityStored is every pull_request_heads and branch_commits row database holds, whole, one row a
// line in one order, and how many events it holds.
func parityStored(t *testing.T, database *store.Store) string {
	t.Helper()
	var stored string
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce(string_agg(row, E'\n' order by row), '')
		from (
			select 'pull request head ' || to_jsonb(head)::text as row from pull_request_heads head
			union all
			select 'branch answer ' || to_jsonb(answer)::text from branch_commits answer
			union all
			select count(*)::text || ' events' from events
		) held
	`).Scan(&stored); err != nil {
		t.Fatalf("read the stored rows: %v", err)
	}
	return stored
}

// normalized is a results answer of this stack as JSON, with every member that names this
// stack's own rows rather than the answer replaced: each result's id by its place in the order
// the test recorded it, and the artifact ids, the record times and the event sequence numbers
// cleared. Every other member, the moment each head was learned at among them, is as answered.
func (s *parityStack) normalized(t *testing.T, body string) string {
	t.Helper()
	var answer any
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatalf("decode %s: %v", s.who, err)
	}
	var walk func(value any)
	walk = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			for member, inner := range value {
				switch member {
				case "verification_artifact_id", "artifact_id", "check_artifact_id", "created_at", "event_seq":
					value[member] = "cleared"
				case "id":
					if id, isString := inner.(string); isString {
						if index := slices.Index(s.ids, id); index >= 0 {
							value[member] = fmt.Sprintf("result %d", index)
						}
					}
				default:
					walk(inner)
				}
			}
		case []any:
			for _, inner := range value {
				walk(inner)
			}
		}
	}
	walk(answer)
	encoded, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("encode %s: %v", s.who, err)
	}
	return string(encoded)
}

// summary is a results answer of this stack in short, for the log: what each check compared
// with, each pull request with the time it was learned by, what CI answered of a check CI proves,
// and where each result's commit stands, each result named by its place in the order the test
// recorded it.
func (s *parityStack) summary(read issueResults) string {
	var checks []string
	for _, check := range read.Checks {
		bases := make([]string, 0, len(check.ComparedWith))
		for _, basis := range check.ComparedWith {
			name := parityBasisName(&basis)
			if basis.LearnedAt != nil {
				name += fmt.Sprintf(" learned by %s %s", basis.LearnedAt.Kind, basis.LearnedAt.At.Format(time.TimeOnly))
			}
			bases = append(bases, name)
		}
		compared := "[" + strings.Join(bases, ", ") + "]"
		if check.ComparedNothing != "" {
			compared = "nothing (" + check.ComparedNothing + ")"
		}
		if check.CI != nil {
			compared += fmt.Sprintf(", CI %s (%s)", check.CI.State, check.CI.Case)
		}
		results := make([]string, 0, len(check.Results))
		for _, result := range check.Results {
			stands := "no answer"
			if result.CommitStands != nil {
				stands = result.CommitStands.Answer
				if result.CommitStands.Basis != nil {
					stands += " " + parityBasisName(result.CommitStands.Basis)
				}
				if result.CommitStands.Reason != "" {
					stands += " (" + result.CommitStands.Reason + ")"
				}
			}
			results = append(results, fmt.Sprintf("result %d at %.7s %s", slices.Index(s.ids, result.ID), result.CommitSHA, stands))
		}
		checks = append(checks, fmt.Sprintf("%s compared with %s: %s", check.BlockID, compared, strings.Join(results, "; ")))
	}
	return strings.Join(checks, " | ")
}

// parityBases is what check compared with, each basis in short (parityBasisName).
func parityBases(check model.CheckBlock) []string {
	bases := make([]string, 0, len(check.ComparedWith))
	for _, basis := range check.ComparedWith {
		bases = append(bases, parityBasisName(&basis))
	}
	return bases
}

// parityBasisName is a basis in short: a pull request with its head, a branch, or a commit, and
// the linked repository it is of where it names one.
func parityBasisName(basis *model.CheckBasis) string {
	var name string
	switch {
	case basis.PullRequest != nil:
		name = fmt.Sprintf("pull request %d at %.7s", *basis.PullRequest, basis.Commit)
	case basis.Branch != "":
		name = "branch " + basis.Branch
	default:
		name = fmt.Sprintf("commit %.7s", basis.Commit)
	}
	if basis.Repo != "" {
		name += " of " + basis.Repo
	}
	return name
}

// standsAtBasis asserts that the writer's result stands at the basis parityBasisName names want.
func standsAtBasis(t *testing.T, read issueResults, check, resultID, want string) {
	t.Helper()
	stands := standsAt(t, read, check, resultID, "the writer's result of "+check, model.CommitAtBasis, false)
	if stands.Basis == nil || parityBasisName(stands.Basis) != want {
		t.Errorf("the writer's result of %s stands at %+v, want %s", check, stands.Basis, want)
	}
}

// standsAtBranch asserts that the writer's result stands at the branch basis.
func standsAtBranch(t *testing.T, read issueResults, check, resultID, branch string) {
	t.Helper()
	stands := standsAt(t, read, check, resultID, "the writer's result on "+branch, model.CommitAtBasis, false)
	if stands.Basis == nil || stands.Basis.Branch != branch {
		t.Errorf("the writer's result stands at %+v, want the branch %s", stands.Basis, branch)
	}
}

// storeParityHead stores the open pull request number of parityRepo with its head at sha,
// attributed to issueKey and learned at learned, as the intake's write of a `pull_request`
// envelope would leave it.
func storeParityHead(t *testing.T, database *store.Store, number int, sha, issueKey string, learned time.Time) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into pull_request_heads (repo, number, head_sha, head_ref, base_ref, state, issue_key, event_at, checked_at)
		values ($1, $2, $3, 'legion/work', 'main', 'open', $4, $5, $5)
	`, parityRepo, number, sha, issueKey, learned); err != nil {
		t.Fatalf("store the head of %s#%d: %v", parityRepo, number, err)
	}
}

// parityRows is every pull_request_heads and branch_commits row database holds, each named by its
// key and what it says, without the moments it was written at.
func parityRows(t *testing.T, database *store.Store) map[string]bool {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select format('pull request head %s#%s at %s, base %s, %s, issue %s', repo, number, head_sha, base_ref, state, coalesce(issue_key, 'none'))
		from pull_request_heads
		union all
		select format('branch answer %s %s %s on the branch: %s', repo, branch, sha, on_branch)
		from branch_commits
	`)
	if err != nil {
		t.Fatalf("read the stored rows: %v", err)
	}
	defer rows.Close()
	held := map[string]bool{}
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatalf("read a stored row: %v", err)
		}
		held[row] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the stored rows: %v", err)
	}
	return held
}
