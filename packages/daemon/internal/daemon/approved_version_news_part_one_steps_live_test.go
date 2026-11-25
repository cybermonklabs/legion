//go:build e2e

package daemon

// approved_version_news_part_one_steps_live_test.go holds part one's steps of the
// approved-version-news live proof, which TestAnApprovedVersionReachesItsHolderLive
// (approved_version_news_live_test.go) runs in the order its constants give, and the reads of an
// issue's phase, of a claim's queued task and of a task's text that the steps check with. Part
// two's steps are approved_version_news_steps_live_test.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// awaitPhase waits for key to stand in want.
func (r *versionNewsLive) awaitPhase(key string, want phase.Phase) {
	r.t.Helper()
	r.await(fmt.Sprintf("%s in %s", key, want), versionNewsWait, func() (bool, string) {
		got := r.stateIssue(key).Phase
		return got == want, "phase " + string(got)
	})
}

// holdPhase holds that key stays in want for liveSettle.
func (r *versionNewsLive) holdPhase(key string, want phase.Phase) {
	r.t.Helper()
	deadline := time.Now().Add(liveSettle)
	for time.Now().Before(deadline) {
		if got := r.stateIssue(key).Phase; got != want {
			r.fail("%s moved to %s, want it to stay in %s", key, got, want)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// awaitQueued waits for a task the daemon queued for c after mark that it has not sent, which is a
// task waiting behind the turn c is in, and returns it. A task it sent into that turn fails the
// run. In A2 its failure is the one-approval check's: `waited <bound> for a task queued for the
// reviewer of <key> behind its open turn; last seen: …`. The script's negative control bites on
// those words alone (control_failure in approved-version-news.sh), so change both together.
func (r *versionNewsLive) awaitQueued(c *versionNewsClaimant, mark time.Time) api.DeliveryView {
	r.t.Helper()
	var queued api.DeliveryView
	r.await("a task queued for "+c.name()+" behind its open turn", versionNewsWait, func() (bool, string) {
		pending := r.d.claim(c.token).Pending
		switch {
		case pending == nil:
			return false, "its claim holds no task"
		case !pending.QueuedAt.After(mark):
			return false, fmt.Sprintf("its claim holds task %s, queued %s, before the approval", pending.ID, pending.QueuedAt.UTC().Format(time.RFC3339Nano))
		case pending.DeliveredAt != nil:
			r.fail("the daemon sent task %s into %s's open turn", pending.ID, c.name())
		}
		queued = *pending
		return true, ""
	})
	return queued
}

// quiet holds that none of claimants was sent a prompt after mark until until, waiting until then:
// until is a moment a delivery the same run makes was seen after mark, and liveSettle past it, so
// the window these claimants were quiet in is one that sees a delivery.
func (r *versionNewsLive) quiet(claimants []*versionNewsClaimant, mark, until time.Time) {
	r.t.Helper()
	time.Sleep(time.Until(until))
	for _, c := range claimants {
		if prompts := c.since(mark); len(prompts) > 0 {
			r.fail("%s was sent %d prompts in the window, the first %q", c.name(), len(prompts), versionNewsFirstLine(prompts[0].message))
		}
	}
}

// versionNewsNames lists claimants as a check names them.
func versionNewsNames(claimants []*versionNewsClaimant) string {
	said := make([]string, len(claimants))
	for i, c := range claimants {
		said[i] = c.name()
	}
	return strings.Join(said, ", ")
}

// versionNewsFirstLine is the first line of a task, for a note that should not carry the whole of
// it.
func versionNewsFirstLine(task string) string {
	line, _, _ := strings.Cut(task, "\n")
	if len(line) > 160 {
		line = line[:160] + "…"
	}
	return line
}

// versionNewsNamesVersion says whether text names version n as a word of its own: "version 2",
// never the "2" of "version 21".
func versionNewsNamesVersion(text string, n int) bool {
	return regexp.MustCompile(`\bversion ` + strconv.Itoa(n) + `\b`).MatchString(text)
}

// versionNewsNamesCheck says whether text names check where the news names a changed check, at the
// head of its `Checks:` line (`Checks: "c-a7" changed.`), the one check of each tree's Verification
// document, quoted as the news quotes a block id, which is the document's own text. A bare check id
// is no proof: `c-a7` can stand inside a UUID the news prints, and the quotes are what keep
// `Checks: "c-a7"` from matching `Checks: "c-a7-open"`.
func versionNewsNamesCheck(text, check string) bool {
	return strings.Contains(text, "Checks: "+strconv.Quote(check)+" ")
}

// Step A2. A tree registers its spec alone, and its gate opens on the setup approval of version 1.
// A new version that changes `## Design` closes it, and while the reviewer is in a turn the person
// approves it. The reviewer's task waits behind that turn, the daemon sending its connection
// nothing, and once the driver ends the turn the task arrives naming `Design` and the two versions,
// and not the `Summary` that did not change. The issue stays in reviewing until the reviewer's
// request_backward_move, which moves it to implementing. The in-run negative: a second tree whose
// gate the approval did not move was sent nothing, in the window the reviewer's task arrived in.
//
// The first five checks hold on a daemon from before the change as well; the sixth, the task
// queued behind the turn, is the run's first one-approval check, where the negative control fails.
func (r *versionNewsLive) a2() {
	tree, unmoved := r.trees["a2"], r.trees["a2-unmoved"]
	reviewer := r.claimant(tree.root(), claim.RoleReviewer)
	others := []*versionNewsClaimant{r.claimant(unmoved.root(), claim.RoleReviewer), r.claimant(unmoved.root(), claim.RoleArchitect)}
	r.openTree(tree)
	r.openTree(unmoved)
	r.newVersion(tree, record.DesignStepSpec, versionNewsDesign, versionNewsDesignAgain, 2)
	r.held("version 2 of %s's spec, changing its Design section, closed the gate", tree.root().key)
	reviewer.startTurn()
	r.held("%s is in a turn", reviewer.name())
	mark := time.Now()
	r.approve(tree.spec, 2)
	r.awaitSteps(tree, "the gate of "+tree.root().key+" open at version 2", func(views map[record.DesignStep]api.GatedStepView) bool {
		view := views[record.DesignStepSpec]
		return view.Status == api.GatedStepApproved && view.CurrentVersion == 2
	})
	r.held("the person's approval of version 2 opened the gate again")

	queued := r.awaitQueued(reviewer, mark)
	r.held("a task for %s is queued behind its open turn (delivery %s)", reviewer.name(), queued.ID)
	time.Sleep(liveSettle)
	if prompts := reviewer.since(mark); len(prompts) > 0 {
		r.fail("%s was sent %q while its turn ran", reviewer.name(), versionNewsFirstLine(prompts[0].message))
	}
	r.held("%s's connection was sent nothing while its turn ran (%s)", reviewer.name(), liveSettle)
	ended := time.Now()
	reviewer.endTurn()
	task := reviewer.awaitPrompt("the news task", ended)
	r.note("%s was prompted, once its turn ended:\n%s", reviewer.name(), task.message)
	if task.delivery != queued.ID {
		r.fail("%s's first task after its turn is delivery %q, want %s, the one queued behind the turn", reviewer.name(), task.delivery, queued.ID)
	}
	if !strings.Contains(task.message, `"Design"`) || strings.Contains(task.message, `"Summary"`) ||
		!versionNewsNamesVersion(task.message, 2) || !versionNewsNamesVersion(task.message, 1) {
		r.fail("%s's task names %s, want the changed section \"Design\", not the unchanged \"Summary\", and versions 2 and 1: %q",
			reviewer.name(), versionNewsFirstLine(task.message), task.message)
	}
	r.held("the task queued behind the turn (delivery %s) reached %s only after its turn ended, naming the section \"Design\" and versions 1 and 2, and not \"Summary\"",
		queued.ID, reviewer.name())
	r.holdPhase(tree.root().key, phase.Reviewing)
	r.held("%s stays in reviewing after the task (%s)", tree.root().key, liveSettle)
	status, failure := reviewer.backward(phase.Implementing, "version 2 of the spec changed the Design section this review read")
	if status != http.StatusOK {
		r.fail("%s's request_backward_move to implementing = %d %s: %s, want 200", reviewer.name(), status, failure.Code, failure.Error)
	}
	r.awaitPhase(tree.root().key, phase.Implementing)
	r.held("%s's request_backward_move moved %s to implementing", reviewer.name(), tree.root().key)
	r.quiet(others, mark, task.at.Add(liveSettle))
	r.held("%s, whose gate did not move, were sent nothing in the window the reviewer's task arrived in", versionNewsNames(others))
}

// Step A1. One approval request names both documents of a tree, the architect registers the
// Verification document as criteria and then the spec, and one approval of both opens both rows,
// which releases the root into planning; a new version of the Verification document then closes
// the criteria row, the spec row standing approved.
func (r *versionNewsLive) a1() {
	tree := r.trees["a1"]
	key := tree.root().key
	architect := r.claimant(tree.root(), claim.RoleArchitect)
	ask := r.requestApproval(architect, tree)
	named := map[string]int{}
	for _, document := range ask.Approval.Documents {
		named[document.ArtifactID] = document.Version
	}
	if len(named) != 2 || named[tree.spec] != 1 || named[tree.verification] != 1 {
		r.fail("the approval request (ask %s) names %v, want the spec %s and the Verification document %s, each at version 1", ask.ID, named, tree.spec, tree.verification)
	}
	r.held("one approval request (ask %s) names the spec and the Verification document, each at version 1", ask.ID)
	r.register(architect, record.DesignStepCriteria, tree.verification, 1)
	r.register(architect, record.DesignStepSpec, tree.spec, 1)
	r.awaitSteps(tree, "both rows of "+key+" registered and waiting", func(views map[record.DesignStep]api.GatedStepView) bool {
		return len(views) == 2 && views[record.DesignStepCriteria].Status == api.GatedStepWaiting && views[record.DesignStepSpec].Status == api.GatedStepWaiting
	})
	r.held("register_gate took criteria and then spec under root-issues, and both rows wait on the person")
	r.approve(tree.spec, 1)
	views := r.awaitSteps(tree, "both rows of "+key+" approved", func(views map[record.DesignStep]api.GatedStepView) bool {
		return views[record.DesignStepCriteria].Status == api.GatedStepApproved && views[record.DesignStepSpec].Status == api.GatedStepApproved
	})
	r.held("one approval opened both rows: %s", versionNewsSteps(views))
	r.awaitPhase(key, phase.Planning)
	r.held("the open gate released %s into planning", key)
	r.newVersion(tree, record.DesignStepCriteria, versionNewsCheck, versionNewsCheckAgain, 2)
	views = r.gatedSteps(key)
	criteria, spec := views[record.DesignStepCriteria], views[record.DesignStepSpec]
	if criteria.ApprovedVersion == nil || *criteria.ApprovedVersion != 1 || spec.Status != api.GatedStepApproved {
		r.fail("after version 2 of the Verification document the rows are %s, want criteria waiting at version 2 with version 1 approved, and spec approved", versionNewsSteps(views))
	}
	r.held("version 2 of the Verification document closed the criteria row, the spec row standing approved: %s", versionNewsSteps(views))
}

// Step A3. Two trees register both documents. An answer to the open decision block in the
// implementing root's spec, the whole of its next version, which Dispatch approves by carrying the
// person's approval to it, sends the implementer and the tree's architect nothing; in the same
// window a checks-only version of the accepting root's Verification document, approved, reaches
// the root architect, which holds that root, and the root stays in accepting. A checks-only version
// of the implementing root's Verification document then reaches its implementer.
func (r *versionNewsLive) a3() {
	accepting, implementing := r.trees["a3-accepting"], r.trees["a3-implementing"]
	owner := r.claimant(accepting.root(), claim.RoleArchitect)
	implementer := r.claimant(implementing.root(), claim.RoleImplementer)
	silent := []*versionNewsClaimant{implementer, r.claimant(implementing.root(), claim.RoleArchitect)}
	r.openTree(accepting)
	r.openTree(implementing)

	key := implementing.root().key
	var asks []struct {
		ID      string  `json:"id"`
		BlockID *string `json:"block_id"`
	}
	r.dispatchAs(versionNewsPerson, http.MethodGet, "/api/v1/issues/"+key+"/asks?state=all", nil, &asks)
	wave := ""
	for _, ask := range asks {
		if ask.BlockID != nil && *ask.BlockID == "wave" {
			wave = ask.ID
		}
	}
	if wave == "" {
		r.fail("Dispatch lists no ask of the decision block wave on %s: %+v", key, asks)
	}
	mark := time.Now()
	r.dispatchAs(versionNewsPerson, http.MethodPost, "/api/v1/asks/"+wave+"/answer", map[string]any{"selected": []string{"Two"}, "expected_edited_at": nil}, nil)
	r.note("POST /api/v1/asks/%s/answer as %s: Two", wave, versionNewsPerson)
	r.awaitSteps(implementing, "the answer's version of "+key+"'s spec approved in the daemon's gate", func(views map[record.DesignStep]api.GatedStepView) bool {
		view := views[record.DesignStepSpec]
		return view.CurrentVersion == 2 && view.Status == api.GatedStepApproved
	})
	r.held("the answer to the decision block wrote version 2 of %s's spec, which Dispatch approved by carrying the person's approval, and the gate stands open at it", key)

	r.newVersion(accepting, record.DesignStepCriteria, versionNewsCheck, versionNewsCheckAgain, 2)
	r.approve(accepting.verification, 2)
	task := owner.awaitPrompt("the news of the accepting root's checks", mark)
	r.note("%s was prompted:\n%s", owner.name(), task.message)
	if !versionNewsNamesCheck(task.message, accepting.check) || !strings.Contains(task.message, accepting.root().key) ||
		!strings.Contains(task.message, "which you hold in accepting") || !versionNewsNamesVersion(task.message, 2) || !versionNewsNamesVersion(task.message, 1) {
		r.fail("%s's task names %s, want the changed check %s of %s, which it holds in accepting, and versions 2 and 1: %q",
			owner.name(), versionNewsFirstLine(task.message), accepting.check, accepting.root().key, task.message)
	}
	r.held("a checks-only approval reached the root architect, at entry %d of its phases row and given that entry, which holds %s in accepting, "+
		"naming the changed check %s and versions 1 and 2", r.standingEntry(owner), accepting.root().key, accepting.check)
	r.holdPhase(accepting.root().key, phase.Accepting)
	r.held("%s stays in accepting (%s)", accepting.root().key, liveSettle)
	r.quiet(silent, mark, task.at.Add(liveSettle))
	r.held("the decision-only version sent %s nothing, in the window the accepting root's task arrived in", versionNewsNames(silent))

	mark = time.Now()
	r.newVersion(implementing, record.DesignStepCriteria, versionNewsCheck, versionNewsCheckAgain, 2)
	r.approve(implementing.verification, 2)
	task = implementer.awaitPrompt("the news of the implementing root's checks", mark)
	r.note("%s was prompted:\n%s", implementer.name(), task.message)
	if !versionNewsNamesCheck(task.message, implementing.check) || strings.Contains(task.message, `"Design"`) ||
		!versionNewsNamesVersion(task.message, 2) || !versionNewsNamesVersion(task.message, 1) {
		r.fail("%s's task names %s, want the changed check %s and versions 2 and 1, and no spec section: %q",
			implementer.name(), versionNewsFirstLine(task.message), implementing.check, task.message)
	}
	r.held("a checks-only approval reached %s, naming the changed check %s and versions 1 and 2", implementer.name(), implementing.check)
}

// Step A5. The tester is in a turn when a checks-only version of its root's Verification document
// is approved; the task that carries the news waits behind that turn. In that same turn the tester
// completes with a pass, and its turn ends. The tester is sent nothing, since its phase is over,
// and the reviewer the pass starts is prompted with a start task naming the changed check and the
// two versions. The same version changes nothing the root's child holds, since a root's checks are
// the root's alone, so the reviewer the child's tester's pass starts is prompted with a start task
// that names no check and no change at all: the same read that found the root reviewer's change.
func (r *versionNewsLive) a5() {
	tree := r.trees["a5"]
	key := tree.root().key
	tester := r.claimant(tree.root(), claim.RoleTester)
	r.openTree(tree)
	tester.startTurn()
	r.held("%s is in a turn", tester.name())
	mark := time.Now()
	r.newVersion(tree, record.DesignStepCriteria, versionNewsCheck, versionNewsCheckAgain, 2)
	r.approve(tree.verification, 2)
	queued := r.awaitQueued(tester, mark)
	r.held("the news of version 2 is queued behind %s's open turn (delivery %s)", tester.name(), queued.ID)
	id, version := r.writeHandoff(key, "test", tester.session)
	before := r.rt.Calls()
	status, failure := tester.complete(api.HandoffCompleteRequest{Summary: "every check passes", Verdict: "pass", HandoffID: id, HandoffVersion: version})
	if status != http.StatusOK {
		r.fail("%s's completion with a pass = %d %s: %s, want 200", tester.name(), status, failure.Code, failure.Error)
	}
	tester.endTurn()
	r.awaitPhase(key, phase.Reviewing)
	r.held("%s completed with a pass in that turn, and %s moved to reviewing", tester.name(), key)
	reviewer := r.connect(tree.root(), claim.RoleReviewer, before)
	task := reviewer.awaitPrompt("the reviewer's start task", mark)
	r.note("%s was prompted:\n%s", reviewer.name(), task.message)
	if !versionNewsNamesCheck(task.message, tree.check) || !versionNewsNamesVersion(task.message, 2) || !versionNewsNamesVersion(task.message, 1) {
		r.fail("%s's start task names %s, want the changed check %s and versions 2 and 1: %q", reviewer.name(), versionNewsFirstLine(task.message), tree.check, task.message)
	}
	r.held("%s's start task names the changed check %s and versions 1 and 2", reviewer.name(), tree.check)
	r.quiet([]*versionNewsClaimant{tester}, mark, task.at.Add(liveSettle))
	r.held("%s, whose phase ended before its task could go, was sent nothing in the window the reviewer's task arrived in", tester.name())

	child := tree.members[1]
	childTester := r.claimant(child, claim.RoleTester)
	id, version = r.writeHandoff(child.key, "test", childTester.session)
	before = r.rt.Calls()
	if status, failure := childTester.complete(api.HandoffCompleteRequest{Summary: "every check passes", Verdict: "pass", HandoffID: id, HandoffVersion: version}); status != http.StatusOK {
		r.fail("%s's completion with a pass = %d %s: %s, want 200", childTester.name(), status, failure.Code, failure.Error)
	}
	r.awaitPhase(child.key, phase.Reviewing)
	r.held("%s completed with a pass after the approval, and %s moved to reviewing", childTester.name(), child.key)
	childReviewer := r.connect(child, claim.RoleReviewer, before)
	start := childReviewer.awaitPrompt("the child reviewer's start task", mark)
	r.note("%s was prompted:\n%s", childReviewer.name(), start.message)
	// A change list shows as a "Checks:" line or as the news's own opening words. A bare check id
	// is not read: it can stand inside a UUID the task prints.
	names := func(message string) bool {
		return strings.Contains(message, "Checks:") || strings.Contains(message, "The approved design of")
	}
	if names(start.message) {
		r.fail("%s's start task names a change; want none, since version 2 changed nothing %s holds: %q", childReviewer.name(), child.key, start.message)
	}
	r.held("%s's start task names no check and no change, since version 2 changed nothing %s holds, where the same read finds %s in %s's",
		childReviewer.name(), child.key, tree.check, reviewer.name())
}

// writeHandoff records the handoff of key's phase word under session on Dispatch's own route, as
// `dispatch handoff-write` does, and returns the record's id and version.
func (r *versionNewsLive) writeHandoff(key, word, session string) (string, int) {
	r.t.Helper()
	r.dispatchAgent(http.MethodPost, "/api/v1/issues/"+key+"/handoffs/"+word, map[string]any{
		"actor": map[string]string{"kind": "session", "id": session},
		"body":  fmt.Sprintf("{\"schemaVersion\": 1, \"phase\": %q, \"issue\": %q, \"summary\": \"every check passes\"}\n", word, key),
	}, http.StatusCreated, nil)
	var latest struct {
		ID      string `json:"id"`
		Version int    `json:"version"`
	}
	r.dispatchAgent(http.MethodGet, "/api/v1/issues/"+key+"/handoffs/"+word+"/latest", nil, http.StatusOK, &latest)
	if latest.ID == "" || latest.Version < 1 {
		r.fail("the %s handoff of %s reads back as %+v", word, key, latest)
	}
	r.note("POST /api/v1/issues/%s/handoffs/%s as session %s: version %d (id %s)", key, word, session, latest.Version, latest.ID)
	return latest.ID, latest.Version
}

// Step A6. A root in merging whose pull request GitHub reports merged moves to verifying, where its
// close waits for its child still in reviewing. It is sent nothing for a spec change the person
// approves, and its merger's request_backward_move is refused, since no role runs verifying and a
// merged change never moves back; the unmerged child's reviewer is sent the task, which is the
// delivery the root's window sees.
func (r *versionNewsLive) a6() {
	tree := r.trees["a6"]
	root, child := tree.root(), tree.members[1]
	merger := r.claimant(root, claim.RoleMerger)
	reviewer := r.claimant(child, claim.RoleReviewer)
	r.openTree(tree)
	r.publishMerged(root)
	r.await("the merge of "+root.key+"'s pull request recorded", versionNewsWait, func() (bool, string) {
		issue := r.stateIssue(root.key)
		if issue.PullRequest == nil {
			return false, "no pull request in the state"
		}
		return issue.PullRequest.State == string(record.PullRequestMerged), "pull request " + issue.PullRequest.State + " in phase " + string(issue.Phase)
	})
	r.holdPhase(root.key, phase.Verifying)
	r.held("GitHub's merge of #%d is recorded and %s waits in verifying for %s", root.pr, root.key, child.key)
	mark := time.Now()
	r.newVersion(tree, record.DesignStepSpec, versionNewsDesign, versionNewsDesignAgain, 2)
	r.approve(tree.spec, 2)
	task := reviewer.awaitPrompt("the news of the spec", mark)
	r.note("%s was prompted:\n%s", reviewer.name(), task.message)
	if !strings.Contains(task.message, `"Design"`) || !versionNewsNamesVersion(task.message, 2) || !versionNewsNamesVersion(task.message, 1) {
		r.fail("%s's task names %s, want the section \"Design\" and versions 2 and 1: %q", reviewer.name(), versionNewsFirstLine(task.message), task.message)
	}
	r.held("%s, unmerged in reviewing, was sent the task naming \"Design\" and versions 1 and 2", reviewer.name())
	r.quiet([]*versionNewsClaimant{merger}, mark, task.at.Add(liveSettle))
	r.held("%s, on a merged pull request, was sent nothing in the window %s's task arrived in", merger.name(), child.key)
	status, failure := merger.backward(phase.Implementing, "version 2 of the spec changed the Design section")
	if status == http.StatusOK || failure.Code != "BACKWARD_REFUSED" {
		r.fail("%s's request_backward_move to implementing = %d %s: %s, want BACKWARD_REFUSED: no role runs verifying, and a merged change never moves back", merger.name(), status, failure.Code, failure.Error)
	}
	r.note("POST /legion/v1/phase/backward as %s: %d %s: %s", merger.name(), status, failure.Code, failure.Error)
	r.holdPhase(root.key, phase.Verifying)
	r.held("%s's request_backward_move was refused %s, and %s stays in verifying", merger.name(), failure.Code, root.key)
}

// publishMerged publishes GitHub's close of member's pull request as merged, as Envoy relays the
// webhook (intake's decodePullRequest reads it), and waits for the daemon to take it.
func (r *versionNewsLive) publishMerged(member *versionNewsIssue) {
	r.t.Helper()
	payload, err := json.Marshal(map[string]string{
		"action": "closed", "kind": "pr", "repo": r.repo, "number": strconv.Itoa(member.pr), "merged": "true",
		"merge_commit_sha": "merge-" + strings.ToLower(member.key), "head_sha": versionNewsHead(member.key), "base_ref": "main",
		"head_ref": "legion/" + member.key, "title": "Approved version news " + member.key,
		"url": "https://github.com/" + r.repo + "/pull/" + strconv.Itoa(member.pr), "updated_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		r.t.Fatal(err)
	}
	subject := fmt.Sprintf("notifications.github.%s.pr.%d", strings.ReplaceAll(r.repo, "/", "."), member.pr)
	eventID := r.publish(subject, "github", fmt.Sprintf("pr-%d-merged", member.pr), string(payload))
	r.await("the daemon's processed_events row of GitHub event "+eventID, versionNewsWait, func() (bool, string) {
		return r.processed("github", eventID), "not processed"
	})
	r.note("published %s: pull request #%d of %s closed merged, taken as %s", subject, member.pr, member.key, eventID)
}

// processed says whether the daemon applied source's event eventID, which the intake records in
// the transaction that applies it. No route shows it.
func (r *versionNewsLive) processed(source, eventID string) bool {
	r.t.Helper()
	var taken bool
	if err := r.st.Pool().QueryRow(context.Background(),
		"select exists(select 1 from processed_events where source = $1 and event_id = $2)", source, eventID).Scan(&taken); err != nil {
		r.fail("read the processed event %s: %v", eventID, err)
	}
	return taken
}

// Step A7. A checks-only version closes one tree's gate while its retro runs, and the implementer
// of that tree and of a second, whose gate stands open, each complete retro at once. The open
// tree's merger is launched, and in that same window the closed tree's merger is not. The person's
// approval then releases it: the merger is launched with a start task that holds the start's own
// text, a blank line, and then the news, which opens with the gate's opening and names what
// changed.
func (r *versionNewsLive) a7() {
	closed, open := r.trees["a7"], r.trees["a7-open"]
	implementers := []*versionNewsClaimant{r.claimant(closed.root(), claim.RoleImplementer), r.claimant(open.root(), claim.RoleImplementer)}
	r.openTree(closed)
	r.openTree(open)
	r.newVersion(closed, record.DesignStepCriteria, versionNewsCheck, versionNewsCheckAgain, 2)
	r.held("version 2 of %s's Verification document closed its gate", closed.root().key)
	held, released := r.token(closed.root().key, claim.RoleMerger), r.token(open.root().key, claim.RoleMerger)
	before := r.rt.Calls()
	mark := time.Now()
	// The completion that ends a retro rates the plan the issue ran (api.PlanRatingContract, main's
	// #4488), so each one carries a rating and its reason as a production retro's does.
	rating, reason := 4, "The plan fit the work: its steps were the steps the issue ran."
	for _, implementer := range implementers {
		if status, failure := implementer.complete(api.HandoffCompleteRequest{Summary: "the retro is written", Commit: versionNewsHead(implementer.issue),
			PlanRating: &rating, PlanReason: &reason}); status != http.StatusOK {
			r.fail("%s's completion of retro = %d %s: %s, want 200", implementer.name(), status, failure.Code, failure.Error)
		}
	}
	r.awaitPhase(closed.root().key, phase.Merging)
	r.awaitPhase(open.root().key, phase.Merging)
	r.held("both retros completed, and %s and %s are in merging", closed.root().key, open.root().key)
	var launchedAt time.Time
	r.await("the launch of "+string(released)+", whose gate is open", versionNewsWait, func() (bool, string) {
		count := r.launches(released, r.rt.Calls())
		launchedAt = time.Now()
		return count > r.launches(released, before), fmt.Sprintf("%d launches", count)
	})
	r.held("the merger of %s, whose gate is open, was launched", open.root().key)
	time.Sleep(time.Until(launchedAt.Add(liveSettle)))
	if count := r.launches(held, r.rt.Calls()); count != r.launches(held, before) {
		r.fail("the merger of %s was launched while its gate was closed", closed.root().key)
	}
	r.held("in the window %s's merger was launched in, the merger of %s, its gate closed, was not", open.root().key, closed.root().key)
	r.approve(closed.verification, 2)
	merger := r.connect(closed.root(), claim.RoleMerger, before)
	task := merger.awaitPrompt("the merger's start task", mark)
	r.note("%s was prompted:\n%s", merger.name(), task.message)
	opening := "The design gate of " + closed.root().key + " opened"
	if strings.Index(task.message, "\n\n"+opening) <= 0 || !versionNewsNamesCheck(task.message, closed.check) || !versionNewsNamesVersion(task.message, 2) || !versionNewsNamesVersion(task.message, 1) {
		r.fail("%s's start task names %s, want the start's own text, a blank line, then %q and the changed check %s with versions 2 and 1: %q", merger.name(), versionNewsFirstLine(task.message), opening, closed.check, task.message)
	}
	r.held("the approval launched %s, whose start task holds its own text, a blank line, then %q, and names the changed check %s and versions 1 and 2", merger.name(), opening, closed.check)
}

// Step R6. In a tree with children, an approved checks-only version of the root's Verification
// document reaches the worker holding the root's phase and the root's architect, whose summary
// names it, each at the entry the daemon's start path gave it, and neither child's worker is sent a
// task, so none names the changed check, in the window the root's task arrived in.
func (r *versionNewsLive) r6() {
	tree := r.trees["r6"]
	holder, architect := r.claimant(tree.root(), claim.RoleImplementer), r.claimant(tree.root(), claim.RoleArchitect)
	children := []*versionNewsClaimant{r.claimant(tree.members[1], claim.RoleImplementer), r.claimant(tree.members[2], claim.RoleTester)}
	r.openTree(tree)
	mark := time.Now()
	r.newVersion(tree, record.DesignStepCriteria, versionNewsCheck, versionNewsCheckAgain, 2)
	r.approve(tree.verification, 2)
	task := holder.awaitPrompt("the news of the root's checks", mark)
	r.note("%s was prompted:\n%s", holder.name(), task.message)
	if !versionNewsNamesCheck(task.message, tree.check) || !versionNewsNamesVersion(task.message, 2) || !versionNewsNamesVersion(task.message, 1) {
		r.fail("%s's task names %s, want the changed check %s and versions 2 and 1: %q", holder.name(), versionNewsFirstLine(task.message), tree.check, task.message)
	}
	r.held("the root's checks-only approval reached %s, at entry %d of its phases row and given that entry, naming the changed check %s and versions 1 and 2",
		holder.name(), r.standingEntry(holder), tree.check)
	summary := architect.awaitPrompt("the root architect's summary of the root's checks", mark)
	r.note("%s was prompted:\n%s", architect.name(), summary.message)
	if !versionNewsNamesCheck(summary.message, tree.check) || !versionNewsNamesVersion(summary.message, 2) || !versionNewsNamesVersion(summary.message, 1) {
		r.fail("%s's summary names %s, want the changed check %s and versions 2 and 1: %q", architect.name(), versionNewsFirstLine(summary.message), tree.check, summary.message)
	}
	r.held("the same approval reached %s, at entry %d of its phases row and given that entry, with a summary naming the changed check %s and versions 1 and 2",
		architect.name(), r.standingEntry(architect), tree.check)
	r.quiet(children, mark, task.at.Add(liveSettle))
	r.held("%s were sent no task, so none naming %s, in the window the root's task arrived in", versionNewsNames(children), tree.check)
}

// Step off. A second daemon, on a configuration and a database of its own, gates nothing, so the
// architect's registration opens each gate at once. A new version of one tree's spec, which the
// person approves and the daemon takes in, sends no holder of that tree anything, in the window in
// which the other tree's tester passes and the reviewer that pass starts is sent its task.
func (r *versionNewsLive) off() {
	tree, control := r.trees["off"], r.trees["off-control"]
	silent := []*versionNewsClaimant{
		r.claimant(tree.root(), claim.RoleArchitect), r.claimant(tree.root(), claim.RoleImplementer), r.claimant(tree.members[1], claim.RoleReviewer),
	}
	tester := r.claimant(control.root(), claim.RoleTester)
	for _, each := range []*versionNewsTree{tree, control} {
		r.register(r.claimant(each.root(), claim.RoleArchitect), record.DesignStepSpec, each.spec, 1)
		key := each.root().key
		r.await("the gate of "+key+" approved at its registration", versionNewsWait, func() (bool, string) {
			gate := r.stateIssue(key).DesignGate
			if gate == nil {
				return false, "no designGate"
			}
			return gate.ApprovedVersion != nil && *gate.ApprovedVersion == 1, fmt.Sprintf("designGate %+v", *gate)
		})
		r.held("under gates.design off the architect's registration opened the gate of %s at version 1, with no person asked", key)
	}
	key := tree.root().key
	var listed struct {
		LastSeq int64 `json:"last_seq"`
	}
	r.dispatchAgent(http.MethodGet, "/api/v1/issues/"+key, nil, http.StatusOK, &listed)
	mark := time.Now()
	r.newVersion(tree, record.DesignStepSpec, versionNewsDesign, versionNewsDesignAgain, 2)
	r.approve(tree.spec, 2)
	eventID := r.approvalEvent(key, listed.LastSeq, tree.spec, 2)
	r.await("the daemon's processed_events row of Dispatch event "+eventID, versionNewsWait, func() (bool, string) {
		return r.processed("dispatch", eventID), "not processed"
	})
	r.held("the daemon took in the person's approval of version 2 of %s's spec (%s)", key, eventID)
	id, version := r.writeHandoff(control.root().key, "test", tester.session)
	before := r.rt.Calls()
	if status, failure := tester.complete(api.HandoffCompleteRequest{Summary: "every check passes", Verdict: "pass", HandoffID: id, HandoffVersion: version}); status != http.StatusOK {
		r.fail("%s's completion with a pass = %d %s: %s, want 200", tester.name(), status, failure.Code, failure.Error)
	}
	reviewer := r.connect(control.root(), claim.RoleReviewer, before)
	task := reviewer.awaitPrompt("the control reviewer's start task", mark)
	r.held("in the same window %s's pass started %s, which was sent its task", tester.name(), reviewer.name())
	r.quiet(silent, mark, task.at.Add(liveSettle))
	r.held("under gates.design off the approved version sent %s nothing, in the window %s's task arrived in", versionNewsNames(silent), reviewer.name())
}

// approvalEvent is the id of the artifact.approved event of version of artifact in key's Dispatch
// event log after sequence after, as the daemon's processed_events names a Dispatch event
// (dispatch-<id>).
func (r *versionNewsLive) approvalEvent(key string, after int64, artifact string, version int) string {
	r.t.Helper()
	var found string
	r.await(fmt.Sprintf("the artifact.approved event of version %d of %s on %s", version, artifact, key), versionNewsWait, func() (bool, string) {
		var events []struct {
			ID      int64  `json:"id"`
			Type    string `json:"type"`
			Payload struct {
				ArtifactID string `json:"artifact_id"`
				Version    any    `json:"version"`
			} `json:"payload"`
		}
		r.dispatchAgent(http.MethodGet, fmt.Sprintf("/api/v1/issues/%s/events?after=%d&limit=200", key, after), nil, http.StatusOK, &events)
		types := make([]string, len(events))
		for i, event := range events {
			types[i] = event.Type
			if number, ok := event.Payload.Version.(float64); ok && event.Type == "artifact.approved" && event.Payload.ArtifactID == artifact && int(number) == version {
				found = fmt.Sprintf("dispatch-%d", event.ID)
				return true, ""
			}
		}
		return false, fmt.Sprintf("events %v", types)
	})
	return found
}
