//go:build e2e

// The GitHub envelopes the whole-daemon live proofs publish alike, merge_wait_readable_live_test.go's
// and review_range_live_test.go's among them: the review App's review of a pull request, the
// implementer's push and GitHub's events of a pull request, its synchronize among them, each
// published on the ENVOY_NOTIFICATIONS stream as Envoy relays the webhook (liveProof's publish)
// and, where the proof reads what the daemon made of it next, waited on until the daemon's intake
// has applied it (waitProcessed). It carries `//go:build e2e` because no untagged test calls it.
package daemon

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// publishReviewAs publishes a review of the pull request numbered number at head, as Envoy relays
// a GitHub pull-request review (intake's decodeReview reads it). It is the review App's own, its
// body ending with footer, the Legion footer the caller writes: legionFooter naming a session and
// head, which is what lets it decide the round at all (workflow's refusesRound) when the head is
// the pull request's current head, or legionEntryFooter naming the entry of its round too. The
// session is held to the one the claims record holds for the claim the daemon starts the reviewer
// under, in the first round as in every later one (workflow's byReviewer): only a review naming
// that session decides the round, and while that claim records no session the review waits and
// decides nothing (workflow's RefusedWhileNoSession). Of the reviewer's own reviews, only one whose
// footer names the entry the reviewer's phases row stands at moves the commit the reviewer read
// (workflow's review); no time is compared. seq numbers the envelope and reviewID the review: each
// review a run publishes carries an id above the one before it, and its submitted_at is the moment
// of the publish to the second, as GitHub's is, so none is ordered behind a review already applied
// (record.ReviewOrder, which orders two reviews of one second by their ids).
func (p *liveProof) publishReviewAs(number int, state, head, footer string, seq, reviewID int64) {
	p.t.Helper()
	payload, err := json.Marshal(map[string]string{
		"kind": "review", "repo": liveRepo, "number": strconv.Itoa(number), "action": "submitted",
		"state": state, "commit_id": head, "head_sha": head, "author": reviewAppLogin,
		"body":           "the reviewer's " + state + " of " + head + footer,
		"body_truncated": "false", "review_id": strconv.FormatInt(1000+reviewID, 10),
		"submitted_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		p.t.Fatalf("encode the review payload: %v", err)
	}
	subject := fmt.Sprintf("notifications.github.%s.pr.%d.review", p.subjectRepo(), number)
	p.waitProcessed(p.publish(subject, "github", fmt.Sprintf("review-%d-%d", number, seq), string(payload)))
}

// publishPushOf publishes the implementer's push of to onto from on issue's branch, changing code,
// as Envoy relays a GitHub push (intake's decodePush reads it). It is published before the
// synchronize that moves the head, which is the order that leaves the move judged by the chain of
// its pushes (classify.AdvancePullRequestHead).
func (p *liveProof) publishPushOf(issue, from, to string) {
	p.t.Helper()
	payload, err := json.Marshal(map[string]string{
		"kind": "push", "repo": liveRepo, "ref": "refs/heads/" + claim.IssueBranch(issue), "before": from, "after": to,
		"changed_paths": "src/widget.go", "changed_paths_truncated": "false", "forced": "false",
		"pusher": "legion-implementer[bot]", "commit_count": "1", "head_subject": "earn what the base requires",
	})
	if err != nil {
		p.t.Fatalf("encode the push payload: %v", err)
	}
	subject := fmt.Sprintf("notifications.github.%s.push.branch.%s", p.subjectRepo(), claim.IssueBranch(issue))
	p.waitProcessed(p.publish(subject, "github", fmt.Sprintf("push-%s-%s", issue, to[:8]), string(payload)))
}

// publishSynchronizeOf publishes GitHub's synchronize of the pull request numbered number, issue's,
// on base and titled title, to head at updated, as Envoy relays a GitHub pull-request event
// (intake's decodePullRequest reads it). updated is GitHub's own clock, which the lifecycle fence
// orders the observation by and the record keeps as HeadUpdatedAt.
func (p *liveProof) publishSynchronizeOf(number int, issue, head, base, title string, updated time.Time) {
	p.t.Helper()
	p.waitProcessed(p.publishPullRequest(livePullRequestEvent{action: "synchronize", number: number, issue: issue, head: head,
		base: base, title: title, updated: updated}, fmt.Sprintf("pr-%d-synchronize-%s", number, head[:8])))
}

// livePullRequestEvent is one GitHub event of a pull request as a live proof publishes it: its
// action, the pull request numbered number from issue's branch into base, titled title, at head,
// GitHub's clock of it (updated), and, for a "closed" event that merged it, the merge commit
// (merge, "" for any other event).
type livePullRequestEvent struct {
	action                   string
	number                   int
	issue, head, base, title string
	updated                  time.Time
	merge                    string
}

// publishPullRequest publishes event under eventID, as Envoy relays a GitHub pull-request event
// (intake's decodePullRequest reads it), and answers the published message for a caller that
// waits for the daemon to apply it (waitProcessed).
func (p *liveProof) publishPullRequest(event livePullRequestEvent, eventID string) string {
	p.t.Helper()
	fields := map[string]string{
		"action": event.action, "kind": "pr", "repo": liveRepo, "number": strconv.Itoa(event.number),
		"head_sha": event.head, "base_ref": event.base, "head_ref": claim.IssueBranch(event.issue),
		"title":      event.title,
		"url":        "https://github.com/" + liveRepo + "/pull/" + strconv.Itoa(event.number),
		"updated_at": event.updated.Format(time.RFC3339),
	}
	if event.merge != "" {
		fields["merged"], fields["merge_commit_sha"] = "true", event.merge
	}
	payload, err := json.Marshal(fields)
	if err != nil {
		p.t.Fatalf("encode the %s payload of pull request #%d: %v", event.action, event.number, err)
	}
	subject := fmt.Sprintf("notifications.github.%s.pr.%d", p.subjectRepo(), event.number)
	return p.publish(subject, "github", eventID, string(payload))
}
