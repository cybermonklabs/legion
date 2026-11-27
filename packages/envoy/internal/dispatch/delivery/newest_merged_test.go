package delivery

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// mergedPullEnvelope is the `pull_request` envelope of acme/widgets#number that a listener carrying
// head_repo and merged_at publishes for a merged pull request of issue, its head branch naming the
// issue, merged into base at mergedAt and last updated at updatedAt. A mergedAt of "" is an envelope
// that carries no merge time, as a listener older than that field publishes.
func mergedPullEnvelope(issue string, number int, action, base, mergedAt, updatedAt string) map[string]string {
	payload := map[string]string{
		"kind": "pr", "action": action, "merged": "true", "repo": "acme/widgets", "number": fmt.Sprint(number),
		"title": issue + " the work", "body": "", "head_ref": "legion/" + issue, "head_repo": "acme/widgets",
		"head_sha": fmt.Sprintf("%02d%038d", number, 0), "base_ref": base,
		"url":        fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number),
		"updated_at": updatedAt,
	}
	if mergedAt != "" {
		payload["merged_at"] = mergedAt
	}
	return payload
}

// The newest merged pull request of an issue is the one GitHub merged last, not the one of the
// highest number and not the one whose last event is newest. #9 merged into integration/nine first,
// then #8, opened before it, merged into integration/eight; then #9 was edited, which moves its
// updated_at (event_at) past #8's merge. NewestPullRequestHead, the head a check CI proves is read
// at, answers #8, where the highest number answered #9; BaseBranchMergedFor, the branch an
// `after_merge` result is measured against, answers integration/eight, where the newest event
// answered #9's integration/nine. The control: an envelope of #8 from a listener that carries no
// merge time, edited later still, leaves #8's merge time as it was, so the answers do not move.
func TestTheNewestMergedPullRequestIsTheOneThatMergedLast(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	issue := seedHeadIssue(t, ctx, pool, 130)
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/pulls/{number}/commits", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("#%s's commit messages were paged; its head branch attributes it", r.PathValue("number"))
		mustEncode(t, w, []any{})
	})
	intake := newIntake(tuningFor(t), nil, pool, nil, fake.newTestClient(), nil, nil, unrenamed())
	record := func(what string, envelope map[string]string) {
		t.Helper()
		number := 0
		fmt.Sscan(envelope["number"], &number)
		if _, err := intake.recordPullRequestHead(ctx, envelope, number); err != nil {
			t.Fatalf("record %s: %v", what, err)
		}
	}
	answers := func(when string) {
		t.Helper()
		head, found, err := NewestPullRequestHead(ctx, pool, issue, HeadAnswers{})
		if err != nil || !found || head.Number != 8 {
			t.Errorf("%s: NewestPullRequestHead(%s) = %+v, %v, %v; want #8, which merged after #9", when, issue, head, found, err)
		}
		branch, err := unrenamed().BaseBranchMergedFor(ctx, pool, issue, "acme/widgets", HeadAnswers{})
		if err != nil || branch != "integration/eight" {
			t.Errorf("%s: BaseBranchMergedFor(%s) = %q, %v; want integration/eight, the branch #8 merged into after #9 merged",
				when, issue, branch, err)
		}
	}

	record("#9's merge", mergedPullEnvelope(issue, 9, "closed", "integration/nine", "2026-03-01T00:00:00Z", "2026-03-01T00:00:00Z"))
	record("#8's merge", mergedPullEnvelope(issue, 8, "closed", "integration/eight", "2026-03-02T00:00:00Z", "2026-03-02T00:00:00Z"))
	record("#9's edit after #8 merged", mergedPullEnvelope(issue, 9, "edited", "integration/nine", "2026-03-01T00:00:00Z", "2026-03-03T00:00:00Z"))
	answers("after #9's edit")

	record("#8's edit by a listener that carries no merge time",
		mergedPullEnvelope(issue, 8, "edited", "integration/eight", "", "2026-03-04T00:00:00Z"))
	var mergedAt *time.Time
	if err := pool.QueryRow(ctx, `select merged_at from pull_request_heads where repo = 'acme/widgets' and number = 8`).Scan(&mergedAt); err != nil {
		t.Fatalf("read #8's merge time: %v", err)
	}
	if want := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC); mergedAt == nil || !mergedAt.Equal(want) {
		t.Fatalf("#8's merged_at after an envelope that carries none = %v, want %s kept", mergedAt, want)
	}
	answers("after #8's edit with no merge time")
}
