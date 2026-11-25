//go:build e2e

package daemon

// approved_version_news_steps_live_test.go holds the bodies of part two's three steps
// (sessionsBodies) and the tasks and helpers each step alone uses: the driver that runs them and
// its reads of the daemon and of Dispatch are approved_version_news_sessions_live_test.go, its
// services, seed and boot are approved_version_news_sessions_setup_live_test.go, and the step
// table they run from, with its picks, is approved_version_news_transcript_test.go.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// sessionsBodies is the body of each step of the step table (sessionsSteps), by the step's name.
// The table is untagged, so that `go test` runs the test of the picks it makes; the bodies drive
// real sessions and stay under the e2e tag.
var sessionsBodies = map[string]func(*sessionsRun) (sessionsOutcome, string){
	sessionsA4:      (*sessionsRun).a4,
	sessionsOff:     (*sessionsRun).off,
	sessionsAnswers: (*sessionsRun).answers,
}

// architectTask is the task the driver gives an architect: one edit of one document, the tree as
// its catch-up would show it, and nothing about who to tell.
func architectTask(document, issue, find, with, tree string) string {
	return fmt.Sprintf("A task from the operator of this tree. Write a new version, version 2, of the %s of %s. "+
		"In it, replace the words `%s` with `%s`. Make that one edit with `dispatch doc-edit`, and change nothing else "+
		"in that document. Your tree stands as follows: %s.", document, issue, find, with, tree)
}

