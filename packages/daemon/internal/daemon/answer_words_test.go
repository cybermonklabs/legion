package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testreason"
)

// The rows of this file hold the daemon's lines that name a member of GitHub's answer the daemon
// decodes as a plain string - a commit's sha, a state, an activity's type, an account's login - to
// the shape GitHub gives that member (githubrest.ShownSHA, ShownWord, ShownLogin, SHAKey). In each,
// GitHub, or whatever answers in its place, writes the member as words that echo the installation
// token the read carried (the stand-ins' outbox-token) and quote a URL carrying made-up
// credentials (oauth2calltest.Words). The line names the member by its fixed phrase and holds none
// of what the stand-in wrote there; a control holds the member printed as it came where it has
// GitHub's shape.

// echoedToken is the installation token every stand-in of this file is asked with (outboxTokens).
const echoedToken = "outbox-token"

// echoedWords is a member of GitHub's answer that echoes the read's token and quotes the probe.
const echoedWords = "token " + echoedToken + " refused; " + oauth2calltest.Words

// realCommit is a commit's sha as GitHub writes one.
const realCommit = "0123456789abcdef0123456789abcdef01234567"

// The fixed phrases a line prints in place of a member that is not in GitHub's shape
// (githubrest.NotACommit, NotDocumented and NotALogin), written out so each row pins the text.
const (
	notACommit    = "(a sha that is not a commit's)"
	notDocumented = "(a word GitHub does not document there)"
	notALogin     = "(a login GitHub does not allow)"
)

// requireNoEcho is testreason.RequireNone of text under capture, and fails t where text holds a run
// of eight characters of the token the read carried.
func requireNoEcho(t *testing.T, capture, text string) {
	t.Helper()
	testreason.RequireNone(t, capture, text)
	for at := 0; at+8 <= len(echoedToken); at++ {
		if strings.Contains(text, echoedToken[at:at+8]) {
			t.Errorf("%s: the output prints the token the read carried: %q", capture, text)
			return
		}
	}
}

// requireHolds fails t for each of wants that text does not hold.
func requireHolds(t *testing.T, text string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Errorf("the output is %s, want %s in it", text, want)
		}
	}
}

// TestAPermissionReadLogsGitHubsLoginAndPermissionOnlyInTheirShape: GitHub's collaborator
// permission answer names another account than the review's author (another-login), or a
// permission that is no word GitHub documents (permission), each echoing the read's token and the
// probe; and a review read from GitHub's list names an author that is no login GitHub allows
// (author), whose read GitHub answers 404. The permission read's lines name each by its fixed
// phrase. The control's account and permission, a login and a documented word, print as they
// came. Captures daemon.answer-words/permission-<case>.
func TestAPermissionReadLogsGitHubsLoginAndPermissionOnlyInTheirShape(t *testing.T) {
	pool := reviewPermissionPool(t)
	for _, c := range []struct {
		name, author, answered, permission string
		want                               []string
	}{
		{"another-login", "a-writer", echoedWords, "write", []string{`answered_login="` + notALogin + `"`, "permission=write"}},
		{"permission", "a-writer", "a-writer", echoedWords, []string{`permission="` + notDocumented + `"`}},
		{"author", echoedWords, "", "", []string{`login="` + notALogin + `"`, "status=404"}},
		{"control", "a-writer", "Copilot", "read", []string{"answered_login=Copilot", "permission=read"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if c.answered == "" {
					http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
					return
				}
				fmt.Fprintf(w, `{"permission":%q,"user":{"login":%q}}`, c.permission, c.answered)
			}))
			defer server.Close()
			var logs bytes.Buffer
			w := reviewPermissionRuntime(pool, server.URL, slog.New(slog.NewTextHandler(&logs, nil)))
			canWrite, err := w.reviewerCanWrite(context.Background(), intake.PullRequestReview{Repo: "acme/widgets", Number: 42, State: "approved", Author: c.author})
			if err != nil || canWrite {
				t.Fatalf("the review's author can write = %v, %v; want false and no error", canWrite, err)
			}
			requireNoEcho(t, "daemon.answer-words/permission-"+c.name, logs.String())
			requireHolds(t, logs.String(), c.want...)
		})
	}
}

