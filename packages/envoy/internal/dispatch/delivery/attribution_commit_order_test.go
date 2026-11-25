package delivery

// A production case (event 162 of a closed issue, received by #4209 on 2026-11-25): a pull request
// merged for an issue, and the issue closed. Nine minutes later another pull request opened whose
// branch, title and body name no issue. Its commit list carried two `Dispatch:` trailers: an older
// commit's, naming the closed issue, and its head commit's, naming another issue. The older
// trailer won, so the later pull request was credited to the closed issue from its commit messages,
// and the closed issue's check that CI proves read the later pull request's head, where its own
// merged head's check runs had passed. How the older commit entered the branch is not known; its
// trailer is history the branch carries, and the head's trailer is the branch's own word now.
//
// Here the closed issue is ACME-6201, whose pull request #893 merged; ACME-6202 is the issue #901's
// head commit names. Each event goes through Intake.deliver, as production's did.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
)

const (
	ev162Repo   = "acme/relays"
	ev162Closed = "ACME-6201"
	ev162Newer  = "ACME-6202"
	// ev162Merged is the pull request of the closed issue, and ev162Later the one opened after it.
	ev162Merged = 893
	ev162Later  = 901
	ev162Head1  = "8ef7e04400000000000000000000000000000893"
	ev162Merge  = "8ef7e04400000000000000000000000000008930"
	ev162Head2  = "29d7491c00000000000000000000000000000901"
)

// ev162Pull is a pull request of ev162Repo as GitHub answers it.
func ev162Pull(number int, state string, merged bool, head, branch, title string, at time.Time) map[string]any {
	pull := map[string]any{
		"number": number, "title": title, "body": "", "state": state, "merged": merged,
		"html_url":   fmt.Sprintf("https://github.com/%s/pull/%d", ev162Repo, number),
		"updated_at": at.UTC().Format(time.RFC3339),
		"head":       map[string]any{"ref": branch, "sha": head, "repo": map[string]any{"full_name": ev162Repo}},
		"base":       map[string]any{"ref": "main"},
	}
	if merged {
		pull["merge_commit_sha"] = ev162Merge
		pull["merged_at"] = at.UTC().Format(time.RFC3339)
	}
	return pull
}