// Step A4. A root waits in admitted while its child stands in reviewing under a live reviewer and
// a live architect of its own, which the child's first phase start started, as it starts every
// child's (workflow.Engine.StartFirstPhase); the root's gate stands open at version 1 of its spec,
// and the root's architect is live above the child. The child's architect is asked to write
// version 2 of its own issue's Verification document. A child's documents are its architect's to
// change and no person approves them, so the daemon sends no news of the version: the reviewer
// hears of it only from the architect that wrote it. Both arms' prompts tell that architect to
// tell the architect above it of each version it writes, and tell that parent to review each
// version, post its verdict as a `Spec review of` comment on the child and tell the child's
// architect with envoy_publish; the head's tell the child's architect to tell the holder of each
// issue the version changes once the version has that approving review.
//
// The parent's review is the proof's (reviewAsParent), in the parent's form and through the
// parent's own channels, once the child's architect has told the parent of the version. A live
// parent that reviewed the fixture's version in a run of this step asked for changes to what the
// version had not changed, the child's empty spec and its check's run and negative control, so a
// live review measures the fixture rather than the telling R4 is about; and the version must have
// its approving review before the head's prompt puts that telling to the child's architect at
// all. The root's architect is live all the same, as every running tree's is, and its start task
// tells it that the operator reviews the child's versions in its place (parentStart), so the
// version gets one review. A run whose version the child's architect did not write, or did not
// tell its parent of, stopped before the assertion. The assertion is the child's architect's own
// envoy_publish to the reviewer's role topic naming version 2 and the changed check, since a card
// in the reviewer's transcript does not say who sent it or how; the reviewer's transcript holding
// a message that names both; and no approval request on the child.
func (r *sessionsRun) a4() (sessionsOutcome, string) {
	root := &sessionsMember{phase: phase.Admitted, roles: []claim.Role{claim.RoleArchitect}, tasks: map[claim.Role]string{}}
	child := &sessionsMember{phase: phase.Reviewing, roles: []claim.Role{claim.RoleArchitect, claim.RoleReviewer}, check: sessionsCheckID,
		tasks: map[claim.Role]string{}}
	r.tree(sessionsSpec("A reader that pages.", sessionsDesign, sessionsLater), root, child)
	root.tasks[claim.RoleArchitect] = parentStart(root.key, child.key)
	child.tasks[claim.RoleReviewer] = holderStart(child.key, phase.Reviewing)
	r.boot()
	r.checkArm(child, claim.RoleArchitect, sessionsArchitectMark)
	r.checkArm(root, claim.RoleArchitect, sessionsArchitectMark)
	for _, held := range []struct {
		member *sessionsMember
		role   claim.Role
	}{{root, claim.RoleArchitect}, {child, claim.RoleReviewer}} {
		if !r.settled(held.member, held.role) {
			return sessionsSetup, fmt.Sprintf("the %s of %s did not end its start turn within the run's bound", held.role, held.member.key)
		}
	}
	r.deliver(child, claim.RoleArchitect, architectTask(
		fmt.Sprintf("Verification document (`verification.md`, document %s)", child.verification), child.key, sessionsCheck, sessionsCheckAgain,
		fmt.Sprintf("%s, the root, in admitted, waiting for its child; %s, its child and your own issue, in reviewing, where its reviewer holds it",
			root.key, child.key)))
	change := sessionsChecksChange
	reviewer := notify.RoleTopicPrefix + string(r.token(child, claim.RoleReviewer))
	parentTopic := notify.RoleTopicPrefix + string(r.token(root, claim.RoleArchitect))
	told, review := "", ""
	var architectQuiet, parentQuiet time.Time
	r.until(r.deadline, func() bool {
		architect := r.transcript(child, claim.RoleArchitect)
		if _, toldParent := architect.publishedTo(parentTopic, "verification"); review == "" && toldParent {
			if written, text := r.version2(child.verification); sessionsUnwritten("", written, text, sessionsCheckAgain) == "" {
				review = r.reviewAsParent(child, root)
				// The review starts the child's architect's next turn, which no quiet read
				// before it can have seen.
				architectQuiet, parentQuiet = time.Time{}, time.Time{}
				return false
			}
		}
		told = r.transcript(child, claim.RoleReviewer).toldOf(change...)
		if _, published := architect.publishedTo(reviewer, change...); published && told != "" {
			return true
		}
		// The route stopped wherever it stopped once both architects are out of a turn at once,
		// each holding no task.
		architectDone, parentDone := r.quiet(child, claim.RoleArchitect, &architectQuiet), r.quiet(root, claim.RoleArchitect, &parentQuiet)
		return architectDone && parentDone
	})
	if told == "" {
		// The architects settled: a message one of them sent may still be on its way.
		r.until(time.Now().Add(sessionsGrace), func() bool {
			told = r.transcript(child, claim.RoleReviewer).toldOf(change...)
			return told != ""
		})
	}
	// The child's architect may still be in its turn: a request for approval would come in it.
	if !r.settled(child, claim.RoleArchitect) {
		r.evidence("the architect of %s was still in a turn or held a task at the run's bound, so what follows is what it did by then", child.key)
	}
	architect, parent := r.transcript(child, claim.RoleArchitect), r.transcript(root, claim.RoleArchitect)
	r.printCalls(child, claim.RoleArchitect, architect.bash("dispatch "), "ran")
	r.printCalls(child, claim.RoleArchitect, architect.calling("envoy_publish"), "called")
	r.printCalls(root, claim.RoleArchitect, parent.bash("dispatch "), "ran")
	r.printCalls(root, claim.RoleArchitect, parent.calling("envoy_publish"), "called")
	written, text := r.version2(child.verification)
	stopped := sessionsUnwritten(child.key+"'s Verification document", written, text, sessionsCheckAgain)
	switch {
	case stopped != "":
	case review == "":
		stopped = fmt.Sprintf("the architect of %s made no envoy_publish to %s, its parent's role topic, naming version 2 of its Verification document, "+
			"so its parent never heard of the version to review it, and the run did not reach its assertion", child.key, parentTopic)
	default:
		r.evidence("version 2 of %s's Verification document %s holds %q", child.key, child.verification, sessionsCheckAgain)
	}
	var why []string
	if told == "" {
		why = append(why, fmt.Sprintf("the reviewer of %s was sent no Envoy message naming version 2 and the change", child.key))
	} else {
		r.evidence("the reviewer of %s's transcript holds the Envoy message %s", child.key, sessionsExcerpt(told))
	}
	if missed := r.publishedBy(architect, "the architect of "+child.key, child, claim.RoleReviewer, change); missed != "" {
		why = append(why, missed)
	}
	if asks := r.approvalAsks(child.key); len(asks) > 0 {
		why = append(why, fmt.Sprintf("Dispatch holds approval requests %v on %s", asks, child.key))
	} else {
		r.evidence("Dispatch holds no approval request on %s", child.key)
	}
	return sessionsJudged(stopped, why)
}

