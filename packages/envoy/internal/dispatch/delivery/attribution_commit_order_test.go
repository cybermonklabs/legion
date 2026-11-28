package delivery

// A production case (event 162 of a closed issue, received by #4209 on 2026-11-25): a pull request
// merged for an issue, and the issue closed. Nine minutes later another pull request opened whose
// branch, title and body name no issue. Its commit list carried two `Dispatch:` trailers: an older
// commit's, naming the closed issue, and its head commit's, naming another issue. The older
// trailer won, so the later pull request was credited to the closed issue from its commit messages,
// and the closed issue's check that CI proves read the later pull request's head, where its own
// merged head's check runs had passed. How the older commit entered the branch is not known.
//
// The rule (R9 of the results lookup): where a pull request's commit messages name more than one
// key, each naming an issue Dispatch holds in any repository, the messages credit no issue, and the
// higher sources alone decide; one key in them credits its issue. No order of the commits picks
// one: the oldest first credits the closed issue here, and the newest first credits a commit
// cherry-picked last (TestCommitMessagesNamingTwoIssuesCreditNoIssue).
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

// The later pull request, whose messages name both issues, credits neither, and not the closed
// issue an older commit's trailer names; the closed issue keeps its merged pull request as its only
// pull request, and the newest head of the issue, the head the check CI proves is read at, is that
// merged head (NewestPullRequestHead). The issue the head's trailer names lists nothing.
// At a6003a51b0 the older trailer won: #901 was credited to ACME-6201 from its commit messages,
// and ACME-6201's newest head was #901's.
func TestALaterPullRequestWhoseCommitsNameTwoIssuesCreditsNeitherAndNotTheClosedOne(t *testing.T) {
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
	// issue, comes last. Neither its branch, its title nor its body names an issue.
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
	if want := "nobody from " + string(sourceNone); later != want {
		t.Errorf("#%d is %s, want %s: its commit messages name two issues, so they credit none, and no higher source names one", ev162Later, later, want)
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
	if want := "[]"; fmt.Sprint(newerHeads) != want {
		t.Errorf("%s lists %v, want %s: the later pull request credits no issue", ev162Newer, newerHeads, want)
	}
}

// TestCommitMessagesNamingTwoIssuesCreditNoIssue holds the rule over commit messages (R9 of the
// results lookup) row by row, each through the statement (resolvedKey): messages that name two
// keys of issues Dispatch holds, in any repository, credit no issue, whatever the order of the
// commits; one key credits its issue; a token of a key's shape that names no issue is no key; and
// where the messages name two, the branch and the body still decide. The rows the old orders get
// wrong are the red: the commits in GitHub's order, oldest first, credit the closed issue of event
// 162, and newest first credit the commit cherry-picked last.
func TestCommitMessagesNamingTwoIssuesCreditNoIssue(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	seedAttributionIssues(t, ctx, pool, []string{"ATTR-141", "ATTR-142", "ATTR-143", "ATTR-144", "ATTR-145", "ATTR-146",
		"ATTR-147", "ATTR-148", "ATTR-149", "ATTR-150", "ATTR-151", "ATTR-152"}, nil)
	if _, err := pool.Exec(ctx, `update issues set status = 'done', closed_at = now() where key = 'ATTR-141'`); err != nil {
		t.Fatalf("close ATTR-141: %v", err)
	}
	other := seedGadgetsIssue(t, ctx, pool, 140)
	for _, row := range []struct {
		name         string
		branch, body string
		messages     []string
		wantKey      string
		wantSource   attributionSource
	}{
		{name: "an older commit's trailer of a closed issue and the head's of another (event 162)",
			messages:   []string{"Move the fleet to the new release\n\nDispatch: ATTR-141", "Move the fleet to the new image\n\nDispatch: ATTR-142"},
			wantSource: sourceNone},
		{name: "a commit of another issue cherry-picked last",
			messages:   []string{"Fix the lease\n\nDispatch: ATTR-143", "Fix the pool\n\n(cherry picked from commit 4c2a9e1)\n\nDispatch: ATTR-144"},
			wantSource: sourceNone},
		{name: "the head commit alone names two keys",
			messages:   []string{"Fix the flake ATTR-146 found\n\nDispatch: ATTR-145"},
			wantSource: sourceNone},
		{name: "a commit of an issue of another repository cherry-picked last",
			messages:   []string{"Fix the lease\n\nDispatch: ATTR-149", "Fix the pool\n\nDispatch: " + other},
			wantSource: sourceNone},
		{name: "one key in two commits",
			messages: []string{"Start the work\n\nDispatch: ATTR-147", "Finish the work\n\nDispatch: ATTR-147"},
			wantKey:  "ATTR-147", wantSource: sourceCommitMessage},
		{name: "SHA-256 beside one key",
			messages: []string{"Hash each body with SHA-256\n\nDispatch: ATTR-148"},
			wantKey:  "ATTR-148", wantSource: sourceCommitMessage},
		{name: "two keys in the messages and a key in the branch",
			branch:   "legion/ATTR-150",
			messages: []string{"Fix the lease\n\nDispatch: ATTR-143", "Fix the pool\n\nDispatch: ATTR-144"},
			wantKey:  "ATTR-150", wantSource: sourceBranch},
		{name: "two keys in the messages and a Dispatch line in the body",
			body:     "Dispatch: ATTR-151",
			messages: []string{"Fix the lease\n\nDispatch: ATTR-143", "Fix the pool\n\nDispatch: ATTR-152"},
			wantKey:  "ATTR-151", wantSource: sourceTitleBody},
	} {
		key, source := resolvedKey(t, ctx, pool, ruleFacts{
			Repo: "acme/widgets", URL: "https://github.com/acme/widgets/pull/140",
			HeadRef: row.branch, Body: row.body, Title: "Tidy the build", CommitMessages: row.messages,
		})
		if key != row.wantKey || source != row.wantSource {
			t.Errorf("%s: resolved %q from %q, want %q from %q", row.name, key, source, row.wantKey, row.wantSource)
		}
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