// TestAMergeQueueReadLogsGitHubsMergeCommitAndStateOnlyInTheirShape: GitHub answers the merge-queue
// read of a merged pull request with a merge commit that is no sha (merged), and of one not merged
// with a state that is no word GitHub documents (state), each echoing the read's token and the
// probe. The read's line names each by its fixed phrase; the control's sha and state print as they
// came. Captures daemon.answer-words/merge-queue-<case>.
func TestAMergeQueueReadLogsGitHubsMergeCommitAndStateOnlyInTheirShape(t *testing.T) {
	pull := func(state string, merged bool, mergeCommit string) string {
		return fmt.Sprintf(`{"number":86,"state":%q,"merged":%t,"merge_commit_sha":%q,"base":{"ref":"main"},"head":{"ref":"legion/CAPTURE-1","sha":"code"},"mergeable":true,"mergeable_state":"clean"}`,
			state, merged, mergeCommit)
	}
	for _, c := range []struct {
		name, pull string
		want       []string
	}{
		{"merged", pull("closed", true, echoedWords), []string{`mergeCommit="` + notACommit + `"`}},
		{"state", pull(echoedWords, false, ""), []string{`state="` + notDocumented + `"`}},
		{"control-merged", pull("closed", true, realCommit), []string{"mergeCommit=" + realCommit}},
		{"control-state", pull("open", false, ""), []string{"state=open"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			seedCapture(t, pool, phase.Merging, "in_progress")
			w := readRuntime(pool, pullStandIn(t, c.pull).URL, quietLogger())
			var logs bytes.Buffer
			r := &outbox{pool: pool, records: record.NewStore(), dispatchProject: "CAPTURE", log: slog.New(slog.NewTextHandler(&logs, nil)), now: time.Now,
				readPullRequest: w.readPullRequest, applyMergeability: w.applyMergeability, mergeStateWait: w.mergeStateWait}
			if err := r.mergeQueueRead(context.Background(), record.OutboxRow{ID: 7, Issue: "CAPTURE-1"}, record.MergeQueueRead{Number: 86, Head: "code"}); err != nil {
				t.Fatalf("the merge-queue read: %v", err)
			}
			requireNoEcho(t, "daemon.answer-words/merge-queue-"+c.name, logs.String())
			requireHolds(t, logs.String(), append([]string{dequeueReadLine}, c.want...)...)
		})
	}
}

// refusingHandler refuses every fact, so a read that follows GitHub's head fails at its apply and
// says so in its error, which the per-issue read logs.
type refusingHandler struct{}

func (refusingHandler) Apply(context.Context, pgx.Tx, intake.Fact) (intake.Result, error) {
	return intake.Result{}, errors.New("the store refuses every fact")
}

// TestAFollowedHeadLogsGitHubsShaOnlyInItsShape: GitHub shows the issue's pull request at a head
// that is no sha, echoing the read's token and the probe, and every fact the read applies is
// refused. Where the branch's activity does not lead back to the recorded head (unfollowed), the
// read's lines - the activity it cannot follow, the synchronize it applies, the apply's error -
// name the head by its fixed phrase; where it does, through a push to that head (pushed), so does
// the push's apply error. Captures daemon.answer-words/follow-head-<case>.
func TestAFollowedHeadLogsGitHubsShaOnlyInItsShape(t *testing.T) {
	at := time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name     string
		head     string
		activity []string
		want     []string
	}{
		{"unfollowed", echoedWords, nil, []string{`"head":"` + notACommit + `"`, "left it at " + notACommit, "apply the synchronize to " + notACommit}},
		{"pushed", echoedWords, []string{activityEntry("legion/CAPTURE-1", "push", "code", echoedWords, implementerLogin, at)}, []string{"apply the push to " + notACommit}},
		{"control", realCommit, nil, []string{`"head":"` + realCommit + `"`, "apply the synchronize to " + realCommit}},
	} {
		t.Run(c.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			seedCapture(t, pool, phase.Testing, "testing", record.PhaseRow{Issue: "CAPTURE-1", Role: claim.RoleTester, Claim: "test-claim"})
			github := &activityGitHub{}
			github.moved(c.head, at, c.activity...)
			var logs bytes.Buffer
			w := reviewAppRuntime(pool, github.serve(t).URL, slog.New(slog.NewJSONHandler(&logs, nil)))
			w.handlers = []intake.Handler{refusingHandler{}}
			readCapture(t, w)
			requireNoEcho(t, "daemon.answer-words/follow-head-"+c.name, logs.String())
			requireHolds(t, logs.String(), c.want...)
		})
	}
}

// TestAnOpenedReadLogsGitHubsShaOnlyInItsShape: the record holds no pull request for the issue,
// and GitHub lists one open on its branch at a head that is no sha, echoing the read's token and
// the probe. The line that says the read applies its opened names the head by its fixed phrase;
// the control's sha prints as it came. Captures daemon.answer-words/opened-read-<case>.
func TestAnOpenedReadLogsGitHubsShaOnlyInItsShape(t *testing.T) {
	opened := time.Date(2026, 10, 26, 19, 30, 0, 0, time.UTC)
	for _, c := range []struct {
		name, head, want string
	}{
		{"echo", echoedWords, `head="` + notACommit + `"`},
		{"control", realCommit, "head=" + realCommit},
	} {
		t.Run(c.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			seedUnrecorded(t, pool, "handoff")
			var logs bytes.Buffer
			w := readRuntime(pool, pullRequestStandIn(t, c.head, opened, &atomic.Int32{}).URL, slog.New(slog.NewTextHandler(&logs, nil)))
			if _, err := w.readIssuePullRequest(context.Background(), "CAPTURE-1"); err != nil {
				t.Fatalf("read the issue's pull request: %v", err)
			}
			requireNoEcho(t, "daemon.answer-words/opened-read-"+c.name, logs.String())
			requireHolds(t, logs.String(), "GitHub holds an open pull request for an issue the daemon records none for", c.want)
		})
	}
}

