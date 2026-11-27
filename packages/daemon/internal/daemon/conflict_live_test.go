//go:build e2e

// The harness the two live proofs of a head that conflicts with its base share:
// conflict_no_takeover_live_test.go's, a conflict in testing that the implementer merges forward
// (dispatch://INFRA-4374), and conflict_merger_live_test.go's, a conflict past the review that the
// merger merges forward (cybermonklabs/legion#4979). Each driver embeds conflictLive, liveIssueProof
// (live_issue_proof_test.go) grown for a conflict: the daemon booted with its own log kept in
// memory as it is written, which the steps read the daemon's decisions of a conflicting head from;
// what the listener stand-in took on each role topic of an issue since a step's baseline; the
// notices the issue's architect took since then; the wait for the outbox to have published every
// message of an issue; the read of a role's connection that must stay silent; and what GitHub
// answers of the base branch for a head it answers conflicting. What one proof alone drives stays
// in its driver.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// conflictBase is the base branch of every pull request of these proofs, the repository's
// default, and conflictRequiredCheck the one check its ruleset requires (conflictBaseRules). The
// daemon reads that ruleset for a conflicting head (readBaseRules).
const (
	conflictRequiredCheck = "ci"
	conflictBase          = "main"
)

// The lines the daemon logs for what it decides of a conflicting head, which the steps read its
// decisions from: the first read (workflow's mergeability), the second read's attempt that GitHub
// answered (the outbox's conflictConfirm), the ask a confirmed conflict needs (workflow's
// mergeForward), and the confirmed conflict that needs none (mergeability).
const (
	conflictFirstReadLine  = "workflow: a conflict read waits for a second read"
	conflictSecondReadLine = "outbox conflict confirm: GitHub answered; the workflow decides on the answer"
	conflictAskedLine      = "workflow: merge forward asked"
	conflictNoMergeLine    = "workflow: a confirmed conflict needs no merge yet"
)

// conflictSecondReadBound is how long a step waits for what the second read decides: the minute
// the daemon waits after a first read (workflow's conflictConfirmWait), with room for the lane that
// runs the read and the one that publishes the messages it writes.
const conflictSecondReadBound = 3 * time.Minute

// conflictLive is what a live proof of a conflicting head holds beyond liveIssueProof: the daemon's
// own log, kept in memory as it is written, and how many messages each role topic of the issue had
// taken at the step's baseline, so a later step reads what the conflict sent apart from anything
// before it.
type conflictLive struct {
	liveIssueProof
	// logged is the daemon's own log, kept in memory as it is written, which the steps read the
	// daemon's decisions of a conflicting head from.
	logged *recordingLog
	// heard is how many messages each role topic of the issue had taken at the baseline a step set.
	heard map[claim.Role]int
}

// bootLogged boots the daemon as liveIssueProof.boot does, with the daemon's own log also kept in
// memory as it is written (recordingLog, as sub_architect_live_test.go keeps it), since the steps
// read what the daemon decided of a conflicting head from the lines it logs for it.
func (r *conflictLive) bootLogged() {
	r.t.Helper()
	r.rt.InPod = true
	o := fakeRuntime(r.rt, r.built)
	o.workflowTokens = &workflowTokenRecorder{}
	o.githubAPI = r.github.url
	r.logged = newRecordingLog(r.log())
	r.d = startDaemonLogging(r.t, r.cfg, o, slog.New(r.logged))
}

// silent holds that the daemon sends this role's connection nothing at all for within: the RPC
// carries no abort frame, and nothing else it could send would end that role's turn either.
func (c *liveClaimant) silent(within time.Duration) {
	t := c.p.t
	t.Helper()
	if err := c.sh.conn.SetReadDeadline(time.Now().Add(within)); err != nil {
		t.Fatalf("set the read deadline on the %s's connection: %v", c.role, err)
	}
	line, err := c.sh.lines.ReadBytes('\n')
	var timeout net.Error
	switch {
	case errors.As(err, &timeout) && timeout.Timeout():
		return
	case err != nil:
		t.Fatalf("read the %s's connection: %v", c.role, err)
	default:
		t.Fatalf("the daemon sent the %s %q, want nothing at all", c.role, strings.TrimSpace(string(line)))
	}
}

// loggedFor is every line the daemon logged with message for issue, oldest first.
func (r *conflictLive) loggedFor(message, issue string) []loggedRecord {
	return r.logged.matching(func(kept loggedRecord) bool { return kept.Message == message && kept.Attrs["issue"] == issue })
}