// parentStart is the start task the root architect of the a4 step holds at the boot, at the entry
// the daemon's start path raised for it, in place of that start's own task: that the proof runs
// no repository, where its tree stands, and that the operator reviews each version of child's
// documents in its place, posting the review as its `Spec review of` comment and telling child's
// architect in its name (reviewAsParent). So it reviews none of them itself, and each version gets
// one review. It says nothing of who tells the child's reviewer.
func parentStart(root, child string) string {
	return fmt.Sprintf("This proof runs no GitHub and no repository, and you have no GitHub access, so run no `gh`, `git` or `jj`. You hold the "+
		"tree of %s, and its work stands where your earlier session left it: %s, the root, in admitted, waiting for its child; %s, its child, "+
		"in reviewing, where its reviewer and its own architect hold it. In this proof the operator of this tree reviews each version of "+
		"%s's spec and Verification document in your place: it posts that review on %s as your `Spec review of` comment and tells %s's "+
		"architect of it in your name, so review none of them yourself. Start no work now: reply with one line that starts with WAITING, "+
		"and end your turn.", root, root, child, child, child, child)
}

// reviewAsParent is the a4 step's parent's review of version 2 of child's Verification document,
// which the proof makes in the place of root's architect once the child's architect has told that
// parent of the version. It is what a live parent posts, through the live parent's own channels,
// as the parent's prompt tells it to (skill://legion-architect/references/decomposition.md,
// "Reviewing a child's spec"): a `dispatch comment` on the child as the parent's own session,
// whose first two lines are those a live root architect wrote when it approved this version in a
// run of this step, `Spec review of <child> v1 by <root>: approved` and `Verification document
// v2`, with its reason after them; then an Envoy message from the parent's session to the role
// topic of the child's architect, in the words a live parent published its verdict in, `<root>
// review of <child>: <verdict>, spec v1 and Verification document v2`, naming the comment. The
// comment is read back as an approval (sessionsApprovedReview), and a review that does not read as
// one, or a message the listener refuses, fails the run's harness. It answers the comment's body.
func (r *sessionsRun) reviewAsParent(child, root *sessionsMember) string {
	t := r.t
	t.Helper()
	session := r.claim(r.token(root, claim.RoleArchitect)).Session
	if session == "" {
		t.Fatalf("the architect of %s holds no session to post its review of %s as", root.key, child.key)
	}
	body := fmt.Sprintf("Spec review of %s v1 by %s: approved\nVerification document v2\n\n"+
		"I read version 2 of the Verification document against the root spec at its approved version 1. It changes one check, %s, from %q "+
		"to %q. The check still serves the root's design, %q, narrows nothing of it and adds no scope.\n",
		child.key, root.key, sessionsCheckID, sessionsCheck, sessionsCheckAgain, sessionsDesign)
	var posted struct {
		ID string `json:"id"`
	}
	r.dispatchAgent(http.MethodPost, "/api/v1/issues/"+child.key+"/comments", map[string]any{
		"body": body, "actor": map[string]string{"kind": "session", "id": session},
	}, 0, &posted)
	if posted.ID == "" || sessionsApprovedReview(child.key, root.key, r.comments(child.key, "")) == "" {
		t.Fatalf("the review the proof posted on %s as %s's architect (comment %q) does not read back as an approval of version 2", child.key, root.key, posted.ID)
	}
	topic := notify.RoleTopicPrefix + string(r.token(child, claim.RoleArchitect))
	message := fmt.Sprintf("%s review of %s: approved, spec v1 and Verification document v2: dispatch://%s/comment/%s",
		root.key, child.key, child.key, posted.ID)
	encoded, err := json.Marshal(map[string]string{
		"source": "agent", "source_session": session, "topic": topic, "message": message, "idempotency_key": "a4-review-" + randomSuffix(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := r.s.client.Post(r.s.listener+"/v1/messages/publish", "application/json", bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("publish the review of %s to %s as %s's architect: %v", child.key, topic, root.key, err)
	}
	answer, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode/100 != 2 {
		t.Fatalf("publish the review of %s to %s as %s's architect = %d %s", child.key, topic, root.key, response.StatusCode, answer)
	}
	r.note("the parent's review, posted as the session %s of the architect of %s: comment %s on %s, %s; and published to %s, %s",
		session, root.key, posted.ID, child.key, sessionsExcerpt(body), topic, sessionsExcerpt(message))
	return body
}

// publishedBy reads the transcript of the architect named who for its envoy_publish of the change
// to the role topic of role on member, which the architect's prompt asks for
// (prompts/go/architect-common.md). It prints the call it found as evidence and answers "", or
// answers why the step failed.
func (r *sessionsRun) publishedBy(architect sessionsTranscript, who string, member *sessionsMember, role claim.Role, change []string) string {
	r.t.Helper()
	topic := notify.RoleTopicPrefix + string(r.token(member, role))
	call, published := architect.publishedTo(topic, change...)
	if !published {
		return fmt.Sprintf("%s made no envoy_publish to %s, the role topic of the %s of %s, naming version 2 and the change",
			who, topic, role, member.key)
	}
	r.evidence("%s's envoy_publish to %s, the role topic of the %s of %s: %s %s", who, topic, role, member.key, call.name, sessionsExcerpt(call.text))
	return ""
}

// Step off. Under gates.design off a root in implementing and its child in reviewing are each held
// by a live worker, and the child has its own live architect, which its first phase start started,
// as it starts every child's (workflow.Engine.StartFirstPhase). The root architect is asked to
// write version 2 of the root's spec, which a deployment that gates nothing sends no news of: each
// holder hears of it only from the architect, by its own envoy_publish to that holder's role
// topic.
func (r *sessionsRun) off() (sessionsOutcome, string) {
	root := &sessionsMember{phase: phase.Implementing, roles: []claim.Role{claim.RoleArchitect, claim.RoleImplementer}, tasks: map[claim.Role]string{}}
	child := &sessionsMember{phase: phase.Reviewing, roles: []claim.Role{claim.RoleArchitect, claim.RoleReviewer}, tasks: map[claim.Role]string{}}
	r.tree(sessionsSpec("A reader that pages.", sessionsDesign+" The child issue builds the reader's fetch, and the root builds the cache in front of it.", sessionsLater), root, child)
	root.tasks[claim.RoleImplementer] = holderStart(root.key, phase.Implementing)
	child.tasks[claim.RoleReviewer] = holderStart(child.key, phase.Reviewing)
	r.boot()
	r.checkArm(root, claim.RoleArchitect, sessionsArchitectMark)
	if !r.settled(root, claim.RoleImplementer) || !r.settled(child, claim.RoleReviewer) {
		return sessionsSetup, "a holder did not end its start turn within the run's bound"
	}
	r.deliver(root, claim.RoleArchitect, architectTask(
		fmt.Sprintf("spec (document %s)", root.spec), root.key, sessionsDesign, sessionsDesignAgain,
		fmt.Sprintf("%s, the root, in implementing, where its implementer holds it; %s, its child, in reviewing, where its reviewer holds it", root.key, child.key)))
	change := sessionsSpecChange
	holders := []struct {
		member *sessionsMember
		role   claim.Role
	}{{root, claim.RoleImplementer}, {child, claim.RoleReviewer}}
	told := make([]string, len(holders))
	all := func() bool {
		for i, holder := range holders {
			if told[i] == "" {
				told[i] = r.transcript(holder.member, holder.role).toldOf(change...)
			}
		}
		return !slices.Contains(told, "")
	}
	var quiet time.Time
	r.until(r.deadline, func() bool { return all() || r.quiet(root, claim.RoleArchitect, &quiet) })
	r.until(time.Now().Add(sessionsGrace), all)
	architect := r.transcript(root, claim.RoleArchitect)
	r.printCalls(root, claim.RoleArchitect, architect.bash("dispatch "), "ran")
	r.printCalls(root, claim.RoleArchitect, architect.calling("envoy_publish"), "called")
	r.printCalls(child, claim.RoleArchitect, r.transcript(child, claim.RoleArchitect).calling("envoy_publish"), "called")
	written, text := r.version2(root.spec)
	unwritten := sessionsUnwritten(root.key+"'s spec", written, text, "two pages")
	if unwritten == "" {
		r.evidence("version 2 of %s's spec %s holds the new Design sentence", root.key, root.spec)
	}
	var why []string
	for i, holder := range holders {
		if told[i] == "" {
			why = append(why, fmt.Sprintf("no Envoy message naming version 2 and the change reached the %s of %s", holder.role, holder.member.key))
		} else {
			r.evidence("the %s of %s's transcript holds the Envoy message %s", holder.role, holder.member.key, sessionsExcerpt(told[i]))
		}
		if missed := r.publishedBy(architect, "the architect of "+root.key, holder.member, holder.role, change); missed != "" {
			why = append(why, missed)
		}
	}
	return sessionsJudged(unwritten, why)
}

// holderStart is the start task a holder of the a4 and off steps holds at the boot, at the entry the
// daemon's start path raised for it, in place of that start's own task, the phase's resume task
// (workflow.Engine.ResumeTask: "Resume the existing phase work"). That task does not serve the step:
// this proof runs no repository and no pull request, so a holder that resumed its phase would find
// nothing to work on and spend its turns saying so. It waits instead, as a holder between turns
// does, for what the step sends it.
func holderStart(issue string, p phase.Phase) string {
	return fmt.Sprintf("This proof runs no GitHub and no repository: %s has no pull request or working copy to work on, and you have no "+
		"GitHub access, so run no `gh`, `git` or `jj`. You hold %s in %s, and its work stands where your earlier session left it. Start no "+
		"work now: reply with one line that starts with WAITING, and end your turn.", issue, issue, p)
}

// reviewerNote is the note a reviewer of the answers step holds at the boot in its start's own
// task, the phase's resume task with the review round's two sentences that the daemon's start path
// stores and sends (workflow.Engine.StartPromotedWorker, Engine.ResumeTask), before those two
// sentences (sessionsRun.seedClaim). It says what the proof stands in for and what a review of the
// pull request holds as fact: the proof runs no GitHub, so the pull request is the one this note
// names; what that pull request changes, and that the review read it against version 1 of the
// spec. It does not say which part of the spec the review rests on, so whether a new version
// makes the review stale is the reviewer's own judgment of the change against the version, not a
// match of the section the news names against words of this note. It ends with the order to wait
// that every holder of this proof gets, since nothing of the pull request is left to read.
func reviewerNote(issue, spec string) string {
	return fmt.Sprintf("This proof runs no GitHub and no repository, and you have no GitHub access, so run no `gh`, `git` or `jj`. The "+
		"pull request of %s stands in for one you have read whole: its change makes the reader fetch one page on each call, and you "+
		"reviewed that change against version 1 of the spec of %s (document %s). Nothing of it is left for you to read now: reply with "+
		"one line that starts with WAITING, and end your turn.", issue, issue, spec)
}

// Step answers. Two roots stand in reviewing, each with its own architect, as admission's
// promotion starts every root's (workflow.Engine.StartArchitect), and a live reviewer, whose start
// task is the one the daemon's start path stores, with a note of what its pull request changes
// (reviewerNote). Version 2 of the first root's spec changes its `## Design` and version 2 of the
// second's its `## Later work` alone, each written as its root architect's own session; the person
// approves both, and the daemon sends each reviewer the news and each architect its summary. The
// stale reviewer must move its issue back with a reason naming the version; the current one must
// write a comment of its own on its issue naming the version, and stay.
func (r *sessionsRun) answers() (sessionsOutcome, string) {
	stale := &sessionsMember{phase: phase.Reviewing, roles: []claim.Role{claim.RoleArchitect, claim.RoleReviewer}, notes: map[claim.Role]string{}}
	current := &sessionsMember{phase: phase.Reviewing, roles: []claim.Role{claim.RoleArchitect, claim.RoleReviewer}, notes: map[claim.Role]string{}}
	r.tree(sessionsSpec("A reader that pages.", sessionsDesign, sessionsLater), stale)
	r.tree(sessionsSpec("A reader that pages.", sessionsDesign, sessionsLater), current)
	for _, member := range []*sessionsMember{stale, current} {
		member.notes[claim.RoleReviewer] = reviewerNote(member.key, member.spec)
	}
	r.boot()
	for _, member := range []*sessionsMember{stale, current} {
		r.checkArm(member, claim.RoleArchitect, sessionsArchitectMark)
		r.checkArm(member, claim.RoleReviewer, sessionsWorkerMark)
		for _, role := range member.roles {
			if !r.settled(member, role) {
				return sessionsSetup, fmt.Sprintf("the %s of %s did not end its start turn within the run's bound", role, member.key)
			}
		}
	}
	r.note("both architects and both reviewers ended their start turns")
	r.edit(stale.spec, r.claim(r.token(stale, claim.RoleArchitect)).Session, sessionsDesign, sessionsDesignAgain, 2)
	r.edit(current.spec, r.claim(r.token(current, claim.RoleArchitect)).Session, sessionsLater, sessionsLaterAgain, 2)
	r.approve(stale.spec, 2)
	r.approve(current.spec, 2)
	r.note("version 2 of %s's spec changes `## Design`, version 2 of %s's changes `## Later work`, each written as its architect's session; "+
		"%s approved both", stale.key, current.key, versionNewsPerson)
	for _, member := range []*sessionsMember{stale, current} {
		news := ""
		if !r.until(r.deadline, func() bool {
			for _, prompt := range r.transcript(member, claim.RoleReviewer).user {
				if strings.Contains(prompt, newsKindLine) {
					news = prompt
					return true
				}
			}
			return false
		}) {
			return sessionsSetup, fmt.Sprintf("the reviewer of %s was never given the news of version 2", member.key)
		}
		r.evidence("the reviewer of %s was given the news %s", member.key, sessionsExcerpt(news))
	}
	// The current reviewer's own comment: its architect, told of the same version, may write one
	// of its own on the issue too.
	currentReviewer := r.claim(r.token(current, claim.RoleReviewer)).Session
	commented := func() string {
		for _, body := range r.comments(current.key, currentReviewer) {
			if sessionsVersion2.MatchString(body) {
				return body
			}
		}
		return ""
	}
	r.until(r.deadline, func() bool {
		return r.stateIssue(stale.key).Phase != phase.Reviewing && commented() != ""
	})
	for _, member := range []*sessionsMember{stale, current} {
		for _, role := range member.roles {
			if !r.settled(member, role) {
				r.evidence("the %s of %s was still in a turn or held a task at the run's bound, so what follows is what it did by then", role, member.key)
			}
		}
		architect := r.transcript(member, claim.RoleArchitect)
		r.printCalls(member, claim.RoleArchitect, architect.calling("envoy_publish"), "called")
		r.printCalls(member, claim.RoleArchitect, architect.bash("dispatch comment"), "ran")
	}
	staleRead, currentRead := r.transcript(stale, claim.RoleReviewer), r.transcript(current, claim.RoleReviewer)
	moves := func(read sessionsTranscript) []sessionsCall {
		var found []sessionsCall
		for _, call := range read.calling("legion") {
			if op, _ := call.args["op"].(string); op == "request_backward_move" || strings.Contains(call.text, "request_backward_move") {
				found = append(found, call)
			}
		}
		return found
	}
	staleMoves, currentMoves := moves(staleRead), moves(currentRead)
	r.printCalls(stale, claim.RoleReviewer, staleMoves, "called")
	r.printCalls(stale, claim.RoleReviewer, staleRead.bash("dispatch comment"), "ran")
	r.printCalls(current, claim.RoleReviewer, currentMoves, "called")
	r.printCalls(current, claim.RoleReviewer, currentRead.bash("dispatch comment"), "ran")
	var why []string
	named := false
	for _, call := range staleMoves {
		if reason, _ := call.args["reason"].(string); sessionsVersion2.MatchString(reason) || sessionsVersion2.MatchString(call.text) {
			named = true
		}
	}
	if got := r.stateIssue(stale.key).Phase; got == phase.Reviewing || !named {
		why = append(why, fmt.Sprintf("the stale reviewer of %s made no request_backward_move naming version 2 that moved it (it stands in %s)", stale.key, got))
	} else {
		r.evidence("%s left reviewing on its reviewer's request_backward_move naming version 2, and stands in %s: the move starts the implementer, "+
			"whose launch this proof's runtime refuses, so the daemon holds the issue once that launch fails", stale.key, got)
	}
	body := commented()
	if body == "" {
		why = append(why, fmt.Sprintf("the current reviewer of %s wrote no comment on it naming version 2", current.key))
	} else {
		r.evidence("Dispatch holds the comment on %s %s", current.key, sessionsExcerpt(body))
	}
	if got := r.stateIssue(current.key).Phase; got != phase.Reviewing || len(currentMoves) > 0 {
		why = append(why, fmt.Sprintf("the current reviewer of %s moved back (it stands in %s, %d request_backward_move calls)", current.key, got, len(currentMoves)))
	} else {
		r.evidence("%s stays in reviewing, and its reviewer made no request_backward_move", current.key)
	}
	if len(why) > 0 {
		return sessionsFailed, strings.Join(why, "; ")
	}
	return sessionsPassed, ""
}