// TestAnActivityItCannotFollowIsNamedOnlyInItsShape: each reason followActivity gives for a branch's
// activity it cannot follow names GitHub's commits and the entry's type only in their shape: the
// head no entry left the branch at (not-on-the-page), the commit an entry left it at (left-it-at),
// an entry's type that is no word GitHub documents (not-a-push), and the head the page does not
// lead back from (no-way-back), each echoing the read's token and the probe. The control's commits
// and type print as they came. Captures daemon.answer-words/activity-<case>.
func TestAnActivityItCannotFollowIsNamedOnlyInItsShape(t *testing.T) {
	entry := func(kind, before, after string) githubrest.Activity {
		return githubrest.Activity{Type: kind, Before: before, After: after}
	}
	other := strings.Repeat("1", 40)
	for _, c := range []struct {
		name     string
		activity []githubrest.Activity
		from, to string
		want     string
	}{
		{"not-on-the-page", nil, "code", echoedWords, "left it at " + notACommit},
		{"left-it-at", []githubrest.Activity{entry("push", other, realCommit), entry("push", "code", echoedWords)}, "code", realCommit, "before it reached " + other + " left it at " + notACommit},
		{"not-a-push", []githubrest.Activity{entry(echoedWords, "code", realCommit)}, "code", realCommit, "by a " + notDocumented + ", not a push"},
		{"no-way-back", []githubrest.Activity{entry("push", other, echoedWords)}, "code", echoedWords, "lead back from " + notACommit},
		{"control", []githubrest.Activity{entry("branch_deletion", "code", realCommit)}, "code", realCommit, "reached " + realCommit + " by a branch_deletion, not a push"},
	} {
		t.Run(c.name, func(t *testing.T) {
			chain, unfollowed := followActivity(c.activity, c.from, c.to)
			if chain != nil || unfollowed == "" {
				t.Fatalf("followActivity = %v, %q; want no chain and a reason", chain, unfollowed)
			}
			requireNoEcho(t, "daemon.answer-words/activity-"+c.name, unfollowed)
			requireHolds(t, unfollowed, c.want)
		})
	}
}

// TestAPushReadsEventIDNamesNoCommitThatIsNotOne: a push the read rebuilds from GitHub's activity is
// applied under an event id that names its two commits, which a log line can print; where GitHub
// names a commit that is no sha, echoing the read's token and the probe, the id names it by a
// digest, which still tells it from another such commit. The control's commits are named as they
// came. Captures daemon.answer-words/push-read-<case>.
func TestAPushReadsEventIDNamesNoCommitThatIsNotOne(t *testing.T) {
	w := &workflowRuntime{dispatchProject: "CAPTURE"}
	pr := record.PullRequest{Repo: "acme/widgets", Number: 86}
	before := strings.Repeat("1", 40)
	id := w.pushReadEventID(pr, before, echoedWords)
	requireNoEcho(t, "daemon.answer-words/push-read-echo", id)
	if id == w.pushReadEventID(pr, before, echoedWords+" again") {
		t.Errorf("two pushes to commits that are no sha share the event id %q", id)
	}
	control := w.pushReadEventID(pr, before, realCommit)
	requireNoEcho(t, "daemon.answer-words/push-read-control", control)
	requireHolds(t, control, before+"..."+realCommit)
}

// TestAListedReviewLogsItsAuthorOnlyInItsShape: GitHub lists a deciding review whose author is no
// login GitHub allows, echoing the read's token and the probe: one with no id, which no review can
// be ordered by (unordered), and one GitHub gives no write access, which the read passes over
// (refused). The read's lines name the author by its fixed phrase; the control's author, a login,
// prints as it came. Captures daemon.answer-words/listed-review-<case>.
func TestAListedReviewLogsItsAuthorOnlyInItsShape(t *testing.T) {
	at := time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC)
	review := func(id int, author string) string {
		return fmt.Sprintf(`[{"id":%d,"user":{"login":%q},"body":"Ship it.","state":"APPROVED","commit_id":"code","submitted_at":%q}]`,
			id, author, at.Format(time.RFC3339))
	}
	for _, c := range []struct {
		name, reviews string
		want          []string
	}{
		{"unordered", review(0, echoedWords), []string{"GitHub listed " + notALogin + "'s approved review"}},
		{"refused", review(5, echoedWords), []string{`"author":"` + notALogin + `"`, "a review GitHub lists decides nothing"}},
		{"control", review(5, "a-stranger"), []string{`"author":"a-stranger"`, "a review GitHub lists decides nothing"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			seedCapture(t, pool, phase.Reviewing, "needs_review", record.PhaseRow{Issue: "CAPTURE-1", Role: claim.RoleReviewer, Claim: "review-claim"})
			github := &activityGitHub{}
			github.moved("code", captureHeadUpdated)
			github.lists(c.reviews)
			var logs bytes.Buffer
			w := reviewAppRuntime(pool, github.serve(t).URL, slog.New(slog.NewJSONHandler(&logs, nil)))
			w.repositories = servedAcme()
			readCapture(t, w)
			requireNoEcho(t, "daemon.answer-words/listed-review-"+c.name, logs.String())
			requireHolds(t, logs.String(), c.want...)
		})
	}
}