// awaitLogged waits up to bound for the daemon's first line with message for issue, and returns it.
// The log grows with every line the daemon writes at debug, so it is read ten times a second rather
// than at testwait's own pace.
func (r *conflictLive) awaitLogged(message, issue string, bound time.Duration) loggedRecord {
	r.t.Helper()
	var lines []loggedRecord
	testwait.WithinPolling(r.t, bound, 100*time.Millisecond, fmt.Sprintf("the daemon's line %q for %s", message, issue), func() bool {
		lines = r.loggedFor(message, issue)
		return len(lines) > 0
	})
	return lines[0]
}

// roleTopic is the Envoy role topic of role's claim on issue, as the daemon spells it.
func (r *conflictLive) roleTopic(issue string, role claim.Role) string {
	r.t.Helper()
	token, err := claim.NewToken(r.project, issue, role)
	if err != nil {
		r.t.Fatal(err)
	}
	return notify.RoleTopicPrefix + string(token)
}

// heardOn is how many messages the listener has taken on the role topic of each of issue's roles,
// the architect's among them.
func (r *conflictLive) heardOn(issue string) map[claim.Role]int {
	heard := map[claim.Role]int{}
	for _, role := range claim.Roles {
		heard[role] = len(r.notices.acceptedOn(r.roleTopic(issue, role)))
	}
	return heard
}

// awaitHeard waits up to bound for the role topic of role on the issue to have taken count messages
// more than it had at the baseline (heard), and returns every message it took since then, oldest
// first.
func (r *conflictLive) awaitHeard(role claim.Role, count int, bound time.Duration) []listenerPublish {
	r.t.Helper()
	topic := r.roleTopic(r.key, role)
	var heard []listenerPublish
	testwait.WithinPolling(r.t, bound, testwait.Poll, fmt.Sprintf("%d message(s) on the %s's role topic %s since the baseline", count, role, topic), func() bool {
		heard = r.notices.acceptedOn(topic)
		return len(heard) >= r.heard[role]+count
	})
	return heard[r.heard[role]:]
}

// architectNoticesSince is every notice the architect's role topic of the issue took since the
// baseline (heard), each decoded from the payload the outbox publishes it with (record.Notice).
func (r *conflictLive) architectNoticesSince() []record.Notice {
	r.t.Helper()
	var notices []record.Notice
	for _, published := range r.notices.acceptedOn(r.roleTopic(r.key, claim.RoleArchitect))[r.heard[claim.RoleArchitect]:] {
		var notice record.Notice
		if err := json.Unmarshal([]byte(published.Payload), &notice); err != nil {
			r.t.Fatalf("decode the architect's notice %q: %v", published.Payload, err)
		}
		notices = append(notices, notice)
	}
	return notices
}

// published waits for the outbox to hold no message of issue still to be published to a role
// topic — a notice, a role message, a hook prompt or a merge-forward message — so the step that
// counts what the topics took does not count while a message an earlier step wrote is on its way.
func (r *conflictLive) published(issue string) {
	r.t.Helper()
	kinds := []string{string(record.OutboxKindNotice), string(record.OutboxKindRoleMessage), string(record.OutboxKindHookPrompt),
		string(record.OutboxKindMergeForward)}
	testwait.Eventually(r.t, "the outbox to publish every message of "+issue+" it holds", func() bool {
		var rows int
		if err := r.st.Pool().QueryRow(context.Background(), "select count(*) from outbox where issue = $1 and kind = any($2)",
			issue, kinds).Scan(&rows); err != nil {
			r.t.Fatalf("read the outbox rows of %s: %v", issue, err)
		}
		return rows == 0
	})
}

// conflictBaseRules is what GitHub answers of the base for a head it answers conflicting, as the
// daemon reads it (readBaseRules): the rules of the branch, one ruleset requiring ci whose rule
// does not require a pull request to be current with the branch, and the branch itself unprotected
// otherwise, which a 404 would make rules the daemon could not read, and act on at once
// (record.ConflictRulesUnread). Each answer is keyed by its path under /repos/acme/widgets, as
// liveGitHub's also holds it, and a driver adds its own answers to the map it is given.
func conflictBaseRules() map[string]any {
	return map[string]any{
		"/rules/branches/" + conflictBase: []map[string]any{
			{"type": "pull_request"},
			{"type": "required_status_checks", "parameters": map[string]any{
				"required_status_checks": []map[string]string{{"context": conflictRequiredCheck}},
			}},
		},
		"/branches/" + conflictBase: map[string]any{"name": conflictBase, "protected": false},
	}
}