// The later pull request is credited to the issue its head commit's trailer names, from its commit
// messages, and not to the closed issue an older commit's trailer names; the closed issue keeps its
// merged pull request as its only pull request, and the newest head of the issue, the head the
// check CI proves is read at, is that merged head (NewestPullRequestHead). The issue the head's
// trailer names lists the later pull request. At a6003a51b0 the older trailer won: #901 was
// credited to ACME-6201 from its commit messages, and ACME-6201's newest head was #901's.
func TestALaterPullRequestIsCreditedToItsHeadCommitsTrailerAndNotToAClosedIssueAnOlderCommitNames(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	seedProjectRepository(t, ctx, pool, "ACME", "Acme", ev162Repo)
	for _, key := range []string{ev162Closed, ev162Newer} {
		var number int
		if _, err := fmt.Sscanf(key, "ACME-%d", &number); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			insert into issues (key, project_key, number, title, status, created_by, rank, repo)
			values ($1, 'ACME', $2, $1, 'in_progress', '{"kind":"system","id":"test"}'::jsonb, $1, $3)
		`, key, number, ev162Repo); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
	fake := newFakeGitHub(t)
	github := serveGitHubPulls(t, fake, ev162Repo)
	intake := newIntake(tuningFor(t), nil, pool, nil, fake.newTestClient(), nil, newSettingsCache(pool, nil, tuningFor(t).log), unrenamed())
	deliver := func(number int, action string, pull map[string]any, messages []string) {
		t.Helper()
		github.mu.Lock()
		github.pulls[number] = pull
		if github.messages == nil {
			github.messages = map[int][]string{}
		}
		github.messages[number] = messages
		github.mu.Unlock()
		head := pull["head"].(map[string]any)
		envelope := map[string]string{
			"kind": "pr", "action": action, "repo": ev162Repo, "number": strconv.Itoa(number),
			"title": pull["title"].(string), "body": "", "head_ref": head["ref"].(string), "head_repo": ev162Repo,
			"head_sha": head["sha"].(string), "base_ref": "main", "merged": strconv.FormatBool(pull["merged"].(bool)),
			"url": pull["html_url"].(string), "updated_at": pull["updated_at"].(string),
		}
		if merged, _ := pull["merge_commit_sha"].(string); merged != "" {
			envelope["merge_commit_sha"] = merged
		}
		payload, err := json.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}
		intake.deliver(deliveryWithNoNATS(ctx), intakeMessage{
			msg:      &natsgo.Msg{Subject: "notifications.github.acme.relays.pr." + strconv.Itoa(number)},
			envelope: &contracts.Envelope{Payload: string(payload)},
		})
	}

	opened := time.Date(2026, 11, 25, 4, 40, 0, 0, time.UTC)
	mergedAt := time.Date(2026, 11, 25, 5, 12, 4, 0, time.UTC)
	laterAt := time.Date(2026, 11, 25, 5, 21, 2, 0, time.UTC)
	const mergedBranch, mergedTitle = "legion/ACME-6201", "Move the fleet to the new release"
	mergedMessages := []string{"Move the fleet to the new release\n\nDispatch: " + ev162Closed}
	deliver(ev162Merged, "opened", ev162Pull(ev162Merged, "open", false, ev162Head1, mergedBranch, mergedTitle, opened), mergedMessages)
	deliver(ev162Merged, "closed", ev162Pull(ev162Merged, "closed", true, ev162Head1, mergedBranch, mergedTitle, mergedAt), mergedMessages)
	if _, err := pool.Exec(ctx, `update issues set status = 'done', closed_at = $2 where key = $1`, ev162Closed, mergedAt); err != nil {
		t.Fatalf("close %s: %v", ev162Closed, err)
	}
	// The older commit, whose trailer names the closed issue, comes first in GitHub's list of the
	// pull request's commits, which is oldest first; the head commit, whose trailer names another
	// issue, comes last.
	deliver(ev162Later, "opened", ev162Pull(ev162Later, "open", false, ev162Head2, "move-to-the-image", "Move the fleet to the new image", laterAt), []string{
		"Move the fleet to the new release\n\nDispatch: " + ev162Closed,
		"Move the fleet to the new image\n\nDispatch: " + ev162Newer,
	})

	later := answerOf(t, unrenamed(), ctx, pool, ev162Repo, ev162Later)
	merged := answerOf(t, unrenamed(), ctx, pool, ev162Repo, ev162Merged)
	closedHeads := listedHeads(t, ctx, pool, ev162Closed)
	newerHeads := listedHeads(t, ctx, pool, ev162Newer)
	newest, found, err := unrenamed().NewestPullRequestHead(ctx, pool, ev162Closed, HeadAnswers{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("#%d: %s; #%d: %s; %s lists %v, newest head #%d %.8s (found %v); %s lists %v",
		ev162Later, later, ev162Merged, merged, ev162Closed, closedHeads, newest.Number, newest.HeadSHA, found, ev162Newer, newerHeads)
	if want := ev162Newer + " from " + string(sourceCommitMessage); later != want {
		t.Errorf("#%d is %s, want %s: the head commit's trailer is the branch's word now, and an older commit's trailer for a closed issue is history it carries", ev162Later, later, want)
	}
	if want := ev162Closed + " from " + string(sourceBranch); merged != want {
		t.Errorf("#%d is %s, want %s", ev162Merged, merged, want)
	}
	if want := fmt.Sprintf("[#%d at %.8s]", ev162Merged, ev162Head1); fmt.Sprint(closedHeads) != want {
		t.Errorf("%s lists %v, want %s alone: a later pull request must not take the closed issue's CI evidence", ev162Closed, closedHeads, want)
	}
	if !found || newest.Number != ev162Merged || newest.HeadSHA != ev162Head1 {
		t.Errorf("%s's newest head is #%d at %.8s (found %v), want the merged #%d at %.8s, where its check CI proves is read",
			ev162Closed, newest.Number, newest.HeadSHA, found, ev162Merged, ev162Head1)
	}
	if want := fmt.Sprintf("[#%d at %.8s]", ev162Later, ev162Head2); fmt.Sprint(newerHeads) != want {
		t.Errorf("%s lists %v, want %s", ev162Newer, newerHeads, want)
	}
}

// listedHeads is each pull request HeadsAttributedTo lists for issue, as "#<number> at <head>".
func listedHeads(t *testing.T, ctx context.Context, pool Querier, issue string) []string {
	t.Helper()
	heads, err := unrenamed().HeadsAttributedTo(ctx, pool, issue, HeadAnswers{})
	if err != nil {
		t.Fatalf("HeadsAttributedTo(%s): %v", issue, err)
	}
	listed := []string{}
	for _, head := range heads {
		listed = append(listed, fmt.Sprintf("#%d at %.8s", head.Number, head.HeadSHA))
	}
	return listed
}
