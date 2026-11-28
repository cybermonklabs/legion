package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func mergeRefreshParityPair(t *testing.T) *parityPair {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate fixture key: %v", err)
	}
	appKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	return newParityPair(t, appKey)
}

func (s parityScript) answersDetail(number int, detail string) {
	s.pair.each(func(stack *parityStack) {
		stack.github.mu.Lock()
		defer stack.github.mu.Unlock()
		if stack.github.details == nil {
			stack.github.details = map[int]string{}
		}
		stack.github.details[number] = detail
	})
}

// The first head read learns the merge. The later, unreadable detail read must stamp that
// request's current head, not replay the old physical row over it.
func TestAnUnreadableMergeRefreshKeepsTheRequestsLearnedHead(t *testing.T) {
	t.Parallel()
	pair := mergeRefreshParityPair(t)
	issue := pair.issue(t, "RRA", parityAfterMergeCheck+"\n"+parityBeforeMergeCheck)
	learned := pair.clock.now().Add(-time.Hour)
	pair.each(func(stack *parityStack) {
		storeParityHead(t, stack.rig.database, 91, newHeadCommit, issue, learned)
		if _, err := stack.rig.database.Pool.Exec(context.Background(), `
			update pull_request_heads set event_at = null where repo = $1 and number = 91
		`, parityRepo); err != nil {
			t.Fatalf("make the old open row eventless: %v", err)
		}
	})
	pair.record(t, issue, "k1", headCommit)
	pair.github.answersPulls(func(sha string) string {
		if sha != headCommit {
			return `[]`
		}
		return fmt.Sprintf(`[{"number":91,"title":%q,"body":"","html_url":"https://github.com/%s/pull/91",
			"state":"closed","merged":true,"merged_at":null,"merge_commit_sha":null,
			"head":{"ref":"legion/work","sha":%q,"repo":{"full_name":%q}},"base":{"ref":"integration/x"}}]`,
			issue+" the work", parityRepo, headCommit, parityRepo)
	})
	// No detail is scripted, so the real refresh route answers 404 after that successful read-through.
	read := pair.read(t, issue, "learn the merge, then refresh its unreadable detail")
	read.calls(t, parityPull, 1)
	before := checkOf(t, read.writer, "k1")
	if len(before.ComparedWith) != 1 || before.ComparedWith[0].Commit != headCommit || !before.ComparedWith[0].Merged {
		t.Fatalf("the writer's current head is %+v, want the newly learned merged head %s", before.ComparedWith, headCommit)
	}
}

// A rule refresh can move the newest merge to another issue. The subject snapshot and the CI
// derivation must both use the remaining attributed head, including in a read-only request.
func TestMergeRefreshReattributionChangesBothSubjectsAndCI(t *testing.T) {
	t.Parallel()
	pair := mergeRefreshParityPair(t)
	issue := pair.issue(t, "RRB", ciCheckBlock("c1", ciCheckKept))
	other := pair.issue(t, "RRC", parityBeforeMergeCheck)
	learned := pair.clock.now().Add(-2 * time.Hour)
	pair.each(func(stack *parityStack) {
		for number, sha := range map[int]string{5: headCommit, 6: newHeadCommit} {
			storeParityHead(t, stack.rig.database, number, sha, issue, learned)
		}
		if _, err := stack.rig.database.Pool.Exec(context.Background(), `
			update pull_request_heads set state = 'merged', merged_at = null, event_at = null,
				attribution_rule = 0 where repo = $1 and number in (5, 6)
		`, parityRepo); err != nil {
			t.Fatalf("store two legacy merged rows needing a rule refresh: %v", err)
		}
	})
	detail := func(number int, sha, titledIssue, merge string, at time.Time) string {
		return fmt.Sprintf(`{"number":%d,"title":%q,"body":"","html_url":"https://github.com/%s/pull/%d",
			"state":"closed","merged":true,"merged_at":%q,"merge_commit_sha":%q,
			"head":{"ref":"legion/work","sha":%q,"repo":{"full_name":%q}},"base":{"ref":"main"}}`,
			number, titledIssue+" the work", parityRepo, number, at.Format(time.RFC3339), merge, sha, parityRepo)
	}
	pair.github.answersDetail(5, detail(5, headCommit, issue, mergeCommit, learned.Add(time.Minute)))
	pair.github.answersDetail(6, detail(6, newHeadCommit, other, baseMergeCommit, learned.Add(time.Hour)))
	pair.github.listsRuns(headCommit, completedRun(11, ciCheckRun, "success"))
	read := pair.read(t, issue, "refresh both legacy merges before choosing a CI subject")
	read.calls(t, parityPull, 2)
	check := checkOf(t, read.writer, "c1")
	if len(check.ComparedWith) != 1 || check.ComparedWith[0].PullRequest == nil || *check.ComparedWith[0].PullRequest != 5 {
		t.Fatalf("comparison retained a reattributed merge: %+v; want only pull request 5", check.ComparedWith)
	}
	if len(check.PassStands.Subjects) != 1 || check.PassStands.Subjects[0].Basis.PullRequest == nil ||
		*check.PassStands.Subjects[0].Basis.PullRequest != 5 || check.PassStands.Answer != model.PassStandsYes {
		t.Fatalf("subjects and successful CI disagree after reattribution: %+v; want a standing pass at pull request 5", check.PassStands)
	}
}
